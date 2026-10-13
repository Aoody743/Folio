//go:build linux

package plugins

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// These tests exercise the production typed Agent and shared action/task path
// against dedicated databases on authenticated external fixtures. They do not
// count as daemon installation/lifecycle acceptance.
func liveManagedFixture(t *testing.T, engine string) (*core.Service, core.Actor, *Manager, *PrivilegedExecutor, Instance, DatabaseRegisterInput) {
	t.Helper()
	s, a, m, fixtures := liveDriverFixture(t)
	var in DatabaseRegisterInput
	if len(fixtures[engine+"_admin"]) == 0 {
		t.Skip("administrative isolated fixture not configured")
	}
	if json.Unmarshal(fixtures[engine+"_admin"], &in) != nil {
		t.Fatal("invalid isolated administrative fixture metadata")
	}
	e := shortPathDatabaseFixtureAgent(t)
	id := engine + "-managed-fixture"
	instance := Instance{ID: id, PluginID: engine, InstanceID: "managed-fixture", Status: "external-fixture", Paths: e.instancePaths(engine, "managed-fixture"), Revision: 1, Ownership: "fixture", Config: map[string]any{"bind": in.Host, "port": in.Port}, Service: ServiceProfile{ID: id, Unit: "powerlinux-plugin-" + id + ".service"}}
	for _, dir := range []string{instance.Paths.DataDir, instance.Paths.RunDir, instance.Paths.BackupDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if engine == "mariadb" || engine == "mysql" {
		installManagedMySQLSocketFixture(t, instance, in)
	}
	if err := e.save("receipts", id, instance); err != nil {
		t.Fatal(err)
	}
	credential := managedDatabaseCredential{Engine: engine, Username: in.Username, Password: in.Password, DataDir: instance.Paths.DataDir, Initialized: true}
	if err := e.save("private", databasePrivateID(instance), credential); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(instance.Paths.RunDir, "database-agent.sock")
	server, err := agent.NewUnixServer(socket, []uint32{uint32(os.Getuid())}, e)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(func() { server.Close() })
	go server.Serve(ctx)
	client, err := agent.NewUnixClient(socket, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	m.executor = client
	if err = s.RegisterExecutor([]string{"database_action"}, client); err != nil {
		t.Fatal(err)
	}
	return s, a, m, e, instance, in
}
func liveManagedRecord(t *testing.T, e *PrivilegedExecutor, instance Instance, database string) managedDatabaseRecord {
	t.Helper()
	var record managedDatabaseRecord
	if err := e.load("private", managedDatabaseKey(instance.ID, database), &record); err != nil {
		t.Fatal("managed ownership receipt unavailable")
	}
	return record
}
func liveManagedJob(t *testing.T, s *core.Service, a core.Actor, name string, input any) json.RawMessage {
	t.Helper()
	submitted := liveAction(t, s, a, name, input).(map[string]any)
	id, _ := submitted["id"].(string)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		data := liveAction(t, s, a, "tasks_get", map[string]any{"id": id}).(map[string]any)
		if data["state"] == "succeeded" {
			result := data["result"].(*agent.Result)
			if len(result.Data) > 0 {
				return result.Data
			}
			return json.RawMessage(result.Stdout)
		}
		if data["state"] == "failed" || data["state"] == "canceled" {
			t.Fatalf("%s real task did not succeed: %s", name, data["error"])
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s real task did not finish", name)
	return nil
}
func uniqueManagedName() string { return "qa_pl_" + fmt.Sprintf("%x", time.Now().UnixNano()) }

func liveManagedSQLAdmin(t *testing.T, engine string, in DatabaseRegisterInput, database string) *sql.DB {
	t.Helper()
	db, err := openSQLProfile(context.Background(), DatabaseConnection{Engine: engine, Host: in.Host, Port: in.Port, Username: in.Username, Access: "instance"}, in.Password, database)
	if err != nil {
		t.Fatal("isolated SQL administrator authentication failed")
	}
	return db
}

func liveManagedSQLRoleExists(ctx context.Context, admin *sql.DB, engine, username string) (bool, error) {
	var count int
	var err error
	if engine == "postgresql" {
		err = admin.QueryRowContext(ctx, "SELECT count(*) FROM pg_roles WHERE rolname=$1", username).Scan(&count)
	} else {
		err = admin.QueryRowContext(ctx, "SELECT count(*) FROM mysql.user WHERE User=?", username).Scan(&count)
	}
	return count > 0, err
}

func TestLiveManagedDeletedDatabaseRecreationCASAndExternalOwnership(t *testing.T) {
	for _, engine := range []string{"mariadb", "postgresql"} {
		t.Run(engine, func(t *testing.T) {
			s, a, _, e, instance, fixture := liveManagedFixture(t, engine)
			ctx := context.Background()
			name := uniqueManagedName()
			admin := liveManagedSQLAdmin(t, engine, fixture, fixture.Database)
			defer admin.Close()
			t.Cleanup(func() {
				admin := liveManagedSQLAdmin(t, engine, fixture, fixture.Database)
				defer admin.Close()
				_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+quoteDatabaseIdentifier(engine, name))
				username := "pl_" + hex.EncodeToString(sha256Digest(instance.ID + "|" + name)[:10])
				if engine == "postgresql" {
					_, _ = admin.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+quoteDatabaseIdentifier(engine, username))
				} else {
					_, _ = admin.ExecContext(context.Background(), "DROP USER IF EXISTS "+quoteDatabaseIdentifier(engine, username)+"@'127.0.0.1'")
				}
			})

			liveAction(t, s, a, "managed_database_create", map[string]any{"instance_id": instance.ID, "database": name, "permission": "read_write"})
			created := liveManagedRecord(t, e, instance, name)
			if created.Revision != 1 {
				t.Fatal("first database receipt did not start at revision one")
			}
			oldCredential := created.Credential
			liveManagedJob(t, s, a, "managed_database_delete", map[string]any{"instance_id": instance.ID, "database": name, "expected_revision": created.Revision})
			tombstone := liveManagedRecord(t, e, instance, name)
			if tombstone.State != "deleted" || tombstone.Revision != 2 || tombstone.Credential != "" {
				t.Fatal("database deletion did not retain a credential-free revision tombstone")
			}
			publicRaw, _ := json.Marshal(liveAction(t, s, a, "managed_database_receipt", map[string]any{"instance_id": instance.ID, "database": name}))
			var publicReceipt ManagedDatabase
			if json.Unmarshal(publicRaw, &publicReceipt) != nil || publicReceipt.State != "deleted" || publicReceipt.Revision != 2 || publicReceipt.Username != tombstone.Username || strings.Contains(string(publicRaw), "credential") || strings.Contains(string(publicRaw), "password") {
				t.Fatal("authorized database receipt did not expose only public tombstone metadata")
			}
			listRaw, _ := json.Marshal(liveAction(t, s, a, "managed_databases_list", map[string]any{"instance_id": instance.ID}))
			var listed struct {
				Items []ManagedDatabase `json:"items"`
			}
			if json.Unmarshal(listRaw, &listed) != nil || strings.Contains(string(listRaw), "credential") || strings.Contains(string(listRaw), "password") {
				t.Fatal("managed database list returned invalid or secret-bearing metadata")
			}
			tombstoneFound := false
			for _, item := range listed.Items {
				if item.Database == name && item.State == "deleted" && item.Revision == 2 {
					tombstoneFound = true
				}
			}
			if !tombstoneFound {
				t.Fatal("managed database list hid the deleted ownership tombstone")
			}

			create := func(expected int64) json.RawMessage {
				raw, _ := json.Marshal(map[string]any{"instance_id": instance.ID, "database": name, "permission": "read_write", "expected_revision": expected})
				return raw
			}
			if _, err := s.Execute(ctx, a, "managed_database_create", create(1)); core.ErrorStatus(err) != 412 || core.ErrorCode(err) != "revision_conflict" {
				t.Fatal("stale create revision did not fail before server mutation", core.ErrorCode(err))
			}

			username := tombstone.Username
			password := randomDatabasePassword()
			if engine == "postgresql" {
				_, err := admin.ExecContext(ctx, "CREATE ROLE "+quoteDatabaseIdentifier(engine, username)+" LOGIN PASSWORD "+pgSQLLiteral(password))
				if err != nil {
					t.Fatal("external SQL role fixture setup failed")
				}
				defer admin.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+quoteDatabaseIdentifier(engine, username))
			} else {
				_, err := admin.ExecContext(ctx, "CREATE USER "+quoteDatabaseIdentifier(engine, username)+"@'127.0.0.1' IDENTIFIED BY "+mysqlLiteral(password))
				if err != nil {
					t.Fatal("external SQL user fixture setup failed")
				}
				defer admin.ExecContext(context.Background(), "DROP USER IF EXISTS "+quoteDatabaseIdentifier(engine, username)+"@'127.0.0.1'")
			}
			if _, err := s.Execute(ctx, a, "managed_database_create", create(2)); core.ErrorStatus(err) != 409 || core.ErrorCode(err) != "ownership_conflict" {
				t.Fatal("same-name external SQL user was accepted for database recreation", core.ErrorCode(err))
			}
			if exists, err := liveManagedSQLRoleExists(ctx, admin, engine, username); err != nil || !exists {
				t.Fatal("database recreation removed or changed an external SQL user")
			}
			if current := liveManagedRecord(t, e, instance, name); current.State != "deleted" || current.Revision != 2 {
				t.Fatal("external-user conflict changed the ownership tombstone")
			}
			if engine == "postgresql" {
				_, _ = admin.ExecContext(ctx, "DROP ROLE IF EXISTS "+quoteDatabaseIdentifier(engine, username))
			} else {
				_, _ = admin.ExecContext(ctx, "DROP USER IF EXISTS "+quoteDatabaseIdentifier(engine, username)+"@'127.0.0.1'")
			}

			_, err := admin.ExecContext(ctx, "CREATE DATABASE "+quoteDatabaseIdentifier(engine, name))
			if err != nil {
				t.Fatal("external SQL database fixture setup failed")
			}
			external := liveManagedSQLAdmin(t, engine, fixture, name)
			if _, err = external.ExecContext(ctx, "CREATE TABLE owned_marker(value VARCHAR(32))"); err == nil {
				_, err = external.ExecContext(ctx, "INSERT INTO owned_marker(value) VALUES('external')")
			}
			external.Close()
			if err != nil {
				t.Fatal("external SQL data fixture setup failed")
			}
			if _, err = s.Execute(ctx, a, "managed_database_create", create(2)); core.ErrorStatus(err) != 409 || core.ErrorCode(err) != "ownership_conflict" {
				t.Fatal("same-name external SQL database was accepted for recreation", core.ErrorCode(err))
			}
			external = liveManagedSQLAdmin(t, engine, fixture, name)
			var marker string
			if err = external.QueryRowContext(ctx, "SELECT value FROM owned_marker").Scan(&marker); err != nil || marker != "external" {
				t.Fatal("database recreation changed existing external SQL data")
			}
			external.Close()
			_, _ = admin.ExecContext(ctx, "DROP DATABASE "+quoteDatabaseIdentifier(engine, name))

			liveAction(t, s, a, "managed_database_create", map[string]any{"instance_id": instance.ID, "database": name, "permission": "read_write", "expected_revision": 2})
			recreated := liveManagedRecord(t, e, instance, name)
			fresh := liveManagedRecord(t, e, instance, name)
			if recreated.Revision != 3 || fresh.State != "active" || fresh.Revision != 3 || fresh.Username != username || fresh.Credential == "" || fresh.Credential == oldCredential {
				t.Fatal("same-name recreation did not commit a fresh credential at the next revision")
			}
			profile := DatabaseConnection{Engine: engine, Host: fixture.Host, Port: fixture.Port, Username: fresh.Username, Database: name, Access: "database"}
			client, err := openSQLProfile(ctx, profile, fresh.Credential, name)
			if err != nil {
				t.Fatal("new managed SQL identity did not authenticate")
			}
			var tableCount int
			query := "SELECT count(*) FROM information_schema.tables WHERE table_schema=? AND table_name='owned_marker'"
			if engine == "postgresql" {
				query = "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='owned_marker'"
			}
			if engine == "postgresql" {
				err = client.QueryRowContext(ctx, query).Scan(&tableCount)
			} else {
				err = client.QueryRowContext(ctx, query, name).Scan(&tableCount)
			}
			client.Close()
			if err != nil || tableCount != 0 {
				t.Fatal("recreated database retained data from the external database")
			}

			liveManagedJob(t, s, a, "managed_database_delete", map[string]any{"instance_id": instance.ID, "database": name, "expected_revision": 3})
			var wg sync.WaitGroup
			results := make(chan error, 2)
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := s.Execute(ctx, a, "managed_database_create", create(4))
					results <- err
				}()
			}
			wg.Wait()
			close(results)
			successes, conflicts := 0, 0
			for err := range results {
				if err == nil {
					successes++
				} else if core.ErrorStatus(err) == 412 && core.ErrorCode(err) == "revision_conflict" {
					conflicts++
				} else {
					t.Fatalf("concurrent same-revision create returned unexpected result: %s", core.ErrorCode(err))
				}
			}
			if successes != 1 || conflicts != 1 {
				t.Fatalf("same tombstone revision was not consumed once: successes=%d conflicts=%d", successes, conflicts)
			}
			if current := liveManagedRecord(t, e, instance, name); current.State != "active" || current.Revision != 5 {
				t.Fatal("concurrent create did not leave one active next-revision receipt")
			}
			liveManagedJob(t, s, a, "managed_database_delete", map[string]any{"instance_id": instance.ID, "database": name, "expected_revision": 5})
		})
	}
}

