package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

const phpSessionHelperPathEnv = "POWERLINUX_PHP_SESSION_HELPER_PATH"
const phpSessionHelperDeniedEnv = "POWERLINUX_PHP_SESSION_HELPER_DENIED"

// This test is run by the parent test process as the configured worker UID.
func TestPHPWorkerSessionPathHelper(t *testing.T) {
	path := os.Getenv(phpSessionHelperPathEnv)
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.Getenv(phpSessionHelperDeniedEnv) == "1" {
		if !errors.Is(err, syscall.EACCES) {
			t.Fatalf("expected traversal denial, got %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString("worker-owned session payload"); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
}

func newPHPWorkerHelperBinary(t *testing.T) string {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.CreateTemp("/tmp", "powerlinux-php-worker-helper-*")
	if err != nil {
		t.Fatal(err)
	}
	name := out.Name()
	t.Cleanup(func() { _ = os.Remove(name) })
	if _, err = io.Copy(out, in); err == nil {
		err = out.Chmod(0755)
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func runPHPWorkerSessionHelper(t *testing.T, binary string, uid, gid uint32, path string, denied bool) error {
	t.Helper()
	command := exec.Command(binary, "-test.run=^TestPHPWorkerSessionPathHelper$")
	command.Env = append(os.Environ(), phpSessionHelperPathEnv+"="+path)
	if denied {
		command.Env = append(command.Env, phpSessionHelperDeniedEnv+"=1")
	}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: []uint32{gid}}}
	return command.Run()
}

func requirePHPWorkerDirectory(t *testing.T, path string, uid, gid uint32, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != mode || stat.Uid != uid || stat.Gid != gid {
		t.Fatalf("unexpected managed directory metadata for %s: mode=%#o owner=%v", path, info.Mode().Perm(), stat)
	}
}

func newLegacyPHPInstance(t *testing.T) (*PrivilegedExecutor, Instance, string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root to set up a distinct managed PHP worker UID")
	}
	worker, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("requires an unprivileged nobody worker account")
	}
	base, err := os.MkdirTemp("/tmp", "powerlinux-php-legacy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err = os.Chmod(base, 0711); err != nil {
		t.Fatal(err)
	}
	e, err := NewPrivilegedExecutor(PrivilegedConfig{
		RootDir: filepath.Join(base, "software"), StateDir: filepath.Join(base, "agent-state"),
		RunUser: worker.Username, APIUser: "root",
	})
	if err != nil {
		t.Fatal(err)
	}
	paths := e.instancePaths("php-apt", "legacy")
	instance := Instance{
		ID: "php-apt-legacy", PluginID: "php-apt", InstanceID: "legacy", Version: "8.4", SourceVersion: "8.4",
		Revision: 7, Status: "running", Ownership: "managed", Paths: paths, Config: defaultConfig("php-apt"),
		Service: ServiceProfile{ID: "php-apt-legacy", PluginID: "php-apt", InstanceID: "legacy", Unit: "powerlinux-plugin-php-apt-legacy.service", Program: "/bin/true", ConfigPath: paths.ConfigDir, DataPath: paths.DataDir, Kind: "daemon", Ownership: "managed"},
	}
	if err = e.workerDirs(instance); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(paths.DataDir, "sessions")
	if err = os.Mkdir(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Chown(sessions, int(e.uid), int(e.gid)); err != nil {
		t.Fatal(err)
	}
	if err = e.save("receipts", instance.ID, instance); err != nil {
		t.Fatal(err)
	}
	// Recreate the legacy root:worker mode that blocks traversal even though
	// the PHP data and session directories already belong to the worker.
	for _, path := range []string{filepath.Dir(paths.Root), paths.Root} {
		if err = os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	privateMarker := filepath.Join(e.config.StateDir, "private", "php-legacy-test-marker")
	if err = os.WriteFile(privateMarker, []byte("agent-private-state"), 0600); err != nil {
		t.Fatal(err)
	}
	return e, instance, privateMarker
}

func runPHPAction(t *testing.T, e *PrivilegedExecutor, input PHPInput) (Instance, error) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		return Instance{}, err
	}
	result, err := e.Run(context.Background(), agent.Request{Operation: "php_action", Input: raw})
	if err != nil {
		return Instance{}, err
	}
	if result.ExitCode != 0 {
		return Instance{}, errors.New("PHP Agent action returned a nonzero exit code")
	}
	var instance Instance
	err = json.Unmarshal([]byte(result.Stdout), &instance)
	return instance, err
}

func TestPHPConfigureRepairsLegacyTraversalForActualWorker(t *testing.T) {
	e, instance, privateMarker := newLegacyPHPInstance(t)
	helper := newPHPWorkerHelperBinary(t)
	sessions := filepath.Join(instance.Paths.DataDir, "sessions")
	if err := runPHPWorkerSessionHelper(t, helper, e.uid, e.gid, filepath.Join(sessions, "sess_before_reconfigure"), true); err != nil {
		t.Fatalf("worker did not fail on the simulated legacy path: %v", err)
	}

	changed, err := runPHPAction(t, e, PHPInput{ID: instance.ID, Command: "configure", ExpectedRevision: instance.Revision, Values: map[string]any{}})
	if err != nil {
		t.Fatal("revision-guarded PHP configure could not repair the legacy path", err)
	}
	if changed.Revision != instance.Revision+1 {
		t.Fatalf("configure did not advance the managed receipt revision: %d", changed.Revision)
	}
	for _, path := range []string{filepath.Dir(instance.Paths.Root), instance.Paths.Root} {
		requirePHPWorkerDirectory(t, path, 0, e.gid, 0750)
	}
	requirePHPWorkerDirectory(t, instance.Paths.DataDir, e.uid, e.gid, 0700)
	requirePHPWorkerDirectory(t, sessions, e.uid, e.gid, 0700)
	requirePHPWorkerDirectory(t, instance.Paths.BackupDir, 0, e.gid, 0700)
	requirePHPWorkerDirectory(t, filepath.Join(e.config.StateDir, "private"), 0, 0, 0700)
	if err := runPHPWorkerSessionHelper(t, helper, e.uid, e.gid, filepath.Join(sessions, "sess_after_reconfigure"), false); err != nil {
		t.Fatalf("worker could not create a session-path file after the explicit configure action: %v", err)
	}
	privateInfo, err := os.Stat(privateMarker)
	privateBody, readErr := os.ReadFile(privateMarker)
	if err != nil || readErr != nil || string(privateBody) != "agent-private-state" || privateInfo.Mode().Perm() != 0600 || privateInfo.Sys().(*syscall.Stat_t).Uid != 0 {
		t.Fatal("PHP directory repair changed Agent-private state", err, readErr)
	}

	invalid, _ := json.Marshal(PHPInput{ID: instance.ID, Command: "configure", ExpectedRevision: changed.Revision, Values: map[string]any{"session_path": "/tmp"}})
	if _, err = e.Run(context.Background(), agent.Request{Operation: "php_action", Input: invalid}); core.ErrorCode(err) != "invalid_config" {
		t.Fatalf("invalid session path was not rejected by the existing PHP guard: %v", err)
	}
	stale, err := runPHPAction(t, e, PHPInput{ID: instance.ID, Command: "configure", ExpectedRevision: instance.Revision, Values: map[string]any{}})
	if core.ErrorCode(err) != "revision_conflict" || stale.ID != "" {
		t.Fatalf("stale PHP configuration was not rejected before mutation: result=%#v err=%v", stale, err)
	}
	got, err := e.get(context.Background(), instance.ID, false)
	if err != nil || got.Revision != changed.Revision {
		t.Fatalf("rejected configure changed the owned PHP revision: %#v %v", got, err)
	}
}

func TestQANativePHPConfigureRestoresWorkerSessionAccess(t *testing.T) {
	if os.Getenv("POWERLINUX_QA_OWNED_VM") != "1" || os.Getenv("POWERLINUX_QA_NATIVE_MANAGED") != "1" || os.Geteuid() != 0 {
		t.Skip("requires explicit owned VM managed-PHP mutation window")
	}
	var policy struct {
		Plugins PrivilegedConfig `json:"plugins"`
	}
	body, err := os.ReadFile("/etc/powerlinux/agent-policy.json")
	if err != nil || json.Unmarshal(body, &policy) != nil {
		t.Fatal("actual installed Agent policy is unavailable")
	}
	e, err := NewPrivilegedExecutor(policy.Plugins)
	if err != nil {
		t.Fatal("could not load owned PHP runtime policy", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	php, err := e.get(ctx, "php-apt-nativeqa", false)
	if err != nil || php.Status != "running" || !isPHPRuntime(php.PluginID) {
		t.Fatal("owned PHP fixture is unavailable", err)
	}
	changed, err := runPHPAction(t, e, PHPInput{ID: php.ID, Command: "configure", ExpectedRevision: php.Revision, Values: cloneConfig(php.Config)})
	if err != nil || changed.Revision != php.Revision+1 {
		t.Fatal("same-values PHP configure did not migrate the current owned revision", changed.Revision, err)
	}
	if health := e.probe(ctx, changed); health.Status != "healthy" {
		t.Fatal("FPM did not answer its owned FastCGI probe after the configure task", health.Reason)
	}
	cli, err := phpCLI(changed)
	if err != nil {
		t.Fatal(err)
	}
	code := `if (!session_start()) { fwrite(STDERR, "session_start_failed"); exit(7); } $_SESSION["powerlinux_qa"] = "session-path-repair"; if (!session_write_close()) { fwrite(STDERR, "session_write_failed"); exit(8); } echo session_id();`
	output, err := runFixedEnvironment(ctx, cli, []string{"-c", filepath.Join(changed.Paths.ConfigDir, "php.ini"), "-r", code}, 10*time.Second, e.phpCredential(), runtimeEnvironment(changed))
	if err != nil {
		t.Fatal("actual package PHP CLI could not create a worker-owned session", err)
	}
	sessionID := strings.TrimSpace(output)
	if !regexp.MustCompile(`^[A-Za-z0-9,-]{20,128}$`).MatchString(sessionID) {
		t.Fatalf("PHP did not return a valid session ID after writing: %q", sessionID)
	}
	path := filepath.Join(changed.Paths.DataDir, "sessions", "sess_"+sessionID)
	t.Cleanup(func() { _ = os.Remove(path) })
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal("PHP reported a written session but no managed session file exists", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Size() == 0 || stat.Uid != e.uid || stat.Gid != e.gid {
		t.Fatal("PHP session file is not a nonempty worker-owned regular file", info)
	}
	for _, ancestor := range []string{filepath.Dir(changed.Paths.Root), changed.Paths.Root} {
		requirePHPWorkerDirectory(t, ancestor, 0, e.gid, 0750)
	}
	t.Log("same-values revision-guarded PHP configure restored worker traversal; the installed package CLI wrote a worker-owned session and FPM remained healthy")
}
