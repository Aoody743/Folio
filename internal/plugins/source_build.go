package plugins

import (
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

var sourceBuildPath = regexp.MustCompile(`^/[A-Za-z0-9/_+.-]+$`)

func sourceUsesSystemPackages(source Source) bool {
	return source.Kind == "signed_package" || source.Kind == "verified_source" && len(source.Packages) > 0
}
func validateSourceBuildConfig(config PrivilegedConfig) error {
	if !config.EnableSourceBuild {
		return nil
	}
	if config.BuildUser == "" || config.BuildRootDir == "" || !sourceBuildPath.MatchString(config.BuildRootDir) || !sourceBuildPath.MatchString(config.RootDir) || filepath.Clean(config.BuildRootDir) != config.BuildRootDir || filepath.Clean(config.RootDir) != config.RootDir || config.BuildRootDir == "/" {
		return errors.New("source builds require a dedicated identity and simple absolute build/runtime paths")
	}
	for _, path := range []string{config.RootDir, config.StateDir} {
		relative, err := filepath.Rel(path, config.BuildRootDir)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, "../") {
			return errors.New("source build cache cannot reside in software or private Agent state")
		}
		relative, err = filepath.Rel(config.BuildRootDir, path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, "../") {
			return errors.New("source build cache cannot contain software/private Agent state")
		}
	}
	builder, err := user.Lookup(config.BuildUser)
	if err != nil {
		return errors.New("source build user does not exist")
	}
	forbiddenGIDs := map[string]bool{"0": true}
	for _, name := range []string{config.APIUser, config.RunUser} {
		account, err := user.Lookup(name)
		if err != nil || account.Uid == builder.Uid {
			return errors.New("source build UID must differ from API and runtime worker UIDs")
		}
		forbiddenGIDs[account.Gid] = true
	}
	if builder.Uid == "0" {
		return errors.New("source compiler cannot run as root")
	}
	buildGID := builder.Gid
	if config.BuildGroup != "" {
		group, lookupErr := user.LookupGroup(config.BuildGroup)
		if lookupErr != nil {
			return errors.New("source build group does not exist")
		}
		buildGID = group.Gid
	}
	if config.RunGroup != "" {
		group, err := user.LookupGroup(config.RunGroup)
		if err != nil {
			return errors.New("runtime group does not exist")
		}
		forbiddenGIDs[group.Gid] = true
	}
	if _, err := strconv.ParseUint(buildGID, 10, 32); err != nil || forbiddenGIDs[buildGID] {
		return errors.New("source build GID must be nonzero and distinct from API/runtime primary and configured groups")
	}
	for path := config.BuildRootDir; ; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0 {
				return errors.New("source build cache ancestors must be real root-controlled directories")
			}
		}
		if path == "/" {
			break
		}
	}
	return nil
}
func (e *PrivilegedExecutor) prepareSourceBuild() error {
	e.buildSlot = make(chan struct{}, 1)
	if !e.config.EnableSourceBuild {
		return nil
	}
	if os.Geteuid() != 0 {
		return errors.New("source build privilege separation requires a root Agent")
	}
	builder, err := user.Lookup(e.config.BuildUser)
	if err != nil {
		return err
	}
	uid, err := strconv.ParseUint(builder.Uid, 10, 32)
	if err != nil {
		return err
	}
	gid, err := strconv.ParseUint(builder.Gid, 10, 32)
	if err != nil {
		return err
	}
	if e.config.BuildGroup != "" {
		group, err := user.LookupGroup(e.config.BuildGroup)
		if err != nil {
			return err
		}
		gid, err = strconv.ParseUint(group.Gid, 10, 32)
		if err != nil {
			return err
		}
	}
	e.buildUID, e.buildGID = uint32(uid), uint32(gid)
	if err = os.MkdirAll(e.config.BuildRootDir, 0711); err != nil {
		return err
	}
	if err = os.Chown(e.config.BuildRootDir, 0, 0); err != nil {
		return err
	}
	return os.Chmod(e.config.BuildRootDir, 0711)
}
func sourceBuildIdentityTree(root string, uid, gid uint32) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return failure("source_build_unsafe", "Source builder permits only regular files and directories", 412)
		}
		mode := os.FileMode(0600)
		if info.IsDir() || info.Mode().Perm()&0111 != 0 {
			mode = 0700
		}
		if err = os.Chmod(path, mode); err != nil {
			return err
		}
		return os.Chown(path, int(uid), int(gid))
	})
}
func runSourceBuild(ctx context.Context, program string, args []string, work, home string, uid, gid uint32) error {
	operation := agent.Operation{Name: "plugin_source_build", Program: program, WorkDir: work, Timeout: 30 * time.Minute, Environment: []string{"HOME=" + home, "LC_ALL=C", "LANG=C", "MAKEFLAGS=", "MFLAGS="}, Credential: &syscall.Credential{Uid: uid, Gid: gid}, BuildArgs: func(json.RawMessage) ([]string, error) { return args, nil }}
	runner, err := agent.NewRunner(agent.Options{Operations: []agent.Operation{operation}, MaxConcurrent: 1, MaxOutputBytes: 1 << 20})
	if err != nil {
		return err
	}
	result, err := runner.Run(ctx, agent.Request{Operation: operation.Name, Input: []byte(`{}`)})
	if err != nil || result.ExitCode != 0 || result.Truncated {
		return failure("source_build_failed", "Fixed native source compilation failed; no generated runtime was promoted", 502)
	}
	return nil
}

