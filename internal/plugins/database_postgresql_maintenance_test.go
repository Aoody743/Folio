package plugins

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestPostgreSQLSlowlogRejectsFIFOWithoutBlocking(t *testing.T) {
	logDir := t.TempDir()
	path := filepath.Join(logDir, "postgresql.log")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, _, err := readPostgreSQLSlowlogFile(Instance{Paths: Paths{LogDir: logDir}}, os.Geteuid())
		done <- err
	}()
	select {
	case err := <-done:
		if core.ErrorCode(err) != "log_unavailable" {
			t.Fatalf("FIFO was not rejected: %v", err)
		}
	case <-time.After(2 * time.Second):
		// Release a broken blocking open so the regression cannot strand a
		// goroutine or the remainder of the test suite.
		fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = syscall.Close(fd)
		}
		t.Fatal("worker-owned FIFO blocked the privileged slow-log reader")
	}
}

type postgresqlMaintenanceFixture struct {
	mu        sync.Mutex
	hbaErrors int64
	reloadOK  bool
	settings  map[string]string
	queries   []string
	without   string
}

type postgresqlMaintenanceFixtureDriver struct{ fixture *postgresqlMaintenanceFixture }
type postgresqlMaintenanceFixtureConn struct{ fixture *postgresqlMaintenanceFixture }
type postgresqlMaintenanceFixtureRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

var postgresqlMaintenanceFixtureID atomic.Uint64

func (d postgresqlMaintenanceFixtureDriver) Open(string) (driver.Conn, error) {
	return &postgresqlMaintenanceFixtureConn{fixture: d.fixture}, nil
}
func (c *postgresqlMaintenanceFixtureConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not used by the PostgreSQL maintenance fixture")
}
func (c *postgresqlMaintenanceFixtureConn) Close() error { return nil }
func (c *postgresqlMaintenanceFixtureConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not used by the PostgreSQL maintenance fixture")
}
func (c *postgresqlMaintenanceFixtureConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.fixture.mu.Lock()
	defer c.fixture.mu.Unlock()
	c.fixture.queries = append(c.fixture.queries, query)
	switch query {
	case "SELECT count(*) FROM pg_hba_file_rules WHERE error IS NOT NULL":
		return &postgresqlMaintenanceFixtureRows{columns: []string{"count"}, values: [][]driver.Value{{c.fixture.hbaErrors}}}, nil
	case "SELECT pg_reload_conf()":
		return &postgresqlMaintenanceFixtureRows{columns: []string{"pg_reload_conf"}, values: [][]driver.Value{{c.fixture.reloadOK}}}, nil
	case "SELECT name,setting,source FROM pg_settings WHERE name IN ('log_min_duration_statement','log_parameter_max_length','log_statement','log_line_prefix')":
		rows := [][]driver.Value{}
		for _, name := range []string{"log_min_duration_statement", "log_parameter_max_length", "log_statement", "log_line_prefix"} {
			if name == c.fixture.without {
				continue
			}
			if value, ok := c.fixture.settings[name]; ok {
				rows = append(rows, []driver.Value{name, value, "configuration file"})
			}
		}
		return &postgresqlMaintenanceFixtureRows{columns: []string{"name", "setting", "source"}, values: rows}, nil
	default:
		return nil, fmt.Errorf("unexpected PostgreSQL maintenance fixture query %q", query)
	}
}
func (r *postgresqlMaintenanceFixtureRows) Columns() []string { return r.columns }
func (r *postgresqlMaintenanceFixtureRows) Close() error      { return nil }
func (r *postgresqlMaintenanceFixtureRows) Next(target []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(target, r.values[r.index])
	r.index++
	return nil
}

