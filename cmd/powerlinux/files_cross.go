package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/files"
)

func asUpload(value any) (files.Upload, error) {
	if typed, ok := value.(files.Upload); ok {
		return typed, nil
	}
	var upload files.Upload
	err := json.Unmarshal(mustJSON(value), &upload)
	return upload, err
}

func (a *fileAdapter) copyBetweenServices(ctx context.Context, actor core.Actor, in files.Input, move bool) (any, error) {
	ctx, stop := a.watchTransfer(ctx, actor, in.Root, in.DestinationRoot, move)
	defer stop()
	entry, err := a.statFile(ctx, actor, files.Ref{Root: in.Root, Path: in.Path})
	if err != nil {
		return nil, err
	}
	if entry.Type == "directory" {
		return a.copyDirectoryBetweenServices(ctx, actor, in, move, entry)
	}
	reader, entry, err := a.openFileRange(ctx, actor, files.Ref{Root: in.Root, Path: in.Path}, 0, 0)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	if entry.Type != "file" {
		return nil, &core.Error{Code: "invalid_input", Message: "This transfer requires a regular file", Status: 400}
	}
	mode := entry.Mode
	value, err := a.executeOnRoot(ctx, actor, "fs_upload_begin", files.Input{Root: in.DestinationRoot, Path: in.DestinationPath, Size: entry.Size, ExpectedHash: in.ExpectedHash, Overwrite: in.Overwrite, Mode: &mode})
	if err != nil {
		return nil, err
	}
	upload, err := asUpload(value)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			a.executeOnRoot(cleanup, actor, "fs_upload_cancel", files.Input{Root: in.DestinationRoot, ID: upload.ID})
		}
	}()
	hash := sha256.New()
	buffer := make([]byte, 256<<10)
	var offset int64
	for offset < entry.Size {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fresh, err := a.service.RefreshActor(ctx, actor)
		if err != nil {
			return nil, err
		}
		if _, err := a.authorizeRoot(ctx, fresh, "files:read", in.Root); err != nil {
			return nil, err
		}
		if _, err := a.authorizeRoot(ctx, fresh, "files:write", in.DestinationRoot); err != nil {
			return nil, err
		}
		want := min(int64(len(buffer)), entry.Size-offset)
		n, err := io.ReadFull(reader, buffer[:want])
		if err != nil {
			return nil, err
		}
		hash.Write(buffer[:n])
		chunk := buffer[:n]
		upload, err = a.uploadChunk(ctx, fresh, transferInput{Root: in.DestinationRoot, ID: upload.ID, Offset: offset}, io.NopCloser(bytes.NewReader(chunk)))
		if err != nil {
			return nil, err
		}
		offset += int64(n)
		if upload.Offset != offset {
			return nil, &core.Error{Code: "conflict", Message: "Cross-root transfer offset changed", Status: 409}
		}
	}
	if n, err := reader.Read(buffer[:1]); n != 0 || err != io.EOF {
		return nil, &core.Error{Code: "conflict", Message: "Source size changed during cross-root copy", Status: 409}
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	data, err := a.executeOnRoot(ctx, actor, "fs_upload_commit", files.Input{Root: in.DestinationRoot, ID: upload.ID, SHA256: digest})
	if err != nil {
		return nil, err
	}
	committed = true
	if move {
		if _, err := a.executeOnRoot(ctx, actor, "fs_delete", files.Input{Root: in.Root, Path: in.Path, ExpectedHash: digest}); err != nil {
			return map[string]any{"destination": data, "copied": true, "moved": false}, err
		}
	}
	return map[string]any{"destination": data, "sha256": digest, "bytes": offset, "moved": move}, nil
}

func (a *fileAdapter) watchTransfer(ctx context.Context, actor core.Actor, source, destination string, move bool) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fresh, err := a.service.RefreshActor(ctx, actor)
				if err == nil {
					_, err = a.authorizeRoot(ctx, fresh, "files:read", source)
				}
				if err == nil && move {
					_, err = a.authorizeRoot(ctx, fresh, "files:write", source)
				}
				if err == nil {
					_, err = a.authorizeRoot(ctx, fresh, "files:write", destination)
				}
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}

