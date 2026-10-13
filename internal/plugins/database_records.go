package plugins

import (
	"context"
	"database/sql"
	"encoding/json"
	"go.mongodb.org/mongo-driver/v2/bson"
	"os"
	"path/filepath"
	"strings"
)

func (e *PrivilegedExecutor) readManagedRecords(instance Instance) ([]managedDatabaseRecord, error) {
	entries, err := os.ReadDir(filepath.Join(e.config.StateDir, "private"))
	if err != nil {
		return nil, err
	}
	records := []managedDatabaseRecord{}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "dbuser_") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var record managedDatabaseRecord
		id := strings.TrimSuffix(entry.Name(), ".json")
		if err = e.load("private", id, &record); err != nil {
			return nil, err
		}
		if record.InstanceID != instance.ID {
			continue
		}
		if id != managedDatabaseKey(record.InstanceID, record.Database) || record.Engine != instance.PluginID {
			return nil, failure("ownership_invalid", "Managed database receipt is inconsistent", 412)
		}
		records = append(records, record)
		if len(records) > 10000 {
			return nil, failure("result_limit", "Managed database list exceeds its bound", 413)
		}
	}
	return records, nil
}
func (e *PrivilegedExecutor) managedDiagnostics(ctx context.Context, instance Instance, command string) (any, error) {
	if instance.PluginID == "mongodb" {
		client, err := e.managedMongo(ctx, instance)
		if err != nil {
			return nil, err
		}
		defer client.Disconnect(context.WithoutCancel(ctx))
		if command == "replication_status" {
			var status bson.M
			if err = client.Database("admin").RunCommand(ctx, bson.D{{Key: "replSetGetStatus", Value: 1}}).Decode(&status); err != nil {
				return nil, failure("replication_not_configured", "MongoDB replica-set status is not available on this instance", 409)
			}
			delete(status, "optimes")
			return status, nil
		}
		var status struct {
			Version     string  `bson:"version"`
			Uptime      float64 `bson:"uptime"`
			Connections bson.M  `bson:"connections"`
			Opcounters  bson.M  `bson:"opcounters"`
		}
		if err = client.Database("admin").RunCommand(ctx, bson.D{{Key: "serverStatus", Value: 1}}).Decode(&status); err != nil {
			return nil, failure("query_failed", "MongoDB diagnostics query failed", 502)
		}
		return map[string]any{"version": status.Version, "uptime_seconds": status.Uptime, "connections": status.Connections, "operations": status.Opcounters}, nil
	}
	db, err := e.openManagedAdmin(ctx, instance)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	queries := map[string]string{}
	if instance.PluginID == "postgresql" {
		if command == "replication_status" {
			queries["senders"] = "SELECT pid,application_name,client_addr::text,state,sync_state,write_lag::text,flush_lag::text,replay_lag::text FROM pg_stat_replication"
			queries["receivers"] = "SELECT status,slot_name,sender_host,sender_port FROM pg_stat_wal_receiver"
			queries["slots"] = "SELECT slot_name,slot_type,active,restart_lsn::text FROM pg_replication_slots"
		} else {
			queries["version"] = "SELECT version() AS version"
			queries["connections"] = "SELECT datname,state,count(*) AS count FROM pg_stat_activity GROUP BY datname,state"
			queries["sizes"] = "SELECT datname,pg_database_size(oid) AS bytes FROM pg_database WHERE datistemplate=false"
		}
	}
	if instance.PluginID != "postgresql" {
		if command == "replication_status" {
			queries["replica"] = "SHOW SLAVE STATUS"
			if instance.PluginID != "mariadb" {
				queries["replica"] = "SHOW REPLICA STATUS"
			}
			queries["binary_logs"] = "SHOW BINARY LOGS"
		} else {
			queries["version"] = "SELECT VERSION() AS version"
			queries["variables"] = "SHOW GLOBAL VARIABLES WHERE Variable_name IN ('max_connections','innodb_buffer_pool_size','binlog_format','server_id','log_bin')"
			queries["connections"] = "SHOW GLOBAL STATUS WHERE Variable_name IN ('Threads_connected','Threads_running','Uptime','Queries','Slow_queries')"
		}
	}
	output := map[string]any{"instance_id": instance.ID, "engine": instance.PluginID}
	for name, query := range queries {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return nil, failure("query_failed", "Database version does not expose the requested diagnostics or replication state", 502)
		}
		result, err := boundedDatabaseRows(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		output[name] = result
	}
	return output, nil
}
func boundedDatabaseRows(rows *sql.Rows) ([]map[string]any, error) {
	columns, err := rows.Columns()
	if err != nil || len(columns) > 256 {
		return nil, failure("result_limit", "Database result columns exceed limits", 413)
	}
	items := []map[string]any{}
	bytes := 0
	for rows.Next() {
		if len(items) >= 1000 {
			return nil, failure("result_limit", "Database diagnostic result exceeds 1000 rows", 413)
		}
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err = rows.Scan(dest...); err != nil {
			return nil, failure("query_failed", "Database diagnostics could not be decoded", 502)
		}
		item := map[string]any{}
		for i, v := range values {
			if strings.Contains(strings.ToLower(columns[i]), "password") {
				continue
			}
			if raw, ok := v.([]byte); ok {
				if len(raw) > 128<<10 {
					return nil, failure("result_limit", "Database diagnostic field exceeds limits", 413)
				}
				v = string(raw)
			}
			item[columns[i]] = exactSQLInteger(v)
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, failure("query_failed", "Database diagnostic result was invalid", 502)
		}
		bytes += len(encoded)
		if bytes > 1<<20 {
			return nil, failure("result_limit", "Database diagnostic response exceeds one MiB", 413)
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		return nil, failure("query_failed", "Database diagnostics failed or were canceled", 502)
	}
	return items, nil
}
