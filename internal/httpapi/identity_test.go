package httpapi

import (
	"context"
	"github.com/Aoody743/PowerLinux/internal/core"
	"net/http"
	"strings"
	"testing"
)

func TestHTTPApplicationExchangeCIDRRevocationAndCSRF(t *testing.T) {
	f := newFixture(t)
	fields := map[string]any{"name": "external application", "scopes": []string{"notes:read", "notes:write"}, "resources": []string{"host:local"}, "cidrs": []string{"127.0.0.1/32", "::1/128"}, "password": "a secure long password"}
	code, _, _ := f.request(t, "POST", "/api/v1/actions/applications_create", fields, nil)
	if code != 403 {
		t.Fatal("application create bypassed CSRF", code)
	}
	code, out, _ := f.request(t, "POST", "/api/v1/actions/applications_create", fields, map[string]string{"X-CSRF-Token": f.csrf, "Idempotency-Key": "app-exchange-0001"})
	if code != 200 {
		t.Fatal(code, out)
	}
	data := out["data"].(map[string]any)
	id := data["application"].(map[string]any)["id"].(string)
	secret := data["secret"].(string)
	code, out, _ = f.request(t, "POST", "/api/v1/auth/application", map[string]any{"id": id, "secret": secret}, map[string]string{"X-Forwarded-For": "192.0.2.88"})
	if code != 200 {
		t.Fatal(code, out)
	}
	token := out["data"].(map[string]any)["token"].(string)
	code, out, _ = f.request(t, "POST", "/api/v1/notes", map[string]any{"title": "real app lease"}, map[string]string{"Authorization": "Bearer " + token})
	if code != 200 || out["data"].(map[string]any)["created_by"].(map[string]any)["kind"] != "api" {
		t.Fatal(code, out)
	}
	code, out, _ = f.request(t, "POST", "/api/v1/actions/applications_set_enabled", map[string]any{"id": id, "enabled": false, "expected_revision": 1, "password": "a secure long password"}, map[string]string{"X-CSRF-Token": f.csrf})
	if code != 200 {
		t.Fatal(code, out)
	}
	code, _, _ = f.request(t, "GET", "/api/v1/notes", nil, map[string]string{"Authorization": "Bearer " + token})
	if code != 401 {
		t.Fatal("disabled application still authenticated", code)
	}
}
func TestHTTPTemporaryLoginProducesScopedCookieAndSingleUse(t *testing.T) {
	f := newFixture(t)
	code, out, _ := f.request(t, "POST", "/api/v1/actions/temporary_grants_create", map[string]any{"name": "support", "scopes": []string{"notes:read", "notes:write"}, "resources": []string{"host:local"}, "expires_in_seconds": 60, "session_seconds": 120, "password": "a secure long password"}, map[string]string{"X-CSRF-Token": f.csrf})
	if code != 200 {
		t.Fatal(code, out)
	}
	created := out["data"].(map[string]any)
	code, out, headers := f.request(t, "POST", "/api/v1/auth/temporary", map[string]any{"token": created["token"]}, nil)
	if code != 200 {
		t.Fatal(code, out)
	}
	originalCookie, originalCSRF := f.cookie, f.csrf
	defer func() { f.cookie = originalCookie; f.csrf = originalCSRF }()
	response := &http.Response{Header: headers}
	cookies := response.Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal(cookies)
	}
	f.cookie = cookies[0]
	f.csrf = out["data"].(map[string]any)["csrf_token"].(string)
	code, out, _ = f.request(t, "GET", "/api/v1/session", nil, nil)
	if code != 200 || out["data"].(map[string]any)["user"].(map[string]any)["kind"] != "temporary" {
		t.Fatal(code, out)
	}
	code, out, _ = f.request(t, "POST", "/api/v1/notes", map[string]any{"title": "scoped temporary cookie"}, map[string]string{"X-CSRF-Token": f.csrf})
	if code != 200 {
		t.Fatal(code, out)
	}
	code, _, _ = f.request(t, "GET", "/api/v1/system/status", nil, nil)
	if code != 403 {
		t.Fatal("temporary cookie acquired admin scopes", code)
	}
	code, _, _ = f.request(t, "POST", "/api/v1/auth/temporary", map[string]any{"token": created["token"]}, nil)
	if code != 401 {
		t.Fatal("temporary token replay succeeded", code)
	}
	f.cookie = originalCookie
	f.csrf = originalCSRF
	code, out, _ = f.request(t, "POST", "/api/v1/actions/temporary_grants_revoke", map[string]any{"id": created["id"]}, map[string]string{"X-CSRF-Token": f.csrf})
	if code != 200 {
		t.Fatal(code, out)
	}
	f.cookie = cookies[0]
	code, _, _ = f.request(t, "GET", "/api/v1/notes", nil, nil)
	if code != 401 {
		t.Fatal("revoked temporary cookie remained active", code)
	}
}
func TestHTTPCustomEntryChallengeAndHumanAPIRecovery(t *testing.T) {
	f := newFixture(t)
	code, out, _ := f.request(t, "POST", "/api/v1/actions/settings_security_update", map[string]any{"password": "a secure long password", "login_entry": "owned-hidden-entry", "captcha_enabled": true, "api_enabled": false, "maintenance": false, "expected_revision": 0}, map[string]string{"X-CSRF-Token": f.csrf})
	if code != 200 {
		t.Fatal(code, out)
	}
	f.cookie = &http.Cookie{Name: "pl_session", Value: "invalid"}
	code, out, _ = f.request(t, "GET", "/api/v1/session", nil, nil)
	if code != 200 {
		t.Fatal(code, out)
	}
	panel := out["data"].(map[string]any)["panel"].(map[string]any)
	if panel["custom_login_entry"] != true || panel["captcha_enabled"] != true {
		t.Fatal(panel)
	}
	if _, exists := panel["login_entry"]; exists {
		t.Fatal("entry disclosed")
	}
	code, out, _ = f.request(t, "GET", "/api/v1/auth/captcha", nil, nil)
	if code != 200 || !strings.HasPrefix(out["data"].(map[string]any)["image"].(string), "data:image/png;base64,") {
		t.Fatal(code, out)
	}
	challenge := out["data"].(map[string]any)["id"]
	code, _, _ = f.request(t, "POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "a secure long password"}, nil)
	if code != 404 {
		t.Fatal("custom entry bypassed", code)
	}
	code, out, _ = f.request(t, "POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "a secure long password", "login_entry": "owned-hidden-entry", "captcha_id": challenge, "captcha_answer": "wrong"}, nil)
	if code != 401 || out["error"].(map[string]any)["code"] != "captcha_invalid" {
		t.Fatal(code, out)
	}
	token, e := f.s.CreateToken(context.Background(), f.actor, core.TokenRequest{Name: "API disabled token", Scopes: []string{"notes:read"}, Resources: []string{"host:local"}})
	if e != nil {
		t.Fatal(e)
	}
	code, out, _ = f.request(t, "GET", "/api/v1/notes", nil, map[string]string{"Authorization": "Bearer " + token.Token})
	if code != 403 || out["error"].(map[string]any)["code"] != "api_disabled" {
		t.Fatal(code, out)
	}
}
