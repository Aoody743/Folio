package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

type sqlState interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type stateStore struct {
	s         *Service
	namespace string
	tx        *sql.Tx
}

func (store stateStore) conn() sqlState {
	if store.tx != nil {
		return store.tx
	}
	return store.s.db
}
func validStateKey(kind, id string) bool {
	return kind != "" && id != "" && len(kind) <= 128 && len(id) <= 512 && utf8.ValidString(kind) && utf8.ValidString(id) && !strings.ContainsAny(kind+id, "\x00")
}
func validStateData(data json.RawMessage) bool { return len(data) <= 1<<20 && json.Valid(data) }

// Save is for trusted bookkeeping. User-configurable mutable records should
// use GetVersioned/CompareAndSwap to refuse stale edits.
func (store stateStore) Save(ctx context.Context, kind, id string, data json.RawMessage) error {
	if !validStateKey(kind, id) || !validStateData(data) {
		return invalid("Invalid metadata key or JSON value")
	}
	_, err := store.conn().ExecContext(ctx, `INSERT INTO provider_state(namespace,kind,id,payload,revision,updated_at) VALUES(?,?,?,?,1,?) ON CONFLICT(namespace,kind,id) DO UPDATE SET payload=excluded.payload,revision=revision+1,updated_at=excluded.updated_at`, store.namespace, kind, id, string(data), nowString())
	if err != nil {
		return internal()
	}
	return nil
}
func (store stateStore) Get(ctx context.Context, kind, id string) (json.RawMessage, error) {
	raw, _, err := store.GetVersioned(ctx, kind, id)
	return raw, err
}
func (store stateStore) GetVersioned(ctx context.Context, kind, id string) (json.RawMessage, int64, error) {
	if !validStateKey(kind, id) {
		return nil, 0, invalid("Invalid metadata key")
	}
	var text string
	var revision int64
	if err := store.conn().QueryRowContext(ctx, "SELECT payload,revision FROM provider_state WHERE namespace=? AND kind=? AND id=?", store.namespace, kind, id).Scan(&text, &revision); err == sql.ErrNoRows {
		return nil, 0, missing("Metadata")
	} else if err != nil {
		return nil, 0, internal()
	}
	return json.RawMessage(text), revision, nil
}
func (store stateStore) List(ctx context.Context, kind string) (map[string]json.RawMessage, error) {
	if !validStateKey(kind, "list") {
		return nil, invalid("Invalid metadata kind")
	}
	rows, err := store.conn().QueryContext(ctx, "SELECT id,payload FROM provider_state WHERE namespace=? AND kind=? ORDER BY id LIMIT 10001", store.namespace, kind)
	if err != nil {
		return nil, internal()
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	total := 0
	for rows.Next() {
		var id, text string
		if err = rows.Scan(&id, &text); err != nil {
			return nil, internal()
		}
		total += len(text)
		if len(out) >= 10000 || total > 32<<20 {
			return nil, &Error{Code: "metadata_limit", Message: "Metadata collection requires bounded pagination", Status: 413}
		}
		out[id] = json.RawMessage(text)
	}
	if rows.Err() != nil {
		return nil, internal()
	}
	return out, nil
}
func (store stateStore) Delete(ctx context.Context, kind, id string) error {
	if !validStateKey(kind, id) {
		return invalid("Invalid metadata key")
	}
	_, err := store.conn().ExecContext(ctx, "DELETE FROM provider_state WHERE namespace=? AND kind=? AND id=?", store.namespace, kind, id)
	if err != nil {
		return internal()
	}
	return nil
}
func (store stateStore) CompareAndSwap(ctx context.Context, kind, id string, expected int64, data json.RawMessage) (int64, error) {
	if !validStateKey(kind, id) || !validStateData(data) || expected < 0 {
		return 0, invalid("Invalid versioned metadata mutation")
	}
	var result sql.Result
	var err error
	if expected == 0 {
		result, err = store.conn().ExecContext(ctx, "INSERT INTO provider_state(namespace,kind,id,payload,revision,updated_at) VALUES(?,?,?,?,1,?) ON CONFLICT(namespace,kind,id) DO NOTHING", store.namespace, kind, id, string(data), nowString())
	} else {
		result, err = store.conn().ExecContext(ctx, "UPDATE provider_state SET payload=?,revision=revision+1,updated_at=? WHERE namespace=? AND kind=? AND id=? AND revision=?", string(data), nowString(), store.namespace, kind, id, expected)
	}
	if err != nil {
		return 0, internal()
	}
	rows, _ := result.RowsAffected()
	if rows == 1 {
		return expected + 1, nil
	}
	_, current, getErr := store.GetVersioned(ctx, kind, id)
	if getErr != nil {
		current = 0
	}
	return current, &Error{Code: "revision_conflict", Message: "Metadata changed; reload its current revision", Status: 412, Details: map[string]any{"current_revision": current}}
}
func (store stateStore) Transaction(ctx context.Context, fn func(MetadataStore) error) error {
	if fn == nil {
		return invalid("Transaction callback required")
	}
	if store.tx != nil {
		return fn(store)
	}
	tx, err := store.s.db.BeginTx(ctx, nil)
	if err != nil {
		return internal()
	}
	defer tx.Rollback()
	store.tx = tx
	if err = fn(store); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return internal()
	}
	return nil
}

