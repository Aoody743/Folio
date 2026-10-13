package mcpbridge

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

const (
	maxOAuthClients       = 256
	maxPendingCodes       = 512
	codeLifetime          = 2 * time.Minute
	accessLifetimeSeconds = 3600
)

type oauthClient struct {
	ID                      string   `json:"client_id"`
	Name                    string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	CreatedAt               int64    `json:"client_id_issued_at"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
}

type authorizationCode struct {
	clientID, redirectURI, challenge, resource string
	scopes                                     []string
	actor                                      core.Actor
	expires                                    time.Time
}

type oauthIssuer struct {
	service                     *core.Service
	origin, resource, stateFile string
	mu                          sync.Mutex
	clients                     map[string]oauthClient
	codes                       map[string]authorizationCode // SHA-256 digests only
	registrationWindows         map[string]rateWindow
}
type rateWindow struct {
	since time.Time
	count int
}

func newOAuthIssuer(service *core.Service, origin, resource string) (*oauthIssuer, error) {
	o := &oauthIssuer{service: service, origin: origin, resource: resource, stateFile: filepath.Join(filepath.Dir(service.SetupSecretFile()), "oauth-clients.json"), clients: map[string]oauthClient{}, codes: map[string]authorizationCode{}, registrationWindows: map[string]rateWindow{}}
	// Client registrations contain public metadata, not authentication secrets.
	// A corrupt file fails closed without overwriting existing registrations.
	if data, err := os.ReadFile(o.stateFile); err == nil {
		var clients map[string]oauthClient
		if len(data) <= 1<<20 && json.Unmarshal(data, &clients) == nil && clients != nil && len(clients) <= maxOAuthClients {
			valid := true
			for id, client := range clients {
				if id != client.ID || !validClient(client) {
					valid = false
					break
				}
			}
			if valid {
				o.clients = clients
			} else {
				return nil, errors.New("stored OAuth client metadata is invalid")
			}
		} else {
			return nil, errors.New("stored OAuth client metadata is invalid or exceeds bounds")
		}
	} else if !os.IsNotExist(err) {
		return nil, errors.New("cannot read stored OAuth client metadata")
	}
	return o, nil
}

func (o *oauthIssuer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp":
		if !method(w, r, http.MethodGet) {
			return
		}
		writeJSON(w, 200, map[string]any{"resource": o.resource, "authorization_servers": []string{o.origin}, "bearer_methods_supported": []string{"header"}, "resource_name": "PowerLinux MCP", "scopes_supported": o.scopes()})
	case "/.well-known/oauth-authorization-server":
		if !method(w, r, http.MethodGet) {
			return
		}
		writeJSON(w, 200, map[string]any{"issuer": o.origin, "authorization_endpoint": o.origin + "/oauth/authorize", "token_endpoint": o.origin + "/oauth/token", "registration_endpoint": o.origin + "/oauth/register", "revocation_endpoint": o.origin + "/oauth/revoke", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code"}, "token_endpoint_auth_methods_supported": []string{"none"}, "code_challenge_methods_supported": []string{"S256"}, "scopes_supported": o.scopes(), "authorization_response_iss_parameter_supported": true})
	case "/oauth/register":
		o.register(w, r)
	case "/oauth/authorize":
		o.authorize(w, r)
	case "/oauth/token":
		o.exchange(w, r)
	case "/oauth/revoke":
		o.revoke(w, r)
	default:
		http.NotFound(w, r)
	}
}

func method(w http.ResponseWriter, r *http.Request, allowed string) bool {
	if r.Method == allowed {
		return true
	}
	w.Header().Set("Allow", allowed)
	oauthError(w, 405, "invalid_request", "HTTP method is not allowed")
	return false
}
func oauthError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": code, "error_description": message})
}
func digestCode(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func randomOAuthID(prefix string) (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(data), nil
}

func (o *oauthIssuer) scopes() []string {
	actions := o.service.Actions(core.Actor{ID: "oauth-discovery", Name: "discovery", Scopes: []string{"*"}, Resources: []string{"*"}})
	var scopes []string
	for _, action := range actions {
		if action.Scope != "" && !strings.Contains(action.Scope, " ") {
			scopes = append(scopes, action.Scope)
		}
	}
	slices.Sort(scopes)
	return slices.Compact(scopes)
}

func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || len(raw) > 2048 {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	return u.Scheme == "http" && loopbackHost(u.Hostname())
}
func validClient(client oauthClient) bool {
	if client.ID == "" || len(client.ID) > 128 || client.Name == "" || len(client.Name) > 120 || len(client.RedirectURIs) == 0 || len(client.RedirectURIs) > 8 || client.TokenEndpointAuthMethod != "none" {
		return false
	}
	for _, uri := range client.RedirectURIs {
		if !validRedirect(uri) {
			return false
		}
	}
	return slices.Equal(client.GrantTypes, []string{"authorization_code"}) && slices.Equal(client.ResponseTypes, []string{"code"})
}

func (o *oauthIssuer) register(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		oauthError(w, 400, "invalid_client_metadata", "Expected application/json")
		return
	}
	if !o.allowRegistration(r.RemoteAddr) {
		oauthError(w, 429, "slow_down", "Client registration rate limit exceeded")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var input oauthClient
	decoder := json.NewDecoder(r.Body)
	if decoder.Decode(&input) != nil {
		oauthError(w, 400, "invalid_client_metadata", "Invalid client metadata")
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		oauthError(w, 400, "invalid_client_metadata", "Expected one JSON object")
		return
	}
	input.ID = ""
	input.CreatedAt = time.Now().Unix()
	if input.TokenEndpointAuthMethod == "" {
		input.TokenEndpointAuthMethod = "none"
	}
	if strings.TrimSpace(input.Name) == "" {
		input.Name = "MCP client"
	}
	if len(input.GrantTypes) == 0 {
		input.GrantTypes = []string{"authorization_code"}
	}
	if len(input.ResponseTypes) == 0 {
		input.ResponseTypes = []string{"code"}
	}
	input.ID = "validation"
	if !validClient(input) {
		oauthError(w, 400, "invalid_client_metadata", "Only public authorization-code clients with HTTPS or loopback redirect URIs are supported")
		return
	}
	var err error
	input.ID, err = randomOAuthID("pl_client_")
	if err != nil {
		oauthError(w, 500, "server_error", "Unable to register client")
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.clients) >= maxOAuthClients {
		oauthError(w, 503, "temporarily_unavailable", "Client registration capacity reached")
		return
	}
	o.clients[input.ID] = input
	if err = o.saveClientsLocked(); err != nil {
		delete(o.clients, input.ID)
		oauthError(w, 500, "server_error", "Unable to persist client registration")
		return
	}
	writeJSON(w, http.StatusCreated, input)
}

func (o *oauthIssuer) allowRegistration(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	for key, window := range o.registrationWindows {
		if now.Sub(window.since) > time.Hour {
			delete(o.registrationWindows, key)
		}
	}
	window := o.registrationWindows[host]
	if window.since.IsZero() || now.Sub(window.since) > time.Hour {
		window = rateWindow{since: now}
	}
	if window.count >= 10 || len(o.registrationWindows) >= 1024 {
		return false
	}
	window.count++
	o.registrationWindows[host] = window
	return true
}

func (o *oauthIssuer) saveClientsLocked() error {
	data, err := json.Marshal(o.clients)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(o.stateFile), ".oauth-clients-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, o.stateFile)
}

type authorizationRequest struct {
	client                                  oauthClient
	redirectURI, state, challenge, resource string
	scopes                                  []string
}

func (o *oauthIssuer) parseAuthorization(values url.Values) (authorizationRequest, error) {
	var result authorizationRequest
	for _, key := range []string{"client_id", "redirect_uri", "response_type", "code_challenge_method", "code_challenge", "resource", "scope", "state"} {
		if len(values[key]) != 1 {
			return result, errors.New("Missing or repeated authorization parameter: " + key)
		}
	}
	if values.Get("response_type") != "code" || values.Get("code_challenge_method") != "S256" {
		return result, errors.New("Authorization code flow with S256 PKCE is required")
	}
	result.challenge = values.Get("code_challenge")
	challenge, err := base64.RawURLEncoding.DecodeString(result.challenge)
	if err != nil || len(challenge) != 32 || len(result.challenge) != 43 {
		return result, errors.New("Invalid S256 code challenge")
	}
	result.resource = values.Get("resource")
	if result.resource != o.resource {
		return result, errors.New("Resource must exactly match this MCP endpoint")
	}
	result.state = values.Get("state")
	if result.state == "" || len(result.state) > 2048 {
		return result, errors.New("A bounded opaque state is required")
	}
	o.mu.Lock()
	client, ok := o.clients[values.Get("client_id")]
	o.mu.Unlock()
	if !ok {
		return result, errors.New("Unknown OAuth client")
	}
	result.client = client
	result.redirectURI = values.Get("redirect_uri")
	if !slices.Contains(client.RedirectURIs, result.redirectURI) {
		return result, errors.New("Redirect URI is not registered")
	}
	result.scopes = strings.Fields(values.Get("scope"))
	if len(result.scopes) == 0 || len(result.scopes) > 64 {
		return result, errors.New("Request explicit, bounded action scopes")
	}
	known := o.scopes()
	for _, scope := range result.scopes {
		if !slices.Contains(known, scope) {
			return result, errors.New("Unknown action scope")
		}
	}
	slices.Sort(result.scopes)
	result.scopes = slices.Compact(result.scopes)
	return result, nil
}

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Authorize PowerLinux</title><style>body{font-family:system-ui;background:#111318;color:#e9ebf0;margin:0;padding:32px}main{max-width:520px;margin:8vh auto;background:#1b1e25;border:1px solid #353a44;border-radius:24px;padding:32px}h1{font-size:24px}p,li{line-height:1.6}code{color:#c6b9ff}button{font:inherit;border:0;border-radius:12px;padding:12px 18px;margin:16px 8px 0 0;background:#c6b9ff;color:#1b1729}.secondary{background:#353a44;color:#e9ebf0}a{color:#c6b9ff}</style></head><body><main><p>PowerLinux · MCP access</p><h1>Authorize {{.ClientName}}?</h1><p>Signed in as <strong>{{.UserName}}</strong>. This external client can use these action scopes on your resources until access expires or you revoke it:</p><ul>{{range .Scopes}}<li><code>{{.}}</code></li>{{end}}</ul><p>Resource: <code>{{.Resource}}</code></p><p>Access expires in one hour. Your password is never shared. Notes and logs do not grant further permission.</p><form method="post" action="/oauth/authorize">{{range $key,$value := .Fields}}<input type="hidden" name="{{$key}}" value="{{$value}}">{{end}}<button name="decision" value="allow">Allow access</button><button class="secondary" name="decision" value="deny">Deny</button></form></main></body></html>`))

