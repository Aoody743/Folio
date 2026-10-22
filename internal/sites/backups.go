package sites

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type backupManifest struct {
	Version       int                           `json:"version"`
	Site          Site                          `json:"site"`
	Backup        Backup                        `json:"backup"`
	Configuration *backupConfigurationReference `json:"configuration,omitempty"`
}
type cancelRead struct {
	ctx    context.Context
	reader io.Reader
}

func (r cancelRead) Read(buf []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buf)
}

func (e *AgentExecutor) backups(id string) ([]Backup, error) {
	if _, err := e.get(id); err != nil {
		return nil, err
	}
	directory, err := e.state.Open(filepath.Join("backups", id))
	if errors.Is(err, fs.ErrNotExist) {
		return []Backup{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := []Backup{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pending-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var metadata backupManifest
		if err := rootJSON(e.state, filepath.Join("backups", id, entry.Name()), &metadata); err != nil {
			return nil, err
		}
		if (metadata.Version != 1 && metadata.Version != 2) || metadata.Site.ID != id || metadata.Backup.SiteID != id || metadata.Backup.ID != strings.TrimSuffix(entry.Name(), ".json") {
			return nil, errors.New("backup ownership receipt mismatch")
		}
		out = append(out, metadata.Backup)
	}
	return out, nil
}

func (e *AgentExecutor) backup(ctx context.Context, in request) (backup Backup, err error) {
	site, err := e.get(in.ID)
	if err != nil {
		return backup, err
	}
	if site.Revision != in.ExpectedRevision {
		return backup, failure("revision_conflict", "Website revision changed before backup", 412)
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return backup, err
	}
	backup = Backup{ID: "b-" + hex.EncodeToString(random[:]), SiteID: site.ID, CreatedAt: time.Now().UTC(), IncludesTLS: false}
	directory := filepath.Join("backups", site.ID)
	if err = e.state.MkdirAll(directory, 0700); err != nil {
		return backup, err
	}
	if err = e.verifyPrivateBackupDirectory(directory); err != nil {
		return backup, err
	}
	if err = e.recoverPendingBackupsForSite(site.ID); err != nil {
		return backup, err
	}
	if _, statErr := e.state.Lstat(filepath.Join(directory, ".config")); statErr == nil {
		if err = e.verifyPrivateBackupDirectory(filepath.Join(directory, ".config")); err != nil {
			return backup, err
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return backup, statErr
	}
	temp := backupArchiveTempPath(site.ID, backup.ID)
	pending := pendingBackupRecord{Version: 1, SiteID: site.ID, BackupID: backup.ID, SiteRevision: site.Revision, CreatedAt: backup.CreatedAt}
	for _, path := range []string{
		filepath.Join(directory, backup.ID+".tar.gz"), filepath.Join(directory, backup.ID+".json"),
		backupConfigurationPath(site.ID, backup.ID), backupConfigurationTempPath(site.ID, backup.ID),
		temp, pendingBackupPath(site.ID, backup.ID),
	} {
		if _, statErr := e.state.Lstat(path); statErr == nil {
			return backup, failure("backup_conflict", "A backup with this identity already exists", 409)
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return backup, statErr
		}
	}
	if err = e.writePendingBackupRecord(pending); err != nil {
		if _, markerErr := e.state.Lstat(pendingBackupPath(site.ID, backup.ID)); markerErr == nil {
			recoveryErr := e.recoverPendingBackup(site.ID, backup.ID)
			return backup, errors.Join(err, recoveryErr)
		}
		return backup, err
	}
	published := false
	defer func() {
		if published {
			return
		}
		cleanupErr := e.cleanupPendingBackupArtifacts(pending)
		if cleanupErr != nil {
			log.Printf("website backup cleanup pending site=%s backup=%s: %v", site.ID, backup.ID, cleanupErr)
			err = errors.Join(err, failure("backup_cleanup_required", "Failed backup artifacts remain under a protected recovery record; restart the Agent or retry backup creation", 500))
		}
	}()
	file, err := e.state.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return backup, err
	}
	defer e.state.Remove(temp)
	hash := sha256.New()
	compressed := gzip.NewWriter(io.MultiWriter(file, hash))
	writer := tar.NewWriter(compressed)
	root, err := e.content.OpenRoot(filepath.Join(site.ID, "public"))
	if err != nil {
		file.Close()
		return backup, err
	}
	defer root.Close()
	entries := 0
	directories := map[string]fs.FileInfo{}
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		entries++
		if entries > 100000 {
			return failure("too_large", "Website backup has too many entries", 413)
		}
		info, err := root.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return failure("unsafe_path", "Website backup refuses symbolic-link paths", 403)
		}
		if path == "." {
			directories[path] = info
			return nil
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return failure("unsafe_path", "Website backup accepts only regular files and directories", 403)
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(path)
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""
		if info.IsDir() {
			directories[path] = info
			return writer.WriteHeader(header)
		}
		source, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer source.Close()
		opened, err := source.Stat()
		if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
			return failure("source_changed", "Website file changed during backup", 409)
		}
		if err = writer.WriteHeader(header); err != nil {
			return err
		}
		written, err := io.CopyBuffer(writer, cancelRead{ctx, io.LimitReader(source, opened.Size()+1)}, make([]byte, 32<<10))
		if err != nil {
			return err
		}
		after, err := source.Stat()
		if err != nil || written != opened.Size() || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
			return failure("source_changed", "Website file changed during backup", 409)
		}
		return nil
	})
	if err == nil {
		for path, before := range directories {
			after, statErr := root.Lstat(path)
			if statErr != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
				err = failure("source_changed", "Website directory changed during backup", 409)
				break
			}
		}
	}
	err = errors.Join(err, writer.Close(), compressed.Close())
	if err == nil {
		err = file.Sync()
	}
	info, statErr := file.Stat()
	err = errors.Join(err, statErr, file.Close())
	if err != nil {
		return backup, err
	}
	backup.Size = info.Size()
	backup.SHA256 = hex.EncodeToString(hash.Sum(nil))
	archive := filepath.Join(directory, backup.ID+".tar.gz")
	if err = e.state.Rename(temp, archive); err != nil {
		return backup, err
	}
	configuration, configErr := e.storeBackupConfiguration(ctx, site, backup.ID)
	if configErr != nil {
		return backup, configErr
	}
	metadata := backupManifest{Version: 2, Site: site, Backup: backup, Configuration: configuration}
	if err = atomicRoot(e.state, filepath.Join(directory, backup.ID+".json"), encode(metadata), 0600); err != nil {
		return backup, err
	}
	published = true
	if err = e.removePendingBackupRecord(pending); err != nil {
		log.Printf("committed website backup has pending owner marker site=%s backup=%s: %v", site.ID, backup.ID, err)
	}
	return backup, nil
}

