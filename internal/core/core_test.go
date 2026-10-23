package core

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testService(t *testing.T) (*Service, Actor) {
	t.Helper()
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	secret, e := os.ReadFile(s.SetupSecretFile())
	if e != nil {
		t.Fatal(e)
	}
	a, e := s.Setup(context.Background(), strings.TrimSpace(string(secret)), "admin", "a secure long password")
	if e != nil {
		t.Fatal(e)
	}
	return s, a
}
func execute(t *testing.T, s *Service, a Actor, name string, in any) any {
	t.Helper()
	raw, _ := json.Marshal(in)
	out, e := s.Execute(context.Background(), a, name, raw)
	if e != nil {
		t.Fatalf("%s: %v", name, e)
	}
	return out
}
func expectCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil || ErrorCode(err) != code {
		t.Fatalf("wanted %s, got %v", code, err)
	}
}

func TestBootstrapIsLocalSecretAndSingleUse(t *testing.T) {
	dataDir := t.TempDir()
	s, e := New(dataDir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	keyPath := filepath.Join(dataDir, "encryption-key")
	keyBefore, e := os.ReadFile(keyPath)
	if e != nil {
		t.Fatal(e)
	}
	keyInfo, e := os.Stat(keyPath)
	if e != nil || keyInfo.Mode().Perm() != 0600 {
		t.Fatal("master key was not created with private 0600 permissions", keyInfo, e)
	}
	_, e = s.Setup(context.Background(), "attacker", "admin", "a secure long password")
	expectCode(t, e, "unauthorized")
	secret, _ := os.ReadFile(s.SetupSecretFile())
	a, e := s.Setup(context.Background(), strings.TrimSpace(string(secret)), "admin", "a secure long password")
	if e != nil || a.ID == "" {
		t.Fatal(e)
	}
	_, e = s.Setup(context.Background(), strings.TrimSpace(string(secret)), "admin2", "a secure long password")
	expectCode(t, e, "already_initialized")
	var encoded string
	if e = s.db.QueryRow("SELECT password_hash FROM users").Scan(&encoded); e != nil {
		t.Fatal(e)
	}
	if !strings.HasPrefix(encoded, "$argon2id$") {
		t.Fatalf("weak password format %q", encoded)
	}
	info, _ := os.Stat(filepath.Join(s.dataDir, "powerlinux.db"))
	if info.Mode().Perm() != 0600 {
		t.Fatalf("database exposed mode %v", info.Mode())
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	reopened, e := New(dataDir)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	keyAfter, e := os.ReadFile(keyPath)
	if e != nil || !bytes.Equal(keyBefore, keyAfter) || !bytes.Equal(reopened.secret, s.secret) {
		t.Fatal("reopening replaced or failed to load the stable master key", e)
	}
	keyInfo, e = os.Stat(keyPath)
	if e != nil || keyInfo.Mode().Perm() != 0600 {
		t.Fatal("reopened master key did not retain private 0600 permissions", keyInfo, e)
	}
}

func TestSessionExpiryDoesNotRenewAndBadLoginDoesNotDisableServer(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		_, e := s.Login(ctx, "nonexistent", "wrong", "", "attacker")
		if i < 8 {
			expectCode(t, e, "unauthorized")
		} else {
			expectCode(t, e, "rate_limited")
		}
	}
	session, e := s.Login(ctx, a.Name, "a secure long password", "", "legitimate")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.AuthenticateSession(ctx, session.ID); e != nil {
		t.Fatal(e)
	}
	_, e = s.db.Exec("UPDATE sessions SET expires=? WHERE hash=?", time.Now().Unix()-1, digest(session.ID))
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.AuthenticateSession(ctx, session.ID)
	expectCode(t, e, "unauthorized")
	_, e = s.AuthenticateSession(ctx, session.ID)
	expectCode(t, e, "unauthorized")
	if got := execute(t, s, a, "system_get_status", map[string]any{}).(SystemStatus); got.Hostname == "" || got.UpdatedAt == "" {
		t.Fatalf("missing real status %+v", got)
	}
}

func TestTOTPIsRequiredAndReplayIsRejected(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	enroll, e := s.BeginTOTP(ctx, a, "a secure long password", "")
	if e != nil {
		t.Fatal(e)
	}
	key, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enroll.Secret)
	now := time.Now()
	code := totpCode(key, now.Unix()/30)
	if e = s.ConfirmTOTP(ctx, a, code); e != nil {
		t.Fatal(e)
	}
	_, e = s.Login(ctx, a.Name, "a secure long password", "", "source")
	expectCode(t, e, "unauthorized")
	_, e = s.Login(ctx, a.Name, "a secure long password", code, "source")
	expectCode(t, e, "unauthorized") // enrollment code is already used
	next := totpCode(key, now.Unix()/30+1)
	if _, e = s.Login(ctx, a.Name, "a secure long password", next, "source"); e != nil {
		t.Fatal(e)
	}
	_, e = s.Login(ctx, a.Name, "a secure long password", next, "source")
	expectCode(t, e, "unauthorized")
	if e = s.DisableTOTP(ctx, a, "a secure long password", ""); e == nil {
		t.Fatal("MFA disabled without second factor")
	}
	var ciphertext string
	_ = s.db.QueryRow("SELECT totp_secret FROM users").Scan(&ciphertext)
	if strings.Contains(ciphertext, enroll.Secret) || ciphertext == enroll.Secret {
		t.Fatal("unencrypted MFA secret")
	}
}

