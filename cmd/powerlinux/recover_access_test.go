package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func recoveryOTP(t *testing.T, secret string) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(time.Now().Unix()/30))
	hash := hmac.New(sha1.New, key)
	hash.Write(counter[:])
	sum := hash.Sum(nil)
	offset := sum[len(sum)-1] & 15
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1000000)
}

func TestRecoverAccessCLI(t *testing.T) {
	const childKey = "POWERLINUX_RECOVER_ACCESS_TEST_DIR"
	if dir := os.Getenv(childKey); dir != "" {
		if err := run([]string{"recover-access", "--data-dir", dir}); err != nil {
			t.Fatal(err)
		}
		return
	}
	invoke := func(dir string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRecoverAccessCLI$", "-test.timeout=12s")
		command.Env = append(os.Environ(), childKey+"="+dir)
		output, err := command.CombinedOutput()
		return string(output), err
	}
	t.Run("uninitialized", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		if output, err := invoke(dir); err == nil || !strings.Contains(output, "administrator is not initialized") {
			t.Fatalf("uninitialized recovery did not fail safely: %v\n%s", err, output)
		}
	})
	t.Run("locked-and-preserved", func(t *testing.T) {
		ctx := context.Background()
		dir := t.TempDir()
		service, err := core.New(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if service != nil {
				service.Close()
			}
		}()
		secret, err := os.ReadFile(service.SetupSecretFile())
		if err != nil {
			t.Fatal(err)
		}
		const password = "a strong local recovery password"
		actor, err := service.Setup(ctx, strings.TrimSpace(string(secret)), "admin", password)
		if err != nil {
			t.Fatal(err)
		}
		execute := func(name string, input any) any {
			t.Helper()
			raw, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.Execute(ctx, actor, name, raw)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return result
		}
		execute("settings_branding_update", map[string]any{"panel_name": "Retained panel", "footer": "Retained footer", "expected_revision": 0})
		execute("settings_basic_auth_update", map[string]any{"password": password, "enabled": true, "basic_username": "entry-admin", "basic_password": "independent gateway password", "expected_revision": 1})
		gateway, err := service.AuthenticateBasicGateway(ctx, "entry-admin", "independent gateway password")
		if err != nil {
			t.Fatal(err)
		}
		execute("settings_security_update", map[string]any{"password": password, "login_entry": "blocked-entry", "captcha_enabled": true, "api_enabled": false, "maintenance": true, "expected_revision": 2})
		note := execute("notes_create", map[string]any{"title": "Retained note", "content": "Local recovery must retain this", "target_id": "host:local"}).(core.Note)
		token, err := service.CreateToken(ctx, actor, core.TokenRequest{Name: "Retained MCP token", Kind: "mcp", Scopes: []string{"notes:read"}, Resources: []string{"host:local"}})
		if err != nil {
			t.Fatal(err)
		}
		enrollment, err := service.BeginTOTP(ctx, actor, password, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := service.ConfirmTOTP(ctx, actor, recoveryOTP(t, enrollment.Secret)); err != nil {
			t.Fatal(err)
		}
		lock, err := acquireLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if output, err := invoke(dir); err == nil || !strings.Contains(output, "already used by a server or upgrade") {
			t.Fatalf("recovery bypassed the real lifetime lock: %v\n%s", err, output)
		}
		before, err := service.PanelSettings(ctx)
		if err != nil || before.Revision != 3 || !before.Maintenance || !before.BasicEnabled {
			t.Fatalf("failed locked recovery changed settings: %+v, %v", before, err)
		}
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
		output, err := invoke(dir)
		if err != nil || !strings.Contains(output, "administrator credentials and MFA retained") {
			t.Fatalf("recovery subprocess failed: %v\n%s", err, output)
		}
		for _, sensitive := range []string{password, enrollment.Secret, token.Token, gateway} {
			if strings.Contains(output, sensitive) {
				t.Fatal("local recovery printed a retained credential")
			}
		}
		service, err = core.New(dir)
		if err != nil {
			t.Fatal(err)
		}
		after, err := service.PanelSettings(ctx)
		if err != nil || after.Revision != before.Revision+1 || after.LoginEntry != "" || after.CAPTCHAEnabled || after.Maintenance || after.BasicEnabled || after.BasicUsername != "" || after.APIEnabled || after.PanelName != before.PanelName || after.Footer != before.Footer {
			t.Fatalf("recovery changed its intended scope: %+v, %v", after, err)
		}
		if required, err := service.GatewayRequired(ctx); err != nil || required {
			t.Fatalf("browser gateway was not cleared: %v, %v", required, err)
		}
		if err := service.AuthenticateGatewaySession(ctx, gateway); core.ErrorCode(err) != "unauthorized" {
			t.Fatalf("stale gateway cookie remained valid: %v", err)
		}
		account, err := service.Account(ctx, actor)
		if err != nil || account.(map[string]any)["totp_enabled"] != true {
			t.Fatalf("recovery removed MFA: %v, %v", account, err)
		}
		if err := service.VerifyCredentials(ctx, actor, password, recoveryOTP(t, enrollment.Secret)); err != nil {
			t.Fatalf("retained password/MFA no longer verify: %v", err)
		}
		if err := service.VerifyCredentials(ctx, actor, "incorrect password", recoveryOTP(t, enrollment.Secret)); core.ErrorCode(err) != "unauthorized" {
			t.Fatalf("incorrect password was accepted: %v", err)
		}
		if _, err := service.ProtectedLogin(ctx, "admin", password, "", "", "", ""); core.ErrorCode(err) != "unauthorized" {
			t.Fatalf("recovered entry bypassed MFA: %v", err)
		}
		if retained := execute("notes_get", map[string]any{"id": note.ID}).(core.Note); retained.Content != note.Content || retained.Revision != note.Revision {
			t.Fatalf("recovery changed persisted notes: %+v", retained)
		}
		tokens, err := service.ListTokens(ctx, actor)
		if err != nil || len(tokens) != 1 || tokens[0].ID != token.ID || tokens[0].Revoked {
			t.Fatalf("recovery changed persisted tokens: %+v, %v", tokens, err)
		}
		if _, err := service.AuthenticateToken(ctx, token.Token); core.ErrorCode(err) != "api_disabled" {
			t.Fatalf("recovery silently enabled token APIs: %v", err)
		}
	})
}