type boundSecrets struct {
	s         *Service
	namespace string
}
type secretEnvelope struct {
	Namespace string `json:"namespace"`
	Resource  string `json:"resource"`
	Value     []byte `json:"value"`
}

func (store boundSecrets) actor(ctx context.Context, prior Actor, resource string) (Actor, error) {
	a, err := store.s.RefreshActor(ctx, prior)
	if err != nil {
		return Actor{}, err
	}
	if resource == "" || resource == "*" || len(resource) > 512 || !hasResource(a, resource) {
		return Actor{}, denied()
	}
	return a, nil
}
func (store boundSecrets) Put(ctx context.Context, prior Actor, resource, name string, value []byte) (string, error) {
	a, err := store.actor(ctx, prior, resource)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(name) == "" || len(name) > 128 || len(value) == 0 || len(value) > 256<<10 {
		return "", invalid("Secret label/value exceeds limits")
	}
	ref := randomID("sec_")
	plain, _ := json.Marshal(secretEnvelope{store.namespace, resource, value})
	ciphertext, err := store.s.encrypt(plain)
	if err != nil {
		return "", internal()
	}
	_, err = store.s.db.ExecContext(ctx, "INSERT INTO provider_secrets(ref,namespace,resource,name,ciphertext,created_at) VALUES(?,?,?,?,?,?)", ref, store.namespace, resource, name, ciphertext, nowString())
	if err != nil {
		return "", internal()
	}
	store.s.recordAudit(ctx, a, "credentials_store", resource, nil, map[string]any{"secret_ref": ref, "label": name})
	return ref, nil
}
func (store boundSecrets) Get(ctx context.Context, prior Actor, resource, ref string) ([]byte, error) {
	a, err := store.actor(ctx, prior, resource)
	if err != nil {
		return nil, err
	}
	var ciphertext, storedResource string
	err = store.s.db.QueryRowContext(ctx, "SELECT ciphertext,resource FROM provider_secrets WHERE ref=? AND namespace=?", ref, store.namespace).Scan(&ciphertext, &storedResource)
	if err == sql.ErrNoRows {
		return nil, missing("Secret reference")
	}
	if err != nil {
		return nil, internal()
	}
	if storedResource != resource {
		return nil, denied()
	}
	raw, err := store.s.decrypt(ciphertext)
	if err != nil {
		return nil, internal()
	}
	var envelope secretEnvelope
	if json.Unmarshal(raw, &envelope) != nil || envelope.Namespace != store.namespace || envelope.Resource != resource {
		return nil, internal()
	}
	store.s.recordAudit(ctx, a, "credentials_resolve", resource, nil, map[string]any{"secret_ref": ref})
	return envelope.Value, nil
}
func (store boundSecrets) Delete(ctx context.Context, prior Actor, resource, ref string) error {
	a, err := store.actor(ctx, prior, resource)
	if err != nil {
		return err
	}
	result, err := store.s.db.ExecContext(ctx, "DELETE FROM provider_secrets WHERE ref=? AND namespace=? AND resource=?", ref, store.namespace, resource)
	if err != nil {
		return internal()
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return missing("Secret reference")
	}
	store.s.recordAudit(ctx, a, "credentials_delete", resource, nil, map[string]any{"secret_ref": ref})
	return nil
}
