package plugins

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

// DockerRegistryExecutor runs only in the API process. The privileged Agent has
// no Core SecretStore and sees credentials only on its authenticated Unix IPC.
type DockerRegistryExecutor struct {
	manager *Manager
	client  *agent.UnixClient
}

func NewDockerRegistryExecutor(manager *Manager, client *agent.UnixClient) *DockerRegistryExecutor {
	return &DockerRegistryExecutor{manager, client}
}
func (e *DockerRegistryExecutor) Validate(r agent.Request) error {
	if r.Operation != "docker_registry_pull" {
		return agent.ErrUnauthorized
	}
	_, err := dockerPullResources(context.Background(), core.Actor{}, r.Input)
	if err != nil {
		return err
	}
	if e.manager == nil || e.client == nil || e.manager.deps.Secrets == nil || e.manager.deps.ResolveActor == nil {
		return failure("agent_unavailable", "Registry pulls require the protected Unix Agent", 503)
	}
	return nil
}
func (e *DockerRegistryExecutor) Run(ctx context.Context, r agent.Request) (result agent.Result, returned error) {
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
	resources, err := dockerPullResources(ctx, actor, r.Input)
	if err != nil {
		return result, err
	}
	for _, resource := range resources {
		if !m.deps.Authorize(actor, "containers:manage", resource) {
			return result, failure("forbidden", "Original image pull grants are no longer valid", 403)
		}
	}
	var in DockerPullInput
	_ = json.Unmarshal(r.Input, &in)
	forward := DockerInput{ID: in.ID, Image: in.Image, Command: "image_pull"}
	if in.CredentialID != "" {
		// A deletion blocks queued starts; rotation may still use the original pin.
		if _, err := loadRegistry(ctx, m.deps.State, in.CredentialID); err != nil {
			return result, err
		}
		raw, err := m.deps.State.Get(ctx, "docker_registry_versions", registryVersionKey(in.CredentialID, in.CredentialRevision))
		if err != nil {
			return result, err
		}
		var version dockerRegistryCredential
		if json.Unmarshal(raw, &version) != nil || version.ID != in.CredentialID || version.Revision != in.CredentialRevision || version.Deleted || version.SecretRef == "" || version.Registry != imageRegistry(in.Image) {
			return result, failure("invalid_state", "Pinned registry credential is unavailable", 412)
		}
		secret, err := m.deps.Secrets.Get(ctx, actor, DockerRegistryResource(in.CredentialID), version.SecretRef)
		if err != nil {
			return result, err
		}
		forward.RegistryAuth = &dockerRegistryAuth{version.Username, string(secret), version.Registry}
		clear(secret)
	}
	input, _ := json.Marshal(forward)
	defer clear(input)
	// Never return transport errors or unvalidated Agent output: either could echo auth.
	reply, err := e.client.Run(ctx, agent.Request{Operation: "docker_action", Input: input})
	if err != nil || reply.ExitCode != 0 {
		return result, failure("image_pull_failed", "Protected digest-pinned image pull failed", 502)
	}
	var view dockerImageView
	if json.Unmarshal([]byte(reply.Stdout), &view) != nil || view.Image != in.Image || !dockerImageID.MatchString(view.ID) || view.SizeBytes < 0 {
		return result, failure("image_pull_failed", "Protected image pull returned invalid metadata", 502)
	}
	safe, _ := json.Marshal(dockerImageView{Image: in.Image, ID: view.ID, SizeBytes: view.SizeBytes})
	result.Stdout = string(safe)
	result.ExitCode = 0
	return result, nil
}
