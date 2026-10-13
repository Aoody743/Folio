//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUnixAgentAuthenticatesPeerAndExecutesScopedTask(t *testing.T) {
	dir := t.TempDir()
	files := testFilesystem(t, dir, false, 1024)
	path := filepath.Join(dir, "agent.sock")
	server, err := NewUnixServer(path, []uint32{uint32(os.Getuid())}, NewFileExecutor(files))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	client, err := NewUnixClient(path, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	input, _ := json.Marshal(FileInput{Root: "workspace", Path: "note", Content: "human and AI shared note"})
	result, err := client.Run(ctx, Request{Operation: "file_write", Input: input})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("agent task: %+v %v", result, err)
	}
	data, _ := files.ReadFile("workspace", "note")
	if string(data) != "human and AI shared note" {
		t.Fatal("file not written")
	}
	if err := client.Validate(Request{Operation: "shell", Input: input}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad denial: %v", err)
	}
	badClient, err := NewUnixClient(path, uint32(os.Getuid())+1)
	if err != nil {
		t.Fatal(err)
	}
	defer badClient.Close()
	if err := badClient.Validate(Request{Operation: "file_write", Input: input}); err == nil {
		t.Fatal("server identity not checked")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUnixListenerRejectsUnexpectedPeerAndUnsafeSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.sock")
	listener, err := ListenUnix(path, []uint32{uint32(os.Getuid()) + 1})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan error, 1)
	go func() { _, err := listener.Accept(); accepted <- err }()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 1)
	if _, err := conn.Read(buffer); err == nil {
		t.Fatal("unauthorized socket was left open")
	}
	listener.Close()
	if err := <-accepted; err == nil {
		t.Fatal("unauthorized peer accepted")
	}
	if err := os.WriteFile(path, []byte("existing user file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenUnix(path, []uint32{uint32(os.Getuid())}); err == nil {
		t.Fatal("existing file overwritten")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "existing user file" {
		t.Fatal("existing file modified")
	}
}

func TestPolicyStrictlyLimitsCapabilities(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	policy := Policy{Version: 1, AllowedUIDs: []uint32{uint32(os.Getuid())}, Roots: []RootSpec{{ID: "workspace", Path: dir}}, Operations: []string{"file_hash"}}
	data, _ := json.Marshal(policy)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewPolicyExecutor(loaded)
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	if err := executor.Validate(Request{Operation: "file_write", Input: json.RawMessage(`{"root":"workspace","path":"x","content":"x"}`)}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("policy operation bypassed")
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"allowed_uids":[1],"roots":[],"operations":["file_hash"],"command":"sh"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(path); err == nil {
		t.Fatal("unknown policy field accepted")
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(path); err == nil {
		t.Fatal("world-writable policy accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "policy-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(link); err == nil {
		t.Fatal("symlink policy accepted")
	}
}
