package plugins

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

// packageBackend uses signature-verified APT indexes and exact package pins.
// No insecure repo flags, third-party installer scripts, or global PATH changes
// are used. Package installation is enabled only in trusted Agent policy.
type packageBackend struct {
	enabled bool
	dir     string
	mu      sync.Mutex
	updated time.Time
}

func newPackageBackend(enabled bool, dir string) *packageBackend {
	return &packageBackend{enabled: enabled, dir: filepath.Join(dir, "apt")}
}

var debVersion = regexp.MustCompile(`^[0-9A-Za-z.+:~_-]{1,128}$`)

func (p *packageBackend) options() []string {
	return []string{"-o", "Dir::Cache=" + filepath.Join(p.dir, "cache"), "-o", "Dir::State::lists=" + filepath.Join(p.dir, "lists"), "-o", "APT::Get::AllowUnauthenticated=false", "-o", "Acquire::AllowInsecureRepositories=false", "-o", "Acquire::AllowDowngradeToInsecureRepositories=false", "-o", "APT::Sandbox::User=root"}
}
func (p *packageBackend) prepare(ctx context.Context) error {
	if !p.enabled || os.Geteuid() != 0 {
		return failure("package_install_disabled", "Trusted Agent policy must explicitly enable system package installation", 412)
	}
	if runtime.GOOS != "linux" {
		return failure("unsupported_platform", "Signed package backend requires Linux", 412)
	}
	if _, err := os.Stat("/usr/bin/apt-get"); err != nil {
		return failure("unsupported_platform", "This backend requires a signature-verifying Debian APT installation", 412)
	}
	if _, err := os.Lstat(filepath.Join(p.dir, "startup-policy.json")); err == nil {
		return failure("package_recovery_required", "Previous package startup policy transaction needs inspection before another package operation", 409)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := rejectInsecureSources(); err != nil {
		return err
	}
	for _, dir := range []string{p.dir, filepath.Join(p.dir, "cache", "archives", "partial"), filepath.Join(p.dir, "lists", "partial")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	if time.Since(p.updated) < 5*time.Minute {
		return nil
	}
	args := append(p.options(), "update", "--error-on=any")
	if _, err := runFixed(ctx, "/usr/bin/apt-get", args, 15*time.Minute, nil); err != nil {
		return failure("package_signature_or_network_failure", "APT could not refresh every repository with signature verification; no package installation was attempted", 412)
	}
	p.updated = time.Now()
	return nil
}

func rejectInsecureSources() error {
	paths := []string{"/etc/apt/sources.list"}
	entries, _ := os.ReadDir("/etc/apt/sources.list.d")
	for _, entry := range entries {
		if !entry.IsDir() && (strings.HasSuffix(entry.Name(), ".list") || strings.HasSuffix(entry.Name(), ".sources")) {
			paths = append(paths, filepath.Join("/etc/apt/sources.list.d", entry.Name()))
		}
	}
	for _, name := range paths {
		data, err := os.ReadFile(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		for scanner.Scan() {
			line := strings.ToLower(strings.TrimSpace(scanner.Text()))
			if strings.HasPrefix(line, "#") {
				continue
			}
			if strings.Contains(line, "trusted=yes") || strings.Contains(line, "trusted: yes") || strings.Contains(line, "allow-insecure=yes") || strings.Contains(line, "allow-weak=yes") {
				return failure("repository_untrusted", "An active APT source bypasses signature verification; restore trusted configuration before installing software", 412)
			}
		}
	}
	return nil
}

func (p *packageBackend) resolve(ctx context.Context, source Source) ([]PackagePin, error) {
	if _, err := os.Stat("/usr/bin/apt-get"); os.IsNotExist(err) {
		return p.resolveRPM(ctx, source)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.prepare(ctx); err != nil {
		return nil, err
	}
	pins := []PackagePin{}
	for _, name := range source.Packages {
		if !packageName.MatchString(name) {
			return nil, failure("invalid_source", "Trusted catalog contains an invalid package name", 500)
		}
		args := append(p.options(), "policy", name)
		output, err := runFixed(ctx, "/usr/bin/apt-cache", args, time.Minute, nil)
		if err != nil {
			return nil, failure("package_unavailable", "Requested software is absent from trusted signed package sources", 412)
		}
		candidate := ""
		for _, line := range strings.Split(output, "\n") {
			if before, after, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && before == "Candidate" {
				candidate = strings.TrimSpace(after)
				break
			}
		}
		if !debVersion.MatchString(candidate) {
			return nil, failure("package_unavailable", "No exact package candidate is available; configure its official signed repository", 412)
		}
		pin, err := p.resolveExactAPT(ctx, name, candidate)
		if err != nil {
			return nil, err
		}
		pins = append(pins, pin)
	}
	changes, err := p.simulateAPT(ctx, pins)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for name := range changes {
		if !slices.Contains(source.Packages, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if len(pins)+len(names) > 128 {
		return nil, failure("package_plan_too_large", "APT plan exceeds 128 exact packages", 413)
	}
	for _, name := range names {
		pin, err := p.resolveExactAPT(ctx, name, changes[name])
		if err != nil {
			return nil, err
		}
		pins = append(pins, pin)
	}
	return pins, nil
}

func (p *packageBackend) resolveExactAPT(ctx context.Context, name, version string) (PackagePin, error) {
	if !packageName.MatchString(name) || !debVersion.MatchString(version) {
		return PackagePin{}, failure("package_metadata_untrusted", "Invalid exact APT dependency identity", 412)
	}
	metadata, err := runFixed(ctx, "/usr/bin/apt-cache", append(p.options(), "show", name+"="+version), time.Minute, nil)
	if err != nil {
		return PackagePin{}, err
	}
	fields := debFields(metadata)
	if fields["Package"] != name || fields["Version"] != version || !artifactHash.MatchString(fields["SHA256"]) {
		return PackagePin{}, failure("package_metadata_untrusted", "Signed package metadata lacks a matching version and SHA-256", 412)
	}
	arch := fields["Architecture"]
	if arch != "all" && arch != runtime.GOARCH {
		return PackagePin{}, failure("unsupported_platform", "Package architecture does not match this Agent", 412)
	}
	uris, err := runFixed(ctx, "/usr/bin/apt-get", append(p.options(), "--print-uris", "download", name+"="+version), time.Minute, nil)
	if err != nil {
		return PackagePin{}, err
	}
	origin := ""
	for _, line := range strings.Split(uris, "\n") {
		if strings.HasPrefix(line, "'") {
			raw, _, _ := strings.Cut(strings.TrimPrefix(line, "'"), "'")
			if candidate := aptURIOrigin(raw); candidate != "" {
				origin = candidate
				break
			}
		}
	}
	if origin == "" {
		return PackagePin{}, failure("package_metadata_untrusted", "APT did not identify the signed package origin", 412)
	}
	return PackagePin{Name: name, Version: version, Architecture: arch, Repository: origin, SHA256: fields["SHA256"]}, nil
}

// aptURIOrigin normalizes apt-get's --print-uris output. Newer Debian APT
// versions can preserve a configured mirror as mirror+file:/path/to/list/...,
// where the actual signed mirror is stored in the referenced mirror-list file.
// The package plan records only that HTTPS origin, never the local resolver URI.
func aptURIOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	if u.Scheme != "mirror+file" || u.Host != "" {
		return ""
	}
	path := filepath.Clean(u.Path)
	if !filepath.IsAbs(path) {
		return ""
	}
	for {
		info, statErr := os.Lstat(path)
		if statErr == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return ""
			}
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				mirror, parseErr := url.Parse(line)
				if parseErr == nil && (mirror.Scheme == "http" || mirror.Scheme == "https") && mirror.Host != "" {
					return mirror.Scheme + "://" + mirror.Host
				}
				return ""
			}
			return ""
		}
		parent := filepath.Dir(path)
		if parent == path {
			return ""
		}
		path = parent
	}
}

var aptSimulationChange = regexp.MustCompile(`^(?:Inst|Conf) ([a-z0-9][a-z0-9+.-]*)(?::([a-z0-9]+))? (?:\[[^\r\n\]]+\] )?\(([0-9A-Za-z.+:~_-]+)(?: |\))`)

func parseAPTChanges(output string) (map[string]string, error) {
	changes := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Remv ") || strings.HasPrefix(line, "Purg ") {
			return nil, failure("package_removal_forbidden", "Signed software plans cannot remove existing system packages", 412)
		}
		if !strings.HasPrefix(line, "Inst ") && !strings.HasPrefix(line, "Conf ") {
			continue
		}
		fields := aptSimulationChange.FindStringSubmatch(line)
		if len(fields) != 4 || fields[2] != "" && fields[2] != runtime.GOARCH || !packageName.MatchString(fields[1]) || !debVersion.MatchString(fields[3]) {
			return nil, failure("package_plan_invalid", "APT returned an unsupported dependency transaction", 412)
		}
		if previous, exists := changes[fields[1]]; exists && previous != fields[3] {
			return nil, failure("package_plan_ambiguous", "APT returned conflicting package versions", 412)
		}
		changes[fields[1]] = fields[3]
		if len(changes) > 128 {
			return nil, failure("package_plan_too_large", "APT dependency plan exceeds 128 packages", 413)
		}
	}
	return changes, nil
}
func (p *packageBackend) simulateAPT(ctx context.Context, pins []PackagePin) (map[string]string, error) {
	args := append(p.options(), "--simulate", "--no-remove", "--no-install-recommends", "install")
	for _, pin := range pins {
		if !packageName.MatchString(pin.Name) || !debVersion.MatchString(pin.Version) {
			return nil, failure("package_pin_invalid", "Invalid exact package selection", 412)
		}
		args = append(args, pin.Name+"="+pin.Version)
	}
	output, err := runFixed(ctx, "/usr/bin/apt-get", args, 2*time.Minute, nil)
	if err != nil {
		return nil, failure("package_plan_conflict", "APT cannot resolve this exact plan without removing existing packages", 412)
	}
	return parseAPTChanges(output)
}

