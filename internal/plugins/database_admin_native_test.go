//go:build linux

package plugins

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

// Opt-in destructive credential acceptance on the isolated native SQL VM.
// Supply a root-private PrivilegedConfig and existing, running managed SQL IDs.
// No daemon is installed, no orphan data is adopted, and no password is printed.
func TestNativeDatabaseAdminSQLCommitCancellationAndExplicitRecovery(t *testing.T) {
	path := os.Getenv("POWERLINUX_NATIVE_SQL_ADMIN_CONFIG")
	if path == "" {
		t.Skip("requires root-owned native SQL VM policy and managed instances")
	}
	if os.Geteuid() != 0 {
		t.Fatal("native administrator recovery acceptance must run as root")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("native SQL policy must be a private regular file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != 0 {
		t.Fatal("native SQL policy must be root-owned")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("native SQL policy unavailable")
	}
	var config PrivilegedConfig
	if json.Unmarshal(raw, &config) != nil || !config.UseSystemd {
		t.Fatal("invalid native SQL policy")
	}
	ids := strings.Split(os.Getenv("POWERLINUX_NATIVE_SQL_ADMIN_INSTANCES"), ",")
	if len(ids) != 2 {
		t.Fatal("native recovery acceptance requires PostgreSQL and MariaDB/MySQL managed instance IDs")
	}
	engines := map[string]bool{}
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			ctx := context.Background()
			e, err := NewPrivilegedExecutor(config)
			if err != nil {
				t.Fatal("native root executor unavailable")
			}
			i, err := e.get(ctx, id, false)
			if err != nil || i.Ownership != "managed" || i.Status != "running" {
				t.Fatal("native instance requires its active trusted managed receipt")
			}
			if i.PluginID == "postgresql" {
				engines["postgresql"] = true
			} else if i.PluginID == "mariadb" || i.PluginID == "mysql" || i.PluginID == "mysql-community" {
				engines["mysql"] = true
			} else {
				t.Fatal("native SQL instance has unsupported engine")
			}
			out, err := callDatabaseAdmin(e, i, "inspect", "")
			if err != nil {
				t.Fatal("native credential inspection failed", core.ErrorCode(err))
			}
			state := out.(databaseAdminState)
			if state.State == "legacy_unversioned" {
				out, err = callDatabaseAdmin(e, i, "initialize", "")
				if err != nil {
					t.Fatal("legacy version initialization failed", core.ErrorCode(err))
				}
				state = out.(databaseAdminState)
			}
			if state.State != "active" {
				t.Fatal("native fixture has unresolved administrator rotation")
			}
			// The user-selected secret path must work against the actual SQL daemon,
			// including literal characters with distinct PostgreSQL/MySQL grammars.
			custom := "NativeSQL-" + state.CurrentCredentialVersion + "-'\\-password"
			request, _ := json.Marshal(databaseAdminInput{InstanceID: id, Command: "rotate", CurrentCredentialVersion: state.CurrentCredentialVersion, Password: custom})
			out, err = e.databaseAdmin(ctx, request)
			if err != nil {
				t.Fatal("native custom-password SQL rotation failed", core.ErrorCode(err))
			}
			state = out.(databaseAdminState)
			// Stop before SQL: passive recovery must preserve intent, while the
			// separate explicit apply_pending verb can perform the pending ALTER.
			unlock, err := e.lockContext(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			var old managedDatabaseCredential
			if err = e.load("private", databasePrivateID(i), &old); err != nil {
				unlock()
				t.Fatal("active native receipt unavailable")
			}
			next, _, err := e.stageDatabaseAdminRotation(i, old, "")
			unlock()
			if err != nil {
				t.Fatal("native pre-SQL checkpoint failed", core.ErrorCode(err))
			}
			if !e.probeDatabaseAdmin(ctx, i, old) || e.probeDatabaseAdmin(ctx, i, next) {
				t.Fatal("pre-SQL old/new authentication boundary was not verified")
			}
			restarted, err := NewPrivilegedExecutor(config)
			if err != nil {
				t.Fatal("native restart failed")
			}
			if _, err = callDatabaseAdmin(restarted, i, "recover", old.Version); core.ErrorCode(err) != "operation_recovery_required" {
				t.Fatal("passive recovery replayed uncommitted SQL")
			}
			if _, err = restarted.openManagedAdmin(ctx, i); core.ErrorCode(err) != "operation_recovery_required" {
				t.Fatal("ordinary admin bypassed pending native intent")
			}
			out, err = callDatabaseAdmin(restarted, i, "apply_pending", old.Version)
			if err != nil || out.(databaseAdminState).CurrentCredentialVersion != next.Version {
				t.Fatal("explicit pending SQL apply failed", core.ErrorCode(err))
			}
			// Stop after SQL commits but before active receipt persistence. This is the
			// real authentication boundary that storage-only simulations cannot test.
			e = restarted
			unlock, err = e.lockContext(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if err = e.load("private", databasePrivateID(i), &old); err != nil {
				unlock()
				t.Fatal("active native receipt unavailable")
			}
			next, intent, err := e.stageDatabaseAdminRotation(i, old, "")
			if err == nil {
				err = e.alterDatabaseAdmin(ctx, i, old, next)
			}
			if err != nil {
				unlock()
				t.Fatal("native SQL commit checkpoint failed", core.ErrorCode(err))
			}
			if e.probeDatabaseAdmin(ctx, i, old) || !e.probeDatabaseAdmin(ctx, i, next) {
				unlock()
				t.Fatal("post-SQL old/new authentication boundary was not verified")
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err = e.commitDatabaseAdmin(canceled, i, next, intent); err == nil {
				unlock()
				t.Fatal("canceled native commit advanced the active receipt")
			}
			var preserved managedDatabaseCredential
			if err = e.load("private", databasePrivateID(i), &preserved); err != nil || preserved.Version != old.Version {
				unlock()
				t.Fatal("SQL commit cancellation failed to preserve old active receipt")
			}
			unlock()
			restarted, err = NewPrivilegedExecutor(config)
			if err != nil {
				t.Fatal("native post-SQL restart failed")
			}
			out, err = callDatabaseAdmin(restarted, i, "recover", old.Version)
			if err != nil || out.(databaseAdminState).CurrentCredentialVersion != next.Version || out.(databaseAdminState).State != "active" {
				t.Fatal("committed SQL could not recover from its new private credential", core.ErrorCode(err))
			}
			if !restarted.probeDatabaseAdmin(ctx, i, next) {
				t.Fatal("recovered native administrator authentication failed")
			}
			// Fail the actual atomic rename after SQL has committed. A directory at
			// the active receipt target produces a filesystem error even as root.
			e = restarted
			unlock, err = e.lockContext(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if err = e.load("private", databasePrivateID(i), &old); err != nil {
				unlock()
				t.Fatal("active native receipt unavailable")
			}
			next, intent, err = e.stageDatabaseAdminRotation(i, old, "")
			if err == nil {
				err = e.alterDatabaseAdmin(ctx, i, old, next)
			}
			if err != nil {
				unlock()
				t.Fatal("native filesystem checkpoint SQL failed", core.ErrorCode(err))
			}
			faulted := false
			_, err = e.commitDatabaseAdminWithSave(ctx, i, next, intent, func(kind, key string, value any) error {
				if key != databasePrivateID(i) {
					return e.save(kind, key, value)
				}
				target := filepath.Join(config.StateDir, "private", key+".json")
				backup := target + ".native-test-preserved"
				if _, checkErr := os.Lstat(backup); !os.IsNotExist(checkErr) {
					return failure("native_test_recovery_required", "A previous native fixture backup must be inspected first", 409)
				}
				if renameErr := os.Rename(target, backup); renameErr != nil {
					return renameErr
				}
				if mkdirErr := os.Mkdir(target, 0700); mkdirErr != nil {
					_ = os.Rename(backup, target)
					return mkdirErr
				}
				failure := e.save(kind, key, value)
				removeErr := os.Remove(target)
				restoreErr := os.Rename(backup, target)
				if removeErr != nil || restoreErr != nil {
					t.Fatal("native receipt fixture restoration failed")
				}
				faulted = failure != nil
				return failure
			})
			if !faulted || core.ErrorCode(err) != "operation_recovery_required" {
				unlock()
				t.Fatal("actual native filesystem save failure did not preserve recovery")
			}
			if err = e.load("private", databasePrivateID(i), &preserved); err != nil || preserved.Version != old.Version {
				unlock()
				t.Fatal("filesystem failure lost the old active receipt")
			}
			if e.probeDatabaseAdmin(ctx, i, old) || !e.probeDatabaseAdmin(ctx, i, next) {
				unlock()
				t.Fatal("filesystem failure did not preserve the committed SQL authentication boundary")
			}
			unlock()
			restarted, err = NewPrivilegedExecutor(config)
			if err != nil {
				t.Fatal("native filesystem recovery restart failed")
			}
			out, err = callDatabaseAdmin(restarted, i, "recover", old.Version)
			if err != nil || out.(databaseAdminState).CurrentCredentialVersion != next.Version {
				t.Fatal("native filesystem save failure could not recover", core.ErrorCode(err))
			}
			t.Log("actual SQL custom rotation, pre-SQL apply, post-SQL cancellation, filesystem save failure and restart recovery passed")
		})
	}
	if !engines["postgresql"] || !engines["mysql"] {
		t.Fatal("native SQL recovery acceptance did not exercise both engine families")
	}
}
