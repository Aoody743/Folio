//go:build linux

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type PeerCredentials struct {
	PID int32  `json:"pid"`
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

// Credentials are obtained from the kernel, never from request headers.
func UnixPeerCredentials(conn *net.UnixConn) (PeerCredentials, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return PeerCredentials{}, err
	}
	var credentials *syscall.Ucred
	var credentialError error
	if err := raw.Control(func(fd uintptr) {
		credentials, credentialError = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return PeerCredentials{}, err
	}
	if credentialError != nil {
		return PeerCredentials{}, credentialError
	}
	return PeerCredentials{PID: credentials.Pid, UID: credentials.Uid, GID: credentials.Gid}, nil
}

type PeerConn struct {
	*net.UnixConn
	Credentials PeerCredentials
}

type PeerListener struct {
	listener *net.UnixListener
	allowed  map[uint32]bool
}
type UnixOptions struct {
	Path        string
	AllowedUIDs []uint32
	SocketGID   *uint32
}

// A privileged Agent keeps socket ownership and grants only the panel group
// mode 0660. Peer UID authentication remains mandatory regardless of mode.
func ListenUnixWithOptions(options UnixOptions) (*PeerListener, error) {
	listener, err := ListenUnix(options.Path, options.AllowedUIDs)
	if err != nil {
		return nil, err
	}
	if options.SocketGID != nil {
		if os.Geteuid() != 0 {
			listener.Close()
			return nil, errors.New("group-owned Agent sockets require the privileged Agent")
		}
		if err := os.Chown(options.Path, 0, int(*options.SocketGID)); err != nil {
			listener.Close()
			return nil, err
		}
		if err := os.Chmod(options.Path, 0660); err != nil {
			listener.Close()
			return nil, err
		}
	}
	return listener, nil
}

// ListenUnix requires a private parent directory, refuses to overwrite any
// existing socket, and admits only explicitly configured kernel peer UIDs.
func ListenUnix(path string, allowedUIDs []uint32) (*PeerListener, error) {
	if !filepath.IsAbs(path) || len(allowedUIDs) == 0 {
		return nil, errors.New("absolute socket path and explicit peer UIDs required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return nil, errors.New("socket parent must be a directory not writable by group or others")
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, errors.New("socket path already exists; refusing to overwrite")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	// A root Agent can grant a single panel service UID access while its
	// parent directory remains root-owned and not writable by that service.
	// Multi-UID sockets retain owner-only mode; use a separately managed group
	// and explicit socket permissions if several service UIDs are necessary.
	if os.Geteuid() == 0 && len(allowedUIDs) == 1 && allowedUIDs[0] != 0 {
		if err := os.Chown(path, int(allowedUIDs[0]), -1); err != nil {
			listener.Close()
			return nil, err
		}
	}
	allowed := make(map[uint32]bool, len(allowedUIDs))
	for _, uid := range allowedUIDs {
		allowed[uid] = true
	}
	return &PeerListener{listener: listener, allowed: allowed}, nil
}

func (l *PeerListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.listener.AcceptUnix()
		if err != nil {
			return nil, err
		}
		credentials, err := UnixPeerCredentials(conn)
		if err != nil || !l.allowed[credentials.UID] {
			conn.Close()
			continue
		}
		return &PeerConn{UnixConn: conn, Credentials: credentials}, nil
	}
}

func (l *PeerListener) Close() error   { return l.listener.Close() }
func (l *PeerListener) Addr() net.Addr { return l.listener.Addr() }

type Executor interface {
	Validate(Request) error
	Run(context.Context, Request) (Result, error)
}

type UnixServer struct {
	listener *PeerListener
	server   *http.Server
	once     sync.Once
}
type wireRequest struct {
	ActorID   string          `json:"actor_id,omitempty"`
	JobID     string          `json:"job_id,omitempty"`
	Operation string          `json:"operation"`
	Input     json.RawMessage `json:"input"`
}
type wireResponse struct {
	Result Result `json:"result"`
	Error  string `json:"error,omitempty"`
	Code   string `json:"code,omitempty"`
}

// RemoteError preserves a module's safe typed error without importing modules.
type RemoteError struct {
	Code    string
	Message string
	Status  int
	cause   error
}

func (e *RemoteError) Error() string { return e.Message }
func (e *RemoteError) Unwrap() error { return e.cause }

func NewUnixServer(path string, allowedUIDs []uint32, executor Executor) (*UnixServer, error) {
	return NewUnixServerWithOptions(UnixOptions{Path: path, AllowedUIDs: allowedUIDs}, executor)
}
func NewUnixServerWithOptions(options UnixOptions, executor Executor) (*UnixServer, error) {
	if executor == nil {
		return nil, errors.New("executor required")
	}
	listener, err := ListenUnixWithOptions(options)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/pipe", pipeHandler(executor))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"ok"}`)
	})
	for _, action := range []string{"run", "validate"} {
		mux.HandleFunc("POST /v1/"+action, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
			decoder.DisallowUnknownFields()
			var input wireRequest
			if err := decoder.Decode(&input); err != nil {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(wireResponse{Error: "invalid request", Code: "invalid_input"})
				return
			}
			var extra any
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(wireResponse{Error: "invalid request", Code: "invalid_input"})
				return
			}
			request := Request{ActorID: input.ActorID, JobID: input.JobID, Operation: input.Operation, Input: input.Input}
			var result Result
			err := executor.Validate(request)
			if err == nil && action == "run" {
				result, err = executor.Run(r.Context(), request)
			}
			response := wireResponse{Result: result}
			if err != nil {
				response.Error, response.Code = err.Error(), errorCode(err)
				var typed interface{ AgentError() (string, int) }
				if errors.As(err, &typed) {
					code, status := typed.AgentError()
					response.Code = code
					if status < 400 || status > 599 {
						status = 500
					}
					w.WriteHeader(status)
				} else if errors.Is(err, ErrUnauthorized) {
					w.WriteHeader(403)
				} else if errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrUnsafePath) || errors.Is(err, ErrReadOnly) || errors.Is(err, ErrFileTooLarge) {
					w.WriteHeader(400)
				} else {
					w.WriteHeader(500)
				}
			}
			json.NewEncoder(w).Encode(response)
		})
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	return &UnixServer{listener: listener, server: server}, nil
}

