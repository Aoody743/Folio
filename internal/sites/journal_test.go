package sites

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

type websiteJournalTestRuntime struct {
	web            WebRuntime
	php            PHPRuntime
	recovered      []string
	recoveryErrors []error
	running        bool
}

type websiteJournalRuntimeMarker struct{}

func (r *websiteJournalTestRuntime) Web(_ context.Context, id string) (WebRuntime, error) {
	if id != r.web.ID {
		return WebRuntime{}, errors.New("unexpected Web runtime ID")
	}
	return r.web, nil
}

func (r *websiteJournalTestRuntime) PHP(_ context.Context, id string) (PHPRuntime, error) {
	if id != r.php.ID {
		return PHPRuntime{}, errors.New("unexpected PHP runtime ID")
	}
	return r.php, nil
}

func (r *websiteJournalTestRuntime) Check(context.Context, string) error { return nil }
func (r *websiteJournalTestRuntime) Reload(context.Context, string) error {
	r.running = true
	return nil
}

func (r *websiteJournalTestRuntime) WithRuntimeTransaction(ctx context.Context, ids []string, callback func(context.Context) error) error {
	marker := new(int)
	return callback(context.WithValue(ctx, websiteJournalRuntimeMarker{}, marker))
}

func (r *websiteJournalTestRuntime) RecoverRuntime(ctx context.Context, id string) error {
	if ctx.Value(websiteJournalRuntimeMarker{}) == nil {
		err := errors.New("runtime lock context marker was lost during recovery")
		r.recoveryErrors = append(r.recoveryErrors, err)
		return err
	}
	if ctx.Err() != nil {
		r.recoveryErrors = append(r.recoveryErrors, ctx.Err())
		return ctx.Err()
	}
	r.recovered = append(r.recovered, id)
	// A stopped runtime remains stopped. This fake models RecoverRuntime's
	// production contract and records if a test accidentally uses Reload.
	return nil
}

func journalTestRuntime(base string) *websiteJournalTestRuntime {
	webDir := filepath.Join(base, "web", "snippets")
	phpDir := filepath.Join(base, "php", "pools")
	return &websiteJournalTestRuntime{
		web: WebRuntime{ID: "web-fixture", SnippetsDir: webDir, Prefix: filepath.Dir(webDir)},
		php: PHPRuntime{ID: "php-fixture", PoolDir: phpDir, Prefix: filepath.Dir(phpDir)},
	}
}

func initializeJournalTestState(t *testing.T, base string) (AgentConfig, Site, *websiteJournalTestRuntime) {
	t.Helper()
	config := AgentConfig{Version: 1, RootDir: filepath.Join(base, "content"), StateDir: filepath.Join(base, "state")}
	for _, path := range []string{
		config.RootDir,
		config.StateDir,
		filepath.Join(config.StateDir, "sites"),
		filepath.Join(config.StateDir, "retained"),
		filepath.Join(config.StateDir, "certificates"),
		filepath.Join(config.StateDir, "workers"),
		filepath.Join(config.StateDir, "transactions"),
		filepath.Join(config.StateDir, "backups", "site-fixture"),
		filepath.Join(config.RootDir, "site-fixture", "public"),
		filepath.Join(config.RootDir, "site-fixture", "auth"),
		filepath.Join(base, "web", "snippets"),
		filepath.Join(base, "php", "pools"),
	} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	site := Site{ID: "site-fixture", Name: "Fixture", WebInstance: "web-fixture", Status: "active", Revision: 4, DocumentRoot: filepath.Join(config.RootDir, "site-fixture", "public")}
	if err := os.WriteFile(filepath.Join(config.StateDir, "sites", site.ID+".json"), encode(site), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(journalTestRuntime(base).web.SnippetsDir, site.ID+".conf"), []byte("before web config\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.RootDir, site.ID, "auth", "htpasswd"), []byte("before password hash\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.RootDir, site.ID, "public", "index.html"), []byte("old content\n"), 0640); err != nil {
		t.Fatal(err)
	}
	return config, site, journalTestRuntime(base)
}

