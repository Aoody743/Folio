// Package serverbrowser provides an authenticated, server-hosted Chromium.
// Browser profiles are temporary and never shared between persisted identities.
package serverbrowser

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/httpapi"
)

const Scope = "browser:use"
const Resource = "browser:server"
const maxTabs = 8

type Dialog struct {
	Type          string `json:"type"`
	Message       string `json:"message"`
	DefaultPrompt string `json:"default_prompt,omitempty"`
}
type Tab struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	URL        string  `json:"url"`
	CanBack    bool    `json:"can_back"`
	CanForward bool    `json:"can_forward"`
	Dialog     *Dialog `json:"dialog,omitempty"`
}
type Session struct {
	ID          string `json:"id"`
	Tabs        []Tab  `json:"tabs"`
	ActiveTabID string `json:"active_tab_id,omitempty"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	ExpiresAt   string `json:"expires_at"`
}
type Input struct {
	ID         string  `json:"id,omitempty"`
	TabID      string  `json:"tab_id,omitempty"`
	URL        string  `json:"url,omitempty"`
	Direction  string  `json:"direction,omitempty"`
	Kind       string  `json:"kind,omitempty"`
	Event      string  `json:"event,omitempty"`
	Button     string  `json:"button,omitempty"`
	Key        string  `json:"key,omitempty"`
	Code       string  `json:"code,omitempty"`
	Text       string  `json:"text,omitempty"`
	Width      int     `json:"width,omitempty"`
	Height     int     `json:"height,omitempty"`
	Modifiers  int     `json:"modifiers,omitempty"`
	X          float64 `json:"x,omitempty"`
	Y          float64 `json:"y,omitempty"`
	DeltaX     float64 `json:"delta_x,omitempty"`
	DeltaY     float64 `json:"delta_y,omitempty"`
	Accept     bool    `json:"accept,omitempty"`
	PromptText string  `json:"prompt_text,omitempty"`
}
type Options struct {
	Engine  EngineOptions
	AppsDir string // Trusted, private runtime directory; optional downloads never enter the panel bundle.
	// IdleTimeout and MaxSessions may be lowered by trusted tests/composition.
	IdleTimeout time.Duration
	MaxSessions int
	MaxLifetime time.Duration
}
type ownedSession struct {
	mu        sync.Mutex
	owner     core.Actor
	engine    *engine
	view      Session
	lastSeen  time.Time
	createdAt time.Time
	closed    bool
}
type Provider struct {
	deps       core.Dependencies
	options    Options
	mu         sync.Mutex
	sessions   map[string]*ownedSession
	closed     bool
	done       chan struct{}
	stopped    chan struct{}
	operations chan struct{}
	appWork    chan struct{}
	appGate    sync.RWMutex
	wisps      map[*wispConnection]struct{}
}

func New(deps core.Dependencies, options Options) (*Provider, error) {
	if deps.Authorize == nil || deps.ResolveActor == nil || deps.State == nil || deps.Tasks == nil {
		return nil, errors.New("server browser requires persisted authorization")
	}
	if options.IdleTimeout <= 0 {
		options.IdleTimeout = 15 * time.Minute
	}
	if options.MaxSessions <= 0 {
		options.MaxSessions = 4
	}
	if options.MaxLifetime <= 0 {
		options.MaxLifetime = 4 * time.Hour
	}
	p := &Provider{deps: deps, options: options, sessions: map[string]*ownedSession{}, wisps: map[*wispConnection]struct{}{}, appWork: make(chan struct{}, 1), done: make(chan struct{}), stopped: make(chan struct{}), operations: make(chan struct{}, 8)}

	if deps.State != nil {
		apps, err := deps.State.List(context.Background(), "apps")
		if err != nil {
			return nil, err
		}
		for id, raw := range apps {
			var a browserApp
			if json.Unmarshal(raw, &a) == nil && (a.TaskStatus == "queued" || a.TaskStatus == "running") {
				a.TaskStatus = "interrupted"
				if err = deps.State.Save(context.Background(), "apps", id, encoded(a)); err != nil {
					return nil, err
				}
			}
		}
	}
	go p.reaper()
	return p, nil
}
func failure(code, message string, status int) error {
	return &core.Error{Code: code, Message: message, Status: status}
}
func invalid() error { return failure("invalid_input", "Invalid browser parameters", 400) }
func denied() error {
	return failure("forbidden", "This browser session is not authorized for the current identity", 403)
}
func encoded(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func decode(raw json.RawMessage) (Input, error) {
	var in Input
	if len(raw) > 24<<10 {
		return in, invalid()
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil {
		return in, invalid()
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return in, invalid()
	}
	return in, nil
}
func (p *Provider) actor(ctx context.Context, a core.Actor) (core.Actor, error) {
	fresh, err := p.deps.ResolveActor(ctx, a)
	if err != nil {
		return core.Actor{}, err
	}
	if !p.deps.Authorize(fresh, Scope, Resource) {
		return core.Actor{}, denied()
	}
	return fresh, nil
}
func validateURL(raw string) error {
	if raw == "about:blank" {
		return nil
	}
	if len(raw) == 0 || len(raw) > 4096 || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\r\n\x00") {
		return invalid()
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return failure("invalid_url", "Use an HTTP or HTTPS address without embedded credentials", 400)
	}
	return nil
}
func dimensions(width, height int) (int, int, error) {
	if width == 0 {
		width = 1280
	}
	if height == 0 {
		height = 720
	}
	if width < 320 || width > 1920 || height < 240 || height > 1200 {
		return 0, 0, invalid()
	}
	return width, height, nil
}
func stringField(max int) map[string]any   { return map[string]any{"type": "string", "maxLength": max} }
func enum(values ...string) map[string]any { return map[string]any{"type": "string", "enum": values} }
func number(min, max float64) map[string]any {
	return map[string]any{"type": "number", "minimum": min, "maximum": max}
}
func integer(min, max int) map[string]any {
	return map[string]any{"type": "integer", "minimum": min, "maximum": max}
}
func schema(fields map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": fields, "required": required, "additionalProperties": false}
}
func fields(names ...string) map[string]any {
	all := map[string]any{"id": stringField(96), "tab_id": stringField(96), "url": stringField(4096), "direction": enum("back", "forward", "reload", "stop"), "kind": enum("mouse", "wheel", "key", "text"), "event": stringField(32), "button": enum("left", "middle", "right", "none"), "key": stringField(128), "code": stringField(128), "text": stringField(16384), "width": integer(320, 1920), "height": integer(240, 1200), "modifiers": integer(0, 15), "x": number(0, 1920), "y": number(0, 1200), "delta_x": number(-10000, 10000), "delta_y": number(-10000, 10000)}
	selected := map[string]any{}
	all["accept"] = map[string]any{"type": "boolean"}
	all["prompt_text"] = stringField(16384)
	for _, key := range names {
		selected[key] = all[key]
	}
	return selected
}
func (p *Provider) Bindings() []core.Binding {
	definitions := []struct {
		name, description string
		read              bool
		properties        map[string]any
		required          []string
	}{
		{"browser_status", "Read server browser availability and the current identity's sessions", true, fields(), nil},
		{"browser_open", "Open a private temporary browser on this server", false, fields("width", "height"), nil},
		{"browser_tabs", "Read tabs in an owned server browser", true, fields("id"), []string{"id"}},
		{"browser_tab_open", "Open an HTTP/HTTPS tab in an owned server browser", false, fields("id", "url"), []string{"id"}},
		{"browser_tab_close", "Close a tab in an owned server browser", false, fields("id", "tab_id"), []string{"id", "tab_id"}},
		{"browser_navigate", "Navigate an owned browser tab from the server network", false, fields("id", "tab_id", "url"), []string{"id", "tab_id", "url"}},
		{"browser_history", "Navigate browser history, reload or stop an owned tab", false, fields("id", "tab_id", "direction"), []string{"id", "tab_id", "direction"}},
		{"browser_input", "Send bounded input to an owned browser tab without recording its text", false, fields("id", "tab_id", "kind", "event", "button", "key", "code", "text", "modifiers", "x", "y", "delta_x", "delta_y"), []string{"id", "tab_id", "kind"}},
		{"browser_resize", "Resize an owned server browser viewport", false, fields("id", "tab_id", "width", "height"), []string{"id", "tab_id", "width", "height"}},
		{"browser_dialog", "Respond to a web page dialog in an owned browser tab", false, fields("id", "tab_id", "accept", "prompt_text"), []string{"id", "tab_id", "accept"}},
		{"browser_close", "Close a private browser and remove its temporary profile", false, fields("id"), []string{"id"}},
	}
	bindings := p.appBindings()
	for _, definition := range definitions {
		definition := definition
		bindings = append(bindings, core.Binding{Action: core.Action{Name: definition.name, Description: definition.description, Scope: Scope, ReadOnly: definition.read, InputSchema: schema(definition.properties, definition.required...)}, Resources: func(ctx context.Context, a core.Actor, raw json.RawMessage) ([]string, error) {
			if _, err := decode(raw); err != nil {
				return nil, err
			}
			return []string{Resource}, nil
		}, Execute: func(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			in, err := decode(raw)
			if err != nil {
				return core.ActionResult{}, err
			}
			a, err = p.actor(ctx, a)
			if err != nil {
				return core.ActionResult{}, err
			}
			value, err := p.perform(ctx, a, definition.name, in)
			summary := map[string]any{}
			if in.ID != "" {
				summary["session_id"] = in.ID
			}
			return core.ActionResult{Data: value, Resources: []string{Resource}, AuditSummary: summary}, err
		}})
	}
	return bindings
}
func (p *Provider) get(a core.Actor, id string) (*ownedSession, error) {
	p.mu.Lock()
	s := p.sessions[id]
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, failure("unavailable", "Server browser is shutting down", 503)
	}
	if s == nil {
		return nil, failure("not_found", "Browser session ended or expired; open a new browser", 404)
	}
	if s.owner.ID != a.ID {
		return nil, denied()
	}
	return s, nil
}
func (p *Provider) snapshot(ctx context.Context, s *ownedSession) (Session, error) {
	tabs, err := s.engine.Tabs(ctx)
	if err != nil {
		return Session{}, engineError(err)
	}
	if tabs == nil {
		tabs = []Tab{}
	}
	s.view.Tabs = tabs
	if len(tabs) > 0 {
		activeFound := false
		for _, tab := range tabs {
			if tab.ID == s.view.ActiveTabID {
				activeFound = true
			}
		}
		if !activeFound {
			s.view.ActiveTabID = tabs[0].ID
		}
	} else {
		s.view.ActiveTabID = ""
	}
	s.lastSeen = time.Now()
	s.view.ExpiresAt = minExpiry(s.lastSeen.Add(p.options.IdleTimeout), s.createdAt.Add(p.options.MaxLifetime)).UTC().Format(time.RFC3339)
	return s.view, nil
}
func engineError(err error) error {
	if err == nil {
		return nil
	}
	var safe *core.Error
	if errors.As(err, &safe) {
		return safe
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return failure("browser_timeout", "The browser took too long to respond; retry or reopen it", 504)
	}
	if errors.Is(err, context.Canceled) {
		return failure("canceled", "Browser operation canceled", 409)
	}
	return failure("browser_unavailable", "The server browser stopped responding; close it and open a new session", 503)
}
func (p *Provider) perform(ctx context.Context, a core.Actor, name string, in Input) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	select {
	case p.operations <- struct{}{}:
		defer func() { <-p.operations }()
	default:
		return nil, failure("busy", "Too many browser requests; retry shortly", 429)
	}
	if name == "browser_status" {
		path, reason := availableEngine(p.engineOptions())
		if !p.installed(ctx, "panel") {
			path, reason = "", "Install the panel browser from Software → Applications first."
		}
		result := struct {
			Available bool      `json:"available"`
			Reason    string    `json:"reason,omitempty"`
			Engine    string    `json:"engine,omitempty"`
			Sessions  []Session `json:"sessions"`
		}{Available: path != "" && reason == "", Reason: reason, Engine: "Chromium", Sessions: []Session{}}
		p.mu.Lock()
		own := []*ownedSession{}
		for _, s := range p.sessions {
			if s.owner.ID == a.ID {
				own = append(own, s)
			}
		}
		p.mu.Unlock()
		for _, s := range own {
			s.mu.Lock()
			if !s.closed && !p.expired(s) {
				if view, err := p.snapshot(ctx, s); err == nil {
					result.Sessions = append(result.Sessions, view)
				} else {
					p.endSession(s)
				}
			} else if !s.closed {
				p.endSession(s)
			}
			s.mu.Unlock()
		}
		return result, nil
	}
	if name == "browser_open" {
		return p.open(ctx, a, in)
	}
	s, err := p.get(a, in.ID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || p.expired(s) {
		if !s.closed {
			p.endSession(s)
		}
		return nil, failure("not_found", "Browser session ended or expired; open a new browser", 404)
	}
	s.lastSeen = time.Now()
	if name == "browser_close" {
		p.endSession(s)
		return map[string]any{"closed": true}, nil
	}
	if name == "browser_tabs" {
		return p.snapshot(ctx, s)
	}
	if name == "browser_tab_open" {
		if in.URL == "" {
			in.URL = "about:blank"
		}
		if err = validateURL(in.URL); err != nil {
			return nil, err
		}
		tabs, e := s.engine.Tabs(ctx)
		if e != nil {
			return nil, engineError(e)
		}
		if len(tabs) >= maxTabs {
			return nil, failure("browser_limit", "Close a tab before opening another (maximum 8 tabs)", 409)
		}
		var tab Tab
		tab, err = s.engine.NewTab(ctx, in.URL, s.view.Width, s.view.Height)
		if err == nil {
			s.view.ActiveTabID = tab.ID
		}
	} else {
		tabs, e := s.engine.Tabs(ctx)
		if e != nil {
			return nil, engineError(e)
		}
		found := false
		for _, tab := range tabs {
			if tab.ID == in.TabID {
				found = true
			}
		}
		if !found {
			return nil, failure("not_found", "Browser tab is no longer open", 404)
		}
		s.view.ActiveTabID = in.TabID
		switch name {
		case "browser_dialog":
			if len(in.PromptText) > 16384 || !utf8.ValidString(in.PromptText) || strings.ContainsRune(in.PromptText, 0) {
				return nil, invalid()
			}
			err = s.engine.HandleDialog(ctx, in.TabID, in.Accept, in.PromptText)
		case "browser_tab_close":
			if len(tabs) == 1 {
				return nil, failure("browser_last_tab", "Keep one tab open, or close the browser", 409)
			}
			err = s.engine.CloseTab(ctx, in.TabID)
		case "browser_navigate":
			if err = validateURL(in.URL); err == nil {
				err = s.engine.Navigate(ctx, in.TabID, in.URL)
			}
		case "browser_history":
			if in.Direction != "back" && in.Direction != "forward" && in.Direction != "reload" && in.Direction != "stop" {
				return nil, invalid()
			}
			err = s.engine.History(ctx, in.TabID, in.Direction)
		case "browser_resize":
			var width, height int
			width, height, err = dimensions(in.Width, in.Height)
			if err == nil {
				err = p.resizeAll(ctx, s, tabs, width, height)
			}
		case "browser_input":
			if err = validateInput(in, s.view.Width, s.view.Height); err == nil {
				err = s.engine.Input(ctx, in.TabID, in)
			}
			if err != nil {
				return nil, engineError(err)
			}
			return map[string]any{"ok": true}, nil
		default:
			return nil, invalid()
		}
	}
	if err != nil {
		return nil, engineError(err)
	}
	return p.snapshot(ctx, s)
}
func validateInput(in Input, width, height int) error {
	if in.Modifiers < 0 || in.Modifiers > 15 || in.X < 0 || in.X > float64(width) || in.Y < 0 || in.Y > float64(height) || in.DeltaX < -10000 || in.DeltaX > 10000 || in.DeltaY < -10000 || in.DeltaY > 10000 {
		return invalid()
	}
	switch in.Kind {
	case "mouse":
		if in.Event != "mouseMoved" && in.Event != "mousePressed" && in.Event != "mouseReleased" {
			return invalid()
		}
		if in.Button != "" && in.Button != "left" && in.Button != "middle" && in.Button != "right" && in.Button != "none" {
			return invalid()
		}
	case "wheel":
	case "key":
		if in.Event != "keyDown" && in.Event != "keyUp" && in.Event != "rawKeyDown" {
			return invalid()
		}
		if len(in.Key) > 128 || len(in.Code) > 128 || in.Key == "" {
			return invalid()
		}
	case "text":
		if len(in.Text) > 16384 || !utf8.ValidString(in.Text) || strings.ContainsRune(in.Text, 0) {
			return invalid()
		}
	default:
		return invalid()
	}
	return nil
}
func (p *Provider) open(ctx context.Context, a core.Actor, in Input) (Session, error) {
	p.appGate.RLock()
	defer p.appGate.RUnlock()
	if !p.installed(ctx, "panel") {
		return Session{}, failure("browser_not_installed", "Install the panel browser from Software → Applications first", 409)
	}
	width, height, err := dimensions(in.Width, in.Height)
	if err != nil {
		return Session{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return Session{}, failure("unavailable", "Server browser is shutting down", 503)
	}
	for _, s := range p.sessions {
		if s.owner.ID == a.ID {
			return Session{}, failure("browser_limit", "A browser is already open; reconnect to it or close it first", 409)
		}
	}
	if len(p.sessions) >= p.options.MaxSessions {
		return Session{}, failure("browser_limit", "The server browser session limit has been reached; close an existing session first", 429)
	}
	path, reason := availableEngine(p.engineOptions())
	if path == "" || reason != "" {
		return Session{}, failure("browser_unavailable", reason, 503)
	}
	browser, err := startEngine(ctx, p.engineOptions(), width, height)
	if err != nil {
		return Session{}, engineError(err)
	}
	idBytes := make([]byte, 16)
	if _, err = rand.Read(idBytes); err != nil {
		browser.Close()
		return Session{}, engineError(err)
	}
	id := "browser-" + hex.EncodeToString(idBytes)
	s := &ownedSession{owner: core.Actor{ID: a.ID}, engine: browser, view: Session{ID: id, Width: width, Height: height}, lastSeen: time.Now(), createdAt: time.Now()}
	view, err := p.snapshot(ctx, s)
	if err != nil {
		browser.Close()
		return Session{}, err
	}
	p.sessions[id] = s
	return view, nil
}

// endSession requires the owned session lock. Lock order is session then provider.
func (p *Provider) endSession(s *ownedSession) {
	s.closed = true
	p.mu.Lock()
	if p.sessions[s.view.ID] == s {
		delete(p.sessions, s.view.ID)
	}
	p.mu.Unlock()
	_ = s.engine.Close()
}
func (p *Provider) resizeAll(ctx context.Context, s *ownedSession, tabs []Tab, width, height int) error {
	changed := []string{}
	for _, tab := range tabs {
		if err := s.engine.Resize(ctx, tab.ID, width, height); err != nil {
			rollback, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			consistent := true
			for _, id := range changed {
				if s.engine.Resize(rollback, id, s.view.Width, s.view.Height) != nil {
					consistent = false
				}
			}
			cancel()
			if !consistent {
				p.endSession(s)
			}
			return err
		}
		changed = append(changed, tab.ID)
	}
	s.view.Width = width
	s.view.Height = height
	return nil
}
func minExpiry(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func (p *Provider) expired(s *ownedSession) bool {
	return time.Since(s.lastSeen) > p.options.IdleTimeout || time.Since(s.createdAt) > p.options.MaxLifetime
}
func (p *Provider) reaper() {
	defer close(p.stopped)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			p.mu.Lock()
			sessions := make(map[string]*ownedSession, len(p.sessions))
			for id, s := range p.sessions {
				sessions[id] = s
			}
			p.mu.Unlock()
			for id, s := range sessions {
				s.mu.Lock()
				expired := p.expired(s)
				check, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_, err := p.actor(check, s.owner)
				cancel()
				if !s.closed && (expired || err != nil) {
					s.closed = true
					p.mu.Lock()
					if p.sessions[id] == s {
						delete(p.sessions, id)
					}
					p.mu.Unlock()
					_ = s.engine.Close()
				}
				s.mu.Unlock()
			}
		}
	}
}
func (p *Provider) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.done)
	for c := range p.wisps {
		c.cancel()
	}
	sessions := p.sessions
	p.sessions = map[string]*ownedSession{}
	p.mu.Unlock()
	for _, s := range sessions {
		s.mu.Lock()
		s.closed = true
		_ = s.engine.Close()
		s.mu.Unlock()
	}
	<-p.stopped
}
func reply(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(core.ErrorStatus(err))
	var safe *core.Error
	if !errors.As(err, &safe) {
		safe = &core.Error{Code: "browser_unavailable", Message: "Browser request failed", Status: 503}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": safe})
}

// HandleResource is valid only behind httpapi's authenticated middleware.
func (p *Provider) HandleResource(w http.ResponseWriter, r *http.Request, _ core.Actor) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/v1/browser/") {
		return false
	}
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		reply(w, failure("method_not_allowed", "Browser frames require GET", 405))
		return true
	}
	a, err := httpapi.RevalidateActor(r)
	if err != nil {
		reply(w, err)
		return true
	}
	a, err = p.actor(r.Context(), a)
	if err != nil {
		reply(w, err)
		return true
	}
	if p.handleWasm(w, r, a) {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/browser/"), "/")
	if len(parts) != 5 || parts[0] != "sessions" || parts[1] == "" || parts[2] != "tabs" || parts[3] == "" || parts[4] != "frame" {
		reply(w, failure("not_found", "Browser frame endpoint not found", 404))
		return true
	}
	s, err := p.get(a, parts[1])
	if err != nil {
		reply(w, err)
		return true
	}
	select {
	case p.operations <- struct{}{}:
		defer func() { <-p.operations }()
	default:
		reply(w, failure("busy", "Too many browser requests; retry shortly", 429))
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || p.expired(s) {
		if !s.closed {
			p.endSession(s)
		}
		reply(w, failure("not_found", "Browser session ended or expired; open a new browser", 404))
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	tabs, err := s.engine.Tabs(ctx)
	if err != nil {
		reply(w, engineError(err))
		return true
	}
	found := false
	for _, tab := range tabs {
		if tab.ID == parts[3] {
			found = true
		}
	}
	if !found {
		reply(w, failure("not_found", "Browser tab is no longer open", 404))
		return true
	}
	frame, err := s.engine.Frame(ctx, parts[3])
	if err != nil {
		reply(w, engineError(err))
		return true
	}
	s.lastSeen = time.Now()
	w.Header().Set("Content-Type", "image/jpeg")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(frame)
	return true
}
