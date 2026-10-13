package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const gatewayPassword = "separate browser entry password"

func TestGatewayCredentialCookieRevocationCASAndLocalRecovery(t *testing.T) {
	s, a := testService(t)
	ctx := WithPeerIP(context.Background(), "127.0.0.1:1234")
	execute(t, s, a, "settings_basic_auth_update", map[string]any{"password": identityPassword, "enabled": true, "basic_username": "entry-user", "basic_password": gatewayPassword, "expected_revision": 0})
	var hash string
	if err := s.db.QueryRow("SELECT password_hash FROM basic_gateway").Scan(&hash); err != nil || strings.Contains(hash, gatewayPassword) || !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatal("gateway credential was not hashed")
	}
	if _, err := s.AuthenticateBasicGateway(ctx, "entry-user", "wrong"); ErrorCode(err) != "unauthorized" {
		t.Fatal("wrong gateway credential accepted")
	}
	if _, err := s.AuthenticateBasicGateway(ctx, "wrong-user", gatewayPassword); ErrorCode(err) != "unauthorized" {
		t.Fatal("wrong gateway username accepted")
	}
	cookie, err := s.AuthenticateBasicGateway(ctx, "entry-user", gatewayPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AuthenticateGatewaySession(ctx, cookie); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateSession(ctx, cookie); ErrorCode(err) != "unauthorized" {
		t.Fatal("gateway cookie granted panel access")
	}
	_, err = s.Execute(ctx, a, "settings_basic_auth_update", json.RawMessage(jsonText(map[string]any{"password": identityPassword, "enabled": false, "expected_revision": 0})))
	expectCode(t, err, "revision_conflict")
	execute(t, s, a, "settings_basic_auth_update", map[string]any{"password": identityPassword, "enabled": true, "basic_username": "entry-user", "basic_password": "rotated browser entry password", "expected_revision": 1})
	expectCode(t, s.AuthenticateGatewaySession(ctx, cookie), "unauthorized")
	execute(t, s, a, "settings_security_update", map[string]any{"password": identityPassword, "login_entry": "owned-locked-entry", "captcha_enabled": true, "api_enabled": false, "maintenance": true, "expected_revision": 2})
	if err = s.RecoverPanelAccess(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := s.PanelSettings(ctx)
	if err != nil || p.BasicEnabled || p.Maintenance || p.LoginEntry != "" || p.CAPTCHAEnabled || p.APIEnabled {
		t.Fatal("recovery changed an unrelated API policy or failed")
	}
	if enabled, err := s.GatewayRequired(ctx); err != nil || enabled {
		t.Fatal("gateway not recovered")
	}
	if _, err = s.ProtectedLogin(ctx, "admin", identityPassword, "", "", "", ""); err != nil {
		t.Fatal("human recovery login failed", err)
	}
}
func TestWrongEntryUsesConfiguredStatusWithoutChangingAuthentication(t *testing.T) {
	s, a := testService(t)
	execute(t, s, a, "settings_security_update", map[string]any{"password": identityPassword, "login_entry": "owned-hidden-entry", "captcha_enabled": false, "api_enabled": true, "maintenance": false, "unauthorized_status": 408, "expected_revision": 0})
	err := s.CheckLoginEntry(context.Background(), "incorrect")
	if ErrorStatus(err) != 408 {
		t.Fatal("custom entry response status ignored")
	}
	if _, err = s.ProtectedLogin(context.Background(), "admin", "wrong password", "", "owned-hidden-entry", "", ""); ErrorStatus(err) != 401 {
		t.Fatal("real credential error status changed")
	}
}
