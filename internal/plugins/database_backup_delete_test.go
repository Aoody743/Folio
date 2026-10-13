package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestOwnedDatabaseBackupDeletionChecksResourceIntegrityAndTombstone(t *testing.T) {
	e := testAgent(t)
	ctx := context.Background()
	instance := Instance{ID: "postgresql-delete", PluginID: "postgresql", InstanceID: "delete", Paths: e.instancePaths("postgresql", "delete")}
	if err := os.MkdirAll(instance.Paths.BackupDir, 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("dedicated owned backup bytes\x00\xff")
	hash := sha256.Sum256(data)
	backup := DatabaseBackup{ID: "dbbackup_delete_fixture", InstanceID: instance.ID, Database: "owned", Engine: instance.PluginID, Filename: "dbbackup_delete_fixture.dump", Bytes: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}
	filename := filepath.Join(instance.Paths.BackupDir, backup.Filename)
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.save("private", backup.ID, backup); err != nil {
		t.Fatal(err)
	}
	record := managedDatabaseRecord{ManagedDatabase: ManagedDatabase{Database: "other"}}
	if _, err := e.deleteDatabaseBackup(ctx, instance, record, backup.ID); core.ErrorStatus(err) != 403 {
		t.Fatal("delete accepted a backup from another database")
	}
	if body, err := os.ReadFile(filename); err != nil || string(body) != string(data) {
		t.Fatal("cross-database delete changed the owned bytes")
	}
	record.Database = backup.Database
	data[0] ^= 1
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.deleteDatabaseBackup(ctx, instance, record, backup.ID); core.ErrorCode(err) != "checksum_mismatch" {
		t.Fatal("delete did not verify the authoritative artifact checksum")
	}
	if _, err := os.Stat(filename); err != nil {
		t.Fatal("corrupt artifact was removed before reconciliation")
	}
	data[0] ^= 1
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.deleteDatabaseBackup(ctx, instance, record, backup.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filename); !os.IsNotExist(err) {
		t.Fatal("successful deletion retained dump bytes")
	}
	if _, err := e.deleteDatabaseBackup(ctx, instance, record, backup.ID); err != nil {
		t.Fatal("repeat owned deletion was not idempotent")
	}
	if _, err := e.databaseBackupMetadata(ctx, instance, backup.Database, backup.ID); core.ErrorStatus(err) != 404 {
		t.Fatal("deleted receipt still allowed export")
	}
	listed, err := e.listDatabaseBackups(ctx, instance, record)
	if err != nil || listed.(map[string]any)["total"] != 0 {
		t.Fatal("deleted backup remained in listing")
	}
	var receipt DatabaseBackup
	if err = e.load("private", backup.ID, &receipt); err != nil || receipt.State != "deleted" {
		t.Fatal("deletion tombstone was not persisted")
	}
	if err = e.save("private", backup.ID, DatabaseBackup{ID: backup.ID, InstanceID: backup.InstanceID, Database: backup.Database, Engine: backup.Engine, Filename: backup.Filename, Bytes: backup.Bytes, SHA256: backup.SHA256, State: "deleting"}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.deleteDatabaseBackup(ctx, instance, record, backup.ID); core.ErrorStatus(err) != 409 {
		t.Fatal("interrupted deletion was silently replayed")
	}
}
