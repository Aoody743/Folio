package plugins

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/go-sql-driver/mysql"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Kept only in the root Agent's private state. Neither a receipt nor a public
// configuration contains administrative credentials.
type managedDatabaseCredential struct {
	Engine, Username, Password, DataDir string
	Initialized                         bool
	Version                             string
}

func databasePrivateID(instance Instance) string { return "database_" + instance.ID }
func pgQuote(value string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(value, "\\", "\\\\"), "'", "''") + "'"
}

// PostgreSQL config files and SQL literals have different escape grammars.
// Explicit escape-string syntax preserves password backslashes regardless of
// the session's standard_conforming_strings setting.
func pgSQLLiteral(value string) string { return "E" + pgQuote(value) }
func mysqlOptionQuote(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(value) + `"`
}

func (e *PrivilegedExecutor) configureDatabase(ctx context.Context, instance *Instance) error {
	bind, _ := instance.Config["bind"].(string)
	if bind == "" {
		bind = "127.0.0.1"
	}
	ip := net.ParseIP(bind)
	if ip == nil || !ip.IsLoopback() {
		return failure("tls_configuration_required", "Database public binding requires an explicit owned TLS profile; initial managed instances use loopback", 412)
	}
	if !e.config.UseSystemd {
		return failure("service_manager_required", "Managed database daemons currently require a controlled systemd service", 412)
	}
	if instance.PluginID != "mongodb" {
		if err := e.validateDatabaseRuntimeTools(ctx, *instance); err != nil {
			return err
		}
	}
	if err := os.Chmod(instance.Paths.RunDir, 0700); err != nil {
		return err
	}
	var credential managedDatabaseCredential
	loadErr := e.load("private", databasePrivateID(*instance), &credential)
	if loadErr != nil {
		entries, err := os.ReadDir(instance.Paths.DataDir)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return failure("ownership_conflict", "Existing database data has no private initialization receipt; it was preserved", 409)
		}
		raw := make([]byte, 32)
		if _, err = rand.Read(raw); err != nil {
			return err
		}
		credential = managedDatabaseCredential{Engine: instance.PluginID, Username: "root", Password: base64.RawURLEncoding.EncodeToString(raw), DataDir: instance.Paths.DataDir}
		if instance.PluginID == "postgresql" {
			credential.Username = e.config.RunUser
		}
		if instance.PluginID == "mongodb" {
			credential.Username = "powerlinux_admin"
		}
		if err = e.save("private", databasePrivateID(*instance), credential); err != nil {
			return err
		}
	} else if credential.Engine != instance.PluginID || credential.DataDir != instance.Paths.DataDir {
		return failure("ownership_invalid", "Private database receipt differs from its trusted layout", 412)
	} else if !credential.Initialized {
		return failure("operation_recovery_required", "Database bootstrap was interrupted; inspect preserved data before recovery", 409)
	}
	port := setting(instance.Config, "port", 0)
	if port < 1024 || port > 65535 {
		return failure("invalid_config", "Managed database port must be 1024–65535", 400)
	}
	switch instance.PluginID {
	case "postgresql":
		if err := e.ensurePostgreSQLMaintenanceSettled(ctx, *instance); err != nil {
			return err
		}
		if !credential.Initialized {
			if err := e.initializePostgres(ctx, *instance, credential); err != nil {
				return err
			}
		}
		body := fmt.Sprintf("data_directory = %s\nlisten_addresses = %s\nport = %d\nunix_socket_directories = %s\nexternal_pid_file = %s\nlogging_collector = on\nlog_directory = %s\nlog_filename = 'postgresql.log'\npassword_encryption = 'scram-sha-256'\nmax_connections = 100\nshared_buffers = '64MB'\n", pgQuote(instance.Paths.DataDir), pgQuote(bind), port, pgQuote(instance.Paths.RunDir), pgQuote(filepath.Join(instance.Paths.RunDir, "server.pid")), pgQuote(instance.Paths.LogDir))
		if previous, readErr := readProfileFile(instance.Paths.ConfigDir, "postgresql.conf"); readErr == nil {
			preserved, preserveErr := preservePostgreSQLMaintenanceRuntimeConfig([]byte(body), previous.Body)
			if preserveErr != nil {
				return preserveErr
			}
			body = string(preserved)
		} else if !os.IsNotExist(readErr) {
			return readErr
		}
		if err := writeAtomicRoot(instance.Paths.ConfigDir, "postgresql.conf", []byte(body), 0640, int(e.gid)); err != nil {
			return err
		}
	case "mysql", "mysql-community", "mariadb":
		if !credential.Initialized {
			if err := e.initializeMySQL(ctx, *instance, credential); err != nil {
				return err
			}
		}
		body := fmt.Sprintf("[mysqld]\nbind-address=%s\nport=%d\ndatadir=%s\nsocket=%s\npid-file=%s\nlog-error=%s\nskip-name-resolve\nlocal-infile=0\n[client]\nsocket=%s\n", bind, port, mysqlOptionQuote(instance.Paths.DataDir), mysqlOptionQuote(filepath.Join(instance.Paths.RunDir, "mysql.sock")), mysqlOptionQuote(filepath.Join(instance.Paths.RunDir, "server.pid")), mysqlOptionQuote(filepath.Join(instance.Paths.LogDir, "mysql.log")), mysqlOptionQuote(filepath.Join(instance.Paths.RunDir, "mysql.sock")))
		if err := writeAtomicRoot(instance.Paths.ConfigDir, "my.cnf", []byte(body), 0640, int(e.gid)); err != nil {
			return err
		}
	case "mongodb":
		if !credential.Initialized {
			if err := e.initializeMongo(ctx, *instance, credential); err != nil {
				return err
			}
		}
		// JSON is a valid YAML subset, with paths serialized rather than injected.
		body, _ := json.Marshal(map[string]any{"storage": map[string]any{"dbPath": instance.Paths.DataDir}, "net": map[string]any{"bindIp": bind, "port": port, "unixDomainSocket": map[string]any{"enabled": true, "pathPrefix": instance.Paths.RunDir, "filePermissions": 0600}}, "systemLog": map[string]any{"destination": "file", "path": filepath.Join(instance.Paths.LogDir, "mongodb.log"), "logAppend": true}, "processManagement": map[string]any{"pidFilePath": filepath.Join(instance.Paths.RunDir, "server.pid")}, "security": map[string]any{"authorization": "enabled"}})
		if err := writeAtomicRoot(instance.Paths.ConfigDir, "mongod.conf", body, 0640, int(e.gid)); err != nil {
			return err
		}
	default:
		return failure("unsupported_database", "Database initialization recipe is unavailable", 412)
	}
	if !credential.Initialized {
		credential.Initialized = true
		if err := e.save("private", databasePrivateID(*instance), credential); err != nil {
			return err
		}
	}
	return nil
}
func (e *PrivilegedExecutor) initializePostgres(ctx context.Context, instance Instance, credential managedDatabaseCredential) error {
	program := filepath.Join(filepath.Dir(instance.Service.Program), "initdb")
	if err := trustedDatabaseProgram(program); err != nil {
		return err
	}
	file, err := os.CreateTemp(instance.Paths.RunDir, ".pg-init-password-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0600); err == nil {
		_, err = file.WriteString(credential.Password + "\n")
	}
	if err == nil && os.Geteuid() == 0 {
		err = file.Chown(int(e.uid), int(e.gid))
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	_, err = runFixed(ctx, program, []string{"-D", instance.Paths.DataDir, "--username=" + credential.Username, "--pwfile=" + name, "--auth-host=scram-sha-256", "--auth-local=peer", "--encoding=UTF8", "--no-locale"}, 2*time.Minute, e.credential(instance))
	return err
}
func trustedDatabaseProgram(program string) error {
	info, err := os.Stat(program)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
		return failure("runtime_unavailable", "Trusted database utility is absent or writable by unrelated users", 412)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != 0 {
		return failure("runtime_unavailable", "Database utility must belong to root", 412)
	}
	return nil
}
func (e *PrivilegedExecutor) initializeMySQL(ctx context.Context, instance Instance, credential managedDatabaseCredential) error {
	if instance.PluginID == "mariadb" {
		installer := "/usr/bin/mariadb-install-db"
		if err := trustedDatabaseProgram(installer); err != nil {
			return err
		}
		if _, err := runFixed(ctx, installer, []string{"--no-defaults", "--datadir=" + instance.Paths.DataDir, "--auth-root-authentication-method=normal", "--skip-test-db"}, 2*time.Minute, e.credential(instance)); err != nil {
			return err
		}
	} else {
		if _, err := runFixed(ctx, instance.Service.Program, []string{"--no-defaults", "--initialize-insecure", "--datadir=" + instance.Paths.DataDir}, 2*time.Minute, e.credential(instance)); err != nil {
			return err
		}
	}
	socket := filepath.Join(instance.Paths.RunDir, "bootstrap-mysql.sock")
	args := []string{"--no-defaults", "--skip-networking", "--datadir=" + instance.Paths.DataDir, "--socket=" + socket, "--pid-file=" + filepath.Join(instance.Paths.RunDir, "bootstrap.pid"), "--log-error=" + filepath.Join(instance.Paths.LogDir, "bootstrap-mysql.log")}
	stop, err := e.startDatabaseBootstrap(ctx, instance, args)
	if err != nil {
		return err
	}
	defer stop()
	config := mysql.NewConfig()
	config.User = "root"
	config.Net = "unix"
	config.Addr = socket
	config.Timeout = time.Second
	config.ReadTimeout = 5 * time.Second
	config.WriteTimeout = 5 * time.Second
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return err
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	deadline := time.Now().Add(45 * time.Second)
	for {
		if db.PingContext(ctx) == nil {
			break
		}
		if time.Now().After(deadline) {
			return failure("bootstrap_failed", "Private MySQL bootstrap did not become ready", 502)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	// Password is random URL-safe ASCII created above, never an API operand.
	if _, err = db.ExecContext(ctx, "ALTER USER 'root'@'localhost' IDENTIFIED BY '"+credential.Password+"'"); err != nil {
		return failure("bootstrap_failed", "Private MySQL credential initialization failed", 502)
	}
	if _, err = db.ExecContext(ctx, "DROP DATABASE IF EXISTS test"); err != nil {
		return failure("bootstrap_failed", "Private MySQL test database cleanup failed", 502)
	}
	_, _ = db.ExecContext(ctx, "SHUTDOWN")
	return nil
}
func (e *PrivilegedExecutor) startDatabaseBootstrap(ctx context.Context, instance Instance, args []string) (func(), error) {
	operation := agent.Operation{Name: "database_private_bootstrap", Program: instance.Service.Program, Credential: e.credential(instance), Timeout: 2 * time.Minute, BuildArgs: func(json.RawMessage) ([]string, error) { return args, nil }}
	runner, err := agent.NewRunner(agent.Options{Operations: []agent.Operation{operation}, MaxConcurrent: 1, MaxOutputBytes: 64 << 10})
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = runner.Run(runCtx, agent.Request{Operation: operation.Name, Input: json.RawMessage(`{}`)})
	}()
	return func() { cancel(); <-done }, nil
}
func (e *PrivilegedExecutor) initializeMongo(ctx context.Context, instance Instance, credential managedDatabaseCredential) error {
	socket := filepath.Join(instance.Paths.RunDir, "bootstrap-mongodb.sock")
	args := []string{"--dbpath", instance.Paths.DataDir, "--bind_ip", socket, "--unixSocketPrefix", instance.Paths.RunDir, "--filePermissions", "0600", "--port", strconv.Itoa(setting(instance.Config, "port", 18017)), "--auth", "--logpath", filepath.Join(instance.Paths.LogDir, "bootstrap-mongodb.log")}
	stop, err := e.startDatabaseBootstrap(ctx, instance, args)
	if err != nil {
		return err
	}
	defer stop()
	uri := "mongodb://" + url.PathEscape(socket) + "/?directConnection=true"
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetServerSelectionTimeout(time.Second).SetConnectTimeout(time.Second))
	if err != nil {
		return err
	}
	defer client.Disconnect(context.WithoutCancel(ctx))
	deadline := time.Now().Add(45 * time.Second)
	for {
		if client.Ping(ctx, nil) == nil {
			break
		}
		if time.Now().After(deadline) {
			return failure("bootstrap_failed", "Private UNIX-only MongoDB bootstrap did not become ready", 502)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	command := bson.D{{Key: "createUser", Value: credential.Username}, {Key: "pwd", Value: credential.Password}, {Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}}}
	if err = client.Database("admin").RunCommand(ctx, command).Err(); err != nil {
		return failure("bootstrap_failed", "MongoDB administrative credential initialization failed", 502)
	}
	return nil
}
func databaseServiceArgs(instance Instance, command string) ([]string, error) {
	switch command {
	case "check":
		return []string{"--version"}, nil
	case "start":
		switch instance.PluginID {
		case "postgresql":
			return []string{"-D", instance.Paths.DataDir, "-c", "config_file=" + filepath.Join(instance.Paths.ConfigDir, "postgresql.conf")}, nil
		case "mysql", "mysql-community", "mariadb":
			return []string{"--defaults-file=" + filepath.Join(instance.Paths.ConfigDir, "my.cnf")}, nil
		case "mongodb":
			return []string{"--config", filepath.Join(instance.Paths.ConfigDir, "mongod.conf")}, nil
		}
	}
	return nil, failure("unsupported_service_verb", "Database runtime uses its controlled service for this verb", 412)
}

// A listening port is not a healthy database. Use its private initialized
// identity and real protocol, never a guessed public or client-supplied secret.
func (e *PrivilegedExecutor) databaseProbe(ctx context.Context, instance Instance) error {
	var credential managedDatabaseCredential
	if err := e.load("private", databasePrivateID(instance), &credential); err != nil || !credential.Initialized || credential.Engine != instance.PluginID || credential.DataDir != instance.Paths.DataDir {
		return failure("credential_unavailable", "Owned database credential receipt is unavailable", 412)
	}
	host, _ := instance.Config["bind"].(string)
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	port := setting(instance.Config, "port", 0)
	if instance.PluginID == "mongodb" {
		client, err := mongo.Connect(options.Client().SetHosts([]string{net.JoinHostPort(host, strconv.Itoa(port))}).SetDirect(true).SetAuth(options.Credential{AuthSource: "admin", Username: credential.Username, Password: credential.Password}).SetConnectTimeout(time.Second).SetServerSelectionTimeout(2 * time.Second))
		if err != nil {
			return failure("health_failed", "Database protocol handshake failed", 502)
		}
		defer client.Disconnect(context.WithoutCancel(ctx))
		if err = client.Ping(ctx, nil); err != nil {
			return failure("health_failed", "MongoDB authentication probe failed", 502)
		}
		return nil
	}
	db, err := e.openManagedAdmin(ctx, instance)
	if err != nil {
		return err
	}
	defer db.Close()
	var value int
	if err = db.QueryRowContext(ctx, "SELECT 1").Scan(&value); err != nil || value != 1 {
		return failure("health_failed", "Database authenticated query probe failed", 502)
	}
	return nil
}
