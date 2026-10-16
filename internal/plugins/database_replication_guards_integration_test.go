package plugins

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestDatabaseReplicationPublicPreviewSchemaCarriesAgentRevisionPins(t *testing.T) {
	for _, binding := range (&Manager{}).databaseReplicationBindings() {
		if binding.Action.Name != "managed_database_replication_preview" {
			continue
		}
		properties := binding.Action.InputSchema["properties"].(map[string]any)
		required := binding.Action.InputSchema["required"].([]string)
		input := map[string]any{"command": "preview", "source_instance_id": "mysql-source", "target_instance_id": "mysql-target", "database": "app_data"}
		for _, key := range []string{"expected_source_instance_revision", "expected_target_instance_revision", "expected_source_database_revision", "expected_target_database_revision"} {
			if _, declared := properties[key]; !declared || !slices.Contains(required, key) {
				t.Fatalf("public preview cannot supply mandatory Agent revision pin %s", key)
			}
			input[key] = 1
		}
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		if err = validateDatabaseReplicationInput(raw); err != nil {
			t.Fatal("publicly declared preview input cannot reach the Agent", err)
		}
		return
	}
	t.Fatal("replication preview action is missing")
}

// Exercise the public Agent dispatch path, rather than just the guard helpers:
// a stopped pair still protects its replica and both owned credentials. There
// is deliberately no SQL server or backup file in this fixture, so a guard
// accidentally removed from dispatch cannot pass by reaching a real mutation.
func TestDatabaseReplicationAttachedDispatchRejectsDestructiveDatabaseActions(t *testing.T) {
	e := testAgent(t)
	pair := replicationTestRecord(time.Now().UTC(), "d", "mysql-source", "mysql-target")
	pair.State, pair.Phase = "stopped", "settled"
	for _, label := range []string{"source", "target"} {
		i := Instance{ID: "mysql-" + label, PluginID: "mysql", InstanceID: label, Ownership: "managed", Status: "running", Revision: 1}
		i.Paths = e.instancePaths(i.PluginID, i.InstanceID)
		i.Service.ID, i.Service.Unit = i.ID, "powerlinux-plugin-"+i.ID+".service"
		if err := e.save("receipts", i.ID, i); err != nil {
			t.Fatal(err)
		}
		db := managedDatabaseRecord{ManagedDatabase: ManagedDatabase{InstanceID: i.ID, Database: pair.Database, Username: "owned_user", Engine: "mysql", Permission: "read_write", State: "active", Revision: 1}}
		if err := e.save("private", managedDatabaseKey(i.ID, pair.Database), db); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.save("private", databaseReplicationPairKey(pair.PairID), pair); err != nil {
		t.Fatal(err)
	}
	if err := e.indexDatabaseReplicationPair(pair); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, instance, command, want string
	}{
		{"target_restore", pair.TargetInstanceID, "restore", "replication_target_read_only"},
		{"target_create", pair.TargetInstanceID, "create", "replication_target_not_dedicated"},
		{"source_delete", pair.SourceInstanceID, "delete", "replication_attached"},
		{"target_delete", pair.TargetInstanceID, "delete", "replication_attached"},
		{"source_password", pair.SourceInstanceID, "set_password", "replication_attached"},
		{"target_permission", pair.TargetInstanceID, "set_permission", "replication_attached"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := ManagedDatabaseInput{InstanceID: tc.instance, Database: pair.Database, Command: tc.command, ExpectedRevision: 1, Permission: "read", Password: "fixture-password-123", BackupID: "missing-backup"}
			raw, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = e.database(context.Background(), raw); core.ErrorCode(err) != tc.want {
				t.Fatalf("action reached past replication protection: want %s, got %v", tc.want, err)
			}
		})
	}
	// Reads stay available so operators can inspect and recover the pair.
	raw, _ := json.Marshal(ManagedDatabaseInput{InstanceID: pair.TargetInstanceID, Database: pair.Database, Command: "receipt"})
	if _, err := e.database(context.Background(), raw); err != nil {
		t.Fatal("attached target receipt must remain readable", err)
	}
}
