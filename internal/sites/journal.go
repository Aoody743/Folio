package sites

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/files"
)

const (
	maxWebsiteJournalBytes   = 768 << 10
	maxWebsiteJournalFiles   = 256
	maxWebsiteJournalDirs    = 16
	maxWebsiteJournalPayload = 384 << 10
	maxWebsiteJournals       = 128
)

// Journal file names are logical paths under a directory opened from a
// protected authority. The record never supplies an absolute host path.
type journalFile struct {
	Kind      string      `json:"kind"`
	RuntimeID string      `json:"runtime_id,omitempty"`
	Path      string      `json:"path"`
	Before    []byte      `json:"before,omitempty"`
	Existed   bool        `json:"existed"`
	Mode      fs.FileMode `json:"mode,omitempty"`
	UID       int         `json:"uid,omitempty"`
	GID       int         `json:"gid,omitempty"`
}

type journalSite struct {
	ID         string `json:"id"`
	WebRuntime string `json:"web_runtime"`
	PHPRuntime string `json:"php_runtime,omitempty"`
}

type journalDirectory struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type journalDirState struct {
	Path    string      `json:"path"`
	Existed bool        `json:"existed"`
	Mode    fs.FileMode `json:"mode,omitempty"`
	UID     int         `json:"uid,omitempty"`
	GID     int         `json:"gid,omitempty"`
	Device  uint64      `json:"device,omitempty"`
	Inode   uint64      `json:"inode,omitempty"`
}

type journalSwap struct {
	SiteID   string           `json:"site_id"`
	BackupID string           `json:"backup_id"`
	Original journalDirectory `json:"original"`
	Staged   journalDirectory `json:"staged,omitempty"`
}

type journalContent struct {
	ID  string           `json:"id"`
	Dir journalDirectory `json:"dir,omitempty"`
}

type journalWorker struct {
	SiteID   string `json:"site_id"`
	Username string `json:"username"`
	Marker   string `json:"marker"`
}

// websiteJournal is a bounded root-private write-ahead record. Committed is
// the commit marker; an uncommitted record always means restore its before
// image, while a committed record only permits idempotent cleanup.
type websiteJournal struct {
	Version       int               `json:"version"`
	ID            string            `json:"id"`
	Operation     string            `json:"operation"`
	Sites         []journalSite     `json:"sites"`
	Certificate   string            `json:"certificate,omitempty"`
	RuntimeIDs    []string          `json:"runtime_ids"`
	Files         []journalFile     `json:"files"`
	Directories   []journalDirState `json:"directories,omitempty"`
	NewContent    *journalContent   `json:"new_content,omitempty"`
	PendingWorker *journalWorker    `json:"pending_worker,omitempty"`
	Swap          *journalSwap      `json:"swap,omitempty"`
	Committed     bool              `json:"committed"`
}

type websiteJournalKey struct{}
type activeWebsiteJournal struct {
	executor        *AgentExecutor
	record          websiteJournal
	commitUncertain bool
}

// RuntimeRecovery restores an already-running protected runtime without
// starting a stopped service. Production composition implements this using
// plugins.PrivilegedExecutor.RecoverRuntime.
type RuntimeRecovery interface {
	RecoverRuntime(context.Context, string) error
}

func journalActive(ctx context.Context, e *AgentExecutor) bool {
	active, ok := ctx.Value(websiteJournalKey{}).(*activeWebsiteJournal)
	return ok && active.executor == e
}

func journalFileKey(file journalFile) string {
	return file.Kind + "\x00" + file.RuntimeID + "\x00" + file.Path
}

func journalSiteRef(site Site) journalSite {
	return journalSite{ID: site.ID, WebRuntime: site.WebInstance, PHPRuntime: site.PHPInstance}
}

