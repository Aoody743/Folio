package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

type DockerInput struct {
	ID          string            `json:"id"`
	Command     string            `json:"command"`
	Name        string            `json:"name,omitempty"`
	Image       string            `json:"image,omitempty"`
	MemoryMB    int64             `json:"memory_mb,omitempty"`
	CPUs        float64           `json:"cpus,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	Ports       []ContainerPort   `json:"ports,omitempty"`
	Volume      string            `json:"volume,omitempty"`
	Tail        int               `json:"tail,omitempty"`
}
type ContainerPort struct {
	Port     int    `json:"port"`
	HostPort int    `json:"host_port"`
	Protocol string `json:"protocol"`
}

var dockerImage = regexp.MustCompile(`^[a-z0-9][a-z0-9./_-]{0,180}@sha256:[a-f0-9]{64}$`)
var dockerEnv = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

func (e *PrivilegedExecutor) validateDocker(raw json.RawMessage) error {
	var input DockerInput
	if err := parse(raw, &input); err != nil {
		return err
	}
	if !validInstanceID(input.ID) {
		return failure("invalid_input", "Invalid managed Docker instance ID", 400)
	}
	switch input.Command {
	case "status", "containers_list", "images_list", "networks_list", "volumes_list":
	case "image_pull":
		if !dockerImage.MatchString(input.Image) {
			return failure("invalid_image", "Images must use an immutable SHA-256 digest", 400)
		}
	case "container_create":
		if !utilityName.MatchString(input.Name) || !dockerImage.MatchString(input.Image) {
			return failure("invalid_input", "Container requires a simple name and digest-pinned image", 400)
		}
		if input.MemoryMB < 16 || input.MemoryMB > 16384 || input.CPUs <= 0 || input.CPUs > 16 {
			return failure("invalid_input", "Container CPU/memory limits are required and bounded", 400)
		}
		if len(input.Environment) > 32 || len(input.Ports) > 16 {
			return failure("invalid_input", "Container configuration exceeds its bounds", 400)
		}
		for key, value := range input.Environment {
			if !dockerEnv.MatchString(key) || len(value) > 4096 || strings.ContainsRune(value, 0) {
				return failure("invalid_input", "Invalid environment entry", 400)
			}
		}
		for _, port := range input.Ports {
			if port.Port < 1 || port.Port > 65535 || port.HostPort < 1024 || port.HostPort > 65535 || (port.Protocol != "tcp" && port.Protocol != "udp") {
				return failure("invalid_input", "Container ports must be typed and host ports must be unprivileged", 400)
			}
		}
		if input.Volume != "" && !utilityName.MatchString(input.Volume) {
			return failure("invalid_input", "Volume name must be a simple owned identifier", 400)
		}
	case "container_get", "container_start", "container_stop", "container_restart", "container_delete", "container_logs", "network_create", "network_delete", "volume_create", "volume_delete":
		if !utilityName.MatchString(input.Name) {
			return failure("invalid_input", "Docker target requires a simple owned name", 400)
		}
		if input.Tail < 0 || input.Tail > 1000 {
			return failure("invalid_input", "Log tail is limited to 1000 lines", 400)
		}
	default:
		return failure("invalid_input", "Unknown typed Docker action", 400)
	}
	return nil
}
func dockerOwner(instance Instance) map[string]string {
	return map[string]string{"io.powerlinux.managed": "true", "io.powerlinux.instance": instance.ID}
}
func dockerOwnedName(instance Instance, name string) string {
	return "powerlinux-" + instance.ID + "-" + name
}
func (e *PrivilegedExecutor) dockerRequest(ctx context.Context, instance Instance, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(instance.Paths.RunDir, "docker.sock"))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Minute}
	request, err := http.NewRequestWithContext(ctx, method, "http://docker/v1.45"+path, body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return failure("docker_unavailable", "Protected Docker socket is unavailable", 502)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return failure("result_too_large", "Docker response exceeds one MiB; request a smaller page", 413)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == 404 {
			return failure("not_found", "Owned Docker target does not exist", 404)
		}
		return failure("docker_operation_failed", "Docker rejected the typed operation", 502)
	}
	if output != nil && len(data) > 0 {
		if text, ok := output.(*string); ok {
			*text = string(data)
		} else if err = json.Unmarshal(data, output); err != nil {
			return failure("invalid_response", "Docker returned invalid bounded JSON", 502)
		}
	}
	return nil
}
func (e *PrivilegedExecutor) dockerInspect(ctx context.Context, instance Instance, kind, name string) (map[string]any, error) {
	var result map[string]any
	path := "/" + kind + "/" + url.PathEscape(dockerOwnedName(instance, name))
	if kind == "containers" {
		path += "/json"
	}
	if err := e.dockerRequest(ctx, instance, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	labels, ok := result["Labels"].(map[string]any)
	if kind == "containers" {
		config, _ := result["Config"].(map[string]any)
		labels, ok = config["Labels"].(map[string]any)
	}
	if !ok || labels["io.powerlinux.managed"] != "true" || labels["io.powerlinux.instance"] != instance.ID {
		return nil, failure("ownership_invalid", "Docker target is not owned by this managed instance", 412)
	}
	return result, nil
}
func (e *PrivilegedExecutor) docker(ctx context.Context, raw json.RawMessage) (any, error) {
	var input DockerInput
	_ = json.Unmarshal(raw, &input)
	unlock, err := e.lockContext(ctx, input.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	instance, err := e.get(ctx, input.ID, false)
	if err != nil {
		return nil, err
	}
	if instance.PluginID != "docker" {
		return nil, failure("runtime_type_mismatch", "A managed Docker daemon is required", 412)
	}
	if instance.Status == "uninstalled" {
		return nil, failure("runtime_unavailable", "Docker instance is uninstalled", 412)
	}
	labels := dockerOwner(instance)
	name := dockerOwnedName(instance, input.Name)
	filter, _ := json.Marshal(map[string][]string{"label": {"io.powerlinux.managed=true", "io.powerlinux.instance=" + instance.ID}})
	filtered := "?filters=" + url.QueryEscape(string(filter))
	var output any
	switch input.Command {
	case "status":
		var info map[string]any
		if err = e.dockerRequest(ctx, instance, http.MethodGet, "/info", nil, &info); err != nil {
			return nil, err
		}
		return map[string]any{"id": instance.ID, "containers": info["Containers"], "images": info["Images"], "server_version": info["ServerVersion"], "driver": info["Driver"], "health": e.utilityProbe(ctx, instance)}, nil
	case "containers_list", "images_list", "networks_list", "volumes_list":
		kind := strings.TrimSuffix(input.Command, "_list")
		path := "/" + kind + filtered
		if kind == "containers" {
			path = "/containers/json" + filtered + "&all=true&limit=100"
		}
		if kind == "images" {
			path = "/images/json"
		}
		if err = e.dockerRequest(ctx, instance, http.MethodGet, path, nil, &output); err != nil {
			return nil, err
		}
		return map[string]any{"items": output}, nil
	case "image_pull":
		var stream string
		if err = e.dockerRequest(ctx, instance, http.MethodPost, "/images/create?fromImage="+url.QueryEscape(input.Image), nil, &stream); err != nil {
			return nil, err
		}
		for _, line := range strings.Split(stream, "\n") {
			var event map[string]any
			if json.Unmarshal([]byte(line), &event) == nil && event["error"] != nil {
				return nil, failure("image_pull_failed", "Registry failed to supply the digest-pinned image", 502)
			}
		}
		var image map[string]any
		if err = e.dockerRequest(ctx, instance, http.MethodGet, "/images/"+url.PathEscape(input.Image)+"/json", nil, &image); err != nil {
			return nil, err
		}
		return map[string]any{"image": input.Image, "image_id": image["Id"], "repo_digests": image["RepoDigests"]}, nil
	case "container_create":
		var image map[string]any
		if err = e.dockerRequest(ctx, instance, http.MethodGet, "/images/"+url.PathEscape(input.Image)+"/json", nil, &image); err != nil {
			return nil, failure("image_not_installed", "Pull and verify the immutable image before creating a container", 412)
		}
		env := []string{}
		for key, value := range input.Environment {
			env = append(env, key+"="+value)
		}
		exposed := map[string]any{}
		ports := map[string]any{}
		for _, port := range input.Ports {
			key := strconv.Itoa(port.Port) + "/" + port.Protocol
			exposed[key] = map[string]any{}
			ports[key] = []map[string]string{{"HostIp": "127.0.0.1", "HostPort": strconv.Itoa(port.HostPort)}}
		}
		host := map[string]any{"Memory": input.MemoryMB << 20, "NanoCpus": int64(input.CPUs * 1e9), "PidsLimit": 256, "Privileged": false, "CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"}, "NetworkMode": "bridge", "PortBindings": ports, "ReadonlyRootfs": true, "Tmpfs": map[string]string{"/tmp": "rw,noexec,nosuid,size=64m"}, "RestartPolicy": map[string]string{"Name": "unless-stopped"}}
		if input.Volume != "" {
			if _, err = e.dockerInspect(ctx, instance, "volumes", input.Volume); err != nil {
				return nil, err
			}
			host["Mounts"] = []map[string]any{{"Type": "volume", "Source": dockerOwnedName(instance, input.Volume), "Target": "/data", "ReadOnly": false}}
		}
		request := map[string]any{"Image": input.Image, "Labels": labels, "Env": env, "ExposedPorts": exposed, "HostConfig": host}
		if err = e.dockerRequest(ctx, instance, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), request, &output); err != nil {
			return nil, err
		}
		return output, nil
	case "container_get", "container_logs", "container_start", "container_stop", "container_restart", "container_delete":
		inspection, err := e.dockerInspect(ctx, instance, "containers", input.Name)
		if err != nil {
			return nil, err
		}
		if input.Command == "container_get" {
			config, _ := inspection["Config"].(map[string]any)
			delete(config, "Env")
			return inspection, nil
		}
		path := "/containers/" + url.PathEscape(name)
		if input.Command == "container_logs" {
			tail := input.Tail
			if tail == 0 {
				tail = 100
			}
			var logs string
			if err = e.dockerRequest(ctx, instance, http.MethodGet, path+"/logs?stdout=true&stderr=true&timestamps=true&tail="+strconv.Itoa(tail), nil, &logs); err != nil {
				return nil, err
			}
			return map[string]any{"logs": stripDockerFrames(logs)}, nil
		}
		method := http.MethodPost
		suffix := strings.TrimPrefix(input.Command, "container_")
		if suffix == "delete" {
			method = http.MethodDelete
			suffix = ""
		}
		if suffix != "" {
			path += "/" + suffix + "?t=10"
		}
		if err = e.dockerRequest(ctx, instance, method, path, nil, &output); err != nil {
			return nil, err
		}
		return map[string]any{"name": input.Name, "command": input.Command}, nil
	case "network_create":
		if err = e.dockerRequest(ctx, instance, http.MethodPost, "/networks/create", map[string]any{"Name": name, "Driver": "bridge", "Internal": true, "Labels": labels}, &output); err != nil {
			return nil, err
		}
		return output, nil
	case "volume_create":
		if err = e.dockerRequest(ctx, instance, http.MethodPost, "/volumes/create", map[string]any{"Name": name, "Driver": "local", "Labels": labels}, &output); err != nil {
			return nil, err
		}
		return output, nil
	case "network_delete", "volume_delete":
		kind := "networks"
		if input.Command == "volume_delete" {
			kind = "volumes"
		}
		if _, err = e.dockerInspect(ctx, instance, kind, input.Name); err != nil {
			return nil, err
		}
		if err = e.dockerRequest(ctx, instance, http.MethodDelete, "/"+kind+"/"+url.PathEscape(name), nil, nil); err != nil {
			return nil, err
		}
		return map[string]any{"name": input.Name, "command": input.Command}, nil
	}
	return nil, failure("invalid_input", "Unsupported Docker action", 400)
}
func stripDockerFrames(input string) string {
	raw := []byte(input)
	var output strings.Builder
	for len(raw) >= 8 && (raw[0] == 1 || raw[0] == 2) && raw[1] == 0 && raw[2] == 0 && raw[3] == 0 {
		size := int(raw[4])<<24 | int(raw[5])<<16 | int(raw[6])<<8 | int(raw[7])
		if size < 0 || size > len(raw)-8 {
			break
		}
		output.Write(raw[8 : 8+size])
		raw = raw[8+size:]
	}
	if output.Len() == 0 {
		return input
	}
	output.Write(raw)
	return output.String()
}
func (m *Manager) dockerBindings() []core.Binding {
	result := []core.Binding{}
	for _, command := range []string{"status", "containers_list", "images_list", "networks_list", "volumes_list", "image_pull", "container_create", "container_get", "container_logs", "container_start", "container_stop", "container_restart", "container_delete", "network_create", "network_delete", "volume_create", "volume_delete"} {
		read := command == "status" || strings.HasSuffix(command, "_list") || command == "container_get" || command == "container_logs"
		properties := map[string]any{"id": idSchema()}
		required := []string{"id"}
		if strings.HasPrefix(command, "container_") || strings.HasSuffix(command, "_create") || strings.HasSuffix(command, "_delete") {
			properties["name"] = map[string]any{"type": "string", "pattern": utilityName.String()}
			required = append(required, "name")
		}
		if command == "image_pull" || command == "container_create" {
			properties["image"] = map[string]any{"type": "string", "pattern": dockerImage.String()}
			required = append(required, "image")
		}
		if command == "container_create" {
			properties["memory_mb"] = map[string]any{"type": "integer", "minimum": 16, "maximum": 16384}
			properties["cpus"] = map[string]any{"type": "number", "exclusiveMinimum": 0, "maximum": 16}
			properties["environment"] = map[string]any{"type": "object", "maxProperties": 32, "additionalProperties": map[string]any{"type": "string", "maxLength": 4096}}
			properties["ports"] = map[string]any{"type": "array", "maxItems": 16, "items": obj(map[string]any{"port": map[string]any{"type": "integer", "minimum": 1, "maximum": 65535}, "host_port": map[string]any{"type": "integer", "minimum": 1024, "maximum": 65535}, "protocol": enumSchema("tcp", "udp")}, "port", "host_port", "protocol")}
			properties["volume"] = idSchema()
			required = append(required, "memory_mb", "cpus")
		}
		if command == "container_logs" {
			properties["tail"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}
		}
		verb := command
		scope := "containers:manage"
		if read {
			scope = "containers:read"
		}
		result = append(result, m.binding("docker_"+command, "Controlled Docker API over a private Agent-owned socket; no host mounts or privileged containers", scope, read, obj(properties, required...), idResource, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			var input DockerInput
			if err := parse(raw, &input); err != nil {
				return core.ActionResult{}, err
			}
			input.Command = verb
			forward, _ := json.Marshal(input)
			resources := []string{InstanceResource(input.ID)}
			if read || verb == "container_create" {
				var output any
				if err := m.agentJSON(ctx, "docker_action", forward, &output); err != nil {
					return core.ActionResult{}, err
				}
				return core.ActionResult{Data: output, Resources: resources}, nil
			}
			job, err := m.deps.Tasks.Submit(ctx, actor, "docker_action", forward, resources)
			if err != nil {
				return core.ActionResult{}, err
			}
			id, _ := job["id"].(string)
			return core.ActionResult{Data: job, TaskID: id, Resources: resources}, nil
		}))
		result[len(result)-1].Action.Task = !read && command != "container_create"
	}
	return result
}
