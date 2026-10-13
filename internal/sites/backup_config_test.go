package sites

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func newBackupConfigurationAgent(t *testing.T) (*AgentExecutor, string, Site) {
	t.Helper()
	return backupConfigurationAgentAt(t, t.TempDir())
}

func backupConfigurationAgentAt(t *testing.T, base string) (*AgentExecutor, string, Site) {
	t.Helper()
	config := AgentConfig{Version: 1, RootDir: filepath.Join(base, "content"), StateDir: filepath.Join(base, "state")}
	for _, path := range []string{
		config.RootDir,
		config.StateDir,
		filepath.Join(config.StateDir, "backups", "site-fixture"),
		filepath.Join(config.StateDir, "sites"),
		filepath.Join(config.RootDir, "site-fixture", "public"),
		filepath.Join(base, "web", "snippets"),
		filepath.Join(base, "php", "pools"),
	} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	state, err := os.OpenRoot(config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.OpenRoot(config.RootDir)
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	runtime := journalTestRuntime(base)
	e := &AgentExecutor{config: config, runtimes: runtime, state: state, content: content}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Errorf("close backup state root: %v", err)
		}
		if err := content.Close(); err != nil {
			t.Errorf("close backup content root: %v", err)
		}
	})
	site := Site{
		ID: "site-fixture", Name: "Fixture", WebInstance: runtime.web.ID, PHPInstance: runtime.php.ID,
		Status: "active", Revision: 7, ConfigurationMode: "advanced",
		DocumentRoot: filepath.Join(config.RootDir, "site-fixture", "public"),
		CreatedAt:    time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC),
	}
	if err := atomicRoot(state, filepath.Join("sites", site.ID+".json"), encode(site), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.RootDir, site.ID, "public", "index.html"), []byte("public page\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return e, base, site
}

