package files

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
	"unicode/utf8"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

// Remote keeps privileged roots in the Agent. It never mirrors an absolute
// path or opens the root in the API process; the adapter supplies the verified
// caller identity and performs source/destination scope checks before each RPC.
type Remote struct{ Client *agent.UnixClient }

func (r *Remote) Execute(ctx context.Context, actorID, operation string, input json.RawMessage) (any, error) {
	result, err := r.Client.Run(ctx, agent.Request{ActorID: actorID, Operation: operation, Input: input})
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(result.Data, &value); err != nil {
		return nil, err
	}
	return value, nil
}
func (r *Remote) Roots(ctx context.Context, actorID string) ([]map[string]any, error) {
	result, err := r.Client.Run(ctx, agent.Request{ActorID: actorID, Operation: "fs_roots", Input: json.RawMessage(`{}`)})
	if err != nil {
		return nil, err
	}
	var roots []map[string]any
	err = json.Unmarshal(result.Data, &roots)
	return roots, err
}
func (r *Remote) Stat(ctx context.Context, actorID string, ref Ref) (Entry, error) {
	result, err := r.Client.Run(ctx, agent.Request{ActorID: actorID, Operation: "fs_stat", Input: encode(Input{Root: ref.Root, Path: ref.Path})})
	if err != nil {
		return Entry{}, err
	}
	var entry Entry
	err = json.Unmarshal(result.Data, &entry)
	return entry, err
}

type cancelReader struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (r cancelReader) Close() error { r.cancel(); return r.PipeReader.Close() }
func (r *Remote) OpenDownload(ctx context.Context, actorID string, ref Ref, offset, length int64) (io.ReadCloser, Entry, error) {
	entry, err := r.Stat(ctx, actorID, ref)
	if err != nil {
		return nil, Entry{}, err
	}
	if entry.Type != "file" || offset < 0 || offset > entry.Size || length < 0 || offset+length > entry.Size {
		return nil, Entry{}, failure("invalid_input", "Invalid download range", 400)
	}
	if length == 0 {
		length = entry.Size - offset
	}
	ctx, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	go func() {
		_, err := r.Client.Stream(ctx, agent.Request{ActorID: actorID, Operation: "fs_download_stream", Input: encode(Input{Root: ref.Root, Path: ref.Path, Offset: int(offset), Size: length})}, agent.IO{Stdout: writer})
		writer.CloseWithError(err)
		cancel()
	}()
	return cancelReader{reader, cancel}, entry, nil
}
func (r *Remote) UploadStatus(ctx context.Context, actorID, root, id string) (Upload, error) {
	result, err := r.Client.Run(ctx, agent.Request{ActorID: actorID, Operation: "fs_upload_status", Input: encode(Input{Root: root, ID: id})})
	if err != nil {
		return Upload{}, err
	}
	var upload Upload
	err = json.Unmarshal(result.Data, &upload)
	return upload, err
}
func (r *Remote) UploadChunk(ctx context.Context, actorID, root, id string, offset int64, reader io.ReadCloser, checksum string) (Upload, error) {
	result, err := r.Client.Stream(ctx, agent.Request{ActorID: actorID, Operation: "fs_upload_chunk", Input: encode(Input{Root: root, ID: id, Offset: int(offset), SHA256: checksum})}, agent.IO{Stdin: reader})
	if err != nil {
		return Upload{}, err
	}
	var upload Upload
	err = json.Unmarshal(result.Data, &upload)
	return upload, err
}

func (r *Remote) SaveText(ctx context.Context, actorID string, in Input, reader io.ReadCloser) (any, error) {
	in.Content = ""
	result, err := r.Client.Stream(ctx, agent.Request{ActorID: actorID, Operation: "fs_edit_stream", Input: encode(in)}, agent.IO{Stdin: reader})
	if err != nil {
		return nil, err
	}
	var data any
	err = json.Unmarshal(result.Data, &data)
	return data, err
}

