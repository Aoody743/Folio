package plugins

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

const (
	dockerExecBundleKind = "docker_exec_bundles"
	dockerExecBundleTTL  = 24 * time.Hour
	dockerExecMaxArgs    = 16
	dockerExecMaxArgSize = 4096
	dockerExecMaxArgv    = 16 << 10
	dockerExecMaxEnv     = 16
	dockerExecMaxEnvSize = 4096
	dockerExecMaxStdin   = 16 << 10
	dockerExecMaxOutput  = 64 << 10
	dockerExecMaxTimeout = 120
	dockerExecMaxFrame   = 16 << 20
)

var dockerExecEnvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
var dockerExecID = regexp.MustCompile(`^[a-f0-9]{64}$`)
var dockerExecBundleID = regexp.MustCompile(`^execbundle_[a-f0-9]{32}$`)

// DockerExecInput is the public one-shot command contract. Environment and
// stdin values are moved into an encrypted provider secret before a task is
// submitted; they never appear in the durable task input.
type DockerExecInput struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Argv             []string          `json:"argv"`
	Environment      map[string]string `json:"env,omitempty"`
	WorkingDir       string            `json:"working_dir,omitempty"`
	Stdin            string            `json:"stdin,omitempty"`
	TimeoutSeconds   int               `json:"timeout_seconds,omitempty"`
	OutputLimitBytes int               `json:"output_limit_bytes,omitempty"`
}

// dockerExecTaskInput contains only non-secret command parameters and an
// opaque bundle ID. The secret reference itself lives in provider state.
type dockerExecTaskInput struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Argv             []string `json:"argv"`
	WorkingDir       string   `json:"working_dir,omitempty"`
	TimeoutSeconds   int      `json:"timeout_seconds"`
	OutputLimitBytes int      `json:"output_limit_bytes"`
	BundleID         string   `json:"bundle_id,omitempty"`
}

type dockerExecAgentInput struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Argv             []string `json:"argv"`
	Environment      []string `json:"env,omitempty"`
	WorkingDir       string   `json:"working_dir,omitempty"`
	Stdin            string   `json:"stdin,omitempty"`
	TimeoutSeconds   int      `json:"timeout_seconds"`
	OutputLimitBytes int      `json:"output_limit_bytes"`
}

type dockerExecSecretBundle struct {
	Environment map[string]string `json:"env,omitempty"`
	Stdin       string            `json:"stdin,omitempty"`
}

type dockerExecBundleRecord struct {
	ActorID   string    `json:"actor_id"`
	Resource  string    `json:"resource"`
	SecretRef string    `json:"secret_ref"`
	ExpiresAt time.Time `json:"expires_at"`
}

type dockerExecOutput struct {
	Stdout         string `json:"stdout"`
	Stderr         string `json:"stderr"`
	ExitCode       int    `json:"exit_code"`
	StdoutBytes    int64  `json:"stdout_bytes"`
	StderrBytes    int64  `json:"stderr_bytes"`
	Truncated      bool   `json:"truncated"`
	ExecutionID    string `json:"execution_id,omitempty"`
	ExecutionState string `json:"execution_state"`
	TimedOut       bool   `json:"timed_out,omitempty"`
}

type dockerExecStatusInput struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ExecutionID string `json:"execution_id"`
}

type dockerExecStatusOutput struct {
	ExecutionID    string `json:"execution_id"`
	ExecutionState string `json:"execution_state"`
	Running        bool   `json:"running"`
	ExitCode       *int   `json:"exit_code,omitempty"`
}

type dockerExecRecord struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	ExecutionID string    `json:"execution_id"`
	StartedAt   time.Time `json:"started_at"`
	State       string    `json:"state"`
	ExitCode    *int      `json:"exit_code,omitempty"`
}

func dockerExecPrivateRecordID(execID string) string { return "d" + execID }

func (e *PrivilegedExecutor) saveDockerExecRecord(record dockerExecRecord) error {
	if !validInstanceID(record.ID) || !utilityName.MatchString(record.Name) || !dockerExecID.MatchString(record.ExecutionID) {
		return failure("invalid_input", "Invalid owned Docker exec record", 400)
	}
	return e.save("private", dockerExecPrivateRecordID(record.ExecutionID), record)
}

func (e *PrivilegedExecutor) loadDockerExecRecord(execID string, record *dockerExecRecord) error {
	if !dockerExecID.MatchString(execID) {
		return failure("not_found", "Owned Docker exec identity does not exist", 404)
	}
	if err := e.load("private", dockerExecPrivateRecordID(execID), record); err != nil {
		return err
	}
	if record.ExecutionID != execID || !validInstanceID(record.ID) || !utilityName.MatchString(record.Name) {
		return failure("ownership_invalid", "Protected Docker exec identity is invalid", 412)
	}
	return nil
}

// DockerContainerResource binds an action to the logical suffix of an owned
// container name, never to an arbitrary Engine container ID.
func DockerContainerResource(instanceID, name string) string {
	if !validInstanceID(instanceID) || !utilityName.MatchString(name) {
		return ""
	}
	return "docker:container:" + instanceID + ":" + name
}

