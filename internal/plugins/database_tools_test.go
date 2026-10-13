package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestDatabaseUtilitySeriesUsesEngineVersionRatherThanClientProtocolLabel(t *testing.T) {
	for _, spec := range []struct{ engine, banner, want string }{
		{"postgresql", "postgres (PostgreSQL) 15.18 (Debian 15.18-0+deb12u1)", "15"},
		{"postgresql", "pg_dump (PostgreSQL) 17.6", "17"},
		{"postgresql", "pg_restore (PostgreSQL) 18devel", "18"},
		{"mariadb", "mariadb-dump Ver 10.19 Distrib 10.11.14-MariaDB, for debian-linux-gnu", "10.11"},
		{"mariadb", "mariadb Ver 15.1 Distrib 10.11.14-MariaDB, for debian-linux-gnu", "10.11"},
		{"mariadb", "mariadbd Ver 10.11.14-MariaDB for debian-linux-gnu on x86_64", "10.11"},
		{"mariadb", "/usr/bin/mariadb from 11.8.6-MariaDB, client 15.2 for debian-linux-gnu (x86_64) using EditLine wrapper", "11.8"},
		{"mariadb", "/usr/bin/mariadb-dump from 11.8.6-MariaDB, client 10.19 for debian-linux-gnu (x86_64)", "11.8"},
		{"mariadb", "mariadb client 15.2 for debian-linux-gnu", ""},
		{"mysql", "mysqld Ver 8.4.6 for Linux on x86_64 (MySQL Community Server - GPL)", "8.4"},
		{"postgresql", "arbitrary unversioned executable", ""},
	} {
		if got := databaseUtilitySeries(spec.engine, spec.banner); got != spec.want {
			t.Fatalf("%s series extraction mismatch: %q", spec.engine, got)
		}
	}
}

func TestDatabaseToolMismatchRejectedBeforeDumpAndPasswordFileCleaned(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-owned executable boundary requires root")
	}
	e := testAgent(t)
	base := t.TempDir()
	if err := os.Chmod(filepath.Dir(base), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0755); err != nil {
		t.Fatal(err)
	}
	for name, banner := range map[string]string{"postgres": "postgres (PostgreSQL) 15.18", "pg_dump": "pg_dump (PostgreSQL) 16.9"} {
		// Actual child processes exercise the production Runner/version boundary.
		if err := os.WriteFile(filepath.Join(base, name), []byte("#!/bin/sh\nprintf '%s\\n' '"+banner+"'\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	instance := Instance{PluginID: "postgresql", Config: map[string]any{"bind": "127.0.0.1", "port": 15432}, Service: ServiceProfile{Program: filepath.Join(base, "postgres")}, Paths: Paths{RunDir: base}}
	record := managedDatabaseRecord{ManagedDatabase: ManagedDatabase{Database: "owned", Username: "owned_user"}, Credential: "private test credential must not be passed as an argument"}
	_, _, _, cleanup, err := e.databaseTool(context.Background(), instance, record, "backup")
	defer cleanup()
	if core.ErrorCode(err) != "database_tool_version_mismatch" {
		t.Fatal("mismatched dump utility was accepted", core.ErrorCode(err))
	}
	files, err := filepath.Glob(filepath.Join(base, ".database-client-*"))
	if err != nil || len(files) != 0 {
		t.Fatal("tool rejection retained the private password file")
	}
	if err = os.WriteFile(filepath.Join(base, "pg_dump"), []byte("#!/bin/sh\nprintf '%s\\n' 'pg_dump (PostgreSQL) 15.18'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	_, args, environment, cleanup, err := e.databaseTool(context.Background(), instance, record, "backup")
	defer cleanup()
	if err != nil {
		t.Fatal("matching dump utility was rejected", core.ErrorCode(err))
	}
	for _, values := range [][]string{args, environment} {
		for _, value := range values {
			if value == record.Credential {
				t.Fatal("database credential leaked into process arguments or environment")
			}
		}
	}
	instance.Packages = []PackagePin{{Name: "postgresql", Version: "16+262"}}
	_, _, _, cleanup, err = e.databaseTool(context.Background(), instance, record, "backup")
	defer cleanup()
	if core.ErrorCode(err) != "database_tool_version_mismatch" {
		t.Fatal("matching old server/dump binaries bypassed the reviewed signed package major", core.ErrorCode(err))
	}
}
