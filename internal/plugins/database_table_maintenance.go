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
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

const (
	databaseTableMaintenanceOperation       = "database_table_maintenance_action"
	databaseTableMaintenancePreviewLifetime = 10 * time.Minute
	databaseTableMaintenanceMaxTables       = 2000
	databaseTableMaintenanceListLimit       = 200
	databaseTableMaintenanceMaxSelection    = 25
	databaseTableMaintenanceMaxMessages     = 16
	databaseTableMaintenanceMaxMessageBytes = 512
	databaseTableMaintenanceMaxDDLBytes     = 1 << 20
)

var (
	databaseTableMaintenanceDigestPattern  = regexp.MustCompile("^[0-9a-f]{64}$")
	databaseTableMaintenancePreviewPattern = regexp.MustCompile("^plan_[0-9a-f]{48}$")
	databaseTableMaintenanceTaskPattern    = regexp.MustCompile("^[0-9a-f]{32}$")
)

// DatabaseTableMaintenanceInput is a closed protocol. A caller can select a
// concrete table only from a freshly inventoried owned database; it cannot
// supply SQL, a path, credentials, or a table definition.
type DatabaseTableMaintenanceInput struct {
	Command                  string   `json:"command"`
	InstanceID               string   `json:"instance_id"`
	Database                 string   `json:"database"`
	Limit                    int      `json:"limit,omitempty"`
	After                    string   `json:"after,omitempty"`
	Operation                string   `json:"operation,omitempty"`
	Tables                   []string `json:"tables,omitempty"`
	ExpectedInstanceRevision int64    `json:"expected_instance_revision,omitempty"`
	ExpectedDatabaseRevision int64    `json:"expected_database_revision,omitempty"`
	ExpectedSchemaSHA256     string   `json:"expected_schema_sha256,omitempty"`
	PreviewID                string   `json:"preview_id,omitempty"`
	SnapshotSHA256           string   `json:"snapshot_sha256,omitempty"`
	ConfirmMaintenance       bool     `json:"confirm_maintenance,omitempty"`
}

type DatabaseTableMaintenanceTable struct {
	Name          string `json:"name"`
	Type          string `json:"type"`
	Engine        string `json:"engine,omitempty"`
	Collation     string `json:"collation,omitempty"`
	ApproxRows    uint64 `json:"approx_rows,omitempty"`
	DataBytes     uint64 `json:"data_bytes,omitempty"`
	IndexBytes    uint64 `json:"index_bytes,omitempty"`
	FreeBytes     uint64 `json:"free_bytes,omitempty"`
	EstimateKnown bool   `json:"estimate_known"`
	createOptions string `json:"-"`
}

type DatabaseTableMaintenanceList struct {
	InstanceID                string                          `json:"instance_id"`
	Database                  string                          `json:"database"`
	Items                     []DatabaseTableMaintenanceTable `json:"items"`
	Total                     int                             `json:"total"`
	Truncated                 bool                            `json:"truncated"`
	NextAfter                 string                          `json:"next_after,omitempty"`
	SchemaSHA256              string                          `json:"schema_sha256"`
	InstanceRevision          int64                           `json:"instance_revision"`
	DatabaseRevision          int64                           `json:"database_revision"`
	ReplicationSourceAttached bool                            `json:"replication_source_attached"`
}

type DatabaseTableMaintenancePreview struct {
	PreviewID             string                          `json:"preview_id"`
	ExpiresAt             time.Time                       `json:"expires_at"`
	InstanceID            string                          `json:"instance_id"`
	Database              string                          `json:"database"`
	Engine                string                          `json:"engine"`
	Operation             string                          `json:"operation"`
	Tables                []DatabaseTableMaintenanceTable `json:"tables"`
	TableCount            int                             `json:"table_count"`
	SchemaSHA256          string                          `json:"schema_sha256"`
	SnapshotSHA256        string                          `json:"snapshot_sha256"`
	InstanceRevision      int64                           `json:"instance_revision"`
	DatabaseRevision      int64                           `json:"database_revision"`
	EstimatedBytes        uint64                          `json:"estimated_bytes"`
	EstimateKnown         bool                            `json:"estimate_known"`
	MayNeedComparableTemp bool                            `json:"may_need_comparable_temporary_space"`
	LockImpact            string                          `json:"lock_impact"`
	ReplicaImpact         string                          `json:"replica_impact"`
	Warnings              []string                        `json:"warnings"`
	ConfirmationRequired  bool                            `json:"confirmation_required"`
}

type DatabaseTableMaintenanceMessage struct {
	Type string `json:"msg_type"`
	Text string `json:"msg_text"`
}

type DatabaseTableMaintenanceItemResult struct {
	Name     string                            `json:"name"`
	Engine   string                            `json:"engine"`
	State    string                            `json:"state"`
	Messages []DatabaseTableMaintenanceMessage `json:"messages"`
}

type DatabaseTableMaintenanceResult struct {
	PreviewID                 string                               `json:"preview_id"`
	TaskID                    string                               `json:"task_id,omitempty"`
	InstanceID                string                               `json:"instance_id"`
	Database                  string                               `json:"database"`
	Engine                    string                               `json:"engine"`
	Operation                 string                               `json:"operation"`
	State                     string                               `json:"state"`
	InstanceRevision          int64                                `json:"instance_revision"`
	DatabaseRevision          int64                                `json:"database_revision"`
	SchemaSHA256              string                               `json:"schema_sha256"`
	StartedAt                 time.Time                            `json:"started_at,omitempty"`
	FinishedAt                time.Time                            `json:"finished_at,omitempty"`
	CompletedCount            int                                  `json:"completed_count"`
	FailedCount               int                                  `json:"failed_count"`
	UnknownCount              int                                  `json:"outcome_unknown_count"`
	Items                     []DatabaseTableMaintenanceItemResult `json:"items"`
	ReplicationSourceAttached bool                                 `json:"replication_source_attached"`
}

type databaseTableMaintenanceInventory struct {
	Items        []DatabaseTableMaintenanceTable
	SchemaSHA256 string
}

type databaseTableMaintenanceEntry struct {
	DatabaseTableMaintenanceTable
	DefinitionSHA256 string `json:"definition_sha256"`
}

type databaseTableMaintenancePreviewRecord struct {
	Version                   int                                  `json:"version"`
	PreviewID                 string                               `json:"preview_id"`
	InstanceID                string                               `json:"instance_id"`
	Database                  string                               `json:"database"`
	Engine                    string                               `json:"engine"`
	Operation                 string                               `json:"operation"`
	Tables                    []databaseTableMaintenanceEntry      `json:"tables"`
	SchemaSHA256              string                               `json:"schema_sha256"`
	SnapshotSHA256            string                               `json:"snapshot_sha256"`
	InstanceRevision          int64                                `json:"instance_revision"`
	DatabaseRevision          int64                                `json:"database_revision"`
	EstimatedBytes            uint64                               `json:"estimated_bytes"`
	EstimateKnown             bool                                 `json:"estimate_known"`
	MayNeedComparableTemp     bool                                 `json:"may_need_comparable_temporary_space"`
	LockImpact                string                               `json:"lock_impact"`
	ReplicaImpact             string                               `json:"replica_impact"`
	ReplicationSourceAttached bool                                 `json:"replication_source_attached"`
	CreatedAt                 time.Time                            `json:"created_at"`
	ExpiresAt                 time.Time                            `json:"expires_at"`
	State                     string                               `json:"state"`
	TaskID                    string                               `json:"task_id,omitempty"`
	StartedAt                 time.Time                            `json:"started_at,omitempty"`
	FinishedAt                time.Time                            `json:"finished_at,omitempty"`
	Results                   []DatabaseTableMaintenanceItemResult `json:"results,omitempty"`
}

