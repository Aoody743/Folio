package plugins

import (
	"context"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type packageGuardFixture struct {
	plan         Plan
	plans        atomic.Int32
	applies      atomic.Int32
	last         agent.Request
	contextValue any
}

func (f *packageGuardFixture) Validate(request agent.Request) error {
	if request.Operation != "plugin_apply" && request.Operation != "plugin_plan" {
		return agent.ErrUnauthorized
	}
	return nil
}
func (f *packageGuardFixture) Run(ctx context.Context, request agent.Request) (agent.Result, error) {
	if err := f.Validate(request); err != nil {
		return agent.Result{}, err
	}
	f.last = request
	f.contextValue = ctx.Value(packageGuardContextKey{})
	if request.Operation == "plugin_plan" {
		f.plans.Add(1)
		raw, _ := json.Marshal(f.plan)
		return agent.Result{Operation: request.Operation, ExitCode: 0, Stdout: string(raw)}, nil
	}
	f.applies.Add(1)
	return agent.Result{Operation: request.Operation, ExitCode: 0}, nil
}

type packageGuardContextKey struct{}

func TestPackagePlanAndExecutionRequireSeparateHostScope(t *testing.T) {
	service, err := core.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	secret, _ := os.ReadFile(service.SetupSecretFile())
	admin, err := service.Setup(context.Background(), strings.TrimSpace(string(secret)), "package-admin", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterScopes("host:packages"); err != nil {
		t.Fatal(err)
	}
	catalog, err := BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := catalog.Get("openresty")
	version, _ := manifest.SelectVersion("distribution")
	f := &packageGuardFixture{plan: Plan{ID: "plan_" + strings.Repeat("a", 48), InstanceID: "openresty-default", PluginID: "openresty", Operation: "install", Version: version, Resources: []string{"software:openresty-default", "host:packages"}}}
	m, err := New(service.Runtime("plugins"), Options{Executor: f})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterProvider(m); err != nil {
		t.Fatal(err)
	}
	mint := func(scopes, resources []string) (core.Actor, string) {
		t.Helper()
		created, err := service.CreateToken(context.Background(), admin, core.TokenRequest{Name: "bounded package caller", Kind: "mcp", Scopes: scopes, Resources: resources, ExpiresInSeconds: 300})
		if err != nil {
			t.Fatal(err)
		}
		actor, err := service.AuthenticateToken(context.Background(), created.Token)
		if err != nil {
			t.Fatal(err)
		}
		return actor, created.ID
	}
	withoutScope, _ := mint([]string{"software:manage"}, f.plan.Resources)
	withoutResource, _ := mint([]string{"software:manage", "host:packages"}, []string{"software:openresty-default"})
	allowed, allowedID := mint([]string{"software:manage", "host:packages"}, f.plan.Resources)
	planInput := json.RawMessage(`{"operation":"install","plugin_id":"openresty"}`)
	for _, actor := range []core.Actor{withoutScope, withoutResource} {
		if _, err := service.Execute(context.Background(), actor, "software_plan", planInput); core.ErrorCode(err) != "forbidden" {
			t.Fatal("host package privilege bypassed", err)
		}
	}
	if f.plans.Load() != 0 {
		t.Fatal("unauthorized plan reached privileged APT planning")
	}
	if _, err := service.Execute(context.Background(), allowed, "software_plan", planInput); err != nil {
		t.Fatal(err)
	}
	applyInput, _ := json.Marshal(ApplyInput{PlanID: f.plan.ID, ExpectedRevision: 0, IdempotencyKey: "bounded-package-guard"})
	ctx := context.WithValue(context.Background(), packageGuardContextKey{}, "trusted task context")
	request := agent.Request{Operation: "plugin_apply", Input: applyInput, ActorID: withoutScope.ID, JobID: "server-job-id"}
	if _, err := m.Run(ctx, request); core.ErrorCode(err) != "forbidden" {
		t.Fatal("queued package job bypassed host scope", err)
	}
	if f.applies.Load() != 0 {
		t.Fatal("denied package job reached Agent")
	}
	request.ActorID = allowed.ID
	if _, err := m.Run(ctx, request); err != nil {
		t.Fatal(err)
	}
	if f.last.JobID != request.JobID || f.last.ActorID != request.ActorID || string(f.last.Input) != string(request.Input) || f.contextValue != "trusted task context" {
		t.Fatal("guard changed trusted task forwarding identity/input/context")
	}
	if err := service.RevokeToken(context.Background(), admin, allowedID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Run(ctx, request); core.ErrorCode(err) != "unauthorized" {
		t.Fatal("revoked package actor forwarded", err)
	}
	if f.applies.Load() != 1 {
		t.Fatal("revoked actor caused another package operation")
	}
}

func TestSoftwareWorkerRejectsRootAndAPIPrimaryGroupsReadOnly(t *testing.T) {
	e := testAgent(t)
	config := e.config
	base := t.TempDir()
	config.RootDir = filepath.Join(base, "never-create-software")
	config.StateDir = filepath.Join(base, "never-create-state")
	config.RunGroup = "root"
	if ValidatePrivilegedConfig(config) == nil {
		t.Fatal("runtime effective root GID accepted")
	}
	api, err := user.Lookup("daemon")
	if err != nil || api.Username == config.RunUser {
		t.Skip("distinct system fixture user absent")
	}
	group, err := user.LookupGroupId(api.Gid)
	if err != nil {
		t.Fatal(err)
	}
	config.APIUser = api.Username
	config.RunGroup = group.Name
	if ValidatePrivilegedConfig(config) == nil {
		t.Fatal("runtime effective API GID accepted")
	}
	if _, err := NewPrivilegedExecutor(config); err == nil {
		t.Fatal("constructor accepted runtime/API shared GID")
	}
	for _, path := range []string{config.RootDir, config.StateDir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("identity rejection created privileged state", path, err)
		}
	}
}
