package plugins

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

// These getters run in the privileged Agent. SQLite values owned by the API
// process are never used to choose a program, unit, or configuration path.
func (e *PrivilegedExecutor) ReadRuntimeTarget(ctx context.Context, id string) (RuntimeTarget, error) {
	instance, err := e.get(ctx, id, false)
	if err != nil {
		return RuntimeTarget{}, err
	}
	if instance.Status == "uninstalled" {
		return RuntimeTarget{}, failure("runtime_unavailable", "Software is uninstalled", 412)
	}
	group := e.config.RunGroup
	if group == "" {
		group = e.config.RunUser
	}
	target := RuntimeTarget{Instance: instance, Prefix: instance.Paths.Root, WorkerUser: e.config.RunUser, WorkerGroup: group}
	switch instance.PluginID {
	case "openresty":
		target.NginxConf = filepath.Join(instance.Paths.ConfigDir, "nginx.conf")
		target.SitesDir = filepath.Join(instance.Paths.ConfigDir, "sites")
	case "php", "php-apt", "php-yum":
		target.PHPIni = filepath.Join(instance.Paths.ConfigDir, "php.ini")
		cli, err := phpCLI(instance)
		if err != nil {
			return RuntimeTarget{}, err
		}
		target.PHPCLIProgram = cli
		target.PHPCLIConfig = target.PHPIni
		target.PoolDir = filepath.Join(instance.Paths.ConfigDir, "pools")
		target.FPMAddress = filepath.Join(instance.Paths.RunDir, "fpm.sock")
	default:
		return RuntimeTarget{}, failure("runtime_type_mismatch", "Instance is not a web or PHP runtime", 412)
	}
	return target, nil
}
func (e *PrivilegedExecutor) Check(ctx context.Context, id string) error {
	_, err := e.control(ctx, ServiceInput{ID: id, Command: "check"})
	return err
}
func (e *PrivilegedExecutor) Reload(ctx context.Context, id string) error {
	_, err := e.control(ctx, ServiceInput{ID: id, Command: "reload"})
	return err
}

