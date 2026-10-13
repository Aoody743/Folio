package sites

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/httpapi"
	"github.com/Aoody743/PowerLinux/internal/mcpbridge"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestWebsiteBackupByteRangesAreBounded(t *testing.T) {
	for _, spec := range []struct {
		header string
		start  int64
		length int64
	}{
		{"bytes=3-25", 3, 23}, {"bytes=5-", 5, 95}, {"bytes=-13", 87, 13}, {"bytes=-1000", 0, 100}, {"bytes=98-999", 98, 2},
	} {
		start, length, err := parseBackupRange(spec.header, 100)
		if err != nil || start != spec.start || length != spec.length {
			t.Fatal("download range did not resolve to the exact owned bytes", spec, start, length, err)
		}
	}
	for _, header := range []string{"bytes=100-", "bytes=2-1", "bytes=0-1,3-4", "bytes=-0", "bytes=+1-2", "bytes=0-9223372036854775808", "items=0-1"} {
		if _, _, err := parseBackupRange(header, 100); core.ErrorCode(err) != "range_not_satisfiable" {
			t.Fatal("invalid range accepted", header, err)
		}
	}
}

func backupServiceFixture(t *testing.T) (*core.Service, core.Actor) {
	t.Helper()
	s, err := core.New(filepath.Join(t.TempDir(), "api"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.RegisterScopes("sites:backup", "files:read"); err != nil {
		t.Fatal(err)
	}
	secret, err := os.ReadFile(s.SetupSecretFile())
	if err != nil {
		t.Fatal(err)
	}
	actor, err := s.Setup(context.Background(), strings.TrimSpace(string(secret)), "backup-admin", "Actual archive fixture password 2026!")
	if err != nil {
		t.Fatal(err)
	}
	return s, actor
}

func TestWebsiteBackupExportRequiresBothScopesAndExactFileResource(t *testing.T) {
	s, admin := backupServiceFixture(t)
	p, err := New(s.Runtime("sites"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterProvider(core.Bindings(p.BackupDownloadBindings())); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct {
		scopes, resources []string
	}{
		{[]string{"sites:backup"}, []string{"site:owned", "fs:site-owned"}},
		{[]string{"files:read"}, []string{"site:owned", "fs:site-owned"}},
		{[]string{"sites:backup", "files:read"}, []string{"site:owned", "fs:site-other"}},
	} {
		token, err := s.CreateToken(context.Background(), admin, core.TokenRequest{Name: "restricted backup fixture", Scopes: spec.scopes, Resources: spec.resources})
		if err != nil {
			t.Fatal(err)
		}
		actor, err := s.AuthenticateToken(context.Background(), token.Token)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"sites_backup_download", "sites_backup_read_chunk"} {
			_, err := s.Execute(context.Background(), actor, name, encode(backupDownloadInput{ID: "owned", BackupID: "b-fixture"}))
			if core.ErrorCode(err) != "forbidden" {
				t.Fatal("partial grants reached archive metadata/Agent instead of denial", name, spec, err)
			}
		}
	}
	_, err = s.Execute(context.Background(), admin, "sites_backup_read_chunk", encode(backupDownloadInput{ID: "owned", BackupID: "b-fixture", Length: maxWebsiteBackupChunk + 1}))
	if core.ErrorStatus(err) != 400 {
		t.Fatal("oversized JSON chunk reached the Agent", err)
	}
}

type backupBuffer struct{ bytes.Buffer }

func (*backupBuffer) Close() error { return nil }

func rootBackupFixture(t *testing.T) (*AgentExecutor, Backup, []byte) {
	t.Helper()
	qaOwnedRootVM(t)
	state, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &AgentExecutor{state: state}
	t.Cleanup(func() { state.Close() })
	public := make([]byte, 257<<10)
	if _, err := rand.Read(public); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "public.bin", Mode: 0640, Size: int64(len(public))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(public); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive.Bytes())
	backup := Backup{ID: "b-fixture", SiteID: "owned", Size: int64(archive.Len()), SHA256: hex.EncodeToString(sum[:]), CreatedAt: time.Now().UTC()}
	if err := state.MkdirAll("backups/owned", 0700); err != nil {
		t.Fatal(err)
	}
	file, err := state.OpenFile("backups/owned/b-fixture.tar.gz", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if err := atomicRoot(state, "backups/owned/b-fixture.json", encode(backupManifest{Version: 1, Site: Site{ID: "owned", Description: "PRIVATE-ROOT-MANIFEST-DO-NOT-EXPORT"}, Backup: backup}), 0600); err != nil {
		t.Fatal(err)
	}
	return e, backup, append([]byte(nil), archive.Bytes()...)
}

func TestQARootWebsiteArchiveStreamIntegrityAndUnsafeFiles(t *testing.T) {
	e, backup, expected := rootBackupFixture(t)
	actor := core.Actor{ID: "root-archive-fixture", Kind: "human"}
	request := request{Actor: actor, ID: backup.SiteID, BackupID: backup.ID}
	sink := &backupBuffer{}
	result, err := e.Stream(context.Background(), agent.Request{ActorID: actor.ID, Operation: "site_backup_artifact", Input: encode(request)}, agent.IO{Stdout: sink, Stdin: io.NopCloser(strings.NewReader(""))})
	if err != nil || result.ExitCode != 0 || !bytes.Equal(sink.Bytes(), expected) || result.Stdout != "" || result.Stderr != "" {
		t.Fatal("owned archive stream lost bytes or entered logs", err)
	}
	// Same size and preserved mtime still changes ctime, invalidating the
	// checksum cache. No corrupted byte may escape before rejection.
	file, err := e.state.OpenFile("backups/owned/b-fixture.tar.gz", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{expected[0] ^ 0xff}, 0); err != nil {
		t.Fatal(err)
	}
	file.Close()
	sink = &backupBuffer{}
	if _, err := e.Stream(context.Background(), agent.Request{ActorID: actor.ID, Operation: "site_backup_artifact", Input: encode(request)}, agent.IO{Stdout: sink}); core.ErrorCode(err) != "checksum_mismatch" || sink.Len() != 0 {
		t.Fatal("changed archive reused a stale checksum or exported bytes", err)
	}
	if err := e.state.Remove("backups/owned/b-fixture.tar.gz"); err != nil {
		t.Fatal(err)
	}
	base := e.state.Name()
	if err := syscall.Mkfifo(filepath.Join(base, "backups/owned/b-fixture.tar.gz"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, _, err := e.openVerifiedBackup(ctx, request); core.ErrorCode(err) != "artifact_invalid" {
		t.Fatal("FIFO archive was not rejected promptly", err)
	}
}

type backupPATTransport struct{ token string }

func (tr backupPATTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+tr.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestQARootWebsiteArchiveActualUnixHTTPAndMCPChunks(t *testing.T) {
	e, backup, expected := rootBackupFixture(t)
	s, admin := backupServiceFixture(t)
	socket := filepath.Join(t.TempDir(), "agent.sock")
	server, err := agent.NewUnixServer(socket, []uint32{uint32(os.Getuid())}, e)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	defer server.Close()
	go server.Serve(ctx)
	client, err := agent.NewUnixClient(socket, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	p, err := New(s.Runtime("sites"), Options{Agent: client})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterProvider(core.Bindings(p.BackupDownloadBindings())); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateToken(ctx, admin, core.TokenRequest{Name: "owned archive read", Kind: "mcp", Scopes: []string{"sites:backup", "files:read"}, Resources: []string{"site:owned", "fs:site-owned"}})
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(httpapi.NewWithOptions(s, httpapi.Options{ResourceHandler: p.BackupResourceHandler(s)}))
	defer api.Close()
	url := api.URL + "/api/v1/sites/backups/download?id=owned&backup_id=b-fixture"
	for _, spec := range []struct {
		method, byteRange  string
		status, start, end int
	}{{"GET", "", 200, 0, len(expected)}, {"HEAD", "", 200, 0, 0}, {"GET", "bytes=3-25", 206, 3, 26}, {"GET", "bytes=-13", 206, len(expected) - 13, len(expected)}} {
		r, _ := http.NewRequestWithContext(ctx, spec.method, url, nil)
		r.Header.Set("Authorization", "Bearer "+token.Token)
		r.Header.Set("Range", spec.byteRange)
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != spec.status || !bytes.Equal(actual, expected[spec.start:spec.end]) || response.Header.Get("ETag") != `"sha256-`+backup.SHA256+`"` {
			t.Fatal("actual UID Unix/HTTP archive bytes or headers differ", spec, response.StatusCode, err)
		}
	}
	native, err := mcpbridge.NewHTTP(s, mcpbridge.HTTPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mcpServer := httptest.NewServer(native)
	defer mcpServer.Close()
	sdk := mcp.NewClient(&mcp.Implementation{Name: "owned-site-archive-sdk", Version: "1"}, nil)
	session, err := sdk.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: mcpServer.URL + "/mcp", HTTPClient: &http.Client{Transport: backupPATTransport{token.Token}}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	params := map[string]any{"id": "owned", "backup_id": "b-fixture", "offset": 3, "length": 32 << 10}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "sites_backup_read_chunk", Arguments: params})
	if err != nil || result.IsError {
		t.Fatal("actual MCP/Unix archive chunk failed", err)
	}
	raw, _ := json.Marshal(result.StructuredContent)
	var envelope struct {
		Data struct {
			Data string `json:"data"`
			Next int64  `json:"next_offset"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	chunk, err := base64.StdEncoding.DecodeString(envelope.Data.Data)
	if err != nil || !bytes.Equal(chunk, expected[3:3+(32<<10)]) || envelope.Data.Next != 3+(32<<10) {
		t.Fatal("native MCP chunk did not match the real HTTP archive bytes", err)
	}
	params["length"] = (32 << 10) + 1
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "sites_backup_read_chunk", Arguments: params})
	if err == nil && !result.IsError {
		t.Fatal("MCP accepted an oversized binary-in-JSON chunk")
	}
	if err := s.RevokeToken(ctx, admin, token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "sites_backup_read_chunk", Arguments: params}); err == nil {
		t.Fatal("revoked MCP credential retained archive access")
	}
}
