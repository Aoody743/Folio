package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

func fixtureBinding(name, scope, resource string, execute func(context.Context, Actor, json.RawMessage) (ActionResult, error)) Binding {
	return Binding{Action: Action{Name: name, Description: "test trusted provider", Scope: scope, InputSchema: object(map[string]any{})}, Resources: func(context.Context, Actor, json.RawMessage) ([]string, error) { return []string{resource}, nil }, Execute: execute}
}

func TestProviderRegistryNestedSchemaAndDynamicScope(t *testing.T) {
	s, admin := testService(t)
	var effects atomic.Int32
	schema := object(map[string]any{"target": map[string]any{"type": "string", "minLength": 1, "maxLength": 16}, "rules": map[string]any{"type": "array", "minItems": 1, "maxItems": 3, "items": object(map[string]any{"mode": map[string]any{"type": "string", "enum": []string{"allow", "deny"}}, "port": map[string]any{"type": "integer", "minimum": 1, "maximum": 65535}}, "mode", "port")}}, "target", "rules")
	b := fixtureBinding("fixture_manage", "fixture:manage", "site:one", func(context.Context, Actor, json.RawMessage) (ActionResult, error) {
		effects.Add(1)
		return ActionResult{Data: map[string]any{"ok": true}, Resources: []string{"site:one"}}, nil
	})
	b.Action.InputSchema = schema
	if e := s.RegisterProvider(Bindings{b}); e != nil {
		t.Fatal(e)
	}
	if e := s.RegisterProvider(Bindings{b}); e == nil {
		t.Fatal("duplicate provider action accepted")
	}
	builtin := b
	builtin.Action.Name = "notes_create"
	if e := s.RegisterProvider(Bindings{builtin}); e == nil {
		t.Fatal("builtin overwritten")
	}
	token, e := s.CreateToken(context.Background(), admin, TokenRequest{Name: "scoped fixture", Scopes: []string{"fixture:manage", "audit:read"}, Resources: []string{"site:one"}})
	if e != nil {
		t.Fatal(e)
	}
	actor, _ := s.AuthenticateToken(context.Background(), token.Token)
	for _, raw := range []string{`{"target":"a","rules":[{"mode":"root","port":80}]}`, `{"target":"a","rules":[{"mode":"allow","port":0}]}`, `{"target":"a","rules":[{"mode":"allow","port":80,"actor":"root"}]}`, `{"target":"a","rules":[]}`, `{"target":"a","rules":[{"mode":"allow","port":80.5}]}`} {
		_, e = s.Execute(context.Background(), actor, b.Action.Name, json.RawMessage(raw))
		expectCode(t, e, "invalid_input")
	}
	if effects.Load() != 0 {
		t.Fatal("invalid nested input caused a side effect")
	}
	if _, e = s.Execute(context.Background(), actor, b.Action.Name, json.RawMessage(`{"target":"a","rules":[{"mode":"allow","port":80}]}`)); e != nil {
		t.Fatal(e)
	}
	rows := execute(t, s, actor, "audit_list", map[string]any{}).([]Audit)
	if len(rows) != 1 || rows[0].Resource != "site:one" {
		t.Fatalf("wrong scoped provider audit %+v", rows)
	}
	paths := s.OpenAPI(actor)["paths"].(map[string]any)
	if _, ok := paths["/api/v1/actions/fixture_manage"]; !ok {
		t.Fatal("provider missing from generated OpenAPI")
	}
	if _, ok := paths["/api/v1/actions/notes_delete"]; ok {
		t.Fatal("OpenAPI exposed unauthorized action")
	}
	s.SealRegistry()
	other := b
	other.Action.Name = "fixture_other"
	if e = s.RegisterProvider(Bindings{other}); e == nil {
		t.Fatal("late registration accepted")
	}
}

func TestProviderResourcesAuthorizeBeforeEffectAndValidateResult(t *testing.T) {
	s, admin := testService(t)
	var effects atomic.Int32
	b := fixtureBinding("fixture_write", "fixture:write", "site:private", func(context.Context, Actor, json.RawMessage) (ActionResult, error) {
		effects.Add(1)
		return ActionResult{Data: map[string]any{"ok": true}, Resources: []string{"site:private"}}, nil
	})
	if e := s.RegisterProvider(Bindings{b}); e != nil {
		t.Fatal(e)
	}
	token, e := s.CreateToken(context.Background(), admin, TokenRequest{Name: "limited", Scopes: []string{"fixture:write"}, Resources: []string{"site:allowed"}})
	if e != nil {
		t.Fatal(e)
	}
	actor, _ := s.AuthenticateToken(context.Background(), token.Token)
	_, e = s.Execute(context.Background(), actor, "fixture_write", nil)
	expectCode(t, e, "forbidden")
	if effects.Load() != 0 {
		t.Fatal("unauthorized effect ran")
	}
	bad := fixtureBinding("fixture_bad_result", "fixture:write", "site:allowed", func(context.Context, Actor, json.RawMessage) (ActionResult, error) {
		return ActionResult{Data: map[string]any{}, Resources: []string{"site:private"}}, nil
	})
	if e = s.RegisterProvider(Bindings{bad}); e != nil {
		t.Fatal(e)
	}
	_, e = s.Execute(context.Background(), actor, bad.Action.Name, nil)
	expectCode(t, e, "provider_contract")
}

