package plugins

import (
	"context"
	"database/sql"
	"encoding/hex"
)

func mysqlDatabasePermission(permission string) string {
	switch permission {
	case "read_write":
		return "ALL PRIVILEGES"
	case "data_write":
		return "SELECT,INSERT,UPDATE,DELETE,SHOW VIEW"
	default:
		return "SELECT,SHOW VIEW"
	}
}

// Permission changes run only against an authoritative owned database receipt.
// PostgreSQL ownership is held by a dedicated, unprivileged NOLOGIN role while
// a database is read-only. Transferring objects to the administrator would turn
// user-created SECURITY DEFINER routines into administrator-owned routines.
func (e *PrivilegedExecutor) setSQLDatabasePermission(ctx context.Context, instance Instance, record *managedDatabaseRecord, permission string, admin *sql.DB) error {
	role := quoteDatabaseIdentifier(instance.PluginID, record.Username)
	if instance.PluginID != "postgresql" {
		if err := verifyManagedSQLCredential(ctx, instance, *record); err != nil {
			return err
		}
		host := "127.0.0.1"
		if managedDatabaseHost(instance) == "::1" {
			host = "::1"
		}
		role += "@'" + host + "'"
		object := mysqlGrantIdentifier(ctx, admin, record.Database) + ".*"
		// MySQL automatically grants routine-level ALTER/EXECUTE privileges to
		// creators. Revoking only database-level grants would leave those writes
		// available. The verified dedicated identity has no other DB's grants.
		if _, err := admin.ExecContext(ctx, "REVOKE ALL PRIVILEGES, GRANT OPTION FROM "+role); err != nil {
			return err
		}
		privilege := mysqlDatabasePermission(permission)
		_, err := admin.ExecContext(ctx, "GRANT "+privilege+" ON "+object+" TO "+role)
		return err
	}
	var root managedDatabaseCredential
	if err := e.load("private", databasePrivateID(instance), &root); err != nil {
		return err
	}
	profile := DatabaseConnection{Engine: "postgresql", Host: managedDatabaseHost(instance), Port: setting(instance.Config, "port", 0), Username: root.Username}
	target, err := openSQLProfile(ctx, profile, root.Password, record.Database)
	if err != nil {
		return err
	}
	defer target.Close()
	if err = checkPostgresOwnedRole(ctx, target, record.Username, record.Database, true); err != nil {
		return err
	}
	owner := record.PermissionOwner
	if permission != "read_write" && record.Permission == "read_write" && owner == "" {
		owner = "pl_owner_" + hex.EncodeToString(sha256Digest(instance.ID + "|" + record.Database)[:16])
		var exists int
		if err = target.QueryRowContext(ctx, "SELECT count(*) FROM pg_roles WHERE rolname=$1", owner).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			return failure("ownership_conflict", "Read-only ownership role already exists without this database's receipt", 409)
		}
		if _, err = target.ExecContext(ctx, "CREATE ROLE "+quoteDatabaseIdentifier("postgresql", owner)+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS"); err != nil {
			return err
		}
		record.PermissionOwner = owner
		if err = e.save("private", managedDatabaseKey(instance.ID, record.Database), record); err != nil {
			return err
		}
	}
	if owner != "" {
		if err = checkPostgresOwnedRole(ctx, target, owner, record.Database, false); err != nil {
			return err
		}
	}
	tx, err := target.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	databaseID := quoteDatabaseIdentifier("postgresql", record.Database)
	if owner != "" && permission != "read_write" {
		_, err = tx.ExecContext(ctx, "REASSIGN OWNED BY "+role+" TO "+quoteDatabaseIdentifier("postgresql", owner))
	} else if owner != "" && permission == "read_write" {
		_, err = tx.ExecContext(ctx, "REASSIGN OWNED BY "+quoteDatabaseIdentifier("postgresql", owner)+" TO "+role)
	}
	if err != nil {
		return err
	}
	if permission == "read_write" {
		if _, err = tx.ExecContext(ctx, "ALTER DATABASE "+databaseID+" OWNER TO "+role); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "REVOKE ALL ON DATABASE "+databaseID+" FROM "+role); err != nil {
		return err
	}
	privilege := "CONNECT"
	if permission == "read_write" {
		privilege = "CONNECT,CREATE,TEMPORARY"
	}
	if _, err = tx.ExecContext(ctx, "GRANT "+privilege+" ON DATABASE "+databaseID+" TO "+role); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT nspname FROM pg_namespace WHERE nspname !~ '^pg_' AND nspname <> 'information_schema' ORDER BY nspname`)
	if err != nil {
		return err
	}
	schemas := []string{}
	for rows.Next() {
		var schema string
		if err = rows.Scan(&schema); err != nil {
			rows.Close()
			return err
		}
		schemas = append(schemas, schema)
		if len(schemas) > 1000 {
			rows.Close()
			return failure("result_limit", "Database has too many schemas for a bounded permission change", 413)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, schema := range schemas {
		name := quoteDatabaseIdentifier("postgresql", schema)
		statements := []string{"REVOKE ALL ON SCHEMA " + name + " FROM " + role, "REVOKE ALL ON ALL TABLES IN SCHEMA " + name + " FROM " + role, "REVOKE ALL ON ALL SEQUENCES IN SCHEMA " + name + " FROM " + role, "REVOKE EXECUTE ON ALL ROUTINES IN SCHEMA " + name + " FROM PUBLIC", "REVOKE EXECUTE ON ALL ROUTINES IN SCHEMA " + name + " FROM " + role}
		data := "SELECT"
		schemaPrivileges := "USAGE"
		if permission == "read_write" {
			data = "ALL PRIVILEGES"
			schemaPrivileges = "USAGE,CREATE"
		}
		if permission == "data_write" {
			data = "SELECT,INSERT,UPDATE,DELETE"
		}
		statements = append(statements, "GRANT "+schemaPrivileges+" ON SCHEMA "+name+" TO "+role, "GRANT "+data+" ON ALL TABLES IN SCHEMA "+name+" TO "+role)
		if permission == "read_write" {
			statements = append(statements, "GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA "+name+" TO "+role, "GRANT EXECUTE ON ALL ROUTINES IN SCHEMA "+name+" TO "+role)
		}
		if permission == "data_write" {
			statements = append(statements, "GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA "+name+" TO "+role)
		}
		for _, statement := range statements {
			if _, err = tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func checkPostgresOwnedRole(ctx context.Context, db *sql.DB, username, database string, login bool) error {
	var unsafe bool
	err := db.QueryRowContext(ctx, `SELECT rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls OR rolcanlogin <> $2 FROM pg_roles WHERE rolname=$1`, username, login).Scan(&unsafe)
	if err != nil || unsafe {
		return failure("ownership_invalid", "Owned database role has unexpected instance privileges", 412)
	}
	var count int
	err = db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM pg_auth_members WHERE member=(SELECT oid FROM pg_roles WHERE rolname=$1) OR roleid=(SELECT oid FROM pg_roles WHERE rolname=$1)) + (SELECT count(*) FROM pg_database WHERE datdba=(SELECT oid FROM pg_roles WHERE rolname=$1) AND datname<>$2) + (SELECT count(*) FROM pg_tablespace WHERE spcowner=(SELECT oid FROM pg_roles WHERE rolname=$1))`, username, database).Scan(&count)
	if err != nil || count != 0 {
		return failure("ownership_invalid", "Database role has memberships or shared ownership outside this database", 412)
	}
	return nil
}
