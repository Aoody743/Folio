package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

const lateHealthChildEnv = "POWERLINUX_INSTALL_LATE_HEALTH_CHILD"
const lateHealthAddressEnv = "POWERLINUX_INSTALL_LATE_HEALTH_ADDRESS"

func TestInstallLateHealthProtocolChild(t *testing.T) {
	if os.Getenv(lateHealthChildEnv) != "1" {
		return
	}
	listener, err := net.Listen("tcp", os.Getenv(lateHealthAddressEnv))
	if err != nil {
		t.Fatalf("listen for Redis health protocol: %v", err)
	}
	defer listener.Close()
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		request := make([]byte, 128)
		_, _ = connection.Read(request)
		if strings.Contains(string(request), "PING") {
			_, _ = io.WriteString(connection, "+PONG\r\n")
		}
		_ = connection.Close()
	}
}

func lateHealthRecoveryFixture(t *testing.T, plugin string) (*PrivilegedExecutor, installJournal) {
	t.Helper()
	e := testAgent(t)
	e.config.UseSystemd = true
	e.unitDir = t.TempDir()
	port := freeLoopbackPort(t)
	programName, health := "redis-server", "redis"
	if plugin == "mariadb" {
		programName, health = "mariadbd", "mysql"
	}
	manifest := Manifest{
		ID: plugin, Name: "Late health recovery fixture", IntegrationVersion: "1",
		Service:  ServiceDefinition{Programs: []string{programName}, Health: health, DefaultPort: port},
		Versions: []Version{{ID: "fixture-v1", Version: "1", Default: true, Source: Source{Kind: "verified_archive", URL: "https://fixture.invalid/runtime.tgz", SHA256: strings.Repeat("a", 64), ArchiveFormat: "tar.gz", Trust: "pinned_sha256"}}},
	}
	e.catalog = &Catalog{Items: []Manifest{manifest}, byID: map[string]int{plugin: 0}}
	plan, err := e.plan(context.Background(), PlanInput{Operation: "install", PluginID: plugin, InstanceID: "latehealth", Config: map[string]any{"bind": "127.0.0.1", "port": port}})
	if err != nil {
		t.Fatal("create install plan", err)
	}
	if err = e.save("plans", plan.ID, plan); err != nil {
		t.Fatal("save trusted install plan", err)
	}
	paths := e.instancePaths(plugin, "latehealth")
	if err = os.MkdirAll(paths.ConfigDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(paths.RunDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(paths.ConfigDir, "fixture.conf"), []byte("protected late-health fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(paths.Root, "runtime", programName)
	if err = os.MkdirAll(filepath.Dir(program), 0750); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Link(executable, program); err != nil {
		source, openErr := os.Open(executable)
		if openErr != nil {
			t.Fatal(openErr)
		}
		destination, createErr := os.OpenFile(program, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
		if createErr != nil {
			source.Close()
			t.Fatal(createErr)
		}
		_, copyErr := io.Copy(destination, source)
		closeSourceErr := source.Close()
		closeDestinationErr := destination.Close()
		if copyErr != nil || closeSourceErr != nil || closeDestinationErr != nil {
			t.Fatalf("copy executable fixture: %v %v %v", copyErr, closeSourceErr, closeDestinationErr)
		}
		if err = os.Chmod(program, 0755); err != nil {
			t.Fatal(err)
		}
	}
	instance := Instance{
		ID: plan.InstanceID, PluginID: plugin, InstanceID: "latehealth", Version: plan.Version.Version, SourceVersion: plan.Version.ID,
		ArtifactSHA256: plan.Version.Source.SHA256, Revision: 1, Status: "configured", Paths: paths, Config: cloneConfig(plan.Config),
		Ownership: "managed", Packages: plan.Packages,
		Service: ServiceProfile{ID: plan.InstanceID, PluginID: plugin, InstanceID: "latehealth", Unit: "powerlinux-plugin-" + plan.InstanceID + ".service", Program: program, ConfigPath: paths.ConfigDir, DataPath: paths.DataDir, Ownership: "managed", Kind: "daemon"},
		Health:  Health{Status: "unhealthy", CheckedAt: time.Now().UTC(), Reason: "initial readiness window elapsed"},
	}
	desiredBoot := true
	journal, err := e.newInstallJournal(context.Background(), plan, ApplyInput{PlanID: plan.ID, IdempotencyKey: "late-health-fixture"}, instance, nil, &desiredBoot, 0)
	if err != nil {
		t.Fatal("create protected install journal", err)
	}
	journal.ConfigSHA256, err = hashManagedTree(paths.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	programProof, err := hashManagedExecutable(program)
	if err != nil {
		t.Fatal(err)
	}
	journal.ProgramProof = &programProof
	unit, err := systemdUnitBytes(e, instance, journal.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(e.systemdUnitDir(), instance.Service.Unit)
	if err = os.WriteFile(unitPath, unit, 0644); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		if err = os.Chown(unitPath, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	journal.UnitSHA256 = hashBytes(unit)
	journal.Instance.Service.UnitSHA256 = journal.UnitSHA256
	journal.FailureCode = "service_start_failed"
	if err = e.saveNextInstallJournal(context.Background(), &journal, "health_failed"); err != nil {
		t.Fatal("save health-failed checkpoint", err)
	}
	return e, journal
}

func hashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func startLateHealthProcess(t *testing.T, instance Instance) *exec.Cmd {
	t.Helper()
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(setting(instance.Config, "port", 0)))
	command := exec.Command(instance.Service.Program, "-test.run=^TestInstallLateHealthProtocolChild$")
	command.Env = append(os.Environ(), lateHealthChildEnv+"=1", lateHealthAddressEnv+"="+address)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal("start actual protocol fixture process", err)
	}
	pidPath := filepath.Join(instance.Paths.RunDir, "server.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(command.Process.Pid)+"\n"), 0600); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal("write owned process proof", err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, 25*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return command
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("protocol fixture process did not become ready")
	return command
}

func setSystemdMainPIDFixture(e *PrivilegedExecutor, instance Instance, state string, pidDelta int) {
	e.installSystemdMainPIDHook = func(_ context.Context, unit string) (agent.Result, error) {
		if unit != instance.Service.Unit {
			return agent.Result{}, nil
		}
		pid, err := ownedProcessID(instance)
		if err != nil {
			return agent.Result{}, err
		}
		return agent.Result{Operation: "plugin_active_main_pid", Stdout: "ActiveState=" + state + "\nMainPID=" + strconv.Itoa(pid+pidDelta) + "\n", ExitCode: 0}, nil
	}
}

func TestInstallHealthFailedExactRunningUnitCanContinueWithoutRestart(t *testing.T) {
	e, journal := lateHealthRecoveryFixture(t, "redis")
	startLateHealthProcess(t, journal.Instance)
	setSystemdMainPIDFixture(e, journal.Instance, "active", 0)
	e.installEnableHook = func(context.Context, *installJournal) error { return nil }
	view, err := e.installRecoveryInspect(context.Background(), journal.ID)
	if err != nil || !containsString(view.Available, "continue") || !containsString(view.Available, "stop_preserve") {
		t.Fatalf("healthy exact health-failed unit did not advertise both safe actions: view=%+v err=%v", view, err)
	}
	view, err = e.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: journal.ID, Command: "continue", ExpectedJournalRevision: journal.Revision})
	if err != nil || view.State != "completed" || !view.ReceiptPresent {
		t.Fatalf("health-only continuation did not complete the journal: view=%+v err=%v", view, err)
	}
	receipt, err := e.get(context.Background(), journal.ID, false)
	if err != nil || receipt.Status != "running" || receipt.Health.Status != "healthy" {
		t.Fatalf("continuation did not commit current protocol health: receipt=%+v err=%v", receipt, err)
	}
	stored, err := e.loadInstallJournal(journal.ID)
	if err != nil || stored.Phase != "completed" || stored.Revision <= journal.Revision {
		t.Fatalf("continuation did not advance through journal CAS: journal=%+v err=%v", stored, err)
	}
}

func TestInstallHealthFailedStoppedRuntimeCannotContinue(t *testing.T) {
	e, journal := lateHealthRecoveryFixture(t, "redis")
	command := startLateHealthProcess(t, journal.Instance)
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	view, err := e.installRecoveryInspect(context.Background(), journal.ID)
	if err != nil || containsString(view.Available, "continue") {
		t.Fatalf("stopped service advertised continue: view=%+v err=%v", view, err)
	}
	_, err = e.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: journal.ID, Command: "continue", ExpectedJournalRevision: journal.Revision})
	if core.ErrorCode(err) != "install_recovery_health_failed" {
		t.Fatalf("stopped service was restarted or accepted: %v", err)
	}
	if _, processErr := ownedProcessID(journal.Instance); core.ErrorCode(processErr) != "runtime_not_running" {
		t.Fatalf("denied continue started or adopted a stopped process: %v", processErr)
	}
	stored, loadErr := e.loadInstallJournal(journal.ID)
	if loadErr != nil || stored.Revision != journal.Revision || stored.Phase != "health_failed" {
		t.Fatalf("denied continuation changed preserved journal: journal=%+v err=%v", stored, loadErr)
	}
}

func TestInstallHealthFailedForeignUnitAndDropInCannotContinue(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*testing.T, *PrivilegedExecutor, installJournal)
	}{
		{name: "foreign unit", edit: func(t *testing.T, e *PrivilegedExecutor, journal installJournal) {
			path := filepath.Join(e.systemdUnitDir(), journal.Instance.Service.Unit)
			if err := os.WriteFile(path, []byte("[Service]\nExecStart=/bin/false\n"), 0644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "drop-in", edit: func(t *testing.T, e *PrivilegedExecutor, journal installJournal) {
			dir := filepath.Join(e.systemdUnitDir(), journal.Instance.Service.Unit+".d")
			if err := os.Mkdir(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "override.conf"), []byte("[Service]\nEnvironment=FOREIGN=1\n"), 0644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			e, journal := lateHealthRecoveryFixture(t, "redis")
			startLateHealthProcess(t, journal.Instance)
			test.edit(t, e, journal)
			view, err := e.installRecoveryInspect(context.Background(), journal.ID)
			if err != nil || containsString(view.Available, "continue") {
				t.Fatalf("unowned unit state advertised continue: view=%+v err=%v", view, err)
			}
			_, err = e.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: journal.ID, Command: "continue", ExpectedJournalRevision: journal.Revision})
			if core.ErrorCode(err) != "ownership_conflict" {
				t.Fatalf("recovery accepted a foreign unit state: %v", err)
			}
		})
	}
}

