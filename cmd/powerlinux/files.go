package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/files"
	"github.com/Aoody743/PowerLinux/internal/httpapi"
)

// fileAdapter is trusted composition code. It never accepts a root's absolute
// path from HTTP/MCP, and resolves both source and destination permissions
// before asking the filesystem to inspect either path.
type fileAdapter struct {
	service *core.Service
	files   *files.Service
	streams chan struct{}
	remote  *files.Remote
}

// The filesystem store contract uses fs.ErrNotExist for an absent record.
// Core keeps typed HTTP errors; translate only its genuine missing-record
// error so optional settings can default without hiding database failures.
type fileMetadataStore struct{ core.MetadataStore }

func (s fileMetadataStore) Get(ctx context.Context, kind, id string) (json.RawMessage, error) {
	value, err := s.MetadataStore.Get(ctx, kind, id)
	var typed *core.Error
	if errors.As(err, &typed) && typed.Code == "not_found" && typed.Status == http.StatusNotFound {
		return nil, fs.ErrNotExist
	}
	return value, err
}

func registerFiles(service *core.Service, dataDir string, trustedRemote ...*files.Remote) (*fileAdapter, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	managed, err := files.New(files.Options{
		Roots:    []files.RootSpec{{ID: "workspace", Label: "Workspace", Path: filepath.Join(abs, "workspace"), DefaultFileMode: 0600, DefaultDirMode: 0700}},
		StateDir: filepath.Join(abs, "file-state"), Store: fileMetadataStore{service.Runtime("files").State},
	})
	if err != nil {
		return nil, err
	}
	a := &fileAdapter{service: service, files: managed, streams: make(chan struct{}, 16)}
	if len(trustedRemote) > 1 {
		managed.Close()
		return nil, errors.New("only one trusted filesystem Agent is supported")
	}
	if len(trustedRemote) == 1 {
		a.remote = trustedRemote[0]
	}
	if err = service.RegisterScopes("files:host"); err != nil {
		managed.Close()
		return nil, err
	}
	if err = service.RegisterProvider(a); err != nil {
		managed.Close()
		return nil, err
	}
	operations := []string{}
	for _, action := range managed.Actions() {
		if action.Task {
			operations = append(operations, action.Name)
		}
	}
	operations = append(operations, "fs_legacy_write", "fs_legacy_hash", "fs_legacy_backup")
	if err = service.RegisterExecutor(operations, a); err != nil {
		managed.Close()
		return nil, err
	}
	if err = service.SetLegacyFileDelegate(a); err != nil {
		managed.Close()
		return nil, err
	}
	return a, nil
}

func fileError(err error) error {
	if err == nil {
		return nil
	}
	var known *core.Error
	if errors.As(err, &known) {
		return err
	}
	var typed *files.Error
	if errors.As(err, &typed) {
		return &core.Error{Code: typed.Code, Message: typed.Message, Status: typed.Status, Details: typed.Details}
	}
	var remote *agent.RemoteError
	if errors.As(err, &remote) {
		return &core.Error{Code: remote.Code, Message: remote.Message, Status: remote.Status}
	}
	var budget *http.MaxBytesError
	if errors.As(err, &budget) {
		return &core.Error{Code: "payload_too_large", Message: "Upload chunk exceeds four MiB", Status: 413}
	}
	switch {
	case errors.Is(err, syscall.ELOOP):
		return &core.Error{Code: "unsafe_path", Message: "Symbolic-link file targets are not permitted", Status: 403}
	case errors.Is(err, fs.ErrNotExist):
		return &core.Error{Code: "not_found", Message: "Managed file or transfer was not found", Status: 404}
	case errors.Is(err, fs.ErrExist):
		return &core.Error{Code: "conflict", Message: "Destination already exists", Status: 409}
	case errors.Is(err, fs.ErrPermission):
		return &core.Error{Code: "forbidden", Message: "Managed file access is not permitted", Status: 403}
	case errors.Is(err, context.Canceled):
		return &core.Error{Code: "canceled", Message: "File operation was canceled", Status: 409}
	case errors.Is(err, context.DeadlineExceeded):
		return &core.Error{Code: "timeout", Message: "File operation timed out", Status: 504}
	default:
		return &core.Error{Code: "file_operation_failed", Message: "File operation could not be completed", Status: 500}
	}
}

