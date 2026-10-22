package systemops

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"testing"
	"time"
)

type scheduledExecutor struct {
	release chan struct{}
	fail    bool
}

func (e *scheduledExecutor) Validate(agent.Request) error { return nil }
func (e *scheduledExecutor) Run(ctx context.Context, _ agent.Request) (agent.Result, error) {
	select {
	case <-e.release:
	case <-ctx.Done():
		return agent.Result{ExitCode: -1}, ctx.Err()
	}
	if e.fail {
		return agent.Result{ExitCode: 1}, errors.New("fixture failure")
	}
	return agent.Result{ExitCode: 0}, nil
}

func asyncScheduleFixture(t *testing.T, fail bool) (*core.Service, *Provider, core.Actor, *scheduledExecutor, Schedule) {
	t.Helper()
	service, p, a := actualProvider(t, nil)
	executor := &scheduledExecutor{release: make(chan struct{}), fail: fail}
	if err := service.RegisterExecutor([]string{"schedule_fixture"}, executor); err != nil {
		t.Fatal(err)
	}
	deps := service.Runtime("schedulefixture")
	if err := service.RegisterProvider(core.Bindings{{Action: core.Action{Name: "test_schedule_action", Description: "durable fixture", Scope: "system:read", Task: true, InputSchema: object(map[string]any{})}, Resources: hostResources, Execute: func(ctx context.Context, a core.Actor, _ json.RawMessage) (core.ActionResult, error) {
		task, err := deps.Tasks.Submit(ctx, a, "schedule_fixture", json.RawMessage(`{}`), []string{"host:local"})
		id, _ := task["id"].(string)
		return core.ActionResult{Data: task, TaskID: id, Resources: []string{"host:local"}}, err
	}}}); err != nil {
		t.Fatal(err)
	}
	schedule := action(t, service, a, "schedules_create", map[string]any{"name": "async fixture", "mode": "interval", "timezone": "UTC", "interval_seconds": 3600, "action": "test_schedule_action", "input": map[string]any{}, "enabled": true}).(Schedule)
	return service, p, a, executor, schedule
}
func TestScheduleTracksFinalTaskStatusAndOverlap(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			service, p, a, e, schedule := asyncScheduleFixture(t, fail)
			run := action(t, service, a, "schedules_run", map[string]any{"id": schedule.ID}).(ScheduleRun)
			if run.Status != "submitted" || run.TaskID == "" || !run.FinishedAt.IsZero() {
				t.Fatal(run)
			}
			if _, err := p.runSchedule(context.Background(), schedule, time.Now(), true); core.ErrorStatus(err) != 409 {
				t.Fatal("overlap was accepted", err)
			}
			close(e.release)
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				p.reconcileSchedules(context.Background())
				runs, _ := scheduleRuns(context.Background(), p.deps.State, schedule.ID)
				if len(runs) > 0 && runs[0].Status != "submitted" {
					expected := "completed"
					if fail {
						expected = "failed"
					}
					if runs[0].Status != expected || runs[0].FinishedAt.IsZero() {
						t.Fatal(runs[0])
					}
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("submitted run never acquired the final task result")
		})
	}
}
func TestScheduleMisfirePolicies(t *testing.T) {
	_, p, _, e, schedule := asyncScheduleFixture(t, false)
	defer close(e.release)
	schedule.NextRunAt = time.Now().Add(-time.Hour)
	schedule.MisfirePolicy = "skip"
	schedule.MisfireGraceSeconds = 60
	raw, _ := json.Marshal(schedule)
	if _, err := p.deps.State.CompareAndSwap(context.Background(), "schedule", schedule.ID, schedule.Revision, raw); err != nil {
		t.Fatal(err)
	}
	run, err := p.runSchedule(context.Background(), schedule, time.Now(), false)
	if err != nil || run.Status != "skipped" || run.ErrorCode != "misfire_skipped" {
		t.Fatal(run, err)
	}
	current, _ := p.loadSchedule(context.Background(), schedule.ID)
	if !current.NextRunAt.After(time.Now()) {
		t.Fatal("missed occurrence was not advanced")
	}
	current.MisfirePolicy = "run_once"
	current.NextRunAt = time.Now().Add(-time.Hour)
	raw, _ = json.Marshal(current)
	if _, err := p.deps.State.CompareAndSwap(context.Background(), "schedule", current.ID, current.Revision, raw); err != nil {
		t.Fatal(err)
	}
	run, err = p.runSchedule(context.Background(), current, time.Now(), false)
	if err != nil || run.Status != "submitted" {
		t.Fatal(run, err)
	}
}
func TestScheduleFailureNotificationIsClaimedOnce(t *testing.T) {
	host, port, pool, messages := smtpTLSFixture(t)
	service, p, a := actualProvider(t, pool)
	channel := action(t, service, a, "notifications_channels_save", map[string]any{"name": "failure notification", "kind": "smtp", "smtp": map[string]any{"host": host, "port": port, "tls_mode": "implicit", "username": "fixture-user", "from": "sender@example.test", "recipients": []string{"receiver@example.test"}}, "secret": "fixture-password"}).(Channel)
	schedule := action(t, service, a, "schedules_create", map[string]any{"name": "notification fixture", "mode": "interval", "timezone": "UTC", "interval_seconds": 3600, "action": "system_get_status", "input": map[string]any{}, "enabled": true, "failure_channel_id": channel.ID}).(Schedule)
	run := ScheduleRun{ID: "failed_fixture", ScheduleID: schedule.ID, Actor: a.Public(), Status: "failed", ErrorCode: "task_failed", StartedAt: time.Now()}
	if err := p.deps.State.Transaction(context.Background(), func(store core.MetadataStore) error { return saveScheduleRun(context.Background(), store, run) }); err != nil {
		t.Fatal(err)
	}
	p.reconcileSchedules(context.Background())
	p.reconcileSchedules(context.Background())
	select {
	case <-messages:
	case <-time.After(5 * time.Second):
		t.Fatal("failure notification was not delivered")
	}
	select {
	case <-messages:
		t.Fatal("duplicate failure notification")
	case <-time.After(100 * time.Millisecond):
	}
	runs, _ := scheduleRuns(context.Background(), p.deps.State, schedule.ID)
	if runs[0].NotificationState != "submitted" || runs[0].NotificationTaskID == "" {
		t.Fatal(runs)
	}
}

