package mcpbridge

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPublicHTTPSRequiresTLSOrActualLoopbackProxy(t *testing.T) {
	service, _ := fixture(t)
	handler, err := NewHTTP(service, HTTPOptions{PublicURL: "https://panel.example/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, peer, forwarded string
		tls                   bool
		status                int
	}{
		{"remote plaintext", "203.0.113.8:32100", "", false, 426},
		{"forged forwarded peer", "203.0.113.8:32100", "127.0.0.1", false, 426},
		{"actual TLS", "203.0.113.8:32100", "", true, 200},
		{"local IPv4 TLS proxy", "127.0.0.1:32100", "203.0.113.8", false, 200},
		{"local IPv6 TLS proxy", "[::1]:32100", "", false, 200},
		{"unresolved peer name", "localhost:32100", "", false, 426},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://panel.example/.well-known/oauth-authorization-server", nil)
			request.RemoteAddr = test.peer
			request.Header.Set("X-Forwarded-For", test.forwarded)
			request.Header.Set("X-Forwarded-Proto", "https")
			if test.tls {
				request.TLS = &tls.ConnectionState{}
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("peer %s tls %v: status=%d want=%d body=%s", test.peer, test.tls, response.Code, test.status, response.Body.String())
			}
		})
	}
}
