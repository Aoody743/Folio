package plugins

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type databaseDownloadKey struct{}
type databasePreparedWriter interface {
	io.WriteCloser
	Prepare(DatabaseBackup) (int64, int64, error)
}
type databaseStreamExecutor interface {
	Stream(context.Context, agent.Request, agent.IO) (agent.Result, error)
}

func databaseDownloadSchema(chunk bool) map[string]any {
	fields := map[string]any{"instance_id": idSchema(), "database": map[string]any{"type": "string", "pattern": databaseIdentifier.String()}, "backup_id": map[string]any{"type": "string", "pattern": artifactName.String()}}
	if chunk {
		fields["offset"] = map[string]any{"type": "integer", "minimum": 0}
		fields["length"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 64 << 10}
	}
	return obj(fields, "instance_id", "database", "backup_id")
}
func (m *Manager) databaseDownloadBindings() []core.Binding {
	bindings := []core.Binding{}
	for _, chunk := range []bool{false, true} {
		name := "managed_database_backup_download"
		description := "Export a verified owned database backup through an authenticated HTTP download"
		if chunk {
			name = "managed_database_backup_read"
			description = "Read one bounded base64 chunk for native MCP clients; authorize every request"
		}
		chunk := chunk
		b := m.binding(name, description, "databases:backup", true, databaseDownloadSchema(chunk), managedDatabaseResources, func(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			var in databaseArtifactInput
			if err := parse(raw, &in); err != nil {
				return core.ActionResult{}, err
			}
			resources, err := managedDatabaseResources(ctx, a, raw)
			if err != nil {
				return core.ActionResult{}, err
			}
			statInput, _ := json.Marshal(ManagedDatabaseInput{InstanceID: in.InstanceID, Database: in.Database, BackupID: in.BackupID, Command: "backup_stat"})
			var backup DatabaseBackup
			if err = m.agentJSON(ctx, "database_action", statInput, &backup); err != nil {
				return core.ActionResult{}, err
			}
			if backup.InstanceID != in.InstanceID || backup.Database != in.Database || backup.ID != in.BackupID {
				return core.ActionResult{}, failure("ownership_invalid", "Backup metadata does not match the resolved database resource", 412)
			}
			result := core.ActionResult{Resources: resources, AuditSummary: map[string]any{"backup_id": backup.ID, "bytes": backup.Bytes, "sha256": backup.SHA256}}
			if chunk {
				if in.Length == 0 {
					in.Length = 16 << 10
				}
				if in.Offset > backup.Bytes {
					return core.ActionResult{}, failure("invalid_input", "Chunk offset exceeds the owned artifact", 400)
				}
				buffer := &boundedDatabaseBuffer{maximum: 64 << 10}
				if err = m.streamDatabaseArtifact(ctx, a, in, buffer); err != nil {
					return core.ActionResult{}, err
				}
				result.Data = map[string]any{"backup": backup, "encoding": "base64", "data": base64.StdEncoding.EncodeToString(buffer.Bytes()), "offset": in.Offset, "next_offset": in.Offset + int64(buffer.Len()), "eof": in.Offset+int64(buffer.Len()) >= backup.Bytes}
				return result, nil
			}
			query := url.Values{"instance_id": {in.InstanceID}, "database": {in.Database}, "backup_id": {in.BackupID}}
			result.Data = map[string]any{"backup": backup, "download_url": "/api/v1/databases/backups/download?" + query.Encode()}
			if writer, ok := ctx.Value(databaseDownloadKey{}).(databasePreparedWriter); ok {
				var prepareErr error
				in.Offset, in.Length, prepareErr = writer.Prepare(backup)
				if prepareErr != nil {
					return core.ActionResult{}, prepareErr
				}
				if err = m.streamDatabaseArtifact(ctx, a, in, writer); err != nil {
					return core.ActionResult{}, err
				}
			}
			return result, nil
		})
		b.Action.Audit = true
		bindings = append(bindings, b)
	}
	return bindings
}
func (m *Manager) streamDatabaseArtifact(ctx context.Context, prior core.Actor, in databaseArtifactInput, writer io.WriteCloser) error {
	a, err := m.deps.ResolveActor(ctx, prior)
	if err != nil {
		return err
	}
	if !m.deps.Authorize(a, "databases:backup", DatabaseResource(in.InstanceID, in.Database)) {
		return failure("forbidden", "Database backup resource is not authorized", 403)
	}
	streamer, ok := m.executor.(databaseStreamExecutor)
	if !ok {
		return failure("runtime_unavailable", "Controlled Agent streaming is required for database backups", 412)
	}
	raw, _ := json.Marshal(in)
	result, err := streamer.Stream(ctx, agent.Request{Operation: "database_artifact", Input: raw, ActorID: a.ID}, agent.IO{Stdout: writer})
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return failure("artifact_transfer_failed", "Database backup stream did not complete", 502)
	}
	return nil
}

type boundedDatabaseBuffer struct {
	bytes.Buffer
	maximum int
}

func (b *boundedDatabaseBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.maximum {
		return 0, failure("result_limit", "Database backup chunk exceeds its bound", 413)
	}
	return b.Buffer.Write(p)
}
func (b *boundedDatabaseBuffer) Close() error { return nil }