func TestNamespaceMetadataCASAndTransactionRollback(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	state := s.Runtime("one").State
	other := s.Runtime("two").State
	revision, e := state.CompareAndSwap(ctx, "config", "a", 0, json.RawMessage(`{"value":1}`))
	if e != nil || revision != 1 {
		t.Fatal(e)
	}
	if _, _, e = other.GetVersioned(ctx, "config", "a"); ErrorCode(e) != "not_found" {
		t.Fatal("namespace leaked", e)
	}
	_, e = state.CompareAndSwap(ctx, "config", "a", 0, json.RawMessage(`{"value":2}`))
	expectCode(t, e, "revision_conflict")
	_, e = state.CompareAndSwap(ctx, "config", "a", 1, json.RawMessage(`{"value":2}`))
	if e != nil {
		t.Fatal(e)
	}
	e = state.Transaction(ctx, func(tx MetadataStore) error {
		if e := tx.Save(ctx, "config", "b", json.RawMessage(`{"value":3}`)); e != nil {
			return e
		}
		if _, e := tx.CompareAndSwap(ctx, "config", "a", 2, json.RawMessage(`{"value":4}`)); e != nil {
			return e
		}
		return fmt.Errorf("rollback")
	})
	if e == nil {
		t.Fatal("transaction unexpectedly committed")
	}
	if _, e = state.Get(ctx, "config", "b"); ErrorCode(e) != "not_found" {
		t.Fatal("failed transaction leaked record")
	}
	raw, revision, e := state.GetVersioned(ctx, "config", "a")
	if e != nil || revision != 2 || string(raw) != `{"value":2}` {
		t.Fatal(string(raw), revision, e)
	}
}

func TestBoundSecretsNamespaceResourceAndRevocation(t *testing.T) {
	s, admin := testService(t)
	ctx := context.Background()
	store := s.Runtime("secretsfixture").Secrets
	ref, e := store.Put(ctx, admin, "db:one", "password", []byte("fixture private credential"))
	if e != nil {
		t.Fatal(e)
	}
	token, e := s.CreateToken(ctx, admin, TokenRequest{Name: "limited", Scopes: []string{"system:read"}, Resources: []string{"db:one", "db:two"}})
	if e != nil {
		t.Fatal(e)
	}
	actor, _ := s.AuthenticateToken(ctx, token.Token)
	value, e := store.Get(ctx, actor, "db:one", ref)
	if e != nil || string(value) != "fixture private credential" {
		t.Fatal(e)
	}
	_, e = store.Get(ctx, actor, "db:two", ref)
	expectCode(t, e, "forbidden")
	_, e = s.Runtime("other").Secrets.Get(ctx, actor, "db:one", ref)
	expectCode(t, e, "not_found")
	var ciphertext string
	_ = s.db.QueryRow("SELECT ciphertext FROM provider_secrets WHERE ref=?", ref).Scan(&ciphertext)
	if ciphertext == string(value) {
		t.Fatal("secret stored unencrypted")
	}
	if e = s.RevokeToken(ctx, admin, token.ID); e != nil {
		t.Fatal(e)
	}
	_, e = store.Get(ctx, actor, "db:one", ref)
	expectCode(t, e, "unauthorized")
}

