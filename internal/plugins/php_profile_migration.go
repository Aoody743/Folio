package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

type oldProfileFile struct {
	Body []byte      `json:"body"`
	Mode os.FileMode `json:"mode"`
	GID  int         `json:"gid"`
}
type profileMigration struct {
	State    string                    `json:"state"`
	Instance Instance                  `json:"instance"`
	Files    map[string]oldProfileFile `json:"files"`
	ScanMode os.FileMode               `json:"scan_mode"`
	Unit     *oldProfileFile           `json:"unit,omitempty"`
	Running  bool                      `json:"running"`
}

func readProfileFile(base, name string) (oldProfileFile, error) {
	root, err := os.OpenRoot(base)
	if err != nil {
		return oldProfileFile{}, err
	}
	defer root.Close()
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return oldProfileFile{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return oldProfileFile{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Size() > 128<<10 || !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
		return oldProfileFile{}, failure("profile_metadata_invalid", "Owned profile metadata is unsafe", 412)
	}
	body, err := io.ReadAll(io.LimitReader(file, (128<<10)+1))
	if err != nil {
		return oldProfileFile{}, err
	}
	return oldProfileFile{Body: body, Mode: info.Mode().Perm(), GID: int(stat.Gid)}, nil
}

func (e *PrivilegedExecutor) legacyPHPProfile(instance Instance) (profileMigration, bool, error) {
	old := profileMigration{State: "prepared", Instance: instance, Files: map[string]oldProfileFile{}}
	if !isPHPRuntime(instance.PluginID) || instance.Status == "uninstalled" || instance.Service.Program == "" || instance.Ownership != "managed" || !ownedUnit(instance) || instance.Paths != e.instancePaths(instance.PluginID, instance.InstanceID) {
		return old, false, nil
	}
	if e.config.UseSystemd {
		if err := checkOwnedUnit(instance); err != nil {
			return old, false, nil
		}
	}
	program, err := os.Stat(instance.Service.Program)
	if err != nil || !program.Mode().IsRegular() || program.Mode().Perm()&0022 != 0 {
		return old, false, nil
	}
	if stat, ok := program.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return old, false, nil
	}
	for _, name := range []string{"php.ini", "php-fpm.conf", "pools/default.conf"} {
		file, err := readProfileFile(instance.Paths.ConfigDir, name)
		if err != nil {
			return old, false, nil
		}
		if file.GID != int(e.gid) || (file.Mode != 0600 && file.Mode != 0640) {
			return old, false, nil
		}
		old.Files[name] = file
	}
	group := e.config.RunGroup
	if group == "" {
		group = e.config.RunUser
	}
	if string(old.Files["pools/default.conf"].Body) != renderDefaultPHPPool(instance, e.config.RunUser, group, false) {
		return old, false, nil
	}
	ini, err := renderPHPIni(instance.Config, filepath.Join(instance.Paths.DataDir, "sessions"))
	if err != nil {
		return old, false, err
	}
	modules, err := renderPHPExtensions(instance.Extensions)
	if err != nil {
		return old, false, err
	}
	fpm := "[global]\npid = " + filepath.Join(instance.Paths.RunDir, "server.pid") + "\nerror_log = " + filepath.Join(instance.Paths.LogDir, "fpm-error.log") + "\ninclude = " + filepath.Join(instance.Paths.ConfigDir, "pools", "*.conf") + "\n"
	if string(old.Files["php.ini"].Body) != ini+modules || string(old.Files["php-fpm.conf"].Body) != fpm {
		return old, false, nil
	}
	scan, err := os.Lstat(filepath.Join(instance.Paths.ConfigDir, "conf.d"))
	if err != nil {
		return old, false, nil
	}
	stat, ok := scan.Sys().(*syscall.Stat_t)
	if !ok || !scan.IsDir() || stat.Uid != uint32(os.Geteuid()) || stat.Gid != e.gid || (scan.Mode().Perm() != 0700 && scan.Mode().Perm() != 0750) {
		return old, false, nil
	}
	old.ScanMode = scan.Mode().Perm()
	_, runningErr := ownedProcessID(instance)
	old.Running = runningErr == nil
	if e.config.UseSystemd {
		unit, err := readProfileFile("/etc/systemd/system", instance.Service.Unit)
		if err != nil {
			return old, false, err
		}
		old.Unit = &unit
	}
	return old, true, nil
}

