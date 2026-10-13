package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/transform"
)

type managedRoot struct {
	spec   RootSpec
	root   *os.Root
	gate   chan struct{}
	active int
}
type Service struct {
	mu          sync.RWMutex
	roots       map[string]*managedRoot
	state       *os.Root
	store       MetadataStore
	options     Options
	maintenance map[string]*rootMaintenance
}

var safeID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,79}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func New(options Options) (*Service, error) {
	if options.MaxTextBytes == 0 {
		options.MaxTextBytes = 2 << 20
	}
	if options.MaxTransferBytes == 0 {
		options.MaxTransferBytes = 2 << 30
	}
	if options.MaxArchiveEntries == 0 {
		options.MaxArchiveEntries = 10000
	}
	if options.MaxTextBytes < 1 || options.MaxTextBytes > 8<<20 || options.MaxTransferBytes < 1 || options.MaxTransferBytes > 64<<30 || options.MaxArchiveEntries < 1 || options.MaxArchiveEntries > 100000 || !filepath.IsAbs(options.StateDir) {
		return nil, errors.New("invalid filesystem limits/private state directory")
	}
	for _, path := range options.ProtectedPaths {
		if !filepath.IsAbs(path) {
			return nil, errors.New("protected filesystem paths must be absolute")
		}
	}
	if err := os.MkdirAll(options.StateDir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(options.StateDir)
	if err != nil {
		return nil, err
	}
	identity, valid := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !valid || identity.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("file state directory must be private and not a symlink")
	}
	state, err := openPinnedRoot(RootSpec{Path: options.StateDir})
	if err != nil {
		return nil, err
	}
	s := &Service{roots: map[string]*managedRoot{}, maintenance: map[string]*rootMaintenance{}, state: state, store: options.Store, options: options}
	for _, name := range []string{"history", "uploads", "trash", "metadata", "staging"} {
		if err := state.MkdirAll(name, 0700); err != nil {
			s.Close()
			return nil, err
		}
	}
	if s.store == nil {
		s.store = &fileMetadata{root: state}
	}
	for _, spec := range options.Roots {
		if err := s.AddRoot(spec); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	for _, r := range s.roots {
		err = errors.Join(err, r.root.Close())
	}
	return errors.Join(err, s.state.Close())
}
func clean(name string) (string, error) {
	if name == "" {
		name = "."
	}
	if filepath.IsAbs(name) || strings.ContainsAny(name, "\x00\\") {
		return "", failure("unsafe_path", "Only relative managed paths are accepted", 403)
	}
	for _, p := range strings.Split(name, "/") {
		if p == ".." {
			return "", failure("unsafe_path", "Parent path traversal is forbidden", 403)
		}
	}
	return filepath.Clean(name), nil
}
func (s *Service) resolve(ref Ref, write bool) (*managedRoot, string, error) {
	s.mu.RLock()
	r, ok := s.roots[ref.Root]
	s.mu.RUnlock()
	if !ok {
		return nil, "", failure("unsafe_path", "Unknown filesystem root", 403)
	}
	path, err := clean(ref.Path)
	if err != nil {
		return nil, "", err
	}
	if err := s.guardRootPath(r, path, write); err != nil {
		return nil, "", err
	}
	if write && (r.spec.ReadOnly || path == ".") {
		return nil, "", failure("read_only", "Filesystem root or root directory cannot be modified", 403)
	}
	for _, prefix := range pathPrefixes(path) {
		info, err := strictLstat(r.root, prefix)
		if err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return nil, "", failure("unsafe_path", "Symbolic link paths are forbidden", 403)
		}
		if errors.Is(err, syscall.ELOOP) || isRootEscape(err) {
			return nil, "", failure("unsafe_path", "Filesystem path escapes its managed root", 403)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, "", err
		}
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
	}
	return r, path, nil
}

func (s *Service) guardRootPath(root *managedRoot, path string, write bool) error {
	if root.spec.HostAccess {
		candidate := filepath.Join(root.spec.Path, path)
		for _, protected := range append([]string{s.options.StateDir}, s.options.ProtectedPaths...) {
			if insidePath(protected, candidate) || (write && insidePath(candidate, protected)) {
				return failure("protected_path", "Private panel and Agent state is protected", 403)
			}
		}
	}
	return nil
}