func TestScopedTokensAndRevocation(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	token, e := s.CreateToken(ctx, a, TokenRequest{Name: "assistant", Scopes: []string{"notes:read", "notes:write"}, Resources: []string{"host:local"}})
	if e != nil {
		t.Fatal(e)
	}
	ai, e := s.AuthenticateToken(ctx, token.Token)
	if e != nil {
		t.Fatal(e)
	}
	n := execute(t, s, ai, "notes_create", map[string]any{"title": "AI note"}).(Note)
	if n.CreatedBy.ID != ai.ID || n.CreatedBy.Kind != "mcp" {
		t.Fatal("false author")
	}
	_, e = s.CreateToken(ctx, ai, TokenRequest{Name: "escalated", Scopes: []string{"*"}, Resources: []string{"*"}})
	expectCode(t, e, "forbidden")
	_, e = s.Execute(ctx, ai, "notes_create", json.RawMessage(`{"title":"escape","target_id":"site:private"}`))
	expectCode(t, e, "forbidden")
	_, e = s.Execute(ctx, ai, "notes_delete", json.RawMessage(`{"id":"anything","expected_revision":1}`))
	expectCode(t, e, "forbidden")
	if e = s.RevokeToken(ctx, a, token.ID); e != nil {
		t.Fatal(e)
	}
	_, e = s.AuthenticateToken(ctx, token.Token)
	expectCode(t, e, "unauthorized")
}