func openPostgreSQLMaintenanceFixture(t *testing.T, fixture *postgresqlMaintenanceFixture) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("powerlinux-postgresql-maintenance-%d", postgresqlMaintenanceFixtureID.Add(1))
	sql.Register(name, postgresqlMaintenanceFixtureDriver{fixture: fixture})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal("could not open PostgreSQL maintenance fixture", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestPostgreSQLAccessRuleProtocolPreservesLocalAuthAndOwnsOnlyItsMarker(t *testing.T) {
	instanceID := "postgresql-access-fixture"
	base := []byte("# PostgreSQL-generated header\nlocal all all peer\nhost all all 127.0.0.1/32 scram-sha-256\n")
	cidr := "127.0.0.1/32"
	rule := DatabasePostgreSQLAccessRule{RuleID: databasePostgreSQLAccessRuleID(instanceID, "app_db", "app_user", cidr), Database: "app_db", Role: "app_user", CIDR: cidr, AuthMethod: "scram-sha-256", State: "allowed"}
	after := renderPostgreSQLHBAAccess(base, "postgres", []DatabasePostgreSQLAccessRule{rule})
	exactLine := "host \"app_db\" \"app_user\" 127.0.0.1/32 scram-sha-256"
	if !strings.HasSuffix(string(after), string(base)) || !strings.Contains(string(after), "host all postgres 127.0.0.0/8 scram-sha-256") || strings.Index(string(after), exactLine) < 0 || strings.Index(string(after), exactLine) > strings.Index(string(after), "host all all 127.0.0.1/32 scram-sha-256") {
		t.Fatal("managed block must preserve local administrator auth and put its exact rule before existing broader host rules", string(after))
	}
	parsed, managed, err := parsePostgreSQLHBAAccess(after, instanceID, "postgres")
	if err != nil || !managed || len(parsed) != 1 || parsed[0] != rule {
		t.Fatal("typed PostgreSQL access rule did not round-trip", err, managed, parsed)
	}
	blocked := rule
	blocked.State, blocked.AuthMethod = "blocked", "reject"
	blockedBody := renderPostgreSQLHBAAccess(after, "postgres", []DatabasePostgreSQLAccessRule{blocked})
	parsed, managed, err = parsePostgreSQLHBAAccess(blockedBody, instanceID, "postgres")
	if err != nil || !managed || len(parsed) != 1 || parsed[0].State != "blocked" || parsed[0].AuthMethod != "reject" {
		t.Fatal("rule removal must persist a specific reject rule", err, parsed)
	}
	if _, _, err = parsePostgreSQLHBAAccess([]byte(strings.Replace(string(after), "scram-sha-256 # ", "trust # ", 1)), instanceID, "postgres"); core.ErrorCode(err) != "configuration_unmanaged" {
		t.Fatal("caller-controlled trust authentication entered the managed block", core.ErrorCode(err))
	}
	if _, _, err = parsePostgreSQLHBAAccess([]byte(strings.Replace(string(after), "# admin-role=postgres", "# admin-role=other", 1)), instanceID, "postgres"); core.ErrorCode(err) != "configuration_unmanaged" {
		t.Fatal("protected administrator authentication was silently changed", core.ErrorCode(err))
	}
	special := rule
	special.Database, special.Role = "all", "samerole"
	special.RuleID = databasePostgreSQLAccessRuleID(instanceID, special.Database, special.Role, special.CIDR)
	specialBody := renderPostgreSQLHBAAccess(base, "postgres", []DatabasePostgreSQLAccessRule{special})
	if !strings.Contains(string(specialBody), "host \"all\" \"samerole\" 127.0.0.1/32 scram-sha-256") {
		t.Fatal("HBA special words must be emitted as literal quoted database and role names", string(specialBody))
	}
	if _, _, err = parsePostgreSQLHBAAccess(specialBody, instanceID, "postgres"); err != nil {
		t.Fatal("quoted HBA special names did not round-trip", err)
	}
}

func TestPostgreSQLAccessCIDRRequiresCanonicalLoopbackPrefix(t *testing.T) {
	for _, test := range []struct {
		cidr string
		want bool
	}{
		{"127.0.0.1/32", true},
		{"127.0.0.0/8", true},
		{"::1/128", true},
		{"127.0.0.1/24", false},
		{"0.0.0.0/0", false},
		{"10.0.0.0/8", false},
		{"::/0", false},
		{"192.0.2.1", false},
	} {
		if got := validPostgreSQLLoopbackCIDR(test.cidr); got != test.want {
			t.Errorf("validPostgreSQLLoopbackCIDR(%q) = %t, want %t", test.cidr, got, test.want)
		}
	}
}

func TestPostgreSQLMaintenanceResourceScopeIncludesInstanceAndServerwideLogs(t *testing.T) {
	for _, test := range []struct {
		command string
		want    []string
	}{
		{"access_add", []string{InstanceResource("pg-one"), DatabaseResource("pg-one", "app_db")}},
		{"slowlog_read", []string{InstanceResource("pg-one"), DatabaseResource("pg-one", "app_db"), DatabaseResource("pg-one", "")}},
		{"slowlog_configure", []string{InstanceResource("pg-one"), DatabaseResource("pg-one", "app_db"), DatabaseResource("pg-one", "")}},
	} {
		raw, err := json.Marshal(DatabasePostgreSQLMaintenanceInput{InstanceID: "pg-one", Database: "app_db", Command: test.command})
		if err != nil {
			t.Fatal("could not encode resource fixture", err)
		}
		got, err := postgreSQLMaintenanceResources(context.Background(), core.Actor{}, raw)
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%s resources = %#v, %v; want %#v", test.command, got, err, test.want)
		}
	}
	resolver := postgreSQLMaintenanceActionResources("slowlog_read")
	raw := json.RawMessage(`{"instance_id":"pg-one","database":"app_db"}`)
	got, err := resolver(context.Background(), core.Actor{}, raw)
	if err != nil || !reflect.DeepEqual(got, []string{InstanceResource("pg-one"), DatabaseResource("pg-one", "app_db"), DatabaseResource("pg-one", "")}) {
		t.Fatalf("public slowlog binding did not declare all serverwide resources: %#v %v", got, err)
	}
}

