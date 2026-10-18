package plugins

import (
	"reflect"
	"testing"
)

func TestNewDistributionUnitIsolationPreservesPreexistingServiceIntent(t *testing.T) {
	for _, spec := range []struct {
		load, active, boot string
		isolate            bool
	}{
		{"not-found", "inactive", "", true},
		{"loaded", "inactive", "disabled", true},
		{"loaded", "failed", "disabled", true},
		{"loaded", "active", "disabled", false},
		{"loaded", "inactive", "enabled", false},
		{"loaded", "active", "enabled", false},
		{"loaded", "inactive", "masked", false},
		{"loaded", "inactive", "static", false},
	} {
		state := distributionNginxUnitState{LoadState: spec.load, ActiveState: spec.active, UnitFileState: spec.boot}
		if got := distributionUnitWasUnused(state); got != spec.isolate {
			t.Fatalf("isolation eligibility for %+v = %v, want %v", state, got, spec.isolate)
		}
	}
}

func TestDistributionRuntimeUnitsComeOnlyFromReviewedRuntimeNames(t *testing.T) {
	for _, spec := range []struct {
		plugin string
		pins   []PackagePin
		units  []string
	}{
		{"mysql", nil, []string{"mysql.service"}},
		{"postgresql", nil, []string{"postgresql.service"}},
		{"openresty", nil, []string{"openresty.service"}},
		{"php-apt", []PackagePin{{Name: "php8.3-cli"}, {Name: "php8.3-fpm"}}, []string{"php8.3-fpm.service"}},
		{"php-apt", []PackagePin{{Name: "../../sshd-fpm"}}, nil},
		{"php-apt", []PackagePin{{Name: "sshd"}}, nil},
		{"redis", []PackagePin{{Name: "mysql-community-server"}}, nil},
	} {
		if got := distributionRuntimeUnits(spec.plugin, spec.pins); !reflect.DeepEqual(got, spec.units) {
			t.Fatalf("vendor units for %s = %v, want %v", spec.plugin, got, spec.units)
		}
	}
}