// RecoverRuntime restores an already-running owned runtime's configuration;
// a stopped daemon remains stopped. Receipt "running" alone is insufficient
// after reboot, and recovery never adopts a foreign PID or starts a service.
func (e *PrivilegedExecutor) RecoverRuntime(ctx context.Context, id string) error {
	unlock, err := e.lockContext(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	instance, err := e.get(ctx, id, false)
	if err != nil {
		return err
	}
	if instance.Status == "uninstalled" || instance.Ownership != "managed" || !ownedUnit(instance) {
		return failure("runtime_unavailable", "Recovery requires an installed protected owned runtime", 412)
	}
	if e.config.UseSystemd {
		if err := checkOwnedUnit(instance); err != nil {
			return err
		}
	}
	if _, err := e.controlLocked(ctx, ServiceInput{ID: id, Command: "check"}, instance); err != nil {
		return err
	}
	if _, err := ownedProcessID(instance); core.ErrorCode(err) == "runtime_not_running" {
		return nil
	} else if err != nil {
		return err
	}
	_, err = e.controlLocked(ctx, ServiceInput{ID: id, Command: "reload"}, instance)
	return err
}
func (e *PrivilegedExecutor) ServiceProfiles(ctx context.Context) ([]ServiceProfile, error) {
	instances, err := e.list(ctx)
	if err != nil {
		return nil, err
	}
	profiles := []ServiceProfile{}
	for _, instance := range instances {
		if instance.Status != "uninstalled" && instance.Service.Program != "" && instance.Service.Kind != "cli" {
			profiles = append(profiles, instance.Service)
		}
	}
	return profiles, nil
}

func isPHPRuntime(plugin string) bool {
	return plugin == "php" || plugin == "php-apt" || plugin == "php-yum"
}
func runtimeEnvironment(instance Instance) []string {
	if isPHPRuntime(instance.PluginID) {
		return []string{"PHP_INI_SCAN_DIR=" + filepath.Join(instance.Paths.ConfigDir, "conf.d")}
	}
	return nil
}
func (e *PrivilegedExecutor) credential(instance Instance) *syscall.Credential {
	// Root masters open protected site configs/keys and drop worker privileges.
	if instance.PluginID == "openresty" || instance.PluginID == "docker" || instance.PluginID == "pureftp" || isPHPRuntime(instance.PluginID) || os.Geteuid() != 0 {
		return nil
	}
	return &syscall.Credential{Uid: e.uid, Gid: e.gid}
}
func (e *PrivilegedExecutor) workerDirs(instance Instance) error {
	if err := e.prepareInstanceDirectories(instance.Paths); err != nil {
		return err
	}
	for _, name := range []string{instance.Paths.DataDir, instance.Paths.RunDir, instance.Paths.LogDir} {
		if err := os.MkdirAll(name, 0700); err != nil {
			return err
		}
		if err := e.setRuntimeDirectory(name, 0700, e.uid, true); err != nil {
			return err
		}
	}
	return nil
}

func (e *PrivilegedExecutor) configure(ctx context.Context, instance *Instance, manifest Manifest) error {
	return e.configureWithInstallJournal(ctx, instance, manifest, "")
}

func (e *PrivilegedExecutor) configureWithInstallJournal(ctx context.Context, instance *Instance, manifest Manifest, installNonce string) error {
	if err := validateConfiguration(instance.PluginID, instance.Config); err != nil {
		return err
	}
	if err := e.workerDirs(*instance); err != nil {
		return err
	}
	group := e.config.RunGroup
	if group == "" {
		group = e.config.RunUser
	}
	bind, _ := instance.Config["bind"].(string)
	if bind == "" {
		bind = "127.0.0.1"
	}
	port := setting(instance.Config, "port", manifest.Service.DefaultPort)
	write := func(relative, body string) error {
		return writeAtomicRoot(instance.Paths.ConfigDir, relative, []byte(body), 0640, int(e.gid))
	}
	switch instance.PluginID {
	case "openresty":
		if err := os.MkdirAll(filepath.Join(instance.Paths.ConfigDir, "sites"), 0700); err != nil {
			return err
		}
		generation, err := newWebGeneration()
		if err != nil {
			return err
		}
		body := e.renderOpenRestyConfiguration(*instance, generation)
		if err := write("nginx.conf", body); err != nil {
			return err
		}
		if err := write("fastcgi_params", "fastcgi_param QUERY_STRING $query_string;\nfastcgi_param REQUEST_METHOD $request_method;\nfastcgi_param CONTENT_TYPE $content_type;\nfastcgi_param CONTENT_LENGTH $content_length;\nfastcgi_param SCRIPT_NAME $fastcgi_script_name;\nfastcgi_param REQUEST_URI $request_uri;\nfastcgi_param DOCUMENT_URI $document_uri;\nfastcgi_param DOCUMENT_ROOT $document_root;\nfastcgi_param SERVER_PROTOCOL $server_protocol;\nfastcgi_param REQUEST_SCHEME $scheme;\nfastcgi_param HTTPS $https if_not_empty;\nfastcgi_param GATEWAY_INTERFACE CGI/1.1;\nfastcgi_param SERVER_SOFTWARE nginx/$nginx_version;\nfastcgi_param REMOTE_ADDR $remote_addr;\nfastcgi_param REMOTE_PORT $remote_port;\nfastcgi_param SERVER_ADDR $server_addr;\nfastcgi_param SERVER_PORT $server_port;\nfastcgi_param SERVER_NAME $server_name;\nfastcgi_param REDIRECT_STATUS 200;\n"); err != nil {
			return err
		}
	case "php", "php-apt", "php-yum":
		poolDir := filepath.Join(instance.Paths.ConfigDir, "pools")
		if err := os.MkdirAll(poolDir, 0700); err != nil {
			return err
		}
		scanDir := filepath.Join(instance.Paths.ConfigDir, "conf.d")
		if err := os.MkdirAll(scanDir, 0750); err != nil {
			return err
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(scanDir, 0, int(e.gid)); err != nil {
				return err
			}
		}
		if err := os.Chmod(scanDir, 0750); err != nil {
			return err
		}
		sessions := filepath.Join(instance.Paths.DataDir, "sessions")
		if err := os.MkdirAll(sessions, 0700); err != nil {
			return err
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(sessions, int(e.uid), int(e.gid)); err != nil {
				return err
			}
		}
		ini, err := renderPHPIni(instance.Config, sessions)
		if err != nil {
			return err
		}
		modules, err := renderPHPExtensions(instance.Extensions)
		if err != nil {
			return err
		}
		ini += modules
		if err = write("php.ini", ini); err != nil {
			return err
		}
		fpm := fmt.Sprintf("[global]\npid = %s\nerror_log = %s\ninclude = %s\n", filepath.Join(instance.Paths.RunDir, "server.pid"), filepath.Join(instance.Paths.LogDir, "fpm-error.log"), filepath.Join(poolDir, "*.conf"))
		if err = write("php-fpm.conf", fpm); err != nil {
			return err
		}
		pool := renderDefaultPHPPool(*instance, e.config.RunUser, group, true)
		if err = write("pools/default.conf", pool); err != nil {
			return err
		}
	case "redis", "valkey":
		body := fmt.Sprintf("bind %s\nport %d\nprotected-mode yes\ndaemonize yes\npidfile %s\nlogfile %s\ndir %s\ndbfilename dump.rdb\nappendonly yes\nmaxmemory %dmb\nmaxclients %d\ndatabases %d\n", bind, port, redisQuote(filepath.Join(instance.Paths.RunDir, "server.pid")), redisQuote(filepath.Join(instance.Paths.LogDir, "server.log")), redisQuote(instance.Paths.DataDir), setting(instance.Config, "maxmemory_mb", 64), setting(instance.Config, "maxclients", 256), setting(instance.Config, "databases", 16))
		if err := write("service.conf", body); err != nil {
			return err
		}
	case "memcached":
		raw, _ := json.Marshal(instance.Config)
		if err := write("service.json", string(raw)); err != nil {
			return err
		}
	case "mysql", "mysql-community", "mariadb", "postgresql", "mongodb":
		if err := e.configureDatabase(ctx, instance); err != nil {
			return err
		}
	case "pureftp", "rsyncd", "supervisor", "docker":
		if err := e.configureUtility(ctx, instance); err != nil {
			return err
		}
	case "phpmyadmin", "pgadmin", "php-guard", "data_query", "backup_ftp", "acme_pandominassl_apply", "webssh":
		raw, _ := json.Marshal(instance.Config)
		if err := write("integration.json", string(raw)); err != nil {
			return err
		}
		instance.Status = "enabled"
	default:
		return failure("recipe_unavailable", "No reviewed native configuration recipe exists for this software", 412)
	}
	if e.config.UseSystemd && instance.Service.Program != "" && instance.Service.Kind != "cli" {
		if installNonce != "" {
			return e.writeInstallUnit(ctx, instance, installNonce)
		}
		return e.writeUnit(ctx, *instance)
	}
	return nil
}

func renderDefaultPHPPool(instance Instance, worker, group string, health bool) string {
	body := fmt.Sprintf("[powerlinux-default]\nuser = %s\ngroup = %s\nlisten = %s\nlisten.owner = %s\nlisten.group = %s\nlisten.mode = 0660\npm = ondemand\npm.max_children = %d\npm.process_idle_timeout = 10s\n", worker, group, filepath.Join(instance.Paths.RunDir, "fpm.sock"), worker, group, setting(instance.Config, "max_children", 5))
	if health {
		body += "ping.path = " + phpHealthPath + "\nping.response = " + phpHealthResponse + "\n"
	}
	return body + "clear_env = yes\ncatch_workers_output = yes\nsecurity.limit_extensions = .php\nphp_admin_value[open_basedir] = " + instance.Paths.DataDir + "\n"
}

func writeAtomicRoot(base, name string, data []byte, mode os.FileMode, gid int) error {
	if !filepath.IsLocal(name) {
		return failure("invalid_path", "Configuration path must remain in its managed root", 400)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer root.Close()
	if err = root.MkdirAll(filepath.Dir(name), 0700); err != nil {
		return err
	}
	nonce := make([]byte, 12)
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	temp := filepath.Join(filepath.Dir(name), ".config-"+hex.EncodeToString(nonce))
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if err == nil && os.Geteuid() == 0 {
		err = file.Chown(0, gid)
	}
	if err == nil {
		err = file.Chmod(mode)
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return root.Rename(temp, name)
}
func nginxQuote(value string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`) + `"`
}
func redisQuote(value string) string { return nginxQuote(value) }
func unitQuote(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(value) + `"`
}

func (e *PrivilegedExecutor) serviceArgs(instance Instance, command string) ([]string, error) {
	switch instance.PluginID {
	case "openresty":
		args := []string{"-p", instance.Paths.Root + "/", "-c", filepath.Join(instance.Paths.ConfigDir, "nginx.conf")}
		switch command {
		case "check":
			return append(args, "-t"), nil
		case "start":
			return args, nil
		case "stop":
			return append(args, "-s", "quit"), nil
		case "reload":
			return append(args, "-s", "reload"), nil
		}
	case "php", "php-apt", "php-yum":
		args := []string{"-y", filepath.Join(instance.Paths.ConfigDir, "php-fpm.conf"), "-c", filepath.Join(instance.Paths.ConfigDir, "php.ini")}
		switch command {
		case "check":
			return append(args, "-t"), nil
		case "start":
			return append(args, "-D"), nil
		}
	case "redis", "valkey":
		switch command {
		case "check":
			return []string{"--version"}, nil
		case "start":
			return []string{filepath.Join(instance.Paths.ConfigDir, "service.conf")}, nil
		}
	case "memcached":
		switch command {
		case "check":
			return []string{"-h"}, nil
		case "start":
			bind, _ := instance.Config["bind"].(string)
			return []string{"-d", "-m", strconv.Itoa(setting(instance.Config, "maxmemory_mb", 64)), "-p", strconv.Itoa(setting(instance.Config, "port", 18211)), "-l", bind, "-t", strconv.Itoa(setting(instance.Config, "threads", 2)), "-P", filepath.Join(instance.Paths.RunDir, "server.pid")}, nil
		}
	case "mysql", "mysql-community", "mariadb", "postgresql", "mongodb":
		return databaseServiceArgs(instance, command)
	case "pureftp", "rsyncd", "supervisor", "docker":
		return utilityServiceArgs(instance, command)
	}
	return nil, failure("unsupported_service_verb", "This runtime does not support the requested service verb", 412)
}
func (e *PrivilegedExecutor) control(ctx context.Context, input ServiceInput) (map[string]any, error) {
	unlock, err := e.lockContext(ctx, input.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	instance, err := e.get(ctx, input.ID, false)
	if err != nil {
		return nil, err
	}
	result, err := e.controlLocked(ctx, input, instance)
	if err != nil {
		return nil, err
	}
	if input.Command != "check" {
		if input.Command == "stop" {
			instance.Status = "stopped"
		} else {
			instance.Status = "running"
		}
		instance.Health = e.probe(ctx, instance)
		instance.UpdatedAt = time.Now().UTC()
		if err = e.save("receipts", instance.ID, instance); err != nil {
			return nil, err
		}
	}
	return result, nil
}
func (e *PrivilegedExecutor) controlLocked(ctx context.Context, input ServiceInput, instance Instance) (map[string]any, error) {
	if instance.Service.Program == "" {
		if input.Command == "check" {
			return map[string]any{"id": instance.ID, "configuration": "valid", "running": false}, nil
		}
		return nil, failure("service_unavailable", "This integration has no independently managed daemon", 412)
	}
	if instance.Service.Kind == "cli" {
		if input.Command != "check" {
			return nil, failure("service_unavailable", "This installed CLI has no daemon to start or stop", 412)
		}
		version, err := runFixed(ctx, instance.Service.Program, []string{"--version"}, 10*time.Second, e.credential(instance))
		if err != nil {
			return nil, err
		}
		return map[string]any{"id": instance.ID, "version": strings.TrimSpace(version), "kind": "cli"}, nil
	}
	if input.Command == "restart" {
		if _, err := e.controlLocked(ctx, ServiceInput{ID: input.ID, Command: "stop"}, instance); err != nil {
			return nil, err
		}
		return e.controlLocked(ctx, ServiceInput{ID: input.ID, Command: "start"}, instance)
	}
	if input.Command == "reload" && instance.PluginID == "openresty" {
		generation, err := e.reloadOpenResty(ctx, instance)
		if err != nil {
			return nil, err
		}
		return map[string]any{"id": input.ID, "command": "reload", "exit_code": 0, "applied": true, "generation": generation, "health": e.probe(ctx, instance)}, nil
	}
	if input.Command == "start" && e.probe(ctx, instance).Status == "healthy" {
		return map[string]any{"id": input.ID, "command": "start", "already_running": true}, nil
	}
	if e.config.UseSystemd && input.Command != "check" {
		if !ownedUnit(instance) {
			return nil, failure("ownership_invalid", "Service unit must match its managed receipt", 412)
		}
		if err := checkOwnedUnit(instance); err != nil {
			return nil, err
		}
		if _, err := runFixed(ctx, "/usr/bin/systemctl", []string{input.Command, instance.Service.Unit}, 45*time.Second, nil); err != nil {
			return nil, err
		}
	} else if input.Command == "stop" || input.Command == "reload" {
		if instance.PluginID == "openresty" {
			args, err := e.serviceArgs(instance, input.Command)
			if err != nil {
				return nil, err
			}
			if _, err = runFixed(ctx, instance.Service.Program, args, 15*time.Second, e.credential(instance)); err != nil {
				return nil, err
			}
		} else {
			signal := syscall.SIGTERM
			if input.Command == "reload" {
				if !isPHPRuntime(instance.PluginID) {
					return nil, failure("reload_unavailable", "This runtime requires a controlled restart", 412)
				}
				signal = syscall.SIGUSR2
			}
			if err := signalOwnedProcess(instance, signal); err != nil {
				return nil, err
			}
		}
	} else {
		args, err := e.serviceArgs(instance, input.Command)
		if err != nil {
			return nil, err
		}
		if _, err = runFixedEnvironment(ctx, instance.Service.Program, args, 45*time.Second, e.credential(instance), runtimeEnvironment(instance)); err != nil {
			return nil, err
		}
	}
	if input.Command == "start" || input.Command == "reload" {
		deadline := time.Now().Add(8 * time.Second)
		for {
			health := e.probe(ctx, instance)
			if health.Status == "healthy" {
				break
			}
			if time.Now().After(deadline) {
				return nil, failure("health_check_failed", "Managed service did not respond to its protocol health check", 502)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	return map[string]any{"id": input.ID, "command": input.Command, "exit_code": 0, "health": e.probe(ctx, instance)}, nil
}
func ownedProcessID(instance Instance) (int, error) {
	data, err := os.ReadFile(filepath.Join(instance.Paths.RunDir, "server.pid"))
	if err != nil {
		return 0, failure("runtime_not_running", "Managed PID is unavailable", 412)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid < 2 {
		return 0, failure("ownership_invalid", "Managed PID file is invalid", 412)
	}
	exe, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return 0, failure("runtime_not_running", "Managed process is not running", 412)
	}
	expected, err := filepath.EvalSymlinks(instance.Service.Program)
	if err != nil {
		return 0, err
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return 0, err
	}
	belongs := strings.Contains(string(cmdline), instance.Paths.Root)
	if instance.PluginID == "redis" || instance.PluginID == "valkey" {
		belongs = belongs || strings.Contains(string(cmdline), ":"+strconv.Itoa(setting(instance.Config, "port", 0)))
	}
	if os.Geteuid() == 0 && (instance.PluginID == "openresty" || isPHPRuntime(instance.PluginID) || instance.PluginID == "docker" || instance.PluginID == "pureftp") {
		status, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
		if err != nil {
			return 0, err
		}
		rootMaster := false
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "Uid:") {
				fields := strings.Fields(line)
				rootMaster = len(fields) == 5 && fields[1] == "0" && fields[2] == "0" && fields[3] == "0"
				break
			}
		}
		if !rootMaster {
			return 0, failure("ownership_invalid", "Managed privileged master must run as the controlled root identity", 412)
		}
	}
	if exe != expected || !belongs {
		return 0, failure("ownership_invalid", "PID does not belong to the managed executable and instance", 412)
	}
	return pid, nil
}
func signalOwnedProcess(instance Instance, signal syscall.Signal) error {
	pid, err := ownedProcessID(instance)
	if core.ErrorCode(err) == "runtime_not_running" {
		return nil
	}
	if err != nil {
		return err
	}
	if err = syscall.Kill(pid, signal); err != nil {
		return err
	}
	if signal == syscall.SIGTERM {
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); os.IsNotExist(err) {
				return nil
			}
			time.Sleep(50 * time.Millisecond)
		}
		return failure("stop_timeout", "Owned process did not stop; no runtime or data was removed", 502)
	}
	return nil
}
func (e *PrivilegedExecutor) probe(ctx context.Context, instance Instance) Health {
	health := Health{Status: "unhealthy", CheckedAt: time.Now().UTC(), Reason: "The owned runtime is not responding"}
	if instance.Status == "uninstalled" {
		health.Status = "uninstalled"
		health.Reason = ""
		return health
	}
	if instance.Service.Program == "" {
		health.Status = "configuration_required"
		health.Reason = "Integration enabled; configure and verify its actual target separately"
		return health
	}
	if instance.Service.Kind == "cli" {
		output, err := runFixed(ctx, instance.Service.Program, []string{"--version"}, 10*time.Second, e.credential(instance))
		if err == nil && strings.Contains(strings.ToLower(output), "lego") {
			health.Status = "available"
			health.Reason = "Verified CLI is installed; DNS credentials/actual issuance remain separate checks"
		}
		return health
	}
	if _, err := ownedProcessID(instance); err != nil {
		return health
	}
	probeTimeout := 500 * time.Millisecond
	database := instance.PluginID == "mysql" || instance.PluginID == "mysql-community" || instance.PluginID == "mariadb" || instance.PluginID == "postgresql" || instance.PluginID == "mongodb"
	if database {
		probeTimeout = 3 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if database {
		if e.databaseProbe(probeCtx, instance) == nil {
			health.Status = "healthy"
			health.Reason = ""
		}
		return health
	}
	if isPHPRuntime(instance.PluginID) {
		if probePHPFastCGI(probeCtx, filepath.Join(instance.Paths.RunDir, "fpm.sock")) == nil {
			health.Status = "healthy"
			health.Reason = ""
		}
		return health
	}
	bind, _ := instance.Config["bind"].(string)
	if bind == "" || bind == "0.0.0.0" {
		bind = "127.0.0.1"
	}
	if bind == "::" {
		bind = "::1"
	}
	port := setting(instance.Config, "port", 0)
	if port == 0 {
		return e.utilityProbe(probeCtx, instance)
	}
	address := net.JoinHostPort(bind, strconv.Itoa(port))
	if instance.PluginID == "openresty" {
		file, err := readProfileFile(instance.Paths.ConfigDir, "nginx.conf")
		if err != nil {
			return health
		}
		generation, err := e.webGeneration(instance, file.Body)
		if err == nil && probeWebGeneration(probeCtx, address, generation) == nil {
			health.Status = "healthy"
			health.Reason = ""
		}
		return health
	}
	connection, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", address)
	if err != nil {
		return health
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(400 * time.Millisecond))
	switch instance.PluginID {
	case "redis", "valkey":
		_, err = io.WriteString(connection, "*1\r\n$4\r\nPING\r\n")
		if err == nil {
			data := make([]byte, 64)
			n, readErr := connection.Read(data)
			if readErr != nil || string(data[:n]) != "+PONG\r\n" {
				return health
			}
		}
	case "memcached":
		_, err = io.WriteString(connection, "version\r\n")
		if err == nil {
			data := make([]byte, 128)
			n, readErr := connection.Read(data)
			if readErr != nil || !strings.HasPrefix(string(data[:n]), "VERSION ") {
				return health
			}
		}
	case "pureftp":
		data := make([]byte, 256)
		n, readErr := connection.Read(data)
		if readErr != nil || !strings.HasPrefix(string(data[:n]), "220") {
			return health
		}
	}
	if err == nil {
		health.Status = "healthy"
		health.Reason = ""
	}
	return health
}
func ownedUnit(instance Instance) bool {
	return validInstanceID(instance.ID) && instance.Service.Unit == "powerlinux-plugin-"+instance.ID+".service"
}

type cachedHealth struct {
	Revision int64
	Status   string
	Health   Health
}

func (e *PrivilegedExecutor) cachedHealth(ctx context.Context, instance Instance) Health {
	e.healthMu.Lock()
	cached, exists := e.healthCache[instance.ID]
	e.healthMu.Unlock()
	if exists && cached.Revision == instance.Revision && cached.Status == instance.Status && time.Since(cached.Health.CheckedAt) < 5*time.Second {
		return cached.Health
	}
	health := e.probe(ctx, instance)
	e.healthMu.Lock()
	if e.healthCache == nil {
		e.healthCache = map[string]cachedHealth{}
	}
	if len(e.healthCache) >= 256 {
		for id := range e.healthCache {
			delete(e.healthCache, id)
			break
		}
	}
	e.healthCache[instance.ID] = cachedHealth{Revision: instance.Revision, Status: instance.Status, Health: health}
	e.healthMu.Unlock()
	return health
}
func (e *PrivilegedExecutor) writeUnit(ctx context.Context, instance Instance) error {
	if !ownedUnit(instance) {
		return failure("ownership_invalid", "Managed unit name is not trusted", 412)
	}
	dir := e.systemdUnitDir()
	name := filepath.Join(dir, instance.Service.Unit)
	if _, err := os.Lstat(name); err == nil {
		var receipt Instance
		if e.load("receipts", instance.ID, &receipt) != nil || receipt.Service.Unit != instance.Service.Unit {
			return failure("ownership_conflict", "Existing service unit is not recorded as owned; it was preserved", 409)
		}
		if err := checkOwnedUnit(receipt); err != nil {
			return err
		}
	}
	unit, err := systemdUnitBytes(e, instance, "")
	if err != nil {
		return err
	}
	if err = writeAtomicRoot(dir, instance.Service.Unit, unit, 0644, 0); err != nil {
		return err
	}
	_, err = runFixed(ctx, "/usr/bin/systemctl", []string{"daemon-reload"}, 20*time.Second, nil)
	return err
}
func (e *PrivilegedExecutor) removeUnit(ctx context.Context, instance Instance) error {
	if !e.config.UseSystemd || instance.Service.Program == "" || instance.Service.Kind == "cli" {
		return nil
	}
	if !ownedUnit(instance) {
		return failure("ownership_invalid", "Service unit does not match its protected receipt", 412)
	}
	name := filepath.Join("/etc/systemd/system", instance.Service.Unit)
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	body, readErr := io.ReadAll(io.LimitReader(file, 64<<10))
	_ = file.Close()
	if readErr != nil {
		return readErr
	}
	digest := sha256.Sum256(body)
	if instance.Service.UnitSHA256 == "" || hex.EncodeToString(digest[:]) != instance.Service.UnitSHA256 {
		return failure("ownership_conflict", "Service unit changed outside its managed receipt; it was preserved", 409)
	}
	if _, err = runFixed(ctx, "/usr/bin/systemctl", []string{"disable", "--now", instance.Service.Unit}, 30*time.Second, nil); err != nil {
		return err
	}
	root, err := os.OpenRoot("/etc/systemd/system")
	if err != nil {
		return err
	}
	defer root.Close()
	if err = root.Remove(instance.Service.Unit); err != nil {
		return err
	}
	_, err = runFixed(ctx, "/usr/bin/systemctl", []string{"daemon-reload"}, 20*time.Second, nil)
	return err
}
func checkOwnedUnit(instance Instance) error {
	file, err := os.OpenFile(filepath.Join("/etc/systemd/system", instance.Service.Unit), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return failure("ownership_invalid", "Managed unit is missing or unsafe", 412)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 || info.Mode().Perm()&0022 != 0 {
		return failure("ownership_invalid", "Managed unit permissions/format are unsafe", 412)
	}
	body, err := io.ReadAll(io.LimitReader(file, 64<<10))
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	if instance.Service.UnitSHA256 == "" || hex.EncodeToString(digest[:]) != instance.Service.UnitSHA256 {
		return failure("ownership_conflict", "Managed unit changed outside its receipt; it was preserved", 409)
	}
	return nil
}
