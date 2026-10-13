package sites

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

type qaRecoveryMarker struct{}
type qaDeleteRuntime struct {
	*qaRuntimeFixture
	fault     func()
	active    bool
	recovered bool
}

func (r *qaDeleteRuntime) WithRuntimeTransaction(ctx context.Context, _ []string, callback func(context.Context) error) error {
	r.active = true
	defer func() { r.active = false }()
	return callback(context.WithValue(ctx, qaRecoveryMarker{}, r))
}
func (r *qaDeleteRuntime) Reload(ctx context.Context, id string) error {
	if !r.active || ctx.Value(qaRecoveryMarker{}) != r || ctx.Err() != nil {
		return errors.New("runtime recovery lost its active transaction identity or remained cancelled")
	}
	err := r.qaRuntimeFixture.Reload(ctx, id)
	if r.fault != nil {
		fault := r.fault
		r.fault = nil
		fault()
	} else {
		r.recovered = true
	}
	return err
}

func TestQARootDeleteLateMetadataFailureRestoresConfigurationInsideCancelledTransaction(t *testing.T) {
	e, fixture := qaRootExecutor(t)
	site := rootOwnedTestSite(t)
	result, err := qaAgentRun(context.Background(), e, "site_apply", request{ID: site.ID, Site: &site})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(result.Data, &site); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.web.SnippetsDir, site.ID+".conf")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(e.config.StateDir, "sites", site.ID+".json")
	receipt, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := &qaDeleteRuntime{qaRuntimeFixture: fixture}
	runtime.fault = func() {
		if err := e.state.Close(); err != nil {
			t.Fatal(err)
		}
		cancel()
	}
	e.runtimes = runtime
	if _, err := qaAgentRun(ctx, e, "site_delete", request{ID: site.ID, ExpectedRevision: site.Revision, KeepData: true}); err == nil {
		t.Fatal("late metadata fault became a successful deletion")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("failed metadata commit removed the active vhost", err)
	}
	afterReceipt, err := os.ReadFile(receiptPath)
	if err != nil || string(afterReceipt) != string(receipt) {
		t.Fatal("failed metadata commit changed the active site receipt", err)
	}
	if !runtime.recovered || fixture.reloads != 3 {
		t.Fatal("rollback did not reload inside its active transaction using an independent context", fixture.reloads)
	}
	if _, err := os.Stat(filepath.Join(e.config.StateDir, "retained", site.ID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed deletion left an inconsistent retained receipt", err)
	}
}

func TestQARootInterruptedRetainedPurgeCanRestartAndFinishOwnershipCleanup(t *testing.T) {
	e, runtime := qaRootExecutor(t)
	site := rootOwnedTestSite(t)
	result, err := qaAgentRun(context.Background(), e, "site_apply", request{ID: site.ID, Site: &site})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(result.Data, &site); err != nil {
		t.Fatal(err)
	}
	if _, err = qaAgentRun(context.Background(), e, "site_delete", request{ID: site.ID, ExpectedRevision: site.Revision, KeepData: true}); err != nil {
		t.Fatal(err)
	}
	retained, err := e.retained(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	retained.Purging = true
	if err = atomicRoot(e.state, filepath.Join("retained", site.ID+".json"), encode(retained), 0600); err != nil {
		t.Fatal(err)
	}
	// The owned public bytes have been removed, but a crash before the final
	// receipt deletion left private logs and the explicit purge intent.
	if err = e.content.RemoveAll(filepath.Join(site.ID, "public")); err != nil {
		t.Fatal(err)
	}
	config := e.config
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewAgentExecutor(config, runtime)
	if err != nil {
		t.Fatal("interrupted purge prevented Agent startup", err)
	}
	t.Cleanup(func() { restarted.Close() })
	if _, err = qaAgentRun(context.Background(), restarted, "site_retained_purge", request{ID: site.ID, ExpectedRevision: site.Revision + 1}); core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("restart purge ignored its ownership CAS", err)
	}
	if _, err = qaAgentRun(context.Background(), restarted, "site_retained_purge", request{ID: site.ID, ExpectedRevision: site.Revision}); err != nil {
		t.Fatal("interrupted purge could not finish without a removed public root", err)
	}
	if _, err = restarted.retained(site.ID); !isNotFound(err) {
		t.Fatal("finished purge left ownership metadata", err)
	}
	if _, err = restarted.content.Lstat(site.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("finished purge left owned data", err)
	}
}
