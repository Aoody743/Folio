package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/files"
)

type fileTextInput struct {
	Root          string  `json:"root"`
	Path          string  `json:"path"`
	Encoding      string  `json:"encoding,omitempty"`
	ExpectedHash  string  `json:"expected_hash,omitempty"`
	Overwrite     bool    `json:"overwrite,omitempty"`
	Content       *string `json:"content,omitempty"`
	ContentSHA256 string  `json:"content_sha256,omitempty"`
}

func (a *fileAdapter) textBinding() core.Binding {
	stringField := map[string]any{"type": "string", "maxLength": 2048}
	schema := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"root", "path"}, "properties": map[string]any{"root": stringField, "path": stringField, "encoding": map[string]any{"type": "string", "enum": []string{"utf-8", "utf8", "gb18030", "gbk", "big5"}}, "expected_hash": map[string]any{"type": "string", "pattern": `^[a-f0-9]{64}$`}, "overwrite": map[string]any{"type": "boolean"}, "content": map[string]any{"type": "string", "maxLength": 49152}, "content_sha256": map[string]any{"type": "string", "pattern": `^[a-f0-9]{64}$`}}}
	return core.Binding{Action: core.Action{Name: "fs_text_save", Description: "Save text with conflict/history preservation; MCP accepts bounded inline content, HTTP streams UTF-8 up to two MiB and preserves the requested disk encoding", Scope: "files:write", Audit: true, InputSchema: schema}, Resources: func(ctx context.Context, actor core.Actor, raw json.RawMessage) ([]string, error) {
		var in fileTextInput
		if json.Unmarshal(raw, &in) != nil {
			return nil, fileInvalid()
		}
		return a.resources(ctx, actor, files.Action{Name: "fs_edit", Scope: "files:write"}, mustJSON(files.Input{Root: in.Root, Path: in.Path}))
	}, Execute: func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		var in fileTextInput
		if json.Unmarshal(raw, &in) != nil {
			return core.ActionResult{}, fileInvalid()
		}
		var content []byte
		if stream, _ := ctx.Value(transferContextKey{}).(*fileStream); stream != nil && stream.isText {
			content = stream.text
		} else {
			if in.Content == nil {
				return core.ActionResult{}, fileInvalid()
			}
			content = []byte(*in.Content)
		}
		if len(content) > 2<<20 {
			return core.ActionResult{}, &core.Error{Code: "payload_too_large", Message: "Text exceeds two MiB", Status: 413}
		}
		if !utf8.Valid(content) {
			return core.ActionResult{}, &core.Error{Code: "invalid_encoding", Message: "Text request body must use UTF-8", Status: 400}
		}
		digest := sha256.Sum256(content)
		checksum := hex.EncodeToString(digest[:])
		if in.ContentSHA256 != "" && in.ContentSHA256 != checksum {
			return core.ActionResult{}, &core.Error{Code: "checksum_mismatch", Message: "Text input digest does not match", Status: 400}
		}
		input := files.Input{Root: in.Root, Path: in.Path, Content: string(content), Encoding: in.Encoding, ExpectedHash: in.ExpectedHash, Overwrite: in.Overwrite}
		root, err := a.authorizeRoot(ctx, actor, "files:write", in.Root)
		if err != nil {
			return core.ActionResult{}, err
		}
		var data any
		if root.Remote {
			data, err = a.remote.SaveText(ctx, actor.ID, input, io.NopCloser(bytes.NewReader(content)))
		} else {
			data, err = a.files.Edit(ctx, actor.ID, input)
		}
		return core.ActionResult{Data: data, Resources: []string{"fs:" + in.Root}, AuditSummary: map[string]any{"path": in.Path, "input_bytes": len(content), "input_sha256": checksum}}, fileError(err)
	}}
}

func (a *fileAdapter) handleText(w http.ResponseWriter, r *http.Request, actor core.Actor, stream *fileStream) bool {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		writeFileError(w, &core.Error{Code: "method_not_allowed", Message: "Method not allowed", Status: 405})
		return true
	}
	q := r.URL.Query()
	in := fileTextInput{Root: q.Get("root"), Path: q.Get("path"), Encoding: q.Get("encoding"), ExpectedHash: q.Get("expected_hash")}
	var err error
	if value := q.Get("overwrite"); value != "" {
		in.Overwrite, err = strconv.ParseBool(value)
		if err != nil {
			writeFileError(w, fileInvalid())
			return true
		}
	}
	if _, err = a.resources(r.Context(), actor, files.Action{Name: "fs_edit", Scope: "files:write"}, mustJSON(files.Input{Root: in.Root, Path: in.Path})); err != nil {
		// Dispatch the denied attempt through core as well, without consuming a
		// body that belongs to an unauthorized root.
		_, err = a.service.Execute(r.Context(), actor, "fs_text_save", mustJSON(in))
		writeFileError(w, err)
		return true
	}
	body := &validatedReader{reader: http.MaxBytesReader(w, r.Body, 2<<20), ctx: r.Context(), check: stream.validate}
	content, err := io.ReadAll(body)
	if err != nil {
		var budget *http.MaxBytesError
		if errors.As(err, &budget) {
			err = &core.Error{Code: "payload_too_large", Message: "Text exceeds two MiB", Status: 413}
		}
		writeFileError(w, err)
		return true
	}
	if !utf8.Valid(content) {
		writeFileError(w, &core.Error{Code: "invalid_encoding", Message: "Text request body must use UTF-8", Status: 400})
		return true
	}
	digest := sha256.Sum256(content)
	in.ContentSHA256 = hex.EncodeToString(digest[:])
	stream.text = content
	stream.isText = true
	ctx := context.WithValue(r.Context(), transferContextKey{}, stream)
	// Bind an idempotency key to the actual body digest, not just its path.
	data, err := a.service.ExecuteWithKey(ctx, actor, "fs_text_save", mustJSON(in), r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeFileError(w, err)
		return true
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	return true
}
