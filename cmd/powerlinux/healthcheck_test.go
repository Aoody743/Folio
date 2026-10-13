package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHealthcheckRequiresActualReadinessAndExpectedVersion(t *testing.T) {
	var body atomic.Value
	body.Store(`{"status":"ok","ready":true,"version":"` + version + `"}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("wrong probe path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body.Load().(string)))
	}))
	defer server.Close()
	args := []string{"--listen", strings.TrimPrefix(server.URL, "http://"), "--server-config", defaultServerConfig}
	// Explicit configuration must exist; optional defaults remain usable for
	// isolated ordinary-user installs whose configuration is CLI-only.
	args = args[:2]
	if err := healthcheck(args); err != nil {
		t.Fatal(err)
	}
	body.Store(`{"status":"ok","ready":true,"version":"old-version"}`)
	if healthcheck(args) == nil {
		t.Fatal("old running process accepted after upgrade")
	}
	body.Store(`{"status":"ok","ready":false,"version":"` + version + `"}`)
	if healthcheck(args) == nil {
		t.Fatal("unready service accepted")
	}
}

func TestHealthEndpointUsesBoundAddressAndVerifiedTLSName(t *testing.T) {
	for _, row := range []struct{ listen, want string }{{"0.0.0.0:9000", "http://127.0.0.1:9000/healthz"}, {"[::]:9443", "http://[::1]:9443/healthz"}, {"192.0.2.4:9000", "http://192.0.2.4:9000/healthz"}} {
		target, err := healthEndpoint(serverConfiguration{Listen: row.listen})
		if err != nil || target.URL != row.want {
			t.Fatalf("%s: %+v %v", row.listen, target, err)
		}
	}
	target, err := healthEndpoint(serverConfiguration{Listen: "0.0.0.0:9443", TLSCert: "/etc/powerlinux/tls/cert.pem", PublicURL: "https://panel.example:9443/mcp"})
	if err != nil || target.URL != "https://127.0.0.1:9443/healthz" || target.ServerName != "panel.example" {
		t.Fatalf("TLS probe must retain certificate hostname: %+v %v", target, err)
	}
}
