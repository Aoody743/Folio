package plugins

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

func (e *PrivilegedExecutor) databaseReplicationList(ctx context.Context, instanceID string) (DatabaseReplicationPairsList, error) {
	if err := ctx.Err(); err != nil {
		return DatabaseReplicationPairsList{}, err
	}
	records, err := e.databaseReplicationPairsForInstance(instanceID)
	if err != nil {
		return DatabaseReplicationPairsList{}, err
	}
	items := make([]DatabaseReplicationPairSummary, 0, len(records))
	for _, record := range records {
		items = append(items, DatabaseReplicationPairSummary{
			PairID: record.PairID, PairRevision: record.Revision, State: record.State, Engine: record.Engine,
			SourceInstanceID: record.SourceInstanceID, TargetInstanceID: record.TargetInstanceID, Database: record.Database,
		})
	}
	return DatabaseReplicationPairsList{Items: items, Total: len(items)}, nil
}

func (e *PrivilegedExecutor) databaseReplicationCapabilities(ctx context.Context, input DatabaseReplicationInput) (DatabaseReplicationCapability, error) {
	capability := DatabaseReplicationCapability{SourceInstanceID: input.SourceInstanceID, TargetInstanceID: input.TargetInstanceID, Actions: []string{}}
	source, err := e.get(ctx, input.SourceInstanceID, false)
	if err != nil {
		return capability, err
	}
	target, err := e.get(ctx, input.TargetInstanceID, false)
	if err != nil {
		return capability, err
	}
	engine := canonicalDatabaseReplicationEngine(source.PluginID)
	capability.Engine = engine
	if engine == "" || engine != canonicalDatabaseReplicationEngine(target.PluginID) {
		capability.Reason = "Replication requires the same managed MySQL or MariaDB engine on both endpoints"
		return capability, nil
	}
	for _, instance := range []Instance{source, target} {
		if err = e.validateDatabaseReplicationInstance(ctx, instance, true); err != nil {
			capability.Reason = replicationSafeReason(err, "Managed database prerequisites could not be verified")
			return capability, nil
		}
	}
	if setting(source.Config, "port", 0) == setting(target.Config, "port", 0) {
		capability.Reason = "Loopback managed endpoints must use different local database ports"
		return capability, nil
	}
	for _, instance := range []Instance{source, target} {
		pairs, pairErr := e.databaseReplicationPairsForInstance(instance.ID)
		if pairErr != nil {
			return capability, pairErr
		}
		for _, pair := range pairs {
			if pair.State != "removed" {
				capability.Reason = "An endpoint already belongs to a managed replication pair; remove it before reuse"
				return capability, nil
			}
		}
		db, openErr := e.openManagedAdmin(ctx, instance)
		if openErr != nil {
			capability.Reason = replicationSafeReason(openErr, "Managed database administrator state could not be verified")
			return capability, nil
		}
		_, protocolErr := readDatabaseReplicationRuntime(ctx, db, instance.PluginID)
		channels, channelErr := readDatabaseReplicationChannels(ctx, db, instance.PluginID)
		if protocolErr == nil && channelErr == nil && len(channels) != 0 {
			channelErr = failure("replication_channel_unmanaged", "Pre-existing replication channels will not be adopted or overwritten", 412)
		}
		if engine == "mariadb" && protocolErr == nil && channelErr == nil {
			filters, filterErr := readDatabaseReplicationVariable(ctx, db, "replicate_wild_do_table")
			if filterErr != nil {
				channelErr = filterErr
			} else if filters != "" {
				channelErr = failure("replication_filter_unmanaged", "MariaDB already has a global replication filter", 412)
			}
		}
		_ = db.Close()
		if protocolErr != nil || channelErr != nil {
			capability.Reason = replicationSafeReason(firstReplicationError(protocolErr, channelErr), "Replication protocol prerequisites could not be verified")
			return capability, nil
		}
		if instance.ID == source.ID {
			_, config, configErr := e.readDatabaseBinlogConfig(instance)
			if configErr != nil {
				capability.Reason = replicationSafeReason(configErr, "Owned source binary-log configuration could not be verified")
				return capability, nil
			}
			_, managed, configState := parseDatabaseBinlogConfig(config.Body)
			if !managed || configState != "settled" {
				capability.Reason = "Source binary-log configuration is not an exact managed state"
				return capability, nil
			}
		} else {
			_, config, configErr := e.readDatabaseBinlogConfig(instance)
			if configErr != nil {
				capability.Reason = replicationSafeReason(configErr, "Owned target configuration could not be verified")
				return capability, nil
			}
			if hasReplicationConfigDirective(config.Body, false) {
				capability.Reason = "Target contains an unmanaged server ID or replication filter"
				return capability, nil
			}
		}
	}
	capability.Supported = true
	capability.Actions = []string{"preview", "configure"}
	return capability, nil
}

