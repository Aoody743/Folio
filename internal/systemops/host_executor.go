package systemops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

var HostOperationNames = []string{"system_services_list", "system_services_get", "system_change_plan", "system_change_apply", "system_change_confirm", "system_change_rollback", "system_change_get", "system_script_profiles", "system_script_run"}

// HostExecutor is instantiated in the independent Agent, never in the API.
// Every privileged path/program/profile comes from its local trusted policy.
type HostExecutor struct {
	config    HostConfig
	services  map[string]ServiceProfile
	kinds     map[string]bool
	runner    *agent.Runner
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	apiGID    int
	closed    bool
	lockFile  *os.File
	pendingID string
	options   HostOptions
	scripts   *ScriptExecutor
}

type HostOptions struct {
	// CLI composition bridges root-owned plugin receipts into service profiles.
	// This callback runs in the independent Agent, never against API metadata.
	ServiceProfiles             func(context.Context) ([]ServiceProfile, error)
	UnsafeSameScriptUIDForTests bool
}

// ValidateHostConfig is a side-effect-free deployment check. It never opens
// the executor, recovers transactions, runs commands or changes host settings.
func ValidateHostConfig(config HostConfig) error {
	return validateHostConfig(config, false)
}
func validateHostConfig(config HostConfig, allowSameScriptUID bool) error {
	if config.Version != 1 || len(config.EnabledKinds) == 0 {
		return errors.New("explicit host policy version 1 and enabled kinds required")
	}
	if !filepath.IsAbs(config.StateDir) || config.StateDir == "/" || config.StateDir == "/tmp" || config.StateDir == "/workspace" {
		return errors.New("dedicated absolute host state directory required")
	}
	if err := trustedRootParents(config.StateDir); err != nil {
		return err
	}
	if info, err := os.Lstat(config.StateDir); err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return errors.New("host state must be a private real directory")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if config.RootPrefix != "" && (!filepath.IsAbs(config.RootPrefix) || config.RootPrefix == "/") {
		return errors.New("invalid trusted root prefix")
	}
	if config.ConfirmationSeconds < 0 || config.ConfirmationSeconds > 600 {
		return errors.New("invalid confirmation window")
	}
	needed := map[string]bool{}
	for _, kind := range config.EnabledKinds {
		switch kind {
		case "service_state":
			needed["systemctl"] = true
		case "ssh_config":
			needed["systemctl"] = true
			needed["sshd"] = true
		case "firewall_rules":
			needed["nft"] = true
		case "timezone", "ntp":
			needed["timedatectl"] = true
		case "power":
			needed["shutdown"] = true
		case "panel_config":
			needed["systemctl"] = true
			if _, err := user.Lookup(config.APIUser); err != nil {
				return err
			}
		case "script_execution":
			if err := validateScriptProfiles(config, allowSameScriptUID); err != nil {
				return err
			}
		default:
			return errors.New("unsupported host kind")
		}
	}
	for name := range needed {
		if err := trustedProgram(config.Programs[name]); err != nil {
			return err
		}
	}
	ids := map[string]bool{}
	for _, profile := range config.Services {
		if !identifier.MatchString(profile.ID) || !unitName.MatchString(profile.Unit) || ids[profile.ID] || (profile.Ownership != "managed" && profile.Ownership != "adopted") {
			return errors.New("invalid trusted service profile")
		}
		ids[profile.ID] = true
		if profile.UnitSHA256 != "" {
			decoded, err := hex.DecodeString(profile.UnitSHA256)
			if err != nil || len(decoded) != 32 {
				return errors.New("invalid trusted unit digest")
			}
		}
	}
	for _, port := range config.ProtectedPorts {
		if port < 1 || port > 65535 {
			return errors.New("invalid protected port")
		}
	}
	return nil
}

