package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func testFilesystem(t *testing.T, root string, readonly bool, limit int64) *Filesystem {
	t.Helper()
	files, err := NewFilesystem([]RootSpec{{ID: "workspace", Path: root, ReadOnly: readonly}}, limit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	return files
}

func TestFilesystemScopedAtomicWriteAndLimits(t *testing.T) {
	dir := t.TempDir()
	files := testFilesystem(t, dir, false, 10)
	if err := files.WriteFile("workspace", "note.txt", []byte("private")); err != nil {
		t.Fatal(err)
	}
	data, err := files.ReadFile("workspace", "note.txt")
	if err != nil || string(data) != "private" {
		t.Fatalf("read: %q %v", data, err)
	}
	info, _ := os.Stat(filepath.Join(dir, "note.txt"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("file is not private")
	}
	if err := files.WriteFile("workspace", "note.txt", []byte("12345678901")); !errors.Is(err, ErrFileTooLarge) {
		t.Fatal("write size not bounded", err)
	}
	for _, path := range []string{"../outside", "nested/../../outside", "/etc/passwd", "."} {
		if err := files.WriteFile("workspace", path, []byte("x")); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("unsafe path %q: %v", path, err)
		}
	}
	if _, err := files.ReadFile("unknown", "note.txt"); !errors.Is(err, ErrUnsafePath) {
		t.Fatal("unknown root accepted")
	}
}

func TestFilesystemRejectsSymlinkEscapesAndSpecialFiles(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	files := testFilesystem(t, dir, false, 100)
	if err := os.WriteFile(filepath.Join(outside, "private"), []byte("outside-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := files.ReadFile("workspace", "escape/private"); err == nil {
		t.Fatal("read escaped root")
	}
	if err := files.WriteFile("workspace", "escape/private", []byte("overwritten")); err == nil {
		t.Fatal("write escaped root")
	}
	data, _ := os.ReadFile(filepath.Join(outside, "private"))
	if string(data) != "outside-secret" {
		t.Fatal("outside file changed")
	}
	if err := os.Symlink(filepath.Join(outside, "private"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := files.WriteFile("workspace", "link", []byte("x")); err == nil {
		t.Fatal("write followed final symlink")
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := files.ReadFile("workspace", "pipe"); err == nil {
		t.Fatal("FIFO accepted as file")
	}
}

func TestFilesystemReadonlyAndRealBackupTask(t *testing.T) {
	dir := t.TempDir()
	files := testFilesystem(t, dir, false, 100)
	if err := files.WriteFile("workspace", "source", []byte("source-content")); err != nil {
		t.Fatal(err)
	}
	executor := NewFileExecutor(files)
	raw, _ := json.Marshal(FileInput{Root: "workspace", Path: "source", DestinationRoot: "workspace", DestinationPath: "backup"})
	result, err := executor.Run(context.Background(), Request{Operation: "file_backup", Input: raw})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("backup: %+v %v", result, err)
	}
	data, err := files.ReadFile("workspace", "backup")
	if err != nil || string(data) != "source-content" {
		t.Fatal("backup mismatch", err)
	}
	readonly := testFilesystem(t, dir, true, 100)
	if err := readonly.WriteFile("workspace", "source", []byte("x")); !errors.Is(err, ErrReadOnly) {
		t.Fatal("readonly root accepted mutation")
	}
	if err := executor.Validate(Request{Operation: "file_write", Input: json.RawMessage(`{"root":"workspace","path":"x","content":"x","argv":["sh"]}`)}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("unknown field accepted", err)
	}
}
