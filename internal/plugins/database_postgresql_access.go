package plugins

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

const (
	databasePostgreSQLMaintenanceAgentOperation = "database_postgresql_maintenance_action"
	databasePostgreSQLMaintenanceJobOperation   = "database_postgresql_maintenance_job"
	databasePostgreSQLAccessBegin               = "# PowerLinux managed PostgreSQL access v1 begin"
	databasePostgreSQLAccessEnd                 = "# PowerLinux managed PostgreSQL access v1 end"
	databasePostgreSQLMaxConfigBytes            = 128 << 10
	databasePostgreSQLMaxAccessRules            = 256
)

var (
	databasePostgreSQLConfigRevisionPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	databasePostgreSQLRuleIDPattern         = regexp.MustCompile(`^pga_[0-9a-f]{32}$`)
	postgresHBAQuotedRulePattern            = regexp.MustCompile(`^pga_([0-9a-f]{32}):(allow|block)$`)
)

type DatabasePostgreSQLMaintenanceInput struct {
	InstanceID               string `json:"instance_id"`
	Database                 string `json:"database"`
	Command                  string `json:"command"`
	CIDR                     string `json:"cidr,omitempty"`
	AuthMethod               string `json:"auth_method,omitempty"`
	RuleID                   string `json:"rule_id,omitempty"`
	RecoveryChoice           string `json:"recovery_choice,omitempty"`
	ExpectedConfigRevision   string `json:"expected_config_revision,omitempty"`
	ExpectedInstanceRevision int64  `json:"expected_instance_revision,omitempty"`
	ExpectedDatabaseRevision int64  `json:"expected_database_revision,omitempty"`
	Enabled                  bool   `json:"enabled,omitempty"`
	MinimumDurationMS        int    `json:"minimum_duration_ms,omitempty"`
	Limit                    int    `json:"limit,omitempty"`
}

type DatabasePostgreSQLAccessRule struct {
	RuleID     string `json:"rule_id"`
	Database   string `json:"database"`
	Role       string `json:"role"`
	CIDR       string `json:"cidr"`
	AuthMethod string `json:"auth_method"`
	State      string `json:"state"`
}

type DatabasePostgreSQLAccessRules struct {
	InstanceID                   string                         `json:"instance_id"`
	Database                     string                         `json:"database"`
	InstanceRevision             int64                          `json:"instance_revision"`
	DatabaseRevision             int64                          `json:"database_revision"`
	ConfigRevision               string                         `json:"config_revision"`
	ConfigurationState           string                         `json:"configuration_state"`
	ProtectedLocalAdminPreserved bool                           `json:"protected_local_admin_preserved"`
	RemoteAccessSupported        bool                           `json:"remote_access_supported"`
	RemoteAccessLimitation       string                         `json:"remote_access_limitation"`
	Rules                        []DatabasePostgreSQLAccessRule `json:"rules"`
}

type postgresAccessFile struct {
	Body []byte
	Mode os.FileMode
	UID  int
	GID  int
}

