//go:build linux

package files

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

func TestPrivilegedFilesystemProxyUploadAndDownloadExactBytes(t *testing.T) {
	service, _ := fixture(t)
	socket := filepath.Join(t.TempDir(), "agent.sock")
	dispatcher, err := agent.NewDispatcher(map[string]agent.Executor{"fs_roots": &Executor{Service: service}, "fs_stat": &Executor{Service: service}, "fs_upload_begin": &Executor{Service: service}, "fs_upload_status": &Executor{Service: service}, "fs_upload_chunk": &Executor{Service: service}, "fs_upload_commit": &Executor{Service: service}, "fs_download_stream": &Executor{Service: service}})
	if err != nil {
		t.Fatal(err)
	}
	server, err := agent.NewUnixServer(socket, []uint32{uint32(os.Getuid())}, dispatcher)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer server.Close()
	go server.Serve(ctx)
	client, err := agent.NewUnixClient(socket, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	remote := &Remote{Client: client}
	data := bytes.Repeat([]byte("raw\x00\xffartifact"), 180000)
	created, err := remote.Execute(ctx, "human", "fs_upload_begin", encode(Input{Root: "workspace", Path: "artifact.bin", Size: int64(len(data)), SHA256: contentHash(data)}))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(created)
	var upload Upload
	if err := json.Unmarshal(encoded, &upload); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.UploadChunk(ctx, "other-actor", "workspace", upload.ID, 0, io.NopCloser(bytes.NewReader(data)), ""); err == nil {
		t.Fatal("another actor uploaded into a private transfer")
	}
	upload, err = remote.UploadChunk(ctx, "human", "workspace", upload.ID, 0, io.NopCloser(bytes.NewReader(data)), contentHash(data))
	if err != nil || upload.Offset != int64(len(data)) {
		t.Fatalf("stream chunk: %+v %v", upload, err)
	}
	if _, err := remote.Execute(ctx, "human", "fs_upload_commit", encode(Input{Root: "workspace", ID: upload.ID})); err != nil {
		t.Fatal(err)
	}
	reader, entry, err := remote.OpenDownload(ctx, "mcp", Ref{"workspace", "artifact.bin"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	actual, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(actual, data) || entry.Size != int64(len(data)) {
		t.Fatalf("proxy download bytes changed: %d/%d %v", len(actual), len(data), err)
	}
	reader, _, err = remote.OpenDownload(ctx, "human", Ref{"workspace", "artifact.bin"}, 10, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	actual, err = io.ReadAll(reader)
	if err != nil || !bytes.Equal(actual, data[10:40]) {
		t.Fatal("range download changed bytes", err)
	}
}
