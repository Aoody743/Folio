package sites

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

type DefaultSelection struct {
	Selected Site  `json:"selected"`
	Previous *Site `json:"previous"`
}

func (e *AgentExecutor) defaultSelection(id string) (DefaultSelection, error) {
	selected, err := e.get(id)
	if err != nil {
		return DefaultSelection{}, err
	}
	all, err := e.list()
	if err != nil {
		return DefaultSelection{}, err
	}
	view := DefaultSelection{Selected: selected}
	for _, site := range all {
		if site.ID != selected.ID && site.WebInstance == selected.WebInstance && site.Default {
			if view.Previous != nil {
				return view, failure("default_site_conflict", "Default website state changed; retry after the active task completes", 409)
			}
			copy := site
			view.Previous = &copy
		}
	}
	if view.Selected.Default && view.Previous != nil {
		return view, failure("default_site_conflict", "Default website state changed; retry after the active task completes", 409)
	}
	return view, nil
}

func (p *Provider) defaultView(ctx context.Context, actor core.Actor, in request) (core.ActionResult, error) {
	var view DefaultSelection
	if err := p.call(ctx, actor, "site_default_get", in, &view); err != nil {
		return core.ActionResult{}, err
	}
	resources := []string{Resource(in.ID)}
	if view.Previous != nil {
		if !p.deps.Authorize(actor, "sites:read", Resource(view.Previous.ID)) {
			return core.ActionResult{}, denied()
		}
		resources = append(resources, Resource(view.Previous.ID))
	}
	return core.ActionResult{Data: view, Resources: resources}, nil
}

func (p *Provider) defaultResources(ctx context.Context, actor core.Actor, raw json.RawMessage) ([]string, error) {
	var input struct {
		ID             string `json:"id"`
		PreviousSiteID string `json:"previous_site_id"`
	}
	if json.Unmarshal(raw, &input) != nil || !safeID.MatchString(input.ID) || input.PreviousSiteID == input.ID || input.PreviousSiteID != "" && !safeID.MatchString(input.PreviousSiteID) {
		return nil, invalid("Invalid default website selection")
	}
	if !p.deps.Authorize(actor, "sites:write", "host:local") {
		return nil, denied()
	}
	resources := []string{"host:local"}
	for _, id := range []string{input.ID, input.PreviousSiteID} {
		if id == "" {
			continue
		}
		if !p.deps.Authorize(actor, "sites:read", Resource(id)) || !p.deps.Authorize(actor, "sites:write", Resource(id)) {
			return nil, denied()
		}
		site, err := p.get(ctx, actor, id)
		if err != nil {
			return nil, err
		}
		if !p.deps.Authorize(actor, "software:read", "software:"+site.WebInstance) {
			return nil, denied()
		}
		resources = append(resources, Resource(id), "software:"+site.WebInstance)
	}
	return resources, nil
}

func (e *AgentExecutor) setDefault(ctx context.Context, in request) (selected Site, err error) {
	selected, err = e.get(in.ID)
	if err != nil {
		return selected, err
	}
	if selected.Revision != in.ExpectedRevision {
		return selected, failure("revision_conflict", "Selected website revision changed", 412)
	}
	if in.PreviousSiteID == in.ID || in.PreviousSiteID != "" && !safeID.MatchString(in.PreviousSiteID) || in.PreviousRevision < 0 {
		return selected, invalid("Invalid previous default website")
	}
	all, err := e.list()
	if err != nil {
		return selected, err
	}
	var previous *Site
	for _, site := range all {
		if site.ID != selected.ID && site.WebInstance == selected.WebInstance && site.Default {
			if previous != nil {
				return selected, failure("default_site_conflict", "Existing default website ownership requires recovery inspection", 409)
			}
			copy := site
			previous = &copy
		}
	}
	if previous == nil && (in.PreviousSiteID != "" || in.PreviousRevision != 0) || previous != nil && (previous.ID != in.PreviousSiteID || previous.Revision != in.PreviousRevision) {
		return selected, failure("default_site_changed", "Reload the current default website before replacing it", 409)
	}
	if selected.Default && previous == nil {
		return selected, nil
	}
	updated := []Site{selected}
	updated[0].Default = true
	if previous != nil {
		previous.Default = false
		updated = append(updated, *previous)
	}
	web, err := e.runtimes.Web(ctx, selected.WebInstance)
	if err != nil {
		return selected, err
	}
	changes := []changedFile{}
	committed := false
	defer func() {
		if committed || journalActive(ctx, e) {
			return
		}
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		recoverErr := rollbackConfigs(changes)
		recoverErr = errors.Join(recoverErr, e.runtimes.Reload(recovery, selected.WebInstance))
		if recoverErr != nil {
			err = errors.Join(err, failure("rollback_incomplete", "Default website rollback requires recovery inspection", 500))
		}
	}()
	replace := func(path string, body []byte) error {
		before, err := snapshotConfig(path)
		if err != nil {
			return err
		}
		changes = append(changes, before)
		return writeProtected(path, body)
	}
	now := time.Now().UTC()
	for index := range updated {
		site := &updated[index]
		if site.ConfigurationMode == "advanced" {
			return selected, failure("advanced_configuration_active", "Explicitly reset advanced configurations before changing the default website", 409)
		}
		if err = e.validateTLS(*site); err != nil {
			return selected, err
		}
		site.Revision++
		site.UpdatedAt, site.UpdatedBy = now, in.Actor.Public()
		paths := e.paths(*site)
		fpm := ""
		if site.PHPInstance != "" {
			fpm = "unix:" + filepath.Join(paths.RunDir, "php-fpm.sock")
		}
		body, renderErr := renderWebsite(*site, paths, web, fpm, now)
		if renderErr != nil {
			return selected, renderErr
		}
		if err = replace(filepath.Join(web.SnippetsDir, site.ID+".conf"), []byte(body)); err != nil {
			return selected, err
		}
	}
	if err = e.runtimes.Check(ctx, selected.WebInstance); err != nil {
		return selected, failure("configuration_rejected", "Web runtime rejected the default website switch", 409)
	}
	if err = e.runtimes.Reload(ctx, selected.WebInstance); err != nil {
		return selected, failure("reload_failed", "Web runtime failed to reload the default website switch", 500)
	}
	if verifier, ok := e.runtimes.(RuntimeVerifier); ok {
		for _, site := range updated {
			if err = verifier.VerifySite(ctx, site); err != nil {
				return selected, failure("runtime_not_applied", "Web runtime did not serve the requested default website generation", 502)
			}
		}
	}
	for _, site := range updated {
		if err = replace(filepath.Join(e.config.StateDir, "sites", site.ID+".json"), encode(site)); err != nil {
			return selected, err
		}
	}
	committed = true
	return updated[0], nil
}