func TestAudienceBoundTokensAndOwnRevocation(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	token, e := s.CreateToken(ctx, a, TokenRequest{Name: "oauth", Scopes: []string{"system:read"}, Resources: []string{"host:local"}, Audience: "https://panel.example/mcp"})
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.AuthenticateToken(ctx, token.Token)
	expectCode(t, e, "unauthorized")
	_, e = s.AuthenticateTokenForAudience(ctx, token.Token, "https://other.example/mcp")
	expectCode(t, e, "unauthorized")
	if _, e = s.AuthenticateTokenForAudience(ctx, token.Token, "https://panel.example/mcp"); e != nil {
		t.Fatal(e)
	}
	if e = s.RevokeOwnToken(ctx, token.Token, "https://other.example/mcp"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AuthenticateTokenForAudience(ctx, token.Token, "https://panel.example/mcp"); e != nil {
		t.Fatal("wrong audience revoked legitimate token")
	}
	if e = s.RevokeOwnToken(ctx, token.Token, "https://panel.example/mcp"); e != nil {
		t.Fatal(e)
	}
	_, e = s.AuthenticateTokenForAudience(ctx, token.Token, "https://panel.example/mcp")
	expectCode(t, e, "unauthorized")
}

func TestNotesConcurrencyHistoryRestoreAndScopedLists(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	n := execute(t, s, a, "notes_create", map[string]any{"title": "Shared", "content": "Original"}).(Note)
	token, e := s.CreateToken(ctx, a, TokenRequest{Name: "AI", Scopes: []string{"notes:read", "notes:write"}, Resources: []string{"host:local"}})
	if e != nil {
		t.Fatal(e)
	}
	ai, _ := s.AuthenticateToken(ctx, token.Token)
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for _, actor := range []Actor{a, ai} {
		wg.Add(1)
		go func(actor Actor) {
			defer wg.Done()
			raw, _ := json.Marshal(map[string]any{"id": n.ID, "expected_revision": 1, "content": actor.Name})
			_, e := s.Execute(ctx, actor, "notes_update", raw)
			errors <- e
		}(actor)
	}
	wg.Wait()
	close(errors)
	successes, conflicts := 0, 0
	for e := range errors {
		if e == nil {
			successes++
		} else if ErrorCode(e) == "revision_conflict" {
			conflicts++
		} else {
			t.Fatal(e)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent writes were not protected successes=%d conflicts=%d", successes, conflicts)
	}
	updated := execute(t, s, a, "notes_get", map[string]any{"id": n.ID}).(Note)
	if updated.Revision != 2 {
		t.Fatal(updated.Revision)
	}
	archived := execute(t, s, a, "notes_archive", map[string]any{"id": n.ID, "expected_revision": 2}).(Note)
	if !archived.Archived {
		t.Fatal("not archived")
	}
	restored := execute(t, s, ai, "notes_restore", map[string]any{"id": n.ID, "expected_revision": 3, "revision": 1}).(Note)
	if restored.Revision != 4 || restored.Content != "Original" || restored.Archived || restored.UpdatedBy.ID != ai.ID {
		t.Fatal(restored)
	}
	history := execute(t, s, a, "notes_history", map[string]any{"id": n.ID}).([]Note)
	if len(history) != 4 {
		t.Fatalf("lost history: %d", len(history))
	}
	private := execute(t, s, a, "notes_create", map[string]any{"title": "Private", "target_id": "site:private"}).(Note)
	_, e = s.Execute(ctx, ai, "notes_get", json.RawMessage(`{"id":"`+private.ID+`"}`))
	expectCode(t, e, "forbidden")
	list := execute(t, s, ai, "notes_list", map[string]any{}).([]Note)
	if len(list) != 1 || list[0].ID != n.ID {
		t.Fatal("resource filtering leaked note")
	}
	_, e = s.Execute(ctx, ai, "notes_create", json.RawMessage(`{"title":"spoof","created_by":{"kind":"human"}}`))
	expectCode(t, e, "invalid_input")
}

func TestDurableRealFileJobsAndCrossRootDenial(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	token, e := s.CreateToken(ctx, a, TokenRequest{Name: "files", Scopes: []string{"files:write", "files:read", "tasks:read", "tasks:write"}, Resources: []string{"files:workspace"}})
	if e != nil {
		t.Fatal(e)
	}
	ai, _ := s.AuthenticateToken(ctx, token.Token)
	created := execute(t, s, ai, "files_write", map[string]any{"root": "workspace", "path": "hello.txt", "content": "real persistent content"}).(map[string]any)
	id := created["id"].(string)
	deadline := time.Now().Add(5 * time.Second)
	for {
		out := execute(t, s, ai, "tasks_get", map[string]any{"id": id}).(map[string]any)
		status := out["status"].(string)
		if status == "succeeded" {
			break
		}
		if status == "failed" {
			t.Fatal(out)
		}
		if time.Now().After(deadline) {
			t.Fatal("file task timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
	content, e := os.ReadFile(filepath.Join(s.dataDir, "workspace", "hello.txt"))
	if e != nil || string(content) != "real persistent content" {
		t.Fatal(string(content), e)
	}
	_, e = s.Execute(ctx, ai, "files_backup", json.RawMessage(`{"root":"workspace","path":"hello.txt","destination_root":"private","destination_path":"secret.txt"}`))
	expectCode(t, e, "forbidden")
	_, e = s.Execute(ctx, ai, "files_write", json.RawMessage(`{"root":"workspace","path":"../powerlinux.db","content":"damage"}`))
	expectCode(t, e, "forbidden")
	dir := s.dataDir
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	restarted, e := New(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer restarted.Close()
	out := execute(t, restarted, ai, "tasks_get", map[string]any{"id": id}).(map[string]any)
	if out["status"] != "succeeded" {
		t.Fatal("lost persistent task")
	}
	_, e = restarted.AuthenticateToken(ctx, token.Token)
	if e != nil {
		t.Fatal("lost persistent token")
	}
}

func TestMaliciousHTMLIsStoredOnlyAsPlainJSONData(t *testing.T) {
	s, a := testService(t)
	payload := `<img src=x onerror=alert(document.cookie)>`
	n := execute(t, s, a, "notes_create", map[string]any{"title": payload, "content": payload}).(Note)
	if n.Content != payload {
		t.Fatal("unexpected note transformation")
	}
	b, _ := json.Marshal(n)
	if strings.Contains(string(b), "<img") {
		t.Fatal("raw HTML in serialized response")
	}
}

func TestNoteMutationRollsBackWhenAuditCannotBeWritten(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	note := execute(t, s, a, "notes_create", map[string]any{"title": "original"}).(Note)
	if _, e := s.db.Exec(`CREATE TRIGGER refuse_audit BEFORE INSERT ON audit BEGIN SELECT RAISE(FAIL, 'simulated audit disk failure'); END`); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(map[string]any{"id": note.ID, "expected_revision": 1, "title": "must not commit"})
	_, e := s.Execute(ctx, a, "notes_update", raw)
	expectCode(t, e, "internal_error")
	current, e := s.noteGet(ctx, a, note.ID)
	if e != nil {
		t.Fatal(e)
	}
	if current.Revision != 1 || current.Title != "original" {
		t.Fatal("unaudited mutation was committed")
	}
	var historyCount int
	if e = s.db.QueryRow("SELECT count(*) FROM note_history WHERE note_id=?", note.ID).Scan(&historyCount); e != nil {
		t.Fatal(e)
	}
	if historyCount != 1 {
		t.Fatal("failed write left history")
	}
}

func TestDelegatedTokenLeaseAudienceAndCascadingRevocation(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	parent, e := s.CreateToken(ctx, a, TokenRequest{Name: "delegator", Scopes: []string{"tokens:manage", "notes:read"}, Resources: []string{"host:local"}, ExpiresInSeconds: 600, Audience: "https://panel.example/mcp"})
	if e != nil {
		t.Fatal(e)
	}
	delegator, e := s.AuthenticateTokenForAudience(ctx, parent.Token, "https://panel.example/mcp")
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.CreateToken(ctx, delegator, TokenRequest{Name: "escape audience", Scopes: []string{"notes:read"}, Resources: []string{"host:local"}})
	expectCode(t, e, "forbidden")
	child, e := s.CreateToken(ctx, delegator, TokenRequest{Name: "child", Scopes: []string{"tokens:manage", "notes:read"}, Resources: []string{"host:local"}, Audience: "https://panel.example/mcp", ExpiresInSeconds: 3600})
	if e != nil {
		t.Fatal(e)
	}
	parentExpiry, _ := time.Parse(time.RFC3339, parent.ExpiresAt)
	childExpiry, _ := time.Parse(time.RFC3339, child.ExpiresAt)
	if childExpiry.After(parentExpiry) {
		t.Fatal("delegation outlived parent lease")
	}
	childActor, e := s.AuthenticateTokenForAudience(ctx, child.Token, "https://panel.example/mcp")
	if e != nil {
		t.Fatal(e)
	}
	grandchild, e := s.CreateToken(ctx, childActor, TokenRequest{Name: "grandchild", Scopes: []string{"notes:read"}, Resources: []string{"host:local"}, Audience: "https://panel.example/mcp"})
	if e != nil {
		t.Fatal(e)
	}
	sibling, e := s.CreateToken(ctx, a, TokenRequest{Name: "sibling", Scopes: []string{"*"}, Resources: []string{"*"}})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.RevokeToken(ctx, delegator, sibling.ID); e == nil {
		t.Fatal("scoped token revoked unrelated administrator identity")
	}
	visible, e := s.ListTokens(ctx, delegator)
	if e != nil {
		t.Fatal(e)
	}
	if len(visible) != 3 {
		t.Fatalf("delegator saw unrelated credentials: %+v", visible)
	}
	if e = s.RevokeToken(ctx, a, parent.ID); e != nil {
		t.Fatal(e)
	}
	for _, token := range []TokenCreated{parent, child, grandchild} {
		_, e = s.AuthenticateTokenForAudience(ctx, token.Token, "https://panel.example/mcp")
		expectCode(t, e, "unauthorized")
	}
	if _, e = s.AuthenticateToken(ctx, sibling.Token); e != nil {
		t.Fatal("cascade revoked unrelated identity")
	}
}

func TestMixedWildcardResourcePaginationAndSchemaNullReject(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	execute(t, s, a, "notes_create", map[string]any{"title": "private", "target_id": "site:private"})
	a.Resources = []string{"host:local", "*"}
	list := execute(t, s, a, "notes_list", map[string]any{}).([]Note)
	if len(list) != 1 {
		t.Fatal("wildcard mixed resource list failed")
	}
	execute(t, s, a, "audit_list", map[string]any{})
	_, e := s.Execute(ctx, a, "notes_create", json.RawMessage(`{"title":null}`))
	expectCode(t, e, "invalid_input")
	_, e = s.Execute(ctx, a, "files_write", json.RawMessage(`{"root":"workspace","path":"missing.txt"}`))
	expectCode(t, e, "invalid_input")
	a.Resources = []string{"site:*"}
	list = execute(t, s, a, "notes_list", map[string]any{}).([]Note)
	if len(list) != 1 {
		t.Fatal("namespace wildcard lost authorized data")
	}
}

func TestReadPollingDoesNotWriteAuditAndTokenLastUsedIsThrottled(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	token, e := s.CreateToken(ctx, a, TokenRequest{Name: "monitor", Scopes: []string{"system:read"}, Resources: []string{"host:local"}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec(`CREATE TABLE token_touch_count(n INTEGER); INSERT INTO token_touch_count VALUES(0); CREATE TRIGGER count_token_touch AFTER UPDATE OF last_used ON tokens BEGIN UPDATE token_touch_count SET n=n+1; END`); e != nil {
		t.Fatal(e)
	}
	var before int
	_ = s.db.QueryRow("SELECT count(*) FROM audit").Scan(&before)
	for i := 0; i < 30; i++ {
		monitor, e := s.AuthenticateToken(ctx, token.Token)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.Execute(ctx, monitor, "system_get_status", nil); e != nil {
			t.Fatal(e)
		}
	}
	var touches, after int
	_ = s.db.QueryRow("SELECT n FROM token_touch_count").Scan(&touches)
	_ = s.db.QueryRow("SELECT count(*) FROM audit").Scan(&after)
	if touches != 1 {
		t.Fatalf("last_used produced %d writes instead of one", touches)
	}
	if after != before {
		t.Fatalf("polling generated %d synchronous audit records", after-before)
	}
	if e = s.RevokeToken(ctx, a, token.ID); e != nil {
		t.Fatal(e)
	}
	_, e = s.AuthenticateToken(ctx, token.Token)
	expectCode(t, e, "unauthorized")
}

func TestRuntimeWorkspaceCannotBeRedirectedOutsideDataDirectory(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if e := os.Symlink(outside, filepath.Join(dir, "workspace")); e != nil {
		t.Fatal(e)
	}
	if service, e := New(dir); e == nil {
		service.Close()
		t.Fatal("symlinked runtime workspace accepted")
	}
	entries, e := os.ReadDir(outside)
	if e != nil {
		t.Fatal(e)
	}
	if len(entries) != 0 {
		t.Fatal("failed initialization wrote outside controlled workspace")
	}
}

func TestRecentNotesOrderDoesNotPromoteOlderPinnedNotes(t *testing.T) {
	s, a := testService(t)
	defer s.Close()
	var oldest Note
	for i, title := range []string{"pinned older", "recent one", "recent two", "recent three"} {
		note := execute(t, s, a, "notes_create", map[string]any{"title": title, "pinned": i == 0}).(Note)
		if i == 0 {
			oldest = note
		}
		stamp := []string{"2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z", "2026-01-03T00:00:00Z", "2026-01-04T00:00:00Z"}[i]
		if _, err := s.db.Exec("UPDATE notes SET updated_at=? WHERE id=?", stamp, note.ID); err != nil {
			t.Fatal(err)
		}
	}
	recent := execute(t, s, a, "notes_list", map[string]any{"limit": 3, "sort": "updated_at"}).([]Note)
	if len(recent) != 3 || recent[0].Title != "recent three" {
		t.Fatal(recent)
	}
	for _, note := range recent {
		if note.ID == oldest.ID {
			t.Fatal("pinned note displaced recent note")
		}
	}
	defaults := execute(t, s, a, "notes_list", map[string]any{"limit": 1}).([]Note)
	if defaults[0].ID != oldest.ID {
		t.Fatal("default pinned ordering changed")
	}
}
