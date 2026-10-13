package plugins

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// DatabaseConnection stores no passwords or root paths. A database-scoped
// profile must pass real least-privilege checks before becoming usable.
type DatabaseConnection struct {
	ID            string    `json:"id"`
	InstanceID    string    `json:"instance_id,omitempty"`
	Name          string    `json:"name"`
	Engine        string    `json:"engine"`
	Host          string    `json:"host"`
	Port          int       `json:"port"`
	Database      string    `json:"database"`
	Username      string    `json:"username"`
	TLS           bool      `json:"tls"`
	Access        string    `json:"access"`
	CredentialRef string    `json:"credential_ref,omitempty"`
	Resource      string    `json:"resource,omitempty"`
	Revision      int64     `json:"revision"`
	CreatedAt     time.Time `json:"created_at"`
}
type DatabaseRegisterInput struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Engine           string `json:"engine"`
	Host             string `json:"host"`
	Port             int    `json:"port"`
	Database         string `json:"database"`
	Username         string `json:"username"`
	Password         string `json:"password"`
	TLS              bool   `json:"tls"`
	Access           string `json:"access"`
	ExpectedRevision int64  `json:"expected_revision"`
}
type DatabaseQueryInput struct {
	ID         string `json:"id"`
	Database   string `json:"database,omitempty"`
	SQL        string `json:"sql"`
	Parameters []any  `json:"parameters,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

func DatabaseResource(id, database string) string {
	if database != "" {
		return "db:" + id + ":" + database
	}
	return "db:" + id
}
func databaseProfileResource(p DatabaseConnection) string {
	if p.InstanceID != "" {
		return DatabaseResource(p.InstanceID, p.Database)
	}
	if p.Access == "database" {
		return DatabaseResource(p.ID, p.Database)
	}
	return DatabaseResource(p.ID, "")
}
func publicDatabaseConnection(p DatabaseConnection) DatabaseConnection {
	p.CredentialRef = ""
	p.Resource = databaseProfileResource(p)
	return p
}
func (m *Manager) databaseRegisterResources(ctx context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var in DatabaseRegisterInput
	if e := parse(raw, &in); e != nil {
		return nil, e
	}
	if !validInstanceID(in.ID) {
		return nil, failure("invalid_input", "Invalid database connection ID", 400)
	}
	resources := []string{DatabaseResource(in.ID, "")}
	if in.Database != "" {
		resources = append(resources, DatabaseResource(in.ID, in.Database))
	}
	previous, err := m.databaseProfile(ctx, in.ID)
	if err == nil {
		if previous.InstanceID != "" {
			return nil, failure("managed_profile", "Managed credentials must be changed using the owned database action", 409)
		}
		resources = append(resources, databaseProfileResource(previous))
	} else if core.ErrorCode(err) != "not_found" {
		return nil, err
	}
	return resources, nil
}
func (m *Manager) databaseProfile(ctx context.Context, id string) (DatabaseConnection, error) {
	var p DatabaseConnection
	raw, revision, e := m.deps.State.GetVersioned(ctx, "database_connections", id)
	if e != nil {
		return p, e
	}
	if json.Unmarshal(raw, &p) != nil {
		return p, failure("invalid_state", "Database connection metadata is corrupt", 500)
	}
	p.Revision = revision
	return p, nil
}
func (m *Manager) databaseResources(ctx context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var in struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &in) != nil || !validInstanceID(in.ID) {
		return nil, failure("invalid_input", "Invalid database connection ID", 400)
	}
	p, e := m.databaseProfile(ctx, in.ID)
	if e != nil {
		return nil, e
	}
	return []string{databaseProfileResource(p)}, nil
}

func (m *Manager) databaseBindings() []core.Binding {
	registerSchema := obj(map[string]any{"id": idSchema(), "name": textSchema(), "engine": enumSchema("mysql", "mariadb", "postgresql", "mongodb"), "host": textSchema(), "port": map[string]any{"type": "integer", "minimum": 1, "maximum": 65535}, "database": textSchema(), "username": textSchema(), "password": map[string]any{"type": "string", "minLength": 1, "maxLength": 4096, "format": "password"}, "tls": map[string]any{"type": "boolean"}, "access": enumSchema("instance", "database"), "expected_revision": revisionSchema()}, "id", "name", "engine", "host", "port", "database", "username", "password", "access", "expected_revision")
	querySchema := obj(map[string]any{"id": idSchema(), "database": textSchema(), "sql": map[string]any{"type": "string", "minLength": 1, "maxLength": 32768}, "parameters": map[string]any{"type": "array", "maxItems": 128, "description": "Use decimal strings or {encoding:integer,value:decimal-string} for exact 64-bit integers. Numeric parameters bind exact decimal text.", "items": map[string]any{}}, "mode": enumSchema("query", "execute"), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}}, "id", "sql")
	bindings := []core.Binding{
		m.binding("database_connections_list", "List authorized persistent database connection profiles without credentials", "databases:read", true, obj(map[string]any{}), func(context.Context, core.Actor, json.RawMessage) ([]string, error) { return nil, nil }, m.databaseListConnections),
		m.binding("database_connection_register", "Validate a real database identity and save its encrypted, resource-bound credential reference", "databases:admin", false, registerSchema, m.databaseRegisterResources, m.databaseRegister),
		m.binding("database_connection_get", "Read a scoped database profile without exposing its password", "databases:read", true, obj(map[string]any{"id": idSchema()}, "id"), m.databaseResources, m.databaseGetConnection),
		m.binding("database_connection_delete", "Delete a connection profile and its credential; server data is retained", "databases:admin", false, obj(map[string]any{"id": idSchema(), "expected_revision": revisionSchema()}, "id", "expected_revision"), m.databaseResources, m.databaseDeleteConnection),
		m.binding("database_status", "Probe the actual scoped database server and current identity", "databases:read", true, obj(map[string]any{"id": idSchema()}, "id"), m.databaseResources, m.databaseStatus),
		m.binding("database_tables_list", "List actual tables or collections within a verified least-privilege database", "databases:read", true, obj(map[string]any{"id": idSchema()}, "id"), m.databaseResources, m.databaseTables),
		m.binding("database_query", "Run bounded parameterized SQL with a verified database-only role, never an instance administrator", "databases:query", false, querySchema, m.databaseResources, m.databaseQuery),
		m.binding("database_instance_query", "Run bounded SQL using an explicitly authorized instance administrator profile", "databases:admin", false, querySchema, m.databaseResources, m.databaseAdminQuery),
		m.binding("mongodb_documents_find", "Read bounded MongoDB documents using database-only credentials", "databases:query", true, mongoFindSchema(), m.databaseResources, m.mongoFind),
		m.binding("mongodb_documents_insert", "Insert documents into the authorized MongoDB database", "databases:write", false, mongoInsertSchema(), m.databaseResources, m.mongoInsert),
		m.binding("mongodb_documents_update", "Update bounded MongoDB documents within the authorized database", "databases:write", false, mongoUpdateSchema(), m.databaseResources, m.mongoUpdate),
		m.binding("mongodb_documents_delete", "Delete matching documents in the authorized MongoDB database", "databases:write", false, mongoDeleteSchema(), m.databaseResources, m.mongoDelete),
	}
	return append(bindings, m.managedDatabaseBindings()...)
}
func localDatabaseHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func validDatabaseName(name string) bool {
	return len(name) > 0 && len(name) <= 63 && databaseIdentifier.MatchString(name)
}
func (m *Manager) databaseRegister(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in DatabaseRegisterInput
	if e := parse(raw, &in); e != nil {
		return core.ActionResult{}, e
	}
	declared, err := m.databaseRegisterResources(ctx, a, raw)
	if err != nil {
		return core.ActionResult{}, err
	}
	for _, resource := range declared {
		if !m.deps.Authorize(a, "databases:admin", resource) {
			return core.ActionResult{}, failure("forbidden", "Current and replacement database resources must both be authorized", 403)
		}
	}
	previous, previousErr := m.databaseProfile(ctx, in.ID)
	if previousErr == nil && previous.Revision != in.ExpectedRevision || previousErr != nil && in.ExpectedRevision != 0 {
		return core.ActionResult{}, failure("revision_conflict", "Database profile changed; reload before updating", 412)
	}
	if !validInstanceID(in.ID) || !validDatabaseName(in.Database) || in.Port < 1 || in.Port > 65535 || len(in.Username) == 0 || len(in.Username) > 128 || strings.ContainsAny(in.Host, "/\\\x00") || in.ExpectedRevision < 0 {
		return core.ActionResult{}, failure("invalid_input", "Invalid database profile", 400)
	}
	if !in.TLS && !localDatabaseHost(in.Host) {
		return core.ActionResult{}, failure("tls_required", "Remote databases require verified TLS", 400)
	}
	if in.Engine != "mysql" && in.Engine != "mariadb" && in.Engine != "postgresql" && in.Engine != "mongodb" {
		return core.ActionResult{}, failure("invalid_input", "Unsupported database engine", 400)
	}
	if in.Access != "instance" && in.Access != "database" {
		return core.ActionResult{}, failure("invalid_input", "Select instance or database access", 400)
	}
	p := DatabaseConnection{ID: in.ID, Name: in.Name, Engine: in.Engine, Host: in.Host, Port: in.Port, Database: in.Database, Username: in.Username, TLS: in.TLS, Access: in.Access, CreatedAt: time.Now().UTC()}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if p.Engine == "mongodb" {
		if e := probeMongoProfile(probeCtx, p, in.Password); e != nil {
			return core.ActionResult{}, e
		}
	} else {
		db, e := openSQLProfile(probeCtx, p, in.Password, p.Database)
		if e != nil {
			return core.ActionResult{}, e
		}
		defer db.Close()
		if p.Access == "database" {
			if e = verifySQLLeastPrivilege(probeCtx, db, p); e != nil {
				return core.ActionResult{}, e
			}
		}
	}
	ref, e := m.deps.Secrets.Put(ctx, a, databaseProfileResource(p), "database credential", []byte(in.Password))
	if e != nil {
		return core.ActionResult{}, e
	}
	p.CredentialRef = ref
	p.Revision = in.ExpectedRevision + 1
	encoded, _ := json.Marshal(p)
	revision, e := m.deps.State.CompareAndSwap(ctx, "database_connections", p.ID, in.ExpectedRevision, encoded)
	if e != nil {
		_ = m.deps.Secrets.Delete(ctx, a, databaseProfileResource(p), ref)
		return core.ActionResult{}, e
	}
	p.Revision = revision
	if previousErr == nil && previous.CredentialRef != "" {
		_ = m.deps.Secrets.Delete(ctx, a, databaseProfileResource(previous), previous.CredentialRef)
	}
	return core.ActionResult{Data: publicDatabaseConnection(p), Resources: declared, AuditSummary: map[string]any{"connection_id": p.ID, "engine": p.Engine, "access": p.Access}}, nil
}
func (m *Manager) databaseListConnections(ctx context.Context, a core.Actor, _ json.RawMessage) (core.ActionResult, error) {
	records, e := m.deps.State.List(ctx, "database_connections")
	if e != nil {
		return core.ActionResult{}, e
	}
	items := []DatabaseConnection{}
	for _, raw := range records {
		var p DatabaseConnection
		if json.Unmarshal(raw, &p) == nil && m.deps.Authorize(a, "databases:read", databaseProfileResource(p)) {
			items = append(items, publicDatabaseConnection(p))
		}
	}
	return core.ActionResult{Data: map[string]any{"items": items, "total": len(items)}}, nil
}
func (m *Manager) databaseGetConnection(ctx context.Context, _ core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in IDInput
	if e := parse(raw, &in); e != nil {
		return core.ActionResult{}, e
	}
	p, e := m.databaseProfile(ctx, in.ID)
	return core.ActionResult{Data: publicDatabaseConnection(p), Resources: []string{databaseProfileResource(p)}}, e
}
func (m *Manager) databaseDeleteConnection(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in struct {
		ID               string `json:"id"`
		ExpectedRevision int64  `json:"expected_revision"`
	}
	if e := parse(raw, &in); e != nil {
		return core.ActionResult{}, e
	}
	p, e := m.databaseProfile(ctx, in.ID)
	if e != nil {
		return core.ActionResult{}, e
	}
	if p.Revision != in.ExpectedRevision {
		return core.ActionResult{}, failure("revision_conflict", "Database profile changed; reload before deleting", 412)
	}
	if e = m.deps.State.Transaction(ctx, func(tx core.MetadataStore) error {
		_, revision, e := tx.GetVersioned(ctx, "database_connections", p.ID)
		if e != nil {
			return e
		}
		if revision != in.ExpectedRevision {
			return failure("revision_conflict", "Database profile changed", 412)
		}
		return tx.Delete(ctx, "database_connections", p.ID)
	}); e != nil {
		return core.ActionResult{}, e
	}
	if e = m.deps.Secrets.Delete(ctx, a, databaseProfileResource(p), p.CredentialRef); e != nil {
		return core.ActionResult{}, e
	}
	return core.ActionResult{Data: map[string]any{"id": p.ID, "deleted": true, "server_data_retained": true}, Resources: []string{databaseProfileResource(p)}}, nil
}
func (m *Manager) sqlConnection(ctx context.Context, a core.Actor, p DatabaseConnection, database string, admin bool) (*sql.DB, error) {
	if p.Engine == "mongodb" {
		return nil, failure("unsupported_operation", "Use MongoDB document actions", 400)
	}
	if admin && p.Access != "instance" || !admin && p.Access != "database" {
		return nil, failure("credential_scope", "Instance credentials cannot be used for database-scoped actions", 403)
	}
	if !admin && database != "" && database != p.Database {
		return nil, failure("forbidden", "Cross-database query is not authorized", 403)
	}
	if database == "" {
		database = p.Database
	}
	secret, e := m.deps.Secrets.Get(ctx, a, databaseProfileResource(p), p.CredentialRef)
	if e != nil {
		return nil, e
	}
	db, e := openSQLProfile(ctx, p, string(secret), database)
	for i := range secret {
		secret[i] = 0
	}
	if e != nil {
		return nil, e
	}
	if !admin {
		if e = verifySQLLeastPrivilege(ctx, db, p); e != nil {
			db.Close()
			return nil, e
		}
	}
	return db, nil
}
func openSQLProfile(ctx context.Context, p DatabaseConnection, password, database string) (*sql.DB, error) {
	var driver, dsn string
	if p.Engine == "postgresql" {
		driver = "pgx"
		u := url.URL{Scheme: "postgres", User: url.UserPassword(p.Username, password), Host: net.JoinHostPort(p.Host, strconv.Itoa(p.Port)), Path: "/" + database}
		q := u.Query()
		q.Set("connect_timeout", "5")
		q.Set("application_name", "powerlinux")
		q.Set("sslmode", "disable")
		if p.TLS {
			q.Set("sslmode", "verify-full")
		}
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		driver = "mysql"
		c := mysql.NewConfig()
		c.User = p.Username
		c.Passwd = password
		c.Net = "tcp"
		c.Addr = net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
		c.DBName = database
		c.MultiStatements = false
		c.Timeout = 5 * time.Second
		c.ReadTimeout = 10 * time.Second
		c.WriteTimeout = 10 * time.Second
		c.ParseTime = true
		c.MaxAllowedPacket = 2 << 20
		if p.TLS {
			c.TLSConfig = "true"
		}
		dsn = c.FormatDSN()
	}
	var db *sql.DB
	var e error
	if p.Engine == "postgresql" {
		config, err := pgx.ParseConfig(dsn)
		if err != nil {
			return nil, failure("connection_failed", "PostgreSQL connection could not be configured", 502)
		}
		config.MaxProtocolMessageBodyLen = 2 << 20
		db = stdlib.OpenDB(*config)
		e = nil
	} else {
		db, e = sql.Open(driver, dsn)
	}
	if e != nil {
		return nil, failure("connection_failed", "Database connection could not be initialized", 502)
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(0)
	if e = db.PingContext(ctx); e != nil {
		db.Close()
		return nil, failure("connection_failed", "Database TLS/authentication or connection failed", 502)
	}
	return db, nil
}
