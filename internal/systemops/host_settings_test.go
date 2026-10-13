package systemops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNTPUnsupportedServiceFailsBeforeMutation(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "timedatectl-fixture")
	script := `#!/bin/bash
set -eu
base="${0%/*}"
case "$1" in
show)
  case "$2" in
  --property=CanNTP) if test -f "$base/supported"; then printf 'yes\n'; else printf 'no\n'; fi ;;
  --property=NTP) cat "$base/value" ;;
  *) exit 31 ;;
  esac ;;
set-ntp)
  touch "$base/mutation"
  if test "$2" = true; then printf yes > "$base/value"; else printf no > "$base/value"; fi ;;
*) exit 32 ;;
esac
`
	if err := os.WriteFile(program, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "value"), []byte("no"), 0600); err != nil {
		t.Fatal(err)
	}
	e, err := NewHostExecutor(HostConfig{Version: 1, StateDir: filepath.Join(dir, "private"), Programs: map[string]string{"timedatectl": program}, EnabledKinds: []string{"ntp"}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	enable := true
	request := hostRequest{ActorID: "actor", Change: &Change{Kind: "ntp", NTP: &enable}}
	if _, err = hostCall(t, e, "system_change_plan", request); err == nil || !strings.Contains(err.Error(), "time-synchronization service") {
		t.Fatal("unsupported NTP was not rejected before applying", err)
	}
	if err = os.WriteFile(filepath.Join(dir, "supported"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := hostCall(t, e, "system_change_plan", request)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(dir, "supported")); err != nil {
		t.Fatal(err)
	}
	if _, err = hostCall(t, e, "system_change_apply", hostRequest{ActorID: "actor", ID: plan.ID}); err == nil {
		t.Fatal("NTP support disappearing after planning bypassed preflight")
	}
	if _, err = os.Stat(filepath.Join(dir, "mutation")); !os.IsNotExist(err) {
		t.Fatal("preflight rejection still invoked mutation", err)
	}
	if e.pendingID != "" {
		t.Fatal("preflight rejection blocked subsequent host actions")
	}
	if err = os.WriteFile(filepath.Join(dir, "supported"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if plan, err = hostCall(t, e, "system_change_apply", hostRequest{ActorID: "actor", ID: plan.ID}); err != nil || plan.State != "applied" {
		t.Fatal(plan, err)
	}
	if plan, err = hostCall(t, e, "system_change_rollback", hostRequest{ActorID: "actor", ID: plan.ID}); err != nil || plan.State != "rolled_back" {
		t.Fatal(plan, err)
	}
	if value, err := os.ReadFile(filepath.Join(dir, "value")); err != nil || string(value) != "no" {
		t.Fatal("NTP prior state was not restored", string(value), err)
	}
}

func TestSSHVerificationRejectsExtraPortsAndIneffectiveAllowUsers(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "sshd-fixture")
	if err := os.WriteFile(program, []byte("#!/bin/bash\nset -eu\ncat \"${0%/*}/effective\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e, err := NewHostExecutor(HostConfig{Version: 1, StateDir: filepath.Join(dir, "private"), EnabledKinds: []string{"ssh_config"}, Programs: map[string]string{"sshd": program, "systemctl": program}, Services: []ServiceProfile{{ID: "ssh", Unit: "ssh.service", Ownership: "adopted"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	change := Change{Kind: "ssh_config", SSH: &SSHConfig{Ports: []int{22}, PermitRootLogin: "prohibit-password", AllowUsers: []string{"operator", "backup"}}}
	base := "port 22\npasswordauthentication no\npermitrootlogin without-password\n"
	for _, item := range []struct {
		name, effective string
		valid           bool
	}{
		{"exact", base + "allowusers backup operator\n", true},
		{"extra-port", base + "port 2200\nallowusers backup operator\n", false},
		{"missing-user", base + "allowusers backup\n", false},
		{"extra-user", base + "allowusers backup operator root\n", false},
	} {
		t.Run(item.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, "effective"), []byte(item.effective), 0600); err != nil {
				t.Fatal(err)
			}
			if err := e.verify(context.Background(), change); (err == nil) != item.valid {
				t.Fatal("SSH effect did not match the requested configuration", err)
			}
		})
	}
}
