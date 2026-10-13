package files

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDirectoryCopyFailureNeverPublishesPartialDestination(t *testing.T) {
	service, root := fixture(t)
	os.Mkdir(filepath.Join(root, "source"), 0750)
	os.WriteFile(filepath.Join(root, "source", "a-good"), []byte("source-data"), 0640)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "key"), []byte("private"), 0600)
	os.Symlink(filepath.Join(outside, "key"), filepath.Join(root, "source", "z-unsafe"))
	_, err := service.CopyAs(context.Background(), "mcp", Input{Root: "workspace", Path: "source", DestinationRoot: "workspace", DestinationPath: "destination", Recursive: true}, false)
	if !IsCode(err, "unsafe_path") {
		t.Fatal("unsafe directory copy result", err)
	}
	if _, err := os.Stat(filepath.Join(root, "destination")); !os.IsNotExist(err) {
		t.Fatal("failed copy left a partial destination")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 || entries[0].Name() != "source" {
		t.Fatal("private directory staging leaked", entries)
	}
	contents, _ := os.ReadFile(filepath.Join(outside, "key"))
	if string(contents) != "private" {
		t.Fatal("unsafe copy modified outside file")
	}
}
