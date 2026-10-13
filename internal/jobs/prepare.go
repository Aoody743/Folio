package jobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

var ErrReservation = errors.New("invalid job reservation")
var ErrCommitted = errors.New("job reservation is already committed")

// Reservation is an opaque, Manager-issued capacity lease. Job returns an
// independent value for a trusted caller to persist in its own transaction.
// Preparing never writes Store and never starts an executor.
type Reservation struct {
	manager *Manager
	job     Job
}

func (r *Reservation) Job() Job {
	if r == nil {
		return Job{}
	}
	return clone(r.job)
}

func (m *Manager) Prepare(ctx context.Context, actorID, operation string, input json.RawMessage) (*Reservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if actorID == "" {
		return nil, errors.New("actor ID required")
	}
	request := agent.Request{Operation: operation, Input: append(json.RawMessage(nil), input...)}
	if err := m.executor.Validate(request); err != nil {
		return nil, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	reservation := &Reservation{manager: m, job: Job{
		ID: hex.EncodeToString(random[:]), ActorID: actorID, Operation: operation,
		Input: request.Input, Status: Queued, CreatedAt: now, UpdatedAt: now,
	}}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(m.active)+len(m.reservations) >= 128 {
		return nil, ErrBusy
	}
	m.reservations[reservation.job.ID] = reservation
	return reservation, nil
}

// Abort releases capacity only after the caller has rolled back its external
// transaction. A visible Store record is committed and cannot be aborted. A
// failed read retains capacity instead of guessing that the transaction failed.
// After commit, callers must retry EnsureQueued, never call Abort.
func (m *Manager) Abort(reservation *Reservation) error {
	if reservation == nil || reservation.manager != m {
		return ErrReservation
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(m.ctx), 5*time.Second)
	defer cancel()
	_, err := m.store.Get(ctx, reservation.job.ID)
	if err == nil {
		return ErrCommitted
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	if current := m.reservations[reservation.job.ID]; current != nil && current != reservation {
		return ErrReservation
	}
	delete(m.reservations, reservation.job.ID)
	return nil
}

func sameReservedJob(reservation *Reservation, job Job) bool {
	return reservation.job.ID == job.ID && reservation.job.ActorID == job.ActorID &&
		reservation.job.Operation == job.Operation && bytes.Equal(reservation.job.Input, job.Input) &&
		reservation.job.CreatedAt.Equal(job.CreatedAt)
}

// EnsureQueued reads the committed job, not the caller's Job copy. Existing
// active/terminal records are idempotent; a queued record starts at most once.
// A nil reservation is only a retry of an already committed identity.
//
// Call this with the service lifetime context after COMMIT. An expired caller
// context prevents starting new work and returns its durable record with an
// error; it does not roll back metadata or authorize discarding a reservation.
func (m *Manager) EnsureQueued(ctx context.Context, id string, reservation *Reservation) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if reservation != nil && (reservation.manager != m || reservation.job.ID != id) {
		return Job{}, ErrReservation
	}
	if active := m.active[id]; active != nil {
		if reservation != nil && !sameReservedJob(reservation, active.job) {
			return Job{}, ErrReservation
		}
		return clone(active.job), nil
	}
	if failed, exists := m.failedPersistence[id]; exists {
		if reservation != nil && !sameReservedJob(reservation, failed) {
			return Job{}, ErrReservation
		}
		return clone(failed), nil
	}
	// The read must remain under m.mu: a lock-free queued snapshot can become
	// stale after a fast executor finishes and otherwise cause a second Run.
	readCtx := ctx
	var cancel context.CancelFunc
	if ctx.Err() != nil {
		readCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
	}
	job, err := m.store.Get(readCtx, id)
	if err != nil {
		if reservation != nil {
			return reservation.Job(), err
		}
		return Job{}, err
	}
	if job.ID != id || reservation != nil && !sameReservedJob(reservation, job) {
		return Job{}, ErrReservation
	}
	lease := m.reservations[id]
	if lease != nil && (reservation != nil && lease != reservation || !sameReservedJob(lease, job)) {
		return Job{}, ErrReservation
	}
	if job.Status != Queued {
		switch job.Status {
		case Running, Succeeded, Failed, Canceled, Interrupted:
			delete(m.reservations, id)
			return clone(job), nil
		default:
			return Job{}, agent.ErrInvalidInput
		}
	}
	if m.closed || m.ctx.Err() != nil {
		return clone(job), ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return clone(job), err
	}
	if job.ActorID == "" {
		return clone(job), agent.ErrInvalidInput
	}
	if job.CancelRequested {
		delete(m.reservations, id)
		now := time.Now().UTC()
		job.Status, job.UpdatedAt = Canceled, now
		job.Result = &agent.Result{Operation: job.Operation, ExitCode: -1, Canceled: true, FinishedAt: now}
		if err := m.store.Save(context.WithoutCancel(ctx), job); err != nil {
			job.Status, job.Error = Failed, "cannot persist canceled task: "+err.Error()
			m.failedPersistence[id] = clone(job)
			return clone(job), err
		}
		return clone(job), nil
	}
	if err := m.executor.Validate(agent.Request{Operation: job.Operation, Input: append(json.RawMessage(nil), job.Input...)}); err != nil {
		return clone(job), err
	}
	if lease == nil && len(m.active)+len(m.reservations) >= 128 {
		return clone(job), ErrBusy
	}
	delete(m.reservations, id)
	m.enqueueLocked(job)
	return clone(job), nil
}

func (m *Manager) enqueueLocked(job Job) {
	runCtx, cancel := context.WithCancel(m.ctx)
	active := &activeJob{job: clone(job), cancel: cancel, subscribers: make(map[chan agent.Output]struct{}), done: make(chan struct{})}
	m.active[job.ID] = active
	m.wg.Add(1)
	go m.run(runCtx, active)
}