func validateDatabaseTableMaintenanceInput(raw json.RawMessage) error {
	if len(raw) > 48<<10 {
		return failure("invalid_input", "Table maintenance descriptor exceeds its bound", 413)
	}
	var in DatabaseTableMaintenanceInput
	if err := parse(raw, &in); err != nil {
		return err
	}
	if !validInstanceID(in.InstanceID) || !validDatabaseName(in.Database) {
		return failure("invalid_input", "A concrete managed MySQL or MariaDB database is required", 400)
	}
	noPreview := in.PreviewID == "" && in.SnapshotSHA256 == "" && !in.ConfirmMaintenance
	noRevision := in.ExpectedInstanceRevision == 0 && in.ExpectedDatabaseRevision == 0
	noSchema := in.ExpectedSchemaSHA256 == ""
	switch in.Command {
	case "list":
		if in.Limit < 0 || in.Limit > databaseTableMaintenanceListLimit || !databaseTableMaintenanceValidCursor(in.After) || in.Operation != "" || len(in.Tables) != 0 || !noRevision || !noSchema || !noPreview {
			return failure("invalid_input", "Table listing accepts only a managed database and bounded page cursor", 400)
		}
	case "preview":
		if in.Limit != 0 || in.After != "" || (in.Operation != "optimize" && in.Operation != "repair") || len(in.Tables) < 1 || len(in.Tables) > databaseTableMaintenanceMaxSelection || in.ExpectedInstanceRevision < 1 || in.ExpectedDatabaseRevision < 1 || !databaseTableMaintenanceDigestPattern.MatchString(in.ExpectedSchemaSHA256) || !noPreview {
			return failure("invalid_input", "Maintenance preview requires a bounded exact table selection, current revisions and inventory digest", 400)
		}
		seen := map[string]bool{}
		for _, name := range in.Tables {
			if !databaseTableMaintenanceValidTableName(name) || seen[name] {
				return failure("invalid_input", "Selected table names must be distinct concrete names from the owned table inventory", 400)
			}
			seen[name] = true
		}
	case "run":
		if in.Limit != 0 || in.After != "" || in.Operation != "" || len(in.Tables) != 0 || !noSchema || !databaseTableMaintenancePreviewPattern.MatchString(in.PreviewID) || !databaseTableMaintenanceDigestPattern.MatchString(in.SnapshotSHA256) || in.ExpectedInstanceRevision < 1 || in.ExpectedDatabaseRevision < 1 || !in.ConfirmMaintenance {
			return failure("invalid_input", "Maintenance requires the exact current preview, revisions and explicit effect confirmation", 400)
		}
	case "inspect":
		if in.Limit != 0 || in.After != "" || in.Operation != "" || len(in.Tables) != 0 || !noRevision || !noSchema || in.SnapshotSHA256 != "" || in.ConfirmMaintenance || !databaseTableMaintenancePreviewPattern.MatchString(in.PreviewID) {
			return failure("invalid_input", "Maintenance inspection accepts only an owned preview ID and database identity", 400)
		}
	default:
		return failure("invalid_input", "Unsupported typed table maintenance command", 400)
	}
	return nil
}

func databaseTableMaintenanceValidCursor(value string) bool {
	return value == "" || databaseTableMaintenanceValidTableName(value)
}

func databaseTableMaintenanceValidTableName(value string) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= 64 && len(value) <= 256 && !strings.ContainsRune(value, '\x00')
}

func (e *PrivilegedExecutor) validateDatabaseTableMaintenance(raw json.RawMessage) error {
	return validateDatabaseTableMaintenanceInput(raw)
}

// databaseTableMaintenance is the Agent-side typed boundary. `request.JobID`
// is required for run so the durable table-operation receipt can identify the
// one task allowed to consume a preview.
func (e *PrivilegedExecutor) databaseTableMaintenance(ctx context.Context, request agent.Request) (any, error) {
	if request.Operation != databaseTableMaintenanceOperation {
		return nil, agent.ErrUnauthorized
	}
	if err := e.validateDatabaseTableMaintenance(request.Input); err != nil {
		return nil, err
	}
	var in DatabaseTableMaintenanceInput
	if err := parse(request.Input, &in); err != nil {
		return nil, err
	}
	if in.Command == "inspect" {
		return e.inspectDatabaseTableMaintenance(ctx, in)
	}
	if in.Command == "run" {
		return e.runDatabaseTableMaintenance(ctx, request, in)
	}
	unlock, err := e.lockContext(ctx, in.InstanceID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err = e.ensureInstallFinalized(ctx, in.InstanceID); err != nil {
		return nil, err
	}
	if err = e.ensureDatabaseReplicationJournalFinalized(ctx, in.InstanceID); err != nil {
		return nil, err
	}
	instance, err := e.tableMaintenanceInstance(ctx, in.InstanceID)
	if err != nil {
		return nil, err
	}
	ownedDB, err := e.tableMaintenanceDatabaseReceipt(instance, in.Database)
	if err != nil {
		return nil, err
	}
	if in.Command == "preview" {
		if err = e.ensureDatabaseReplicationTargetWritable(ctx, instance.ID, in.Database); err != nil {
			return nil, err
		}
		return e.previewDatabaseTableMaintenance(ctx, instance, ownedDB, in)
	}
	if in.Command != "list" {
		return nil, failure("invalid_input", "Unsupported table maintenance command", 400)
	}
	return e.listDatabaseTablesForMaintenance(ctx, instance, ownedDB, in)
}

func (e *PrivilegedExecutor) tableMaintenanceInstance(ctx context.Context, id string) (Instance, error) {
	instance, err := e.get(ctx, id, false)
	if err != nil {
		return Instance{}, err
	}
	if !databaseBinlogSupportedEngine(instance.PluginID) || instance.Ownership != "managed" || instance.Paths != e.instancePaths(instance.PluginID, instance.InstanceID) ||
		instance.Service.ID != instance.ID || instance.Service.PluginID != instance.PluginID || instance.Service.InstanceID != instance.InstanceID ||
		instance.Service.Unit != "powerlinux-plugin-"+instance.ID+".service" || instance.Service.ConfigPath != instance.Paths.ConfigDir ||
		instance.Service.DataPath != instance.Paths.DataDir || instance.Service.Ownership != "managed" || instance.Service.Program == "" {
		return Instance{}, failure("ownership_invalid", "Table maintenance requires the exact owned managed MySQL or MariaDB runtime", 412)
	}
	if instance.Status == "uninstalled" {
		return Instance{}, failure("runtime_unavailable", "Database software is uninstalled", 412)
	}
	return instance, nil
}

func (e *PrivilegedExecutor) tableMaintenanceDatabaseReceipt(instance Instance, database string) (managedDatabaseRecord, error) {
	var record managedDatabaseRecord
	if err := e.load("private", managedDatabaseKey(instance.ID, database), &record); err != nil {
		if core.ErrorCode(err) == "not_found" {
			return record, failure("managed_database_required", "Table maintenance requires an active managed database ownership receipt", 412)
		}
		return record, err
	}
	if record.InstanceID != instance.ID || record.Database != database || record.Engine != instance.PluginID {
		return record, failure("ownership_invalid", "Managed database receipt does not match this instance and schema", 412)
	}
	if record.State != "active" || record.Revision < 1 {
		return record, failure("operation_recovery_required", "Managed database ownership is unfinished; inspect its receipt before maintenance", 409)
	}
	return record, nil
}

func (e *PrivilegedExecutor) openTableMaintenanceAdmin(ctx context.Context, instance Instance, database string) (*sql.DB, error) {
	if !validDatabaseName(database) {
		return nil, failure("invalid_input", "A valid managed database name is required", 400)
	}
	db, err := e.openManagedAdmin(ctx, instance)
	if err != nil {
		return nil, err
	}
	var present int
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME=?", database).Scan(&present); err != nil {
		db.Close()
		return nil, failure("database_unavailable", "Owned database schema could not be verified", 502)
	}
	if present != 1 {
		db.Close()
		return nil, failure("managed_database_missing", "The owned database schema is absent from the live server catalog", 412)
	}
	return db, nil
}

