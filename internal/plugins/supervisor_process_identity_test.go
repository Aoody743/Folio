package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type supervisorProcessFixture struct {
	instance Instance
	paths    supervisorIdentityPaths
	process  *exec.Cmd
}

func startSupervisorProcessFixture(t *testing.T, argsForConfig func(string) []string, launchDifferentScript bool) supervisorProcessFixture {
	t.Helper()
	interpreter, err := filepath.EvalSymlinks(supervisorInterpreterPath)
	if err != nil {
		t.Skip("Python 3 interpreter is unavailable")
	}
	interpreterInfo, err := os.Stat(interpreter)
	if err != nil {
		t.Skip("Python 3 interpreter cannot be inspected")
	}
	interpreterOwner, ok := statOf(interpreterInfo)
	if !ok || interpreterOwner.Uid != 0 {
		t.Skip("trusted process identity fixtures require a root-owned Python interpreter")
	}
	if filepath.Dir(interpreter) != "/usr/bin" {
		t.Skip("host Python interpreter is outside the Debian trusted path")
	}
	root := t.TempDir()
	script := filepath.Join(root, "supervisord")
	if err := os.WriteFile(script, []byte("#!/usr/bin/python3\nimport time\ntime.sleep(60)\n"), 0755); err != nil {
		t.Fatal(err)
	}
	launchedScript := script
	if launchDifferentScript {
		launchedScript = filepath.Join(root, "other-python-service")
		if err := os.WriteFile(launchedScript, []byte("#!/usr/bin/python3\nimport time\ntime.sleep(60)\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	configDir := filepath.Join(root, "config")
	runDir := filepath.Join(root, "run")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runDir, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "supervisord.conf")
	process := exec.Command(launchedScript, argsForConfig(configPath)...)
	if err := process.Start(); err != nil {
		t.Skipf("host cannot launch a shebang Python fixture: %v", err)
	}
	t.Cleanup(func() {
		if process.Process != nil {
			_ = process.Process.Kill()
			_, _ = process.Process.Wait()
		}
	})
	time.Sleep(30 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(runDir, "server.pid"), []byte(strconv.Itoa(process.Process.Pid)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroupId(strconv.Itoa(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	id := "supervisor-identity-fixture"
	instance := Instance{
		ID:         id,
		PluginID:   "supervisor",
		InstanceID: "identity-fixture",
		Status:     "running",
		Ownership:  "managed",
		Packages: []PackagePin{{
			Name: "supervisor", Version: "4.2.5-3", Architecture: "amd64", Repository: "fixture-signed-repository", SHA256: strings.Repeat("a", 64),
		}},
		Paths: Paths{Root: root, ConfigDir: configDir, RunDir: runDir},
		Service: ServiceProfile{
			ID: id, PluginID: "supervisor", InstanceID: "identity-fixture", Unit: "powerlinux-plugin-" + id + ".service",
			Program: script, ConfigPath: configDir, Kind: "daemon", Ownership: "managed",
		},
	}
	unitBody := []byte("[Unit]\nDescription=fixture\n[Service]\nType=simple\nUser=" + current.Username + "\nGroup=" + group.Name + "\n" +
		"ExecStart=" + unitQuote(script) + " " + unitQuote("-c") + " " + unitQuote(configPath) + " " + unitQuote("-n") + "\n" +
		"Restart=on-failure\n[Install]\nWantedBy=multi-user.target\n")
	unitRoot := filepath.Join(root, "units")
	if err := os.MkdirAll(unitRoot, 0700); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(unitRoot, instance.Service.Unit)
	if err := os.WriteFile(unitPath, unitBody, 0640); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(unitBody)
	instance.Service.UnitSHA256 = hex.EncodeToString(digest[:])
	return supervisorProcessFixture{
		instance: instance,
		process:  process,
		paths: supervisorIdentityPaths{
			procRoot: "/proc", unitRoot: unitRoot, interpreter: supervisorInterpreterPath, script: script,
			trustedFileUID: uint32(os.Geteuid()), requireUnitCgroup: false,
		},
	}
}

func TestSupervisorScriptIdentityUsesKernelShebangArgv(t *testing.T) {
	fixture := startSupervisorProcessFixture(t, func(config string) []string { return []string{"-c", config, "-n"} }, false)
	if err := verifySupervisorProcessIdentityAt(fixture.instance, fixture.process.Process.Pid, fixture.paths); err != nil {
		t.Fatalf("real shebang-launched Python process was rejected: %v", err)
	}
}

func TestSupervisorProcessIdentityRejectsWrongScriptArgumentsAndOwner(t *testing.T) {
	t.Run("wrong script", func(t *testing.T) {
		fixture := startSupervisorProcessFixture(t, func(config string) []string { return []string{"-c", config, "-n"} }, true)
		if err := verifySupervisorProcessIdentityAt(fixture.instance, fixture.process.Process.Pid, fixture.paths); err == nil {
			t.Fatal("accepted Python process running a different script")
		}
	})
	t.Run("wrong arguments", func(t *testing.T) {
		fixture := startSupervisorProcessFixture(t, func(string) []string { return []string{"-c", "/tmp/other-supervisor.conf", "-n"} }, false)
		if err := verifySupervisorProcessIdentityAt(fixture.instance, fixture.process.Process.Pid, fixture.paths); err == nil {
			t.Fatal("accepted Python process with a different configuration path")
		}
	})
	t.Run("wrong trusted file owner", func(t *testing.T) {
		fixture := startSupervisorProcessFixture(t, func(config string) []string { return []string{"-c", config, "-n"} }, false)
		fixture.paths.trustedFileUID = uint32(os.Geteuid()) + 1
		if err := verifySupervisorProcessIdentityAt(fixture.instance, fixture.process.Process.Pid, fixture.paths); err == nil {
			t.Fatal("accepted a script not owned by the configured trusted owner")
		}
	})
}

func TestSupervisorCgroupAndWorkerIdentityAreExact(t *testing.T) {
	unit := "powerlinux-plugin-supervisor-fixture.service"
	if !supervisorCgroupMatchesUnit([]byte("0::/system.slice/"+unit+"\n"), unit) {
		t.Fatal("did not recognize exact systemd unit in cgroup")
	}
	if supervisorCgroupMatchesUnit([]byte("0::/system.slice/foreign-"+unit+"\n"), unit) {
		t.Fatal("accepted a cgroup with only a unit-name suffix")
	}
	status := []byte("Uid:\t997\t997\t997\t997\nGid:\t990\t990\t990\t990\n")
	if !supervisorProcessCredentialsMatch(status, 997, 990) {
		t.Fatal("did not accept the configured worker identity")
	}
	if supervisorProcessCredentialsMatch(status, 0, 0) {
		t.Fatal("accepted a root process in place of the configured worker")
	}
}

func ExampleexactSupervisorArgv() {
	cmdline := []byte("/usr/bin/python3\x00/usr/bin/supervisord\x00-c\x00/opt/supervisor/config/supervisord.conf\x00-n\x00")
	fmt.Println(exactSupervisorArgv(cmdline, "/usr/bin/python3", "/usr/bin/supervisord", "/opt/supervisor/config/supervisord.conf"))
	// Output: true
}
