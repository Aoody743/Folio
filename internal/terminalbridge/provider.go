// Package terminalbridge binds the same bounded terminal engine to REST and
// MCP. Only trusted composition supplies the Agent and persisted identities.
package terminalbridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/terminal"
)

type Options struct {
	Agent agent.Executor
	// Optional for HTTP/WS input retries. The root composition can supply
	// service.ExecuteWithKey; ordinary Dispatch remains the shared core path.
	DispatchWithKey func(context.Context, core.Actor, string, json.RawMessage, string) (any, error)
}
type Provider struct {
	deps    core.Dependencies
	options Options
	streams chan struct{}
}
type input struct {
	terminal.Input
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}

func New(deps core.Dependencies, options Options) (*Provider, error) {
	if deps.Authorize == nil || deps.ResolveActor == nil || deps.Secrets == nil || deps.Dispatch == nil {
		return nil, errors.New("terminal bridge requires persisted authorization, bound secrets and core dispatch")
	}
	return &Provider{deps: deps, options: options, streams: make(chan struct{}, 16)}, nil
}
func Resource(id string) string { return "terminal:" + id }
func failure(code, message string, status int) error {
	return &core.Error{Code: code, Message: message, Status: status}
}
func denied() error {
	return failure("forbidden", "Terminal target, root session or shared session is not authorized", 403)
}
func invalid() error { return failure("invalid_input", "Invalid bounded terminal parameters", 400) }
func bridgeError(err error) error {
	if err == nil {
		return nil
	}
	var native *core.Error
	if errors.As(err, &native) {
		return err
	}
	var remote *agent.RemoteError
	if errors.As(err, &remote) {
		return &core.Error{Code: remote.Code, Message: remote.Message, Status: remote.Status}
	}
	var local *terminal.Error
	if errors.As(err, &local) {
		return &core.Error{Code: local.Code, Message: local.Message, Status: local.Status}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return failure("timeout", "Terminal operation timed out", 504)
	}
	if errors.Is(err, context.Canceled) {
		return failure("canceled", "Terminal operation canceled", 409)
	}
	return failure("agent_unavailable", "Terminal Agent could not complete the operation", 503)
}
func decode(raw json.RawMessage) (input, error) {
	var in input
	if len(raw) > 64<<10 {
		return in, invalid()
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil {
		return in, invalid()
	}
	var extra any
	if !errors.Is(d.Decode(&extra), io.EOF) {
		return in, invalid()
	}
	if in.SecretRef != "" {
		return in, invalid()
	}
	return in, nil
}
func encoded(value any) json.RawMessage { data, _ := json.Marshal(value); return data }
func (p *Provider) call(ctx context.Context, a core.Actor, name string, payload terminal.AgentPayload, out any) error {
	if p.options.Agent == nil {
		return failure("agent_unavailable", "Terminal Agent is not configured", 503)
	}
	rpc, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	result, err := p.options.Agent.Run(rpc, agent.Request{ActorID: a.ID, Operation: name, Input: encoded(payload)})
	if err != nil {
		return bridgeError(err)
	}
	data := result.Data
	if len(data) == 0 {
		data = json.RawMessage(result.Stdout)
	}
	if len(data) == 0 || len(data) > 128<<10 || json.Unmarshal(data, out) != nil {
		return failure("agent_response", "Terminal Agent returned an invalid bounded result", 502)
	}
	return nil
}
func (p *Provider) target(ctx context.Context, a core.Actor, id string) (terminal.Target, error) {
	var target terminal.Target
	err := p.call(ctx, a, "terminal_targets_get", terminal.AgentPayload{Input: terminal.Input{ID: id}}, &target)
	if err == nil && (target.ID != id || target.Revision < 1) {
		err = failure("agent_response", "Terminal target identity is inconsistent", 502)
	}
	return target, err
}
func (p *Provider) info(ctx context.Context, a core.Actor, id string) (terminal.SessionInfo, error) {
	var info terminal.SessionInfo
	err := p.call(ctx, a, "terminal_info", terminal.AgentPayload{Input: terminal.Input{ID: id}}, &info)
	if err == nil && (info.ID != id || info.TargetID == "" || info.ActorID == "") {
		err = failure("agent_response", "Terminal session identity is inconsistent", 502)
	}
	return info, err
}
func (p *Provider) root(a core.Actor, root bool) error {
	if root && !p.deps.Authorize(a, "terminal:root", "host:local") {
		return denied()
	}
	return nil
}
func issuedTarget(in input) string {
	port := in.Port
	if port == 0 {
		port = 22
	}
	identity := struct {
		Host                               string
		Port                               int
		Username, AuthType, HostKey, Label string
	}{strings.ToLower(in.Host), port, in.Username, in.AuthType, in.HostKey, in.Label}
	hash := sha256.Sum256(encoded(identity))
	return "ssh-" + hex.EncodeToString(hash[:16])
}
func issuedCommand(in input) string {
	hash := sha256.Sum256(encoded(struct{ Title, Command string }{in.Title, in.Command}))
	return "command-" + hex.EncodeToString(hash[:16])
}
func targetMutation(name string) bool {
	return name == "terminal_targets_update" || name == "terminal_targets_delete"
}
func sessionOperation(name string) bool {
	return name == "terminal_read" || name == "terminal_input" || name == "terminal_resize" || name == "terminal_renew" || name == "terminal_close" || name == "terminal_share_discover"
}
func (p *Provider) resources(ctx context.Context, a core.Actor, name string, raw json.RawMessage) ([]string, error) {
	current, err := p.deps.ResolveActor(ctx, a)
	if err != nil {
		return nil, err
	}
	a = current
	in, err := decode(raw)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(name, "_list") {
		return nil, nil
	}
	var id, scope string
	root := false
	switch {
	case name == "terminal_targets_create":
		id, scope, root = issuedTarget(in), "terminal:manage", in.Username == "root"
	case name == "terminal_commands_create":
		id = in.ID
		if id == "" {
			id = issuedCommand(in)
		}
		scope = "terminal:manage"
	case name == "terminal_commands_delete":
		id, scope = in.ID, "terminal:manage"
	case name == "terminal_targets_get" || targetMutation(name):
		id = in.ID
		scope = "terminal:connect"
		if targetMutation(name) {
			scope = "terminal:manage"
		}
		if !p.deps.Authorize(a, scope, Resource(id)) {
			return nil, denied()
		}
		target, e := p.target(ctx, a, id)
		if e != nil {
			return nil, e
		}
		root = target.Root || (name == "terminal_targets_update" && in.Username == "root")
	case name == "terminal_open" || name == "terminal_root_open":
		id = in.TargetID
		scope = "terminal:connect"
		if !p.deps.Authorize(a, scope, Resource(id)) {
			return nil, denied()
		}
		target, e := p.target(ctx, a, id)
		if e != nil {
			return nil, e
		}
		root = target.Root
		if name == "terminal_root_open" && !root {
			return nil, invalid()
		}
	case sessionOperation(name):
		info, e := p.info(ctx, a, in.ID)
		if e != nil {
			return nil, e
		}
		id, scope, root = info.TargetID, "terminal:connect", info.Root
		if info.ActorID != a.ID && !p.deps.Authorize(a, "terminal:share", Resource(id)) {
			return nil, denied()
		}
		if name == "terminal_share_discover" && !p.deps.Authorize(a, "terminal:share", Resource(id)) {
			return nil, denied()
		}
	default:
		return nil, invalid()
	}
	if id == "" || !p.deps.Authorize(a, scope, Resource(id)) {
		return nil, denied()
	}
	if err = p.root(a, root); err != nil {
		return nil, err
	}
	resources := []string{Resource(id)}
	if root {
		resources = append(resources, "host:local")
	}
	return resources, nil
}
func (p *Provider) Bindings() []core.Binding {
	var bindings []core.Binding
	for _, descriptor := range descriptors() {
		spec := descriptor
		bindings = append(bindings, core.Binding{Action: spec, Resources: func(ctx context.Context, a core.Actor, raw json.RawMessage) ([]string, error) {
			return p.resources(ctx, a, spec.Name, raw)
		}, Execute: func(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			return p.execute(ctx, a, spec.Name, raw)
		}})
	}
	return bindings
}
func (p *Provider) execute(ctx context.Context, a core.Actor, name string, raw json.RawMessage) (core.ActionResult, error) {
	a, err := p.deps.ResolveActor(ctx, a)
	if err != nil {
		return core.ActionResult{}, err
	}
	if _, err = p.resources(ctx, a, name, raw); err != nil {
		return core.ActionResult{}, err
	}
	in, err := decode(raw)
	if err != nil {
		return core.ActionResult{}, err
	}
	payload := terminal.AgentPayload{Input: in.Input}
	var data any
	if name == "terminal_targets_list" {
		var targets []terminal.Target
		if err = p.call(ctx, a, name, payload, &targets); err != nil {
			return core.ActionResult{}, err
		}
		visible := []terminal.Target{}
		for _, target := range targets {
			if p.deps.Authorize(a, "terminal:connect", Resource(target.ID)) && p.root(a, target.Root) == nil {
				target.SecretRef = ""
				visible = append(visible, target)
			}
		}
		return core.ActionResult{Data: visible}, nil
	}
	if name == "terminal_commands_list" {
		var commands []terminal.Command
		if err = p.call(ctx, a, name, payload, &commands); err != nil {
			return core.ActionResult{}, err
		}
		visible := []terminal.Command{}
		for _, command := range commands {
			if p.deps.Authorize(a, "terminal:connect", Resource(command.ID)) {
				visible = append(visible, command)
			}
		}
		return core.ActionResult{Data: visible}, nil
	}
	if name == "terminal_open" || name == "terminal_root_open" {
		target, e := p.target(ctx, a, in.TargetID)
		if e != nil {
			return core.ActionResult{}, e
		}
		if !p.deps.Authorize(a, "terminal:connect", Resource(target.ID)) {
			return core.ActionResult{}, denied()
		}
		if e = p.root(a, target.Root); e != nil {
			return core.ActionResult{}, e
		}
		payload.RootAuthorized = target.Root
		payload.Input.ExpectedRevision = target.Revision
		if target.Type == "ssh" {
			secret, e := p.deps.Secrets.Get(ctx, a, Resource(target.ID), target.SecretRef)
			if e != nil {
				return core.ActionResult{}, e
			}
			var credential terminal.Credential
			if json.Unmarshal(secret, &credential) != nil {
				return core.ActionResult{}, failure("invalid_state", "Bound SSH credential is unavailable", 500)
			}
			payload.Credential = &credential
		}
		var info terminal.SessionInfo
		err = p.call(ctx, a, "terminal_open", payload, &info)
		data = info
	} else if sessionOperation(name) {
		info, e := p.info(ctx, a, in.ID)
		if e != nil {
			return core.ActionResult{}, e
		}
		if !p.deps.Authorize(a, "terminal:connect", Resource(info.TargetID)) {
			return core.ActionResult{}, denied()
		}
		if e = p.root(a, info.Root); e != nil {
			return core.ActionResult{}, e
		}
		payload.RootAuthorized = info.Root
		payload.AuthorizedShared = info.ActorID != a.ID
		if payload.AuthorizedShared && !p.deps.Authorize(a, "terminal:share", Resource(info.TargetID)) {
			return core.ActionResult{}, denied()
		}
		if name == "terminal_share_discover" {
			return core.ActionResult{Data: info}, nil
		}
		if name == "terminal_input" {
			bytes, e := base64.StdEncoding.DecodeString(in.Data)
			if e != nil || len(bytes) < 1 || len(bytes) > 16<<10 {
				return core.ActionResult{}, invalid()
			}
		}
		err = p.call(ctx, a, name, payload, &data)
	} else if name == "terminal_targets_create" || name == "terminal_targets_update" {
		id := in.ID
		var prior terminal.Target
		if name == "terminal_targets_create" {
			id = issuedTarget(in)
			payload.IssuedID = id
		} else {
			prior, err = p.target(ctx, a, id)
			if err != nil {
				return core.ActionResult{}, err
			}
			if in.ExpectedRevision != prior.Revision {
				return core.ActionResult{}, failure("conflict", "Terminal target revision changed", 409)
			}
		}
		if !p.deps.Authorize(a, "terminal:manage", Resource(id)) {
			return core.ActionResult{}, denied()
		}
		if e := p.root(a, prior.Root || in.Username == "root"); e != nil {
			return core.ActionResult{}, e
		}
		changed := name == "terminal_targets_create" || (in.Host != "" && in.Host != prior.Host) || (in.Port != 0 && in.Port != prior.Port) || (in.Username != "" && in.Username != prior.Username) || (in.AuthType != "" && in.AuthType != prior.AuthType) || (in.HostKey != "" && in.HostKey != prior.HostKey)
		credential, provided, e := credentialInput(in, prior.AuthType)
		if e != nil {
			return core.ActionResult{}, e
		}
		if changed && !provided {
			return core.ActionResult{}, failure("new_credential_required", "Changing SSH identity requires a new bound credential", 422)
		}
		ref := ""
		if provided {
			ref, err = p.deps.Secrets.Put(ctx, a, Resource(id), "SSH authentication", encoded(credential))
			if err != nil {
				return core.ActionResult{}, err
			}
			payload.Input.SecretRef = ref
		} else {
			payload.Input.SecretRef = prior.SecretRef
		}
		var target terminal.Target
		err = p.call(ctx, a, name, payload, &target)
		if err != nil {
			if ref != "" && core.ErrorStatus(err) < 500 {
				_ = p.deps.Secrets.Delete(ctx, a, Resource(id), ref)
			}
			if core.ErrorStatus(err) >= 500 {
				return core.ActionResult{}, &core.Error{Code: "operation_recovery_required", Message: "SSH target update was not acknowledged; inspect its state before a new operation", Status: 409, Details: map[string]any{"target_id": id}}
			}
			return core.ActionResult{}, err
		}
		target.SecretRef = ""
		data = target
		// Old secrets remain inaccessible except to this exact resource. Do not
		// delete them while a concurrent authorized open may still resolve them.
	} else if name == "terminal_targets_get" {
		target, e := p.target(ctx, a, in.ID)
		if e != nil {
			return core.ActionResult{}, e
		}
		target.SecretRef = ""
		data = target
	} else {
		if name == "terminal_commands_create" && in.ID == "" {
			payload.IssuedID = issuedCommand(in)
		}
		if name == "terminal_targets_delete" {
			target, e := p.target(ctx, a, in.ID)
			if e != nil {
				return core.ActionResult{}, e
			}
			if e = p.root(a, target.Root); e != nil {
				return core.ActionResult{}, e
			}
		}
		err = p.call(ctx, a, name, payload, &data)
	}
	return core.ActionResult{Data: data}, err
}
func credentialInput(in input, priorType string) (terminal.Credential, bool, error) {
	kind := in.AuthType
	if kind == "" {
		kind = priorType
	}
	provided := in.Password != "" || in.PrivateKey != "" || in.Passphrase != ""
	if !provided {
		return terminal.Credential{}, false, nil
	}
	if kind == "password" && in.Password != "" && in.PrivateKey == "" && in.Passphrase == "" {
		return terminal.Credential{Password: []byte(in.Password)}, true, nil
	}
	if kind == "key" && in.PrivateKey != "" && in.Password == "" {
		return terminal.Credential{PrivateKey: []byte(in.PrivateKey), Passphrase: []byte(in.Passphrase)}, true, nil
	}
	return terminal.Credential{}, false, invalid()
}
