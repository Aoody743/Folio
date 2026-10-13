package systemops

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type Options struct {
	Agent                  agent.Executor
	Status                 func(context.Context) (core.SystemStatus, error)
	AllowedScheduleActions []string
	SampleInterval         time.Duration
	HistoryRetentionDays   int
	TLSRoots               *x509.CertPool
	TelegramEndpoint       string
}
type Provider struct {
	deps            core.Dependencies
	options         Options
	ctx             context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	mu              sync.Mutex
	scheduleTargets map[string]bool
	networkPrevious map[string]NetworkUsage
	networkAt       time.Time
	historyPrunedAt time.Time
}

func New(deps core.Dependencies, options Options) (*Provider, error) {
	if deps.State == nil || deps.Secrets == nil || deps.Tasks == nil || deps.Authorize == nil || deps.ResolveActor == nil || deps.Dispatch == nil {
		return nil, errors.New("system operations require complete shared runtime dependencies")
	}
	if options.SampleInterval == 0 {
		options.SampleInterval = time.Minute
	}
	if options.SampleInterval < time.Second {
		return nil, errors.New("sampling interval must be at least one second")
	}
	if options.HistoryRetentionDays == 0 {
		options.HistoryRetentionDays = 30
	}
	if options.HistoryRetentionDays < 1 || options.HistoryRetentionDays > 365 {
		return nil, errors.New("history retention must be 1..365 days")
	}
	p := &Provider{deps: deps, options: options, scheduleTargets: map[string]bool{}, networkPrevious: map[string]NetworkUsage{}}
	if len(options.AllowedScheduleActions) == 0 {
		options.AllowedScheduleActions = []string{"files_backup", "files_hash", "notifications_send", "scripts_run"}
	}
	for _, name := range options.AllowedScheduleActions {
		if !identifier.MatchString(name) || strings.HasPrefix(name, "schedules_") {
			return nil, errors.New("invalid or recursive schedule target")
		}
		p.scheduleTargets[name] = true
	}
	if err := p.recoverScheduleRuns(context.Background()); err != nil {
		return nil, err
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.wg.Add(1)
	go p.background()
	return p, nil
}
func (p *Provider) Close() error { p.cancel(); p.wg.Wait(); return nil }
func newID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value[:]), nil
}
func input(raw json.RawMessage, target any) error {
	if err := strictJSON(raw, target); err != nil {
		return &core.Error{Code: "invalid_input", Message: "Invalid typed system operation input", Status: 400}
	}
	return nil
}
func unavailable(message string) error {
	return &core.Error{Code: "unavailable", Message: message, Status: 503}
}
func conflict(message string) error {
	return &core.Error{Code: "conflict", Message: message, Status: 409}
}
func invalid(message string) error {
	return &core.Error{Code: "invalid_input", Message: message, Status: 400}
}
func denied() error {
	return &core.Error{Code: "forbidden", Message: "System resource is outside this identity's authorization", Status: 403}
}
func missing(message string) error {
	return &core.Error{Code: "not_found", Message: message, Status: 404}
}
func object(fields map[string]any, required ...string) map[string]any {
	out := map[string]any{"type": "object", "properties": fields, "additionalProperties": false}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}
func text(maximum int) map[string]any { return map[string]any{"type": "string", "maxLength": maximum} }
func integer(minimum, maximum int) map[string]any {
	return map[string]any{"type": "integer", "minimum": minimum, "maximum": maximum}
}
func boolean() map[string]any              { return map[string]any{"type": "boolean"} }
func enum(values ...string) map[string]any { return map[string]any{"type": "string", "enum": values} }
func hostResources(context.Context, core.Actor, json.RawMessage) ([]string, error) {
	return []string{"host:local"}, nil
}
func (p *Provider) bind(name, description, scope string, readOnly bool, schema map[string]any, resources func(context.Context, core.Actor, json.RawMessage) ([]string, error), execute func(context.Context, core.Actor, json.RawMessage) (core.ActionResult, error)) core.Binding {
	task := name == "system_change_apply" || name == "system_change_rollback" || name == "notifications_send"
	return core.Binding{Action: core.Action{Name: name, Description: description, Scope: scope, ReadOnly: readOnly, Task: task, InputSchema: schema}, Resources: resources, Execute: execute}
}

