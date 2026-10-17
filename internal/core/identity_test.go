package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"os"
	"strings"
	"sync"
	"testing"
)

const identityPassword = "a secure long password"

func TestPanelBrandingCASAndAccountRenamePreserveIdentity(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	session, e := s.Login(ctx, "admin", identityPassword, "", "test")
	if e != nil {
		t.Fatal(e)
	}
	panel := execute(t, s, a, "settings_branding_update", map[string]any{"panel_name": "<script>example</script>", "footer": "plain footer", "expected_revision": 0}).(PanelSettings)
	if panel.Revision != 1 {
		t.Fatal(panel)
	}
	_, e = s.Execute(ctx, a, "settings_branding_update", json.RawMessage(`{"panel_name":"stale","footer":"","expected_revision":0}`))
	expectCode(t, e, "revision_conflict")
	execute(t, s, a, "account_rename", map[string]any{"username": "renamed-admin", "password": identityPassword})
	current, e := s.AuthenticateSession(ctx, session.ID)
	if e != nil || current.Actor.ID != a.ID || current.Actor.Name != "renamed-admin" {
		t.Fatal(current, e)
	}
	if _, e = s.Login(ctx, "admin", identityPassword, "", "test"); e == nil {
		t.Fatal("old username remained valid")
	}
	if _, e = s.Login(ctx, "renamed-admin", identityPassword, "", "test"); e != nil {
		t.Fatal(e)
	}
	execute(t, s, current.Actor, "panel_bookmarks_save", map[string]any{"id": "owned-panel", "name": "Other panel", "url": "https://example.com/panel", "description": "plain text", "expected_revision": 0})
	_, e = s.Execute(ctx, current.Actor, "panel_bookmarks_save", json.RawMessage(`{"id":"bad","name":"bad","url":"javascript:alert(1)","expected_revision":0}`))
	expectCode(t, e, "invalid_input")
	_, e = s.Execute(ctx, current.Actor, "panel_bookmarks_save", json.RawMessage(`{"id":"bad","name":"bad","url":"https://user:password@example.com","expected_revision":0}`))
	expectCode(t, e, "invalid_input")
	bookmarks := execute(t, s, current.Actor, "panel_bookmarks_list", map[string]any{}).(map[string]any)["items"].([]PanelBookmark)
	if len(bookmarks) != 1 || bookmarks[0].Revision != 1 {
		t.Fatal(bookmarks)
	}
	path := s.dataDir
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = New(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	loaded, e := s.PanelSettings(ctx)
	if e != nil || loaded.PanelName != panel.PanelName || loaded.Footer != panel.Footer {
		t.Fatal(loaded, e)
	}
}

func TestDefaultPanelBrandingUsesFolio(t *testing.T) {
	s, _ := testService(t)
	defer s.Close()

	settings, err := s.PublicPanelSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings["panel_name"] != "Folio · 折纸面板" || settings["footer"] != "Folio · 折纸面板" {
		t.Fatalf("unexpected default branding: %#v", settings)
	}
}

func TestApplicationPeerScopesRevocationAndEncryptedRetry(t *testing.T) {
	s, a := testService(t)
	ctx := WithPeerIP(context.Background(), "127.0.0.1:1234")
	input := json.RawMessage(jsonText(map[string]any{"name": "external automation", "scopes": []string{"notes:read", "notes:write"}, "resources": []string{"host:local"}, "cidrs": []string{"127.0.0.1/32"}, "password": identityPassword}))
	out, e := s.ExecuteWithKey(ctx, a, "applications_create", input, "app-create-0001")
	if e != nil {
		t.Fatal(e)
	}
	created := out.(map[string]any)
	app := created["application"].(Application)
	secret := created["secret"].(string)
	var cached string
	if e = s.db.QueryRow("SELECT result FROM idempotency WHERE action='applications_create'").Scan(&cached); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(cached, secret) || !strings.HasPrefix(cached, "encrypted:v1:") {
		t.Fatal("raw app secret persisted in replay cache")
	}
	repeat, e := s.ExecuteWithKey(ctx, a, "applications_create", input, "app-create-0001")
	if e != nil || repeat.(map[string]any)["secret"] != secret {
		t.Fatal(repeat, e)
	}
	if _, e = s.ExchangeApplication(WithPeerIP(context.Background(), "192.0.2.1"), app.ID, secret); e == nil {
		t.Fatal("outside CIDR exchanged app")
	}
	token, e := s.ExchangeApplication(ctx, app.ID, secret)
	if e != nil {
		t.Fatal(e)
	}
	actor, e := s.AuthenticateTokenForAudience(ctx, token.Token, "https://example.test/mcp")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.AuthenticateToken(context.Background(), token.Token); e == nil {
		t.Fatal("missing actual peer bypassed app CIDR")
	}
	_, e = s.Execute(ctx, actor, "system_get_status", nil)
	expectCode(t, e, "forbidden")
	note := execute(t, s, actor, "notes_create", map[string]any{"title": "from scoped application"}).(Note)
	if note.CreatedBy.ID != actor.ID {
		t.Fatal(note)
	}
	_, e = s.Execute(ctx, a, "applications_set_enabled", json.RawMessage(jsonText(map[string]any{"id": app.ID, "enabled": false, "expected_revision": 1, "password": identityPassword})))
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.AuthenticateToken(ctx, token.Token)
	expectCode(t, e, "unauthorized")
	_, e = s.RefreshActor(ctx, actor)
	expectCode(t, e, "unauthorized")
	if _, e = s.ExchangeApplication(ctx, app.ID, secret); e == nil {
		t.Fatal("disabled app exchanged")
	}
	list := execute(t, s, a, "applications_list", map[string]any{}).(map[string]any)["items"].([]Application)
	if len(list) != 1 || list[0].Enabled || list[0].Revision != 2 {
		t.Fatal(list)
	}
	var audit string
	rows, e := s.db.Query("SELECT summary FROM audit")
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	for rows.Next() {
		rows.Scan(&audit)
		if strings.Contains(audit, secret) || strings.Contains(audit, identityPassword) {
			t.Fatal("app secret leaked into audit")
		}
	}
}
func TestAPISwitchImmediatelyDeniesBothTokenChannelsKeepsHumanRecovery(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	token, e := s.CreateToken(ctx, a, TokenRequest{Name: "human-issued MCP", Scopes: []string{"system:read"}, Resources: []string{"host:local"}})
	if e != nil {
		t.Fatal(e)
	}
	session, e := s.Login(ctx, "admin", identityPassword, "", "test")
	if e != nil {
		t.Fatal(e)
	}
	execute(t, s, a, "settings_security_update", map[string]any{"password": identityPassword, "login_entry": "", "captcha_enabled": false, "api_enabled": false, "maintenance": false, "expected_revision": 0})
	_, e = s.AuthenticateToken(ctx, token.Token)
	expectCode(t, e, "api_disabled")
	_, e = s.AuthenticateTokenForAudience(ctx, token.Token, "https://example.test/mcp")
	expectCode(t, e, "api_disabled")
	current, e := s.AuthenticateSession(ctx, session.ID)
	if e != nil {
		t.Fatal("human recovery session lost", e)
	}
	execute(t, s, current.Actor, "settings_security_update", map[string]any{"password": identityPassword, "login_entry": "", "captcha_enabled": false, "api_enabled": true, "maintenance": false, "expected_revision": 1})
	if _, e = s.AuthenticateToken(ctx, token.Token); e != nil {
		t.Fatal(e)
	}
}
func TestTemporarySingleUseScopedSessionRefreshLogsAndRevoke(t *testing.T) {
	s, a := testService(t)
	ctx := WithPeerIP(context.Background(), "127.0.0.1:12")
	created := execute(t, s, a, "temporary_grants_create", map[string]any{"name": "short support visit", "scopes": []string{"notes:read", "notes:write"}, "resources": []string{"host:local"}, "expires_in_seconds": 60, "session_seconds": 120, "password": identityPassword}).(map[string]any)
	secret := created["token"].(string)
	id := created["id"].(string)
	var wg sync.WaitGroup
	results := make(chan Session, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess, e := s.RedeemTemporary(ctx, secret)
			if e == nil {
				results <- sess
			}
		}()
	}
	wg.Wait()
	close(results)
	var session Session
	count := 0
	for sess := range results {
		session = sess
		count++
	}
	if count != 1 {
		t.Fatalf("grant redeemed %d times", count)
	}
	if session.Actor.Kind != "temporary" || session.Actor.ID != "grant:"+id {
		t.Fatal(session.Actor)
	}
	fresh, e := s.RefreshActor(ctx, Actor{ID: session.Actor.ID, Kind: "human", Scopes: []string{"*"}, Resources: []string{"*"}})
	if e != nil || fresh.Kind != "temporary" || hasScope(fresh, "system:read") {
		t.Fatal("temporary refresh escalated", fresh, e)
	}
	note := execute(t, s, fresh, "notes_create", map[string]any{"title": "temporary visit"}).(Note)
	if note.CreatedBy.Kind != "temporary" || note.CreatedBy.ID != fresh.ID {
		t.Fatal(note)
	}
	_, e = s.Execute(ctx, fresh, "system_get_status", nil)
	expectCode(t, e, "forbidden")
	_, e = s.CreateToken(ctx, fresh, TokenRequest{Name: "escape", Scopes: []string{"*"}, Resources: []string{"*"}})
	expectCode(t, e, "forbidden")
	logs := execute(t, s, a, "temporary_grants_logs", map[string]any{}).(map[string]any)["items"].([]map[string]any)
	if len(logs) != 1 || logs[0]["source"] != "127.0.0.1" {
		t.Fatal(logs)
	}
	execute(t, s, a, "temporary_grants_revoke", map[string]any{"id": id})
	_, e = s.AuthenticateSession(ctx, session.ID)
	expectCode(t, e, "unauthorized")
	_, e = s.RefreshActor(ctx, fresh)
	expectCode(t, e, "unauthorized")
}
func TestCAPTCHAImagePeerExpiryReplayAndCustomEntry(t *testing.T) {
	s, a := testService(t)
	ctx := WithPeerIP(context.Background(), "127.0.0.1:10")
	execute(t, s, a, "settings_security_update", map[string]any{"password": identityPassword, "login_entry": "owned-entry-21", "captcha_enabled": true, "api_enabled": true, "maintenance": false, "expected_revision": 0})
	public, e := s.PublicPanelSettings(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, exists := public["login_entry"]; exists {
		t.Fatal("anonymous branding disclosed login entry")
	}
	challenge, answer, e := s.newCAPTCHA(ctx)
	if e != nil {
		t.Fatal(e)
	}
	imageBytes, e := base64.StdEncoding.DecodeString(strings.TrimPrefix(challenge.Image, "data:image/png;base64,"))
	if e != nil {
		t.Fatal(e)
	}
	img, e := png.Decode(bytes.NewReader(imageBytes))
	if e != nil || img.Bounds().Dx() != 212 {
		t.Fatal("actual CAPTCHA image invalid", e)
	}
	_, e = s.ProtectedLogin(ctx, "admin", identityPassword, "", "wrong-entry", challenge.ID, answer)
	expectCode(t, e, "not_found")
	_, e = s.ProtectedLogin(WithPeerIP(context.Background(), "192.0.2.1"), "admin", identityPassword, "", "owned-entry-21", challenge.ID, answer)
	expectCode(t, e, "captcha_invalid")
	session, e := s.ProtectedLogin(ctx, "admin", identityPassword, "", "owned-entry-21", challenge.ID, answer)
	if e != nil || session.Actor.ID != a.ID {
		t.Fatal(session, e)
	}
	_, e = s.ProtectedLogin(ctx, "admin", identityPassword, "", "owned-entry-21", challenge.ID, answer)
	expectCode(t, e, "captcha_invalid")
	expired, code, e := s.newCAPTCHA(ctx)
	if e != nil {
		t.Fatal(e)
	}
	s.db.Exec("UPDATE captcha_challenges SET expires=0 WHERE id=?", expired.ID)
	e = s.verifyCAPTCHA(ctx, expired.ID, code)
	expectCode(t, e, "captcha_invalid")
	failed, code, e := s.newCAPTCHA(ctx)
	if e != nil {
		t.Fatal(e)
	}
	e = s.verifyCAPTCHA(ctx, failed.ID, "incorrect")
	expectCode(t, e, "captcha_invalid")
	e = s.verifyCAPTCHA(ctx, failed.ID, code)
	expectCode(t, e, "captcha_invalid")
	raw, e := os.ReadFile(s.dataDir + "/powerlinux.db")
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(raw, []byte(challenge.Image)) {
		t.Fatal("CAPTCHA image unnecessarily persisted")
	}
}
