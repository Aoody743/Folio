package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunnerProcess(t *testing.T) {
	if os.Getenv("POWERLINUX_RUNNER_HELPER") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "flood":
		for i := 0; i < 512; i++ {
			fmt.Fprint(os.Stdout, strings.Repeat("o", 16384))
			fmt.Fprint(os.Stderr, strings.Repeat("e", 16384))
		}
	case "fail":
		os.Exit(17)
	case "environment":
		fmt.Fprint(os.Stdout, os.Getenv("POWERLINUX_UNTRUSTED_SECRET"))
	case "copy":
		io.Copy(os.Stdout, os.Stdin)
	case "child":
		time.Sleep(30 * time.Second)
	case "descendant", "blocking":
		binary, _ := os.Executable()
		child := exec.Command(binary, "-test.run=^TestRunnerProcess$", "--", "child")
		child.Env = os.Environ()
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(18)
		}
		fmt.Fprintln(os.Stdout, child.Process.Pid)
		if mode == "blocking" {
			child.Wait()
		}
	default:
		os.Exit(19)
	}
	os.Exit(0)
}

func helperRunner(t *testing.T, timeout time.Duration) *Runner {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(Options{MaxOutputBytes: 4096, MaxConcurrent: 1, Operations: []Operation{{Name: "test.helper", Program: binary, Timeout: timeout, Environment: []string{"POWERLINUX_RUNNER_HELPER=1", "GORACE=atexit_sleep_ms=0"}, BuildArgs: func(input json.RawMessage) ([]string, error) {
		var mode string
		if err := json.Unmarshal(input, &mode); err != nil {
			return nil, err
		}
		switch mode {
		case "flood", "fail", "environment", "descendant", "blocking", "copy":
		default:
			return nil, errors.New("unknown helper mode")
		}
		return []string{"-test.run=^TestRunnerProcess$", "--", mode}, nil
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func helperRequest(mode string) Request {
	input, _ := json.Marshal(mode)
	return Request{Operation: "test.helper", Input: input}
}

func TestRunnerBoundsAndDrainsOutput(t *testing.T) {
	runner := helperRunner(t, 30*time.Second)
	request := helperRequest("flood")
	request.Output = make(chan Output, 1) // Intentionally unread throughout execution.
	result, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !result.Truncated || len(result.Stdout)+len(result.Stderr) != 4096 || result.DroppedEvents == 0 {
		t.Fatalf("unexpected capture: %+v", result)
	}
}

func TestRunnerRejectsUnregisteredAndBadInput(t *testing.T) {
	runner := helperRunner(t, 10*time.Second)
	_, err := runner.Run(context.Background(), Request{Operation: "sh", Input: json.RawMessage(`"flood"`)})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unauthorized operation accepted: %v", err)
	}
	_, err = runner.Run(context.Background(), helperRequest("flood; arbitrary shell"))
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid args accepted: %v", err)
	}
}

func TestRunnerReportsExitAndDoesNotInheritSecrets(t *testing.T) {
	runner := helperRunner(t, 10*time.Second)
	result, err := runner.Run(context.Background(), helperRequest("fail"))
	if err == nil || result.ExitCode != 17 {
		t.Fatalf("failed process shown successful: %+v %v", result, err)
	}
	t.Setenv("POWERLINUX_UNTRUSTED_SECRET", "must-not-be-inherited")
	result, err = runner.Run(context.Background(), helperRequest("environment"))
	if err != nil || result.Stdout != "" {
		t.Fatalf("environment leaked: %+v %v", result, err)
	}
}

func assertChildStopped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if errors.Is(err, os.ErrNotExist) || strings.Contains(string(status), "State:\tZ") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant %d still running", pid)
}

func TestRunnerReapsDescendantsAfterParentExit(t *testing.T) {
	result, err := helperRunner(t, 10*time.Second).Run(context.Background(), helperRequest("descendant"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(result.Stdout))
	if err != nil {
		t.Fatalf("missing child PID: %+v", result)
	}
	assertChildStopped(t, pid)
}

func TestRunnerCancellationAndDeadlineKillProcessGroup(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit-%v", canceled), func(t *testing.T) {
			timeout := 8 * time.Second
			if canceled {
				timeout = 20 * time.Second
			}
			runner := helperRunner(t, timeout)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			output := make(chan Output, 8)
			request := helperRequest("blocking")
			request.Output = output
			if canceled {
				go func() {
					select {
					case <-output:
						cancel()
					case <-time.After(10 * time.Second):
						cancel()
					}
				}()
			}
			result, err := runner.Run(ctx, request)
			if canceled && (!errors.Is(err, context.Canceled) || !result.Canceled) {
				t.Fatalf("bad cancel result: %+v %v", result, err)
			}
			if !canceled && (!errors.Is(err, context.DeadlineExceeded) || !result.TimedOut) {
				t.Fatalf("bad deadline result: %+v %v", result, err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(result.Stdout))
			if err != nil {
				t.Fatal(err)
			}
			assertChildStopped(t, pid)
		})
	}
}

func TestRunnerCanceledQueueDoesNotStartProcess(t *testing.T) {
	runner := helperRunner(t, 20*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := helperRequest("blocking")
	output := make(chan Output, 8)
	request.Output = output
	finished := make(chan struct{})
	go func() { defer close(finished); runner.Run(ctx, request) }()
	select {
	case <-output:
	case <-time.After(10 * time.Second):
		t.Fatal("first process did not start")
	}
	queued, cancelQueued := context.WithCancel(context.Background())
	cancelQueued()
	result, err := runner.Run(queued, helperRequest("fail"))
	if !errors.Is(err, context.Canceled) || !result.Canceled || !result.StartedAt.IsZero() {
		t.Fatalf("queued process was started: %+v %v", result, err)
	}
	cancel()
	<-finished
}
