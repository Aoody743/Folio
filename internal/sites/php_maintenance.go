package sites

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/phpfilesessions"
)

const (
	sitePHPDefaultAge   = int64(24 * time.Hour / time.Second)
	sitePHPMinAge       = int64(time.Hour / time.Second)
	sitePHPMaxAge       = int64(365 * 24 * time.Hour / time.Second)
	sitePHPPreviewTTL   = 5 * time.Minute
	sitePHPMaxEntries   = 4096
	sitePHPMaxCleanup   = 256
	sitePHPMaxLogBytes  = 256 << 10
	sitePHPMaxPoolBytes = 256 << 10
	sitePHPMaxFileBytes = 256 << 10
	sitePHPMaxScanFiles = 128
	sitePHPMaxScanBytes = 1 << 20
	sitePHPMaxLogLines  = 500
)

var sitePHPPreviewID = regexp.MustCompile(`^spv_[a-f0-9]{48}$`)

type sitePHPPreview struct {
	Version          int                               `json:"version"`
	SiteID           string                            `json:"site_id"`
	ActorID          string                            `json:"actor_id"`
	PreviewID        string                            `json:"preview_id"`
	SiteRevision     int64                             `json:"site_revision"`
	PHPInstance      string                            `json:"php_instance"`
	Worker           Identity                          `json:"worker"`
	Directory        phpfilesessions.DirectoryIdentity `json:"directory"`
	OlderThanSeconds int64                             `json:"older_than_seconds"`
	CapturedAt       time.Time                         `json:"captured_at"`
	ExpiresAt        time.Time                         `json:"expires_at"`
	Files            []phpfilesessions.FileIdentity    `json:"files"`
	Completed        *sitePHPSessionCleanupResult      `json:"completed,omitempty"`
}

type sitePHPSessionCleanupResult struct {
	SiteID              string    `json:"site_id"`
	SiteRevision        int64     `json:"site_revision"`
	PHPInstance         string    `json:"php_instance"`
	PreviewID           string    `json:"preview_id"`
	DeletedCount        int       `json:"deleted_count"`
	SkippedMissingCount int       `json:"skipped_missing_count"`
	DeletedBytes        int64     `json:"deleted_bytes"`
	CompletedAt         time.Time `json:"completed_at"`
}

type sitePoolConfig struct {
	values map[string][]string
}

func validSitePHPAge(age int64) bool { return age >= sitePHPMinAge && age <= sitePHPMaxAge }

func validateSitePHPAgentRequest(operation string, raw []byte, in request) error {
	allowed := map[string]bool{"actor": true, "id": true}
	switch operation {
	case "site_php_slowlog_read":
		allowed["limit"] = true
		if in.Limit < 0 || in.Limit > sitePHPMaxLogLines || in.ExpectedRevision != 0 || in.OlderThanSeconds != 0 || in.PreviewID != "" {
			return agent.ErrInvalidInput
		}
	case "site_php_session_stats":
		allowed["older_than_seconds"] = true
		if in.ExpectedRevision != 0 || in.PreviewID != "" || in.Limit != 0 || in.OlderThanSeconds != 0 && !validSitePHPAge(in.OlderThanSeconds) {
			return agent.ErrInvalidInput
		}
	case "site_php_session_preview":
		allowed["expected_revision"], allowed["older_than_seconds"] = true, true
		if in.ExpectedRevision < 1 || !validSitePHPAge(in.OlderThanSeconds) || in.PreviewID != "" || in.Limit != 0 {
			return agent.ErrInvalidInput
		}
	case "site_php_session_cleanup":
		allowed["expected_revision"], allowed["preview_id"] = true, true
		if in.ExpectedRevision < 1 || !sitePHPPreviewID.MatchString(in.PreviewID) || in.OlderThanSeconds != 0 || in.Limit != 0 {
			return agent.ErrInvalidInput
		}
	default:
		return agent.ErrUnauthorized
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return agent.ErrInvalidInput
	}
	for key := range fields {
		if !allowed[key] {
			return agent.ErrInvalidInput
		}
	}
	return nil
}

