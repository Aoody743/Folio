package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/jobs"
	sqlite3 "github.com/ncruces/go-sqlite3"
)

const preparedEnqueueOperation = "fixture_exact_once"
const preparedEnqueueResource = "fixture:prepared"

type preparedEnqueueExecutor struct {
	validationCalls    atomic.Int32
	runs               atomic.Int32
	rejectValidationAt int32
	markerPath         string
}

func (e *preparedEnqueueExecutor) Validate(request agent.Request) error {
	call := e.validationCalls.Add(1)
	if request.Operation != preparedEnqueueOperation || !json.Valid(request.Input) {
		return agent.ErrInvalidInput
	}
	if e.rejectValidationAt > 0 && call >= e.rejectValidationAt {
		return agent.ErrInvalidInput
	}
	return nil
}

func (e *preparedEnqueueExecutor) Run(ctx context.Context, request agent.Request) (agent.Result, error) {
	started := time.Now().UTC()
	call := e.runs.Add(1)
	result := agent.Result{Operation: request.Operation, StartedAt: started, FinishedAt: time.Now().UTC(), ExitCode: 0}
	if err := ctx.Err(); err != nil {
		result.Canceled, result.ExitCode = true, -1
		return result, err
	}
	if call != 1 {
		return result, errors.New("prepared executor ran more than once")
	}
	if e.markerPath != "" {
		if err := os.WriteFile(e.markerPath, []byte(request.JobID), 0600); err != nil {
			return result, err
		}
	}
	return result, nil
}

func registerPreparedEnqueueBinding(t *testing.T, s *Service, executor *preparedEnqueueExecutor) Binding {
	t.Helper()
	if err := s.executors.register([]string{preparedEnqueueOperation}, executor); err != nil {
		t.Fatal(err)
	}
	deps := s.Runtime("prepared_enqueue")
	b := fixtureBinding("fixture_prepared_enqueue", "fixture:write", preparedEnqueueResource, func(ctx context.Context, actor Actor, _ json.RawMessage) (ActionResult, error) {
		input := json.RawMessage(`{"task":"exactly once"}`)
		view, err := deps.Tasks.SubmitPrepared(ctx, actor, preparedEnqueueOperation, input, TaskMetadata{Resources: []string{preparedEnqueueResource}}, func(tx MetadataStore, job jobs.Job) error {
			_, err := tx.CompareAndSwap(ctx, "receipt", "current", 0, json.RawMessage(`{"task_id":"`+job.ID+`"}`))
			return err
		})
		return ActionResult{Data: view, Resources: []string{preparedEnqueueResource}}, err
	})
	b.Action.Task = true
	b.Resources = func(context.Context, Actor, json.RawMessage) ([]string, error) {
		return []string{preparedEnqueueResource}, nil
	}
	if err := s.RegisterProvider(Bindings{b}); err != nil {
		t.Fatal(err)
	}
	return b
}

// lockedFirstGetStore creates a real rollback-journal EXCLUSIVE lock during
// its first durable task read. It releases the lock before returning SQLite's
// BUSY code, then delegates all subsequent reads to the service's store.
type lockedFirstGetStore struct {
	jobs.Store
	reader     *sql.DB
	locker     *sql.Conn
	getCalls   atomic.Int32
	lockErrors atomic.Int32
	firstError string
}

func (s *lockedFirstGetStore) Get(ctx context.Context, id string) (jobs.Job, error) {
	if s.getCalls.Add(1) != 1 {
		return s.Store.Get(ctx, id)
	}
	var mode string
	if err := s.locker.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		s.firstError = "begin: " + err.Error()
		return jobs.Job{}, err
	}
	if mode != "delete" {
		return jobs.Job{}, errors.New("test database did not enter rollback-journal mode")
	}
	if _, err := s.locker.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		s.firstError = "exclusive: " + err.Error()
		_ = s.locker.QueryRowContext(context.Background(), "PRAGMA journal_mode=WAL").Scan(&mode)
		return jobs.Job{}, err
	}
	var raw string
	readErr := s.reader.QueryRowContext(ctx, "SELECT payload FROM tasks WHERE id=?", id).Scan(&raw)
	_, rollbackErr := s.locker.ExecContext(context.Background(), "ROLLBACK")
	walErr := s.locker.QueryRowContext(context.Background(), "PRAGMA journal_mode=WAL").Scan(&mode)
	if errors.Is(readErr, sqlite3.BUSY) {
		if rollbackErr != nil || walErr != nil {
			return jobs.Job{}, errors.Join(rollbackErr, walErr)
		}
		s.lockErrors.Add(1)
		return jobs.Job{}, readErr
	}
	if readErr != nil {
		s.firstError = readErr.Error()
		return jobs.Job{}, readErr
	}
	if rollbackErr != nil || walErr != nil {
		s.firstError = errors.Join(rollbackErr, walErr).Error()
		return jobs.Job{}, errors.Join(rollbackErr, walErr)
	}
	return s.Store.Get(ctx, id)
}

func openPreparedSharedSQLiteDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	fileURL := url.URL{Scheme: "file", Path: path}
	query := fileURL.Query()
	query.Add("_pragma", "busy_timeout(0)")
	fileURL.RawQuery = query.Encode()
	db, err := sql.Open("sqlite3", fileURL.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func openPreparedSharedSQLiteConn(t *testing.T, path string) (*sql.DB, *sql.Conn) {
	t.Helper()
	db := openPreparedSharedSQLiteDB(t, path)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return db, conn
}

func TestPreparedEnqueueRetriesRealSQLiteStoreLockAndExecutesExactlyOnce(t *testing.T) {
	s, actor := testService(t)
	marker := filepath.Join(t.TempDir(), "executed")
	executor := &preparedEnqueueExecutor{markerPath: marker}
	b := registerPreparedEnqueueBinding(t, s, executor)
	if err := s.jobs.Close(); err != nil {
		t.Fatal(err)
	}
	reader := openPreparedSharedSQLiteDB(t, filepath.Join(s.dataDir, "powerlinux.db"))
	_, locker := openPreparedSharedSQLiteConn(t, filepath.Join(s.dataDir, "powerlinux.db"))
	s.db.SetMaxIdleConns(0)
	lockedStore := &lockedFirstGetStore{Store: sqliteJobStore{s: s}, reader: reader, locker: locker}
	manager, err := jobs.NewManager(s.lifetime, s.executors, lockedStore)
	if err != nil {
		t.Fatal(err)
	}
	s.jobs = manager

	value, err := s.ExecuteWithKey(context.Background(), actor, b.Action.Name, nil, "prepared-real-sqlite-lock")
	if err != nil {
		t.Fatal(err)
	}
	view := value.(map[string]any)
	id, ok := view["id"].(string)
	if !ok || id == "" || view["pending"] == true {
		t.Fatal("transient SQLite lock did not resolve to the durable queued task", view, "observed locks", lockedStore.lockErrors.Load(), "store reads", lockedStore.getCalls.Load(), "first error", lockedStore.firstError, "validations", executor.validationCalls.Load())
	}
	if lockedStore.lockErrors.Load() != 1 || lockedStore.getCalls.Load() < 2 {
		t.Fatal("test did not trigger and recover from a real SQLite task-row lock", lockedStore.lockErrors.Load(), lockedStore.getCalls.Load(), lockedStore.firstError)
	}
	if finished := waitPrepared(t, s, id); finished.Status != jobs.Succeeded {
		t.Fatal("retried prepared task failed", finished.Error)
	}
	if executor.runs.Load() != 1 {
		t.Fatal("prepared task was replayed after retry", executor.runs.Load())
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != id {
		t.Fatal("the real executor did not write its single durable task marker", err)
	}
	if tasks, metadata, state := preparedRowCounts(t, s); tasks != 1 || metadata != 1 || state != 1 {
		t.Fatal("retry created duplicate or partial durable rows", tasks, metadata, state)
	}
}

func TestPreparedEnqueueServiceCloseCancelsBoundedRetry(t *testing.T) {
	s, _ := testService(t)
	ctx, cancel := context.WithTimeout(s.lifetime, preparedEnqueueBudget)
	defer cancel()
	job := jobs.Job{ID: "known-committed-id", Status: jobs.Queued}
	started := make(chan struct{}, 1)
	done := make(chan struct {
		job jobs.Job
		err error
	}, 1)
	var calls atomic.Int32
	go func() {
		got, err := retryPreparedEnqueue(ctx, func(ctx context.Context) (jobs.Job, error) {
			calls.Add(1)
			started <- struct{}{}
			<-ctx.Done()
			return job, jobs.ErrBusy
		})
		done <- struct {
			job jobs.Job
			err error
		}{got, err}
	}()
	<-started
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if !errors.Is(result.err, context.Canceled) || result.job.ID != job.ID {
			t.Fatal("service close did not cancel while preserving the known task identity", result.job, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("prepared enqueue retry outlived Service.Close")
	}
	if calls.Load() != 1 {
		t.Fatal("service close allowed another enqueue attempt", calls.Load())
	}
}

func TestPreparedEnqueueDoesNotRetryPermanentOrUnknownErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "manager_closed", err: jobs.ErrClosed},
		{name: "identity_mismatch", err: jobs.ErrReservation},
		{name: "executor_validation", err: agent.ErrInvalidInput},
		{name: "unknown_io", err: sqlite3.IOERR},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), preparedEnqueueBudget)
			defer cancel()
			var calls atomic.Int32
			job := jobs.Job{ID: "preserved-task-id", Status: jobs.Queued}
			view, err := retryPreparedEnqueue(ctx, func(context.Context) (jobs.Job, error) {
				calls.Add(1)
				return job, test.err
			})
			if !errors.Is(err, test.err) || calls.Load() != 1 || view.ID != job.ID {
				t.Fatal("non-transient failure was retried or lost its task identity", calls.Load(), view, err)
			}
		})
	}
}

