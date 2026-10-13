package core

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math/big"
	"time"
)

type CAPTCHAChallenge struct {
	ID        string `json:"id"`
	Image     string `json:"image"`
	ExpiresAt string `json:"expires_at"`
}

func secureDigit(limit int) int {
	v, e := rand.Int(rand.Reader, big.NewInt(int64(limit)))
	if e != nil {
		panic(e)
	}
	return int(v.Int64())
}
func (s *Service) captchaHash(id, answer, source string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(jsonText([]string{"PowerLinux:CAPTCHA:v1", id, answer, source})))
	return hex.EncodeToString(mac.Sum(nil))
}
func captchaImage(answer string) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 212, 64))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{237, 241, 247, 255}}, image.Point{}, draw.Src)
	// Small bitmap numerals keep the generator dependency-free. Every digit
	// receives independent offsets and noise; answers never occur in metadata.
	glyphs := [10][7]string{
		{"01110", "11011", "11011", "11011", "11011", "11011", "01110"},
		{"00100", "01100", "00100", "00100", "00100", "00100", "01110"},
		{"01110", "11011", "00011", "00110", "01100", "11000", "11111"},
		{"01110", "11011", "00011", "00110", "00011", "11011", "01110"},
		{"00011", "00111", "01111", "11011", "11111", "00011", "00011"},
		{"11111", "11000", "11110", "00011", "00011", "11011", "01110"},
		{"01110", "11000", "11110", "11011", "11011", "11011", "01110"},
		{"11111", "00011", "00110", "00110", "01100", "01100", "01100"},
		{"01110", "11011", "11011", "01110", "11011", "11011", "01110"},
		{"01110", "11011", "11011", "01111", "00011", "00011", "01110"},
	}
	for i, digit := range answer {
		x := 12 + i*32 + secureDigit(5)
		y := 9 + secureDigit(10)
		ink := color.RGBA{uint8(20 + secureDigit(50)), uint8(25 + secureDigit(55)), uint8(40 + secureDigit(70)), 255}
		for row, line := range glyphs[int(digit-'0')] {
			for column, value := range line {
				if value == '1' {
					draw.Draw(img, image.Rect(x+column*4, y+row*5, x+column*4+4, y+row*5+5), &image.Uniform{C: ink}, image.Point{}, draw.Src)
				}
			}
		}
	}
	for i := 0; i < 160; i++ {
		img.Set(secureDigit(212), secureDigit(64), color.RGBA{uint8(110 + secureDigit(70)), uint8(120 + secureDigit(65)), uint8(140 + secureDigit(60)), 255})
	}
	var buffer bytes.Buffer
	if png.Encode(&buffer, img) != nil {
		panic("PNG encoder failed")
	}
	return buffer.Bytes()
}
func (s *Service) newCAPTCHA(ctx context.Context) (CAPTCHAChallenge, string, error) {
	source, _ := ctx.Value(peerIPKey{}).(string)
	now := unixNow()
	_, _ = s.db.ExecContext(ctx, "DELETE FROM captcha_challenges WHERE expires<=?", now)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return CAPTCHAChallenge{}, "", internal()
	}
	defer tx.Rollback()
	var total, perSource int
	if e = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(CASE WHEN source=? THEN 1 ELSE 0 END),0) FROM captcha_challenges", source).Scan(&total, &perSource); e != nil {
		return CAPTCHAChallenge{}, "", internal()
	}
	if total >= 512 || perSource >= 10 {
		return CAPTCHAChallenge{}, "", &Error{Code: "rate_limited", Message: "Too many active login challenges; retry after expiry", Status: 429}
	}
	answer := ""
	for i := 0; i < 6; i++ {
		answer += string(rune('0' + secureDigit(10)))
	}
	id := randomID("captcha_")
	expiry := now + 120
	if _, e = tx.ExecContext(ctx, "INSERT INTO captcha_challenges(id,answer_hash,source,expires) VALUES(?,?,?,?)", id, s.captchaHash(id, answer, source), source, expiry); e != nil {
		return CAPTCHAChallenge{}, "", internal()
	}
	if e = tx.Commit(); e != nil {
		return CAPTCHAChallenge{}, "", internal()
	}
	return CAPTCHAChallenge{ID: id, Image: "data:image/png;base64," + base64.StdEncoding.EncodeToString(captchaImage(answer)), ExpiresAt: time.Unix(expiry, 0).UTC().Format(time.RFC3339)}, answer, nil
}
func (s *Service) NewCAPTCHA(ctx context.Context) (CAPTCHAChallenge, error) {
	challenge, _, e := s.newCAPTCHA(ctx)
	return challenge, e
}
func (s *Service) verifyCAPTCHA(ctx context.Context, id, answer string) error {
	if len(id) > 80 || len(answer) > 16 {
		return unauthorized()
	}
	source, _ := ctx.Value(peerIPKey{}).(string)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return internal()
	}
	defer tx.Rollback()
	var expected string
	var expiry int64
	if e = tx.QueryRowContext(ctx, "SELECT answer_hash,expires FROM captcha_challenges WHERE id=? AND source=?", id, source).Scan(&expected, &expiry); e != nil {
		return &Error{Code: "captcha_invalid", Message: "Login challenge is invalid, expired or already used", Status: 401}
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM captcha_challenges WHERE id=?", id); e != nil {
		return internal()
	}
	if e = tx.Commit(); e != nil {
		return internal()
	}
	if expiry <= unixNow() || len(answer) != 6 || subtle.ConstantTimeCompare([]byte(expected), []byte(s.captchaHash(id, answer, source))) != 1 {
		return &Error{Code: "captcha_invalid", Message: "Login challenge is invalid, expired or already used", Status: 401}
	}
	return nil
}
func (s *Service) ProtectedLogin(ctx context.Context, username, password, otp, entry, challenge, answer string) (Session, error) {
	if e := s.CheckLoginEntry(ctx, entry); e != nil {
		return Session{}, e
	}
	settings, e := s.PanelSettings(ctx)
	if e != nil {
		return Session{}, e
	}
	if settings.CAPTCHAEnabled {
		if e = s.verifyCAPTCHA(ctx, challenge, answer); e != nil {
			return Session{}, e
		}
	}
	source, _ := ctx.Value(peerIPKey{}).(string)
	return s.Login(ctx, username, password, otp, source)
}
