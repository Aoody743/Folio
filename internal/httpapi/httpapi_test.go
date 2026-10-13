package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

type fixture struct {
	s      *core.Service
	server *httptest.Server
	actor  core.Actor
	cookie *http.Cookie
	csrf   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s, e := core.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(New(s))
	t.Cleanup(func() { server.Close(); s.Close() })
	secret, _ := os.ReadFile(s.SetupSecretFile())
	a, e := s.Setup(context.Background(), strings.TrimSpace(string(secret)), "admin", "a secure long password")
	if e != nil {
		t.Fatal(e)
	}
	session, e := s.Login(context.Background(), "admin", "a secure long password", "", "test")
	if e != nil {
		t.Fatal(e)
	}
	return &fixture{s: s, server: server, actor: a, cookie: &http.Cookie{Name: "pl_session", Value: session.ID}, csrf: session.CSRFToken}
}
func (f *fixture) request(t *testing.T, method, path string, input any, headers map[string]string) (int, map[string]any, http.Header) {
	t.Helper()
	var body io.Reader
	if input != nil {
		b, _ := json.Marshal(input)
		body = bytes.NewReader(b)
	}
	r, e := http.NewRequest(method, f.server.URL+path, body)
	if e != nil {
		t.Fatal(e)
	}
	if input != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	r.AddCookie(f.cookie)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	resp, e := f.server.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	var out map[string]any
	if e = json.NewDecoder(resp.Body).Decode(&out); e != nil {
		t.Fatal(e)
	}
	return resp.StatusCode, out, resp.Header
}

