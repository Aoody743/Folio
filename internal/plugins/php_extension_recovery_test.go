package plugins

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestPHPExtensionRecoveryInspectionReturnsOnlyExactReplayTuple(t *testing.T) {
	e, instance, intent := phpExtensionStartupFixture(t)
	input := PHPInput{ID: instance.ID, Command: "extension_recovery_inspect"}
	raw, _ := json.Marshal(input)
	if err := e.Validate(agent.Request{Operation: "php_action", Input: raw}); err != nil {
		t.Fatal("typed recovery inspection was rejected", err)
	}
	result, err := e.php(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	inspection, ok := result.(PHPExtensionRecoveryInspection)
	if !ok || inspection.ID != instance.ID || !inspection.Pending || inspection.PlanID != intent.PlanID ||
		inspection.Operation != "install" || inspection.ModuleID != "curl" || inspection.ExpectedRevision != instance.Revision ||
		inspection.Phase != "package_installed" || !inspection.ReplayAvailable {
		t.Fatalf("recovery inspection did not return the exact pending plan tuple: %+v", result)
	}
	encoded, err := json.Marshal(inspection)
	if err != nil {
		t.Fatal(err)
	}
	for _, privateField := range []string{"package_name", "extension_dir_sha256", "package_pins", "password", "secret", "resource_paths"} {
		if strings.Contains(string(encoded), privateField) {
			t.Fatalf("recovery inspection leaked private field %q: %s", privateField, encoded)
		}
	}

	state, err := e.loadPHPExtensionState(instance.ID)
	if err != nil || state.Pending == nil {
		t.Fatal("inspection changed or removed pending intent", err)
	}
	state.Pending = nil
	if err = e.savePHPExtensionState(state); err != nil {
		t.Fatal(err)
	}
	result, err = e.php(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	inspection = result.(PHPExtensionRecoveryInspection)
	if inspection.Pending || inspection.ReplayAvailable || inspection.PlanID != "" {
		t.Fatalf("settled extension state still advertised a replay: %+v", inspection)
	}
}

func TestPHPExtensionRecoveryInspectionRejectsExpandedOrCorruptInput(t *testing.T) {
	e, instance, _ := phpExtensionStartupFixture(t)
	input := PHPInput{ID: instance.ID, Command: "extension_recovery_inspect", Operation: "install"}
	raw, _ := json.Marshal(input)
	if err := e.Validate(agent.Request{Operation: "php_action", Input: raw}); core.ErrorCode(err) != "invalid_input" {
		t.Fatalf("inspection accepted extra mutation fields: %v", err)
	}

	state, err := e.loadPHPExtensionState(instance.ID)
	if err != nil || state.Pending == nil {
		t.Fatal(err)
	}
	state.Pending.Phase = "unexpected-phase"
	if err = e.save("private", phpExtensionStateKey(instance.ID), state); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(PHPInput{ID: instance.ID, Command: "extension_recovery_inspect"})
	if _, err = e.php(context.Background(), raw); core.ErrorCode(err) != "extension_state_invalid" {
		t.Fatalf("inspection trusted an unknown pending phase: %v", err)
	}
}

func TestPHPExtensionRecoveryCapabilityUsesOnlyExactPHPScopeResource(t *testing.T) {
	manager := &Manager{}
	var binding *core.Binding
	for index := range manager.phpExtensionBindings() {
		candidate := manager.phpExtensionBindings()[index]
		if candidate.Action.Name == "php_extension_recovery_inspect" {
			binding = &candidate
			break
		}
	}
	if binding == nil || binding.Action.Scope != "php:manage" || !binding.Action.ReadOnly || binding.Action.Task {
		t.Fatalf("recovery inspector capability is not a read-only PHP management action: %+v", binding)
	}
	resources, err := binding.Resources(context.Background(), core.Actor{ID: "actor"}, json.RawMessage(`{"id":"php-apt-startup-recovery"}`))
	if err != nil || len(resources) != 1 || resources[0] != InstanceResource("php-apt-startup-recovery") {
		t.Fatalf("recovery inspection unexpectedly widened its resource scope: %v %v", resources, err)
	}
}
