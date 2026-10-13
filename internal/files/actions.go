package files

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

func (s *Service) Actions() []Action {
	stringField := func() map[string]any { return map[string]any{"type": "string"} }
	properties := map[string]any{}
	for _, field := range []string{"root", "path", "destination_root", "destination_path", "expected_hash", "encoding", "content", "query", "sort", "order", "format", "id", "sha256", "url", "operation"} {
		properties[field] = stringField()
	}
	for _, field := range []string{"offset", "limit", "mode", "uid", "gid", "size", "lines"} {
		properties[field] = map[string]any{"type": "integer", "minimum": 0}
	}
	for _, field := range []string{"overwrite", "recursive", "enabled"} {
		properties[field] = map[string]any{"type": "boolean"}
	}
	properties["items"] = map[string]any{"type": "array", "items": stringField(), "maxItems": 1000}
	properties["expected_hashes"] = map[string]any{"type": "object", "maxProperties": 500, "additionalProperties": map[string]any{"type": "string", "minLength": 64, "maxLength": 64}}
	properties["operation"] = map[string]any{"type": "string", "enum": []string{"copy", "move", "delete", "chmod"}}
	rows := []struct {
		name, scope      string
		read, task       bool
		fields, required []string
	}{
		{"fs_roots", "files:read", true, false, nil, nil},
		{"fs_formats", "files:read", true, false, nil, nil},
		{"fs_list", "files:read", true, false, []string{"root", "path", "limit", "offset", "sort", "order", "query"}, []string{"root"}},
		{"fs_read", "files:read", true, false, []string{"root", "path", "encoding", "limit"}, []string{"root", "path"}},
		{"fs_tail", "files:read", true, false, []string{"root", "path", "encoding", "lines"}, []string{"root", "path"}},
		{"fs_history", "files:read", true, false, []string{"root", "path", "id"}, []string{"root", "path"}},
		{"fs_search", "files:read", true, true, []string{"root", "path", "query", "offset", "limit"}, []string{"root", "query"}},
		{"fs_size", "files:read", true, true, []string{"root", "path"}, []string{"root", "path"}},
		{"fs_edit", "files:write", false, false, []string{"root", "path", "content", "encoding", "expected_hash", "overwrite"}, []string{"root", "path", "content"}},
		{"fs_create", "files:write", false, false, []string{"root", "path", "content", "encoding"}, []string{"root", "path"}},
		{"fs_mkdir", "files:write", false, false, []string{"root", "path"}, []string{"root", "path"}},
		{"fs_copy", "files:write", false, true, []string{"root", "path", "destination_root", "destination_path", "recursive", "overwrite", "expected_hash"}, []string{"root", "path", "destination_root", "destination_path"}},
		{"fs_move", "files:write", false, true, []string{"root", "path", "destination_root", "destination_path", "recursive", "overwrite", "expected_hash"}, []string{"root", "path", "destination_root", "destination_path"}},
		{"fs_chmod", "files:permissions", false, true, []string{"root", "path", "mode", "uid", "gid", "recursive"}, []string{"root", "path"}},
		{"fs_preflight", "files:read", true, true, []string{"root", "path", "items", "operation", "destination_root", "destination_path", "recursive"}, []string{"root", "items", "operation"}},
		{"fs_batch", "files:write", false, true, []string{"root", "path", "items", "operation", "destination_root", "destination_path", "expected_hashes", "recursive", "mode", "uid", "gid"}, []string{"root", "items", "operation"}},
		{"fs_delete", "files:write", false, true, []string{"root", "path", "recursive", "expected_hash"}, []string{"root", "path"}},
		{"fs_trash_list", "files:read", true, false, []string{"root"}, []string{"root"}},
		{"fs_trash_settings", "files:read", true, false, []string{"root"}, []string{"root"}},
		{"fs_trash_configure", "files:write", false, false, []string{"root", "enabled"}, []string{"root", "enabled"}},
		{"fs_trash_clear", "files:write", false, true, []string{"root"}, []string{"root"}},
		{"fs_restore", "files:write", false, true, []string{"root", "id"}, []string{"root", "id"}},
		{"fs_purge", "files:write", false, true, []string{"root", "id"}, []string{"root", "id"}},
		{"fs_archive", "files:write", false, true, []string{"root", "path", "items", "format", "destination_root", "destination_path", "overwrite", "expected_hash"}, []string{"root", "path", "format", "destination_root", "destination_path"}},
		{"fs_extract", "files:write", false, true, []string{"root", "path", "format", "destination_root", "destination_path"}, []string{"root", "path", "destination_root", "destination_path"}},
		{"fs_upload_begin", "files:write", false, false, []string{"root", "path", "size", "sha256", "expected_hash", "overwrite"}, []string{"root", "path", "size"}},
		{"fs_upload_status", "files:write", false, false, []string{"root", "id"}, []string{"root", "id"}},
		{"fs_upload_commit", "files:write", false, true, []string{"root", "id"}, []string{"root", "id"}},
		{"fs_upload_cancel", "files:write", false, false, []string{"root", "id"}, []string{"root", "id"}},
		{"fs_remote_download", "files:write", false, true, []string{"root", "path", "url", "sha256", "overwrite", "expected_hash"}, []string{"root", "path", "url"}},
	}
	out := make([]Action, 0, len(rows))
	for _, row := range rows {
		props := map[string]any{}
		for _, field := range row.fields {
			props[field] = properties[field]
		}
		schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(row.required) > 0 {
			schema["required"] = row.required
		}
		out = append(out, Action{Name: row.name, Scope: row.scope, ReadOnly: row.read, Task: row.task, InputSchema: schema})
	}
	return out
}
func (s *Service) Resources(action string, raw json.RawMessage) ([]string, error) {
	in, err := parse(raw)
	if err != nil {
		return nil, err
	}
	roots := map[string]bool{}
	if in.Root != "" {
		if _, _, err := s.resolve(Ref{in.Root, in.Path}, false); err != nil {
			return nil, err
		}
		roots[in.Root] = true
	}
	if in.DestinationRoot != "" {
		if _, _, err := s.resolve(Ref{in.DestinationRoot, in.DestinationPath}, false); err != nil {
			return nil, err
		}
		roots[in.DestinationRoot] = true
	}
	out := []string{}
	for root := range roots {
		out = append(out, "fs:"+root)
	}
	sort.Strings(out)
	return out, nil
}
func (s *Service) Execute(ctx context.Context, actorID, action string, raw json.RawMessage) (any, error) {
	if len(raw) > 8<<20 {
		return nil, failure("too_large", "Action payload exceeds budget", 413)
	}
	in, err := parse(raw)
	if err != nil {
		return nil, err
	}
	switch action {
	case "fs_roots":
		return s.Roots(), nil
	case "fs_formats":
		return s.ArchiveFormats(), nil
	case "fs_list":
		return s.List(ctx, in)
	case "fs_stat":
		root, path, err := s.resolve(Ref{in.Root, in.Path}, false)
		if err != nil {
			return nil, err
		}
		info, err := strictLstat(root.root, path)
		if err != nil {
			return nil, err
		}
		return fileEntry(filepath.Base(path), path, info), nil
	case "fs_read":
		return s.ReadText(ctx, in)
	case "fs_tail":
		return s.Tail(ctx, in)
	case "fs_history":
		return s.History(ctx, in)
	case "fs_edit", "fs_create":
		return s.Edit(ctx, actorID, in)
	case "fs_mkdir":
		return s.Mkdir(ctx, in)
	case "fs_search", "fs_size":
		return s.Walk(ctx, in, action == "fs_size")
	case "fs_copy", "fs_move":
		return s.CopyAs(ctx, actorID, in, action == "fs_move")
	case "fs_chmod":
		return s.Permissions(ctx, in)
	case "fs_preflight":
		return s.Preflight(ctx, in)
	case "fs_batch":
		return s.Batch(ctx, actorID, in)
	case "fs_delete":
		return s.Delete(ctx, actorID, in)
	case "fs_trash_list":
		return s.TrashList(ctx, in)
	case "fs_trash_settings":
		return s.TrashSettings(ctx, in.Root)
	case "fs_trash_configure":
		return s.ConfigureTrash(ctx, actorID, in)
	case "fs_trash_clear":
		return s.ClearTrash(ctx, in)
	case "fs_restore", "fs_purge":
		return s.Restore(ctx, in, action == "fs_purge")
	case "fs_archive":
		return s.Archive(ctx, in)
	case "fs_extract":
		return s.Extract(ctx, in)
	case "fs_upload_begin":
		return s.UploadBegin(ctx, actorID, in)
	case "fs_upload_status":
		session, err := s.UploadStatus(ctx, actorID, in.ID)
		if err != nil {
			return nil, err
		}
		if in.Root != session.Root {
			return nil, failure("forbidden", "Upload root mismatch", 403)
		}
		return session, nil
	case "fs_upload_commit":
		return s.UploadCommit(ctx, actorID, in)
	case "fs_upload_cancel":
		session, err := s.UploadStatus(ctx, actorID, in.ID)
		if err != nil {
			return nil, err
		}
		if in.Root != session.Root {
			return nil, failure("forbidden", "Upload root mismatch", 403)
		}
		return s.UploadCancel(ctx, actorID, in)
	case "fs_remote_download":
		return s.RemoteDownload(ctx, actorID, in)
	default:
		return nil, failure("unknown_action", "Unknown filesystem action", 404)
	}
}

