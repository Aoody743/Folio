package plugins

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

type databaseBinlogFixture struct {
	variables  [][]driver.Value
	masterRows [][]driver.Value
	logRows    [][]driver.Value
	queries    []string
	masterName string
}

type databaseBinlogFixtureDriver struct{ fixture *databaseBinlogFixture }
type databaseBinlogFixtureConn struct{ fixture *databaseBinlogFixture }
type databaseBinlogFixtureRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

var databaseBinlogFixtureID atomic.Uint64

func (d databaseBinlogFixtureDriver) Open(string) (driver.Conn, error) {
	return &databaseBinlogFixtureConn{fixture: d.fixture}, nil
}
func (c *databaseBinlogFixtureConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not used by the SHOW protocol fixture")
}
func (c *databaseBinlogFixtureConn) Close() error { return nil }
func (c *databaseBinlogFixtureConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not used by the SHOW protocol fixture")
}
func (c *databaseBinlogFixtureConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.fixture.queries = append(c.fixture.queries, query)
	switch query {
	case "SHOW GLOBAL VARIABLES WHERE Variable_name IN ('log_bin','log_bin_basename','binlog_format','binlog_expire_logs_seconds','expire_logs_days','server_id')":
		return &databaseBinlogFixtureRows{columns: []string{"Variable_name", "Value"}, values: c.fixture.variables}, nil
	case "SHOW MASTER STATUS":
		if c.fixture.masterName == "SHOW BINARY LOG STATUS" {
			return nil, errors.New("SHOW MASTER STATUS is unavailable in this server version")
		}
		return &databaseBinlogFixtureRows{columns: []string{"File", "Position", "Binlog_Do_DB"}, values: c.fixture.masterRows}, nil
	case "SHOW BINARY LOG STATUS":
		if c.fixture.masterName != "SHOW BINARY LOG STATUS" {
			return nil, errors.New("SHOW BINARY LOG STATUS is unavailable in this server version")
		}
		return &databaseBinlogFixtureRows{columns: []string{"File", "Position"}, values: c.fixture.masterRows}, nil
	case "SHOW BINARY LOGS":
		return &databaseBinlogFixtureRows{columns: []string{"Log_name", "File_size", "Encrypted"}, values: c.fixture.logRows}, nil
	default:
		return nil, fmt.Errorf("unexpected SQL protocol request %q", query)
	}
}
func (r *databaseBinlogFixtureRows) Columns() []string { return r.columns }
func (r *databaseBinlogFixtureRows) Close() error      { return nil }
func (r *databaseBinlogFixtureRows) Next(target []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(target, r.values[r.index])
	r.index++
	return nil
}

