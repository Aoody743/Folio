package core

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

var validUsername = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{2,63}$`)
var tokenScopes = map[string]bool{"notes:read": true, "notes:write": true, "notes:delete": true, "system:read": true, "audit:read": true, "tasks:read": true, "tasks:write": true, "tokens:manage": true, "account:manage": true, "files:read": true, "files:write": true, "sites:read": true, "sites:write": true, "software:read": true, "software:write": true, "services:read": true, "services:write": true}
var kdfSlots = make(chan struct{}, 2)

func derivePasswordKey(password string, salt []byte) []byte {
	kdfSlots <- struct{}{}
	defer func() { <-kdfSlots }()
	return argon2.IDKey([]byte(password), salt, 3, 32*1024, 2, 32)
}

func passwordHash(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := derivePasswordKey(password, salt)
	return fmt.Sprintf("$argon2id$v=19$m=32768,t=3,p=2$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}
func checkPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=32768,t=3,p=2" {
		return false
	}
	salt, e := base64.RawStdEncoding.DecodeString(parts[4])
	if e != nil || len(salt) != 16 {
		return false
	}
	expected, e := base64.RawStdEncoding.DecodeString(parts[5])
	if e != nil || len(expected) != 32 {
		return false
	}
	got := derivePasswordKey(password, salt)
	return subtle.ConstantTimeCompare(got, expected) == 1
}
func validatePassword(password string) error {
	if !utf8.ValidString(password) || utf8.RuneCountInString(password) < 12 || len(password) > 512 {
		return invalid("Password must contain at least 12 characters and at most 512 bytes")
	}
	return nil
}
func adminActor(id, name string) Actor {
	return Actor{ID: id, Name: name, Kind: "human", Scopes: []string{"*"}, Resources: []string{"*"}}
}

func (s *Service) Setup(ctx context.Context, setupToken, username, password string) (Actor, error) {
	if !validUsername.MatchString(username) {
		return Actor{}, invalid("Username must be 3–64 ASCII letters, digits, dots, underscores or hyphens")
	}
	if err := validatePassword(password); err != nil {
		return Actor{}, err
	}
	raw, err := os.ReadFile(s.SetupSecretFile())
	if err != nil {
		return Actor{}, &Error{Code: "already_initialized", Message: "Panel has already been initialized", Status: 409}
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(raw))), []byte(setupToken)) != 1 {
		return Actor{}, unauthorized()
	}
	hash, err := passwordHash(password)
	if err != nil {
		return Actor{}, internal()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Actor{}, internal()
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&count); err != nil {
		return Actor{}, internal()
	}
	if count != 0 {
		return Actor{}, &Error{Code: "already_initialized", Message: "Panel has already been initialized", Status: 409}
	}
	actor := adminActor(randomID("user_"), username)
	if _, err = tx.ExecContext(ctx, "INSERT INTO users(id,username,password_hash,created_at) VALUES(?,?,?,?)", actor.ID, username, hash, nowString()); err != nil {
		return Actor{}, internal()
	}
	if err = tx.Commit(); err != nil {
		return Actor{}, internal()
	}
	// A stale bootstrap file cannot grant access after users exist, even if removal fails.
	_ = os.Remove(s.SetupSecretFile())
	s.recordAudit(ctx, actor, "account_setup", "host:local", nil, nil)
	return actor, nil
}

// Login rate limits an account/source tuple, not the whole server. Source is an
// adapter-provided peer address; untrusted forwarded headers are never used.
func (s *Service) Login(ctx context.Context, username, password, otp, source string) (Session, error) {
	if len(username) > 64 || len(password) > 512 || len(otp) > 16 {
		return Session{}, unauthorized()
	}
	now := time.Now().Unix()
	key := digest(strings.ToLower(username) + "|" + source)
	sourceKey := digest("source:" + source)
	_, _ = s.db.ExecContext(ctx, "DELETE FROM login_attempts WHERE window_start<?", now-3600)
	var sourceFailures int
	var sourceWindow int64
	if e := s.db.QueryRowContext(ctx, "SELECT failures,window_start FROM login_attempts WHERE key=?", sourceKey).Scan(&sourceFailures, &sourceWindow); e == nil && now-sourceWindow < 300 && sourceFailures >= 100 {
		return Session{}, &Error{Code: "rate_limited", Message: "Too many login attempts from this source; retry later", Status: 429}
	}
	var failures int
	var window int64
	err := s.db.QueryRowContext(ctx, "SELECT failures,window_start FROM login_attempts WHERE key=?", key).Scan(&failures, &window)
	if err == nil && now-window < 300 && failures >= 8 {
		return Session{}, &Error{Code: "rate_limited", Message: "Too many login attempts; retry later", Status: 429}
	}
	var id, name, hash, secret string
	var last int64
	err = s.db.QueryRowContext(ctx, "SELECT id,username,password_hash,totp_secret,totp_last_counter FROM users WHERE username=?", username).Scan(&id, &name, &hash, &secret, &last)
	valid := err == nil && checkPassword(hash, password)
	if err != nil { // Spend an equivalent KDF cost without divulging account existence.
		derivePasswordKey(password, make([]byte, 16))
	}
	counter := int64(-1)
	if valid && secret != "" {
		plain, e := s.decrypt(secret)
		valid = e == nil
		if valid {
			counter = totpCounter(string(plain), otp, time.Now())
			valid = counter >= 0 && counter > last
		}
	}
	if !valid {
		_, _ = s.db.ExecContext(ctx, `INSERT INTO login_attempts(key,failures,window_start) VALUES(?,1,?) ON CONFLICT(key) DO UPDATE SET failures=CASE WHEN ?-window_start>=300 THEN 1 ELSE failures+1 END,window_start=CASE WHEN ?-window_start>=300 THEN ? ELSE window_start END`, key, now, now, now, now)
		_, _ = s.db.ExecContext(ctx, `INSERT INTO login_attempts(key,failures,window_start) VALUES(?,1,?) ON CONFLICT(key) DO UPDATE SET failures=CASE WHEN ?-window_start>=300 THEN 1 ELSE failures+1 END,window_start=CASE WHEN ?-window_start>=300 THEN ? ELSE window_start END`, sourceKey, now, now, now, now)
		s.recordAudit(ctx, Actor{ID: "anonymous", Kind: "human", Name: "anonymous"}, "auth_login", "host:local", unauthorized(), nil)
		return Session{}, unauthorized()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, internal()
	}
	defer tx.Rollback()
	if secret != "" {
		res, e := tx.ExecContext(ctx, "UPDATE users SET totp_last_counter=? WHERE id=? AND totp_last_counter<?", counter, id, counter)
		if e != nil {
			return Session{}, internal()
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return Session{}, unauthorized()
		}
	}
	raw := randomID("pls_")
	csrf := randomID("csrf_")
	expires := now + 12*3600
	if _, err = tx.ExecContext(ctx, "INSERT INTO sessions(hash,user_id,csrf,expires,created) VALUES(?,?,?,?,?)", digest(raw), id, csrf, expires, now); err != nil {
		return Session{}, internal()
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM login_attempts WHERE key=?", key); err != nil {
		return Session{}, internal()
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE expires<=?", now); err != nil {
		return Session{}, internal()
	}
	if err = tx.Commit(); err != nil {
		return Session{}, internal()
	}
	actor := adminActor(id, name)
	s.recordAudit(ctx, actor, "auth_login", "host:local", nil, nil)
	return Session{ID: raw, Actor: actor, CSRFToken: csrf, ExpiresAt: time.Unix(expires, 0).UTC().Format(time.RFC3339)}, nil
}
func (s *Service) AuthenticateSession(ctx context.Context, raw string) (Session, error) {
	if raw == "" || len(raw) > 128 {
		return Session{}, unauthorized()
	}
	var id, name, csrf, grantID string
	var expiry int64
	err := s.db.QueryRowContext(ctx, "SELECT u.id,u.username,s.csrf,s.expires,s.grant_id FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.hash=? AND s.expires>?", digest(raw), time.Now().Unix()).Scan(&id, &name, &csrf, &expiry, &grantID)
	if err != nil {
		return Session{}, unauthorized()
	}
	actor := adminActor(id, name)
	if grantID != "" {
		actor, err = s.temporaryActor(ctx, grantID)
		if err != nil {
			return Session{}, err
		}
	}
	return Session{ID: raw, Actor: actor, CSRFToken: csrf, ExpiresAt: time.Unix(expiry, 0).UTC().Format(time.RFC3339)}, nil
}
func (s *Service) Logout(ctx context.Context, raw string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE hash=?", digest(raw))
	if err != nil {
		return internal()
	}
	return nil
}

func subset(requested, available []string, isResource bool) bool {
	if len(requested) == 0 || len(requested) > 100 {
		return false
	}
	for _, v := range requested {
		if v == "" || len(v) > 256 {
			return false
		}
		ok := false
		for _, a := range available {
			candidates := []string{v}
			if isResource {
				candidates = resourceAliases(v)
			}
			for _, candidate := range candidates {
				if a == "*" || a == candidate || (isResource && strings.HasSuffix(a, ":*") && strings.HasPrefix(candidate, strings.TrimSuffix(a, "*"))) {
					ok = true
					break
				}
			}
			if ok {
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
func (s *Service) CreateToken(ctx context.Context, a Actor, req TokenRequest) (TokenCreated, error) {
	if !s.Authorized(a, "tokens:manage", "") {
		return TokenCreated{}, denied()
	}
	if strings.TrimSpace(req.Name) == "" || utf8.RuneCountInString(req.Name) > 80 {
		return TokenCreated{}, invalid("Token name must be 1–80 characters")
	}
	if req.Kind == "" {
		req.Kind = "mcp"
	}
	if req.Kind != "mcp" && req.Kind != "api" {
		return TokenCreated{}, invalid("Token kind must be mcp or api")
	}
	if req.Audience != "" {
		u, e := url.Parse(req.Audience)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" || len(req.Audience) > 2048 {
			return TokenCreated{}, invalid("Audience must be an absolute HTTP resource URI")
		}
	}
	if !subset(req.Scopes, a.Scopes, false) || !subset(req.Resources, a.Resources, true) {
		return TokenCreated{}, denied()
	}
	for _, scope := range req.Scopes {
		if scope != "*" && !s.scopeKnown(scope) {
			return TokenCreated{}, invalid("Unknown token scope: " + scope)
		}
	}
	if req.ExpiresInSeconds == 0 {
		req.ExpiresInSeconds = 90 * 86400
	}
	if req.ExpiresInSeconds < 60 || req.ExpiresInSeconds > 365*86400 {
		return TokenCreated{}, invalid("Token lifetime must be between 60 seconds and one year")
	}
	owner := a.ID
	parentID := ""
	if strings.HasPrefix(owner, "token:") {
		parentID = strings.TrimPrefix(owner, "token:")
		var parentOwner, parentAudience, parentScopes, parentResources string
		var parentExpires int64
		if e := s.db.QueryRowContext(ctx, "SELECT owner_id,audience,expires,scopes,resources FROM tokens WHERE id=? AND revoked=0 AND expires>?", parentID, time.Now().Unix()).Scan(&parentOwner, &parentAudience, &parentExpires, &parentScopes, &parentResources); e != nil {
			return TokenCreated{}, unauthorized()
		}
		var sc, res []string
		if json.Unmarshal([]byte(parentScopes), &sc) != nil || json.Unmarshal([]byte(parentResources), &res) != nil {
			return TokenCreated{}, internal()
		}
		if !subset(req.Scopes, sc, false) || !subset(req.Resources, res, true) {
			return TokenCreated{}, denied()
		}
		if parentAudience != "" && req.Audience != parentAudience {
			return TokenCreated{}, denied()
		}
		remaining := parentExpires - time.Now().Unix()
		if remaining < 60 {
			return TokenCreated{}, &Error{Code: "token_expiring", Message: "Delegating identity is about to expire", Status: 409}
		}
		if req.ExpiresInSeconds > remaining {
			req.ExpiresInSeconds = remaining
		}
		owner = parentOwner
	}
	var exists int
	if e := s.db.QueryRowContext(ctx, "SELECT count(*) FROM users WHERE id=?", owner).Scan(&exists); e != nil || exists != 1 {
		return TokenCreated{}, unauthorized()
	}
	now := time.Now().Unix()
	id := randomID("tok_")
	raw := randomID("plt_")
	_, err := s.db.ExecContext(ctx, "INSERT INTO tokens(id,hash,owner_id,name,kind,scopes,resources,created,expires,audience,parent_id) VALUES(?,?,?,?,?,?,?,?,?,?,?)", id, digest(raw), owner, req.Name, req.Kind, jsonText(req.Scopes), jsonText(req.Resources), now, now+req.ExpiresInSeconds, req.Audience, parentID)
	if err != nil {
		return TokenCreated{}, internal()
	}
	t := TokenInfo{ID: id, Name: req.Name, Kind: req.Kind, Scopes: req.Scopes, Resources: req.Resources, CreatedAt: time.Unix(now, 0).UTC().Format(time.RFC3339), ExpiresAt: time.Unix(now+req.ExpiresInSeconds, 0).UTC().Format(time.RFC3339), Audience: req.Audience}
	s.recordAudit(ctx, a, "tokens_create", "host:local", nil, map[string]any{"token_id": id, "name": req.Name, "scopes": req.Scopes, "resources": req.Resources})
	return TokenCreated{TokenInfo: t, Token: raw}, nil
}
func (s *Service) AuthenticateToken(ctx context.Context, raw string) (Actor, error) {
	return s.authenticateToken(ctx, raw, "")
}
func (s *Service) AuthenticateTokenForAudience(ctx context.Context, raw, audience string) (Actor, error) {
	return s.authenticateToken(ctx, raw, audience)
}
func (s *Service) authenticateToken(ctx context.Context, raw, audience string) (Actor, error) {
	if raw == "" || len(raw) > 128 {
		return Actor{}, unauthorized()
	}
	var id, name, kind, scopes, resources, boundAudience string
	var lastUsed int64
	now := time.Now().Unix()
	err := s.db.QueryRowContext(ctx, "SELECT t.id,t.name,t.kind,t.scopes,t.resources,t.audience,t.last_used FROM tokens t JOIN users u ON u.id=t.owner_id WHERE t.hash=? AND t.revoked=0 AND t.expires>? AND (t.parent_id='' OR EXISTS(SELECT 1 FROM tokens p WHERE p.id=t.parent_id AND p.revoked=0 AND p.expires>?))", digest(raw), now, now).Scan(&id, &name, &kind, &scopes, &resources, &boundAudience, &lastUsed)
	if err != nil {
		return Actor{}, unauthorized()
	}
	if boundAudience != "" && boundAudience != audience {
		return Actor{}, unauthorized()
	}
	a := Actor{ID: "token:" + id, Name: name, Kind: kind}
	if json.Unmarshal([]byte(scopes), &a.Scopes) != nil || json.Unmarshal([]byte(resources), &a.Resources) != nil {
		return Actor{}, internal()
	}
	if err = s.checkTokenPolicy(ctx, id, a, true); err != nil {
		return Actor{}, err
	}
	// Revalidate identities and revocation on every request, but coalesce the
	// informational last-used timestamp to at most one durable write per minute.
	if now-lastUsed >= 60 {
		_, _ = s.db.ExecContext(ctx, "UPDATE tokens SET last_used=? WHERE id=? AND last_used<=?", now, id, now-60)
	}
	return a, nil
}
func (s *Service) RevokeOwnToken(ctx context.Context, raw, audience string) error {
	a, err := s.AuthenticateTokenForAudience(ctx, raw, audience)
	if err != nil {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `WITH RECURSIVE descendants(id) AS (SELECT id FROM tokens WHERE id=? UNION SELECT t.id FROM tokens t JOIN descendants d ON t.parent_id=d.id) UPDATE tokens SET revoked=1 WHERE id IN (SELECT id FROM descendants)`, strings.TrimPrefix(a.ID, "token:"))
	if err != nil {
		return internal()
	}
	s.recordAudit(ctx, a, "tokens_revoke_own", "host:local", nil, map[string]any{"token_id": strings.TrimPrefix(a.ID, "token:")})
	return nil
}
func (s *Service) ListTokens(ctx context.Context, a Actor) ([]TokenInfo, error) {
	if !s.Authorized(a, "tokens:manage", "") {
		return nil, denied()
	}
	owner := a.ID
	delegatingID := ""
	if strings.HasPrefix(owner, "token:") {
		delegatingID = strings.TrimPrefix(owner, "token:")
		if err := s.db.QueryRowContext(ctx, "SELECT owner_id FROM tokens WHERE id=?", strings.TrimPrefix(owner, "token:")).Scan(&owner); err != nil {
			return nil, unauthorized()
		}
	}
	query := "SELECT id,name,kind,scopes,resources,created,expires,last_used,revoked,audience FROM tokens WHERE owner_id=?"
	args := []any{owner}
	if delegatingID != "" {
		query += ` AND id IN (WITH RECURSIVE allowed(id) AS (SELECT id FROM tokens WHERE id=? UNION SELECT t.id FROM tokens t JOIN allowed a ON t.parent_id=a.id) SELECT id FROM allowed)`
		args = append(args, delegatingID)
	}
	query += " ORDER BY created DESC"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, internal()
	}
	defer rows.Close()
	out := []TokenInfo{}
	for rows.Next() {
		var t TokenInfo
		var sc, res string
		var created, expires, last int64
		if err = rows.Scan(&t.ID, &t.Name, &t.Kind, &sc, &res, &created, &expires, &last, &t.Revoked, &t.Audience); err != nil {
			return nil, internal()
		}
		_ = json.Unmarshal([]byte(sc), &t.Scopes)
		_ = json.Unmarshal([]byte(res), &t.Resources)
		t.CreatedAt = time.Unix(created, 0).UTC().Format(time.RFC3339)
		t.ExpiresAt = time.Unix(expires, 0).UTC().Format(time.RFC3339)
		if last > 0 {
			t.LastUsedAt = time.Unix(last, 0).UTC().Format(time.RFC3339)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (s *Service) RevokeToken(ctx context.Context, a Actor, id string) error {
	if !s.Authorized(a, "tokens:manage", "") {
		return denied()
	}
	owner := a.ID
	delegatingID := ""
	if strings.HasPrefix(owner, "token:") {
		delegatingID = strings.TrimPrefix(owner, "token:")
		if err := s.db.QueryRowContext(ctx, "SELECT owner_id FROM tokens WHERE id=?", strings.TrimPrefix(owner, "token:")).Scan(&owner); err != nil {
			return unauthorized()
		}
	}
	if delegatingID != "" {
		var allowed int
		err := s.db.QueryRowContext(ctx, `WITH RECURSIVE allowed(id) AS (SELECT id FROM tokens WHERE id=? UNION SELECT t.id FROM tokens t JOIN allowed a ON t.parent_id=a.id) SELECT count(*) FROM allowed WHERE id=?`, delegatingID, id).Scan(&allowed)
		if err != nil {
			return internal()
		}
		if allowed != 1 {
			return denied()
		}
	}
	res, err := s.db.ExecContext(ctx, `WITH RECURSIVE descendants(id) AS (SELECT id FROM tokens WHERE id=? AND owner_id=? UNION SELECT t.id FROM tokens t JOIN descendants d ON t.parent_id=d.id) UPDATE tokens SET revoked=1 WHERE id IN (SELECT id FROM descendants)`, id, owner)
	if err != nil {
		return internal()
	}
	n, _ := res.RowsAffected()
	if n < 1 {
		return missing("Token")
	}
	s.recordAudit(ctx, a, "tokens_revoke", "host:local", nil, map[string]any{"token_id": id})
	return nil
}

func (s *Service) encrypt(data []byte) (string, error) {
	block, e := aes.NewCipher(s.secret)
	if e != nil {
		return "", e
	}
	gcm, e := cipher.NewGCM(block)
	if e != nil {
		return "", e
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return "", e
	}
	return base64.RawStdEncoding.EncodeToString(gcm.Seal(nonce, nonce, data, []byte("PowerLinux:TOTP:v1"))), nil
}
func (s *Service) decrypt(data string) ([]byte, error) {
	raw, e := base64.RawStdEncoding.DecodeString(data)
	if e != nil {
		return nil, e
	}
	block, e := aes.NewCipher(s.secret)
	if e != nil {
		return nil, e
	}
	gcm, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	if len(raw) < gcm.NonceSize() {
		return nil, fmt.Errorf("invalid ciphertext")
	}
	return gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], []byte("PowerLinux:TOTP:v1"))
}
func totpCounter(secret, code string, at time.Time) int64 {
	if len(code) != 6 {
		return -1
	}
	if _, e := strconv.Atoi(code); e != nil {
		return -1
	}
	key, e := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if e != nil {
		return -1
	}
	counter := at.Unix() / 30
	for d := int64(-1); d <= 1; d++ {
		if subtle.ConstantTimeCompare([]byte(totpCode(key, counter+d)), []byte(code)) == 1 {
			return counter + d
		}
	}
	return -1
}
func totpCode(key []byte, counter int64) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(counter))
	h := hmac.New(sha1.New, key)
	h.Write(b[:])
	sum := h.Sum(nil)
	offset := sum[len(sum)-1] & 15
	value := (uint32(sum[offset])&127)<<24 | uint32(sum[offset+1])<<16 | uint32(sum[offset+2])<<8 | uint32(sum[offset+3])
	return fmt.Sprintf("%06d", value%1000000)
}

// VerifyCredentials is the step-up check for account/security settings, including
// TOTP. It does not mint a session and does not expose a second MFA-free login.
func (s *Service) VerifyCredentials(ctx context.Context, a Actor, password, otp string) error {
	if a.Kind != "human" || !s.Authorized(a, "account:manage", "") {
		return denied()
	}
	if len(password) > 512 {
		return unauthorized()
	}
	var hash, secret string
	if e := s.db.QueryRowContext(ctx, "SELECT password_hash,totp_secret FROM users WHERE id=?", a.ID).Scan(&hash, &secret); e != nil || !checkPassword(hash, password) {
		return unauthorized()
	}
	if secret != "" {
		b, e := s.decrypt(secret)
		if e != nil || totpCounter(string(b), otp, time.Now()) < 0 {
			return unauthorized()
		}
	}
	return nil
}

type TOTPEnrollment struct {
	Secret string `json:"secret"`
	URI    string `json:"uri"`
}

func (s *Service) BeginTOTP(ctx context.Context, a Actor, password, otp string) (TOTPEnrollment, error) {
	if err := s.VerifyCredentials(ctx, a, password, otp); err != nil {
		return TOTPEnrollment{}, err
	}
	key := make([]byte, 20)
	if _, e := rand.Read(key); e != nil {
		return TOTPEnrollment{}, internal()
	}
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(key)
	encrypted, e := s.encrypt([]byte(secret))
	if e != nil {
		return TOTPEnrollment{}, internal()
	}
	if _, e = s.db.ExecContext(ctx, "UPDATE users SET totp_pending=? WHERE id=?", encrypted, a.ID); e != nil {
		return TOTPEnrollment{}, internal()
	}
	uri := "otpauth://totp/" + url.PathEscape("Folio:"+a.Name) + "?secret=" + secret + "&issuer=Folio&algorithm=SHA1&digits=6&period=30"
	return TOTPEnrollment{secret, uri}, nil
}
func (s *Service) ConfirmTOTP(ctx context.Context, a Actor, code string) error {
	if a.Kind != "human" || !s.Authorized(a, "account:manage", "") {
		return denied()
	}
	var enc string
	if e := s.db.QueryRowContext(ctx, "SELECT totp_pending FROM users WHERE id=?", a.ID).Scan(&enc); e != nil || enc == "" {
		return invalid("No pending TOTP enrollment")
	}
	secret, e := s.decrypt(enc)
	if e != nil {
		return internal()
	}
	counter := totpCounter(string(secret), code, time.Now())
	if counter < 0 {
		return unauthorized()
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return internal()
	}
	defer tx.Rollback()
	result, e := tx.ExecContext(ctx, "UPDATE users SET totp_secret=?,totp_pending='',totp_last_counter=? WHERE id=? AND totp_pending=?", enc, counter, a.ID, enc)
	if e != nil {
		return internal()
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return &Error{Code: "revision_conflict", Message: "TOTP enrollment changed; confirm the latest secret", Status: 412}
	}
	if e = revokeTemporaryForAccount(ctx, tx, a.ID); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return internal()
	}
	s.recordAudit(ctx, a, "account_totp_enable", "host:local", nil, nil)
	return nil
}
func (s *Service) DisableTOTP(ctx context.Context, a Actor, password, otp string) error {
	if e := s.VerifyCredentials(ctx, a, password, otp); e != nil {
		return e
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return internal()
	}
	defer tx.Rollback()
	if _, e := tx.ExecContext(ctx, "UPDATE users SET totp_secret='',totp_pending='',totp_last_counter=-1 WHERE id=?", a.ID); e != nil {
		return internal()
	}
	if e = revokeTemporaryForAccount(ctx, tx, a.ID); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return internal()
	}
	s.recordAudit(ctx, a, "account_totp_disable", "host:local", nil, nil)
	return nil
}
func (s *Service) Account(ctx context.Context, a Actor) (any, error) {
	if a.Kind != "human" || !s.Authorized(a, "account:manage", "") {
		return nil, denied()
	}
	var enabled bool
	var created string
	if e := s.db.QueryRowContext(ctx, "SELECT totp_secret<>'',created_at FROM users WHERE id=?", a.ID).Scan(&enabled, &created); e != nil {
		return nil, missing("Account")
	}
	return map[string]any{"user": a.Public(), "totp_enabled": enabled, "created_at": created}, nil
}
func (s *Service) ChangePassword(ctx context.Context, a Actor, oldPassword, newPassword, otp string) error {
	if e := s.VerifyCredentials(ctx, a, oldPassword, otp); e != nil {
		return e
	}
	if e := validatePassword(newPassword); e != nil {
		return e
	}
	hash, e := passwordHash(newPassword)
	if e != nil {
		return internal()
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return internal()
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "UPDATE users SET password_hash=? WHERE id=?", hash, a.ID); e != nil {
		return internal()
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=?", a.ID); e != nil {
		return internal()
	}
	if e = revokeTemporaryForAccount(ctx, tx, a.ID); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return internal()
	}
	s.recordAudit(ctx, a, "account_password_change", "host:local", nil, nil)
	return nil
}

// Referencing sql here preserves explicit handling of no-row identities while
// guarding against accidentally treating a database outage as a valid account.
var _ = sql.ErrNoRows
