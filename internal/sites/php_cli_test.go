package sites

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPHPCLISanitizationKeepsSettingsAndDropsPrivatePaths(t *testing.T) {
	raw := []byte("[PHP]\nmemory_limit=48M\nmax_execution_time=30\ndate.timezone=Asia/Shanghai\ndisable_functions=exec,system\nextension=redis.so\nzend_extension=opcache.so\nsession.save_path=/root/private/sessions\nerror_log=/var/lib/powerlinux-agent/private.log\nauto_prepend_file=/root/private.php\ninclude_path=/root/private\nsmtp.password=PRIVATE_VALUE\n")
	ini, modules, err := sanitizePHPCLIIni(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range []string{"memory_limit=48M", "date.timezone=\"Asia/Shanghai\"", "extension=redis.so", "zend_extension=opcache.so", "disable_functions=exec,system"} {
		if !strings.Contains(string(ini), setting) {
			t.Fatal("Safe CLI setting was omitted", setting)
		}
	}
	for _, private := range []string{"/root/", "/var/lib/", "PRIVATE_VALUE", "auto_prepend_file", "include_path", "error_log"} {
		if strings.Contains(string(ini), private) {
			t.Fatal("Protected configuration leaked to public CLI ini", private)
		}
	}
	if len(modules) != 2 {
		t.Fatal("Selected extensions were not retained", modules)
	}
	for _, invalid := range []string{"extension=/root/private.so", "extension=../../secret.so", "extension=redis.so\nextension=redis.so", "memory_limit=1M; auto_prepend_file=/secret", "date.timezone=Unknown/Timezone"} {
		if _, _, err := sanitizePHPCLIIni([]byte(invalid)); err == nil {
			t.Fatal("Unsafe CLI setting accepted", invalid)
		}
	}
}
func TestPHPCLIWrapperQuotesTrustedPathsAndPreservesArguments(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "php ' dollar$ back` tick")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	selection := filepath.Join(dir, "ini ' $ ` space")
	wrapper := filepath.Join(dir, "wrapper")
	if err := os.WriteFile(wrapper, phpCLIWrapper(program, selection), 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "should-not-exist")
	argument := "$(touch " + marker + ") ; `id` ' literal"
	output, err := exec.Command(wrapper, argument).Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	if len(lines) != 3 || lines[0] != "-c" || lines[1] != filepath.Join(selection, "php.ini") || lines[2] != argument {
		t.Fatal("CLI argv was modified by shell quoting", string(output))
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("CLI argument executed as shell code")
	}
}

type phpCLITestRuntimes struct{ runtime PHPRuntime }

func (r *phpCLITestRuntimes) Web(context.Context, string) (WebRuntime, error) {
	return WebRuntime{}, invalid("Not a web fixture")
}
func (r *phpCLITestRuntimes) PHP(_ context.Context, id string) (PHPRuntime, error) {
	if id != r.runtime.ID {
		return PHPRuntime{}, invalid("Unknown fixture")
	}
	return r.runtime, nil
}
func (r *phpCLITestRuntimes) Check(context.Context, string) error  { return nil }
func (r *phpCLITestRuntimes) Reload(context.Context, string) error { return nil }

