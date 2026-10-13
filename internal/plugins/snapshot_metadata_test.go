package plugins

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRuntimeSnapshotPreservesModesAndOwnership(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "source")
	if err := os.MkdirAll(filepath.Join(source, "pools"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "pools", "default.conf"), []byte("controlled configuration"), 0640); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{source, filepath.Join(source, "pools")} {
		if err := os.Chmod(path, 0750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(source, "pools", "default.conf"), 0640); err != nil {
		t.Fatal(err)
	}
	// Group zero is harmless for this owned fixture but distinct from its
	// service group in root VM runs, exposing dropped-GID rollback bugs.
	if os.Geteuid() == 0 {
		for _, path := range []string{source, filepath.Join(source, "pools"), filepath.Join(source, "pools", "default.conf")} {
			if err := os.Chown(path, 0, 65534); err != nil {
				t.Fatal(err)
			}
		}
	}
	oldUmask := syscall.Umask(0077)
	defer syscall.Umask(oldUmask)
	snapshot := filepath.Join(base, "snapshot")
	if err := copyTree(context.Background(), source, snapshot); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(base, "restored")
	if err := copyTree(context.Background(), snapshot, restored); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".", "pools", "pools/default.conf"} {
		original, err := os.Stat(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range []string{snapshot, restored} {
			actual, err := os.Stat(filepath.Join(target, name))
			if err != nil {
				t.Fatal(err)
			}
			a, b := original.Sys().(*syscall.Stat_t), actual.Sys().(*syscall.Stat_t)
			if actual.Mode().Perm() != original.Mode().Perm() || a.Uid != b.Uid || a.Gid != b.Gid {
				t.Fatalf("rollback metadata changed for %s: original %o %d:%d actual %o %d:%d", name, original.Mode().Perm(), a.Uid, a.Gid, actual.Mode().Perm(), b.Uid, b.Gid)
			}
		}
	}
}

func TestOwnedConfigurationModeDoesNotDependOnAgentUmask(t *testing.T) {
	base := t.TempDir()
	oldUmask := syscall.Umask(0077)
	defer syscall.Umask(oldUmask)
	if err := writeAtomicRoot(base, "php.ini", []byte("memory_limit=96M\n"), 0640, os.Getgid()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(base, "php.ini"))
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatal("ordinary runtime CLI cannot read its explicitly group-readable config", info, err)
	}
}