func NewHostExecutor(config HostConfig) (*HostExecutor, error) {
	return NewHostExecutorWithOptions(config, HostOptions{})
}
func NewHostExecutorWithOptions(config HostConfig, options HostOptions) (*HostExecutor, error) {
	if err := validateHostConfig(config, options.UnsafeSameScriptUIDForTests); err != nil {
		return nil, err
	}
	if config.Version != 1 || len(config.EnabledKinds) == 0 {
		return nil, errors.New("explicit system policy version 1 and kinds required")
	}
	if err := privateDir(config.StateDir); err != nil {
		return nil, err
	}
	if config.RootPrefix != "" && (!filepath.IsAbs(config.RootPrefix) || config.RootPrefix == "/") {
		return nil, errors.New("invalid trusted root prefix")
	}
	if config.ConfirmationSeconds == 0 {
		config.ConfirmationSeconds = 120
	}
	if config.ConfirmationSeconds < 1 || config.ConfirmationSeconds > 600 {
		return nil, errors.New("connection confirmation window must be 1..600 seconds")
	}
	if len(config.ProtectedPorts) == 0 {
		config.ProtectedPorts = []int{22, 80, 443, 9000}
	}
	for _, port := range config.ProtectedPorts {
		if port < 1 || port > 65535 {
			return nil, errors.New("invalid protected port")
		}
	}
	e := &HostExecutor{config: config, services: map[string]ServiceProfile{}, kinds: map[string]bool{}, options: options}
	for _, kind := range config.EnabledKinds {
		switch kind {
		case "service_state", "ssh_config", "firewall_rules", "timezone", "ntp", "power", "panel_config", "script_execution":
			e.kinds[kind] = true
		default:
			return nil, fmt.Errorf("unsupported system kind %q", kind)
		}
	}
	if e.kinds["panel_config"] {
		record, err := user.Lookup(config.APIUser)
		if err != nil {
			return nil, err
		}
		gid, err := strconv.Atoi(record.Gid)
		if err != nil {
			return nil, err
		}
		e.apiGID = gid
	}
	for _, profile := range config.Services {
		if !identifier.MatchString(profile.ID) || !unitName.MatchString(profile.Unit) || (profile.Ownership != "managed" && profile.Ownership != "adopted") {
			return nil, errors.New("trusted services need unique IDs, known unit and explicit ownership")
		}
		if _, exists := e.services[profile.ID]; exists {
			return nil, errors.New("duplicate service ID")
		}
		e.services[profile.ID] = profile
	}
	needed := map[string]bool{}
	if e.kinds["service_state"] || e.kinds["ssh_config"] || e.kinds["panel_config"] {
		needed["systemctl"] = true
	}
	if e.kinds["ssh_config"] {
		needed["sshd"] = true
	}
	if e.kinds["firewall_rules"] {
		needed["nft"] = true
	}
	if e.kinds["timezone"] || e.kinds["ntp"] {
		needed["timedatectl"] = true
	}
	if e.kinds["power"] {
		needed["shutdown"] = true
	}
	var operations []agent.Operation
	for program := range needed {
		path := config.Programs[program]
		if err := trustedProgram(path); err != nil {
			return nil, err
		}
		operations = append(operations, agent.Operation{Name: "systemops.program." + program, Program: path, Timeout: 30 * time.Second, BuildArgs: func(input json.RawMessage) ([]string, error) {
			var args []string
			if err := json.Unmarshal(input, &args); err != nil {
				return nil, err
			}
			return args, nil
		}})
	}
	runner, err := agent.NewRunner(agent.Options{Operations: operations, MaxConcurrent: 4, MaxOutputBytes: 1 << 20})
	if err != nil {
		return nil, err
	}
	e.runner = runner
	if e.kinds["script_execution"] {
		e.scripts, err = newScriptExecutor(config, options.UnsafeSameScriptUIDForTests)
		if err != nil {
			return nil, err
		}
	}
	lockPath := filepath.Join(config.StateDir, "executor.lock")
	fd, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	e.lockFile = os.NewFile(uintptr(fd), lockPath)
	info, err := e.lockFile.Stat()
	if err != nil {
		e.lockFile.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		e.lockFile.Close()
		return nil, errors.New("invalid host executor lock")
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		e.lockFile.Close()
		return nil, errors.New("another host executor owns this state")
	}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	if err = e.refreshProfiles(e.ctx); err != nil {
		e.Close()
		return nil, err
	}
	if err = e.restoreFirewallBoot(e.ctx); err != nil {
		e.Close()
		return nil, err
	}
	if err = e.recover(); err != nil {
		e.Close()
		return nil, err
	}
	return e, nil
}
func (e *HostExecutor) Close() error {
	e.mu.Lock()
	e.closed = true
	e.cancel()
	e.mu.Unlock()
	if e.scripts != nil {
		e.scripts.Close()
	}
	e.wg.Wait()
	e.mu.Lock()
	if e.lockFile != nil {
		_ = syscall.Flock(int(e.lockFile.Fd()), syscall.LOCK_UN)
		_ = e.lockFile.Close()
		e.lockFile = nil
	}
	e.mu.Unlock()
	return nil
}
func (e *HostExecutor) path(name string) string {
	if e.config.RootPrefix != "" {
		return filepath.Join(e.config.RootPrefix, strings.TrimPrefix(name, "/"))
	}
	return name
}
func (e *HostExecutor) command(ctx context.Context, name string, args ...string) (agent.Result, error) {
	raw, _ := json.Marshal(args)
	return e.runner.Run(ctx, agent.Request{Operation: "systemops.program." + name, Input: raw})
}
func (e *HostExecutor) Validate(request agent.Request) error {
	if strings.HasPrefix(request.Operation, "system_script_") {
		if e.scripts == nil {
			return agent.ErrUnauthorized
		}
		return e.scripts.Validate(request)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("host executor is closed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.refreshProfiles(ctx); err != nil {
		return err
	}
	return e.validateRequest(request)
}
func (e *HostExecutor) refreshProfiles(ctx context.Context) error {
	if e.options.ServiceProfiles == nil {
		return nil
	}
	profiles, err := e.options.ServiceProfiles(ctx)
	if err != nil {
		return err
	}
	all := map[string]ServiceProfile{}
	for _, profile := range e.config.Services {
		all[profile.ID] = profile
	}
	for _, profile := range profiles {
		if !identifier.MatchString(profile.ID) || !unitName.MatchString(profile.Unit) || profile.Ownership != "managed" || !strings.HasPrefix(profile.Unit, "powerlinux-plugin-") {
			return errors.New("invalid receipt service profile")
		}
		if _, exists := all[profile.ID]; exists {
			return errors.New("receipt service profile conflicts with trusted static policy")
		}
		all[profile.ID] = profile
	}
	e.services = all
	return nil
}
func (e *HostExecutor) validateRequest(request agent.Request) error {
	known := false
	for _, name := range HostOperationNames {
		if name == request.Operation {
			known = true
			break
		}
	}
	if !known {
		return agent.ErrUnauthorized
	}
	var input hostRequest
	if err := strictJSON(request.Input, &input); err != nil {
		return fmt.Errorf("%w: %v", agent.ErrInvalidInput, err)
	}
	if input.ActorID == "" || len(input.ActorID) > 128 {
		return agent.ErrInvalidInput
	}
	switch request.Operation {
	case "system_services_list":
		if !e.kinds["service_state"] || input.ID != "" || input.Change != nil || input.ServiceID != "" {
			return agent.ErrInvalidInput
		}
	case "system_services_get":
		if !e.kinds["service_state"] || input.ID != "" || input.Change != nil || !identifier.MatchString(input.ServiceID) {
			return agent.ErrInvalidInput
		}
		if _, ok := e.services[input.ServiceID]; !ok {
			return agent.ErrUnauthorized
		}
	case "system_change_plan":
		if input.Change == nil || input.ID != "" || input.ServiceID != "" {
			return agent.ErrInvalidInput
		}
		return e.validateChange(*input.Change)
	default:
		if !identifier.MatchString(input.ID) || input.Change != nil || input.ServiceID != "" {
			return agent.ErrInvalidInput
		}
	}
	return nil
}
func (e *HostExecutor) Run(ctx context.Context, request agent.Request) (result agent.Result, err error) {
	if strings.HasPrefix(request.Operation, "system_script_") {
		if e.scripts == nil {
			return agent.Result{}, agent.ErrUnauthorized
		}
		return e.scripts.Run(ctx, request)
	}
	result.Operation = request.Operation
	result.StartedAt = time.Now().UTC()
	result.ExitCode = -1
	defer func() {
		result.FinishedAt = time.Now().UTC()
		if err == nil {
			result.ExitCode = 0
		}
		result.Canceled = errors.Is(err, context.Canceled)
		result.TimedOut = errors.Is(err, context.DeadlineExceeded)
	}()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return result, errors.New("host executor is closed")
	}
	if err = e.refreshProfiles(ctx); err != nil {
		return result, err
	}
	if err = e.validateRequest(request); err != nil {
		return result, err
	}
	var input hostRequest
	_ = strictJSON(request.Input, &input)
	var data any
	switch request.Operation {
	case "system_services_list":
		all := []ServiceState{}
		ids := make([]string, 0, len(e.services))
		for id := range e.services {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			profile := e.services[id]
			var state ServiceState
			state, err = e.serviceState(ctx, profile)
			if err != nil {
				return result, err
			}
			all = append(all, state)
		}
		data = all
	case "system_services_get":
		data, err = e.serviceState(ctx, e.services[input.ServiceID])
	case "system_change_plan":
		data, err = e.plan(ctx, input.ActorID, *input.Change)
	default:
		var plan storedPlan
		plan, err = e.load(input.ID)
		if err != nil {
			return result, err
		}
		if plan.ActorID != input.ActorID {
			return result, agent.ErrUnauthorized
		}
		switch request.Operation {
		case "system_change_get":
			data = plan.HostPlan
		case "system_change_apply":
			data, err = e.apply(ctx, &plan)
		case "system_change_confirm":
			data, err = e.confirm(ctx, &plan)
		case "system_change_rollback":
			data, err = e.rollback(ctx, &plan)
		}
	}
	if err == nil {
		var raw []byte
		raw, err = json.Marshal(data)
		result.Stdout = string(raw)
	}
	return result, err
}
func (e *HostExecutor) Stream(ctx context.Context, request agent.Request, streams agent.IO) (agent.Result, error) {
	if e.scripts == nil {
		return agent.Result{}, agent.ErrUnauthorized
	}
	return e.scripts.Stream(ctx, request, streams)
}
func (e *HostExecutor) serviceState(ctx context.Context, profile ServiceProfile) (ServiceState, error) {
	result, err := e.command(ctx, "systemctl", "show", profile.Unit, "--property=ActiveState,UnitFileState,FragmentPath", "--no-pager")
	if err != nil {
		return ServiceState{}, err
	}
	state := ServiceState{ServiceProfile: profile}
	for _, line := range strings.Split(result.Stdout, "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch key {
		case "ActiveState":
			state.ActiveState = value
		case "UnitFileState":
			state.UnitFileState = value
		case "FragmentPath":
			state.FragmentPath = value
		}
	}
	if state.ActiveState == "" || state.FragmentPath == "" {
		return ServiceState{}, errors.New("configured service is not installed")
	}
	if profile.Ownership == "managed" && !strings.HasPrefix(profile.Unit, "powerlinux-plugin-") {
		return ServiceState{}, errors.New("managed service must use owned unit namespace")
	}
	if profile.UnitSHA256 != "" {
		path, err := filepath.EvalSymlinks(state.FragmentPath)
		if err != nil {
			return ServiceState{}, err
		}
		if err = trustedRootParents(path); err != nil {
			return ServiceState{}, err
		}
		fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
		if err != nil {
			return ServiceState{}, err
		}
		file := os.NewFile(uintptr(fd), path)
		info, statErr := file.Stat()
		if statErr != nil {
			file.Close()
			return ServiceState{}, statErr
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > 1<<20 {
			file.Close()
			return ServiceState{}, errors.New("managed service unit is not trusted")
		}
		value, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
		file.Close()
		if err != nil {
			return ServiceState{}, err
		}
		if hashBytes(value) != profile.UnitSHA256 {
			return ServiceState{}, ErrPlanConflict
		}
	}
	return state, nil
}
func (e *HostExecutor) plan(ctx context.Context, actor string, change Change) (HostPlan, error) {
	before, err := e.capture(ctx, change)
	if err != nil {
		return HostPlan{}, err
	}
	if err = e.preflight(ctx, change, before); err != nil {
		return HostPlan{}, err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return HostPlan{}, err
	}
	now := time.Now().UTC()
	plan := storedPlan{HostPlan: HostPlan{ID: "chg_" + hex.EncodeToString(nonce[:]), ActorID: actor, Change: change, State: "planned", CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute)}, Before: before}
	if err = e.save(plan); err != nil {
		return HostPlan{}, err
	}
	return plan.HostPlan, nil
}
func (e *HostExecutor) save(plan storedPlan) error {
	raw, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(e.config.StateDir, plan.ID+".json"), raw)
}
func (e *HostExecutor) load(id string) (storedPlan, error) {
	if !identifier.MatchString(id) {
		return storedPlan{}, agent.ErrInvalidInput
	}
	path := filepath.Join(e.config.StateDir, id+".json")
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return storedPlan{}, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return storedPlan{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4<<20 {
		return storedPlan{}, errors.New("invalid private plan file")
	}
	var plan storedPlan
	decoder := json.NewDecoder(io.LimitReader(file, 4<<20+1))
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&plan)
	if err == nil && (plan.ID != id || plan.ActorID == "" || !validPlanSnapshot(plan.Change, plan.Before) || (plan.After != nil && !validPlanSnapshot(plan.Change, *plan.After))) {
		err = errors.New("private plan identity or change is invalid")
	}
	if err == nil && decoder.Decode(new(any)) != io.EOF {
		err = errors.New("one private plan document required")
	}
	return plan, err
}
func validPlanSnapshot(change Change, value snapshot) bool {
	switch change.Kind {
	case "ssh_config", "panel_config":
		return value.Hash == hashBytes(value.File)
	case "service_state", "firewall_rules", "timezone", "ntp", "power":
		return value.Hash == hashBytes([]byte(value.Value))
	default:
		return false
	}
}