func (e *AgentExecutor) journalPaths(ctx context.Context, j websiteJournal) ([]journalFile, []string, error) {
	allowed := map[string]journalFile{}
	add := func(file journalFile) {
		allowed[journalFileKey(file)] = file
	}
	for _, site := range j.Sites {
		if !safeID.MatchString(site.ID) || !safeID.MatchString(site.WebRuntime) || site.PHPRuntime != "" && !safeID.MatchString(site.PHPRuntime) {
			return nil, nil, errors.New("invalid website journal identity")
		}
		web, err := e.runtimes.Web(ctx, site.WebRuntime)
		if err != nil {
			return nil, nil, err
		}
		if web.ID != "" && web.ID != site.WebRuntime {
			return nil, nil, errors.New("protected Web runtime identity mismatch")
		}
		add(journalFile{Kind: "web", RuntimeID: site.WebRuntime, Path: site.ID + ".conf"})
		add(journalFile{Kind: "content", Path: filepath.Join(site.ID, "auth", "htpasswd")})
		for _, directory := range []string{"sites", "retained"} {
			add(journalFile{Kind: "state", Path: filepath.Join(directory, site.ID+".json")})
		}
		if site.PHPRuntime != "" {
			php, err := e.runtimes.PHP(ctx, site.PHPRuntime)
			if err != nil {
				return nil, nil, err
			}
			if php.ID != "" && php.ID != site.PHPRuntime {
				return nil, nil, errors.New("protected PHP runtime identity mismatch")
			}
			add(journalFile{Kind: "php", RuntimeID: site.PHPRuntime, Path: site.ID + ".conf"})
		}
	}
	if j.Certificate != "" {
		if !safeID.MatchString(j.Certificate) {
			return nil, nil, errors.New("invalid journal certificate identity")
		}
		for _, extension := range []string{"json", "crt", "key"} {
			add(journalFile{Kind: "state", Path: filepath.Join("certificates", j.Certificate+"."+extension)})
		}
	}
	files := make([]journalFile, 0, len(allowed))
	for _, file := range allowed {
		files = append(files, file)
	}
	sort.Slice(files, func(i, k int) bool { return journalFileKey(files[i]) < journalFileKey(files[k]) })
	if len(files) > maxWebsiteJournalFiles {
		return nil, nil, failure("transaction_too_large", "Too many website recovery files", 413)
	}
	ids := make([]string, 0, len(j.Sites)*2)
	for _, site := range j.Sites {
		ids = append(ids, site.WebRuntime)
		if site.PHPRuntime != "" {
			ids = append(ids, site.PHPRuntime)
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	return files, ids, nil
}

func journalDirectoryPaths(j websiteJournal) ([]string, error) {
	if j.Operation == "site_apply" && j.NewContent != nil {
		return nil, nil
	}
	if j.Operation != "site_apply" && j.Operation != "site_backup_restore" {
		return nil, nil
	}
	if len(j.Sites) == 0 || !safeID.MatchString(j.Sites[0].ID) {
		return nil, errors.New("website directory journal has no protected site identity")
	}
	id := j.Sites[0].ID
	return []string{
		id,
		filepath.Join(id, "public"),
		filepath.Join(id, "logs"),
		filepath.Join(id, "run"),
		filepath.Join(id, "run", "proxy-cache"),
		filepath.Join(id, "auth"),
		filepath.Join(id, "tmp"),
	}, nil
}

func (e *AgentExecutor) snapshotJournalDirectory(path string) (journalDirState, error) {
	state := journalDirState{Path: path}
	info, err := e.content.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Mode().Perm()&0022 != 0 {
		return state, errors.New("website transaction directory metadata is not protected")
	}
	identity, err := e.contentDirectory(path)
	if err != nil {
		return state, err
	}
	state.Existed = true
	state.Mode, state.UID, state.GID = info.Mode().Perm(), int(stat.Uid), int(stat.Gid)
	state.Device, state.Inode = identity.Device, identity.Inode
	return state, nil
}

func (e *AgentExecutor) journalSites(ctx context.Context, operation string, in request) ([]journalSite, string, *journalContent, *journalSwap, error) {
	sites := []journalSite{}
	add := func(site Site) {
		ref := journalSiteRef(site)
		for _, present := range sites {
			if present == ref {
				return
			}
		}
		sites = append(sites, ref)
	}
	certificate := ""
	var newContent *journalContent
	var swap *journalSwap
	switch operation {
	case "site_apply", "site_delete", "site_config_set", "site_default_set", "site_backup_restore":
		current, err := e.get(in.ID)
		if err == nil {
			add(current)
		} else if operation != "site_apply" || !isNotFound(err) {
			return nil, "", nil, nil, err
		}
		if operation == "site_apply" {
			if in.Site == nil || in.Site.ID != in.ID {
				return nil, "", nil, nil, invalid("Website transaction requires typed site input")
			}
			if isNotFound(err) {
				if err = e.rejectUnownedContent(in.ID); err != nil {
					return nil, "", nil, nil, err
				}
				newContent = &journalContent{ID: in.ID}
			}
			add(*in.Site)
		}
		if operation == "site_default_set" && in.PreviousSiteID != "" {
			previous, err := e.get(in.PreviousSiteID)
			if err != nil {
				return nil, "", nil, nil, err
			}
			add(previous)
		}
		if operation == "site_backup_restore" {
			manifest, err := e.backupMetadata(in)
			if err != nil {
				return nil, "", nil, nil, err
			}
			add(manifest.Site)
			original, err := e.contentDirectory(filepath.Join(in.ID, "public"))
			if err != nil {
				return nil, "", nil, nil, err
			}
			swap = &journalSwap{SiteID: in.ID, BackupID: in.BackupID, Original: original}
		}
	case "certificate_import", "certificate_delete":
		certificate = in.ID
		if operation == "certificate_delete" {
			if _, err := e.certificate(in.ID); err != nil {
				return nil, "", nil, nil, err
			}
		} else {
			all, err := e.list()
			if err != nil {
				return nil, "", nil, nil, err
			}
			for _, site := range all {
				if site.TLS.Enabled && site.TLS.CertificateID == in.ID {
					add(site)
				}
			}
		}
	default:
		return nil, "", nil, nil, errors.New("unsupported website transaction")
	}
	if len(sites) > 128 {
		return nil, "", nil, nil, failure("transaction_too_large", "Website transaction affects too many sites", 413)
	}
	return sites, certificate, newContent, swap, nil
}

func (e *AgentExecutor) readJournalFile(ctx context.Context, file journalFile) (result journalFile, resultErr error) {
	result = file
	root, closeRoot, path, err := e.journalFileRoot(ctx, file)
	if err != nil {
		return file, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeRoot()) }()
	opened, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		file.Before = nil
		file.Existed = false
		file.Mode, file.UID, file.GID = 0, 0, 0
		return file, nil
	}
	if err != nil {
		return file, err
	}
	defer opened.Close()
	info, err := opened.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return file, errors.New("website recovery target is not a regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
		return file, errors.New("website recovery target ownership is not protected")
	}
	before, err := io.ReadAll(io.LimitReader(opened, maxWebsiteJournalPayload+1))
	if err != nil {
		return file, err
	}
	if len(before) > maxWebsiteJournalPayload {
		return file, failure("transaction_too_large", "A website recovery file exceeds the protected snapshot budget", 413)
	}
	file.Before = before
	file.Existed = true
	file.Mode = info.Mode().Perm()
	file.UID, file.GID = int(stat.Uid), int(stat.Gid)
	return file, nil
}