func openDatabaseBinlogFixture(t *testing.T, fixture *databaseBinlogFixture) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("powerlinux-binlog-fixture-%d", databaseBinlogFixtureID.Add(1))
	sql.Register(name, databaseBinlogFixtureDriver{fixture: fixture})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal("could not open SQL protocol fixture", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func fixtureDatabaseBinlog() *databaseBinlogFixture {
	return &databaseBinlogFixture{
		variables: [][]driver.Value{
			{[]byte("log_bin"), []byte("ON")},
			{[]byte("log_bin_basename"), []byte("/srv/managed/mysql-bin")},
			{[]byte("binlog_format"), []byte("MIXED")},
			{[]byte("binlog_expire_logs_seconds"), []byte("604800")},
			{[]byte("server_id"), []byte("41")},
		},
		masterRows: [][]driver.Value{{[]byte("mysql-bin.000003"), []byte("8192"), nil}},
		logRows: [][]driver.Value{
			{[]byte("mysql-bin.000001"), []byte("1024"), []byte("No")},
			{[]byte("mysql-bin.000002"), []byte("2048"), []byte("No")},
			{[]byte("mysql-bin.000003"), []byte("8192"), []byte("No")},
		},
	}
}

func TestDatabaseBinlogMySQLShowProtocolUsesTypedServerRows(t *testing.T) {
	fixture := fixtureDatabaseBinlog()
	db := openDatabaseBinlogFixture(t, fixture)
	instance := Instance{ID: "mysql-binlog-protocol", PluginID: "mysql"}
	status, logs, err := queryDatabaseBinlogProtocol(context.Background(), db, instance)
	if err != nil {
		t.Fatal("MySQL SHOW protocol fixture failed", err)
	}
	if !status.Enabled || status.Format != "MIXED" || status.Basename != "mysql-bin" || status.ActiveLog != "mysql-bin.000003" || status.ServerID != "41" || status.ExpirationSeconds != 604800 {
		t.Fatalf("typed server status did not preserve expected fields: %#v", status)
	}
	if len(logs) != 3 || logs[0].Name != "mysql-bin.000001" || logs[0].SizeBytes != 1024 || logs[2].Name != status.ActiveLog {
		t.Fatalf("typed SHOW BINARY LOGS rows were decoded incorrectly: %#v", logs)
	}
	if !strings.Contains(strings.Join(fixture.queries, "\n"), "SHOW BINARY LOGS") {
		t.Fatal("list did not come from the server binary-log protocol")
	}
}

func TestDatabaseBinlogMariaDBUsesBinaryLogStatusCompatibilityFallback(t *testing.T) {
	fixture := fixtureDatabaseBinlog()
	fixture.masterName = "SHOW BINARY LOG STATUS"
	db := openDatabaseBinlogFixture(t, fixture)
	status, err := readDatabaseBinlogSQL(context.Background(), db, Instance{ID: "mariadb-binlog-protocol", PluginID: "mariadb"})
	if err != nil || status.ActiveLog != "mysql-bin.000003" {
		t.Fatal("MariaDB/current MySQL active binary-log compatibility path failed", err, status.ActiveLog)
	}
	joined := strings.Join(fixture.queries, "\n")
	if !strings.Contains(joined, "SHOW MASTER STATUS") || !strings.Contains(joined, "SHOW BINARY LOG STATUS") {
		t.Fatal("server did not try the compatible active-log status verbs")
	}
}

func TestDatabaseBinlogListPageUsesExactCursorAndBoundedContinuation(t *testing.T) {
	fixture := fixtureDatabaseBinlog()
	db := openDatabaseBinlogFixture(t, fixture)
	first, total, truncated, cursor, err := readDatabaseBinlogListPage(context.Background(), db, 2, "")
	if err != nil || total != 3 || !truncated || cursor != "mysql-bin.000002" || len(first) != 2 || first[0].Name != "mysql-bin.000001" {
		t.Fatal("first binlog page did not return its exact continuation cursor", err, total, truncated, cursor, first)
	}
	second, total, truncated, cursor, err := readDatabaseBinlogListPage(context.Background(), db, 2, cursor)
	if err != nil || total != 3 || truncated || cursor != "" || len(second) != 1 || second[0].Name != "mysql-bin.000003" {
		t.Fatal("continued binlog page did not resume strictly after its cursor", err, total, truncated, cursor, second)
	}
	if _, _, _, _, err = readDatabaseBinlogListPage(context.Background(), db, 2, "mysql-bin.000009"); core.ErrorCode(err) != "stale_page_cursor" {
		t.Fatal("missing cursor did not require a list refresh", core.ErrorCode(err))
	}
}

func TestDatabaseBinlogSnapshotAllowsOnlyActiveFileGrowth(t *testing.T) {
	snapshot := databaseBinlogSnapshot{Logs: []DatabaseBinlogLog{{Name: "mysql-bin.000001", SizeBytes: 10}, {Name: "mysql-bin.000002", SizeBytes: 20}, {Name: "mysql-bin.000003", SizeBytes: 30}}, Candidates: []DatabaseBinlogLog{{Name: "mysql-bin.000001", SizeBytes: 10}}, RetainFrom: "mysql-bin.000002", ActiveLog: "mysql-bin.000003"}
	current := []DatabaseBinlogLog{{Name: "mysql-bin.000001", SizeBytes: 10}, {Name: "mysql-bin.000002", SizeBytes: 20}, {Name: "mysql-bin.000003", SizeBytes: 35}}
	if err := validateDatabaseBinlogSnapshotStillCurrent(snapshot, current, snapshot.ActiveLog); err != nil {
		t.Fatal("normal writes to the preserved active log invalidated its purge prefix", core.ErrorCode(err))
	}
	current[0].SizeBytes++
	if err := validateDatabaseBinlogSnapshotStillCurrent(snapshot, current, snapshot.ActiveLog); core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("historical candidate size change was accepted")
	}
	current = append(current, DatabaseBinlogLog{Name: "mysql-bin.000004", SizeBytes: 1})
	if err := validateDatabaseBinlogSnapshotStillCurrent(snapshot, current, snapshot.ActiveLog); core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("new binary-log rotation after preview was accepted")
	}
}

