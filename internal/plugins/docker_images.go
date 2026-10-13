package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

var dockerImageID = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// DockerImageResource binds access to the exact lower-case RepoDigest string
// supplied by Docker. Tags and daemon-local image IDs are never accepted here;
// repository aliases therefore resolve to distinct explicit grants.
func DockerImageResource(instanceID, image string) string {
	digest := sha256.Sum256([]byte(image))
	return "docker:image:" + instanceID + ":" + hex.EncodeToString(digest[:])
}

type dockerImageInspection struct {
	ID           string   `json:"Id"`
	RepoDigests  []string `json:"RepoDigests"`
	Created      string   `json:"Created"`
	Architecture string   `json:"Architecture"`
	OS           string   `json:"Os"`
	Size         int64    `json:"Size"`
}

type dockerImageView struct {
	Image        string `json:"image"`
	ID           string `json:"id"`
	Created      string `json:"created,omitempty"`
	Architecture string `json:"architecture,omitempty"`
	OS           string `json:"os,omitempty"`
	SizeBytes    int64  `json:"size_bytes"`
}

func (e *PrivilegedExecutor) inspectDockerImage(ctx context.Context, instance Instance, image string) (dockerImageInspection, error) {
	var inspected dockerImageInspection
	path := "/images/" + url.PathEscape(image) + "/json"
	if err := e.dockerRequest(ctx, instance, http.MethodGet, path, nil, &inspected); err != nil {
		return dockerImageInspection{}, err
	}
	if !dockerImageID.MatchString(inspected.ID) || inspected.Size < 0 {
		return dockerImageInspection{}, failure("invalid_response", "Docker returned invalid image metadata", 502)
	}
	for _, repoDigest := range inspected.RepoDigests {
		if repoDigest == image {
			return inspected, nil
		}
	}
	return dockerImageInspection{}, failure("ownership_invalid", "Docker did not confirm the exact digest-qualified image reference", 412)
}

func (e *PrivilegedExecutor) dockerImageAction(ctx context.Context, instance Instance, input DockerInput) (any, error) {
	inspected, err := e.inspectDockerImage(ctx, instance, input.Image)
	if err != nil {
		return nil, err
	}
	if input.Command == "image_inspect" {
		return dockerImageView{Image: input.Image, ID: inspected.ID, Created: inspected.Created, Architecture: inspected.Architecture, OS: inspected.OS, SizeBytes: inspected.Size}, nil
	}
	if input.Command != "image_remove" {
		return nil, failure("invalid_input", "Unsupported typed Docker image action", 400)
	}
	query := url.Values{}
	query.Set("force", "false")
	query.Set("noprune", "true")
	if err = e.dockerRequest(ctx, instance, http.MethodDelete, "/images/"+url.PathEscape(input.Image)+"?"+query.Encode(), nil, nil); err != nil {
		return nil, err
	}
	return map[string]any{"image": input.Image, "image_id": inspected.ID, "removed": true}, nil
}

func dockerImageResources(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var input DockerInput
	if err := parse(raw, &input); err != nil {
		return nil, err
	}
	if !validInstanceID(input.ID) || !dockerImage.MatchString(input.Image) {
		return nil, failure("invalid_input", "A managed instance and immutable image digest are required", 400)
	}
	return []string{DockerImageResource(input.ID, input.Image), InstanceResource(input.ID)}, nil
}

func (m *Manager) dockerImageBindings() []core.Binding {
	bindings := make([]core.Binding, 0, 2)
	for _, command := range []string{"image_inspect", "image_remove"} {
		command := command
		read := command == "image_inspect"
		scope := "containers:manage"
		if read {
			scope = "containers:read"
		}
		binding := m.binding("docker_"+command, "Inspect or remove one digest-pinned image in its managed Docker instance", scope, read,
			obj(map[string]any{"id": idSchema(), "image": map[string]any{"type": "string", "pattern": dockerImage.String()}}, "id", "image"), dockerImageResources,
			func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
				var input DockerInput
				if err := parse(raw, &input); err != nil {
					return core.ActionResult{}, err
				}
				input.Command = command
				forward, _ := json.Marshal(input)
				resources, err := dockerImageResources(ctx, actor, forward)
				if err != nil {
					return core.ActionResult{}, err
				}
				if read {
					var output dockerImageView
					if err = m.agentJSON(ctx, "docker_action", forward, &output); err != nil {
						return core.ActionResult{}, err
					}
					return core.ActionResult{Data: output, Resources: resources, AuditSummary: map[string]any{"image_resource": resources[0]}}, nil
				}
				job, err := m.deps.Tasks.Submit(ctx, actor, "docker_action", forward, resources)
				if err != nil {
					return core.ActionResult{}, err
				}
				id, _ := job["id"].(string)
				return core.ActionResult{Data: job, TaskID: id, Resources: resources, AuditSummary: map[string]any{"image_resource": resources[0]}}, nil
			})
		binding.Action.Audit = true
		binding.Action.Task = !read
		bindings = append(bindings, binding)
	}
	return bindings
}

var _ agent.Executor = (*PrivilegedExecutor)(nil)
