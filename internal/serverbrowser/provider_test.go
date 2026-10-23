package serverbrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/httpapi"
)

type providerFixture struct {
	service *core.Service
	admin   core.Actor
	browser *Provider
}

func setupProvider(t *testing.T, opts Options) providerFixture {
	t.Helper()
	service, err := core.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	secret, err := os.ReadFile(service.SetupSecretFile())
	if err != nil {
		t.Fatal(err)
	}
	admin, err := service.Setup(context.Background(), strings.TrimSpace(string(secret)), "admin", "browser fixture secure password")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := New(service.Runtime("serverbrowser"), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.RegisterProvider(provider); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(provider.Close)
	return providerFixture{service, admin, provider}
}
func call(t *testing.T, f providerFixture, a core.Actor, name string, in any) any {
	t.Helper()
	out, err := f.service.Execute(context.Background(), a, name, encoded(in))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}
func createActor(t *testing.T, f providerFixture, scopes, resources []string) (core.Actor, core.TokenCreated) {
	t.Helper()
	created, err := f.service.CreateToken(context.Background(), f.admin, core.TokenRequest{Name: "isolated browser", Kind: "mcp", Scopes: scopes, Resources: resources})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := f.service.AuthenticateToken(context.Background(), created.Token)
	if err != nil {
		t.Fatal(err)
	}
	return actor, created
}
func readSession(t *testing.T, value any) Session {
	t.Helper()
	var session Session
	if err := json.Unmarshal(encoded(value), &session); err != nil || session.ID == "" {
		t.Fatalf("missing session: %v %v", value, err)
	}
	return session
}
func request(t *testing.T, server *httptest.Server, path, method string, body any, cookie *http.Cookie, headers http.Header) (int, []byte, http.Header) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw = encoded(body)
	}
	r, err := http.NewRequest(method, server.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for key, values := range headers {
		r.Header[key] = values
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	result, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	data, _ := io.ReadAll(result.Body)
	return result.StatusCode, data, result.Header
}
func TestProviderRequiresExplicitScopeAndConcreteServerResource(t *testing.T) {
	f := setupProvider(t, Options{})
	noScope, _ := createActor(t, f, []string{"system:read"}, []string{"*"})
	wrongResource, _ := createActor(t, f, []string{Scope}, []string{"browser:another"})
	for _, a := range []core.Actor{noScope, wrongResource} {
		if _, err := f.service.Execute(context.Background(), a, "browser_status", encoded(map[string]any{})); core.ErrorCode(err) != "forbidden" {
			t.Fatalf("browser authority leaked: %v", err)
		}
	}
	correct, _ := createActor(t, f, []string{Scope}, []string{Resource})
	status := call(t, f, correct, "browser_status", map[string]any{})
	var decoded struct {
		Sessions []Session `json:"sessions"`
	}
	_ = json.Unmarshal(encoded(status), &decoded)
	if len(decoded.Sessions) != 0 {
		t.Fatal("new identity inherited browser sessions")
	}
	forged := httptest.NewRecorder()
	f.browser.HandleResource(forged, httptest.NewRequest("GET", "/api/v1/browser/sessions/forged/tabs/tab/frame", nil), f.admin)
	if forged.Code != 401 {
		t.Fatal("resource hook accepted supplied Actor without authenticated context", forged.Code)
	}
}
func TestProviderRealBrowserOwnershipCSRFRevocationAndPrivateAudit(t *testing.T) {
	opts := Options{Engine: EngineOptions{UnsafeNoSandboxForTests: os.Geteuid() == 0}}
	if path, _ := availableEngine(opts.Engine); path == "" {
		t.Skip("Chromium not installed")
	}
	f := setupProvider(t, opts)
	session := readSession(t, call(t, f, f.admin, "browser_open", map[string]any{"width": 640, "height": 480}))
	if len(session.Tabs) != 1 {
		t.Fatal("no initial browser tab")
	}
	if _, err := f.service.Execute(context.Background(), f.admin, "browser_open", encoded(map[string]any{})); core.ErrorCode(err) != "browser_limit" {
		t.Fatalf("duplicate owner browser admitted: %v", err)
	}
	second, created := createActor(t, f, []string{Scope}, []string{Resource})
	other := readSession(t, call(t, f, second, "browser_open", map[string]any{"width": 640, "height": 480}))
	var status struct {
		Sessions []Session `json:"sessions"`
	}
	_ = json.Unmarshal(encoded(call(t, f, second, "browser_status", map[string]any{})), &status)
	if len(status.Sessions) != 1 || status.Sessions[0].ID != other.ID {
		t.Fatalf("browser list leaked another identity: %+v", status)
	}
	for _, name := range []string{"browser_tabs", "browser_close"} {
		if _, err := f.service.Execute(context.Background(), second, name, encoded(map[string]any{"id": session.ID})); core.ErrorCode(err) != "forbidden" {
			t.Fatalf("cross-owner %s admitted: %v", name, err)
		}
	}
	login, err := f.service.Login(context.Background(), "admin", "browser fixture secure password", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: "pl_session", Value: login.ID}
	server := httptest.NewServer(httpapi.NewWithOptions(f.service, httpapi.Options{ResourceHandler: f.browser}))
	defer server.Close()
	frame := "/api/v1/browser/sessions/" + session.ID + "/tabs/" + session.Tabs[0].ID + "/frame"
	if code, _, _ := request(t, server, frame, "GET", nil, nil, nil); code != 401 {
		t.Fatal("anonymous frame admitted", code)
	}
	if code, _, _ := request(t, server, frame, "GET", nil, nil, http.Header{"Authorization": {"Bearer " + created.Token}}); code != 403 {
		t.Fatal("cross-owner frame admitted", code)
	}
	code, body, headers := request(t, server, frame, "GET", nil, cookie, nil)
	if code != 200 || !bytes.HasPrefix(body, []byte{0xff, 0xd8}) || !strings.Contains(headers.Get("Cache-Control"), "no-store") {
		t.Fatalf("authenticated actual JPEG not private: %d %s", code, body)
	}
	actionPath := "/api/v1/actions/browser_input"
	input := map[string]any{"id": session.ID, "tab_id": session.Tabs[0].ID, "kind": "text", "text": "secret-password-canary"}
	if code, _, _ := request(t, server, actionPath, "POST", input, cookie, nil); code != 403 {
		t.Fatal("browser input without CSRF admitted", code)
	}
	if code, _, _ := request(t, server, actionPath, "POST", input, cookie, http.Header{"X-Csrf-Token": {login.CSRFToken}, "Origin": {"https://attacker.example"}}); code != 403 {
		t.Fatal("browser input cross origin admitted", code)
	}
	if code, _, _ := request(t, server, actionPath, "POST", input, cookie, http.Header{"X-Csrf-Token": {login.CSRFToken}, "Idempotency-Key": {"browser-password-input"}}); code != 200 {
		t.Fatal("real browser input failed", code)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<title>Actual server request</title>")
	}))
	defer origin.Close()
	call(t, f, f.admin, "browser_navigate", map[string]any{"id": session.ID, "tab_id": session.Tabs[0].ID, "url": origin.URL + "/?secret-query-canary"})
	for _, bad := range []string{"file:///etc/passwd", "javascript:alert(1)", "data:text/html,test", "chrome://version", "https://username:password@example.test/"} {
		if _, err := f.service.Execute(context.Background(), f.admin, "browser_navigate", encoded(map[string]any{"id": session.ID, "tab_id": session.Tabs[0].ID, "url": bad})); core.ErrorStatus(err) != 400 {
			t.Fatalf("unsafe address admitted %q: %v", bad, err)
		}
	}
	audit := encoded(call(t, f, f.admin, "audit_list", map[string]any{}))
	if bytes.Contains(audit, []byte("secret-password-canary")) || bytes.Contains(audit, []byte("secret-query-canary")) {
		t.Fatal("browser content leaked into audit")
	}
	otherFrame := "/api/v1/browser/sessions/" + other.ID + "/tabs/" + other.Tabs[0].ID + "/frame"
	if err = f.service.RevokeToken(context.Background(), f.admin, created.ID); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := request(t, server, otherFrame, "GET", nil, nil, http.Header{"Authorization": {"Bearer " + created.Token}}); code != 401 {
		t.Fatal("revoked credential retained screenshot access", code)
	}
	call(t, f, f.admin, "browser_close", map[string]any{"id": session.ID})
	if code, _, _ := request(t, server, frame, "GET", nil, cookie, nil); code != 404 {
		t.Fatal("closed browser retained frame", code)
	}
}
func TestProviderExpiryAndGlobalResize(t *testing.T) {
	opts := Options{Engine: EngineOptions{UnsafeNoSandboxForTests: os.Geteuid() == 0}}
	if path, _ := availableEngine(opts.Engine); path == "" {
		t.Skip("Chromium not installed")
	}
	f := setupProvider(t, opts)
	session := readSession(t, call(t, f, f.admin, "browser_open", map[string]any{"width": 640, "height": 480}))
	session = readSession(t, call(t, f, f.admin, "browser_tab_open", map[string]any{"id": session.ID}))
	session = readSession(t, call(t, f, f.admin, "browser_resize", map[string]any{"id": session.ID, "tab_id": session.Tabs[1].ID, "width": 360, "height": 600}))
	owned, _ := f.browser.get(f.admin, session.ID)
	for _, tab := range session.Tabs {
		image, err := owned.engine.Frame(context.Background(), tab.ID)
		if err != nil {
			t.Fatal(err)
		}
		size, err := decodeJPEGSize(image)
		if err != nil || size[0] != 360 || size[1] != 600 {
			t.Fatalf("tab viewport disagreed with session: %v %v", size, err)
		}
	}
	owned.mu.Lock()
	owned.createdAt = time.Now().Add(-5 * time.Hour)
	profile := owned.engine.profile
	owned.mu.Unlock()
	if _, err := f.service.Execute(context.Background(), f.admin, "browser_tabs", encoded(map[string]any{"id": session.ID})); core.ErrorCode(err) != "not_found" {
		t.Fatal("expired browser remained active", err)
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatal("expired profile retained", err)
	}
	// Expired sessions immediately release the owner slot rather than trapping reconnect.
	replacement := readSession(t, call(t, f, f.admin, "browser_open", map[string]any{"width": 640, "height": 480}))
	if replacement.ID == session.ID {
		t.Fatal("expired browser identity reused")
	}
}
func TestBrowserInputAndURLBounds(t *testing.T) {
	for _, in := range []Input{{Kind: "mouse", Event: "inject", X: 1, Y: 1}, {Kind: "key", Event: "keyDown", Key: "Enter", Modifiers: 16}, {Kind: "text", Text: strings.Repeat("a", 16385)}, {Kind: "wheel", X: 641}, {Kind: "text", Text: "nul\x00byte"}} {
		if validateInput(in, 640, 480) == nil {
			t.Fatalf("unbounded browser input accepted: %+v", in)
		}
	}
	for _, address := range []string{"http://127.0.0.1:8080/", "https://example.test/path?q=yes", "about:blank"} {
		if err := validateURL(address); err != nil {
			t.Fatal("expected server-side URL rejected", err)
		}
	}
	for _, address := range []string{"about:config", "file:///tmp/x", "https://user:pass@example.test/", "http://", "https://example.test/\n"} {
		if validateURL(address) == nil {
			t.Fatal("unsafe address accepted", address)
		}
	}
}

func decodeJPEGSize(data []byte) ([2]int, error) {
	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	return [2]int{config.Width, config.Height}, err
}
