//go:build linux

package plugins

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/jobs"
)

func registryTestService(t *testing.T) (*core.Service, core.Actor, *Manager) {
	t.Helper()
	ctx := context.Background()
	s, err := core.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	secret, err := os.ReadFile(s.SetupSecretFile())
	if err != nil {
		t.Fatal(err)
	}
	actor, err := s.Setup(ctx, strings.TrimSpace(string(secret)), "registry-admin", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(s.Runtime("plugins"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterProvider(core.Bindings(m.dockerRegistryBindings())); err != nil {
		t.Fatal(err)
	}
	return s, actor, m
}
func TestDockerRegistryCredentialVersionsCASAndNoSecretViews(t *testing.T) {
	s, a, m := registryTestService(t)
	ctx := context.Background()
	data, err := s.Execute(ctx, a, "docker_registry_credential_create", json.RawMessage(`{"name":"private","registry":"registry.example:5443","username":"robot","password":"PRIVATE_PASSWORD_SENTINEL"}`))
	if err != nil {
		t.Fatal(err)
	}
	view := data.(map[string]any)
	id := view["id"].(string)
	if !strings.HasPrefix(id, "reg-") {
		t.Fatal("ID was not server generated")
	}
	for _, kind := range []string{"get", "list"} {
		raw := json.RawMessage(`{}`)
		if kind == "get" {
			raw, _ = json.Marshal(map[string]any{"id": id})
		}
		out, err := s.Execute(ctx, a, "docker_registry_credential_"+kind, raw)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(out)
		if strings.Contains(string(encoded), "PRIVATE_PASSWORD") || strings.Contains(string(encoded), "secret_ref") || strings.Contains(string(encoded), "sec_") {
			t.Fatal("public credential view leaked secret material")
		}
	}
	rotate, _ := json.Marshal(map[string]any{"id": id, "name": "private", "registry": "registry.example:5443", "username": "robot", "password": "ROTATED_PASSWORD_SENTINEL", "expected_revision": 1})
	if _, err = s.Execute(ctx, a, "docker_registry_credential_rotate", rotate); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Execute(ctx, a, "docker_registry_credential_rotate", rotate); core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("stale CAS accepted", err)
	}
	raw, err := m.deps.State.Get(ctx, "docker_registry_versions", registryVersionKey(id, 1))
	if err != nil {
		t.Fatal(err)
	}
	var v dockerRegistryCredential
	_ = json.Unmarshal(raw, &v)
	secret, err := m.deps.Secrets.Get(ctx, a, DockerRegistryResource(id), v.SecretRef)
	if err != nil || string(secret) != "PRIVATE_PASSWORD_SENTINEL" {
		t.Fatal("rotation changed immutable credential version", err)
	}
	clear(secret)
	deletion, _ := json.Marshal(map[string]any{"id": id, "expected_revision": 2})
	if _, err = s.Execute(ctx, a, "docker_registry_credential_delete", deletion); err != nil {
		t.Fatal(err)
	}
	if _, err = loadRegistry(ctx, m.deps.State, id); core.ErrorCode(err) != "not_found" {
		t.Fatal("deleted credential remained current", err)
	}
	if _, err = m.deps.State.Get(ctx, "docker_registry_versions", registryVersionKey(id, 1)); err != nil {
		t.Fatal("deletion invalidated pinned credential", err)
	}
}
func TestDockerRegistryExactHostValidation(t *testing.T) {
	for _, host := range []string{"registry.example", "registry.example:5443", "localhost:5000", "docker.io"} {
		if !canonicalRegistry(host) {
			t.Fatal("valid registry rejected", host)
		}
	}
	for _, host := range []string{"https://registry.example", "REGISTRY.example", "registry.example/path", "registry.example:65536", "evil.example@registry.example", "registry.example:0"} {
		if canonicalRegistry(host) {
			t.Fatal("unsafe registry accepted", host)
		}
	}
	auth := &dockerRegistryAuth{"robot", "secret", "registry.example"}
	if !validDockerRegistryAuth(dockerImageFixtureRef, auth) {
		t.Fatal("exact match rejected")
	}
	if validDockerRegistryAuth(strings.Replace(dockerImageFixtureRef, "registry.example", "evil.example", 1), auth) {
		t.Fatal("cross registry credential forwarding accepted")
	}
}
func TestDockerRegistryAuthUnixHTTPAndSafeErrors(t *testing.T) {
	e, instance := dockerImageFixtureAgent(t)
	if err := os.MkdirAll(instance.Paths.RunDir, 0700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(instance.Paths.RunDir, "docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	authSeen := make(chan dockerRegistryAuth, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := base64.URLEncoding.DecodeString(r.Header.Get("X-Registry-Auth"))
		if err != nil {
			t.Error(err)
		}
		var auth dockerRegistryAuth
		_ = json.Unmarshal(raw, &auth)
		authSeen <- auth
		http.Error(w, "PRIVATE_PASSWORD_SENTINEL echoed", 502)
	})}
	go server.Serve(l)
	t.Cleanup(func() { server.Close() })
	var output string
	err = e.dockerRequestAuth(context.Background(), instance, http.MethodPost, "/images/create?fromImage="+dockerImageFixtureRef, nil, &output, &dockerRegistryAuth{"robot", "PRIVATE_PASSWORD_SENTINEL", "registry.example"})
	if core.ErrorCode(err) != "docker_operation_failed" || strings.Contains(err.Error(), "PRIVATE_PASSWORD") || output != "" {
		t.Fatal("engine error leaked credentials", err)
	}
	got := <-authSeen
	if got.Password != "PRIVATE_PASSWORD_SENTINEL" || got.ServerAddress != "registry.example" {
		t.Fatal("typed registry header missing")
	}
}

func TestDockerImagesListFiltersExactRepoDigestsOverUnixHTTP(t *testing.T) {
	s, a, _ := registryTestService(t)
	e, instance := dockerImageFixtureAgent(t)
	if err := os.MkdirAll(instance.Paths.RunDir, 0700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(instance.Paths.RunDir, "docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	alias := strings.Replace(dockerImageFixtureRef, "registry.example", "other.example", 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.45/images/json" {
			t.Error("unexpected image list path", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"Id": "sha256:" + strings.Repeat("b", 64), "RepoDigests": []string{dockerImageFixtureRef, alias, "repo:latest", "<none>@<none>"}, "RepoTags": []string{"SECRET_TAG"}, "Labels": map[string]string{"secret": "PRIVATE_LABEL"}, "Size": 42}, {"Id": "sha256:" + strings.Repeat("c", 64), "RepoTags": []string{"tag-only:latest"}, "Size": 45}})
	})}
	go server.Serve(l)
	t.Cleanup(func() { server.Close() })
	m, err := New(s.Runtime("plugins"), Options{Executor: e})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterProvider(core.Bindings{m.dockerImagesListBinding()}); err != nil {
		t.Fatal(err)
	}
	allowed := dockerImageToken(t, s, a, "exact-digest", []string{"containers:read"}, []string{InstanceResource(instance.ID), DockerImageResource(instance.ID, dockerImageFixtureRef)})
	raw, _ := json.Marshal(IDInput{ID: instance.ID})
	out, err := s.Execute(context.Background(), allowed, "docker_images_list", raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(out)
	var view struct {
		Items []dockerImageView `json:"items"`
	}
	if json.Unmarshal(encoded, &view) != nil || len(view.Items) != 1 || view.Items[0].Image != dockerImageFixtureRef {
		t.Fatalf("digest allowlist mismatch: %s", encoded)
	}
	for _, secret := range []string{alias, "SECRET_TAG", "PRIVATE_LABEL", "tag-only"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("ungranted alias or raw metadata leaked")
		}
	}
}

