package core

import (
	"context"
	"time"
)

// Each registered executor receives a revocable lease in addition to the
// fresh authorization immediately before it starts. The identity and grants
// come from server-issued task metadata, never the operation's JSON input.
func (s *Service) taskLeaseContext(ctx context.Context) (context.Context, context.CancelFunc) {
	grant, ok := ctx.Value(authorizedActionKey{}).(authorizedAction)
	if !ok {
		return context.WithCancel(ctx)
	}
	lease, cancel := context.WithCancelCause(ctx)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-lease.Done():
				return
			case <-ticker.C:
				actor, err := s.RefreshActor(lease, Actor{ID: grant.ActorID})
				if err == nil && !hasScope(actor, grant.Scope) {
					err = denied()
				}
				if err == nil {
					err = authorizeResources(actor, grant.Resources)
				}
				if err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	return lease, func() { cancel(context.Canceled) }
}