func (e *AgentExecutor) sitePHPSlowlog(ctx context.Context, in request) (any, error) {
	site, worker, runtime, pool, err := e.sitePHPConfig(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if err = verifySitePoolSlowlog(site, pool, e.paths(site)); err != nil {
		return nil, err
	}
	if !site.PHP.SlowlogEnabled {
		return map[string]any{"site_id": site.ID, "site_revision": site.Revision, "php_instance": site.PHPInstance, "configured": false, "generated": false, "lines": []string{}, "truncated": false}, nil
	}
	root, err := e.openSitePHPLogDirectory(site)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	limit := in.Limit
	if limit == 0 {
		limit = 100
	}
	file, err := root.OpenFile("php-slow.log", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"site_id": site.ID, "site_revision": site.Revision, "php_instance": runtime.ID, "configured": true, "generated": false, "lines": []string{}, "truncated": false}, nil
	}
	if err != nil {
		return nil, failure("php_slowlog_unsafe", "Managed site PHP slowlog could not be opened safely", 412)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !sitePHPLogFileSafe(info, worker.UID) {
		return nil, failure("php_slowlog_unsafe", "Managed site PHP slowlog must be a protected regular file", 412)
	}
	pathInfo, err := root.Lstat("php-slow.log")
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) {
		return nil, failure("php_slowlog_unsafe", "Managed site PHP slowlog identity changed", 412)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	start := max(int64(0), info.Size()-sitePHPMaxLogBytes)
	if _, err = file.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, info.Size()-start))
	if err != nil {
		return nil, err
	}
	truncated := start > 0
	if start > 0 {
		if newline := strings.IndexByte(string(raw), '\n'); newline >= 0 {
			raw = raw[newline+1:]
		} else {
			raw = nil
		}
	}
	lines := []string{}
	text := strings.TrimSuffix(strings.ToValidUTF8(string(raw), "�"), "\n")
	if text != "" {
		lines = strings.Split(text, "\n")
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
		truncated = true
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"site_id": site.ID, "site_revision": site.Revision, "php_instance": runtime.ID, "configured": true, "generated": true, "lines": lines, "truncated": truncated}, nil
}

func sitePHPLogFileSafe(info os.FileInfo, workerUID uint32) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() < 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1 && (stat.Uid == uint32(os.Geteuid()) || stat.Uid == workerUID)
}

func (e *AgentExecutor) sitePHPSessionStats(ctx context.Context, in request) (any, error) {
	site, worker, runtime, pool, err := e.sitePHPConfig(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if err = e.verifySiteSessionHandler(ctx, site, runtime, pool); err != nil {
		return nil, err
	}
	root, directory, err := e.openSitePHPTmp(site, worker)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"site_id": site.ID, "site_revision": site.Revision, "php_instance": runtime.ID, "generated": false, "total_sessions": 0, "expired_sessions": 0, "older_than_seconds": sitePHPStatsAge(in.OlderThanSeconds), "oldest_age_seconds": int64(0), "allow_actions": []string{}}, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	files, err := scanSitePHPSessions(ctx, root, worker)
	if err != nil {
		return nil, err
	}
	age := sitePHPStatsAge(in.OlderThanSeconds)
	counts := sitePHPSessionCounts(files, age, time.Now().UTC())
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	allow := []string{}
	if counts.Expired > 0 {
		allow = []string{"preview"}
	}
	_ = directory
	return map[string]any{"site_id": site.ID, "site_revision": site.Revision, "php_instance": runtime.ID, "generated": true, "total_sessions": counts.Total, "expired_sessions": counts.Expired, "older_than_seconds": age, "oldest_age_seconds": counts.OldestAgeSeconds, "allow_actions": allow}, nil
}

func sitePHPStatsAge(value int64) int64 {
	if value == 0 {
		return sitePHPDefaultAge
	}
	return value
}

type sitePHPSessionCount struct {
	Total            int
	Expired          int
	OldestAgeSeconds int64
}

func sitePHPSessionCounts(files []phpfilesessions.FileIdentity, ageSeconds int64, now time.Time) sitePHPSessionCount {
	var out sitePHPSessionCount
	cutoff := now.Add(-time.Duration(ageSeconds) * time.Second)
	for _, item := range files {
		out.Total++
		modified := time.Unix(item.MTimeSec, item.MTimeNsec)
		age := max(int64(0), int64(now.Sub(modified)/time.Second))
		if age > out.OldestAgeSeconds {
			out.OldestAgeSeconds = age
		}
		if !modified.After(cutoff) {
			out.Expired++
		}
	}
	return out
}

