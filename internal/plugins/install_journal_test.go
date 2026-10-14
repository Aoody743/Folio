package plugins

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

const installJournalChildEnv = "POWERLINUX_INSTALL_JOURNAL_CHILD"

func installRecoveryFixture(t *testing.T, sourceKind string, daemon bool) (*PrivilegedExecutor, installJournal) {
	t.Helper()
	e := testAgent(t)
	manifest := Manifest{
		ID: "redis", Name: "Journal fixture", IntegrationVersion: "1",
		Service:  ServiceDefinition{Programs: []string{"fixture-daemon"}, Health: "tcp", DefaultPort: 18455},
		Versions: []Version{{ID: "1", Version: "1", Default: true, Source: Source{Kind: sourceKind, Trust: "sha256", SHA256: strings.Repeat("a", 64)}}},
	}
	e.catalog = &Catalog{Items: []Manifest{manifest}, byID: map[string]int{"redis": 0}}
	plan, err := e.plan(context.Background(), PlanInput{Operation: "install", PluginID: "redis", InstanceID: "journal"})
	if err != nil {
		t.Fatal("create fixture plan", err)
	}
	if err = e.save("plans", plan.ID, plan); err != nil {
		t.Fatal("save fixture plan", err)
	}
	label := "journal"
	paths := e.instancePaths("redis", label)
	if err = os.MkdirAll(paths.ConfigDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(paths.ConfigDir, "fixture.conf"), []byte("private-test-config-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	instance := Instance{
		ID: plan.InstanceID, PluginID: "redis", InstanceID: label, Version: "1", SourceVersion: "1",
		Revision: 1, Status: "running", Paths: paths, Config: cloneConfig(plan.Config), Ownership: "managed",
		Service: ServiceProfile{ID: plan.InstanceID, PluginID: "redis", InstanceID: label, Unit: "powerlinux-plugin-" + plan.InstanceID + ".service", ConfigPath: paths.ConfigDir, DataPath: paths.DataDir, Ownership: "managed", Kind: "integration"},
	}
	if daemon {
		instance.Service.Kind = "daemon"
		instance.Service.Program = filepath.Join(paths.Root, "runtime", "fixture-daemon")
		if err = os.MkdirAll(filepath.Dir(instance.Service.Program), 0750); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(instance.Service.Program, []byte("fixture executable bytes\n"), 0750); err != nil {
			t.Fatal(err)
		}
	}
	instance.Health = Health{Status: "healthy", CheckedAt: time.Now().UTC()}
	var desired *bool
	if daemon {
		value := true
		desired = &value
	}
	j, err := e.newInstallJournal(context.Background(), plan, ApplyInput{PlanID: plan.ID, IdempotencyKey: "fixture-install-key"}, instance, nil, desired, 0)
	if err != nil {
		t.Fatal("create protected journal", err)
	}
	configHash, err := hashManagedTree(paths.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	j.ConfigSHA256 = configHash
	if daemon {
		proof, err := hashManagedExecutable(instance.Service.Program)
		if err != nil {
			t.Fatal(err)
		}
		j.ProgramProof = &proof
	}
	if err = e.saveNextInstallJournal(context.Background(), &j, "health_verified"); err != nil {
		t.Fatal("save health checkpoint", err)
	}
	return e, j
}

func TestInstallJournalCrashChild(t *testing.T) {
	if os.Getenv(installJournalChildEnv) != "1" {
		return
	}
	root := os.Getenv("POWERLINUX_INSTALL_JOURNAL_ROOT")
	state := os.Getenv("POWERLINUX_INSTALL_JOURNAL_STATE")
	runUser := os.Getenv("POWERLINUX_INSTALL_JOURNAL_RUN_USER")
	apiUser := os.Getenv("POWERLINUX_INSTALL_JOURNAL_API_USER")
	e, err := NewPrivilegedExecutor(PrivilegedConfig{RootDir: root, StateDir: state, RunUser: runUser, APIUser: apiUser})
	if err != nil {
		t.Fatalf("open existing protected Agent state: %v", err)
	}
	manifest, err := e.catalog.Get("webssh")
	if err != nil {
		t.Fatal(err)
	}
	version, err := manifest.SelectVersion("")
	if err != nil {
		t.Fatal(err)
	}
	id := "webssh-crash"
	plan := Plan{ID: "plan_" + strings.Repeat("b", 48), InstanceID: id, PluginID: "webssh", Operation: "install", Version: version, State: "planned", Resources: []string{InstanceResource(id)}, Config: map[string]any{}}
	paths := e.instancePaths("webssh", "crash")
	instance := Instance{ID: id, PluginID: "webssh", InstanceID: "crash", Version: version.Version, SourceVersion: version.ID, Revision: 1, Status: "configured", Paths: paths, Config: map[string]any{}, Ownership: "managed", Service: ServiceProfile{ID: id, PluginID: "webssh", InstanceID: "crash", Unit: "powerlinux-plugin-" + id + ".service", ConfigPath: paths.ConfigDir, DataPath: paths.DataDir, Ownership: "managed", Kind: "integration"}}
	if _, err = e.newInstallJournal(context.Background(), plan, ApplyInput{PlanID: plan.ID, IdempotencyKey: "child-sigkill-key"}, instance, nil, nil, 0); err != nil {
		t.Fatal("persist crash checkpoint", err)
	}
	_, _ = os.Stdout.WriteString("journal-durable\n")
	select {}
}

func TestInstallJournalSurvivesChildSIGKILL(t *testing.T) {
	e := testAgent(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestInstallJournalCrashChild$")
	cmd.Env = append(os.Environ(), installJournalChildEnv+"=1", "POWERLINUX_INSTALL_JOURNAL_ROOT="+e.config.RootDir,
		"POWERLINUX_INSTALL_JOURNAL_STATE="+e.config.StateDir, "POWERLINUX_INSTALL_JOURNAL_RUN_USER="+e.config.RunUser,
		"POWERLINUX_INSTALL_JOURNAL_API_USER="+e.config.APIUser)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- strings.TrimSpace(line)
	}()
	select {
	case line := <-ready:
		if line != "journal-durable" {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("child did not confirm durable journal: %q", line)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("child did not persist its journal before timeout")
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err == nil {
		t.Fatal("SIGKILL child unexpectedly exited cleanly")
	}
	journal, err := e.loadInstallJournal("webssh-crash")
	if err != nil || journal.Phase != "prepared" || journal.Revision != 1 || !installNoncePattern.MatchString(journal.Nonce) {
		t.Fatal("protected pre-side-effect checkpoint did not survive child SIGKILL", journal.Phase, journal.Revision, err)
	}
	sealed, err := os.ReadFile(filepath.Join(e.config.StateDir, "private", installJournalKey("webssh-crash")+".json"))
	if err != nil || !bytes.HasPrefix(sealed, []byte(privateRecordPrefix)) || bytes.Contains(sealed, []byte("child-sigkill-key")) {
		t.Fatal("journal was not stored as protected private state", err)
	}
}

func TestInstallUnitCreateOnlyPreservesNameConflict(t *testing.T) {
	dir := t.TempDir()
	name := "powerlinux-plugin-fixture-journal.service"
	foreign := []byte("foreign unit body\n")
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, foreign, 0600); err != nil {
		t.Fatal(err)
	}
	journalNonce := strings.Repeat("c", 32)
	unit := []byte("[Unit]\nDescription=fixture\n# PowerLinux install journal nonce: " + journalNonce + "\n")
	if err := createOnlyUnitFile(dir, name, unit); core.ErrorCode(err) != "ownership_conflict" {
		t.Fatal("create-only unit write did not reject a pre-existing name", err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, foreign) {
		t.Fatal("existing external unit was changed", err)
	}
	newPath := filepath.Join(dir, "new.service")
	if err = createOnlyUnitFile(dir, filepath.Base(newPath), unit); err != nil {
		t.Fatal("new owned unit was not created", err)
	}
	actual, err = os.ReadFile(newPath)
	if err != nil || !bytes.Equal(actual, unit) {
		t.Fatal("created unit differs from the expected nonce-bound body", err)
	}
}

func TestInstallJournalStoresExpectedUnitProofBeforeCreateOnlyWrite(t *testing.T) {
	e, j := installRecoveryFixture(t, "verified_archive", true)
	e.config.UseSystemd = true
	e.unitDir = t.TempDir()
	e.systemdReloadHook = func(context.Context) error { return nil }
	if err := e.saveNextInstallJournal(context.Background(), &j, "configure_started"); err != nil {
		t.Fatal(err)
	}
	e.installUnitCreateHook = func(dir, name string, unit []byte) error {
		stored, err := e.loadInstallJournal(j.ID)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(unit)
		if stored.Phase != "unit_write_pending" || stored.UnitSHA256 != hex.EncodeToString(digest[:]) || !bytes.Contains(unit, []byte("# PowerLinux install journal nonce: "+stored.Nonce+"\n")) {
			t.Fatal("expected unit hash and nonce were not durable before file creation")
		}
		return createOnlyUnitFile(dir, name, unit)
	}
	if err := e.writeInstallUnit(context.Background(), &j.Instance, j.Nonce); err != nil {
		t.Fatal("create nonce-bound unit", err)
	}
	stored, err := e.loadInstallJournal(j.ID)
	if err != nil || stored.Phase != "unit_written" || stored.UnitSHA256 == "" || j.Instance.Service.UnitSHA256 != stored.UnitSHA256 {
		t.Fatal("unit write did not commit its expected hash checkpoint", stored.Phase, err)
	}
	if err = e.verifyJournalUnit(stored); err != nil {
		t.Fatal("created unit failed exact hash/nonce verification", err)
	}
}

func TestInstallRecoveryLateBootFailureLeavesHealthCheckpoint(t *testing.T) {
	archive := archiveFixture(t, []*tar.Header{{Name: "bin/redis-server", Mode: 0755, Size: 16, Typeflag: tar.TypeReg}})
	archiveHash := sha256.Sum256(archive)
	archiveSHA := hex.EncodeToString(archiveHash[:])
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write(archive) }))
	defer server.Close()
	e := testAgent(t)
	e.artifacts.client = server.Client()
	manifest := Manifest{
		ID: "redis", Name: "Journal boot fixture", IntegrationVersion: "1",
		Service:  ServiceDefinition{Programs: []string{"redis-server"}, Health: "tcp", DefaultPort: 18455},
		Versions: []Version{{ID: "fixture-v1", Version: "1", Default: true, Source: Source{Kind: "verified_archive", URL: server.URL, SHA256: archiveSHA, ArchiveFormat: "tar.gz", Trust: "pinned_sha256"}}},
	}
	e.catalog = &Catalog{Items: []Manifest{manifest}, byID: map[string]int{"redis": 0}}
	e.config.UseSystemd = true
	e.unitDir = t.TempDir()
	e.installUnitCreateHook = createOnlyUnitFile
	e.systemdReloadHook = func(context.Context) error { return nil }
	e.installControlHook = func(context.Context, ServiceInput, Instance) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	}
	e.installHealthHook = func(context.Context, Instance) Health { return Health{Status: "healthy", CheckedAt: time.Now().UTC()} }
	plan, err := e.plan(context.Background(), PlanInput{Operation: "install", PluginID: "redis", InstanceID: "lateboot"})
	if err != nil {
		t.Fatal("create isolated install plan", err)
	}
	bootFailure := true
	e.installEnableHook = func(context.Context, *installJournal) error {
		if bootFailure {
			return failure("autostart_check_failed", "injected late boot failure", 502)
		}
		return nil
	}
	input := ApplyInput{PlanID: plan.ID, ExpectedRevision: 0, IdempotencyKey: "late-boot-failure-key"}
	_, err = e.apply(context.Background(), input)
	if core.ErrorCode(err) != "autostart_check_failed" {
		t.Fatal("injected late boot failure did not fail install", err)
	}
	actual, err := e.loadInstallJournal(plan.InstanceID)
	if err != nil || actual.Phase != "health_verified" {
		t.Fatal("late boot failure lost the health-verified checkpoint", actual.Phase, actual.Revision, err)
	}
	if _, err = os.Stat(filepath.Join(e.config.StateDir, "receipts", plan.InstanceID+".json")); !os.IsNotExist(err) {
		t.Fatal("failed boot recovery published a false active receipt", err)
	}
	if _, err = e.apply(context.Background(), input); core.ErrorCode(err) != "operation_recovery_required" {
		t.Fatal("failed install task replayed package/runtime actions", err)
	}
	bootFailure = false
	e.installJournalSaveHook = func(_ context.Context, next installJournal, _ int64) error {
		if next.Phase == "completed" {
			return errors.New("injected final journal checkpoint failure")
		}
		return nil
	}
	_, err = e.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: plan.InstanceID, Command: "continue", ExpectedJournalRevision: actual.Revision})
	if err == nil {
		t.Fatal("injected final journal checkpoint failure was hidden")
	}
	actual, err = e.loadInstallJournal(plan.InstanceID)
	if err != nil || actual.Phase != "receipt_pending" {
		t.Fatal("receipt commit did not retain the last durable journal checkpoint", actual.Phase, actual.Revision, err)
	}
	view, err := e.installRecoveryInspect(context.Background(), plan.InstanceID)
	if err != nil || !view.ReceiptPresent || len(view.Available) != 1 || view.Available[0] != "continue" {
		t.Fatal("committed receipt exposed an unsafe stop-preserve action", view, err)
	}
	if _, err = e.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: plan.InstanceID, Command: "stop_preserve", ExpectedJournalRevision: actual.Revision}); core.ErrorCode(err) != "ownership_conflict" {
		t.Fatal("stop-preserve changed a service after its receipt was committed", err)
	}
	e.installJournalSaveHook = nil
	view, err = e.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: plan.InstanceID, Command: "continue", ExpectedJournalRevision: actual.Revision})
	if err != nil || view.State != "completed" || !view.ReceiptPresent {
		t.Fatal("health-verified checkpoint did not finish boot/receipt without replaying install phases", view, err)
	}
}