func TestDockerRegistryDurablePullRealAuthenticatedUnixIPC(t *testing.T) {
	s, a, m := registryTestService(t)
	ctx := context.Background()
	e, instance := dockerImageFixtureAgent(t)
	if err := os.MkdirAll(instance.Paths.RunDir, 0700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(instance.Paths.RunDir, "docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan string, 1)
	engine := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoded, _ := base64.URLEncoding.DecodeString(r.Header.Get("X-Registry-Auth"))
		seen <- string(encoded)
		http.Error(w, "PRIVATE_PASSWORD_SENTINEL", 502)
	})}
	go engine.Serve(l)
	t.Cleanup(func() { engine.Close() })
	socket := filepath.Join(t.TempDir(), "agent.sock")
	server, err := agent.NewUnixServer(socket, []uint32{uint32(os.Getuid())}, e)
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(ctx)
	t.Cleanup(func() { server.Close() })
	client, err := agent.NewUnixClient(socket, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	if err = s.RegisterExecutor([]string{"docker_registry_pull"}, NewDockerRegistryExecutor(m, client)); err != nil {
		t.Fatal(err)
	}
	created, err := s.Execute(ctx, a, "docker_registry_credential_create", json.RawMessage(`{"name":"private","registry":"registry.example","username":"robot","password":"PRIVATE_PASSWORD_SENTINEL"}`))
	if err != nil {
		t.Fatal(err)
	}
	id := created.(map[string]any)["id"].(string)
	badHost, _ := json.Marshal(DockerPullInput{ID: instance.ID, Image: strings.Replace(dockerImageFixtureRef, "registry.example", "other.example", 1), CredentialID: id, CredentialRevision: 1})
	if _, err = s.Execute(ctx, a, "docker_image_pull", badHost); core.ErrorCode(err) != "invalid_registry" {
		t.Fatal("cross-registry queued pull accepted", err)
	}
	limited := dockerImageToken(t, s, a, "software-only-pull", []string{"containers:manage"}, []string{InstanceResource(instance.ID)})
	pull, _ := json.Marshal(DockerPullInput{ID: instance.ID, Image: dockerImageFixtureRef, CredentialID: id, CredentialRevision: 1})
	if _, err = s.Execute(ctx, limited, "docker_image_pull", pull); core.ErrorCode(err) != "forbidden" {
		t.Fatal("pull without image/credential grants accepted", err)
	}
	view, err := s.Execute(ctx, a, "docker_image_pull", pull)
	if err != nil {
		t.Fatal(err)
	}
	jobID := view.(map[string]any)["id"].(string)
	select {
	case got := <-seen:
		if !strings.Contains(got, "PRIVATE_PASSWORD_SENTINEL") {
			t.Fatal("IPC did not carry resolved pinned auth")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("durable worker did not reach Unix Docker API")
	}
	var final map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		value, err := s.Execute(ctx, a, "tasks_get", json.RawMessage(`{"id":"`+jobID+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		final = value.(map[string]any)
		if final["status"] == jobs.Failed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	deletion, _ := json.Marshal(map[string]any{"id": id, "expected_revision": 1})
	if _, err = s.Execute(ctx, a, "docker_registry_credential_delete", deletion); err != nil {
		t.Fatal(err)
	}
	if _, err = NewDockerRegistryExecutor(m, client).Run(ctx, agent.Request{ActorID: a.ID, Operation: "docker_registry_pull", Input: pull}); core.ErrorCode(err) != "not_found" {
		t.Fatal("deleted credential allowed queued start", err)
	}
	if final["status"] != jobs.Failed {
		t.Fatal("engine rejection did not fail durable task")
	}
	encoded, _ := json.Marshal(final)
	for _, forbidden := range []string{"PRIVATE_PASSWORD_SENTINEL", "secret_ref", "registry_auth", "serveraddress"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatal("durable task leaked transient auth", forbidden)
		}
	}
}

func TestDockerRegistryPullStreamErrorsNeverEchoCredentials(t *testing.T) {
	for _, body := range []string{`{"error":"PRIVATE_PASSWORD_SENTINEL"}`, `{"errorDetail":{"message":"PRIVATE_PASSWORD_SENTINEL"}}`, `PRIVATE_PASSWORD_SENTINEL`} {
		t.Run(body[:4], func(t *testing.T) {
			e, instance := dockerImageFixtureAgent(t)
			if err := os.MkdirAll(instance.Paths.RunDir, 0700); err != nil {
				t.Fatal(err)
			}
			l, err := net.Listen("unix", filepath.Join(instance.Paths.RunDir, "docker.sock"))
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Error("failed pull reached inspect")
				}
				_, _ = w.Write([]byte(body))
			})}
			go server.Serve(l)
			t.Cleanup(func() { server.Close() })
			raw, _ := json.Marshal(DockerInput{ID: instance.ID, Image: dockerImageFixtureRef, Command: "image_pull", RegistryAuth: &dockerRegistryAuth{"robot", "PRIVATE_PASSWORD_SENTINEL", "registry.example"}})
			reply, err := e.Run(context.Background(), agent.Request{Operation: "docker_action", Input: raw})
			encoded, _ := json.Marshal(reply)
			if err == nil && reply.ExitCode == 0 {
				t.Fatal("registry stream failure reported success")
			}
			if strings.Contains(string(encoded), "PRIVATE_PASSWORD_SENTINEL") || (err != nil && strings.Contains(err.Error(), "PRIVATE_PASSWORD_SENTINEL")) {
				t.Fatal("registry stream echoed secret")
			}
		})
	}
}
