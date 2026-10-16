package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"

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
	RepoTags     []string `json:"RepoTags"`
	RepoDigests  []string `json:"RepoDigests"`
	Created      string   `json:"Created"`
	Architecture string   `json:"Architecture"`
	OS           string   `json:"Os"`
	Size         int64    `json:"Size"`
}

type dockerImageView struct {
	Image               string `json:"image"`
	ID                  string `json:"id"`
	Created             string `json:"created,omitempty"`
	Architecture        string `json:"architecture,omitempty"`
	OS                  string `json:"os,omitempty"`
	SizeBytes           int64  `json:"size_bytes"`
	RestoredFromArchive bool   `json:"restored_from_archive,omitempty"`
}

// Hub equivalence is only an Engine ownership proof. Authorization and public
// list references retain the caller/Engine's exact spelling. These restricted
// rules follow distribution/reference v0.6.0 splitDockerDomain: index.docker.io
// is a legacy Hub alias, absent domains mean Hub, and single Hub names live in
// library/. Explicit private registries (including Hub-looking hosts with a
// port) are never familiarized.
var dockerHubRepository = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)

func dockerHubDigestProof(ref string) string {
	if !dockerImage.MatchString(ref) {
		return ""
	}
	repository, digest, ok := strings.Cut(ref, "@")
	if !ok {
		return ""
	}
	first, remainder, slash := strings.Cut(repository, "/")
	if slash {
		switch {
		case first == "docker.io" || first == "index.docker.io":
			repository = remainder
		case first == "localhost" || strings.ContainsAny(first, ".:"):
			return ""
		}
	}
	if !dockerHubRepository.MatchString(repository) {
		return ""
	}
	if !strings.ContainsRune(repository, '/') {
		repository = "library/" + repository
	}
	return "docker.io/" + repository + "@" + digest
}

func dockerImageDigestProofMatches(requested, supplied string) bool {
	if requested == supplied {
		return true
	}
	hub := dockerHubDigestProof(requested)
	return hub != "" && hub == dockerHubDigestProof(supplied)
}

func (e *PrivilegedExecutor) inspectDockerImage(ctx context.Context, instance Instance, image string) (dockerImageInspection, error) {
	inspected, err := e.inspectDockerImageMetadata(ctx, instance, image)
	if err != nil {
		return dockerImageInspection{}, err
	}
	for _, repoDigest := range inspected.RepoDigests {
		if dockerImageDigestProofMatches(image, repoDigest) {
			return inspected, nil
		}
	}
	return dockerImageInspection{}, failure("ownership_invalid", "Docker did not confirm the exact digest-qualified image reference", 412)
}

func (e *PrivilegedExecutor) inspectDockerImageMetadata(ctx context.Context, instance Instance, image string) (dockerImageInspection, error) {
	var inspected dockerImageInspection
	path := "/images/" + url.PathEscape(image) + "/json"
	if err := e.dockerRequest(ctx, instance, http.MethodGet, path, nil, &inspected); err != nil {
		return dockerImageInspection{}, err
	}
	if !dockerImageID.MatchString(inspected.ID) || inspected.Size < 0 || len(inspected.RepoTags) > 256 || len(inspected.RepoDigests) > 256 {
		return dockerImageInspection{}, failure("invalid_response", "Docker returned invalid image metadata", 502)
	}
	for _, repoDigest := range inspected.RepoDigests {
		if !dockerImage.MatchString(repoDigest) {
			return dockerImageInspection{}, failure("invalid_response", "Docker returned an invalid RepoDigest", 502)
		}
	}
	for _, tag := range inspected.RepoTags {
		if tag == "<none>:<none>" {
			continue
		}
		if len(tag) > 255 || strings.ContainsAny(tag, "\x00\r\n\t ") {
			return dockerImageInspection{}, failure("invalid_response", "Docker returned an invalid RepoTag", 502)
		}
	}
	return inspected, nil
}

func (e *PrivilegedExecutor) dockerImageAction(ctx context.Context, instance Instance, input DockerInput) (any, error) {
	inspected, restored, err := e.resolveDockerImageArchive(ctx, instance, input.Image)
	if err != nil {
		return nil, err
	}
	if input.Command == "image_inspect" {
		return dockerImageView{Image: input.Image, ID: inspected.ID, Created: inspected.Created, Architecture: inspected.Architecture, OS: inspected.OS, SizeBytes: inspected.Size, RestoredFromArchive: restored}, nil
	}
	if input.Command != "image_remove" {
		return nil, failure("invalid_input", "Unsupported typed Docker image action", 400)
	}
	query := url.Values{}
	query.Set("force", "false")
	query.Set("noprune", "true")
	removeRef := input.Image
	if restored {
		// The receipt resolver only returns ID-only images with no RepoTags or
		// RepoDigests, so removing by digest here cannot revoke another alias.
		removeRef = inspected.ID
	}
	if err = e.dockerRequest(ctx, instance, http.MethodDelete, "/images/"+url.PathEscape(removeRef)+"?"+query.Encode(), nil, nil); err != nil {
		return nil, err
	}
	return map[string]any{"image": input.Image, "image_id": inspected.ID, "removed": true, "restored_from_archive": restored}, nil
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

// List expands only actual RepoDigests from Docker, never tags or daemon IDs.
func (m *Manager) dockerImagesListBinding() core.Binding {
	return m.binding("docker_images_list", "List authorized exact digest-qualified Docker images", "containers:read", true, obj(map[string]any{"id": idSchema()}, "id"), idResource,
		func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			var input IDInput
			if err := parse(raw, &input); err != nil {
				return core.ActionResult{}, err
			}
			forward, _ := json.Marshal(DockerInput{ID: input.ID, Command: "images_list"})
			var output struct {
				Items []struct {
					ID          string   `json:"Id"`
					RepoDigests []string `json:"RepoDigests"`
					Size        int64    `json:"Size"`
				} `json:"items"`
				RestoredFromArchive []dockerImageView `json:"restored_from_archive"`
			}
			if err := m.agentJSON(ctx, "docker_action", forward, &output); err != nil {
				return core.ActionResult{}, err
			}
			items := []dockerImageView{}
			seen := map[string]bool{}
			for _, image := range output.Items {
				if !dockerImageID.MatchString(image.ID) || image.Size < 0 {
					continue
				}
				for _, ref := range image.RepoDigests {
					if !dockerImage.MatchString(ref) || seen[ref] || !m.deps.Authorize(actor, "containers:read", InstanceResource(input.ID)) || !m.deps.Authorize(actor, "containers:read", DockerImageResource(input.ID, ref)) {
						continue
					}
					seen[ref] = true
					items = append(items, dockerImageView{Image: ref, ID: image.ID, SizeBytes: image.Size})
				}
			}
			for _, image := range output.RestoredFromArchive {
				if !dockerImage.MatchString(image.Image) || !dockerImageID.MatchString(image.ID) || !image.RestoredFromArchive || seen[image.Image] || !m.deps.Authorize(actor, "containers:read", InstanceResource(input.ID)) || !m.deps.Authorize(actor, "containers:read", DockerImageResource(input.ID, image.Image)) {
					continue
				}
				seen[image.Image] = true
				items = append(items, image)
			}
			return core.ActionResult{Data: map[string]any{"items": items}, Resources: []string{InstanceResource(input.ID)}}, nil
		})
}
