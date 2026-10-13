package terminalbridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/httpapi"
	"github.com/coder/websocket"
)

func request(t *testing.T, server *httptest.Server, path, method string, body any, cookie *http.Cookie, headers http.Header) int {
	t.Helper()
	var raw []byte
	if body != nil {
		raw = encoded(body)
	}
	r, err := http.NewRequest(method, server.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	for key, values := range headers {
		r.Header[key] = values
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}
func TestActualHTTPMiddlewareAndSharedPTY(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	session := sessionFrom(t, run(t, f, f.admin, "terminal_open", map[string]any{"target_id": "local"}))
	login, err := f.service.Login(ctx, "admin", "bridge test secure password", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: "pl_session", Value: login.ID}
	server := httptest.NewServer(httpapi.NewWithOptions(f.service, httpapi.Options{ResourceHandler: f.bridge}))
	defer server.Close()
	path := "/api/v1/terminal/sessions/" + session.ID
	if code := request(t, server, path+"/read", "GET", nil, nil, nil); code != 401 {
		t.Fatal("anonymous stream accepted", code)
	}
	if code := request(t, server, path+"/input", "POST", map[string]any{"data": base64.StdEncoding.EncodeToString([]byte("printf 'HTTP_NATIVE_RESULT'\n"))}, cookie, nil); code != 403 {
		t.Fatal("cookie write accepted without CSRF", code)
	}
	if code := request(t, server, path+"/input", "POST", map[string]any{"data": "eA=="}, cookie, http.Header{"X-Csrf-Token": {login.CSRFToken}, "Origin": {"https://attacker.example"}}); code != 403 {
		t.Fatal("foreign Origin accepted", code)
	}
	header := http.Header{"X-Csrf-Token": {login.CSRFToken}, "Idempotency-Key": {"http-frame-intent"}}
	if code := request(t, server, path+"/input", "POST", map[string]any{"data": base64.StdEncoding.EncodeToString([]byte("printf 'HTTP_NATIVE_RESULT'\n"))}, cookie, header); code != 200 {
		t.Fatal("real HTTP terminal input failed", code)
	}
	if code := request(t, server, path+"/read?limit=8", "GET", nil, cookie, nil); code != 200 {
		t.Fatal("real HTTP terminal read failed", code)
	}
	direct := httptest.NewRecorder()
	f.bridge.HandleResource(direct, httptest.NewRequest("GET", path+"/read", nil), f.admin)
	if direct.Code != 401 {
		t.Fatal("direct hook accepted forged Actor without middleware context")
	}
}
func TestRealWebSocketOutputIdempotentInputAndRevocation(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session := sessionFrom(t, run(t, f, f.admin, "terminal_open", map[string]any{"target_id": "local"}))
	_, created := token(t, f, []string{"terminal:connect", "terminal:share"}, []string{Resource("local")})
	server := httptest.NewServer(httpapi.NewWithOptions(f.service, httpapi.Options{ResourceHandler: f.bridge}))
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/terminal/sessions/" + session.ID + "/ws"
	connection, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + created.Token}, "Origin": {server.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	payload := map[string]any{"type": "input", "data": base64.StdEncoding.EncodeToString([]byte("printf 'WS_REAL_''NATIVE_VALUE\\n'\n")), "idempotency_key": "ws-real-intent-1"}
	if err = connection.Write(ctx, websocket.MessageText, encoded(payload)); err != nil {
		t.Fatal(err)
	}
	observed, ack := false, false
	for !observed || !ack {
		_, body, e := connection.Read(ctx)
		if e != nil {
			t.Fatal("actual WS output missing", e)
		}
		var envelope struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(body, &envelope) != nil {
			t.Fatal("invalid WS envelope")
		}
		if envelope.Type == "ack" {
			ack = true
		}
		if envelope.Type == "output" {
			var out struct {
				Frames []struct {
					Data string `json:"data"`
				} `json:"frames"`
			}
			if json.Unmarshal(envelope.Data, &out) != nil {
				t.Fatal("invalid WS terminal output")
			}
			for _, frame := range out.Frames {
				plain, _ := base64.StdEncoding.DecodeString(frame.Data)
				if strings.Contains(string(plain), "WS_REAL_NATIVE_VALUE") {
					observed = true
				}
			}
		}
	}
	if err = f.service.RevokeToken(ctx, f.admin, created.ID); err != nil {
		t.Fatal(err)
	}
	_ = connection.Write(ctx, websocket.MessageText, encoded(map[string]any{"type": "input", "data": base64.StdEncoding.EncodeToString([]byte("printf 'REVOKED_MUST_NOT_WRITE'\n")), "idempotency_key": "ws-revoked-frame"}))
	for {
		_, body, e := connection.Read(ctx)
		if e != nil {
			break
		}
		if bytes.Contains(body, []byte("REVOKED_MUST_NOT_WRITE")) {
			t.Fatal("revoked websocket executed input")
		}
	}
	actor, err := f.service.AuthenticateToken(context.Background(), created.Token)
	if err == nil || actor.ID != "" {
		t.Fatal("revocation was not persisted")
	}
}
func TestCookieWebsocketCSRFSubprotocolAndForeignOrigin(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session := sessionFrom(t, run(t, f, f.admin, "terminal_open", map[string]any{"target_id": "local"}))
	login, err := f.service.Login(ctx, "admin", "bridge test secure password", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpapi.NewWithOptions(f.service, httpapi.Options{ResourceHandler: f.bridge}))
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/terminal/sessions/" + session.ID + "/ws"
	headers := http.Header{"Cookie": {"pl_session=" + login.ID}, "Origin": {server.URL}}
	connection, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: headers})
	if err == nil {
		connection.CloseNow()
		t.Fatal("cookie websocket accepted without CSRF")
	}
	if response == nil || response.StatusCode != 403 {
		t.Fatal("missing CSRF wrong response")
	}
	headers.Set("Origin", "https://attacker.example")
	connection, response, err = websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: headers, Subprotocols: []string{"powerlinux-csrf." + login.CSRFToken}})
	if err == nil {
		connection.CloseNow()
		t.Fatal("foreign websocket origin accepted")
	}
	if response == nil || response.StatusCode != 403 {
		t.Fatal("foreign Origin wrong response")
	}
	headers.Set("Origin", server.URL)
	connection, _, err = websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: headers, Subprotocols: []string{"powerlinux-csrf." + login.CSRFToken}})
	if err != nil {
		t.Fatal("correct cookie websocket rejected", err)
	}
	connection.CloseNow()
}
