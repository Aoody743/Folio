package sites

import (
	"context"
	"testing"
)

func TestSiteStatusCountsRespectAuthorizationFiltersAndPagination(t *testing.T) {
	f := newCategoryCoordFixture(t)
	for _, entry := range []struct{ id, category, status, name string }{
		{"visible-a", "one", "running", "Production A"},
		{"visible-b", "one", "stopped", "Production B"},
		{"visible-c", "one", "new-state", "Production C"},
		{"other-category", "two", "running", "Production D"},
		{"other-name", "one", "running", "Development"},
		{"hidden", "one", "running", "Production secret"},
	} {
		site := categoryTestSite(entry.id, entry.category)
		site.Status, site.Name = entry.status, entry.name
		f.agent.sites[entry.id] = site
	}
	_, actor := f.tokenActor(t, "limited", []string{"sites:read"}, []string{Resource("visible-a"), Resource("visible-b"), Resource("visible-c"), Resource("other-category"), Resource("other-name")})
	for _, offset := range []int{0, 1, 2, 50} {
		result, err := f.service.Execute(context.Background(), actor, "sites_list", encode(map[string]any{"limit": 1, "offset": offset, "query": "production", "category_id": "one"}))
		if err != nil {
			t.Fatal(err)
		}
		data := result.(map[string]any)
		counts := data["status_counts"].(map[string]int)
		if data["total"] != 3 || counts["running"] != 1 || counts["stopped"] != 1 || counts["unknown"] != 1 {
			t.Fatalf("counts leak or use page: %#v", data)
		}
		if len(data["items"].([]Site)) > 1 {
			t.Fatal("pagination ignored")
		}
	}
	result, err := f.service.Execute(context.Background(), actor, "sites_list", encode(map[string]any{"limit": 1, "status": "unknown"}))
	if err != nil {
		t.Fatal(err)
	}
	data := result.(map[string]any)
	if data["total"] != 1 || data["items"].([]Site)[0].ID != "visible-c" {
		t.Fatalf("unknown status lost: %#v", data)
	}
}

func TestProjectAndMultipleStatusFiltersBeforePagination(t *testing.T) {
	f := newCategoryCoordFixture(t)
	for _, entry := range []struct{ id, kind, status string }{
		{"php-a", "php", "running"}, {"php-b", "php", "stopped"}, {"php-c", "php", "future"},
		{"static", "static", "running"}, {"git", "git", "running"}, {"proxy", "proxy", "running"}, {"hidden", "php", "running"},
	} {
		site := categoryTestSite(entry.id, "one")
		site.Status = entry.status
		switch entry.kind {
		case "php":
			site.PHPInstance = "php-main"
		case "git":
			site.ProjectType = "git"
		case "proxy":
			site.Proxies = []Proxy{{Enabled: true}}
		}
		f.agent.sites[entry.id] = site
	}
	_, actor := f.tokenActor(t, "projects", []string{"sites:read"}, []string{Resource("php-a"), Resource("php-b"), Resource("php-c"), Resource("static"), Resource("git"), Resource("proxy")})
	for _, offset := range []int{0, 1, 5} {
		result, err := f.service.Execute(context.Background(), actor, "sites_list", encode(map[string]any{"project_type": "php", "statuses": []string{"running", "stopped"}, "limit": 1, "offset": offset}))
		if err != nil {
			t.Fatal(err)
		}
		data := result.(map[string]any)
		if data["total"] != 2 {
			t.Fatalf("incorrect total: %#v", data)
		}
		for _, site := range data["items"].([]Site) {
			if site.ProjectType != "php" || site.ID == "hidden" {
				t.Fatalf("wrong project: %#v", site)
			}
		}
	}
	for _, kind := range []string{"static", "git", "proxy"} {
		result, err := f.service.Execute(context.Background(), actor, "sites_list", encode(map[string]any{"project_type": kind}))
		if err != nil {
			t.Fatal(err)
		}
		data := result.(map[string]any)
		if data["total"] != 1 || data["items"].([]Site)[0].ID != kind {
			t.Fatalf("wrong %s selection: %#v", kind, data)
		}
	}
}
