package plugins

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

func TestSystemdActiveStateRecognizesRealInactiveExitCode(t *testing.T) {
	exitThree := exec.Command("/bin/sh", "-c", "exit 3").Run()
	result := agent.Result{Operation: "plugin_active_state", Stdout: "inactive\n", ExitCode: 3}
	active, err := decodeSystemdActiveState(result, exitThree)
	if err != nil || active {
		t.Fatal("inactive service exit 3 was treated as an unknown failure", active, err)
	}
	for _, changed := range []agent.Result{
		{Operation: "plugin_active_state", Stdout: "inactive", ExitCode: 1},
		{Operation: "plugin_active_state", Stdout: "activating", ExitCode: 3},
		{Operation: "plugin_active_state", Stdout: "inactive", ExitCode: 3, Truncated: true},
		{Operation: "plugin_active_state", Stdout: "inactive", ExitCode: 3, Canceled: true},
		{Operation: "plugin_active_state", Stdout: "inactive", ExitCode: 3, Stderr: "manager error"},
	} {
		if _, err := decodeSystemdActiveState(changed, exitThree); err == nil {
			t.Fatal("unknown/incomplete active state was accepted", changed)
		}
	}
}

func TestSystemdIdentityRejectsExternalFragmentsOverridesAndStaleManager(t *testing.T) {
	path := "/etc/systemd/system/powerlinux-plugin-redis-qa.service"
	owned := "LoadState=loaded\nFragmentPath=" + path + "\nDropInPaths=\nNeedDaemonReload=no\n"
	if err := validateSystemdIdentity(owned, path, false); err != nil {
		t.Fatal("exact loaded unit was refused", err)
	}
	for _, changed := range []string{
		strings.Replace(owned, path, "/usr/lib/systemd/system/foreign.service", 1),
		strings.Replace(owned, "DropInPaths=", "DropInPaths=/run/systemd/system/foreign.d/override.conf", 1),
		strings.Replace(owned, "NeedDaemonReload=no", "NeedDaemonReload=yes", 1),
		strings.Replace(owned, "LoadState=loaded", "LoadState=masked", 1),
		strings.Replace(owned, "NeedDaemonReload=no\n", "", 1),
		strings.Replace(owned, "NeedDaemonReload=no", "NeedDaemonReload=unknown", 1),
		owned + "FragmentPath=" + path + "\n",
	} {
		if err := validateSystemdIdentity(changed, path, false); err == nil {
			t.Fatal("unproven effective unit was accepted", changed)
		}
	}
	if err := validateSystemdIdentity(owned, path, true); err == nil {
		t.Fatal("new installation adopted a loaded service")
	}
	absent := "LoadState=not-found\nFragmentPath=\nDropInPaths=\nNeedDaemonReload=no\n"
	if err := validateSystemdIdentity(absent, path, true); err != nil {
		t.Fatal("absent new unit was refused", err)
	}
	if err := validateSystemdIdentity(absent, path, false); err == nil {
		t.Fatal("recovery accepted an absent service")
	}
}