func (e *AgentExecutor) sitePHPSessionPreview(ctx context.Context, in request) (any, error) {
	site, worker, runtime, pool, err := e.sitePHPConfig(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if site.Revision != in.ExpectedRevision {
		return nil, failure("site_revision_conflict", "Website changed after the session preview was requested", 412)
	}
	if err = e.verifySiteSessionHandler(ctx, site, runtime, pool); err != nil {
		return nil, err
	}
	root, directory, err := e.openSitePHPTmp(site, worker)
	if errors.Is(err, os.ErrNotExist) {
		return nil, failure("session_directory_unavailable", "The managed site PHP session directory has not been generated", 412)
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	files, err := scanSitePHPSessions(ctx, root, worker)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	cutoff := now.Add(-time.Duration(in.OlderThanSeconds) * time.Second)
	candidates := make([]phpfilesessions.FileIdentity, 0, len(files))
	for _, item := range files {
		if !time.Unix(item.MTimeSec, item.MTimeNsec).After(cutoff) {
			candidates = append(candidates, item)
		}
	}
	if len(candidates) > sitePHPMaxCleanup {
		return nil, failure("session_cleanup_limit", "Session preview exceeds its bounded deletion count", 413)
	}
	bytes, ok := sitePHPSessionBytes(candidates)
	if !ok || bytes > 1<<30 {
		return nil, failure("session_cleanup_limit", "Session preview exceeds its bounded cleanup byte count", 413)
	}
	token, err := newSitePHPPreviewID()
	if err != nil {
		return nil, err
	}
	preview := sitePHPPreview{Version: 1, SiteID: site.ID, ActorID: in.Actor.ID, PreviewID: token, SiteRevision: site.Revision, PHPInstance: runtime.ID, Worker: worker, Directory: directory, OlderThanSeconds: in.OlderThanSeconds, CapturedAt: now, ExpiresAt: now.Add(sitePHPPreviewTTL), Files: candidates}
	if err = e.saveSitePHPPreview(preview); err != nil {
		return nil, err
	}
	counts := sitePHPSessionCounts(files, in.OlderThanSeconds, now)
	allow := []string{}
	if len(candidates) > 0 {
		allow = []string{"cleanup"}
	}
	return map[string]any{"site_id": site.ID, "site_revision": site.Revision, "php_instance": runtime.ID, "preview_id": token, "captured_at": now, "expires_at": preview.ExpiresAt, "total_sessions": counts.Total, "candidate_count": len(candidates), "candidate_bytes": bytes, "older_than_seconds": in.OlderThanSeconds, "allow_actions": allow}, nil
}

func sitePHPSessionBytes(files []phpfilesessions.FileIdentity) (int64, bool) {
	var total int64
	for _, file := range files {
		if file.Size < 0 || total > int64(^uint64(0)>>1)-file.Size {
			return 0, false
		}
		total += file.Size
	}
	return total, true
}

func newSitePHPPreviewID() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "spv_" + hex.EncodeToString(raw[:]), nil
}

func sitePHPPreviewKey(siteID string) string {
	sum := sha256.Sum256([]byte(siteID))
	return "site-php-" + hex.EncodeToString(sum[:]) + ".json"
}

func (e *AgentExecutor) saveSitePHPPreview(preview sitePHPPreview) error {
	if preview.SiteID == "" || len(preview.Files) > sitePHPMaxCleanup {
		return failure("session_preview_invalid", "Session preview exceeds its protected state limit", 413)
	}
	raw := encode(preview)
	if len(raw) > 256<<10 {
		return failure("session_preview_invalid", "Session preview exceeds its protected state limit", 413)
	}
	return atomicRoot(e.state, filepath.Join("php-sessions", sitePHPPreviewKey(preview.SiteID)), raw, 0600)
}

func (e *AgentExecutor) loadSitePHPPreview(siteID string, preview *sitePHPPreview) error {
	return rootJSON(e.state, filepath.Join("php-sessions", sitePHPPreviewKey(siteID)), preview)
}

func (e *AgentExecutor) sitePHPSessionCleanup(ctx context.Context, in request) (any, error) {
	site, worker, runtime, pool, err := e.sitePHPConfig(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if site.Revision != in.ExpectedRevision {
		return nil, failure("site_revision_conflict", "Website changed after the session preview was requested", 412)
	}
	if err = e.verifySiteSessionHandler(ctx, site, runtime, pool); err != nil {
		return nil, err
	}
	var preview sitePHPPreview
	if err = e.loadSitePHPPreview(site.ID, &preview); err != nil {
		return nil, failure("preview_expired", "Session cleanup needs a current immutable preview", 412)
	}
	if preview.Version != 1 || preview.SiteID != site.ID || preview.ActorID != in.Actor.ID || preview.PreviewID != in.PreviewID || preview.SiteRevision != in.ExpectedRevision || preview.PHPInstance != runtime.ID || preview.Worker != worker {
		return nil, failure("preview_stale", "Session preview does not match this site, actor, runtime or revision", 412)
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
	root, directory, err := e.openSitePHPTmp(site, worker)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if directory != preview.Directory {
		return nil, failure("preview_stale", "Managed site PHP session directory changed after preview", 412)
	}
	currentFiles, err := scanSitePHPSessions(ctx, root, worker)
	if err != nil {
		return nil, err
	}
	currentByName := make(map[string]phpfilesessions.FileIdentity, len(currentFiles))
	for _, item := range currentFiles {
		currentByName[item.Name] = item
	}
	for _, expected := range preview.Files {
		current, ok := currentByName[expected.Name]
		if !ok {
			continue
		}
		if current != expected {
			return nil, failure("preview_stale", "A previewed session changed; create a fresh preview", 412)
		}
	}
	locked := make([]*phpfilesessions.LockedFile, 0, len(preview.Files))
	defer func() {
		for i := len(locked) - 1; i >= 0; i-- {
			locked[i].Close()
		}
	}()
	missing := 0
	for _, expected := range preview.Files {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if _, exists := currentByName[expected.Name]; !exists {
			missing++
			continue
		}
		item, lockErr := phpfilesessions.Lock(root, expected, worker.UID, worker.GID)
		if errors.Is(lockErr, os.ErrNotExist) {
			missing++
			continue
		}
		if errors.Is(lockErr, phpfilesessions.ErrBusy) {
			return nil, failure("session_busy", "A PHP request is using a previewed session; create a new preview later", 409)
		}
		if errors.Is(lockErr, phpfilesessions.ErrStale) {
			return nil, failure("preview_stale", "A previewed session changed; create a fresh preview", 412)
		}
		if lockErr != nil {
			return nil, lockErr
		}
		locked = append(locked, item)
	}
	quarantine, err := phpfilesessions.EnsureQuarantine(root, ".powerlinux-session-quarantine")
	if err != nil {
		if errors.Is(err, phpfilesessions.ErrUnsafe) {
			return nil, failure("session_directory_unsafe", "Session quarantine is not a private managed directory", 412)
		}
		return nil, err
	}
	defer quarantine.Close()
	result := sitePHPSessionCleanupResult{SiteID: site.ID, SiteRevision: site.Revision, PHPInstance: runtime.ID, PreviewID: preview.PreviewID, SkippedMissingCount: missing}
	for _, item := range locked {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		name, nameErr := newSitePHPQuarantineName()
		if nameErr != nil {
			return nil, nameErr
		}
		if err = phpfilesessions.Remove(root, quarantine, item, name); errors.Is(err, phpfilesessions.ErrStale) {
			return nil, failure("preview_stale", "A previewed session path changed; new data was preserved", 412)
		} else if errors.Is(err, phpfilesessions.ErrUnsafe) {
			return nil, failure("session_directory_unsafe", "Session quarantine is not private on the session filesystem", 412)
		} else if errors.Is(err, phpfilesessions.ErrRemove) {
			return nil, failure("session_cleanup_failed", "A stale PHP session could not be removed safely", 500)
		} else if err != nil {
			return nil, err
		}
		result.DeletedCount++
		result.DeletedBytes += item.Identity.Size
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	result.CompletedAt = time.Now().UTC()
	preview.Completed = &result
	if err = e.saveSitePHPPreview(preview); err != nil {
		return nil, failure("persistence_failed", "Session cleanup completed but its idempotent result could not be saved", 500)
	}
	return result, nil
}

func newSitePHPQuarantineName() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return ".php-session-" + hex.EncodeToString(raw[:]), nil
}

func scanSitePHPSessions(ctx context.Context, root *os.Root, worker Identity) ([]phpfilesessions.FileIdentity, error) {
	files, err := phpfilesessions.Scan(ctx, root, phpfilesessions.ScanOptions{UID: worker.UID, GID: worker.GID, MaxEntries: sitePHPMaxEntries, IgnoreNonSessionFiles: true})
	if errors.Is(err, phpfilesessions.ErrScanLimit) {
		return nil, failure("session_scan_limit", "Site PHP tmp directory exceeds the bounded scan limit", 413)
	}
	if errors.Is(err, phpfilesessions.ErrUnsafe) {
		return nil, failure("session_directory_unsafe", "Site PHP tmp directory contains an unsafe session entry", 412)
	}
	return files, err
}

func (e *AgentExecutor) sitePHPConfig(ctx context.Context, siteID string) (Site, Identity, PHPRuntime, sitePoolConfig, error) {
	site, err := e.get(siteID)
	if err != nil {
		return Site{}, Identity{}, PHPRuntime{}, sitePoolConfig{}, err
	}
	if !safeID.MatchString(site.PHPInstance) {
		return Site{}, Identity{}, PHPRuntime{}, sitePoolConfig{}, failure("php_runtime_required", "This website has no managed PHP runtime", 412)
	}
	worker, err := e.migrationWorkerIdentity(site.ID)
	if err != nil {
		return Site{}, Identity{}, PHPRuntime{}, sitePoolConfig{}, err
	}
	runtime, err := e.runtimes.PHP(ctx, site.PHPInstance)
	if err != nil {
		return Site{}, Identity{}, PHPRuntime{}, sitePoolConfig{}, err
	}
	if _, err = migrationPHPIdentityHash(runtime, site.PHPInstance); err != nil {
		return Site{}, Identity{}, PHPRuntime{}, sitePoolConfig{}, err
	}
	main, err := readSitePHPProtectedFile(runtime.MainConfig, sitePHPMaxPoolBytes)
	if err != nil {
		return Site{}, Identity{}, PHPRuntime{}, sitePoolConfig{}, err
	}
	if err = verifySitePHPInclude(main, runtime.PoolDir); err != nil {
		return Site{}, Identity{}, PHPRuntime{}, sitePoolConfig{}, err
	}
	pool, err := readSitePHPProtectedFile(filepath.Join(runtime.PoolDir, site.ID+".conf"), sitePHPMaxPoolBytes)
	if err != nil {
		return Site{}, Identity{}, PHPRuntime{}, sitePoolConfig{}, err
	}
	config, err := parseSitePool(pool, site.ID)
	if err != nil {
		return Site{}, Identity{}, PHPRuntime{}, sitePoolConfig{}, err
	}
	paths := e.paths(site)
	if err = verifySitePoolIdentityAndSessionPath(config, site, worker, paths); err != nil {
		return Site{}, Identity{}, PHPRuntime{}, sitePoolConfig{}, err
	}
	return site, worker, runtime, config, nil
}

func (e *AgentExecutor) verifySiteSessionHandler(ctx context.Context, site Site, runtime PHPRuntime, pool sitePoolConfig) error {
	if values := pool.values["php_admin_value[session.save_handler]"]; len(values) > 0 {
		if values[0] != "files" {
			return failure("unsupported_session_handler", "Only the effective PHP files session handler can be inspected or cleaned", 412)
		}
		return nil
	}
	effective, err := sitePHPGlobalSessionHandler(ctx, runtime)
	if err != nil {
		return err
	}
	if effective != "files" {
		return failure("unsupported_session_handler", "Only the effective PHP files session handler can be inspected or cleaned", 412)
	}
	return nil
}

func sitePHPGlobalSessionHandler(ctx context.Context, runtime PHPRuntime) (string, error) {
	if !filepath.IsAbs(runtime.CLIProgram) || !filepath.IsAbs(runtime.CLIConfig) || filepath.Clean(runtime.CLIConfig) != runtime.CLIConfig || runtime.CLIConfig != filepath.Join(filepath.Dir(runtime.MainConfig), "php.ini") {
		return "", failure("php_runtime_invalid", "Protected PHP CLI configuration path is invalid", 412)
	}
	if _, err := readSitePHPProtectedFile(runtime.CLIConfig, sitePHPMaxPoolBytes); err != nil {
		return "", err
	}
	program, err := filepath.EvalSymlinks(runtime.CLIProgram)
	if err != nil {
		return "", err
	}
	if err = trustedPHPCLIFile(program, true); err != nil {
		return "", err
	}
	scanDir := filepath.Join(filepath.Dir(runtime.CLIConfig), "conf.d")
	if strings.ContainsAny(scanDir, string(filepath.ListSeparator)+"\x00\r\n") {
		return "", failure("php_runtime_invalid", "Protected PHP scan directory cannot be represented in the managed environment", 412)
	}
	if err = verifySitePHPScanDir(scanDir); err != nil {
		return "", err
	}
	op := agent.Operation{Name: "site_php_session_handler", Program: program, Timeout: 8 * time.Second, Environment: []string{"PHP_INI_SCAN_DIR=" + scanDir, "PHPRC="}, BuildArgs: func(json.RawMessage) ([]string, error) {
		// The protected ini tree is loaded only to read its effective handler.
		// Disable configured script hooks and PHP logging so this root-run probe
		// never executes application PHP files or writes runtime data as root.
		return []string{"-c", runtime.CLIConfig, "-d", "auto_prepend_file=", "-d", "auto_append_file=", "-d", "opcache.preload=", "-d", "opcache.enable_cli=0", "-d", "display_errors=0", "-d", "log_errors=0", "-d", "error_log=/dev/null", "-r", "echo ini_get('session.save_handler');"}, nil
	}}
	runner, err := agent.NewRunner(agent.Options{Operations: []agent.Operation{op}, MaxConcurrent: 1, MaxOutputBytes: 1024})
	if err != nil {
		return "", err
	}
	result, err := runner.Run(ctx, agent.Request{Operation: op.Name, Input: json.RawMessage(`{}`)})
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if result.TimedOut {
			return "", context.DeadlineExceeded
		}
		return "", failure("php_session_handler_unavailable", "The managed PHP runtime could not verify its effective session handler", 412)
	}
	if result.ExitCode != 0 || result.Truncated {
		return "", failure("php_session_handler_unavailable", "The managed PHP runtime could not verify its effective session handler", 412)
	}
	handler := strings.TrimSpace(result.Stdout)
	if handler == "" || strings.ContainsAny(handler, "\r\n\x00") {
		return "", failure("php_session_handler_unavailable", "The managed PHP runtime returned an invalid session handler", 412)
	}
	return handler, nil
}

// verifySitePHPScanDir validates exactly the bounded set of PHP files that the
// CLI will load from its configured scan directory. Other entries are ignored
// because PHP ignores them too; every .ini file must be a protected regular
// file so an untrusted symlink or writable fragment cannot change the handler
// probe's effective configuration.
func verifySitePHPScanDir(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return failure("php_runtime_invalid", "Protected PHP scan directory path is invalid", 412)
	}
	if err := protectedParents(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return failure("php_runtime_invalid", "Protected PHP scan directory is not a real directory", 412)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return failure("php_runtime_invalid", "Protected PHP scan directory identity changed", 412)
	}
	if err = verifyJournalRoot(root); err != nil {
		return err
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(sitePHPMaxScanFiles + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) > sitePHPMaxScanFiles {
		return failure("php_runtime_invalid", "Protected PHP scan directory exceeds its entry limit", 413)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".ini") {
			paths = append(paths, entry.Name())
		}
	}
	sort.Strings(paths)
	var total int
	for _, name := range paths {
		var raw []byte
		raw, err = readSitePHPProtectedFile(filepath.Join(path, name), sitePHPMaxFileBytes)
		if err != nil {
			return err
		}
		total += len(raw)
		if total > sitePHPMaxScanBytes {
			return failure("php_runtime_invalid", "Protected PHP scan files exceed their aggregate read limit", 413)
		}
	}
	return nil
}

func verifySitePHPInclude(raw []byte, poolDir string) error {
	includeCount := 0
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := sitePHPDirective(line)
		if !ok || key != "include" {
			continue
		}
		includeCount++
		if sitePHPUnquote(value) != filepath.Join(poolDir, "*.conf") {
			return failure("php_pool_invalid", "Managed PHP master config includes an unexpected pool path", 412)
		}
	}
	if includeCount != 1 {
		return failure("php_pool_invalid", "Managed PHP master config does not include exactly one protected site pool directory", 412)
	}
	return nil
}