func TestDatabaseBinlogSnapshotShapeAndRetainAnchorAreFrozen(t *testing.T) {
	now := time.Now().UTC()
	logs := []DatabaseBinlogLog{{Name: "mysql-bin.000001", SizeBytes: 10}, {Name: "mysql-bin.000002", SizeBytes: 20}, {Name: "mysql-bin.000003", SizeBytes: 30}}
	snapshot := databaseBinlogSnapshot{
		Version: 1, InstanceID: "mysql-binlog-preview", Engine: "mysql", PreviewID: strings.Repeat("a", 32),
		RetainFrom: "mysql-bin.000002", ActiveLog: "mysql-bin.000003", Logs: logs,
		Candidates: []DatabaseBinlogLog{logs[0]}, ConfigRevision: strings.Repeat("b", 64), InstanceRevision: 2,
		CreatedAt: now, ExpiresAt: now.Add(databaseBinlogPreviewLifetime),
	}
	if !validDatabaseBinlogSnapshot(snapshot) {
		t.Fatal("ordered concrete pre-anchor snapshot was rejected")
	}
	mutated := snapshot
	mutated.Candidates = []DatabaseBinlogLog{{Name: logs[0].Name, SizeBytes: logs[0].SizeBytes + 1}}
	if validDatabaseBinlogSnapshot(mutated) {
		t.Fatal("snapshot candidates differing from the frozen prefix were accepted")
	}
	mutated = snapshot
	mutated.RetainFrom = "mysql-bin.000001"
	if validDatabaseBinlogSnapshot(mutated) {
		t.Fatal("empty purge prefix was accepted")
	}
	mutated = snapshot
	mutated.ActiveLog = "mysql-bin.000001"
	if validDatabaseBinlogSnapshot(mutated) {
		t.Fatal("retain anchor after the active log was accepted")
	}
	mutated = snapshot
	mutated.ExpiresAt = now.Add(databaseBinlogPreviewLifetime + time.Second)
	if validDatabaseBinlogSnapshot(mutated) {
		t.Fatal("overlong snapshot lifetime was accepted")
	}
}

func TestDatabaseBinlogPreviewAllowsPartialPrefixAndRejectsAnchorAfterActive(t *testing.T) {
	e := testAgent(t)
	instance := Instance{ID: "mysql-binlog-preview", PluginID: "mysql", Revision: 3}
	status := DatabaseBinlogStatus{ActiveLog: "mysql-bin.000003", ConfigRevision: strings.Repeat("c", 64)}
	logs := []DatabaseBinlogLog{{Name: "mysql-bin.000001", SizeBytes: 10}, {Name: "mysql-bin.000002", SizeBytes: 20}, {Name: status.ActiveLog, SizeBytes: 30}}
	preview, err := e.createDatabaseBinlogPurgePreview(instance, status, logs, "mysql-bin.000002")
	if err != nil || len(preview.BinlogsToPurge) != 1 || preview.BinlogsToPurge[0].Name != "mysql-bin.000001" || preview.BytesToPurge != 10 {
		t.Fatal("partial frozen prefix was not represented exactly", err, preview)
	}
	var frozen databaseBinlogSnapshot
	if err = e.load("private", databaseBinlogSnapshotID(instance), &frozen); err != nil || !validDatabaseBinlogSnapshot(frozen) {
		t.Fatal("stored preview was not a valid protected snapshot", err)
	}
	status.ActiveLog = "mysql-bin.000002"
	if _, err = e.createDatabaseBinlogPurgePreview(instance, status, logs, "mysql-bin.000003"); core.ErrorCode(err) != "unsafe_purge_target" {
		t.Fatal("anchor after active log was accepted", core.ErrorCode(err))
	}
}

