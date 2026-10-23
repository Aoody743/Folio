package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

type noteCreateInput struct {
	Title    string `json:"title"`
	Content  string `json:"content"`
	Pinned   bool   `json:"pinned"`
	TargetID string `json:"target_id"`
}
type noteUpdateInput struct {
	ID               string  `json:"id"`
	ExpectedRevision int64   `json:"expected_revision"`
	Title            *string `json:"title,omitempty"`
	Content          *string `json:"content,omitempty"`
	Pinned           *bool   `json:"pinned,omitempty"`
	TargetID         *string `json:"target_id,omitempty"`
}
type noteIDInput struct {
	ID               string `json:"id"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
	Revision         int64  `json:"revision,omitempty"`
}
type noteListInput struct {
	Sort     string `json:"sort,omitempty"`
	Archived bool   `json:"archived,omitempty"`
	TargetID string `json:"target_id,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Offset   int    `json:"offset,omitempty"`
}

const noteColumns = "id,title,content,pinned,archived,target_id,revision,created_at,updated_at,created_by,updated_by"

type scanner interface{ Scan(...any) error }

func scanNote(row scanner) (Note, error) {
	var n Note
	var created, updated string
	e := row.Scan(&n.ID, &n.Title, &n.Content, &n.Pinned, &n.Archived, &n.TargetID, &n.Revision, &n.CreatedAt, &n.UpdatedAt, &created, &updated)
	if e != nil {
		return n, e
	}
	if json.Unmarshal([]byte(created), &n.CreatedBy) != nil || json.Unmarshal([]byte(updated), &n.UpdatedBy) != nil {
		return n, internal()
	}
	return n, nil
}
func validateNote(n Note) error {
	if strings.TrimSpace(n.Title) == "" || utf8.RuneCountInString(n.Title) > 80 || !utf8.ValidString(n.Title) {
		return invalid("Note title must be 1–80 Unicode characters")
	}
	if utf8.RuneCountInString(n.Content) > 10000 || len(n.Content) > 60000 || !utf8.ValidString(n.Content) {
		return invalid("Note content must be at most 10,000 Unicode characters and 60,000 bytes")
	}
	if n.TargetID == "" || len(n.TargetID) > 256 {
		return invalid("A valid target_id is required")
	}
	return nil
}
func conflict(n Note) error {
	return &Error{Code: "revision_conflict", Message: "Note changed; merge with the current revision before saving", Status: 412, Details: map[string]any{"current": n, "current_revision": n.Revision}}
}

