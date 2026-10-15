//go:build linux

package plugins

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type supervisorLogRPCFixture struct {
	mu     sync.Mutex
	calls  int
	method string
	name   string
	mode   string
	stdout string
	stderr string
}

func (f *supervisorLogRPCFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "read request", http.StatusBadRequest)
		return
	}
	var request struct {
		MethodName string `xml:"methodName"`
		Params     struct {
			Param []struct {
				Value struct {
					String string `xml:"string"`
				} `xml:"value"`
			} `xml:"param"`
		} `xml:"params"`
	}
	if xml.Unmarshal(body, &request) != nil {
		http.Error(w, "invalid XML-RPC", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.calls++
	f.method = request.MethodName
	if len(request.Params.Param) > 0 {
		f.name = request.Params.Param[0].Value.String
	} else {
		f.name = ""
	}
	mode := f.mode
	f.mu.Unlock()
	if request.MethodName != "supervisor.clearProcessLogs" || len(request.Params.Param) != 1 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if mode == "fault" {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?><methodResponse><fault><value><string>PRIVATE_FIXTURE_PASSWORD_SENTINEL</string></value></fault></methodResponse>`)
		return
	}
	if mode == "false" {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?><methodResponse><params><param><value><boolean>0</boolean></value></param></params></methodResponse>`)
		return
	}
	if mode != "noop" {
		for _, name := range []string{f.stdout, f.stderr} {
			if err = os.Truncate(name, 0); err != nil {
				http.Error(w, "could not clear fixture log", http.StatusInternalServerError)
				return
			}
		}
	}
	w.Header().Set("Content-Type", "text/xml")
	_, _ = io.WriteString(w, `<?xml version="1.0"?><methodResponse><params><param><value><boolean>1</boolean></value></param></params></methodResponse>`)
}

