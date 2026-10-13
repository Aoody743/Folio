package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

func (m *Manager) Actions() []Action {
	fields := map[string]any{}
	for _, name := range []string{"id", "target_id", "data", "label", "host", "username", "auth_type", "secret_ref", "host_key", "title", "command"} {
		fields[name] = map[string]any{"type": "string"}
	}
	for _, name := range []string{"rows", "cols", "after", "limit", "port", "expected_revision"} {
		fields[name] = map[string]any{"type": "integer", "minimum": 0}
	}
	rows := []struct {
		name, scope      string
		read             bool
		fields, required []string
	}{
		{"terminal_targets_list", "terminal:connect", true, nil, nil},
		{"terminal_targets_get", "terminal:connect", true, []string{"id"}, []string{"id"}},
		{"terminal_targets_create", "terminal:manage", false, []string{"label", "host", "port", "username", "auth_type", "secret_ref", "host_key"}, []string{"label", "host", "username", "auth_type", "secret_ref", "host_key"}},
		{"terminal_targets_update", "terminal:manage", false, []string{"id", "label", "host", "port", "username", "auth_type", "secret_ref", "host_key", "expected_revision"}, []string{"id", "expected_revision"}},
		{"terminal_targets_delete", "terminal:manage", false, []string{"id"}, []string{"id"}},
		{"terminal_commands_list", "terminal:connect", true, nil, nil},
		{"terminal_commands_create", "terminal:manage", false, []string{"id", "title", "command", "expected_revision"}, []string{"title", "command"}},
		{"terminal_commands_delete", "terminal:manage", false, []string{"id"}, []string{"id"}},
		{"terminal_open", "terminal:connect", false, []string{"target_id", "rows", "cols", "expected_revision"}, []string{"target_id"}},
		{"terminal_read", "terminal:connect", true, []string{"id", "after", "limit"}, []string{"id"}},
		{"terminal_input", "terminal:connect", false, []string{"id", "data"}, []string{"id", "data"}},
		{"terminal_resize", "terminal:connect", false, []string{"id", "rows", "cols"}, []string{"id", "rows", "cols"}},
		{"terminal_renew", "terminal:connect", false, []string{"id"}, []string{"id"}},
		{"terminal_close", "terminal:connect", false, []string{"id"}, []string{"id"}},
	}
	out := []Action{}
	for _, row := range rows {
		properties := map[string]any{}
		for _, name := range row.fields {
			properties[name] = fields[name]
		}
		schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(row.required) > 0 {
			schema["required"] = row.required
		}
		out = append(out, Action{Name: row.name, Scope: row.scope, ReadOnly: row.read, InputSchema: schema})
	}
	return out
}
func OperationNames() []string {
	return []string{"terminal_targets_list", "terminal_targets_get", "terminal_targets_create", "terminal_targets_update", "terminal_targets_delete", "terminal_commands_list", "terminal_commands_create", "terminal_commands_delete", "terminal_open", "terminal_read", "terminal_input", "terminal_resize", "terminal_renew", "terminal_close", "terminal_info"}
}
func (m *Manager) Execute(ctx context.Context, actorID, operation string, in Input, credential *Credential) (any, error) {
	switch operation {
	case "terminal_targets_list":
		return m.Targets(ctx)
	case "terminal_targets_get":
		return m.GetTarget(ctx, in.ID)
	case "terminal_targets_create":
		return m.CreateTarget(ctx, actorID, in)
	case "terminal_targets_update":
		return m.UpdateTarget(ctx, actorID, in)
	case "terminal_targets_delete":
		return m.DeleteTarget(ctx, in)
	case "terminal_commands_list":
		return m.Commands(ctx)
	case "terminal_commands_create":
		return m.SaveCommand(ctx, actorID, in)
	case "terminal_commands_delete":
		return m.DeleteCommand(ctx, in)
	case "terminal_open":
		flags, _ := ctx.Value(flagsKey{}).(trustedFlags)
		return m.Open(ctx, actorID, OpenInput{TargetID: in.TargetID, Rows: in.Rows, Cols: in.Cols, ExpectedRevision: in.ExpectedRevision, RootAuthorized: flags.root}, credential)
	case "terminal_info":
		return m.Info(in.ID)
	case "terminal_read":
		return m.Read(ctx, actorID, in)
	case "terminal_input":
		return m.Input(ctx, actorID, in)
	case "terminal_resize":
		return m.Resize(ctx, actorID, in)
	case "terminal_renew":
		return m.Renew(ctx, actorID, in)
	case "terminal_close":
		return m.CloseSession(ctx, actorID, in)
	default:
		return nil, agent.ErrUnauthorized
	}
}

type AgentExecutor struct{ Manager *Manager }

func decodePayload(raw json.RawMessage) (AgentPayload, error) {
	var input AgentPayload
	if len(raw) > 96<<10 {
		return input, failure("invalid_input", "Terminal RPC exceeds frame budget", 400)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, failure("invalid_input", "Invalid typed terminal RPC", 400)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return input, failure("invalid_input", "Expected one terminal RPC document", 400)
	}
	return input, nil
}
func (e *AgentExecutor) Validate(request agent.Request) error {
	if request.ActorID == "" {
		return failure("unauthorized", "Trusted caller identity is required", 401)
	}
	found := false
	for _, name := range OperationNames() {
		if request.Operation == name {
			found = true
		}
	}
	if !found {
		return agent.ErrUnauthorized
	}
	payload, err := decodePayload(request.Input)
	if err != nil {
		return err
	}
	if request.Operation != "terminal_open" && payload.Credential != nil {
		return failure("invalid_input", "Credentials are accepted only during SSH session creation", 400)
	}
	if payload.Credential != nil && payload.Input.ExpectedRevision < 1 {
		return failure("invalid_input", "Resolved SSH credentials require their target revision", 400)
	}
	if payload.IssuedID != "" && request.Operation != "terminal_targets_create" && request.Operation != "terminal_commands_create" {
		return failure("invalid_input", "An issued ID is accepted only for creation", 400)
	}
	return nil
}
func (e *AgentExecutor) Run(ctx context.Context, request agent.Request) (agent.Result, error) {
	result := agent.Result{Operation: request.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	if err := e.Validate(request); err != nil {
		return result, err
	}
	payload, _ := decodePayload(request.Input)
	ctx = withFlags(ctx, payload.AuthorizedShared, payload.RootAuthorized)
	var data any
	var err error
	if payload.IssuedID != "" && request.Operation == "terminal_targets_create" {
		data, err = e.Manager.CreateTargetWithID(ctx, request.ActorID, payload.IssuedID, payload.Input)
	} else if payload.IssuedID != "" && request.Operation == "terminal_commands_create" {
		data, err = e.Manager.SaveCommandWithID(ctx, request.ActorID, payload.IssuedID, payload.Input)
	} else {
		data, err = e.Manager.Execute(ctx, request.ActorID, request.Operation, payload.Input, payload.Credential)
	}
	result.FinishedAt = time.Now().UTC()
	result.Data = raw(data)
	if err == nil {
		result.ExitCode = 0
	}
	return result, err
}
