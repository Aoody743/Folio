package sites

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type migrationTestRuntime struct {
	webs                  map[string]WebRuntime
	php                   map[string]PHPRuntime
	listenerOwner         string
	failCheck             string
	failReload            string
	failVerify            bool
	checks                []string
	listenerOwnersAtCheck []string
	reloads               []string
	recoveries            []string
	verified              []Site
	transactions          [][]string
}

type migrationProviderAgent struct {
	view        MigrationPlanView
	calls       []string
	applyCalled bool
}

func (a *migrationProviderAgent) Validate(agent.Request) error { return nil }

func (a *migrationProviderAgent) Run(_ context.Context, call agent.Request) (agent.Result, error) {
	a.calls = append(a.calls, call.Operation)
	if call.Operation == "site_migration_get" {
		return agent.Result{Data: encode(a.view)}, nil
	}
	if call.Operation == "site_migration_apply" {
		a.applyCalled = true
	}
	return agent.Result{Data: encode(map[string]any{"accepted": true})}, nil
}

func (r *migrationTestRuntime) Web(_ context.Context, id string) (WebRuntime, error) {
	value, ok := r.webs[id]
	if !ok {
		return WebRuntime{}, errors.New("unknown Web runtime")
	}
	return value, nil
}

func (r *migrationTestRuntime) PHP(_ context.Context, id string) (PHPRuntime, error) {
	value, ok := r.php[id]
	if !ok {
		return PHPRuntime{}, errors.New("unknown PHP runtime")
	}
	return value, nil
}

func (r *migrationTestRuntime) Check(_ context.Context, id string) error {
	r.checks = append(r.checks, id)
	r.listenerOwnersAtCheck = append(r.listenerOwnersAtCheck, r.listenerOwner)
	if r.failCheck == id {
		r.failCheck = ""
		return errors.New("injected configuration check failure")
	}
	return nil
}

func (r *migrationTestRuntime) Reload(_ context.Context, id string) error {
	r.reloads = append(r.reloads, id)
	if _, ok := r.webs[id]; ok {
		if err := r.reloadWebListener(id); err != nil {
			return err
		}
	}
	if r.failReload == id {
		r.failReload = ""
		return errors.New("injected reload failure")
	}
	return nil
}

func (r *migrationTestRuntime) WithRuntimeTransaction(ctx context.Context, ids []string, run func(context.Context) error) error {
	r.transactions = append(r.transactions, append([]string(nil), ids...))
	return run(ctx)
}

func (r *migrationTestRuntime) RecoverRuntime(_ context.Context, id string) error {
	r.recoveries = append(r.recoveries, id)
	if _, ok := r.webs[id]; ok {
		return r.reloadWebListener(id)
	}
	return nil
}

