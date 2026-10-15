package plugins

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func supervisorProgramPathFixture(t *testing.T) (*PrivilegedExecutor, Instance) {
	t.Helper()
	e := testAgent(t)
	// Use a dedicated /tmp child so the worker can traverse its ancestors even
	// when tests run as root; keep only the fixture base searchable.
	base, err := os.MkdirTemp("", "powerlinux-supervisor-path-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err := os.Chmod(base, 0755); err != nil {
		t.Fatal(err)
	}
	e.config.RootDir = filepath.Join(base, "software")
	// Root startup establishes the software root's service group and traversal
	// mode. Re-run that real setup after changing the fixture root, rather than
	// testing a layout that the production constructor never creates.
	e, err = NewPrivilegedExecutor(e.config)
	if err != nil {
		t.Fatal(err)
	}
	instance := Instance{
		ID:         "supervisor-path-fixture",
		PluginID:   "supervisor",
		InstanceID: "path-fixture",
		Paths:      e.instancePaths("supervisor", "path-fixture"),
	}
	if err := e.workerDirs(instance); err != nil {
		t.Fatal(err)
	}
	var program string
	for _, candidate := range []string{"/usr/bin/env", "/bin/sh"} {
		canonical, err := validateTrustedProgramPath(candidate)
		if err == nil {
			program = canonical
			break
		}
	}
	if program == "" {
		t.Skip("no root-owned protected executable is available for the Supervisor config fixture")
	}
	e.config.Programs = map[string]TrustedProgram{"trusted": {Program: program}}
	if err := e.save("private", utilityStateID(instance), utilityRecord{Revision: 1, Items: map[string]utilityAccount{
		"phase2child": {Name: "phase2child", ProgramID: "trusted"},
	}}); err != nil {
		t.Fatal(err)
	}
	return e, instance
}

func TestSupervisorWriteRepairsProgramAncestorAndWorkerCanTraverse(t *testing.T) {
	e, instance := supervisorProgramPathFixture(t)
	programs := filepath.Join(instance.Paths.DataDir, "programs")
	if err := os.Mkdir(programs, 0750); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(programs, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(programs, 0700); err != nil {
		t.Fatal(err)
	}
	oldUmask := syscall.Umask(0077)
	defer syscall.Umask(oldUmask)
	if err := e.writeSupervisor(instance); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(programs, "phase2child")
	for path, want := range map[string]uint32{programs: e.uid, leaf: e.uid, instance.Paths.LogDir: e.uid} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || stat.Uid != want || stat.Gid != e.gid || stat.Mode&07777 != supervisorWorkerDirMode {
			t.Fatalf("worker directory %s has uid/gid/mode %v", path, info.Sys())
		}
	}
	config, err := os.ReadFile(filepath.Join(instance.Paths.ConfigDir, "supervisord.conf"))
	if err != nil || !strings.Contains(string(config), "directory="+leaf+"\n") {
		t.Fatalf("Supervisor config omitted the repaired working directory: err=%v", err)
	}
	touch, err := exec.LookPath("touch")
	if err != nil {
		t.Skip("touch is unavailable for the worker identity traversal check")
	}
	probe := filepath.Join(leaf, "worker-can-write")
	cmd := exec.Command(touch, probe)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: e.uid, Gid: e.gid, NoSetGroups: true}}
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		t.Fatalf("Supervisor worker could not traverse/write its program directory: %v (%s)", runErr, output)
	}
	info, err := os.Stat(probe)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != e.uid || stat.Gid != e.gid {
		t.Fatalf("worker probe owner is %d:%d, want %d:%d", stat.Uid, stat.Gid, e.uid, e.gid)
	}
}

func TestSupervisorProgramDirectoryRejectsSymlinksAndUnknownModes(t *testing.T) {
	e, instance := supervisorProgramPathFixture(t)
	data := instance.Paths.DataDir
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(data, "programs")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.prepareSupervisorProgramDirectory(instance, "phase2child"); err == nil {
		t.Fatal("Supervisor followed a symlink in the managed programs path")
	}
	if _, err := os.Lstat(filepath.Join(outside, "phase2child")); !os.IsNotExist(err) {
		t.Fatalf("symlink target was modified: %v", err)
	}

	if err := os.Remove(filepath.Join(data, "programs")); err != nil {
		t.Fatal(err)
	}
	programs := filepath.Join(data, "programs")
	if err := os.Mkdir(programs, 0770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(programs, 0770); err != nil {
		t.Fatal(err)
	}
	if _, err := e.prepareSupervisorProgramDirectory(instance, "phase2child"); err == nil {
		t.Fatal("Supervisor accepted an unrecognized writable programs directory")
	}
}

func TestSupervisorProgramsDirectoryRecognizesOnlyManagedRepairStates(t *testing.T) {
	cases := []struct {
		name                  string
		uid, gid, mode        uint32
		wantRepair, wantChown bool
	}{
		{name: "worker private", uid: 1000, gid: 990, mode: 0700, wantRepair: true},
		{name: "worker inherited managed mode", uid: 1000, gid: 990, mode: 0750, wantRepair: true},
		{name: "root private", uid: 0, gid: 0, mode: 0700, wantRepair: true, wantChown: true},
		{name: "root service group", uid: 0, gid: 990, mode: 0750, wantRepair: true, wantChown: true},
		{name: "writable", uid: 1000, gid: 990, mode: 0770},
		{name: "foreign owner", uid: 1001, gid: 990, mode: 0700},
		{name: "broad access", uid: 0, gid: 0, mode: 0755},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotRepair, gotChown := supervisorProgramsDirectoryState(tc.uid, tc.gid, tc.mode, 1000, 990)
			if gotRepair != tc.wantRepair || gotChown != tc.wantChown {
				t.Fatalf("state (%d:%d %04o) = (%v,%v), want (%v,%v)", tc.uid, tc.gid, tc.mode, gotRepair, gotChown, tc.wantRepair, tc.wantChown)
			}
		})
	}
}
