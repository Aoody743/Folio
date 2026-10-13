//go:build linux

package files

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRootRegistrationRefusesSymlinksAndChangedReceipt(t *testing.T) {
	service, path := fixture(t)
	info, _ := os.Stat(path)
	identity := info.Sys().(*syscall.Stat_t)
	if err := service.AddRoot(RootSpec{ID: "pinned", Path: path, ExpectedDevice: uint64(identity.Dev), ExpectedInode: identity.Ino}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := service.AddRoot(RootSpec{ID: "symlink", Path: link}); err == nil {
		t.Fatal("symlink root registered")
	}
	if err := service.AddRoot(RootSpec{ID: "changed", Path: path, ExpectedInode: identity.Ino + 1}); err == nil {
		t.Fatal("changed receipt identity registered")
	}
	// The original descriptor remains pinned even after its pathname changes.
	if err := os.Rename(path, path+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0750); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(path, "sentinel"), []byte("replacement"), 0644)
	os.WriteFile(filepath.Join(path+"-original", "sentinel"), []byte("original"), 0644)
	read, err := service.ReadText(context.Background(), Input{Root: "pinned", Path: "sentinel"})
	if err != nil || read.Content != "original" {
		t.Fatalf("pinned descriptor redirected: %+v %v", read, err)
	}
}

func TestExplicitHostRootProtectsPrivateStateRecursively(t *testing.T) {
	base := t.TempDir()
	private := filepath.Join(base, "panel")
	public := filepath.Join(base, "public")
	os.Mkdir(private, 0700)
	os.Mkdir(public, 0750)
	os.WriteFile(filepath.Join(private, "key"), []byte("private-sentinel"), 0600)
	os.WriteFile(filepath.Join(public, "text"), []byte("public"), 0644)
	service, err := New(Options{StateDir: filepath.Join(base, "file-state"), ProtectedPaths: []string{private}, Roots: []RootSpec{{ID: "host", Path: base, HostAccess: true}, {ID: "public", Path: public}}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	ctx := context.Background()
	if _, err := service.ReadText(ctx, Input{Root: "host", Path: "panel/key"}); !IsCode(err, "protected_path") {
		t.Fatal("host read exposed panel key", err)
	}
	if _, err := service.Permissions(ctx, Input{Root: "host", Path: ".", Recursive: true}); !IsCode(err, "protected_path") {
		t.Fatal("host ancestor could change private-state permissions", err)
	}
	listing, err := service.Walk(ctx, Input{Root: "host", Path: ".", Query: "key"}, false)
	if err != nil || listing.(Listing).Total != 0 {
		t.Fatal("host recursive search exposed private-state metadata", err)
	}
	if _, err := service.Archive(ctx, Input{Root: "host", Path: ".", Format: "zip", DestinationRoot: "public", DestinationPath: "private.zip"}); !IsCode(err, "protected_path") {
		t.Fatal("host archive exposed private-state contents", err)
	}
	if _, err := os.Stat(filepath.Join(public, "private.zip")); !os.IsNotExist(err) {
		t.Fatal("failed protected archive published an artifact")
	}
	if err := service.AddRoot(RootSpec{ID: "unsafe", Path: base}); err == nil {
		t.Fatal("ordinary root containing private state accepted")
	}
}
