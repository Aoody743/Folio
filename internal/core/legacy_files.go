package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// LegacyFileDelegate is a startup-only trusted compatibility adapter. The three
// phase-one names keep their schemas/scopes while the actual IO goes through
// the same history-aware filesystem implementation as fs_* operations.
type LegacyFileDelegate interface {
	Resources(context.Context, Actor, string, json.RawMessage) ([]string, error)
	Execute(context.Context, Actor, string, json.RawMessage) (ActionResult, error)
}

func (s *Service) SetLegacyFileDelegate(delegate LegacyFileDelegate) error {
	if delegate == nil {
		return fmt.Errorf("legacy file delegate required")
	}
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	if s.registrySealed || s.legacyFiles != nil {
		return fmt.Errorf("legacy file delegate must be installed exactly once before serving")
	}
	s.legacyFiles = delegate
	return nil
}
func (s *Service) legacyFileBinding(name string) (Binding, bool) {
	if name != "files_write" && name != "files_backup" && name != "files_hash" {
		return Binding{}, false
	}
	s.registryMu.RLock()
	delegate := s.legacyFiles
	s.registryMu.RUnlock()
	if delegate == nil {
		return Binding{}, false
	}
	var action Action
	for _, a := range registry() {
		if a.Name == name {
			action = a
			break
		}
	}
	action.Task = true
	return Binding{Action: action, Resources: func(ctx context.Context, a Actor, input json.RawMessage) ([]string, error) {
		return delegate.Resources(ctx, a, name, input)
	}, Execute: func(ctx context.Context, a Actor, input json.RawMessage) (ActionResult, error) {
		return delegate.Execute(ctx, a, name, input)
	}}, true
}
func resourceAliases(resource string) []string {
	if strings.HasPrefix(resource, "files:") {
		return []string{resource, "fs:" + strings.TrimPrefix(resource, "files:")}
	}
	if strings.HasPrefix(resource, "fs:") {
		return []string{resource, "files:" + strings.TrimPrefix(resource, "fs:")}
	}
	return []string{resource}
}
func expandResourceAliases(resources []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, resource := range resources {
		for _, alias := range resourceAliases(resource) {
			if !seen[alias] {
				seen[alias] = true
				out = append(out, alias)
			}
		}
	}
	return out
}