func (o *oauthIssuer) authorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		oauthError(w, 405, "invalid_request", "HTTP method is not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, 400, "invalid_request", "Invalid authorization form")
		return
	}
	values := r.URL.Query()
	if r.Method == http.MethodPost {
		values = r.PostForm
	}
	request, err := o.parseAuthorization(values)
	if err != nil {
		oauthError(w, 400, "invalid_request", err.Error())
		return
	}
	cookie, err := r.Cookie("pl_session")
	if err != nil {
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/?return_to="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		} else {
			oauthError(w, 401, "login_required", "Sign in to PowerLinux before authorizing a client")
		}
		return
	}
	session, err := o.service.AuthenticateSession(r.Context(), cookie.Value)
	if err != nil {
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/?return_to="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		} else {
			oauthError(w, 401, "login_required", "Session has expired")
		}
		return
	}
	for _, scope := range request.scopes {
		if !slices.Contains(session.Actor.Scopes, "*") && !slices.Contains(session.Actor.Scopes, scope) {
			oauthError(w, 403, "invalid_scope", "Current account cannot grant the requested scope")
			return
		}
	}
	if r.Method == http.MethodGet {
		fields := map[string]string{"csrf_token": session.CSRFToken}
		for _, key := range []string{"client_id", "redirect_uri", "response_type", "code_challenge_method", "code_challenge", "resource", "scope", "state"} {
			fields[key] = values.Get(key)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		_ = consentPage.Execute(w, map[string]any{"ClientName": request.client.Name, "UserName": session.Actor.Name, "Scopes": request.scopes, "Resource": request.resource, "Fields": fields})
		return
	}
	if len(values["csrf_token"]) != 1 || subtle.ConstantTimeCompare([]byte(values.Get("csrf_token")), []byte(session.CSRFToken)) != 1 {
		oauthError(w, 403, "access_denied", "Invalid consent CSRF token")
		return
	}
	redirect, _ := url.Parse(request.redirectURI)
	query := redirect.Query()
	query.Set("state", request.state)
	query.Set("iss", o.origin)
	if values.Get("decision") != "allow" {
		query.Set("error", "access_denied")
		redirect.RawQuery = query.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusSeeOther)
		return
	}
	code, err := randomOAuthID("pl_code_")
	if err != nil {
		oauthError(w, 500, "server_error", "Unable to issue authorization code")
		return
	}
	o.mu.Lock()
	for key, pending := range o.codes {
		if time.Now().After(pending.expires) {
			delete(o.codes, key)
		}
	}
	if len(o.codes) >= maxPendingCodes {
		o.mu.Unlock()
		oauthError(w, 503, "temporarily_unavailable", "Authorization capacity reached")
		return
	}
	o.codes[digestCode(code)] = authorizationCode{clientID: request.client.ID, redirectURI: request.redirectURI, challenge: request.challenge, resource: request.resource, scopes: request.scopes, actor: session.Actor, expires: time.Now().Add(codeLifetime)}
	o.mu.Unlock()
	query.Set("code", code)
	redirect.RawQuery = query.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusSeeOther)
}