func parseSitePool(raw []byte, siteID string) (sitePoolConfig, error) {
	config := sitePoolConfig{values: map[string][]string{}}
	expectedSection := "powerlinux-" + siteTag(siteID)
	sectionCount := 0
	inside := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			sectionCount++
			inside = strings.TrimSpace(trimmed[1:len(trimmed)-1]) == expectedSection
			if !inside {
				return config, failure("php_pool_invalid", "Site PHP pool contains an unexpected section", 412)
			}
			continue
		}
		if !inside {
			return config, failure("php_pool_invalid", "Site PHP pool has directives outside its protected section", 412)
		}
		key, value, ok := sitePHPDirective(line)
		if !ok {
			return config, failure("php_pool_invalid", "Site PHP pool contains an invalid directive", 412)
		}
		config.values[key] = append(config.values[key], sitePHPUnquote(value))
	}
	if sectionCount != 1 || len(config.values) == 0 {
		return config, failure("php_pool_invalid", "Site PHP pool section is missing or ambiguous", 412)
	}
	return config, nil
}

func sitePHPDirective(line string) (string, string, bool) {
	key, value, ok := strings.Cut(line, "=")
	if !ok {
		return "", "", false
	}
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	return key, value, key != "" && value != ""
}

func sitePHPUnquote(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
		if value[0] == '"' {
			if unquoted, err := strconv.Unquote(value); err == nil {
				return unquoted
			}
		}
		return value[1 : len(value)-1]
	}
	return value
}