func fileDenied() error {
	return &core.Error{Code: "forbidden", Message: "The required filesystem root or operation is not authorized", Status: 403}
}
func fileInvalid() error {
	return &core.Error{Code: "invalid_input", Message: "Invalid file transfer parameters", Status: 400}
}

func (a *fileAdapter) authorize(actor core.Actor, scope, root string) bool {
	return root != "" && a.service.Authorized(actor, scope, "fs:"+root)
}

func (a *fileAdapter) resources(ctx context.Context, actor core.Actor, action files.Action, raw json.RawMessage) ([]string, error) {
	var in files.Input
	if json.Unmarshal(raw, &in) != nil {
		return nil, fileInvalid()
	}
	if action.Name == "fs_roots" || action.Name == "fs_formats" {
		return nil, nil
	}
	sourceScope := action.Scope
	if action.Name == "fs_copy" || action.Name == "fs_archive" || action.Name == "fs_extract" || ((action.Name == "fs_batch" || action.Name == "fs_preflight") && in.Operation == "copy") {
		sourceScope = "files:read"
	}
	// Check every requested permission before even discovering Agent roots.
	if !a.authorize(actor, sourceScope, in.Root) {
		return nil, fileDenied()
	}
	if action.Name == "fs_batch" && in.Operation == "chmod" && !a.authorize(actor, "files:permissions", in.Root) {
		return nil, fileDenied()
	}
	if in.DestinationRoot != "" {
		if !a.authorize(actor, "files:write", in.DestinationRoot) || !a.authorize(actor, "files:read", in.Root) {
			return nil, fileDenied()
		}
	}
	source, err := a.authorizeRoot(ctx, actor, sourceScope, in.Root)
	if err != nil {
		return nil, err
	}
	if in.DestinationRoot != "" {
		if _, err = a.authorizeRoot(ctx, actor, "files:write", in.DestinationRoot); err != nil {
			return nil, err
		}
	}
	validatePath := func(root string, path string) error {
		info := source
		if root != in.Root {
			var err error
			info, err = a.rootInfo(ctx, actor, root)
			if err != nil {
				return err
			}
		}
		value := mustJSON(files.Input{Root: root, Path: path})
		if info.Remote {
			return fileError(a.remote.Client.Validate(agent.Request{ActorID: actor.ID, Operation: "fs_stat", Input: value}))
		}
		_, err := a.files.Resources("fs_read", value)
		return fileError(err)
	}
	if err = validatePath(in.Root, in.Path); err != nil {
		return nil, err
	}
	resources := []string{"fs:" + in.Root}
	if in.DestinationRoot != "" {
		if err = validatePath(in.DestinationRoot, in.DestinationPath); err != nil {
			return nil, err
		}
		if in.DestinationRoot != in.Root {
			resources = append(resources, "fs:"+in.DestinationRoot)
		}
	}
	sort.Strings(resources)
	return resources, nil
}