func openJournalTestExecutor(t *testing.T, base string) (*AgentExecutor, *websiteJournalTestRuntime) {
	t.Helper()
	config := AgentConfig{Version: 1, RootDir: filepath.Join(base, "content"), StateDir: filepath.Join(base, "state")}
	runtime := journalTestRuntime(base)
	state, err := os.OpenRoot(config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.OpenRoot(config.RootDir)
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	e := &AgentExecutor{config: config, runtimes: runtime, state: state, content: content}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Errorf("close journal test executor: %v", err)
		}
	})
	return e, runtime
}

func TestWebsiteJournalCrashChild(t *testing.T) {
	base := os.Getenv("POWERLINUX_JOURNAL_CHILD_BASE")
	if base == "" {
		t.Skip("subprocess helper")
	}
	e, _ := openJournalTestExecutor(t, base)
	_, _, err := e.beginWebsiteJournal(context.Background(), "site_config_set", request{ID: "site-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	changed := map[string][]byte{
		filepath.Join(e.config.RootDir, "site-fixture", "auth", "htpasswd"):          []byte("after password hash\n"),
		filepath.Join(e.config.StateDir, "sites", "site-fixture.json"):               []byte(`{"id":"corrupt"}`),
		filepath.Join(journalTestRuntime(base).web.SnippetsDir, "site-fixture.conf"): []byte("after web config\n"),
	}
	for path, body := range changed {
		if err = os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(e.config.StateDir, "retained", "site-fixture.json"), []byte("created after begin"), 0600); err != nil {
		t.Fatal(err)
	}
	writeJournalBarrier(t, filepath.Join(base, "ready"))
	select {}
}

func TestWebsiteJournalRestoreSwapCrashChild(t *testing.T) {
	base := os.Getenv("POWERLINUX_JOURNAL_SWAP_CHILD_BASE")
	if base == "" {
		t.Skip("subprocess helper")
	}
	phase := os.Getenv("POWERLINUX_JOURNAL_SWAP_CHILD_PHASE")
	e, _ := openJournalTestExecutor(t, base)
	ctx, _, err := e.beginWebsiteJournal(context.Background(), "site_backup_restore", request{ID: "site-fixture", BackupID: "backup-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	stage, err := e.recordRestoreStagePath(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.content.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err = e.syncContentParent(stage); err != nil {
		t.Fatal(err)
	}
	if err = e.recordRestoreStage(ctx, "site-fixture", "backup-fixture", stage); err != nil {
		t.Fatal(err)
	}
	root, err := e.content.OpenRoot(stage)
	if err != nil {
		t.Fatal(err)
	}
	output, err := root.OpenFile("index.html", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
	if err != nil {
		root.Close()
		t.Fatal(err)
	}
	_, writeErr := output.Write([]byte("replacement content\n"))
	syncErr := output.Sync()
	closeErr := output.Close()
	rootCloseErr := root.Close()
	if err = errors.Join(writeErr, syncErr, closeErr, rootCloseErr); err != nil {
		t.Fatal(err)
	}
	if err = e.recordRestoreSwap(ctx, "site-fixture"); err != nil {
		t.Fatal(err)
	}
	active := ctx.Value(websiteJournalKey{}).(*activeWebsiteJournal)
	public, previous, _ := websiteRestorePaths(*active.record.Swap, active.record.ID)
	if err = e.content.Rename(public, previous); err != nil {
		t.Fatal(err)
	}
	if err = e.syncContentSite("site-fixture"); err != nil {
		t.Fatal(err)
	}
	if phase == "first_rename" {
		writeJournalBarrier(t, filepath.Join(base, "ready"))
		select {}
	}
	if err = e.content.Rename(stage, public); err != nil {
		t.Fatal(err)
	}
	if err = e.syncContentSite("site-fixture"); err != nil {
		t.Fatal(err)
	}
	writeJournalBarrier(t, filepath.Join(base, "ready"))
	select {}
}

func TestWebsiteJournalSubprocessSIGKILLRestoresBytesOwnershipAndModes(t *testing.T) {
	runWebsiteJournalSubprocessSIGKILL(t, false)
}

func TestWebsiteJournalRootOwnedSubprocessSIGKILL(t *testing.T) {
	qaOwnedRootVM(t)
	runWebsiteJournalSubprocessSIGKILL(t, true)
}

func TestWebsiteJournalPendingIdentityCrashChild(t *testing.T) {
	base := os.Getenv("POWERLINUX_JOURNAL_PENDING_CHILD_BASE")
	if base == "" {
		t.Skip("subprocess helper")
	}
	e, _ := openJournalTestExecutor(t, base)
	e.config.Programs = map[string]string{
		"useradd": os.Getenv("POWERLINUX_JOURNAL_PENDING_USERADD"),
		"userdel": "/usr/sbin/userdel",
	}
	site := qaRenderSite()
	site.ID = os.Getenv("POWERLINUX_JOURNAL_PENDING_SITE")
	site.WebInstance = "web-fixture"
	site.DocumentRoot = filepath.Join(e.config.RootDir, site.ID, "public")
	ctx, _, err := e.beginWebsiteJournal(context.Background(), "site_apply", request{ID: site.ID, Site: &site})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.identity(ctx, site.ID); err != nil {
		t.Fatal(err)
	}
	t.Fatal("useradd wrapper returned before the parent killed this subprocess")
}

func TestQARootPendingIdentitySIGKILLRecoveryAndUnmarkedRefusal(t *testing.T) {
	qaOwnedRootVM(t)
	t.Run("marked account is removed after useradd crash", func(t *testing.T) {
		base := t.TempDir()
		config, _, runtime := initializeJournalTestState(t, base)
		site := qaPendingIdentitySite(t)
		name := "plsite_" + siteTag(site.ID)
		if _, err := user.Lookup(name); err == nil {
			t.Fatalf("refusing to use existing website account %q", name)
		}
		defer cleanupQAWorker(t, name)

		wrapper := filepath.Join(base, "useradd-after-account")
		ready := filepath.Join(base, "useradd-ready")
		pidFile := filepath.Join(base, "useradd-pid")
		script := "#!/bin/sh\n/usr/sbin/useradd \"$@\"\nstatus=$?\n[ \"$status\" -eq 0 ] || exit \"$status\"\nprintf '%s\\n' \"$$\" > '" + pidFile + "'\n: > '" + ready + "'\nexec /bin/sleep 300\n"
		if err := os.WriteFile(wrapper, []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
		config.Programs = map[string]string{"useradd": wrapper, "userdel": "/usr/sbin/userdel"}
		cmd := exec.Command(os.Args[0], "-test.run=^TestWebsiteJournalPendingIdentityCrashChild$")
		cmd.Env = append(os.Environ(),
			"POWERLINUX_JOURNAL_PENDING_CHILD_BASE="+base,
			"POWERLINUX_JOURNAL_PENDING_USERADD="+wrapper,
			"POWERLINUX_JOURNAL_PENDING_SITE="+site.ID,
		)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var wrapperPID int
		childAlive := true
		t.Cleanup(func() {
			if childAlive && cmd.Process != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
			if wrapperPID > 1 {
				_ = syscall.Kill(wrapperPID, syscall.SIGKILL)
			}
		})
		waitJournalBarrier(t, ready)
		pidBytes, err := os.ReadFile(pidFile)
		if err != nil {
			t.Fatal(err)
		}
		wrapperPID, err = strconv.Atoi(strings.TrimSpace(string(pidBytes)))
		if err != nil || wrapperPID <= 1 {
			t.Fatalf("invalid blocked useradd wrapper PID %q: %v", pidBytes, err)
		}
		if err = cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		if err = cmd.Wait(); err == nil {
			t.Fatal("pending identity child survived SIGKILL")
		}
		childAlive = false
		if err = syscall.Kill(wrapperPID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Fatal("could not stop the blocked useradd wrapper", err)
		}
		account, err := user.Lookup(name)
		if err != nil {
			t.Fatalf("expected account to exist at the useradd crash boundary: %v", err)
		}
		entries, err := os.ReadDir(filepath.Join(config.StateDir, "transactions"))
		if err != nil || len(entries) != 1 {
			t.Fatal("crashed identity transaction was not durable", err, entries)
		}
		journalBytes, err := os.ReadFile(filepath.Join(config.StateDir, "transactions", entries[0].Name()))
		if err != nil {
			t.Fatal(err)
		}
		var pending websiteJournal
		if err = json.Unmarshal(journalBytes, &pending); err != nil || pending.PendingWorker == nil || account.Name != pending.PendingWorker.Marker {
			t.Fatal("useradd crash did not preserve the exact transaction marker", err)
		}
		restarted, err := NewAgentExecutor(config, runtime)
		if err != nil {
			t.Fatal("startup did not recover the marked pending identity", err)
		}
		defer restarted.Close()
		if _, err = user.Lookup(name); err == nil {
			t.Fatal("startup recovery kept the uncommitted marked account")
		} else if !isUnknownUserError(err) {
			t.Fatal("could not verify pending account removal", err)
		}
		if entries, err := os.ReadDir(filepath.Join(config.StateDir, "transactions")); err != nil || len(entries) != 0 {
			t.Fatal("successful identity recovery did not clear its journal", err, entries)
		}
	})

	t.Run("unmarked account is never adopted or deleted", func(t *testing.T) {
		base := t.TempDir()
		config, _, runtime := initializeJournalTestState(t, base)
		config.Programs = map[string]string{"useradd": "/usr/sbin/useradd", "userdel": "/usr/sbin/userdel"}
		site := qaPendingIdentitySite(t)
		name := "plsite_" + siteTag(site.ID)
		if _, err := user.Lookup(name); err == nil {
			t.Fatalf("refusing to use existing website account %q", name)
		}
		defer cleanupQAWorker(t, name)
		e, err := NewAgentExecutor(config, runtime)
		if err != nil {
			t.Fatal(err)
		}
		ctx, _, err := e.beginWebsiteJournal(context.Background(), "site_apply", request{ID: site.ID, Site: &site})
		if err != nil {
			e.Close()
			t.Fatal(err)
		}
		marker, err := e.recordPendingWorker(ctx, site.ID, name)
		if err != nil {
			e.Close()
			t.Fatal(err)
		}
		if err = exec.Command("/usr/sbin/useradd", "--system", "--user-group", "--no-create-home", "--home-dir", filepath.Join(config.RootDir, site.ID, "public"), "--shell", "/usr/sbin/nologin", "--comment", "unmarked-website-account", name).Run(); err != nil {
			e.Close()
			t.Fatal("could not create controlled unmarked account", err)
		}
		if err = e.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err = NewAgentExecutor(config, runtime); err == nil || !strings.Contains(err.Error(), "unique transaction marker") {
			t.Fatal("startup adopted or removed an account without the transaction marker")
		}
		account, lookupErr := user.Lookup(name)
		if lookupErr != nil || account.Name == marker {
			t.Fatal("unmarked account was removed or changed during refusal", lookupErr)
		}
		if _, err = os.Stat(filepath.Join(config.StateDir, "workers", site.ID+".json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("recovery adopted an unmarked account into an owned receipt", err)
		}
	})
}

func qaPendingIdentitySite(t *testing.T) Site {
	t.Helper()
	var entropy [10]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		t.Fatal(err)
	}
	site := qaRenderSite()
	site.ID = "qa-" + hex.EncodeToString(entropy[:])
	site.WebInstance = "web-fixture"
	site.DocumentRoot = filepath.Join("/var/lib/powerlinux/sites", site.ID, "public")
	return site
}

func cleanupQAWorker(t *testing.T, name string) {
	t.Helper()
	if _, err := user.Lookup(name); err != nil {
		return
	}
	if output, err := exec.Command("/usr/sbin/userdel", name).CombinedOutput(); err != nil {
		t.Errorf("owned QA worker cleanup failed: %v (%d diagnostic bytes)", err, len(output))
	}
}

func runWebsiteJournalSubprocessSIGKILL(t *testing.T, requireRootOwned bool) {
	t.Helper()
	if requireRootOwned && os.Geteuid() != 0 {
		t.Fatal("root-owned journal subprocess requires root")
	}
	base := t.TempDir()
	config, site, _ := initializeJournalTestState(t, base)
	beforeSite, err := os.ReadFile(filepath.Join(config.StateDir, "sites", site.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join(config.RootDir, site.ID, "auth", "htpasswd"),
		filepath.Join(config.StateDir, "sites", site.ID+".json"),
		filepath.Join(journalTestRuntime(base).web.SnippetsDir, site.ID+".conf"),
	}
	before := map[string]fileIdentity{}
	for _, path := range paths {
		before[path], err = inspectJournalFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if requireRootOwned && (before[path].uid != 0 || before[path].gid != 0) {
			t.Fatalf("root-owned recovery fixture has unexpected ownership for %s: %d:%d", path, before[path].uid, before[path].gid)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestWebsiteJournalCrashChild$")
	cmd.Env = append(os.Environ(), "POWERLINUX_JOURNAL_CHILD_BASE="+base)
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	waitJournalBarrier(t, filepath.Join(base, "ready"))
	if err = cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err == nil {
		t.Fatal("SIGKILLed journal subprocess returned success")
	}
	cmd.Process = nil

	e, runtime := openJournalTestExecutor(t, base)
	if err = e.recoverWebsiteJournals(context.Background()); err != nil {
		t.Fatal("restart recovery failed", err)
	}
	if runtime.running || len(runtime.recovered) != 1 || runtime.recovered[0] != site.WebInstance {
		t.Fatal("recovery lost the runtime lock marker or started a stopped service", runtime.running, runtime.recovered)
	}
	for _, path := range paths {
		actual, inspectErr := inspectJournalFile(path)
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		if actual != before[path] {
			t.Fatalf("recovery did not restore exact bytes, mode, UID and GID for %s", path)
		}
	}
	if restored, err := os.ReadFile(filepath.Join(config.StateDir, "sites", site.ID+".json")); err != nil || string(restored) != string(beforeSite) {
		t.Fatal("site metadata before-image changed", err)
	}
	if _, err = os.Stat(filepath.Join(config.StateDir, "retained", site.ID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("recovery retained a file absent from the before-image", err)
	}
	if entries, err := os.ReadDir(filepath.Join(config.StateDir, "transactions")); err != nil || len(entries) != 0 {
		t.Fatal("recovery did not remove the completed journal", err)
	}
}

func TestWebsiteJournalCommitMarkerRecoversByCleanupOnly(t *testing.T) {
	base := t.TempDir()
	_, _, runtime := initializeJournalTestState(t, base)
	e, _ := openJournalTestExecutor(t, base)
	ctx, _, err := e.beginWebsiteJournal(context.Background(), "site_config_set", request{ID: "site-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(runtime.web.SnippetsDir, "site-fixture.conf")
	if err = os.WriteFile(configPath, []byte("committed generation\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = commitWebsiteJournal(ctx); err != nil {
		t.Fatal(err)
	}
	restarted, _ := openJournalTestExecutor(t, base)
	if err = restarted.recoverWebsiteJournals(context.Background()); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(configPath); err != nil || string(data) != "committed generation\n" {
		t.Fatal("committed journal rolled back its published generation", err)
	}
}

func TestWebsiteJournalRollbackContinuesAfterBeforeImageFailure(t *testing.T) {
	base := t.TempDir()
	config, site, _ := initializeJournalTestState(t, base)
	e, runtime := openJournalTestExecutor(t, base)
	var operationErr error
	lockedTransactionErr := runtime.WithRuntimeTransaction(context.Background(), []string{site.WebInstance}, func(transactionCtx context.Context) error {
		operationCtx, cancel := context.WithCancel(transactionCtx)
		locked, finish, err := e.beginWebsiteJournal(operationCtx, "site_config_set", request{ID: site.ID})
		if err != nil {
			cancel()
			return err
		}
		if err = os.WriteFile(filepath.Join(config.RootDir, site.ID, "auth", "htpasswd"), []byte("new password hash\n"), 0600); err != nil {
			cancel()
			return err
		}
		if err = os.WriteFile(filepath.Join(runtime.web.SnippetsDir, site.ID+".conf"), []byte("new web config\n"), 0600); err != nil {
			cancel()
			return err
		}
		receipt := filepath.Join(config.StateDir, "sites", site.ID+".json")
		if err = os.Remove(receipt); err != nil {
			cancel()
			return err
		}
		if err = os.Mkdir(receipt, 0700); err != nil {
			cancel()
			return err
		}
		cancel()
		operationErr = errors.New("simulated late metadata failure")
		if active := locked.Value(websiteJournalKey{}).(*activeWebsiteJournal); len(active.record.RuntimeIDs) != 1 {
			t.Fatalf("test fixture expected one protected runtime, got %v", active.record.RuntimeIDs)
		}
		finish(&operationErr)
		return nil
	})
	if lockedTransactionErr != nil {
		t.Fatal(lockedTransactionErr)
	}
	if operationErr == nil || !strings.Contains(operationErr.Error(), "recovery journal is retained") {
		t.Fatal("failed before-image was not reported with retained recovery state", operationErr)
	}
	for path, want := range map[string]string{
		filepath.Join(config.RootDir, site.ID, "auth", "htpasswd"): "before password hash\n",
		filepath.Join(runtime.web.SnippetsDir, site.ID+".conf"):    "before web config\n",
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("rollback stopped before restoring %s: %q (%v)", path, got, err)
		}
	}
	if len(runtime.recovered) != 1 || runtime.running {
		t.Fatal("rollback did not revalidate the runtime under its original marker", runtime.recovered, runtime.running, runtime.recoveryErrors, operationErr)
	}
	entries, err := os.ReadDir(filepath.Join(config.StateDir, "transactions"))
	if err != nil || len(entries) != 1 {
		t.Fatal("failed rollback discarded its durable journal", err, entries)
	}
	receipt := filepath.Join(config.StateDir, "sites", site.ID+".json")
	if err = os.Remove(receipt); err != nil {
		t.Fatal(err)
	}
	if err = e.recoverWebsiteJournals(context.Background()); err != nil {
		t.Fatal("restart retry did not finish retained recovery", err)
	}
	if restored, err := os.ReadFile(receipt); err != nil || string(restored) != string(encode(site)) {
		t.Fatal("retained recovery did not restore the final metadata before-image", err)
	}
	if len(runtime.recovered) != 2 {
		t.Fatal("restart retry skipped runtime recovery", runtime.recovered)
	}
}

func TestWebsiteJournalRollbackRestoresDirectoryMetadataAndEmptyCreations(t *testing.T) {
	base := t.TempDir()
	config, site, _ := initializeJournalTestState(t, base)
	e, runtime := openJournalTestExecutor(t, base)
	directories := []string{
		filepath.Join(config.RootDir, site.ID),
		filepath.Join(config.RootDir, site.ID, "public"),
		filepath.Join(config.RootDir, site.ID, "auth"),
	}
	before := make(map[string]fileIdentity, len(directories))
	for _, path := range directories {
		before[path] = inspectJournalDirectory(t, path)
	}
	var operationErr error
	transactionErr := runtime.WithRuntimeTransaction(context.Background(), []string{site.WebInstance}, func(transactionCtx context.Context) error {
		_, finish, err := e.beginWebsiteJournal(transactionCtx, "site_apply", request{ID: site.ID, Site: &site})
		if err != nil {
			return err
		}
		for _, path := range directories {
			if err = os.Chmod(path, 0775); err != nil {
				return err
			}
		}
		created := filepath.Join(config.RootDir, site.ID, "logs")
		if err = os.Mkdir(created, 0700); err != nil {
			return err
		}
		operationErr = errors.New("simulated website apply failure")
		finish(&operationErr)
		return nil
	})
	if transactionErr != nil {
		t.Fatal(transactionErr)
	}
	if operationErr == nil || strings.Contains(operationErr.Error(), "recovery journal is retained") {
		t.Fatal("directory rollback did not complete cleanly", operationErr)
	}
	for _, path := range directories {
		if actual := inspectJournalDirectory(t, path); actual.mode != before[path].mode || actual.uid != before[path].uid || actual.gid != before[path].gid {
			t.Fatalf("rollback changed directory metadata for %s: got %#v, want %#v", path, actual, before[path])
		}
	}
	if _, err := os.Lstat(filepath.Join(config.RootDir, site.ID, "logs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rollback kept an empty directory created by the failed apply", err)
	}
}

func inspectJournalDirectory(t *testing.T, path string) fileIdentity {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		t.Fatal("directory identity is unavailable", path)
	}
	return fileIdentity{mode: info.Mode().Perm(), uid: stat.Uid, gid: stat.Gid}
}

func TestWebsiteJournalRestoreSwapSubprocessSIGKILLRollsBackBothRenameWindows(t *testing.T) {
	for _, phase := range []string{"first_rename", "second_rename"} {
		t.Run(phase, func(t *testing.T) {
			base := t.TempDir()
			config, site, _ := initializeJournalTestState(t, base)
			manifest := backupManifest{Version: 1, Site: site, Backup: Backup{ID: "backup-fixture", SiteID: site.ID}}
			if err := os.WriteFile(filepath.Join(config.StateDir, "backups", site.ID, "backup-fixture.json"), encode(manifest), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestWebsiteJournalRestoreSwapCrashChild$")
			cmd.Env = append(os.Environ(), "POWERLINUX_JOURNAL_SWAP_CHILD_BASE="+base, "POWERLINUX_JOURNAL_SWAP_CHILD_PHASE="+phase)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			waitJournalBarrier(t, filepath.Join(base, "ready"))
			if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err == nil {
				t.Fatal("SIGKILLed restore subprocess returned success")
			}
			cmd.Process = nil
			e, runtime := openJournalTestExecutor(t, base)
			if err := e.recoverWebsiteJournals(context.Background()); err != nil {
				t.Fatal("restore swap restart recovery failed", err)
			}
			public := filepath.Join(config.RootDir, site.ID, "public")
			got, err := os.ReadFile(filepath.Join(public, "index.html"))
			if err != nil || string(got) != "old content\n" {
				t.Fatal("restore crash did not recover the exact old public tree", string(got), err)
			}
			entries, err := os.ReadDir(filepath.Join(config.RootDir, site.ID))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".restore-") {
					t.Fatal("restore recovery left a staged or previous directory", entry.Name())
				}
			}
			if runtime.running || len(runtime.recovered) != 1 || runtime.recovered[0] != site.WebInstance {
				t.Fatal("restore recovery lost runtime lock context or started a stopped runtime", runtime.running, runtime.recovered)
			}
		})
	}
}

func TestWebsiteStateLockRejectsSecondProducer(t *testing.T) {
	base := t.TempDir()
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	first, err := acquireWebsiteStateLock(root)
	if err != nil {
		t.Fatal(err)
	}
	secondRoot, err := os.OpenRoot(base)
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	defer secondRoot.Close()
	if second, lockErr := acquireWebsiteStateLock(secondRoot); lockErr == nil {
		second.Close()
		first.Close()
		t.Fatal("second Agent producer acquired the same website state")
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := acquireWebsiteStateLock(secondRoot)
	if err != nil {
		t.Fatal("state lock was not released after owner exit", err)
	}
	if err = third.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWebsiteJournalRejectsUnstampedNonemptyNewContent(t *testing.T) {
	base := t.TempDir()
	config, _, _ := initializeJournalTestState(t, base)
	e, _ := openJournalTestExecutor(t, base)
	content := &journalContent{ID: "site-unowned"}
	if err := e.content.MkdirAll(content.ID, 0750); err != nil {
		t.Fatal(err)
	}
	if err := e.content.WriteFile(filepath.Join(content.ID, "foreign.txt"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.removeNewContent(context.Background(), content); err == nil {
		t.Fatal("recovery recursively deleted unstamped content")
	}
	if data, err := os.ReadFile(filepath.Join(config.RootDir, content.ID, "foreign.txt")); err != nil || string(data) != "preserve" {
		t.Fatal("recovery changed unstamped content", err)
	}
}

type fileIdentity struct {
	body string
	mode os.FileMode
	uid  uint32
	gid  uint32
}

func inspectJournalFile(path string) (fileIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return fileIdentity{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return fileIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, errors.New("journal test filesystem has no UID/GID metadata")
	}
	return fileIdentity{body: string(data), mode: info.Mode().Perm(), uid: stat.Uid, gid: stat.Gid}, nil
}

func writeJournalBarrier(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	err = errors.Join(file.Sync(), file.Close())
	if err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	err = errors.Join(parent.Sync(), parent.Close())
	if err != nil {
		t.Fatal(err)
	}
}

func waitJournalBarrier(t *testing.T, path string) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-deadline.C:
			t.Fatal("subprocess did not reach the journal fault barrier")
		case <-tick.C:
		}
	}
}

func TestWebsiteJournalRecordRemainsBounded(t *testing.T) {
	base := t.TempDir()
	initializeJournalTestState(t, base)
	e, _ := openJournalTestExecutor(t, base)
	j := websiteJournal{Version: 1, ID: "t-" + strings.Repeat("a", 32), Operation: "site_config_set", Files: []journalFile{{Kind: "state", Path: "sites/site-fixture.json", Existed: true, Before: []byte(strings.Repeat("x", maxWebsiteJournalBytes))}}}
	if err := e.storeJournal(j); err == nil || !strings.Contains(fmt.Sprint(err), "bounded protected recovery budget") {
		t.Fatal("oversized WAL record was accepted", err)
	}
}

func TestWebsiteJournalDecodeIsStrict(t *testing.T) {
	base := t.TempDir()
	initializeJournalTestState(t, base)
	e, _ := openJournalTestExecutor(t, base)
	var decoded websiteJournal
	if err := e.rootJSONStrictForJournalTest([]byte(`{"version":1,"unexpected":true}`), &decoded); err == nil {
		t.Fatal("unknown journal fields were accepted")
	}
}

func TestSnapshotConfigRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := snapshotConfig(path)
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("FIFO was accepted as a protected configuration file")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("configuration snapshot blocked while opening a FIFO")
	}
}

func (e *AgentExecutor) rootJSONStrictForJournalTest(data []byte, out any) error {
	path := filepath.Join(e.config.StateDir, "transactions", "strict.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	return rootJSON(e.state, filepath.Join("transactions", "strict.json"), out)
}
