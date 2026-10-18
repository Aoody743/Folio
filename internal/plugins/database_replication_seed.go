package plugins

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

var databaseReplicationCoordinatePattern = regexp.MustCompile(`(?m)^-- CHANGE (?:MASTER TO MASTER_LOG_FILE|REPLICATION SOURCE TO SOURCE_LOG_FILE)=['"]([^'"]+)['"], (?:MASTER_LOG_POS|SOURCE_LOG_POS)=([0-9]+)[^\r\n]*;\s*$`)

const databaseReplicationSeedDirectory = "database_replication_seeds"

type databaseReplicationDumpOutput struct {
	ctx    context.Context
	file   *os.File
	hash   hash.Hash
	bytes  int64
	prefix []byte
}

func (w *databaseReplicationDumpOutput) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(data)) > databaseReplicationMaxSeed-w.bytes {
		return 0, failure("replication_seed_limit", "Consistent replication seed exceeds four GiB", 413)
	}
	n, err := w.file.Write(data)
	if n > 0 {
		_, _ = w.hash.Write(data[:n])
		w.bytes += int64(n)
		if len(w.prefix) < 1<<20 {
			remaining := (1 << 20) - len(w.prefix)
			if remaining > n {
				remaining = n
			}
			w.prefix = append(w.prefix, data[:remaining]...)
		}
	}
	return n, err
}

func (w *databaseReplicationDumpOutput) Close() error { return nil }

func databaseReplicationSeedFilename(pairID string) string { return "dbrep_" + pairID + ".sql" }

func parseDatabaseReplicationCoordinate(dumpPrefix []byte) (string, uint64, error) {
	match := databaseReplicationCoordinatePattern.FindSubmatch(dumpPrefix)
	if len(match) != 3 {
		return "", 0, failure("replication_coordinate_unavailable", "Consistent dump did not contain a verifiable binary-log coordinate", 412)
	}
	file := string(match[1])
	position, err := strconv.ParseUint(string(match[2]), 10, 64)
	if err != nil || position == 0 || !databaseBinlogNamePattern.MatchString(file) {
		return "", 0, failure("replication_coordinate_unavailable", "Consistent dump contained an invalid binary-log coordinate", 412)
	}
	return file, position, nil
}

func databaseReplicationDumpComplete(tail []byte) bool {
	return bytes.Contains(tail, []byte("-- Dump completed on ")) || bytes.Contains(tail, []byte("-- MySQL dump complete"))
}

