package plugins

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// Retained application data may only be reused through the same protected
// uninstall receipt. A directory without that exact receipt is never adopted.
func (e *PrivilegedExecutor) validateInstallTarget(ctx context.Context, current Instance, loadErr error, plugin, label string) error {
	manifest, err := e.catalog.Get(plugin)
	if err != nil {
		return err
	}
	if e.config.UseSystemd && len(manifest.Service.Programs) > 0 && manifest.Service.Health != "cli" {
		if err := e.checkSystemdIdentity(ctx, "powerlinux-plugin-"+plugin+"-"+label+".service", true); err != nil {
			return err
		}
	}
	paths := e.instancePaths(plugin, label)
	info, err := os.Lstat(paths.Root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
		return failure("ownership_conflict", "Installation target is not a protected Agent-owned directory", 409)
	}
	entries, err := os.ReadDir(paths.Root)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	if loadErr == nil && current.ID == plugin+"-"+label && current.PluginID == plugin && current.InstanceID == label && current.Status == "uninstalled" && current.Ownership == "managed" && current.Paths == paths {
		return nil
	}
	return failure("ownership_conflict", "Target directory contains files without its matching managed uninstall receipt; existing data was preserved", 409)
}

func reviewedPostgreSQLMajor(pins []PackagePin) (int, error) {
	major := 0
	serverName := regexp.MustCompile(`^postgresql-([1-9][0-9]{0,2})$`)
	leading := regexp.MustCompile(`^[1-9][0-9]{0,2}`)
	for _, pin := range pins {
		selected := 0
		if match := serverName.FindStringSubmatch(pin.Name); match != nil {
			selected, _ = strconv.Atoi(match[1])
		} else if pin.Name == "postgresql" || pin.Name == "postgresql-server" {
			version := pin.Version
			if _, after, ok := strings.Cut(version, ":"); ok {
				version = after
			}
			selected, _ = strconv.Atoi(leading.FindString(version))
		}
		if selected == 0 {
			continue
		}
		if major != 0 && major != selected {
			return 0, failure("package_plan_ambiguous", "Reviewed PostgreSQL pins select conflicting server majors", 412)
		}
		major = selected
	}
	if major == 0 {
		return 0, failure("package_plan_invalid", "An exact reviewed PostgreSQL server major is required", 412)
	}
	return major, nil
}

// Site snippets and per-site FPM pools belong to the sites module. Software
// uninstall must not erase its independent root receipts/configuration.
func (e *PrivilegedExecutor) requireRuntimeUnused(instance Instance) error {
	directory := ""
	if instance.PluginID == "openresty" {
		directory = filepath.Join(instance.Paths.ConfigDir, "sites")
	} else if isPHPRuntime(instance.PluginID) {
		directory = filepath.Join(instance.Paths.ConfigDir, "pools")
	}
	if directory == "" {
		return nil
	}
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".conf") && !(isPHPRuntime(instance.PluginID) && entry.Name() == "default.conf") {
			return failure("runtime_in_use", "Migrate or delete the dependent websites before uninstalling their Web/PHP runtime", 409)
		}
	}
	return nil
}

func samePackageRuntime(left, right []PackagePin) bool {
	if len(left) != len(right) {
		return false
	}
	for _, a := range left {
		found := false
		for _, b := range right {
			if a.Format == b.Format && a.Name == b.Name && a.Version == b.Version && a.Architecture == b.Architecture && a.SHA256 == b.SHA256 {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (e *PrivilegedExecutor) restoreOwnWebConfiguration(ctx context.Context, instance Instance, snapshot string) error {
	files := []string{"nginx.conf", "fastcgi_params"}
	private := "sites"
	if isPHPRuntime(instance.PluginID) {
		files = []string{"php.ini", "php-fpm.conf", "pools/default.conf"}
		private = "pools"
	}
	if err := os.MkdirAll(instance.Paths.ConfigDir, 0750); err != nil {
		return err
	}
	if err := realDirectory(instance.Paths.ConfigDir); err != nil {
		return err
	}
	if err := os.Chown(instance.Paths.ConfigDir, os.Geteuid(), int(e.gid)); err != nil {
		return err
	}
	if err := os.Chmod(instance.Paths.ConfigDir, 0750); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(instance.Paths.ConfigDir, private), 0700); err != nil {
		return err
	}
	if isPHPRuntime(instance.PluginID) {
		directory := filepath.Join(instance.Paths.ConfigDir, "conf.d")
		if err := os.MkdirAll(directory, 0750); err != nil {
			return err
		}
		if err := os.Chown(directory, os.Geteuid(), int(e.gid)); err != nil {
			return err
		}
		if err := os.Chmod(directory, 0750); err != nil {
			return err
		}
	}
	root, err := os.OpenRoot(snapshot)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, name := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || info.Size() > 4<<20 || !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
			file.Close()
			return failure("snapshot_metadata_invalid", "Previous owned Web/PHP configuration has unsafe metadata", 412)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, (4<<20)+1))
		closeErr := file.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(data) > 4<<20 {
			return failure("snapshot_metadata_invalid", "Previous configuration exceeded its size bound", 412)
		}
		if err := writeAtomicRoot(instance.Paths.ConfigDir, name, data, info.Mode().Perm(), int(e.gid)); err != nil {
			return err
		}
	}
	return nil
}
