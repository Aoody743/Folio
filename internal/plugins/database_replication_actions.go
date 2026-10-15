package plugins

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func (e *PrivilegedExecutor) mutateDatabaseReplication(ctx context.Context, input DatabaseReplicationInput) (any, error) {
	if input.Command == "configure" {
		var out DatabaseReplicationStatus
		err := e.WithRuntimeTransaction(ctx, []string{input.SourceInstanceID, input.TargetInstanceID}, func(held context.Context) error {
			for _, id := range []string{input.SourceInstanceID, input.TargetInstanceID} {
				if guardErr := e.ensureDatabaseReplicationJournalFinalized(held, id); guardErr != nil {
					return guardErr
				}
			}
			var configureErr error
			out, configureErr = e.configureDatabaseReplication(held, input)
			return configureErr
		})
		return out, err
	}
	record, err := e.loadDatabaseReplication(input.PairID)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, failure("replication_pair_not_found", "Managed replication pair does not exist", 404)
	}
	if err = validateReplicationPairInput(*record, input); err != nil {
		return nil, err
	}
	if record.Revision != input.ExpectedPairRevision {
		return nil, failure("revision_conflict", "Replication pair changed; refresh its status before retrying", 412)
	}
	var out DatabaseReplicationStatus
	err = e.WithRuntimeTransaction(ctx, []string{record.SourceInstanceID, record.TargetInstanceID}, func(held context.Context) error {
		current, loadErr := e.loadDatabaseReplication(record.PairID)
		if loadErr != nil {
			return loadErr
		}
		if current == nil || current.Revision != input.ExpectedPairRevision {
			return failure("revision_conflict", "Replication pair changed before its command began", 412)
		}
		for _, id := range []string{current.SourceInstanceID, current.TargetInstanceID} {
			if guardErr := e.ensureDatabaseReplicationJournalFinalized(held, id); guardErr != nil {
				return guardErr
			}
		}
		source, target, _, _, endpointErr := e.loadReplicationPairEndpoints(held, *current)
		if endpointErr != nil {
			return endpointErr
		}
		switch input.Command {
		case "start":
			out, endpointErr = e.startDatabaseReplication(held, source, target, *current)
		case "stop":
			out, endpointErr = e.stopDatabaseReplication(held, source, target, *current)
		case "remove":
			out, endpointErr = e.removeDatabaseReplication(held, source, target, *current)
		}
		return endpointErr
	})
	return out, err
}