func TestPostgreSQLRecoveryContextIsBoundToOneExactPendingJournal(t *testing.T) {
	executor := &PrivilegedExecutor{}
	intent := databasePostgreSQLFileIntent{Kind: "access", State: "pending", ExpectedRevision: strings.Repeat("a", 64), After: []byte("exact managed target"), CreatedAt: time.Now().UTC()}
	ctx := databasePostgreSQLIntentContext(context.Background(), executor, "pg-one", intent)
	recovery := ctx.Value(databasePostgreSQLAccessRecoveryContextKey{}).(databasePostgreSQLAccessRecoveryContext)
	if !matchesPostgreSQLMaintenanceRecoveryContext(recovery, executor, "pg-one", intent) {
		t.Fatal("exact recovery journal was not recognized")
	}
	changedTarget := intent
	changedTarget.After = []byte("different target")
	if matchesPostgreSQLMaintenanceRecoveryContext(recovery, executor, "pg-one", changedTarget) {
		t.Fatal("recovery context bypassed a different journal target")
	}
	otherInstance := intent
	if matchesPostgreSQLMaintenanceRecoveryContext(recovery, executor, "pg-two", otherInstance) {
		t.Fatal("recovery context bypassed another instance")
	}
	completed := intent
	completed.State = "completed"
	if matchesPostgreSQLMaintenanceRecoveryContext(recovery, executor, "pg-one", completed) {
		t.Fatal("recovery context bypassed a non-pending journal")
	}
}

func TestPostgreSQLHBAParseAndReloadFixtureReportsServerErrors(t *testing.T) {
	fixture := &postgresqlMaintenanceFixture{reloadOK: true}
	db := openPostgreSQLMaintenanceFixture(t, fixture)
	if err := validatePostgreSQLHBAFile(context.Background(), db); err != nil {
		t.Fatal("valid pg_hba_file_rules fixture was rejected", err)
	}
	if err := reloadPostgreSQLConfig(context.Background(), db); err != nil {
		t.Fatal("PostgreSQL reload fixture did not report its acknowledged reload", err)
	}
	fixture.mu.Lock()
	fixture.hbaErrors = 1
	fixture.mu.Unlock()
	if err := validatePostgreSQLHBAFile(context.Background(), db); core.ErrorCode(err) != "configuration_invalid" {
		t.Fatal("server-side HBA parse errors did not reject the staged file", core.ErrorCode(err))
	}
	fixture.mu.Lock()
	fixture.reloadOK = false
	fixture.mu.Unlock()
	if err := reloadPostgreSQLConfig(context.Background(), db); core.ErrorCode(err) != "reload_not_applied" {
		t.Fatal("a false pg_reload_conf response was reported as applied", core.ErrorCode(err))
	}
}

