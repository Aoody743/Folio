// Package httpapi is a thin authenticated protocol adapter. All resource
// operations go through core.Execute, the same registry used by native MCP.
package httpapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

type Options struct {
	// Set true when TLS is terminated by a trusted proxy. X-Forwarded-* headers
	// are never implicitly trusted; local direct HTTP remains usable by default.
	SecureCookies   bool
	AllowedOrigins  []string
	ResourceHandler ResourceHandler
}
type ResourceHandler interface {
	HandleResource(http.ResponseWriter, *http.Request, core.Actor) bool
}
type revalidationKey struct{}

// RevalidateActor allows stream adapters to check the original credential on
// each bounded frame/lease, including session expiry and token revocation.
func RevalidateActor(r *http.Request) (core.Actor, error) {
	check, ok := r.Context().Value(revalidationKey{}).(func() (core.Actor, error))
	if !ok {
		return core.Actor{}, &core.Error{Code: "unauthorized", Message: "Authenticated stream context required", Status: 401}
	}
	return check()
}

type handler struct {
	service   *core.Service
	options   Options
	authSlots chan struct{}
}

func New(s *core.Service) http.Handler { return NewWithOptions(s, Options{}) }
func NewWithOptions(s *core.Service, options Options) http.Handler {
	s.SealRegistry()
	return &handler{service: s, options: options, authSlots: make(chan struct{}, 2)}
}

type identity struct {
	actor    core.Actor
	session  *core.Session
	rawToken string
}

