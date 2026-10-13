package core

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
)

type PanelBookmark struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Description string `json:"description"`
	Revision    int64  `json:"revision"`
}

func (s *Service) bookmarkBindings() []Binding {
	fields := map[string]any{"id": map[string]any{"type": "string", "minLength": 1, "maxLength": 80}, "name": map[string]any{"type": "string", "minLength": 1, "maxLength": 80}, "url": map[string]any{"type": "string", "format": "uri", "maxLength": 2048}, "description": boundedText(240), "expected_revision": map[string]any{"type": "integer", "minimum": 0}}
	return []Binding{identityBinding("panel_bookmarks_list", "Read persisted panel links as safe URLs", "settings:read", true, object(map[string]any{}), hostIdentityResource, s.listBookmarks), identityBinding("panel_bookmarks_save", "Create or update a panel link using revision protection", "settings:write", false, object(fields, "id", "name", "url", "expected_revision"), hostIdentityResource, s.saveBookmark), identityBinding("panel_bookmarks_delete", "Remove one persisted panel link with revision protection", "settings:write", false, object(map[string]any{"id": fields["id"], "expected_revision": map[string]any{"type": "integer", "minimum": 1}}, "id", "expected_revision"), hostIdentityResource, s.deleteBookmark)}
}
func (s *Service) listBookmarks(ctx context.Context, a Actor, _ json.RawMessage) (ActionResult, error) {
	store := s.Runtime("identity").State
	records, e := store.List(ctx, "bookmarks")
	if e != nil {
		return ActionResult{}, e
	}
	items := []PanelBookmark{}
	for id, raw := range records {
		var p PanelBookmark
		if json.Unmarshal(raw, &p) != nil {
			return ActionResult{}, internal()
		}
		_, rev, e := store.GetVersioned(ctx, "bookmarks", id)
		if e != nil {
			return ActionResult{}, e
		}
		p.Revision = rev
		items = append(items, p)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return ActionResult{Data: map[string]any{"items": items, "total": len(items)}}, nil
}
func (s *Service) saveBookmark(ctx context.Context, a Actor, raw json.RawMessage) (ActionResult, error) {
	var in struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		URL         string `json:"url"`
		Description string `json:"description"`
		Expected    int64  `json:"expected_revision"`
	}
	if e := decodeInput(raw, &in); e != nil {
		return ActionResult{}, e
	}
	u, e := url.Parse(in.URL)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return ActionResult{}, invalid("Panel link must be an HTTP(S) URL without embedded credentials")
	}
	p := PanelBookmark{ID: in.ID, Name: in.Name, URL: u.String(), Description: in.Description, Revision: in.Expected + 1}
	rev, e := s.Runtime("identity").State.CompareAndSwap(ctx, "bookmarks", in.ID, in.Expected, json.RawMessage(jsonText(p)))
	p.Revision = rev
	return ActionResult{Data: p}, e
}
func (s *Service) deleteBookmark(ctx context.Context, a Actor, raw json.RawMessage) (ActionResult, error) {
	var in struct {
		ID       string `json:"id"`
		Expected int64  `json:"expected_revision"`
	}
	if e := decodeInput(raw, &in); e != nil {
		return ActionResult{}, e
	}
	e := s.Runtime("identity").State.Transaction(ctx, func(tx MetadataStore) error {
		_, rev, e := tx.GetVersioned(ctx, "bookmarks", in.ID)
		if e != nil {
			return e
		}
		if rev != in.Expected {
			return &Error{Code: "revision_conflict", Message: "Panel link changed; reload before deleting", Status: 412}
		}
		return tx.Delete(ctx, "bookmarks", in.ID)
	})
	return ActionResult{Data: map[string]any{"id": in.ID, "deleted": e == nil}}, e
}