func (e *PrivilegedExecutor) configureDatabaseReplication(ctx context.Context, input DatabaseReplicationInput) (DatabaseReplicationStatus, error) {
	var empty DatabaseReplicationStatus
	var frozen databaseReplicationPreviewRecord
	if err := e.load("private", databaseReplicationPreviewKey(input.PairID), &frozen); err != nil || !validDatabaseReplicationPreview(frozen) {
		return empty, failure("replication_preview_unavailable", "The exact protected replication preview is unavailable", 412)
	}
	if frozen.PairID != input.PairID || frozen.PreviewID != input.PreviewID || frozen.SnapshotSHA256 != input.SnapshotSHA256 || !time.Now().UTC().Before(frozen.ExpiresAt) ||
		frozen.SourceInstanceID != input.SourceInstanceID || frozen.TargetInstanceID != input.TargetInstanceID || frozen.Database != input.Database ||
		frozen.SourceInstanceRevision != input.ExpectedSourceInstanceRevision || frozen.TargetInstanceRevision != input.ExpectedTargetInstanceRevision ||
		frozen.SourceDatabaseRevision != input.ExpectedSourceDatabaseRevision || frozen.TargetDatabaseRevision != input.ExpectedTargetDatabaseRevision {
		return empty, failure("revision_conflict", "Replication preview expired or no longer matches the selected endpoint revisions", 412)
	}
	if existing, err := e.loadDatabaseReplication(input.PairID); err != nil {
		return empty, err
	} else if existing != nil {
		return empty, failure("replication_pair_exists", "This preview already has a protected replication pair record", 409)
	}
	currentPreview, err := e.inspectReplicationPreflight(ctx, input)
	if err != nil {
		return empty, err
	}
	if !sameReplicationPreview(frozen, currentPreview) {
		return empty, failure("revision_conflict", "Managed endpoint configuration, table inventory or empty target changed after preview", 412)
	}
	source, err := e.get(ctx, input.SourceInstanceID, false)
	if err != nil {
		return empty, err
	}
	target, err := e.get(ctx, input.TargetInstanceID, false)
	if err != nil {
		return empty, err
	}
	_, sourceConfig, err := e.readDatabaseBinlogConfig(source)
	if err != nil {
		return empty, err
	}
	_, targetConfig, err := e.readDatabaseBinlogConfig(target)
	if err != nil {
		return empty, err
	}
	if sha256Hex(sourceConfig.Body) != frozen.SourceConfigRevision || sha256Hex(targetConfig.Body) != frozen.TargetConfigRevision {
		return empty, failure("revision_conflict", "Managed configuration changed after preview", 412)
	}
	sourceConfigAfter, err := databaseReplicationConfigAfter(sourceConfig.Body, input.PairID, frozen.Engine, "source", input.Database, frozen.SourceServerID)
	if err != nil {
		return empty, err
	}
	targetConfigAfter, err := databaseReplicationConfigAfter(targetConfig.Body, input.PairID, frozen.Engine, "target", input.Database, frozen.TargetServerID)
	if err != nil {
		return empty, err
	}
	account, password, err := databaseReplicationNewAccount()
	if err != nil {
		return empty, failure("identity_generation_failed", "A protected replication account could not be generated", 500)
	}
	now := time.Now().UTC()
	record := databaseReplicationRecord{
		Version: 1, PairID: input.PairID, Revision: 1, State: "configuring", Phase: "config_pending", Operation: "configure",
		Engine: frozen.Engine, SourceInstanceID: source.ID, TargetInstanceID: target.ID, Database: input.Database,
		SourceInstanceRevision: source.Revision, TargetInstanceRevision: target.Revision,
		SourceDatabaseRevision: frozen.SourceDatabaseRevision, TargetDatabaseRevision: frozen.TargetDatabaseRevision,
		SourceDataDir: source.Paths.DataDir, TargetDataDir: target.Paths.DataDir,
		SourceConfigDir: source.Paths.ConfigDir, TargetConfigDir: target.Paths.ConfigDir,
		SourceServerID: frozen.SourceServerID, TargetServerID: frozen.TargetServerID,
		Channel: "pl_" + input.PairID, Account: account, AccountPassword: password,
		SourceConfigBefore: append([]byte(nil), sourceConfig.Body...), SourceConfigAfter: sourceConfigAfter,
		TargetConfigBefore: append([]byte(nil), targetConfig.Body...), TargetConfigAfter: targetConfigAfter,
		SourceConfigSHA256: sha256Hex(sourceConfig.Body), TargetConfigSHA256: sha256Hex(targetConfig.Body),
		SourceConfigAfterSHA256: sha256Hex(sourceConfigAfter), TargetConfigAfterSHA256: sha256Hex(targetConfigAfter),
		SourceSchemaSHA256: frozen.SchemaSHA256, TableCount: frozen.TableCount,
		CreatedAt: now, UpdatedAt: now,
	}
	if !validDatabaseReplicationPair(record) {
		return empty, failure("replication_intent_invalid", "Protected replication intent could not be validated", 500)
	}
	// Persist the journal before either index or any external file/SQL effect.
	if err = e.save("private", databaseReplicationPairKey(record.PairID), record); err != nil {
		return empty, failure("operation_recovery_required", "Protected replication intent could not be written; no endpoint was changed", 409)
	}
	if err = e.indexDatabaseReplicationPair(record); err != nil {
		return empty, failure("operation_recovery_required", "Replication intent is durable but endpoint indexing is incomplete; recover this exact pair before retrying", 409)
	}
	return e.continueDatabaseReplicationConfigure(ctx, source, target, &record, false)
}

func sameReplicationPreview(frozen databaseReplicationPreviewRecord, current DatabaseReplicationPreview) bool {
	return frozen.PairID == current.PairID && frozen.PreviewID == current.PreviewID && frozen.SourceInstanceID == current.SourceInstanceID && frozen.TargetInstanceID == current.TargetInstanceID &&
		frozen.Database == current.Database && frozen.Engine == current.Engine && frozen.SourceInstanceRevision == current.SourceInstanceRevision && frozen.TargetInstanceRevision == current.TargetInstanceRevision &&
		frozen.SourceDatabaseRevision == current.SourceDatabaseRevision && frozen.TargetDatabaseRevision == current.TargetDatabaseRevision && frozen.SourceConfigRevision == current.SourceConfigRevision && frozen.TargetConfigRevision == current.TargetConfigRevision &&
		frozen.SourceServerID == current.SourceServerID && frozen.TargetServerID == current.TargetServerID && frozen.EstimatedSeedBytes == current.EstimatedSeedBytes && frozen.TableCount == current.TableCount && frozen.SchemaSHA256 == current.SchemaSHA256
}

func databaseReplicationNewAccount() (string, string, error) {
	userRaw := make([]byte, 8)
	passwordRaw := make([]byte, 32)
	if _, err := rand.Read(userRaw); err != nil {
		return "", "", err
	}
	if _, err := rand.Read(passwordRaw); err != nil {
		return "", "", err
	}
	return "plr_" + hex.EncodeToString(userRaw), base64.RawURLEncoding.EncodeToString(passwordRaw), nil
}

