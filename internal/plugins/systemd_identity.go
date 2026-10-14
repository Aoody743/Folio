package plugins

import (
	"context"
	"path/filepath"
	"strings"
	"time"
)

// A matching unit file is insufficient: systemd can load another fragment or
// merge external drop-ins. Verify the manager's effective identity as well.
func validateSystemdIdentity(output, expectedPath string, absent bool) error {
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || (key != "LoadState" && key != "FragmentPath" && key != "DropInPaths" && key != "NeedDaemonReload") {
			return failure("ownership_conflict", "The effective systemd unit identity could not be verified; it was preserved", 409)
		}
		if _, exists := fields[key]; exists {
			return failure("ownership_conflict", "The effective systemd unit identity is ambiguous; it was preserved", 409)
		}
		fields[key] = value
	}
	if len(fields) != 4 {
		return failure("ownership_conflict", "The effective systemd unit identity is incomplete; it was preserved", 409)
	}
	if absent && fields["LoadState"] == "not-found" && fields["FragmentPath"] == "" && fields["DropInPaths"] == "" && fields["NeedDaemonReload"] == "no" {
		return nil
	}
	if !absent && fields["LoadState"] == "loaded" && fields["FragmentPath"] == expectedPath && fields["DropInPaths"] == "" && fields["NeedDaemonReload"] == "no" {
		return nil
	}
	return failure("ownership_conflict", "An existing or externally modified systemd unit is not owned by this operation; it was preserved", 409)
}

func (e *PrivilegedExecutor) checkSystemdIdentity(ctx context.Context, unit string, absent bool) error {
	// Alternate unit directories are private Go-only filesystem fixtures. The
	// production constructor fixes this to /etc/systemd/system, never API input.
	if !e.config.UseSystemd || e.systemdUnitDir() != "/etc/systemd/system" {
		return nil
	}
	output, err := runFixed(ctx, "/usr/bin/systemctl", []string{"show", "--property=LoadState,FragmentPath,DropInPaths,NeedDaemonReload", "--", unit}, 15*time.Second, nil)
	if err != nil {
		return failure("ownership_conflict", "The effective systemd unit identity could not be read; no service operation was taken", 409)
	}
	return validateSystemdIdentity(output, filepath.Join(e.systemdUnitDir(), unit), absent)
}
