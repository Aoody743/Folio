package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeBuildCopyFlattensOnlyOwnedLinksAndUsesNewInodes(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "runtime")
	prefix := "/opt/powerlinux-software/openresty/test/runtime"
	if err := os.MkdirAll(filepath.Join(source, "nginx", "sbin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(source, "nginx", "sbin", "nginx")
	if err := os.WriteFile(original, []byte("native-produced-by-dedicated-builder"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(prefix, "nginx", "sbin", "nginx"), filepath.Join(source, "bin", "openresty")); err != nil {
		t.Fatal(err)
	}
	if err := copyNativeBuildTree(context.Background(), source, destination, prefix); err != nil {
		t.Fatal(err)
	}
	entry, err := os.Lstat(filepath.Join(destination, "bin", "openresty"))
	if err != nil || !entry.Mode().IsRegular() {
		t.Fatal("Promoted runtime still uses a builder-controlled link", err)
	}
	if err = os.WriteFile(original, []byte("changed-through-builder-descriptor"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(destination, "bin", "openresty"), filepath.Join(destination, "nginx", "sbin", "nginx")} {
		body, err := os.ReadFile(path)
		if err != nil || string(body) != "native-produced-by-dedicated-builder" {
			t.Fatal("Builder inode remained attached to promoted root runtime", path, err)
		}
	}
	for _, target := range []string{"/etc/shadow", "../../outside"} {
		root := t.TempDir()
		if err = os.Symlink(target, filepath.Join(root, "escape")); err != nil {
			t.Fatal(err)
		}
		if err = copyNativeBuildTree(context.Background(), root, t.TempDir(), prefix); err == nil {
			t.Fatal("Native build link escaped fixed runtime prefix", target)
		}
	}
}
func TestSourceBuilderValidationRejectsSharedUserGroupsAndIsReadOnly(t *testing.T) {
	executor := testAgent(t)
	config := executor.config
	config.EnableSourceBuild = true
	config.BuildUser = "nobody"
	config.BuildRootDir = "/var/cache/powerlinux-test-source-builder-do-not-create"
	if config.RunUser == "nobody" {
		config.BuildUser = "daemon"
	}
	config.BuildGroup = "root"
	if validateSourceBuildConfig(config) == nil {
		t.Fatal("Builder gained privileged root group")
	}
	config.BuildGroup = config.RunGroup
	if validateSourceBuildConfig(config) == nil {
		t.Fatal("Builder gained runtime group-protected configuration access")
	}
	config.BuildUser = config.RunUser
	if validateSourceBuildConfig(config) == nil {
		t.Fatal("Runtime daemon UID can tamper source build outputs")
	}
	if _, err := os.Stat(config.BuildRootDir); !os.IsNotExist(err) {
		t.Fatal("Read-only source builder validation created cache", err)
	}
}
