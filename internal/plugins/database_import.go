package plugins

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/go-sql-driver/mysql"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	maxDatabaseImportChunkBytes  = 32 << 10
	maxDatabaseImportChunkBase64 = 44 << 10
	maxDatabaseImportStatement   = 16 << 20
)

const postgresImportHasObjectsSQL = `SELECT EXISTS (
	SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%'
	UNION ALL SELECT 1 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%'
	UNION ALL SELECT 1 FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%'
	UNION ALL SELECT 1 FROM pg_catalog.pg_namespace n WHERE n.nspname NOT IN ('pg_catalog','information_schema','public') AND n.nspname NOT LIKE 'pg_toast%'
	UNION ALL SELECT 1 FROM pg_catalog.pg_extension e JOIN pg_catalog.pg_namespace n ON n.oid=e.extnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%'
	UNION ALL SELECT 1 FROM pg_catalog.pg_collation c JOIN pg_catalog.pg_namespace n ON n.oid=c.collnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%'
	UNION ALL SELECT 1 FROM pg_catalog.pg_foreign_server
)`

var databaseImportIDPattern = regexp.MustCompile(`^dbimport_[a-f0-9]{32}$`)

// DatabaseImport is safe to return to API clients. It contains no path,
// credential or raw imported content.
type DatabaseImport struct {
	ID               string `json:"id"`
	InstanceID       string `json:"instance_id"`
	Database         string `json:"database"`
	Engine           string `json:"engine"`
	State            string `json:"state"`
	Bytes            int64  `json:"bytes"`
	BytesReceived    int64  `json:"bytes_received"`
	SHA256           string `json:"sha256,omitempty"`
	DatabaseRevision int64  `json:"database_revision"`
	Revision         int64  `json:"revision"`
	CreatedAt        string `json:"created_at"`
}

// databaseImportReceipt is stored only in encrypted Agent-private state. An
// external upload has a distinct ID prefix and receipt type from DatabaseBackup.
type databaseImportReceipt struct {
	ID               string `json:"id"`
	InstanceID       string `json:"instance_id"`
	Database         string `json:"database"`
	Engine           string `json:"engine"`
	Filename         string `json:"filename"`
	ExpectedBytes    int64  `json:"expected_bytes"`
	BytesReceived    int64  `json:"bytes_received"`
	ExpectedSHA256   string `json:"expected_sha256"`
	SHA256           string `json:"sha256,omitempty"`
	DatabaseRevision int64  `json:"database_revision"`
	Revision         int64  `json:"revision"`
	State            string `json:"state"`
	CreatedAt        string `json:"created_at"`
}

type databaseImportStreamInput struct {
	InstanceID       string `json:"instance_id"`
	Database         string `json:"database"`
	ImportID         string `json:"import_id"`
	ExpectedBytes    int64  `json:"expected_bytes"`
	ExpectedSHA256   string `json:"expected_sha256"`
	DatabaseRevision int64  `json:"database_revision"`
}

type databaseImportBodyKey struct{}
type databaseImportBody struct{ io.ReadCloser }

func validDatabaseImportID(id string) bool { return databaseImportIDPattern.MatchString(id) }

func validDatabaseImportSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func newDatabaseImportID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "dbimport_" + hex.EncodeToString(random), nil
}

func (r databaseImportReceipt) public() DatabaseImport {
	result := DatabaseImport{ID: r.ID, InstanceID: r.InstanceID, Database: r.Database, Engine: r.Engine, State: r.State, Bytes: r.ExpectedBytes, BytesReceived: r.BytesReceived, DatabaseRevision: r.DatabaseRevision, Revision: r.Revision, CreatedAt: r.CreatedAt}
	if r.State == "uploaded" || r.State == "applying" || r.State == "applied" || r.State == "recovery_required" {
		result.SHA256 = r.SHA256
	}
	return result
}

func databaseImportTempName(id string) string { return "." + id + ".uploading" }

func (e *PrivilegedExecutor) loadDatabaseImport(instance Instance, database, id string) (databaseImportReceipt, error) {
	var receipt databaseImportReceipt
	if !validDatabaseImportID(id) {
		return receipt, failure("invalid_input", "Invalid external database import ID", 400)
	}
	if err := e.load("private", id, &receipt); err != nil {
		return receipt, err
	}
	if receipt.ID != id || receipt.InstanceID != instance.ID || receipt.Database != database || receipt.Engine != instance.PluginID || receipt.Filename != id+".external" {
		return receipt, failure("forbidden", "Import artifact belongs to a different database or instance", 403)
	}
	if receipt.DatabaseRevision < 1 || receipt.Revision < 1 || receipt.ExpectedBytes < 1 || receipt.ExpectedBytes > maxExtractedBytes || !validDatabaseImportSHA256(receipt.ExpectedSHA256) || receipt.BytesReceived < 0 || receipt.BytesReceived > receipt.ExpectedBytes {
		return receipt, failure("ownership_invalid", "Import receipt does not match its protected artifact", 412)
	}
	switch receipt.State {
	case "uploading", "uploaded", "applying", "applied", "invalid", "recovery_required", "deleted":
	default:
		return receipt, failure("ownership_invalid", "Import receipt has an unknown state", 412)
	}
	return receipt, nil
}

