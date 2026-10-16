package plugins

import "testing"

func TestManagerBindingsRegisterPHPModuleActionsExactlyOnce(t *testing.T) {
	want := map[string]struct {
		readOnly bool
		task     bool
		scope    string
	}{
		"php_extension_plan":             {readOnly: true, scope: "php:manage"},
		"php_extension_apply":            {task: true, scope: "php:manage"},
		"php_extension_recovery_inspect": {readOnly: true, scope: "php:manage"},
	}
	counts := make(map[string]int, len(want))
	for _, binding := range (&Manager{}).Bindings() {
		expected, ok := want[binding.Action.Name]
		if !ok {
			continue
		}
		counts[binding.Action.Name]++
		if binding.Action.ReadOnly != expected.readOnly || binding.Action.Task != expected.task || binding.Action.Scope != expected.scope {
			t.Errorf("public binding %q has unexpected flags: read_only=%t task=%t scope=%q", binding.Action.Name, binding.Action.ReadOnly, binding.Action.Task, binding.Action.Scope)
		}
	}
	for name := range want {
		if counts[name] != 1 {
			t.Errorf("public Manager.Bindings() must register %q exactly once, got %d", name, counts[name])
		}
	}
}