func dockerExecInput(raw json.RawMessage) (DockerExecInput, error) {
	var in DockerExecInput
	if len(raw) > 64<<10 {
		return in, failure("invalid_input", "Docker exec input exceeds 64 KiB", 413)
	}
	if err := parse(raw, &in); err != nil {
		return in, err
	}
	if !validInstanceID(in.ID) || !utilityName.MatchString(in.Name) {
		return in, failure("invalid_input", "A managed Docker instance and logical container name are required", 400)
	}
	if err := validateDockerExecCommand(in.Argv, in.WorkingDir, in.TimeoutSeconds, in.OutputLimitBytes); err != nil {
		return in, err
	}
	if len(in.Environment) > dockerExecMaxEnv {
		return in, failure("invalid_input", "Docker exec accepts at most 16 environment entries", 400)
	}
	total := 0
	for key, value := range in.Environment {
		if !dockerExecEnvKey.MatchString(key) || len(value) > dockerExecMaxEnvSize || strings.ContainsRune(value, 0) {
			return in, failure("invalid_input", "Invalid Docker exec environment entry", 400)
		}
		total += len(key) + 1 + len(value)
	}
	if total > dockerExecMaxEnvSize {
		return in, failure("invalid_input", "Docker exec environment exceeds four KiB", 400)
	}
	if len(in.Stdin) > dockerExecMaxStdin || !utf8.ValidString(in.Stdin) || strings.ContainsRune(in.Stdin, 0) {
		return in, failure("invalid_input", "Docker exec stdin must be valid UTF-8 of at most 16 KiB", 400)
	}
	return in, nil
}

func validateDockerExecCommand(argv []string, cwd string, timeoutSeconds, outputLimit int) error {
	if len(argv) == 0 || len(argv) > dockerExecMaxArgs {
		return failure("invalid_input", "Docker exec requires 1 to 16 argv entries", 400)
	}
	total := 0
	for _, arg := range argv {
		if len(arg) == 0 || len(arg) > dockerExecMaxArgSize || strings.ContainsRune(arg, 0) {
			return failure("invalid_input", "Docker exec argv entries must be non-empty and at most 4096 bytes", 400)
		}
		total += len(arg) + 1
	}
	if total > dockerExecMaxArgv {
		return failure("invalid_input", "Docker exec argv exceeds 16 KiB", 400)
	}
	if cwd != "" && (len(cwd) > 256 || !path.IsAbs(cwd) || path.Clean(cwd) != cwd || strings.ContainsRune(cwd, 0)) {
		return failure("invalid_input", "Docker exec working directory must be an absolute normalized path of at most 256 bytes", 400)
	}
	if timeoutSeconds < 0 || timeoutSeconds > dockerExecMaxTimeout {
		return failure("invalid_input", "Docker exec timeout must be from 1 to 120 seconds", 400)
	}
	if outputLimit < 0 || outputLimit > dockerExecMaxOutput {
		return failure("invalid_input", "Docker exec output limit must be at most 64 KiB", 400)
	}
	return nil
}

func validateDockerExecAgent(raw json.RawMessage) error {
	var in dockerExecAgentInput
	if err := parse(raw, &in); err != nil {
		return err
	}
	if !validInstanceID(in.ID) || !utilityName.MatchString(in.Name) || in.TimeoutSeconds < 1 || in.TimeoutSeconds > dockerExecMaxTimeout || in.OutputLimitBytes < 1 || in.OutputLimitBytes > dockerExecMaxOutput {
		return failure("invalid_input", "Invalid bounded Docker exec request", 400)
	}
	if err := validateDockerExecCommand(in.Argv, in.WorkingDir, in.TimeoutSeconds, in.OutputLimitBytes); err != nil {
		return err
	}
	if len(in.Environment) > dockerExecMaxEnv {
		return failure("invalid_input", "Docker exec accepts at most 16 environment entries", 400)
	}
	total := 0
	seen := map[string]bool{}
	for _, entry := range in.Environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !dockerExecEnvKey.MatchString(key) || seen[key] || len(value) > dockerExecMaxEnvSize || strings.ContainsRune(value, 0) {
			return failure("invalid_input", "Invalid Docker exec environment entry", 400)
		}
		seen[key] = true
		total += len(entry) + 1
	}
	if total > dockerExecMaxEnvSize || len(in.Stdin) > dockerExecMaxStdin || !utf8.ValidString(in.Stdin) || strings.ContainsRune(in.Stdin, 0) {
		return failure("invalid_input", "Docker exec environment or stdin exceeds its bound", 400)
	}
	return nil
}

func validateDockerExecTask(raw json.RawMessage) (dockerExecTaskInput, error) {
	var in dockerExecTaskInput
	if len(raw) > 32<<10 {
		return in, failure("invalid_input", "Docker exec task input exceeds 32 KiB", 413)
	}
	if err := parse(raw, &in); err != nil {
		return in, err
	}
	if !validInstanceID(in.ID) || !utilityName.MatchString(in.Name) || (in.BundleID != "" && !dockerExecBundleID.MatchString(in.BundleID)) || in.TimeoutSeconds < 1 || in.TimeoutSeconds > dockerExecMaxTimeout || in.OutputLimitBytes < 1 || in.OutputLimitBytes > dockerExecMaxOutput {
		return in, failure("invalid_input", "Invalid bounded Docker exec task", 400)
	}
	if err := validateDockerExecCommand(in.Argv, in.WorkingDir, in.TimeoutSeconds, in.OutputLimitBytes); err != nil {
		return in, err
	}
	return in, nil
}