func (e *PrivilegedExecutor) listDatabaseTablesForMaintenance(ctx context.Context, instance Instance, receipt managedDatabaseRecord, in DatabaseTableMaintenanceInput) (any, error) {
	db, err := e.openTableMaintenanceAdmin(ctx, instance, in.Database)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	inventory, err := readDatabaseTableMaintenanceInventory(ctx, db, in.Database)
	if err != nil {
		return nil, err
	}
	attached, err := e.databaseTableMaintenanceSourceAttached(ctx, instance.ID, in.Database)
	if err != nil {
		return nil, err
	}
	limit := in.Limit
	if limit == 0 {
		limit = databaseTableMaintenanceListLimit
	}
	start := sort.Search(len(inventory.Items), func(i int) bool { return inventory.Items[i].Name > in.After })
	items := inventory.Items[start:]
	truncated := len(items) > limit
	nextAfter := ""
	if truncated {
		items = items[:limit]
		nextAfter = items[len(items)-1].Name
	}
	return DatabaseTableMaintenanceList{InstanceID: instance.ID, Database: receipt.Database, Items: items, Total: len(inventory.Items), Truncated: truncated, NextAfter: nextAfter, SchemaSHA256: inventory.SchemaSHA256, InstanceRevision: instance.Revision, DatabaseRevision: receipt.Revision, ReplicationSourceAttached: attached}, nil
}

func (e *PrivilegedExecutor) previewDatabaseTableMaintenance(ctx context.Context, instance Instance, receipt managedDatabaseRecord, in DatabaseTableMaintenanceInput) (any, error) {
	if instance.Revision != in.ExpectedInstanceRevision || receipt.Revision != in.ExpectedDatabaseRevision {
		return nil, failure("revision_conflict", "Managed instance or database changed; reload the table list before previewing", 412)
	}
	db, err := e.openTableMaintenanceAdmin(ctx, instance, in.Database)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	inventory, err := readDatabaseTableMaintenanceInventory(ctx, db, in.Database)
	if err != nil {
		return nil, err
	}
	if inventory.SchemaSHA256 != in.ExpectedSchemaSHA256 {
		return nil, failure("revision_conflict", "Database table inventory changed; refresh it before creating a maintenance preview", 412)
	}
	byName := make(map[string]DatabaseTableMaintenanceTable, len(inventory.Items))
	for _, table := range inventory.Items {
		byName[table.Name] = table
	}
	record := databaseTableMaintenancePreviewRecord{Version: 1, InstanceID: instance.ID, Database: in.Database, Engine: instance.PluginID, Operation: in.Operation, SchemaSHA256: inventory.SchemaSHA256, InstanceRevision: instance.Revision, DatabaseRevision: receipt.Revision, EstimateKnown: true, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(databaseTableMaintenancePreviewLifetime), State: "ready"}
	for _, name := range in.Tables {
		table, found := byName[name]
		if !found || table.Type != "BASE TABLE" {
			return nil, failure("table_not_owned", "Every selected object must still be an exact base table in the owned database", 412)
		}
		if err := validateDatabaseTableMaintenanceEngine(in.Operation, table.Engine); err != nil {
			return nil, err
		}
		definition, definitionErr := readDatabaseTableMaintenanceDefinition(ctx, db, in.Database, table.Name)
		if definitionErr != nil {
			return nil, definitionErr
		}
		definitionHash := sha256.Sum256([]byte(definition))
		record.Tables = append(record.Tables, databaseTableMaintenanceEntry{DatabaseTableMaintenanceTable: table, DefinitionSHA256: hex.EncodeToString(definitionHash[:])})
		if !table.EstimateKnown || ^uint64(0)-record.EstimatedBytes < table.DataBytes || ^uint64(0)-(record.EstimatedBytes+table.DataBytes) < table.IndexBytes {
			record.EstimateKnown = false
		} else {
			record.EstimatedBytes += table.DataBytes + table.IndexBytes
		}
	}
	if !record.EstimateKnown {
		record.EstimatedBytes = 0
	}
	if in.Operation == "optimize" {
		record.MayNeedComparableTemp = true
		record.LockImpact = "OPTIMIZE may rebuild a table and can wait for or hold metadata/table locks; exact algorithm and blocking behavior vary by server version and engine."
	} else {
		record.LockImpact = "REPAIR may lock the table and is only a limited MyISAM repair attempt; it cannot guarantee recovery of damaged data."
	}
	attached, err := e.databaseTableMaintenanceSourceAttached(ctx, instance.ID, in.Database)
	if err != nil {
		return nil, err
	}
	record.ReplicationSourceAttached = attached
	if attached {
		record.ReplicaImpact = "This database is attached to a managed replication source. Table maintenance is binary logged and can replay on its target, adding replication lag and disk or lock load."
	} else {
		record.ReplicaImpact = "No managed replication source pair was found for this database. Unmanaged or external replicas are not discoverable here; server replication may still propagate this operation."
	}
	previewID, err := randomPlanID()
	if err != nil {
		return nil, failure("random_unavailable", "A protected table maintenance preview ID could not be created", 500)
	}
	record.PreviewID = previewID
	record.SnapshotSHA256, err = databaseTableMaintenanceSnapshotDigest(record)
	if err != nil {
		return nil, failure("snapshot_failed", "Table maintenance preview could not be fingerprinted", 500)
	}
	if err = e.save("private", record.PreviewID, record); err != nil {
		return nil, failure("preview_save_failed", "Protected table maintenance preview could not be stored", 500)
	}
	return publicDatabaseTableMaintenancePreview(record), nil
}

