//go:build linux

package plugins

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/jobs"
)

const dockerPrivateImageFixture = "registry.example/team/private-app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const dockerPrivatePasswordFixture = "private-registry-password-canary"

func newDockerRegistryCore(t *testing.T) *core.Service {
	t.Helper()
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0700); err != nil {
		t.Fatal("could not prepare private PowerLinux test data directory", err)
	}
	service, err := core.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

type observedDockerTaskSubmitter struct {
	core.TaskSubmitter
	mu        sync.Mutex
	operation string
	input     []byte
	prepared  int
}

func (o *observedDockerTaskSubmitter) SubmitPrepared(ctx context.Context, actor core.Actor, operation string, input json.RawMessage, metadata core.TaskMetadata, prepare func(core.MetadataStore, jobs.Job) error) (map[string]any, error) {
	o.mu.Lock()
	o.operation = operation
	o.input = append([]byte(nil), input...)
	o.prepared++
	o.mu.Unlock()
	return o.TaskSubmitter.SubmitPrepared(ctx, actor, operation, input, metadata, prepare)
}

func (o *observedDockerTaskSubmitter) snapshot() (string, []byte, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.operation, append([]byte(nil), o.input...), o.prepared
}

type observedDockerSecretStore struct {
	core.ExpiringSecretStore
	mu        sync.Mutex
	puts      int
	deletes   int
	expiring  int
	expiresAt time.Time
}

func (o *observedDockerSecretStore) Put(ctx context.Context, actor core.Actor, resource, name string, value []byte) (string, error) {
	o.mu.Lock()
	o.puts++
	o.mu.Unlock()
	return o.ExpiringSecretStore.Put(ctx, actor, resource, name, value)
}

func (o *observedDockerSecretStore) PutExpiring(ctx context.Context, actor core.Actor, resource, name string, value []byte, expires time.Time) (string, error) {
	o.mu.Lock()
	o.puts++
	o.expiring++
	o.expiresAt = expires
	o.mu.Unlock()
	return o.ExpiringSecretStore.PutExpiring(ctx, actor, resource, name, value, expires)
}

func (o *observedDockerSecretStore) Delete(ctx context.Context, actor core.Actor, resource, ref string) error {
	o.mu.Lock()
	o.deletes++
	o.mu.Unlock()
	return o.ExpiringSecretStore.Delete(ctx, actor, resource, ref)
}

func (o *observedDockerSecretStore) counts() (int, int, int, time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.puts, o.deletes, o.expiring, o.expiresAt
}

type nonExpiringDockerSecretStore struct{ core.SecretStore }

func TestDockerPrivatePullFailsClosedWithoutExpiringSecretStorage(t *testing.T) {
	service := newDockerRegistryCore(t)
	defer service.Close()
	deps := service.Runtime("plugins")
	deps.Secrets = nonExpiringDockerSecretStore{SecretStore: deps.Secrets}
	manager := &Manager{deps: deps}
	input, _ := json.Marshal(map[string]any{
		"id":                "docker-test",
		"image":             dockerPrivateImageFixture,
		"registry_username": "private-registry-user",
		"registry_password": dockerPrivatePasswordFixture,
	})
	_, err := manager.dockerImagePullAction(context.Background(), core.Actor{}, input)
	if core.ErrorCode(err) != "secret_expiry_unavailable" {
		t.Fatalf("private pull with non-expiring storage did not fail closed: %v", err)
	}
}

type dockerRegistryFixture struct {
	listener net.Listener
	mu       sync.Mutex
	header   string
	pullErr  bool
}

