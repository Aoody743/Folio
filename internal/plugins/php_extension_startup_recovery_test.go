package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

const phpExtensionOrdinaryRestartEnv = "POWERLINUX_PHP_EXTENSION_ORDINARY_RESTART"

func phpExtensionStartupFixture(t *testing.T) (*PrivilegedExecutor, Instance, phpExtensionIntent) {
	t.Helper()
	e := testAgent(t)
	id := "php-apt-startup-recovery"
	label := "startup-recovery"
	instance := phpExtensionRecoveryTestInstance(e, id, label, 7)
	if err := e.save("receipts", id, instance); err != nil {
		t.Fatal(err)
	}
	planID := "plan_" + strings.Repeat("e", 48)
	intent := phpExtensionRecoveryTestIntent(instance, planID, "install", "curl", "package_installed")
	state := phpExtensionState{Version: 1, InstanceID: id, References: map[string][]string{}, Pending: &intent}
	if err := e.savePHPExtensionState(state); err != nil {
		t.Fatal(err)
	}
	return e, instance, intent
}

func phpExtensionRecoveryTestInstance(e *PrivilegedExecutor, id, label string, revision int64) Instance {
	paths := e.instancePaths("php-apt", label)
	return Instance{
		ID: id, PluginID: "php-apt", InstanceID: label, SourceVersion: "8.3", Revision: revision,
		Status: "stopped", Ownership: "managed", Paths: paths, Config: map[string]any{}, Extensions: map[string]bool{},
		Service: ServiceProfile{
			ID: id, PluginID: "php-apt", InstanceID: label, Unit: "powerlinux-plugin-" + id + ".service",
			Program: "/usr/sbin/php-fpm8.3", ConfigPath: paths.ConfigDir, DataPath: paths.DataDir,
			Kind: "daemon", Ownership: "managed",
		},
	}
}

func phpExtensionRecoveryTestIntent(previous Instance, planID, operation, moduleID, phase string) phpExtensionIntent {
	now := time.Now().UTC()
	platform := Platform{OS: "linux", Arch: "amd64", Distro: "ubuntu", Release: "24.04"}
	packageName, _ := phpExtensionPackage("8.3", moduleID)
	plan := PHPModulePlan{
		PlanID: planID, ID: previous.ID, Operation: operation, ModuleID: moduleID,
		PHPVersion: "8.3.6", PHPModuleAPI: "20230831", Platform: platform,
		ExpectedRevision: previous.Revision, Resources: []string{InstanceResource(previous.ID), "host:packages"},
		CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(phpExtensionPlanTTL - time.Minute), PackageRemovable: false,
		AlreadyInstalled: false, AlreadyEnabled: previous.Extensions[moduleID],
	}
	if operation == "install" {
		plan.PackagePins = []PackagePin{{
			Name: packageName, Version: "8.3.6-0ubuntu0.24.04.2", Architecture: "amd64",
			Repository: "http://archive.ubuntu.com", SHA256: strings.Repeat("a", 64),
		}}
	}
	planRecord := phpExtensionPlanRecord{
		Public: plan, PackageName: packageName, ExtensionDirSHA256: strings.Repeat("b", 64),
		TargetPackageWasInstalled: false,
	}
	desired := previous
	desired.Extensions = maps.Clone(previous.Extensions)
	if desired.Extensions == nil {
		desired.Extensions = map[string]bool{}
	}
	desired.Extensions[moduleID] = operation == "install"
	if previous.Extensions[moduleID] != (operation == "install") {
		desired.Revision++
	}
	return phpExtensionIntent{
		PlanID: planID, Phase: phase, Plan: planRecord, Previous: previous, Desired: desired,
		StartedAt: now, UpdatedAt: now,
	}
}

