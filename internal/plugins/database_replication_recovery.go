package plugins

import (
	"context"
)

func (e *PrivilegedExecutor) recoverDatabaseReplication(ctx context.Context, input DatabaseReplicationInput) (DatabaseReplicationStatus, error) {
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
		return DatabaseReplicationStatus{}, failure("revision_conflict", "Replication pair changed; refresh its status before recovery", 412)
	}
	if record.State == "removed" || record.Phase == "settled" && record.State != "recovery_required" {
		return DatabaseReplicationStatus{}, failure("replication_recovery_not_needed", "This pair has no unfinished operation to recover", 409)
	}
	transactionContext, err := databaseReplicationRecoveryContextFor(ctx, e, *record)
	if err != nil {
		return DatabaseReplicationStatus{}, err
	}
	var out DatabaseReplicationStatus
	err = e.WithRuntimeTransaction(transactionContext, []string{record.SourceInstanceID, record.TargetInstanceID}, func(held context.Context) error {
		current, loadErr := e.loadDatabaseReplication(record.PairID)
		if loadErr != nil {
			return loadErr
		}
		if current == nil || current.Revision != input.ExpectedPairRevision {
			return failure("revision_conflict", "Replication pair changed before its recovery began", 412)
		}
		if err := validateReplicationPairInput(*current, input); err != nil {
			return err
		}
		// Pair record first, then both endpoint indexes, always under the same
		// sorted instance locks and before any recovery side effect.
		if err := e.indexDatabaseReplicationPair(*current); err != nil {
			return failure("operation_recovery_required", "Replication intent is durable but endpoint indexing remains incomplete", 409)
		}
		freshSource, sourceErr := e.get(held, current.SourceInstanceID, false)
		freshTarget, targetErr := e.get(held, current.TargetInstanceID, false)
		if sourceErr != nil || targetErr != nil {
			return failure("ownership_invalid", "Replication recovery cannot prove both original managed instance receipts", 412)
		}
		if !databaseReplicationEndpointIdentityMatches(freshSource, *current, true) || !databaseReplicationEndpointIdentityMatches(freshTarget, *current, false) {
			return failure("ownership_invalid", "Replication recovery cannot prove both original managed endpoint identities", 412)
		}
		if input.RecoveryChoice == "restore_previous" && current.Operation == "configure" && current.Phase == "config_pending" {
			// config_pending is persisted before either config file is changed.
			// Revisions may increase from legal service operations; the exact
			// endpoint identity and unchanged config images still gate this abort.
			if err := e.advanceDatabaseReplicationOperation(current, "recover", "removed", "settled"); err != nil {
				return err
			}
			out = replicationStatusFromRecord(*current)
			return nil
		}
		source, target, _, _, endpointErr := e.loadReplicationPairEndpoints(held, *current)
		if endpointErr != nil {
			return endpointErr
		}
		if input.RecoveryChoice == "restore_previous" {
			out, endpointErr = e.restorePreviousDatabaseReplication(held, source, target, current)
			return endpointErr
		}
		switch current.Operation {
		case "configure":
			out, endpointErr = e.continueDatabaseReplicationConfigure(held, source, target, current, true)
		case "start":
			out, endpointErr = e.recoverDatabaseReplicationStart(held, source, target, current)
		case "stop":
			out, endpointErr = e.recoverDatabaseReplicationStop(held, source, target, current)
		case "remove":
			out, endpointErr = e.continueDatabaseReplicationRemoval(held, source, target, current)
		default:
			endpointErr = failure("ownership_invalid", "Protected pair has an unsupported unfinished operation", 412)
		}
		return endpointErr
	})
	return out, err
}

func (e *PrivilegedExecutor) recoverDatabaseReplicationStart(ctx context.Context, source, target Instance, record *databaseReplicationRecord) (DatabaseReplicationStatus, error) {
	if record.Operation != "start" || record.Phase != "channel_start_pending" {
		return DatabaseReplicationStatus{}, failure("operation_recovery_required", "Pair is not at the exact pending start boundary", 409)
	}
	if err := e.startReplicationChannel(ctx, source, target, *record); err != nil {
		return DatabaseReplicationStatus{}, e.databaseReplicationFailure(record, err)
	}
	status, err := e.readLiveDatabaseReplicationStatus(ctx, source, target, *record)
	if err != nil {
		return DatabaseReplicationStatus{}, e.databaseReplicationFailure(record, err)
	}
	if status.State != "running" {
		return DatabaseReplicationStatus{}, e.databaseReplicationFailure(record, failure("replication_start_unverified", "Both replication threads did not become active; no further retry was attempted", 409))
	}
	if err = e.advanceDatabaseReplicationOperation(record, "start", "running", "settled"); err != nil {
		return DatabaseReplicationStatus{}, e.databaseReplicationFailure(record, err)
	}
	status.PairRevision, status.State, status.Phase = record.Revision, record.State, record.Phase
	return status, nil
}