func supervisorLogFixture(t *testing.T) (*PrivilegedExecutor, Instance, *supervisorLogRPCFixture) {
	t.Helper()
	e := testAgent(t)
	shortRoot, err := os.MkdirTemp("/tmp", "plog-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortRoot) })
	e.config.RootDir = filepath.Join(shortRoot, "s")
	e.config.Programs = map[string]TrustedProgram{"trusted": {Program: "/usr/bin/true"}}
	instance := Instance{
		ID:         "supervisor-log-fixture",
		PluginID:   "supervisor",
		InstanceID: "logs-fixture",
		Status:     "running",
		Revision:   4,
		Ownership:  "managed",
		Paths:      e.instancePaths("supervisor", "logs-fixture"),
		Service: ServiceProfile{
			ID:         "supervisor-log-fixture",
			PluginID:   "supervisor",
			InstanceID: "logs-fixture",
			Unit:       "powerlinux-plugin-supervisor-log-fixture.service",
			Ownership:  "managed",
		},
	}
	for _, directory := range []string{instance.Paths.Root, instance.Paths.RunDir, instance.Paths.LogDir} {
		if err := os.MkdirAll(directory, 0750); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.save("receipts", instance.ID, instance); err != nil {
		t.Fatal(err)
	}
	if err := e.save("private", utilityStateID(instance), utilityRecord{Revision: 9, Items: map[string]utilityAccount{
		"worker": {Name: "worker", ProgramID: "trusted", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	stdout := filepath.Join(instance.Paths.LogDir, "worker.log")
	stderr := filepath.Join(instance.Paths.LogDir, "worker.err")
	if err := os.WriteFile(stdout, []byte("stdout fixture\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderr, []byte("stderr fixture\n"), 0640); err != nil {
		t.Fatal(err)
	}
	fixture := &supervisorLogRPCFixture{stdout: stdout, stderr: stderr}
	socket := filepath.Join(instance.Paths.RunDir, "supervisor.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(fixture.serveHTTP)}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})
	return e, instance, fixture
}

func runSupervisorLogAction(t *testing.T, e *PrivilegedExecutor, input SupervisorLogInput, ctx context.Context) (agent.Result, error) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return e.Run(ctx, agent.Request{Operation: "supervisor_log_action", Input: raw})
}

func readSupervisorLogResult(t *testing.T, raw string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		t.Fatalf("invalid Supervisor log result: %v", err)
	}
}

func TestSupervisorLogReadPreviewClearUsesOwnedJobXMLRPC(t *testing.T) {
	e, instance, fixture := supervisorLogFixture(t)
	stdoutPath := filepath.Join(instance.Paths.LogDir, "worker.log")
	large := strings.Repeat("x", supervisorLogReadLimit+50) + "尾\n"
	if err := os.WriteFile(stdoutPath, []byte(large), 0640); err != nil {
		t.Fatal(err)
	}
	read, err := runSupervisorLogAction(t, e, SupervisorLogInput{ID: instance.ID, Command: "read", Name: "worker", Stream: "stdout", MaxBytes: 32}, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var readView struct {
		ID           string                `json:"id"`
		Name         string                `json:"name"`
		Stream       string                `json:"stream"`
		Text         string                `json:"text"`
		Bytes        int                   `json:"bytes"`
		Truncated    bool                  `json:"truncated"`
		Revision     int64                 `json:"revision"`
		FileIdentity SupervisorLogIdentity `json:"file_identity"`
	}
	readSupervisorLogResult(t, read.Stdout, &readView)
	if readView.ID != instance.ID || readView.Name != "worker" || readView.Stream != "stdout" || readView.Revision != 9 || len(readView.Text) > 32 || !readView.Truncated || !utf8.ValidString(readView.Text) || !strings.HasSuffix(readView.Text, "\n") {
		t.Fatalf("bounded tail response was incorrect: %#v", readView)
	}

	preview, err := runSupervisorLogAction(t, e, SupervisorLogInput{ID: instance.ID, Command: "preview_clear", Name: "worker"}, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var previewView struct {
		ID            string                `json:"id"`
		Name          string                `json:"name"`
		Revision      int64                 `json:"revision"`
		Stdout        SupervisorLogIdentity `json:"stdout"`
		Stderr        SupervisorLogIdentity `json:"stderr"`
		ClearsStreams []string              `json:"clears_streams"`
	}
	readSupervisorLogResult(t, preview.Stdout, &previewView)
	if previewView.Revision != 9 || !previewView.Stdout.Exists || !previewView.Stderr.Exists || len(previewView.ClearsStreams) != 2 || previewView.ClearsStreams[0] != "stdout" || previewView.ClearsStreams[1] != "stderr" {
		t.Fatalf("clear preview did not include both log identities: %#v", previewView)
	}
	if !validateSupervisorLogIdentity(&previewView.Stdout) || !validateSupervisorLogIdentity(&previewView.Stderr) {
		t.Fatal("preview identities are not safe exact decimal strings")
	}

	clear, err := runSupervisorLogAction(t, e, SupervisorLogInput{ID: instance.ID, Command: "clear", Name: "worker", ExpectedRevision: previewView.Revision, ExpectedStdout: &previewView.Stdout, ExpectedStderr: &previewView.Stderr}, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var clearView struct {
		Cleared []string              `json:"cleared_streams"`
		Stdout  SupervisorLogIdentity `json:"stdout_after"`
		Stderr  SupervisorLogIdentity `json:"stderr_after"`
	}
	readSupervisorLogResult(t, clear.Stdout, &clearView)
	if len(clearView.Cleared) != 2 || clearView.Cleared[0] != "stdout" || clearView.Cleared[1] != "stderr" || clearView.Stdout.Size != "0" || clearView.Stderr.Size != "0" {
		t.Fatalf("clear did not report and verify both streams: %#v", clearView)
	}
	if data, err := os.ReadFile(stdoutPath); err != nil || len(data) != 0 {
		t.Fatalf("stdout file was not actually cleared: %q %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(instance.Paths.LogDir, "worker.err")); err != nil || len(data) != 0 {
		t.Fatalf("stderr file was not actually cleared: %q %v", data, err)
	}
	fixture.mu.Lock()
	calls, method, name := fixture.calls, fixture.method, fixture.name
	fixture.mu.Unlock()
	if calls != 1 || method != "supervisor.clearProcessLogs" || name != "worker" {
		t.Fatalf("clear used an unexpected Supervisor RPC: calls=%d method=%q name=%q", calls, method, name)
	}
}

func TestSupervisorLogClearRejectsStaleIdentityAndRPCFailureWithoutLeak(t *testing.T) {
	e, instance, fixture := supervisorLogFixture(t)
	preview, err := runSupervisorLogAction(t, e, SupervisorLogInput{ID: instance.ID, Command: "preview_clear", Name: "worker"}, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Revision int64                 `json:"revision"`
		Stdout   SupervisorLogIdentity `json:"stdout"`
		Stderr   SupervisorLogIdentity `json:"stderr"`
	}
	readSupervisorLogResult(t, preview.Stdout, &view)
	stderrPath := filepath.Join(instance.Paths.LogDir, "worker.err")
	if err = os.Rename(stderrPath, stderrPath+".rotated"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(stderrPath, []byte("new generation"), 0640); err != nil {
		t.Fatal(err)
	}
	input := SupervisorLogInput{ID: instance.ID, Command: "clear", Name: "worker", ExpectedRevision: view.Revision, ExpectedStdout: &view.Stdout, ExpectedStderr: &view.Stderr}
	if _, err = runSupervisorLogAction(t, e, input, context.Background()); core.ErrorCode(err) != "revision_conflict" {
		t.Fatalf("replacement after preview was not rejected: %v", err)
	}
	fixture.mu.Lock()
	callsAfterConflict := fixture.calls
	fixture.mu.Unlock()
	if callsAfterConflict != 0 {
		t.Fatal("stale preview reached Supervisor RPC")
	}

	fixture.mu.Lock()
	fixture.mode = "fault"
	fixture.mu.Unlock()
	preview, err = runSupervisorLogAction(t, e, SupervisorLogInput{ID: instance.ID, Command: "preview_clear", Name: "worker"}, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	readSupervisorLogResult(t, preview.Stdout, &view)
	input.ExpectedRevision, input.ExpectedStdout, input.ExpectedStderr = view.Revision, &view.Stdout, &view.Stderr
	result, err := runSupervisorLogAction(t, e, input, context.Background())
	if err == nil || strings.Contains(err.Error(), "PRIVATE_FIXTURE_PASSWORD_SENTINEL") || strings.Contains(result.Stderr, "PRIVATE_FIXTURE_PASSWORD_SENTINEL") {
		t.Fatalf("Supervisor fault was not safely rejected: err=%v stderr=%q", err, result.Stderr)
	}
	if data, readErr := os.ReadFile(filepath.Join(instance.Paths.LogDir, "worker.log")); readErr != nil || string(data) != "stdout fixture\n" {
		t.Fatalf("failed clear changed stdout: %q %v", data, readErr)
	}
	if data, readErr := os.ReadFile(stderrPath); readErr != nil || string(data) != "new generation" {
		t.Fatalf("failed clear changed stderr: %q %v", data, readErr)
	}
}

func TestSupervisorLogClearRejectsTrueNoopRPCWithoutClaimingSuccess(t *testing.T) {
	e, instance, fixture := supervisorLogFixture(t)
	preview, err := runSupervisorLogAction(t, e, SupervisorLogInput{ID: instance.ID, Command: "preview_clear", Name: "worker"}, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Revision int64
		Stdout   SupervisorLogIdentity
		Stderr   SupervisorLogIdentity
	}
	readSupervisorLogResult(t, preview.Stdout, &view)
	fixture.mu.Lock()
	fixture.mode = "noop"
	fixture.mu.Unlock()
	result, err := runSupervisorLogAction(t, e, SupervisorLogInput{
		ID: instance.ID, Command: "clear", Name: "worker", ExpectedRevision: view.Revision,
		ExpectedStdout: &view.Stdout, ExpectedStderr: &view.Stderr,
	}, context.Background())
	if core.ErrorCode(err) != "log_clear_verification_failed" || strings.Contains(result.Stdout, "cleared_streams") {
		t.Fatalf("successful no-op RPC was reported as cleared: err=%v stdout=%q", err, result.Stdout)
	}
	if data, readErr := os.ReadFile(filepath.Join(instance.Paths.LogDir, "worker.log")); readErr != nil || string(data) != "stdout fixture\n" {
		t.Fatalf("no-op clear changed stdout: %q %v", data, readErr)
	}
	if data, readErr := os.ReadFile(filepath.Join(instance.Paths.LogDir, "worker.err")); readErr != nil || string(data) != "stderr fixture\n" {
		t.Fatalf("no-op clear changed stderr: %q %v", data, readErr)
	}
}

func TestSupervisorLogRejectsUnregisteredPathsAndUnsafeFileTypes(t *testing.T) {
	t.Run("input-cannot-name-path", func(t *testing.T) {
		e, instance, fixture := supervisorLogFixture(t)
		outside := filepath.Join(t.TempDir(), "sentinel")
		if err := os.WriteFile(outside, []byte("keep me"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := e.Validate(agent.Request{Operation: "supervisor_log_action", Input: []byte(`{"id":"supervisor-log-fixture","command":"read","name":"../sentinel","stream":"stdout"}`)}); core.ErrorCode(err) != "invalid_input" {
			t.Fatalf("path-like job name was not rejected: %v", err)
		}
		if _, err := runSupervisorLogAction(t, e, SupervisorLogInput{ID: instance.ID, Command: "read", Name: "worker", Stream: "stdout"}, context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.mu.Lock()
		calls := fixture.calls
		fixture.mu.Unlock()
		if calls != 0 {
			t.Fatal("read contacted Supervisor RPC unexpectedly")
		}
		if data, err := os.ReadFile(outside); err != nil || string(data) != "keep me" {
			t.Fatalf("external file changed: %q %v", data, err)
		}
	})
	for _, kind := range []string{"symlink", "hardlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			e, instance, _ := supervisorLogFixture(t)
			log := filepath.Join(instance.Paths.LogDir, "worker.log")
			if err := os.Remove(log); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "outside.log")
			if err := os.WriteFile(outside, []byte("external log sentinel"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				if err := os.Symlink(outside, log); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(outside, log); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(log, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := runSupervisorLogAction(t, e, SupervisorLogInput{ID: instance.ID, Command: "read", Name: "worker", Stream: "stdout"}, ctx); core.ErrorCode(err) != "log_file_unsafe" {
				t.Fatalf("unsafe %s was not rejected: %v", kind, err)
			}
			if data, err := os.ReadFile(outside); err != nil || string(data) != "external log sentinel" {
				t.Fatalf("external file changed: %q %v", data, err)
			}
		})
	}
}

func TestSupervisorLogCancelDoesNotCallRPCAndConcurrentReadIsBounded(t *testing.T) {
	e, instance, fixture := supervisorLogFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runSupervisorLogAction(t, e, SupervisorLogInput{ID: instance.ID, Command: "clear", Name: "worker", ExpectedRevision: 9, ExpectedStdout: &SupervisorLogIdentity{Exists: true, Size: "15", ModTimeNS: "0", Device: "1", Inode: "1"}, ExpectedStderr: &SupervisorLogIdentity{Exists: true, Size: "15", ModTimeNS: "0", Device: "1", Inode: "2"}}, ctx); err == nil {
		t.Fatal("canceled clear proceeded")
	}
	fixture.mu.Lock()
	calls := fixture.calls
	fixture.mu.Unlock()
	if calls != 0 {
		t.Fatal("canceled clear reached Supervisor RPC")
	}

	log := filepath.Join(instance.Paths.LogDir, "worker.log")
	if err := os.WriteFile(log, []byte(strings.Repeat("a", supervisorLogReadLimit*2)), 0640); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := runSupervisorLogAction(t, e, SupervisorLogInput{ID: instance.ID, Command: "read", Name: "worker", Stream: "stdout"}, context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			var output struct {
				Text string `json:"text"`
			}
			if json.Unmarshal([]byte(result.Stdout), &output) != nil || len(output.Text) > supervisorLogReadLimit {
				t.Error("concurrent bounded read returned an invalid or oversized response")
			}
		}()
	}
	wg.Wait()
}

func TestSupervisorLogBindingsDeclareSharedReadAndDurableClearScopes(t *testing.T) {
	bindings := (&Manager{}).supervisorLogBindings()
	if len(bindings) != 3 {
		t.Fatalf("expected read, clear preview and clear bindings, got %d", len(bindings))
	}
	want := map[string]struct {
		scope    string
		readOnly bool
		task     bool
	}{
		"supervisor_log_read":          {scope: "supervisor:read", readOnly: true},
		"supervisor_log_clear_preview": {scope: "supervisor:read", readOnly: true},
		"supervisor_log_clear":         {scope: "supervisor:manage", task: true},
	}
	service, err := core.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	if err = service.RegisterProvider(core.Bindings(bindings)); err != nil {
		t.Fatalf("Supervisor action schemas did not register: %v", err)
	}
	for _, binding := range bindings {
		expected, exists := want[binding.Action.Name]
		if !exists || binding.Action.Scope != expected.scope || binding.Action.ReadOnly != expected.readOnly || binding.Action.Task != expected.task {
			t.Fatalf("incorrect capability for %s: scope=%s read_only=%t task=%t", binding.Action.Name, binding.Action.Scope, binding.Action.ReadOnly, binding.Action.Task)
		}
	}
	clear := bindings[2]
	properties := clear.Action.InputSchema["properties"].(map[string]any)
	for _, field := range []string{"expected_stdout", "expected_stderr"} {
		identity, ok := properties[field].(map[string]any)
		if !ok {
			t.Fatalf("clear schema omitted %s identity", field)
		}
		required, ok := identity["required"].([]string)
		if !ok || len(required) != 5 {
			t.Fatalf("clear schema does not require every exact identity field for %s: %#v", field, identity)
		}
	}
}
