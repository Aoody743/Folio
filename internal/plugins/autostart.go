package plugins

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type autostartInput struct {
	ID               string `json:"id"`
	Enabled          *bool  `json:"enabled,omitempty"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
}

func (e *PrivilegedExecutor) validateAutostart(raw json.RawMessage) error {
	var in autostartInput
	if err := parse(raw, &in); err != nil {
		return err
	}
	if !validInstanceID(in.ID) || in.Enabled != nil && in.ExpectedRevision < 1 || in.Enabled == nil && in.ExpectedRevision != 0 {
		return failure("invalid_input", "A valid software identity and revision are required for boot changes", 400)
	}
	return nil
}

func (e *PrivilegedExecutor) bootState(ctx context.Context, instance Instance) (bool, string, error) {
	if !e.config.UseSystemd || instance.Status == "uninstalled" || instance.Service.Program == "" || instance.Service.Kind != "daemon" || !ownedUnit(instance) {
		return false, "", failure("autostart_unavailable", "Boot selection requires an installed owned systemd daemon", 412)
	}
	if err := checkOwnedUnit(instance); err != nil {
		return false, "", err
	}
	runner, err := agent.NewRunner(agent.Options{Operations: []agent.Operation{{Name: "plugin_boot_state", Program: "/usr/bin/systemctl", Timeout: 15 * time.Second,
		BuildArgs: func(json.RawMessage) ([]string, error) { return []string{"is-enabled", instance.Service.Unit}, nil }}}, MaxOutputBytes: 1024, MaxConcurrent: 1})
	if err != nil {
		return false, "", err
	}
	result, err := runner.Run(ctx, agent.Request{Operation: "plugin_boot_state", Input: json.RawMessage(`{}`)})
	if err != nil || result.Truncated {
		return false, "", failure("autostart_check_failed", "Owned service boot state could not be read", 502)
	}
	state := strings.TrimSpace(result.Stdout)
	switch state {
	case "enabled":
		if result.ExitCode != 0 {
			return false, "", failure("autostart_check_failed", "Owned service boot state could not be verified", 502)
		}
		return true, state, nil
	case "disabled", "masked", "static", "indirect", "enabled-runtime", "linked", "linked-runtime":
		return false, state, nil // Only persistent enabled counts as boot enabled.
	default:
		return false, "", failure("autostart_check_failed", "Owned service boot state is unknown", 502)
	}
}

func (e *PrivilegedExecutor) setBootState(ctx context.Context, instance *Instance, enabled bool) error {
	_, _, err := e.bootState(ctx, *instance)
	if err != nil {
		return err
	}
	command := "disable"
	if enabled {
		command = "enable"
	}
	if _, err := runFixed(ctx, "/usr/bin/systemctl", []string{command, instance.Service.Unit}, 20*time.Second, nil); err != nil {
		return err
	}
	actual, state, err := e.bootState(ctx, *instance)
	if err != nil || actual != enabled || !enabled && state != "disabled" {
		return failure("autostart_not_applied", "Owned service boot selection was not confirmed", 502)
	}
	value := enabled
	instance.Service.Autostart = &value
	return nil
}

func (e *PrivilegedExecutor) autostart(ctx context.Context, raw json.RawMessage) (any, error) {
	var in autostartInput
	if err := parse(raw, &in); err != nil {
		return nil, err
	}
	unlock, err := e.lockContext(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	instance, err := e.get(ctx, in.ID, false)
	if err != nil {
		return nil, err
	}
	enabled, state, err := e.bootState(ctx, instance)
	if err != nil {
		return nil, err
	}
	if in.Enabled == nil {
		return map[string]any{"id": instance.ID, "enabled": enabled, "state": state, "recorded": instance.Service.Autostart, "revision": instance.Revision}, nil
	}
	if instance.Revision != in.ExpectedRevision {
		return nil, failure("revision_conflict", "Software changed before boot selection", 412)
	}
	key := "boot_" + instance.ID
	var intent struct {
		State string `json:"state"`
	}
	if err := e.load("private", key, &intent); err == nil && intent.State == "prepared" {
		return nil, failure("autostart_recovery_required", "Interrupted boot selection requires protected recovery inspection", 409)
	} else if err != nil && core.ErrorCode(err) != "not_found" {
		return nil, err
	}
	if err := e.save("private", key, map[string]any{"state": "prepared", "instance_id": instance.ID, "revision": instance.Revision, "previous": enabled, "desired": *in.Enabled}); err != nil {
		return nil, err
	}
	if err := e.setBootState(ctx, &instance, *in.Enabled); err != nil {
		return nil, err
	}
	instance.Revision++
	instance.UpdatedAt = time.Now().UTC()
	if err := e.save("receipts", instance.ID, instance); err != nil {
		return nil, failure("autostart_recovery_required", "Boot selection changed but its receipt needs protected recovery inspection", 409)
	}
	if err := e.save("private", key, map[string]any{"state": "completed", "instance_id": instance.ID, "revision": instance.Revision}); err != nil {
		return nil, failure("autostart_recovery_required", "Boot selection finished but its durable completion needs inspection", 409)
	}
	return map[string]any{"id": instance.ID, "enabled": *in.Enabled, "state": map[bool]string{true: "enabled", false: "disabled"}[*in.Enabled], "revision": instance.Revision, "running_state": instance.Status}, nil
}

func (m *Manager) autostartBindings() []core.Binding {
	bindings := []core.Binding{}
	for _, set := range []bool{false, true} {
		name, description, scope := "software_autostart_get", "Read the actual persistent boot state of an owned software daemon", "software:read"
		properties := map[string]any{"id": idSchema()}
		required := []string{"id"}
		if set {
			name, description, scope = "software_autostart_set", "Enable or disable the exact owned daemon at boot without changing its current running state", "software:manage"
			properties["enabled"] = map[string]any{"type": "boolean"}
			properties["expected_revision"] = revisionSchema()
			required = append(required, "enabled", "expected_revision")
		}
		set := set
		binding := m.binding(name, description, scope, !set, obj(properties, required...), idResource, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			if !set {
				var output any
				if err := m.agentJSON(ctx, "plugin_autostart", raw, &output); err != nil {
					return core.ActionResult{}, err
				}
				return core.ActionResult{Data: output, Resources: []string{InstanceResource(extractInstanceID(raw))}}, nil
			}
			resources, err := idResource(ctx, actor, raw)
			if err != nil {
				return core.ActionResult{}, err
			}
			job, err := m.deps.Tasks.Submit(ctx, actor, "plugin_autostart", raw, resources)
			id, _ := job["id"].(string)
			return core.ActionResult{Data: job, Resources: resources, TaskID: id}, err
		})
		binding.Action.Task = set
		bindings = append(bindings, binding)
	}
	return bindings
}

func extractInstanceID(raw json.RawMessage) string {
	var in autostartInput
	_ = json.Unmarshal(raw, &in)
	return in.ID
}
