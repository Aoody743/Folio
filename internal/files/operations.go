package files

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

func (s *Service) History(ctx context.Context, in Input) (any, error) {
	_, path, err := s.resolve(Ref{in.Root, in.Path}, false)
	if err != nil {
		return nil, err
	}
	all, err := s.store.List(ctx, "file_history")
	if err != nil {
		return nil, err
	}
	out := []Revision{}
	for _, raw := range all {
		var rev Revision
		if err := json.Unmarshal(raw, &rev); err != nil {
			return nil, err
		}
		if rev.Root == in.Root && rev.Path == path {
			out = append(out, rev)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if in.ID != "" {
		for _, rev := range out {
			if rev.ID == in.ID {
				data, err := s.state.ReadFile(filepath.Join("history", rev.SHA256))
				if err != nil {
					return nil, err
				}
				text, enc, err := decodeText(data, rev.Encoding)
				if err != nil {
					return nil, err
				}
				return map[string]any{"revision": rev, "content": text, "encoding": enc}, nil
			}
		}
		return nil, failure("not_found", "Revision not found for this file", 404)
	}
	return out, nil
}
func (s *Service) Tail(ctx context.Context, in Input) (Text, error) {
	r, path, err := s.resolve(Ref{in.Root, in.Path}, false)
	if err != nil {
		return Text{}, err
	}
	f, info, err := openRegular(r.root, path, os.O_RDONLY)
	if err != nil {
		return Text{}, err
	}
	defer f.Close()
	limit := int64(64 << 10)
	start := max(int64(0), info.Size()-limit)
	if _, err := f.Seek(start, 0); err != nil {
		return Text{}, err
	}
	data, err := io.ReadAll(contextReader{ctx, f})
	if err != nil {
		return Text{}, err
	}
	if start > 0 {
		if index := strings.IndexByte(string(data), '\n'); index >= 0 {
			data = data[index+1:]
		}
	}
	lines := in.Lines
	if lines == 0 {
		lines = 100
	}
	if lines < 1 || lines > 1000 {
		return Text{}, failure("invalid_input", "Tail line count must be 1..1000", 400)
	}
	parts := strings.Split(string(data), "\n")
	endedNewline := len(data) > 0 && data[len(data)-1] == '\n'
	if endedNewline {
		parts = parts[:len(parts)-1]
	}
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	joined := strings.Join(parts, "\n")
	if endedNewline {
		joined += "\n"
	}
	text, enc, err := decodeText([]byte(joined), in.Encoding)
	if err != nil {
		return Text{}, err
	}
	return Text{Content: text, Encoding: enc, Size: info.Size(), Truncated: start > 0}, nil
}

func (s *Service) Walk(ctx context.Context, in Input, sizeOnly bool) (any, error) {
	r, base, err := s.resolve(Ref{in.Root, in.Path}, false)
	if err != nil {
		return nil, err
	}
	limit := in.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 || in.Offset < 0 {
		return nil, failure("invalid_input", "Invalid pagination", 400)
	}
	out := []Entry{}
	var totalBytes int64
	visited, total := 0, 0
	var visit func(string) error
	visit = func(path string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.guardRootPath(r, path, false); err != nil {
			return nil
		}
		info, err := strictLstat(r.root, path)
		if err != nil {
			return err
		}
		visited++
		if visited > 100000 {
			return failure("too_large", "Traversal exceeds 100000 entries", 413)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil
		}
		if !info.IsDir() {
			totalBytes += info.Size()
		}
		if path != base && (in.Query == "" || strings.Contains(strings.ToLower(filepath.Base(path)), strings.ToLower(in.Query))) {
			if !sizeOnly && total >= in.Offset && len(out) < limit {
				out = append(out, fileEntry(filepath.Base(path), path, info))
			}
			total++
		}
		if info.IsDir() {
			dir, err := strictOpen(r.root, path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			defer dir.Close()
			for {
				entries, err := dir.ReadDir(128)
				for _, entry := range entries {
					if err := visit(filepath.Join(path, entry.Name())); err != nil {
						return err
					}
				}
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(base); err != nil {
		return nil, err
	}
	if sizeOnly {
		return map[string]any{"bytes": totalBytes, "entries": visited, "path": base}, nil
	}
	return Listing{Items: out, Total: total, Offset: in.Offset, Limit: limit, Path: base}, nil
}

func (s *Service) Mkdir(ctx context.Context, in Input) (any, error) {
	r, path, err := s.resolve(Ref{in.Root, in.Path}, true)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lock(ctx, in.Root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	err = strictMkdir(r.root, path, r.spec.DefaultDirMode)
	if err != nil {
		return nil, err
	}
	dir, err := strictOpen(r.root, path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if err := inheritParentOwner(r.root, path, dir); err != nil {
		strictRemove(r.root, path, false)
		return nil, err
	}
	if err := dir.Chmod(r.spec.DefaultDirMode); err != nil {
		strictRemove(r.root, path, false)
		return nil, err
	}
	info, err := strictLstat(r.root, path)
	if err != nil {
		return nil, err
	}
	return fileEntry(filepath.Base(path), path, info), nil
}
func (s *Service) Copy(ctx context.Context, in Input, move bool) (any, error) {
	return s.CopyAs(ctx, "system", in, move)
}
func (s *Service) CopyAs(ctx context.Context, actorID string, in Input, move bool) (any, error) {
	source := Ref{in.Root, in.Path}
	dest := Ref{in.DestinationRoot, in.DestinationPath}
	sr, sp, err := s.resolve(source, move)
	if err != nil {
		return nil, err
	}
	dr, dp, err := s.resolve(dest, true)
	if err != nil {
		return nil, err
	}
	if sr == dr && (dp == sp || strings.HasPrefix(dp, sp+"/")) {
		return nil, failure("invalid_input", "Cannot copy or move a path into itself", 400)
	}
	unlock, err := s.lock(ctx, in.Root, in.DestinationRoot)
	if err != nil {
		return nil, err
	}
	defer unlock()
	info, err := strictLstat(sr.root, sp)
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, failure("unsafe_path", "Copying symbolic links is not allowed", 403)
	}
	if move {
		if !info.IsDir() {
			if err := s.checkExpected(ctx, dest, in.ExpectedHash, in.Overwrite); err != nil {
				return nil, err
			}
		} else if _, err := strictLstat(dr.root, dp); err == nil {
			return nil, failure("conflict", "Move destination exists", 409)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if !info.IsDir() {
			if _, err := s.snapshot(ctx, "external", dest, ""); err != nil {
				return nil, err
			}
		}
		if err := renameBetween(sr.root, sp, dr.root, dp); err == nil {
			if !info.IsDir() {
				if _, err := s.snapshot(ctx, actorID, dest, ""); err != nil {
					return nil, err
				}
			}
			return map[string]any{"path": dp, "moved": true}, nil
		} else if !errors.Is(err, syscall.EXDEV) {
			return nil, err
		}
		return s.moveAcrossDevices(ctx, actorID, sr, sp, dr, dp, in)
	}
	if info.IsDir() {
		if !in.Recursive {
			return nil, failure("invalid_input", "Directory copy requires recursive=true", 400)
		}
		return s.copyDirectoryAtomic(ctx, actorID, sr, sp, dr, dp, move)
	}
	var total int64
	count := 0
	var copyPath func(string, string) error
	copyPath = func(from, to string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.guardRootPath(sr, from, false); err != nil {
			return err
		}
		if err := s.guardRootPath(dr, to, true); err != nil {
			return err
		}
		count++
		if count > 100000 {
			return failure("too_large", "Copy has too many entries", 413)
		}
		info, err := strictLstat(sr.root, from)
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return failure("unsafe_path", "Copy cannot follow symbolic links", 403)
		}
		if info.IsDir() {
			if !in.Recursive {
				return failure("invalid_input", "Directory copy requires recursive=true", 400)
			}
			if err := strictMkdir(dr.root, to, info.Mode().Perm()); err != nil {
				return err
			}
			created, err := strictOpen(dr.root, to, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			err = inheritParentOwner(dr.root, to, created)
			if err == nil {
				err = created.Chmod(info.Mode().Perm())
			}
			created.Close()
			if err != nil {
				return err
			}
			dir, err := strictOpen(sr.root, from, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			defer dir.Close()
			entries, err := dir.ReadDir(-1)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if err := copyPath(filepath.Join(from, e.Name()), filepath.Join(to, e.Name())); err != nil {
					return err
				}
			}
			return nil
		}
		file, info, err := openRegular(sr.root, from, os.O_RDONLY)
		if err != nil {
			return err
		}
		defer file.Close()
		total += info.Size()
		if total > s.options.MaxTransferBytes {
			return failure("too_large", "Copy exceeds transfer budget", 413)
		}
		if err := s.checkExpected(ctx, Ref{in.DestinationRoot, to}, in.ExpectedHash, in.Overwrite); err != nil {
			return err
		}
		if _, err := s.snapshot(ctx, "external", Ref{in.DestinationRoot, to}, ""); err != nil {
			return err
		}
		_, _, err = s.writeAtomic(ctx, Ref{in.DestinationRoot, to}, file, s.options.MaxTransferBytes)
		if err != nil {
			return err
		}
		if _, err := s.snapshot(ctx, actorID, Ref{in.DestinationRoot, to}, ""); err != nil {
			return err
		}
		target, _, err := openRegular(dr.root, to, os.O_RDONLY)
		if err != nil {
			return err
		}
		defer target.Close()
		return target.Chmod(info.Mode().Perm())
	}
	if err := copyPath(sp, dp); err != nil {
		return nil, err
	}
	if move {
		if err := strictRemove(sr.root, sp, true); err != nil {
			return nil, err
		}
	}
	return map[string]any{"path": dp, "bytes": total, "entries": count, "moved": move}, nil
}

func inheritParentOwner(root *os.Root, path string, created *os.File) error {
	parent, err := strictLstat(root, filepath.Dir(path))
	if err != nil {
		return err
	}
	info, err := created.Stat()
	if err != nil {
		return err
	}
	parentIdentity, ok := parent.Sys().(*syscall.Stat_t)
	createdIdentity, valid := info.Sys().(*syscall.Stat_t)
	if !ok || !valid {
		return errors.New("filesystem ownership metadata unavailable")
	}
	if parentIdentity.Uid != createdIdentity.Uid || parentIdentity.Gid != createdIdentity.Gid {
		return created.Chown(int(parentIdentity.Uid), int(parentIdentity.Gid))
	}
	return nil
}

func (s *Service) Permissions(ctx context.Context, in Input) (any, error) {
	r, path, err := s.resolve(Ref{in.Root, in.Path}, true)
	if err != nil {
		return nil, err
	}
	if in.Mode != nil && *in.Mode > 0777 {
		return nil, failure("invalid_input", "Special permission bits are forbidden", 400)
	}
	unlock, err := s.lock(ctx, in.Root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	count := 0
	var apply func(string) error
	apply = func(path string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > 100000 {
			return failure("too_large", "Permission change exceeds traversal budget", 413)
		}
		f, err := strictOpen(r.root, path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return failure("unsafe_path", "Only directories and regular files have managed permissions", 403)
		}
		if in.UID != nil || in.GID != nil {
			uid, gid := -1, -1
			if in.UID != nil {
				uid = int(*in.UID)
			}
			if in.GID != nil {
				gid = int(*in.GID)
			}
			if err := f.Chown(uid, gid); err != nil {
				return err
			}
		}
		if in.Mode != nil {
			if err := f.Chmod(fs.FileMode(*in.Mode)); err != nil {
				return err
			}
		}
		if in.Recursive && info.IsDir() {
			entries, err := f.ReadDir(-1)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if err := apply(filepath.Join(path, e.Name())); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := apply(path); err != nil {
		return nil, err
	}
	return map[string]any{"updated": count}, nil
}

func (s *Service) Delete(ctx context.Context, actorID string, in Input) (any, error) {
	r, path, err := s.resolve(Ref{in.Root, in.Path}, true)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lock(ctx, in.Root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	info, err := strictLstat(r.root, path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() && !in.Recursive {
		return nil, failure("invalid_input", "Directory deletion requires recursive=true", 400)
	}
	if in.ExpectedHash != "" {
		if !info.IsDir() {
			if err := s.checkExpected(ctx, Ref{in.Root, path}, in.ExpectedHash, true); err != nil {
				return nil, err
			}
		} else {
			digest, _, err := s.exportTree(ctx, r, path, io.Discard)
			if err != nil {
				return nil, err
			}
			if digest != in.ExpectedHash {
				return nil, failure("conflict", "Directory changed during cross-root move; both copies were retained", 409)
			}
		}
	}
	settings, err := s.TrashSettings(ctx, in.Root)
	if err != nil {
		return nil, err
	}
	if !settings.Enabled {
		if !info.IsDir() {
			if _, err := s.snapshot(ctx, actorID, Ref{in.Root, path}, ""); err != nil {
				return nil, err
			}
		}
		if err := strictRemove(r.root, path, info.IsDir()); err != nil {
			return nil, err
		}
		return map[string]any{"path": path, "deleted": true, "permanent": true}, nil
	}
	record := Trash{ID: newID(), Root: in.Root, Path: path, ActorID: actorID, DeletedAt: time.Now().UTC(), Directory: info.IsDir(), State: "prepared"}
	if err := s.store.Save(ctx, "file_trash", record.ID, encode(record)); err != nil {
		return nil, err
	}
	trashName := filepath.Join("trash", record.ID)
	if err := renameBetween(r.root, path, s.state, trashName); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			err = s.recycleAcrossDevices(ctx, r, path, trashName)
		}
		if err != nil {
			if _, copyErr := strictLstat(s.state, trashName); copyErr == nil {
				record.State = "cleanup_required"
				s.store.Save(context.WithoutCancel(ctx), "file_trash", record.ID, encode(record))
				return record, &Error{Code: "source_cleanup_failed", Message: "The recycle copy is complete, but source cleanup failed; inspect both copies before retrying", Status: 409, Details: record}
			}
			s.store.Delete(context.WithoutCancel(ctx), "file_trash", record.ID)
			return nil, err
		}
	}
	record.State = "trashed"
	if err := s.store.Save(ctx, "file_trash", record.ID, encode(record)); err != nil {
		return nil, err
	}
	return record, nil
}
func (s *Service) trashRecord(ctx context.Context, id string) (Trash, error) {
	if !safeID.MatchString(id) {
		return Trash{}, failure("invalid_input", "Invalid trash ID", 400)
	}
	raw, err := s.store.Get(ctx, "file_trash", id)
	if err != nil {
		return Trash{}, err
	}
	var record Trash
	err = json.Unmarshal(raw, &record)
	return record, err
}
func (s *Service) TrashList(ctx context.Context, in Input) (any, error) {
	all, err := s.store.List(ctx, "file_trash")
	if err != nil {
		return nil, err
	}
	out := []Trash{}
	for _, raw := range all {
		var record Trash
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, err
		}
		if in.Root == record.Root {
			out = append(out, record)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeletedAt.After(out[j].DeletedAt) })
	return out, nil
}
func (s *Service) Restore(ctx context.Context, in Input, purge bool) (any, error) {
	record, err := s.trashRecord(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if in.Root != record.Root {
		return nil, failure("unsafe_path", "Trash ID does not belong to this root", 403)
	}
	r, path, err := s.resolve(Ref{record.Root, record.Path}, true)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lock(ctx, record.Root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	name := filepath.Join("trash", record.ID)
	if purge {
		err = s.state.RemoveAll(name)
	} else {
		if _, err := strictLstat(r.root, path); err == nil {
			return nil, failure("conflict", "Restore destination exists", 409)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		err = renameBetween(s.state, name, r.root, path)
		if errors.Is(err, syscall.EXDEV) {
			err = s.restoreAcrossDevices(ctx, name, r, path)
		}
	}
	if err != nil {
		return nil, err
	}
	if err := s.store.Delete(ctx, "file_trash", record.ID); err != nil {
		return nil, err
	}
	return map[string]any{"id": record.ID, "restored": !purge, "purged": purge}, nil
}

// OpenDownload returns a regular-file stream and metadata for an authenticated
// HTTP adapter. The adapter must authorize root before calling and close it.
func (s *Service) OpenDownload(ctx context.Context, ref Ref) (*os.File, Entry, error) {
	r, path, err := s.resolve(ref, false)
	if err != nil {
		return nil, Entry{}, err
	}
	f, info, err := openRegular(r.root, path, os.O_RDONLY)
	if err != nil {
		return nil, Entry{}, err
	}
	if err := ctx.Err(); err != nil {
		f.Close()
		return nil, Entry{}, err
	}
	return f, fileEntry(filepath.Base(path), path, info), nil
}

func renameBetween(source *os.Root, sourceName string, dest *os.Root, destName string) error {
	return renameBetweenFlags(source, sourceName, dest, destName, 0)
}
func renameBetweenNoReplace(source *os.Root, sourceName string, dest *os.Root, destName string) error {
	return renameBetweenFlags(source, sourceName, dest, destName, 1)
}
func renameBetweenFlags(source *os.Root, sourceName string, dest *os.Root, destName string, flags uint) error {
	from, err := strictSubroot(source, filepath.Dir(sourceName))
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := strictSubroot(dest, filepath.Dir(destName))
	if err != nil {
		return err
	}
	defer to.Close()
	fromFD, err := from.Open(".")
	if err != nil {
		return err
	}
	defer fromFD.Close()
	toFD, err := to.Open(".")
	if err != nil {
		return err
	}
	defer toFD.Close()
	return renameDescriptors(int(fromFD.Fd()), filepath.Base(sourceName), int(toFD.Fd()), filepath.Base(destName), flags)
}