func firstReplicationError(first, second error) error {
	if first != nil {
		return first
	}
	return second
}

func replicationSafeReason(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	return strings.TrimSpace(err.Error())
}

func (e *PrivilegedExecutor) readDatabaseReplicationStatus(ctx context.Context, input DatabaseReplicationInput) (DatabaseReplicationStatus, error) {
	record, err := e.loadDatabaseReplication(input.PairID)
	if err != nil {
		return DatabaseReplicationStatus{}, err
	}
	if record == nil {
		return DatabaseReplicationStatus{}, failure("replication_pair_not_found", "Managed replication pair does not exist", 404)
	}
	if err = validateReplicationPairInput(*record, input); err != nil {
		return DatabaseReplicationStatus{}, err
	}
	if record.Revision != input.ExpectedPairRevision {
		return DatabaseReplicationStatus{}, failure("revision_conflict", "Replication pair changed; refresh its status before retrying", 412)
	}
	status := replicationStatusFromRecord(*record)
	if record.State == "pending" || record.State == "recovery_required" || record.State == "configuring" || record.State == "seeding" || record.Phase != "settled" {
		status.RecoveryRequired = true
		status.ErrorClass = "recovery_required"
		return status, nil
	}
	return e.withReplicationPairTransaction(ctx, *record, func(held context.Context, source, target Instance, _ managedDatabaseRecord, _ managedDatabaseRecord) (DatabaseReplicationStatus, error) {
		current, loadErr := e.loadDatabaseReplication(input.PairID)
		if loadErr != nil {
			return DatabaseReplicationStatus{}, loadErr
		}
		if current == nil || current.Revision != input.ExpectedPairRevision || current.PairID != record.PairID {
			return DatabaseReplicationStatus{}, failure("revision_conflict", "Replication pair changed while status was opening", 412)
		}
		return e.readLiveDatabaseReplicationStatus(held, source, target, *current)
	})
}

func (e *PrivilegedExecutor) withReplicationPairTransaction(ctx context.Context, record databaseReplicationRecord, callback func(context.Context, Instance, Instance, managedDatabaseRecord, managedDatabaseRecord) (DatabaseReplicationStatus, error)) (DatabaseReplicationStatus, error) {
	transactionContext := ctx
	var err error
	if record.State != "removed" && (record.State == "pending" || record.State == "recovery_required" || record.State == "configuring" || record.State == "seeding" || record.Phase != "settled") {
		transactionContext, err = databaseReplicationRecoveryContextFor(ctx, e, record)
		if err != nil {
			return DatabaseReplicationStatus{}, err
		}
	}
	var out DatabaseReplicationStatus
	err = e.WithRuntimeTransaction(transactionContext, []string{record.SourceInstanceID, record.TargetInstanceID}, func(held context.Context) error {
		source, target, sourceDBRecord, targetDBRecord, endpointErr := e.loadReplicationPairEndpoints(held, record)
		if endpointErr != nil {
			return endpointErr
		}
		var callbackErr error
		out, callbackErr = callback(held, source, target, sourceDBRecord, targetDBRecord)
		return callbackErr
	})
	return out, err
}

