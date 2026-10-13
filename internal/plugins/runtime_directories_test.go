package plugins

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRuntimeOwnedAncestorsRemainWorkerTraversableUnderPrivateUmask(t *testing.T) {
	e := testAgent(t)
	paths := e.instancePaths("postgresql", "umask")
	previous := syscall.Umask(0077)
	defer syscall.Umask(previous)
	if err := e.prepareInstanceDirectories(paths); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Dir(paths.Root), paths.Root, paths.ConfigDir} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0750 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
			t.Fatal("managed root ancestor is not protected and worker traversable", path, err)
		}
		// Simulate an earlier root:worker directory created by umask0077.
		if err := os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.prepareInstanceDirectories(paths); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Dir(paths.Root), paths.Root, paths.ConfigDir} {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0750 {
			t.Fatal("existing inaccessible owned ancestor was not repaired", path)
		}
	}
	backup, _ := os.Stat(paths.BackupDir)
	private, _ := os.Stat(e.config.StateDir)
	if backup.Mode().Perm() != 0700 || private.Mode().Perm() != 0700 {
		t.Fatal("runtime traversal repair exposed private backup/Agent state")
	}
}

func TestRuntimeAncestorRepairRejectsUnknownLayoutAndSymlink(t *testing.T) {
	e := testAgent(t)
	if err := e.prepareInstanceDirectories(Paths{}); err == nil {
		t.Fatal("missing layout could alter the current working directory")
	}
	paths := e.instancePaths("postgresql", "symlink")
	if err := os.MkdirAll(filepath.Dir(paths.Root), 0750); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	before, _ := os.Stat(outside)
	if err := os.Symlink(outside, paths.Root); err != nil {
		t.Fatal(err)
	}
	if err := e.prepareInstanceDirectories(paths); err == nil {
		t.Fatal("symlink software layout was adopted")
	}
	after, _ := os.Stat(outside)
	if after.Mode() != before.Mode() {
		t.Fatal("foreign symlink target permissions changed")
	}
}