func (e *AgentExecutor) deleteBackup(in request) (any, error) {
	if !safeID.MatchString(in.BackupID) {
		return nil, invalid("Invalid backup ID")
	}
	var metadata backupManifest
	directory := filepath.Join("backups", in.ID)
	if err := rootJSON(e.state, filepath.Join(directory, in.BackupID+".json"), &metadata); err != nil {
		return nil, err
	}
	if metadata.Site.ID != in.ID || metadata.Backup.SiteID != in.ID || metadata.Backup.ID != in.BackupID {
		return nil, failure("ownership_conflict", "Backup ownership receipt does not match the selected website", 409)
	}
	if metadata.Version != 1 && metadata.Version != 2 {
		return nil, failure("ownership_conflict", "Backup metadata version is invalid", 409)
	}
	if err := removeBackupArtifact(e, filepath.Join(directory, in.BackupID+".tar.gz")); err != nil {
		return nil, backupCleanupFailure(in.ID, in.BackupID, err)
	}
	if err := removeBackupConfigSidecar(e, in.ID, in.BackupID); err != nil {
		return nil, backupCleanupFailure(in.ID, in.BackupID, err)
	}
	if err := cleanupBackupConfigDirectory(e, in.ID); err != nil {
		return nil, backupCleanupFailure(in.ID, in.BackupID, err)
	}
	if err := removeBackupArtifact(e, filepath.Join(directory, in.BackupID+".json")); err != nil {
		return nil, backupCleanupFailure(in.ID, in.BackupID, err)
	}
	if err := syncBackupDirectory(e, directory); err != nil {
		return nil, backupCleanupFailure(in.ID, in.BackupID, err)
	}
	return map[string]any{"id": in.BackupID, "deleted": true}, nil
}

