package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestPackageDiagnosticRunnerHelper(t *testing.T) {
	mode := os.Getenv("POWERLINUX_PACKAGE_DIAGNOSTIC_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "nonzero":
		fmt.Fprintln(os.Stdout, "private apt stdout")
		fmt.Fprintln(os.Stderr, "private apt stderr")
		os.Exit(23)
	case "signal":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		t.Fatal("SIGKILL returned")
	case "sleep":
		if marker := os.Getenv("POWERLINUX_PACKAGE_DIAGNOSTIC_MARKER"); marker != "" {
			if err := os.WriteFile(marker, []byte("ready"), 0600); err != nil {
				os.Exit(24)
			}
		}
		time.Sleep(30 * time.Second)
	}
	os.Exit(0)
}

func packageDiagnosticChild(t *testing.T, mode string, timeout time.Duration, marker string) (agent.Result, error) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	environment := []string{"POWERLINUX_PACKAGE_DIAGNOSTIC_HELPER=" + mode}
	if marker != "" {
		environment = append(environment, "POWERLINUX_PACKAGE_DIAGNOSTIC_MARKER="+marker)
	}
	return runFixedResult(context.Background(), binary, []string{"-test.run=^TestPackageDiagnosticRunnerHelper$"}, timeout, nil, environment)
}

func TestPackageInstallFailureClassifiesRunnerOutcomes(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-executable")
	result, err := runFixedResult(context.Background(), missing, nil, time.Second, nil, nil)
	if err == nil || packageInstallFailureClass(result, err) != "not_started" {
		t.Fatalf("start failure classification = %q, result=%+v, err=%v", packageInstallFailureClass(result, err), result, err)
	}

	missingInterpreter := filepath.Join(t.TempDir(), "missing-interpreter")
	if err := os.WriteFile(missingInterpreter, []byte("#!/definitely/missing/powerlinux-interpreter\n"), 0700); err != nil {
		t.Fatal(err)
	}
	result, err = runFixedResult(context.Background(), missingInterpreter, nil, time.Second, nil, nil)
	var processStartErr *os.PathError
	if err == nil || result.StartedAt.IsZero() || !errors.As(err, &processStartErr) || processStartErr.Op != "fork/exec" || packageInstallFailureClass(result, err) != "not_started" {
		t.Fatalf("cmd.Start failure was misclassified: result=%+v, err=%v", result, err)
	}
	encoded, marshalErr := json.Marshal(packageInstallFailure(result, err))
	if marshalErr != nil || strings.Contains(string(encoded), missingInterpreter) {
		t.Fatalf("start failure path escaped diagnostic error: %s (marshal err %v)", encoded, marshalErr)
	}
	startDetails := packageInstallFailure(result, err).(*core.Error).Details.(packageFailureDetails)
	if startDetails.ExitCode != nil || strings.Contains(string(encoded), "exit_code") {
		t.Fatalf("failed cmd.Start incorrectly reported a process exit code: %+v (%s)", startDetails, encoded)
	}

	runnerError := agent.Result{StartedAt: time.Now(), ExitCode: -1}
	if got := packageInstallFailureClass(runnerError, errors.New("capture failed")); got != "runner_error" {
		t.Fatalf("runner error classification = %q", got)
	}

	result, err = packageDiagnosticChild(t, "nonzero", time.Second, "")
	details := packageInstallFailure(result, err).(*core.Error).Details.(packageFailureDetails)
	if err == nil || details.FailureClass != "nonzero_exit" || details.ExitCode == nil || *details.ExitCode != 23 {
		t.Fatalf("nonzero classification = %+v, result=%+v, err=%v", details, result, err)
	}
	encoded, marshalErr = json.Marshal(packageInstallFailure(result, err))
	if marshalErr != nil || strings.Contains(string(encoded), "private apt") {
		t.Fatalf("raw command output escaped diagnostic error: %s (marshal err %v)", encoded, marshalErr)
	}

	result, err = packageDiagnosticChild(t, "signal", time.Second, "")
	if err == nil || packageInstallFailureClass(result, err) != "signaled" {
		t.Fatalf("signal classification = %q, result=%+v, err=%v", packageInstallFailureClass(result, err), result, err)
	}
	if details := packageInstallFailure(result, err).(*core.Error).Details.(packageFailureDetails); details.ExitCode != nil {
		t.Fatalf("signal exposed an untrusted exit code: %+v", details)
	}

	marker := filepath.Join(t.TempDir(), "started")
	ctx, cancel := context.WithCancel(context.Background())
	cancelDone := make(chan struct{})
	go func() {
		defer close(cancelDone)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, statErr := os.Stat(marker); statErr == nil {
				cancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	binary, binaryErr := os.Executable()
	if binaryErr != nil {
		t.Fatal(binaryErr)
	}
	result, err = runFixedResult(ctx, binary, []string{"-test.run=^TestPackageDiagnosticRunnerHelper$"}, 10*time.Second, nil, []string{
		"POWERLINUX_PACKAGE_DIAGNOSTIC_HELPER=sleep",
		"POWERLINUX_PACKAGE_DIAGNOSTIC_MARKER=" + marker,
	})
	<-cancelDone
	if !result.Canceled || !errors.Is(err, context.Canceled) || packageInstallFailureClass(result, err) != "canceled" {
		t.Fatalf("cancel classification = %q, result=%+v, err=%v", packageInstallFailureClass(result, err), result, err)
	}

	result, err = packageDiagnosticChild(t, "sleep", 50*time.Millisecond, "")
	if !result.TimedOut || !errors.Is(err, context.DeadlineExceeded) || packageInstallFailureClass(result, err) != "timeout" {
		t.Fatalf("timeout classification = %q, result=%+v, err=%v", packageInstallFailureClass(result, err), result, err)
	}
}

func TestPackageInstallFailureOnlyExposesFixedSafeDetails(t *testing.T) {
	result := agent.Result{
		ExitCode:  1,
		StartedAt: time.Now(),
		Stdout:    "token-from-stdout",
		Stderr:    "credential-from-stderr",
		Operation: "plugin_fixed_command",
	}
	problem := packageInstallFailure(result, errors.New("argv contains private-url"))
	encoded, err := json.Marshal(problem)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"token-from-stdout", "credential-from-stderr", "private-url", "plugin_fixed_command"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("sensitive or execution detail %q escaped: %s", secret, encoded)
		}
	}
	var wire struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details struct {
			Stage        string `json:"stage"`
			FailureClass string `json:"failure_class"`
			ExitCode     int    `json:"exit_code"`
		} `json:"details"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Code != "package_install_failed" || wire.Details.Stage != "apt_get_install" || wire.Details.FailureClass != "nonzero_exit" || wire.Details.ExitCode != 1 {
		t.Fatalf("unexpected safe diagnostic envelope: %+v", wire)
	}
}

func TestPackageInstallFailureOmitsUnknownExitCode(t *testing.T) {
	for _, code := range []int{-1, 256} {
		result := agent.Result{StartedAt: time.Now(), ExitCode: code}
		encoded, err := json.Marshal(packageInstallFailure(result, errors.New("runner error")))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "exit_code") {
			t.Fatalf("unknown exit code %d was exposed: %s", code, encoded)
		}
	}
}
