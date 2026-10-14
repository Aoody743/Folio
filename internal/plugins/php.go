package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

type PHPInput struct {
	ID               string         `json:"id"`
	Command          string         `json:"command"`
	ExpectedRevision int64          `json:"expected_revision,omitempty"`
	Values           map[string]any `json:"values,omitempty"`
	Extension        string         `json:"extension,omitempty"`
	Enabled          bool           `json:"enabled,omitempty"`
}

var phpFunction = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(,[A-Za-z_][A-Za-z0-9_]*)*$`)
var phpTimezone = regexp.MustCompile(`^[A-Za-z0-9_+./-]{1,64}$`)
var phpModuleName = regexp.MustCompile(`^[A-Za-z0-9_ -]{1,96}$`)
var approvedPHPExtensions = map[string]bool{"bcmath": true, "curl": true, "gd": true, "intl": true, "mbstring": true, "mysqli": true, "opcache": true, "pdo_mysql": true, "pdo_pgsql": true, "pgsql": true, "redis": true, "soap": true, "sqlite3": true, "xml": true, "zip": true, "imagick": true, "mongodb": true}

func phpConfigurationSchema() map[string]any {
	properties := map[string]any{}
	for _, key := range []string{"memory_limit_mb", "max_children", "max_execution_time", "upload_max_filesize_mb", "post_max_size_mb", "max_input_vars"} {
		properties[key] = map[string]any{"type": "integer", "minimum": 1, "maximum": 1048576}
	}
	properties["disable_functions"] = map[string]any{"type": "string", "maxLength": 1024, "pattern": `^(?:[A-Za-z_][A-Za-z0-9_]*(?:,[A-Za-z_][A-Za-z0-9_]*)*)?$`}
	properties["timezone"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 64, "pattern": phpTimezone.String()}
	properties["display_errors"] = map[string]any{"type": "boolean"}
	properties["session_handler"] = enumSchema("files")
	return obj(properties)
}

// PHP startup warnings are diagnostic text, not loaded modules. Keep -m's
// reported names, including Zend OPcache, and normalize only for comparisons.
func parsePHPModules(output string) []string {
	loaded := []string{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if phpModuleName.MatchString(line) && !slices.Contains(loaded, line) {
			loaded = append(loaded, line)
		}
	}
	slices.Sort(loaded)
	return loaded
}
func phpModuleSet(output string) map[string]bool {
	loaded := map[string]bool{}
	for _, line := range parsePHPModules(output) {
		name := strings.ToLower(line)
		if name == "zend opcache" {
			name = "opcache"
		}
		loaded[name] = true
	}
	return loaded
}

func renderPHPIni(config map[string]any, sessions string) (string, error) {
	if err := validateConfiguration("php", config); err != nil {
		return "", err
	}
	body := fmt.Sprintf("[PHP]\nexpose_php = Off\nmemory_limit = %dM\nmax_execution_time = %d\nupload_max_filesize = %dM\npost_max_size = %dM\nmax_input_vars = %d\ndisplay_errors = Off\nlog_errors = On\nallow_url_include = Off\nsession.save_handler = files\nsession.save_path = %q\n", setting(config, "memory_limit_mb", 128), setting(config, "max_execution_time", 30), setting(config, "upload_max_filesize_mb", 16), setting(config, "post_max_size_mb", 20), setting(config, "max_input_vars", 1000), sessions)
	if functions, ok := config["disable_functions"].(string); ok && functions != "" {
		if !phpFunction.MatchString(functions) {
			return "", failure("invalid_config", "Disabled functions must be a comma-separated list of PHP function names", 400)
		}
		body += "disable_functions = " + functions + "\n"
	}
	if timezone, ok := config["timezone"].(string); ok {
		if !phpTimezone.MatchString(timezone) || strings.Contains(timezone, "..") {
			return "", failure("invalid_config", "Invalid PHP timezone", 400)
		}
		if _, err := time.LoadLocation(timezone); err != nil {
			return "", failure("invalid_config", "Unknown PHP timezone", 400)
		}
		body += "date.timezone = " + timezone + "\n"
	}
	if display, ok := config["display_errors"].(bool); ok && display {
		body += "display_errors = On\n"
	}
	if handler, ok := config["session_handler"].(string); ok && handler != "files" {
		return "", failure("unsupported_session_handler", "Remote session handlers need a dedicated credential-bound connection; no arbitrary session path is accepted", 412)
	}
	if path, ok := config["session_path"].(string); ok && path != "" && path != sessions {
		return "", failure("invalid_config", "Session files must stay in the managed runtime data directory", 400)
	}
	return body, nil
}
func (e *PrivilegedExecutor) validatePHP(raw json.RawMessage) error {
	var input PHPInput
	if err := parse(raw, &input); err != nil {
		return err
	}
	if !validInstanceID(input.ID) {
		return failure("invalid_input", "Invalid PHP instance ID", 400)
	}
	switch input.Command {
	case "status", "extensions", "configuration", "guard_status":
	case "configure", "guard_configure":
		if input.ExpectedRevision < 1 {
			return failure("invalid_input", "expected_revision is required", 400)
		}
		if len(input.Values) > 32 {
			return failure("invalid_input", "Too many PHP configuration fields", 400)
		}
		if _, err := renderPHPIni(input.Values, "/controlled/sessions"); err != nil {
			return err
		}
	case "extension_toggle":
		if input.ExpectedRevision < 1 || !approvedPHPExtensions[input.Extension] {
			return failure("invalid_input", "Extension or revision is not approved", 400)
		}
	default:
		return failure("invalid_input", "Unknown PHP action", 400)
	}
	return nil
}
func phpCLI(instance Instance) (string, error) {
	candidates := []string{filepath.Join(filepath.Dir(instance.Service.Program), "php"), filepath.Join(instance.Paths.Root, "runtime", "bin", "php")}
	if instance.PluginID == "php-yum" {
		candidates = append([]string{filepath.Join(filepath.Dir(filepath.Dir(instance.Service.Program)), "bin", "php")}, candidates...)
	}
	if strings.HasPrefix(instance.Service.Program, "/usr/sbin/php-fpm") {
		version := strings.TrimPrefix(filepath.Base(instance.Service.Program), "php-fpm")
		if version != "" {
			candidates = append(candidates, "/usr/bin/php"+version)
		}
	}
	for _, name := range candidates {
		info, err := os.Stat(name)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0022 == 0 && info.Mode().Perm()&0111 != 0 {
			return name, nil
		}
	}
	return "", failure("runtime_unavailable", "Installed PHP runtime does not contain its matching CLI executable", 412)
}
func (e *PrivilegedExecutor) php(ctx context.Context, raw json.RawMessage) (any, error) {
	var input PHPInput
	_ = json.Unmarshal(raw, &input)
	unlock, err := e.lockContext(ctx, input.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if input.Command == "configure" || input.Command == "guard_configure" || input.Command == "extension_toggle" {
		if err := e.ensureInstallFinalized(ctx, input.ID); err != nil {
			return nil, err
		}
	}
	instance, err := e.get(ctx, input.ID, false)
	if err != nil {
		return nil, err
	}
	if !isPHPRuntime(instance.PluginID) {
		return nil, failure("runtime_type_mismatch", "An actual PHP runtime instance is required", 412)
	}
	switch input.Command {
	case "configuration", "guard_status":
		return map[string]any{"id": instance.ID, "revision": instance.Revision, "values": instance.Config, "pool_dir": filepath.Join(instance.Paths.ConfigDir, "pools"), "worker_isolation": "per-site pools supplied by protected sites Agent"}, nil
	case "status":
		program, err := phpCLI(instance)
		if err != nil {
			return nil, err
		}
		result, err := runFixedEnvironment(ctx, program, []string{"-c", filepath.Join(instance.Paths.ConfigDir, "php.ini"), "-v"}, 10*time.Second, e.phpCredential(), runtimeEnvironment(instance))
		if err != nil {
			return nil, err
		}
		return map[string]any{"id": instance.ID, "version": strings.TrimSpace(result), "health": e.probe(ctx, instance)}, nil
	case "extensions":
		program, err := phpCLI(instance)
		if err != nil {
			return nil, err
		}
		result, err := runFixedEnvironment(ctx, program, []string{"-c", filepath.Join(instance.Paths.ConfigDir, "php.ini"), "-m"}, 10*time.Second, e.phpCredential(), runtimeEnvironment(instance))
		if err != nil {
			return nil, err
		}
		return map[string]any{"items": parsePHPModules(result), "revision": instance.Revision}, nil
	case "configure", "guard_configure":
		if instance.Revision != input.ExpectedRevision {
			return nil, failure("revision_conflict", "PHP configuration changed", 412)
		}
		config := cloneConfig(instance.Config)
		for key, value := range input.Values {
			config[key] = value
		}
		if _, err = renderPHPIni(config, filepath.Join(instance.Paths.DataDir, "sessions")); err != nil {
			return nil, err
		}
		if err = e.takeSnapshot(ctx, instance); err != nil {
			return nil, err
		}
		old := instance
		instance.Config = config
		manifest, _ := e.catalog.Get(instance.PluginID)
		if err = e.configure(ctx, &instance, manifest); err != nil {
			e.restoreSnapshot(ctx, old)
			return nil, err
		}
		if _, err = e.controlLocked(ctx, ServiceInput{ID: instance.ID, Command: "check"}, instance); err != nil {
			e.restoreSnapshot(ctx, old)
			return nil, err
		}
		if _, err = e.controlLocked(ctx, ServiceInput{ID: instance.ID, Command: "reload"}, instance); err != nil {
			e.restoreSnapshot(ctx, old)
			return nil, err
		}
		instance.Revision++
		instance.UpdatedAt = time.Now().UTC()
		instance.Health = e.probe(ctx, instance)
		if err = e.save("receipts", instance.ID, instance); err != nil {
			return nil, err
		}
		return instance, nil
	case "extension_toggle":
		return e.togglePHPExtension(ctx, instance, input)
	}
	return nil, failure("invalid_input", "Unknown PHP action", 400)
}
func (e *PrivilegedExecutor) phpCredential() *syscall.Credential {
	if os.Geteuid() != 0 {
		return nil
	}
	return &syscall.Credential{Uid: e.uid, Gid: e.gid}
}
func renderPHPExtensions(states map[string]bool) (string, error) {
	names := []string{}
	for name, enabled := range states {
		if !approvedPHPExtensions[name] {
			return "", failure("extension_untrusted", "Stored module is not approved", 412)
		}
		if enabled {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	body := ""
	for _, name := range names {
		directive := "extension="
		if name == "opcache" {
			directive = "zend_extension="
		}
		body += directive + name + ".so\n"
	}
	return body, nil
}
func (e *PrivilegedExecutor) checkPHPExtensions(ctx context.Context, instance Instance) error {
	program, err := phpCLI(instance)
	if err != nil {
		return err
	}
	output, err := runFixedEnvironment(ctx, program, []string{"-c", filepath.Join(instance.Paths.ConfigDir, "php.ini"), "-m"}, 10*time.Second, e.phpCredential(), runtimeEnvironment(instance))
	if err != nil {
		return err
	}
	loaded := phpModuleSet(output)
	for name, enabled := range instance.Extensions {
		if enabled && !loaded[name] {
			return failure("extension_load_failed", "A selected PHP module did not load in this exact runtime; configuration was not accepted", 502)
		}
		if !enabled && loaded[name] {
			return failure("extension_disable_failed", "The requested PHP module remains loaded; configuration was not accepted", 412)
		}
	}
	return nil
}
func (e *PrivilegedExecutor) togglePHPExtension(ctx context.Context, instance Instance, input PHPInput) (any, error) {
	if instance.Revision != input.ExpectedRevision {
		return nil, failure("revision_conflict", "PHP configuration changed", 412)
	}
	if !input.Enabled {
		program, err := phpCLI(instance)
		if err != nil {
			return nil, err
		}
		output, err := runFixedEnvironment(ctx, program, []string{"-n", "-m"}, 10*time.Second, e.phpCredential(), runtimeEnvironment(instance))
		if err != nil {
			return nil, err
		}
		if phpModuleSet(output)[input.Extension] {
			return nil, failure("extension_builtin", "This PHP module is compiled into the selected runtime and cannot be disabled by an extension toggle", 412)
		}
	}
	if input.Enabled {
		program, err := phpCLI(instance)
		if err != nil {
			return nil, err
		}
		directory, err := runFixedEnvironment(ctx, program, []string{"-n", "-r", "echo PHP_EXTENSION_DIR;"}, 10*time.Second, e.phpCredential(), runtimeEnvironment(instance))
		if err != nil {
			return nil, err
		}
		directory = strings.TrimSpace(directory)
		if !filepath.IsAbs(directory) || strings.ContainsAny(directory, "\x00\r\n") {
			return nil, failure("extension_source_invalid", "Runtime module directory is invalid", 412)
		}
		module := filepath.Join(directory, input.Extension+".so")
		info, err := os.Stat(module)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
			return nil, failure("extension_not_installed", "Install the exact signature-verified module package for this PHP version first", 412)
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
			return nil, failure("extension_untrusted", "PHP module must belong to the trusted Agent identity", 412)
		}
	}
	if err := e.takeSnapshot(ctx, instance); err != nil {
		return nil, err
	}
	old := instance
	instance.Extensions = maps.Clone(instance.Extensions)
	if instance.Extensions == nil {
		instance.Extensions = map[string]bool{}
	}
	instance.Extensions[input.Extension] = input.Enabled
	manifest, _ := e.catalog.Get(instance.PluginID)
	if err := e.configure(ctx, &instance, manifest); err != nil {
		e.restoreSnapshot(ctx, old)
		return nil, err
	}
	if err := e.checkPHPExtensions(ctx, instance); err != nil {
		e.restoreSnapshot(ctx, old)
		return nil, err
	}
	if _, err := e.controlLocked(ctx, ServiceInput{ID: instance.ID, Command: "check"}, instance); err != nil {
		e.restoreSnapshot(ctx, old)
		return nil, err
	}
	if _, err := e.controlLocked(ctx, ServiceInput{ID: instance.ID, Command: "reload"}, instance); err != nil {
		e.restoreSnapshot(ctx, old)
		return nil, err
	}
	instance.Revision++
	instance.UpdatedAt = time.Now().UTC()
	if err := e.save("receipts", instance.ID, instance); err != nil {
		return nil, err
	}
	return map[string]any{"id": instance.ID, "extension": input.Extension, "enabled": input.Enabled, "revision": instance.Revision}, nil
}
func (m *Manager) phpBindings() []core.Binding {
	result := []core.Binding{}
	for _, spec := range []struct {
		name, command string
		read          bool
	}{{"php_status", "status", true}, {"php_extensions", "extensions", true}, {"php_configuration", "configuration", true}, {"php_configure", "configure", false}, {"php_guard_status", "guard_status", true}, {"php_guard_configure", "guard_configure", false}, {"php_extension_toggle", "extension_toggle", false}} {
		properties := map[string]any{"id": idSchema()}
		required := []string{"id"}
		if !spec.read {
			properties["expected_revision"] = revisionSchema()
			required = append(required, "expected_revision")
		}
		if strings.Contains(spec.command, "configure") {
			properties["values"] = phpConfigurationSchema()
			required = append(required, "values")
		}
		if spec.command == "extension_toggle" {
			names := []string{}
			for name := range approvedPHPExtensions {
				names = append(names, name)
			}
			slices.Sort(names)
			properties["extension"] = enumSchema(names...)
			properties["enabled"] = map[string]any{"type": "boolean"}
			required = append(required, "extension", "enabled")
		}
		command := spec.command
		read := spec.read
		scope := "php:manage"
		if read {
			scope = "php:read"
		}
		result = append(result, m.binding(spec.name, "Typed PHP runtime action with owned configuration and actual FPM validation", scope, read, obj(properties, required...), idResource, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			var input PHPInput
			if err := parse(raw, &input); err != nil {
				return core.ActionResult{}, err
			}
			input.Command = command
			forward, _ := json.Marshal(input)
			if read {
				var output any
				if err := m.agentJSON(ctx, "php_action", forward, &output); err != nil {
					return core.ActionResult{}, err
				}
				return core.ActionResult{Data: output, Resources: []string{InstanceResource(input.ID)}}, nil
			}
			job, err := m.deps.Tasks.Submit(ctx, actor, "php_action", forward, []string{InstanceResource(input.ID)})
			if err != nil {
				return core.ActionResult{}, err
			}
			id, _ := job["id"].(string)
			return core.ActionResult{Data: job, TaskID: id, Resources: []string{InstanceResource(input.ID)}}, nil
		}))
		result[len(result)-1].Action.Task = !read
	}
	return result
}
