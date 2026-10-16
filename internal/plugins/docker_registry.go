package plugins

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/jobs"
)

const dockerRegistryCredentialTTL = 30 * time.Minute

type dockerRegistrySecret struct {
	Username string `json:"username"`
	Password []byte `json:"password"`
}

type dockerRegistryPullTask struct {
	ID            string `json:"id"`
	Image         string `json:"image"`
	CredentialRef string `json:"credential_ref"`
}

func dockerRegistryAddress(image string) (string, error) {
	reference, _, ok := strings.Cut(image, "@sha256:")
	if !ok || reference == "" {
		return "", failure("invalid_image", "Registry authentication requires a digest-pinned image reference", 400)
	}
	first, _, _ := strings.Cut(reference, "/")
	if first == "docker.io" || first == "index.docker.io" {
		return "https://index.docker.io/v1/", nil
	}
	if first == "localhost" || strings.ContainsAny(first, ".:") {
		if host, port, err := net.SplitHostPort(first); err == nil {
			parsed, parseErr := strconv.Atoi(port)
			if host == "" || parseErr != nil || parsed < 1 || parsed > 65535 {
				return "", failure("invalid_image", "Registry port is invalid", 400)
			}
		}
		return first, nil
	}
	if first != "" {
		return "https://index.docker.io/v1/", nil
	}
	return "", failure("invalid_image", "Image registry reference is invalid", 400)
}

func validateDockerImageReference(image string) error {
	if !dockerImage.MatchString(image) {
		return failure("invalid_image", "Images must use an immutable SHA-256 digest", 400)
	}
	_, err := dockerRegistryAddress(image)
	return err
}

func dockerRegistryAuthHeader(input DockerInput) (string, error) {
	if input.RegistryUsername == "" && input.RegistryPassword == "" {
		return "", nil
	}
	if input.RegistryUsername == "" || input.RegistryPassword == "" {
		return "", failure("invalid_input", "Registry username and password must be provided together", 400)
	}
	address, err := dockerRegistryAddress(input.Image)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		ServerAddress string `json:"serveraddress"`
	}{input.RegistryUsername, input.RegistryPassword, address})
	if err != nil {
		return "", failure("invalid_input", "Registry credentials could not be encoded", 400)
	}
	// Docker Engine expects AuthConfig JSON as padded base64url in
	// X-Registry-Auth. Standard base64 can contain '+' and '/', which are not
	// part of the URL-safe encoding expected by the Engine API.
	header := base64.URLEncoding.EncodeToString(encoded)
	clear(encoded)
	return header, nil
}

func (m *Manager) dockerImagePullBinding() core.Binding {
	binding := m.binding("docker_image_pull", "Queue a digest-pinned image pull; optional registry credentials are secret-scoped to this image and removed after task completion", "containers:manage", false,
		obj(map[string]any{
			"id":                idSchema(),
			"image":             map[string]any{"type": "string", "pattern": dockerImage.String()},
			"registry_username": map[string]any{"type": "string", "minLength": 1, "maxLength": 512, "format": "password", "writeOnly": true, "description": "私有仓库用户名；填写用户名时必须同时填写密码。"},
			"registry_password": map[string]any{"type": "string", "minLength": 1, "maxLength": 4096, "format": "password", "writeOnly": true, "description": "仅用于本次摘要镜像拉取；任务仅保存加密凭据引用。"},
		}, "id", "image"), dockerImageResources, m.dockerImagePullAction)
	binding.Action.Task = true
	return binding
}

