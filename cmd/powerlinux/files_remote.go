package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/files"
)

type fileRootInfo struct {
	ID                           string
	HostAccess, ReadOnly, Remote bool
}

func (a *fileAdapter) remoteRoots(ctx context.Context, actor core.Actor) ([]map[string]any, error) {
	if a.remote == nil {
		return nil, nil
	}
	roots, err := a.remote.Roots(ctx, actor.ID)
	var typed *agent.RemoteError
	if errors.As(err, &typed) && ((typed.Code == "module_unavailable" && typed.Status == 503) || (typed.Code == "unauthorized" && errors.Is(err, agent.ErrUnauthorized))) {
		return nil, nil
	}
	return roots, fileError(err)
}

func rootInformation(row map[string]any, remote bool) fileRootInfo {
	id, _ := row["id"].(string)
	host, _ := row["host_access"].(bool)
	readOnly, _ := row["read_only"].(bool)
	return fileRootInfo{ID: id, HostAccess: host, ReadOnly: readOnly, Remote: remote}
}

func (a *fileAdapter) rootInfo(ctx context.Context, actor core.Actor, id string) (fileRootInfo, error) {
	// This function discovers metadata only; callers must authorize the root ID
	// before it. No absolute path is returned or opened by the API process.
	for _, root := range a.files.Roots() {
		info := rootInformation(root, false)
		if info.ID == id {
			return info, nil
		}
	}
	roots, err := a.remoteRoots(ctx, actor)
	if err != nil {
		return fileRootInfo{}, err
	}
	for _, root := range roots {
		info := rootInformation(root, true)
		if info.ID == id {
			return info, nil
		}
	}
	return fileRootInfo{}, &core.Error{Code: "unsafe_path", Message: "Unknown filesystem root", Status: 403}
}

func (a *fileAdapter) authorizeRoot(ctx context.Context, actor core.Actor, scope, root string) (fileRootInfo, error) {
	if !a.authorize(actor, scope, root) {
		return fileRootInfo{}, fileDenied()
	}
	info, err := a.rootInfo(ctx, actor, root)
	if err != nil {
		return info, err
	}
	if info.HostAccess && !a.authorize(actor, "files:host", root) {
		return info, fileDenied()
	}
	if info.ReadOnly && scope != "files:read" {
		return info, &core.Error{Code: "read_only", Message: "Filesystem root is read-only", Status: 403}
	}
	return info, nil
}

