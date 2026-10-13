package files

import (
	"archive/tar"
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

func fixture(t *testing.T) (*Service, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "public")
	os.Mkdir(root, 0750)
	service, err := New(Options{Roots: []RootSpec{{ID: "workspace", Path: root}}, StateDir: filepath.Join(base, "private")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	return service, root
}
func contentHash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func TestEncodingEditingHistoryConflictAndMetadata(t *testing.T) {
	service, root := fixture(t)
	ctx := context.Background()
	for encoding, content := range map[string]string{"utf-8": "中文🙂", "gb18030": "中文🙂", "gbk": "你好，世界", "big5": "繁體中文"} {
		path := encoding + ".txt"
		if _, err := service.Edit(ctx, "human", Input{Root: "workspace", Path: path, Content: content, Encoding: encoding}); err != nil {
			t.Fatal(err)
		}
		text, err := service.ReadText(ctx, Input{Root: "workspace", Path: path, Encoding: encoding})
		if err != nil || text.Content != content {
			t.Fatalf("encoding %s: %+v %v", encoding, text, err)
		}
		os.Chmod(filepath.Join(root, path), 0640)
		if _, err := service.Edit(ctx, "mcp", Input{Root: "workspace", Path: path, Content: content + "!", Encoding: encoding, ExpectedHash: text.SHA256}); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Edit(ctx, "human", Input{Root: "workspace", Path: path, Content: "stale", ExpectedHash: text.SHA256}); !IsCode(err, "conflict") {
			t.Fatal("stale edit accepted", err)
		}
		info, _ := os.Stat(filepath.Join(root, path))
		if info.Mode().Perm() != 0640 {
			t.Fatal("edit changed existing permissions")
		}
		history, err := service.History(ctx, Input{Root: "workspace", Path: path})
		if err != nil {
			t.Fatal(err)
		}
		revisions := history.([]Revision)
		if len(revisions) != 2 || revisions[0].ActorID != "mcp" || revisions[1].ActorID != "human" {
			t.Fatalf("wrong history author: %+v", revisions)
		}
		old, err := service.History(ctx, Input{Root: "workspace", Path: path, ID: revisions[1].ID})
		if err != nil || old.(map[string]any)["content"] != content {
			t.Fatal("old contents not retained", err)
		}
	}
	if _, err := service.Edit(ctx, "human", Input{Root: "workspace", Path: "bad", Content: "🙂", Encoding: "gbk"}); err == nil {
		t.Fatal("unencodable character silently dropped")
	}
}
func TestConcurrentExpectedHashHasSingleWinner(t *testing.T) {
	service, _ := fixture(t)
	ctx := context.Background()
	service.Edit(ctx, "owner", Input{Root: "workspace", Path: "shared", Content: "initial"})
	text, _ := service.ReadText(ctx, Input{Root: "workspace", Path: "shared"})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, actor := range []string{"human", "mcp"} {
		wg.Add(1)
		go func(actor string) {
			defer wg.Done()
			_, err := service.Edit(ctx, actor, Input{Root: "workspace", Path: "shared", Content: actor, ExpectedHash: text.SHA256})
			results <- err
		}(actor)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if IsCode(err, "conflict") {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("concurrent updates lost", success, conflict)
	}
}
func TestPaginationSearchCopyMovePermissionsAndPrivateRecycle(t *testing.T) {
	service, root := fixture(t)
	ctx := context.Background()
	os.Mkdir(filepath.Join(root, "nested"), 0750)
	for _, name := range []string{"c", "a", "b", "nested/match"} {
		os.WriteFile(filepath.Join(root, name), []byte("content"), 0640)
	}
	listing, err := service.List(ctx, Input{Root: "workspace", Path: ".", Offset: 1, Limit: 2, Sort: "name"})
	if err != nil || listing.Total != 4 || listing.Items[0].Name != "b" {
		t.Fatalf("pagination: %+v %v", listing, err)
	}
	search, err := service.Walk(ctx, Input{Root: "workspace", Path: ".", Query: "match"}, false)
	if err != nil || search.(Listing).Items[0].Path != "nested/match" {
		t.Fatal("recursive search failed", err)
	}
	if _, err := service.Copy(ctx, Input{Root: "workspace", Path: "nested", DestinationRoot: "workspace", DestinationPath: "copy", Recursive: true}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Copy(ctx, Input{Root: "workspace", Path: "copy", DestinationRoot: "workspace", DestinationPath: "moved", Recursive: true}, true); err != nil {
		t.Fatal(err)
	}
	mode := uint32(0600)
	if _, err := service.Permissions(ctx, Input{Root: "workspace", Path: "moved/match", Mode: &mode}); err != nil {
		t.Fatal(err)
	}
	deleted, err := service.Delete(ctx, "human", Input{Root: "workspace", Path: "moved", Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	trash := deleted.(Trash)
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		if strings.Contains(entry.Name(), trash.ID) {
			t.Fatal("deleted contents exposed in public directory")
		}
	}
	if _, err := service.Restore(ctx, Input{Root: "workspace", ID: trash.ID}, false); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "moved/match"))
	if err != nil || string(content) != "content" {
		t.Fatal("restore lost contents", err)
	}
	deleted, err = service.Delete(ctx, "human", Input{Root: "workspace", Path: "moved", Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Restore(ctx, Input{Root: "workspace", ID: deleted.(Trash).ID}, true); err != nil {
		t.Fatal(err)
	}
}
func TestResumableUploadChecksumIdentityConflictAndStream(t *testing.T) {
	service, _ := fixture(t)
	ctx := context.Background()
	content := strings.Repeat("chunk-data\n", 10000)
	session, err := service.UploadBegin(ctx, "human", Input{Root: "workspace", Path: "uploaded", Size: int64(len(content)), SHA256: contentHash([]byte(content))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UploadChunk(ctx, "mcp", session.ID, 0, strings.NewReader("x"), ""); !IsCode(err, "forbidden") {
		t.Fatal("another actor appended upload", err)
	}
	half := len(content) / 2
	if _, err := service.UploadChunk(ctx, "human", session.ID, 0, strings.NewReader(content[:half]), strings.Repeat("0", 64)); !IsCode(err, "checksum_mismatch") {
		t.Fatal("bad checksum accepted", err)
	}
	if _, err := service.UploadChunk(ctx, "human", session.ID, 0, strings.NewReader(content[:half]), contentHash([]byte(content[:half]))); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UploadChunk(ctx, "human", session.ID, 0, strings.NewReader(content[half:]), ""); !IsCode(err, "conflict") {
		t.Fatal("stale chunk offset accepted", err)
	}
	if _, err := service.UploadChunk(ctx, "human", session.ID, int64(half), strings.NewReader(content[half:]), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UploadCommit(ctx, "human", Input{Root: "workspace", ID: session.ID}); err != nil {
		t.Fatal(err)
	}
	file, entry, err := service.OpenDownload(ctx, Ref{"workspace", "uploaded"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	file.Close()
	if err != nil || string(data) != content || entry.Size != int64(len(content)) {
		t.Fatal("stream did not preserve upload", err)
	}
	if _, err := service.UploadCommit(ctx, "human", Input{Root: "workspace", ID: session.ID}); err != nil {
		t.Fatal("commit is not idempotent", err)
	}
}
func TestRemoteDownloadVerifiesTLSChecksumAndBlocksPrivateTargets(t *testing.T) {
	service, root := fixture(t)
	ctx := context.Background()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "verified remote content") }))
	defer server.Close()
	if _, err := service.RemoteDownload(ctx, "human", Input{Root: "workspace", Path: "remote", URL: server.URL}); err == nil {
		t.Fatal("private target accepted")
	}
	service.options.AllowPrivateDownloads = true
	service.options.DownloadClient = server.Client()
	if _, err := service.RemoteDownload(ctx, "human", Input{Root: "workspace", Path: "remote", URL: server.URL, SHA256: strings.Repeat("0", 64)}); !IsCode(err, "checksum_mismatch") {
		t.Fatal("bad remote digest accepted", err)
	}
	if _, err := os.Stat(filepath.Join(root, "remote")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unverified remote content committed")
	}
	if _, err := service.RemoteDownload(ctx, "human", Input{Root: "workspace", Path: "remote", URL: server.URL, SHA256: contentHash([]byte("verified remote content"))}); err != nil {
		t.Fatal(err)
	}
}
func TestArchivesRoundTripAndUnsafeEntriesHaveNoSideEffects(t *testing.T) {
	service, root := fixture(t)
	ctx := context.Background()
	os.Mkdir(filepath.Join(root, "source"), 0750)
	os.WriteFile(filepath.Join(root, "source", "note"), []byte("archive-content"), 0640)
	for _, format := range []string{"zip", "tar", "tar.gz", "tar.xz", "tar.bz2"} {
		t.Run(format, func(t *testing.T) {
			archiveName := "bundle." + format
			if _, err := service.Archive(ctx, Input{Root: "workspace", Path: "source", DestinationRoot: "workspace", DestinationPath: archiveName, Format: format}); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Extract(ctx, Input{Root: "workspace", Path: archiveName, DestinationRoot: "workspace", DestinationPath: "restore-" + format, Format: format}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(root, "restore-"+format, "source", "note"))
			if err != nil || string(data) != "archive-content" {
				t.Fatal("archive round trip failed", err)
			}
		})
	}
	bad, err := os.Create(filepath.Join(root, "unsafe.zip"))
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(bad)
	entry, _ := zw.Create("../outside")
	entry.Write([]byte("escape"))
	zw.Close()
	bad.Close()
	if _, err := service.Extract(ctx, Input{Root: "workspace", Path: "unsafe.zip", DestinationRoot: "workspace", DestinationPath: "unsafe-out", Format: "zip"}); err == nil {
		t.Fatal("archive traversal accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "unsafe-out")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsafe archive touched destination")
	}
	bad, _ = os.Create(filepath.Join(root, "link.tar"))
	tw := tar.NewWriter(bad)
	tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0777})
	tw.Close()
	bad.Close()
	if _, err := service.Extract(ctx, Input{Root: "workspace", Path: "link.tar", DestinationRoot: "workspace", DestinationPath: "link-out", Format: "tar"}); err == nil {
		t.Fatal("archive symlink accepted")
	}
}
func TestBoundariesUTF8TailDynamicRootAndReadOnly(t *testing.T) {
	service, root := fixture(t)
	ctx := context.Background()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600)
	os.Symlink(outside, filepath.Join(root, "escape"))
	if _, err := service.ReadText(ctx, Input{Root: "workspace", Path: "escape/secret"}); err == nil {
		t.Fatal("symlink escaped filesystem root")
	}
	if _, err := service.Edit(ctx, "human", Input{Root: "workspace", Path: "../secret", Content: "x"}); err == nil {
		t.Fatal("write escaped root")
	}
	content := strings.Repeat("🙂", 20000)
	service.Edit(ctx, "human", Input{Root: "workspace", Path: "unicode", Content: content})
	text, err := service.ReadText(ctx, Input{Root: "workspace", Path: "unicode"})
	if err != nil || !text.Truncated || len(text.Content) > 64<<10 {
		t.Fatal("unicode truncation invalid", err)
	}
	service.Edit(ctx, "human", Input{Root: "workspace", Path: "log", Content: "first\nsecond\nthird"})
	tail, err := service.Tail(ctx, Input{Root: "workspace", Path: "log", Lines: 2})
	if err != nil || tail.Content != "second\nthird" {
		t.Fatal("tail incorrect", err)
	}
	if err := service.AddRoot(RootSpec{ID: "other", Path: outside, ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Edit(ctx, "human", Input{Root: "other", Path: "new", Content: "x"}); !IsCode(err, "read_only") {
		t.Fatal("read-only root writable", err)
	}
	if err := service.DeleteRoot("other"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReadText(ctx, Input{Root: "other", Path: "secret"}); !IsCode(err, "unsafe_path") {
		t.Fatal("removed root still usable", err)
	}
	raw, _ := json.Marshal(Input{Root: "workspace", Path: "x", Content: "x"})
	result, err := (&Executor{Service: service}).Run(ctx, agent.Request{ActorID: "mcp", Operation: "fs_edit", Input: raw})
	if err != nil || result.ExitCode != 0 || len(result.Data) == 0 {
		t.Fatal("task executor lost trusted actor/result", err)
	}
}
