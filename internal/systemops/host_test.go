package systemops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

// These executable fixtures validate policy/transaction logic on real files
// and subprocesses. Actual systemd/SSH/nft host behavior requires the VM suite.
func fixtureHost(t *testing.T, protected bool) (*HostExecutor, string) {
	t.Helper()
	dir := t.TempDir()
	active := filepath.Join(dir, "active")
	enabled := filepath.Join(dir, "enabled")
	fail := filepath.Join(dir, "fail-start")
	_ = os.WriteFile(active, []byte("inactive"), 0600)
	_ = os.WriteFile(enabled, []byte("disabled"), 0600)
	script := fmt.Sprintf(`#!/bin/bash
set -eu
case "$1" in
show) printf 'ActiveState=%%s\nUnitFileState=%%s\nFragmentPath=/owned/powerlinux-plugin-test-default.service\n' "$(cat '%s')" "$(cat '%s')" ;;
start|restart|reload) printf active > '%s'; if test -e '%s'; then exit 17; fi ;;
stop) printf inactive > '%s' ;;
enable) printf enabled > '%s' ;;
disable) printf disabled > '%s' ;;
*) exit 29 ;;
esac
`, active, enabled, active, fail, active, enabled, enabled)
	program := filepath.Join(dir, "systemctl-fixture")
	if err := os.WriteFile(program, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	config := HostConfig{Version: 1, StateDir: filepath.Join(dir, "state"), Programs: map[string]string{"systemctl": program}, EnabledKinds: []string{"service_state"}, Services: []ServiceProfile{{ID: "test", Unit: "powerlinux-plugin-test-default.service", Ownership: "managed", Protected: protected}}, ConfirmationSeconds: 1}
	executor, err := NewHostExecutor(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { executor.Close() })
	return executor, dir
}
func hostCall(t *testing.T, executor *HostExecutor, operation string, request hostRequest) (HostPlan, error) {
	t.Helper()
	raw, _ := json.Marshal(request)
	result, err := executor.Run(context.Background(), agent.Request{Operation: operation, Input: raw})
	if err != nil {
		return HostPlan{}, err
	}
	var plan HostPlan
	if err = json.Unmarshal([]byte(result.Stdout), &plan); err != nil {
		t.Fatal(err)
	}
	return plan, nil
}
func TestHostServicePlanApplyAndRestore(t *testing.T) {
	executor, dir := fixtureHost(t, false)
	plan, err := hostCall(t, executor, "system_change_plan", hostRequest{ActorID: "actor", Change: &Change{Kind: "service_state", ServiceID: "test", ServiceState: "start"}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.State != "planned" {
		t.Fatal(plan.State)
	}
	plan, err = hostCall(t, executor, "system_change_apply", hostRequest{ActorID: "actor", ID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	if plan.State != "applied" {
		t.Fatal(plan.State)
	}
	active, _ := os.ReadFile(filepath.Join(dir, "active"))
	if string(active) != "active" {
		t.Fatal(string(active))
	}
	plan, err = hostCall(t, executor, "system_change_rollback", hostRequest{ActorID: "actor", ID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	active, _ = os.ReadFile(filepath.Join(dir, "active"))
	if string(active) != "inactive" || plan.State != "rolled_back" {
		t.Fatalf("not restored: %s %+v", active, plan)
	}
	info, _ := os.Stat(filepath.Join(executor.config.StateDir, plan.ID+".json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
}
func TestHostPlanRefusesStaleStateAndCrossActor(t *testing.T) {
	executor, dir := fixtureHost(t, false)
	plan, err := hostCall(t, executor, "system_change_plan", hostRequest{ActorID: "actor", Change: &Change{Kind: "service_state", ServiceID: "test", ServiceState: "start"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = hostCall(t, executor, "system_change_apply", hostRequest{ActorID: "other", ID: plan.ID}); !errors.Is(err, agent.ErrUnauthorized) {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "active"), []byte("active"), 0600)
	if _, err = hostCall(t, executor, "system_change_apply", hostRequest{ActorID: "actor", ID: plan.ID}); !errors.Is(err, ErrPlanConflict) {
		t.Fatal(err)
	}
}
func TestFailedHostMutationRestoresPriorState(t *testing.T) {
	executor, dir := fixtureHost(t, false)
	plan, err := hostCall(t, executor, "system_change_plan", hostRequest{ActorID: "actor", Change: &Change{Kind: "service_state", ServiceID: "test", ServiceState: "start"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "fail-start"), []byte("fail"), 0600)
	if _, err = hostCall(t, executor, "system_change_apply", hostRequest{ActorID: "actor", ID: plan.ID}); err == nil {
		t.Fatal("failed command reported success")
	}
	active, _ := os.ReadFile(filepath.Join(dir, "active"))
	if string(active) != "inactive" {
		t.Fatal("prior state not restored")
	}
	plan, err = hostCall(t, executor, "system_change_get", hostRequest{ActorID: "actor", ID: plan.ID})
	if err != nil || plan.State != "rolled_back" {
		t.Fatal(plan, err)
	}
}
func TestProtectedServiceAutomaticallyRestoresWithoutConfirmation(t *testing.T) {
	executor, dir := fixtureHost(t, true)
	plan, err := hostCall(t, executor, "system_change_plan", hostRequest{ActorID: "actor", Change: &Change{Kind: "service_state", ServiceID: "test", ServiceState: "start"}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err = hostCall(t, executor, "system_change_apply", hostRequest{ActorID: "actor", ID: plan.ID})
	if err != nil || plan.State != "awaiting_confirmation" {
		t.Fatal(plan, err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(filepath.Join(dir, "active"))
		if string(data) == "inactive" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("protected service was not restored")
}
func TestHostPolicyRejectsArbitraryCommandsAndForeignPrograms(t *testing.T) {
	executor, dir := fixtureHost(t, false)
	for _, change := range []Change{{Kind: "service_state", ServiceID: "ssh", ServiceState: "stop"}, {Kind: "service_state", ServiceID: "test", ServiceState: "start; touch /bad"}, {Kind: "service_state", ServiceID: "test", ServiceState: "start", Timezone: "UTC"}, {Kind: "power", Power: "reboot", DelaySeconds: 60}} {
		if err := executor.validateChange(change); err == nil {
			t.Fatal("unsafe change accepted", change)
		}
	}
	if err := executor.Validate(agent.Request{Operation: "systemops.program.systemctl", Input: json.RawMessage(`{"actor_id":"actor"}`)}); !errors.Is(err, agent.ErrUnauthorized) {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "world-writable")
	_ = os.WriteFile(program, []byte("#!/bin/sh\nexit 0\n"), 0777)
	_ = os.Chmod(program, 0777)
	if err := trustedProgram(program); err == nil {
		t.Fatal("untrusted program accepted")
	}
}
func TestHostValidationIsSideEffectFree(t *testing.T) {
	executor, dir := fixtureHost(t, false)
	config := executor.config
	config.StateDir = filepath.Join(dir, "not-created")
	if err := ValidateHostConfig(config); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(config.StateDir); !os.IsNotExist(err) {
		t.Fatal("validation created state")
	}
}
func TestFirewallCompilePreservesManagementAndRejectsInjection(t *testing.T) {
	executor, _ := fixtureHost(t, false)
	executor.kinds["firewall_rules"] = true
	good := Change{Kind: "firewall_rules", Firewall: &FirewallConfig{DefaultPolicy: "drop", Rules: []FirewallRule{{ID: "http", Protocol: "tcp", Port: 8080, Source: "192.0.2.0/24", Action: "accept"}}}}
	if err := executor.validateChange(good); err != nil {
		t.Fatal(err)
	}
	text := string(executor.firewallText(*good.Firewall, false))
	if !strings.Contains(text, "ct state established,related accept") || !strings.Contains(text, "tcp dport { 22, 80, 443, 9000 } accept") {
		t.Fatal(text)
	}
	for _, rule := range []FirewallRule{{ID: "bad", Protocol: "tcp", Port: 22, Action: "drop"}, {ID: "a\";delete", Protocol: "tcp", Port: 8080, Action: "accept"}, {ID: "source", Protocol: "tcp", Source: "0.0.0.0/0; flush ruleset", Action: "drop"}} {
		good.Firewall.Rules = []FirewallRule{rule}
		if err := executor.validateChange(good); err == nil {
			t.Fatal("unsafe rule accepted", rule)
		}
	}
}

func TestPendingProtectionExcludesLaterMutation(t *testing.T) {
	e, _ := fixtureHost(t, true)
	first, err := hostCall(t, e, "system_change_plan", hostRequest{ActorID: "actor", Change: &Change{Kind: "service_state", ServiceID: "test", ServiceState: "start"}})
	if err != nil {
		t.Fatal(err)
	}
	first, err = hostCall(t, e, "system_change_apply", hostRequest{ActorID: "actor", ID: first.ID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := hostCall(t, e, "system_change_plan", hostRequest{ActorID: "actor", Change: &Change{Kind: "service_state", ServiceID: "test", ServiceState: "stop"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = hostCall(t, e, "system_change_apply", hostRequest{ActorID: "actor", ID: second.ID}); !errors.Is(err, ErrConfirmation) {
		t.Fatalf("unconfirmed change was overwritten: %v", err)
	}
	if _, err = hostCall(t, e, "system_change_confirm", hostRequest{ActorID: "actor", ID: first.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err = hostCall(t, e, "system_change_apply", hostRequest{ActorID: "actor", ID: second.ID}); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackRefusesNewerExternalState(t *testing.T) {
	e, dir := fixtureHost(t, false)
	plan, err := hostCall(t, e, "system_change_plan", hostRequest{ActorID: "actor", Change: &Change{Kind: "service_state", ServiceID: "test", ServiceState: "start"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = hostCall(t, e, "system_change_apply", hostRequest{ActorID: "actor", ID: plan.ID}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "enabled"), []byte("enabled"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = hostCall(t, e, "system_change_rollback", hostRequest{ActorID: "actor", ID: plan.ID}); !errors.Is(err, ErrPlanConflict) {
		t.Fatalf("newer state overwritten: %v", err)
	}
	active, _ := os.ReadFile(filepath.Join(dir, "active"))
	if string(active) != "active" {
		t.Fatal("rollback mutated host despite conflict")
	}
}

func TestAgentRestartRecoversExpiredUnconfirmedChange(t *testing.T) {
	e, dir := fixtureHost(t, true)
	plan, err := hostCall(t, e, "system_change_plan", hostRequest{ActorID: "actor", Change: &Change{Kind: "service_state", ServiceID: "test", ServiceState: "start"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = hostCall(t, e, "system_change_apply", hostRequest{ActorID: "actor", ID: plan.ID}); err != nil {
		t.Fatal(err)
	}
	e.Close()
	stored, err := e.load(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Second)
	stored.ConfirmationDeadline = &before
	if err = e.save(stored); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewHostExecutor(e.config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	active, _ := os.ReadFile(filepath.Join(dir, "active"))
	if string(active) != "inactive" {
		t.Fatal("expired host state was not recovered")
	}
}

func TestPrivatePlanRejectsSymlinkAndForgedIdentity(t *testing.T) {
	e, dir := fixtureHost(t, false)
	plan, err := hostCall(t, e, "system_change_plan", hostRequest{ActorID: "actor", Change: &Change{Kind: "service_state", ServiceID: "test", ServiceState: "start"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.config.StateDir, plan.ID+".json")
	value, _ := os.ReadFile(path)
	other := filepath.Join(dir, "other-plan")
	if err = os.WriteFile(other, value, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(other, path); err != nil {
		t.Fatal(err)
	}
	if _, err = e.load(plan.ID); err == nil {
		t.Fatal("symlink plan accepted")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	var stored storedPlan
	if err = json.Unmarshal(value, &stored); err != nil {
		t.Fatal(err)
	}
	stored.ID = "chg_other"
	value, _ = json.Marshal(stored)
	if err = os.WriteFile(path, value, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = e.load(plan.ID); err == nil {
		t.Fatal("mismatched plan identity accepted")
	}
}
