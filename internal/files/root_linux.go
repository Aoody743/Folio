//go:build linux

package files

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Open the absolute path without following any component's symlink, then
// anchor os.Root to that exact descriptor. A renamed pathname cannot redirect
// privileged operations to a different directory after this point.
func openPinnedRoot(spec RootSpec) (*os.Root, error) {
	fd, err := unix.Openat2(unix.AT_FDCWD, filepath.Clean(spec.Path), &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, fmt.Errorf("managed root needs a symlink-free path: %w", err)
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if (spec.ExpectedDevice != 0 && spec.ExpectedDevice != uint64(stat.Dev)) || (spec.ExpectedInode != 0 && spec.ExpectedInode != stat.Ino) {
		return nil, errors.New("managed root identity differs from its trusted receipt")
	}
	return os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", fd))
}

// Every managed-path open rejects intermediate symlinks too. os.Root protects
// containment, but a host root also needs to prevent links to protected paths
// that are themselves inside its broad boundary.
func strictOpen(root *os.Root, path string, flags int, mode fs.FileMode) (*os.File, error) {
	base, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer base.Close()
	permissions := uint64(0)
	if flags&unix.O_CREAT != 0 {
		permissions = uint64(mode.Perm())
	}
	fd, err := unix.Openat2(int(base.Fd()), path, &unix.OpenHow{
		Flags: uint64(flags | unix.O_CLOEXEC), Mode: permissions,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV) {
			return nil, failure("unsafe_path", "Symbolic link paths and root escapes are forbidden", 403)
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), fmt.Sprintf("/proc/self/fd/%d", fd)), nil
}

func strictLstat(root *os.Root, path string) (fs.FileInfo, error) {
	file, err := strictOpen(root, path, unix.O_PATH|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return file.Stat()
}

func strictSubroot(root *os.Root, path string) (*os.Root, error) {
	file, err := strictOpen(root, path, unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return os.OpenRoot(file.Name())
}

func strictMkdir(root *os.Root, path string, mode fs.FileMode) error {
	parent, err := strictSubroot(root, filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Mkdir(filepath.Base(path), mode)
}

func strictRemove(root *os.Root, path string, recursive bool) error {
	parent, err := strictSubroot(root, filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	if recursive {
		return parent.RemoveAll(filepath.Base(path))
	}
	return parent.Remove(filepath.Base(path))
}

func renameDescriptors(source int, sourceName string, destination int, destinationName string, flags uint) error {
	return unix.Renameat2(source, sourceName, destination, destinationName, flags)
}
