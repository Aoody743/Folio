package plugins

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type distributionNginxUnitState struct {
	LoadState     string
	ActiveState   string
	UnitFileState string
}

func parseDistributionNginxUnitState(output string) (distributionNginxUnitState, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 2 && strings.TrimSpace(lines[0]) == "not-found" {
		lines = append(lines, "")
	}
	if len(lines) != 3 {
		return distributionNginxUnitState{}, fmt.Errorf("unexpected systemd nginx unit state")
	}
	state := distributionNginxUnitState{
		LoadState:     strings.TrimSpace(lines[0]),
		ActiveState:   strings.TrimSpace(lines[1]),
		UnitFileState: strings.TrimSpace(lines[2]),
	}
	if state.LoadState != "loaded" && state.LoadState != "not-found" {
		return distributionNginxUnitState{}, fmt.Errorf("unrecognized systemd nginx load state")
	}
	if state.LoadState == "not-found" {
		if state.ActiveState != "inactive" || state.UnitFileState != "" {
			return distributionNginxUnitState{}, fmt.Errorf("inconsistent missing systemd nginx unit state")
		}
		return state, nil
	}
	if state.ActiveState != "active" && state.ActiveState != "inactive" && state.ActiveState != "failed" {
		return distributionNginxUnitState{}, fmt.Errorf("unrecognized systemd nginx active state")
	}
	switch state.UnitFileState {
	case "enabled", "enabled-runtime", "disabled", "static", "indirect", "masked", "masked-runtime", "generated", "transient", "linked", "linked-runtime", "alias":
	default:
		return distributionNginxUnitState{}, fmt.Errorf("unrecognized systemd nginx enablement state")
	}
	return state, nil
}

func distributionNginxUnitStateRead(ctx context.Context) (distributionNginxUnitState, error) {
	result, err := runFixedResult(ctx, "/usr/bin/systemctl", []string{"show", "--property=LoadState", "--property=ActiveState", "--property=UnitFileState", "--value", "nginx.service"}, 15*time.Second, nil, nil)
	if err != nil || result.ExitCode != 0 || result.Truncated || result.Canceled || result.TimedOut || result.Stderr != "" {
		return distributionNginxUnitState{}, failure("nginx_service_state_unknown", "The distribution Nginx service state could not be verified", 502)
	}
	state, err := parseDistributionNginxUnitState(result.Stdout)
	if err != nil {
		return distributionNginxUnitState{}, failure("nginx_service_state_unknown", "The distribution Nginx service state could not be verified", 502)
	}
	return state, nil
}

// The signed distro package ships nginx.service with the host's default
// configuration. PowerLinux runs the same native binary under its own unit
// and isolated config tree, so keep the package-provided unit from claiming
// public listeners on a fresh panel host.
func (e *PrivilegedExecutor) isolateDistributionNginxService(ctx context.Context) error {
	if !e.config.UseSystemd {
		return failure("service_manager_required", "The managed Nginx stack requires systemd to isolate its distribution service", 412)
	}
	state, err := distributionNginxUnitStateRead(ctx)
	if err != nil {
		return err
	}
	if state.LoadState == "not-found" {
		return nil
	}
	if _, err = runFixed(ctx, "/usr/bin/systemctl", []string{"disable", "--now", "nginx.service"}, 30*time.Second, nil); err != nil {
		return failure("nginx_service_isolation_failed", "The distribution nginx.service could not be disabled; inspect its state before retrying", 502)
	}
	state, err = distributionNginxUnitStateRead(ctx)
	if err != nil {
		return err
	}
	if state.LoadState != "loaded" || state.ActiveState == "active" || state.UnitFileState == "enabled" || state.UnitFileState == "enabled-runtime" {
		return failure("nginx_service_isolation_failed", "The distribution nginx.service is still active or enabled; the managed Nginx stack was not started", 502)
	}
	return nil
}