func TestPostgreSQLSlowlogBlockUsesTypedThresholdAndPreservesAcrossRegeneration(t *testing.T) {
	base := []byte("data_directory = '/srv/db/data'\nlogging_collector = on\nlog_directory = '/srv/db/logs'\nlog_filename = 'postgresql.log'\n")
	configured, err := makePostgreSQLSlowlogConfig(base, true, 750)
	if err != nil {
		t.Fatal("typed slow-log configuration failed", err)
	}
	settings, managed, err := parsePostgreSQLSlowlogBlock(configured)
	if err != nil || !managed || !settings.Enabled || settings.MinimumDurationMS != 750 {
		t.Fatal("enabled slow-log block did not preserve its threshold", err, settings)
	}
	disabled, err := makePostgreSQLSlowlogConfig(configured, false, 1250)
	if err != nil {
		t.Fatal("typed slow-log disable failed", err)
	}
	settings, managed, err = parsePostgreSQLSlowlogBlock(disabled)
	if err != nil || !managed || settings.Enabled || settings.MinimumDurationMS != 1250 {
		t.Fatal("disabled slow-log block did not preserve the next selected threshold", err, settings)
	}
	regenerated, err := preservePostgreSQLMaintenanceRuntimeConfig([]byte("data_directory='/srv/new/data'\nlogging_collector=on\n"), disabled)
	if err != nil {
		t.Fatal("managed slow-log block was not preserved across generated config replacement", err)
	}
	settings, managed, err = parsePostgreSQLSlowlogBlock(regenerated)
	if err != nil || !managed || settings.Enabled || settings.MinimumDurationMS != 1250 || !strings.Contains(string(regenerated), "data_directory='/srv/new/data'") {
		t.Fatal("generated config did not retain typed settings and new base fields", err, settings)
	}
	if _, err = makePostgreSQLSlowlogConfig(append(base, []byte("log_statement = 'all'\n")...), true, 750); core.ErrorCode(err) != "configuration_unmanaged" {
		t.Fatal("unowned statement logging directive was overwritten", core.ErrorCode(err))
	}
	if _, err = makePostgreSQLSlowlogConfig(base, true, 0); core.ErrorCode(err) != "invalid_input" {
		t.Fatal("zero threshold bypassed the typed duration bound", core.ErrorCode(err))
	}
}

func TestPostgreSQLSlowlogSettingsFixtureVerifiesVersionAndReloadValues(t *testing.T) {
	fixture := &postgresqlMaintenanceFixture{settings: map[string]string{
		"log_min_duration_statement": "250",
		"log_parameter_max_length":   "0",
		"log_statement":              "none",
		"log_line_prefix":            "%m [%p] %u@%d ",
	}}
	db := openPostgreSQLMaintenanceFixture(t, fixture)
	settings, err := queryPostgreSQLSlowlogSettings(context.Background(), db)
	if err != nil {
		t.Fatal("supported PostgreSQL settings fixture was rejected", err)
	}
	expected := []byte(databasePostgreSQLSlowlogBegin + "\n# threshold-ms=250\nlog_min_duration_statement = 250\nlog_parameter_max_length = 0\nlog_statement = 'none'\nlog_line_prefix = '%m [%p] %u@%d '\n" + databasePostgreSQLSlowlogEnd + "\n")
	if err = verifyPostgreSQLSlowlogSettingMap(settings, postgresqlSlowlogConfig{Enabled: true, MinimumDurationMS: 250}); err != nil {
		t.Fatal("live settings did not match the managed values", err)
	}
	if err = verifyPostgreSQLSlowlogSettings(context.Background(), db, expected); err != nil {
		t.Fatal("effective PostgreSQL settings failed reload verification", err)
	}
	fixture.mu.Lock()
	fixture.settings["log_min_duration_statement"] = "500"
	fixture.without = ""
	fixture.mu.Unlock()
	settings, err = queryPostgreSQLSlowlogSettings(context.Background(), db)
	if err != nil {
		t.Fatal("settings refresh failed", err)
	}
	if err = verifyPostgreSQLSlowlogSettingMap(settings, postgresqlSlowlogConfig{Enabled: true, MinimumDurationMS: 250}); core.ErrorCode(err) != "reload_not_applied" {
		t.Fatal("reload verification accepted a stale live threshold", core.ErrorCode(err))
	}
	fixture.mu.Lock()
	fixture.without = "log_parameter_max_length"
	fixture.mu.Unlock()
	if _, err = queryPostgreSQLSlowlogSettings(context.Background(), db); core.ErrorCode(err) != "slowlog_unsupported" {
		t.Fatal("server without the parameter-redaction setting was treated as safe", core.ErrorCode(err))
	}
}

