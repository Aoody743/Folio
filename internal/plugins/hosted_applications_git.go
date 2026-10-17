package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

type hostedAppGitInput struct {
	ID               string
	ExpectedRevision int64
	RepositoryURL    string
	Branch           string
	HostKeySHA256    string
}

type hostedAppGitRemote struct {
	URL         string
	Scheme      string
	Host        string
	Port        string
	Branch      string
	Fingerprint string
}

var hostedGitHostPattern = regexp.MustCompile("^[a-zA-Z0-9.-]{1,253}$")
var hostedGitPathPattern = regexp.MustCompile("^/[A-Za-z0-9._/-]+$")
var hostedGitBranchPattern = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$")
var hostedGitCommitPattern = regexp.MustCompile("^(?:[a-f0-9]{40}|[a-f0-9]{64})$")
var hostedGitStagePattern = regexp.MustCompile(`^\.git-import-[a-f0-9]{48}$`)

func hostedAppGitSchema() map[string]any {
	return obj(map[string]any{
		"id":                idSchema(),
		"expected_revision": revisionSchema(),
		"repository_url":    map[string]any{"type": "string", "minLength": 1, "maxLength": 2048},
		"branch":            map[string]any{"type": "string", "maxLength": 128},
		"host_key_sha256":   map[string]any{"type": "string", "maxLength": 64},
	}, "id", "expected_revision", "repository_url")
}

func parseHostedAppGitInput(raw json.RawMessage) (hostedAppGitInput, error) {
	var input hostedAppGitInput
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return input, failure("invalid_input", "Git source input must be an object", 400)
	}
	for name := range fields {
		switch name {
		case "id", "expected_revision", "repository_url", "branch", "host_key_sha256":
		default:
			return input, failure("invalid_input", "Git source input contains an unsupported field", 400)
		}
	}
	if json.Unmarshal(fields["id"], &input.ID) != nil ||
		json.Unmarshal(fields["expected_revision"], &input.ExpectedRevision) != nil ||
		json.Unmarshal(fields["repository_url"], &input.RepositoryURL) != nil {
		return input, failure("invalid_input", "Git source target, revision and repository URL are required", 400)
	}
	if len(fields["branch"]) != 0 && json.Unmarshal(fields["branch"], &input.Branch) != nil {
		return input, failure("invalid_input", "Git branch is invalid", 400)
	}
	if len(fields["host_key_sha256"]) != 0 && json.Unmarshal(fields["host_key_sha256"], &input.HostKeySHA256) != nil {
		return input, failure("invalid_input", "SSH host-key fingerprint is invalid", 400)
	}
	return input, nil
}

func parseHostedAppGitRemote(input hostedAppGitInput) (hostedAppGitRemote, error) {
	remote := hostedAppGitRemote{URL: input.RepositoryURL, Branch: input.Branch, Fingerprint: input.HostKeySHA256}
	if !hostedAppValidID(input.ID) || input.ExpectedRevision < 1 || len(input.RepositoryURL) == 0 || len(input.RepositoryURL) > 2048 {
		return remote, failure("invalid_input", "Git source target or repository URL is invalid", 400)
	}
	if remote.Branch == "" {
		remote.Branch = "main"
	}
	if !hostedGitBranchPattern.MatchString(remote.Branch) || strings.Contains(remote.Branch, "..") || strings.Contains(remote.Branch, "//") || strings.Contains(remote.Branch, "@{") || strings.HasSuffix(remote.Branch, "/") || strings.HasSuffix(remote.Branch, ".") || strings.Contains(remote.Branch, "/.") || strings.Contains(remote.Branch, ".lock") {
		return remote, failure("invalid_input", "Git branch must be a simple valid branch name", 400)
	}
	parsed, err := url.Parse(remote.URL)
	if err != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Host == "" || strings.HasSuffix(parsed.Host, ":") {
		return remote, failure("invalid_input", "Git source must be an HTTPS or SSH repository URL without query, fragment or credentials", 400)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "ssh" {
		return remote, failure("invalid_input", "Git source supports HTTPS and SSH URLs only", 400)
	}
	host := parsed.Hostname()
	if host == "" || strings.Contains(host, "%") {
		return remote, failure("invalid_input", "Git repository host is invalid", 400)
	}
	if net.ParseIP(host) == nil {
		if !hostedGitHostPattern.MatchString(host) || strings.Contains(host, "..") || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
			return remote, failure("invalid_input", "Git repository host is invalid", 400)
		}
		for _, label := range strings.Split(host, ".") {
			if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				return remote, failure("invalid_input", "Git repository host is invalid", 400)
			}
		}
	}
	if !hostedGitPathPattern.MatchString(parsed.EscapedPath()) || strings.Contains(parsed.Path, "//") {
		return remote, failure("invalid_input", "Git repository path contains unsupported characters", 400)
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) < 2 {
		return remote, failure("invalid_input", "Git repository URL must include an owner or group and repository name", 400)
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return remote, failure("invalid_input", "Git repository path is invalid", 400)
		}
	}
	port := parsed.Port()
	if port != "" {
		value, parseErr := strconv.Atoi(port)
		if parseErr != nil || value < 1 || value > 65535 {
			return remote, failure("invalid_input", "Git repository port is invalid", 400)
		}
	}
	remote.Scheme = parsed.Scheme
	remote.Host = host
	remote.Port = port
	if remote.Scheme == "https" {
		if parsed.User != nil || remote.Fingerprint != "" {
			return remote, failure("invalid_input", "HTTPS Git URLs cannot contain credentials or an SSH host-key fingerprint", 400)
		}
	} else {
		password, hasPassword := "", false
		if parsed.User != nil {
			password, hasPassword = parsed.User.Password()
		}
		if parsed.User == nil || parsed.User.Username() != "git" || hasPassword || password != "" {
			return remote, failure("invalid_input", "SSH Git URLs must use the public-repository git account and no embedded credentials", 400)
		}
		if !strings.HasPrefix(remote.Fingerprint, "SHA256:") {
			return remote, failure("host_key_pin_required", "SSH Git sources require a SHA256 host-key fingerprint", 400)
		}
		fingerprint, decodeErr := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(remote.Fingerprint, "SHA256:"))
		if decodeErr != nil || len(fingerprint) != sha256.Size {
			return remote, failure("host_key_pin_required", "SSH Git host-key fingerprint must be a valid SHA256 fingerprint", 400)
		}
	}
	return remote, nil
}