func TestInstallHealthFailedSystemdStateMustBeActiveAndMatchOwnedPID(t *testing.T) {
	for _, test := range []struct {
		name      string
		state     string
		pidDelta  int
		wantError string
	}{
		{name: "inactive unit", state: "inactive", wantError: "install_recovery_health_failed"},
		{name: "different main pid", state: "active", pidDelta: 1, wantError: "ownership_conflict"},
	} {
		t.Run(test.name, func(t *testing.T) {
			e, journal := lateHealthRecoveryFixture(t, "redis")
			startLateHealthProcess(t, journal.Instance)
			setSystemdMainPIDFixture(e, journal.Instance, test.state, test.pidDelta)
			view, err := e.installRecoveryInspect(context.Background(), journal.ID)
			if err != nil || containsString(view.Available, "continue") {
				t.Fatalf("unsafe systemd runtime state advertised continue: view=%+v err=%v", view, err)
			}
			_, err = e.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: journal.ID, Command: "continue", ExpectedJournalRevision: journal.Revision})
			if core.ErrorCode(err) != test.wantError {
				t.Fatalf("unsafe systemd runtime state was accepted: got=%v want=%s", err, test.wantError)
			}
			stored, loadErr := e.loadInstallJournal(journal.ID)
			if loadErr != nil || stored.Revision != journal.Revision || stored.Phase != "health_failed" {
				t.Fatalf("rejected systemd state mutated preserved journal: journal=%+v err=%v", stored, loadErr)
			}
		})
	}
}