func (e *PrivilegedExecutor) loadReplicationPairEndpoints(ctx context.Context, record databaseReplicationRecord) (Instance, Instance, managedDatabaseRecord, managedDatabaseRecord, error) {
	var sourceRecord, targetRecord managedDatabaseRecord
	source, err := e.get(ctx, record.SourceInstanceID, false)
	if err != nil {
		return Instance{}, Instance{}, sourceRecord, targetRecord, err
	}
	target, err := e.get(ctx, record.TargetInstanceID, false)
	if err != nil {
		return Instance{}, Instance{}, sourceRecord, targetRecord, err
	}
	if !databaseReplicationEndpointIdentityMatches(source, record, true) || !databaseReplicationEndpointIdentityMatches(target, record, false) {
		return source, target, sourceRecord, targetRecord, failure("ownership_invalid", "Replication pair no longer matches its exact managed endpoints", 412)
	}
	if err = e.validateDatabaseReplicationInstance(ctx, source, true); err != nil {
		return source, target, sourceRecord, targetRecord, err
	}
	if err = e.validateDatabaseReplicationInstance(ctx, target, true); err != nil {
		return source, target, sourceRecord, targetRecord, err
	}
	for _, item := range []struct {
		instance Instance
		out      *managedDatabaseRecord
		revision int64
	}{
		{source, &sourceRecord, record.SourceDatabaseRevision}, {target, &targetRecord, record.TargetDatabaseRevision},
	} {
		if err = e.load("private", managedDatabaseKey(item.instance.ID, record.Database), item.out); err != nil {
			return source, target, sourceRecord, targetRecord, failure("managed_database_required", "Replication endpoint no longer has its active owned database receipt", 412)
		}
		if item.out.InstanceID != item.instance.ID || item.out.Database != record.Database || item.out.Engine != item.instance.PluginID || item.out.State != "active" || item.out.Revision != item.revision {
			return source, target, sourceRecord, targetRecord, failure("ownership_invalid", "Replication database receipt no longer matches its owned endpoint", 412)
		}
	}
	return source, target, sourceRecord, targetRecord, nil
}

// Instance Revision protects the initial preview/configure CAS. Later service
// start/stop and other allowed receipt revisions do not change endpoint
// ownership; these immutable fields and the exact journaled config images do.
func databaseReplicationEndpointIdentityMatches(instance Instance, record databaseReplicationRecord, source bool) bool {
	dataDir, configDir, instanceID := record.TargetDataDir, record.TargetConfigDir, record.TargetInstanceID
	if source {
		dataDir, configDir, instanceID = record.SourceDataDir, record.SourceConfigDir, record.SourceInstanceID
	}
	return instance.ID == instanceID && instance.Ownership == "managed" && canonicalDatabaseReplicationEngine(instance.PluginID) == record.Engine &&
		instance.Paths.DataDir == dataDir && instance.Paths.ConfigDir == configDir && instance.Service.ID == instance.ID && instance.Service.PluginID == instance.PluginID &&
		instance.Service.InstanceID == instance.InstanceID && instance.Service.Unit == "powerlinux-plugin-"+instance.ID+".service" &&
		instance.Service.ConfigPath == configDir && instance.Service.DataPath == dataDir && instance.Service.Ownership == "managed"
}

func validateReplicationPairInput(record databaseReplicationRecord, input DatabaseReplicationInput) error {
	if record.PairID != input.PairID || record.SourceInstanceID != input.SourceInstanceID || record.TargetInstanceID != input.TargetInstanceID || record.Database != input.Database {
		return failure("forbidden", "Replication pair identity differs from the selected endpoints", 403)
	}
	return nil
}

func replicationStatusFromRecord(record databaseReplicationRecord) DatabaseReplicationStatus {
	return DatabaseReplicationStatus{
		PairID: record.PairID, PairRevision: record.Revision, State: record.State, Phase: record.Phase,
		Operation: record.Operation, AllowedRecoveryChoices: databaseReplicationRecoveryChoices(record),
		Engine: record.Engine, SourceInstanceID: record.SourceInstanceID, TargetInstanceID: record.TargetInstanceID,
		Database: record.Database, Channel: record.Channel, SourceServerID: record.SourceServerID, TargetServerID: record.TargetServerID,
		SourceLogFile: record.SourceLogFile, SourceLogPosition: record.SourceLogPosition,
		SeedBytes: record.SeedBytes, SeedSHA256: record.SeedSHA256,
	}
}

