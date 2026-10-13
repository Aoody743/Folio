package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestPostgreSQLProgramMajorComesFromReviewedPins(t *testing.T) {
	for _, test := range []struct {
		pins []PackagePin
		want int
		code string
	}{
		{pins: []PackagePin{{Name: "postgresql", Version: "17+269"}, {Name: "postgresql-17", Version: "17.11-1"}}, want: 17},
		{pins: []PackagePin{{Name: "postgresql-server", Version: "1:16.4-2"}}, want: 16},
		{pins: []PackagePin{{Name: "postgresql-client-15", Version: "15.8-1"}}, code: "package_plan_invalid"},
		{pins: []PackagePin{{Name: "postgresql", Version: "17+269"}, {Name: "postgresql-15", Version: "15.8-1"}}, code: "package_plan_ambiguous"},
	} {
		major, err := reviewedPostgreSQLMajor(test.pins)
		if major != test.want || (test.code == "" && err != nil) || (test.code != "" && core.ErrorCode(err) != test.code) {
			t.Fatal("server selection differed from the reviewed package major", test.pins, major, err)
		}
	}
}

func TestRuntimeUninstallRefusesIndependentWebAndPHPPools(t *testing.T) {
	e := testAgent(t)
	for _, plugin := range []string{"openresty", "php-apt"} {
		instance := Instance{PluginID: plugin, Paths: e.instancePaths(plugin, "dependency")}
		subdir := "sites"
		if isPHPRuntime(plugin) {
			subdir = "pools"
		}
		directory := filepath.Join(instance.Paths.ConfigDir, subdir)
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		if isPHPRuntime(plugin) {
			if err := os.WriteFile(filepath.Join(directory, "default.conf"), []byte("owned default"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := e.requireRuntimeUnused(instance); err != nil {
			t.Fatal("unused runtime was refused", err)
		}
		if err := os.WriteFile(filepath.Join(directory, "independent-site.conf"), []byte("owned by sites"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := e.requireRuntimeUnused(instance); core.ErrorCode(err) != "runtime_in_use" {
			t.Fatal("uninstall would erase an independent site configuration", err)
		}
	}
}

func TestOwnRuntimeRollbackPreservesIndependentSiteConfigurations(t *testing.T) {
	e := testAgent(t)
	for _, plugin := range []string{"openresty", "php-apt"} {
		instance := Instance{PluginID: plugin, Paths: e.instancePaths(plugin, "restore")}
		snapshot := t.TempDir()
		files := []string{"nginx.conf", "fastcgi_params"}
		subdir := "sites"
		if isPHPRuntime(plugin) {
			files = []string{"php.ini", "php-fpm.conf", "pools/default.conf"}
			subdir = "pools"
		}
		for _, name := range files {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(snapshot, name)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := writeAtomicRoot(snapshot, name, []byte("previous owned "+name), 0640, int(e.gid)); err != nil {
				t.Fatal(err)
			}
		}
		independent := filepath.Join(instance.Paths.ConfigDir, subdir, "new-site.conf")
		if err := os.MkdirAll(filepath.Dir(independent), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(independent, []byte("later independent site remains"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := e.restoreOwnWebConfiguration(context.Background(), instance, snapshot); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(independent)
		if err != nil || string(body) != "later independent site remains" {
			t.Fatal("software rollback changed independent site content", err)
		}
		for _, name := range files {
			body, err := os.ReadFile(filepath.Join(instance.Paths.ConfigDir, name))
			if err != nil || string(body) != "previous owned "+name {
				t.Fatal("own main config was not restored", name, err)
			}
		}
	}
}