func TestDatabaseBinlogConfigOnlyChangesRecognizedOwnedBlock(t *testing.T) {
	baseline := []byte("[mysqld]\nbind-address=127.0.0.1\nport=3306\n[client]\nsocket=/run/mysql.sock\n")
	enabled, err := setDatabaseBinlogConfig(baseline, true)
	if err != nil {
		t.Fatal("managed binlog enable block could not be created", err)
	}
	configured, managed, state := parseDatabaseBinlogConfig(enabled)
	if !managed || !configured || state != "settled" || !strings.HasPrefix(string(enabled), string(baseline)+"\n") {
		t.Fatal("enable config did not preserve the existing owned profile", string(enabled))
	}
	disabled, err := setDatabaseBinlogConfig(enabled, false)
	if err != nil {
		t.Fatal("managed binlog disable block could not be created", err)
	}
	configured, managed, state = parseDatabaseBinlogConfig(disabled)
	if !managed || configured || state != "settled" || !strings.HasPrefix(string(disabled), string(baseline)+"\n") {
		t.Fatal("disable config did not preserve the existing owned profile", string(disabled))
	}
	if _, err = setDatabaseBinlogConfig([]byte("[mysqld]\nlog-bin=external-bin\n"), false); core.ErrorCode(err) != "configuration_unmanaged" {
		t.Fatal("unmanaged log-bin directive was overwritten")
	}
	tampered := append([]byte(nil), enabled...)
	tampered = []byte(strings.Replace(string(tampered), "binlog_format=mixed", "binlog_format=row", 1))
	if _, managed, _ = parseDatabaseBinlogConfig(tampered); managed {
		t.Fatal("edited managed binlog marker block was accepted")
	}
}

func TestDatabaseBinlogCommandValidationRequiresFrozenDestructiveConfirmation(t *testing.T) {
	valid := DatabaseBinlogInput{InstanceID: "mariadb-binlog", Command: "purge", ExpectedConfigRevision: strings.Repeat("a", 64), ExpectedInstanceRevision: 2, PreviewID: strings.Repeat("b", 32), SnapshotSHA256: strings.Repeat("c", 64), BinlogsToPurge: []string{"mariadb-bin.000001"}, ConfirmReplicaHistoryLoss: true}
	raw, _ := json.Marshal(valid)
	if err := validateDatabaseBinlogInput(raw); err != nil {
		t.Fatal("valid frozen purge descriptor rejected", err)
	}
	valid.ConfirmReplicaHistoryLoss = false
	raw, _ = json.Marshal(valid)
	if core.ErrorCode(validateDatabaseBinlogInput(raw)) != "invalid_input" {
		t.Fatal("purge without explicit replica-history-loss confirmation was accepted")
	}
	valid.ConfirmReplicaHistoryLoss = true
	valid.BinlogsToPurge = []string{"mariadb-bin.000001", "mariadb-bin.000001"}
	raw, _ = json.Marshal(valid)
	if core.ErrorCode(validateDatabaseBinlogInput(raw)) != "invalid_input" {
		t.Fatal("duplicate frozen binlog names were accepted")
	}
	if core.ErrorCode(validateDatabaseBinlogInput([]byte(`{"instance_id":"mysql-binlog","command":"purge","sql":"PURGE BINARY LOGS"}`))) != "invalid_input" {
		t.Fatal("generic SQL purge input was accepted")
	}
}

