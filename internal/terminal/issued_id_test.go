package terminal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

func TestTrustedIssuedIDsBindTargetsAndCommandsWithoutOverwrite(t *testing.T) {
	m := ptyFixture(t, 60)
	executor := &AgentExecutor{Manager: m}
	ctx := context.Background()
	targetInput := Input{Label: "Pinned target", Host: "example.com", Username: "fixture", AuthType: "key", SecretRef: "resource-bound-ref", HostKey: "SHA256:" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))}
	payload := AgentPayload{Input: targetInput, IssuedID: "authorized-target"}
	encoded, _ := json.Marshal(payload)
	request := agent.Request{ActorID: "human", Operation: "terminal_targets_create", Input: encoded}
	result, err := executor.Run(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	var target Target
	json.Unmarshal(result.Data, &target)
	if target.ID != payload.IssuedID || target.CreatedBy != "human" || target.SecretRef != targetInput.SecretRef {
		t.Fatalf("issued identity was not retained: %+v", target)
	}
	if _, err := executor.Run(ctx, request); err == nil {
		t.Fatal("duplicate issued ID overwrote target")
	}
	if _, err := m.CreateTargetWithID(ctx, "human", "local", targetInput); err == nil {
		t.Fatal("trusted local profile overwritten")
	}
	if _, err := m.CreateTargetWithID(ctx, "human", "../escape", targetInput); err == nil {
		t.Fatal("unsafe issued target ID accepted")
	}
	command, err := m.SaveCommandWithID(ctx, "mcp", "authorized-command", Input{Title: "Inspect", Command: "pwd"})
	if err != nil || command.ID != "authorized-command" || command.CreatedBy != "mcp" {
		t.Fatalf("command identity: %+v %v", command, err)
	}
	if _, err := m.SaveCommandWithID(ctx, "mcp", command.ID, Input{Title: "Replace", Command: "true"}); err == nil {
		t.Fatal("duplicate command creation overwrote existing record")
	}
	updated, err := m.SaveCommandWithID(ctx, "mcp", command.ID, Input{ID: command.ID, ExpectedRevision: 1, Title: "Inspect again", Command: "id"})
	if err != nil || updated.Revision != 2 {
		t.Fatalf("command update: %+v %v", updated, err)
	}
}
