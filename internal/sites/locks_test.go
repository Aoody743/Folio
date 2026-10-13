package sites

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCertificateLockCancellationReclaimsEntriesAndBoundsDistinctWork(t *testing.T) {
	var locks certificateLocks
	unlocks := []func(){}
	for index := 0; index < 64; index++ {
		unlock, err := locks.acquire(context.Background(), fmt.Sprint(index))
		if err != nil {
			t.Fatal(err)
		}
		unlocks = append(unlocks, unlock)
	}
	if _, err := locks.acquire(context.Background(), "one-too-many"); err == nil {
		t.Fatal("unbounded distinct certificate operations accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { _, err := locks.acquire(ctx, "0"); finished <- err }()
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled operation stayed blocked behind another certificate")
	}
	for _, unlock := range unlocks {
		unlock()
	}
	if len(locks.items) != 0 {
		t.Fatal("completed certificate identities retained indefinitely")
	}
}
