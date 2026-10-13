package plugins

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestPHPModuleOutputDoesNotTreatWarningsAsLoaded(t *testing.T) {
	output := "PHP Warning: Unable to load dynamic library 'xml.so'\n[PHP Modules]\nCore\nxml\n[Zend Modules]\nZend OPcache\nZend OPcache\n"
	items := parsePHPModules(output)
	if !slices.Equal(items, []string{"Core", "Zend OPcache", "xml"}) || !phpModuleSet(output)["opcache"] {
		t.Fatalf("module state is not actual bounded names: %#v", items)
	}
}

func TestPHPBuiltinDisableCannotCommitFalseState(t *testing.T) {
	e := testAgent(t)
	paths := e.instancePaths("php", "builtin")
	bin := filepath.Join(paths.Root, "runtime", "bin")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	// Controlled process fixture supplies -n's compile-time modules. It never
	// stands in for native PHP lifecycle or external acceptance evidence.
	if err := os.WriteFile(filepath.Join(bin, "php"), []byte("#!/bin/sh\nprintf '[PHP Modules]\\nCore\\nxml\\n'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	instance := Instance{ID: "php-builtin", PluginID: "php", Paths: paths, Revision: 4,
		Service: ServiceProfile{Program: filepath.Join(bin, "php-fpm")}, Extensions: map[string]bool{"xml": true}}
	if _, err := e.togglePHPExtension(context.Background(), instance, PHPInput{Extension: "xml", Enabled: false, ExpectedRevision: 4}); core.ErrorCode(err) != "extension_builtin" {
		t.Fatalf("compiled-in module accepted disabled state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.ConfigDir, "php.ini")); !os.IsNotExist(err) {
		t.Fatal("built-in refusal mutated configuration", err)
	}
	if !instance.Extensions["xml"] || instance.Revision != 4 {
		t.Fatal("built-in refusal changed prior selection")
	}
}
