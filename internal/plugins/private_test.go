package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrivateAgentRecordsEncryptSecretsAndBindIdentity(t *testing.T) {
	executor := testAgent(t)
	secret := map[string]any{"password": "never-store-root-credentials-as-plaintext"}
	if err := executor.save("private", "fixture_secret", secret); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(executor.config.StateDir, "private", "fixture_secret.json")
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(secret["password"].(string))) || !bytes.HasPrefix(body, []byte(privateRecordPrefix)) {
		t.Fatal("Agent credential record remains plaintext")
	}
	var recovered map[string]any
	if err = executor.load("private", "fixture_secret", &recovered); err != nil || recovered["password"] != secret["password"] {
		t.Fatal("encrypted Agent record did not round-trip", err)
	}
	if err = os.WriteFile(filepath.Join(executor.config.StateDir, "private", "copied_secret.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	if err = executor.load("private", "copied_secret", &recovered); err == nil {
		t.Fatal("encrypted record was transplanted to another identity")
	}
	legacy, _ := json.Marshal(secret)
	name = filepath.Join(executor.config.StateDir, "private", "legacy_secret.json")
	if err = os.WriteFile(name, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if err = executor.load("private", "legacy_secret", &recovered); err != nil {
		t.Fatal(err)
	}
	migrated, _ := os.ReadFile(name)
	if bytes.Contains(migrated, []byte(secret["password"].(string))) {
		t.Fatal("trusted legacy private record was not atomically encrypted")
	}
}
func TestResourceLockCancellationAndRegistryCleanup(t *testing.T) {
	executor := testAgent(t)
	release := executor.lock("webssh-test")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		unlock, err := executor.lockContext(ctx, "webssh-test")
		if err == nil {
			unlock()
		}
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("waiting resource lock ignored cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled lock waiter remains blocked")
	}
	release()
	executor.mu.Lock()
	entries := len(executor.locks)
	executor.mu.Unlock()
	if entries != 0 {
		t.Fatal("idle resource lock leaked", entries)
	}
}
func TestSignedRPMValidationRejectsUnsignedDigestOnlyAndWeakSignatures(t *testing.T) {
	for _, result := range []string{"package.rpm: digests OK", "Header V4 RSA/SHA1 Signature, key ID 1234: OK", "Header SHA256 digest: OK"} {
		if rpmSignature.MatchString(result) {
			t.Fatalf("untrusted RPM output accepted %q", result)
		}
	}
	if !rpmSignature.MatchString("Header V4 RSA/SHA256 Signature, key ID 12345678: OK\n") {
		t.Fatal("valid strong RPM signature was rejected")
	}
}