func validateDatabaseTableMaintenanceEngine(operation, engine string) error {
	switch strings.ToLower(engine) {
	case "innodb":
		if operation == "repair" {
			return failure("maintenance_unsupported", "REPAIR TABLE is not supported for InnoDB; no repair statement was sent", 412)
		}
		if operation == "optimize" {
			return nil
		}
	case "myisam":
		if operation == "repair" || operation == "optimize" {
			return nil
		}
	}
	return failure("maintenance_unsupported", "This initial maintenance subset supports OPTIMIZE on InnoDB/MyISAM and REPAIR on MyISAM only", 412)
}

func readDatabaseTableMaintenanceInventory(ctx context.Context, db *sql.DB, database string) (databaseTableMaintenanceInventory, error) {
	var inventory databaseTableMaintenanceInventory
	rows, err := db.QueryContext(ctx, "SELECT TABLE_NAME, TABLE_TYPE, COALESCE(ENGINE,''), COALESCE(TABLE_COLLATION,''), COALESCE(CREATE_OPTIONS,''), TABLE_ROWS, DATA_LENGTH, INDEX_LENGTH, DATA_FREE FROM information_schema.TABLES WHERE TABLE_SCHEMA=? ORDER BY BINARY TABLE_NAME LIMIT ?", database, databaseTableMaintenanceMaxTables+1)
	if err != nil {
		return inventory, failure("table_inventory_failed", "Owned database table inventory could not be read", 502)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		if len(inventory.Items) >= databaseTableMaintenanceMaxTables {
			return inventory, failure("result_limit", "Table maintenance supports at most 2000 database objects in one schema", 413)
		}
		var item DatabaseTableMaintenanceTable
		var createOptions string
		var rowsEstimate, dataBytes, indexBytes, freeBytes sql.NullInt64
		if err = rows.Scan(&item.Name, &item.Type, &item.Engine, &item.Collation, &createOptions, &rowsEstimate, &dataBytes, &indexBytes, &freeBytes); err != nil {
			return inventory, failure("table_inventory_failed", "Owned database table inventory could not be decoded", 502)
		}
		if !databaseTableMaintenanceValidTableName(item.Name) || seen[item.Name] || (item.Type != "BASE TABLE" && item.Type != "VIEW") {
			return inventory, failure("table_inventory_invalid", "Database server returned a malformed or duplicate table identity", 502)
		}
		seen[item.Name] = true
		item.createOptions = createOptions
		if rowsEstimate.Valid && rowsEstimate.Int64 >= 0 {
			item.ApproxRows = uint64(rowsEstimate.Int64)
		}
		if dataBytes.Valid && dataBytes.Int64 >= 0 && indexBytes.Valid && indexBytes.Int64 >= 0 {
			item.EstimateKnown = true
			item.DataBytes = uint64(dataBytes.Int64)
			item.IndexBytes = uint64(indexBytes.Int64)
		}
		if freeBytes.Valid && freeBytes.Int64 >= 0 {
			item.FreeBytes = uint64(freeBytes.Int64)
		}
		inventory.Items = append(inventory.Items, item)
	}
	if err = rows.Err(); err != nil {
		return inventory, failure("table_inventory_failed", "Owned database table inventory was interrupted", 502)
	}
	if err = rows.Close(); err != nil {
		return inventory, failure("table_inventory_failed", "Owned database table inventory could not be finalized", 502)
	}
	sort.Slice(inventory.Items, func(i, j int) bool { return inventory.Items[i].Name < inventory.Items[j].Name })
	digest := sha256.New()
	for _, item := range inventory.Items {
		databaseReplicationHashFields(digest, item.Name, item.Type, strings.ToLower(item.Engine), item.Collation, item.createOptions)
	}
	inventory.SchemaSHA256 = hex.EncodeToString(digest.Sum(nil))
	return inventory, nil
}

