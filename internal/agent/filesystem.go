package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

var (
	ErrUnsafePath   = errors.New("path is outside an allowed filesystem root")
	ErrReadOnly     = errors.New("filesystem root is read-only")
	ErrFileTooLarge = errors.New("file exceeds configured size limit")
)

type RootSpec struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"read_only"`
}

type scopedRoot struct {
	root     *os.Root
	readOnly bool
	mu       sync.Mutex
}

// Filesystem uses descriptor-relative os.Root operations, not a vulnerable
// validate-then-open absolute path. Only explicitly installed root IDs exist.
// Operators must not grant roots containing device mounts or /proc; regular
// file reads are checked before reading and directories are listed separately.
type Filesystem struct {
	roots        map[string]*scopedRoot
	maxFileBytes int64
}

type Entry struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Directory bool   `json:"directory"`
	Symlink   bool   `json:"symlink"`
	Mode      uint32 `json:"mode"`
}

func NewFilesystem(specs []RootSpec, maxFileBytes int64) (*Filesystem, error) {
	if maxFileBytes == 0 {
		maxFileBytes = 8 << 20
	}
	if maxFileBytes < 1 || maxFileBytes > 128<<20 {
		return nil, errors.New("invalid file size limit")
	}
	f := &Filesystem{roots: make(map[string]*scopedRoot), maxFileBytes: maxFileBytes}
	for _, spec := range specs {
		if !operationName.MatchString(spec.ID) || !filepath.IsAbs(spec.Path) {
			f.Close()
			return nil, errors.New("filesystem root needs a valid ID and absolute path")
		}
		if _, exists := f.roots[spec.ID]; exists {
			f.Close()
			return nil, errors.New("duplicate filesystem root")
		}
		root, err := os.OpenRoot(spec.Path)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("open root %q: %w", spec.ID, err)
		}
		f.roots[spec.ID] = &scopedRoot{root: root, readOnly: spec.ReadOnly}
	}
	return f, nil
}

func (f *Filesystem) Close() error {
	var result error
	for _, root := range f.roots {
		result = errors.Join(result, root.root.Close())
	}
	return result
}

func (f *Filesystem) resolve(rootID, name string, write bool) (*scopedRoot, string, error) {
	root, ok := f.roots[rootID]
	if !ok {
		return nil, "", ErrUnsafePath
	}
	if name == "" || filepath.IsAbs(name) || strings.ContainsAny(name, "\x00\\") {
		return nil, "", ErrUnsafePath
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return nil, "", ErrUnsafePath
		}
	}
	name = filepath.Clean(name)
	if write && (root.readOnly || name == ".") {
		if root.readOnly {
			return nil, "", ErrReadOnly
		}
		return nil, "", ErrUnsafePath
	}
	return root, name, nil
}

func (f *Filesystem) ReadFile(rootID, name string) ([]byte, error) {
	root, name, err := f.resolve(rootID, name, false)
	if err != nil {
		return nil, err
	}
	file, err := root.root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open scoped file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("only regular files can be read")
	}
	if info.Size() > f.maxFileBytes {
		return nil, ErrFileTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(file, f.maxFileBytes+1))
	if int64(len(data)) > f.maxFileBytes {
		return nil, ErrFileTooLarge
	}
	return data, err
}

// WriteFile atomically replaces a regular file using a private file in the
// same rooted directory. It never follows a destination symlink, and uses
// restrictive permissions independent of the process umask.
func (f *Filesystem) WriteFile(rootID, name string, data []byte) error {
	root, name, err := f.resolve(rootID, name, true)
	if err != nil {
		return err
	}
	if int64(len(data)) > f.maxFileBytes {
		return ErrFileTooLarge
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	if info, err := root.root.Lstat(name); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("write destination must be a regular file")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	temp := filepath.Join(filepath.Dir(name), ".powerlinux-"+hex.EncodeToString(random[:])+".tmp")
	file, err := root.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer root.root.Remove(temp)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err := root.root.Rename(temp, name); err != nil {
		return err
	}
	dir, err := root.root.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (f *Filesystem) List(rootID, name string, limit int) ([]Entry, error) {
	root, name, err := f.resolve(rootID, name, false)
	if err != nil {
		return nil, err
	}
	if limit < 1 || limit > 10000 {
		return nil, errors.New("listing limit must be between 1 and 10000")
	}
	dir, err := root.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(limit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > limit {
		return nil, errors.New("directory exceeds listing limit; use a narrower root")
	}
	result := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		result = append(result, Entry{Name: entry.Name(), Size: info.Size(), Directory: entry.IsDir(), Symlink: entry.Type()&fs.ModeSymlink != 0, Mode: uint32(info.Mode().Perm())})
	}
	return result, nil
}

func (f *Filesystem) Mkdir(rootID, name string) error {
	root, name, err := f.resolve(rootID, name, true)
	if err != nil {
		return err
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	return root.root.Mkdir(name, 0700)
}

// Remove deliberately does not recursively remove directories.
func (f *Filesystem) Remove(rootID, name string) error {
	root, name, err := f.resolve(rootID, name, true)
	if err != nil {
		return err
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	return root.root.Remove(name)
}

func (f *Filesystem) Rename(rootID, oldName, newName string) error {
	root, oldName, err := f.resolve(rootID, oldName, true)
	if err != nil {
		return err
	}
	_, newName, err = f.resolve(rootID, newName, true)
	if err != nil {
		return err
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	if _, err := root.root.Lstat(newName); err == nil {
		return errors.New("rename destination already exists")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return root.root.Rename(oldName, newName)
}
