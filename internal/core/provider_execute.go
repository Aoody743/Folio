package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type authorizedActionKey struct{}
type authorizedAction struct {
	Name, Scope string
	Resources   []string
	ActorID     string
}

func authorizeResources(a Actor, resources []string) error {
	if len(resources) > 128 {
		return invalid("Action declares too many resources")
	}
	for _, resource := range resources {
		if resource == "" || resource == "*" || strings.HasSuffix(resource, ":*") || len(resource) > 512 {
			return invalid("Action must resolve concrete resources")
		}
		if !hasResource(a, resource) {
			return denied()
		}
	}
	return nil
}
func (s *Service) executeProvider(ctx context.Context, a Actor, binding Binding, raw json.RawMessage) (data any, err error) {
	resource := ""
	summary := map[string]any{}
	resources, e := binding.Resources(ctx, a, raw)
	if e != nil {
		return nil, e
	}
	if len(resources) > 0 {
		resource = resources[0]
	}
	if !binding.Action.ReadOnly && len(resources) == 0 {
		return nil, &Error{Code: "provider_contract", Message: "Mutation did not resolve its actual resource", Status: 500}
	}
	defer func() {
		if recover() != nil {
			data = nil
			err = &Error{Code: "operation_recovery_required", Message: "Provider was interrupted; inspect affected resources before retrying", Status: 409}
		}
		if err != nil || !binding.Action.ReadOnly || binding.Action.Audit || binding.Action.Task {
			s.recordAudit(ctx, a, binding.Action.Name, resource, err, summary)
		}
	}()
	if e = authorizeResources(a, resources); e != nil {
		return nil, e
	}
	ctx = context.WithValue(ctx, authorizedActionKey{}, authorizedAction{Name: binding.Action.Name, Scope: binding.Action.Scope, Resources: append([]string(nil), resources...), ActorID: a.ID})
	result, e := invokeBinding(ctx, a, binding, raw)
	if e != nil {
		return nil, e
	}
	if len(result.Resources) == 0 {
		result.Resources = resources
	}
	if e = authorizeResources(a, result.Resources); e != nil {
		return nil, &Error{Code: "provider_contract", Message: "Provider returned an undeclared or unauthorized resource", Status: 500}
	}
	for _, actual := range result.Resources {
		found := false
		for _, declared := range resources {
			if declared == actual {
				found = true
				break
			}
		}
		if !found {
			return nil, &Error{Code: "provider_contract", Message: "Provider changed an undeclared resource", Status: 500}
		}
	}
	if len(result.Resources) > 0 {
		resource = result.Resources[0]
	}
	summary = safeSummary(result.AuditSummary)
	if result.TaskID != "" {
		summary["task_id"] = result.TaskID
	}
	if binding.Action.OutputSchema != nil {
		encoded, e := json.Marshal(result.Data)
		if e != nil {
			return nil, internal()
		}
		var value any
		decoder := json.NewDecoder(strings.NewReader(string(encoded)))
		decoder.UseNumber()
		if decoder.Decode(&value) != nil || validateValue(value, binding.Action.OutputSchema, "output", 0) != nil {
			return nil, &Error{Code: "provider_contract", Message: "Provider output does not match its schema", Status: 500}
		}
	}
	if !binding.Action.ReadOnly {
		s.publish(Event{Type: "resource_changed", Data: map[string]any{"resources": result.Resources, "action": binding.Action.Name}}, binding.Action.Scope, resource)
	}
	return result.Data, nil
}
func invokeBinding(ctx context.Context, a Actor, binding Binding, raw json.RawMessage) (result ActionResult, err error) {
	defer func() {
		if recover() != nil {
			err = &Error{Code: "operation_recovery_required", Message: "Provider execution interrupted; inspect affected resource before retrying", Status: 409}
		}
	}()
	return binding.Execute(ctx, a, raw)
}
func safeSummary(summary map[string]any) map[string]any {
	out := map[string]any{}
	keys := make([]string, 0, len(summary))
	for key := range summary {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if auditSecretField(key) {
			continue
		}
		encoded, e := json.Marshal(summary[key])
		if e != nil || len(encoded) > 32<<10 {
			continue
		}
		var value any
		decoder := json.NewDecoder(strings.NewReader(string(encoded)))
		decoder.UseNumber()
		if decoder.Decode(&value) != nil {
			continue
		}
		value = redactAuditValue(value, 0)
		encoded, e = json.Marshal(value)
		if e != nil || len(encoded) > 1024 {
			continue
		}
		out[key] = value
		if len(out) >= 24 {
			break
		}
	}
	return out
}

