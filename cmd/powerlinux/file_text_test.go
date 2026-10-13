package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/files"
)

func TestLargeTextStreamKeepsDiskEncodingHistoryAndIdempotencyBodyBinding(t *testing.T) {
	svc, adapter, admin, dir := fileFixture(t)
	content := "中文 🧪 " + strings.Repeat("large text ", 12000)
	endpoint := "/api/v1/files/text?root=workspace&path=large.txt&encoding=gb18030"
	send := func(body string, key string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPut, endpoint, strings.NewReader(body))
		request.Header.Set("Content-Type", "text/plain; charset=utf-8")
		request.Header.Set("Idempotency-Key", key)
		return serveFileRequest(t, svc, adapter, request, "")
	}
	first := send(content, "large-text-intent-1")
	if first.Code != 200 {
		t.Fatalf("large text save: %d %s", first.Code, first.Body.String())
	}
	var out struct {
		Data struct {
			SHA256 string `json:"sha256"`
		}
	}
	if json.Unmarshal(first.Body.Bytes(), &out) != nil || out.Data.SHA256 == "" {
		t.Fatal("no disk checksum", first.Body.String())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "workspace", "large.txt"))
	if err != nil || len(raw) < 64<<10 || string(raw) == content {
		t.Fatal("large body did not preserve selected non-UTF8 disk encoding", err)
	}
	read := invokeFile(t, svc, admin, "fs_read", map[string]any{"root": "workspace", "path": "large.txt", "encoding": "gb18030", "limit": 2 << 20}).(files.Text)
	if read.Content != content || read.SHA256 != out.Data.SHA256 {
		t.Fatal("disk decoding/checksum differs from response")
	}
	repeat := send(content, "large-text-intent-1")
	if repeat.Code != 200 {
		t.Fatalf("same intent replay: %d %s", repeat.Code, repeat.Body.String())
	}
	changed := send(content+"changed", "large-text-intent-1")
	if changed.Code != 409 || !strings.Contains(changed.Body.String(), "idempotency_conflict") {
		t.Fatalf("key was not bound to actual body: %d %s", changed.Code, changed.Body.String())
	}
	rawAfter, _ := os.ReadFile(filepath.Join(dir, "workspace", "large.txt"))
	if string(rawAfter) != string(raw) {
		t.Fatal("rejected intent modified actual file")
	}
	endpoint += "&expected_hash=" + out.Data.SHA256
	updated := send(content+"updated", "large-text-intent-2")
	if updated.Code != 200 {
		t.Fatalf("large update: %d %s", updated.Code, updated.Body.String())
	}
	history := invokeFile(t, svc, admin, "fs_history", map[string]any{"root": "workspace", "path": "large.txt"}).([]files.Revision)
	if len(history) != 2 || history[1].SHA256 != out.Data.SHA256 || history[0].ActorID != admin.ID {
		t.Fatalf("large editing bypassed content history: %+v", history)
	}
}

type watchedBody struct {
	reads  int
	source io.Reader
}

func (r *watchedBody) Read(buf []byte) (int, error) { r.reads++; return r.source.Read(buf) }

func TestLargeTextStreamRejectsUnauthorizedBeforeReadingAndOverBudgetWithoutPublishing(t *testing.T) {
	svc, adapter, admin, dir := fileFixture(t)
	created, err := svc.CreateToken(context.Background(), admin, core.TokenRequest{Name: "read only files", Scopes: []string{"files:read"}, Resources: []string{"fs:workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	body := &watchedBody{source: strings.NewReader(strings.Repeat("x", 2<<20))}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/files/text?root=workspace&path=denied", body)
	recorder := serveFileRequest(t, svc, adapter, request, created.Token)
	if recorder.Code != 403 || body.reads != 0 {
		t.Fatalf("unauthorized body consumed: status %d reads %d", recorder.Code, body.reads)
	}
	request = httptest.NewRequest(http.MethodPut, "/api/v1/files/text?root=workspace&path=too-large", strings.NewReader(strings.Repeat("x", (2<<20)+1)))
	recorder = serveFileRequest(t, svc, adapter, request, "")
	if recorder.Code != 413 {
		t.Fatalf("text budget: %d %s", recorder.Code, recorder.Body.String())
	}
	if _, err = os.Stat(filepath.Join(dir, "workspace", "too-large")); !os.IsNotExist(err) {
		t.Fatal("rejected large text published a file", err)
	}
}