func (a *fileAdapter) artifactBetweenServices(ctx context.Context, actor core.Actor, in files.Input, extract bool) (any, error) {
	ctx, stop := a.watchTransfer(ctx, actor, in.Root, in.DestinationRoot, false)
	defer stop()
	reader, writer := io.Pipe()
	defer reader.Close()
	produced := make(chan error, 1)
	go func() {
		var err error
		if extract {
			var file io.ReadCloser
			file, _, err = a.openFileRange(ctx, actor, files.Ref{Root: in.Root, Path: in.Path}, 0, 0)
			if err == nil {
				_, err = io.CopyBuffer(writer, file, make([]byte, 32<<10))
				file.Close()
			}
		} else {
			root, lookupErr := a.rootInfo(ctx, actor, in.Root)
			err = lookupErr
			if err == nil {
				source := in
				source.DestinationRoot, source.DestinationPath = "", ""
				source.ExpectedHash = ""
				source.Overwrite = false
				request := agent.Request{ActorID: actor.ID, Operation: "fs_archive_stream", Input: mustJSON(source)}
				if root.Remote {
					_, err = a.remote.Client.Stream(ctx, request, agent.IO{Stdout: writer})
				} else {
					_, err = (&files.Executor{Service: a.files}).Stream(ctx, request, agent.IO{Stdout: writer})
				}
			}
		}
		writer.CloseWithError(err)
		produced <- err
	}()
	destination, err := a.rootInfo(ctx, actor, in.DestinationRoot)
	if err != nil {
		stop()
		reader.CloseWithError(err)
		<-produced
		return nil, err
	}
	input := files.Input{Root: in.DestinationRoot, Path: in.DestinationPath, Format: in.Format, ExpectedHash: in.ExpectedHash, Overwrite: in.Overwrite}
	operation := "fs_import_file_stream"
	if extract {
		operation = "fs_extract_stream"
		input.Items = []string{filepath.Base(in.Path)}
	}
	request := agent.Request{ActorID: actor.ID, Operation: operation, Input: mustJSON(input)}
	var result agent.Result
	if destination.Remote {
		result, err = a.remote.Client.Stream(ctx, request, agent.IO{Stdin: reader})
	} else {
		result, err = (&files.Executor{Service: a.files}).Stream(ctx, request, agent.IO{Stdin: reader})
	}
	if err != nil {
		stop()
		reader.CloseWithError(err)
		<-produced
		return nil, err
	}
	if err := <-produced; err != nil {
		return nil, err
	}
	var data any
	err = json.Unmarshal(result.Data, &data)
	return data, err
}

