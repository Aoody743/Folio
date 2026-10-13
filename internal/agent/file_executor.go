package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// FileExecutor provides a small native Agent surface. It never starts a
// subprocess and all paths pass through the configured scoped Filesystem.
type FileExecutor struct{ fs *Filesystem }

func NewFileExecutor(filesystem *Filesystem) *FileExecutor { return &FileExecutor{fs: filesystem} }

type FileInput struct {
	Root            string `json:"root"`
	Path            string `json:"path"`
	Content         string `json:"content,omitempty"`
	DestinationRoot string `json:"destination_root,omitempty"`
	DestinationPath string `json:"destination_path,omitempty"`
}

func parseFileInput(raw json.RawMessage) (FileInput, error) {
	var input FileInput
	if len(raw) > 1<<20 {
		return input, ErrInvalidInput
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return input, ErrInvalidInput
	}
	return input, nil
}

func (e *FileExecutor) Validate(request Request) error {
	if e == nil || e.fs == nil {
		return errors.New("filesystem executor is not configured")
	}
	input, err := parseFileInput(request.Input)
	if err != nil {
		return err
	}
	switch request.Operation {
	case "file_write":
		_, _, err = e.fs.resolve(input.Root, input.Path, true)
		if err == nil && int64(len(input.Content)) > e.fs.maxFileBytes {
			return ErrFileTooLarge
		}
	case "file_hash":
		_, _, err = e.fs.resolve(input.Root, input.Path, false)
	case "file_backup":
		_, _, err = e.fs.resolve(input.Root, input.Path, false)
		if err == nil {
			_, _, err = e.fs.resolve(input.DestinationRoot, input.DestinationPath, true)
		}
	default:
		return ErrUnauthorized
	}
	return err
}

func (e *FileExecutor) Run(ctx context.Context, request Request) (result Result, returnedError error) {
	result = Result{Operation: request.Operation, ExitCode: -1, StartedAt: time.Now().UTC()}
	defer func() {
		result.FinishedAt = time.Now().UTC()
		result.Canceled = errors.Is(returnedError, context.Canceled)
		result.TimedOut = errors.Is(returnedError, context.DeadlineExceeded)
	}()
	if err := e.Validate(request); err != nil {
		return result, err
	}
	input, _ := parseFileInput(request.Input)
	if err := ctx.Err(); err != nil {
		return result, err
	}
	var err error
	switch request.Operation {
	case "file_write":
		err = e.fs.WriteFile(input.Root, input.Path, []byte(input.Content))
		if err == nil {
			result.Stdout = fmt.Sprintf("wrote %d bytes\n", len(input.Content))
		}
	case "file_hash", "file_backup":
		var content []byte
		content, err = e.fs.ReadFile(input.Root, input.Path)
		if err == nil {
			err = ctx.Err()
		}
		if err == nil && request.Operation == "file_backup" {
			err = e.fs.WriteFile(input.DestinationRoot, input.DestinationPath, content)
		}
		if err == nil {
			digest := sha256.Sum256(content)
			result.Stdout = hex.EncodeToString(digest[:]) + "\n"
		}
	}
	result.FinishedAt = time.Now().UTC()
	if err != nil {
		result.Canceled, result.TimedOut = errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded)
		return result, err
	}
	result.ExitCode = 0
	if request.Output != nil {
		select {
		case request.Output <- Output{Stream: "stdout", Text: result.Stdout}:
		default:
			result.DroppedEvents++
		}
	}
	return result, nil
}