func (e *PrivilegedExecutor) readLiveDatabaseReplicationStatus(ctx context.Context, source, target Instance, record databaseReplicationRecord) (DatabaseReplicationStatus, error) {
	if err := e.verifyReplicationConfig(ctx, source, target, record, true); err != nil {
		return DatabaseReplicationStatus{}, err
	}
	sourceDB, err := e.openManagedAdmin(ctx, source)
	if err != nil {
		return DatabaseReplicationStatus{}, err
	}
	defer sourceDB.Close()
	targetDB, err := e.openManagedAdmin(ctx, target)
	if err != nil {
		return DatabaseReplicationStatus{}, err
	}
	defer targetDB.Close()
	sourceChannels, err := readDatabaseReplicationChannels(ctx, sourceDB, source.PluginID)
	if err != nil {
		return DatabaseReplicationStatus{}, err
	}
	if len(sourceChannels) != 0 {
		return DatabaseReplicationStatus{}, failure("replication_channel_unmanaged", "Source acquired an unrelated replication channel after pair creation", 412)
	}
	channels, err := readDatabaseReplicationChannels(ctx, targetDB, target.PluginID)
	if err != nil {
		return DatabaseReplicationStatus{}, err
	}
	if len(channels) != 1 || databaseReplicationChannelName(channels[0], record.Engine) != record.Channel {
		return DatabaseReplicationStatus{}, failure("replication_channel_unmanaged", "Target replication channels no longer match the one managed pair", 412)
	}
	row := channels[0]
	if err = validateReplicationChannelRow(row, source, record); err != nil {
		return DatabaseReplicationStatus{}, err
	}
	status := replicationStatusFromRecord(record)
	targetRuntime, err := readDatabaseReplicationRuntime(ctx, targetDB, target.PluginID)
	if err != nil {
		return DatabaseReplicationStatus{}, err
	}
	status.TargetReadOnly = targetRuntime.ReadOnly
	status.TargetSuperReadOnly = targetRuntime.SuperReadOnly
	if !targetRuntime.ReadOnly || record.Engine == "mysql" && !targetRuntime.SuperReadOnly {
		return DatabaseReplicationStatus{}, failure("replication_target_protection_unverified", "Target runtime did not confirm the required read-only protection", 412)
	}
	status.TargetProtectionVerified = true
	status.IOThreadRunning = strings.EqualFold(databaseReplicationFirstValue(row, "replica_io_running", "slave_io_running"), "yes")
	status.SQLThreadRunning = strings.EqualFold(databaseReplicationFirstValue(row, "replica_sql_running", "slave_sql_running"), "yes")
	status.ErrorClass = ""
	if databaseReplicationFirstValue(row, "last_io_error") != "" {
		status.ErrorClass = "io"
	}
	if databaseReplicationFirstValue(row, "last_sql_error") != "" {
		status.ErrorClass = "sql"
	}
	if seconds := databaseReplicationFirstValue(row, "seconds_behind_source", "seconds_behind_master"); seconds != "" && seconds != "<nil>" {
		value, parseErr := strconv.ParseUint(seconds, 10, 64)
		if parseErr == nil {
			status.SecondsBehindSource = &value
		}
	}
	status.SourceLogFile = databaseReplicationFirstValue(row, "source_log_file", "master_log_file")
	if pos := databaseReplicationFirstValue(row, "read_source_log_pos", "read_master_log_pos"); pos != "" {
		status.SourceLogPosition, _ = strconv.ParseUint(pos, 10, 64)
	}
	if status.IOThreadRunning && status.SQLThreadRunning {
		status.State = "running"
	} else if !status.IOThreadRunning && !status.SQLThreadRunning {
		status.State = "stopped"
	} else {
		status.State = "degraded"
	}
	return status, nil
}