// Unconfirmed connectivity changes exclude further host mutations. A later
// successful operation must never be undone by an older protection timer.
func (e *HostExecutor) pendingConflict(id string) error {
	if e.pendingID != "" && e.pendingID != id {
		return ErrConfirmation
	}
	return nil
}
func (e *HostExecutor) apply(ctx context.Context, plan *storedPlan) (HostPlan, error) {
	if plan.State != "planned" {
		return HostPlan{}, errors.New("plan is not in planned state")
	}
	if time.Now().After(plan.ExpiresAt) {
		return HostPlan{}, ErrPlanExpired
	}
	if err := e.pendingConflict(plan.ID); err != nil {
		return HostPlan{}, err
	}
	current, err := e.capture(ctx, plan.Change)
	if err != nil {
		return HostPlan{}, err
	}
	if current.Hash != plan.Before.Hash {
		return HostPlan{}, ErrPlanConflict
	}
	if err = e.preflight(ctx, plan.Change, current); err != nil {
		return HostPlan{}, err
	}
	plan.State = "applying"
	if err = e.save(*plan); err != nil {
		return HostPlan{}, err
	}
	e.pendingID = plan.ID
	if e.connectionSensitive(plan.Change) {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
		defer cancel()
	}
	if err = e.commit(ctx, plan.Change, current); err == nil {
		err = e.verify(ctx, plan.Change)
	}
	if err == nil {
		var after snapshot
		after, err = e.capture(ctx, plan.Change)
		if err == nil {
			plan.After = &after
		}
	}
	if err != nil {
		original := err
		recovery, recoverCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer recoverCancel()
		if restoreErr := e.restore(recovery, plan.Change, plan.Before); restoreErr != nil {
			plan.State = "recovery_required"
			plan.Error = "apply failed and recovery needs operator attention"
			_ = e.save(*plan)
			return plan.HostPlan, fmt.Errorf("%w; recovery failed: %v", original, restoreErr)
		}
		plan.State = "rolled_back"
		plan.Error = "apply verification failed; previous state restored"
		_ = e.save(*plan)
		e.pendingID = ""
		return plan.HostPlan, original
	}
	if e.connectionSensitive(plan.Change) {
		deadline := time.Now().UTC().Add(time.Duration(e.config.ConfirmationSeconds) * time.Second)
		plan.ConfirmationDeadline = &deadline
		plan.State = "awaiting_confirmation"
	} else {
		plan.State = "applied"
	}
	if err = e.save(*plan); err != nil {
		return HostPlan{}, err
	}
	if plan.State == "awaiting_confirmation" {
		e.arm(*plan)
	} else {
		e.pendingID = ""
	}
	return plan.HostPlan, nil
}
func (e *HostExecutor) connectionSensitive(change Change) bool {
	return change.Kind == "ssh_config" || change.Kind == "firewall_rules" || change.Kind == "panel_config" || (change.Kind == "service_state" && e.services[change.ServiceID].Protected)
}
func (e *HostExecutor) confirm(ctx context.Context, plan *storedPlan) (HostPlan, error) {
	if plan.State != "awaiting_confirmation" || plan.ConfirmationDeadline == nil {
		return HostPlan{}, ErrConfirmation
	}
	if time.Now().After(*plan.ConfirmationDeadline) {
		_, _ = e.rollback(ctx, plan)
		return HostPlan{}, ErrPlanExpired
	}
	if err := e.verify(ctx, plan.Change); err != nil {
		return HostPlan{}, err
	}
	plan.State = "applied"
	plan.ConfirmationDeadline = nil
	err := e.save(*plan)
	if err == nil && e.pendingID == plan.ID {
		e.pendingID = ""
	}
	return plan.HostPlan, err
}
func (e *HostExecutor) rollback(ctx context.Context, plan *storedPlan) (HostPlan, error) {
	if plan.State != "applied" && plan.State != "awaiting_confirmation" && plan.State != "applying" && plan.State != "recovery_required" {
		return HostPlan{}, errors.New("plan is not restorable")
	}
	if err := e.validateChange(plan.Change); err != nil {
		return HostPlan{}, err
	}
	if plan.After != nil {
		current, err := e.capture(ctx, plan.Change)
		if err != nil {
			return HostPlan{}, err
		}
		if current.Hash != plan.After.Hash {
			plan.State = "recovery_required"
			plan.Error = "host changed after apply; automatic restore refused"
			_ = e.save(*plan)
			return plan.HostPlan, ErrPlanConflict
		}
	}
	if err := e.restore(ctx, plan.Change, plan.Before); err != nil {
		plan.State = "recovery_required"
		_ = e.save(*plan)
		return plan.HostPlan, err
	}
	plan.State = "rolled_back"
	plan.ConfirmationDeadline = nil
	err := e.save(*plan)
	if err == nil && e.pendingID == plan.ID {
		e.pendingID = ""
	}
	return plan.HostPlan, err
}
func (e *HostExecutor) arm(plan storedPlan) {
	if e.closed {
		return
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		delay := time.Until(*plan.ConfirmationDeadline)
		timer := time.NewTimer(max(delay, 0))
		defer timer.Stop()
		select {
		case <-e.ctx.Done():
			return
		case <-timer.C:
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		current, err := e.load(plan.ID)
		if err == nil && current.State == "awaiting_confirmation" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = e.rollback(ctx, &current)
		}
	}()
}
func (e *HostExecutor) recover() error {
	entries, err := os.ReadDir(e.config.StateDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "chg_") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		plan, err := e.load(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return err
		}
		if plan.State == "applying" || plan.State == "recovery_required" || (plan.State == "awaiting_confirmation" && plan.ConfirmationDeadline != nil && time.Now().After(*plan.ConfirmationDeadline)) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, err = e.rollback(ctx, &plan)
			cancel()
			if err != nil {
				return fmt.Errorf("unfinished host change needs recovery: %w", err)
			}
		} else if plan.State == "awaiting_confirmation" && plan.ConfirmationDeadline != nil {
			if e.pendingID != "" {
				return errors.New("multiple unfinished connection changes require operator recovery")
			}
			e.pendingID = plan.ID
			e.arm(plan)
		}
	}
	return nil
}