func (e *PrivilegedExecutor) beginManagedDatabaseImport(ctx context.Context, instance Instance, record managedDatabaseRecord, in ManagedDatabaseInput) (any, error) {
	if record.Permission != "read_write" || record.Revision != in.ExpectedRevision {
		return nil, failure("revision_conflict", "Import requires the current read/write database receipt", 412)
	}
	if in.ImportBytes < 1 || in.ImportBytes > maxExtractedBytes || !validDatabaseImportSHA256(in.ImportSHA256) {
		return nil, failure("invalid_input", "Import size or SHA-256 is invalid", 400)
	}
	id, err := newDatabaseImportID()
	if err != nil {
		return nil, err
	}
	if err = realDirectory(instance.Paths.BackupDir); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(instance.Paths.BackupDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.OpenFile(databaseImportTempName(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, failure("artifact_exists", "Import upload could not be initialized", 409)
	}
	if err = file.Sync(); err == nil {
		err = file.Close()
	} else {
		file.Close()
	}
	if err != nil {
		root.Remove(databaseImportTempName(id))
		return nil, err
	}
	if err = syncDatabaseImportDirectory(instance.Paths.BackupDir); err != nil {
		root.Remove(databaseImportTempName(id))
		return nil, err
	}
	receipt := databaseImportReceipt{ID: id, InstanceID: instance.ID, Database: record.Database, Engine: instance.PluginID, Filename: id + ".external", ExpectedBytes: in.ImportBytes, ExpectedSHA256: strings.ToLower(in.ImportSHA256), DatabaseRevision: record.Revision, Revision: 1, State: "uploading", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = e.save("private", id, receipt); err != nil {
		root.Remove(databaseImportTempName(id))
		_ = syncDatabaseImportDirectory(instance.Paths.BackupDir)
		return nil, err
	}
	return receipt.public(), nil
}

func syncDatabaseImportDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err == nil {
		err = closeErr
	}
	return err
}

func (e *PrivilegedExecutor) appendManagedDatabaseImportChunk(ctx context.Context, instance Instance, target managedDatabaseRecord, in ManagedDatabaseInput) (any, error) {
	receipt, err := e.loadDatabaseImport(instance, target.Database, in.ImportID)
	if err != nil {
		return nil, err
	}
	if receipt.State != "uploading" || receipt.DatabaseRevision != in.ExpectedRevision || receipt.Revision != in.ImportRevision || in.ImportOffset != receipt.BytesReceived {
		return nil, failure("revision_conflict", "Import upload changed; inspect its current receipt before retrying", 412)
	}
	chunk, err := base64.StdEncoding.DecodeString(in.ImportChunk)
	if err != nil || len(chunk) == 0 || len(chunk) > maxDatabaseImportChunkBytes || int64(len(chunk)) > receipt.ExpectedBytes-in.ImportOffset {
		return nil, failure("invalid_input", "Database import chunk is empty, oversized or exceeds the declared artifact size", 400)
	}
	nextOffset := in.ImportOffset + int64(len(chunk))
	if in.ImportFinal != (nextOffset == receipt.ExpectedBytes) {
		return nil, failure("invalid_input", "The final import chunk must end exactly at the declared size", 400)
	}
	if err = realDirectory(instance.Paths.BackupDir); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(instance.Paths.BackupDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	temporary := databaseImportTempName(receipt.ID)
	if in.ImportFinal {
		if info, statErr := root.Lstat(receipt.Filename); statErr == nil {
			// A previous final chunk may have been durably renamed immediately
			// before its receipt update failed. Reconcile that exact commit by the
			// full declared digest; never append to or replace the final artifact.
			if !info.Mode().IsRegular() || info.Size() != receipt.ExpectedBytes {
				return nil, failure("operation_recovery_required", "Import final artifact conflicts with its upload receipt", 409)
			}
			file, openErr := root.OpenFile(receipt.Filename, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
			if openErr != nil {
				return nil, failure("operation_recovery_required", "Import final artifact cannot be reconciled", 409)
			}
			hasher := sha256.New()
			count, hashErr := io.Copy(hasher, &contextReader{ctx: ctx, reader: file})
			file.Close()
			if hashErr != nil || count != receipt.ExpectedBytes || hex.EncodeToString(hasher.Sum(nil)) != receipt.ExpectedSHA256 {
				return nil, failure("operation_recovery_required", "Import final artifact failed receipt reconciliation", 409)
			}
			receipt.BytesReceived = count
			receipt.SHA256 = receipt.ExpectedSHA256
			receipt.State = "uploaded"
			receipt.Revision++
			if err = e.save("private", receipt.ID, receipt); err != nil {
				return nil, failure("operation_recovery_required", "Import file is complete but its receipt remains uncommitted", 409)
			}
			return receipt.public(), nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
	}
	file, err := root.OpenFile(temporary, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, failure("operation_recovery_required", "Import upload staging file is missing", 409)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, failure("operation_recovery_required", "Import upload staging file is invalid", 409)
	}
	if info.Size() == in.ImportOffset {
		written, writeErr := file.WriteAt(chunk, in.ImportOffset)
		if writeErr == nil && written != len(chunk) {
			writeErr = io.ErrShortWrite
		}
		if writeErr == nil {
			writeErr = file.Sync()
		}
		if writeErr != nil {
			file.Close()
			return nil, writeErr
		}
	} else if info.Size() == nextOffset {
		prior := make([]byte, len(chunk))
		if _, err = file.ReadAt(prior, in.ImportOffset); err != nil || !equalDatabaseImportBytes(prior, chunk) {
			file.Close()
			return nil, failure("operation_recovery_required", "Import upload has an uncommitted conflicting chunk", 409)
		}
	} else {
		file.Close()
		return nil, failure("operation_recovery_required", "Import upload size differs from its revision receipt", 409)
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	receipt.BytesReceived = nextOffset
	receipt.Revision++
	if !in.ImportFinal {
		if err = e.save("private", receipt.ID, receipt); err != nil {
			return nil, failure("operation_recovery_required", "Import chunk was written but its progress receipt was not committed", 409)
		}
		return receipt.public(), nil
	}
	hasher := sha256.New()
	verifyFile, err := root.OpenFile(temporary, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, failure("operation_recovery_required", "Completed import upload could not be verified", 409)
	}
	count, hashErr := io.Copy(hasher, &contextReader{ctx: ctx, reader: verifyFile})
	verifyFile.Close()
	if hashErr != nil || count != receipt.ExpectedBytes {
		return nil, failure("operation_recovery_required", "Completed import upload could not be read consistently", 409)
	}
	receipt.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	if receipt.SHA256 != receipt.ExpectedSHA256 {
		receipt.State = "invalid"
		receipt.Revision++
		if err = e.save("private", receipt.ID, receipt); err != nil {
			return nil, failure("operation_recovery_required", "Import digest mismatch was detected but its receipt could not be saved", 409)
		}
		return nil, failure("checksum_mismatch", "Completed import upload does not match its declared SHA-256", 412)
	}
	if err = root.Rename(temporary, receipt.Filename); err != nil {
		return nil, failure("operation_recovery_required", "Verified import upload could not be committed", 409)
	}
	if err = syncDatabaseImportDirectory(instance.Paths.BackupDir); err != nil {
		return nil, failure("operation_recovery_required", "Verified import upload needs durability recovery", 409)
	}
	receipt.State = "uploaded"
	receipt.Revision++
	if err = e.save("private", receipt.ID, receipt); err != nil {
		return nil, failure("operation_recovery_required", "Import file is complete but its receipt remains uncommitted", 409)
	}
	return receipt.public(), nil
}

func equalDatabaseImportBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var diff byte
	for i := range left {
		diff |= left[i] ^ right[i]
	}
	return diff == 0
}

func (e *PrivilegedExecutor) listManagedDatabaseImports(instance Instance, record managedDatabaseRecord) (any, error) {
	entries, err := os.ReadDir(filepath.Join(e.config.StateDir, "private"))
	if err != nil {
		return nil, err
	}
	items := []DatabaseImport{}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "dbimport_") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		imported, loadErr := e.loadDatabaseImport(instance, record.Database, id)
		if core.ErrorCode(loadErr) == "forbidden" {
			continue
		}
		if loadErr != nil {
			return nil, loadErr
		}
		if imported.State == "deleted" {
			continue
		}
		items = append(items, imported.public())
		if len(items) > 10000 {
			return nil, failure("result_limit", "Database import list exceeds its bound", 413)
		}
	}
	return map[string]any{"items": items, "total": len(items)}, nil
}

func (e *PrivilegedExecutor) deleteManagedDatabaseImport(ctx context.Context, instance Instance, record managedDatabaseRecord, id string, expectedRevision int64) (any, error) {
	if record.InstanceID != instance.ID || record.Engine != instance.PluginID || record.State != "active" {
		return nil, failure("ownership_invalid", "Import deletion requires the current active database receipt", 412)
	}
	imported, err := e.loadDatabaseImport(instance, record.Database, id)
	if err != nil {
		return nil, err
	}
	if imported.Revision != expectedRevision {
		return nil, failure("revision_conflict", "Import changed; reload before deleting it", 412)
	}
	if imported.State == "deleted" {
		return imported.public(), nil
	}
	if imported.State == "applying" {
		return nil, failure("operation_recovery_required", "An import is applying; inspect its task before deleting the artifact", 409)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(instance.Paths.BackupDir)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{imported.Filename, databaseImportTempName(imported.ID)} {
		err = root.Remove(name)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			root.Close()
			return nil, failure("operation_recovery_required", "Import file removal was incomplete; inspect its receipt", 409)
		}
	}
	root.Close()
	if err = syncDatabaseImportDirectory(instance.Paths.BackupDir); err != nil {
		return nil, failure("operation_recovery_required", "Import file removal needs durability recovery", 409)
	}
	imported.State = "deleted"
	imported.Revision++
	if err = e.save("private", imported.ID, imported); err != nil {
		return nil, failure("operation_recovery_required", "Import was removed but its receipt commit failed", 409)
	}
	return imported.public(), nil
}

func (e *PrivilegedExecutor) recoverManagedDatabaseImport(instance Instance, record managedDatabaseRecord, id string, expectedRevision int64) (any, error) {
	if record.InstanceID != instance.ID || record.Engine != instance.PluginID || record.State != "active" {
		return nil, failure("ownership_invalid", "Import recovery requires the current active database receipt", 412)
	}
	imported, err := e.loadDatabaseImport(instance, record.Database, id)
	if err != nil {
		return nil, err
	}
	if imported.Revision != expectedRevision {
		return nil, failure("revision_conflict", "Import changed; reload before recovery", 412)
	}
	if imported.State == "recovery_required" {
		return imported.public(), nil
	}
	if imported.State != "applying" {
		return nil, failure("invalid_transition", "Only an import left applying by an interrupted task can be resolved", 409)
	}
	imported.State = "recovery_required"
	imported.Revision++
	if err = e.save("private", imported.ID, imported); err != nil {
		return nil, failure("operation_recovery_required", "Import recovery state could not be committed", 409)
	}
	return imported.public(), nil
}

func (e *PrivilegedExecutor) openVerifiedDatabaseImport(ctx context.Context, instance Instance, receipt databaseImportReceipt) (*os.File, error) {
	if receipt.State != "uploaded" || receipt.BytesReceived != receipt.ExpectedBytes || receipt.SHA256 != receipt.ExpectedSHA256 {
		return nil, failure("operation_recovery_required", "Import is incomplete or requires recovery", 409)
	}
	root, err := os.OpenRoot(instance.Paths.BackupDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.OpenFile(receipt.Filename, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, failure("artifact_invalid", "Import file cannot be opened safely", 412)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != receipt.ExpectedBytes {
		file.Close()
		return nil, failure("artifact_invalid", "Import file differs from its protected receipt", 412)
	}
	hash := sha256.New()
	count, err := io.Copy(hash, &contextReader{ctx: ctx, reader: file})
	if err != nil {
		file.Close()
		return nil, err
	}
	if count != receipt.ExpectedBytes || hex.EncodeToString(hash.Sum(nil)) != receipt.SHA256 {
		file.Close()
		return nil, failure("checksum_mismatch", "Import integrity verification failed", 412)
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

type preparedDatabaseImport struct {
	Reader   *os.File
	Postgres *preparedPostgresJSONImport
	Cleanup  func()
}

func (e *PrivilegedExecutor) prepareDatabaseImport(ctx context.Context, instance Instance, record managedDatabaseRecord, source *os.File) (preparedDatabaseImport, error) {
	prepared := preparedDatabaseImport{Reader: source, Cleanup: func() {}}
	switch instance.PluginID {
	case "postgresql":
		document, err := parsePostgresJSONImport(ctx, source)
		if err != nil {
			return preparedDatabaseImport{}, err
		}
		prepared.Postgres = document
	case "mysql", "mysql-community", "mariadb":
		file, err := e.prepareMySQLImportSQL(ctx, instance, source)
		if err != nil {
			return preparedDatabaseImport{}, err
		}
		prepared.Reader = file
		prepared.Cleanup = func() { name := file.Name(); file.Close(); _ = os.Remove(name) }
	case "mongodb":
		if err := validateMongoImportArtifact(ctx, source); err != nil {
			return preparedDatabaseImport{}, err
		}
	default:
		return preparedDatabaseImport{}, failure("runtime_type_mismatch", "External imports are not supported for this database engine", 412)
	}
	return prepared, nil
}

func (e *PrivilegedExecutor) verifyManagedDatabaseImportTargetEmpty(ctx context.Context, instance Instance, record managedDatabaseRecord) error {
	if record.Permission != "read_write" || record.State != "active" || record.Engine != instance.PluginID {
		return failure("credential_scope", "Import requires an active database-only read/write identity", 403)
	}
	if instance.PluginID == "mongodb" {
		profile := managedMongoProfile(instance, record)
		client, err := openMongoProfile(ctx, profile, record.Credential)
		if err != nil {
			return err
		}
		defer client.Disconnect(context.WithoutCancel(ctx))
		if err = verifyMongoLeastPrivilege(ctx, client, profile); err != nil {
			return err
		}
		names, err := client.Database(record.Database).ListCollectionNames(ctx, bson.D{})
		if err != nil {
			return failure("import_preflight_failed", "MongoDB target namespace could not be checked", 502)
		}
		if len(names) != 0 {
			return failure("import_target_not_empty", "External imports require an empty managed database; existing collections are preserved", 409)
		}
		return nil
	}
	profile := DatabaseConnection{Engine: instance.PluginID, Host: managedDatabaseHost(instance), Port: setting(instance.Config, "port", 0), Database: record.Database, Username: record.Username}
	db, err := openSQLProfile(ctx, profile, record.Credential, record.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	if err = verifySQLLeastPrivilege(ctx, db, profile); err != nil {
		return err
	}
	var hasObjects bool
	if instance.PluginID == "postgresql" {
		err = db.QueryRowContext(ctx, postgresImportHasObjectsSQL).Scan(&hasObjects)
	} else {
		err = db.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM information_schema.tables WHERE table_schema=?
			UNION ALL SELECT 1 FROM information_schema.routines WHERE routine_schema=?
			UNION ALL SELECT 1 FROM information_schema.triggers WHERE trigger_schema=?
			UNION ALL SELECT 1 FROM information_schema.events WHERE event_schema=?
		)`, record.Database, record.Database, record.Database, record.Database).Scan(&hasObjects)
	}
	if err != nil {
		return failure("import_preflight_failed", "Database target namespace could not be checked", 502)
	}
	if hasObjects {
		return failure("import_target_not_empty", "External imports require an empty managed database; existing tables and views are preserved", 409)
	}
	return nil
}

func (e *PrivilegedExecutor) prepareMySQLImportSQL(ctx context.Context, instance Instance, source *os.File) (*os.File, error) {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if err := realDirectory(instance.Paths.RunDir); err != nil {
		return nil, err
	}
	output, err := os.CreateTemp(instance.Paths.RunDir, ".database-import-sql-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { name := output.Name(); output.Close(); _ = os.Remove(name) }
	if err = output.Chmod(0600); err == nil && os.Geteuid() == 0 {
		err = output.Chown(int(e.uid), int(e.gid))
	}
	if err == nil {
		err = sanitizeMySQLImport(ctx, source, output)
	}
	if err == nil {
		err = output.Sync()
	}
	if err != nil {
		cleanup()
		return nil, err
	}
	if _, err = output.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, err
	}
	return output, nil
}

func (e *PrivilegedExecutor) restoreManagedDatabaseFile(ctx context.Context, instance Instance, record managedDatabaseRecord, source *os.File, postgres *preparedPostgresJSONImport) error {
	if instance.PluginID == "mongodb" {
		return e.restoreMongoImportDatabase(ctx, instance, record, source)
	}
	if err := e.verifyManagedDatabaseImportTargetEmpty(ctx, instance, record); err != nil {
		return err
	}
	if instance.PluginID == "postgresql" {
		return e.restorePostgresJSONImport(ctx, instance, record, postgres)
	}
	switch instance.PluginID {
	case "mysql", "mysql-community", "mariadb":
		return e.restoreMySQLImport(ctx, instance, record, source)
	default:
		return failure("runtime_type_mismatch", "External imports are not supported for this database engine", 412)
	}
}

func (e *PrivilegedExecutor) restoreMySQLImport(ctx context.Context, instance Instance, record managedDatabaseRecord, source *os.File) error {
	profile := DatabaseConnection{Engine: instance.PluginID, Host: managedDatabaseHost(instance), Port: setting(instance.Config, "port", 0), Database: record.Database, Username: record.Username}
	db, err := openMySQLImportProfile(ctx, profile, record.Credential)
	if err != nil {
		return err
	}
	defer db.Close()
	if err = verifySQLLeastPrivilege(ctx, db, profile); err != nil {
		return err
	}
	if err = e.verifyManagedDatabaseImportTargetEmpty(ctx, instance, record); err != nil {
		return err
	}
	if _, err = source.Seek(0, io.SeekStart); err != nil {
		return err
	}
	statements := 0
	err = visitSanitizedMySQLStatements(ctx, source, func(statement string) error {
		statements++
		if statements > 1_000_000 {
			return failure("artifact_invalid", "MySQL import contains too many statements", 413)
		}
		if _, execErr := db.ExecContext(ctx, statement); execErr != nil {
			return failure("operation_recovery_required", "MySQL rejected an imported table or row; inspect the empty target before retrying", 409)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if statements == 0 {
		return failure("artifact_invalid", "MySQL import contains no executable table or data statements", 412)
	}
	return nil
}

func openMySQLImportProfile(ctx context.Context, profile DatabaseConnection, password string) (*sql.DB, error) {
	config := mysql.NewConfig()
	config.User = profile.Username
	config.Passwd = password
	config.Net = "tcp"
	config.Addr = net.JoinHostPort(profile.Host, strconv.Itoa(profile.Port))
	config.DBName = profile.Database
	config.MultiStatements = false
	config.Timeout = 5 * time.Second
	config.ReadTimeout = 30 * time.Minute
	config.WriteTimeout = 30 * time.Minute
	config.ParseTime = true
	config.MaxAllowedPacket = maxDatabaseImportStatement + (64 << 10)
	if profile.TLS {
		config.TLSConfig = "true"
	}
	db, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		return nil, failure("connection_failed", "MySQL import connection could not be initialized", 502)
	}
	db.SetMaxOpenConns(1)
	// Keep the same session for LOCK/UNLOCK and other approved dump statements.
	// This private pool is closed at the end of the import.
	db.SetMaxIdleConns(1)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, failure("connection_failed", "MySQL import could not connect with its database-only identity", 502)
	}
	return db, nil
}

func visitSanitizedMySQLStatements(ctx context.Context, source io.Reader, visit func(string) error) error {
	reader := bufio.NewReaderSize(source, 64<<10)
	statement := make([]byte, 0, 4096)
	quote := byte(0)
	emit := func() error {
		trimmed := strings.TrimSpace(string(statement))
		statement = statement[:0]
		if trimmed == "" {
			return nil
		}
		return visit(trimmed)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ch, err := reader.ReadByte()
		if err != nil {
			if err != io.EOF {
				return err
			}
			if quote != 0 {
				return failure("artifact_invalid", "Sanitized MySQL import has an unterminated value", 412)
			}
			return emit()
		}
		if quote != 0 {
			statement = append(statement, ch)
			if ch == '\\' && quote != '`' {
				next, readErr := reader.ReadByte()
				if readErr != nil {
					return failure("artifact_invalid", "Sanitized MySQL import ends inside an escape sequence", 412)
				}
				statement = append(statement, next)
			} else if ch == quote {
				if next, peekErr := reader.Peek(1); peekErr == nil && next[0] == quote {
					_, _ = reader.ReadByte()
					statement = append(statement, quote)
				} else {
					quote = 0
				}
			}
		} else if ch == '\'' || ch == '"' || ch == '`' {
			quote = ch
			statement = append(statement, ch)
		} else if ch == ';' {
			if err = emit(); err != nil {
				return err
			}
		} else {
			statement = append(statement, ch)
		}
		if len(statement) > maxDatabaseImportStatement {
			return mysqlImportLimit()
		}
	}
}

func (e *PrivilegedExecutor) applyManagedDatabaseImport(ctx context.Context, instance Instance, record managedDatabaseRecord, id string, expectedImportRevision int64) (any, error) {
	if record.Permission != "read_write" {
		return nil, failure("credential_scope", "A read-only database identity cannot import data", 403)
	}
	imported, err := e.loadDatabaseImport(instance, record.Database, id)
	if err != nil {
		return nil, err
	}
	if imported.Revision != expectedImportRevision || imported.DatabaseRevision != record.Revision || imported.State != "uploaded" {
		return nil, failure("revision_conflict", "Import or managed database changed; inspect and reload before applying", 412)
	}
	source, err := e.openVerifiedDatabaseImport(ctx, instance, imported)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	prepared, err := e.prepareDatabaseImport(ctx, instance, record, source)
	if err != nil {
		return nil, err
	}
	defer prepared.Cleanup()
	if err = e.verifyManagedDatabaseImportTargetEmpty(ctx, instance, record); err != nil {
		return nil, err
	}
	imported.State = "applying"
	imported.Revision++
	if err = e.save("private", imported.ID, imported); err != nil {
		return nil, failure("operation_recovery_required", "Import could not claim its protected receipt", 409)
	}
	if err = e.restoreManagedDatabaseFile(ctx, instance, record, prepared.Reader, prepared.Postgres); err != nil {
		imported.State = "recovery_required"
		imported.Revision++
		if saveErr := e.save("private", imported.ID, imported); saveErr != nil {
			return nil, failure("operation_recovery_required", "Import failed and its recovery state could not be committed", 409)
		}
		return nil, err
	}
	imported.State = "applied"
	imported.Revision++
	if err = e.save("private", imported.ID, imported); err != nil {
		return nil, failure("operation_recovery_required", "Import completed but its protected receipt was not committed", 409)
	}
	return map[string]any{"import": imported.public(), "applied": true}, nil
}

func validImportTarget(ctx context.Context, instance Instance, record managedDatabaseRecord, database string, databaseRevision int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if record.InstanceID != instance.ID || record.Database != database || record.Engine != instance.PluginID || record.State != "active" || record.Revision != databaseRevision || record.Permission != "read_write" {
		return failure("revision_conflict", "Import target is no longer the current read/write database", 412)
	}
	return nil
}

// sanitizeMySQLImport accepts only table/data statements that can act inside
// the already-selected database. It strips comments (including executable
// version comments) and rejects client directives, qualified names and
// server/global operations before a connection is opened.
func sanitizeMySQLImport(ctx context.Context, source io.Reader, destination io.Writer) error {
	reader := bufio.NewReaderSize(source, 64<<10)
	statement := make([]byte, 0, 4096)
	quote := byte(0)
	lineStart := true
	createdTables := map[string]bool{}
	sawCreate := false
	emit := func(trimmed string) error {
		if err := writeSafeMySQLStatement(io.Discard, trimmed); err != nil {
			return err
		}
		tokens, _, _ := mysqlImportTokens(trimmed)
		first, second := strings.ToUpper(tokens[0]), ""
		if len(tokens) > 1 {
			second = strings.ToUpper(tokens[1])
		}
		if first == "CREATE" && second == "TABLE" {
			if len(tokens) < 3 || strings.EqualFold(tokens[2], "IF") {
				return failure("artifact_invalid", "External imports cannot reuse an existing table namespace", 412)
			}
			table := strings.ToLower(tokens[2])
			if createdTables[table] {
				return failure("artifact_invalid", "MySQL import defines the same table more than once", 412)
			}
			createdTables[table] = true
			sawCreate = true
		}
		if first == "INSERT" {
			if len(tokens) < 3 || !createdTables[strings.ToLower(tokens[2])] {
				return failure("artifact_invalid", "MySQL import data must follow creation of its target table", 412)
			}
		}
		if first == "ALTER" || first == "LOCK" {
			if len(tokens) < 3 || !createdTables[strings.ToLower(tokens[2])] {
				return failure("artifact_invalid", "MySQL import may only alter or lock tables it created", 412)
			}
		}
		return writeSafeMySQLStatement(destination, trimmed)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ch, err := reader.ReadByte()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return err
			}
			if quote != 0 {
				return failure("artifact_invalid", "MySQL import contains an unterminated quoted value", 412)
			}
			if trimmed := strings.TrimSpace(string(statement)); trimmed != "" {
				if err := emit(trimmed); err != nil {
					return err
				}
			}
			if !sawCreate {
				return failure("artifact_invalid", "External SQL import must create at least one new table", 412)
			}
			return nil
		}
		if quote == 0 {
			if ch == '\n' {
				lineStart = true
				statement = appendBoundedMySQL(statement, ch)
				if len(statement) > maxDatabaseImportStatement {
					return mysqlImportLimit()
				}
				continue
			}
			if lineStart && (ch == ' ' || ch == '\t' || ch == '\r') {
				statement = appendBoundedMySQL(statement, ch)
				continue
			}
			lineStart = false
			if ch == '#' || (ch == '-' && mysqlStartsDashComment(reader)) {
				if ch == '-' {
					_, _ = reader.ReadByte()
				}
				if err := skipMySQLLine(reader); err != nil && !errors.Is(err, io.EOF) {
					return err
				}
				statement = appendBoundedMySQL(statement, ' ')
				lineStart = true
				continue
			}
			if ch == '/' {
				next, peekErr := reader.Peek(1)
				if peekErr == nil && next[0] == '*' {
					_, _ = reader.ReadByte()
					if err := skipMySQLBlockComment(reader); err != nil {
						return failure("artifact_invalid", "MySQL import contains an unterminated comment", 412)
					}
					statement = appendBoundedMySQL(statement, ' ')
					continue
				}
			}
			if ch == ';' {
				if trimmed := strings.TrimSpace(string(statement)); trimmed != "" {
					if err := emit(trimmed); err != nil {
						return err
					}
				}
				statement = statement[:0]
				lineStart = true
				continue
			}
			if ch == '\'' || ch == '"' || ch == '`' {
				quote = ch
			}
		} else {
			if ch == '\\' && quote != '`' {
				statement = appendBoundedMySQL(statement, ch)
				if next, readErr := reader.ReadByte(); readErr == nil {
					statement = appendBoundedMySQL(statement, next)
					if len(statement) > maxDatabaseImportStatement {
						return mysqlImportLimit()
					}
					continue
				} else {
					return failure("artifact_invalid", "MySQL import ends inside an escape sequence", 412)
				}
			}
			if ch == quote {
				if next, peekErr := reader.Peek(1); peekErr == nil && next[0] == quote {
					statement = appendBoundedMySQL(statement, ch)
					_, _ = reader.ReadByte()
					statement = appendBoundedMySQL(statement, ch)
					continue
				}
				quote = 0
			}
		}
		statement = appendBoundedMySQL(statement, ch)
		if len(statement) > maxDatabaseImportStatement {
			return mysqlImportLimit()
		}
	}
}

func appendBoundedMySQL(statement []byte, value byte) []byte { return append(statement, value) }

func mysqlImportLimit() error {
	return failure("artifact_invalid", "MySQL import statement exceeds 16 MiB", 412)
}

func mysqlStartsDashComment(reader *bufio.Reader) bool {
	peek, err := reader.Peek(2)
	return err == nil && peek[0] == '-' && (peek[1] == ' ' || peek[1] == '\t' || peek[1] == '\r' || peek[1] == '\n' || peek[1] < 0x20)
}

func skipMySQLLine(reader *bufio.Reader) error {
	for {
		ch, err := reader.ReadByte()
		if err != nil || ch == '\n' {
			return err
		}
	}
}

func skipMySQLBlockComment(reader *bufio.Reader) error {
	previous := byte(0)
	for {
		ch, err := reader.ReadByte()
		if err != nil {
			return err
		}
		if previous == '*' && ch == '/' {
			return nil
		}
		previous = ch
	}
}

func writeSafeMySQLStatement(destination io.Writer, statement string) error {
	tokens, qualified, err := mysqlImportTokens(statement)
	if err != nil || len(tokens) == 0 || qualified {
		return failure("artifact_invalid", "MySQL import contains unsupported SQL syntax", 412)
	}
	first, second := strings.ToUpper(tokens[0]), ""
	if len(tokens) > 1 {
		second = strings.ToUpper(tokens[1])
	}
	safe := false
	switch first + " " + second {
	case "CREATE TABLE", "DROP TABLE", "ALTER TABLE", "INSERT INTO":
		safe = true
	case "LOCK TABLES":
		safe = len(tokens) == 4 && !strings.Contains(tokens[2], ".") && strings.EqualFold(tokens[3], "WRITE")
	case "UNLOCK TABLES":
		safe = len(tokens) == 2
	case "START TRANSACTION", "COMMIT":
		safe = len(tokens) == 2
	case "SET FOREIGN_KEY_CHECKS", "SET UNIQUE_CHECKS":
		safe = len(tokens) == 4 && tokens[2] == "=" && (tokens[3] == "0" || tokens[3] == "1")
	case "SET NAMES":
		safe = len(tokens) == 3 && strings.EqualFold(tokens[2], "utf8mb4")
	}
	if !safe {
		return failure("artifact_invalid", "MySQL import contains a database, account, server or client directive that is not allowed", 412)
	}
	if first == "DROP" {
		if len(tokens) < 5 || !strings.EqualFold(tokens[2], "IF") || !strings.EqualFold(tokens[3], "EXISTS") {
			return failure("artifact_invalid", "Only no-op DROP TABLE IF EXISTS lines can be removed from an external dump", 412)
		}
		return nil // Never execute an external DROP against even an apparently empty target.
	}
	if first == "CREATE" && (len(tokens) > 3 && strings.EqualFold(tokens[2], "IF")) {
		return failure("artifact_invalid", "External imports cannot use CREATE TABLE IF NOT EXISTS", 412)
	}
	for _, token := range tokens {
		upper := strings.ToUpper(token)
		if token == "@" {
			return failure("artifact_invalid", "MySQL import cannot reference session or server variables", 412)
		}
		switch upper {
		case "OUTFILE", "DUMPFILE", "LOAD_FILE", "LOAD", "DEFINER", "PROCEDURE", "FUNCTION", "TRIGGER", "EVENT", "TABLESPACE", "FEDERATED", "CONNECT", "BLACKHOLE", "CSV", "DIRECTORY", "PARSER", "SLEEP", "BENCHMARK", "GET_LOCK", "RELEASE_LOCK", "INSTALL", "UNINSTALL", "PLUGIN", "UDF":
			return failure("artifact_invalid", "MySQL import contains a privileged or external operation", 412)
		}
	}
	if first == "CREATE" || first == "ALTER" {
		for index := 0; index+2 < len(tokens); index++ {
			if strings.EqualFold(tokens[index], "ENGINE") && tokens[index+1] == "=" && !strings.EqualFold(tokens[index+2], "INNODB") {
				return failure("artifact_invalid", "External MySQL imports may only use the built-in InnoDB table engine", 412)
			}
		}
	}
	if containsMySQLKeyword(tokens, "SELECT") {
		return failure("artifact_invalid", "External imports cannot execute SELECT expressions", 412)
	}
	if first == "CREATE" || first == "DROP" || first == "ALTER" {
		if len(tokens) < 3 || tokens[2] == "." || strings.Contains(tokens[2], ".") {
			return failure("artifact_invalid", "MySQL import must address tables only inside the selected database", 412)
		}
	}
	if first == "INSERT" && !containsMySQLKeyword(tokens, "VALUES") && !containsMySQLKeyword(tokens, "VALUE") {
		return failure("artifact_invalid", "MySQL imports must provide literal table rows rather than SELECT expressions", 412)
	}
	if err := validateMySQLImportCalls(tokens, first, second); err != nil {
		return failure("artifact_invalid", "MySQL import contains an unsupported function or expression", 412)
	}
	if _, err := io.WriteString(destination, statement+";\n"); err != nil {
		return err
	}
	return nil
}

func containsMySQLKeyword(tokens []string, keyword string) bool {
	for _, token := range tokens {
		if strings.EqualFold(token, keyword) {
			return true
		}
	}
	return false
}

func validateMySQLImportCalls(tokens []string, first, second string) error {
	tableIndex := -1
	if first == "CREATE" || first == "DROP" || first == "ALTER" {
		tableIndex = 2
		if len(tokens) > 5 && strings.EqualFold(tokens[2], "IF") && strings.EqualFold(tokens[3], "NOT") && strings.EqualFold(tokens[4], "EXISTS") {
			tableIndex = 5
		} else if len(tokens) > 5 && strings.EqualFold(tokens[2], "IF") && strings.EqualFold(tokens[3], "EXISTS") {
			tableIndex = 4
		}
	} else if first == "INSERT" {
		tableIndex = 2
	}
	allowedGrammar := map[string]bool{
		"VARCHAR": true, "CHAR": true, "NCHAR": true, "NVARCHAR": true, "BINARY": true, "VARBINARY": true,
		"DECIMAL": true, "NUMERIC": true, "BIT": true, "INT": true, "INTEGER": true, "TINYINT": true,
		"SMALLINT": true, "MEDIUMINT": true, "BIGINT": true, "FLOAT": true, "DOUBLE": true, "REAL": true,
		"TIME": true, "TIMESTAMP": true, "DATETIME": true, "YEAR": true, "ENUM": true, "SET": true,
		"KEY": true, "VALUES": true, "VALUE": true, "ROW": true,
	}
	for index := 0; index+1 < len(tokens); index++ {
		if tokens[index+1] != "(" || !mysqlImportWordToken(tokens[index]) {
			continue
		}
		name := strings.ToUpper(tokens[index])
		if allowedGrammar[name] || index == tableIndex || (index > 0 && (strings.EqualFold(tokens[index-1], "KEY") || strings.EqualFold(tokens[index-1], "INDEX"))) {
			continue
		}
		return errors.New("function call is not allowed")
	}
	return nil
}

func mysqlImportWordToken(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || character == '_' || character == '$' || (index > 0 && character >= '0' && character <= '9') {
			continue
		}
		return false
	}
	return true
}

func mysqlImportTokens(statement string) ([]string, bool, error) {
	tokens := []string{}
	var current strings.Builder
	quote := byte(0)
	parentheses := 0
	qualified := false
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	for i := 0; i < len(statement); i++ {
		ch := statement[i]
		if quote != 0 {
			if ch == '\\' && quote != '`' {
				i++
				continue
			}
			if ch == quote {
				if i+1 < len(statement) && statement[i+1] == quote {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			flush()
			quote = ch
			continue
		}
		if ch == '`' {
			flush()
			quote = ch
			start := i + 1
			for i+1 < len(statement) && statement[i+1] != '`' {
				i++
			}
			if i+1 >= len(statement) {
				return nil, false, errors.New("unclosed identifier")
			}
			tokens = append(tokens, statement[start:i+1])
			i++
			quote = 0
			continue
		}
		if ch == '.' {
			digitBefore := i > 0 && statement[i-1] >= '0' && statement[i-1] <= '9'
			digitAfter := i+1 < len(statement) && statement[i+1] >= '0' && statement[i+1] <= '9'
			if digitBefore && digitAfter {
				current.WriteByte(ch)
				continue
			}
			qualified = true
			flush()
			tokens = append(tokens, ".")
			continue
		}
		if ch == '(' {
			parentheses++
		} else if ch == ')' {
			parentheses--
			if parentheses < 0 {
				return nil, false, errors.New("unbalanced SQL parentheses")
			}
		}
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '$' {
			current.WriteByte(ch)
			continue
		}
		flush()
		if strings.ContainsRune("=(),@", rune(ch)) {
			tokens = append(tokens, string(ch))
		}
	}
	if quote != 0 {
		return nil, false, errors.New("unclosed quote")
	}
	if parentheses != 0 {
		return nil, false, errors.New("unbalanced SQL parentheses")
	}
	flush()
	return tokens, qualified, nil
}

func validDatabaseImportStream(raw json.RawMessage) error {
	var in databaseImportStreamInput
	if err := parse(raw, &in); err != nil {
		return err
	}
	if !validInstanceID(in.InstanceID) || !validDatabaseName(in.Database) || !validDatabaseImportID(in.ImportID) || in.ExpectedBytes < 1 || in.ExpectedBytes > maxExtractedBytes || !validDatabaseImportSHA256(in.ExpectedSHA256) || in.DatabaseRevision < 1 {
		return failure("invalid_input", "Invalid bounded database import descriptor", 400)
	}
	return nil
}

func (e *PrivilegedExecutor) databaseImportArtifactStream(ctx context.Context, request agent.Request, streams agent.IO) (result agent.Result, err error) {
	result = agent.Result{Operation: request.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	defer func() { result.FinishedAt = time.Now().UTC() }()
	if err = validDatabaseImportStream(request.Input); err != nil {
		return result, err
	}
	if streams.Stdin == nil || streams.Stdout != nil {
		return result, failure("stream_required", "Database import upload requires only an input stream", 400)
	}
	var in databaseImportStreamInput
	_ = json.Unmarshal(request.Input, &in)
	unlock, err := e.lockContext(ctx, in.InstanceID)
	if err != nil {
		return result, err
	}
	defer unlock()
	if err = e.ensureInstallFinalized(ctx, in.InstanceID); err != nil {
		return result, err
	}
	instance, err := e.get(ctx, in.InstanceID, false)
	if err != nil {
		return result, err
	}
	var target managedDatabaseRecord
	if err = e.load("private", managedDatabaseKey(instance.ID, in.Database), &target); err != nil {
		return result, err
	}
	if err = validImportTarget(ctx, instance, target, in.Database, in.DatabaseRevision); err != nil {
		return result, err
	}
	if err = realDirectory(instance.Paths.BackupDir); err != nil {
		return result, err
	}
	root, err := os.OpenRoot(instance.Paths.BackupDir)
	if err != nil {
		return result, err
	}
	defer root.Close()
	var existing databaseImportReceipt
	if err = e.load("private", in.ImportID, &existing); err == nil {
		return result, failure("artifact_exists", "Import ID has already been used", 409)
	} else if core.ErrorCode(err) != "not_found" {
		return result, err
	}
	filename := in.ImportID + ".external"
	if _, err = root.Lstat(filename); err == nil {
		return result, failure("artifact_exists", "Import file already exists", 409)
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	temporary := databaseImportTempName(in.ImportID)
	receipt := databaseImportReceipt{ID: in.ImportID, InstanceID: instance.ID, Database: in.Database, Engine: instance.PluginID, Filename: filename, ExpectedBytes: in.ExpectedBytes, ExpectedSHA256: strings.ToLower(in.ExpectedSHA256), DatabaseRevision: target.Revision, Revision: 1, State: "uploading", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	// Publish the protected receipt before creating or filling the upload file.
	// If the Agent is interrupted during a large HTTP stream, the private
	// artifact remains visible and can be explicitly deleted by its owner.
	if err = e.save("private", in.ImportID, receipt); err != nil {
		return result, err
	}
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		receipt.State = "invalid"
		receipt.Revision++
		_ = e.save("private", in.ImportID, receipt)
		return result, failure("artifact_exists", "Import upload could not be initialized", 409)
	}
	defer func() {
		file.Close()
	}()
	hasher := sha256.New()
	count, err := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(&contextReader{ctx: ctx, reader: streams.Stdin}, in.ExpectedBytes+1))
	if err != nil || count != in.ExpectedBytes || count > maxExtractedBytes {
		if count > in.ExpectedBytes {
			// The received data exceeded the declared size. Discard it and retain
			// an invalid receipt so it cannot be mistaken for a resumable upload.
			_ = file.Close()
			_ = root.Remove(temporary)
			receipt.State = "invalid"
			receipt.Revision++
			_ = e.save("private", in.ImportID, receipt)
		} else if count > 0 {
			// Preserve only bounded progress metadata. The owner can inspect and
			// delete the interrupted artifact; this path never applies it.
			receipt.BytesReceived = count
			receipt.Revision++
			_ = e.save("private", in.ImportID, receipt)
		}
		return result, failure("artifact_transfer_failed", "Import upload ended early or exceeded its declared size", 413)
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if digest != strings.ToLower(in.ExpectedSHA256) {
		_ = file.Close()
		receipt.BytesReceived = count
		receipt.SHA256 = digest
		receipt.State = "invalid"
		receipt.Revision++
		_ = e.save("private", in.ImportID, receipt)
		return result, failure("checksum_mismatch", "Uploaded import does not match its declared SHA-256", 412)
	}
	if err = file.Sync(); err != nil {
		return result, err
	}
	if err = file.Close(); err != nil {
		return result, err
	}
	if err = root.Rename(temporary, filename); err != nil {
		return result, err
	}
	if err = syncDatabaseImportDirectory(instance.Paths.BackupDir); err != nil {
		return result, failure("operation_recovery_required", "Import file was received but not durably committed", 409)
	}
	receipt.BytesReceived = count
	receipt.SHA256 = digest
	receipt.Revision++
	receipt.State = "uploaded"
	if err = e.save("private", in.ImportID, receipt); err != nil {
		// The prior durable receipt still says uploading, so retain the file for
		// explicit deletion rather than risking cleanup that races the receipt.
		return result, failure("operation_recovery_required", "Import file was received but its protected receipt was not committed", 409)
	}
	result.Data, _ = json.Marshal(receipt.public())
	result.StdoutBytes = count
	result.ExitCode = 0
	return result, nil
}
