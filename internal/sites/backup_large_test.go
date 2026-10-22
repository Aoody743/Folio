package sites

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// This opt-in test writes/restores more than two GiB using bounded buffers.
// It runs in the disposable systemd VM, not as a normal fast CI unit test.
func TestQARootLargeWebsiteBackupRoundTrip(t *testing.T) {
	if os.Getenv("FOLIO_LARGE_BACKUP_TEST") != "1" {
		t.Skip("enable the multi-GiB backup test explicitly")
	}
	e, _ := qaRootExecutor(t)
	site := rootOwnedTestSite(t)
	created, err := qaAgentRun(context.Background(), e, "site_apply", request{ID: site.ID, Site: &site})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(created.Data, &site); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(site.DocumentRoot, "large.bin")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(2<<30) + 4096
	if err = file.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteAt([]byte("TAIL"), size-4); err != nil {
		t.Fatal(err)
	}
	file.Close()
	backed, err := qaAgentRun(context.Background(), e, "site_backup_create", request{ID: site.ID, ExpectedRevision: site.Revision})
	if err != nil {
		t.Fatal(err)
	}
	var backup Backup
	if err = json.Unmarshal(backed.Data, &backup); err != nil {
		t.Fatal(err)
	}
	if err = os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = qaAgentRun(context.Background(), e, "site_backup_restore", request{ID: site.ID, ExpectedRevision: site.Revision, BackupID: backup.ID}); err != nil {
		t.Fatal(err)
	}
	file, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() != size {
		t.Fatal("large restore lost bytes", err)
	}
	tail := make([]byte, 4)
	if _, err = file.ReadAt(tail, size-4); err != nil || string(tail) != "TAIL" {
		t.Fatal("large restore lost the tail", err)
	}
	if _, _, err = parseBackupRange("bytes=4294967296-", int64(5<<30)); err != nil {
		t.Fatal("large HTTP range rejected", err)
	}
}