func (a *fileAdapter) listRoots(ctx context.Context, actor core.Actor) ([]map[string]any, error) {
	rows := a.files.Roots()
	remote, err := a.remoteRoots(ctx, actor)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []map[string]any{}
	for _, row := range append(rows, remote...) {
		info := rootInformation(row, false)
		if info.ID == "" || seen[info.ID] {
			return nil, &core.Error{Code: "filesystem_configuration_conflict", Message: "Filesystem root IDs must be unique across local and privileged roots", Status: 500}
		}
		seen[info.ID] = true
		if a.authorize(actor, "files:read", info.ID) && (!info.HostAccess || a.authorize(actor, "files:host", info.ID)) {
			out = append(out, map[string]any{"id": info.ID, "label": row["label"], "read_only": info.ReadOnly, "host_access": info.HostAccess})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
	return out, nil
}

func (a *fileAdapter) executeFile(ctx context.Context, actor core.Actor, name string, raw json.RawMessage) (any, error) {
	var in files.Input
	if json.Unmarshal(raw, &in) != nil {
		return nil, fileInvalid()
	}
	if in.Root == "" {
		return a.files.Execute(ctx, actor.ID, name, raw)
	}
	root, err := a.rootInfo(ctx, actor, in.Root)
	if err != nil {
		return nil, err
	}
	if (name == "fs_copy" || name == "fs_move" || name == "fs_preflight" || name == "fs_batch" || name == "fs_archive" || name == "fs_extract") && in.DestinationRoot != "" {
		destination, err := a.rootInfo(ctx, actor, in.DestinationRoot)
		if err != nil {
			return nil, err
		}
		if root.Remote != destination.Remote {
			if name == "fs_archive" || name == "fs_extract" {
				return a.artifactBetweenServices(ctx, actor, in, name == "fs_extract")
			}
			if name == "fs_preflight" {
				return a.preflightBetweenServices(ctx, actor, in)
			}
			if name == "fs_batch" {
				return a.batchBetweenServices(ctx, actor, in)
			}
			return a.copyBetweenServices(ctx, actor, in, name == "fs_move")
		}
	}
	if root.Remote {
		return a.remote.Execute(ctx, actor.ID, name, raw)
	}
	return a.files.Execute(ctx, actor.ID, name, raw)
}

func (a *fileAdapter) uploadStatus(ctx context.Context, actor core.Actor, root, id string) (files.Upload, error) {
	info, err := a.authorizeRoot(ctx, actor, "files:write", root)
	if err != nil {
		return files.Upload{}, err
	}
	if info.Remote {
		return a.remote.UploadStatus(ctx, actor.ID, root, id)
	}
	upload, err := a.files.UploadStatus(ctx, actor.ID, id)
	if err == nil && upload.Root != root {
		return files.Upload{}, fileDenied()
	}
	return upload, err
}

func (a *fileAdapter) uploadChunk(ctx context.Context, actor core.Actor, in transferInput, reader io.ReadCloser) (files.Upload, error) {
	root, err := a.authorizeRoot(ctx, actor, "files:write", in.Root)
	if err != nil {
		return files.Upload{}, err
	}
	if root.Remote {
		return a.remote.UploadChunk(ctx, actor.ID, in.Root, in.ID, in.Offset, reader, in.SHA256)
	}
	return a.files.UploadChunk(ctx, actor.ID, in.ID, in.Offset, reader, in.SHA256)
}

func (a *fileAdapter) statFile(ctx context.Context, actor core.Actor, ref files.Ref) (files.Entry, error) {
	info, err := a.authorizeRoot(ctx, actor, "files:read", ref.Root)
	if err != nil {
		return files.Entry{}, err
	}
	if info.Remote {
		return a.remote.Stat(ctx, actor.ID, ref)
	}
	value, err := a.files.Execute(ctx, actor.ID, "fs_stat", mustJSON(files.Input{Root: ref.Root, Path: ref.Path}))
	if err != nil {
		return files.Entry{}, err
	}
	return value.(files.Entry), nil
}

func (a *fileAdapter) openFileRange(ctx context.Context, actor core.Actor, ref files.Ref, offset, length int64) (io.ReadCloser, files.Entry, error) {
	info, err := a.authorizeRoot(ctx, actor, "files:read", ref.Root)
	if err != nil {
		return nil, files.Entry{}, err
	}
	if info.Remote {
		return a.remote.OpenDownload(ctx, actor.ID, ref, offset, length)
	}
	file, entry, err := a.files.OpenDownload(ctx, ref)
	if err != nil {
		return nil, files.Entry{}, err
	}
	if offset < 0 || length < 0 || offset > entry.Size || (length > 0 && length > entry.Size-offset) {
		file.Close()
		return nil, files.Entry{}, fileInvalid()
	}
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		file.Close()
		return nil, files.Entry{}, err
	}
	if length == 0 {
		length = entry.Size - offset
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(file, length), file}, entry, nil
}

type remoteSeekFile struct {
	adapter  *fileAdapter
	ctx      context.Context
	actor    core.Actor
	ref      files.Ref
	entry    files.Entry
	position int64
	reader   io.ReadCloser
}

func (f *remoteSeekFile) Close() error {
	if f.reader != nil {
		return f.reader.Close()
	}
	return nil
}
func (f *remoteSeekFile) Seek(offset int64, whence int) (int64, error) {
	position := offset
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		position += f.position
	case io.SeekEnd:
		position += f.entry.Size
	default:
		return 0, fileInvalid()
	}
	if position < 0 || position > f.entry.Size {
		return 0, fileInvalid()
	}
	if f.reader != nil {
		f.reader.Close()
		f.reader = nil
	}
	f.position = position
	return position, nil
}
func (f *remoteSeekFile) Read(buffer []byte) (int, error) {
	if f.position >= f.entry.Size {
		return 0, io.EOF
	}
	if f.reader == nil {
		reader, entry, err := f.adapter.openFileRange(f.ctx, f.actor, f.ref, f.position, f.entry.Size-f.position)
		if err != nil {
			return 0, err
		}
		if entry.Size != f.entry.Size || !entry.ModifiedAt.Equal(f.entry.ModifiedAt) {
			reader.Close()
			return 0, &core.Error{Code: "conflict", Message: "Remote file changed during download", Status: 409}
		}
		f.reader = reader
	}
	n, err := f.reader.Read(buffer)
	f.position += int64(n)
	return n, err
}

func (a *fileAdapter) openSeekFile(ctx context.Context, actor core.Actor, ref files.Ref) (io.ReadSeekCloser, files.Entry, error) {
	root, err := a.authorizeRoot(ctx, actor, "files:read", ref.Root)
	if err != nil {
		return nil, files.Entry{}, err
	}
	if root.Remote {
		entry, err := a.remote.Stat(ctx, actor.ID, ref)
		if err != nil {
			return nil, entry, err
		}
		if entry.Type != "file" {
			return nil, entry, &core.Error{Code: "unsafe_path", Message: "Download requires a regular file", Status: 403}
		}
		return &remoteSeekFile{adapter: a, ctx: ctx, actor: actor, ref: ref, entry: entry}, entry, nil
	}
	file, entry, err := a.files.OpenDownload(ctx, ref)
	return file, entry, err
}

var _ io.ReadSeekCloser = (*remoteSeekFile)(nil)