func TestProviderIdempotencyConcurrentInputBindingAndRestart(t *testing.T) {
	s, admin := testService(t)
	ctx := context.Background()
	marker := filepath.Join(s.dataDir, "side-effect.txt")
	var effects atomic.Int32
	b := fixtureBinding("fixture_effect", "fixture:write", "site:one", func(context.Context, Actor, json.RawMessage) (ActionResult, error) {
		effects.Add(1)
		file, e := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if e != nil {
			return ActionResult{}, e
		}
		_, e = file.WriteString("effect\n")
		file.Close()
		if e != nil {
			return ActionResult{}, e
		}
		time.Sleep(20 * time.Millisecond)
		return ActionResult{Data: map[string]any{"ok": true}, Resources: []string{"site:one"}}, nil
	})
	b.Action.InputSchema = object(map[string]any{"value": integer()}, "value")
	if e := s.RegisterProvider(Bindings{b}); e != nil {
		t.Fatal(e)
	}
	token, e := s.CreateToken(ctx, admin, TokenRequest{Name: "effects", Scopes: []string{"fixture:write"}, Resources: []string{"site:one"}})
	if e != nil {
		t.Fatal(e)
	}
	actor, _ := s.AuthenticateToken(ctx, token.Token)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.ExecuteWithKey(ctx, actor, b.Action.Name, json.RawMessage(`{"value":1}`), "one-intent-0001")
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if effects.Load() != 1 {
		t.Fatalf("duplicate effects: %d", effects.Load())
	}
	_, e = s.ExecuteWithKey(ctx, actor, b.Action.Name, json.RawMessage(`{"value":2}`), "one-intent-0001")
	expectCode(t, e, "idempotency_conflict")
	dir := s.dataDir
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	restart, e := New(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer restart.Close()
	if e = restart.RegisterProvider(Bindings{b}); e != nil {
		t.Fatal(e)
	}
	if _, e = restart.ExecuteWithKey(ctx, actor, b.Action.Name, json.RawMessage(`{"value":1}`), "one-intent-0001"); e != nil {
		t.Fatal(e)
	}
	if effects.Load() != 1 {
		t.Fatal("restart replayed side effect")
	}
	if e = restart.RevokeToken(ctx, admin, token.ID); e != nil {
		t.Fatal(e)
	}
	_, e = restart.ExecuteWithKey(ctx, actor, b.Action.Name, json.RawMessage(`{"value":1}`), "one-intent-0001")
	expectCode(t, e, "unauthorized")
}

type fixtureExecutor struct{ inner *agent.FileExecutor }

func (e fixtureExecutor) Validate(r agent.Request) error {
	r.Operation = "file_write"
	return e.inner.Validate(r)
}
func (e fixtureExecutor) Run(ctx context.Context, r agent.Request) (agent.Result, error) {
	r.Operation = "file_write"
	return e.inner.Run(ctx, r)
}

func TestProviderTaskResourcesPersistAndFileAliasAudit(t *testing.T) {
	s, admin := testService(t)
	ctx := context.Background()
	deps := s.Runtime("taskfixture")
	if e := s.RegisterExecutor([]string{"fixture_file_write"}, fixtureExecutor{agent.NewFileExecutor(s.filesystem)}); e != nil {
		t.Fatal(e)
	}
	b := fixtureBinding("fixture_task", "fixture:write", "fs:workspace", func(ctx context.Context, a Actor, input json.RawMessage) (ActionResult, error) {
		job, e := deps.Tasks.Submit(ctx, a, "fixture_file_write", json.RawMessage(`{"root":"workspace","path":"provider.txt","content":"provider real task"}`), []string{"fs:workspace"})
		if e != nil {
			return ActionResult{}, e
		}
		return ActionResult{Data: job, TaskID: job["id"].(string), Resources: []string{"fs:workspace"}}, nil
	})
	b.Action.Task = true
	if e := s.RegisterProvider(Bindings{b}); e != nil {
		t.Fatal(e)
	}
	token, e := s.CreateToken(ctx, admin, TokenRequest{Name: "task", Scopes: []string{"fixture:write", "tasks:read", "audit:read"}, Resources: []string{"files:workspace"}})
	if e != nil {
		t.Fatal(e)
	}
	actor, _ := s.AuthenticateToken(ctx, token.Token)
	result := execute(t, s, actor, b.Action.Name, map[string]any{}).(map[string]any)
	id := result["id"].(string)
	deadline := time.Now().Add(5 * time.Second)
	for {
		value := execute(t, s, actor, "tasks_get", map[string]any{"id": id}).(map[string]any)
		if value["status"] == "succeeded" {
			if value["resource"] != "fs:workspace" {
				t.Fatal(value)
			}
			break
		}
		if value["status"] == "failed" {
			t.Fatal(value)
		}
		if time.Now().After(deadline) {
			t.Fatal("task did not complete")
		}
		time.Sleep(5 * time.Millisecond)
	}
	content, e := os.ReadFile(filepath.Join(s.dataDir, "workspace", "provider.txt"))
	if e != nil || string(content) != "provider real task" {
		t.Fatal(string(content), e)
	}
	audit := execute(t, s, actor, "audit_list", map[string]any{}).([]Audit)
	found := false
	for _, row := range audit {
		if row.Action == b.Action.Name && row.Resource == "fs:workspace" {
			found = true
		}
	}
	if !found {
		t.Fatal("alias-scoped actor cannot see actual provider task audit")
	}
}

func TestInterruptedIdempotencyNeverReexecutesAfterRestart(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	var effects atomic.Int32
	b := fixtureBinding("fixture_interrupted", "fixture:write", "site:one", func(context.Context, Actor, json.RawMessage) (ActionResult, error) {
		effects.Add(1)
		return ActionResult{Data: map[string]any{"ok": true}}, nil
	})
	if e := s.RegisterProvider(Bindings{b}); e != nil {
		t.Fatal(e)
	}
	key := "interrupted-intent"
	id := digest(jsonText([]string{a.ID, b.Action.Name, key}))
	_, e := s.db.Exec("INSERT INTO idempotency(id,actor_id,action,input_hash,state,resources,created_at,updated_at) VALUES(?,?,?,?,'running',?,?,?)", id, a.ID, b.Action.Name, digest("{}"), jsonText([]string{"site:one"}), nowString(), nowString())
	if e != nil {
		t.Fatal(e)
	}
	dir := s.dataDir
	s.Close()
	restart, e := New(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer restart.Close()
	if e = restart.RegisterProvider(Bindings{b}); e != nil {
		t.Fatal(e)
	}
	_, e = restart.ExecuteWithKey(ctx, a, b.Action.Name, json.RawMessage(`{}`), key)
	expectCode(t, e, "operation_recovery_required")
	if effects.Load() != 0 {
		t.Fatal("interrupted side effect replayed")
	}
}