func TestPostgreSQLSlowlogReadIsFixedPathBoundedAndRedactsStatements(t *testing.T) {
	logDir := t.TempDir()
	log := "2026-10-15 11:12:12.123 UTC [41] other_user@other_db LOG:  duration: 999.00 ms  statement: SELECT 'other database secret';\n" +
		"2026-10-15 11:12:13.123 UTC [42] app_user@app_db LOG:  duration: 124.50 ms  statement: SELECT * FROM users WHERE id = 424242 AND email = 'person@example.invalid' AND password='secret-value';\n"
	if err := os.WriteFile(filepath.Join(logDir, "postgresql.log"), []byte(log), 0600); err != nil {
		t.Fatal("could not prepare protected slow-log fixture", err)
	}
	data, available, truncated, err := readPostgreSQLSlowlogFile(Instance{Paths: Paths{LogDir: logDir}}, os.Geteuid())
	if err != nil || !available || truncated {
		t.Fatal("fixed owned slow-log fixture was not read", err, available, truncated)
	}
	var truncatedEntries bool
	entries := parsePostgreSQLSlowlogEntries(data, "app_db", 5, &truncatedEntries)
	if len(entries) != 1 || entries[0].DurationMS != 124.5 || entries[0].Role != "app_user" || entries[0].Database != "app_db" || !strings.Contains(entries[0].Statement, "[REDACTED]") {
		t.Fatal("slow-log entry was not decoded into bounded typed fields", entries)
	}
	if strings.Contains(entries[0].Statement, "person@example.invalid") || strings.Contains(entries[0].Statement, "secret-value") || strings.Contains(entries[0].Statement, "424242") {
		t.Fatal("literal secret material escaped slow-log redaction", entries[0].Statement)
	}
	longLine := "2026-10-15 11:12:14.123 UTC [43] app_user@app_db LOG:  duration: 12 ms  statement: SELECT $tag$cutoff-secret" + strings.Repeat("private-token-", 1800) + "$tag$\n"
	var longLineTruncated bool
	longEntries := parsePostgreSQLSlowlogEntries([]byte(longLine), "app_db", 5, &longLineTruncated)
	if len(longEntries) != 1 || !longLineTruncated || !strings.Contains(longEntries[0].Statement, "[REDACTED]") || strings.Contains(longEntries[0].Statement, "cutoff-secret") || strings.Contains(longEntries[0].Statement, "private-token") {
		t.Fatal("a dollar-quoted value cut by the 16 KiB log-line bound escaped redaction", longLineTruncated, longEntries)
	}
	inline, _ := redactPostgreSQLStatement("SELECT $tag$inline-cutoff-secret" + strings.Repeat("x", databasePostgreSQLSlowlogMaxStmt*2))
	if strings.Contains(inline, "inline-cutoff-secret") || !strings.Contains(inline, "[REDACTED]") {
		t.Fatal("an unclosed inline dollar quote escaped redaction", inline)
	}
	if err = os.Remove(filepath.Join(logDir, "postgresql.log")); err != nil {
		t.Fatal("could not remove regular slow-log fixture", err)
	}
	if err = os.Symlink("/etc/passwd", filepath.Join(logDir, "postgresql.log")); err != nil {
		t.Fatal("could not create symlink refusal fixture", err)
	}
	if _, _, _, err = readPostgreSQLSlowlogFile(Instance{Paths: Paths{LogDir: logDir}}, os.Geteuid()); core.ErrorCode(err) != "log_unavailable" {
		t.Fatal("slow-log reader followed a caller-controlled symlink", core.ErrorCode(err))
	}
}
