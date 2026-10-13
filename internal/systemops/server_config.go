package systemops

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func ValidateServerConfig(config ServerConfig) error {
	if config.Version != 1 {
		return errors.New("server config version 1 required")
	}
	host, port, err := net.SplitHostPort(config.Listen)
	if err != nil {
		return errors.New("listen requires host:port")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return errors.New("invalid listen port")
	}
	loopback := host == "localhost"
	if address := net.ParseIP(host); address != nil {
		loopback = address.IsLoopback()
	}
	if (config.TLSCert == "") != (config.TLSKey == "") {
		return errors.New("TLS certificate/key must be configured together")
	}
	if !loopback && config.TLSCert == "" {
		return errors.New("non-loopback panel requires TLS")
	}
	for _, path := range []string{config.TLSCert, config.TLSKey} {
		if path != "" && (!filepath.IsAbs(path) || !strings.HasPrefix(filepath.Clean(path), "/etc/powerlinux/tls/")) {
			return errors.New("panel TLS must reference its owned TLS directory")
		}
	}
	if config.PublicURL != "" {
		value, err := url.Parse(config.PublicURL)
		if err != nil || value.Host == "" || value.Path != "/mcp" || value.User != nil || value.RawQuery != "" || value.Fragment != "" {
			return errors.New("public URL must be an explicit /mcp endpoint")
		}
		if value.Scheme != "https" {
			ip := net.ParseIP(value.Hostname())
			if value.Scheme != "http" || (value.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
				return errors.New("remote public URL requires HTTPS")
			}
		}
	}
	if len(config.AllowedOrigins) > 16 {
		return errors.New("origin limit exceeded")
	}
	for _, origin := range config.AllowedOrigins {
		value, err := url.Parse(origin)
		if err != nil || value.Host == "" || value.Path != "" || value.User != nil || value.RawQuery != "" || value.Fragment != "" {
			return errors.New("invalid trusted origin")
		}
		if value.Scheme != "https" {
			ip := net.ParseIP(value.Hostname())
			if value.Scheme != "http" || (value.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
				return errors.New("remote origins require HTTPS")
			}
		}
	}
	if config.TLSCert != "" && !config.SecureCookies {
		return errors.New("TLS panel cookies must be secure")
	}
	return nil
}
func readPrivateServerConfig(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return nil, errors.New("server configuration must be owned by Agent and not writable by others")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 64<<10+1))
	if err != nil {
		return nil, err
	}
	var config ServerConfig
	if err = strictJSON(raw, &config); err != nil {
		return nil, err
	}
	return raw, ValidateServerConfig(config)
}
func (e *HostExecutor) writeServerConfig(raw []byte) error {
	path := e.path("/etc/powerlinux/server.json")
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return errors.New("server config parent is not trusted")
	}
	file, err := os.CreateTemp(dir, ".server-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0640); err == nil {
		err = file.Chown(os.Geteuid(), e.apiGID)
	}
	if err == nil {
		_, err = file.Write(raw)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
func (e *HostExecutor) checkPanelHealth(ctx context.Context, config ServerConfig) error {
	host, port, _ := net.SplitHostPort(config.Listen)
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	scheme := "http"
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	if config.TLSCert != "" {
		scheme = "https"
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		pem, err := os.ReadFile(e.path(config.TLSCert))
		if err != nil {
			return err
		}
		if !roots.AppendCertsFromPEM(pem) {
			return errors.New("configured TLS certificate cannot establish health trust")
		}
		serverName := host
		if public, err := url.Parse(config.PublicURL); err == nil && public.Hostname() != "" {
			serverName = public.Hostname()
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: serverName}
	}
	client := &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("panel health redirects are not allowed")
	}}
	endpoint := scheme + "://" + net.JoinHostPort(host, port) + "/healthz"
	for i := 0; i < 30; i++ {
		request, _ := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
		response, err := client.Do(request)
		if err == nil {
			var body struct {
				Status string `json:"status"`
				Ready  bool   `json:"ready"`
			}
			err = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&body)
			response.Body.Close()
			if err == nil && response.StatusCode == 200 && body.Status == "ok" && body.Ready {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return errors.New("configured panel did not become healthy on the new endpoint")
}