func validJournalRelativePath(path string) bool {
	return path != "" && !filepath.IsAbs(path) && filepath.Clean(path) == path && fs.ValidPath(filepath.ToSlash(path)) && path != "."
}

func (e *AgentExecutor) journalFileRoot(ctx context.Context, file journalFile) (*os.Root, func() error, string, error) {
	if !validJournalRelativePath(file.Path) {
		return nil, func() error { return nil }, "", errors.New("invalid website journal relative path")
	}
	switch file.Kind {
	case "state":
		if file.RuntimeID != "" {
			return nil, func() error { return nil }, "", errors.New("state journal path has a runtime identity")
		}
		return e.state, func() error { return nil }, file.Path, nil
	case "content":
		if file.RuntimeID != "" {
			return nil, func() error { return nil }, "", errors.New("content journal path has a runtime identity")
		}
		return e.content, func() error { return nil }, file.Path, nil
	case "web":
		if !safeID.MatchString(file.RuntimeID) || filepath.Dir(file.Path) != "." || !strings.HasSuffix(file.Path, ".conf") {
			return nil, func() error { return nil }, "", errors.New("invalid protected Web journal path")
		}
		runtime, err := e.runtimes.Web(ctx, file.RuntimeID)
		if err != nil {
			return nil, func() error { return nil }, "", err
		}
		root, err := os.OpenRoot(runtime.SnippetsDir)
		if err != nil {
			return nil, func() error { return nil }, "", err
		}
		if err = verifyJournalRoot(root); err != nil {
			return nil, func() error { return nil }, "", errors.Join(err, root.Close())
		}
		return root, root.Close, file.Path, nil
	case "php":
		if !safeID.MatchString(file.RuntimeID) || filepath.Dir(file.Path) != "." || !strings.HasSuffix(file.Path, ".conf") {
			return nil, func() error { return nil }, "", errors.New("invalid protected PHP journal path")
		}
		runtime, err := e.runtimes.PHP(ctx, file.RuntimeID)
		if err != nil {
			return nil, func() error { return nil }, "", err
		}
		root, err := os.OpenRoot(runtime.PoolDir)
		if err != nil {
			return nil, func() error { return nil }, "", err
		}
		if err = verifyJournalRoot(root); err != nil {
			return nil, func() error { return nil }, "", errors.Join(err, root.Close())
		}
		return root, root.Close, file.Path, nil
	default:
		return nil, func() error { return nil }, "", errors.New("unknown website journal file authority")
	}
}

func verifyJournalRoot(root *os.Root) error {
	info, err := root.Stat(".")
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
		return errors.New("protected runtime directory is not owned by the Agent")
	}
	return nil
}

func (e *AgentExecutor) storeJournal(j websiteJournal) error {
	raw := encode(j)
	if len(raw) > maxWebsiteJournalBytes {
		return failure("transaction_too_large", "Website transaction exceeds its bounded protected recovery budget", 413)
	}
	return atomicRoot(e.state, filepath.Join("transactions", j.ID+".json"), raw, 0600)
}