func newDockerRegistryFixture(t *testing.T, instance Instance, pullErr bool) *dockerRegistryFixture {
	t.Helper()
	if err := os.MkdirAll(instance.Paths.RunDir, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(instance.Paths.RunDir, "docker.sock"))
	if err != nil {
		t.Fatal("could not create isolated Docker API Unix socket", err)
	}
	fixture := &dockerRegistryFixture{listener: listener, pullErr: pullErr}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
			fixture.mu.Lock()
			fixture.header = r.Header.Get("X-Registry-Auth")
			fixture.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if fixture.pullErr {
				_, _ = fmt.Fprintf(w, "{\"error\":%q}\n", "private registry denied: "+dockerPrivatePasswordFixture)
				return
			}
			_, _ = w.Write([]byte("{\"status\":\"digest verified\"}\n"))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.EscapedPath(), "/v1.45/images/") && strings.HasSuffix(r.URL.EscapedPath(), "/json"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"Id":"sha256:%s","RepoDigests":[%q],"Created":"2026-01-02T03:04:05Z","Architecture":"amd64","Os":"linux","Size":1234}`, strings.Repeat("b", 64), dockerPrivateImageFixture)
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

func (f *dockerRegistryFixture) registryAuth(t *testing.T) (string, string, string) {
	t.Helper()
	f.mu.Lock()
	header := f.header
	f.mu.Unlock()
	if header == "" {
		t.Fatal("private pull did not send a registry auth header")
	}
	decoded, err := base64.URLEncoding.DecodeString(header)
	if err != nil {
		t.Fatal("Docker auth header was not padded base64url encoded", err)
	}
	var auth struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		ServerAddress string `json:"serveraddress"`
	}
	if err = json.Unmarshal(decoded, &auth); err != nil {
		t.Fatal("Docker auth header did not contain an AuthConfig", err)
	}
	return auth.Username, auth.Password, auth.ServerAddress
}

func TestDockerPrivateDigestPullUsesScopedSecretReferenceTaskAndCleansCredential(t *testing.T) {
	for _, pullErr := range []bool{false, true} {
		name := "success"
		if pullErr {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			service := newDockerRegistryCore(t)
			defer service.Close()
			bootstrap, err := os.ReadFile(service.SetupSecretFile())
			if err != nil {
				t.Fatal(err)
			}
			admin, err := service.Setup(ctx, strings.TrimSpace(string(bootstrap)), "docker-private-admin", "correct horse battery staple")
			if err != nil {
				t.Fatal(err)
			}
			executor, instance := dockerImageFixtureAgent(t)
			fixture := newDockerRegistryFixture(t, instance, pullErr)
			deps := service.Runtime("plugins")
			observedTasks := &observedDockerTaskSubmitter{TaskSubmitter: deps.Tasks}
			observedSecrets := &observedDockerSecretStore{ExpiringSecretStore: deps.Secrets.(core.ExpiringSecretStore)}
			deps.Tasks = observedTasks
			deps.Secrets = observedSecrets
			manager, err := New(deps, Options{Executor: executor})
			if err != nil {
				t.Fatal(err)
			}
			if err = service.RegisterProvider(manager); err != nil {
				t.Fatal(err)
			}
			if err = service.RegisterExecutor(AgentJobOperations(), executor); err != nil {
				t.Fatal(err)
			}
			if err = service.RegisterExecutor(append(ManagedJobOperations(), NativeOperations()...), manager); err != nil {
				t.Fatal(err)
			}
			input, _ := json.Marshal(map[string]any{
				"id":                instance.ID,
				"image":             dockerPrivateImageFixture,
				"registry_username": "private-registry-user",
				"registry_password": dockerPrivatePasswordFixture,
			})
			idempotencyKey := "docker-private-pull-" + name
			submitted, err := service.ExecuteWithKey(ctx, admin, "docker_image_pull", input, idempotencyKey)
			if err != nil {
				t.Fatal("private digest pull task was not accepted", err)
			}
			replayed, replayErr := service.ExecuteWithKey(ctx, admin, "docker_image_pull", input, idempotencyKey)
			if replayErr != nil {
				t.Fatal("private digest pull retry was not idempotent", replayErr)
			}
			job, ok := submitted.(map[string]any)
			if !ok {
				t.Fatalf("private pull did not return a task: %#v", submitted)
			}
			jobID, _ := job["id"].(string)
			if jobID == "" || job["operation"] != "docker_registry_pull" {
				t.Fatalf("private pull did not use its secret-resolving task operation: %#v", job)
			}
			replayedJob, ok := replayed.(map[string]any)
			if !ok || replayedJob["id"] != jobID {
				t.Fatalf("idempotent retry returned a different private pull task: %#v", replayed)
			}
			deadline := time.Now().Add(5 * time.Second)
			var final map[string]any
			for time.Now().Before(deadline) {
				value, getErr := service.Execute(ctx, admin, "tasks_get", json.RawMessage(`{"id":"`+jobID+`"}`))
				if getErr != nil {
					t.Fatal("private pull task could not be inspected", getErr)
				}
				final, _ = value.(map[string]any)
				if final != nil && final["status"] != jobs.Queued && final["status"] != jobs.Running {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			wantState := jobs.Succeeded
			if pullErr {
				wantState = jobs.Failed
			}
			if final == nil || final["status"] != wantState {
				t.Fatalf("private pull ended in an unexpected state: %#v", final)
			}
			username, password, registry := fixture.registryAuth(t)
			if username != "private-registry-user" || password != dockerPrivatePasswordFixture || registry != "registry.example" {
				t.Fatalf("Docker did not receive the credentials for the image's registry: %q %q %q", username, password, registry)
			}
			operation, taskInput, preparedCount := observedTasks.snapshot()
			secretPutCount, secretDeleteCount, expiringPutCount, credentialExpires := observedSecrets.counts()
			if preparedCount != 1 || secretPutCount != 1 || expiringPutCount != 1 {
				t.Fatalf("idempotent retry created duplicate durable task/secret state: prepared=%d secret puts=%d expiring puts=%d", preparedCount, secretPutCount, expiringPutCount)
			}
			if secretDeleteCount != 1 {
				t.Fatalf("private pull task did not eagerly remove its credential after completion: deletes=%d", secretDeleteCount)
			}
			if remaining := time.Until(credentialExpires); remaining < dockerRegistryCredentialTTL-time.Minute || remaining > dockerRegistryCredentialTTL {
				t.Fatalf("private registry credential did not receive the bounded 30-minute TTL: remaining=%s", remaining)
			}
			var stored map[string]any
			if operation != "docker_registry_pull" || json.Unmarshal(taskInput, &stored) != nil {
				t.Fatalf("task submitter did not persist the typed private pull descriptor: %s %s", operation, taskInput)
			}
			if len(stored) != 3 || stored["id"] != instance.ID || stored["image"] != dockerPrivateImageFixture || stored["credential_ref"] == "" || strings.Contains(string(taskInput), dockerPrivatePasswordFixture) || strings.Contains(string(taskInput), "private-registry-user") {
				t.Fatalf("durable task contained more than the image, instance and opaque credential reference: %s", taskInput)
			}
			credentialRef, _ := stored["credential_ref"].(string)
			resource := DockerImageResource(instance.ID, dockerPrivateImageFixture)
			if secret, getErr := deps.Secrets.Get(ctx, admin, resource, credentialRef); core.ErrorCode(getErr) != "not_found" || len(secret) != 0 {
				t.Fatal("private credential remained in the vault after task completion", getErr)
			}
			encoded, _ := json.Marshal(final)
			if strings.Contains(string(encoded), dockerPrivatePasswordFixture) || strings.Contains(string(encoded), "private-registry-user") {
				t.Fatal("private pull task result exposed registry credentials", string(encoded))
			}
			if !pullErr {
				result, ok := final["result"].(map[string]any)
				if !ok {
					t.Fatalf("successful pull omitted its bounded result: %#v", final)
				}
				resultJSON, _ := json.Marshal(result)
				if strings.Contains(string(resultJSON), dockerPrivatePasswordFixture) || strings.Contains(string(resultJSON), "private-registry-user") {
					t.Fatal("Docker pull output contained private registry credentials", string(resultJSON))
				}
			}
			audits, err := service.Execute(ctx, admin, "audit_list", json.RawMessage(`{"limit":100}`))
			if err != nil {
				t.Fatal("audit trail could not be inspected", err)
			}
			auditJSON, _ := json.Marshal(audits)
			if strings.Contains(string(auditJSON), dockerPrivatePasswordFixture) || strings.Contains(string(auditJSON), "private-registry-user") {
				t.Fatal("audit metadata exposed private registry credentials", string(auditJSON))
			}
		})
	}
}

// The fixture speaks enough Docker Engine HTTP over a private test Unix socket
// to verify auth-header and task secret handling. It is not a live Docker
// daemon or registry integration test.
