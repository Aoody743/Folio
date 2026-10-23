package systemops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/jobs"
)

// Cleanup is always an explicitly approved, expiring snapshot. Saving a
// retention policy never schedules deletion or restores data.
type BackupTarget struct {
	ID         string `json:"id,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
	Database   string `json:"database,omitempty"`
}
type RetentionPolicy struct {
	BackupTarget
	Kind      string `json:"kind"`
	KeepCount int    `json:"keep_count"`
	KeepDays  int    `json:"keep_days"`
	Revision  int64  `json:"revision"`
}
type retainedBackup struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	SHA256    string    `json:"sha256"`
	Size      int64     `json:"size"`
}
type RetentionOutcome struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	ErrorCode string `json:"error_code,omitempty"`
}
type RetentionPlan struct {
	ID         string             `json:"plan_id"`
	Policy     RetentionPolicy    `json:"policy"`
	CreatedAt  time.Time          `json:"created_at"`
	ExpiresAt  time.Time          `json:"expires_at"`
	Candidates []retainedBackup   `json:"candidates"`
	Total      int                `json:"total"`
	Status     string             `json:"status"`
	TaskID     string             `json:"task_id,omitempty"`
	Outcomes   []RetentionOutcome `json:"outcomes"`
}
type retentionInput struct {
	BackupTarget
	PlanID           string `json:"plan_id,omitempty"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
	KeepCount        int    `json:"keep_count,omitempty"`
	KeepDays         int    `json:"keep_days,omitempty"`
}
type retentionJob struct {
	Actor  core.Actor `json:"actor"`
	PlanID string     `json:"plan_id"`
}

