package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateTrustedProgramPathAcceptsCanonicalSystemExecutables(t *testing.T) {
	for _, candidate := range []string{"/usr/bin/env", "/bin/sh"} {
		resolved, err := validateTrustedProgramPath(candidate)
		if err == nil {
			if !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved {
				t.Fatalf("trusted program was not returned as a canonical absolute path: %q", resolved)
			}
			info, statErr := os.Lstat(resolved)
			if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("canonical trusted program is not a regular target: %q, %v", resolved, statErr)
			}
			return
		}
	}
	t.Skip("no root-owned protected system executable is available on this host")
}

func TestValidateTrustedProgramPathResolvesSymlinkAliases(t *testing.T) {
	alias := "/bin/sh"
	resolvedByOS, err := filepath.EvalSymlinks(alias)
	if err != nil || filepath.Clean(resolvedByOS) == filepath.Clean(alias) {
		t.Skip("this platform has no /bin/sh symlink alias")
	}
	resolved, err := validateTrustedProgramPath(alias)
	if err != nil {
		t.Skipf("platform system executable is not root-owned and protected: %v", err)
	}
	if filepath.Clean(resolved) != filepath.Clean(resolvedByOS) {
		t.Fatalf("symlink alias was not resolved to its actual target: got %q want %q", resolved, resolvedByOS)
	}
}

func TestValidateTrustedProgramPathRejectsWritableAncestors(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to create a root-owned executable beneath an unsafe directory")
	}
	base := t.TempDir()
	unsafe := filepath.Join(base, "shared")
	if err := os.Mkdir(unsafe, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafe, 0777); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(unsafe, "worker")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := validateTrustedProgramPath(program); err == nil || !strings.Contains(err.Error(), "directories") {
		t.Fatalf("program beneath a writable ancestor was accepted or failed for the wrong reason: %v", err)
	}
}

func TestValidateTrustedProgramPathRejectsUntrustedExecutables(t *testing.T) {
	program := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		// A root-run test otherwise creates a root-owned file that satisfies the
		// helper's ownership policy. Give the executable an untrusted owner while
		// keeping its path and mode protected, so this assertion tests ownership.
		if err := os.Chown(program, 65534, 65534); err != nil {
			t.Skipf("cannot assign an untrusted executable owner: %v", err)
		}
	}
	if _, err := validateTrustedProgramPath(program); err == nil {
		t.Fatal("an executable owned by an untrusted user was accepted as root-trusted")
	}
}
