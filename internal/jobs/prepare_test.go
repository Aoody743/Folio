package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

type preparedRealExecutor struct {
	delegate       Executor
	runs           atomic.Int32
	mu             sync.Mutex
	actorID, jobID string
	after          func()
}

type countingPreparedStore struct {
	Store
	getCalls atomic.Int32
}

func (s *countingPreparedStore) Get(ctx context.Context, id string) (Job, error) {
	s.getCalls.Add(1)
	return s.Store.Get(ctx, id)
}

func (e *preparedRealExecutor) Validate(request agent.Request) error {
	return e.delegate.Validate(request)
}
func (e *preparedRealExecutor) Run(ctx context.Context, request agent.Request) (agent.Result, error) {
	e.runs.Add(1)
	e.mu.Lock()
	e.actorID, e.jobID = request.ActorID, request.JobID
	e.mu.Unlock()
	result, err := e.delegate.Run(ctx, request)
	if e.after != nil {
		e.after()
	}
	return result, err
}
func preparedFilesystem(t *testing.T) (string, *preparedRealExecutor) {
	t.Helper()
	directory := t.TempDir()
	files, err := agent.NewFilesystem([]agent.RootSpec{{ID: "workspace", Path: directory}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	return directory, &preparedRealExecutor{delegate: agent.NewFileExecutor(files)}
}
func preparedInput(path string) json.RawMessage {
	raw, _ := json.Marshal(agent.FileInput{Root: "workspace", Path: path, Content: "committed real file data"})
	return raw
}
func preparedManager(t *testing.T, executor Executor, store Store) *Manager {
	t.Helper()
	manager, err := NewManager(context.Background(), executor, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	return manager
}
func preparedLease(t *testing.T, manager *Manager, path string) *Reservation {
	t.Helper()
	reservation, err := manager.Prepare(context.Background(), "trusted-actor", "file_write", preparedInput(path))
	if err != nil {
		t.Fatal(err)
	}
	return reservation
}
func preparedCommit(t *testing.T, store Store, reservation *Reservation) {
	t.Helper()
	if err := store.Save(context.Background(), reservation.Job()); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedRealWriteWaitsForCommitAndConcurrentEnsureDoesNotReplay(t *testing.T) {
	directory, executor := preparedFilesystem(t)
	store := testStore(t)
	manager := preparedManager(t, executor, store)
	reservation := preparedLease(t, manager, "note")
	job := reservation.Job()
	edited := reservation.Job()
	edited.ActorID, edited.Input[0] = "forged", '!'
	if reservation.Job().ActorID != "trusted-actor" || reservation.Job().Input[0] != '{' {
		t.Fatal("Job returned a mutable reservation backing value")
	}
	if _, err := store.Get(context.Background(), job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("Prepare wrote a durable record", err)
	}
	if view, err := manager.EnsureQueued(context.Background(), job.ID, reservation); !errors.Is(err, ErrNotFound) || view.ID != job.ID {
		t.Fatal("uncommitted job started or lost its prepared identity", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "note")); !errors.Is(err, os.ErrNotExist) || executor.runs.Load() != 0 {
		t.Fatal("uncommitted preparation executed")
	}
	preparedCommit(t, store, reservation)
	var group sync.WaitGroup
	failures := make(chan error, 64)
	for index := range 64 {
		group.Go(func() {
			var lease *Reservation
			if index%2 == 0 {
				lease = reservation
			}
			_, err := manager.EnsureQueued(context.Background(), job.ID, lease)
			failures <- err
		})
	}
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if finished := waitJob(t, manager, job.ID); finished.Status != Succeeded {
		t.Fatal("real committed write failed")
	}
	data, err := os.ReadFile(filepath.Join(directory, "note"))
	if err != nil || string(data) != "committed real file data" || executor.runs.Load() != 1 {
		t.Fatal("concurrent Ensure did not perform exactly one physical write", err)
	}
	executor.mu.Lock()
	if executor.actorID != job.ActorID || executor.jobID != job.ID {
		t.Error("trusted actor/job identity was lost at Run")
	}
	executor.mu.Unlock()
	if err := os.WriteFile(filepath.Join(directory, "note"), []byte("later human edit"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 16 {
		if result, err := manager.EnsureQueued(context.Background(), job.ID, nil); err != nil || result.Status != Succeeded {
			t.Fatal("terminal Ensure was not idempotent", err)
		}
	}
	data, _ = os.ReadFile(filepath.Join(directory, "note"))
	if string(data) != "later human edit" || executor.runs.Load() != 1 {
		t.Fatal("terminal Ensure replayed a completed mutation")
	}
	if err := manager.Abort(reservation); !errors.Is(err, ErrCommitted) {
		t.Fatal("completed reservation was abortable", err)
	}
}

func TestPreparedConcurrentReservationsAndLegacySubmissionShare128Capacity(t *testing.T) {
	_, executor := preparedFilesystem(t)
	store := testStore(t)
	manager := preparedManager(t, executor, store)
	type result struct {
		reservation *Reservation
		err         error
	}
	results := make(chan result, 160)
	var group sync.WaitGroup
	for index := range 160 {
		group.Go(func() {
			lease, err := manager.Prepare(context.Background(), "trusted-actor", "file_write", preparedInput(fmt.Sprintf("reserved-%d", index)))
			results <- result{lease, err}
		})
	}
	group.Wait()
	close(results)
	reservations := []*Reservation{}
	rejected := 0
	for result := range results {
		if result.err == nil {
			reservations = append(reservations, result.reservation)
		} else if errors.Is(result.err, ErrBusy) {
			rejected++
		} else {
			t.Fatal(result.err)
		}
	}
	if len(reservations) != 128 || rejected != 32 || executor.runs.Load() != 0 {
		t.Fatal("concurrent preparation exceeded capacity or executed")
	}
	if records, err := store.List(context.Background()); err != nil || len(records) != 0 {
		t.Fatal("capacity reservation persisted jobs before commit", err)
	}
	if _, err := manager.Submit(context.Background(), "trusted-actor", "file_write", preparedInput("legacy-blocked")); !errors.Is(err, ErrBusy) {
		t.Fatal("legacy submission bypassed prepared capacity", err)
	}
	if err := manager.Abort(reservations[0]); err != nil {
		t.Fatal(err)
	}
	if err := manager.Abort(reservations[0]); err != nil {
		t.Fatal("uncommitted Abort was not idempotent", err)
	}
	legacy, err := manager.Submit(context.Background(), "trusted-actor", "file_write", preparedInput("legacy"))
	if err != nil {
		t.Fatal(err)
	}
	if finished := waitJob(t, manager, legacy.ID); finished.Status != Succeeded {
		t.Fatal("released capacity was unavailable to legacy Submit")
	}
	replacement := preparedLease(t, manager, "replacement")
	for _, lease := range reservations[1:] {
		if err := manager.Abort(lease); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.Abort(replacement); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.Prepare(ctx, "trusted-actor", "file_write", preparedInput("cancelled")); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled Prepare reserved capacity", err)
	}
	if _, err := manager.Prepare(context.Background(), "trusted-actor", "sh", preparedInput("unsafe")); !errors.Is(err, agent.ErrUnauthorized) {
		t.Fatal("Prepare accepted an unregistered operation", err)
	}
	manager.mu.Lock()
	count := len(manager.reservations)
	manager.mu.Unlock()
	if count != 0 || executor.runs.Load() != 1 {
		t.Fatal("Abort or invalid preparation leaked capacity")
	}
}

func TestPreparedCommitCancellationAndClosePreserveDurableRecovery(t *testing.T) {
	for _, mode := range []string{"caller_canceled", "prestart_cancel", "closed"} {
		t.Run(mode, func(t *testing.T) {
			directory, executor := preparedFilesystem(t)
			store := testStore(t)
			manager := preparedManager(t, executor, store)
			reservation := preparedLease(t, manager, "note")
			job := reservation.Job()
			preparedCommit(t, store, reservation)
			switch mode {
			case "caller_canceled":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				view, err := manager.EnsureQueued(ctx, job.ID, reservation)
				if !errors.Is(err, context.Canceled) || view.ID != job.ID || view.Status != Queued {
					t.Fatal("canceled caller lost a committed identity", err)
				}
				if err := manager.Abort(reservation); !errors.Is(err, ErrCommitted) {
					t.Fatal("caller cancellation rolled back a committed reservation", err)
				}
				if executor.runs.Load() != 0 {
					t.Fatal("canceled enqueue started work")
				}
				if _, err := manager.EnsureQueued(context.Background(), job.ID, nil); err != nil {
					t.Fatal(err)
				}
				if finished := waitJob(t, manager, job.ID); finished.Status != Succeeded {
					t.Fatal("service-lifetime retry did not execute committed work")
				}
			case "prestart_cancel":
				view, err := manager.Cancel(context.Background(), job.ID)
				if err != nil || view.Status != Canceled || !view.CancelRequested || view.Result == nil || !view.Result.StartedAt.IsZero() {
					t.Fatal("committed gap cancellation was not durable and execution-free", err)
				}
				if view, err := manager.EnsureQueued(context.Background(), job.ID, reservation); err != nil || view.Status != Canceled {
					t.Fatal("Ensure replayed a canceled committed task", err)
				}
			case "closed":
				if err := manager.Close(); err != nil {
					t.Fatal(err)
				}
				view, err := manager.EnsureQueued(context.Background(), job.ID, reservation)
				if !errors.Is(err, ErrClosed) || view.ID != job.ID || view.Status != Queued {
					t.Fatal("shutdown discarded the committed pending identity", err)
				}
				if _, err := manager.Prepare(context.Background(), "trusted-actor", "file_write", preparedInput("later")); !errors.Is(err, ErrClosed) {
					t.Fatal("closed manager accepted preparation", err)
				}
				restarted := preparedManager(t, executor, store)
				view, err = restarted.EnsureQueued(context.Background(), job.ID, nil)
				if err != nil || view.Status != Interrupted {
					t.Fatal("restart replayed an unknown committed side effect", err)
				}
			}
			if mode != "caller_canceled" {
				if executor.runs.Load() != 0 {
					t.Fatal("canceled/shutdown task executed")
				}
				if _, err := os.Stat(filepath.Join(directory, "note")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unknown work modified the filesystem")
				}
			}
		})
	}
}

func TestEnsureQueuedCanceledContextNeverDetachesAStoreRead(t *testing.T) {
	_, executor := preparedFilesystem(t)
	base := testStore(t)
	store := &countingPreparedStore{Store: base}
	manager := preparedManager(t, executor, store)
	reservation := preparedLease(t, manager, "cancel-before-commit")
	job := reservation.Job()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	view, err := manager.EnsureQueued(canceled, job.ID, reservation)
	if !errors.Is(err, context.Canceled) || view.ID != job.ID || view.Status != Queued {
		t.Fatal("canceled uncommitted request lost its opaque reservation identity", err, view)
	}
	if store.getCalls.Load() != 0 {
		t.Fatal("canceled uncommitted request detached its context for a store read")
	}
	if _, err := base.Get(context.Background(), job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("canceled preparation was mistaken for a durable commit", err)
	}

	preparedCommit(t, base, reservation)
	view, err = manager.EnsureQueued(canceled, job.ID, reservation)
	if !errors.Is(err, context.Canceled) || view.ID != job.ID || view.Status != Queued {
		t.Fatal("canceled committed task lost its opaque recovery identity", err, view)
	}
	if store.getCalls.Load() != 0 {
		t.Fatal("service-lifetime cancellation detached its context for a store read")
	}
	if view, err = manager.EnsureQueued(context.Background(), job.ID, reservation); err != nil || view.ID != job.ID {
		t.Fatal("live service context did not verify and queue the committed task", err)
	}
	if store.getCalls.Load() != 1 {
		t.Fatal("uncanceled retry did not verify its committed task through the store", store.getCalls.Load())
	}
	if finished := waitJob(t, manager, job.ID); finished.Status != Succeeded || executor.runs.Load() != 1 {
		t.Fatal("committed work did not run once after a live retry")
	}
}

func TestPreparedCommittedIdentityAndForeignReservationCannotBeSubstituted(t *testing.T) {
	for _, field := range []string{"actor", "input", "operation", "created_at"} {
		t.Run(field, func(t *testing.T) {
			_, executor := preparedFilesystem(t)
			store := testStore(t)
			manager := preparedManager(t, executor, store)
			reservation := preparedLease(t, manager, "note")
			original := reservation.Job()
			changed := reservation.Job()
			switch field {
			case "actor":
				changed.ActorID = "substituted-actor"
			case "input":
				changed.Input = preparedInput("substituted-path")
			case "operation":
				changed.Operation = "sh"
			case "created_at":
				changed.CreatedAt = changed.CreatedAt.Add(time.Second)
			}
			if err := store.Save(context.Background(), changed); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.EnsureQueued(context.Background(), original.ID, reservation); !errors.Is(err, ErrReservation) {
				t.Fatal("committed identity mismatch was executed", err)
			}
			if executor.runs.Load() != 0 {
				t.Fatal("identity mismatch reached Run")
			}
			if err := store.Save(context.Background(), original); err != nil {
				t.Fatal(err)
			}
			foreignManager := preparedManager(t, executor, testStore(t))
			foreign := preparedLease(t, foreignManager, "foreign")
			if _, err := manager.EnsureQueued(context.Background(), original.ID, foreign); !errors.Is(err, ErrReservation) {
				t.Fatal("another Manager's token was accepted", err)
			}
			if err := foreignManager.Abort(foreign); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.EnsureQueued(context.Background(), original.ID, reservation); err != nil {
				t.Fatal(err)
			}
			if finished := waitJob(t, manager, original.ID); finished.Status != Succeeded || executor.runs.Load() != 1 {
				t.Fatal("valid original identity did not run once")
			}
		})
	}
}

// Closing an actual pinned FileStore descriptor exercises OS persistence
// failure rather than fabricating a successful in-memory durable record.
type preparedDescriptorStore struct {
	mu              sync.Mutex
	current         *FileStore
	closeOnRunning  bool
	closeOnCanceled bool
}

func (s *preparedDescriptorStore) Save(ctx context.Context, job Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeOnRunning && job.Status == Running {
		s.closeOnRunning = false
		s.current.Close()
	}
	if s.closeOnCanceled && job.Status == Canceled {
		s.closeOnCanceled = false
		s.current.Close()
	}
	return s.current.Save(ctx, job)
}
func (s *preparedDescriptorStore) Get(ctx context.Context, id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current.Get(ctx, id)
}
func (s *preparedDescriptorStore) List(ctx context.Context) ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current.List(ctx)
}
func (s *preparedDescriptorStore) close() { s.mu.Lock(); defer s.mu.Unlock(); s.current.Close() }
func (s *preparedDescriptorStore) reopen(t *testing.T, directory string) {
	t.Helper()
	next, err := NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.current = next
	s.mu.Unlock()
	t.Cleanup(func() { next.Close() })
}

func TestPreparedCommittedReadFailureKeepsReservationAndSafeRetry(t *testing.T) {
	directory, executor := preparedFilesystem(t)
	storeDirectory := t.TempDir()
	base, err := NewFileStore(storeDirectory)
	if err != nil {
		t.Fatal(err)
	}
	store := &preparedDescriptorStore{current: base}
	t.Cleanup(store.close)
	manager := preparedManager(t, executor, store)
	reservation := preparedLease(t, manager, "note")
	preparedCommit(t, store, reservation)
	store.close()
	view, err := manager.EnsureQueued(context.Background(), reservation.Job().ID, reservation)
	if err == nil || view.ID != reservation.Job().ID || executor.runs.Load() != 0 {
		t.Fatal("failed durable read started or lost committed work", err)
	}
	if err := manager.Abort(reservation); err == nil {
		t.Fatal("an unreadable store was guessed to contain no commit")
	}
	manager.mu.Lock()
	leases := len(manager.reservations)
	manager.mu.Unlock()
	if leases != 1 {
		t.Fatal("unknown persistence released its reserved capacity")
	}
	store.reopen(t, storeDirectory)
	if _, err := manager.EnsureQueued(context.Background(), reservation.Job().ID, nil); err != nil {
		t.Fatal(err)
	}
	if finished := waitJob(t, manager, reservation.Job().ID); finished.Status != Succeeded {
		t.Fatal("committed retry did not execute")
	}
	if data, err := os.ReadFile(filepath.Join(directory, "note")); err != nil || string(data) != "committed real file data" || executor.runs.Load() != 1 {
		t.Fatal("retry lost the physical write", err)
	}
}

func TestPreparedActualPersistenceFailureNeverReplaysFilesystemMutation(t *testing.T) {
	for _, mode := range []string{"running", "finished"} {
		t.Run(mode, func(t *testing.T) {
			directory, executor := preparedFilesystem(t)
			storeDirectory := t.TempDir()
			base, err := NewFileStore(storeDirectory)
			if err != nil {
				t.Fatal(err)
			}
			store := &preparedDescriptorStore{current: base}
			t.Cleanup(store.close)
			manager := preparedManager(t, executor, store)
			reservation := preparedLease(t, manager, "note")
			preparedCommit(t, store, reservation)
			if mode == "running" {
				store.closeOnRunning = true
			} else {
				executor.after = store.close
			}
			if _, err := manager.EnsureQueued(context.Background(), reservation.Job().ID, reservation); err != nil {
				t.Fatal(err)
			}
			if finished := waitJob(t, manager, reservation.Job().ID); finished.Status != Failed {
				t.Fatal("actual store failure reported fabricated success")
			}
			expectedRuns := int32(0)
			if mode == "finished" {
				expectedRuns = 1
				if data, err := os.ReadFile(filepath.Join(directory, "note")); err != nil || string(data) != "committed real file data" {
					t.Fatal("successful physical mutation did not precede persistence failure", err)
				}
				if err := os.WriteFile(filepath.Join(directory, "note"), []byte("later human edit"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if view, err := manager.EnsureQueued(context.Background(), reservation.Job().ID, nil); err != nil || view.Status != Failed {
				t.Fatal("failedPersistence was replayed", err)
			}
			if executor.runs.Load() != expectedRuns {
				t.Fatal("Ensure reran a failed-persistence operation")
			}
			manager.Close()
			store.reopen(t, storeDirectory)
			restarted := preparedManager(t, executor, store)
			if view, err := restarted.EnsureQueued(context.Background(), reservation.Job().ID, nil); err != nil || view.Status != Interrupted {
				t.Fatal("restart silently repeated unconfirmed mutation", err)
			}
			if executor.runs.Load() != expectedRuns {
				t.Fatal("interrupted mutation executed on restart")
			}
			if mode == "finished" {
				if data, _ := os.ReadFile(filepath.Join(directory, "note")); string(data) != "later human edit" {
					t.Fatal("restart replay overwrote the later edit")
				}
			} else if _, err := os.Stat(filepath.Join(directory, "note")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("work began without durable running state")
			}
		})
	}
}

func TestPreparedRealProcessesRespectFourSlotsAndCommittedCancellation(t *testing.T) {
	directory := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner, err := agent.NewRunner(agent.Options{MaxConcurrent: 8, Operations: []agent.Operation{{
		Name: "test.wait", Program: binary, WorkDir: directory, Timeout: 30 * time.Second,
		Environment: []string{"POWERLINUX_JOB_HELPER=1", "GORACE=atexit_sleep_ms=0", "GOMAXPROCS=2"},
		BuildArgs: func(raw json.RawMessage) ([]string, error) {
			var number int
			if json.Unmarshal(raw, &number) != nil || number < 0 || number > 10 {
				return nil, agent.ErrInvalidInput
			}
			return []string{"-test.run=^TestConcurrentJobProcess$", "--", "started-" + strconv.Itoa(number)}, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	manager := preparedManager(t, runner, store)
	commitProcess := func(number int, enqueue bool) Job {
		t.Helper()
		reservation, err := manager.Prepare(context.Background(), "trusted-actor", "test.wait", json.RawMessage(strconv.Itoa(number)))
		if err != nil {
			t.Fatal(err)
		}
		preparedCommit(t, store, reservation)
		job := reservation.Job()
		if enqueue {
			if _, err := manager.EnsureQueued(context.Background(), job.ID, reservation); err != nil {
				t.Fatal(err)
			}
		}
		return job
	}
	waitMarker := func(number int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(directory, fmt.Sprintf("started-%d", number))); err == nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("prepared real process failed to start")
	}
	jobs := []Job{}
	for number := range 4 {
		jobs = append(jobs, commitProcess(number, true))
	}
	for number := range 4 {
		waitMarker(number)
	}
	beforeStart := commitProcess(4, false)
	if view, err := manager.Cancel(context.Background(), beforeStart.ID); err != nil || view.Status != Canceled {
		t.Fatal("committed-before-enqueue cancellation failed", err)
	}
	if view, err := manager.EnsureQueued(context.Background(), beforeStart.ID, nil); err != nil || view.Status != Canceled {
		t.Fatal("canceled committed task entered the process queue", err)
	}
	fifth := commitProcess(5, true)
	if view, err := manager.Get(context.Background(), fifth.ID); err != nil || view.Status != Queued {
		t.Fatal("prepared fifth process exceeded the execution budget", err)
	}
	for _, number := range []int{4, 5} {
		if _, err := os.Stat(filepath.Join(directory, fmt.Sprintf("started-%d", number))); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("a canceled/waiting process consumed a fifth execution slot")
		}
	}
	if _, err := manager.Cancel(context.Background(), jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	waitMarker(5)
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	for _, job := range append(jobs, fifth) {
		if view := waitJob(t, manager, job.ID); view.Status != Canceled {
			t.Fatal("shutdown left a prepared process running")
		}
	}
	if _, err := os.Stat(filepath.Join(directory, "started-4")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled committed process executed later")
	}
}

func TestPreparedCommittedCancelRequestUsesNoQueueCapacityOrExecution(t *testing.T) {
	_, executor := preparedFilesystem(t)
	store := testStore(t)
	manager := preparedManager(t, executor, store)
	reservations := []*Reservation{}
	for index := range 128 {
		reservations = append(reservations, preparedLease(t, manager, fmt.Sprintf("capacity-%d", index)))
	}
	// A committed recovery identity with a persisted cancellation does not
	// need a new capacity lease, even if the queue is completely reserved.
	job := Job{ID: "cccccccccccccccccccccccccccccccc", ActorID: "trusted-actor", Operation: "file_write", Input: preparedInput("never-start"), Status: Queued, CancelRequested: true, CreatedAt: time.Now().UTC()}
	if err := store.Save(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	view, err := manager.EnsureQueued(context.Background(), job.ID, nil)
	if err != nil || view.Status != Canceled || view.Result == nil || !view.Result.StartedAt.IsZero() || executor.runs.Load() != 0 {
		t.Fatal("persisted cancellation consumed capacity or executed", err)
	}
	persisted, err := store.Get(context.Background(), job.ID)
	if err != nil || persisted.Status != Canceled {
		t.Fatal("cancellation was not durably terminal", err)
	}
	for _, lease := range reservations {
		if err := manager.Abort(lease); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPreparedCommittedCancelPersistenceFailureIsNeverFabricatedTerminalSuccess(t *testing.T) {
	_, executor := preparedFilesystem(t)
	directory := t.TempDir()
	base, err := NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	store := &preparedDescriptorStore{current: base}
	t.Cleanup(store.close)
	manager := preparedManager(t, executor, store)
	reservation := preparedLease(t, manager, "never-start")
	job := reservation.Job()
	job.CancelRequested = true
	if err := store.Save(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	store.closeOnCanceled = true
	view, err := manager.EnsureQueued(context.Background(), job.ID, reservation)
	if err == nil || view.Status != Failed || executor.runs.Load() != 0 {
		t.Fatal("failed cancellation persistence became success", err)
	}
	if view, err := manager.EnsureQueued(context.Background(), job.ID, nil); err != nil || view.Status != Failed {
		t.Fatal("failed-persistence cancellation was replayed", err)
	}
	manager.Close()
	store.reopen(t, directory)
	restarted := preparedManager(t, executor, store)
	if view, err := restarted.EnsureQueued(context.Background(), job.ID, nil); err != nil || view.Status != Interrupted || executor.runs.Load() != 0 {
		t.Fatal("failed cancellation silently resumed after restart", err)
	}
}
