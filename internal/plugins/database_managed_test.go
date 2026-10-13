package plugins

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestManagedDatabaseCreateTombstoneUsesMonotonicCompareAndSwap(t *testing.T) {
	e := testAgent(t)
	instance := Instance{ID: "postgresql-cas", PluginID: "postgresql"}
	input := ManagedDatabaseInput{InstanceID: instance.ID, Database: "qa_cas", Permission: "read_write"}

	prior, err := e.loadManagedDatabaseCreationTombstone(instance, input)
	if err != nil || prior != nil {
		t.Fatal("fresh database name did not begin without a tombstone")
	}
	first := managedDatabaseRecord{ManagedDatabase: ManagedDatabase{InstanceID: instance.ID, Database: input.Database, Username: "pl_qa_cas", Engine: instance.PluginID, Permission: input.Permission, State: "creating", Revision: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}, Credential: "first-generation-secret"}
	if err = e.claimManagedDatabaseCreation(instance, input, nil, first); err != nil {
		t.Fatal("initial zero-to-one creation claim failed", core.ErrorCode(err))
	}
	if _, err = e.loadManagedDatabaseCreationTombstone(instance, input); core.ErrorCode(err) != "operation_recovery_required" {
		t.Fatal("unfinished creation was not protected by its receipt state")
	}

	deleted := first
	deleted.State = "deleted"
	deleted.Revision = 2
	deleted.Credential = ""
	if err = e.save("private", managedDatabaseKey(instance.ID, input.Database), deleted); err != nil {
		t.Fatal(err)
	}
	input.ExpectedRevision = 1
	if _, err = e.loadManagedDatabaseCreationTombstone(instance, input); core.ErrorStatus(err) != 412 || core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("stale tombstone revision was accepted")
	}
	input.ExpectedRevision = 2
	prior, err = e.loadManagedDatabaseCreationTombstone(instance, input)
	if err != nil || prior == nil || prior.State != "deleted" || prior.Revision != 2 {
		t.Fatal("current deletion tombstone could not be consumed")
	}
	next := managedDatabaseRecord{ManagedDatabase: ManagedDatabase{InstanceID: instance.ID, Database: input.Database, Username: prior.Username, Engine: instance.PluginID, Permission: input.Permission, State: "creating", Revision: 3, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}, Credential: "second-generation-secret"}
	if err = e.claimManagedDatabaseCreation(instance, input, prior, next); err != nil {
		t.Fatal("next monotonic creation claim failed", core.ErrorCode(err))
	}
	if err = e.claimManagedDatabaseCreation(instance, input, prior, next); core.ErrorStatus(err) != 412 || core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("same tombstone revision was consumed twice")
	}
	if err = e.commitManagedDatabaseCreation(instance, next); err != nil {
		t.Fatal("matching creation receipt could not commit")
	}
	var current managedDatabaseRecord
	if err = e.load("private", managedDatabaseKey(instance.ID, input.Database), &current); err != nil || current.State != "active" || current.Revision != 3 || current.Credential != next.Credential {
		t.Fatal("creation commit did not preserve the incremented revision and fresh credential")
	}
	if _, err = e.loadManagedDatabaseCreationTombstone(instance, ManagedDatabaseInput{InstanceID: instance.ID, Database: input.Database, ExpectedRevision: 3}); core.ErrorCode(err) != "database_exists" {
		t.Fatal("active managed database was allowed to consume a tombstone")
	}
}

func TestMongoTombstoneRecreationIsExplicitlyUnsupported(t *testing.T) {
	e := testAgent(t)
	instance := Instance{ID: "mongodb-cas", PluginID: "mongodb"}
	input := ManagedDatabaseInput{InstanceID: instance.ID, Database: "qa_mongo_cas", Permission: "read_write", ExpectedRevision: 3}
	tombstone := managedDatabaseRecord{ManagedDatabase: ManagedDatabase{InstanceID: instance.ID, Database: input.Database, Username: "pl_qa_mongo_cas", Engine: instance.PluginID, Permission: input.Permission, State: "deleted", Revision: 3}}
	key := managedDatabaseKey(instance.ID, input.Database)
	if err := e.save("private", key, tombstone); err != nil {
		t.Fatal(err)
	}
	if _, err := e.createManagedDatabase(context.Background(), instance, input); core.ErrorStatus(err) != 409 || core.ErrorCode(err) != "database_recreation_unsupported" {
		t.Fatal("MongoDB tombstone recreation did not fail with the ownership-fence limitation", core.ErrorCode(err))
	}
	var current managedDatabaseRecord
	if err := e.load("private", key, &current); err != nil || current.State != "deleted" || current.Revision != 3 || current.Credential != "" {
		t.Fatal("unsupported MongoDB recreation changed its deletion tombstone")
	}
	fresh := input
	fresh.Database = "qa_mongo_fresh"
	fresh.ExpectedRevision = 0
	if prior, err := e.loadManagedDatabaseCreationTombstone(instance, fresh); err != nil || prior != nil {
		t.Fatal("MongoDB first-create path was unexpectedly disabled")
	}
}

func TestManagedDatabaseReceiptMissingUsesTypedNotFound(t *testing.T) {
	e := testAgent(t)
	instance := Instance{
		ID:         "mariadb-receipt-fixture",
		PluginID:   "mariadb",
		InstanceID: "receipt-fixture",
		Paths:      e.instancePaths("mariadb", "receipt-fixture"),
		Service:    ServiceProfile{ID: "mariadb-receipt-fixture", Unit: "powerlinux-plugin-mariadb-receipt-fixture.service"},
	}
	if err := e.save("receipts", instance.ID, instance); err != nil {
		t.Fatal("valid database instance receipt could not be saved")
	}

	input := ManagedDatabaseInput{InstanceID: instance.ID, Database: "qa_new_receipt_name", Command: "receipt"}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.database(context.Background(), raw); core.ErrorStatus(err) != 404 || core.ErrorCode(err) != "database_receipt_not_found" || err.Error() != "database_receipt_not_found: Managed database receipt does not exist for this instance and database" {
		t.Fatal("missing managed database receipt did not return its stable typed 404", core.ErrorCode(err), core.ErrorStatus(err))
	}

	input.InstanceID = "mariadb-missing-instance"
	raw, err = json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.database(context.Background(), raw); core.ErrorStatus(err) != 404 || core.ErrorCode(err) != "not_found" {
		t.Fatal("missing software instance was conflated with a missing database receipt", core.ErrorCode(err), core.ErrorStatus(err))
	}
}