func dockerExecTaskSchema() map[string]any {
	return obj(map[string]any{
		"id":                 idSchema(),
		"name":               map[string]any{"type": "string", "pattern": utilityName.String()},
		"argv":               map[string]any{"type": "array", "minItems": 1, "maxItems": dockerExecMaxArgs, "items": map[string]any{"type": "string", "minLength": 1, "maxLength": dockerExecMaxArgSize}},
		"env":                map[string]any{"type": "object", "maxProperties": dockerExecMaxEnv, "propertyNames": map[string]any{"pattern": dockerExecEnvKey.String()}, "additionalProperties": map[string]any{"type": "string", "maxLength": dockerExecMaxEnvSize}},
		"working_dir":        map[string]any{"type": "string", "pattern": "^/", "maxLength": 256},
		"stdin":              map[string]any{"type": "string", "maxLength": dockerExecMaxStdin},
		"timeout_seconds":    map[string]any{"type": "integer", "minimum": 1, "maximum": dockerExecMaxTimeout, "default": 30},
		"output_limit_bytes": map[string]any{"type": "integer", "minimum": 1, "maximum": dockerExecMaxOutput, "default": 16 << 10},
	}, "id", "name", "argv")
}

func DockerExecJobOperations() []string { return []string{"docker_exec_job"} }

func DockerExecAgentOperations() []string { return []string{"docker_exec", "docker_exec_status"} }

func (m *Manager) dockerExecBindings() []core.Binding {
	binding := m.binding("docker_container_exec", "Run one bounded argv command directly in an owned managed Docker container", "containers:manage", false, dockerExecTaskSchema(), dockerExecResources, func(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		in, err := dockerExecInput(raw)
		if err != nil {
			return core.ActionResult{}, err
		}
		resources, err := dockerExecResources(ctx, a, raw)
		if err != nil {
			return core.ActionResult{}, err
		}
		m.pruneDockerExecBundles(ctx, a)
		timeout := in.TimeoutSeconds
		if timeout == 0 {
			timeout = 30
		}
		outputLimit := in.OutputLimitBytes
		if outputLimit == 0 {
			outputLimit = 16 << 10
		}
		bundleID := ""
		if len(in.Environment) > 0 || in.Stdin != "" {
			bundleID, err = m.saveDockerExecBundle(ctx, a, resources[1], dockerExecSecretBundle{Environment: in.Environment, Stdin: in.Stdin})
			if err != nil {
				return core.ActionResult{}, err
			}
		}
		task := dockerExecTaskInput{ID: in.ID, Name: in.Name, Argv: in.Argv, WorkingDir: in.WorkingDir, TimeoutSeconds: timeout, OutputLimitBytes: outputLimit, BundleID: bundleID}
		encoded, err := json.Marshal(task)
		if err != nil {
			m.deleteDockerExecBundle(ctx, a, resources[1], bundleID)
			return core.ActionResult{}, failure("invalid_input", "Docker exec task could not be encoded", 400)
		}
		job, err := m.deps.Tasks.Submit(ctx, a, "docker_exec_job", encoded, resources)
		clear(encoded)
		if err != nil {
			m.deleteDockerExecBundle(ctx, a, resources[1], bundleID)
			return core.ActionResult{}, err
		}
		id, _ := job["id"].(string)
		return core.ActionResult{Data: job, TaskID: id, Resources: resources}, nil
	})
	binding.Action.Task = true
	status := m.binding("docker_container_exec_status", "Inspect the live status of an exec previously created in an owned managed Docker container", "containers:read", true, obj(map[string]any{
		"id":           idSchema(),
		"name":         map[string]any{"type": "string", "pattern": utilityName.String()},
		"execution_id": map[string]any{"type": "string", "pattern": dockerExecID.String()},
	}, "id", "name", "execution_id"), dockerExecStatusResources, func(ctx context.Context, _ core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		var input dockerExecStatusInput
		if err := parse(raw, &input); err != nil {
			return core.ActionResult{}, err
		}
		encoded, _ := json.Marshal(input)
		var output dockerExecStatusOutput
		if err := m.agentJSON(ctx, "docker_exec_status", encoded, &output); err != nil {
			return core.ActionResult{}, err
		}
		resources, err := dockerExecStatusResources(ctx, core.Actor{}, raw)
		if err != nil {
			return core.ActionResult{}, err
		}
		return core.ActionResult{Data: output, Resources: resources}, nil
	})
	return []core.Binding{binding, status}
}

func dockerExecResources(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	in, err := dockerExecInput(raw)
	if err != nil {
		return nil, err
	}
	return []string{InstanceResource(in.ID), DockerContainerResource(in.ID, in.Name)}, nil
}

func dockerExecStatusResources(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var in dockerExecStatusInput
	if err := parse(raw, &in); err != nil {
		return nil, err
	}
	if !validInstanceID(in.ID) || !utilityName.MatchString(in.Name) || !dockerExecID.MatchString(in.ExecutionID) {
		return nil, failure("invalid_input", "Invalid owned Docker exec identity", 400)
	}
	return []string{InstanceResource(in.ID), DockerContainerResource(in.ID, in.Name)}, nil
}