func (e *PrivilegedExecutor) databaseReplicationClientFile(instance Instance) (string, func(), error) {
	if err := realDirectory(instance.Paths.RunDir); err != nil {
		return "", func() {}, err
	}
	var credential managedDatabaseCredential
	if err := e.load("private", databasePrivateID(instance), &credential); err != nil || !credential.Initialized || credential.Engine != instance.PluginID || credential.DataDir != instance.Paths.DataDir {
		return "", func() {}, failure("credential_unavailable", "Owned database administrator credential is unavailable", 412)
	}
	file, err := os.CreateTemp(instance.Paths.RunDir, ".replication-client-")
	if err != nil {
		return "", func() {}, err
	}
	name := file.Name()
	cleanup := func() { _ = os.Remove(name) }
	body := "[client]\nuser=" + mysqlOptionQuote(credential.Username) + "\npassword=" + mysqlOptionQuote(credential.Password) + "\nhost=127.0.0.1\nport=" + strconv.Itoa(setting(instance.Config, "port", 0)) + "\nprotocol=tcp\n"
	if err = file.Chmod(0600); err == nil && os.Geteuid() == 0 {
		err = file.Chown(int(e.uid), int(e.gid))
	}
	if err == nil {
		_, err = file.WriteString(body)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	return name, cleanup, nil
}

func databaseReplicationDumpProgram(instance Instance) string {
	if instance.PluginID == "mariadb" {
		return "/usr/bin/mariadb-dump"
	}
	return mysqlClientProgram(instance, "mysqldump")
}

func databaseReplicationImportProgram(instance Instance) string {
	if instance.PluginID == "mariadb" {
		return "/usr/bin/mariadb"
	}
	return mysqlClientProgram(instance, "mysql")
}

func databaseReplicationDumpArgs(configFile, database string) ([]string, error) {
	if configFile == "" || !validDatabaseName(database) {
		return nil, failure("invalid_input", "A private client file and valid managed database are required for a seed dump", 400)
	}
	return []string{
		"--defaults-file=" + configFile,
		"--single-transaction", "--master-data=2", "--skip-lock-tables", "--no-tablespaces",
		"--no-create-db", "--skip-add-drop-table", "--skip-triggers", "--skip-routines", "--skip-events",
		"--hex-blob", "--default-character-set=utf8mb4", database,
	}, nil
}

func databaseReplicationImportArgs(configFile, database string) ([]string, error) {
	if configFile == "" || !validDatabaseName(database) {
		return nil, failure("invalid_input", "A private client file and valid managed database are required for a seed import", 400)
	}
	// mysqldump without --databases may omit USE. Select the exact validated
	// target schema in the client argv so import behavior does not depend on
	// comments or header statements in a dump file.
	return []string{"--defaults-file=" + configFile, "--binary-mode", "--local-infile=0", "--default-character-set=utf8mb4", database}, nil
}

// openDatabaseReplicationSeedDirectory opens the Agent-private seed store.
// Database workers can read/write their instance backup trees, so replication
// snapshots never live there. The Agent UID and exact 0700 directory mode are
// checked every time, and the caller receives both a path-confined root and a
// directory FD for durable rename/remove syncs.
func (e *PrivilegedExecutor) openDatabaseReplicationSeedDirectory(create bool) (*os.Root, *os.File, string, error) {
	privateDir := filepath.Join(e.config.StateDir, "private")
	if err := realDirectory(privateDir); err != nil {
		return nil, nil, "", failure("private_state_unavailable", "Agent private state directory is unavailable for replication seeds", 500)
	}
	privateInfo, err := os.Stat(privateDir)
	if err != nil {
		return nil, nil, "", err
	}
	privateOwner, ok := privateInfo.Sys().(*syscall.Stat_t)
	if !ok || !privateInfo.IsDir() || privateInfo.Mode().Perm()&0077 != 0 || privateOwner.Uid != uint32(os.Geteuid()) {
		return nil, nil, "", failure("ownership_invalid", "Agent private state directory ownership or permissions changed", 412)
	}
	privateRoot, err := os.OpenRoot(privateDir)
	if err != nil {
		return nil, nil, "", err
	}
	defer privateRoot.Close()
	created := false
	if create {
		if err = privateRoot.Mkdir(databaseReplicationSeedDirectory, 0700); err == nil {
			created = true
		} else if !os.IsExist(err) {
			return nil, nil, "", err
		}
	}
	if created {
		parent, openErr := os.OpenFile(privateDir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if openErr != nil {
			return nil, nil, "", openErr
		}
		syncErr := parent.Sync()
		_ = parent.Close()
		if syncErr != nil {
			return nil, nil, "", failure("operation_recovery_required", "Agent private state directory could not be durably synced", 409)
		}
	}
	entry, err := privateRoot.Lstat(databaseReplicationSeedDirectory)
	if err != nil {
		return nil, nil, "", err
	}
	seedDir := filepath.Join(privateDir, databaseReplicationSeedDirectory)
	seedOwner, ok := entry.Sys().(*syscall.Stat_t)
	if !ok || !entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 || entry.Mode().Perm() != 0700 || seedOwner.Uid != uint32(os.Geteuid()) {
		return nil, nil, "", failure("ownership_invalid", "Replication seed directory must be an Agent-owned private 0700 directory", 412)
	}
	directory, err := os.OpenFile(seedDir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, "", err
	}
	directoryInfo, err := directory.Stat()
	if err != nil {
		directory.Close()
		return nil, nil, "", err
	}
	directoryOwner, ok := directoryInfo.Sys().(*syscall.Stat_t)
	if !ok || !directoryInfo.IsDir() || directoryInfo.Mode().Perm() != 0700 || directoryOwner.Uid != uint32(os.Geteuid()) {
		directory.Close()
		return nil, nil, "", failure("ownership_invalid", "Replication seed directory changed while opening it", 412)
	}
	root, err := os.OpenRoot(seedDir)
	if err != nil {
		directory.Close()
		return nil, nil, "", err
	}
	return root, directory, seedDir, nil
}

func validateDatabaseReplicationSeedFile(file *os.File, expectedSize int64) error {
	info, err := file.Stat()
	if err != nil {
		return failure("artifact_invalid", "Replication seed file metadata could not be verified", 412)
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || owner.Uid != uint32(os.Geteuid()) || owner.Nlink != 1 ||
		info.Size() <= 0 || info.Size() > databaseReplicationMaxSeed || expectedSize > 0 && info.Size() != expectedSize {
		return failure("artifact_invalid", "Replication seed must be an Agent-owned regular 0600 single-link file within its exact size bound", 412)
	}
	return nil
}

func (e *PrivilegedExecutor) createDatabaseReplicationSeed(ctx context.Context, source, target Instance, record databaseReplicationRecord) (string, int64, string, string, uint64, error) {
	var empty string
	if err := e.verifyReplicationRoleRuntime(ctx, source, "source", record); err != nil {
		return empty, 0, empty, empty, 0, err
	}
	configFile, cleanup, err := e.databaseReplicationClientFile(source)
	if err != nil {
		return empty, 0, empty, empty, 0, err
	}
	defer cleanup()
	program := databaseReplicationDumpProgram(source)
	if err = e.validateDatabaseUtility(ctx, source, program); err != nil {
		return empty, 0, empty, empty, 0, err
	}
	seedRoot, seedDirectory, seedDir, err := e.openDatabaseReplicationSeedDirectory(true)
	if err != nil {
		return empty, 0, empty, empty, 0, err
	}
	defer seedRoot.Close()
	defer seedDirectory.Close()
	file, err := os.CreateTemp(seedDir, ".replication-seed-")
	if err != nil {
		return empty, 0, empty, empty, 0, err
	}
	tempName := file.Name()
	defer os.Remove(tempName)
	tempBase := filepath.Base(tempName)
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return empty, 0, empty, empty, 0, err
	}
	if err = validateDatabaseReplicationSeedFile(file, 0); err != nil {
		file.Close()
		return empty, 0, empty, empty, 0, err
	}
	writer := &databaseReplicationDumpOutput{ctx: ctx, file: file, hash: sha256.New()}
	args, err := databaseReplicationDumpArgs(configFile, record.Database)
	if err != nil {
		file.Close()
		return empty, 0, empty, empty, 0, err
	}
	runner, err := agent.NewRunner(agent.Options{Operations: []agent.Operation{{
		Name: "database_replication_dump", Program: program,
		BuildArgs: func(json.RawMessage) ([]string, error) { return args, nil },
		Timeout:   30 * time.Minute, Environment: []string{"LC_ALL=C"}, Credential: e.credential(source),
	}}, MaxOutputBytes: 32 << 10, MaxConcurrent: 1})
	if err != nil {
		file.Close()
		return empty, 0, empty, empty, 0, err
	}
	result, runErr := runner.Stream(ctx, agent.Request{Operation: "database_replication_dump", Input: []byte(`{}`)}, agent.IO{Stdout: writer})
	if runErr != nil || result.ExitCode != 0 {
		file.Close()
		return empty, 0, empty, empty, 0, failure("replication_dump_failed", "Consistent managed database dump did not complete", 502)
	}
	logFile, position, err := parseDatabaseReplicationCoordinate(writer.prefix)
	if err != nil {
		file.Close()
		return empty, 0, empty, empty, 0, err
	}
	if writer.bytes == 0 {
		file.Close()
		return empty, 0, empty, empty, 0, failure("replication_dump_failed", "Consistent managed database dump was empty", 502)
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return empty, 0, empty, empty, 0, err
	}
	if err = validateDatabaseReplicationSeedFile(file, writer.bytes); err != nil {
		file.Close()
		return empty, 0, empty, empty, 0, err
	}
	tail, err := readDatabaseReplicationSeedTail(file)
	if err != nil || !databaseReplicationDumpComplete(tail) {
		file.Close()
		return empty, 0, empty, empty, 0, failure("replication_dump_incomplete", "Consistent dump completion marker was not verified", 502)
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return empty, 0, empty, empty, 0, err
	}
	if err = file.Close(); err != nil {
		return empty, 0, empty, empty, 0, err
	}
	filename := databaseReplicationSeedFilename(record.PairID)
	if _, err = seedRoot.Lstat(filename); err == nil {
		return empty, 0, empty, empty, 0, failure("replication_seed_exists", "Replication seed path already exists; explicit recovery must inspect it", 409)
	} else if !os.IsNotExist(err) {
		return empty, 0, empty, empty, 0, err
	}
	if err = seedRoot.Rename(tempBase, filename); err != nil {
		return empty, 0, empty, empty, 0, failure("replication_seed_exists", "Replication seed path could not be committed without replacing an existing artifact", 409)
	}
	if err = seedDirectory.Sync(); err != nil {
		_ = seedRoot.Remove(filename)
		_ = seedDirectory.Sync()
		return empty, 0, empty, empty, 0, failure("operation_recovery_required", "Protected replication seed directory could not be durably synced", 409)
	}
	return filename, writer.bytes, hex.EncodeToString(writer.hash.Sum(nil)), logFile, position, nil
}

func readDatabaseReplicationSeedTail(file *os.File) ([]byte, error) {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > databaseReplicationMaxSeed {
		return nil, failure("artifact_invalid", "Replication seed artifact is outside its exact size bound", 412)
	}
	tailLimit := int64(4096)
	if info.Size() < tailLimit {
		tailLimit = info.Size()
	}
	if _, err = file.Seek(-tailLimit, io.SeekEnd); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(file, tailLimit))
}

func (e *PrivilegedExecutor) openVerifiedDatabaseReplicationSeed(ctx context.Context, target Instance, record databaseReplicationRecord) (*os.File, error) {
	if target.ID != record.TargetInstanceID || record.SeedFilename != databaseReplicationSeedFilename(record.PairID) || record.SeedBytes <= 0 || record.SeedBytes > databaseReplicationMaxSeed || !databaseReplicationDigestPattern.MatchString(record.SeedSHA256) {
		return nil, failure("artifact_invalid", "Protected replication seed receipt is incomplete", 412)
	}
	root, directory, _, err := e.openDatabaseReplicationSeedDirectory(false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	defer directory.Close()
	file, err := root.OpenFile(record.SeedFilename, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, failure("artifact_invalid", "Protected replication seed artifact is unavailable", 412)
	}
	if err = validateDatabaseReplicationSeedFile(file, record.SeedBytes); err != nil {
		file.Close()
		return nil, err
	}
	hash := sha256.New()
	prefix := make([]byte, 0, 1<<20)
	tail := make([]byte, 0, 4096)
	buf := make([]byte, 128<<10)
	var size int64
	for {
		if err = ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		n, readErr := file.Read(buf)
		if n > 0 {
			size += int64(n)
			_, _ = hash.Write(buf[:n])
			if len(prefix) < 1<<20 {
				remaining := (1 << 20) - len(prefix)
				if remaining > n {
					remaining = n
				}
				prefix = append(prefix, buf[:remaining]...)
			}
			tail = append(tail, buf[:n]...)
			if len(tail) > 4096 {
				tail = append(tail[:0], tail[len(tail)-4096:]...)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			file.Close()
			return nil, failure("artifact_invalid", "Replication seed could not be fully verified", 412)
		}
	}
	if size != record.SeedBytes || hex.EncodeToString(hash.Sum(nil)) != record.SeedSHA256 || !databaseReplicationDumpComplete(tail) {
		file.Close()
		return nil, failure("artifact_invalid", "Replication seed bytes, hash or completion marker differs from its protected receipt", 412)
	}
	if err = validateDatabaseReplicationSeedFile(file, record.SeedBytes); err != nil {
		file.Close()
		return nil, err
	}
	if _, _, err = parseDatabaseReplicationCoordinate(prefix); err != nil {
		file.Close()
		return nil, err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func (e *PrivilegedExecutor) importDatabaseReplicationSeed(ctx context.Context, target Instance, record databaseReplicationRecord) error {
	if err := e.ensureDatabaseReplicationTargetWritable(ctx, target.ID, record.Database); err != nil {
		return err
	}
	if err := e.verifyReplicationRoleRuntime(ctx, target, "target", record); err != nil {
		return err
	}
	db, err := e.openManagedAdmin(ctx, target)
	if err != nil {
		return err
	}
	count, err := databaseReplicationScalar(ctx, db, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=?", record.Database)
	if err == nil && count != 0 {
		err = failure("target_not_empty", "Target gained tables before the seed import; no import was replayed", 409)
	}
	_ = db.Close()
	if err != nil {
		return err
	}
	seed, err := e.openVerifiedDatabaseReplicationSeed(ctx, target, record)
	if err != nil {
		return err
	}
	defer seed.Close()
	configFile, cleanup, err := e.databaseReplicationClientFile(target)
	if err != nil {
		return err
	}
	defer cleanup()
	program := databaseReplicationImportProgram(target)
	if err = e.validateDatabaseUtility(ctx, target, program); err != nil {
		return err
	}
	args, err := databaseReplicationImportArgs(configFile, record.Database)
	if err != nil {
		return err
	}
	runner, err := agent.NewRunner(agent.Options{Operations: []agent.Operation{{
		Name: "database_replication_import", Program: program,
		BuildArgs: func(json.RawMessage) ([]string, error) { return args, nil },
		Timeout:   30 * time.Minute, Environment: []string{"LC_ALL=C"}, Credential: e.credential(target),
	}}, MaxOutputBytes: 32 << 10, MaxConcurrent: 1})
	if err != nil {
		return err
	}
	if record.Engine == "mysql" {
		// Keep read_only enabled, but temporarily release super_read_only so the
		// exact root-owned seed import can populate an empty dedicated target.
		// The durable seed_import_pending journal is committed before this SQL.
		if err = e.setDatabaseReplicationSuperReadOnly(ctx, target, record, false); err != nil {
			return err
		}
	}
	result, runErr := runner.Stream(ctx, agent.Request{Operation: "database_replication_import", Input: []byte(`{}`)}, agent.IO{Stdin: seed})
	if record.Engine == "mysql" {
		protectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		protectErr := e.setDatabaseReplicationSuperReadOnly(protectCtx, target, record, true)
		cancel()
		if protectErr != nil {
			return failure("replication_target_protection_unverified", "Seed import ended without verifying that target super_read_only was restored; explicitly recover the pair", 409)
		}
	}
	if runErr != nil || result.ExitCode != 0 {
		return failure("replication_import_ambiguous", "Seed import did not complete cleanly; target contents were retained and no automatic import replay will occur", 409)
	}
	db, err = e.openManagedAdmin(ctx, target)
	if err != nil {
		return err
	}
	schema, inspectErr := inspectDatabaseReplicationSchema(ctx, db, record.Database)
	_ = db.Close()
	if inspectErr != nil {
		return inspectErr
	}
	if schema.Fingerprint != record.SourceSchemaSHA256 || schema.Tables != record.TableCount {
		return failure("replication_seed_schema_mismatch", "Imported target schema does not match the frozen source table inventory", 409)
	}
	return nil
}

// A crash during mysql client import leaves no transaction boundary. Explicit
// recovery retries only if an inspection proves that the target is still
// completely empty; any partial or complete non-empty result is retained for
// operator inspection and is never replayed over.
func (e *PrivilegedExecutor) recoverDatabaseReplicationSeedImport(ctx context.Context, target Instance, record databaseReplicationRecord) error {
	db, err := e.openManagedAdmin(ctx, target)
	if err != nil {
		return err
	}
	count, err := databaseReplicationScalar(ctx, db, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=?", record.Database)
	_ = db.Close()
	if err != nil {
		return err
	}
	if count != 0 {
		if record.Engine == "mysql" {
			protectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			protectErr := e.setDatabaseReplicationSuperReadOnly(protectCtx, target, record, true)
			cancel()
			if protectErr != nil {
				return failure("replication_target_protection_unverified", "Interrupted seed import left target super_read_only unverified; no import replay was attempted", 409)
			}
		}
		return failure("replication_import_ambiguous", "Target contains objects after an interrupted seed import; no import was replayed and the target was retained for manual inspection", 409)
	}
	return e.importDatabaseReplicationSeed(ctx, target, record)
}

func (e *PrivilegedExecutor) setDatabaseReplicationSuperReadOnly(ctx context.Context, target Instance, record databaseReplicationRecord, enabled bool) error {
	if err := e.authorizeDatabaseReplicationSeedTransition(ctx, target, record); err != nil {
		return err
	}
	db, err := e.openManagedAdmin(ctx, target)
	if err != nil {
		return err
	}
	defer db.Close()
	runtime, err := readDatabaseReplicationRuntime(ctx, db, target.PluginID)
	if err != nil {
		return err
	}
	if runtime.SuperReadOnly == enabled {
		return nil
	}
	value := "OFF"
	if enabled {
		value = "ON"
	}
	if err = databaseReplicationExec(ctx, db, "SET GLOBAL super_read_only="+value); err != nil {
		return err
	}
	runtime, err = readDatabaseReplicationRuntime(ctx, db, target.PluginID)
	if err != nil {
		return err
	}
	if runtime.SuperReadOnly != enabled || !runtime.ReadOnly {
		return failure("replication_target_protection_unverified", "MySQL did not confirm the requested super-read-only state", 412)
	}
	return nil
}

func (e *PrivilegedExecutor) authorizeDatabaseReplicationSeedTransition(ctx context.Context, target Instance, record databaseReplicationRecord) error {
	if canonicalDatabaseReplicationEngine(target.PluginID) != "mysql" || record.Engine != "mysql" || record.TargetInstanceID != target.ID || record.Operation != "configure" || record.Phase != "seed_import_pending" {
		return failure("ownership_invalid", "Super-read-only transition is limited to the exact MySQL seed-import intent", 412)
	}
	current, err := e.loadDatabaseReplication(record.PairID)
	if err != nil || current == nil || current.Revision != record.Revision {
		return failure("revision_conflict", "MySQL seed-import intent changed before its read-only transition", 409)
	}
	currentDigest, digestErr := databaseReplicationRecordDigest(*current)
	recordDigest, recordDigestErr := databaseReplicationRecordDigest(record)
	marker, hasMarker := ctx.Value(databaseReplicationRecoveryContextKey{}).(databaseReplicationRecoveryContext)
	if digestErr != nil || recordDigestErr != nil || currentDigest != recordDigest || !hasMarker || marker.executor != e || marker.pairID != record.PairID || marker.revision != record.Revision || marker.digest != currentDigest || !marker.instanceIDs[target.ID] {
		return failure("ownership_invalid", "MySQL read-only transition lacks an exact in-process pair authorization", 412)
	}
	return nil
}

func databaseReplicationAccountName(record databaseReplicationRecord) string {
	return "'" + record.Account + "'@'127.0.0.1'"
}

func databaseReplicationAccountExists(ctx context.Context, db *sql.DB, record databaseReplicationRecord) (bool, error) {
	var count uint64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mysql.user WHERE User=? AND Host='127.0.0.1'", record.Account).Scan(&count); err != nil {
		return false, failure("replication_protocol_unsupported", "Managed server could not inspect the generated replication account", 412)
	}
	if count > 1 {
		return false, failure("replication_account_unmanaged", "Generated replication account identity is ambiguous", 412)
	}
	return count == 1, nil
}

func databaseReplicationAccountGrants(ctx context.Context, db *sql.DB, record databaseReplicationRecord) (bool, bool, error) {
	exists, err := databaseReplicationAccountExists(ctx, db, record)
	if err != nil || !exists {
		return exists, false, err
	}
	rows, err := db.QueryContext(ctx, "SHOW GRANTS FOR "+databaseReplicationAccountName(record))
	if err != nil {
		return true, false, failure("replication_protocol_unsupported", "Generated replication account grants could not be verified", 412)
	}
	defer rows.Close()
	granted, seen := false, 0
	for rows.Next() {
		var grant string
		if err = rows.Scan(&grant); err != nil {
			return true, false, failure("replication_protocol_unsupported", "Generated replication account grants could not be decoded", 412)
		}
		upper := strings.ToUpper(strings.TrimSpace(grant))
		if !strings.HasPrefix(upper, "GRANT ") {
			return true, false, failure("replication_account_unmanaged", "Generated replication account has an unrecognized grant", 412)
		}
		before, _, ok := strings.Cut(strings.TrimPrefix(upper, "GRANT "), " ON *.* TO ")
		if !ok || before != "USAGE" && before != "REPLICATION SLAVE" {
			return true, false, failure("replication_account_unmanaged", "Generated replication account has privileges outside the least-privilege replication grant", 412)
		}
		if before == "REPLICATION SLAVE" {
			granted = true
		}
		seen++
		if seen > 3 {
			return true, false, failure("replication_account_unmanaged", "Generated replication account has too many grants to verify safely", 412)
		}
	}
	if err = rows.Err(); err != nil {
		return true, false, failure("replication_protocol_unsupported", "Generated replication account grant listing did not complete", 412)
	}
	return true, granted, nil
}

func (e *PrivilegedExecutor) createDatabaseReplicationAccount(ctx context.Context, source Instance, record databaseReplicationRecord) error {
	db, err := e.openManagedAdmin(ctx, source)
	if err != nil {
		return err
	}
	defer db.Close()
	exists, err := databaseReplicationAccountExists(ctx, db, record)
	if err != nil {
		return err
	}
	if exists {
		return failure("replication_account_unmanaged", "Generated replication account existed before its journaled create phase", 412)
	}
	statement := "CREATE USER " + databaseReplicationAccountName(record) + " IDENTIFIED BY " + databaseReplicationStringLiteral(record.AccountPassword)
	return databaseReplicationExec(ctx, db, statement)
}

func (e *PrivilegedExecutor) grantDatabaseReplicationAccount(ctx context.Context, source Instance, record databaseReplicationRecord) error {
	db, err := e.openManagedAdmin(ctx, source)
	if err != nil {
		return err
	}
	defer db.Close()
	exists, granted, err := databaseReplicationAccountGrants(ctx, db, record)
	if err != nil {
		return err
	}
	if !exists {
		return failure("replication_account_missing", "Generated replication account is missing; explicit recovery must recreate only this exact account", 409)
	}
	if granted {
		return nil
	}
	statement := "GRANT REPLICATION SLAVE ON *.* TO " + databaseReplicationAccountName(record)
	if err = databaseReplicationExec(ctx, db, statement); err != nil {
		return err
	}
	_, granted, err = databaseReplicationAccountGrants(ctx, db, record)
	if err != nil {
		return err
	}
	if !granted {
		return failure("replication_account_unmanaged", "Least-privilege replication grant did not verify after creation", 412)
	}
	return nil
}

func mysqlReplicationChannelStatement(source Instance, record databaseReplicationRecord) string {
	return "CHANGE REPLICATION SOURCE TO SOURCE_HOST='127.0.0.1', SOURCE_PORT=" + strconv.Itoa(setting(source.Config, "port", 0)) +
		", SOURCE_USER=" + databaseReplicationStringLiteral(record.Account) + ", SOURCE_PASSWORD=" + databaseReplicationStringLiteral(record.AccountPassword) +
		", SOURCE_LOG_FILE=" + databaseReplicationStringLiteral(record.SourceLogFile) + ", SOURCE_LOG_POS=" + strconv.FormatUint(record.SourceLogPosition, 10) +
		", GET_SOURCE_PUBLIC_KEY=1, SOURCE_CONNECT_RETRY=10 FOR CHANNEL '" + record.Channel + "'"
}

func mariaReplicationChannelStatement(source Instance, record databaseReplicationRecord) string {
	return "CHANGE MASTER '" + record.Channel + "' TO MASTER_HOST='127.0.0.1', MASTER_PORT=" + strconv.Itoa(setting(source.Config, "port", 0)) +
		", MASTER_USER=" + databaseReplicationStringLiteral(record.Account) + ", MASTER_PASSWORD=" + databaseReplicationStringLiteral(record.AccountPassword) +
		", MASTER_LOG_FILE=" + databaseReplicationStringLiteral(record.SourceLogFile) + ", MASTER_LOG_POS=" + strconv.FormatUint(record.SourceLogPosition, 10) +
		", MASTER_USE_GTID=no, MASTER_CONNECT_RETRY=10"
}

func (e *PrivilegedExecutor) configureDatabaseReplicationChannel(ctx context.Context, source, target Instance, record databaseReplicationRecord) error {
	if err := e.verifyReplicationConfig(ctx, source, target, record, true); err != nil {
		return err
	}
	db, err := e.openManagedAdmin(ctx, target)
	if err != nil {
		return err
	}
	defer db.Close()
	channels, err := readDatabaseReplicationChannels(ctx, db, target.PluginID)
	if err != nil {
		return err
	}
	if len(channels) == 1 && databaseReplicationChannelName(channels[0], record.Engine) == record.Channel {
		return validateReplicationChannelRowWithoutFilter(channels[0], source, record)
	}
	if len(channels) != 0 {
		return failure("replication_channel_unmanaged", "Target has a channel other than the journaled generated channel", 412)
	}
	statement := mysqlReplicationChannelStatement(source, record)
	if record.Engine == "mariadb" {
		statement = mariaReplicationChannelStatement(source, record)
	}
	if err = databaseReplicationExec(ctx, db, statement); err != nil {
		return err
	}
	channels, err = readDatabaseReplicationChannels(ctx, db, target.PluginID)
	if err != nil || len(channels) != 1 || databaseReplicationChannelName(channels[0], record.Engine) != record.Channel {
		return failure("replication_channel_unverified", "Generated replication channel could not be proved after configuration", 412)
	}
	return validateReplicationChannelRowWithoutFilter(channels[0], source, record)
}

func validateReplicationChannelRowWithoutFilter(row map[string]any, source Instance, record databaseReplicationRecord) error {
	if databaseReplicationFirstValue(row, "source_host", "master_host") != "127.0.0.1" ||
		databaseReplicationFirstValue(row, "source_user", "master_user") != record.Account ||
		databaseReplicationFirstValue(row, "source_port", "master_port") != strconv.Itoa(setting(source.Config, "port", 0)) {
		return failure("replication_channel_unmanaged", "Generated channel source identity differs from the frozen managed endpoint", 412)
	}
	if logFile := databaseReplicationFirstValue(row, "source_log_file", "master_log_file"); logFile != "" && logFile != record.SourceLogFile {
		return failure("replication_channel_unmanaged", "Generated channel binary-log file differs from the seed coordinate", 412)
	}
	return nil
}

func (e *PrivilegedExecutor) configureDatabaseReplicationFilter(ctx context.Context, target Instance, record databaseReplicationRecord) error {
	if record.Engine == "mariadb" {
		db, err := e.openManagedAdmin(ctx, target)
		if err != nil {
			return err
		}
		filter, err := readDatabaseReplicationVariable(ctx, db, "replicate_wild_do_table")
		_ = db.Close()
		if err != nil {
			return err
		}
		if filter != databaseReplicationSQLWildcard(record.Database) {
			return failure("replication_filter_unverified", "MariaDB global table filter does not match the exact owned schema", 412)
		}
		return nil
	}
	db, err := e.openManagedAdmin(ctx, target)
	if err != nil {
		return err
	}
	defer db.Close()
	channels, err := readDatabaseReplicationChannels(ctx, db, target.PluginID)
	if err != nil || len(channels) != 1 || databaseReplicationChannelName(channels[0], record.Engine) != record.Channel {
		return failure("replication_channel_unverified", "Exact MySQL channel must exist before installing its database filter", 412)
	}
	filter := databaseReplicationSQLWildcard(record.Database)
	statement := "CHANGE REPLICATION FILTER REPLICATE_WILD_DO_TABLE = (" + databaseReplicationStringLiteral(filter) + ") FOR CHANNEL '" + record.Channel + "'"
	if current := databaseReplicationFirstValue(channels[0], "replicate_wild_do_table"); current == filter {
		return nil
	}
	if err = databaseReplicationExec(ctx, db, statement); err != nil {
		return err
	}
	channels, err = readDatabaseReplicationChannels(ctx, db, target.PluginID)
	if err != nil || len(channels) != 1 || databaseReplicationFirstValue(channels[0], "replicate_wild_do_table") != filter {
		return failure("replication_filter_unverified", "MySQL did not report the exact escaped per-channel table filter", 412)
	}
	return validateReplicationChannelRow(channels[0], Instance{Config: map[string]any{"port": setting(target.Config, "port", 0)}}, record)
}

func (e *PrivilegedExecutor) startReplicationChannel(ctx context.Context, source, target Instance, record databaseReplicationRecord) error {
	db, err := e.openManagedAdmin(ctx, target)
	if err != nil {
		return err
	}
	defer db.Close()
	channels, err := readDatabaseReplicationChannels(ctx, db, target.PluginID)
	if err != nil || len(channels) != 1 || databaseReplicationChannelName(channels[0], record.Engine) != record.Channel {
		return failure("replication_channel_unverified", "Exact generated channel is unavailable before start", 412)
	}
	if err = validateReplicationChannelRow(channels[0], source, record); err != nil {
		return err
	}
	if strings.EqualFold(databaseReplicationFirstValue(channels[0], "replica_io_running", "slave_io_running"), "yes") && strings.EqualFold(databaseReplicationFirstValue(channels[0], "replica_sql_running", "slave_sql_running"), "yes") {
		return nil
	}
	if record.Engine == "mariadb" {
		return databaseReplicationExec(ctx, db, "START SLAVE '"+record.Channel+"'")
	}
	return databaseReplicationExec(ctx, db, "START REPLICA FOR CHANNEL '"+record.Channel+"'")
}

func (e *PrivilegedExecutor) stopReplicationChannel(ctx context.Context, target Instance, record databaseReplicationRecord) error {
	db, err := e.openManagedAdmin(ctx, target)
	if err != nil {
		return err
	}
	defer db.Close()
	channels, err := readDatabaseReplicationChannels(ctx, db, target.PluginID)
	if err != nil {
		return err
	}
	if len(channels) != 1 || databaseReplicationChannelName(channels[0], record.Engine) != record.Channel {
		return failure("replication_channel_unmanaged", "Target channel list no longer proves the exact generated channel", 412)
	}
	if !strings.EqualFold(databaseReplicationFirstValue(channels[0], "replica_io_running", "slave_io_running"), "yes") && !strings.EqualFold(databaseReplicationFirstValue(channels[0], "replica_sql_running", "slave_sql_running"), "yes") {
		return nil
	}
	if record.Engine == "mariadb" {
		return databaseReplicationExec(ctx, db, "STOP SLAVE '"+record.Channel+"'")
	}
	return databaseReplicationExec(ctx, db, "STOP REPLICA FOR CHANNEL '"+record.Channel+"'")
}