func (e *AgentExecutor) restore(ctx context.Context, in request) (any, error) {
	current, err := e.get(in.ID)
	if err != nil {
		return nil, err
	}
	if current.Revision != in.ExpectedRevision {
		return nil, failure("revision_conflict", "Website revision changed before restore", 412)
	}
	if !safeID.MatchString(in.BackupID) {
		return nil, invalid("Invalid backup ID")
	}
	var metadata backupManifest
	directory := filepath.Join("backups", in.ID)
	if err = rootJSON(e.state, filepath.Join(directory, in.BackupID+".json"), &metadata); err != nil {
		return nil, err
	}
	if (metadata.Version != 1 && metadata.Version != 2) || metadata.Site.ID != in.ID || metadata.Backup.SiteID != in.ID || metadata.Backup.ID != in.BackupID {
		return nil, failure("ownership_conflict", "Backup belongs to another website", 409)
	}
	file, err := e.state.OpenFile(filepath.Join(directory, in.BackupID+".tar.gz"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.CopyBuffer(hash, cancelRead{ctx, file}, make([]byte, 32<<10))
	if err != nil {
		return nil, err
	}
	if size != metadata.Backup.Size || hex.EncodeToString(hash.Sum(nil)) != metadata.Backup.SHA256 {
		return nil, failure("checksum_mismatch", "Website backup was modified after creation", 409)
	}
	restoreConfiguration, err := e.loadBackupConfiguration(ctx, metadata)
	if err != nil {
		return nil, err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	stage, err := e.recordRestoreStagePath(ctx)
	if err != nil {
		return nil, err
	}
	if err = e.content.Mkdir(stage, 0700); err != nil {
		return nil, err
	}
	if err = e.syncContentParent(stage); err != nil {
		return nil, err
	}
	if err = e.recordRestoreStage(ctx, in.ID, in.BackupID, stage); err != nil {
		return nil, err
	}
	root, err := e.content.OpenRoot(stage)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	count := 0
	paths := map[string]bool{}
	directoryModes := map[string]fs.FileMode{".": 0750}
	ensureDirectories := func(path string) error {
		if path == "." {
			return nil
		}
		parts := strings.Split(path, string(filepath.Separator))
		for index := range parts {
			name := filepath.Join(parts[:index+1]...)
			if _, exists := directoryModes[name]; !exists {
				directoryModes[name] = 0750
			}
			if err := root.Mkdir(name, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
		}
		return nil
	}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		count++
		if count > 100000 {
			return nil, failure("too_large", "Backup restore has too many entries", 413)
		}
		path, err := relative(header.Name)
		if err != nil || path == "." || paths[path] {
			return nil, failure("unsafe_archive", "Backup contains an unsafe or duplicate path", 403)
		}
		paths[path] = true
		switch header.Typeflag {
		case tar.TypeDir:
			if err = ensureDirectories(path); err != nil {
				return nil, err
			}
			directoryModes[path] = fs.FileMode(header.Mode) & 0777 &^ 0022
		case tar.TypeReg:
			if header.Size < 0 {
				return nil, invalid("Backup has a negative file size")
			}
			if err = ensureDirectories(filepath.Dir(path)); err != nil {
				return nil, err
			}
			output, err := root.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
			if err != nil {
				return nil, err
			}
			written, copyErr := io.CopyBuffer(output, cancelRead{ctx, reader}, make([]byte, 32<<10))
			modeErr := output.Chmod(fs.FileMode(header.Mode) & 0777 &^ 0022)
			syncErr := output.Sync()
			closeErr := output.Close()
			if copyErr != nil || modeErr != nil || syncErr != nil || closeErr != nil || written != header.Size {
				return nil, errors.Join(copyErr, modeErr, syncErr, closeErr, errors.New("backup file did not restore completely"))
			}
		default:
			return nil, failure("unsafe_archive", "Backup links and special entries cannot be restored", 403)
		}
	}
	// Force verification of the gzip trailer after tar's end markers.
	if trailing, err := io.Copy(io.Discard, io.LimitReader(cancelRead{ctx, compressed}, 1025)); err != nil || trailing > 1024 {
		return nil, failure("invalid_archive", "Backup has a corrupt trailer or unexpected trailing content", 400)
	}
	worker, err := e.identity(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	web, err := e.runtimes.Web(ctx, current.WebInstance)
	if err != nil {
		return nil, err
	}
	group, err := user.LookupGroup(web.WorkerGroup)
	if err != nil {
		return nil, err
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return nil, err
	}
	if err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return root.Chown(path, int(worker.UID), gid)
	}); err != nil {
		return nil, err
	}
	// Read-only directory headers are applied only after all content and its
	// isolated ownership are ready. Explicit modes are independent of umask.
	names := make([]string, 0, len(directoryModes))
	for name := range directoryModes {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		return strings.Count(names[i], "/") > strings.Count(names[j], "/") || strings.Count(names[i], "/") == strings.Count(names[j], "/") && len(names[i]) > len(names[j])
	})
	for _, name := range names {
		if err = root.Chmod(name, directoryModes[name]); err != nil {
			return nil, err
		}
	}
	if err = syncRestoreTree(root); err != nil {
		return nil, err
	}
	var result any
	err = e.files.RootTransaction(ctx, "site-"+in.ID, func() error {
		var err error
		result, err = e.swapRestore(ctx, in, current, metadata, stage, restoreConfiguration)
		return err
	})
	return result, err
}

func (e *AgentExecutor) swapRestore(ctx context.Context, in request, current Site, metadata backupManifest, stage string, restoreConfiguration *backupRestoreConfiguration) (result any, err error) {
	changes, err := e.restoreSnapshots(ctx, current, metadata.Site)
	if err != nil {
		return nil, err
	}
	if err = e.recordRestoreSwap(ctx, in.ID); err != nil {
		return nil, err
	}
	active := ctx.Value(websiteJournalKey{}).(*activeWebsiteJournal)
	public, previous, _ := websiteRestorePaths(*active.record.Swap, active.record.ID)
	if err = e.content.Rename(public, previous); err != nil {
		return nil, err
	}
	if err = e.syncContentSite(in.ID); err != nil {
		return nil, err
	}
	if err = e.content.Rename(stage, public); err != nil {
		return nil, err
	}
	if err = e.syncContentSite(in.ID); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if committed || journalActive(ctx, e) {
			return
		}
		restoreErr := e.content.Rename(public, stage)
		restoreErr = errors.Join(restoreErr, e.content.Rename(previous, public), rollbackConfigs(changes))
		recover, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		for _, id := range []string{current.WebInstance, current.PHPInstance, metadata.Site.PHPInstance} {
			if id != "" {
				restoreErr = errors.Join(restoreErr, e.runtimes.Reload(recover, id))
			}
		}
		if restoreErr != nil {
			err = errors.Join(err, failure("rollback_incomplete", "Website restoration requires recovery inspection", 500))
		}
	}()
	restored := metadata.Site
	restored.Revision = current.Revision
	restored.CreatedAt = current.CreatedAt
	restored.UpdatedBy = in.Actor.Public()
	restored.DocumentRoot = current.DocumentRoot
	apply := request{ID: in.ID, Actor: in.Actor, ExpectedRevision: current.Revision, Site: &restored, Password: in.Password, ResetAdvancedConfig: true}
	restoreCtx := withBackupRestoreConfiguration(ctx, restoreConfiguration)
	restoredSite, applyErr := e.apply(restoreCtx, apply)
	if applyErr != nil {
		return nil, applyErr
	}
	if err = e.files.DeleteRoot("site-" + in.ID); err != nil {
		return nil, err
	}
	if err = e.addFileRoot(restoredSite); err != nil {
		return nil, err
	}
	committed = true
	return map[string]any{"site": restoredSite, "backup_id": in.BackupID, "restored": true}, nil
}