func newDockerExecBundleID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "execbundle_" + hex.EncodeToString(raw[:]), nil
}

func (m *Manager) saveDockerExecBundle(ctx context.Context, actor core.Actor, resource string, bundle dockerExecSecretBundle) (string, error) {
	encoded, err := json.Marshal(bundle)
	if err != nil {
		return "", failure("invalid_input", "Docker exec inputs could not be encoded", 400)
	}
	defer clear(encoded)
	ref, err := m.deps.Secrets.Put(ctx, actor, resource, "one-shot Docker exec inputs", encoded)
	if err != nil {
		return "", err
	}
	id, err := newDockerExecBundleID()
	if err != nil {
		_ = m.deps.Secrets.Delete(ctx, actor, resource, ref)
		return "", failure("internal", "Docker exec inputs could not be staged", 500)
	}
	record, _ := json.Marshal(dockerExecBundleRecord{ActorID: actor.ID, Resource: resource, SecretRef: ref, ExpiresAt: time.Now().UTC().Add(dockerExecBundleTTL)})
	if err = m.deps.State.Save(ctx, dockerExecBundleKind, id, record); err != nil {
		_ = m.deps.Secrets.Delete(ctx, actor, resource, ref)
		return "", err
	}
	return id, nil
}

func (m *Manager) deleteDockerExecBundle(ctx context.Context, actor core.Actor, resource, id string) {
	if id == "" || !dockerExecBundleID.MatchString(id) {
		return
	}
	raw, err := m.deps.State.Get(ctx, dockerExecBundleKind, id)
	if err == nil {
		var record dockerExecBundleRecord
		if json.Unmarshal(raw, &record) == nil && record.ActorID == actor.ID && record.Resource == resource && record.SecretRef != "" {
			_ = m.deps.Secrets.Delete(ctx, actor, resource, record.SecretRef)
		}
	}
	_ = m.deps.State.Delete(ctx, dockerExecBundleKind, id)
}

func (m *Manager) pruneDockerExecBundles(ctx context.Context, actor core.Actor) {
	rows, err := m.deps.State.List(ctx, dockerExecBundleKind)
	if err != nil {
		return
	}
	now := time.Now()
	for id, raw := range rows {
		if !dockerExecBundleID.MatchString(id) {
			continue
		}
		var record dockerExecBundleRecord
		if json.Unmarshal(raw, &record) != nil || record.ActorID != actor.ID || record.SecretRef == "" || record.ExpiresAt.After(now) || !m.deps.Authorize(actor, "containers:manage", record.Resource) {
			continue
		}
		if m.deps.Secrets.Delete(ctx, actor, record.Resource, record.SecretRef) == nil {
			_ = m.deps.State.Delete(ctx, dockerExecBundleKind, id)
		}
	}
}

// DockerExecExecutor resolves the opaque bundle only while the original actor
// still has both grants. It is a jobs executor; its transient Agent operation
// is separately named docker_exec and accepts values only over authenticated
// Unix IPC.
type DockerExecExecutor struct {
	manager *Manager
	client  *agent.UnixClient
}

func NewDockerExecExecutor(manager *Manager, client *agent.UnixClient) *DockerExecExecutor {
	return &DockerExecExecutor{manager: manager, client: client}
}

func (e *DockerExecExecutor) Validate(request agent.Request) error {
	if request.Operation != "docker_exec_job" {
		return agent.ErrUnauthorized
	}
	if _, err := validateDockerExecTask(request.Input); err != nil {
		return err
	}
	if e == nil || e.manager == nil || e.client == nil || e.manager.deps.ResolveActor == nil || e.manager.deps.Secrets == nil {
		return failure("agent_unavailable", "Docker exec requires the protected Unix Agent", 503)
	}
	return nil
}

