package terminalbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/terminal"
)

type fixture struct {
	service *core.Service
	admin   core.Actor
	manager *terminal.Manager
	bridge  *Provider
}

func setup(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	service, err := core.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	secret, err := os.ReadFile(service.SetupSecretFile())
	if err != nil {
		t.Fatal(err)
	}
	admin, err := service.Setup(ctx, strings.TrimSpace(string(secret)), "admin", "bridge test secure password")
	if err != nil {
		t.Fatal(err)
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	manager, err := terminal.New(terminal.Options{Config: terminal.Config{APIUser: account.Username, StateDir: filepath.Join(home, "state"), Profiles: []terminal.LocalProfile{{ID: "local", Label: "Local test", User: account.Username, Shell: "/bin/bash", Home: home, WorkDir: home}}, LeaseSeconds: 60}, UnsafeSameUIDForTests: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	bridge, err := New(service.Runtime("terminal"), Options{Agent: &terminal.AgentExecutor{Manager: manager}, DispatchWithKey: service.ExecuteWithKey})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.RegisterProvider(bridge); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.Close)
	return fixture{service, admin, manager, bridge}
}
func run(t *testing.T, f fixture, a core.Actor, name string, in any) any {
	t.Helper()
	result, err := f.service.Execute(context.Background(), a, name, encoded(in))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return result
}
func token(t *testing.T, f fixture, scopes, resources []string) (core.Actor, core.TokenCreated) {
	t.Helper()
	created, err := f.service.CreateToken(context.Background(), f.admin, core.TokenRequest{Name: "terminal scoped test", Kind: "mcp", Scopes: scopes, Resources: resources})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := f.service.AuthenticateToken(context.Background(), created.Token)
	if err != nil {
		t.Fatal(err)
	}
	return actor, created
}
func sessionFrom(t *testing.T, value any) terminal.SessionInfo {
	t.Helper()
	var out terminal.SessionInfo
	if json.Unmarshal(encoded(value), &out) != nil || out.ID == "" {
		t.Fatal("no actual session")
	}
	return out
}
func TestRealPTYSharedRESTMCPPermissionAndInputAudit(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	session := sessionFrom(t, run(t, f, f.admin, "terminal_open", map[string]any{"target_id": "local", "rows": 31, "cols": 99}))
	if session.ProcessID == 0 {
		t.Fatal("terminal has no real PTY process")
	}
	noShare, _ := token(t, f, []string{"terminal:connect"}, []string{Resource("local")})
	if _, err := f.service.Execute(ctx, noShare, "terminal_read", encoded(terminal.Input{ID: session.ID})); core.ErrorCode(err) != "forbidden" {
		t.Fatal("another actor read a session without share permission", err)
	}
	shared, created := token(t, f, []string{"terminal:connect", "terminal:share"}, []string{Resource("local")})
	command := "printf 'BRIDGE_REAL_NATIVE'; stty size\n"
	payload := base64.StdEncoding.EncodeToString([]byte(command))
	run(t, f, shared, "terminal_input", map[string]any{"id": session.ID, "data": payload})
	text := ""
	var after uint64
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var read terminal.ReadResult
		result := run(t, f, shared, "terminal_read", map[string]any{"id": session.ID, "after": after, "limit": 8})
		if json.Unmarshal(encoded(result), &read) != nil {
			t.Fatal("invalid read result")
		}
		for _, frame := range read.Frames {
			data, _ := base64.StdEncoding.DecodeString(frame.Data)
			text += string(data)
		}
		after = read.Next
		if strings.Contains(text, "31 99") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(text, "BRIDGE_REAL_NATIVE") || !strings.Contains(text, "31 99") {
		t.Fatal("real terminal output/resize is missing")
	}
	audit := string(encoded(run(t, f, f.admin, "audit_list", map[string]any{"limit": 100})))
	if strings.Contains(audit, payload) || strings.Contains(audit, command) {
		t.Fatal("terminal input leaked into audit")
	}
	if err := f.service.RevokeToken(ctx, f.admin, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Execute(ctx, shared, "terminal_input", encoded(map[string]any{"id": session.ID, "data": payload})); core.ErrorCode(err) != "unauthorized" {
		t.Fatal("revoked actor still wrote terminal", err)
	}
}
func TestPublicFlagsBoundsAndRootExtraAuthorization(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, body := range []map[string]any{{"target_id": "local", "root_authorized": true}, {"target_id": "local", "credential": map[string]any{"password": "spoof"}}, {"label": "x", "host": "localhost", "username": "test", "auth_type": "password", "host_key": "SHA256:" + base64.RawStdEncoding.EncodeToString(make([]byte, 32)), "issued_id": "local", "password": "password"}} {
		name := "terminal_open"
		if body["issued_id"] != nil {
			name = "terminal_targets_create"
		}
		if _, err := f.service.Execute(ctx, f.admin, name, encoded(body)); core.ErrorCode(err) != "invalid_input" {
			t.Fatal("public authorization/identity flag accepted", err)
		}
	}
	session := sessionFrom(t, run(t, f, f.admin, "terminal_open", map[string]any{"target_id": "local"}))
	if _, err := f.service.Execute(ctx, f.admin, "terminal_input", encoded(map[string]any{"id": session.ID, "data": base64.StdEncoding.EncodeToString(make([]byte, 16385))})); core.ErrorCode(err) != "invalid_input" {
		t.Fatal("oversized input accepted", err)
	}
	if _, err := f.service.Execute(ctx, f.admin, "terminal_read", encoded(map[string]any{"id": session.ID, "limit": 9})); core.ErrorCode(err) != "invalid_input" {
		t.Fatal("oversized read accepted", err)
	}
	in := input{Input: terminal.Input{Label: "Root target", Host: "localhost", Username: "root", AuthType: "password", HostKey: "SHA256:" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))}, Password: "test root credential"}
	id := issuedTarget(in)
	narrow, _ := token(t, f, []string{"terminal:manage", "terminal:connect"}, []string{Resource(id)})
	if _, err := f.service.Execute(ctx, narrow, "terminal_targets_create", encoded(in)); core.ErrorCode(err) != "forbidden" {
		t.Fatal("root target created without root/host permission", err)
	}
}
func TestTargetStablePreAuthorizationAndCredentialRebindGuard(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	in := input{Input: terminal.Input{Label: "SSH test", Host: "localhost", Username: "testuser", AuthType: "password", HostKey: "SHA256:" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))}, Password: "protected test password"}
	id := issuedTarget(in)
	wrong, _ := token(t, f, []string{"terminal:manage"}, []string{Resource("unrelated")})
	if _, err := f.service.Execute(ctx, wrong, "terminal_targets_create", encoded(in)); core.ErrorCode(err) != "forbidden" {
		t.Fatal("creation happened outside pre-authorized server ID", err)
	}
	if _, err := f.manager.GetTarget(ctx, id); err == nil {
		t.Fatal("unauthorized target exists")
	}
	out := run(t, f, f.admin, "terminal_targets_create", in)
	var target terminal.Target
	if json.Unmarshal(encoded(out), &target) != nil || target.ID != id || target.SecretRef != "" {
		t.Fatal("creation exposed secret or changed server-issued ID")
	}
	stored, err := f.manager.GetTarget(ctx, id)
	if err != nil || stored.SecretRef == "" {
		t.Fatal("credential was not bound")
	}
	if _, err := f.service.Execute(ctx, f.admin, "terminal_targets_update", encoded(map[string]any{"id": id, "expected_revision": 1, "host": "attacker.example"})); core.ErrorCode(err) != "new_credential_required" {
		t.Fatal("old password rebound to a new host", err)
	}
	unchanged, _ := f.manager.GetTarget(ctx, id)
	if unchanged.Host != "localhost" || unchanged.SecretRef != stored.SecretRef || unchanged.Revision != 1 {
		t.Fatal("rejected update changed target")
	}
	if _, err := f.service.Execute(ctx, f.admin, "terminal_targets_update", encoded(map[string]any{"id": id, "expected_revision": 1, "host": "attacker.example", "secret_ref": stored.SecretRef})); core.ErrorCode(err) != "invalid_input" {
		t.Fatal("public secret reference rebinding accepted", err)
	}
	run(t, f, f.admin, "terminal_targets_update", map[string]any{"id": id, "expected_revision": 1, "label": "Changed label"})
	final, _ := f.manager.GetTarget(ctx, id)
	if final.Revision != 2 || final.SecretRef != stored.SecretRef {
		t.Fatal("label-only update broke stable secret")
	}
}
