package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

const phpExtensionPlanTTL = 30 * time.Minute
const phpExtensionPlanTTLTolerance = time.Second
const phpExtensionApplyJobOperation = "php_extension_apply_job"

type phpExtensionRecoveryMarkerKey struct{}

type phpExtensionRecoveryMarker struct {
	Executor             *PrivilegedExecutor
	InstanceID           string
	PlanID               string
	IdempotencyKeySHA256 string
	IntentSHA256         string
	ExpectedRevision     int64
}

// Package ownership and module enablement are shared across instances that use
// the same PHP ABI, so serialize extension transactions across those instances.
var phpExtensionMutationMu sync.Mutex

var phpExtensionIdempotencyKey = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)
var phpExtensionAPISuffix = regexp.MustCompile(`([0-9]{8})$`)

// Module IDs are stable API names. Package names are compiled into the trusted
// Agent policy below and can never come from an API request.
var phpExtensionPackages = map[string]string{
	"bcmath":    "bcmath",
	"curl":      "curl",
	"gd":        "gd",
	"intl":      "intl",
	"mbstring":  "mbstring",
	"mysqli":    "mysql",
	"pdo_mysql": "mysql",
	"pdo_pgsql": "pgsql",
	"pgsql":     "pgsql",
	"redis":     "redis",
	"soap":      "soap",
	"sqlite3":   "sqlite3",
	"xml":       "xml",
	"zip":       "zip",
}

type phpExtensionPlatformRecipe struct {
	Minor string
	API   string
}

func phpExtensionPlatform(platform Platform) (phpExtensionPlatformRecipe, bool) {
	if platform.OS != "linux" || (platform.Arch != "amd64" && platform.Arch != "arm64") {
		return phpExtensionPlatformRecipe{}, false
	}
	switch {
	case platform.Distro == "debian" && platform.Release == "12":
		return phpExtensionPlatformRecipe{Minor: "8.2", API: "20220829"}, true
	case platform.Distro == "ubuntu" && platform.Release == "24.04":
		return phpExtensionPlatformRecipe{Minor: "8.3", API: "20230831"}, true
	case platform.Distro == "debian" && platform.Release == "13":
		return phpExtensionPlatformRecipe{Minor: "8.4", API: "20240924"}, true
	default:
		return phpExtensionPlatformRecipe{}, false
	}
}

