//go:build linux

package plugins

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/jobs"
)

func TestDockerExecValidatesBoundedArgvEnvCWDStdinTimeoutAndOutput(t *testing.T) {
	base := `{"id":"docker-test","name":"web","argv":["printf","ok"],"timeout_seconds":30,"output_limit_bytes":1024}`
	if _, err := dockerExecInput(json.RawMessage(base)); err != nil {
		t.Fatal("valid bounded one-shot command rejected", err)
	}
	for _, raw := range []string{
		`{"id":"docker-test","name":"web","argv":[]}`,
		`{"id":"docker-test","name":"web","argv":["printf"],"daemon":"/var/run/docker.sock"}`,
		`{"id":"docker-test","name":"web","argv":["printf"],"working_dir":"relative"}`,
		`{"id":"docker-test","name":"web","argv":["printf"],"working_dir":"/tmp/../root"}`,
		`{"id":"docker-test","name":"web","argv":["printf"],"timeout_seconds":121}`,
		`{"id":"docker-test","name":"web","argv":["printf"],"output_limit_bytes":65537}`,
		`{"id":"docker-test","name":"web","argv":["printf"],"stdin":"bad\u0000input"}`,
		`{"id":"docker-test","name":"web","argv":["printf"],"env":{"BAD-KEY":"x"}}`,
	} {
		if _, err := dockerExecInput(json.RawMessage(raw)); err == nil {
			t.Fatalf("unsafe or unbounded exec request accepted: %s", raw)
		}
	}
	long := strings.Repeat("x", dockerExecMaxArgSize+1)
	raw, _ := json.Marshal(DockerExecInput{ID: "docker-test", Name: "web", Argv: []string{long}})
	if _, err := dockerExecInput(raw); err == nil {
		t.Fatal("oversized argv was accepted")
	}
}

type dockerExecEngineFixture struct {
	mu                sync.Mutex
	create            map[string]any
	stdin             string
	container         string
	start             bool
	inspect           bool
	wrongOwner        bool
	runningAfterStart bool
	startDelay        time.Duration
}

func newDockerExecEngineFixture(t *testing.T, instance Instance, stdout, stderr string, exitCode int, wrongOwner, runningAfterStart bool, startDelay time.Duration) *dockerExecEngineFixture {
	t.Helper()
	if err := os.MkdirAll(instance.Paths.RunDir, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(instance.Paths.RunDir, "docker.sock"))
	if err != nil {
		t.Fatal("could not create isolated Docker Engine API socket", err)
	}
	fixture := &dockerExecEngineFixture{wrongOwner: wrongOwner, runningAfterStart: runningAfterStart, startDelay: startDelay}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			fixture.mu.Lock()
			fixture.container = r.URL.Path
			fixture.mu.Unlock()
			labels := map[string]string{"io.powerlinux.managed": "true", "io.powerlinux.instance": instance.ID}
			if wrongOwner {
				labels["io.powerlinux.instance"] = "foreign-instance"
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Labels": labels}, "State": map[string]any{"Running": true}})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/exec"):
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("Docker exec create did not send JSON")
			}
			fixture.mu.Lock()
			fixture.create = body
			fixture.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Id":"`+strings.Repeat("a", 64)+`"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["Tty"] != false || body["Detach"] != false {
				t.Error("Docker exec start was not attached and non-TTY")
			}
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("Docker Engine fixture cannot hijack the exec connection")
				return
			}
			conn, rw, hijackErr := hijacker.Hijack()
			if hijackErr != nil {
				t.Error("could not hijack Engine exec stream", hijackErr)
				return
			}
			_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			if rw.Flush() != nil {
				_ = conn.Close()
				return
			}
			input, readErr := io.ReadAll(rw.Reader)
			if readErr != nil {
				t.Error("could not read attached Docker stdin", readErr)
			}
			fixture.mu.Lock()
			fixture.stdin = string(input)
			fixture.start = true
			fixture.mu.Unlock()
			if startDelay > 0 {
				time.Sleep(startDelay)
			}
			_, _ = conn.Write(dockerExecFrame(1, []byte(stdout)))
			_, _ = conn.Write(dockerExecFrame(2, []byte(stderr)))
			_ = conn.Close()
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/exec/"):
			fixture.mu.Lock()
			fixture.inspect = true
			fixture.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			status := map[string]any{"Running": runningAfterStart}
			if !runningAfterStart {
				status["ExitCode"] = exitCode
			}
			_ = json.NewEncoder(w).Encode(status)
		default:
			http.NotFound(w, r)
		}
	})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-done
	})
	return fixture
}

