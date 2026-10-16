package plugins

import (
	"bytes"
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
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/httpapi"
)

type databaseImportHTTPStreamer struct {
	received []byte
	called   bool
}

func (s *databaseImportHTTPStreamer) Validate(agent.Request) error { return nil }
func (s *databaseImportHTTPStreamer) Run(context.Context, agent.Request) (agent.Result, error) {
	return agent.Result{}, errors.New("import upload must use Stream")
}
func (s *databaseImportHTTPStreamer) Stream(_ context.Context, request agent.Request, streams agent.IO) (agent.Result, error) {
	s.called = true
	var input databaseImportStreamInput
	if err := json.Unmarshal(request.Input, &input); err != nil {
		return agent.Result{}, err
	}
	body, err := io.ReadAll(streams.Stdin)
	if err != nil {
		return agent.Result{}, err
	}
	s.received = body
	imported := DatabaseImport{ID: input.ImportID, InstanceID: input.InstanceID, Database: input.Database, Engine: "mariadb", State: "uploaded", Bytes: int64(len(body)), BytesReceived: int64(len(body)), SHA256: input.ExpectedSHA256, DatabaseRevision: input.DatabaseRevision, Revision: 2, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	raw, _ := json.Marshal(imported)
	return agent.Result{Operation: request.Operation, ExitCode: 0, Data: raw}, nil
}

type databaseImportTrackingBody struct {
	reader *bytes.Reader
	reads  int
}

func (b *databaseImportTrackingBody) Read(data []byte) (int, error) {
	b.reads++
	return b.reader.Read(data)
}
func (b *databaseImportTrackingBody) Close() error { return nil }

func TestDatabaseHTTPImportUsesSharedAuthorizationBeforeReadingBody(t *testing.T) {
	service, err := core.New(filepath.Join(t.TempDir(), "core"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	secret, err := os.ReadFile(service.SetupSecretFile())
	if err != nil {
		t.Fatal(err)
	}
	admin, err := service.Setup(context.Background(), strings.TrimSpace(string(secret)), "admin", "database import test credential")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(service.Runtime("plugins"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.RegisterProvider(manager); err != nil {
		t.Fatal(err)
	}
	streamer := &databaseImportHTTPStreamer{}
	manager.executor = streamer
	api := httpapi.NewWithOptions(service, httpapi.Options{ResourceHandler: manager.DatabaseResourceHandler(service)})

	resource := DatabaseResource("mariadb-target", "appdb")
	allowed, err := service.CreateToken(context.Background(), admin, core.TokenRequest{Name: "import resource", Scopes: []string{"databases:restore"}, Resources: []string{resource}})
	if err != nil {
		t.Fatal(err)
	}
	denied, err := service.CreateToken(context.Background(), admin, core.TokenRequest{Name: "other database", Scopes: []string{"databases:restore"}, Resources: []string{DatabaseResource("mariadb-target", "otherdb")}})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("external native dump")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	path := "/api/v1/databases/backups/imports?instance_id=mariadb-target&database=appdb&expected_revision=9"
	request := func(token string, requestBody io.ReadCloser) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path, nil)
		r.Body = requestBody
		r.ContentLength = int64(len(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/octet-stream")
		r.Header.Set("X-Content-SHA256", digest)
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		return w
	}

	tracking := &databaseImportTrackingBody{reader: bytes.NewReader(body)}
	response := request(denied.Token, tracking)
	if response.Code != http.StatusForbidden || tracking.reads != 0 || streamer.called {
		t.Fatalf("unscoped request was consumed or streamed: status=%d reads=%d called=%t", response.Code, tracking.reads, streamer.called)
	}
	response = request(allowed.Token, io.NopCloser(bytes.NewReader(body)))
	if response.Code != http.StatusCreated || !streamer.called || !bytes.Equal(streamer.received, body) {
		t.Fatalf("authorized upload did not cross the shared service path: status=%d called=%t bytes=%q body=%s", response.Code, streamer.called, streamer.received, response.Body.String())
	}
	var output map[string]any
	if err = json.Unmarshal(response.Body.Bytes(), &output); err != nil {
		t.Fatal("upload receipt was not valid JSON")
	}
	id, _ := output["id"].(string)
	if output["sha256"] != digest || output["database_revision"] != float64(9) || id == "" || strings.HasPrefix(id, "dbbackup_") {
		t.Fatal("HTTP returned an unsafe or backup-shaped import receipt")
	}
}