func (e *DockerExecExecutor) Run(ctx context.Context, request agent.Request) (result agent.Result, returned error) {
	result = agent.Result{Operation: request.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	defer func() { result.FinishedAt = time.Now().UTC() }()
	if err := e.Validate(request); err != nil {
		return result, err
	}
	in, _ := validateDockerExecTask(request.Input)
	m := e.manager
	actor, err := m.deps.ResolveActor(ctx, core.Actor{ID: request.ActorID})
	if err != nil {
		return result, dockerExecSafeError(err)
	}
	resources := []string{InstanceResource(in.ID), DockerContainerResource(in.ID, in.Name)}
	for _, resource := range resources {
		if !m.deps.Authorize(actor, "containers:manage", resource) {
			return result, failure("forbidden", "Original Docker exec grants are no longer valid", 403)
		}
	}
	bundle := dockerExecSecretBundle{}
	if in.BundleID != "" {
		raw, getErr := m.deps.State.Get(ctx, dockerExecBundleKind, in.BundleID)
		if getErr != nil {
			return result, failure("secret_unavailable", "Staged Docker exec inputs are unavailable", 412)
		}
		var record dockerExecBundleRecord
		if json.Unmarshal(raw, &record) != nil || record.ActorID != actor.ID || record.Resource != resources[1] || record.SecretRef == "" || !record.ExpiresAt.After(time.Now()) {
			return result, failure("secret_unavailable", "Staged Docker exec inputs are unavailable", 412)
		}
		secret, getErr := m.deps.Secrets.Get(ctx, actor, resources[1], record.SecretRef)
		if getErr != nil {
			return result, failure("secret_unavailable", "Staged Docker exec inputs are unavailable", 412)
		}
		if json.Unmarshal(secret, &bundle) != nil {
			clear(secret)
			return result, failure("secret_unavailable", "Staged Docker exec inputs are invalid", 412)
		}
		clear(secret)
	}
	secretValues := append([]string{bundle.Stdin}, valuesOf(bundle.Environment)...)
	defer func() {
		for i := range secretValues {
			secretValues[i] = ""
		}
	}()
	defer func() {
		for key := range bundle.Environment {
			bundle.Environment[key] = ""
		}
		bundle.Stdin = ""
	}()
	env := make([]string, 0, len(bundle.Environment))
	for key, value := range bundle.Environment {
		env = append(env, key+"="+value)
	}
	sort.Strings(env)
	forward := dockerExecAgentInput{ID: in.ID, Name: in.Name, Argv: in.Argv, Environment: env, WorkingDir: in.WorkingDir, Stdin: bundle.Stdin, TimeoutSeconds: in.TimeoutSeconds, OutputLimitBytes: in.OutputLimitBytes}
	if bundle.Environment != nil {
		for key := range bundle.Environment {
			bundle.Environment[key] = ""
		}
	}
	encoded, err := json.Marshal(forward)
	if err != nil {
		return result, failure("invalid_input", "Docker exec request could not be encoded", 400)
	}
	defer clear(encoded)
	for i := range forward.Environment {
		forward.Environment[i] = ""
	}
	forward.Stdin = ""
	// Docker Engine has no endpoint that kills an individual exec process. Keep
	// observing the bounded operation after a task cancel request so the durable
	// task reports the actual Engine state instead of claiming cancellation.
	executionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(in.TimeoutSeconds+5)*time.Second)
	defer cancel()
	reply, err := e.client.Run(executionCtx, agent.Request{ActorID: actor.ID, JobID: request.JobID, Operation: "docker_exec", Input: encoded})
	if err != nil {
		return result, dockerExecSafeError(err)
	}
	var output dockerExecOutput
	maxEncodedText := 3*in.OutputLimitBytes + 16
	if len(reply.Stdout) > 512<<10 || json.Unmarshal([]byte(reply.Stdout), &output) != nil || len(output.Stdout)+len(output.Stderr) > maxEncodedText || output.StdoutBytes < 0 || output.StderrBytes < 0 {
		return result, failure("agent_response", "Docker exec Agent returned an invalid bounded result", 502)
	}
	if output.ExecutionState != "completed" && output.ExecutionState != "running" && output.ExecutionState != "unknown" {
		return result, failure("agent_response", "Docker exec Agent returned an invalid execution state", 502)
	}
	if !dockerExecID.MatchString(output.ExecutionID) {
		return result, failure("agent_response", "Docker exec Agent returned an invalid execution identity", 502)
	}
	if output.ExecutionState == "completed" && (output.ExitCode < 0 || output.ExitCode > 255) || output.ExecutionState != "completed" && output.ExitCode != -1 {
		return result, failure("agent_response", "Docker exec Agent returned an invalid exit status", 502)
	}
	output.Stdout = redactDockerExecOutput(output.Stdout, secretValues)
	output.Stderr = redactDockerExecOutput(output.Stderr, secretValues)
	output.Stdout = strings.ToValidUTF8(output.Stdout, "\uFFFD")
	output.Stderr = strings.ToValidUTF8(output.Stderr, "\uFFFD")
	var redactedOutputTruncated bool
	output.Stdout, output.Stderr, redactedOutputTruncated = boundDockerExecText(output.Stdout, output.Stderr, in.OutputLimitBytes)
	output.Truncated = output.Truncated || redactedOutputTruncated
	result.Stdout, result.Stderr = output.Stdout, output.Stderr
	result.StdoutBytes, result.StderrBytes = output.StdoutBytes, output.StderrBytes
	result.ExitCode, result.Truncated = output.ExitCode, output.Truncated
	result.TimedOut = output.TimedOut
	result.Data, _ = json.Marshal(map[string]any{"execution_id": output.ExecutionID, "execution_state": output.ExecutionState, "timed_out": output.TimedOut, "cancel_requested": ctx.Err() != nil})
	if request.Output != nil {
		for _, event := range []agent.Output{{Stream: "stdout", Text: output.Stdout}, {Stream: "stderr", Text: output.Stderr}} {
			if event.Text != "" {
				select {
				case request.Output <- event:
				default:
					result.DroppedEvents++
				}
			}
		}
	}
	if output.ExecutionState == "running" || output.ExecutionState == "unknown" {
		return result, failure("execution_unresolved", "Docker exec remains active or its final Engine state is unknown; inspect its execution ID", 409)
	}
	if output.TimedOut {
		return result, failure("timeout", "Docker exec exceeded its time limit; inspect its execution ID for current state", 504)
	}
	if output.ExitCode != 0 {
		return result, failure("command_failed", "Container command exited with a non-zero status", 409)
	}
	return result, nil
}