func (a *fileAdapter) Bindings() []core.Binding {
	bindings := []core.Binding{}
	for _, action := range a.files.Actions() {
		bindings = append(bindings, core.Binding{
			Action: core.Action{Name: action.Name, Description: "Managed filesystem: " + strings.ReplaceAll(strings.TrimPrefix(action.Name, "fs_"), "_", " "), Scope: action.Scope, ReadOnly: action.ReadOnly, Task: action.Task, InputSchema: action.InputSchema},
			Resources: func(ctx context.Context, actor core.Actor, raw json.RawMessage) ([]string, error) {
				return a.resources(ctx, actor, action, raw)
			},
			Execute: func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
				resources, err := a.resources(ctx, actor, action, raw)
				if err != nil {
					return core.ActionResult{}, err
				}
				if action.Name == "fs_roots" {
					roots, err := a.listRoots(ctx, actor)
					return core.ActionResult{Data: roots}, err
				}
				if action.Task {
					job, err := a.service.Runtime("files").Tasks.Submit(ctx, actor, action.Name, raw, resources)
					id, _ := job["id"].(string)
					return core.ActionResult{Data: job, Resources: resources, TaskID: id}, err
				}
				data, err := a.executeFile(ctx, actor, action.Name, raw)
				return core.ActionResult{Data: data, Resources: resources}, fileError(err)
			},
		})
	}
	bindings = append(bindings, a.transferBindings()...)
	return append(bindings, a.textBinding())
}

// Queued work rechecks every permission, including the extra source-read scope
// on a copy/extract/archive, against the persisted current identity.
func (a *fileAdapter) Validate(request agent.Request) error {
	if strings.HasPrefix(request.Operation, "fs_legacy_") {
		var input files.Input
		if json.Unmarshal(request.Input, &input) != nil {
			return fileInvalid()
		}
		return nil
	}
	var input files.Input
	if json.Unmarshal(request.Input, &input) != nil {
		return fileInvalid()
	}
	for _, action := range a.files.Actions() {
		if action.Name == request.Operation {
			return nil
		}
	}
	return agent.ErrUnauthorized
}
func (a *fileAdapter) Run(ctx context.Context, request agent.Request) (agent.Result, error) {
	actor, err := a.service.RefreshActor(ctx, core.Actor{ID: request.ActorID})
	if err != nil {
		return agent.Result{}, err
	}
	if strings.HasPrefix(request.Operation, "fs_legacy_") {
		return a.runLegacy(ctx, actor, request)
	}
	for _, action := range a.files.Actions() {
		if action.Name == request.Operation {
			if _, err = a.resources(ctx, actor, action, request.Input); err != nil {
				return agent.Result{}, err
			}
			result := agent.Result{Operation: request.Operation, ExitCode: -1, StartedAt: time.Now().UTC()}
			data, err := a.executeFile(ctx, actor, request.Operation, request.Input)
			result.Data = mustJSON(data)
			result.FinishedAt = time.Now().UTC()
			result.Canceled = errors.Is(err, context.Canceled)
			result.TimedOut = errors.Is(err, context.DeadlineExceeded)
			if err == nil {
				result.ExitCode = 0
			}
			return result, fileError(err)
		}
	}
	return agent.Result{}, fileDenied()
}