func TestDecodeSystemdActiveMainPIDFailsClosed(t *testing.T) {
	good := agent.Result{Operation: "plugin_active_main_pid", Stdout: "ActiveState=active\nMainPID=42\n", ExitCode: 0}
	if err := decodeSystemdActiveMainPID(good, nil, 42); err != nil {
		t.Fatal("exact active systemd main process was refused", err)
	}
	for _, test := range []struct {
		name   string
		result agent.Result
		runErr error
	}{
		{name: "inactive", result: agent.Result{Operation: "plugin_active_main_pid", Stdout: "ActiveState=inactive\nMainPID=42\n", ExitCode: 0}},
		{name: "mismatch", result: agent.Result{Operation: "plugin_active_main_pid", Stdout: "ActiveState=active\nMainPID=43\n", ExitCode: 0}},
		{name: "missing field", result: agent.Result{Operation: "plugin_active_main_pid", Stdout: "ActiveState=active\n", ExitCode: 0}},
		{name: "duplicate field", result: agent.Result{Operation: "plugin_active_main_pid", Stdout: "ActiveState=active\nMainPID=42\nMainPID=42\n", ExitCode: 0}},
		{name: "nonpositive", result: agent.Result{Operation: "plugin_active_main_pid", Stdout: "ActiveState=active\nMainPID=0\n", ExitCode: 0}},
		{name: "overflow", result: agent.Result{Operation: "plugin_active_main_pid", Stdout: "ActiveState=active\nMainPID=999999999999999999999\n", ExitCode: 0}},
		{name: "unexpected field", result: agent.Result{Operation: "plugin_active_main_pid", Stdout: "ActiveState=active\nMainPID=42\nFragmentPath=/foreign/unit\n", ExitCode: 0}},
		{name: "truncated", result: agent.Result{Operation: "plugin_active_main_pid", Stdout: "ActiveState=active\nMainPID=42\n", ExitCode: 0, Truncated: true}},
		{name: "canceled", result: agent.Result{Operation: "plugin_active_main_pid", Stdout: "ActiveState=active\nMainPID=42\n", ExitCode: 0, Canceled: true}},
		{name: "timed out", result: agent.Result{Operation: "plugin_active_main_pid", Stdout: "ActiveState=active\nMainPID=42\n", ExitCode: 0, TimedOut: true}},
		{name: "stderr", result: agent.Result{Operation: "plugin_active_main_pid", Stdout: "ActiveState=active\nMainPID=42\n", Stderr: "warning", ExitCode: 0}},
		{name: "nonzero", result: agent.Result{Operation: "plugin_active_main_pid", Stdout: "ActiveState=active\nMainPID=42\n", ExitCode: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := decodeSystemdActiveMainPID(test.result, test.runErr, 42); err == nil {
				t.Fatal("unproven systemd main process was accepted")
			}
		})
	}
}

