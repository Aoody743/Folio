package sites

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	migrationPlanVersion = 1
	migrationPlanTTL     = 30 * time.Minute
	maxMigrationPlans    = 128
)

type protectedMigrationPlan struct {
	Version             int       `json:"version"`
	ID                  string    `json:"id"`
	ActorID             string    `json:"actor_id"`
	Site                Site      `json:"site"`
	ExpectedRevision    int64     `json:"expected_revision"`
	SourceWebInstance   string    `json:"source_web_instance"`
	TargetWebInstance   string    `json:"target_web_instance"`
	SourceWebHash       string    `json:"source_web_hash"`
	TargetWebHash       string    `json:"target_web_hash"`
	PHPHash             string    `json:"php_hash,omitempty"`
	WorkerHash          string    `json:"worker_hash"`
	TLSHash             string    `json:"tls_hash,omitempty"`
	SourceConfigHash    string    `json:"source_config_hash"`
	ResetAdvancedConfig bool      `json:"reset_advanced_config"`
	CreatedAt           time.Time `json:"created_at"`
	ExpiresAt           time.Time `json:"expires_at"`
}

func validMigrationPlanID(id string) bool {
	if len(id) != 34 || !strings.HasPrefix(id, "m-") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(id, "m-"))
	return err == nil
}

func (e *AgentExecutor) verifyMigrationPlanDirectory() error {
	info, err := e.state.Lstat("migration-plans")
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		return errors.New("website migration plan directory must be private and owned by the Agent")
	}
	return nil
}

func (e *AgentExecutor) loadMigrationPlan(id string) (protectedMigrationPlan, error) {
	var plan protectedMigrationPlan
	if !validMigrationPlanID(id) {
		return plan, invalid("Invalid website migration plan")
	}
	if err := e.verifyMigrationPlanDirectory(); err != nil {
		return plan, err
	}
	path := filepath.Join("migration-plans", id+".json")
	info, err := e.state.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return plan, failure("migration_plan_not_found", "Website migration plan was not found", 404)
	}
	stat, ok := (*syscall.Stat_t)(nil), false
	if err == nil {
		stat, ok = info.Sys().(*syscall.Stat_t)
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 || info.Size() > 256<<10 {
		return plan, errors.New("invalid protected website migration plan metadata")
	}
	if err = rootJSON(e.state, path, &plan); err != nil {
		return plan, errors.New("invalid protected website migration plan")
	}
	if plan.Version != migrationPlanVersion || plan.ID != id || plan.ActorID == "" || !safeID.MatchString(plan.Site.ID) || plan.ExpectedRevision < 1 || plan.Site.Revision != plan.ExpectedRevision || plan.SourceWebInstance != plan.Site.WebInstance || !safeID.MatchString(plan.SourceWebInstance) || !safeID.MatchString(plan.TargetWebInstance) || plan.SourceWebInstance == plan.TargetWebInstance || len(plan.SourceWebHash) != 64 || len(plan.TargetWebHash) != 64 || len(plan.SourceConfigHash) != 64 || len(plan.WorkerHash) != 64 || plan.Site.PHPInstance == "" && plan.PHPHash != "" || plan.Site.PHPInstance != "" && len(plan.PHPHash) != 64 || plan.Site.TLS.Enabled && len(plan.TLSHash) != 64 || !plan.Site.TLS.Enabled && plan.TLSHash != "" || plan.CreatedAt.IsZero() || !plan.ExpiresAt.After(plan.CreatedAt) {
		return protectedMigrationPlan{}, errors.New("protected website migration plan identity is invalid")
	}
	return plan, nil
}

func migrationPlanView(plan protectedMigrationPlan) MigrationPlanView {
	return MigrationPlanView{
		ID: plan.ID, SiteID: plan.Site.ID, SiteName: plan.Site.Name,
		ExpectedRevision: plan.ExpectedRevision, SourceWebInstance: plan.SourceWebInstance,
		TargetWebInstance: plan.TargetWebInstance, PHPInstance: plan.Site.PHPInstance,
		TLSCertificateID: plan.Site.TLS.CertificateID, TLSCertificateRev: plan.Site.TLS.CertificateRevision,
		Domains: append([]string(nil), allSiteDomains(plan.Site)...), Default: plan.Site.Default,
		TLSEnabled: plan.Site.TLS.Enabled, ConfigurationMode: plan.Site.ConfigurationMode,
		ResetAdvancedConfig: plan.ResetAdvancedConfig, ExpiresAt: plan.ExpiresAt,
	}
}