func (a *fileAdapter) copyDirectoryBetweenServices(ctx context.Context, actor core.Actor, in files.Input, move bool, entry files.Entry) (any, error) {
	if !in.Recursive {
		return nil, &core.Error{Code: "invalid_input", Message: "Directory transfers require recursive=true", Status: 400}
	}
	if _, err := a.statDestination(ctx, actor, in.DestinationRoot, in.DestinationPath); err == nil {
		return nil, &core.Error{Code: "conflict", Message: "Directory destination already exists", Status: 409}
	} else if core.ErrorCode(fileError(err)) != "not_found" {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	exported := make(chan error, 1)
	go func() {
		root, err := a.rootInfo(ctx, actor, in.Root)
		if err != nil {
			writer.CloseWithError(err)
			exported <- err
			return
		}
		request := agent.Request{ActorID: actor.ID, Operation: "fs_export_tree_stream", Input: mustJSON(files.Input{Root: in.Root, Path: in.Path})}
		if root.Remote {
			_, err = a.remote.Client.Stream(ctx, request, agent.IO{Stdout: writer})
		} else {
			_, err = (&files.Executor{Service: a.files}).Stream(ctx, request, agent.IO{Stdout: writer})
		}
		writer.CloseWithError(err)
		exported <- err
	}()
	check := func() (core.Actor, error) {
		fresh, err := a.service.RefreshActor(ctx, actor)
		if err != nil {
			return core.Actor{}, err
		}
		if _, err := a.authorizeRoot(ctx, fresh, "files:read", in.Root); err != nil {
			return core.Actor{}, err
		}
		if _, err := a.authorizeRoot(ctx, fresh, "files:write", in.DestinationRoot); err != nil {
			return core.Actor{}, err
		}
		return fresh, nil
	}
	input := files.Input{Root: in.DestinationRoot, Path: in.DestinationPath, Mode: &entry.Mode}
	request := agent.Request{ActorID: actor.ID, Operation: "fs_import_tree_stream", Input: mustJSON(input)}
	destination, err := a.rootInfo(ctx, actor, in.DestinationRoot)
	if err != nil {
		cancel()
		reader.CloseWithError(err)
		<-exported
		return nil, err
	}
	streams := agent.IO{Stdin: struct {
		io.Reader
		io.Closer
	}{&validatedReader{reader: reader, ctx: ctx, check: check}, reader}}
	var result agent.Result
	if destination.Remote {
		result, err = a.remote.Client.Stream(ctx, request, streams)
	} else {
		result, err = (&files.Executor{Service: a.files}).Stream(ctx, request, streams)
	}
	if err != nil {
		cancel()
		reader.CloseWithError(err)
		<-exported
		return nil, err
	}
	if exportError := <-exported; exportError != nil {
		return nil, exportError
	}
	var data map[string]any
	if err := json.Unmarshal(result.Data, &data); err != nil {
		return nil, err
	}
	if move {
		digest, _ := data["sha256"].(string)
		if _, err := a.executeOnRoot(ctx, actor, "fs_delete", files.Input{Root: in.Root, Path: in.Path, Recursive: true, ExpectedHash: digest}); err != nil {
			return map[string]any{"destination": data, "copied": true, "moved": false}, err
		}
	}
	return map[string]any{"destination": data, "moved": move}, nil
}

func (a *fileAdapter) statDestination(ctx context.Context, actor core.Actor, root, path string) (files.Entry, error) {
	info, err := a.authorizeRoot(ctx, actor, "files:write", root)
	if err != nil {
		return files.Entry{}, err
	}
	if info.Remote {
		return a.remote.Stat(ctx, actor.ID, files.Ref{Root: root, Path: path})
	}
	value, err := a.files.Execute(ctx, actor.ID, "fs_stat", mustJSON(files.Input{Root: root, Path: path}))
	if err != nil {
		return files.Entry{}, fileError(err)
	}
	return value.(files.Entry), nil
}

func (a *fileAdapter) executeOnRoot(ctx context.Context, actor core.Actor, operation string, in files.Input) (any, error) {
	actor, err := a.service.RefreshActor(ctx, actor)
	if err != nil {
		return nil, err
	}
	root, err := a.authorizeRoot(ctx, actor, "files:write", in.Root)
	if err != nil {
		return nil, err
	}
	if root.Remote {
		return a.remote.Execute(ctx, actor.ID, operation, mustJSON(in))
	}
	return a.files.Execute(ctx, actor.ID, operation, mustJSON(in))
}

func safeSelection(name string) bool {
	if name == "" || name == "." || filepath.IsAbs(name) || strings.ContainsAny(name, "\x00\\") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

func (a *fileAdapter) preflightBetweenServices(ctx context.Context, actor core.Actor, in files.Input) (files.PreflightResult, error) {
	result := files.PreflightResult{Items: []files.PreflightItem{}}
	if (in.Operation != "copy" && in.Operation != "move") || len(in.Items) < 1 || len(in.Items) > 500 {
		return result, fileInvalid()
	}
	seen := map[string]bool{}
	for _, name := range in.Items {
		if !safeSelection(name) || seen[name] {
			return result, fileInvalid()
		}
		seen[name] = true
		path := filepath.Join(in.Path, name)
		entry, err := a.statFile(ctx, actor, files.Ref{Root: in.Root, Path: path})
		if err != nil {
			return result, err
		}
		if entry.Type == "directory" && !in.Recursive {
			return result, fileInvalid()
		}
		item := files.PreflightItem{Name: name, Source: path, Destination: filepath.Join(in.DestinationPath, name), Entry: entry}
		prior, err := a.statDestination(ctx, actor, in.DestinationRoot, item.Destination)
		if err == nil {
			item.Exists = true
			result.Conflicts++
			if prior.Type == "file" {
				file, _, err := a.openSeekFile(ctx, actor, files.Ref{Root: in.DestinationRoot, Path: item.Destination})
				if err != nil {
					return result, err
				}
				hash := sha256.New()
				_, err = io.CopyBuffer(hash, &authorizedFile{File: file, ctx: ctx, service: a.service, actor: actor, root: in.DestinationRoot, adapter: a}, make([]byte, 32<<10))
				file.Close()
				if err != nil {
					return result, err
				}
				item.CurrentHash = hex.EncodeToString(hash.Sum(nil))
			}
		} else if core.ErrorCode(fileError(err)) != "not_found" {
			return result, err
		}
		result.Items = append(result.Items, item)
	}
	for i, a := range result.Items {
		for j, b := range result.Items {
			if i != j && strings.HasPrefix(b.Source, a.Source+"/") {
				return result, fileInvalid()
			}
		}
	}
	result.Total = len(result.Items)
	return result, nil
}

func (a *fileAdapter) batchBetweenServices(ctx context.Context, actor core.Actor, in files.Input) (files.BatchResult, error) {
	result := files.BatchResult{Items: []files.BatchOutcome{}}
	preflight, err := a.preflightBetweenServices(ctx, actor, in)
	if err != nil {
		return result, err
	}
	for _, item := range preflight.Items {
		if item.Exists && (item.Entry.Type == "directory" || item.CurrentHash == "" || in.ExpectedHashes[item.Destination] != item.CurrentHash) {
			return result, &core.Error{Code: "conflict", Message: "Existing batch destinations need matching preflight hashes", Status: 409, Details: preflight}
		}
	}
	for _, item := range preflight.Items {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		data, err := a.copyBetweenServices(ctx, actor, files.Input{Root: in.Root, Path: item.Source, DestinationRoot: in.DestinationRoot, DestinationPath: item.Destination, ExpectedHash: in.ExpectedHashes[item.Destination], Recursive: in.Recursive}, in.Operation == "move")
		outcome := files.BatchOutcome{Name: item.Name, Completed: err == nil, Data: data}
		if err != nil {
			outcome.Code = core.ErrorCode(fileError(err))
			result.Failed++
		} else {
			result.Completed++
		}
		result.Items = append(result.Items, outcome)
	}
	if result.Failed > 0 {
		return result, &core.Error{Code: "partial_failure", Message: "Some cross-root batch items failed; inspect per-item results", Status: 409, Details: result}
	}
	return result, nil
}