var retentionID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,96}$`)
var retentionDigest = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

func retentionKey(kind string, target BackupTarget) string {
	data, _ := json.Marshal([]string{kind, target.ID, target.InstanceID, target.Database})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func retentionResource(kind string, target BackupTarget) (string, error) {
	if kind == "site" && retentionID.MatchString(target.ID) && target.InstanceID == "" && target.Database == "" {
		return "site:" + target.ID, nil
	}
	if kind == "database" && target.ID == "" && retentionID.MatchString(target.InstanceID) && retentionID.MatchString(target.Database) {
		return "db:" + target.InstanceID + ":" + target.Database, nil
	}
	return "", invalid("Invalid retention resource")
}
func retentionScope(kind string) string {
	if kind == "site" {
		return "sites:backup"
	}
	return "databases:backup"
}
func (p *Provider) retentionBindings() []core.Binding {
	bindings := []core.Binding{}
	for _, spec := range []struct{ kind, prefix string }{{"site", "sites_retention_"}, {"database", "managed_database_retention_"}} {
		kind, prefix := spec.kind, spec.prefix
		base := map[string]any{}
		required := []string{"id"}
		if kind == "site" {
			base["id"] = text(96)
		} else {
			base["instance_id"] = text(96)
			base["database"] = text(96)
			required = []string{"instance_id", "database"}
		}
		for _, op := range []string{"get", "save", "plan", "apply"} {
			op := op
			fields := map[string]any{}
			for k, v := range base {
				fields[k] = v
			}
			needed := append([]string{}, required...)
			if op == "save" {
				fields["keep_count"] = integer(1, 1000)
				fields["keep_days"] = integer(1, 3650)
				fields["expected_revision"] = integer(0, 1<<30)
				needed = append(needed, "keep_count", "keep_days", "expected_revision")
			}
			if op == "apply" {
				fields["plan_id"] = text(96)
				needed = append(needed, "plan_id")
			}
			resources := func(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
				var in retentionInput
				if err := input(raw, &in); err != nil {
					return nil, err
				}
				resource, err := retentionResource(kind, in.BackupTarget)
				return []string{resource}, err
			}
			binding := p.bind(prefix+op, "Resource-bound backup retention: preserve newest count and age window; cleanup requires an explicit expiring plan", retentionScope(kind), (op == "get" || op == "plan"), object(fields, needed...), resources, func(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
				return p.retentionAction(ctx, a, kind, op, raw)
			})
			binding.Action.Task = op == "apply"
			bindings = append(bindings, binding)
		}
	}
	return bindings
}
func (p *Provider) retentionPolicy(ctx context.Context, kind string, target BackupTarget) (RetentionPolicy, error) {
	raw, rev, err := p.deps.State.GetVersioned(ctx, "retention_policy", retentionKey(kind, target))
	if core.ErrorStatus(err) == 404 {
		return RetentionPolicy{BackupTarget: target, Kind: kind, KeepCount: 7, KeepDays: 30}, nil
	}
	if err != nil {
		return RetentionPolicy{}, err
	}
	var policy RetentionPolicy
	err = json.Unmarshal(raw, &policy)
	policy.Revision = rev
	return policy, err
}
func (p *Provider) retentionAction(ctx context.Context, a core.Actor, kind, op string, raw json.RawMessage) (core.ActionResult, error) {
	var in retentionInput
	if err := input(raw, &in); err != nil {
		return core.ActionResult{}, err
	}
	resource, err := retentionResource(kind, in.BackupTarget)
	if err != nil {
		return core.ActionResult{}, err
	}
	p.retentionMu.Lock()
	defer p.retentionMu.Unlock()
	policy, err := p.retentionPolicy(ctx, kind, in.BackupTarget)
	if err != nil {
		return core.ActionResult{}, err
	}
	switch op {
	case "get":
		return core.ActionResult{Data: policy}, nil
	case "save":
		if in.KeepCount < 1 || in.KeepCount > 1000 || in.KeepDays < 1 || in.KeepDays > 3650 {
			return core.ActionResult{}, invalid("Retention must preserve at least one backup and one day")
		}
		policy.KeepCount = in.KeepCount
		policy.KeepDays = in.KeepDays
		data, _ := json.Marshal(policy)
		policy.Revision, err = p.deps.State.CompareAndSwap(ctx, "retention_policy", retentionKey(kind, in.BackupTarget), in.ExpectedRevision, data)
		return core.ActionResult{Data: policy}, err
	case "plan":
		backups, err := p.retentionBackups(ctx, a, policy)
		if err != nil {
			return core.ActionResult{}, err
		}
		now := time.Now().UTC()
		id, err := newID("cleanup_")
		if err != nil {
			return core.ActionResult{}, err
		}
		records, listErr := p.deps.State.List(ctx, "retention_plan")
		if listErr != nil {
			return core.ActionResult{}, listErr
		}
		for existingID, data := range records {
			var old RetentionPlan
			if json.Unmarshal(data, &old) == nil && old.CreatedAt.Before(now.AddDate(0, 0, -7)) && old.Status != "submitted" && old.Status != "running" {
				if err = p.deps.State.Delete(ctx, "retention_plan", existingID); err != nil {
					return core.ActionResult{}, err
				}
				delete(records, existingID)
			}
		}
		if len(records) >= 2000 {
			return core.ActionResult{}, invalid("Too many retained cleanup previews; inspect pending tasks")
		}
		plan := RetentionPlan{ID: id, Policy: policy, CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute), Candidates: retentionCandidates(backups, policy, now), Total: len(backups), Status: "ready", Outcomes: []RetentionOutcome{}}
		if len(plan.Candidates) > 100 {
			return core.ActionResult{}, invalid("More than 100 cleanup candidates; use a more conservative policy first")
		}
		data, _ := json.Marshal(plan)
		err = p.deps.State.Save(ctx, "retention_plan", id, data)
		return core.ActionResult{Data: plan}, err
	case "apply":
		plan, _, err := p.loadRetentionPlan(ctx, in.PlanID)
		if err != nil {
			return core.ActionResult{}, err
		}
		if retentionKey(kind, in.BackupTarget) != retentionKey(plan.Policy.Kind, plan.Policy.BackupTarget) || plan.Policy.Revision != policy.Revision || plan.Status != "ready" || time.Now().After(plan.ExpiresAt) {
			return core.ActionResult{}, conflict("Cleanup preview changed, expired or was already submitted")
		}
		payload, _ := json.Marshal(retentionJob{Actor: a.Public(), PlanID: plan.ID})
		task, err := p.deps.Tasks.SubmitPrepared(ctx, a, "systemops_backup_cleanup", payload, core.TaskMetadata{Resources: []string{resource}}, func(store core.MetadataStore, job jobs.Job) error {
			raw, rev, err := store.GetVersioned(ctx, "retention_plan", plan.ID)
			if err != nil {
				return err
			}
			var current RetentionPlan
			if err = json.Unmarshal(raw, &current); err != nil {
				return err
			}
			if current.Status != "ready" {
				return conflict("Cleanup already submitted")
			}
			current.Status = "submitted"
			current.TaskID = job.ID
			data, _ := json.Marshal(current)
			_, err = store.CompareAndSwap(ctx, "retention_plan", plan.ID, rev, data)
			return err
		})
		id, _ := task["id"].(string)
		return core.ActionResult{Data: task, TaskID: id, Resources: []string{resource}}, err
	}
	return core.ActionResult{}, invalid("Invalid retention action")
}
func (p *Provider) loadRetentionPlan(ctx context.Context, id string) (RetentionPlan, int64, error) {
	raw, rev, err := p.deps.State.GetVersioned(ctx, "retention_plan", id)
	if err != nil {
		return RetentionPlan{}, 0, err
	}
	var plan RetentionPlan
	err = json.Unmarshal(raw, &plan)
	return plan, rev, err
}
func (p *Provider) retentionBackups(ctx context.Context, a core.Actor, policy RetentionPolicy) ([]retainedBackup, error) {
	name := "sites_backups_list"
	payload := map[string]any{"id": policy.ID}
	if policy.Kind == "database" {
		name = "managed_database_backups_list"
		payload = map[string]any{"instance_id": policy.InstanceID, "database": policy.Database}
	}
	raw, _ := json.Marshal(payload)
	result, err := p.deps.Dispatch(ctx, a, name, raw)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	var items []json.RawMessage
	if err = json.Unmarshal(data, &items); err != nil {
		var page struct {
			Items []json.RawMessage `json:"items"`
		}
		if err = json.Unmarshal(data, &page); err != nil || page.Items == nil {
			return nil, invalid("Invalid backup listing")
		}
		items = page.Items
	}
	out := []retainedBackup{}
	seen := map[string]bool{}
	for _, item := range items {
		var value struct {
			ID        string `json:"id"`
			CreatedAt string `json:"created_at"`
			SHA256    string `json:"sha256"`
			Size      int64  `json:"size"`
			Bytes     int64  `json:"bytes"`
		}
		if err = json.Unmarshal(item, &value); err != nil {
			return nil, err
		}
		if !retentionID.MatchString(value.ID) || seen[value.ID] {
			return nil, invalid("Invalid or duplicate backup identity")
		}
		seen[value.ID] = true
		at, _ := time.Parse(time.RFC3339Nano, value.CreatedAt)
		size := value.Size
		if policy.Kind == "database" {
			size = value.Bytes
		}
		out = append(out, retainedBackup{value.ID, at, value.SHA256, size})
	}
	return out, nil
}
func retentionCandidates(backups []retainedBackup, policy RetentionPolicy, now time.Time) []retainedBackup {
	// Unknown dates/digests and future timestamps are always retained.
	sorted := append([]retainedBackup{}, backups...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
	})
	cutoff := now.AddDate(0, 0, -policy.KeepDays)
	out := []retainedBackup{}
	for index, backup := range sorted {
		if index < policy.KeepCount || backup.CreatedAt.IsZero() || !backup.CreatedAt.Before(cutoff) || !retentionDigest.MatchString(backup.SHA256) {
			continue
		}
		out = append(out, backup)
	}
	return out
}
func (p *Provider) cleanupBackups(ctx context.Context, job retentionJob) (plan RetentionPlan, err error) {
	p.retentionMu.Lock()
	defer p.retentionMu.Unlock()
	plan, _, err = p.loadRetentionPlan(ctx, job.PlanID)
	if err != nil {
		return plan, err
	}
	if plan.Status != "submitted" {
		return plan, conflict("Cleanup is not in submitted state")
	}
	plan.Status = "running"
	persist := func(storeCtx context.Context) error {
		data, _ := json.Marshal(plan)
		return p.deps.State.Save(storeCtx, "retention_plan", plan.ID, data)
	}
	defer func() {
		storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err != nil {
			plan.Status = "interrupted"
		} else {
			plan.Status = "completed"
		}
		if saveErr := persist(storeCtx); err == nil {
			err = saveErr
		}
	}()
	for _, candidate := range plan.Candidates {
		if time.Now().After(plan.ExpiresAt) {
			return plan, conflict("Cleanup preview expired before completion")
		}
		if err = ctx.Err(); err != nil {
			return plan, err
		}
		var actor core.Actor
		actor, err = p.deps.ResolveActor(ctx, job.Actor)
		if err != nil {
			return plan, err
		}
		resource, e := retentionResource(plan.Policy.Kind, plan.Policy.BackupTarget)
		if e != nil {
			return plan, e
		}
		if !p.deps.Authorize(actor, retentionScope(plan.Policy.Kind), resource) {
			return plan, denied()
		}
		var current RetentionPolicy
		current, err = p.retentionPolicy(ctx, plan.Policy.Kind, plan.Policy.BackupTarget)
		if err != nil {
			return plan, err
		}
		if current.Revision != plan.Policy.Revision {
			return plan, conflict("Retention policy changed")
		}
		var backups []retainedBackup
		backups, err = p.retentionBackups(ctx, actor, current)
		if err != nil {
			return plan, err
		}
		eligible := false
		for _, backup := range retentionCandidates(backups, current, plan.CreatedAt) {
			if backup == candidate {
				eligible = true
				break
			}
		}
		if !eligible {
			plan.Outcomes = append(plan.Outcomes, RetentionOutcome{ID: candidate.ID, Status: "skipped", ErrorCode: "changed_or_retained"})
			if err = persist(ctx); err != nil {
				return plan, err
			}
			continue
		}
		plan.Outcomes = append(plan.Outcomes, RetentionOutcome{ID: candidate.ID, Status: "outcome_unknown"})
		if err = persist(ctx); err != nil {
			return plan, err
		}
		name := "sites_backup_delete"
		payload := map[string]any{"id": plan.Policy.ID, "backup_id": candidate.ID}
		if plan.Policy.Kind == "database" {
			name = "managed_database_backup_delete"
			payload = map[string]any{"instance_id": plan.Policy.InstanceID, "database": plan.Policy.Database, "backup_id": candidate.ID}
		}
		raw, _ := json.Marshal(payload)
		_, deleteErr := p.deps.Dispatch(ctx, actor, name, raw)
		outcome := &plan.Outcomes[len(plan.Outcomes)-1]
		if deleteErr != nil {
			outcome.Status = "failed"
			outcome.ErrorCode = core.ErrorCode(deleteErr)
		} else {
			outcome.Status = "completed"
		}
		if err = persist(ctx); err != nil {
			return plan, err
		}
		if deleteErr != nil {
			return plan, deleteErr
		}
	}
	return plan, nil
}
func (p *Provider) runBackupCleanup(ctx context.Context, request agent.Request) (agent.Result, error) {
	result := agent.Result{Operation: request.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	var job retentionJob
	_ = json.Unmarshal(request.Input, &job)
	plan, err := p.cleanupBackups(ctx, job)
	result.FinishedAt = time.Now().UTC()
	if err == nil {
		result.ExitCode = 0
	}
	data, _ := json.Marshal(plan)
	result.Stdout = string(data)
	if err != nil {
		result.Stderr = fmt.Sprintf("Cleanup stopped: %s", core.ErrorCode(err))
	}
	return result, err
}