func dockerExecFrame(stream byte, payload []byte) []byte {
	frame := make([]byte, 8+len(payload))
	frame[0] = stream
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], payload)
	return frame
}

func TestDockerExecUsesOwnedContainerAndEngineHijackWithRealExitInspection(t *testing.T) {
	e, instance := dockerImageFixtureAgent(t)
	fixture := newDockerExecEngineFixture(t, instance, "ok:secret-input", "warning", 7, false, false, 0)
	in := dockerExecAgentInput{ID: instance.ID, Name: "web", Argv: []string{"printf", "%s", "literal"}, Environment: []string{"MODE=check"}, WorkingDir: "/tmp", Stdin: "secret-input", TimeoutSeconds: 10, OutputLimitBytes: 1024}
	raw, _ := json.Marshal(in)
	if err := e.validateDockerExec(raw); err != nil {
		t.Fatal("valid Agent exec request failed validation", err)
	}
	got, err := e.dockerExec(context.Background(), raw)
	if err != nil {
		t.Fatal("managed container exec failed", err)
	}
	output, ok := got.(dockerExecOutput)
	if !ok || output.Stdout != "ok:secret-input" || output.Stderr != "warning" || output.ExitCode != 7 || output.StdoutBytes != int64(len(output.Stdout)) || output.StderrBytes != int64(len(output.Stderr)) || output.Truncated {
		t.Fatalf("unexpected bounded exec output: %#v", got)
	}
	fixture.mu.Lock()
	container, create, stdin, started, inspected := fixture.container, fixture.create, fixture.stdin, fixture.start, fixture.inspect
	fixture.mu.Unlock()
	if container != "/v1.45/containers/powerlinux-"+instance.ID+"-web/json" {
		t.Fatalf("exec inspected an unexpected container name: %q", container)
	}
	if create["Tty"] != false || create["AttachStdin"] != true || create["AttachStdout"] != true || create["AttachStderr"] != true || create["WorkingDir"] != "/tmp" {
		t.Fatalf("Docker exec create used unsafe or incomplete options: %#v", create)
	}
	if stdin != "secret-input" || !started || !inspected {
		t.Fatalf("attached stdin/start/exit-inspect lifecycle incomplete: stdin=%q start=%v inspect=%v", stdin, started, inspected)
	}
	statusRaw, _ := json.Marshal(dockerExecStatusInput{ID: instance.ID, Name: "web", ExecutionID: output.ExecutionID})
	statusValue, err := e.dockerExecStatus(context.Background(), statusRaw)
	if err != nil {
		t.Fatal("recorded Docker exec status could not be inspected", err)
	}
	status, ok := statusValue.(dockerExecStatusOutput)
	if !ok || status.ExecutionID != output.ExecutionID || status.ExecutionState != "completed" || status.Running || status.ExitCode == nil || *status.ExitCode != 7 {
		t.Fatalf("recorded exec returned the wrong final Engine state: %#v", statusValue)
	}
}

