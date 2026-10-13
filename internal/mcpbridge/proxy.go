package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ProxyOptions uses an existing PowerLinux service. Token is a scoped core API
// token, never an identity supplied by tool arguments. Do not put tokens in CLI
// arguments or logs: load them from protected configuration in the caller.
type ProxyOptions struct{ Endpoint, Token string }

type httpSource struct {
	endpoint, token string
	client          *http.Client
}

func newHTTPSource(options ProxyOptions) (*httpSource, error) {
	u, err := validatedEndpoint(options.Endpoint)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(options.Token) == "" || strings.ContainsAny(options.Token, "\r\n") {
		return nil, errors.New("a scoped PowerLinux API token is required")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Ignore ambient HTTP_PROXY for the local authenticated connection.
	transport.Proxy = nil
	return &httpSource{endpoint: strings.TrimSuffix(u.String(), "/"), token: options.Token, client: &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func validatedEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("endpoint must be a PowerLinux HTTP(S) origin without credentials, query or path")
	}
	if u.Scheme == "https" {
		return u, nil
	}
	if u.Scheme != "http" || !loopbackHost(u.Hostname()) {
		return nil, errors.New("plaintext HTTP is allowed only for an explicit loopback endpoint; use verified HTTPS remotely")
	}
	return u, nil
}

func loopbackHost(host string) bool {
	// DNS names other than localhost are not treated as trusted loopback.
	if host == "localhost" {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func (s *httpSource) request(ctx context.Context, method, path string, input json.RawMessage, output any, keys ...string) error {
	req, err := http.NewRequestWithContext(ctx, method, s.endpoint+path, bytes.NewReader(input))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/json")
	if len(keys) > 0 && keys[0] != "" {
		req.Header.Set("Idempotency-Key", keys[0])
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := s.client.Do(req)
	if err != nil {
		return &core.Error{Code: "backend_unavailable", Message: "Cannot reach the configured PowerLinux backend", Status: 503}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxRequestBytes+1))
	if err != nil {
		return err
	}
	if len(body) > MaxRequestBytes {
		return &core.Error{Code: "result_too_large", Message: "Backend result exceeds the response limit", Status: 413}
	}
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error *core.Error     `json:"error"`
	}
	if err = json.Unmarshal(body, &envelope); err != nil {
		return &core.Error{Code: "backend_protocol_error", Message: "Backend did not return a valid JSON response", Status: 502}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if envelope.Error != nil {
			envelope.Error.Status = response.StatusCode
			return envelope.Error
		}
		return &core.Error{Code: "backend_error", Message: "Backend rejected the operation", Status: response.StatusCode}
	}
	if envelope.Error != nil {
		return envelope.Error
	}
	if len(envelope.Data) == 0 {
		return &core.Error{Code: "backend_protocol_error", Message: "Backend response is missing data", Status: 502}
	}
	return json.Unmarshal(envelope.Data, output)
}

func (s *httpSource) Actions(ctx context.Context) ([]core.Action, error) {
	var result []core.Action
	err := s.request(ctx, http.MethodGet, "/api/v1/capabilities", nil, &result)
	return result, err
}
func (s *httpSource) Verify(ctx context.Context) error { _, err := s.Actions(ctx); return err }
func (s *httpSource) Execute(ctx context.Context, name string, input json.RawMessage) (any, error) {
	var result any
	err := s.request(ctx, http.MethodPost, "/api/v1/actions/"+url.PathEscape(name), input, &result)
	return result, err
}
func (s *httpSource) ExecuteWithKey(ctx context.Context, name string, input json.RawMessage, key string) (any, error) {
	var result any
	err := s.request(ctx, http.MethodPost, "/api/v1/actions/"+url.PathEscape(name), input, &result, key)
	return result, err
}

// RunStdio writes only MCP protocol messages to stdout. It never opens the
// backend database; business actions and authorization stay in the service.
func RunStdio(ctx context.Context, options ProxyOptions) error {
	return ServeIO(ctx, options, os.Stdin, os.Stdout)
}

// ServeIO is the same line-delimited stdio transport on caller-provided pipes.
func ServeIO(ctx context.Context, options ProxyOptions, reader io.ReadCloser, writer io.WriteCloser) error {
	source, err := newHTTPSource(options)
	if err != nil {
		return err
	}
	defer source.client.CloseIdleConnections()
	server, err := newServer(ctx, source)
	if err != nil {
		return fmt.Errorf("connect authorized PowerLinux registry: %w", safeError(err))
	}
	return server.Run(ctx, &mcp.IOTransport{Reader: &lineBoundedReader{ReadCloser: reader}, Writer: writer})
}

type lineBoundedReader struct {
	io.ReadCloser
	size int
}

func (r *lineBoundedReader) Read(buffer []byte) (int, error) {
	n, err := r.ReadCloser.Read(buffer)
	for i, b := range buffer[:n] {
		if b == '\n' {
			r.size = 0
			continue
		}
		r.size++
		if r.size > MaxRequestBytes {
			return i, errors.New("MCP stdio message exceeds the request limit")
		}
	}
	return n, err
}
