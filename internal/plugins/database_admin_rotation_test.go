package plugins

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func databaseAdminFixture(t *testing.T) (*PrivilegedExecutor, Instance, managedDatabaseCredential) {
	t.Helper()
	e := testAgent(t)
	i := Instance{ID: "mariadb-admin", PluginID: "mariadb", InstanceID: "admin", Ownership: "managed", Status: "running"}
	i.Paths = e.instancePaths(i.PluginID, i.InstanceID)
	i.Service.ID = i.ID
	i.Service.Unit = "powerlinux-plugin-" + i.ID + ".service"
	if err := e.save("receipts", i.ID, i); err != nil {
		t.Fatal(err)
	}
	c := managedDatabaseCredential{Engine: i.PluginID, Username: "root", Password: "private-admin-original-secret", DataDir: i.Paths.DataDir, Initialized: true}
	if err := e.save("private", databasePrivateID(i), c); err != nil {
		t.Fatal(err)
	}
	return e, i, c
}
func callDatabaseAdmin(e *PrivilegedExecutor, i Instance, command, version string) (any, error) {
	raw, _ := json.Marshal(databaseAdminInput{InstanceID: i.ID, Command: command, CurrentCredentialVersion: version})
	return e.databaseAdmin(context.Background(), raw)
}
func TestDatabaseAdminLegacyRequiresExplicitVersionInitialization(t *testing.T) {
	e, i, c := databaseAdminFixture(t)
	result, err := callDatabaseAdmin(e, i, "inspect", "")
	if err != nil || result.(databaseAdminState).State != "legacy_unversioned" {
		t.Fatal("legacy state unavailable", err)
	}
	if _, err = callDatabaseAdmin(e, i, "rotate", ""); core.ErrorCode(err) != "invalid_input" {
		t.Fatal("unversioned rotation accepted")
	}
	result, err = callDatabaseAdmin(e, i, "initialize", "")
	if err != nil {
		t.Fatal(err)
	}
	state := result.(databaseAdminState)
	if !validDatabaseAdminVersion(state.CurrentCredentialVersion) || state.State != "active" {
		t.Fatal("version initialization failed")
	}
	version, err := e.loadDatabaseAdminVersion(i, state.CurrentCredentialVersion, c.Username)
	if err != nil || version.Password != c.Password {
		t.Fatal("initialization changed existing password")
	}
	if _, err = callDatabaseAdmin(e, i, "initialize", ""); core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("stale initialization accepted")
	}
	if _, err = callDatabaseAdmin(e, i, "rotate", strings.Repeat("f", 32)); core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("stale rotation reached SQL")
	}
	if err = e.saveDatabaseAdminVersion(i, version); core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("immutable version was overwritten")
	}
	public, _ := json.Marshal(state)
	if strings.Contains(string(public), c.Password) || strings.Contains(string(public), "secret") {
		t.Fatal("public state exposed a credential")
	}
	privatePath := filepath.Join(e.config.StateDir, "private", databaseAdminVersionID(i, version.Version)+".json")
	private, err := os.ReadFile(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(private), c.Password) {
		t.Fatal("credential version stored plaintext")
	}
	info, err := os.Stat(privatePath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("version is not private0600")
	}
}
func TestDatabaseAdminPendingSurvivesExecutorRestartAndBlocksOrdinaryAdmin(t *testing.T) {
	e, i, c := databaseAdminFixture(t)
	out, err := callDatabaseAdmin(e, i, "initialize", "")
	if err != nil {
		t.Fatal(err)
	}
	c.Version = out.(databaseAdminState).CurrentCredentialVersion
	next := c
	next.Version = strings.Repeat("1", 32)
	next.Password = "private-pending-secret"
	if err = e.saveDatabaseAdminVersion(i, next); err != nil {
		t.Fatal(err)
	}
	intent := databaseAdminIntent{InstanceID: i.ID, Engine: i.PluginID, DataDir: i.Paths.DataDir, Username: c.Username, FromVersion: c.Version, ToVersion: next.Version, State: "pending"}
	if err = e.save("private", databaseAdminIntentID(i), intent); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewPrivilegedExecutor(e.config)
	if err != nil {
		t.Fatal(err)
	}
	out, err = callDatabaseAdmin(restarted, i, "inspect", "")
	if err != nil || out.(databaseAdminState).State != "recovery_required" {
		t.Fatal("restart lost pending rotation", err)
	}
	if _, err = callDatabaseAdmin(restarted, i, "rotate", c.Version); core.ErrorCode(err) != "operation_recovery_required" {
		t.Fatal("pending rotation overwritten")
	}
	if _, err = restarted.openManagedAdmin(context.Background(), i); core.ErrorCode(err) != "operation_recovery_required" {
		t.Fatal("ordinary admin bypassed recovery")
	}
	// Model a SQL commit followed by active-receipt save and a crash before the
	// intent commit. Recovery must still expose the new CAS version as pending.
	if err = e.save("private", databasePrivateID(i), next); err != nil {
		t.Fatal(err)
	}
	out, err = callDatabaseAdmin(restarted, i, "inspect", "")
	state := out.(databaseAdminState)
	if err != nil || state.CurrentCredentialVersion != next.Version || state.State != "recovery_required" {
		t.Fatal("partial receipt commit concealed pending recovery")
	}
	if _, err = callDatabaseAdmin(restarted, i, "recover", c.Version); core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("stale recovery accepted")
	}
}
func TestDatabaseAdminRefusesUnknownBootstrapAndNonSQL(t *testing.T) {
	e, i, c := databaseAdminFixture(t)
	c.Initialized = false
	if err := e.save("private", databasePrivateID(i), c); err != nil {
		t.Fatal(err)
	}
	if _, err := callDatabaseAdmin(e, i, "initialize", ""); core.ErrorCode(err) != "credential_unavailable" {
		t.Fatal("interrupted bootstrap adopted")
	}
	c.Initialized = true
	c.Username = "external_role"
	if err := e.save("private", databasePrivateID(i), c); err != nil {
		t.Fatal(err)
	}
	if _, err := callDatabaseAdmin(e, i, "initialize", ""); core.ErrorCode(err) != "credential_unavailable" {
		t.Fatal("external role adopted")
	}
	i.PluginID = "mongodb"
	i.Paths = e.instancePaths(i.PluginID, i.InstanceID)
	if err := e.save("receipts", i.ID, i); err != nil {
		t.Fatal(err)
	}
	if _, err := callDatabaseAdmin(e, i, "inspect", ""); core.ErrorCode(err) != "unsupported_database" {
		t.Fatal("MongoDB falsely advertised support")
	}
}

