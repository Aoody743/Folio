package sites

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/files"
)

type AgentExecutor struct {
	config              AgentConfig
	runtimes            Runtimes
	state               *os.Root
	content             *os.Root
	files               *files.Service
	filesOwned          bool
	mu                  sync.Mutex
	acmeLocks           certificateLocks
	acmeAccounts        certificateLocks
	acmeHTTPClient      *http.Client // Trusted test fixture only; production uses system TLS roots.
	nativeDNSHTTPClient *http.Client // Trusted Go fixture only; never accepted from public input.
	nativeDNSLookup     func(context.Context, string) ([]string, error)
	backupStreams       backupStreamState
}

func AgentOperationNames() []string {
	return []string{"site_list", "site_get", "site_apply", "site_delete", "site_retained_list", "site_retained_purge", "site_default_set", "site_default_get", "site_logs", "site_config_get", "site_config_set", "site_backups_list", "site_backup_get", "site_backup_stat", "site_backup_artifact", "site_backup_create", "site_backup_restore", "site_backup_delete", "certificate_list", "certificate_get", "certificate_import", "certificate_delete", "certificate_export", "certificate_acme_issue", "certificate_acme_order", "site_php_cli_get", "site_php_cli_set"}
}

func NewAgentExecutor(config AgentConfig, runtimes Runtimes) (*AgentExecutor, error) {
	return NewAgentExecutorWithFiles(config, runtimes, nil)
}
func NewAgentExecutorWithFiles(config AgentConfig, runtimes Runtimes, sharedFiles *files.Service) (*AgentExecutor, error) {
	if os.Geteuid() != 0 || runtimes == nil {
		return nil, errors.New("website Agent requires root and protected runtime receipts")
	}
	if err := ValidateAgentConfig(config); err != nil {
		return nil, err
	}
	if len(config.AllowedPorts) == 0 {
		config.AllowedPorts = []int{80, 443}
	}
	for path, mode := range map[string]fs.FileMode{config.RootDir: 0755, config.StateDir: 0700} {
		if err := os.MkdirAll(path, mode); err != nil {
			return nil, err
		}
		if err := protectedParents(path); err != nil {
			return nil, err
		}
		if err := os.Chmod(path, mode); err != nil {
			return nil, err
		}
	}
	state, err := os.OpenRoot(config.StateDir)
	if err != nil {
		return nil, err
	}
	content, err := os.OpenRoot(config.RootDir)
	if err != nil {
		state.Close()
		return nil, err
	}
	e := &AgentExecutor{config: config, runtimes: runtimes, state: state, content: content}
	for _, directory := range []string{"sites", "retained", "certificates", "workers", "backups", "transactions", "acme"} {
		if err = state.MkdirAll(directory, 0700); err != nil {
			e.Close()
			return nil, err
		}
	}
	e.files = sharedFiles
	if e.files == nil {
		e.filesOwned = true
		e.files, err = files.New(files.Options{StateDir: filepath.Join(config.StateDir, "files"), ProtectedPaths: []string{config.StateDir, "/var/lib/powerlinux", "/etc/powerlinux"}})
	}
	if err != nil {
		e.Close()
		return nil, err
	}
	all, err := e.list()
	if err != nil {
		e.Close()
		return nil, err
	}
	for _, site := range all {
		if err = e.addFileRoot(site); err != nil {
			e.Close()
			return nil, err
		}
	}
	retained, err := e.retainedSites()
	if err != nil {
		e.Close()
		return nil, err
	}
	for _, value := range retained {
		site := value.Site
		if value.Purging {
			if _, err = content.Lstat(filepath.Join(site.ID, "public")); errors.Is(err, fs.ErrNotExist) {
				// A committed purge intent may already have removed the public
				// root. Keep its receipt visible so cleanup can resume explicitly.
				continue
			} else if err != nil {
				e.Close()
				return nil, err
			}
		}
		site.Name += " (retained)"
		if err = e.addFileRoot(site); err != nil {
			e.Close()
			return nil, err
		}
	}
	return e, nil
}
func (e *AgentExecutor) Close() error {
	var err error
	if e.files != nil && e.filesOwned {
		err = e.files.Close()
	}
	if e.state != nil {
		err = errors.Join(err, e.state.Close())
	}
	if e.content != nil {
		err = errors.Join(err, e.content.Close())
	}
	return err
}
func (e *AgentExecutor) FileService() *files.Service { return e.files }