func (e *AgentExecutor) deleteJournal(id string) error {
	if err := e.state.Remove(filepath.Join("transactions", id+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	dir, err := e.state.Open("transactions")
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	return errors.Join(syncErr, closeErr)
}

func (e *AgentExecutor) ensureNoPendingJournal() error {
	dir, err := e.state.Open("transactions")
	if err != nil {
		return err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(maxWebsiteJournals + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) > 0 {
		return failure("transaction_recovery_required", "A previous website transaction needs restart recovery before another write", 503)
	}
	if len(entries) > maxWebsiteJournals {
		return failure("transaction_recovery_required", "Website recovery inventory exceeds its limit", 503)
	}
	return nil
}

func (e *AgentExecutor) verifyJournalDirectory() error {
	info, err := e.state.Lstat("transactions")
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		return errors.New("website transaction directory must be private and owned by the Agent")
	}
	return nil
}

func acquireWebsiteStateLock(state *os.Root) (*os.File, error) {
	file, err := state.OpenFile("sites-agent.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	stat, ok := (*syscall.Stat_t)(nil), false
	if err == nil {
		stat, ok = info.Sys().(*syscall.Stat_t)
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ok || stat.Uid != uint32(os.Geteuid()) {
		file.Close()
		return nil, errors.New("invalid protected website Agent lock")
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("website state is already owned by another Agent")
	}
	return file, nil
}

func (e *AgentExecutor) beginWebsiteJournal(ctx context.Context, operation string, in request) (context.Context, func(*error), error) {
	if active, ok := ctx.Value(websiteJournalKey{}).(*activeWebsiteJournal); ok && active.executor == e {
		return ctx, func(*error) {}, nil
	}
	if err := e.ensureNoPendingJournal(); err != nil {
		return ctx, nil, err
	}
	sites, certificate, newContent, swap, err := e.journalSites(ctx, operation, in)
	if err != nil {
		return ctx, nil, err
	}
	j := websiteJournal{Version: 1, Operation: operation, Sites: sites, Certificate: certificate, NewContent: newContent, Swap: swap}
	files, ids, err := e.journalPaths(ctx, j)
	if err != nil {
		return ctx, nil, err
	}
	j.RuntimeIDs = ids
	if len(files) > maxWebsiteJournalFiles {
		return ctx, nil, failure("transaction_too_large", "Too many website recovery files", 413)
	}
	total := 0
	for _, file := range files {
		file, err = e.readJournalFile(ctx, file)
		if err != nil {
			return ctx, nil, err
		}
		total += len(file.Before)
		if total > maxWebsiteJournalPayload {
			return ctx, nil, failure("transaction_too_large", "Website recovery snapshots exceed the protected budget", 413)
		}
		j.Files = append(j.Files, file)
	}
	directories, err := journalDirectoryPaths(j)
	if err != nil {
		return ctx, nil, err
	}
	if len(directories) > maxWebsiteJournalDirs {
		return ctx, nil, failure("transaction_too_large", "Too many website recovery directories", 413)
	}
	for _, path := range directories {
		directory, snapshotErr := e.snapshotJournalDirectory(path)
		if snapshotErr != nil {
			return ctx, nil, snapshotErr
		}
		j.Directories = append(j.Directories, directory)
	}
	var entropy [16]byte
	if _, err = rand.Read(entropy[:]); err != nil {
		return ctx, nil, err
	}
	j.ID = "t-" + hex.EncodeToString(entropy[:])
	if err = e.storeJournal(j); err != nil {
		return ctx, nil, err
	}
	active := &activeWebsiteJournal{executor: e, record: j}
	locked := context.WithValue(ctx, websiteJournalKey{}, active)
	finish := func(operationErr *error) {
		if *operationErr == nil && !active.record.Committed {
			if err := active.commit(); err != nil {
				*operationErr = errors.Join(*operationErr, err)
				active.commitUncertain = true
				return
			}
		}
		if active.commitUncertain {
			return
		}
		if active.record.Committed {
			if err := e.cleanupJournal(locked, active.record); err != nil {
				*operationErr = errors.Join(*operationErr, failure("cleanup_pending", "Website transaction committed; protected cleanup will resume on restart", 500))
				return
			}
			if err := e.deleteJournal(active.record.ID); err != nil {
				*operationErr = errors.Join(*operationErr, err)
			}
			return
		}
		recovery, cancel := context.WithTimeout(context.WithoutCancel(locked), 30*time.Second)
		defer cancel()
		if err := e.rollbackJournal(recovery, active.record); err != nil {
			*operationErr = errors.Join(*operationErr, failure("rollback_incomplete", "Website recovery journal is retained for restart recovery", 500))
			return
		}
		if err := e.deleteJournal(active.record.ID); err != nil {
			*operationErr = errors.Join(*operationErr, err)
		}
	}
	return locked, finish, nil
}

func (a *activeWebsiteJournal) commit() error {
	if a.record.Committed {
		return nil
	}
	next := a.record
	next.Committed = true
	if err := a.executor.storeJournal(next); err != nil {
		a.commitUncertain = true
		return err
	}
	a.record = next
	return nil
}

func commitWebsiteJournal(ctx context.Context) error {
	active, ok := ctx.Value(websiteJournalKey{}).(*activeWebsiteJournal)
	if !ok {
		return nil
	}
	return active.commit()
}

func (e *AgentExecutor) recordNewContent(ctx context.Context, id string) error {
	active, ok := ctx.Value(websiteJournalKey{}).(*activeWebsiteJournal)
	if !ok || active.executor != e || active.record.NewContent == nil || active.record.NewContent.ID != id {
		return errors.New("new website content requires its durable transaction")
	}
	dir, err := e.contentDirectory(id)
	if err != nil {
		return err
	}
	next := active.record
	copy := *next.NewContent
	copy.Dir = dir
	next.NewContent = &copy
	if err = e.storeJournal(next); err != nil {
		return err
	}
	active.record = next
	return nil
}

func (e *AgentExecutor) recordRestoreStage(ctx context.Context, siteID, backupID, stage string) error {
	active, ok := ctx.Value(websiteJournalKey{}).(*activeWebsiteJournal)
	if !ok || active.executor != e || active.record.Swap == nil || active.record.Swap.SiteID != siteID || active.record.Swap.BackupID != backupID {
		return errors.New("website restore stage requires its durable transaction")
	}
	dir, err := e.contentDirectory(stage)
	if err != nil {
		return err
	}
	next := active.record
	copy := *next.Swap
	copy.Staged = dir
	next.Swap = &copy
	if err = e.storeJournal(next); err != nil {
		return err
	}
	active.record = next
	return nil
}

func (e *AgentExecutor) recordPendingWorker(ctx context.Context, siteID, username string) (string, error) {
	active, ok := ctx.Value(websiteJournalKey{}).(*activeWebsiteJournal)
	if !ok || active.executor != e || active.record.NewContent == nil || active.record.NewContent.ID != siteID {
		return "", errors.New("new website identity requires its durable transaction")
	}
	expected := "plsite_" + siteTag(siteID)
	if username != expected {
		return "", errors.New("website identity name does not match its protected site ID")
	}
	marker := pendingWorkerMarker(siteID, active.record.ID)
	next := active.record
	next.PendingWorker = &journalWorker{SiteID: siteID, Username: username, Marker: marker}
	if err := e.storeJournal(next); err != nil {
		return "", err
	}
	active.record = next
	return marker, nil
}

func pendingWorkerMarker(siteID, journalID string) string {
	return "powerlinux-site-" + siteID + "-" + journalID
}

func (e *AgentExecutor) clearPendingWorker(ctx context.Context, siteID string) error {
	active, ok := ctx.Value(websiteJournalKey{}).(*activeWebsiteJournal)
	if !ok || active.executor != e || active.record.PendingWorker == nil || active.record.PendingWorker.SiteID != siteID {
		return errors.New("website identity completion has no matching pending intent")
	}
	next := active.record
	next.PendingWorker = nil
	if err := e.storeJournal(next); err != nil {
		return err
	}
	active.record = next
	return nil
}

func (e *AgentExecutor) recordRestoreSwap(ctx context.Context, siteID string) error {
	active, ok := ctx.Value(websiteJournalKey{}).(*activeWebsiteJournal)
	if !ok || active.executor != e || active.record.Swap == nil || active.record.Swap.SiteID != siteID || active.record.Swap.Staged.Inode == 0 {
		return errors.New("content swap requires a durable website transaction and owned stage")
	}
	actual, err := e.contentDirectory(filepath.Join(siteID, "public"))
	if err != nil || actual != active.record.Swap.Original {
		return errors.New("website original directory identity changed before restore swap")
	}
	return e.syncContentSite(siteID)
}

func (e *AgentExecutor) contentDirectory(name string) (journalDirectory, error) {
	if !validJournalRelativePath(name) {
		return journalDirectory{}, errors.New("invalid website transaction directory path")
	}
	parts := strings.Split(filepath.ToSlash(name), "/")
	path := ""
	var info os.FileInfo
	for _, part := range parts {
		path = filepath.Join(path, part)
		current, err := e.content.Lstat(path)
		if err != nil {
			return journalDirectory{}, err
		}
		if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 {
			return journalDirectory{}, errors.New("website transaction directory is not a real directory")
		}
		info = current
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return journalDirectory{}, errors.New("website transaction directory identity is unavailable")
	}
	return journalDirectory{Device: uint64(stat.Dev), Inode: stat.Ino}, nil
}

func websiteRestorePaths(s journalSwap, journalID string) (public, previous, stage string) {
	tag := strings.TrimPrefix(journalID, "t-")
	return filepath.Join(s.SiteID, "public"), filepath.Join(s.SiteID, ".restore-previous-"+tag), filepath.Join(s.SiteID, ".restore-"+tag)
}

func (e *AgentExecutor) syncContentSite(id string) error {
	dir, err := e.content.Open(id)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	return errors.Join(syncErr, closeErr)
}

func (e *AgentExecutor) rollbackJournal(ctx context.Context, j websiteJournal) error {
	var result error
	if j.Swap != nil {
		result = errors.Join(result, e.rollbackRestoreSwap(j))
	}
	if j.PendingWorker != nil {
		result = errors.Join(result, e.cleanupPendingWorker(ctx, *j.PendingWorker))
	}
	for i := len(j.Files) - 1; i >= 0; i-- {
		result = errors.Join(result, e.restoreJournalFile(ctx, j.Files[i]))
	}
	for i := len(j.Directories) - 1; i >= 0; i-- {
		result = errors.Join(result, e.restoreJournalDirectory(j.Directories[i]))
	}
	if j.NewContent != nil {
		result = errors.Join(result, e.removeNewContent(ctx, j.NewContent))
	}
	for _, id := range j.RuntimeIDs {
		if recovery, ok := e.runtimes.(RuntimeRecovery); ok {
			result = errors.Join(result, recovery.RecoverRuntime(ctx, id))
		} else {
			result = errors.Join(result, errors.New("safe protected runtime recovery is unavailable"))
		}
	}
	return result
}

func (e *AgentExecutor) cleanupPendingWorker(ctx context.Context, worker journalWorker) error {
	if !safeID.MatchString(worker.SiteID) || worker.Username != "plsite_"+siteTag(worker.SiteID) || worker.Marker == "" {
		return errors.New("invalid website pending identity receipt")
	}
	var owned Identity
	err := rootJSON(e.state, filepath.Join("workers", worker.SiteID+".json"), &owned)
	if err == nil {
		account, lookupErr := user.Lookup(worker.Username)
		uid, uidErr := strconv.ParseUint(accountUID(account), 10, 32)
		gid, gidErr := strconv.ParseUint(accountGID(account), 10, 32)
		if lookupErr != nil || uidErr != nil || gidErr != nil || owned.User != account.Username || owned.UID != uint32(uid) || owned.GID != uint32(gid) || account.Name != worker.Marker || filepath.Clean(account.HomeDir) != filepath.Join(e.config.RootDir, worker.SiteID, "public") {
			return errors.New("pending website identity ownership receipt does not match its account")
		}
		// The durable receipt won the race with SIGKILL. Preserve it so a failed
		// site apply can be retried without claiming a foreign user.
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	account, lookupErr := user.Lookup(worker.Username)
	if lookupErr != nil {
		if isUnknownUserError(lookupErr) {
			return nil
		}
		return lookupErr
	}
	uid, uidErr := strconv.ParseUint(accountUID(account), 10, 32)
	gid, gidErr := strconv.ParseUint(accountGID(account), 10, 32)
	if account.Name != worker.Marker || filepath.Clean(account.HomeDir) != filepath.Join(e.config.RootDir, worker.SiteID, "public") || uidErr != nil || gidErr != nil || uid == 0 || gid == 0 {
		return errors.New("pending website identity no longer carries its unique transaction marker")
	}
	program := e.config.Programs["userdel"]
	if program == "" {
		return failure("identity_recovery_unavailable", "Website recovery needs the trusted userdel program to remove an uncommitted identity", 500)
	}
	if err = trustedProgram(program); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, program, worker.Username)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	if err = command.Run(); err != nil {
		return failure("identity_recovery_failed", "Could not remove an uncommitted website identity", 500)
	}
	if _, err = user.Lookup(worker.Username); err == nil {
		return errors.New("uncommitted website identity still exists after cleanup")
	} else if !isUnknownUserError(err) {
		return err
	}
	return nil
}

func isUnknownUserError(err error) bool {
	var value user.UnknownUserError
	if errors.As(err, &value) {
		return true
	}
	var pointer *user.UnknownUserError
	return errors.As(err, &pointer)
}

func accountUID(account *user.User) string {
	if account == nil {
		return ""
	}
	return account.Uid
}

func accountGID(account *user.User) string {
	if account == nil {
		return ""
	}
	return account.Gid
}

func (e *AgentExecutor) restoreJournalFile(ctx context.Context, file journalFile) (resultErr error) {
	root, closeRoot, path, err := e.journalFileRoot(ctx, file)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, closeRoot()) }()
	if !file.Existed {
		if err = root.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return syncJournalParentIfPresent(root, path)
	}
	if file.UID < 0 || file.GID < 0 || file.Mode&^0777 != 0 || file.Mode&0022 != 0 || len(file.Before) > maxWebsiteJournalPayload {
		return errors.New("invalid website recovery file metadata")
	}
	var entropy [16]byte
	if _, err = rand.Read(entropy[:]); err != nil {
		return err
	}
	temp := filepath.Join(filepath.Dir(path), ".powerlinux-recover-"+hex.EncodeToString(entropy[:]))
	output, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			if removeErr := root.Remove(temp); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
				resultErr = errors.Join(resultErr, removeErr)
			}
		}
	}()
	_, writeErr := output.Write(file.Before)
	chownErr := output.Chown(file.UID, file.GID)
	chmodErr := output.Chmod(file.Mode)
	syncErr := output.Sync()
	closeErr := output.Close()
	if err = errors.Join(writeErr, chownErr, chmodErr, syncErr, closeErr); err != nil {
		return err
	}
	if err = root.Rename(temp, path); err != nil {
		return err
	}
	cleanup = false
	return syncJournalParent(root, path)
}

