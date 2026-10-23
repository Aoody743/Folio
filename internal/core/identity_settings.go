package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"regexp"
	"strings"
	"unicode/utf8"
)

type peerIPKey struct{}

// WithPeerIP accepts only a protocol adapter's actual peer, never Forwarded.
func WithPeerIP(ctx context.Context, address string) context.Context {
	if host, _, err := net.SplitHostPort(address); err == nil {
		address = host
	}
	ip := net.ParseIP(address)
	if ip == nil {
		address = ""
	} else {
		address = ip.String()
	}
	return context.WithValue(ctx, peerIPKey{}, address)
}

type PanelSettings struct {
	PanelName          string `json:"panel_name"`
	ServerName         string `json:"server_name"`
	Footer             string `json:"footer"`
	LoginEntry         string `json:"login_entry"`
	CAPTCHAEnabled     bool   `json:"captcha_enabled"`
	APIEnabled         bool   `json:"api_enabled"`
	Maintenance        bool   `json:"maintenance"`
	BasicEnabled       bool   `json:"basic_auth_enabled"`
	BasicUsername      string `json:"basic_auth_username,omitempty"`
	UnauthorizedStatus int    `json:"unauthorized_status"`
	Revision           int64  `json:"revision"`
}

func (s *Service) PanelSettings(ctx context.Context) (PanelSettings, error) {
	return loadPanelSettings(ctx, s.db)
}
func loadPanelSettings(ctx context.Context, conn sqlState) (PanelSettings, error) {
	p := PanelSettings{PanelName: "Folio · 折纸面板", Footer: "Folio · 折纸面板", APIEnabled: true}
	var raw string
	var revision int64
	err := conn.QueryRowContext(ctx, "SELECT payload,revision FROM identity_settings WHERE id='panel'").Scan(&raw, &revision)
	if err == sql.ErrNoRows {
		return p, nil
	}
	if err != nil || json.Unmarshal([]byte(raw), &p) != nil {
		return p, internal()
	}
	p.Revision = revision
	return p, nil
}

// PublicPanelSettings omits the custom entry itself; it is not an authentication
// credential, but disclosing it from every unauthenticated session defeats it.
func (s *Service) PublicPanelSettings(ctx context.Context) (map[string]any, error) {
	p, err := s.PanelSettings(ctx)
	return map[string]any{"panel_name": p.PanelName, "server_name": p.ServerName, "footer": p.Footer, "captcha_enabled": p.CAPTCHAEnabled, "custom_login_entry": p.LoginEntry != "", "maintenance": p.Maintenance}, err
}

func (s *Service) CheckLoginEntry(ctx context.Context, entry string) error {
	p, err := s.PanelSettings(ctx)
	if err != nil {
		return err
	}
	if p.Maintenance {
		return &Error{Code: "maintenance", Message: "Panel login is temporarily paused; an existing administrator can resume it", Status: 503}
	}
	if p.LoginEntry != "" && entry != p.LoginEntry {
		if validUnauthorizedStatus(p.UnauthorizedStatus) && p.UnauthorizedStatus != 0 {
			return &Error{Code: "login_entry", Message: "Login entry unavailable", Status: p.UnauthorizedStatus}
		}
		return missing("Login entry")
	}
	return nil
}

func identityBinding(name, description, scope string, read bool, schema map[string]any, resources func(context.Context, Actor, json.RawMessage) ([]string, error), execute func(context.Context, Actor, json.RawMessage) (ActionResult, error)) Binding {
	return Binding{Action: Action{Name: name, Description: description, Scope: scope, ReadOnly: read, InputSchema: schema}, Resources: resources, Execute: execute}
}
func hostIdentityResource(context.Context, Actor, json.RawMessage) ([]string, error) {
	return []string{"host:local"}, nil
}
func boundedText(max int) map[string]any { return map[string]any{"type": "string", "maxLength": max} }
func credentialFields() map[string]any {
	return map[string]any{"password": map[string]any{"type": "string", "minLength": 12, "maxLength": 512, "format": "password"}, "otp": boundedText(16)}
}

