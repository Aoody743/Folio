package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestServerConfigRejectsUntrustedFileAndSymlink(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "server.json")
	if err := os.WriteFile(name, []byte(`{"version":1,"listen":"127.0.0.1:9000"}`), 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := loadServerConfiguration(name, true); err == nil {
		t.Fatal("untrusted/writable server configuration accepted")
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(name, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadServerConfiguration(link, true); err == nil {
		t.Fatal("configuration symlink accepted")
	}
	missing := filepath.Join(dir, "missing")
	if _, err := loadServerConfiguration(missing, false); err != nil {
		t.Fatal("absent optional configuration changed the safe default", err)
	}
	if _, err := loadServerConfiguration(missing, true); err == nil {
		t.Fatal("missing explicitly requested configuration ignored")
	}
}

func TestServerConfigRejectsInvalidInputAndCredentialURLs(t *testing.T) {
	for _, raw := range []string{
		`{"version":1,"shell":"/bin/bash"}`,
		`{"version":1} {"version":1}`,
		`{"version":2}`,
		`{"version":1,"listen":"127.0.0.1:99999"}`,
		`{"version":1,"listen":"127.0.0.1:0"}`,
		`{"version":1,"public_url":"http://panel.example/mcp"}`,
		`{"version":1,"public_url":"https://user:private@panel.example/mcp"}`,
		`{"version":1,"allowed_origins":["https://panel.example/path"]}`,
		`{"version":1,"tls_cert":"/etc/powerlinux/tls/cert.pem"}`,
	} {
		if _, err := decodeServerConfiguration([]byte(raw)); err == nil {
			t.Fatalf("invalid configuration accepted: %s", raw)
		}
	}
	c, err := decodeServerConfiguration([]byte(`{"version":1,"listen":"[::1]:9000","public_url":"https://panel.example/mcp","allowed_origins":["https://panel.example"],"secure_cookies":true}`))
	if err != nil || c.Listen != "[::1]:9000" || !c.SecureCookies {
		t.Fatal("valid loopback proxy configuration rejected", err)
	}
}
