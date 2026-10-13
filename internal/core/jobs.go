package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/jobs"
)

type sqliteJobStore struct{ s *Service }

func (store sqliteJobStore) Save(ctx context.Context, j jobs.Job) error {
	resource := jobResource(j)
	meta, present := ctx.Value(taskMetaKey{}).(taskMetadata)
	if !present {
		meta, present = store.s.loadTaskMetadata(ctx, j.ID)
	}
	if present && len(meta.Resources) > 0 {
		resource = meta.Resources[0]
	}
	tx, err := store.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO tasks(id,actor,resource,state,payload,created_at,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET state=excluded.state,payload=excluded.payload,updated_at=excluded.updated_at`, j.ID, j.ActorID, resource, j.Status, jsonText(j), j.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), j.UpdatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"))
	if err == nil && present {
		_, err = tx.ExecContext(ctx, "INSERT INTO task_metadata(id,actor,resources,action) VALUES(?,?,?,?) ON CONFLICT(id) DO NOTHING", j.ID, jsonText(meta.Actor), jsonText(meta.Resources), jsonText(map[string]any{"name": meta.Action, "scope": meta.Scope}))
	}
	if err == nil {
		if id, ok := ctx.Value(idempotencyContextKey{}).(string); ok && id != "" {
			_, err = tx.ExecContext(ctx, "UPDATE idempotency SET task_id=? WHERE id=? AND state='running'", j.ID, id)
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	if err == nil {
		store.s.publish(Event{Type: "tasks_changed", Data: map[string]any{"id": j.ID, "status": j.Status}}, "tasks:read", resource)
		if j.Status == jobs.Succeeded || j.Status == jobs.Failed || j.Status == jobs.Canceled || j.Status == jobs.Interrupted {
			code := ""
			if j.Status != jobs.Succeeded {
				code = "task_" + j.Status
			}
			store.s.persistAudit(ctx, Audit{ID: randomID("audit_"), Action: j.Operation + "_finished", Actor: store.s.jobActor(ctx, j), Resource: resource, Status: j.Status, ErrorCode: code, Summary: map[string]any{"task_id": j.ID, "task_status": j.Status}, CreatedAt: nowString()})
		}
	}
	return err
}
func (store sqliteJobStore) Get(ctx context.Context, id string) (jobs.Job, error) {
	var j jobs.Job
	var raw string
	if err := store.s.db.QueryRowContext(ctx, "SELECT payload FROM tasks WHERE id=?", id).Scan(&raw); err == sql.ErrNoRows {
		return j, jobs.ErrNotFound
	} else if err != nil {
		return j, err
	}
	if err := json.Unmarshal([]byte(raw), &j); err != nil {
		return j, err
	}
	return j, nil
}
func (store sqliteJobStore) List(ctx context.Context) ([]jobs.Job, error) {
	rows, err := store.s.db.QueryContext(ctx, "SELECT payload FROM tasks WHERE state IN ('queued','running') OR id IN (SELECT id FROM tasks ORDER BY created_at DESC LIMIT 1000) ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []jobs.Job{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var j jobs.Job
		if err = json.Unmarshal([]byte(raw), &j); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func jobResource(j jobs.Job) string {
	var input agent.FileInput
	_ = json.Unmarshal(j.Input, &input)
	if input.Root == "" {
		return "host:local"
	}
	return "files:" + input.Root
}
func jobAllowed(a Actor, j jobs.Job) bool {
	var in agent.FileInput
	_ = json.Unmarshal(j.Input, &in)
	if !hasResource(a, jobResource(j)) {
		return false
	}
	return in.DestinationRoot == "" || hasResource(a, "files:"+in.DestinationRoot)
}
func (s *Service) loadTaskMetadata(ctx context.Context, id string) (taskMetadata, bool) {
	var actor, resources, action string
	var meta taskMetadata
	if err := s.db.QueryRowContext(ctx, "SELECT actor,resources,action FROM task_metadata WHERE id=?", id).Scan(&actor, &resources, &action); err != nil {
		return meta, false
	}
	if json.Unmarshal([]byte(actor), &meta.Actor) != nil || json.Unmarshal([]byte(resources), &meta.Resources) != nil {
		return meta, false
	}
	var source struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	if json.Unmarshal([]byte(action), &source) != nil {
		return meta, false
	}
	meta.Action = source.Name
	meta.Scope = source.Scope
	return meta, true
}
func (s *Service) taskResources(ctx context.Context, j jobs.Job) []string {
	if meta, ok := s.loadTaskMetadata(ctx, j.ID); ok {
		return meta.Resources
	}
	resources := []string{jobResource(j)}
	var in agent.FileInput
	_ = json.Unmarshal(j.Input, &in)
	if in.DestinationRoot != "" {
		resources = append(resources, "files:"+in.DestinationRoot)
	}
	return resources
}
func (s *Service) canAccessJob(ctx context.Context, a Actor, j jobs.Job) bool {
	for _, resource := range s.taskResources(ctx, j) {
		if !hasResource(a, resource) {
			return false
		}
	}
	return true
}

// A task view intentionally omits input contents, which can contain private
// configuration files. The task author is resolved from the server identity.
func (s *Service) jobView(ctx context.Context, j jobs.Job) map[string]any {
	a := s.jobActor(ctx, j)
	resources := s.taskResources(ctx, j)
	return resolvedJobView(j, a, resources)
}
func resolvedJobView(j jobs.Job, a Actor, resources []string) map[string]any {
	if len(resources) == 0 {
		resources = []string{jobResource(j)}
	}
	out := map[string]any{"id": j.ID, "operation": j.Operation, "status": j.Status, "state": j.Status, "actor": a, "actor_id": j.ActorID, "resource": resources[0], "resources": resources, "created_at": j.CreatedAt, "updated_at": j.UpdatedAt, "cancel_requested": j.CancelRequested, "error": j.Error}
	if j.Result != nil {
		out["result"] = j.Result
	}
	return out
}
func (s *Service) jobActor(ctx context.Context, j jobs.Job) Actor {
	if meta, ok := s.loadTaskMetadata(ctx, j.ID); ok {
		return meta.Actor.Public()
	}
	a := Actor{ID: j.ActorID, Name: j.ActorID, Kind: "human"}
	if strings.HasPrefix(j.ActorID, "token:") {
		a.Kind = "mcp"
		_ = s.db.QueryRowContext(ctx, "SELECT name,kind FROM tokens WHERE id=?", strings.TrimPrefix(j.ActorID, "token:")).Scan(&a.Name, &a.Kind)
	} else {
		_ = s.db.QueryRowContext(ctx, "SELECT username FROM users WHERE id=?", j.ActorID).Scan(&a.Name)
	}
	return a.Public()
}
func (s *Service) listJobs(ctx context.Context, a Actor) (any, error) {
	all, err := s.jobs.List(ctx)
	if err != nil {
		return nil, internal()
	}
	out := []map[string]any{}
	for _, j := range all {
		if s.canAccessJob(ctx, a, j) {
			out = append(out, s.jobView(ctx, j))
		}
		if len(out) >= 100 {
			break
		}
	}
	return out, nil
}
func (s *Service) jobAction(ctx context.Context, a Actor, name, id string) (any, error) {
	if id == "" {
		return nil, invalid("id is required")
	}
	j, err := s.jobs.Get(ctx, id)
	if errors.Is(err, jobs.ErrNotFound) {
		return nil, missing("Task")
	}
	if err != nil {
		return nil, internal()
	}
	if !s.canAccessJob(ctx, a, j) {
		return nil, denied()
	}
	if name == "tasks_cancel" {
		j, err = s.jobs.Cancel(ctx, id)
		if err != nil {
			return nil, internal()
		}
	}
	if name == "tasks_logs" {
		if j.Result == nil {
			return map[string]any{"id": j.ID, "status": j.Status, "stdout": "", "stderr": "", "truncated": false}, nil
		}
		return map[string]any{"id": j.ID, "status": j.Status, "stdout": j.Result.Stdout, "stderr": j.Result.Stderr, "truncated": j.Result.Truncated}, nil
	}
	return s.jobView(ctx, j), nil
}
func (s *Service) submitFileJob(ctx context.Context, a Actor, name string, raw json.RawMessage) (any, error) {
	var input agent.FileInput
	if err := decodeInput(raw, &input); err != nil {
		return nil, err
	}
	if input.Root == "" || input.Path == "" {
		return nil, invalid("root and path are required")
	}
	if !hasResource(a, "files:"+input.Root) {
		return nil, denied()
	}
	if name == "files_backup" {
		if input.DestinationRoot == "" || input.DestinationPath == "" {
			return nil, invalid("Backup destination is required")
		}
		if !hasResource(a, "files:"+input.DestinationRoot) {
			return nil, denied()
		}
	}
	operation := map[string]string{"files_write": "file_write", "files_hash": "file_hash", "files_backup": "file_backup"}[name]
	scope := "files:write"
	if name == "files_hash" {
		scope = "files:read"
	}
	resources := []string{"files:" + input.Root}
	if input.DestinationRoot != "" {
		resources = append(resources, "files:"+input.DestinationRoot)
	}
	ctx = context.WithValue(ctx, taskMetaKey{}, taskMetadata{Actor: a.Public(), Resources: resources, Action: name, Scope: scope})
	j, err := s.jobs.Submit(ctx, a.ID, operation, raw)
	if errors.Is(err, agent.ErrUnsafePath) || errors.Is(err, agent.ErrReadOnly) {
		return nil, denied()
	}
	if errors.Is(err, jobs.ErrBusy) {
		return nil, &Error{Code: "busy", Message: "Task queue is full", Status: 429}
	}
	if err != nil {
		return nil, invalid("File task parameters could not be accepted")
	}
	return s.jobView(ctx, j), nil
}