func TestLiveManagedDatabaseCreateRacePreservesExternalDatabaseAndCreatingReceipt(t *testing.T) {
	for _, engine := range []string{"mariadb", "postgresql"} {
		t.Run(engine, func(t *testing.T) {
			_, _, _, e, instance, fixture := liveManagedFixture(t, engine)
			ctx := context.Background()
			name := uniqueManagedName()
			admin := liveManagedSQLAdmin(t, engine, fixture, fixture.Database)
			defer admin.Close()
			t.Cleanup(func() {
				cleanupAdmin := liveManagedSQLAdmin(t, engine, fixture, fixture.Database)
				defer cleanupAdmin.Close()
				_, _ = cleanupAdmin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+quoteDatabaseIdentifier(engine, name))
				username := "pl_" + hex.EncodeToString(sha256Digest(instance.ID + "|" + name)[:10])
				if engine == "postgresql" {
					_, _ = cleanupAdmin.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+quoteDatabaseIdentifier(engine, username))
				} else {
					_, _ = cleanupAdmin.ExecContext(context.Background(), "DROP USER IF EXISTS "+quoteDatabaseIdentifier(engine, username)+"@'127.0.0.1'")
				}
			})
			input := ManagedDatabaseInput{InstanceID: instance.ID, Database: name, Permission: "read_write"}
			_, err := e.createManagedDatabaseWithRaceHook(ctx, instance, input, func() error {
				if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+quoteDatabaseIdentifier(engine, name)); err != nil {
					return err
				}
				external := liveManagedSQLAdmin(t, engine, fixture, name)
				defer external.Close()
				if _, err := external.ExecContext(ctx, "CREATE TABLE owned_marker(value VARCHAR(32))"); err != nil {
					return err
				}
				_, err := external.ExecContext(ctx, "INSERT INTO owned_marker(value) VALUES('race-winner')")
				return err
			})
			if core.ErrorStatus(err) != 409 || core.ErrorCode(err) != "operation_recovery_required" {
				t.Fatal("server-side duplicate CREATE was treated as successful", core.ErrorCode(err))
			}
			receipt := liveManagedRecord(t, e, instance, name)
			if receipt.State != "creating" || receipt.Revision != 1 {
				t.Fatal("failed server-side CREATE lost its durable recovery receipt")
			}
			if exists, err := liveManagedSQLRoleExists(ctx, admin, engine, receipt.Username); err != nil || exists {
				t.Fatal("database CREATE race left an owned login that could take over the external database")
			}
			external := liveManagedSQLAdmin(t, engine, fixture, name)
			var marker string
			if err := external.QueryRowContext(ctx, "SELECT value FROM owned_marker").Scan(&marker); err != nil || marker != "race-winner" {
				t.Fatal("failed managed CREATE changed the external race winner's data")
			}
			external.Close()
		})
	}
}

