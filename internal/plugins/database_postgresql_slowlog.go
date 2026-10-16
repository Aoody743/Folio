package plugins

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Aoody743/PowerLinux/internal/core"
)

const (
	databasePostgreSQLSlowlogBegin           = "# PowerLinux managed PostgreSQL slow log v1 begin"
	databasePostgreSQLSlowlogEnd             = "# PowerLinux managed PostgreSQL slow log v1 end"
	databasePostgreSQLSlowlogMaxBytes        = 4 << 20
	databasePostgreSQLSlowlogMaxLine         = 16 << 10
	databasePostgreSQLSlowlogMaxStmt         = 8 << 10
	databasePostgreSQLSlowlogRedactionNotice = "String and numeric literals and SQL comments are masked; SQL identifiers may still contain sensitive text."
)

var (
	postgresqlSlowlogDirectivePattern = regexp.MustCompile(`(?im)^\s*(log_min_duration_statement|log_parameter_max_length|log_statement|log_line_prefix)\s*=`)
	postgresqlSlowlogRecordPattern    = regexp.MustCompile(`^(.*?) LOG:\s+duration:\s+([0-9]+(?:\.[0-9]+)?) ms\s+statement:\s*(.*)$`)
	postgresqlSlowlogPrefixPattern    = regexp.MustCompile(`^(.+?) \[[0-9]+\] ([^@ ]+)@([^ ]+)$`)
	postgresqlSlowlogSecretPattern    = regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api[_-]?key)\s*=\s*[^,\s;]+`)
)

type DatabasePostgreSQLSlowlogEntry struct {
	StartedAt  string  `json:"started_at"`
	DurationMS float64 `json:"duration_ms"`
	Database   string  `json:"database"`
	Role       string  `json:"role"`
	Statement  string  `json:"statement"`
}

type DatabasePostgreSQLSlowlogPage struct {
	InstanceID         string                           `json:"instance_id"`
	Database           string                           `json:"database"`
	InstanceRevision   int64                            `json:"instance_revision"`
	DatabaseRevision   int64                            `json:"database_revision"`
	ConfigRevision     string                           `json:"config_revision"`
	Enabled            bool                             `json:"enabled"`
	MinimumDurationMS  int                              `json:"minimum_duration_ms"`
	ConfigurationState string                           `json:"configuration_state"`
	ReloadApplied      bool                             `json:"reload_applied"`
	RedactionNotice    string                           `json:"redaction_notice"`
	Items              []DatabasePostgreSQLSlowlogEntry `json:"items"`
	Truncated          bool                             `json:"truncated"`
	LogAvailable       bool                             `json:"log_available"`
}

type postgresqlSlowlogConfig struct {
	Enabled           bool
	MinimumDurationMS int
}

type postgresqlSlowlogSetting struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

var postgresqlSlowlogSettingNames = [...]string{"log_min_duration_statement", "log_parameter_max_length", "log_statement", "log_line_prefix"}

func readPostgreSQLConfig(instance Instance, serviceGID int) (postgresAccessFile, error) {
	file, err := readProfileFile(instance.Paths.ConfigDir, "postgresql.conf")
	if err != nil || file.Mode != 0640 || file.GID != serviceGID || len(file.Body) > databasePostgreSQLMaxConfigBytes {
		return postgresAccessFile{}, failure("configuration_unmanaged", "Owned PostgreSQL runtime configuration is unavailable or unsafe", 412)
	}
	return postgresAccessFile{Body: file.Body, Mode: file.Mode, UID: 0, GID: file.GID}, nil
}

func parsePostgreSQLSlowlogBlock(body []byte) (postgresqlSlowlogConfig, bool, error) {
	text := string(body)
	startCount, endCount := strings.Count(text, databasePostgreSQLSlowlogBegin), strings.Count(text, databasePostgreSQLSlowlogEnd)
	if startCount == 0 && endCount == 0 {
		return postgresqlSlowlogConfig{}, false, nil
	}
	if startCount != 1 || endCount != 1 {
		return postgresqlSlowlogConfig{}, false, failure("configuration_unmanaged", "PostgreSQL slow-log block markers are ambiguous", 412)
	}
	start := strings.Index(text, databasePostgreSQLSlowlogBegin)
	end := strings.Index(text, databasePostgreSQLSlowlogEnd)
	if end <= start || text[end+len(databasePostgreSQLSlowlogEnd):] != "\n" && text[end+len(databasePostgreSQLSlowlogEnd):] != "" {
		return postgresqlSlowlogConfig{}, false, failure("configuration_unmanaged", "PostgreSQL slow-log block is malformed", 412)
	}
	block := strings.TrimSuffix(text[start:end], "\n")
	lines := strings.Split(block, "\n")
	if len(lines) != 6 || lines[0] != databasePostgreSQLSlowlogBegin {
		return postgresqlSlowlogConfig{}, false, failure("configuration_unmanaged", "PostgreSQL slow-log block contains unrecognized directives", 412)
	}
	thresholdLine := strings.TrimPrefix(lines[1], "# threshold-ms=")
	threshold, err := strconv.Atoi(thresholdLine)
	if err != nil || threshold < 1 || threshold > 600000 || lines[3] != "log_parameter_max_length = 0" || lines[4] != "log_statement = 'none'" || lines[5] != "log_line_prefix = '%m [%p] %u@%d '" {
		return postgresqlSlowlogConfig{}, false, failure("configuration_unmanaged", "PostgreSQL slow-log block values differ from the typed policy", 412)
	}
	minLine := strings.TrimPrefix(lines[2], "log_min_duration_statement = ")
	minimum, err := strconv.Atoi(minLine)
	if err != nil || (minimum != -1 && (minimum < 1 || minimum > 600000)) {
		return postgresqlSlowlogConfig{}, false, failure("configuration_unmanaged", "PostgreSQL slow-query threshold is invalid", 412)
	}
	return postgresqlSlowlogConfig{Enabled: minimum >= 0, MinimumDurationMS: threshold}, true, nil
}

func postgresqlSlowlogEnabled(body []byte) bool {
	config, managed, err := parsePostgreSQLSlowlogBlock(body)
	return err == nil && managed && config.Enabled
}

func postgresqlSlowlogDuration(body []byte) int {
	config, managed, err := parsePostgreSQLSlowlogBlock(body)
	if err != nil || !managed {
		return 0
	}
	return config.MinimumDurationMS
}

func makePostgreSQLSlowlogConfig(body []byte, enabled bool, threshold int) ([]byte, error) {
	if threshold < 1 || threshold > 600000 {
		return nil, failure("invalid_input", "Slow-query threshold must be 1–600000 milliseconds", 400)
	}
	current, managed, err := parsePostgreSQLSlowlogBlock(body)
	if err != nil {
		return nil, err
	}
	text := string(body)
	if !managed && postgresqlSlowlogDirectivePattern.Match(body) {
		return nil, failure("configuration_unmanaged", "PostgreSQL slow-log directives outside the exact managed block must be reviewed before changing settings", 412)
	}
	if managed && current.Enabled == enabled && current.MinimumDurationMS == threshold {
		return append([]byte(nil), body...), nil
	}
	minimum := threshold
	if !enabled {
		minimum = -1
	}
	block := databasePostgreSQLSlowlogBegin + "\n" +
		"# threshold-ms=" + strconv.Itoa(threshold) + "\n" +
		"log_min_duration_statement = " + strconv.Itoa(minimum) + "\n" +
		"log_parameter_max_length = 0\n" +
		"log_statement = 'none'\n" +
		"log_line_prefix = '%m [%p] %u@%d '\n" +
		databasePostgreSQLSlowlogEnd + "\n"
	if managed {
		start := strings.Index(text, databasePostgreSQLSlowlogBegin)
		end := strings.Index(text, databasePostgreSQLSlowlogEnd) + len(databasePostgreSQLSlowlogEnd)
		if end < len(text) && text[end] == '\n' {
			end++
		}
		text = text[:start] + block + text[end:]
	} else {
		if len(text) > 0 && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += "\n" + block
	}
	if len(text) > databasePostgreSQLMaxConfigBytes {
		return nil, failure("result_limit", "PostgreSQL runtime configuration exceeds its 128 KiB bound", 413)
	}
	return []byte(text), nil
}

// Preserve the one typed slow-log block when software config regeneration
// writes the generated base postgresql.conf. Custom directives remain a CAS
// conflict and are never copied or interpreted as an arbitrary profile.
func preservePostgreSQLMaintenanceRuntimeConfig(generated, previous []byte) ([]byte, error) {
	config, managed, err := parsePostgreSQLSlowlogBlock(previous)
	if err != nil {
		return nil, err
	}
	if !managed {
		if postgresqlSlowlogDirectivePattern.Match(previous) {
			return nil, failure("configuration_unmanaged", "Existing PostgreSQL slow-log directives are not in the typed owned block", 412)
		}
		return append([]byte(nil), generated...), nil
	}
	return makePostgreSQLSlowlogConfig(generated, config.Enabled, config.MinimumDurationMS)
}

func (e *PrivilegedExecutor) postgreSQLSlowlogStatus(ctx context.Context, instance Instance, record managedDatabaseRecord) (DatabasePostgreSQLSlowlogPage, error) {
	file, err := readPostgreSQLConfig(instance, int(e.gid))
	if err != nil {
		return DatabasePostgreSQLSlowlogPage{}, err
	}
	pending, intentErr := e.loadPostgreSQLMaintenanceIntent(instance, "slowlog")
	if intentErr != nil {
		return DatabasePostgreSQLSlowlogPage{}, intentErr
	}
	pendingRecovery := pending != nil && pending.State == "pending"
	config, managed, parseErr := parsePostgreSQLSlowlogBlock(file.Body)
	if parseErr != nil && !pendingRecovery {
		return DatabasePostgreSQLSlowlogPage{}, parseErr
	}
	state := "unconfigured"
	if managed && parseErr == nil {
		state = "settled"
	}
	if parseErr == nil && !managed && postgresqlSlowlogDirectivePattern.Match(file.Body) {
		state = "unmanaged"
	}
	if parseErr == nil {
		settings, settingsErr := e.readPostgreSQLSlowlogSettings(ctx, instance)
		if settingsErr != nil {
			if state != "unmanaged" {
				return DatabasePostgreSQLSlowlogPage{}, settingsErr
			}
		} else if managed {
			if err = verifyPostgreSQLSlowlogSettingMap(settings, config); err != nil {
				state = "pending_reload"
			}
		}
	}
	if pendingRecovery {
		state = "pending_recovery"
	}
	page := DatabasePostgreSQLSlowlogPage{InstanceID: instance.ID, Database: record.Database, InstanceRevision: instance.Revision, DatabaseRevision: record.Revision, ConfigRevision: databasePostgreSQLConfigDigest(file.Body), Enabled: config.Enabled, MinimumDurationMS: config.MinimumDurationMS, ConfigurationState: state, ReloadApplied: state == "settled", RedactionNotice: databasePostgreSQLSlowlogRedactionNotice, Items: []DatabasePostgreSQLSlowlogEntry{}}
	return page, nil
}

func (e *PrivilegedExecutor) readPostgreSQLSlowlogSettings(ctx context.Context, instance Instance) (map[string]postgresqlSlowlogSetting, error) {
	db, err := e.openManagedAdmin(ctx, instance)
	if err != nil {
		return nil, failure("runtime_unavailable", "Owned PostgreSQL administrator connection is unavailable", 412)
	}
	defer db.Close()
	return queryPostgreSQLSlowlogSettings(ctx, db)
}

func queryPostgreSQLSlowlogSettings(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (map[string]postgresqlSlowlogSetting, error) {
	rows, err := db.QueryContext(ctx, "SELECT name,setting,source FROM pg_settings WHERE name IN ('log_min_duration_statement','log_parameter_max_length','log_statement','log_line_prefix')")
	if err != nil {
		return nil, failure("slowlog_unsupported", "PostgreSQL slow-query logging settings are not available on this server version", 412)
	}
	defer rows.Close()
	settings := map[string]postgresqlSlowlogSetting{}
	for rows.Next() {
		var item postgresqlSlowlogSetting
		if err = rows.Scan(&item.Name, &item.Value, &item.Source); err != nil {
			return nil, failure("runtime_unavailable", "PostgreSQL slow-query settings could not be decoded", 502)
		}
		settings[item.Name] = item
	}
	if err = rows.Err(); err != nil {
		return nil, failure("runtime_unavailable", "PostgreSQL slow-query settings could not be read", 502)
	}
	if len(settings) != 4 {
		return nil, failure("slowlog_unsupported", "This PostgreSQL version lacks the safe parameter-redaction settings required for slow logs", 412)
	}
	return settings, nil
}

func verifyPostgreSQLSlowlogSettings(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, body []byte) error {
	want, managed, err := parsePostgreSQLSlowlogBlock(body)
	if err != nil || !managed {
		return failure("configuration_unmanaged", "PostgreSQL slow-log target does not contain the exact managed block", 412)
	}
	settings, err := queryPostgreSQLSlowlogSettings(ctx, db)
	if err != nil {
		return err
	}
	if err = verifyPostgreSQLSlowlogSettingMap(settings, want); err != nil {
		return err
	}
	for _, item := range settings {
		if item.Source != "configuration file" {
			return failure("reload_not_applied", "PostgreSQL did not load the typed slow-log settings from its managed configuration", 409)
		}
	}
	return nil
}

func validPostgreSQLSlowlogBaseline(settings map[string]postgresqlSlowlogSetting) bool {
	if len(settings) != len(postgresqlSlowlogSettingNames) {
		return false
	}
	for _, name := range postgresqlSlowlogSettingNames {
		item, ok := settings[name]
		if !ok || item.Name != name || len(item.Value) > 1024 || !utf8.ValidString(item.Value) || strings.ContainsAny(item.Value, "\x00\r\n") {
			return false
		}
		if item.Source != "" && item.Source != "default" && item.Source != "configuration file" {
			return false
		}
	}
	return true
}

func verifyPostgreSQLSlowlogBaselineSettings(live, expected map[string]postgresqlSlowlogSetting) error {
	if !validPostgreSQLSlowlogBaseline(expected) || len(live) != len(postgresqlSlowlogSettingNames) {
		return failure("runtime_unavailable", "PostgreSQL prior slow-log settings could not be verified", 502)
	}
	for _, name := range postgresqlSlowlogSettingNames {
		want := expected[name]
		got, ok := live[name]
		if !ok || got.Name != name || got.Value != want.Value || len(got.Value) > 1024 || !utf8.ValidString(got.Value) || strings.ContainsAny(got.Value, "\x00\r\n") {
			return failure("reload_not_applied", "PostgreSQL did not restore the recorded prior slow-log values", 409)
		}
		if want.Source != "" {
			if got.Source != want.Source {
				return failure("reload_not_applied", "PostgreSQL did not restore the recorded prior slow-log source", 409)
			}
		} else if got.Source != "default" && got.Source != "configuration file" {
			return failure("reload_not_applied", "PostgreSQL prior slow-log setting source is not provable", 409)
		}
	}
	return nil
}

func postgresqlConfigSettingOutput(name, output string) (string, error) {
	allowed := false
	for _, fixed := range postgresqlSlowlogSettingNames {
		if name == fixed {
			allowed = true
			break
		}
	}
	if !allowed || !strings.HasSuffix(output, "\n") {
		return "", failure("configuration_check_failed", "PostgreSQL prior settings could not be read safely", 409)
	}
	value := strings.TrimSuffix(output, "\n")
	if len(value) > 1024 || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
		return "", failure("configuration_check_failed", "PostgreSQL prior settings exceeded their safe output bound", 409)
	}
	return value, nil
}

// derivePostgreSQLSlowlogBaseline handles old pending journals that predate a
// recorded live-settings snapshot. The result is the effective configuration
// derived from the exact journaled Before file, not a historical live snapshot.
// The candidate is a root-owned sibling so relative include paths resolve from
// the same directory. Its suffix is intentionally not .conf, so include_dir
// scans cannot load the candidate a second time.
func (e *PrivilegedExecutor) derivePostgreSQLSlowlogBaseline(ctx context.Context, instance Instance, before []byte) (result map[string]postgresqlSlowlogSetting, retErr error) {
	baseline := make(map[string]postgresqlSlowlogSetting, len(postgresqlSlowlogSettingNames))
	if len(before) == 0 || len(before) > databasePostgreSQLMaxConfigBytes || !filepath.IsAbs(instance.Paths.ConfigDir) || !filepath.IsAbs(instance.Paths.DataDir) {
		return nil, failure("configuration_check_failed", "PostgreSQL prior configuration is outside the recovery bounds", 409)
	}
	if err := trustedDatabaseProgram(instance.Service.Program); err != nil {
		return nil, failure("configuration_check_failed", "The trusted PostgreSQL program is unavailable for prior-state verification", 409)
	}
	var entropy [12]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return nil, err
	}
	tempName := ".powerlinux-slowlog-baseline-" + hex.EncodeToString(entropy[:])
	root, err := os.OpenRoot(instance.Paths.ConfigDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	defer func() {
		removeErr := root.Remove(tempName)
		if removeErr != nil && !os.IsNotExist(removeErr) {
			result = nil
			retErr = failure("configuration_check_failed", "The temporary PostgreSQL prior-configuration copy could not be removed safely", 409)
			return
		}
		if removeErr == nil {
			dir, openErr := root.Open(".")
			if openErr != nil {
				result = nil
				retErr = failure("configuration_check_failed", "PostgreSQL prior-configuration cleanup could not be verified", 409)
				return
			}
			syncErr := dir.Sync()
			closeErr := dir.Close()
			if syncErr != nil || closeErr != nil {
				result = nil
				retErr = failure("configuration_check_failed", "PostgreSQL prior-configuration cleanup could not be verified", 409)
			}
		}
	}()
	if err = writeAtomicRoot(instance.Paths.ConfigDir, tempName, before, 0640, int(e.gid)); err != nil {
		return nil, failure("configuration_check_failed", "The journaled PostgreSQL prior configuration could not be staged safely", 409)
	}
	staged, err := root.Open(tempName)
	if err != nil {
		return nil, failure("configuration_check_failed", "The journaled PostgreSQL prior configuration could not be verified safely", 409)
	}
	info, statErr := staged.Stat()
	var stat *syscall.Stat_t
	if statErr == nil {
		stat, _ = info.Sys().(*syscall.Stat_t)
	}
	validFile := statErr == nil && stat != nil && info.Mode().IsRegular() && info.Size() == int64(len(before)) && stat.Nlink == 1 && (os.Geteuid() != 0 || stat.Uid == 0) && stat.Gid == uint32(e.gid) && info.Mode().Perm() == 0640
	if validFile {
		linkInfo, linkErr := root.Lstat(tempName)
		linkStat, _ := linkInfo.Sys().(*syscall.Stat_t)
		validFile = linkErr == nil && linkStat != nil && linkStat.Dev == stat.Dev && linkStat.Ino == stat.Ino && linkInfo.Mode().IsRegular()
	}
	if validFile {
		contents, readErr := io.ReadAll(io.LimitReader(staged, int64(len(before))+1))
		validFile = readErr == nil && len(contents) == len(before) && bytes.Equal(contents, before)
	}
	closeErr := staged.Close()
	if !validFile || closeErr != nil {
		return nil, failure("configuration_check_failed", "The journaled PostgreSQL prior configuration could not be verified safely", 409)
	}
	dir, err := root.Open(".")
	if err != nil {
		return nil, failure("configuration_check_failed", "The temporary PostgreSQL prior-configuration directory could not be synced", 409)
	}
	err = dir.Sync()
	closeErr = dir.Close()
	if err != nil || closeErr != nil {
		return nil, failure("configuration_check_failed", "The temporary PostgreSQL prior-configuration directory could not be synced", 409)
	}
	configPath := filepath.Join(instance.Paths.ConfigDir, tempName)
	for _, name := range postgresqlSlowlogSettingNames {
		args, argsErr := postgresqlConfigValueArgs(instance.Paths.DataDir, configPath, name)
		if argsErr != nil {
			return nil, argsErr
		}
		output, runErr := runFixed(ctx, instance.Service.Program, args, 10*time.Second, e.credential(instance))
		if runErr != nil {
			return nil, failure("configuration_check_failed", "PostgreSQL could not evaluate the journaled prior configuration", 409)
		}
		value, parseErr := postgresqlConfigSettingOutput(name, output)
		if parseErr != nil {
			return nil, parseErr
		}
		baseline[name] = postgresqlSlowlogSetting{Name: name, Value: value}
	}
	if !validPostgreSQLSlowlogBaseline(baseline) {
		return nil, failure("configuration_check_failed", "PostgreSQL prior settings did not match the fixed slow-log schema", 409)
	}
	return baseline, nil
}

func postgresqlConfigValueArgs(dataDir, configPath, name string) ([]string, error) {
	allowed := false
	for _, fixed := range postgresqlSlowlogSettingNames {
		if name == fixed {
			allowed = true
			break
		}
	}
	if !allowed || !filepath.IsAbs(dataDir) || !filepath.IsAbs(configPath) || strings.ContainsAny(dataDir+configPath, "\x00\r\n") {
		return nil, failure("configuration_check_failed", "PostgreSQL prior setting request is outside the fixed recovery scope", 409)
	}
	return []string{"-D", dataDir, "-c", "config_file=" + configPath, "-C", name}, nil
}

func (e *PrivilegedExecutor) waitForPostgreSQLSlowlogTarget(ctx context.Context, instance Instance, target []byte) error {
	return waitForPostgreSQLSlowlogSettings(ctx, func(callCtx context.Context) (*sql.DB, error) {
		return e.openManagedAdmin(callCtx, instance)
	}, func(callCtx context.Context, db *sql.DB) error {
		return verifyPostgreSQLSlowlogSettings(callCtx, db, target)
	})
}

func (e *PrivilegedExecutor) waitForPostgreSQLSlowlogBaseline(ctx context.Context, instance Instance, baseline map[string]postgresqlSlowlogSetting) error {
	return waitForPostgreSQLSlowlogBaselineUsing(ctx, func(callCtx context.Context) (*sql.DB, error) {
		return e.openManagedAdmin(callCtx, instance)
	}, baseline)
}

func waitForPostgreSQLSlowlogBaselineUsing(ctx context.Context, open func(context.Context) (*sql.DB, error), baseline map[string]postgresqlSlowlogSetting) error {
	if !validPostgreSQLSlowlogBaseline(baseline) {
		return failure("operation_recovery_required", "PostgreSQL recovery journal has no valid prior settings snapshot", 409)
	}
	return waitForPostgreSQLSlowlogSettings(ctx, open, func(callCtx context.Context, db *sql.DB) error {
		live, err := queryPostgreSQLSlowlogSettings(callCtx, db)
		if err != nil {
			return err
		}
		return verifyPostgreSQLSlowlogBaselineSettings(live, baseline)
	})
}

func waitForPostgreSQLSlowlogSettings(ctx context.Context, open func(context.Context) (*sql.DB, error), verify func(context.Context, *sql.DB) error) error {
	if open == nil || verify == nil {
		return failure("runtime_unavailable", "PostgreSQL live settings verifier is unavailable", 502)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := waitCtx.Err(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return failure("reload_not_applied", "PostgreSQL did not load the exact slow-log settings within the verification window", 409)
		}
		db, err := open(waitCtx)
		if err == nil {
			err = verify(waitCtx, db)
			_ = db.Close()
			if err == nil {
				return nil
			}
			if code := core.ErrorCode(err); code != "reload_not_applied" && code != "runtime_unavailable" {
				return err
			}
		}
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return failure("reload_not_applied", "PostgreSQL did not load the exact slow-log settings within the verification window", 409)
		case <-ticker.C:
		}
	}
}

func verifyPostgreSQLSlowlogSettingMap(settings map[string]postgresqlSlowlogSetting, expected postgresqlSlowlogConfig) error {
	minimum := expected.MinimumDurationMS
	if !expected.Enabled {
		minimum = -1
	}
	wanted := map[string]string{"log_min_duration_statement": strconv.Itoa(minimum), "log_parameter_max_length": "0", "log_statement": "none", "log_line_prefix": "%m [%p] %u@%d "}
	for name, value := range wanted {
		if settings[name].Name == "" || settings[name].Value != value {
			return failure("reload_not_applied", "PostgreSQL live slow-log settings do not match the typed configuration", 409)
		}
	}
	return nil
}

func validatePostgreSQLServerConfig(ctx context.Context, instance Instance, credential *syscall.Credential) error {
	if err := trustedDatabaseProgram(instance.Service.Program); err != nil {
		return err
	}
	args := []string{"-D", instance.Paths.DataDir, "-c", "config_file=" + filepath.Join(instance.Paths.ConfigDir, "postgresql.conf"), "-C", "log_min_duration_statement"}
	if _, err := runFixed(ctx, instance.Service.Program, args, 10*time.Second, credential); err != nil {
		return failure("configuration_check_failed", "PostgreSQL rejected the owned runtime configuration", 409)
	}
	return nil
}

func (e *PrivilegedExecutor) configurePostgreSQLSlowlog(ctx context.Context, instance Instance, record managedDatabaseRecord, in DatabasePostgreSQLMaintenanceInput) (any, error) {
	if instance.Revision != in.ExpectedInstanceRevision || record.Revision != in.ExpectedDatabaseRevision {
		return nil, failure("revision_conflict", "PostgreSQL instance or database changed; reload slow-log status before editing", 412)
	}
	file, err := readPostgreSQLConfig(instance, int(e.gid))
	if err != nil {
		return nil, err
	}
	revision := databasePostgreSQLConfigDigest(file.Body)
	if revision != in.ExpectedConfigRevision {
		return nil, failure("revision_conflict", "PostgreSQL runtime configuration changed; reload slow-log status before editing", 412)
	}
	beforeSettings, err := e.readPostgreSQLSlowlogSettings(ctx, instance)
	if err != nil || !validPostgreSQLSlowlogBaseline(beforeSettings) {
		if err == nil {
			err = failure("runtime_unavailable", "PostgreSQL prior slow-log settings have an unsupported source or value", 412)
		}
		return nil, err
	}
	after, err := makePostgreSQLSlowlogConfig(file.Body, in.Enabled, in.MinimumDurationMS)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(file.Body, after) {
		return e.postgreSQLSlowlogStatus(ctx, instance, record)
	}
	intent := databasePostgreSQLFileIntent{Version: 1, Kind: "slowlog", InstanceID: instance.ID, Engine: "postgresql", DataDir: instance.Paths.DataDir, ConfigDir: instance.Paths.ConfigDir, Database: record.Database, DatabaseRevision: record.Revision, InstanceRevision: instance.Revision, ExpectedRevision: revision, Before: append([]byte(nil), file.Body...), After: after, BeforeSettings: beforeSettings, BeforeMode: uint32(file.Mode), BeforeUID: file.UID, BeforeGID: file.GID, Operation: "configure", State: "pending", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err = e.save("private", databasePostgreSQLMaintenanceIntentID(instance, "slowlog"), intent); err != nil {
		return nil, failure("operation_recovery_required", "PostgreSQL slow-log intent could not be durably recorded", 409)
	}
	return e.applyPostgreSQLMaintenanceIntent(ctx, instance, intent, true)
}

func (e *PrivilegedExecutor) readPostgreSQLSlowlog(ctx context.Context, instance Instance, record managedDatabaseRecord, limit int) (DatabasePostgreSQLSlowlogPage, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 {
		return DatabasePostgreSQLSlowlogPage{}, failure("invalid_input", "PostgreSQL slow-log page size must be 1–200 entries", 400)
	}
	page, err := e.postgreSQLSlowlogStatus(ctx, instance, record)
	if err != nil {
		return page, err
	}
	if page.ConfigurationState != "settled" {
		return DatabasePostgreSQLSlowlogPage{}, failure("configuration_unmanaged", "PostgreSQL slow logs require the exact typed configuration and a successful runtime reload", 412)
	}
	settings, err := e.readPostgreSQLSlowlogSettings(ctx, instance)
	if err != nil || verifyPostgreSQLSlowlogSettingMap(settings, postgresqlSlowlogConfig{Enabled: page.Enabled, MinimumDurationMS: page.MinimumDurationMS}) != nil {
		return DatabasePostgreSQLSlowlogPage{}, failure("configuration_unmanaged", "PostgreSQL live slow-log settings do not match their protected configuration", 412)
	}
	data, available, truncated, err := readPostgreSQLSlowlogFile(instance, int(e.uid))
	if err != nil {
		return DatabasePostgreSQLSlowlogPage{}, err
	}
	page.LogAvailable, page.Truncated = available, truncated
	page.Items = parsePostgreSQLSlowlogEntries(data, record.Database, limit, &page.Truncated)
	return page, nil
}

func readPostgreSQLSlowlogFile(instance Instance, serviceUID int) ([]byte, bool, bool, error) {
	root, err := os.OpenRoot(instance.Paths.LogDir)
	if err != nil {
		return nil, false, false, failure("log_unavailable", "Managed PostgreSQL log directory is unavailable", 412)
	}
	defer root.Close()
	file, err := root.OpenFile("postgresql.log", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, failure("log_unavailable", "Managed PostgreSQL slow log is unavailable or unsafe", 412)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() < 0 {
		return nil, false, false, failure("log_unavailable", "Managed PostgreSQL slow log metadata is unsafe", 412)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != serviceUID {
		return nil, false, false, failure("log_unavailable", "Managed PostgreSQL slow log ownership changed", 412)
	}
	truncated := info.Size() > databasePostgreSQLSlowlogMaxBytes
	if truncated {
		if _, err = file.Seek(info.Size()-databasePostgreSQLSlowlogMaxBytes, io.SeekStart); err != nil {
			return nil, false, false, failure("log_unavailable", "Managed PostgreSQL slow log could not be read", 502)
		}
	}
	body, err := io.ReadAll(io.LimitReader(file, databasePostgreSQLSlowlogMaxBytes+1))
	if err != nil || len(body) > databasePostgreSQLSlowlogMaxBytes {
		return nil, false, false, failure("log_unavailable", "Managed PostgreSQL slow log exceeded its read bound", 502)
	}
	if truncated {
		if index := bytes.IndexByte(body, '\n'); index >= 0 {
			body = body[index+1:]
		}
	}
	return body, true, truncated, nil
}

func parsePostgreSQLSlowlogEntries(data []byte, database string, limit int, truncated *bool) []DatabasePostgreSQLSlowlogEntry {
	lines := strings.Split(string(data), "\n")
	entries := make([]DatabasePostgreSQLSlowlogEntry, 0, min(limit, len(lines)))
	for _, line := range lines {
		if len(line) > databasePostgreSQLSlowlogMaxLine {
			line = line[:databasePostgreSQLSlowlogMaxLine]
			*truncated = true
		}
		match := postgresqlSlowlogRecordPattern.FindStringSubmatch(strings.TrimSuffix(line, "\r"))
		if match == nil {
			continue
		}
		duration, err := strconv.ParseFloat(match[2], 64)
		if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration < 0 || duration > 1e12 {
			continue
		}
		prefix := strings.TrimSpace(match[1])
		meta := postgresqlSlowlogPrefixPattern.FindStringSubmatch(prefix)
		if meta == nil {
			continue
		}
		if meta[3] != database {
			continue
		}
		statement, statementTruncated := redactPostgreSQLStatement(match[3])
		if statementTruncated {
			*truncated = true
		}
		entries = append(entries, DatabasePostgreSQLSlowlogEntry{StartedAt: safePostgreSQLLogField(meta[1], 128), DurationMS: duration, Role: safePostgreSQLLogField(meta[2], 63), Database: safePostgreSQLLogField(meta[3], 63), Statement: statement})
	}
	if len(entries) > limit {
		*truncated = true
		entries = entries[len(entries)-limit:]
	}
	return entries
}

func safePostgreSQLLogField(value string, maxBytes int) string {
	var out strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) {
			out.WriteByte(' ')
		} else {
			out.WriteRune(r)
		}
		if out.Len() >= maxBytes {
			break
		}
	}
	return strings.TrimSpace(out.String())
}

func redactPostgreSQLStatement(statement string) (string, bool) {
	var out strings.Builder
	truncated := false
	for i := 0; i < len(statement); {
		switch {
		case statement[i] >= '0' && statement[i] <= '9' && (i == 0 || !isPostgreSQLSQLWordByte(statement[i-1])):
			i = consumePostgreSQLNumericLiteral(statement, i)
			out.WriteString("[REDACTED]")
		case statement[i] == '\'':
			i++
			for i < len(statement) {
				if statement[i] == '\\' && i+1 < len(statement) {
					i += 2
					continue
				}
				if statement[i] == '\'' {
					if i+1 < len(statement) && statement[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			out.WriteString("'[REDACTED]'")
		case statement[i] == '$':
			if delimiter, end := postgresDollarQuoteDelimiter(statement[i:]); delimiter != "" {
				rest := statement[i+end:]
				if closeAt := strings.Index(rest, delimiter); closeAt >= 0 {
					i += end + closeAt + len(delimiter)
					out.WriteString("'[REDACTED]'")
					continue
				} else {
					out.WriteString("'[REDACTED]'")
					i = len(statement)
					break
				}
			}
			out.WriteByte(statement[i])
			i++
		case i+1 < len(statement) && statement[i:i+2] == "--":
			if newline := strings.IndexByte(statement[i:], '\n'); newline >= 0 {
				i += newline
			} else {
				i = len(statement)
			}
			out.WriteString(" /*[COMMENT REDACTED]*/ ")
		case i+1 < len(statement) && statement[i:i+2] == "/*":
			i += 2
			depth := 1
			for i < len(statement) && depth > 0 {
				if i+1 < len(statement) && statement[i:i+2] == "/*" {
					depth++
					i += 2
				} else if i+1 < len(statement) && statement[i:i+2] == "*/" {
					depth--
					i += 2
				} else {
					i++
				}
			}
			out.WriteString(" /*[COMMENT REDACTED]*/ ")
		default:
			r := rune(statement[i])
			if unicode.IsControl(r) {
				out.WriteByte(' ')
			} else {
				out.WriteByte(statement[i])
			}
			i++
		}
		if out.Len() >= databasePostgreSQLSlowlogMaxStmt {
			truncated = i < len(statement)
			break
		}
	}
	value := out.String()
	value = postgresqlSlowlogSecretPattern.ReplaceAllString(value, "$1=[REDACTED]")
	if len(value) > databasePostgreSQLSlowlogMaxStmt {
		value = value[:databasePostgreSQLSlowlogMaxStmt]
		truncated = true
	}
	return strings.TrimSpace(value), truncated
}

func isPostgreSQLSQLWordByte(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '$'
}

func consumePostgreSQLNumericLiteral(value string, start int) int {
	i := start
	i = consumePostgreSQLDigits(value, i)
	if i < len(value) && value[i] == '.' {
		i++
		i = consumePostgreSQLDigits(value, i)
	}
	if i < len(value) && (value[i] == 'e' || value[i] == 'E') {
		exponent := i + 1
		if exponent < len(value) && (value[exponent] == '+' || value[exponent] == '-') {
			exponent++
		}
		digitStart := exponent
		exponent = consumePostgreSQLDigits(value, exponent)
		if exponent > digitStart {
			i = exponent
		}
	}
	return i
}

func consumePostgreSQLDigits(value string, start int) int {
	i := start
	for i < len(value) {
		if value[i] >= '0' && value[i] <= '9' {
			i++
			continue
		}
		if value[i] == '_' && i+1 < len(value) && value[i+1] >= '0' && value[i+1] <= '9' {
			i++
			continue
		}
		break
	}
	return i
}

func postgresDollarQuoteDelimiter(value string) (string, int) {
	if len(value) < 2 || value[0] != '$' {
		return "", 0
	}
	for i := 1; i < len(value); i++ {
		if value[i] == '$' {
			tag := value[1:i]
			for j := range tag {
				c := tag[j]
				if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || j > 0 && c >= '0' && c <= '9') {
					return "", 0
				}
			}
			return value[:i+1], i + 1
		}
		if !(value[i] == '_' || value[i] >= 'a' && value[i] <= 'z' || value[i] >= 'A' && value[i] <= 'Z' || value[i] >= '0' && value[i] <= '9') {
			return "", 0
		}
	}
	return "", 0
}
