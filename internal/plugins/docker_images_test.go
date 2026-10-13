//go:build linux

package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/jobs"
)

const dockerImageFixtureRef = "registry.example/team/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type dockerImageFixtureCall struct {
	method string
	path   string
	query  url.Values
}

type dockerImageAPIFixture struct {
	mu           sync.Mutex
	calls        []dockerImageFixtureCall
	deleteStatus int
}

func newDockerImageFixture(t *testing.T, instance Instance, deleteStatus int) *dockerImageAPIFixture {
	t.Helper()
	if err := os.MkdirAll(instance.Paths.RunDir, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(instance.Paths.RunDir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal("could not create isolated Docker API Unix socket", err)
	}
	fixture := &dockerImageAPIFixture{deleteStatus: deleteStatus}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := dockerImageFixtureCall{method: r.Method, path: r.URL.EscapedPath(), query: r.URL.Query()}
		fixture.mu.Lock()
		fixture.calls = append(fixture.calls, call)
		fixture.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(call.path, "/json"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"Id":"sha256:%s","RepoDigests":[%q],"Created":"2026-01-02T03:04:05Z","Architecture":"amd64","Os":"linux","Size":1234,"Config":{"Env":["PRIVATE_ENV_SENTINEL=must-not-return"],"Labels":{"private":"PRIVATE_LABEL_SENTINEL"}}}`, strings.Repeat("b", 64), dockerImageFixtureRef)
		case r.Method == http.MethodDelete && strings.Contains(call.path, "/images/"):
			if fixture.deleteStatus != 0 && fixture.deleteStatus != http.StatusOK {
				http.Error(w, "image is in use by a container", fixture.deleteStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"Untagged":"registry.example/team/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`))
		default:
			http.NotFound(w, r)
		}
	})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-done
	})
	return fixture
}

func (f *dockerImageAPIFixture) snapshot() []dockerImageFixtureCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dockerImageFixtureCall(nil), f.calls...)
}

