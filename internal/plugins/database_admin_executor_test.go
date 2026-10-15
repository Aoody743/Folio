//go:build linux

package plugins

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/jobs"
)

type databaseAdminIPCFixture struct {
	mu        sync.Mutex
	version   string
	seen      chan databaseAdminInput
	malicious bool
}

func (f *databaseAdminIPCFixture) Validate(r agent.Request) error {
	if r.Operation != "database_admin_action" {
		return agent.ErrUnauthorized
	}
	var in databaseAdminInput
	return parse(r.Input, &in)
}
func (f *databaseAdminIPCFixture) Run(_ context.Context, r agent.Request) (agent.Result, error) {
	var in databaseAdminInput
	_ = json.Unmarshal(r.Input, &in)
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.Command == "rotate" {
		f.seen <- in
		if f.malicious {
			return agent.Result{ExitCode: -1, Stdout: in.Password}, failure("private_error", in.Password, 502)
		}
		f.version = strings.Repeat("b", 32)
	}
	raw, _ := json.Marshal(databaseAdminState{CurrentCredentialVersion: f.version, State: "active", AvailableActions: []string{"rotate"}})
	return agent.Result{ExitCode: 0, Stdout: string(raw)}, nil
}
func databaseAdminAPIService(t *testing.T, malicious bool) (*core.Service, core.Actor, *Manager, *agent.UnixClient, *databaseAdminIPCFixture) {
	t.Helper()
	ctx := context.Background()
	s, err := core.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	setup, err := os.ReadFile(s.SetupSecretFile())
	if err != nil {
		t.Fatal(err)
	}
	actor, err := s.Setup(ctx, strings.TrimSpace(string(setup)), "db-admin", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &databaseAdminIPCFixture{version: strings.Repeat("a", 32), seen: make(chan databaseAdminInput, 8), malicious: malicious}
	m, err := New(s.Runtime("plugins"), Options{Executor: fixture})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterProvider(core.Bindings(m.databaseAdminBindings())); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "agent.sock")
	server, err := agent.NewUnixServer(socket, []uint32{uint32(os.Getuid())}, fixture)
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
	if err = s.RegisterExecutor(DatabaseAdminJobOperations(), NewDatabaseAdminExecutor(m, client)); err != nil {
		t.Fatal(err)
	}
	return s, actor, m, client, fixture
}
func TestDatabaseAdminCustomPasswordPinnedDurableTaskAuthenticatedIPC(t *testing.T) {
	for _, malicious := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "native_error_redaction"}[malicious], func(t *testing.T) {
			s, a, m, _, fixture := databaseAdminAPIService(t, malicious)
			ctx := context.Background()
			password := "PRIVATE_CUSTOM_ADMIN_PASSWORD_SENTINEL"
			prepare, _ := json.Marshal(databaseAdminPrepareInput{InstanceID: "mariadb-admin", CurrentCredentialVersion: strings.Repeat("a", 32), Password: password})
			prepared, err := s.Execute(ctx, a, "managed_database_admin_prepare_password", prepare)
			if err != nil {
				t.Fatal(err)
			}
			version := prepared.(map[string]any)["password_version"].(string)
			if !validDatabaseAdminVersion(version) {
				t.Fatal("custom password pin is not opaque")
			}
			metadata, err := m.deps.State.Get(ctx, "database_admin_password_versions", version)
			if err != nil {
				t.Fatal(err)
			}
			var pin databaseAdminPasswordVersion
			if json.Unmarshal(metadata, &pin) != nil || pin.InstanceID != "mariadb-admin" || strings.Contains(string(metadata), password) {
				t.Fatal("invalid immutable metadata")
			}
			if _, err = m.deps.State.CompareAndSwap(ctx, "database_admin_password_versions", version, 0, metadata); err == nil {
				t.Fatal("immutable pin overwritten")
			}
			rotate, _ := json.Marshal(databaseAdminRotateInput{InstanceID: "mariadb-admin", CurrentCredentialVersion: strings.Repeat("a", 32), PasswordVersion: version})
			view, err := s.Execute(ctx, a, "managed_database_admin_rotate", rotate)
			if err != nil {
				t.Fatal(err)
			}
			id := view.(map[string]any)["id"].(string)
			select {
			case in := <-fixture.seen:
				if in.Password != password || in.CurrentCredentialVersion != strings.Repeat("a", 32) {
					t.Fatal("authenticated IPC did not resolve exact custom pin")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("durable rotation did not reach protected Unix IPC")
			}
			var final map[string]any
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				raw, _ := json.Marshal(map[string]any{"id": id})
				value, err := s.Execute(ctx, a, "tasks_get", raw)
				if err != nil {
					t.Fatal(err)
				}
				final = value.(map[string]any)
				if final["status"] == jobs.Succeeded || final["status"] == jobs.Failed {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			want := jobs.Succeeded
			if malicious {
				want = jobs.Failed
			}
			if final["status"] != want {
				t.Fatal("unexpected durable rotation outcome", final["status"])
			}
			encoded, _ := json.Marshal(final)
			audit, err := s.Execute(ctx, a, "audit_list", json.RawMessage(`{"limit":100}`))
			if err != nil {
				t.Fatal(err)
			}
			auditJSON, _ := json.Marshal(audit)
			for _, forbidden := range []string{password, pin.SecretRef, "secret_ref", "ALTER USER", "ALTER ROLE"} {
				if strings.Contains(string(encoded), forbidden) {
					t.Fatal("durable job exposed private value")
				}
				if strings.Contains(string(auditJSON), forbidden) {
					t.Fatal("audit exposed private value")
				}
			}
		})
	}
}
func TestDatabaseAdminExecutorRejectsPlaintextAndCrossInstancePins(t *testing.T) {
	s, a, m, client, fixture := databaseAdminAPIService(t, false)
	ctx := context.Background()
	executor := NewDatabaseAdminExecutor(m, client)
	if err := executor.Validate(agent.Request{Operation: "database_admin_rotate", Input: json.RawMessage(`{"instance_id":"mariadb-admin","current_credential_version":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","password":"NEVER_DURABLE_PASSWORD"}`)}); core.ErrorCode(err) != "invalid_input" {
		t.Fatal("durable executor accepted plaintext password")
	}
	pin := databaseAdminPasswordVersion{InstanceID: "postgresql-other", CurrentCredentialVersion: strings.Repeat("a", 32), Version: strings.Repeat("d", 32), SecretRef: "sec_unresolvable"}
	data, _ := json.Marshal(pin)
	if _, err := m.deps.State.CompareAndSwap(ctx, "database_admin_password_versions", pin.Version, 0, data); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(databaseAdminRotateInput{InstanceID: "mariadb-admin", CurrentCredentialVersion: pin.CurrentCredentialVersion, PasswordVersion: pin.Version})
	if _, err := executor.Run(ctx, agent.Request{Operation: "database_admin_rotate", ActorID: a.ID, Input: input}); core.ErrorCode(err) != "credential_unavailable" {
		t.Fatal("cross-instance password pin accepted")
	}
	select {
	case <-fixture.seen:
		t.Fatal("invalid pin reached IPC")
	default:
	}
	_ = s
}
