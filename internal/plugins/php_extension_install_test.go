package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestPHPModuleRecipesAreLimitedToTrustedOfficialABIs(t *testing.T) {
	cases := []struct {
		platform Platform
		minor    string
		api      string
		ok       bool
	}{
		{Platform{OS: "linux", Arch: "amd64", Distro: "debian", Release: "12"}, "8.2", "20220829", true},
		{Platform{OS: "linux", Arch: "arm64", Distro: "ubuntu", Release: "24.04"}, "8.3", "20230831", true},
		{Platform{OS: "linux", Arch: "amd64", Distro: "debian", Release: "13"}, "8.4", "20240924", true},
		{Platform{OS: "linux", Arch: "amd64", Distro: "ubuntu", Release: "24.10"}, "", "", false},
		{Platform{OS: "linux", Arch: "amd64", Distro: "ubuntu", Release: "22.04"}, "", "", false},
		{Platform{OS: "linux", Arch: "386", Distro: "debian", Release: "12"}, "", "", false},
		{Platform{OS: "darwin", Arch: "arm64", Distro: "debian", Release: "13"}, "", "", false},
	}
	for _, tc := range cases {
		recipe, ok := phpExtensionPlatform(tc.platform)
		if ok != tc.ok || ok && (recipe.Minor != tc.minor || recipe.API != tc.api) {
			t.Errorf("recipe for %+v = %+v,%v; want %q/%q,%v", tc.platform, recipe, ok, tc.minor, tc.api, tc.ok)
		}
	}
	for _, tc := range []struct{ minor, module, want string }{
		{"8.2", "mysqli", "php8.2-mysql"},
		{"8.3", "pdo_mysql", "php8.3-mysql"},
		{"8.4", "pgsql", "php8.4-pgsql"},
		{"8.4", "pdo_pgsql", "php8.4-pgsql"},
		{"8.3", "imagick", ""},
		{"8.4", "mongodb", ""},
	} {
		got, err := phpExtensionPackage(tc.minor, tc.module)
		if tc.want == "" {
			if err == nil {
				t.Errorf("unsupported recipe %s/%s returned %q", tc.minor, tc.module, got)
			}
		} else if err != nil || got != tc.want {
			t.Errorf("recipe %s/%s = %q, %v; want %q", tc.minor, tc.module, got, err, tc.want)
		}
	}
}

func TestPHPModulePackageOwnershipDoesNotClaimPreexistingPackages(t *testing.T) {
	pin := PackagePin{Name: "php8.3-mysql", Version: "8.3.6-0ubuntu0.24.04.2", Architecture: "amd64", Repository: "https://archive.ubuntu.com", SHA256: strings.Repeat("a", 64)}
	external := phpExtensionPlanRecord{Public: PHPModulePlan{ID: "php-site"}, PackageName: pin.Name, TargetPackageWasInstalled: true}
	if receipt, save, err := phpExtensionInstalledPackageReceipt(external, pin, nil); err != nil || save || receipt.InstalledByAgent {
		t.Fatalf("reinstalling a pre-existing external package created ownership: %+v %v %v", receipt, save, err)
	}
	firstInstall := external
	firstInstall.TargetPackageWasInstalled = false
	if receipt, save, err := phpExtensionInstalledPackageReceipt(firstInstall, pin, nil); err != nil || !save || !receipt.InstalledByAgent || !receipt.Installed {
		t.Fatalf("newly installed package did not receive an ownership receipt: %+v %v %v", receipt, save, err)
	}
	owned := external
	owned.OwnedPackageAtPlan = true
	previous := phpExtensionPackageReceipt{Package: pin.Name, Pin: pin, InstalledByAgent: true, Installed: true, SourceInstanceID: "php-original"}
	if receipt, save, err := phpExtensionInstalledPackageReceipt(owned, pin, &previous); err != nil || !save || receipt.SourceInstanceID != "php-original" {
		t.Fatalf("existing Agent ownership was not preserved: %+v %v %v", receipt, save, err)
	}
	changedPin := pin
	changedPin.Version += ".changed"
	if _, _, err := phpExtensionInstalledPackageReceipt(owned, changedPin, &previous); core.ErrorCode(err) != "extension_state_invalid" {
		t.Fatalf("changed Agent package pin was accepted: %v", err)
	}
}

