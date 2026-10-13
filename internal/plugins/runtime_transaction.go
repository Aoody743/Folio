package plugins

import (
	"context"
	"slices"
	"sync/atomic"
)

type runtimeTransactionKey struct{}
type runtimeTransaction struct {
	executor *PrivilegedExecutor
	ids      map[string]bool
	active   atomic.Bool
}

// WithRuntimeTransaction serializes site configuration/certificate changes
// against software mutations. The trusted composition callback must execute
// synchronously and must not retain its context. Only matching instance locks
// of this executor are reentrant; a completed transaction grants no bypass.
func (e *PrivilegedExecutor) WithRuntimeTransaction(ctx context.Context, ids []string, callback func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if callback == nil || len(ids) > 256 {
		return failure("invalid_runtime_transaction", "A bounded synchronous runtime transaction is required", 400)
	}
	ordered := append([]string(nil), ids...)
	for _, id := range ordered {
		if !validInstanceID(id) {
			return failure("invalid_runtime_transaction", "Invalid runtime transaction instance ID", 400)
		}
	}
	slices.Sort(ordered)
	ordered = slices.Compact(ordered)
	if prior, ok := ctx.Value(runtimeTransactionKey{}).(*runtimeTransaction); ok && prior.executor == e && prior.active.Load() {
		for _, id := range ordered {
			if !prior.ids[id] {
				return failure("runtime_transaction_nested", "Nested runtime transactions cannot expand their predeclared lock set", 409)
			}
		}
		return callback(ctx)
	}
	transaction := &runtimeTransaction{executor: e, ids: map[string]bool{}}
	unlock := []func(){}
	defer func() {
		transaction.active.Store(false)
		for i := len(unlock) - 1; i >= 0; i-- {
			unlock[i]()
		}
	}()
	for _, id := range ordered {
		release, err := e.lockContext(ctx, id)
		if err != nil {
			return err
		}
		unlock = append(unlock, release)
		transaction.ids[id] = true
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	transaction.active.Store(true)
	return callback(context.WithValue(ctx, runtimeTransactionKey{}, transaction))
}

func (e *PrivilegedExecutor) runtimeLockHeld(ctx context.Context, id string) bool {
	transaction, ok := ctx.Value(runtimeTransactionKey{}).(*runtimeTransaction)
	return ok && transaction.executor == e && transaction.active.Load() && transaction.ids[id]
}
