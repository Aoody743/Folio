package files

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// Only use on implementation-owned staging containers, never public content.
// Copied archive modes may prohibit deletion by an unprivileged service, so
// restore owner traversal/write bits in private directories before cleanup.
func removePrivateTree(root *os.Root, path string) error {
	var writable func(string) error
	writable = func(name string) error {
		info, err := strictLstat(root, name)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return nil
		}
		identity, err := strictOpen(root, name, unix.O_PATH|syscall.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		// chmod through our pinned descriptor also works for a directory whose
		// archive mode is 0000; opening it for reading would fail first.
		err = os.Chmod(identity.Name(), info.Mode().Perm()|0700)
		identity.Close()
		if err != nil {
			return err
		}
		directory, err := strictOpen(root, name, os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		defer directory.Close()
		entries, err := directory.ReadDir(-1)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := writable(filepath.Join(name, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	if err := writable(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return root.RemoveAll(path)
}
