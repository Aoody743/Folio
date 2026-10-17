package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// HTTPOptions configures the externally advertised MCP resource and exact
// browser origins. PublicURL is required for OAuth and must be HTTPS remotely.
// A blank URL exposes PAT authentication only, for local development.
type HTTPOptions struct {
	PublicURL      string
	AllowedOrigins []string
}

type httpHandler struct {
	service    *core.Service
	options    HTTPOptions
	stream     http.Handler
	oauth      *oauthIssuer
	origin     string
	host       string
	requireTLS bool
	allowed    map[string]bool
}

type actorContextKey struct{}

// NewHTTP includes /mcp, /oauth/* and both OAuth discovery paths. Mount these
// paths on the primary server; no separate listener or database is needed.
func NewHTTP(service *core.Service, options HTTPOptions) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("MCP requires the shared core service")
	}
	h := &httpHandler{service: service, options: options, allowed: map[string]bool{}}
	if options.PublicURL != "" {
		u, err := url.Parse(options.PublicURL)
		if err != nil || u.Host == "" || u.User != nil || u.Path != "/mcp" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && (u.Scheme != "http" || !loopbackHost(u.Hostname()))) {
			return nil, errors.New("MCP PublicURL must be HTTPS with /mcp path, or HTTP on explicit loopback")
		}
		h.origin = u.Scheme + "://" + u.Host
		h.host = u.Host
		h.requireTLS = u.Scheme == "https"
		h.allowed[h.origin] = true
		h.oauth, err = newOAuthIssuer(service, h.origin, options.PublicURL)
		if err != nil {
			return nil, err
		}
	}
	for _, origin := range options.AllowedOrigins {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && (u.Scheme != "http" || !loopbackHost(u.Hostname()))) {
			return nil, errors.New("MCP allowed origins must be explicit HTTP(S) origins")
		}
		h.allowed[origin] = true
	}
	h.stream = mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		actor, ok := r.Context().Value(actorContextKey{}).(core.Actor)
		if !ok {
			return nil
		}
		server, err := newServer(r.Context(), directSource{service: service, actor: actor})
		if err != nil {
			return nil
		}
		return server
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: MaxRequestBytes, SessionTimeout: 5 * time.Minute, PropagateRequestCancellation: true, DisableLocalhostProtection: h.host != ""})
	return h, nil
}

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Network app grants bind to the authenticated transport's actual peer.
	// Forwarded headers are deliberately irrelevant to this identity context.
	peer, _, peerErr := net.SplitHostPort(r.RemoteAddr)
	if peerErr == nil {
		r = r.WithContext(core.WithPeerIP(r.Context(), peer))
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A configured public host is exact, including port. This also protects
	// a trusted TLS reverse proxy targeting a loopback listener; forwarded
	// headers never decide which host or audience is trusted.
	if h.host != "" && !strings.EqualFold(r.Host, h.host) {
		writeError(w, &core.Error{Code: "host_denied", Message: "Host does not match the configured MCP public URL", Status: 403})
		return
	}
	if h.requireTLS && r.TLS == nil {
		peer, _, err := net.SplitHostPort(r.RemoteAddr)
		address := net.ParseIP(peer)
		if err != nil || address == nil || !address.IsLoopback() {
			writeError(w, &core.Error{Code: "tls_required", Message: "Remote MCP requires TLS; a TLS reverse proxy must connect through loopback", Status: 426})
			return
		}
	}
	if r.URL.Path == "/mcp" {
		if !h.checkOrigin(w, r) {
			return
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		token := bearerToken(r)
		actor, err := h.service.AuthenticateTokenForAudience(r.Context(), token, h.options.PublicURL)
		if token == "" || err != nil {
			challenge := `Bearer realm="powerlinux"`
			if h.origin != "" {
				challenge += `, resource_metadata="` + h.origin + `/.well-known/oauth-protected-resource/mcp"`
			}
			w.Header().Set("WWW-Authenticate", challenge)
			writeError(w, &core.Error{Code: "unauthorized", Message: "A valid scoped Folio bearer token is required", Status: 401})
			return
		}
		h.stream.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorContextKey{}, actor)))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/oauth/") || strings.HasPrefix(r.URL.Path, "/.well-known/oauth-") {
		if h.oauth == nil {
			writeError(w, &core.Error{Code: "oauth_not_configured", Message: "Set a verified public MCP URL before enabling OAuth", Status: 503})
			return
		}
		// The consent form is protected against cross-site posts. Token and
		// registration endpoints use no browser cookies and explicit grants.
		if r.URL.Path == "/oauth/authorize" {
			protection := http.NewCrossOriginProtection()
			protection.Handler(h.oauth).ServeHTTP(w, r)
		} else {
			if !h.checkOrigin(w, r) {
				return
			}
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			h.oauth.ServeHTTP(w, r)
		}
		return
	}
	http.NotFound(w, r)
}

func (h *httpHandler) checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	allowed := h.allowed[origin]
	if h.origin == "" {
		// Local PAT-only endpoint: origin must exactly match request Host.
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		allowed = allowed || origin == scheme+"://"+r.Host
	}
	if !allowed {
		writeError(w, &core.Error{Code: "origin_denied", Message: "This browser origin is not allowed", Status: 403})
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Add("Vary", "Origin")
	return true
}

func bearerToken(r *http.Request) string {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return ""
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, err error) {
	value := safeError(err)
	writeJSON(w, value.Status, map[string]any{"error": value})
}