func (h *handler) authenticate(r *http.Request) (identity, error) {
	if value := r.Header.Get("Authorization"); value != "" {
		if strings.HasPrefix(value, "Basic ") {
			// The outer optional Basic gateway verified this credential; the
			// native panel still needs its own session cookie.
		} else if !strings.HasPrefix(value, "Bearer ") {
			return identity{}, &core.Error{Code: "unauthorized", Message: "Bearer authentication required", Status: 401}
		} else {
			raw := strings.TrimPrefix(value, "Bearer ")
			a, e := h.service.AuthenticateToken(r.Context(), raw)
			return identity{actor: a, rawToken: raw}, e
		}
	}
	cookie, e := r.Cookie("pl_session")
	if e != nil {
		return identity{}, &core.Error{Code: "unauthorized", Message: "Authentication required", Status: 401}
	}
	session, e := h.service.AuthenticateSession(r.Context(), cookie.Value)
	return identity{actor: session.Actor, session: &session}, e
}
func fail(w http.ResponseWriter, err error) {
	var e *core.Error
	if !errors.As(err, &e) {
		e = &core.Error{Code: "internal_error", Message: "Operation could not be completed", Status: 500}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": e})
}
func success(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}
func invalid(message string) error {
	return &core.Error{Code: "invalid_input", Message: message, Status: 400}
}
func forbidden(message string) error {
	return &core.Error{Code: "forbidden", Message: message, Status: 403}
}
func method(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, m := range allowed {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	fail(w, &core.Error{Code: "method_not_allowed", Message: "Method not allowed", Status: 405})
	return false
}
func (h *handler) checkOrigin(r *http.Request) error {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return forbidden("Cross-site requests are not permitted")
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	u, e := url.Parse(origin)
	if e != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return forbidden("Request origin is not permitted")
	}
	scheme := "http"
	if r.TLS != nil || h.options.SecureCookies {
		scheme = "https"
	}
	if u.Scheme == scheme && strings.EqualFold(u.Host, r.Host) {
		return nil
	}
	for _, allowed := range h.options.AllowedOrigins {
		if origin == allowed {
			return nil
		}
	}
	return forbidden("Request origin is not permitted")
}
func isWrite(r *http.Request) bool {
	return r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS"
}
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(core.WithPeerIP(r.Context(), r.RemoteAddr))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	if e := h.checkOrigin(r); e != nil {
		fail(w, e)
		return
	}
	if r.URL.Path == "/healthz" {
		if !method(w, r, http.MethodGet) {
			return
		}
		success(w, map[string]any{"status": "ok"})
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
		fail(w, &core.Error{Code: "not_found", Message: "Endpoint not found", Status: 404})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	if path == "session" {
		if !method(w, r, http.MethodGet) {
			return
		}
		out := map[string]any{"initialized": h.service.Initialized(r.Context()), "authenticated": false}
		branding, e := h.service.PublicPanelSettings(r.Context())
		if e != nil {
			fail(w, e)
			return
		}
		out["panel"] = branding
		who, e := h.authenticate(r)
		if e == nil {
			out["authenticated"] = true
			out["user"] = who.actor
			if who.session != nil {
				out["csrf_token"] = who.session.CSRFToken
			}
		}
		success(w, out)
		return
	}
	if path == "auth/captcha" {
		if !method(w, r, http.MethodGet) {
			return
		}
		challenge, e := h.service.NewCAPTCHA(r.Context())
		if e != nil {
			fail(w, e)
			return
		}
		success(w, challenge)
		return
	}
	if path == "auth/application" || path == "auth/temporary" {
		h.delegatedAuth(w, r, path)
		return
	}
	if path == "auth/setup" || path == "auth/login" {
		h.auth(w, r, path)
		return
	}
	who, e := h.authenticate(r)
	if e != nil {
		fail(w, e)
		return
	}
	if isWrite(r) && who.session != nil {
		provided := r.Header.Get("X-CSRF-Token")
		if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(who.session.CSRFToken)) != 1 {
			fail(w, forbidden("A valid CSRF token is required"))
			return
		}
	}
	if h.options.ResourceHandler != nil && (strings.HasPrefix(path, "files/") || strings.HasPrefix(path, "terminal/") || strings.HasPrefix(path, "databases/") || strings.HasPrefix(path, "sites/") || strings.HasPrefix(path, "applications/") || strings.HasPrefix(path, "browser/")) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && who.session != nil {
			valid := false
			for _, protocol := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
				protocol = strings.TrimSpace(protocol)
				if strings.HasPrefix(protocol, "powerlinux-csrf.") && subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(protocol, "powerlinux-csrf.")), []byte(who.session.CSRFToken)) == 1 {
					valid = true
					break
				}
			}
			if !valid {
				fail(w, forbidden("WebSocket cookie authentication requires a CSRF subprotocol"))
				return
			}
		}
		streamRequest := r.WithContext(context.WithValue(r.Context(), revalidationKey{}, func() (core.Actor, error) { current, e := h.authenticate(r); return current.actor, e }))
		if h.options.ResourceHandler.HandleResource(w, streamRequest, who.actor) {
			return
		}
	}
	if path == "auth/logout" {
		if !method(w, r, http.MethodPost) {
			return
		}
		if who.session != nil {
			if e = h.service.Logout(r.Context(), who.session.ID); e != nil {
				fail(w, e)
				return
			}
		}
		h.setCookie(w, r, "", time.Unix(0, 0))
		success(w, map[string]any{"logged_out": true})
		return
	}
	if path == "events" {
		if !method(w, r, http.MethodGet) {
			return
		}
		h.events(w, r, who)
		return
	}
	if path == "openapi.json" {
		if !method(w, r, http.MethodGet) {
			return
		}
		success(w, h.service.OpenAPI(who.actor))
		return
	}
	if path == "scopes" {
		if !method(w, r, http.MethodGet) {
			return
		}
		success(w, h.service.Scopes(who.actor))
		return
	}
	if path == "tokens" || strings.HasPrefix(path, "tokens/") {
		h.tokens(w, r, who, path)
		return
	}
	if path == "account" || strings.HasPrefix(path, "account/") {
		h.account(w, r, who, path)
		return
	}
	var action string
	var input json.RawMessage
	switch path {
	case "system/status":
		if !method(w, r, http.MethodGet) {
			return
		}
		action = "system_get_status"
	case "capabilities":
		if !method(w, r, http.MethodGet) {
			return
		}
		action = "capabilities_list"
	case "notes":
		if !method(w, r, http.MethodGet, http.MethodPost) {
			return
		}
		if r.Method == "POST" {
			action = "notes_create"
			input, e = readRaw(w, r)
		} else {
			action = "notes_list"
			params := map[string]any{}
			if r.URL.Query().Get("archived") == "true" {
				params["archived"] = true
			}
			if target := r.URL.Query().Get("target_id"); target != "" {
				params["target_id"] = target
			}
			e = copyPagination(r, params)
			input, _ = json.Marshal(params)
		}
	case "audit":
		if !method(w, r, http.MethodGet) {
			return
		}
		action = "audit_list"
		params := map[string]any{}
		e = copyPagination(r, params)
		input, _ = json.Marshal(params)
	case "tasks":
		if !method(w, r, http.MethodGet) {
			return
		}
		action = "tasks_list"
	default:
		if strings.HasPrefix(path, "actions/") && !strings.Contains(strings.TrimPrefix(path, "actions/"), "/") {
			if !method(w, r, http.MethodPost) {
				return
			}
			action = strings.TrimPrefix(path, "actions/")
			input, e = readRaw(w, r)
		} else if strings.HasPrefix(path, "notes/") {
			action, input, e = h.noteRoute(w, r, strings.TrimPrefix(path, "notes/"))
			if action == "" && e == nil {
				return
			}
		} else if strings.HasPrefix(path, "audit/") {
			if !method(w, r, http.MethodGet) {
				return
			}
			id := strings.TrimPrefix(path, "audit/")
			if id == "" || strings.Contains(id, "/") {
				e = &core.Error{Code: "not_found", Message: "Endpoint not found", Status: 404}
				break
			}
			action = "audit_get"
			input, _ = json.Marshal(map[string]string{"id": id})
		} else if strings.HasPrefix(path, "tasks/") {
			suffix := strings.Split(strings.TrimPrefix(path, "tasks/"), "/")
			if len(suffix) < 1 || len(suffix) > 2 || suffix[0] == "" {
				e = &core.Error{Code: "not_found", Message: "Endpoint not found", Status: 404}
				break
			}
			action = "tasks_get"
			if len(suffix) == 2 {
				switch suffix[1] {
				case "logs":
					action = "tasks_logs"
				case "cancel":
					action = "tasks_cancel"
				default:
					e = &core.Error{Code: "not_found", Message: "Endpoint not found", Status: 404}
				}
			}
			required := http.MethodGet
			if action == "tasks_cancel" {
				required = http.MethodPost
			}
			if !method(w, r, required) {
				return
			}
			input, _ = json.Marshal(map[string]any{"id": suffix[0]})
		} else {
			e = &core.Error{Code: "not_found", Message: "Endpoint not found", Status: 404}
		}
	}
	if e != nil {
		fail(w, e)
		return
	}
	data, e := h.service.ExecuteWithKey(r.Context(), who.actor, action, input, r.Header.Get("Idempotency-Key"))
	if e != nil {
		fail(w, e)
		return
	}
	if note, ok := data.(core.Note); ok {
		w.Header().Set("ETag", fmt.Sprintf("\"%d\"", note.Revision))
	}
	success(w, data)
}
func copyPagination(r *http.Request, params map[string]any) error {
	for _, key := range []string{"limit", "offset"} {
		if value := r.URL.Query().Get(key); value != "" {
			n, e := strconv.Atoi(value)
			if e != nil {
				return invalid("Invalid pagination")
			}
			params[key] = n
		}
	}
	return nil
}
func readRaw(w http.ResponseWriter, r *http.Request) (json.RawMessage, error) {
	ct := r.Header.Get("Content-Type")
	if r.ContentLength != 0 && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return nil, &core.Error{Code: "unsupported_media_type", Message: "Use application/json", Status: 415}
	}
	data, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if e != nil {
		return nil, &core.Error{Code: "payload_too_large", Message: "Request exceeds 64 KiB", Status: 413}
	}
	if len(data) == 0 {
		data = []byte("{}")
	}
	trim := bytes.TrimSpace(data)
	if len(trim) == 0 || trim[0] != '{' || !json.Valid(trim) {
		return nil, invalid("Request must contain one JSON object")
	}
	return data, nil
}
func decode(w http.ResponseWriter, r *http.Request, target any) error {
	data, e := readRaw(w, r)
	if e != nil {
		return e
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if e = d.Decode(target); e != nil {
		return invalid("Request has invalid or unknown fields")
	}
	return nil
}
func (h *handler) auth(w http.ResponseWriter, r *http.Request, path string) {
	if !method(w, r, http.MethodPost) {
		return
	}
	select {
	case h.authSlots <- struct{}{}:
		defer func() { <-h.authSlots }()
	default:
		fail(w, &core.Error{Code: "busy", Message: "Authentication is busy; retry shortly", Status: 429})
		return
	}
	var input struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		OTP           string `json:"otp,omitempty"`
		SetupToken    string `json:"setup_token,omitempty"`
		LoginEntry    string `json:"login_entry,omitempty"`
		CAPTCHAID     string `json:"captcha_id,omitempty"`
		CAPTCHAAnswer string `json:"captcha_answer,omitempty"`
	}
	if e := decode(w, r, &input); e != nil {
		fail(w, e)
		return
	}
	if path == "auth/setup" {
		secret := input.SetupToken
		if secret == "" {
			secret = r.Header.Get("X-Setup-Token")
		}
		a, e := h.service.Setup(r.Context(), secret, input.Username, input.Password)
		if e != nil {
			fail(w, e)
			return
		}
		success(w, map[string]any{"initialized": true, "user": a.Public()})
		return
	}
	session, e := h.service.ProtectedLogin(r.Context(), input.Username, input.Password, input.OTP, input.LoginEntry, input.CAPTCHAID, input.CAPTCHAAnswer)
	if e != nil {
		fail(w, e)
		return
	}
	expiry, _ := time.Parse(time.RFC3339, session.ExpiresAt)
	h.setCookie(w, r, session.ID, expiry)
	success(w, map[string]any{"authenticated": true, "user": session.Actor, "csrf_token": session.CSRFToken, "expires_at": session.ExpiresAt})
}
func (h *handler) setCookie(w http.ResponseWriter, r *http.Request, value string, expires time.Time) {
	maxAge := int(time.Until(expires).Seconds())
	if value == "" {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{Name: "pl_session", Value: value, Path: "/", Expires: expires, MaxAge: maxAge, HttpOnly: true, Secure: h.options.SecureCookies || r.TLS != nil, SameSite: http.SameSiteStrictMode})
}
func (h *handler) noteRoute(w http.ResponseWriter, r *http.Request, path string) (string, json.RawMessage, error) {
	parts := strings.Split(path, "/")
	if len(parts) < 1 || len(parts) > 2 || parts[0] == "" {
		return "", nil, &core.Error{Code: "not_found", Message: "Endpoint not found", Status: 404}
	}
	action := "notes_get"
	params := map[string]any{"id": parts[0]}
	if len(parts) == 2 {
		switch parts[1] {
		case "history":
			if !method(w, r, http.MethodGet) {
				return "", nil, nil
			}
			action = "notes_history"
		case "archive", "restore":
			if !method(w, r, http.MethodPost) {
				return "", nil, nil
			}
			action = "notes_" + parts[1]
		default:
			return "", nil, &core.Error{Code: "not_found", Message: "Endpoint not found", Status: 404}
		}
	} else {
		if !method(w, r, http.MethodGet, http.MethodPatch, http.MethodDelete) {
			return "", nil, nil
		}
		if r.Method == "PATCH" {
			action = "notes_update"
		}
		if r.Method == "DELETE" {
			action = "notes_delete"
		}
	}
	if action == "notes_update" || action == "notes_delete" || action == "notes_archive" || action == "notes_restore" {
		raw, e := readRaw(w, r)
		if e != nil {
			return "", nil, e
		}
		if e = json.Unmarshal(raw, &params); e != nil {
			return "", nil, invalid("Invalid note input")
		}
		if id, ok := params["id"]; ok && id != parts[0] {
			return "", nil, invalid("Note id is set by the request path")
		}
		params["id"] = parts[0]
		if value := r.Header.Get("If-Match"); value != "" {
			n, e := strconv.ParseInt(strings.Trim(value, "\""), 10, 64)
			if e != nil {
				return "", nil, invalid("If-Match must be a note revision")
			}
			if body, ok := params["expected_revision"]; ok && body != float64(n) {
				return "", nil, invalid("If-Match and expected_revision differ")
			}
			params["expected_revision"] = n
		}
		if r.Method == "DELETE" {
			if value := r.URL.Query().Get("expected_revision"); value != "" {
				n, e := strconv.ParseInt(value, 10, 64)
				if e != nil {
					return "", nil, invalid("Invalid expected_revision")
				}
				params["expected_revision"] = n
			}
		}
	}
	raw, _ := json.Marshal(params)
	return action, raw, nil
}
func (h *handler) tokens(w http.ResponseWriter, r *http.Request, who identity, path string) {
	var data any
	var e error
	if path == "tokens" {
		if !method(w, r, http.MethodGet, http.MethodPost) {
			return
		}
		if r.Method == "GET" {
			data, e = h.service.ListTokens(r.Context(), who.actor)
		} else {
			var input core.TokenRequest
			if e = decode(w, r, &input); e == nil {
				data, e = h.service.CreateToken(r.Context(), who.actor, input)
			}
		}
	} else {
		if !method(w, r, http.MethodDelete) {
			return
		}
		id := strings.TrimPrefix(path, "tokens/")
		if id == "" || strings.Contains(id, "/") {
			fail(w, &core.Error{Code: "not_found", Message: "Token not found", Status: 404})
			return
		}
		e = h.service.RevokeToken(r.Context(), who.actor, id)
		data = map[string]any{"revoked": e == nil}
	}
	if e != nil {
		fail(w, e)
		return
	}
	success(w, data)
}
func (h *handler) account(w http.ResponseWriter, r *http.Request, who identity, path string) {
	if path == "account" {
		if !method(w, r, http.MethodGet) {
			return
		}
		data, e := h.service.Account(r.Context(), who.actor)
		if e != nil {
			fail(w, e)
			return
		}
		success(w, data)
		return
	}
	if !method(w, r, http.MethodPost) {
		return
	}
	select {
	case h.authSlots <- struct{}{}:
		defer func() { <-h.authSlots }()
	default:
		fail(w, &core.Error{Code: "busy", Message: "Authentication is busy; retry shortly", Status: 429})
		return
	}
	var data any = map[string]any{"updated": true}
	var e error
	switch path {
	case "account/password":
		var input struct {
			OldPassword string `json:"old_password"`
			NewPassword string `json:"new_password"`
			OTP         string `json:"otp,omitempty"`
		}
		if e = decode(w, r, &input); e == nil {
			e = h.service.ChangePassword(r.Context(), who.actor, input.OldPassword, input.NewPassword, input.OTP)
		}
	case "account/totp/enroll", "account/totp/disable":
		var input struct {
			Password string `json:"password"`
			OTP      string `json:"otp,omitempty"`
		}
		if e = decode(w, r, &input); e == nil {
			if path == "account/totp/enroll" {
				data, e = h.service.BeginTOTP(r.Context(), who.actor, input.Password, input.OTP)
			} else {
				e = h.service.DisableTOTP(r.Context(), who.actor, input.Password, input.OTP)
			}
		}
	case "account/totp/confirm":
		var input struct {
			OTP string `json:"otp"`
		}
		if e = decode(w, r, &input); e == nil {
			e = h.service.ConfirmTOTP(r.Context(), who.actor, input.OTP)
		}
	default:
		e = &core.Error{Code: "not_found", Message: "Endpoint not found", Status: 404}
	}
	if e != nil {
		fail(w, e)
		return
	}
	success(w, data)
}
func (h *handler) events(w http.ResponseWriter, r *http.Request, who identity) {
	if _, ok := w.(http.Flusher); !ok {
		fail(w, &core.Error{Code: "stream_unavailable", Message: "Event streaming unavailable", Status: 500})
		return
	}
	// Subscribe before exposing the open stream. Otherwise an immediate token
	// revoke plus invalidation can land between Flush and subscription setup.
	events := h.service.Subscribe(r.Context(), who.actor)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	_, _ = fmt.Fprint(w, ": connected\n\n")
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	authTick := time.NewTicker(time.Second)
	defer authTick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			if _, e := h.authenticate(r); e != nil {
				_, _ = fmt.Fprint(w, "event: auth_expired\ndata: {}\n\n")
				_ = rc.Flush()
				return
			}
			data, e := json.Marshal(event.Data)
			if e != nil {
				continue
			}
			if _, e = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data); e != nil {
				return
			}
			if e = rc.Flush(); e != nil {
				return
			}
		case <-authTick.C:
			// An open transport does not preserve a revoked credential indefinitely.
			if _, e := h.authenticate(r); e != nil {
				_, _ = fmt.Fprint(w, "event: auth_expired\ndata: {}\n\n")
				_ = rc.Flush()
				return
			}
		case <-tick.C:
			if _, e := fmt.Fprint(w, ": heartbeat\n\n"); e != nil {
				return
			}
			if e := rc.Flush(); e != nil {
				return
			}
		}
	}
}
