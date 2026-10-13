package sites

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

const phpCLIOwnerFile = "php-cli-owner.json"

type phpCLIOwner struct {
	Directory string `json:"directory"`
	Device    uint64 `json:"device"`
	Inode     uint64 `json:"inode"`
}
type phpCLISelection struct {
	ID            string   `json:"id"`
	Instance      string   `json:"php_instance"`
	Program       string   `json:"program"`
	Version       string   `json:"version"`
	WrapperSHA256 string   `json:"wrapper_sha256"`
	IniSHA256     string   `json:"ini_sha256"`
	Extensions    []string `json:"extensions"`
}
type phpCLIProbe struct {
	Version      string   `json:"version"`
	ExtensionDir string   `json:"extension_dir"`
	Modules      []string `json:"modules"`
}
type phpCLIPermission struct {
	Path string
	Mode os.FileMode
}

var phpCLISelectionPath = regexp.MustCompile(`^selections/[a-f0-9]{32}$`)
var phpCLIExtension = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}\.so$`)
var phpCLIInteger = regexp.MustCompile(`^[0-9]{1,8}[KMGkmg]?$`)
var phpCLIFunctions = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(,[A-Za-z_][A-Za-z0-9_]*)*$`)

func phpCLIQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
func phpCLIWrapper(program, selection string) []byte {
	return []byte("#!/bin/sh\n# PowerLinux owned PHP CLI entry point.\nunset PHPRC\nPHP_INI_SCAN_DIR=" + phpCLIQuote(filepath.Join(selection, "conf.d")) + "\nexport PHP_INI_SCAN_DIR\nexec " + phpCLIQuote(program) + " -c " + phpCLIQuote(filepath.Join(selection, "php.ini")) + " \"$@\"\n")
}
func phpCLIDigest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// Only scalar CLI settings and module basenames leave the protected FPM tree.
// Includes, session/log paths, pools, credentials and arbitrary PHP directives
// are deliberately absent from the public configuration.
func sanitizePHPCLIIni(raw []byte) ([]byte, []string, error) {
	if len(raw) > 256<<10 {
		return nil, nil, failure("php_cli_config_invalid", "PHP configuration exceeds its limit", 413)
	}
	out := "[PHP]\nexpose_php=Off\nallow_url_include=Off\nsession.save_handler=files\n"
	integer := map[string]bool{"memory_limit": true, "max_execution_time": true, "max_input_time": true, "upload_max_filesize": true, "post_max_size": true, "max_input_vars": true}
	boolean := map[string]bool{"display_errors": true, "log_errors": true, "short_open_tag": true, "file_uploads": true, "opcache.enable": true, "opcache.enable_cli": true}
	modules := []string{}
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)
		if strings.ContainsAny(value, "\x00\r\n") {
			return nil, nil, failure("php_cli_config_invalid", "Invalid PHP CLI setting", 412)
		}
		switch {
		case integer[key]:
			if !phpCLIInteger.MatchString(value) && !(key == "memory_limit" && value == "-1") {
				return nil, nil, failure("php_cli_config_invalid", "Invalid bounded PHP CLI setting", 412)
			}
			out += key + "=" + value + "\n"
		case boolean[key]:
			if !slices.Contains([]string{"0", "1", "on", "off", "true", "false", "yes", "no"}, strings.ToLower(value)) {
				return nil, nil, failure("php_cli_config_invalid", "Invalid PHP CLI boolean", 412)
			}
			out += key + "=" + value + "\n"
		case key == "disable_functions":
			if value != "" && !phpCLIFunctions.MatchString(value) {
				return nil, nil, failure("php_cli_config_invalid", "Invalid disabled PHP functions", 412)
			}
			out += key + "=" + value + "\n"
		case key == "date.timezone":
			value = strings.Trim(value, "\"'")
			if _, err := time.LoadLocation(value); err != nil {
				return nil, nil, failure("php_cli_config_invalid", "Invalid PHP CLI timezone", 412)
			}
			out += key + "=" + strconv.Quote(value) + "\n"
		case key == "extension" || key == "zend_extension":
			if !phpCLIExtension.MatchString(value) {
				return nil, nil, failure("php_cli_extension_invalid", "CLI modules must be trusted runtime module basenames", 412)
			}
			if slices.Contains(modules, value) {
				return nil, nil, failure("php_cli_extension_invalid", "Duplicate PHP CLI module", 412)
			}
			modules = append(modules, value)
			out += key + "=" + value + "\n"
		}
	}
	if scanner.Err() != nil {
		return nil, nil, failure("php_cli_config_invalid", "Invalid PHP CLI configuration", 412)
	}
	return []byte(out), modules, nil
}