func TestDatabaseAdminRecoveryConnectionRequiresPrivateCandidateProof(t *testing.T) {
	e, i, _ := databaseAdminFixture(t)
	if _, err := callDatabaseAdmin(e, i, "initialize", ""); err != nil {
		t.Fatal(err)
	}
	var current managedDatabaseCredential
	if err := e.load("private", databasePrivateID(i), &current); err != nil {
		t.Fatal(err)
	}
	if err := e.verifyDatabaseAdminCandidate(i, current); err != nil {
		t.Fatal("canonical private candidate was rejected")
	}
	forged := current
	forged.Password = "UNOWNED_CANDIDATE_PASSWORD"
	if err := e.verifyDatabaseAdminCandidate(i, forged); core.ErrorCode(err) != "credential_unavailable" {
		t.Fatal("unowned candidate reached administrative connection")
	}
	next, _, err := e.stageDatabaseAdminRotation(i, current, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = e.verifyDatabaseAdminCandidate(i, next); err != nil {
		t.Fatal("protected immutable pending candidate was rejected")
	}
	forged = next
	forged.Password = "FORGED_PENDING_CANDIDATE_PASSWORD"
	if err = e.verifyDatabaseAdminCandidate(i, forged); core.ErrorCode(err) != "credential_unavailable" {
		t.Fatal("candidate password differed from protected immutable version")
	}
}
