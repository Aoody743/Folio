package core

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
)

func (s *Service) gatewayBindings() []Binding {
	fields := credentialFields()
	fields["enabled"] = boolean()
	fields["basic_username"] = map[string]any{"type": "string", "pattern": validUsername.String()}
	fields["basic_password"] = map[string]any{"type": "string", "minLength": 12, "maxLength": 512, "format": "password"}
	fields["expected_revision"] = map[string]any{"type": "integer", "minimum": 0}
	return []Binding{identityBinding("settings_basic_auth_update", "Configure an additional browser entry gate with independent hashed credentials and revocable cookies", "account:manage", false, object(fields, "password", "enabled", "expected_revision"), hostIdentityResource, s.updateBasicGateway)}
}

func (s *Service) updateBasicGateway(ctx context.Context, a Actor, raw json.RawMessage) (ActionResult, error) {
	var in struct {
		Password      string `json:"password"`
		OTP           string `json:"otp"`
		Enabled       bool   `json:"enabled"`
		Username      string `json:"basic_username"`
		BasicPassword string `json:"basic_password"`
		Expected      int64  `json:"expected_revision"`
	}
	if err := decodeInput(raw, &in); err != nil {
		return ActionResult{}, err
	}
	if err := s.VerifyCredentials(ctx, a, in.Password, in.OTP); err != nil {
		return ActionResult{}, err
	}
	hash := ""
	if in.Enabled {
		if !validUsername.MatchString(in.Username) {
			return ActionResult{}, invalid("Invalid gateway username")
		}
		if err := validatePassword(in.BasicPassword); err != nil {
			return ActionResult{}, err
		}
		var err error
		hash, err = passwordHash(in.BasicPassword)
		if err != nil {
			return ActionResult{}, internal()
		}
	} else {
		in.Username = ""
	}
	p, err := s.PanelSettings(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	p.BasicEnabled = in.Enabled
	p.BasicUsername = in.Username
	p.Revision = in.Expected + 1
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ActionResult{}, internal()
	}
	defer tx.Rollback()
	var result sql.Result
	if in.Expected == 0 {
		result, err = tx.ExecContext(ctx, "INSERT INTO identity_settings(id,payload,revision) VALUES('panel',?,1) ON CONFLICT(id) DO NOTHING", jsonText(p))
	} else {
		result, err = tx.ExecContext(ctx, "UPDATE identity_settings SET payload=?,revision=revision+1 WHERE id='panel' AND revision=?", jsonText(p), in.Expected)
	}
	if err != nil {
		return ActionResult{}, internal()
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ActionResult{}, &Error{Code: "revision_conflict", Message: "Panel settings changed; reload before saving", Status: 412}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO basic_gateway(id,username,password_hash,enabled,revision) VALUES('panel',?,?,?,1) ON CONFLICT(id) DO UPDATE SET username=excluded.username,password_hash=excluded.password_hash,enabled=excluded.enabled,revision=revision+1", in.Username, hash, in.Enabled); err != nil {
		return ActionResult{}, internal()
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM gateway_sessions"); err != nil {
		return ActionResult{}, internal()
	}
	if err = tx.Commit(); err != nil {
		return ActionResult{}, internal()
	}
	return ActionResult{Data: p, AuditSummary: map[string]any{"basic_auth_enabled": in.Enabled}}, nil
}