func (e *PrivilegedExecutor) continueDatabaseReplicationConfigure(ctx context.Context, source, target Instance, record *databaseReplicationRecord, explicitRecovery bool) (DatabaseReplicationStatus, error) {
	fail := func(err error) (DatabaseReplicationStatus, error) {
		return DatabaseReplicationStatus{}, e.databaseReplicationFailure(record, err)
	}
	if record.Phase == "config_pending" {
		if err := e.advanceDatabaseReplication(record, "configuring", "source_config_pending"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "source_config_pending" {
		if err := e.applyDatabaseReplicationConfig(ctx, source, *record, "source"); err != nil {
			return fail(err)
		}
		if err := e.advanceDatabaseReplication(record, "configuring", "source_configured"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "source_configured" {
		if err := e.advanceDatabaseReplication(record, "configuring", "target_config_pending"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "target_config_pending" {
		if err := e.applyDatabaseReplicationConfig(ctx, target, *record, "target"); err != nil {
			return fail(err)
		}
		if err := e.advanceDatabaseReplication(record, "configuring", "target_configured"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "target_configured" {
		if err := e.verifyReplicationConfig(ctx, source, target, *record, true); err != nil {
			return fail(err)
		}
		if err := e.advanceDatabaseReplication(record, "seeding", "seed_dump_pending"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "seed_dump_pending" {
		filename, size, digest, logFile, logPosition, err := e.createDatabaseReplicationSeed(ctx, source, target, *record)
		if err != nil {
			return fail(err)
		}
		record.SeedFilename, record.SeedBytes, record.SeedSHA256 = filename, size, digest
		record.SourceLogFile, record.SourceLogPosition = logFile, logPosition
		if err = e.advanceDatabaseReplication(record, "seeding", "seed_dumped"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "seed_dumped" {
		if err := e.advanceDatabaseReplication(record, "seeding", "seed_import_pending"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "seed_import_pending" {
		seedCtx, markerErr := databaseReplicationRecoveryContextFor(ctx, e, *record)
		if markerErr != nil {
			return fail(markerErr)
		}
		var err error
		if explicitRecovery {
			err = e.recoverDatabaseReplicationSeedImport(seedCtx, target, *record)
		} else {
			err = e.importDatabaseReplicationSeed(seedCtx, target, *record)
		}
		if err != nil {
			return fail(err)
		}
		if err := e.advanceDatabaseReplication(record, "seeding", "seed_imported"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "seed_imported" {
		if err := e.advanceDatabaseReplication(record, "configuring", "account_create_pending"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "account_create_pending" {
		if err := e.createDatabaseReplicationAccount(ctx, source, *record); err != nil {
			return fail(err)
		}
		if err := e.advanceDatabaseReplication(record, "configuring", "account_created"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "account_created" {
		if err := e.advanceDatabaseReplication(record, "configuring", "account_grant_pending"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "account_grant_pending" {
		if err := e.grantDatabaseReplicationAccount(ctx, source, *record); err != nil {
			return fail(err)
		}
		if err := e.advanceDatabaseReplication(record, "configuring", "account_granted"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "account_granted" {
		if err := e.advanceDatabaseReplication(record, "configuring", "channel_config_pending"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "channel_config_pending" {
		if err := e.configureDatabaseReplicationChannel(ctx, source, target, *record); err != nil {
			return fail(err)
		}
		if err := e.advanceDatabaseReplication(record, "configuring", "channel_configured"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "channel_configured" {
		if record.Engine == "mysql" {
			if err := e.advanceDatabaseReplication(record, "configuring", "filter_config_pending"); err != nil {
				return fail(err)
			}
		} else if err := e.advanceDatabaseReplication(record, "configuring", "channel_created"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "filter_config_pending" {
		if err := e.configureDatabaseReplicationFilter(ctx, target, *record); err != nil {
			return fail(err)
		}
		if err := e.advanceDatabaseReplication(record, "configuring", "channel_created"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "channel_created" {
		if err := e.advanceDatabaseReplication(record, "configuring", "channel_start_pending"); err != nil {
			return fail(err)
		}
	}
	if record.Phase == "channel_start_pending" {
		if err := e.startReplicationChannel(ctx, source, target, *record); err != nil {
			return fail(err)
		}
		status, err := e.readLiveDatabaseReplicationStatus(ctx, source, target, *record)
		if err != nil {
			return fail(err)
		}
		state := status.State
		if state == "stopped" {
			return fail(failure("replication_start_failed", "Replication channel did not start; inspect the sanitized pair status before recovery", 409))
		}
		if err = e.advanceDatabaseReplication(record, state, "settled"); err != nil {
			return fail(err)
		}
	}
	status, err := e.readLiveDatabaseReplicationStatus(ctx, source, target, *record)
	if err != nil {
		return fail(err)
	}
	status.PairRevision = record.Revision
	status.State = record.State
	status.Phase = record.Phase
	status.RecoveryRequired = false
	return status, nil
}

func (e *PrivilegedExecutor) advanceDatabaseReplication(record *databaseReplicationRecord, state, phase string) error {
	if record == nil {
		return failure("ownership_invalid", "Replication journal transition has no current record", 412)
	}
	return e.advanceDatabaseReplicationOperation(record, record.Operation, state, phase)
}

func (e *PrivilegedExecutor) databaseReplicationFailure(record *databaseReplicationRecord, cause error) error {
	if record != nil && record.State != "removed" && record.State != "recovery_required" {
		_ = e.advanceDatabaseReplication(record, "recovery_required", record.Phase)
	}
	if cause == nil {
		return failure("operation_recovery_required", "Replication operation requires explicit recovery", 409)
	}
	if core.ErrorCode(cause) != "" {
		return cause
	}
	return failure("operation_recovery_required", "Replication operation stopped before its next durable phase; inspect and explicitly recover the pair", 409)
}

func (e *PrivilegedExecutor) applyDatabaseReplicationConfig(ctx context.Context, instance Instance, record databaseReplicationRecord, role string) error {
	before, after, expectedBefore, expectedAfter := record.SourceConfigBefore, record.SourceConfigAfter, record.SourceConfigSHA256, record.SourceConfigAfterSHA256
	if role == "target" {
		before, after, expectedBefore, expectedAfter = record.TargetConfigBefore, record.TargetConfigAfter, record.TargetConfigSHA256, record.TargetConfigAfterSHA256
	}
	_, current, err := e.readDatabaseBinlogConfig(instance)
	if err != nil {
		return err
	}
	if sha256Hex(before) != expectedBefore || sha256Hex(after) != expectedAfter {
		return failure("ownership_invalid", "Replication configuration journal digest does not match its exact bytes", 412)
	}
	if string(current.Body) != string(before) && string(current.Body) != string(after) {
		return failure("configuration_unmanaged", "Managed database configuration differs from both journaled images", 412)
	}
	if string(current.Body) != string(after) {
		if err = writeAtomicRoot(instance.Paths.ConfigDir, "my.cnf", after, current.Mode, current.GID); err != nil {
			return failure("operation_recovery_required", "Replication configuration could not be written; inspect the exact journal before recovery", 409)
		}
	}
	if e.probe(ctx, instance).Status != "healthy" {
		return failure("runtime_unavailable", "Managed database must be healthy before the replication configuration restart", 409)
	}
	if err = e.verifyReplicationRoleRuntime(ctx, instance, role, record); err == nil {
		return nil
	}
	markerCtx, markerErr := databaseReplicationRecoveryContextFor(ctx, e, record)
	if markerErr != nil {
		return markerErr
	}
	if _, err = e.controlLocked(markerCtx, ServiceInput{ID: instance.ID, Command: "restart"}, instance); err != nil {
		return failure("operation_recovery_required", "Managed database restart after replication configuration requires explicit recovery", 409)
	}
	return e.verifyReplicationRoleRuntime(ctx, instance, role, record)
}

func (e *PrivilegedExecutor) verifyReplicationRoleRuntime(ctx context.Context, instance Instance, role string, record databaseReplicationRecord) error {
	db, err := e.openManagedAdmin(ctx, instance)
	if err != nil {
		return err
	}
	defer db.Close()
	runtime, err := readDatabaseReplicationRuntime(ctx, db, instance.PluginID)
	if err != nil {
		return err
	}
	if role == "source" {
		if runtime.ServerID != record.SourceServerID || !runtime.LogBin || runtime.Format != "ROW" {
			return failure("replication_runtime_mismatch", "Source server did not start with its exact managed server ID, active binary log and ROW format", 412)
		}
		return nil
	}
	if runtime.ServerID != record.TargetServerID {
		return failure("replication_runtime_mismatch", "Target server did not start with its exact managed server ID", 412)
	}
	if !runtime.ReadOnly || record.Engine == "mysql" && !runtime.SuperReadOnly {
		return failure("replication_target_protection_unverified", "Target runtime did not confirm the required read-only protection", 412)
	}
	if record.Engine == "mariadb" {
		filter, filterErr := readDatabaseReplicationVariable(ctx, db, "replicate_wild_do_table")
		if filterErr != nil {
			return filterErr
		}
		if filter != databaseReplicationSQLWildcard(record.Database) {
			return failure("replication_filter_unverified", "MariaDB did not load the exact escaped table filter from the managed config", 412)
		}
	}
	return nil
}

var _ = time.Second
