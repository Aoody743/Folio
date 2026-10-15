package plugins

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
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
	if err != nil || !strings.HasSuffix(string(target), databaseReplicationConfigMarker+"\n# pair="+pairID+" role=target\n[mysqld]\nserver-id="+fmtUint(targetID)+"\n") {
		t.Fatal("MySQL target config must carry its generated server ID without global filtering", err, string(target))
	}
	maria, err := databaseReplicationConfigAfter(base, pairID, "mariadb", "target", "owned_db", targetID)
	if err != nil || !strings.HasSuffix(string(maria), databaseReplicationConfigMarker+"\n# pair="+pairID+" role=target\n[mysqld]\nserver-id="+fmtUint(targetID)+"\nreplicate-wild-do-table=owned\\\\_db.%\n") {
		t.Fatal("MariaDB target must carry the exact managed database table filter", err, string(maria))
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
		EstimatedSeedBytes: 4096, TableCount: 1, CreatedAt: now, ExpiresAt: now.Add(databaseReplicationPreviewTTL), SnapshotSHA256: strings.Repeat("f", 64),
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
	base := DatabaseReplicationInput{Command: "configure", PairID: strings.Repeat("a", 32), PreviewID: strings.Repeat("b", 32), SnapshotSHA256: strings.Repeat("c", 64), SourceInstanceID: "mysql-source", TargetInstanceID: "mysql-target", Database: "app_data", ExpectedSourceInstanceRevision: 1, ExpectedTargetInstanceRevision: 1, ExpectedSourceDatabaseRevision: 1, ExpectedTargetDatabaseRevision: 1, ConfirmEmptyTarget: true, ConfirmReplicaHistoryLoss: true}
	encoded, _ := json.Marshal(base)
	if err := validateDatabaseReplicationInput(encoded); err != nil {
		t.Fatal("well-shaped configure choice was rejected", err)
	}
	base.ConfirmReplicaHistoryLoss = false
	encoded, _ = json.Marshal(base)
	if err := validateDatabaseReplicationInput(encoded); err == nil {
		t.Fatal("replication-history-loss confirmation was optional")
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

func fmtUint(value uint32) string {
	return strconv.FormatUint(uint64(value), 10)
}
