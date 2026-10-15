package plugins

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type databaseReplicationRuntime struct {
	Version  string
	ServerID uint32
	LogBin   bool
	Format   string
}

type databaseReplicationSchema struct {
	Tables int
	Bytes  uint64
}

var databaseReplicationVersionPattern = regexp.MustCompile(`([0-9]+)\.([0-9]+)\.([0-9]+)`)

func databaseReplicationParseVersion(engine, version string) (int, int, int, bool) {
	canonical := canonicalDatabaseReplicationEngine(engine)
	if canonical == "" || canonical == "mariadb" && !strings.Contains(strings.ToLower(version), "mariadb") || canonical == "mysql" && strings.Contains(strings.ToLower(version), "mariadb") {
		return 0, 0, 0, false
	}
	match := databaseReplicationVersionPattern.FindStringSubmatch(version)
	if len(match) != 4 {
		return 0, 0, 0, false
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	patch, _ := strconv.Atoi(match[3])
	return major, minor, patch, true
}

func databaseReplicationVersionSupported(engine, version string) bool {
	major, minor, patch, ok := databaseReplicationParseVersion(engine, version)
	if !ok {
		return false
	}
	switch canonicalDatabaseReplicationEngine(engine) {
	case "mysql":
		return major > 8 || major == 8 && (minor > 0 || minor == 0 && patch >= 26)
	case "mariadb":
		return major > 10 || major == 10 && minor >= 6
	default:
		return false
	}
}

func readDatabaseReplicationRuntime(ctx context.Context, db *sql.DB, engine string) (databaseReplicationRuntime, error) {
	var runtime databaseReplicationRuntime
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&runtime.Version); err != nil || !databaseReplicationVersionSupported(engine, runtime.Version) {
		return runtime, failure("replication_version_unsupported", "Database server version is outside the verified replication protocol range", 412)
	}
	rows, err := db.QueryContext(ctx, "SHOW GLOBAL VARIABLES WHERE Variable_name IN ('server_id','log_bin','binlog_format')")
	if err != nil {
		return runtime, failure("replication_protocol_unsupported", "Database server did not expose required replication variables", 412)
	}
	values, err := boundedDatabaseRows(rows)
	rows.Close()
	if err != nil {
		return runtime, err
	}
	variables := map[string]string{}
	for _, row := range values {
		name, nameOK := row["Variable_name"].(string)
		value, valueOK := row["Value"].(string)
		if !nameOK || !valueOK {
			for column, field := range row {
				switch strings.ToLower(column) {
				case "variable_name":
					name, _ = field.(string)
				case "value":
					value, _ = field.(string)
				}
			}
		}
		variables[strings.ToLower(name)] = value
	}
	serverID, err := strconv.ParseUint(variables["server_id"], 10, 32)
	if err != nil || serverID == 0 {
		return runtime, failure("replication_identity_unavailable", "Database server ID is missing or outside the supported range", 412)
	}
	runtime.ServerID = uint32(serverID)
	runtime.LogBin = strings.EqualFold(variables["log_bin"], "on") || variables["log_bin"] == "1"
	runtime.Format = strings.ToUpper(variables["binlog_format"])
	return runtime, nil
}

func databaseReplicationChannelName(row map[string]any, engine string) string {
	if canonicalDatabaseReplicationEngine(engine) == "mariadb" {
		return databaseReplicationValue(row, "connection_name")
	}
	return databaseReplicationValue(row, "channel_name")
}

func databaseReplicationValue(row map[string]any, name string) string {
	for key, value := range row {
		if strings.EqualFold(key, name) {
			return fmt.Sprint(value)
		}
	}
	return ""
}