func TestDatabaseBinlogDoesNotAcceptPostgresOrMongoInstances(t *testing.T) {
	for _, engine := range []string{"postgresql", "mongodb"} {
		t.Run(engine, func(t *testing.T) {
			e := testAgent(t)
			instance := Instance{ID: engine + "-binlog", PluginID: engine, InstanceID: "fixture", Status: "running", Revision: 1, Ownership: "managed"}
			instance.Paths = e.instancePaths(engine, instance.InstanceID)
			instance.Service = ServiceProfile{ID: instance.ID, PluginID: engine, InstanceID: instance.InstanceID, Unit: "powerlinux-plugin-" + instance.ID + ".service", Program: "/usr/bin/test", ConfigPath: instance.Paths.ConfigDir, DataPath: instance.Paths.DataDir, Ownership: "managed"}
			if err := e.save("receipts", instance.ID, instance); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(DatabaseBinlogInput{InstanceID: instance.ID, Command: "status"})
			if _, err := e.databaseBinlog(context.Background(), raw); core.ErrorCode(err) != "unsupported_database" {
				t.Fatal("non-MySQL server reached the binlog query path", core.ErrorCode(err))
			}
		})
	}
}

func TestDatabaseBinlogPendingConfigIntentGuardIsInstanceAndExecutorScoped(t *testing.T) {
	e := testAgent(t)
	i := Instance{ID: "mariadb-binlog-guard", PluginID: "mariadb", InstanceID: "guard", Revision: 4, Ownership: "managed"}
	i.Paths = e.instancePaths(i.PluginID, i.InstanceID)
	if err := e.save("receipts", i.ID, i); err != nil {
		t.Fatal(err)
	}
	before := []byte("[mysqld]\n")
	beforeDigest := sha256.Sum256(before)
	intent := databaseBinlogConfigIntent{Version: 1, InstanceID: i.ID, Engine: i.PluginID, ConfigDir: i.Paths.ConfigDir, InstanceRevision: i.Revision, ExpectedRevision: hex.EncodeToString(beforeDigest[:]), FromEnabled: false, ToEnabled: true, Before: before, After: []byte("[mysqld]\n" + databaseBinlogConfigMarker + "\n[mysqld]\nlog-bin=mysql-bin\nbinlog_format=mixed\n"), State: "pending", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := e.save("private", databaseBinlogConfigIntentID(i), intent); err != nil {
		t.Fatal(err)
	}
	if core.ErrorCode(e.ensureInstallFinalized(context.Background(), i.ID)) != "operation_recovery_required" {
		t.Fatal("ordinary mutation passed the pending binlog configuration guard")
	}
	allowed := databaseBinlogWithIntentContext(context.Background(), e, i.ID)
	if err := e.ensureInstallFinalized(allowed, i.ID); err != nil {
		t.Fatal("validated same-instance recovery context did not reach recovery", err)
	}
	other := databaseBinlogWithIntentContext(context.Background(), e, "different-instance")
	if core.ErrorCode(e.ensureInstallFinalized(other, i.ID)) != "operation_recovery_required" {
		t.Fatal("recovery context for another instance bypassed the pending guard")
	}
	restarted, err := NewPrivilegedExecutor(e.config)
	if err != nil {
		t.Fatal(err)
	}
	foreign := databaseBinlogWithIntentContext(context.Background(), e, i.ID)
	if err := restarted.ensureInstallFinalized(foreign, i.ID); core.ErrorCode(err) != "operation_recovery_required" {
		t.Fatal("recovery context from another executor bypassed the pending guard", core.ErrorCode(err), err)
	}
	intent.State = "completed"
	intent.InstanceRevision = i.Revision - 1
	if err := e.save("private", databaseBinlogConfigIntentID(i), intent); err != nil {
		t.Fatal(err)
	}
	if err := e.ensureInstallFinalized(context.Background(), i.ID); err != nil {
		t.Fatal("settled historical config receipt blocked a later instance revision", err)
	}
}
