package plugins

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

const (
	databaseBinlogActionOperation = "database_binlog_action"
	databaseBinlogMaxList         = 500
	databaseBinlogMaxObserved     = 10000
	databaseBinlogMaxSnapshot     = 128
	databaseBinlogPreviewLifetime = 15 * time.Minute
	databaseBinlogConfigMarker    = "# PowerLinux managed binary log configuration v1"
)

var (
	databaseBinlogRevisionPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	databaseBinlogPreviewPattern  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	databaseBinlogNamePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)
)

// DatabaseBinlogInput is a closed protected-Agent command protocol. Callers
// cannot provide SQL, paths, credentials, or server-generated log identifiers.
type DatabaseBinlogInput struct {
	InstanceID                string   `json:"instance_id"`
	Command                   string   `json:"command"`
	Limit                     int      `json:"limit,omitempty"`
	After                     string   `json:"after,omitempty"`
	Enabled                   bool     `json:"enabled,omitempty"`
	ExpectedConfigRevision    string   `json:"expected_config_revision,omitempty"`
	ExpectedInstanceRevision  int64    `json:"expected_instance_revision,omitempty"`
	RetainFrom                string   `json:"retain_from,omitempty"`
	PreviewID                 string   `json:"preview_id,omitempty"`
	SnapshotSHA256            string   `json:"snapshot_sha256,omitempty"`
	BinlogsToPurge            []string `json:"binlogs_to_purge,omitempty"`
	ConfirmReplicaHistoryLoss bool     `json:"confirm_replica_history_loss,omitempty"`
}

type DatabaseBinlogLog struct {
	Name      string `json:"name"`
	SizeBytes uint64 `json:"size_bytes"`
}

type DatabaseBinlogStatus struct {
	InstanceID           string `json:"instance_id"`
	Engine               string `json:"engine"`
	Enabled              bool   `json:"enabled"`
	Format               string `json:"format"`
	Basename             string `json:"basename"`
	ActiveLog            string `json:"active_log"`
	ServerID             string `json:"server_id"`
	ExpirationSeconds    uint64 `json:"expiration_seconds"`
	ConfigRevision       string `json:"config_revision"`
	InstanceRevision     int64  `json:"instance_revision"`
	ConfiguredEnabled    *bool  `json:"configured_enabled,omitempty"`
	ConfigurationManaged bool   `json:"configuration_managed"`
	ConfigurationState   string `json:"configuration_state"`
}

type DatabaseBinlogList struct {
	InstanceID       string              `json:"instance_id"`
	Items            []DatabaseBinlogLog `json:"items"`
	Total            int                 `json:"total"`
	Truncated        bool                `json:"truncated"`
	NextAfter        string              `json:"next_after,omitempty"`
	ActiveLog        string              `json:"active_log"`
	ConfigRevision   string              `json:"config_revision"`
	InstanceRevision int64               `json:"instance_revision"`
}

type DatabaseBinlogPurgePreview struct {
	PreviewID                string              `json:"preview_id"`
	ExpiresAt                time.Time           `json:"expires_at"`
	RetainFrom               string              `json:"retain_from"`
	ActiveLog                string              `json:"active_log"`
	BinlogsToPurge           []DatabaseBinlogLog `json:"binlogs_to_purge"`
	BytesToPurge             uint64              `json:"bytes_to_purge"`
	SnapshotSHA256           string              `json:"snapshot_sha256"`
	ExpectedConfigRevision   string              `json:"expected_config_revision"`
	ExpectedInstanceRevision int64               `json:"expected_instance_revision"`
	ReplicaSafety            string              `json:"replica_safety"`
	ConfirmationRequired     bool                `json:"confirmation_required"`
}

type DatabaseBinlogPurgeResult struct {
	InstanceID     string              `json:"instance_id"`
	RetainFrom     string              `json:"retain_from"`
	ActiveLog      string              `json:"active_log"`
	Purged         []DatabaseBinlogLog `json:"purged"`
	BytesPurged    uint64              `json:"bytes_purged"`
	ReplicaSafety  string              `json:"replica_safety"`
	SnapshotSHA256 string              `json:"snapshot_sha256"`
}

type databaseBinlogSnapshot struct {
	Version          int                 `json:"version"`
	InstanceID       string              `json:"instance_id"`
	Engine           string              `json:"engine"`
	PreviewID        string              `json:"preview_id"`
	RetainFrom       string              `json:"retain_from"`
	ActiveLog        string              `json:"active_log"`
	Logs             []DatabaseBinlogLog `json:"logs"`
	Candidates       []DatabaseBinlogLog `json:"candidates"`
	ConfigRevision   string              `json:"config_revision"`
	InstanceRevision int64               `json:"instance_revision"`
	CreatedAt        time.Time           `json:"created_at"`
	ExpiresAt        time.Time           `json:"expires_at"`
	SnapshotSHA256   string              `json:"snapshot_sha256"`
}