func TestInstallRecoveryReceiptSaveFailureRemainsRetryable(t *testing.T) {
	e := testAgent(t)
	plan, err := e.plan(context.Background(), PlanInput{Operation: "install", PluginID: "webssh", InstanceID: "receiptfail"})
	if err != nil {
		t.Fatal("create integration install plan", err)
	}
	e.receiptSaveHook = func(string, Instance) error { return errors.New("injected receipt persistence failure") }
	input := ApplyInput{PlanID: plan.ID, ExpectedRevision: 0, IdempotencyKey: "receipt-save-failure-key"}
	_, err = e.apply(context.Background(), input)
	if err == nil {
		t.Fatal("injected root receipt failure was hidden by install")
	}
	actual, err := e.loadInstallJournal(plan.InstanceID)
	if err != nil || actual.Phase != "receipt_pending" || actual.Revision < 1 {
		t.Fatal("receipt failure did not leave a durable receipt_pending checkpoint", actual.Phase, actual.Revision, err)
	}
	if _, err = e.get(context.Background(), plan.InstanceID, false); core.ErrorCode(err) != "not_found" {
		t.Fatal("failed receipt write was exposed as an active software receipt", err)
	}
	e.receiptSaveHook = nil
	view, err := e.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: plan.InstanceID, Command: "continue", ExpectedJournalRevision: actual.Revision})
	if err != nil || view.State != "completed" || !view.ReceiptPresent {
		t.Fatal("receipt_pending checkpoint did not commit through its explicit action", view, err)
	}
}