func TestDockerExecRefusesForeignContainerBeforeExecCreate(t *testing.T) {
	e, instance := dockerImageFixtureAgent(t)
	fixture := newDockerExecEngineFixture(t, instance, "", "", 0, true, false, 0)
	raw, _ := json.Marshal(dockerExecAgentInput{ID: instance.ID, Name: "web", Argv: []string{"true"}, TimeoutSeconds: 5, OutputLimitBytes: 1024})
	if _, err := e.dockerExec(context.Background(), raw); core.ErrorCode(err) != "ownership_invalid" {
		t.Fatalf("foreign container labels passed the ownership check: %v", err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.create != nil || fixture.start || fixture.inspect {
		t.Fatal("foreign container reached exec create/start")
	}
}

func TestDockerExecReportsRunningEngineProcessAfterClientTimeoutAndAllowsStatusCheck(t *testing.T) {
	e, instance := dockerImageFixtureAgent(t)
	fixture := newDockerExecEngineFixture(t, instance, "", "", 0, false, true, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	in := dockerExecAgentInput{ID: instance.ID, Name: "web", Argv: []string{"sleep", "30"}, TimeoutSeconds: 30, OutputLimitBytes: 1024}
	raw, _ := json.Marshal(in)
	got, err := e.dockerExec(ctx, raw)
	if err != nil {
		t.Fatal("timed-out Engine attach did not return its inspected state", err)
	}
	output, ok := got.(dockerExecOutput)
	if !ok || output.ExecutionState != "running" || output.ExecutionID == "" || !output.TimedOut || output.ExitCode != -1 {
		t.Fatalf("Docker exec timeout falsely claimed a completed/canceled process: %#v", got)
	}
	statusRaw, _ := json.Marshal(dockerExecStatusInput{ID: instance.ID, Name: "web", ExecutionID: output.ExecutionID})
	statusValue, err := e.dockerExecStatus(context.Background(), statusRaw)
	if err != nil {
		t.Fatal("orphan execution could not be checked by its recorded identity", err)
	}
	status, ok := statusValue.(dockerExecStatusOutput)
	if !ok || !status.Running || status.ExecutionState != "running" || status.ExitCode != nil {
		t.Fatalf("timeout status hid the active Engine process: %#v", statusValue)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if !fixture.start || !fixture.inspect {
		t.Fatal("timeout path failed to start and inspect the actual Engine exec")
	}
}

type dockerExecTimeoutFixture struct{}

func (dockerExecTimeoutFixture) Error() string   { return "socket deadline elapsed" }
func (dockerExecTimeoutFixture) Timeout() bool   { return true }
func (dockerExecTimeoutFixture) Temporary() bool { return true }

type dockerExecDeadlineRaceContext struct {
	context.Context
	deadline time.Time
}

func (c dockerExecDeadlineRaceContext) Deadline() (time.Time, bool) { return c.deadline, true }
func (dockerExecDeadlineRaceContext) Done() <-chan struct{}         { return nil }
func (dockerExecDeadlineRaceContext) Err() error                    { return nil }

func TestDockerExecSocketDeadlineWinsContextTimerPublicationRace(t *testing.T) {
	err := &net.OpError{Op: "read", Net: "unix", Err: dockerExecTimeoutFixture{}}
	ctx := dockerExecDeadlineRaceContext{Context: context.Background(), deadline: time.Now().Add(-time.Second)}
	if got := dockerExecContextError(ctx, err); got != context.DeadlineExceeded {
		t.Fatalf("expired socket deadline was not attributed to the operation timeout: %v", got)
	}

	future := dockerExecDeadlineRaceContext{Context: context.Background(), deadline: time.Now().Add(time.Second)}
	if got := dockerExecContextError(future, err); got != nil {
		t.Fatalf("unexpired context deadline was misattributed: %v", got)
	}
}

func TestDockerExecFrameDecoderBoundsCombinedOutputAndRedactsLongestSecretFirst(t *testing.T) {
	wire := append(dockerExecFrame(1, []byte("123456")), dockerExecFrame(2, []byte("abcdef"))...)
	stdout, stderr, outN, errN, truncated, err := decodeDockerExecFrames(strings.NewReader(string(wire)), 8)
	if err != nil || string(stdout)+string(stderr) != "123456ab" || outN != 6 || errN != 6 || !truncated {
		t.Fatalf("output cap was not enforced across both streams: %q %q %d %d %v %v", stdout, stderr, outN, errN, truncated, err)
	}
	redacted := redactDockerExecOutput("prefix-longsecret-longsecret-suffix", []string{"longsecret", "longsecret"})
	if redacted != "prefix-[redacted]-[redacted]-suffix" || strings.Contains(redacted, "longsecret") {
		t.Fatalf("secret was not redacted from task output: %q", redacted)
	}
	boundedStdout, boundedStderr, redactionTruncated := boundDockerExecText(redactDockerExecOutput("secret-secret", []string{"secret"}), "also-secret", 8)
	if len(boundedStdout)+len(boundedStderr) > 8 || !redactionTruncated || strings.Contains(boundedStdout+boundedStderr, "secret") {
		t.Fatalf("redaction expansion escaped the task output cap: %q %q truncated=%v", boundedStdout, boundedStderr, redactionTruncated)
	}
}

type dockerExecTaskCapture struct{ input json.RawMessage }

func (c *dockerExecTaskCapture) Submit(_ context.Context, _ core.Actor, operation string, input json.RawMessage, _ []string) (map[string]any, error) {
	if operation != "docker_exec_job" {
		return nil, failure("forbidden", "Unexpected operation", 403)
	}
	c.input = append(json.RawMessage(nil), input...)
	return map[string]any{"id": "captured-task"}, nil
}
func (*dockerExecTaskCapture) SubmitPrepared(context.Context, core.Actor, string, json.RawMessage, core.TaskMetadata, func(core.MetadataStore, jobs.Job) error) (map[string]any, error) {
	return nil, failure("forbidden", "Unexpected operation", 403)
}
func (*dockerExecTaskCapture) Get(context.Context, core.Actor, string) (map[string]any, error) {
	return nil, failure("forbidden", "Unexpected operation", 403)
}
func (*dockerExecTaskCapture) Cancel(context.Context, core.Actor, string) (map[string]any, error) {
	return nil, failure("forbidden", "Unexpected operation", 403)
}

func TestDockerExecActionStagesEnvAndStdinOutsideDurableTaskInput(t *testing.T) {
	s, admin, _ := registryTestService(t)
	instanceID, name := "docker-image-fixture", "web"
	resources := []string{InstanceResource(instanceID), DockerContainerResource(instanceID, name)}
	a := dockerImageToken(t, s, admin, "docker-exec-inputs", []string{"containers:manage"}, resources)
	capture := &dockerExecTaskCapture{}
	deps := s.Runtime("plugins")
	deps.Tasks = capture
	m, err := New(deps, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterProvider(core.Bindings(m.dockerExecBindings())); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"id":"docker-image-fixture","name":"web","argv":["printenv","PASS"],"env":{"PASS":"PRIVATE_ENV_SENTINEL"},"stdin":"PRIVATE_STDIN_SENTINEL","timeout_seconds":10}`)
	result, err := s.Execute(context.Background(), a, "docker_container_exec", raw)
	if err != nil {
		t.Fatal("authorized one-shot Docker command did not enqueue", err)
	}
	if capture.input == nil || strings.Contains(string(capture.input), "PRIVATE_ENV_SENTINEL") || strings.Contains(string(capture.input), "PRIVATE_STDIN_SENTINEL") {
		t.Fatalf("plaintext env/stdin entered durable task input: %s", capture.input)
	}
	var task dockerExecTaskInput
	if json.Unmarshal(capture.input, &task) != nil || task.BundleID == "" || !dockerExecBundleID.MatchString(task.BundleID) {
		t.Fatalf("durable task lacks an opaque staged-input ID: %s", capture.input)
	}
	bundleRaw, err := m.deps.State.Get(context.Background(), dockerExecBundleKind, task.BundleID)
	if err != nil {
		t.Fatal("staged input metadata was not saved", err)
	}
	var record dockerExecBundleRecord
	if json.Unmarshal(bundleRaw, &record) != nil || record.ActorID != a.ID || record.Resource != resources[1] || strings.Contains(string(bundleRaw), "PRIVATE_ENV_SENTINEL") || strings.Contains(string(bundleRaw), "PRIVATE_STDIN_SENTINEL") {
		t.Fatalf("staged-input metadata was not actor/resource bound: %s", bundleRaw)
	}
	secret, err := m.deps.Secrets.Get(context.Background(), a, resources[1], record.SecretRef)
	if err != nil {
		t.Fatal("encrypted staged input could not be resolved for its owner", err)
	}
	defer clear(secret)
	if !strings.Contains(string(secret), "PRIVATE_ENV_SENTINEL") || !strings.Contains(string(secret), "PRIVATE_STDIN_SENTINEL") {
		t.Fatal("encrypted input bundle lost the transient values")
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "PRIVATE_ENV_SENTINEL") || strings.Contains(string(encoded), "PRIVATE_STDIN_SENTINEL") {
		t.Fatal("task submission response exposed staged values")
	}
}

func TestDockerExecJobOperationUsesSeparateNameFromNativeAgentOperation(t *testing.T) {
	if got := DockerExecJobOperations(); len(got) != 1 || got[0] != "docker_exec_job" || got[0] == "docker_exec" {
		t.Fatalf("job/native operation namespaces collide: %v", got)
	}
	request := agent.Request{Operation: "docker_exec_job", Input: json.RawMessage(`{"id":"docker-test","name":"web","argv":["true"],"timeout_seconds":30,"output_limit_bytes":16384}`)}
	if _, err := validateDockerExecTask(request.Input); err != nil {
		t.Fatal("valid internal task payload failed validation", err)
	}
	if _, err := validateDockerExecTask(json.RawMessage(`{"id":"docker-test","name":"web","argv":["true"],"stdin":"plaintext"}`)); err == nil {
		t.Fatal("internal task operation accepted plaintext transient input")
	}
	if !dockerExecBundleID.MatchString("execbundle_" + strings.Repeat("a", 32)) {
		t.Fatal("opaque bundle IDs no longer have their expected format")
	}
}