func TestInstallHealthFailedDatabaseRequiresInitializedMatchingPrivateCredential(t *testing.T) {
	for _, test := range []struct {
		name       string
		credential managedDatabaseCredential
	}{
		{name: "not initialized", credential: managedDatabaseCredential{Engine: "mariadb", Username: "root", Password: "private-secret", Initialized: false}},
		{name: "engine mismatch", credential: managedDatabaseCredential{Engine: "mysql", Username: "root", Password: "private-secret", Initialized: true}},
		{name: "data path mismatch", credential: managedDatabaseCredential{Engine: "mariadb", Username: "root", Password: "private-secret", DataDir: "/foreign/data", Initialized: true}},
		{name: "missing password", credential: managedDatabaseCredential{Engine: "mariadb", Username: "root", Initialized: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			e, journal := lateHealthRecoveryFixture(t, "mariadb")
			if test.credential.DataDir == "" {
				test.credential.DataDir = journal.Instance.Paths.DataDir
			}
			if err := e.save("private", databasePrivateID(journal.Instance), test.credential); err != nil {
				t.Fatal(err)
			}
			view, err := e.installRecoveryInspect(context.Background(), journal.ID)
			if err != nil || containsString(view.Available, "continue") {
				t.Fatalf("uninitialized/mismatched private credential advertised continue: view=%+v err=%v", view, err)
			}
			_, err = e.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: journal.ID, Command: "continue", ExpectedJournalRevision: journal.Revision})
			if core.ErrorCode(err) != "credential_unavailable" {
				t.Fatalf("recovery accepted an invalid private credential proof: %v", err)
			}
			stored, loadErr := e.loadInstallJournal(journal.ID)
			if loadErr != nil || stored.Revision != journal.Revision || stored.Phase != "health_failed" {
				t.Fatalf("credential rejection changed preserved journal: journal=%+v err=%v", stored, loadErr)
			}
		})
	}
}

func TestInstallHealthFailedRecoveryRevisionCASIsRechecked(t *testing.T) {
	e, journal := lateHealthRecoveryFixture(t, "redis")
	startLateHealthProcess(t, journal.Instance)
	setSystemdMainPIDFixture(e, journal.Instance, "active", 0)
	e.installEnableHook = func(context.Context, *installJournal) error { return nil }
	var phases []string
	e.installJournalSaveHook = func(_ context.Context, next installJournal, _ int64) error {
		phases = append(phases, next.Phase)
		return nil
	}
	_, err := e.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: journal.ID, Command: "continue", ExpectedJournalRevision: journal.Revision})
	want := []string{"health_verified", "boot_verified", "receipt_pending", "completed"}
	if err != nil || len(phases) != len(want) {
		t.Fatalf("recovery did not traverse durable phase CAS saves: phases=%v err=%v", phases, err)
	}
	for index := range want {
		if phases[index] != want[index] {
			t.Fatalf("recovery phase order = %v, want %v", phases, want)
		}
	}
	_, err = e.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: journal.ID, Command: "continue", ExpectedJournalRevision: journal.Revision})
	if core.ErrorCode(err) != "revision_conflict" {
		t.Fatalf("stale recovery revision was accepted: %v", err)
	}
}
