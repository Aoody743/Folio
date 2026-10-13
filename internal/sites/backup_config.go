package sites

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	backupConfigurationSchema = 1
	maxBackupConfigFile       = 128 << 10
	maxBackupConfigTotal      = 256 << 10
	maxBackupConfigArtifact   = 384 << 10
	maxPendingBackupRecords   = 128
	maxBackupInventoryEntries = 100000
)

type backupConfigurationReference struct {
	Schema int    `json:"schema"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type pendingBackupRecord struct {
	Version      int       `json:"version"`
	SiteID       string    `json:"site_id"`
	BackupID     string    `json:"backup_id"`
	SiteRevision int64     `json:"site_revision"`
	CreatedAt    time.Time `json:"created_at"`
}

// backupConfiguration contains exact runtime configuration bytes. It is kept
// in a root-private sidecar, never in the public archive or API response.
// Runtime paths are intentionally absent: they are re-derived from protected
// runtime receipts when the backup is restored.
type backupConfiguration struct {
	Schema       int                      `json:"schema"`
	SiteID       string                   `json:"site_id"`
	BackupID     string                   `json:"backup_id"`
	SiteRevision int64                    `json:"site_revision"`
	Mode         string                   `json:"mode"`
	Web          backupConfigurationFile  `json:"web"`
	PHP          *backupConfigurationFile `json:"php,omitempty"`
}

type backupConfigurationFile struct {
	Kind        string `json:"kind"`
	RuntimeID   string `json:"runtime_id"`
	Size        int    `json:"size"`
	SHA256      string `json:"sha256"`
	SourceDev   uint64 `json:"source_device"`
	SourceInode uint64 `json:"source_inode"`
	SourceMode  uint32 `json:"source_mode"`
	SourceUID   uint32 `json:"source_uid"`
	SourceGID   uint32 `json:"source_gid"`
	Data        []byte `json:"data"`
}

type backupRestoreConfiguration struct {
	SiteID     string
	Mode       string
	WebRuntime string
	PHPRuntime string
	Web        backupConfigurationFile
	PHP        *backupConfigurationFile
}

type backupRestoreConfigurationKey struct{}

func backupConfigurationPath(siteID, backupID string) string {
	return filepath.Join("backups", siteID, ".config", backupID+".json")
}

func pendingBackupPath(siteID, backupID string) string {
	return filepath.Join("backups", siteID, ".pending-"+backupID+".json")
}

func backupArchiveTempPath(siteID, backupID string) string {
	return filepath.Join("backups", siteID, "."+backupID+".tar.gz.part")
}

func backupConfigurationTempPath(siteID, backupID string) string {
	return backupConfigurationPath(siteID, backupID) + ".part"
}

func (e *AgentExecutor) writePendingBackupRecord(record pendingBackupRecord) error {
	if record.Version != 1 || !safeID.MatchString(record.SiteID) || !safeID.MatchString(record.BackupID) || record.SiteRevision < 0 || record.CreatedAt.IsZero() {
		return failure("backup_recovery_required", "Pending backup owner record is invalid", 503)
	}
	if err := e.verifyPrivateBackupDirectory(filepath.Join("backups", record.SiteID)); err != nil {
		return err
	}
	return atomicRoot(e.state, pendingBackupPath(record.SiteID, record.BackupID), encode(record), 0600)
}

func (e *AgentExecutor) readPendingBackupRecord(siteID, backupID string) (pendingBackupRecord, error) {
	var record pendingBackupRecord
	if !safeID.MatchString(siteID) || !safeID.MatchString(backupID) {
		return record, failure("backup_recovery_required", "Pending backup identity is invalid", 503)
	}
	path := pendingBackupPath(siteID, backupID)
	file, err := e.state.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return record, err
	}
	info, err := file.Stat()
	closeErr := file.Close()
	stat, ok := (*syscall.Stat_t)(nil), false
	if err == nil {
		stat, ok = info.Sys().(*syscall.Stat_t)
	}
	if err != nil || closeErr != nil || !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Mode()&os.ModeSetuid != 0 || info.Mode()&os.ModeSetgid != 0 || info.Mode()&os.ModeSticky != 0 || stat.Uid != uint32(os.Geteuid()) || info.Size() > 16<<10 {
		return record, failure("backup_recovery_required", "Pending backup owner record is not a protected regular file", 503)
	}
	if err = rootJSON(e.state, path, &record); err != nil {
		return record, failure("backup_recovery_required", "Pending backup owner record is malformed", 503)
	}
	if record.Version != 1 || record.SiteID != siteID || record.BackupID != backupID || record.SiteRevision < 0 || record.CreatedAt.IsZero() {
		return record, failure("backup_recovery_required", "Pending backup owner record does not match its path", 503)
	}
	return record, nil
}

func (e *AgentExecutor) recoverPendingBackupArtifacts() error {
	if err := e.verifyPrivateBackupDirectory("backups"); err != nil {
		return errors.Join(failure("backup_recovery_required", "Backup storage is not protected by the Agent", 503), err)
	}
	backups, err := e.state.Open("backups")
	if err != nil {
		return err
	}
	sites, readErr := backups.ReadDir(10001)
	closeErr := backups.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return errors.Join(readErr, closeErr)
	}
	if len(sites) > 10000 {
		return failure("backup_recovery_required", "Backup recovery inventory exceeds its site limit", 503)
	}
	if closeErr != nil {
		return closeErr
	}
	for _, entry := range sites {
		if !entry.IsDir() || !safeID.MatchString(entry.Name()) {
			continue
		}
		if err = e.recoverPendingBackupsForSite(entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func (e *AgentExecutor) recoverPendingBackupsForSite(siteID string) error {
	if !safeID.MatchString(siteID) {
		return failure("backup_recovery_required", "Backup recovery site identity is invalid", 503)
	}
	if err := e.verifyPrivateBackupDirectory(filepath.Join("backups", siteID)); err != nil {
		return errors.Join(failure("backup_recovery_required", "Backup recovery found an unprotected site directory", 503), err)
	}
	directory, err := e.state.Open(filepath.Join("backups", siteID))
	if err != nil {
		return err
	}
	pending := []string{}
	total := 0
	for {
		entries, batchErr := directory.ReadDir(256)
		total += len(entries)
		if total > maxBackupInventoryEntries {
			_ = directory.Close()
			return failure("backup_recovery_required", "Backup recovery inventory exceeds its entry limit", 503)
		}
		for _, candidate := range entries {
			if !strings.HasPrefix(candidate.Name(), ".pending-") || !strings.HasSuffix(candidate.Name(), ".json") {
				continue
			}
			backupID := strings.TrimSuffix(strings.TrimPrefix(candidate.Name(), ".pending-"), ".json")
			if !safeID.MatchString(backupID) {
				_ = directory.Close()
				return failure("backup_recovery_required", "Backup recovery found a malformed pending owner name", 503)
			}
			pending = append(pending, backupID)
			if len(pending) > maxPendingBackupRecords {
				_ = directory.Close()
				return failure("backup_recovery_required", "Backup recovery has too many pending owner records", 503)
			}
		}
		if errors.Is(batchErr, io.EOF) {
			break
		}
		if batchErr != nil {
			_ = directory.Close()
			return batchErr
		}
	}
	if err = directory.Close(); err != nil {
		return err
	}
	for _, backupID := range pending {
		if err = e.recoverPendingBackup(siteID, backupID); err != nil {
			return err
		}
	}
	return nil
}

func (e *AgentExecutor) recoverPendingBackup(siteID, backupID string) error {
	record, err := e.readPendingBackupRecord(siteID, backupID)
	if err != nil {
		return errors.Join(failure("backup_recovery_required", "An unfinished backup needs safe recovery before the Agent can continue", 503), err)
	}
	manifestPath := filepath.Join("backups", siteID, backupID+".json")
	var metadata backupManifest
	manifestInfo, manifestErr := e.state.Lstat(manifestPath)
	if manifestErr == nil {
		stat, ok := manifestInfo.Sys().(*syscall.Stat_t)
		if !ok || !manifestInfo.Mode().IsRegular() || manifestInfo.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Geteuid()) {
			return failure("backup_recovery_required", "Committed backup receipt is not protected by the Agent", 503)
		}
		err = rootJSON(e.state, manifestPath, &metadata)
	}
	if manifestErr == nil && err == nil {
		if (metadata.Version != 2) || metadata.Site.ID != siteID || metadata.Site.Revision != record.SiteRevision || metadata.Backup.ID != backupID || metadata.Backup.SiteID != siteID || !metadata.Backup.CreatedAt.Equal(record.CreatedAt) {
			return failure("backup_recovery_required", "Committed backup receipt conflicts with its pending owner record", 503)
		}
		return e.removePendingBackupRecord(record)
	}
	if manifestErr != nil && !errors.Is(manifestErr, fs.ErrNotExist) || manifestErr == nil && err != nil {
		return errors.Join(failure("backup_recovery_required", "Unfinished backup receipt cannot be validated safely", 503), err)
	}
	if _, statErr := e.state.Lstat(filepath.Join("backups", siteID, ".config")); statErr == nil {
		if err = e.verifyPrivateBackupDirectory(filepath.Join("backups", siteID, ".config")); err != nil {
			return errors.Join(failure("backup_recovery_required", "Private backup configuration directory is not protected", 503), err)
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return statErr
	}
	for _, path := range []string{
		backupArchiveTempPath(siteID, backupID),
		filepath.Join("backups", siteID, backupID+".tar.gz"),
		backupConfigurationTempPath(siteID, backupID),
		backupConfigurationPath(siteID, backupID),
	} {
		if err = e.removeOwnedPendingBackupArtifact(path); err != nil {
			return errors.Join(failure("backup_recovery_required", "Unfinished backup artifact ownership could not be proven", 503), err)
		}
	}
	// A manifest may have been renamed before its directory fsync reported an
	// error. It was absent above; check again before cleaning the marker.
	if _, err = e.state.Lstat(manifestPath); err == nil {
		return failure("backup_recovery_required", "Backup receipt appeared during pending recovery", 503)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err = cleanupBackupConfigDirectory(e, siteID); err != nil {
		return err
	}
	if err = syncBackupDirectory(e, filepath.Join("backups", siteID, ".config")); err != nil {
		return err
	}
	if err = syncBackupDirectory(e, filepath.Join("backups", siteID)); err != nil {
		return err
	}
	return e.removePendingBackupRecord(record)
}

func (e *AgentExecutor) removeOwnedPendingBackupArtifact(path string) error {
	info, err := e.state.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || info.Mode()&os.ModeSetuid != 0 || info.Mode()&os.ModeSetgid != 0 || info.Mode()&os.ModeSticky != 0 || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("pending backup artifact is not a protected regular file owned by the Agent")
	}
	return e.state.Remove(path)
}

func (e *AgentExecutor) cleanupPendingBackupArtifacts(record pendingBackupRecord) error {
	stored, err := e.readPendingBackupRecord(record.SiteID, record.BackupID)
	if err != nil || !stored.CreatedAt.Equal(record.CreatedAt) || stored.Version != record.Version || stored.SiteID != record.SiteID || stored.BackupID != record.BackupID || stored.SiteRevision != record.SiteRevision {
		return failure("backup_recovery_required", "Backup artifacts have no matching protected owner record", 503)
	}
	manifestPath := filepath.Join("backups", record.SiteID, record.BackupID+".json")
	if info, statErr := e.state.Lstat(manifestPath); statErr == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Geteuid()) {
			return failure("backup_recovery_required", "Backup receipt is not protected by the Agent", 503)
		}
		var metadata backupManifest
		if err = rootJSON(e.state, manifestPath, &metadata); err != nil || metadata.Version != 2 || metadata.Site.ID != record.SiteID || metadata.Site.Revision != record.SiteRevision || metadata.Backup.ID != record.BackupID || metadata.Backup.SiteID != record.SiteID || !metadata.Backup.CreatedAt.Equal(record.CreatedAt) {
			return failure("backup_recovery_required", "Backup receipt cannot be removed without a matching owner record", 503)
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return statErr
	}
	if _, err = e.state.Lstat(filepath.Join("backups", record.SiteID, ".config")); err == nil {
		if err = e.verifyPrivateBackupDirectory(filepath.Join("backups", record.SiteID, ".config")); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, path := range []string{
		backupArchiveTempPath(record.SiteID, record.BackupID),
		filepath.Join("backups", record.SiteID, record.BackupID+".tar.gz"),
		backupConfigurationTempPath(record.SiteID, record.BackupID),
		backupConfigurationPath(record.SiteID, record.BackupID),
		manifestPath,
	} {
		if err = e.removeOwnedPendingBackupArtifact(path); err != nil {
			return errors.Join(failure("backup_recovery_required", "Backup artifact ownership could not be proven", 503), err)
		}
	}
	if err = cleanupBackupConfigDirectory(e, record.SiteID); err != nil {
		return err
	}
	if err = syncBackupDirectory(e, filepath.Join("backups", record.SiteID, ".config")); err != nil {
		return err
	}
	if err = syncBackupDirectory(e, filepath.Join("backups", record.SiteID)); err != nil {
		return err
	}
	return e.removePendingBackupRecord(record)
}

func (e *AgentExecutor) removePendingBackupRecord(record pendingBackupRecord) error {
	stored, err := e.readPendingBackupRecord(record.SiteID, record.BackupID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || stored != record {
		return failure("backup_recovery_required", "Pending backup owner record changed during recovery", 503)
	}
	if err = e.state.Remove(pendingBackupPath(record.SiteID, record.BackupID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncBackupDirectory(e, filepath.Join("backups", record.SiteID))
}

func (e *AgentExecutor) storeBackupConfiguration(ctx context.Context, site Site, backupID string) (*backupConfigurationReference, error) {
	if site.ConfigurationMode != "" && site.ConfigurationMode != "managed" && site.ConfigurationMode != "advanced" {
		return nil, failure("backup_configuration_invalid", "Website configuration mode is not recognized", 409)
	}
	if site.ConfigurationMode != "advanced" {
		return nil, nil
	}
	if !safeID.MatchString(site.ID) || !safeID.MatchString(backupID) {
		return nil, invalid("Invalid backup configuration identity")
	}
	web, err := e.runtimes.Web(ctx, site.WebInstance)
	if err != nil {
		return nil, err
	}
	if web.ID != site.WebInstance {
		return nil, failure("backup_configuration_invalid", "Backup Web runtime receipt does not match the website", 409)
	}
	webFile, err := captureBackupConfigurationFile(filepath.Join(web.SnippetsDir, site.ID+".conf"), "web", web.ID)
	if err != nil {
		return nil, err
	}
	configuration := backupConfiguration{
		Schema: backupConfigurationSchema, SiteID: site.ID, BackupID: backupID,
		SiteRevision: site.Revision, Mode: site.ConfigurationMode, Web: webFile,
	}
	if site.PHPInstance != "" {
		php, err := e.runtimes.PHP(ctx, site.PHPInstance)
		if err != nil {
			return nil, err
		}
		if php.ID != site.PHPInstance {
			return nil, failure("backup_configuration_invalid", "Backup PHP runtime receipt does not match the website", 409)
		}
		phpFile, err := captureBackupConfigurationFile(filepath.Join(php.PoolDir, site.ID+".conf"), "php", php.ID)
		if err != nil {
			return nil, err
		}
		configuration.PHP = &phpFile
	}
	if len(configuration.Web.Data)+backupConfigFileSize(configuration.PHP) > maxBackupConfigTotal {
		return nil, failure("too_large", "Website runtime configuration exceeds the private backup limit", 413)
	}
	raw := encode(configuration)
	if len(raw) == 0 || len(raw) > maxBackupConfigArtifact {
		return nil, failure("too_large", "Website runtime configuration artifact exceeds its private backup limit", 413)
	}
	directory := filepath.Join("backups", site.ID)
	if err = e.verifyPrivateBackupDirectory(directory); err != nil {
		return nil, err
	}
	configDirectory := filepath.Join(directory, ".config")
	if err = e.state.MkdirAll(configDirectory, 0700); err != nil {
		return nil, err
	}
	if err = e.verifyPrivateBackupDirectory(configDirectory); err != nil {
		return nil, err
	}
	path := backupConfigurationPath(site.ID, backupID)
	if _, err = e.state.Lstat(path); err == nil {
		return nil, failure("backup_conflict", "A private configuration artifact already exists for this backup", 409)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err = writePrivateBackupConfiguration(e, site.ID, backupID, raw); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return &backupConfigurationReference{Schema: backupConfigurationSchema, Size: int64(len(raw)), SHA256: hex.EncodeToString(sum[:])}, nil
}

func writePrivateBackupConfiguration(e *AgentExecutor, siteID, backupID string, raw []byte) error {
	temp := backupConfigurationTempPath(siteID, backupID)
	path := backupConfigurationPath(siteID, backupID)
	file, err := e.state.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer e.state.Remove(temp)
	written, writeErr := io.Copy(file, bytes.NewReader(raw))
	if writeErr == nil && written != int64(len(raw)) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err = errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	if err = e.state.Rename(temp, path); err != nil {
		return err
	}
	return syncBackupDirectory(e, filepath.Join("backups", siteID, ".config"))
}

func backupConfigFileSize(file *backupConfigurationFile) int {
	if file == nil {
		return 0
	}
	return len(file.Data)
}

func captureBackupConfigurationFile(path, kind, runtimeID string) (backupConfigurationFile, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !safeID.MatchString(runtimeID) {
		return backupConfigurationFile{}, failure("backup_configuration_invalid", "Runtime configuration path is invalid", 409)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return backupConfigurationFile{}, failure("backup_configuration_missing", "Advanced website configuration is missing from its protected runtime", 409)
		}
		return backupConfigurationFile{}, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() < 0 || before.Size() > maxBackupConfigFile {
		return backupConfigurationFile{}, failure("backup_configuration_invalid", "Runtime configuration must be a bounded regular file", 409)
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || before.Mode().Perm()&0022 != 0 || before.Mode()&os.ModeSetuid != 0 || before.Mode()&os.ModeSetgid != 0 || before.Mode()&os.ModeSticky != 0 {
		return backupConfigurationFile{}, failure("backup_configuration_invalid", "Runtime configuration ownership or mode is not protected", 409)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBackupConfigFile+1))
	if err != nil {
		return backupConfigurationFile{}, err
	}
	if len(data) > maxBackupConfigFile || int64(len(data)) != before.Size() {
		return backupConfigurationFile{}, failure("source_changed", "Runtime configuration changed during backup", 409)
	}
	after, err := file.Stat()
	if err != nil {
		return backupConfigurationFile{}, err
	}
	afterStat, ok := after.Sys().(*syscall.Stat_t)
	if !ok || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || stat.Dev != afterStat.Dev || stat.Ino != afterStat.Ino || stat.Ctim != afterStat.Ctim {
		return backupConfigurationFile{}, failure("source_changed", "Runtime configuration changed during backup", 409)
	}
	sum := sha256.Sum256(data)
	return backupConfigurationFile{
		Kind: kind, RuntimeID: runtimeID, Size: len(data), SHA256: hex.EncodeToString(sum[:]),
		SourceDev: uint64(stat.Dev), SourceInode: stat.Ino, SourceMode: uint32(before.Mode().Perm()),
		SourceUID: stat.Uid, SourceGID: stat.Gid, Data: data,
	}, nil
}

func (e *AgentExecutor) verifyPrivateBackupDirectory(path string) error {
	info, err := e.state.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSetuid != 0 || info.Mode()&os.ModeSetgid != 0 || info.Mode()&os.ModeSticky != 0 || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		return failure("backup_configuration_invalid", "Private backup storage is not owned and protected by the Agent", 409)
	}
	return nil
}

func validateBackupConfigurationFile(file backupConfigurationFile, kind, runtimeID string) error {
	if file.Kind != kind || file.RuntimeID != runtimeID || !safeID.MatchString(runtimeID) || file.Size < 0 || file.Size > maxBackupConfigFile || file.Size != len(file.Data) || file.SourceDev == 0 || file.SourceInode == 0 || file.SourceMode&^0777 != 0 || file.SourceMode&0022 != 0 || file.SourceUID != uint32(os.Geteuid()) || len(file.SHA256) != 64 {
		return errors.New("private runtime configuration binding is invalid")
	}
	if _, err := hex.DecodeString(file.SHA256); err != nil {
		return errors.New("private runtime configuration hash is invalid")
	}
	sum := sha256.Sum256(file.Data)
	if hex.EncodeToString(sum[:]) != file.SHA256 {
		return errors.New("private runtime configuration bytes do not match their hash")
	}
	return nil
}

func (e *AgentExecutor) loadBackupConfiguration(ctx context.Context, metadata backupManifest) (*backupRestoreConfiguration, error) {
	if metadata.Site.ConfigurationMode != "" && metadata.Site.ConfigurationMode != "managed" && metadata.Site.ConfigurationMode != "advanced" {
		return nil, failure("backup_configuration_invalid", "Website backup configuration mode is not recognized", 409)
	}
	advanced := metadata.Site.ConfigurationMode == "advanced"
	if metadata.Version == 1 {
		if metadata.Configuration != nil {
			return nil, failure("backup_configuration_invalid", "Version-one backup contains an unsupported configuration reference", 409)
		}
		if advanced {
			return nil, failure("backup_configuration_missing", "This advanced website backup predates exact configuration snapshots and cannot be restored safely", 409)
		}
		return nil, nil
	}
	if metadata.Version != 2 || !safeID.MatchString(metadata.Site.ID) || !safeID.MatchString(metadata.Backup.ID) {
		return nil, failure("backup_configuration_invalid", "Website backup metadata version or identity is invalid", 409)
	}
	if !advanced {
		if metadata.Configuration != nil {
			return nil, failure("backup_configuration_invalid", "Managed website backup unexpectedly contains raw configuration", 409)
		}
		return nil, nil
	}
	ref := metadata.Configuration
	if ref == nil || ref.Schema != backupConfigurationSchema || ref.Size < 1 || ref.Size > maxBackupConfigArtifact || len(ref.SHA256) != 64 {
		return nil, failure("backup_configuration_missing", "Advanced website backup has no valid exact configuration snapshot", 409)
	}
	if _, err := hex.DecodeString(ref.SHA256); err != nil {
		return nil, failure("backup_configuration_invalid", "Advanced website backup configuration hash is invalid", 409)
	}
	if err := e.verifyPrivateBackupDirectory(filepath.Join("backups", metadata.Site.ID)); err != nil {
		return nil, err
	}
	if err := e.verifyPrivateBackupDirectory(filepath.Join("backups", metadata.Site.ID, ".config")); err != nil {
		return nil, failure("backup_configuration_missing", "Advanced website backup configuration artifact is missing", 409)
	}
	path := backupConfigurationPath(metadata.Site.ID, metadata.Backup.ID)
	file, err := e.state.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, failure("backup_configuration_missing", "Advanced website backup configuration artifact is missing", 409)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Mode()&os.ModeSetuid != 0 || info.Mode()&os.ModeSetgid != 0 || info.Mode()&os.ModeSticky != 0 || info.Size() != ref.Size || info.Size() > maxBackupConfigArtifact {
		return nil, failure("backup_configuration_invalid", "Advanced website backup configuration artifact is not a protected bounded file", 409)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, failure("backup_configuration_invalid", "Advanced website backup configuration artifact has the wrong owner", 409)
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxBackupConfigArtifact+1))
	if err != nil || int64(len(raw)) != ref.Size || len(raw) > maxBackupConfigArtifact {
		return nil, failure("backup_configuration_invalid", "Advanced website backup configuration artifact is truncated or oversized", 409)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != ref.SHA256 {
		return nil, failure("backup_configuration_invalid", "Advanced website backup configuration artifact failed its integrity check", 409)
	}
	var configuration backupConfiguration
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&configuration) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, failure("backup_configuration_invalid", "Advanced website backup configuration artifact is malformed", 409)
	}
	if configuration.Schema != backupConfigurationSchema || configuration.SiteID != metadata.Site.ID || configuration.BackupID != metadata.Backup.ID || configuration.SiteRevision != metadata.Site.Revision || configuration.Mode != "advanced" {
		return nil, failure("backup_configuration_invalid", "Advanced website backup configuration binding does not match its receipt", 409)
	}
	if err = validateBackupConfigurationFile(configuration.Web, "web", metadata.Site.WebInstance); err != nil {
		return nil, failure("backup_configuration_invalid", "Advanced Web configuration snapshot failed validation", 409)
	}
	web, err := e.runtimes.Web(ctx, metadata.Site.WebInstance)
	if err != nil || web.ID != metadata.Site.WebInstance || !filepath.IsAbs(web.SnippetsDir) {
		return nil, failure("backup_configuration_invalid", "Advanced Web configuration runtime receipt is unavailable", 409)
	}
	restored := &backupRestoreConfiguration{
		SiteID: metadata.Site.ID, Mode: configuration.Mode, WebRuntime: web.ID,
		Web: cloneBackupConfigurationFile(configuration.Web),
	}
	if metadata.Site.PHPInstance == "" {
		if configuration.PHP != nil {
			return nil, failure("backup_configuration_invalid", "Advanced PHP configuration snapshot does not match the website receipt", 409)
		}
	} else {
		if configuration.PHP == nil {
			return nil, failure("backup_configuration_invalid", "Advanced PHP configuration snapshot is missing", 409)
		}
		if err = validateBackupConfigurationFile(*configuration.PHP, "php", metadata.Site.PHPInstance); err != nil {
			return nil, failure("backup_configuration_invalid", "Advanced PHP configuration snapshot failed validation", 409)
		}
		php, phpErr := e.runtimes.PHP(ctx, metadata.Site.PHPInstance)
		if phpErr != nil || php.ID != metadata.Site.PHPInstance || !filepath.IsAbs(php.PoolDir) {
			return nil, failure("backup_configuration_invalid", "Advanced PHP configuration runtime receipt is unavailable", 409)
		}
		restored.PHPRuntime = php.ID
		phpFile := cloneBackupConfigurationFile(*configuration.PHP)
		restored.PHP = &phpFile
	}
	return restored, nil
}

func withBackupRestoreConfiguration(ctx context.Context, configuration *backupRestoreConfiguration) context.Context {
	if configuration == nil {
		return ctx
	}
	return context.WithValue(ctx, backupRestoreConfigurationKey{}, configuration)
}

func cloneBackupConfigurationFile(file backupConfigurationFile) backupConfigurationFile {
	file.Data = append([]byte(nil), file.Data...)
	return file
}

func backupRestoreBytes(ctx context.Context, site Site, kind, runtimeID string) (backupConfigurationFile, bool, error) {
	configuration, ok := ctx.Value(backupRestoreConfigurationKey{}).(*backupRestoreConfiguration)
	if !ok || configuration == nil {
		return backupConfigurationFile{}, false, nil
	}
	if configuration.SiteID != site.ID || configuration.Mode != "advanced" || configuration.WebRuntime != site.WebInstance {
		return backupConfigurationFile{}, false, failure("backup_configuration_invalid", "Exact website configuration is not bound to this restore", 409)
	}
	switch kind {
	case "web":
		if runtimeID != configuration.WebRuntime {
			return backupConfigurationFile{}, false, failure("backup_configuration_invalid", "Exact Web configuration runtime does not match the receipt", 409)
		}
		return configuration.Web, true, nil
	case "php":
		if site.PHPInstance == "" && configuration.PHPRuntime == "" {
			return backupConfigurationFile{}, false, nil
		}
		if runtimeID != configuration.PHPRuntime || configuration.PHP == nil {
			return backupConfigurationFile{}, false, failure("backup_configuration_invalid", "Exact PHP configuration runtime does not match the receipt", 409)
		}
		return *configuration.PHP, true, nil
	default:
		return backupConfigurationFile{}, false, fmt.Errorf("unknown runtime configuration kind %q", kind)
	}
}

func restoreBackupConfigurationMode(path string, configuration backupConfigurationFile) error {
	if configuration.SourceUID != uint32(os.Geteuid()) || configuration.SourceMode&^0777 != 0 || configuration.SourceMode&0022 != 0 {
		return failure("backup_configuration_invalid", "Saved runtime configuration ownership or mode is unsafe", 409)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("restored runtime configuration is not a regular file")
	}
	if err == nil {
		err = file.Chown(int(configuration.SourceUID), int(configuration.SourceGID))
	}
	if err == nil {
		err = file.Chmod(fs.FileMode(configuration.SourceMode))
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr = directory.Close()
	return errors.Join(syncErr, closeErr)
}

func removeBackupConfigSidecar(e *AgentExecutor, siteID, backupID string) error {
	err := e.state.Remove(backupConfigurationPath(siteID, backupID))
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return err
	}
	directory, openErr := e.state.Open(filepath.Join("backups", siteID, ".config"))
	if errors.Is(openErr, fs.ErrNotExist) {
		return nil
	}
	if openErr != nil {
		return openErr
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

func cleanupBackupConfigDirectory(e *AgentExecutor, siteID string) error {
	err := e.state.Remove(filepath.Join("backups", siteID, ".config"))
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTEMPTY) {
		return nil
	}
	if err != nil {
		return err
	}
	directory, err := e.state.Open(filepath.Join("backups", siteID))
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

func removeBackupArtifact(e *AgentExecutor, path string) error {
	err := e.state.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func syncBackupDirectory(e *AgentExecutor, path string) error {
	directory, err := e.state.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

func backupCleanupFailure(siteID, backupID string, err error) error {
	log.Printf("website backup cleanup pending site=%s backup=%s: %v", siteID, backupID, err)
	return errors.Join(err, failure("backup_cleanup_required", "Backup cleanup is incomplete; retry deletion or inspect Agent logs", 500))
}
