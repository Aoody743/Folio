package sites

import (
	"context"
	"sync"
)

type certificateLock struct {
	gate chan struct{}
	refs int
}
type certificateLocks struct {
	mu    sync.Mutex
	items map[string]*certificateLock
}

func (locks *certificateLocks) acquire(ctx context.Context, id string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	locks.mu.Lock()
	if locks.items == nil {
		locks.items = map[string]*certificateLock{}
	}
	item := locks.items[id]
	if item == nil {
		if len(locks.items) >= 64 {
			locks.mu.Unlock()
			return nil, failure("busy", "Certificate operations exceed their concurrency budget", 429)
		}
		item = &certificateLock{gate: make(chan struct{}, 1)}
		locks.items[id] = item
	}
	if item.refs >= 64 {
		locks.mu.Unlock()
		return nil, failure("busy", "Certificate operation queue exceeds its budget", 429)
	}
	item.refs++
	locks.mu.Unlock()
	releaseRef := func() {
		locks.mu.Lock()
		defer locks.mu.Unlock()
		item.refs--
		if item.refs == 0 {
			delete(locks.items, id)
		}
	}
	select {
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	case item.gate <- struct{}{}:
	}
	if err := ctx.Err(); err != nil {
		<-item.gate
		releaseRef()
		return nil, err
	}
	return func() { <-item.gate; releaseRef() }, nil
}
