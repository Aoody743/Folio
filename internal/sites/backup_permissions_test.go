package sites

import (
	"context"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestQARootWebsiteBackupRestorePreservesGroupReadUnderPrivateUmask(t *testing.T) {
	e, runtime := qaRootExecutor(t)
	priorMask := syscall.Umask(0077)
	defer syscall.Umask(priorMask)
	site := rootOwnedTestSite(t)
	created, err := qaAgentRun(context.Background(), e, "site_apply", request{ID: site.ID, Site: &site})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(created.Data, &site); err != nil {
		t.Fatal(err)
	}
	worker, err := e.identity(context.Background(), site.ID)
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroup(runtime.web.WorkerGroup)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]os.FileMode{"static": 0750, "static/readonly": 0550, "static/readonly/style.css": 0640}
	for _, path := range []string{"static", "static/readonly"} {
		if err = os.MkdirAll(filepath.Join(site.DocumentRoot, path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	css := filepath.Join(site.DocumentRoot, "static/readonly/style.css")
	if err = os.WriteFile(css, []byte("body { color: green; }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for path, mode := range paths {
		full := filepath.Join(site.DocumentRoot, path)
		if err = os.Chown(full, int(worker.UID), gid); err != nil {
			t.Fatal(err)
		}
		if err = os.Chmod(full, mode); err != nil {
			t.Fatal(err)
		}
	}
	backed, err := qaAgentRun(context.Background(), e, "site_backup_create", request{ID: site.ID, ExpectedRevision: site.Revision})
	if err != nil {
		t.Fatal(err)
	}
	var backup Backup
	if err = json.Unmarshal(backed.Data, &backup); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(css, []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = qaAgentRun(context.Background(), e, "site_backup_restore", request{ID: site.ID, ExpectedRevision: site.Revision, BackupID: backup.ID}); err != nil {
		t.Fatal(err)
	}
	for path, mode := range paths {
		info, err := os.Stat(filepath.Join(site.DocumentRoot, path))
		if err != nil {
			t.Fatal(err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		if info.Mode().Perm() != mode || stat.Uid != worker.UID || stat.Gid != uint32(gid) {
			t.Fatalf("restored %s mode/ownership differs: %04o uid=%d gid=%d", path, info.Mode().Perm(), stat.Uid, stat.Gid)
		}
	}
	data, err := os.ReadFile(css)
	if err != nil || string(data) != "body { color: green; }\n" {
		t.Fatal("restored bytes differ", err)
	}
	// Actual Web access is a separate native-process gate; this proves that
	// archive permissions and isolated ownership survive root's umask0077.
}