// Executor is registered by the CLI with core's durable TaskSubmitter.
type Executor struct{ Service *Service }

func (e *Executor) Validate(request agent.Request) error {
	in, err := parse(request.Input)
	if err != nil {
		return err
	}
	for _, path := range []string{in.Path, in.DestinationPath} {
		if _, err := clean(path); err != nil {
			return err
		}
	}
	if request.Operation == "fs_stat" || request.Operation == "fs_download_stream" || request.Operation == "fs_upload_chunk" || request.Operation == "fs_edit_stream" || request.Operation == "fs_export_tree_stream" || request.Operation == "fs_import_tree_stream" || request.Operation == "fs_archive_stream" || request.Operation == "fs_import_file_stream" || request.Operation == "fs_extract_stream" {
		return nil
	}
	for _, a := range e.Service.Actions() {
		if a.Name == request.Operation {
			return nil
		}
	}
	return agent.ErrUnauthorized
}
func (e *Executor) Run(ctx context.Context, request agent.Request) (agent.Result, error) {
	result := agent.Result{Operation: request.Operation, ExitCode: -1, StartedAt: time.Now().UTC()}
	in, parseErr := parse(request.Input)
	if parseErr != nil {
		return result, parseErr
	}
	ctx, release, leaseErr := e.Service.beginRequest(ctx, in.Root, in.DestinationRoot)
	if leaseErr != nil {
		return result, leaseErr
	}
	defer release()
	data, err := e.Service.Execute(ctx, request.ActorID, request.Operation, request.Input)
	result.FinishedAt = time.Now().UTC()
	result.Data = encode(data)
	if err == nil {
		result.ExitCode = 0
	}
	result.Canceled = errors.Is(err, context.Canceled)
	result.TimedOut = errors.Is(err, context.DeadlineExceeded)
	return result, moduleError(err)
}

var _ = fs.ErrExist