func (e *AgentExecutor) migrationTLSHash(site Site) (string, error) {
	if !site.TLS.Enabled {
		return "", nil
	}
	certificate, err := e.certificate(site.TLS.CertificateID)
	if err != nil {
		return "", err
	}
	certificatePEM, err := e.readMigrationStateFile(filepath.Join("certificates", certificate.ID+".crt"), 64<<10)
	if err != nil {
		return "", err
	}
	privateKey, err := e.readMigrationStateFile(filepath.Join("certificates", certificate.ID+".key"), 16<<10)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write(encode(certificate))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(certificatePEM)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(privateKey)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (e *AgentExecutor) readMigrationStateFile(path string, limit int64) ([]byte, error) {
	file, err := e.state.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	stat, ok := (*syscall.Stat_t)(nil), false
	if err == nil {
		stat, ok = info.Sys().(*syscall.Stat_t)
	}
	if err != nil || !info.Mode().IsRegular() || !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 || info.Size() > limit {
		return nil, errors.New("website migration TLS receipt file is not protected")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("website migration TLS receipt file exceeds its limit")
	}
	return data, nil
}

func (e *AgentExecutor) getMigrationPlanView(id string) (MigrationPlanView, error) {
	plan, err := e.loadMigrationPlan(id)
	if err != nil {
		return MigrationPlanView{}, err
	}
	if !time.Now().Before(plan.ExpiresAt) {
		return MigrationPlanView{}, failure("migration_plan_expired", "Website migration plan expired; inspect and create a new plan", 410)
	}
	return migrationPlanView(plan), nil
}

func migrationWebIdentityHash(runtime WebRuntime, expectedID string) (string, error) {
	if runtime.ID != expectedID || runtime.Program == "" || runtime.MainConfig == "" || runtime.SnippetsDir == "" || runtime.Prefix == "" || runtime.WorkerUser == "" || runtime.WorkerGroup == "" {
		return "", failure("migration_runtime_unavailable", "Web runtime does not have a complete protected identity receipt", 409)
	}
	for _, path := range []string{runtime.Program, runtime.MainConfig, runtime.SnippetsDir, runtime.Prefix} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return "", failure("migration_runtime_unavailable", "Web runtime identity receipt contains an invalid protected path", 409)
		}
	}
	if err := protectedParents(runtime.SnippetsDir); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(runtime.SnippetsDir)
	if err != nil {
		return "", err
	}
	err = verifyJournalRoot(root)
	err = errors.Join(err, root.Close())
	if err != nil {
		return "", err
	}
	raw := encode(runtime)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}

func migrationPHPIdentityHash(runtime PHPRuntime, expectedID string) (string, error) {
	if runtime.ID != expectedID || runtime.Program == "" || runtime.MainConfig == "" || runtime.PoolDir == "" || runtime.Prefix == "" {
		return "", failure("migration_runtime_unavailable", "PHP runtime does not have a complete protected identity receipt", 409)
	}
	for _, path := range []string{runtime.Program, runtime.MainConfig, runtime.PoolDir, runtime.Prefix} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return "", failure("migration_runtime_unavailable", "PHP runtime identity receipt contains an invalid protected path", 409)
		}
	}
	if err := protectedParents(runtime.PoolDir); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(runtime.PoolDir)
	if err != nil {
		return "", err
	}
	err = verifyJournalRoot(root)
	err = errors.Join(err, root.Close())
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encode(runtime))
	return hex.EncodeToString(hash[:]), nil
}

func migrationConfigSnapshot(runtime WebRuntime, siteID string) (changedFile, error) {
	path := filepath.Join(runtime.SnippetsDir, siteID+".conf")
	if err := protectedParents(filepath.Dir(path)); err != nil {
		return changedFile{}, err
	}
	return snapshotConfig(path)
}

