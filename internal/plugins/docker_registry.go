package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/jobs"
)

// Secret references stay inside API metadata; public views and durable jobs use IDs.
type dockerRegistryCredential struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Registry  string `json:"registry"`
	Username  string `json:"username"`
	SecretRef string `json:"secret_ref,omitempty"`
	Revision  int64  `json:"revision"`
	Deleted   bool   `json:"deleted,omitempty"`
}
type dockerRegistryAuth struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	ServerAddress string `json:"serveraddress"`
}
type DockerPullInput struct {
	ID                 string `json:"id"`
	Image              string `json:"image"`
	CredentialID       string `json:"credential_id,omitempty"`
	CredentialRevision int64  `json:"credential_revision,omitempty"`
}

var registryDNS = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)

func canonicalRegistry(host string) bool {
	if host == "" || host != strings.ToLower(host) || strings.ContainsAny(host, "/\\@\x00\r\n") {
		return false
	}
	h := host
	if strings.Contains(host, ":") {
		var port string
		var err error
		h, port, err = net.SplitHostPort(host)
		if err != nil || port == "" {
			return false
		}
		for _, r := range port {
			if r < '0' || r > '9' {
				return false
			}
		}
		number, parseErr := strconv.Atoi(port)
		if len(port) > 5 || parseErr != nil || number < 1 || number > 65535 {
			return false
		}
	}
	return len(host) <= 253 && registryDNS.MatchString(h) && !strings.Contains(h, "..")
}
func imageRegistry(image string) string {
	repo, _, ok := strings.Cut(image, "@sha256:")
	if !ok {
		return ""
	}
	first, _, slash := strings.Cut(repo, "/")
	if slash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return first
	}
	return "docker.io"
}
func validDockerRegistryAuth(image string, auth *dockerRegistryAuth) bool {
	return auth != nil && canonicalRegistry(auth.ServerAddress) && imageRegistry(image) == auth.ServerAddress && auth.Username != "" && len(auth.Username) <= 256 && auth.Password != "" && len(auth.Password) <= 4096 && !strings.ContainsAny(auth.Username+auth.Password, "\x00\r\n")
}
func DockerRegistryResource(id string) string        { return "docker:registry:" + id }
func registryVersionKey(id string, rev int64) string { return fmt.Sprintf("%s@%d", id, rev) }
func registryPublic(v dockerRegistryCredential) any {
	return map[string]any{"id": v.ID, "name": v.Name, "registry": v.Registry, "username": v.Username, "revision": v.Revision, "credential_configured": !v.Deleted && v.SecretRef != ""}
}
func loadRegistry(ctx context.Context, store core.MetadataStore, id string) (dockerRegistryCredential, error) {
	raw, rev, err := store.GetVersioned(ctx, "docker_registry_credentials", id)
	var v dockerRegistryCredential
	if err != nil {
		return v, err
	}
	if json.Unmarshal(raw, &v) != nil || v.ID != id || v.Revision != rev {
		return v, failure("invalid_state", "Registry credential metadata is invalid", 500)
	}
	if v.Deleted {
		return v, failure("not_found", "Registry credential does not exist", 404)
	}
	return v, nil
}
func registryResources(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var in struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &in) != nil || !validInstanceID(in.ID) {
		return nil, failure("invalid_input", "A credential ID is required", 400)
	}
	return []string{DockerRegistryResource(in.ID)}, nil
}
func dockerPullResources(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var in DockerPullInput
	if err := parse(raw, &in); err != nil {
		return nil, err
	}
	if !validInstanceID(in.ID) || !dockerImage.MatchString(in.Image) || (in.CredentialID == "" && in.CredentialRevision != 0) || (in.CredentialID != "" && (!validInstanceID(in.CredentialID) || in.CredentialRevision <= 0)) {
		return nil, failure("invalid_input", "Digest image and optional pinned credential are required", 400)
	}
	resources := []string{InstanceResource(in.ID), DockerImageResource(in.ID, in.Image)}
	if in.CredentialID != "" {
		resources = append(resources, DockerRegistryResource(in.CredentialID))
	}
	return resources, nil
}
func (m *Manager) dockerRegistryBindings() []core.Binding {
	out := []core.Binding{}
	for _, kind := range []string{"create", "rotate", "get", "list", "delete"} {
		kind := kind
		read := kind == "get" || kind == "list"
		scope := "containers:manage"
		if read {
			scope = "containers:read"
		}
		fields := map[string]any{"id": idSchema()}
		required := []string{"id"}
		resources := registryResources
		if kind == "create" || kind == "rotate" {
			fields["name"] = textSchema()
			fields["registry"] = textSchema()
			fields["username"] = textSchema()
			fields["password"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 4096, "format": "password"}
			required = append(required, "name", "registry", "username", "password")
		}
		if kind == "rotate" || kind == "delete" {
			fields["expected_revision"] = revisionSchema()
			required = append(required, "expected_revision")
		}
		if kind == "create" {
			delete(fields, "id")
			required = required[1:]
			resources = func(context.Context, core.Actor, json.RawMessage) ([]string, error) {
				return []string{DockerRegistryResource("create")}, nil
			}
		}
		if kind == "list" {
			fields = map[string]any{}
			required = nil
			resources = func(context.Context, core.Actor, json.RawMessage) ([]string, error) { return nil, nil }
		}
		out = append(out, m.binding("docker_registry_credential_"+kind, "Manage encrypted versioned registry credentials without returning secrets", "containers:manage", read, obj(fields, required...), resources, func(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			return m.registryAction(ctx, a, kind, raw)
		}))
		out[len(out)-1].Action.Scope = scope
	}
	pull := m.binding("docker_image_pull", "Pull an exact digest using an optional immutable registry credential version", "containers:manage", false, obj(map[string]any{"id": idSchema(), "image": map[string]any{"type": "string", "pattern": dockerImage.String()}, "credential_id": idSchema(), "credential_revision": revisionSchema()}, "id", "image"), dockerPullResources, func(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		var in DockerPullInput
		if err := parse(raw, &in); err != nil {
			return core.ActionResult{}, err
		}
		resources, err := dockerPullResources(ctx, a, raw)
		if err != nil {
			return core.ActionResult{}, err
		}
		job, err := m.deps.Tasks.SubmitPrepared(ctx, a, "docker_registry_pull", raw, core.TaskMetadata{Resources: resources}, func(store core.MetadataStore, _ jobs.Job) error {
			if in.CredentialID == "" {
				return nil
			}
			v, err := loadRegistry(ctx, store, in.CredentialID)
			if err != nil {
				return err
			}
			if v.Revision != in.CredentialRevision {
				return failure("revision_conflict", "Registry credential changed before submission", 412)
			}
			if v.Registry != imageRegistry(in.Image) {
				return failure("invalid_registry", "Credential does not match the exact image registry", 400)
			}
			data, _ := json.Marshal(v)
			saved, err := store.Get(ctx, "docker_registry_versions", registryVersionKey(v.ID, v.Revision))
			if err != nil {
				return err
			}
			if string(saved) != string(data) {
				return failure("invalid_state", "Registry credential version is invalid", 500)
			}
			return nil
		})
		id, _ := job["id"].(string)
		return core.ActionResult{Data: job, TaskID: id, Resources: resources}, err
	})
	pull.Action.Task = true
	out = append(out, pull)
	return out
}
func (m *Manager) registryAction(ctx context.Context, a core.Actor, kind string, raw json.RawMessage) (core.ActionResult, error) {
	if kind == "list" {
		rows, err := m.deps.State.List(ctx, "docker_registry_credentials")
		if err != nil {
			return core.ActionResult{}, err
		}
		items := []any{}
		for id := range rows {
			if !m.deps.Authorize(a, "containers:read", DockerRegistryResource(id)) {
				continue
			}
			v, err := loadRegistry(ctx, m.deps.State, id)
			if core.ErrorCode(err) == "not_found" {
				continue
			}
			if err != nil {
				return core.ActionResult{}, err
			}
			items = append(items, registryPublic(v))
		}
		return core.ActionResult{Data: map[string]any{"items": items}}, nil
	}
	var in struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Registry string `json:"registry"`
		Username string `json:"username"`
		Password string `json:"password"`
		Expected int64  `json:"expected_revision"`
	}
	if err := parse(raw, &in); err != nil {
		return core.ActionResult{}, err
	}
	if kind == "create" {
		id, err := randomPlanID()
		if err != nil {
			return core.ActionResult{}, err
		}
		in.ID = "reg-" + id
	}
	resource := DockerRegistryResource(in.ID)
	if kind == "get" {
		v, err := loadRegistry(ctx, m.deps.State, in.ID)
		return core.ActionResult{Data: registryPublic(v), Resources: []string{resource}}, err
	}
	if !m.deps.Authorize(a, "containers:manage", resource) {
		return core.ActionResult{}, failure("forbidden", "Registry management grant is required", 403)
	}
	v := dockerRegistryCredential{ID: in.ID, Name: in.Name, Registry: in.Registry, Username: in.Username, Revision: in.Expected + 1}
	if kind == "delete" {
		v.Deleted = true
	} else {
		if !canonicalRegistry(in.Registry) || len(in.Name) == 0 || len(in.Name) > 256 || !validDockerRegistryAuth(in.Registry+"/image@sha256:"+strings.Repeat("a", 64), &dockerRegistryAuth{in.Username, in.Password, in.Registry}) {
			return core.ActionResult{}, failure("invalid_input", "Bounded credentials and a canonical registry host are required", 400)
		}
		ref, err := m.deps.Secrets.Put(ctx, a, resource, "docker-registry-password", []byte(in.Password))
		if err != nil {
			return core.ActionResult{}, err
		}
		v.SecretRef = ref
	}
	data, _ := json.Marshal(v)
	err := m.deps.State.Transaction(ctx, func(store core.MetadataStore) error {
		if kind != "create" {
			old, err := loadRegistry(ctx, store, in.ID)
			if err != nil {
				return err
			}
			if old.Revision != in.Expected {
				return failure("revision_conflict", "Registry credential revision changed", 412)
			}
		}
		if _, err := store.CompareAndSwap(ctx, "docker_registry_credentials", in.ID, in.Expected, data); err != nil {
			return err
		}
		if !v.Deleted {
			_, err := store.CompareAndSwap(ctx, "docker_registry_versions", registryVersionKey(v.ID, v.Revision), 0, data)
			return err
		}
		return nil
	})
	if err != nil && v.SecretRef != "" {
		_ = m.deps.Secrets.Delete(ctx, a, resource, v.SecretRef)
	}
	if kind == "delete" {
		return core.ActionResult{Data: map[string]any{"id": in.ID, "deleted": err == nil}, Resources: []string{resource}}, err
	}
	if kind == "create" {
		resource = DockerRegistryResource("create")
	}
	return core.ActionResult{Data: registryPublic(v), Resources: []string{resource}}, err
}