func valuesOf(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func redactDockerExecOutput(text string, values []string) string {
	values = append([]string(nil), values...)
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, value := range values {
		if value != "" {
			text = strings.ReplaceAll(text, value, "[redacted]")
		}
	}
	return text
}

func boundDockerExecText(stdout, stderr string, limit int) (string, string, bool) {
	if limit < 0 {
		limit = 0
	}
	remaining := limit
	truncate := func(value string) (string, bool) {
		if len(value) <= remaining {
			remaining -= len(value)
			return value, false
		}
		value = value[:remaining]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
		remaining = 0
		return value, true
	}
	stdout, stdoutCut := truncate(stdout)
	stderr, stderrCut := truncate(stderr)
	return stdout, stderr, stdoutCut || stderrCut
}

func dockerExecSafeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return failure("timeout", "Docker exec exceeded its time limit", 504)
	}
	if errors.Is(err, context.Canceled) {
		return failure("canceled", "Docker exec was canceled", 409)
	}
	var remote *agent.RemoteError
	if errors.As(err, &remote) {
		switch remote.Code {
		case "timeout", "canceled", "not_found", "ownership_invalid", "runtime_unavailable", "runtime_type_mismatch", "container_not_running", "docker_unavailable", "docker_operation_failed", "private_state_unavailable", "invalid_response", "forbidden", "invalid_input":
			return failure(remote.Code, remote.Message, remote.Status)
		}
	}
	return failure("docker_exec_failed", "Protected Docker exec could not complete", 502)
}

func (e *PrivilegedExecutor) validateDockerExec(raw json.RawMessage) error {
	return validateDockerExecAgent(raw)
}

func (e *PrivilegedExecutor) validateDockerExecStatus(raw json.RawMessage) error {
	var in dockerExecStatusInput
	if err := parse(raw, &in); err != nil {
		return err
	}
	if !validInstanceID(in.ID) || !utilityName.MatchString(in.Name) || !dockerExecID.MatchString(in.ExecutionID) {
		return failure("invalid_input", "Invalid owned Docker exec identity", 400)
	}
	return nil
}

