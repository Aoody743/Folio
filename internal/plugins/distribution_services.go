package plugins

import (
	"context"
	"regexp"
	"strings"
	"time"
)

var distributionFPMUnitPackage = regexp.MustCompile(`^php[0-9]+\.[0-9]+-fpm$`)

func distributionRuntimeUnits(plugin string, packages []PackagePin) []string {
	switch plugin {
	case "openresty":
		return []string{"openresty.service"}
	case "mysql", "mysql-community":
		return []string{"mysql.service"}
	case "postgresql":
		return []string{"postgresql.service"}
	case "php-apt":
		for _, pin := range packages {
			if distributionFPMUnitPackage.MatchString(pin.Name) {
				return []string{pin.Name + ".service"}
			}
		}
	}
	return nil
}

func distributionUnitState(ctx context.Context, unit string) (distributionNginxUnitState, error) {
	result, err := runFixedResult(ctx, "/usr/bin/systemctl", []string{"show", "--property=LoadState", "--property=ActiveState", "--property=UnitFileState", "--value", unit}, 15*time.Second, nil, nil)
	if err != nil || result.ExitCode != 0 || result.Truncated || result.Canceled || result.TimedOut || result.Stderr != "" {
		return distributionNginxUnitState{}, failure("distribution_service_state_unknown", "Could not verify a distribution service before package installation", 502)
	}
	state, err := parseDistributionNginxUnitState(result.Stdout)
	if err != nil {
		return state, failure("distribution_service_state_unknown", "Distribution service returned an unrecognized state", 502)
	}
	return state, nil
}

func distributionUnitWasUnused(state distributionNginxUnitState) bool {
	return state.LoadState == "not-found" || state.LoadState == "loaded" &&
		(state.ActiveState == "inactive" || state.ActiveState == "failed") && state.UnitFileState == "disabled"
}

func (e *PrivilegedExecutor) captureDistributionServices(ctx context.Context, plan Plan) (map[string]distributionNginxUnitState, error) {
	states := map[string]distributionNginxUnitState{}
	if !e.config.UseSystemd {
		return states, nil
	}
	for _, unit := range distributionRuntimeUnits(plan.PluginID, plan.Packages) {
		state, err := distributionUnitState(ctx, unit)
		if err != nil {
			return nil, err
		}
		states[unit] = state
	}
	return states, nil
}

// Maintainer scripts enable units even when policy-rc.d denies starting them.
// Restore the unused baseline for newly introduced vendor units before starting
// Folio's isolated runtime. Existing active/enabled services are never stopped.
func isolateNewDistributionServices(ctx context.Context, states map[string]distributionNginxUnitState) error {
	for unit, before := range states {
		if !distributionUnitWasUnused(before) {
			continue
		}
		after, err := distributionUnitState(ctx, unit)
		if err != nil {
			return err
		}
		if after.LoadState == "not-found" {
			continue
		}
		if _, err = runFixed(ctx, "/usr/bin/systemctl", []string{"disable", "--now", unit}, 30*time.Second, nil); err != nil {
			return failure("distribution_service_isolation_failed", "A newly installed distribution service could not be disabled", 502)
		}
		after, err = distributionUnitState(ctx, unit)
		if err != nil {
			return err
		}
		if after.ActiveState == "active" || strings.HasPrefix(after.UnitFileState, "enabled") {
			return failure("distribution_service_isolation_failed", "A newly installed distribution service remains active or enabled", 502)
		}
	}
	return nil
}