func (e *AgentExecutor) restoreJournalDirectory(directory journalDirState) error {
	if !validJournalRelativePath(directory.Path) {
		return errors.New("invalid website recovery directory path")
	}
	actual, err := e.contentDirectory(directory.Path)
	if !directory.Existed {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = e.content.Remove(directory.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return errors.New("unowned website recovery directory is not empty; refusing recursive cleanup")
		}
		return e.syncContentParent(directory.Path)
	}
	if err != nil {
		return err
	}
	if actual.Device != directory.Device || actual.Inode != directory.Inode {
		return errors.New("website recovery directory inode changed")
	}
	if directory.UID < 0 || directory.GID < 0 || directory.Mode&^0777 != 0 || directory.Mode&0022 != 0 || directory.Inode == 0 {
		return errors.New("invalid website recovery directory metadata")
	}
	chownErr := e.content.Chown(directory.Path, directory.UID, directory.GID)
	chmodErr := e.content.Chmod(directory.Path, directory.Mode)
	directorySyncErr := e.syncContentDirectory(directory.Path)
	parentSyncErr := e.syncContentParent(directory.Path)
	return errors.Join(chownErr, chmodErr, directorySyncErr, parentSyncErr)
}

func syncJournalParent(root *os.Root, path string) error {
	parent, err := root.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	syncErr := parent.Sync()
	closeErr := parent.Close()
	return errors.Join(syncErr, closeErr)
}

