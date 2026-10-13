package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIdempotencyPasswordFingerprintIsKeyedAndLegacyReplayDoesNotRepeat(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "effect")
	b := fixtureBinding("fixture_sensitive_write", "fixture:write", "site:one", func(context.Context, Actor, json.RawMessage) (ActionResult, error) {
		file, err := os.OpenFile(marker, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return ActionResult{}, err
		}
		_, err = file.WriteString("committed\n")
		file.Close()
		return ActionResult{Data: map[string]any{"ok": true}}, err
	})
	b.Action.InputSchema = object(map[string]any{"password": boundedText(512)}, "password")
	if err := s.RegisterProvider(Bindings{b}); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"password":"dictionary-target-password"}`)
	key := "protected-intent-0001"
	if _, err := s.ExecuteWithKey(ctx, a, b.Action.Name, raw, key); err != nil {
		t.Fatal(err)
	}
	id := digest(jsonText([]string{a.ID, b.Action.Name, key}))
	var stored string
	if err := s.db.QueryRow("SELECT input_hash FROM idempotency WHERE id=?", id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	legacy := digest(string(raw))
	if !strings.HasPrefix(stored, "hmac:v1:") || strings.Contains(stored, legacy) {
		t.Fatal("password request exposed an unkeyed fingerprint")
	}
	// Simulate an existing pre-HMAC cache entry, preserving the already
	// committed operation and its encrypted result.
	if _, err := s.db.Exec("UPDATE idempotency SET input_hash=? WHERE id=?", legacy, id); err != nil {
		t.Fatal(err)
	}
	dir := s.dataDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restart, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer restart.Close()
	if err = restart.db.QueryRow("SELECT input_hash FROM idempotency WHERE id=?", id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "legacy-encrypted:v1:") || strings.Contains(stored, legacy) {
		t.Fatal("startup left a legacy offline password oracle in SQLite")
	}
	if err = restart.RegisterProvider(Bindings{b}); err != nil {
		t.Fatal(err)
	}
	if _, err = restart.ExecuteWithKey(ctx, a, b.Action.Name, raw, key); err != nil {
		t.Fatal(err)
	}
	if err = restart.db.QueryRow("SELECT input_hash FROM idempotency WHERE id=?", id).Scan(&stored); err != nil || !strings.HasPrefix(stored, "hmac:v1:") {
		t.Fatal("exact replay did not upgrade legacy fingerprint")
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "committed\n" {
		t.Fatal("migration repeated the external effect")
	}
	_, err = restart.ExecuteWithKey(ctx, a, b.Action.Name, json.RawMessage(`{"password":"changed-password"}`), key)
	expectCode(t, err, "idempotency_conflict")
	// Reservation identity is part of the HMAC domain: equal input in a new
	// separately authorized intent cannot be correlated by its fingerprint.
	if _, err = restart.ExecuteWithKey(ctx, a, b.Action.Name, raw, "protected-intent-0002"); err != nil {
		t.Fatal(err)
	}
	otherID := digest(jsonText([]string{a.ID, b.Action.Name, "protected-intent-0002"}))
	var other string
	if err = restart.db.QueryRow("SELECT input_hash FROM idempotency WHERE id=?", otherID).Scan(&other); err != nil || other == stored {
		t.Fatal("fingerprint omitted reservation identity")
	}
}
