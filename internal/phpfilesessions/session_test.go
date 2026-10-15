package phpfilesessions

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func testSessionRoots(t *testing.T) (*os.Root, *os.Root, FileIdentity) {
	t.Helper()
	base := t.TempDir()
	sourcePath := filepath.Join(base, "sessions")
	quarantinePath := filepath.Join(sourcePath, ".quarantine")
	if err := os.Mkdir(sourcePath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(quarantinePath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "sess_candidate"), []byte("session-data"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err := os.OpenRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	quarantine, err := os.OpenRoot(quarantinePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = quarantine.Close() })
	info, err := source.Lstat("sess_candidate")
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	identity, err := Identify("sess_candidate", info, stat.Uid, stat.Gid)
	if err != nil {
		t.Fatal(err)
	}
	return source, quarantine, identity
}

func TestRemoveUsesRootPrivateSameFilesystemQuarantine(t *testing.T) {
	source, quarantine, identity := testSessionRoots(t)
	locked, err := Lock(source, identity, identity.UID, identity.GID)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	if err = Remove(source, quarantine, locked, ".php-session-a1"); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Lstat(identity.Name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session file remains after removal: %v", err)
	}
	if _, err = quarantine.Lstat(".php-session-a1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quarantine entry remains after removal: %v", err)
	}
}

func TestRemovePreservesReplacementAtQuarantineName(t *testing.T) {
	source, quarantine, identity := testSessionRoots(t)
	locked, err := Lock(source, identity, identity.UID, identity.GID)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	replacementName := ".php-session-a2"
	err = remove(source, quarantine, locked, replacementName, func(root *os.Root, name string) error {
		if err := root.Rename(name, name+".original"); err != nil {
			return err
		}
		file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write([]byte("unpreviewed replacement"))
		return errors.Join(writeErr, file.Close())
	})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("replacement race should be rejected, got %v", err)
	}
	data, err := quarantine.Open(replacementName)
	if err != nil {
		t.Fatal("replacement file was removed", err)
	}
	buf := make([]byte, len("unpreviewed replacement"))
	if _, err = data.Read(buf); err != nil {
		data.Close()
		t.Fatal(err)
	}
	_ = data.Close()
	if string(buf) != "unpreviewed replacement" {
		t.Fatalf("replacement contents changed: %q", buf)
	}
	if _, err = quarantine.Lstat(replacementName + ".original"); err != nil {
		t.Fatalf("original candidate was lost from private quarantine: %v", err)
	}
}

func TestEnsureQuarantineRequiresPrivateOwnedDirectory(t *testing.T) {
	parentPath := t.TempDir()
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	root, err := EnsureQuarantine(parent, ".private")
	if err != nil {
		t.Fatal(err)
	}
	root.Close()
	if err = os.Chmod(filepath.Join(parentPath, ".private"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err = EnsureQuarantine(parent, ".private"); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("non-private quarantine accepted: %v", err)
	}
}
