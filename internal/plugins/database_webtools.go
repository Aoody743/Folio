package plugins

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var managedDatabaseWebToolIDs = map[string]bool{
	"mysql": true, "mysql-community": true, "mariadb": true,
}

func validatePGAdminInstallConfig(config map[string]any) error {
	email, _ := config["admin_email"].(string)
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email || len(email) > 254 {
		return failure("invalid_config", "pgAdmin requires a valid initial administrator email address", 400)
	}
	password, _ := config["admin_password"].(string)
	if len(password) < 12 || len(password) > 128 || strings.ContainsAny(password, "\x00\r\n") {
		return failure("invalid_config", "pgAdmin's initial administrator password must be 12–128 characters without line breaks", 400)
	}
	return nil
}

func (e *PrivilegedExecutor) configurePHPMyAdmin(ctx context.Context, instance *Instance) error {
	appRoot := filepath.Join(instance.Paths.Root, "runtime")
	for _, path := range []string{filepath.Join(appRoot, "index.php"), appRoot} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (path == appRoot && !info.IsDir()) || (path != appRoot && !info.Mode().IsRegular()) {
			return failure("application_unavailable", "The pinned phpMyAdmin release is missing from its owned runtime directory", 412)
		}
	}
	target, err := e.phpMyAdminDatabaseTarget(ctx, instance.Config)
	if err != nil {
		return err
	}
	host := managedDatabaseHost(target)
	port := setting(target.Config, "port", 0)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() || port < 1 || port > 65535 {
		return failure("database_target_invalid", "phpMyAdmin can only target a managed MySQL-compatible database listening on loopback", 412)
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	secret := base64.RawStdEncoding.EncodeToString(raw)
	for _, name := range []string{"sessions", "tmp"} {
		path := filepath.Join(instance.Paths.DataDir, name)
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
		if err := e.setRuntimeDirectory(path, 0700, e.uid, true); err != nil {
			return err
		}
	}
	body := "<?php\n" +
		"$cfg['blowfish_secret'] = " + phpSingleQuoted(secret) + ";\n" +
		"$cfg['Servers'][1]['auth_type'] = 'cookie';\n" +
		"$cfg['Servers'][1]['host'] = " + phpSingleQuoted(host) + ";\n" +
		"$cfg['Servers'][1]['port'] = " + phpSingleQuoted(strconv.Itoa(port)) + ";\n" +
		"$cfg['Servers'][1]['connect_type'] = 'tcp';\n" +
		"$cfg['Servers'][1]['AllowNoPassword'] = false;\n" +
		"$cfg['Servers'][1]['AllowArbitraryServer'] = false;\n" +
		"$cfg['ServerDefault'] = 1;\n" +
		"$cfg['TempDir'] = " + phpSingleQuoted(filepath.Join(instance.Paths.DataDir, "tmp")) + ";\n" +
		"$cfg['UploadDir'] = '';\n$cfg['SaveDir'] = '';\n"
	if err := writeAtomicRoot(instance.Paths.Root, "runtime/config.inc.php", []byte(body), 0640, int(e.gid)); err != nil {
		return err
	}
	router := "<?php\n" +
		"file_put_contents(" + phpSingleQuoted(filepath.Join(instance.Paths.RunDir, "server.pid")) + ", (string) getmypid(), LOCK_EX);\n" +
		"$path = rawurldecode((string) parse_url($_SERVER['REQUEST_URI'] ?? '/', PHP_URL_PATH));\n" +
		"if (preg_match('~(?:^|/)setup(?:/|$)~i', $path) === 1) { http_response_code(404); return true; }\n" +
		"return false;\n"
	if err := writeAtomicRoot(instance.Paths.ConfigDir, "phpmyadmin-router.php", []byte(router), 0640, int(e.gid)); err != nil {
		return err
	}
	return nil
}

func phpSingleQuoted(value string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(value) + "'"
}

func (e *PrivilegedExecutor) phpMyAdminDatabaseTarget(ctx context.Context, config map[string]any) (Instance, error) {
	wanted, _ := config["target_id"].(string)
	entries, err := os.ReadDir(filepath.Join(e.config.StateDir, "receipts"))
	if err != nil {
		return Instance{}, failure("database_target_missing", "Install a managed MySQL or MariaDB instance before configuring phpMyAdmin", 412)
	}
	available := []Instance{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		candidate, getErr := e.get(ctx, id, false)
		if getErr != nil || candidate.Status == "uninstalled" || !managedDatabaseWebToolIDs[candidate.PluginID] {
			continue
		}
		if wanted != "" && id == wanted {
			return candidate, nil
		}
		available = append(available, candidate)
	}
	if wanted != "" {
		return Instance{}, failure("database_target_missing", "The selected phpMyAdmin target is not an installed managed MySQL-compatible instance", 412)
	}
	if len(available) == 0 {
		return Instance{}, failure("database_target_missing", "Install a managed MySQL or MariaDB instance before configuring phpMyAdmin", 412)
	}
	if len(available) != 1 {
		return Instance{}, failure("database_target_ambiguous", "Set phpMyAdmin config.target_id to a managed MySQL or MariaDB instance ID when zero or multiple targets are installed", 412)
	}
	return available[0], nil
}

