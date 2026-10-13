package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

func testStore(t *testing.T) *FileStore {
	t.Helper()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestConcurrentJobProcess(t *testing.T) {
	if os.Getenv("POWERLINUX_JOB_HELPER") != "1" {
		return
	}
	if err := os.WriteFile(os.Args[len(os.Args)-1], []byte("started"), 0600); err != nil {
		os.Exit(21)
	}
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

func TestManagerBoundsRealExecutionsAndCancelsQueuedTask(t *testing.T) {
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
			if err := json.Unmarshal(raw, &number); err != nil || number < 0 || number > 10 {
				return nil, agent.ErrInvalidInput
			}
			return []string{"-test.run=^TestConcurrentJobProcess$", "--", "started-" + strconv.Itoa(number)}, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(context.Background(), runner, testStore(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	created := make([]Job, 4)
	for i := range created {
		created[i], err = manager.Submit(context.Background(), "user-1", "test.wait", json.RawMessage(strconv.Itoa(i)))
		if err != nil {
			t.Fatal(err)
		}
	}
	waitMarker := func(number int) {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(directory, fmt.Sprintf("started-%d", number))); err == nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("process %d did not start", number)
	}
	for i := range created {
		waitMarker(i)
	}
	queued, err := manager.Submit(context.Background(), "user-1", "test.wait", json.RawMessage(`4`))
	if err != nil {
		t.Fatal(err)
	}
	view, err := manager.Get(context.Background(), queued.ID)
	if err != nil || view.Status != Queued {
		t.Fatalf("fifth task started without slot: %+v %v", view, err)
	}
	if _, err := manager.Cancel(context.Background(), queued.ID); err != nil {
		t.Fatal(err)
	}
	if finished := waitJob(t, manager, queued.ID); finished.Status != Canceled || !finished.Result.StartedAt.IsZero() {
		t.Fatalf("queued task was executed: %+v", finished)
	}
	if _, err := os.Stat(filepath.Join(directory, "started-4")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled queued process left startup marker")
	}
	replacement, err := manager.Submit(context.Background(), "user-1", "test.wait", json.RawMessage(`5`))
	if err != nil {
		t.Fatal(err)
	}
	view, _ = manager.Get(context.Background(), replacement.ID)
	if view.Status != Queued {
		t.Fatal("replacement prematurely consumed a slot")
	}
	if _, err := manager.Cancel(context.Background(), created[0].ID); err != nil {
		t.Fatal(err)
	}
	waitMarker(5)
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	for _, job := range append(created, replacement) {
		if finished := waitJob(t, manager, job.ID); finished.Status != Canceled {
			t.Fatalf("shutdown left running process: %+v", finished)
		}
	}
}

func waitJob(t *testing.T, manager *Manager, id string) Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, err := manager.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status != Queued && job.Status != Running {
			return job
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("job did not finish")
	return Job{}
}

func TestDurableRealWriteBackupAndRestart(t *testing.T) {
	root := t.TempDir()
	files, err := agent.NewFilesystem([]agent.RootSpec{{ID: "workspace", Path: root}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	store := testStore(t)
	manager, err := NewManager(context.Background(), agent.NewFileExecutor(files), store)
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(agent.FileInput{Root: "workspace", Path: "note", Content: "shared-note-data"})
	job, err := manager.Submit(context.Background(), "user-1", "file_write", input)
	if err != nil {
		t.Fatal(err)
	}
	finished := waitJob(t, manager, job.ID)
	if finished.Status != Succeeded || finished.Result.ExitCode != 0 {
		t.Fatalf("write failed: %+v", finished)
	}
	input, _ = json.Marshal(agent.FileInput{Root: "workspace", Path: "note", DestinationRoot: "workspace", DestinationPath: "note.backup"})
	backup, err := manager.Submit(context.Background(), "user-1", "file_backup", input)
	if err != nil {
		t.Fatal(err)
	}
	if job := waitJob(t, manager, backup.ID); job.Status != Succeeded {
		t.Fatalf("backup failed: %+v", job)
	}
	data, err := os.ReadFile(filepath.Join(root, "note.backup"))
	if err != nil || string(data) != "shared-note-data" {
		t.Fatal("backup content wrong", err)
	}
	manager.Close()
	restarted, err := NewManager(context.Background(), agent.NewFileExecutor(files), store)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	persisted, err := restarted.Get(context.Background(), backup.ID)
	if err != nil || persisted.Status != Succeeded || persisted.ActorID != "user-1" {
		t.Fatal("completed task lost across restart", err)
	}
	if _, err := manager.Submit(context.Background(), "user-1", "file_write", input); !errors.Is(err, ErrClosed) {
		t.Fatal("closed manager accepted task", err)
	}
}

type blockingExecutor struct{ started chan struct{} }

func (e *blockingExecutor) Validate(request agent.Request) error {
	if request.Operation != "test.wait" {
		return agent.ErrUnauthorized
	}
	return nil
}
func (e *blockingExecutor) Run(ctx context.Context, request agent.Request) (agent.Result, error) {
	close(e.started)
	select {
	case request.Output <- agent.Output{Stream: "stdout", Text: "started\n"}:
	default:
	}
	<-ctx.Done()
	return agent.Result{Operation: request.Operation, Canceled: true, ExitCode: -1}, ctx.Err()
}

func TestCancellationAndBoundedSubscription(t *testing.T) {
	executor := &blockingExecutor{started: make(chan struct{})}
	manager, err := NewManager(context.Background(), executor, testStore(t))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	job, err := manager.Submit(context.Background(), "user-1", "test.wait", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := manager.Subscribe(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	<-executor.started
	cancelResult, err := manager.Cancel(context.Background(), job.ID)
	if err != nil || !cancelResult.CancelRequested {
		t.Fatal("cancel not requested", err)
	}
	finished := waitJob(t, manager, job.ID)
	if finished.Status != Canceled || !finished.Result.Canceled {
		t.Fatalf("not canceled: %+v", finished)
	}
	for range stream {
	} // Subscription closes when task finishes, even without canceling its context.
	cancel() // Must not double-close a completed subscriber.
	if _, err := manager.Submit(context.Background(), "user-1", "sh", json.RawMessage(`{}`)); !errors.Is(err, agent.ErrUnauthorized) {
		t.Fatal("manager accepted unregistered operation", err)
	}
}

func TestRestartDoesNotRepeatUnfinishedMutation(t *testing.T) {
	store := testStore(t)
	job := Job{ID: "0123456789abcdef0123456789abcdef", ActorID: "user-1", Operation: "test.wait", Input: json.RawMessage(`{}`), Status: Running, CreatedAt: time.Now().UTC()}
	if err := store.Save(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	executor := &blockingExecutor{started: make(chan struct{})}
	manager, err := NewManager(context.Background(), executor, store)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	recovered, err := manager.Get(context.Background(), job.ID)
	if err != nil || recovered.Status != Interrupted {
		t.Fatal("unfinished action not marked interrupted", err)
	}
	select {
	case <-executor.started:
		t.Fatal("unfinished mutation silently reran")
	default:
	}
}

func TestFileStoreMissingInvalidIDAndRestrictivePermissions(t *testing.T) {
	store := testStore(t)
	if _, err := store.Get(context.Background(), "../../outside"); !errors.Is(err, ErrNotFound) {
		t.Fatal("invalid ID accepted", err)
	}
	if _, err := store.Get(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing task not reported", err)
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(directory); err == nil {
		t.Fatal("publicly accessible job store accepted")
	}
}
