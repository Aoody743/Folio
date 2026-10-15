package plugins

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type packageFailureDetails struct {
	Stage        string `json:"stage"`
	FailureClass string `json:"failure_class"`
	ExitCode     *int   `json:"exit_code,omitempty"`
}

func packageInstallFailure(result agent.Result, err error) error {
	failureClass := packageInstallFailureClass(result, err)
	details := packageFailureDetails{Stage: "apt_get_install", FailureClass: failureClass}
	if failureClass == "nonzero_exit" && result.ExitCode > 0 && result.ExitCode <= 255 {
		exitCode := result.ExitCode
		details.ExitCode = &exitCode
	}
	return &core.Error{
		Code:    "package_install_failed",
		Message: "Exact signed package installation failed; existing application data was preserved",
		Status:  502,
		Details: details,
	}
}

func packageInstallFailureClass(result agent.Result, err error) string {
	if result.TimedOut || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if result.Canceled || errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var startErr *exec.Error
	if errors.As(err, &startErr) || result.StartedAt.IsZero() {
		return "not_started"
	}
	var processStartErr *os.PathError
	if errors.As(err, &processStartErr) && processStartErr.Op == "fork/exec" {
		return "not_started"
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return "signaled"
		}
	}
	if result.ExitCode > 0 && result.ExitCode <= 255 {
		return "nonzero_exit"
	}
	return "runner_error"
}
