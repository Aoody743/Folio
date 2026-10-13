package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/mcpbridge"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBrowserGatewayCookieAndAudienceBoundNativeBypass(t *testing.T) {
	f := newFixture(t)
	input := map[string]any{"password": "a secure long password", "enabled": true, "basic_username": "entry-user", "basic_password": "separate browser entry password", "expected_revision": 0}
	code, out, _ := f.request(t, "POST", "/api/v1/actions/settings_basic_auth_update", input, map[string]string{"X-CSRF-Token": f.csrf})
	if code != 200 {
		t.Fatal(code, out)
	}
	api := New(f.s)
	var otherCalls atomic.Int32
	mux := http.NewServeMux()
	mux.Handle("/api/v1/", api)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { otherCalls.Add(1); w.WriteHeader(204) })
	front := NewFrontdoor(f.s, FrontdoorOptions{MCPAudience: "https://panel.example/mcp"}, mux)
	request := func(path, token string, basic bool, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://panel.example"+path, nil)
		r.RemoteAddr = "192.0.2.10:31337"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if basic {
			r.SetBasicAuth("entry-user", "separate browser entry password")
		}
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		front.ServeHTTP(w, r)
		return w
	}
	if w := request("/", "", false); w.Code != 401 || !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Basic ") {
		t.Fatal("static entry bypassed gateway")
	}
	if w := request("/.well-known/oauth-protected-resource/mcp", "", false); w.Code != 204 {
		t.Fatal("OAuth discovery blocked")
	}
	w := request("/", "", true)
	if w.Code != 204 {
		t.Fatal("valid Basic gateway rejected", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].MaxAge != 3600 {
		t.Fatal("unsafe gateway cookie")
	}
	if w = request("/api/v1/notes", "", false, cookies[0]); w.Code != 401 {
		t.Fatal("gateway cookie became a panel session", w.Code)
	}
	if w = request("/api/v1/notes", "", false, cookies[0], f.cookie); w.Code != 200 {
		t.Fatal("gateway and panel session failed", w.Code)
	}
	pat, err := f.s.CreateToken(context.Background(), f.actor, core.TokenRequest{Name: "API automation", Scopes: []string{"notes:read"}, Resources: []string{"host:local"}})
	if err != nil {
		t.Fatal(err)
	}
	if w = request("/api/v1/notes", pat.Token, false); w.Code != 200 {
		t.Fatal("valid API PAT blocked", w.Code)
	}
	if w = request("/", pat.Token, false); w.Code != 401 {
		t.Fatal("API token opened browser static entry")
	}
	before := otherCalls.Load()
	if w = request("/mcp", "invalid-token", false); w.Code != 401 || !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Bearer resource_metadata=") {
		t.Fatal("invalid native token accepted or SDK discovery challenge lost")
	}
	if otherCalls.Load() != before {
		t.Fatal("invalid token reached protected native handler")
	}
	bound, err := f.s.CreateToken(context.Background(), f.actor, core.TokenRequest{Name: "Native MCP", Scopes: []string{"notes:read"}, Resources: []string{"host:local"}, Audience: "https://panel.example/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if w = request("/mcp", bound.Token, false); w.Code != 204 {
		t.Fatal("audience-matching native token blocked")
	}
	if w = request("/api/v1/notes", bound.Token, false); w.Code != 401 {
		t.Fatal("MCP audience accepted by API")
	}
	strict := NewFrontdoor(f.s, FrontdoorOptions{MCPAudience: "https://panel.example/mcp", StrictProtectExternalAPIs: true}, mux)
	r := httptest.NewRequest("GET", "http://panel.example/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+bound.Token)
	w = httptest.NewRecorder()
	strict.ServeHTTP(w, r)
	if w.Code != 401 || !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Basic ") {
		t.Fatal("strict gateway opt-in ignored")
	}
}

type gatewayPATTransport struct{ token string }

func (tr gatewayPATTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+tr.token)
	return http.DefaultTransport.RoundTrip(r)
}
func TestGatewaySupportsRealSDKLocalMCPWithoutPublicURL(t *testing.T) {
	f := newFixture(t)
	code, out, _ := f.request(t, "POST", "/api/v1/actions/settings_basic_auth_update", map[string]any{"password": "a secure long password", "enabled": true, "basic_username": "entry-user", "basic_password": "separate browser entry password", "expected_revision": 0}, map[string]string{"X-CSRF-Token": f.csrf})
	if code != 200 {
		t.Fatal(code, out)
	}
	token, err := f.s.CreateToken(context.Background(), f.actor, core.TokenRequest{Name: "Local native AI", Kind: "mcp", Scopes: []string{"notes:read", "notes:write"}, Resources: []string{"host:local"}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := mcpbridge.NewHTTP(f.s, mcpbridge.HTTPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewFrontdoor(f.s, FrontdoorOptions{}, native))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "gateway-native-sdk-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: gatewayPATTransport{token.Token}}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal("local native MCP blocked by browser gate", err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "notes_create", Arguments: map[string]any{"title": "Real SDK through gateway"}})
	if err != nil || result.IsError {
		t.Fatal("native tool operation failed", err)
	}
	if err = f.s.RevokeToken(ctx, f.actor, token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = session.ListTools(ctx, nil); err == nil {
		t.Fatal("revoked SDK token bypassed the gate")
	}
}