func TestCookieCSRFAndCrossSiteProtection(t *testing.T) {
	f := newFixture(t)
	payload := map[string]any{"title": "note"}
	code, _, _ := f.request(t, "POST", "/api/v1/notes", payload, nil)
	if code != 403 {
		t.Fatal(code)
	}
	code, _, _ = f.request(t, "POST", "/api/v1/notes", payload, map[string]string{"X-CSRF-Token": f.csrf, "Origin": "https://evil.example"})
	if code != 403 {
		t.Fatal(code)
	}
	code, out, _ := f.request(t, "POST", "/api/v1/notes", payload, map[string]string{"X-CSRF-Token": f.csrf})
	if code != 200 {
		t.Fatal(code, out)
	}
	code, out, _ = f.request(t, "GET", "/api/v1/session", nil, nil)
	if code != 200 || out["data"].(map[string]any)["csrf_token"] != f.csrf {
		t.Fatal(code, out)
	}
}
func TestRESTAndNativeRegistryHaveSameACLAndConflict(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	token, e := f.s.CreateToken(ctx, f.actor, core.TokenRequest{Name: "AI", Scopes: []string{"notes:read", "notes:write"}, Resources: []string{"host:local"}})
	if e != nil {
		t.Fatal(e)
	}
	ai, _ := f.s.AuthenticateToken(ctx, token.Token)
	code, out, _ := f.request(t, "POST", "/api/v1/notes", map[string]any{"title": "shared"}, map[string]string{"X-CSRF-Token": f.csrf})
	if code != 200 {
		t.Fatal(out)
	}
	note := out["data"].(map[string]any)
	id := note["id"].(string)
	raw, _ := json.Marshal(map[string]any{"id": id, "expected_revision": 1, "content": "AI change"})
	if _, e = f.s.Execute(ctx, ai, "notes_update", raw); e != nil {
		t.Fatal(e)
	}
	code, out, _ = f.request(t, "PATCH", "/api/v1/notes/"+id, map[string]any{"expected_revision": 1, "content": "human stale"}, map[string]string{"X-CSRF-Token": f.csrf})
	if code != 412 || out["error"].(map[string]any)["code"] != "revision_conflict" {
		t.Fatal(code, out)
	}
	code, out, headers := f.request(t, "GET", "/api/v1/notes/"+id, nil, map[string]string{"Authorization": "Bearer " + token.Token})
	if code != 200 || headers.Get("ETag") != "\"2\"" {
		t.Fatal(code, out, headers)
	}
	code, out, _ = f.request(t, "POST", "/api/v1/actions/notes_create", map[string]any{"title": "escalate", "target_id": "private:resource"}, map[string]string{"Authorization": "Bearer " + token.Token})
	if code != 403 {
		t.Fatal(code, out)
	}
	code, out, _ = f.request(t, "POST", "/api/v1/notes", map[string]any{"title": "spoof", "actor": map[string]any{"kind": "human"}}, map[string]string{"Authorization": "Bearer " + token.Token})
	if code != 400 {
		t.Fatal(code, out)
	}
	if e = f.s.RevokeToken(ctx, f.actor, token.ID); e != nil {
		t.Fatal(e)
	}
	code, _, _ = f.request(t, "GET", "/api/v1/notes", nil, map[string]string{"Authorization": "Bearer " + token.Token})
	if code != 401 {
		t.Fatal(code)
	}
}
func TestLoginSetsDefensiveCookieAndUnknownFieldsAreRejected(t *testing.T) {
	f := newFixture(t)
	code, out, headers := f.request(t, "POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "a secure long password"}, nil)
	if code != 200 {
		t.Fatal(code, out)
	}
	cookie := headers.Get("Set-Cookie")
	if !strings.Contains(cookie, "HttpOnly") || !strings.Contains(cookie, "SameSite=Strict") {
		t.Fatal(cookie)
	}
	code, _, _ = f.request(t, "POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "a secure long password", "skip_totp": true}, nil)
	if code != 400 {
		t.Fatal(code)
	}
	code, out, _ = f.request(t, "POST", "/api/v1/auth/logout", map[string]any{}, map[string]string{"X-CSRF-Token": f.csrf})
	if code != 200 {
		t.Fatal(code, out)
	}
	code, out, _ = f.request(t, "GET", "/api/v1/session", nil, nil)
	if code != 200 || out["data"].(map[string]any)["authenticated"] != false {
		t.Fatal(code, out)
	}
}

func TestOpenEventStreamStopsAfterTokenRevocation(t *testing.T) {
	f := newFixture(t)
	token, e := f.s.CreateToken(context.Background(), f.actor, core.TokenRequest{Name: "events", Scopes: []string{"notes:read"}, Resources: []string{"host:local"}})
	if e != nil {
		t.Fatal(e)
	}
	r, _ := http.NewRequest("GET", f.server.URL+"/api/v1/events", nil)
	r.Header.Set("Authorization", "Bearer "+token.Token)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, e := client.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	if e = f.s.RevokeToken(context.Background(), f.actor, token.ID); e != nil {
		t.Fatal(e)
	}
	_, e = f.s.Execute(context.Background(), f.actor, "notes_create", json.RawMessage(`{"title":"Trigger invalidation"}`))
	if e != nil {
		t.Fatal(e)
	}
	reader := bufio.NewReader(resp.Body)
	var text strings.Builder
	for {
		line, e := reader.ReadString('\n')
		text.WriteString(line)
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
	}
	if !strings.Contains(text.String(), "event: auth_expired") {
		t.Fatalf("revoked stream stayed authorized: %s", text.String())
	}
}

func TestIdleEventStreamStopsAfterTokenRevocationWithinLease(t *testing.T) {
	f := newFixture(t)
	token, err := f.s.CreateToken(context.Background(), f.actor, core.TokenRequest{Name: "idle events", Scopes: []string{"notes:read"}, Resources: []string{"host:local"}})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest("GET", f.server.URL+"/api/v1/events", nil)
	r.Header.Set("Authorization", "Bearer "+token.Token)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	if err = f.s.RevokeToken(context.Background(), f.actor, token.ID); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	data, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Contains(data, []byte("event: auth_expired")) || time.Since(start) > 2*time.Second {
		t.Fatal("idle revoked event stream exceeded its one-second authentication lease", err)
	}
}
