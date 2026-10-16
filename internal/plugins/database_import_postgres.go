package plugins

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
)

const maxPostgresJSONImportBytes = 64 << 20

var (
	postgresImportNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
	postgresImportNumeric     = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)
)

type postgresJSONImport struct {
	Format string              `json:"format"`
	Tables []postgresJSONTable `json:"tables"`
}

type postgresJSONTable struct {
	Name       string               `json:"name"`
	Columns    []postgresJSONColumn `json:"columns"`
	PrimaryKey []string             `json:"primary_key,omitempty"`
	Rows       [][]json.RawMessage  `json:"rows,omitempty"`
}

type postgresJSONColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable,omitempty"`
}

type preparedPostgresJSONImport struct {
	Tables []preparedPostgresTable
}

type preparedPostgresTable struct {
	Name       string
	Columns    []postgresJSONColumn
	PrimaryKey []string
	Rows       [][]any
}

func parsePostgresJSONImport(ctx context.Context, source io.ReadSeeker) (*preparedPostgresJSONImport, error) {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: source}, maxPostgresJSONImportBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > maxPostgresJSONImportBytes {
		return nil, failure("artifact_invalid", "PostgreSQL JSON import must be between 1 byte and 64 MiB", 413)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var input postgresJSONImport
	if err = decoder.Decode(&input); err != nil {
		return nil, failure("artifact_invalid", "PostgreSQL import is not a supported typed JSON document", 412)
	}
	var trailing any
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, failure("artifact_invalid", "PostgreSQL import contains trailing data", 412)
	}
	if input.Format != "powerlinux-postgresql-import-v1" || len(input.Tables) == 0 || len(input.Tables) > 1000 {
		return nil, failure("artifact_invalid", "PostgreSQL import format or table count is unsupported", 412)
	}
	prepared := &preparedPostgresJSONImport{Tables: make([]preparedPostgresTable, 0, len(input.Tables))}
	seenTables := map[string]bool{}
	totalCells := 0
	for _, table := range input.Tables {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !postgresImportNamePattern.MatchString(table.Name) || strings.HasPrefix(table.Name, "pg_") || len(table.Columns) == 0 || len(table.Columns) > 128 || seenTables[table.Name] {
			return nil, failure("artifact_invalid", "PostgreSQL import contains an unsafe or duplicate table name", 412)
		}
		seenTables[table.Name] = true
		columns := make(map[string]postgresJSONColumn, len(table.Columns))
		for _, column := range table.Columns {
			if !postgresImportNamePattern.MatchString(column.Name) || strings.HasPrefix(column.Name, "pg_") || !postgresImportType(column.Type) {
				return nil, failure("artifact_invalid", "PostgreSQL import contains an unsafe column name or unsupported type", 412)
			}
			if _, exists := columns[column.Name]; exists {
				return nil, failure("artifact_invalid", "PostgreSQL import repeats a column name", 412)
			}
			columns[column.Name] = column
		}
		primary := map[string]bool{}
		for _, name := range table.PrimaryKey {
			if _, exists := columns[name]; !exists || primary[name] {
				return nil, failure("artifact_invalid", "PostgreSQL import primary key references an unknown or repeated column", 412)
			}
			primary[name] = true
		}
		if len(table.Rows) > 100000 {
			return nil, failure("artifact_invalid", "PostgreSQL import contains too many rows in one table", 413)
		}
		out := preparedPostgresTable{Name: table.Name, Columns: table.Columns, PrimaryKey: table.PrimaryKey, Rows: make([][]any, 0, len(table.Rows))}
		for _, row := range table.Rows {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if len(row) != len(table.Columns) {
				return nil, failure("artifact_invalid", "PostgreSQL import row width differs from its column list", 412)
			}
			totalCells += len(row)
			if totalCells > 1_000_000 {
				return nil, failure("artifact_invalid", "PostgreSQL import exceeds one million typed values", 413)
			}
			values := make([]any, len(row))
			for i, raw := range row {
				value, err := postgresImportValue(raw, table.Columns[i])
				if err != nil {
					return nil, err
				}
				values[i] = value
			}
			out.Rows = append(out.Rows, values)
		}
		prepared.Tables = append(prepared.Tables, out)
	}
	return prepared, nil
}

func postgresImportType(value string) bool {
	switch value {
	case "smallint", "integer", "bigint", "boolean", "real", "double precision", "numeric", "text", "varchar", "jsonb", "bytea":
		return true
	default:
		return false
	}
}

func postgresImportSQLType(value string) string {
	switch value {
	case "smallint":
		return "pg_catalog.int2"
	case "integer":
		return "pg_catalog.int4"
	case "bigint":
		return "pg_catalog.int8"
	case "boolean":
		return "pg_catalog.bool"
	case "real":
		return "pg_catalog.float4"
	case "double precision":
		return "pg_catalog.float8"
	case "numeric":
		return "pg_catalog.numeric"
	case "text":
		return "pg_catalog.text"
	case "varchar":
		return "pg_catalog.varchar"
	case "jsonb":
		return "pg_catalog.jsonb"
	case "bytea":
		return "pg_catalog.bytea"
	default:
		return ""
	}
}

