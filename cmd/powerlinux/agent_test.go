package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

func agentHealthFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.sock")
	dispatcher, err := agent.NewDispatcher(map[string]agent.Executor{"agent_health": healthExecutor{agentHealth{Status: "ok", Ready: true, Version: "fixture-v2", PolicySHA256: strings.Repeat("a", 64), Modules: []string{"fixture"}}}, "system_services_list": unavailableExecutor{}})
	if err != nil {
		t.Fatal(err)
	}
	server, err := agent.NewUnixServer(path, []uint32{uint32(os.Getuid())}, dispatcher)
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
			t.Error("Agent server did not stop")
		}
	})
	return path
}
func TestAgentHealthUsesActualUnixPeerAndDisabledModules(t *testing.T) {
	path := agentHealthFixture(t)
	client, err := agent.NewUnixClient(path, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := agent.Request{ActorID: "fixture", Operation: "agent_health", Input: json.RawMessage(`{}`)}
	result, err := client.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var health agentHealth
	if err = json.Unmarshal(result.Data, &health); err != nil {
		t.Fatal(err)
	}
	if !health.Ready || health.Version != "fixture-v2" || health.PolicySHA256 != strings.Repeat("a", 64) {
		t.Fatal(health)
	}
	request.Operation = "system_services_list"
	if _, err = client.Run(context.Background(), request); err == nil {
		t.Fatal("disabled module returned successful fake state")
	}
	wrong, err := agent.NewUnixClient(path, uint32(os.Getuid()+1))
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	request.Operation = "agent_health"
	if _, err = wrong.Run(context.Background(), request); err == nil {
		t.Fatal("wrong kernel server UID accepted")
	}
}
func TestAgentHealthCLIRequiresRootPeerAndExactVersionPolicy(t *testing.T) {
	path := agentHealthFixture(t)
	if os.Geteuid() != 0 {
		if err := agentHealthcheck([]string{"--socket", path, "--timeout", "1s"}); err == nil {
			t.Fatal("production health CLI trusted an ordinary UID server")
		}
		return
	}
	if err := agentHealthcheck([]string{"--socket", path, "--expected-version", "fixture-v2", "--expected-policy-sha256", strings.Repeat("a", 64), "--timeout", "1s"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--socket", path, "--expected-version", "wrong"}, {"--socket", path, "--expected-policy-sha256", strings.Repeat("b", 64)}} {
		if err := agentHealthcheck(args); err == nil {
			t.Fatal("incompatible readiness accepted", args)
		}
	}
}
func TestPureAgentCheckDoesNotCreateRuntimeDirectories(t *testing.T) {
	dir := t.TempDir()
	policy := filepath.Join(dir, "policy.json")
	socket := filepath.Join(dir, "uncreated-runtime", "agent.sock")
	value, _ := json.Marshal(map[string]any{"version": 1, "allowed_uids": []uint32{uint32(os.Getuid())}, "roots": []map[string]any{{"id": "fixture", "path": dir}}, "operations": []string{"file_hash"}})
	if err := os.WriteFile(policy, value, 0600); err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	err = serveAgent([]string{"--policy", policy, "--socket", socket, "--check-policy"})
	os.Stdout = original
	writer.Close()
	output, _ := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), `"valid":true`) {
		t.Fatal(string(output))
	}
	if _, err = os.Stat(filepath.Dir(socket)); !os.IsNotExist(err) {
		t.Fatal("pure CLI created runtime directory", err)
	}
}