func (e *AgentExecutor) migrationWorkerIdentity(siteID string) (Identity, error) {
	var owned Identity
	err := rootJSON(e.state, filepath.Join("workers", siteID+".json"), &owned)
	if errors.Is(err, fs.ErrNotExist) {
		return owned, failure("migration_worker_missing", "Website worker ownership receipt is missing; migration cannot create or replace it", 409)
	}
	if err != nil {
		return owned, err
	}
	current, err := user.Lookup(owned.User)
	if err != nil || owned.UID == 0 || current.Uid != strconv.FormatUint(uint64(owned.UID), 10) || current.Gid != strconv.FormatUint(uint64(owned.GID), 10) {
		return owned, failure("migration_worker_changed", "Website worker identity no longer matches its protected receipt", 409)
	}
	group, err := user.LookupGroupId(strconv.FormatUint(uint64(owned.GID), 10))
	if err != nil || group.Name != owned.Group {
		return owned, failure("migration_worker_changed", "Website worker group no longer matches its protected receipt", 409)
	}
	return owned, nil
}

func migrationConfigHash(file changedFile) (string, error) {
	if !file.existed {
		return "", failure("migration_source_missing", "Source Web configuration is missing; migration cannot proceed", 409)
	}
	hash := sha256.Sum256(file.before)
	return hex.EncodeToString(hash[:]), nil
}

func (e *AgentExecutor) checkMigrationConflicts(site Site, targetWeb string) error {
	all, err := e.list()
	if err != nil {
		return err
	}
	for _, other := range all {
		if other.ID == site.ID || other.WebInstance != targetWeb {
			continue
		}
		for _, first := range allSiteDomains(other) {
			for _, second := range allSiteDomains(site) {
				if first == second {
					return failure("domain_conflict", "Domain already belongs to another website on the target Web instance", 409)
				}
			}
		}
		if site.Default && other.Default {
			return failure("default_site_conflict", "Target Web instance already has a default website", 409)
		}
	}
	return nil
}

func (e *AgentExecutor) validateMigrationTarget(ctx context.Context, site Site, source, target WebRuntime, targetID string) error {
	if source.WorkerUser != target.WorkerUser || source.WorkerGroup != target.WorkerGroup {
		return failure("migration_worker_mismatch", "Source and target Web instances use different worker identities; content ownership must be reviewed before migration", 409)
	}
	workerUser, err := user.Lookup(target.WorkerUser)
	_, groupErr := user.LookupGroup(target.WorkerGroup)
	workerUID, uidErr := strconv.ParseUint(workerUserID(workerUser), 10, 32)
	if err != nil || groupErr != nil || uidErr != nil || workerUID == 0 {
		return failure("migration_worker_mismatch", "Target Web worker account or group is unavailable or unsafe", 409)
	}
	if site.IPv6 && !e.config.AllowIPv6 {
		return failure("policy_denied", "IPv6 website listeners are disabled by this Agent policy", 403)
	}
	if site.TLS.Enabled {
		allowedTLS := false
		for _, port := range e.config.AllowedPorts {
			allowedTLS = allowedTLS || port == 443
		}
		if !allowedTLS {
			return failure("policy_denied", "TLS listener port is outside the trusted policy", 403)
		}
	}
	for _, value := range allSiteDomains(site) {
		_, _, port, err := domain(value)
		if err != nil {
			return err
		}
		allowed := false
		for _, configured := range e.config.AllowedPorts {
			if configured == port {
				allowed = true
			}
		}
		if !allowed {
			return failure("policy_denied", "Website listener port is outside the trusted policy", 403)
		}
	}
	if err := e.validateTLS(site); err != nil {
		return err
	}
	if err := e.checkMigrationConflicts(site, targetID); err != nil {
		return err
	}
	targetFile, err := migrationConfigSnapshot(target, site.ID)
	if err != nil {
		return err
	}
	if targetFile.existed {
		return failure("migration_target_conflict", "A configuration already exists for this website on the target Web instance", 409)
	}
	fpm := ""
	if site.PHPInstance != "" {
		fpm = "unix:" + filepath.Join(e.paths(site).RunDir, "php-fpm.sock")
	}
	copy := site
	copy.WebInstance = target.ID
	copy.ConfigurationMode = "managed"
	_, err = renderWebsite(copy, e.paths(copy), target, fpm, time.Now())
	return err
}