func (e *AgentExecutor) Validate(job agent.Request) error {
	known := false
	for _, operation := range AgentOperationNames() {
		if operation == job.Operation {
			known = true
			break
		}
	}
	if !known {
		return agent.ErrUnauthorized
	}
	if job.ActorID == "" || len(job.Input) > 256<<10 {
		return agent.ErrInvalidInput
	}
	var in request
	if err := strict(job.Input, &in); err != nil {
		return err
	}
	if in.Actor.ID != job.ActorID {
		return denied()
	}
	if job.Operation == "site_list" || job.Operation == "site_retained_list" || job.Operation == "certificate_list" || job.Operation == "site_php_cli_get" {
		if in.ID != "" || in.Site != nil || in.Certificate != nil || in.KeyPEM != "" || in.Password != "" {
			return agent.ErrInvalidInput
		}
		return nil
	}
	if !safeID.MatchString(in.ID) || in.ExpectedRevision < 0 {
		return agent.ErrInvalidInput
	}
	if job.Operation == "site_backup_stat" || job.Operation == "site_backup_artifact" {
		return validateBackupArtifact(in)
	}
	if job.Operation == "site_apply" {
		if in.Site == nil || in.Site.ID != in.ID {
			return agent.ErrInvalidInput
		}
		return validateSite(in.Site)
	}
	return nil
}
func (e *AgentExecutor) Run(ctx context.Context, job agent.Request) (result agent.Result, err error) {
	result = agent.Result{Operation: job.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	defer func() {
		result.FinishedAt = time.Now().UTC()
		result.Canceled = errors.Is(err, context.Canceled)
		result.TimedOut = errors.Is(err, context.DeadlineExceeded)
		if err == nil {
			result.ExitCode = 0
		}
	}()
	if err = e.Validate(job); err != nil {
		return result, err
	}
	var in request
	_ = strict(job.Input, &in)
	if job.Operation == "certificate_acme_issue" || job.Operation == "certificate_acme_order" {
		unlock, lockErr := e.acmeLocks.acquire(ctx, in.ID)
		if lockErr != nil {
			return result, lockErr
		}
		defer unlock()
	} else if !readOperation(job.Operation) {
		e.mu.Lock()
		defer e.mu.Unlock()
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	var data any
	execute := func(locked context.Context) error {
		data, err = e.executeOperation(locked, job.Operation, in)
		return err
	}
	if !readOperation(job.Operation) && job.Operation != "certificate_acme_issue" && job.Operation != "certificate_acme_order" {
		err = e.withMutationRuntimes(ctx, job.Operation, in, execute)
	} else {
		err = execute(ctx)
	}
	result.Data = encode(data)
	return result, err
}

func (e *AgentExecutor) executeOperation(ctx context.Context, operation string, in request) (data any, err error) {
	switch operation {
	case "site_list":
		data, err = e.list()
	case "site_get":
		data, err = e.get(in.ID)
	case "site_apply":
		data, err = e.apply(ctx, in)
	case "site_delete":
		data, err = e.remove(ctx, in)
	case "site_retained_list":
		data, err = e.retainedSites()
	case "site_retained_purge":
		data, err = e.purgeRetained(ctx, in)
	case "site_default_set":
		data, err = e.setDefault(ctx, in)
	case "site_default_get":
		data, err = e.defaultSelection(in.ID)
	case "site_logs":
		data, err = e.logs(ctx, in)
	case "site_config_get":
		data, err = e.configGet(ctx, in.ID)
	case "site_config_set":
		data, err = e.configSet(ctx, in)
	case "site_backups_list":
		data, err = e.backups(in.ID)
	case "site_backup_get":
		data, err = e.backupMetadata(in)
	case "site_backup_stat":
		data, err = e.runBackupStat(ctx, in)
	case "site_backup_artifact":
		err = failure("stream_required", "Website archive export requires an output stream", 400)
	case "site_backup_create":
		data, err = e.backup(ctx, in)
	case "site_backup_restore":
		data, err = e.restore(ctx, in)
	case "site_backup_delete":
		data, err = e.deleteBackup(in)
	case "certificate_list":
		data, err = e.certificates()
	case "certificate_get":
		data, err = e.certificate(in.ID)
	case "certificate_import":
		data, err = e.importCertificate(ctx, in)
	case "certificate_delete":
		data, err = e.deleteCertificate(in)
	case "certificate_export":
		data, err = e.exportCertificate(in.ID)
	case "certificate_acme_issue":
		data, err = e.issueCertificate(ctx, in)
	case "certificate_acme_order":
		data, err = e.manualACME(ctx, in)
	case "site_php_cli_get":
		data, err = e.phpCLIGet(ctx)
	case "site_php_cli_set":
		data, err = e.phpCLISet(ctx, in.ID)
	default:
		err = agent.ErrUnauthorized
	}
	return data, err
}

func readOperation(operation string) bool {
	switch operation {
	case "site_list", "site_retained_list", "site_get", "site_default_get", "site_logs", "site_config_get", "site_backups_list", "site_backup_get", "site_backup_stat", "site_backup_artifact", "certificate_list", "certificate_get", "site_php_cli_get":
		return true
	}
	return false
}
func rootJSON(root *os.Root, path string, out any) error {
	file, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("invalid protected website record")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid protected website metadata")
	}
	return nil
}
func atomicRoot(root *os.Root, path string, data []byte, mode fs.FileMode) error {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return err
	}
	temp := filepath.Join(filepath.Dir(path), ".powerlinux-"+hex.EncodeToString(entropy[:]))
	file, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err = root.Rename(temp, path); err != nil {
		return err
	}
	dir, err := root.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func writeProtected(path string, data []byte) error {
	if err := protectedParents(filepath.Dir(path)); err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer root.Close()
	return atomicRoot(root, filepath.Base(path), data, 0600)
}
func (e *AgentExecutor) get(id string) (Site, error) {
	var site Site
	if !safeID.MatchString(id) {
		return site, invalid("Invalid website ID")
	}
	err := rootJSON(e.state, filepath.Join("sites", id+".json"), &site)
	if errors.Is(err, fs.ErrNotExist) {
		return site, failure("not_found", "Website was not found", 404)
	}
	if err != nil {
		return site, err
	}
	if site.ID != id {
		return site, errors.New("website receipt identity mismatch")
	}
	return site, nil
}
func (e *AgentExecutor) list() ([]Site, error) {
	directory, err := e.state.Open("sites")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	if len(entries) > 10000 {
		return nil, failure("too_many_sites", "Website collection exceeds its limit", 413)
	}
	out := []Site{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil, errors.New("unexpected website receipt")
		}
		site, err := e.get(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, site)
	}
	return out, nil
}
func (e *AgentExecutor) paths(site Site) renderedPaths {
	base := filepath.Join(e.config.RootDir, site.ID)
	return renderedPaths{DocumentRoot: filepath.Join(base, "public"), LogDir: filepath.Join(base, "logs"), RunDir: filepath.Join(base, "run"), TempDir: filepath.Join(base, "tmp"), PasswordFile: filepath.Join(base, "auth", "htpasswd"), Certificate: filepath.Join(e.config.StateDir, "certificates", site.TLS.CertificateID+".crt"), PrivateKey: filepath.Join(e.config.StateDir, "certificates", site.TLS.CertificateID+".key")}
}