func AgentOperationNames() []string {
	names := []string{"fs_stat", "fs_download_stream", "fs_upload_chunk", "fs_edit_stream", "fs_export_tree_stream", "fs_import_tree_stream", "fs_archive_stream", "fs_import_file_stream", "fs_extract_stream"}
	for _, action := range (&Service{}).Actions() {
		names = append(names, action.Name)
	}
	return names
}
func (e *Executor) Stream(ctx context.Context, request agent.Request, streams agent.IO) (result agent.Result, returnedError error) {
	result = agent.Result{Operation: request.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	defer func() {
		returnedError = moduleError(returnedError)
		if result.FinishedAt.IsZero() {
			result.FinishedAt = time.Now().UTC()
		}
	}()
	in, err := parse(request.Input)
	if err != nil {
		return result, err
	}
	if request.ActorID == "" {
		return result, failure("unauthorized", "Trusted actor identity required", 401)
	}
	ctx, release, err := e.Service.beginRequest(ctx, in.Root, in.DestinationRoot)
	if err != nil {
		return result, err
	}
	defer release()
	switch request.Operation {
	case "fs_archive_stream":
		if streams.Stdout == nil {
			return result, failure("invalid_input", "Archive export requires an output sink", 400)
		}
		data, n, err := e.Service.streamArchive(ctx, in, streams.Stdout)
		result.StdoutBytes = n
		result.Data = encode(data)
		if err != nil {
			return result, err
		}
	case "fs_import_file_stream", "fs_extract_stream":
		if streams.Stdin == nil {
			return result, failure("invalid_input", "Artifact import requires an input stream", 400)
		}
		data, err := e.Service.receiveArtifact(ctx, request.ActorID, in, streams.Stdin, request.Operation == "fs_extract_stream")
		result.Data = encode(data)
		if err != nil {
			return result, err
		}
	case "fs_export_tree_stream":
		if streams.Stdout == nil {
			return result, failure("invalid_input", "Tree export requires an output sink", 400)
		}
		root, path, err := e.Service.resolve(Ref{in.Root, in.Path}, false)
		if err != nil {
			return result, err
		}
		unlock, err := e.Service.lock(ctx, in.Root)
		if err != nil {
			return result, err
		}
		defer unlock()
		stop := context.AfterFunc(ctx, func() { streams.Stdout.Close() })
		defer stop()
		digest, n, err := e.Service.exportTree(ctx, root, path, streams.Stdout)
		result.StdoutBytes = n
		result.Data = encode(map[string]any{"sha256": digest, "bytes": n})
		if err != nil {
			return result, err
		}
	case "fs_import_tree_stream":
		if streams.Stdin == nil {
			return result, failure("invalid_input", "Tree import requires an input stream", 400)
		}
		data, err := e.Service.importTree(ctx, request.ActorID, in, streams.Stdin)
		result.Data = encode(data)
		if err != nil {
			return result, err
		}
	case "fs_edit_stream":
		if streams.Stdin == nil {
			return result, failure("invalid_input", "Text edit requires a UTF-8 input stream", 400)
		}
		content, err := io.ReadAll(io.LimitReader(contextReader{ctx, streams.Stdin}, e.Service.options.MaxTextBytes+1))
		if err != nil {
			return result, err
		}
		if int64(len(content)) > e.Service.options.MaxTextBytes {
			return result, failure("too_large", "Text stream exceeds editor budget", 413)
		}
		if !utf8.Valid(content) {
			return result, failure("invalid_encoding", "Text input stream must use UTF-8", 400)
		}
		in.Content = string(content)
		data, err := e.Service.Edit(ctx, request.ActorID, in)
		result.Data = encode(data)
		if err != nil {
			return result, err
		}
	case "fs_download_stream":
		if streams.Stdout == nil {
			return result, failure("invalid_input", "Download requires a binary output sink", 400)
		}
		file, entry, err := e.Service.OpenDownload(ctx, Ref{in.Root, in.Path})
		if err != nil {
			return result, err
		}
		defer file.Close()
		offset, length := int64(in.Offset), in.Size
		if length == 0 {
			length = entry.Size - offset
		}
		if offset < 0 || length < 0 || offset+length > entry.Size || length > e.Service.options.MaxTransferBytes {
			return result, failure("invalid_input", "Download range exceeds file or transfer budget", 400)
		}
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return result, err
		}
		stop := context.AfterFunc(ctx, func() { streams.Stdout.Close(); file.Close() })
		defer stop()
		n, err := io.CopyBuffer(streams.Stdout, io.LimitReader(contextReader{ctx, file}, length), make([]byte, 32<<10))
		result.StdoutBytes = n
		result.Data = encode(entry)
		if err != nil {
			return result, err
		}
		if n != length {
			return result, failure("conflict", "File changed during download", 409)
		}
	case "fs_upload_chunk":
		if streams.Stdin == nil {
			return result, failure("invalid_input", "Upload requires a binary input source", 400)
		}
		session, err := e.Service.UploadStatus(ctx, request.ActorID, in.ID)
		if err != nil {
			return result, err
		}
		if session.Root != in.Root {
			return result, failure("forbidden", "Upload root mismatch", 403)
		}
		session, err = e.Service.UploadChunk(ctx, request.ActorID, in.ID, int64(in.Offset), streams.Stdin, in.SHA256)
		result.Data = encode(session)
		if err != nil {
			return result, err
		}
	default:
		return result, errors.New("filesystem operation does not support binary streaming")
	}
	result.ExitCode = 0
	result.FinishedAt = time.Now().UTC()
	return result, nil
}