func phpCLIRead(path string, limit int64) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || !ok || stat.Uid != 0 || info.Size() > limit {
		return nil, failure("php_cli_ownership_invalid", "CLI metadata/configuration must remain root owned and bounded", 412)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if int64(len(data)) > limit {
		return nil, failure("php_cli_config_invalid", "PHP CLI file exceeds its limit", 413)
	}
	return data, err
}
func phpCLIExec(ctx context.Context, program string, args []string, scan string, ordinary bool) (phpCLIProbe, error) {
	op := agent.Operation{Name: "php_cli_probe", Program: program, Timeout: 10 * time.Second, Environment: []string{"PHP_INI_SCAN_DIR=" + scan, "PHPRC="}, BuildArgs: func(json.RawMessage) ([]string, error) { return args, nil }}
	if ordinary {
		worker, err := user.Lookup("nobody")
		if err != nil {
			return phpCLIProbe{}, failure("php_cli_user_unavailable", "A nonprivileged CLI probe identity is required", 412)
		}
		uid, err := strconv.ParseUint(worker.Uid, 10, 32)
		if err != nil || uid == 0 {
			return phpCLIProbe{}, failure("php_cli_user_unavailable", "Invalid CLI probe identity", 412)
		}
		gid, err := strconv.ParseUint(worker.Gid, 10, 32)
		if err != nil {
			return phpCLIProbe{}, err
		}
		op.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	}
	runner, err := agent.NewRunner(agent.Options{Operations: []agent.Operation{op}, MaxConcurrent: 1, MaxOutputBytes: 32 << 10})
	if err != nil {
		return phpCLIProbe{}, err
	}
	result, err := runner.Run(ctx, agent.Request{Operation: op.Name, Input: json.RawMessage(`{}`)})
	if err != nil || result.ExitCode != 0 || result.Truncated {
		return phpCLIProbe{}, failure("php_cli_probe_failed", "Selected PHP CLI could not run with the public configuration", 502)
	}
	var probe phpCLIProbe
	if json.Unmarshal([]byte(result.Stdout), &probe) != nil || !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+`).MatchString(probe.Version) || !filepath.IsAbs(probe.ExtensionDir) {
		return probe, failure("php_cli_probe_failed", "Trusted executable did not report a valid PHP CLI runtime", 502)
	}
	return probe, nil
}

const phpCLIProbeCode = `echo json_encode(["version"=>PHP_VERSION,"extension_dir"=>PHP_EXTENSION_DIR,"modules"=>get_loaded_extensions()]);`

func phpCLIModulesLoaded(probe phpCLIProbe, modules []string) bool {
	for _, module := range modules {
		wanted := strings.TrimSuffix(module, ".so")
		if wanted == "opcache" {
			wanted = "zend opcache"
		}
		found := false
		for _, loaded := range probe.Modules {
			if strings.EqualFold(loaded, wanted) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Native receipts authorize only these exact executable/module paths to become
// public. Their configuration/data siblings are never traversed or chmodded.
func phpCLIPublicAccess(path, prefix string, executable bool) ([]phpCLIPermission, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if err = trustedPHPCLIFile(canonical, executable); err != nil {
		return nil, err
	}
	rel, relErr := filepath.Rel(prefix, canonical)
	native := relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
	changes := []phpCLIPermission{}
	for current := canonical; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return changes, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		stickyAncestor := current != canonical && info.IsDir() && info.Mode()&os.ModeSticky != 0
		if !ok || stat.Uid != 0 || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 && !stickyAncestor || current != canonical && !info.IsDir() {
			return changes, failure("php_cli_ownership_invalid", "PHP CLI access paths must remain root controlled", 412)
		}
		needed := os.FileMode(0011)
		if current == canonical {
			needed = 0044
			if executable {
				needed |= 0011
			}
		}
		if info.Mode().Perm()&needed != needed {
			if !native {
				return changes, failure("php_cli_access_denied", "External distribution paths must already support ordinary CLI users", 412)
			}
			changes = append(changes, phpCLIPermission{current, info.Mode().Perm()})
			if err := os.Chmod(current, info.Mode().Perm()|needed); err != nil {
				return changes, err
			}
		}
		if current == "/" {
			break
		}
	}
	return changes, nil
}
func trustedPHPCLIFile(path string, executable bool) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 || info.Mode().Perm()&0022 != 0 || !ok || stat.Uid != 0 || executable && info.Mode().Perm()&0111 == 0 {
		return failure("php_cli_ownership_invalid", "PHP CLI executable/modules must be regular root-owned files", 412)
	}
	return protectedParents(filepath.Dir(path))
}
func restorePHPCLIPermissions(changes []phpCLIPermission) {
	for i := len(changes) - 1; i >= 0; i-- {
		_ = os.Chmod(changes[i].Path, changes[i].Mode)
	}
}

// Read ELF dependency metadata instead of executing ldd or accepting caller
// search paths. Native libraries may be made readable only inside the trusted
// runtime prefix; distribution libraries are inspected without chmod changes.
func phpCLILibraryAccess(program, prefix string) ([]phpCLIPermission, error) {
	queue := []string{program}
	seen := map[string]bool{}
	changes := []phpCLIPermission{}
	triple := map[string]string{"amd64": "x86_64-linux-gnu", "arm64": "aarch64-linux-gnu"}[runtime.GOARCH]
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		canonical, err := filepath.EvalSymlinks(name)
		if err != nil {
			return changes, err
		}
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		if len(seen) > 128 {
			return changes, failure("php_cli_dependency_limit", "PHP CLI dependency graph exceeds its reviewed bound", 413)
		}
		binary, err := elf.Open(canonical)
		if err != nil {
			return changes, failure("php_cli_program_invalid", "PHP CLI and dynamic modules must be trusted ELF files", 412)
		}
		needed, readErr := binary.DynString(elf.DT_NEEDED)
		paths := []string{}
		for _, tag := range []elf.DynTag{elf.DT_RPATH, elf.DT_RUNPATH} {
			values, err := binary.DynString(tag)
			if err != nil && readErr == nil {
				readErr = err
			}
			for _, value := range values {
				for _, path := range strings.Split(value, ":") {
					path = strings.ReplaceAll(strings.ReplaceAll(path, "${ORIGIN}", filepath.Dir(canonical)), "$ORIGIN", filepath.Dir(canonical))
					if filepath.IsAbs(path) && !strings.Contains(path, "$") {
						paths = append(paths, filepath.Clean(path))
					}
				}
			}
		}
		for _, segment := range binary.Progs {
			if segment.Type != elf.PT_INTERP {
				continue
			}
			path, err := io.ReadAll(io.LimitReader(segment.Open(), 4097))
			if err != nil || len(path) > 4096 {
				readErr = errors.New("invalid ELF interpreter")
				break
			}
			loader := strings.TrimSuffix(string(path), "\x00")
			if !filepath.IsAbs(loader) || strings.ContainsAny(loader, "\x00\r\n") {
				readErr = errors.New("invalid ELF interpreter")
				break
			}
			permissions, err := phpCLIPublicAccess(loader, prefix, true)
			changes = append(changes, permissions...)
			if err != nil {
				_ = binary.Close()
				return changes, err
			}
		}
		_ = binary.Close()
		if readErr != nil {
			return changes, failure("php_cli_dependency_invalid", "PHP CLI ELF dependency metadata is invalid", 412)
		}
		paths = append(paths, "/lib/"+triple, "/usr/lib/"+triple, "/lib64", "/usr/lib64", "/lib", "/usr/lib")
		for _, dependency := range needed {
			if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]{0,127}$`).MatchString(dependency) {
				return changes, failure("php_cli_dependency_invalid", "PHP CLI dependencies must have bounded library basenames", 412)
			}
			found := ""
			for _, directory := range paths {
				candidate := filepath.Join(directory, dependency)
				if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
					found = candidate
					break
				}
			}
			if found == "" {
				return changes, failure("php_cli_dependency_unavailable", "A PHP CLI shared library is absent from trusted runtime/system locations", 412)
			}
			permissions, err := phpCLIPublicAccess(found, prefix, false)
			changes = append(changes, permissions...)
			if err != nil {
				return changes, err
			}
			queue = append(queue, found)
		}
	}
	return changes, nil
}

