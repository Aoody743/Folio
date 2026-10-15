package plugins

import (
	"context"
	"os"
	"strings"
	"syscall"
	"time"
)

func (e *PrivilegedExecutor) advanceDatabaseReplicationOperation(record *databaseReplicationRecord, operation, state, phase string) error {
	if record == nil || !validReplicationOperation(operation) || !validReplicationState(state) || !validReplicationPhase(phase) {
		return failure("ownership_invalid", "Replication journal transition is invalid", 412)
	}
	current, err := e.loadDatabaseReplication(record.PairID)
	if err != nil || current == nil || current.Revision != record.Revision {
		return failure("revision_conflict", "Replication journal changed during its operation; explicit recovery is required", 409)
	}
	next := *record
	next.Revision++
	next.Operation = operation
	next.State = state
	next.Phase = phase
	next.UpdatedAt = time.Now().UTC()
	if !validDatabaseReplicationPair(next) {
		return failure("ownership_invalid", "Replication journal transition failed protected record validation", 412)
	}
	if err = e.save("private", databaseReplicationPairKey(next.PairID), next); err != nil {
		return failure("operation_recovery_required", "Replication phase could not be committed; inspect the exact pair before retrying", 409)
	}
	*record = next
	return nil
}

func (e *PrivilegedExecutor) startDatabaseReplication(ctx context.Context, source, target Instance, record databaseReplicationRecord) (DatabaseReplicationStatus, error) {
	if record.Phase != "settled" || record.State == "removed" {
		return DatabaseReplicationStatus{}, failure("operation_recovery_required", "Finish explicit recovery before starting this replication pair", 409)
	}
	status, err := e.readLiveDatabaseReplicationStatus(ctx, source, target, record)
	if err != nil {
		return DatabaseReplicationStatus{}, err
	}
	if status.State == "running" {
		if record.State != "running" {
			if err = e.advanceDatabaseReplicationOperation(&record, "start", "running", "settled"); err != nil {
				return DatabaseReplicationStatus{}, err
			}
		}
		status.PairRevision, status.State, status.Phase = record.Revision, record.State, record.Phase
		return status, nil
	}
	if err = e.advanceDatabaseReplicationOperation(&record, "start", "configuring", "channel_start_pending"); err != nil {
		return DatabaseReplicationStatus{}, err
	}
	if err = e.startReplicationChannel(ctx, source, target, record); err != nil {
		return DatabaseReplicationStatus{}, e.databaseReplicationFailure(&record, err)
	}
	status, err = e.readLiveDatabaseReplicationStatus(ctx, source, target, record)
	if err != nil {
		return DatabaseReplicationStatus{}, e.databaseReplicationFailure(&record, err)
	}
	if status.State != "running" {
		return DatabaseReplicationStatus{}, e.databaseReplicationFailure(&record, failure("replication_start_unverified", "Both replication threads did not become active; inspect status and explicitly recover", 409))
	}
	if err = e.advanceDatabaseReplicationOperation(&record, "start", "running", "settled"); err != nil {
		return DatabaseReplicationStatus{}, e.databaseReplicationFailure(&record, err)
	}
	status.PairRevision, status.State, status.Phase = record.Revision, record.State, record.Phase
	return status, nil
}

func (e *PrivilegedExecutor) stopDatabaseReplication(ctx context.Context, source, target Instance, record databaseReplicationRecord) (DatabaseReplicationStatus, error) {
	if record.Phase != "settled" || record.State == "removed" {
		return DatabaseReplicationStatus{}, failure("operation_recovery_required", "Finish explicit recovery before stopping this replication pair", 409)
	}
	status, err := e.readLiveDatabaseReplicationStatus(ctx, source, target, record)
	if err != nil {
		return DatabaseReplicationStatus{}, err
	}
	if status.State == "stopped" {
		if record.State != "stopped" {
			if err = e.advanceDatabaseReplicationOperation(&record, "stop", "stopped", "settled"); err != nil {
				return DatabaseReplicationStatus{}, err
			}
		}
		status.PairRevision, status.State, status.Phase = record.Revision, record.State, record.Phase
		return status, nil
	}
	if err = e.advanceDatabaseReplicationOperation(&record, "stop", "configuring", "channel_stop_pending"); err != nil {
		return DatabaseReplicationStatus{}, err
	}
	if err = e.stopReplicationChannel(ctx, target, record); err != nil {
		return DatabaseReplicationStatus{}, e.databaseReplicationFailure(&record, err)
	}
	status, err = e.readLiveDatabaseReplicationStatus(ctx, source, target, record)
	if err != nil {
		return DatabaseReplicationStatus{}, e.databaseReplicationFailure(&record, err)
	}
	if status.State != "stopped" {
		return DatabaseReplicationStatus{}, e.databaseReplicationFailure(&record, failure("replication_stop_unverified", "Both replication threads did not stop; inspect status and explicitly recover", 409))
	}
	if err = e.advanceDatabaseReplicationOperation(&record, "stop", "stopped", "settled"); err != nil {
		return DatabaseReplicationStatus{}, e.databaseReplicationFailure(&record, err)
	}
	status.PairRevision, status.State, status.Phase = record.Revision, record.State, record.Phase
	return status, nil
}