func TestLiveManagedMongoCreatePermissionsBSONRestoreAndDelete(t *testing.T) {
	s, a, m, e, instance, _ := liveManagedFixture(t, "mongodb")
	ctx := context.Background()
	name := uniqueManagedName()
	admin, err := e.managedMongo(ctx, instance)
	if err != nil {
		t.Fatal("administrative fixture authentication failed")
	}
	defer admin.Disconnect(ctx)
	t.Cleanup(func() {
		admin, err := e.managedMongo(context.Background(), instance)
		if err != nil {
			t.Error("isolated database cleanup authentication failed")
			return
		}
		defer admin.Disconnect(context.Background())
		_ = admin.Database(name).Drop(context.Background())
		_ = admin.Database(name).RunCommand(context.Background(), bson.D{{Key: "dropAllUsersFromDatabase", Value: 1}}).Err()
		names, err := admin.ListDatabaseNames(context.Background(), bson.D{{Key: "name", Value: name}})
		if err != nil || len(names) != 0 {
			t.Error("isolated MongoDB cleanup verification failed")
		}
	})
	create := map[string]any{"instance_id": instance.ID, "database": name, "permission": "read_write"}
	liveAction(t, s, a, "managed_database_create", create)
	record := liveManagedRecord(t, e, instance, name)
	if record.State != "active" || record.Revision != 1 {
		t.Fatal("database creation receipt was not committed")
	}
	profileID := "managed_" + hex.EncodeToString(sha256Digest(instance.ID + "|" + name)[:16])
	profile, err := m.databaseProfile(ctx, profileID)
	if err != nil {
		t.Fatal("managed scoped profile was not saved")
	}
	client, err := openMongoProfile(ctx, profile, record.Credential)
	if err != nil {
		t.Fatal("dedicated MongoDB authentication failed")
	}
	defer client.Disconnect(ctx)
	collection := client.Database(name).Collection("owned_records")
	if _, err = collection.InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "binary", Value: bson.Binary{Subtype: 0x80, Data: []byte{0, 0xff, 2}}}, {Key: "value", Value: int64(9007199254740993)}}); err != nil {
		t.Fatal("isolated BSON fixture insert failed")
	}
	if err = client.Database(name).RunCommand(ctx, bson.D{{Key: "createIndexes", Value: "owned_records"}, {Key: "indexes", Value: bson.A{bson.D{{Key: "key", Value: bson.D{{Key: "value", Value: 1}}}, {Key: "name", Value: "value_unique"}, {Key: "unique", Value: true}}}}}).Err(); err != nil {
		t.Fatal("isolated unique index creation failed")
	}
	liveAction(t, s, a, "managed_database_set_permission", map[string]any{"instance_id": instance.ID, "database": name, "permission": "read", "expected_revision": 1})
	if _, err = collection.InsertOne(ctx, bson.D{{Key: "_id", Value: 2}}); err == nil {
		t.Fatal("read-only database identity retained actual write permission")
	}
	if err = collection.FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Err(); err != nil {
		t.Fatal("read-only database identity lost actual read permission")
	}
	raw, _ := json.Marshal(map[string]any{"instance_id": instance.ID, "database": name, "permission": "read_write", "expected_revision": 1})
	if _, err = s.Execute(ctx, a, "managed_database_set_permission", raw); core.ErrorStatus(err) != 412 {
		t.Fatal("stale permission change did not fail before mutation")
	}
	liveAction(t, s, a, "managed_database_set_permission", map[string]any{"instance_id": instance.ID, "database": name, "permission": "read_write", "expected_revision": 2})
	password := "isolated 'quoted\\ password 1234"
	liveAction(t, s, a, "managed_database_set_password", map[string]any{"instance_id": instance.ID, "database": name, "password": password, "expected_revision": 3})
	record = liveManagedRecord(t, e, instance, name)
	if record.Revision != 4 || record.Credential != password {
		t.Fatal("password receipt mismatch")
	}
	client.Disconnect(ctx)
	client, err = openMongoProfile(ctx, profile, password)
	if err != nil {
		t.Fatal("changed password could not authenticate")
	}
	defer client.Disconnect(ctx)
	backupJSON := liveManagedJob(t, s, a, "managed_database_backup", map[string]any{"instance_id": instance.ID, "database": name})
	var backup DatabaseBackup
	if json.Unmarshal(backupJSON, &backup) != nil || backup.Bytes == 0 {
		t.Fatal("real backup task did not return integrity receipt")
	}
	if _, err = client.Database(name).Collection("owned_records").DeleteMany(ctx, bson.D{}); err != nil {
		t.Fatal("isolated restore precondition failed")
	}
	if _, err = client.Database(name).Collection("after_backup").InsertOne(ctx, bson.D{{Key: "new", Value: true}}); err != nil {
		t.Fatal("isolated new collection failed")
	}
	liveManagedJob(t, s, a, "managed_database_restore", map[string]any{"instance_id": instance.ID, "database": name, "backup_id": backup.ID})
	var restored struct {
		Binary bson.Binary `bson:"binary"`
		Value  int64       `bson:"value"`
	}
	if err = client.Database(name).Collection("owned_records").FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&restored); err != nil || restored.Value != 9007199254740993 || restored.Binary.Subtype != 0x80 || !bytes.Equal(restored.Binary.Data, []byte{0, 0xff, 2}) {
		t.Fatal("restore did not preserve native BSON values")
	}
	if _, err = client.Database(name).Collection("owned_records").InsertOne(ctx, bson.D{{Key: "_id", Value: 3}, {Key: "value", Value: int64(9007199254740993)}}); err == nil {
		t.Fatal("restore lost the unique index")
	}
	names, err := client.Database(name).ListCollectionNames(ctx, bson.D{{Key: "name", Value: "after_backup"}})
	if err != nil || len(names) != 0 {
		t.Fatal("restore retained a collection absent from the backup")
	}
	liveAction(t, s, a, "managed_database_backup_delete", map[string]any{"instance_id": instance.ID, "database": name, "backup_id": backup.ID})
	if _, err = os.Stat(filepath.Join(instance.Paths.BackupDir, backup.Filename)); !os.IsNotExist(err) {
		t.Fatal("deleted backup bytes remain")
	}
	if _, err = e.databaseBackupMetadata(ctx, instance, name, backup.ID); core.ErrorStatus(err) != 404 {
		t.Fatal("deleted backup receipt remains downloadable")
	}
	oldCredential := record.Credential
	liveManagedJob(t, s, a, "managed_database_delete", map[string]any{"instance_id": instance.ID, "database": name, "expected_revision": 4})
	tombstone := liveManagedRecord(t, e, instance, name)
	if tombstone.State != "deleted" || tombstone.Revision != 5 || tombstone.Credential != "" {
		t.Fatal("database deletion receipt missing")
	}

	// MongoDB createUser implicitly creates its target database, so a DB-absent
	// check cannot be made atomic with creation. Keep both external-object cases
	// visible and require the typed tombstone limitation to leave them untouched.
	createInput := map[string]any{"instance_id": instance.ID, "database": name, "permission": "read_write", "expected_revision": tombstone.Revision}
	if err = admin.Database(name).RunCommand(ctx, bson.D{{Key: "createUser", Value: tombstone.Username}, {Key: "pwd", Value: randomDatabasePassword()}, {Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "readWrite"}, {Key: "db", Value: name}}}}}).Err(); err != nil {
		t.Fatal("external MongoDB user fixture setup failed")
	}
	if _, err = s.Execute(ctx, a, "managed_database_create", mustDatabaseJSON(t, createInput)); core.ErrorStatus(err) != 409 || core.ErrorCode(err) != "database_recreation_unsupported" {
		t.Fatal("MongoDB tombstone recreation did not report its ownership-fence limitation", core.ErrorCode(err))
	}
	var externalUsers struct {
		Users []bson.M `bson:"users"`
	}
	if err = admin.Database(name).RunCommand(ctx, bson.D{{Key: "usersInfo", Value: tombstone.Username}}).Decode(&externalUsers); err != nil || len(externalUsers.Users) != 1 {
		t.Fatal("unsupported MongoDB recreation changed the external user")
	}
	_ = admin.Database(name).Drop(ctx)

	if _, err = admin.Database(name).Collection("external_marker").InsertOne(ctx, bson.D{{Key: "owner", Value: "external"}}); err != nil {
		t.Fatal("external MongoDB data fixture setup failed")
	}
	if _, err = s.Execute(ctx, a, "managed_database_create", mustDatabaseJSON(t, createInput)); core.ErrorStatus(err) != 409 || core.ErrorCode(err) != "database_recreation_unsupported" {
		t.Fatal("MongoDB tombstone recreation accepted an external database", core.ErrorCode(err))
	}
	var marker struct {
		Owner string `bson:"owner"`
	}
	if err = admin.Database(name).Collection("external_marker").FindOne(ctx, bson.D{}).Decode(&marker); err != nil || marker.Owner != "external" {
		t.Fatal("unsupported MongoDB recreation changed external database data")
	}
	_ = admin.Database(name).Drop(ctx)
	if _, err = s.Execute(ctx, a, "managed_database_create", mustDatabaseJSON(t, createInput)); core.ErrorStatus(err) != 409 || core.ErrorCode(err) != "database_recreation_unsupported" {
		t.Fatal("MongoDB tombstone recreation was not explicitly blocked after authoritative absence", core.ErrorCode(err))
	}
	if current := liveManagedRecord(t, e, instance, name); current.State != "deleted" || current.Revision != tombstone.Revision || current.Credential != "" {
		t.Fatal("unsupported MongoDB recreation changed the deletion tombstone")
	}
	private, err := os.ReadFile(filepath.Join(e.config.StateDir, "private", managedDatabaseKey(instance.ID, name)+".json"))
	if err != nil || strings.Contains(string(private), oldCredential) {
		t.Fatal("private credential state was not encrypted")
	}
	t.Logf("isolated database %s created, read/write checked, BSON+index restored; MongoDB same-name recreation remains typed blocked pending atomic ownership fence", name)
}