type databaseBinlogConfigIntent struct {
	Version          int       `json:"version"`
	InstanceID       string    `json:"instance_id"`
	Engine           string    `json:"engine"`
	ConfigDir        string    `json:"config_dir"`
	InstanceRevision int64     `json:"instance_revision"`
	ExpectedRevision string    `json:"expected_revision"`
	FromEnabled      bool      `json:"from_enabled"`
	ToEnabled        bool      `json:"to_enabled"`
	Before           []byte    `json:"before"`
	After            []byte    `json:"after"`
	State            string    `json:"state"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type databaseBinlogRecoveryContextKey struct{}
type databaseBinlogRecoveryContext struct {
	executor   *PrivilegedExecutor
	instanceID string
}

func databaseBinlogSupportedEngine(engine string) bool {
	switch engine {
	case "mysql", "mysql-community", "mariadb":
		return true
	default:
		return false
	}
}

func validateDatabaseBinlogInput(raw json.RawMessage) error {
	var in DatabaseBinlogInput
	if err := parse(raw, &in); err != nil {
		return err
	}
	if !validInstanceID(in.InstanceID) {
		return failure("invalid_input", "A valid managed MySQL or MariaDB instance is required", 400)
	}
	validRevision := func() bool {
		return databaseBinlogRevisionPattern.MatchString(in.ExpectedConfigRevision) && in.ExpectedInstanceRevision > 0
	}
	switch in.Command {
	case "status":
		if in.Limit != 0 || in.After != "" || in.Enabled || in.ExpectedConfigRevision != "" || in.ExpectedInstanceRevision != 0 || in.RetainFrom != "" || in.PreviewID != "" || in.SnapshotSHA256 != "" || len(in.BinlogsToPurge) != 0 || in.ConfirmReplicaHistoryLoss {
			return failure("invalid_input", "Binlog status accepts only an instance ID", 400)
		}
	case "list":
		if in.Limit < 0 || in.Limit > databaseBinlogMaxList || (in.After != "" && !databaseBinlogNamePattern.MatchString(in.After)) || in.Enabled || in.ExpectedConfigRevision != "" || in.ExpectedInstanceRevision != 0 || in.RetainFrom != "" || in.PreviewID != "" || in.SnapshotSHA256 != "" || len(in.BinlogsToPurge) != 0 || in.ConfirmReplicaHistoryLoss {
			return failure("invalid_input", "Binlog listing accepts only an instance ID", 400)
		}
	case "configure":
		if in.Limit != 0 || in.After != "" || !validRevision() || in.RetainFrom != "" || in.PreviewID != "" || in.SnapshotSHA256 != "" || len(in.BinlogsToPurge) != 0 || in.ConfirmReplicaHistoryLoss {
			return failure("invalid_input", "Binlog configuration requires current config and instance revisions", 400)
		}
	case "preview_purge":
		if in.Limit != 0 || in.After != "" || !databaseBinlogNamePattern.MatchString(in.RetainFrom) || in.Enabled || in.ExpectedConfigRevision != "" || in.ExpectedInstanceRevision != 0 || in.PreviewID != "" || in.SnapshotSHA256 != "" || len(in.BinlogsToPurge) != 0 || in.ConfirmReplicaHistoryLoss {
			return failure("invalid_input", "Purge preview requires a concrete retained binlog name", 400)
		}
	case "purge":
		if in.Limit != 0 || in.After != "" || !validRevision() || !databaseBinlogPreviewPattern.MatchString(in.PreviewID) || !databaseBinlogRevisionPattern.MatchString(in.SnapshotSHA256) || !in.ConfirmReplicaHistoryLoss || in.RetainFrom != "" || in.Enabled || len(in.BinlogsToPurge) == 0 || len(in.BinlogsToPurge) > databaseBinlogMaxSnapshot {
			return failure("invalid_input", "Purge requires an exact preview snapshot, current revisions and explicit replica-history-loss confirmation", 400)
		}
		seen := map[string]bool{}
		for _, name := range in.BinlogsToPurge {
			if !databaseBinlogNamePattern.MatchString(name) || seen[name] {
				return failure("invalid_input", "Purge binlog names must be distinct concrete server names", 400)
			}
			seen[name] = true
		}
	case "recover_config":
		if in.Limit != 0 || in.After != "" || !validRevision() || (in.RetainFrom != "apply_pending" && in.RetainFrom != "restore_previous") || in.Enabled || in.PreviewID != "" || in.SnapshotSHA256 != "" || len(in.BinlogsToPurge) != 0 || in.ConfirmReplicaHistoryLoss {
			return failure("invalid_input", "Configuration recovery requires an explicit command and current revisions", 400)
		}
	default:
		return failure("invalid_input", "Unsupported typed binary-log command", 400)
	}
	return nil
}

func (e *PrivilegedExecutor) validateDatabaseBinlog(raw json.RawMessage) error {
	if err := validateDatabaseBinlogInput(raw); err != nil {
		return err
	}
	var in DatabaseBinlogInput
	_ = json.Unmarshal(raw, &in)
	if in.Command == "purge" && len(raw) > 48<<10 {
		return failure("invalid_input", "Frozen binlog purge descriptor exceeds its bound", 413)
	}
	return nil
}

func (e *PrivilegedExecutor) databaseBinlog(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := e.validateDatabaseBinlog(raw); err != nil {
		return nil, err
	}
	var in DatabaseBinlogInput
	_ = json.Unmarshal(raw, &in)
	unlock, err := e.lockContext(ctx, in.InstanceID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if in.Command != "status" && in.Command != "list" && in.Command != "recover_config" {
		if err = e.ensureInstallFinalized(ctx, in.InstanceID); err != nil {
			return nil, err
		}
	}
	instance, err := e.get(ctx, in.InstanceID, false)
	if err != nil {
		return nil, err
	}
	if err = e.validateManagedBinlogInstance(instance); err != nil {
		return nil, err
	}
	if instance.Status == "uninstalled" {
		return nil, failure("runtime_unavailable", "Database software is uninstalled", 412)
	}
	if in.Command == "configure" || in.Command == "recover_config" || in.Command == "purge" {
		if err = e.ensureDatabaseReplicationDetached(ctx, instance.ID); err != nil {
			return nil, err
		}
	}
	if in.Command == "recover_config" {
		return e.recoverDatabaseBinlogConfig(ctx, instance, in)
	}
	if in.Command == "status" || in.Command == "list" || in.Command == "preview_purge" {
		return e.readManagedDatabaseBinlog(ctx, instance, in)
	}
	if in.Command == "configure" {
		return e.configureManagedDatabaseBinlog(ctx, instance, in)
	}
	if in.Command == "purge" {
		return e.purgeManagedDatabaseBinlogs(ctx, instance, in)
	}
	return nil, failure("invalid_input", "Unsupported typed binary-log command", 400)
}

func (e *PrivilegedExecutor) validateManagedBinlogInstance(instance Instance) error {
	if !databaseBinlogSupportedEngine(instance.PluginID) {
		return failure("unsupported_database", "Binary-log actions are supported only by managed MySQL and MariaDB instances", 412)
	}
	if instance.Ownership != "managed" || instance.Paths != e.instancePaths(instance.PluginID, instance.InstanceID) ||
		instance.Service.ID != instance.ID || instance.Service.PluginID != instance.PluginID || instance.Service.InstanceID != instance.InstanceID ||
		instance.Service.Unit != "powerlinux-plugin-"+instance.ID+".service" || instance.Service.ConfigPath != instance.Paths.ConfigDir ||
		instance.Service.DataPath != instance.Paths.DataDir || instance.Service.Ownership != "managed" || instance.Service.Program == "" {
		return failure("ownership_invalid", "Binary-log actions require the exact owned managed MySQL or MariaDB runtime and service", 412)
	}
	return nil
}

func (e *PrivilegedExecutor) readManagedDatabaseBinlog(ctx context.Context, instance Instance, in DatabaseBinlogInput) (any, error) {
	db, err := e.openManagedAdmin(ctx, instance)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	state, err := readDatabaseBinlogSQL(ctx, db, instance)
	if err != nil {
		return nil, err
	}
	configRevision, configFile, configErr := e.readDatabaseBinlogConfig(instance)
	state.ConfigRevision = configRevision
	state.InstanceRevision = instance.Revision
	if configErr != nil {
		state.ConfigurationManaged = false
		state.ConfigurationState = "configuration_unmanaged"
	} else {
		configured, managed, stateName := parseDatabaseBinlogConfig(configFile.Body)
		state.ConfigurationManaged = managed
		state.ConfigurationState = stateName
		if managed {
			state.ConfiguredEnabled = &configured
			if configured != state.Enabled && stateName == "settled" {
				state.ConfigurationState = "runtime_config_mismatch"
			}
		}
	}
	intent, loadErr := e.loadDatabaseBinlogConfigIntent(instance)
	if loadErr != nil {
		return nil, loadErr
	}
	if intent != nil && intent.State == "pending" {
		state.ConfigurationState = "recovery_required"
		state.ConfigurationManaged = false
	}
	if in.Command == "status" {
		return state, nil
	}
	if in.Command == "list" {
		limit := in.Limit
		if limit == 0 {
			limit = databaseBinlogMaxList
		}
		logs := []DatabaseBinlogLog{}
		total := 0
		truncated := false
		nextAfter := ""
		if state.Enabled {
			logs, total, truncated, nextAfter, err = readDatabaseBinlogListPage(ctx, db, limit, in.After)
			if err != nil {
				return nil, err
			}
		}
		return DatabaseBinlogList{InstanceID: instance.ID, Items: logs, Total: total, Truncated: truncated, NextAfter: nextAfter, ActiveLog: state.ActiveLog, ConfigRevision: configRevision, InstanceRevision: instance.Revision}, nil
	}
	logs := []DatabaseBinlogLog{}
	if state.Enabled {
		logs, err = readDatabaseBinlogLogs(ctx, db, databaseBinlogMaxList)
		if err != nil {
			return nil, err
		}
	}
	if !state.Enabled {
		return nil, failure("binlog_disabled", "Binary logging is disabled; enable it before creating a purge preview", 409)
	}
	if configErr != nil {
		return nil, failure("configuration_unmanaged", "Binary-log purge requires a safe revision of the owned managed configuration", 412)
	}
	if len(logs) > databaseBinlogMaxSnapshot {
		return nil, failure("result_limit", "Purge previews are limited to 128 ordered binlog files", 413)
	}
	return e.createDatabaseBinlogPurgePreview(instance, state, logs, in.RetainFrom)
}

func readDatabaseBinlogSQL(ctx context.Context, db *sql.DB, instance Instance) (DatabaseBinlogStatus, error) {
	rows, err := db.QueryContext(ctx, "SHOW GLOBAL VARIABLES WHERE Variable_name IN ('log_bin','log_bin_basename','binlog_format','binlog_expire_logs_seconds','expire_logs_days','server_id')")
	if err != nil {
		return DatabaseBinlogStatus{}, failure("query_failed", "MySQL or MariaDB did not expose binary-log variables", 502)
	}
	variables, err := boundedDatabaseRows(rows)
	rows.Close()
	if err != nil {
		return DatabaseBinlogStatus{}, err
	}
	values := map[string]string{}
	for _, row := range variables {
		name, value := "", ""
		for column, field := range row {
			switch strings.ToLower(column) {
			case "variable_name":
				name, _ = field.(string)
			case "value":
				value, _ = field.(string)
			}
		}
		values[strings.ToLower(name)] = value
	}
	state := DatabaseBinlogStatus{InstanceID: instance.ID, Engine: instance.PluginID, Enabled: strings.EqualFold(values["log_bin"], "on") || values["log_bin"] == "1", Format: values["binlog_format"], ServerID: values["server_id"], ConfigurationState: "settled"}
	if basename := values["log_bin_basename"]; basename != "" {
		state.Basename = filepath.Base(basename)
	}
	if seconds, parseErr := strconv.ParseUint(values["binlog_expire_logs_seconds"], 10, 64); parseErr == nil {
		state.ExpirationSeconds = seconds
	} else if days, parseErr := strconv.ParseFloat(values["expire_logs_days"], 64); parseErr == nil && days > 0 {
		state.ExpirationSeconds = uint64(days * 24 * 60 * 60)
	}
	if state.Enabled {
		state.ActiveLog, err = readDatabaseBinlogActive(ctx, db)
		if err != nil {
			return DatabaseBinlogStatus{}, err
		}
	}
	return state, nil
}

// queryDatabaseBinlogProtocol keeps the real MySQL/MariaDB SHOW sequence
// narrow enough for protocol fixtures without substituting filesystem scans.
func queryDatabaseBinlogProtocol(ctx context.Context, db *sql.DB, instance Instance) (DatabaseBinlogStatus, []DatabaseBinlogLog, error) {
	state, err := readDatabaseBinlogSQL(ctx, db, instance)
	if err != nil || !state.Enabled {
		return state, []DatabaseBinlogLog{}, err
	}
	logs, err := readDatabaseBinlogLogs(ctx, db, databaseBinlogMaxList)
	return state, logs, err
}

func readDatabaseBinlogActive(ctx context.Context, db *sql.DB) (string, error) {
	for _, query := range []string{"SHOW MASTER STATUS", "SHOW BINARY LOG STATUS"} {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			continue
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			continue
		}
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if !rows.Next() {
			err = rows.Err()
			rows.Close()
			if err == nil {
				return "", nil
			}
			continue
		}
		if err = rows.Scan(dest...); err != nil {
			rows.Close()
			continue
		}
		rows.Close()
		for i, column := range columns {
			if strings.EqualFold(column, "File") {
				name := databaseBinlogString(values[i])
				if !databaseBinlogNamePattern.MatchString(name) {
					return "", failure("query_failed", "MySQL or MariaDB returned an invalid active binary-log name", 502)
				}
				return name, nil
			}
		}
		return "", failure("query_failed", "MySQL or MariaDB active-log response omitted its file name", 502)
	}
	return "", failure("query_failed", "MySQL or MariaDB active binary-log status is unavailable", 502)
}

func readDatabaseBinlogLogs(ctx context.Context, db *sql.DB, limit int) ([]DatabaseBinlogLog, error) {
	if limit < 1 || limit > databaseBinlogMaxList {
		return nil, failure("result_limit", "Binary-log listing limit is outside its bound", 413)
	}
	rows, err := db.QueryContext(ctx, "SHOW BINARY LOGS")
	if err != nil {
		return nil, failure("query_failed", "MySQL or MariaDB binary-log list is unavailable", 502)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil || len(columns) < 2 || len(columns) > 16 {
		return nil, failure("query_failed", "MySQL or MariaDB returned an invalid binary-log list", 502)
	}
	nameIndex, sizeIndex := -1, -1
	for i, column := range columns {
		switch strings.ToLower(column) {
		case "log_name":
			nameIndex = i
		case "file_size":
			sizeIndex = i
		}
	}
	if nameIndex < 0 || sizeIndex < 0 {
		return nil, failure("query_failed", "MySQL or MariaDB binary-log response omitted name or size", 502)
	}
	logs := []DatabaseBinlogLog{}
	seen := map[string]bool{}
	for rows.Next() {
		if len(logs) >= limit {
			return nil, failure("result_limit", "Binary-log result exceeds its bounded row limit", 413)
		}
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err = rows.Scan(dest...); err != nil {
			return nil, failure("query_failed", "MySQL or MariaDB binary-log row could not be decoded", 502)
		}
		name := databaseBinlogString(values[nameIndex])
		size, parseErr := strconv.ParseUint(databaseBinlogString(values[sizeIndex]), 10, 64)
		if !databaseBinlogNamePattern.MatchString(name) || parseErr != nil || seen[name] {
			return nil, failure("query_failed", "MySQL or MariaDB returned an invalid binary-log name or size", 502)
		}
		seen[name] = true
		logs = append(logs, DatabaseBinlogLog{Name: name, SizeBytes: size})
	}
	if err = rows.Err(); err != nil {
		return nil, failure("query_failed", "MySQL or MariaDB binary-log list was interrupted", 502)
	}
	return logs, nil
}

func readDatabaseBinlogListPage(ctx context.Context, db *sql.DB, limit int, after string) ([]DatabaseBinlogLog, int, bool, string, error) {
	if limit < 1 || limit > databaseBinlogMaxList || (after != "" && !databaseBinlogNamePattern.MatchString(after)) {
		return nil, 0, false, "", failure("invalid_input", "Binary-log page cursor or limit is invalid", 400)
	}
	rows, err := db.QueryContext(ctx, "SHOW BINARY LOGS")
	if err != nil {
		return nil, 0, false, "", failure("query_failed", "MySQL or MariaDB binary-log list is unavailable", 502)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil || len(columns) < 2 || len(columns) > 16 {
		return nil, 0, false, "", failure("query_failed", "MySQL or MariaDB returned an invalid binary-log list", 502)
	}
	nameIndex, sizeIndex := -1, -1
	for i, column := range columns {
		switch strings.ToLower(column) {
		case "log_name":
			nameIndex = i
		case "file_size":
			sizeIndex = i
		}
	}
	if nameIndex < 0 || sizeIndex < 0 {
		return nil, 0, false, "", failure("query_failed", "MySQL or MariaDB binary-log response omitted name or size", 502)
	}
	items := make([]DatabaseBinlogLog, 0, limit+1)
	seen := map[string]bool{}
	cursorFound := after == ""
	total := 0
	for rows.Next() {
		if total >= databaseBinlogMaxObserved {
			return nil, 0, false, "", failure("result_limit", "Binary-log listing exceeds the 10,000-row scan bound", 413)
		}
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err = rows.Scan(dest...); err != nil {
			return nil, 0, false, "", failure("query_failed", "MySQL or MariaDB binary-log row could not be decoded", 502)
		}
		name := databaseBinlogString(values[nameIndex])
		size, parseErr := strconv.ParseUint(databaseBinlogString(values[sizeIndex]), 10, 64)
		if !databaseBinlogNamePattern.MatchString(name) || parseErr != nil || seen[name] {
			return nil, 0, false, "", failure("query_failed", "MySQL or MariaDB returned a duplicate or invalid binary-log name or size", 502)
		}
		seen[name] = true
		total++
		if !cursorFound {
			if name == after {
				cursorFound = true
			}
			continue
		}
		if after != "" && name == after {
			continue
		}
		if len(items) < limit+1 {
			items = append(items, DatabaseBinlogLog{Name: name, SizeBytes: size})
		}
	}
	if err = rows.Err(); err != nil {
		return nil, 0, false, "", failure("query_failed", "MySQL or MariaDB binary-log list was interrupted", 502)
	}
	if !cursorFound {
		return nil, 0, false, "", failure("stale_page_cursor", "The requested binlog page cursor is no longer present; refresh the list", 412)
	}
	truncated := len(items) > limit
	nextAfter := ""
	if truncated {
		items = items[:limit]
		nextAfter = items[len(items)-1].Name
	}
	return items, total, truncated, nextAfter, nil
}

func databaseBinlogString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case uint64:
		return strconv.FormatUint(typed, 10)
	default:
		return fmt.Sprint(value)
	}
}

func (e *PrivilegedExecutor) readDatabaseBinlogConfig(instance Instance) (string, oldProfileFile, error) {
	file, err := readProfileFile(instance.Paths.ConfigDir, "my.cnf")
	if err != nil {
		return "", oldProfileFile{}, failure("configuration_unmanaged", "Owned MySQL configuration is unavailable or unsafe", 412)
	}
	if file.Mode != 0640 || file.GID != int(e.gid) {
		return "", oldProfileFile{}, failure("configuration_unmanaged", "Owned MySQL configuration mode or group changed", 412)
	}
	digest := sha256.Sum256(file.Body)
	return hex.EncodeToString(digest[:]), file, nil
}

func parseDatabaseBinlogConfig(body []byte) (enabled bool, managed bool, state string) {
	count := strings.Count(string(body), databaseBinlogConfigMarker)
	if count == 0 {
		if hasBinlogDirective(body) {
			return false, false, "configuration_unmanaged"
		}
		return false, true, "settled"
	}
	if count != 1 {
		return false, false, "configuration_unmanaged"
	}
	marker := databaseBinlogConfigMarker + "\n[mysqld]\n"
	index := strings.LastIndex(string(body), marker)
	if index < 0 {
		return false, false, "configuration_unmanaged"
	}
	switch string(body[index:]) {
	case marker + "log-bin=mysql-bin\nbinlog_format=mixed\n":
		return true, true, "settled"
	case marker + "skip-log-bin\n":
		return false, true, "settled"
	default:
		return false, false, "configuration_unmanaged"
	}
}

func hasBinlogDirective(body []byte) bool {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(strings.SplitN(line, "=", 2)[0]))
		key = strings.TrimPrefix(key, "loose-")
		switch strings.ReplaceAll(key, "_", "-") {
		case "log-bin", "log-bin-index", "skip-log-bin", "disable-log-bin", "binlog-format":
			return true
		}
	}
	return false
}

func setDatabaseBinlogConfig(body []byte, enabled bool) ([]byte, error) {
	configured, managed, state := parseDatabaseBinlogConfig(body)
	if !managed || state != "settled" {
		return nil, failure("configuration_unmanaged", "Only the exact managed binary-log block can be changed; inspect my.cnf before retrying", 412)
	}
	if configured == enabled {
		return append([]byte(nil), body...), nil
	}
	block := databaseBinlogConfigMarker + "\n[mysqld]\nskip-log-bin\n"
	if enabled {
		block = databaseBinlogConfigMarker + "\n[mysqld]\nlog-bin=mysql-bin\nbinlog_format=mixed\n"
	}
	if index := strings.Index(string(body), databaseBinlogConfigMarker); index >= 0 {
		return append(append([]byte(nil), body[:index]...), []byte(block)...), nil
	}
	result := append([]byte(nil), body...)
	if len(result) > 0 && result[len(result)-1] != '\n' {
		result = append(result, '\n')
	}
	result = append(result, '\n')
	return append(result, []byte(block)...), nil
}

func databaseBinlogConfigIntentID(instance Instance) string {
	digest := sha256.Sum256([]byte(instance.ID))
	return "database_binlog_config_" + hex.EncodeToString(digest[:16])
}

func databaseBinlogSnapshotID(instance Instance) string {
	digest := sha256.Sum256([]byte(instance.ID))
	return "database_binlog_purge_" + hex.EncodeToString(digest[:16])
}

func validDatabaseBinlogConfigIntent(instance Instance, intent databaseBinlogConfigIntent) bool {
	if intent.Version != 1 || intent.InstanceID != instance.ID || intent.Engine != instance.PluginID || intent.ConfigDir != instance.Paths.ConfigDir ||
		intent.InstanceRevision > instance.Revision || (intent.State == "pending" && intent.InstanceRevision != instance.Revision) || !databaseBinlogRevisionPattern.MatchString(intent.ExpectedRevision) ||
		len(intent.Before) == 0 || len(intent.Before) > 128<<10 || len(intent.After) == 0 || len(intent.After) > 128<<10 ||
		(intent.State != "pending" && intent.State != "completed" && intent.State != "restored") {
		return false
	}
	beforeDigest := sha256.Sum256(intent.Before)
	beforeEnabled, beforeManaged, beforeState := parseDatabaseBinlogConfig(intent.Before)
	afterEnabled, afterManaged, afterState := parseDatabaseBinlogConfig(intent.After)
	return hex.EncodeToString(beforeDigest[:]) == intent.ExpectedRevision && beforeManaged && beforeState == "settled" && beforeEnabled == intent.FromEnabled &&
		afterManaged && afterState == "settled" && afterEnabled == intent.ToEnabled
}

func validDatabaseBinlogSnapshot(snapshot databaseBinlogSnapshot) bool {
	if snapshot.Version != 1 || !validInstanceID(snapshot.InstanceID) || !databaseBinlogSupportedEngine(snapshot.Engine) ||
		!databaseBinlogPreviewPattern.MatchString(snapshot.PreviewID) || !databaseBinlogNamePattern.MatchString(snapshot.RetainFrom) ||
		!databaseBinlogNamePattern.MatchString(snapshot.ActiveLog) || !databaseBinlogRevisionPattern.MatchString(snapshot.ConfigRevision) ||
		snapshot.InstanceRevision <= 0 || len(snapshot.Logs) < 2 || len(snapshot.Logs) > databaseBinlogMaxSnapshot ||
		len(snapshot.Candidates) == 0 || len(snapshot.Candidates) >= len(snapshot.Logs) || snapshot.CreatedAt.IsZero() ||
		!snapshot.ExpiresAt.After(snapshot.CreatedAt) || snapshot.ExpiresAt.After(snapshot.CreatedAt.Add(databaseBinlogPreviewLifetime)) {
		return false
	}
	retainIndex, activeIndex := -1, -1
	seen := make(map[string]bool, len(snapshot.Logs))
	for i, log := range snapshot.Logs {
		if !databaseBinlogNamePattern.MatchString(log.Name) || seen[log.Name] {
			return false
		}
		seen[log.Name] = true
		if log.Name == snapshot.RetainFrom {
			retainIndex = i
		}
		if log.Name == snapshot.ActiveLog {
			activeIndex = i
		}
	}
	if retainIndex <= 0 || activeIndex < retainIndex || len(snapshot.Candidates) != retainIndex {
		return false
	}
	for i, candidate := range snapshot.Candidates {
		if candidate != snapshot.Logs[i] || candidate.Name == snapshot.ActiveLog {
			return false
		}
	}
	return true
}

func databaseBinlogWithIntentContext(ctx context.Context, executor *PrivilegedExecutor, id string) context.Context {
	return context.WithValue(ctx, databaseBinlogRecoveryContextKey{}, databaseBinlogRecoveryContext{executor: executor, instanceID: id})
}

func (e *PrivilegedExecutor) ensureDatabaseBinlogConfigFinalized(ctx context.Context, id string) error {
	if marker, ok := ctx.Value(databaseBinlogRecoveryContextKey{}).(databaseBinlogRecoveryContext); ok && marker.executor == e && marker.instanceID == id {
		return nil
	}
	var instance Instance
	if err := e.load("receipts", id, &instance); core.ErrorCode(err) == "not_found" {
		return nil
	} else if err != nil {
		return err
	}
	if !databaseBinlogSupportedEngine(instance.PluginID) {
		return nil
	}
	intent, err := e.loadDatabaseBinlogConfigIntent(instance)
	if err != nil {
		return err
	}
	if intent != nil && intent.State == "pending" {
		return failure("operation_recovery_required", "Binary-log configuration has an unfinished restart; explicitly recover it before changing this instance", 409)
	}
	return nil
}

func (e *PrivilegedExecutor) loadDatabaseBinlogConfigIntent(instance Instance) (*databaseBinlogConfigIntent, error) {
	var intent databaseBinlogConfigIntent
	err := e.load("private", databaseBinlogConfigIntentID(instance), &intent)
	if core.ErrorCode(err) == "not_found" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !validDatabaseBinlogConfigIntent(instance, intent) {
		return nil, failure("ownership_invalid", "Binary-log configuration intent does not match its instance", 412)
	}
	return &intent, nil
}

func (e *PrivilegedExecutor) configureManagedDatabaseBinlog(ctx context.Context, instance Instance, in DatabaseBinlogInput) (any, error) {
	if instance.Revision != in.ExpectedInstanceRevision {
		return nil, failure("revision_conflict", "Managed database instance changed; reload binlog status before configuring it", 412)
	}
	if pending, err := e.loadDatabaseBinlogConfigIntent(instance); err != nil {
		return nil, err
	} else if pending != nil && pending.State == "pending" {
		return nil, failure("operation_recovery_required", "Binary-log configuration has an unfinished restart; explicitly recover it before another change", 409)
	}
	configRevision, file, err := e.readDatabaseBinlogConfig(instance)
	if err != nil {
		return nil, err
	}
	if configRevision != in.ExpectedConfigRevision {
		return nil, failure("revision_conflict", "Managed MySQL configuration changed; reload status before editing", 412)
	}
	configured, managed, configState := parseDatabaseBinlogConfig(file.Body)
	if !managed || configState != "settled" {
		return nil, failure("configuration_unmanaged", "Only the exact managed binary-log configuration can be changed", 412)
	}
	db, err := e.openManagedAdmin(ctx, instance)
	if err != nil {
		return nil, err
	}
	status, err := readDatabaseBinlogSQL(ctx, db, instance)
	db.Close()
	if err != nil {
		return nil, err
	}
	if status.Enabled != configured {
		return nil, failure("configuration_unmanaged", "Live binary-log state differs from the managed my.cnf block; resolve the external configuration before editing", 412)
	}
	if status.Enabled == in.Enabled {
		status.ConfigRevision = configRevision
		status.InstanceRevision = instance.Revision
		status.ConfiguredEnabled = &configured
		status.ConfigurationManaged = true
		status.ConfigurationState = "settled"
		return status, nil
	}
	if health := e.probe(ctx, instance); health.Status != "healthy" {
		return nil, failure("runtime_unavailable", "Binary-log configuration changes require the managed database to be healthy and restartable", 409)
	}
	after, err := setDatabaseBinlogConfig(file.Body, in.Enabled)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	intent := databaseBinlogConfigIntent{Version: 1, InstanceID: instance.ID, Engine: instance.PluginID, ConfigDir: instance.Paths.ConfigDir, InstanceRevision: instance.Revision, ExpectedRevision: configRevision, FromEnabled: status.Enabled, ToEnabled: in.Enabled, Before: append([]byte(nil), file.Body...), After: append([]byte(nil), after...), State: "pending", CreatedAt: now, UpdatedAt: now}
	if err = e.save("private", databaseBinlogConfigIntentID(instance), intent); err != nil {
		return nil, failure("operation_recovery_required", "Binary-log configuration intent could not be saved; inspect protected Agent state", 409)
	}
	// This narrow marker is set only after the shared lock, frozen config CAS,
	// owned-admin check and durable pending intent have all been verified.
	return e.applyDatabaseBinlogConfigIntent(databaseBinlogWithIntentContext(ctx, e, instance.ID), instance, intent, true)
}

func (e *PrivilegedExecutor) recoverDatabaseBinlogConfig(ctx context.Context, instance Instance, in DatabaseBinlogInput) (any, error) {
	if instance.Revision != in.ExpectedInstanceRevision {
		return nil, failure("revision_conflict", "Managed database instance changed; reload before recovering its binlog configuration", 412)
	}
	intent, err := e.loadDatabaseBinlogConfigIntent(instance)
	if err != nil {
		return nil, err
	}
	if intent == nil || intent.State != "pending" {
		return nil, failure("operation_not_pending", "No binary-log configuration restart requires recovery", 409)
	}
	configRevision, file, err := e.readDatabaseBinlogConfig(instance)
	if err != nil {
		return nil, err
	}
	if configRevision != in.ExpectedConfigRevision {
		return nil, failure("revision_conflict", "Managed MySQL configuration changed; reload before recovering its binlog configuration", 412)
	}
	if !sameDatabaseBinlogConfig(file.Body, intent.Before) && !sameDatabaseBinlogConfig(file.Body, intent.After) {
		return nil, failure("operation_recovery_required", "my.cnf differs from both recorded configuration versions; inspect it before recovery", 409)
	}
	if in.RetainFrom == "restore_previous" {
		intent.ToEnabled = intent.FromEnabled
		intent.After = append([]byte(nil), intent.Before...)
		intent.State = "pending"
		intent.UpdatedAt = time.Now().UTC()
		if err = e.save("private", databaseBinlogConfigIntentID(instance), *intent); err != nil {
			return nil, failure("operation_recovery_required", "The explicit restore choice could not be recorded; no restart was attempted", 409)
		}
	}
	// The bypass exists only after the lock, revision, intent, and exact config
	// bytes have all been validated. It cannot be supplied by an API caller.
	recoveryCtx := databaseBinlogWithIntentContext(ctx, e, instance.ID)
	if err = e.ensureInstallFinalized(recoveryCtx, instance.ID); err != nil {
		return nil, err
	}
	return e.applyDatabaseBinlogConfigIntent(recoveryCtx, instance, *intent, true)
}

func sameDatabaseBinlogConfig(a, b []byte) bool { return string(a) == string(b) }

func (e *PrivilegedExecutor) applyDatabaseBinlogConfigIntent(ctx context.Context, instance Instance, intent databaseBinlogConfigIntent, restart bool) (any, error) {
	if !validDatabaseBinlogConfigIntent(instance, intent) || intent.State != "pending" {
		return nil, failure("ownership_invalid", "Binary-log configuration recovery intent is invalid", 412)
	}
	_, current, err := e.readDatabaseBinlogConfig(instance)
	if err != nil {
		return nil, err
	}
	if !sameDatabaseBinlogConfig(current.Body, intent.Before) && !sameDatabaseBinlogConfig(current.Body, intent.After) {
		return nil, failure("operation_recovery_required", "my.cnf differs from both recorded configuration versions; no change was made", 409)
	}
	if !sameDatabaseBinlogConfig(current.Body, intent.After) {
		if err = writeAtomicRoot(instance.Paths.ConfigDir, "my.cnf", intent.After, current.Mode, current.GID); err != nil {
			return nil, failure("operation_recovery_required", "Recorded binary-log configuration remains pending; inspect and explicitly recover it", 409)
		}
	}
	if restart {
		if _, err = e.controlLocked(ctx, ServiceInput{ID: instance.ID, Command: "restart"}, instance); err != nil {
			return nil, failure("operation_recovery_required", "Binary-log configuration was staged but restart was not verified; explicitly recover it", 409)
		}
	}
	db, err := e.openManagedAdmin(ctx, instance)
	if err != nil {
		return nil, failure("operation_recovery_required", "Binary-log configuration restart needs explicit recovery; the database did not authenticate", 409)
	}
	status, err := readDatabaseBinlogSQL(ctx, db, instance)
	db.Close()
	if err != nil || status.Enabled != intent.ToEnabled {
		return nil, failure("operation_recovery_required", "Database binary-log state does not match the staged configuration; explicitly recover it", 409)
	}
	intent.State = "completed"
	intent.UpdatedAt = time.Now().UTC()
	if err = e.save("private", databaseBinlogConfigIntentID(instance), intent); err != nil {
		return nil, failure("operation_recovery_required", "Binary-log restart succeeded but its protected completion receipt was not saved", 409)
	}
	configRevision, _, err := e.readDatabaseBinlogConfig(instance)
	if err != nil {
		return nil, err
	}
	configured := intent.ToEnabled
	status.ConfigRevision = configRevision
	status.InstanceRevision = instance.Revision
	status.ConfiguredEnabled = &configured
	status.ConfigurationManaged = true
	status.ConfigurationState = "settled"
	return status, nil
}

func (e *PrivilegedExecutor) createDatabaseBinlogPurgePreview(instance Instance, status DatabaseBinlogStatus, logs []DatabaseBinlogLog, retainFrom string) (DatabaseBinlogPurgePreview, error) {
	if status.ActiveLog == "" {
		return DatabaseBinlogPurgePreview{}, failure("active_log_unavailable", "A current active binary log could not be proven; no purge preview was created", 412)
	}
	retainIndex, activeIndex := -1, -1
	for i, log := range logs {
		if log.Name == retainFrom {
			retainIndex = i
		}
		if log.Name == status.ActiveLog {
			activeIndex = i
		}
	}
	if retainIndex < 0 || activeIndex < 0 || retainIndex > activeIndex {
		return DatabaseBinlogPurgePreview{}, failure("unsafe_purge_target", "Retain-from log must exist at or before the current active log so the active log is preserved", 409)
	}
	candidates := append([]DatabaseBinlogLog(nil), logs[:retainIndex]...)
	if len(candidates) == 0 {
		return DatabaseBinlogPurgePreview{}, failure("nothing_to_purge", "No historical binary logs precede the selected retained-from log", 409)
	}
	var bytesToPurge uint64
	for _, log := range candidates {
		if log.Name == status.ActiveLog || ^uint64(0)-bytesToPurge < log.SizeBytes {
			return DatabaseBinlogPurgePreview{}, failure("unsafe_purge_target", "Purge candidates include the active log or exceed the supported size total", 409)
		}
		bytesToPurge += log.SizeBytes
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return DatabaseBinlogPurgePreview{}, err
	}
	created := time.Now().UTC()
	snapshot := databaseBinlogSnapshot{Version: 1, InstanceID: instance.ID, Engine: instance.PluginID, PreviewID: hex.EncodeToString(random[:]), RetainFrom: retainFrom, ActiveLog: status.ActiveLog, Logs: append([]DatabaseBinlogLog(nil), logs...), Candidates: candidates, ConfigRevision: status.ConfigRevision, InstanceRevision: instance.Revision, CreatedAt: created, ExpiresAt: created.Add(databaseBinlogPreviewLifetime)}
	digest, err := databaseBinlogSnapshotDigest(snapshot)
	if err != nil {
		return DatabaseBinlogPurgePreview{}, err
	}
	snapshot.SnapshotSHA256 = digest
	if err = e.save("private", databaseBinlogSnapshotID(instance), snapshot); err != nil {
		return DatabaseBinlogPurgePreview{}, failure("preview_storage_failed", "Protected binary-log purge preview could not be saved", 502)
	}
	return publicDatabaseBinlogPreview(snapshot, bytesToPurge), nil
}

func databaseBinlogSnapshotDigest(snapshot databaseBinlogSnapshot) (string, error) {
	snapshot.SnapshotSHA256 = ""
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func publicDatabaseBinlogPreview(snapshot databaseBinlogSnapshot, size uint64) DatabaseBinlogPurgePreview {
	return DatabaseBinlogPurgePreview{PreviewID: snapshot.PreviewID, ExpiresAt: snapshot.ExpiresAt, RetainFrom: snapshot.RetainFrom, ActiveLog: snapshot.ActiveLog, BinlogsToPurge: append([]DatabaseBinlogLog(nil), snapshot.Candidates...), BytesToPurge: size, SnapshotSHA256: snapshot.SnapshotSHA256, ExpectedConfigRevision: snapshot.ConfigRevision, ExpectedInstanceRevision: snapshot.InstanceRevision, ReplicaSafety: "unproven_offline_or_unregistered_replicas", ConfirmationRequired: true}
}

func (e *PrivilegedExecutor) purgeManagedDatabaseBinlogs(ctx context.Context, instance Instance, in DatabaseBinlogInput) (DatabaseBinlogPurgeResult, error) {
	if instance.Revision != in.ExpectedInstanceRevision {
		return DatabaseBinlogPurgeResult{}, failure("revision_conflict", "Managed database instance changed; create a fresh purge preview", 412)
	}
	configRevision, _, err := e.readDatabaseBinlogConfig(instance)
	if err != nil {
		return DatabaseBinlogPurgeResult{}, err
	}
	if configRevision != in.ExpectedConfigRevision {
		return DatabaseBinlogPurgeResult{}, failure("revision_conflict", "Managed MySQL configuration changed; create a fresh purge preview", 412)
	}
	var snapshot databaseBinlogSnapshot
	if err = e.load("private", databaseBinlogSnapshotID(instance), &snapshot); err != nil {
		return DatabaseBinlogPurgeResult{}, failure("preview_not_found", "Protected binary-log purge preview is unavailable; create a fresh preview", 404)
	}
	if !validDatabaseBinlogSnapshot(snapshot) || snapshot.InstanceID != instance.ID || snapshot.Engine != instance.PluginID || snapshot.PreviewID != in.PreviewID || snapshot.ConfigRevision != in.ExpectedConfigRevision || snapshot.InstanceRevision != in.ExpectedInstanceRevision || !snapshot.ExpiresAt.After(time.Now().UTC()) {
		return DatabaseBinlogPurgeResult{}, failure("revision_conflict", "Binary-log purge preview expired or belongs to a different instance revision", 412)
	}
	digest, err := databaseBinlogSnapshotDigest(snapshot)
	if err != nil || digest != snapshot.SnapshotSHA256 || digest != in.SnapshotSHA256 || !sameDatabaseBinlogNames(in.BinlogsToPurge, snapshot.Candidates) {
		return DatabaseBinlogPurgeResult{}, failure("revision_conflict", "Purge confirmation does not match the exact protected binlog snapshot", 412)
	}
	db, err := e.openManagedAdmin(ctx, instance)
	if err != nil {
		return DatabaseBinlogPurgeResult{}, err
	}
	defer db.Close()
	status, err := readDatabaseBinlogSQL(ctx, db, instance)
	if err != nil {
		return DatabaseBinlogPurgeResult{}, err
	}
	if !status.Enabled || status.ActiveLog != snapshot.ActiveLog {
		return DatabaseBinlogPurgeResult{}, failure("revision_conflict", "Binary-log active file changed after preview; create a fresh preview", 412)
	}
	current, err := readDatabaseBinlogLogs(ctx, db, databaseBinlogMaxSnapshot)
	if err != nil {
		return DatabaseBinlogPurgeResult{}, err
	}
	if err = validateDatabaseBinlogSnapshotStillCurrent(snapshot, current, status.ActiveLog); err != nil {
		return DatabaseBinlogPurgeResult{}, err
	}
	if len(snapshot.Candidates) == 0 || !databaseBinlogNamePattern.MatchString(snapshot.RetainFrom) {
		return DatabaseBinlogPurgeResult{}, failure("unsafe_purge_target", "Frozen purge target is invalid; no logs were purged", 412)
	}
	// The literal is generated from a path-free name returned by SHOW BINARY
	// LOGS and is rejected unless it exactly matches the immutable preview.
	if _, err = db.ExecContext(ctx, "PURGE BINARY LOGS TO '"+snapshot.RetainFrom+"'"); err != nil {
		return DatabaseBinlogPurgeResult{}, failure("purge_failed", "MySQL or MariaDB refused the confirmed binary-log purge; inspect current logs before retrying", 502)
	}
	var bytesPurged uint64
	for _, log := range snapshot.Candidates {
		bytesPurged += log.SizeBytes
	}
	return DatabaseBinlogPurgeResult{InstanceID: instance.ID, RetainFrom: snapshot.RetainFrom, ActiveLog: snapshot.ActiveLog, Purged: append([]DatabaseBinlogLog(nil), snapshot.Candidates...), BytesPurged: bytesPurged, ReplicaSafety: "unproven_offline_or_unregistered_replicas_confirmed", SnapshotSHA256: snapshot.SnapshotSHA256}, nil
}

func validateDatabaseBinlogSnapshotStillCurrent(snapshot databaseBinlogSnapshot, current []DatabaseBinlogLog, active string) error {
	if len(current) != len(snapshot.Logs) {
		return failure("revision_conflict", "Binary-log count changed after preview; create a fresh preview", 412)
	}
	for i, item := range current {
		frozen := snapshot.Logs[i]
		if item.Name != frozen.Name || (item.Name != active && item.SizeBytes != frozen.SizeBytes) || (item.Name == active && item.SizeBytes < frozen.SizeBytes) {
			return failure("revision_conflict", "A binary-log name, order or historical size changed after preview; create a fresh preview", 412)
		}
	}
	for _, candidate := range snapshot.Candidates {
		if candidate.Name == active {
			return failure("unsafe_purge_target", "Frozen purge candidates include the active binary log", 412)
		}
	}
	return nil
}

func sameDatabaseBinlogNames(names []string, logs []DatabaseBinlogLog) bool {
	if len(names) != len(logs) {
		return false
	}
	for i, log := range logs {
		if names[i] != log.Name {
			return false
		}
	}
	return true
}

func (m *Manager) databaseBinlogBindings() []core.Binding {
	instanceResources := func(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
		var input struct {
			InstanceID string `json:"instance_id"`
		}
		if json.Unmarshal(raw, &input) != nil || !validInstanceID(input.InstanceID) {
			return nil, failure("invalid_input", "A valid managed MySQL or MariaDB instance is required", 400)
		}
		return []string{DatabaseResource(input.InstanceID, "")}, nil
	}
	instanceRevisionSchema := map[string]any{"type": "integer", "minimum": 1}
	result := []core.Binding{}
	for _, spec := range []struct {
		name, command, description string
		scope                      string
		readOnly, task             bool
		fields                     map[string]any
		required                   []string
	}{
		{"managed_database_binlog_status", "status", "Read actual MySQL or MariaDB binary-log runtime and managed configuration state", "databases:read", true, false, map[string]any{"instance_id": idSchema()}, []string{"instance_id"}},
		{"managed_database_binlogs_list", "list", "List a bounded page of binary-log names and server-reported sizes from the managed MySQL or MariaDB instance", "databases:read", true, false, map[string]any{"instance_id": idSchema(), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": databaseBinlogMaxList}, "after": map[string]any{"type": "string", "pattern": databaseBinlogNamePattern.String()}}, []string{"instance_id"}},
		{"managed_database_binlog_configure", "configure", "Enable or disable managed MySQL/MariaDB binary logging through its owned configuration and verified restart", "databases:admin", false, true, map[string]any{"instance_id": idSchema(), "enabled": map[string]any{"type": "boolean"}, "expected_config_revision": map[string]any{"type": "string", "pattern": databaseBinlogRevisionPattern.String()}, "expected_instance_revision": instanceRevisionSchema}, []string{"instance_id", "enabled", "expected_config_revision", "expected_instance_revision"}},
		{"managed_database_binlog_purge_preview", "preview_purge", "Freeze the exact historical binary-log prefix that a later confirmed purge would remove", "databases:admin", false, false, map[string]any{"instance_id": idSchema(), "retain_from": map[string]any{"type": "string", "pattern": databaseBinlogNamePattern.String()}}, []string{"instance_id", "retain_from"}},
		{"managed_database_binlog_purge", "purge", "Purge the exact frozen MySQL/MariaDB binary-log prefix after explicit offline-replica history-loss confirmation", "databases:admin", false, true, map[string]any{"instance_id": idSchema(), "preview_id": map[string]any{"type": "string", "pattern": databaseBinlogPreviewPattern.String()}, "snapshot_sha256": map[string]any{"type": "string", "pattern": databaseBinlogRevisionPattern.String()}, "expected_config_revision": map[string]any{"type": "string", "pattern": databaseBinlogRevisionPattern.String()}, "expected_instance_revision": instanceRevisionSchema, "binlogs_to_purge": map[string]any{"type": "array", "minItems": 1, "maxItems": databaseBinlogMaxSnapshot, "items": map[string]any{"type": "string", "pattern": databaseBinlogNamePattern.String()}}, "confirm_replica_history_loss": map[string]any{"type": "boolean", "const": true}}, []string{"instance_id", "preview_id", "snapshot_sha256", "expected_config_revision", "expected_instance_revision", "binlogs_to_purge", "confirm_replica_history_loss"}},
		{"managed_database_binlog_config_recover", "recover_config", "Explicitly apply or restore a staged MySQL/MariaDB binlog configuration after an interrupted restart", "databases:admin", false, true, map[string]any{"instance_id": idSchema(), "command": enumSchema("apply_pending", "restore_previous"), "expected_config_revision": map[string]any{"type": "string", "pattern": databaseBinlogRevisionPattern.String()}, "expected_instance_revision": instanceRevisionSchema}, []string{"instance_id", "command", "expected_config_revision", "expected_instance_revision"}},
	} {
		spec := spec
		binding := m.binding(spec.name, spec.description, spec.scope, spec.readOnly, obj(spec.fields, spec.required...), instanceResources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			var input DatabaseBinlogInput
			if err := parse(raw, &input); err != nil {
				return core.ActionResult{}, err
			}
			choice := input.Command
			input.Command = spec.command
			if spec.command == "recover_config" {
				input.RetainFrom = choice
			}
			if spec.command == "list" && input.Limit == 0 {
				input.Limit = databaseBinlogMaxList
			}
			forward, err := json.Marshal(input)
			if err != nil {
				return core.ActionResult{}, err
			}
			resources, err := instanceResources(ctx, actor, raw)
			if err != nil {
				return core.ActionResult{}, err
			}
			if spec.task {
				job, submitErr := m.deps.Tasks.Submit(ctx, actor, databaseBinlogActionOperation, forward, resources)
				if submitErr != nil {
					return core.ActionResult{}, submitErr
				}
				taskID, _ := job["id"].(string)
				if taskID == "" {
					taskID, _ = job["task_id"].(string)
				}
				summary := map[string]any{"instance_id": input.InstanceID, "command": spec.command}
				if spec.command == "purge" {
					summary["preview_id"] = input.PreviewID
					summary["snapshot_sha256"] = input.SnapshotSHA256
					summary["confirmed_binlog_count"] = len(input.BinlogsToPurge)
				}
				return core.ActionResult{Data: job, TaskID: taskID, Resources: resources, AuditSummary: summary}, nil
			}
			var output any
			if err = m.agentJSON(ctx, databaseBinlogActionOperation, forward, &output); err != nil {
				return core.ActionResult{}, err
			}
			summary := map[string]any{"instance_id": input.InstanceID, "command": spec.command}
			if spec.command == "preview_purge" {
				if preview, ok := output.(map[string]any); ok {
					summary["preview_id"] = preview["preview_id"]
					summary["binlog_count"] = lenAny(preview["binlogs_to_purge"])
				}
			}
			return core.ActionResult{Data: output, Resources: resources, AuditSummary: summary}, nil
		})
		binding.Action.Task = spec.task
		result = append(result, binding)
	}
	return result
}

func lenAny(value any) int {
	switch typed := value.(type) {
	case []any:
		return len(typed)
	case []DatabaseBinlogLog:
		return len(typed)
	default:
		return 0
	}
}