func (e *PrivilegedExecutor) removeDatabaseReplication(ctx context.Context, source, target Instance, record databaseReplicationRecord) (DatabaseReplicationStatus, error) {
	if record.Phase != "settled" || record.State == "removed" {
		return DatabaseReplicationStatus{}, failure("operation_recovery_required", "Finish explicit recovery before removing this replication pair", 409)
	}
	if _, err := e.readLiveDatabaseReplicationStatus(ctx, source, target, record); err != nil {
		return DatabaseReplicationStatus{}, err
	}
	if err := e.advanceDatabaseReplicationOperation(&record, "remove", "removing", "channel_stop_pending"); err != nil {
		return DatabaseReplicationStatus{}, err
	}
	return e.continueDatabaseReplicationRemoval(ctx, source, target, &record)
}

func (e *PrivilegedExecutor) continueDatabaseReplicationRemoval(ctx context.Context, source, target Instance, record *databaseReplicationRecord) (DatabaseReplicationStatus, error) {
	fail := func(err error) (DatabaseReplicationStatus, error) {
		return DatabaseReplicationStatus{}, e.databaseReplicationFailure(record, err)
	}
	if record.Operation != "remove" {
		return fail(failure("ownership_invalid", "Only an exact remove intent can run the pair removal sequence", 412))
	}
	if record.Phase == "channel_stop_pending" {
		if err := e.stopReplicationChannel(ctx, target, *record); err != nil {
			return fail(err)
		}
		status, err := e.readLiveDatabaseReplicationStatus(ctx, source, target, *record)
		if err != nil {
			return fail(err)
		}
		if status.State != "stopped" {
			return fail(failure("replication_stop_unverified", "Both replication threads must be stopped before removal", 409))
		}
		if err = e.advanceDatabaseReplicationOperation(record, "remove", "removing", "remove_channel_pending"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "remove_channel_pending" {
		if err := e.removeReplicationChannel(ctx, source, target, *record); err != nil {
			return fail(err)
		}
		if err := e.advanceDatabaseReplicationOperation(record, "remove", "removing", "channel_removed"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "channel_removed" {
		if err := e.advanceDatabaseReplicationOperation(record, "remove", "removing", "remove_account_pending"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "remove_account_pending" {
		if err := e.removeDatabaseReplicationAccount(ctx, source, *record); err != nil {
			return fail(err)
		}
		if err := e.advanceDatabaseReplicationOperation(record, "remove", "removing", "account_removed"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "account_removed" {
		if err := e.advanceDatabaseReplicationOperation(record, "remove", "removing", "restore_target_config_pending"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "restore_target_config_pending" {
		if err := e.restoreDatabaseReplicationConfig(ctx, target, *record, "target"); err != nil {
			return fail(err)
		}
		if err := e.advanceDatabaseReplicationOperation(record, "remove", "removing", "restore_source_config_pending"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "restore_source_config_pending" {
		if err := e.restoreDatabaseReplicationConfig(ctx, source, *record, "source"); err != nil {
			return fail(err)
		}
		if err := e.advanceDatabaseReplicationOperation(record, "remove", "removing", "config_restored"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "config_restored" {
		if err := e.advanceDatabaseReplicationOperation(record, "remove", "removing", "seed_cleanup_pending"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "seed_cleanup_pending" {
		if err := e.removeDatabaseReplicationSeed(ctx, target, *record); err != nil {
			return fail(err)
		}
		if err := e.advanceDatabaseReplicationOperation(record, "remove", "removing", "seed_cleaned"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "seed_cleaned" {
		if err := e.advanceDatabaseReplicationOperation(record, "remove", "removed", "settled"); err != nil {
			return fail(err)
		}
	}
	if record.State != "removed" || record.Phase != "settled" {
		return fail(failure("operation_recovery_required", "Replication removal stopped in an unknown protected phase", 409))
	}
	return replicationStatusFromRecord(*record), nil
}

func (e *PrivilegedExecutor) removeReplicationChannel(ctx context.Context, source, target Instance, record databaseReplicationRecord) error {
	if err := e.verifyReplicationConfig(ctx, source, target, record, true); err != nil {
		return err
	}
	db, err := e.openManagedAdmin(ctx, target)
	if err != nil {
		return err
	}
	defer db.Close()
	channels, err := readDatabaseReplicationChannels(ctx, db, record.Engine)
	if err != nil {
		return err
	}
	if len(channels) == 0 {
		return nil // Explicit recovery after RESET already completed.
	}
	if len(channels) != 1 || databaseReplicationChannelName(channels[0], record.Engine) != record.Channel {
		return failure("replication_channel_unmanaged", "Target channel list no longer proves the exact generated channel", 412)
	}
	if err = validateReplicationChannelRow(channels[0], source, record); err != nil {
		return err
	}
	if strings.EqualFold(databaseReplicationFirstValue(channels[0], "replica_io_running", "slave_io_running"), "yes") || strings.EqualFold(databaseReplicationFirstValue(channels[0], "replica_sql_running", "slave_sql_running"), "yes") {
		return failure("replication_channel_running", "Generated channel must be stopped before it can be removed", 409)
	}
	statement := "RESET REPLICA ALL FOR CHANNEL '" + record.Channel + "'"
	if record.Engine == "mariadb" {
		statement = "RESET SLAVE '" + record.Channel + "' ALL"
	}
	return databaseReplicationExec(ctx, db, statement)
}

func (e *PrivilegedExecutor) removeDatabaseReplicationAccount(ctx context.Context, source Instance, record databaseReplicationRecord) error {
	db, err := e.openManagedAdmin(ctx, source)
	if err != nil {
		return err
	}
	defer db.Close()
	exists, _, err := databaseReplicationAccountGrants(ctx, db, record)
	if err != nil {
		return err
	}
	if !exists {
		return nil // Explicit recovery after DROP USER completed.
	}
	return databaseReplicationExec(ctx, db, "DROP USER "+databaseReplicationAccountName(record))
}

func (e *PrivilegedExecutor) restoreDatabaseReplicationConfig(ctx context.Context, instance Instance, record databaseReplicationRecord, role string) error {
	before, after, expectedBefore, expectedAfter := record.SourceConfigBefore, record.SourceConfigAfter, record.SourceConfigSHA256, record.SourceConfigAfterSHA256
	if role == "target" {
		before, after, expectedBefore, expectedAfter = record.TargetConfigBefore, record.TargetConfigAfter, record.TargetConfigSHA256, record.TargetConfigAfterSHA256
	}
	if len(before) == 0 || sha256Hex(before) != expectedBefore || sha256Hex(after) != expectedAfter {
		return failure("ownership_invalid", "Replication config restore receipt does not prove its exact previous and managed images", 412)
	}
	_, current, err := e.readDatabaseBinlogConfig(instance)
	if err != nil {
		return err
	}
	if string(current.Body) != string(before) && string(current.Body) != string(after) {
		return failure("configuration_unmanaged", "Current database configuration differs from both journaled images; it was not overwritten", 412)
	}
	wasAfter := string(current.Body) == string(after)
	if wasAfter {
		if err = writeAtomicRoot(instance.Paths.ConfigDir, "my.cnf", before, current.Mode, current.GID); err != nil {
			return failure("operation_recovery_required", "Previous database configuration could not be restored; inspect this exact pair", 409)
		}
	}
	needRestart := wasAfter
	if !needRestart {
		db, openErr := e.openManagedAdmin(ctx, instance)
		if openErr != nil {
			return openErr
		}
		runtime, runtimeErr := readDatabaseReplicationRuntime(ctx, db, instance.PluginID)
		if runtimeErr == nil {
			expectedServerID := record.SourceServerID
			if role == "target" {
				expectedServerID = record.TargetServerID
			}
			needRestart = runtime.ServerID == expectedServerID
		}
		if runtimeErr == nil && role == "target" && record.Engine == "mariadb" {
			var filter string
			filter, runtimeErr = readDatabaseReplicationVariable(ctx, db, "replicate_wild_do_table")
			needRestart = needRestart || filter == databaseReplicationSQLWildcard(record.Database)
		}
		_ = db.Close()
		if runtimeErr != nil {
			return runtimeErr
		}
	}
	if needRestart {
		markerCtx, markerErr := databaseReplicationRecoveryContextFor(ctx, e, record)
		if markerErr != nil {
			return markerErr
		}
		if _, err = e.controlLocked(markerCtx, ServiceInput{ID: instance.ID, Command: "restart"}, instance); err != nil {
			return failure("operation_recovery_required", "Database restart after restoring its journaled configuration requires explicit recovery", 409)
		}
	}
	if e.probe(ctx, instance).Status != "healthy" {
		return failure("runtime_unavailable", "Database did not become healthy after restoring its journaled configuration", 409)
	}
	return nil
}

func (e *PrivilegedExecutor) removeDatabaseReplicationSeed(ctx context.Context, target Instance, record databaseReplicationRecord) error {
	if record.SeedFilename == "" {
		return nil
	}
	root, directory, _, err := e.openDatabaseReplicationSeedDirectory(false)
	if err != nil {
		return err
	}
	defer root.Close()
	defer directory.Close()
	file, err := root.OpenFile(record.SeedFilename, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	file.Close()
	file, err = e.openVerifiedDatabaseReplicationSeed(ctx, target, record)
	if err != nil {
		return err
	}
	_ = file.Close()
	if err = root.Remove(record.SeedFilename); err != nil && !os.IsNotExist(err) {
		return failure("operation_recovery_required", "Verified replication seed artifact could not be removed", 409)
	}
	if err = directory.Sync(); err != nil {
		return failure("operation_recovery_required", "Protected replication seed cleanup could not be durably synced", 409)
	}
	return nil
}
