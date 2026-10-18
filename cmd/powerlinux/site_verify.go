package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/sites"
)

func websiteTLSConfig(certificate []byte, host string) (*tls.Config, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificate) {
		return nil, errors.New("invalid deployed website certificate")
	}
	block, _ := pem.Decode(certificate)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("invalid deployed website leaf certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	expected := sha256.Sum256(leaf.Raw)
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: host, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 || sha256.Sum256(state.PeerCertificates[0].Raw) != expected {
			return errors.New("running Web cycle serves a different certificate")
		}
		return nil
	}}, nil
}

// A successful reload signal does not prove Nginx accepted its new cycle.
// Require the rendered resource/revision on a fresh local connection.
func (r siteRuntimes) VerifySite(ctx context.Context, site sites.Site) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	domains := append([]string{}, site.Domains...)
	for _, binding := range site.DirectoryBindings {
		domains = append(domains, binding.Domains...)
	}
	seen := map[string]bool{}
	for _, value := range domains {
		host, port := value, "80"
		if h, p, err := net.SplitHostPort(value); err == nil {
			host, port = h, p
		}
		host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
		if strings.HasPrefix(host, "*.") {
			host = "pl-health." + strings.TrimPrefix(host, "*.")
		}
		checks := []struct{ scheme, port string }{{"http", port}}
		if site.TLS.Enabled {
			checks = append(checks, struct{ scheme, port string }{"https", "443"})
			if port == "443" {
				checks = checks[1:]
			}
		}
		for _, check := range checks {
			key := host + ":" + check.scheme + ":" + check.port
			if seen[key] {
				continue
			}
			seen[key] = true
			transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: 2 * time.Second}
			if check.scheme == "https" {
				certificate, err := os.ReadFile(filepath.Join(r.stateDir, "certificates", site.TLS.CertificateID+".crt"))
				if err != nil {
					return err
				}
				transport.TLSClientConfig, err = websiteTLSConfig(certificate, host)
				if err != nil {
					return err
				}
			}
			client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			matched := false
			for !matched {
				request, err := http.NewRequestWithContext(ctx, "GET", check.scheme+"://"+net.JoinHostPort("127.0.0.1", check.port)+"/.__powerlinux_health", nil)
				if err != nil {
					return err
				}
				request.Host = host
				if strings.Contains(host, ":") {
					request.Host = "[" + host + "]"
				}
				response, err := client.Do(request)
				if err == nil {
					siteHeader := response.Header.Get("X-Folio-Site")
					if siteHeader == "" {
						siteHeader = response.Header.Get("X-PowerLinux-Site") // Older generated sites remain manageable during upgrade.
					}
					revisionHeader := response.Header.Get("X-Folio-Revision")
					if revisionHeader == "" {
						revisionHeader = response.Header.Get("X-PowerLinux-Revision")
					}
					matched = siteHeader == site.ID && revisionHeader == strconv.FormatInt(site.Revision, 10)
					// Advanced configuration is explicitly authorized root code. Its
					// headers are user controlled; TLS still must serve the exact leaf.
					if site.ConfigurationMode == "advanced" {
						matched = true
					}
					_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 512))
					response.Body.Close()
				}
				if matched {
					break
				}
				select {
				case <-ctx.Done():
					transport.CloseIdleConnections()
					return errors.New("running Web cycle does not match the website revision")
				case <-time.After(100 * time.Millisecond):
				}
			}
			transport.CloseIdleConnections()
		}
	}
	return nil
}