func privatePHPStateDigest(t *testing.T, e *PrivilegedExecutor, id string) string {
	t.Helper()
	name := filepath.Join(e.config.StateDir, "private", phpExtensionStateKey(id)+".json")
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func TestPHPProfileMigrationSkipsOnlyValidatedPendingExtensionAndKeepsExactReplay(t *testing.T) {
	e, instance, intent := phpExtensionStartupFixture(t)
	stateBefore := privatePHPStateDigest(t, e, instance.ID)
	receiptBefore, err := os.ReadFile(filepath.Join(e.config.StateDir, "receipts", instance.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	changed, err := e.migratePHPProfile(context.Background(), instance)
	if err != nil || changed {
		t.Fatalf("validated pending extension state should be left for explicit replay: changed=%v err=%v", changed, err)
	}
	if got := privatePHPStateDigest(t, e, instance.ID); got != stateBefore {
		t.Fatal("startup migration rewrote the protected pending extension intent")
	}
	receiptAfter, err := os.ReadFile(filepath.Join(e.config.StateDir, "receipts", instance.ID+".json"))
	if err != nil || string(receiptAfter) != string(receiptBefore) {
		t.Fatal("startup migration changed the PHP receipt while a package transaction was pending", err)
	}
	if err := e.ensureInstallFinalized(context.Background(), instance.ID); core.ErrorCode(err) != "extension_recovery_required" {
		t.Fatalf("pending extension must continue blocking unrelated mutations: %v", err)
	}

	request := PHPInput{ID: instance.ID, Command: "extension_apply", PlanID: intent.PlanID, ExpectedRevision: instance.Revision, IdempotencyKey: "startup-replay-key-01"}
	replay, err := e.phpExtensionRecoveryContext(context.Background(), request)
	if err != nil {
		t.Fatal("exact replay context was not reconstructible after startup", err)
	}
	if err = e.ensureInstallFinalized(replay, instance.ID); err != nil {
		t.Fatal("exact current intent replay was not allowed after startup", err)
	}

	state, err := e.loadPHPExtensionState(instance.ID)
	if err != nil || state.Pending == nil {
		t.Fatal("startup fixture lost its pending extension transaction", err)
	}
	state.Pending.Phase = "receipt_commit_started"
	if err = e.savePHPExtensionState(state); err != nil {
		t.Fatal(err)
	}
	if err = e.ensureInstallFinalized(replay, instance.ID); core.ErrorCode(err) != "extension_recovery_required" {
		t.Fatalf("a stale replay marker bypassed the changed intent: %v", err)
	}
}

func TestPrivilegedStartupWithPendingPHPExtensionRemainsAvailable(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("constructor migration runs only in the root Agent")
	}
	e, instance, intent := phpExtensionStartupFixture(t)
	stateBefore := privatePHPStateDigest(t, e, instance.ID)
	receiptBefore, err := os.ReadFile(filepath.Join(e.config.StateDir, "receipts", instance.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewPrivilegedExecutor(e.config)
	if err != nil {
		t.Fatalf("Agent restart should leave a validated exact PHP extension intent inspectable: %v", err)
	}
	if got := privatePHPStateDigest(t, restarted, instance.ID); got != stateBefore {
		t.Fatal("root startup migration changed the pending extension intent")
	}
	receiptAfter, err := os.ReadFile(filepath.Join(e.config.StateDir, "receipts", instance.ID+".json"))
	if err != nil || string(receiptAfter) != string(receiptBefore) {
		t.Fatal("root startup migration changed the PHP receipt", err)
	}
	request := PHPInput{ID: instance.ID, Command: "extension_apply", PlanID: intent.PlanID, ExpectedRevision: instance.Revision, IdempotencyKey: "restart-exact-replay-01"}
	replay, err := restarted.phpExtensionRecoveryContext(context.Background(), request)
	if err != nil || restarted.ensureInstallFinalized(replay, instance.ID) != nil {
		t.Fatal("exact plan recovery was not available from the restarted Agent", err)
	}
}

func TestPHPProfileMigrationDoesNotSilentlyAcceptCorruptExtensionState(t *testing.T) {
	e, instance, _ := phpExtensionStartupFixture(t)
	state := phpExtensionState{Version: 99, InstanceID: instance.ID, References: map[string][]string{}}
	if err := e.save("private", phpExtensionStateKey(instance.ID), state); err != nil {
		t.Fatal(err)
	}
	before := privatePHPStateDigest(t, e, instance.ID)
	if changed, err := e.migratePHPProfile(context.Background(), instance); changed || core.ErrorCode(err) != "extension_state_invalid" {
		t.Fatalf("corrupt extension state must remain a startup error, changed=%v err=%v", changed, err)
	}
	if after := privatePHPStateDigest(t, e, instance.ID); after != before {
		t.Fatal("migration rewrote corrupt protected extension state")
	}
}

func TestPHPProfileMigrationOrdinaryAgentRestartLeavesPendingStateUntouched(t *testing.T) {
	if os.Getenv(phpExtensionOrdinaryRestartEnv) == "1" {
		var config PrivilegedConfig
		configPath := os.Getenv(phpExtensionOrdinaryRestartEnv + "_CONFIG")
		data, err := os.ReadFile(configPath)
		if err != nil || json.Unmarshal(data, &config) != nil {
			t.Fatalf("read ordinary Agent fixture config: %v", err)
		}
		e, err := NewPrivilegedExecutor(config)
		if err != nil {
			t.Fatalf("ordinary Agent startup failed: %v", err)
		}
		if err = e.ensureInstallFinalized(context.Background(), os.Getenv(phpExtensionOrdinaryRestartEnv+"_ID")); core.ErrorCode(err) != "extension_recovery_required" {
			t.Fatalf("ordinary Agent did not preserve the pending mutation gate: %v", err)
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("ordinary UID subprocess setup requires root")
	}
	worker, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("configured software worker account is unavailable")
	}
	uid, err := strconv.ParseUint(worker.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.ParseUint(worker.Gid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp("/tmp", "powerlinux-php-extension-ordinary-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	config := PrivilegedConfig{RootDir: filepath.Join(base, "software"), StateDir: filepath.Join(base, "private"), RunUser: worker.Username, APIUser: "root"}
	e, err := NewPrivilegedExecutor(config)
	if err != nil {
		t.Fatal(err)
	}
	instance := phpExtensionRecoveryTestInstance(e, "php-apt-ordinary-restart", "ordinary-restart", 2)
	if err = e.save("receipts", instance.ID, instance); err != nil {
		t.Fatal(err)
	}
	planID := "plan_" + strings.Repeat("f", 48)
	intent := phpExtensionRecoveryTestIntent(instance, planID, "install", "curl", "package_installed")
	state := phpExtensionState{Version: 1, InstanceID: instance.ID, References: map[string][]string{}, Pending: &intent}
	if err = e.savePHPExtensionState(state); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(base, "ordinary-agent-config.json")
	configData, _ := json.Marshal(e.config)
	if err = os.WriteFile(configPath, configData, 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binData, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(base, "plugins.test")
	if err = os.WriteFile(binPath, binData, 0700); err != nil {
		t.Fatal(err)
	}
	if err = filepath.Walk(base, func(name string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		mode := os.FileMode(0750)
		if info.Mode().IsRegular() {
			mode = info.Mode().Perm()
		}
		if err := os.Chown(name, int(uid), int(gid)); err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(name, mode)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(binPath, 0755); err != nil {
		t.Fatal(err)
	}
	before := privatePHPStateDigest(t, e, instance.ID)
	cmd := exec.Command(binPath, "-test.run=^TestPHPProfileMigrationOrdinaryAgentRestartLeavesPendingStateUntouched$")
	cmd.Dir = base
	cmd.Env = append(os.Environ(), phpExtensionOrdinaryRestartEnv+"=1", phpExtensionOrdinaryRestartEnv+"_CONFIG="+configPath, phpExtensionOrdinaryRestartEnv+"_ID="+instance.ID)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		t.Fatalf("ordinary Agent restart failed: %v: %s", runErr, output)
	}
	if after := privatePHPStateDigest(t, e, instance.ID); after != before {
		t.Fatal("ordinary Agent startup rewrote the protected extension recovery intent")
	}
}
