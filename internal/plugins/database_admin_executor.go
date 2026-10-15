package plugins

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type databaseAdminRotateInput struct {
	InstanceID               string `json:"instance_id"`
	CurrentCredentialVersion string `json:"current_credential_version"`
	PasswordVersion          string `json:"password_version,omitempty"`
}
type databaseAdminPrepareInput struct {
	InstanceID               string `json:"instance_id"`
	CurrentCredentialVersion string `json:"current_credential_version"`
	Password                 string `json:"password"`
}

// API metadata contains only an immutable pin; the secret itself is encrypted
// by Core's SecretStore and is resolved only immediately before Unix IPC.
type databaseAdminPasswordVersion struct {
	InstanceID, CurrentCredentialVersion, Version, SecretRef string
}

func validDatabaseAdminPassword(s string) bool {
	return len(s) >= 12 && len(s) <= 512 && !strings.ContainsAny(s, "\x00\n\r")
}
func databaseAdminRotateResources(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var in databaseAdminRotateInput
	if err := parse(raw, &in); err != nil {
		return nil, err
	}
	if !validInstanceID(in.InstanceID) || len(in.InstanceID) > 70 || !validDatabaseAdminVersion(in.CurrentCredentialVersion) || (in.PasswordVersion != "" && !validDatabaseAdminVersion(in.PasswordVersion)) {
		return nil, failure("invalid_input", "Administrator rotation requires the current version and a valid optional password pin", 400)
	}
	return []string{DatabaseResource(in.InstanceID, "")}, nil
}
func (m *Manager) databaseAdminPasswordBindings() []core.Binding {
	fields := func() map[string]any {
		return map[string]any{"instance_id": idSchema(), "current_credential_version": map[string]any{"type": "string", "pattern": "^[0-9a-f]{32}$"}}
	}
	prepareFields := fields()
	prepareFields["password"] = map[string]any{"type": "string", "format": "password", "minLength": 12, "maxLength": 512}
	prepareResources := func(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
		var in databaseAdminPrepareInput
		if err := parse(raw, &in); err != nil {
			return nil, err
		}
		if !validInstanceID(in.InstanceID) || len(in.InstanceID) > 70 || !validDatabaseAdminVersion(in.CurrentCredentialVersion) || !validDatabaseAdminPassword(in.Password) {
			return nil, failure("invalid_input", "Administrator password must be 12–512 bytes without line breaks", 400)
		}
		return []string{DatabaseResource(in.InstanceID, "")}, nil
	}
	prepare := m.binding("managed_database_admin_prepare_password", "Privately stage an immutable custom SQL administrator password; only its opaque version can enter a task", "databases:credentials", false, obj(prepareFields, "instance_id", "current_credential_version", "password"), prepareResources, func(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		var in databaseAdminPrepareInput
		if err := parse(raw, &in); err != nil {
			return core.ActionResult{}, err
		}
		resources, err := prepareResources(ctx, a, raw)
		if err != nil {
			return core.ActionResult{}, err
		}
		inspect, _ := json.Marshal(databaseAdminInput{InstanceID: in.InstanceID, Command: "inspect"})
		var state databaseAdminState
		if err = m.agentJSON(ctx, "database_admin_action", inspect, &state); err != nil {
			return core.ActionResult{}, err
		}
		if state.CurrentCredentialVersion != in.CurrentCredentialVersion || state.State != "active" {
			return core.ActionResult{}, failure("revision_conflict", "Administrator credential changed or requires recovery", 412)
		}
		version, err := newDatabaseAdminVersion()
		if err != nil {
			return core.ActionResult{}, err
		}
		secret := []byte(in.Password)
		ref, err := m.deps.Secrets.Put(ctx, a, resources[0], "SQL administrator password version", secret)
		clear(secret)
		if err != nil {
			return core.ActionResult{}, err
		}
		record := databaseAdminPasswordVersion{InstanceID: in.InstanceID, CurrentCredentialVersion: in.CurrentCredentialVersion, Version: version, SecretRef: ref}
		data, _ := json.Marshal(record)
		if _, err = m.deps.State.CompareAndSwap(ctx, "database_admin_password_versions", version, 0, data); err != nil {
			_ = m.deps.Secrets.Delete(ctx, a, resources[0], ref)
			return core.ActionResult{}, failure("credential_storage_failed", "Custom administrator password could not be staged", 502)
		}
		return core.ActionResult{Data: map[string]any{"password_version": version}, Resources: resources}, nil
	})
	rotateFields := fields()
	rotateFields["password_version"] = map[string]any{"type": "string", "pattern": "^[0-9a-f]{32}$"}
	rotate := m.binding("managed_database_admin_rotate", "Rotate an owned SQL administrator to a generated password or an immutable staged custom password", "databases:credentials", false, obj(rotateFields, "instance_id", "current_credential_version"), databaseAdminRotateResources, func(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		resources, err := databaseAdminRotateResources(ctx, a, raw)
		if err != nil {
			return core.ActionResult{}, err
		}
		job, err := m.deps.Tasks.Submit(ctx, a, "database_admin_rotate", raw, resources)
		if err != nil {
			return core.ActionResult{}, err
		}
		id, _ := job["id"].(string)
		return core.ActionResult{Data: job, TaskID: id, Resources: resources}, nil
	})
	rotate.Action.Task = true
	return []core.Binding{prepare, rotate}
}