// GatewayRequired is independent of native session/token authentication. A
// gateway cookie only opens the entry; it grants no panel action permissions.
func (s *Service) GatewayRequired(ctx context.Context) (bool, error) {
	var enabled bool
	err := s.db.QueryRowContext(ctx, "SELECT enabled FROM basic_gateway WHERE id='panel'").Scan(&enabled)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, internal()
	}
	return enabled, nil
}
func (s *Service) AuthenticateGatewaySession(ctx context.Context, raw string) error {
	if len(raw) < 32 || len(raw) > 128 {
		return unauthorized()
	}
	var valid bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM gateway_sessions s JOIN basic_gateway g ON g.id='panel' AND g.enabled=1 AND g.revision=s.revision WHERE s.hash=? AND s.expires>?)", digest(raw), unixNow()).Scan(&valid); err != nil {
		return internal()
	}
	if !valid {
		return unauthorized()
	}
	return nil
}
func (s *Service) AuthenticateBasicGateway(ctx context.Context, username, password string) (string, error) {
	if len(username) > 64 || len(password) > 512 {
		return "", unauthorized()
	}
	peer, _ := ctx.Value(peerIPKey{}).(string)
	now := unixNow()
	source := digest("gateway-source:" + peer)
	var failures int
	var window int64
	if err := s.db.QueryRowContext(ctx, "SELECT failures,window_start FROM login_attempts WHERE key=?", source).Scan(&failures, &window); err == nil && now-window < 300 && failures >= 8 {
		return "", &Error{Code: "rate_limited", Message: "Too many gateway attempts; retry later", Status: 429}
	}
	var stored, hash string
	var revision int64
	if err := s.db.QueryRowContext(ctx, "SELECT username,password_hash,revision FROM basic_gateway WHERE id='panel' AND enabled=1").Scan(&stored, &hash, &revision); err != nil {
		return "", unauthorized()
	}
	passwordOK := checkPassword(hash, password)
	if subtle.ConstantTimeCompare([]byte(stored), []byte(username)) != 1 || !passwordOK {
		_, _ = s.db.ExecContext(ctx, `INSERT INTO login_attempts(key,failures,window_start) VALUES(?,1,?) ON CONFLICT(key) DO UPDATE SET failures=CASE WHEN ?-window_start>=300 THEN 1 ELSE failures+1 END,window_start=CASE WHEN ?-window_start>=300 THEN ? ELSE window_start END`, source, now, now, now, now)
		s.recordAudit(ctx, Actor{ID: "anonymous", Kind: "human", Name: "anonymous"}, "auth_gateway", "host:local", unauthorized(), nil)
		return "", unauthorized()
	}
	token := randomID("gateway_")
	result, err := s.db.ExecContext(ctx, "INSERT INTO gateway_sessions(hash,revision,expires) SELECT ?,revision,? FROM basic_gateway WHERE id='panel' AND enabled=1 AND revision=? AND password_hash=? AND (SELECT count(*) FROM gateway_sessions WHERE expires>?)<4096", digest(token), now+3600, revision, hash, now)
	if err != nil {
		return "", internal()
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return "", unauthorized()
	}
	_, _ = s.db.ExecContext(ctx, "DELETE FROM gateway_sessions WHERE expires<=?", now)
	_, _ = s.db.ExecContext(ctx, "DELETE FROM login_attempts WHERE key=?", source)
	s.recordAudit(ctx, Actor{ID: "gateway", Kind: "human", Name: "gateway"}, "auth_gateway", "host:local", nil, nil)
	return token, nil
}

// RecoverPanelAccess is for an explicitly invoked local CLI recovery operation,
// never an HTTP route. It retains account passwords, MFA, tokens and data.
func (s *Service) RecoverPanelAccess(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return internal()
	}
	defer tx.Rollback()
	p, err := loadPanelSettings(ctx, tx)
	if err != nil {
		return err
	}
	p.LoginEntry = ""
	p.CAPTCHAEnabled = false
	p.Maintenance = false
	p.BasicEnabled = false
	p.BasicUsername = ""
	p.Revision++
	if _, err = tx.ExecContext(ctx, "INSERT INTO identity_settings(id,payload,revision) VALUES('panel',?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,revision=revision+1", jsonText(p), p.Revision); err != nil {
		return internal()
	}
	if _, err = tx.ExecContext(ctx, "UPDATE basic_gateway SET enabled=0,username='',password_hash='',revision=revision+1 WHERE id='panel'"); err != nil {
		return internal()
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM gateway_sessions"); err != nil {
		return internal()
	}
	if err = tx.Commit(); err != nil {
		return internal()
	}
	s.recordAudit(ctx, Actor{ID: "local-recovery", Kind: "local", Name: "local recovery"}, "settings_recover_access", "host:local", nil, nil)
	return nil
}