func (e *AgentExecutor) identity(ctx context.Context, id string) (Identity, error) {
	var owned Identity
	path := filepath.Join("workers", id+".json")
	err := rootJSON(e.state, path, &owned)
	if err == nil {
		current, lookupErr := user.Lookup(owned.User)
		if lookupErr != nil || current.Uid != strconv.FormatUint(uint64(owned.UID), 10) || current.Gid != strconv.FormatUint(uint64(owned.GID), 10) {
			return owned, errors.New("website identity changed outside its ownership receipt")
		}
		return owned, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return owned, err
	}
	name := "plsite_" + siteTag(id)
	if _, err = user.Lookup(name); err == nil {
		return owned, failure("identity_conflict", "Website identity exists without a PowerLinux ownership receipt", 409)
	}
	program := e.config.Programs["useradd"]
	if program == "" {
		return owned, failure("policy_denied", "Website user provisioning is not enabled by the Agent policy", 403)
	}
	if err = trustedProgram(program); err != nil {
		return owned, err
	}
	command := exec.CommandContext(ctx, program, "--system", "--user-group", "--no-create-home", "--home-dir", filepath.Join(e.config.RootDir, id, "public"), "--shell", "/usr/sbin/nologin", name)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	if err = command.Run(); err != nil {
		return owned, failure("identity_failed", "Could not provision the isolated website identity", 500)
	}
	record, err := user.Lookup(name)
	if err != nil {
		return owned, err
	}
	uid, err := strconv.ParseUint(record.Uid, 10, 32)
	if err != nil || uid == 0 {
		return owned, errors.New("website identity must not be root")
	}
	gid, err := strconv.ParseUint(record.Gid, 10, 32)
	if err != nil {
		return owned, err
	}
	owned = Identity{User: name, Group: name, UID: uint32(uid), GID: uint32(gid)}
	if err = atomicRoot(e.state, path, encode(owned), 0600); err != nil {
		return owned, err
	}
	return owned, nil
}
func (e *AgentExecutor) prepareDirectories(site Site, worker Identity, web WebRuntime) error {
	group, err := user.LookupGroup(web.WorkerGroup)
	if err != nil {
		return err
	}
	webGID, err := strconv.Atoi(group.Gid)
	if err != nil {
		return err
	}
	webUser, err := user.Lookup(web.WorkerUser)
	if err != nil {
		return err
	}
	webUID, err := strconv.Atoi(webUser.Uid)
	if err != nil || webUID == 0 {
		return errors.New("Web worker must use an isolated non-root UID")
	}
	// The parent belongs to root. A PHP worker can write its public contents,
	// but cannot rename the public root or replace config/run directories.
	for path, spec := range map[string]struct {
		mode     fs.FileMode
		uid, gid int
	}{site.ID: {0751, 0, webGID}, filepath.Join(site.ID, "public"): {0750, int(worker.UID), webGID}, filepath.Join(site.ID, "logs"): {0750, 0, webGID}, filepath.Join(site.ID, "run"): {0750, 0, webGID}, filepath.Join(site.ID, "run", "proxy-cache"): {0700, webUID, webGID}, filepath.Join(site.ID, "auth"): {0750, 0, webGID}, filepath.Join(site.ID, "tmp"): {0700, int(worker.UID), int(worker.GID)}} {
		if err = e.content.MkdirAll(path, spec.mode); err != nil {
			return err
		}
		info, err := e.content.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("website directory is not an owned real directory")
		}
		if err = e.content.Chown(path, spec.uid, spec.gid); err != nil {
			return err
		}
		if err = e.content.Chmod(path, spec.mode); err != nil {
			return err
		}
	}
	return nil
}
func (e *AgentExecutor) addFileRoot(site Site) error {
	info, err := e.content.Stat(filepath.Join(site.ID, "public"))
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("website inode identity is unavailable")
	}
	return e.files.AddRoot(files.RootSpec{ID: "site-" + site.ID, Label: site.Name, Path: e.paths(site).DocumentRoot, DefaultFileMode: 0640, DefaultDirMode: 0750, ExpectedDevice: uint64(stat.Dev), ExpectedInode: stat.Ino})
}
func (e *AgentExecutor) validateTLS(site Site) error {
	if !site.TLS.Enabled {
		return nil
	}
	certificate, err := e.certificate(site.TLS.CertificateID)
	if err != nil {
		return err
	}
	if time.Now().Before(certificate.NotBefore) || !time.Now().Before(certificate.ExpiresAt) {
		return failure("certificate_invalid", "Website certificate is not currently valid", 409)
	}
	data, err := e.state.ReadFile(filepath.Join("certificates", certificate.ID+".crt"))
	if err != nil {
		return err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return errors.New("certificate receipt PEM is invalid")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	for _, value := range allSiteDomains(site) {
		if !certificateCovers(leaf, value) {
			return failure("certificate_name_mismatch", "Certificate does not cover every website domain", 409)
		}
	}
	return nil
}

type changedFile struct {
	path     string
	before   []byte
	existed  bool
	mode     fs.FileMode
	uid, gid int
}

func snapshotConfig(path string) (changedFile, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return changedFile{path: path}, nil
	}
	if err != nil {
		return changedFile{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return changedFile{}, errors.New("configuration target is not a regular owned file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
		return changedFile{}, errors.New("configuration target must belong to Agent UID and be protected")
	}
	before, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return changedFile{}, err
	}
	if len(before) > 1<<20 {
		return changedFile{}, errors.New("configuration snapshot exceeds one MiB")
	}
	return changedFile{path: path, before: before, existed: true, mode: info.Mode().Perm(), uid: int(stat.Uid), gid: int(stat.Gid)}, nil
}
func rollbackConfigs(changes []changedFile) error {
	var result error
	for i := len(changes) - 1; i >= 0; i-- {
		change := changes[i]
		if change.existed {
			err := writeProtected(change.path, change.before)
			if err == nil {
				err = os.Chown(change.path, change.uid, change.gid)
			}
			if err == nil {
				err = os.Chmod(change.path, change.mode)
			}
			result = errors.Join(result, err)
		} else {
			err := os.Remove(change.path)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				result = errors.Join(result, err)
			}
		}
	}
	return result
}

