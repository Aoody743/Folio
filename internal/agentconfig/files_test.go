package agentconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilePolicyRejectsUnprivilegedAuthorityWithoutSideEffects(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("ordinary-UID rejection is exercised in the cloud environment")
	}
	dir := t.TempDir()
	config := FileConfig{Version: 1, StateDir: filepath.Join(dir, "not-created"), Roots: []FileRoot{{ID: "fixture", Path: dir}}}
	if err := validateFiles(config); err == nil || !strings.Contains(err.Error(), "root-owned") {
		t.Fatalf("unprivileged authority accepted or rejected for an unrelated reason: %v", err)
	}
	if _, err := os.Stat(config.StateDir); !os.IsNotExist(err) {
		t.Fatal("validation created private state", err)
	}
}

func TestFilePolicyRequiresExplicitHostAccess(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("positive root-owned filesystem policy validation requires the QA root VM")
	}
	dir := t.TempDir()
	base := FileConfig{Version: 1, StateDir: filepath.Join(dir, "private-state"), APIDataDir: filepath.Join(dir, "api-data"), Roots: []FileRoot{{ID: "host", Path: "/", HostAccess: true}}}
	if err := validateFiles(base); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"unapproved-host", "missing-api", "root-api", "reserved-id", "symlink", "relative-private"} {
		t.Run(name, func(t *testing.T) {
			config := base
			config.Roots = append([]FileRoot{}, base.Roots...)
			switch name {
			case "unapproved-host":
				config.Roots[0].HostAccess = false
			case "missing-api":
				config.APIDataDir = ""
			case "root-api":
				config.APIDataDir = "/"
			case "reserved-id":
				config.Roots[0].ID = "workspace"
			case "symlink":
				link := filepath.Join(t.TempDir(), "root")
				if err := os.Symlink(dir, link); err != nil {
					t.Fatal(err)
				}
				config.Roots[0].Path = link
			case "relative-private":
				config.ProtectedPaths = []string{"relative"}
			}
			if err := validateFiles(config); err == nil {
				t.Fatal("invalid host file policy accepted", name)
			}
		})
	}
	if _, err := os.Stat(base.StateDir); !os.IsNotExist(err) {
		t.Fatal("validation created private state", err)
	}
	if err := os.Mkdir(base.StateDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := validateFiles(base); err == nil {
		t.Fatal("public filesystem authority state accepted")
	}
	if err := os.Chmod(base.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := validateFiles(base); err != nil {
		t.Fatal("private root-owned filesystem authority rejected", err)
	}
}

func TestFilePolicyOptionsProtectAPIAndModulePaths(t *testing.T) {
	dir := t.TempDir()
	base := FileConfig{Version: 1, StateDir: filepath.Join(dir, "private-state"), APIDataDir: filepath.Join(dir, "api-data"), Roots: []FileRoot{{ID: "host", Path: "/", HostAccess: true}}, ProtectedPaths: []string{filepath.Join(dir, "custom-private")}}
	options := base.Options([]string{filepath.Join(dir, "module-private")})
	for _, protected := range []string{"/etc/powerlinux", "/var/lib/powerlinux", "/var/lib/powerlinux-agent", base.APIDataDir, filepath.Join(dir, "module-private"), filepath.Join(dir, "custom-private")} {
		found := false
		for _, actual := range options.ProtectedPaths {
			if actual == protected {
				found = true
			}
		}
		if !found {
			t.Fatal("private path missing from root protection", protected)
		}
	}
	if !options.Roots[0].HostAccess || options.Roots[0].DefaultFileMode != 0600 || options.Roots[0].DefaultDirMode != 0700 {
		t.Fatal(options.Roots)
	}
}
