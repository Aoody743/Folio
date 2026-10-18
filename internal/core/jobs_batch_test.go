package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/jobs"
)

func TestTaskListBatchRetainsResourceFilteringPublicActorsAndLegacyBoundaries(t *testing.T) {
	s, admin := testService(t)
	store := sqliteJobStore{s: s}
	now := time.Now().UTC()
	for i := 0; i < 270; i++ {
		resource := "site:foreign"
		if i >= 150 {
			resource = "site:allowed"
		}
		meta := taskMetadata{Actor: admin, Resources: []string{resource}, Action: "sites_update", Scope: "sites:write"}
		ctx := context.WithValue(context.Background(), taskMetaKey{}, meta)
		job := jobs.Job{ID: fmt.Sprintf("%032x", i), ActorID: admin.ID, Operation: "site_apply", Input: json.RawMessage(`{"private":"stored-private-input"}`), Status: jobs.Succeeded, CreatedAt: now.Add(-time.Duration(i) * time.Second), UpdatedAt: now}
		if err := store.Save(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	legacy := jobs.Job{ID: fmt.Sprintf("%032x", 10000), ActorID: admin.ID, Operation: "file_copy", Input: json.RawMessage(`{"root":"workspace","destination_root":"private","content":"stored-private-input"}`), Status: jobs.Succeeded, CreatedAt: now.Add(time.Second), UpdatedAt: now}
	if err := store.Save(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	actor := Actor{ID: "token:scoped", Name: "Scoped task reader", Kind: "mcp", Scopes: []string{"tasks:read"}, Resources: []string{"site:allowed", "files:workspace"}}
	value, err := s.listJobs(context.Background(), actor)
	if err != nil {
		t.Fatal(err)
	}
	items := value.([]map[string]any)
	if len(items) != 100 {
		t.Fatalf("expected 100 authorized tasks beyond the first metadata batch, got %d", len(items))
	}
	for i, item := range items {
		if item["id"] != fmt.Sprintf("%032x", i+150) || item["resource"] != "site:allowed" {
			t.Fatalf("task ordering, resource filtering, or legacy multi-root boundary changed: %+v", item)
		}
	}
	raw, _ := json.Marshal(items)
	if strings.Contains(string(raw), "stored-private-input") || strings.Contains(string(raw), `"scopes"`) {
		t.Fatal("task list leaked private inputs or actor grants")
	}
	actor.Resources = nil
	value, err = s.listJobs(context.Background(), actor)
	if err != nil || len(value.([]map[string]any)) != 0 {
		t.Fatal("a new request reused prior resource authority", err)
	}
}