type databasePostgreSQLFileIntent struct {
	Version          int       `json:"version"`
	Kind             string    `json:"kind"`
	InstanceID       string    `json:"instance_id"`
	Engine           string    `json:"engine"`
	DataDir          string    `json:"data_dir"`
	ConfigDir        string    `json:"config_dir"`
	Database         string    `json:"database"`
	Role             string    `json:"role,omitempty"`
	DatabaseRevision int64     `json:"database_revision"`
	InstanceRevision int64     `json:"instance_revision"`
	ExpectedRevision string    `json:"expected_revision"`
	Before           []byte    `json:"before"`
	After            []byte    `json:"after"`
	BeforeMode       uint32    `json:"before_mode"`
	BeforeUID        int       `json:"before_uid"`
	BeforeGID        int       `json:"before_gid"`
	Operation        string    `json:"operation"`
	State            string    `json:"state"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type databasePostgreSQLAccessRecoveryContextKey struct{}

type databasePostgreSQLAccessRecoveryContext struct {
	executor       *PrivilegedExecutor
	instance       string
	kind           string
	expectedBefore string
	expectedAfter  string
	createdAt      time.Time
}

func (e *PrivilegedExecutor) ValidatePostgreSQLMaintenance(raw json.RawMessage) error {
	var in DatabasePostgreSQLMaintenanceInput
	if err := parse(raw, &in); err != nil {
		return err
	}
	return validatePostgreSQLMaintenanceInputFull(in)
}

func validatePostgreSQLMaintenanceInputFull(in DatabasePostgreSQLMaintenanceInput) error {
	if !validInstanceID(in.InstanceID) || !validDatabaseName(in.Database) {
		return failure("invalid_input", "A managed PostgreSQL instance and database are required", 400)
	}
	if in.ExpectedConfigRevision != "" && !databasePostgreSQLConfigRevisionPattern.MatchString(in.ExpectedConfigRevision) {
		return failure("invalid_input", "PostgreSQL configuration revision is invalid", 400)
	}
	switch in.Command {
	case "access_list", "slowlog_status", "slowlog_read":
		if in.ExpectedConfigRevision != "" || in.ExpectedInstanceRevision != 0 || in.ExpectedDatabaseRevision != 0 || in.CIDR != "" || in.RuleID != "" || in.AuthMethod != "" || in.RecoveryChoice != "" || in.Enabled || in.MinimumDurationMS != 0 {
			return failure("invalid_input", "Read-only PostgreSQL maintenance commands do not accept mutation fields", 400)
		}
		if in.Command == "slowlog_read" && in.Limit != 0 && (in.Limit < 1 || in.Limit > 200) {
			return failure("invalid_input", "PostgreSQL slow-log page size must be 1–200 entries", 400)
		}
		if in.Command != "slowlog_read" && in.Limit != 0 {
			return failure("invalid_input", "This PostgreSQL command does not accept a page size", 400)
		}
	case "access_add":
		if !validPostgreSQLLoopbackCIDR(in.CIDR) || in.AuthMethod != "scram-sha-256" || !validPostgreSQLCAS(in) {
			return failure("invalid_input", "Access rules require a loopback CIDR, SCRAM authentication and current revisions", 400)
		}
		if in.RuleID != "" || in.RecoveryChoice != "" || in.Enabled || in.MinimumDurationMS != 0 || in.Limit != 0 {
			return failure("invalid_input", "Access rule addition contains unexpected fields", 400)
		}
	case "access_remove":
		if !databasePostgreSQLRuleIDPattern.MatchString(in.RuleID) || !validPostgreSQLCAS(in) {
			return failure("invalid_input", "Access rule removal requires its exact managed ID and current revisions", 400)
		}
		if in.CIDR != "" || in.AuthMethod != "" || in.RecoveryChoice != "" || in.Enabled || in.MinimumDurationMS != 0 || in.Limit != 0 {
			return failure("invalid_input", "Access rule removal contains unexpected fields", 400)
		}
	case "access_recover", "slowlog_recover":
		if !validPostgreSQLCAS(in) || (in.RecoveryChoice != "apply_pending" && in.RecoveryChoice != "restore_previous") {
			return failure("invalid_input", "PostgreSQL recovery requires an explicit choice and current revisions", 400)
		}
		if in.CIDR != "" || in.RuleID != "" || in.AuthMethod != "" || in.Enabled || in.MinimumDurationMS != 0 || in.Limit != 0 {
			return failure("invalid_input", "PostgreSQL recovery contains unexpected fields", 400)
		}
	case "slowlog_configure":
		if !validPostgreSQLCAS(in) || in.MinimumDurationMS < 1 || in.MinimumDurationMS > 600000 {
			return failure("invalid_input", "Slow-query logging requires current revisions and a threshold of 1–600000 milliseconds", 400)
		}
		if in.CIDR != "" || in.RuleID != "" || in.AuthMethod != "" || in.RecoveryChoice != "" || in.Limit != 0 {
			return failure("invalid_input", "Slow-query logging configuration contains unexpected fields", 400)
		}
	default:
		return failure("invalid_input", "Unsupported typed PostgreSQL maintenance command", 400)
	}
	return nil
}

func validPostgreSQLCAS(in DatabasePostgreSQLMaintenanceInput) bool {
	return databasePostgreSQLConfigRevisionPattern.MatchString(in.ExpectedConfigRevision) && in.ExpectedInstanceRevision > 0 && in.ExpectedDatabaseRevision > 0
}

func validPostgreSQLLoopbackCIDR(value string) bool {
	prefix, err := netip.ParsePrefix(value)
	if err != nil || prefix != prefix.Masked() || !prefix.Addr().IsLoopback() {
		return false
	}
	if prefix.Addr().Is4() {
		return prefix.Bits() >= 8 && netip.MustParsePrefix("127.0.0.0/8").Contains(prefix.Addr())
	}
	return prefix.Bits() == 128 && prefix.Addr() == netip.IPv6Loopback()
}

func (e *PrivilegedExecutor) RunPostgreSQLMaintenance(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := e.ValidatePostgreSQLMaintenance(raw); err != nil {
		return nil, err
	}
	var in DatabasePostgreSQLMaintenanceInput
	_ = json.Unmarshal(raw, &in)
	unlock, err := e.lockContext(ctx, in.InstanceID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	switch in.Command {
	case "access_list", "slowlog_status", "slowlog_read":
		// Read-only status must remain available while a protected journal needs
		// recovery so the user can inspect the exact current revision.
	case "access_recover", "slowlog_recover":
		// Recovery constructs its exact-journal context before the full guard.
	default:
		if err = e.ensureInstallFinalized(ctx, in.InstanceID); err != nil {
			return nil, err
		}
	}
	instance, err := e.get(ctx, in.InstanceID, false)
	if err != nil {
		return nil, err
	}
	if instance.PluginID != "postgresql" || instance.Ownership != "managed" || instance.Status == "uninstalled" {
		return nil, failure("runtime_type_mismatch", "PostgreSQL maintenance requires an installed managed PostgreSQL instance", 412)
	}
	if !postgresqlLoopbackBinding(instance) {
		return nil, failure("listener_policy", "PostgreSQL access rules currently require the managed loopback listener; public bindings need a TLS profile", 412)
	}
	if in.Command == "access_add" || in.Command == "access_remove" || in.Command == "slowlog_configure" {
		if err = e.ensurePostgreSQLMaintenanceSettled(ctx, instance); err != nil {
			return nil, err
		}
	}
	if in.Command == "access_list" || in.Command == "access_add" || in.Command == "access_remove" || in.Command == "access_recover" || in.Command == "slowlog_status" || in.Command == "slowlog_configure" || in.Command == "slowlog_read" || in.Command == "slowlog_recover" {
		if in.ExpectedInstanceRevision > 0 && instance.Revision != in.ExpectedInstanceRevision {
			return nil, failure("revision_conflict", "PostgreSQL instance changed; refresh its maintenance status", 412)
		}
	}
	record, err := e.loadPostgreSQLMaintenanceDatabase(instance, in.Database, in.ExpectedDatabaseRevision, in.ExpectedDatabaseRevision > 0)
	if err != nil {
		return nil, err
	}
	if in.Command == "access_recover" || in.Command == "slowlog_recover" {
		return e.recoverPostgreSQLMaintenance(ctx, instance, record, in)
	}
	switch in.Command {
	case "access_list":
		return e.listPostgreSQLAccessRules(ctx, instance, record)
	case "access_add":
		return e.mutatePostgreSQLAccess(ctx, instance, record, in, true)
	case "access_remove":
		return e.mutatePostgreSQLAccess(ctx, instance, record, in, false)
	case "slowlog_status":
		return e.postgreSQLSlowlogStatus(ctx, instance, record)
	case "slowlog_configure":
		return e.configurePostgreSQLSlowlog(ctx, instance, record, in)
	case "slowlog_read":
		return e.readPostgreSQLSlowlog(ctx, instance, record, in.Limit)
	default:
		return nil, failure("invalid_input", "Unsupported typed PostgreSQL maintenance command", 400)
	}
}

func postgresqlLoopbackBinding(instance Instance) bool {
	bind, _ := instance.Config["bind"].(string)
	if bind == "" {
		bind = "127.0.0.1"
	}
	ip, _ := netip.ParseAddr(bind)
	return ip.IsValid() && ip.IsLoopback()
}

func (e *PrivilegedExecutor) loadPostgreSQLMaintenanceDatabase(instance Instance, name string, expectedRevision int64, compare bool) (managedDatabaseRecord, error) {
	var record managedDatabaseRecord
	if !validDatabaseName(name) {
		return record, failure("invalid_input", "A concrete managed PostgreSQL database is required", 400)
	}
	if err := e.load("private", managedDatabaseKey(instance.ID, name), &record); err != nil {
		if core.ErrorCode(err) == "not_found" {
			return record, failure("database_receipt_not_found", "Managed database receipt does not exist for this PostgreSQL instance", 404)
		}
		return record, err
	}
	if record.InstanceID != instance.ID || record.Engine != "postgresql" || record.Database != name || record.State != "active" || !validDatabaseName(record.Username) {
		return record, failure("ownership_invalid", "Managed PostgreSQL database receipt is inconsistent or inactive", 412)
	}
	if compare && record.Revision != expectedRevision {
		return record, failure("revision_conflict", "Managed PostgreSQL database changed; refresh its maintenance status", 412)
	}
	return record, nil
}

func databasePostgreSQLMaintenanceIntentID(instance Instance, kind string) string {
	digest := sha256.Sum256([]byte(instance.ID + "|" + kind))
	return "database_postgresql_" + kind + "_" + hex.EncodeToString(digest[:16])
}

func databasePostgreSQLConfigDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func databasePostgreSQLAccessRuleID(instanceID, database, role, cidr string) string {
	canonical := strings.Join([]string{instanceID, database, role, cidr, "scram-sha-256"}, "\x00")
	digest := sha256.Sum256([]byte(canonical))
	return "pga_" + hex.EncodeToString(digest[:16])
}

func parsePostgreSQLHBAAccess(body []byte, instanceID, adminRole string) ([]DatabasePostgreSQLAccessRule, bool, error) {
	text := string(body)
	starts, ends := strings.Count(text, databasePostgreSQLAccessBegin), strings.Count(text, databasePostgreSQLAccessEnd)
	if starts == 0 && ends == 0 {
		return []DatabasePostgreSQLAccessRule{}, false, nil
	}
	if starts != 1 || ends != 1 {
		return nil, false, failure("configuration_unmanaged", "PostgreSQL access file has an ambiguous managed marker; inspect it before editing", 412)
	}
	start := strings.Index(text, databasePostgreSQLAccessBegin)
	end := strings.Index(text, databasePostgreSQLAccessEnd)
	if start != 0 || end <= start || !strings.HasPrefix(text[end+len(databasePostgreSQLAccessEnd):], "\n") && end+len(databasePostgreSQLAccessEnd) != len(text) {
		return nil, false, failure("configuration_unmanaged", "Managed PostgreSQL access rules are not at the exact owned file prefix", 412)
	}
	block := strings.TrimSuffix(text[start:end], "\n")
	lines := strings.Split(block, "\n")
	if len(lines) < 4 || lines[0] != databasePostgreSQLAccessBegin {
		return nil, false, failure("configuration_unmanaged", "Managed PostgreSQL access rules are incomplete", 412)
	}
	adminHeader := "# admin-role=" + adminRole
	if lines[1] != adminHeader || lines[2] != "host all "+adminRole+" 127.0.0.0/8 scram-sha-256" || lines[3] != "host all "+adminRole+" ::1/128 scram-sha-256" {
		return nil, false, failure("configuration_unmanaged", "Protected PostgreSQL administrator authentication entries changed", 412)
	}
	rules := make([]DatabasePostgreSQLAccessRule, 0, len(lines)-4)
	seen := map[string]bool{}
	for _, line := range lines[4:] {
		fields := strings.Fields(line)
		if len(fields) != 7 || fields[0] != "host" || fields[5] != "#" {
			return nil, false, failure("configuration_unmanaged", "Managed PostgreSQL access block contains an unrecognized line", 412)
		}
		database, role, cidr, method := fields[1], fields[2], fields[3], fields[4]
		if len(database) < 2 || database[0] != '"' || database[len(database)-1] != '"' || len(role) < 2 || role[0] != '"' || role[len(role)-1] != '"' {
			return nil, false, failure("configuration_unmanaged", "Managed PostgreSQL access database and role must be quoted literal names", 412)
		}
		database, role = database[1:len(database)-1], role[1:len(role)-1]
		match := postgresHBAQuotedRulePattern.FindStringSubmatch(fields[6])
		if !validDatabaseName(database) || !validDatabaseName(role) || !validPostgreSQLLoopbackCIDR(cidr) || match == nil {
			return nil, false, failure("configuration_unmanaged", "Managed PostgreSQL access rule is malformed", 412)
		}
		id := "pga_" + match[1]
		if databasePostgreSQLAccessRuleID(instanceID, database, role, cidr) != id {
			return nil, false, failure("configuration_unmanaged", "Managed PostgreSQL access rule ID does not match its exact target", 412)
		}
		state, authMethod := "allowed", "scram-sha-256"
		if match[2] == "block" {
			state, authMethod = "blocked", "reject"
			if method != "reject" {
				return nil, false, failure("configuration_unmanaged", "Managed PostgreSQL deny rule changed its authentication method", 412)
			}
		} else if method != "scram-sha-256" {
			return nil, false, failure("configuration_unmanaged", "Managed PostgreSQL access uses an unsupported authentication method", 412)
		}
		if seen[id] {
			return nil, false, failure("configuration_unmanaged", "Managed PostgreSQL access block contains duplicate rules", 412)
		}
		seen[id] = true
		rules = append(rules, DatabasePostgreSQLAccessRule{RuleID: id, Database: database, Role: role, CIDR: cidr, AuthMethod: authMethod, State: state})
	}
	if len(rules) > databasePostgreSQLMaxAccessRules {
		return nil, false, failure("configuration_unmanaged", "Managed PostgreSQL access block exceeds its bound", 412)
	}
	return rules, true, nil
}

func protectedPostgreSQLLocalAdminAuth(body []byte) bool {
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "local" || (fields[1] != "all" && fields[1] != "postgres") || (fields[2] != "all" && fields[2] != "postgres") {
			continue
		}
		if fields[3] == "peer" || fields[3] == "scram-sha-256" {
			return true
		}
	}
	return false
}

func renderPostgreSQLHBAAccess(body []byte, adminRole string, rules []DatabasePostgreSQLAccessRule) []byte {
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].Database != rules[j].Database {
			return rules[i].Database < rules[j].Database
		}
		if rules[i].Role != rules[j].Role {
			return rules[i].Role < rules[j].Role
		}
		return rules[i].CIDR < rules[j].CIDR
	})
	var block strings.Builder
	block.WriteString(databasePostgreSQLAccessBegin + "\n")
	block.WriteString("# admin-role=" + adminRole + "\n")
	block.WriteString("host all " + adminRole + " 127.0.0.0/8 scram-sha-256\n")
	block.WriteString("host all " + adminRole + " ::1/128 scram-sha-256\n")
	for _, rule := range rules {
		method, state := "scram-sha-256", "allow"
		if rule.State == "blocked" {
			method, state = "reject", "block"
		}
		block.WriteString("host \"" + rule.Database + "\" \"" + rule.Role + "\" " + rule.CIDR + " " + method + " # " + rule.RuleID + ":" + state + "\n")
	}
	block.WriteString(databasePostgreSQLAccessEnd + "\n")
	text := string(body)
	if strings.Contains(text, databasePostgreSQLAccessBegin) || strings.Contains(text, databasePostgreSQLAccessEnd) {
		end := strings.Index(text, databasePostgreSQLAccessEnd)
		return []byte(block.String() + text[end+len(databasePostgreSQLAccessEnd):])
	}
	return append([]byte(block.String()), body...)
}

func readPostgreSQLHBA(instance Instance, serviceUID, serviceGID int) (postgresAccessFile, error) {
	root, err := os.OpenRoot(instance.Paths.DataDir)
	if err != nil {
		return postgresAccessFile{}, failure("configuration_unmanaged", "Managed PostgreSQL access file is unavailable", 412)
	}
	defer root.Close()
	file, err := root.OpenFile("pg_hba.conf", os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return postgresAccessFile{}, failure("configuration_unmanaged", "Managed PostgreSQL access file is unavailable or unsafe", 412)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > databasePostgreSQLMaxConfigBytes || info.Mode().Perm()&0022 != 0 {
		return postgresAccessFile{}, failure("configuration_unmanaged", "Managed PostgreSQL access file metadata is unsafe", 412)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (int(stat.Uid) != 0 && int(stat.Uid) != serviceUID) || (int(stat.Gid) != serviceGID && int(stat.Uid) == 0) {
		return postgresAccessFile{}, failure("configuration_unmanaged", "Managed PostgreSQL access file ownership changed", 412)
	}
	body, err := io.ReadAll(io.LimitReader(file, databasePostgreSQLMaxConfigBytes+1))
	if err != nil || len(body) > databasePostgreSQLMaxConfigBytes {
		return postgresAccessFile{}, failure("configuration_unmanaged", "Managed PostgreSQL access file exceeds its bound", 412)
	}
	return postgresAccessFile{Body: body, Mode: info.Mode().Perm(), UID: int(stat.Uid), GID: int(stat.Gid)}, nil
}

func writePostgreSQLManagedFile(base, name string, body []byte, mode os.FileMode, uid, gid int) error {
	if !filepath.IsLocal(name) || len(body) > databasePostgreSQLMaxConfigBytes {
		return failure("invalid_path", "PostgreSQL managed file is outside its bounded root", 400)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer root.Close()
	random := make([]byte, 12)
	if _, err = rand.Read(random); err != nil {
		return err
	}
	temp := ".postgresql-maintenance-" + hex.EncodeToString(random)
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	if _, err = file.Write(body); err == nil {
		err = file.Sync()
	}
	if err == nil && os.Geteuid() == 0 {
		err = file.Chown(uid, gid)
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
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	if err = root.Rename(temp, name); err != nil {
		return err
	}
	return directory.Sync()
}

func databasePostgreSQLIntentContext(ctx context.Context, executor *PrivilegedExecutor, instanceID string, intent databasePostgreSQLFileIntent) context.Context {
	return context.WithValue(ctx, databasePostgreSQLAccessRecoveryContextKey{}, databasePostgreSQLAccessRecoveryContext{
		executor: executor, instance: instanceID, kind: intent.Kind,
		expectedBefore: intent.ExpectedRevision, expectedAfter: databasePostgreSQLConfigDigest(intent.After), createdAt: intent.CreatedAt,
	})
}

func matchesPostgreSQLMaintenanceRecoveryContext(recovery databasePostgreSQLAccessRecoveryContext, executor *PrivilegedExecutor, instanceID string, intent databasePostgreSQLFileIntent) bool {
	return recovery.executor == executor && recovery.instance == instanceID && recovery.kind == intent.Kind && intent.State == "pending" &&
		recovery.expectedBefore == intent.ExpectedRevision && recovery.expectedAfter == databasePostgreSQLConfigDigest(intent.After) && recovery.createdAt.Equal(intent.CreatedAt)
}

func (e *PrivilegedExecutor) ensurePostgreSQLMaintenanceSettled(ctx context.Context, instance Instance) error {
	allowedKind := ""
	if recovery, ok := ctx.Value(databasePostgreSQLAccessRecoveryContextKey{}).(databasePostgreSQLAccessRecoveryContext); ok && recovery.executor == e && recovery.instance == instance.ID {
		intent, err := e.loadPostgreSQLMaintenanceIntent(instance, recovery.kind)
		if err != nil {
			return err
		}
		if intent == nil || !matchesPostgreSQLMaintenanceRecoveryContext(recovery, e, instance.ID, *intent) {
			return failure("operation_recovery_required", "PostgreSQL recovery context no longer matches its exact protected journal", 409)
		}
		allowedKind = recovery.kind
	}
	for _, kind := range []string{"access", "slowlog"} {
		intent, err := e.loadPostgreSQLMaintenanceIntent(instance, kind)
		if err != nil {
			return err
		}
		if intent != nil && intent.State == "pending" && kind != allowedKind {
			return failure("operation_recovery_required", "PostgreSQL configuration has an unfinished change; inspect and explicitly recover it before another mutation", 409)
		}
	}
	return nil
}

// ensurePostgreSQLMaintenanceFinalized is called by shared mutation guards.
// It deliberately treats non-PostgreSQL and missing receipts as out of scope.
func (e *PrivilegedExecutor) ensurePostgreSQLMaintenanceFinalized(ctx context.Context, instanceID string) error {
	if !validInstanceID(instanceID) {
		return failure("invalid_input", "Invalid software instance ID", 400)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var instance Instance
	if err := e.load("receipts", instanceID, &instance); err != nil {
		if core.ErrorCode(err) == "not_found" {
			return nil
		}
		return err
	}
	if instance.ID != instanceID {
		return failure("ownership_invalid", "Software receipt disagrees with its maintenance guard identity", 412)
	}
	if instance.PluginID != "postgresql" || instance.Ownership != "managed" || instance.Status == "uninstalled" {
		return nil
	}
	verified, err := e.get(ctx, instanceID, false)
	if err != nil {
		return err
	}
	return e.ensurePostgreSQLMaintenanceSettled(ctx, verified)
}

func (e *PrivilegedExecutor) loadPostgreSQLMaintenanceIntent(instance Instance, kind string) (*databasePostgreSQLFileIntent, error) {
	var intent databasePostgreSQLFileIntent
	err := e.load("private", databasePostgreSQLMaintenanceIntentID(instance, kind), &intent)
	if core.ErrorCode(err) == "not_found" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !validPostgreSQLMaintenanceIntent(instance, kind, intent) {
		return nil, failure("ownership_invalid", "Protected PostgreSQL maintenance intent does not match its instance", 412)
	}
	return &intent, nil
}

func validPostgreSQLMaintenanceIntent(instance Instance, kind string, intent databasePostgreSQLFileIntent) bool {
	if intent.Version != 1 || intent.Kind != kind || intent.InstanceID != instance.ID || intent.Engine != "postgresql" || intent.DataDir != instance.Paths.DataDir || intent.ConfigDir != instance.Paths.ConfigDir ||
		!validDatabaseName(intent.Database) || intent.DatabaseRevision < 1 || intent.InstanceRevision < 1 || intent.InstanceRevision > instance.Revision ||
		!databasePostgreSQLConfigRevisionPattern.MatchString(intent.ExpectedRevision) || len(intent.Before) == 0 || len(intent.Before) > databasePostgreSQLMaxConfigBytes || len(intent.After) == 0 || len(intent.After) > databasePostgreSQLMaxConfigBytes ||
		(intent.State != "pending" && intent.State != "completed" && intent.State != "restored") || intent.CreatedAt.IsZero() || intent.UpdatedAt.Before(intent.CreatedAt) {
		return false
	}
	if databasePostgreSQLConfigDigest(intent.Before) != intent.ExpectedRevision {
		return false
	}
	if kind == "access" {
		_, ok, err := parsePostgreSQLHBAAccess(intent.After, instance.ID, intent.Role)
		return err == nil && ok
	}
	_, ok, err := parsePostgreSQLSlowlogBlock(intent.After)
	return err == nil && ok
}

func (e *PrivilegedExecutor) listPostgreSQLAccessRules(ctx context.Context, instance Instance, record managedDatabaseRecord) (DatabasePostgreSQLAccessRules, error) {
	file, err := readPostgreSQLHBA(instance, int(e.uid), int(e.gid))
	if err != nil {
		return DatabasePostgreSQLAccessRules{}, err
	}
	var credential managedDatabaseCredential
	if err = e.load("private", databasePrivateID(instance), &credential); err != nil || credential.Engine != "postgresql" || credential.Username == "" {
		return DatabasePostgreSQLAccessRules{}, failure("credential_unavailable", "Owned PostgreSQL administrator identity is unavailable", 412)
	}
	pending, intentErr := e.loadPostgreSQLMaintenanceIntent(instance, "access")
	if intentErr != nil {
		return DatabasePostgreSQLAccessRules{}, intentErr
	}
	pendingRecovery := pending != nil && pending.State == "pending"
	rules, configured, err := parsePostgreSQLHBAAccess(file.Body, instance.ID, credential.Username)
	if err != nil {
		if !pendingRecovery {
			return DatabasePostgreSQLAccessRules{}, err
		}
		rules, configured = []DatabasePostgreSQLAccessRule{}, false
	}
	matching := make([]DatabasePostgreSQLAccessRule, 0)
	for _, rule := range rules {
		if rule.Database == record.Database && rule.Role == record.Username {
			matching = append(matching, rule)
		}
	}
	state := "unconfigured"
	if configured {
		state = "settled"
	}
	if pendingRecovery {
		state = "pending_recovery"
	}
	protected := protectedPostgreSQLLocalAdminAuth(file.Body)
	if !protected && !pendingRecovery {
		return DatabasePostgreSQLAccessRules{}, failure("configuration_unmanaged", "Protected local PostgreSQL administrator authentication is missing", 412)
	}
	if state != "pending_recovery" {
		if err = e.checkPostgreSQLHBAFile(ctx, instance); err != nil {
			return DatabasePostgreSQLAccessRules{}, err
		}
	}
	return DatabasePostgreSQLAccessRules{InstanceID: instance.ID, Database: record.Database, InstanceRevision: instance.Revision, DatabaseRevision: record.Revision, ConfigRevision: databasePostgreSQLConfigDigest(file.Body), ConfigurationState: state, ProtectedLocalAdminPreserved: protected, RemoteAccessSupported: false, RemoteAccessLimitation: "managed_tls_not_configured", Rules: matching}, nil
}

func (e *PrivilegedExecutor) checkPostgreSQLHBAFile(ctx context.Context, instance Instance) error {
	db, err := e.openManagedAdmin(ctx, instance)
	if err != nil {
		return failure("runtime_unavailable", "Owned PostgreSQL administrator connection is unavailable", 412)
	}
	defer db.Close()
	return validatePostgreSQLHBAFile(ctx, db)
}

func validatePostgreSQLHBAFile(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) error {
	var invalid int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM pg_hba_file_rules WHERE error IS NOT NULL").Scan(&invalid); err != nil || invalid != 0 {
		return failure("configuration_invalid", "PostgreSQL rejected or cannot inspect its client authentication file", 409)
	}
	return nil
}

func reloadPostgreSQLConfig(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) error {
	var reloaded bool
	if err := db.QueryRowContext(ctx, "SELECT pg_reload_conf()").Scan(&reloaded); err != nil || !reloaded {
		return failure("reload_not_applied", "PostgreSQL did not acknowledge the configuration reload", 409)
	}
	return nil
}

func (e *PrivilegedExecutor) mutatePostgreSQLAccess(ctx context.Context, instance Instance, record managedDatabaseRecord, in DatabasePostgreSQLMaintenanceInput, allow bool) (any, error) {
	if instance.Revision != in.ExpectedInstanceRevision || record.Revision != in.ExpectedDatabaseRevision {
		return nil, failure("revision_conflict", "PostgreSQL instance or database changed; reload access rules before editing", 412)
	}
	file, err := readPostgreSQLHBA(instance, int(e.uid), int(e.gid))
	if err != nil {
		return nil, err
	}
	revision := databasePostgreSQLConfigDigest(file.Body)
	if revision != in.ExpectedConfigRevision {
		return nil, failure("revision_conflict", "PostgreSQL client authentication file changed; reload access rules before editing", 412)
	}
	if !protectedPostgreSQLLocalAdminAuth(file.Body) {
		return nil, failure("configuration_unmanaged", "Protected local PostgreSQL administrator authentication is missing; no change was made", 412)
	}
	var credential managedDatabaseCredential
	if err = e.load("private", databasePrivateID(instance), &credential); err != nil || credential.Engine != "postgresql" || credential.Username == "" {
		return nil, failure("credential_unavailable", "Owned PostgreSQL administrator identity is unavailable", 412)
	}
	rules, managed, err := parsePostgreSQLHBAAccess(file.Body, instance.ID, credential.Username)
	if err != nil {
		return nil, err
	}
	if !managed && !protectedPostgreSQLLocalAdminAuth(file.Body) {
		return nil, failure("configuration_unmanaged", "PostgreSQL local administrator authentication is not a protected supported profile", 412)
	}
	id := in.RuleID
	if allow {
		canonical, _ := netip.ParsePrefix(in.CIDR)
		cidr := canonical.String()
		id = databasePostgreSQLAccessRuleID(instance.ID, record.Database, record.Username, cidr)
	}
	found := -1
	for i := range rules {
		if rules[i].RuleID == id {
			found = i
			break
		}
	}
	if allow {
		if found >= 0 {
			rules[found].State, rules[found].AuthMethod = "allowed", "scram-sha-256"
		} else {
			if len(rules) >= databasePostgreSQLMaxAccessRules {
				return nil, failure("result_limit", "Managed PostgreSQL access rules reached their limit", 413)
			}
			rules = append(rules, DatabasePostgreSQLAccessRule{RuleID: id, Database: record.Database, Role: record.Username, CIDR: in.CIDR, AuthMethod: "scram-sha-256", State: "allowed"})
		}
	} else {
		if found < 0 {
			return nil, failure("not_found", "Managed PostgreSQL access rule does not exist", 404)
		}
		if rules[found].RuleID != databasePostgreSQLAccessRuleID(instance.ID, record.Database, record.Username, rules[found].CIDR) {
			return nil, failure("ownership_invalid", "Access rule ID does not belong to this managed database and role", 412)
		}
		rules[found].State, rules[found].AuthMethod = "blocked", "reject"
	}
	if !managed && !protectedPostgreSQLLocalAdminAuth(file.Body) {
		return nil, failure("configuration_unmanaged", "PostgreSQL authentication file cannot be safely managed", 412)
	}
	after := renderPostgreSQLHBAAccess(file.Body, credential.Username, rules)
	intent := databasePostgreSQLFileIntent{Version: 1, Kind: "access", InstanceID: instance.ID, Engine: "postgresql", DataDir: instance.Paths.DataDir, ConfigDir: instance.Paths.ConfigDir, Database: record.Database, Role: credential.Username, DatabaseRevision: record.Revision, InstanceRevision: instance.Revision, ExpectedRevision: revision, Before: append([]byte(nil), file.Body...), After: after, BeforeMode: uint32(file.Mode), BeforeUID: file.UID, BeforeGID: file.GID, Operation: map[bool]string{true: "add", false: "remove"}[allow], State: "pending", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err = e.save("private", databasePostgreSQLMaintenanceIntentID(instance, "access"), intent); err != nil {
		return nil, failure("operation_recovery_required", "PostgreSQL access-rule intent could not be durably recorded", 409)
	}
	return e.applyPostgreSQLMaintenanceIntent(ctx, instance, intent, true)
}

func (e *PrivilegedExecutor) recoverPostgreSQLMaintenance(ctx context.Context, instance Instance, record managedDatabaseRecord, in DatabasePostgreSQLMaintenanceInput) (any, error) {
	kind := "access"
	if in.Command == "slowlog_recover" {
		kind = "slowlog"
	}
	intent, err := e.loadPostgreSQLMaintenanceIntent(instance, kind)
	if err != nil {
		return nil, err
	}
	if intent == nil || intent.State != "pending" || intent.Database != in.Database {
		return nil, failure("operation_not_pending", "No matching PostgreSQL configuration change requires recovery", 409)
	}
	if record.Revision != in.ExpectedDatabaseRevision || record.Database != intent.Database || (kind == "access" && record.Username != intent.Role) {
		return nil, failure("revision_conflict", "PostgreSQL recovery authorization no longer matches its recorded instance and database revisions", 412)
	}
	if !databasePostgreSQLConfigRevisionPattern.MatchString(in.ExpectedConfigRevision) {
		return nil, failure("invalid_input", "A current PostgreSQL configuration revision is required for recovery", 400)
	}
	recoveryCtx := databasePostgreSQLIntentContext(ctx, e, instance.ID, *intent)
	if err = e.ensureInstallFinalized(recoveryCtx, instance.ID); err != nil {
		return nil, err
	}
	var current postgresAccessFile
	if kind == "access" {
		current, err = readPostgreSQLHBA(instance, int(e.uid), int(e.gid))
	} else {
		current, err = readPostgreSQLConfig(instance, int(e.gid))
	}
	if err != nil {
		return nil, err
	}
	if databasePostgreSQLConfigDigest(current.Body) != in.ExpectedConfigRevision {
		return nil, failure("revision_conflict", "PostgreSQL configuration changed; inspect current status before recovery", 412)
	}
	if !bytes.Equal(current.Body, intent.Before) && !bytes.Equal(current.Body, intent.After) {
		return nil, failure("operation_recovery_required", "PostgreSQL configuration differs from both exact journaled versions; no overwrite was attempted", 409)
	}
	return e.applyPostgreSQLMaintenanceIntent(recoveryCtx, instance, *intent, in.RecoveryChoice == "apply_pending")
}

func (e *PrivilegedExecutor) applyPostgreSQLMaintenanceIntent(ctx context.Context, instance Instance, intent databasePostgreSQLFileIntent, applyAfter bool) (any, error) {
	kind := intent.Kind
	if !validPostgreSQLMaintenanceIntent(instance, kind, intent) || intent.State != "pending" {
		return nil, failure("ownership_invalid", "PostgreSQL recovery journal is invalid", 412)
	}
	var current postgresAccessFile
	var err error
	if kind == "access" {
		current, err = readPostgreSQLHBA(instance, int(e.uid), int(e.gid))
	} else {
		current, err = readPostgreSQLConfig(instance, int(e.gid))
	}
	if err != nil {
		return nil, err
	}
	target, targetMode, targetUID, targetGID, completion := intent.After, os.FileMode(0640), 0, int(e.gid), "completed"
	if !applyAfter {
		target, targetMode, targetUID, targetGID, completion = intent.Before, os.FileMode(intent.BeforeMode), intent.BeforeUID, intent.BeforeGID, "restored"
	}
	if !bytes.Equal(current.Body, intent.Before) && !bytes.Equal(current.Body, intent.After) {
		return nil, failure("operation_recovery_required", "PostgreSQL configuration differs from both exact journaled versions; no overwrite was attempted", 409)
	}
	if !bytes.Equal(current.Body, target) {
		base, name := instance.Paths.DataDir, "pg_hba.conf"
		if kind == "slowlog" {
			base, name = instance.Paths.ConfigDir, "postgresql.conf"
			targetMode, targetUID, targetGID = current.Mode, 0, current.GID
		}
		if err = writePostgreSQLManagedFile(base, name, target, targetMode, targetUID, targetGID); err != nil {
			return nil, failure("operation_recovery_required", "Journaled PostgreSQL configuration remains pending; use explicit recovery", 409)
		}
	}
	verifyErr := e.verifyPostgreSQLMaintenanceRuntime(ctx, instance, intent, target, applyAfter)
	if verifyErr != nil {
		if !applyAfter {
			return nil, failure("operation_recovery_required", "Previous PostgreSQL configuration was restored but runtime verification is still pending", 409)
		}
		// A failed check attempts exact rollback. If the rollback reload cannot be
		// proven, the journal remains pending and only explicit recovery may proceed.
		if rollbackErr := e.restorePostgreSQLMaintenanceBefore(ctx, instance, intent); rollbackErr != nil {
			return nil, failure("operation_recovery_required", "PostgreSQL configuration was rejected and exact rollback could not be verified", 409)
		}
		intent.State, intent.UpdatedAt = "restored", time.Now().UTC()
		if err = e.save("private", databasePostgreSQLMaintenanceIntentID(instance, kind), intent); err != nil {
			return nil, failure("operation_recovery_required", "PostgreSQL configuration was rolled back but its recovery receipt remains pending", 409)
		}
		return nil, failure("configuration_rejected", "PostgreSQL rejected the typed configuration; the previous file was restored", 409)
	}
	intent.State, intent.UpdatedAt = completion, time.Now().UTC()
	if err = e.save("private", databasePostgreSQLMaintenanceIntentID(instance, kind), intent); err != nil {
		return nil, failure("operation_recovery_required", "PostgreSQL configuration applied but its protected completion receipt could not be saved", 409)
	}
	configRevision := databasePostgreSQLConfigDigest(target)
	if kind == "access" {
		var record managedDatabaseRecord
		if err = e.load("private", managedDatabaseKey(instance.ID, intent.Database), &record); err != nil {
			return nil, failure("operation_recovery_required", "PostgreSQL access rule applied; refresh its owned database receipt", 409)
		}
		return e.listPostgreSQLAccessRules(ctx, instance, record)
	}
	state := "settled"
	if !applyAfter {
		state = "restored"
	}
	return map[string]any{"instance_id": instance.ID, "database": intent.Database, "instance_revision": instance.Revision, "database_revision": intent.DatabaseRevision, "config_revision": configRevision, "enabled": postgresqlSlowlogEnabled(target), "minimum_duration_ms": postgresqlSlowlogDuration(target), "configuration_state": state, "reload_applied": true}, nil
}

func (e *PrivilegedExecutor) restorePostgreSQLMaintenanceBefore(ctx context.Context, instance Instance, intent databasePostgreSQLFileIntent) error {
	base, name := instance.Paths.DataDir, "pg_hba.conf"
	mode, uid, gid := os.FileMode(intent.BeforeMode), intent.BeforeUID, intent.BeforeGID
	if intent.Kind == "slowlog" {
		base, name = instance.Paths.ConfigDir, "postgresql.conf"
		mode, uid, gid = 0640, 0, int(e.gid)
	}
	if err := writePostgreSQLManagedFile(base, name, intent.Before, mode, uid, gid); err != nil {
		return err
	}
	return e.verifyPostgreSQLMaintenanceRuntime(ctx, instance, intent, intent.Before, false)
}

func (e *PrivilegedExecutor) verifyPostgreSQLMaintenanceRuntime(ctx context.Context, instance Instance, intent databasePostgreSQLFileIntent, target []byte, applyAfter bool) error {
	if intent.Kind == "slowlog" {
		if err := validatePostgreSQLServerConfig(ctx, instance, e.credential(instance)); err != nil {
			return err
		}
	}
	db, err := e.openManagedAdmin(ctx, instance)
	if err != nil {
		return err
	}
	defer db.Close()
	if intent.Kind == "access" {
		if err = validatePostgreSQLHBAFile(ctx, db); err != nil {
			return err
		}
	} else if err = verifyPostgreSQLSlowlogSettings(ctx, db, target); err != nil {
		return err
	}
	if err = reloadPostgreSQLConfig(ctx, db); err != nil {
		return err
	}
	if intent.Kind == "access" {
		return validatePostgreSQLHBAFile(ctx, db)
	}
	return verifyPostgreSQLSlowlogSettings(ctx, db, target)
}

func (e *PrivilegedExecutor) recoverPostgreSQLIntentDirect(ctx context.Context, instance Instance, kind string) error {
	intent, err := e.loadPostgreSQLMaintenanceIntent(instance, kind)
	if err != nil || intent == nil {
		return err
	}
	if intent.State != "pending" {
		return nil
	}
	if !bytes.Equal(intent.Before, intent.After) {
		return failure("operation_recovery_required", "PostgreSQL configuration requires an explicit user recovery choice", 409)
	}
	return nil
}

func postgreSQLAccessResourceList(instanceID, database string) []string {
	return []string{InstanceResource(instanceID), DatabaseResource(instanceID, database)}
}

func postgreSQLMaintenanceResources(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var in DatabasePostgreSQLMaintenanceInput
	if err := parse(raw, &in); err != nil {
		return nil, err
	}
	if !validInstanceID(in.InstanceID) || !validDatabaseName(in.Database) {
		return nil, failure("invalid_input", "A managed PostgreSQL instance and database are required", 400)
	}
	resources := postgreSQLAccessResourceList(in.InstanceID, in.Database)
	if strings.HasPrefix(in.Command, "slowlog_") {
		resources = append(resources, DatabaseResource(in.InstanceID, ""))
	}
	return resources, nil
}

func postgreSQLMaintenanceActionResources(command string) func(context.Context, core.Actor, json.RawMessage) ([]string, error) {
	return func(ctx context.Context, actor core.Actor, raw json.RawMessage) ([]string, error) {
		var in DatabasePostgreSQLMaintenanceInput
		if err := parse(raw, &in); err != nil {
			return nil, err
		}
		in.Command = command
		forward, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		return postgreSQLMaintenanceResources(ctx, actor, forward)
	}
}

func (m *Manager) postgresqlMaintenanceBindings() []core.Binding {
	id, database := idSchema(), map[string]any{"type": "string", "pattern": databaseIdentifier.String()}
	revision := map[string]any{"type": "integer", "minimum": 1}
	digest := map[string]any{"type": "string", "pattern": databasePostgreSQLConfigRevisionPattern.String()}
	base := func() map[string]any { return map[string]any{"instance_id": id, "database": database} }
	type spec struct {
		name, command, scope, description string
		readOnly, task                    bool
		fields                            map[string]any
		required                          []string
	}
	specs := []spec{
		{"managed_postgresql_access_rules_list", "access_list", "databases:read", "List typed access rules for one exact managed PostgreSQL database", true, false, base(), []string{"instance_id", "database"}},
		{"managed_postgresql_access_rule_add", "access_add", "databases:admin", "Grant a managed PostgreSQL role access from a loopback CIDR using SCRAM-SHA-256", false, true, map[string]any{"instance_id": id, "database": database, "cidr": map[string]any{"type": "string", "maxLength": 64, "description": "Canonical loopback CIDR only; public database binds require a TLS profile"}, "auth_method": enumSchema("scram-sha-256"), "expected_config_revision": digest, "expected_instance_revision": revision, "expected_database_revision": revision}, []string{"instance_id", "database", "cidr", "auth_method", "expected_config_revision", "expected_instance_revision", "expected_database_revision"}},
		{"managed_postgresql_access_rule_remove", "access_remove", "databases:admin", "Block only the exact managed PostgreSQL database role and CIDR rule", false, true, map[string]any{"instance_id": id, "database": database, "rule_id": map[string]any{"type": "string", "pattern": databasePostgreSQLRuleIDPattern.String()}, "expected_config_revision": digest, "expected_instance_revision": revision, "expected_database_revision": revision}, []string{"instance_id", "database", "rule_id", "expected_config_revision", "expected_instance_revision", "expected_database_revision"}},
		{"managed_postgresql_access_recover", "access_recover", "databases:admin", "Explicitly apply or restore a journaled PostgreSQL access-rule change", false, true, map[string]any{"instance_id": id, "database": database, "recovery_choice": enumSchema("apply_pending", "restore_previous"), "expected_config_revision": digest, "expected_instance_revision": revision, "expected_database_revision": revision}, []string{"instance_id", "database", "recovery_choice", "expected_config_revision", "expected_instance_revision", "expected_database_revision"}},
		{"managed_postgresql_slowlog_status", "slowlog_status", "databases:read", "Read actual managed PostgreSQL slow-query logging settings and revisions", true, false, base(), []string{"instance_id", "database"}},
		{"managed_postgresql_slowlog_configure", "slowlog_configure", "databases:admin", "Configure a bounded PostgreSQL slow-query threshold and reload its owned runtime", false, true, map[string]any{"instance_id": id, "database": database, "enabled": map[string]any{"type": "boolean"}, "minimum_duration_ms": map[string]any{"type": "integer", "minimum": 1, "maximum": 600000}, "expected_config_revision": digest, "expected_instance_revision": revision, "expected_database_revision": revision}, []string{"instance_id", "database", "enabled", "minimum_duration_ms", "expected_config_revision", "expected_instance_revision", "expected_database_revision"}},
		{"managed_postgresql_slowlog_recover", "slowlog_recover", "databases:admin", "Explicitly apply or restore a journaled PostgreSQL slow-query configuration", false, true, map[string]any{"instance_id": id, "database": database, "recovery_choice": enumSchema("apply_pending", "restore_previous"), "expected_config_revision": digest, "expected_instance_revision": revision, "expected_database_revision": revision}, []string{"instance_id", "database", "recovery_choice", "expected_config_revision", "expected_instance_revision", "expected_database_revision"}},
		{"managed_postgresql_slowlog_read", "slowlog_read", "databases:read", "Read a bounded redacted window from the exact managed PostgreSQL slow log", true, false, map[string]any{"instance_id": id, "database": database, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 200}}, []string{"instance_id", "database"}},
	}
	bindings := make([]core.Binding, 0, len(specs))
	for _, spec := range specs {
		spec := spec
		binding := m.binding(spec.name, spec.description, spec.scope, spec.readOnly, obj(spec.fields, spec.required...), postgreSQLMaintenanceActionResources(spec.command), func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			var in DatabasePostgreSQLMaintenanceInput
			if err := parse(raw, &in); err != nil {
				return core.ActionResult{}, err
			}
			in.Command = spec.command
			if spec.command == "slowlog_read" && in.Limit == 0 {
				in.Limit = 50
			}
			forward, err := json.Marshal(in)
			if err != nil {
				return core.ActionResult{}, err
			}
			resources, err := postgreSQLMaintenanceResources(ctx, actor, forward)
			if err != nil {
				return core.ActionResult{}, err
			}
			if spec.task {
				job, submitErr := m.deps.Tasks.Submit(ctx, actor, databasePostgreSQLMaintenanceJobOperation, forward, resources)
				if submitErr != nil {
					return core.ActionResult{}, submitErr
				}
				taskID, _ := job["id"].(string)
				if taskID == "" {
					taskID, _ = job["task_id"].(string)
				}
				return core.ActionResult{Data: job, TaskID: taskID, Resources: resources, AuditSummary: map[string]any{"instance_id": in.InstanceID, "database": in.Database, "command": spec.command, "rule_id": in.RuleID, "cidr": in.CIDR, "enabled": in.Enabled, "minimum_duration_ms": in.MinimumDurationMS}}, nil
			}
			var output any
			if err = m.agentJSON(ctx, databasePostgreSQLMaintenanceAgentOperation, forward, &output); err != nil {
				return core.ActionResult{}, err
			}
			return core.ActionResult{Data: output, Resources: resources, AuditSummary: map[string]any{"instance_id": in.InstanceID, "database": in.Database, "command": spec.command}}, nil
		})
		binding.Action.Task = spec.task
		bindings = append(bindings, binding)
	}
	return bindings
}

func PostgreSQLMaintenanceJobOperations() []string {
	return []string{databasePostgreSQLMaintenanceJobOperation}
}

// PostgreSQLMaintenanceExecutor rechecks the task owner's current grants before
// forwarding a typed maintenance request to the root-owned Agent.
type PostgreSQLMaintenanceExecutor struct {
	manager *Manager
	client  *agent.UnixClient
}

func NewPostgreSQLMaintenanceExecutor(manager *Manager, client *agent.UnixClient) *PostgreSQLMaintenanceExecutor {
	return &PostgreSQLMaintenanceExecutor{manager: manager, client: client}
}

func (e *PostgreSQLMaintenanceExecutor) Validate(request agent.Request) error {
	if request.Operation != databasePostgreSQLMaintenanceJobOperation {
		return agent.ErrUnauthorized
	}
	if e.manager == nil || e.client == nil || e.manager.deps.ResolveActor == nil || e.manager.deps.Authorize == nil {
		return failure("agent_unavailable", "PostgreSQL maintenance requires current actor authorization and the protected Unix Agent", 503)
	}
	var in DatabasePostgreSQLMaintenanceInput
	if err := parse(request.Input, &in); err != nil {
		return err
	}
	if in.Command != "access_add" && in.Command != "access_remove" && in.Command != "access_recover" && in.Command != "slowlog_configure" && in.Command != "slowlog_recover" {
		return failure("invalid_input", "Only typed PostgreSQL mutations may enter the durable task queue", 400)
	}
	return validatePostgreSQLMaintenanceInputDirect(in)
}

func validatePostgreSQLMaintenanceInputDirect(in DatabasePostgreSQLMaintenanceInput) error {
	if !validInstanceID(in.InstanceID) || !validDatabaseName(in.Database) || !validPostgreSQLCAS(in) {
		return failure("invalid_input", "PostgreSQL task identity or revision is invalid", 400)
	}
	switch in.Command {
	case "access_add":
		if !validPostgreSQLLoopbackCIDR(in.CIDR) || in.AuthMethod != "scram-sha-256" || in.RuleID != "" || in.RecoveryChoice != "" || in.Enabled || in.MinimumDurationMS != 0 || in.Limit != 0 {
			return failure("invalid_input", "PostgreSQL access task is outside its typed bounds", 400)
		}
	case "access_remove":
		if !databasePostgreSQLRuleIDPattern.MatchString(in.RuleID) || in.CIDR != "" || in.AuthMethod != "" || in.RecoveryChoice != "" || in.Enabled || in.MinimumDurationMS != 0 || in.Limit != 0 {
			return failure("invalid_input", "PostgreSQL access removal task is outside its typed bounds", 400)
		}
	case "access_recover", "slowlog_recover":
		if (in.RecoveryChoice != "apply_pending" && in.RecoveryChoice != "restore_previous") || in.CIDR != "" || in.RuleID != "" || in.AuthMethod != "" || in.Enabled || in.MinimumDurationMS != 0 || in.Limit != 0 {
			return failure("invalid_input", "PostgreSQL recovery task is outside its typed bounds", 400)
		}
	case "slowlog_configure":
		if in.MinimumDurationMS < 1 || in.MinimumDurationMS > 600000 || in.CIDR != "" || in.RuleID != "" || in.AuthMethod != "" || in.RecoveryChoice != "" || in.Limit != 0 {
			return failure("invalid_input", "PostgreSQL slow-log task is outside its typed bounds", 400)
		}
	default:
		return failure("invalid_input", "Unsupported PostgreSQL task command", 400)
	}
	return nil
}

func (e *PostgreSQLMaintenanceExecutor) Run(ctx context.Context, request agent.Request) (result agent.Result, returned error) {
	result = agent.Result{Operation: request.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	defer func() { result.FinishedAt = time.Now().UTC() }()
	if err := e.Validate(request); err != nil {
		return result, err
	}
	actor, err := e.manager.deps.ResolveActor(ctx, core.Actor{ID: request.ActorID})
	if err != nil {
		return result, err
	}
	var in DatabasePostgreSQLMaintenanceInput
	if err = parse(request.Input, &in); err != nil {
		return result, err
	}
	resources, err := postgreSQLMaintenanceResources(ctx, actor, request.Input)
	if err != nil {
		return result, err
	}
	for _, resource := range resources {
		resourceScope := "databases:admin"
		if strings.HasPrefix(resource, "software:") {
			resourceScope = "software:manage"
		}
		if !e.manager.deps.Authorize(actor, resourceScope, resource) {
			return result, failure("forbidden", "PostgreSQL maintenance grants were revoked before task execution", 403)
		}
	}
	forward, _ := json.Marshal(in)
	reply, err := e.client.Run(ctx, agent.Request{ActorID: request.ActorID, JobID: request.JobID, Operation: databasePostgreSQLMaintenanceAgentOperation, Input: forward})
	if err != nil || reply.ExitCode != 0 {
		return result, failure("postgresql_maintenance_failed", "Protected PostgreSQL maintenance failed; inspect current status and recovery state", 502)
	}
	if len(reply.Stdout) > 1<<20 || !json.Valid([]byte(reply.Stdout)) {
		return result, failure("agent_protocol_error", "PostgreSQL maintenance Agent returned an invalid result", 502)
	}
	result.Stdout, result.ExitCode = reply.Stdout, 0
	return result, nil
}
