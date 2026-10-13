package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// Policy is local trusted configuration. Runtime API requests cannot add root
// paths, executables, environment variables, or operations to this policy.
type Policy struct {
	Version      int        `json:"version"`
	AllowedUIDs  []uint32   `json:"allowed_uids"`
	Roots        []RootSpec `json:"roots"`
	Operations   []string   `json:"operations"`
	MaxFileBytes int64      `json:"max_file_bytes,omitempty"`
}

func LoadPolicy(path string) (Policy, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return Policy{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Policy{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return Policy{}, errors.New("policy must be a regular file not writable by group or others")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return Policy{}, errors.New("policy must be owned by the Agent service UID")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var policy Policy
	if err := decoder.Decode(&policy); err != nil {
		return Policy{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Policy{}, errors.New("policy must contain exactly one JSON document")
	}
	if policy.Version != 1 || len(policy.AllowedUIDs) == 0 || len(policy.Roots) == 0 || len(policy.Operations) == 0 {
		return Policy{}, errors.New("policy version 1 needs explicit peer UIDs, roots and operations")
	}
	for _, operation := range policy.Operations {
		if operation != "file_write" && operation != "file_backup" && operation != "file_hash" {
			return Policy{}, fmt.Errorf("unsupported policy operation %q", operation)
		}
	}
	return policy, nil
}

type PolicyExecutor struct {
	files      *Filesystem
	executor   *FileExecutor
	operations map[string]bool
}

func NewPolicyExecutor(policy Policy) (*PolicyExecutor, error) {
	if policy.Version != 1 || len(policy.Operations) == 0 {
		return nil, errors.New("policy version 1 and operation allowlist required")
	}
	files, err := NewFilesystem(policy.Roots, policy.MaxFileBytes)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(policy.Operations))
	for _, operation := range policy.Operations {
		if operation != "file_write" && operation != "file_backup" && operation != "file_hash" {
			files.Close()
			return nil, ErrUnauthorized
		}
		allowed[operation] = true
	}
	return &PolicyExecutor{files: files, executor: NewFileExecutor(files), operations: allowed}, nil
}

func (e *PolicyExecutor) Close() error { return e.files.Close() }
func (e *PolicyExecutor) Validate(request Request) error {
	if !e.operations[request.Operation] {
		return ErrUnauthorized
	}
	return e.executor.Validate(request)
}
func (e *PolicyExecutor) Run(ctx context.Context, request Request) (Result, error) {
	if err := e.Validate(request); err != nil {
		return Result{}, err
	}
	return e.executor.Run(ctx, request)
}
