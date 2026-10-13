//go:build linux && integration

package files

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

// This binary is deliberately opt-in and only runs as root in our disposable
// QEMU guest. It never mounts disks, changes users or edits production roots.
// Each filesystem gets its own uniquely named test container.
func kernelEXDEVFixture(t *testing.T) (*Service, string, string) {
	t.Helper()
	if os.Getenv("POWERLINUX_QA_EXDEV_GUEST") != "1" {
		t.Skip("requires explicit disposable-QEMU EXDEV environment")
	}
	if os.Geteuid() != 0 {
		t.Fatal("UID/GID preservation acceptance requires the root Agent identity")
	}
	virtualization, err := exec.Command("/usr/bin/systemd-detect-virt", "--vm").Output()
	if err != nil || strings.TrimSpace(string(virtualization)) != "qemu" {
		t.Fatal("kernel EXDEV fixture may only run in our disposable QEMU guest")
	}
	paths := []string{os.Getenv("POWERLINUX_QA_EXDEV_DISK"), os.Getenv("POWERLINUX_QA_EXDEV_TMPFS")}
	containers := make([]string, 2)
	for i, base := range paths {
		if !filepath.IsAbs(base) {
			t.Fatal("explicit absolute disk and tmpfs fixture parents are required")
		}
		container, err := os.MkdirTemp(base, "powerlinux-qa-exdev-")
		if err != nil {
			t.Fatal(err)
		}
		containers[i] = container
		t.Cleanup(func() { os.RemoveAll(container) })
	}
	disk, tmpfs := filepath.Join(containers[0], "public"), filepath.Join(containers[1], "public")
	for _, directory := range []string{disk, tmpfs} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	a, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(tmpfs)
	if err != nil {
		t.Fatal(err)
	}
	if a.Sys().(*syscall.Stat_t).Dev == b.Sys().(*syscall.Stat_t).Dev {
		t.Fatal("test roots are on the same device; this cannot count as EXDEV acceptance")
	}
	probe := filepath.Join(disk, "kernel-rename-probe")
	if err := os.WriteFile(probe, []byte("kernel EXDEV probe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(probe, filepath.Join(tmpfs, "kernel-rename-probe")); !errors.Is(err, syscall.EXDEV) {
		t.Fatal("native kernel rename did not return EXDEV", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	t.Logf("actual kernel EXDEV; disk device=%d tmpfs device=%d", a.Sys().(*syscall.Stat_t).Dev, b.Sys().(*syscall.Stat_t).Dev)
	service, err := New(Options{
		Roots:            []RootSpec{{ID: "disk", Path: disk}, {ID: "tmpfs", Path: tmpfs}},
		StateDir:         filepath.Join(containers[0], "private-state"),
		MaxTransferBytes: 96 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	return service, disk, tmpfs
}

func kernelMetadata(t *testing.T, path string, mode os.FileMode, uid, gid int) {
	t.Helper()
	if err := os.Chown(path, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func kernelVerifyMetadata(t *testing.T, path string, mode os.FileMode, uid, gid uint32) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	identity := info.Sys().(*syscall.Stat_t)
	if info.Mode().Perm() != mode || identity.Uid != uid || identity.Gid != gid {
		t.Fatalf("actual kernel transfer changed %s: mode=%o owner=%d:%d", filepath.Base(path), info.Mode().Perm(), identity.Uid, identity.Gid)
	}
}

func kernelNoPrivateContainers(t *testing.T, roots ...string) {
	t.Helper()
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".powerlinux-") {
				t.Fatal("kernel transfer leaked a private container", filepath.Join(root, entry.Name()))
			}
		}
	}
}

func TestKernelEXDEVMoveTreeRecycleRestoreAndRollback(t *testing.T) {
	service, disk, tmpfs := kernelEXDEVFixture(t)
	ctx := context.Background()
	payload := []byte("actual EXDEV bytes with distinct UID and GID")
	if err := os.WriteFile(filepath.Join(disk, "value"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	kernelMetadata(t, filepath.Join(disk, "value"), 0640, 60001, 60002)
	if _, err := service.CopyAs(ctx, "human", Input{Root: "disk", Path: "value", DestinationRoot: "tmpfs", DestinationPath: "moved"}, true); err != nil {
		t.Fatal(err)
	}
	kernelVerifyMetadata(t, filepath.Join(tmpfs, "moved"), 0640, 60001, 60002)
	if data, err := os.ReadFile(filepath.Join(tmpfs, "moved")); err != nil || string(data) != string(payload) {
		t.Fatal("EXDEV move did not preserve exact bytes", err)
	}
	if _, err := os.Stat(filepath.Join(disk, "value")); !os.IsNotExist(err) {
		t.Fatal("EXDEV move retained its original visible source", err)
	}
	if err := os.MkdirAll(filepath.Join(disk, "tree", "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(disk, "tree", "nested", "value"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	kernelMetadata(t, filepath.Join(disk, "tree"), 0750, 60003, 60004)
	kernelMetadata(t, filepath.Join(disk, "tree", "nested"), 0500, 60005, 60006)
	kernelMetadata(t, filepath.Join(disk, "tree", "nested", "value"), 0440, 60007, 60008)
	if _, err := service.CopyAs(ctx, "mcp", Input{Root: "disk", Path: "tree", DestinationRoot: "tmpfs", DestinationPath: "moved-tree", Recursive: true}, true); err != nil {
		t.Fatal(err)
	}
	verifyTree := func() {
		t.Helper()
		kernelVerifyMetadata(t, filepath.Join(tmpfs, "moved-tree"), 0750, 60003, 60004)
		kernelVerifyMetadata(t, filepath.Join(tmpfs, "moved-tree", "nested"), 0500, 60005, 60006)
		kernelVerifyMetadata(t, filepath.Join(tmpfs, "moved-tree", "nested", "value"), 0440, 60007, 60008)
		if data, err := os.ReadFile(filepath.Join(tmpfs, "moved-tree", "nested", "value")); err != nil || string(data) != string(payload) {
			t.Fatal("EXDEV tree artifact differs", err)
		}
	}
	verifyTree()
	if _, err := os.Stat(filepath.Join(disk, "tree")); !os.IsNotExist(err) {
		t.Fatal("EXDEV tree move retained original source", err)
	}
	deleted, err := service.Delete(ctx, "mcp", Input{Root: "tmpfs", Path: "moved-tree", Recursive: true})
	if err != nil {
		t.Fatal("actual EXDEV trash failed", err)
	}
	trash := deleted.(Trash)
	if trash.ActorID != "mcp" || trash.State != "trashed" {
		t.Fatal("EXDEV trash did not retain its durable actor/state", trash)
	}
	privatePath := filepath.Join(service.options.StateDir, "trash", trash.ID)
	kernelVerifyMetadata(t, privatePath, 0750, 60003, 60004)
	if _, err := os.Stat(filepath.Join(tmpfs, "moved-tree")); !os.IsNotExist(err) {
		t.Fatal("trash retained visible original tree", err)
	}
	if _, err := service.Restore(ctx, Input{Root: "tmpfs", ID: trash.ID}, false); err != nil {
		t.Fatal("actual EXDEV restore failed", err)
	}
	verifyTree()
	if records, err := service.TrashList(ctx, Input{Root: "tmpfs"}); err != nil || len(records.([]Trash)) != 0 {
		t.Fatal("EXDEV restore retained a trash record", err)
	}
	if err := os.Mkdir(filepath.Join(disk, "unsafe"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(disk, "unsafe", "good"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("good", filepath.Join(disk, "unsafe", "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CopyAs(ctx, "human", Input{Root: "disk", Path: "unsafe", DestinationRoot: "tmpfs", DestinationPath: "unsafe", Recursive: true}, true); !IsCode(err, "unsafe_path") {
		t.Fatal("unsafe EXDEV move did not fail and roll back", err)
	}
	if data, err := os.ReadFile(filepath.Join(disk, "unsafe", "good")); err != nil || string(data) != string(payload) {
		t.Fatal("failed EXDEV move lost its intact original", err)
	}
	if link, err := os.Readlink(filepath.Join(disk, "unsafe", "link")); err != nil || link != "good" {
		t.Fatal("failed EXDEV move did not restore its original link", err)
	}
	if _, err := os.Stat(filepath.Join(tmpfs, "unsafe")); !os.IsNotExist(err) {
		t.Fatal("failed EXDEV move published a partial destination", err)
	}
	kernelNoPrivateContainers(t, disk, tmpfs)
}

func TestKernelEXDEVCancelDrainsLeaseAndRestoresSource(t *testing.T) {
	service, disk, tmpfs := kernelEXDEVFixture(t)
	source := filepath.Join(disk, "canceling")
	file, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	err = file.Truncate(64 << 20)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	kernelMetadata(t, source, 0640, 60009, 60010)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (&Executor{Service: service}).Run(ctx, agent.Request{ActorID: "mcp", Operation: "fs_move", Input: encode(Input{Root: "disk", Path: "canceling", DestinationRoot: "tmpfs", DestinationPath: "canceling"})})
		done <- err
	}()
	staged := false
	deadline := time.Now().Add(15 * time.Second)
	for !staged && time.Now().Before(deadline) {
		entries, err := os.ReadDir(tmpfs)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".powerlinux-transfer-") {
				if info, err := os.Stat(filepath.Join(tmpfs, entry.Name(), "payload")); err == nil && info.Size() > 0 {
					staged = true
					break
				}
			}
		}
		if !staged {
			select {
			case err := <-done:
				t.Fatal("copy finished before observing actual staged IO; cancellation was not exercised", err)
			default:
			}
			time.Sleep(time.Millisecond)
		}
	}
	if !staged {
		t.Fatal("no actual EXDEV staging IO was observed")
	}
	maintenance := make(chan error, 1)
	callback := make(chan struct{})
	maintenanceCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	go func() {
		maintenance <- service.RootTransaction(maintenanceCtx, "tmpfs", func() error {
			close(callback)
			if _, err := os.Stat(source); err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(tmpfs, "canceling")); !os.IsNotExist(err) {
				return errors.New("maintenance saw an incomplete canceled destination")
			}
			return nil
		})
	}()
	for {
		service.mu.RLock()
		paused := service.maintenance["tmpfs"] != nil
		service.mu.RUnlock()
		if paused {
			break
		}
		select {
		case <-callback:
			t.Fatal("maintenance callback bypassed an active EXDEV lease")
		case <-maintenanceCtx.Done():
			t.Fatal("maintenance never paused the active EXDEV root")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("actual canceled EXDEV operation did not report cancellation", err)
	}
	if err := <-maintenance; err != nil {
		t.Fatal("maintenance did not drain and inspect the restored source", err)
	}
	kernelVerifyMetadata(t, source, 0640, 60009, 60010)
	if info, err := os.Stat(source); err != nil || info.Size() != 64<<20 {
		t.Fatal("canceled EXDEV move did not restore the complete source", err)
	}
	if _, err := os.Stat(filepath.Join(tmpfs, "canceling")); !os.IsNotExist(err) {
		t.Fatal("canceled EXDEV move left a public destination", err)
	}
	kernelNoPrivateContainers(t, disk, tmpfs)
}
