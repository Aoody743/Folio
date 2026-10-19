package plugins

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestDatabaseWebToolConfigurationIsLoopbackOnly(t *testing.T) {
	for _, plugin := range []string{"phpmyadmin", "pgadmin"} {
		config := defaultConfig(plugin)
		if err := validateConfiguration(plugin, config); err != nil {
			t.Fatalf("default %s config rejected: %v", plugin, err)
		}
		for _, bind := range []string{"0.0.0.0", "192.0.2.5", "::"} {
			bad := cloneConfig(config)
			bad["bind"] = bind
			if err := validateConfiguration(plugin, bad); err == nil {
				t.Errorf("%s accepted non-loopback bind %q", plugin, bind)
			}
		}
	}
}

func TestPGAdminFirstInstallRequiresStrongAdministratorCredentials(t *testing.T) {
	valid := map[string]any{"admin_email": "admin@example.test", "admin_password": "correct horse battery staple"}
	if err := validatePGAdminInstallConfig(valid); err != nil {
		t.Fatal("valid initial administrator rejected", err)
	}
	for name, config := range map[string]map[string]any{
		"missing email":      {"admin_password": "correct horse battery staple"},
		"invalid email":      {"admin_email": "not-an-email", "admin_password": "correct horse battery staple"},
		"short password":     {"admin_email": "admin@example.test", "admin_password": "short"},
		"multiline password": {"admin_email": "admin@example.test", "admin_password": "a long secret\nline"},
	} {
		if err := validatePGAdminInstallConfig(config); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestDatabaseWebToolServiceArgsUseManagedLoopbackEndpoints(t *testing.T) {
	for _, test := range []struct {
		plugin string
		port   int
		arg    string
	}{
		{plugin: "phpmyadmin", port: 19050, arg: "phpmyadmin-router.php"},
		{plugin: "pgadmin", port: 19051, arg: "pgadmin-launch.py"},
	} {
		e := &PrivilegedExecutor{}
		instance := Instance{PluginID: test.plugin, Config: map[string]any{"bind": "127.0.0.1", "port": test.port}, Paths: Paths{Root: "/var/lib/folio/example", ConfigDir: "/var/lib/folio/example/config", DataDir: "/var/lib/folio/example/data"}}
		args, err := e.serviceArgs(instance, "start")
		if err != nil {
			t.Fatalf("%s service args: %v", test.plugin, err)
		}
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, test.arg) || (test.plugin == "phpmyadmin" && !strings.Contains(joined, "127.0.0.1:"+strconv.Itoa(test.port))) {
			t.Errorf("%s did not use its loopback listener and managed launcher: %v", test.plugin, args)
		}
	}
}

func TestDatabaseWebToolSystemdUnitsKeepApplicationCodeReadOnly(t *testing.T) {
	e := testAgent(t)
	for _, plugin := range []string{"phpmyadmin", "pgadmin"} {
		paths := e.instancePaths(plugin, "fixture")
		instance := Instance{ID: plugin + "-fixture", PluginID: plugin, InstanceID: "fixture", Paths: paths, Config: defaultConfig(plugin), Service: ServiceProfile{ID: plugin + "-fixture", PluginID: plugin, InstanceID: "fixture", Unit: "powerlinux-plugin-" + plugin + "-fixture.service", Program: "/usr/bin/python3"}}
		unit, err := systemdUnitBytes(e, instance, "")
		if err != nil {
			t.Fatalf("%s unit: %v", plugin, err)
		}
		text := string(unit)
		want := "ReadWritePaths=" + unitQuote(paths.DataDir) + " " + unitQuote(paths.RunDir) + " " + unitQuote(paths.LogDir) + " -/srv/powerlinux-sites"
		if !strings.Contains(text, want) || strings.Contains(text, "ReadWritePaths="+unitQuote(paths.Root)+" ") {
			t.Errorf("%s unit does not keep the app/config tree read-only: %s", plugin, text)
		}
		if plugin == "phpmyadmin" && !strings.Contains(text, "PHP_CLI_SERVER_WORKERS=4") {
			t.Errorf("phpMyAdmin service does not configure bounded built-in-server workers")
		}
	}
}

func TestDatabaseWebToolHealthRequiresExpectedApplicationMarker(t *testing.T) {
	for _, test := range []struct {
		plugin string
		body   string
		want   bool
	}{
		{plugin: "phpmyadmin", body: "<title>phpMyAdmin</title>", want: true},
		{plugin: "pgadmin", body: "pgAdmin 4", want: true},
		{plugin: "phpmyadmin", body: "generic web server", want: false},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(test.body)) }))
		err := probeManagedDatabaseWebTool(context.Background(), test.plugin, strings.TrimPrefix(server.URL, "http://"))
		server.Close()
		if (err == nil) != test.want {
			t.Errorf("%s marker check err=%v, want success %v", test.plugin, err, test.want)
		}
	}
}

func TestPublicSoftwarePlanAndInstanceRedactPGAdminPassword(t *testing.T) {
	secret := "private-pgadmin-bootstrap-password"
	plan := Plan{Config: map[string]any{"admin_email": "admin@example.test", "admin_password": secret}}
	publicPlan := publicSoftwarePlan(plan)
	instance := publicSoftwareInstance(Instance{Config: map[string]any{"admin_password": secret}})
	for _, result := range []any{publicPlan, instance} {
		body, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), secret) || strings.Contains(string(body), "admin_password") {
			t.Fatalf("public software response retained the one-time credential: %s", body)
		}
	}
	if plan.Config["admin_password"] != secret {
		t.Fatal("public response sanitization modified the internal plan")
	}
}