func (m *Manager) dockerImagePullAction(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var input DockerInput
	if err := parse(raw, &input); err != nil {
		return core.ActionResult{}, err
	}
	if !validInstanceID(input.ID) || validateDockerImageReference(input.Image) != nil || (input.RegistryUsername == "") != (input.RegistryPassword == "") || len(input.RegistryUsername) > 512 || len(input.RegistryPassword) > 4096 || strings.ContainsRune(input.RegistryUsername, 0) || strings.ContainsRune(input.RegistryPassword, 0) {
		return core.ActionResult{}, failure("invalid_input", "A digest-pinned image and complete bounded registry credentials are required", 400)
	}
	resources, err := dockerImageResources(ctx, actor, raw)
	if err != nil {
		return core.ActionResult{}, err
	}
	input.Command = "image_pull"
	if input.RegistryUsername == "" {
		forward, _ := json.Marshal(input)
		job, err := m.deps.Tasks.Submit(ctx, actor, "docker_action", forward, resources)
		if err != nil {
			return core.ActionResult{}, err
		}
		return dockerPullTaskResult(job, resources), nil
	}
	if m.deps.Secrets == nil {
		return core.ActionResult{}, failure("secret_store_unavailable", "Private registry pulls require protected credential storage", 503)
	}
	expiringSecrets, ok := m.deps.Secrets.(core.ExpiringSecretStore)
	if !ok {
		return core.ActionResult{}, failure("secret_expiry_unavailable", "Private registry pulls require expiring protected credential storage", 503)
	}
	resource := DockerImageResource(input.ID, input.Image)
	secretValue, err := json.Marshal(dockerRegistrySecret{Username: input.RegistryUsername, Password: []byte(input.RegistryPassword)})
	if err != nil {
		return core.ActionResult{}, failure("invalid_input", "Registry credentials could not be prepared", 400)
	}
	credentialRef, err := expiringSecrets.PutExpiring(ctx, actor, resource, "docker registry pull credential", secretValue, time.Now().UTC().Add(dockerRegistryCredentialTTL))
	clear(secretValue)
	if err != nil {
		return core.ActionResult{}, err
	}
	taskInput, err := json.Marshal(dockerRegistryPullTask{ID: input.ID, Image: input.Image, CredentialRef: credentialRef})
	if err != nil {
		_ = m.deps.Secrets.Delete(ctx, actor, resource, credentialRef)
		return core.ActionResult{}, failure("invalid_input", "Private registry task could not be prepared", 400)
	}
	job, err := m.deps.Tasks.SubmitPrepared(ctx, actor, "docker_registry_pull", taskInput, core.TaskMetadata{Resources: resources}, func(core.MetadataStore, jobs.Job) error { return nil })
	if err != nil {
		_ = m.deps.Secrets.Delete(ctx, actor, resource, credentialRef)
		return core.ActionResult{}, err
	}
	return dockerPullTaskResult(job, resources), nil
}

func dockerPullTaskResult(job map[string]any, resources []string) core.ActionResult {
	id, _ := job["id"].(string)
	if id == "" {
		id, _ = job["task_id"].(string)
	}
	return core.ActionResult{Data: job, TaskID: id, Resources: resources, AuditSummary: map[string]any{"image_resource": resources[0], "task_id": id}}
}

func (m *Manager) runDockerRegistryPull(ctx context.Context, request agent.Request, actor core.Actor) (result agent.Result, returned error) {
	result = agent.Result{Operation: request.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	defer func() { result.FinishedAt = time.Now().UTC() }()
	var task dockerRegistryPullTask
	if err := parse(request.Input, &task); err != nil {
		return result, err
	}
	if request.JobID == "" {
		return result, failure("invalid_input", "Private registry pull must run as a durable task", 400)
	}
	resource := DockerImageResource(task.ID, task.Image)
	defer m.cleanupDockerRegistryPullCredential(ctx, actor, resource, task.CredentialRef)
	if !m.deps.Authorize(actor, "containers:manage", InstanceResource(task.ID)) || !m.deps.Authorize(actor, "containers:manage", resource) {
		return result, failure("forbidden", "Current authorization no longer permits this private image pull", 403)
	}
	if m.executor == nil || m.deps.Secrets == nil {
		return result, failure("agent_unavailable", "Private registry pull requires a protected Agent and secret store", 503)
	}
	secret, err := m.deps.Secrets.Get(ctx, actor, resource, task.CredentialRef)
	if err != nil {
		return result, failure("credential_unavailable", "Private registry credential is no longer available", 412)
	}
	defer clear(secret)
	var credentials dockerRegistrySecret
	if json.Unmarshal(secret, &credentials) != nil || credentials.Username == "" || len(credentials.Password) == 0 {
		clear(credentials.Password)
		return result, failure("credential_invalid", "Private registry credential is invalid", 412)
	}
	defer clear(credentials.Password)
	forward, err := json.Marshal(DockerInput{ID: task.ID, Command: "image_pull", Image: task.Image, RegistryUsername: credentials.Username, RegistryPassword: string(credentials.Password)})
	if err != nil {
		return result, failure("credential_invalid", "Private registry request could not be prepared", 412)
	}
	defer clear(forward)
	agentRequest := agent.Request{ActorID: request.ActorID, JobID: request.JobID, Operation: "docker_action", Input: forward, Output: request.Output}
	if err = m.executor.Validate(agentRequest); err != nil {
		return result, failure("invalid_input", "Agent rejected the typed private registry pull", 400)
	}
	result, err = m.executor.Run(ctx, agentRequest)
	result.Operation = request.Operation
	return result, err
}

func (m *Manager) cleanupDockerRegistryPullCredential(ctx context.Context, actor core.Actor, resource, credentialRef string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if m.deps.Secrets != nil && m.deps.Authorize(actor, "containers:manage", resource) {
		_ = m.deps.Secrets.Delete(cleanupCtx, actor, resource, credentialRef)
	}
}
