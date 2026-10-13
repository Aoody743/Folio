package files

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestArchiveAndTreeImportsPreserveDirectoryModes(t *testing.T) {
	service, root := fixture(t)
	ctx := context.Background()
	source := filepath.Join(root, "source")
	t.Cleanup(func() {
		for _, path := range []string{"source", "source/nested", "tar-out/source", "tar-out/source/nested", "zip-out/source", "zip-out/source/nested", "tree-out", "tree-out/nested"} {
			os.Chmod(filepath.Join(root, path), 0700)
		}
	})
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "value"), []byte("readonly directory still extracts its children"), 0440); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{source: 0770, filepath.Join(source, "nested"): 0500, filepath.Join(source, "nested", "value"): 0440} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	parent, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	identity := parent.Sys().(*syscall.Stat_t)
	verify := func(base string) {
		t.Helper()
		for path, mode := range map[string]os.FileMode{".": 0770, "nested": 0500, "nested/value": 0440} {
			info, err := os.Stat(filepath.Join(base, path))
			if err != nil {
				t.Fatal(err)
			}
			owner := info.Sys().(*syscall.Stat_t)
			if info.Mode().Perm() != mode || owner.Uid != identity.Uid || owner.Gid != identity.Gid {
				t.Fatalf("import changed %s mode or destination identity: mode=%o owner=%d:%d", path, info.Mode().Perm(), owner.Uid, owner.Gid)
			}
		}
		data, err := os.ReadFile(filepath.Join(base, "nested", "value"))
		if err != nil || string(data) != "readonly directory still extracts its children" {
			t.Fatal("import did not populate readonly directory", err)
		}
	}
	for _, format := range []string{"tar", "zip"} {
		t.Run(format, func(t *testing.T) {
			if _, err := service.Archive(ctx, Input{Root: "workspace", Path: "source", DestinationRoot: "workspace", DestinationPath: "source." + format, Format: format}); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Extract(ctx, Input{Root: "workspace", Path: "source." + format, DestinationRoot: "workspace", DestinationPath: format + "-out", Format: format}); err != nil {
				t.Fatal(err)
			}
			verify(filepath.Join(root, format+"-out", "source"))
		})
	}
	var stream bytes.Buffer
	managed, path, err := service.resolve(Ref{"workspace", "source"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.exportTree(ctx, managed, path, &stream); err != nil {
		t.Fatal(err)
	}
	mode := uint32(0770)
	if _, err := service.importTree(ctx, "mcp", Input{Root: "workspace", Path: "tree-out", Mode: &mode}, &stream); err != nil {
		t.Fatal(err)
	}
	verify(filepath.Join(root, "tree-out"))
}
