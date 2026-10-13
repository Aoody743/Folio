package sites

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/files"
)

// Retained receipts keep the ownership of content after its virtual host is
// deleted. They never turn an arbitrary existing directory into an owned site.
type RetainedSite struct {
	Site       Site      `json:"site"`
	RetainedAt time.Time `json:"retained_at"`
	Purging    bool      `json:"purging,omitempty"`
}

func (e *AgentExecutor) retained(id string) (RetainedSite, error) {
	var value RetainedSite
	if !safeID.MatchString(id) {
		return value, invalid("Invalid retained website ID")
	}
	err := rootJSON(e.state, filepath.Join("retained", id+".json"), &value)
	if errors.Is(err, fs.ErrNotExist) {
		return value, failure("not_found", "Retained website content was not found", 404)
	}
	if err != nil {
		return value, err
	}
	if value.Site.ID != id || value.Site.Revision < 1 || value.RetainedAt.IsZero() || value.Site.DocumentRoot != e.paths(value.Site).DocumentRoot {
		return value, errors.New("retained website receipt identity mismatch")
	}
	return value, nil
}

func (e *AgentExecutor) retainedSites() ([]RetainedSite, error) {
	directory, err := e.state.Open("retained")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	items := []RetainedSite{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		value, err := e.retained(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		if _, err = e.get(value.Site.ID); err == nil {
			// A crash between saving a retention receipt and removing the active
			// receipt has not committed deletion. The active receipt still owns it.
			continue
		} else if !isNotFound(err) {
			return nil, err
		}
		items = append(items, value)
		if len(items) > 10000 {
			return nil, failure("too_many_retained_sites", "Retained website inventory exceeds its limit", 413)
		}
	}
	return items, nil
}

func (e *AgentExecutor) rejectUnownedContent(id string) error {
	if _, err := e.retained(id); err == nil {
		return failure("retained_content_exists", "This website ID still owns retained content; use a distinct ID to create a new website", 409)
	} else if !isNotFound(err) {
		return err
	}
	if _, err := e.content.Lstat(id); err == nil {
		return failure("unowned_content_exists", "Existing content cannot be claimed by creating a new website", 409)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (e *AgentExecutor) purgeRetained(ctx context.Context, in request) (any, error) {
	if _, err := e.get(in.ID); err == nil {
		return nil, failure("site_still_active", "Active website content cannot be purged through the retained-content action", 409)
	} else if !isNotFound(err) {
		return nil, err
	}
	value, err := e.retained(in.ID)
	if err != nil {
		return nil, err
	}
	if value.Site.Revision != in.ExpectedRevision {
		return nil, failure("revision_conflict", "Retained content receipt changed", 412)
	}
	// Persist intent before irreversible cleanup, so a restart can distinguish
	// intentionally removed bytes from a broken retained-content root.
	value.Purging = true
	if err = atomicRoot(e.state, filepath.Join("retained", in.ID+".json"), encode(value), 0600); err != nil {
		return nil, err
	}
	cleanup := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := e.content.RemoveAll(in.ID); err != nil {
			return err
		}
		if err := e.files.DeleteRoot("site-" + in.ID); err != nil && !errors.Is(err, files.ErrNotFound) {
			return err
		}
		return e.state.Remove(filepath.Join("retained", in.ID+".json"))
	}
	err = e.files.RootTransaction(ctx, "site-"+in.ID, cleanup)
	if errors.Is(err, files.ErrNotFound) {
		// A restarted Agent omits a root whose committed purge intent already
		// removed all content. No descriptor or active lease then needs draining.
		if _, contentErr := e.content.Lstat(filepath.Join(in.ID, "public")); errors.Is(contentErr, fs.ErrNotExist) {
			err = cleanup()
		}
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": in.ID, "deleted": true, "data_retained": false}, nil
}