func readDatabaseReplicationChannels(ctx context.Context, db *sql.DB, engine string) ([]map[string]any, error) {
	query := "SHOW REPLICA STATUS"
	if canonicalDatabaseReplicationEngine(engine) == "mariadb" {
		query = "SHOW ALL SLAVES STATUS"
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, failure("replication_protocol_unsupported", "Database server did not expose a complete channel list", 412)
	}
	items, err := boundedDatabaseRows(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(items) > 128 {
		return nil, failure("result_limit", "Database replication channel count exceeds its bound", 413)
	}
	for _, item := range items {
		if databaseReplicationChannelName(item, engine) == "" {
			return nil, failure("replication_protocol_unsupported", "Database channel listing omitted its exact channel name", 412)
		}
	}
	return items, nil
}

func databaseReplicationScalar(ctx context.Context, db *sql.DB, query string, args ...any) (uint64, error) {
	var value uint64
	if err := db.QueryRowContext(ctx, query, args...).Scan(&value); err != nil {
		return 0, failure("replication_preflight_failed", "Managed database schema inventory could not be verified", 502)
	}
	return value, nil
}

func inspectDatabaseReplicationSchema(ctx context.Context, db *sql.DB, database string) (databaseReplicationSchema, error) {
	var schema databaseReplicationSchema
	rows, err := db.QueryContext(ctx, "SELECT TABLE_NAME, ENGINE, COALESCE(DATA_LENGTH,0), COALESCE(INDEX_LENGTH,0) FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_TYPE='BASE TABLE' ORDER BY TABLE_NAME LIMIT 1001", database)
	if err != nil {
		return schema, failure("replication_preflight_failed", "Source table inventory could not be verified", 502)
	}
	for rows.Next() {
		if schema.Tables >= 1000 {
			rows.Close()
			return schema, failure("result_limit", "Replication supports at most 1000 owned base tables", 413)
		}
		var name, engine string
		var dataLength, indexLength sql.NullInt64
		if err = rows.Scan(&name, &engine, &dataLength, &indexLength); err != nil {
			rows.Close()
			return schema, failure("replication_preflight_failed", "Source table inventory could not be decoded", 502)
		}
		if name == "" || !strings.EqualFold(engine, "InnoDB") {
			rows.Close()
			return schema, failure("replication_storage_unsupported", "Every source base table must use InnoDB for a consistent transaction snapshot", 412)
		}
		for _, length := range []sql.NullInt64{dataLength, indexLength} {
			if length.Valid && length.Int64 > 0 {
				if uint64(length.Int64) > ^uint64(0)-schema.Bytes {
					rows.Close()
					return schema, failure("result_limit", "Source schema size exceeds its numeric bound", 413)
				}
				schema.Bytes += uint64(length.Int64)
			}
		}
		schema.Tables++
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return schema, failure("replication_preflight_failed", "Source table inventory did not complete", 502)
	}
	rows.Close()
	if schema.Tables == 0 {
		return schema, failure("replication_source_empty", "Source database has no base tables to seed", 412)
	}
	if schema.Bytes > databaseReplicationMaxSeed {
		return schema, failure("result_limit", "Source database estimate exceeds the four GiB seed limit", 413)
	}
	for _, object := range []struct {
		name, query string
	}{
		{"view", "SELECT COUNT(*) FROM information_schema.VIEWS WHERE TABLE_SCHEMA=?"},
		{"trigger", "SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA=?"},
		{"routine", "SELECT COUNT(*) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA=?"},
		{"event", "SELECT COUNT(*) FROM information_schema.EVENTS WHERE EVENT_SCHEMA=?"},
	} {
		count, err := databaseReplicationScalar(ctx, db, object.query, database)
		if err != nil {
			return schema, err
		}
		if count != 0 {
			return schema, failure("replication_schema_unsupported", "Replication requires a base-table-only database; views, triggers, routines and events cannot be proven schema-isolated", 412)
		}
	}
	return schema, nil
}

func databaseReplicationLoopback(instance Instance) bool {
	bind, _ := instance.Config["bind"].(string)
	if bind == "" {
		bind = "127.0.0.1"
	}
	ip := net.ParseIP(bind)
	return ip != nil && ip.IsLoopback()
}

func (e *PrivilegedExecutor) validateDatabaseReplicationInstance(ctx context.Context, instance Instance, requireHealthy bool) error {
	if !databaseBinlogSupportedEngine(instance.PluginID) || instance.Ownership != "managed" || instance.Paths != e.instancePaths(instance.PluginID, instance.InstanceID) ||
		instance.Service.ID != instance.ID || instance.Service.PluginID != instance.PluginID || instance.Service.InstanceID != instance.InstanceID ||
		instance.Service.Unit != "powerlinux-plugin-"+instance.ID+".service" || instance.Service.ConfigPath != instance.Paths.ConfigDir ||
		instance.Service.DataPath != instance.Paths.DataDir || instance.Service.Ownership != "managed" || instance.Service.Program == "" ||
		instance.Status == "uninstalled" || !databaseReplicationLoopback(instance) {
		return failure("ownership_invalid", "Replication requires the exact owned managed loopback MySQL or MariaDB instance", 412)
	}
	port := setting(instance.Config, "port", 0)
	if port < 1024 || port > 65535 {
		return failure("invalid_config", "Managed database port is outside its supported range", 412)
	}
	if requireHealthy && e.probe(ctx, instance).Status != "healthy" {
		return failure("runtime_unavailable", "Both managed database services must be healthy before replication changes", 409)
	}
	if err := e.validateDatabaseRuntimeTools(ctx, instance); err != nil {
		return err
	}
	return nil
}

func (e *PrivilegedExecutor) loadReplicationDatabase(instance Instance, database string, revision int64) (managedDatabaseRecord, error) {
	var record managedDatabaseRecord
	if err := e.load("private", managedDatabaseKey(instance.ID, database), &record); err != nil {
		return record, failure("managed_database_required", "Both replication endpoints require an active owned database receipt", 412)
	}
	if record.InstanceID != instance.ID || record.Database != database || record.Engine != instance.PluginID || record.State != "active" {
		return record, failure("ownership_invalid", "Managed database receipt does not match the selected instance and schema", 412)
	}
	if record.Revision != revision {
		return record, failure("revision_conflict", "Managed database receipt changed; refresh both endpoint revisions", 412)
	}
	return record, nil
}

func (e *PrivilegedExecutor) replicationInstances(ctx context.Context, input DatabaseReplicationInput) (Instance, Instance, managedDatabaseRecord, managedDatabaseRecord, error) {
	source, err := e.get(ctx, input.SourceInstanceID, false)
	if err != nil {
		return Instance{}, Instance{}, managedDatabaseRecord{}, managedDatabaseRecord{}, err
	}
	target, err := e.get(ctx, input.TargetInstanceID, false)
	if err != nil {
		return Instance{}, Instance{}, managedDatabaseRecord{}, managedDatabaseRecord{}, err
	}
	if source.Revision != input.ExpectedSourceInstanceRevision || target.Revision != input.ExpectedTargetInstanceRevision {
		return source, target, managedDatabaseRecord{}, managedDatabaseRecord{}, failure("revision_conflict", "A managed instance changed; refresh its replication preview", 412)
	}
	if canonicalDatabaseReplicationEngine(source.PluginID) == "" || canonicalDatabaseReplicationEngine(source.PluginID) != canonicalDatabaseReplicationEngine(target.PluginID) {
		return source, target, managedDatabaseRecord{}, managedDatabaseRecord{}, failure("replication_engine_mismatch", "Replication requires the same verified MySQL or MariaDB engine on both endpoints", 412)
	}
	if err = e.validateDatabaseReplicationInstance(ctx, source, true); err != nil {
		return source, target, managedDatabaseRecord{}, managedDatabaseRecord{}, err
	}
	if err = e.validateDatabaseReplicationInstance(ctx, target, true); err != nil {
		return source, target, managedDatabaseRecord{}, managedDatabaseRecord{}, err
	}
	sourceRecord, err := e.loadReplicationDatabase(source, input.Database, input.ExpectedSourceDatabaseRevision)
	if err != nil {
		return source, target, sourceRecord, managedDatabaseRecord{}, err
	}
	targetRecord, err := e.loadReplicationDatabase(target, input.Database, input.ExpectedTargetDatabaseRevision)
	if err != nil {
		return source, target, sourceRecord, targetRecord, err
	}
	return source, target, sourceRecord, targetRecord, nil
}

func (e *PrivilegedExecutor) inspectReplicationPreflight(ctx context.Context, input DatabaseReplicationInput) (DatabaseReplicationPreview, error) {
	var out DatabaseReplicationPreview
	source, target, sourceRecord, targetRecord, err := e.replicationInstances(ctx, input)
	if err != nil {
		return out, err
	}
	if setting(source.Config, "port", 0) == setting(target.Config, "port", 0) {
		return out, failure("replication_port_conflict", "Loopback managed endpoints must use different local database ports", 412)
	}
	sourcePairs, err := e.databaseReplicationPairsForInstance(source.ID)
	if err != nil {
		return out, err
	}
	targetPairs, err := e.databaseReplicationPairsForInstance(target.ID)
	if err != nil {
		return out, err
	}
	records := append(sourcePairs, targetPairs...)
	for _, record := range records {
		if record.State != "removed" && (record.SourceInstanceID == source.ID || record.TargetInstanceID == source.ID || record.SourceInstanceID == target.ID || record.TargetInstanceID == target.ID) {
			return out, failure("replication_attached", "An endpoint already belongs to a managed replication pair; remove it before reuse", 409)
		}
	}
	sourceDB, err := e.openManagedAdmin(ctx, source)
	if err != nil {
		return out, err
	}
	defer sourceDB.Close()
	targetDB, err := e.openManagedAdmin(ctx, target)
	if err != nil {
		return out, err
	}
	defer targetDB.Close()
	sourceRuntime, err := readDatabaseReplicationRuntime(ctx, sourceDB, source.PluginID)
	if err != nil {
		return out, err
	}
	if _, err = readDatabaseReplicationRuntime(ctx, targetDB, target.PluginID); err != nil {
		return out, err
	}
	if !sourceRuntime.LogBin {
		return out, failure("replication_binlog_unavailable", "The managed source binary log is not active despite its owned configuration", 412)
	}
	sourceChannels, err := readDatabaseReplicationChannels(ctx, sourceDB, source.PluginID)
	if err != nil {
		return out, err
	}
	targetChannels, err := readDatabaseReplicationChannels(ctx, targetDB, target.PluginID)
	if err != nil {
		return out, err
	}
	if len(sourceChannels) != 0 || len(targetChannels) != 0 {
		return out, failure("replication_channel_unmanaged", "Pre-existing source or target replication channels will not be adopted or overwritten", 412)
	}
	if canonicalDatabaseReplicationEngine(target.PluginID) == "mariadb" {
		filters, filterErr := readDatabaseReplicationVariable(ctx, targetDB, "replicate_wild_do_table")
		if filterErr != nil {
			return out, filterErr
		}
		if filters != "" {
			return out, failure("replication_filter_unmanaged", "MariaDB target already has a global replication filter; unrelated filter behavior will not be changed", 412)
		}
	}
	sourceConfigRevision, sourceConfig, err := e.readDatabaseBinlogConfig(source)
	if err != nil {
		return out, err
	}
	targetConfigRevision, targetConfig, err := e.readDatabaseBinlogConfig(target)
	if err != nil {
		return out, err
	}
	if _, _, state := parseDatabaseBinlogConfig(sourceConfig.Body); state != "settled" {
		return out, failure("configuration_unmanaged", "Source binary-log configuration is not an exact managed state", 412)
	}
	if !hasReplicationConfigDirective(targetConfig.Body, false) {
		// Safe target baseline: no server ID or replication filter has been
		// configured outside the exact future managed marker.
	} else {
		return out, failure("configuration_unmanaged", "Target contains an unmanaged server ID or replication filter", 412)
	}
	schema, err := inspectDatabaseReplicationSchema(ctx, sourceDB, input.Database)
	if err != nil {
		return out, err
	}
	targetTables, err := databaseReplicationScalar(ctx, targetDB, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=?", input.Database)
	if err != nil {
		return out, err
	}
	if targetTables != 0 {
		return out, failure("target_not_empty", "Target database must contain no tables or views before the preview can be frozen", 409)
	}
	for _, object := range []struct {
		name, query string
	}{
		{"trigger", "SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA=?"},
		{"routine", "SELECT COUNT(*) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA=?"},
		{"event", "SELECT COUNT(*) FROM information_schema.EVENTS WHERE EVENT_SCHEMA=?"},
	} {
		count, countErr := databaseReplicationScalar(ctx, targetDB, object.query, input.Database)
		if countErr != nil {
			return out, countErr
		}
		if count != 0 {
			return out, failure("target_not_empty", "Target database must contain no triggers, routines or events", 409)
		}
	}
	if sourceRecord.State != "active" {
		return out, failure("managed_database_required", "Source database ownership is not active", 412)
	}
	pairID, previewID, err := newDatabaseReplicationIDs(records, source.ID, target.ID)
	if err != nil {
		return out, err
	}
	serverIDSource, serverIDTarget, err := databaseReplicationServerIDs(pairID)
	if err != nil {
		return out, err
	}
	created := time.Now().UTC()
	preview := databaseReplicationPreviewRecord{
		Version: 1, PairID: pairID, PreviewID: previewID, SourceInstanceID: source.ID, TargetInstanceID: target.ID, Database: input.Database,
		Engine: canonicalDatabaseReplicationEngine(source.PluginID), SourceInstanceRevision: source.Revision, TargetInstanceRevision: target.Revision,
		SourceDatabaseRevision: sourceRecord.Revision, TargetDatabaseRevision: targetRecord.Revision,
		SourceConfigRevision: sourceConfigRevision, TargetConfigRevision: targetConfigRevision, SourceServerID: serverIDSource, TargetServerID: serverIDTarget,
		EstimatedSeedBytes: schema.Bytes, TableCount: schema.Tables, CreatedAt: created, ExpiresAt: created.Add(databaseReplicationPreviewTTL),
	}
	digest, err := databaseReplicationPreviewDigest(preview)
	if err != nil {
		return out, err
	}
	preview.SnapshotSHA256 = digest
	if !validDatabaseReplicationPreview(preview) {
		return out, failure("replication_preview_invalid", "A verified replication preview could not be frozen", 500)
	}
	if err = e.save("private", databaseReplicationPreviewKey(pairID), preview); err != nil {
		return out, failure("replication_preview_storage_failed", "Protected replication preview could not be saved", 502)
	}
	return databaseReplicationPreviewPublic(preview), nil
}

func newDatabaseReplicationIDs(existing []databaseReplicationRecord, sourceID, targetID string) (string, string, error) {
	used := map[uint32]bool{}
	for _, record := range existing {
		used[record.SourceServerID] = true
		used[record.TargetServerID] = true
	}
	for attempt := 0; attempt < 16; attempt++ {
		var pairRaw [16]byte
		if _, err := rand.Read(pairRaw[:]); err != nil {
			return "", "", err
		}
		pairID := hex.EncodeToString(pairRaw[:])
		sourceServerID, targetServerID, err := databaseReplicationServerIDs(pairID)
		if err != nil || used[sourceServerID] || used[targetServerID] {
			continue
		}
		var previewRaw [16]byte
		if _, err = rand.Read(previewRaw[:]); err != nil {
			return "", "", err
		}
		return pairID, hex.EncodeToString(previewRaw[:]), nil
	}
	return "", "", failure("identity_generation_failed", "Unique managed replication identities could not be generated", 500)
}

func databaseReplicationPreviewDigest(preview databaseReplicationPreviewRecord) (string, error) {
	preview.SnapshotSHA256 = ""
	encoded, err := json.Marshal(preview)
	if err != nil {
		return "", err
	}
	return sha256Hex(encoded), nil
}

func databaseReplicationPreviewPublic(preview databaseReplicationPreviewRecord) DatabaseReplicationPreview {
	return DatabaseReplicationPreview{
		PairID: preview.PairID, PreviewID: preview.PreviewID, ExpiresAt: preview.ExpiresAt, SourceInstanceID: preview.SourceInstanceID,
		TargetInstanceID: preview.TargetInstanceID, Database: preview.Database, Engine: preview.Engine,
		SourceInstanceRevision: preview.SourceInstanceRevision, TargetInstanceRevision: preview.TargetInstanceRevision,
		SourceDatabaseRevision: preview.SourceDatabaseRevision, TargetDatabaseRevision: preview.TargetDatabaseRevision,
		SourceConfigRevision: preview.SourceConfigRevision, TargetConfigRevision: preview.TargetConfigRevision,
		SourceServerID: preview.SourceServerID, TargetServerID: preview.TargetServerID, EstimatedSeedBytes: preview.EstimatedSeedBytes,
		TableCount: preview.TableCount, SnapshotSHA256: preview.SnapshotSHA256, ConfirmEmptyTarget: true,
		ConfirmReplicaHistoryLoss: true, TargetWillBeReadOnly: false,
	}
}

func readDatabaseReplicationVariable(ctx context.Context, db *sql.DB, name string) (string, error) {
	if !regexp.MustCompile(`^[a-z_]{1,64}$`).MatchString(name) {
		return "", failure("invalid_input", "Invalid replication variable", 400)
	}
	// The variable name is internal and validated above. MySQL and MariaDB do
	// not consistently support placeholders in SHOW statements.
	rows, err := db.QueryContext(ctx, "SHOW GLOBAL VARIABLES LIKE '"+name+"'")
	if err != nil {
		return "", failure("replication_protocol_unsupported", "Database replication variable could not be inspected", 412)
	}
	items, err := boundedDatabaseRows(rows)
	rows.Close()
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "", nil
	}
	if len(items) != 1 {
		return "", failure("replication_protocol_unsupported", "Database returned duplicate replication variables", 412)
	}
	return databaseReplicationValue(items[0], "value"), nil
}
