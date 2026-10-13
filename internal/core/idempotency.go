package core

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

type idempotencyFlight struct{ done chan struct{} }
type idempotencyContextKey struct{}

var validIdempotencyKey = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)

type cachedOperation struct {
	Hash, State, Result, Error, TaskID string
	Resources                          []string
}

func (s *Service) Execute(ctx context.Context, a Actor, name string, raw json.RawMessage) (any, error) {
	return s.ExecuteWithKey(ctx, a, name, raw, "")
}

// ExecuteWithKey gives mutations durable at-most-once admission. An interrupted
// external side effect is explicitly uncertain after restart, never replayed
// blindly. Actual permissions are checked again even for cached results.
func (s *Service) ExecuteWithKey(ctx context.Context, a Actor, name string, raw json.RawMessage, key string) (any, error) {
	fresh, err := s.RefreshActor(ctx, a)
	if err != nil {
		return nil, err
	}
	a = fresh
	if key == "" {
		return s.executeOnce(ctx, a, name, raw)
	}
	if !validIdempotencyKey.MatchString(key) {
		return nil, invalid("Idempotency-Key must contain 8–128 letters, digits, dots, underscores, colons or hyphens")
	}
	var action *Action
	for _, candidate := range s.allActions() {
		if candidate.Name == name {
			copy := candidate
			action = &copy
			break
		}
	}
	if action == nil {
		return nil, missing("Action")
	}
	if action.ReadOnly && !action.Task && !strings.HasPrefix(name, "files_") {
		return s.executeOnce(ctx, a, name, raw)
	}
	if action.Scope != "" && !hasScope(a, action.Scope) {
		s.recordAudit(ctx, a, name, "", denied(), nil)
		return nil, denied()
	}
	if err = validateSchema(raw, action.InputSchema); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var input any
	_ = decoder.Decode(&input)
	canonical, _ := json.Marshal(input)
	id := digest(jsonText([]string{a.ID, name, key}))
	inputHash := s.idempotencyInputHash(id, canonical)
	for {
		s.idempotencyMu.Lock()
		cached, found, e := s.cachedOperation(ctx, id)
		if e != nil {
			s.idempotencyMu.Unlock()
			return nil, e
		}
		if found {
			matched, legacy, hashErr := s.matchesIdempotencyHash(id, cached.Hash, inputHash, canonical)
			if hashErr != nil {
				s.idempotencyMu.Unlock()
				return nil, hashErr
			}
			if !matched {
				s.idempotencyMu.Unlock()
				return nil, &Error{Code: "idempotency_conflict", Message: "Idempotency key was already bound to different input", Status: 409}
			}
			if e = authorizeResources(a, cached.Resources); e != nil {
				s.idempotencyMu.Unlock()
				resource := ""
				if len(cached.Resources) > 0 {
					resource = cached.Resources[0]
				}
				s.recordAudit(ctx, a, name, resource, e, nil)
				return nil, e
			}
			if legacy {
				if _, e = s.db.ExecContext(ctx, "UPDATE idempotency SET input_hash=? WHERE id=? AND input_hash=?", inputHash, id, cached.Hash); e != nil {
					s.idempotencyMu.Unlock()
					return nil, internal()
				}
			}
			if cached.State == "completed" || cached.State == "failed" {
				s.idempotencyMu.Unlock()
				return decodeCached(name, cached)
			}
			if flight, ok := s.idempotencyFlights[id]; ok {
				done := flight.done
				s.idempotencyMu.Unlock()
				select {
				case <-done:
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			s.idempotencyMu.Unlock()
			return nil, &Error{Code: "operation_recovery_required", Message: "Previous attempt was interrupted; inspect its task and affected resource before a new operation", Status: 409, Details: map[string]any{"task_id": cached.TaskID, "state": cached.State}}
		}
		resources, e := s.resolveActionResources(ctx, a, name, raw)
		if e != nil {
			s.idempotencyMu.Unlock()
			return nil, e
		}
		if e = authorizeResources(a, resources); e != nil {
			s.idempotencyMu.Unlock()
			resource := ""
			if len(resources) > 0 {
				resource = resources[0]
			}
			s.recordAudit(ctx, a, name, resource, e, nil)
			return nil, e
		}
		if len(s.idempotencyFlights) >= 1024 {
			s.idempotencyMu.Unlock()
			return nil, &Error{Code: "busy", Message: "Too many active operations", Status: 429}
		}
		_, e = s.db.ExecContext(ctx, "INSERT INTO idempotency(id,actor_id,action,input_hash,state,resources,created_at,updated_at) VALUES(?,?,?,?,'running',?,?,?)", id, a.ID, name, inputHash, jsonText(resources), nowString(), nowString())
		if e != nil {
			s.idempotencyMu.Unlock()
			return nil, internal()
		}
		flight := &idempotencyFlight{done: make(chan struct{})}
		s.idempotencyFlights[id] = flight
		s.idempotencyMu.Unlock()
		result, execErr := s.safeExecuteOnce(context.WithValue(ctx, idempotencyContextKey{}, id), a, name, raw)
		resultText := ""
		errorText := ""
		state := "completed"
		if execErr != nil {
			state = "failed"
			if ErrorCode(execErr) == "operation_recovery_required" {
				state = "uncertain"
			}
			var safe *Error
			if !errors.As(execErr, &safe) {
				safe = &Error{Code: ErrorCode(execErr), Message: "Operation could not be completed", Status: ErrorStatus(execErr)}
			}
			errorText = jsonText(struct {
				Code, Message string
				Status        int
				Details       any
			}{safe.Code, safe.Message, safe.Status, safe.Details})
		} else {
			encoded, e := json.Marshal(result)
			if e != nil || len(encoded) > 512<<10 {
				state = "uncertain"
				execErr = &Error{Code: "operation_recovery_required", Message: "Operation completed but its result could not be safely cached; inspect before retrying", Status: 409}
			} else {
				ciphertext, encryptionErr := s.encrypt([]byte(jsonText(map[string]string{"id": id, "value": string(encoded)})))
				if encryptionErr != nil {
					state = "uncertain"
					execErr = internal()
				} else {
					resultText = "encrypted:v1:" + ciphertext
				}
			}
		}
		persistCtx := context.WithoutCancel(ctx)
		_, persistErr := s.db.ExecContext(persistCtx, "UPDATE idempotency SET state=?,result=?,error=?,updated_at=? WHERE id=?", state, resultText, errorText, nowString(), id)
		s.idempotencyMu.Lock()
		delete(s.idempotencyFlights, id)
		close(flight.done)
		s.idempotencyMu.Unlock()
		if persistErr != nil {
			return nil, &Error{Code: "operation_recovery_required", Message: "Operation result was not durably recorded; inspect before retrying", Status: 409}
		}
		return result, execErr
	}
}

// A fast unkeyed digest of password-bearing requests would let a database-only
// leak bypass password KDF costs. Bind the fingerprint to both the private key
// and reservation identity; it exposes no offline password verification oracle.
func (s *Service) idempotencyInputHash(id string, canonical []byte) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte("PowerLinux:IdempotencyInput:v1\x00"))
	mac.Write([]byte(id))
	mac.Write([]byte{0})
	mac.Write(canonical)
	return "hmac:v1:" + hex.EncodeToString(mac.Sum(nil))
}
func (s *Service) matchesIdempotencyHash(id, stored, expected string, canonical []byte) (bool, bool, error) {
	if strings.HasPrefix(stored, "hmac:v1:") {
		return hmac.Equal([]byte(stored), []byte(expected)), false, nil
	}
	if strings.HasPrefix(stored, "legacy-encrypted:v1:") {
		plain, err := s.decrypt(strings.TrimPrefix(stored, "legacy-encrypted:v1:"))
		var envelope struct{ ID, Hash string }
		if err != nil || json.Unmarshal(plain, &envelope) != nil || envelope.ID != id {
			return false, false, internal()
		}
		stored = envelope.Hash
	}
	decoded, err := hex.DecodeString(stored)
	if err != nil || len(decoded) != sha256.Size {
		return false, false, internal()
	}
	return hmac.Equal([]byte(stored), []byte(digest(string(canonical)))), true, nil
}

