package files

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAllSymlinkPathsHaveSafeTypedErrors(t *testing.T) {
	service, root := fixture(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "private"), []byte("private-sentinel"), 0600)
	os.Symlink(filepath.Join(outside, "private"), filepath.Join(root, "leaf"))
	os.Symlink(outside, filepath.Join(root, "parent"))
	for _, path := range []string{"leaf", "parent/private"} {
		if _, err := service.ReadText(context.Background(), Input{Root: "workspace", Path: path}); !IsCode(err, "unsafe_path") {
			t.Fatalf("read %s: %v", path, err)
		}
		if _, err := service.Edit(context.Background(), "owner", Input{Root: "workspace", Path: path, Content: "changed", Overwrite: true}); !IsCode(err, "unsafe_path") {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	contents, _ := os.ReadFile(filepath.Join(outside, "private"))
	if string(contents) != "private-sentinel" {
		t.Fatal("symlink changed private source")
	}
}
func TestCopyAndMoveOverwriteRetainBothRevisions(t *testing.T) {
	service, _ := fixture(t)
	ctx := context.Background()
	for _, operation := range []string{"copy", "move"} {
		service.Edit(ctx, "human", Input{Root: "workspace", Path: operation + "-source", Content: "new-data"})
		service.Edit(ctx, "human", Input{Root: "workspace", Path: operation + "-target", Content: "original-data"})
		target, _ := service.ReadText(ctx, Input{Root: "workspace", Path: operation + "-target"})
		_, err := service.CopyAs(ctx, "mcp", Input{Root: "workspace", Path: operation + "-source", DestinationRoot: "workspace", DestinationPath: operation + "-target", ExpectedHash: target.SHA256}, operation == "move")
		if err != nil {
			t.Fatal(err)
		}
		values, err := service.History(ctx, Input{Root: "workspace", Path: operation + "-target"})
		if err != nil {
			t.Fatal(err)
		}
		history := values.([]Revision)
		if len(history) != 2 || history[0].ActorID != "mcp" || history[1].ActorID != "human" {
			t.Fatalf("%s bypassed history: %+v", operation, history)
		}
		prior, err := service.History(ctx, Input{Root: "workspace", Path: operation + "-target", ID: history[1].ID})
		if err != nil || prior.(map[string]any)["content"] != "original-data" {
			t.Fatal("previous copy/move target was lost")
		}
	}
}