func postgresImportValue(raw json.RawMessage, column postgresJSONColumn) (any, error) {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return nil, failure("artifact_invalid", "PostgreSQL import cell is empty or exceeds one MiB", 412)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, failure("artifact_invalid", "PostgreSQL import cell is invalid JSON", 412)
	}
	if value == nil {
		if !column.Nullable {
			return nil, failure("artifact_invalid", "PostgreSQL import contains null for a non-nullable column", 412)
		}
		return nil, nil
	}
	invalid := func() (any, error) {
		return nil, failure("artifact_invalid", "PostgreSQL import value does not match its declared type", 412)
	}
	switch column.Type {
	case "smallint", "integer", "bigint":
		number, ok := value.(json.Number)
		if !ok {
			return invalid()
		}
		integer, err := strconv.ParseInt(string(number), 10, 64)
		if err != nil || (column.Type == "smallint" && (integer < -32768 || integer > 32767)) || (column.Type == "integer" && (integer < -2147483648 || integer > 2147483647)) {
			return invalid()
		}
		return integer, nil
	case "real", "double precision":
		number, ok := value.(json.Number)
		if !ok {
			return invalid()
		}
		floating, err := strconv.ParseFloat(string(number), 64)
		if err != nil || math.IsInf(floating, 0) || math.IsNaN(floating) || (column.Type == "real" && math.Abs(floating) > math.MaxFloat32) {
			return invalid()
		}
		return floating, nil
	case "numeric":
		number, ok := value.(json.Number)
		if !ok || len(number) > 128 || !postgresImportNumeric.MatchString(string(number)) {
			return invalid()
		}
		return string(number), nil
	case "boolean":
		boolean, ok := value.(bool)
		if !ok {
			return invalid()
		}
		return boolean, nil
	case "text", "varchar":
		text, ok := value.(string)
		if !ok || len(text) > 1<<20 {
			return invalid()
		}
		return text, nil
	case "jsonb":
		return string(raw), nil
	case "bytea":
		text, ok := value.(string)
		if !ok {
			return invalid()
		}
		decoded, err := base64.StdEncoding.DecodeString(text)
		if err != nil || len(decoded) > 1<<20 {
			return invalid()
		}
		return decoded, nil
	default:
		return invalid()
	}
}

func (e *PrivilegedExecutor) restorePostgresJSONImport(ctx context.Context, instance Instance, record managedDatabaseRecord, input *preparedPostgresJSONImport) error {
	profile := DatabaseConnection{Engine: instance.PluginID, Host: managedDatabaseHost(instance), Port: setting(instance.Config, "port", 0), Database: record.Database, Username: record.Username}
	db, err := openSQLProfile(ctx, profile, record.Credential, record.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	if err = verifySQLLeastPrivilege(ctx, db, profile); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return failure("import_preflight_failed", "PostgreSQL import transaction could not be started", 502)
	}
	defer tx.Rollback()
	var hasObjects bool
	err = tx.QueryRowContext(ctx, postgresImportHasObjectsSQL).Scan(&hasObjects)
	if err != nil {
		return failure("import_preflight_failed", "PostgreSQL target namespace could not be checked in its import transaction", 502)
	}
	if hasObjects {
		return failure("import_target_not_empty", "External imports require an empty managed database; existing tables and views are preserved", 409)
	}
	for _, table := range input.Tables {
		if err = ctx.Err(); err != nil {
			return err
		}
		var create strings.Builder
		create.WriteString(`CREATE TABLE "public"."`)
		create.WriteString(table.Name)
		create.WriteString(`" (`)
		for i, column := range table.Columns {
			if i > 0 {
				create.WriteString(", ")
			}
			create.WriteByte('"')
			create.WriteString(column.Name)
			create.WriteString(`" `)
			create.WriteString(postgresImportSQLType(column.Type))
			if !column.Nullable {
				create.WriteString(" NOT NULL")
			}
		}
		if len(table.PrimaryKey) > 0 {
			create.WriteString(", PRIMARY KEY (")
			for i, name := range table.PrimaryKey {
				if i > 0 {
					create.WriteString(", ")
				}
				create.WriteByte('"')
				create.WriteString(name)
				create.WriteByte('"')
			}
			create.WriteByte(')')
		}
		create.WriteByte(')')
		if _, err = tx.ExecContext(ctx, create.String()); err != nil {
			return failure("import_apply_failed", "PostgreSQL could not create an imported public table", 409)
		}
		if len(table.Rows) == 0 {
			continue
		}
		var insert strings.Builder
		insert.WriteString(`INSERT INTO "public"."`)
		insert.WriteString(table.Name)
		insert.WriteString(`" (`)
		for i, column := range table.Columns {
			if i > 0 {
				insert.WriteString(", ")
			}
			insert.WriteByte('"')
			insert.WriteString(column.Name)
			insert.WriteByte('"')
		}
		insert.WriteString(") VALUES (")
		for i, column := range table.Columns {
			if i > 0 {
				insert.WriteString(", ")
			}
			insert.WriteByte('$')
			insert.WriteString(strconv.Itoa(i + 1))
			if column.Type == "jsonb" {
				insert.WriteString("::pg_catalog.jsonb")
			}
		}
		insert.WriteByte(')')
		for _, row := range table.Rows {
			if _, err = tx.ExecContext(ctx, insert.String(), row...); err != nil {
				return failure("import_apply_failed", "PostgreSQL rejected an imported row; the transaction will be rolled back", 409)
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return failure("operation_recovery_required", "PostgreSQL import transaction did not commit; inspect the managed database", 409)
	}
	return nil
}
