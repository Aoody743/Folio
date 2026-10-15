package plugins

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	supervisorManagedDirMode = uint32(0750)
	supervisorWorkerDirMode  = uint32(0700)
)

// prepareSupervisorProgramDirectory opens the managed instance layout without
// following child symlinks, validates the worker-owned runtime paths, and
// repairs only the known root-created programs ancestor. The service worker
// owns its private data tree; this directory must therefore be worker-owned
// and private as well.
func (e *PrivilegedExecutor) prepareSupervisorProgramDirectory(instance Instance, name string) (string, error) {
	if instance.PluginID != "supervisor" || !validInstanceID(instance.InstanceID) || !utilityName.MatchString(name) || instance.Paths != e.instancePaths("supervisor", instance.InstanceID) {
		return "", failure("ownership_invalid", "Supervisor program paths do not match the protected managed instance", 412)
	}
	data, err := e.openSupervisorDataDirectory(instance)
	if err != nil {
		return "", err
	}
	defer data.Close()

	programs, err := openSupervisorDirectoryAt(data, "programs")
	if errors.Is(err, unix.ENOENT) {
		if mkdirErr := unix.Mkdirat(int(data.Fd()), "programs", supervisorWorkerDirMode); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
			return "", failure("ownership_invalid", "Supervisor program directory could not be created safely", 412)
		}
		programs, err = openSupervisorDirectoryAt(data, "programs")
	}
	if err != nil {
		return "", failure("ownership_invalid", "Supervisor program directory must be a real managed directory", 412)
	}
	defer programs.Close()
	if err = e.repairSupervisorProgramsDirectory(programs); err != nil {
		return "", err
	}

	leaf, err := openSupervisorDirectoryAt(programs, name)
	createdLeaf := false
	if errors.Is(err, unix.ENOENT) {
		mkdirErr := unix.Mkdirat(int(programs.Fd()), name, supervisorWorkerDirMode)
		if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
			return "", failure("ownership_invalid", "Supervisor program working directory could not be created safely", 412)
		}
		createdLeaf = mkdirErr == nil
		leaf, err = openSupervisorDirectoryAt(programs, name)
	}
	if err != nil {
		return "", failure("ownership_invalid", "Supervisor program working directory must be a real directory", 412)
	}
	defer leaf.Close()
	if createdLeaf && os.Geteuid() == 0 {
		info, statErr := leaf.Stat()
		stat, ok := (*syscall.Stat_t)(nil), false
		if statErr == nil {
			stat, ok = info.Sys().(*syscall.Stat_t)
		}
		if statErr != nil || !ok || !info.IsDir() || stat.Mode&syscall.S_IFMT != syscall.S_IFDIR || stat.Uid != 0 || (stat.Gid != 0 && stat.Gid != e.gid) || stat.Mode&0022 != 0 {
			return "", failure("ownership_invalid", "New Supervisor program working directory changed ownership or permissions", 412)
		}
		if err = leaf.Chown(int(e.uid), int(e.gid)); err != nil {
			return "", failure("ownership_invalid", "Supervisor program working directory ownership could not be assigned safely", 412)
		}
	}
	if createdLeaf {
		if err = leaf.Chmod(os.FileMode(supervisorWorkerDirMode)); err != nil {
			return "", failure("ownership_invalid", "Supervisor program working directory permissions could not be restricted", 412)
		}
	}
	if err = e.repairSupervisorProgramLeaf(leaf); err != nil {
		return "", err
	}
	return filepath.Join(instance.Paths.DataDir, "programs", name), nil
}