func TestPreparedPostCommitWALHookErrorReturnsDurableIDWithoutReplay(t *testing.T) {
	s, actor := testService(t)
	marker := filepath.Join(t.TempDir(), "executed")
	executor := &preparedEnqueueExecutor{markerPath: marker}
	if err := s.executors.register([]string{preparedEnqueueOperation}, executor); err != nil {
		t.Fatal(err)
	}
	var armed atomic.Bool
	var hookErrors atomic.Int32
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = conn.Raw(func(raw any) error {
		raw.(interface{ Raw() *sqlite3.Conn }).Raw().WALHook(func(*sqlite3.Conn, string, int) error {
			if armed.CompareAndSwap(true, false) {
				hookErrors.Add(1)
				return errors.New("injected post-commit WAL hook error")
			}
			return nil
		})
		return nil
	})
	if closeErr := conn.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	deps := s.Runtime("prepared_enqueue")
	b := fixtureBinding("fixture_prepared_enqueue_postcommit_hook", "fixture:write", preparedEnqueueResource, func(ctx context.Context, actor Actor, _ json.RawMessage) (ActionResult, error) {
		input := json.RawMessage(`{"task":"post-commit hook recovery"}`)
		view, submitErr := deps.Tasks.SubmitPrepared(ctx, actor, preparedEnqueueOperation, input, TaskMetadata{Resources: []string{preparedEnqueueResource}}, func(tx MetadataStore, job jobs.Job) error {
			_, prepareErr := tx.CompareAndSwap(ctx, "receipt", "postcommit-hook", 0, json.RawMessage(`{"task_id":"`+job.ID+`"}`))
			if prepareErr == nil {
				armed.Store(true)
			}
			return prepareErr
		})
		return ActionResult{Data: view, Resources: []string{preparedEnqueueResource}}, submitErr
	})
	b.Action.Task = true
	b.Resources = func(context.Context, Actor, json.RawMessage) ([]string, error) {
		return []string{preparedEnqueueResource}, nil
	}
	if err := s.RegisterProvider(Bindings{b}); err != nil {
		t.Fatal(err)
	}

	key := "prepared-postcommit-wal-hook-error"
	value, err := s.ExecuteWithKey(context.Background(), actor, b.Action.Name, nil, key)
	if err != nil {
		t.Fatal("post-commit hook error hid the committed task identity", err)
	}
	view, ok := value.(map[string]any)
	if !ok || view["id"] == "" || view["pending"] == true || view["input"] != nil {
		t.Fatal("post-commit hook error did not return a safe durable task view", value)
	}
	id := view["id"].(string)
	if hookErrors.Load() != 1 {
		t.Fatal("test did not inject exactly one post-commit WAL hook error", hookErrors.Load())
	}
	if finished := waitPrepared(t, s, id); finished.Status != jobs.Succeeded {
		t.Fatal("durable task did not finish after its post-commit hook error", finished.Error)
	}
	replayed, err := s.ExecuteWithKey(context.Background(), actor, b.Action.Name, nil, key)
	if err != nil || replayed.(map[string]any)["id"] != id {
		t.Fatal("same-key replay changed the task ID after post-commit hook error", err)
	}
	if executor.runs.Load() != 1 {
		t.Fatal("post-commit hook error replayed the committed task", executor.runs.Load())
	}
	if tasks, metadata, state := preparedRowCounts(t, s); tasks != 1 || metadata != 1 || state != 1 {
		t.Fatal("post-commit hook error did not preserve one atomic task/metadata/receipt", tasks, metadata, state)
	}
	receipt, err := s.Runtime("prepared_enqueue").State.Get(context.Background(), "receipt", "postcommit-hook")
	if err != nil || string(receipt) != `{"task_id":"`+id+`"}` {
		t.Fatal("committed provider receipt did not match the returned task", err, string(receipt))
	}
	if markerBytes, err := os.ReadFile(marker); err != nil || string(markerBytes) != id {
		t.Fatal("executor marker did not confirm exactly one execution", err)
	}
}

