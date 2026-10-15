package plugins

import (
	"reflect"
	"testing"
)

func TestDatabaseReplicationRecoveryChoicesAreBoundToOperationAndPhase(t *testing.T) {
	tests := []struct {
		name   string
		record databaseReplicationRecord
		want   []string
	}{
		{"configure at first boundary", databaseReplicationRecord{Operation: "configure", State: "recovery_required", Phase: "config_pending", Engine: "mysql"}, []string{"apply_pending", "restore_previous"}},
		{"configure before seed", databaseReplicationRecord{Operation: "configure", State: "recovery_required", Phase: "target_configured", Engine: "mariadb"}, []string{"apply_pending", "restore_previous"}},
		{"configure after seed boundary", databaseReplicationRecord{Operation: "configure", State: "recovery_required", Phase: "seed_dump_pending", Engine: "mysql"}, []string{"apply_pending"}},
		{"mysql filter apply only", databaseReplicationRecord{Operation: "configure", State: "recovery_required", Phase: "filter_config_pending", Engine: "mysql"}, []string{"apply_pending"}},
		{"MariaDB cannot claim mysql filter phase", databaseReplicationRecord{Operation: "configure", State: "recovery_required", Phase: "filter_config_pending", Engine: "mariadb"}, []string{}},
		{"remove restore overlap never rolls back", databaseReplicationRecord{Operation: "remove", State: "recovery_required", Phase: "restore_target_config_pending", Engine: "mysql"}, []string{"apply_pending"}},
		{"remove later config restore never rolls back", databaseReplicationRecord{Operation: "remove", State: "recovery_required", Phase: "restore_source_config_pending", Engine: "mysql"}, []string{"apply_pending"}},
		{"recover restore journal resumes restore", databaseReplicationRecord{Operation: "recover", State: "recovery_required", Phase: "restore_target_config_pending", Engine: "mysql"}, []string{"restore_previous"}},
		{"start exact boundary", databaseReplicationRecord{Operation: "start", State: "recovery_required", Phase: "channel_start_pending", Engine: "mysql"}, []string{"apply_pending"}},
		{"stop exact boundary", databaseReplicationRecord{Operation: "stop", State: "recovery_required", Phase: "channel_stop_pending", Engine: "mysql"}, []string{"apply_pending"}},
		{"settled healthy pair has no recovery", databaseReplicationRecord{Operation: "configure", State: "running", Phase: "settled", Engine: "mysql"}, []string{}},
		{"removed pair has no recovery", databaseReplicationRecord{Operation: "remove", State: "removed", Phase: "settled", Engine: "mysql"}, []string{}},
		{"unknown operation fails closed", databaseReplicationRecord{Operation: "unknown", State: "recovery_required", Phase: "config_pending", Engine: "mysql"}, []string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := databaseReplicationRecoveryChoices(test.record)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("recovery choices mismatch: got %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestDatabaseReplicationStatusCarriesProtectedRecoveryChoices(t *testing.T) {
	record := databaseReplicationRecord{PairID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Revision: 4, State: "recovery_required", Phase: "source_configured", Operation: "configure", Engine: "mysql"}
	status := replicationStatusFromRecord(record)
	if status.Operation != "configure" || !reflect.DeepEqual(status.AllowedRecoveryChoices, []string{"apply_pending", "restore_previous"}) {
		t.Fatalf("status did not expose choices derived from its protected journal: %#v", status)
	}
	record.Operation = "remove"
	record.Phase = "restore_target_config_pending"
	status = replicationStatusFromRecord(record)
	if status.Operation != "remove" || !reflect.DeepEqual(status.AllowedRecoveryChoices, []string{"apply_pending"}) {
		t.Fatalf("overlapping remove phase incorrectly exposed restore_previous: %#v", status)
	}
}
