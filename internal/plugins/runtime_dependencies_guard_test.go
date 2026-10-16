package plugins

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestFreshInstallCannotBypassOrphanPHPExtensionJournal(t *testing.T) {
	e, instance, _ := phpExtensionStartupFixture(t)
	stateBefore := privatePHPStateDigest(t, e, instance.ID)
	if err := os.Remove(filepath.Join(e.config.StateDir, "receipts", instance.ID+".json")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	unlock, err := e.lockContext(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = e.validateInstallTarget(ctx, Instance{}, failure("not_found", "missing fixture receipt", 404), instance.PluginID, instance.InstanceID)
	unlock()
	if core.ErrorCode(err) != "extension_state_invalid" {
		t.Fatalf("fresh target validation bypassed its orphan extension journal: %v", err)
	}
	if _, err := e.plan(ctx, PlanInput{Operation: "install", PluginID: instance.PluginID, InstanceID: instance.InstanceID}); core.ErrorCode(err) != "extension_state_invalid" {
		t.Fatalf("public install planner bypassed its orphan extension journal: %v", err)
	}
	// A previously approved plan must also be rejected when receipt loss and
	// an orphan runtime journal occur between planning and actual execution.
	manifest, err := e.catalog.Get(instance.PluginID)
	if err != nil || len(manifest.Versions) == 0 {
		t.Fatalf("fixture runtime catalog unavailable: %v", err)
	}
	plan := Plan{ID: "plan_" + strings.Repeat("f", 48), InstanceID: instance.ID, PluginID: instance.PluginID,
		Operation: "install", Version: manifest.Versions[0], State: "planned", ExpiresAt: time.Now().Add(time.Minute)}
	if err := e.save("plans", plan.ID, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := e.apply(ctx, ApplyInput{PlanID: plan.ID, IdempotencyKey: "orphan-install-apply-key"}); core.ErrorCode(err) != "extension_state_invalid" {
		t.Fatalf("public install execution bypassed its orphan extension journal: %v", err)
	}
	state, err := e.loadPHPExtensionState(instance.ID)
	if err != nil || state.Pending == nil {
		t.Fatalf("denied install changed pending recovery state: %v", err)
	}
	if got := privatePHPStateDigest(t, e, instance.ID); got != stateBefore {
		t.Fatal("denied plan/apply changed the protected recovery journal")
	}
}