func databaseReplicationFirstValue(row map[string]any, names ...string) string {
	for _, name := range names {
		if value := databaseReplicationValue(row, name); value != "" && value != "<nil>" {
			return value
		}
	}
	return ""
}

func validateReplicationChannelRow(row map[string]any, source Instance, record databaseReplicationRecord) error {
	if databaseReplicationFirstValue(row, "source_host", "master_host") != "127.0.0.1" ||
		databaseReplicationFirstValue(row, "source_user", "master_user") != record.Account ||
		databaseReplicationFirstValue(row, "source_port", "master_port") != strconv.Itoa(setting(source.Config, "port", 0)) {
		return failure("replication_channel_unmanaged", "Replication channel source identity differs from the frozen managed endpoint", 412)
	}
	serverIDText := databaseReplicationFirstValue(row, "source_server_id", "master_server_id")
	serverID, err := strconv.ParseUint(serverIDText, 10, 32)
	if err != nil || uint32(serverID) != record.SourceServerID {
		return failure("replication_channel_unmanaged", "Replication channel did not prove the frozen source server identity", 412)
	}
	if record.Engine == "mysql" {
		filter := databaseReplicationFirstValue(row, "replicate_wild_do_table")
		if filter == "" || filter != databaseReplicationSQLWildcard(record.Database) {
			return failure("replication_filter_unverified", "MySQL channel table filter does not prove exact database isolation", 412)
		}
	}
	return nil
}

func (e *PrivilegedExecutor) verifyReplicationConfig(ctx context.Context, source, target Instance, record databaseReplicationRecord, requireHealthy bool) error {
	_, sourceConfig, err := e.readDatabaseBinlogConfig(source)
	if err != nil {
		return err
	}
	_, targetConfig, err := e.readDatabaseBinlogConfig(target)
	if err != nil {
		return err
	}
	if string(sourceConfig.Body) != string(record.SourceConfigAfter) || string(targetConfig.Body) != string(record.TargetConfigAfter) {
		return failure("configuration_unmanaged", "Replication configuration differs from its exact managed pair images", 412)
	}
	if requireHealthy {
		sourceDB, openErr := e.openManagedAdmin(ctx, source)
		if openErr != nil {
			return openErr
		}
		sourceRuntime, runtimeErr := readDatabaseReplicationRuntime(ctx, sourceDB, source.PluginID)
		_ = sourceDB.Close()
		if runtimeErr != nil {
			return runtimeErr
		}
		if sourceRuntime.ServerID != record.SourceServerID || !sourceRuntime.LogBin || sourceRuntime.Format != "ROW" {
			return failure("replication_runtime_mismatch", "Managed source runtime differs from its frozen server ID, binary-log state or ROW format", 412)
		}
		targetDB, openErr := e.openManagedAdmin(ctx, target)
		if openErr != nil {
			return openErr
		}
		targetRuntime, runtimeErr := readDatabaseReplicationRuntime(ctx, targetDB, target.PluginID)
		if runtimeErr == nil && (targetRuntime.ServerID != record.TargetServerID || !targetRuntime.ReadOnly || record.Engine == "mysql" && !targetRuntime.SuperReadOnly) {
			runtimeErr = failure("replication_runtime_mismatch", "Managed target runtime differs from its frozen server identity or required read-only protection", 412)
		}
		if runtimeErr == nil && record.Engine == "mariadb" {
			var filter string
			filter, runtimeErr = readDatabaseReplicationVariable(ctx, targetDB, "replicate_wild_do_table")
			if runtimeErr == nil && filter != databaseReplicationSQLWildcard(record.Database) {
				runtimeErr = failure("replication_filter_unverified", "MariaDB did not apply the exact database filter from its owned configuration", 412)
			}
		}
		_ = targetDB.Close()
		if runtimeErr != nil {
			return runtimeErr
		}
	}
	return nil
}

func databaseReplicationExec(ctx context.Context, db *sql.DB, statement string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, statement); err != nil {
		return failure("replication_sql_failed", "Managed replication SQL command did not complete; inspect the protected pair state before retrying", 502)
	}
	return nil
}

func databaseReplicationStringLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
