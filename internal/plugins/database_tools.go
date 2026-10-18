package plugins

import (
	"context"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

var postgresUtilityVersion = regexp.MustCompile(`\bPostgreSQL\)?\s+([0-9]+)(?:\.[0-9]+|devel)\b`)
var mysqlDistributionVersion = regexp.MustCompile(`\bDistrib\s+([0-9]+\.[0-9]+)\.[0-9]+`)
var mariaFromVersion = regexp.MustCompile(`\bfrom\s+([0-9]+\.[0-9]+)\.[0-9]+-MariaDB\b`)
var mysqlUtilityVersion = regexp.MustCompile(`\bVer\s+([0-9]+\.[0-9]+)\.[0-9]+`)

// Oracle's signed Debian/Ubuntu packages put the server in sbin and clients
// in bin. Archive runtimes keep clients beside their owned server instead.
// Both paths still pass the root-ownership and matching-series checks.
func mysqlClientProgram(instance Instance, name string) string {
	if len(instance.Packages) > 0 && instance.Service.Program == "/usr/sbin/mysqld" {
		return filepath.Join("/usr/bin", name)
	}
	return filepath.Join(filepath.Dir(instance.Service.Program), name)
}

func databaseUtilitySeries(engine, output string) string {
	if engine == "postgresql" {
		if match := postgresUtilityVersion.FindStringSubmatch(output); len(match) == 2 {
			return match[1]
		}
		return ""
	}
	// MariaDB's client "Ver 15.1" is a client protocol label. Its actual
	// distribution version follows "Distrib 10.11.x", unlike MySQL's banner.
	if match := mysqlDistributionVersion.FindStringSubmatch(output); len(match) == 2 {
		return match[1]
	}
	// MariaDB 11.8 replaced "Ver <protocol> Distrib <server>" with
	// "from <server>-MariaDB, client <protocol>". Use only the server release;
	// accepting the client protocol label would defeat the series guard.
	if engine == "mariadb" {
		if match := mariaFromVersion.FindStringSubmatch(output); len(match) == 2 {
			return match[1]
		}
	}
	if match := mysqlUtilityVersion.FindStringSubmatch(output); len(match) == 2 {
		return match[1]
	}
	return ""
}

func (e *PrivilegedExecutor) validateDatabaseUtility(ctx context.Context, instance Instance, program string) error {
	for _, path := range []string{instance.Service.Program, program} {
		if err := trustedDatabaseProgram(path); err != nil {
			return err
		}
	}
	args := []string{"--version"}
	if instance.PluginID != "postgresql" {
		args = []string{"--no-defaults", "--version"}
	}
	server, err := runFixed(ctx, instance.Service.Program, args, 5*time.Second, e.credential(instance))
	if err != nil {
		return failure("runtime_unavailable", "Owned database server version could not be verified", 412)
	}
	utility, err := runFixed(ctx, program, args, 5*time.Second, e.credential(instance))
	if err != nil {
		return failure("runtime_unavailable", "Owned database utility version could not be verified", 412)
	}
	series := databaseUtilitySeries(instance.PluginID, server)
	if instance.PluginID == "postgresql" && len(instance.Packages) > 0 {
		major, err := reviewedPostgreSQLMajor(instance.Packages)
		if err != nil {
			return err
		}
		if series != strconv.Itoa(major) {
			return failure("database_tool_version_mismatch", "PostgreSQL runtime major does not match its reviewed signed package plan", 412)
		}
	}
	if series == "" || databaseUtilitySeries(instance.PluginID, utility) != series {
		return failure("database_tool_version_mismatch", "Database utility must match the owned server's PostgreSQL major or MySQL/MariaDB series", 412)
	}
	return nil
}

func (e *PrivilegedExecutor) validateDatabaseRuntimeTools(ctx context.Context, instance Instance) error {
	var programs []string
	switch instance.PluginID {
	case "postgresql":
		for _, name := range []string{"initdb", "pg_dump", "pg_restore"} {
			programs = append(programs, filepath.Join(filepath.Dir(instance.Service.Program), name))
		}
	case "mariadb":
		programs = []string{"/usr/bin/mariadb", "/usr/bin/mariadb-dump"}
	default:
		for _, name := range []string{"mysql", "mysqldump"} {
			programs = append(programs, mysqlClientProgram(instance, name))
		}
	}
	for _, program := range programs {
		if err := e.validateDatabaseUtility(ctx, instance, program); err != nil {
			return err
		}
	}
	return nil
}
