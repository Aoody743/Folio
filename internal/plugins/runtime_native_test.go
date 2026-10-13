package plugins

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

// This opt-in test uses already product-installed, owned OpenResty/FPM
// instances. It tests the new root runtime helpers, not an immutable release
// upgrade or an installation recipe, which require separate API acceptance.
func TestQANativeManagedWebGenerationAndFPMProfile(t *testing.T) {
	if os.Getenv("POWERLINUX_QA_OWNED_VM") != "1" || os.Getenv("POWERLINUX_QA_NATIVE_MANAGED") != "1" || os.Geteuid() != 0 {
		t.Skip("requires explicit owned VM managed-runtime mutation window")
	}
	comm, err := os.ReadFile("/proc/1/comm")
	if err != nil || strings.TrimSpace(string(comm)) != "systemd" {
		t.Fatal("owned VM must run its real systemd")
	}
	var policy struct {
		Plugins PrivilegedConfig `json:"plugins"`
	}
	body, err := os.ReadFile("/etc/powerlinux/agent-policy.json")
	if err != nil || json.Unmarshal(body, &policy) != nil {
		t.Fatal("actual installed policy unavailable")
	}
	e, err := NewPrivilegedExecutor(policy.Plugins)
	if err != nil {
		t.Fatal("native owned profile initialization failed", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	php, err := e.get(ctx, "php-apt-nativeqa", false)
	if err != nil || php.Status != "running" || php.Revision != 1 {
		t.Fatal("owned PHP fixture or its retained revision changed", err)
	}
	if e.probe(ctx, php).Status != "healthy" {
		t.Fatal("actual FPM did not return its exact FastCGI ping and END_REQUEST")
	}
	cli, err := phpCLI(php)
	if err != nil {
		t.Fatal(err)
	}
	modules, err := runFixedEnvironment(ctx, cli, []string{"-c", filepath.Join(php.Paths.ConfigDir, "php.ini"), "-m"}, 10*time.Second, e.phpCredential(), runtimeEnvironment(php))
	if err != nil || !phpModuleSet(modules)["openssl"] {
		t.Fatal("ordinary runtime UID could not execute matching CLI with protected configuration", err)
	}
	for _, name := range []string{"php.ini", "php-fpm.conf", "pools/default.conf"} {
		info, err := os.Stat(filepath.Join(php.Paths.ConfigDir, name))
		if err != nil || info.Mode().Perm() != 0640 {
			t.Fatal("migrated owned PHP configuration lacks actual group-readable mode", name, err)
		}
	}
	t.Logf("actual managed PHP revision=%d: root FPM authenticated ownership, FastCGI ping completion, worker-UID CLI and explicit config modes passed", php.Revision)

	web, err := e.get(ctx, "openresty-nativeqa", false)
	if err != nil || web.Status != "running" {
		t.Fatal("owned OpenResty fixture unavailable", err)
	}
	if err := e.WithRuntimeTransaction(ctx, []string{web.ID}, func(held context.Context) error { return e.Reload(held, web.ID) }); err != nil {
		t.Fatal("actual native generation publication failed", err)
	}
	main := filepath.Join(web.Paths.ConfigDir, "nginx.conf")
	previous, err := os.ReadFile(main)
	if err != nil {
		t.Fatal(err)
	}
	previousGeneration, err := e.webGeneration(web, previous)
	if err != nil || previousGeneration == "" {
		t.Fatal("successful native reload did not publish its exact nonce", err)
	}
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	snippet := filepath.Join(web.Paths.ConfigDir, "sites", "powerlinux-qa-generation.conf")
	if _, err := os.Lstat(snippet); !os.IsNotExist(err) {
		t.Fatal("negative test refuses to overwrite an existing snippet")
	}
	t.Cleanup(func() {
		_ = os.Remove(snippet)
		recovery, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := e.Reload(recovery, web.ID); err != nil {
			t.Error("native test cleanup failed to publish recovered Web cycle", err)
		}
	})
	if err := writeAtomicRoot(filepath.Dir(snippet), filepath.Base(snippet), []byte("server { listen "+occupied.Addr().String()+"; server_name generation-qa.invalid; return 200 'blocked'; }\n"), 0600, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.Check(ctx, web.ID); err != nil {
		t.Fatal("occupied-listener fixture did not pass real nginx syntax validation", err)
	}
	// nginx -t succeeds and systemctl reload acknowledges its signal, but
	// bind() fails in the master. The old HTTP cycle must never count applied.
	if err := e.WithRuntimeTransaction(ctx, []string{web.ID}, func(held context.Context) error { return e.Reload(held, web.ID) }); core.ErrorCode(err) != "reload_not_applied" {
		t.Fatal("occupied listener was incorrectly acknowledged as applied", err)
	}
	actual, err := os.ReadFile(main)
	if err != nil || string(actual) != string(previous) {
		t.Fatal("negative reload failed to preserve the previous owned main profile", err)
	}
	if err := probeWebGeneration(ctx, webHealthAddress(web), previousGeneration); err != nil {
		t.Fatal("negative reload lost its actual healthy previous generation", err)
	}
	t.Log("actual nginx -t accepted occupied listener; old healthy HTTP cycle was rejected as a new generation, owned main was restored and previous cycle remained live")
}
