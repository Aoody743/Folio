package terminal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"regexp"
	"sort"
	"strings"
)

var hostName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]{0,252}$`)
var userName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]{0,63}$`)

func raw(value any) json.RawMessage { encoded, _ := json.Marshal(value); return encoded }
func validateTarget(t Target) error {
	if len(t.Label) > 128 || t.Label == "" || t.Port < 1 || t.Port > 65535 || (!hostName.MatchString(t.Host) && net.ParseIP(t.Host) == nil) || !userName.MatchString(t.Username) {
		return failure("invalid_input", "Invalid SSH target address, account or label", 400)
	}
	if t.AuthType != "password" && t.AuthType != "key" {
		return failure("invalid_input", "SSH authentication must be password or key", 400)
	}
	if len(t.SecretRef) == 0 || len(t.SecretRef) > 256 {
		return failure("missing_secret", "A bound secret reference is required", 422)
	}
	fingerprint := strings.TrimPrefix(t.HostKey, "SHA256:")
	decoded, err := base64.RawStdEncoding.DecodeString(fingerprint)
	if !strings.HasPrefix(t.HostKey, "SHA256:") || err != nil || len(decoded) != 32 {
		return failure("invalid_input", "SSH target needs a pinned SHA256 host-key fingerprint", 400)
	}
	return nil
}
func (m *Manager) GetTarget(ctx context.Context, id string) (Target, error) {
	if p, ok := m.profiles[id]; ok {
		label := p.Label
		if label == "" {
			label = p.ID
		}
		return Target{ID: p.ID, Label: label, Type: "local", Username: p.User, Root: p.root, Revision: 1}, nil
	}
	if !safeID.MatchString(id) {
		return Target{}, failure("invalid_input", "Invalid terminal target ID", 400)
	}
	value, err := m.store.Get(ctx, "terminal_targets", id)
	if errors.Is(err, fs.ErrNotExist) {
		return Target{}, failure("not_found", "Terminal target not found", 404)
	}
	if err != nil {
		return Target{}, err
	}
	var target Target
	if err := json.Unmarshal(value, &target); err != nil {
		return Target{}, err
	}
	if target.ID != id || target.Type != "ssh" {
		return Target{}, errors.New("invalid terminal target record")
	}
	if err := validateTarget(target); err != nil {
		return Target{}, err
	}
	return target, nil
}
func (m *Manager) Targets(ctx context.Context) ([]Target, error) {
	out := []Target{}
	for id := range m.profiles {
		target, err := m.GetTarget(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, target)
	}
	all, err := m.store.List(ctx, "terminal_targets")
	if err != nil {
		return nil, err
	}
	for id := range all {
		target, err := m.GetTarget(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, target)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}
func (m *Manager) CreateTarget(ctx context.Context, actorID string, in Input) (Target, error) {
	return m.CreateTargetWithID(ctx, actorID, newID(), in)
}

// CreateTargetWithID accepts an ID issued and authorized by the trusted adapter.
// It never overwrites an existing SSH target or an operator-owned local profile.
func (m *Manager) CreateTargetWithID(ctx context.Context, actorID, issuedID string, in Input) (Target, error) {
	if !safeID.MatchString(issuedID) || actorID == "" {
		return Target{}, failure("invalid_input", "Invalid issued target ID or actor", 400)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.profiles[issuedID]; exists {
		return Target{}, failure("conflict", "Target ID already exists", 409)
	}
	if _, err := m.store.Get(ctx, "terminal_targets", issuedID); err == nil {
		return Target{}, failure("conflict", "Target ID already exists", 409)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Target{}, err
	}
	target := Target{ID: issuedID, Label: in.Label, Type: "ssh", Host: in.Host, Port: in.Port, Username: in.Username, AuthType: in.AuthType, SecretRef: in.SecretRef, HostKey: in.HostKey, Root: in.Username == "root", CreatedBy: actorID, Revision: 1}
	if target.Port == 0 {
		target.Port = 22
	}
	if err := validateTarget(target); err != nil {
		return Target{}, err
	}
	if err := m.store.Save(ctx, "terminal_targets", target.ID, raw(target)); err != nil {
		return Target{}, err
	}
	return target, nil
}
func (m *Manager) UpdateTarget(ctx context.Context, actorID string, in Input) (Target, error) {
	if _, ok := m.profiles[in.ID]; ok {
		return Target{}, failure("forbidden", "Trusted local profiles cannot be changed through the panel", 403)
	}
	prior, err := m.GetTarget(ctx, in.ID)
	if err != nil {
		return Target{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, err := m.GetTarget(ctx, in.ID)
	if err != nil {
		return Target{}, err
	}
	if in.ExpectedRevision != current.Revision || current.Revision != prior.Revision {
		return Target{}, failure("conflict", "SSH target revision changed", 409)
	}
	identityChanged := (in.Host != "" && in.Host != current.Host) || (in.Username != "" && in.Username != current.Username) || (in.HostKey != "" && in.HostKey != current.HostKey) || (in.AuthType != "" && in.AuthType != current.AuthType)
	if identityChanged && (in.SecretRef == "" || in.SecretRef == current.SecretRef) {
		return Target{}, failure("credential_rebind_required", "Changing SSH identity requires a newly bound credential reference", 409)
	}
	if in.Label != "" {
		current.Label = in.Label
	}
	if in.Host != "" {
		current.Host = in.Host
	}
	if in.Port != 0 {
		current.Port = in.Port
	}
	if in.Username != "" {
		current.Username = in.Username
	}
	if in.AuthType != "" {
		current.AuthType = in.AuthType
	}
	if in.SecretRef != "" {
		current.SecretRef = in.SecretRef
	}
	if in.HostKey != "" {
		current.HostKey = in.HostKey
	}
	current.Root = current.Username == "root"
	current.Revision++
	if err := validateTarget(current); err != nil {
		return Target{}, err
	}
	if err := m.store.Save(ctx, "terminal_targets", current.ID, raw(current)); err != nil {
		return Target{}, err
	}
	return current, nil
}
func (m *Manager) DeleteTarget(ctx context.Context, in Input) (any, error) {
	if _, ok := m.profiles[in.ID]; ok {
		return nil, failure("forbidden", "Trusted local profiles cannot be deleted", 403)
	}
	if _, err := m.GetTarget(ctx, in.ID); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		s.mu.Lock()
		active := s.info.TargetID == in.ID && !s.info.Exited
		s.mu.Unlock()
		if active {
			return nil, failure("conflict", "Close active sessions before deleting the target", 409)
		}
	}
	if err := m.store.Delete(ctx, "terminal_targets", in.ID); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": true, "id": in.ID}, nil
}
func (m *Manager) Commands(ctx context.Context) ([]Command, error) {
	values, err := m.store.List(ctx, "terminal_commands")
	if err != nil {
		return nil, err
	}
	out := []Command{}
	for _, value := range values {
		var command Command
		if err := json.Unmarshal(value, &command); err != nil {
			return nil, err
		}
		out = append(out, command)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out, nil
}
func (m *Manager) SaveCommand(ctx context.Context, actorID string, in Input) (Command, error) {
	return m.SaveCommandWithID(ctx, actorID, newID(), in)
}

// SaveCommandWithID uses an adapter-issued ID for creation. Existing shortcuts
// still require their public ID and revision for an update.
func (m *Manager) SaveCommandWithID(ctx context.Context, actorID, issuedID string, in Input) (Command, error) {
	if !safeID.MatchString(issuedID) || actorID == "" {
		return Command{}, failure("invalid_input", "Invalid issued command ID or actor", 400)
	}
	if len(in.Title) == 0 || len(in.Title) > 128 || len(in.Command) == 0 || len(in.Command) > 4096 || strings.ContainsRune(in.Command, 0) {
		return Command{}, failure("invalid_input", "Shortcut title/command is empty or too long", 400)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	command := Command{ID: issuedID, Title: in.Title, Command: in.Command, CreatedBy: actorID, Revision: 1}
	if in.ID != "" {
		value, err := m.store.Get(ctx, "terminal_commands", in.ID)
		if err != nil {
			return Command{}, err
		}
		if err := json.Unmarshal(value, &command); err != nil {
			return Command{}, err
		}
		if in.ExpectedRevision != command.Revision {
			return Command{}, failure("conflict", "Command shortcut revision changed", 409)
		}
		command.Title, command.Command, command.Revision = in.Title, in.Command, command.Revision+1
	} else {
		if _, err := m.store.Get(ctx, "terminal_commands", issuedID); err == nil {
			return Command{}, failure("conflict", "Command ID already exists", 409)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Command{}, err
		}
	}
	if err := m.store.Save(ctx, "terminal_commands", command.ID, raw(command)); err != nil {
		return Command{}, err
	}
	return command, nil
}
func (m *Manager) DeleteCommand(ctx context.Context, in Input) (any, error) {
	if err := m.store.Delete(ctx, "terminal_commands", in.ID); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": true, "id": in.ID}, nil
}
