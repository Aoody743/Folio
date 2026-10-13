package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestDecodeBootStateUsesActualSystemctlExitCodes(t *testing.T) {
	systemctl, err := exec.LookPath("systemctl")
	if err != nil {
		t.Skip("systemctl is unavailable")
	}
	for _, test := range []struct {
		name          string
		wantEnabled   bool
		wantState     string
		wantRunErr    bool
		wantDecodeErr bool
	}{
		{name: "enabled", wantEnabled: true, wantState: "enabled"},
		{name: "disabled", wantState: "disabled", wantRunErr: true},
		{name: "static", wantState: "static"},
		{name: "masked", wantState: "masked", wantRunErr: true},
		{name: "missing", wantRunErr: true, wantDecodeErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, runErr := runSystemctlBootStateForTest(t, systemctl, test.name)
			var exitErr *exec.ExitError
			if test.wantRunErr && !errors.As(runErr, &exitErr) {
				t.Fatalf("expected a real exec.ExitError for systemctl state %q, got %T %v", test.name, runErr, runErr)
			}
			if !test.wantRunErr && runErr != nil {
				t.Fatalf("systemctl state %q unexpectedly failed: %v", test.name, runErr)
			}
			enabled, state, decodeErr := decodeBootState(result, runErr)
			if test.wantDecodeErr {
				if core.ErrorCode(decodeErr) != "autostart_check_failed" {
					t.Fatalf("systemctl state %q was not safely rejected: state=%q err=%v", test.name, state, decodeErr)
				}
				return
			}
			if decodeErr != nil || enabled != test.wantEnabled || state != test.wantState {
				t.Fatalf("systemctl state decode mismatch: enabled=%t state=%q err=%v", enabled, state, decodeErr)
			}
		})
	}
}

func TestDecodeBootStateRejectsNonExitErrorsAndMismatchedResults(t *testing.T) {
	systemctl, err := exec.LookPath("systemctl")
	if err != nil {
		t.Skip("systemctl is unavailable")
	}
	result, runErr := runSystemctlBootStateForTest(t, systemctl, "disabled")
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		t.Fatalf("disabled state did not produce a real exec.ExitError: %T %v", runErr, runErr)
	}
	if _, _, err := decodeBootState(result, nil); core.ErrorCode(err) != "autostart_check_failed" {
		t.Fatal("nonzero result without an exec error was accepted", err)
	}
	wrongCode := result
	wrongCode.ExitCode = 0
	if _, _, err := decodeBootState(wrongCode, runErr); core.ErrorCode(err) != "autostart_check_failed" {
		t.Fatal("mismatched exec error and result exit codes were accepted", err)
	}
	for _, test := range []struct {
		name   string
		result agent.Result
		err    error
	}{
		{name: "start", result: agent.Result{Operation: "plugin_boot_state", ExitCode: -1}, err: errors.New("start operation: executable unavailable")},
		{name: "transport", result: result, err: errors.New("runner transport failed")},
		{name: "canceled", result: agent.Result{Operation: "plugin_boot_state", ExitCode: 0, Stdout: "enabled", Canceled: true}, err: context.Canceled},
		{name: "timed_out", result: agent.Result{Operation: "plugin_boot_state", ExitCode: 0, Stdout: "enabled", TimedOut: true}, err: context.DeadlineExceeded},
		{name: "truncated", result: agent.Result{Operation: "plugin_boot_state", ExitCode: 0, Stdout: "enabled", Truncated: true}},
		{name: "unknown_state", result: agent.Result{Operation: "plugin_boot_state", ExitCode: 0, Stdout: "future-state"}},
		{name: "wrong_operation", result: agent.Result{Operation: "other", ExitCode: 0, Stdout: "enabled"}},
		{name: "unexpected_stderr", result: agent.Result{Operation: "plugin_boot_state", ExitCode: 0, Stdout: "enabled", Stderr: "warning"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := decodeBootState(test.result, test.err); core.ErrorCode(err) != "autostart_check_failed" {
				t.Fatalf("unsafe %s result was accepted: %v", test.name, err)
			}
		})
	}
}

func runSystemctlBootStateForTest(t *testing.T, systemctl, state string) (agent.Result, error) {
	t.Helper()
	root := t.TempDir()
	unitName := "fixture.service"
	unitDir := filepath.Join(root, "usr", "lib", "systemd", "system")
	if err := os.MkdirAll(unitDir, 0755); err != nil {
		t.Fatal(err)
	}
	if state != "missing" {
		body := "[Unit]\nDescription=Fixture\n[Service]\nExecStart=/bin/true\n"
		if state != "static" {
			body += "[Install]\nWantedBy=multi-user.target\n"
		}
		if err := os.WriteFile(filepath.Join(unitDir, unitName), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	switch state {
	case "enabled":
		wants := filepath.Join(root, "etc", "systemd", "system", "multi-user.target.wants")
		if err := os.MkdirAll(wants, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/usr/lib/systemd/system/"+unitName, filepath.Join(wants, unitName)); err != nil {
			t.Fatal(err)
		}
	case "masked":
		unitLinks := filepath.Join(root, "etc", "systemd", "system")
		if err := os.MkdirAll(unitLinks, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/dev/null", filepath.Join(unitLinks, unitName)); err != nil {
			t.Fatal(err)
		}
	}

	command := exec.Command(systemctl, "--root="+root, "is-enabled", unitName)
	var stderr strings.Builder
	command.Stderr = &stderr
	stdout, runErr := command.Output()
	result := agent.Result{Operation: "plugin_boot_state", ExitCode: 0, Stdout: string(stdout), Stderr: stderr.String()}
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			t.Fatalf("systemctl failed without an exec.ExitError: %T %v", runErr, runErr)
		}
		result.ExitCode = exitErr.ExitCode()
	}
	return result, runErr
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
