package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/jobs"
)

func phpMaintenanceFixture(t *testing.T) (*PrivilegedExecutor, Instance) {
	t.Helper()
	e := testAgent(t)
	paths := e.instancePaths("php", "maintenance-fixture")
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{
		{filepath.Join(e.config.RootDir, "php"), 0750},
		{paths.Root, 0750},
		{paths.ConfigDir, 0750},
		{paths.DataDir, 0700},
		{paths.RunDir, 0700},
		{paths.LogDir, 0700},
		{paths.BackupDir, 0700},
		{filepath.Join(paths.DataDir, "sessions"), 0700},
	} {
		if err := os.MkdirAll(item.path, item.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(item.path, item.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(item.path, int(e.uid), int(e.gid)); err != nil {
			t.Fatal(err)
		}
	}
	instance := Instance{
		ID: "php-maintenance-fixture", PluginID: "php", InstanceID: "maintenance-fixture", Revision: 7,
		Status: "running", Ownership: "managed", Paths: paths, Config: map[string]any{},
		Service: ServiceProfile{ID: "php-maintenance-fixture", Unit: "powerlinux-plugin-php-maintenance-fixture.service"},
	}
	if err := e.save("receipts", instance.ID, instance); err != nil {
		t.Fatal(err)
	}
	return e, instance
}

func writePHPSession(t *testing.T, e *PrivilegedExecutor, instance Instance, name string, modified time.Time, body string) string {
	t.Helper()
	path := filepath.Join(instance.Paths.DataDir, "sessions", name)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, int(e.uid), int(e.gid)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	return path
}

func maintenanceInput(t *testing.T, input PHPMaintenanceInput) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func runPHPMaintenance(t *testing.T, e *PrivilegedExecutor, ctx context.Context, input PHPMaintenanceInput) (any, error) {
	t.Helper()
	return e.RunPHPMaintenance(ctx, maintenanceInput(t, input))
}

func TestPHPMaintenanceSessionsRequireImmutableFreshSnapshot(t *testing.T) {
	e, instance := phpMaintenanceFixture(t)
	old := time.Now().Add(-48 * time.Hour)
	stale := writePHPSession(t, e, instance, "sess_oldfixture", old, "private-old-session-payload")
	fresh := writePHPSession(t, e, instance, "sess_newfixture", time.Now(), "private-new-session-payload")
	ctx := context.Background()

	statsAny, err := runPHPMaintenance(t, e, ctx, PHPMaintenanceInput{ID: instance.ID, Command: "session_stats", OlderThanSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	stats := statsAny.(map[string]any)
	if stats["total_sessions"] != 2 || stats["expired_sessions"] != 1 || stats["revision"] != int64(instance.Revision) || !slices.Equal(stats["allow_actions"].([]string), []string{"preview"}) {
		t.Fatalf("unexpected aggregate session stats: %#v", stats)
	}
	statsJSON, _ := json.Marshal(stats)
	for _, private := range []string{"sess_oldfixture", "sess_newfixture", "private-old-session-payload", "private-new-session-payload"} {
		if strings.Contains(string(statsJSON), private) {
			t.Fatalf("session details escaped the aggregate response: %s", statsJSON)
		}
	}

	previewAny, err := runPHPMaintenance(t, e, ctx, PHPMaintenanceInput{ID: instance.ID, Command: "session_preview", ExpectedRevision: instance.Revision, OlderThanSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	preview := previewAny.(map[string]any)
	previewID, _ := preview["preview_id"].(string)
	if !phpMaintenancePreviewID.MatchString(previewID) || preview["candidate_count"] != 1 || preview["candidate_bytes"] != int64(len("private-old-session-payload")) || !slices.Equal(preview["allow_actions"].([]string), []string{"cleanup"}) {
		t.Fatalf("unexpected immutable cleanup preview: %#v", preview)
	}
	previewJSON, _ := json.Marshal(preview)
	for _, private := range []string{"sess_oldfixture", "private-old-session-payload"} {
		if strings.Contains(string(previewJSON), private) {
			t.Fatalf("session identity or contents escaped preview: %s", previewJSON)
		}
	}

	// A session updated after preview keeps its path and inode but changes its
	// metadata. The stale snapshot must stop before deleting any candidate.
	if err = os.WriteFile(stale, []byte("newer-session-data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = runPHPMaintenance(t, e, ctx, PHPMaintenanceInput{ID: instance.ID, Command: "session_cleanup", ExpectedRevision: instance.Revision, PreviewID: previewID}); core.ErrorCode(err) != "preview_stale" {
		t.Fatalf("updated session did not invalidate the preview: %v", err)
	}
	if body, readErr := os.ReadFile(stale); readErr != nil || string(body) != "newer-session-data" {
		t.Fatalf("updated session data was not preserved: %q, %v", body, readErr)
	}

	// A fresh preview binds the updated file identity and age. Cleanup then
	// removes only that exact stale file and retains the fresh session.
	if err = os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	previewAny, err = runPHPMaintenance(t, e, ctx, PHPMaintenanceInput{ID: instance.ID, Command: "session_preview", ExpectedRevision: instance.Revision, OlderThanSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	preview = previewAny.(map[string]any)
	previewID = preview["preview_id"].(string)
	cleanupAny, err := runPHPMaintenance(t, e, ctx, PHPMaintenanceInput{ID: instance.ID, Command: "session_cleanup", ExpectedRevision: instance.Revision, PreviewID: previewID})
	if err != nil {
		t.Fatal("valid preview cleanup failed", err)
	}
	cleanup := cleanupAny.(phpMaintenanceCleanupResult)
	if cleanup.DeletedCount != 1 || cleanup.DeletedBytes != int64(len("newer-session-data")) {
		t.Fatalf("unexpected cleanup result: %#v", cleanup)
	}
	if _, err = os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatal("expired session was not removed", err)
	}
	if body, readErr := os.ReadFile(fresh); readErr != nil || string(body) != "private-new-session-payload" {
		t.Fatalf("fresh session was changed: %q, %v", body, readErr)
	}
	replayed, err := runPHPMaintenance(t, e, ctx, PHPMaintenanceInput{ID: instance.ID, Command: "session_cleanup", ExpectedRevision: instance.Revision, PreviewID: previewID})
	if err != nil || replayed.(phpMaintenanceCleanupResult) != cleanup {
		t.Fatalf("completed preview was not idempotent: %#v, %v", replayed, err)
	}
}

func TestPHPMaintenanceSessionCleanupRejectsReplacedInodeAndRevision(t *testing.T) {
	e, instance := phpMaintenanceFixture(t)
	old := time.Now().Add(-48 * time.Hour)
	path := writePHPSession(t, e, instance, "sess_replacefixture", old, "old-session")
	previewAny, err := runPHPMaintenance(t, e, context.Background(), PHPMaintenanceInput{ID: instance.ID, Command: "session_preview", ExpectedRevision: instance.Revision, OlderThanSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	previewID := previewAny.(map[string]any)["preview_id"].(string)
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	writePHPSession(t, e, instance, "sess_replacefixture", old, "replacement-session-data")
	if _, err = runPHPMaintenance(t, e, context.Background(), PHPMaintenanceInput{ID: instance.ID, Command: "session_cleanup", ExpectedRevision: instance.Revision, PreviewID: previewID}); core.ErrorCode(err) != "preview_stale" {
		t.Fatalf("replacement inode was accepted: %v", err)
	}
	if body, readErr := os.ReadFile(path); readErr != nil || string(body) != "replacement-session-data" {
		t.Fatalf("replacement session was not preserved: %q, %v", body, readErr)
	}
	if _, err = runPHPMaintenance(t, e, context.Background(), PHPMaintenanceInput{ID: instance.ID, Command: "session_cleanup", ExpectedRevision: instance.Revision + 1, PreviewID: previewID}); core.ErrorCode(err) != "revision_conflict" {
		t.Fatalf("stale receipt revision was accepted: %v", err)
	}
}

func TestPHPMaintenanceSessionDirectoryFailsClosedForLinksAndSharing(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *PrivilegedExecutor, Instance)
	}{
		{name: "symlink", setup: func(t *testing.T, e *PrivilegedExecutor, instance Instance) {
			outside := filepath.Join(t.TempDir(), "outside-session")
			if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(instance.Paths.DataDir, "sessions", "sess_linkfixture")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hardlink", setup: func(t *testing.T, e *PrivilegedExecutor, instance Instance) {
			outside := filepath.Join(t.TempDir(), "outside-session")
			if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(outside, int(e.uid), int(e.gid)); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(outside, filepath.Join(instance.Paths.DataDir, "sessions", "sess_linkfixture")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "shared_directory", setup: func(t *testing.T, e *PrivilegedExecutor, instance Instance) {
			if err := os.Chmod(filepath.Join(instance.Paths.DataDir, "sessions"), 0750); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			e, instance := phpMaintenanceFixture(t)
			test.setup(t, e, instance)
			if _, err := runPHPMaintenance(t, e, context.Background(), PHPMaintenanceInput{ID: instance.ID, Command: "session_stats", OlderThanSeconds: 3600}); core.ErrorCode(err) != "session_directory_unsafe" {
				t.Fatalf("unsafe session path was accepted: %v", err)
			}
		})
	}
}

func TestPHPMaintenanceCancelPreservesSessionData(t *testing.T) {
	e, instance := phpMaintenanceFixture(t)
	old := time.Now().Add(-48 * time.Hour)
	path := writePHPSession(t, e, instance, "sess_cancel-fixture", old, "preserve-on-cancel")
	previewAny, err := runPHPMaintenance(t, e, context.Background(), PHPMaintenanceInput{ID: instance.ID, Command: "session_preview", ExpectedRevision: instance.Revision, OlderThanSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	previewID := previewAny.(map[string]any)["preview_id"].(string)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = runPHPMaintenance(t, e, canceled, PHPMaintenanceInput{ID: instance.ID, Command: "session_cleanup", ExpectedRevision: instance.Revision, PreviewID: previewID}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup continued: %v", err)
	}
	other, err := NewPrivilegedExecutor(e.config)
	if err != nil {
		t.Fatal(err)
	}
	held, err := other.lockContext(context.Background(), instance.ID)
	if err != nil {
		t.Fatal("could not acquire cross-executor fixture lock", err)
	}
	short, stop := context.WithTimeout(context.Background(), 80*time.Millisecond)
	_, lockErr := runPHPMaintenance(t, e, short, PHPMaintenanceInput{ID: instance.ID, Command: "session_cleanup", ExpectedRevision: instance.Revision, PreviewID: previewID})
	stop()
	held()
	if !errors.Is(lockErr, context.DeadlineExceeded) {
		t.Fatalf("cleanup did not wait for the shared cross-process instance lock: %v", lockErr)
	}
	if body, readErr := os.ReadFile(path); readErr != nil || string(body) != "preserve-on-cancel" {
		t.Fatalf("canceled cleanup changed session data: %q, %v", body, readErr)
	}
}

func TestPHPMaintenanceLogIsBoundedOwnedAndHonestWhenAbsent(t *testing.T) {
	e, instance := phpMaintenanceFixture(t)
	missingAny, err := runPHPMaintenance(t, e, context.Background(), PHPMaintenanceInput{ID: instance.ID, Command: "log_read", Stream: "fpm_error"})
	if err != nil || missingAny.(map[string]any)["generated"] != false {
		t.Fatalf("missing error log was fabricated: %#v, %v", missingAny, err)
	}
	var body strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&body, "line-%03d\n", i)
	}
	logPath := filepath.Join(instance.Paths.LogDir, "fpm-error.log")
	if err = os.WriteFile(logPath, []byte(body.String()), 0640); err != nil {
		t.Fatal(err)
	}
	logUID, logGID := e.uid, e.gid
	if os.Geteuid() == 0 {
		logUID, logGID = 0, 0 // The FPM master may create the configured global log as root.
	}
	if err = os.Chown(logPath, int(logUID), int(logGID)); err != nil {
		t.Fatal(err)
	}
	logAny, err := runPHPMaintenance(t, e, context.Background(), PHPMaintenanceInput{ID: instance.ID, Command: "log_read", Stream: "fpm_error", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	log := logAny.(map[string]any)
	lines := log["lines"].([]string)
	if len(lines) != 3 || lines[0] != "line-297" || lines[2] != "line-299" || log["generated"] != true || log["truncated"] != true {
		t.Fatalf("log tail was not correctly bounded: %#v", log)
	}
	_ = os.Remove(logPath)
	if err = os.Symlink(filepath.Join(t.TempDir(), "foreign.log"), logPath); err != nil {
		t.Fatal(err)
	}
	if _, err = runPHPMaintenance(t, e, context.Background(), PHPMaintenanceInput{ID: instance.ID, Command: "log_read", Stream: "fpm_error"}); core.ErrorCode(err) != "session_directory_unsafe" {
		t.Fatalf("symlinked log was accepted: %v", err)
	}
	if err = e.ValidatePHPMaintenance(maintenanceInput(t, PHPMaintenanceInput{ID: instance.ID, Command: "log_read", Stream: "slow"})); err != nil {
		t.Fatalf("typed slowlog stream was rejected: %v", err)
	}
}

func TestPHPMaintenanceSlowLogRequiresEnabledConfigAndReadsOnlyTrustedPath(t *testing.T) {
	e, instance := phpMaintenanceFixture(t)
	missingAny, err := runPHPMaintenance(t, e, context.Background(), PHPMaintenanceInput{ID: instance.ID, Command: "log_read", Stream: "slow"})
	if err != nil {
		t.Fatal(err)
	}
	missing := missingAny.(map[string]any)
	if missing["configured"] != false || missing["generated"] != false || len(missing["lines"].([]string)) != 0 {
		t.Fatalf("disabled slowlog was represented as configured or generated: %#v", missing)
	}
	instance.Config["slowlog_enabled"] = true
	instance.Config["slowlog_timeout_seconds"] = 5
	if err = e.save("receipts", instance.ID, instance); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(instance.Paths.LogDir, "fpm-slow.log")
	if err = os.WriteFile(path, []byte("slow-one\nslow-two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chown(path, int(e.uid), int(e.gid)); err != nil {
		t.Fatal(err)
	}
	resultAny, err := runPHPMaintenance(t, e, context.Background(), PHPMaintenanceInput{ID: instance.ID, Command: "log_read", Stream: "slow", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	result := resultAny.(map[string]any)
	lines := result["lines"].([]string)
	if result["configured"] != true || result["generated"] != true || len(lines) != 1 || lines[0] != "slow-two" {
		t.Fatalf("configured slowlog was not read with a bounded tail: %#v", result)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(t.TempDir(), "foreign.log"), path); err != nil {
		t.Fatal(err)
	}
	if _, err = runPHPMaintenance(t, e, context.Background(), PHPMaintenanceInput{ID: instance.ID, Command: "log_read", Stream: "slow"}); core.ErrorCode(err) != "session_directory_unsafe" {
		t.Fatalf("symlinked slowlog was accepted: %v", err)
	}
}

func TestPHPMaintenanceLogRejectsFIFOWithoutBlocking(t *testing.T) {
	e, instance := phpMaintenanceFixture(t)
	logPath := filepath.Join(instance.Paths.LogDir, "fpm-error.log")
	if err := syscall.Mkfifo(logPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(logPath, int(e.uid), int(e.gid)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := runPHPMaintenance(t, e, context.Background(), PHPMaintenanceInput{ID: instance.ID, Command: "log_read", Stream: "fpm_error"})
		done <- err
	}()
	select {
	case err := <-done:
		if core.ErrorCode(err) != "session_directory_unsafe" {
			t.Fatalf("FIFO log was not rejected as unsafe: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("opening a substituted FIFO log blocked instead of rejecting it")
	}
}

func TestPHPMaintenanceSessionOpenRejectsFIFOWithoutBlocking(t *testing.T) {
	e, instance := phpMaintenanceFixture(t)
	old := time.Now().Add(-48 * time.Hour)
	path := writePHPSession(t, e, instance, "sess_fifo-fixture", old, "private-session-payload")
	root, _, err := e.phpSessionDirectory(instance)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	freshSnapshot, err := scanPHPSessions(context.Background(), root, e.uid, e.gid)
	if err != nil || len(freshSnapshot) != 1 {
		t.Fatalf("could not capture session identity: %#v, %v", freshSnapshot, err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chown(path, int(e.uid), int(e.gid)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		item, openErr := openPHPLockedSession(root, freshSnapshot[0], e.uid, e.gid)
		if item != nil {
			item.close()
		}
		done <- openErr
	}()
	select {
	case err = <-done:
		if core.ErrorCode(err) != "preview_stale" {
			t.Fatalf("FIFO session replacement was not rejected as stale: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("opening a substituted FIFO session blocked instead of rejecting it")
	}
}

func TestPHPMaintenanceSessionsRequireManagedFilesHandler(t *testing.T) {
	e, instance := phpMaintenanceFixture(t)
	for _, test := range []struct {
		name  string
		key   string
		value any
		code  string
	}{
		{name: "remote handler", key: "session_handler", value: "redis", code: "unsupported_session_handler"},
		{name: "foreign path", key: "session_path", value: filepath.Join(t.TempDir(), "sessions"), code: "session_directory_unsafe"},
	} {
		t.Run(test.name, func(t *testing.T) {
			instance.Config = map[string]any{test.key: test.value}
			if err := e.save("receipts", instance.ID, instance); err != nil {
				t.Fatal(err)
			}
			_, err := runPHPMaintenance(t, e, context.Background(), PHPMaintenanceInput{ID: instance.ID, Command: "session_stats", OlderThanSeconds: 3600})
			if core.ErrorCode(err) != test.code {
				t.Fatalf("unmanaged session storage was accepted or mislabeled: %v", err)
			}
		})
	}
}

type phpMaintenanceTaskFixture struct {
	operation string
	input     json.RawMessage
	resources []string
}

func (f *phpMaintenanceTaskFixture) Submit(_ context.Context, _ core.Actor, operation string, input json.RawMessage, resources []string) (map[string]any, error) {
	f.operation = operation
	f.input = append(json.RawMessage(nil), input...)
	f.resources = slices.Clone(resources)
	return map[string]any{"id": "task-fixture"}, nil
}
func (*phpMaintenanceTaskFixture) SubmitPrepared(context.Context, core.Actor, string, json.RawMessage, core.TaskMetadata, func(core.MetadataStore, jobs.Job) error) (map[string]any, error) {
	return nil, errors.New("unexpected prepared task")
}
func (*phpMaintenanceTaskFixture) Get(context.Context, core.Actor, string) (map[string]any, error) {
	return nil, errors.New("unexpected task lookup")
}
func (*phpMaintenanceTaskFixture) Cancel(context.Context, core.Actor, string) (map[string]any, error) {
	return nil, errors.New("unexpected task cancellation")
}

func TestPHPMaintenanceBindingsUseScopeResourceAndDurableCleanupTask(t *testing.T) {
	tasks := &phpMaintenanceTaskFixture{}
	manager := &Manager{deps: core.Dependencies{Tasks: tasks}}
	bindings := manager.phpMaintenanceBindings()
	if len(bindings) != 4 {
		t.Fatalf("expected four typed maintenance actions, got %d", len(bindings))
	}
	want := map[string]string{
		"php_maintenance_log_read":        "php:read",
		"php_maintenance_session_stats":   "php:read",
		"php_maintenance_session_preview": "php:read",
		"php_maintenance_session_cleanup": "php:manage",
	}
	for _, binding := range bindings {
		if want[binding.Action.Name] != binding.Action.Scope {
			t.Fatalf("action %s has wrong scope %q", binding.Action.Name, binding.Action.Scope)
		}
		if binding.Action.Name == "php_maintenance_session_cleanup" && !binding.Action.Task {
			t.Fatal("cleanup must be a durable task")
		}
	}
	var cleanup core.Binding
	for _, binding := range bindings {
		if binding.Action.Name == "php_maintenance_session_cleanup" {
			cleanup = binding
		}
	}
	input := json.RawMessage(`{"id":"php-test","expected_revision":7,"preview_id":"pmv_0123456789abcdef0123456789abcdef0123456789abcdef"}`)
	result, err := cleanup.Execute(context.Background(), core.Actor{}, input)
	if err != nil {
		t.Fatal(err)
	}
	var submitted PHPMaintenanceInput
	if err = json.Unmarshal(tasks.input, &submitted); err != nil {
		t.Fatal(err)
	}
	if tasks.operation != "php_maintenance" || submitted.Command != "session_cleanup" || submitted.ExpectedRevision != 7 || submitted.PreviewID == "" || !slices.Equal(tasks.resources, []string{InstanceResource("php-test")}) || result.TaskID != "task-fixture" {
		t.Fatalf("cleanup was not submitted as a resource-bound durable task: %#v, %#v, %#v", tasks, submitted, result)
	}
}

func TestPHPMaintenanceFixtureUsesWorkerOwnership(t *testing.T) {
	e, instance := phpMaintenanceFixture(t)
	worker, err := user.LookupId(fmt.Sprint(e.uid))
	if err != nil || worker.Username == "" {
		t.Fatal("fixture worker identity missing", err)
	}
	info, err := os.Stat(filepath.Join(instance.Paths.DataDir, "sessions"))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("fixture session directory not private", err)
	}
}

func TestPHPMaintenanceNativeOperationDispatch(t *testing.T) {
	e, instance := phpMaintenanceFixture(t)
	if !slices.Contains(Operations(), "php_maintenance") {
		t.Fatal("php_maintenance is absent from the registered native Agent operations")
	}
	raw := maintenanceInput(t, PHPMaintenanceInput{ID: instance.ID, Command: "session_stats", OlderThanSeconds: 3600})
	result, err := e.Run(context.Background(), agent.Request{Operation: "php_maintenance", Input: raw})
	if err != nil {
		t.Fatal("registered PHP maintenance operation failed", err)
	}
	var output map[string]any
	if err = json.Unmarshal([]byte(result.Stdout), &output); err != nil {
		t.Fatal("native operation returned invalid JSON", err)
	}
	if output["id"] != instance.ID || output["generated"] != true || output["total_sessions"] != float64(0) {
		t.Fatalf("native operation response mismatch: %#v", output)
	}
}

func TestPHPMaintenanceInstallRecoveryJournalBlocksCleanup(t *testing.T) {
	e, instance := phpMaintenanceFixture(t)
	old := time.Now().Add(-48 * time.Hour)
	path := writePHPSession(t, e, instance, "sess_installfixture", old, "preserve-during-recovery")
	previewAny, err := runPHPMaintenance(t, e, context.Background(), PHPMaintenanceInput{ID: instance.ID, Command: "session_preview", ExpectedRevision: instance.Revision, OlderThanSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	previewID := previewAny.(map[string]any)["preview_id"].(string)
	manifest, err := e.catalog.Get(instance.PluginID)
	if err != nil || len(manifest.Versions) == 0 {
		t.Fatal("trusted PHP version fixture missing", err)
	}
	planID, err := randomPlanID()
	if err != nil {
		t.Fatal(err)
	}
	version := manifest.Versions[0]
	resources := []string{InstanceResource(instance.ID)}
	if sourceUsesSystemPackages(version.Source) {
		resources = append(resources, "host:packages")
	}
	plan := Plan{ID: planID, InstanceID: instance.ID, PluginID: instance.PluginID, Operation: "install", Version: version, ExpectedRevision: 0, Config: cloneConfig(instance.Config), Resources: resources}
	journalInstance := instance
	journalInstance.Revision = 1
	journalInstance.Version = version.Version
	journalInstance.SourceVersion = version.ID
	journalInstance.Service.PluginID = instance.PluginID
	journalInstance.Service.InstanceID = instance.InstanceID
	journalInstance.Service.ConfigPath = instance.Paths.ConfigDir
	journalInstance.Service.DataPath = instance.Paths.DataDir
	journalInstance.Service.Ownership = "managed"
	journal := installJournal{Version: 1, ID: instance.ID, Phase: "prepared", Plan: plan, Instance: journalInstance, Nonce: strings.Repeat("b", 32), IdempotencySHA: strings.Repeat("a", 64), CreatedAt: time.Now().UTC()}
	if err = e.saveInstallJournal(context.Background(), &journal, 0); err != nil {
		t.Fatal("could not make a valid protected unfinished install fixture", err)
	}
	if _, err = runPHPMaintenance(t, e, context.Background(), PHPMaintenanceInput{ID: instance.ID, Command: "session_cleanup", ExpectedRevision: instance.Revision, PreviewID: previewID}); core.ErrorCode(err) != "operation_recovery_required" {
		t.Fatalf("cleanup bypassed the unfinished install guard: %v", err)
	}
	if body, readErr := os.ReadFile(path); readErr != nil || string(body) != "preserve-during-recovery" {
		t.Fatalf("recovery-blocked cleanup changed session: %q, %v", body, readErr)
	}
}

func TestPHPMaintenancePreviewCapsDeleteAndDirectoryScan(t *testing.T) {
	e, instance := phpMaintenanceFixture(t)
	old := time.Now().Add(-48 * time.Hour)
	for i := 0; i <= phpMaintenanceMaxCleanup; i++ {
		name := fmt.Sprintf("sess_bulk%04d", i)
		writePHPSession(t, e, instance, name, old, "x")
	}
	if _, err := runPHPMaintenance(t, e, context.Background(), PHPMaintenanceInput{ID: instance.ID, Command: "session_preview", ExpectedRevision: instance.Revision, OlderThanSeconds: 3600}); core.ErrorCode(err) != "session_cleanup_limit" {
		t.Fatalf("preview exceeded its bounded open-file/deletion count: %v", err)
	}
}