func dockerImageFixtureAgent(t *testing.T) (*PrivilegedExecutor, Instance) {
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
	base, err := os.MkdirTemp("/tmp", "pldi-")
	if err != nil {
		t.Fatal("short private Docker fixture directory could not be created", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	executor, err := NewPrivilegedExecutor(PrivilegedConfig{RootDir: filepath.Join(base, "software"), StateDir: filepath.Join(base, "private"), RunUser: worker.Username, APIUser: api})
	if err != nil {
		t.Fatal(err)
	}
	instance := Instance{ID: "docker-image-fixture", PluginID: "docker", InstanceID: "i", Paths: executor.instancePaths("docker", "i"), Status: "running", Revision: 1, Ownership: "managed", Service: ServiceProfile{ID: "docker-image-fixture", Unit: "powerlinux-plugin-docker-image-fixture.service"}}
	if err = executor.save("receipts", instance.ID, instance); err != nil {
		t.Fatal(err)
	}
	return executor, instance
}

func dockerImageToken(t *testing.T, service *core.Service, actor core.Actor, name string, scopes, resources []string) core.Actor {
	t.Helper()
	created, err := service.CreateToken(context.Background(), actor, core.TokenRequest{Name: name, Kind: "mcp", Scopes: scopes, Resources: resources, ExpiresInSeconds: 300})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.AuthenticateToken(context.Background(), created.Token)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestDockerImageInspectUsesExactDigestResourceAndSafeAllowlistOverUnixHTTP(t *testing.T) {
	executor, instance := dockerImageFixtureAgent(t)
	fixture := newDockerImageFixture(t, instance, http.StatusOK)
	if err := executor.Validate(agentDockerRequest(t, instance.ID, "image_inspect", dockerImageFixtureRef)); err != nil {
		t.Fatal("digest-pinned image inspection request failed validation", err)
	}
	request := agentDockerRequest(t, instance.ID, "image_inspect", dockerImageFixtureRef)
	result, err := executor.Run(context.Background(), request)
	if err != nil || result.ExitCode != 0 {
		t.Fatal("native Docker inspect failed over its per-instance Unix socket", err, result.Stderr)
	}
	var view dockerImageView
	if err := json.Unmarshal([]byte(result.Stdout), &view); err != nil {
		t.Fatal("Docker inspect response did not use the safe view", err)
	}
	if view.Image != dockerImageFixtureRef || view.ID != "sha256:"+strings.Repeat("b", 64) || view.Architecture != "amd64" || view.OS != "linux" || view.SizeBytes != 1234 {
		t.Fatalf("unexpected image view: %+v", view)
	}
	if strings.Contains(result.Stdout, "PRIVATE_ENV_SENTINEL") || strings.Contains(result.Stdout, "PRIVATE_LABEL_SENTINEL") || strings.Contains(result.Stdout, "RepoDigests") {
		t.Fatal("image inspection exposed environment, arbitrary labels, or unrequested aliases", result.Stdout)
	}
	calls := fixture.snapshot()
	expectedPath := "/v1.45/images/" + url.PathEscape(dockerImageFixtureRef) + "/json"
	if len(calls) != 1 || calls[0].method != http.MethodGet || calls[0].path != expectedPath {
		t.Fatalf("Docker API fixture did not receive the exact digest reference: %+v", calls)
	}
	resource := DockerImageResource(instance.ID, dockerImageFixtureRef)
	if resource == "" || resource != DockerImageResource(instance.ID, dockerImageFixtureRef) || len(resource) > 256 {
		t.Fatal("image authorization resource is not stable")
	}
	alias := "registry-alias.example/team/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if DockerImageResource(instance.ID, dockerImageFixtureRef) == DockerImageResource(instance.ID, alias) {
		t.Fatal("repository aliases must require distinct image resource grants")
	}
	for _, raw := range []string{
		`{"id":"docker-image-fixture","command":"image_inspect","image":"registry.example/team/app:latest@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		`{"id":"docker-image-fixture","command":"image_inspect","image":"registry.example/team/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","daemon_id":"sha256:bbbb"}`,
	} {
		if err := executor.validateDocker(json.RawMessage(raw)); err == nil {
			t.Fatalf("tag, daemon ID, or unknown image selector was accepted: %s", raw)
		}
	}
}

func TestDockerImageRemoveRequiresImageGrantAndKeepsFailedDurableTask(t *testing.T) {
	ctx := context.Background()
	service, err := core.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	secret, err := os.ReadFile(service.SetupSecretFile())
	if err != nil {
		t.Fatal(err)
	}
	admin, err := service.Setup(ctx, strings.TrimSpace(string(secret)), "docker-admin", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	executor, instance := dockerImageFixtureAgent(t)
	fixture := newDockerImageFixture(t, instance, http.StatusConflict)
	manager, err := New(service.Runtime("plugins"), Options{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.RegisterProvider(manager); err != nil {
		t.Fatal(err)
	}
	if err = service.RegisterExecutor(AgentJobOperations(), executor); err != nil {
		t.Fatal(err)
	}
	imageResource := DockerImageResource(instance.ID, dockerImageFixtureRef)
	instanceResource := InstanceResource(instance.ID)
	scopes := []string{"containers:manage", "containers:read", "tasks:read", "audit:read"}
	limited := dockerImageToken(t, service, admin, "instance-only", scopes, []string{instanceResource})
	allowed := dockerImageToken(t, service, admin, "one-image", scopes, []string{instanceResource, imageResource})
	input, _ := json.Marshal(map[string]any{"id": instance.ID, "image": dockerImageFixtureRef})
	if _, err = service.Execute(ctx, limited, "docker_image_inspect", input); core.ErrorCode(err) != "forbidden" {
		t.Fatal("instance-only grant inspected a specifically scoped image", err)
	}
	if _, err = service.Execute(ctx, limited, "docker_image_remove", input); core.ErrorCode(err) != "forbidden" {
		t.Fatal("instance-only grant removed a specifically scoped image", err)
	}
	if len(fixture.snapshot()) != 0 {
		t.Fatal("unauthorized image removal reached Docker")
	}
	alias := "registry-alias.example/team/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	aliasOnly := dockerImageToken(t, service, admin, "one-alias", scopes, []string{instanceResource, DockerImageResource(instance.ID, alias)})
	if _, err = service.Execute(ctx, aliasOnly, "docker_image_inspect", input); core.ErrorCode(err) != "forbidden" {
		t.Fatal("a repository alias grant bypassed the exact digest-reference resource", err)
	}
	if len(fixture.snapshot()) != 0 {
		t.Fatal("alias-only authorization reached Docker")
	}
	inspected, err := service.Execute(ctx, allowed, "docker_image_inspect", input)
	if err != nil {
		t.Fatal("authorized image owner could not inspect the image", err)
	}
	inspection, ok := inspected.(dockerImageView)
	if !ok || inspection.Image != dockerImageFixtureRef || inspection.ID != "sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("image inspection returned an unexpected view: %#v", inspected)
	}
	queued, err := service.Execute(ctx, allowed, "docker_image_remove", input)
	if err != nil {
		t.Fatal("authorized image removal did not enqueue a durable task", err)
	}
	jobView, ok := queued.(map[string]any)
	if !ok || jobView["id"] == "" || jobView["resource"] != imageResource {
		t.Fatalf("durable task omitted its image-specific resource: %#v", queued)
	}
	jobID, _ := jobView["id"].(string)
	deadline := time.Now().Add(5 * time.Second)
	var final map[string]any
	for time.Now().Before(deadline) {
		value, getErr := service.Execute(ctx, allowed, "tasks_get", json.RawMessage(`{"id":"`+jobID+`"}`))
		if getErr != nil {
			t.Fatal("authorized image owner could not inspect the removal task", getErr)
		}
		final, ok = value.(map[string]any)
		if ok && final["status"] == jobs.Failed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if final == nil || final["status"] != jobs.Failed {
		t.Fatalf("Docker's non-force in-use rejection was not preserved as a failed task: %#v", final)
	}
	softwareOnly := dockerImageToken(t, service, admin, "task-without-image-grant", []string{"tasks:read"}, []string{instanceResource})
	if _, err = service.Execute(ctx, softwareOnly, "tasks_get", json.RawMessage(`{"id":"`+jobID+`"}`)); core.ErrorCode(err) != "forbidden" {
		t.Fatal("task metadata lease omitted the image-specific resource", err)
	}
	calls := fixture.snapshot()
	if len(calls) != 3 || calls[0].method != http.MethodGet || calls[1].method != http.MethodGet || calls[2].method != http.MethodDelete {
		t.Fatalf("removal did not inspect then issue one non-force image DELETE: %+v", calls)
	}
	expectedDeletePath := "/v1.45/images/" + url.PathEscape(dockerImageFixtureRef)
	if calls[2].path != expectedDeletePath || calls[2].query.Get("force") != "false" || calls[2].query.Get("noprune") != "true" || strings.Contains(calls[2].path, "/containers/") {
		t.Fatalf("removal attempted force/cascade or addressed a container: %+v", calls[2])
	}
	audits, err := service.Execute(ctx, allowed, "audit_list", json.RawMessage(`{"limit":100}`))
	if err != nil {
		t.Fatal("image-scoped token could not read its action audit", err)
	}
	rows, ok := audits.([]core.Audit)
	if !ok {
		t.Fatalf("unexpected audit response: %#v", audits)
	}
	foundInspect, foundRemove, foundFailedTask := false, false, false
	for _, row := range rows {
		if row.Resource != imageResource {
			continue
		}
		if row.Action == "docker_image_inspect" {
			foundInspect = true
		}
		if row.Action == "docker_image_remove" {
			foundRemove = true
		}
		if row.Action == "docker_action_finished" && row.Status == jobs.Failed {
			foundFailedTask = true
		}
	}
	if !foundInspect || !foundRemove || !foundFailedTask {
		t.Fatalf("image action/task failure was not audited against its image resource: %+v", rows)
	}
}

func agentDockerRequest(t *testing.T, id, command, image string) agent.Request {
	t.Helper()
	raw, err := json.Marshal(DockerInput{ID: id, Command: command, Image: image})
	if err != nil {
		t.Fatal(err)
	}
	return agent.Request{Operation: "docker_action", Input: raw}
}
