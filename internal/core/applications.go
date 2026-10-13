package core

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"net"
	"strings"
	"time"
)

type Application struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	Resources []string `json:"resources"`
	CIDRs     []string `json:"cidrs"`
	Enabled   bool     `json:"enabled"`
	Revision  int64    `json:"revision"`
	CreatedAt string   `json:"created_at"`
}
type applicationCreate struct {
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	Resources []string `json:"resources"`
	CIDRs     []string `json:"cidrs"`
	Password  string   `json:"password"`
	OTP       string   `json:"otp"`
}

func aclSchema() map[string]any {
	return map[string]any{"type": "array", "minItems": 1, "maxItems": 100, "uniqueItems": true, "items": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}}
}
func (s *Service) validateLimitedACL(a Actor, scopes, resources []string) error {
	if !subset(scopes, a.Scopes, false) || !subset(resources, a.Resources, true) {
		return denied()
	}
	for _, scope := range scopes {
		if scope == "*" || scope == "tokens:manage" || scope == "account:manage" || scope == "applications:manage" || scope == "temporary:manage" || scope == "settings:write" {
			return invalid("Delegated access cannot manage identities, grants or panel security")
		}
		if !s.scopeKnown(scope) {
			return invalid("Unknown delegated scope: " + scope)
		}
	}
	for _, resource := range resources {
		if resource == "*" {
			return invalid("Delegated access must name explicit resources or resource namespaces")
		}
	}
	return nil
}
func applicationResource(_ context.Context, _ Actor, raw json.RawMessage) ([]string, error) {
	var v struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &v) != nil || len(v.ID) < 1 || len(v.ID) > 80 {
		return nil, invalid("Invalid application ID")
	}
	return []string{"application:" + v.ID}, nil
}
func (s *Service) applicationBindings() []Binding {
	fields := credentialFields()
	fields["name"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 80}
	fields["scopes"] = aclSchema()
	fields["resources"] = aclSchema()
	fields["cidrs"] = map[string]any{"type": "array", "minItems": 1, "maxItems": 32, "uniqueItems": true, "items": boundedText(80)}
	updateFields := credentialFields()
	updateFields["id"] = boundedText(80)
	updateFields["enabled"] = boolean()
	updateFields["expected_revision"] = map[string]any{"type": "integer", "minimum": 1}
	return []Binding{
		identityBinding("applications_list", "List application grants without secrets", "applications:manage", true, object(map[string]any{}), hostIdentityResource, s.listApplications),
		identityBinding("applications_create", "Create a scoped application identity and return its random secret once", "applications:manage", false, object(fields, "name", "scopes", "resources", "cidrs", "password"), hostIdentityResource, s.createApplication),
		identityBinding("applications_set_enabled", "Enable or disable an application and revoke all its outstanding leases", "applications:manage", false, object(updateFields, "id", "enabled", "expected_revision", "password"), applicationResource, s.setApplicationEnabled),
		identityBinding("applications_rotate_secret", "Replace an application secret and revoke existing access leases", "applications:manage", false, object(map[string]any{"id": boundedText(80), "expected_revision": map[string]any{"type": "integer", "minimum": 1}, "password": credentialFields()["password"], "otp": boundedText(16)}, "id", "expected_revision", "password"), applicationResource, s.rotateApplicationSecret),
		identityBinding("applications_delete", "Delete an application and revoke every associated token", "applications:manage", false, object(map[string]any{"id": boundedText(80), "expected_revision": map[string]any{"type": "integer", "minimum": 1}, "password": credentialFields()["password"], "otp": boundedText(16)}, "id", "expected_revision", "password"), applicationResource, s.deleteApplication),
	}
}
func (s *Service) createApplication(ctx context.Context, a Actor, raw json.RawMessage) (ActionResult, error) {
	var in applicationCreate
	if e := decodeInput(raw, &in); e != nil {
		return ActionResult{}, e
	}
	if e := s.VerifyCredentials(ctx, a, in.Password, in.OTP); e != nil {
		return ActionResult{}, e
	}
	if e := s.validateLimitedACL(a, in.Scopes, in.Resources); e != nil {
		return ActionResult{}, e
	}
	if strings.TrimSpace(in.Name) == "" {
		return ActionResult{}, invalid("Application name is required")
	}
	if len(in.CIDRs) < 1 || len(in.CIDRs) > 32 {
		return ActionResult{}, invalid("Applications require 1–32 peer CIDRs")
	}
	for i, cidr := range in.CIDRs {
		_, network, e := net.ParseCIDR(cidr)
		if e != nil {
			return ActionResult{}, invalid("Application peer restrictions must be CIDR networks")
		}
		in.CIDRs[i] = network.String()
	}
	p := Application{ID: randomID("app_"), Name: in.Name, Scopes: in.Scopes, Resources: in.Resources, CIDRs: in.CIDRs, Enabled: true, Revision: 1, CreatedAt: nowString()}
	secret := randomID("appsecret_")
	_, e := s.db.ExecContext(ctx, "INSERT INTO applications(id,owner_id,name,secret_hash,scopes,resources,cidrs,enabled,revision,created_at) VALUES(?,?,?,?,?,?,?,1,1,?)", p.ID, a.ID, p.Name, digest(secret), jsonText(p.Scopes), jsonText(p.Resources), jsonText(p.CIDRs), p.CreatedAt)
	if e != nil {
		return ActionResult{}, internal()
	}
	return ActionResult{Data: map[string]any{"application": p, "secret": secret}, AuditSummary: map[string]any{"application_id": p.ID}}, nil
}
func scanApplication(row interface{ Scan(...any) error }) (Application, error) {
	var p Application
	var scopes, resources, cidrs string
	e := row.Scan(&p.ID, &p.Name, &scopes, &resources, &cidrs, &p.Enabled, &p.Revision, &p.CreatedAt)
	if e != nil {
		return p, e
	}
	if json.Unmarshal([]byte(scopes), &p.Scopes) != nil || json.Unmarshal([]byte(resources), &p.Resources) != nil || json.Unmarshal([]byte(cidrs), &p.CIDRs) != nil {
		return p, internal()
	}
	return p, nil
}
func (s *Service) listApplications(ctx context.Context, a Actor, _ json.RawMessage) (ActionResult, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT id,name,scopes,resources,cidrs,enabled,revision,created_at FROM applications WHERE owner_id=? ORDER BY created_at DESC LIMIT 1000", a.ID)
	if e != nil {
		return ActionResult{}, internal()
	}
	defer rows.Close()
	items := []Application{}
	for rows.Next() {
		p, e := scanApplication(rows)
		if e != nil {
			return ActionResult{}, internal()
		}
		if hasResource(a, "application:"+p.ID) {
			items = append(items, p)
		}
	}
	if rows.Err() != nil {
		return ActionResult{}, internal()
	}
	return ActionResult{Data: map[string]any{"items": items, "total": len(items)}}, nil
}

