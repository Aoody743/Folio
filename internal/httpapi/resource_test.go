package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

type resourceFunc func(http.ResponseWriter, *http.Request, core.Actor) bool

func (fn resourceFunc) HandleResource(w http.ResponseWriter, r *http.Request, a core.Actor) bool {
	return fn(w, r, a)
}
func resourceFixture(t *testing.T, fn func(*core.Service, core.Actor) resourceFunc) *fixture {
	t.Helper()
	s, e := core.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	secret, _ := os.ReadFile(s.SetupSecretFile())
	a, e := s.Setup(context.Background(), strings.TrimSpace(string(secret)), "admin", "a secure long password")
	if e != nil {
		t.Fatal(e)
	}
	session, e := s.Login(context.Background(), a.Name, "a secure long password", "", "test")
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(NewWithOptions(s, Options{ResourceHandler: fn(s, a)}))
	t.Cleanup(func() { server.Close(); s.Close() })
	return &fixture{s: s, server: server, actor: a, cookie: &http.Cookie{Name: "pl_session", Value: session.ID}, csrf: session.CSRFToken}
}
func TestResourceHookAuthenticatesOriginCSRFAndExcludesAuth(t *testing.T) {
	var called atomic.Int32
	f := resourceFixture(t, func(s *core.Service, a core.Actor) resourceFunc {
		return func(w http.ResponseWriter, r *http.Request, actor core.Actor) bool {
			called.Add(1)
			if actor.ID == "" {
				t.Error("untrusted actor")
			}
			success(w, map[string]any{"ok": true})
			return true
		}
	})
	code, _, _ := f.request(t, "PATCH", "/api/v1/files/uploads/a/chunk", map[string]any{}, nil)
	if code != 403 || called.Load() != 0 {
		t.Fatal(code, called.Load())
	}
	code, _, _ = f.request(t, "PATCH", "/api/v1/files/uploads/a/chunk", map[string]any{}, map[string]string{"X-CSRF-Token": f.csrf, "Origin": "https://evil.example"})
	if code != 403 || called.Load() != 0 {
		t.Fatal(code)
	}
	code, _, _ = f.request(t, "PATCH", "/api/v1/files/uploads/a/chunk", map[string]any{}, map[string]string{"X-CSRF-Token": f.csrf})
	if code != 200 || called.Load() != 1 {
		t.Fatal(code, called.Load())
	}
	code, _, _ = f.request(t, "GET", "/api/v1/files/download", nil, map[string]string{"Authorization": "Bearer invalid"})
	if code != 401 || called.Load() != 1 {
		t.Fatal(code)
	}
	code, _, _ = f.request(t, "GET", "/api/v1/account", nil, nil)
	if code != 200 || called.Load() != 1 {
		t.Fatal("resource hook intercepted account authority", code)
	}
}
func TestCookieWebsocketRequiresCSRFSubprotocol(t *testing.T) {
	var called atomic.Int32
	f := resourceFixture(t, func(s *core.Service, a core.Actor) resourceFunc {
		return func(w http.ResponseWriter, r *http.Request, actor core.Actor) bool {
			called.Add(1)
			success(w, map[string]any{"ok": true})
			return true
		}
	})
	base := map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"}
	code, _, _ := f.request(t, "GET", "/api/v1/terminal/connect", nil, base)
	if code != 403 || called.Load() != 0 {
		t.Fatal(code)
	}
	base["Sec-WebSocket-Protocol"] = f.csrf
	code, _, _ = f.request(t, "GET", "/api/v1/terminal/connect", nil, base)
	if code != 403 {
		t.Fatal("unprefixed protocol accepted", code)
	}
	base["Sec-WebSocket-Protocol"] = "powerlinux-csrf." + f.csrf
	code, _, _ = f.request(t, "GET", "/api/v1/terminal/connect", nil, base)
	if code != 200 || called.Load() != 1 {
		t.Fatal(code)
	}
}
func TestStreamRevalidationUsesOriginalCredentialAndSeesRevocation(t *testing.T) {
	var tokenID string
	f := resourceFixture(t, func(s *core.Service, admin core.Actor) resourceFunc {
		return func(w http.ResponseWriter, r *http.Request, actor core.Actor) bool {
			if e := s.RevokeToken(r.Context(), admin, tokenID); e != nil {
				fail(w, e)
				return true
			}
			fresh, e := RevalidateActor(r)
			if e == nil || fresh.ID != "" {
				t.Error("revoked stream preserved authority")
			}
			success(w, map[string]any{"revoked": core.ErrorCode(e) == "unauthorized"})
			return true
		}
	})
	token, e := f.s.CreateToken(context.Background(), f.actor, core.TokenRequest{Name: "stream", Scopes: []string{"files:read"}, Resources: []string{"fs:workspace"}})
	if e != nil {
		t.Fatal(e)
	}
	tokenID = token.ID
	code, out, _ := f.request(t, "GET", "/api/v1/files/download", nil, map[string]string{"Authorization": "Bearer " + token.Token})
	if code != 200 || out["data"].(map[string]any)["revoked"] != true {
		t.Fatal(code, out)
	}
}
func TestHTTPActionIdempotencyRepeatsOneNoteAndGeneratedOpenAPI(t *testing.T) {
	f := newFixture(t)
	headers := map[string]string{"X-CSRF-Token": f.csrf, "Idempotency-Key": "note-intent-0001"}
	code, first, _ := f.request(t, "POST", "/api/v1/notes", map[string]any{"title": "one note"}, headers)
	if code != 200 {
		t.Fatal(code, first)
	}
	code, second, _ := f.request(t, "POST", "/api/v1/notes", map[string]any{"title": "one note"}, headers)
	if code != 200 {
		t.Fatal(code, second)
	}
	if first["data"].(map[string]any)["id"] != second["data"].(map[string]any)["id"] {
		t.Fatal("HTTP retry created duplicate note")
	}
	code, changed, _ := f.request(t, "POST", "/api/v1/notes", map[string]any{"title": "changed intent"}, headers)
	if code != 409 {
		t.Fatal(code, changed)
	}
	code, spec, _ := f.request(t, "GET", "/api/v1/openapi.json", nil, nil)
	if code != 200 {
		t.Fatal(code, spec)
	}
	raw, _ := json.Marshal(spec)
	if !strings.Contains(string(raw), "/api/v1/actions/notes_create") {
		t.Fatal("schema document missing registered action")
	}
}
