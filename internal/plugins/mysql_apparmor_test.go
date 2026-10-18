package plugins

import (
	"strings"
	"testing"
)

func mysqlAppArmorTestInstance() Instance {
	return Instance{ID: "mysql-qa", Paths: Paths{
		Root: "/opt/folio/mysql/qa", ConfigDir: "/opt/folio/mysql/qa/config",
		DataDir: "/opt/folio/mysql/qa/data", RunDir: "/opt/folio/mysql/qa/run", LogDir: "/opt/folio/mysql/qa/logs",
	}}
}

func TestMySQLAppArmorGrantsOnlyOwnedRuntimeDirectories(t *testing.T) {
	rules, err := mysqlAppArmorRules(mysqlAppArmorTestInstance())
	if err != nil {
		t.Fatal(err)
	}
	text := string(rules)
	if !strings.Contains(text, `"/opt/folio/mysql/qa/config/**" r,`) ||
		strings.Contains(text, `"/opt/folio/mysql/qa/config/**" rw`) ||
		!strings.Contains(text, `"/opt/folio/mysql/qa/data/**" rwk,`) ||
		!strings.Contains(text, `"/opt/folio/mysql/qa/run/**" rwk,`) ||
		!strings.Contains(text, `"/opt/folio/mysql/qa/logs/**" rw,`) {
		t.Fatal("runtime policy must separate read-only config from writable data/run/log paths")
	}
	for _, line := range strings.Split(text, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, `"/opt/folio/mysql/qa/`) {
			t.Fatalf("grant escaped the instance: %s", line)
		}
	}
}

func TestMySQLAppArmorRejectsEscapingAndPolicyInjectionPaths(t *testing.T) {
	for _, path := range []string{
		"/opt/folio/mysql/qa-other/data", "/var/lib/mysql", "/opt/folio/mysql/qa/data/../config",
		"/opt/folio/mysql/qa/*", "/opt/folio/mysql/qa/{data,config}", "/opt/folio/mysql/qa/[a-z]",
		"/opt/folio/mysql/qa/data\n/** rw,", "/opt/folio/mysql/qa/data\" rw,", "/opt/folio/mysql/qa/data\\*",
	} {
		instance := mysqlAppArmorTestInstance()
		instance.Paths.DataDir = path
		if _, err := mysqlAppArmorRules(instance); err == nil {
			t.Fatalf("unsafe policy path was accepted: %q", path)
		}
	}
}
