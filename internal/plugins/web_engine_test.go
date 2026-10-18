package plugins

import (
	"strings"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestNginxCatalogDeclaresSignedSupportedPackage(t *testing.T) {
	catalog, err := BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := catalog.Get("nginx")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Versions) != 1 || manifest.Versions[0].Source.Kind != "signed_package" || len(manifest.Versions[0].Source.Packages) != 1 || manifest.Versions[0].Source.Packages[0] != "nginx" {
		t.Fatalf("unexpected nginx source: %+v", manifest.Versions)
	}
	if manifest.Coexist {
		t.Fatal("Nginx must be marked as an exclusive managed Web stack")
	}
	for _, platform := range []Platform{
		{OS: "linux", Arch: "amd64", Distro: "debian", Release: "13"},
		{OS: "linux", Arch: "amd64", Distro: "ubuntu", Release: "24.04"},
	} {
		if !manifest.Supports(platform) {
			t.Errorf("Nginx does not support target platform %+v", platform)
		}
	}
	if manifest.Supports(Platform{OS: "linux", Arch: "amd64", Distro: "ubuntu", Release: "22.04"}) {
		t.Fatal("Nginx was enabled outside the reviewed first-release target")
	}
}

func TestParseDistributionNginxUnitState(t *testing.T) {
	tests := []struct {
		name      string
		output    string
		wantState distributionNginxUnitState
		wantError bool
	}{
		{name: "enabled but inactive", output: "loaded\ninactive\nenabled\n", wantState: distributionNginxUnitState{LoadState: "loaded", ActiveState: "inactive", UnitFileState: "enabled"}},
		{name: "already disabled", output: "loaded\nfailed\ndisabled\n", wantState: distributionNginxUnitState{LoadState: "loaded", ActiveState: "failed", UnitFileState: "disabled"}},
		{name: "package service missing", output: "not-found\ninactive\n", wantState: distributionNginxUnitState{LoadState: "not-found", ActiveState: "inactive"}},
		{name: "unknown active state", output: "loaded\nactivating\nenabled\n", wantError: true},
		{name: "unknown enablement state", output: "loaded\ninactive\nunknown\n", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseDistributionNginxUnitState(test.output)
			if test.wantError {
				if err == nil {
					t.Fatalf("accepted unexpected state %+v", got)
				}
				return
			}
			if err != nil || got != test.wantState {
				t.Fatalf("state = %+v, %v; want %+v", got, err, test.wantState)
			}
		})
	}
}

func TestManagedWebEnginesAreMutuallyExclusive(t *testing.T) {
	instances := []Instance{
		{ID: "openresty-default", PluginID: "openresty", Status: "running"},
	}
	if err := validateManagedWebEngineAvailable("nginx", instances); core.ErrorCode(err) != "web_engine_conflict" {
		t.Fatalf("installing Nginx beside OpenResty returned %v", err)
	}
	instances[0].Status = "uninstalled"
	if err := validateManagedWebEngineAvailable("nginx", instances); err != nil {
		t.Fatalf("uninstalled OpenResty blocked Nginx: %v", err)
	}
	if err := validateManagedWebEngineAvailable("openresty", instances); err != nil {
		t.Fatalf("same engine was blocked: %v", err)
	}
}

func TestNginxUsesIsolatedConfigAndReloadProfile(t *testing.T) {
	instance := Instance{
		ID: "nginx-default", PluginID: "nginx", InstanceID: "default",
		Config: defaultConfig("nginx"),
		Paths:  Paths{Root: "/opt/powerlinux-software/nginx/default", ConfigDir: "/opt/powerlinux-software/nginx/default/config", RunDir: "/opt/powerlinux-software/nginx/default/run", LogDir: "/opt/powerlinux-software/nginx/default/log"},
	}
	executor := &PrivilegedExecutor{config: PrivilegedConfig{RunUser: "powerlinux", RunGroup: "powerlinux"}}
	generation := strings.Repeat("a", 32)
	body := executor.renderManagedWebConfiguration(instance, generation)
	if strings.Contains(body, "lua") || !strings.Contains(body, "Folio Nginx") || !strings.Contains(body, "X-Folio-Runtime-Generation") || !strings.Contains(body, "127.0.0.1:18081") || !strings.Contains(body, "include /etc/nginx/mime.types") || !strings.Contains(body, `client_body_temp_path "/opt/powerlinux-software/nginx/default/run/client_body" 1 2`) {
		t.Fatalf("Nginx profile is not isolated or generation-verifiable:\n%s", body)
	}
	parsed, err := executor.webGeneration(instance, []byte(body))
	if err != nil || parsed != generation {
		t.Fatalf("Nginx profile generation = %q, %v", parsed, err)
	}
	args, err := executor.serviceArgs(instance, "check")
	if err != nil || len(args) != 5 || args[0] != "-p" || args[1] != "/etc/nginx/" || args[4] != "-t" {
		t.Fatalf("Nginx check command = %q, %v", args, err)
	}
}
