package sites

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type categoryAgentBarrier struct {
	started chan struct{}
	release <-chan struct{}
}

type categoryObservedContext struct {
	context.Context
	doneCalled chan struct{}
	once       sync.Once
}

func (c *categoryObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.doneCalled) })
	return c.Context.Done()
}

func (b *categoryAgentBarrier) wait(ctx context.Context) error {
	if b == nil {
		return nil
	}
	if b.started != nil {
		select {
		case b.started <- struct{}{}:
		default:
		}
	}
	if b.release == nil {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.release:
		return ctx.Err()
	}
}

// categoryCoordAgent is the controlled trusted-Agent boundary. Core's real
// SQLite state, authorization metadata, task commits and task worker run in
// these tests; barriers make the category/task interleavings deterministic.
type categoryCoordAgent struct {
	mu       sync.Mutex
	sites    map[string]Site
	backups  map[string]backupManifest
	barriers map[string]*categoryAgentBarrier
	calls    map[string]int
}

func newCategoryCoordAgent() *categoryCoordAgent {
	return &categoryCoordAgent{sites: map[string]Site{}, backups: map[string]backupManifest{}, barriers: map[string]*categoryAgentBarrier{}, calls: map[string]int{}}
}

func (a *categoryCoordAgent) Validate(agent.Request) error { return nil }

func (a *categoryCoordAgent) setBarrier(operation string, barrier *categoryAgentBarrier) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.barriers[operation] = barrier
}

func (a *categoryCoordAgent) site(id string) (Site, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	site, ok := a.sites[id]
	return site, ok
}

func (a *categoryCoordAgent) callCount(operation string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[operation]
}

func (a *categoryCoordAgent) Run(ctx context.Context, call agent.Request) (agent.Result, error) {
	var in request
	if err := strict(call.Input, &in); err != nil {
		return agent.Result{}, err
	}
	a.mu.Lock()
	a.calls[call.Operation]++
	barrier := a.barriers[call.Operation]
	var sites []Site
	if call.Operation == "site_list" {
		sites = make([]Site, 0, len(a.sites))
		for _, site := range a.sites {
			sites = append(sites, site)
		}
	}
	manifest := a.backups[in.BackupID]
	site, exists := a.sites[in.ID]
	a.mu.Unlock()
	if err := barrier.wait(ctx); err != nil {
		return agent.Result{}, err
	}
	switch call.Operation {
	case "site_list":
		return agent.Result{Data: encode(sites)}, nil
	case "site_get":
		if !exists {
			return agent.Result{}, failure("not_found", "Website not found", 404)
		}
		return agent.Result{Data: encode(site)}, nil
	case "site_backup_get":
		if manifest.Site.ID != in.ID || manifest.Backup.ID != in.BackupID {
			return agent.Result{}, failure("not_found", "Backup not found", 404)
		}
		return agent.Result{Data: encode(manifest)}, nil
	case "site_apply":
		if in.Site == nil || in.Site.ID != in.ID {
			return agent.Result{}, invalid("Website settings are required")
		}
		a.mu.Lock()
		updated := *in.Site
		if current, ok := a.sites[in.ID]; ok {
			updated.Revision = current.Revision + 1
		} else if updated.Revision == 0 {
			updated.Revision = 1
		}
		a.sites[in.ID] = updated
		a.mu.Unlock()
		return agent.Result{Data: encode(updated)}, nil
	case "site_backup_restore":
		if manifest.Site.ID != in.ID || manifest.Backup.ID != in.BackupID {
			return agent.Result{}, failure("not_found", "Backup not found", 404)
		}
		a.mu.Lock()
		updated := manifest.Site
		if current, ok := a.sites[in.ID]; ok {
			updated.Revision = current.Revision + 1
		}
		a.sites[in.ID] = updated
		a.mu.Unlock()
		return agent.Result{Data: encode(updated)}, nil
	case "site_delete":
		a.mu.Lock()
		delete(a.sites, in.ID)
		a.mu.Unlock()
		return agent.Result{Data: encode(map[string]any{"id": in.ID, "deleted": true})}, nil
	case "site_default_set", "site_config_set":
		return agent.Result{Data: encode(site)}, nil
	default:
		return agent.Result{}, agent.ErrUnauthorized
	}
}