func validHostedAppGitInput(raw json.RawMessage) error {
	input, err := parseHostedAppGitInput(raw)
	if err != nil {
		return err
	}
	_, err = parseHostedAppGitRemote(input)
	return err
}

func (e *PrivilegedExecutor) hostedAppGitProgram(ctx context.Context) (string, error) {
	instance, err := e.get(ctx, "git-default", false)
	if err != nil || instance.PluginID != "git" || instance.Status == "uninstalled" || instance.Service.Program == "" {
		return "", failure("application_git_required", "Install the trusted Git client from Software before importing a repository", 412)
	}
	program, err := validateTrustedProgramPath(instance.Service.Program)
	if err != nil {
		return "", failure("application_git_required", "The managed Git client executable is missing or no longer trusted", 412)
	}
	return program, nil
}

func hostedGitEnvironment(home string, remote hostedAppGitRemote, knownHosts string, sshProgram string) []string {
	environment := []string{
		"HOME=" + home,
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=https:ssh",
		"GIT_LFS_SKIP_SMUDGE=1",
	}
	if remote.Scheme == "ssh" {
		sshCommand := shellSingleQuote(sshProgram) + " -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=" + shellSingleQuote(knownHosts) + " -o GlobalKnownHostsFile=/dev/null -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no -o IdentitiesOnly=yes -o ConnectTimeout=10"
		environment = append(environment, "GIT_SSH_COMMAND="+sshCommand)
	}
	return environment
}

