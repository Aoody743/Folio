package plugins

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

func testAgent(t *testing.T) *PrivilegedExecutor {
	t.Helper()
	worker, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if worker.Uid == "0" {
		worker, err = user.Lookup("nobody")
		if err != nil {
			t.Skip("non-root fixture user unavailable")
		}
	}
	api := "root"
	if worker.Username == api {
		api = "nobody"
	}
	base := t.TempDir()
	executor, err := NewPrivilegedExecutor(PrivilegedConfig{RootDir: filepath.Join(base, "software"), StateDir: filepath.Join(base, "private"), RunUser: worker.Username, APIUser: api})
	if err != nil {
		t.Fatal(err)
	}
	return executor
}
func TestCoreCatalogHasEveryLegacyPluginAndHonestSources(t *testing.T) {
	catalog, err := BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Items) != 27 {
		t.Fatalf("catalog plugin count %d", len(catalog.Items))
	}
	legacyPlugins := map[string]bool{}
	for _, id := range []string{
		"openresty", "php", "php-apt", "php-yum", "php-guard", "phpmyadmin",
		"mysql", "mysql-community", "mariadb", "postgresql", "mongodb", "pgadmin",
		"data_query", "redis", "valkey", "memcached", "docker", "pureftp", "rsyncd",
		"backup_ftp", "acme_pandominassl_apply", "supervisor", "webssh",
	} {
		legacyPlugins[id] = true
	}
	for _, item := range catalog.Items {
		if legacyPlugins[item.ID] && len(item.Capabilities) == 0 {
			t.Fatalf("%s loses legacy capability ledger", item.ID)
		}
		delete(legacyPlugins, item.ID)
		for _, version := range item.Versions {
			if version.Source.Kind == "unavailable" && version.Verification.Reason == "" {
				t.Fatal("unavailable version conceals blocker")
			}
			if version.Source.Kind == "verified_archive" && version.Source.SHA256 == "" {
				t.Fatal("unpinned artifact")
			}
		}
	}
	if len(legacyPlugins) != 0 {
		t.Fatalf("catalog lost legacy plugins: %v", legacyPlugins)
	}
	php, err := catalog.Get("php")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := php.SelectVersion("legacy-52")
	if err == nil && legacy.Source.Kind != "unavailable" {
		t.Fatal("unverified legacy PHP must not be installed as trusted")
	}
}
func TestPolicyValidationIsReadOnlyAndRejectsSharedIdentity(t *testing.T) {
	executor := testAgent(t)
	config := executor.config
	config.RootDir = filepath.Join(t.TempDir(), "must-not-create")
	config.StateDir = filepath.Join(t.TempDir(), "must-not-create")
	if err := ValidatePrivilegedConfig(config); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(config.RootDir); !os.IsNotExist(err) {
		t.Fatal("pure policy check created software root")
	}
	if _, err := os.Stat(config.StateDir); !os.IsNotExist(err) {
		t.Fatal("pure policy check created private state")
	}
	config.APIUser = config.RunUser
	if ValidatePrivilegedConfig(config) == nil {
		t.Fatal("shared worker/API UID allowed")
	}
}
func TestOwnedIntegrationLifecycleRevisionIdempotencyAndDataRetention(t *testing.T) {
	executor := testAgent(t)
	ctx := context.Background()
	plan, err := executor.plan(ctx, PlanInput{Operation: "install", PluginID: "webssh", InstanceID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	input := ApplyInput{PlanID: plan.ID, ExpectedRevision: 0, IdempotencyKey: "install-webssh-001"}
	instance, err := executor.apply(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if instance.Status != "enabled" || instance.Health.Status != "configuration_required" {
		t.Fatal("integration falsely claimed daemon health", instance)
	}
	data := filepath.Join(instance.Paths.DataDir, "keep.txt")
	if err = os.WriteFile(data, []byte("real application data"), 0600); err != nil {
		t.Fatal(err)
	}
	repeat, err := executor.apply(ctx, input)
	if err != nil || repeat.Revision != instance.Revision {
		t.Fatal("exact plan replay did not return durable same result", repeat, err)
	}
	input.IdempotencyKey = "different-key"
	if _, err = executor.apply(ctx, input); core.ErrorCode(err) != "idempotency_conflict" {
		t.Fatal("plan accepted another execution key", err)
	}
	change, err := executor.plan(ctx, PlanInput{Operation: "config", PluginID: "webssh", InstanceID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = executor.apply(ctx, ApplyInput{PlanID: change.ID, ExpectedRevision: 0, IdempotencyKey: "stale-config-0001"}); core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("stale plan modified configuration", err)
	}
	uninstall, err := executor.plan(ctx, PlanInput{Operation: "uninstall", PluginID: "webssh", InstanceID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	removed, err := executor.apply(ctx, ApplyInput{PlanID: uninstall.ID, ExpectedRevision: instance.Revision, IdempotencyKey: "uninstall-webssh-001"})
	if err != nil {
		t.Fatal(err)
	}
	if removed.Status != "uninstalled" {
		t.Fatal(removed)
	}
	body, err := os.ReadFile(data)
	if err != nil || string(body) != "real application data" {
		t.Fatal("default uninstall lost data", err)
	}
	if _, err = executor.get(ctx, removed.ID, false); err != nil {
		t.Fatal("uninstall lost ownership receipt", err)
	}
	reinstall, err := executor.plan(ctx, PlanInput{Operation: "install", PluginID: "webssh", InstanceID: "test"})
	if err != nil {
		t.Fatal("matching retained uninstall receipt could not be reinstalled", err)
	}
	reinstalled, err := executor.apply(ctx, ApplyInput{PlanID: reinstall.ID, ExpectedRevision: removed.Revision, IdempotencyKey: "reinstall-webssh-001"})
	if err != nil || reinstalled.Revision != removed.Revision+1 {
		t.Fatal("retained reinstall did not advance the existing receipt", reinstalled, err)
	}
	body, err = os.ReadFile(data)
	if err != nil || string(body) != "real application data" {
		t.Fatal("retained reinstall replaced application data", err)
	}
	foreign := executor.instancePaths("webssh", "foreign")
	if err = os.MkdirAll(foreign.DataDir, 0750); err != nil {
		t.Fatal(err)
	}
	if _, err = executor.plan(ctx, PlanInput{Operation: "install", PluginID: "webssh", InstanceID: "foreign"}); core.ErrorCode(err) != "ownership_conflict" {
		t.Fatal("unowned nonempty installation directory was adopted", err)
	}
}
func TestCorruptExecutionReceiptNeverReplays(t *testing.T) {
	executor := testAgent(t)
	plan, err := executor.plan(context.Background(), PlanInput{Operation: "install", PluginID: "webssh", InstanceID: "crash"})
	if err != nil {
		t.Fatal(err)
	}
	id := "apply_" + strings.TrimPrefix(plan.ID, "plan_")
	if err = os.WriteFile(filepath.Join(executor.config.StateDir, "private", id+".json"), []byte("interrupted invalid record"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = executor.apply(context.Background(), ApplyInput{PlanID: plan.ID, IdempotencyKey: "do-not-replay-0001"}); err == nil {
		t.Fatal("corrupt crash record was ignored")
	}
	if _, err = os.Stat(executor.instancePaths("webssh", "crash").Root); !os.IsNotExist(err) {
		t.Fatal("operation ran despite uncertain crash receipt")
	}
}
func TestAgentTypedInputsRejectArbitraryCommandsAndInjection(t *testing.T) {
	executor := testAgent(t)
	for _, request := range []agent.Request{{Operation: "shell", Input: []byte(`{"command":"id"}`)}, {Operation: "plugin_service_control", Input: []byte(`{"id":"webssh-test","command":"start","program":"/bin/sh"}`)}, {Operation: "plugin_plan", Input: []byte(`{"operation":"install","plugin_id":"webssh","instance_id":"../../etc"}`)}, {Operation: "php_action", Input: []byte(`{"id":"php-test","command":"configure","expected_revision":1,"values":{"timezone":"UTC\nextension=evil.so"}}`)}, {Operation: "docker_action", Input: []byte(`{"id":"docker-test","command":"container_create","name":"x","image":"ubuntu:latest","memory_mb":64,"cpus":1}`)}} {
		if executor.Validate(request) == nil {
			t.Fatalf("unsafe request accepted %+v", request)
		}
	}
}
func archiveFixture(t *testing.T, headers []*tar.Header) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(gzipWriter)
	for _, header := range headers {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := writer.Write(bytes.Repeat([]byte("a"), int(header.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
func TestVerifiedArtifactTLSHashAndSafeExtraction(t *testing.T) {
	archive := archiveFixture(t, []*tar.Header{{Name: "software/bin/server", Mode: 0755, Size: 16, Typeflag: tar.TypeReg}})
	digest := sha256.Sum256(archive)
	hash := hex.EncodeToString(digest[:])
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(archive) }))
	defer server.Close()
	store, err := newArtifactStore(t.TempDir(), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	source := Source{Kind: "verified_archive", Trust: "pinned_sha256", SHA256: hash, URL: server.URL, ArchiveFormat: "tar.gz", StripComponents: 1}
	name, err := store.fetch(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err = extractArtifact(context.Background(), name, target, source); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "bin", "server")); err != nil || len(data) != 16 {
		t.Fatal("verified artifact did not extract", err)
	}
	source.SHA256 = strings.Repeat("0", 64)
	if _, err = store.fetch(context.Background(), source); core.ErrorCode(err) != "artifact_checksum_failed" {
		t.Fatal("wrong artifact checksum accepted", err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err = os.WriteFile(outside, archive, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(store.dir, hash)
	if err = os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err = verifyFile(link, hash); err == nil {
		t.Fatal("cache followed symlink")
	}
}
func TestArtifactsRejectTraversalSymlinksHardlinksAndPrivilegedModes(t *testing.T) {
	for _, header := range []*tar.Header{{Name: "../../etc/passwd", Mode: 0644, Typeflag: tar.TypeReg, Size: 1}, {Name: "bin/link", Linkname: "/etc/passwd", Mode: 0777, Typeflag: tar.TypeSymlink}, {Name: "bin/hard", Linkname: "/etc/passwd", Mode: 0644, Typeflag: tar.TypeLink}, {Name: "bin/root", Mode: 04755, Typeflag: tar.TypeReg, Size: 1}} {
		t.Run(strings.ReplaceAll(header.Name, "/", "_"), func(t *testing.T) {
			archive := archiveFixture(t, []*tar.Header{header})
			name := filepath.Join(t.TempDir(), "archive.gz")
			if err := os.WriteFile(name, archive, 0600); err != nil {
				t.Fatal(err)
			}
			if err := extractArtifact(context.Background(), name, t.TempDir(), Source{ArchiveFormat: "tar.gz"}); err == nil {
				t.Fatalf("unsafe archive entry accepted %+v", header)
			}
		})
	}
}
func TestPluginProviderSharesCoreAuthorizationAndSchemas(t *testing.T) {
	service, err := core.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	secret, err := os.ReadFile(service.SetupSecretFile())
	if err != nil {
		t.Fatal(err)
	}
	actor, err := service.Setup(context.Background(), strings.TrimSpace(string(secret)), "administrator", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(service.Runtime("plugins"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.RegisterProvider(manager); err != nil {
		t.Fatal(err)
	}
	data, err := service.Execute(context.Background(), actor, "plugins_catalog", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(data)
	if !bytes.Contains(raw, []byte(`"id":"openresty"`)) {
		t.Fatal("shared catalog omitted actual plugin")
	}
	token, err := service.CreateToken(context.Background(), actor, core.TokenRequest{Name: "catalog only", Kind: "mcp", Scopes: []string{"software:read"}, Resources: []string{"software:webssh-default"}, ExpiresInSeconds: 300})
	if err != nil {
		t.Fatal(err)
	}
	limited, err := service.AuthenticateToken(context.Background(), token.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Execute(context.Background(), limited, "software_plan", json.RawMessage(`{"plugin_id":"webssh","operation":"install"}`)); core.ErrorCode(err) != "forbidden" {
		t.Fatal("shared core did not deny limited actor", err)
	}
	if _, err = service.Execute(context.Background(), actor, "plugins_get", json.RawMessage(`{"id":"webssh-default","program":"/bin/sh"}`)); err == nil {
		t.Fatal("shared schema accepted arbitrary program field")
	}
}
