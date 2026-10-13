package files

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBatchPreflightRejectsUnsafeAndStaleDestinationsBeforeMutations(t *testing.T) {
	service, root := fixture(t)
	ctx := context.Background()
	os.Mkdir(filepath.Join(root, "from"), 0750)
	os.Mkdir(filepath.Join(root, "to"), 0750)
	for _, name := range []string{"a", "b"} {
		service.Edit(ctx, "human", Input{Root: "workspace", Path: "from/" + name, Content: "new-" + name})
		service.Edit(ctx, "human", Input{Root: "workspace", Path: "to/" + name, Content: "old-" + name})
	}
	in := Input{Root: "workspace", Path: "from", DestinationRoot: "workspace", DestinationPath: "to", Items: []string{"a", "b"}, Operation: "copy"}
	preflight, err := service.Preflight(ctx, in)
	if err != nil || preflight.Conflicts != 2 || preflight.Total != 2 {
		t.Fatalf("preflight: %+v %v", preflight, err)
	}
	in.ExpectedHashes = map[string]string{}
	for _, item := range preflight.Items {
		in.ExpectedHashes[item.Destination] = item.CurrentHash
	}
	service.Edit(ctx, "mcp", Input{Root: "workspace", Path: "to/b", Content: "changed-after-preflight", ExpectedHash: in.ExpectedHashes["to/b"]})
	if _, err := service.Batch(ctx, "human", in); !IsCode(err, "conflict") {
		t.Fatal("stale preflight accepted", err)
	}
	contents, _ := os.ReadFile(filepath.Join(root, "to/a"))
	if string(contents) != "old-a" {
		t.Fatal("an early batch item mutated before all destination checks passed")
	}
	in.Items = []string{"a", "../outside"}
	if _, err := service.Batch(ctx, "human", in); !IsCode(err, "unsafe_path") {
		t.Fatal("unsafe late batch path accepted", err)
	}
	in.Items = []string{"a", "b"}
	preflight, _ = service.Preflight(ctx, in)
	for _, item := range preflight.Items {
		in.ExpectedHashes[item.Destination] = item.CurrentHash
	}
	result, err := service.Batch(ctx, "mcp", in)
	if err != nil || result.Completed != 2 || result.Failed != 0 {
		t.Fatalf("batch copy: %+v %v", result, err)
	}
	for _, name := range in.Items {
		contents, _ := os.ReadFile(filepath.Join(root, "to", name))
		if string(contents) != "new-"+name {
			t.Fatal("batch artifact wrong")
		}
	}
}

func TestRecycleTogglePermanentDeleteAndClearAreRootScoped(t *testing.T) {
	service, root := fixture(t)
	ctx := context.Background()
	enabled, err := service.TrashSettings(ctx, "workspace")
	if err != nil || !enabled.Enabled {
		t.Fatal("safe recycle default absent", err)
	}
	service.Edit(ctx, "human", Input{Root: "workspace", Path: "kept", Content: "restore me"})
	service.Delete(ctx, "human", Input{Root: "workspace", Path: "kept"})
	disabled := false
	if _, err := service.ConfigureTrash(ctx, "mcp", Input{Root: "workspace", Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	service.Edit(ctx, "human", Input{Root: "workspace", Path: "permanent", Content: "remove me"})
	result, err := service.Delete(ctx, "human", Input{Root: "workspace", Path: "permanent"})
	if err != nil || result.(map[string]any)["permanent"] != true {
		t.Fatal("disabled trash did not delete permanently", err)
	}
	if _, err := os.Stat(filepath.Join(root, "permanent")); !os.IsNotExist(err) {
		t.Fatal("permanent delete did not remove file")
	}
	trash, _ := service.TrashList(ctx, Input{Root: "workspace"})
	if len(trash.([]Trash)) != 1 {
		t.Fatal("toggle changed existing recycle entries")
	}
	cleared, err := service.ClearTrash(ctx, Input{Root: "workspace"})
	if err != nil || cleared.Completed != 1 || cleared.Failed != 0 {
		t.Fatal("clear recycle bin failed", err)
	}
	trash, _ = service.TrashList(ctx, Input{Root: "workspace"})
	if len(trash.([]Trash)) != 0 {
		t.Fatal("purged recycle metadata retained")
	}
	if _, err := service.ConfigureTrash(ctx, "human", Input{Root: "workspace"}); !IsCode(err, "invalid_input") {
		t.Fatal("implicit recycle toggle accepted", err)
	}
}
