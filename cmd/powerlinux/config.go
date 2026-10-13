package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

const defaultServerConfig = "/etc/powerlinux/server.json"

type serverConfiguration struct {
	Version        int      `json:"version"`
	Listen         string   `json:"listen"`
	PublicURL      string   `json:"public_url"`
	AllowedOrigins []string `json:"allowed_origins"`
	SecureCookies  bool     `json:"secure_cookies"`
	TLSCert        string   `json:"tls_cert"`
	TLSKey         string   `json:"tls_key"`
}

func decodeServerConfiguration(raw []byte) (serverConfiguration, error) {
	var c serverConfiguration
	if len(raw) > 64<<10 {
		return c, errors.New("server configuration exceeds 64 KiB")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, errors.New("server configuration must be a known JSON object")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, errors.New("server configuration must contain exactly one JSON object")
	}
	if c.Version != 1 {
		return c, errors.New("unsupported server configuration version")
	}
	if c.Listen != "" {
		_, port, err := net.SplitHostPort(c.Listen)
		value, portErr := strconv.Atoi(port)
		if err != nil || portErr != nil || value < 1 || value > 65535 {
			return c, errors.New("configuration listen must contain a host and port")
		}
	}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || u.Host == "" || u.User != nil || u.Path != "/mcp" || u.RawQuery != "" || u.Fragment != "" {
			return c, errors.New("configuration public URL must be an absolute /mcp endpoint")
		}
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "https" && (u.Scheme != "http" || (u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()))) {
			return c, errors.New("configuration remote public URL must use HTTPS")
		}
	}
	for _, origin := range c.AllowedOrigins {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || (u.Scheme != "https" && u.Scheme != "http") {
			return c, errors.New("configuration origins must be exact HTTP(S) origins")
		}
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return c, errors.New("configuration TLS certificate and key must both be set")
	}
	for _, path := range []string{c.TLSCert, c.TLSKey} {
		if path != "" && !filepath.IsAbs(path) {
			return c, errors.New("configuration TLS paths must be absolute")
		}
	}
	return c, nil
}

func loadServerConfiguration(path string, required bool) (serverConfiguration, error) {
	var c serverConfiguration
	if path == "" {
		return c, nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && !required {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("read server configuration: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 || !ok || stat.Uid != 0 {
		return c, errors.New("server configuration must be a root-owned regular file without group/world write access")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return c, fmt.Errorf("open server configuration: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return c, errors.New("server configuration changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil {
		return c, fmt.Errorf("read server configuration: %w", err)
	}
	return decodeServerConfiguration(raw)
}