func (e *PrivilegedExecutor) openSupervisorDataDirectory(instance Instance) (*os.File, error) {
	softwareRoot, err := openSupervisorDirectory(e.config.RootDir)
	if err != nil {
		return nil, failure("ownership_invalid", "Supervisor software root must be a protected real directory", 412)
	}
	defer softwareRoot.Close()
	if !supervisorDirectoryTraversable(softwareRoot, e.uid, e.gid) {
		return nil, failure("ownership_invalid", "Supervisor software root is not traversable by its worker", 412)
	}

	pluginRoot, err := openSupervisorDirectoryAt(softwareRoot, "supervisor")
	if err != nil {
		return nil, failure("ownership_invalid", "Supervisor plugin root must be a protected real directory", 412)
	}
	defer pluginRoot.Close()
	if !supervisorDirectoryIdentity(pluginRoot, uint32(os.Geteuid()), e.gid, supervisorManagedDirMode) {
		return nil, failure("ownership_invalid", "Supervisor plugin root ownership or permissions changed", 412)
	}

	instanceRoot, err := openSupervisorDirectoryAt(pluginRoot, instance.InstanceID)
	if err != nil {
		return nil, failure("ownership_invalid", "Supervisor instance path is unavailable or unsafe", 412)
	}
	defer instanceRoot.Close()
	if !supervisorDirectoryIdentity(instanceRoot, uint32(os.Geteuid()), e.gid, supervisorManagedDirMode) {
		return nil, failure("ownership_invalid", "Supervisor instance path ownership or permissions changed", 412)
	}

	config, err := openSupervisorDirectoryAt(instanceRoot, "config")
	if err != nil {
		return nil, failure("ownership_invalid", "Supervisor configuration path is unavailable or unsafe", 412)
	}
	defer config.Close()
	if !supervisorDirectoryIdentity(config, uint32(os.Geteuid()), e.gid, supervisorManagedDirMode) {
		return nil, failure("ownership_invalid", "Supervisor configuration path ownership or permissions changed", 412)
	}

	for _, name := range []string{"run", "logs"} {
		workerDir, openErr := openSupervisorDirectoryAt(instanceRoot, name)
		if openErr != nil {
			return nil, failure("ownership_invalid", "Supervisor worker runtime paths are unavailable or unsafe", 412)
		}
		valid := supervisorDirectoryIdentity(workerDir, e.uid, e.gid, supervisorWorkerDirMode)
		_ = workerDir.Close()
		if !valid {
			return nil, failure("ownership_invalid", "Supervisor worker runtime path ownership or permissions changed", 412)
		}
	}

	data, err := openSupervisorDirectoryAt(instanceRoot, "data")
	if err != nil || !supervisorDirectoryIdentity(data, e.uid, e.gid, supervisorWorkerDirMode) {
		if data != nil {
			_ = data.Close()
		}
		return nil, failure("ownership_invalid", "Supervisor data path ownership or permissions changed", 412)
	}
	return data, nil
}

func (e *PrivilegedExecutor) repairSupervisorProgramsDirectory(directory *os.File) error {
	info, err := directory.Stat()
	if err != nil {
		return failure("ownership_invalid", "Supervisor program directory could not be inspected", 412)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || stat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return failure("ownership_invalid", "Supervisor program directory is not a regular directory", 412)
	}
	mode := stat.Mode & 07777
	repairable, needsChown := supervisorProgramsDirectoryState(stat.Uid, stat.Gid, mode, e.uid, e.gid)
	if !repairable || needsChown && os.Geteuid() != 0 {
		return failure("ownership_invalid", "Supervisor program directory ownership or permissions are not a recognized managed state", 412)
	}
	if needsChown {
		if err = directory.Chown(int(e.uid), int(e.gid)); err != nil {
			return failure("ownership_invalid", "Supervisor program directory ownership could not be repaired safely", 412)
		}
	}
	if mode != supervisorWorkerDirMode {
		if err = directory.Chmod(os.FileMode(supervisorWorkerDirMode)); err != nil {
			return failure("ownership_invalid", "Supervisor program directory permissions could not be repaired safely", 412)
		}
	}
	return nil
}

func supervisorProgramsDirectoryState(uid, gid, mode, workerUID, workerGID uint32) (repairable, needsChown bool) {
	mode &= 07777
	if (mode == supervisorWorkerDirMode || mode == 0750) && uid == workerUID && gid == workerGID {
		return true, false
	}
	if (mode == supervisorWorkerDirMode || mode == 0750) && uid == 0 && (gid == 0 || gid == workerGID) {
		return true, true
	}
	return false, false
}

func (e *PrivilegedExecutor) repairSupervisorProgramLeaf(directory *os.File) error {
	info, err := directory.Stat()
	if err != nil {
		return failure("ownership_invalid", "Supervisor program working directory could not be inspected", 412)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || stat.Mode&syscall.S_IFMT != syscall.S_IFDIR || stat.Uid != e.uid || stat.Gid != e.gid || (stat.Mode&07777 != supervisorWorkerDirMode && stat.Mode&07777 != 0750) {
		return failure("ownership_invalid", "Supervisor program working directory ownership or permissions changed", 412)
	}
	if stat.Mode&07777 != supervisorWorkerDirMode {
		if err = directory.Chmod(os.FileMode(supervisorWorkerDirMode)); err != nil {
			return failure("ownership_invalid", "Supervisor program working directory permissions could not be restricted", 412)
		}
	}
	return nil
}

func openSupervisorDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Base(path)), nil
}

func openSupervisorDirectoryAt(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func supervisorDirectoryIdentity(directory *os.File, uid, gid uint32, mode uint32) bool {
	info, err := directory.Stat()
	if err != nil || !info.IsDir() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Mode&syscall.S_IFMT == syscall.S_IFDIR && stat.Uid == uid && stat.Gid == gid && stat.Mode&07777 == mode
}

func supervisorDirectoryTraversable(directory *os.File, uid, gid uint32) bool {
	info, err := directory.Stat()
	if err != nil || !info.IsDir() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Mode&syscall.S_IFMT != syscall.S_IFDIR || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0022 != 0 {
		return false
	}
	if stat.Uid == uid {
		return stat.Mode&0100 != 0
	}
	if stat.Gid == gid {
		return stat.Mode&0010 != 0
	}
	return stat.Mode&0001 != 0
}
