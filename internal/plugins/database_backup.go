package plugins

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

type DatabaseBackup struct {
	ID         string `json:"id"`
	InstanceID string `json:"instance_id"`
	Database   string `json:"database"`
	Engine     string `json:"engine"`
	Filename   string `json:"filename"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
	CreatedAt  string `json:"created_at"`
	State      string `json:"state,omitempty"`
}
type databaseOutput struct {
	ctx   context.Context
	file  *os.File
	hash  hash.Hash
	bytes int64
}

func (w *databaseOutput) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if w.bytes+int64(len(data)) > maxExtractedBytes {
		return 0, failure("backup_limit", "Database backup exceeds four GiB", 413)
	}
	n, err := w.file.Write(data)
	w.hash.Write(data[:n])
	w.bytes += int64(n)
	return n, err
}
func (w *databaseOutput) Close() error { return w.file.Close() }
func (e *PrivilegedExecutor) backupManagedDatabase(ctx context.Context, instance Instance, record managedDatabaseRecord) (DatabaseBackup, error) {
	var out DatabaseBackup
	if instance.PluginID != "mongodb" {
		if err := verifyManagedSQLCredential(ctx, instance, record); err != nil {
			return out, err
		}
	}
	if err := realDirectory(instance.Paths.BackupDir); err != nil {
		return out, err
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return out, err
	}
	out = DatabaseBackup{ID: "dbbackup_" + hex.EncodeToString(random), InstanceID: instance.ID, Database: record.Database, Engine: instance.PluginID, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	out.Filename = out.ID + ".dump"
	file, err := os.CreateTemp(instance.Paths.BackupDir, ".database-backup-")
	if err != nil {
		return out, err
	}
	name := file.Name()
	defer os.Remove(name)
	defer file.Close()
	if err = file.Chmod(0600); err != nil {
		return out, err
	}
	sink := &databaseOutput{ctx: ctx, file: file, hash: sha256.New()}
	if instance.PluginID == "mongodb" {
		err = e.backupMongoDatabase(ctx, instance, record, sink)
	} else {
		program, args, environment, cleanup, prepErr := e.databaseTool(ctx, instance, record, "backup")
		if prepErr != nil {
			return out, prepErr
		}
		defer cleanup()
		runner, prepErr := agent.NewRunner(agent.Options{Operations: []agent.Operation{{Name: "database_dump", Program: program, BuildArgs: func(json.RawMessage) ([]string, error) { return args, nil }, Timeout: 30 * time.Minute, Environment: environment, Credential: e.credential(instance)}}, MaxOutputBytes: 32 << 10, MaxConcurrent: 1})
		if prepErr != nil {
			return out, prepErr
		}
		result, runErr := runner.Stream(ctx, agent.Request{Operation: "database_dump", Input: json.RawMessage(`{}`)}, agent.IO{Stdout: sink})
		if runErr != nil || result.ExitCode != 0 {
			err = failure("backup_failed", "Database dump did not complete; partial artifact was discarded", 502)
		}
	}
	if err != nil {
		return out, err
	}
	if sink.bytes == 0 {
		return out, failure("backup_failed", "Database dump produced an empty artifact", 502)
	}
	if err = file.Sync(); err != nil {
		return out, err
	}
	if err = file.Close(); err != nil {
		return out, err
	}
	target := filepath.Join(instance.Paths.BackupDir, out.Filename)
	if err = os.Link(name, target); err != nil {
		return out, failure("artifact_exists", "Backup artifact already exists or cannot be committed", 409)
	}
	out.Bytes = sink.bytes
	out.SHA256 = hex.EncodeToString(sink.hash.Sum(nil))
	if err = e.save("private", out.ID, out); err != nil {
		return out, failure("operation_recovery_required", "Backup artifact exists but its integrity receipt was not committed", 409)
	}
	if dir, err := os.Open(instance.Paths.BackupDir); err == nil {
		dir.Sync()
		dir.Close()
	}
	return out, nil
}
func (e *PrivilegedExecutor) listDatabaseBackups(ctx context.Context, instance Instance, record managedDatabaseRecord) (any, error) {
	entries, err := os.ReadDir(filepath.Join(e.config.StateDir, "private"))
	if err != nil {
		return nil, err
	}
	items := []DatabaseBackup{}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "dbbackup_") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var backup DatabaseBackup
		id := strings.TrimSuffix(entry.Name(), ".json")
		if err = e.load("private", id, &backup); err != nil {
			return nil, err
		}
		if backup.InstanceID != instance.ID || backup.Database != record.Database {
			continue
		}
		if backup.State == "deleted" {
			continue
		}
		if backup.State != "" && backup.State != "active" {
			return nil, failure("operation_recovery_required", "Backup deletion was interrupted; inspect its ownership receipt", 409)
		}
		if backup.ID != id || backup.Engine != instance.PluginID || backup.Filename != backup.ID+".dump" {
			return nil, failure("ownership_invalid", "Backup receipt does not match its owned artifact", 412)
		}
		root, err := os.OpenRoot(instance.Paths.BackupDir)
		if err != nil {
			return nil, err
		}
		info, err := root.Lstat(backup.Filename)
		root.Close()
		if err != nil || !info.Mode().IsRegular() || info.Size() != backup.Bytes {
			return nil, failure("artifact_invalid", "Owned backup is missing or differs from its integrity receipt", 412)
		}
		items = append(items, backup)
		if len(items) > 10000 {
			return nil, failure("result_limit", "Backup list exceeds its bound", 413)
		}
	}
	return map[string]any{"items": items, "total": len(items)}, nil
}
func (e *PrivilegedExecutor) restoreManagedDatabase(ctx context.Context, instance Instance, record managedDatabaseRecord, id string) (any, error) {
	if record.Permission != "read_write" {
		return nil, failure("credential_scope", "A read-only database identity cannot restore data", 403)
	}
	if instance.PluginID != "mongodb" {
		if err := verifyManagedSQLCredential(ctx, instance, record); err != nil {
			return nil, err
		}
	}
	file, backup, err := e.openVerifiedDatabaseBackup(ctx, instance, record.Database, id)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if instance.PluginID == "mongodb" {
		err = e.restoreMongoDatabase(ctx, instance, record, file)
	} else {
		program, args, environment, cleanup, prepErr := e.databaseTool(ctx, instance, record, "restore")
		if prepErr != nil {
			return nil, prepErr
		}
		defer cleanup()
		runner, prepErr := agent.NewRunner(agent.Options{Operations: []agent.Operation{{Name: "database_restore", Program: program, BuildArgs: func(json.RawMessage) ([]string, error) { return args, nil }, Timeout: 30 * time.Minute, Environment: environment, Credential: e.credential(instance)}}, MaxOutputBytes: 32 << 10, MaxConcurrent: 1})
		if prepErr != nil {
			return nil, prepErr
		}
		result, runErr := runner.Stream(ctx, agent.Request{Operation: "database_restore", Input: json.RawMessage(`{}`)}, agent.IO{Stdin: file})
		if runErr != nil || result.ExitCode != 0 {
			err = failure("operation_recovery_required", "Database restore was interrupted or rejected; inspect the database before a new operation", 409)
		}
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"instance_id": instance.ID, "database": record.Database, "backup_id": id, "restored": true, "sha256": backup.SHA256}, nil
}

func verifyManagedSQLCredential(ctx context.Context, instance Instance, record managedDatabaseRecord) error {
	engine := instance.PluginID
	if engine == "mysql-community" {
		engine = "mysql"
	}
	profile := DatabaseConnection{Engine: engine, Host: managedDatabaseHost(instance), Port: setting(instance.Config, "port", 0), Username: record.Username, Database: record.Database, Access: "database"}
	db, err := openSQLProfile(ctx, profile, record.Credential, record.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	return verifySQLLeastPrivilege(ctx, db, profile)
}

func (e *PrivilegedExecutor) deleteDatabaseBackup(ctx context.Context, instance Instance, record managedDatabaseRecord, id string) (any, error) {
	var backup DatabaseBackup
	if err := e.load("private", id, &backup); err != nil {
		return nil, err
	}
	if backup.ID != id || backup.InstanceID != instance.ID || backup.Database != record.Database || backup.Engine != instance.PluginID || backup.Filename != id+".dump" {
		return nil, failure("forbidden", "Backup belongs to a different database or instance", 403)
	}
	if backup.State == "deleted" {
		return map[string]any{"backup_id": id, "deleted": true}, nil
	}
	file, backup, err := e.openVerifiedDatabaseBackup(ctx, instance, record.Database, id)
	if err != nil {
		return nil, err
	}
	file.Close()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	backup.State = "deleting"
	if err = e.save("private", id, backup); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(instance.Paths.BackupDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err = root.Remove(backup.Filename); err != nil {
		return nil, failure("operation_recovery_required", "Backup deletion was not fully acknowledged; inspect its owned receipt", 409)
	}
	directory, err := os.Open(instance.Paths.BackupDir)
	if err == nil {
		err = directory.Sync()
		directory.Close()
	}
	if err != nil {
		return nil, failure("operation_recovery_required", "Backup removal requires durability recovery", 409)
	}
	backup.State = "deleted"
	if err = e.save("private", id, backup); err != nil {
		return nil, failure("operation_recovery_required", "Backup was removed but its receipt commit failed", 409)
	}
	return map[string]any{"backup_id": id, "deleted": true}, nil
}
func (e *PrivilegedExecutor) databaseTool(ctx context.Context, instance Instance, record managedDatabaseRecord, command string) (program string, args []string, environment []string, cleanup func(), err error) {
	file, err := os.CreateTemp(instance.Paths.RunDir, ".database-client-")
	if err != nil {
		return "", nil, nil, func() {}, err
	}
	name := file.Name()
	cleanup = func() { os.Remove(name) }
	body := ""
	host := managedDatabaseHost(instance)
	port := setting(instance.Config, "port", 0)
	if instance.PluginID == "postgresql" {
		escape := func(v string) string { return strings.NewReplacer(`\`, `\\`, ":", `\:`).Replace(v) }
		body = strings.Join([]string{escape(host), strconv.Itoa(port), escape(record.Database), escape(record.Username), escape(record.Credential)}, ":") + "\n"
		environment = []string{"PGPASSFILE=" + name, "PGSSLMODE=disable", "PGCONNECT_TIMEOUT=5"}
		program = filepath.Join(filepath.Dir(instance.Service.Program), "pg_dump")
		args = []string{"--format=custom", "--no-owner", "--no-acl"}
		if command == "restore" {
			program = filepath.Join(filepath.Dir(instance.Service.Program), "pg_restore")
			args = []string{"--no-owner", "--no-acl", "--clean", "--if-exists", "--single-transaction", "--exit-on-error"}
		}
		args = append(args, "--host", host, "--port", strconv.Itoa(port), "--username", record.Username, "--no-password", "--dbname", record.Database)
	} else {
		body = "[client]\nuser=" + mysqlOptionQuote(record.Username) + "\npassword=" + mysqlOptionQuote(record.Credential) + "\nhost=" + mysqlOptionQuote(host) + "\nport=" + strconv.Itoa(port) + "\n"
		program = filepath.Join(filepath.Dir(instance.Service.Program), "mysqldump")
		if instance.PluginID == "mariadb" {
			program = "/usr/bin/mariadb-dump"
		}
		args = []string{"--defaults-file=" + name, "--single-transaction", "--skip-lock-tables", "--no-tablespaces", "--routines", "--events", "--triggers", "--hex-blob", record.Database}
		if command == "restore" {
			program = filepath.Join(filepath.Dir(instance.Service.Program), "mysql")
			if instance.PluginID == "mariadb" {
				program = "/usr/bin/mariadb"
			}
			args = []string{"--defaults-file=" + name, "--binary-mode", "--local-infile=0", "--default-character-set=utf8mb4", "--database=" + record.Database}
		}
	}
	if err = file.Chmod(0600); err == nil {
		_, err = file.WriteString(body)
	}
	if err == nil && os.Geteuid() == 0 {
		err = file.Chown(int(e.uid), int(e.gid))
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = e.validateDatabaseUtility(ctx, instance, program)
	}
	if err != nil {
		cleanup()
	}
	return
}