func (e *AgentExecutor) openPHPCLIDirectory(create bool) (*os.Root, error) {
	directory := filepath.Join(e.config.RootDir, ".cli")
	var owner phpCLIOwner
	err := rootJSON(e.state, phpCLIOwnerFile, &owner)
	if errors.Is(err, os.ErrNotExist) {
		if !create {
			return nil, os.ErrNotExist
		}
		if _, err := os.Lstat(directory); !os.IsNotExist(err) {
			return nil, failure("php_cli_ownership_conflict", "Existing CLI directory has no owned receipt and was preserved", 409)
		}
		if err := os.Mkdir(directory, 0755); err != nil {
			return nil, err
		}
		if err := os.Chmod(directory, 0755); err != nil {
			_ = os.Remove(directory)
			return nil, err
		}
		info, err := os.Lstat(directory)
		if err != nil {
			return nil, err
		}
		stat := info.Sys().(*syscall.Stat_t)
		owner = phpCLIOwner{directory, uint64(stat.Dev), stat.Ino}
		if err = atomicRoot(e.state, phpCLIOwnerFile, encode(owner), 0600); err != nil {
			_ = os.Remove(directory)
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if owner.Directory != directory || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0755 || !ok || stat.Uid != 0 || owner.Device != uint64(stat.Dev) || owner.Inode != stat.Ino {
		return nil, failure("php_cli_ownership_invalid", "Owned CLI directory identity or permissions changed", 409)
	}
	if err = protectedParents(directory); err != nil {
		return nil, err
	}
	return os.OpenRoot(directory)
}
func (e *AgentExecutor) phpCLISelection(root *os.Root) (phpCLISelection, error) {
	target, err := root.Readlink("current")
	if err != nil {
		return phpCLISelection{}, err
	}
	if !phpCLISelectionPath.MatchString(target) {
		return phpCLISelection{}, failure("php_cli_ownership_invalid", "CLI selector is not an owned generation", 409)
	}
	var selection phpCLISelection
	if err = rootJSON(e.state, filepath.Join("php-cli", target+".json"), &selection); err != nil {
		return selection, err
	}
	if selection.ID != filepath.Base(target) || !safeID.MatchString(selection.Instance) {
		return selection, failure("php_cli_ownership_invalid", "CLI selector receipt does not match", 409)
	}
	link, err := root.Readlink("powerlinux-php")
	if err != nil || link != "current/wrapper" {
		return selection, failure("php_cli_ownership_invalid", "Owned CLI entry point was replaced", 409)
	}
	for name, expected := range map[string]string{"wrapper": selection.WrapperSHA256, "php.ini": selection.IniSHA256} {
		data, err := phpCLIRead(filepath.Join(e.config.RootDir, ".cli", target, name), 256<<10)
		if err != nil {
			return selection, err
		}
		if phpCLIDigest(data) != expected {
			return selection, failure("php_cli_ownership_invalid", "Public CLI configuration changed outside its managed selector", 409)
		}
	}
	scan, err := root.Open(filepath.Join(target, "conf.d"))
	if err != nil {
		return selection, err
	}
	defer scan.Close()
	entries, err := scan.ReadDir(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return selection, err
	}
	if len(entries) != 0 {
		return selection, failure("php_cli_ownership_invalid", "Public CLI scan directory contains unmanaged configuration", 409)
	}
	return selection, nil
}
func (e *AgentExecutor) phpCLIGet(ctx context.Context) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	wrapper := filepath.Join(e.config.RootDir, ".cli", "powerlinux-php")
	root, err := e.openPHPCLIDirectory(false)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"available": false, "wrapper_path": wrapper}, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	selection, err := e.phpCLISelection(root)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"available": false, "wrapper_path": wrapper}, nil
	}
	if err != nil {
		return nil, err
	}
	if err = trustedPHPCLIFile(selection.Program, true); err != nil {
		return map[string]any{"available": false, "php_instance": selection.Instance, "wrapper_path": wrapper, "reason": "selected_runtime_unavailable"}, nil
	}
	currentRuntime, err := e.runtimes.PHP(ctx, selection.Instance)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return map[string]any{"available": false, "php_instance": selection.Instance, "wrapper_path": wrapper, "reason": "selected_runtime_unavailable"}, nil
	}
	currentProgram, err := filepath.EvalSymlinks(currentRuntime.CLIProgram)
	if err != nil || currentProgram != selection.Program {
		return map[string]any{"available": false, "php_instance": selection.Instance, "wrapper_path": wrapper, "reason": "selected_runtime_changed"}, nil
	}
	probe, err := phpCLIExec(ctx, wrapper, []string{"-d", "disable_functions=", "-r", phpCLIProbeCode}, "", true)
	if err != nil || !phpCLIModulesLoaded(probe, selection.Extensions) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return map[string]any{"available": false, "php_instance": selection.Instance, "wrapper_path": wrapper, "reason": "public_cli_probe_failed"}, nil
	}
	return map[string]any{"available": true, "php_instance": selection.Instance, "wrapper_path": wrapper, "program": selection.Program, "version": probe.Version, "extensions": probe.Modules}, nil
}
func (e *AgentExecutor) phpCLISet(ctx context.Context, id string) (any, error) {
	if !safeID.MatchString(id) {
		return nil, invalid("Invalid PHP instance")
	}
	runtime, err := e.runtimes.PHP(ctx, id)
	if err != nil {
		return nil, err
	}
	if runtime.ID != id || runtime.CLIProgram == "" || runtime.CLIConfig == "" || !filepath.IsAbs(runtime.Prefix) || filepath.Clean(runtime.Prefix) == "/" {
		return nil, failure("php_cli_unavailable", "Protected runtime receipt does not provide its matching PHP CLI", 412)
	}
	if err = protectedParents(runtime.Prefix); err != nil {
		return nil, err
	}
	program, err := filepath.EvalSymlinks(runtime.CLIProgram)
	if err != nil {
		return nil, err
	}
	if err = trustedPHPCLIFile(program, true); err != nil {
		return nil, err
	}
	if relative, err := filepath.Rel(e.config.StateDir, program); err == nil && relative != ".." && !strings.HasPrefix(relative, "../") {
		return nil, failure("php_cli_ownership_invalid", "A public CLI cannot reside in private Agent state", 412)
	}
	raw, err := phpCLIRead(runtime.CLIConfig, 256<<10)
	if err != nil {
		return nil, err
	}
	ini, modules, err := sanitizePHPCLIIni(raw)
	if err != nil {
		return nil, err
	}
	probe, err := phpCLIExec(ctx, program, []string{"-n", "-r", phpCLIProbeCode}, "", false)
	if err != nil {
		return nil, err
	}
	root, err := e.openPHPCLIDirectory(true)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	previous, selectionErr := e.phpCLISelection(root)
	if selectionErr != nil && !errors.Is(selectionErr, os.ErrNotExist) {
		return nil, selectionErr
	}
	if selectionErr == nil && previous.Instance == id && previous.Program == program && previous.IniSHA256 == phpCLIDigest(ini) {
		current, err := e.phpCLIGet(ctx)
		if err != nil {
			return nil, err
		}
		if current.(map[string]any)["available"] == true {
			return current, nil
		}
	}
	if existing, err := root.Open("selections"); err == nil {
		entries, readErr := existing.ReadDir(129)
		_ = existing.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		if len(entries) >= 128 {
			return nil, failure("php_cli_selection_limit", "Public CLI generation retention limit reached; older owned generations need maintenance", 409)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err = e.state.MkdirAll("php-cli/selections", 0700); err != nil {
		return nil, err
	}
	if err = root.MkdirAll("selections", 0755); err != nil {
		return nil, err
	}
	if err = root.Chmod("selections", 0755); err != nil {
		return nil, err
	}
	var entropy [16]byte
	if _, err = rand.Read(entropy[:]); err != nil {
		return nil, err
	}
	generation := hex.EncodeToString(entropy[:])
	relative := filepath.Join("selections", generation)
	absolute := filepath.Join(e.config.RootDir, ".cli", relative)
	if err = root.Mkdir(relative, 0755); err != nil {
		return nil, err
	}
	if err = root.Chmod(relative, 0755); err != nil {
		_ = root.Remove(relative)
		return nil, err
	}
	committed := false
	changes := []phpCLIPermission{}
	defer func() {
		if !committed {
			restorePHPCLIPermissions(changes)
			_ = root.RemoveAll(relative)
			_ = e.state.Remove(filepath.Join("php-cli", relative+".json"))
		}
	}()
	if err = root.Mkdir(filepath.Join(relative, "conf.d"), 0755); err != nil {
		return nil, err
	}
	if err = root.Chmod(filepath.Join(relative, "conf.d"), 0755); err != nil {
		return nil, err
	}
	wrapper := phpCLIWrapper(program, absolute)
	if err = atomicRoot(root, filepath.Join(relative, "php.ini"), ini, 0644); err != nil {
		return nil, err
	}
	if err = atomicRoot(root, filepath.Join(relative, "wrapper"), wrapper, 0755); err != nil {
		return nil, err
	}
	if err = root.Chmod(filepath.Join(relative, "php.ini"), 0644); err != nil {
		return nil, err
	}
	if err = root.Chmod(filepath.Join(relative, "wrapper"), 0755); err != nil {
		return nil, err
	}
	modifications, err := phpCLIPublicAccess(program, runtime.Prefix, true)
	changes = append(changes, modifications...)
	if err != nil {
		return nil, err
	}
	modifications, err = phpCLILibraryAccess(program, runtime.Prefix)
	changes = append(changes, modifications...)
	if err != nil {
		return nil, err
	}
	for _, module := range modules {
		modifications, err = phpCLIPublicAccess(filepath.Join(probe.ExtensionDir, module), runtime.Prefix, false)
		changes = append(changes, modifications...)
		if err != nil {
			return nil, err
		}
		modifications, err = phpCLILibraryAccess(filepath.Join(probe.ExtensionDir, module), runtime.Prefix)
		changes = append(changes, modifications...)
		if err != nil {
			return nil, err
		}
	}
	actual, err := phpCLIExec(ctx, filepath.Join(absolute, "wrapper"), []string{"-d", "disable_functions=", "-r", phpCLIProbeCode}, "", true)
	if err != nil {
		return nil, err
	}
	if !phpCLIModulesLoaded(actual, modules) {
		return nil, failure("php_cli_extension_failed", "Selected PHP extension did not load for an ordinary CLI user; previous selector was preserved", 502)
	}
	selection := phpCLISelection{generation, id, program, actual.Version, phpCLIDigest(wrapper), phpCLIDigest(ini), modules}
	if err = atomicRoot(e.state, filepath.Join("php-cli", relative+".json"), encode(selection), 0600); err != nil {
		return nil, err
	}
	if link, err := root.Readlink("powerlinux-php"); errors.Is(err, os.ErrNotExist) {
		if err = root.Symlink("current/wrapper", "powerlinux-php"); err != nil {
			return nil, err
		}
	} else if err != nil || link != "current/wrapper" {
		return nil, failure("php_cli_ownership_conflict", "Existing CLI entry point was preserved", 409)
	}
	temporary := ".current-" + generation
	if err = root.Symlink(relative, temporary); err != nil {
		return nil, err
	}
	defer root.Remove(temporary)
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = root.Rename(temporary, "current"); err != nil {
		return nil, err
	}
	committed = true
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	if err = directory.Sync(); err != nil {
		return nil, failure("php_cli_persistence_failed", "CLI selection switched but its directory could not be synced; inspect selection before retry", 500)
	}
	return map[string]any{"available": true, "php_instance": id, "wrapper_path": filepath.Join(e.config.RootDir, ".cli", "powerlinux-php"), "program": program, "version": actual.Version, "extensions": actual.Modules}, nil
}
