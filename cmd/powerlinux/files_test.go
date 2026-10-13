package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/files"
	"github.com/Aoody743/PowerLinux/internal/httpapi"
)

func fileFixture(t *testing.T) (*core.Service, *fileAdapter, core.Actor, string) {
	t.Helper()
	dir := t.TempDir()
	svc, err := core.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := os.ReadFile(svc.SetupSecretFile())
	if err != nil {
		t.Fatal(err)
	}
	actor, err := svc.Setup(context.Background(), strings.TrimSpace(string(secret)), "admin", "a strong adapter test password")
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := registerFiles(svc, dir)
	if err != nil {
		svc.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close(); adapter.files.Close() })
	return svc, adapter, actor, dir
}

func serveFileRequest(t *testing.T, svc *core.Service, adapter *fileAdapter, request *http.Request, token string) *httptest.ResponseRecorder {
	t.Helper()
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	} else {
		session, err := svc.Login(request.Context(), "admin", "a strong adapter test password", "", "file-adapter-test")
		if err != nil {
			t.Fatal(err)
		}
		request.AddCookie(&http.Cookie{Name: "pl_session", Value: session.ID})
		request.Header.Set("X-CSRF-Token", session.CSRFToken)
	}
	recorder := httptest.NewRecorder()
	httpapi.NewWithOptions(svc, httpapi.Options{ResourceHandler: adapter}).ServeHTTP(recorder, request)
	return recorder
}

func invokeFile(t *testing.T, svc *core.Service, actor core.Actor, name string, input any) any {
	t.Helper()
	result, err := svc.Execute(context.Background(), actor, name, mustJSON(input))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return result
}

func TestFileAdapterSQLiteDefaultTrashAndRestore(t *testing.T) {
	svc, _, actor, dir := fileFixture(t)
	settings := invokeFile(t, svc, actor, "fs_trash_settings", map[string]any{"root": "workspace"}).(files.TrashSettings)
	if !settings.Enabled || settings.Root != "workspace" {
		t.Fatalf("empty SQLite metadata must default to enabled trash: %+v", settings)
	}
	invokeFile(t, svc, actor, "fs_create", map[string]any{"root": "workspace", "path": "recycled.txt", "content": "durable recycle fixture"})
	waitFileJob(t, svc, actor, invokeFile(t, svc, actor, "fs_delete", map[string]any{"root": "workspace", "path": "recycled.txt"}))
	if _, err := os.Stat(filepath.Join(dir, "workspace", "recycled.txt")); !os.IsNotExist(err) {
		t.Fatal("delete did not remove the original path", err)
	}
	trash := invokeFile(t, svc, actor, "fs_trash_list", map[string]any{"root": "workspace"}).([]files.Trash)
	if len(trash) != 1 || trash[0].ActorID != actor.ID || trash[0].State != "trashed" {
		t.Fatalf("durable trash record missing: %+v", trash)
	}
	waitFileJob(t, svc, actor, invokeFile(t, svc, actor, "fs_restore", map[string]any{"root": "workspace", "id": trash[0].ID}))
	content, err := os.ReadFile(filepath.Join(dir, "workspace", "recycled.txt"))
	if err != nil || string(content) != "durable recycle fixture" {
		t.Fatal("restore did not recover the original content", err)
	}
	settings = invokeFile(t, svc, actor, "fs_trash_configure", map[string]any{"root": "workspace", "enabled": false}).(files.TrashSettings)
	if settings.Enabled || settings.ActorID != actor.ID {
		t.Fatalf("explicit trash setting was not persisted: %+v", settings)
	}
	waitFileJob(t, svc, actor, invokeFile(t, svc, actor, "fs_delete", map[string]any{"root": "workspace", "path": "recycled.txt"}))
	if records := invokeFile(t, svc, actor, "fs_trash_list", map[string]any{"root": "workspace"}).([]files.Trash); len(records) != 0 {
		t.Fatal("disabled trash retained a recycle record", records)
	}
}