func verifySitePoolIdentityAndSessionPath(pool sitePoolConfig, site Site, worker Identity, paths renderedPaths) error {
	expected := map[string]string{
		"user": worker.User, "group": worker.Group,
		"listen":      filepath.Join(paths.RunDir, "php-fpm.sock"),
		"listen.mode": "0660",
		"pm":          "ondemand", "pm.max_children": "4", "pm.process_idle_timeout": "20s", "pm.max_requests": "500",
		"clear_env": "yes", "security.limit_extensions": ".php",
		"php_admin_value[error_log]": filepath.Join(paths.TempDir, "php-errors.log"),
		"php_admin_flag[log_errors]": "on", "php_admin_value[upload_tmp_dir]": paths.TempDir,
		"php_admin_value[session.save_path]": paths.TempDir,
	}
	if site.PHP.SlowlogEnabled {
		expected["request_slowlog_timeout"] = fmt.Sprintf("%ds", site.PHP.SlowlogTimeoutSeconds)
		expected["slowlog"] = filepath.Join(paths.LogDir, "php-slow.log")
	}
	if site.PHP.OpenBasedir {
		expected["php_admin_value[open_basedir]"] = paths.DocumentRoot + string(filepath.ListSeparator) + paths.TempDir
	}
	for key, value := range site.PHP.Values {
		switch key {
		case "open_basedir", "error_log", "upload_tmp_dir", "session.save_path":
			return failure("php_pool_invalid", "Website PHP values cannot replace protected pool paths", 412)
		}
		expected["php_admin_value["+key+"]"] = value
	}
	for key, values := range pool.values {
		value, ok := expected[key]
		if key == "listen.owner" || key == "listen.group" {
			if len(values) == 1 && safeID.MatchString(values[0]) {
				continue
			}
			return failure("php_pool_invalid", "Site PHP pool socket identity is invalid", 412)
		}
		if !ok || len(values) != 1 || values[0] != value {
			return failure("php_pool_invalid", "Site PHP pool contains a directive outside its protected website configuration", 412)
		}
	}
	expected["listen.owner"], expected["listen.group"] = "", ""
	for key := range expected {
		if values := pool.values[key]; len(values) != 1 {
			return failure("php_pool_invalid", "Site PHP pool is missing a protected website directive", 412)
		}
	}
	return nil
}