func (r *migrationTestRuntime) reloadWebListener(id string) error {
	web, ok := r.webs[id]
	if !ok {
		return errors.New("unknown Web runtime")
	}
	_, err := os.Lstat(filepath.Join(web.SnippetsDir, "migration-site.conf"))
	if err == nil {
		if r.listenerOwner != "" && r.listenerOwner != id {
			return errors.New("fixture bind address is already in use by another Web runtime")
		}
		r.listenerOwner = id
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if r.listenerOwner == id {
		r.listenerOwner = ""
	}
	return nil
}

func (r *migrationTestRuntime) VerifySite(_ context.Context, site Site) error {
	r.verified = append(r.verified, site)
	if r.failVerify {
		return errors.New("injected target verification failure")
	}
	if _, ok := r.webs[site.WebInstance]; !ok {
		return errors.New("migration verified the wrong Web instance")
	}
	return nil
}

func newMigrationTestExecutor(t *testing.T, advanced bool) (*AgentExecutor, *migrationTestRuntime, Site, string) {
	t.Helper()
	return newMigrationTestExecutorWithWebIDs(t, advanced, "web-source", "web-target")
}

func newMigrationTestExecutorWithWebIDs(t *testing.T, advanced bool, sourceID, targetID string) (*AgentExecutor, *migrationTestRuntime, Site, string) {
	t.Helper()
	base := t.TempDir()
	site, original := initializeMigrationTestFixture(t, base, advanced)
	site.WebInstance = sourceID
	if err := os.WriteFile(filepath.Join(base, "state", "sites", site.ID+".json"), encode(site), 0600); err != nil {
		t.Fatal(err)
	}
	e, runtime, openedSite := openMigrationTestExecutorWithWebIDs(t, base, sourceID, targetID)
	if openedSite.ID != site.ID {
		t.Fatal("migration fixture reopened a different site", openedSite.ID)
	}
	return e, runtime, site, original
}

func migrationTestConfig(base string) AgentConfig {
	return AgentConfig{Version: 1, RootDir: filepath.Join(base, "content"), StateDir: filepath.Join(base, "state"), AllowedPorts: []int{80, 443, 8080}}
}

func migrationTestRuntimeAt(t *testing.T, base string) *migrationTestRuntime {
	t.Helper()
	return migrationTestRuntimeAtWebIDs(t, base, "web-source", "web-target")
}

func migrationTestRuntimeAtWebIDs(t *testing.T, base, sourceID, targetID string) *migrationTestRuntime {
	t.Helper()
	sourcePrefix := filepath.Join(base, "source-web")
	targetPrefix := filepath.Join(base, "target-web")
	phpPrefix := filepath.Join(base, "php")
	webs := map[string]WebRuntime{}
	for _, value := range []struct{ id, prefix string }{{sourceID, sourcePrefix}, {targetID, targetPrefix}} {
		web := WebRuntime{ID: value.id, Program: "/usr/bin/true", MainConfig: filepath.Join(value.prefix, "nginx.conf"), SnippetsDir: filepath.Join(value.prefix, "snippets"), Prefix: value.prefix, WorkerUser: "nobody", WorkerGroup: "nogroup"}
		webs[value.id] = web
	}
	workerAccount, err := user.Lookup("nobody")
	if err != nil {
		t.Fatal("test requires an existing non-root nobody account", err)
	}
	workerGroup, err := user.LookupGroupId(workerAccount.Gid)
	if err != nil {
		t.Fatal(err)
	}
	for id, web := range webs {
		web.WorkerUser, web.WorkerGroup = workerAccount.Username, workerGroup.Name
		webs[id] = web
	}
	php := PHPRuntime{ID: "php-shared", Program: "/usr/bin/true", MainConfig: filepath.Join(phpPrefix, "php-fpm.conf"), PoolDir: filepath.Join(phpPrefix, "pools"), Prefix: phpPrefix}
	runtime := &migrationTestRuntime{webs: webs, php: map[string]PHPRuntime{php.ID: php}}
	for id, web := range webs {
		if _, err := os.Lstat(filepath.Join(web.SnippetsDir, "migration-site.conf")); err == nil {
			runtime.listenerOwner = id
		} else if !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	return runtime
}

func initializeMigrationTestFixture(t *testing.T, base string, advanced bool) (Site, string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("migration Agent integration requires a root-owned test filesystem")
	}
	rootDir := filepath.Join(base, "content")
	stateDir := filepath.Join(base, "state")
	sourcePrefix := filepath.Join(base, "source-web")
	targetPrefix := filepath.Join(base, "target-web")
	phpPrefix := filepath.Join(base, "php")
	for _, path := range []string{
		filepath.Join(sourcePrefix, "snippets"), filepath.Join(targetPrefix, "snippets"),
		filepath.Join(phpPrefix, "pools"), filepath.Join(stateDir, "sites"),
		filepath.Join(stateDir, "workers"), filepath.Join(stateDir, "certificates"),
		filepath.Join(rootDir, "migration-site", "public"), filepath.Join(rootDir, "migration-site", "auth"),
	} {
		if err := os.MkdirAll(path, 0750); err != nil {
			t.Fatal(err)
		}
	}
	runtime := migrationTestRuntimeAt(t, base)
	for _, web := range runtime.webs {
		for _, path := range []string{web.MainConfig} {
			if err := os.WriteFile(path, []byte("# fixture runtime\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	workerAccount, err := user.Lookup("nobody")
	if err != nil {
		t.Fatal("test requires an existing non-root nobody account", err)
	}
	workerGroup, err := user.LookupGroupId(workerAccount.Gid)
	if err != nil {
		t.Fatal(err)
	}
	php := runtime.php["php-shared"]
	if err = os.WriteFile(php.MainConfig, []byte("# fixture PHP runtime\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(php.PoolDir, "migration-site.conf"), []byte("protected PHP pool configuration\n"), 0600); err != nil {
		t.Fatal(err)
	}
	site := qaRenderSite()
	site.ID, site.Name = "migration-site", "Migration fixture"
	site.Domains = []string{"migration.example.test"}
	site.WebInstance, site.PHPInstance = "web-source", php.ID
	site.Revision, site.Status = 11, "running"
	site.DocumentRoot = filepath.Join(rootDir, site.ID, "public")
	site.Description = "preserve site settings"
	site.PHP = PHPSettings{OpenBasedir: true, Values: map[string]string{"memory_limit": "128M"}}
	site.Protection = Protection{Enabled: true, Username: "fixture-user", PasswordRef: "secret-reference-must-not-leak"}
	if advanced {
		site.ConfigurationMode = "advanced"
	}
	if err = os.WriteFile(filepath.Join(site.DocumentRoot, "index.html"), []byte("preserve content\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(rootDir, site.ID, "auth", "htpasswd"), []byte("fixture-user:$2y$protected-hash\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sourceConfig := "server {\n    # original source vhost\n}\n"
	if advanced {
		sourceConfig = "include /opt/old-web/private-snippet.conf;\n"
	}
	if err = os.WriteFile(filepath.Join(runtime.webs[site.WebInstance].SnippetsDir, site.ID+".conf"), []byte(sourceConfig), 0640); err != nil {
		t.Fatal(err)
	}
	worker := Identity{User: workerAccount.Username, Group: workerGroup.Name}
	if _, err = fmt.Sscan(workerAccount.Uid, &worker.UID); err != nil {
		t.Fatal(err)
	}
	if _, err = fmt.Sscan(workerAccount.Gid, &worker.GID); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(stateDir, "sites", site.ID+".json"), encode(site), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(stateDir, "workers", site.ID+".json"), encode(worker), 0600); err != nil {
		t.Fatal(err)
	}
	return site, sourceConfig
}

func openMigrationTestAgent(t *testing.T, base string) (*AgentExecutor, *migrationTestRuntime, Site, error) {
	t.Helper()
	return openMigrationTestAgentWithWebIDs(t, base, "web-source", "web-target")
}

func openMigrationTestAgentWithWebIDs(t *testing.T, base, sourceID, targetID string) (*AgentExecutor, *migrationTestRuntime, Site, error) {
	t.Helper()
	runtime := migrationTestRuntimeAtWebIDs(t, base, sourceID, targetID)
	e, err := NewAgentExecutor(migrationTestConfig(base), runtime)
	if err != nil {
		return nil, nil, Site{}, err
	}
	site, err := e.get("migration-site")
	if err != nil {
		_ = e.Close()
		return nil, nil, Site{}, err
	}
	return e, runtime, site, nil
}

func openMigrationTestExecutor(t *testing.T, base string) (*AgentExecutor, *migrationTestRuntime, Site) {
	t.Helper()
	e, runtime, site, err := openMigrationTestAgent(t, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := e.Close(); closeErr != nil {
			t.Errorf("close migration test executor: %v", closeErr)
		}
	})
	return e, runtime, site
}

func openMigrationTestExecutorWithWebIDs(t *testing.T, base, sourceID, targetID string) (*AgentExecutor, *migrationTestRuntime, Site) {
	t.Helper()
	e, runtime, site, err := openMigrationTestAgentWithWebIDs(t, base, sourceID, targetID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := e.Close(); closeErr != nil {
			t.Errorf("close migration test executor: %v", closeErr)
		}
	})
	return e, runtime, site
}

func runMigrationAgent(t *testing.T, e *AgentExecutor, operation string, in request) (agent.Result, error) {
	t.Helper()
	if in.Actor.ID == "" {
		in.Actor = core.Actor{ID: "migration-owner", Kind: "human", Name: "Migration owner"}
	}
	return e.Run(context.Background(), agent.Request{Operation: operation, ActorID: in.Actor.ID, Input: encode(in)})
}

func createMigrationTestPlan(t *testing.T, e *AgentExecutor, site Site, resetAdvanced bool) MigrationPlanView {
	t.Helper()
	targetID := "web-target"
	if runtime, ok := e.runtimes.(*migrationTestRuntime); ok {
		for id := range runtime.webs {
			if id != site.WebInstance {
				targetID = id
				break
			}
		}
	}
	result, err := runMigrationAgent(t, e, "site_migration_plan", request{ID: site.ID, ExpectedRevision: site.Revision, TargetWebInstance: targetID, ResetAdvancedConfig: resetAdvanced})
	if err != nil {
		t.Fatal(err)
	}
	var view MigrationPlanView
	if err = json.Unmarshal(result.Data, &view); err != nil {
		t.Fatal(err)
	}
	if !validMigrationPlanID(view.ID) {
		t.Fatalf("Agent returned an invalid protected migration plan identity: %+v", view)
	}
	return view
}

func TestWebMigrationPlanApplyPreservesContentWorkerPHPAndJournalRefs(t *testing.T) {
	e, runtime, site, _ := newMigrationTestExecutor(t, false)
	view := createMigrationTestPlan(t, e, site, false)
	if view.SourceWebInstance != "web-source" || view.TargetWebInstance != "web-target" || view.PHPInstance != "php-shared" || view.ExpectedRevision != site.Revision {
		t.Fatalf("migration plan omitted its reviewed runtime/revision bindings: %+v", view)
	}
	planResult, err := runMigrationAgent(t, e, "site_migration_get", request{PlanID: view.ID})
	if err != nil || strings.Contains(string(planResult.Data), "secret-reference-must-not-leak") {
		t.Fatal("migration plan response exposed a secret reference", err)
	}
	planInfo, err := e.state.Stat(filepath.Join("migration-plans", view.ID+".json"))
	if err != nil || planInfo.Mode().Perm() != 0600 {
		t.Fatal("migration plan was not stored as a private Agent record", err)
	}
	plan, err := e.loadMigrationPlan(view.ID)
	if err != nil {
		t.Fatal(err)
	}
	refs, _, _, _, err := e.journalSites(context.Background(), "site_migration_apply", request{PlanID: view.ID, ExpectedRevision: site.Revision})
	if err != nil || len(refs) != 2 || refs[0].WebRuntime != "web-source" || refs[1].WebRuntime != "web-target" || refs[0].PHPRuntime != "php-shared" || refs[1].PHPRuntime != "php-shared" {
		t.Fatal("migration journal did not bind old/new Web plus unchanged PHP authority", refs, err)
	}
	files, ids, err := e.journalPaths(context.Background(), websiteJournal{Operation: "site_migration_apply", MigrationPlanID: plan.ID, Sites: refs})
	if err != nil || !slices.Equal(ids, []string{"php-shared", "web-source", "web-target"}) || len(files) < 6 {
		t.Fatal("migration journal omitted one side or its PHP/config backup", ids, files, err)
	}
	contentBefore, err := os.ReadFile(filepath.Join(site.DocumentRoot, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	workerBefore, err := e.state.ReadFile(filepath.Join("workers", site.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	phpConfigBefore, err := os.ReadFile(filepath.Join(runtime.php["php-shared"].PoolDir, site.ID+".conf"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runMigrationAgent(t, e, "site_migration_apply", request{PlanID: view.ID, ExpectedRevision: site.Revision})
	if err != nil {
		t.Fatal(err)
	}
	var migrated Site
	if err = json.Unmarshal(result.Data, &migrated); err != nil {
		t.Fatal(err)
	}
	if migrated.Revision != site.Revision+1 || migrated.WebInstance != "web-target" || migrated.PHPInstance != site.PHPInstance || migrated.Description != site.Description || migrated.ConfigurationMode != "managed" {
		t.Fatalf("migration did not preserve settings and advance the site CAS: %+v", migrated)
	}
	if _, err = os.Stat(filepath.Join(runtime.webs["web-source"].SnippetsDir, site.ID+".conf")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("source Web vhost remains after migration", err)
	}
	targetConfig, err := os.ReadFile(filepath.Join(runtime.webs["web-target"].SnippetsDir, site.ID+".conf"))
	if err != nil || !strings.Contains(string(targetConfig), "migration.example.test") || strings.Contains(string(targetConfig), "/opt/old-web") {
		t.Fatal("target Web did not receive safe rendered configuration", err)
	}
	contentAfter, _ := os.ReadFile(filepath.Join(site.DocumentRoot, "index.html"))
	workerAfter, _ := e.state.ReadFile(filepath.Join("workers", site.ID+".json"))
	phpConfigAfter, _ := os.ReadFile(filepath.Join(runtime.php["php-shared"].PoolDir, site.ID+".conf"))
	if !slices.Equal(contentBefore, contentAfter) || !slices.Equal(workerBefore, workerAfter) || !slices.Equal(phpConfigBefore, phpConfigAfter) {
		t.Fatal("migration changed content, worker ownership or PHP configuration")
	}
	if !slices.Equal(runtime.checks, []string{"php-shared", "web-source", "web-target"}) || !slices.Equal(runtime.reloads, []string{"web-source", "web-target", "php-shared"}) || runtime.listenerOwner != "web-target" || !slices.Equal(runtime.listenerOwnersAtCheck, []string{"web-source", "web-source", "web-source"}) {
		t.Fatal("migration did not validate and reload PHP plus both Web instances", runtime.checks, runtime.reloads)
	}
	if len(runtime.verified) != 1 || runtime.verified[0].WebInstance != "web-target" {
		t.Fatal("migration did not verify the target Web revision", runtime.verified)
	}
	entries, err := e.state.Open("transactions")
	if err != nil {
		t.Fatal(err)
	}
	defer entries.Close()
	values, readErr := entries.ReadDir(-1)
	if readErr != nil || len(values) != 0 {
		t.Fatal("successful migration retained a pending WAL", values, readErr)
	}
	legacy := migrated
	legacy.WebInstance = "web-source"
	if _, err = runMigrationAgent(t, e, "site_apply", request{ID: migrated.ID, ExpectedRevision: migrated.Revision, Site: &legacy}); err == nil || !strings.Contains(err.Error(), "explicit migration plan") {
		t.Fatal("sites_update/site_apply bypassed the explicit Web migration plan", err)
	}
}

func TestWebMigrationReloadAndRollbackOrdersIgnoreRuntimeNames(t *testing.T) {
	for _, names := range []struct {
		name     string
		sourceID string
		targetID string
	}{
		{name: "target sorts before source", sourceID: "web-zsource", targetID: "web-atarget"},
		{name: "source sorts before target", sourceID: "web-asource", targetID: "web-ztarget"},
	} {
		t.Run(names.name+"/success", func(t *testing.T) {
			e, runtime, site, _ := newMigrationTestExecutorWithWebIDs(t, false, names.sourceID, names.targetID)
			view := createMigrationTestPlan(t, e, site, false)
			result, err := runMigrationAgent(t, e, "site_migration_apply", request{PlanID: view.ID, ExpectedRevision: site.Revision})
			if err != nil {
				t.Fatal(err)
			}
			var migrated Site
			if err = json.Unmarshal(result.Data, &migrated); err != nil {
				t.Fatal(err)
			}
			wantChecks := []string{"php-shared", names.sourceID, names.targetID}
			slices.Sort(wantChecks)
			if !slices.Equal(runtime.checks, wantChecks) || !slices.Equal(runtime.reloads, []string{names.sourceID, names.targetID, "php-shared"}) || runtime.listenerOwner != names.targetID {
				t.Fatal("migration runtime order depended on lexical runtime IDs", runtime.checks, runtime.reloads, runtime.listenerOwner)
			}
			if len(runtime.transactions) != 1 || !slices.Equal(runtime.transactions[0], wantChecks) {
				t.Fatal("migration did not retain sorted, deduplicated runtime lock authority", runtime.transactions)
			}
			if migrated.WebInstance != names.targetID || migrated.Revision != site.Revision+1 {
				t.Fatal("migration did not commit the reviewed target and revision", migrated)
			}
		})

		t.Run(names.name+"/destination reload failure", func(t *testing.T) {
			e, runtime, site, original := newMigrationTestExecutorWithWebIDs(t, false, names.sourceID, names.targetID)
			view := createMigrationTestPlan(t, e, site, false)
			runtime.failReload = names.targetID
			if _, err := runMigrationAgent(t, e, "site_migration_apply", request{PlanID: view.ID, ExpectedRevision: site.Revision}); core.ErrorCode(err) != "reload_failed" {
				t.Fatal("injected destination reload failure was not returned", err)
			}
			wantChecks := []string{"php-shared", names.sourceID, names.targetID}
			slices.Sort(wantChecks)
			if !slices.Equal(runtime.checks, wantChecks) || !slices.Equal(runtime.reloads, []string{names.sourceID, names.targetID}) {
				t.Fatal("migration checks or effects used an incorrect order", runtime.checks, runtime.reloads)
			}
			if !slices.Equal(runtime.recoveries, []string{names.targetID, names.sourceID, "php-shared"}) || runtime.listenerOwner != names.sourceID {
				t.Fatal("rollback did not release the target before restoring the source listener", runtime.recoveries, runtime.listenerOwner)
			}
			source, err := os.ReadFile(filepath.Join(runtime.webs[names.sourceID].SnippetsDir, site.ID+".conf"))
			if err != nil || string(source) != original {
				t.Fatal("rollback did not restore the original source configuration", err)
			}
			if _, err = os.Stat(filepath.Join(runtime.webs[names.targetID].SnippetsDir, site.ID+".conf")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("rollback left a destination vhost behind", err)
			}
			current, err := e.get(site.ID)
			if err != nil || current.WebInstance != names.sourceID || current.Revision != site.Revision {
				t.Fatal("rollback changed protected site metadata", current, err)
			}
		})
	}
}

func TestMigrationJournalRequiresSourceReceiptToMatchRecoveryOrder(t *testing.T) {
	e, runtime, site, _ := newMigrationTestExecutor(t, false)
	view := createMigrationTestPlan(t, e, site, false)
	in := request{Actor: core.Actor{ID: "migration-owner", Kind: "human", Name: "Migration owner"}, PlanID: view.ID, ExpectedRevision: site.Revision}
	ctx, finish, err := e.beginWebsiteJournal(context.Background(), "site_migration_apply", in)
	if err != nil {
		t.Fatal(err)
	}
	active := ctx.Value(websiteJournalKey{}).(*activeWebsiteJournal)
	journal := active.record
	if err = e.validateJournal(ctx, journal, journal.ID+".json"); err != nil {
		t.Fatal("fresh migration journal failed validation", err)
	}
	swapped := journal
	swapped.Sites = append([]journalSite(nil), journal.Sites...)
	swapped.Sites[0], swapped.Sites[1] = swapped.Sites[1], swapped.Sites[0]
	if err = e.validateJournal(ctx, swapped, swapped.ID+".json"); err == nil {
		t.Fatal("migration journal accepted a source/target order that disagrees with its protected site receipt")
	}
	tamperedTarget := journal
	tamperedTarget.Sites = append([]journalSite(nil), journal.Sites...)
	tamperedTarget.Sites[1].WebRuntime = "web-unrelated"
	if err = e.validateJournal(ctx, tamperedTarget, tamperedTarget.ID+".json"); err == nil {
		t.Fatal("migration journal accepted a target runtime different from its protected plan")
	}
	operationErr := errors.New("force journal rollback after order validation")
	finish(&operationErr)
	if !slices.Equal(active.record.RuntimeIDs, []string{"php-shared", "web-source", "web-target"}) {
		t.Fatal("migration journal changed sorted runtime authority", active.record.RuntimeIDs)
	}
	if !slices.Equal(runtime.recoveries, []string{"web-target", "web-source", "php-shared"}) || runtime.listenerOwner != "web-source" {
		t.Fatal("active journal rollback did not follow the validated migration direction", runtime.recoveries, runtime.listenerOwner)
	}
}

func TestWebMigrationAdvancedResetKeepsPrivateOriginalAndRollbackRestoresConfig(t *testing.T) {
	e, _, site, original := newMigrationTestExecutor(t, true)
	if _, err := runMigrationAgent(t, e, "site_migration_plan", request{ID: site.ID, ExpectedRevision: site.Revision, TargetWebInstance: "web-target"}); core.ErrorCode(err) != "advanced_migration_choice_required" {
		t.Fatal("advanced config migration was planned without an explicit reset choice", err)
	}
	view := createMigrationTestPlan(t, e, site, true)
	if !view.ResetAdvancedConfig || view.ConfigurationMode != "advanced" {
		t.Fatal("review omitted the explicit advanced-configuration reset", view)
	}
	result, err := runMigrationAgent(t, e, "site_migration_apply", request{PlanID: view.ID, ExpectedRevision: site.Revision})
	if err != nil {
		t.Fatal(err)
	}
	var migrated Site
	if err = json.Unmarshal(result.Data, &migrated); err != nil || migrated.ConfigurationMode != "managed" {
		t.Fatal("advanced config was not reset to safe managed output", migrated, err)
	}
	backup, err := e.state.ReadFile(filepath.Join("migration-plans", view.ID+".original"))
	if err != nil || string(backup) != original {
		t.Fatal("committed migration did not retain its root-private original advanced configuration", err)
	}

	e2, runtime2, site2, original2 := newMigrationTestExecutor(t, true)
	view2 := createMigrationTestPlan(t, e2, site2, true)
	runtime2.failCheck = "web-target"
	if _, err = runMigrationAgent(t, e2, "site_migration_apply", request{PlanID: view2.ID, ExpectedRevision: site2.Revision}); core.ErrorCode(err) != "configuration_rejected" {
		t.Fatal("injected migration check failure was not returned", err)
	}
	sourceAfter, _ := os.ReadFile(filepath.Join(runtime2.webs["web-source"].SnippetsDir, site2.ID+".conf"))
	if string(sourceAfter) != original2 {
		t.Fatal("failed migration did not restore the original source configuration", string(sourceAfter))
	}
	if _, err = os.Stat(filepath.Join(runtime2.webs["web-target"].SnippetsDir, site2.ID+".conf")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("failed migration left a target vhost behind", err)
	}
	if _, err = e2.state.Stat(filepath.Join("migration-plans", view2.ID+".original")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("failed migration retained a backup for an uncommitted reset", err)
	}
	current, err := e2.get(site2.ID)
	if err != nil || current.Revision != site2.Revision || current.WebInstance != site2.WebInstance || current.ConfigurationMode != "advanced" {
		t.Fatal("failed migration did not restore protected website metadata", current, err)
	}
	if !slices.Equal(runtime2.recoveries, []string{"web-target", "web-source", "php-shared"}) || runtime2.listenerOwner != "web-source" {
		t.Fatal("migration rollback did not recover each journaled runtime", runtime2.recoveries)
	}
}

// These subprocess tests exercise Agent WAL recovery against protected runtime
// fixture directories; they do not start or claim to validate a real Web server.
func TestWebMigrationFixtureWALSubprocessSIGKILLRecoversCommitAndRollback(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("migration WAL subprocess test requires a root-owned fixture")
	}
	for _, phase := range []string{"uncommitted", "committed-before-cleanup"} {
		t.Run(phase, func(t *testing.T) {
			base := t.TempDir()
			site, original := initializeMigrationTestFixture(t, base, false)
			e, _, openedSite, err := openMigrationTestAgent(t, base)
			if err != nil {
				t.Fatal(err)
			}
			view := createMigrationTestPlan(t, e, openedSite, false)
			if err = e.Close(); err != nil {
				t.Fatal("close plan creator before subprocess recovery", err)
			}

			cmd := exec.Command(os.Args[0], "-test.run=^TestWebMigrationFixtureWALCrashChild$")
			cmd.Env = append(os.Environ(),
				"POWERLINUX_SITE_MIGRATION_CHILD_BASE="+base,
				"POWERLINUX_SITE_MIGRATION_CHILD_PLAN="+view.ID,
				"POWERLINUX_SITE_MIGRATION_CHILD_PHASE="+phase,
			)
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			waitJournalBarrier(t, filepath.Join(base, "ready"))
			if err = cmd.Process.Signal(syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			if err = cmd.Wait(); err == nil {
				t.Fatal("SIGKILLed migration subprocess returned success")
			}
			cmd.Process = nil

			restarted, runtime, recoveredSite, err := openMigrationTestAgent(t, base)
			if err != nil {
				t.Fatal("startup recovery failed after migration SIGKILL", err)
			}
			t.Cleanup(func() {
				if closeErr := restarted.Close(); closeErr != nil {
					t.Errorf("close recovered migration Agent: %v", closeErr)
				}
			})
			sourcePath := filepath.Join(runtime.webs["web-source"].SnippetsDir, site.ID+".conf")
			targetPath := filepath.Join(runtime.webs["web-target"].SnippetsDir, site.ID+".conf")
			transactionEntries, readErr := os.ReadDir(filepath.Join(base, "state", "transactions"))
			if readErr != nil || len(transactionEntries) != 0 {
				t.Fatal("startup did not finish and remove the migration WAL", readErr, transactionEntries)
			}
			if phase == "uncommitted" {
				if recoveredSite.WebInstance != site.WebInstance || recoveredSite.Revision != site.Revision {
					t.Fatal("uncommitted migration changed protected site metadata", recoveredSite)
				}
				if source, readErr := os.ReadFile(sourcePath); readErr != nil || string(source) != original {
					t.Fatal("uncommitted migration did not restore source vhost bytes", readErr)
				}
				if _, readErr := os.Stat(targetPath); !errors.Is(readErr, fs.ErrNotExist) {
					t.Fatal("uncommitted migration left a target vhost behind", readErr)
				}
				if !slices.Equal(runtime.recoveries, []string{"web-target", "web-source", "php-shared"}) || runtime.listenerOwner != "web-source" {
					t.Fatal("rollback did not recover every journaled runtime", runtime.recoveries)
				}
			} else {
				if recoveredSite.WebInstance != "web-target" || recoveredSite.Revision != site.Revision+1 {
					t.Fatal("committed migration was rolled back during cleanup recovery", recoveredSite)
				}
				if _, readErr := os.Stat(sourcePath); !errors.Is(readErr, fs.ErrNotExist) {
					t.Fatal("committed cleanup restored a removed source vhost", readErr)
				}
				if target, readErr := os.ReadFile(targetPath); readErr != nil || !strings.Contains(string(target), "migration.example.test") {
					t.Fatal("committed target vhost was lost during cleanup recovery", readErr)
				}
				if len(runtime.recoveries) != 0 {
					t.Fatal("committed cleanup rolled back or restarted runtimes", runtime.recoveries)
				}
			}
			content, err := os.ReadFile(filepath.Join(base, "content", site.ID, "public", "index.html"))
			if err != nil || string(content) != "preserve content\n" {
				t.Fatal("migration WAL recovery changed website content", err)
			}
		})
	}
}

func TestWebMigrationFixtureWALCrashChild(t *testing.T) {
	base := os.Getenv("POWERLINUX_SITE_MIGRATION_CHILD_BASE")
	if base == "" {
		t.Skip("subprocess helper")
	}
	phase := os.Getenv("POWERLINUX_SITE_MIGRATION_CHILD_PHASE")
	if phase != "uncommitted" && phase != "committed-before-cleanup" {
		t.Fatal("unknown migration crash phase", phase)
	}
	e, _, site, err := openMigrationTestAgent(t, base)
	if err != nil {
		t.Fatal(err)
	}
	in := request{
		Actor:  core.Actor{ID: "migration-owner", Kind: "human", Name: "Migration owner"},
		PlanID: os.Getenv("POWERLINUX_SITE_MIGRATION_CHILD_PLAN"), ExpectedRevision: site.Revision,
	}
	err = e.withMutationRuntimes(context.Background(), "site_migration_apply", in, func(runtimeCtx context.Context) error {
		locked, _, beginErr := e.beginWebsiteJournal(runtimeCtx, "site_migration_apply", in)
		if beginErr != nil {
			return beginErr
		}
		if _, applyErr := e.applyMigration(locked, in); applyErr != nil {
			return applyErr
		}
		if phase == "committed-before-cleanup" {
			if commitErr := commitWebsiteJournal(locked); commitErr != nil {
				return commitErr
			}
		}
		writeJournalBarrier(t, filepath.Join(base, "ready"))
		select {}
	})
	t.Fatalf("migration crash child unexpectedly returned: %v", err)
}

func TestWebMigrationRejectsRuntimeConfigAndTargetConflicts(t *testing.T) {
	t.Run("identity-proof", func(t *testing.T) {
		e, runtime, site, _ := newMigrationTestExecutor(t, false)
		view := createMigrationTestPlan(t, e, site, false)
		changed := runtime.webs["web-target"]
		changed.Program = "/usr/bin/false"
		runtime.webs["web-target"] = changed
		if _, err := runMigrationAgent(t, e, "site_migration_apply", request{PlanID: view.ID, ExpectedRevision: site.Revision}); core.ErrorCode(err) != "migration_runtime_changed" {
			t.Fatal("migration accepted a target runtime whose protected identity changed", err)
		}
	})
	t.Run("source-config-proof", func(t *testing.T) {
		e, runtime, site, _ := newMigrationTestExecutor(t, false)
		view := createMigrationTestPlan(t, e, site, false)
		if err := os.WriteFile(filepath.Join(runtime.webs["web-source"].SnippetsDir, site.ID+".conf"), []byte("changed after review\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := runMigrationAgent(t, e, "site_migration_apply", request{PlanID: view.ID, ExpectedRevision: site.Revision}); core.ErrorCode(err) != "migration_source_config_changed" {
			t.Fatal("migration accepted a source vhost whose reviewed hash changed", err)
		}
	})
	t.Run("domain-conflict", func(t *testing.T) {
		e, _, site, _ := newMigrationTestExecutor(t, false)
		conflict := site
		conflict.ID, conflict.WebInstance, conflict.Revision = "target-conflict", "web-target", 1
		conflict.Domains = []string{"migration.example.test"}
		if err := atomicRoot(e.state, filepath.Join("sites", conflict.ID+".json"), encode(conflict), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := runMigrationAgent(t, e, "site_migration_plan", request{ID: site.ID, ExpectedRevision: site.Revision, TargetWebInstance: "web-target"}); core.ErrorCode(err) != "domain_conflict" {
			t.Fatal("migration plan ignored a domain conflict on the target Web instance", err)
		}
	})
	t.Run("default-conflict", func(t *testing.T) {
		e, _, site, _ := newMigrationTestExecutor(t, false)
		site.Default = true
		if err := atomicRoot(e.state, filepath.Join("sites", site.ID+".json"), encode(site), 0600); err != nil {
			t.Fatal(err)
		}
		conflict := site
		conflict.ID, conflict.WebInstance, conflict.Revision, conflict.Default = "target-default", "web-target", 1, true
		conflict.Domains = []string{"other.example.test"}
		if err := atomicRoot(e.state, filepath.Join("sites", conflict.ID+".json"), encode(conflict), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := runMigrationAgent(t, e, "site_migration_plan", request{ID: site.ID, ExpectedRevision: site.Revision, TargetWebInstance: "web-target"}); core.ErrorCode(err) != "default_site_conflict" {
			t.Fatal("migration plan ignored a default-site conflict on target Web", err)
		}
	})
}

func TestWebMigrationResourceAuthorizationCoversSiteSourceTargetAndPHP(t *testing.T) {
	view := MigrationPlanView{SiteID: "site-one", SourceWebInstance: "web-source", TargetWebInstance: "web-target", PHPInstance: "php-shared"}
	all := map[string]bool{
		"sites:write:site:site-one":         true,
		"software:read:software:web-source": true,
		"software:read:software:web-target": true,
		"software:read:software:php-shared": true,
	}
	actor := core.Actor{ID: "migration-owner"}
	p := &Provider{deps: core.Dependencies{Authorize: func(_ core.Actor, action, resource string) bool { return all[action+":"+resource] }}}
	resources, err := p.authorizeMigrationView(actor, view, true)
	if err != nil || !slices.Equal(resources, []string{"site:site-one", "software:web-source", "software:web-target", "software:php-shared"}) {
		t.Fatal("migration plan did not require and retain every protected resource", resources, err)
	}
	for _, deniedResource := range []string{"site:site-one", "software:web-source", "software:web-target", "software:php-shared"} {
		delete(all, "sites:write:"+deniedResource)
		delete(all, "software:read:"+deniedResource)
		if _, err = p.authorizeMigrationView(actor, view, true); core.ErrorCode(err) != "forbidden" {
			t.Fatalf("migration was authorized without %s", deniedResource)
		}
		all["sites:write:"+deniedResource] = true
		all["software:read:"+deniedResource] = true
	}
}

func TestWebMigrationTaskRechecksAllResourcesBeforeAgentApply(t *testing.T) {
	view := MigrationPlanView{ID: "m-0123456789abcdef0123456789abcdef", SiteID: "site-one", ExpectedRevision: 8, SourceWebInstance: "web-source", TargetWebInstance: "web-target", PHPInstance: "php-shared"}
	agentFixture := &migrationProviderAgent{view: view}
	actor := core.Actor{ID: "migration-owner", Kind: "human", Name: "Migration owner"}
	allowed := map[string]bool{
		"sites:write:site:site-one":         true,
		"software:read:software:web-source": true,
		"software:read:software:web-target": true,
		"software:read:software:php-shared": true,
	}
	provider := &Provider{
		options: Options{Agent: agentFixture},
		deps: core.Dependencies{
			ResolveActor: func(context.Context, core.Actor) (core.Actor, error) { return actor, nil },
			Authorize:    func(_ core.Actor, action, resource string) bool { return allowed[action+":"+resource] },
		},
	}
	job := agent.Request{Operation: "site_migration_apply", ActorID: actor.ID, Input: encode(request{Actor: actor, PlanID: view.ID, ExpectedRevision: view.ExpectedRevision})}
	delete(allowed, "software:read:software:web-target")
	if _, err := provider.Run(context.Background(), job); core.ErrorCode(err) != "forbidden" || agentFixture.applyCalled {
		t.Fatal("migration task reached the privileged Agent after target-resource access was revoked", err, agentFixture.calls)
	}
	allowed["software:read:software:web-target"] = true
	if _, err := provider.Run(context.Background(), job); err != nil || !agentFixture.applyCalled {
		t.Fatal("authorized migration task did not reach the privileged Agent", err, agentFixture.calls)
	}
}