func TestInstallRecoveryJournalRevisionCASAndSecretFreeInspect(t *testing.T) {
	e, j := installRecoveryFixture(t, "integration", false)
	view, err := e.installRecoveryInspect(context.Background(), j.ID)
	if err != nil || view.Revision != j.Revision || !containsString(view.Available, "continue") {
		t.Fatal("typed inspect did not expose the safe current checkpoint and revision", view, err)
	}
	encoded, _ := json.Marshal(view)
	if bytes.Contains(encoded, []byte("private-test-config-value")) || bytes.Contains(encoded, []byte(j.Nonce)) || bytes.Contains(encoded, []byte(j.IdempotencySHA)) {
		t.Fatal("public recovery inspect leaked private config, nonce, or execution proof")
	}
	var successes, conflicts int
	var mutex sync.Mutex
	var wait sync.WaitGroup
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, actionErr := e.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: j.ID, Command: "continue", ExpectedJournalRevision: j.Revision})
			mutex.Lock()
			defer mutex.Unlock()
			if actionErr == nil {
				successes++
			} else if core.ErrorCode(actionErr) == "revision_conflict" {
				conflicts++
			} else {
				t.Errorf("unexpected concurrent recovery result: %v", actionErr)
			}
		}()
	}
	wait.Wait()
	if successes != 1 || conflicts != 1 {
		t.Fatal("journal revision CAS did not serialize concurrent recovery", successes, conflicts)
	}
	_, err = e.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: j.ID, Command: "continue", ExpectedJournalRevision: j.Revision})
	if core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("stale recovery revision was accepted", err)
	}
}

