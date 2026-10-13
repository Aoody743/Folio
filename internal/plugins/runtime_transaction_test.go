package plugins

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestRuntimeTransactionReentrantCheckAndClosedLockSet(t *testing.T) {
	e := testAgent(t)
	instance := installIntegration(t, e, "transaction")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.WithRuntimeTransaction(ctx, []string{instance.ID, instance.ID}, func(held context.Context) error {
		if err := e.Check(held, instance.ID); err != nil {
			return err
		}
		if err := e.WithRuntimeTransaction(held, []string{"webssh-unclaimed"}, func(context.Context) error { return nil }); core.ErrorCode(err) != "runtime_transaction_nested" {
			t.Fatal("nested callback expanded locks without canonical acquisition", err)
		}
		return e.WithRuntimeTransaction(held, []string{instance.ID}, func(nested context.Context) error { return e.Check(nested, instance.ID) })
	}); err != nil {
		t.Fatal("trusted runtime Check deadlocked or failed inside transaction", err)
	}
}

func TestRuntimeTransactionCancelledWaitReleasesEarlierLocks(t *testing.T) {
	e := testAgent(t)
	releaseLast := e.lock("webssh-z")
	defer releaseLast()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	called := false
	err := e.WithRuntimeTransaction(ctx, []string{"webssh-z", "webssh-a"}, func(context.Context) error { called = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatal("cancelled partial acquisition entered mutation callback", err, called)
	}
	probe, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	unlock, err := e.lockContext(probe, "webssh-a")
	if err != nil {
		t.Fatal("partial runtime transaction leaked earlier lock", err)
	}
	unlock()
}

func TestRuntimeTransactionCannotBypassAnotherExecutorOrOutliveCallback(t *testing.T) {
	e, other := testAgent(t), testAgent(t)
	foreignUnlock := other.lock("webssh-same")
	defer foreignUnlock()
	var escaped context.Context
	if err := e.WithRuntimeTransaction(context.Background(), []string{"webssh-same"}, func(held context.Context) error {
		escaped = held
		deadline, cancel := context.WithTimeout(held, 40*time.Millisecond)
		defer cancel()
		if unlock, err := other.lockContext(deadline, "webssh-same"); err == nil {
			unlock()
			t.Fatal("one executor's callback bypassed another executor's lock")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	unlocked := e.lock("webssh-same")
	defer unlocked()
	deadline, cancel := context.WithTimeout(escaped, 40*time.Millisecond)
	defer cancel()
	if unlock, err := e.lockContext(deadline, "webssh-same"); err == nil {
		unlock()
		t.Fatal("completed runtime transaction remained an active lock bypass")
	}
}

func TestRuntimeTransactionIndependentRecoveryPreservesHeldMarker(t *testing.T) {
	e := testAgent(t)
	instance := installIntegration(t, e, "recovery")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := e.WithRuntimeTransaction(ctx, []string{instance.ID}, func(held context.Context) error {
		cancel()
		if !errors.Is(held.Err(), context.Canceled) {
			t.Fatal("business request did not cancel")
		}
		recovery, stop := context.WithTimeout(context.WithoutCancel(held), time.Second)
		defer stop()
		// Check crosses the actual instance lock boundary. A Background-based
		// recovery would wait on its own still-held transaction until timeout.
		return e.Check(recovery, instance.ID)
	}); err != nil {
		t.Fatal("bounded recovery lost its active transaction marker", err)
	}
}