func verifySitePoolSlowlog(site Site, pool sitePoolConfig, paths renderedPaths) error {
	slowlog := pool.values["slowlog"]
	timeout := pool.values["request_slowlog_timeout"]
	if !site.PHP.SlowlogEnabled {
		if len(slowlog) != 0 || len(timeout) != 0 {
			return failure("php_slowlog_configuration_invalid", "Site PHP pool slowlog directives do not match the saved website configuration", 412)
		}
		return nil
	}
	if len(slowlog) != 1 || slowlog[0] != filepath.Join(paths.LogDir, "php-slow.log") || len(timeout) != 1 || timeout[0] != fmt.Sprintf("%ds", site.PHP.SlowlogTimeoutSeconds) {
		return failure("php_slowlog_configuration_invalid", "Site PHP pool slowlog directives do not match the saved website configuration", 412)
	}
	return nil
}

func readSitePHPProtectedFile(path string, limit int64) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || limit < 1 {
		return nil, failure("php_runtime_invalid", "Managed PHP configuration path is invalid", 412)
	}
	if err := protectedParents(filepath.Dir(path)); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() < 0 || before.Size() > limit {
		return nil, failure("php_runtime_invalid", "Managed PHP configuration must be a bounded regular file", 412)
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || before.Mode().Perm()&0022 != 0 || before.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return nil, failure("php_runtime_invalid", "Managed PHP configuration ownership or mode is unsafe", 412)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, failure("php_runtime_invalid", "Managed PHP configuration exceeds its read limit", 413)
	}
	pathInfo, err := os.Lstat(path)
	openedInfo, statErr := file.Stat()
	if err != nil || statErr != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, pathInfo) || !os.SameFile(before, openedInfo) || !before.ModTime().Equal(openedInfo.ModTime()) {
		return nil, failure("php_runtime_invalid", "Managed PHP configuration identity changed during inspection", 412)
	}
	return data, nil
}

