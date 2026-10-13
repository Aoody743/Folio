package terminal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

func TestStaleSSHCredentialRevisionNeverDialsChangedTarget(t *testing.T) {
	manager := ptyFixture(t, 60)
	ctx := context.Background()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	portNumber, _ := strconv.Atoi(port)
	target, err := manager.CreateTarget(ctx, "human", Input{Label: "Before", Host: host, Port: portNumber, Username: "alice", AuthType: "password", SecretRef: "old-bound-secret", HostKey: "SHA256:" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := manager.UpdateTarget(ctx, "human", Input{ID: target.ID, ExpectedRevision: target.Revision, Username: "bob", SecretRef: "new-bound-secret"})
	if err != nil || changed.Revision != 2 {
		t.Fatal("target update failed", err)
	}
	executor := &AgentExecutor{Manager: manager}
	payload, _ := json.Marshal(AgentPayload{Input: Input{TargetID: target.ID, ExpectedRevision: target.Revision}, Credential: &Credential{Password: []byte("old-resolved-password")}})
	_, err = executor.Run(ctx, agent.Request{ActorID: "human", Operation: "terminal_open", Input: payload})
	typed, ok := err.(*Error)
	if !ok || typed.Code != "conflict" {
		t.Fatalf("stale credential not rejected before dial: %v", err)
	}
	listener.(*net.TCPListener).SetDeadline(time.Now().Add(100 * time.Millisecond))
	if connection, err := listener.Accept(); err == nil {
		connection.Close()
		t.Fatal("stale credential operation contacted new target")
	}
	payload, _ = json.Marshal(AgentPayload{Input: Input{TargetID: target.ID}, Credential: &Credential{Password: []byte("old-resolved-password")}})
	if err := executor.Validate(agent.Request{ActorID: "human", Operation: "terminal_open", Input: payload}); err == nil {
		t.Fatal("credential without a bound target revision accepted")
	}
}
