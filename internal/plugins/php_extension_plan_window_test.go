package plugins

import (
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestPHPModulePlanWindowUsesOneInstantAndAcceptsBoundedExistingClockSkew(t *testing.T) {
	e := testAgent(t)
	now := time.Date(2026, time.October, 15, 12, 34, 56, 789, time.FixedZone("fixture", 2*60*60))
	createdAt, expiresAt := phpModulePlanWindow(now)
	if createdAt.Location() != time.UTC || !expiresAt.Equal(createdAt.Add(phpExtensionPlanTTL)) || !validPHPModulePlanWindow(createdAt, expiresAt) {
		t.Fatalf("new plan window was not built from one UTC instant: created=%s expires=%s", createdAt, expiresAt)
	}

	planID := "plan_" + strings.Repeat("a", 48)
	plan := phpExtensionPlanRecord{Public: PHPModulePlan{
		PlanID: planID, ID: "php-window-test", Operation: "uninstall", ModuleID: "curl",
		ExpectedRevision: 1, Resources: []string{InstanceResource("php-window-test"), "host:packages"},
		CreatedAt: createdAt, ExpiresAt: createdAt.Add(phpExtensionPlanTTL + phpExtensionPlanTTLTolerance/2),
	}, PackageName: "php8.3-curl", ExtensionDirSHA256: strings.Repeat("b", 64)}
	if err := e.save("plans", planID, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := e.loadPHPExtensionPlan(planID); err != nil {
		t.Fatalf("a previously persisted plan with subsecond clock-call skew was rejected: %v", err)
	}

	plan.Public.ExpiresAt = createdAt.Add(phpExtensionPlanTTL + phpExtensionPlanTTLTolerance + time.Nanosecond)
	if err := e.save("plans", planID, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := e.loadPHPExtensionPlan(planID); core.ErrorCode(err) != "extension_plan_invalid" {
		t.Fatalf("plan window beyond the bounded compatibility tolerance was accepted: %v", err)
	}
}
