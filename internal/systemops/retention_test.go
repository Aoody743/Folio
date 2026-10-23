package systemops

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestRetentionProtectsNewestRecentAndUnknownBackups(t *testing.T) {
	now := time.Now().UTC()
	digest := strings.Repeat("a", 64)
	backups := []retainedBackup{}
	for i := 0; i < 12; i++ {
		backups = append(backups, retainedBackup{ID: string(rune('a' + i)), CreatedAt: now.AddDate(0, 0, -i*3), SHA256: digest})
	}
	backups = append(backups, retainedBackup{ID: "unknown", SHA256: digest}, retainedBackup{ID: "no-hash", CreatedAt: now.AddDate(0, 0, -100)})
	policy := RetentionPolicy{KeepCount: 3, KeepDays: 20}
	candidates := retentionCandidates(backups, policy, now)
	if len(candidates) != 5 {
		t.Fatal(candidates)
	}
	for _, candidate := range candidates {
		if !candidate.CreatedAt.Before(now.AddDate(0, 0, -20)) || candidate.ID == "unknown" || candidate.ID == "no-hash" {
			t.Fatal(candidate)
		}
	}
	if _, err := retentionResource("site", BackupTarget{ID: "../escape"}); err == nil {
		t.Fatal("unsafe site identity accepted")
	}
	if _, err := retentionResource("database", BackupTarget{ID: "extra", InstanceID: "db-main", Database: "owned"}); err == nil {
		t.Fatal("ambiguous target accepted")
	}
}
func TestRetentionUsesExpiringResourceBoundPlansAndActualTaskReceipts(t *testing.T) {
	service, p, admin := actualProvider(t, nil)
	p.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	digest := strings.Repeat("a", 64)
	var mu sync.Mutex
	backups := map[string]retainedBackup{"newest": {ID: "newest", CreatedAt: now, SHA256: digest}, "old-a": {ID: "old-a", CreatedAt: now.AddDate(0, 0, -90), SHA256: digest}, "old-b": {ID: "old-b", CreatedAt: now.AddDate(0, 0, -100), SHA256: digest}}
	deleted := []string{}
	p.deps.Dispatch = func(_ context.Context, _ core.Actor, name string, raw json.RawMessage) (any, error) {
		var in map[string]string
		_ = json.Unmarshal(raw, &in)
		if in["id"] != "fixture" {
			t.Errorf("wrong resource: %v", in)
		}
		mu.Lock()
		defer mu.Unlock()
		if name == "sites_backups_list" {
			out := []retainedBackup{}
			for _, backup := range backups {
				out = append(out, backup)
			}
			return out, nil
		}
		if name != "sites_backup_delete" {
			t.Errorf("unexpected nested action: %s", name)
		}
		deleted = append(deleted, in["backup_id"])
		delete(backups, in["backup_id"])
		return map[string]any{"deleted": true}, nil
	}
	values := map[string]any{"id": "fixture", "keep_count": 1, "keep_days": 30, "expected_revision": 0}
	policy := action(t, service, admin, "sites_retention_save", values).(RetentionPolicy)
	plan := action(t, service, admin, "sites_retention_plan", map[string]any{"id": "fixture"}).(RetentionPlan)
	if len(plan.Candidates) != 2 || plan.Policy.Revision != policy.Revision {
		t.Fatal(plan)
	}
	mu.Lock()
	if len(deleted) != 0 {
		t.Fatal("preview deleted data")
	}
	changed := backups["old-a"]
	changed.SHA256 = strings.Repeat("b", 64)
	backups["old-a"] = changed
	mu.Unlock()
	raw, _ := json.Marshal(map[string]any{"id": "other", "plan_id": plan.ID})
	if _, err := service.Execute(ctx, admin, "sites_retention_apply", raw); core.ErrorStatus(err) != 409 {
		t.Fatal("cross-resource preview used", err)
	}
	task := action(t, service, admin, "sites_retention_apply", map[string]any{"id": "fixture", "plan_id": plan.ID}).(map[string]any)
	taskID, _ := task["id"].(string)
	if taskID == "" {
		t.Fatal(task)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		job, err := p.deps.Tasks.Get(ctx, admin, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if job["status"] == "succeeded" {
			if job["action"] != "sites_retention_apply" {
				t.Fatal("task lost initiating action", job)
			}
			break
		}
		if job["status"] == "failed" || time.Now().After(deadline) {
			t.Fatal(job)
		}
		time.Sleep(10 * time.Millisecond)
	}
	listed := action(t, service, admin, "tasks_list", map[string]any{}).([]map[string]any)
	if len(listed) == 0 || listed[0]["action"] != "sites_retention_apply" {
		t.Fatal("list omitted initiating action", listed)
	}
	finished, _, err := p.loadRetentionPlan(ctx, plan.ID)
	if err != nil || finished.Status != "completed" || len(finished.Outcomes) != 2 {
		t.Fatal(finished, err)
	}
	mu.Lock()
	if len(deleted) != 1 || deleted[0] != "old-b" || len(backups) != 2 {
		t.Fatal("changed backup was deleted", deleted, backups)
	}
	mu.Unlock()
	raw, _ = json.Marshal(map[string]any{"id": "fixture", "plan_id": plan.ID})
	if _, err = service.Execute(ctx, admin, "sites_retention_apply", raw); core.ErrorStatus(err) != 409 {
		t.Fatal("plan replay accepted", err)
	}
	expired := action(t, service, admin, "sites_retention_plan", map[string]any{"id": "fixture"}).(RetentionPlan)
	expired.ExpiresAt = now.Add(-time.Second)
	data, _ := json.Marshal(expired)
	if err = p.deps.State.Save(ctx, "retention_plan", expired.ID, data); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(map[string]any{"id": "fixture", "plan_id": expired.ID})
	if _, err = service.Execute(ctx, admin, "sites_retention_apply", raw); core.ErrorStatus(err) != 409 {
		t.Fatal("expired plan accepted", err)
	}
	fresh := action(t, service, admin, "sites_retention_plan", map[string]any{"id": "fixture"}).(RetentionPlan)
	values["expected_revision"] = policy.Revision
	values["keep_count"] = 2
	action(t, service, admin, "sites_retention_save", values)
	raw, _ = json.Marshal(map[string]any{"id": "fixture", "plan_id": fresh.ID})
	if _, err = service.Execute(ctx, admin, "sites_retention_apply", raw); core.ErrorStatus(err) != 409 {
		t.Fatal("stale policy preview accepted", err)
	}
}

func TestRetentionRechecksOwnerBetweenIndividualDeletions(t *testing.T) {
	service, p, admin := actualProvider(t, nil)
	p.Close()
	ctx := context.Background()
	created, err := service.CreateToken(ctx, admin, core.TokenRequest{Name: "cleanup-owner", Kind: "mcp", Scopes: []string{"sites:backup", "sites:read"}, Resources: []string{"site:fixture"}, ExpiresInSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := service.AuthenticateToken(ctx, created.Token)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := retainedBackup{ID: "old-a", CreatedAt: now.AddDate(0, 0, -60), SHA256: strings.Repeat("a", 64)}
	older := old
	older.ID = "old-b"
	older.CreatedAt = now.AddDate(0, 0, -90)
	policy := action(t, service, admin, "sites_retention_save", map[string]any{"id": "fixture", "keep_count": 1, "keep_days": 30, "expected_revision": 0}).(RetentionPolicy)
	plan := RetentionPlan{ID: "cleanup_fixture", Policy: policy, CreatedAt: now, ExpiresAt: now.Add(time.Minute), Candidates: []retainedBackup{old, older}, Status: "submitted", Outcomes: []RetentionOutcome{}}
	raw, _ := json.Marshal(plan)
	if err = p.deps.State.Save(ctx, "retention_plan", plan.ID, raw); err != nil {
		t.Fatal(err)
	}
	deletes := 0
	p.deps.Dispatch = func(_ context.Context, _ core.Actor, name string, _ json.RawMessage) (any, error) {
		if name == "sites_backups_list" {
			return []retainedBackup{{ID: "newest", CreatedAt: now, SHA256: old.SHA256}, old, older}, nil
		}
		deletes++
		return map[string]any{"deleted": true}, service.RevokeToken(ctx, admin, created.ID)
	}
	result, err := p.cleanupBackups(ctx, retentionJob{Actor: actor.Public(), PlanID: plan.ID})
	if err == nil || deletes != 1 || result.Status != "interrupted" {
		t.Fatal("revoked identity continued cleanup", result, err, deletes)
	}
	if _, err = p.cleanupBackups(ctx, retentionJob{Actor: actor.Public(), PlanID: plan.ID}); core.ErrorStatus(err) != 409 {
		t.Fatal("interrupted cleanup replayed", err)
	}
}

func TestDatabaseRetentionPreservesNativeResourceAndBackupFormat(t *testing.T) {
	service, p, admin := actualProvider(t, nil)
	p.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	target := map[string]any{"instance_id": "mysql-main", "database": "owned"}
	p.deps.Dispatch = func(_ context.Context, _ core.Actor, name string, raw json.RawMessage) (any, error) {
		var in map[string]string
		_ = json.Unmarshal(raw, &in)
		if name != "managed_database_backups_list" || in["instance_id"] != "mysql-main" || in["database"] != "owned" || len(in) != 2 {
			t.Errorf("wrong native backup request %s %v", name, in)
		}
		return map[string]any{"items": []map[string]any{{"id": "newest", "created_at": now.Format(time.RFC3339Nano), "sha256": strings.Repeat("a", 64), "bytes": 128}, {"id": "old", "created_at": now.AddDate(0, 0, -90).Format(time.RFC3339Nano), "sha256": strings.Repeat("b", 64), "bytes": 64}}}, nil
	}
	save := map[string]any{"instance_id": "mysql-main", "database": "owned", "keep_count": 1, "keep_days": 30, "expected_revision": 0}
	action(t, service, admin, "managed_database_retention_save", save)
	plan := action(t, service, admin, "managed_database_retention_plan", target).(RetentionPlan)
	if len(plan.Candidates) != 1 || plan.Candidates[0].Size != 64 || plan.Candidates[0].ID != "old" {
		t.Fatal(plan)
	}
	resource, err := retentionResource("database", plan.Policy.BackupTarget)
	if err != nil || resource != "db:mysql-main:owned" {
		t.Fatal(resource, err)
	}
	token, err := service.CreateToken(ctx, admin, core.TokenRequest{Name: "other-database", Kind: "mcp", Scopes: []string{"databases:backup"}, Resources: []string{"db:mysql-main:other"}, ExpiresInSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := service.AuthenticateToken(ctx, token.Token)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(target)
	if _, err = service.Execute(ctx, actor, "managed_database_retention_get", raw); core.ErrorStatus(err) != 403 {
		t.Fatal("retention policy escaped database resource grants", err)
	}
}
