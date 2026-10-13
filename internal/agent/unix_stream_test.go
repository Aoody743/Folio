//go:build linux

package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUIDCheckedUnixBinaryStreamPreservesFullArtifact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.sock")
	server, err := NewUnixServer(path, []uint32{uint32(os.Getuid())}, helperRunner(t, 20000000000))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer server.Close()
	go server.Serve(ctx)
	client, err := NewUnixClient(path, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	file, err := os.Create(filepath.Join(t.TempDir(), "artifact"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	payload := strings.Repeat("binary-dump-fixture\x00\xff", 100000)
	request := helperRequest("copy")
	request.ActorID = "token:verified-caller"
	request.JobID = "durable-job"
	result, err := client.Stream(ctx, request, IO{Stdin: io.NopCloser(strings.NewReader(payload)), Stdout: file})
	if err != nil {
		t.Fatal(err)
	}
	if result.StdoutBytes != int64(len(payload)) || result.Stdout != "" {
		t.Fatalf("incorrect streaming result: %+v", result)
	}
	file.Seek(0, 0)
	content, err := io.ReadAll(file)
	if err != nil || string(content) != payload {
		t.Fatal("Unix stream artifact bytes were lost", err)
	}
}