func readDatabaseTableMaintenanceDefinition(ctx context.Context, db *sql.DB, database, table string) (string, error) {
	if !validDatabaseName(database) || !databaseTableMaintenanceValidTableName(table) {
		return "", failure("invalid_input", "Invalid owned table identity", 400)
	}
	statement := "SHOW CREATE TABLE " + quoteDatabaseMaintenanceIdentifier(database) + "." + quoteDatabaseMaintenanceIdentifier(table)
	// openManagedAdminCredential sets the driver's per-connection MaxAllowedPacket
	// to 2 MiB, and the vendored driver enforces that bound on received packets.
	// The 1 MiB check below is an application limit inside that hard transport cap;
	// neither limit changes the server's global max_allowed_packet setting.
	rows, err := db.QueryContext(ctx, statement)
	if err != nil {
		return "", failure("table_definition_failed", "Selected table definition could not be verified", 502)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil || len(columns) < 2 || len(columns) > 16 {
		return "", failure("table_definition_failed", "Database server returned an invalid table definition response", 502)
	}
	definitionIndex := -1
	nameIndex := -1
	for index, name := range columns {
		lower := strings.ToLower(strings.TrimSpace(name))
		if lower == "table" {
			nameIndex = index
		}
		if lower == "create table" {
			definitionIndex = index
		}
	}
	if definitionIndex < 0 || nameIndex < 0 || !rows.Next() {
		return "", failure("table_definition_failed", "Database server omitted the selected table definition", 502)
	}
	values := make([]any, len(columns))
	dest := make([]any, len(columns))
	for index := range values {
		dest[index] = &values[index]
	}
	if err = rows.Scan(dest...); err != nil {
		return "", failure("table_definition_failed", "Selected table definition could not be decoded", 502)
	}
	if maintenanceSQLText(values[nameIndex]) != table {
		return "", failure("table_definition_failed", "Database server returned a different table identity", 412)
	}
	definition := maintenanceSQLText(values[definitionIndex])
	if len(definition) == 0 || len(definition) > databaseTableMaintenanceMaxDDLBytes || rows.Next() {
		return "", failure("table_definition_invalid", "Selected table definition is empty, oversized or ambiguous", 413)
	}
	if err = rows.Err(); err != nil {
		return "", failure("table_definition_failed", "Selected table definition read was interrupted", 502)
	}
	return definition, nil
}

func quoteDatabaseMaintenanceIdentifier(value string) string {
	return "`" + strings.ReplaceAll(value, "`", "``") + "`"
}

func databaseTableMaintenanceStatement(database, table, operation string) (string, error) {
	if !validDatabaseName(database) || !databaseTableMaintenanceValidTableName(table) || (operation != "optimize" && operation != "repair") {
		return "", failure("invalid_input", "Invalid typed table maintenance target", 400)
	}
	command := "OPTIMIZE TABLE "
	if operation == "repair" {
		command = "REPAIR TABLE "
	}
	return command + quoteDatabaseMaintenanceIdentifier(database) + "." + quoteDatabaseMaintenanceIdentifier(table), nil
}

func readDatabaseTableMaintenanceResult(ctx context.Context, db *sql.DB, database, table, operation string) (string, []DatabaseTableMaintenanceMessage, error) {
	statement, err := databaseTableMaintenanceStatement(database, table, operation)
	if err != nil {
		return "", nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return "outcome_unknown", []DatabaseTableMaintenanceMessage{{Type: "error", Text: "Server result could not be confirmed; inspect the maintenance record before retrying."}}, nil
	}
	defer conn.Close()
	messages := []DatabaseTableMaintenanceMessage{}
	expectedTable := database + "." + table
	identityMismatch := false
	tooMany := false
	queryFailed := false
	protocolInvalid := false
	readErr := conn.Raw(func(raw any) error {
		queryer, ok := raw.(driver.QueryerContext)
		if !ok {
			protocolInvalid = true
			return driver.ErrBadConn
		}
		rows, queryErr := queryer.QueryContext(ctx, statement, nil)
		if queryErr != nil {
			queryFailed = true
			if errors.Is(queryErr, driver.ErrBadConn) {
				return driver.ErrBadConn
			}
			return nil
		}
		if rows == nil {
			protocolInvalid = true
			return driver.ErrBadConn
		}
		columns := rows.Columns()
		if len(columns) == 0 || len(columns) > 16 {
			protocolInvalid = true
			return driver.ErrBadConn
		}
		tableIndex, opIndex, typeIndex, textIndex := -1, -1, -1, -1
		for index, column := range columns {
			var target *int
			switch strings.ToLower(strings.TrimSpace(column)) {
			case "table":
				target = &tableIndex
			case "op":
				target = &opIndex
			case "msg_type":
				target = &typeIndex
			case "msg_text":
				target = &textIndex
			}
			if target != nil {
				if *target >= 0 {
					protocolInvalid = true
					return driver.ErrBadConn
				}
				*target = index
			}
		}
		if tableIndex < 0 || opIndex < 0 || typeIndex < 0 || textIndex < 0 {
			protocolInvalid = true
			return driver.ErrBadConn
		}
		for rowCount := 0; ; rowCount++ {
			values := make([]driver.Value, len(columns))
			if nextErr := rows.Next(values); nextErr != nil {
				if errors.Is(nextErr, io.EOF) {
					break
				}
				queryFailed = true
				return driver.ErrBadConn
			}
			if maintenanceSQLText(values[tableIndex]) != expectedTable || !strings.EqualFold(strings.TrimSpace(maintenanceSQLText(values[opIndex])), operation) {
				identityMismatch = true
			}
			if rowCount >= databaseTableMaintenanceMaxMessages {
				// Returning ErrBadConn from Raw makes database/sql close this
				// driver connection. Do not call Rows.Close here: MySQL drivers
				// may drain unread rows during Close, defeating the row cap.
				tooMany = true
				return driver.ErrBadConn
			}
			messages = append(messages, DatabaseTableMaintenanceMessage{Type: maintenanceBoundMessage(maintenanceSQLText(values[typeIndex])), Text: maintenanceBoundMessage(maintenanceSQLText(values[textIndex]))})
		}
		if closeErr := rows.Close(); closeErr != nil {
			queryFailed = true
			return driver.ErrBadConn
		}
		return nil
	})
	if tooMany {
		return "outcome_unknown", messages, nil
	}
	if protocolInvalid {
		return "outcome_unknown", maintenanceTableResultNotice(messages, "Server returned an unrecognized maintenance result; inspect before retrying."), nil
	}
	if queryFailed || readErr != nil {
		return "outcome_unknown", maintenanceTableResultNotice(messages, "Server response was interrupted; inspect before retrying."), nil
	}
	if identityMismatch {
		return "outcome_unknown", maintenanceTableResultNotice(messages, "Server response did not match the selected table and operation; inspect before retrying."), nil
	}
	if len(messages) == 0 {
		return "outcome_unknown", maintenanceTableResultNotice(messages, "Server did not return a maintenance result; inspect before retrying."), nil
	}
	state, classification := classifyDatabaseTableMaintenanceMessages(operation, messages)
	if classification != "" && len(messages) < databaseTableMaintenanceMaxMessages {
		messages = append(messages, DatabaseTableMaintenanceMessage{Type: "policy", Text: classification})
	}
	return state, messages, nil
}

func maintenanceTableResultNotice(messages []DatabaseTableMaintenanceMessage, text string) []DatabaseTableMaintenanceMessage {
	if len(messages) >= databaseTableMaintenanceMaxMessages {
		return messages
	}
	return append(messages, DatabaseTableMaintenanceMessage{Type: "error", Text: text})
}

func classifyDatabaseTableMaintenanceMessages(operation string, messages []DatabaseTableMaintenanceMessage) (string, string) {
	finalStatusOK := false
	notSupported := false
	hasError := false
	fallback := false
	for index, message := range messages {
		typ := strings.ToLower(strings.TrimSpace(message.Type))
		text := strings.ToLower(strings.TrimSpace(message.Text))
		if typ == "error" {
			hasError = true
		}
		if index == len(messages)-1 && typ == "status" && strings.TrimSpace(message.Text) == "OK" {
			finalStatusOK = true
		}
		if strings.Contains(text, "doesn't support") || strings.Contains(text, "does not support") || strings.Contains(text, "not supported") {
			notSupported = true
			if operation == "optimize" && (strings.Contains(text, "recreate") || strings.Contains(text, "re-create")) {
				fallback = true
			}
		}
	}
	if hasError || (notSupported && !(operation == "optimize" && fallback && finalStatusOK)) {
		if notSupported {
			return "failed", "Server reported that this table engine does not support the requested operation."
		}
		return "failed", "Database server reported an error for this table."
	}
	if finalStatusOK {
		return "completed", ""
	}
	return "outcome_unknown", "Server did not return a final status=OK row; inspect before retrying."
}

func maintenanceSQLText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		return fmt.Sprint(typed)
	}
}

func maintenanceBoundMessage(value string) string {
	value = strings.ToValidUTF8(value, "�")
	var builder strings.Builder
	for _, r := range value {
		if r == '\n' || r == '\r' || r == '\t' || (r >= 0x20 && r != 0x7f) {
			builder.WriteRune(r)
		}
		if builder.Len() >= databaseTableMaintenanceMaxMessageBytes {
			break
		}
	}
	result := builder.String()
	if len(result) > databaseTableMaintenanceMaxMessageBytes {
		result = result[:databaseTableMaintenanceMaxMessageBytes]
		for !utf8.ValidString(result) {
			result = result[:len(result)-1]
		}
	}
	return result
}

func (e *PrivilegedExecutor) databaseTableMaintenanceSourceAttached(ctx context.Context, instanceID, database string) (bool, error) {
	index, err := e.loadDatabaseReplicationIndex(instanceID)
	if err != nil {
		return false, err
	}
	for _, pairID := range index.PairIDs {
		pair, loadErr := e.loadDatabaseReplication(pairID)
		if loadErr != nil {
			return false, loadErr
		}
		if pair == nil || (pair.SourceInstanceID != instanceID && pair.TargetInstanceID != instanceID) {
			return false, failure("ownership_invalid", "Replication index contains an unrelated pair", 412)
		}
		if pair.State != "removed" && pair.SourceInstanceID == instanceID && pair.Database == database {
			return true, nil
		}
	}
	return false, nil
}

