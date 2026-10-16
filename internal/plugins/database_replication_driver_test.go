package plugins

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

// These fixtures exercise SQL protocol decoding and typed statement selection.
// They deliberately do not claim native MySQL or MariaDB behavior.
type replicationSQLFixture struct {
	query func(string, []driver.NamedValue) ([]string, [][]driver.Value, error)
	execs []string
}

type replicationSQLConnector struct{ fixture *replicationSQLFixture }
type replicationSQLConn struct{ fixture *replicationSQLFixture }
type replicationSQLRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (c replicationSQLConnector) Connect(context.Context) (driver.Conn, error) {
	return &replicationSQLConn{fixture: c.fixture}, nil
}
func (replicationSQLConnector) Driver() driver.Driver { return replicationSQLDriver{} }
func (replicationSQLDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("replication fixture requires its connector")
}

type replicationSQLDriver struct{}

func (c *replicationSQLConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared queries are not used by these protocol fixtures")
}
func (c *replicationSQLConn) Close() error { return nil }
func (c *replicationSQLConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not used")
}
func (c *replicationSQLConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	columns, values, err := c.fixture.query(query, args)
	if err != nil {
		return nil, err
	}
	return &replicationSQLRows{columns: columns, values: values}, nil
}
func (c *replicationSQLConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.fixture.execs = append(c.fixture.execs, query)
	return driver.RowsAffected(1), nil
}
func (r *replicationSQLRows) Columns() []string { return r.columns }
func (r *replicationSQLRows) Close() error      { return nil }
func (r *replicationSQLRows) Next(dest []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

func openReplicationSQLFixture(t *testing.T, fixture *replicationSQLFixture) *sql.DB {
	t.Helper()
	db := sql.OpenDB(replicationSQLConnector{fixture: fixture})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestDatabaseReplicationDriverFixturesRuntimeAndChannelRows(t *testing.T) {
	for _, test := range []struct {
		engine, version string
		channelQuery    string
		channelName     string
		row             []driver.Value
	}{
		{
			engine: "mysql", version: "8.0.36", channelQuery: "SHOW REPLICA STATUS", channelName: "Channel_Name",
			row: []driver.Value{"pl_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "127.0.0.1", "plr_bbbbbbbbbbbbbbbb", "3307", "123", "Yes", "Yes", `app\_data.%`, "mysql-bin.000001"},
		},
		{
			engine: "mariadb", version: "5.5.5-10.11.5-MariaDB", channelQuery: "SHOW ALL SLAVES STATUS", channelName: "Connection_name",
			row: []driver.Value{"pl_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "127.0.0.1", "plr_bbbbbbbbbbbbbbbb", "3307", "123", "Yes", "Yes", "mysql-bin.000001"},
		},
	} {
		t.Run(test.engine, func(t *testing.T) {
			fixture := &replicationSQLFixture{query: func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
				switch query {
				case "SELECT VERSION()":
					return []string{"VERSION()"}, [][]driver.Value{{test.version}}, nil
				case "SHOW GLOBAL VARIABLES WHERE Variable_name IN ('server_id','log_bin','binlog_format','read_only')":
					return []string{"Variable_name", "Value"}, [][]driver.Value{{"server_id", "123"}, {"log_bin", "ON"}, {"binlog_format", "ROW"}, {"read_only", "ON"}}, nil
				case "SHOW GLOBAL VARIABLES WHERE Variable_name IN ('server_id','log_bin','binlog_format','read_only','super_read_only')":
					return []string{"Variable_name", "Value"}, [][]driver.Value{{"server_id", "123"}, {"log_bin", "ON"}, {"binlog_format", "ROW"}, {"read_only", "ON"}, {"super_read_only", "ON"}}, nil
				case test.channelQuery:
					columns := []string{test.channelName}
					if test.engine == "mysql" {
						columns = []string{"Channel_Name", "Source_Host", "Source_User", "Source_Port", "Source_Server_Id", "Replica_IO_Running", "Replica_SQL_Running", "Replicate_Wild_Do_Table", "Source_Log_File"}
					} else {
						columns = []string{"Connection_name", "Master_Host", "Master_User", "Master_Port", "Master_Server_Id", "Slave_IO_Running", "Slave_SQL_Running", "Master_Log_File"}
					}
					return columns, [][]driver.Value{test.row}, nil
				default:
					return nil, nil, errors.New("unexpected SQL fixture query: " + query)
				}
			}}
			db := openReplicationSQLFixture(t, fixture)
			runtime, err := readDatabaseReplicationRuntime(context.Background(), db, test.engine)
			if err != nil || runtime.ServerID != 123 || !runtime.LogBin || runtime.Format != "ROW" || !runtime.ReadOnly || runtime.SuperReadOnly != (test.engine == "mysql") {
				t.Fatalf("runtime fixture failed: %#v %v", runtime, err)
			}
			channels, err := readDatabaseReplicationChannels(context.Background(), db, test.engine)
			if err != nil || len(channels) != 1 || databaseReplicationChannelName(channels[0], test.engine) != "pl_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
				t.Fatalf("channel fixture failed: %#v %v", channels, err)
			}
			instance := Instance{Config: map[string]any{"port": 3307}}
			record := databaseReplicationRecord{Engine: test.engine, Channel: "pl_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Account: "plr_bbbbbbbbbbbbbbbb", SourceServerID: 123, Database: "app_data"}
			if err = validateReplicationChannelRow(channels[0], instance, record); err != nil {
				t.Fatalf("exact generated channel identity was rejected: %v", err)
			}
		})
	}
}

func TestDatabaseReplicationDriverFixturesFailClosedOnMissingChannelIdentity(t *testing.T) {
	fixture := &replicationSQLFixture{query: func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if query != "SHOW REPLICA STATUS" {
			return nil, nil, errors.New("unexpected query")
		}
		return []string{"Source_Host"}, [][]driver.Value{{"127.0.0.1"}}, nil
	}}
	db := openReplicationSQLFixture(t, fixture)
	if _, err := readDatabaseReplicationChannels(context.Background(), db, "mysql"); err == nil {
		t.Fatal("channel without its exact generated name was accepted")
	}
}

func TestDatabaseReplicationGeneratedChannelSQLIsLoopbackAndBounded(t *testing.T) {
	source := Instance{Config: map[string]any{"port": 3307}}
	record := databaseReplicationRecord{
		Engine: "mysql", Channel: "pl_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Account: "plr_bbbbbbbbbbbbbbbb",
		AccountPassword: "secret'with-quote", SourceLogFile: "mysql-bin.000012", SourceLogPosition: 456,
	}
	mysqlSQL := mysqlReplicationChannelStatement(source, record)
	for _, expected := range []string{"SOURCE_HOST='127.0.0.1'", "SOURCE_PORT=3307", "FOR CHANNEL 'pl_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'", "SOURCE_LOG_POS=456", "secret''with-quote"} {
		if !strings.Contains(mysqlSQL, expected) {
			t.Fatalf("generated MySQL statement omitted %q: %s", expected, mysqlSQL)
		}
	}
	if strings.Contains(mysqlSQL, "SOURCE_HOST='source") {
		t.Fatalf("generated MySQL statement accepted a non-loopback source: %s", mysqlSQL)
	}
	record.Engine = "mariadb"
	mariaSQL := mariaReplicationChannelStatement(source, record)
	for _, expected := range []string{"CHANGE MASTER 'pl_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'", "MASTER_HOST='127.0.0.1'", "MASTER_PORT=3307", "MASTER_USE_GTID=no", "MASTER_LOG_POS=456"} {
		if !strings.Contains(mariaSQL, expected) {
			t.Fatalf("generated MariaDB statement omitted %q: %s", expected, mariaSQL)
		}
	}
}

func TestDatabaseReplicationDumpCoordinatesSupportMySQLAndMariaDBFormats(t *testing.T) {
	for _, dump := range []string{
		"-- CHANGE MASTER TO MASTER_LOG_FILE='mysql-bin.000003', MASTER_LOG_POS=987;\n",
		"-- CHANGE REPLICATION SOURCE TO SOURCE_LOG_FILE='mysql-bin.000004', SOURCE_LOG_POS=654;\n",
	} {
		file, position, err := parseDatabaseReplicationCoordinate([]byte(dump))
		if err != nil || file == "" || position == 0 {
			t.Fatalf("known consistent-dump coordinate was rejected: %q %d %v", file, position, err)
		}
	}
}

func TestDatabaseReplicationDriverFixtureFingerprintsOwnedSchemaAndRejectsCrossSchemaObjects(t *testing.T) {
	newFixture := func(crossSchema, functionalIndex bool) *replicationSQLFixture {
		return &replicationSQLFixture{query: func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
			switch {
			case strings.HasPrefix(query, "SELECT TABLE_NAME, ENGINE,"):
				return []string{"TABLE_NAME", "ENGINE", "DATA_LENGTH", "INDEX_LENGTH", "ROW_FORMAT", "TABLE_COLLATION", "CREATE_OPTIONS", "TABLE_COMMENT"}, [][]driver.Value{{"orders", "InnoDB", int64(1024), int64(512), "Dynamic", "utf8mb4_general_ci", "", ""}}, nil
			case strings.HasPrefix(query, "SELECT TABLE_NAME, COLUMN_NAME, ORDINAL_POSITION"):
				return []string{"TABLE_NAME", "COLUMN_NAME", "ORDINAL_POSITION", "COLUMN_TYPE", "IS_NULLABLE", "COLUMN_DEFAULT", "EXTRA", "CHARACTER_SET_NAME", "COLLATION_NAME", "COLUMN_KEY", "GENERATION_EXPRESSION"}, [][]driver.Value{{"orders", "id", int64(1), "bigint unsigned", "NO", "", "auto_increment", "", "", "PRI", ""}}, nil
			case strings.HasPrefix(query, "SELECT TABLE_NAME, INDEX_NAME, NON_UNIQUE"):
				column := "id"
				if functionalIndex {
					column = ""
				}
				return []string{"TABLE_NAME", "INDEX_NAME", "NON_UNIQUE", "SEQ_IN_INDEX", "COLUMN_NAME", "COLLATION", "SUB_PART", "NULLABLE", "INDEX_TYPE", "COMMENT", "INDEX_COMMENT"}, [][]driver.Value{{"orders", "PRIMARY", int64(0), int64(1), column, "A", int64(0), "", "BTREE", "", ""}}, nil
			case strings.HasPrefix(query, "SELECT CONSTRAINT_NAME, TABLE_NAME, COLUMN_NAME"):
				referencedSchema := ""
				if crossSchema {
					referencedSchema = "other_db"
				}
				return []string{"CONSTRAINT_NAME", "TABLE_NAME", "COLUMN_NAME", "ORDINAL_POSITION", "POSITION_IN_UNIQUE_CONSTRAINT", "REFERENCED_TABLE_SCHEMA", "REFERENCED_TABLE_NAME", "REFERENCED_COLUMN_NAME"}, [][]driver.Value{{"PRIMARY", "orders", "id", int64(1), int64(0), referencedSchema, "", ""}}, nil
			case strings.HasPrefix(query, "SELECT CONSTRAINT_NAME, CHECK_CLAUSE"):
				return []string{"CONSTRAINT_NAME", "CHECK_CLAUSE"}, nil, nil
			case strings.HasPrefix(query, "SELECT COUNT(*)"):
				return []string{"COUNT(*)"}, [][]driver.Value{{int64(0)}}, nil
			default:
				return nil, nil, errors.New("unexpected schema-fixture query: " + query)
			}
		}}
	}
	db := openReplicationSQLFixture(t, newFixture(false, false))
	schema, err := inspectDatabaseReplicationSchema(context.Background(), db, "app_data")
	if err != nil || schema.Tables != 1 || schema.Bytes != 1536 || len(schema.Names) != 1 || schema.Fingerprint == "" {
		t.Fatalf("bounded schema fixture failed: %#v %v", schema, err)
	}
	db.Close()

	db = openReplicationSQLFixture(t, newFixture(true, false))
	if _, err = inspectDatabaseReplicationSchema(context.Background(), db, "app_data"); core.ErrorCode(err) != "replication_cross_schema_constraint" {
		t.Fatalf("cross-schema foreign key was not rejected: %v", err)
	}
	db.Close()

	db = openReplicationSQLFixture(t, newFixture(false, true))
	if _, err = inspectDatabaseReplicationSchema(context.Background(), db, "app_data"); core.ErrorCode(err) != "replication_schema_unsupported" {
		t.Fatalf("unfingerprintable functional index was not rejected: %v", err)
	}
}