func TestAPTSourceSnapshotSupportsInternalLinksAndRejectsEscapes(t *testing.T) {
	aptRoot := t.TempDir()
	sourceDir := filepath.Join(aptRoot, "sources.list.d")
	if err := os.Mkdir(sourceDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aptRoot, "legacy.list"), []byte("deb https://mirror.example/debian stable main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(aptRoot, "legacy.list"), filepath.Join(sourceDir, "linked.list")); err != nil {
		t.Fatal(err)
	}
	legacy, _, err := readAPTSourceFiles(aptRoot)
	if err != nil || !strings.Contains(string(legacy), "mirror.example") {
		t.Fatalf("internal APT source link was not copied safely: %q %v", legacy, err)
	}
	outside := filepath.Join(t.TempDir(), "outside.list")
	if err := os.WriteFile(outside, []byte("deb https://attacker.example/debian stable main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(sourceDir, "outside.list")); err != nil {
		t.Fatal(err)
	}
	if _, _, err = readAPTSourceFiles(aptRoot); core.ErrorCode(err) != "repository_source_invalid" {
		t.Fatalf("APT source symlink outside config root was accepted: %v", err)
	}
}

func TestPHPMyAdminPlanningFailsBeforeMutationWithoutManagedDatabase(t *testing.T) {
	e := testAgent(t)
	e.config.UseSystemd = true
	_, err := e.plan(context.Background(), PlanInput{Operation: "install", PluginID: "phpmyadmin"})
	if core.ErrorCode(err) != "database_target_missing" {
		t.Fatalf("phpMyAdmin planning did not reject the missing database before package resolution: %v", err)
	}
}

func TestConfigurePHPMyAdminWritesManagedCookieAuthAndLoopbackTarget(t *testing.T) {
	e := testAgent(t)
	paths := e.instancePaths("phpmyadmin", "fixture")
	if err := os.MkdirAll(filepath.Join(paths.Root, "runtime"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Root, "runtime", "index.php"), []byte("<?php"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.ConfigDir, 0700); err != nil {
		t.Fatal(err)
	}
	db := Instance{ID: "mariadb-fixture", PluginID: "mariadb", InstanceID: "fixture", Status: "running", Config: map[string]any{"bind": "127.0.0.1", "port": 18308}, Paths: e.instancePaths("mariadb", "fixture")}
	db.Service = ServiceProfile{ID: db.ID, PluginID: db.PluginID, InstanceID: db.InstanceID, Unit: "powerlinux-plugin-" + db.ID + ".service"}
	if err := e.save("receipts", db.ID, db); err != nil {
		t.Fatal(err)
	}
	instance := Instance{PluginID: "phpmyadmin", Config: defaultConfig("phpmyadmin"), Paths: paths}
	if err := e.configurePHPMyAdmin(context.Background(), &instance); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(paths.Root, "runtime", "config.inc.php"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"auth_type'] = 'cookie'", "127.0.0.1", "18308", "AllowNoPassword'] = false", "AllowArbitraryServer'] = false"} {
		if !strings.Contains(string(config), expected) {
			t.Errorf("managed phpMyAdmin configuration is missing %q", expected)
		}
	}
	if strings.Contains(string(config), "password") {
		t.Fatal("phpMyAdmin configuration must never contain a database password")
	}
	router, err := os.ReadFile(filepath.Join(paths.ConfigDir, "phpmyadmin-router.php"))
	if err != nil || !strings.Contains(string(router), "setup(?:/|$)") || !strings.Contains(string(router), "rawurldecode") || !strings.Contains(string(router), "return true") {
		t.Fatalf("phpMyAdmin's setup console is not blocked by the managed router: %s %v", router, err)
	}
}

func TestPHPMyAdminPlanPinsItsManagedDatabaseResource(t *testing.T) {
	e := testAgent(t)
	e.config.UseSystemd = true
	db := Instance{ID: "mariadb-fixture", PluginID: "mariadb", InstanceID: "fixture", Status: "running", Config: map[string]any{"bind": "127.0.0.1", "port": 18308}, Paths: e.instancePaths("mariadb", "fixture")}
	db.Service = ServiceProfile{ID: db.ID, PluginID: db.PluginID, InstanceID: db.InstanceID, Unit: "powerlinux-plugin-" + db.ID + ".service"}
	if err := e.save("receipts", db.ID, db); err != nil {
		t.Fatal(err)
	}
	pma := Instance{ID: "phpmyadmin-fixture", PluginID: "phpmyadmin", InstanceID: "fixture", SourceVersion: "5.2.3", Status: "configured", Config: defaultConfig("phpmyadmin"), Paths: e.instancePaths("phpmyadmin", "fixture")}
	pma.Service = ServiceProfile{ID: pma.ID, PluginID: pma.PluginID, InstanceID: pma.InstanceID, Unit: "powerlinux-plugin-" + pma.ID + ".service", Program: "/usr/bin/php8.2"}
	if err := e.save("receipts", pma.ID, pma); err != nil {
		t.Fatal(err)
	}
	plan, err := e.plan(context.Background(), PlanInput{Operation: "config", PluginID: "phpmyadmin", InstanceID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Config["target_id"] != db.ID {
		t.Fatalf("implicit database target was not pinned into the plan: %+v", plan.Config)
	}
	want := InstanceResource(db.ID)
	found := false
	for _, resource := range plan.Resources {
		if resource == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("phpMyAdmin plan did not lock its selected database resource: %v", plan.Resources)
	}
}