func databaseTableMaintenanceSnapshotDigest(record databaseTableMaintenancePreviewRecord) (string, error) {
	identity := struct {
		Version                   int                             `json:"version"`
		PreviewID                 string                          `json:"preview_id"`
		InstanceID                string                          `json:"instance_id"`
		Database                  string                          `json:"database"`
		Engine                    string                          `json:"engine"`
		Operation                 string                          `json:"operation"`
		Tables                    []databaseTableMaintenanceEntry `json:"tables"`
		SchemaSHA256              string                          `json:"schema_sha256"`
		InstanceRevision          int64                           `json:"instance_revision"`
		DatabaseRevision          int64                           `json:"database_revision"`
		ReplicationSourceAttached bool                            `json:"replication_source_attached"`
	}{record.Version, record.PreviewID, record.InstanceID, record.Database, record.Engine, record.Operation, record.Tables, record.SchemaSHA256, record.InstanceRevision, record.DatabaseRevision, record.ReplicationSourceAttached}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func publicDatabaseTableMaintenancePreview(record databaseTableMaintenancePreviewRecord) DatabaseTableMaintenancePreview {
	warnings := []string{"Definitions are checked again immediately before each table operation, but an independent administrator can change a table between that check and the server statement; refresh if schema changes are underway."}
	if record.Operation == "optimize" {
		warnings = append(warnings, "OPTIMIZE can rebuild tables and may temporarily use disk space comparable to their current size; metadata estimates do not guarantee reclaimable space.")
	} else {
		warnings = append(warnings, "REPAIR is a limited MyISAM recovery attempt. It can lock the table and cannot guarantee recovery of damaged data.")
	}
	if record.ReplicationSourceAttached {
		warnings = append(warnings, "This managed source may replicate the maintenance statement; expect target load and possible lag.")
	} else {
		warnings = append(warnings, "Unmanaged or external replicas cannot be discovered by this preview and may still receive the statement.")
	}
	tables := make([]DatabaseTableMaintenanceTable, len(record.Tables))
	for index, item := range record.Tables {
		tables[index] = item.DatabaseTableMaintenanceTable
	}
	return DatabaseTableMaintenancePreview{PreviewID: record.PreviewID, ExpiresAt: record.ExpiresAt, InstanceID: record.InstanceID, Database: record.Database, Engine: record.Engine, Operation: record.Operation, Tables: tables, TableCount: len(tables), SchemaSHA256: record.SchemaSHA256, SnapshotSHA256: record.SnapshotSHA256, InstanceRevision: record.InstanceRevision, DatabaseRevision: record.DatabaseRevision, EstimatedBytes: record.EstimatedBytes, EstimateKnown: record.EstimateKnown, MayNeedComparableTemp: record.MayNeedComparableTemp, LockImpact: record.LockImpact, ReplicaImpact: record.ReplicaImpact, Warnings: warnings, ConfirmationRequired: true}
}

func publicDatabaseTableMaintenanceResult(record databaseTableMaintenancePreviewRecord) DatabaseTableMaintenanceResult {
	results := append([]DatabaseTableMaintenanceItemResult(nil), record.Results...)
	state := record.State
	if state == "running" {
		state = "not_started"
		hasExecuting := false
		hasTerminal := false
		for index := range results {
			if results[index].State == "executing" {
				hasExecuting = true
				results[index].State = "outcome_unknown"
				results[index].Messages = append(results[index].Messages, DatabaseTableMaintenanceMessage{Type: "policy", Text: "Task stopped before confirming the server result; this table will not be replayed automatically."})
			} else if results[index].State == "pending" || results[index].State == "checking" {
				results[index].State = "not_started"
			} else if results[index].State == "completed" || results[index].State == "failed" || results[index].State == "outcome_unknown" {
				hasTerminal = true
			}
		}
		if hasExecuting {
			state = "outcome_unknown"
		} else if hasTerminal {
			state = databaseTableMaintenanceAggregateState(results)
		}
	}
	out := DatabaseTableMaintenanceResult{PreviewID: record.PreviewID, TaskID: record.TaskID, InstanceID: record.InstanceID, Database: record.Database, Engine: record.Engine, Operation: record.Operation, State: state, InstanceRevision: record.InstanceRevision, DatabaseRevision: record.DatabaseRevision, SchemaSHA256: record.SchemaSHA256, StartedAt: record.StartedAt, FinishedAt: record.FinishedAt, Items: results, ReplicationSourceAttached: record.ReplicationSourceAttached}
	for _, item := range results {
		switch item.State {
		case "completed":
			out.CompletedCount++
		case "failed":
			out.FailedCount++
		case "outcome_unknown", "executing":
			out.UnknownCount++
		}
	}
	return out
}

func (e *PrivilegedExecutor) inspectDatabaseTableMaintenance(ctx context.Context, in DatabaseTableMaintenanceInput) (any, error) {
	unlock, err := e.lockContext(ctx, in.InstanceID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err = e.ensureDatabaseReplicationJournalFinalized(ctx, in.InstanceID); err != nil {
		return nil, err
	}
	var record databaseTableMaintenancePreviewRecord
	if err = e.load("private", in.PreviewID, &record); err != nil {
		return nil, err
	}
	if record.PreviewID != in.PreviewID || record.InstanceID != in.InstanceID || record.Database != in.Database || record.Version != 1 {
		return nil, failure("ownership_invalid", "Protected maintenance preview does not match the requested database", 412)
	}
	return publicDatabaseTableMaintenanceResult(record), nil
}

func (e *PrivilegedExecutor) runDatabaseTableMaintenance(ctx context.Context, request agent.Request, in DatabaseTableMaintenanceInput) (any, error) {
	if !databaseTableMaintenanceTaskPattern.MatchString(request.JobID) {
		return nil, failure("invalid_task", "Durable maintenance task identity is unavailable", 400)
	}
	unlock, err := e.lockContext(ctx, in.InstanceID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err = e.ensureInstallFinalized(ctx, in.InstanceID); err != nil {
		return nil, err
	}
	if err = e.ensureDatabaseReplicationJournalFinalized(ctx, in.InstanceID); err != nil {
		return nil, err
	}
	instance, err := e.tableMaintenanceInstance(ctx, in.InstanceID)
	if err != nil {
		return nil, err
	}
	receipt, err := e.tableMaintenanceDatabaseReceipt(instance, in.Database)
	if err != nil {
		return nil, err
	}
	if instance.Revision != in.ExpectedInstanceRevision || receipt.Revision != in.ExpectedDatabaseRevision {
		return nil, failure("revision_conflict", "Managed instance or database changed after preview; create a fresh table maintenance preview", 412)
	}
	if err = e.ensureDatabaseReplicationTargetWritable(ctx, instance.ID, in.Database); err != nil {
		return nil, err
	}
	db, err := e.openTableMaintenanceAdmin(ctx, instance, in.Database)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var record databaseTableMaintenancePreviewRecord
	if err = e.load("private", in.PreviewID, &record); err != nil {
		return nil, err
	}
	if record.PreviewID != in.PreviewID || record.InstanceID != instance.ID || record.Database != in.Database || record.Engine != instance.PluginID || record.Version != 1 {
		return nil, failure("ownership_invalid", "Protected maintenance preview does not match the managed runtime", 412)
	}
	if record.SnapshotSHA256 != in.SnapshotSHA256 || record.InstanceRevision != in.ExpectedInstanceRevision || record.DatabaseRevision != in.ExpectedDatabaseRevision {
		return nil, failure("revision_conflict", "Maintenance preview snapshot or managed revisions changed", 412)
	}
	if record.State != "ready" {
		return nil, failure("preview_unavailable", "Maintenance preview is expired or has already been consumed; create a new preview", 409)
	}
	if time.Now().UTC().After(record.ExpiresAt) {
		record.State = "expired"
		_ = e.save("private", record.PreviewID, record)
		return nil, failure("preview_unavailable", "Maintenance preview expired; create a fresh table list and preview", 409)
	}
	digest, err := databaseTableMaintenanceSnapshotDigest(record)
	if err != nil || digest != record.SnapshotSHA256 {
		return nil, failure("ownership_invalid", "Protected maintenance preview fingerprint does not match its contents", 412)
	}
	inventory, err := readDatabaseTableMaintenanceInventory(ctx, db, in.Database)
	if err != nil {
		return nil, err
	}
	if inventory.SchemaSHA256 != record.SchemaSHA256 {
		return nil, failure("revision_conflict", "Database table inventory changed after preview; create a fresh preview", 412)
	}
	for _, table := range record.Tables {
		if err = validateDatabaseTableMaintenanceEngine(record.Operation, table.Engine); err != nil {
			return nil, err
		}
		definition, definitionErr := readDatabaseTableMaintenanceDefinition(ctx, db, in.Database, table.Name)
		if definitionErr != nil {
			return nil, definitionErr
		}
		current := sha256.Sum256([]byte(definition))
		if !strings.EqualFold(hex.EncodeToString(current[:]), table.DefinitionSHA256) {
			return nil, failure("revision_conflict", "A selected table definition changed after preview; create a fresh preview", 412)
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	record.State = "running"
	record.TaskID = request.JobID
	record.StartedAt = time.Now().UTC()
	record.Results = make([]DatabaseTableMaintenanceItemResult, len(record.Tables))
	for index, table := range record.Tables {
		record.Results[index] = DatabaseTableMaintenanceItemResult{Name: table.Name, Engine: table.Engine, State: "pending", Messages: []DatabaseTableMaintenanceMessage{}}
	}
	if err = e.save("private", record.PreviewID, record); err != nil {
		return nil, failure("operation_recovery_required", "Maintenance task could not claim its durable preview; no SQL was sent", 409)
	}
	for index, table := range record.Tables {
		if err = ctx.Err(); err != nil {
			record.Results[index].State = "not_started"
			for remaining := index + 1; remaining < len(record.Results); remaining++ {
				record.Results[remaining].State = "not_started"
			}
			record.State = "canceled"
			record.FinishedAt = time.Now().UTC()
			if saveErr := e.save("private", record.PreviewID, record); saveErr != nil {
				return nil, failure("operation_recovery_required", "Task was canceled and its table progress could not be finalized; inspect the protected preview before retrying", 409)
			}
			return nil, err
		}
		if err = validateDatabaseTableMaintenanceEngine(record.Operation, table.Engine); err != nil {
			record.Results[index].State = "failed"
			record.Results[index].Messages = []DatabaseTableMaintenanceMessage{{Type: "policy", Text: maintenanceBoundMessage(coreErrorMessage(err))}}
			if saveErr := e.save("private", record.PreviewID, record); saveErr != nil {
				return nil, failure("operation_recovery_required", "Could not durably record the rejected table operation", 409)
			}
			continue
		}
		record.Results[index].State = "checking"
		if err = e.save("private", record.PreviewID, record); err != nil {
			return nil, failure("operation_recovery_required", "Could not durably record the selected table recheck", 409)
		}
		definition, definitionErr := readDatabaseTableMaintenanceDefinition(ctx, db, in.Database, table.Name)
		if definitionErr != nil {
			if ctx.Err() != nil {
				record.Results[index].State = "not_started"
				for remaining := index + 1; remaining < len(record.Results); remaining++ {
					record.Results[remaining].State = "not_started"
				}
				record.State = "canceled"
				record.FinishedAt = time.Now().UTC()
				if saveErr := e.save("private", record.PreviewID, record); saveErr != nil {
					return nil, failure("operation_recovery_required", "Task was canceled before SQL and its table progress could not be finalized; inspect before retrying", 409)
				}
				return nil, ctx.Err()
			}
			record.Results[index].State = "failed"
			record.Results[index].Messages = []DatabaseTableMaintenanceMessage{{Type: "policy", Text: "Selected table could not be rechecked; no maintenance statement was sent."}}
			for remaining := index + 1; remaining < len(record.Results); remaining++ {
				record.Results[remaining].State = "not_started"
			}
			record.State = "partial"
			record.FinishedAt = time.Now().UTC()
			if saveErr := e.save("private", record.PreviewID, record); saveErr != nil {
				return nil, failure("operation_recovery_required", "Selected table could not be rechecked and the result journal could not be finalized", 409)
			}
			return nil, definitionErr
		}
		currentDefinition := sha256.Sum256([]byte(definition))
		if !strings.EqualFold(hex.EncodeToString(currentDefinition[:]), table.DefinitionSHA256) {
			record.Results[index].State = "failed"
			record.Results[index].Messages = []DatabaseTableMaintenanceMessage{{Type: "policy", Text: "Selected table definition changed after preview; no maintenance statement was sent."}}
			for remaining := index + 1; remaining < len(record.Results); remaining++ {
				record.Results[remaining].State = "not_started"
			}
			record.State = "partial"
			record.FinishedAt = time.Now().UTC()
			if saveErr := e.save("private", record.PreviewID, record); saveErr != nil {
				return nil, failure("operation_recovery_required", "Table definition changed after preview and its result journal could not be finalized", 409)
			}
			return nil, failure("revision_conflict", "A selected table definition changed after preview; no maintenance statement was sent", 412)
		}
		if err = ctx.Err(); err != nil {
			record.Results[index].State = "not_started"
			for remaining := index + 1; remaining < len(record.Results); remaining++ {
				record.Results[remaining].State = "not_started"
			}
			record.State = "canceled"
			record.FinishedAt = time.Now().UTC()
			if saveErr := e.save("private", record.PreviewID, record); saveErr != nil {
				return nil, failure("operation_recovery_required", "Task was canceled before SQL and its table progress could not be finalized; inspect before retrying", 409)
			}
			return nil, err
		}
		record.Results[index].State = "executing"
		if err = e.save("private", record.PreviewID, record); err != nil {
			return nil, failure("operation_recovery_required", "Could not durably record the next table operation; no replay will be attempted", 409)
		}
		state, messages, queryErr := readDatabaseTableMaintenanceResult(ctx, db, in.Database, table.Name, record.Operation)
		if queryErr != nil {
			state = "outcome_unknown"
			messages = []DatabaseTableMaintenanceMessage{{Type: "error", Text: "Server result could not be confirmed; inspect before retrying."}}
		}
		record.Results[index].State = state
		record.Results[index].Messages = messages
		if err = e.save("private", record.PreviewID, record); err != nil {
			return nil, failure("operation_recovery_required", "The server returned a result but its protected maintenance journal could not be updated; inspect before retrying", 409)
		}
		if state == "outcome_unknown" {
			for remaining := index + 1; remaining < len(record.Results); remaining++ {
				record.Results[remaining].State = "not_started"
			}
			break
		}
		if ctx.Err() != nil {
			for remaining := index + 1; remaining < len(record.Results); remaining++ {
				record.Results[remaining].State = "not_started"
			}
			record.State = "canceled"
			record.FinishedAt = time.Now().UTC()
			if saveErr := e.save("private", record.PreviewID, record); saveErr != nil {
				return nil, failure("operation_recovery_required", "Task was canceled after a table operation; inspect the protected preview", 409)
			}
			return nil, ctx.Err()
		}
	}
	if record.State == "running" {
		record.State = databaseTableMaintenanceAggregateState(record.Results)
	}
	record.FinishedAt = time.Now().UTC()
	if err = e.save("private", record.PreviewID, record); err != nil {
		return nil, failure("operation_recovery_required", "Maintenance outcome could not be committed to its protected record; inspect before retrying", 409)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return publicDatabaseTableMaintenanceResult(record), nil
}

func databaseTableMaintenanceAggregateState(results []DatabaseTableMaintenanceItemResult) string {
	completed, failed, unknown, notStarted := 0, 0, 0, 0
	for _, item := range results {
		switch item.State {
		case "completed":
			completed++
		case "failed":
			failed++
		case "outcome_unknown", "executing":
			unknown++
		case "not_started", "pending":
			notStarted++
		}
	}
	if unknown > 0 {
		return "outcome_unknown"
	}
	if notStarted > 0 && completed > 0 || failed > 0 && completed > 0 || failed > 0 && notStarted > 0 {
		return "partial"
	}
	if notStarted > 0 {
		return "canceled"
	}
	if failed > 0 {
		return "failed"
	}
	return "completed"
}

func coreErrorMessage(err error) string {
	var known *core.Error
	if !errors.As(err, &known) {
		return "Table maintenance was rejected by policy."
	}
	return known.Message
}

func databaseTableMaintenanceResources(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var input DatabaseTableMaintenanceInput
	if err := parse(raw, &input); err != nil || !validInstanceID(input.InstanceID) || !validDatabaseName(input.Database) {
		return nil, failure("invalid_input", "A valid managed instance and database are required", 400)
	}
	return []string{DatabaseResource(input.InstanceID, input.Database)}, nil
}

// databaseTableMaintenanceBindings exposes only table inventory, frozen
// previews, durable maintenance tasks, and read-only task inspection.
func (m *Manager) databaseTableMaintenanceBindings() []core.Binding {
	instance := map[string]any{"type": "string", "pattern": validIDExtended.String()}
	database := map[string]any{"type": "string", "pattern": databaseIdentifier.String()}
	digest := map[string]any{"type": "string", "pattern": databaseTableMaintenanceDigestPattern.String()}
	preview := map[string]any{"type": "string", "pattern": databaseTableMaintenancePreviewPattern.String()}
	revision := map[string]any{"type": "integer", "minimum": 1, "maximum": int64(9223372036854775807)}
	fields := map[string]any{
		"instance_id":                instance,
		"database":                   database,
		"limit":                      map[string]any{"type": "integer", "minimum": 0, "maximum": databaseTableMaintenanceListLimit},
		"after":                      map[string]any{"type": "string", "maxLength": 64},
		"operation":                  enumSchema("optimize", "repair"),
		"tables":                     map[string]any{"type": "array", "minItems": 1, "maxItems": databaseTableMaintenanceMaxSelection, "uniqueItems": true, "items": map[string]any{"type": "string", "minLength": 1, "maxLength": 64}},
		"expected_instance_revision": revision,
		"expected_database_revision": revision,
		"expected_schema_sha256":     digest,
		"preview_id":                 preview,
		"snapshot_sha256":            digest,
		"confirm_maintenance":        map[string]any{"type": "boolean", "const": true},
	}
	specs := []struct {
		name, command, description, scope string
		readOnly, task, audit             bool
		fields                            []string
		required                          []string
	}{
		{"managed_database_tables_list", "list", "List bounded tables and a pinned schema inventory for an owned MySQL or MariaDB database", "databases:read", true, false, false, []string{"instance_id", "database", "limit", "after"}, []string{"instance_id", "database"}},
		{"managed_database_table_maintenance_preview", "preview", "Freeze selected owned table definitions, revisions, size estimates, and operation impact before maintenance", "databases:admin", false, false, true, []string{"instance_id", "database", "operation", "tables", "expected_instance_revision", "expected_database_revision", "expected_schema_sha256"}, []string{"instance_id", "database", "operation", "tables", "expected_instance_revision", "expected_database_revision", "expected_schema_sha256"}},
		{"managed_database_table_maintenance", "run", "Submit a one-shot durable OPTIMIZE or MyISAM REPAIR task from the exact preview after explicit effect confirmation", "databases:admin", false, true, true, []string{"instance_id", "database", "preview_id", "snapshot_sha256", "expected_instance_revision", "expected_database_revision", "confirm_maintenance"}, []string{"instance_id", "database", "preview_id", "snapshot_sha256", "expected_instance_revision", "expected_database_revision", "confirm_maintenance"}},
		{"managed_database_table_maintenance_inspect", "inspect", "Inspect the protected result of a table maintenance preview without replaying SQL", "databases:read", true, false, false, []string{"instance_id", "database", "preview_id"}, []string{"instance_id", "database", "preview_id"}},
	}
	bindings := make([]core.Binding, 0, len(specs))
	for _, spec := range specs {
		spec := spec
		inputFields := map[string]any{}
		for _, field := range spec.fields {
			inputFields[field] = fields[field]
		}
		binding := m.binding(spec.name, spec.description, spec.scope, spec.readOnly, obj(inputFields, spec.required...), databaseTableMaintenanceResources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			var input DatabaseTableMaintenanceInput
			if err := parse(raw, &input); err != nil {
				return core.ActionResult{}, err
			}
			input.Command = spec.command
			forward, err := json.Marshal(input)
			if err != nil {
				return core.ActionResult{}, failure("invalid_input", "Table maintenance request could not be encoded", 400)
			}
			resources, err := databaseTableMaintenanceResources(ctx, actor, forward)
			if err != nil {
				return core.ActionResult{}, err
			}
			summary := map[string]any{"instance_id": input.InstanceID, "database": input.Database, "command": spec.command}
			if spec.task {
				job, submitErr := m.deps.Tasks.Submit(ctx, actor, databaseTableMaintenanceOperation, forward, resources)
				if submitErr != nil {
					return core.ActionResult{}, submitErr
				}
				taskID, _ := job["id"].(string)
				if taskID == "" {
					taskID, _ = job["task_id"].(string)
				}
				summary["preview_id"] = input.PreviewID
				return core.ActionResult{Data: job, TaskID: taskID, Resources: resources, AuditSummary: summary}, nil
			}
			var output any
			if err = m.agentJSON(ctx, databaseTableMaintenanceOperation, forward, &output); err != nil {
				return core.ActionResult{}, err
			}
			if spec.command == "preview" {
				if previewOutput, ok := output.(map[string]any); ok {
					summary["preview_id"] = previewOutput["preview_id"]
					summary["operation"] = input.Operation
					summary["table_count"] = previewOutput["table_count"]
				}
			}
			if spec.audit {
				return core.ActionResult{Data: output, Resources: resources, AuditSummary: summary}, nil
			}
			return core.ActionResult{Data: output, Resources: resources}, nil
		})
		binding.Action.Task = spec.task
		binding.Action.Audit = spec.audit
		bindings = append(bindings, binding)
	}
	return bindings
}
