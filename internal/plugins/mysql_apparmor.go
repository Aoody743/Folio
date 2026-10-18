package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const mysqlAppArmorLocal = "/etc/apparmor.d/local"
const mysqlAppArmorInclude = "#include if exists <local/folio-mysql.d>"

func mysqlAppArmorRules(instance Instance) ([]byte, error) {
	if !validInstanceID(instance.ID) || !filepath.IsAbs(instance.Paths.Root) {
		return nil, failure("apparmor_path_invalid", "MySQL AppArmor rules require an owned instance layout", 412)
	}
	var out strings.Builder
	out.WriteString("# Managed by Folio for " + instance.ID + "; retain with its database data.\n")
	for _, entry := range []struct{ path, access string }{
		{instance.Paths.ConfigDir, "r"}, {instance.Paths.DataDir, "rwk"},
		{instance.Paths.RunDir, "rwk"}, {instance.Paths.LogDir, "rw"},
	} {
		path := filepath.Clean(entry.path)
		if path != entry.path || !strings.HasPrefix(path, filepath.Clean(instance.Paths.Root)+"/") ||
			strings.ContainsAny(path, "*?[]{}^\"\\\n\r\x00") {
			return nil, failure("apparmor_path_invalid", "MySQL AppArmor rules must stay within literal owned directories", 412)
		}
		fmt.Fprintf(&out, "\"%s/\" r,\n\"%s/**\" %s,\n", path, path, entry.access)
	}
	return []byte(out.String()), nil
}

func protectedAppArmorDirectory(path string) error {
	if err := realDirectory(path); err != nil {
		return err
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
			return failure("apparmor_metadata_invalid", "AppArmor directories must be protected and root owned", 412)
		}
		if current == "/" {
			return nil
		}
	}
}

// Extend the vendor profile through its supported local include. Never disable
// confinement or overwrite vendor/user rules; each generated file grants only
// the exact configuration/data/run/log directories derived from a root receipt.
func (e *PrivilegedExecutor) prepareMySQLAppArmor(ctx context.Context, instance Instance) error {
	if instance.Service.Program != "/usr/sbin/mysqld" || len(instance.Packages) == 0 ||
		(instance.PluginID != "mysql" && instance.PluginID != "mysql-community") {
		return nil
	}
	enabled, err := os.ReadFile("/sys/module/apparmor/parameters/enabled")
	if os.IsNotExist(err) || err == nil && strings.TrimSpace(string(enabled)) != "Y" {
		return nil
	}
	if err != nil {
		return err
	}
	unlock, err := e.lockContext(ctx, "host-mysql-apparmor")
	if err != nil {
		return err
	}
	defer unlock()
	profile, err := readProfileFile("/etc/apparmor.d", "usr.sbin.mysqld")
	if os.IsNotExist(err) {
		return nil // No vendor profile is installed on this platform.
	}
	if err != nil {
		return err
	}
	if !bytes.Contains(profile.Body, []byte("#include <local/usr.sbin.mysqld>")) {
		return failure("apparmor_profile_unsupported", "MySQL profile lacks the vendor local override hook; inspect its policy before installation", 412)
	}
	parser, err := validateTrustedProgramPath("/usr/sbin/apparmor_parser")
	if err != nil {
		return failure("apparmor_parser_unavailable", "A trusted AppArmor parser is required for the managed MySQL directories", 412)
	}
	if err = protectedAppArmorDirectory(mysqlAppArmorLocal); err != nil {
		return err
	}
	rules, err := mysqlAppArmorRules(instance)
	if err != nil {
		return err
	}
	rulesDir := filepath.Join(mysqlAppArmorLocal, "folio-mysql.d")
	if err = os.Mkdir(rulesDir, 0755); err != nil && !os.IsExist(err) {
		return err
	}
	if err = protectedAppArmorDirectory(rulesDir); err != nil {
		return err
	}
	local, localErr := readProfileFile(mysqlAppArmorLocal, "usr.sbin.mysqld")
	if localErr != nil && !os.IsNotExist(localErr) {
		return localErr
	}
	previous, previousErr := readProfileFile(rulesDir, instance.ID)
	if previousErr != nil && !os.IsNotExist(previousErr) {
		return previousErr
	}
	body := append([]byte(nil), local.Body...)
	hasInclude := false
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == mysqlAppArmorInclude {
			hasInclude = true
		}
	}
	if !hasInclude {
		body = append(body, []byte("\n"+mysqlAppArmorInclude+"\n")...)
	}
	if len(body) > 128<<10 {
		return failure("apparmor_profile_unsupported", "Local MySQL AppArmor policy exceeds the supported bound", 412)
	}
	if err = writeAtomicRoot(rulesDir, instance.ID, rules, 0644, 0); err != nil {
		return err
	}
	restore := func() error {
		var result error
		if previousErr == nil {
			result = writeAtomicRoot(rulesDir, instance.ID, previous.Body, previous.Mode, previous.GID)
		} else {
			result = os.Remove(filepath.Join(rulesDir, instance.ID))
		}
		if localErr == nil {
			result = errors.Join(result, writeAtomicRoot(mysqlAppArmorLocal, "usr.sbin.mysqld", local.Body, local.Mode, local.GID))
		} else {
			if err := os.Remove(filepath.Join(mysqlAppArmorLocal, "usr.sbin.mysqld")); err != nil && !os.IsNotExist(err) {
				result = errors.Join(result, err)
			}
		}
		return result
	}
	mode, gid := os.FileMode(0644), 0
	if localErr == nil {
		mode, gid = local.Mode, local.GID
	}
	if err = writeAtomicRoot(mysqlAppArmorLocal, "usr.sbin.mysqld", body, mode, gid); err != nil {
		if restoreErr := restore(); restoreErr != nil {
			return failure("apparmor_recovery_required", "MySQL policy update failed and prior files could not all be restored; inspect local policy", 409)
		}
		return err
	}
	for step, args := range [][]string{{"-Q", "-T", "/etc/apparmor.d/usr.sbin.mysqld"}, {"-r", "-T", "/etc/apparmor.d/usr.sbin.mysqld"}} {
		if _, err = runFixed(ctx, parser, args, 30*time.Second, nil); err != nil {
			if restoreErr := restore(); restoreErr != nil {
				return failure("apparmor_recovery_required", "MySQL policy loading failed and prior files could not all be restored; inspect local policy", 409)
			}
			if step == 1 {
				recovery, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if _, reloadErr := runFixed(recovery, parser, args, 30*time.Second, nil); reloadErr != nil {
					return failure("apparmor_recovery_required", "Previous MySQL policy files were restored but kernel policy reload failed; inspect confinement", 409)
				}
			}
			return failure("apparmor_profile_failed", "Managed MySQL directory rules failed validation or loading; previous policy files were restored", 412)
		}
	}
	return nil
}