func mustDatabaseJSON(t *testing.T, input any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal("database input serialization failed")
	}
	return raw
}

func TestLiveManagedPostgresPermissionOwnershipAndCleanup(t *testing.T) {
	s, a, m, e, instance, _ := liveManagedFixture(t, "postgresql")
	ctx := context.Background()
	name := uniqueManagedName()
	admin, err := e.openManagedAdmin(ctx, instance)
	if err != nil {
		t.Fatal("administrative PostgreSQL authentication failed")
	}
	defer admin.Close()
	var username, owner string
	t.Cleanup(func() {
		admin, err := e.openManagedAdmin(context.Background(), instance)
		if err != nil {
			t.Error("isolated PostgreSQL cleanup authentication failed")
			return
		}
		defer admin.Close()
		if _, err := admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+quoteDatabaseIdentifier("postgresql", name)); err != nil {
			t.Error("isolated PostgreSQL database cleanup failed")
		}
		for _, role := range []string{username, owner} {
			if role != "" {
				if _, err := admin.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+quoteDatabaseIdentifier("postgresql", role)); err != nil {
					t.Error("isolated PostgreSQL role cleanup failed")
				}
			}
		}
		var count int
		if err := admin.QueryRowContext(context.Background(), "SELECT count(*) FROM pg_database WHERE datname=$1", name).Scan(&count); err != nil || count != 0 {
			t.Error("isolated PostgreSQL cleanup verification failed")
		}
	})
	liveAction(t, s, a, "managed_database_create", map[string]any{"instance_id": instance.ID, "database": name, "permission": "read_write"})
	record := liveManagedRecord(t, e, instance, name)
	username = record.Username
	profileID := "managed_" + hex.EncodeToString(sha256Digest(instance.ID + "|" + name)[:16])
	profile, err := m.databaseProfile(ctx, profileID)
	if err != nil {
		t.Fatal("PostgreSQL scoped profile missing")
	}
	db, err := openSQLProfile(ctx, profile, record.Credential, name)
	if err != nil {
		t.Fatal("dedicated PostgreSQL login failed")
	}
	defer db.Close()
	for _, statement := range []string{"CREATE TABLE owned_records(id BIGINT PRIMARY KEY, payload BYTEA)", "CREATE SEQUENCE owned_sequence", "INSERT INTO owned_records VALUES (1,decode('00ff02','hex'))", "CREATE FUNCTION owned_writer() RETURNS void LANGUAGE SQL SECURITY DEFINER AS $$ INSERT INTO owned_records VALUES (2,decode('03','hex')) $$"} {
		if _, err = db.ExecContext(ctx, statement); err != nil {
			t.Fatal("isolated PostgreSQL data/definer fixture failed")
		}
	}
	liveAction(t, s, a, "managed_database_set_permission", map[string]any{"instance_id": instance.ID, "database": name, "permission": "read", "expected_revision": 1})
	record = liveManagedRecord(t, e, instance, name)
	owner = record.PermissionOwner
	if owner == "" {
		t.Fatal("read-only ownership role was not persisted")
	}
	for _, statement := range []string{"INSERT INTO owned_records VALUES(3,NULL)", "ALTER TABLE owned_records ADD COLUMN forbidden BOOLEAN", "SELECT owned_writer()", "CREATE TABLE forbidden(id INT)"} {
		if _, err = db.ExecContext(ctx, statement); err == nil {
			t.Fatal("read-only identity retained write/ownership/definer permission")
		}
	}
	var data []byte
	if err = db.QueryRowContext(ctx, "SELECT payload FROM owned_records WHERE id=1").Scan(&data); err != nil || !bytes.Equal(data, []byte{0, 255, 2}) {
		t.Fatal("read-only identity lost actual data access")
	}
	liveAction(t, s, a, "managed_database_set_permission", map[string]any{"instance_id": instance.ID, "database": name, "permission": "data_write", "expected_revision": 2})
	for _, statement := range []string{"INSERT INTO owned_records VALUES(3,NULL)", "UPDATE owned_records SET payload=decode('04','hex') WHERE id=3", "DELETE FROM owned_records WHERE id=3", "SELECT nextval('owned_sequence')"} {
		if _, err = db.ExecContext(ctx, statement); err != nil {
			t.Fatal("data-write identity lost actual CRUD/sequence access")
		}
	}
	for _, statement := range []string{"ALTER TABLE owned_records ADD COLUMN forbidden BOOLEAN", "TRUNCATE owned_records", "CREATE TABLE forbidden(id INT)", "SELECT owned_writer()", "SELECT setval('owned_sequence',100)"} {
		if _, err = db.ExecContext(ctx, statement); err == nil {
			t.Fatal("data-write identity gained DDL/routine/sequence-configuration permission")
		}
	}
	liveAction(t, s, a, "managed_database_set_permission", map[string]any{"instance_id": instance.ID, "database": name, "permission": "read_write", "expected_revision": 3})
	if _, err = db.ExecContext(ctx, "ALTER TABLE owned_records ADD COLUMN restored BOOLEAN"); err != nil {
		t.Fatal("write permission did not restore table ownership")
	}
	if _, err = db.ExecContext(ctx, "SELECT owned_writer()"); err != nil {
		t.Fatal("write permission did not restore routine access")
	}
	var routineOwner string
	if err = db.QueryRowContext(ctx, "SELECT pg_get_userbyid(proowner) FROM pg_proc WHERE proname='owned_writer'").Scan(&routineOwner); err != nil || routineOwner != username {
		t.Fatal("definer routine was transferred to an administrator")
	}
	liveAction(t, s, a, "managed_database_set_permission", map[string]any{"instance_id": instance.ID, "database": name, "permission": "read", "expected_revision": 4})
	db.Close()
	liveManagedJob(t, s, a, "managed_database_delete", map[string]any{"instance_id": instance.ID, "database": name, "expected_revision": 5})
	var count int
	if err = admin.QueryRowContext(ctx, "SELECT count(*) FROM pg_roles WHERE rolname IN ($1,$2)", username, owner).Scan(&count); err != nil || count != 0 {
		t.Fatal("database deletion left dedicated login/ownership roles behind")
	}
	t.Logf("isolated database %s ownership and SECURITY DEFINER revocation checked; database and two dedicated roles removed", name)
}

