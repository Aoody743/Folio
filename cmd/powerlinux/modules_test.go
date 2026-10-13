package main

import (
	"context"
	"encoding/json"
	"github.com/Aoody743/PowerLinux/internal/core"
	"strings"
	"testing"
	"time"
)

func TestFullCompositionSharesRegistryWithoutAgent(t *testing.T) {
	svc, _, admin, _ := fileFixture(t)
	modules, err := registerModules(svc, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer modules.system.Close()
	ctx := context.Background()
	for _, name := range []string{"certificates_dns_providers", "plugins_catalog", "terminal_targets_list", "sites_list", "services_list"} {
		_, err = svc.Execute(ctx, admin, name, json.RawMessage(`{}`))
		if err != nil && core.ErrorStatus(err) != 503 {
			t.Fatalf("%s should be available or report missing optional Agent, got %v", name, err)
		}
	}
	for _, scope := range []string{"terminal:root", "terminal:share", "files:host", "files:permissions", "certificates:write", "sites:backup"} {
		token, err := svc.CreateToken(ctx, admin, core.TokenRequest{Name: "explicit composition scope", Scopes: []string{scope}, Resources: []string{"*"}})
		if err != nil {
			t.Fatalf("scope %s was not registered: %v", scope, err)
		}
		if token.Token == "" {
			t.Fatal("empty scoped token")
		}
	}
	paths := svc.OpenAPI(admin)["paths"].(map[string]any)
	for _, name := range []string{"fs_read", "sites_create", "certificates_acme_confirm", "terminal_open", "schedules_create", "software_plan"} {
		if _, ok := paths["/api/v1/actions/"+name]; !ok {
			t.Fatalf("shared OpenAPI missed %s", name)
		}
	}
	if err = svc.RegisterScopes("invalid"); err == nil {
		t.Fatal("invalid scope accepted")
	}
	svc.SealRegistry()
	if _, err = registerModules(svc, nil); err == nil {
		t.Fatal("late module registration was allowed")
	}
}

func TestRetainedPurgeReachesDurableDispatcherAndMissingAgentFails(t *testing.T) {
	svc, _, admin, _ := fileFixture(t)
	modules, err := registerModules(svc, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer modules.system.Close()
	ctx := context.Background()
	result, err := svc.Execute(ctx, admin, "sites_retained_purge", json.RawMessage(`{"id":"retained-fixture","expected_revision":1}`))
	if err != nil {
		t.Fatal("retained-content action was not admitted to its registered durable executor", err)
	}
	job, ok := result.(map[string]any)
	if !ok || job["id"] == nil {
		t.Fatal("missing durable task identity")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, err := svc.Execute(ctx, admin, "tasks_get", mustJSON(map[string]any{"id": job["id"]}))
		if err != nil {
			t.Fatal(err)
		}
		state := view.(map[string]any)
		if state["status"] == "failed" {
			message, _ := state["error"].(string)
			if !strings.HasPrefix(message, "agent_unavailable:") {
				t.Fatalf("durable executor did not preserve missing-Agent error: %#v", state)
			}
			return
		}
		if state["status"] == "succeeded" {
			t.Fatal("retained content purge succeeded without an Agent")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("retained-content task was not executed")
}
