//go:build linux && integration

package terminal

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// QA creates and removes the single shared bad-group account, allowing this
// read-only identity test and SystemOps validation to use the same fixture.
func TestKernelTerminalNonRootPrimaryGroupZeroRejected(t *testing.T) {
	name := os.Getenv("POWERLINUX_QA_TERMINAL_BADGROUP_USER")
	apiUser := os.Getenv("POWERLINUX_QA_TERMINAL_API_USER")
	if os.Getenv("POWERLINUX_QA_TERMINAL_GUEST") != "1" || name == "" || apiUser == "" {
		t.Skip("requires the explicit disposable-QEMU primary-group fixture")
	}
	if os.Geteuid() != 0 {
		t.Fatal("production identity validation must run as the root Agent")
	}
	virtualization, err := exec.Command("/usr/bin/systemd-detect-virt", "--vm").Output()
	if err != nil || strings.TrimSpace(string(virtualization)) != "qemu" {
		t.Fatal("this identity fixture may only run in our disposable QEMU guest")
	}
	account, err := user.Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	if account.Uid == "0" || account.Gid != "0" || account.Username == apiUser {
		t.Fatal("actual fixture is not an independent non-root UID with primary group 0")
	}
	home := t.TempDir()
	for _, enableRoot := range []bool{false, true} {
		state := filepath.Join(home, map[bool]string{false: "default", true: "explicit-root"}[enableRoot], "state")
		config := Config{
			APIUser: apiUser, StateDir: state, EnableRoot: enableRoot,
			Profiles: []LocalProfile{{ID: "isolated", User: name, Shell: "/bin/bash", Home: home, WorkDir: home}},
		}
		if err := ValidateConfig(config); err == nil || !strings.Contains(err.Error(), "root group") {
			t.Fatal("pure production validation did not reject the actual GID 0 account", err)
		}
		manager, err := New(Options{Config: config})
		if err == nil {
			manager.Close()
			t.Fatal("production constructor allowed the actual GID 0 account")
		}
		if !strings.Contains(err.Error(), "root group") {
			t.Fatal("constructor rejected for the wrong reason", err)
		}
		if _, err := os.Stat(filepath.Dir(state)); !os.IsNotExist(err) {
			t.Fatal("rejected policy created runtime state", err)
		}
	}
	t.Logf("real OS account UID=%s GID=%s rejected by pure policy and constructor with root profiles both disabled and enabled", account.Uid, account.Gid)
}
