package plugins

import (
	"context"
	"database/sql"
	"database/sql/driver"
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

type tableMaintenanceFixture struct {
	inventory            [][]driver.Value
	definitions          map[string]string
	resultColumns        []string
	resultRows           [][]driver.Value
	resultErr            error
	queries              []string
	resultRowsConsumed   int
	resultRowsCloseCalls int
	connectionCloseCalls int
}

type tableMaintenanceFixtureDriver struct{ fixture *tableMaintenanceFixture }
type tableMaintenanceFixtureConn struct{ fixture *tableMaintenanceFixture }
type tableMaintenanceFixtureRows struct {
	columns []string
	values  [][]driver.Value
	index   int
	fixture *tableMaintenanceFixture
	kind    string
}

var tableMaintenanceFixtureID atomic.Uint64

func (d tableMaintenanceFixtureDriver) Open(string) (driver.Conn, error) {
	return &tableMaintenanceFixtureConn{fixture: d.fixture}, nil
}
func (c *tableMaintenanceFixtureConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fixture does not support prepared statements")
}
func (c *tableMaintenanceFixtureConn) Close() error {
	c.fixture.connectionCloseCalls++
	return nil
}
func (c *tableMaintenanceFixtureConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fixture does not support transactions")
}
func (c *tableMaintenanceFixtureConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.fixture.queries = append(c.fixture.queries, query)
	if strings.HasPrefix(query, "SELECT TABLE_NAME, TABLE_TYPE, COALESCE(ENGINE,'')") {
		return &tableMaintenanceFixtureRows{columns: []string{"TABLE_NAME", "TABLE_TYPE", "ENGINE", "TABLE_COLLATION", "CREATE_OPTIONS", "TABLE_ROWS", "DATA_LENGTH", "INDEX_LENGTH", "DATA_FREE"}, values: c.fixture.inventory, fixture: c.fixture, kind: "inventory"}, nil
	}
	if strings.HasPrefix(query, "SHOW CREATE TABLE ") {
		name := strings.TrimPrefix(query, "SHOW CREATE TABLE `app_data`.`")
		name = strings.TrimSuffix(name, "`")
		name = strings.ReplaceAll(name, "``", "`")
		definition, ok := c.fixture.definitions[name]
		if !ok {
			return nil, fmt.Errorf("unexpected table definition query %q", query)
		}
		return &tableMaintenanceFixtureRows{columns: []string{"Table", "Create Table"}, values: [][]driver.Value{{[]byte(name), []byte(definition)}}, fixture: c.fixture, kind: "definition"}, nil
	}
	if strings.HasPrefix(query, "OPTIMIZE TABLE ") || strings.HasPrefix(query, "REPAIR TABLE ") {
		if c.fixture.resultErr != nil {
			return nil, c.fixture.resultErr
		}
		return &tableMaintenanceFixtureRows{columns: c.fixture.resultColumns, values: c.fixture.resultRows, fixture: c.fixture, kind: "result"}, nil
	}
	return nil, fmt.Errorf("unexpected SQL protocol request %q", query)
}
func (r *tableMaintenanceFixtureRows) Columns() []string { return r.columns }
func (r *tableMaintenanceFixtureRows) Close() error {
	if r.kind == "result" {
		r.fixture.resultRowsCloseCalls++
	}
	return nil
}
func (r *tableMaintenanceFixtureRows) Next(target []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(target, r.values[r.index])
	r.index++
	if r.kind == "result" {
		r.fixture.resultRowsConsumed++
	}
	return nil
}

func openTableMaintenanceFixture(t *testing.T, fixture *tableMaintenanceFixture) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("powerlinux-table-maintenance-fixture-%d", tableMaintenanceFixtureID.Add(1))
	sql.Register(name, tableMaintenanceFixtureDriver{fixture: fixture})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal("could not open SQL protocol fixture", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func maintenanceInventoryRow(name, engine, options string, rows, data, index, free any) []driver.Value {
	return []driver.Value{[]byte(name), []byte("BASE TABLE"), []byte(engine), []byte("utf8mb4_general_ci"), []byte(options), rows, data, index, free}
}

func TestDatabaseTableMaintenanceClosedTypedInputAndEnginePolicy(t *testing.T) {
	valid := json.RawMessage(`{"command":"preview","instance_id":"mysql-test","database":"app_data","operation":"optimize","tables":["orders"],"expected_instance_revision":2,"expected_database_revision":3,"expected_schema_sha256":"` + strings.Repeat("a", 64) + `"}`)
	if err := validateDatabaseTableMaintenanceInput(valid); err != nil {
		t.Fatal("valid typed preview was rejected", err)
	}
	unknown := json.RawMessage(strings.TrimSuffix(string(valid), "}") + `,"sql":"DROP DATABASE app_data"}`)
	if err := validateDatabaseTableMaintenanceInput(unknown); coreErrorMessage(err) != "Invalid or unknown action fields" {
		t.Fatal("arbitrary SQL field was accepted", err)
	}
	if err := validateDatabaseTableMaintenanceEngine("repair", "InnoDB"); err == nil || !strings.Contains(coreErrorMessage(err), "not supported for InnoDB") {
		t.Fatal("InnoDB REPAIR was not rejected before SQL", err)
	}
	if err := validateDatabaseTableMaintenanceEngine("optimize", "InnoDB"); err != nil {
		t.Fatal("InnoDB OPTIMIZE should be supported", err)
	}
	if err := validateDatabaseTableMaintenanceEngine("repair", "MyISAM"); err != nil {
		t.Fatal("MyISAM REPAIR should be supported", err)
	}
	if err := validateDatabaseTableMaintenanceEngine("optimize", "Aria"); err == nil {
		t.Fatal("unsupported engine was accepted")
	}
	zeroLimit := json.RawMessage(`{"command":"list","instance_id":"mysql-test","database":"app_data","limit":0}`)
	if err := validateDatabaseTableMaintenanceInput(zeroLimit); err != nil {
		t.Fatal("zero list limit should match the default-page behavior", err)
	}
	unicodeName := strings.Repeat("😀", 64)
	unicodePreview := json.RawMessage(`{"command":"preview","instance_id":"mysql-test","database":"app_data","operation":"optimize","tables":["` + unicodeName + `"],"expected_instance_revision":2,"expected_database_revision":3,"expected_schema_sha256":"` + strings.Repeat("a", 64) + `"}`)
	if err := validateDatabaseTableMaintenanceInput(unicodePreview); err != nil {
		t.Fatal("valid 64-character, 256-byte Unicode table identifier was rejected", err)
	}
	tooLongUnicodePreview := json.RawMessage(strings.Replace(string(unicodePreview), unicodeName, unicodeName+"a", 1))
	if err := validateDatabaseTableMaintenanceInput(tooLongUnicodePreview); err == nil {
		t.Fatal("65-character table identifier was accepted")
	}
}

func TestDatabaseTableMaintenanceIdentifiersAndInventoryProtocol(t *testing.T) {
	statement, err := databaseTableMaintenanceStatement("app_data", "odd`table", "optimize")
	if err != nil || statement != "OPTIMIZE TABLE `app_data`.`odd``table`" {
		t.Fatalf("table identity was not safely quoted: %q, %v", statement, err)
	}
	if !databaseTableMaintenanceValidTableName(strings.Repeat("😀", 64)) || !databaseTableMaintenanceValidCursor(strings.Repeat("表", 64)) {
		t.Fatal("valid 64-rune multibyte table/cursor names were rejected")
	}
	if databaseTableMaintenanceValidTableName(strings.Repeat("😀", 65)) || databaseTableMaintenanceValidTableName(string([]byte{0xff})) {
		t.Fatal("overlong or malformed UTF-8 table name was accepted")
	}
	fixture := &tableMaintenanceFixture{
		inventory: [][]driver.Value{
			maintenanceInventoryRow("zeta", "MyISAM", "ROW_FORMAT=FIXED", int64(20), int64(2048), int64(128), int64(32)),
			maintenanceInventoryRow("alpha", "InnoDB", "", int64(10), int64(1024), int64(512), int64(0)),
		},
	}
	db := openTableMaintenanceFixture(t, fixture)
	first, err := readDatabaseTableMaintenanceInventory(context.Background(), db, "app_data")
	if err != nil || len(first.Items) != 2 || first.Items[0].Name != "alpha" || !first.Items[0].EstimateKnown || first.Items[1].createOptions != "ROW_FORMAT=FIXED" {
		t.Fatalf("bounded typed inventory was decoded incorrectly: %#v, %v", first, err)
	}
	fixture.inventory[0][6] = int64(9000) // changing live size estimates is not a schema revision
	fixture.inventory[0][7] = int64(7000)
	second, err := readDatabaseTableMaintenanceInventory(context.Background(), db, "app_data")
	if err != nil || second.SchemaSHA256 != first.SchemaSHA256 {
		t.Fatal("row/size statistic churn incorrectly changed the schema inventory fingerprint", err)
	}
	fixture.inventory[0][4] = []byte("ROW_FORMAT=DYNAMIC")
	third, err := readDatabaseTableMaintenanceInventory(context.Background(), db, "app_data")
	if err != nil || third.SchemaSHA256 == first.SchemaSHA256 {
		t.Fatal("schema-affecting create options were not included in the fingerprint", err)
	}
	tooMany := &tableMaintenanceFixture{inventory: make([][]driver.Value, databaseTableMaintenanceMaxTables+1)}
	for index := range tooMany.inventory {
		tooMany.inventory[index] = maintenanceInventoryRow(fmt.Sprintf("table_%04d", index), "InnoDB", "", int64(1), int64(1), int64(1), int64(0))
	}
	largeDB := openTableMaintenanceFixture(t, tooMany)
	if _, err = readDatabaseTableMaintenanceInventory(context.Background(), largeDB, "app_data"); err == nil || core.ErrorCode(err) != "result_limit" {
		t.Fatal("schema inventory larger than the bounded object limit was accepted", err)
	}
}

func TestDatabaseTableMaintenanceResultProtocolRequiresClassifiedFinalRows(t *testing.T) {
	fixture := &tableMaintenanceFixture{resultColumns: []string{"Table", "Op", "Msg_type", "Msg_text"}, resultRows: [][]driver.Value{
		{[]byte("app_data.orders"), []byte("optimize"), []byte("note"), []byte("Table does not support optimize, doing recreate + analyze instead")},
		{[]byte("app_data.orders"), []byte("optimize"), []byte("status"), []byte("OK")},
	}}
	db := openTableMaintenanceFixture(t, fixture)
	state, messages, err := readDatabaseTableMaintenanceResult(context.Background(), db, "app_data", "orders", "optimize")
	if err != nil || state != "completed" || len(messages) != 2 || messages[0].Type != "note" || messages[1].Type != "status" {
		t.Fatalf("InnoDB recreate+analyze with final OK was not reported as success with note: %s %#v %v", state, messages, err)
	}
	fixture.resultRows = [][]driver.Value{{[]byte("app_data.orders"), []byte("repair"), []byte("error"), []byte("Corrupt block")}}
	state, _, err = readDatabaseTableMaintenanceResult(context.Background(), db, "app_data", "orders", "repair")
	if err != nil || state != "failed" {
		t.Fatalf("server Error row was not classified as failed: %s %v", state, err)
	}
	fixture.resultRows = [][]driver.Value{{[]byte("app_data.orders"), []byte("repair"), []byte("note"), []byte("This engine does not support repair")}, {[]byte("app_data.orders"), []byte("repair"), []byte("status"), []byte("OK")}}
	state, _, err = readDatabaseTableMaintenanceResult(context.Background(), db, "app_data", "orders", "repair")
	if err != nil || state != "failed" {
		t.Fatalf("unsupported maintenance notice was masked by status OK: %s %v", state, err)
	}
	fixture.resultRows = [][]driver.Value{
		{[]byte("other_db.orders"), []byte("repair"), []byte("status"), []byte("OK")},
	}
	state, _, err = readDatabaseTableMaintenanceResult(context.Background(), db, "app_data", "orders", "repair")
	if err != nil || state != "outcome_unknown" {
		t.Fatalf("mismatched result table identity was trusted: %s %v", state, err)
	}
	fixture.resultRows = [][]driver.Value{
		{[]byte("app_data.orders"), []byte("optimize"), []byte("status"), []byte("OK")},
		{[]byte("app_data.orders"), []byte("repair"), []byte("status"), []byte("OK")},
	}
	state, _, err = readDatabaseTableMaintenanceResult(context.Background(), db, "app_data", "orders", "repair")
	if err != nil || state != "outcome_unknown" {
		t.Fatalf("mismatched result operation identity was trusted: %s %v", state, err)
	}
	fixture.resultRows = [][]driver.Value{
		{[]byte("app_data.orders"), []byte("optimize"), []byte("status"), []byte("OK")},
		{[]byte("app_data.orders"), []byte("optimize"), []byte("status"), []byte("WARN")},
	}
	state, _, err = readDatabaseTableMaintenanceResult(context.Background(), db, "app_data", "orders", "optimize")
	if err != nil || state != "outcome_unknown" {
		t.Fatalf("an OK followed by a non-OK status was reported as complete: %s %v", state, err)
	}
	fixture.resultRows = [][]driver.Value{
		{[]byte("app_data.orders"), []byte("optimize"), []byte("status"), []byte("OK")},
		{[]byte("app_data.orders"), []byte("optimize"), []byte("error"), []byte("late server error")},
	}
	state, _, err = readDatabaseTableMaintenanceResult(context.Background(), db, "app_data", "orders", "optimize")
	if err != nil || state != "failed" {
		t.Fatalf("an explicit Error row after OK was not reported as failed: %s %v", state, err)
	}
	fixture.resultErr = errors.New("connection dropped during table operation")
	state, messages, err = readDatabaseTableMaintenanceResult(context.Background(), db, "app_data", "orders", "optimize")
	if err != nil || state != "outcome_unknown" || len(messages) == 0 {
		t.Fatalf("transport ambiguity was not inspectable: %s %#v %v", state, messages, err)
	}
	fixture.resultErr = nil
	fixture.resultRows = [][]driver.Value{{[]byte("app_data.orders"), []byte("optimize"), []byte("status"), []byte("OK")}}
	fixture.resultColumns = []string{"Table", "Msg_text"}
	state, _, err = readDatabaseTableMaintenanceResult(context.Background(), db, "app_data", "orders", "optimize")
	if err != nil || state != "outcome_unknown" {
		t.Fatalf("unrecognized status schema was reported as complete: %s %v", state, err)
	}
	fixture.resultColumns = []string{"Table", "Op", "Msg_type", "Msg_text"}
	fixture.resultRows = make([][]driver.Value, databaseTableMaintenanceMaxMessages+1000)
	for index := range fixture.resultRows {
		fixture.resultRows[index] = []driver.Value{[]byte("app_data.orders"), []byte("optimize"), []byte("status"), []byte("OK")}
	}
	fixture.resultRowsConsumed = 0
	fixture.resultRowsCloseCalls = 0
	fixture.connectionCloseCalls = 0
	state, messages, err = readDatabaseTableMaintenanceResult(context.Background(), db, "app_data", "orders", "optimize")
	if err != nil || state != "outcome_unknown" || len(messages) != databaseTableMaintenanceMaxMessages {
		t.Fatalf("oversized SQL response was not bounded and marked unknown: %s (%d rows) %v", state, len(messages), err)
	}
	if fixture.resultRowsConsumed != databaseTableMaintenanceMaxMessages+1 || fixture.resultRowsCloseCalls != 0 || fixture.connectionCloseCalls == 0 {
		t.Fatalf("oversized SQL response was drained instead of aborting its connection: consumed=%d rowsClose=%d connClose=%d", fixture.resultRowsConsumed, fixture.resultRowsCloseCalls, fixture.connectionCloseCalls)
	}
}

func TestDatabaseTableMaintenancePublicSchemaMatchesNativeBounds(t *testing.T) {
	var listSchema, previewSchema map[string]any
	for _, binding := range (&Manager{}).databaseTableMaintenanceBindings() {
		switch binding.Action.Name {
		case "managed_database_tables_list":
			listSchema = binding.Action.InputSchema
		case "managed_database_table_maintenance_preview":
			previewSchema = binding.Action.InputSchema
		}
	}
	if listSchema == nil || previewSchema == nil {
		t.Fatal("table maintenance public bindings were not generated")
	}
	listProperties := listSchema["properties"].(map[string]any)
	if listProperties["limit"].(map[string]any)["minimum"] != 0 {
		t.Fatal("public list limit must accept native zero/default behavior")
	}
	previewProperties := previewSchema["properties"].(map[string]any)
	tables := previewProperties["tables"].(map[string]any)
	if tables["uniqueItems"] != true || tables["items"].(map[string]any)["maxLength"] != 64 {
		t.Fatal("public table selection must enforce uniqueness and the 64-character identifier limit")
	}
	for _, key := range []string{"expected_instance_revision", "expected_database_revision"} {
		max := previewProperties[key].(map[string]any)["maximum"]
		if max != int64(9223372036854775807) {
			t.Fatalf("public revision %s does not match native int64 upper bound: %#v", key, max)
		}
	}
}

func TestDatabaseTableMaintenanceSnapshotAndInterruptedInspectAreConservative(t *testing.T) {
	record := databaseTableMaintenancePreviewRecord{
		Version: 1, PreviewID: "plan_" + strings.Repeat("a", 48), InstanceID: "mysql-test", Database: "app_data", Engine: "mysql", Operation: "optimize",
		Tables:       []databaseTableMaintenanceEntry{{DatabaseTableMaintenanceTable: DatabaseTableMaintenanceTable{Name: "orders", Engine: "InnoDB"}, DefinitionSHA256: strings.Repeat("b", 64)}},
		SchemaSHA256: strings.Repeat("c", 64), InstanceRevision: 2, DatabaseRevision: 3,
	}
	first, err := databaseTableMaintenanceSnapshotDigest(record)
	if err != nil {
		t.Fatal("could not fingerprint preview", err)
	}
	record.Tables[0].DefinitionSHA256 = strings.Repeat("d", 64)
	second, err := databaseTableMaintenanceSnapshotDigest(record)
	if err != nil || first == second {
		t.Fatal("preview fingerprint did not bind the selected table definition", err)
	}
	record.Tables[0].DefinitionSHA256 = strings.Repeat("b", 64)
	record.State = "running"
	record.Results = []DatabaseTableMaintenanceItemResult{{Name: "orders", Engine: "InnoDB", State: "executing"}, {Name: "later", Engine: "InnoDB", State: "checking"}, {Name: "last", Engine: "MyISAM", State: "pending"}}
	public := publicDatabaseTableMaintenanceResult(record)
	if public.State != "outcome_unknown" || public.Items[0].State != "outcome_unknown" || public.Items[1].State != "not_started" || public.Items[2].State != "not_started" {
		t.Fatalf("interrupted SQL was not kept distinct from unstarted tables: %#v", public)
	}
	if public.FinishedAt != (time.Time{}) {
		t.Fatal("inspect mutated timestamps while projecting an interrupted task")
	}
}
