package systemops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

// Profiles are trusted root policy, never fields accepted by a scripts action.
type ScriptProfile struct {
	ID             string `json:"id"`
	User           string `json:"user"`
	Shell          string `json:"shell"`
	Home           string `json:"home"`
	WorkDir        string `json:"work_dir"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}
type ScriptProfileView struct {
	ID             string `json:"id"`
	User           string `json:"user"`
	Root           bool   `json:"root"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}
type scriptAgentInput struct {
	ActorID        string `json:"actor_id"`
	ProfileID      string `json:"profile_id,omitempty"`
	RootAuthorized bool   `json:"root_authorized,omitempty"`
}
type ScriptExecutor struct {
	views  map[string]ScriptProfileView
	runner *agent.Runner
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func validateScriptProfiles(config HostConfig, allowSameUID bool) error {
	if config.APIUser == "" || len(config.ScriptProfiles) < 1 || len(config.ScriptProfiles) > 16 {
		return errors.New("scripts require an API user and 1..16 explicit execution profiles")
	}
	api, err := user.Lookup(config.APIUser)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, profile := range config.ScriptProfiles {
		if !identifier.MatchString(profile.ID) || len(profile.ID) > 60 || seen[profile.ID] || profile.TimeoutSeconds < 0 || profile.TimeoutSeconds > 3600 {
			return errors.New("invalid or duplicate script profile")
		}
		seen[profile.ID] = true
		account, err := user.Lookup(profile.User)
		if err != nil {
			return errors.New("script execution account does not exist")
		}
		if account.Uid == api.Uid && !allowSameUID {
			return errors.New("script execution UID must differ from API UID")
		}
		if account.Uid == "0" && !config.EnableRootScripts {
			return errors.New("root script profiles require explicit root policy enablement")
		}
		if account.Uid != "0" && !allowSameUID && (account.Gid == "0" || account.Gid == api.Gid) {
			return errors.New("non-root script execution GID must be non-root and distinct from the API GID")
		}
		if os.Geteuid() != 0 && account.Uid != strconv.Itoa(os.Geteuid()) {
			return errors.New("ordinary Agent cannot change script execution identity")
		}
		if !filepath.IsAbs(profile.Shell) {
			return errors.New("script shell must be trusted absolute path")
		}
		resolved, err := filepath.EvalSymlinks(profile.Shell)
		if err != nil {
			return err
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
			return errors.New("script shell is not trusted")
		}
		if err = trustedRootParents(resolved); err != nil {
			return err
		}
		base := filepath.Base(profile.Shell)
		if base != "bash" && base != "sh" && base != "dash" {
			return errors.New("supported script shells are bash/sh/dash")
		}
		uid, err := strconv.ParseUint(account.Uid, 10, 32)
		if err != nil {
			return err
		}
		for _, path := range []string{profile.Home, profile.WorkDir} {
			if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
				return errors.New("script profile requires dedicated absolute home/work directory")
			}
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || uint64(stat.Uid) != uid || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
				return errors.New("script profile home/work directory is not privately owned by its execution account")
			}
		}
	}
	return nil
}
func newScriptExecutor(config HostConfig, allowSameUID bool) (*ScriptExecutor, error) {
	if err := validateScriptProfiles(config, allowSameUID); err != nil {
		return nil, err
	}
	e := &ScriptExecutor{views: map[string]ScriptProfileView{}}
	operations := []agent.Operation{}
	for _, profile := range config.ScriptProfiles {
		account, _ := user.Lookup(profile.User)
		uid, _ := strconv.ParseUint(account.Uid, 10, 32)
		gid, _ := strconv.ParseUint(account.Gid, 10, 32)
		timeout := profile.TimeoutSeconds
		if timeout == 0 {
			timeout = 300
		}
		args := []string{"-s", "--"}
		if filepath.Base(profile.Shell) == "bash" {
			args = []string{"--noprofile", "--norc", "-s", "--"}
		}
		operation := agent.Operation{Name: "systemops.script." + profile.ID, Program: profile.Shell, WorkDir: profile.WorkDir, Timeout: time.Duration(timeout) * time.Second, Environment: []string{"HOME=" + profile.Home, "USER=" + profile.User, "LOGNAME=" + profile.User}, BuildArgs: func(raw json.RawMessage) ([]string, error) {
			var empty struct{}
			if err := strictJSON(raw, &empty); err != nil {
				return nil, err
			}
			return args, nil
		}}
		if os.Geteuid() == 0 {
			operation.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}
		}
		operations = append(operations, operation)
		e.views[profile.ID] = ScriptProfileView{ID: profile.ID, User: profile.User, Root: uid == 0, TimeoutSeconds: timeout}
	}
	runner, err := agent.NewRunner(agent.Options{Operations: operations, MaxConcurrent: 4, MaxOutputBytes: 64 << 10})
	if err != nil {
		return nil, err
	}
	e.runner = runner
	e.ctx, e.cancel = context.WithCancel(context.Background())
	return e, nil
}
func (e *ScriptExecutor) Close() error {
	e.mu.Lock()
	e.closed = true
	e.cancel()
	e.mu.Unlock()
	e.wg.Wait()
	return nil
}
func (e *ScriptExecutor) Validate(request agent.Request) error {
	var input scriptAgentInput
	if err := strictJSON(request.Input, &input); err != nil {
		return agent.ErrInvalidInput
	}
	if input.ActorID == "" {
		return agent.ErrInvalidInput
	}
	switch request.Operation {
	case "system_script_profiles":
		if input.ProfileID != "" || input.RootAuthorized {
			return agent.ErrInvalidInput
		}
	case "system_script_run":
		profile, ok := e.views[input.ProfileID]
		if !ok {
			return agent.ErrUnauthorized
		}
		if profile.Root && !input.RootAuthorized {
			return agent.ErrUnauthorized
		}
	default:
		return agent.ErrUnauthorized
	}
	return nil
}
func (e *ScriptExecutor) Run(ctx context.Context, request agent.Request) (agent.Result, error) {
	if err := e.Validate(request); err != nil {
		return agent.Result{}, err
	}
	if request.Operation != "system_script_profiles" {
		return agent.Result{}, errors.New("script execution requires private stdin stream")
	}
	views := []ScriptProfileView{}
	for _, view := range e.views {
		views = append(views, view)
	}
	raw, _ := json.Marshal(views)
	now := time.Now().UTC()
	return agent.Result{Operation: request.Operation, Stdout: string(raw), ExitCode: 0, StartedAt: now, FinishedAt: now}, nil
}
func (e *ScriptExecutor) Stream(ctx context.Context, request agent.Request, streams agent.IO) (agent.Result, error) {
	if err := e.Validate(request); err != nil {
		return agent.Result{}, err
	}
	if request.Operation != "system_script_run" || streams.Stdin == nil {
		return agent.Result{}, agent.ErrInvalidInput
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return agent.Result{}, errors.New("script executor is closed")
	}
	e.wg.Add(1)
	e.mu.Unlock()
	defer e.wg.Done()
	var input scriptAgentInput
	_ = strictJSON(request.Input, &input)
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(e.ctx, cancel)
	defer stop()
	defer cancel()
	// Reject an oversized script before starting it, rather than execute a
	// truncated program. The body never enters argv/environment or task input.
	readCtx, cancelRead := context.WithTimeout(ctx, 5*time.Second)
	stopRead := context.AfterFunc(readCtx, func() { streams.Stdin.Close() })
	body, readErr := io.ReadAll(io.LimitReader(streams.Stdin, 32<<10+1))
	stopRead()
	if readCtx.Err() != nil {
		readErr = readCtx.Err()
	}
	cancelRead()
	if readErr != nil {
		_ = streams.Stdin.Close()
		return agent.Result{}, readErr
	}
	if !validScriptBody(string(body)) {
		_ = streams.Stdin.Close()
		return agent.Result{}, agent.ErrInvalidInput
	}
	// A successful Unix request EOF must remain open until the response is
	// drained: its interrupting Close sets a read deadline and cancels the HTTP
	// request. The handler owns closure after our final result is transmitted.
	defer func() {
		for i := range body {
			body[i] = 0
		}
	}()
	streams.Stdin = io.NopCloser(bytes.NewReader(body))
	request.Operation = "systemops.script." + input.ProfileID
	request.Input = json.RawMessage(`{}`)
	result, err := e.runner.Stream(ctx, request, streams)
	result.Operation = "system_script_run"
	return result, err
}

func scriptHasRootGrant(aScopes []string) bool {
	for _, scope := range aScopes {
		if scope == "*" || scope == "terminal:root" {
			return true
		}
	}
	return false
}
func validScriptBody(body string) bool {
	return len(body) > 0 && len(body) <= 32<<10 && !strings.ContainsRune(body, 0)
}
