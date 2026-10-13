package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type TemporaryGrant struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Scopes           []string `json:"scopes"`
	Resources        []string `json:"resources"`
	ExpiresAt        string   `json:"expires_at"`
	SessionExpiresAt string   `json:"session_expires_at"`
	Consumed         bool     `json:"consumed"`
	Revoked          bool     `json:"revoked"`
	CreatedAt        string   `json:"created_at"`
	ConsumedAt       string   `json:"consumed_at,omitempty"`
	Source           string   `json:"source,omitempty"`
}

func (s *Service) temporaryBindings() []Binding {
	fields := credentialFields()
	fields["name"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 80}
	fields["scopes"] = aclSchema()
	fields["resources"] = aclSchema()
	fields["expires_in_seconds"] = map[string]any{"type": "integer", "minimum": 60, "maximum": 3600}
	fields["session_seconds"] = map[string]any{"type": "integer", "minimum": 60, "maximum": 3600}
	resource := func(_ context.Context, _ Actor, raw json.RawMessage) ([]string, error) {
		var in struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &in) != nil || len(in.ID) < 1 || len(in.ID) > 80 {
			return nil, invalid("Invalid temporary grant ID")
		}
		return []string{"temporary:" + in.ID}, nil
	}
	return []Binding{
		identityBinding("temporary_grants_create", "Create an explicitly scoped single-use login grant after password and MFA confirmation", "temporary:manage", false, object(fields, "name", "scopes", "resources", "expires_in_seconds", "session_seconds", "password"), hostIdentityResource, s.createTemporary),
		identityBinding("temporary_grants_list", "Read temporary grant state and real redemption metadata without login secrets", "temporary:manage", true, object(map[string]any{}), hostIdentityResource, s.listTemporary),
		identityBinding("temporary_grants_logs", "Read actual grant redemption logs without raw login secrets", "temporary:manage", true, object(map[string]any{}), hostIdentityResource, s.temporaryLogs),
		identityBinding("temporary_grants_revoke", "Revoke a login grant and any session it already created", "temporary:manage", false, object(map[string]any{"id": boundedText(80)}, "id"), resource, s.revokeTemporary),
	}
}
func (s *Service) createTemporary(ctx context.Context, a Actor, raw json.RawMessage) (ActionResult, error) {
	var in struct {
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		Resources []string `json:"resources"`
		Expires   int64    `json:"expires_in_seconds"`
		Session   int64    `json:"session_seconds"`
		Password  string   `json:"password"`
		OTP       string   `json:"otp"`
	}
	if e := decodeInput(raw, &in); e != nil {
		return ActionResult{}, e
	}
	if e := s.VerifyCredentials(ctx, a, in.Password, in.OTP); e != nil {
		return ActionResult{}, e
	}
	if e := s.validateLimitedACL(a, in.Scopes, in.Resources); e != nil {
		return ActionResult{}, e
	}
	if in.Expires < 60 || in.Expires > 3600 || in.Session < 60 || in.Session > 3600 {
		return ActionResult{}, invalid("Temporary access must last 60–3600 seconds")
	}
	secret := randomID("temporary_")
	id := randomID("grant_")
	now := unixNow()
	expiry := now + in.Expires
	sessionExpiry := expiry + in.Session
	_, e := s.db.ExecContext(ctx, "INSERT INTO temporary_grants(id,owner_id,name,hash,scopes,resources,expires,session_expires,created_at) VALUES(?,?,?,?,?,?,?,?,?)", id, a.ID, in.Name, digest(secret), jsonText(in.Scopes), jsonText(in.Resources), expiry, sessionExpiry, nowString())
	if e != nil {
		return ActionResult{}, internal()
	}
	return ActionResult{Data: map[string]any{"id": id, "token": secret, "fragment": "#temporary_login=" + secret, "expires_at": time.Unix(expiry, 0).UTC().Format(time.RFC3339), "session_seconds": in.Session}, AuditSummary: map[string]any{"grant_id": id}}, nil
}
func scanTemporary(row interface{ Scan(...any) error }) (TemporaryGrant, error) {
	var p TemporaryGrant
	var sc, res string
	var exp, sess int64
	e := row.Scan(&p.ID, &p.Name, &sc, &res, &exp, &sess, &p.Consumed, &p.Revoked, &p.CreatedAt, &p.ConsumedAt, &p.Source)
	if e != nil {
		return p, e
	}
	if json.Unmarshal([]byte(sc), &p.Scopes) != nil || json.Unmarshal([]byte(res), &p.Resources) != nil {
		return p, internal()
	}
	p.ExpiresAt = time.Unix(exp, 0).UTC().Format(time.RFC3339)
	p.SessionExpiresAt = time.Unix(sess, 0).UTC().Format(time.RFC3339)
	return p, nil
}
func (s *Service) listTemporary(ctx context.Context, a Actor, _ json.RawMessage) (ActionResult, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT id,name,scopes,resources,expires,session_expires,consumed,revoked,created_at,consumed_at,source FROM temporary_grants WHERE owner_id=? ORDER BY created_at DESC LIMIT 1000", a.ID)
	if e != nil {
		return ActionResult{}, internal()
	}
	defer rows.Close()
	items := []TemporaryGrant{}
	for rows.Next() {
		p, e := scanTemporary(rows)
		if e != nil {
			return ActionResult{}, internal()
		}
		if hasResource(a, "temporary:"+p.ID) {
			items = append(items, p)
		}
	}
	if rows.Err() != nil {
		return ActionResult{}, internal()
	}
	return ActionResult{Data: map[string]any{"items": items, "total": len(items)}}, nil
}
func (s *Service) temporaryLogs(ctx context.Context, a Actor, raw json.RawMessage) (ActionResult, error) {
	result, e := s.listTemporary(ctx, a, raw)
	if e != nil {
		return result, e
	}
	items := result.Data.(map[string]any)["items"].([]TemporaryGrant)
	logs := []map[string]any{}
	for _, p := range items {
		if p.Consumed {
			logs = append(logs, map[string]any{"grant_id": p.ID, "name": p.Name, "consumed_at": p.ConsumedAt, "source": p.Source, "revoked": p.Revoked, "actor": Actor{ID: "grant:" + p.ID, Name: p.Name, Kind: "temporary"}})
		}
	}
	return ActionResult{Data: map[string]any{"items": logs, "total": len(logs)}}, nil
}
func (s *Service) revokeTemporary(ctx context.Context, a Actor, raw json.RawMessage) (ActionResult, error) {
	var in struct {
		ID string `json:"id"`
	}
	if e := decodeInput(raw, &in); e != nil {
		return ActionResult{}, e
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return ActionResult{}, internal()
	}
	defer tx.Rollback()
	result, e := tx.ExecContext(ctx, "UPDATE temporary_grants SET revoked=1 WHERE id=? AND owner_id=?", in.ID, a.ID)
	if e != nil {
		return ActionResult{}, internal()
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ActionResult{}, missing("Temporary grant")
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM sessions WHERE grant_id=?", in.ID); e != nil {
		return ActionResult{}, internal()
	}
	if e = tx.Commit(); e != nil {
		return ActionResult{}, internal()
	}
	return ActionResult{Data: map[string]any{"id": in.ID, "revoked": true}}, nil
}
func (s *Service) RedeemTemporary(ctx context.Context, secret string) (Session, error) {
	if secret == "" || len(secret) > 128 {
		return Session{}, unauthorized()
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return Session{}, internal()
	}
	defer tx.Rollback()
	var id, owner, name, scopes, resources string
	var expiry, originalSessionExpiry int64
	if e = tx.QueryRowContext(ctx, "SELECT id,owner_id,name,scopes,resources,expires,session_expires FROM temporary_grants WHERE hash=? AND consumed=0 AND revoked=0 AND expires>?", digest(secret), unixNow()).Scan(&id, &owner, &name, &scopes, &resources, &expiry, &originalSessionExpiry); e != nil {
		return Session{}, unauthorized()
	}
	var present int
	if e = tx.QueryRowContext(ctx, "SELECT count(*) FROM users WHERE id=?", owner).Scan(&present); e != nil || present != 1 {
		return Session{}, unauthorized()
	}
	actor := Actor{ID: "grant:" + id, Kind: "temporary", Name: name}
	if json.Unmarshal([]byte(scopes), &actor.Scopes) != nil || json.Unmarshal([]byte(resources), &actor.Resources) != nil {
		return Session{}, internal()
	}
	now := unixNow()
	sessionExpiry := now + (originalSessionExpiry - expiry)
	source, _ := ctx.Value(peerIPKey{}).(string)
	result, e := tx.ExecContext(ctx, "UPDATE temporary_grants SET consumed=1,session_expires=?,consumed_at=?,source=? WHERE id=? AND consumed=0 AND revoked=0", sessionExpiry, nowString(), source, id)
	if e != nil {
		return Session{}, internal()
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return Session{}, unauthorized()
	}
	raw := randomID("pls_")
	csrf := randomID("csrf_")
	if _, e = tx.ExecContext(ctx, "INSERT INTO sessions(hash,user_id,csrf,expires,created,grant_id) VALUES(?,?,?,?,?,?)", digest(raw), owner, csrf, sessionExpiry, now, id); e != nil {
		return Session{}, internal()
	}
	if e = tx.Commit(); e != nil {
		return Session{}, internal()
	}
	s.recordAudit(ctx, actor, "temporary_redeem", "temporary:"+id, nil, map[string]any{"source": source})
	return Session{ID: raw, Actor: actor, CSRFToken: csrf, ExpiresAt: time.Unix(sessionExpiry, 0).UTC().Format(time.RFC3339)}, nil
}
func (s *Service) temporaryActor(ctx context.Context, id string) (Actor, error) {
	var a Actor
	var sc, res string
	e := s.db.QueryRowContext(ctx, "SELECT g.id,g.name,g.scopes,g.resources FROM temporary_grants g JOIN users u ON u.id=g.owner_id WHERE g.id=? AND g.consumed=1 AND g.revoked=0 AND g.session_expires>?", id, unixNow()).Scan(&a.ID, &a.Name, &sc, &res)
	if e == sql.ErrNoRows {
		return a, unauthorized()
	}
	if e != nil {
		return a, internal()
	}
	a.ID = "grant:" + a.ID
	a.Kind = "temporary"
	if json.Unmarshal([]byte(sc), &a.Scopes) != nil || json.Unmarshal([]byte(res), &a.Resources) != nil {
		return a, internal()
	}
	return a, nil
}

func revokeTemporaryForAccount(ctx context.Context, tx *sql.Tx, userID string) error {
	if _, err := tx.ExecContext(ctx, "UPDATE temporary_grants SET revoked=1 WHERE owner_id=?", userID); err != nil {
		return internal()
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=? AND grant_id<>''", userID); err != nil {
		return internal()
	}
	return nil
}
