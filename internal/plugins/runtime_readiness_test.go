package plugins

import (
	"context"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestRuntimeReadinessBudgets(t *testing.T) {
	for _, test := range []struct {
		plugin string
		want   time.Duration
	}{
		{plugin: "mysql", want: 45 * time.Second},
		{plugin: "mysql-community", want: 45 * time.Second},
		{plugin: "mariadb", want: 45 * time.Second},
		{plugin: "postgresql", want: 45 * time.Second},
		{plugin: "mongodb", want: 60 * time.Second},
		{plugin: "redis", want: 8 * time.Second},
		{plugin: "openresty", want: 8 * time.Second},
	} {
		if got := runtimeReadinessBudget(Instance{PluginID: test.plugin}); got != test.want {
			t.Errorf("%s readiness budget = %s, want %s", test.plugin, got, test.want)
		}
	}
}

func TestWaitForRuntimeReadinessFindsLateHealth(t *testing.T) {
	started := time.Now()
	calls := 0
	health, err := waitForRuntimeReadiness(context.Background(), 250*time.Millisecond, 25*time.Millisecond, time.Millisecond, func(ctx context.Context) Health {
		calls++
		if calls < 3 {
			timer := time.NewTimer(3 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return Health{Status: "unhealthy"}
			case <-timer.C:
			}
			return Health{Status: "unhealthy"}
		}
		return Health{Status: "healthy"}
	})
	if err != nil || health.Status != "healthy" {
		t.Fatalf("late readiness was not returned: health=%+v err=%v", health, err)
	}
	if calls != 3 || time.Since(started) < 5*time.Millisecond || time.Since(started) >= 100*time.Millisecond {
		t.Fatalf("readiness did not poll promptly: calls=%d elapsed=%s", calls, time.Since(started))
	}
}

func TestWaitForRuntimeReadinessHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	health, err := waitForRuntimeReadiness(ctx, 100*time.Millisecond, 10*time.Millisecond, time.Millisecond, func(context.Context) Health {
		cancel()
		return Health{Status: "unhealthy"}
	})
	if err != context.Canceled {
		t.Fatalf("readiness ignored cancellation: health=%+v err=%v", health, err)
	}
}

func TestWaitForRuntimeReadinessBoundsProbeAndTimesOut(t *testing.T) {
	started := time.Now()
	var sawProbeDeadline bool
	health, err := waitForRuntimeReadiness(context.Background(), 18*time.Millisecond, 5*time.Millisecond, time.Millisecond, func(ctx context.Context) Health {
		deadline, ok := ctx.Deadline()
		sawProbeDeadline = sawProbeDeadline || ok && time.Until(deadline) <= 5*time.Millisecond
		<-ctx.Done()
		return Health{Status: "unhealthy"}
	})
	if core.ErrorCode(err) != "health_check_failed" {
		t.Fatalf("readiness did not report a bounded timeout: health=%+v err=%v", health, err)
	}
	if !sawProbeDeadline {
		t.Fatal("probe did not receive an explicit bounded context")
	}
	if elapsed := time.Since(started); elapsed >= 100*time.Millisecond {
		t.Fatalf("bounded readiness timeout took too long: %s", elapsed)
	}
}