func debFields(raw string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(strings.SplitN(raw, "\n\n", 2)[0], "\n") {
		if key, value, ok := strings.Cut(line, ":"); ok {
			fields[key] = strings.TrimSpace(value)
		}
	}
	return fields
}

func (p *packageBackend) install(ctx context.Context, pins []PackagePin) (returned error) {
	if len(pins) > 0 && pins[0].Format == "rpm" {
		return p.installRPM(ctx, pins)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.prepare(ctx); err != nil {
		return err
	}
	if len(pins) == 0 || len(pins) > 128 {
		return errors.New("exact package pins required")
	}
	args := append(p.options(), "--yes", "--no-remove", "--no-install-recommends", "install")
	for _, pin := range pins {
		if !packageName.MatchString(pin.Name) || !debVersion.MatchString(pin.Version) || !artifactHash.MatchString(pin.SHA256) {
			return failure("package_pin_invalid", "Software plan has an invalid package pin", 412)
		}
		metadata, err := runFixed(ctx, "/usr/bin/apt-cache", append(p.options(), "show", pin.Name+"="+pin.Version), time.Minute, nil)
		if err != nil {
			return err
		}
		fields := debFields(metadata)
		if fields["SHA256"] != pin.SHA256 || fields["Version"] != pin.Version {
			return failure("package_plan_stale", "Signed package metadata changed since planning; make a fresh plan", 412)
		}
		args = append(args, pin.Name+"="+pin.Version)
	}
	changes, err := p.simulateAPT(ctx, pins)
	if err != nil {
		return err
	}
	reviewed := map[string]string{}
	for _, pin := range pins {
		if _, exists := reviewed[pin.Name]; exists {
			return failure("package_plan_ambiguous", "Plan contains duplicate package pins", 412)
		}
		reviewed[pin.Name] = pin.Version
	}
	for name, version := range changes {
		if reviewed[name] != version {
			return failure("package_plan_stale", "APT needs an unreviewed dependency transaction; prepare a fresh plan", 412)
		}
	}
	cleanup, err := p.startupPolicy()
	if err != nil {
		return err
	}
	defer func() {
		if err := cleanup(); err != nil {
			returned = failure("package_recovery_required", "Package transaction could not restore its owned service startup policy; inspect state before retry", 500)
		}
	}()
	installResult, installErr := runFixedResult(ctx, "/usr/bin/apt-get", args, 30*time.Minute, nil, nil)
	if installErr != nil || installResult.ExitCode != 0 || installResult.Truncated {
		return packageInstallFailure(installResult, installErr)
	}
	for _, pin := range pins {
		result, err := runFixed(ctx, "/usr/bin/dpkg-query", []string{"-W", "-f=${Version}", pin.Name}, time.Minute, nil)
		if err != nil || strings.TrimSpace(result) != pin.Version {
			return failure("package_install_unverified", "Installed package version does not match the trusted plan", 412)
		}
	}
	return nil
}

const packageStartupPolicy = "/usr/sbin/policy-rc.d"
const packageStartupBody = "#!/bin/sh\nexit 101\n"

var packageDenyPolicy = regexp.MustCompile(`\A#!/bin/sh\r?\nexit 101\s*\z`)

func readStartupPolicy() ([]byte, os.FileInfo, error) {
	file, err := os.OpenFile(packageStartupPolicy, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 || info.Size() > 4096 {
		return nil, nil, failure("package_service_policy_conflict", "Existing service startup policy must remain trusted and bounded", 412)
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) > 4096 {
		return nil, nil, failure("package_service_policy_conflict", "Existing local service startup policy was preserved", 412)
	}
	return data, info, nil
}
func (p *packageBackend) startupPolicy() (func() error, error) {
	data, _, err := readStartupPolicy()
	if err == nil {
		if !packageDenyPolicy.Match(data) {
			return nil, failure("package_service_policy_conflict", "Existing local service startup policy is preserved; an explicit deny-start policy is required for isolated software installation", 412)
		}
		return func() error { return nil }, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	// A durable marker precedes the global change. After an interrupted Agent,
	// planning fails closed instead of blindly deleting a possibly in-use policy.
	marker := filepath.Join(p.dir, "startup-policy.json")
	if err = writeAtomicRoot(p.dir, "startup-policy.json", []byte(`{"state":"prepared","path":"/usr/sbin/policy-rc.d"}`), 0600, 0); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(packageStartupPolicy, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
	if err != nil {
		_ = os.Remove(marker)
		return nil, err
	}
	info, statErr := file.Stat()
	_, writeErr := file.WriteString(packageStartupBody)
	syncErr := file.Sync()
	closeErr := file.Close()
	if statErr != nil {
		return nil, failure("package_recovery_required", "Unable to identify newly owned startup policy; its transaction marker was retained", 500)
	}
	owned := info.Sys().(*syscall.Stat_t)
	cleanup := func() error {
		contents, current, err := readStartupPolicy()
		if os.IsNotExist(err) {
			return os.Remove(marker)
		}
		if err != nil {
			return err
		}
		stat, ok := current.Sys().(*syscall.Stat_t)
		if !ok || stat.Dev != owned.Dev || stat.Ino != owned.Ino || string(contents) != packageStartupBody {
			return failure("package_service_policy_changed", "Owned startup policy changed; foreign edits were preserved", 409)
		}
		if err = os.Remove(packageStartupPolicy); err != nil {
			return err
		}
		directory, err := os.Open(filepath.Dir(packageStartupPolicy))
		if err != nil {
			return err
		}
		syncErr := directory.Sync()
		_ = directory.Close()
		if syncErr != nil {
			return syncErr
		}
		if err = os.Remove(marker); err != nil {
			return err
		}
		directory, err = os.Open(p.dir)
		if err != nil {
			return err
		}
		defer directory.Close()
		return directory.Sync()
	}
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = cleanup()
		return nil, failure("package_policy_failed", "Unable to establish the owned deny-start policy; inspect transaction state if interrupted", 502)
	}
	return cleanup, nil
}

// runFixed is constructed only by trusted Go recipes. Runtime requests never
// choose program names, arbitrary argv, environment or process credentials.
func runFixed(ctx context.Context, program string, args []string, timeout time.Duration, credential *syscall.Credential) (string, error) {
	return runFixedEnvironment(ctx, program, args, timeout, credential, nil)
}
func runFixedEnvironment(ctx context.Context, program string, args []string, timeout time.Duration, credential *syscall.Credential, environment []string) (string, error) {
	result, err := runFixedResult(ctx, program, args, timeout, credential, environment)
	if err != nil && result.Operation == "" {
		return "", err
	}
	if err != nil || result.ExitCode != 0 {
		return "", failure("software_command_failed", "A trusted software command failed; inspect service prerequisites", 502)
	}
	if result.Truncated {
		return "", failure("result_too_large", "Trusted command output exceeded its limit", 413)
	}
	return result.Stdout, nil
}

func runFixedResult(ctx context.Context, program string, args []string, timeout time.Duration, credential *syscall.Credential, environment []string) (agent.Result, error) {
	operation := agent.Operation{Name: "plugin_fixed_command", Program: program, Timeout: timeout, Environment: append([]string{"DEBIAN_FRONTEND=noninteractive"}, environment...), BuildArgs: func(json.RawMessage) ([]string, error) { return append([]string(nil), args...), nil }}
	if credential != nil {
		operation.Credential = credential
	}
	runner, err := agent.NewRunner(agent.Options{Operations: []agent.Operation{operation}, MaxConcurrent: 1, MaxOutputBytes: 1 << 20})
	if err != nil {
		return agent.Result{}, err
	}
	return runner.Run(ctx, agent.Request{Operation: operation.Name, Input: json.RawMessage(`{}`)})
}

func runFixedDirectoryResult(ctx context.Context, program, directory string, args []string, timeout time.Duration, credential *syscall.Credential, environment []string) (agent.Result, error) {
	operation := agent.Operation{Name: "plugin_fixed_command", Program: program, Timeout: timeout, WorkDir: directory, Environment: append([]string{"DEBIAN_FRONTEND=noninteractive"}, environment...), BuildArgs: func(json.RawMessage) ([]string, error) { return append([]string(nil), args...), nil }}
	if credential != nil {
		operation.Credential = credential
	}
	runner, err := agent.NewRunner(agent.Options{Operations: []agent.Operation{operation}, MaxConcurrent: 1, MaxOutputBytes: 1 << 20})
	if err != nil {
		return agent.Result{}, err
	}
	return runner.Run(ctx, agent.Request{Operation: operation.Name, Input: json.RawMessage(`{}`)})
}
