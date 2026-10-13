package httpapi

import (
	"github.com/Aoody743/PowerLinux/internal/core"
	"net/http"
	"time"
)

func (h *handler) delegatedAuth(w http.ResponseWriter, r *http.Request, path string) {
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
	if path == "auth/application" {
		var in struct {
			ID     string `json:"id"`
			Secret string `json:"secret"`
		}
		if e := decode(w, r, &in); e != nil {
			fail(w, e)
			return
		}
		token, e := h.service.ExchangeApplication(r.Context(), in.ID, in.Secret)
		if e != nil {
			fail(w, e)
			return
		}
		success(w, token)
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if e := decode(w, r, &in); e != nil {
		fail(w, e)
		return
	}
	settings, e := h.service.PanelSettings(r.Context())
	if e != nil {
		fail(w, e)
		return
	}
	if settings.Maintenance {
		fail(w, &core.Error{Code: "maintenance", Message: "Panel login is temporarily paused", Status: 503})
		return
	}
	session, e := h.service.RedeemTemporary(r.Context(), in.Token)
	if e != nil {
		fail(w, e)
		return
	}
	expiry, _ := time.Parse(time.RFC3339, session.ExpiresAt)
	h.setCookie(w, r, session.ID, expiry)
	success(w, map[string]any{"authenticated": true, "user": session.Actor, "csrf_token": session.CSRFToken, "expires_at": session.ExpiresAt})
}
