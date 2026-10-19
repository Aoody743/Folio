package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func hostedAppHasNodeBuildScript(project string) (bool, error) {
	if err := validateHostedAppProjectManifest(project, "nodejs"); err != nil {
		return false, err
	}
	body, err := os.ReadFile(filepath.Join(project, "package.json"))
	if err != nil {
		return false, failure("application_manifest_invalid", "Node.js package.json could not be read", 400)
	}
	var manifest hostedAppPackageManifest
	if json.Unmarshal(body, &manifest) != nil {
		return false, failure("application_manifest_invalid", "Node.js package.json is invalid JSON", 400)
	}
	return strings.TrimSpace(manifest.Scripts["build"]) != "", nil
}

func (e *PrivilegedExecutor) buildNodeHostedApp(ctx context.Context, receipt hostedAppReceipt, npm string, credential *syscall.Credential, environment []string) error {
	if err := validateHostedAppProjectManifest(receipt.ProjectDir, receipt.Runtime); err != nil {
		return err
	}
	random, err := randomPlanID()
	if err != nil {
		return err
	}
	stage := filepath.Join(receipt.Root, ".project.build-"+strings.TrimPrefix(random, "plan_"))
	if err = os.Mkdir(stage, 0700); err != nil {
		return err
	}
	stageExists := true
	defer func() {
		if stageExists {
			_ = os.RemoveAll(stage)
		}
	}()
	if err = os.Chown(stage, int(receipt.UID), int(receipt.GID)); err != nil {
		return err
	}
	if err = copyHostedAppProject(receipt.ProjectDir, stage, receipt.UID, receipt.GID); err != nil {
		return failure("application_build_failed", "Application source could not be safely staged for its isolated build", 412)
	}
	stageHome := filepath.Join(stage, ".home")
	if err = os.Mkdir(stageHome, 0700); err != nil {
		return err
	}
	if err = os.Chown(stageHome, int(receipt.UID), int(receipt.GID)); err != nil {
		return err
	}
	buildEnvironment := append([]string(nil), environment...)
	for i := range buildEnvironment {
		if strings.HasPrefix(buildEnvironment[i], "HOME=") {
			buildEnvironment[i] = "HOME=" + stageHome
		}
	}
	result, runErr := runFixedDirectoryResult(ctx, npm, stage, []string{"ci", "--no-audit", "--no-fund"}, 30*time.Minute, credential, buildEnvironment)
	if runErr != nil || result.ExitCode != 0 || result.Truncated || result.Canceled || result.TimedOut {
		return failure("application_build_failed", "Node.js locked dependency installation failed under its isolated system user; inspect the task output", 502)
	}
	buildScript, err := hostedAppHasNodeBuildScript(stage)
	if err != nil {
		return err
	}
	if receipt.Runtime == "nextjs" && !buildScript {
		return failure("application_build_script_required", "Next.js project must declare a non-empty scripts.build command", 400)
	}
	if buildScript {
		result, runErr = runFixedDirectoryResult(ctx, npm, stage, []string{"run", "build"}, 30*time.Minute, credential, buildEnvironment)
		if runErr != nil || result.ExitCode != 0 || result.Truncated || result.Canceled || result.TimedOut {
			return failure("application_build_failed", "Node.js production build failed under its isolated system user; inspect the task output", 502)
		}
	}
	if receipt.Runtime == "nextjs" {
		info, statErr := os.Lstat(filepath.Join(stage, ".next"))
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return failure("application_build_failed", "Next.js build did not create a real production output directory", 502)
		}
	} else if err = writeHostedAppBuildMarker(stage, receipt.UID, receipt.GID); err != nil {
		return err
	}
	return e.commitHostedAppBuild(receipt, stage, &stageExists)
}

func copyHostedAppProject(source, destination string, uid, gid uint32) error {
	entries := 0
	var total int64
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil || !filepath.IsLocal(relative) {
			return errors.New("application project path escaped its root")
		}
		if relative == ".home" || relative == "node_modules" || relative == ".next" || relative == ".next.previous" || relative == ".next.failed" || relative == ".powerlinux-node-built" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entries++
		if entries > maxHostedAppArchiveEntries {
			return errors.New("application source contains too many entries")
		}
		target := filepath.Join(destination, relative)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("application source contains a symbolic link")
		}
		if entry.IsDir() {
			if err = os.Mkdir(target, 0750); err != nil {
				return err
			}
			return os.Chown(target, int(uid), int(gid))
		}
		if !info.Mode().IsRegular() {
			return errors.New("application source contains a non-regular file")
		}
		if info.Size() < 0 || info.Size() > maxExtractedBytes || total+info.Size() > maxExtractedBytes {
			return errors.New("application source exceeds the build staging size bound")
		}
		total += info.Size()
		fd, openErr := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if openErr != nil {
			return openErr
		}
		input := os.NewFile(uintptr(fd), path)
		output, createErr := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if createErr != nil {
			_ = input.Close()
			return createErr
		}
		written, copyErr := io.CopyN(output, input, info.Size())
		if copyErr == nil && written != info.Size() {
			copyErr = io.ErrUnexpectedEOF
		}
		closeInputErr := input.Close()
		mode := os.FileMode(0640)
		if info.Mode()&0111 != 0 {
			mode = 0750
		}
		if copyErr == nil {
			copyErr = output.Chown(int(uid), int(gid))
		}
		if copyErr == nil {
			copyErr = output.Chmod(mode)
		}
		if copyErr == nil {
			copyErr = output.Sync()
		}
		closeOutputErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeInputErr != nil {
			return closeInputErr
		}
		return closeOutputErr
	})
}

func writeHostedAppBuildMarker(project string, uid, gid uint32) error {
	path := filepath.Join(project, ".powerlinux-node-built")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	if _, err = file.WriteString("built\n"); err == nil {
		err = file.Chown(int(uid), int(gid))
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (e *PrivilegedExecutor) commitHostedAppBuild(receipt hostedAppReceipt, stage string, stageExists *bool) error {
	previous := filepath.Join(receipt.Root, ".project.build.previous")
	if _, err := os.Lstat(previous); err == nil {
		if err = validateParkedHostedAppSource(previous); err != nil {
			return err
		}
		if err = os.RemoveAll(previous); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(receipt.ProjectDir, previous); err != nil {
		return err
	}
	if err := parkHostedAppSource(previous); err != nil {
		_ = os.Rename(previous, receipt.ProjectDir)
		return err
	}
	if err := os.Rename(stage, receipt.ProjectDir); err != nil {
		_ = restoreParkedHostedAppSource(previous, receipt.ProjectDir, receipt.UID, receipt.GID)
		return err
	}
	*stageExists = false
	if err := os.Chown(receipt.ProjectDir, int(receipt.UID), int(receipt.GID)); err != nil {
		_ = os.Rename(receipt.ProjectDir, stage)
		*stageExists = true
		_ = restoreParkedHostedAppSource(previous, receipt.ProjectDir, receipt.UID, receipt.GID)
		return err
	}
	if err := os.Chmod(receipt.ProjectDir, 0750); err != nil {
		_ = os.Rename(receipt.ProjectDir, stage)
		*stageExists = true
		_ = restoreParkedHostedAppSource(previous, receipt.ProjectDir, receipt.UID, receipt.GID)
		return err
	}
	return nil
}
