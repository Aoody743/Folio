//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

type deadlineStreamExecutor struct {
	validated chan struct{}
	once      sync.Once
}

func (e *deadlineStreamExecutor) Validate(request Request) error {
	if request.Operation != "database_import_artifact" {
		return ErrUnauthorized
	}
	e.once.Do(func() { close(e.validated) })
	return nil
}

func (*deadlineStreamExecutor) Run(context.Context, Request) (Result, error) {
	return Result{}, errors.New("streaming is required")
}

func (e *deadlineStreamExecutor) Stream(ctx context.Context, request Request, streams IO) (Result, error) {
	count, err := io.Copy(io.Discard, streams.Stdin)
	result := Result{Operation: request.Operation, ExitCode: 0, StdoutBytes: count}
	if err != nil {
		result.ExitCode = 1
		return result, err
	}
	return result, ctx.Err()
}

type delayedStreamBody struct {
	validated <-chan struct{}
	delay     time.Duration
	reader    *strings.Reader
	once      sync.Once
}

func (b *delayedStreamBody) Read(buffer []byte) (int, error) {
	b.once.Do(func() {
		<-b.validated
		time.Sleep(b.delay)
	})
	return b.reader.Read(buffer)
}

func (*delayedStreamBody) Close() error { return nil }

func TestAuthorizedDatabaseImportStreamExtendsUnixReadDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.sock")
	executor := &deadlineStreamExecutor{validated: make(chan struct{})}
	server, err := NewUnixServer(path, []uint32{uint32(os.Getuid())}, executor)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the production stream handler against a short initial server
	// timeout without waiting ten seconds. A valid import stream must extend
	// only its already-authorized body deadline before reading the payload.
	server.server.ReadTimeout = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer server.Close()
	go server.Serve(ctx)
	client, err := NewUnixClient(path, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	body := &delayedStreamBody{validated: executor.validated, delay: 1250 * time.Millisecond, reader: strings.NewReader(strings.Repeat("payload-", 100))}
	result, err := client.Stream(ctx, Request{Operation: "database_import_artifact", Input: json.RawMessage(`{"instance_id":"db-target"}`)}, IO{Stdin: body})
	if err != nil {
		var remote *RemoteError
		if errors.As(err, &remote) {
			t.Fatalf("authorized import stream exceeded the server's initial deadline: %s (%s)", remote.Message, remote.Code)
		}
		t.Fatalf("authorized import stream failed over the Unix transport: %v", err)
	}
	if result.ExitCode != 0 || result.StdoutBytes != int64(len(strings.Repeat("payload-", 100))) {
		t.Fatalf("authorized stream did not preserve its delayed payload: %+v", result)
	}
}