func workerUserID(value *user.User) string {
	if value == nil {
		return ""
	}
	return value.Uid
}

func (e *AgentExecutor) createMigrationPlan(ctx context.Context, in request) (MigrationPlanView, error) {
	current, err := e.get(in.ID)
	if err != nil {
		return MigrationPlanView{}, err
	}
	if current.Revision != in.ExpectedRevision {
		return MigrationPlanView{}, failure("revision_conflict", "Website revision changed", 412)
	}
	if current.WebInstance == in.TargetWebInstance {
		return MigrationPlanView{}, invalid("Choose a different target Web instance")
	}
	if current.ConfigurationMode == "advanced" && !in.ResetAdvancedConfig {
		return MigrationPlanView{}, failure("advanced_migration_choice_required", "Advanced configuration may reference source-runtime paths; explicitly choose to reset it before migration", 409)
	}
	if current.ConfigurationMode != "advanced" && in.ResetAdvancedConfig {
		return MigrationPlanView{}, invalid("Reset advanced configuration is only available for an advanced website")
	}
	source, err := e.runtimes.Web(ctx, current.WebInstance)
	if err != nil {
		return MigrationPlanView{}, err
	}
	target, err := e.runtimes.Web(ctx, in.TargetWebInstance)
	if err != nil {
		return MigrationPlanView{}, err
	}
	sourceHash, err := migrationWebIdentityHash(source, current.WebInstance)
	if err != nil {
		return MigrationPlanView{}, err
	}
	targetHash, err := migrationWebIdentityHash(target, in.TargetWebInstance)
	if err != nil {
		return MigrationPlanView{}, err
	}
	config, err := migrationConfigSnapshot(source, current.ID)
	if err != nil {
		return MigrationPlanView{}, err
	}
	configHash, err := migrationConfigHash(config)
	if err != nil {
		return MigrationPlanView{}, err
	}
	worker, err := e.migrationWorkerIdentity(current.ID)
	if err != nil {
		return MigrationPlanView{}, err
	}
	workerHash := sha256.Sum256(encode(worker))
	phpHash := ""
	if current.PHPInstance != "" {
		php, phpErr := e.runtimes.PHP(ctx, current.PHPInstance)
		if phpErr != nil {
			return MigrationPlanView{}, phpErr
		}
		phpHash, err = migrationPHPIdentityHash(php, current.PHPInstance)
		if err != nil {
			return MigrationPlanView{}, err
		}
	}
	tlsHash, err := e.migrationTLSHash(current)
	if err != nil {
		return MigrationPlanView{}, err
	}
	if err = e.validateMigrationTarget(ctx, current, source, target, in.TargetWebInstance); err != nil {
		return MigrationPlanView{}, err
	}
	if err = e.prepareMigrationPlanInventory(time.Now(), true, in.ResetAdvancedConfig); err != nil {
		return MigrationPlanView{}, err
	}
	var entropy [16]byte
	if _, err = rand.Read(entropy[:]); err != nil {
		return MigrationPlanView{}, err
	}
	now := time.Now().UTC()
	plan := protectedMigrationPlan{
		Version: migrationPlanVersion, ID: "m-" + hex.EncodeToString(entropy[:]), ActorID: in.Actor.ID,
		Site: current, ExpectedRevision: current.Revision, SourceWebInstance: current.WebInstance,
		TargetWebInstance: in.TargetWebInstance, SourceWebHash: sourceHash, TargetWebHash: targetHash,
		PHPHash: phpHash, WorkerHash: hex.EncodeToString(workerHash[:]), TLSHash: tlsHash, SourceConfigHash: configHash, ResetAdvancedConfig: in.ResetAdvancedConfig,
		CreatedAt: now, ExpiresAt: now.Add(migrationPlanTTL),
	}
	if err = atomicRoot(e.state, filepath.Join("migration-plans", plan.ID+".json"), encode(plan), 0600); err != nil {
		return MigrationPlanView{}, err
	}
	return migrationPlanView(plan), nil
}

