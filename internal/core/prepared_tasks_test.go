package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/jobs"
	"github.com/ncruces/go-sqlite3"
)

func preparedBinding(t *testing.T, s *Service, action string, fn func(context.Context, MetadataStore, jobs.Job) error) Binding {
	t.Helper()
	deps := s.Runtime("prepared_fixture")
	b := fixtureBinding(action, "fixture:write", "files:workspace", func(ctx context.Context, a Actor, _ json.RawMessage) (ActionResult, error) {
		input := json.RawMessage(`{"root":"workspace","path":"prepared.txt","content":"real prepared task bytes"}`)
		view, err := deps.Tasks.SubmitPrepared(ctx, a, "file_write", input, TaskMetadata{Resources: []string{"files:workspace"}}, func(tx MetadataStore, j jobs.Job) error { return fn(ctx, tx, j) })
		return ActionResult{Data: view, Resources: []string{"files:workspace", "certificate:extra"}}, err
	})
	b.Action.Task = true
	b.Resources = func(context.Context, Actor, json.RawMessage) ([]string, error) {
		return []string{"files:workspace", "certificate:extra"}, nil
	}
	return b
}
func preparedRowCounts(t *testing.T, s *Service) (int, int, int) {
	t.Helper()
	counts := [3]int{}
	for i, table := range []string{"tasks", "task_metadata", "provider_state"} {
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&counts[i]); err != nil {
			t.Fatal(err)
		}
	}
	return counts[0], counts[1], counts[2]
}
func waitPrepared(t *testing.T, s *Service, id string) jobs.Job {
	t.Helper()
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		j, err := s.jobs.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status == jobs.Succeeded || j.Status == jobs.Failed || j.Status == jobs.Canceled || j.Status == jobs.Interrupted {
			return j
		}
		select {
		case <-deadline:
			t.Fatal("prepared task did not finish")
		case <-tick.C:
		}
	}
}

func TestPreparedTaskCommitStoresProviderStateJobAndFullAuthorizationBeforeRealWrite(t *testing.T) {
	s, a := testService(t)
	var id string
	b := preparedBinding(t, s, "fixture_prepare", func(ctx context.Context, tx MetadataStore, j jobs.Job) error {
		id = j.ID
		if _, err := os.Stat(filepath.Join(s.dataDir, "workspace", "prepared.txt")); !os.IsNotExist(err) {
			t.Fatal("executor ran before the metadata transaction committed")
		}
		_, err := tx.CompareAndSwap(ctx, "settings", "current", 0, json.RawMessage(`{"job_id":"`+j.ID+`"}`))
		return err
	})
	if err := s.RegisterProvider(Bindings{b}); err != nil {
		t.Fatal(err)
	}
	result, err := s.ExecuteWithKey(context.Background(), a, b.Action.Name, nil, "prepared-idempotency-fixture")
	if err != nil {
		t.Fatal(err)
	}
	view := result.(map[string]any)
	if view["id"] != id || view["input"] != nil {
		t.Fatal("prepared task did not return its safe durable identity")
	}
	replayed, err := s.ExecuteWithKey(context.Background(), a, b.Action.Name, nil, "prepared-idempotency-fixture")
	if err != nil || replayed.(map[string]any)["id"] != id {
		t.Fatal("same prepared idempotency key did not preserve its original durable task")
	}
	if tasks, metadata, state := preparedRowCounts(t, s); tasks != 1 || metadata != 1 || state != 1 {
		t.Fatal("prepared idempotency retry created a duplicate task or marker")
	}
	meta, ok := s.loadTaskMetadata(context.Background(), id)
	if !ok || meta.Action != b.Action.Name || meta.Scope != "fixture:write" || meta.Actor.ID != a.ID || len(meta.Resources) != 2 {
		t.Fatal("prepared job lost part of its originating authorization")
	}
	if j := waitPrepared(t, s, id); j.Status != jobs.Succeeded {
		t.Fatal("actual file executor failed", j.Error)
	}
	data, err := os.ReadFile(filepath.Join(s.dataDir, "workspace", "prepared.txt"))
	if err != nil || string(data) != "real prepared task bytes" {
		t.Fatal("committed prepared task did not write its real scoped file")
	}
	raw, err := s.Runtime("prepared_fixture").State.Get(context.Background(), "settings", "current")
	if err != nil || string(raw) != `{"job_id":"`+id+`"}` {
		t.Fatal("committed task lost its matching provider receipt")
	}
	if _, err := s.Runtime("other_namespace").State.Get(context.Background(), "settings", "current"); ErrorCode(err) != "not_found" {
		t.Fatal("prepared state escaped its runtime namespace")
	}
}