func TestLiveManagedMariaPermissionRevokesAutomaticRoutineGrants(t *testing.T) {
	_, _, _, e, instance, in := liveManagedFixture(t, "mariadb")
	ctx := context.Background()
	name := uniqueManagedName()
	admin, err := openSQLProfile(ctx, DatabaseConnection{Engine: "mariadb", Host: in.Host, Port: in.Port, Username: in.Username, Database: in.Database, Access: "instance"}, in.Password, in.Database)
	if err != nil {
		t.Fatal("isolated MariaDB admin authentication failed")
	}
	// Keep this test's session-mode change local to this exact connection.
	admin.SetMaxOpenConns(1)
	admin.SetMaxIdleConns(1)
	defer admin.Close()
	if _, err = admin.ExecContext(ctx, "SET SESSION sql_mode='NO_BACKSLASH_ESCAPES'"); err != nil {
		t.Fatal("local test SQL mode setup failed")
	}
	username := "qa_pl_" + fmt.Sprintf("%x", time.Now().UnixNano())
	role := quoteDatabaseIdentifier("mariadb", username) + "@'127.0.0.1'"
	password := randomDatabasePassword()
	defer func() {
		if _, err := admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+quoteDatabaseIdentifier("mariadb", name)); err != nil {
			t.Error("isolated MariaDB DB cleanup failed")
		}
		if _, err := admin.ExecContext(context.Background(), "DROP USER IF EXISTS "+role); err != nil {
			t.Error("isolated MariaDB role cleanup failed")
		}
		var count int
		if err := admin.QueryRowContext(context.Background(), "SELECT count(*) FROM mysql.user WHERE User=?", username).Scan(&count); err != nil || count != 0 {
			t.Error("isolated MariaDB cleanup verification failed")
		}
	}()
	for _, statement := range []string{"CREATE USER " + role + " IDENTIFIED BY " + mysqlLiteral(password), "CREATE DATABASE " + quoteDatabaseIdentifier("mariadb", name), "GRANT ALL PRIVILEGES ON " + mysqlGrantIdentifier(ctx, admin, name) + ".* TO " + role} {
		if _, err = admin.ExecContext(ctx, statement); err != nil {
			t.Fatal("isolated MariaDB database/role setup failed")
		}
	}
	profile := DatabaseConnection{Engine: "mariadb", Host: in.Host, Port: in.Port, Username: username, Database: name, Access: "database"}
	db, err := openSQLProfile(ctx, profile, password, name)
	if err != nil {
		t.Fatal("isolated MariaDB dedicated login failed")
	}
	defer db.Close()
	for _, statement := range []string{"CREATE TABLE owned_records(id INT PRIMARY KEY)", "INSERT INTO owned_records VALUES(1)", "CREATE PROCEDURE owned_writer() SQL SECURITY DEFINER INSERT INTO owned_records VALUES(2)"} {
		if _, err = db.ExecContext(ctx, statement); err != nil {
			t.Fatal("isolated MariaDB data/routine setup failed")
		}
	}
	record := managedDatabaseRecord{ManagedDatabase: ManagedDatabase{InstanceID: instance.ID, Database: name, Username: username, Engine: instance.PluginID, Permission: "read_write", State: "active", Revision: 1}, Credential: password}
	if err = e.setSQLDatabasePermission(ctx, instance, &record, "read", admin); err != nil {
		t.Fatal("MariaDB production read-permission helper failed", core.ErrorCode(err))
	}
	for _, statement := range []string{"INSERT INTO owned_records VALUES(3)", "ALTER TABLE owned_records ADD COLUMN forbidden INT", "CALL owned_writer()", "DROP PROCEDURE owned_writer"} {
		if _, err = db.ExecContext(ctx, statement); err == nil {
			t.Fatal("read-only MariaDB identity retained data/DDL/routine permission")
		}
	}
	var count int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM owned_records").Scan(&count); err != nil || count != 1 {
		t.Fatal("read-only MariaDB identity lost data access")
	}
	record.Permission = "read"
	if err = e.setSQLDatabasePermission(ctx, instance, &record, "data_write", admin); err != nil {
		t.Fatal("MariaDB production data-write helper failed", core.ErrorCode(err))
	}
	for _, statement := range []string{"INSERT INTO owned_records VALUES(3)", "UPDATE owned_records SET id=4 WHERE id=3", "DELETE FROM owned_records WHERE id=4"} {
		if _, err = db.ExecContext(ctx, statement); err != nil {
			t.Fatal("MariaDB data-write identity lost actual CRUD access")
		}
	}
	for _, statement := range []string{"ALTER TABLE owned_records ADD COLUMN forbidden INT", "TRUNCATE owned_records", "CREATE TABLE forbidden(id INT)", "CALL owned_writer()", "DROP PROCEDURE owned_writer"} {
		if _, err = db.ExecContext(ctx, statement); err == nil {
			t.Fatal("MariaDB data-write identity gained DDL/routine permission")
		}
	}
	record.Permission = "data_write"
	if err = e.setSQLDatabasePermission(ctx, instance, &record, "read_write", admin); err != nil {
		t.Fatal("MariaDB production write-permission helper failed", core.ErrorCode(err))
	}
	if _, err = db.ExecContext(ctx, "CALL owned_writer()"); err != nil {
		t.Fatal("MariaDB write permission did not restore routine execution")
	}
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM owned_records").Scan(&count); err != nil || count != 2 {
		t.Fatal("MariaDB restored routine did not write actual data")
	}
	t.Logf("isolated database %s routine-level privileges revoked and restored; owned database/role cleanup verified", name)
}
