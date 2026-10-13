// Package jobs runs durable, explicitly registered Agent operations. It is
// independent of authentication; the shared business layer owns actor/resource
// authorization before Submit, Get, Cancel or Subscribe is exposed remotely.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

var ErrNotFound = errors.New("job not found")
var ErrClosed = errors.New("job manager closed")
var ErrBusy = errors.New("job queue is full")

const (
	Queued      = "queued"
	Running     = "running"
	Succeeded   = "succeeded"
	Failed      = "failed"
	Canceled    = "canceled"
	Interrupted = "interrupted"
)

type Job struct {
	ID              string          `json:"id"`
	ActorID         string          `json:"actor_id"`
	Operation       string          `json:"operation"`
	Input           json.RawMessage `json:"input"`
	Status          string          `json:"status"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	CancelRequested bool            `json:"cancel_requested"`
	Result          *agent.Result   `json:"result,omitempty"`
	Error           string          `json:"error,omitempty"`
}

// Store.Save must atomically persist the complete record. It must never log
// Input, which can contain private files. Get/List return independent copies.
type Store interface {
	Save(context.Context, Job) error
	Get(context.Context, string) (Job, error)
	List(context.Context) ([]Job, error)
}

type Executor interface {
	Validate(agent.Request) error
	Run(context.Context, agent.Request) (agent.Result, error)
}

type activeJob struct {
	job         Job
	cancel      context.CancelFunc
	subscribers map[chan agent.Output]struct{}
	done        chan struct{}
}

type Manager struct {
	mu                sync.Mutex
	ctx               context.Context
	cancel            context.CancelFunc
	executor          Executor
	store             Store
	active            map[string]*activeJob
	reservations      map[string]*Reservation
	failedPersistence map[string]Job
	closed            bool
	wg                sync.WaitGroup
	slots             chan struct{}
}

// NewManager records unfinished tasks as interrupted after a process restart.
// Retrying a mutation automatically could duplicate a completed host action.
func NewManager(ctx context.Context, executor Executor, store Store) (*Manager, error) {
	if executor == nil || store == nil {
		return nil, errors.New("executor and durable store are required")
	}
	all, err := store.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, job := range all {
		if job.Status == Queued || job.Status == Running {
			job.Status, job.Error, job.UpdatedAt = Interrupted, "service restarted before task completion; inspect before retrying", time.Now().UTC()
			if err := store.Save(ctx, job); err != nil {
				return nil, err
			}
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	return &Manager{ctx: ctx, cancel: cancel, executor: executor, store: store, active: make(map[string]*activeJob), reservations: make(map[string]*Reservation), failedPersistence: make(map[string]Job), slots: make(chan struct{}, 4)}, nil
}

func (m *Manager) Submit(ctx context.Context, actorID, operation string, input json.RawMessage) (Job, error) {
	if actorID == "" {
		return Job{}, errors.New("actor ID required")
	}
	request := agent.Request{Operation: operation, Input: append(json.RawMessage(nil), input...)}
	if err := m.executor.Validate(request); err != nil {
		return Job{}, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Job{}, err
	}
	now := time.Now().UTC()
	job := Job{ID: hex.EncodeToString(random[:]), ActorID: actorID, Operation: operation, Input: request.Input, Status: Queued, CreatedAt: now, UpdatedAt: now}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return Job{}, ErrClosed
	}
	if len(m.active)+len(m.reservations) >= 128 {
		return Job{}, ErrBusy
	}
	if err := m.store.Save(ctx, job); err != nil {
		return Job{}, err
	}
	m.enqueueLocked(job)
	return clone(job), nil
}

func (m *Manager) run(ctx context.Context, a *activeJob) {
	defer m.wg.Done()
	defer a.cancel()
	// Bound native operations as well as subprocesses. A queued task must not
	// allocate its bounded file/output buffers before owning an execution slot.
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-ctx.Done():
		m.finishCanceledBeforeStart(a, ctx.Err())
		return
	}
	if err := ctx.Err(); err != nil {
		m.finishCanceledBeforeStart(a, err)
		return
	}
	m.mu.Lock()
	a.job.Status, a.job.UpdatedAt = Running, time.Now().UTC()
	if err := m.store.Save(context.WithoutCancel(m.ctx), a.job); err != nil {
		a.job.Status, a.job.Error = Failed, "cannot persist running task: "+err.Error()
		m.finishLocked(a)
		m.mu.Unlock()
		return
	}
	request := agent.Request{ActorID: a.job.ActorID, JobID: a.job.ID, Operation: a.job.Operation, Input: append(json.RawMessage(nil), a.job.Input...)}
	m.mu.Unlock()
	output := make(chan agent.Output, 64)
	forwarded := make(chan struct{})
	go func() {
		defer close(forwarded)
		for event := range output {
			m.mu.Lock()
			for subscriber := range a.subscribers {
				select {
				case subscriber <- event:
				default:
				}
			}
			m.mu.Unlock()
		}
	}()
	request.Output = output
	result, err := m.executor.Run(ctx, request)
	close(output)
	<-forwarded
	m.mu.Lock()
	defer m.mu.Unlock()
	a.job.Result, a.job.UpdatedAt = &result, time.Now().UTC()
	a.job.Status = Succeeded
	if err != nil {
		a.job.Status, a.job.Error = Failed, err.Error()
		if errors.Is(err, context.Canceled) {
			a.job.Status = Canceled
		}
	}
	m.finishLocked(a)
}

func (m *Manager) finishCanceledBeforeStart(a *activeJob, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	a.job.Status, a.job.UpdatedAt, a.job.Error = Canceled, now, err.Error()
	a.job.Result = &agent.Result{Operation: a.job.Operation, ExitCode: -1, Canceled: true, FinishedAt: now}
	m.finishLocked(a)
}

func (m *Manager) finishLocked(a *activeJob) {
	if err := m.store.Save(context.WithoutCancel(m.ctx), a.job); err != nil {
		// The previously durable running record will recover as interrupted.
		// Never return a fabricated success if final persistence failed.
		a.job.Status, a.job.Error = Failed, "cannot persist completed task: "+err.Error()
		m.failedPersistence[a.job.ID] = clone(a.job)
	}
	for subscriber := range a.subscribers {
		close(subscriber)
		delete(a.subscribers, subscriber)
	}
	close(a.done)
	delete(m.active, a.job.ID)
}

func (m *Manager) Get(ctx context.Context, id string) (Job, error) {
	m.mu.Lock()
	if a, ok := m.active[id]; ok {
		job := clone(a.job)
		m.mu.Unlock()
		return job, nil
	}
	if job, ok := m.failedPersistence[id]; ok {
		m.mu.Unlock()
		return clone(job), nil
	}
	m.mu.Unlock()
	return m.store.Get(ctx, id)
}

func (m *Manager) List(ctx context.Context) ([]Job, error) {
	all, err := m.store.List(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, job := range all {
		if failure, ok := m.failedPersistence[job.ID]; ok {
			all[i] = clone(failure)
		}
	}
	return all, nil
}

// Cancel requests cancellation; terminal state is persisted only after the
// executor has stopped. An atomic action that already committed may succeed.
func (m *Manager) Cancel(ctx context.Context, id string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.active[id]; ok {
		a.job.CancelRequested, a.job.UpdatedAt = true, time.Now().UTC()
		if err := m.store.Save(ctx, a.job); err != nil {
			return Job{}, err
		}
		a.cancel()
		return clone(a.job), nil
	}
	if job, ok := m.failedPersistence[id]; ok {
		return clone(job), nil
	}
	job, err := m.store.Get(ctx, id)
	if err != nil || job.Status != Queued {
		return job, err
	}
	// Atomic submissions can be committed before EnsureQueued. Cancellation
	// in that gap must persist a terminal record without consuming a slot.
	now := time.Now().UTC()
	job.Status, job.CancelRequested, job.UpdatedAt = Canceled, true, now
	job.Result = &agent.Result{Operation: job.Operation, ExitCode: -1, Canceled: true, FinishedAt: now}
	if err := m.store.Save(ctx, job); err != nil {
		return Job{}, err
	}
	delete(m.reservations, id)
	return clone(job), nil
}

// Subscribe streams live output with bounded buffers. The final bounded output
// remains in Job.Result for reconnecting clients; replay is intentionally not
// mixed into this stream. Call Get to inspect its durable status and result.
func (m *Manager) Subscribe(ctx context.Context, id string) (<-chan agent.Output, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	channel := make(chan agent.Output, 64)
	a, ok := m.active[id]
	if !ok {
		if _, err := m.store.Get(ctx, id); err != nil {
			return nil, err
		}
		close(channel)
		return channel, nil
	}
	a.subscribers[channel] = struct{}{}
	go func() {
		select {
		case <-ctx.Done():
		case <-a.done:
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if _, exists := a.subscribers[channel]; exists {
			delete(a.subscribers, channel)
			close(channel)
		}
	}()
	return channel, nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		m.cancel()
		clear(m.reservations)
	}
	m.mu.Unlock()
	m.wg.Wait()
	return nil
}

func clone(job Job) Job {
	job.Input = append(json.RawMessage(nil), job.Input...)
	if job.Result != nil {
		result := *job.Result
		job.Result = &result
	}
	return job
}

func (j Job) String() string { return fmt.Sprintf("%s (%s): %s", j.ID, j.Operation, j.Status) }