func (s *Service) noteCreate(ctx context.Context, a Actor, input noteCreateInput) (any, error) {
	if input.TargetID == "" {
		input.TargetID = "host:local"
	}
	if !hasResource(a, input.TargetID) {
		return nil, denied()
	}
	n := Note{ID: randomID("note_"), Title: input.Title, Content: input.Content, Pinned: input.Pinned, TargetID: input.TargetID, Revision: 1, CreatedAt: nowString(), CreatedBy: a.Public(), UpdatedBy: a.Public()}
	n.UpdatedAt = n.CreatedAt
	if e := validateNote(n); e != nil {
		return nil, e
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, internal()
	}
	defer tx.Rollback()
	_, e = tx.ExecContext(ctx, "INSERT INTO notes("+noteColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?)", n.ID, n.Title, n.Content, n.Pinned, n.Archived, n.TargetID, n.Revision, n.CreatedAt, n.UpdatedAt, jsonText(n.CreatedBy), jsonText(n.UpdatedBy))
	if e != nil {
		return nil, internal()
	}
	if e = saveRevision(ctx, tx, n); e != nil {
		return nil, e
	}
	entry, e := noteAudit(ctx, tx, a, "notes_create", n)
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, internal()
	}
	s.publish(Event{Type: "notes_changed", Data: map[string]any{"id": n.ID, "revision": n.Revision}}, "notes:read", n.TargetID)
	s.publish(Event{Type: "audit_changed", Data: map[string]any{"id": entry.ID}}, "audit:read", entry.Resource)
	return n, nil
}
func (s *Service) noteGet(ctx context.Context, a Actor, id string) (Note, error) {
	n, e := scanNote(s.db.QueryRowContext(ctx, "SELECT "+noteColumns+" FROM notes WHERE id=?", id))
	if e == sql.ErrNoRows {
		return Note{}, missing("Note")
	}
	if e != nil {
		return Note{}, internal()
	}
	if !hasResource(a, n.TargetID) {
		return Note{}, denied()
	}
	return n, nil
}
func (s *Service) noteList(ctx context.Context, a Actor, input noteListInput) (any, error) {
	if input.Limit == 0 {
		input.Limit = 50
	}
	if input.Limit < 1 || input.Limit > 100 || input.Offset < 0 || input.Offset > 100000 {
		return nil, invalid("Pagination requires limit 1–100 and offset 0–100,000")
	}
	if input.TargetID != "" && !hasResource(a, input.TargetID) {
		return nil, denied()
	}
	// Filtering takes place before pagination so restricted actors cannot infer
	// inaccessible resource counts from sparse pages.
	query := "SELECT " + noteColumns + " FROM notes WHERE archived=?"
	args := []any{input.Archived}
	if input.TargetID != "" {
		query += " AND target_id=?"
		args = append(args, input.TargetID)
	}
	allowed := []string{}
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
			allowed = append(allowed, "target_id LIKE ? ESCAPE '\\'")
			prefix := strings.TrimSuffix(r, "*")
			prefix = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(prefix, "\\", "\\\\"), "%", "\\%"), "_", "\\_")
			args = append(args, prefix+"%")
		} else {
			allowed = append(allowed, "target_id=?")
			args = append(args, r)
		}
	}
	if !all {
		if len(allowed) == 0 {
			return []Note{}, nil
		}
		query += " AND (" + strings.Join(allowed, " OR ") + ")"
	}
	if input.Sort != "" && input.Sort != "updated_at" {
		return nil, invalid("Unsupported note order")
	}
	if input.Sort == "updated_at" {
		query += " ORDER BY updated_at DESC,id LIMIT ? OFFSET ?"
	} else {
		query += " ORDER BY pinned DESC,updated_at DESC,id LIMIT ? OFFSET ?"
	}
	args = append(args, input.Limit, input.Offset)
	rows, e := s.db.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, internal()
	}
	defer rows.Close()
	out := []Note{}
	for rows.Next() {
		n, e := scanNote(rows)
		if e != nil {
			return nil, internal()
		}
		out = append(out, n)
	}
	if rows.Err() != nil {
		return nil, internal()
	}
	return out, nil
}
func saveRevision(ctx context.Context, tx *sql.Tx, n Note) error {
	_, e := tx.ExecContext(ctx, "INSERT INTO note_history(note_id,revision,snapshot) VALUES(?,?,?)", n.ID, n.Revision, jsonText(n))
	if e != nil {
		return internal()
	}
	return nil
}
func (s *Service) noteMutate(ctx context.Context, a Actor, action string, input noteUpdateInput, restoreRevision int64) (any, error) {
	if input.ExpectedRevision < 1 {
		return nil, invalid("expected_revision is required")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, internal()
	}
	defer tx.Rollback()
	n, e := scanNote(tx.QueryRowContext(ctx, "SELECT "+noteColumns+" FROM notes WHERE id=?", input.ID))
	if e == sql.ErrNoRows {
		return nil, missing("Note")
	}
	if e != nil {
		return nil, internal()
	}
	if !hasResource(a, n.TargetID) {
		return nil, denied()
	}
	if n.Revision != input.ExpectedRevision {
		return nil, conflict(n)
	}
	previousTarget := n.TargetID
	switch action {
	case "notes_update":
		if input.Title != nil {
			n.Title = *input.Title
		}
		if input.Content != nil {
			n.Content = *input.Content
		}
		if input.Pinned != nil {
			n.Pinned = *input.Pinned
		}
		if input.TargetID != nil {
			if !hasResource(a, *input.TargetID) {
				return nil, denied()
			}
			n.TargetID = *input.TargetID
		}
	case "notes_archive":
		n.Archived = true
	case "notes_restore":
		if restoreRevision > 0 {
			var snapshot string
			if e = tx.QueryRowContext(ctx, "SELECT snapshot FROM note_history WHERE note_id=? AND revision=?", n.ID, restoreRevision).Scan(&snapshot); e == sql.ErrNoRows {
				return nil, missing("Note revision")
			}
			if e != nil {
				return nil, internal()
			}
			var old Note
			if json.Unmarshal([]byte(snapshot), &old) != nil {
				return nil, internal()
			}
			if !hasResource(a, old.TargetID) {
				return nil, denied()
			}
			n.Title = old.Title
			n.Content = old.Content
			n.Pinned = old.Pinned
			n.TargetID = old.TargetID
		}
		n.Archived = false
	}
	if e = validateNote(n); e != nil {
		return nil, e
	}
	n.Revision++
	n.UpdatedAt = nowString()
	n.UpdatedBy = a.Public()
	res, e := tx.ExecContext(ctx, "UPDATE notes SET title=?,content=?,pinned=?,archived=?,target_id=?,revision=?,updated_at=?,updated_by=? WHERE id=? AND revision=?", n.Title, n.Content, n.Pinned, n.Archived, n.TargetID, n.Revision, n.UpdatedAt, jsonText(n.UpdatedBy), n.ID, input.ExpectedRevision)
	if e != nil {
		return nil, internal()
	}
	changed, _ := res.RowsAffected()
	if changed != 1 {
		return nil, conflict(n)
	}
	if e = saveRevision(ctx, tx, n); e != nil {
		return nil, e
	}
	entry, e := noteAudit(ctx, tx, a, action, n)
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, internal()
	}
	s.publish(Event{Type: "notes_changed", Data: map[string]any{"id": n.ID, "revision": n.Revision}}, "notes:read", previousTarget)
	if previousTarget != n.TargetID {
		s.publish(Event{Type: "notes_changed", Data: map[string]any{"id": n.ID, "revision": n.Revision}}, "notes:read", n.TargetID)
	}
	s.publish(Event{Type: "audit_changed", Data: map[string]any{"id": entry.ID}}, "audit:read", entry.Resource)
	return n, nil
}
func (s *Service) noteDelete(ctx context.Context, a Actor, input noteIDInput) (any, error) {
	if input.ExpectedRevision < 1 {
		return nil, invalid("expected_revision is required")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, internal()
	}
	defer tx.Rollback()
	n, e := scanNote(tx.QueryRowContext(ctx, "SELECT "+noteColumns+" FROM notes WHERE id=?", input.ID))
	if e == sql.ErrNoRows {
		return nil, missing("Note")
	}
	if e != nil {
		return nil, internal()
	}
	if !hasResource(a, n.TargetID) {
		return nil, denied()
	}
	if n.Revision != input.ExpectedRevision {
		return nil, conflict(n)
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM notes WHERE id=? AND revision=?", n.ID, n.Revision); e != nil {
		return nil, internal()
	}
	entry, e := noteAudit(ctx, tx, a, "notes_delete", n)
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, internal()
	}
	s.publish(Event{Type: "notes_changed", Data: map[string]any{"id": n.ID, "deleted": true}}, "notes:read", n.TargetID)
	s.publish(Event{Type: "audit_changed", Data: map[string]any{"id": entry.ID}}, "audit:read", entry.Resource)
	return map[string]any{"id": n.ID, "deleted": true}, nil
}
func (s *Service) noteHistory(ctx context.Context, a Actor, input noteIDInput) (any, error) {
	n, e := s.noteGet(ctx, a, input.ID)
	if e != nil {
		return nil, e
	}
	rows, e := s.db.QueryContext(ctx, "SELECT snapshot FROM note_history WHERE note_id=? ORDER BY revision DESC LIMIT 100", n.ID)
	if e != nil {
		return nil, internal()
	}
	defer rows.Close()
	history := []Note{}
	for rows.Next() {
		var text string
		if e = rows.Scan(&text); e != nil {
			return nil, internal()
		}
		var old Note
		if json.Unmarshal([]byte(text), &old) != nil {
			return nil, internal()
		}
		if hasResource(a, old.TargetID) {
			history = append(history, old)
		}
	}
	return history, nil
}

func noteAudit(ctx context.Context, tx *sql.Tx, a Actor, action string, n Note) (Audit, error) {
	entry := Audit{ID: randomID("audit_"), Action: action, Actor: a.Public(), Resource: n.TargetID, Status: "succeeded", Summary: map[string]any{"id": n.ID, "revision": n.Revision}, CreatedAt: nowString()}
	_, err := tx.ExecContext(ctx, "INSERT INTO audit(id,action,actor,resource,status,error_code,summary,created_at) VALUES(?,?,?,?,?,?,?,?)", entry.ID, entry.Action, jsonText(entry.Actor), entry.Resource, entry.Status, "", jsonText(entry.Summary), entry.CreatedAt)
	if err != nil {
		return Audit{}, internal()
	}
	return entry, nil
}