func TestInstallRecoveryUnknownAndLegacyCheckpointsFailClosed(t *testing.T) {
	e, j := installRecoveryFixture(t, "integration", false)
	j.Phase = "package_install_started"
	if err := e.saveNextInstallJournal(context.Background(), &j, j.Phase); err != nil {
		t.Fatal(err)
	}
	view, err := e.installRecoveryInspect(context.Background(), j.ID)
	if err != nil || len(view.Available) != 0 {
		t.Fatal("pre-health install checkpoint advertised an automatic recovery action", view, err)
	}
	_, err = e.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: j.ID, Command: "continue", ExpectedJournalRevision: j.Revision})
	if core.ErrorCode(err) != "recovery_checkpoint_unsupported" {
		t.Fatal("continue replayed an uncertain package phase", err)
	}
	legacy, err := e.installRecoveryInspect(context.Background(), "redis-legacy")
	if err != nil || legacy.State != "journal_missing" || len(legacy.Available) != 0 {
		t.Fatal("legacy/no-journal state was adopted", legacy, err)
	}
}

func TestInstallRecoveryInspectAndContinueRejectChangedConfiguration(t *testing.T) {
	e, j := installRecoveryFixture(t, "integration", false)
	path := filepath.Join(j.Instance.Paths.ConfigDir, "fixture.conf")
	changed := []byte("externally changed configuration\n")
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	view, err := e.installRecoveryInspect(context.Background(), j.ID)
	if err != nil || len(view.Available) != 0 {
		t.Fatal("inspect advertised recovery without current ownership proof", view, err)
	}
	_, err = e.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: j.ID, Command: "continue", ExpectedJournalRevision: j.Revision})
	if core.ErrorCode(err) != "ownership_conflict" {
		t.Fatal("continue accepted changed configuration", err)
	}
	if _, err = e.get(context.Background(), j.ID, false); core.ErrorCode(err) != "not_found" {
		t.Fatal("unproven candidate was committed", err)
	}
	stored, err := e.loadInstallJournal(j.ID)
	if err != nil || stored.Revision != j.Revision || stored.Phase != j.Phase {
		t.Fatal("rejected recovery changed the protected checkpoint", stored.Phase, err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, changed) {
		t.Fatal("recovery overwrote externally changed configuration", err)
	}
}

