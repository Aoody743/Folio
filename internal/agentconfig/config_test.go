package agentconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/systemops"
)

func fixturePolicy(t *testing.T) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	config := Config{Version: 1, AllowedUIDs: []uint32{uint32(os.Getuid())}, Roots: []agent.RootSpec{{ID: "fixture", Path: dir}}, Operations: []string{"file_hash"}}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "policy.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path, raw
}
func TestPolicyLoadPinsDigestAndRejectsMalformedInput(t *testing.T) {
	path, raw := fixturePolicy(t)
	config, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if config.SHA256 != hex.EncodeToString(digest[:]) || len(config.AllowedUIDs) != 1 {
		t.Fatal(config)
	}
	for _, name := range []string{"unknown", "trailing", "size", "writable"} {
		t.Run(name, func(t *testing.T) {
			value := append([]byte{}, raw...)
			mode := os.FileMode(0600)
			switch name {
			case "unknown":
				value = []byte(`{"version":1,"unknown":true}`)
			case "trailing":
				value = append(value, []byte(` {}`)...)
			case "size":
				value = []byte(strings.Repeat(" ", 1<<20+1))
			case "writable":
				mode = 0660
			}
			candidate := filepath.Join(filepath.Dir(path), name+".json")
			if err := os.WriteFile(candidate, value, mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(candidate, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(candidate); err == nil {
				t.Fatal("invalid policy accepted", name)
			}
		})
	}
	link := filepath.Join(filepath.Dir(path), "symlink.json")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = Load(link); err == nil {
		t.Fatal("policy symlink accepted")
	}
}
func TestPolicyRejectsForeignOwner(t *testing.T) {
	path := "/bin/bash"
	if os.Geteuid() == 0 {
		path, _ = fixturePolicy(t)
		if err := os.Chown(path, 65534, -1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Load(path); err == nil {
		t.Fatal("foreign owner accepted")
	} else if os.Geteuid() != 0 && !strings.Contains(err.Error(), "Agent service UID") {
		t.Fatalf("owner check was not reached: %v", err)
	}
}
func TestPureHostValidationCreatesNoStateOrProcess(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "not-created")
	marker := filepath.Join(dir, "executed")
	program := filepath.Join(dir, "trusted-systemctl")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf executed > "+marker+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	config := systemops.HostConfig{Version: 1, StateDir: state, EnabledKinds: []string{"service_state"}, Programs: map[string]string{"systemctl": program}, Services: []systemops.ServiceProfile{{ID: "panel", Unit: "powerlinux.service", Ownership: "adopted"}}}
	if err := systemops.ValidateHostConfig(config); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{state, marker} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("pure validator had side effects", path, err)
		}
	}
}
func TestV2PeerNamesAndRootGuard(t *testing.T) {
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Version: 2, AllowedUsers: []string{account.Username}, Files: &FileConfig{Version: 1, StateDir: filepath.Join(t.TempDir(), "uncreated"), Roots: []FileRoot{{ID: "fixture", Path: t.TempDir()}}}}
	err = Validate(&config)
	if os.Geteuid() != 0 {
		if err == nil || !strings.Contains(err.Error(), "root") {
			t.Fatal("ordinary UID accepted privileged module policy", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	wanted, _ := strconv.ParseUint(account.Uid, 10, 32)
	if len(config.AllowedUIDs) != 1 || config.AllowedUIDs[0] != uint32(wanted) {
		t.Fatal("peer user lookup failed", config.AllowedUIDs)
	}
	config.AllowedUIDs = nil
	config.AllowedUsers = []string{"powerlinux_missing_fixture_user_947"}
	if err = Validate(&config); err == nil {
		t.Fatal("missing peer accepted")
	}
}
