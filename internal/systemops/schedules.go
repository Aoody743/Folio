package systemops

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

type Schedule struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Mode            string          `json:"mode"`
	Cron            string          `json:"cron,omitempty"`
	Timezone        string          `json:"timezone"`
	IntervalSeconds int             `json:"interval_seconds,omitempty"`
	RunAt           string          `json:"run_at,omitempty"`
	Action          string          `json:"action"`
	Input           json.RawMessage `json:"input"`
	Enabled         bool            `json:"enabled"`
	NextRunAt       time.Time       `json:"next_run_at"`
	CreatedBy       core.Actor      `json:"created_by"`
	Revision        int64           `json:"revision"`
	LastRunAt       *time.Time      `json:"last_run_at,omitempty"`
	LastError       string          `json:"last_error,omitempty"`
}
type scheduleInput struct {
	ID               string          `json:"id,omitempty"`
	ExpectedRevision int64           `json:"expected_revision,omitempty"`
	Name             string          `json:"name"`
	Mode             string          `json:"mode"`
	Cron             string          `json:"cron,omitempty"`
	Timezone         string          `json:"timezone"`
	IntervalSeconds  int             `json:"interval_seconds,omitempty"`
	RunAt            string          `json:"run_at,omitempty"`
	Action           string          `json:"action"`
	Input            json.RawMessage `json:"input"`
	Enabled          bool            `json:"enabled"`
}
type ScheduleRun struct {
	ID         string    `json:"id"`
	ScheduleID string    `json:"schedule_id"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Status     string    `json:"status"`
	ErrorCode  string    `json:"error_code,omitempty"`
	Result     any       `json:"result,omitempty"`
}

func scheduleFields() map[string]any {
	return map[string]any{"id": text(96), "expected_revision": integer(1, 1<<30), "name": text(160), "mode": enum("interval", "cron", "once"), "cron": text(128), "timezone": text(128), "interval_seconds": integer(15, 365*86400), "run_at": text(64), "action": text(96), "input": map[string]any{"type": "object", "additionalProperties": true}, "enabled": boolean()}
}
func (p *Provider) scheduleResources(ctx context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var input struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, err
	}
	if input.ID == "" {
		return []string{"host:local"}, nil
	}
	return []string{"schedule:" + input.ID}, nil
}
func (p *Provider) scheduleBindings() []core.Binding {
	createFields := scheduleFields()
	delete(createFields, "id")
	delete(createFields, "expected_revision")
	bindings := []core.Binding{
		p.bind("schedules_list", "List actual persisted schedules authorized to this identity", "schedules:read", true, object(map[string]any{}), func(context.Context, core.Actor, json.RawMessage) ([]string, error) { return nil, nil }, p.listSchedules),
		p.bind("schedules_create", "Create a durable named-action schedule without modifying root cron", "schedules:write", false, object(createFields, "name", "mode", "timezone", "action", "input", "enabled"), p.scheduleResources, p.saveSchedule),
		p.bind("schedules_update", "Update a schedule using its expected revision", "schedules:write", false, object(scheduleFields(), "id", "expected_revision", "name", "mode", "timezone", "action", "input", "enabled"), p.scheduleResources, p.saveSchedule),
	}
	for _, spec := range []struct {
		name, scope string
		read        bool
	}{{"schedules_delete", "schedules:write", false}, {"schedules_run", "schedules:write", false}, {"schedules_history", "schedules:read", true}} {
		bindings = append(bindings, p.bind(spec.name, "Operate on a real schedule and its execution records", spec.scope, spec.read, object(map[string]any{"id": text(96), "expected_revision": integer(1, 1<<30)}, "id"), p.scheduleResources, func(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			var in struct {
				ID               string `json:"id"`
				ExpectedRevision int64  `json:"expected_revision"`
			}
			if err := input(raw, &in); err != nil {
				return core.ActionResult{}, err
			}
			schedule, err := p.loadSchedule(ctx, in.ID)
			if err != nil {
				return core.ActionResult{}, err
			}
			switch spec.name {
			case "schedules_delete":
				if in.ExpectedRevision != schedule.Revision {
					return core.ActionResult{}, conflict("Schedule revision changed")
				}
				err = p.deps.State.Transaction(ctx, func(store core.MetadataStore) error {
					_, rev, err := store.GetVersioned(ctx, "schedule", in.ID)
					if err != nil {
						return err
					}
					if rev != in.ExpectedRevision {
						return conflict("Schedule revision changed")
					}
					if err = store.Delete(ctx, "schedule", in.ID); err != nil {
						return err
					}
					return store.Delete(ctx, "schedule_runs", in.ID)
				})
				return core.ActionResult{Data: map[string]any{"deleted": err == nil}}, err
			case "schedules_run":
				run, err := p.runScheduleAs(ctx, schedule, time.Now().UTC(), true, &a)
				return core.ActionResult{Data: run}, err
			default:
				out, err := scheduleRuns(ctx, p.deps.State, in.ID)
				if err != nil {
					return core.ActionResult{}, err
				}
				sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
				if len(out) > 100 {
					out = out[:100]
				}
				return core.ActionResult{Data: out}, nil
			}
		}))
	}
	return bindings
}
func (p *Provider) loadSchedule(ctx context.Context, id string) (Schedule, error) {
	raw, revision, err := p.deps.State.GetVersioned(ctx, "schedule", id)
	if err != nil {
		return Schedule{}, err
	}
	var out Schedule
	if err = json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	out.Revision = revision
	return out, nil
}
func (p *Provider) listSchedules(ctx context.Context, a core.Actor, _ json.RawMessage) (core.ActionResult, error) {
	records, err := p.deps.State.List(ctx, "schedule")
	if err != nil {
		return core.ActionResult{}, err
	}
	out := []Schedule{}
	for id := range records {
		if !p.deps.Authorize(a, "schedules:read", "schedule:"+id) {
			continue
		}
		schedule, err := p.loadSchedule(ctx, id)
		if err != nil {
			return core.ActionResult{}, err
		}
		out = append(out, schedule)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return core.ActionResult{Data: out}, nil
}
func (p *Provider) saveSchedule(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in scheduleInput
	if err := input(raw, &in); err != nil {
		return core.ActionResult{}, err
	}
	if in.Name == "" || !p.scheduleTargets[in.Action] {
		return core.ActionResult{}, invalid("Schedule requires a name and trusted allowed action")
	}
	if len(in.Input) == 0 || !json.Valid(in.Input) || in.Input[0] != '{' {
		return core.ActionResult{}, invalid("Schedule action input must be an object")
	}
	location, err := time.LoadLocation(in.Timezone)
	if err != nil {
		return core.ActionResult{}, invalid("Unknown IANA timezone")
	}
	now := time.Now().UTC()
	schedule := Schedule{ID: in.ID, Name: in.Name, Mode: in.Mode, Cron: in.Cron, Timezone: in.Timezone, IntervalSeconds: in.IntervalSeconds, RunAt: in.RunAt, Action: in.Action, Input: in.Input, Enabled: in.Enabled, CreatedBy: a.Public()}
	switch in.Mode {
	case "interval":
		if in.IntervalSeconds < 15 || in.IntervalSeconds > 365*86400 || in.Cron != "" || in.RunAt != "" {
			return core.ActionResult{}, invalid("Invalid interval schedule")
		}
		schedule.NextRunAt = now.Add(time.Duration(in.IntervalSeconds) * time.Second)
	case "cron":
		if in.IntervalSeconds != 0 || in.RunAt != "" {
			return core.ActionResult{}, invalid("Mixed schedule modes")
		}
		cron, err := parseCron(in.Cron)
		if err != nil {
			return core.ActionResult{}, invalid("Invalid five-field cron expression")
		}
		schedule.NextRunAt, err = cron.next(now, location)
		if err != nil {
			return core.ActionResult{}, err
		}
	case "once":
		if in.Cron != "" || in.IntervalSeconds != 0 {
			return core.ActionResult{}, invalid("Mixed schedule modes")
		}
		schedule.NextRunAt, err = time.Parse(time.RFC3339, in.RunAt)
		if err != nil || !schedule.NextRunAt.After(now) {
			return core.ActionResult{}, invalid("One-time execution must be a future RFC3339 timestamp")
		}
	default:
		return core.ActionResult{}, invalid("Invalid schedule mode")
	}
	if in.ID == "" {
		schedule.ID, err = newID("sch_")
		if err != nil {
			return core.ActionResult{}, err
		}
		if !p.deps.Authorize(a, "schedules:write", "schedule:"+schedule.ID) {
			return core.ActionResult{}, denied()
		}
		if in.ExpectedRevision != 0 {
			return core.ActionResult{}, invalid("New schedules have no expected revision")
		}
	} else {
		old, err := p.loadSchedule(ctx, in.ID)
		if err != nil {
			return core.ActionResult{}, err
		}
		if old.Revision != in.ExpectedRevision {
			return core.ActionResult{}, conflict("Schedule revision changed")
		}
		if old.CreatedBy.ID != a.ID {
			full := false
			for _, scope := range a.Scopes {
				if scope == "*" {
					full = true
				}
			}
			if !full {
				return core.ActionResult{}, denied()
			}
		} else {
			schedule.CreatedBy = old.CreatedBy
		}
		schedule.LastRunAt = old.LastRunAt
	}
	schedule.Revision = in.ExpectedRevision + 1
	value, _ := json.Marshal(schedule)
	var revision int64
	err = p.deps.State.Transaction(ctx, func(store core.MetadataStore) error {
		if in.ID == "" {
			records, err := store.List(ctx, "schedule")
			if err != nil {
				return err
			}
			if len(records) >= 256 {
				return invalid("Schedule limit (256) reached")
			}
		}
		var swapErr error
		revision, swapErr = store.CompareAndSwap(ctx, "schedule", schedule.ID, in.ExpectedRevision, value)
		return swapErr
	})
	schedule.Revision = revision
	resource := "schedule:" + schedule.ID
	if in.ID == "" {
		resource = "host:local"
	}
	return core.ActionResult{Data: schedule, Resources: []string{resource}, AuditSummary: map[string]any{"id": schedule.ID, "action": schedule.Action}}, err
}
func (p *Provider) tickSchedules(ctx context.Context, now time.Time) {
	records, err := p.deps.State.List(ctx, "schedule")
	if err != nil {
		return
	}
	count := 0
	for id := range records {
		if ctx.Err() != nil {
			return
		}
		schedule, err := p.loadSchedule(ctx, id)
		if err != nil || !schedule.Enabled || schedule.NextRunAt.After(now) {
			continue
		}
		_, _ = p.runSchedule(ctx, schedule, now, false)
		count++
		if count >= 4 {
			return
		}
	}
}
func (p *Provider) runSchedule(ctx context.Context, schedule Schedule, now time.Time, manual bool) (ScheduleRun, error) {
	return p.runScheduleAs(ctx, schedule, now, manual, nil)
}
func (p *Provider) runScheduleAs(ctx context.Context, schedule Schedule, now time.Time, manual bool, caller *core.Actor) (ScheduleRun, error) {
	identity := schedule.CreatedBy
	if caller != nil {
		identity = *caller
	}
	actor, err := p.deps.ResolveActor(ctx, identity)
	if err != nil {
		if !manual {
			schedule.Enabled = false
			schedule.LastError = "Scheduling identity expired or was revoked; reauthorize before enabling"
			raw, _ := json.Marshal(schedule)
			_, _ = p.deps.State.CompareAndSwap(ctx, "schedule", schedule.ID, schedule.Revision, raw)
		}
		return ScheduleRun{}, err
	}
	if !p.deps.Authorize(actor, "schedules:write", "schedule:"+schedule.ID) {
		return ScheduleRun{}, denied()
	}
	id, err := newID("run_")
	if err != nil {
		return ScheduleRun{}, err
	}
	run := ScheduleRun{ID: id, ScheduleID: schedule.ID, StartedAt: now, Status: "running"}
	if !manual {
		schedule.LastRunAt = &now
		switch schedule.Mode {
		case "once":
			schedule.Enabled = false
		case "interval":
			schedule.NextRunAt = now.Add(time.Duration(schedule.IntervalSeconds) * time.Second)
		case "cron":
			cron, err := parseCron(schedule.Cron)
			if err != nil {
				return ScheduleRun{}, err
			}
			location, err := time.LoadLocation(schedule.Timezone)
			if err != nil {
				return ScheduleRun{}, err
			}
			schedule.NextRunAt, err = cron.next(now, location)
			if err != nil {
				return ScheduleRun{}, err
			}
		}
	}
	// The automatic claim and running receipt share one SQLite transaction.
	// A crash cannot silently advance a schedule without recording its attempt.
	err = p.deps.State.Transaction(ctx, func(store core.MetadataStore) error {
		if !manual {
			raw, _ := json.Marshal(schedule)
			if _, err := store.CompareAndSwap(ctx, "schedule", schedule.ID, schedule.Revision, raw); err != nil {
				return err
			}
		}
		return saveScheduleRun(ctx, store, run)
	})
	if err != nil {
		return run, err
	}
	result, err := p.deps.Dispatch(ctx, actor, schedule.Action, schedule.Input)
	run.FinishedAt = time.Now().UTC()
	if err != nil {
		run.Status = "failed"
		run.ErrorCode = core.ErrorCode(err)
	} else {
		run.Status = "completed"
		if object, ok := result.(map[string]any); ok && (object["task_id"] != nil || object["operation_id"] != nil) {
			run.Status = "submitted"
		}
		run.Result = result
		bounded, _ := json.Marshal(result)
		if len(bounded) > 4<<10 {
			run.Result = map[string]any{"summary": "Result exceeds history limit; inspect the action task/artifact"}
		}
	}
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if storeErr := p.deps.State.Transaction(storeCtx, func(store core.MetadataStore) error { return saveScheduleRun(storeCtx, store, run) }); storeErr != nil {
		return run, storeErr
	}
	return run, err
}

func scheduleRuns(ctx context.Context, store core.MetadataStore, id string) ([]ScheduleRun, error) {
	value, err := store.Get(ctx, "schedule_runs", id)
	if core.ErrorStatus(err) == 404 {
		return []ScheduleRun{}, nil
	}
	if err != nil {
		return nil, err
	}
	var runs []ScheduleRun
	err = json.Unmarshal(value, &runs)
	return runs, err
}
func saveScheduleRun(ctx context.Context, store core.MetadataStore, run ScheduleRun) error {
	runs, err := scheduleRuns(ctx, store, run.ScheduleID)
	if err != nil {
		return err
	}
	found := false
	for i := range runs {
		if runs[i].ID == run.ID {
			runs[i] = run
			found = true
			break
		}
	}
	if !found {
		runs = append(runs, run)
	}
	if len(runs) > 100 {
		runs = runs[len(runs)-100:]
	}
	value, _ := json.Marshal(runs)
	return store.Save(ctx, "schedule_runs", run.ScheduleID, value)
}

func (p *Provider) recoverScheduleRuns(ctx context.Context) error {
	records, err := p.deps.State.List(ctx, "schedule")
	if err != nil {
		return err
	}
	for id := range records {
		runs, err := scheduleRuns(ctx, p.deps.State, id)
		if err != nil {
			return err
		}
		changed := false
		for i := range runs {
			if runs[i].Status == "running" {
				runs[i].Status = "interrupted"
				runs[i].ErrorCode = "server_restarted"
				runs[i].FinishedAt = time.Now().UTC()
				changed = true
			}
		}
		if changed {
			value, _ := json.Marshal(runs)
			if err = p.deps.State.Save(ctx, "schedule_runs", id, value); err != nil {
				return err
			}
		}
	}
	return nil
}

type cronSpec struct {
	fields [5]map[int]bool
	wild   [5]bool
}

func parseCron(value string) (cronSpec, error) {
	parts := strings.Fields(value)
	if len(parts) != 5 {
		return cronSpec{}, errors.New("five fields required")
	}
	bounds := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	var out cronSpec
	for i, part := range parts {
		out.fields[i] = map[int]bool{}
		out.wild[i] = part == "*"
		for _, item := range strings.Split(part, ",") {
			term, stepText, stepped := strings.Cut(item, "/")
			step := 1
			if stepped {
				var err error
				step, err = strconv.Atoi(stepText)
				if err != nil || step < 1 || step > bounds[i][1]+1 {
					return out, errors.New("invalid step")
				}
			}
			start, end := bounds[i][0], bounds[i][1]
			if term != "*" {
				left, right, ranged := strings.Cut(term, "-")
				var err error
				start, err = strconv.Atoi(left)
				if err != nil {
					return out, err
				}
				end = start
				if ranged {
					end, err = strconv.Atoi(right)
					if err != nil {
						return out, err
					}
				}
			}
			if start < bounds[i][0] || end > bounds[i][1] || start > end {
				return out, errors.New("field outside bounds")
			}
			for n := start; n <= end; n += step {
				out.fields[i][n] = true
			}
		}
	}
	return out, nil
}
func (c cronSpec) matches(t time.Time) bool {
	if !c.fields[0][t.Minute()] || !c.fields[1][t.Hour()] || !c.fields[3][int(t.Month())] {
		return false
	}
	day := c.fields[2][t.Day()]
	week := c.fields[4][int(t.Weekday())] || (t.Weekday() == time.Sunday && c.fields[4][7])
	if !c.wild[2] && !c.wild[4] {
		return day || week
	}
	return day && week
}
func (c cronSpec) next(after time.Time, location *time.Location) (time.Time, error) {
	candidate := after.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := candidate.AddDate(5, 0, 0)
	for candidate.Before(limit) {
		if c.matches(candidate.In(location)) {
			return candidate, nil
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}, errors.New("cron has no execution within five years")
}