func syncJournalParentIfPresent(root *os.Root, path string) error {
	parent, err := root.Open(filepath.Dir(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	syncErr := parent.Sync()
	closeErr := parent.Close()
	return errors.Join(syncErr, closeErr)
}

func (e *AgentExecutor) syncJournalFileParent(ctx context.Context, file journalFile) (resultErr error) {
	root, closeRoot, path, err := e.journalFileRoot(ctx, file)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, closeRoot()) }()
	return syncJournalParent(root, path)
}

func (e *AgentExecutor) syncStateDirectory(path string) error {
	dir, err := e.state.Open(path)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	return errors.Join(syncErr, closeErr)
}

func (e *AgentExecutor) removeNewContent(ctx context.Context, content *journalContent) error {
	if !safeID.MatchString(content.ID) {
		return errors.New("invalid new website content ownership ID")
	}
	actual, err := e.contentDirectory(content.ID)
	if errors.Is(err, fs.ErrNotExist) {
		if e.files == nil {
			return nil
		}
		removeRoot := func() error {
			if deleteErr := e.files.DeleteRoot("site-" + content.ID); deleteErr != nil && !errors.Is(deleteErr, files.ErrNotFound) {
				return deleteErr
			}
			return nil
		}
		if err = e.files.RootTransaction(ctx, "site-"+content.ID, removeRoot); errors.Is(err, files.ErrNotFound) {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	if content.Dir.Inode == 0 {
		// The process may have died after mkdir but before persisting the inode.
		// Only an empty directory can be removed without an ownership stamp.
		if err = e.content.Remove(content.ID); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return errors.New("un-stamped website content is not empty; refusing recursive cleanup")
		}
		return e.syncContentParent(content.ID)
	}
	if actual != content.Dir {
		return errors.New("website recovery content inode changed")
	}
	cleanup := func() error {
		if e.files != nil {
			if deleteErr := e.files.DeleteRoot("site-" + content.ID); deleteErr != nil && !errors.Is(deleteErr, files.ErrNotFound) {
				return deleteErr
			}
		}
		if removeErr := e.content.RemoveAll(content.ID); removeErr != nil {
			return removeErr
		}
		return e.syncContentParent(content.ID)
	}
	if e.files != nil {
		err = e.files.RootTransaction(ctx, "site-"+content.ID, cleanup)
		if errors.Is(err, files.ErrNotFound) {
			return cleanup()
		}
		return err
	}
	return cleanup()
}

func (e *AgentExecutor) rollbackRestoreSwap(j websiteJournal) error {
	s := *j.Swap
	public, previous, stage := websiteRestorePaths(s, j.ID)
	prior, priorErr := e.contentDirectory(previous)
	if priorErr == nil && prior != s.Original {
		return errors.New("website recovery original directory identity changed")
	}
	if priorErr != nil && !errors.Is(priorErr, fs.ErrNotExist) {
		return priorErr
	}
	if priorErr == nil {
		current, currentErr := e.contentDirectory(public)
		if currentErr == nil {
			if s.Staged.Inode == 0 || current != s.Staged {
				return errors.New("website recovery replacement directory identity changed")
			}
			if err := e.content.RemoveAll(public); err != nil {
				return err
			}
		} else if !errors.Is(currentErr, fs.ErrNotExist) {
			return currentErr
		}
		if err := e.content.Rename(previous, public); err != nil {
			return err
		}
		if err := e.syncContentSite(s.SiteID); err != nil {
			return err
		}
	} else {
		current, currentErr := e.contentDirectory(public)
		if currentErr != nil || current != s.Original {
			return errors.New("website recovery original content is missing")
		}
	}
	return e.removeRestoreStage(j, stage)
}

func (e *AgentExecutor) removeRestoreStage(j websiteJournal, stage string) error {
	actual, err := e.contentDirectory(stage)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if j.Swap.Staged.Inode == 0 {
		if err = e.content.Remove(stage); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return errors.New("un-stamped website restore stage is not empty; refusing recursive cleanup")
		}
		return e.syncContentSite(j.Swap.SiteID)
	}
	if actual != j.Swap.Staged {
		return errors.New("website restore stage inode changed")
	}
	if err = e.content.RemoveAll(stage); err != nil {
		return err
	}
	return e.syncContentSite(j.Swap.SiteID)
}