func (e *PrivilegedExecutor) pinnedGitKnownHosts(ctx context.Context, remote hostedAppGitRemote, credential *syscall.Credential, path string) error {
	keyscan, err := validateTrustedProgramPath("/usr/bin/ssh-keyscan")
	if err != nil {
		return failure("application_git_ssh_unavailable", "OpenSSH client tools are required for pinned SSH Git imports", 412)
	}
	port := remote.Port
	if port == "" {
		port = "22"
	}
	result, err := runFixedResult(ctx, keyscan, []string{"-T", "5", "-p", port, "-t", "ed25519,ecdsa,rsa", remote.Host}, 15*time.Second, credential, []string{"PATH=/usr/bin:/bin"})
	if err != nil || result.ExitCode != 0 || result.Truncated || result.Canceled || result.TimedOut {
		return failure("application_git_host_unreachable", "Could not retrieve SSH host keys for the requested Git server", 502)
	}
	expectedHost := remote.Host
	if port != "22" {
		expectedHost = "[" + remote.Host + "]:" + port
	}
	matches := []string{}
	for _, line := range strings.Split(result.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != expectedHost {
			continue
		}
		keyBytes, decodeErr := base64.StdEncoding.DecodeString(fields[2])
		if decodeErr != nil {
			continue
		}
		publicKey, parseErr := ssh.ParsePublicKey(keyBytes)
		if parseErr != nil || ssh.FingerprintSHA256(publicKey) != remote.Fingerprint {
			continue
		}
		matches = append(matches, fields[0]+" "+fields[1]+" "+fields[2])
	}
	if len(matches) == 0 {
		return failure("application_git_host_key_mismatch", "The Git server did not present the pinned SSH host key", 412)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	for _, line := range matches {
		if _, err = file.WriteString(line + "\n"); err != nil {
			break
		}
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (e *PrivilegedExecutor) importHostedAppGit(ctx context.Context, input hostedAppGitInput) (HostedApplication, error) {
	var empty HostedApplication
	if err := e.hostedAppsEnabled(); err != nil {
		return empty, err
	}
	remote, err := parseHostedAppGitRemote(input)
	if err != nil {
		return empty, err
	}
	runtimeUnlock, err := e.lockContext(ctx, "hosted-app-runtimes")
	if err != nil {
		return empty, err
	}
	defer runtimeUnlock()
	unlock, err := e.lockContext(ctx, "application-"+input.ID)
	if err != nil {
		return empty, err
	}
	defer unlock()
	receipt, err := e.hostedAppReceipt(input.ID)
	if err != nil {
		return empty, err
	}
	if receipt.Status == "retained" || receipt.Revision != input.ExpectedRevision {
		return empty, failure("revision_conflict", "Application changed or was retained; reload before importing a Git source", 412)
	}
	if running, stateErr := e.hostedAppRunning(ctx, receipt, true); stateErr != nil {
		return empty, stateErr
	} else if running {
		return empty, failure("application_running", "Stop the application before importing a Git source", 412)
	}
	git, err := e.hostedAppGitProgram(ctx)
	if err != nil {
		return empty, err
	}
	if err = cleanupHostedGitStages(receipt.Root, receipt.UID, receipt.GID); err != nil {
		return empty, err
	}
	credential := &syscall.Credential{Uid: receipt.UID, Gid: receipt.GID, Groups: []uint32{receipt.GID}}
	stageID, err := randomPlanID()
	if err != nil {
		return empty, err
	}
	stage := filepath.Join(receipt.Root, ".git-import-"+strings.TrimPrefix(stageID, "plan_"))
	if err = os.Mkdir(stage, 0700); err != nil {
		return empty, err
	}
	defer os.RemoveAll(stage)
	if err = os.Chown(stage, int(receipt.UID), int(receipt.GID)); err != nil {
		return empty, err
	}
	if err = os.Chmod(stage, 0750); err != nil {
		return empty, err
	}
	home := filepath.Join(stage, ".home")
	if err = os.Mkdir(home, 0700); err != nil {
		return empty, err
	}
	if err = os.Chown(home, int(receipt.UID), int(receipt.GID)); err != nil {
		return empty, err
	}
	sshProgram := ""
	if remote.Scheme == "ssh" {
		sshProgram, err = validateTrustedProgramPath("/usr/bin/ssh")
		if err != nil {
			return empty, failure("application_git_ssh_unavailable", "OpenSSH client tools are required for pinned SSH Git imports", 412)
		}
	}
	knownHosts := ""
	if remote.Scheme == "ssh" {
		knownHosts = filepath.Join(home, "known_hosts")
		if err = e.pinnedGitKnownHosts(ctx, remote, credential, knownHosts); err != nil {
			return empty, err
		}
		if err = os.Chown(knownHosts, int(receipt.UID), int(receipt.GID)); err != nil {
			return empty, err
		}
	}
	environment := hostedGitEnvironment(home, remote, knownHosts, sshProgram)
	cloneArgs := []string{"-c", "core.hooksPath=/dev/null", "clone", "--no-tags", "--depth=1", "--single-branch", "--branch=" + remote.Branch, "--", remote.URL, "project"}
	clone, cloneErr := runFixedDirectoryResult(ctx, git, stage, cloneArgs, 30*time.Minute, credential, environment)
	if cloneErr != nil || clone.ExitCode != 0 || clone.Truncated || clone.Canceled || clone.TimedOut {
		return empty, failure("application_git_clone_failed", "Git clone failed under the application's isolated system user", 502)
	}
	project := filepath.Join(stage, "project")
	commitResult, commitErr := runFixedDirectoryResult(ctx, git, project, []string{"rev-parse", "--verify", "HEAD^{commit}"}, 15*time.Second, credential, environment)
	if commitErr != nil || commitResult.ExitCode != 0 || commitResult.Truncated || commitResult.Canceled || commitResult.TimedOut {
		return empty, failure("application_git_clone_failed", "Git checkout did not resolve to a verified commit", 502)
	}
	commit := strings.TrimSpace(commitResult.Stdout)
	if !hostedGitCommitPattern.MatchString(commit) {
		return empty, failure("application_git_clone_failed", "Git checkout returned an invalid commit identity", 502)
	}
	manifest := "package.json"
	if receipt.Runtime == "go" {
		manifest = "go.mod"
	}
	if err = validateHostedGitProject(project, manifest, receipt.UID, receipt.GID); err != nil {
		return empty, err
	}
	if remote.Scheme == "ssh" {
		if err = os.Remove(knownHosts); err != nil {
			return empty, err
		}
	}
	if err = os.Rename(home, filepath.Join(project, ".home")); err != nil {
		return empty, err
	}
	digest := sha256.Sum256([]byte(remote.URL + "\x00" + commit))
	sourceHash := hex.EncodeToString(digest[:])
	previous := hostedAppPreviousProject(receipt)
	if _, statErr := os.Lstat(previous); statErr == nil {
		if err = validateParkedHostedAppSource(previous); err != nil {
			return empty, err
		}
		if err = os.RemoveAll(previous); err != nil {
			return empty, err
		}
	} else if !os.IsNotExist(statErr) {
		return empty, statErr
	}
	if err = os.Rename(receipt.ProjectDir, previous); err != nil {
		return empty, err
	}
	if err = parkHostedAppSource(previous); err != nil {
		_ = os.Rename(previous, receipt.ProjectDir)
		return empty, err
	}
	if err = os.Rename(project, receipt.ProjectDir); err != nil {
		_ = restoreParkedHostedAppSource(previous, receipt.ProjectDir, receipt.UID, receipt.GID)
		return empty, err
	}
	if err = os.Chown(receipt.ProjectDir, int(receipt.UID), int(receipt.GID)); err != nil {
		_ = os.Rename(receipt.ProjectDir, project)
		_ = restoreParkedHostedAppSource(previous, receipt.ProjectDir, receipt.UID, receipt.GID)
		return empty, err
	}
	if err = os.Chmod(receipt.ProjectDir, 0750); err != nil {
		_ = os.Rename(receipt.ProjectDir, project)
		_ = restoreParkedHostedAppSource(previous, receipt.ProjectDir, receipt.UID, receipt.GID)
		return empty, err
	}
	oldSourceHash := receipt.SourceSHA256
	receipt.PreviousSourceSHA256 = oldSourceHash
	receipt.SourceSHA256 = sourceHash
	receipt.Status = "created"
	receipt.Revision++
	receipt.UpdatedAt = time.Now().UTC()
	if err = e.saveHostedApp(receipt); err != nil {
		current, readErr := e.hostedAppReceipt(receipt.ID)
		if readErr != nil || current.Revision != receipt.Revision || current.SourceSHA256 != receipt.SourceSHA256 {
			if removeErr := os.RemoveAll(receipt.ProjectDir); removeErr == nil {
				_ = restoreParkedHostedAppSource(previous, receipt.ProjectDir, receipt.UID, receipt.GID)
			}
			return empty, err
		}
	}
	return receipt.HostedApplication, nil
}

func cleanupHostedGitStages(root string, uid, gid uint32) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !hostedGitStagePattern.MatchString(entry.Name()) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, statErr := os.Lstat(path)
		stat, ok := statOf(info)
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || (stat.Uid != 0 && stat.Uid != uid) || (stat.Gid != 0 && stat.Gid != gid) || info.Mode().Perm()&0022 != 0 {
			return failure("ownership_conflict", "An interrupted Git import directory has unexpected ownership or type; it was preserved", 409)
		}
		if err = os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

func validateHostedGitProject(project, manifest string, uid, gid uint32) error {
	manifestFound := false
	err := filepath.WalkDir(project, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == project {
			return nil
		}
		relative, err := filepath.Rel(project, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		first := strings.Split(relative, "/")[0]
		if first == ".home" || first == ".next" || strings.HasPrefix(first, ".next.") || strings.HasPrefix(first, ".powerlinux-app") {
			return failure("application_git_reserved_path", "Git repository contains a reserved PowerLinux runtime path", 400)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!entry.IsDir() && !info.Mode().IsRegular()) {
			return failure("application_git_unsafe_entry", "Git repositories may contain only regular files and directories", 400)
		}
		if entry.IsDir() {
			if err = os.Chown(path, int(uid), int(gid)); err != nil {
				return err
			}
			return os.Chmod(path, 0750)
		}
		if relative == filepath.FromSlash(manifest) {
			manifestFound = true
		}
		if err = os.Chown(path, int(uid), int(gid)); err != nil {
			return err
		}
		mode := os.FileMode(0640)
		if info.Mode()&0111 != 0 {
			mode = 0750
		}
		return os.Chmod(path, mode)
	})
	if err != nil {
		return err
	}
	if !manifestFound {
		return failure("application_manifest_required", "Git repository must contain a root-level "+manifest+" file", 400)
	}
	return nil
}

func (e *PrivilegedExecutor) runHostedAppGit(ctx context.Context, raw json.RawMessage) (HostedApplication, error) {
	input, err := parseHostedAppGitInput(raw)
	if err != nil {
		return HostedApplication{}, err
	}
	if _, err = parseHostedAppGitRemote(input); err != nil {
		return HostedApplication{}, err
	}
	return e.importHostedAppGit(ctx, input)
}