// Protect every old digest at startup, including reservations whose original
// input will never be retried. An authorized exact replay upgrades it to HMAC
// without repeating the operation; interrupted reservations remain uncertain.
func (s *Service) migrateIdempotencyHashes(ctx context.Context) error {
	for {
		rows, err := s.db.QueryContext(ctx, "SELECT id,input_hash FROM idempotency WHERE input_hash NOT LIKE 'hmac:v1:%' AND input_hash NOT LIKE 'legacy-encrypted:v1:%' LIMIT 256")
		if err != nil {
			return err
		}
		type entry struct{ ID, Hash string }
		batch := []entry{}
		for rows.Next() {
			var row entry
			if err = rows.Scan(&row.ID, &row.Hash); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, row)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, row := range batch {
			decoded, err := hex.DecodeString(row.Hash)
			if err != nil || len(decoded) != sha256.Size {
				return errors.New("invalid legacy idempotency fingerprint")
			}
			cipher, err := s.encrypt([]byte(jsonText(row)))
			if err != nil {
				return err
			}
			if _, err = s.db.ExecContext(ctx, "UPDATE idempotency SET input_hash=? WHERE id=? AND input_hash=?", "legacy-encrypted:v1:"+cipher, row.ID, row.Hash); err != nil {
				return err
			}
		}
	}
}
func (s *Service) safeExecuteOnce(ctx context.Context, a Actor, name string, input json.RawMessage) (data any, err error) {
	defer func() {
		if recover() != nil {
			data = nil
			err = &Error{Code: "operation_recovery_required", Message: "Operation interrupted unexpectedly; inspect affected resources before a new operation", Status: 409}
		}
	}()
	return s.executeOnce(ctx, a, name, input)
}
func (s *Service) cachedOperation(ctx context.Context, id string) (cachedOperation, bool, error) {
	var cached cachedOperation
	var resources string
	err := s.db.QueryRowContext(ctx, "SELECT input_hash,state,result,error,resources,task_id FROM idempotency WHERE id=?", id).Scan(&cached.Hash, &cached.State, &cached.Result, &cached.Error, &resources, &cached.TaskID)
	if err == sql.ErrNoRows {
		return cached, false, nil
	}
	if err != nil {
		return cached, false, internal()
	}
	if json.Unmarshal([]byte(resources), &cached.Resources) != nil {
		return cached, false, internal()
	}
	if strings.HasPrefix(cached.Result, "encrypted:v1:") {
		plain, e := s.decrypt(strings.TrimPrefix(cached.Result, "encrypted:v1:"))
		var envelope struct {
			ID    string `json:"id"`
			Value string `json:"value"`
		}
		if e != nil || json.Unmarshal(plain, &envelope) != nil || envelope.ID != id {
			return cached, false, internal()
		}
		cached.Result = envelope.Value
	}
	return cached, true, nil
}
func decodeCached(name string, cached cachedOperation) (any, error) {
	if cached.State == "failed" {
		var data struct {
			Code, Message string
			Status        int
			Details       any
		}
		if json.Unmarshal([]byte(cached.Error), &data) != nil {
			return nil, internal()
		}
		return nil, &Error{Code: data.Code, Message: data.Message, Status: data.Status, Details: data.Details}
	}
	if strings.HasPrefix(name, "notes_") && name != "notes_delete" {
		var note Note
		if json.Unmarshal([]byte(cached.Result), &note) == nil && note.ID != "" {
			return note, nil
		}
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(cached.Result))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return nil, internal()
	}
	return value, nil
}
func (s *Service) resolveActionResources(ctx context.Context, a Actor, name string, raw json.RawMessage) ([]string, error) {
	if binding, ok := s.binding(name); ok {
		return binding.Resources(ctx, a, raw)
	}
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	if name == "notes_create" {
		target, _ := fields["target_id"].(string)
		if target == "" {
			target = "host:local"
		}
		return []string{target}, nil
	}
	if strings.HasPrefix(name, "notes_") {
		id, _ := fields["id"].(string)
		note, e := s.noteGet(ctx, a, id)
		if e != nil {
			return nil, e
		}
		resources := []string{note.TargetID}
		if target, ok := fields["target_id"].(string); ok && target != note.TargetID {
			resources = append(resources, target)
		}
		return resources, nil
	}
	if strings.HasPrefix(name, "files_") {
		root, _ := fields["root"].(string)
		resources := []string{"files:" + root}
		if destination, ok := fields["destination_root"].(string); ok && destination != "" {
			resources = append(resources, "files:"+destination)
		}
		return resources, nil
	}
	if strings.HasPrefix(name, "tasks_") {
		id, _ := fields["id"].(string)
		j, e := s.jobs.Get(ctx, id)
		if e != nil {
			return nil, missing("Task")
		}
		return s.taskResources(ctx, j), nil
	}
	return []string{"host:local"}, nil
}
