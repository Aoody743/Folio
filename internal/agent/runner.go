// Package agent provides narrowly configured host operations. Callers must
// authorize actors before calling a Runner; the Runner independently enforces
// the operation allowlist and never accepts command lines or executable paths.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	ErrUnauthorized = errors.New("operation is not allowlisted")
	ErrInvalidInput = errors.New("invalid operation input")
)

// Operation is installed by trusted application code, never by an API request.
// BuildArgs must parse and validate typed input (including filesystem scope).
// Programs should accept a -- argument separator for user-provided operands.
// Shell interpreters are deliberately not rejected by name: a trusted build
// may run a fixed script, but MUST NOT map user text to a shell expression.
type Operation struct {
	Name        string
	Program     string
	BuildArgs   func(json.RawMessage) ([]string, error)
	Timeout     time.Duration
	WorkDir     string
	Environment []string
	// Installed by trusted Agent profiles only. Supplementary groups are
	// dropped; UID/GID never come from an API/tool operation input.
	Credential *syscall.Credential
}

type Options struct {
	Operations     []Operation
	MaxOutputBytes int
	MaxConcurrent  int
	DefaultTimeout time.Duration
}

type Output struct {
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

type Request struct {
	ActorID   string `json:"-"` // Trusted business/Job identity; never parsed from tool input.
	JobID     string `json:"-"` // Durable job identity for execution-time authorization.
	Operation string
	Input     json.RawMessage
	// Output is caller-owned and must remain open until Run returns. Delivery
	// is nonblocking; slow subscribers cannot stop stdout/stderr drainage.
	Output chan<- Output
}

type Result struct {
	StdoutBytes   int64           `json:"stdout_bytes,omitempty"`
	StderrBytes   int64           `json:"stderr_bytes,omitempty"`
	Data          json.RawMessage `json:"data,omitempty"`
	Operation     string          `json:"operation"`
	StartedAt     time.Time       `json:"started_at"`
	FinishedAt    time.Time       `json:"finished_at"`
	ExitCode      int             `json:"exit_code"`
	Stdout        string          `json:"stdout"`
	Stderr        string          `json:"stderr"`
	Truncated     bool            `json:"truncated"`
	DroppedEvents uint64          `json:"dropped_events"`
	Canceled      bool            `json:"canceled"`
	TimedOut      bool            `json:"timed_out"`
}

type Runner struct {
	operations map[string]Operation
	semaphore  chan struct{}
	limit      int
}

var operationName = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,79}$`)

func NewRunner(options Options) (*Runner, error) {
	if options.MaxOutputBytes == 0 {
		options.MaxOutputBytes = 1 << 20
	}
	if options.MaxConcurrent == 0 {
		options.MaxConcurrent = 4
	}
	if options.DefaultTimeout == 0 {
		options.DefaultTimeout = 5 * time.Minute
	}
	if options.MaxOutputBytes < 1 || options.MaxOutputBytes > 16<<20 || options.MaxConcurrent < 1 || options.MaxConcurrent > 128 || options.DefaultTimeout < time.Millisecond {
		return nil, errors.New("invalid runner limits")
	}
	r := &Runner{operations: make(map[string]Operation), semaphore: make(chan struct{}, options.MaxConcurrent), limit: options.MaxOutputBytes}
	for _, operation := range options.Operations {
		if !operationName.MatchString(operation.Name) || operation.BuildArgs == nil {
			return nil, errors.New("operation needs a valid name and typed argument builder")
		}
		if _, exists := r.operations[operation.Name]; exists {
			return nil, fmt.Errorf("duplicate operation %q", operation.Name)
		}
		if !filepath.IsAbs(operation.Program) {
			return nil, errors.New("program must be an absolute trusted path")
		}
		resolved, err := filepath.EvalSymlinks(operation.Program)
		if err != nil {
			return nil, fmt.Errorf("resolve program: %w", err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Mode()&0111 == 0 || info.Mode().Perm()&0002 != 0 {
			return nil, errors.New("program must be a non-world-writable executable regular file")
		}
		operation.Program = resolved
		if operation.Timeout == 0 {
			operation.Timeout = options.DefaultTimeout
		}
		if operation.Timeout < time.Millisecond {
			return nil, errors.New("operation timeout must be positive")
		}
		if operation.WorkDir != "" && !filepath.IsAbs(operation.WorkDir) {
			return nil, errors.New("working directory must be absolute")
		}
		for _, env := range operation.Environment {
			key, _, ok := strings.Cut(env, "=")
			if !ok || key == "" || strings.ContainsAny(env, "\x00\n\r") {
				return nil, errors.New("invalid configured environment")
			}
		}
		r.operations[operation.Name] = operation
	}
	return r, nil
}

func (r *Runner) HasOperation(name string) bool { _, ok := r.operations[name]; return ok }

// Validate resolves only a trusted argument builder; it never starts a process.
func (r *Runner) Validate(request Request) error {
	_, _, err := r.prepare(request)
	return err
}

func (r *Runner) prepare(request Request) (Operation, []string, error) {
	operation, ok := r.operations[request.Operation]
	if !ok {
		return Operation{}, nil, ErrUnauthorized
	}
	if len(request.Input) > 1<<20 {
		return Operation{}, nil, fmt.Errorf("%w: payload too large", ErrInvalidInput)
	}
	args, err := operation.BuildArgs(request.Input)
	if err != nil {
		return Operation{}, nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	total := 0
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) {
			return Operation{}, nil, fmt.Errorf("%w: NUL argument", ErrInvalidInput)
		}
		total += len(arg)
	}
	if len(args) > 1024 || total > 128<<10 {
		return Operation{}, nil, fmt.Errorf("%w: argument limit exceeded", ErrInvalidInput)
	}
	return operation, append([]string(nil), args...), nil
}

// IO belongs to trusted module code. Close must interrupt an in-progress read
// or write; the Runner uses it to release backpressure after cancellation.
type IO struct {
	Stdin  io.ReadCloser
	Stdout io.WriteCloser
	Stderr io.WriteCloser
}

func (r *Runner) Run(ctx context.Context, request Request) (Result, error) {
	return r.run(ctx, request, IO{})
}
func (r *Runner) Stream(ctx context.Context, request Request, streams IO) (Result, error) {
	return r.run(ctx, request, streams)
}
func (r *Runner) run(ctx context.Context, request Request, streams IO) (result Result, returnedError error) {
	result = Result{Operation: request.Operation, ExitCode: -1}
	defer func() {
		if result.FinishedAt.IsZero() {
			result.FinishedAt = time.Now().UTC()
		}
		result.Canceled = errors.Is(returnedError, context.Canceled)
		result.TimedOut = errors.Is(returnedError, context.DeadlineExceeded)
	}()
	operation, args, err := r.prepare(request)
	if err != nil {
		return result, err
	}
	select {
	case r.semaphore <- struct{}{}:
	case <-ctx.Done():
		return result, ctx.Err()
	}
	defer func() { <-r.semaphore }()
	callerContext := ctx
	ctx, cancel := context.WithTimeout(ctx, operation.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	cmd := exec.Command(operation.Program, args...)
	cmd.Dir = operation.WorkDir
	cmd.Env = append([]string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}, operation.Environment...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if operation.Credential != nil {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: operation.Credential.Uid, Gid: operation.Credential.Gid, Groups: []uint32{}}
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return result, err
	}
	defer stdoutRead.Close()
	defer stdoutWrite.Close()
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		return result, err
	}
	defer stderrRead.Close()
	defer stderrWrite.Close()
	var inputRead, inputWrite *os.File
	inputDone := make(chan struct{})
	var inputError error
	if streams.Stdin != nil {
		inputRead, inputWrite, err = os.Pipe()
		if err != nil {
			return result, err
		}
		defer inputRead.Close()
		defer inputWrite.Close()
		cmd.Stdin = inputRead
	} else {
		close(inputDone)
	}
	cmd.Stdout, cmd.Stderr = stdoutWrite, stderrWrite
	result.StartedAt = time.Now().UTC()
	if err = cmd.Start(); err != nil {
		result.FinishedAt = time.Now().UTC()
		return result, fmt.Errorf("start operation: %w", err)
	}
	stdoutWrite.Close()
	stderrWrite.Close()
	if streams.Stdin != nil {
		inputRead.Close()
		go func() {
			_, inputError = io.CopyBuffer(inputWrite, contextReader{ctx, streams.Stdin}, make([]byte, 32<<10))
			// Mark the request body consumed before delivering EOF to the child.
			// Closing a consumed HTTP body here can cancel the response context
			// while the child's final output is still draining.
			close(inputDone)
			inputWrite.Close()
		}()
	}
	capture := &boundedCapture{remaining: r.limit, output: request.Output, sinks: map[string]io.WriteCloser{"stdout": streams.Stdout, "stderr": streams.Stderr}, cancel: cancel}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); capture.read(stdoutRead, "stdout") }()
	go func() { defer readers.Done(); capture.read(stderrRead, "stderr") }()
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	var cause error
	select {
	case err = <-wait:
	case <-ctx.Done():
		cause = ctx.Err()
		if streams.Stdin != nil {
			streams.Stdin.Close()
		}
		if streams.Stdout != nil {
			streams.Stdout.Close()
		}
		if streams.Stderr != nil {
			streams.Stderr.Close()
		}
		// The entire process group is terminated, including children retaining
		// output descriptors. The operation is required to stay in its group.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-wait
	}
	// Also reap descendants when the direct child exits before them.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if streams.Stdin != nil {
		select {
		case <-inputDone:
		default:
			streams.Stdin.Close()
			inputWrite.Close()
			<-inputDone
		}
	}
	readDone := make(chan struct{})
	go func() { readers.Wait(); close(readDone) }()
	select {
	case <-readDone:
	case <-ctx.Done():
		if cause == nil {
			cause = ctx.Err()
		}
		if streams.Stdout != nil {
			streams.Stdout.Close()
		}
		if streams.Stderr != nil {
			streams.Stderr.Close()
		}
		stdoutRead.Close()
		stderrRead.Close()
		<-readDone
	}
	result.FinishedAt = time.Now().UTC()
	result.ExitCode = cmd.ProcessState.ExitCode()
	result.Stdout, result.Stderr, result.Truncated, result.DroppedEvents = capture.result()
	capture.mu.Lock()
	result.StdoutBytes, result.StderrBytes = capture.stdoutBytes, capture.stderrBytes
	streamError := capture.err
	capture.mu.Unlock()
	if streamError != nil && (cause == nil || (errors.Is(cause, context.Canceled) && callerContext.Err() == nil)) {
		return result, fmt.Errorf("operation stream failed: %w", streamError)
	}
	if cause != nil {
		result.Canceled, result.TimedOut = errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded)
		return result, cause
	}
	if inputError != nil {
		return result, fmt.Errorf("operation input stream failed: %w", inputError)
	}
	if err != nil {
		return result, fmt.Errorf("operation failed (exit %d): %w", result.ExitCode, err)
	}
	return result, nil
}

type boundedCapture struct {
	mu                       sync.Mutex
	remaining                int
	stdout, stderr           strings.Builder
	truncated                bool
	dropped                  uint64
	output                   chan<- Output
	sinks                    map[string]io.WriteCloser
	stdoutBytes, stderrBytes int64
	err                      error
	cancel                   context.CancelFunc
}

func (b *boundedCapture) read(reader io.Reader, stream string) {
	buffer := make([]byte, 16<<10)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			if sink := b.sinks[stream]; sink != nil {
				written, writeErr := sink.Write(buffer[:n])
				if writeErr == nil && written != n {
					writeErr = io.ErrShortWrite
				}
				b.mu.Lock()
				if stream == "stdout" {
					b.stdoutBytes += int64(written)
				} else {
					b.stderrBytes += int64(written)
				}
				if writeErr != nil && b.err == nil {
					b.err = writeErr
					b.cancel()
				}
				b.mu.Unlock()
				if writeErr != nil {
					return
				}
				if err != nil {
					return
				}
				continue
			}
			b.mu.Lock()
			if stream == "stdout" {
				b.stdoutBytes += int64(n)
			} else {
				b.stderrBytes += int64(n)
			}
			keep := n
			if keep > b.remaining {
				keep = b.remaining
				b.truncated = true
			}
			if stream == "stdout" {
				b.stdout.Write(buffer[:keep])
			} else {
				b.stderr.Write(buffer[:keep])
			}
			b.remaining -= keep
			if b.output != nil {
				select {
				case b.output <- Output{Stream: stream, Text: string(buffer[:n])}:
				default:
					b.dropped++
				}
			}
			b.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (b *boundedCapture) result() (string, string, bool, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stdout.String(), b.stderr.String(), b.truncated, b.dropped
}