func (e *PrivilegedExecutor) configurePGAdmin(ctx context.Context, instance *Instance) error {
	const appMain = "/usr/pgadmin4/web/pgAdmin4.py"
	if _, err := os.Stat(appMain); err != nil {
		return failure("application_unavailable", "The official pgAdmin server package does not contain its web application entry point", 412)
	}
	for _, name := range []string{"sessions", "storage"} {
		path := filepath.Join(instance.Paths.DataDir, name)
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
		if err := e.setRuntimeDirectory(path, 0700, e.uid, true); err != nil {
			return err
		}
	}
	bind, _ := instance.Config["bind"].(string)
	if bind == "" {
		bind = "127.0.0.1"
	}
	config := fmt.Sprintf("SERVER_MODE = True\nDATA_DIR = %s\nDEFAULT_SERVER = %s\nDEFAULT_SERVER_PORT = %d\n", pythonQuoted(instance.Paths.DataDir), pythonQuoted(bind), setting(instance.Config, "port", 19051))
	if err := writeAtomicRoot(instance.Paths.ConfigDir, "pgadmin.py", []byte(config), 0640, int(e.gid)); err != nil {
		return err
	}
	pidPath := pythonQuoted(filepath.Join(instance.Paths.RunDir, "server.pid"))
	launcher := "import builtins, os, runpy\nbuiltins.SERVER_MODE = True\n" +
		"pid_path = " + pidPath + "\nfd = os.open(pid_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, 'O_NOFOLLOW', 0), 0o600)\n" +
		"os.write(fd, str(os.getpid()).encode())\nos.close(fd)\n" +
		"runpy.run_path(" + pythonQuoted(appMain) + ", run_name='__main__')\n"
	if err := writeAtomicRoot(instance.Paths.ConfigDir, "pgadmin-launch.py", []byte(launcher), 0640, int(e.gid)); err != nil {
		return err
	}
	if password, ok := instance.Config["admin_password"].(string); ok && password != "" {
		email, _ := instance.Config["admin_email"].(string)
		if err := validatePGAdminInstallConfig(instance.Config); err != nil {
			return err
		}
		if err := e.bootstrapPGAdminUser(ctx, *instance, email, password); err != nil {
			return err
		}
		delete(instance.Config, "admin_password")
	}
	return nil
}

func pythonQuoted(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func (e *PrivilegedExecutor) bootstrapPGAdminUser(ctx context.Context, instance Instance, email, password string) error {
	program := instance.Service.Program
	if program == "" {
		return failure("application_unavailable", "pgAdmin's managed Python runtime is not installed", 412)
	}
	secretEmail, _ := json.Marshal(email)
	secretPassword, _ := json.Marshal(password)
	script := "import sys, os\nos.umask(0o077)\nsys.path.insert(0, '/usr/pgadmin4/web')\n" +
		"import pgAdmin4\nfrom pgadmin.model import User\nfrom pgadmin.utils.constants import INTERNAL\nimport setup\n" +
		"email = " + string(secretEmail) + "\npassword = " + string(secretPassword) + "\n" +
		"with pgAdmin4.app.test_request_context():\n" +
		"    user = User.query.filter_by(username=email, auth_source=INTERNAL).first()\n" +
		"if user is None:\n" +
		"    setup.ManageUsers.create_user({'email': email, 'role': 'Administrator', 'active': True, 'auth_source': INTERNAL, 'newPassword': password, 'confirmPassword': password}, False, False)\n" +
		"with pgAdmin4.app.test_request_context():\n" +
		"    user = User.query.filter_by(username=email, auth_source=INTERNAL).first()\n" +
		"    if user is None or not user.active:\n" +
		"        raise RuntimeError('pgAdmin initial administrator was not created')\n"
	nameBytes := make([]byte, 12)
	if _, err := rand.Read(nameBytes); err != nil {
		return err
	}
	name := ".pgadmin-bootstrap-" + hex.EncodeToString(nameBytes) + ".py"
	if err := writeWorkerPrivate(instance.Paths.DataDir, name, []byte(script), e.uid, e.gid); err != nil {
		return err
	}
	root, err := os.OpenRoot(instance.Paths.DataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	defer root.Remove(name)
	environment := runtimeEnvironment(instance)
	if _, err := runFixedEnvironment(ctx, program, []string{filepath.Join(instance.Paths.DataDir, name)}, 2*time.Minute, e.credential(instance), environment); err != nil {
		return failure("pgadmin_admin_setup_failed", "pgAdmin could not create the initial administrator account; inspect its protected install checkpoint", 502)
	}
	return nil
}

func writeWorkerPrivate(base, name string, data []byte, uid, gid uint32) error {
	if !filepath.IsLocal(name) || strings.ContainsAny(name, `/\\`) {
		return failure("invalid_path", "Temporary setup program path is invalid", 400)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		err = file.Chown(int(uid), int(gid))
	}
	if err == nil {
		err = file.Chmod(0600)
	}
	if err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = root.Remove(name)
	}
	return err
}

func probeManagedDatabaseWebTool(ctx context.Context, plugin, address string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/", nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if request.URL.Scheme != "http" || request.URL.Host != address || len(via) >= 5 {
			return fmt.Errorf("admin web tool redirected outside its loopback listener")
		}
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil || len(body) == 1<<20 || response.StatusCode < 200 || response.StatusCode >= 500 {
		return fmt.Errorf("admin web tool returned an invalid health response")
	}
	marker := map[string]string{"phpmyadmin": "phpmyadmin", "pgadmin": "pgadmin"}[plugin]
	if marker == "" || !strings.Contains(strings.ToLower(string(body)), marker) {
		return fmt.Errorf("admin web tool response did not identify the expected application")
	}
	return nil
}