func (e *AgentExecutor) cleanupJournal(ctx context.Context, j websiteJournal) error {
	if j.Swap == nil {
		return nil
	}
	public, previous, stage := websiteRestorePaths(*j.Swap, j.ID)
	current, err := e.contentDirectory(public)
	if err != nil || j.Swap.Staged.Inode == 0 || current != j.Swap.Staged {
		return errors.New("committed website restore does not own the published directory")
	}
	prior, err := e.contentDirectory(previous)
	if err == nil {
		if prior != j.Swap.Original {
			return errors.New("committed website cleanup directory identity changed")
		}
		if err = e.content.RemoveAll(previous); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err = e.removeRestoreStage(j, stage); err != nil {
		return err
	}
	return e.syncContentSite(j.Swap.SiteID)
}

func (e *AgentExecutor) validateJournal(ctx context.Context, j websiteJournal, name string) error {
	if j.Version != 1 || j.ID+".json" != name || len(j.ID) != 34 || !strings.HasPrefix(j.ID, "t-") || len(j.Files) > maxWebsiteJournalFiles || len(j.Directories) > maxWebsiteJournalDirs || len(j.Sites) > 128 || len(j.RuntimeIDs) > 256 {
		return errors.New("invalid protected website journal")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(j.ID, "t-")); err != nil {
		return err
	}
	switch j.Operation {
	case "site_apply", "site_delete", "site_config_set", "site_default_set", "site_backup_restore", "certificate_import", "certificate_delete":
	default:
		return errors.New("unknown protected website journal operation")
	}
	certificateOperation := j.Operation == "certificate_import" || j.Operation == "certificate_delete"
	if certificateOperation && !safeID.MatchString(j.Certificate) || !certificateOperation && j.Certificate != "" {
		return errors.New("website journal certificate identity is invalid for its operation")
	}
	if (j.Operation == "site_backup_restore") != (j.Swap != nil) {
		return errors.New("website restore journal is missing its directory swap ownership")
	}
	if (j.Operation == "site_apply" || j.Operation == "site_delete" || j.Operation == "site_config_set" || j.Operation == "site_default_set" || j.Operation == "site_backup_restore") && len(j.Sites) == 0 {
		return errors.New("website journal has no protected site identity")
	}
	allowed, ids, err := e.journalPaths(ctx, j)
	if err != nil {
		return err
	}
	if !slices.Equal(ids, j.RuntimeIDs) {
		return errors.New("website journal runtime authority changed")
	}
	allowedKeys := map[string]bool{}
	for _, file := range allowed {
		allowedKeys[journalFileKey(file)] = true
	}
	seen, total := map[string]bool{}, 0
	for _, file := range j.Files {
		key := journalFileKey(file)
		if !allowedKeys[key] || seen[key] || !validJournalRelativePath(file.Path) ||
			file.Existed && (file.UID != os.Geteuid() || file.GID < 0 || file.Mode&^0777 != 0 || file.Mode&0022 != 0 || len(file.Before) > maxWebsiteJournalPayload) ||
			!file.Existed && (len(file.Before) != 0 || file.UID != 0 || file.GID != 0 || file.Mode != 0) {
			return errors.New("website journal file path or metadata is unauthorized")
		}
		seen[key] = true
		total += len(file.Before)
	}
	if total > maxWebsiteJournalPayload || len(seen) != len(allowed) {
		return errors.New("website journal snapshot set is invalid")
	}
	directoryPaths, err := journalDirectoryPaths(j)
	if err != nil {
		return err
	}
	if len(directoryPaths) != len(j.Directories) {
		return errors.New("website journal directory snapshot set is invalid")
	}
	for index, directory := range j.Directories {
		if directory.Path != directoryPaths[index] || !validJournalRelativePath(directory.Path) {
			return errors.New("website journal directory path is unauthorized")
		}
		if directory.Existed {
			if directory.UID < 0 || directory.GID < 0 || directory.Mode&^0777 != 0 || directory.Mode&0022 != 0 || directory.Inode == 0 {
				return errors.New("website journal directory metadata is invalid")
			}
		} else if directory.Mode != 0 || directory.UID != 0 || directory.GID != 0 || directory.Device != 0 || directory.Inode != 0 {
			return errors.New("absent website journal directory has unexpected metadata")
		}
	}
	if j.NewContent != nil && (j.Operation != "site_apply" || !safeID.MatchString(j.NewContent.ID) || j.NewContent.Dir.Inode != 0 && j.NewContent.Dir.Device == 0 || len(j.Sites) == 0 || j.Sites[0].ID != j.NewContent.ID) {
		return errors.New("website journal new content ownership is invalid")
	}
	if j.Operation == "site_apply" {
		if len(j.Sites) == 0 {
			return errors.New("website apply journal has no protected site identity")
		}
		statePath := filepath.Join("sites", j.Sites[0].ID+".json")
		found, existed := false, false
		for _, file := range j.Files {
			if file.Kind == "state" && file.Path == statePath {
				found, existed = true, file.Existed
				break
			}
		}
		if !found || !existed && j.NewContent == nil || existed && j.NewContent != nil {
			return errors.New("website apply journal new-content intent disagrees with its site receipt")
		}
	}
	if j.PendingWorker != nil && (j.Operation != "site_apply" || j.NewContent == nil || j.PendingWorker.SiteID != j.NewContent.ID || !safeID.MatchString(j.PendingWorker.SiteID) || j.PendingWorker.Username != "plsite_"+siteTag(j.PendingWorker.SiteID) || j.PendingWorker.Marker != pendingWorkerMarker(j.PendingWorker.SiteID, j.ID)) {
		return errors.New("website journal pending identity ownership is invalid")
	}
	if j.Swap != nil && (j.Operation != "site_backup_restore" || !safeID.MatchString(j.Swap.SiteID) || !safeID.MatchString(j.Swap.BackupID) || len(j.Sites) < 1 || j.Sites[0].ID != j.Swap.SiteID || j.Swap.Original.Inode == 0 || j.Swap.Staged.Inode != 0 && j.Swap.Staged.Device == 0) {
		return errors.New("website journal restore ownership is invalid")
	}
	return nil
}

func validJournalTempName(name string) bool {
	suffix := strings.TrimPrefix(name, ".powerlinux-")
	suffix = strings.TrimPrefix(suffix, "recover-")
	if suffix == name || len(suffix) != 32 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func (e *AgentExecutor) recoverWebsiteJournals(ctx context.Context) error {
	dir, err := e.state.Open("transactions")
	if err != nil {
		return err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(maxWebsiteJournals + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) > maxWebsiteJournals {
		return errors.New("website recovery inventory exceeds its limit")
	}
	for _, entry := range entries {
		path := filepath.Join("transactions", entry.Name())
		info, statErr := e.state.Lstat(path)
		stat, ok := (*syscall.Stat_t)(nil), false
		if statErr == nil {
			stat, ok = info.Sys().(*syscall.Stat_t)
		}
		if statErr != nil || !info.Mode().IsRegular() || !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
			return errors.New("invalid protected website recovery receipt metadata")
		}
		if strings.HasPrefix(entry.Name(), ".powerlinux-") {
			if !validJournalTempName(entry.Name()) {
				return errors.New("invalid temporary website recovery receipt")
			}
			if err = e.state.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") || info.Size() > maxWebsiteJournalBytes {
			return errors.New("invalid protected website recovery receipt size or name")
		}
		var j websiteJournal
		if rootJSON(e.state, path, &j) != nil {
			return errors.New("invalid protected website recovery receipt")
		}
		if err := e.validateJournal(ctx, j, entry.Name()); err != nil {
			return err
		}
		apply := func(locked context.Context) error {
			if j.Committed {
				return e.cleanupJournal(locked, j)
			}
			return e.rollbackJournal(locked, j)
		}
		if tx, ok := e.runtimes.(RuntimeTransactions); ok {
			err = tx.WithRuntimeTransaction(ctx, j.RuntimeIDs, apply)
		} else {
			err = apply(ctx)
		}
		if err != nil {
			return errors.Join(errors.New("website recovery requires a healthy protected runtime"), err)
		}
		if err = e.deleteJournal(j.ID); err != nil {
			return err
		}
	}
	if len(entries) > 0 {
		txdir, err := e.state.Open("transactions")
		if err != nil {
			return err
		}
		defer txdir.Close()
		if err = txdir.Sync(); err != nil {
			return err
		}
	}
	return nil
}

func (e *AgentExecutor) recordRestoreStagePath(ctx context.Context) (string, error) {
	active, ok := ctx.Value(websiteJournalKey{}).(*activeWebsiteJournal)
	if !ok || active.executor != e || active.record.Swap == nil {
		return "", errors.New("website restore requires its durable transaction")
	}
	_, _, stage := websiteRestorePaths(*active.record.Swap, active.record.ID)
	return stage, nil
}
