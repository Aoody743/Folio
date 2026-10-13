package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
)

var safeID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,79}$`)

type FileStore struct {
	root *os.Root
	mu   sync.Mutex
}

func NewFileStore(path string) (*FileStore, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("terminal state directory must be absolute")
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ok || st.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("terminal state must be private and owned by Agent UID")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	return &FileStore{root: root}, nil
}
func (s *FileStore) Close() error { return s.root.Close() }
func statePath(kind, id string) (string, error) {
	if !safeID.MatchString(kind) || !safeID.MatchString(id) {
		return "", errors.New("invalid terminal metadata key")
	}
	return filepath.Join(kind, id+".json"), nil
}
func (s *FileStore) Save(ctx context.Context, kind, id string, data json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := statePath(kind, id)
	if err != nil {
		return err
	}
	if len(data) > 64<<10 {
		return errors.New("terminal metadata is too large")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.root.MkdirAll(kind, 0700); err != nil {
		return err
	}
	name := path + "." + newID() + ".tmp"
	file, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer s.root.Remove(name)
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err := s.root.Rename(name, path); err != nil {
		return err
	}
	dir, err := s.root.Open(kind)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *FileStore) Get(ctx context.Context, kind, id string) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := statePath(kind, id)
	if err != nil {
		return nil, err
	}
	file, err := s.root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, errors.New("invalid terminal metadata record")
	}
	return io.ReadAll(io.LimitReader(file, 64<<10))
}
func (s *FileStore) List(ctx context.Context, kind string) (map[string]json.RawMessage, error) {
	if !safeID.MatchString(kind) {
		return nil, errors.New("invalid terminal metadata kind")
	}
	dir, err := s.root.Open(kind)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(10001)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > 10000 {
		return nil, errors.New("terminal metadata exceeds record budget")
	}
	out := map[string]json.RawMessage{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		raw, err := s.Get(ctx, kind, id)
		if err != nil {
			return nil, err
		}
		out[id] = raw
	}
	return out, nil
}
func (s *FileStore) Delete(ctx context.Context, kind, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := statePath(kind, id)
	if err != nil {
		return err
	}
	return s.root.Remove(path)
}