type categoryCoordFixture struct {
	service  *core.Service
	actor    core.Actor
	provider *Provider
	agent    *categoryCoordAgent
}

func newCategoryCoordFixture(t *testing.T) categoryCoordFixture {
	t.Helper()
	service, err := core.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	secret, err := os.ReadFile(service.SetupSecretFile())
	if err != nil {
		t.Fatal(err)
	}
	actor, err := service.Setup(context.Background(), strings.TrimSpace(string(secret)), "category-admin", "category fixture password 2026!")
	if err != nil {
		t.Fatal(err)
	}
	agentFixture := newCategoryCoordAgent()
	provider, err := New(service.Runtime("sites"), Options{Agent: agentFixture})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.RegisterProvider(core.Bindings(provider.Bindings())); err != nil {
		t.Fatal(err)
	}
	if err = service.RegisterExecutor(JobOperationNames(), provider); err != nil {
		t.Fatal(err)
	}
	return categoryCoordFixture{service: service, actor: actor, provider: provider, agent: agentFixture}
}

func (f categoryCoordFixture) saveCategory(t *testing.T, id string) {
	t.Helper()
	if _, err := f.provider.deps.State.CompareAndSwap(context.Background(), "categories", id, 0, encode(Category{ID: id, Name: "Category " + id, Revision: 1})); err != nil {
		t.Fatal(err)
	}
}

func categoryTestSite(id, category string) Site {
	return Site{ID: id, Name: "Fixture website", Domains: []string{"fixture.example"}, WebInstance: "nginx", RunPath: ".", CategoryID: category, Status: "running", Index: []string{"index.html"}, DirectoryBindings: []DirectoryBinding{}, Proxies: []Proxy{}, Redirects: []Redirect{}}
}

func (f categoryCoordFixture) tokenActor(t *testing.T, name string, scopes, resources []string) (core.TokenCreated, core.Actor) {
	t.Helper()
	token, err := f.service.CreateToken(context.Background(), f.actor, core.TokenRequest{Name: name, Scopes: scopes, Resources: resources})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := f.service.AuthenticateToken(context.Background(), token.Token)
	if err != nil {
		t.Fatal(err)
	}
	return token, actor
}

func categorySubmitTask(t *testing.T, f categoryCoordFixture, actor core.Actor, action string, input any) string {
	t.Helper()
	result, err := f.service.Execute(context.Background(), actor, action, encode(input))
	if err != nil {
		t.Fatal(err)
	}
	job, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("%s did not return a task: %#v", action, result)
	}
	id, _ := job["id"].(string)
	if id == "" {
		t.Fatalf("%s task has no durable ID: %#v", action, job)
	}
	return id
}

