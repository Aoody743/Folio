package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

const (
	databaseReplicationActionOperation = "database_replication_action"
	databaseReplicationMaxRecord       = 1 << 20
	databaseReplicationMaxConfig       = 128 << 10
	databaseReplicationMaxSeed         = 4 << 30
	databaseReplicationPreviewTTL      = 15 * time.Minute
)

var (
	databaseReplicationPairPattern    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	databaseReplicationDigestPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	databaseReplicationChannelPattern = regexp.MustCompile(`^pl_[0-9a-f]{32}$`)
	databaseReplicationAccountPattern = regexp.MustCompile(`^plr_[0-9a-f]{16}$`)
)

// DatabaseReplicationInput is a closed Agent protocol. The caller identifies
// managed resources and revisions, but never supplies SQL, paths, coordinates,
// replication credentials, or generated server identities.
type DatabaseReplicationInput struct {
	Command                        string `json:"command"`
	InstanceID                     string `json:"instance_id,omitempty"`
	PairID                         string `json:"pair_id,omitempty"`
	PreviewID                      string `json:"preview_id,omitempty"`
	SnapshotSHA256                 string `json:"snapshot_sha256,omitempty"`
	SourceInstanceID               string `json:"source_instance_id"`
	TargetInstanceID               string `json:"target_instance_id"`
	Database                       string `json:"database,omitempty"`
	ExpectedSourceInstanceRevision int64  `json:"expected_source_instance_revision,omitempty"`
	ExpectedTargetInstanceRevision int64  `json:"expected_target_instance_revision,omitempty"`
	ExpectedSourceDatabaseRevision int64  `json:"expected_source_database_revision,omitempty"`
	ExpectedTargetDatabaseRevision int64  `json:"expected_target_database_revision,omitempty"`
	ExpectedPairRevision           int64  `json:"expected_pair_revision,omitempty"`
	ConfirmEmptyTarget             bool   `json:"confirm_empty_target,omitempty"`
	ConfirmRemove                  bool   `json:"confirm_remove,omitempty"`
	ConfirmReplicaHistoryLoss      bool   `json:"confirm_replica_history_loss,omitempty"`
	ConfirmTargetReadOnly          bool   `json:"confirm_target_read_only,omitempty"`
	RecoveryChoice                 string `json:"recovery_choice,omitempty"`
}

type DatabaseReplicationCapability struct {
	SourceInstanceID string   `json:"source_instance_id"`
	TargetInstanceID string   `json:"target_instance_id"`
	Engine           string   `json:"engine"`
	Supported        bool     `json:"supported"`
	Reason           string   `json:"reason,omitempty"`
	Actions          []string `json:"actions"`
}

type DatabaseReplicationPreview struct {
	PairID                    string    `json:"pair_id"`
	PreviewID                 string    `json:"preview_id"`
	ExpiresAt                 time.Time `json:"expires_at"`
	SourceInstanceID          string    `json:"source_instance_id"`
	TargetInstanceID          string    `json:"target_instance_id"`
	Database                  string    `json:"database"`
	Engine                    string    `json:"engine"`
	SourceInstanceRevision    int64     `json:"source_instance_revision"`
	TargetInstanceRevision    int64     `json:"target_instance_revision"`
	SourceDatabaseRevision    int64     `json:"source_database_revision"`
	TargetDatabaseRevision    int64     `json:"target_database_revision"`
	SourceConfigRevision      string    `json:"source_config_revision"`
	TargetConfigRevision      string    `json:"target_config_revision"`
	SourceServerID            uint32    `json:"source_server_id"`
	TargetServerID            uint32    `json:"target_server_id"`
	EstimatedSeedBytes        uint64    `json:"estimated_seed_bytes"`
	TableCount                int       `json:"table_count"`
	SchemaSHA256              string    `json:"schema_sha256"`
	SnapshotSHA256            string    `json:"snapshot_sha256"`
	ConfirmEmptyTarget        bool      `json:"confirm_empty_target_required"`
	ConfirmReplicaHistoryLoss bool      `json:"confirm_replica_history_loss_required"`
	ConfirmTargetReadOnly     bool      `json:"confirm_target_read_only_required"`
	TargetWillBeReadOnly      bool      `json:"target_will_be_read_only"`
}

type DatabaseReplicationStatus struct {
	PairID                   string   `json:"pair_id"`
	PairRevision             int64    `json:"pair_revision"`
	State                    string   `json:"state"`
	Phase                    string   `json:"phase,omitempty"`
	Operation                string   `json:"operation"`
	AllowedRecoveryChoices   []string `json:"allowed_recovery_choices"`
	Engine                   string   `json:"engine"`
	SourceInstanceID         string   `json:"source_instance_id"`
	TargetInstanceID         string   `json:"target_instance_id"`
	Database                 string   `json:"database"`
	Channel                  string   `json:"channel"`
	SourceServerID           uint32   `json:"source_server_id"`
	TargetServerID           uint32   `json:"target_server_id"`
	SourceLogFile            string   `json:"source_log_file,omitempty"`
	SourceLogPosition        uint64   `json:"source_log_position,omitempty"`
	SeedBytes                int64    `json:"seed_bytes,omitempty"`
	SeedSHA256               string   `json:"seed_sha256,omitempty"`
	TargetReadOnly           bool     `json:"target_read_only"`
	TargetSuperReadOnly      bool     `json:"target_super_read_only"`
	TargetProtectionVerified bool     `json:"target_protection_verified"`
	IOThreadRunning          bool     `json:"io_thread_running"`
	SQLThreadRunning         bool     `json:"sql_thread_running"`
	SecondsBehindSource      *uint64  `json:"seconds_behind_source,omitempty"`
	ErrorClass               string   `json:"error_class,omitempty"`
	RecoveryRequired         bool     `json:"recovery_required"`
}