// os.Root's escape sentinel is unexported in the pinned toolchain. This only
// classifies its precise PathError; containment remains enforced by os.Root.
func isRootEscape(err error) bool {
	var pathError *os.PathError
	return errors.As(err, &pathError) && pathError.Err.Error() == "path escapes from parent"
}
func pathPrefixes(path string) []string {
	if path == "." {
		return nil
	}
	parts := strings.Split(path, "/")
	out := []string{}
	for i := range parts {
		out = append(out, filepath.Join(parts[:i+1]...))
	}
	return out
}
func (s *Service) lock(ctx context.Context, rootIDs ...string) (func(), error) {
	sort.Strings(rootIDs)
	locks := []*managedRoot{}
	last := ""
	for _, id := range rootIDs {
		if id == last {
			continue
		}
		last = id
		s.mu.RLock()
		r, ok := s.roots[id]
		s.mu.RUnlock()
		if !ok {
			for _, locked := range locks {
				<-locked.gate
			}
			return nil, failure("unsafe_path", "Unknown filesystem root", 403)
		}
		select {
		case r.gate <- struct{}{}:
			locks = append(locks, r)
		case <-ctx.Done():
			for _, locked := range locks {
				<-locked.gate
			}
			return nil, ctx.Err()
		}
	}
	return func() {
		for _, r := range locks {
			<-r.gate
		}
	}, nil
}
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func encode(value any) json.RawMessage { b, _ := json.Marshal(value); return b }
func parse(raw json.RawMessage) (Input, error) {
	var in Input
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, failure("invalid_input", "Invalid filesystem parameters", 400)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return in, failure("invalid_input", "Expected one JSON document", 400)
	}
	return in, nil
}
func openRegular(r *os.Root, path string, flags int) (*os.File, fs.FileInfo, error) {
	f, err := strictOpen(r, path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, nil, failure("unsafe_path", "Symbolic link access is forbidden", 403)
		}
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, failure("unsafe_path", "Only regular files are supported", 403)
	}
	return f, info, nil
}
func fileEntry(name, path string, info fs.FileInfo) Entry {
	kind := "file"
	if info.IsDir() {
		kind = "directory"
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		kind = "symlink"
	}
	e := Entry{Name: name, Path: path, Type: kind, Size: info.Size(), Mode: uint32(info.Mode().Perm()), ModifiedAt: info.ModTime().UTC()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		e.Owner, e.Group = st.Uid, st.Gid
	}
	return e
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func hashReader(ctx context.Context, r io.Reader) (string, int64, error) {
	hash := sha256.New()
	n, err := io.CopyBuffer(hash, contextReader{ctx, r}, make([]byte, 32<<10))
	return hex.EncodeToString(hash.Sum(nil)), n, err
}
func (s *Service) hash(ctx context.Context, ref Ref) (string, error) {
	r, path, err := s.resolve(ref, false)
	if err != nil {
		return "", err
	}
	f, _, err := openRegular(r.root, path, os.O_RDONLY)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash, _, err := hashReader(ctx, f)
	return hash, err
}
func encodingFor(name string) encoding.Encoding {
	switch strings.ToLower(name) {
	case "gbk":
		return simplifiedchinese.GBK
	case "gb18030":
		return simplifiedchinese.GB18030
	case "big5":
		return traditionalchinese.Big5
	}
	return nil
}
func decodeText(data []byte, name string) (string, string, error) {
	if name == "" {
		if utf8.Valid(data) {
			name = "utf-8"
		} else {
			for _, candidate := range []string{"gb18030", "gbk", "big5"} {
				out, encoding, err := decodeText(data, candidate)
				if err == nil {
					return out, encoding, nil
				}
			}
			return "", "", failure("unsupported_encoding", "Cannot decode file without data loss", 400)
		}
	}
	if name == "utf8" {
		name = "utf-8"
	}
	if name == "utf-8" {
		if !utf8.Valid(data) {
			return "", "", failure("unsupported_encoding", "File is not valid UTF-8", 400)
		}
		return string(data), name, nil
	}
	enc := encodingFor(name)
	if enc == nil {
		return "", "", failure("unsupported_encoding", "Encoding must be utf-8, GB18030, GBK or BIG5", 400)
	}
	out, _, err := transform.Bytes(enc.NewDecoder(), data)
	if err != nil {
		return "", "", err
	}
	if strings.ContainsRune(string(out), utf8.RuneError) {
		return "", "", failure("unsupported_encoding", "Invalid encoded byte sequence", 400)
	}
	return string(out), strings.ToLower(name), nil
}
func encodeText(content, name string) ([]byte, error) {
	if name == "" || name == "utf-8" || name == "utf8" {
		return []byte(content), nil
	}
	enc := encodingFor(name)
	if enc == nil {
		return nil, failure("unsupported_encoding", "Encoding must be utf-8, GB18030, GBK or BIG5", 400)
	}
	out, _, err := transform.Bytes(enc.NewEncoder(), []byte(content))
	return out, err
}

func (s *Service) Roots() []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []map[string]any{}
	for _, r := range s.roots {
		if r.spec.internal {
			continue
		}
		out = append(out, map[string]any{"id": r.spec.ID, "label": r.spec.Label, "read_only": r.spec.ReadOnly, "host_access": r.spec.HostAccess})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
	return out
}
func (s *Service) List(ctx context.Context, in Input) (Listing, error) {
	r, path, err := s.resolve(Ref{in.Root, in.Path}, false)
	if err != nil {
		return Listing{}, err
	}
	if in.Limit == 0 {
		in.Limit = 100
	}
	if in.Limit < 1 || in.Limit > 500 || in.Offset < 0 {
		return Listing{}, failure("invalid_input", "Invalid pagination", 400)
	}
	dir, err := strictOpen(r.root, path, os.O_RDONLY, 0)
	if err != nil {
		return Listing{}, err
	}
	defer dir.Close()
	out := Listing{Items: []Entry{}, Path: path, Limit: in.Limit, Offset: in.Offset}
	all := []Entry{}
	for {
		entries, err := dir.ReadDir(256)
		for _, e := range entries {
			if ctx.Err() != nil {
				return Listing{}, ctx.Err()
			}
			if in.Query != "" && !strings.Contains(strings.ToLower(e.Name()), strings.ToLower(in.Query)) {
				continue
			}
			if err := s.guardRootPath(r, filepath.Join(path, e.Name()), false); err != nil {
				continue
			}
			info, err := e.Info()
			if err != nil {
				return Listing{}, err
			}
			all = append(all, fileEntry(e.Name(), filepath.Join(path, e.Name()), info))
			if len(all) > 100000 {
				return Listing{}, failure("too_large", "Directory exceeds managed listing budget", 413)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Listing{}, err
		}
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		less := false
		switch in.Sort {
		case "size":
			if a.Size == b.Size {
				less = a.Name < b.Name
			} else {
				less = a.Size < b.Size
			}
		case "modified_at", "mtime":
			if a.ModifiedAt.Equal(b.ModifiedAt) {
				less = a.Name < b.Name
			} else {
				less = a.ModifiedAt.Before(b.ModifiedAt)
			}
		default:
			less = a.Name < b.Name
		}
		if in.Order == "desc" {
			return !less && a.Name != b.Name
		}
		return less
	})
	out.Total = len(all)
	if in.Offset < len(all) {
		end := min(in.Offset+in.Limit, len(all))
		out.Items = all[in.Offset:end]
	}
	return out, nil
}
func (s *Service) ReadText(ctx context.Context, in Input) (Text, error) {
	r, path, err := s.resolve(Ref{in.Root, in.Path}, false)
	if err != nil {
		return Text{}, err
	}
	f, info, err := openRegular(r.root, path, os.O_RDONLY)
	if err != nil {
		return Text{}, err
	}
	defer f.Close()
	if info.Size() > s.options.MaxTextBytes {
		return Text{}, failure("too_large", "Use streaming download or tail for files above the editing budget", 413)
	}
	limit := int64(64 << 10)
	if in.Limit > 0 {
		limit = min(int64(in.Limit), s.options.MaxTextBytes)
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, f}, s.options.MaxTextBytes+1))
	if err != nil {
		return Text{}, err
	}
	if int64(len(data)) > s.options.MaxTextBytes {
		return Text{}, failure("too_large", "File grew above editing budget", 413)
	}
	content, name, err := decodeText(data, in.Encoding)
	if err != nil {
		return Text{}, err
	}
	truncated := int64(len(content)) > limit
	if truncated {
		end := int(limit)
		for end > 0 && !utf8.RuneStart(content[end]) {
			end--
		}
		content = content[:end]
	}
	digest := sha256.Sum256(data)
	return Text{Content: content, Encoding: name, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(data)), Truncated: truncated}, nil
}
func (s *Service) checkExpected(ctx context.Context, ref Ref, expected string, overwrite bool) error {
	hash, err := s.hash(ctx, ref)
	if err != nil {
		return err
	}
	if hash != "" && expected == "" && !overwrite {
		return failure("conflict", "Destination exists; supply expected_hash or explicit overwrite", 409)
	}
	if expected != "" && hash != expected {
		return &Error{Code: "conflict", Message: "File changed since it was read", Status: 409, Details: map[string]any{"current_hash": hash}}
	}
	return nil
}
func (s *Service) writeAtomic(ctx context.Context, ref Ref, reader io.Reader, maxBytes int64) (Entry, string, error) {
	r, path, err := s.resolve(ref, true)
	if err != nil {
		return Entry{}, "", err
	}
	mode := r.spec.DefaultFileMode
	var uid, gid int = -1, -1
	if info, err := strictLstat(r.root, path); err == nil {
		if !info.Mode().IsRegular() {
			return Entry{}, "", failure("unsafe_path", "Destination is not a regular file", 403)
		}
		mode = info.Mode().Perm()
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Entry{}, "", err
	} else {
		parentInfo, err := strictLstat(r.root, filepath.Dir(path))
		if err != nil {
			return Entry{}, "", err
		}
		if st, ok := parentInfo.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
	}
	temp := filepath.Join(filepath.Dir(path), ".powerlinux-"+newID()+".tmp")
	out, err := strictOpen(r.root, temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return Entry{}, "", err
	}
	defer strictRemove(r.root, temp, false)
	hash := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(out, hash), io.LimitReader(contextReader{ctx, reader}, maxBytes+1), make([]byte, 32<<10))
	if err == nil && n > maxBytes {
		err = failure("too_large", "File exceeds transfer budget", 413)
	}
	if err == nil {
		err = out.Chmod(mode)
	}
	if err == nil && uid >= 0 {
		if info, e := out.Stat(); e == nil {
			st := info.Sys().(*syscall.Stat_t)
			if int(st.Uid) != uid || int(st.Gid) != gid {
				err = out.Chown(uid, gid)
			}
		} else {
			err = e
		}
	}
	if err == nil {
		err = out.Sync()
	}
	err = errors.Join(err, out.Close())
	if err != nil {
		return Entry{}, "", err
	}
	if err := ctx.Err(); err != nil {
		return Entry{}, "", err
	}
	if err := renameBetween(r.root, temp, r.root, path); err != nil {
		return Entry{}, "", err
	}
	parent, err := strictOpen(r.root, filepath.Dir(path), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return Entry{}, "", err
	}
	err = parent.Sync()
	parent.Close()
	if err != nil {
		return Entry{}, "", err
	}
	info, err := strictLstat(r.root, path)
	if err != nil {
		return Entry{}, "", err
	}
	return fileEntry(filepath.Base(path), path, info), hex.EncodeToString(hash.Sum(nil)), nil
}
func (s *Service) snapshot(ctx context.Context, actorID string, ref Ref, enc string) (*Revision, error) {
	r, path, err := s.resolve(ref, false)
	if err != nil {
		return nil, err
	}
	f, info, err := openRegular(r.root, path, os.O_RDONLY)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info.Size() > s.options.MaxTextBytes {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, f}, s.options.MaxTextBytes+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if actorID == "external" {
		all, err := s.store.List(ctx, "file_history")
		if err != nil {
			return nil, err
		}
		for _, raw := range all {
			var old Revision
			if err := json.Unmarshal(raw, &old); err != nil {
				return nil, err
			}
			if old.Root == ref.Root && old.Path == path && old.SHA256 == hash {
				return &old, nil
			}
		}
	}
	if err := s.state.WriteFile(filepath.Join("history", hash), data, 0600); err != nil {
		return nil, err
	}
	rev := Revision{ID: newID(), Root: ref.Root, Path: path, SHA256: hash, ActorID: actorID, CreatedAt: time.Now().UTC(), Size: int64(len(data)), Encoding: enc}
	if err := s.store.Save(ctx, "file_history", rev.ID, encode(rev)); err != nil {
		return nil, err
	}
	return &rev, nil
}
func (s *Service) Edit(ctx context.Context, actorID string, in Input) (any, error) {
	data, err := encodeText(in.Content, in.Encoding)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > s.options.MaxTextBytes {
		return nil, failure("too_large", "Text editing is limited to 2 MiB by default", 413)
	}
	ref := Ref{in.Root, in.Path}
	unlock, err := s.lock(ctx, in.Root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.checkExpected(ctx, ref, in.ExpectedHash, in.Overwrite); err != nil {
		return nil, err
	}
	if _, err := s.snapshot(ctx, "external", ref, in.Encoding); err != nil {
		return nil, err
	}
	entry, hash, err := s.writeAtomic(ctx, ref, bytes.NewReader(data), s.options.MaxTextBytes)
	if err != nil {
		return nil, err
	}
	revision, err := s.snapshot(ctx, actorID, ref, in.Encoding)
	if err != nil {
		return nil, err
	}
	return map[string]any{"entry": entry, "sha256": hash, "revision": revision}, nil
}

type fileMetadata struct{ root *os.Root }

func metaPath(kind, id string) (string, error) {
	if !safeID.MatchString(kind) || !safeID.MatchString(id) {
		return "", errors.New("invalid metadata key")
	}
	return filepath.Join("metadata", kind, id+".json"), nil
}
func (m *fileMetadata) Save(ctx context.Context, kind, id string, raw json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := metaPath(kind, id)
	if err != nil {
		return err
	}
	if err := m.root.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temp := path + "." + newID() + ".tmp"
	f, err := m.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer m.root.Remove(temp)
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return m.root.Rename(temp, path)
}
func (m *fileMetadata) Get(ctx context.Context, kind, id string) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := metaPath(kind, id)
	if err != nil {
		return nil, err
	}
	f, _, err := openRegular(m.root, path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 8<<20))
}
func (m *fileMetadata) List(ctx context.Context, kind string) (map[string]json.RawMessage, error) {
	if !safeID.MatchString(kind) {
		return nil, errors.New("invalid metadata kind")
	}
	dir, err := m.root.Open(filepath.Join("metadata", kind))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		raw, err := m.Get(ctx, kind, id)
		if err != nil {
			return nil, err
		}
		out[id] = raw
	}
	return out, nil
}
func (m *fileMetadata) Delete(ctx context.Context, kind, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := metaPath(kind, id)
	if err != nil {
		return err
	}
	return m.root.Remove(path)
}

