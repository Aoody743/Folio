package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

var rpmSignature = regexp.MustCompile(`(?m)(?:RSA|ECDSA|EdDSA)/(?:SHA256|SHA384|SHA512) Signature[^\r\n]*: OK\s*$`)
var rpmArtifactName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+~-]{0,240}\.rpm$`)

func (p *packageBackend) rpmOptions() []string {
	return []string{"--setopt=gpgcheck=1", "--setopt=localpkg_gpgcheck=1", "--setopt=sslverify=1", "--setopt=install_weak_deps=False", "--setopt=cachedir=" + filepath.Join(p.dir, "rpm-cache"), "--setopt=logdir=" + filepath.Join(p.dir, "rpm-logs")}
}
func (p *packageBackend) prepareRPM() error {
	if !p.enabled || os.Geteuid() != 0 {
		return failure("package_install_disabled", "Root Agent policy must enable signed RPM packages", 412)
	}
	for _, program := range []string{"/usr/bin/dnf", "/usr/bin/rpm"} {
		if _, err := os.Stat(program); err != nil {
			return failure("unsupported_platform", "This signed RPM recipe requires DNF and RPM", 412)
		}
	}
	for _, dir := range []string{filepath.Join(p.dir, "rpm"), filepath.Join(p.dir, "rpm-cache"), filepath.Join(p.dir, "rpm-logs")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		if err := realDirectory(dir); err != nil {
			return err
		}
	}
	return nil
}
func (p *packageBackend) resolveRPM(ctx context.Context, source Source) ([]PackagePin, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.prepareRPM(); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(filepath.Join(p.dir, "rpm"), ".plan-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	// Plan downloads signed artifacts into private cache. Existing trust keys
	// are required; -y is deliberately absent from the planning transaction.
	args := append(p.rpmOptions(), "--assumeno", "download", "--resolve", "--alldeps", "--destdir", stage)
	for _, name := range source.Packages {
		if !packageName.MatchString(name) {
			return nil, failure("invalid_source", "Trusted RPM package name is invalid", 500)
		}
		args = append(args, name)
	}
	if _, err = runFixed(ctx, "/usr/bin/dnf", args, 15*time.Minute, nil); err != nil {
		return nil, failure("signed_rpm_unavailable", "DNF could not obtain every dependency from configured trusted repositories; install the official source/key and download plugin first", 412)
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 || len(entries) > 128 {
		return nil, failure("package_plan_too_large", "RPM plan must contain 1–128 exact signed packages", 413)
	}
	var primary []PackagePin
	dependencies := []PackagePin{}
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !rpmArtifactName.MatchString(entry.Name()) {
			return nil, failure("package_artifact_invalid", "DNF download produced an unexpected entry", 412)
		}
		name := filepath.Join(stage, entry.Name())
		pin, err := p.inspectRPM(ctx, name)
		if err != nil {
			return nil, err
		}
		if seen[pin.Name] {
			return nil, failure("package_plan_ambiguous", "RPM plan contains multiple versions of a package", 412)
		}
		seen[pin.Name] = true
		cache := filepath.Join(p.dir, "rpm", pin.SHA256+".rpm")
		if err = os.Rename(name, cache); err != nil {
			return nil, err
		}
		if containsString(source.Packages, pin.Name) {
			primary = append(primary, pin)
		} else {
			dependencies = append(dependencies, pin)
		}
	}
	if len(primary) != len(source.Packages) {
		return nil, failure("package_plan_incomplete", "DNF did not return every explicitly requested RPM package", 412)
	}
	ordered := make([]PackagePin, 0, len(primary))
	for _, name := range source.Packages {
		for _, pin := range primary {
			if pin.Name == name {
				ordered = append(ordered, pin)
			}
		}
	}
	return append(ordered, dependencies...), nil
}
func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
func (p *packageBackend) inspectRPM(ctx context.Context, name string) (PackagePin, error) {
	var pin PackagePin
	if !rpmArtifactName.MatchString(filepath.Base(name)) {
		return pin, failure("package_artifact_invalid", "RPM artifact filename is not canonical", 412)
	}
	result, err := runFixed(ctx, "/usr/bin/rpm", []string{"--checksig", "--verbose", name}, time.Minute, nil)
	if err != nil || !rpmSignature.MatchString(result) {
		return pin, failure("package_signature_invalid", "RPM must have a valid trusted signature using SHA-256 or stronger; unsigned/digest-only artifacts are rejected", 412)
	}
	metadata, err := runFixed(ctx, "/usr/bin/rpm", []string{"-qp", "--queryformat", "%{NAME}|%{EPOCHNUM}:%{VERSION}-%{RELEASE}|%{ARCH}", name}, time.Minute, nil)
	if err != nil {
		return pin, err
	}
	fields := strings.Split(strings.TrimSpace(metadata), "|")
	if len(fields) != 3 || !packageName.MatchString(fields[0]) || !debVersion.MatchString(fields[1]) {
		return pin, failure("package_metadata_invalid", "Signed RPM identity metadata is invalid", 412)
	}
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	if fields[2] != arch && fields[2] != "noarch" {
		return pin, failure("unsupported_platform", "Signed RPM architecture does not match this privileged host executor", 412)
	}
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return pin, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxArtifactBytes {
		return pin, failure("package_artifact_invalid", "RPM must be a bounded regular file", 412)
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return pin, err
	}
	location, err := runFixed(ctx, "/usr/bin/dnf", append(p.rpmOptions(), "repoquery", "--location", fields[0]+"-"+strings.TrimPrefix(fields[1], "0:")), time.Minute, nil)
	if err != nil {
		return pin, failure("package_origin_unknown", "Signed RPM source could not be resolved", 412)
	}
	origin := ""
	for _, line := range strings.Split(location, "\n") {
		u, err := url.Parse(strings.TrimSpace(line))
		if err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
			origin = u.Scheme + "://" + u.Host
			break
		}
	}
	if origin == "" {
		return pin, failure("package_origin_unknown", "DNF did not identify the exact RPM repository", 412)
	}
	return PackagePin{Name: fields[0], Version: fields[1], Architecture: fields[2], Repository: origin, SHA256: hex.EncodeToString(hash.Sum(nil)), Format: "rpm"}, nil
}
func (p *packageBackend) installRPM(ctx context.Context, pins []PackagePin) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.prepareRPM(); err != nil {
		return err
	}
	if len(pins) == 0 || len(pins) > 128 {
		return failure("package_plan_invalid", "An exact bounded RPM plan is required", 412)
	}
	args := append(p.rpmOptions(), "--assumeyes", "install")
	for _, pin := range pins {
		if pin.Format != "rpm" || !artifactHash.MatchString(pin.SHA256) {
			return failure("package_pin_invalid", "RPM plan contains an invalid pin", 412)
		}
		name := filepath.Join(p.dir, "rpm", pin.SHA256+".rpm")
		actual, err := p.inspectRPM(ctx, name)
		if err != nil {
			return err
		}
		if actual.Name != pin.Name || actual.Version != pin.Version || actual.SHA256 != pin.SHA256 || actual.Architecture != pin.Architecture {
			return failure("package_plan_stale", "Signed RPM artifact no longer matches its reviewed plan", 412)
		}
		args = append(args, name)
	}
	if _, err := runFixed(ctx, "/usr/bin/dnf", args, 30*time.Minute, nil); err != nil {
		return failure("package_install_failed", "Exact signed RPM installation failed", 502)
	}
	for _, pin := range pins {
		version, err := runFixed(ctx, "/usr/bin/rpm", []string{"-q", "--queryformat", "%{EPOCHNUM}:%{VERSION}-%{RELEASE}", pin.Name}, time.Minute, nil)
		if err != nil || strings.TrimSpace(version) != pin.Version {
			return failure("package_install_unverified", "Installed RPM version does not match its signed plan", 412)
		}
	}
	return nil
}
