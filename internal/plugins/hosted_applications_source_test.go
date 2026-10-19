package plugins

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/httpapi"
)

func hostedTestTarGz(t *testing.T, headers []*tar.Header, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gz)
	for _, header := range headers {
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			if _, err := io.WriteString(tarWriter, files[header.Name]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func regularHostedHeader(name, body string) *tar.Header {
	return &tar.Header{Name: name, Mode: 0644, Size: int64(len(body)), Typeflag: tar.TypeReg}
}

func TestHostedAppArchiveExtractsBuildableGoProject(t *testing.T) {
	archive := hostedTestTarGz(t, []*tar.Header{
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0755},
		regularHostedHeader("./go.mod", "module example.test/app\n\ngo 1.22\n"),
		regularHostedHeader("./go.sum", ""),
		regularHostedHeader("./main.go", "package main\n"),
		{Name: "cmd", Typeflag: tar.TypeDir, Mode: 0755},
		regularHostedHeader("cmd/health.go", "package main\n"),
	}, map[string]string{"./go.mod": "module example.test/app\n\ngo 1.22\n", "./go.sum": "", "./main.go": "package main\n", "cmd/health.go": "package main\n"})
	destination := t.TempDir()
	expanded, err := extractHostedAppArchive(context.Background(), destination, "go", bytes.NewReader(archive))
	if err != nil || expanded == 0 {
		t.Fatalf("valid source archive was rejected: bytes=%d err=%v", expanded, err)
	}
	for name, expected := range map[string]string{"go.mod": "module example.test/app\n\ngo 1.22\n", "main.go": "package main\n", "cmd/health.go": "package main\n"} {
		actual, readErr := os.ReadFile(filepath.Join(destination, name))
		if readErr != nil || string(actual) != expected {
			t.Fatalf("extracted %s differs: %q err=%v", name, actual, readErr)
		}
	}
}

func TestHostedAppArchiveRequiresRuntimeLockfilesAndStartScripts(t *testing.T) {
	nodeFiles := map[string]string{
		"package.json":      "{\"name\":\"demo\",\"scripts\":{\"start\":\"node server.js\"}}\n",
		"package-lock.json": "{\"name\":\"demo\",\"lockfileVersion\":3,\"packages\":{\"\":{\"name\":\"demo\"}}}\n",
		"server.js":         "process.exit(0)\n",
	}
	for _, test := range []struct {
		name      string
		files     map[string]string
		wantError string
	}{
		{name: "valid node", files: nodeFiles},
		{name: "missing lockfile", files: map[string]string{"package.json": nodeFiles["package.json"]}, wantError: "application_lockfile_required"},
		{name: "missing start", files: map[string]string{"package.json": "{\"name\":\"demo\"}", "package-lock.json": nodeFiles["package-lock.json"]}, wantError: "application_start_script_required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var headers []*tar.Header
			for name, body := range test.files {
				headers = append(headers, regularHostedHeader(name, body))
			}
			archive := hostedTestTarGz(t, headers, test.files)
			_, err := extractHostedAppArchive(context.Background(), t.TempDir(), "nodejs", bytes.NewReader(archive))
			if test.wantError == "" && err != nil {
				t.Fatalf("valid Node.js archive was rejected: %v", err)
			}
			if test.wantError != "" && core.ErrorCode(err) != test.wantError {
				t.Fatalf("archive error = %q, want %q (err=%v)", core.ErrorCode(err), test.wantError, err)
			}
		})
	}
	if err := validateHostedAppProjectManifest(t.TempDir(), "go"); core.ErrorCode(err) != "application_manifest_required" {
		t.Fatalf("Go project without go.mod/go.sum was not rejected: %v", err)
	}
}

func TestHostedAppSourceMCPArchiveDescriptorIsBoundedAndDigestChecked(t *testing.T) {
	archive := []byte("small-gzip-placeholder")
	digest := sha256.Sum256(archive)
	input := hostedAppSourceUploadInput{ID: "demo", ExpectedRevision: 1, ExpectedBytes: int64(len(archive)), ExpectedSHA256: hex.EncodeToString(digest[:]), ArchiveBase64: base64.StdEncoding.EncodeToString(archive)}
	raw, _ := json.Marshal(input)
	if err := validHostedAppSourceInput(raw); err != nil {
		t.Fatalf("valid bounded MCP descriptor was rejected: %v", err)
	}
	input.ExpectedSHA256 = strings.Repeat("0", 64)
	raw, _ = json.Marshal(input)
	if err := validHostedAppSourceInput(raw); err == nil {
		t.Fatal("MCP archive with a mismatched digest was accepted")
	}
}

func TestHostedAppArchiveRejectsTraversalLinksAndMissingManifest(t *testing.T) {
	for _, test := range []struct {
		name    string
		headers []*tar.Header
		files   map[string]string
	}{
		{name: "traversal", headers: []*tar.Header{regularHostedHeader("../escape", "bad"), regularHostedHeader("go.mod", "module x\n")}, files: map[string]string{"../escape": "bad", "go.mod": "module x\n"}},
		{name: "symlink", headers: []*tar.Header{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}, regularHostedHeader("go.mod", "module x\n")}, files: map[string]string{"go.mod": "module x\n"}},
		{name: "missing-manifest", headers: []*tar.Header{regularHostedHeader("main.go", "package main\n")}, files: map[string]string{"main.go": "package main\n"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			archive := hostedTestTarGz(t, test.headers, test.files)
			destination := t.TempDir()
			_, err := extractHostedAppArchive(context.Background(), destination, "go", bytes.NewReader(archive))
			if err == nil {
				t.Fatal("unsafe or incomplete source archive was accepted")
			}
			if _, statErr := os.Lstat(filepath.Join(filepath.Dir(destination), "escape")); statErr == nil {
				t.Fatal("archive path traversal wrote outside the staging directory")
			}
		})
	}
}

