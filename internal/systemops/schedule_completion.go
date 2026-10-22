package systemops

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func scheduleTaskID(result any) string {
	object, ok := result.(map[string]any)
	if !ok {
		return ""
	}
	if id, ok := object["task_id"].(string); ok && id != "" {
		return id
	}
	if task, ok := object["task"].(map[string]any); ok {
		return scheduleTaskID(task)
	}
	status, _ := object["status"].(string)
	switch status {
	case "queued", "running", "succeeded", "failed", "canceled", "interrupted":
		id, _ := object["id"].(string)
		return id
	}
	return ""
}

// Poll with the recorded initiating identity, never a synthetic administrator.
// This also reconciles task receipts after an API restart.
func (p *Provider) reconcileSchedules(ctx context.Context) {
	p.scheduleMu.Lock()
	defer p.scheduleMu.Unlock()
	records, err := p.deps.State.List(ctx, "schedule")
	if err != nil {
		return
	}
	for id := range records {
		if ctx.Err() != nil {
			return
		}
		schedule, err := p.loadSchedule(ctx, id)
		if err != nil {
			continue
		}
		runs, err := scheduleRuns(ctx, p.deps.State, id)
		if err != nil {
			continue
		}
		for _, run := range runs {
			if run.Status != "submitted" {
				switch run.Status {
				case "failed", "canceled", "interrupted", "observation_denied":
					p.notifyScheduleFailure(ctx, schedule, &run)
				}
				continue
			}
			if run.TaskID == "" {
				run.TaskID = scheduleTaskID(run.Result)
			}
			if run.Actor.ID == "" {
				run.Actor = schedule.CreatedBy
			}
			actor, resolveErr := p.deps.ResolveActor(ctx, run.Actor)
			if resolveErr != nil {
				run.Status = "observation_denied"
				run.ErrorCode = "identity_revoked"
			} else if run.TaskID == "" {
				run.Status = "interrupted"
				run.ErrorCode = "task_receipt_missing"
			} else {
				task, getErr := p.deps.Tasks.Get(ctx, actor, run.TaskID)
				if getErr != nil {
					if core.ErrorStatus(getErr) != 401 && core.ErrorStatus(getErr) != 403 && core.ErrorStatus(getErr) != 404 {
						continue
					}
					run.Status = "observation_denied"
					run.ErrorCode = core.ErrorCode(getErr)
				} else {
					status, _ := task["status"].(string)
					switch status {
					case "succeeded":
						run.Status = "completed"
						run.ErrorCode = ""
					case "failed", "canceled", "interrupted":
						run.Status = status
						run.ErrorCode = "task_" + status
					default:
						continue
					}
				}
			}
			run.FinishedAt = time.Now().UTC()
			if err = p.deps.State.Transaction(ctx, func(store core.MetadataStore) error { return saveScheduleRun(ctx, store, run) }); err != nil {
				continue
			}
			p.recordScheduleOutcome(ctx, run)
			if run.Status != "completed" {
				p.notifyScheduleFailure(ctx, schedule, &run)
			}
		}
	}
}

func (p *Provider) notifyScheduleFailure(ctx context.Context, schedule Schedule, run *ScheduleRun) {
	if schedule.FailureChannelID == "" || run.NotificationState != "" {
		return
	}
	actor, err := p.deps.ResolveActor(ctx, run.Actor)
	if err != nil {
		return
	}
	// Persist the claim before sending. A restart never silently sends duplicates;
	// an interrupted delivery claim remains visible for operator inspection.
	run.NotificationState = "sending"
	if p.deps.State.Transaction(ctx, func(store core.MetadataStore) error { return saveScheduleRun(ctx, store, *run) }) != nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{"channel_id": schedule.FailureChannelID, "subject": "Folio scheduled task failed", "content": "Schedule: " + schedule.ID + "\nRun: " + run.ID + "\nTask: " + run.TaskID + "\nOutcome: " + run.Status + "\nCode: " + run.ErrorCode})
	result, err := p.deps.Dispatch(ctx, actor, "notifications_send", payload)
	if err != nil {
		run.NotificationState = "failed"
	} else {
		run.NotificationState = "submitted"
		run.NotificationTaskID = scheduleTaskID(result)
	}
	_ = p.deps.State.Transaction(ctx, func(store core.MetadataStore) error { return saveScheduleRun(ctx, store, *run) })
}

// Metadata updates use the latest revision so an in-flight completion never
// overwrites an operator's newer schedule configuration.
func (p *Provider) recordScheduleOutcome(ctx context.Context, run ScheduleRun) {
	current, err := p.loadSchedule(ctx, run.ScheduleID)
	if err != nil {
		return
	}
	if current.LastRunAt != nil && current.LastRunAt.After(run.StartedAt) {
		return
	}
	current.LastRunAt = &run.StartedAt
	current.LastRunStatus = run.Status
	current.LastTaskID = run.TaskID
	current.LastError = run.ErrorCode
	raw, _ := json.Marshal(current)
	_, _ = p.deps.State.CompareAndSwap(ctx, "schedule", current.ID, current.Revision, raw)
}