func (a *fileAdapter) Resources(ctx context.Context, actor core.Actor, name string, raw json.RawMessage) ([]string, error) {
	var action files.Action
	switch name {
	case "files_write":
		action = files.Action{Name: "fs_edit", Scope: "files:write"}
	case "files_hash":
		action = files.Action{Name: "fs_read", Scope: "files:read"}
	case "files_backup":
		action = files.Action{Name: "fs_copy", Scope: "files:write"}
	default:
		return nil, fileDenied()
	}
	resources, err := a.resources(ctx, actor, action, raw)
	for i, resource := range resources {
		resources[i] = "files:" + strings.TrimPrefix(resource, "fs:")
	}
	return resources, err
}
func (a *fileAdapter) Execute(ctx context.Context, actor core.Actor, name string, raw json.RawMessage) (core.ActionResult, error) {
	resources, err := a.Resources(ctx, actor, name, raw)
	if err != nil {
		return core.ActionResult{}, err
	}
	operation := "fs_legacy_" + strings.TrimPrefix(name, "files_")
	job, err := a.service.Runtime("files").Tasks.Submit(ctx, actor, operation, raw, resources)
	id, _ := job["id"].(string)
	return core.ActionResult{Data: job, Resources: resources, TaskID: id}, err
}
func (a *fileAdapter) runLegacy(ctx context.Context, actor core.Actor, request agent.Request) (result agent.Result, err error) {
	result = agent.Result{Operation: request.Operation, ExitCode: -1, StartedAt: time.Now().UTC()}
	defer func() {
		result.FinishedAt = time.Now().UTC()
		result.Canceled = errors.Is(err, context.Canceled)
		result.TimedOut = errors.Is(err, context.DeadlineExceeded)
		if err == nil {
			result.ExitCode = 0
		}
	}()
	name := "files_" + strings.TrimPrefix(request.Operation, "fs_legacy_")
	if _, err = a.Resources(ctx, actor, name, request.Input); err != nil {
		return result, err
	}
	var input files.Input
	if json.Unmarshal(request.Input, &input) != nil {
		return result, fileInvalid()
	}
	var data any
	switch name {
	case "files_write":
		input.Overwrite = true
		data, err = a.executeFile(ctx, actor, "fs_edit", mustJSON(input))
	case "files_backup":
		input.Overwrite = true
		data, err = a.executeFile(ctx, actor, "fs_copy", mustJSON(input))
	case "files_hash":
		var file io.ReadSeekCloser
		file, _, err = a.openSeekFile(ctx, actor, files.Ref{Root: input.Root, Path: input.Path})
		if err == nil {
			defer file.Close()
			hash := sha256.New()
			_, err = io.CopyBuffer(hash, &authorizedFile{File: file, ctx: ctx, service: a.service, actor: actor, root: input.Root, adapter: a}, make([]byte, 32<<10))
			if err == nil {
				digest := hex.EncodeToString(hash.Sum(nil))
				result.Stdout = digest + "\n"
				data = map[string]any{"sha256": digest}
			}
		}
	default:
		err = fileDenied()
	}
	result.Data = mustJSON(data)
	return result, fileError(err)
}