func phpCLIRootFixture(t *testing.T) (*AgentExecutor, *phpCLITestRuntimes, string) {
	t.Helper()
	qaOwnedRootVM(t)
	program := os.Getenv("POWERLINUX_QA_PHP_CLI")
	if program == "" {
		t.Skip("Actual PHP CLI fixture is required; set POWERLINUX_QA_PHP_CLI in the disposable VM")
	}
	if err := trustedProgram(program); err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp("/tmp", "powerlinux-qa-php-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err = os.Chmod(base, 0755); err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(base, "runtime")
	config := filepath.Join(prefix, "private-config")
	if err = os.MkdirAll(config, 0700); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(config, "php.ini")
	if err = os.WriteFile(private, []byte("memory_limit=48M\nsession.save_path=/root/private/sessions\nsmtp.password=KEEP_PRIVATE\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runtimes := &phpCLITestRuntimes{PHPRuntime{ID: "owned-php", Prefix: prefix, CLIProgram: program, CLIConfig: private}}
	e, err := NewAgentExecutor(AgentConfig{Version: 1, RootDir: filepath.Join(base, "content"), StateDir: filepath.Join(base, "state")}, runtimes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e, runtimes, base
}
func ordinaryPHPCLI(t *testing.T, program string, args ...string) []byte {
	t.Helper()
	account, err := user.Lookup("nobody")
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := strconv.ParseUint(account.Uid, 10, 32)
	gid, _ := strconv.ParseUint(account.Gid, 10, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, program, args...)
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), NoSetGroups: false}}
	command.Env = []string{"PATH=/usr/bin:/bin", "PHPRC=/root/untrusted.php.ini", "PHP_INI_SCAN_DIR=/root/untrusted-scan"}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatal("Ordinary UID CLI invocation failed", err, string(output))
	}
	return output
}
func TestQAPHPCLIPublicSelectionActuallyRunsAsOrdinaryUID(t *testing.T) {
	e, runtimes, base := phpCLIRootFixture(t)
	result, err := e.phpCLISet(context.Background(), "owned-php")
	if err != nil {
		t.Fatal(err)
	}
	data := result.(map[string]any)
	if data["available"] != true {
		t.Fatal("CLI selection was not verified", data)
	}
	wrapper := data["wrapper_path"].(string)
	version := ordinaryPHPCLI(t, wrapper, "-v")
	if !strings.Contains(string(version), "PHP ") {
		t.Fatal("Actual PHP CLI version missing", string(version))
	}
	output := ordinaryPHPCLI(t, wrapper, "-r", `echo json_encode(["limit"=>ini_get("memory_limit"),"ini"=>php_ini_loaded_file(),"uid"=>function_exists("posix_geteuid")?posix_geteuid():null]);`)
	var observed struct {
		Limit, Ini string
		UID        *int
	}
	if json.Unmarshal(output, &observed) != nil || observed.Limit != "48M" || !strings.HasPrefix(observed.Ini, filepath.Join(e.config.RootDir, ".cli", "selections")) {
		t.Fatal("Public CLI settings were not actually loaded", string(output))
	}
	if observed.UID != nil && *observed.UID == 0 {
		t.Fatal("Wrapper elevated ordinary user")
	}
	ini, err := os.ReadFile(observed.Ini)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ini), "KEEP_PRIVATE") || strings.Contains(string(ini), "/root/") {
		t.Fatal("Public CLI config exposed private values")
	}
	for path, mode := range map[string]os.FileMode{runtimes.runtime.CLIConfig: 0600, filepath.Dir(runtimes.runtime.CLIConfig): 0700, e.config.StateDir: 0700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatal("Private permissions changed", path, err)
		}
	}
	old, err := os.Readlink(filepath.Join(e.config.RootDir, ".cli", "current"))
	if err != nil {
		t.Fatal(err)
	}
	runtimes.runtime.CLIProgram = "/usr/bin/true"
	if _, err = e.phpCLISet(context.Background(), "owned-php"); err == nil {
		t.Fatal("Non-PHP executable accepted")
	}
	runtimes.runtime.CLIProgram = data["program"].(string)
	current, _ := os.Readlink(filepath.Join(e.config.RootDir, ".cli", "current"))
	if current != old {
		t.Fatal("Failed CLI switch replaced working selector")
	}
	get, err := e.phpCLIGet(context.Background())
	if err != nil || get.(map[string]any)["available"] != true {
		t.Fatal("Old selection stopped working after failed switch", get, err)
	}
	marker := filepath.Join(base, "argv-must-not-run")
	literal := "$(touch " + marker + ")"
	if output := ordinaryPHPCLI(t, wrapper, "-r", `echo $argv[1];`, "--", literal); string(output) != literal {
		t.Fatal("CLI arguments changed", string(output))
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("Wrapper evaluated PHP argument as shell")
	}
}
func TestQAPHPCLIRejectsForeignDirectoryAndEntryPoint(t *testing.T) {
	e, _, _ := phpCLIRootFixture(t)
	directory := filepath.Join(e.config.RootDir, ".cli")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(directory, "powerlinux-php")
	if err := os.WriteFile(foreign, []byte("FOREIGN"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.phpCLISet(context.Background(), "owned-php"); err == nil {
		t.Fatal("Unowned directory was adopted")
	}
	data, _ := os.ReadFile(foreign)
	if string(data) != "FOREIGN" {
		t.Fatal("Unowned CLI entry point was overwritten")
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	if _, err := e.phpCLISet(context.Background(), "owned-php"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("REPLACED"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.phpCLISet(context.Background(), "owned-php"); err == nil {
		t.Fatal("Replaced managed wrapper was silently overwritten")
	}
	data, _ = os.ReadFile(foreign)
	if string(data) != "REPLACED" {
		t.Fatal("Foreign replacement was not preserved")
	}
}

func TestQAPHPCLINativeBinaryAccessKeepsSiblingStatePrivate(t *testing.T) {
	e, runtimes, base := phpCLIRootFixture(t)
	actual, err := os.ReadFile(runtimes.runtime.CLIProgram)
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(base, "native-software", "php", "private-instance")
	bin := filepath.Join(prefix, "runtime", "bin")
	if err = os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(bin, "php")
	if err = os.WriteFile(program, actual, 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(prefix, "config")
	data := filepath.Join(prefix, "data")
	for _, directory := range []string{config, data} {
		if err = os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	ini := filepath.Join(config, "php.ini")
	if err = os.WriteFile(ini, []byte("memory_limit=64M\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runtimes.runtime.Prefix = prefix
	runtimes.runtime.CLIProgram = program
	runtimes.runtime.CLIConfig = ini
	result, err := e.phpCLISet(context.Background(), "owned-php")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := result.(map[string]any)["wrapper_path"].(string)
	if out := ordinaryPHPCLI(t, wrapper, "-r", `echo ini_get("memory_limit");`); string(out) != "64M" {
		t.Fatal("Native CLI did not use public config", string(out))
	}
	for _, directory := range []string{config, data} {
		info, err := os.Stat(directory)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal("Private sibling permissions changed", directory, err)
		}
	}
	info, err := os.Stat(program)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("Exact native CLI executable was not made publicly callable", err)
	}
}

func TestQAPHPCLIExtensionStateUsesItsOwnPublicConfiguration(t *testing.T) {
	e, runtimes, _ := phpCLIRootFixture(t)
	probe, err := phpCLIExec(context.Background(), runtimes.runtime.CLIProgram, []string{"-n", "-r", phpCLIProbeCode}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(probe.ExtensionDir, "opcache.so")); err != nil {
		t.Fatal("Signed actual PHP opcache fixture is required", err)
	}
	if err = os.WriteFile(runtimes.runtime.CLIConfig, []byte("memory_limit=48M\nzend_extension=opcache.so\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := e.phpCLISet(context.Background(), "owned-php")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := result.(map[string]any)["wrapper_path"].(string)
	if out := ordinaryPHPCLI(t, wrapper, "-m"); !strings.Contains(strings.ToLower(string(out)), "zend opcache") {
		t.Fatal("Selected extension did not actually load", string(out))
	}
	if err = os.WriteFile(runtimes.runtime.CLIConfig, []byte("memory_limit=48M\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = e.phpCLISet(context.Background(), "owned-php"); err != nil {
		t.Fatal(err)
	}
	if out := ordinaryPHPCLI(t, wrapper, "-m"); strings.Contains(strings.ToLower(string(out)), "opcache") {
		t.Fatal("Distribution scan directory overrode disabled managed extension", string(out))
	}
	previous, _ := os.Readlink(filepath.Join(e.config.RootDir, ".cli", "current"))
	if err = os.WriteFile(runtimes.runtime.CLIConfig, []byte("extension=uninstalled_fixture.so\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = e.phpCLISet(context.Background(), "owned-php"); err == nil {
		t.Fatal("Missing module accepted as a successful CLI selection")
	}
	current, _ := os.Readlink(filepath.Join(e.config.RootDir, ".cli", "current"))
	if previous != current {
		t.Fatal("Missing extension destroyed prior working CLI selection")
	}
}