func syncRestoreTree(root *os.Root) error {
	directories := []string{}
	files := []string{}
	if err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, path)
		} else {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, path := range files {
		file, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if err = errors.Join(syncErr, closeErr); err != nil {
			return err
		}
	}
	sort.Slice(directories, func(i, j int) bool {
		return strings.Count(directories[i], string(filepath.Separator)) > strings.Count(directories[j], string(filepath.Separator))
	})
	for _, path := range directories {
		directory, err := root.Open(path)
		if err != nil {
			return err
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if err = errors.Join(syncErr, closeErr); err != nil {
			return err
		}
	}
	return nil
}
func (e *AgentExecutor) restoreSnapshots(ctx context.Context, sites ...Site) ([]changedFile, error) {
	paths := map[string]bool{}
	for _, site := range sites {
		paths[filepath.Join(e.config.StateDir, "sites", site.ID+".json")] = true
		paths[e.paths(site).PasswordFile] = true
		web, err := e.runtimes.Web(ctx, site.WebInstance)
		if err != nil {
			return nil, err
		}
		paths[filepath.Join(web.SnippetsDir, site.ID+".conf")] = true
		if site.PHPInstance != "" {
			php, err := e.runtimes.PHP(ctx, site.PHPInstance)
			if err != nil {
				return nil, err
			}
			paths[filepath.Join(php.PoolDir, site.ID+".conf")] = true
		}
	}
	changes := []changedFile{}
	for path := range paths {
		change, err := snapshotConfig(path)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, nil
}
func (e *AgentExecutor) backupMetadata(in request) (backupManifest, error) {
	var metadata backupManifest
	if !safeID.MatchString(in.ID) || !safeID.MatchString(in.BackupID) {
		return metadata, invalid("Invalid backup identity")
	}
	err := rootJSON(e.state, filepath.Join("backups", in.ID, in.BackupID+".json"), &metadata)
	if err != nil {
		return metadata, err
	}
	if (metadata.Version != 1 && metadata.Version != 2) || metadata.Site.ID != in.ID || metadata.Backup.SiteID != in.ID || metadata.Backup.ID != in.BackupID {
		return metadata, failure("ownership_conflict", "Backup belongs to another website", 409)
	}
	return metadata, nil
}