type transferContextKey struct{}
type fileStream struct {
	writer   http.ResponseWriter
	request  *http.Request
	reader   io.Reader
	started  bool
	validate func() (core.Actor, error)
	text     []byte
	isText   bool
}
type transferInput struct {
	Root   string `json:"root"`
	Path   string `json:"path,omitempty"`
	ID     string `json:"id,omitempty"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Base64 string `json:"data_base64,omitempty"`
}

func (a *fileAdapter) transferBindings() []core.Binding {
	str := map[string]any{"type": "string", "maxLength": 2048}
	object := func(props map[string]any, required []string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}
	return []core.Binding{
		{Action: core.Action{Name: "fs_upload_chunk", Description: "Append a verified chunk at a resumable upload offset; MCP accepts up to 32 KiB base64, HTTP streams up to 4 MiB", Scope: "files:write", Audit: true, InputSchema: object(map[string]any{"root": str, "id": str, "offset": map[string]any{"type": "integer", "minimum": 0}, "sha256": str, "data_base64": map[string]any{"type": "string", "maxLength": 43692}}, []string{"root", "id", "offset"})},
			Resources: func(ctx context.Context, actor core.Actor, raw json.RawMessage) ([]string, error) {
				var in transferInput
				if json.Unmarshal(raw, &in) != nil {
					return nil, fileInvalid()
				}
				if !a.authorize(actor, "files:write", in.Root) {
					return nil, fileDenied()
				}
				session, err := a.uploadStatus(ctx, actor, in.Root, in.ID)
				if err != nil {
					return nil, fileError(err)
				}
				if session.Root != in.Root {
					return nil, fileDenied()
				}
				return []string{"fs:" + session.Root}, nil
			}, Execute: func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
				var in transferInput
				_ = json.Unmarshal(raw, &in)
				var reader io.Reader
				if stream, _ := ctx.Value(transferContextKey{}).(*fileStream); stream != nil {
					reader = stream.reader
				} else {
					chunk, err := base64.StdEncoding.Strict().DecodeString(in.Base64)
					if err != nil || len(chunk) > 32<<10 {
						return core.ActionResult{}, fileInvalid()
					}
					reader = bytes.NewReader(chunk)
				}
				if stream, _ := ctx.Value(transferContextKey{}).(*fileStream); stream != nil {
					reader = &validatedReader{reader: reader, ctx: ctx, check: stream.validate}
				}
				data, err := a.uploadChunk(ctx, actor, in, io.NopCloser(reader))
				return core.ActionResult{Data: data, Resources: []string{"fs:" + in.Root}, AuditSummary: map[string]any{"upload_id": in.ID, "offset": in.Offset}}, fileError(err)
			}},
		{Action: core.Action{Name: "fs_download", Description: "Download a bounded binary range through MCP or stream an authorized file through HTTP", Scope: "files:read", ReadOnly: true, Audit: true, InputSchema: object(map[string]any{"root": str, "path": str, "offset": map[string]any{"type": "integer", "minimum": 0}, "length": map[string]any{"type": "integer", "minimum": 1, "maximum": 32768}}, []string{"root", "path"})},
			Resources: func(ctx context.Context, actor core.Actor, raw json.RawMessage) ([]string, error) {
				var in transferInput
				if json.Unmarshal(raw, &in) != nil {
					return nil, fileInvalid()
				}
				if !a.authorize(actor, "files:read", in.Root) {
					return nil, fileDenied()
				}
				return a.resources(ctx, actor, files.Action{Name: "fs_read", Scope: "files:read"}, mustJSON(files.Input{Root: in.Root, Path: in.Path}))
			}, Execute: func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
				var in transferInput
				_ = json.Unmarshal(raw, &in)
				file, entry, err := a.openSeekFile(ctx, actor, files.Ref{Root: in.Root, Path: in.Path})
				if err != nil {
					return core.ActionResult{}, fileError(err)
				}
				defer file.Close()
				var data any
				if stream, _ := ctx.Value(transferContextKey{}).(*fileStream); stream != nil {
					stream.writer.Header().Set("Content-Type", "application/octet-stream")
					stream.writer.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": entry.Name}))
					stream.started = true
					authorized := &authorizedFile{File: file, ctx: ctx, service: a.service, actor: actor, root: in.Root, revalidate: stream.validate, adapter: a}
					http.ServeContent(stream.writer, stream.request, entry.Name, entry.ModifiedAt, authorized)
					if authorized.err != nil {
						return core.ActionResult{}, fileError(authorized.err)
					}
					data = map[string]any{"path": entry.Path, "size": entry.Size}
				} else {
					if in.Offset > entry.Size {
						return core.ActionResult{}, fileInvalid()
					}
					if in.Length == 0 {
						in.Length = 32 << 10
					}
					file.Seek(in.Offset, io.SeekStart)
					content, readErr := io.ReadAll(io.LimitReader(file, in.Length))
					if readErr != nil {
						return core.ActionResult{}, fileError(readErr)
					}
					data = map[string]any{"path": entry.Path, "size": entry.Size, "offset": in.Offset, "data_base64": base64.StdEncoding.EncodeToString(content), "next_offset": in.Offset + int64(len(content)), "complete": in.Offset+int64(len(content)) == entry.Size}
				}
				return core.ActionResult{Data: data, Resources: []string{"fs:" + in.Root}, AuditSummary: map[string]any{"path": entry.Path, "size": entry.Size}}, nil
			}},
	}
}

func mustJSON(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }

type authorizedFile struct {
	File       io.ReadSeekCloser
	adapter    *fileAdapter
	ctx        context.Context
	service    *core.Service
	actor      core.Actor
	root       string
	checked    time.Time
	revalidate func() (core.Actor, error)
	err        error
}

func (f *authorizedFile) Read(buf []byte) (int, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	if time.Since(f.checked) > 250*time.Millisecond {
		actor, err := f.service.RefreshActor(f.ctx, f.actor)
		if f.revalidate != nil {
			actor, err = f.revalidate()
		}
		if err != nil {
			f.err = err
			return 0, err
		}
		if !f.service.Authorized(actor, "files:read", "fs:"+f.root) {
			f.err = fileDenied()
			return 0, fileDenied()
		}
		if f.adapter != nil {
			if _, err := f.adapter.authorizeRoot(f.ctx, actor, "files:read", f.root); err != nil {
				f.err = err
				return 0, err
			}
		}
		f.checked = time.Now()
	}
	n, err := f.File.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		f.err = err
	}
	return n, err
}
func (f *authorizedFile) Seek(offset int64, whence int) (int64, error) {
	return f.File.Seek(offset, whence)
}

type validatedReader struct {
	reader  io.Reader
	ctx     context.Context
	check   func() (core.Actor, error)
	checked time.Time
}

func (r *validatedReader) Read(buf []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if time.Since(r.checked) > 250*time.Millisecond {
		if _, err := r.check(); err != nil {
			return 0, err
		}
		r.checked = time.Now()
	}
	return r.reader.Read(buf)
}

// Called only after the HTTP adapter authenticates and validates Origin/CSRF.
func (a *fileAdapter) HandleResource(w http.ResponseWriter, r *http.Request, actor core.Actor) bool {
	path := r.URL.Path
	if path != "/api/v1/files/download" && path != "/api/v1/files/text" && !(strings.HasPrefix(path, "/api/v1/files/uploads/") && strings.HasSuffix(path, "/chunk")) {
		return false
	}
	select {
	case a.streams <- struct{}{}:
		defer func() { <-a.streams }()
	default:
		writeFileError(w, &core.Error{Code: "stream_busy", Message: "File transfer slots are busy; retry shortly", Status: 429})
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	stream := &fileStream{writer: w, request: r, validate: func() (core.Actor, error) { return httpapi.RevalidateActor(r) }}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	defer func() { _ = controller.SetReadDeadline(time.Time{}); _ = controller.SetWriteDeadline(time.Time{}) }()
	if path == "/api/v1/files/text" {
		return a.handleText(w, r, actor, stream)
	}
	var action string
	var input transferInput
	var err error
	query := r.URL.Query()
	input.Root = query.Get("root")
	if path == "/api/v1/files/download" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeFileError(w, &core.Error{Code: "method_not_allowed", Message: "Method not allowed", Status: 405})
			return true
		}
		input.Path = query.Get("path")
		action = "fs_download"
	} else {
		if r.Method != http.MethodPatch {
			w.Header().Set("Allow", "PATCH")
			writeFileError(w, &core.Error{Code: "method_not_allowed", Message: "Method not allowed", Status: 405})
			return true
		}
		input.ID = strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/files/uploads/"), "/chunk")
		input.Offset, err = strconv.ParseInt(query.Get("offset"), 10, 64)
		if err != nil || input.Offset < 0 || strings.Contains(input.ID, "/") {
			writeFileError(w, fileInvalid())
			return true
		}
		input.SHA256 = query.Get("sha256")
		action = "fs_upload_chunk"
		stream.reader = http.MaxBytesReader(w, r.Body, 4<<20)
	}
	ctx := context.WithValue(r.Context(), transferContextKey{}, stream)
	data, err := a.service.Execute(ctx, actor, action, mustJSON(input))
	if stream.started {
		return true
	}
	if err != nil {
		writeFileError(w, fileError(err))
		return true
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	return true
}

func writeFileError(w http.ResponseWriter, err error) {
	var e *core.Error
	if !errors.As(fileError(err), &e) {
		e = &core.Error{Code: "internal_error", Message: "File transfer failed", Status: 500}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": e})
}