func TestInstallRecoveryCommittedReceiptOnlyFinalizesJournal(t *testing.T) {
	e, j := installRecoveryFixture(t, "verified_archive", true)
	if err := e.saveNextInstallJournal(context.Background(), &j, "receipt_pending"); err != nil {
		t.Fatal(err)
	}
	if err := e.saveReceipt(j.ID, j.Instance); err != nil {
		t.Fatal(err)
	}
	// Simulate a reboot/stop after receipt commit but before journal completion.
	// Finalization cannot enable the unit or run any program/package phase again.
	e.installHealthHook = func(context.Context, Instance) Health {
		t.Fatal("journal finalization re-probed a committed service")
		return Health{Status: "unhealthy"}
	}
	e.installEnableHook = func(context.Context, *installJournal) error {
		t.Fatal("journal finalization changed a committed service's boot state")
		return nil
	}
	e.installProofHook = func(context.Context, installJournal) error {
		t.Fatal("journal finalization treated a completed install as an interrupted mutation")
		return nil
	}
	view, err := e.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: j.ID, Command: "continue", ExpectedJournalRevision: j.Revision})
	if err != nil || view.State != "completed" || len(view.Available) != 0 {
		t.Fatal("committed receipt could not finalize without replay", view, err)
	}
}

func TestInstallRecoveryRevisionCASAcrossExecutors(t *testing.T) {
	e, j := installRecoveryFixture(t, "integration", false)
	other, err := NewPrivilegedExecutor(e.config)
	if err != nil {
		t.Fatal(err)
	}
	other.catalog = e.catalog
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, executor := range []*PrivilegedExecutor{e, other} {
		go func(executor *PrivilegedExecutor) {
			<-start
			_, err := executor.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: j.ID, Command: "continue", ExpectedJournalRevision: j.Revision})
			results <- err
		}(executor)
	}
	close(start)
	first, second := <-results, <-results
	if first == nil && core.ErrorCode(second) == "revision_conflict" || second == nil && core.ErrorCode(first) == "revision_conflict" {
		return
	}
	t.Fatal("independent Agent executors bypassed the journal lock/CAS", first, second)
}