func (o *oauthIssuer) exchange(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		oauthError(w, 400, "invalid_request", "Expected a form-encoded token request")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if r.ParseForm() != nil {
		oauthError(w, 400, "invalid_request", "Invalid token form")
		return
	}
	values := r.PostForm
	for _, key := range []string{"grant_type", "client_id", "redirect_uri", "code", "code_verifier", "resource"} {
		if len(values[key]) != 1 {
			oauthError(w, 400, "invalid_request", "Missing or repeated token parameter")
			return
		}
	}
	if values.Get("grant_type") != "authorization_code" {
		oauthError(w, 400, "unsupported_grant_type", "Only authorization_code is supported")
		return
	}
	if values.Get("resource") != o.resource {
		oauthError(w, 400, "invalid_target", "Resource audience does not match")
		return
	}
	verifier := values.Get("code_verifier")
	if !validVerifier(verifier) {
		oauthError(w, 400, "invalid_grant", "Invalid PKCE verifier")
		return
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	key := digestCode(values.Get("code"))
	o.mu.Lock()
	code, ok := o.codes[key]
	delete(o.codes, key)
	o.mu.Unlock()
	if !ok || time.Now().After(code.expires) || code.clientID != values.Get("client_id") || code.redirectURI != values.Get("redirect_uri") || code.resource != values.Get("resource") || subtle.ConstantTimeCompare([]byte(code.challenge), []byte(challenge)) != 1 {
		oauthError(w, 400, "invalid_grant", "Authorization code is invalid, expired, or already used")
		return
	}
	created, err := o.service.CreateToken(r.Context(), code.actor, core.TokenRequest{Name: "OAuth MCP client " + code.clientID, Kind: "mcp", Scopes: code.scopes, Resources: code.actor.Resources, ExpiresInSeconds: accessLifetimeSeconds, Audience: o.resource})
	if err != nil {
		oauthError(w, 403, "access_denied", "Current account cannot issue the requested access token")
		return
	}
	writeJSON(w, 200, map[string]any{"access_token": created.Token, "token_type": "Bearer", "expires_in": accessLifetimeSeconds, "scope": strings.Join(code.scopes, " ")})
}

func validVerifier(value string) bool {
	if len(value) < 43 || len(value) > 128 {
		return false
	}
	for _, b := range []byte(value) {
		if !(b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-' || b == '.' || b == '_' || b == '~') {
			return false
		}
	}
	return true
}

func (o *oauthIssuer) revoke(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if r.ParseForm() != nil || len(r.PostForm["token"]) != 1 {
		oauthError(w, 400, "invalid_request", "Expected a token to revoke")
		return
	}
	// RFC 7009 deliberately returns success for an already invalid token.
	_ = o.service.RevokeOwnToken(r.Context(), r.PostForm.Get("token"), o.resource)
	writeJSON(w, 200, map[string]any{})
}