func (e *AgentExecutor) openSitePHPTmp(site Site, worker Identity) (*os.Root, phpfilesessions.DirectoryIdentity, error) {
	parentPath := site.ID
	parentInfo, err := e.content.Lstat(parentPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, phpfilesessions.DirectoryIdentity{}, os.ErrNotExist
	}
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 || parentInfo.Mode().Perm() != 0751 {
		return nil, phpfilesessions.DirectoryIdentity{}, failure("session_directory_unsafe", "Site root is not a protected managed directory", 412)
	}
	parentStat, ok := parentInfo.Sys().(*syscall.Stat_t)
	if !ok || parentStat.Uid != uint32(os.Geteuid()) {
		return nil, phpfilesessions.DirectoryIdentity{}, failure("session_directory_unsafe", "Site root ownership changed", 412)
	}
	tmpPath := filepath.Join(site.ID, "tmp")
	pathInfo, err := e.content.Lstat(tmpPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, phpfilesessions.DirectoryIdentity{}, os.ErrNotExist
	}
	if err != nil || !pathInfo.IsDir() || pathInfo.Mode()&os.ModeSymlink != 0 {
		return nil, phpfilesessions.DirectoryIdentity{}, failure("session_directory_unsafe", "Site PHP tmp directory is not a real directory", 412)
	}
	root, err := e.content.OpenRoot(tmpPath)
	if err != nil {
		return nil, phpfilesessions.DirectoryIdentity{}, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(pathInfo, opened) {
		_ = root.Close()
		return nil, phpfilesessions.DirectoryIdentity{}, failure("session_directory_unsafe", "Site PHP tmp directory identity changed", 412)
	}
	identity, err := phpfilesessions.IdentifyDirectory(opened, worker.UID, worker.GID, true)
	if err != nil || opened.Mode().Perm() != 0700 {
		_ = root.Close()
		return nil, phpfilesessions.DirectoryIdentity{}, failure("session_directory_unsafe", "Site PHP tmp directory ownership or mode is unsafe", 412)
	}
	return root, identity, nil
}

func (e *AgentExecutor) openSitePHPLogDirectory(site Site) (*os.Root, error) {
	rootPath := site.ID
	parentInfo, err := e.content.Lstat(rootPath)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 || parentInfo.Mode().Perm() != 0751 {
		return nil, failure("php_slowlog_unsafe", "Site root is not a protected managed directory", 412)
	}
	parentStat, ok := parentInfo.Sys().(*syscall.Stat_t)
	if !ok || parentStat.Uid != uint32(os.Geteuid()) {
		return nil, failure("php_slowlog_unsafe", "Site root ownership changed", 412)
	}
	path := filepath.Join(site.ID, "logs")
	pathInfo, err := e.content.Lstat(path)
	if err != nil || !pathInfo.IsDir() || pathInfo.Mode()&os.ModeSymlink != 0 || pathInfo.Mode().Perm() != 0750 {
		return nil, failure("php_slowlog_unsafe", "Site PHP log directory is not a protected managed directory", 412)
	}
	stat, ok := pathInfo.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, failure("php_slowlog_unsafe", "Site PHP log directory ownership changed", 412)
	}
	root, err := e.content.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(pathInfo, opened) {
		_ = root.Close()
		return nil, failure("php_slowlog_unsafe", "Site PHP log directory identity changed", 412)
	}
	return root, nil
}