func (p *Provider) Bindings() []core.Binding {
	bindings := p.scheduleBindings()
	bindings = append(bindings, p.notificationBindings()...)
	bindings = append(bindings, p.historyBindings()...)
	bindings = append(bindings, p.scriptBindings()...)
	for _, spec := range []struct {
		name, operation, scope string
		read                   bool
	}{
		{"services_list", "system_services_list", "services:read", true}, {"services_get", "system_services_get", "services:read", true},
		{"system_change_plan", "system_change_plan", "system:write", false}, {"system_change_get", "system_change_get", "system:read", true},
		{"system_change_apply", "system_change_apply", "system:write", false}, {"system_change_confirm", "system_change_confirm", "system:write", false}, {"system_change_rollback", "system_change_rollback", "system:write", false},
	} {
		schema := object(map[string]any{})
		if spec.name == "services_get" {
			schema = object(map[string]any{"service_id": text(96)}, "service_id")
		}
		if spec.name == "system_change_plan" {
			schema = object(map[string]any{"change": changeSchema()}, "change")
		} else if strings.HasPrefix(spec.name, "system_change_") {
			schema = object(map[string]any{"id": text(96)}, "id")
		}
		binding := p.bind(spec.name, "Typed Linux administration through explicit Agent policy and recoverable plans", spec.scope, spec.read, schema, hostResources, func(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			return p.hostAction(ctx, a, spec.operation, raw)
		})
		bindings = append(bindings, binding)
	}
	return bindings
}
func changeSchema() map[string]any {
	return object(map[string]any{"kind": enum("service_state", "ssh_config", "firewall_rules", "timezone", "ntp", "power", "panel_config"), "service_id": text(96), "service_state": enum("start", "stop", "restart", "reload", "enable", "disable"), "timezone": text(128), "ntp": boolean(), "power": enum("reboot", "poweroff"), "delay_seconds": integer(60, 600), "ssh": object(map[string]any{"ports": map[string]any{"type": "array", "items": integer(1, 65535), "minItems": 1, "maxItems": 16}, "password_authentication": boolean(), "permit_root_login": enum("no", "prohibit-password", "yes"), "allow_users": map[string]any{"type": "array", "items": text(96), "maxItems": 64}}, "ports", "password_authentication", "permit_root_login"), "firewall": object(map[string]any{"default_policy": enum("accept", "drop"), "rules": map[string]any{"type": "array", "maxItems": 128, "items": object(map[string]any{"id": text(96), "protocol": enum("tcp", "udp", "any"), "port": integer(0, 65535), "source": text(64), "action": enum("accept", "drop", "reject")}, "id", "protocol", "action")}}, "default_policy", "rules"), "panel": object(map[string]any{"version": integer(1, 1), "listen": text(256), "public_url": text(2048), "allowed_origins": map[string]any{"type": "array", "items": text(2048), "maxItems": 16}, "secure_cookies": boolean(), "tls_cert": text(1024), "tls_key": text(1024)}, "version", "listen", "allowed_origins", "secure_cookies")}, "kind")
}
func (p *Provider) hostAction(ctx context.Context, a core.Actor, operation string, raw json.RawMessage) (core.ActionResult, error) {
	if p.options.Agent == nil {
		return core.ActionResult{}, unavailable("Privileged Agent is not explicitly configured for this host")
	}
	var request hostRequest
	if err := input(raw, &request); err != nil {
		return core.ActionResult{}, err
	}
	request.ActorID = a.ID
	payload, _ := json.Marshal(request)
	if operation == "system_change_apply" || operation == "system_change_rollback" {
		job := hostJob{Actor: a.Public(), Operation: operation, Request: request}
		jobRaw, _ := json.Marshal(job)
		jobOperation := "systemops_host_apply"
		if operation == "system_change_rollback" {
			jobOperation = "systemops_host_rollback"
		}
		task, err := p.deps.Tasks.Submit(ctx, a, jobOperation, jobRaw, []string{"host:local"})
		id, _ := task["id"].(string)
		return core.ActionResult{Data: map[string]any{"task": task, "task_id": id, "plan_id": request.ID, "status": "submitted"}, TaskID: id, Resources: []string{"host:local"}}, err
	}
	result, err := p.options.Agent.Run(ctx, agent.Request{Operation: operation, Input: payload})
	if err != nil {
		return core.ActionResult{}, hostActionError(err)
	}
	var data any
	if err = json.Unmarshal([]byte(result.Stdout), &data); err != nil {
		return core.ActionResult{}, err
	}
	return core.ActionResult{Data: data, Resources: []string{"host:local"}, AuditSummary: map[string]any{"operation": operation, "plan_id": request.ID}}, nil
}
func hostActionError(err error) error {
	var remote *agent.RemoteError
	if errors.As(err, &remote) {
		return &core.Error{Code: remote.Code, Message: remote.Message, Status: remote.Status}
	}
	var typed interface{ AgentError() (string, int) }
	if errors.As(err, &typed) {
		code, status := typed.AgentError()
		return &core.Error{Code: code, Message: err.Error(), Status: status}
	}
	if errors.Is(err, agent.ErrUnauthorized) {
		return denied()
	}
	if errors.Is(err, agent.ErrInvalidInput) {
		return invalid("Invalid host operation input")
	}
	return &core.Error{Code: "host_operation_failed", Message: "Host operation failed; inspect its plan and Agent prerequisites", Status: 502}
}
func (p *Provider) background() {
	defer p.wg.Done()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	nextSample := time.Now()
	for {
		select {
		case <-p.ctx.Done():
			return
		case now := <-tick.C:
			p.tickSchedules(p.ctx, now)
			if !now.Before(nextSample) && p.options.Status != nil {
				_ = p.sample(p.ctx, now)
				nextSample = now.Add(p.options.SampleInterval)
			}
		}
	}
}
