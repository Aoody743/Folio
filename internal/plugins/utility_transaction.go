package plugins

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Aoody743/PowerLinux/internal/core"
)

type utilityFileSnapshot struct {
	Exists bool
	Body   []byte
	Mode   os.FileMode
	GID    int
}

type utilityDirectorySnapshot struct {
	Exists bool
	Mode   os.FileMode
	UID    int
	GID    int
}

func snapshotUtilityFile(base, name string) (utilityFileSnapshot, error) {
	if !filepath.IsLocal(name) {
		return utilityFileSnapshot{}, failure("invalid_path", "Utility state path must remain in its managed root", 400)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return utilityFileSnapshot{}, err
	}
	defer root.Close()
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		return utilityFileSnapshot{}, nil
	}
	if err != nil {
		return utilityFileSnapshot{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return utilityFileSnapshot{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Size() > 1<<20 || !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
		return utilityFileSnapshot{}, failure("ownership_invalid", "Utility configuration or private metadata is unsafe", 412)
	}
	body, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return utilityFileSnapshot{}, err
	}
	if len(body) > 1<<20 {
		return utilityFileSnapshot{}, failure("record_too_large", "Utility configuration exceeds one MiB", 413)
	}
	return utilityFileSnapshot{Exists: true, Body: body, Mode: info.Mode().Perm(), GID: int(stat.Gid)}, nil
}

func syncUtilityDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func restoreUtilityFile(base, name string, snapshot utilityFileSnapshot) error {
	if !filepath.IsLocal(name) {
		return failure("invalid_path", "Utility state path must remain in its managed root", 400)
	}
	if snapshot.Exists {
		if err := writeAtomicRoot(base, name, snapshot.Body, snapshot.Mode, snapshot.GID); err != nil {
			return err
		}
	} else {
		root, err := os.OpenRoot(base)
		if err != nil {
			return err
		}
		err = root.Remove(name)
		_ = root.Close()
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return syncUtilityDirectory(base)
}

func cloneUtilityRecord(record utilityRecord) utilityRecord {
	cloned := utilityRecord{Revision: record.Revision, Items: make(map[string]utilityAccount, len(record.Items))}
	for name, item := range record.Items {
		cloned.Items[name] = item
	}
	return cloned
}

func (e *PrivilegedExecutor) removeUtilityDataDirectory(instance Instance, relative string) error {
	if !filepath.IsLocal(relative) {
		return failure("invalid_path", "Utility data path must remain inside its managed instance", 400)
	}
	root, err := os.OpenRoot(instance.Paths.DataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err = root.Remove(relative); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// requireUtilityDataTargetAbsent prevents a newly created account/program from
// silently taking ownership of an orphaned or otherwise unknown data path.
// Existing entries are only accepted when the protected utility record already
// owns them and writeRsyncRecord/writeSupervisorRecord is rendering that record.
func inspectUtilityDataPath(root *os.Root, relative string) (bool, error) {
	if !filepath.IsLocal(relative) {
		return false, failure("invalid_path", "Utility data path must remain inside its managed instance", 400)
	}
	parts := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	current := ""
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, statErr := root.Lstat(current)
		if os.IsNotExist(statErr) {
			return false, nil
		}
		if statErr != nil {
			return false, statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false, failure("ownership_invalid", "Utility data path contains an unexpected symbolic link", 412)
		}
		if index == len(parts)-1 {
			if !info.IsDir() {
				return false, failure("ownership_invalid", "Utility data path is not a directory", 412)
			}
			return true, nil
		}
		if !info.IsDir() {
			return false, failure("ownership_invalid", "Utility data path parent is not a directory", 412)
		}
	}
	return false, nil
}

func snapshotUtilityDataDirectory(instance Instance, relative string) (utilityDirectorySnapshot, error) {
	root, err := os.OpenRoot(instance.Paths.DataDir)
	if err != nil {
		return utilityDirectorySnapshot{}, err
	}
	defer root.Close()
	exists, err := inspectUtilityDataPath(root, relative)
	if err != nil || !exists {
		return utilityDirectorySnapshot{Exists: exists}, err
	}
	directory, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return utilityDirectorySnapshot{}, failure("ownership_invalid", "Utility data directory cannot be safely inspected", 412)
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil {
		return utilityDirectorySnapshot{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || stat.Uid != uint32(os.Geteuid()) && os.Geteuid() != 0 || info.Mode().Perm()&0022 != 0 {
		return utilityDirectorySnapshot{}, failure("ownership_invalid", "Utility data directory ownership or permissions are unsafe", 412)
	}
	return utilityDirectorySnapshot{Exists: true, Mode: info.Mode().Perm(), UID: int(stat.Uid), GID: int(stat.Gid)}, nil
}

func restoreUtilityDataDirectory(instance Instance, relative string, snapshot utilityDirectorySnapshot) error {
	if !snapshot.Exists {
		return nil
	}
	root, err := os.OpenRoot(instance.Paths.DataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	if _, err = inspectUtilityDataPath(root, relative); err != nil {
		return err
	}
	directory, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return failure("ownership_invalid", "Utility data directory cannot be safely restored", 412)
	}
	defer directory.Close()
	if err = directory.Chown(snapshot.UID, snapshot.GID); err != nil {
		return err
	}
	return directory.Chmod(snapshot.Mode)
}

func requireUtilityDataTargetAbsent(instance Instance, relative string) error {
	root, err := os.OpenRoot(instance.Paths.DataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	exists, err := inspectUtilityDataPath(root, relative)
	if err != nil {
		return err
	}
	if exists {
		return failure("utility_data_path_conflict", "An unowned data directory already exists for this account or program", 409)
	}
	return nil
}

// commitUtilityMutation writes rendered files and applies them before the
// protected account/program record becomes visible. If any later step fails,
// restore the exact prior files and private record while the caller holds the
// shared per-instance lock.
func (e *PrivilegedExecutor) commitUtilityMutation(
	instance Instance,
	oldRecordSnapshot utilityFileSnapshot,
	nextRecord utilityRecord,
	configNames []string,
	writeCandidate func() error,
	applyCandidate func(utilityRecord) error,
	restoreRuntime func() error,
	cleanupDataDirectories []string,
) error {
	configSnapshots := make(map[string]utilityFileSnapshot, len(configNames))
	for _, name := range configNames {
		snapshot, err := snapshotUtilityFile(instance.Paths.ConfigDir, name)
		if err != nil {
			return err
		}
		configSnapshots[name] = snapshot
	}

	rollback := func(restoreRecord bool, restoreAppliedRuntime bool) error {
		var rollbackErrs []error
		for index := len(configNames) - 1; index >= 0; index-- {
			name := configNames[index]
			if err := restoreUtilityFile(instance.Paths.ConfigDir, name, configSnapshots[name]); err != nil {
				rollbackErrs = append(rollbackErrs, err)
			}
		}
		if restoreAppliedRuntime && restoreRuntime != nil {
			if err := restoreRuntime(); err != nil {
				rollbackErrs = append(rollbackErrs, err)
			}
		}
		if restoreRecord {
			if err := restoreUtilityFile(filepath.Join(e.config.StateDir, "private"), utilityStateID(instance)+".json", oldRecordSnapshot); err != nil {
				rollbackErrs = append(rollbackErrs, err)
			}
		}
		for _, path := range cleanupDataDirectories {
			if err := e.removeUtilityDataDirectory(instance, path); err != nil {
				rollbackErrs = append(rollbackErrs, err)
			}
		}
		return errors.Join(rollbackErrs...)
	}

	if err := writeCandidate(); err != nil {
		if rollbackErr := rollback(false, false); rollbackErr != nil {
			return failure("operation_recovery_required", "Utility configuration failed and could not be restored; inspect status before retrying", 500)
		}
		return err
	}
	if err := syncUtilityDirectory(instance.Paths.ConfigDir); err != nil {
		if rollbackErr := rollback(false, false); rollbackErr != nil {
			return failure("operation_recovery_required", "Utility configuration durability failed and could not be restored; inspect status before retrying", 500)
		}
		return err
	}
	if err := applyCandidate(nextRecord); err != nil {
		if rollbackErr := rollback(false, true); rollbackErr != nil {
			return failure("operation_recovery_required", "Utility change failed and could not be fully restored; inspect status before retrying", 500)
		}
		return err
	}
	var saveErr error
	if e.utilitySaveHook != nil {
		saveErr = e.utilitySaveHook(instance, nextRecord)
	} else {
		saveErr = e.save("private", utilityStateID(instance), nextRecord)
	}
	if saveErr != nil {
		if rollbackErr := rollback(true, true); rollbackErr != nil {
			return failure("operation_recovery_required", "Utility metadata commit failed and the prior configuration could not be fully restored; inspect status before retrying", 500)
		}
		return saveErr
	}
	return nil
}

func (e *PrivilegedExecutor) utilityRuntimeRunning(instance Instance) (bool, error) {
	_, err := ownedProcessID(instance)
	if err == nil {
		return true, nil
	}
	if core.ErrorCode(err) == "runtime_not_running" {
		if instance.Status == "running" {
			return false, failure("runtime_state_ambiguous", "Managed receipt reports a running utility but its owned process cannot be verified", 412)
		}
		return false, nil
	}
	return false, err
}