func (e *PrivilegedExecutor) profileCommand(ctx context.Context, instance Instance, command string) error {
	if e.profileControl != nil {
		return e.profileControl(ctx, instance, command)
	}
	_, err := e.controlLocked(ctx, ServiceInput{ID: instance.ID, Command: command}, instance)
	return err
}

func (e *PrivilegedExecutor) restoreLegacyPHPProfile(ctx context.Context, old profileMigration) error {
	var errs []error
	for _, name := range []string{"php.ini", "php-fpm.conf", "pools/default.conf"} {
		file := old.Files[name]
		errs = append(errs, writeAtomicRoot(old.Instance.Paths.ConfigDir, name, file.Body, file.Mode, file.GID))
	}
	errs = append(errs, os.Chmod(filepath.Join(old.Instance.Paths.ConfigDir, "conf.d"), old.ScanMode))
	if old.Unit != nil {
		errs = append(errs, writeAtomicRoot("/etc/systemd/system", old.Instance.Service.Unit, old.Unit.Body, old.Unit.Mode, old.Unit.GID))
		_, err := runFixed(ctx, "/usr/bin/systemctl", []string{"daemon-reload"}, 20*time.Second, nil)
		errs = append(errs, err)
	}
	if old.Running {
		if e.profileControl != nil {
			errs = append(errs, e.profileControl(ctx, old.Instance, "restore_reload"))
		} else if e.config.UseSystemd {
			_, err := runFixed(ctx, "/usr/bin/systemctl", []string{"reload", old.Instance.Service.Unit}, 45*time.Second, nil)
			errs = append(errs, err)
		} else {
			errs = append(errs, signalOwnedProcess(old.Instance, syscall.SIGUSR2))
		}
	}
	errs = append(errs, e.save("receipts", old.Instance.ID, old.Instance))
	return errors.Join(errs...)
}

func (e *PrivilegedExecutor) migratePHPProfile(ctx context.Context, instance Instance) (bool, error) {
	unlock, err := e.lockContext(ctx, instance.ID)
	if err != nil {
		return false, err
	}
	defer unlock()
	key := "profile_" + instance.ID
	var prior profileMigration
	if err := e.load("private", key, &prior); err == nil && prior.State == "prepared" {
		return false, failure("profile_recovery_required", "Interrupted PHP profile migration requires protected recovery inspection", 409)
	} else if err != nil && core.ErrorCode(err) != "not_found" {
		return false, err
	}
	old, match, err := e.legacyPHPProfile(instance)
	if err != nil || !match {
		return false, err
	}
	if err := e.save("private", key, old); err != nil {
		return false, err
	}
	current := instance
	manifest, err := e.catalog.Get(instance.PluginID)
	if err == nil {
		err = e.configure(ctx, &current, manifest)
	}
	if err == nil && e.config.UseSystemd {
		var body []byte
		body, err = os.ReadFile(filepath.Join("/etc/systemd/system", current.Service.Unit))
		if err == nil {
			hash := sha256.Sum256(body)
			current.Service.UnitSHA256 = hex.EncodeToString(hash[:])
		}
	}
	if err == nil {
		err = e.profileCommand(ctx, current, "check")
	}
	if err == nil && old.Running {
		err = e.profileCommand(ctx, current, "reload")
	}
	if err == nil {
		current.UpdatedAt = time.Now().UTC()
		if old.Running {
			current.Health = e.probe(ctx, current)
		}
		err = e.save("receipts", current.ID, current)
	}
	if err != nil {
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if restoreErr := e.restoreLegacyPHPProfile(recovery, old); restoreErr != nil {
			return false, failure("profile_recovery_required", "PHP profile migration failed and its owned prior state needs protected recovery inspection", 409)
		}
		old.State = "rolled_back"
		if saveErr := e.save("private", key, old); saveErr != nil {
			return false, saveErr
		}
		return false, err
	}
	old.State = "completed"
	if err := e.save("private", key, old); err != nil {
		return false, err
	}
	return true, nil
}

func (e *PrivilegedExecutor) migratePHPProfiles(ctx context.Context) error {
	entries, err := os.ReadDir(filepath.Join(e.config.StateDir, "receipts"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || !validInstanceID(id) {
			continue
		}
		var instance Instance
		if err := e.load("receipts", id, &instance); err != nil {
			return err
		}
		if isPHPRuntime(instance.PluginID) {
			if _, err := e.migratePHPProfile(ctx, instance); err != nil {
				return err
			}
		}
	}
	return nil
}
