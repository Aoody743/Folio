package core

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Aoody743/PowerLinux/internal/jobs"
	"github.com/ncruces/go-sqlite3"
)

const preparedEnqueueBudget = 2 * time.Second
const preparedEnqueueAttempts = 6

func (t taskSubmitter) SubmitPrepared(ctx context.Context, prior Actor, operation string, input json.RawMessage, requested TaskMetadata, prepare func(MetadataStore, jobs.Job) error) (map[string]any, error) {
	if prepare == nil || !validNamespace.MatchString(t.namespace) {
		return nil, invalid("Prepared tasks require a namespace-bound metadata callback")
	}
	a, meta, err := t.authorization(ctx, prior, requested.Resources)
	if err != nil {
		return nil, err
	}
	// A queued operation must retain every source/destination declared by its
	// originating action, including resources the provider did not repeat.
	source := ctx.Value(authorizedActionKey{}).(authorizedAction)
	meta.Resources = append([]string(nil), source.Resources...)
	reservation, err := t.s.jobs.Prepare(ctx, a.ID, operation, input)
	if errors.Is(err, jobs.ErrBusy) {
		return nil, &Error{Code: "busy", Message: "Task queue is full", Status: 429}
	}
	if err != nil {
		return nil, &Error{Code: "task_rejected", Message: "Task could not be prepared", Status: 400}
	}
	job := reservation.Job()
	committed := false
	defer func() {
		if !committed {
			_ = t.s.jobs.Abort(reservation)
		}
	}()
	tx, err := t.s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, internal()
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO tasks(id,actor,resource,state,payload,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, job.ID, job.ActorID, meta.Resources[0], job.Status, jsonText(job), job.CreatedAt.UTC().Format(time.RFC3339Nano), job.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO task_metadata(id,actor,resources,action) VALUES(?,?,?,?)`, job.ID, jsonText(meta.Actor), jsonText(meta.Resources), jsonText(map[string]any{"name": meta.Action, "scope": meta.Scope}))
	}
	if err == nil {
		if id, ok := ctx.Value(idempotencyContextKey{}).(string); ok && id != "" {
			_, err = tx.ExecContext(ctx, `UPDATE idempotency SET task_id=? WHERE id=? AND state='running'`, job.ID, id)
		}
	}
	if err != nil {
		return nil, internal()
	}
	if err = prepare(stateStore{s: t.s, namespace: t.namespace, tx: tx}, reservation.Job()); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		// A commit error is not proof that no durable row exists. Do not abort a
		// committed reservation or replay an unknown mutation on that assumption.
		_ = tx.Rollback()
		checkCtx, cancel := context.WithTimeout(context.WithoutCancel(t.s.lifetime), 2*time.Second)
		stored, checkErr := (sqliteJobStore{s: t.s}).Get(checkCtx, job.ID)
		cancel()
		if errors.Is(checkErr, jobs.ErrNotFound) {
			return nil, internal()
		}
		committed = true
		if checkErr != nil {
			return nil, &Error{Code: "operation_recovery_required", Message: "Task commit could not be confirmed; inspect its durable ID before retrying", Status: 409, Details: map[string]any{"task_id": job.ID, "state": "commit_uncertain"}}
		}
		job = stored
	} else {
		committed = true
	}
	t.s.publish(Event{Type: "tasks_changed", Data: map[string]any{"id": job.ID, "status": job.Status}}, "tasks:read", meta.Resources[0])
	// Only the service lifetime controls post-commit enqueue. A disconnected
	// HTTP/MCP caller cannot undo the committed metadata or orphan its task.
	enqueueCtx, cancelEnqueue := context.WithTimeout(t.s.lifetime, preparedEnqueueBudget)
	queued, enqueueErr := retryPreparedEnqueue(enqueueCtx, func(ctx context.Context) (jobs.Job, error) {
		return t.s.jobs.EnsureQueued(ctx, job.ID, reservation)
	})
	cancelEnqueue()
	viewJob := job
	if queued.ID == job.ID {
		viewJob = queued
	}
	view := resolvedJobView(viewJob, meta.Actor.Public(), meta.Resources)
	if enqueueErr != nil {
		view["pending"] = true
		view["recovery_required"] = true
		view["recovery_state"] = viewJob.Status
		return view, nil
	}
	return resolvedJobView(queued, meta.Actor.Public(), meta.Resources), nil
}

// retryPreparedEnqueue retries only known transient admission failures. It
// keeps the same committed task identity and reservation; it never prepares,
// validates a new identity, or reruns the task itself. The shared context
// bounds every database call and all backoff to the service lifetime.
func retryPreparedEnqueue(ctx context.Context, ensure func(context.Context) (jobs.Job, error)) (jobs.Job, error) {
	delay := 25 * time.Millisecond
	var job jobs.Job
	var err error
	for attempt := 0; attempt < preparedEnqueueAttempts; attempt++ {
		if err = ctx.Err(); err != nil {
			return job, err
		}
		job, err = ensure(ctx)
		if err == nil {
			return job, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return job, ctxErr
		}
		if !retryablePreparedEnqueue(err) || attempt+1 == preparedEnqueueAttempts {
			return job, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return job, ctx.Err()
		case <-timer.C:
		}
		if delay < 400*time.Millisecond {
			delay *= 2
		}
	}
	return job, err
}

func retryablePreparedEnqueue(err error) bool {
	return errors.Is(err, jobs.ErrBusy) || errors.Is(err, sqlite3.BUSY) || errors.Is(err, sqlite3.LOCKED)
}