func (e *PrivilegedExecutor) dockerExec(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := e.validateDockerExec(raw); err != nil {
		return nil, err
	}
	var in dockerExecAgentInput
	_ = json.Unmarshal(raw, &in)
	deadlineCtx, cancel := context.WithTimeout(ctx, time.Duration(in.TimeoutSeconds)*time.Second)
	defer cancel()
	unlock, err := e.lockContext(deadlineCtx, in.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err = e.ensureInstallFinalized(deadlineCtx, in.ID); err != nil {
		return nil, err
	}
	instance, err := e.get(deadlineCtx, in.ID, false)
	if err != nil {
		return nil, err
	}
	if instance.PluginID != "docker" {
		return nil, failure("runtime_type_mismatch", "A managed Docker daemon is required", 412)
	}
	if instance.Status == "uninstalled" {
		return nil, failure("runtime_unavailable", "Docker instance is uninstalled", 412)
	}
	inspection, err := e.dockerInspect(deadlineCtx, instance, "containers", in.Name)
	if err != nil {
		return nil, err
	}
	state, _ := inspection["State"].(map[string]any)
	running, _ := state["Running"].(bool)
	if !running {
		return nil, failure("container_not_running", "Owned Docker container must be running", 412)
	}
	container := dockerOwnedName(instance, in.Name)
	var created struct {
		ID string `json:"Id"`
	}
	createRequest := map[string]any{"AttachStdin": true, "AttachStdout": true, "AttachStderr": true, "Tty": false, "Cmd": in.Argv, "Env": in.Environment, "WorkingDir": in.WorkingDir}
	if err = e.dockerRequest(deadlineCtx, instance, http.MethodPost, "/containers/"+url.PathEscape(container)+"/exec", createRequest, &created); err != nil {
		return nil, err
	}
	if !dockerExecID.MatchString(created.ID) {
		return nil, failure("invalid_response", "Docker returned an invalid exec identity", 502)
	}
	record := dockerExecRecord{ID: instance.ID, Name: in.Name, ExecutionID: created.ID, StartedAt: time.Now().UTC(), State: "created"}
	if err = e.saveDockerExecRecord(record); err != nil {
		return nil, failure("private_state_unavailable", "Docker exec recovery identity could not be saved", 500)
	}
	stdout, stderr, stdoutBytes, stderrBytes, truncated, err := startDockerExec(deadlineCtx, instance, created.ID, in.Stdin, in.OutputLimitBytes)
	if err != nil {
		truncated = true
		status, inspectErr := e.inspectDockerExecAfterCancel(ctx, instance, in.Name, created.ID)
		if inspectErr != nil {
			record.State = "unknown"
			_ = e.saveDockerExecRecord(record)
			return dockerExecOutput{ExitCode: -1, ExecutionID: created.ID, ExecutionState: "unknown", TimedOut: errors.Is(err, context.DeadlineExceeded) || errors.Is(deadlineCtx.Err(), context.DeadlineExceeded)}, nil
		}
		status.ExecutionID = created.ID
		if status.Running {
			record.State = "running"
			_ = e.saveDockerExecRecord(record)
			return dockerExecOutput{Stdout: string(stdout), Stderr: string(stderr), ExitCode: -1, StdoutBytes: stdoutBytes, StderrBytes: stderrBytes, Truncated: truncated, ExecutionID: created.ID, ExecutionState: "running", TimedOut: errors.Is(err, context.DeadlineExceeded) || errors.Is(deadlineCtx.Err(), context.DeadlineExceeded)}, nil
		}
		if status.ExitCode != nil {
			record.State, record.ExitCode = "completed", status.ExitCode
			_ = e.saveDockerExecRecord(record)
			return dockerExecOutput{Stdout: string(stdout), Stderr: string(stderr), ExitCode: *status.ExitCode, StdoutBytes: stdoutBytes, StderrBytes: stderrBytes, Truncated: truncated, ExecutionID: created.ID, ExecutionState: "completed", TimedOut: errors.Is(err, context.DeadlineExceeded) || errors.Is(deadlineCtx.Err(), context.DeadlineExceeded)}, nil
		}
		record.State = "unknown"
		_ = e.saveDockerExecRecord(record)
		return dockerExecOutput{ExitCode: -1, ExecutionID: created.ID, ExecutionState: "unknown", TimedOut: errors.Is(err, context.DeadlineExceeded) || errors.Is(deadlineCtx.Err(), context.DeadlineExceeded)}, nil
	}
	timedOut := errors.Is(deadlineCtx.Err(), context.DeadlineExceeded)
	inspected, err := e.inspectDockerExec(deadlineCtx, instance, created.ID)
	if err != nil {
		record.State = "unknown"
		_ = e.saveDockerExecRecord(record)
		return dockerExecOutput{Stdout: string(stdout), Stderr: string(stderr), ExitCode: -1, StdoutBytes: stdoutBytes, StderrBytes: stderrBytes, Truncated: truncated, ExecutionID: created.ID, ExecutionState: "unknown", TimedOut: timedOut}, nil
	}
	if inspected.Running {
		record.State = "running"
		_ = e.saveDockerExecRecord(record)
		return dockerExecOutput{Stdout: string(stdout), Stderr: string(stderr), ExitCode: -1, StdoutBytes: stdoutBytes, StderrBytes: stderrBytes, Truncated: truncated, ExecutionID: created.ID, ExecutionState: "running", TimedOut: timedOut}, nil
	}
	if inspected.ExecutionState != "completed" || inspected.ExitCode == nil {
		record.State = "unknown"
		_ = e.saveDockerExecRecord(record)
		return dockerExecOutput{Stdout: string(stdout), Stderr: string(stderr), ExitCode: -1, StdoutBytes: stdoutBytes, StderrBytes: stderrBytes, Truncated: truncated, ExecutionID: created.ID, ExecutionState: "unknown", TimedOut: timedOut}, nil
	}
	record.State, record.ExitCode = "completed", inspected.ExitCode
	_ = e.saveDockerExecRecord(record)
	return dockerExecOutput{Stdout: string(stdout), Stderr: string(stderr), ExitCode: *inspected.ExitCode, StdoutBytes: stdoutBytes, StderrBytes: stderrBytes, Truncated: truncated, ExecutionID: created.ID, ExecutionState: "completed", TimedOut: timedOut}, nil
}

func (e *PrivilegedExecutor) inspectDockerExec(ctx context.Context, instance Instance, execID string) (dockerExecStatusOutput, error) {
	var inspected struct {
		Running  bool `json:"Running"`
		ExitCode *int `json:"ExitCode"`
	}
	if err := e.dockerRequest(ctx, instance, http.MethodGet, "/exec/"+url.PathEscape(execID)+"/json", nil, &inspected); err != nil {
		return dockerExecStatusOutput{}, err
	}
	status := dockerExecStatusOutput{ExecutionID: execID, ExecutionState: "unknown", Running: inspected.Running}
	if inspected.Running {
		status.ExecutionState = "running"
	} else if inspected.ExitCode != nil && *inspected.ExitCode >= 0 && *inspected.ExitCode <= 255 {
		status.ExecutionState = "completed"
		status.ExitCode = inspected.ExitCode
	}
	return status, nil
}

func (e *PrivilegedExecutor) inspectDockerExecAfterCancel(ctx context.Context, instance Instance, name, execID string) (dockerExecStatusOutput, error) {
	inspectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if _, err := e.dockerInspect(inspectCtx, instance, "containers", name); err != nil {
		return dockerExecStatusOutput{}, err
	}
	return e.inspectDockerExec(inspectCtx, instance, execID)
}

func (e *PrivilegedExecutor) dockerExecStatus(ctx context.Context, raw json.RawMessage) (any, error) {
	var in dockerExecStatusInput
	if err := e.validateDockerExecStatus(raw); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(raw, &in)
	instance, err := e.get(ctx, in.ID, false)
	if err != nil {
		return nil, err
	}
	if instance.PluginID != "docker" || instance.Status == "uninstalled" {
		return nil, failure("runtime_unavailable", "Managed Docker instance is unavailable", 412)
	}
	if _, err = e.dockerInspect(ctx, instance, "containers", in.Name); err != nil {
		return nil, err
	}
	unlock, err := e.lockContext(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var record dockerExecRecord
	if err = e.loadDockerExecRecord(in.ExecutionID, &record); err != nil || record.ID != in.ID || record.Name != in.Name || record.ExecutionID != in.ExecutionID {
		return nil, failure("not_found", "Owned Docker exec identity does not exist", 404)
	}
	status, err := e.inspectDockerExec(ctx, instance, in.ExecutionID)
	if err != nil {
		return nil, err
	}
	if status.ExecutionState == "completed" {
		record.State, record.ExitCode = "completed", status.ExitCode
		_ = e.saveDockerExecRecord(record)
	} else if status.Running {
		record.State = "running"
		_ = e.saveDockerExecRecord(record)
	}
	return status, nil
}

func startDockerExec(ctx context.Context, instance Instance, execID, stdin string, outputLimit int) (stdout, stderr []byte, stdoutBytes, stderrBytes int64, truncated bool, returned error) {
	socket := filepath.Join(instance.Paths.RunDir, "docker.sock")
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		if contextErr := dockerExecContextError(ctx, err); contextErr != nil {
			return nil, nil, 0, 0, false, contextErr
		}
		return nil, nil, 0, 0, false, failure("docker_unavailable", "Protected Docker socket is unavailable", 502)
	}
	defer conn.Close()
	stopClose := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopClose:
		}
	}()
	defer close(stopClose)
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	body, err := json.Marshal(map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return nil, nil, 0, 0, false, err
	}
	request, err := http.NewRequest(http.MethodPost, "http://docker/v1.45/exec/"+url.PathEscape(execID)+"/start", bytes.NewReader(body))
	if err != nil {
		return nil, nil, 0, 0, false, err
	}
	request.Host = "docker"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "tcp")
	if err = request.Write(conn); err != nil {
		if contextErr := dockerExecContextError(ctx, err); contextErr != nil {
			return nil, nil, 0, 0, false, contextErr
		}
		return nil, nil, 0, 0, false, err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		if contextErr := dockerExecContextError(ctx, err); contextErr != nil {
			return nil, nil, 0, 0, false, contextErr
		}
		return nil, nil, 0, 0, false, err
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		_ = response.Body.Close()
		return nil, nil, 0, 0, false, failure("docker_exec_failed", "Docker rejected the owned exec start request", 502)
	}
	if _, err = io.WriteString(conn, stdin); err != nil {
		if contextErr := dockerExecContextError(ctx, err); contextErr != nil {
			return nil, nil, 0, 0, false, contextErr
		}
		return nil, nil, 0, 0, false, err
	}
	if unix, ok := conn.(*net.UnixConn); ok {
		if err = unix.CloseWrite(); err != nil {
			if contextErr := dockerExecContextError(ctx, err); contextErr != nil {
				return nil, nil, 0, 0, false, contextErr
			}
			return nil, nil, 0, 0, false, err
		}
	}
	stdout, stderr, stdoutBytes, stderrBytes, truncated, err = decodeDockerExecFrames(reader, outputLimit)
	if contextErr := dockerExecContextError(ctx, err); contextErr != nil {
		return stdout, stderr, stdoutBytes, stderrBytes, truncated, contextErr
	}
	return stdout, stderr, stdoutBytes, stderrBytes, truncated, err
}

