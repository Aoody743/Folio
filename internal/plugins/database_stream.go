package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

type databaseArtifactInput struct {
	InstanceID string `json:"instance_id"`
	Database   string `json:"database"`
	BackupID   string `json:"backup_id"`
	Offset     int64  `json:"offset,omitempty"`
	Length     int64  `json:"length,omitempty"`
}

func (e *PrivilegedExecutor) validateDatabaseArtifact(raw json.RawMessage) error {
	var in databaseArtifactInput
	if err := parse(raw, &in); err != nil {
		return err
	}
	if !validInstanceID(in.InstanceID) || !validDatabaseName(in.Database) || !artifactName.MatchString(in.BackupID) || in.Offset < 0 || in.Length < 0 {
		return failure("invalid_input", "Invalid database backup stream descriptor", 400)
	}
	if in.Offset != 0 && in.Length == 0 {
		return failure("invalid_input", "A bounded chunk length is required for an offset", 400)
	}
	return nil
}
func (e *PrivilegedExecutor) databaseBackupMetadata(ctx context.Context, instance Instance, database, id string) (DatabaseBackup, error) {
	var backup DatabaseBackup
	if err := e.load("private", id, &backup); err != nil {
		return backup, err
	}
	if backup.ID != id || backup.InstanceID != instance.ID || backup.Database != database || backup.Engine != instance.PluginID || backup.Filename != backup.ID+".dump" || backup.Bytes < 1 || len(backup.SHA256) != 64 {
		return backup, failure("forbidden", "Backup belongs to a different database or instance", 403)
	}
	if backup.State == "deleted" {
		return backup, failure("not_found", "Owned backup was deleted", 404)
	}
	if backup.State != "" && backup.State != "active" {
		return backup, failure("operation_recovery_required", "Owned backup has an unfinished deletion receipt", 409)
	}
	root, err := os.OpenRoot(instance.Paths.BackupDir)
	if err != nil {
		return backup, err
	}
	defer root.Close()
	info, err := root.Lstat(backup.Filename)
	if err != nil || !info.Mode().IsRegular() || info.Size() != backup.Bytes {
		return backup, failure("artifact_invalid", "Backup file differs from its owned receipt", 412)
	}
	return backup, nil
}
func (e *PrivilegedExecutor) openVerifiedDatabaseBackup(ctx context.Context, instance Instance, database, id string) (*os.File, DatabaseBackup, error) {
	backup, err := e.databaseBackupMetadata(ctx, instance, database, id)
	if err != nil {
		return nil, backup, err
	}
	root, err := os.OpenRoot(instance.Paths.BackupDir)
	if err != nil {
		return nil, backup, err
	}
	defer root.Close()
	file, err := root.OpenFile(backup.Filename, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, backup, failure("artifact_invalid", "Owned backup cannot be opened safely", 412)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != backup.Bytes {
		file.Close()
		return nil, backup, failure("artifact_invalid", "Backup file differs from its owned receipt", 412)
	}
	checksum := sha256.New()
	count, err := io.Copy(checksum, &contextReader{ctx: ctx, reader: file})
	if err != nil {
		file.Close()
		return nil, backup, err
	}
	if count != backup.Bytes || hex.EncodeToString(checksum.Sum(nil)) != backup.SHA256 {
		file.Close()
		return nil, backup, failure("checksum_mismatch", "Backup integrity verification failed; no bytes were exported", 412)
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, backup, err
	}
	return file, backup, nil
}
func (e *PrivilegedExecutor) databaseArtifactStream(ctx context.Context, request agent.Request, streams agent.IO) (result agent.Result, err error) {
	result = agent.Result{Operation: request.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	defer func() { result.FinishedAt = time.Now().UTC() }()
	if err = e.validateDatabaseArtifact(request.Input); err != nil {
		return result, err
	}
	if streams.Stdout == nil {
		return result, failure("stream_required", "Database backup export requires only an output stream", 400)
	}
	var in databaseArtifactInput
	json.Unmarshal(request.Input, &in)
	unlock, err := e.lockContext(ctx, in.InstanceID)
	if err != nil {
		return result, err
	}
	defer unlock()
	instance, err := e.get(ctx, in.InstanceID, false)
	if err != nil {
		return result, err
	}
	file, backup, err := e.openVerifiedDatabaseBackup(ctx, instance, in.Database, in.BackupID)
	if err != nil {
		return result, err
	}
	defer file.Close()
	if in.Offset > backup.Bytes {
		return result, failure("invalid_input", "Chunk offset exceeds the owned artifact", 400)
	}
	if _, err = file.Seek(in.Offset, io.SeekStart); err != nil {
		return result, err
	}
	stop := context.AfterFunc(ctx, func() { file.Close(); streams.Stdout.Close() })
	defer stop()
	var reader io.Reader = &contextReader{ctx: ctx, reader: file}
	if in.Length > 0 {
		reader = io.LimitReader(reader, in.Length)
	}
	written, err := io.Copy(streams.Stdout, reader)
	result.StdoutBytes = written
	if err != nil {
		return result, err
	}
	expected := backup.Bytes - in.Offset
	if in.Length > 0 {
		expected = min(expected, in.Length)
	}
	if written != expected {
		return result, failure("artifact_transfer_failed", "Backup export was incomplete", 502)
	}
	result.Data, _ = json.Marshal(map[string]any{"backup_id": backup.ID, "bytes": written, "offset": in.Offset, "sha256": backup.SHA256})
	result.ExitCode = 0
	return result, nil
}
