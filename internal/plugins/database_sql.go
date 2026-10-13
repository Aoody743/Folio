package plugins

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Aoody743/PowerLinux/internal/core"
)

var databaseIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,62}$`)

func verifySQLLeastPrivilege(ctx context.Context, db *sql.DB, p DatabaseConnection) error {
	if p.Engine == "postgresql" {
		var super, createDB, createRole, replication, bypassRLS bool
		if e := db.QueryRowContext(ctx, "SELECT rolsuper,rolcreatedb,rolcreaterole,rolreplication,rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&super, &createDB, &createRole, &replication, &bypassRLS); e != nil {
			return failure("credential_verification_failed", "Could not verify PostgreSQL role privileges", 502)
		}
		if super || createDB || createRole || replication || bypassRLS {
			return failure("credential_scope", "Database actions require a role without superuser, create-role, create-database, replication or RLS-bypass privileges", 403)
		}
		// SET ROLE can activate privileges not shown on current_user. Reject
		// membership chains rather than trusting the login role's flags alone.
		var memberships int
		if e := db.QueryRowContext(ctx, `WITH RECURSIVE inherited(roleid) AS (SELECT roleid FROM pg_auth_members WHERE member=(SELECT oid FROM pg_roles WHERE rolname=current_user) UNION SELECT a.roleid FROM pg_auth_members a JOIN inherited i ON a.member=i.roleid) SELECT count(*) FROM inherited`).Scan(&memberships); e != nil {
			return failure("credential_verification_failed", "Could not verify PostgreSQL role membership", 502)
		}
		if memberships != 0 {
			return failure("credential_scope", "Database-scoped identities must be dedicated roles without SET ROLE memberships", 403)
		}
		return nil
	}
	rows, e := db.QueryContext(ctx, "SHOW GRANTS FOR CURRENT_USER")
	if e != nil {
		return failure("credential_verification_failed", "Could not verify MySQL role privileges", 502)
	}
	defer rows.Close()
	grantDatabase := p.Database
	if !mysqlPartialRevokes(ctx, db) {
		grantDatabase = strings.NewReplacer(`\`, `\\`, "_", `\_`, "%", `\%`).Replace(grantDatabase)
	}
	target := "`" + grantDatabase + "`.*"
	found := false
	for rows.Next() {
		var grant string
		if e = rows.Scan(&grant); e != nil {
			return failure("credential_verification_failed", "MySQL grant response is unavailable", 502)
		}
		upper := strings.ToUpper(grant)
		if strings.HasPrefix(upper, "GRANT USAGE ON *.* ") {
			continue
		}
		if strings.Contains(upper, " WITH GRANT OPTION") || strings.Contains(upper, "GRANT PROXY ") {
			return failure("credential_scope", "Delegating database privileges cannot be used for scoped queries", 403)
		}
		split := strings.SplitN(grant, " ON ", 2)
		if len(split) != 2 {
			return failure("credential_scope", "Database role grants are not confined to the selected database", 403)
		}
		object := strings.SplitN(split[1], " TO ", 2)[0]
		if object != target && !strings.HasPrefix(object, "`"+grantDatabase+"`.") {
			return failure("credential_scope", "Database identity has grants outside its selected database", 403)
		}
		found = true
	}
	if rows.Err() != nil {
		return failure("credential_verification_failed", "MySQL grants could not be read", 502)
	}
	if !found {
		return failure("credential_scope", "Database identity has no verified grant on the selected database", 403)
	}
	return nil
}
func mysqlPartialRevokes(ctx context.Context, db *sql.DB) bool {
	var name, value string
	return db.QueryRowContext(ctx, "SHOW GLOBAL VARIABLES LIKE 'partial_revokes'").Scan(&name, &value) == nil && strings.EqualFold(value, "ON")
}
func mysqlGrantIdentifier(ctx context.Context, db *sql.DB, name string) string {
	if !mysqlPartialRevokes(ctx, db) {
		name = strings.NewReplacer(`\`, `\\`, "_", `\_`, "%", `\%`).Replace(name)
	}
	return "`" + name + "`"
}
func (m *Manager) databaseStatus(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in IDInput
	if e := parse(raw, &in); e != nil {
		return core.ActionResult{}, e
	}
	p, e := m.databaseProfile(ctx, in.ID)
	if e != nil {
		return core.ActionResult{}, e
	}
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if p.Engine == "mongodb" {
		return m.mongoStatus(probe, a, p)
	}
	db, e := m.sqlConnection(probe, a, p, p.Database, p.Access == "instance")
	if e != nil {
		return core.ActionResult{}, e
	}
	defer db.Close()
	var version, user, database string
	query := "SELECT VERSION(),CURRENT_USER(),DATABASE()"
	if p.Engine == "postgresql" {
		query = "SELECT version(),current_user,current_database()"
	}
	if e = db.QueryRowContext(probe, query).Scan(&version, &user, &database); e != nil {
		return core.ActionResult{}, failure("query_failed", "Database status query failed", 502)
	}
	return core.ActionResult{Data: map[string]any{"id": p.ID, "engine": p.Engine, "connected": true, "server_version": version, "user": user, "database": database, "sampled_at": time.Now().UTC()}, Resources: []string{databaseProfileResource(p)}}, nil
}
func (m *Manager) databaseTables(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in IDInput
	if e := parse(raw, &in); e != nil {
		return core.ActionResult{}, e
	}
	p, e := m.databaseProfile(ctx, in.ID)
	if e != nil {
		return core.ActionResult{}, e
	}
	if p.Engine == "mongodb" {
		return m.mongoCollections(ctx, a, p)
	}
	query := "SELECT table_name FROM information_schema.tables WHERE table_schema=? ORDER BY table_name"
	parameters := []any{p.Database}
	if p.Engine == "postgresql" {
		query = "SELECT schemaname,tablename FROM pg_catalog.pg_tables WHERE schemaname NOT IN ('pg_catalog','information_schema') ORDER BY schemaname,tablename"
		parameters = nil
	}
	result, e := m.runSQLQuery(ctx, a, p, DatabaseQueryInput{ID: p.ID, SQL: query, Parameters: parameters, Limit: 1000, Mode: "query"}, false)
	if e != nil {
		return core.ActionResult{}, e
	}
	return core.ActionResult{Data: result, Resources: []string{databaseProfileResource(p)}}, nil
}
func (m *Manager) databaseQuery(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	return m.queryDatabase(ctx, a, raw, false)
}
func (m *Manager) databaseAdminQuery(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	return m.queryDatabase(ctx, a, raw, true)
}
func (m *Manager) queryDatabase(ctx context.Context, a core.Actor, raw json.RawMessage, admin bool) (core.ActionResult, error) {
	var in DatabaseQueryInput
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if e := decoder.Decode(&in); e != nil {
		return core.ActionResult{}, failure("invalid_input", "Invalid SQL input", 400)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return core.ActionResult{}, failure("invalid_input", "Invalid SQL input", 400)
	}
	for i, value := range in.Parameters {
		parameter, err := exactSQLParameter(value)
		if err != nil {
			return core.ActionResult{}, err
		}
		in.Parameters[i] = parameter
	}
	p, e := m.databaseProfile(ctx, in.ID)
	if e != nil {
		return core.ActionResult{}, e
	}
	if in.Mode == "" {
		in.Mode = "query"
	}
	if in.Mode != "query" && in.Mode != "execute" {
		return core.ActionResult{}, failure("invalid_input", "SQL mode must be query or execute", 400)
	}
	if len(in.SQL) == 0 || len(in.SQL) > 32768 || len(in.Parameters) > 128 {
		return core.ActionResult{}, failure("invalid_input", "SQL input exceeds limits", 400)
	}
	result, e := m.runSQLQuery(ctx, a, p, in, admin)
	if e != nil {
		return core.ActionResult{}, e
	}
	if m.deps.Fingerprint == nil {
		return core.ActionResult{Data: result, Resources: []string{databaseProfileResource(p)}, AuditSummary: map[string]any{"connection_id": p.ID, "database": p.Database, "mode": in.Mode}}, nil
	}
	fingerprint := m.deps.Fingerprint(a.ID+":sql:"+p.ID+":"+in.Mode, []byte(in.SQL))
	return core.ActionResult{Data: result, Resources: []string{databaseProfileResource(p)}, AuditSummary: map[string]any{"connection_id": p.ID, "database": p.Database, "statement_hash": fingerprint, "mode": in.Mode}}, nil
}
func (m *Manager) runSQLQuery(ctx context.Context, a core.Actor, p DatabaseConnection, in DatabaseQueryInput, admin bool) (map[string]any, error) {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	db, e := m.sqlConnection(bounded, a, p, in.Database, admin)
	if e != nil {
		return nil, e
	}
	defer db.Close()
	if in.Mode == "execute" {
		result, e := db.ExecContext(bounded, in.SQL, in.Parameters...)
		if e != nil {
			return nil, failure("query_failed", "Database rejected the operation or its scoped privileges", 400)
		}
		affected, _ := result.RowsAffected()
		return map[string]any{"rows_affected": exactSQLInteger(affected), "database": p.Database}, nil
	}
	limit := in.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 {
		return nil, failure("invalid_input", "Query limit must be 1–1000", 400)
	}
	rows, e := db.QueryContext(bounded, in.SQL, in.Parameters...)
	if e != nil {
		return nil, failure("query_failed", "Database rejected the query or its scoped privileges", 400)
	}
	defer rows.Close()
	columns, e := rows.Columns()
	if e != nil || len(columns) > 256 {
		return nil, failure("result_limit", "Query column count exceeds the limit", 413)
	}
	columnTypes, e := rows.ColumnTypes()
	if e != nil {
		return nil, failure("query_failed", "Query column metadata could not be decoded", 502)
	}
	items := []map[string]any{}
	totalBytes := 0
	truncated := false
	for rows.Next() {
		if len(items) >= limit {
			truncated = true
			break
		}
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for i := range values {
			destinations[i] = &values[i]
		}
		if e = rows.Scan(destinations...); e != nil {
			return nil, failure("query_failed", "Query row could not be decoded", 502)
		}
		item := map[string]any{}
		for i, value := range values {
			switch v := value.(type) {
			case []byte:
				if len(v) > 128<<10 {
					return nil, failure("result_limit", "One result field exceeds 128 KiB; use a streaming export", 413)
				}
				typeName := strings.ToUpper(columnTypes[i].DatabaseTypeName())
				if strings.Contains(typeName, "BLOB") || strings.Contains(typeName, "BINARY") || typeName == "BYTEA" || !utf8.Valid(v) {
					value = map[string]any{"encoding": "base64", "data": base64.StdEncoding.EncodeToString(v)}
				} else {
					value = integerColumnText(typeName, string(v))
				}
			case time.Time:
				value = v.UTC().Format(time.RFC3339Nano)
			case string:
				if len(v) > 128<<10 {
					return nil, failure("result_limit", "One result field exceeds 128 KiB", 413)
				}
			case float64:
				if math.IsNaN(v) {
					value = "NaN"
				} else if math.IsInf(v, 1) {
					value = "Infinity"
				} else if math.IsInf(v, -1) {
					value = "-Infinity"
				}
			case int64, uint64:
				value = exactSQLInteger(value)
			}
			name := columns[i]
			if _, exists := item[name]; exists {
				name = fmt.Sprintf("%s_%d", name, i)
			}
			item[name] = value
		}
		encoded, marshalErr := json.Marshal(item)
		if marshalErr != nil {
			return nil, failure("query_failed", "Query row contains an unsupported wire value", 502)
		}
		totalBytes += len(encoded)
		if totalBytes > 1<<20 {
			return nil, failure("result_limit", "Query result exceeds 1 MiB; use a streaming export", 413)
		}
		items = append(items, item)
	}
	if e = rows.Err(); e != nil {
		return nil, failure("query_failed", "Query was canceled, timed out or failed", 502)
	}
	return map[string]any{"columns": columns, "items": items, "count": len(items), "limit": limit, "truncated": truncated, "database": p.Database}, nil
}