// A net.Conn deadline enforces the operation context deadline. The socket can
// report its timeout before the context timer publishes ctx.Err(), so consult
// the recorded deadline as well.
func dockerExecContextError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
	}
	return nil
}

func decodeDockerExecFrames(reader io.Reader, outputLimit int) (stdout, stderr []byte, stdoutBytes, stderrBytes int64, truncated bool, returned error) {
	remaining := outputLimit
	buffer := make([]byte, 32<<10)
	for {
		var header [8]byte
		n, err := io.ReadFull(reader, header[:])
		if err == io.EOF && n == 0 {
			return stdout, stderr, stdoutBytes, stderrBytes, truncated, nil
		}
		if err != nil {
			return stdout, stderr, stdoutBytes, stderrBytes, truncated, failure("invalid_response", "Docker exec stream ended in a partial frame", 502)
		}
		if header[1] != 0 || header[2] != 0 || header[3] != 0 {
			return stdout, stderr, stdoutBytes, stderrBytes, truncated, failure("invalid_response", "Docker exec stream returned an invalid frame", 502)
		}
		stream := header[0]
		frameBytes := int64(binary.BigEndian.Uint32(header[4:]))
		if frameBytes < 0 || frameBytes > dockerExecMaxFrame || (stream != 1 && stream != 2 && stream != 3) {
			return stdout, stderr, stdoutBytes, stderrBytes, truncated, failure("invalid_response", "Docker exec stream exceeded its frame bound", 502)
		}
		for frameBytes > 0 {
			want := int64(len(buffer))
			if frameBytes < want {
				want = frameBytes
			}
			count, readErr := io.ReadFull(reader, buffer[:int(want)])
			if readErr != nil {
				return stdout, stderr, stdoutBytes, stderrBytes, truncated, failure("invalid_response", "Docker exec stream ended in a partial frame", 502)
			}
			if stream == 3 {
				return stdout, stderr, stdoutBytes, stderrBytes, truncated, failure("docker_exec_failed", "Docker reported an exec process error", 502)
			}
			keep := count
			if keep > remaining {
				keep = remaining
				truncated = true
			}
			if stream == 1 {
				stdoutBytes += int64(count)
				stdout = append(stdout, buffer[:keep]...)
			} else {
				stderrBytes += int64(count)
				stderr = append(stderr, buffer[:keep]...)
			}
			remaining -= keep
			frameBytes -= int64(count)
		}
	}
}
