package terminal

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

	"github.com/Aoody743/PowerLinux/internal/agent"
)

func ptyFixture(t *testing.T, lease int) *Manager {
	t.Helper()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	manager, err := New(Options{Config: Config{APIUser: account.Username, StateDir: filepath.Join(home, "state"), Profiles: []LocalProfile{{ID: "local", User: account.Username, Shell: "/bin/bash", Home: home, WorkDir: home}}, LeaseSeconds: lease}, UnsafeSameUIDForTests: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	return manager
}
func waitOutput(t *testing.T, m *Manager, actor, id string, after uint64, needle string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	text := ""
	for time.Now().Before(deadline) {
		out, err := m.Read(context.Background(), actor, Input{ID: id, After: after})
		if err != nil {
			t.Fatal(err)
		}
		for _, frame := range out.Frames {
			data, err := base64.StdEncoding.DecodeString(frame.Data)
			if err != nil {
				t.Fatal(err)
			}
			text += string(data)
			after = frame.Sequence
		}
		if strings.Contains(text, needle) {
			return text
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("terminal did not return %q: %q", needle, text)
	return ""
}
func TestRealPTYResizeOutputAndActorBoundary(t *testing.T) {
	m := ptyFixture(t, 60)
	ctx := context.Background()
	info, err := m.Open(ctx, "human", OpenInput{TargetID: "local", Rows: 29, Cols: 101}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.ProcessID == 0 {
		t.Fatal("no real PTY process")
	}
	payload := base64.StdEncoding.EncodeToString([]byte("printf 'PTY_NATIVE_RESULT'; stty size\n"))
	if _, err := m.Input(ctx, "human", Input{ID: info.ID, Data: payload}); err != nil {
		t.Fatal(err)
	}
	out := waitOutput(t, m, "human", info.ID, 0, "29 101")
	if !strings.Contains(out, "PTY_NATIVE_RESULT") {
		t.Fatal("PTY did not execute command")
	}
	if _, err := m.Resize(ctx, "human", Input{ID: info.ID, Rows: 37, Cols: 93}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Read(ctx, "unrelated", Input{ID: info.ID}); err == nil {
		t.Fatal("session leaked to another actor")
	}
	if _, err := m.Input(ctx, "human", Input{ID: info.ID, Data: base64.StdEncoding.EncodeToString(make([]byte, 16385))}); err == nil {
		t.Fatal("oversized input accepted")
	}
	m.CloseSession(ctx, "human", Input{ID: info.ID})
}
func TestSharedActorViaTrustedAgentPayloadAndLease(t *testing.T) {
	m := ptyFixture(t, 1)
	ctx := context.Background()
	info, err := m.Open(ctx, "human", OpenInput{TargetID: "local"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	executor := &AgentExecutor{Manager: m}
	payload, _ := json.Marshal(AgentPayload{Input: Input{ID: info.ID, Data: base64.StdEncoding.EncodeToString([]byte("printf 'SHARED_MCP_VALUE\\n'\n"))}, AuthorizedShared: true})
	if _, err := executor.Run(ctx, agent.Request{ActorID: "mcp", Operation: "terminal_input", Input: payload}); err != nil {
		t.Fatal(err)
	}
	waitOutput(t, m, "human", info.ID, 0, "SHARED_MCP_VALUE")
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		state, _ := m.Info(info.ID)
		if state.Exited {
			if state.Reason != "expired" {
				t.Fatal("wrong terminal lease reason", state)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("abandoned terminal lease did not stop process")
}
func TestProductionSameAPIIdentityRejectedAndPayloadSpoofRejected(t *testing.T) {
	account, _ := user.Current()
	home := t.TempDir()
	_, err := New(Options{Config: Config{APIUser: account.Username, StateDir: filepath.Join(home, "state"), Profiles: []LocalProfile{{ID: "local", User: account.Username, Shell: "/bin/bash", Home: home, WorkDir: home}}}})
	if err == nil {
		t.Fatal("production terminal accepted API identity")
	}
	m := ptyFixture(t, 60)
	executor := &AgentExecutor{Manager: m}
	if err := executor.Validate(agent.Request{Operation: "terminal_open", Input: json.RawMessage(`{"input":{"target_id":"local"},"actor_id":"admin"}`)}); err == nil {
		t.Fatal("caller-controlled actor accepted")
	}
}
func TestTargetCommandPersistenceAndHostKeyRequirement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	m, err := New(Options{Config: Config{StateDir: path}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	in := Input{Label: "test", Host: "127.0.0.1", Port: 22, Username: "alice", AuthType: "key", SecretRef: "bound-ref"}
	if _, err := m.CreateTarget(ctx, "human", in); err == nil {
		t.Fatal("unpinned SSH target accepted")
	}
	in.HostKey = "SHA256:" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	target, err := m.CreateTarget(ctx, "human", in)
	if err != nil {
		t.Fatal(err)
	}
	command, err := m.SaveCommand(ctx, "human", Input{Title: "Status", Command: "uname -a"})
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	restarted, err := New(Options{Config: Config{StateDir: path}})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	loaded, err := restarted.GetTarget(ctx, target.ID)
	if err != nil || loaded.SecretRef != "bound-ref" {
		t.Fatal("target did not persist", err)
	}
	commands, err := restarted.Commands(ctx)
	if err != nil || len(commands) != 1 || commands[0].ID != command.ID {
		t.Fatal("commands did not persist", err)
	}
	raw, err := os.ReadFile(filepath.Join(path, "terminal_targets", target.ID+".json"))
	if err != nil || strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatal("credentials leaked into metadata")
	}
}
