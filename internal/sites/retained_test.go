package sites

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/files"
)

func rootOwnedTestSite(t *testing.T) Site {
	t.Helper()
	var entropy [10]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		t.Fatal(err)
	}
	site := qaRenderSite()
	site.ID = "qa-" + hex.EncodeToString(entropy[:])
	workerName := "plsite_" + siteTag(site.ID)
	if _, err := user.Lookup(workerName); err == nil {
		t.Fatal("refuse to claim an existing worker")
	}
	t.Cleanup(func() {
		if _, err := user.Lookup(workerName); err == nil {
			if output, err := exec.Command("/usr/sbin/userdel", workerName).CombinedOutput(); err != nil {
				t.Errorf("owned worker cleanup: %v (%d diagnostic bytes)", err, len(output))
			}
		}
	})
	return site
}

func TestQARootRetainedWebsiteContentSurvivesRestartAndRejectsImplicitAdoption(t *testing.T) {
	e, runtime := qaRootExecutor(t)
	site := rootOwnedTestSite(t)
	result, err := qaAgentRun(context.Background(), e, "site_apply", request{ID: site.ID, Site: &site})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(result.Data, &site); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(site.DocumentRoot, "kept.txt"), []byte("retained bytes\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if _, err = qaAgentRun(context.Background(), e, "site_delete", request{ID: site.ID, ExpectedRevision: site.Revision, KeepData: true}); err != nil {
		t.Fatal(err)
	}
	config := e.config
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewAgentExecutor(config, runtime)
	if err != nil {
		t.Fatal("retained root lost on restart", err)
	}
	defer restarted.Close()
	text, err := restarted.files.ReadText(context.Background(), files.Input{Root: "site-" + site.ID, Path: "kept.txt"})
	if err != nil || text.Content != "retained bytes\n" {
		t.Fatal("retained root bytes inaccessible", err)
	}
	if _, err = qaAgentRun(context.Background(), restarted, "site_apply", request{ID: site.ID, Site: &site}); core.ErrorCode(err) != "retained_content_exists" {
		t.Fatal("new website implicitly adopted retained content", err)
	}
	if _, err = qaAgentRun(context.Background(), restarted, "site_retained_purge", request{ID: site.ID, ExpectedRevision: site.Revision + 1}); core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("stale retained purge accepted", err)
	}
	if _, err = qaAgentRun(context.Background(), restarted, "site_retained_purge", request{ID: site.ID, ExpectedRevision: site.Revision}); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(site.DocumentRoot); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("purged website bytes still exist", err)
	}
	if values, err := restarted.retainedSites(); err != nil || len(values) != 0 {
		t.Fatal("purged ownership receipt remains", err)
	}
	unknown := "qa-unowned"
	if err = restarted.content.MkdirAll(filepath.Join(unknown, "public"), 0755); err != nil {
		t.Fatal(err)
	}
	other := qaRenderSite()
	other.ID = unknown
	if _, err = qaAgentRun(context.Background(), restarted, "site_apply", request{ID: unknown, Site: &other}); core.ErrorCode(err) != "unowned_content_exists" {
		t.Fatal("new website adopted an existing unowned directory", err)
	}
}

func TestQARootFailedWebsiteCreationCleansOnlyItsNewContentAndCanRetry(t *testing.T) {
	e, runtime := qaRootExecutor(t)
	site := rootOwnedTestSite(t)
	runtime.failCheck = true
	if _, err := qaAgentRun(context.Background(), e, "site_apply", request{ID: site.ID, Site: &site}); err == nil {
		t.Fatal("failed check accepted")
	}
	if _, err := e.content.Lstat(site.ID); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("failed creation left content that cannot be safely retried", err)
	}
	if _, err := qaAgentRun(context.Background(), e, "site_apply", request{ID: site.ID, Site: &site}); err != nil {
		t.Fatal("failed creation could not retry", err)
	}
}