func TestHostedAppSourceRollbackSwapsOwnedProjectDirectories(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-owned rollback fixtures require root")
	}
	root := t.TempDir()
	project := filepath.Join(root, "project")
	previous := filepath.Join(root, ".project.previous")
	for path, marker := range map[string]string{project: "new", previous: "old"} {
		if err := os.Mkdir(path, 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "marker"), []byte(marker), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, os.Getuid(), os.Getgid()); err != nil {
			t.Fatal(err)
		}
	}
	stat, _ := os.Stat(project)
	identity, ok := statOf(stat)
	if !ok {
		t.Fatal("platform does not expose directory identity")
	}
	receipt := hostedAppReceipt{HostedApplication: HostedApplication{ID: "demo", Runtime: "go", SourceSHA256: "new-hash"}, UID: identity.Uid, GID: identity.Gid, Root: root, ProjectDir: project, PreviousSourceSHA256: "old-hash"}
	digest, sourceRollback, err := rollbackHostedApp(receipt)
	if err != nil || !sourceRollback || digest != "old-hash" {
		t.Fatalf("source rollback failed: digest=%q source=%t err=%v", digest, sourceRollback, err)
	}
	current, _ := os.ReadFile(filepath.Join(project, "marker"))
	previousContent, _ := os.ReadFile(filepath.Join(previous, "marker"))
	if string(current) != "old" || string(previousContent) != "new" {
		t.Fatalf("source rollback did not retain both sides: current=%q previous=%q", current, previousContent)
	}
}

type hostedSourceHTTPStreamer struct {
	received []byte
	called   bool
}

func (s *hostedSourceHTTPStreamer) Validate(agent.Request) error { return nil }
func (s *hostedSourceHTTPStreamer) Run(context.Context, agent.Request) (agent.Result, error) {
	return agent.Result{}, errors.New("application archive must use Stream")
}
func (s *hostedSourceHTTPStreamer) Stream(_ context.Context, request agent.Request, streams agent.IO) (agent.Result, error) {
	s.called = true
	var input hostedAppSourceUploadInput
	if err := json.Unmarshal(request.Input, &input); err != nil {
		return agent.Result{}, err
	}
	body, err := io.ReadAll(streams.Stdin)
	if err != nil {
		return agent.Result{}, err
	}
	s.received = body
	app := HostedApplication{ID: input.ID, Revision: input.ExpectedRevision + 1, Status: "created", SourceSHA256: input.ExpectedSHA256}
	raw, _ := json.Marshal(app)
	return agent.Result{Operation: request.Operation, ExitCode: 0, Data: raw}, nil
}

type hostedSourceTrackingBody struct {
	reader *bytes.Reader
	reads  int
}

func (b *hostedSourceTrackingBody) Read(data []byte) (int, error) {
	b.reads++
	return b.reader.Read(data)
}
func (b *hostedSourceTrackingBody) Close() error { return nil }

func TestHostedAppSourceUploadAuthorizesBeforeReadingBody(t *testing.T) {
	service, err := core.New(filepath.Join(t.TempDir(), "core"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	if err = service.RegisterScopes("hosted_apps:read", "hosted_apps:manage"); err != nil {
		t.Fatal(err)
	}
	secret, err := os.ReadFile(service.SetupSecretFile())
	if err != nil {
		t.Fatal(err)
	}
	admin, err := service.Setup(context.Background(), strings.TrimSpace(string(secret)), "admin", "source upload test")
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
	streamer := &hostedSourceHTTPStreamer{}
	manager.executor = streamer
	api := httpapi.NewWithOptions(service, httpapi.Options{ResourceHandler: manager.HostedAppSourceResourceHandler(service)})
	allowed, err := service.CreateToken(context.Background(), admin, core.TokenRequest{Name: "source import", Scopes: []string{"hosted_apps:manage"}, Resources: []string{hostedAppResource("demo")}})
	if err != nil {
		t.Fatal(err)
	}
	denied, err := service.CreateToken(context.Background(), admin, core.TokenRequest{Name: "other source", Scopes: []string{"hosted_apps:manage"}, Resources: []string{hostedAppResource("other")}})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("bounded tar.gz placeholder")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	makeRequest := func(token string, input io.ReadCloser) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/applications/source/import?id=demo&expected_revision=7", nil)
		r.Body = input
		r.ContentLength = int64(len(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/octet-stream")
		r.Header.Set("X-Content-SHA256", digest)
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		return w
	}
	tracked := &hostedSourceTrackingBody{reader: bytes.NewReader(body)}
	response := makeRequest(denied.Token, tracked)
	if response.Code != http.StatusForbidden || tracked.reads != 0 || streamer.called {
		t.Fatalf("unauthorized upload was read or streamed: status=%d reads=%d called=%t", response.Code, tracked.reads, streamer.called)
	}
	response = makeRequest(allowed.Token, io.NopCloser(bytes.NewReader(body)))
	if response.Code != http.StatusCreated || !streamer.called || !bytes.Equal(streamer.received, body) {
		t.Fatalf("authorized source upload did not use the stream: status=%d called=%t response=%s", response.Code, streamer.called, response.Body.String())
	}
}
