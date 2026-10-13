package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Explicit modes survive the root Agent's restrictive umask and repair owned
// paths created by the earlier lifecycle. Private Agent state is never passed
// here, nor made accessible to a runtime worker.
func (e *PrivilegedExecutor) prepareInstanceDirectories(paths Paths) error {
	relative, err := filepath.Rel(e.config.RootDir, paths.Root)
	parts := strings.Split(relative, string(filepath.Separator))
	if err != nil || !filepath.IsAbs(paths.Root) || !filepath.IsLocal(relative) || len(parts) != 2 || !validID.MatchString(parts[0]) || !validID.MatchString(parts[1]) || paths != e.instancePaths(parts[0], parts[1]) {
		return failure("ownership_invalid", "Runtime directories must match the exact trusted software layout", 412)
	}
	for _, dir := range []string{filepath.Dir(paths.Root), paths.Root, paths.ConfigDir, paths.BackupDir} {
		mode := os.FileMode(0750)
		if dir == paths.BackupDir {
			mode = 0700
		}
		if err := os.MkdirAll(dir, mode); err != nil {
			return err
		}
		if err := e.setRuntimeDirectory(dir, mode, uint32(os.Geteuid()), false); err != nil {
			return err
		}
	}
	return nil
}

func (e *PrivilegedExecutor) setRuntimeDirectory(path string, mode os.FileMode, uid uint32, worker bool) error {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return failure("ownership_invalid", "Managed runtime directory must be an owned real directory", 412)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0022 != 0 || (stat.Uid != uint32(os.Geteuid()) && !(worker && stat.Uid == e.uid)) {
		return failure("ownership_invalid", "Managed runtime directory ownership or permissions changed; it was preserved", 412)
	}
	if os.Geteuid() == 0 {
		if err := file.Chown(int(uid), int(e.gid)); err != nil {
			return err
		}
	}
	return file.Chmod(mode)
}