func (e *PrivilegedExecutor) recoverDatabaseReplicationStop(ctx context.Context, source, target Instance, record *databaseReplicationRecord) (DatabaseReplicationStatus, error) {
	if record.Operation != "stop" || record.Phase != "channel_stop_pending" {
		return DatabaseReplicationStatus{}, failure("operation_recovery_required", "Pair is not at the exact pending stop boundary", 409)
	}
	if err := e.stopReplicationChannel(ctx, target, *record); err != nil {
		return DatabaseReplicationStatus{}, e.databaseReplicationFailure(record, err)
	}
	status, err := e.readLiveDatabaseReplicationStatus(ctx, source, target, *record)
	if err != nil {
		return DatabaseReplicationStatus{}, e.databaseReplicationFailure(record, err)
	}
	if status.State != "stopped" {
		return DatabaseReplicationStatus{}, e.databaseReplicationFailure(record, failure("replication_stop_unverified", "Both replication threads did not stop; no further retry was attempted", 409))
	}
	if err = e.advanceDatabaseReplicationOperation(record, "stop", "stopped", "settled"); err != nil {
		return DatabaseReplicationStatus{}, e.databaseReplicationFailure(record, err)
	}
	status.PairRevision, status.State, status.Phase = record.Revision, record.State, record.Phase
	return status, nil
}

func (e *PrivilegedExecutor) restorePreviousDatabaseReplication(ctx context.Context, source, target Instance, record *databaseReplicationRecord) (DatabaseReplicationStatus, error) {
	if record.Operation == "configure" {
		switch record.Phase {
		case "config_pending", "source_config_pending", "source_configured", "target_config_pending", "target_configured":
		default:
			return DatabaseReplicationStatus{}, failure("recovery_choice_unsupported", "Previous configuration restore is available only before the seed-dump boundary", 409)
		}
		if err := e.verifyNoReplicationSideEffects(ctx, source, target, *record); err != nil {
			return DatabaseReplicationStatus{}, err
		}
		if err := e.advanceDatabaseReplicationOperation(record, "recover", "configuring", "restore_target_config_pending"); err != nil {
			return DatabaseReplicationStatus{}, err
		}
	} else if record.Operation != "recover" {
		return DatabaseReplicationStatus{}, failure("recovery_choice_unsupported", "Previous configuration can be restored only before replication data or SQL changes", 409)
	}
	if record.Phase == "restore_target_config_pending" {
		if err := e.restoreDatabaseReplicationConfig(ctx, target, *record, "target"); err != nil {
			return DatabaseReplicationStatus{}, e.databaseReplicationFailure(record, err)
		}
		if err := e.advanceDatabaseReplicationOperation(record, "recover", "configuring", "restore_source_config_pending"); err != nil {
			return DatabaseReplicationStatus{}, e.databaseReplicationFailure(record, err)
		}
	}
	if record.Phase == "restore_source_config_pending" {
		if err := e.restoreDatabaseReplicationConfig(ctx, source, *record, "source"); err != nil {
			return DatabaseReplicationStatus{}, e.databaseReplicationFailure(record, err)
		}
		if err := e.advanceDatabaseReplicationOperation(record, "recover", "configuring", "config_restored"); err != nil {
			return DatabaseReplicationStatus{}, e.databaseReplicationFailure(record, err)
		}
	}
	if record.Phase == "config_restored" {
		if err := e.advanceDatabaseReplicationOperation(record, "recover", "removed", "settled"); err != nil {
			return DatabaseReplicationStatus{}, e.databaseReplicationFailure(record, err)
		}
	}
	if record.State != "removed" || record.Phase != "settled" {
		return DatabaseReplicationStatus{}, failure("operation_recovery_required", "Previous configuration restore stopped in an unknown protected phase", 409)
	}
	return replicationStatusFromRecord(*record), nil
}

func (e *PrivilegedExecutor) verifyNoReplicationSideEffects(ctx context.Context, source, target Instance, record databaseReplicationRecord) error {
	for _, item := range []struct {
		instance Instance
		source   bool
	}{{source, true}, {target, false}} {
		db, err := e.openManagedAdmin(ctx, item.instance)
		if err != nil {
			return err
		}
		channels, channelErr := readDatabaseReplicationChannels(ctx, db, item.instance.PluginID)
		if channelErr == nil && len(channels) != 0 {
			channelErr = failure("replication_recovery_unsafe", "A replication channel exists; previous configuration will not be restored over it", 409)
		}
		if channelErr == nil && item.source {
			var exists bool
			exists, channelErr = databaseReplicationAccountExists(ctx, db, record)
			if channelErr == nil && exists {
				channelErr = failure("replication_recovery_unsafe", "The generated replication account exists; previous configuration will not be restored", 409)
			}
		}
		_ = db.Close()
		if channelErr != nil {
			return channelErr
		}
	}
	return nil
}
