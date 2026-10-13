//go:build linux

package plugins

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

// Uses the production UID-checked Unix HTTP pipe. The file is an isolated
// integrity fixture, not evidence of database installation or dump generation.
func TestDatabaseArtifactRealUnixPipeIgnoresEmptyStdinAndChecksIntegrity(t *testing.T) {
	e := testAgent(t)
	id := "postgresql-stream"
	instance := Instance{ID: id, PluginID: "postgresql", InstanceID: "stream", Paths: e.instancePaths("postgresql", "stream"), Revision: 1, Ownership: "managed", Service: ServiceProfile{ID: id, Unit: "powerlinux-plugin-" + id + ".service"}}
	if err := e.save("receipts", id, instance); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(instance.Paths.BackupDir, 0700); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("owned-database-byte-fixture\x00\xff"), 5000)
	digest := sha256.Sum256(data)
	backup := DatabaseBackup{ID: "dbbackup_stream_fixture", InstanceID: id, Database: "owned_db", Engine: "postgresql", Filename: "dbbackup_stream_fixture.dump", Bytes: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}
	filename := filepath.Join(instance.Paths.BackupDir, backup.Filename)
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.save("private", backup.ID, backup); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "agent.sock")
	server, err := agent.NewUnixServer(socket, []uint32{uint32(os.Getuid())}, e)
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
	in := databaseArtifactInput{InstanceID: id, Database: backup.Database, BackupID: backup.ID}
	run := func(in databaseArtifactInput) (agent.Result, *bufferSink, error) {
		raw, _ := json.Marshal(in)
		sink := &bufferSink{}
		result, err := client.Stream(ctx, agent.Request{Operation: "database_artifact", Input: raw, ActorID: "token:verified-fixture"}, agent.IO{Stdout: sink})
		return result, sink, err
	}
	result, sink, err := run(in)
	if err != nil || result.ExitCode != 0 || result.StdoutBytes != int64(len(data)) || !bytes.Equal(sink.Bytes(), data) {
		t.Fatal("real Unix database export failed", core.ErrorCode(err), result.StdoutBytes, sink.Len())
	}
	chunk := in
	chunk.Offset = 7
	chunk.Length = 64 << 10
	result, sink, err = run(chunk)
	if err != nil || result.ExitCode != 0 || !bytes.Equal(sink.Bytes(), data[7:7+(64<<10)]) {
		t.Fatal("Unix export chunk lost bytes", err)
	}
	cross := in
	cross.Database = "other_db"
	_, sink, err = run(cross)
	var remote *agent.RemoteError
	if !errors.As(err, &remote) || remote.Code != "forbidden" || remote.Status != 403 || sink.Len() != 0 {
		t.Fatal("cross-database Unix export was permitted")
	}
	data[0] ^= 1
	if err = os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	_, sink, err = run(in)
	if !errors.As(err, &remote) || remote.Code != "checksum_mismatch" || remote.Status != 412 || sink.Len() != 0 {
		t.Fatal("corrupt database artifact exported bytes before verification")
	}
}