func TestPHPExtensionPlansCannotRequestPhysicalPackageRemoval(t *testing.T) {
	e := testAgent(t)
	now := time.Now().UTC()
	id := "php-removal-test"
	planID := "plan_" + strings.Repeat("e", 48)
	plan := phpExtensionPlanRecord{
		Public: PHPModulePlan{
			PlanID: planID, ID: id, Operation: "uninstall", ModuleID: "mysqli",
			ExpectedRevision: 1, Resources: []string{InstanceResource(id), "host:packages"},
			CreatedAt: now, ExpiresAt: now.Add(phpExtensionPlanTTL), PackageRemovable: true,
		},
		PackageName: "php8.3-mysql", ExtensionDirSHA256: strings.Repeat("a", 64),
	}
	if err := e.save("plans", planID, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := e.loadPHPExtensionPlan(planID); core.ErrorCode(err) != "extension_plan_invalid" {
		t.Fatalf("protected plan with a physical package removal request was accepted: %v", err)
	}

	state := phpExtensionState{Version: 1, InstanceID: id, References: map[string][]string{}, Pending: &phpExtensionIntent{
		PlanID: planID, Phase: "prepared", Plan: plan,
		Previous:  Instance{ID: id, PluginID: "php-apt", Revision: 1},
		StartedAt: now, UpdatedAt: now,
	}}
	if err := e.savePHPExtensionState(state); err != nil {
		t.Fatal(err)
	}
	if _, err := e.loadPHPExtensionState(id); core.ErrorCode(err) != "extension_state_invalid" {
		t.Fatalf("pending journal with a physical package removal request was accepted: %v", err)
	}
}

func TestPHPExtensionBindingsKeepPackageAuthorizationSeparate(t *testing.T) {
	allowInstance, allowPackages := true, true
	manager := &Manager{deps: core.Dependencies{Authorize: func(_ core.Actor, scope, resource string) bool {
		if scope == "php:manage" {
			return allowInstance && resource == "software:php-main"
		}
		return allowPackages && scope == "host:packages" && resource == "host:packages"
	}}}
	var plan, apply *core.Binding
	for _, binding := range manager.phpExtensionBindings() {
		switch binding.Action.Name {
		case "php_extension_plan":
			plan = &binding
		case "php_extension_apply":
			apply = &binding
		}
	}
	if plan == nil || apply == nil || plan.Action.Scope != "php:manage" || !plan.Action.ReadOnly || plan.Action.Task || apply.Action.Scope != "php:manage" || apply.Action.ReadOnly || !apply.Action.Task {
		t.Fatal("PHP extension plan/apply capability metadata is incomplete")
	}
	raw := json.RawMessage(`{"id":"php-main","operation":"install","module_id":"pdo_mysql","expected_revision":3}`)
	resources, err := plan.Resources(context.Background(), core.Actor{ID: "operator"}, raw)
	if err != nil || strings.Join(resources, ",") != "software:php-main,host:packages" {
		t.Fatalf("plan resources = %v, %v", resources, err)
	}
	allowPackages = false
	if _, err = apply.Resources(context.Background(), core.Actor{ID: "operator"}, json.RawMessage(`{"id":"php-main","plan_id":"plan_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","expected_revision":3,"idempotency_key":"retry-key-0001"}`)); core.ErrorCode(err) != "forbidden" {
		t.Fatalf("apply accepted without the separate host:packages grant: %v", err)
	}
	allowPackages, allowInstance = true, false
	if _, err = plan.Resources(context.Background(), core.Actor{ID: "operator"}, raw); core.ErrorCode(err) != "forbidden" {
		t.Fatalf("plan accepted without the PHP instance grant: %v", err)
	}
	planProperties := plan.Action.InputSchema["properties"].(map[string]any)
	moduleEnum := planProperties["module_id"].(map[string]any)["enum"].([]string)
	if !slices.Contains(moduleEnum, "pdo_mysql") || slices.Contains(moduleEnum, "imagick") {
		t.Fatal("module enum does not match the trusted package allowlist")
	}
}

type phpExtensionAgentFixture struct {
	mu       sync.Mutex
	requests []agent.Request
}

func (f *phpExtensionAgentFixture) Validate(request agent.Request) error {
	if request.Operation != "php_action" {
		return agent.ErrUnauthorized
	}
	var input PHPInput
	if err := parse(request.Input, &input); err != nil {
		return err
	}
	if input.Command != "extension_apply" {
		return failure("invalid_input", "extension apply required", 400)
	}
	return nil
}

func (f *phpExtensionAgentFixture) Run(_ context.Context, request agent.Request) (agent.Result, error) {
	if err := f.Validate(request); err != nil {
		return agent.Result{ExitCode: -1}, err
	}
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	return agent.Result{Operation: request.Operation, ExitCode: 0, Stdout: `{"id":"php-main","module_id":"curl","operation":"install","enabled":true,"package_removed":false,"revision":4}`}, nil
}

func TestPHPExtensionJobRechecksRevokedPackageGrantBeforeAgentForward(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture := &phpExtensionAgentFixture{}
	socket := filepath.Join(t.TempDir(), "agent.sock")
	server, err := agent.NewUnixServer(socket, []uint32{uint32(os.Getuid())}, fixture)
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(ctx)
	t.Cleanup(func() { _ = server.Close() })
	client, err := agent.NewUnixClient(socket, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	packageGrant := true
	actor := core.Actor{ID: "operator"}
	manager := &Manager{deps: core.Dependencies{
		ResolveActor: func(_ context.Context, prior core.Actor) (core.Actor, error) { return actor, nil },
		Authorize: func(_ core.Actor, scope, resource string) bool {
			if scope == "php:manage" {
				return resource == "software:php-main"
			}
			return packageGrant && scope == "host:packages" && resource == "host:packages"
		},
	}}
	executor := NewPHPExtensionExecutor(manager, client)
	input, _ := json.Marshal(PHPInput{ID: "php-main", Command: "extension_apply", PlanID: "plan_" + strings.Repeat("a", 48), ExpectedRevision: 3, IdempotencyKey: "retry-key-0001"})
	request := agent.Request{ActorID: actor.ID, JobID: "php-extension-job", Operation: phpExtensionApplyJobOperation, Input: input}
	packageGrant = false // The queued task's package grant was revoked before execution.
	if _, err = executor.Run(ctx, request); core.ErrorCode(err) != "forbidden" {
		t.Fatalf("task with revoked package grant returned %v", err)
	}
	fixture.mu.Lock()
	calls := len(fixture.requests)
	fixture.mu.Unlock()
	if calls != 0 {
		t.Fatalf("revoked task reached the root Agent %d times", calls)
	}

	packageGrant = true
	result, err := executor.Run(ctx, request)
	if err != nil || result.ExitCode != 0 || !json.Valid([]byte(result.Stdout)) {
		t.Fatalf("authorized task was not forwarded: %+v %v", result, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.requests) != 1 || fixture.requests[0].Operation != "php_action" || fixture.requests[0].ActorID != actor.ID || fixture.requests[0].JobID != request.JobID {
		t.Fatalf("unexpected native forwarding request: %+v", fixture.requests)
	}
}

func TestPHPExtensionIntentDigestChangesWithProtectedIntent(t *testing.T) {
	intent := phpExtensionIntent{PlanID: "plan_" + strings.Repeat("b", 48), Phase: "prepared", StartedAt: time.Now().UTC(), Previous: Instance{ID: "php-main", Revision: 3}}
	first, err := phpExtensionIntentDigest(intent)
	if err != nil || !artifactHash.MatchString(first) {
		t.Fatalf("intent digest invalid: %q %v", first, err)
	}
	intent.Phase = "package_installed"
	second, err := phpExtensionIntentDigest(intent)
	if err != nil || first == second {
		t.Fatalf("intent phase change did not invalidate recovery marker: %q %q %v", first, second, err)
	}
}

func TestPHPExtensionPendingJournalAllowsOnlyExactReplayContext(t *testing.T) {
	e := testAgent(t)
	id := "php-journal-test"
	planID := "plan_" + strings.Repeat("c", 48)
	instance := phpExtensionRecoveryTestInstance(e, id, "journal-test", 7)
	if err := e.save("receipts", id, instance); err != nil {
		t.Fatal(err)
	}
	intent := phpExtensionRecoveryTestIntent(instance, planID, "install", "curl", "package_installed")
	state := phpExtensionState{Version: 1, InstanceID: id, References: map[string][]string{}, Pending: &intent}
	if err := e.savePHPExtensionState(state); err != nil {
		t.Fatal(err)
	}
	if err := e.ensurePHPExtensionFinalized(context.Background(), id); core.ErrorCode(err) != "extension_recovery_required" {
		t.Fatalf("unrelated mutation was not blocked by the pending PHP extension journal: %v", err)
	}
	request := PHPInput{ID: id, Command: "extension_apply", PlanID: planID, ExpectedRevision: 7, IdempotencyKey: "retry-key-0001"}
	replay, err := e.phpExtensionRecoveryContext(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	marker, ok := replay.Value(phpExtensionRecoveryMarkerKey{}).(phpExtensionRecoveryMarker)
	if !ok || marker.Executor != e || marker.InstanceID != id || marker.PlanID != planID || marker.ExpectedRevision != 7 || marker.IdempotencyKeySHA256 != hashPHPIdempotencyKey(request.IdempotencyKey) {
		t.Fatalf("typed retry did not produce a fully bound recovery marker: %+v", marker)
	}
	if err = e.ensurePHPExtensionFinalized(replay, id); err != nil {
		t.Fatalf("exact pending plan replay was not allowed: %v", err)
	}
	other, err := NewPrivilegedExecutor(e.config)
	if err != nil {
		t.Fatal(err)
	}
	marker.Executor = other
	wrongExecutor := context.WithValue(context.Background(), phpExtensionRecoveryMarkerKey{}, marker)
	if err = e.ensurePHPExtensionFinalized(wrongExecutor, id); core.ErrorCode(err) != "extension_recovery_required" {
		t.Fatalf("recovery marker from another executor bypassed the journal: %v", err)
	}
	badReplay, err := e.phpExtensionRecoveryContext(context.Background(), PHPInput{ID: id, Command: "extension_apply", PlanID: planID, ExpectedRevision: 6, IdempotencyKey: "retry-key-0001"})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.ensurePHPExtensionFinalized(badReplay, id); core.ErrorCode(err) != "extension_recovery_required" {
		t.Fatalf("wrong expected revision bypassed pending journal: %v", err)
	}
	state.Pending.Phase = "receipt_commit_started"
	if err = e.savePHPExtensionState(state); err != nil {
		t.Fatal(err)
	}
	if err = e.ensurePHPExtensionFinalized(replay, id); core.ErrorCode(err) != "extension_recovery_required" {
		t.Fatalf("changed intent digest reused an old replay marker: %v", err)
	}

	if err = e.ensurePHPExtensionFinalized(context.Background(), "missing-php"); err != nil {
		t.Fatalf("missing software receipt did not fast-return: %v", err)
	}
	nonPHP := Instance{ID: "redis-guard-test", PluginID: "redis", Revision: 1}
	if err = e.save("receipts", nonPHP.ID, nonPHP); err != nil {
		t.Fatal(err)
	}
	if err = e.ensurePHPExtensionFinalized(context.Background(), nonPHP.ID); err != nil {
		t.Fatalf("non-PHP mutation guard did not fast-return: %v", err)
	}
}

func TestPHPExtensionConfigFailurePersistsRecoveryUntilRollbackSucceeds(t *testing.T) {
	e := testAgent(t)
	id := "php-rollback-test"
	planID := "plan_" + strings.Repeat("d", 48)
	instance := Instance{ID: id, PluginID: "php-apt", Revision: 4, Ownership: "managed"}
	intent := phpExtensionIntent{
		PlanID: planID, Phase: "config_applied",
		Plan:     phpExtensionPlanRecord{Public: PHPModulePlan{PlanID: planID, ID: id, Operation: "install", ModuleID: "curl", ExpectedRevision: 4}},
		Previous: instance, Desired: Instance{ID: id, PluginID: "php-apt", Revision: 5, Ownership: "managed"},
		StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	state := phpExtensionState{Version: 1, InstanceID: id, References: map[string][]string{}, Pending: &intent}
	if err := e.savePHPExtensionState(state); err != nil {
		t.Fatal(err)
	}
	if err := e.persistPHPModuleConfigFailure(&state, &intent, errors.New("fixture rollback failure")); core.ErrorCode(err) != "extension_recovery_required" {
		t.Fatalf("failed rollback did not require recovery: %v", err)
	}
	reloaded, err := e.loadPHPExtensionState(id)
	if err != nil || reloaded.Pending == nil || reloaded.Pending.Phase != "rollback_required" {
		t.Fatalf("failed rollback intent was not durable: %+v %v", reloaded, err)
	}
	if err = e.persistPHPModuleConfigFailure(&reloaded, reloaded.Pending, nil); err != nil {
		t.Fatalf("successful prior-config restore did not settle the intent: %v", err)
	}
	reloaded, err = e.loadPHPExtensionState(id)
	if err != nil || reloaded.Pending != nil {
		t.Fatalf("settled rollback left a pending mutation: %+v %v", reloaded, err)
	}
}
