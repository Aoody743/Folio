package plugins

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func oldPHPProfileFixture(t *testing.T) (*PrivilegedExecutor, Instance) {
	t.Helper()
	e := testAgent(t)
	paths := e.instancePaths("php-apt", "profile")
	for _, path := range []string{paths.Root, paths.ConfigDir, paths.DataDir, paths.LogDir, paths.RunDir, filepath.Join(paths.ConfigDir, "pools"), filepath.Join(paths.ConfigDir, "conf.d")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	program := filepath.Join(paths.Root, "fixture-php-fpm")
	if err := os.WriteFile(program, []byte("controlled test executable fixture"), 0750); err != nil {
		t.Fatal(err)
	}
	instance := Instance{ID: "php-apt-profile", PluginID: "php-apt", InstanceID: "profile", SourceVersion: "legacy-83", Revision: 9, Status: "stopped", Ownership: "managed", Paths: paths, Config: defaultConfig("php-apt"), Service: ServiceProfile{ID: "php-apt-profile", Unit: "powerlinux-plugin-php-apt-profile.service", Program: program, Ownership: "managed"}}
	ini, err := renderPHPIni(instance.Config, filepath.Join(paths.DataDir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	fpm := "[global]\npid = " + filepath.Join(paths.RunDir, "server.pid") + "\nerror_log = " + filepath.Join(paths.LogDir, "fpm-error.log") + "\ninclude = " + filepath.Join(paths.ConfigDir, "pools", "*.conf") + "\n"
	for name, body := range map[string]string{"php.ini": ini, "php-fpm.conf": fpm, "pools/default.conf": renderDefaultPHPPool(instance, e.config.RunUser, e.config.RunGroup, false)} {
		if err := writeAtomicRoot(paths.ConfigDir, name, []byte(body), 0600, int(e.gid)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chown(filepath.Join(paths.ConfigDir, "conf.d"), os.Geteuid(), int(e.gid)); err != nil {
		t.Fatal(err)
	}
	if err := e.save("receipts", instance.ID, instance); err != nil {
		t.Fatal(err)
	}
	return e, instance
}

func TestLegacyPHPProfileMigrationExactFactoryKeepsStoppedAndRevision(t *testing.T) {
	e, instance := oldPHPProfileFixture(t)
	commands := []string{}
	e.profileControl = func(_ context.Context, _ Instance, command string) error {
		commands = append(commands, command)
		return nil
	}
	changed, err := e.migratePHPProfile(context.Background(), instance)
	if err != nil || !changed {
		t.Fatal("matching owned factory was not migrated", changed, err)
	}
	if strings.Join(commands, ",") != "check" {
		t.Fatal("stopped runtime was started or reloaded", commands)
	}
	var current Instance
	if err := e.load("receipts", instance.ID, &current); err != nil {
		t.Fatal(err)
	}
	if current.Revision != 9 || current.Status != "stopped" {
		t.Fatal("migration changed user-selected state or configuration revision", current)
	}
	pool, _ := os.ReadFile(filepath.Join(instance.Paths.ConfigDir, "pools/default.conf"))
	if !strings.Contains(string(pool), "ping.path = "+phpHealthPath) {
		t.Fatal("owned ping profile absent")
	}
	info, _ := os.Stat(filepath.Join(instance.Paths.ConfigDir, "php.ini"))
	if info.Mode().Perm() != 0640 {
		t.Fatal("CLI config not group-readable")
	}
	info, _ = os.Stat(filepath.Join(instance.Paths.ConfigDir, "conf.d"))
	if info.Mode().Perm() != 0750 {
		t.Fatal("CLI scan directory not traversable")
	}
}

func TestLegacyPHPProfileMigrationPreservesUnknownManualProfile(t *testing.T) {
	e, instance := oldPHPProfileFixture(t)
	path := filepath.Join(instance.Paths.ConfigDir, "pools/default.conf")
	body, _ := os.ReadFile(path)
	body = append(body, []byte("; manually customized pool\n")...)
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	e.profileControl = func(context.Context, Instance, string) error { t.Fatal("manual profile reached control"); return nil }
	changed, err := e.migratePHPProfile(context.Background(), instance)
	actual, _ := os.ReadFile(path)
	if err != nil || changed || string(actual) != string(body) {
		t.Fatal("manual profile was rewritten", changed, err)
	}
}

func TestLegacyPHPProfileMigrationFailureRestoresPriorBytesModesAndReceipt(t *testing.T) {
	e, instance := oldPHPProfileFixture(t)
	original, _, err := e.legacyPHPProfile(instance)
	if err != nil {
		t.Fatal(err)
	}
	e.profileControl = func(context.Context, Instance, string) error {
		return errors.New("controlled configuration check failure")
	}
	if changed, err := e.migratePHPProfile(context.Background(), instance); changed || err == nil {
		t.Fatal("failed profile claimed publication", changed, err)
	}
	for name, old := range original.Files {
		actual, err := readProfileFile(instance.Paths.ConfigDir, name)
		if err != nil || string(actual.Body) != string(old.Body) || actual.Mode != old.Mode || actual.GID != old.GID {
			t.Fatal("failure did not restore exact old profile", name, actual, err)
		}
	}
	var current Instance
	if err := e.load("receipts", instance.ID, &current); err != nil {
		t.Fatal(err)
	}
	if current.Revision != instance.Revision || current.Status != instance.Status {
		t.Fatal("failed migration published a new receipt")
	}
}