func (s *Service) registerIdentityBindings() error {
	bindings := []Binding{
		identityBinding("settings_panel_get", "Read persistent panel branding and login policy", "settings:read", true, object(map[string]any{}), hostIdentityResource, func(ctx context.Context, a Actor, raw json.RawMessage) (ActionResult, error) {
			p, e := s.PanelSettings(ctx)
			return ActionResult{Data: p}, e
		}),
		identityBinding("settings_branding_update", "Update plain-text panel name and footer with revision protection", "settings:write", false, object(map[string]any{"panel_name": map[string]any{"type": "string", "minLength": 1, "maxLength": 80}, "footer": boundedText(240), "server_name": boundedText(80), "expected_revision": map[string]any{"type": "integer", "minimum": 0}}, "panel_name", "footer", "expected_revision"), hostIdentityResource, s.updateBranding),
	}
	fields := credentialFields()
	fields["login_entry"] = boundedText(64)
	fields["captcha_enabled"] = boolean()
	fields["api_enabled"] = boolean()
	fields["maintenance"] = boolean()
	fields["expected_revision"] = map[string]any{"type": "integer", "minimum": 0}
	fields["unauthorized_status"] = map[string]any{"type": "integer", "enum": []int{0, 400, 401, 403, 404, 408, 416}}
	bindings = append(bindings, identityBinding("settings_security_update", "Change login entry, CAPTCHA, API access or maintenance after password and MFA confirmation", "account:manage", false, object(fields, "password", "login_entry", "captcha_enabled", "api_enabled", "maintenance", "expected_revision"), hostIdentityResource, s.updateSecurity))
	nameFields := credentialFields()
	nameFields["username"] = map[string]any{"type": "string", "pattern": validUsername.String()}
	bindings = append(bindings, identityBinding("account_rename", "Rename the existing account without changing its identity or losing resources", "account:manage", false, object(nameFields, "username", "password"), hostIdentityResource, s.renameAccount))
	bindings = append(bindings, s.applicationBindings()...)
	bindings = append(bindings, s.temporaryBindings()...)
	bindings = append(bindings, s.bookmarkBindings()...)
	bindings = append(bindings, s.gatewayBindings()...)
	return s.RegisterProvider(Bindings(bindings))
}
func (s *Service) savePanelSettings(ctx context.Context, p PanelSettings, expected int64) (PanelSettings, error) {
	p.Revision = expected + 1
	var result sql.Result
	var err error
	if expected == 0 {
		result, err = s.db.ExecContext(ctx, "INSERT INTO identity_settings(id,payload,revision) VALUES('panel',?,1) ON CONFLICT(id) DO NOTHING", jsonText(p))
	} else {
		result, err = s.db.ExecContext(ctx, "UPDATE identity_settings SET payload=?,revision=revision+1 WHERE id='panel' AND revision=?", jsonText(p), expected)
	}
	if err != nil {
		return p, internal()
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return p, &Error{Code: "revision_conflict", Message: "Panel settings changed; reload before saving", Status: 412}
	}
	return p, nil
}
func (s *Service) updateBranding(ctx context.Context, a Actor, raw json.RawMessage) (ActionResult, error) {
	var in struct {
		PanelName  string  `json:"panel_name"`
		ServerName *string `json:"server_name"`
		Footer     string  `json:"footer"`
		Expected   int64   `json:"expected_revision"`
	}
	if e := decodeInput(raw, &in); e != nil {
		return ActionResult{}, e
	}
	if !utf8.ValidString(in.PanelName+in.Footer) || strings.TrimSpace(in.PanelName) == "" {
		return ActionResult{}, invalid("Panel name must be nonempty plain text")
	}
	p, e := s.PanelSettings(ctx)
	if e != nil {
		return ActionResult{}, e
	}
	if in.ServerName != nil {
		if !utf8.ValidString(*in.ServerName) {
			return ActionResult{}, invalid("Server display name must be plain text")
		}
		p.ServerName = strings.TrimSpace(*in.ServerName)
	}
	p.PanelName = in.PanelName
	p.Footer = in.Footer
	p, e = s.savePanelSettings(ctx, p, in.Expected)
	return ActionResult{Data: p}, e
}

var loginEntryPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{7,63}$`)

func (s *Service) updateSecurity(ctx context.Context, a Actor, raw json.RawMessage) (ActionResult, error) {
	var in struct {
		Password           string `json:"password"`
		OTP                string `json:"otp"`
		LoginEntry         string `json:"login_entry"`
		CAPTCHA            bool   `json:"captcha_enabled"`
		API                bool   `json:"api_enabled"`
		Maintenance        bool   `json:"maintenance"`
		Expected           int64  `json:"expected_revision"`
		UnauthorizedStatus *int   `json:"unauthorized_status,omitempty"`
	}
	if e := decodeInput(raw, &in); e != nil {
		return ActionResult{}, e
	}
	if e := s.VerifyCredentials(ctx, a, in.Password, in.OTP); e != nil {
		return ActionResult{}, e
	}
	if in.LoginEntry != "" && !loginEntryPattern.MatchString(in.LoginEntry) {
		return ActionResult{}, invalid("Custom entry must be an 8–64 character slug")
	}
	p, e := s.PanelSettings(ctx)
	if e != nil {
		return ActionResult{}, e
	}
	p.LoginEntry = in.LoginEntry
	p.CAPTCHAEnabled = in.CAPTCHA
	p.APIEnabled = in.API
	p.Maintenance = in.Maintenance
	if in.UnauthorizedStatus != nil {
		if !validUnauthorizedStatus(*in.UnauthorizedStatus) {
			return ActionResult{}, invalid("Unsupported unauthorized entry status")
		}
		p.UnauthorizedStatus = *in.UnauthorizedStatus
	}
	p, e = s.savePanelSettings(ctx, p, in.Expected)
	return ActionResult{Data: p}, e
}

func validUnauthorizedStatus(status int) bool {
	switch status {
	case 0, 400, 401, 403, 404, 408, 416:
		return true
	}
	return false
}
func (s *Service) renameAccount(ctx context.Context, a Actor, raw json.RawMessage) (ActionResult, error) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		OTP      string `json:"otp"`
	}
	if e := decodeInput(raw, &in); e != nil {
		return ActionResult{}, e
	}
	if !validUsername.MatchString(in.Username) {
		return ActionResult{}, invalid("Invalid account name")
	}
	if e := s.VerifyCredentials(ctx, a, in.Password, in.OTP); e != nil {
		return ActionResult{}, e
	}
	_, e := s.db.ExecContext(ctx, "UPDATE users SET username=? WHERE id=?", in.Username, a.ID)
	if e != nil {
		return ActionResult{}, &Error{Code: "username_conflict", Message: "Account name is already in use", Status: 409}
	}
	a.Name = in.Username
	return ActionResult{Data: map[string]any{"user": a.Public()}}, nil
}
