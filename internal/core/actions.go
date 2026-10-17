package core

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

func object(properties map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
func str() map[string]any     { return map[string]any{"type": "string"} }
func integer() map[string]any { return map[string]any{"type": "integer"} }
func boolean() map[string]any { return map[string]any{"type": "boolean"} }
func registry() []Action {
	id := map[string]any{"id": str()}
	revision := map[string]any{"id": str(), "expected_revision": integer()}
	restore := map[string]any{"id": str(), "expected_revision": integer(), "revision": integer()}
	return []Action{
		{Name: "capabilities_list", Description: "Discover actions permitted for the current identity", InputSchema: object(map[string]any{}), ReadOnly: true, Scope: ""},
		{Name: "system_get_status", Description: "Read the cached, real host health and metrics snapshot", InputSchema: object(map[string]any{}), ReadOnly: true, Scope: "system:read"},
		{Name: "notes_list", Description: "List shared notes within authorized resources", InputSchema: object(map[string]any{"archived": boolean(), "target_id": str(), "limit": integer(), "offset": integer()}), ReadOnly: true, Scope: "notes:read"},
		{Name: "notes_get", Description: "Read one shared note", InputSchema: object(id, "id"), ReadOnly: true, Scope: "notes:read"},
		{Name: "notes_create", Description: "Create a shared note; identity is assigned by the server", InputSchema: object(map[string]any{"title": str(), "content": str(), "pinned": boolean(), "target_id": str()}, "title"), Scope: "notes:write"},
		{Name: "notes_update", Description: "Update a note using optimistic revision concurrency", InputSchema: object(map[string]any{"id": str(), "expected_revision": integer(), "title": str(), "content": str(), "pinned": boolean(), "target_id": str()}, "id", "expected_revision"), Scope: "notes:write"},
		{Name: "notes_archive", Description: "Archive a shared note without destroying its history", InputSchema: object(revision, "id", "expected_revision"), Scope: "notes:write"},
		{Name: "notes_restore", Description: "Unarchive a note or restore a historical revision as a new revision", InputSchema: object(restore, "id", "expected_revision"), Scope: "notes:write"},
		{Name: "notes_delete", Description: "Permanently delete a note and its revisions", InputSchema: object(revision, "id", "expected_revision"), Scope: "notes:delete"},
		{Name: "notes_history", Description: "Read up to 100 latest note revisions", InputSchema: object(id, "id"), ReadOnly: true, Scope: "notes:read"},
		{Name: "audit_list", Description: "Read bounded operation records scoped to authorized resources", InputSchema: object(map[string]any{"limit": integer(), "offset": integer()}), ReadOnly: true, Scope: "audit:read"},
		{Name: "audit_get", Description: "Read one authorized operation record", InputSchema: object(id, "id"), ReadOnly: true, Scope: "audit:read"},
		{Name: "tasks_list", Description: "List durable jobs in authorized filesystem roots", InputSchema: object(map[string]any{}), ReadOnly: true, Scope: "tasks:read"},
		{Name: "tasks_get", Description: "Read the durable result and state of a job", InputSchema: object(id, "id"), ReadOnly: true, Scope: "tasks:read"},
		{Name: "tasks_cancel", Description: "Request cancellation of a running job", InputSchema: object(id, "id"), Scope: "tasks:write"},
		{Name: "tasks_logs", Description: "Read bounded captured job output", InputSchema: object(id, "id"), ReadOnly: true, Scope: "tasks:read"},
		{Name: "files_write", Description: "Submit an atomic file-write task in an explicitly permitted filesystem root", InputSchema: object(map[string]any{"root": str(), "path": str(), "content": str()}, "root", "path", "content"), Scope: "files:write"},
		{Name: "files_backup", Description: "Submit an atomic file backup between permitted filesystem roots", InputSchema: object(map[string]any{"root": str(), "path": str(), "destination_root": str(), "destination_path": str()}, "root", "path", "destination_root", "destination_path"), Scope: "files:write"},
		{Name: "files_hash", Description: "Submit a real SHA-256 verification task for an authorized file", InputSchema: object(map[string]any{"root": str(), "path": str()}, "root", "path"), ReadOnly: true, Scope: "files:read"},
	}
}
func (s *Service) Actions(a Actor) []Action {
	out := []Action{}
	if a.ID == "" {
		return out
	}
	for _, d := range s.allActions() {
		if d.Scope == "" || hasScope(a, d.Scope) {
			if d.Name == "system_get_status" && !hasResource(a, "host:local") {
				continue
			}
			d.InputSchema = cloneSchema(d.InputSchema)
			d.OutputSchema = cloneSchema(d.OutputSchema)
			out = append(out, d)
		}
	}
	return out
}
func decodeInput(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if len(raw) > 64<<10 {
		return &Error{Code: "payload_too_large", Message: "Action input exceeds 64 KiB", Status: 413}
	}
	if !json.Valid(raw) || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return invalid("Action input must be a JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return invalid("Action input has invalid or unknown fields")
	}
	if e := d.Decode(new(any)); e != io.EOF {
		return invalid("Action input must contain exactly one JSON object")
	}
	return nil
}

// Registry schema checks live here rather than in protocol adapters, so REST
// and MCP reject the same missing, null, mistyped and unknown parameters.
func validateLegacySchema(raw json.RawMessage, schema map[string]any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if len(raw) > 64<<10 {
		return &Error{Code: "payload_too_large", Message: "Action input exceeds 64 KiB", Status: 413}
	}
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(raw, &fields); e != nil || fields == nil {
		return invalid("Action input must be a JSON object")
	}
	if required, ok := schema["required"].([]string); ok {
		for _, name := range required {
			if _, present := fields[name]; !present {
				return invalid("Required field is missing: " + name)
			}
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	for name, value := range fields {
		property, known := properties[name]
		if !known {
			return invalid("Unknown field: " + name)
		}
		definition, _ := property.(map[string]any)
		kind, _ := definition["type"].(string)
		value = bytes.TrimSpace(value)
		switch kind {
		case "string":
			var text string
			if len(value) == 0 || value[0] != '"' || json.Unmarshal(value, &text) != nil {
				return invalid("Field must be a string: " + name)
			}
		case "boolean":
			if !bytes.Equal(value, []byte("true")) && !bytes.Equal(value, []byte("false")) {
				return invalid("Field must be a boolean: " + name)
			}
		case "integer":
			if _, e := strconv.ParseInt(string(value), 10, 64); e != nil {
				return invalid("Field must be an integer: " + name)
			}
		}
	}
	return nil
}

func (s *Service) executeOnce(ctx context.Context, a Actor, name string, raw json.RawMessage) (result any, err error) {
	if binding, ok := s.binding(name); ok {
		if a.ID == "" {
			return nil, unauthorized()
		}
		if !hasScope(a, binding.Action.Scope) {
			s.recordAudit(ctx, a, name, "", denied(), nil)
			return nil, denied()
		}
		if e := validateSchema(raw, binding.Action.InputSchema); e != nil {
			return nil, e
		}
		return s.executeProvider(ctx, a, binding, raw)
	}
	var action *Action
	for _, d := range registry() {
		if d.Name == name {
			copy := d
			action = &copy
			break
		}
	}
	if action == nil {
		return nil, missing("Action")
	}
	if a.ID == "" {
		return nil, unauthorized()
	}
	resource := "host:local"
	summary := map[string]any{}
	defer func() {
		if err == nil && !action.ReadOnly && strings.HasPrefix(name, "notes_") {
			return
		} // Note mutations atomically persist their own audit.
		// Background polling must not fsync every read on the single SQLite
		// writer. Mutations, authentication and denied attempts remain durable;
		// successful snapshots/lists are deliberately not operation records.
		if err == nil && action.ReadOnly {
			return
		}
		if name != "audit_list" && name != "capabilities_list" {
			s.recordAudit(ctx, a, name, resource, err, summary)
		}
	}()
	if action.Scope != "" && !hasScope(a, action.Scope) {
		return nil, denied()
	}
	if e := validateSchema(raw, action.InputSchema); e != nil {
		return nil, e
	}
	switch name {
	case "capabilities_list":
		var input struct{}
		if e := decodeInput(raw, &input); e != nil {
			return nil, e
		}
		return s.Actions(a), nil
	case "system_get_status":
		var input struct{}
		if e := decodeInput(raw, &input); e != nil {
			return nil, e
		}
		if !hasResource(a, "host:local") {
			return nil, denied()
		}
		return s.status.get(), nil
	case "notes_list":
		var input noteListInput
		if e := decodeInput(raw, &input); e != nil {
			return nil, e
		}
		return s.noteList(ctx, a, input)
	case "notes_create":
		var input noteCreateInput
		if e := decodeInput(raw, &input); e != nil {
			return nil, e
		}
		if input.TargetID != "" {
			resource = input.TargetID
		}
		res, e := s.noteCreate(ctx, a, input)
		if n, ok := res.(Note); ok {
			summary["id"] = n.ID
			summary["revision"] = n.Revision
		}
		return res, e
	case "notes_update":
		var input noteUpdateInput
		if e := decodeInput(raw, &input); e != nil {
			return nil, e
		}
		summary["id"] = input.ID
		res, e := s.noteMutate(ctx, a, name, input, 0)
		if n, ok := res.(Note); ok {
			resource = n.TargetID
			summary["revision"] = n.Revision
		}
		return res, e
	case "notes_get", "notes_history", "notes_archive", "notes_restore", "notes_delete":
		var input noteIDInput
		if e := decodeInput(raw, &input); e != nil {
			return nil, e
		}
		if input.ID == "" {
			return nil, invalid("id is required")
		}
		summary["id"] = input.ID
		if n, e := s.noteGet(ctx, a, input.ID); e == nil {
			resource = n.TargetID
		}
		switch name {
		case "notes_get":
			return s.noteGet(ctx, a, input.ID)
		case "notes_history":
			return s.noteHistory(ctx, a, input)
		case "notes_delete":
			return s.noteDelete(ctx, a, input)
		default:
			return s.noteMutate(ctx, a, name, noteUpdateInput{ID: input.ID, ExpectedRevision: input.ExpectedRevision}, input.Revision)
		}
	case "audit_list":
		var input struct {
			Limit  int `json:"limit"`
			Offset int `json:"offset"`
		}
		if e := decodeInput(raw, &input); e != nil {
			return nil, e
		}
		return s.auditList(ctx, a, input.Limit, input.Offset)
	case "audit_get":
		var input struct {
			ID string `json:"id"`
		}
		if e := decodeInput(raw, &input); e != nil {
			return nil, e
		}
		if input.ID == "" {
			return nil, invalid("id is required")
		}
		return s.auditGet(ctx, a, input.ID)
	case "tasks_list":
		var input struct{}
		if e := decodeInput(raw, &input); e != nil {
			return nil, e
		}
		return s.listJobs(ctx, a)
	case "tasks_get", "tasks_cancel", "tasks_logs":
		var input struct {
			ID string `json:"id"`
		}
		if e := decodeInput(raw, &input); e != nil {
			return nil, e
		}
		if job, e := s.jobs.Get(ctx, input.ID); e == nil && s.canAccessJob(ctx, a, job) {
			resources := s.taskResources(ctx, job)
			if len(resources) > 0 {
				resource = resources[0]
			}
		}
		return s.jobAction(ctx, a, name, input.ID)
	case "files_write", "files_backup", "files_hash":
		res, e := s.submitFileJob(ctx, a, name, raw)
		if job, ok := res.(map[string]any); ok {
			summary["task_id"] = job["id"]
			summary["task_status"] = job["status"]
			if target, ok := job["resource"].(string); ok {
				resource = target
			}
		}
		return res, e
	}
	return nil, missing("Action")
}
func (s *Service) auditList(ctx context.Context, a Actor, limit, offset int) (any, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 || offset < 0 || offset > 100000 {
		return nil, invalid("Pagination requires limit 1–100 and offset 0–100,000")
	}
	query := "SELECT id,action,actor,resource,status,error_code,summary,created_at FROM audit"
	args := []any{}
	conditions := []string{}
	all := false
	for _, r := range expandResourceAliases(a.Resources) {
		if r == "*" {
			all = true
			break
		}
	}
	for _, r := range expandResourceAliases(a.Resources) {
		if all {
			break
		}
		if strings.HasSuffix(r, ":*") {
			conditions = append(conditions, "resource LIKE ? ESCAPE '\\'")
			prefix := strings.TrimSuffix(r, "*")
			prefix = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(prefix, "\\", "\\\\"), "%", "\\%"), "_", "\\_")
			args = append(args, prefix+"%")
		} else {
			conditions = append(conditions, "resource=?")
			args = append(args, r)
		}
	}
	if !all {
		if len(conditions) == 0 {
			return []Audit{}, nil
		}
		query += " WHERE (" + strings.Join(conditions, " OR ") + ")"
	}
	query += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, e := s.db.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, internal()
	}
	defer rows.Close()
	out := []Audit{}
	for rows.Next() {
		var row Audit
		var actor, summary string
		if e = rows.Scan(&row.ID, &row.Action, &actor, &row.Resource, &row.Status, &row.ErrorCode, &summary, &row.CreatedAt); e != nil {
			return nil, internal()
		}
		_ = json.Unmarshal([]byte(actor), &row.Actor)
		_ = json.Unmarshal([]byte(summary), &row.Summary)
		out = append(out, row)
	}
	if rows.Err() != nil {
		return nil, internal()
	}
	return out, nil
}

func (s *Service) auditGet(ctx context.Context, a Actor, id string) (any, error) {
	var entry Audit
	var actor, summary string
	err := s.db.QueryRowContext(ctx, "SELECT id,action,actor,resource,status,error_code,summary,created_at FROM audit WHERE id=?", id).
		Scan(&entry.ID, &entry.Action, &actor, &entry.Resource, &entry.Status, &entry.ErrorCode, &summary, &entry.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, missing("Audit event")
	}
	if err != nil {
		return nil, internal()
	}
	if !hasResource(a, entry.Resource) {
		return nil, denied()
	}
	if json.Unmarshal([]byte(actor), &entry.Actor) != nil || json.Unmarshal([]byte(summary), &entry.Summary) != nil {
		return nil, internal()
	}
	return entry, nil
}
