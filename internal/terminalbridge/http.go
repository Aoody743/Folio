package terminalbridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/httpapi"
	"github.com/Aoody743/PowerLinux/internal/terminal"
	"github.com/coder/websocket"
)

func reply(w http.ResponseWriter, data any, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		err = bridgeError(err)
		w.WriteHeader(core.ErrorStatus(err))
		var safe *core.Error
		safe, _ = err.(*core.Error)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": safe})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}
func (p *Provider) dispatch(ctx context.Context, a core.Actor, name string, in terminal.Input, key string) (any, error) {
	if key != "" && p.options.DispatchWithKey != nil {
		return p.options.DispatchWithKey(ctx, a, name, encoded(in), key)
	}
	if key != "" && p.options.DispatchWithKey == nil {
		return nil, failure("unavailable", "Idempotent terminal dispatch has not been configured", 503)
	}
	return p.deps.Dispatch(ctx, a, name, encoded(in))
}

// HandleResource must be installed only as httpapi.Options.ResourceHandler.
// The private revalidation context proves authentication/Origin/CSRF passed;
// accepting a caller-supplied Actor alone is deliberately insufficient.
func (p *Provider) HandleResource(w http.ResponseWriter, r *http.Request, _ core.Actor) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/v1/terminal/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/terminal/"), "/")
	if len(parts) != 3 || parts[0] != "sessions" || parts[1] == "" {
		reply(w, nil, failure("not_found", "Terminal resource endpoint not found", 404))
		return true
	}
	a, err := httpapi.RevalidateActor(r)
	if err != nil {
		reply(w, nil, err)
		return true
	}
	id, operation := parts[1], parts[2]
	if operation == "ws" {
		if r.Method != http.MethodGet {
			reply(w, nil, failure("method_not_allowed", "WebSocket requires GET", 405))
			return true
		}
		p.serveWS(w, r, a, id)
		return true
	}
	in := terminal.Input{ID: id}
	name := "terminal_" + operation
	if operation == "read" {
		if r.Method != http.MethodGet {
			reply(w, nil, failure("method_not_allowed", "Terminal read requires GET", 405))
			return true
		}
		if raw := r.URL.Query().Get("after"); raw != "" {
			in.After, err = strconv.ParseUint(raw, 10, 53)
		}
		if raw := r.URL.Query().Get("limit"); raw != "" && err == nil {
			in.Limit, err = strconv.Atoi(raw)
		}
		if err != nil || in.Limit < 0 || in.Limit > 8 {
			reply(w, nil, invalid())
			return true
		}
		if in.Limit == 0 {
			in.Limit = 8
		}
	} else if operation == "input" || operation == "resize" || operation == "renew" || operation == "close" {
		if r.Method != http.MethodPost {
			reply(w, nil, failure("method_not_allowed", "Terminal write requires POST", 405))
			return true
		}
		var body struct {
			Data string `json:"data,omitempty"`
			Rows int    `json:"rows,omitempty"`
			Cols int    `json:"cols,omitempty"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 24<<10))
		decoder.DisallowUnknownFields()
		if e := decoder.Decode(&body); e != nil && e != io.EOF {
			reply(w, nil, invalid())
			return true
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			reply(w, nil, invalid())
			return true
		}
		in.Data, in.Rows, in.Cols = body.Data, body.Rows, body.Cols
		if operation != "input" && in.Data != "" || operation != "resize" && (in.Rows != 0 || in.Cols != 0) {
			reply(w, nil, invalid())
			return true
		}
	} else {
		reply(w, nil, failure("not_found", "Terminal operation endpoint not found", 404))
		return true
	}
	data, err := p.dispatch(r.Context(), a, name, in, r.Header.Get("Idempotency-Key"))
	reply(w, data, err)
	return true
}

type frame struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Rows int    `json:"rows,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Key  string `json:"idempotency_key,omitempty"`
}

func (p *Provider) streamActor(r *http.Request, id string) (core.Actor, error) {
	a, err := httpapi.RevalidateActor(r)
	if err != nil {
		return a, err
	}
	if _, err = p.resources(r.Context(), a, "terminal_read", encoded(terminal.Input{ID: id})); err != nil {
		return a, err
	}
	return a, nil
}
func writeWS(ctx context.Context, connection *websocket.Conn, value any) error {
	limited, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return connection.Write(limited, websocket.MessageText, encoded(value))
}
func (p *Provider) serveWS(w http.ResponseWriter, r *http.Request, a core.Actor, id string) {
	if _, err := p.streamActor(r, id); err != nil {
		reply(w, nil, err)
		return
	}
	select {
	case p.streams <- struct{}{}:
		defer func() { <-p.streams }()
	default:
		reply(w, nil, failure("busy", "Terminal stream budget is exhausted", 429))
		return
	}
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"powerlinux-terminal-v1"}})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	connection.SetReadLimit(24 << 10)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	fail := func(err error) {
		safe := bridgeError(err)
		_ = writeWS(ctx, connection, map[string]any{"type": "error", "error": safe})
		_ = connection.Close(websocket.StatusPolicyViolation, core.ErrorCode(safe))
		cancel()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		renew := time.NewTicker(5 * time.Second)
		defer renew.Stop()
		var after uint64
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				actor, e := p.streamActor(r, id)
				if e != nil {
					fail(e)
					return
				}
				result, e := p.dispatch(ctx, actor, "terminal_read", terminal.Input{ID: id, After: after, Limit: 8}, "")
				if e != nil {
					fail(e)
					return
				}
				var read terminal.ReadResult
				if json.Unmarshal(encoded(result), &read) != nil {
					fail(failure("agent_response", "Invalid terminal stream output", 502))
					return
				}
				if len(read.Frames) > 0 || read.Truncated || read.Exited {
					if e = writeWS(ctx, connection, map[string]any{"type": "output", "data": read}); e != nil {
						cancel()
						return
					}
				}
				after = read.Next
				if read.Exited {
					_ = connection.Close(websocket.StatusNormalClosure, "terminal exited")
					cancel()
					return
				}
			case <-renew.C:
				actor, e := p.streamActor(r, id)
				if e != nil {
					fail(e)
					return
				}
				result, e := p.dispatch(ctx, actor, "terminal_renew", terminal.Input{ID: id}, "")
				if e != nil {
					fail(e)
					return
				}
				if e = writeWS(ctx, connection, map[string]any{"type": "session", "data": result}); e != nil {
					cancel()
					return
				}
			}
		}
	}()
	for {
		kind, data, e := connection.Read(ctx)
		if e != nil {
			cancel()
			break
		}
		if kind != websocket.MessageText {
			fail(invalid())
			break
		}
		var request frame
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil {
			fail(invalid())
			break
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			fail(invalid())
			break
		}
		if request.Type != "input" && request.Type != "resize" && request.Type != "renew" && request.Type != "close" {
			fail(invalid())
			break
		}
		if request.Type != "input" && request.Data != "" || request.Type != "resize" && (request.Rows != 0 || request.Cols != 0) || len(request.Key) > 128 {
			fail(invalid())
			break
		}
		if request.Type == "input" && len(request.Key) < 8 {
			fail(failure("invalid_input", "Input frames require a unique intent key", 400))
			break
		}
		actor, e := p.streamActor(r, id)
		if e != nil {
			fail(e)
			break
		}
		result, e := p.dispatch(ctx, actor, "terminal_"+request.Type, terminal.Input{ID: id, Data: request.Data, Rows: request.Rows, Cols: request.Cols}, request.Key)
		if e != nil {
			fail(e)
			break
		}
		if e = writeWS(ctx, connection, map[string]any{"type": "ack", "operation": request.Type, "data": result, "idempotency_key": request.Key}); e != nil {
			cancel()
			break
		}
		if request.Type == "close" {
			_ = connection.Close(websocket.StatusNormalClosure, "terminal closed")
			cancel()
			break
		}
	}
	<-done
}