func TestScheduleReconcilesPersistedSubmissionAfterProviderRestart(t *testing.T) {
	service, p, a, e, schedule := asyncScheduleFixture(t, false)
	run := action(t, service, a, "schedules_run", map[string]any{"id": schedule.ID}).(ScheduleRun)
	p.Close()
	close(e.release)
	restarted, err := New(p.deps, Options{AllowedScheduleActions: []string{"test_schedule_action"}})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		restarted.reconcileSchedules(context.Background())
		runs, _ := scheduleRuns(context.Background(), p.deps.State, schedule.ID)
		if len(runs) > 0 && runs[0].Status == "completed" {
			if runs[0].TaskID != run.TaskID {
				t.Fatal("restart changed the linked task")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("persisted submission was not reconciled after restart")
}

func TestScheduleTracksCancellation(t *testing.T) {
	service, p, a, _, schedule := asyncScheduleFixture(t, false)
	run := action(t, service, a, "schedules_run", map[string]any{"id": schedule.ID}).(ScheduleRun)
	action(t, service, a, "tasks_cancel", map[string]any{"id": run.TaskID})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.reconcileSchedules(context.Background())
		runs, _ := scheduleRuns(context.Background(), p.deps.State, schedule.ID)
		if len(runs) > 0 && runs[0].Status == "canceled" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cancellation was not reflected in schedule history")
}
