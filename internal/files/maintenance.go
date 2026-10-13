package files

import (
	"context"
	"errors"
	"sort"
)

type rootMaintenance struct {
	prior           *managedRoot
	drained, resume chan struct{}
}
type rootRequestKey struct{ service *Service }

// RootTransaction pauses new Agent requests for this root and waits for active
// operations, including streams, before calling the trusted maintenance code.
// The callback may DeleteRoot/AddRoot. On error the prior pinned descriptor is
// restored; the caller must also roll back its filesystem directory swap.
func (s *Service) RootTransaction(ctx context.Context, id string, callback func() error) error {
	if callback == nil {
		return errors.New("root maintenance callback required")
	}
	s.mu.Lock()
	prior, exists := s.roots[id]
	if !exists {
		s.mu.Unlock()
		return ErrNotFound
	}
	if s.maintenance[id] != nil {
		s.mu.Unlock()
		return failure("busy", "Filesystem root maintenance is already in progress", 409)
	}
	m := &rootMaintenance{prior: prior, drained: make(chan struct{}), resume: make(chan struct{})}
	s.maintenance[id] = m
	if prior.active == 0 {
		close(m.drained)
	}
	s.mu.Unlock()
	finish := func(err error) error {
		s.mu.Lock()
		current := s.roots[id]
		if err != nil {
			s.roots[id] = prior
		}
		delete(s.maintenance, id)
		close(m.resume)
		s.mu.Unlock()
		if err != nil && current != nil && current != prior {
			current.root.Close()
		}
		if err == nil && current != prior {
			prior.root.Close()
		}
		return err
	}
	select {
	case <-ctx.Done():
		return finish(ctx.Err())
	case <-m.drained:
	}
	select {
	case <-ctx.Done():
		return finish(ctx.Err())
	case prior.gate <- struct{}{}:
	}
	defer func() { <-prior.gate }()
	if err := ctx.Err(); err != nil {
		return finish(err)
	}
	return finish(callback())
}

func (s *Service) beginRequest(ctx context.Context, ids ...string) (context.Context, func(), error) {
	if ctx.Value(rootRequestKey{s}) != nil {
		return ctx, func() {}, nil
	}
	sort.Strings(ids)
	leased := []*managedRoot{}
	release := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, root := range leased {
			root.active--
			if root.active == 0 {
				for _, maintenance := range s.maintenance {
					if maintenance.prior == root {
						close(maintenance.drained)
					}
				}
			}
		}
	}
	unique := []string{}
	for _, id := range ids {
		if id != "" && (len(unique) == 0 || unique[len(unique)-1] != id) {
			unique = append(unique, id)
		}
	}
	for {
		s.mu.Lock()
		var paused <-chan struct{}
		for _, id := range unique {
			if m := s.maintenance[id]; m != nil {
				paused = m.resume
				break
			}
		}
		if paused != nil {
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx, nil, ctx.Err()
			case <-paused:
				continue
			}
		}
		for _, id := range unique {
			if s.roots[id] == nil || s.roots[id].spec.internal {
				s.mu.Unlock()
				return ctx, nil, failure("unsafe_path", "Unknown filesystem root", 403)
			}
		}
		for _, id := range unique {
			root := s.roots[id]
			root.active++
			leased = append(leased, root)
		}
		s.mu.Unlock()
		break
	}
	return context.WithValue(ctx, rootRequestKey{s}, true), release, nil
}