// Metadata is suitable for injecting into another independent module.
func (s *Service) Metadata() MetadataStore { return s.store }

// AddRoot is only a trusted control-plane API. HTTP/MCP must never expose a
// user-supplied absolute path to it. Sites should register only their public
// directory, each with a distinct root ID, excluding private keys and secrets.
func (s *Service) AddRoot(spec RootSpec) error {
	if !safeID.MatchString(spec.ID) || !filepath.IsAbs(spec.Path) {
		return errors.New("invalid managed root")
	}
	spec.Path = filepath.Clean(spec.Path)
	for _, protected := range append([]string{s.options.StateDir}, s.options.ProtectedPaths...) {
		if insidePath(protected, spec.Path) || (!spec.HostAccess && insidePath(spec.Path, protected)) {
			return errors.New("managed root cannot expose private panel or Agent state")
		}
	}
	root, err := openPinnedRoot(spec)
	if err != nil {
		return err
	}
	if spec.DefaultFileMode == 0 {
		spec.DefaultFileMode = 0644
	}
	if spec.DefaultDirMode == 0 {
		spec.DefaultDirMode = 0750
	}
	if spec.Label == "" {
		spec.Label = spec.ID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.roots[spec.ID]; exists {
		root.Close()
		return errors.New("root already registered")
	}
	s.roots[spec.ID] = &managedRoot{spec: spec, root: root, gate: make(chan struct{}, 1)}
	return nil
}

func insidePath(parent, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func (s *Service) DeleteRoot(id string) error {
	s.mu.Lock()
	root, ok := s.roots[id]
	if ok {
		delete(s.roots, id)
	}
	s.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	s.mu.RLock()
	maintenance := s.maintenance[id]
	s.mu.RUnlock()
	if maintenance != nil && maintenance.prior == root {
		return nil
	}
	return root.root.Close()
}
