package files

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCrossDeviceMoveTransferAndFailureRestore(t *testing.T) {
	service, root := fixture(t)
	ctx := context.Background()
	source, _, _ := service.resolve(Ref{"workspace", "from"}, true)
	for _, directory := range []string{"from", "to"} {
		os.Mkdir(filepath.Join(root, directory), 0750)
	}
	os.WriteFile(filepath.Join(root, "from", "good"), []byte("actual-transfer"), 0640)
	_, err := service.moveAcrossDevices(ctx, "human", source, "from/good", source, "to/good", Input{})
	if err != nil {
		t.Fatal(err)
	}
	contents, _ := os.ReadFile(filepath.Join(root, "to", "good"))
	if string(contents) != "actual-transfer" {
		t.Fatal("cross-device transfer artifact differs")
	}
	if _, err := os.Stat(filepath.Join(root, "from", "good")); !os.IsNotExist(err) {
		t.Fatal("moved source still visible")
	}
	os.Mkdir(filepath.Join(root, "from", "unsafe"), 0750)
	os.WriteFile(filepath.Join(root, "from", "unsafe", "good"), []byte("retained"), 0640)
	os.Symlink("../good", filepath.Join(root, "from", "unsafe", "link"))
	if _, err := service.moveAcrossDevices(ctx, "human", source, "from/unsafe", source, "to/unsafe", Input{Recursive: true}); !IsCode(err, "unsafe_path") {
		t.Fatal("unsafe fallback move result", err)
	}
	contents, _ = os.ReadFile(filepath.Join(root, "from", "unsafe", "good"))
	if string(contents) != "retained" {
		t.Fatal("failed move did not restore intact source")
	}
	if _, err := os.Stat(filepath.Join(root, "to", "unsafe")); !os.IsNotExist(err) {
		t.Fatal("failed move published partial destination")
	}
	entries, _ := os.ReadDir(filepath.Join(root, "from"))
	if len(entries) != 1 || entries[0].Name() != "unsafe" {
		t.Fatal("failed move leaked private recovery containers", entries)
	}
}

func TestCrossDeviceRecycleAndRestorePreserveBytesAndMode(t *testing.T) {
	service, root := fixture(t)
	ctx := context.Background()
	os.WriteFile(filepath.Join(root, "file"), []byte("recycle-transfer"), 0640)
	if err := os.Chmod(filepath.Join(root, "file"), 0640); err != nil {
		t.Fatal(err)
	}
	managed, _, _ := service.resolve(Ref{"workspace", "file"}, true)
	name := "trash/fallback-fixture"
	if err := service.recycleAcrossDevices(ctx, managed, "file", name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "file")); !os.IsNotExist(err) {
		t.Fatal("recycled source remains")
	}
	if err := service.restoreAcrossDevices(ctx, name, managed, "file"); err != nil {
		t.Fatal(err)
	}
	contents, _ := os.ReadFile(filepath.Join(root, "file"))
	info, _ := os.Stat(filepath.Join(root, "file"))
	if string(contents) != "recycle-transfer" || info.Mode().Perm() != 0640 {
		t.Fatalf("restore changed bytes or mode: content=%q mode=%o", contents, info.Mode().Perm())
	}
}