// DatabaseAdminExecutor runs in the API process. Durable jobs contain only an
// opaque immutable version; plaintext crosses the UID-authenticated Unix
// transport only after the original actor's current grants are checked.
type DatabaseAdminExecutor struct {
	manager *Manager
	client  *agent.UnixClient
}

func NewDatabaseAdminExecutor(m *Manager, c *agent.UnixClient) *DatabaseAdminExecutor {
	return &DatabaseAdminExecutor{m, c}
}
func DatabaseAdminJobOperations() []string { return []string{"database_admin_rotate"} }
func (e *DatabaseAdminExecutor) Validate(r agent.Request) error {
	if r.Operation != "database_admin_rotate" {
		return agent.ErrUnauthorized
	}
	if _, err := databaseAdminRotateResources(context.Background(), core.Actor{}, r.Input); err != nil {
		return err
	}
	if e.manager == nil || e.client == nil || e.manager.deps.Secrets == nil || e.manager.deps.ResolveActor == nil || e.manager.deps.Authorize == nil {
		return failure("agent_unavailable", "Administrator rotation requires the protected Unix Agent", 503)
	}
	return nil
}
func (e *DatabaseAdminExecutor) Run(ctx context.Context, r agent.Request) (result agent.Result, returned error) {
	result = agent.Result{Operation: r.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	defer func() { result.FinishedAt = time.Now().UTC() }()
	if err := e.Validate(r); err != nil {
		return result, err
	}
	m := e.manager
	actor, err := m.deps.ResolveActor(ctx, core.Actor{ID: r.ActorID})
	if err != nil {
		return result, err
	}
	var in databaseAdminRotateInput
	_ = json.Unmarshal(r.Input, &in)
	resource := DatabaseResource(in.InstanceID, "")
	if !m.deps.Authorize(actor, "databases:credentials", resource) {
		return result, failure("forbidden", "Original administrator credential grants are no longer valid", 403)
	}
	forward := databaseAdminInput{InstanceID: in.InstanceID, CurrentCredentialVersion: in.CurrentCredentialVersion, Command: "rotate"}
	if in.PasswordVersion != "" {
		raw, err := m.deps.State.Get(ctx, "database_admin_password_versions", in.PasswordVersion)
		if err != nil {
			return result, failure("credential_unavailable", "Pinned custom administrator password is unavailable", 412)
		}
		var pin databaseAdminPasswordVersion
		if json.Unmarshal(raw, &pin) != nil || pin.Version != in.PasswordVersion || pin.InstanceID != in.InstanceID || pin.CurrentCredentialVersion != in.CurrentCredentialVersion || pin.SecretRef == "" {
			return result, failure("credential_unavailable", "Pinned custom administrator password does not match this rotation", 412)
		}
		secret, err := m.deps.Secrets.Get(ctx, actor, resource, pin.SecretRef)
		if err != nil {
			return result, failure("credential_unavailable", "Pinned custom administrator password cannot be resolved", 412)
		}
		forward.Password = string(secret)
		clear(secret)
		if !validDatabaseAdminPassword(forward.Password) {
			return result, failure("credential_unavailable", "Pinned custom administrator password is invalid", 412)
		}
	}
	input, _ := json.Marshal(forward)
	defer clear(input)
	reply, err := e.client.Run(ctx, agent.Request{Operation: "database_admin_action", Input: input})
	if err != nil || reply.ExitCode != 0 {
		return result, failure("credential_rotation_failed", "Protected administrator rotation failed; inspect its current state before recovery", 502)
	}
	var state databaseAdminState
	if parse(json.RawMessage(reply.Stdout), &state) != nil || !validDatabaseAdminVersion(state.CurrentCredentialVersion) || state.State != "active" || len(state.AvailableActions) != 1 || state.AvailableActions[0] != "rotate" {
		return result, failure("credential_rotation_failed", "Protected administrator rotation returned invalid state", 502)
	}
	safe, _ := json.Marshal(databaseAdminState{CurrentCredentialVersion: state.CurrentCredentialVersion, State: "active", AvailableActions: []string{"rotate"}})
	result.Stdout = string(safe)
	result.ExitCode = 0
	return result, nil
}