type applicationMutation struct {
	ID       string `json:"id"`
	Enabled  bool   `json:"enabled"`
	Expected int64  `json:"expected_revision"`
	Password string `json:"password"`
	OTP      string `json:"otp"`
}

func (s *Service) mutateApplication(ctx context.Context, a Actor, raw json.RawMessage, operation string) (ActionResult, error) {
	var in applicationMutation
	if e := decodeInput(raw, &in); e != nil {
		return ActionResult{}, e
	}
	if e := s.VerifyCredentials(ctx, a, in.Password, in.OTP); e != nil {
		return ActionResult{}, e
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return ActionResult{}, internal()
	}
	defer tx.Rollback()
	var owner string
	var revision int64
	if e = tx.QueryRowContext(ctx, "SELECT owner_id,revision FROM applications WHERE id=?", in.ID).Scan(&owner, &revision); e == sql.ErrNoRows {
		return ActionResult{}, missing("Application")
	} else if e != nil {
		return ActionResult{}, internal()
	}
	if owner != a.ID {
		return ActionResult{}, denied()
	}
	if revision != in.Expected {
		return ActionResult{}, &Error{Code: "revision_conflict", Message: "Application changed; reload it", Status: 412}
	}
	secret := ""
	switch operation {
	case "enabled":
		_, e = tx.ExecContext(ctx, "UPDATE applications SET enabled=?,revision=revision+1 WHERE id=?", in.Enabled, in.ID)
	case "rotate":
		secret = randomID("appsecret_")
		_, e = tx.ExecContext(ctx, "UPDATE applications SET secret_hash=?,revision=revision+1 WHERE id=?", digest(secret), in.ID)
	case "delete":
		_, e = tx.ExecContext(ctx, "DELETE FROM applications WHERE id=?", in.ID)
	}
	if e != nil {
		return ActionResult{}, internal()
	}
	if _, e = tx.ExecContext(ctx, "UPDATE tokens SET revoked=1 WHERE application_id=?", in.ID); e != nil {
		return ActionResult{}, internal()
	}
	if e = tx.Commit(); e != nil {
		return ActionResult{}, internal()
	}
	result := map[string]any{"id": in.ID, "revision": revision + 1, "leases_revoked": true}
	if operation == "enabled" {
		result["enabled"] = in.Enabled
	}
	if operation == "rotate" {
		result["secret"] = secret
	}
	if operation == "delete" {
		result["deleted"] = true
	}
	return ActionResult{Data: result}, nil
}
func (s *Service) setApplicationEnabled(c context.Context, a Actor, r json.RawMessage) (ActionResult, error) {
	return s.mutateApplication(c, a, r, "enabled")
}
func (s *Service) rotateApplicationSecret(c context.Context, a Actor, r json.RawMessage) (ActionResult, error) {
	return s.mutateApplication(c, a, r, "rotate")
}
func (s *Service) deleteApplication(c context.Context, a Actor, r json.RawMessage) (ActionResult, error) {
	return s.mutateApplication(c, a, r, "delete")
}
func peerAllowed(ctx context.Context, cidrs []string) bool {
	source, _ := ctx.Value(peerIPKey{}).(string)
	ip := net.ParseIP(source)
	if ip == nil {
		return false
	}
	for _, cidr := range cidrs {
		_, network, e := net.ParseCIDR(cidr)
		if e == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}
func (s *Service) ExchangeApplication(ctx context.Context, id, secret string) (TokenCreated, error) {
	if len(id) > 80 || len(secret) > 128 {
		return TokenCreated{}, unauthorized()
	}
	var owner, hash string
	p, e := scanApplication(s.db.QueryRowContext(ctx, "SELECT id,name,scopes,resources,cidrs,enabled,revision,created_at FROM applications WHERE id=?", id))
	if e != nil || !p.Enabled || !peerAllowed(ctx, p.CIDRs) {
		return TokenCreated{}, unauthorized()
	}
	if e = s.db.QueryRowContext(ctx, "SELECT owner_id,secret_hash FROM applications WHERE id=?", id).Scan(&owner, &hash); e != nil || subtle.ConstantTimeCompare([]byte(hash), []byte(digest(secret))) != 1 {
		return TokenCreated{}, unauthorized()
	}
	settings, e := s.PanelSettings(ctx)
	if e != nil {
		return TokenCreated{}, e
	}
	if !settings.APIEnabled {
		return TokenCreated{}, &Error{Code: "api_disabled", Message: "Panel API access is disabled", Status: 403}
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return TokenCreated{}, internal()
	}
	defer tx.Rollback()
	var active bool
	var revision int64
	if e = tx.QueryRowContext(ctx, "SELECT enabled,revision FROM applications WHERE id=? AND secret_hash=?", id, digest(secret)).Scan(&active, &revision); e != nil || !active || revision != p.Revision {
		return TokenCreated{}, unauthorized()
	}
	raw := randomID("plk_")
	tid := randomID("token_")
	now := unixNow()
	if _, e = tx.ExecContext(ctx, "INSERT INTO tokens(id,hash,owner_id,name,kind,scopes,resources,created,expires,application_id) VALUES(?,?,?,?,'api',?,?,?,?,?)", tid, digest(raw), owner, p.Name, jsonText(p.Scopes), jsonText(p.Resources), now, now+3600, id); e != nil {
		return TokenCreated{}, internal()
	}
	if e = tx.Commit(); e != nil {
		return TokenCreated{}, internal()
	}
	info := TokenInfo{ID: tid, Name: p.Name, Kind: "api", Scopes: p.Scopes, Resources: p.Resources, CreatedAt: time.Unix(now, 0).UTC().Format(time.RFC3339), ExpiresAt: time.Unix(now+3600, 0).UTC().Format(time.RFC3339)}
	s.recordAudit(ctx, Actor{ID: "token:" + tid, Kind: "api", Name: p.Name}, "applications_exchange", "application:"+id, nil, nil)
	return TokenCreated{TokenInfo: info, Token: raw}, nil
}
func (s *Service) checkTokenPolicy(ctx context.Context, id string, a Actor, checkPeer bool) error {
	settings, e := s.PanelSettings(ctx)
	if e != nil {
		return e
	}
	if !settings.APIEnabled {
		return &Error{Code: "api_disabled", Message: "Panel API access is disabled", Status: 403}
	}
	var appID string
	if e = s.db.QueryRowContext(ctx, "SELECT application_id FROM tokens WHERE id=?", id).Scan(&appID); e != nil {
		return unauthorized()
	}
	if appID == "" {
		return nil
	}
	app, e := scanApplication(s.db.QueryRowContext(ctx, "SELECT id,name,scopes,resources,cidrs,enabled,revision,created_at FROM applications WHERE id=?", appID))
	if e != nil || !app.Enabled || !subset(a.Scopes, app.Scopes, false) || !subset(a.Resources, app.Resources, true) || (checkPeer && !peerAllowed(ctx, app.CIDRs)) {
		return unauthorized()
	}
	return nil
}
