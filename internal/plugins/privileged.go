package plugins

import (
	"context"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

// PrivilegedConfig is loaded from the root-owned Agent policy, never from an
// API action. Software paths, service identities and package privileges are
// fixed here; runtime actions select an existing instance and typed verb.
type PrivilegedConfig struct {
	RootDir              string                    `json:"root_dir"`
	StateDir             string                    `json:"state_dir"`
	RunUser              string                    `json:"run_user"`
	RunGroup             string                    `json:"run_group,omitempty"`
	APIUser              string                    `json:"api_user"`
	EnableSystemPackages bool                      `json:"enable_system_packages"`
	EnableSourceBuild    bool                      `json:"enable_source_build,omitempty"`
	BuildUser            string                    `json:"build_user,omitempty"`
	BuildGroup           string                    `json:"build_group,omitempty"`
	BuildRootDir         string                    `json:"build_root_dir,omitempty"`
	UseSystemd           bool                      `json:"use_systemd"`
	Programs             map[string]TrustedProgram `json:"programs,omitempty"`
}
type PrivilegedOptions struct {
	Catalog    *Catalog
	HTTPClient *http.Client
}
type PrivilegedExecutor struct {
	config                 PrivilegedConfig
	catalog                *Catalog
	artifacts              *artifactStore
	packages               *packageBackend
	unitDir                string
	receiptSaveHook        func(string, Instance) error
	installJournalSaveHook func(context.Context, installJournal, int64) error
	installProofHook       func(context.Context, installJournal) error
	installHealthHook      func(context.Context, Instance) Health
	installEnableHook      func(context.Context, *installJournal) error
	installControlHook     func(context.Context, ServiceInput, Instance) (map[string]any, error)
	installUnitCreateHook  func(string, string, []byte) error
	systemdReloadHook      func(context.Context) error
	uid, gid               uint32
	buildUID, buildGID     uint32
	buildSlot              chan struct{}
	mu                     sync.Mutex
	locks                  map[string]*instanceMutex
	healthMu               sync.Mutex
	healthCache            map[string]cachedHealth
	privateCipher          cipher.AEAD
	profileControl         func(context.Context, Instance, string) error              // private trusted test dependency
	utilityApplyHook       func(context.Context, Instance, utilityRecord, bool) error // private utility transaction test dependency
	utilitySaveHook        func(Instance, utilityRecord) error                        // private utility transaction test dependency
}

var validAccount = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}\$?$`)

func Operations() []string {
	return []string{"plugin_list", "plugin_get", "plugin_plan", "plugin_apply", "plugin_service_control", "plugin_autostart", "plugin_recovery_inspect", "plugin_recovery_action", "php_action", "utility_action", "docker_action", "backup_artifact", "database_action", "database_artifact", "database_import_artifact"}
}

// ValidatePrivilegedConfig performs read-only policy validation. In particular
// --check-policy must never create software directories or launch services.
func ValidatePrivilegedConfig(config PrivilegedConfig) error {
	if !filepath.IsAbs(config.RootDir) || !filepath.IsAbs(config.StateDir) || filepath.Clean(config.RootDir) == "/" || filepath.Clean(config.StateDir) == "/" || filepath.Clean(config.RootDir) == filepath.Clean(config.StateDir) {
		return errors.New("software and Agent state require distinct absolute non-root directories")
	}
	for _, name := range []string{config.RunUser, config.APIUser, config.RunGroup, config.BuildUser, config.BuildGroup} {
		if name != "" && !validAccount.MatchString(name) {
			return errors.New("service user/group name is invalid")
		}
	}
	if len(config.Programs) > 64 {
		return errors.New("at most 64 trusted program profiles are supported")
	}
	for id, profile := range config.Programs {
		if !utilityName.MatchString(id) || !filepath.IsAbs(profile.Program) || len(profile.Arguments) > 32 {
			return errors.New("invalid trusted program profile")
		}
		for _, arg := range profile.Arguments {
			if len(arg) > 4096 || strings.ContainsAny(arg, "\x00\r\n") {
				return errors.New("invalid trusted program argument")
			}
		}
		if _, err := validateTrustedProgramPath(profile.Program); err != nil {
			return err
		}
	}
	if config.RunUser == "" || config.APIUser == "" || config.RunUser == config.APIUser {
		return errors.New("software worker must differ from API identity")
	}
	worker, err := user.Lookup(config.RunUser)
	if err != nil {
		return errors.New("software worker user does not exist")
	}
	api, err := user.Lookup(config.APIUser)
	if err != nil {
		return errors.New("API user does not exist")
	}
	if worker.Uid == "0" || worker.Uid == api.Uid {
		return errors.New("software worker UID must be non-root and distinct from API UID")
	}
	runGID := worker.Gid
	if config.RunGroup != "" {
		group, lookupErr := user.LookupGroup(config.RunGroup)
		if lookupErr != nil {
			return errors.New("software worker group does not exist")
		}
		runGID = group.Gid
	}
	if _, err := strconv.ParseUint(runGID, 10, 32); err != nil || runGID == "0" || runGID == api.Gid {
		return errors.New("software worker effective GID must be non-root and distinct from API primary GID")
	}
	if err := validateSourceBuildConfig(config); err != nil {
		return err
	}
	for _, dir := range []string{config.RootDir, config.StateDir} {
		if os.Geteuid() == 0 {
			for parent := filepath.Dir(dir); ; parent = filepath.Dir(parent) {
				info, err := os.Lstat(parent)
				if err == nil {
					stat, ok := info.Sys().(*syscall.Stat_t)
					if !ok || stat.Uid != 0 || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0) {
						return errors.New("privileged software/state ancestors must be root owned and protected against rename")
					}
				} else if !os.IsNotExist(err) {
					return err
				}
				if parent == "/" {
					break
				}
			}
		}
		if _, err = os.Lstat(dir); err == nil {
			if err = realDirectory(dir); err != nil {
				return err
			}
			info, err := os.Stat(dir)
			if err != nil {
				return err
			}
			if info.Mode().Perm()&0022 != 0 {
				return errors.New("managed directories must not be group/world writable")
			}
			if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
				return errors.New("managed directories must belong to Agent UID")
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
func NewPrivilegedExecutor(config PrivilegedConfig) (*PrivilegedExecutor, error) {
	return NewPrivilegedExecutorWithOptions(config, PrivilegedOptions{})
}
func NewPrivilegedExecutorWithOptions(config PrivilegedConfig, options PrivilegedOptions) (*PrivilegedExecutor, error) {
	if err := ValidatePrivilegedConfig(config); err != nil {
		return nil, err
	}
	config.Programs = maps.Clone(config.Programs)
	for id, profile := range config.Programs {
		canonical, err := validateTrustedProgramPath(profile.Program)
		if err != nil {
			return nil, err
		}
		profile.Program = canonical
		config.Programs[id] = profile
	}
	if !filepath.IsAbs(config.RootDir) || !filepath.IsAbs(config.StateDir) || filepath.Clean(config.RootDir) == "/" || filepath.Clean(config.StateDir) == "/" {
		return nil, errors.New("software and private Agent state need explicit absolute non-root directories")
	}
	if config.RunUser == "" || config.APIUser == "" || config.RunUser == config.APIUser {
		return nil, errors.New("software worker user must differ from the API user")
	}
	worker, err := user.Lookup(config.RunUser)
	if err != nil {
		return nil, errors.New("software worker user does not exist")
	}
	uid64, err := strconv.ParseUint(worker.Uid, 10, 32)
	if err != nil || uid64 == 0 {
		return nil, errors.New("software worker must not run as root")
	}
	api, err := user.Lookup(config.APIUser)
	if err != nil {
		return nil, errors.New("API user does not exist")
	}
	if api.Uid == worker.Uid {
		return nil, errors.New("software worker UID must differ from API UID")
	}
	gid64, err := strconv.ParseUint(worker.Gid, 10, 32)
	if err != nil {
		return nil, err
	}
	if config.RunGroup != "" {
		group, err := user.LookupGroup(config.RunGroup)
		if err != nil {
			return nil, err
		}
		gid64, err = strconv.ParseUint(group.Gid, 10, 32)
		if err != nil {
			return nil, err
		}
	}
	apiGID, parseErr := strconv.ParseUint(api.Gid, 10, 32)
	if parseErr != nil || gid64 == 0 || gid64 == apiGID {
		return nil, errors.New("software worker effective GID must be non-root and distinct from API primary GID")
	}
	if os.Geteuid() != 0 && uid64 != uint64(os.Geteuid()) {
		return nil, errors.New("ordinary Agent cannot impersonate another software worker")
	}
	if config.RunGroup == "" {
		group, err := user.LookupGroupId(strconv.FormatUint(gid64, 10))
		if err != nil {
			return nil, errors.New("software worker primary group does not exist")
		}
		config.RunGroup = group.Name
	}
	for _, dir := range []string{config.RootDir, config.StateDir} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		if err = realDirectory(dir); err != nil {
			return nil, err
		}
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm()&0022 != 0 {
			return nil, errors.New("managed software directories must not be group/world writable")
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
			return nil, errors.New("Agent root directories must belong to its service UID")
		}
	}
	for _, part := range []string{"receipts", "plans", "snapshots", "locks", "artifacts", "private"} {
		if err = os.MkdirAll(filepath.Join(config.StateDir, part), 0700); err != nil {
			return nil, err
		}
		dir := filepath.Join(config.StateDir, part)
		if err = realDirectory(dir); err != nil {
			return nil, err
		}
		info, err := os.Stat(dir)
		if err != nil {
			return nil, err
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
			return nil, errors.New("Agent private state directory belongs to another UID")
		}
		if err = os.Chmod(dir, 0700); err != nil {
			return nil, err
		}
	}
	catalog := options.Catalog
	if catalog == nil {
		catalog, err = BuiltinCatalog()
		if err != nil {
			return nil, err
		}
	}
	artifacts, err := newArtifactStore(filepath.Join(config.StateDir, "artifacts"), options.HTTPClient)
	if err != nil {
		return nil, err
	}
	e := &PrivilegedExecutor{config: config, catalog: catalog, artifacts: artifacts, unitDir: "/etc/systemd/system", uid: uint32(uid64), gid: uint32(gid64), locks: map[string]*instanceMutex{}, healthCache: map[string]cachedHealth{}}
	e.packages = newPackageBackend(config.EnableSystemPackages, config.StateDir)
	e.privateCipher, err = loadPrivateCipher(config.StateDir)
	if err != nil {
		return nil, err
	}
	if err = e.prepareSourceBuild(); err != nil {
		return nil, err
	}
	if os.Geteuid() == 0 {
		if err = os.Chown(config.RootDir, 0, int(e.gid)); err != nil {
			return nil, err
		}
		if err = os.Chmod(config.RootDir, 0750); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err = e.migratePHPProfiles(ctx); err != nil {
			return nil, err
		}
	}
	return e, nil
}

func (e *PrivilegedExecutor) Validate(request agent.Request) error {
	if len(request.Input) > 64<<10 {
		return failure("invalid_input", "Software control input exceeds 64 KiB", 413)
	}
	switch request.Operation {
	case "plugin_list":
		var input struct{}
		return parse(request.Input, &input)
	case "plugin_get":
		var input IDInput
		if err := parse(request.Input, &input); err != nil {
			return err
		}
		if !validInstanceID(input.ID) {
			return failure("invalid_input", "Invalid software instance ID", 400)
		}
	case "plugin_plan":
		var input PlanInput
		if err := parse(request.Input, &input); err != nil {
			return err
		}
		if _, err := e.catalog.Get(input.PluginID); err != nil {
			return failure("not_found", "Unknown trusted plugin", 404)
		}
		if input.InstanceID != "" && !validID.MatchString(input.InstanceID) {
			return failure("invalid_input", "Invalid instance label", 400)
		}
		switch input.Operation {
		case "install", "upgrade", "config", "uninstall", "rollback":
		default:
			return failure("invalid_input", "Unknown software operation", 400)
		}
	case "plugin_apply":
		var input ApplyInput
		if err := parse(request.Input, &input); err != nil {
			return err
		}
		if !validPlanID(input.PlanID) || input.ExpectedRevision < 0 || len(input.IdempotencyKey) < 8 || len(input.IdempotencyKey) > 128 {
			return failure("invalid_input", "Invalid plan revision/idempotency parameters", 400)
		}
	case "plugin_recovery_inspect":
		var input installRecoveryInspectInput
		if err := parse(request.Input, &input); err != nil {
			return err
		}
		if !validInstanceID(input.ID) {
			return failure("invalid_input", "Invalid software instance ID", 400)
		}
	case "plugin_recovery_action":
		var input installRecoveryActionInput
		if err := parse(request.Input, &input); err != nil {
			return err
		}
		if !validInstanceID(input.ID) || input.ExpectedJournalRevision < 1 || (input.Command != "continue" && input.Command != "stop_preserve") {
			return failure("invalid_input", "Invalid install recovery command or journal revision", 400)
		}
	case "plugin_service_control":
		var input ServiceInput
		if err := parse(request.Input, &input); err != nil {
			return err
		}
		if !validInstanceID(input.ID) {
			return failure("invalid_input", "Invalid instance ID", 400)
		}
		switch input.Command {
		case "start", "stop", "restart", "reload", "check":
		default:
			return failure("invalid_input", "Unknown managed service verb", 400)
		}
	case "plugin_autostart":
		return e.validateAutostart(request.Input)
	case "php_action":
		return e.validatePHP(request.Input)
	case "utility_action":
		return e.validateUtility(request.Input)
	case "docker_action":
		return e.validateDocker(request.Input)
	case "database_action":
		return e.validateDatabase(request.Input)
	case "database_artifact":
		return e.validateDatabaseArtifact(request.Input)
	case "database_import_artifact":
		return validDatabaseImportStream(request.Input)
	case "backup_artifact":
		var input struct {
			ID            string `json:"id"`
			Filename      string `json:"filename"`
			Direction     string `json:"direction"`
			ExpectedBytes int64  `json:"expected_bytes,omitempty"`
		}
		if err := parse(request.Input, &input); err != nil {
			return err
		}
		if !validInstanceID(input.ID) || !artifactName.MatchString(input.Filename) || (input.Direction != "upload" && input.Direction != "download") {
			return failure("invalid_input", "Invalid owned backup stream descriptor", 400)
		}
		if input.ExpectedBytes < 0 || input.ExpectedBytes > maxExtractedBytes {
			return failure("invalid_input", "Backup artifact size is outside its bounds", 400)
		}
	default:
		return agent.ErrUnauthorized
	}
	return nil
}

func (e *PrivilegedExecutor) Run(ctx context.Context, request agent.Request) (result agent.Result, err error) {
	result = agent.Result{Operation: request.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	defer func() {
		result.FinishedAt = time.Now().UTC()
		if err != nil {
			var known *core.Error
			if !errors.As(err, &known) {
				known = &core.Error{Code: "software_operation_failed", Message: "Software operation failed; inspect its prerequisites and task", Status: 502}
			}
			raw, _ := json.Marshal(map[string]any{"code": known.Code, "message": known.Message, "status": known.Status, "details": known.Details})
			result.Stderr = string(raw)
			err = known
		} else {
			result.ExitCode = 0
		}
	}()
	if err = e.Validate(request); err != nil {
		return result, err
	}
	var output any
	switch request.Operation {
	case "plugin_list":
		output, err = e.list(ctx)
	case "plugin_get":
		var input IDInput
		_ = json.Unmarshal(request.Input, &input)
		output, err = e.get(ctx, input.ID, true)
	case "plugin_plan":
		var input PlanInput
		_ = json.Unmarshal(request.Input, &input)
		output, err = e.plan(ctx, input)
	case "plugin_apply":
		var input ApplyInput
		_ = json.Unmarshal(request.Input, &input)
		output, err = e.apply(ctx, input)
	case "plugin_recovery_inspect":
		var input installRecoveryInspectInput
		_ = json.Unmarshal(request.Input, &input)
		output, err = e.installRecoveryInspect(ctx, input.ID)
	case "plugin_recovery_action":
		var input installRecoveryActionInput
		_ = json.Unmarshal(request.Input, &input)
		output, err = e.installRecoveryAction(ctx, input)
	case "plugin_service_control":
		var input ServiceInput
		_ = json.Unmarshal(request.Input, &input)
		output, err = e.control(ctx, input)
	case "php_action":
		output, err = e.php(ctx, request.Input)
	case "plugin_autostart":
		output, err = e.autostart(ctx, request.Input)
	case "utility_action":
		output, err = e.utility(ctx, request.Input)
	case "docker_action":
		output, err = e.docker(ctx, request.Input)
	case "database_action":
		output, err = e.database(ctx, request.Input)
	case "backup_artifact", "database_artifact", "database_import_artifact":
		return result, failure("stream_required", "Backup artifacts must use the authenticated streaming transport", 400)
	}
	if err != nil {
		return result, err
	}
	raw, encodeErr := json.Marshal(output)
	if encodeErr != nil {
		return result, encodeErr
	}
	if len(raw) > 1<<20 {
		return result, failure("result_too_large", "Software Agent result exceeds one MiB", 413)
	}
	result.Stdout = string(raw)
	return result, nil
}

func randomPlanID() (string, error) {
	data := make([]byte, 24)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return "plan_" + hex.EncodeToString(data), nil
}
func validPlanID(id string) bool {
	if len(id) != 53 || id[:5] != "plan_" {
		return false
	}
	_, err := hex.DecodeString(id[5:])
	return err == nil
}
func (e *PrivilegedExecutor) lock(id string) func() {
	unlock, _ := e.lockContext(context.Background(), id)
	return unlock
}
func (e *PrivilegedExecutor) lockContext(ctx context.Context, id string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.runtimeLockHeld(ctx, id) {
		return func() {}, nil
	}
	e.mu.Lock()
	entry := e.locks[id]
	if entry == nil {
		entry = &instanceMutex{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		e.locks[id] = entry
	}
	entry.refs++
	e.mu.Unlock()
	select {
	case <-entry.token:
	case <-ctx.Done():
		e.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(e.locks, id)
		}
		e.mu.Unlock()
		return nil, ctx.Err()
	}
	unlock := func() {
		entry.token <- struct{}{}
		e.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(e.locks, id)
		}
		e.mu.Unlock()
	}
	if err := ctx.Err(); err != nil {
		unlock()
		return nil, err
	}
	// Every receipt writer and runtime mutation shares this process lock.
	// Recovery CAS must serialize with config, uninstall, service control and
	// boot changes even when independent Agent executors use the same state.
	processUnlock, err := e.installRecoveryLock(ctx, id)
	if err != nil {
		unlock()
		return nil, err
	}
	return func() {
		processUnlock()
		unlock()
	}, nil
}

type instanceMutex struct {
	token chan struct{}
	refs  int
}

func (e *PrivilegedExecutor) save(kind, id string, value any) error {
	if kind != "receipts" && kind != "plans" && kind != "private" {
		return errors.New("unknown private Agent record kind")
	}
	if !validInstanceID(id) && !validPlanID(id) {
		return errors.New("invalid Agent record identity")
	}
	dir := filepath.Join(e.config.StateDir, kind)
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if kind == "private" {
		raw, err = e.sealPrivate(id, raw)
		if err != nil {
			return err
		}
	}
	if len(raw) > 1<<20 {
		return failure("record_too_large", "Protected Agent record exceeds one MiB", 413)
	}
	file, err := os.CreateTemp(dir, ".record-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(raw)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temp, filepath.Join(dir, id+".json")); err != nil {
		return err
	}
	// A later durable install checkpoint must never outrun its receipt or plan.
	// Sync the directory entry as well as the record's contents before success.
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr = directory.Close()
	if err == nil {
		err = closeErr
	}
	return err
}
func (e *PrivilegedExecutor) load(kind, id string, value any) error {
	if !validInstanceID(id) && !validPlanID(id) {
		return failure("invalid_input", "Invalid Agent record ID", 400)
	}
	name := filepath.Join(e.config.StateDir, kind, id+".json")
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		return failure("not_found", "Managed software record does not exist", 404)
	}
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 || info.Mode().Perm()&0077 != 0 {
		return failure("ownership_invalid", "Managed receipt permissions or format are invalid", 412)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return failure("ownership_invalid", "Managed receipt must belong to the Agent service UID", 412)
	}
	data, err := io.ReadAll(io.LimitReader(file, (2<<20)+1))
	if err != nil {
		return err
	}
	legacy := false
	if kind == "private" {
		data, legacy, err = e.openPrivate(id, data)
		if err != nil {
			return err
		}
		defer clear(data)
	}
	if err = json.Unmarshal(data, value); err != nil {
		return failure("record_invalid", "Protected Agent record is corrupt", 412)
	}
	if legacy {
		return e.save(kind, id, value)
	}
	return nil
}

func (e *PrivilegedExecutor) instancePaths(plugin, label string) Paths {
	base := filepath.Join(e.config.RootDir, plugin, label)
	return Paths{Root: base, ConfigDir: filepath.Join(base, "config"), DataDir: filepath.Join(base, "data"), RunDir: filepath.Join(base, "run"), LogDir: filepath.Join(base, "logs"), BackupDir: filepath.Join(base, "backups")}
}
func (e *PrivilegedExecutor) get(ctx context.Context, id string, health bool) (Instance, error) {
	var instance Instance
	if err := e.load("receipts", id, &instance); err != nil {
		return instance, err
	}
	if instance.ID != id || instance.Paths != e.instancePaths(instance.PluginID, instance.InstanceID) || instance.Service.ID != id || instance.Service.Unit != "powerlinux-plugin-"+id+".service" {
		return instance, failure("ownership_invalid", "Managed receipt disagrees with trusted instance layout", 412)
	}
	if _, err := e.catalog.Get(instance.PluginID); err != nil {
		return instance, failure("ownership_invalid", "Managed receipt plugin is absent from the trusted catalog", 412)
	}
	if health {
		instance.Health = e.cachedHealth(ctx, instance)
	}
	return instance, nil
}
func (e *PrivilegedExecutor) list(ctx context.Context) ([]Instance, error) {
	entries, err := os.ReadDir(filepath.Join(e.config.StateDir, "receipts"))
	if err != nil {
		return nil, err
	}
	result := []Instance{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if len(result) >= 256 {
			return nil, failure("result_too_large", "Managed instance listing requires a smaller page", 413)
		}
		id := entry.Name()[:len(entry.Name())-5]
		instance, err := e.get(ctx, id, true)
		if err != nil {
			return nil, err
		}
		result = append(result, instance)
	}
	return result, nil
}
