package plugins

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestDatabaseReplicationConfigIntentFreezesExactManagedDelta(t *testing.T) {
	pairID := strings.Repeat("a", 32)
	sourceID, targetID, err := databaseReplicationServerIDs(pairID)
	if err != nil || sourceID == 0 || targetID == 0 || sourceID == targetID {
		t.Fatal("generated source and target server IDs must be distinct and nonzero", sourceID, targetID, err)
	}
	base := []byte("[mysqld]\nbind-address=127.0.0.1\nport=3307\n")
	source, err := databaseReplicationConfigAfter(base, pairID, "mysql", "source", "owned_db", sourceID)
	if err != nil || !strings.HasSuffix(string(source), databaseReplicationConfigMarker+"\n# pair="+pairID+" role=source\n[mysqld]\nserver-id="+fmtUint(sourceID)+"\nlog-bin=mysql-bin\nbinlog_format=ROW\n") {
		t.Fatal("source config must append only the exact owned ROW/binlog block", err, string(source))
	}
	if strings.Contains(string(source), "replicate-wild-do-table") {
		t.Fatal("MySQL source config must not contain a global replica filter")
	}
	target, err := databaseReplicationConfigAfter(base, pairID, "mysql", "target", "owned_db", targetID)
	if err != nil || !strings.HasSuffix(string(target), databaseReplicationConfigMarker+"\n# pair="+pairID+" role=target\n[mysqld]\nserver-id="+fmtUint(targetID)+"\nread_only=ON\nsuper_read_only=ON\n") {
		t.Fatal("MySQL target config must enable read-only and super-read-only without a global filter", err, string(target))
	}
	maria, err := databaseReplicationConfigAfter(base, pairID, "mariadb", "target", "owned_db", targetID)
	if err != nil || !strings.HasSuffix(string(maria), databaseReplicationConfigMarker+"\n# pair="+pairID+" role=target\n[mysqld]\nserver-id="+fmtUint(targetID)+"\nread_only=ON\nreplicate-wild-do-table=owned\\\\_db.%\n") {
		t.Fatal("MariaDB target must enable read-only and carry the exact managed database table filter", err, string(maria))
	}
	if got := databaseReplicationSQLWildcard("owned_db"); got != `owned\_db.%` {
		t.Fatal("MySQL channel filter did not escape underscore as a literal", got)
	}
	if got := databaseReplicationSQLWildcard("owned-db"); got != "owned-db.%" {
		t.Fatal("non-wildcard database characters were changed", got)
	}
	if _, ok := databaseReplicationConfigAtExpectedState(source, base, source); !ok {
		t.Fatal("exact staged config bytes should match the journaled after image")
	}
	if _, ok := databaseReplicationConfigAtExpectedState(append(source, []byte("# unreviewed change\n")...), base, source); ok {
		t.Fatal("journal accepted config bytes beyond the exact before/after images")
	}
}

func TestDatabaseReplicationSeedCLIArgvSelectsExactDatabase(t *testing.T) {
	const config = "/private/db-client.cnf"
	const database = "billing_prod"
	dumpArgs, err := databaseReplicationDumpArgs(config, database)
	if err != nil || len(dumpArgs) < 2 || dumpArgs[0] != "--defaults-file="+config || dumpArgs[len(dumpArgs)-1] != database {
		t.Fatal("dump argv must select exactly the validated source schema", dumpArgs, err)
	}
	if !strings.Contains(strings.Join(dumpArgs, "\x00"), "--master-data=2") || !strings.Contains(strings.Join(dumpArgs, "\x00"), "--single-transaction") || !strings.Contains(strings.Join(dumpArgs, "\x00"), "--skip-lock-tables") {
		t.Fatal("dump argv fixture lost the expected coordinate/snapshot options", dumpArgs)
	}
	importArgs, err := databaseReplicationImportArgs(config, database)
	if err != nil || len(importArgs) < 2 || importArgs[0] != "--defaults-file="+config || importArgs[len(importArgs)-1] != database {
		t.Fatal("import argv must select exactly the validated target schema even when the dump has no USE", importArgs, err)
	}
	for _, arg := range importArgs {
		if arg == "--databases" || arg == "--all-databases" {
			t.Fatal("import argv must select one exact database without broadening scope", importArgs)
		}
	}
	// This fixture checks our argv contract only. It does not claim MySQL 8.4 or
	// MariaDB native client compatibility for --master-data=2.
}

