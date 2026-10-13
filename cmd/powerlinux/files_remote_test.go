package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/files"
)

type countedFiles struct {
	executor *files.Executor
	paths    atomic.Int64
}

func (e *countedFiles) Validate(r agent.Request) error { return e.executor.Validate(r) }
func (e *countedFiles) Run(ctx context.Context, r agent.Request) (agent.Result, error) {
	if r.Operation != "fs_roots" {
		e.paths.Add(1)
	}
	return e.executor.Run(ctx, r)
}
func (e *countedFiles) Stream(ctx context.Context, r agent.Request, streams agent.IO) (agent.Result, error) {
	e.paths.Add(1)
	return e.executor.Stream(ctx, r, streams)
}

func remoteFileFixture(t *testing.T) (*core.Service, *fileAdapter, core.Actor, string, string, *countedFiles) {
	t.Helper()
	svc, adapter, actor, local := fileFixture(t)
	base := t.TempDir()
	site := filepath.Join(base, "site-public")
	host := filepath.Join(base, "host")
	os.Mkdir(site, 0750)
	os.Mkdir(host, 0750)
	private, err := files.New(files.Options{StateDir: filepath.Join(base, "agent-state"), Roots: []files.RootSpec{{ID: "site-one", Label: "Site One", Path: site}, {ID: "host", Label: "Host", Path: host, HostAccess: true}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { private.Close() })
	executor := &countedFiles{executor: &files.Executor{Service: private}}
	bindings := map[string]agent.Executor{}
	for _, name := range files.AgentOperationNames() {
		bindings[name] = executor
	}
	dispatcher, err := agent.NewDispatcher(bindings)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "agent.sock")
	server, err := agent.NewUnixServer(socket, []uint32{uint32(os.Getuid())}, dispatcher)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go server.Serve(ctx)
	t.Cleanup(func() { cancel(); server.Close() })
	client, err := agent.NewUnixClient(socket, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	adapter.remote = &files.Remote{Client: client}
	return svc, adapter, actor, local, site, executor
}

func waitFileJob(t *testing.T, svc *core.Service, actor core.Actor, value any) {
	t.Helper()
	id := value.(map[string]any)["id"]
	deadline := time.Now().Add(15 * time.Second)
	for {
		job := invokeFile(t, svc, actor, "tasks_get", map[string]any{"id": id}).(map[string]any)
		if job["status"] == "succeeded" {
			return
		}
		if job["status"] == "failed" || job["status"] == "canceled" || time.Now().After(deadline) {
			t.Fatalf("file job failed: %+v", job)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRemoteFilesActualRPCStreamsHistoryCrossCopyAndScopeBeforePaths(t *testing.T) {
	svc, adapter, actor, local, site, executor := remoteFileFixture(t)
	content := strings.Repeat("GB18030 中文🙂 and native file text\n", 7000)
	request := httptest.NewRequest(http.MethodPut, "/api/v1/files/text?root=site-one&path=text.txt&encoding=gb18030", strings.NewReader(content))
	request.Header.Set("Idempotency-Key", "remote-text-intent-0001")
	response := serveFileRequest(t, svc, adapter, request, "")
	if response.Code != 200 {
		t.Fatalf("remote large text: %d %s", response.Code, response.Body.String())
	}
	var result struct {
		Data struct {
			SHA256 string `json:"sha256"`
		}
	}
	json.Unmarshal(response.Body.Bytes(), &result)
	data := invokeFile(t, svc, actor, "fs_read", map[string]any{"root": "site-one", "path": "text.txt", "encoding": "gb18030", "limit": 2 << 20}).(map[string]any)
	if data["content"] != content || data["sha256"] != result.Data.SHA256 {
		t.Fatal("remote disk encoding/content differs")
	}
	actual, _ := os.ReadFile(filepath.Join(site, "text.txt"))
	if string(actual) == content {
		t.Fatal("remote selected encoding ignored")
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/files/download?root=site-one&path=text.txt", nil)
	request.Header.Set("Range", "bytes=2-12")
	response = serveFileRequest(t, svc, adapter, request, "")
	if response.Code != 206 || response.Body.String() != string(actual[2:13]) {
		t.Fatalf("remote range: %d %q", response.Code, response.Body.String())
	}
	job := invokeFile(t, svc, actor, "fs_copy", map[string]any{"root": "site-one", "path": "text.txt", "destination_root": "workspace", "destination_path": "from-agent.txt"})
	waitFileJob(t, svc, actor, job)
	copied, _ := os.ReadFile(filepath.Join(local, "workspace", "from-agent.txt"))
	if string(copied) != string(actual) {
		t.Fatal("remote to local changed bytes")
	}
	job = invokeFile(t, svc, actor, "fs_copy", map[string]any{"root": "workspace", "path": "from-agent.txt", "destination_root": "site-one", "destination_path": "back-to-agent.txt"})
	waitFileJob(t, svc, actor, job)
	copied, _ = os.ReadFile(filepath.Join(site, "back-to-agent.txt"))
	if string(copied) != string(actual) {
		t.Fatal("local to remote changed bytes")
	}
	created, err := svc.CreateToken(context.Background(), actor, core.TokenRequest{Name: "Host without extra privilege", Scopes: []string{"files:read"}, Resources: []string{"fs:host"}})
	if err != nil {
		t.Fatal(err)
	}
	limited, err := svc.AuthenticateToken(context.Background(), created.Token)
	if err != nil {
		t.Fatal(err)
	}
	before := executor.paths.Load()
	_, err = svc.Execute(context.Background(), limited, "fs_read", mustJSON(map[string]any{"root": "host", "path": "missing"}))
	if core.ErrorCode(err) != "forbidden" || executor.paths.Load() != before {
		t.Fatal("host privilege denial occurred after path RPC", err)
	}
	roots := invokeFile(t, svc, limited, "fs_roots", map[string]any{}).([]map[string]any)
	if len(roots) != 0 {
		t.Fatal("host root disclosed without files:host")
	}
}

func TestRemoteFilesUploadActorBindingAndUnavailableDiscovery(t *testing.T) {
	svc, adapter, actor, _, site, _ := remoteFileFixture(t)
	created := invokeFile(t, svc, actor, "fs_upload_begin", map[string]any{"root": "site-one", "path": "upload.bin", "size": 12}).(map[string]any)
	invokeFile(t, svc, actor, "fs_upload_chunk", map[string]any{"root": "site-one", "id": created["id"], "offset": 0, "data_base64": base64.StdEncoding.EncodeToString([]byte("native-bytes"))})
	job := invokeFile(t, svc, actor, "fs_upload_commit", map[string]any{"root": "site-one", "id": created["id"]})
	waitFileJob(t, svc, actor, job)
	actual, _ := os.ReadFile(filepath.Join(site, "upload.bin"))
	if string(actual) != "native-bytes" {
		t.Fatal("remote chunk did not produce file")
	}
	other, err := svc.CreateToken(context.Background(), actor, core.TokenRequest{Name: "Other actor", Scopes: []string{"files:write"}, Resources: []string{"fs:site-one"}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/files/uploads/"+created["id"].(string)+"/chunk?root=site-one&offset=0", strings.NewReader("new"))
	response := serveFileRequest(t, svc, adapter, request, other.Token)
	if response.Code != 403 {
		t.Fatalf("remote upload actor binding: %d %s", response.Code, response.Body.String())
	}
	// A deliberately disabled module reports its exact typed unavailable code.
	socket := filepath.Join(t.TempDir(), "disabled.sock")
	server, err := agent.NewUnixServer(socket, []uint32{uint32(os.Getuid())}, unavailableExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.Serve(ctx)
	client, err := agent.NewUnixClient(socket, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	adapter.remote = &files.Remote{Client: client}
	roots := invokeFile(t, svc, actor, "fs_roots", map[string]any{}).([]map[string]any)
	if len(roots) != 1 || roots[0]["id"] != "workspace" {
		t.Fatal("disabled module removed local workspace")
	}
}

func TestRemoteDirectoryAndBatchTransfersAreCompleteAndConflictSafe(t *testing.T) {
	svc, _, actor, local, site, _ := remoteFileFixture(t)
	directory := filepath.Join(local, "workspace", "tree")
	os.Mkdir(directory, 0750)
	os.Mkdir(filepath.Join(directory, "nested"), 0750)
	os.WriteFile(filepath.Join(directory, "nested", "value"), []byte("actual-private-tree"), 0640)
	if err := os.Chmod(filepath.Join(directory, "nested", "value"), 0640); err != nil {
		t.Fatal(err)
	}
	job := invokeFile(t, svc, actor, "fs_copy", map[string]any{"root": "workspace", "path": "tree", "destination_root": "site-one", "destination_path": "from-local", "recursive": true})
	waitFileJob(t, svc, actor, job)
	actual, err := os.ReadFile(filepath.Join(site, "from-local", "nested", "value"))
	if err != nil || string(actual) != "actual-private-tree" {
		t.Fatal("directory transfer bytes", err)
	}
	info, _ := os.Stat(filepath.Join(site, "from-local", "nested", "value"))
	if info.Mode().Perm() != 0640 {
		t.Fatal("directory transfer permission changed")
	}
	job = invokeFile(t, svc, actor, "fs_move", map[string]any{"root": "site-one", "path": "from-local", "destination_root": "workspace", "destination_path": "moved-from-agent", "recursive": true})
	waitFileJob(t, svc, actor, job)
	if _, err := os.Stat(filepath.Join(site, "from-local")); !os.IsNotExist(err) {
		t.Fatal("cross-service move retained original directory")
	}
	actual, err = os.ReadFile(filepath.Join(local, "workspace", "moved-from-agent", "nested", "value"))
	if err != nil || string(actual) != "actual-private-tree" {
		t.Fatal("moved directory artifact", err)
	}
	os.Mkdir(filepath.Join(site, "batch"), 0750)
	for _, name := range []string{"a", "b"} {
		invokeFile(t, svc, actor, "fs_create", map[string]any{"root": "workspace", "path": name, "content": "new-" + name})
		invokeFile(t, svc, actor, "fs_create", map[string]any{"root": "site-one", "path": "batch/" + name, "content": "old-" + name})
	}
	preflightJob := invokeFile(t, svc, actor, "fs_preflight", map[string]any{"root": "workspace", "items": []string{"a", "b"}, "operation": "copy", "destination_root": "site-one", "destination_path": "batch"})
	waitFileJob(t, svc, actor, preflightJob)
	// Exact hashes come from the target's observed bytes, then one changes.
	expected := map[string]string{"batch/a": contentHashMain([]byte("old-a")), "batch/b": contentHashMain([]byte("old-b"))}
	invokeFile(t, svc, actor, "fs_edit", map[string]any{"root": "site-one", "path": "batch/b", "content": "changed", "expected_hash": expected["batch/b"]})
	job = invokeFile(t, svc, actor, "fs_batch", map[string]any{"root": "workspace", "items": []string{"a", "b"}, "operation": "copy", "destination_root": "site-one", "destination_path": "batch", "expected_hashes": expected})
	id := job.(map[string]any)["id"]
	deadline := time.Now().Add(15 * time.Second)
	for {
		state := invokeFile(t, svc, actor, "tasks_get", map[string]any{"id": id}).(map[string]any)
		if state["status"] == "failed" {
			break
		}
		if state["status"] == "succeeded" || time.Now().After(deadline) {
			t.Fatal("stale cross-root batch succeeded", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	actual, _ = os.ReadFile(filepath.Join(site, "batch", "a"))
	if string(actual) != "old-a" {
		t.Fatal("batch changed early target before full preflight")
	}
}

func contentHashMain(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func TestRemoteArchiveAndExtractStreamAcrossServices(t *testing.T) {
	svc, _, actor, local, site, _ := remoteFileFixture(t)
	content := strings.Repeat("bounded archive streaming\x00 bytes 中文\n", 9000)
	for _, format := range []string{"zip", "tar.gz", "tar.xz"} {
		t.Run(format, func(t *testing.T) {
			name := strings.ReplaceAll(format, ".", "-")
			directory := filepath.Join(local, "workspace", name)
			if err := os.Mkdir(directory, 0750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "value"), []byte(content), 0640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(directory, "value"), 0640); err != nil {
				t.Fatal(err)
			}
			archive := name + "." + format
			waitFileJob(t, svc, actor, invokeFile(t, svc, actor, "fs_archive", map[string]any{"root": "workspace", "path": name, "destination_root": "site-one", "destination_path": archive, "format": format}))
			waitFileJob(t, svc, actor, invokeFile(t, svc, actor, "fs_extract", map[string]any{"root": "site-one", "path": archive, "destination_root": "workspace", "destination_path": name + "-out", "format": format}))
			actual, err := os.ReadFile(filepath.Join(local, "workspace", name+"-out", name, "value"))
			if err != nil || string(actual) != content {
				t.Fatal("mixed archive/extract changed bytes", err)
			}
			info, err := os.Stat(filepath.Join(local, "workspace", name+"-out", name, "value"))
			if err != nil || info.Mode().Perm() != 0640 {
				t.Fatal("mixed archive/extract lost file mode", err)
			}
			waitFileJob(t, svc, actor, invokeFile(t, svc, actor, "fs_archive", map[string]any{"root": "site-one", "path": archive, "destination_root": "workspace", "destination_path": name + "-back.tar", "format": "tar"}))
			waitFileJob(t, svc, actor, invokeFile(t, svc, actor, "fs_extract", map[string]any{"root": "workspace", "path": name + "-back.tar", "destination_root": "site-one", "destination_path": name + "-back-out", "format": "tar"}))
			before, err := os.ReadFile(filepath.Join(site, archive))
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(filepath.Join(site, name+"-back-out", archive))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("reverse mixed archive/extract changed bytes", err)
			}
		})
	}
	// An archive arriving over the Agent stream must still reject traversal
	// before any destination becomes visible.
	var unsafe bytes.Buffer
	writer := tar.NewWriter(&unsafe)
	if err := writer.WriteHeader(&tar.Header{Name: "../escape", Mode: 0600, Size: 7}); err != nil {
		t.Fatal(err)
	}
	writer.Write([]byte("outside"))
	writer.Close()
	if err := os.WriteFile(filepath.Join(local, "workspace", "unsafe.tar"), unsafe.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	job := invokeFile(t, svc, actor, "fs_extract", map[string]any{"root": "workspace", "path": "unsafe.tar", "destination_root": "site-one", "destination_path": "unsafe-out", "format": "tar"}).(map[string]any)
	deadline := time.Now().Add(15 * time.Second)
	for {
		state := invokeFile(t, svc, actor, "tasks_get", map[string]any{"id": job["id"]}).(map[string]any)
		if state["status"] == "failed" {
			break
		}
		if state["status"] == "succeeded" || time.Now().After(deadline) {
			t.Fatal("unsafe streamed archive succeeded", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(site, "unsafe-out")); !os.IsNotExist(err) {
		t.Fatal("unsafe archive published a destination", err)
	}
	if _, err := os.Stat(filepath.Join(site, "escape")); !os.IsNotExist(err) {
		t.Fatal("unsafe archive escaped its destination", err)
	}
}
