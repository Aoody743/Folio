package plugins

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/go-sql-driver/mysql"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type ManagedDatabaseInput struct {
	InstanceID       string `json:"instance_id"`
	Database         string `json:"database,omitempty"`
	Username         string `json:"username,omitempty"`
	Command          string `json:"command,omitempty"`
	Permission       string `json:"permission,omitempty"`
	Password         string `json:"password,omitempty"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
	BackupID         string `json:"backup_id,omitempty"`
}
type ManagedDatabase struct {
	InstanceID string `json:"instance_id"`
	Database   string `json:"database"`
	Username   string `json:"username"`
	Engine     string `json:"engine"`
	Permission string `json:"permission"`
	State      string `json:"state"`
	Revision   int64  `json:"revision"`
	CreatedAt  string `json:"created_at"`
	Password   string `json:"-"`
}
type managedDatabaseRecord struct {
	ManagedDatabase
	Credential      string `json:"credential"`
	PermissionOwner string `json:"permission_owner,omitempty"`
}

func managedDatabaseKey(instance, database string) string {
	hash := sha256.Sum256([]byte(instance + "|" + database))
	return "dbuser_" + hex.EncodeToString(hash[:])
}
func randomDatabasePassword() string {
	data := make([]byte, 32)
	if _, e := rand.Read(data); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}
func validDatabasePermission(permission string) bool {
	return permission == "read" || permission == "data_write" || permission == "read_write"
}
func (e *PrivilegedExecutor) validateDatabase(raw json.RawMessage) error {
	var in ManagedDatabaseInput
	if err := parse(raw, &in); err != nil {
		return err
	}
	if !validInstanceID(in.InstanceID) {
		return failure("invalid_input", "Invalid managed database instance", 400)
	}
	switch in.Command {
	case "list", "diagnostics", "replication_status":
	case "create":
		if !validDatabaseName(in.Database) || (in.Username != "" && !validDatabaseName(in.Username)) || !validDatabasePermission(in.Permission) {
			return failure("invalid_input", "Invalid managed database/user/permission", 400)
		}
		if in.ExpectedRevision < 0 {
			return failure("invalid_input", "Expected database revision cannot be negative", 400)
		}
	case "delete", "credential", "receipt", "set_password", "set_permission", "backup", "backups_list", "backup_delete", "restore", "backup_stat":
		if !validDatabaseName(in.Database) {
			return failure("invalid_input", "A concrete database name is required", 400)
		}
		if (in.Command == "delete" || in.Command == "set_password" || in.Command == "set_permission") && in.ExpectedRevision < 1 {
			return failure("invalid_input", "Expected database revision is required", 400)
		}
		if in.Command == "set_permission" && !validDatabasePermission(in.Permission) {
			return failure("invalid_input", "Select read, data_write or read_write database permission", 400)
		}
		if in.Command == "set_password" && (len(in.Password) < 12 || len(in.Password) > 512 || strings.ContainsAny(in.Password, "\x00\n\r")) {
			return failure("invalid_input", "Database password must be 12–512 bytes without line breaks", 400)
		}
		if (in.Command == "restore" || in.Command == "backup_stat" || in.Command == "backup_delete") && !artifactName.MatchString(in.BackupID) {
			return failure("invalid_input", "Invalid owned backup receipt ID", 400)
		}
	default:
		return failure("invalid_input", "Unsupported typed database command", 400)
	}
	return nil
}
func (e *PrivilegedExecutor) database(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := e.validateDatabase(raw); err != nil {
		return nil, err
	}
	var in ManagedDatabaseInput
	json.Unmarshal(raw, &in)
	unlock, err := e.lockContext(ctx, in.InstanceID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if in.Command != "receipt" && in.Command != "list" && in.Command != "diagnostics" && in.Command != "replication_status" && in.Command != "backup_stat" {
		if err := e.ensureInstallFinalized(ctx, in.InstanceID); err != nil {
			return nil, err
		}
	}
	instance, err := e.get(ctx, in.InstanceID, false)
	if err != nil {
		return nil, err
	}
	switch instance.PluginID {
	case "mysql", "mysql-community", "mariadb", "postgresql", "mongodb":
	default:
		return nil, failure("runtime_type_mismatch", "Selected software is not a managed database", 412)
	}
	if in.Command == "receipt" {
		var record managedDatabaseRecord
		if err = e.load("private", managedDatabaseKey(instance.ID, in.Database), &record); err != nil {
			if core.ErrorCode(err) == "not_found" {
				return nil, failure("database_receipt_not_found", "Managed database receipt does not exist for this instance and database", 404)
			}
			return nil, err
		}
		if record.InstanceID != instance.ID || record.Database != in.Database || record.Engine != instance.PluginID {
			return nil, failure("ownership_invalid", "Database receipt does not match its instance", 412)
		}
		return record.ManagedDatabase, nil
	}
	if instance.Status == "uninstalled" {
		return nil, failure("runtime_unavailable", "Database software is uninstalled", 412)
	}
	if instance.PluginID == "mongodb" && in.Permission == "data_write" {
		return nil, failure("permission_unsupported", "The SQL data-only permission is not a MongoDB built-in role", 400)
	}
	if in.Command == "backup_stat" {
		return e.databaseBackupMetadata(ctx, instance, in.Database, in.BackupID)
	}
	if in.Command == "list" {
		return e.listManagedDatabases(ctx, instance)
	}
	if in.Command == "diagnostics" || in.Command == "replication_status" {
		return e.managedDiagnostics(ctx, instance, in.Command)
	}
	if in.Command == "create" {
		return e.createManagedDatabase(ctx, instance, in)
	}
	var record managedDatabaseRecord
	if err = e.load("private", managedDatabaseKey(instance.ID, in.Database), &record); err != nil {
		return nil, err
	}
	if record.InstanceID != instance.ID || record.Database != in.Database || record.Engine != instance.PluginID {
		return nil, failure("ownership_invalid", "Database receipt does not match its instance", 412)
	}
	if record.State != "active" {
		return nil, failure("operation_recovery_required", "Database operation is unfinished or deleted; inspect its owned receipt", 409)
	}
	switch in.Command {
	case "credential":
		return map[string]any{"database": record.Database, "username": record.Username, "password": record.Credential, "engine": record.Engine, "instance_id": record.InstanceID, "revision": record.Revision}, nil
	case "delete", "set_password", "set_permission":
		if record.Revision != in.ExpectedRevision {
			return nil, failure("revision_conflict", "Managed database changed; reload before editing", 412)
		}
		return e.mutateManagedDatabase(ctx, instance, record, in)
	case "backup":
		return e.backupManagedDatabase(ctx, instance, record)
	case "backups_list":
		return e.listDatabaseBackups(ctx, instance, record)
	case "backup_delete":
		return e.deleteDatabaseBackup(ctx, instance, record, in.BackupID)
	case "restore":
		return e.restoreManagedDatabase(ctx, instance, record, in.BackupID)
	}
	return nil, failure("invalid_input", "Unsupported database operation", 400)
}
func (e *PrivilegedExecutor) openManagedAdmin(ctx context.Context, instance Instance) (*sql.DB, error) {
	if err := e.ensureDatabaseAdminSettled(instance); err != nil {
		return nil, err
	}
	var credential managedDatabaseCredential
	if err := e.load("private", databasePrivateID(instance), &credential); err != nil || !credential.Initialized || credential.Engine != instance.PluginID || credential.DataDir != instance.Paths.DataDir {
		return nil, failure("credential_unavailable", "Owned database initialization receipt is unavailable", 412)
	}
	return e.openManagedAdminCredential(ctx, instance, credential)
}
func (e *PrivilegedExecutor) openManagedAdminCredential(ctx context.Context, instance Instance, credential managedDatabaseCredential) (*sql.DB, error) {
	if err := e.verifyDatabaseAdminCandidate(instance, credential); err != nil {
		return nil, err
	}
	if instance.PluginID == "postgresql" {
		return openSQLProfile(ctx, DatabaseConnection{Engine: "postgresql", Host: managedDatabaseHost(instance), Port: setting(instance.Config, "port", 0), Username: credential.Username}, credential.Password, "postgres")
	}
	config := mysql.NewConfig()
	config.User = credential.Username
	config.Passwd = credential.Password
	config.Net = "unix"
	config.Addr = instance.Paths.RunDir + "/mysql.sock"
	config.MultiStatements = false
	config.MaxAllowedPacket = 2 << 20
	config.Params = map[string]string{"sql_mode": "'NO_BACKSLASH_ESCAPES'"}
	config.Timeout = 5 * time.Second
	config.ReadTimeout = 10 * time.Second
	config.WriteTimeout = 10 * time.Second
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return nil, failure("connection_failed", "Owned MySQL connection could not be initialized", 502)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, failure("connection_failed", "Owned MySQL administrative authentication failed", 502)
	}
	return db, nil
}
func managedDatabaseHost(instance Instance) string {
	host, _ := instance.Config["bind"].(string)
	if host == "" || host == "0.0.0.0" {
		return "127.0.0.1"
	}
	if host == "::" {
		return "::1"
	}
	return host
}
func (e *PrivilegedExecutor) managedMongo(ctx context.Context, instance Instance) (*mongo.Client, error) {
	var credential managedDatabaseCredential
	if err := e.load("private", databasePrivateID(instance), &credential); err != nil || !credential.Initialized || credential.Engine != "mongodb" || credential.DataDir != instance.Paths.DataDir {
		return nil, failure("credential_unavailable", "Owned MongoDB initialization receipt is unavailable", 412)
	}
	client, err := mongo.Connect(options.Client().SetHosts([]string{net.JoinHostPort(managedDatabaseHost(instance), strconv.Itoa(setting(instance.Config, "port", 0)))}).SetDirect(true).SetAuth(options.Credential{AuthSource: "admin", Username: credential.Username, Password: credential.Password}).SetConnectTimeout(5 * time.Second).SetServerSelectionTimeout(5 * time.Second))
	if err != nil {
		return nil, failure("connection_failed", "Owned MongoDB connection could not be initialized", 502)
	}
	if err = client.Ping(ctx, nil); err != nil {
		client.Disconnect(context.WithoutCancel(ctx))
		return nil, failure("connection_failed", "Owned MongoDB administrative authentication failed", 502)
	}
	return client, nil
}
func quoteDatabaseIdentifier(engine, name string) string {
	if engine == "postgresql" {
		return `"` + name + `"`
	}
	return "`" + name + "`"
}
func (e *PrivilegedExecutor) createManagedDatabase(ctx context.Context, instance Instance, in ManagedDatabaseInput) (ManagedDatabase, error) {
	return e.createManagedDatabaseWithRaceHook(ctx, instance, in, nil)
}

// The optional hook lets isolated real-service tests place an external CREATE
// in the narrow window after authoritative absence and before our CREATE.
// Production callers always pass nil.
func (e *PrivilegedExecutor) createManagedDatabaseWithRaceHook(ctx context.Context, instance Instance, in ManagedDatabaseInput, afterClaim func() error) (ManagedDatabase, error) {
	var existing managedDatabaseRecord
	prior, err := e.loadManagedDatabaseCreationTombstone(instance, in)
	if err != nil {
		return ManagedDatabase{}, err
	}
	if prior != nil {
		existing = *prior
	}
	username := in.Username
	if username == "" {
		username = "pl_" + hex.EncodeToString(sha256Digest(instance.ID + "|" + in.Database)[:10])
	}
	if len(username) > 32 {
		return ManagedDatabase{}, failure("invalid_input", "Managed database usernames are limited to 32 bytes", 400)
	}
	revision := int64(1)
	if prior != nil {
		if prior.Revision == int64(^uint64(0)>>1) {
			return ManagedDatabase{}, failure("revision_exhausted", "Managed database revision cannot be advanced", 409)
		}
		revision = prior.Revision + 1
	}
	record := managedDatabaseRecord{ManagedDatabase: ManagedDatabase{InstanceID: instance.ID, Database: in.Database, Username: username, Engine: instance.PluginID, Permission: in.Permission, State: "creating", Revision: revision, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}, Credential: randomDatabasePassword()}
	if instance.PluginID == "mongodb" {
		// MongoDB's createUser command implicitly creates this database and has no
		// conditional absent-database operation. Tombstone recreation is blocked
		// above; first creation retains this pre-existing external-writer race as
		// an explicit ownership-fence follow-up rather than claiming atomicity.
		client, err := e.managedMongo(ctx, instance)
		if err != nil {
			return ManagedDatabase{}, err
		}
		defer client.Disconnect(context.WithoutCancel(ctx))
		var info struct {
			Users []bson.M `bson:"users"`
		}
		if err = client.Database(in.Database).RunCommand(ctx, bson.D{{Key: "usersInfo", Value: 1}}).Decode(&info); err != nil {
			return ManagedDatabase{}, failure("query_failed", "MongoDB user preflight failed", 502)
		}
		if len(info.Users) > 0 {
			return ManagedDatabase{}, failure("ownership_conflict", "Existing MongoDB user is not owned by this panel", 409)
		}
		dbs, err := client.ListDatabaseNames(ctx, bson.D{{Key: "name", Value: in.Database}})
		if err != nil {
			return ManagedDatabase{}, failure("query_failed", "MongoDB database preflight failed", 502)
		}
		if len(dbs) > 0 {
			return ManagedDatabase{}, failure("ownership_conflict", "Existing MongoDB data is not owned by this panel", 409)
		}
		if err = e.claimManagedDatabaseCreation(instance, in, prior, record); err != nil {
			return ManagedDatabase{}, err
		}
		if afterClaim != nil {
			if err = afterClaim(); err != nil {
				return ManagedDatabase{}, failure("operation_recovery_required", "External database creation raced with this request; the ownership receipt was preserved", 409)
			}
		}
		role := "read"
		if in.Permission == "read_write" {
			role = "readWrite"
		}
		err = client.Database(in.Database).RunCommand(ctx, bson.D{{Key: "createUser", Value: username}, {Key: "pwd", Value: record.Credential}, {Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: role}, {Key: "db", Value: in.Database}}}}}).Err()
		if err != nil {
			return ManagedDatabase{}, failure("operation_recovery_required", "MongoDB creation was not durably acknowledged; inspect preserved ownership receipt", 409)
		}
	} else {
		db, err := e.openManagedAdmin(ctx, instance)
		if err != nil {
			return ManagedDatabase{}, err
		}
		defer db.Close()
		var exists int
		if instance.PluginID == "postgresql" {
			err = db.QueryRowContext(ctx, "SELECT count(*) FROM pg_database WHERE datname=$1", in.Database).Scan(&exists)
		} else {
			err = db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.schemata WHERE schema_name=?", in.Database).Scan(&exists)
		}
		if err != nil {
			return ManagedDatabase{}, failure("query_failed", "Database existence preflight failed", 502)
		}
		if exists != 0 {
			return ManagedDatabase{}, failure("ownership_conflict", "Existing server database is not owned by this panel", 409)
		}
		rolesToCheck := []string{username}
		if prior != nil {
			rolesToCheck = appendUniqueName(rolesToCheck, existing.Username)
			rolesToCheck = appendUniqueName(rolesToCheck, existing.PermissionOwner)
		}
		for _, roleName := range rolesToCheck {
			if roleName == "" {
				continue
			}
			if instance.PluginID == "postgresql" {
				err = db.QueryRowContext(ctx, "SELECT count(*) FROM pg_roles WHERE rolname=$1", roleName).Scan(&exists)
			} else {
				err = db.QueryRowContext(ctx, "SELECT count(*) FROM mysql.user WHERE User=?", roleName).Scan(&exists)
			}
			if err != nil || exists != 0 {
				return ManagedDatabase{}, failure("ownership_conflict", "Existing or unverifiable database role is not owned by this panel", 409)
			}
		}
		if err = e.claimManagedDatabaseCreation(instance, in, prior, record); err != nil {
			return ManagedDatabase{}, err
		}
		if afterClaim != nil {
			if err = afterClaim(); err != nil {
				return ManagedDatabase{}, failure("operation_recovery_required", "External database creation raced with this request; the ownership receipt was preserved", 409)
			}
		}
		databaseID := quoteDatabaseIdentifier(instance.PluginID, in.Database)
		roleID := quoteDatabaseIdentifier(instance.PluginID, username)
		if instance.PluginID == "postgresql" {
			// Let the server's CREATE DATABASE uniqueness check win before creating
			// any login. If another actor won after preflight, its database remains
			// untouched and our durable creating receipt remains inspectable.
			_, err = db.ExecContext(ctx, "CREATE DATABASE "+databaseID)
			if err == nil {
				_, err = db.ExecContext(ctx, "CREATE ROLE "+roleID+" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS PASSWORD "+pgSQLLiteral(record.Credential))
			}
			if err == nil && in.Permission == "read_write" {
				_, err = db.ExecContext(ctx, "ALTER DATABASE "+databaseID+" OWNER TO "+roleID)
			}
			// Database ownership gives CREATE only in this database. Read-only roles
			// own no database and receive explicit data privileges instead.
			if err == nil && in.Permission != "read_write" {
				var root managedDatabaseCredential
				e.load("private", databasePrivateID(instance), &root)
				_, err = db.ExecContext(ctx, "ALTER DATABASE "+databaseID+" OWNER TO "+quoteDatabaseIdentifier("postgresql", root.Username))
			}
			if err == nil {
				_, err = db.ExecContext(ctx, "REVOKE ALL ON DATABASE "+databaseID+" FROM PUBLIC")
			}
			if err == nil {
				_, err = db.ExecContext(ctx, "GRANT CONNECT ON DATABASE "+databaseID+" TO "+roleID)
			}
			if err == nil {
				var root managedDatabaseCredential
				e.load("private", databasePrivateID(instance), &root)
				target, openErr := openSQLProfile(ctx, DatabaseConnection{Engine: "postgresql", Host: managedDatabaseHost(instance), Port: setting(instance.Config, "port", 0), Username: root.Username}, root.Password, in.Database)
				if openErr != nil {
					err = openErr
				} else {
					defer target.Close()
					_, err = target.ExecContext(ctx, "REVOKE CREATE ON SCHEMA public FROM PUBLIC")
					if err == nil {
						privilege := "USAGE"
						if in.Permission == "read_write" {
							privilege = "USAGE,CREATE"
						}
						_, err = target.ExecContext(ctx, "GRANT "+privilege+" ON SCHEMA public TO "+roleID)
					}
				}
			}
		} else {
			role := roleID + "@'127.0.0.1'"
			if managedDatabaseHost(instance) == "::1" {
				role = roleID + "@'::1'"
			}
			// Create the namespace first so a duplicate-name race cannot leave a
			// panel login that could be mistaken for ownership of that database.
			_, err = db.ExecContext(ctx, "CREATE DATABASE "+databaseID+" CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci")
			if err == nil {
				_, err = db.ExecContext(ctx, "CREATE USER "+role+" IDENTIFIED BY "+mysqlLiteral(record.Credential))
			}
			privilege := mysqlDatabasePermission(in.Permission)
			if err == nil {
				_, err = db.ExecContext(ctx, "GRANT "+privilege+" ON "+mysqlGrantIdentifier(ctx, db, in.Database)+".* TO "+role)
			}
		}
		if err != nil {
			return ManagedDatabase{}, failure("operation_recovery_required", "Database/user creation was not fully committed; inspect its preserved receipt", 409)
		}
	}
	if err = e.commitManagedDatabaseCreation(instance, record); err != nil {
		return ManagedDatabase{}, failure("operation_recovery_required", "Database exists but ownership receipt was not committed", 409)
	}
	return record.ManagedDatabase, nil
}

func (e *PrivilegedExecutor) loadManagedDatabaseCreationTombstone(instance Instance, in ManagedDatabaseInput) (*managedDatabaseRecord, error) {
	var existing managedDatabaseRecord
	err := e.load("private", managedDatabaseKey(instance.ID, in.Database), &existing)
	if core.ErrorCode(err) == "not_found" {
		if in.ExpectedRevision != 0 {
			return nil, failure("revision_conflict", "Managed database receipt changed; reload before creating", 412)
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if existing.InstanceID != instance.ID || existing.Database != in.Database || existing.Engine != instance.PluginID || existing.Revision < 1 {
		return nil, failure("ownership_invalid", "Database receipt does not match its instance", 412)
	}
	if existing.State != "deleted" {
		if in.ExpectedRevision > 0 && existing.Revision != in.ExpectedRevision {
			return nil, failure("revision_conflict", "Managed database changed; reload before creating", 412)
		}
		if existing.State == "creating" || existing.State == "deleting" || existing.State == "updating" {
			return nil, failure("operation_recovery_required", "Database operation is unfinished; inspect its owned receipt", 409)
		}
		return nil, failure("database_exists", "Managed database already has an ownership receipt", 409)
	}
	if existing.Revision != in.ExpectedRevision {
		return nil, failure("revision_conflict", "Managed database changed; reload before creating", 412)
	}
	if instance.PluginID == "mongodb" {
		return nil, failure("database_recreation_unsupported", "MongoDB cannot atomically create only an absent database; tombstone recreation is disabled until an ownership fence is available", 409)
	}
	return &existing, nil
}

// This compare-and-swap is protected by the per-instance lock held by
// database(). The exact tombstone revision is consumed once and advanced before
// any server write; fresh names use the zero-to-one transition.
func (e *PrivilegedExecutor) claimManagedDatabaseCreation(instance Instance, in ManagedDatabaseInput, prior *managedDatabaseRecord, next managedDatabaseRecord) error {
	key := managedDatabaseKey(instance.ID, in.Database)
	var current managedDatabaseRecord
	err := e.load("private", key, &current)
	if prior == nil {
		if core.ErrorCode(err) != "not_found" {
			if err != nil {
				return err
			}
			return failure("revision_conflict", "Managed database was created concurrently", 412)
		}
		if next.Revision != 1 {
			return failure("revision_conflict", "New database revision must begin at one", 412)
		}
	} else if err != nil || current.InstanceID != instance.ID || current.Database != in.Database || current.Engine != instance.PluginID || current.State != "deleted" || current.Revision != prior.Revision || current.Revision != in.ExpectedRevision || next.Revision != current.Revision+1 {
		return failure("revision_conflict", "Deleted database receipt changed; reload before recreating", 412)
	}
	if next.State != "creating" || next.InstanceID != instance.ID || next.Database != in.Database || next.Engine != instance.PluginID {
		return failure("ownership_invalid", "New database ownership receipt is invalid", 412)
	}
	return e.save("private", key, next)
}

func (e *PrivilegedExecutor) commitManagedDatabaseCreation(instance Instance, next managedDatabaseRecord) error {
	key := managedDatabaseKey(instance.ID, next.Database)
	var current managedDatabaseRecord
	if err := e.load("private", key, &current); err != nil {
		return err
	}
	if current.InstanceID != instance.ID || current.Database != next.Database || current.Engine != instance.PluginID || current.State != "creating" || current.Revision != next.Revision {
		return failure("revision_conflict", "Database creation receipt changed before commit", 412)
	}
	next.State = "active"
	return e.save("private", key, next)
}

func appendUniqueName(values []string, candidate string) []string {
	if candidate == "" {
		return values
	}
	for _, value := range values {
		if value == candidate {
			return values
		}
	}
	return append(values, candidate)
}
func sha256Digest(value string) []byte { hash := sha256.Sum256([]byte(value)); return hash[:] }
func (e *PrivilegedExecutor) mutateManagedDatabase(ctx context.Context, instance Instance, record managedDatabaseRecord, in ManagedDatabaseInput) (any, error) {
	old := record
	record.State = "updating"
	if in.Command == "delete" {
		record.State = "deleting"
	}
	if err := e.save("private", managedDatabaseKey(instance.ID, record.Database), record); err != nil {
		return nil, err
	}
	var err error
	if instance.PluginID == "mongodb" {
		client, openErr := e.managedMongo(ctx, instance)
		if openErr != nil {
			return nil, openErr
		}
		defer client.Disconnect(context.WithoutCancel(ctx))
		if in.Command == "delete" {
			err = client.Database(record.Database).Drop(ctx)
			if err == nil {
				err = client.Database(record.Database).RunCommand(ctx, bson.D{{Key: "dropUser", Value: record.Username}}).Err()
			}
		} else if in.Command == "set_permission" {
			role := "read"
			if in.Permission == "read_write" {
				role = "readWrite"
			}
			err = client.Database(record.Database).RunCommand(ctx, bson.D{{Key: "updateUser", Value: record.Username}, {Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: role}, {Key: "db", Value: record.Database}}}}}).Err()
		} else {
			err = client.Database(record.Database).RunCommand(ctx, bson.D{{Key: "updateUser", Value: record.Username}, {Key: "pwd", Value: in.Password}}).Err()
		}
	} else {
		db, openErr := e.openManagedAdmin(ctx, instance)
		if openErr != nil {
			return nil, openErr
		}
		defer db.Close()
		role := quoteDatabaseIdentifier(instance.PluginID, record.Username)
		if instance.PluginID != "postgresql" {
			host := "127.0.0.1"
			if managedDatabaseHost(instance) == "::1" {
				host = "::1"
			}
			role += "@'" + host + "'"
		}
		if in.Command == "delete" {
			_, err = db.ExecContext(ctx, "DROP DATABASE "+quoteDatabaseIdentifier(instance.PluginID, record.Database))
			if err == nil {
				_, err = db.ExecContext(ctx, "DROP "+map[bool]string{true: "ROLE ", false: "USER "}[instance.PluginID == "postgresql"]+role)
			}
			if err == nil && instance.PluginID == "postgresql" && record.PermissionOwner != "" {
				_, err = db.ExecContext(ctx, "DROP ROLE "+quoteDatabaseIdentifier("postgresql", record.PermissionOwner))
			}
		} else if in.Command == "set_permission" {
			err = e.setSQLDatabasePermission(ctx, instance, &record, in.Permission, db)
		} else if instance.PluginID == "postgresql" {
			_, err = db.ExecContext(ctx, "ALTER ROLE "+role+" PASSWORD "+pgSQLLiteral(in.Password))
		} else {
			_, err = db.ExecContext(ctx, "ALTER USER "+role+" IDENTIFIED BY "+mysqlLiteral(in.Password))
		}
	}
	if err != nil {
		return nil, failure("operation_recovery_required", "Database mutation was not fully acknowledged; inspect its preserved receipt", 409)
	}
	record.Revision = old.Revision + 1
	record.State = "active"
	if in.Command == "delete" {
		record.State = "deleted"
		record.Credential = ""
	} else if in.Command == "set_permission" {
		record.Permission = in.Permission
	} else {
		record.Credential = in.Password
	}
	if err = e.save("private", managedDatabaseKey(instance.ID, record.Database), record); err != nil {
		return nil, failure("operation_recovery_required", "Database mutation completed but receipt commit failed", 409)
	}
	return record.ManagedDatabase, nil
}
func (e *PrivilegedExecutor) listManagedDatabases(ctx context.Context, instance Instance) (any, error) {
	// The private receipt list is intersected with actual server databases/users.
	records, err := e.readManagedRecords(instance)
	if err != nil {
		return nil, err
	}
	items := []ManagedDatabase{}
	// A deleted receipt is the discoverable, non-secret ownership tombstone
	// needed for a caller to present the exact expected revision on recreation.
	// Active entries below still require proof in the live server catalog.
	for _, r := range records {
		if r.State == "deleted" {
			items = append(items, r.ManagedDatabase)
		}
	}
	if instance.PluginID == "mongodb" {
		client, err := e.managedMongo(ctx, instance)
		if err != nil {
			return nil, err
		}
		defer client.Disconnect(context.WithoutCancel(ctx))
		for _, r := range records {
			if r.State != "active" {
				continue
			}
			var info struct {
				Users []bson.M `bson:"users"`
			}
			if client.Database(r.Database).RunCommand(ctx, bson.D{{Key: "usersInfo", Value: r.Username}}).Decode(&info) == nil && len(info.Users) == 1 {
				items = append(items, r.ManagedDatabase)
			}
		}
	} else {
		db, err := e.openManagedAdmin(ctx, instance)
		if err != nil {
			return nil, err
		}
		defer db.Close()
		for _, r := range records {
			if r.State != "active" {
				continue
			}
			var count int
			query := "SELECT count(*) FROM information_schema.schemata WHERE schema_name=?"
			if instance.PluginID == "postgresql" {
				query = "SELECT count(*) FROM pg_database WHERE datname=$1"
			}
			if db.QueryRowContext(ctx, query, r.Database).Scan(&count) == nil && count == 1 {
				items = append(items, r.ManagedDatabase)
			}
		}
	}
	return map[string]any{"items": items, "total": len(items)}, nil
}
func managedDatabaseResources(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var in ManagedDatabaseInput
	if json.Unmarshal(raw, &in) != nil || !validInstanceID(in.InstanceID) {
		return nil, failure("invalid_input", "Invalid managed database instance", 400)
	}
	return []string{DatabaseResource(in.InstanceID, in.Database)}, nil
}
func (m *Manager) managedDatabaseBindings() []core.Binding {
	schema := func() map[string]any {
		return map[string]any{"instance_id": idSchema(), "database": map[string]any{"type": "string", "pattern": databaseIdentifier.String()}, "username": map[string]any{"type": "string", "pattern": databaseIdentifier.String()}, "permission": enumSchema("read", "data_write", "read_write"), "password": map[string]any{"type": "string", "minLength": 12, "maxLength": 512, "format": "password"}, "expected_revision": revisionSchema(), "backup_id": map[string]any{"type": "string", "pattern": artifactName.String()}}
	}
	specs := []struct {
		Name, Command, Scope string
		Read, Task           bool
		Fields               []string
		Required             []string
	}{
		{"managed_databases_list", "list", "databases:read", true, false, []string{"instance_id"}, []string{"instance_id"}},
		{"managed_database_receipt", "receipt", "databases:read", true, false, []string{"instance_id", "database"}, []string{"instance_id", "database"}},
		{"managed_database_create", "create", "databases:admin", false, false, []string{"instance_id", "database", "username", "permission", "expected_revision"}, []string{"instance_id", "database", "permission"}},
		{"managed_database_delete", "delete", "databases:delete", false, true, []string{"instance_id", "database", "expected_revision"}, []string{"instance_id", "database", "expected_revision"}},
		{"managed_database_credentials", "credential", "databases:credentials", false, false, []string{"instance_id", "database"}, []string{"instance_id", "database"}},
		{"managed_database_set_password", "set_password", "databases:credentials", false, false, []string{"instance_id", "database", "password", "expected_revision"}, []string{"instance_id", "database", "password", "expected_revision"}},
		{"managed_database_set_permission", "set_permission", "databases:admin", false, false, []string{"instance_id", "database", "permission", "expected_revision"}, []string{"instance_id", "database", "permission", "expected_revision"}},
		{"managed_database_backup", "backup", "databases:backup", false, true, []string{"instance_id", "database"}, []string{"instance_id", "database"}},
		{"managed_database_backups_list", "backups_list", "databases:backup", true, false, []string{"instance_id", "database"}, []string{"instance_id", "database"}},
		{"managed_database_backup_delete", "backup_delete", "databases:backup", false, false, []string{"instance_id", "database", "backup_id"}, []string{"instance_id", "database", "backup_id"}},
		{"managed_database_restore", "restore", "databases:restore", false, true, []string{"instance_id", "database", "backup_id"}, []string{"instance_id", "database", "backup_id"}},
		{"managed_database_diagnostics", "diagnostics", "databases:admin", true, false, []string{"instance_id"}, []string{"instance_id"}},
		{"managed_database_replication_status", "replication_status", "databases:replication", true, false, []string{"instance_id"}, []string{"instance_id"}},
	}
	bindings := []core.Binding{}
	for _, spec := range specs {
		spec := spec
		fields := map[string]any{}
		all := schema()
		for _, name := range spec.Fields {
			fields[name] = all[name]
		}
		description := "Operate on an owned database using typed commands and dedicated database-only identities"
		if spec.Command == "receipt" {
			description = "Read the public ownership state and revision of a managed database without returning credentials"
		}
		binding := m.binding(spec.Name, description, spec.Scope, spec.Read, obj(fields, spec.Required...), managedDatabaseResources, func(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			var input ManagedDatabaseInput
			if err := parse(raw, &input); err != nil {
				return core.ActionResult{}, err
			}
			input.Command = spec.Command
			forward, _ := json.Marshal(input)
			resources, err := managedDatabaseResources(ctx, a, raw)
			if err != nil {
				return core.ActionResult{}, err
			}
			if spec.Task {
				job, err := m.deps.Tasks.Submit(ctx, a, "database_action", forward, resources)
				if err != nil {
					return core.ActionResult{}, err
				}
				id, _ := job["id"].(string)
				return core.ActionResult{Data: job, TaskID: id, Resources: resources}, nil
			}
			var output any
			if err = m.agentJSON(ctx, "database_action", forward, &output); err != nil {
				return core.ActionResult{}, err
			}
			if spec.Command == "credential" {
				return m.bindManagedDatabaseCredential(ctx, a, input, output)
			}
			if spec.Command == "create" || spec.Command == "set_password" {
				credentialInput := input
				credentialInput.Command = "credential"
				credentialInput.Password = ""
				credentialRaw, _ := json.Marshal(credentialInput)
				var credential any
				if err = m.agentJSON(ctx, "database_action", credentialRaw, &credential); err != nil {
					return core.ActionResult{}, err
				}
				if _, err = m.bindManagedDatabaseCredential(ctx, a, credentialInput, credential); err != nil {
					return core.ActionResult{}, err
				}
			}
			return core.ActionResult{Data: output, Resources: resources}, nil
		})
		binding.Action.Task = spec.Task
		bindings = append(bindings, binding)
	}
	return append(bindings, m.databaseDownloadBindings()...)
}
func (m *Manager) bindManagedDatabaseCredential(ctx context.Context, a core.Actor, in ManagedDatabaseInput, output any) (core.ActionResult, error) {
	data, ok := output.(map[string]any)
	if !ok {
		return core.ActionResult{}, failure("agent_protocol_error", "Database credential export was invalid", 502)
	}
	password, _ := data["password"].(string)
	username, _ := data["username"].(string)
	engine, _ := data["engine"].(string)
	if password == "" || username == "" {
		return core.ActionResult{}, failure("agent_protocol_error", "Database credential export was incomplete", 502)
	}
	resource := DatabaseResource(in.InstanceID, in.Database)
	ref, err := m.deps.Secrets.Put(ctx, a, resource, "managed database credential", []byte(password))
	data["password"] = ""
	delete(data, "password")
	if err != nil {
		return core.ActionResult{}, err
	}
	hash := sha256.Sum256([]byte(in.InstanceID + "|" + in.Database))
	profileID := "managed_" + hex.EncodeToString(hash[:16])
	var instance Instance
	if err = m.agentJSON(ctx, "plugin_get", json.RawMessage(jsonTextPlugin(map[string]any{"id": in.InstanceID})), &instance); err != nil {
		m.deps.Secrets.Delete(ctx, a, resource, ref)
		return core.ActionResult{}, err
	}
	p := DatabaseConnection{ID: profileID, InstanceID: in.InstanceID, Name: in.Database, Engine: engine, Host: managedDatabaseHost(instance), Port: setting(instance.Config, "port", 0), Database: in.Database, Username: username, Access: "database", CredentialRef: ref, CreatedAt: time.Now().UTC()}
	if engine == "mysql-community" {
		p.Engine = "mysql"
	}
	existing, err := m.databaseProfile(ctx, profileID)
	expected := int64(0)
	if err == nil {
		expected = existing.Revision
		p.CreatedAt = existing.CreatedAt
	} else if core.ErrorCode(err) != "not_found" {
		m.deps.Secrets.Delete(ctx, a, resource, ref)
		return core.ActionResult{}, err
	}
	p.Revision = expected + 1
	raw, _ := json.Marshal(p)
	rev, err := m.deps.State.CompareAndSwap(ctx, "database_connections", profileID, expected, raw)
	if err != nil {
		m.deps.Secrets.Delete(ctx, a, resource, ref)
		return core.ActionResult{}, err
	}
	p.Revision = rev
	if existing.CredentialRef != "" {
		_ = m.deps.Secrets.Delete(ctx, a, resource, existing.CredentialRef)
	}
	return core.ActionResult{Data: p, Resources: []string{resource}}, nil
}
func jsonTextPlugin(value any) string { raw, _ := json.Marshal(value); return string(raw) }

// Administrative connectors set NO_BACKSLASH_ESCAPES before interpreting DDL.
func mysqlLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
