package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestDatabaseBackupWriteAndMetadataBeyondFourGiB(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "large-backup")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	offset := int64(4<<30) + 17
	if _, err = file.Seek(offset, 0); err != nil {
		t.Fatal(err)
	}
	sink := &databaseOutput{ctx: context.Background(), file: file, hash: sha256.New(), bytes: offset}
	if n, err := sink.Write([]byte("tail")); err != nil || n != 4 || sink.bytes != offset+4 {
		t.Fatal("backup writer retained the old total size cap", n, err)
	}
	e := testAgent(t)
	instance := Instance{ID: "postgresql-large", PluginID: "postgresql", InstanceID: "large", Paths: e.instancePaths("postgresql", "large")}
	if err := os.MkdirAll(instance.Paths.BackupDir, 0700); err != nil {
		t.Fatal(err)
	}
	backup := DatabaseBackup{ID: "dbbackup_large", InstanceID: instance.ID, Database: "large_db", Engine: instance.PluginID, Filename: "dbbackup_large.dump", Bytes: offset + 4, SHA256: hex.EncodeToString(sha256.New().Sum(nil))}
	target := filepath.Join(instance.Paths.BackupDir, backup.Filename)
	data, err := os.Create(target)
	if err != nil {
		t.Fatal(err)
	}
	err = data.Truncate(backup.Bytes)
	data.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = e.save("private", backup.ID, backup); err != nil {
		t.Fatal(err)
	}
	if _, err = e.databaseBackupMetadata(context.Background(), instance, backup.Database, backup.ID); err != nil {
		t.Fatal("large backup receipt rejected", err)
	}
	if err = e.validateDatabaseArtifact(mustJSON(databaseArtifactInput{InstanceID: instance.ID, Database: backup.Database, BackupID: backup.ID, Offset: offset, Length: 4})); err != nil {
		t.Fatal("large backup offset rejected", err)
	}
}
