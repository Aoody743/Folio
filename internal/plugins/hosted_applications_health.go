package plugins

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// An empty configuration preserves existing applications with a strict 2xx/3xx
// default. Requests always target the application's owned loopback port.
type HostedAppHealthCheck struct {
	Path           string `json:"path,omitempty"`
	StatusCodes    []int  `json:"status_codes,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

func hostedAppHealthSchema() map[string]any {
	return obj(map[string]any{
		"path":            map[string]any{"type": "string", "minLength": 1, "maxLength": 2048},
		"status_codes":    map[string]any{"type": "array", "minItems": 1, "maxItems": 20, "items": map[string]any{"type": "integer", "minimum": 200, "maximum": 499}},
		"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 300},
	})
}

func validateHostedAppHealth(check HostedAppHealthCheck) error {
	if check.TimeoutSeconds < 0 || check.TimeoutSeconds > 300 || len(check.StatusCodes) > 20 {
		return failure("invalid_input", "Invalid health check timeout or status codes", 400)
	}
	path := check.Path
	if path == "" {
		path = "/"
	}
	parsed, err := url.ParseRequestURI(path)
	if err != nil || len(path) > 2048 || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\r\n\x00#") || parsed.IsAbs() || parsed.Host != "" {
		return failure("invalid_input", "Health check must use a local HTTP path", 400)
	}
	for _, code := range check.StatusCodes {
		if code < 200 || code > 499 {
			return failure("invalid_input", "Expected health status must be between 200 and 499", 400)
		}
	}
	return nil
}

func waitHostedAppReady(ctx context.Context, port int) error {
	return waitHostedAppHealthy(ctx, port, HostedAppHealthCheck{})
}

func waitHostedAppHealthy(ctx context.Context, port int, check HostedAppHealthCheck) error {
	if err := validateHostedAppHealth(check); err != nil {
		return err
	}
	path := check.Path
	if path == "" {
		path = "/"
	}
	seconds := check.TimeoutSeconds
	if seconds == 0 {
		seconds = 20
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 500 * time.Millisecond}).DialContext}
	client := &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer transport.CloseIdleConnections()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	lastStatus := 0
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("application health check failed (last HTTP status %d): %w", lastStatus, err)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+path, nil)
		if err != nil {
			return err
		}
		response, requestErr := client.Do(request)
		if requestErr == nil {
			lastStatus = response.StatusCode
			_ = response.Body.Close()
			healthy := lastStatus >= 200 && lastStatus < 400
			if len(check.StatusCodes) > 0 {
				healthy = false
				for _, code := range check.StatusCodes {
					if code == lastStatus {
						healthy = true
						break
					}
				}
			}
			if healthy {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("application health check failed (last HTTP status %d): %w", lastStatus, ctx.Err())
		case <-ticker.C:
		}
	}
}