// Providers can supply typed objects or RawMessage, so sanitizing only the
// outer map is insufficient. Normalize separators and camel case to cover
// API-key/private-key aliases while retaining safe identifiers and hashes.
func auditSecretField(key string) bool {
	canonical := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, key)
	lower := strings.ToLower(key)
	for _, suffix := range []string{"id", "ref", "hash", "fingerprint"} {
		if strings.HasSuffix(lower, "_"+suffix) || strings.HasSuffix(lower, ":"+suffix) || strings.HasSuffix(lower, "-"+suffix) || strings.HasSuffix(key, strings.ToUpper(suffix[:1])+suffix[1:]) || strings.HasSuffix(key, strings.ToUpper(suffix)) {
			return false
		}
	}
	for _, fragment := range []string{"password", "credential", "secret", "token", "privatekey", "apikey", "accesskey", "clientkey", "keypem", "certkey", "passphrase", "authorization", "content", "body", "query", "sql", "statement", "command", "script", "parameters", "environment"} {
		if strings.Contains(canonical, fragment) {
			return true
		}
	}
	switch canonical {
	case "args", "argv", "headers":
		return true
	}
	return false
}
func redactAuditValue(value any, depth int) any {
	if depth > 8 {
		return "[redacted]"
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any)
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if !auditSecretField(key) && len(out) < 64 {
				out[key] = redactAuditValue(typed[key], depth+1)
			}
		}
		return out
	case []any:
		out := make([]any, 0, min(len(typed), 32))
		for _, item := range typed[:min(len(typed), 32)] {
			out = append(out, redactAuditValue(item, depth+1))
		}
		return out
	case string:
		// Private PEM cannot become public simply by choosing a benign field.
		if strings.Contains(typed, "-----BEGIN ") && (strings.Contains(typed, "PRIVATE KEY-----") || strings.Contains(typed, "OPENSSH PRIVATE KEY-----")) {
			return "[redacted]"
		}
		return typed
	default:
		return value
	}
}

type taskMetaKey struct{}
type taskMetadata struct {
	Actor     Actor    `json:"actor"`
	Resources []string `json:"resources"`
	Action    string   `json:"action"`
	Scope     string   `json:"scope"`
}
type taskSubmitter struct {
	s         *Service
	namespace string
}

func (t taskSubmitter) Submit(ctx context.Context, prior Actor, operation string, input json.RawMessage, resources []string) (map[string]any, error) {
	a, meta, err := t.authorization(ctx, prior, resources)
	if err != nil {
		return nil, err
	}
	submitCtx := context.WithValue(ctx, taskMetaKey{}, meta)
	j, err := t.s.jobs.Submit(submitCtx, a.ID, operation, input)
	if err != nil {
		return nil, &Error{Code: "task_rejected", Message: "Task could not be submitted: " + fmt.Sprint(err), Status: 400}
	}
	return t.s.jobView(ctx, j), nil
}
func (t taskSubmitter) authorization(ctx context.Context, prior Actor, resources []string) (Actor, taskMetadata, error) {
	a, err := t.s.RefreshActor(ctx, prior)
	if err != nil {
		return Actor{}, taskMetadata{}, err
	}
	authorization, ok := ctx.Value(authorizedActionKey{}).(authorizedAction)
	if !ok || authorization.ActorID != a.ID || !hasScope(a, authorization.Scope) {
		return Actor{}, taskMetadata{}, denied()
	}
	if len(resources) == 0 {
		return Actor{}, taskMetadata{}, invalid("Task resources must be server-issued")
	}
	if err = authorizeResources(a, resources); err != nil {
		return Actor{}, taskMetadata{}, err
	}
	for _, resource := range resources {
		found := false
		for _, allowed := range authorization.Resources {
			if allowed == resource {
				found = true
				break
			}
		}
		if !found {
			return Actor{}, taskMetadata{}, denied()
		}
	}
	meta := taskMetadata{Actor: a.Public(), Resources: append([]string(nil), resources...), Action: authorization.Name, Scope: authorization.Scope}
	return a, meta, nil
}
func (t taskSubmitter) Get(ctx context.Context, a Actor, id string) (map[string]any, error) {
	fresh, e := t.s.RefreshActor(ctx, a)
	if e != nil {
		return nil, e
	}
	if !hasScope(fresh, "tasks:read") {
		return nil, denied()
	}
	value, e := t.s.jobAction(ctx, fresh, "tasks_get", id)
	if e != nil {
		return nil, e
	}
	return value.(map[string]any), nil
}
func (t taskSubmitter) Cancel(ctx context.Context, a Actor, id string) (map[string]any, error) {
	fresh, e := t.s.RefreshActor(ctx, a)
	if e != nil {
		return nil, e
	}
	if !hasScope(fresh, "tasks:write") {
		return nil, denied()
	}
	value, e := t.s.jobAction(ctx, fresh, "tasks_cancel", id)
	if e != nil {
		return nil, e
	}
	return value.(map[string]any), nil
}
