//go:build hosted_apps_integration

package plugins

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestHostedNodeBuildUsesNpmLockAndSwitchesAfterSuccess(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("isolated application build requires root to set its child identity")
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm is not installed in this test environment")
	}
	account, err := user.LookupId("65534")
	if err != nil {
		t.Skip("nobody account is unavailable")
	}
	uid64, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil || uid64 == 0 {
		t.Fatal("nobody account has an invalid UID")
	}
	gid64, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil || gid64 == 0 {
		t.Fatal("nobody account has an invalid GID")
	}
	root := t.TempDir()
	if err = os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "project")
	data := filepath.Join(root, "data")
	for _, directory := range []string{project, data} {
		if err = os.Mkdir(directory, 0750); err != nil {
			t.Fatal(err)
		}
		if err = os.Chown(directory, int(uid64), int(gid64)); err != nil {
			t.Fatal(err)
		}
	}
	if err = writeHostedAppFiles(project, hostedAppTemplates(hostedAppCreateInput{ID: "smoke", Name: "Smoke", Runtime: "nodejs"}), uint32(uid64), uint32(gid64)); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(project, ".home")
	if err = os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Chown(home, int(uid64), int(gid64)); err != nil {
		t.Fatal(err)
	}
	receipt := hostedAppReceipt{
		HostedApplication: HostedApplication{ID: "smoke", Runtime: "nodejs"},
		UID:               uint32(uid64),
		GID:               uint32(gid64),
		Root:              root,
		ProjectDir:        project,
		DataDir:           data,
	}
	credential := &syscall.Credential{Uid: uint32(uid64), Gid: uint32(gid64), Groups: []uint32{uint32(gid64)}}
	environment := []string{
		"HOME=" + home,
		"PATH=" + filepath.Dir(npm) + ":/usr/bin:/bin",
		"PORT=43127",
		"NODE_ENV=production",
		"npm_config_offline=true",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err = (&PrivilegedExecutor{}).buildNodeHostedApp(ctx, receipt, npm, credential, environment); err != nil {
		t.Fatalf("locked Node.js template build failed: %v", err)
	}
	if !hostedAppProjectBuilt(project, "nodejs") {
		t.Fatal("successful npm ci did not produce the managed built-project marker")
	}
	if _, err = os.Stat(filepath.Join(root, ".project.build.previous", "package-lock.json")); err != nil {
		t.Fatalf("the prior project was not retained after the successful switch: %v", err)
	}
}
