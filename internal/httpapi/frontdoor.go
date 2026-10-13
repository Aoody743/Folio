package httpapi

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

type FrontdoorOptions struct {
	SecureCookies  bool
	AllowedOrigins []string
	MCPAudience    string
	// Explicit opt-in for deployments that also require a gateway cookie on
	// external API/MCP clients. Normal MCP OAuth clients need no extra cookie.
	StrictProtectExternalAPIs bool
}
type frontdoor struct {
	service *core.Service
	options FrontdoorOptions
	next    http.Handler
	slots   chan struct{}
}

// NewFrontdoor wraps the assembled static/API/MCP/OAuth mux. It is additional
// entry protection, never a replacement for the native authenticated handlers.
func NewFrontdoor(s *core.Service, options FrontdoorOptions, next http.Handler) http.Handler {
	return &frontdoor{service: s, options: options, next: next, slots: make(chan struct{}, 2)}
}
func (h *frontdoor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(core.WithPeerIP(r.Context(), r.RemoteAddr))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	checker := handler{options: Options{SecureCookies: h.options.SecureCookies, AllowedOrigins: h.options.AllowedOrigins}}
	if err := checker.checkOrigin(r); err != nil {
		fail(w, err)
		return
	}
	path := r.URL.Path
	// OAuth discovery and public-client PKCE exchanges must remain usable by
	// external SDKs. Their respective handlers enforce protocol authorization.
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if path == "/.well-known/oauth-authorization-server" || path == "/.well-known/oauth-protected-resource" || path == "/.well-known/oauth-protected-resource/mcp" {
			h.next.ServeHTTP(w, r)
			return
		}
	}
	if !h.options.StrictProtectExternalAPIs && (path == "/oauth/register" || path == "/oauth/token" || path == "/oauth/revoke") {
		h.next.ServeHTTP(w, r)
		return
	}
	if path == "/healthz" {
		peer, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			if ip := net.ParseIP(peer); ip != nil && ip.IsLoopback() {
				h.next.ServeHTTP(w, r)
				return
			}
		}
	}
	required, err := h.service.GatewayRequired(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	if !required {
		h.next.ServeHTTP(w, r)
		return
	}
	mcpPath := path == "/mcp"
	apiPath := strings.HasPrefix(path, "/api/v1/") && !strings.HasPrefix(path, "/api/v1/auth/")
	if !h.options.StrictProtectExternalAPIs && (mcpPath || apiPath) {
		if value := r.Header.Get("Authorization"); strings.HasPrefix(value, "Bearer ") {
			audience := ""
			if mcpPath {
				audience = h.options.MCPAudience
			}
			if _, err = h.service.AuthenticateTokenForAudience(r.Context(), strings.TrimPrefix(value, "Bearer "), audience); err == nil {
				h.next.ServeHTTP(w, r)
				return
			}
		}
	}
	if cookie, cookieErr := r.Cookie("pl_gateway"); cookieErr == nil && h.service.AuthenticateGatewaySession(r.Context(), cookie.Value) == nil {
		h.next.ServeHTTP(w, r)
		return
	}
	if username, password, ok := r.BasicAuth(); ok {
		select {
		case h.slots <- struct{}{}:
			defer func() { <-h.slots }()
		default:
			fail(w, &core.Error{Code: "rate_limited", Message: "Gateway verification is busy; retry shortly", Status: 429})
			return
		}
		cookie, checkErr := h.service.AuthenticateBasicGateway(r.Context(), username, password)
		if checkErr == nil {
			http.SetCookie(w, &http.Cookie{Name: "pl_gateway", Value: cookie, Path: "/", HttpOnly: true, Secure: h.options.SecureCookies || r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 3600, Expires: time.Now().Add(time.Hour)})
			// Basic authorizes the entry, while panel authorization still uses its
			// existing session or independently supplied Bearer token.
			r = r.Clone(r.Context())
			r.Header = r.Header.Clone()
			r.Header.Del("Authorization")
			h.next.ServeHTTP(w, r)
			return
		}
		if core.ErrorStatus(checkErr) != 401 {
			fail(w, checkErr)
			return
		}
	}
	if mcpPath && !h.options.StrictProtectExternalAPIs {
		if resource, parseErr := url.Parse(h.options.MCPAudience); parseErr == nil && resource.Host != "" {
			resource.Path = "/.well-known/oauth-protected-resource/mcp"
			resource.RawQuery = ""
			resource.Fragment = ""
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+resource.String()+`"`)
		} else {
			w.Header().Set("WWW-Authenticate", "Bearer")
		}
	} else {
		w.Header().Set("WWW-Authenticate", `Basic realm="PowerLinux", charset="UTF-8"`)
	}
	fail(w, &core.Error{Code: "unauthorized", Message: "Entry authentication required", Status: 401})
}
