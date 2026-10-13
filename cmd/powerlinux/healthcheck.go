package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type healthTarget struct {
	URL, ServerName string
	CertFile        string
}

func healthEndpoint(config serverConfiguration) (healthTarget, error) {
	address := config.Listen
	if address == "" {
		address = "127.0.0.1:9000"
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return healthTarget{}, errors.New("invalid health-check listen address")
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	} else if host == "::" {
		host = "::1"
	}
	scheme := "http"
	name := host
	if config.TLSCert != "" {
		scheme = "https"
		if config.PublicURL != "" {
			canonical, err := url.Parse(config.PublicURL)
			if err != nil {
				return healthTarget{}, err
			}
			name = canonical.Hostname()
		}
	}
	return healthTarget{URL: scheme + "://" + net.JoinHostPort(host, port) + "/healthz", ServerName: name, CertFile: config.TLSCert}, nil
}

func healthcheck(args []string) error {
	flags := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	path := flags.String("server-config", defaultServerConfig, "root-owned server configuration")
	listen := flags.String("listen", "", "override local address for a user-mode installation")
	expected := flags.String("expected-version", version, "require the newly installed version; empty skips only the version comparison")
	timeout := flags.Duration("timeout", 3*time.Second, "bounded health-check timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *timeout < 100*time.Millisecond || *timeout > 30*time.Second {
		return errors.New("invalid health-check arguments or timeout")
	}
	explicit := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "server-config" {
			explicit = true
		}
	})
	config, err := loadServerConfiguration(*path, explicit)
	if err != nil {
		return err
	}
	if *listen != "" {
		config.Listen = *listen
	}
	target, err := healthEndpoint(config)
	if err != nil {
		return err
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: *timeout}).DialContext, TLSHandshakeTimeout: *timeout}
	defer transport.CloseIdleConnections()
	if target.CertFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		chain, err := os.ReadFile(target.CertFile)
		if err != nil {
			return errors.New("read configured TLS certificate for health verification")
		}
		if !pool.AppendCertsFromPEM(chain) {
			return errors.New("configured TLS certificate is invalid")
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: target.ServerName}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: transport, Timeout: *timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("health checks must not redirect") }}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("health check connection failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned HTTP %d", response.StatusCode)
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		return errors.New("health check did not return JSON")
	}
	var body struct {
		Status  string `json:"status"`
		Ready   bool   `json:"ready"`
		Version string `json:"version"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4096))
	if err = decoder.Decode(&body); err != nil || body.Status != "ok" || !body.Ready {
		return errors.New("service is not ready")
	}
	if *expected != "" && body.Version != *expected {
		return errors.New("healthy process does not match the expected installed version")
	}
	fmt.Printf("PowerLinux %s healthy.\n", body.Version)
	return nil
}
