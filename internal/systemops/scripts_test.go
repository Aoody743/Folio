package systemops

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

// Ordinary-UID fixtures explicitly opt out of the production separate-UID
// rule. Root/independent-UID separation is a separate real VM requirement.
func fixtureScriptExecutor(t *testing.T, timeout int) *HostExecutor {
	t.Helper()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	// This cloud workspace maps base-image owners to nobody. A private copied
	// executable is owned by the fixture Agent and preserves the same validator.
	shell := filepath.Join(home, "bash")
	value, err := os.ReadFile("/bin/bash")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(shell, value, 0700); err != nil {
		t.Fatal(err)
	}
	config := HostConfig{Version: 1, StateDir: filepath.Join(home, "root-state"), APIUser: account.Username, EnabledKinds: []string{"script_execution"}, ScriptProfiles: []ScriptProfile{{ID: "local", User: account.Username, Shell: shell, Home: home, WorkDir: home, TimeoutSeconds: timeout}}}
	if err = ValidateHostConfig(config); err == nil {
		t.Fatal("production validation accepted scripts under API UID")
	}
	executor, err := NewHostExecutorWithOptions(config, HostOptions{UnsafeSameScriptUIDForTests: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { executor.Close() })
	return executor
}
func TestScriptStdinUsesTrustedProfileAndPrivateEnvironment(t *testing.T) {
	executor := fixtureScriptExecutor(t, 5)
	badEnv := filepath.Join(t.TempDir(), "bash-env")
	t.Setenv("BASH_ENV", badEnv)
	if err := os.WriteFile(badEnv, []byte("printf ENV_LEAK\n"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(scriptAgentInput{ActorID: "actor", ProfileID: "local"})
	result, err := executor.Stream(context.Background(), agent.Request{Operation: "system_script_run", Input: raw}, agent.IO{Stdin: io.NopCloser(strings.NewReader("printf 'PRIVATE_STDIN_OK\\n'\n"))})
	if err != nil || result.ExitCode != 0 || result.Stdout != "PRIVATE_STDIN_OK\n" {
		t.Fatal(result, err)
	}
	if _, err = executor.Run(context.Background(), agent.Request{Operation: "system_script_run", Input: raw}); err == nil {
		t.Fatal("ordinary JSON script body pathway accepted")
	}
	raw = json.RawMessage(`{"actor_id":"actor","profile_id":"local","program":"/bin/other"}`)
	if err = executor.Validate(agent.Request{Operation: "system_script_run", Input: raw}); err == nil {
		t.Fatal("untrusted executable field accepted")
	}
}

func TestScriptProfileRejectsExistingSharedPrimaryGroup(t *testing.T) {
	// Debian/Ubuntu's existing _apt/nobody identities have distinct non-root
	// UIDs but the same primary group. No passwd/group file is modified and
	// no subprocess is launched under either account.
	api, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("the distro's existing shared-primary-group account fixture is absent")
	}
	profile, err := user.Lookup("_apt")
	if err != nil || api.Uid == profile.Uid || api.Gid != profile.Gid || profile.Uid == "0" {
		t.Skip("the distro's existing shared-primary-group account fixture is absent")
	}
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	config := HostConfig{Version: 1, StateDir: state, APIUser: api.Username, EnabledKinds: []string{"script_execution"}, ScriptProfiles: []ScriptProfile{{ID: "local", User: profile.Username, Shell: "/bin/bash", Home: dir, WorkDir: dir}}}
	if err := ValidateHostConfig(config); err == nil || !strings.Contains(err.Error(), "distinct from the API GID") {
		t.Fatalf("distinct UIDs bypassed the shared API-group boundary: %v", err)
	}
	if _, err := os.Lstat(state); !os.IsNotExist(err) {
		t.Fatal("rejected identity created host state")
	}
}

func TestOwnedVMRejectsUnprivilegedScriptPrivilegedGroup(t *testing.T) {
	name := os.Getenv("POWERLINUX_TEST_WRONG_GID_USER")
	if name == "" || os.Geteuid() != 0 {
		t.Skip("requires a named wrong-primary-group account in the exclusively owned root QA VM")
	}
	profile, err := user.Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	apiName := os.Getenv("POWERLINUX_TEST_API_USER")
	if apiName == "" {
		apiName = "powerlinux"
	}
	api, err := user.Lookup(apiName)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Uid == "0" || profile.Uid == api.Uid || (profile.Gid != "0" && profile.Gid != api.Gid) {
		t.Fatal("named VM account is not a distinct non-root UID with root/API primary group")
	}
	state := filepath.Join(t.TempDir(), "state")
	config := HostConfig{Version: 1, StateDir: state, APIUser: api.Username, EnabledKinds: []string{"script_execution"}, ScriptProfiles: []ScriptProfile{{ID: "local", User: profile.Username, Shell: "/bin/bash", Home: profile.HomeDir, WorkDir: profile.HomeDir}}}
	for _, rootEnabled := range []bool{false, true} {
		config.EnableRootScripts = rootEnabled
		for _, construct := range []bool{false, true} {
			if construct {
				executor, err := NewHostExecutor(config)
				if executor != nil {
					executor.Close()
				}
				if err == nil || !strings.Contains(err.Error(), "script execution GID") {
					t.Fatalf("constructor accepted a privileged primary group (root=%v): %v", rootEnabled, err)
				}
			} else if err := ValidateHostConfig(config); err == nil || !strings.Contains(err.Error(), "script execution GID") {
				t.Fatalf("pure policy check accepted a privileged primary group (root=%v): %v", rootEnabled, err)
			}
			if _, err := os.Lstat(state); !os.IsNotExist(err) {
				t.Fatal("rejected primary group created host state")
			}
		}
	}
}
func TestScriptRejectsOversizeBeforeExecutionAndCancelsGroup(t *testing.T) {
	executor := fixtureScriptExecutor(t, 5)
	raw, _ := json.Marshal(scriptAgentInput{ActorID: "actor", ProfileID: "local"})
	_, err := executor.Stream(context.Background(), agent.Request{Operation: "system_script_run", Input: raw}, agent.IO{Stdin: io.NopCloser(strings.NewReader("printf SHOULD_NOT_RUN\n" + strings.Repeat("#", 32<<10)))})
	if !errors.Is(err, agent.ErrInvalidInput) {
		t.Fatal("oversized script accepted", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := executor.Stream(ctx, agent.Request{Operation: "system_script_run", Input: raw}, agent.IO{Stdin: io.NopCloser(strings.NewReader("sleep 20 &\nwait\n"))})
	if !errors.Is(err, context.DeadlineExceeded) || !result.TimedOut || time.Since(started) > 3*time.Second {
		t.Fatal("process group cancellation failed", result, err, time.Since(started))
	}
}
func TestEncryptedScriptRevisionDurableTaskAndManualAuthority(t *testing.T) {
	service, provider, admin := actualProvider(t, nil)
	provider.options.Agent = fixtureScriptExecutor(t, 5)
	body := "printf 'TASK_SCRIPT_OK\\n'\n# fixture-private-script-body-493\n"
	script := action(t, service, admin, "scripts_save", map[string]any{"name": "backup-step", "profile_id": "local", "body": body}).(Script)
	if script.SecretRef != "" || script.Revision != 1 {
		t.Fatal(script)
	}
	stored, err := provider.loadScript(context.Background(), script.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SecretRef == "" {
		t.Fatal("missing encrypted script reference")
	}
	metadata, err := provider.deps.State.Get(context.Background(), "script", script.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(metadata), body) {
		t.Fatal("script body stored in metadata")
	}
	result := action(t, service, admin, "scripts_run", map[string]any{"id": script.ID, "expected_revision": 1}).(map[string]any)
	id := result["task_id"].(string)
	deadline := time.Now().Add(5 * time.Second)
	var task map[string]any
	for time.Now().Before(deadline) {
		task = action(t, service, admin, "tasks_get", map[string]any{"id": id}).(map[string]any)
		if task["status"] == "succeeded" || task["status"] == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if task["status"] != "succeeded" {
		t.Fatal(task)
	}
	encoded, _ := json.Marshal(task)
	if strings.Contains(string(encoded), "fixture-private-script-body-493") {
		t.Fatal("plaintext script persisted in task")
	}
	updated := action(t, service, admin, "scripts_save", map[string]any{"id": script.ID, "expected_revision": 1, "name": "backup-step", "profile_id": "local", "body": "printf updated\n"}).(Script)
	if updated.Revision != 2 {
		t.Fatal(updated)
	}
	raw, _ := json.Marshal(map[string]any{"id": script.ID, "expected_revision": 1})
	if _, err = service.Execute(context.Background(), admin, "scripts_run", raw); core.ErrorStatus(err) != 409 {
		t.Fatal("stale script executed", err)
	}
	get := action(t, service, admin, "scripts_get", map[string]any{"id": script.ID, "include_body": true}).(map[string]any)
	if get["body"] != "printf updated\n" {
		t.Fatal(get)
	}
	audit := action(t, service, admin, "audit_list", map[string]any{"limit": 100})
	encoded, _ = json.Marshal(audit)
	if strings.Contains(string(encoded), "fixture-private-script-body-493") {
		t.Fatal("script body entered audit")
	}
}

func TestRealUIDCheckedUnixScriptStream(t *testing.T) {
	executor := fixtureScriptExecutor(t, 5)
	path := filepath.Join(t.TempDir(), "agent.sock")
	server, err := agent.NewUnixServer(path, []uint32{uint32(os.Getuid())}, executor)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		server.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Unix server did not stop")
		}
	})
	client, err := agent.NewUnixClient(path, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(scriptAgentInput{ActorID: "actor", ProfileID: "local"})
	result, err := client.Stream(context.Background(), agent.Request{Operation: "system_script_run", Input: raw}, agent.IO{Stdin: io.NopCloser(strings.NewReader("printf UNIX_STDIN_OK\n"))})
	if err != nil || result.ExitCode != 0 || result.Stdout != "UNIX_STDIN_OK" {
		t.Fatal(result, err)
	}
}