func phpExtensionModuleIDs() []string {
	ids := make([]string, 0, len(phpExtensionPackages))
	for id := range phpExtensionPackages {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func supportedPHPModuleID(id string) bool {
	_, ok := phpExtensionPackages[id]
	return ok
}

func phpExtensionPackage(minor, moduleID string) (string, error) {
	suffix, ok := phpExtensionPackages[moduleID]
	if !ok || (minor != "8.2" && minor != "8.3" && minor != "8.4") {
		return "", failure("extension_recipe_unavailable", "No trusted signed package recipe is available for this PHP module", 412)
	}
	return "php" + minor + "-" + suffix, nil
}

type phpExtensionABI struct {
	Version      string `json:"version"`
	VersionID    int    `json:"version_id"`
	ExtensionDir string `json:"extension_dir"`
	ModuleAPI    string `json:"module_api"`
	ZTS          bool   `json:"zts"`
}

type phpExtensionAffectedProfile struct {
	ID               string `json:"id"`
	PHPVersion       string `json:"php_version"`
	ModuleEnabled    bool   `json:"module_enabled"`
	PackageReference bool   `json:"package_reference"`
}

// PHPModulePlan is the public, reviewable summary. The plan record stored by
// the Agent also binds its ABI and pre-install package state.
type PHPModulePlan struct {
	PlanID                string                        `json:"plan_id"`
	ID                    string                        `json:"id"`
	Operation             string                        `json:"operation"`
	ModuleID              string                        `json:"module_id"`
	PHPVersion            string                        `json:"php_version"`
	PHPModuleAPI          string                        `json:"php_module_api"`
	Platform              Platform                      `json:"platform"`
	PackagePins           []PackagePin                  `json:"package_pins"`
	AffectedProfiles      []phpExtensionAffectedProfile `json:"affected_profiles"`
	Changes               []Change                      `json:"changes"`
	Resources             []string                      `json:"resources"`
	ExpectedRevision      int64                         `json:"expected_revision"`
	CreatedAt             time.Time                     `json:"created_at"`
	ExpiresAt             time.Time                     `json:"expires_at"`
	AlreadyInstalled      bool                          `json:"already_installed"`
	AlreadyEnabled        bool                          `json:"already_enabled"`
	PackageRemovable      bool                          `json:"package_removable"` // Always false until shared PHP writers can be atomically fenced.
	PackageRetainedReason string                        `json:"package_retained_reason,omitempty"`
}

type phpExtensionPlanRecord struct {
	Public                    PHPModulePlan `json:"public"`
	PackageName               string        `json:"package_name"`
	ExtensionDirSHA256        string        `json:"extension_dir_sha256"`
	TargetPackageWasInstalled bool          `json:"target_package_was_installed"`
	TargetPackageVersion      string        `json:"target_package_version,omitempty"`
	OwnedPackageAtPlan        bool          `json:"owned_package_at_plan"`
}

type phpExtensionIntent struct {
	PlanID                string                 `json:"plan_id"`
	Phase                 string                 `json:"phase"`
	Plan                  phpExtensionPlanRecord `json:"plan"`
	Previous              Instance               `json:"previous"`
	Desired               Instance               `json:"desired"`
	PackageRetainedReason string                 `json:"package_retained_reason,omitempty"`
	StartedAt             time.Time              `json:"started_at"`
	UpdatedAt             time.Time              `json:"updated_at"`
}

type phpExtensionState struct {
	Version    int                       `json:"version"`
	InstanceID string                    `json:"instance_id"`
	References map[string][]string       `json:"references,omitempty"`
	Pending    *phpExtensionIntent       `json:"pending,omitempty"`
	Completed  []phpExtensionApplyResult `json:"completed,omitempty"`
}

type phpExtensionApplyResult struct {
	PlanID               string         `json:"plan_id"`
	IdempotencyKeySHA256 string         `json:"idempotency_key_sha256"`
	Result               map[string]any `json:"result"`
	CompletedAt          time.Time      `json:"completed_at"`
}

type phpExtensionPackageReceipt struct {
	Version          int        `json:"version"`
	Package          string     `json:"package"`
	Pin              PackagePin `json:"pin"`
	InstalledByAgent bool       `json:"installed_by_agent"`
	Installed        bool       `json:"installed"`
	SourceInstanceID string     `json:"source_instance_id,omitempty"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

func phpExtensionStateKey(id string) string {
	hash := sha256.Sum256([]byte(id))
	return "phpx_" + hex.EncodeToString(hash[:20])
}

func phpExtensionPackageKey(packageName string) string {
	hash := sha256.Sum256([]byte(packageName))
	return "phppkg_" + hex.EncodeToString(hash[:20])
}

func (e *PrivilegedExecutor) loadPHPExtensionState(id string) (phpExtensionState, error) {
	state := phpExtensionState{Version: 1, InstanceID: id, References: map[string][]string{}}
	err := e.load("private", phpExtensionStateKey(id), &state)
	if core.ErrorCode(err) == "not_found" {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if state.Version != 1 || state.InstanceID != id || len(state.References) > 64 || len(state.Completed) > 16 {
		return state, failure("extension_state_invalid", "Protected PHP extension state is invalid", 412)
	}
	for name, modules := range state.References {
		if !packageName.MatchString(name) || len(modules) > 32 {
			return state, failure("extension_state_invalid", "Protected PHP extension package reference is invalid", 412)
		}
		seen := map[string]bool{}
		for _, module := range modules {
			if !supportedPHPModuleID(module) || seen[module] {
				return state, failure("extension_state_invalid", "Protected PHP extension module reference is invalid", 412)
			}
			seen[module] = true
		}
	}
	if state.Pending != nil && (!validPlanID(state.Pending.PlanID) || state.Pending.Plan.Public.PlanID != state.Pending.PlanID || state.Pending.Plan.Public.PackageRemovable || state.Pending.Phase == "" || state.Pending.StartedAt.IsZero()) {
		return state, failure("extension_state_invalid", "Protected PHP extension intent is invalid", 412)
	}
	for _, completed := range state.Completed {
		if !validPlanID(completed.PlanID) || !artifactHash.MatchString(completed.IdempotencyKeySHA256) || completed.Result == nil || completed.CompletedAt.IsZero() {
			return state, failure("extension_state_invalid", "Protected PHP extension idempotency receipt is invalid", 412)
		}
	}
	return state, nil
}

// ensurePHPExtensionFinalized blocks other mutations while an extension package
// transaction is pending. Only the typed extension_apply path can attach the
// private exact-plan recovery marker, and applyPHPExtension rechecks the journal.
func (e *PrivilegedExecutor) ensurePHPExtensionFinalized(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validInstanceID(id) {
		return nil
	}
	var instance Instance
	err := e.load("receipts", id, &instance)
	if core.ErrorCode(err) == "not_found" {
		state, stateErr := e.loadPHPExtensionState(id)
		if stateErr != nil {
			return stateErr
		}
		if state.Pending != nil {
			return failure("extension_state_invalid", "Protected PHP extension intent has no matching managed software receipt", 412)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if instance.ID != id {
		return failure("ownership_invalid", "Software receipt disagrees with its extension guard identity", 412)
	}
	if !isPHPRuntime(instance.PluginID) {
		return nil
	}
	state, err := e.loadPHPExtensionState(id)
	if err != nil || state.Pending == nil {
		return err
	}
	if _, err = e.validatePHPExtensionPendingIntent(ctx, id, *state.Pending); err != nil {
		return err
	}
	marker, ok := ctx.Value(phpExtensionRecoveryMarkerKey{}).(phpExtensionRecoveryMarker)
	intentDigest, digestErr := phpExtensionIntentDigest(*state.Pending)
	if digestErr != nil {
		return digestErr
	}
	if ok && marker.Executor == e && marker.InstanceID == id && marker.PlanID == state.Pending.PlanID && marker.ExpectedRevision == state.Pending.Previous.Revision && marker.IntentSHA256 == intentDigest && artifactHash.MatchString(marker.IdempotencyKeySHA256) {
		return nil
	}
	return failure("extension_recovery_required", "A PHP extension package transaction is unfinished; resume its exact plan before changing this instance", 409)
}

func phpExtensionIntentDigest(intent phpExtensionIntent) (string, error) {
	raw, err := json.Marshal(intent)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (e *PrivilegedExecutor) phpExtensionRecoveryContext(ctx context.Context, input PHPInput) (context.Context, error) {
	state, err := e.loadPHPExtensionState(input.ID)
	if err != nil || state.Pending == nil {
		return ctx, err
	}
	intent := *state.Pending
	if intent.PlanID != input.PlanID || intent.Previous.ID != input.ID || intent.Previous.Revision != input.ExpectedRevision {
		return ctx, nil
	}
	intentDigest, err := phpExtensionIntentDigest(intent)
	if err != nil {
		return nil, err
	}
	marker := phpExtensionRecoveryMarker{
		Executor:   e,
		InstanceID: input.ID, PlanID: input.PlanID,
		IdempotencyKeySHA256: hashPHPIdempotencyKey(input.IdempotencyKey),
		IntentSHA256:         intentDigest, ExpectedRevision: input.ExpectedRevision,
	}
	return context.WithValue(ctx, phpExtensionRecoveryMarkerKey{}, marker), nil
}

func (e *PrivilegedExecutor) savePHPExtensionState(state phpExtensionState) error {
	if state.Version != 1 || !validInstanceID(state.InstanceID) || len(state.References) > 64 || len(state.Completed) > 16 {
		return failure("extension_state_invalid", "PHP extension state cannot be saved", 412)
	}
	state.Version = 1
	return e.save("private", phpExtensionStateKey(state.InstanceID), state)
}

func (e *PrivilegedExecutor) loadPHPExtensionPackage(packageName string) (phpExtensionPackageReceipt, bool, error) {
	var receipt phpExtensionPackageReceipt
	err := e.load("private", phpExtensionPackageKey(packageName), &receipt)
	if core.ErrorCode(err) == "not_found" {
		return receipt, false, nil
	}
	if err != nil {
		return receipt, false, err
	}
	if receipt.Version != 1 || receipt.Package != packageName || receipt.Pin.Name != packageName || receipt.InstalledByAgent && (receipt.Pin.Version == "" || !artifactHash.MatchString(receipt.Pin.SHA256)) {
		return receipt, false, failure("extension_state_invalid", "Protected PHP module package receipt is invalid", 412)
	}
	return receipt, true, nil
}

func (e *PrivilegedExecutor) savePHPExtensionPackage(receipt phpExtensionPackageReceipt) error {
	if receipt.Version != 1 || !packageName.MatchString(receipt.Package) || receipt.Pin.Name != receipt.Package || !artifactHash.MatchString(receipt.Pin.SHA256) {
		return failure("extension_state_invalid", "PHP module package receipt cannot be saved", 412)
	}
	receipt.UpdatedAt = time.Now().UTC()
	return e.save("private", phpExtensionPackageKey(receipt.Package), receipt)
}

func trustedPHPRepository(platform Platform, raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" || parsed.Port() != "" || (parsed.Path != "" && parsed.Path != "/") {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if platform.Distro == "debian" {
		return host == "deb.debian.org" || host == "security.debian.org" || host == "ftp.debian.org" || host == "cdn-fastly.deb.debian.org"
	}
	if platform.Distro == "ubuntu" {
		return host == "archive.ubuntu.com" || host == "security.ubuntu.com" || host == "ports.ubuntu.com"
	}
	return false
}

func (e *PrivilegedExecutor) phpExtensionRuntime(ctx context.Context, instance Instance) (phpExtensionPlatformRecipe, Platform, phpExtensionABI, error) {
	platform := CurrentPlatform()
	recipe, ok := phpExtensionPlatform(platform)
	if !ok {
		return recipe, platform, phpExtensionABI{}, failure("extension_recipe_unavailable", "Trusted PHP module packages are supported only on Debian 12 PHP 8.2, Ubuntu 24.04 PHP 8.3 and Debian 13 PHP 8.4", 412)
	}
	if instance.PluginID != "php-apt" || instance.Status == "uninstalled" || instance.Ownership != "managed" {
		return recipe, platform, phpExtensionABI{}, failure("extension_recipe_unavailable", "Only an installed Agent-managed Debian/Ubuntu PHP APT runtime has a trusted extension package recipe", 412)
	}
	manifest, err := e.catalog.Get(instance.PluginID)
	if err != nil {
		return recipe, platform, phpExtensionABI{}, err
	}
	version, err := manifest.SelectVersion(instance.SourceVersion)
	if err != nil || version.Version != recipe.Minor || !manifest.Supports(platform) {
		return recipe, platform, phpExtensionABI{}, failure("extension_recipe_unavailable", "The installed PHP version does not match this distribution's trusted module ABI", 412)
	}
	if instance.Service.Kind != "daemon" || instance.Service.Program == "" {
		return recipe, platform, phpExtensionABI{}, failure("runtime_unavailable", "A managed PHP-FPM service is required for extension installation", 412)
	}
	if filepath.Clean(instance.Service.Program) != "/usr/sbin/php-fpm"+recipe.Minor {
		return recipe, platform, phpExtensionABI{}, failure("extension_runtime_unverified", "The managed FPM executable is not the exact distro PHP ABI selected by the trusted package recipe", 412)
	}
	if len(instance.Packages) == 0 {
		return recipe, platform, phpExtensionABI{}, failure("extension_runtime_unverified", "The PHP receipt does not contain signed package identity pins", 412)
	}
	fpmPackage := "php" + recipe.Minor + "-fpm"
	cliPackage := "php" + recipe.Minor + "-cli"
	var fpmPin, cliPin *PackagePin
	for _, pin := range instance.Packages {
		if strings.HasPrefix(pin.Name, "php") && !trustedPHPRepository(platform, pin.Repository) {
			return recipe, platform, phpExtensionABI{}, failure("extension_runtime_unverified", "The installed PHP packages did not come from this distribution's signed repositories", 412)
		}
		if pin.Name == fpmPackage && pin.Architecture == platform.Arch && trustedPHPRepository(platform, pin.Repository) && artifactHash.MatchString(pin.SHA256) && debVersion.MatchString(pin.Version) {
			copy := pin
			fpmPin = &copy
		}
		if pin.Name == cliPackage && pin.Architecture == platform.Arch && trustedPHPRepository(platform, pin.Repository) && artifactHash.MatchString(pin.SHA256) && debVersion.MatchString(pin.Version) {
			copy := pin
			cliPin = &copy
		}
	}
	if fpmPin == nil || cliPin == nil {
		return recipe, platform, phpExtensionABI{}, failure("extension_runtime_unverified", "The installed PHP-FPM and CLI package pins are not proven by the official signed distribution repositories", 412)
	}
	for _, pin := range []*PackagePin{fpmPin, cliPin} {
		version, installed, packageErr := installedDebPackageVersion(ctx, pin.Name)
		if packageErr != nil || !installed || version != pin.Version {
			return recipe, platform, phpExtensionABI{}, failure("extension_runtime_unverified", "Installed PHP package versions do not match their signed Agent receipt", 412)
		}
	}
	program, err := phpCLI(instance)
	if err != nil {
		return recipe, platform, phpExtensionABI{}, err
	}
	if filepath.Clean(program) != "/usr/bin/php"+recipe.Minor {
		return recipe, platform, phpExtensionABI{}, failure("extension_runtime_unverified", "The managed CLI executable is not the exact distro PHP ABI selected by the trusted package recipe", 412)
	}
	if err = requirePHPExecutablePackage(ctx, instance.Service.Program, fpmPackage); err != nil {
		return recipe, platform, phpExtensionABI{}, err
	}
	if err = requirePHPExecutablePackage(ctx, program, cliPackage); err != nil {
		return recipe, platform, phpExtensionABI{}, err
	}
	abi, err := e.probePHPExtensionABI(ctx, instance, program)
	if err != nil {
		return recipe, platform, phpExtensionABI{}, err
	}
	if !strings.HasPrefix(abi.Version, recipe.Minor+".") || abi.ZTS || abi.ModuleAPI != recipe.API || abi.ExtensionDir == "" {
		return recipe, platform, phpExtensionABI{}, failure("extension_abi_mismatch", "The installed PHP CLI version or module API does not match the trusted distribution ABI", 412)
	}
	if err = e.checkOwnedPHPService(instance); err != nil {
		return recipe, platform, phpExtensionABI{}, err
	}
	return recipe, platform, abi, nil
}

func (e *PrivilegedExecutor) checkOwnedPHPService(instance Instance) error {
	if !ownedUnit(instance) {
		return failure("ownership_invalid", "PHP extension changes require the exact protected FPM service receipt", 412)
	}
	if e.config.UseSystemd {
		return checkOwnedUnit(instance)
	}
	return nil
}

func (e *PrivilegedExecutor) probePHPExtensionABI(ctx context.Context, instance Instance, program string) (phpExtensionABI, error) {
	var result phpExtensionABI
	code := `echo PHP_VERSION,"|",PHP_VERSION_ID,"|",PHP_EXTENSION_DIR,"|",(PHP_ZTS?1:0);`
	output, err := runFixedEnvironment(ctx, program, []string{"-n", "-r", code}, 10*time.Second, e.phpCredential(), nil)
	if err != nil {
		return result, failure("extension_abi_probe_failed", "The exact PHP CLI could not report its ABI as the software worker", 412)
	}
	fields := strings.Split(strings.TrimSpace(output), "|")
	if len(fields) != 4 || !regexp.MustCompile(`^8\.[0-9]+\.[0-9]+`).MatchString(fields[0]) {
		return result, failure("extension_abi_probe_failed", "The exact PHP CLI returned an invalid ABI identity", 412)
	}
	versionID, err := strconv.Atoi(fields[1])
	if err != nil || versionID < 80200 || versionID > 89999 || !filepath.IsAbs(fields[2]) || strings.ContainsAny(fields[2], "\x00\r\n") || (fields[3] != "0" && fields[3] != "1") {
		return result, failure("extension_abi_probe_failed", "The exact PHP CLI returned an invalid ABI identity", 412)
	}
	match := phpExtensionAPISuffix.FindStringSubmatch(filepath.Base(filepath.Clean(fields[2])))
	if len(match) != 2 {
		return result, failure("extension_abi_probe_failed", "The PHP extension directory does not identify a module API", 412)
	}
	result = phpExtensionABI{Version: fields[0], VersionID: versionID, ExtensionDir: filepath.Clean(fields[2]), ModuleAPI: match[1], ZTS: fields[3] == "1"}
	return result, nil
}

func hashExtensionDir(path string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return hex.EncodeToString(sum[:])
}

func (e *PrivilegedExecutor) resolvePHPModulePackage(ctx context.Context, platform Platform, minor, moduleID string) ([]PackagePin, error) {
	packageName, err := phpExtensionPackage(minor, moduleID)
	if err != nil {
		return nil, err
	}
	if e.packages == nil || !e.packages.enabled {
		return nil, failure("package_install_disabled", "Trusted system package installation is disabled in Agent policy", 412)
	}
	pins, err := e.packages.resolve(ctx, Source{Kind: "signed_package", Trust: "repository_signature", Packages: []string{packageName}})
	if err != nil {
		return nil, err
	}
	if len(pins) == 0 || pins[0].Name != packageName || pins[0].Format == "rpm" {
		return nil, failure("package_plan_invalid", "The trusted PHP module package did not resolve to an exact Debian package pin", 412)
	}
	for index, pin := range pins {
		if pin.Format == "rpm" || pin.Architecture != "all" && pin.Architecture != platform.Arch || !trustedPHPRepository(platform, pin.Repository) || !artifactHash.MatchString(pin.SHA256) || pin.Version == "" {
			return nil, failure("package_metadata_untrusted", "PHP module package pins must come from signed official distribution repositories for this Agent architecture", 412)
		}
		// A PHP extension package may depend on system libraries, but it must
		// never upgrade, replace or install a PHP runtime shared by other sites.
		if index > 0 && (strings.HasPrefix(pin.Name, "php") || strings.HasPrefix(pin.Name, "libapache2-mod-php")) {
			return nil, failure("extension_package_transaction_unsafe", "APT proposed changing another PHP runtime package; no module package was installed", 412)
		}
	}
	return pins, nil
}

// installedDebPackageVersion distinguishes a missing package from a failed or
// malformed dpkg query without invoking a shell or accepting caller arguments.
func installedDebPackageVersion(ctx context.Context, name string) (string, bool, error) {
	if !packageName.MatchString(name) {
		return "", false, failure("package_name_invalid", "Invalid Debian package identity", 412)
	}
	commandCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, "/usr/bin/dpkg-query", "-W", "-f=${db:Status-Abbrev}|${Version}|${Architecture}", name)
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive"}
	var stdout, stderr boundedPHPCommandOutput
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if commandCtx.Err() != nil {
		return "", false, commandCtx.Err()
	}
	if stdout.overflow || stderr.overflow {
		return "", false, failure("package_query_invalid", "dpkg returned oversized package identity output", 412)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && strings.Contains(strings.ToLower(stderr.String()), "no packages found matching") {
			return "", false, nil
		}
		return "", false, failure("package_query_failed", "dpkg could not verify the installed module package", 412)
	}
	fields := strings.Split(strings.TrimSpace(stdout.String()), "|")
	if len(fields) != 3 || strings.TrimSpace(fields[0]) != "ii" || !debVersion.MatchString(fields[1]) || !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`).MatchString(fields[2]) {
		return "", false, failure("package_query_invalid", "dpkg returned an unsupported installed package state", 412)
	}
	return fields[1], true, nil
}

type boundedPHPCommandOutput struct {
	data     []byte
	overflow bool
}

func (b *boundedPHPCommandOutput) Write(data []byte) (int, error) {
	const limit = 4096
	if len(b.data)+len(data) > limit {
		b.overflow = true
		remaining := limit - len(b.data)
		if remaining > 0 {
			b.data = append(b.data, data[:remaining]...)
		}
		return len(data), nil
	}
	b.data = append(b.data, data...)
	return len(data), nil
}

func (b *boundedPHPCommandOutput) String() string { return string(b.data) }

func requirePHPExecutablePackage(ctx context.Context, path, expectedPackage string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !packageName.MatchString(expectedPackage) {
		return failure("extension_runtime_unverified", "Managed PHP executable identity is invalid", 412)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return failure("extension_runtime_unverified", "Managed PHP executable must be a protected distro package file", 412)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 || info.Mode().Perm()&0111 == 0 {
		return failure("extension_runtime_unverified", "Managed PHP executable must be a protected distro package file", 412)
	}
	output, err := runFixed(ctx, "/usr/bin/dpkg-query", []string{"-S", path}, 20*time.Second, nil)
	if err != nil {
		return failure("extension_runtime_unverified", "dpkg could not prove PHP executable package ownership", 412)
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 1 {
		return failure("extension_runtime_unverified", "PHP executable has ambiguous package ownership", 412)
	}
	owner, _, found := strings.Cut(lines[0], ": ")
	if !found {
		return failure("extension_runtime_unverified", "dpkg returned an invalid PHP executable owner", 412)
	}
	owner = strings.TrimSuffix(owner, ":"+runtime.GOARCH)
	if owner != expectedPackage {
		return failure("extension_runtime_unverified", "PHP executable is not owned by the exact trusted runtime package", 412)
	}
	return nil
}

func (e *PrivilegedExecutor) verifyPHPModuleFile(ctx context.Context, abi phpExtensionABI, moduleID, packageName, packageVersion string) (string, error) {
	path := filepath.Join(abi.ExtensionDir, moduleID+".so")
	info, err := os.Lstat(path)
	if err != nil {
		return "", failure("extension_module_unverified", "The selected module file is absent or not a protected distro package file", 412)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 || info.Size() <= 0 || info.Size() > 128<<20 {
		return "", failure("extension_module_unverified", "The selected module file is absent or not a protected distro package file", 412)
	}
	installed, present, err := installedDebPackageVersion(ctx, packageName)
	if err != nil || !present || installed != packageVersion {
		return "", failure("extension_module_unverified", "The selected module package version changed before ABI validation", 412)
	}
	output, err := runFixed(ctx, "/usr/bin/dpkg-query", []string{"-S", path}, 20*time.Second, nil)
	if err != nil {
		return "", failure("extension_module_unverified", "dpkg could not prove selected module package ownership", 412)
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 1 {
		return "", failure("extension_module_unverified", "Selected module file has ambiguous package ownership", 412)
	}
	owner, _, found := strings.Cut(lines[0], ": ")
	if !found {
		return "", failure("extension_module_unverified", "dpkg returned an invalid module file owner", 412)
	}
	owner = strings.TrimSuffix(owner, ":"+runtime.GOARCH)
	if owner != packageName {
		return "", failure("extension_module_unverified", "Selected module file is owned by a different package", 412)
	}
	return path, nil
}

func verifyPHPModuleAsWorker(ctx context.Context, program, moduleID, path string, credential *syscall.Credential) error {
	code := `exit(extension_loaded("` + moduleID + `") ? 0 : 1);`
	_, err := runFixedEnvironment(ctx, program, []string{"-n", "-d", "extension=" + path, "-r", code}, 15*time.Second, credential, nil)
	if err != nil {
		return failure("extension_module_load_failed", "The exact distro module did not load in its matching CLI ABI as the software worker", 412)
	}
	return nil
}

func phpExtensionMinor(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) < 2 || parts[0] != "8" || parts[1] == "" {
		return ""
	}
	return parts[0] + "." + parts[1]
}

func samePackagePins(a, b []PackagePin) bool { return reflect.DeepEqual(a, b) }

func phpExtensionPackageAgentOwned(record phpExtensionPlanRecord) bool {
	return !record.TargetPackageWasInstalled || record.OwnedPackageAtPlan
}

func phpExtensionInstalledPackageReceipt(record phpExtensionPlanRecord, pin PackagePin, previous *phpExtensionPackageReceipt) (phpExtensionPackageReceipt, bool, error) {
	if !phpExtensionPackageAgentOwned(record) {
		return phpExtensionPackageReceipt{}, false, nil
	}
	if pin.Name != record.PackageName || pin.Format == "rpm" || !debVersion.MatchString(pin.Version) || !artifactHash.MatchString(pin.SHA256) {
		return phpExtensionPackageReceipt{}, false, failure("extension_state_invalid", "Verified PHP package pin is invalid", 412)
	}
	receipt := phpExtensionPackageReceipt{Version: 1, Package: record.PackageName, Pin: pin, InstalledByAgent: true, Installed: true, SourceInstanceID: record.Public.ID}
	if record.OwnedPackageAtPlan {
		if previous == nil || !previous.InstalledByAgent || !previous.Installed || previous.Package != record.PackageName || previous.Pin != pin {
			return phpExtensionPackageReceipt{}, false, failure("extension_state_invalid", "Shared Agent-owned PHP package receipt changed during installation", 412)
		}
		receipt.SourceInstanceID = previous.SourceInstanceID
	}
	return receipt, true, nil
}

func (e *PrivilegedExecutor) reinstallPHPModulePackage(ctx context.Context, pin PackagePin) (returned error) {
	if e.packages == nil || !e.packages.enabled || os.Geteuid() != 0 || pin.Format == "rpm" || !packageName.MatchString(pin.Name) || !debVersion.MatchString(pin.Version) || !artifactHash.MatchString(pin.SHA256) {
		return failure("package_install_disabled", "Trusted signed Debian package management is required to verify this PHP module", 412)
	}
	e.packages.mu.Lock()
	defer e.packages.mu.Unlock()
	if err := e.packages.prepare(ctx); err != nil {
		return err
	}
	resolved, err := e.packages.resolveExactAPT(ctx, pin.Name, pin.Version)
	if err != nil || resolved != pin {
		return failure("package_plan_stale", "The exact signed PHP module package pin changed; make a fresh plan", 412)
	}
	simulateArgs := append(e.packages.options(), "--simulate", "--no-remove", "--no-install-recommends", "--reinstall", "install", pin.Name+"="+pin.Version)
	output, err := runFixedEnvironment(ctx, "/usr/bin/apt-get", simulateArgs, 2*time.Minute, nil, []string{"LC_ALL=C"})
	if err != nil {
		return failure("package_plan_stale", "APT cannot safely verify the exact PHP module reinstall", 412)
	}
	changes, err := parseAPTChanges(output)
	if err != nil || len(changes) != 1 || changes[pin.Name] != pin.Version {
		return failure("extension_package_transaction_unsafe", "APT proposed changes beyond the selected PHP module package", 412)
	}
	cleanup, err := e.packages.startupPolicy()
	if err != nil {
		return err
	}
	defer func() {
		if err := cleanup(); err != nil && returned == nil {
			returned = failure("package_recovery_required", "PHP module package startup policy could not be restored; inspect Agent package state before retry", 500)
		}
	}()
	installArgs := append(e.packages.options(), "--yes", "--no-remove", "--no-install-recommends", "--reinstall", "install", pin.Name+"="+pin.Version)
	if _, err = runFixedEnvironment(ctx, "/usr/bin/apt-get", installArgs, 30*time.Minute, nil, []string{"LC_ALL=C"}); err != nil {
		return failure("package_install_failed", "APT could not reinstall the exact signed PHP module package", 502)
	}
	version, installed, err := installedDebPackageVersion(ctx, pin.Name)
	if err != nil || !installed || version != pin.Version {
		return failure("package_install_unverified", "Reinstalled PHP module version does not match the signed plan", 412)
	}
	return nil
}

func hashPHPIdempotencyKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (e *PrivilegedExecutor) loadPHPExtensionPlan(planID string) (phpExtensionPlanRecord, error) {
	var record phpExtensionPlanRecord
	if err := e.load("plans", planID, &record); err != nil {
		return record, err
	}
	created := record.Public.CreatedAt
	if record.Public.PlanID != planID || !validInstanceID(record.Public.ID) || (record.Public.Operation != "install" && record.Public.Operation != "uninstall") || !supportedPHPModuleID(record.Public.ModuleID) || record.Public.PackageRemovable || !packageName.MatchString(record.PackageName) || !artifactHash.MatchString(record.ExtensionDirSHA256) || record.Public.ExpectedRevision < 1 || !validPHPModulePlanWindow(created, record.Public.ExpiresAt) || len(record.Public.PackagePins) > 128 || len(record.Public.Resources) != 2 {
		return record, failure("extension_plan_invalid", "Protected PHP extension plan is invalid", 412)
	}
	if record.Public.Resources[0] != InstanceResource(record.Public.ID) || record.Public.Resources[1] != "host:packages" {
		return record, failure("extension_plan_invalid", "Protected PHP extension plan has invalid resource ownership", 412)
	}
	return record, nil
}

func (e *PrivilegedExecutor) validatePHPExtensionPlan(ctx context.Context, instance Instance, record phpExtensionPlanRecord, pending bool) (phpExtensionABI, error) {
	plan := record.Public
	if plan.ID != instance.ID || plan.ExpectedRevision != instance.Revision && !pending || !slices.Equal(plan.Resources, []string{InstanceResource(instance.ID), "host:packages"}) {
		return phpExtensionABI{}, failure("revision_conflict", "PHP extension plan no longer matches this instance revision", 412)
	}
	if !pending && time.Now().After(plan.ExpiresAt) {
		return phpExtensionABI{}, failure("plan_expired", "PHP extension plan expired; prepare a fresh plan", 412)
	}
	platform := CurrentPlatform()
	if !reflect.DeepEqual(plan.Platform, platform) {
		return phpExtensionABI{}, failure("extension_plan_stale", "The host platform changed after the PHP extension plan was prepared", 412)
	}
	minor, err := phpExtensionPackage(phpExtensionMinor(plan.PHPVersion), plan.ModuleID)
	if err != nil || minor != record.PackageName {
		return phpExtensionABI{}, failure("extension_plan_invalid", "Protected plan does not match the compiled-in PHP module package recipe", 412)
	}
	recipe, runtimePlatform, abi, err := e.phpExtensionRuntime(ctx, instance)
	if err != nil {
		return phpExtensionABI{}, err
	}
	if !reflect.DeepEqual(runtimePlatform, plan.Platform) || abi.Version != plan.PHPVersion || abi.ModuleAPI != plan.PHPModuleAPI || abi.ModuleAPI != recipe.API || hashExtensionDir(abi.ExtensionDir) != record.ExtensionDirSHA256 {
		return phpExtensionABI{}, failure("extension_plan_stale", "The exact PHP executable or extension ABI changed after planning", 412)
	}
	return abi, nil
}

func (e *PrivilegedExecutor) validatePHPModulePackageState(ctx context.Context, record phpExtensionPlanRecord, packageReceipt phpExtensionPackageReceipt, hasPackageReceipt bool, pending bool) (string, bool, error) {
	version, installed, err := installedDebPackageVersion(ctx, record.PackageName)
	if err != nil {
		return "", false, err
	}
	if !pending && (installed != record.TargetPackageWasInstalled || installed && version != record.TargetPackageVersion) {
		return version, installed, failure("extension_plan_stale", "The selected PHP module package state changed after planning", 412)
	}
	if record.OwnedPackageAtPlan && (!hasPackageReceipt || !packageReceipt.InstalledByAgent || packageReceipt.Package != record.PackageName || packageReceipt.Pin.Version != record.TargetPackageVersion || !packageReceipt.Installed) {
		return version, installed, failure("extension_plan_stale", "Agent package ownership changed after planning", 412)
	}
	if installed && hasPackageReceipt && packageReceipt.Installed && packageReceipt.Pin.Version != version {
		return version, installed, failure("extension_state_invalid", "Installed PHP module version disagrees with the protected Agent receipt", 412)
	}
	return version, installed, nil
}

func (e *PrivilegedExecutor) planPHPExtension(ctx context.Context, instance Instance, input PHPInput) (any, error) {
	if instance.Revision != input.ExpectedRevision {
		return nil, failure("revision_conflict", "PHP configuration changed", 412)
	}
	state, err := e.loadPHPExtensionState(instance.ID)
	if err != nil {
		return nil, err
	}
	if state.Pending != nil {
		return nil, failure("extension_recovery_required", "A previous PHP extension package transaction needs to resume before another plan can be created", 409)
	}
	recipe, platform, abi, err := e.phpExtensionRuntime(ctx, instance)
	if err != nil {
		return nil, err
	}
	packageName, err := phpExtensionPackage(recipe.Minor, input.ModuleID)
	if err != nil {
		return nil, err
	}
	packageReceipt, hasPackageReceipt, err := e.loadPHPExtensionPackage(packageName)
	if err != nil {
		return nil, err
	}
	installedVersion, packageInstalled, err := installedDebPackageVersion(ctx, packageName)
	if err != nil {
		return nil, err
	}
	if hasPackageReceipt && packageReceipt.InstalledByAgent && packageReceipt.Installed && (!packageInstalled || packageReceipt.Pin.Version != installedVersion) {
		return nil, failure("extension_state_invalid", "The protected Agent package receipt disagrees with dpkg; resolve package ownership before planning", 412)
	}
	createdAt, expiresAt := phpModulePlanWindow(time.Now())
	plan := PHPModulePlan{
		ID: instance.ID, Operation: input.Operation, ModuleID: input.ModuleID,
		PHPVersion: abi.Version, PHPModuleAPI: abi.ModuleAPI, Platform: platform,
		ExpectedRevision: instance.Revision, Resources: []string{InstanceResource(instance.ID), "host:packages"},
		CreatedAt: createdAt, ExpiresAt: expiresAt,
		AlreadyInstalled: packageInstalled, AlreadyEnabled: instance.Extensions[input.ModuleID],
		AffectedProfiles: []phpExtensionAffectedProfile{},
	}
	record := phpExtensionPlanRecord{Public: plan, PackageName: packageName, ExtensionDirSHA256: hashExtensionDir(abi.ExtensionDir), TargetPackageWasInstalled: packageInstalled, TargetPackageVersion: installedVersion}
	if hasPackageReceipt && packageReceipt.Installed && packageReceipt.InstalledByAgent {
		if packageReceipt.Pin.Name != packageName {
			return nil, failure("extension_state_invalid", "Agent-owned module package receipt does not match this trusted package", 412)
		}
		record.OwnedPackageAtPlan = true
	}
	if input.Operation == "install" {
		pins, resolveErr := e.resolvePHPModulePackage(ctx, platform, recipe.Minor, input.ModuleID)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if packageInstalled && installedVersion != pins[0].Version {
			return nil, failure("extension_package_version_conflict", "A different version of this PHP module package is already installed; no shared PHP runtime package will be changed", 412)
		}
		if hasPackageReceipt && packageReceipt.Installed && packageReceipt.InstalledByAgent && packageReceipt.Pin.Version != pins[0].Version {
			return nil, failure("extension_package_owner_conflict", "The Agent-owned module package receipt pins a different version", 412)
		}
		plan.PackagePins = pins
		plan.AlreadyInstalled = packageInstalled
		for _, pin := range pins {
			description := "Install this exact package from signed official distribution metadata; its dependencies are kept on uninstall"
			if packageInstalled {
				description = "Reinstall this exact package from signed official distribution metadata to prove its module files; dependencies remain installed"
			}
			plan.Changes = append(plan.Changes, Change{Kind: "signed_package", Target: pin.Name + "=" + pin.Version, Description: description})
		}
		if !plan.AlreadyEnabled {
			plan.Changes = append(plan.Changes, Change{Kind: "php_extension", Target: input.ModuleID, Description: "Load this module in the exact managed FPM ABI, validate PHP configuration and reload only this instance if it is running"})
		}
	} else {
		if packageInstalled && record.OwnedPackageAtPlan && packageReceipt.Pin.Version != installedVersion {
			record.OwnedPackageAtPlan = false
			plan.PackageRetainedReason = "installed package version differs from the protected Agent receipt"
		}
		if !packageInstalled {
			plan.PackageRetainedReason = "module package is not currently installed"
		} else if !record.OwnedPackageAtPlan {
			if plan.PackageRetainedReason == "" {
				plan.PackageRetainedReason = "module package was not installed by the Agent"
			}
		} else {
			plan.PackageRetainedReason = "physical package removal is deferred until PHP and Site profile changes can be atomically fenced; this action disables the module in the selected instance and retains its package"
		}
		plan.Changes = append(plan.Changes, Change{Kind: "php_extension", Target: input.ModuleID, Description: "Disable this module in the exact managed PHP instance and reload it only if running"})
		plan.Changes = append(plan.Changes, Change{Kind: "retained_php_module_package", Target: packageName, Description: "Retain the module package: " + plan.PackageRetainedReason})
	}
	plan.PlanID, err = randomPlanID()
	if err != nil {
		return nil, err
	}
	record.Public = plan
	if err = e.save("plans", plan.PlanID, record); err != nil {
		return nil, err
	}
	return plan, nil
}

func phpExtensionPackagePin(pins []PackagePin, name string) (PackagePin, bool) {
	for _, pin := range pins {
		if pin.Name == name {
			return pin, true
		}
	}
	return PackagePin{}, false
}

func setPHPModuleReference(state *phpExtensionState, packageName, moduleID string, enabled bool) {
	if state.References == nil {
		state.References = map[string][]string{}
	}
	modules := slices.Clone(state.References[packageName])
	modules = slices.DeleteFunc(modules, func(existing string) bool { return existing == moduleID })
	if enabled {
		modules = append(modules, moduleID)
		slices.Sort(modules)
		modules = slices.Compact(modules)
	}
	if len(modules) == 0 {
		delete(state.References, packageName)
	} else {
		state.References[packageName] = modules
	}
}

func phpExtensionCompletedResult(state phpExtensionState, planID, keyHash string) (map[string]any, bool, error) {
	for _, completed := range state.Completed {
		if completed.IdempotencyKeySHA256 != keyHash {
			continue
		}
		if completed.PlanID != planID {
			return nil, false, failure("idempotency_conflict", "This PHP extension idempotency key was already used for another plan", 409)
		}
		return maps.Clone(completed.Result), true, nil
	}
	return nil, false, nil
}

func (e *PrivilegedExecutor) recordPHPModuleCompletion(state *phpExtensionState, planID, keyHash string, result map[string]any) error {
	state.Completed = append(state.Completed, phpExtensionApplyResult{PlanID: planID, IdempotencyKeySHA256: keyHash, Result: maps.Clone(result), CompletedAt: time.Now().UTC()})
	if len(state.Completed) > 16 {
		state.Completed = slices.Clone(state.Completed[len(state.Completed)-16:])
	}
	return e.savePHPExtensionState(*state)
}

func phpExtensionOutput(instance Instance, record phpExtensionPlanRecord, retainedReason string) map[string]any {
	result := map[string]any{
		"id": instance.ID, "module_id": record.Public.ModuleID,
		"operation":         record.Public.Operation,
		"enabled":           instance.Extensions[record.Public.ModuleID],
		"package_removable": false,
		"package_removed":   false,
		"revision":          instance.Revision,
	}
	if retainedReason != "" {
		result["package_retained_reason"] = retainedReason
	}
	return result
}

func (e *PrivilegedExecutor) savePHPModulePackageOutcome(ctx context.Context, record phpExtensionPlanRecord, state phpExtensionState) error {
	packageName := record.PackageName
	version, installed, err := installedDebPackageVersion(ctx, packageName)
	if err != nil {
		return err
	}
	switch record.Public.Operation {
	case "install":
		pin, ok := phpExtensionPackagePin(record.Public.PackagePins, packageName)
		if !ok || !installed || version != pin.Version {
			return failure("extension_recovery_required", "The exact PHP module package is not installed at its reviewed version", 409)
		}
		previous, found, loadErr := e.loadPHPExtensionPackage(packageName)
		if loadErr != nil {
			return loadErr
		}
		var previousPtr *phpExtensionPackageReceipt
		if found {
			previousPtr = &previous
		}
		receipt, saveReceipt, receiptErr := phpExtensionInstalledPackageReceipt(record, pin, previousPtr)
		if receiptErr != nil {
			return receiptErr
		}
		if saveReceipt {
			if err = e.savePHPExtensionPackage(receipt); err != nil {
				return err
			}
		}
	case "uninstall":
		receipt, found, loadErr := e.loadPHPExtensionPackage(packageName)
		if loadErr != nil {
			return loadErr
		}
		if found && receipt.InstalledByAgent {
			if installed && receipt.Pin.Version != version {
				return failure("extension_recovery_required", "The PHP module package changed during uninstall recovery", 409)
			}
			receipt.Installed = installed
			if err = e.savePHPExtensionPackage(receipt); err != nil {
				return err
			}
		}
	default:
		return failure("extension_plan_invalid", "Unknown PHP extension operation", 412)
	}
	setPHPModuleReference(&state, packageName, record.Public.ModuleID, record.Public.Operation == "install")
	if err = e.savePHPExtensionState(state); err != nil {
		return err
	}
	return nil
}

func (e *PrivilegedExecutor) finishPHPModuleIntent(ctx context.Context, state *phpExtensionState, intent phpExtensionIntent, keyHash string) (map[string]any, error) {
	if err := e.savePHPModulePackageOutcome(ctx, intent.Plan, *state); err != nil {
		return nil, err
	}
	retained := intent.PackageRetainedReason
	if intent.Plan.Public.Operation == "uninstall" && retained == "" {
		retained = intent.Plan.Public.PackageRetainedReason
	}
	if intent.Plan.Public.Operation == "uninstall" && retained == "" {
		retained = "physical package removal is deferred; this action disables the module in the selected instance and retains its package"
	}
	result := phpExtensionOutput(intent.Desired, intent.Plan, retained)
	state.Pending = nil
	if err := e.recordPHPModuleCompletion(state, intent.PlanID, keyHash, result); err != nil {
		return nil, err
	}
	return result, nil
}

func (e *PrivilegedExecutor) phpModulePackageReceipt(ctx context.Context, record phpExtensionPlanRecord, pending bool) (phpExtensionPackageReceipt, bool, error) {
	receipt, found, err := e.loadPHPExtensionPackage(record.PackageName)
	if err != nil || !found {
		return receipt, found, err
	}
	if !receipt.InstalledByAgent {
		if record.OwnedPackageAtPlan {
			return receipt, found, failure("extension_state_invalid", "Protected PHP module package ownership changed", 412)
		}
		return receipt, found, nil
	}
	if receipt.Pin.Name != record.PackageName || receipt.Pin.Format == "rpm" || !trustedPHPRepository(record.Public.Platform, receipt.Pin.Repository) || !artifactHash.MatchString(receipt.Pin.SHA256) || receipt.Pin.Architecture != record.Public.Platform.Arch || record.OwnedPackageAtPlan && receipt.Pin.Version != record.TargetPackageVersion {
		return receipt, found, failure("extension_state_invalid", "Protected PHP package ownership receipt is not a signed official distro pin", 412)
	}
	version, installed, queryErr := installedDebPackageVersion(ctx, record.PackageName)
	if queryErr != nil {
		return receipt, found, queryErr
	}
	if installed && version != receipt.Pin.Version || !installed && receipt.Installed || installed && !receipt.Installed && pending {
		return receipt, found, failure("extension_state_invalid", "Agent-owned PHP module package is absent or changed from its protected version", 412)
	}
	return receipt, found, nil
}

func (e *PrivilegedExecutor) extensionServiceRunning(instance Instance) (bool, error) {
	_, err := ownedProcessID(instance)
	if err == nil {
		return true, nil
	}
	if core.ErrorCode(err) == "runtime_not_running" {
		return false, nil
	}
	return false, err
}

func (e *PrivilegedExecutor) configurePHPModuleState(ctx context.Context, previous, desired Instance, running bool) error {
	manifest, err := e.catalog.Get(previous.PluginID)
	if err != nil {
		return err
	}
	if err = e.configure(ctx, &desired, manifest); err != nil {
		return err
	}
	if err = e.checkPHPExtensions(ctx, desired); err != nil {
		return err
	}
	if _, err = e.controlLocked(ctx, ServiceInput{ID: desired.ID, Command: "check"}, desired); err != nil {
		return err
	}
	if running {
		if _, err = e.controlLocked(ctx, ServiceInput{ID: desired.ID, Command: "reload"}, desired); err != nil {
			return err
		}
	}
	return nil
}

func (e *PrivilegedExecutor) rollbackPHPModuleState(ctx context.Context, previous Instance, running bool) error {
	manifest, err := e.catalog.Get(previous.PluginID)
	if err != nil {
		return err
	}
	restored := previous
	if err = e.configure(ctx, &restored, manifest); err != nil {
		return err
	}
	if err = e.checkPHPExtensions(ctx, restored); err != nil {
		return err
	}
	if _, err = e.controlLocked(ctx, ServiceInput{ID: restored.ID, Command: "check"}, restored); err != nil {
		return err
	}
	if running {
		if _, err = e.controlLocked(ctx, ServiceInput{ID: restored.ID, Command: "reload"}, restored); err != nil {
			return err
		}
	}
	return nil
}

func (e *PrivilegedExecutor) persistPHPModuleConfigFailure(state *phpExtensionState, intent *phpExtensionIntent, rollbackErr error) error {
	if state == nil || intent == nil || intent.PlanID == "" {
		return failure("extension_state_invalid", "PHP extension configuration recovery state is invalid", 412)
	}
	if rollbackErr != nil {
		intent.Phase = "rollback_required"
		intent.UpdatedAt = time.Now().UTC()
		state.Pending = intent
		if err := e.savePHPExtensionState(*state); err != nil {
			return failure("extension_recovery_required", "PHP configuration rollback failed and its recovery intent could not be committed", 500)
		}
		return failure("extension_recovery_required", "PHP extension configuration failed and the prior runtime configuration could not be restored", 500)
	}
	state.Pending = nil
	if err := e.savePHPExtensionState(*state); err != nil {
		return failure("extension_recovery_required", "The prior PHP configuration was restored but transaction state needs reconciliation", 500)
	}
	return nil
}

func (e *PrivilegedExecutor) applyPHPExtension(ctx context.Context, instance Instance, input PHPInput) (any, error) {
	phpExtensionMutationMu.Lock()
	defer phpExtensionMutationMu.Unlock()
	state, err := e.loadPHPExtensionState(instance.ID)
	if err != nil {
		return nil, err
	}
	keyHash := hashPHPIdempotencyKey(input.IdempotencyKey)
	if result, found, completedErr := phpExtensionCompletedResult(state, input.PlanID, keyHash); completedErr != nil || found {
		return result, completedErr
	}
	var record phpExtensionPlanRecord
	pending := state.Pending != nil
	if pending {
		if state.Pending.PlanID != input.PlanID {
			return nil, failure("extension_recovery_required", "A different PHP extension package transaction is pending; resume that exact plan first", 409)
		}
		record = state.Pending.Plan
	} else {
		record, err = e.loadPHPExtensionPlan(input.PlanID)
		if err != nil {
			return nil, err
		}
	}
	if record.Public.ID != instance.ID || record.Public.ExpectedRevision != input.ExpectedRevision {
		return nil, failure("revision_conflict", "PHP extension plan does not match the requested instance revision", 412)
	}
	if pending {
		intent := *state.Pending
		if !reflect.DeepEqual(record, intent.Plan) || input.ExpectedRevision != intent.Previous.Revision {
			return nil, failure("extension_recovery_required", "Pending PHP extension state does not match this reviewed plan", 409)
		}
		marker, markerOK := ctx.Value(phpExtensionRecoveryMarkerKey{}).(phpExtensionRecoveryMarker)
		intentDigest, digestErr := phpExtensionIntentDigest(intent)
		if digestErr != nil {
			return nil, digestErr
		}
		if !markerOK || marker.Executor != e || marker.InstanceID != instance.ID || marker.PlanID != input.PlanID || marker.ExpectedRevision != input.ExpectedRevision || marker.IdempotencyKeySHA256 != hashPHPIdempotencyKey(input.IdempotencyKey) || marker.IntentSHA256 != intentDigest {
			return nil, failure("extension_recovery_required", "The exact pending PHP extension intent changed before recovery", 409)
		}
		if reflect.DeepEqual(instance, intent.Desired) {
			return e.finishPHPModuleIntent(ctx, &state, intent, keyHash)
		}
		if !reflect.DeepEqual(instance, intent.Previous) {
			return nil, failure("extension_recovery_required", "PHP receipt changed during an interrupted module transaction", 409)
		}
	} else if instance.Revision != record.Public.ExpectedRevision {
		return nil, failure("revision_conflict", "PHP configuration changed after planning", 412)
	}
	abi, err := e.validatePHPExtensionPlan(ctx, instance, record, pending)
	if err != nil {
		return nil, err
	}
	packageReceipt, hasPackageReceipt, err := e.phpModulePackageReceipt(ctx, record, pending)
	if err != nil {
		return nil, err
	}
	installedVersion, packageInstalled, err := e.validatePHPModulePackageState(ctx, record, packageReceipt, hasPackageReceipt, pending)
	if err != nil {
		return nil, err
	}
	var modulePin PackagePin
	if record.Public.Operation == "install" {
		pins, resolveErr := e.resolvePHPModulePackage(ctx, record.Public.Platform, phpExtensionMinor(record.Public.PHPVersion), record.Public.ModuleID)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if !samePackagePins(record.Public.PackagePins, pins) {
			return nil, failure("extension_plan_stale", "Signed PHP module package metadata changed after planning", 412)
		}
		var ok bool
		modulePin, ok = phpExtensionPackagePin(pins, record.PackageName)
		if !ok || packageInstalled && installedVersion != modulePin.Version || !packageInstalled && record.TargetPackageWasInstalled && !pending {
			return nil, failure("extension_plan_stale", "The exact module package version no longer matches the reviewed plan", 412)
		}
	}
	if record.Public.Operation == "uninstall" && packageInstalled && installedVersion != record.TargetPackageVersion && !pending {
		return nil, failure("extension_plan_stale", "Installed PHP module package version changed after planning", 412)
	}
	previous := instance
	var intent phpExtensionIntent
	if pending {
		intent = *state.Pending
	} else {
		desired := instance
		desired.Extensions = maps.Clone(instance.Extensions)
		if desired.Extensions == nil {
			desired.Extensions = map[string]bool{}
		}
		desired.Extensions[record.Public.ModuleID] = record.Public.Operation == "install"
		if desired.Extensions[record.Public.ModuleID] != instance.Extensions[record.Public.ModuleID] {
			desired.Revision++
		}
		if desired.Extensions == nil {
			desired.Extensions = map[string]bool{}
		}
		intent = phpExtensionIntent{PlanID: input.PlanID, Phase: "prepared", Plan: record, Previous: previous, Desired: desired, StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
		state.Pending = &intent
		if err = e.savePHPExtensionState(state); err != nil {
			return nil, err
		}
	}
	previous = intent.Previous
	desired := intent.Desired
	running, err := e.extensionServiceRunning(previous)
	if err != nil {
		return nil, err
	}
	if record.Public.Operation == "install" {
		intent.Phase = "package_install_started"
		intent.UpdatedAt = time.Now().UTC()
		state.Pending = &intent
		if err = e.savePHPExtensionState(state); err != nil {
			return nil, err
		}
		if packageInstalled {
			err = e.reinstallPHPModulePackage(ctx, modulePin)
		} else {
			err = e.packages.install(ctx, record.Public.PackagePins)
		}
		if err != nil {
			return nil, err
		}
		intent.Phase = "package_installed"
		intent.UpdatedAt = time.Now().UTC()
		state.Pending = &intent
		if err = e.savePHPExtensionState(state); err != nil {
			return nil, err
		}
		path, pathErr := e.verifyPHPModuleFile(ctx, abi, record.Public.ModuleID, record.PackageName, modulePin.Version)
		if pathErr != nil {
			return nil, pathErr
		}
		program, programErr := phpCLI(previous)
		if programErr != nil {
			return nil, programErr
		}
		if err = verifyPHPModuleAsWorker(ctx, program, record.Public.ModuleID, path, e.phpCredential()); err != nil {
			return nil, err
		}
		existing, found, loadErr := e.loadPHPExtensionPackage(record.PackageName)
		if loadErr != nil {
			return nil, loadErr
		}
		var existingPtr *phpExtensionPackageReceipt
		if found {
			existingPtr = &existing
		}
		packageState, saveReceipt, receiptErr := phpExtensionInstalledPackageReceipt(record, modulePin, existingPtr)
		if receiptErr != nil {
			return nil, receiptErr
		}
		if saveReceipt {
			if err = e.savePHPExtensionPackage(packageState); err != nil {
				return nil, err
			}
		}
	}
	configChanged := desired.Extensions[record.Public.ModuleID] != previous.Extensions[record.Public.ModuleID]
	if configChanged {
		if err = e.configurePHPModuleState(ctx, previous, desired, running); err != nil {
			rollbackErr := e.rollbackPHPModuleState(context.WithoutCancel(ctx), previous, running)
			if recoveryErr := e.persistPHPModuleConfigFailure(&state, &intent, rollbackErr); recoveryErr != nil {
				return nil, recoveryErr
			}
			return nil, err
		}
	}
	intent.Phase = "config_applied"
	intent.UpdatedAt = time.Now().UTC()
	state.Pending = &intent
	if err = e.savePHPExtensionState(state); err != nil {
		return nil, err
	}
	retainedReason := record.Public.PackageRetainedReason
	if running {
		desired.Health = e.probe(ctx, desired)
	}
	intent.Desired = desired
	intent.PackageRetainedReason = retainedReason
	intent.UpdatedAt = time.Now().UTC()
	intent.Phase = "receipt_commit_started"
	state.Pending = &intent
	if err = e.savePHPExtensionState(state); err != nil {
		return nil, err
	}
	if err = e.save("receipts", desired.ID, desired); err != nil {
		return nil, err
	}
	return e.finishPHPModuleIntent(ctx, &state, intent, keyHash)
}

func (m *Manager) phpExtensionResources(_ context.Context, actor core.Actor, raw json.RawMessage) ([]string, error) {
	var input PHPInput
	if err := parse(raw, &input); err != nil {
		return nil, err
	}
	if !validInstanceID(input.ID) {
		return nil, failure("invalid_input", "A valid PHP instance ID is required", 400)
	}
	resource := InstanceResource(input.ID)
	if !m.deps.Authorize(actor, "php:manage", resource) {
		return nil, failure("forbidden", "PHP instance is outside the current management grant", 403)
	}
	if err := m.requirePackageGrant(actor); err != nil {
		return nil, err
	}
	return []string{resource, "host:packages"}, nil
}

func (m *Manager) phpExtensionBindings() []core.Binding {
	planSchema := obj(map[string]any{
		"id": idSchema(), "operation": enumSchema("install", "uninstall"),
		"module_id": enumSchema(phpExtensionModuleIDs()...), "expected_revision": map[string]any{"type": "integer", "minimum": 1},
	}, "id", "operation", "module_id", "expected_revision")
	applySchema := obj(map[string]any{
		"id": idSchema(), "plan_id": map[string]any{"type": "string", "pattern": `^plan_[a-f0-9]{48}$`}, "expected_revision": map[string]any{"type": "integer", "minimum": 1},
		"idempotency_key": map[string]any{"type": "string", "minLength": 8, "maxLength": 128, "pattern": phpExtensionIdempotencyKey.String()},
	}, "id", "plan_id", "expected_revision", "idempotency_key")
	planBinding := m.binding("php_extension_plan", "Review a signed distro PHP module package and selected-instance change", "php:manage", true, planSchema, m.phpExtensionResources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		var input PHPInput
		if err := parse(raw, &input); err != nil {
			return core.ActionResult{}, err
		}
		input.Command = "extension_plan"
		forward, _ := json.Marshal(input)
		var plan PHPModulePlan
		if err := m.agentJSON(ctx, "php_action", forward, &plan); err != nil {
			return core.ActionResult{}, err
		}
		visible := make([]phpExtensionAffectedProfile, 0, len(plan.AffectedProfiles))
		for _, profile := range plan.AffectedProfiles {
			resource := InstanceResource(profile.ID)
			if m.deps.Authorize(actor, "php:read", resource) || m.deps.Authorize(actor, "php:manage", resource) {
				visible = append(visible, profile)
			}
		}
		plan.AffectedProfiles = visible
		return core.ActionResult{Data: plan, Resources: []string{InstanceResource(input.ID), "host:packages"}}, nil
	})
	applyBinding := m.binding("php_extension_apply", "Queue a durable trusted PHP extension package transaction", "php:manage", false, applySchema, m.phpExtensionResources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		var input PHPInput
		if err := parse(raw, &input); err != nil {
			return core.ActionResult{}, err
		}
		input.Command = "extension_apply"
		forward, _ := json.Marshal(input)
		resources := []string{InstanceResource(input.ID), "host:packages"}
		job, err := m.deps.Tasks.Submit(ctx, actor, phpExtensionApplyJobOperation, forward, resources)
		if err != nil {
			return core.ActionResult{}, err
		}
		jobID, _ := job["id"].(string)
		return core.ActionResult{Data: job, TaskID: jobID, Resources: resources}, nil
	})
	recoverySchema := obj(map[string]any{"id": idSchema()}, "id")
	recoveryBinding := m.binding("php_extension_recovery_inspect", "Inspect a pending PHP extension transaction so its exact plan can be explicitly resumed", "php:manage", true, recoverySchema, idResource, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		var input struct {
			ID string `json:"id"`
		}
		if err := parse(raw, &input); err != nil {
			return core.ActionResult{}, err
		}
		forward, _ := json.Marshal(PHPInput{ID: input.ID, Command: "extension_recovery_inspect"})
		var inspection PHPExtensionRecoveryInspection
		if err := m.agentJSON(ctx, "php_action", forward, &inspection); err != nil {
			return core.ActionResult{}, err
		}
		response := struct {
			PHPExtensionRecoveryInspection
			CanApplyPackageGrant bool `json:"can_apply_package_grant"`
		}{
			PHPExtensionRecoveryInspection: inspection,
			CanApplyPackageGrant:           m.deps.Authorize(actor, "host:packages", "host:packages"),
		}
		return core.ActionResult{Data: response, Resources: []string{InstanceResource(input.ID)}}, nil
	})
	applyBinding.Action.Task = true
	return []core.Binding{planBinding, applyBinding, recoveryBinding}
}

func PHPExtensionJobOperations() []string { return []string{phpExtensionApplyJobOperation} }

// PHPExtensionExecutor is the API-host runner for durable extension tasks. It
// refreshes grants at execution time because package access is an independent
// authorization resource from the managed PHP instance.
type PHPExtensionExecutor struct {
	manager *Manager
	client  *agent.UnixClient
}

func NewPHPExtensionExecutor(manager *Manager, client *agent.UnixClient) *PHPExtensionExecutor {
	return &PHPExtensionExecutor{manager: manager, client: client}
}

func (e *PHPExtensionExecutor) Validate(request agent.Request) error {
	if e == nil || e.manager == nil || e.client == nil || request.Operation != phpExtensionApplyJobOperation {
		return agent.ErrUnauthorized
	}
	var input PHPInput
	if err := parse(request.Input, &input); err != nil {
		return err
	}
	if input.Command != "extension_apply" || input.Operation != "" || input.ModuleID != "" || input.Values != nil || input.Extension != "" || input.Enabled {
		return failure("invalid_input", "Only a typed PHP extension apply request may enter this task queue", 400)
	}
	if e.manager.deps.ResolveActor == nil || e.manager.deps.Authorize == nil {
		return failure("agent_unavailable", "PHP extension tasks require current identity authorization and the protected Unix Agent", 503)
	}
	return validatePHPInput(request.Input)
}

func (e *PHPExtensionExecutor) Run(ctx context.Context, request agent.Request) (result agent.Result, returned error) {
	result = agent.Result{Operation: request.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	defer func() { result.FinishedAt = time.Now().UTC() }()
	if err := e.Validate(request); err != nil {
		return result, err
	}
	var input PHPInput
	if err := parse(request.Input, &input); err != nil {
		return result, err
	}
	actor, err := e.manager.deps.ResolveActor(ctx, core.Actor{ID: request.ActorID})
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, failure("identity_unavailable", "The PHP extension task identity is no longer available", 403)
	}
	if actor.ID != request.ActorID {
		return result, failure("identity_unavailable", "The PHP extension task identity changed before execution", 403)
	}
	resource := InstanceResource(input.ID)
	if !e.manager.deps.Authorize(actor, "php:manage", resource) {
		return result, failure("forbidden", "The current identity no longer manages this PHP instance", 403)
	}
	if err = e.manager.requirePackageGrant(actor); err != nil {
		return result, err
	}
	forward, _ := json.Marshal(input)
	reply, err := e.client.Run(ctx, agent.Request{ActorID: request.ActorID, JobID: request.JobID, Operation: "php_action", Input: forward})
	if err != nil || reply.ExitCode != 0 {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, failure("php_extension_failed", "Protected PHP extension package transaction failed; inspect the instance and resume its exact plan if required", 502)
	}
	if len(reply.Stdout) > 1<<20 || !json.Valid([]byte(reply.Stdout)) {
		return result, failure("agent_protocol_error", "PHP extension Agent returned an invalid result", 502)
	}
	result.Stdout, result.ExitCode = reply.Stdout, 0
	return result, nil
}
