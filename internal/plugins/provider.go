package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/jobs"
)

type Options struct {
	Catalog  *Catalog
	Executor jobs.Executor
	RootDir  string
}
type Manager struct {
	deps     core.Dependencies
	catalog  *Catalog
	executor jobs.Executor
	rootDir  string
	submitMu sync.Mutex
}

var validIDExtended = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,127}$`)

func New(deps core.Dependencies, options Options) (*Manager, error) {
	if deps.State == nil || deps.Tasks == nil || deps.Authorize == nil {
		return nil, errors.New("plugins require shared metadata, tasks and authorization")
	}
	catalog := options.Catalog
	var err error
	if catalog == nil {
		catalog, err = BuiltinCatalog()
		if err != nil {
			return nil, err
		}
	}
	return &Manager{deps: deps, catalog: catalog, executor: options.Executor, rootDir: options.RootDir}, nil
}

func failure(code, message string, status int) error {
	return &core.Error{Code: code, Message: message, Status: status}
}
func parse(raw json.RawMessage, value any) error {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return failure("invalid_input", "Invalid or unknown action fields", 400)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return failure("invalid_input", "Action must contain exactly one JSON object", 400)
	}
	return nil
}
func obj(properties map[string]any, required ...string) map[string]any {
	result := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		result["required"] = required
	}
	return result
}
func textSchema() map[string]any {
	return map[string]any{"type": "string", "minLength": 1, "maxLength": 256}
}
func idSchema() map[string]any {
	return map[string]any{"type": "string", "pattern": `^[a-z][a-z0-9_-]{0,127}$`}
}
func enumSchema(values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values}
}
func revisionSchema() map[string]any { return map[string]any{"type": "integer", "minimum": 0} }

func (m *Manager) Bindings() []core.Binding {
	bindings := []core.Binding{
		m.binding("plugins_catalog", "Discover trusted software sources, versions and migration verification; declarations are not success evidence", "software:read", true, obj(map[string]any{}), func(context.Context, core.Actor, json.RawMessage) ([]string, error) { return nil, nil }, m.catalogAction),
		m.binding("plugins_list", "List actual installed software with scoped health and ownership", "software:read", true, obj(map[string]any{}), func(context.Context, core.Actor, json.RawMessage) ([]string, error) { return nil, nil }, m.listAction),
		m.binding("plugins_get", "Read one actual software instance/configuration without revealing credentials", "software:read", true, obj(map[string]any{"id": idSchema()}, "id"), idResource, m.getAction),
		m.binding("software_plan", "Prepare a bounded trusted install/upgrade/config/uninstall/rollback plan without modifying unrelated system settings", "software:manage", true, obj(map[string]any{"operation": enumSchema("install", "upgrade", "config", "uninstall", "rollback"), "plugin_id": idSchema(), "instance_id": idSchema(), "version": textSchema(), "config": map[string]any{"type": "object", "maxProperties": 32}, "keep_data": map[string]any{"type": "boolean"}}, "operation", "plugin_id"), m.planResources, m.planAction),
		m.binding("software_apply", "Queue a durable trusted plan; inspect task and health before claiming success", "software:manage", false, obj(map[string]any{"plan_id": textSchema(), "expected_revision": revisionSchema(), "idempotency_key": map[string]any{"type": "string", "minLength": 8, "maxLength": 128}}, "plan_id", "expected_revision", "idempotency_key"), m.applyResources, m.applyAction),
		m.binding("software_service_control", "Start/stop/restart/reload/check only the instance's root-owned service profile", "software:manage", false, obj(map[string]any{"id": idSchema(), "command": enumSchema("start", "stop", "restart", "reload", "check")}, "id", "command"), idResource, m.serviceAction),
		m.binding("software_recovery_inspect", "Read the protected checkpoint for an interrupted install without exposing its private plan or proofs", "software:read", true, obj(map[string]any{"id": idSchema()}, "id"), idResource, m.recoveryInspectAction),
		m.binding("software_recovery", "Explicitly continue only a health-verified install checkpoint or stop and preserve an exactly proven owned service", "software:manage", false, obj(map[string]any{"id": idSchema(), "command": enumSchema("continue", "stop_preserve"), "expected_journal_revision": map[string]any{"type": "integer", "minimum": 1}}, "id", "command", "expected_journal_revision"), idResource, m.recoveryAction),
	}
	for i := range bindings {
		bindings[i].Action.Task = bindings[i].Action.Name == "software_apply" || bindings[i].Action.Name == "software_service_control" || bindings[i].Action.Name == "software_recovery"
	}
	bindings = append(bindings, m.databaseBindings()...)
	bindings = append(bindings, m.databaseAdminBindings()...)
	bindings = append(bindings, m.databaseBinlogBindings()...)
	bindings = append(bindings, m.databaseReplicationBindings()...)
	bindings = append(bindings, m.databaseTableMaintenanceBindings()...)
	bindings = append(bindings, m.autostartBindings()...)
	bindings = append(bindings, m.cacheBindings()...)
	bindings = append(bindings, m.cacheClusterBindings()...)
	bindings = append(bindings, m.postgresqlMaintenanceBindings()...)
	bindings = append(bindings, m.phpBindings()...)
	bindings = append(bindings, m.phpMaintenanceBindings()...)
	bindings = append(bindings, m.utilityBindings()...)
	bindings = append(bindings, m.ftpAccountStateBindings()...)
	bindings = append(bindings, m.ftpLegacyRecoveryBinding())
	bindings = append(bindings, m.supervisorLogBindings()...)
	bindings = append(bindings, m.dockerBindings()...)
	bindings = append(bindings, m.dockerImageBindings()...)
	bindings = append(bindings, m.dockerImagesListBinding())
	bindings = append(bindings, m.dockerRegistryBindings()...)
	bindings = append(bindings, m.dockerArchiveBindings()...)
	bindings = append(bindings, m.dockerExecBindings()...)
	bindings = append(bindings, m.ftpBindings()...)
	bindings = append(bindings, m.ftpMaintenanceBindings()...)
	bindings = append(bindings, m.hostedAppBindings()...)
	return bindings
}

func (m *Manager) binding(name, description, scope string, readOnly bool, schema map[string]any, resources func(context.Context, core.Actor, json.RawMessage) ([]string, error), execute func(context.Context, core.Actor, json.RawMessage) (core.ActionResult, error)) core.Binding {
	return core.Binding{Action: core.Action{Name: name, Description: description, Scope: scope, ReadOnly: readOnly, InputSchema: schema}, Resources: resources, Execute: execute}
}
func idResource(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var input struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &input) != nil || !validInstanceID(input.ID) {
		return nil, failure("invalid_input", "A valid instance ID is required", 400)
	}
	return []string{InstanceResource(input.ID)}, nil
}
func validInstanceID(id string) bool { return len(id) <= 128 && validIDExtended.MatchString(id) }
func planResource(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var input PlanInput
	if err := parse(raw, &input); err != nil {
		return nil, err
	}
	if !validID.MatchString(input.PluginID) {
		return nil, failure("invalid_input", "Unknown plugin ID", 400)
	}
	label := input.InstanceID
	if label == "" {
		label = "default"
	}
	if !validID.MatchString(label) {
		return nil, failure("invalid_input", "Invalid instance label", 400)
	}
	return []string{InstanceResource(input.PluginID + "-" + label)}, nil
}
func (m *Manager) planResources(ctx context.Context, actor core.Actor, raw json.RawMessage) ([]string, error) {
	resources, err := planResource(ctx, actor, raw)
	if err != nil {
		return nil, err
	}
	var input PlanInput
	_ = json.Unmarshal(raw, &input)
	if !m.deps.Authorize(actor, "software:manage", resources[0]) {
		return nil, failure("forbidden", "Software instance is outside the current management grant", 403)
	}
	if input.Operation == "install" || input.Operation == "upgrade" {
		manifest, err := m.catalog.Get(input.PluginID)
		if err != nil {
			return nil, failure("not_found", "Unknown software plugin", 404)
		}
		if input.Operation == "upgrade" && input.Version == "" {
			var current Instance
			encoded, _ := json.Marshal(IDInput{ID: strings.TrimPrefix(resources[0], "software:")})
			if err := m.agentJSON(ctx, "plugin_get", encoded, &current); err != nil {
				return nil, err
			}
			input.Version = current.SourceVersion
		}
		version, err := manifest.SelectVersion(input.Version)
		if err != nil {
			return nil, failure("unsupported_version", "Version is unavailable in the trusted catalog", 412)
		}
		if sourceUsesSystemPackages(version.Source) {
			if err := m.requirePackageGrant(actor); err != nil {
				return nil, err
			}
			resources = append(resources, "host:packages")
		}
	}
	return resources, nil
}
func (m *Manager) requirePackageGrant(actor core.Actor) error {
	if !m.deps.Authorize(actor, "host:packages", "host:packages") {
		return failure("forbidden", "Installing signed system packages or build dependencies requires the explicit host:packages scope and resource", 403)
	}
	return nil
}

func (m *Manager) applyResources(ctx context.Context, actor core.Actor, raw json.RawMessage) ([]string, error) {
	var input ApplyInput
	if err := parse(raw, &input); err != nil {
		return nil, err
	}
	var plan Plan
	data, err := m.deps.State.Get(ctx, "plans", input.PlanID)
	if err != nil {
		return nil, failure("not_found", "Software plan does not exist", 404)
	}
	if json.Unmarshal(data, &plan) != nil {
		return nil, failure("invalid_state", "Software plan cannot be decoded", 500)
	}
	resources := []string{InstanceResource(plan.InstanceID)}
	if (plan.Operation == "install" || plan.Operation == "upgrade") && sourceUsesSystemPackages(plan.Version.Source) {
		if err := m.requirePackageGrant(actor); err != nil {
			return nil, err
		}
		resources = append(resources, "host:packages")
	}
	return resources, nil
}

func (m *Manager) catalogAction(context.Context, core.Actor, json.RawMessage) (core.ActionResult, error) {
	return core.ActionResult{Data: map[string]any{"items": m.catalog.Items}}, nil
}
func (m *Manager) listAction(ctx context.Context, actor core.Actor, _ json.RawMessage) (core.ActionResult, error) {
	var instances []Instance
	if m.executor == nil {
		return core.ActionResult{Data: map[string]any{"items": []Instance{}, "availability": "agent_not_configured"}}, nil
	}
	if err := m.agentJSON(ctx, "plugin_list", json.RawMessage(`{}`), &instances); err != nil {
		return core.ActionResult{}, err
	}
	visible := []Instance{}
	for _, instance := range instances {
		if m.deps.Authorize(actor, "software:read", InstanceResource(instance.ID)) {
			visible = append(visible, publicSoftwareInstance(instance))
		}
	}
	return core.ActionResult{Data: map[string]any{"items": visible}}, nil
}
func (m *Manager) getAction(ctx context.Context, _ core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var input IDInput
	if err := parse(raw, &input); err != nil {
		return core.ActionResult{}, err
	}
	var instance Instance
	if err := m.agentJSON(ctx, "plugin_get", raw, &instance); err != nil {
		return core.ActionResult{}, err
	}
	return core.ActionResult{Data: publicSoftwareInstance(instance), Resources: []string{InstanceResource(input.ID)}}, nil
}
func (m *Manager) planAction(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var input PlanInput
	if err := parse(raw, &input); err != nil {
		return core.ActionResult{}, err
	}
	if _, err := m.catalog.Get(input.PluginID); err != nil {
		return core.ActionResult{}, failure("not_found", "Plugin is not in the trusted catalog", 404)
	}
	var plan Plan
	if err := m.agentJSON(ctx, "plugin_plan", raw, &plan); err != nil {
		return core.ActionResult{}, err
	}
	if m.deps.ResolveActor != nil {
		current, err := m.deps.ResolveActor(ctx, actor)
		if err != nil {
			return core.ActionResult{}, err
		}
		actor = current
	}
	for _, resource := range plan.Resources {
		if !m.deps.Authorize(actor, "software:manage", resource) {
			return core.ActionResult{}, failure("forbidden", "Software plan requires authorization for every instance and system package resource", 403)
		}
		if resource == "host:packages" {
			if err := m.requirePackageGrant(actor); err != nil {
				return core.ActionResult{}, err
			}
		}
	}
	publicPlan := publicSoftwarePlan(plan)
	data, _ := json.Marshal(publicPlan)
	if err := m.deps.State.Save(ctx, "plans", plan.ID, data); err != nil {
		return core.ActionResult{}, failure("persistence_failed", "Unable to persist the software plan", 500)
	}
	return core.ActionResult{Data: publicPlan, Resources: plan.Resources}, nil
}

func publicSoftwarePlan(plan Plan) Plan {
	plan.Config = cloneConfig(plan.Config)
	delete(plan.Config, "admin_password")
	if plan.Result != nil {
		instance := publicSoftwareInstance(*plan.Result)
		plan.Result = &instance
	}
	return plan
}

func publicSoftwareInstance(instance Instance) Instance {
	instance.Config = cloneConfig(instance.Config)
	delete(instance.Config, "admin_password")
	return instance
}
func (m *Manager) applyAction(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var input ApplyInput
	if err := parse(raw, &input); err != nil {
		return core.ActionResult{}, err
	}
	resources, err := m.applyResources(ctx, actor, raw)
	if err != nil {
		return core.ActionResult{}, err
	}
	if m.deps.ResolveActor != nil {
		current, err := m.deps.ResolveActor(ctx, actor)
		if err != nil {
			return core.ActionResult{}, err
		}
		actor = current
	}
	for _, resource := range resources {
		if !m.deps.Authorize(actor, "software:manage", resource) {
			return core.ActionResult{}, failure("forbidden", "Software execution requires current authorization for every reviewed resource", 403)
		}
		if resource == "host:packages" {
			if err := m.requirePackageGrant(actor); err != nil {
				return core.ActionResult{}, err
			}
		}
	}
	// Idempotency is an atomic metadata operation before task submission. The
	// Agent also binds the key to its exact immutable plan for crash retries.
	var job map[string]any
	m.submitMu.Lock()
	defer m.submitMu.Unlock()
	key := input.PlanID + ":" + input.IdempotencyKey
	if prior, e := m.deps.State.Get(ctx, "submissions", key); e == nil {
		err = json.Unmarshal(prior, &job)
	} else {
		job, err = m.deps.Tasks.Submit(ctx, actor, "plugin_apply", raw, resources)
		if err == nil {
			data, _ := json.Marshal(job)
			err = m.deps.State.Save(ctx, "submissions", key, data)
		}
	}
	if err != nil {
		return core.ActionResult{}, err
	}
	taskID, _ := job["id"].(string)
	if taskID == "" {
		taskID, _ = job["task_id"].(string)
	}
	return core.ActionResult{Data: job, TaskID: taskID, Resources: resources, AuditSummary: map[string]any{"plan_id": input.PlanID, "task_id": taskID}}, nil
}
func (m *Manager) serviceAction(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var input ServiceInput
	if err := parse(raw, &input); err != nil {
		return core.ActionResult{}, err
	}
	resources := []string{InstanceResource(input.ID)}
	job, err := m.deps.Tasks.Submit(ctx, actor, "plugin_service_control", raw, resources)
	if err != nil {
		return core.ActionResult{}, err
	}
	id, _ := job["id"].(string)
	return core.ActionResult{Data: job, Resources: resources, TaskID: id}, nil
}

func (m *Manager) recoveryInspectAction(ctx context.Context, _ core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var input installRecoveryInspectInput
	if err := parse(raw, &input); err != nil {
		return core.ActionResult{}, err
	}
	var view installRecoveryView
	if err := m.agentJSON(ctx, "plugin_recovery_inspect", raw, &view); err != nil {
		return core.ActionResult{}, err
	}
	return core.ActionResult{Data: view, Resources: []string{InstanceResource(input.ID)}}, nil
}

func (m *Manager) recoveryAction(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var input installRecoveryActionInput
	if err := parse(raw, &input); err != nil {
		return core.ActionResult{}, err
	}
	if !validInstanceID(input.ID) || input.ExpectedJournalRevision < 1 || (input.Command != "continue" && input.Command != "stop_preserve") {
		return core.ActionResult{}, failure("invalid_input", "Invalid install recovery command or journal revision", 400)
	}
	resources := []string{InstanceResource(input.ID)}
	if m.deps.ResolveActor != nil {
		current, err := m.deps.ResolveActor(ctx, actor)
		if err != nil {
			return core.ActionResult{}, err
		}
		actor = current
	}
	if !m.deps.Authorize(actor, "software:manage", resources[0]) {
		return core.ActionResult{}, failure("forbidden", "Install recovery requires current software management authorization for this instance", 403)
	}
	job, err := m.deps.Tasks.Submit(ctx, actor, "plugin_recovery_action", raw, resources)
	if err != nil {
		return core.ActionResult{}, err
	}
	id, _ := job["id"].(string)
	if id == "" {
		id, _ = job["task_id"].(string)
	}
	return core.ActionResult{Data: job, Resources: resources, TaskID: id, AuditSummary: map[string]any{"instance_id": input.ID, "command": input.Command}}, nil
}

func (m *Manager) agentJSON(ctx context.Context, operation string, input json.RawMessage, output any) error {
	if m.executor == nil {
		return failure("agent_unavailable", "A trusted software Agent must be configured", 503)
	}
	request := agent.Request{Operation: operation, Input: input}
	if err := m.executor.Validate(request); err != nil {
		return failure("invalid_input", "Agent rejected the typed software request", 400)
	}
	result, err := m.executor.Run(ctx, request)
	if err != nil || result.ExitCode != 0 {
		var problem struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Status  int    `json:"status"`
			Details any    `json:"details"`
		}
		if json.Unmarshal([]byte(result.Stderr), &problem) == nil && problem.Code != "" && problem.Status >= 400 {
			return &core.Error{Code: problem.Code, Message: problem.Message, Status: problem.Status, Details: problem.Details}
		}
		return failure("agent_operation_failed", "Software operation failed; inspect its task and prerequisites", 502)
	}
	if result.Truncated || len(result.Stdout) > 1<<20 {
		return failure("result_too_large", "Software result exceeds the response limit", 413)
	}
	if err = json.Unmarshal([]byte(result.Stdout), output); err != nil {
		return failure("agent_protocol_error", "Software Agent returned an invalid result", 502)
	}
	return nil
}

// ReadRuntimeTarget is for trusted domain providers already enforcing their
// resource ACL; unlike an action it is never exported as an HTTP/MCP bypass.
func (m *Manager) ReadRuntimeTarget(ctx context.Context, id string) (RuntimeTarget, error) {
	if !validInstanceID(id) {
		return RuntimeTarget{}, failure("invalid_input", "Invalid software instance ID", 400)
	}
	data, _ := json.Marshal(IDInput{ID: id})
	var instance Instance
	if err := m.agentJSON(ctx, "plugin_get", data, &instance); err != nil {
		return RuntimeTarget{}, err
	}
	target := RuntimeTarget{Instance: instance}
	switch instance.PluginID {
	case "openresty", "nginx":
		target.NginxConf = filepath.Join(instance.Paths.ConfigDir, "nginx.conf")
		target.SitesDir = filepath.Join(instance.Paths.ConfigDir, "sites")
	case "php", "php-apt", "php-yum":
		target.PHPIni = filepath.Join(instance.Paths.ConfigDir, "php.ini")
		target.FPMAddress = "unix:" + filepath.Join(instance.Paths.RunDir, "php-fpm.sock")
	}
	return target, nil
}
func (m *Manager) RuntimeTarget(ctx context.Context, actor core.Actor, id string) (RuntimeTarget, error) {
	if !m.deps.Authorize(actor, "software:read", InstanceResource(id)) {
		return RuntimeTarget{}, failure("forbidden", "Software instance is outside the authorized resources", 403)
	}
	return m.ReadRuntimeTarget(ctx, id)
}
func (m *Manager) Check(ctx context.Context, id string) error {
	input, _ := json.Marshal(ServiceInput{ID: id, Command: "check"})
	var result any
	return m.agentJSON(ctx, "plugin_service_control", input, &result)
}
func (m *Manager) Reload(ctx context.Context, id string) error {
	input, _ := json.Marshal(ServiceInput{ID: id, Command: "reload"})
	var result any
	return m.agentJSON(ctx, "plugin_service_control", input, &result)
}
func (m *Manager) ServiceProfiles(ctx context.Context) ([]ServiceProfile, error) {
	var instances []Instance
	if err := m.agentJSON(ctx, "plugin_list", json.RawMessage(`{}`), &instances); err != nil {
		return nil, err
	}
	result := []ServiceProfile{}
	for _, instance := range instances {
		if instance.Ownership == "managed" && instance.Service.Unit != "" {
			result = append(result, instance.Service)
		}
	}
	return result, nil
}

var _ core.Provider = (*Manager)(nil)
