package plugins

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

const (
	phpMaintenanceDefaultAge = int64(24 * time.Hour / time.Second)
	phpMaintenanceMinAge     = int64(time.Hour / time.Second)
	phpMaintenanceMaxAge     = int64(365 * 24 * time.Hour / time.Second)
	phpMaintenancePreviewTTL = 5 * time.Minute
	phpMaintenanceMaxFiles   = 4096
	phpMaintenanceMaxCleanup = 256
	phpMaintenanceMaxLog     = 256 << 10
)

var (
	phpSessionFilename      = regexp.MustCompile(`^sess_[A-Za-z0-9,-]{1,256}$`)
	phpMaintenancePreviewID = regexp.MustCompile(`^pmv_[a-f0-9]{48}$`)
)

// PHPMaintenanceInput is intentionally limited to named PHP maintenance
// operations. Paths, commands, session identifiers and log filenames are
// always derived from protected receipts and managed directory contents.
type PHPMaintenanceInput struct {
	ID               string `json:"id"`
	Command          string `json:"command"`
	Stream           string `json:"stream,omitempty"`
	Limit            int    `json:"limit,omitempty"`
	OlderThanSeconds int64  `json:"older_than_seconds,omitempty"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
	PreviewID        string `json:"preview_id,omitempty"`
}

type phpMaintenanceSessionFile struct {
	Name      string `json:"name"`
	Device    uint64 `json:"device"`
	Inode     uint64 `json:"inode"`
	Size      int64  `json:"size"`
	MTimeSec  int64  `json:"mtime_sec"`
	MTimeNsec int64  `json:"mtime_nsec"`
	CTimeSec  int64  `json:"ctime_sec"`
	CTimeNsec int64  `json:"ctime_nsec"`
	UID       uint32 `json:"uid"`
	GID       uint32 `json:"gid"`
	Mode      uint32 `json:"mode"`
	Links     uint64 `json:"links"`
}

type phpMaintenanceDirectory struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	UID    uint32 `json:"uid"`
	GID    uint32 `json:"gid"`
	Mode   uint32 `json:"mode"`
}

type phpMaintenancePreview struct {
	Version          int                          `json:"version"`
	ID               string                       `json:"id"`
	PreviewID        string                       `json:"preview_id"`
	Revision         int64                        `json:"revision"`
	OlderThanSeconds int64                        `json:"older_than_seconds"`
	CapturedAt       time.Time                    `json:"captured_at"`
	ExpiresAt        time.Time                    `json:"expires_at"`
	Directory        phpMaintenanceDirectory      `json:"directory"`
	Files            []phpMaintenanceSessionFile  `json:"files"`
	Completed        *phpMaintenanceCleanupResult `json:"completed,omitempty"`
}

type phpMaintenanceCleanupResult struct {
	ID           string    `json:"id"`
	Revision     int64     `json:"revision"`
	PreviewID    string    `json:"preview_id"`
	DeletedCount int       `json:"deleted_count"`
	DeletedBytes int64     `json:"deleted_bytes"`
	CompletedAt  time.Time `json:"completed_at"`
}

func validPHPMaintenanceAge(seconds int64) bool {
	return seconds >= phpMaintenanceMinAge && seconds <= phpMaintenanceMaxAge
}

func validPHPMaintenanceInput(in PHPMaintenanceInput) error {
	if !validInstanceID(in.ID) {
		return failure("invalid_input", "A valid PHP instance ID is required", 400)
	}
	switch in.Command {
	case "log_read":
		if in.Stream != "fpm_error" || in.Limit < 0 || in.Limit > 500 {
			return failure("invalid_input", "Only the bounded managed FPM error log can be read", 400)
		}
	case "session_stats":
		if in.OlderThanSeconds != 0 && !validPHPMaintenanceAge(in.OlderThanSeconds) {
			return failure("invalid_input", "Session age must be between one hour and one year", 400)
		}
	case "session_preview":
		if in.ExpectedRevision < 1 || !validPHPMaintenanceAge(in.OlderThanSeconds) {
			return failure("invalid_input", "A current PHP revision and bounded session age are required", 400)
		}
	case "session_cleanup":
		if in.ExpectedRevision < 1 || !phpMaintenancePreviewID.MatchString(in.PreviewID) {
			return failure("invalid_input", "A current PHP revision and immutable preview ID are required", 400)
		}
	default:
		return failure("invalid_input", "Unknown PHP maintenance action", 400)
	}
	return nil
}

// ValidatePHPMaintenance is called by the native Agent operation registry.
func (e *PrivilegedExecutor) ValidatePHPMaintenance(raw json.RawMessage) error {
	var input PHPMaintenanceInput
	if err := parse(raw, &input); err != nil {
		return err
	}
	return validPHPMaintenanceInput(input)
}

// RunPHPMaintenance is called by the native Agent operation registry.
func (e *PrivilegedExecutor) RunPHPMaintenance(ctx context.Context, raw json.RawMessage) (any, error) {
	var input PHPMaintenanceInput
	if err := parse(raw, &input); err != nil {
		return nil, err
	}
	if err := validPHPMaintenanceInput(input); err != nil {
		return nil, err
	}
	switch input.Command {
	case "log_read":
		return e.phpMaintenanceLog(ctx, input)
	case "session_stats":
		return e.phpMaintenanceSessionStats(ctx, input)
	case "session_preview":
		return e.phpMaintenanceSessionPreview(ctx, input)
	case "session_cleanup":
		return e.phpMaintenanceSessionCleanup(ctx, input)
	default:
		return nil, failure("invalid_input", "Unknown PHP maintenance action", 400)
	}
}

func (e *PrivilegedExecutor) phpMaintenanceInstance(ctx context.Context, id string) (Instance, error) {
	instance, err := e.get(ctx, id, false)
	if err != nil {
		return Instance{}, err
	}
	if !isPHPRuntime(instance.PluginID) || instance.Ownership != "managed" || instance.Status == "uninstalled" {
		return Instance{}, failure("runtime_type_mismatch", "An installed protected PHP runtime is required", 412)
	}
	return instance, nil
}

func (e *PrivilegedExecutor) phpMaintenanceLog(ctx context.Context, in PHPMaintenanceInput) (any, error) {
	instance, err := e.phpMaintenanceInstance(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	root, _, err := openPHPMaintenanceDirectory(instance.Paths.LogDir, e.uid, true)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"id": instance.ID, "stream": "fpm_error", "generated": false, "lines": []string{}, "revision": instance.Revision, "truncated": false}, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	limit := in.Limit
	if limit == 0 {
		limit = 100
	}
	// O_NONBLOCK is required because the name may be replaced with a FIFO
	// between directory validation and open. Stat below still rejects every
	// non-regular file; nonblocking open prevents that race from hanging.
	file, err := root.OpenFile("fpm-error.log", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"id": instance.ID, "stream": "fpm_error", "generated": false, "lines": []string{}, "revision": instance.Revision, "truncated": false}, nil
	}
	if err != nil {
		return nil, phpMaintenanceUnsafe("Managed FPM error log could not be opened safely")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !phpMaintenanceSafeLogFile(info, e.uid) {
		return nil, phpMaintenanceUnsafe("Managed FPM error log must be a private regular file")
	}
	pathInfo, err := root.Lstat("fpm-error.log")
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) {
		return nil, phpMaintenanceUnsafe("Managed FPM error log identity changed")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	start := max(int64(0), info.Size()-phpMaintenanceMaxLog)
	if _, err = file.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	readSize := info.Size() - start
	raw, err := io.ReadAll(io.LimitReader(file, readSize))
	if err != nil {
		return nil, err
	}
	truncated := start > 0
	if start > 0 {
		if newline := strings.IndexByte(string(raw), '\n'); newline >= 0 {
			raw = raw[newline+1:]
			truncated = true
		} else {
			raw = nil
		}
	}
	text := strings.TrimSuffix(string(raw), "\n")
	lines := []string{}
	if text != "" {
		lines = strings.Split(strings.ToValidUTF8(text, "�"), "\n")
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
		truncated = true
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"id": instance.ID, "stream": "fpm_error", "generated": true, "lines": lines, "revision": instance.Revision, "truncated": truncated}, nil
}

func phpMaintenanceSafeLogFile(info os.FileInfo, workerUID uint32) bool {
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() < 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return false
	}
	return stat.Uid == workerUID || stat.Uid == 0
}

func phpMaintenanceUnsafe(message string) error {
	return failure("session_directory_unsafe", message, 412)
}

func (e *PrivilegedExecutor) phpMaintenanceSessionStats(ctx context.Context, in PHPMaintenanceInput) (any, error) {
	unlock, err := e.lockContext(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err = e.ensureInstallFinalized(ctx, in.ID); err != nil {
		return nil, err
	}
	instance, err := e.phpMaintenanceInstance(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	age := in.OlderThanSeconds
	if age == 0 {
		age = phpMaintenanceDefaultAge
	}
	root, _, err := e.phpSessionDirectory(instance)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"id": instance.ID, "revision": instance.Revision, "generated": false, "total_sessions": 0, "expired_sessions": 0, "older_than_seconds": age, "allow_actions": []string{}}, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	files, err := scanPHPSessions(ctx, root, e.uid, e.gid)
	if err != nil {
		return nil, err
	}
	counts := phpSessionCounts(files, age, time.Now().UTC())
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"id": instance.ID, "revision": instance.Revision, "generated": true, "total_sessions": counts.Total, "expired_sessions": counts.Expired, "older_than_seconds": age, "oldest_age_seconds": counts.OldestAgeSeconds, "allow_actions": []string{"preview"}}, nil
}

type phpSessionCount struct {
	Total            int
	Expired          int
	OldestAgeSeconds int64
}

func phpSessionCounts(files []phpMaintenanceSessionFile, ageSeconds int64, now time.Time) phpSessionCount {
	var out phpSessionCount
	cutoff := now.Add(-time.Duration(ageSeconds) * time.Second)
	for _, file := range files {
		out.Total++
		modified := time.Unix(file.MTimeSec, file.MTimeNsec)
		currentAge := max(int64(0), int64(now.Sub(modified)/time.Second))
		if currentAge > out.OldestAgeSeconds {
			out.OldestAgeSeconds = currentAge
		}
		if !modified.After(cutoff) {
			out.Expired++
		}
	}
	return out
}

func (e *PrivilegedExecutor) phpMaintenanceSessionPreview(ctx context.Context, in PHPMaintenanceInput) (any, error) {
	unlock, err := e.lockContext(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err = e.ensureInstallFinalized(ctx, in.ID); err != nil {
		return nil, err
	}
	instance, err := e.phpMaintenanceInstance(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if instance.Revision != in.ExpectedRevision {
		return nil, failure("revision_conflict", "PHP instance changed after the session preview was requested", 412)
	}
	root, directory, err := e.phpSessionDirectory(instance)
	if errors.Is(err, os.ErrNotExist) {
		return nil, failure("session_directory_unavailable", "The managed PHP files session directory has not been generated", 412)
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	files, err := scanPHPSessions(ctx, root, e.uid, e.gid)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	cutoff := now.Add(-time.Duration(in.OlderThanSeconds) * time.Second)
	candidates := make([]phpMaintenanceSessionFile, 0, len(files))
	for _, file := range files {
		if !time.Unix(file.MTimeSec, file.MTimeNsec).After(cutoff) {
			candidates = append(candidates, file)
		}
	}
	if len(candidates) > phpMaintenanceMaxCleanup {
		return nil, failure("session_cleanup_limit", "Session cleanup preview exceeds its bounded deletion limit", 413)
	}
	previewToken, err := randomPHPPreviewID()
	if err != nil {
		return nil, err
	}
	preview := phpMaintenancePreview{Version: 1, ID: instance.ID, PreviewID: previewToken, Revision: instance.Revision, OlderThanSeconds: in.OlderThanSeconds, CapturedAt: now, ExpiresAt: now.Add(phpMaintenancePreviewTTL), Directory: directory, Files: candidates}
	if err = e.save("private", phpMaintenanceStateKey(instance.ID), preview); err != nil {
		return nil, err
	}
	counts := phpSessionCounts(files, in.OlderThanSeconds, now)
	allow := []string{}
	if len(candidates) > 0 {
		allow = []string{"cleanup"}
	}
	return map[string]any{"id": instance.ID, "revision": instance.Revision, "preview_id": previewToken, "captured_at": now, "expires_at": preview.ExpiresAt, "total_sessions": counts.Total, "candidate_count": len(candidates), "candidate_bytes": phpSessionBytes(candidates), "older_than_seconds": in.OlderThanSeconds, "allow_actions": allow}, nil
}

func phpSessionBytes(files []phpMaintenanceSessionFile) int64 {
	var total int64
	for _, file := range files {
		if file.Size > 0 && total <= int64(^uint64(0)>>1)-file.Size {
			total += file.Size
		}
	}
	return total
}

func randomPHPPreviewID() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "pmv_" + hex.EncodeToString(raw[:]), nil
}

func phpMaintenanceStateKey(instanceID string) string {
	digest := sha256.Sum256([]byte(instanceID))
	return "phpm_" + hex.EncodeToString(digest[:])
}

func (e *PrivilegedExecutor) phpMaintenanceSessionCleanup(ctx context.Context, in PHPMaintenanceInput) (any, error) {
	unlock, err := e.lockContext(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err = e.ensureInstallFinalized(ctx, in.ID); err != nil {
		return nil, err
	}
	instance, err := e.phpMaintenanceInstance(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if instance.Revision != in.ExpectedRevision {
		return nil, failure("revision_conflict", "PHP instance changed after the session preview was requested", 412)
	}
	var preview phpMaintenancePreview
	if err = e.load("private", phpMaintenanceStateKey(instance.ID), &preview); err != nil {
		return nil, failure("preview_expired", "Session cleanup requires a current immutable preview", 412)
	}
	if preview.ID != instance.ID || preview.PreviewID != in.PreviewID || preview.Revision != in.ExpectedRevision || preview.Version != 1 {
		return nil, failure("preview_stale", "Session preview does not match this PHP instance revision", 412)
	}
	if time.Now().After(preview.ExpiresAt) {
		return nil, failure("preview_expired", "Session cleanup preview has expired", 412)
	}
	if preview.Completed != nil {
		return *preview.Completed, nil
	}
	if len(preview.Files) == 0 {
		return nil, failure("preview_not_actionable", "The preview contains no expired sessions to clean", 412)
	}
	root, directory, err := e.phpSessionDirectory(instance)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if directory != preview.Directory {
		return nil, failure("preview_stale", "Managed session directory identity changed after preview", 412)
	}
	currentFiles, err := scanPHPSessions(ctx, root, e.uid, e.gid)
	if err != nil {
		return nil, err
	}
	currentByName := make(map[string]phpMaintenanceSessionFile, len(currentFiles))
	for _, file := range currentFiles {
		currentByName[file.Name] = file
	}
	for _, expected := range preview.Files {
		current, ok := currentByName[expected.Name]
		if !ok {
			continue // A prior interrupted run may already have removed this exact target.
		}
		if current != expected {
			return nil, failure("preview_stale", "A previewed session changed; create a fresh preview before cleanup", 412)
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	locked := make([]*phpLockedSession, 0, len(preview.Files))
	defer func() {
		for i := len(locked) - 1; i >= 0; i-- {
			locked[i].close()
		}
	}()
	for _, expected := range preview.Files {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		item, openErr := openPHPLockedSession(root, expected, e.uid, e.gid)
		if errors.Is(openErr, os.ErrNotExist) {
			continue
		}
		if openErr != nil {
			if core.ErrorCode(openErr) == "preview_stale" {
				return nil, openErr
			}
			return nil, openErr
		}
		locked = append(locked, item)
	}
	result := phpMaintenanceCleanupResult{ID: instance.ID, Revision: instance.Revision, PreviewID: preview.PreviewID}
	for _, item := range locked {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if err = removePHPLockedSession(root, item); err != nil {
			return nil, err
		}
		result.DeletedCount++
		result.DeletedBytes += item.identity.Size
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	result.CompletedAt = time.Now().UTC()
	preview.Completed = &result
	if err = e.save("private", phpMaintenanceStateKey(instance.ID), preview); err != nil {
		return nil, failure("persistence_failed", "Session cleanup completed but its idempotent result could not be saved", 500)
	}
	return result, nil
}

type phpLockedSession struct {
	file     *os.File
	identity phpMaintenanceSessionFile
}

func (s *phpLockedSession) close() {
	if s == nil || s.file == nil {
		return
	}
	_ = syscall.Flock(int(s.file.Fd()), syscall.LOCK_UN)
	_ = s.file.Close()
	s.file = nil
}

func openPHPLockedSession(root *os.Root, expected phpMaintenanceSessionFile, uid, gid uint32) (*phpLockedSession, error) {
	// A previewed session can be swapped for a FIFO before open. Keep this
	// nonblocking, then verify regular-file type and the captured identity.
	file, err := root.OpenFile(expected.Name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	identity, err := phpSessionIdentity(expected.Name, info, uid, gid)
	if err != nil || identity != expected {
		_ = file.Close()
		return nil, failure("preview_stale", "A previewed session changed; create a fresh preview before cleanup", 412)
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, failure("session_busy", "A PHP request is using a previewed session; create a fresh preview later", 409)
		}
		return nil, err
	}
	currentInfo, statErr := root.Lstat(expected.Name)
	openedInfo, openedErr := file.Stat()
	if statErr != nil || openedErr != nil || currentInfo == nil || openedInfo == nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, failure("preview_stale", "A previewed session changed; create a fresh preview before cleanup", 412)
	}
	currentIdentity, identityErr := phpSessionIdentity(expected.Name, currentInfo, uid, gid)
	if identityErr != nil || currentIdentity != expected || !os.SameFile(openedInfo, currentInfo) {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, failure("preview_stale", "A previewed session changed; create a fresh preview before cleanup", 412)
	}
	return &phpLockedSession{file: file, identity: expected}, nil
}

func removePHPLockedSession(root *os.Root, item *phpLockedSession) error {
	openedInfo, err := item.file.Stat()
	if err != nil {
		return err
	}
	pathInfo, err := root.Lstat(item.identity.Name)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, pathInfo) {
		return failure("preview_stale", "A previewed session path changed; new data was preserved", 412)
	}
	temporary, err := randomPHPSessionQuarantine()
	if err != nil {
		return err
	}
	if err = root.Rename(item.identity.Name, temporary); err != nil {
		return failure("preview_stale", "A previewed session could not be isolated safely; create a fresh preview", 412)
	}
	quarantine, statErr := root.Lstat(temporary)
	if statErr != nil || quarantine.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, quarantine) {
		// Never unlink a replacement. Restore it only if no new session has taken
		// the old name; otherwise leave it recoverable under its private name.
		if _, existsErr := root.Lstat(item.identity.Name); errors.Is(existsErr, os.ErrNotExist) {
			_ = root.Rename(temporary, item.identity.Name)
		}
		return failure("preview_stale", "A previewed session changed while cleanup was isolating it; new data was preserved", 412)
	}
	quarantinedIdentity, identityErr := phpSessionIdentity(item.identity.Name, quarantine, item.identity.UID, item.identity.GID)
	if identityErr != nil || !samePHPSessionIdentityExceptCTime(quarantinedIdentity, item.identity) {
		if _, existsErr := root.Lstat(item.identity.Name); errors.Is(existsErr, os.ErrNotExist) {
			_ = root.Rename(temporary, item.identity.Name)
		}
		return failure("preview_stale", "A previewed session changed while cleanup was isolating it; new data was preserved", 412)
	}
	if err = root.Remove(temporary); err != nil {
		if _, existsErr := root.Lstat(item.identity.Name); errors.Is(existsErr, os.ErrNotExist) {
			_ = root.Rename(temporary, item.identity.Name)
		}
		return failure("session_cleanup_failed", "A stale PHP session could not be removed safely", 500)
	}
	return nil
}

func samePHPSessionIdentityExceptCTime(a, b phpMaintenanceSessionFile) bool {
	a.CTimeSec, a.CTimeNsec = 0, 0
	b.CTimeSec, b.CTimeNsec = 0, 0
	return a == b
}

func randomPHPSessionQuarantine() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return ".php-maint-" + hex.EncodeToString(raw[:]), nil
}

func (e *PrivilegedExecutor) phpSessionDirectory(instance Instance) (*os.Root, phpMaintenanceDirectory, error) {
	path := filepath.Join(instance.Paths.DataDir, "sessions")
	if handler, ok := instance.Config["session_handler"].(string); ok && handler != "" && handler != "files" {
		return nil, phpMaintenanceDirectory{}, failure("unsupported_session_handler", "Only the managed PHP files session handler can be inspected or cleaned", 412)
	}
	if configuredPath, ok := instance.Config["session_path"].(string); ok && configuredPath != "" && configuredPath != path {
		return nil, phpMaintenanceDirectory{}, failure("session_directory_unsafe", "Only the managed PHP files session directory can be inspected or cleaned", 412)
	}
	root, identity, err := openPHPMaintenanceDirectory(path, e.uid, true)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, phpMaintenanceDirectory{}, os.ErrNotExist
		}
		return nil, phpMaintenanceDirectory{}, phpMaintenanceUnsafe("Managed PHP session storage must be a private real directory owned by its runtime worker")
	}
	return root, identity, nil
}

func openPHPMaintenanceDirectory(path string, owner uint32, private bool) (*os.Root, phpMaintenanceDirectory, error) {
	if err := realDirectory(path); err != nil {
		return nil, phpMaintenanceDirectory{}, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, phpMaintenanceDirectory{}, err
	}
	info, err := root.Stat(".")
	pathInfo, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !info.IsDir() || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) {
		_ = root.Close()
		return nil, phpMaintenanceDirectory{}, phpMaintenanceUnsafe("Managed PHP directory identity changed")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || stat.Uid != owner || info.Mode().Perm()&0022 != 0 || (private && info.Mode().Perm()&0077 != 0) {
		_ = root.Close()
		return nil, phpMaintenanceDirectory{}, phpMaintenanceUnsafe("Managed PHP directory ownership or permissions are unsafe")
	}
	return root, phpMaintenanceDirectory{Device: uint64(stat.Dev), Inode: stat.Ino, UID: stat.Uid, GID: stat.Gid, Mode: uint32(info.Mode().Perm())}, nil
}

func scanPHPSessions(ctx context.Context, root *os.Root, uid, gid uint32) ([]phpMaintenanceSessionFile, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, phpMaintenanceUnsafe("Managed PHP session directory could not be scanned safely")
	}
	defer directory.Close()
	entries, err := directory.ReadDir(phpMaintenanceMaxFiles + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, phpMaintenanceUnsafe("Managed PHP session directory could not be scanned safely")
	}
	if len(entries) > phpMaintenanceMaxFiles {
		return nil, failure("session_scan_limit", "Managed PHP session directory exceeds its bounded scan limit", 413)
	}
	files := make([]phpMaintenanceSessionFile, 0, len(entries))
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if !phpSessionFilename.MatchString(entry.Name()) {
			return nil, phpMaintenanceUnsafe("Managed PHP session directory contains an unexpected entry")
		}
		info, statErr := root.Lstat(entry.Name())
		if statErr != nil {
			return nil, phpMaintenanceUnsafe("Managed PHP session entry changed during scan")
		}
		identity, identityErr := phpSessionIdentity(entry.Name(), info, uid, gid)
		if identityErr != nil {
			return nil, phpMaintenanceUnsafe("Managed PHP session entries must be private, single-link regular files owned by the runtime worker")
		}
		files = append(files, identity)
	}
	slices.SortFunc(files, func(a, b phpMaintenanceSessionFile) int { return strings.Compare(a.Name, b.Name) })
	return files, nil
}

func phpSessionIdentity(name string, info os.FileInfo, uid, gid uint32) (phpMaintenanceSessionFile, error) {
	var identity phpMaintenanceSessionFile
	if !phpSessionFilename.MatchString(name) || info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return identity, errors.New("unsafe PHP session entry")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uid || stat.Gid != gid || stat.Nlink != 1 || stat.Size < 0 {
		return identity, errors.New("PHP session entry ownership or link count is unsafe")
	}
	return phpMaintenanceSessionFile{Name: name, Device: uint64(stat.Dev), Inode: stat.Ino, Size: stat.Size, MTimeSec: stat.Mtim.Sec, MTimeNsec: stat.Mtim.Nsec, CTimeSec: stat.Ctim.Sec, CTimeNsec: stat.Ctim.Nsec, UID: stat.Uid, GID: stat.Gid, Mode: uint32(info.Mode().Perm()), Links: uint64(stat.Nlink)}, nil
}

func (m *Manager) phpMaintenanceBindings() []core.Binding {
	result := []core.Binding{}
	for _, spec := range []struct {
		name, command, scope string
		readOnly, task       bool
	}{
		{"php_maintenance_log_read", "log_read", "php:read", true, false},
		{"php_maintenance_session_stats", "session_stats", "php:read", true, false},
		{"php_maintenance_session_preview", "session_preview", "php:read", false, false},
		{"php_maintenance_session_cleanup", "session_cleanup", "php:manage", false, true},
	} {
		fields := map[string]any{"id": idSchema()}
		required := []string{"id"}
		switch spec.command {
		case "log_read":
			fields["stream"] = enumSchema("fpm_error")
			fields["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 500}
			required = append(required, "stream")
		case "session_stats":
			fields["older_than_seconds"] = map[string]any{"type": "integer", "minimum": phpMaintenanceMinAge, "maximum": phpMaintenanceMaxAge}
		case "session_preview":
			fields["expected_revision"] = revisionSchema()
			fields["older_than_seconds"] = map[string]any{"type": "integer", "minimum": phpMaintenanceMinAge, "maximum": phpMaintenanceMaxAge}
			required = append(required, "expected_revision", "older_than_seconds")
		case "session_cleanup":
			fields["expected_revision"] = revisionSchema()
			fields["preview_id"] = map[string]any{"type": "string", "pattern": phpMaintenancePreviewID.String(), "maxLength": 52}
			required = append(required, "expected_revision", "preview_id")
		}
		command, scope, readOnly, task := spec.command, spec.scope, spec.readOnly, spec.task
		binding := m.binding(spec.name, "Read bounded owned PHP logs and inspect or clean private files-handler sessions", scope, readOnly, obj(fields, required...), idResource, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			var input PHPMaintenanceInput
			if err := parse(raw, &input); err != nil {
				return core.ActionResult{}, err
			}
			input.Command = command
			forward, err := json.Marshal(input)
			if err != nil {
				return core.ActionResult{}, err
			}
			resources := []string{InstanceResource(input.ID)}
			if task {
				job, submitErr := m.deps.Tasks.Submit(ctx, actor, "php_maintenance", forward, resources)
				if submitErr != nil {
					return core.ActionResult{}, submitErr
				}
				taskID, _ := job["task_id"].(string)
				if taskID == "" {
					taskID, _ = job["id"].(string)
				}
				return core.ActionResult{Data: job, TaskID: taskID, Resources: resources, AuditSummary: map[string]any{"id": input.ID, "command": command}}, nil
			}
			var output any
			if err := m.agentJSON(ctx, "php_maintenance", forward, &output); err != nil {
				return core.ActionResult{}, err
			}
			return core.ActionResult{Data: output, Resources: resources, AuditSummary: map[string]any{"id": input.ID, "command": command}}, nil
		})
		binding.Action.Task = task
		result = append(result, binding)
	}
	return result
}
