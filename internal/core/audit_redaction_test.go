package core

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
)

func TestSecretBookkeepingAuditKeepsResourceWithoutInternalReferences(t *testing.T) {
	s, admin := testService(t)
	ctx := context.Background()
	store := s.Runtime("fixture").Secrets
	ref, err := store.Put(ctx, admin, "site:owned", "SQL administrator version", []byte("private-value-canary"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Get(ctx, admin, "site:owned", ref); err != nil {
		t.Fatal(err)
	}
	if err = store.Delete(ctx, admin, "site:owned", ref); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query("SELECT action,resource,summary FROM audit WHERE action IN ('credentials_store','credentials_resolve','credentials_delete') ORDER BY action")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var action, resource, summary string
		if err = rows.Scan(&action, &resource, &summary); err != nil {
			t.Fatal(err)
		}
		if resource != "site:owned" || strings.Contains(summary, ref) || strings.Contains(summary, "secret_ref") || strings.Contains(summary, "private-value-canary") {
			t.Fatal("credential audit lost resource scope or retained private material")
		}
		if action == "credentials_store" && !strings.Contains(summary, "SQL administrator version") {
			t.Fatal("credential audit lost its public purpose")
		}
		count++
	}
	if err = rows.Err(); err != nil || count != 3 {
		t.Fatalf("credential audit records: count=%d err=%v", count, err)
	}
}

func TestCertificateProviderAuditCannotExportPrivateMaterial(t *testing.T) {
	s, admin := testService(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	digest := sha256.Sum256(der)
	fingerprint := hex.EncodeToString(digest[:])
	input := map[string]any{"key_pem": privatePEM}
	b := fixtureBinding("certificates_import", "certificates:write", "certificate:owned", func(_ context.Context, _ Actor, raw json.RawMessage) (ActionResult, error) {
		var summary map[string]any
		json.Unmarshal(raw, &summary)
		summary["fingerprint"] = fingerprint
		summary["metadata"] = map[string]any{"nested": []any{map[string]any{"privateKey": privatePEM, "api_key": "api-secret-canary", "access:key": "access-secret-canary", "client_key": "client-secret-canary", "passphrase": "passphrase-canary", "cert:key": "cert-secret-canary", "script_body": "body-secret-canary", "query": "SELECT 'literal-password-canary'", "key_fingerprint": fingerprint}}}
		summary["unknown_field"] = privatePEM
		return ActionResult{Data: map[string]any{"fingerprint": fingerprint}, Resources: []string{"certificate:owned"}, AuditSummary: summary}, nil
	})
	b.Action.InputSchema = object(map[string]any{"key_pem": boundedText(8192)}, "key_pem")
	if err = s.RegisterProvider(Bindings{b}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Execute(context.Background(), admin, "certificates_import", json.RawMessage(jsonText(input))); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateToken(context.Background(), admin, TokenRequest{Name: "audit-only", Scopes: []string{"audit:read"}, Resources: []string{"certificate:owned"}})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := s.AuthenticateToken(context.Background(), token.Token)
	if err != nil {
		t.Fatal(err)
	}
	audits := execute(t, s, actor, "audit_list", map[string]any{}).([]Audit)
	if len(audits) != 1 || audits[0].Summary["fingerprint"] != fingerprint {
		t.Fatal("certificate audit metadata missing")
	}
	encoded, _ := json.Marshal(audits)
	for _, value := range []string{privatePEM, "api-secret-canary", "access-secret-canary", "client-secret-canary", "passphrase-canary", "cert-secret-canary", "body-secret-canary", "literal-password-canary"} {
		if strings.Contains(string(encoded), value) {
			t.Fatal("audit-only actor obtained private provider material")
		}
	}
	var stored string
	if err = s.db.QueryRow("SELECT summary FROM audit WHERE action='certificates_import'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "PRIVATE KEY") || strings.Contains(stored, "canary") {
		t.Fatal("private material persisted in SQLite audit")
	}
	// Direct authentication/task bookkeeping must use the same persistence gate.
	s.recordAudit(context.Background(), admin, "fixture_direct", "certificate:owned", nil, map[string]any{"data": json.RawMessage(`{"password":"direct-canary","token_id":"public-token-id"}`)})
	if err = s.db.QueryRow("SELECT summary FROM audit WHERE action='fixture_direct'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "direct-canary") || !strings.Contains(stored, "public-token-id") {
		t.Fatal("direct audit redaction failed")
	}
}