func (e *AgentExecutor) apply(ctx context.Context, in request) (site Site, err error) {
	site = *in.Site
	if err = validateSite(&site); err != nil {
		return site, err
	}
	old, getErr := e.get(site.ID)
	exists := getErr == nil
	if getErr != nil && !isNotFound(getErr) {
		return site, getErr
	}
	if exists && old.Revision != in.ExpectedRevision || !exists && in.ExpectedRevision != 0 {
		return site, failure("revision_conflict", "Website revision changed", 412)
	}
	if !exists {
		if err = e.rejectUnownedContent(site.ID); err != nil {
			return site, err
		}
	}
	if exists && site.WebInstance != old.WebInstance {
		return site, invalid("Changing the Web instance requires an explicit migration plan")
	}
	if exists && old.ConfigurationMode == "advanced" && !in.ResetAdvancedConfig {
		return site, failure("advanced_configuration_active", "Explicitly reset advanced configuration before applying typed website settings", 409)
	}
	site.ConfigurationMode = "managed"
	web, err := e.runtimes.Web(ctx, site.WebInstance)
	if err != nil {
		return site, err
	}
	if site.IPv6 && !e.config.AllowIPv6 {
		return site, failure("policy_denied", "IPv6 website listeners are disabled by this Agent policy", 403)
	}
	if site.TLS.Enabled {
		allowed := false
		for _, port := range e.config.AllowedPorts {
			if port == 443 {
				allowed = true
			}
		}
		if !allowed {
			return site, failure("policy_denied", "TLS listener port is outside the trusted policy", 403)
		}
	}
	for _, value := range allSiteDomains(site) {
		_, _, port, err := domain(value)
		if err != nil {
			return site, err
		}
		allowed := false
		for _, configured := range e.config.AllowedPorts {
			if configured == port {
				allowed = true
			}
		}
		if !allowed {
			return site, failure("policy_denied", "Website listener port is outside the trusted policy", 403)
		}
	}
	all, err := e.list()
	if err != nil {
		return site, err
	}
	for _, other := range all {
		if other.ID == site.ID || other.WebInstance != site.WebInstance {
			continue
		}
		for _, first := range allSiteDomains(other) {
			for _, second := range allSiteDomains(site) {
				if first == second {
					return site, failure("domain_conflict", "Domain already belongs to another website", 409)
				}
			}
		}
		if site.Default && other.Default {
			return site, failure("default_site_conflict", "Replace the existing default through the explicit default-site action", 409)
		}
	}
	if err = e.validateTLS(site); err != nil {
		return site, err
	}
	worker, err := e.identity(ctx, site.ID)
	if err != nil {
		return site, err
	}
	changes := []changedFile{}
	runtimeIDs := []string{site.WebInstance}
	committed, fileRootAdded := false, false
	defer func() {
		if committed {
			return
		}
		restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		restoreErr := rollbackConfigs(changes)
		for _, id := range runtimeIDs {
			restoreErr = errors.Join(restoreErr, e.runtimes.Reload(restoreCtx, id))
		}
		if !exists {
			removeContent := func() error {
				if fileRootAdded {
					if err := e.files.DeleteRoot("site-" + site.ID); err != nil {
						return err
					}
				}
				return e.content.RemoveAll(site.ID)
			}
			if fileRootAdded {
				restoreErr = errors.Join(restoreErr, e.files.RootTransaction(restoreCtx, "site-"+site.ID, removeContent))
			} else {
				restoreErr = errors.Join(restoreErr, removeContent())
			}
		}
		if restoreErr != nil {
			err = errors.Join(err, failure("rollback_incomplete", "Website update failed and its configuration needs recovery inspection", 500))
		}
	}()
	if err = e.prepareDirectories(site, worker, web); err != nil {
		return site, err
	}
	paths := e.paths(site)
	site.DocumentRoot = paths.DocumentRoot
	site.Revision = in.ExpectedRevision + 1
	root, err := e.content.OpenRoot(filepath.Join(site.ID, "public"))
	if err != nil {
		return site, err
	}
	runDirectory, openErr := root.OpenRoot(site.RunPath)
	root.Close()
	if runDirectory != nil {
		runDirectory.Close()
	}
	if openErr != nil {
		return site, failure("directory_missing", "Website run directory does not exist inside its public root", 409)
	}
	applyFile := func(path string, data []byte) error {
		before, err := snapshotConfig(path)
		if err != nil {
			return err
		}
		changes = append(changes, before)
		return writeProtected(path, data)
	}
	fpm := ""
	if site.PHPInstance != "" {
		php, err := e.runtimes.PHP(ctx, site.PHPInstance)
		if err != nil {
			return site, err
		}
		fpm = "unix:" + filepath.Join(paths.RunDir, "php-fpm.sock")
		body, err := renderPool(site, paths, worker, web)
		if err != nil {
			return site, err
		}
		if err = applyFile(filepath.Join(php.PoolDir, site.ID+".conf"), []byte(body)); err != nil {
			return site, err
		}
		runtimeIDs = append(runtimeIDs, site.PHPInstance)
	}
	if exists && old.PHPInstance != "" && old.PHPInstance != site.PHPInstance {
		php, err := e.runtimes.PHP(ctx, old.PHPInstance)
		if err != nil {
			return site, err
		}
		path := filepath.Join(php.PoolDir, site.ID+".conf")
		previous, err := snapshotConfig(path)
		if err != nil {
			return site, err
		}
		changes = append(changes, previous)
		if err = os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return site, err
		}
		runtimeIDs = append(runtimeIDs, old.PHPInstance)
	}
	if site.Protection.Enabled {
		if len(in.Password) < 8 || len(in.Password) > 72 || strings.ContainsAny(in.Password, "\x00\r\n") {
			return site, invalid("HTTP protection needs a bound password of eight to 72 bytes")
		}
		hash, err := basicPasswordHash(in.Password)
		if err != nil {
			return site, err
		}
		if err = applyFile(paths.PasswordFile, []byte(site.Protection.Username+":"+hash+"\n")); err != nil {
			return site, err
		}
		group, lookupErr := user.LookupGroup(web.WorkerGroup)
		if lookupErr != nil {
			return site, lookupErr
		}
		gid, parseErr := strconv.Atoi(group.Gid)
		if parseErr != nil {
			return site, parseErr
		}
		if err = os.Chown(paths.PasswordFile, 0, gid); err != nil {
			return site, err
		}
		if err = os.Chmod(paths.PasswordFile, 0640); err != nil {
			return site, err
		}
	}
	body, err := renderWebsite(site, paths, web, fpm, time.Now())
	if err != nil {
		return site, err
	}
	if err = applyFile(filepath.Join(web.SnippetsDir, site.ID+".conf"), []byte(body)); err != nil {
		return site, err
	}
	for _, id := range runtimeIDs {
		if err = e.runtimes.Check(ctx, id); err != nil {
			return site, failure("configuration_rejected", "Web/PHP rejected the staged website configuration", 409)
		}
	}
	for _, id := range runtimeIDs {
		if err = e.runtimes.Reload(ctx, id); err != nil {
			return site, failure("reload_failed", "Web/PHP did not accept the website reload", 500)
		}
	}
	site.Revision = in.ExpectedRevision + 1
	if verifier, ok := e.runtimes.(RuntimeVerifier); ok {
		if err = verifier.VerifySite(ctx, site); err != nil {
			return site, failure("runtime_not_applied", "Web runtime did not serve the requested website revision; configuration was restored", 502)
		}
	}
	site.UpdatedAt = time.Now().UTC()
	site.UpdatedBy = in.Actor.Public()
	if !exists {
		site.CreatedAt = site.UpdatedAt
	}
	if site.TLS.Enabled {
		cert, _ := e.certificate(site.TLS.CertificateID)
		site.TLS.ExpiresAt = cert.ExpiresAt.UTC().Format(time.RFC3339)
		site.TLS.CertificateRevision = cert.Revision
	}
	if !exists {
		if err = e.addFileRoot(site); err != nil {
			return site, err
		}
		fileRootAdded = true
	}
	if err = atomicRoot(e.state, filepath.Join("sites", site.ID+".json"), encode(site), 0600); err != nil {
		return site, err
	}
	committed = true
	return site, nil
}