// DatabaseReplicationPairSummary contains only protected pair metadata that
// is safe to show in a list after authorizing both endpoints and databases.
type DatabaseReplicationPairSummary struct {
	PairID           string `json:"pair_id"`
	PairRevision     int64  `json:"pair_revision"`
	State            string `json:"state"`
	Engine           string `json:"engine"`
	SourceInstanceID string `json:"source_instance_id"`
	TargetInstanceID string `json:"target_instance_id"`
	Database         string `json:"database"`
}

type DatabaseReplicationPairsList struct {
	Items []DatabaseReplicationPairSummary `json:"items"`
	Total int                              `json:"total"`
}

type databaseReplicationRecord struct {
	Version                 int       `json:"version"`
	PairID                  string    `json:"pair_id"`
	Revision                int64     `json:"revision"`
	State                   string    `json:"state"`
	Phase                   string    `json:"phase"`
	Operation               string    `json:"operation"`
	Engine                  string    `json:"engine"`
	SourceInstanceID        string    `json:"source_instance_id"`
	TargetInstanceID        string    `json:"target_instance_id"`
	Database                string    `json:"database"`
	SourceInstanceRevision  int64     `json:"source_instance_revision"`
	TargetInstanceRevision  int64     `json:"target_instance_revision"`
	SourceDatabaseRevision  int64     `json:"source_database_revision"`
	TargetDatabaseRevision  int64     `json:"target_database_revision"`
	SourceDataDir           string    `json:"source_data_dir"`
	TargetDataDir           string    `json:"target_data_dir"`
	SourceConfigDir         string    `json:"source_config_dir"`
	TargetConfigDir         string    `json:"target_config_dir"`
	SourceServerID          uint32    `json:"source_server_id"`
	TargetServerID          uint32    `json:"target_server_id"`
	Channel                 string    `json:"channel"`
	Account                 string    `json:"account"`
	AccountPassword         string    `json:"account_password"`
	SourceConfigBefore      []byte    `json:"source_config_before,omitempty"`
	SourceConfigAfter       []byte    `json:"source_config_after,omitempty"`
	TargetConfigBefore      []byte    `json:"target_config_before,omitempty"`
	TargetConfigAfter       []byte    `json:"target_config_after,omitempty"`
	SourceConfigSHA256      string    `json:"source_config_sha256,omitempty"`
	TargetConfigSHA256      string    `json:"target_config_sha256,omitempty"`
	SourceConfigAfterSHA256 string    `json:"source_config_after_sha256,omitempty"`
	TargetConfigAfterSHA256 string    `json:"target_config_after_sha256,omitempty"`
	SourceSchemaSHA256      string    `json:"source_schema_sha256"`
	TableCount              int       `json:"table_count"`
	SeedFilename            string    `json:"seed_filename,omitempty"`
	SeedBytes               int64     `json:"seed_bytes,omitempty"`
	SeedSHA256              string    `json:"seed_sha256,omitempty"`
	SourceLogFile           string    `json:"source_log_file,omitempty"`
	SourceLogPosition       uint64    `json:"source_log_position,omitempty"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
}

type databaseReplicationPreviewRecord struct {
	Version                int       `json:"version"`
	PairID                 string    `json:"pair_id"`
	PreviewID              string    `json:"preview_id"`
	SourceInstanceID       string    `json:"source_instance_id"`
	TargetInstanceID       string    `json:"target_instance_id"`
	Database               string    `json:"database"`
	Engine                 string    `json:"engine"`
	SourceInstanceRevision int64     `json:"source_instance_revision"`
	TargetInstanceRevision int64     `json:"target_instance_revision"`
	SourceDatabaseRevision int64     `json:"source_database_revision"`
	TargetDatabaseRevision int64     `json:"target_database_revision"`
	SourceConfigRevision   string    `json:"source_config_revision"`
	TargetConfigRevision   string    `json:"target_config_revision"`
	SourceServerID         uint32    `json:"source_server_id"`
	TargetServerID         uint32    `json:"target_server_id"`
	EstimatedSeedBytes     uint64    `json:"estimated_seed_bytes"`
	TableCount             int       `json:"table_count"`
	SchemaSHA256           string    `json:"schema_sha256"`
	CreatedAt              time.Time `json:"created_at"`
	ExpiresAt              time.Time `json:"expires_at"`
	SnapshotSHA256         string    `json:"snapshot_sha256"`
}

type databaseReplicationInstanceIndex struct {
	Version    int      `json:"version"`
	InstanceID string   `json:"instance_id"`
	PairIDs    []string `json:"pair_ids"`
}

type databaseReplicationRecoveryContextKey struct{}
type databaseReplicationRecoveryContext struct {
	executor    *PrivilegedExecutor
	pairID      string
	revision    int64
	digest      string
	instanceIDs map[string]bool
}

func databaseReplicationPairKey(pairID string) string    { return "dbrep_" + pairID }
func databaseReplicationPreviewKey(pairID string) string { return "dbrep_preview_" + pairID }
func databaseReplicationIndexKey(instanceID string) string {
	digest := sha256.Sum256([]byte(instanceID))
	return "dbrep_i_" + hex.EncodeToString(digest[:16])
}

func canonicalDatabaseReplicationEngine(engine string) string {
	switch engine {
	case "mysql", "mysql-community":
		return "mysql"
	case "mariadb":
		return "mariadb"
	default:
		return ""
	}
}

func validDatabaseReplicationPair(record databaseReplicationRecord) bool {
	if record.Version != 1 || !databaseReplicationPairPattern.MatchString(record.PairID) || record.Revision < 1 ||
		!validInstanceID(record.SourceInstanceID) || !validInstanceID(record.TargetInstanceID) || record.SourceInstanceID == record.TargetInstanceID ||
		!validDatabaseName(record.Database) || canonicalDatabaseReplicationEngine(record.Engine) == "" ||
		!databaseReplicationChannelPattern.MatchString(record.Channel) || record.Channel != "pl_"+record.PairID ||
		!databaseReplicationAccountPattern.MatchString(record.Account) || record.AccountPassword == "" || len(record.AccountPassword) > 128 ||
		record.SourceServerID == 0 || record.TargetServerID == 0 || record.SourceServerID == record.TargetServerID ||
		record.SourceInstanceRevision < 1 || record.TargetInstanceRevision < 1 || record.SourceDatabaseRevision < 1 || record.TargetDatabaseRevision < 1 ||
		record.State == "" || record.Phase == "" || record.Operation == "" || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() {
		return false
	}
	if !validReplicationState(record.State) || !validReplicationPhase(record.Phase) || !validReplicationOperation(record.Operation) {
		return false
	}
	for _, value := range [][]byte{record.SourceConfigBefore, record.SourceConfigAfter, record.TargetConfigBefore, record.TargetConfigAfter} {
		if len(value) > databaseReplicationMaxConfig {
			return false
		}
	}
	if len(record.SourceConfigBefore) != 0 && (len(record.SourceConfigAfter) == 0 || sha256Hex(record.SourceConfigBefore) != record.SourceConfigSHA256 || sha256Hex(record.SourceConfigAfter) != record.SourceConfigAfterSHA256) {
		return false
	}
	if len(record.TargetConfigBefore) != 0 && (len(record.TargetConfigAfter) == 0 || sha256Hex(record.TargetConfigBefore) != record.TargetConfigSHA256 || sha256Hex(record.TargetConfigAfter) != record.TargetConfigAfterSHA256) {
		return false
	}
	if record.SeedBytes < 0 || record.SeedBytes > databaseReplicationMaxSeed || (record.SeedFilename != "" && !artifactName.MatchString(record.SeedFilename)) || (record.SeedSHA256 != "" && !databaseReplicationDigestPattern.MatchString(record.SeedSHA256)) {
		return false
	}
	if record.SourceSchemaSHA256 != "" && (!databaseReplicationDigestPattern.MatchString(record.SourceSchemaSHA256) || record.TableCount < 1 || record.TableCount > 1000) {
		return false
	}
	if record.SeedFilename != "" && (record.SeedBytes < 1 || record.SeedSHA256 == "" || record.SourceLogFile == "" || record.SourceLogPosition == 0) {
		return false
	}
	if record.SourceLogFile != "" && !databaseBinlogNamePattern.MatchString(record.SourceLogFile) {
		return false
	}
	return true
}

func validReplicationState(state string) bool {
	switch state {
	case "pending", "recovery_required", "configuring", "seeding", "stopped", "running", "degraded", "removing", "removed":
		return true
	default:
		return false
	}
}

func validReplicationPhase(phase string) bool {
	switch phase {
	case "config_pending", "source_config_pending", "source_configured", "target_config_pending", "target_configured", "seed_dump_pending", "seed_dumped", "seed_import_pending", "seed_imported", "account_create_pending", "account_created", "account_grant_pending", "account_granted", "channel_config_pending", "channel_configured", "filter_config_pending", "channel_created", "channel_start_pending", "channel_started", "channel_stop_pending", "channel_stopped", "remove_channel_pending", "channel_removed", "remove_account_pending", "account_removed", "restore_target_config_pending", "restore_source_config_pending", "config_restored", "seed_cleanup_pending", "seed_cleaned", "settled":
		return true
	default:
		return false
	}
}

func validReplicationOperation(operation string) bool {
	switch operation {
	case "configure", "start", "stop", "remove", "recover":
		return true
	default:
		return false
	}
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func databaseReplicationRecordDigest(record databaseReplicationRecord) (string, error) {
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	return sha256Hex(encoded), nil
}

func validDatabaseReplicationPreview(record databaseReplicationPreviewRecord) bool {
	return record.Version == 1 && databaseReplicationPairPattern.MatchString(record.PairID) && databaseReplicationPairPattern.MatchString(record.PreviewID) &&
		validInstanceID(record.SourceInstanceID) && validInstanceID(record.TargetInstanceID) && record.SourceInstanceID != record.TargetInstanceID &&
		validDatabaseName(record.Database) && canonicalDatabaseReplicationEngine(record.Engine) != "" &&
		record.SourceInstanceRevision > 0 && record.TargetInstanceRevision > 0 && record.SourceDatabaseRevision > 0 && record.TargetDatabaseRevision > 0 &&
		databaseReplicationDigestPattern.MatchString(record.SourceConfigRevision) && databaseReplicationDigestPattern.MatchString(record.TargetConfigRevision) &&
		record.EstimatedSeedBytes <= databaseReplicationMaxSeed && record.TableCount > 0 && databaseReplicationDigestPattern.MatchString(record.SchemaSHA256) && record.SourceServerID > 0 && record.TargetServerID > 0 && record.SourceServerID != record.TargetServerID && record.CreatedAt.Before(record.ExpiresAt) &&
		record.ExpiresAt.Sub(record.CreatedAt) <= databaseReplicationPreviewTTL && databaseReplicationDigestPattern.MatchString(record.SnapshotSHA256)
}

func validateDatabaseReplicationInput(raw json.RawMessage) error {
	var input DatabaseReplicationInput
	if err := parse(raw, &input); err != nil {
		return err
	}
	if input.Command == "pairs_list" {
		if !validInstanceID(input.InstanceID) || input.SourceInstanceID != "" || input.TargetInstanceID != "" || input.Database != "" || input.PairID != "" || input.PreviewID != "" || input.SnapshotSHA256 != "" || input.ExpectedSourceInstanceRevision != 0 || input.ExpectedTargetInstanceRevision != 0 || input.ExpectedSourceDatabaseRevision != 0 || input.ExpectedTargetDatabaseRevision != 0 || input.ExpectedPairRevision != 0 || input.ConfirmEmptyTarget || input.ConfirmRemove || input.ConfirmReplicaHistoryLoss || input.ConfirmTargetReadOnly || input.RecoveryChoice != "" {
			return failure("invalid_input", "Replication pair listing requires only one managed instance ID", 400)
		}
		return nil
	}
	if input.InstanceID != "" {
		return failure("invalid_input", "Instance ID is accepted only when listing replication pairs", 400)
	}
	if !validInstanceID(input.SourceInstanceID) || !validInstanceID(input.TargetInstanceID) || input.SourceInstanceID == input.TargetInstanceID {
		return failure("invalid_input", "Distinct managed source and target instances are required", 400)
	}
	if input.Database != "" && !validDatabaseName(input.Database) {
		return failure("invalid_input", "A valid managed database name is required", 400)
	}
	positiveRevisions := func() bool {
		return input.ExpectedSourceInstanceRevision > 0 && input.ExpectedTargetInstanceRevision > 0 && input.ExpectedSourceDatabaseRevision > 0 && input.ExpectedTargetDatabaseRevision > 0
	}
	pairIdentity := func() bool { return databaseReplicationPairPattern.MatchString(input.PairID) }
	clearConfirmation := func() bool {
		return !input.ConfirmEmptyTarget && !input.ConfirmRemove && !input.ConfirmReplicaHistoryLoss && !input.ConfirmTargetReadOnly
	}
	switch input.Command {
	case "capabilities":
		if input.Database != "" || input.PairID != "" || input.PreviewID != "" || input.SnapshotSHA256 != "" || input.ExpectedPairRevision != 0 || input.RecoveryChoice != "" || !clearConfirmation() {
			return failure("invalid_input", "Replication capability inspection accepts only source and target IDs", 400)
		}
	case "preview":
		if !validDatabaseName(input.Database) || input.PairID != "" || input.PreviewID != "" || input.SnapshotSHA256 != "" || !positiveRevisions() || input.ExpectedPairRevision != 0 || input.RecoveryChoice != "" || !clearConfirmation() {
			return failure("invalid_input", "Replication preview requires source and target revisions and one owned database", 400)
		}
	case "configure":
		if !validDatabaseName(input.Database) || !pairIdentity() || !databaseReplicationPairPattern.MatchString(input.PreviewID) || !databaseReplicationDigestPattern.MatchString(input.SnapshotSHA256) || !positiveRevisions() || input.ExpectedPairRevision != 0 || !input.ConfirmEmptyTarget || !input.ConfirmReplicaHistoryLoss || !input.ConfirmTargetReadOnly || input.ConfirmRemove || input.RecoveryChoice != "" {
			return failure("invalid_input", "Replication configure requires an exact preview, current revisions and explicit target-read-only, empty-target and replica-history confirmations", 400)
		}
	case "status", "start", "stop", "remove":
		if !validDatabaseName(input.Database) || !pairIdentity() || input.PreviewID != "" || input.SnapshotSHA256 != "" || input.ExpectedPairRevision < 1 || input.RecoveryChoice != "" || input.ConfirmEmptyTarget ||
			(input.Command == "remove" && (!input.ConfirmRemove || !input.ConfirmReplicaHistoryLoss)) || (input.Command != "remove" && !clearConfirmation()) {
			return failure("invalid_input", "Replication pair command requires its current pair revision and any required explicit confirmation", 400)
		}
	case "recover":
		if !validDatabaseName(input.Database) || !pairIdentity() || input.PreviewID != "" || input.SnapshotSHA256 != "" || input.ExpectedPairRevision < 1 || input.RecoveryChoice != "apply_pending" && input.RecoveryChoice != "restore_previous" || !clearConfirmation() {
			return failure("invalid_input", "Replication recovery requires an explicit choice and current pair revision", 400)
		}
	default:
		return failure("invalid_input", "Unsupported typed database replication command", 400)
	}
	return nil
}

func (e *PrivilegedExecutor) validateDatabaseReplication(raw json.RawMessage) error {
	if len(raw) > 64<<10 {
		return failure("invalid_input", "Database replication input exceeds 64 KiB", 413)
	}
	if err := validateDatabaseReplicationInput(raw); err != nil {
		return err
	}
	return nil
}

func (e *PrivilegedExecutor) loadDatabaseReplication(pairID string) (*databaseReplicationRecord, error) {
	if !databaseReplicationPairPattern.MatchString(pairID) {
		return nil, failure("invalid_input", "Invalid replication pair ID", 400)
	}
	var record databaseReplicationRecord
	if err := e.load("private", databaseReplicationPairKey(pairID), &record); core.ErrorCode(err) == "not_found" {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if !validDatabaseReplicationPair(record) || record.PairID != pairID {
		return nil, failure("ownership_invalid", "Replication receipt does not match its protected pair identity", 412)
	}
	return &record, nil
}

func (e *PrivilegedExecutor) loadDatabaseReplicationIndex(instanceID string) (databaseReplicationInstanceIndex, error) {
	var index databaseReplicationInstanceIndex
	if !validInstanceID(instanceID) {
		return index, failure("invalid_input", "Invalid replication index instance", 400)
	}
	err := e.load("private", databaseReplicationIndexKey(instanceID), &index)
	if core.ErrorCode(err) == "not_found" {
		return databaseReplicationInstanceIndex{Version: 1, InstanceID: instanceID, PairIDs: []string{}}, nil
	}
	if err != nil {
		return index, err
	}
	if index.Version != 1 || index.InstanceID != instanceID || len(index.PairIDs) > 256 {
		return index, failure("ownership_invalid", "Protected replication index is invalid", 412)
	}
	seen := map[string]bool{}
	for _, pairID := range index.PairIDs {
		if !databaseReplicationPairPattern.MatchString(pairID) || seen[pairID] {
			return index, failure("ownership_invalid", "Protected replication index contains an invalid or duplicate pair reference", 412)
		}
		seen[pairID] = true
	}
	return index, nil
}

func (e *PrivilegedExecutor) saveDatabaseReplicationIndex(index databaseReplicationInstanceIndex) error {
	if index.Version != 1 || !validInstanceID(index.InstanceID) || len(index.PairIDs) > 256 {
		return failure("ownership_invalid", "Replication index cannot be saved with invalid state", 412)
	}
	seen := map[string]bool{}
	for _, pairID := range index.PairIDs {
		if !databaseReplicationPairPattern.MatchString(pairID) || seen[pairID] {
			return failure("ownership_invalid", "Replication index cannot contain invalid pair references", 412)
		}
		seen[pairID] = true
	}
	return e.save("private", databaseReplicationIndexKey(index.InstanceID), index)
}

func (e *PrivilegedExecutor) indexDatabaseReplicationPair(record databaseReplicationRecord) error {
	if !validDatabaseReplicationPair(record) {
		return failure("ownership_invalid", "Replication pair cannot be indexed before validation", 412)
	}
	for _, instanceID := range []string{record.SourceInstanceID, record.TargetInstanceID} {
		index, err := e.loadDatabaseReplicationIndex(instanceID)
		if err != nil {
			return err
		}
		found := false
		for _, pairID := range index.PairIDs {
			if pairID == record.PairID {
				found = true
				break
			}
		}
		if !found {
			index.PairIDs = append(index.PairIDs, record.PairID)
			if err = e.saveDatabaseReplicationIndex(index); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *PrivilegedExecutor) databaseReplicationPairsForInstance(instanceID string) ([]databaseReplicationRecord, error) {
	index, err := e.loadDatabaseReplicationIndex(instanceID)
	if err != nil {
		return nil, err
	}
	records := make([]databaseReplicationRecord, 0, len(index.PairIDs))
	for _, pairID := range index.PairIDs {
		record, loadErr := e.loadDatabaseReplication(pairID)
		if loadErr != nil {
			return nil, loadErr
		}
		if record == nil || (record.SourceInstanceID != instanceID && record.TargetInstanceID != instanceID) {
			return nil, failure("ownership_invalid", "Protected replication index points at a missing or unrelated pair", 412)
		}
		records = append(records, *record)
	}
	return records, nil
}

func (e *PrivilegedExecutor) ensureDatabaseReplicationJournalFinalized(ctx context.Context, instanceID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var instance Instance
	if err := e.load("receipts", instanceID, &instance); core.ErrorCode(err) == "not_found" {
		return nil
	} else if err != nil {
		return err
	}
	if instance.ID != instanceID || !validInstanceID(instanceID) {
		return failure("ownership_invalid", "Managed receipt does not match its replication guard identity", 412)
	}
	if canonicalDatabaseReplicationEngine(instance.PluginID) == "" {
		return nil
	}
	index, err := e.loadDatabaseReplicationIndex(instanceID)
	if err != nil {
		return err
	}
	marker, hasMarker := ctx.Value(databaseReplicationRecoveryContextKey{}).(databaseReplicationRecoveryContext)
	markerValid := false
	if hasMarker && marker.executor == e && marker.instanceIDs[instanceID] {
		record, loadErr := e.loadDatabaseReplication(marker.pairID)
		if loadErr != nil || record == nil || record.Revision != marker.revision {
			return failure("operation_recovery_required", "Replication recovery authorization no longer matches its protected revision", 409)
		}
		digest, digestErr := databaseReplicationRecordDigest(*record)
		if digestErr != nil || digest != marker.digest || (record.SourceInstanceID != instanceID && record.TargetInstanceID != instanceID) {
			return failure("operation_recovery_required", "Replication recovery authorization no longer matches its frozen pair", 409)
		}
		markerValid = true
	}
	for _, pairID := range index.PairIDs {
		record, loadErr := e.loadDatabaseReplication(pairID)
		if loadErr != nil {
			return loadErr
		}
		if record == nil || (record.SourceInstanceID != instanceID && record.TargetInstanceID != instanceID) {
			return failure("ownership_invalid", "Protected replication index points at a missing or unrelated pair", 412)
		}
		if markerValid && pairID == marker.pairID {
			continue
		}
		if record.State != "removed" && (record.State == "pending" || record.State == "recovery_required" || record.State == "configuring" || record.State == "seeding" || record.Phase != "settled") &&
			(record.SourceInstanceID == instanceID || record.TargetInstanceID == instanceID) {
			return failure("operation_recovery_required", "A managed replication change is unfinished; inspect and explicitly recover the pair before mutating this instance", 409)
		}
	}
	return nil
}

// ensureDatabaseReplicationDetached is for mutations that would invalidate an
// active pair's ownership: deleting either owned database, rotating either
// server administrator, changing or uninstalling either server.
func (e *PrivilegedExecutor) ensureDatabaseReplicationDetached(ctx context.Context, instanceID string) error {
	return e.ensureDatabaseReplicationDetachedForDatabase(ctx, instanceID, "")
}

func (e *PrivilegedExecutor) ensureDatabaseReplicationDatabaseDetached(ctx context.Context, instanceID, database string) error {
	if !validDatabaseName(database) {
		return failure("invalid_input", "A valid managed database name is required for the replication detach guard", 400)
	}
	return e.ensureDatabaseReplicationDetachedForDatabase(ctx, instanceID, database)
}

// ensureDatabaseReplicationTargetWritable protects application writes to the
// exact owned target schema. Source traffic remains writable. The only bypass
// is an unforgeable, in-process marker matching the exact protected
// configure-seed revision; user-supplied task context cannot create one.
func (e *PrivilegedExecutor) ensureDatabaseReplicationTargetWritable(ctx context.Context, instanceID, database string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validInstanceID(instanceID) || !validDatabaseName(database) {
		return failure("invalid_input", "A valid managed instance and database are required for the replica write guard", 400)
	}
	var instance Instance
	if err := e.load("receipts", instanceID, &instance); core.ErrorCode(err) == "not_found" {
		return nil
	} else if err != nil {
		return err
	}
	if instance.ID != instanceID {
		return failure("ownership_invalid", "Managed receipt does not match its replication write-guard identity", 412)
	}
	if canonicalDatabaseReplicationEngine(instance.PluginID) == "" {
		return nil
	}
	index, err := e.loadDatabaseReplicationIndex(instanceID)
	if err != nil {
		return err
	}
	marker, hasMarker := ctx.Value(databaseReplicationRecoveryContextKey{}).(databaseReplicationRecoveryContext)
	for _, pairID := range index.PairIDs {
		record, loadErr := e.loadDatabaseReplication(pairID)
		if loadErr != nil {
			return loadErr
		}
		if record == nil || (record.SourceInstanceID != instanceID && record.TargetInstanceID != instanceID) {
			return failure("ownership_invalid", "Protected replication index points at a missing or unrelated pair", 412)
		}
		if record.State == "removed" || record.TargetInstanceID != instanceID || record.Database != database {
			continue
		}
		if hasMarker && marker.executor == e && marker.pairID == pairID && marker.revision == record.Revision && marker.instanceIDs[instanceID] {
			digest, digestErr := databaseReplicationRecordDigest(*record)
			if digestErr == nil && digest == marker.digest && record.Operation == "configure" && record.Phase == "seed_import_pending" {
				return nil
			}
		}
		return failure("replication_target_read_only", "Writes to the managed replica target database are blocked while its replication pair is attached", 409)
	}
	return nil
}

// ensureDatabaseReplicationTargetSchemaCreateAllowed prevents adding another
// non-system database to an instance whose global read-only setting protects
// a dedicated replica target.
func (e *PrivilegedExecutor) ensureDatabaseReplicationTargetSchemaCreateAllowed(ctx context.Context, instanceID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validInstanceID(instanceID) {
		return failure("invalid_input", "A valid managed instance is required for the replica target guard", 400)
	}
	var instance Instance
	if err := e.load("receipts", instanceID, &instance); core.ErrorCode(err) == "not_found" {
		return nil
	} else if err != nil {
		return err
	}
	if canonicalDatabaseReplicationEngine(instance.PluginID) == "" {
		return nil
	}
	index, err := e.loadDatabaseReplicationIndex(instanceID)
	if err != nil {
		return err
	}
	for _, pairID := range index.PairIDs {
		record, loadErr := e.loadDatabaseReplication(pairID)
		if loadErr != nil {
			return loadErr
		}
		if record == nil || (record.SourceInstanceID != instanceID && record.TargetInstanceID != instanceID) {
			return failure("ownership_invalid", "Protected replication index points at a missing or unrelated pair", 412)
		}
		if record.State != "removed" && record.TargetInstanceID == instanceID {
			return failure("replication_target_not_dedicated", "Do not add databases to a managed replica target; remove its pair first", 409)
		}
	}
	return nil
}

func (e *PrivilegedExecutor) ensureDatabaseReplicationDetachedForDatabase(ctx context.Context, instanceID, database string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var instance Instance
	if err := e.load("receipts", instanceID, &instance); core.ErrorCode(err) == "not_found" {
		return nil
	} else if err != nil {
		return err
	}
	if instance.ID != instanceID || !validInstanceID(instanceID) {
		return failure("ownership_invalid", "Managed receipt does not match its replication ownership identity", 412)
	}
	if canonicalDatabaseReplicationEngine(instance.PluginID) == "" {
		return nil
	}
	index, err := e.loadDatabaseReplicationIndex(instanceID)
	if err != nil {
		return err
	}
	for _, pairID := range index.PairIDs {
		record, loadErr := e.loadDatabaseReplication(pairID)
		if loadErr != nil {
			return loadErr
		}
		if record == nil || (record.SourceInstanceID != instanceID && record.TargetInstanceID != instanceID) {
			return failure("ownership_invalid", "Protected replication index points at a missing or unrelated pair", 412)
		}
		if database != "" && record.Database != database {
			continue
		}
		if record.State != "removed" && (record.SourceInstanceID == instanceID || record.TargetInstanceID == instanceID) {
			return failure("replication_attached", "Remove the managed replication pair before changing or deleting its source or target database instance", 409)
		}
	}
	return nil
}

func databaseReplicationRecoveryContextFor(ctx context.Context, e *PrivilegedExecutor, record databaseReplicationRecord) (context.Context, error) {
	if !validDatabaseReplicationPair(record) || record.State == "removed" {
		return nil, failure("ownership_invalid", "Replication recovery record is not valid", 412)
	}
	digest, err := databaseReplicationRecordDigest(record)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, databaseReplicationRecoveryContextKey{}, databaseReplicationRecoveryContext{
		executor: e, pairID: record.PairID, revision: record.Revision, digest: digest,
		instanceIDs: map[string]bool{record.SourceInstanceID: true, record.TargetInstanceID: true},
	}), nil
}

func (e *PrivilegedExecutor) databaseReplication(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := e.validateDatabaseReplication(raw); err != nil {
		return nil, err
	}
	var input DatabaseReplicationInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, failure("invalid_input", "Replication command could not be decoded", 400)
	}
	switch input.Command {
	case "pairs_list":
		return e.databaseReplicationList(ctx, input.InstanceID)
	case "capabilities":
		return e.databaseReplicationCapabilities(ctx, input)
	case "preview":
		var out DatabaseReplicationPreview
		err := e.WithRuntimeTransaction(ctx, []string{input.SourceInstanceID, input.TargetInstanceID}, func(held context.Context) error {
			var inspectErr error
			out, inspectErr = e.inspectReplicationPreflight(held, input)
			return inspectErr
		})
		return out, err
	case "configure", "start", "stop", "remove":
		return e.mutateDatabaseReplication(ctx, input)
	case "status":
		return e.readDatabaseReplicationStatus(ctx, input)
	case "recover":
		return e.recoverDatabaseReplication(ctx, input)
	default:
		return nil, failure("invalid_input", "Unsupported typed database replication command", 400)
	}
}

func databaseReplicationResources(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var input DatabaseReplicationInput
	if err := parse(raw, &input); err != nil {
		return nil, err
	}
	if input.InstanceID != "" {
		if !validInstanceID(input.InstanceID) {
			return nil, failure("invalid_input", "A valid managed instance ID is required", 400)
		}
		return []string{InstanceResource(input.InstanceID), DatabaseResource(input.InstanceID, "")}, nil
	}
	if !validInstanceID(input.SourceInstanceID) || !validInstanceID(input.TargetInstanceID) || input.SourceInstanceID == input.TargetInstanceID {
		return nil, failure("invalid_input", "Distinct managed source and target IDs are required", 400)
	}
	resources := []string{InstanceResource(input.SourceInstanceID), InstanceResource(input.TargetInstanceID), DatabaseResource(input.SourceInstanceID, ""), DatabaseResource(input.TargetInstanceID, "")}
	if input.Database != "" {
		if !validDatabaseName(input.Database) {
			return nil, failure("invalid_input", "Invalid managed database name", 400)
		}
		resources = append(resources, DatabaseResource(input.SourceInstanceID, input.Database), DatabaseResource(input.TargetInstanceID, input.Database))
	}
	return resources, nil
}

func (m *Manager) databaseReplicationBindings() []core.Binding {
	instanceSchema := idSchema()
	databaseSchema := textSchema()
	pairSchema := map[string]any{"type": "string", "pattern": databaseReplicationPairPattern.String()}
	revision := map[string]any{"type": "integer", "minimum": 1}
	stringDigest := map[string]any{"type": "string", "pattern": databaseReplicationDigestPattern.String()}
	base := map[string]any{"source_instance_id": instanceSchema, "target_instance_id": instanceSchema}
	listSchema := map[string]any{"instance_id": instanceSchema}
	withDatabase := func() map[string]any {
		fields := map[string]any{}
		for key, value := range base {
			fields[key] = value
		}
		fields["database"] = databaseSchema
		for _, name := range []string{"expected_source_instance_revision", "expected_target_instance_revision", "expected_source_database_revision", "expected_target_database_revision"} {
			fields[name] = revision
		}
		return fields
	}
	bindings := []core.Binding{}
	for _, spec := range []struct {
		name, command, description, scope string
		readOnly, task                    bool
		fields                            map[string]any
		required                          []string
	}{
		{"managed_database_replication_capabilities", "capabilities", "Inspect whether two managed local MySQL/MariaDB instances meet the supported replication prerequisites", "databases:replication", true, false, base, []string{"source_instance_id", "target_instance_id"}},
		{"managed_database_replication_pairs_list", "pairs_list", "List managed replication pairs only when both endpoint instances and databases are authorized", "databases:replication", true, false, listSchema, []string{"instance_id"}},
		{"managed_database_replication_preview", "preview", "Freeze current managed source, target, empty-target and seed-size revisions before configuring database replication", "databases:admin", true, false, withDatabase(), []string{"source_instance_id", "target_instance_id", "database", "expected_source_instance_revision", "expected_target_instance_revision", "expected_source_database_revision", "expected_target_database_revision"}},
		{"managed_database_replication_configure", "configure", "Seed one owned database consistently and configure a protected single-channel same-host replication pair", "databases:admin", false, true, map[string]any{"pair_id": pairSchema, "preview_id": pairSchema, "snapshot_sha256": stringDigest, "source_instance_id": instanceSchema, "target_instance_id": instanceSchema, "database": databaseSchema, "expected_source_instance_revision": revision, "expected_target_instance_revision": revision, "expected_source_database_revision": revision, "expected_target_database_revision": revision, "confirm_empty_target": map[string]any{"type": "boolean", "const": true}, "confirm_replica_history_loss": map[string]any{"type": "boolean", "const": true}, "confirm_target_read_only": map[string]any{"type": "boolean", "const": true}}, []string{"pair_id", "preview_id", "snapshot_sha256", "source_instance_id", "target_instance_id", "database", "expected_source_instance_revision", "expected_target_instance_revision", "expected_source_database_revision", "expected_target_database_revision", "confirm_empty_target", "confirm_replica_history_loss", "confirm_target_read_only"}},
		{"managed_database_replication_pair_status", "status", "Read only the exact stored pair's bounded and sanitized replication thread status", "databases:replication", true, false, map[string]any{"pair_id": pairSchema, "source_instance_id": instanceSchema, "target_instance_id": instanceSchema, "database": databaseSchema, "expected_pair_revision": revision}, []string{"pair_id", "source_instance_id", "target_instance_id", "database", "expected_pair_revision"}},
		{"managed_database_replication_start", "start", "Start only the exact managed replication channel after verifying its frozen source identity", "databases:admin", false, true, map[string]any{"pair_id": pairSchema, "source_instance_id": instanceSchema, "target_instance_id": instanceSchema, "database": databaseSchema, "expected_pair_revision": revision}, []string{"pair_id", "source_instance_id", "target_instance_id", "database", "expected_pair_revision"}},
		{"managed_database_replication_stop", "stop", "Stop only the exact managed replication channel and retain its seed and metadata", "databases:admin", false, true, map[string]any{"pair_id": pairSchema, "source_instance_id": instanceSchema, "target_instance_id": instanceSchema, "database": databaseSchema, "expected_pair_revision": revision}, []string{"pair_id", "source_instance_id", "target_instance_id", "database", "expected_pair_revision"}},
		{"managed_database_replication_remove", "remove", "Detach the exact managed channel and generated replication account after explicit replica-history-loss confirmation; target data is retained", "databases:admin", false, true, map[string]any{"pair_id": pairSchema, "source_instance_id": instanceSchema, "target_instance_id": instanceSchema, "database": databaseSchema, "expected_pair_revision": revision, "confirm_remove": map[string]any{"type": "boolean", "const": true}, "confirm_replica_history_loss": map[string]any{"type": "boolean", "const": true}}, []string{"pair_id", "source_instance_id", "target_instance_id", "database", "expected_pair_revision", "confirm_remove", "confirm_replica_history_loss"}},
		{"managed_database_replication_recover", "recover", "Explicitly continue or restore a journaled replication change after checking its exact config and SQL state", "databases:admin", false, true, map[string]any{"pair_id": pairSchema, "source_instance_id": instanceSchema, "target_instance_id": instanceSchema, "database": databaseSchema, "expected_pair_revision": revision, "recovery_choice": enumSchema("apply_pending", "restore_previous")}, []string{"pair_id", "source_instance_id", "target_instance_id", "database", "expected_pair_revision", "recovery_choice"}},
	} {
		spec := spec
		binding := m.binding(spec.name, spec.description, spec.scope, spec.readOnly, obj(spec.fields, spec.required...), databaseReplicationResources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			var input DatabaseReplicationInput
			if err := parse(raw, &input); err != nil {
				return core.ActionResult{}, err
			}
			input.Command = spec.command
			forward, _ := json.Marshal(input)
			resources, err := databaseReplicationResources(ctx, actor, forward)
			if err != nil {
				return core.ActionResult{}, err
			}
			if spec.task {
				job, submitErr := m.deps.Tasks.Submit(ctx, actor, databaseReplicationActionOperation, forward, resources)
				if submitErr != nil {
					return core.ActionResult{}, submitErr
				}
				id, _ := job["id"].(string)
				return core.ActionResult{Data: job, TaskID: id, Resources: resources}, nil
			}
			var output any
			if err = m.agentJSON(ctx, databaseReplicationActionOperation, forward, &output); err != nil {
				return core.ActionResult{}, err
			}
			if spec.command == "pairs_list" {
				output, err = filterDatabaseReplicationPairs(actor, input.InstanceID, output, m.deps.Authorize)
				if err != nil {
					return core.ActionResult{}, err
				}
			}
			return core.ActionResult{Data: output, Resources: resources}, nil
		})
		binding.Action.Task = spec.task
		bindings = append(bindings, binding)
	}
	return bindings
}

func filterDatabaseReplicationPairs(actor core.Actor, requestedInstanceID string, output any, authorize func(core.Actor, string, string) bool) (DatabaseReplicationPairsList, error) {
	var listed DatabaseReplicationPairsList
	encoded, err := json.Marshal(output)
	if err != nil || json.Unmarshal(encoded, &listed) != nil || len(listed.Items) > 256 {
		return DatabaseReplicationPairsList{}, failure("provider_contract", "Replication pair list response was malformed", 500)
	}
	visible := make([]DatabaseReplicationPairSummary, 0, len(listed.Items))
	for _, pair := range listed.Items {
		if !validInstanceID(pair.SourceInstanceID) || !validInstanceID(pair.TargetInstanceID) || pair.SourceInstanceID == pair.TargetInstanceID || !validDatabaseName(pair.Database) || !databaseReplicationPairPattern.MatchString(pair.PairID) || pair.PairRevision < 1 || !validReplicationState(pair.State) || canonicalDatabaseReplicationEngine(pair.Engine) == "" {
			return DatabaseReplicationPairsList{}, failure("provider_contract", "Replication pair list contains invalid protected metadata", 500)
		}
		if pair.SourceInstanceID != requestedInstanceID && pair.TargetInstanceID != requestedInstanceID {
			return DatabaseReplicationPairsList{}, failure("provider_contract", "Replication pair list returned an unrelated pair", 500)
		}
		resources := []string{
			InstanceResource(pair.SourceInstanceID), InstanceResource(pair.TargetInstanceID),
			DatabaseResource(pair.SourceInstanceID, pair.Database), DatabaseResource(pair.TargetInstanceID, pair.Database),
		}
		permitted := true
		for _, resource := range resources {
			if !authorize(actor, "databases:replication", resource) {
				permitted = false
				break
			}
		}
		if permitted {
			visible = append(visible, pair)
		}
	}
	listed.Items = visible
	listed.Total = len(visible)
	return listed, nil
}