func writeBackupRuntimeConfig(t *testing.T, base string, site Site, web, php []byte) {
	t.Helper()
	runtime := journalTestRuntime(base)
	for _, item := range []struct {
		path string
		body []byte
	}{{filepath.Join(runtime.web.SnippetsDir, site.ID+".conf"), web}, {filepath.Join(runtime.php.PoolDir, site.ID+".conf"), php}} {
		if err := os.WriteFile(item.path, item.body, 0640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(item.path, 0640); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWebsiteBackupAdvancedConfigurationSidecarIsPrivateAndRestorable(t *testing.T) {
	e, base, site := newBackupConfigurationAgent(t)
	webRaw := []byte("server { # private advanced web config\n}\n")
	phpRaw := []byte("[site-fixture]\n; private PHP pool config\n")
	writeBackupRuntimeConfig(t, base, site, webRaw, phpRaw)
	created, err := e.backup(context.Background(), request{ID: site.ID, ExpectedRevision: site.Revision})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := e.backupMetadata(request{ID: site.ID, BackupID: created.ID})
	if err != nil || metadata.Version != 2 || metadata.Configuration == nil {
		t.Fatal("advanced backup did not publish a versioned private configuration receipt", err)
	}
	restored, err := e.loadBackupConfiguration(context.Background(), metadata)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored.Web.Data, webRaw) || restored.Web.SourceMode != 0640 || restored.Web.SourceUID != uint32(os.Geteuid()) || !bytes.Equal(restored.PHP.Data, phpRaw) || restored.PHP.SourceMode != 0640 {
		t.Fatalf("private sidecar did not preserve exact Web/PHP bytes and mode metadata: web=%q mode=%04o uid=%d php=%q mode=%04o", restored.Web.Data, restored.Web.SourceMode, restored.Web.SourceUID, restored.PHP.Data, restored.PHP.SourceMode)
	}
	encodedMetadata := encode(metadata)
	if bytes.Contains(encodedMetadata, webRaw) || bytes.Contains(encodedMetadata, phpRaw) || bytes.Contains(encode(created), webRaw) || bytes.Contains(encode(created), phpRaw) {
		t.Fatal("raw runtime configuration leaked into backup metadata")
	}
	configDirInfo, err := os.Stat(filepath.Join(e.config.StateDir, "backups", site.ID, ".config"))
	if err != nil || configDirInfo.Mode().Perm()&0077 != 0 {
		t.Fatal("sidecar directory is not private", err)
	}
	sidecarInfo, err := os.Stat(filepath.Join(e.config.StateDir, backupConfigurationPath(site.ID, created.ID)))
	if err != nil || sidecarInfo.Mode().Perm() != 0600 {
		t.Fatal("raw configuration sidecar is not mode 0600", err)
	}
	listing, err := e.backups(site.ID)
	if err != nil || len(listing) != 1 || listing[0].ID != created.ID || bytes.Contains(encode(listing), webRaw) {
		t.Fatal("backup listing exposed sidecar data or hid the backup", err)
	}
	archive, err := e.state.OpenFile(filepath.Join("backups", site.ID, created.ID+".tar.gz"), os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(archive)
	if err != nil {
		archive.Close()
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		header, nextErr := tr.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		body, readErr := io.ReadAll(tr)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(header.Name, ".conf") || bytes.Contains(body, webRaw) || bytes.Contains(body, phpRaw) {
			t.Fatal("public tar archive included private runtime configuration")
		}
	}
	if err = errors.Join(gz.Close(), archive.Close()); err != nil {
		t.Fatal(err)
	}

	// Simulate a failed first deletion after the public tar has already been
	// removed. The protected manifest remains so retry can finish idempotently.
	sidecarPath := filepath.Join(e.config.StateDir, backupConfigurationPath(site.ID, created.ID))
	if err = os.Remove(sidecarPath); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(sidecarPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(sidecarPath, "blocker"), []byte("keep directory nonempty"), 0600); err != nil {
		t.Fatal(err)
	}
	_, deleteErr := e.deleteBackup(request{ID: site.ID, BackupID: created.ID})
	if core.ErrorCode(deleteErr) != "backup_cleanup_required" {
		t.Fatal("partial cleanup did not retain an actionable receipt", deleteErr)
	}
	if _, err = e.state.Lstat(filepath.Join("backups", site.ID, created.ID+".json")); err != nil {
		t.Fatal("partial cleanup removed its retry receipt", err)
	}
	if err = os.Remove(filepath.Join(sidecarPath, "blocker")); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(sidecarPath); err != nil {
		t.Fatal(err)
	}
	if _, err = e.deleteBackup(request{ID: site.ID, BackupID: created.ID}); err != nil {
		t.Fatal("cleanup retry did not finish", err)
	}
}

func TestWebsiteBackupConfigurationIntegrityAndReceiptBinding(t *testing.T) {
	e, base, site := newBackupConfigurationAgent(t)
	writeBackupRuntimeConfig(t, base, site, []byte("web private"), []byte("php private"))
	reference, err := e.storeBackupConfiguration(context.Background(), site, "b-fixture")
	if err != nil {
		t.Fatal(err)
	}
	backup := Backup{ID: "b-fixture", SiteID: site.ID}
	metadata := backupManifest{Version: 2, Site: site, Backup: backup, Configuration: reference}
	path := backupConfigurationPath(site.ID, backup.ID)
	raw, err := os.ReadFile(filepath.Join(e.config.StateDir, path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.loadBackupConfiguration(context.Background(), metadata); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(e.config.StateDir, path), append(raw, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = e.loadBackupConfiguration(context.Background(), metadata); core.ErrorCode(err) != "backup_configuration_invalid" {
		t.Fatal("sidecar bytes changed without detection", err)
	}
	if err = os.WriteFile(filepath.Join(e.config.StateDir, path), raw, 0600); err != nil {
		t.Fatal(err)
	}
	var configuration backupConfiguration
	if err = jsonUnmarshalStrictForBackupTest(raw, &configuration); err != nil {
		t.Fatal(err)
	}
	configuration.Web.RuntimeID = "web-other"
	forged := encode(configuration)
	sum := sha256.Sum256(forged)
	metadata.Configuration.Size = int64(len(forged))
	metadata.Configuration.SHA256 = hex.EncodeToString(sum[:])
	if err = atomicRoot(e.state, path, forged, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = e.loadBackupConfiguration(context.Background(), metadata); core.ErrorCode(err) != "backup_configuration_invalid" {
		t.Fatal("configuration bound to a different runtime receipt was accepted", err)
	}
}

func TestWebsiteBackupRestoreLegacyModeCompatibilityAndFailClosedAdvanced(t *testing.T) {
	e, _, site := newBackupConfigurationAgent(t)
	legacyManaged := backupManifest{Version: 1, Site: Site{ID: site.ID}, Backup: Backup{ID: "b-old", SiteID: site.ID}}
	if config, err := e.loadBackupConfiguration(context.Background(), legacyManaged); err != nil || config != nil {
		t.Fatal("version-one managed backup should continue using typed rendering", err)
	}
	legacyAdvanced := legacyManaged
	legacyAdvanced.Site.ConfigurationMode = "advanced"
	if _, err := e.loadBackupConfiguration(context.Background(), legacyAdvanced); core.ErrorCode(err) != "backup_configuration_missing" {
		t.Fatal("version-one advanced backup did not fail closed", err)
	}
	v2Advanced := legacyAdvanced
	v2Advanced.Version = 2
	if _, err := e.loadBackupConfiguration(context.Background(), v2Advanced); core.ErrorCode(err) != "backup_configuration_missing" {
		t.Fatal("version-two advanced backup without a sidecar was not rejected", err)
	}
}

func TestWebsiteBackupRestoreLegacyAdvancedRefusesBeforeCreatingStage(t *testing.T) {
	e, base, site := newBackupConfigurationAgent(t)
	writeBackupRuntimeConfig(t, base, site, []byte("advanced web"), []byte("advanced php"))
	backup, err := e.backup(context.Background(), request{ID: site.ID, ExpectedRevision: site.Revision})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := e.backupMetadata(request{ID: site.ID, BackupID: backup.ID})
	if err != nil {
		t.Fatal(err)
	}
	metadata.Version = 1
	metadata.Configuration = nil
	if err = atomicRoot(e.state, filepath.Join("backups", site.ID, backup.ID+".json"), encode(metadata), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(e.config.RootDir, site.ID, "public", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.restore(context.Background(), request{ID: site.ID, ExpectedRevision: site.Revision, BackupID: backup.ID}); core.ErrorCode(err) != "backup_configuration_missing" {
		t.Fatal("legacy advanced backup did not fail before swap", err)
	}
	after, err := os.ReadFile(filepath.Join(e.config.RootDir, site.ID, "public", "index.html"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected legacy restore changed public content", err)
	}
	entries, err := os.ReadDir(filepath.Join(e.config.RootDir, site.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".restore-") {
			t.Fatal("rejected legacy restore created a staging directory", entry.Name())
		}
	}
}

func TestWebsiteBackupRestoreRawBytesStayBoundToSiteAndRuntime(t *testing.T) {
	site := Site{ID: "site-fixture", WebInstance: "web-fixture", PHPInstance: "php-fixture"}
	configuration := &backupRestoreConfiguration{
		SiteID: site.ID, Mode: "advanced", WebRuntime: site.WebInstance, PHPRuntime: site.PHPInstance,
		Web: backupConfigurationFile{Data: []byte("web raw"), SourceMode: 0600, SourceUID: uint32(os.Geteuid())},
		PHP: &backupConfigurationFile{Data: []byte("php raw"), SourceMode: 0640, SourceUID: uint32(os.Geteuid())},
	}
	ctx := withBackupRestoreConfiguration(context.Background(), configuration)
	web, ok, err := backupRestoreBytes(ctx, site, "web", site.WebInstance)
	if err != nil || !ok || string(web.Data) != "web raw" || web.SourceMode != 0600 {
		t.Fatal("exact Web bytes or protected mode were not selected", err)
	}
	php, ok, err := backupRestoreBytes(ctx, site, "php", site.PHPInstance)
	if err != nil || !ok || string(php.Data) != "php raw" || php.SourceMode != 0640 {
		t.Fatal("exact PHP bytes or protected mode were not selected", err)
	}
	wrongSite := site
	wrongSite.ID = "site-other"
	if _, _, err = backupRestoreBytes(ctx, wrongSite, "web", site.WebInstance); core.ErrorCode(err) != "backup_configuration_invalid" {
		t.Fatal("raw bytes were accepted for another site", err)
	}
	if _, _, err = backupRestoreBytes(ctx, site, "php", "php-other"); core.ErrorCode(err) != "backup_configuration_invalid" {
		t.Fatal("raw bytes were accepted for another PHP runtime", err)
	}
	if _, ok, err = backupRestoreBytes(context.Background(), site, "web", site.WebInstance); err != nil || ok {
		t.Fatal("ordinary typed apply unexpectedly received raw backup bytes", err)
	}
}

func TestWebsiteBackupRestorePreservesSavedConfigurationPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.conf")
	if err := os.WriteFile(path, []byte("new raw config"), 0600); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("test filesystem has no UID/GID metadata")
	}
	configuration := backupConfigurationFile{SourceMode: 0640, SourceUID: owner.Uid, SourceGID: owner.Gid}
	if err = restoreBackupConfigurationMode(path, configuration); err != nil {
		t.Fatal(err)
	}
	restored, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	identity := restored.Sys().(*syscall.Stat_t)
	if restored.Mode().Perm() != 0640 || identity.Uid != owner.Uid || identity.Gid != owner.Gid {
		t.Fatalf("raw configuration restore lost saved permissions: mode=%04o owner=%d:%d", restored.Mode().Perm(), identity.Uid, identity.Gid)
	}
}

func TestWebsiteBackupConfigurationCaptureRejectsSpecialAndOversizedFiles(t *testing.T) {
	base := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(base, "fifo.conf"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureBackupConfigurationFile(filepath.Join(base, "fifo.conf"), "web", "web-fixture"); core.ErrorCode(err) != "backup_configuration_invalid" {
		t.Fatal("FIFO runtime configuration was not rejected promptly", err)
	}
	large := bytes.Repeat([]byte("x"), maxBackupConfigFile+1)
	largePath := filepath.Join(base, "large.conf")
	if err := os.WriteFile(largePath, large, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureBackupConfigurationFile(largePath, "web", "web-fixture"); core.ErrorCode(err) != "backup_configuration_invalid" {
		t.Fatal("oversized runtime configuration was not rejected", err)
	}
}

func TestQARootWebsiteAdvancedBackupRestoreRestoresExactWebAndPHPConfiguration(t *testing.T) {
	qaOwnedRootVM(t)
	e, runtime := qaRootExecutor(t)
	e.config.AllowAdvancedConfig = true
	site := rootOwnedTestSite(t)
	site.PHPInstance = runtime.php.ID
	created, err := qaAgentRun(context.Background(), e, "site_apply", request{ID: site.ID, Site: &site})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(created.Data, &site); err != nil {
		t.Fatal(err)
	}
	webRaw := []byte("server {\n    # exact advanced Web configuration\n}\n")
	advanced, err := qaAgentRun(context.Background(), e, "site_config_set", request{ID: site.ID, ExpectedRevision: site.Revision, Config: string(webRaw)})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(advanced.Data, &site); err != nil {
		t.Fatal(err)
	}
	phpRaw := []byte("[" + site.ID + "]\n; exact advanced PHP configuration\n")
	webPath := filepath.Join(runtime.web.SnippetsDir, site.ID+".conf")
	phpPath := filepath.Join(runtime.php.PoolDir, site.ID+".conf")
	if err = os.WriteFile(phpPath, phpRaw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{webPath, phpPath} {
		if err = os.Chmod(path, 0640); err != nil {
			t.Fatal(err)
		}
	}
	createdBackup, err := qaAgentRun(context.Background(), e, "site_backup_create", request{ID: site.ID, ExpectedRevision: site.Revision})
	if err != nil {
		t.Fatal(err)
	}
	var backup Backup
	if err = json.Unmarshal(createdBackup.Data, &backup); err != nil {
		t.Fatal(err)
	}
	metadata, err := e.backupMetadata(request{ID: site.ID, BackupID: backup.ID})
	if err != nil || metadata.Version != 2 || metadata.Configuration == nil {
		t.Fatal("advanced backup did not bind its private Web/PHP configuration", err)
	}
	type fileIdentity struct{ uid, gid uint32 }
	wantIdentity := map[string]fileIdentity{}
	for _, path := range []string{webPath, phpPath} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		stat := info.Sys().(*syscall.Stat_t)
		wantIdentity[path] = fileIdentity{uid: stat.Uid, gid: stat.Gid}
	}
	if err = os.WriteFile(webPath, []byte("changed Web configuration\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(phpPath, []byte("changed PHP configuration\n"), 0600); err != nil {
		t.Fatal(err)
	}
	checksBeforeRestore, reloadsBeforeRestore := runtime.checks, runtime.reloads
	restored, err := qaAgentRun(context.Background(), e, "site_backup_restore", request{ID: site.ID, ExpectedRevision: site.Revision, BackupID: backup.ID})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.checks != checksBeforeRestore+2 || runtime.reloads != reloadsBeforeRestore+2 {
		t.Fatalf("restore did not perform one Check and Reload for each runtime: checks=%d reloads=%d", runtime.checks-checksBeforeRestore, runtime.reloads-reloadsBeforeRestore)
	}
	var result struct {
		Site Site `json:"site"`
	}
	if err = json.Unmarshal(restored.Data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Site.ConfigurationMode != "advanced" {
		t.Fatalf("restore lost the saved configuration mode: %q", result.Site.ConfigurationMode)
	}
	for path, want := range map[string][]byte{webPath: webRaw, phpPath: phpRaw} {
		actual, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(actual, want) {
			t.Fatalf("restore did not recover exact runtime configuration at %s: %v", path, readErr)
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		stat := info.Sys().(*syscall.Stat_t)
		if info.Mode().Perm() != 0640 || stat.Uid != wantIdentity[path].uid || stat.Gid != wantIdentity[path].gid {
			t.Fatalf("restore did not recover protected runtime permissions at %s: mode=%04o uid=%d gid=%d", path, info.Mode().Perm(), stat.Uid, stat.Gid)
		}
	}
}

func TestWebsiteBackupPendingArtifactsSIGKILLChild(t *testing.T) {
	base := os.Getenv("POWERLINUX_BACKUP_PENDING_CHILD_BASE")
	if base == "" {
		t.Skip("subprocess helper")
	}
	e, configBase, site := backupConfigurationAgentAt(t, base)
	backupID := "b-pending-fixture"
	record := pendingBackupRecord{Version: 1, SiteID: site.ID, BackupID: backupID, SiteRevision: site.Revision, CreatedAt: time.Date(2026, 10, 13, 1, 2, 3, 0, time.UTC)}
	if err := e.writePendingBackupRecord(record); err != nil {
		t.Fatal(err)
	}
	writeBackupRuntimeConfig(t, configBase, site, []byte("raw web orphan"), []byte("raw php orphan"))
	if _, err := e.storeBackupConfiguration(context.Background(), site, backupID); err != nil {
		t.Fatal(err)
	}
	archive, err := e.state.OpenFile(filepath.Join("backups", site.ID, backupID+".tar.gz"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = archive.Write([]byte("incomplete unpublished archive")); err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(archive.Sync(), archive.Close()); err != nil {
		t.Fatal(err)
	}
	writeJournalBarrier(t, filepath.Join(base, "backup-pending-ready"))
	select {}
}

func TestWebsiteBackupPendingArtifactsSIGKILLRecovery(t *testing.T) {
	base := t.TempDir()
	e, _, site := backupConfigurationAgentAt(t, base)
	cmd := exec.Command(os.Args[0], "-test.run=^TestWebsiteBackupPendingArtifactsSIGKILLChild$")
	cmd.Env = append(os.Environ(), "POWERLINUX_BACKUP_PENDING_CHILD_BASE="+base)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	childAlive := true
	t.Cleanup(func() {
		if childAlive && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	waitJournalBarrier(t, filepath.Join(base, "backup-pending-ready"))
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("SIGKILLed pending backup child returned success")
	}
	childAlive = false
	if err := e.recoverPendingBackupArtifacts(); err != nil {
		t.Fatal("pending backup restart recovery failed", err)
	}
	backupID := "b-pending-fixture"
	for _, path := range []string{
		pendingBackupPath(site.ID, backupID),
		backupArchiveTempPath(site.ID, backupID),
		filepath.Join("backups", site.ID, backupID+".tar.gz"),
		backupConfigurationTempPath(site.ID, backupID),
		backupConfigurationPath(site.ID, backupID),
	} {
		if _, err := e.state.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("pending recovery left an unpublished backup artifact %q: %v", path, err)
		}
	}
}

func TestWebsiteBackupPendingRecoveryRefusesUnownedArtifact(t *testing.T) {
	e, _, site := newBackupConfigurationAgent(t)
	backupID := "b-pending-fixture"
	record := pendingBackupRecord{Version: 1, SiteID: site.ID, BackupID: backupID, SiteRevision: site.Revision, CreatedAt: time.Date(2026, 10, 13, 1, 2, 3, 0, time.UTC)}
	if err := e.writePendingBackupRecord(record); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(e.config.StateDir, "backups", site.ID, backupID+".tar.gz")
	if err := os.WriteFile(archivePath, []byte("not owned mode"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(archivePath, 0644); err != nil {
		t.Fatal(err)
	}
	if err := e.recoverPendingBackupsForSite(site.ID); core.ErrorCode(err) != "backup_recovery_required" {
		t.Fatal("recovery deleted or accepted an artifact without the expected private ownership mode", err)
	}
	if _, err := os.Stat(archivePath); err != nil {
		t.Fatal("unowned artifact was deleted", err)
	}
	if err := os.Chmod(archivePath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.recoverPendingBackupsForSite(site.ID); err != nil {
		t.Fatal("owner marker could not safely recover its exact private artifact", err)
	}
	if _, err := os.Stat(archivePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("confirmed pending artifact was not removed", err)
	}
}

func jsonUnmarshalStrictForBackupTest(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("backup configuration has trailing content")
	}
	return nil
}
