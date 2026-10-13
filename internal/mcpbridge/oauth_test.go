package mcpbridge

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

func oauthFixture(t *testing.T) (*core.Service, core.Actor, core.Session, *httptest.Server, *httpHandler) {
	t.Helper()
	service, actor := fixture(t)
	session, err := service.Login(context.Background(), actor.Name, "correct horse battery staple", "", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	origin := "http://" + server.Listener.Addr().String()
	handler, err := NewHTTP(service, HTTPOptions{PublicURL: origin + "/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
	return service, actor, session, server, handler.(*httpHandler)
}

func browser(t *testing.T, endpoint string, session core.Session, method string, values url.Values, origin string) *http.Response {
	t.Helper()
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(values.Encode())
	} else if values != nil {
		endpoint += "?" + values.Encode()
	}
	request, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		t.Fatal(err)
	}
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if session.ID != "" {
		request.AddCookie(&http.Cookie{Name: "pl_session", Value: session.ID})
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func registerClient(t *testing.T, server *httptest.Server, name, redirect string) oauthClient {
	t.Helper()
	input, _ := json.Marshal(map[string]any{"client_name": name, "redirect_uris": []string{redirect}, "token_endpoint_auth_method": "none"})
	response, err := http.Post(server.URL+"/oauth/register", "application/json", strings.NewReader(string(input)))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 201 {
		body, _ := io.ReadAll(response.Body)
		t.Fatal("registration", response.StatusCode, string(body))
	}
	var client oauthClient
	if json.NewDecoder(response.Body).Decode(&client) != nil || client.ID == "" {
		t.Fatal("missing registered client")
	}
	return client
}

func authorizationValues(client oauthClient, resource, verifier string) url.Values {
	sum := sha256.Sum256([]byte(verifier))
	return url.Values{"client_id": {client.ID}, "redirect_uri": {client.RedirectURIs[0]}, "response_type": {"code"}, "scope": {"notes:read notes:write"}, "state": {"opaque-test-state"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "resource": {resource}}
}

func consentCode(t *testing.T, server *httptest.Server, session core.Session, values url.Values) string {
	t.Helper()
	form := values.Clone()
	form.Set("csrf_token", session.CSRFToken)
	form.Set("decision", "allow")
	response := browser(t, server.URL+"/oauth/authorize", session, http.MethodPost, form, server.URL)
	response.Body.Close()
	if response.StatusCode != 303 {
		t.Fatal("authorization failed", response.StatusCode)
	}
	redirect, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if redirect.Query().Get("state") != values.Get("state") || redirect.Query().Get("iss") != server.URL {
		t.Fatal("authorization response binding", redirect)
	}
	code := redirect.Query().Get("code")
	if code == "" {
		t.Fatal("authorization code missing")
	}
	return code
}

func exchangeCode(t *testing.T, server *httptest.Server, client oauthClient, resource, code, verifier string) (int, map[string]any) {
	t.Helper()
	values := url.Values{"grant_type": {"authorization_code"}, "client_id": {client.ID}, "redirect_uri": {client.RedirectURIs[0]}, "code": {code}, "code_verifier": {verifier}, "resource": {resource}}
	response := browser(t, server.URL+"/oauth/token", core.Session{}, http.MethodPost, values, "")
	defer response.Body.Close()
	var result map[string]any
	if json.NewDecoder(response.Body).Decode(&result) != nil {
		t.Fatal("invalid token response")
	}
	return response.StatusCode, result
}

func TestOAuthPKCEConsentAudienceRevocationAndClientPersistence(t *testing.T) {
	service, _, session, server, handler := oauthFixture(t)
	redirectURI := "http://127.0.0.1:19876/callback"
	client := registerClient(t, server, `Test <script>alert(1)</script>`, redirectURI)
	verifier := strings.Repeat("v", 64)
	values := authorizationValues(client, server.URL+"/mcp", verifier)
	unauthenticated := browser(t, server.URL+"/oauth/authorize", core.Session{}, http.MethodGet, values, "")
	unauthenticated.Body.Close()
	if unauthenticated.StatusCode != 303 || !strings.HasPrefix(unauthenticated.Header.Get("Location"), "/?return_to=") {
		t.Fatal("authorization bypassed login")
	}
	page := browser(t, server.URL+"/oauth/authorize", session, http.MethodGet, values, "")
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if page.StatusCode != 200 || strings.Contains(string(body), "<script>alert(1)</script>") || !strings.Contains(string(body), session.CSRFToken) {
		t.Fatal("unsafe or incomplete consent page", page.StatusCode)
	}
	badCSRF := values.Clone()
	badCSRF.Set("decision", "allow")
	badCSRF.Set("csrf_token", "wrong")
	response := browser(t, server.URL+"/oauth/authorize", session, http.MethodPost, badCSRF, server.URL)
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("CSRF bypass")
	}
	crossOrigin := values.Clone()
	crossOrigin.Set("decision", "allow")
	crossOrigin.Set("csrf_token", session.CSRFToken)
	response = browser(t, server.URL+"/oauth/authorize", session, http.MethodPost, crossOrigin, "https://attacker.invalid")
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("cross-origin consent bypass")
	}
	for _, key := range []string{"redirect_uri", "resource", "scope", "code_challenge_method"} {
		bad := values.Clone()
		bad.Set(key, "invalid-value")
		response = browser(t, server.URL+"/oauth/authorize", session, http.MethodGet, bad, "")
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatal("bad authorization accepted", key)
		}
	}
	code := consentCode(t, server, session, values)
	status, result := exchangeCode(t, server, client, server.URL+"/mcp", code, verifier)
	if status != 200 {
		t.Fatal(status, result)
	}
	token := result["access_token"].(string)
	if _, err := service.AuthenticateTokenForAudience(context.Background(), token, server.URL+"/other"); err == nil {
		t.Fatal("OAuth audience bypass")
	}
	if _, err := service.AuthenticateToken(context.Background(), token); err == nil {
		t.Fatal("OAuth token escaped its bound audience into generic API")
	}
	status, _ = exchangeCode(t, server, client, server.URL+"/mcp", code, verifier)
	if status != 400 {
		t.Fatal("authorization code reused")
	}
	mcpSession := sdkHTTP(t, server.URL+"/mcp", token)
	assertTools(t, mcpSession, "notes_create", true)
	assertTools(t, mcpSession, "notes_delete", false)
	_, resultCall := call(t, mcpSession, "notes_create", map[string]any{"title": "OAuth shared note", "content": "real persisted OAuth write"})
	if resultCall.IsError {
		t.Fatal(resultCall)
	}
	response = browser(t, server.URL+"/oauth/revoke", core.Session{}, http.MethodPost, url.Values{"token": {token}}, "")
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("revoke failed")
	}
	if _, err := mcpSession.ListTools(context.Background(), nil); err == nil {
		t.Fatal("revoked OAuth token retained discovery")
	}
	// Persisted public client metadata remains usable after handler restart.
	state, err := os.Stat(handler.oauth.stateFile)
	if err != nil || state.Mode().Perm() != 0600 {
		t.Fatal("OAuth client metadata permissions", state, err)
	}
	newHandler, err := NewHTTP(service, HTTPOptions{PublicURL: server.URL + "/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := newHandler.(*httpHandler).oauth.clients[client.ID]; !ok {
		t.Fatal("client registration lost after handler restart")
	}
	// Exact Host validation permits trusted reverse-proxy setup and rejects
	// DNS rebinding regardless of bearer possession.
	request := httptest.NewRequest(http.MethodGet, server.URL+"/.well-known/oauth-authorization-server", nil)
	request.Host = "attacker.invalid"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != 403 {
		t.Fatal("unexpected Host accepted")
	}
}

func TestOAuthInvalidVerifierExpiredCodeAndUnsafePublicURL(t *testing.T) {
	service, _, session, server, handler := oauthFixture(t)
	for _, public := range []string{"http://example.com/mcp", "https://example.com/not-mcp", "https://user:secret@example.com/mcp"} {
		if _, err := NewHTTP(service, HTTPOptions{PublicURL: public}); err == nil {
			t.Fatal("unsafe public URL accepted", public)
		}
	}
	client := registerClient(t, server, "PKCE test client", "http://127.0.0.1:19877/callback")
	verifier := strings.Repeat("k", 64)
	values := authorizationValues(client, server.URL+"/mcp", verifier)
	code := consentCode(t, server, session, values)
	status, _ := exchangeCode(t, server, client, server.URL+"/mcp", code, strings.Repeat("x", 64))
	if status != 400 {
		t.Fatal("incorrect verifier accepted")
	}
	status, _ = exchangeCode(t, server, client, server.URL+"/mcp", code, verifier)
	if status != 400 {
		t.Fatal("failed exchange did not consume code")
	}
	code = consentCode(t, server, session, values)
	handler.oauth.mu.Lock()
	pending := handler.oauth.codes[digestCode(code)]
	pending.expires = time.Now().Add(-time.Second)
	handler.oauth.codes[digestCode(code)] = pending
	handler.oauth.mu.Unlock()
	status, _ = exchangeCode(t, server, client, server.URL+"/mcp", code, verifier)
	if status != 400 {
		t.Fatal("expired authorization code accepted")
	}
}

func TestOfficialSDKOAuthDiscoveryRegistrationPKCEAndToolCall(t *testing.T) {
	_, _, session, server, _ := oauthFixture(t)
	redirect := "http://127.0.0.1:19878/sdk-callback"
	oauth, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{ClientName: "Official SDK integration", RedirectURIs: []string{redirect}, TokenEndpointAuthMethod: "none", GrantTypes: []string{"authorization_code"}, ResponseTypes: []string{"code"}}},
		RedirectURL:                     redirect,
		AuthorizationCodeFetcher: func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			link, err := url.Parse(args.URL)
			if err != nil {
				return nil, err
			}
			// The SDK owns PKCE/state. This callback simulates the signed-in user
			// reading and consenting in a browser, not server-side auto-approval.
			page := browser(t, args.URL, session, http.MethodGet, nil, "")
			body, _ := io.ReadAll(page.Body)
			page.Body.Close()
			if page.StatusCode != 200 || !strings.Contains(string(body), "Allow access") {
				return nil, fmt.Errorf("consent page: %d %s", page.StatusCode, string(body))
			}
			values := link.Query()
			values.Set("csrf_token", session.CSRFToken)
			values.Set("decision", "allow")
			response := browser(t, server.URL+"/oauth/authorize", session, http.MethodPost, values, server.URL)
			response.Body.Close()
			if response.StatusCode != 303 {
				return nil, fmt.Errorf("consent denied: %d", response.StatusCode)
			}
			location, err := url.Parse(response.Header.Get("Location"))
			if err != nil {
				return nil, err
			}
			return &auth.AuthorizationResult{Code: location.Query().Get("code"), State: location.Query().Get("state"), Iss: location.Query().Get("iss")}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "official-oauth-client", Version: "1"}, nil)
	connection, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", OAuthHandler: oauth, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	assertTools(t, connection, "system_get_status", true)
	envelope, result := call(t, connection, "system_get_status", map[string]any{})
	if result.IsError || envelope["data"] == nil {
		t.Fatal(envelope)
	}
}
