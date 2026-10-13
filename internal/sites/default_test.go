package sites

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestQARootDefaultSwitchRequiresBothRevisionsAndRollsBackTogether(t *testing.T) {
	e, runtime := qaRootExecutor(t)
	old := rootOwnedTestSite(t)
	old.Default, old.Domains = true, []string{"old-default.example"}
	selected := rootOwnedTestSite(t)
	selected.Domains = []string{"new-default.example"}
	for _, site := range []*Site{&old, &selected} {
		result, err := qaAgentRun(context.Background(), e, "site_apply", request{ID: site.ID, Site: site})
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(result.Data, site); err != nil {
			t.Fatal(err)
		}
	}
	before := map[string]string{}
	for _, site := range []Site{old, selected} {
		data, err := os.ReadFile(filepath.Join(runtime.web.SnippetsDir, site.ID+".conf"))
		if err != nil {
			t.Fatal(err)
		}
		before[site.ID] = string(data)
	}
	input := request{ID: selected.ID, ExpectedRevision: selected.Revision}
	if _, err := qaAgentRun(context.Background(), e, "site_default_set", input); core.ErrorCode(err) != "default_site_changed" {
		t.Fatal("previous default was replaced without its revision", err)
	}
	input.PreviousSiteID, input.PreviousRevision = old.ID, old.Revision
	for _, fail := range []string{"check", "reload"} {
		t.Run(fail, func(t *testing.T) {
			if fail == "check" {
				runtime.failCheck = true
			} else {
				runtime.failReload = true
			}
			if _, err := qaAgentRun(context.Background(), e, "site_default_set", input); err == nil {
				t.Fatal("failed runtime switch accepted")
			}
			for _, site := range []Site{old, selected} {
				current, err := e.get(site.ID)
				if err != nil || current.Revision != site.Revision || current.Default != site.Default {
					t.Fatal("failed switch changed a committed receipt", err)
				}
				body, err := os.ReadFile(filepath.Join(runtime.web.SnippetsDir, site.ID+".conf"))
				if err != nil || string(body) != before[site.ID] {
					t.Fatal("failed switch did not restore both actual configuration files", err)
				}
			}
		})
	}
	result, err := qaAgentRun(context.Background(), e, "site_default_set", input)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(result.Data, &selected); err != nil {
		t.Fatal(err)
	}
	old, err = e.get(old.ID)
	if err != nil || !selected.Default || old.Default || selected.Revision != 2 || old.Revision != 2 {
		t.Fatal("both default generations did not commit together", err)
	}
	for _, site := range []Site{old, selected} {
		body, err := os.ReadFile(filepath.Join(runtime.web.SnippetsDir, site.ID+".conf"))
		if err != nil || strings.Contains(string(body), "default_server") != site.Default {
			t.Fatal("actual default listener differs from the committed receipt", err)
		}
	}
	if _, err = qaAgentRun(context.Background(), e, "site_default_set", input); core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("stale switch reapplied", err)
	}
	// Check/Reload is a fixture; actual Web switching has a separate managed
	// process gate. These assertions exercise protected config/receipt rollback.
}
