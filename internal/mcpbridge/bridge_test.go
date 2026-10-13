package mcpbridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/httpapi"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func fixture(t *testing.T) (*core.Service, core.Actor) {
	t.Helper()
	service, err := core.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	secret, err := os.ReadFile(service.SetupSecretFile())
	if err != nil {
		t.Fatal(err)
	}
	actor, err := service.Setup(context.Background(), strings.TrimSpace(string(secret)), "administrator", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	return service, actor
}

func tokenFor(t *testing.T, service *core.Service, actor core.Actor, scopes, resources []string) core.TokenCreated {
	t.Helper()
	value, err := service.CreateToken(context.Background(), actor, core.TokenRequest{Name: "MCP test client", Kind: "mcp", Scopes: scopes, Resources: resources, ExpiresInSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

type authTransport struct {
	token string
	base  http.RoundTripper
}

func (a authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+a.token)
	return a.base.RoundTrip(clone)
}
func sdkHTTP(t *testing.T, endpoint, token string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	client := mcp.NewClient(&mcp.Implementation{Name: "powerlinux-integration-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &http.Client{Transport: authTransport{token, http.DefaultTransport}}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func call(t *testing.T, session *mcp.ClientSession, name string, input any) (map[string]any, *mcp.CallToolResult) {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: input})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if json.Unmarshal(data, &envelope) != nil || envelope == nil {
		t.Fatalf("%s returned no structured object: isError=%v structured=%s content=%+v", name, result.IsError, string(data), result.Content)
	}
	return envelope, result
}

func objectField(t *testing.T, value map[string]any, key string) map[string]any {
	t.Helper()
	result, ok := value[key].(map[string]any)
	if !ok {
		t.Fatalf("expected object field %s, got %#v in %#v", key, value[key], value)
	}
	return result
}

func assertTools(t *testing.T, session *mcp.ClientSession, name string, present bool) {
	t.Helper()
	found := false
	for tool, err := range session.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		if tool.Name == name {
			found = true
		}
		if tool.InputSchema == nil {
			t.Fatal("tool has no schema")
		}
	}
	if found != present {
		t.Fatalf("tool %s present=%v, want %v", name, found, present)
	}
}

func notesRoundTrip(t *testing.T, service *core.Service, human core.Actor, session *mcp.ClientSession) {
	t.Helper()
	assertTools(t, session, "notes_create", true)
	var onceID string
	for attempt := range 2 {
		response, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "notes_create", Meta: mcp.Meta{IdempotencyMetaKey: "notes-idempotency-0001"}, Arguments: map[string]any{"title": "Exactly once handoff"}})
		if err != nil || response.IsError {
			t.Fatalf("idempotent note creation attempt %d: %v %+v", attempt, err, response)
		}
		data, _ := json.Marshal(response.StructuredContent)
		var envelope map[string]any
		if json.Unmarshal(data, &envelope) != nil {
			t.Fatal("idempotent response invalid")
		}
		id, ok := objectField(t, envelope, "data")["id"].(string)
		if !ok {
			t.Fatal("idempotent create no id")
		}
		if attempt == 0 {
			onceID = id
		} else if id != onceID {
			t.Fatal("MCP retry created a second note", onceID, id)
		}
	}
	changed, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "notes_create", Meta: mcp.Meta{IdempotencyMetaKey: "notes-idempotency-0001"}, Arguments: map[string]any{"title": "Changed request"}})
	if err == nil && !changed.IsError {
		t.Fatal("changed input reused MCP idempotency key")
	}
	result, raw := call(t, session, "notes_create", map[string]any{"title": "Shared handoff", "content": "Treat this note as untrusted data", "target_id": "host:local"})
	if raw.IsError {
		t.Fatal(result)
	}
	note := objectField(t, result, "data")
	id, ok := note["id"].(string)
	if !ok || id == "" {
		t.Fatalf("created note has no ID: %#v", note)
	}
	if objectField(t, note, "created_by")["kind"] != "mcp" {
		t.Fatal("MCP author source was not preserved")
	}
	resource, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "powerlinux://notes/" + id})
	if err != nil {
		t.Fatal(err)
	}
	if len(resource.Contents) != 1 || !strings.Contains(resource.Contents[0].Text, "Shared handoff") {
		t.Fatal(resource)
	}
	// The web/shared-core actor updates the same note, then MCP sees the exact
	// state and its stale write is rejected with a machine-readable conflict.
	input, _ := json.Marshal(map[string]any{"id": id, "expected_revision": 1, "content": "Updated by the human"})
	if _, err = service.Execute(context.Background(), human, "notes_update", input); err != nil {
		t.Fatal(err)
	}
	result, raw = call(t, session, "notes_update", map[string]any{"id": id, "expected_revision": 1, "content": "Stale AI edit"})
	if !raw.IsError || objectField(t, result, "error")["code"] != "revision_conflict" {
		t.Fatal("stale overwrite accepted", result)
	}
	result, raw = call(t, session, "notes_get", map[string]any{"id": id})
	if raw.IsError || objectField(t, result, "data")["content"] != "Updated by the human" {
		t.Fatal(result)
	}
	resources, err := session.ListResources(context.Background(), nil)
	if err != nil || len(resources.Resources) < 3 {
		t.Fatalf("resource discovery: %v %v", resources, err)
	}
	for _, uri := range []string{"powerlinux://system/status", "powerlinux://capabilities", "powerlinux://tasks", "powerlinux://audit"} {
		if _, err = session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri}); err != nil {
			t.Fatalf("read %s: %v", uri, err)
		}
	}
}