func categoryWaitTask(t *testing.T, f categoryCoordFixture, id, expected string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for {
		job, err := f.provider.deps.Tasks.Get(ctx, f.actor, id)
		if err != nil {
			t.Fatal(err)
		}
		status, _ := job["status"].(string)
		if status == expected {
			return job
		}
		if status == "succeeded" || status == "failed" || status == "canceled" || status == "interrupted" {
			t.Fatalf("task %s ended %q; wanted %q", id, status, expected)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("task %s did not reach %q", id, expected)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func categoryDeleteInput(id string) map[string]any {
	return map[string]any{"id": id, "expected_revision": 1}
}

func TestCategoryDeleteWinsAgainstQueuedAssignment(t *testing.T) {
	f := newCategoryCoordFixture(t)
	f.saveCategory(t, "shared")
	listEntered, releaseList := make(chan struct{}, 1), make(chan struct{})
	f.agent.setBarrier("site_list", &categoryAgentBarrier{started: listEntered, release: releaseList})

	deleteDone := make(chan error, 1)
	go func() {
		_, err := f.service.Execute(context.Background(), f.actor, "sites_category_delete", encode(categoryDeleteInput("shared")))
		deleteDone <- err
	}()
	select {
	case <-listEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("category delete did not reach the controlled Agent list")
	}

	taskID := categorySubmitTask(t, f, f.actor, "sites_create", map[string]any{
		"id": "created-after-delete", "name": "Assigned site", "domains": []string{"assigned.example"}, "web_instance": "nginx", "category_id": "shared",
	})
	close(releaseList)
	if err := <-deleteDone; err != nil {
		t.Fatalf("category delete should commit before the waiting assignment: %v", err)
	}
	categoryWaitTask(t, f, taskID, "failed")
	if got := f.agent.callCount("site_apply"); got != 0 {
		t.Fatalf("waiting assignment reached Agent after its target category was deleted: calls=%d", got)
	}
	if _, err := f.provider.deps.State.Get(context.Background(), "categories", "shared"); !isNotFound(err) {
		t.Fatalf("category delete did not persist in SQLite: %v", err)
	}
}

func TestCategoryAssignmentWinsAgainstDeleteAndHiddenSiteBlocksDelete(t *testing.T) {
	f := newCategoryCoordFixture(t)
	f.saveCategory(t, "shared")
	applyEntered, releaseApply := make(chan struct{}, 1), make(chan struct{})
	listEntered := make(chan struct{}, 1)
	f.agent.setBarrier("site_apply", &categoryAgentBarrier{started: applyEntered, release: releaseApply})
	f.agent.setBarrier("site_list", &categoryAgentBarrier{started: listEntered})

	taskID := categorySubmitTask(t, f, f.actor, "sites_create", map[string]any{
		"id": "assigned-first", "name": "Assigned site", "domains": []string{"assigned.example"}, "web_instance": "nginx", "category_id": "shared",
	})
	select {
	case <-applyEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("site assignment did not reach the controlled Agent barrier")
	}
	observed := &categoryObservedContext{Context: context.Background(), doneCalled: make(chan struct{})}
	deleteDone := make(chan error, 1)
	go func() {
		_, err := f.provider.categoryAction(observed, f.actor, "sites_category_delete", encode(categoryDeleteInput("shared")))
		deleteDone <- err
	}()
	select {
	case <-observed.doneCalled:
	case <-time.After(3 * time.Second):
		t.Fatal("category delete did not wait on the held category gate")
	}
	select {
	case <-listEntered:
		t.Fatal("category delete reached Agent while assignment still held the category gate")
	default:
	}
	close(releaseApply)
	categoryWaitTask(t, f, taskID, "succeeded")
	if err := <-deleteDone; core.ErrorCode(err) != "category_in_use" {
		t.Fatalf("delete did not observe the committed assignment: %v", err)
	}
	if got, ok := f.agent.site("assigned-first"); !ok || got.CategoryID != "shared" {
		t.Fatalf("site assignment was not retained: %#v, found=%v", got, ok)
	}

	// The list is an authoritative all-sites view. A caller without access to
	// this site still cannot delete a category that the hidden site references.
	if _, err := f.provider.deps.State.Get(context.Background(), "categories", "shared"); err != nil {
		t.Fatal("category disappeared despite the live reference", err)
	}
	_, hiddenActor := f.tokenActor(t, "category manager", []string{"sites:write"}, []string{"site-category:shared"})
	if _, err := f.service.Execute(context.Background(), hiddenActor, "sites_category_delete", encode(categoryDeleteInput("shared"))); core.ErrorCode(err) != "category_in_use" {
		t.Fatalf("category delete did not refuse a hidden-site reference: %v", err)
	}
}

func TestCategoryRestoreUsesBackupTargetAndCanMoveOrClearOldCategory(t *testing.T) {
	f := newCategoryCoordFixture(t)
	f.saveCategory(t, "old")
	f.saveCategory(t, "target")
	oldSite := categoryTestSite("restore-move", "old")
	oldSite.Revision = 2
	f.agent.mu.Lock()
	f.agent.sites[oldSite.ID] = oldSite
	f.agent.backups["move-backup"] = backupManifest{Version: 1, Site: categoryTestSite(oldSite.ID, "target"), Backup: Backup{ID: "move-backup", SiteID: oldSite.ID}}
	f.agent.mu.Unlock()
	restoreEntered, releaseRestore := make(chan struct{}, 1), make(chan struct{})
	f.agent.setBarrier("site_backup_restore", &categoryAgentBarrier{started: restoreEntered, release: releaseRestore})

	taskID := categorySubmitTask(t, f, f.actor, "sites_backup_restore", map[string]any{"id": oldSite.ID, "expected_revision": oldSite.Revision, "backup_id": "move-backup"})
	select {
	case <-restoreEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("restore did not reach the controlled Agent barrier")
	}
	job, err := f.provider.deps.Tasks.Get(context.Background(), f.actor, taskID)
	if err != nil {
		t.Fatal(err)
	}
	resources, ok := job["resources"].([]string)
	if !ok || !containsString(resources, "site-category:target") || containsString(resources, "site-category:old") {
		t.Fatalf("restore task did not commit its actual target category resource: %#v", job["resources"])
	}
	close(releaseRestore)
	categoryWaitTask(t, f, taskID, "succeeded")
	if got, ok := f.agent.site(oldSite.ID); !ok || got.CategoryID != "target" {
		t.Fatalf("restore failed to move the old category assignment: %#v, found=%v", got, ok)
	}
	if _, err := f.service.Execute(context.Background(), f.actor, "sites_category_delete", encode(categoryDeleteInput("old"))); err != nil {
		t.Fatalf("old category remained in use after restore moved the assignment: %v", err)
	}

	// A backup with no category clears the assignment. Even if a stale site
	// receipt points at a removed category, restore can repair it without
	// requiring access to or existence of that old category.
	dangling := categoryTestSite("restore-clear", "missing-old")
	dangling.Revision = 1
	f.agent.mu.Lock()
	f.agent.sites[dangling.ID] = dangling
	f.agent.backups["clear-backup"] = backupManifest{Version: 1, Site: categoryTestSite(dangling.ID, ""), Backup: Backup{ID: "clear-backup", SiteID: dangling.ID}}
	f.agent.mu.Unlock()
	taskID = categorySubmitTask(t, f, f.actor, "sites_backup_restore", map[string]any{"id": dangling.ID, "expected_revision": dangling.Revision, "backup_id": "clear-backup"})
	job, err = f.provider.deps.Tasks.Get(context.Background(), f.actor, taskID)
	if err != nil {
		t.Fatal(err)
	}
	resources, _ = job["resources"].([]string)
	if containsString(resources, "site-category:missing-old") {
		t.Fatalf("restore acquired stale old-category authorization: %#v", resources)
	}
	categoryWaitTask(t, f, taskID, "succeeded")
	if got, ok := f.agent.site(dangling.ID); !ok || got.CategoryID != "" {
		t.Fatalf("restore failed to clear a dangling category assignment: %#v, found=%v", got, ok)
	}
}

func TestCategoryInheritedUpdateCarriesTargetAndRequiresCategoryReadAtSubmission(t *testing.T) {
	f := newCategoryCoordFixture(t)
	f.saveCategory(t, "inherited")
	site := categoryTestSite("inherited-site", "inherited")
	site.Revision = 1
	f.agent.mu.Lock()
	f.agent.sites[site.ID] = site
	f.agent.mu.Unlock()

	_, actor := f.tokenActor(t, "category-aware editor", []string{"sites:write", "sites:read", "software:read", "tasks:read"}, []string{Resource(site.ID), "site-category:inherited", "software:nginx"})
	applyEntered, releaseApply := make(chan struct{}, 1), make(chan struct{})
	f.agent.setBarrier("site_apply", &categoryAgentBarrier{started: applyEntered, release: releaseApply})
	taskID := categorySubmitTask(t, f, actor, "sites_update", map[string]any{"id": site.ID, "expected_revision": site.Revision, "description": "updated without category_id"})
	select {
	case <-applyEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("inherited update did not reach the controlled Agent barrier")
	}
	job, err := f.provider.deps.Tasks.Get(context.Background(), f.actor, taskID)
	if err != nil {
		t.Fatal(err)
	}
	resources, ok := job["resources"].([]string)
	if !ok || !containsString(resources, "site-category:inherited") {
		t.Fatalf("task authorization omitted the inherited target category: %#v", job["resources"])
	}
	close(releaseApply)
	categoryWaitTask(t, f, taskID, "succeeded")

	// Categorized updates now require access to their effective target category
	// during submission. They fail before creating a task, rather than queueing a
	// job that fails only after it starts.
	_, limited := f.tokenActor(t, "site-only editor", []string{"sites:write", "software:read"}, []string{Resource(site.ID), "software:nginx"})
	if _, err = f.service.Execute(context.Background(), limited, "sites_update", encode(map[string]any{"id": site.ID, "expected_revision": 2, "description": "should be denied before submit"})); core.ErrorCode(err) != "forbidden" {
		t.Fatalf("categorized update without category read was not rejected at submission: %v", err)
	}
	if got := f.agent.callCount("site_apply"); got != 1 {
		t.Fatalf("unauthorized update reached Agent: calls=%d", got)
	}
}

func TestCategoryClearRepairsDanglingAssignment(t *testing.T) {
	f := newCategoryCoordFixture(t)
	site := categoryTestSite("clear-dangling", "removed-category")
	site.Revision = 1
	f.agent.mu.Lock()
	f.agent.sites[site.ID] = site
	f.agent.mu.Unlock()
	_, actor := f.tokenActor(t, "site editor without category rights", []string{"sites:write", "software:read"}, []string{Resource(site.ID), "software:nginx"})
	taskID := categorySubmitTask(t, f, actor, "sites_update", map[string]any{"id": site.ID, "expected_revision": site.Revision, "category_id": ""})
	categoryWaitTask(t, f, taskID, "succeeded")
	if got, ok := f.agent.site(site.ID); !ok || got.CategoryID != "" {
		t.Fatalf("explicit clear could not repair a dangling category: %#v, found=%v", got, ok)
	}
}

func TestCategoryPermissionRevokedBeforeExecutionPreventsAgentWrite(t *testing.T) {
	f := newCategoryCoordFixture(t)
	f.saveCategory(t, "revocable")
	token, actor := f.tokenActor(t, "revocable category writer", []string{"sites:write", "sites:read", "software:read", "tasks:read"}, []string{Resource("revoked-site"), "site-category:revocable", "software:nginx"})
	lockedRelease, err := f.provider.lockCategoryMutations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lockedRelease()

	var resolved atomic.Int32
	firstProviderResolve := make(chan struct{}, 1)
	originalResolve := f.provider.deps.ResolveActor
	f.provider.deps.ResolveActor = func(ctx context.Context, prior core.Actor) (core.Actor, error) {
		resolved.Add(1)
		actor, resolveErr := originalResolve(ctx, prior)
		if resolveErr == nil {
			select {
			case firstProviderResolve <- struct{}{}:
			default:
			}
		}
		return actor, resolveErr
	}
	site := categoryTestSite("revoked-site", "revocable")
	runDone := make(chan error, 1)
	go func() {
		_, runErr := f.provider.Run(context.Background(), agent.Request{Operation: "site_apply", ActorID: actor.ID, Input: encode(request{ID: site.ID, Site: &site, Actor: actor.Public()})})
		runDone <- runErr
	}()
	select {
	case <-firstProviderResolve:
	case <-time.After(3 * time.Second):
		t.Fatal("execution did not reach Provider before the gate")
	}
	if err = f.service.RevokeToken(context.Background(), f.actor, token.ID); err != nil {
		t.Fatal(err)
	}
	lockedRelease()
	select {
	case err = <-runDone:
		if core.ErrorCode(err) != "unauthorized" {
			t.Fatalf("execution did not reject the revoked actor after gate acquisition: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoked execution remained blocked after gate release")
	}
	if got := f.agent.callCount("site_apply"); got != 0 {
		t.Fatalf("revoked actor reached the Agent after waiting on the gate: calls=%d", got)
	}
	if resolved.Load() < 2 {
		t.Fatalf("Provider did not refresh identity after gate acquisition: resolve calls=%d", resolved.Load())
	}
}

func TestCategoryGateWaitCancellationReleasesWithoutDeadlock(t *testing.T) {
	f := newCategoryCoordFixture(t)
	heldRelease, err := f.provider.lockCategoryMutations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, executeErr := f.provider.categoryAction(ctx, f.actor, "sites_category_save", encode(map[string]any{"id": "after-cancel", "name": "After cancel", "expected_revision": 0}))
		done <- executeErr
	}()
	cancel()
	select {
	case err = <-done:
		if !errorsIsContext(err) {
			t.Fatalf("canceled category gate wait returned an unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		heldRelease()
		t.Fatal("canceled category action remained blocked on the gate")
	}
	if _, err = f.provider.deps.State.Get(context.Background(), "categories", "after-cancel"); !isNotFound(err) {
		t.Fatalf("canceled action wrote category state: %v", err)
	}
	heldRelease()
	if _, err = f.service.Execute(context.Background(), f.actor, "sites_category_save", encode(map[string]any{"id": "after-cancel", "name": "After cancel", "expected_revision": 0})); err != nil {
		t.Fatalf("gate was not released for the next action: %v", err)
	}
}

func errorsIsContext(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