// Build artifacts are copied to a new root-owned tree. Builder-owned inodes
// are never renamed into the root master runtime, even if a process retains an
// open descriptor. Native install links are flattened only within this prefix.
func copyNativeBuildTree(ctx context.Context, source, destination, finalPrefix string) error {
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	if err = os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	output, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer output.Close()
	var bytes int64
	count := 0
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		count++
		if count > maxArtifactFiles {
			return failure("source_build_limit", "Native runtime file count exceeds its limit", 413)
		}
		if entry.IsDir() {
			return output.MkdirAll(relative, 0750)
		}
		name := relative
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := root.Readlink(relative)
			if err != nil {
				return err
			}
			if filepath.IsAbs(link) {
				name, err = filepath.Rel(finalPrefix, link)
				if err != nil || name == ".." || strings.HasPrefix(name, "../") {
					return failure("source_build_unsafe", "Native install link escapes its fixed runtime prefix", 412)
				}
			} else {
				name = filepath.Clean(filepath.Join(filepath.Dir(relative), link))
			}
			if name == ".." || strings.HasPrefix(name, "../") {
				return failure("source_build_unsafe", "Native install link escapes its fixed runtime prefix", 412)
			}
		}
		file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 || info.Size() < 0 || bytes+info.Size() > maxExtractedBytes {
			return failure("source_build_unsafe", "Native runtime contains an unsafe or oversized file", 412)
		}
		bytes += info.Size()
		if err = output.MkdirAll(filepath.Dir(relative), 0750); err != nil {
			return err
		}
		mode := os.FileMode(0640)
		if info.Mode().Perm()&0111 != 0 {
			mode = 0750
		}
		copy, err := output.OpenFile(relative, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, err = io.CopyN(copy, &contextReader{ctx: ctx, reader: file}, info.Size())
		syncErr := copy.Sync()
		closeErr := copy.Close()
		return errors.Join(err, syncErr, closeErr)
	})
}
func (e *PrivilegedExecutor) buildSource(ctx context.Context, source Source, artifact string, paths Paths, manifest Manifest) (string, error) {
	if !e.config.EnableSourceBuild {
		return "", failure("source_build_disabled", "Trusted Agent policy must enable an isolated native source builder", 412)
	}
	select {
	case e.buildSlot <- struct{}{}:
		defer func() { <-e.buildSlot }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if source.BuildRecipe != "openresty" && source.BuildRecipe != "php-fpm" {
		return "", failure("source_recipe_unknown", "Native source recipe is not compiled into the Agent", 412)
	}
	build, err := os.MkdirTemp(e.config.BuildRootDir, ".build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(build)
	src := filepath.Join(build, "source")
	dest := filepath.Join(build, "destination")
	if err = extractArtifact(ctx, artifact, src, source); err != nil {
		return "", err
	}
	if err = os.Mkdir(dest, 0700); err != nil {
		return "", err
	}
	if err = sourceBuildIdentityTree(build, e.buildUID, e.buildGID); err != nil {
		return "", err
	}
	prefix := filepath.Join(paths.Root, "runtime")
	switch source.BuildRecipe {
	case "openresty":
		args := []string{filepath.Join(src, "configure"), "--prefix=" + prefix, "--with-http_ssl_module", "--with-http_v2_module", "--with-stream", "--with-stream_ssl_module", "--with-stream_ssl_preread_module", "--with-pcre-jit", "-j2"}
		if err = runSourceBuild(ctx, "/usr/bin/perl", args, src, build, e.buildUID, e.buildGID); err != nil {
			return "", err
		}
	case "php-fpm":
		args := []string{filepath.Join(src, "configure"), "--prefix=" + prefix, "--enable-fpm", "--with-fpm-user=" + e.config.RunUser, "--with-fpm-group=" + e.config.RunGroup, "--with-config-file-path=" + paths.ConfigDir, "--with-config-file-scan-dir=" + filepath.Join(paths.ConfigDir, "conf.d"), "--with-openssl", "--with-zlib", "--enable-mbstring", "--with-curl", "--with-pdo-mysql=mysqlnd", "--with-mysqli=mysqlnd", "--with-pdo-pgsql", "--with-pgsql", "--enable-bcmath", "--enable-intl", "--enable-soap", "--enable-sockets", "--with-zip", "--enable-opcache"}
		if err = runSourceBuild(ctx, "/bin/sh", args, src, build, e.buildUID, e.buildGID); err != nil {
			return "", err
		}
	}
	if err = runSourceBuild(ctx, "/usr/bin/make", []string{"-j2"}, src, build, e.buildUID, e.buildGID); err != nil {
		return "", err
	}
	if err = runSourceBuild(ctx, "/usr/bin/make", []string{"install", "DESTDIR=" + dest}, src, build, e.buildUID, e.buildGID); err != nil {
		return "", err
	}
	installed := filepath.Join(dest, strings.TrimPrefix(prefix, "/"))
	stage, err := os.MkdirTemp(paths.Root, ".native-")
	if err != nil {
		return "", err
	}
	completed := false
	defer func() {
		if !completed {
			_ = os.RemoveAll(stage)
		}
	}()
	if err = copyNativeBuildTree(ctx, installed, stage, prefix); err != nil {
		return "", err
	}
	if err = secureRuntimeTree(stage, e.gid); err != nil {
		return "", err
	}
	program, err := resolveArchiveProgram(stage, manifest)
	if err != nil {
		return "", err
	}
	binary, err := elf.Open(program)
	if err != nil {
		return "", failure("source_build_invalid", "Native compiler did not produce its expected ELF executable", 412)
	}
	_ = binary.Close()
	completed = true
	return stage, nil
}