func TestInstallRecoveryLockAlsoSerializesReceiptWriters(t *testing.T) {
	e := testAgent(t)
	ctx := context.Background()
	plan, err := e.plan(ctx, PlanInput{Operation: "install", PluginID: "webssh", InstanceID: "sharedlock"})
	if err != nil {
		t.Fatal(err)
	}
	instance, err := e.apply(ctx, ApplyInput{PlanID: plan.ID, IdempotencyKey: "shared-lock-install"})
	if err != nil {
		t.Fatal(err)
	}
	change, err := e.plan(ctx, PlanInput{Operation: "config", PluginID: "webssh", InstanceID: "sharedlock"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewPrivilegedExecutor(e.config)
	if err != nil {
		t.Fatal(err)
	}
	input := ApplyInput{PlanID: change.ID, ExpectedRevision: instance.Revision, IdempotencyKey: "shared-lock-config"}
	unlock, err := e.lockContext(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	_, err = other.apply(waitCtx, input)
	cancel()
	unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("config writer bypassed the cross-process recovery lock", err)
	}
	current, err := e.get(ctx, instance.ID, false)
	if err != nil || current.Revision != instance.Revision {
		t.Fatal("blocked config writer changed the receipt", current.Revision, err)
	}
	current, err = other.apply(ctx, input)
	if err != nil || current.Revision != instance.Revision+1 {
		t.Fatal("canceled lock waiter consumed the plan or kept the lock", current.Revision, err)
	}
	view, err := e.installRecoveryInspect(ctx, instance.ID)
	if err != nil || view.State != "completed" || len(view.Available) != 0 {
		t.Fatal("ordinary receipt evolution reopened a completed install", view, err)
	}
}

// This is a source-test integration with a real daemon and systemd, not an
// installed release or a signed-package installer acceptance. Its prerequisite
// binary must be prepared inside a disposable Linux VM, never on the host.
func TestQAInstallRecoveryActualSystemd(t *testing.T) {
	if os.Getenv("POWERLINUX_QA_OWNED_VM") != "1" || os.Geteuid() != 0 {
		t.Skip("requires an explicitly disposable root Linux VM")
	}
	marker, err := os.OpenFile("/run/powerlinux-qa-disposable", os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal("disposable VM provision marker is required", err)
	}
	info, statErr := marker.Stat()
	mark, readErr := io.ReadAll(io.LimitReader(marker, 128))
	marker.Close()
	if statErr != nil || readErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || strings.TrimSpace(string(mark)) != "powerlinux-local-qa-20261014" {
		t.Fatal("disposable VM provision marker is invalid")
	}
	if owner, ok := info.Sys().(*syscall.Stat_t); !ok || owner.Uid != 0 {
		t.Fatal("disposable VM provision marker must belong to root")
	}
	if _, err = os.Stat("/run/systemd/system"); err != nil {
		t.Fatal("disposable VM must run systemd", err)
	}
	program, err := os.ReadFile("/usr/bin/redis-server")
	if err != nil {
		t.Fatal("prepare a real Redis binary in the disposable VM", err)
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tarWriter := tar.NewWriter(gz)
	if err = tarWriter.WriteHeader(&tar.Header{Name: "bin/redis-server", Mode: 0755, Size: int64(len(program)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err = tarWriter.Write(program); err != nil {
		t.Fatal(err)
	}
	if err = tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err = gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive := compressed.Bytes()
	digest := sha256.Sum256(archive)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
	defer server.Close()
	for _, command := range []string{"continue", "stop_preserve"} {
		t.Run(command, func(t *testing.T) {
			e := testAgent(t)
			// PrivateTmp hides /tmp and /run is commonly mounted noexec. Keep the
			// actual runtime in a unique guest /opt prefix, as in production, while
			// preserving the Agent's separate 0700 private directory.
			base, err := os.MkdirTemp("/opt", "powerlinux-install-recovery-qa-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(base); err != nil {
					t.Error(err)
				}
			})
			if err = os.Chmod(base, 0711); err != nil {
				t.Fatal(err)
			}
			config := e.config
			config.RootDir = filepath.Join(base, "software")
			config.StateDir = filepath.Join(base, "private")
			e, err = NewPrivilegedExecutor(config)
			if err != nil {
				t.Fatal(err)
			}
			e.config.UseSystemd = true
			e.artifacts.client = server.Client()
			manifest := Manifest{ID: "redis", Name: "Actual systemd recovery QA", IntegrationVersion: "1",
				Service:  ServiceDefinition{Programs: []string{"redis-server"}, Health: "tcp"},
				Versions: []Version{{ID: "qa-native", Version: "qa-native", Default: true, Source: Source{Kind: "verified_archive", URL: server.URL, SHA256: hex.EncodeToString(digest[:]), ArchiveFormat: "tar.gz", Trust: "pinned_sha256"}}}}
			e.catalog = &Catalog{Items: []Manifest{manifest}, byID: map[string]int{"redis": 0}}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			listener.Close()
			nonce, err := newInstallNonce()
			if err != nil {
				t.Fatal(err)
			}
			label := "qa-" + nonce[:12]
			plan, err := e.plan(context.Background(), PlanInput{Operation: "install", PluginID: "redis", InstanceID: label, Config: map[string]any{"bind": "127.0.0.1", "port": port}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				journal, loadErr := e.loadInstallJournal(plan.InstanceID)
				if core.ErrorCode(loadErr) == "not_found" {
					return
				}
				if loadErr != nil {
					t.Error("QA cleanup cannot prove install state", loadErr)
					return
				}
				if _, unitErr := os.Lstat(filepath.Join(e.systemdUnitDir(), journal.Instance.Service.Unit)); os.IsNotExist(unitErr) {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				if verifyErr := e.verifyJournalUnit(journal); verifyErr != nil {
					t.Error("QA cleanup refused an unproven unit", verifyErr)
					return
				}
				if _, stopErr := runFixed(ctx, "/usr/bin/systemctl", []string{"disable", "--now", journal.Instance.Service.Unit}, 15*time.Second, nil); stopErr != nil {
					t.Error("QA owned service cleanup failed", stopErr)
					return
				}
				if removeErr := e.removeUnit(ctx, journal.Instance); removeErr != nil {
					t.Error("QA owned unit cleanup failed", removeErr)
				}
			})
			// Inject only the late failure; configuration/start/protocol health and
			// recovery execute production paths against the actual system manager.
			e.installEnableHook = func(context.Context, *installJournal) error {
				return failure("autostart_check_failed", "QA late boot interruption", 502)
			}
			input := ApplyInput{PlanID: plan.ID, IdempotencyKey: "qa-native-" + nonce}
			_, err = e.apply(context.Background(), input)
			journal, journalErr := e.loadInstallJournal(plan.InstanceID)
			if journalErr != nil {
				t.Fatal("install did not retain its journal", journalErr, err)
			}

			if core.ErrorCode(err) != "autostart_check_failed" || journal.Phase != "health_verified" || e.probe(context.Background(), journal.Instance).Status != "healthy" {
				check := exec.Command(journal.Instance.Service.Program, "--version")
				check.SysProcAttr = &syscall.SysProcAttr{Credential: e.credential(journal.Instance)}
				checkOutput, checkErr := check.CombinedOutput()
				t.Logf("actual isolated Redis check: %v, %s", checkErr, checkOutput)
				unitOutput, _ := exec.Command("/usr/bin/journalctl", "--unit", journal.Instance.Service.Unit, "--no-pager", "-n", "12").CombinedOutput()
				t.Logf("actual isolated unit diagnostics: %s", unitOutput)
				t.Fatal("real service did not reach the late boot failure checkpoint", journal.Phase, err)
			}
			if _, err = e.apply(context.Background(), input); core.ErrorCode(err) != "operation_recovery_required" {
				t.Fatal("interrupted installation replayed", err)
			}
			dataFile := filepath.Join(journal.Instance.Paths.DataDir, "qa-preserve")
			if err = os.WriteFile(dataFile, []byte("preserve real data "+strconv.Itoa(port)), 0600); err != nil {
				t.Fatal(err)
			}
			e.installEnableHook = nil
			if command == "continue" {
				// Exact base-file bytes must not authorize a unit with an external
				// systemd override, whether pending reload or already merged.
				dropDir := filepath.Join(e.systemdUnitDir(), journal.Instance.Service.Unit+".d")
				if err := os.Mkdir(dropDir, 0755); err != nil {
					t.Fatal(err)
				}
				dropFile := filepath.Join(dropDir, "qa-override.conf")
				if err := os.WriteFile(dropFile, []byte("[Service]\nEnvironment=POWERLINUX_QA_OVERRIDE=1\n"), 0644); err != nil {
					t.Fatal(err)
				}
				removeDrop := func() {
					if err := os.Remove(dropFile); err != nil && !os.IsNotExist(err) {
						t.Error(err)
					}
					if err := os.Remove(dropDir); err != nil && !os.IsNotExist(err) {
						t.Error(err)
					}
					if _, err := runFixed(context.Background(), "/usr/bin/systemctl", []string{"daemon-reload"}, 20*time.Second, nil); err != nil {
						t.Error(err)
					}
				}
				t.Cleanup(removeDrop)
				if _, err := runFixed(context.Background(), "/usr/bin/systemctl", []string{"daemon-reload"}, 20*time.Second, nil); err != nil {
					t.Fatal(err)
				}
				view, err := e.installRecoveryInspect(context.Background(), journal.ID)
				if err != nil || len(view.Available) != 0 {
					t.Fatal("external effective override advertised recovery", view, err)
				}
				if _, err := e.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: journal.ID, Command: "stop_preserve", ExpectedJournalRevision: journal.Revision}); core.ErrorCode(err) != "ownership_conflict" {
					t.Fatal("recovery acted on an overridden unit", err)
				}
				if active, err := systemdUnitActive(context.Background(), journal.Instance.Service.Unit); err != nil || !active {
					t.Fatal("denied recovery altered the running service", active, err)
				}
				removeDrop()
			}
			// Reopen Agent private state as after a process crash, retaining the
			// exact trusted fixture catalog but no fault hooks.
			restarted, err := NewPrivilegedExecutor(e.config)
			if err != nil {
				t.Fatal(err)
			}
			restarted.catalog = e.catalog
			view, err := restarted.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: journal.ID, Command: command, ExpectedJournalRevision: journal.Revision})
			if err != nil {
				t.Fatal("actual systemd recovery failed", command, err)
			}
			active, activeErr := systemdUnitActive(context.Background(), journal.Instance.Service.Unit)
			enabled, state, bootErr := restarted.journalBootState(context.Background(), journal)
			if activeErr != nil || bootErr != nil {
				t.Fatal(activeErr, bootErr)
			}
			if command == "continue" {
				if view.State != "completed" || !view.ReceiptPresent || !active || !enabled || state != "enabled" {
					t.Fatal("continue did not commit a healthy enabled real service", view, active, state)
				}
				unitPath := filepath.Join(e.systemdUnitDir(), journal.Instance.Service.Unit)
				heldPath := filepath.Join(base, "held-unit")
				if err := os.Rename(unitPath, heldPath); err != nil {
					t.Fatal(err)
				}
				restoreUnit := func() {
					if _, err := os.Stat(heldPath); err == nil {
						if err := os.Rename(heldPath, unitPath); err != nil {
							t.Error(err)
						}
					}
					if _, err := runFixed(context.Background(), "/usr/bin/systemctl", []string{"daemon-reload"}, 20*time.Second, nil); err != nil {
						t.Error(err)
					}
				}
				t.Cleanup(restoreUnit)
				if err := e.removeUnit(context.Background(), journal.Instance); core.ErrorCode(err) != "ownership_conflict" {
					t.Fatal("missing disk unit was treated as an inactive unloaded service", err)
				}
				if active, err := systemdUnitActive(context.Background(), journal.Instance.Service.Unit); err != nil || !active {
					t.Fatal("missing-unit refusal altered the service", active, err)
				}
				restoreUnit()
			} else if view.State != "preserved_stopped" || view.ReceiptPresent || active || enabled || state != "disabled" {
				t.Fatal("stop_preserve did not stop and disable the real service", view, active, state)
			}
			if actual, err := os.ReadFile(dataFile); err != nil || string(actual) != "preserve real data "+strconv.Itoa(port) {
				t.Fatal("recovery did not preserve data", err)
			}
			if err := restarted.verifyInstallJournalProofs(context.Background(), journal); err != nil {
				t.Fatal("recovery changed the exact config/runtime/unit", err)
			}
		})
	}
}

func TestInstallRecoveryBindingsAreTypedAndAuthorized(t *testing.T) {
	operations := Operations()
	if !containsString(operations, "plugin_recovery_inspect") || !containsString(operations, "plugin_recovery_action") {
		t.Fatal("recovery Agent operations are not allowlisted")
	}
	catalog, err := BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]core.Action{}
	for _, binding := range (&Manager{catalog: catalog}).Bindings() {
		byName[binding.Action.Name] = binding.Action
	}
	inspect, ok := byName["software_recovery_inspect"]
	if !ok || !inspect.ReadOnly || inspect.Scope != "software:read" {
		t.Fatal("recovery inspect is not a typed read action", inspect)
	}
	action, ok := byName["software_recovery"]
	if !ok || action.ReadOnly || !action.Task || action.Scope != "software:manage" {
		t.Fatal("recovery mutation is not a durable authorized task", action)
	}
}