func TestFileAdapterFiltersRootsAndRejectsBeforeDestinationWrite(t *testing.T) {
	svc, adapter, admin, _ := fileFixture(t)
	extra := t.TempDir()
	if err := os.WriteFile(filepath.Join(extra, "secret"), []byte("do not copy"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := adapter.files.AddRoot(files.RootSpec{ID: "other", Path: extra}); err != nil {
		t.Fatal(err)
	}
	created, err := svc.CreateToken(context.Background(), admin, core.TokenRequest{Name: "workspace only", Scopes: []string{"files:read", "files:write", "tasks:read"}, Resources: []string{"fs:workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := svc.AuthenticateToken(context.Background(), created.Token)
	if err != nil {
		t.Fatal(err)
	}
	roots := invokeFile(t, svc, actor, "fs_roots", map[string]any{}).([]map[string]any)
	if len(roots) != 1 || roots[0]["id"] != "workspace" {
		t.Fatalf("unauthorized root disclosed: %v", roots)
	}
	_, err = svc.Execute(context.Background(), actor, "fs_copy", mustJSON(map[string]any{"root": "other", "path": "secret", "destination_root": "workspace", "destination_path": "copied"}))
	if core.ErrorCode(err) != "forbidden" {
		t.Fatalf("unauthorized copy: %v", err)
	}
	_, err = svc.Execute(context.Background(), actor, "fs_read", mustJSON(map[string]any{"root": "other", "path": "missing"}))
	if core.ErrorCode(err) != "forbidden" {
		t.Fatalf("unauthorized path existence leak: %v", err)
	}
	if listing := invokeFile(t, svc, actor, "fs_list", map[string]any{"root": "workspace"}).(files.Listing); listing.Total != 0 {
		t.Fatalf("denied copy modified destination: %+v", listing)
	}
}

func TestFileAdapterUploadUsesActorRootOffsetChecksumAndAudit(t *testing.T) {
	svc, adapter, admin, dir := fileFixture(t)
	data := []byte("real streamed file\x00content")
	upload := invokeFile(t, svc, admin, "fs_upload_begin", map[string]any{"root": "workspace", "path": "result.bin", "size": len(data)}).(files.Upload)
	chunk := invokeFile(t, svc, admin, "fs_upload_chunk", map[string]any{"root": "workspace", "id": upload.ID, "offset": 0, "data_base64": base64.StdEncoding.EncodeToString(data)}).(files.Upload)
	if chunk.Offset != int64(len(data)) {
		t.Fatalf("upload offset: %+v", chunk)
	}
	_, err := svc.Execute(context.Background(), admin, "fs_upload_chunk", mustJSON(map[string]any{"root": "workspace", "id": upload.ID, "offset": 0, "data_base64": base64.StdEncoding.EncodeToString(data)}))
	if core.ErrorCode(err) != "conflict" {
		t.Fatalf("duplicate chunk accepted: %v", err)
	}
	job := invokeFile(t, svc, admin, "fs_upload_commit", map[string]any{"root": "workspace", "id": upload.ID}).(map[string]any)
	deadline := time.Now().Add(10 * time.Second)
	for {
		state := invokeFile(t, svc, admin, "tasks_get", map[string]any{"id": job["id"]}).(map[string]any)
		if state["status"] == "succeeded" {
			break
		}
		if state["status"] == "failed" || time.Now().After(deadline) {
			t.Fatalf("upload commit: %+v", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	actual, err := os.ReadFile(filepath.Join(dir, "workspace", "result.bin"))
	if err != nil || string(actual) != string(data) {
		t.Fatal("upload did not create expected real file", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/files/download?root=workspace&path=result.bin", nil)
	recorder := serveFileRequest(t, svc, adapter, request, "")
	if recorder.Code != 200 || recorder.Body.String() != string(data) {
		t.Fatalf("streamed download: %d %q", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("download is not an attachment")
	}
	partial := invokeFile(t, svc, admin, "fs_download", map[string]any{"root": "workspace", "path": "result.bin", "offset": 5, "length": 4}).(map[string]any)
	decoded, err := base64.StdEncoding.DecodeString(partial["data_base64"].(string))
	if err != nil || string(decoded) != string(data[5:9]) {
		t.Fatal("MCP bounded download differs", partial, err)
	}
	audit := invokeFile(t, svc, admin, "audit_list", map[string]any{"limit": 100})
	raw, _ := json.Marshal(audit)
	if !strings.Contains(string(raw), "fs_upload_chunk") || !strings.Contains(string(raw), "fs_download") {
		t.Fatal("file transfers not audited", string(raw))
	}
	if strings.Contains(string(raw), base64.StdEncoding.EncodeToString(data)) {
		t.Fatal("audit disclosed file content")
	}
}

func TestFileAdapterStreamingChunkChecksRootAndSizeBeforeCommit(t *testing.T) {
	svc, adapter, admin, dir := fileFixture(t)
	upload := invokeFile(t, svc, admin, "fs_upload_begin", map[string]any{"root": "workspace", "path": "large", "size": 5 << 20}).(files.Upload)
	tooLarge := strings.NewReader(strings.Repeat("x", (4<<20)+1))
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/files/uploads/"+upload.ID+"/chunk?root=workspace&offset=0", tooLarge)
	recorder := serveFileRequest(t, svc, adapter, request, "")
	if recorder.Code < 400 {
		t.Fatalf("over-budget chunk accepted: %d", recorder.Code)
	}
	status := invokeFile(t, svc, admin, "fs_upload_status", map[string]any{"root": "workspace", "id": upload.ID}).(files.Upload)
	if status.Offset != 0 {
		t.Fatalf("rejected chunk changed upload offset: %+v", status)
	}
	if _, err := os.Stat(filepath.Join(dir, "workspace", "large")); !os.IsNotExist(err) {
		t.Fatal("uncommitted transfer created a public file", err)
	}
	created, err := svc.CreateToken(context.Background(), admin, core.TokenRequest{Name: "another actor", Scopes: []string{"files:write"}, Resources: []string{"fs:workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPatch, "/api/v1/files/uploads/"+upload.ID+"/chunk?root=workspace&offset=0", strings.NewReader("x"))
	recorder = serveFileRequest(t, svc, adapter, request, created.Token)
	if recorder.Code != 403 {
		t.Fatalf("another actor continued upload: %d %s", recorder.Code, recorder.Body.String())
	}
}
