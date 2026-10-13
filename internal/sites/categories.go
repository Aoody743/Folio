package sites

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func categoryResource(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var in struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &in) != nil || !safeID.MatchString(in.ID) {
		return nil, invalid("A valid category ID is required")
	}
	return []string{"site-category:" + in.ID}, nil
}
func (p *Provider) categoryBindings() []core.Binding {
	out := []core.Binding{}
	for _, name := range []string{"sites_categories_list", "sites_category_save", "sites_category_delete"} {
		schema := object(map[string]any{})
		resource := noResources
		read := name == "sites_categories_list"
		scope := "sites:read"
		if !read {
			scope = "sites:write"
			resource = categoryResource
			fields := map[string]any{"id": idField(), "expected_revision": number(0, 1<<30)}
			if name == "sites_category_save" {
				fields["name"] = text(160)
				schema = object(fields, "id", "expected_revision", "name")
			} else {
				schema = object(fields, "id", "expected_revision")
			}
		}
		out = append(out, core.Binding{Action: core.Action{Name: name, Description: "Persist website categories with revision checks and live-reference protection", Scope: scope, ReadOnly: read, InputSchema: schema}, Resources: resource, Execute: func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			return p.categoryAction(ctx, actor, name, raw)
		}})
	}
	return out
}
func (p *Provider) categoryAction(ctx context.Context, actor core.Actor, name string, raw json.RawMessage) (core.ActionResult, error) {
	if name == "sites_categories_list" {
		values, err := p.deps.State.List(ctx, "categories")
		if err != nil {
			return core.ActionResult{}, err
		}
		items := []Category{}
		for id, value := range values {
			if !p.deps.Authorize(actor, "sites:read", "site-category:"+id) {
				continue
			}
			var category Category
			if json.Unmarshal(value, &category) != nil {
				return core.ActionResult{}, failure("metadata_invalid", "Stored category needs inspection", 500)
			}
			items = append(items, category)
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
		return core.ActionResult{Data: map[string]any{"items": items, "total": len(items)}}, nil
	}
	var in struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		ExpectedRevision int64  `json:"expected_revision"`
	}
	if err := strict(raw, &in); err != nil {
		return core.ActionResult{}, err
	}
	resource := "site-category:" + in.ID
	if name == "sites_category_save" {
		if strings.TrimSpace(in.Name) == "" {
			return core.ActionResult{}, invalid("Category name cannot be empty")
		}
		category := Category{ID: in.ID, Name: strings.TrimSpace(in.Name), Revision: in.ExpectedRevision + 1}
		_, err := p.deps.State.CompareAndSwap(ctx, "categories", in.ID, in.ExpectedRevision, encode(category))
		return core.ActionResult{Data: category, Resources: []string{resource}}, err
	}
	var sites []Site
	if err := p.call(ctx, actor, "site_list", request{}, &sites); err != nil {
		return core.ActionResult{}, err
	}
	for _, site := range sites {
		if site.CategoryID == in.ID {
			return core.ActionResult{}, failure("category_in_use", "Move the category's websites before deleting it", 409)
		}
	}
	err := p.deps.State.Transaction(ctx, func(store core.MetadataStore) error {
		_, revision, err := store.GetVersioned(ctx, "categories", in.ID)
		if err != nil {
			return err
		}
		if revision != in.ExpectedRevision {
			return failure("revision_conflict", "Category revision changed", 412)
		}
		return store.Delete(ctx, "categories", in.ID)
	})
	return core.ActionResult{Data: map[string]any{"id": in.ID, "deleted": err == nil}, Resources: []string{resource}}, err
}
