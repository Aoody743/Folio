package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestTaskSecretExpiryBoundAndRevokedActorCleanup(t *testing.T) {
	s, admin := testService(t)
	ctx := context.Background()
	store := s.Runtime("ttlfixture").Secrets.(ExpiringSecretStore)
	for _, expiry := range []time.Time{time.Time{}, time.Now().Add(-time.Second), time.Now().Add(25 * time.Hour)} {
		_, err := store.PutExpiring(ctx, admin, "db:one", "temporary password", []byte("expiry-canary"), expiry)
		expectCode(t, err, "invalid_input")
	}
	token, err := s.CreateToken(ctx, admin, TokenRequest{Name: "expiry actor", Scopes: []string{"system:read"}, Resources: []string{"db:one"}})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := s.AuthenticateToken(ctx, token.Token)
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour)
	ref, err := store.PutExpiring(ctx, actor, "db:one", "temporary password", []byte("expiry-canary"), expiry)
	if err != nil {
		t.Fatal(err)
	}
	regular, err := store.Put(ctx, admin, "db:one", "persistent password", []byte("persistent-canary"))
	if err != nil {
		t.Fatal(err)
	}
	if value, getErr := store.Get(ctx, actor, "db:one", ref); getErr != nil || string(value) != "expiry-canary" {
		t.Fatal("live task secret unavailable", getErr)
	}
	if err = s.RevokeToken(ctx, admin, token.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.expireProviderSecrets(ctx, expiry.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = store.Get(ctx, admin, "db:one", ref)
	expectCode(t, err, "not_found")
	if value, getErr := store.Get(ctx, admin, "db:one", regular); getErr != nil || string(value) != "persistent-canary" {
		t.Fatal("janitor deleted a nonexpiring secret", getErr)
	}
	var expiryRows int
	if err = s.db.QueryRow("SELECT count(*) FROM provider_secret_expiry WHERE ref=?", ref).Scan(&expiryRows); err != nil || expiryRows != 0 {
		t.Fatal("expiry row was not deleted with credential", err, expiryRows)
	}
}

func TestTaskSecretExpiryIsEncryptedAndImmediateWithoutJanitor(t *testing.T) {
	s, admin := testService(t)
	ctx := context.Background()
	store := s.Runtime("ttlfixture").Secrets.(ExpiringSecretStore)
	ref, err := store.PutExpiring(ctx, admin, "db:one", "temporary", []byte("expiry-canary"), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext string
	if err = s.db.QueryRow("SELECT ciphertext FROM provider_secrets WHERE ref=?", ref).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	raw, err := s.decrypt(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	var envelope secretEnvelope
	if err = json.Unmarshal(raw, &envelope); err != nil || envelope.ExpiresAt == 0 {
		t.Fatal("expiry not bound to encrypted credential", err)
	}
	envelope.ExpiresAt = time.Now().Add(-time.Minute).UnixMilli()
	raw, _ = json.Marshal(envelope)
	ciphertext, err = s.encrypt(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE provider_secrets SET ciphertext=? WHERE ref=?", ciphertext, ref); err != nil {
		t.Fatal(err)
	}
	// Even a missing cleanup index cannot make the sealed expired value usable.
	if _, err = s.db.Exec("DELETE FROM provider_secret_expiry WHERE ref=?", ref); err != nil {
		t.Fatal(err)
	}
	_, err = store.Get(ctx, admin, "db:one", ref)
	expectCode(t, err, "not_found")
}

func TestTaskSecretExpiryStartupRemovesCanceledTaskCredential(t *testing.T) {
	s, admin := testService(t)
	ctx := context.Background()
	store := s.Runtime("ttlfixture").Secrets.(ExpiringSecretStore)
	ref, err := store.PutExpiring(ctx, admin, "db:one", "queued task credential", []byte("queued-expiry-canary"), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE provider_secret_expiry SET expires_at=? WHERE ref=?", time.Now().Add(-time.Minute).UnixMilli(), ref); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(s.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var count int
	if err = reopened.db.QueryRow("SELECT count(*) FROM provider_secrets WHERE ref=?", ref).Scan(&count); err != nil || count != 0 {
		t.Fatal("restart did not remove expired unexecuted task credential", err, count)
	}
}