func TestPreparedRequestCanceledByActualWALCommitStillStartsCommittedTask(t *testing.T) {
	s, a := testService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var armed atomic.Bool
	connection, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = connection.Raw(func(raw any) error {
		raw.(interface{ Raw() *sqlite3.Conn }).Raw().WALHook(func(*sqlite3.Conn, string, int) error {
			// SQLite invokes the WAL hook after the actual transaction commits.
			// This cancels the caller at that boundary without a synthetic delay.
			if armed.CompareAndSwap(true, false) {
				cancel()
			}
			return nil
		})
		return nil
	})
	connection.Close()
	if err != nil {
		t.Fatal(err)
	}
	b := preparedBinding(t, s, "fixture_prepare_disconnect", func(ctx context.Context, tx MetadataStore, j jobs.Job) error {
		_, err := tx.CompareAndSwap(ctx, "settings", "current", 0, json.RawMessage(`{"job_id":"`+j.ID+`"}`))
		if err == nil {
			armed.Store(true)
		}
		return err
	})
	if err := s.RegisterProvider(Bindings{b}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Execute(ctx, a, b.Action.Name, nil)
	if err != nil || ctx.Err() == nil {
		t.Fatal("actual WAL-committed admission was lost when its caller disconnected", err)
	}
	id := result.(map[string]any)["id"].(string)
	if j := waitPrepared(t, s, id); j.Status != jobs.Succeeded {
		t.Fatal("caller cancellation controlled a committed task's service lifetime", j.Error)
	}
	if tasks, metadata, state := preparedRowCounts(t, s); tasks != 1 || metadata != 1 || state != 1 {
		t.Fatal("post-commit cancellation left inconsistent task metadata")
	}
}

func TestPreparedTaskCannotForgeResourcesOrBypassItsOriginatingAction(t *testing.T) {
	s, a := testService(t)
	deps := s.Runtime("prepared_fixture")
	called := false
	prepare := func(MetadataStore, jobs.Job) error { called = true; return nil }
	input := json.RawMessage(`{"root":"workspace","path":"prepared.txt","content":"must not execute"}`)
	if _, err := deps.Tasks.SubmitPrepared(context.Background(), a, "file_write", input, TaskMetadata{Resources: []string{"files:workspace"}}, prepare); ErrorCode(err) != "forbidden" {
		t.Fatal("prepared task bypassed its originating action")
	}
	b := fixtureBinding("fixture_prepare_forged", "fixture:write", "files:workspace", func(ctx context.Context, a Actor, _ json.RawMessage) (ActionResult, error) {
		_, err := deps.Tasks.SubmitPrepared(ctx, a, "file_write", input, TaskMetadata{Resources: []string{"files:other"}}, prepare)
		return ActionResult{}, err
	})
	if err := s.RegisterProvider(Bindings{b}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(context.Background(), a, b.Action.Name, nil); ErrorCode(err) != "forbidden" {
		t.Fatal("prepared metadata claimed a resource absent from its authorized action")
	}
	if called {
		t.Fatal("unauthorized prepared metadata callback executed")
	}
	if tasks, metadata, state := preparedRowCounts(t, s); tasks != 0 || metadata != 0 || state != 0 {
		t.Fatal("unauthorized task left durable rows")
	}
}

func TestPreparedTaskRealSQLiteFaultsRollbackAllRowsAndExecuteNothing(t *testing.T) {
	for _, fault := range []string{"callback", "task_metadata_insert", "provider_insert", "deferred_commit", "context_canceled"} {
		t.Run(fault, func(t *testing.T) {
			s, a := testService(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var statement string
			switch fault {
			case "task_metadata_insert":
				statement = `CREATE TRIGGER reject_prepared_meta BEFORE INSERT ON task_metadata BEGIN SELECT RAISE(FAIL,'prepared metadata fault'); END`
			case "provider_insert":
				statement = `CREATE TRIGGER reject_prepared_state BEFORE INSERT ON provider_state BEGIN SELECT RAISE(FAIL,'prepared provider fault'); END`
			case "deferred_commit":
				statement = `CREATE TABLE prepared_parent(id TEXT PRIMARY KEY); CREATE TABLE prepared_child(id TEXT REFERENCES prepared_parent(id) DEFERRABLE INITIALLY DEFERRED); CREATE TRIGGER reject_prepared_commit AFTER INSERT ON tasks BEGIN INSERT INTO prepared_child(id) VALUES(NEW.id); END`
			}
			if statement != "" {
				if _, err := s.db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			b := preparedBinding(t, s, "fixture_prepare_fault", func(ctx context.Context, tx MetadataStore, j jobs.Job) error {
				if _, err := tx.CompareAndSwap(ctx, "settings", "current", 0, json.RawMessage(`{"job_id":"`+j.ID+`"}`)); err != nil {
					return err
				}
				if fault == "callback" {
					return errors.New("injected metadata callback failure")
				}
				if fault == "context_canceled" {
					cancel()
				}
				return nil
			})
			if err := s.RegisterProvider(Bindings{b}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Execute(ctx, a, b.Action.Name, nil); err == nil {
				t.Fatal("injected SQL failure was reported as successful admission")
			}
			if tasks, metadata, state := preparedRowCounts(t, s); tasks != 0 || metadata != 0 || state != 0 {
				t.Fatal("rollback left a partial job or provider marker", tasks, metadata, state)
			}
			if _, err := os.Stat(filepath.Join(s.dataDir, "workspace", "prepared.txt")); !os.IsNotExist(err) {
				t.Fatal("failed transaction executed an external file mutation")
			}
		})
	}
}

func TestPreparedConcurrentMetadataCASAllowsExactlyOneDurableRealTask(t *testing.T) {
	s, a := testService(t)
	b := preparedBinding(t, s, "fixture_prepare_cas", func(ctx context.Context, tx MetadataStore, j jobs.Job) error {
		_, err := tx.CompareAndSwap(ctx, "settings", "current", 0, json.RawMessage(`{"job_id":"`+j.ID+`"}`))
		return err
	})
	if err := s.RegisterProvider(Bindings{b}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := s.Execute(context.Background(), a, b.Action.Name, nil)
			if err == nil {
				results <- data.(map[string]any)["id"].(string)
			} else if ErrorCode(err) != "revision_conflict" {
				t.Error("concurrent admission failed for an unexpected reason", ErrorCode(err))
			}
		}()
	}
	wg.Wait()
	close(results)
	var id string
	winners := 0
	for result := range results {
		id = result
		winners++
	}
	if winners != 1 {
		t.Fatal("metadata CAS admitted duplicate durable tasks", winners)
	}
	if tasks, metadata, state := preparedRowCounts(t, s); tasks != 1 || metadata != 1 || state != 1 {
		t.Fatal("concurrent submission left a partial or duplicate row", tasks, metadata, state)
	}
	if j := waitPrepared(t, s, id); j.Status != jobs.Succeeded {
		t.Fatal("single committed real task failed", j.Error)
	}
}

func TestPreparedCommittedQueuedJobSurvivesManagerCloseAndRestartsInterrupted(t *testing.T) {
	s, a := testService(t)
	b := preparedBinding(t, s, "fixture_prepare_close", func(ctx context.Context, tx MetadataStore, j jobs.Job) error {
		if _, err := tx.CompareAndSwap(ctx, "settings", "current", 0, json.RawMessage(`{"job_id":"`+j.ID+`"}`)); err != nil {
			return err
		}
		return s.jobs.Close()
	})
	if err := s.RegisterProvider(Bindings{b}); err != nil {
		t.Fatal(err)
	}
	value, err := s.Execute(context.Background(), a, b.Action.Name, nil)
	if err != nil {
		t.Fatal("committed task was misreported as rolled back", err)
	}
	view := value.(map[string]any)
	id := view["id"].(string)
	if view["pending"] != true || view["state"] != jobs.Queued {
		t.Fatal("committed task did not retain its pending durable identity")
	}
	if tasks, metadata, state := preparedRowCounts(t, s); tasks != 1 || metadata != 1 || state != 1 {
		t.Fatal("manager close lost part of the committed transaction")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(s.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	j, err := restarted.jobs.Get(context.Background(), id)
	if err != nil || j.Status != jobs.Interrupted {
		t.Fatal("restart lost or automatically replayed the pending mutation")
	}
	if _, err := restarted.Runtime("prepared_fixture").State.Get(context.Background(), "settings", "current"); err != nil {
		t.Fatal("restart lost matching committed provider metadata")
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, "workspace", "prepared.txt")); !os.IsNotExist(err) {
		t.Fatal("closed manager or restart replayed an unstarted mutation")
	}
}