func TestPreparedPermanentEnqueueFailureKeepsRecoveryViewAndIdempotentID(t *testing.T) {
	s, actor := testService(t)
	executor := &preparedEnqueueExecutor{rejectValidationAt: 2}
	b := registerPreparedEnqueueBinding(t, s, executor)
	value, err := s.ExecuteWithKey(context.Background(), actor, b.Action.Name, nil, "prepared-validation-recovery")
	if err != nil {
		t.Fatal(err)
	}
	view := value.(map[string]any)
	id, ok := view["id"].(string)
	if !ok || id == "" || view["state"] != jobs.Queued || view["pending"] != true || view["recovery_required"] != true || view["recovery_state"] != jobs.Queued {
		t.Fatal("permanent validation failure hid the durable recovery state", view)
	}
	if _, present := view["input"]; present {
		t.Fatal("recovery view exposed private task input")
	}
	replayed, err := s.ExecuteWithKey(context.Background(), actor, b.Action.Name, nil, "prepared-validation-recovery")
	if err != nil || replayed.(map[string]any)["id"] != id {
		t.Fatal("idempotency did not preserve the unresolved durable task identity", err)
	}
	if executor.validationCalls.Load() != 2 || executor.runs.Load() != 0 {
		t.Fatal("permanent validation failure was retried or executed", executor.validationCalls.Load(), executor.runs.Load())
	}
	if tasks, metadata, state := preparedRowCounts(t, s); tasks != 1 || metadata != 1 || state != 1 {
		t.Fatal("recovery left incomplete or duplicate committed rows", tasks, metadata, state)
	}
	stored, err := s.jobs.Get(context.Background(), id)
	if err != nil || stored.Status != jobs.Queued {
		t.Fatal("permanent failure did not leave the durable task queued for explicit recovery", err, stored.Status)
	}
}

func TestServicePreparedTransactionsUseFullSynchronousAndSurviveReopen(t *testing.T) {
	s, actor := testService(t)
	marker := filepath.Join(t.TempDir(), "executed")
	executor := &preparedEnqueueExecutor{markerPath: marker}
	b := registerPreparedEnqueueBinding(t, s, executor)
	s.db.SetMaxIdleConns(0)
	for attempt := 0; attempt < 2; attempt++ {
		conn, err := s.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var synchronous int
		var journal string
		if err = conn.QueryRowContext(context.Background(), "PRAGMA synchronous").Scan(&synchronous); err == nil {
			err = conn.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&journal)
		}
		_ = conn.Close()
		if err != nil || synchronous != 2 || journal != "wal" {
			t.Fatal("fresh database connection did not retain WAL with FULL synchronization", synchronous, journal, err)
		}
	}
	value, err := s.ExecuteWithKey(context.Background(), actor, b.Action.Name, nil, "prepared-full-sync")
	if err != nil {
		t.Fatal(err)
	}
	id := value.(map[string]any)["id"].(string)
	if finished := waitPrepared(t, s, id); finished.Status != jobs.Succeeded {
		t.Fatal("prepared operation did not finish before durability check", finished.Error)
	}
	dataDir := s.dataDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var synchronous int
	if err := reopened.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatal("reopened connection did not use FULL synchronization", synchronous, err)
	}
	job, err := reopened.jobs.Get(context.Background(), id)
	if err != nil || job.Status != jobs.Succeeded {
		t.Fatal("committed prepared task did not survive reopening", err, job.Status)
	}
	state, err := reopened.Runtime("prepared_enqueue").State.Get(context.Background(), "receipt", "current")
	if err != nil || string(state) != `{"task_id":"`+id+`"}` {
		t.Fatal("matching transactional metadata did not survive reopening", err, string(state))
	}
}