func TestDatabaseReplicationSeedArtifactsStayAgentPrivateAndRejectLinkAttacks(t *testing.T) {
	e := testAgent(t)
	record := replicationTestRecord(time.Now().UTC(), "f", "mysql-source", "mysql-target")
	seed := []byte("-- CHANGE MASTER TO MASTER_LOG_FILE='mysql-bin.000001', MASTER_LOG_POS=4;\nCREATE TABLE owned_db.t (id INT);\n-- Dump completed on 2026-10-15\n")
	record.SeedFilename = databaseReplicationSeedFilename(record.PairID)
	record.SeedBytes = int64(len(seed))
	record.SeedSHA256 = sha256Hex(seed)
	root, directory, seedDir, err := e.openDatabaseReplicationSeedDirectory(true)
	if err != nil {
		t.Fatal("create private seed store", err)
	}
	defer root.Close()
	defer directory.Close()
	dirInfo, err := directory.Stat()
	if err != nil {
		t.Fatal("stat private seed directory", err)
	}
	owner, ownerOK := dirInfo.Sys().(*syscall.Stat_t)
	if !dirInfo.IsDir() || dirInfo.Mode().Perm() != 0700 || !ownerOK || owner.Uid != uint32(os.Geteuid()) {
		t.Fatal("seed store is not an Agent-owned 0700 directory", dirInfo, err)
	}
	writeSeed := func() {
		t.Helper()
		file, openErr := root.OpenFile(record.SeedFilename, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if openErr != nil {
			t.Fatal("create private seed fixture", openErr)
		}
		if _, openErr = file.Write(seed); openErr == nil {
			openErr = file.Sync()
		}
		closeErr := file.Close()
		if openErr == nil {
			openErr = closeErr
		}
		if openErr != nil {
			t.Fatal("write private seed fixture", openErr)
		}
	}
	writeSeed()
	if err = directory.Sync(); err != nil {
		t.Fatal("sync seed fixture directory", err)
	}
	verified, err := e.openVerifiedDatabaseReplicationSeed(context.Background(), Instance{ID: record.TargetInstanceID}, record)
	if err != nil {
		t.Fatal("valid Agent-private seed should verify", err)
	}
	verified.Close()
	if _, err = os.Stat(filepath.Join(seedDir, record.SeedFilename)); err != nil {
		t.Fatal("seed should be stored in protected Agent state", err)
	}
	if err = root.Chmod(record.SeedFilename, 0640); err != nil {
		t.Fatal("change seed mode for adversarial test", err)
	}
	if _, err = e.openVerifiedDatabaseReplicationSeed(context.Background(), Instance{ID: record.TargetInstanceID}, record); core.ErrorCode(err) != "artifact_invalid" {
		t.Fatal("group-readable seed artifact was accepted", err)
	}
	if err = root.Remove(record.SeedFilename); err != nil {
		t.Fatal("remove seed before hard-link test", err)
	}
	writeSeed()
	if err = root.Link(record.SeedFilename, "alias.sql"); err != nil {
		t.Fatal("create adversarial hard link", err)
	}
	if _, err = e.openVerifiedDatabaseReplicationSeed(context.Background(), Instance{ID: record.TargetInstanceID}, record); core.ErrorCode(err) != "artifact_invalid" {
		t.Fatal("hard-linked seed artifact was accepted", err)
	}
	_ = root.Remove("alias.sql")
	_ = root.Remove(record.SeedFilename)
	if err = root.Symlink("missing-target.sql", record.SeedFilename); err != nil {
		t.Fatal("create adversarial symlink", err)
	}
	if _, err = e.openVerifiedDatabaseReplicationSeed(context.Background(), Instance{ID: record.TargetInstanceID}, record); core.ErrorCode(err) != "artifact_invalid" {
		t.Fatal("symlink seed artifact was accepted", err)
	}
}

func TestDatabaseReplicationConfigReplacesOnlyExactManagedBinlogBlock(t *testing.T) {
	pairID := strings.Repeat("b", 32)
	sourceID, targetID, _ := databaseReplicationServerIDs(pairID)
	base := []byte("[mysqld]\nbind-address=127.0.0.1\nport=3307\n\n" + databaseBinlogConfigMarker + "\n[mysqld]\nlog-bin=mysql-bin\nbinlog_format=mixed\n")
	if _, managed, state := parseDatabaseBinlogConfig(base); !managed || state != "settled" {
		t.Fatal("test requires an exact current managed binlog marker")
	}
	source, err := databaseReplicationConfigAfter(base, pairID, "mysql", "source", "app_data", sourceID)
	if err != nil || strings.Contains(string(source), "binlog_format=mixed") || strings.Count(string(source), databaseReplicationConfigMarker) != 1 {
		t.Fatal("ROW source setup must replace the exact prior managed binlog suffix", err, string(source))
	}
	if !strings.HasPrefix(string(source), "[mysqld]\nbind-address=127.0.0.1\nport=3307\n\n") {
		t.Fatal("source configuration changed bytes before the managed binlog marker")
	}
	if _, err = databaseReplicationConfigAfter([]byte("[mysqld]\nserver-id=41\n"), pairID, "mysql", "source", "app_data", sourceID); err == nil {
		t.Fatal("unmanaged source server ID was silently overwritten")
	}
	if _, err = databaseReplicationConfigAfter([]byte("[mysqld]\nreplicate-do-db=other\n"), pairID, "mariadb", "target", "app_data", targetID); err == nil {
		t.Fatal("unmanaged target replication filter was silently overwritten")
	}
	if _, err = databaseReplicationConfigAfter([]byte("[mysqld]\nread_only=ON\n"), pairID, "mysql", "target", "app_data", targetID); err == nil {
		t.Fatal("unmanaged target read-only state was silently adopted")
	}
	if _, err = databaseReplicationConfigAfter(append(base, []byte(databaseReplicationConfigMarker+"\n")...), pairID, "mysql", "source", "app_data", sourceID); err == nil {
		t.Fatal("existing replication marker was accepted")
	}
}

func TestDatabaseReplicationVersionRangeIsEngineSpecific(t *testing.T) {
	for _, test := range []struct {
		engine, version string
		want            bool
	}{
		{"mysql", "8.0.25", false},
		{"mysql", "8.0.26", true},
		{"mysql", "8.4.1", true},
		{"mysql", "10.6.4-MariaDB", false},
		{"mariadb", "10.5.23-MariaDB", false},
		{"mariadb", "10.6.0-MariaDB", true},
		{"mariadb", "11.2.2-MariaDB", true},
		{"mariadb", "8.0.36", false},
	} {
		if got := databaseReplicationVersionSupported(test.engine, test.version); got != test.want {
			t.Errorf("version support for %s %s = %t, want %t", test.engine, test.version, got, test.want)
		}
	}
}

func TestDatabaseReplicationPairAndPreviewRequireProtectedFields(t *testing.T) {
	now := time.Now().UTC()
	pair := databaseReplicationRecord{
		Version: 1, PairID: strings.Repeat("c", 32), Revision: 1, State: "pending", Phase: "config_pending", Operation: "configure",
		Engine: "mysql", SourceInstanceID: "mysql-source", TargetInstanceID: "mysql-target", Database: "app_data",
		SourceInstanceRevision: 1, TargetInstanceRevision: 1, SourceDatabaseRevision: 1, TargetDatabaseRevision: 1,
		SourceServerID: 100, TargetServerID: 101, Channel: "pl_" + strings.Repeat("c", 32), Account: "plr_" + strings.Repeat("d", 16), AccountPassword: "private-generated-secret",
		CreatedAt: now, UpdatedAt: now,
	}
	if !validDatabaseReplicationPair(pair) {
		t.Fatal("complete encrypted pair journal was rejected")
	}
	mutated := pair
	mutated.SourceServerID = mutated.TargetServerID
	if validDatabaseReplicationPair(mutated) {
		t.Fatal("duplicate source and target server IDs were accepted")
	}
	mutated = pair
	mutated.Channel = "user-channel"
	if validDatabaseReplicationPair(mutated) {
		t.Fatal("unmanaged channel name was accepted")
	}
	preview := databaseReplicationPreviewRecord{
		Version: 1, PairID: strings.Repeat("c", 32), PreviewID: strings.Repeat("e", 32), SourceInstanceID: "mysql-source", TargetInstanceID: "mysql-target", Database: "app_data", Engine: "mysql",
		SourceInstanceRevision: 1, TargetInstanceRevision: 1, SourceDatabaseRevision: 1, TargetDatabaseRevision: 1,
		SourceConfigRevision: strings.Repeat("1", 64), TargetConfigRevision: strings.Repeat("2", 64), SourceServerID: 100, TargetServerID: 101,
		EstimatedSeedBytes: 4096, TableCount: 1, SchemaSHA256: strings.Repeat("a", 64), CreatedAt: now, ExpiresAt: now.Add(databaseReplicationPreviewTTL), SnapshotSHA256: strings.Repeat("f", 64),
	}
	if !validDatabaseReplicationPreview(preview) {
		t.Fatal("bounded revision-pinned replication preview was rejected")
	}
	preview.TargetServerID = preview.SourceServerID
	if validDatabaseReplicationPreview(preview) {
		t.Fatal("preview with colliding generated server IDs was accepted")
	}
}

func TestDatabaseReplicationInputIsClosedAndRequiresExplicitDestructiveChoices(t *testing.T) {
	base := DatabaseReplicationInput{Command: "configure", PairID: strings.Repeat("a", 32), PreviewID: strings.Repeat("b", 32), SnapshotSHA256: strings.Repeat("c", 64), SourceInstanceID: "mysql-source", TargetInstanceID: "mysql-target", Database: "app_data", ExpectedSourceInstanceRevision: 1, ExpectedTargetInstanceRevision: 1, ExpectedSourceDatabaseRevision: 1, ExpectedTargetDatabaseRevision: 1, ConfirmEmptyTarget: true, ConfirmReplicaHistoryLoss: true, ConfirmTargetReadOnly: true}
	encoded, _ := json.Marshal(base)
	if err := validateDatabaseReplicationInput(encoded); err != nil {
		t.Fatal("well-shaped configure choice was rejected", err)
	}
	base.ConfirmReplicaHistoryLoss = false
	encoded, _ = json.Marshal(base)
	if err := validateDatabaseReplicationInput(encoded); err == nil {
		t.Fatal("replication-history-loss confirmation was optional")
	}
	base.ConfirmReplicaHistoryLoss = true
	base.ConfirmTargetReadOnly = false
	encoded, _ = json.Marshal(base)
	if err := validateDatabaseReplicationInput(encoded); err == nil {
		t.Fatal("target read-only effect confirmation was optional")
	}
	if err := validateDatabaseReplicationInput([]byte(`{"command":"status","source_instance_id":"a","target_instance_id":"b","pair_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","database":"db","sql":"DROP DATABASE db"}`)); err == nil {
		t.Fatal("arbitrary SQL was accepted by the closed protocol")
	}
	if err := validateDatabaseReplicationInput([]byte(`{"command":"pairs_list","instance_id":"mysql-source"}`)); err != nil {
		t.Fatal("pair listing should accept one explicit managed instance", err)
	}
	if err := validateDatabaseReplicationInput([]byte(`{"command":"pairs_list","instance_id":"mysql-source","target_instance_id":"mysql-target"}`)); err == nil {
		t.Fatal("pair listing accepted a second caller-selected endpoint")
	}
	resources, err := databaseReplicationResources(context.Background(), core.Actor{}, []byte(`{"instance_id":"mysql-source"}`))
	if err != nil || len(resources) != 2 || resources[0] != InstanceResource("mysql-source") || resources[1] != DatabaseResource("mysql-source", "") {
		t.Fatal("pair list resource scope was not pinned to its requested managed instance", resources, err)
	}
}

func TestDatabaseReplicationPairListHidesPairsWithoutBothInstanceAndDatabaseGrants(t *testing.T) {
	actor := core.Actor{ID: "limited", Scopes: []string{"databases:replication"}}
	pairs := DatabaseReplicationPairsList{Items: []DatabaseReplicationPairSummary{
		{PairID: strings.Repeat("a", 32), PairRevision: 1, State: "running", Engine: "mysql", SourceInstanceID: "mysql-source", TargetInstanceID: "mysql-target", Database: "app_data"},
	}}
	encoded, _ := json.Marshal(pairs)
	var output any
	_ = json.Unmarshal(encoded, &output)
	grants := map[string]bool{
		InstanceResource("mysql-source"):             true,
		InstanceResource("mysql-target"):             true,
		DatabaseResource("mysql-source", "app_data"): true,
		DatabaseResource("mysql-target", "app_data"): true,
	}
	filtered, err := filterDatabaseReplicationPairs(actor, "mysql-source", output, func(a core.Actor, scope, resource string) bool {
		if scope != "databases:replication" || a.ID != actor.ID {
			return false
		}
		return grants[resource]
	})
	if err != nil || filtered.Total != 1 || len(filtered.Items) != 1 || filtered.Items[0].TargetInstanceID != "mysql-target" {
		t.Fatal("fully authorized pair was not returned", filtered, err)
	}
	filtered, err = filterDatabaseReplicationPairs(actor, "mysql-source", output, func(a core.Actor, scope, resource string) bool {
		return scope == "databases:replication" && resource != DatabaseResource("mysql-target", "app_data")
	})
	if err != nil || filtered.Total != 0 || len(filtered.Items) != 0 {
		t.Fatal("pair list exposed a target whose exact database grant was missing", filtered, err)
	}
}

func TestDatabaseReplicationIndexedGuardAndRecoveryScope(t *testing.T) {
	e := testAgent(t)
	now := time.Now().UTC()
	for _, id := range []string{"mysql-source", "mysql-target", "mysql-other-target"} {
		if err := e.save("receipts", id, Instance{ID: id, PluginID: "mysql", InstanceID: strings.TrimPrefix(id, "mysql-")}); err != nil {
			t.Fatal("save managed instance receipt", id, err)
		}
	}
	newRecord := func(pairLetter, target string) databaseReplicationRecord {
		pairID := strings.Repeat(pairLetter, 32)
		return databaseReplicationRecord{
			Version: 1, PairID: pairID, Revision: 1, State: "pending", Phase: "config_pending", Operation: "configure",
			Engine: "mysql", SourceInstanceID: "mysql-source", TargetInstanceID: target, Database: "app_data",
			SourceInstanceRevision: 1, TargetInstanceRevision: 1, SourceDatabaseRevision: 1, TargetDatabaseRevision: 1,
			SourceServerID: 100, TargetServerID: 101, Channel: "pl_" + pairID, Account: "plr_" + pairLetter + strings.Repeat("d", 15), AccountPassword: "private-generated-secret",
			CreatedAt: now, UpdatedAt: now,
		}
	}
	first := newRecord("c", "mysql-target")
	if !validDatabaseReplicationPair(first) {
		t.Fatal("test pair record must satisfy the protected schema")
	}
	if err := e.save("private", databaseReplicationPairKey(first.PairID), first); err != nil {
		t.Fatal("persist pair before indexing", err)
	}
	if err := e.indexDatabaseReplicationPair(first); err != nil {
		t.Fatal("index pair for both endpoints", err)
	}
	if err := e.ensureDatabaseReplicationJournalFinalized(context.Background(), first.SourceInstanceID); core.ErrorCode(err) != "operation_recovery_required" {
		t.Fatal("unfinished indexed intent did not block ordinary mutation", err)
	}
	markerContext, err := databaseReplicationRecoveryContextFor(context.Background(), e, first)
	if err != nil {
		t.Fatal("create exact recovery marker", err)
	}
	if err = e.ensureDatabaseReplicationJournalFinalized(markerContext, first.SourceInstanceID); err != nil {
		t.Fatal("validated recovery marker should bypass only its own pending record", err)
	}
	first.Revision++
	first.UpdatedAt = now.Add(time.Second)
	if err = e.save("private", databaseReplicationPairKey(first.PairID), first); err != nil {
		t.Fatal("advance protected pair revision", err)
	}
	if err = e.ensureDatabaseReplicationJournalFinalized(markerContext, first.SourceInstanceID); core.ErrorCode(err) != "operation_recovery_required" {
		t.Fatal("stale recovery marker authorized a newer pair revision", err)
	}
	markerContext, err = databaseReplicationRecoveryContextFor(context.Background(), e, first)
	if err != nil {
		t.Fatal("create refreshed recovery marker", err)
	}
	second := newRecord("e", "mysql-other-target")
	if err = e.save("private", databaseReplicationPairKey(second.PairID), second); err != nil {
		t.Fatal("persist second indexed test record", err)
	}
	if err = e.indexDatabaseReplicationPair(second); err != nil {
		t.Fatal("index second test record", err)
	}
	if err = e.ensureDatabaseReplicationJournalFinalized(markerContext, first.SourceInstanceID); core.ErrorCode(err) != "operation_recovery_required" {
		t.Fatal("recovery marker ignored another pending pair attached to the same instance", err)
	}
	if err = e.ensureDatabaseReplicationDetached(context.Background(), first.SourceInstanceID); core.ErrorCode(err) != "replication_attached" {
		t.Fatal("active pair did not protect its endpoint from invalidating mutations", err)
	}
	first.State, first.Phase, first.UpdatedAt = "stopped", "settled", now.Add(2*time.Second)
	if err = e.save("private", databaseReplicationPairKey(first.PairID), first); err != nil {
		t.Fatal("settle first pair", err)
	}
	if err = e.ensureDatabaseReplicationJournalFinalized(context.Background(), first.SourceInstanceID); core.ErrorCode(err) != "operation_recovery_required" {
		t.Fatal("settling one pair incorrectly cleared another unfinished intent", err)
	}
	second.State, second.Phase, second.UpdatedAt = "removed", "settled", now.Add(3*time.Second)
	if err = e.save("private", databaseReplicationPairKey(second.PairID), second); err != nil {
		t.Fatal("settle second pair", err)
	}
	if err = e.ensureDatabaseReplicationJournalFinalized(context.Background(), first.SourceInstanceID); err != nil {
		t.Fatal("settled pair metadata should not block ordinary database traffic", err)
	}
	if err = e.ensureDatabaseReplicationDetached(context.Background(), first.SourceInstanceID); core.ErrorCode(err) != "replication_attached" {
		t.Fatal("stopped owned pair must remain attached until explicit removal", err)
	}
	first.State, first.UpdatedAt = "removed", now.Add(4*time.Second)
	if err = e.save("private", databaseReplicationPairKey(first.PairID), first); err != nil {
		t.Fatal("remove first pair", err)
	}
	if err = e.ensureDatabaseReplicationDetached(context.Background(), first.SourceInstanceID); err != nil {
		t.Fatal("explicitly removed pair should no longer own its endpoints", err)
	}
}

func TestDatabaseReplicationGuardFailsClosedForOrphanedIndex(t *testing.T) {
	e := testAgent(t)
	if err := e.save("receipts", "mysql-orphan", Instance{ID: "mysql-orphan", PluginID: "mysql", InstanceID: "orphan"}); err != nil {
		t.Fatal("save managed receipt", err)
	}
	index := databaseReplicationInstanceIndex{Version: 1, InstanceID: "mysql-orphan", PairIDs: []string{strings.Repeat("f", 32)}}
	if err := e.saveDatabaseReplicationIndex(index); err != nil {
		t.Fatal("save orphaned protected index", err)
	}
	if err := e.ensureDatabaseReplicationJournalFinalized(context.Background(), "mysql-orphan"); core.ErrorCode(err) != "ownership_invalid" {
		t.Fatal("orphaned protected index did not fail closed", err)
	}
}

func TestDatabaseReplicationIndexSaveBoundariesRemainBeforeExternalWork(t *testing.T) {
	now := time.Now().UTC()
	for _, failAt := range []string{"record", "source_index", "target_index"} {
		t.Run(failAt, func(t *testing.T) {
			e := testAgent(t)
			record := replicationTestRecord(now, "a", "mysql-source", "mysql-target")
			privateDir := filepath.Join(e.config.StateDir, "private")
			if failAt == "record" {
				if err := os.Mkdir(filepath.Join(privateDir, databaseReplicationPairKey(record.PairID)+".json"), 0700); err != nil {
					t.Fatal("create deterministic record-save failure", err)
				}
				defer os.RemoveAll(filepath.Join(privateDir, databaseReplicationPairKey(record.PairID)+".json"))
				if err := e.save("private", databaseReplicationPairKey(record.PairID), record); err == nil {
					t.Fatal("blocked protected record save unexpectedly succeeded")
				}
				if len(record.SourceConfigAfter) != 0 || record.SeedFilename != "" || record.AccountPassword == "" {
					t.Fatal("record-save fault crossed into config, seed, or SQL work")
				}
				return
			}
			if err := e.save("private", databaseReplicationPairKey(record.PairID), record); err != nil {
				t.Fatal("persist exact pair record first", err)
			}
			blockedID := record.TargetInstanceID
			if failAt == "source_index" {
				blockedID = record.SourceInstanceID
			}
			blockedPath := filepath.Join(privateDir, databaseReplicationIndexKey(blockedID)+".json")
			if err := os.Mkdir(blockedPath, 0700); err != nil {
				t.Fatal("create deterministic index-save failure", err)
			}
			if err := e.indexDatabaseReplicationPair(record); err == nil {
				t.Fatal("blocked endpoint index save unexpectedly succeeded")
			}
			stored, err := e.loadDatabaseReplication(record.PairID)
			if err != nil || stored == nil || stored.Phase != "config_pending" || stored.State != "pending" || stored.SeedFilename != "" {
				t.Fatal("index-save fault must leave a durable untouched pair intent", stored, err)
			}
			if stored.SourceConfigSHA256 != "" || stored.TargetConfigSHA256 != "" {
				t.Fatal("index-save fault crossed into config mutation")
			}
			_ = os.RemoveAll(blockedPath)
			if err = e.indexDatabaseReplicationPair(record); err != nil {
				t.Fatal("explicit index repair could not finish the exact saved pair", err)
			}
			for _, endpoint := range []string{record.SourceInstanceID, record.TargetInstanceID} {
				index, err := e.loadDatabaseReplicationIndex(endpoint)
				if err != nil || len(index.PairIDs) != 1 || index.PairIDs[0] != record.PairID {
					t.Fatal("repaired index does not reference exact pair", endpoint, index, err)
				}
			}
		})
	}
}

func TestDatabaseReplicationDatabaseDetachGuardIsSchemaScoped(t *testing.T) {
	e := testAgent(t)
	if err := e.save("receipts", "mysql-source", Instance{ID: "mysql-source", PluginID: "mysql", InstanceID: "source"}); err != nil {
		t.Fatal("save source receipt", err)
	}
	record := replicationTestRecord(time.Now().UTC(), "b", "mysql-source", "mysql-target")
	record.State, record.Phase = "stopped", "settled"
	if err := e.save("private", databaseReplicationPairKey(record.PairID), record); err != nil {
		t.Fatal("save settled pair", err)
	}
	if err := e.indexDatabaseReplicationPair(record); err != nil {
		t.Fatal("index settled pair", err)
	}
	if err := e.ensureDatabaseReplicationDatabaseDetached(context.Background(), record.SourceInstanceID, "unrelated_db"); err != nil {
		t.Fatal("unrelated managed schema should remain mutable", err)
	}
	if err := e.ensureDatabaseReplicationDatabaseDetached(context.Background(), record.SourceInstanceID, record.Database); core.ErrorCode(err) != "replication_attached" {
		t.Fatal("the exact paired schema was not protected from deletion", err)
	}
}

func TestDatabaseReplicationReadOnlyTargetGuardsOnlyOwnedReplicaSchema(t *testing.T) {
	e := testAgent(t)
	for _, id := range []string{"mysql-source", "mysql-target"} {
		instance := Instance{ID: id, PluginID: "mysql", InstanceID: id + "-runtime"}
		if err := e.save("receipts", id, instance); err != nil {
			t.Fatal("save managed endpoint receipt", err)
		}
	}
	record := replicationTestRecord(time.Now().UTC(), "d", "mysql-source", "mysql-target")
	record.State, record.Phase = "running", "settled"
	if err := e.save("private", databaseReplicationPairKey(record.PairID), record); err != nil {
		t.Fatal("save settled pair", err)
	}
	if err := e.indexDatabaseReplicationPair(record); err != nil {
		t.Fatal("index settled pair", err)
	}
	if err := e.ensureDatabaseReplicationTargetWritable(context.Background(), record.SourceInstanceID, record.Database); err != nil {
		t.Fatal("ordinary source writes must remain allowed", err)
	}
	if err := e.ensureDatabaseReplicationTargetWritable(context.Background(), record.TargetInstanceID, record.Database); core.ErrorCode(err) != "replication_target_read_only" {
		t.Fatal("writes to the exact target schema were not blocked", err)
	}
	if err := e.ensureDatabaseReplicationTargetSchemaCreateAllowed(context.Background(), record.SourceInstanceID); err != nil {
		t.Fatal("source database creation must remain allowed", err)
	}
	if err := e.ensureDatabaseReplicationTargetSchemaCreateAllowed(context.Background(), record.TargetInstanceID); core.ErrorCode(err) != "replication_target_not_dedicated" {
		t.Fatal("a new target schema was not blocked", err)
	}

	// Only the exact authenticated configure context for the seed-import phase
	// can pass the target write guard during initial data seeding.
	record.State, record.Phase, record.Operation = "seeding", "seed_import_pending", "configure"
	record.Revision++
	if err := e.save("private", databaseReplicationPairKey(record.PairID), record); err != nil {
		t.Fatal("save seed-import intent", err)
	}
	seedContext, err := databaseReplicationRecoveryContextFor(context.Background(), e, record)
	if err != nil {
		t.Fatal("create exact internal seed marker", err)
	}
	if err = e.ensureDatabaseReplicationTargetWritable(seedContext, record.TargetInstanceID, record.Database); err != nil {
		t.Fatal("exact seed-import marker should permit the internal import", err)
	}
	if err = e.ensureDatabaseReplicationTargetWritable(context.Background(), record.TargetInstanceID, record.Database); core.ErrorCode(err) != "replication_target_read_only" {
		t.Fatal("user request without the internal marker passed the target write guard", err)
	}
}

func TestDatabaseReplicationLongLivedEndpointIdentityIgnoresServiceRevision(t *testing.T) {
	record := replicationTestRecord(time.Now().UTC(), "e", "mysql-source", "mysql-target")
	makeEndpoint := func(instanceID, runtimeID, dataDir, configDir string, revision int64) Instance {
		id := instanceID
		return Instance{
			ID: id, PluginID: "mysql", InstanceID: runtimeID, Revision: revision, Ownership: "managed",
			Paths:   Paths{DataDir: dataDir, ConfigDir: configDir},
			Service: ServiceProfile{ID: id, PluginID: "mysql", InstanceID: runtimeID, Unit: "powerlinux-plugin-" + id + ".service", ConfigPath: configDir, DataPath: dataDir, Ownership: "managed", Program: "/usr/sbin/mysqld"},
		}
	}
	source := makeEndpoint(record.SourceInstanceID, "source-runtime", record.SourceDataDir, record.SourceConfigDir, 42)
	target := makeEndpoint(record.TargetInstanceID, "target-runtime", record.TargetDataDir, record.TargetConfigDir, 73)
	if !databaseReplicationEndpointIdentityMatches(source, record, true) || !databaseReplicationEndpointIdentityMatches(target, record, false) {
		t.Fatal("ordinary service/config revision changes must not invalidate an otherwise exact managed endpoint")
	}
	target.Paths.DataDir = "/unowned/data"
	if databaseReplicationEndpointIdentityMatches(target, record, false) {
		t.Fatal("a changed managed data path must invalidate long-lived endpoint ownership")
	}
}

func replicationTestRecord(now time.Time, pairLetter, source, target string) databaseReplicationRecord {
	pairID := strings.Repeat(pairLetter, 32)
	return databaseReplicationRecord{
		Version: 1, PairID: pairID, Revision: 1, State: "pending", Phase: "config_pending", Operation: "configure",
		Engine: "mysql", SourceInstanceID: source, TargetInstanceID: target, Database: "app_data",
		SourceInstanceRevision: 1, TargetInstanceRevision: 1, SourceDatabaseRevision: 1, TargetDatabaseRevision: 1,
		SourceServerID: 100, TargetServerID: 101, Channel: "pl_" + pairID, Account: "plr_" + strings.Repeat(pairLetter, 16), AccountPassword: "private-generated-secret",
		CreatedAt: now, UpdatedAt: now,
	}
}

func fmtUint(value uint32) string {
	return strconv.FormatUint(uint64(value), 10)
}