func (s *UnixServer) Serve(ctx context.Context) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-done:
		}
	}()
	err := s.server.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (s *UnixServer) Close() error {
	var err error
	s.once.Do(func() { err = s.server.Close(); s.listener.Close() })
	return err
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, ErrInvalidInput):
		return "invalid_input"
	case errors.Is(err, ErrUnsafePath):
		return "unsafe_path"
	case errors.Is(err, ErrReadOnly):
		return "read_only"
	case errors.Is(err, ErrFileTooLarge):
		return "file_too_large"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return "operation_failed"
	}
}

func codeError(code, message string) error {
	var kind error
	switch code {
	case "unauthorized":
		kind = ErrUnauthorized
	case "invalid_input":
		kind = ErrInvalidInput
	case "unsafe_path":
		kind = ErrUnsafePath
	case "read_only":
		kind = ErrReadOnly
	case "file_too_large":
		kind = ErrFileTooLarge
	case "canceled":
		kind = context.Canceled
	case "deadline_exceeded":
		kind = context.DeadlineExceeded
	default:
		return errors.New(message)
	}
	return fmt.Errorf("%w: %s", kind, message)
}

type UnixClient struct {
	client    *http.Client
	transport *http.Transport
}

// NewUnixClient verifies the server's kernel UID as well, preventing an
// untrusted process from impersonating a privileged Agent socket.
func NewUnixClient(path string, serverUID uint32) (*UnixClient, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("socket path must be absolute")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
		if err != nil {
			return nil, err
		}
		unix, ok := conn.(*net.UnixConn)
		if !ok {
			conn.Close()
			return nil, errors.New("agent connection is not Unix")
		}
		peer, err := UnixPeerCredentials(unix)
		if err != nil || peer.UID != serverUID {
			conn.Close()
			return nil, errors.New("agent server UID mismatch")
		}
		return conn, nil
	}, ResponseHeaderTimeout: 10 * time.Minute}
	return &UnixClient{client: &http.Client{Transport: transport}, transport: transport}, nil
}

func (c *UnixClient) Close() error { c.transport.CloseIdleConnections(); return nil }
func (c *UnixClient) Validate(request Request) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.call(ctx, "validate", request)
	return err
}
func (c *UnixClient) Run(ctx context.Context, request Request) (Result, error) {
	result, err := c.call(ctx, "run", request)
	if request.Output != nil {
		for _, out := range []Output{{Stream: "stdout", Text: result.Stdout}, {Stream: "stderr", Text: result.Stderr}} {
			if out.Text != "" {
				select {
				case request.Output <- out:
				default:
					result.DroppedEvents++
				}
			}
		}
	}
	return result, err
}
func (c *UnixClient) call(ctx context.Context, action string, request Request) (Result, error) {
	body, err := json.Marshal(wireRequest{ActorID: request.ActorID, JobID: request.JobID, Operation: request.Operation, Input: request.Input})
	if err != nil {
		return Result{}, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent/v1/"+action, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	r.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(r)
	if err != nil {
		return Result{}, err
	}
	defer response.Body.Close()
	var wire wireResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 32<<20)).Decode(&wire); err != nil {
		return Result{}, err
	}
	if wire.Error != "" {
		return wire.Result, &RemoteError{Code: wire.Code, Message: wire.Error, Status: response.StatusCode, cause: codeError(wire.Code, wire.Error)}
	}
	if response.StatusCode != http.StatusOK {
		return wire.Result, fmt.Errorf("agent returned HTTP %d", response.StatusCode)
	}
	return wire.Result, nil
}