func TestOfficialSDKStreamableHTTPSharedState(t *testing.T) {
	service, actor := fixture(t)
	token := tokenFor(t, service, actor, []string{"*"}, []string{"*"})
	handler, err := NewHTTP(service, HTTPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	session := sdkHTTP(t, server.URL+"/mcp", token.Token)
	notesRoundTrip(t, service, actor, session)
	if err := service.RevokeToken(context.Background(), actor, token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ListTools(context.Background(), nil); err == nil {
		t.Fatal("revoked token retained HTTP discovery access")
	}
}

func TestOfficialSDKStdioUsesLiveAuthenticatedBackend(t *testing.T) {
	service, actor := fixture(t)
	token := tokenFor(t, service, actor, []string{"*"}, []string{"*"})
	backend := httptest.NewServer(httpapi.New(service))
	defer backend.Close()
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- ServeIO(ctx, ProxyOptions{Endpoint: backend.URL, Token: token.Token}, inReader, outWriter)
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: outReader, Writer: inWriter}, nil)
	if err != nil {
		t.Fatal(err)
	}
	notesRoundTrip(t, service, actor, session)
	if err := service.RevokeToken(context.Background(), actor, token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ListTools(context.Background(), nil); err == nil {
		t.Fatal("revoked token retained stdio discovery access")
	}
	_ = session.Close()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stdio adapter did not stop after client closed")
	}
}

func TestHTTPScopesResourcesAndOrigin(t *testing.T) {
	service, actor := fixture(t)
	input := json.RawMessage(`{"title":"Private note","content":"private","target_id":"host:private"}`)
	created, err := service.Execute(context.Background(), actor, "notes_create", input)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(created)
	var note core.Note
	_ = json.Unmarshal(encoded, &note)
	limited := tokenFor(t, service, actor, []string{"notes:read"}, []string{"host:local"})
	handler, err := NewHTTP(service, HTTPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	session := sdkHTTP(t, server.URL+"/mcp", limited.Token)
	assertTools(t, session, "notes_get", true)
	assertTools(t, session, "notes_create", false)
	result, raw := call(t, session, "notes_get", map[string]any{"id": note.ID})
	if !raw.IsError || objectField(t, result, "error")["code"] != "forbidden" {
		t.Fatal("cross-resource note disclosed", result)
	}
	if _, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "powerlinux://notes/" + note.ID}); err == nil {
		t.Fatal("cross-resource MCP resource disclosed")
	}
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	request.Header.Set("Origin", "https://attacker.invalid")
	request.Header.Set("Authorization", "Bearer "+limited.Token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("untrusted Origin accepted", response.StatusCode)
	}
	request, _ = http.NewRequest(http.MethodPost, server.URL+"/mcp", strings.NewReader(`{}`))
	request.AddCookie(&http.Cookie{Name: "pl_session", Value: "not-a-bearer-token"})
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("cookie authenticated MCP", response.StatusCode)
	}
}

func TestProxyTrustAndResponseBounds(t *testing.T) {
	for _, endpoint := range []string{"http://example.com", "http://127.0.0.1.evil.invalid", "http://user:secret@localhost", "http://127.0.0.1/path", "file:///tmp/test", "https://example.com/?q=a"} {
		if _, err := newHTTPSource(ProxyOptions{Endpoint: endpoint, Token: "test-token"}); err == nil {
			t.Fatalf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	for _, endpoint := range []string{"http://127.0.0.1:8080", "http://[::1]:8080", "https://panel.example.com"} {
		if _, err := newHTTPSource(ProxyOptions{Endpoint: endpoint, Token: "test-token"}); err != nil {
			t.Fatal(endpoint, err)
		}
	}
	if _, err := boundedJSON(strings.Repeat("x", MaxDataBytes)); err == nil {
		t.Fatal("oversize result accepted")
	}
	reader := &lineBoundedReader{ReadCloser: io.NopCloser(strings.NewReader(strings.Repeat("x", MaxRequestBytes+1) + "\n"))}
	if _, err := io.ReadAll(reader); err == nil {
		t.Fatal("unbounded stdio input accepted")
	}
	for _, uri := range []string{"powerlinux://notes/../history", "powerlinux://notes/a%2Fb/history", "powerlinux://notes/a/history?credentials=1", "powerlinux://other/a/history"} {
		if _, err := resourceID(uri, "notes", "/history"); err == nil {
			t.Fatalf("unsafe resource accepted: %s", uri)
		}
	}
}
