package sites

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type sitePHPActionFixture struct {
	e       *AgentExecutor
	runtime *qaRuntimeFixture
	site    Site
	worker  Identity
}

func newSitePHPActionFixture(t *testing.T, slowlog bool) *sitePHPActionFixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("site PHP maintenance exercises root-owned Agent receipts and worker-owned session files")
	}
	account, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("test requires the unprivileged nobody account")
	}
	uid, err := strconvUint32(account.Uid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconvUint32(account.Gid)
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	phpPrefix := filepath.Join(base, "php")
	poolDir := filepath.Join(phpPrefix, "pool.d")
	configDir := filepath.Join(phpPrefix, "conf.d")
	for _, path := range []string{poolDir, configDir} {
		if err = os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	cli := filepath.Join(phpPrefix, "php-cli")
	writeSitePHPFixtureCLI(t, cli, "files")
	mainConfig := filepath.Join(phpPrefix, "php-fpm.conf")
	cliConfig := filepath.Join(phpPrefix, "php.ini")
	if err = os.WriteFile(mainConfig, []byte("[global]\ninclude = "+filepath.Join(poolDir, "*.conf")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cliConfig, []byte("session.save_handler=files\n"), 0600); err != nil {
		t.Fatal(err)
	}
	php := PHPRuntime{ID: "owned-php", Program: "/usr/bin/true", MainConfig: mainConfig, PoolDir: poolDir, Prefix: phpPrefix, CLIProgram: cli, CLIConfig: cliConfig}
	runtime := &qaRuntimeFixture{php: php}
	rootDir, stateDir := filepath.Join(base, "content"), filepath.Join(base, "state")
	e, err := NewAgentExecutor(AgentConfig{Version: 1, RootDir: rootDir, StateDir: stateDir}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })

	site := qaRenderSite()
	site.ID = "php-qa-site"
	site.PHPInstance = php.ID
	site.Revision = 7
	site.PHP.SlowlogEnabled = slowlog
	if slowlog {
		site.PHP.SlowlogTimeoutSeconds = 5
	}
	worker := Identity{User: account.Username, Group: group.Name, UID: uid, GID: gid}
	if err = atomicRoot(e.state, filepath.Join("workers", site.ID+".json"), encode(worker), 0600); err != nil {
		t.Fatal(err)
	}
	if err = atomicRoot(e.state, filepath.Join("sites", site.ID+".json"), encode(site), 0600); err != nil {
		t.Fatal(err)
	}
	paths := e.paths(site)
	for path, mode := range map[string]os.FileMode{site.ID: 0751, filepath.Join(site.ID, "tmp"): 0700, filepath.Join(site.ID, "logs"): 0750} {
		if err = os.MkdirAll(filepath.Join(rootDir, path), mode); err != nil {
			t.Fatal(err)
		}
		if err = os.Chmod(filepath.Join(rootDir, path), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.Chown(filepath.Join(rootDir, site.ID), 0, 0); err != nil {
		t.Fatal(err)
	}
	if err = os.Chown(filepath.Join(rootDir, site.ID, "logs"), 0, 0); err != nil {
		t.Fatal(err)
	}
	if err = os.Chown(filepath.Join(rootDir, site.ID, "tmp"), int(uid), int(gid)); err != nil {
		t.Fatal(err)
	}
	pool, err := renderPool(site, paths, worker, WebRuntime{WorkerUser: "nobody", WorkerGroup: group.Name})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(poolDir, site.ID+".conf"), []byte(pool), 0600); err != nil {
		t.Fatal(err)
	}
	return &sitePHPActionFixture{e: e, runtime: runtime, site: site, worker: worker}
}

func strconvUint32(value string) (uint32, error) {
	n, err := strconv.ParseUint(value, 10, 32)
	return uint32(n), err
}

func writeSitePHPFixtureCLI(t *testing.T, path, handler string) {
	t.Helper()
	body := "#!/bin/sh\nprintf '%s' '" + handler + "'\n"
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, 0, 0); err != nil {
		t.Fatal(err)
	}
}

func (f *sitePHPActionFixture) run(operation string, in request) (agent.Result, error) {
	if in.Actor.ID == "" {
		in.Actor = core.Actor{ID: "site-php-test-actor", Kind: "human", Name: "Site PHP test"}
	}
	return f.e.Run(context.Background(), agent.Request{ActorID: in.Actor.ID, Operation: operation, Input: encode(in)})
}

func (f *sitePHPActionFixture) addSession(t *testing.T, name, value string, modified time.Time) string {
	t.Helper()
	path := filepath.Join(f.e.config.RootDir, f.site.ID, "tmp", name)
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, int(f.worker.UID), int(f.worker.GID)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSitePHPSessionStatsPreviewCleanupIgnoresUnrelatedTmpFiles(t *testing.T) {
	f := newSitePHPActionFixture(t, true)
	oldPath := f.addSession(t, "sess_expired", "old", time.Now().Add(-48*time.Hour))
	freshPath := f.addSession(t, "sess_current", "fresh", time.Now())
	artifact := filepath.Join(f.e.config.RootDir, f.site.ID, "tmp", "upload_123")
	if err := os.WriteFile(artifact, []byte("not a PHP session"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.e.config.RootDir, f.site.ID, "logs", "php-slow.log"), []byte("slow one\nslow two\nslow three\n"), 0600); err != nil {
		t.Fatal(err)
	}

	statsResult, err := f.run("site_php_session_stats", request{ID: f.site.ID, OlderThanSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	var stats struct {
		SiteID       string   `json:"site_id"`
		SiteRevision int64    `json:"site_revision"`
		PHPInstance  string   `json:"php_instance"`
		Total        int      `json:"total_sessions"`
		Expired      int      `json:"expired_sessions"`
		AllowActions []string `json:"allow_actions"`
	}
	if err = json.Unmarshal(statsResult.Data, &stats); err != nil || stats.SiteID != f.site.ID || stats.SiteRevision != f.site.Revision || stats.PHPInstance != f.site.PHPInstance || stats.Total != 2 || stats.Expired != 1 || len(stats.AllowActions) != 1 || stats.AllowActions[0] != "preview" {
		t.Fatalf("unexpected site session stats: %+v, err=%v", stats, err)
	}

	previewResult, err := f.run("site_php_session_preview", request{ID: f.site.ID, ExpectedRevision: f.site.Revision, OlderThanSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	var preview struct {
		SiteID         string   `json:"site_id"`
		SiteRevision   int64    `json:"site_revision"`
		PHPInstance    string   `json:"php_instance"`
		PreviewID      string   `json:"preview_id"`
		CandidateCount int      `json:"candidate_count"`
		CandidateBytes int64    `json:"candidate_bytes"`
		AllowActions   []string `json:"allow_actions"`
	}
	if err = json.Unmarshal(previewResult.Data, &preview); err != nil || preview.SiteID != f.site.ID || preview.SiteRevision != f.site.Revision || preview.PHPInstance != f.site.PHPInstance || !sitePHPPreviewID.MatchString(preview.PreviewID) || preview.CandidateCount != 1 || preview.CandidateBytes != int64(len("old")) || len(preview.AllowActions) != 1 || preview.AllowActions[0] != "cleanup" {
		t.Fatalf("unexpected site session preview: %+v, err=%v", preview, err)
	}

	logResult, err := f.run("site_php_slowlog_read", request{ID: f.site.ID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	var log struct {
		Configured bool     `json:"configured"`
		Generated  bool     `json:"generated"`
		Lines      []string `json:"lines"`
		Truncated  bool     `json:"truncated"`
	}
	if err = json.Unmarshal(logResult.Data, &log); err != nil || !log.Configured || !log.Generated || log.Truncated || strings.Join(log.Lines, "|") != "slow two|slow three" {
		t.Fatalf("unexpected bounded site slowlog: %+v, err=%v", log, err)
	}

	cleanupResult, err := f.run("site_php_session_cleanup", request{ID: f.site.ID, ExpectedRevision: f.site.Revision, PreviewID: preview.PreviewID})
	if err != nil {
		t.Fatal(err)
	}
	var cleaned sitePHPSessionCleanupResult
	if err = json.Unmarshal(cleanupResult.Data, &cleaned); err != nil || cleaned.DeletedCount != 1 || cleaned.DeletedBytes != int64(len("old")) || cleaned.SkippedMissingCount != 0 || cleaned.SiteRevision != f.site.Revision {
		t.Fatalf("unexpected cleanup result: %+v, err=%v", cleaned, err)
	}
	if _, err = os.Lstat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("captured stale session remains: %v", err)
	}
	for path, expected := range map[string]string{freshPath: "fresh", artifact: "not a PHP session"} {
		actual, readErr := os.ReadFile(path)
		if readErr != nil || string(actual) != expected {
			t.Fatalf("unrelated or fresh file changed: %s (%v)", path, readErr)
		}
	}
	// The stored completion receipt makes a retry with the same preview safe
	// even though the original session path has already been removed.
	retry, err := f.run("site_php_session_cleanup", request{ID: f.site.ID, ExpectedRevision: f.site.Revision, PreviewID: preview.PreviewID})
	if err != nil {
		t.Fatal(err)
	}
	var retried sitePHPSessionCleanupResult
	if err = json.Unmarshal(retry.Data, &retried); err != nil || retried != cleaned {
		t.Fatalf("cleanup retry was not idempotent: %+v, err=%v", retried, err)
	}
}

func TestSitePHPSessionMaintenanceRejectsNonFilesHandlerAndStaleReplacement(t *testing.T) {
	f := newSitePHPActionFixture(t, false)
	file := f.addSession(t, "sess_candidate", "original", time.Now().Add(-48*time.Hour))
	writeSitePHPFixtureCLI(t, f.runtime.php.CLIProgram, "redis")
	if _, err := f.run("site_php_session_stats", request{ID: f.site.ID, OlderThanSeconds: 3600}); core.ErrorCode(err) != "unsupported_session_handler" {
		t.Fatalf("non-files effective handler should block inspection, got %v", err)
	}
	writeSitePHPFixtureCLI(t, f.runtime.php.CLIProgram, "files")

	previewResult, err := f.run("site_php_session_preview", request{ID: f.site.ID, ExpectedRevision: f.site.Revision, OlderThanSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	var preview struct {
		PreviewID string `json:"preview_id"`
	}
	if err = json.Unmarshal(previewResult.Data, &preview); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chown(file, int(f.worker.UID), int(f.worker.GID)); err != nil {
		t.Fatal(err)
	}
	_, err = f.run("site_php_session_cleanup", request{ID: f.site.ID, ExpectedRevision: f.site.Revision, PreviewID: preview.PreviewID})
	if core.ErrorCode(err) != "preview_stale" {
		t.Fatalf("changed previewed file should block cleanup, got %v", err)
	}
	actual, err := os.ReadFile(file)
	if err != nil || string(actual) != "replacement" {
		t.Fatalf("replacement session was changed: %q (%v)", actual, err)
	}
}

func TestSitePHPScanDirectoryRejectsSymlinkedIni(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected PHP scan directory validation requires root-owned test paths")
	}
	base := t.TempDir()
	scan := filepath.Join(base, "conf.d")
	if err := os.Mkdir(scan, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "outside.ini")
	if err := os.WriteFile(target, []byte("session.save_handler=redis\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(scan, "10-untrusted.ini")); err != nil {
		t.Fatal(err)
	}
	if err := verifySitePHPScanDir(scan); err == nil {
		t.Fatalf("symlinked INI fragment should be rejected, got %v", err)
	}
}

func TestSitePHPGeneratedPoolBindingAndExplicitHandlerSemantics(t *testing.T) {
	site := qaRenderSite()
	site.ID = "php-qa-site"
	site.PHPInstance = "owned-php"
	site.PHP.SlowlogEnabled = true
	site.PHP.SlowlogTimeoutSeconds = 12
	worker := Identity{User: "site-worker", Group: "site-worker", UID: 1000, GID: 1000}
	paths := qaRenderPaths()
	web := WebRuntime{WorkerUser: "web-worker", WorkerGroup: "web-worker"}
	body, err := renderPool(site, paths, worker, web)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := parseSitePool([]byte(body), site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifySitePoolIdentityAndSessionPath(pool, site, worker, paths); err != nil {
		t.Fatal("generated pool did not bind to the exact site session path:", err)
	}
	if err = verifySitePoolSlowlog(site, pool, paths); err != nil {
		t.Fatal("generated pool did not bind the typed slowlog settings:", err)
	}

	withInclude := body + "include = /etc/php/pool.d/*.conf\n"
	foreign, err := parseSitePool([]byte(withInclude), site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifySitePoolIdentityAndSessionPath(foreign, site, worker, paths); core.ErrorCode(err) != "php_pool_invalid" {
		t.Fatalf("pool include outside the generated website config should be rejected, got %v", err)
	}

	for _, handler := range []string{"files", "redis"} {
		site.PHP.Values = map[string]string{"session.save_handler": handler}
		body, err = renderPool(site, paths, worker, web)
		if err != nil {
			t.Fatal(err)
		}
		pool, err = parseSitePool([]byte(body), site.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err = verifySitePoolIdentityAndSessionPath(pool, site, worker, paths); err != nil {
			t.Fatal("explicit session handler did not match protected site settings:", err)
		}
		err = (&AgentExecutor{}).verifySiteSessionHandler(context.Background(), site, PHPRuntime{}, pool)
		if handler == "files" && err != nil {
			t.Fatalf("explicit files handler should avoid a global probe: %v", err)
		}
		if handler == "redis" && core.ErrorCode(err) != "unsupported_session_handler" {
			t.Fatalf("explicit non-files handler should remain configured but block file cleanup, got %v", err)
		}
	}
}

func TestSitePHPSlowlogRejectsSymlinkedLog(t *testing.T) {
	f := newSitePHPActionFixture(t, true)
	logs := filepath.Join(f.e.config.RootDir, f.site.ID, "logs")
	outside := filepath.Join(t.TempDir(), "outside.log")
	if err := os.WriteFile(outside, []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(logs, "php-slow.log")); err != nil {
		t.Fatal(err)
	}
	_, err := f.run("site_php_slowlog_read", request{ID: f.site.ID})
	if core.ErrorCode(err) != "php_slowlog_unsafe" {
		t.Fatalf("symlinked slowlog should be rejected, got %v", err)
	}
	if _, err = os.Stat(outside); err != nil {
		t.Fatal(err)
	}
}
