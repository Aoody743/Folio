package files

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
)

type PreflightItem struct {
	Name        string `json:"name"`
	Source      string `json:"source"`
	Destination string `json:"destination,omitempty"`
	Entry       Entry  `json:"entry"`
	Exists      bool   `json:"exists"`
	CurrentHash string `json:"current_hash,omitempty"`
}
type PreflightResult struct {
	Items     []PreflightItem `json:"items"`
	Total     int             `json:"total"`
	Conflicts int             `json:"conflicts"`
}
type BatchOutcome struct {
	Name      string `json:"name"`
	Completed bool   `json:"completed"`
	Data      any    `json:"data,omitempty"`
	Code      string `json:"code,omitempty"`
}
type BatchResult struct {
	Items     []BatchOutcome `json:"items"`
	Completed int            `json:"completed"`
	Failed    int            `json:"failed"`
}

func (s *Service) Preflight(ctx context.Context, in Input) (PreflightResult, error) {
	out := PreflightResult{Items: []PreflightItem{}}
	if in.Operation != "copy" && in.Operation != "move" && in.Operation != "delete" && in.Operation != "chmod" {
		return out, failure("invalid_input", "Batch operation must be copy, move, delete or chmod", 400)
	}
	if len(in.Items) < 1 || len(in.Items) > 500 || len(in.ExpectedHashes) > 500 {
		return out, failure("invalid_input", "Batch needs 1..500 paths and at most 500 hashes", 400)
	}
	_, base, err := s.resolve(Ref{in.Root, in.Path}, false)
	if err != nil {
		return out, err
	}
	copying := in.Operation == "copy" || in.Operation == "move"
	if copying && in.DestinationRoot == "" {
		return out, failure("invalid_input", "Copy/move needs a destination root", 400)
	}
	for name, digest := range in.ExpectedHashes {
		if _, err := clean(name); err != nil || !digestPattern.MatchString(digest) {
			return out, failure("invalid_input", "Invalid expected_hashes entry", 400)
		}
	}
	seen := map[string]bool{}
	for _, item := range in.Items {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		name, err := clean(item)
		if err != nil {
			return out, err
		}
		if name == "." || seen[name] {
			return out, failure("invalid_input", "Batch paths must be distinct and cannot select the root", 400)
		}
		seen[name] = true
		path := filepath.Join(base, name)
		root, path, err := s.resolve(Ref{in.Root, path}, in.Operation != "copy")
		if err != nil {
			return out, err
		}
		info, err := strictLstat(root.root, path)
		if err != nil {
			return out, err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return out, failure("unsafe_path", "Only regular files and directories can be batched", 403)
		}
		if info.IsDir() && !in.Recursive {
			return out, failure("invalid_input", "Directory batches require recursive=true", 400)
		}
		record := PreflightItem{Name: name, Source: path, Entry: fileEntry(filepath.Base(path), path, info)}
		if copying {
			// Selection paths are relative to the source base. Flattening nested
			// names would create implicit collisions and surprise users.
			destination := filepath.Join(in.DestinationPath, name)
			dest, target, err := s.resolve(Ref{in.DestinationRoot, destination}, true)
			if err != nil {
				return out, err
			}
			if in.Root == in.DestinationRoot && insidePath(path, target) {
				return out, failure("invalid_input", "Cannot paste a path into itself", 400)
			}
			record.Destination = target
			prior, err := strictLstat(dest.root, target)
			if err == nil {
				record.Exists = true
				out.Conflicts++
				if prior.Mode().IsRegular() {
					record.CurrentHash, err = s.hash(ctx, Ref{in.DestinationRoot, target})
					if err != nil {
						return out, err
					}
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				return out, err
			}
		}
		out.Items = append(out.Items, record)
	}
	// Ancestor selections would execute overlapping mutations twice.
	for i, a := range out.Items {
		for j, b := range out.Items {
			if i != j && insidePath(a.Source, b.Source) {
				return out, failure("invalid_input", "Batch paths overlap", 400)
			}
		}
	}
	out.Total = len(out.Items)
	return out, nil
}

func (s *Service) Batch(ctx context.Context, actorID string, in Input) (BatchResult, error) {
	result := BatchResult{Items: []BatchOutcome{}}
	if in.Operation == "chmod" && in.Mode == nil && in.UID == nil && in.GID == nil {
		return result, failure("invalid_input", "Permission batch needs a mode, UID or GID", 400)
	}
	if in.Mode != nil && *in.Mode > 0777 {
		return result, failure("invalid_input", "Special permission bits are forbidden", 400)
	}
	preflight, err := s.Preflight(ctx, in)
	if err != nil {
		return result, err
	}
	for _, item := range preflight.Items {
		if item.Exists {
			expected := in.ExpectedHashes[item.Destination]
			if item.Entry.Type == "directory" || item.CurrentHash == "" || expected == "" || expected != item.CurrentHash {
				return result, &Error{Code: "conflict", Message: "Existing batch destinations require matching preflight hashes; directories cannot be merged", Status: 409, Details: preflight}
			}
		}
	}
	for _, item := range preflight.Items {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		one := Input{Root: in.Root, Path: item.Source, DestinationRoot: in.DestinationRoot, DestinationPath: item.Destination, Recursive: in.Recursive, ExpectedHash: in.ExpectedHashes[item.Destination], Mode: in.Mode, UID: in.UID, GID: in.GID}
		var data any
		switch in.Operation {
		case "copy", "move":
			data, err = s.CopyAs(ctx, actorID, one, in.Operation == "move")
		case "delete":
			data, err = s.Delete(ctx, actorID, one)
		case "chmod":
			data, err = s.Permissions(ctx, one)
		}
		outcome := BatchOutcome{Name: item.Name, Completed: err == nil, Data: data}
		if err != nil {
			result.Failed++
			outcome.Code = "file_operation_failed"
			var typed *Error
			if errors.As(err, &typed) {
				outcome.Code = typed.Code
			}
		} else {
			result.Completed++
		}
		result.Items = append(result.Items, outcome)
	}
	if result.Failed > 0 {
		return result, &Error{Code: "partial_failure", Message: "Some batch items failed; inspect per-item results before retrying", Status: 409, Details: result}
	}
	return result, nil
}
