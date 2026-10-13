package plugins

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestBootSelectionRejectsCallerSelectedUnitsAndMissingRevision(t *testing.T) {
	e := testAgent(t)
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"id":"openresty-own","enabled":true,"expected_revision":1,"unit":"ssh.service"}`),
		json.RawMessage(`{"id":"openresty-own","enabled":true,"expected_revision":1,"program":"/bin/sh"}`),
		json.RawMessage(`{"id":"openresty-own","enabled":true}`),
		json.RawMessage(`{"id":"../foreign","enabled":false,"expected_revision":1}`),
	} {
		if err := e.Validate(agent.Request{Operation: "plugin_autostart", Input: raw}); err == nil {
			t.Fatal("unowned system boot target or stale write descriptor accepted")
		}
	}
	instance := installIntegration(t, e, "boot-unavailable")
	if _, err := e.autostart(context.Background(), encodePlugin(autostartInput{ID: instance.ID})); core.ErrorCode(err) != "autostart_unavailable" {
		t.Fatal("integration without a real owned daemon fabricated boot availability", err)
	}
}

func encodePlugin(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func TestRuntimeRecoveryKeepsActualStoppedDespiteStaleRunningReceipt(t *testing.T) {
	e := testAgent(t)
	paths := e.instancePaths("openresty", "recovery")
	if err := os.MkdirAll(paths.RunDir, 0700); err != nil {
		t.Fatal(err)
	}
	// /bin/true is a controlled syntax-check fixture. It creates no daemon or
	// PID and cannot count as a native OpenResty/service acceptance result.
	instance := Instance{ID: "openresty-recovery", PluginID: "openresty", InstanceID: "recovery", Paths: paths,
		Ownership: "managed", Status: "running", Revision: 7, Config: defaultConfig("openresty"),
		Service: ServiceProfile{ID: "openresty-recovery", Program: "/bin/true", Kind: "daemon", Unit: "powerlinux-plugin-openresty-recovery.service", Ownership: "managed"}}
	if err := e.save("receipts", instance.ID, instance); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.WithRuntimeTransaction(ctx, []string{instance.ID}, func(held context.Context) error { return e.RecoverRuntime(held, instance.ID) }); err != nil {
		t.Fatal("stopped runtime recovery tried to start/reload or deadlocked", err)
	}
	if _, err := os.Stat(filepath.Join(paths.RunDir, "server.pid")); !os.IsNotExist(err) {
		t.Fatal("stopped recovery created a process marker")
	}
	current, err := e.get(ctx, instance.ID, false)
	if err != nil || current.Revision != 7 {
		t.Fatal("syntax-only stopped recovery published a new config revision", err)
	}
}