func isNotFound(err error) bool {
	var typed interface{ AgentError() (string, int) }
	if errors.As(err, &typed) {
		_, status := typed.AgentError()
		return status == 404
	}
	return errors.Is(err, fs.ErrNotExist)
}

func (e *AgentExecutor) remove(ctx context.Context, in request) (result any, err error) {
	site, err := e.get(in.ID)
	if err != nil {
		return nil, err
	}
	if site.Revision != in.ExpectedRevision {
		return nil, failure("revision_conflict", "Website revision changed", 412)
	}
	web, err := e.runtimes.Web(ctx, site.WebInstance)
	if err != nil {
		return nil, err
	}
	targets := []string{filepath.Join(web.SnippetsDir, site.ID+".conf")}
	runtimeIDs := []string{site.WebInstance}
	if site.PHPInstance != "" {
		php, err := e.runtimes.PHP(ctx, site.PHPInstance)
		if err != nil {
			return nil, err
		}
		targets = append(targets, filepath.Join(php.PoolDir, site.ID+".conf"))
		runtimeIDs = append(runtimeIDs, site.PHPInstance)
	}
	changes := []changedFile{}
	// Capture all writable receipts before the first runtime change. An
	// unavailable receipt directory must not remove a still-active vhost.
	for _, path := range []string{filepath.Join(e.config.StateDir, "sites", site.ID+".json"), filepath.Join(e.config.StateDir, "retained", site.ID+".json")} {
		before, snapshotErr := snapshotConfig(path)
		if snapshotErr != nil {
			return nil, snapshotErr
		}
		changes = append(changes, before)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		recoverErr := rollbackConfigs(changes)
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		for _, id := range runtimeIDs {
			recoverErr = errors.Join(recoverErr, e.runtimes.Reload(recovery, id))
		}
		if recoverErr != nil {
			err = errors.Join(err, failure("rollback_incomplete", "Website removal failed and its owned configuration needs recovery inspection", 500))
		}
	}()
	for _, path := range targets {
		before, err := snapshotConfig(path)
		if err != nil {
			return nil, err
		}
		changes = append(changes, before)
		if err = os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	for _, id := range runtimeIDs {
		if err = e.runtimes.Check(ctx, id); err == nil {
			err = e.runtimes.Reload(ctx, id)
		}
		if err != nil {
			return nil, failure("delete_failed", "Website removal was rolled back after a failed runtime check", 500)
		}
	}
	// Keep ownership visible before committing deletion, including when the
	// requested next step is permanent content cleanup.
	retained := RetainedSite{Site: site, RetainedAt: time.Now().UTC()}
	if err = atomicRoot(e.state, filepath.Join("retained", site.ID+".json"), encode(retained), 0600); err != nil {
		return nil, err
	}
	if err = e.state.Remove(filepath.Join("sites", site.ID+".json")); err != nil {
		return nil, err
	}
	committed = true
	if !in.KeepData {
		if _, purgeErr := e.purgeRetained(ctx, request{ID: site.ID, ExpectedRevision: site.Revision}); purgeErr != nil {
			return map[string]any{"id": site.ID, "deleted": true, "data_retained": true}, failure("site_cleanup_required", "Website configuration was removed; its retained ownership receipt remains for content cleanup", 500)
		}
	}
	return map[string]any{"id": site.ID, "deleted": true, "data_retained": in.KeepData}, nil
}

func (e *AgentExecutor) logs(ctx context.Context, in request) (any, error) {
	site, err := e.get(in.ID)
	if err != nil {
		return nil, err
	}
	paths := e.paths(site)
	path := ""
	switch in.Kind {
	case "access":
		path = filepath.Join(paths.LogDir, "access.log")
	case "error":
		path = filepath.Join(paths.LogDir, "error.log")
	case "php":
		path = filepath.Join(paths.TempDir, "php-errors.log")
	default:
		return nil, invalid("Unknown website log")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{"lines": []string{}, "offset": in.Offset, "limit": in.Limit}, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, invalid("Website log must be a regular file")
	}
	size := min(info.Size(), int64(256<<10))
	if _, err = file.Seek(info.Size()-size, io.SeekStart); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, size))
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if in.Limit == 0 {
		in.Limit = 100
	}
	start := min(max(0, in.Offset), len(lines))
	end := min(start+in.Limit, len(lines))
	return map[string]any{"lines": lines[start:end], "total": len(lines), "offset": in.Offset, "limit": in.Limit, "truncated": info.Size() > size}, nil
}
func (e *AgentExecutor) configGet(ctx context.Context, id string) (any, error) {
	site, err := e.get(id)
	if err != nil {
		return nil, err
	}
	web, err := e.runtimes.Web(ctx, site.WebInstance)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(web.SnippetsDir, id+".conf")
	if err = protectedParents(filepath.Dir(path)); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > 64<<10 {
		return nil, failure("too_large", "Website configuration exceeds read budget", 413)
	}
	return map[string]any{"id": id, "revision": site.Revision, "config": string(data)}, nil
}
func (e *AgentExecutor) configSet(ctx context.Context, in request) (data any, err error) {
	if !e.config.AllowAdvancedConfig {
		return nil, failure("policy_denied", "Advanced Web configuration is disabled by the trusted Agent policy", 403)
	}
	site, err := e.get(in.ID)
	if err != nil {
		return nil, err
	}
	if site.Revision != in.ExpectedRevision {
		return nil, failure("revision_conflict", "Website revision changed", 412)
	}
	if len(in.Config) > 48<<10 || strings.ContainsRune(in.Config, 0) {
		return nil, invalid("Invalid advanced website configuration")
	}
	web, err := e.runtimes.Web(ctx, site.WebInstance)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(web.SnippetsDir, site.ID+".conf")
	previous, err := snapshotConfig(path)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		recoverErr := rollbackConfigs([]changedFile{previous})
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		recoverErr = errors.Join(recoverErr, e.runtimes.Reload(recovery, site.WebInstance))
		if recoverErr != nil {
			err = errors.Join(err, failure("rollback_incomplete", "Advanced configuration recovery needs inspection", 500))
		}
	}()
	if err = writeProtected(path, []byte(in.Config)); err != nil {
		return nil, err
	}
	if err = e.runtimes.Check(ctx, site.WebInstance); err == nil {
		err = e.runtimes.Reload(ctx, site.WebInstance)
	}
	if err != nil {
		return nil, failure("configuration_rejected", "Advanced Web configuration was rejected and restored", 409)
	}
	site.Revision++
	site.ConfigurationMode = "advanced"
	site.UpdatedAt = time.Now().UTC()
	site.UpdatedBy = in.Actor.Public()
	if err = atomicRoot(e.state, filepath.Join("sites", site.ID+".json"), encode(site), 0600); err != nil {
		return nil, err
	}
	committed = true
	return site, nil
}