func (e *AgentExecutor) prepareMigrationPlanInventory(now time.Time, creatingPlan, needsAdvancedBackup bool) error {
	if err := e.verifyMigrationPlanDirectory(); err != nil {
		return err
	}
	dir, err := e.state.Open("migration-plans")
	if err != nil {
		return err
	}
	entries, err := dir.ReadDir(maxMigrationPlans*2 + 1)
	closeErr := dir.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return errors.Join(err, closeErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if len(entries) > maxMigrationPlans*2 {
		return failure("migration_inventory_limit", "Website migration plan and backup inventory exceeds its protected limit", 429)
	}
	count, backups := 0, 0
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".powerlinux-") && validJournalTempName(name) {
			if err = e.state.Remove(filepath.Join("migration-plans", name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			continue
		}
		if strings.HasSuffix(name, ".original") {
			planID := strings.TrimSuffix(name, ".original")
			if !validMigrationPlanID(planID) {
				return errors.New("unexpected website migration backup entry")
			}
			info, statErr := e.state.Lstat(filepath.Join("migration-plans", name))
			stat, ok := (*syscall.Stat_t)(nil), false
			if statErr == nil {
				stat, ok = info.Sys().(*syscall.Stat_t)
			}
			if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 || info.Size() > 1<<20 {
				return errors.New("invalid protected advanced-configuration migration backup")
			}
			backups++
			continue
		}
		if !strings.HasSuffix(name, ".json") || entry.IsDir() {
			return errors.New("unexpected website migration plan entry")
		}
		id := strings.TrimSuffix(name, ".json")
		plan, loadErr := e.loadMigrationPlan(id)
		if loadErr != nil {
			return loadErr
		}
		if !now.Before(plan.ExpiresAt) {
			original := filepath.Join("migration-plans", id+".original")
			if _, backupErr := e.state.Lstat(original); backupErr == nil {
				continue
			} else if !errors.Is(backupErr, fs.ErrNotExist) {
				return backupErr
			}
			if err = e.state.Remove(filepath.Join("migration-plans", name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			continue
		}
		count++
	}
	if creatingPlan && count >= maxMigrationPlans {
		return failure("migration_plan_limit", "Too many active website migration plans; wait for existing plans to expire", 429)
	}
	if needsAdvancedBackup && backups >= maxMigrationPlans {
		return failure("migration_backup_limit", "Advanced-configuration migration backup inventory is full and needs administrator review", 429)
	}
	return nil
}

func (e *AgentExecutor) validateMigrationPlanForApply(ctx context.Context, plan protectedMigrationPlan, in request) (Site, WebRuntime, WebRuntime, error) {
	if !time.Now().Before(plan.ExpiresAt) {
		return Site{}, WebRuntime{}, WebRuntime{}, failure("migration_plan_expired", "Website migration plan expired; inspect and create a new plan", 410)
	}
	if plan.ActorID != in.Actor.ID {
		return Site{}, WebRuntime{}, WebRuntime{}, denied()
	}
	if in.ExpectedRevision != plan.ExpectedRevision {
		return Site{}, WebRuntime{}, WebRuntime{}, failure("revision_conflict", "Website revision changed", 412)
	}
	current, err := e.get(plan.Site.ID)
	if err != nil {
		return Site{}, WebRuntime{}, WebRuntime{}, err
	}
	if current.Revision != in.ExpectedRevision {
		return Site{}, WebRuntime{}, WebRuntime{}, failure("revision_conflict", "Website revision changed", 412)
	}
	if !bytes.Equal(encode(current), encode(plan.Site)) {
		return Site{}, WebRuntime{}, WebRuntime{}, failure("migration_source_changed", "Website settings changed after this migration was reviewed; create a new plan", 412)
	}
	source, err := e.runtimes.Web(ctx, plan.SourceWebInstance)
	if err != nil {
		return Site{}, WebRuntime{}, WebRuntime{}, err
	}
	target, err := e.runtimes.Web(ctx, plan.TargetWebInstance)
	if err != nil {
		return Site{}, WebRuntime{}, WebRuntime{}, err
	}
	sourceHash, err := migrationWebIdentityHash(source, plan.SourceWebInstance)
	if err != nil {
		return Site{}, WebRuntime{}, WebRuntime{}, err
	}
	targetHash, err := migrationWebIdentityHash(target, plan.TargetWebInstance)
	if err != nil {
		return Site{}, WebRuntime{}, WebRuntime{}, err
	}
	if sourceHash != plan.SourceWebHash || targetHash != plan.TargetWebHash {
		return Site{}, WebRuntime{}, WebRuntime{}, failure("migration_runtime_changed", "Source or target Web runtime identity changed after review; create a new plan", 412)
	}
	if current.PHPInstance != "" {
		php, phpErr := e.runtimes.PHP(ctx, current.PHPInstance)
		if phpErr != nil {
			return Site{}, WebRuntime{}, WebRuntime{}, phpErr
		}
		phpHash, hashErr := migrationPHPIdentityHash(php, current.PHPInstance)
		if hashErr != nil {
			return Site{}, WebRuntime{}, WebRuntime{}, hashErr
		}
		if phpHash != plan.PHPHash {
			return Site{}, WebRuntime{}, WebRuntime{}, failure("migration_runtime_changed", "PHP runtime identity changed after review; create a new plan", 412)
		}
	}
	tlsHash, err := e.migrationTLSHash(current)
	if err != nil {
		return Site{}, WebRuntime{}, WebRuntime{}, err
	}
	if tlsHash != plan.TLSHash {
		return Site{}, WebRuntime{}, WebRuntime{}, failure("migration_tls_changed", "TLS certificate changed after review; create a new plan", 412)
	}
	config, err := migrationConfigSnapshot(source, current.ID)
	if err != nil {
		return Site{}, WebRuntime{}, WebRuntime{}, err
	}
	configHash, err := migrationConfigHash(config)
	if err != nil || configHash != plan.SourceConfigHash {
		return Site{}, WebRuntime{}, WebRuntime{}, failure("migration_source_config_changed", "Source Web configuration changed after review; create a new plan", 412)
	}
	if current.ConfigurationMode == "advanced" && !plan.ResetAdvancedConfig || current.ConfigurationMode != "advanced" && plan.ResetAdvancedConfig {
		return Site{}, WebRuntime{}, WebRuntime{}, failure("migration_source_changed", "Advanced-configuration choice no longer matches the website", 412)
	}
	if err = e.validateMigrationTarget(ctx, current, source, target, plan.TargetWebInstance); err != nil {
		return Site{}, WebRuntime{}, WebRuntime{}, err
	}
	return current, source, target, nil
}

func (e *AgentExecutor) applyMigration(ctx context.Context, in request) (site Site, err error) {
	if !journalActive(ctx, e) {
		return site, failure("transaction_required", "Website migration requires its protected recovery journal", 503)
	}
	plan, err := e.loadMigrationPlan(in.PlanID)
	if err != nil {
		return site, err
	}
	current, source, target, err := e.validateMigrationPlanForApply(ctx, plan, in)
	if err != nil {
		return site, err
	}
	worker, err := e.migrationWorkerIdentity(current.ID)
	if err != nil {
		return site, err
	}
	workerHash := sha256.Sum256(encode(worker))
	if hex.EncodeToString(workerHash[:]) != plan.WorkerHash {
		return site, failure("migration_worker_changed", "Website worker identity changed after review; create a new plan", 412)
	}
	copy := current
	copy.WebInstance = target.ID
	copy.ConfigurationMode = "managed"
	copy.Revision = current.Revision + 1
	copy.UpdatedAt = time.Now().UTC()
	copy.UpdatedBy = in.Actor.Public()
	if copy.TLS.Enabled {
		cert, certErr := e.certificate(copy.TLS.CertificateID)
		if certErr != nil {
			return site, certErr
		}
		copy.TLS.ExpiresAt = cert.ExpiresAt.UTC().Format(time.RFC3339)
		copy.TLS.CertificateRevision = cert.Revision
	}
	fpm := ""
	if copy.PHPInstance != "" {
		fpm = "unix:" + filepath.Join(e.paths(copy).RunDir, "php-fpm.sock")
	}
	body, err := renderWebsite(copy, e.paths(copy), target, fpm, time.Now())
	if err != nil {
		return site, err
	}
	sourcePath := filepath.Join(source.SnippetsDir, copy.ID+".conf")
	targetPath := filepath.Join(target.SnippetsDir, copy.ID+".conf")
	sourceSnapshot, err := snapshotConfig(sourcePath)
	if err != nil {
		return site, err
	}
	sourceConfigHash, err := migrationConfigHash(sourceSnapshot)
	if err != nil || sourceConfigHash != plan.SourceConfigHash {
		return site, failure("migration_source_config_changed", "Source Web configuration changed after review; create a new plan", 412)
	}
	targetSnapshot, err := snapshotConfig(targetPath)
	if err != nil {
		return site, err
	}
	if !sourceSnapshot.existed || targetSnapshot.existed {
		return site, failure("migration_config_conflict", "Source or target Web configuration changed before migration", 412)
	}
	if plan.ResetAdvancedConfig && current.ConfigurationMode == "advanced" {
		if err = e.prepareMigrationPlanInventory(time.Now(), false, true); err != nil {
			return site, err
		}
		backupPath := filepath.Join("migration-plans", plan.ID+".original")
		if _, statErr := e.state.Lstat(backupPath); statErr == nil {
			return site, failure("migration_backup_conflict", "An original advanced-configuration backup already exists for this plan", 409)
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return site, statErr
		}
		if err = atomicRoot(e.state, backupPath, sourceSnapshot.before, 0600); err != nil {
			return site, err
		}
	}
	changed := false
	defer func() {
		if !changed || journalActive(ctx, e) {
			return
		}
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		restoreErr := rollbackConfigs([]changedFile{targetSnapshot, sourceSnapshot})
		for _, id := range migrationRollbackRuntimeIDs(copy, source.ID) {
			restoreErr = errors.Join(restoreErr, e.runtimes.Reload(recovery, id))
		}
		if restoreErr != nil {
			err = errors.Join(err, failure("rollback_incomplete", "Website migration failed and needs recovery inspection", 500))
		}
	}()
	if err = writeProtected(targetPath, []byte(body)); err != nil {
		return site, err
	}
	changed = true
	if err = os.Remove(sourcePath); err != nil {
		return site, err
	}
	if err = syncRuntimeDirectory(source.SnippetsDir); err != nil {
		return site, err
	}
	if err = syncRuntimeDirectory(target.SnippetsDir); err != nil {
		return site, err
	}
	ids := migrationRuntimeIDs(copy, source.ID)
	for _, id := range ids {
		if err = e.runtimes.Check(ctx, id); err != nil {
			return site, failure("configuration_rejected", "Source, target Web or PHP rejected the staged migration configuration", 409)
		}
	}
	for _, id := range migrationApplyRuntimeIDs(copy, source.ID) {
		if err = e.runtimes.Reload(ctx, id); err != nil {
			return site, failure("reload_failed", "Source, target Web or PHP did not accept the migration reload", 500)
		}
	}
	if verifier, ok := e.runtimes.(RuntimeVerifier); ok {
		if err = verifier.VerifySite(ctx, copy); err != nil {
			return site, failure("runtime_not_applied", "Target Web runtime did not serve the migrated website revision", 502)
		}
	}
	_ = worker // The protected site worker receipt and content tree are deliberately preserved.
	if err = atomicRoot(e.state, filepath.Join("sites", copy.ID+".json"), encode(copy), 0600); err != nil {
		return site, err
	}
	return copy, nil
}

func migrationRuntimeIDs(site Site, sourceWeb string) []string {
	ids := []string{site.WebInstance}
	if sourceWeb != "" {
		ids = append(ids, sourceWeb)
	}
	if site.PHPInstance != "" {
		ids = append(ids, site.PHPInstance)
	}
	sort.Strings(ids)
	return slices.Compact(ids)
}

// Migration resources stay sorted for authorization and transaction locking.
// Runtime effects have a different constraint: release the old Web listener
// before asking the destination Web to claim it. Rollback reverses that order.
func migrationApplyRuntimeIDs(site Site, sourceWeb string) []string {
	return orderedUniqueRuntimeIDs(sourceWeb, site.WebInstance, site.PHPInstance)
}

func migrationRollbackRuntimeIDs(site Site, sourceWeb string) []string {
	return orderedUniqueRuntimeIDs(site.WebInstance, sourceWeb, site.PHPInstance)
}

func orderedUniqueRuntimeIDs(ids ...string) []string {
	result := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

func syncRuntimeDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	return errors.Join(syncErr, directory.Close())
}
