package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExclusiveDataLock(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLock(dir); err == nil {
		t.Fatal("second process acquired live server data lock")
	}
	first.Close()
	second, err := acquireLock(dir)
	if err != nil {
		t.Fatal("lock did not release", err)
	}
	second.Close()
}

func TestLockRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "outside"), filepath.Join(dir, "server.lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLock(dir); err == nil {
		t.Fatal("lock followed untrusted symlink")
	}
}

func TestInitUsesPrivatePasswordAndCannotRepeat(t *testing.T) {
	dir := t.TempDir()
	passwordFile := filepath.Join(dir, "password")
	if err := os.WriteFile(passwordFile, []byte("temporary-testing-passphrase-33\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	args := []string{"init", "--data-dir", state, "--username", "operator", "--password-file", passwordFile}
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	if err := run(args); err == nil {
		t.Fatal("initialized administrator was silently replaced")
	}
	if _, err := os.Stat(filepath.Join(state, "bootstrap-token")); !os.IsNotExist(err) {
		t.Fatal("one-time bootstrap credential remained after initialization")
	}
	if err := os.Chmod(passwordFile, 0644); err != nil {
		t.Fatal(err)
	}
	args[2] = filepath.Join(dir, "other-state")
	if err := run(args); err == nil {
		t.Fatal("public password file accepted")
	}
}

func TestStdioDoesNotAllowMissingIdentity(t *testing.T) {
	t.Setenv("POWERLINUX_MCP_TOKEN", "")
	if err := run([]string{"mcp", "--stdio"}); err == nil {
		t.Fatal("stdio did not require scoped identity")
	}
}

func TestServeRejectsRemotePlaintext(t *testing.T) {
	err := run([]string{"serve", "--data-dir", t.TempDir(), "--listen", "0.0.0.0:0", "--allow-root"})
	if err == nil || !strings.Contains(err.Error(), "plaintext HTTP must bind a loopback") {
		t.Fatalf("remote plaintext listener was not rejected: %v", err)
	}
}
