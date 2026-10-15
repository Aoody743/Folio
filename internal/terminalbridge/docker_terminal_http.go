package terminalbridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/coder/websocket"
)

type dockerTerminalWSFrame struct {
	Type    string `json:"type"`
	InputID string `json:"input_id,omitempty"`
	Data    string `json:"data,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Cols    int    `json:"cols,omitempty"`
}

func (p *Provider) dispatchDockerTerminal(ctx context.Context, actor core.Actor, action string, input any) (any, error) {
	data := encoded(input)
	return p.deps.Dispatch(ctx, actor, action, data)
}

func (p *Provider) serveDockerTerminalWS(w http.ResponseWriter, r *http.Request, sessionID string) {
	if !dockerTerminalSessionIDPattern.MatchString(sessionID) {
		reply(w, nil, invalid())
		return
	}
	actor, err := p.dockerTerminalStreamActor(r, sessionID)
	if err != nil {
		reply(w, nil, err)
		return
	}
	if !p.docker.beginWebsocket() {
		reply(w, nil, failure("unavailable", "Docker terminal provider is shutting down", 503))
		return
	}
	defer p.docker.websocketWG.Done()
	runtime, err := p.runtimeFor(r.Context(), actor.ID, sessionID)
	if err != nil {
		reply(w, nil, err)
		return
	}
	select {
	case p.streams <- struct{}{}:
		defer func() { <-p.streams }()
	default:
		reply(w, nil, failure("busy", "Terminal browser stream budget is exhausted", 429))
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
	stopShutdown := context.AfterFunc(p.docker.ctx, cancel)
	defer stopShutdown()
	var writeMu sync.Mutex
	write := func(value any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return writeWS(ctx, connection, value)
	}
	fail := func(err error) {
		safe := bridgeError(err)
		_ = write(map[string]any{"type": "error", "error": safe})
		_ = connection.Close(websocket.StatusPolicyViolation, core.ErrorCode(safe))
		cancel()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var after uint64
		lastState := ""
		changed := runtime.changed()
		readLatest := func() bool {
			currentActor, e := p.dockerTerminalStreamActor(r, sessionID)
			if e != nil {
				fail(e)
				return false
			}
			result, e := p.dispatchDockerTerminal(ctx, currentActor, "docker_terminal_read", dockerTerminalReadInput{SessionID: sessionID, After: after, Limit: 8})
			if e != nil {
				fail(e)
				return false
			}
			var read dockerTerminalReadOutput
			if json.Unmarshal(encoded(result), &read) != nil || read.SessionID != sessionID {
				fail(failure("agent_response", "Invalid Docker terminal stream output", 502))
				return false
			}
			if len(read.Frames) > 0 || read.Truncated || read.OutputUnavailable || read.ExecutionState != lastState {
				if e = write(map[string]any{"type": "output", "data": read}); e != nil {
					cancel()
					return false
				}
			}
			after = read.Next
			lastState = read.ExecutionState
			if read.ExecutionState == "exited" || read.CloseRequested {
				_ = connection.Close(websocket.StatusNormalClosure, "Docker terminal detached")
				cancel()
				return false
			}
			return true
		}
		if !readLatest() {
			return
		}
		for {
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				if _, e := p.dockerTerminalStreamActor(r, sessionID); e != nil {
					fail(e)
					return
				}
				continue
			case <-changed:
				timer.Stop()
				coalesce := time.NewTimer(30 * time.Millisecond)
				select {
				case <-ctx.Done():
					coalesce.Stop()
					return
				case <-coalesce.C:
				}
				changed = runtime.changed()
				if !readLatest() {
					return
				}
			}
		}
	}()
	for {
		kind, data, readErr := connection.Read(ctx)
		if readErr != nil {
			cancel()
			break
		}
		if kind != websocket.MessageText || len(data) == 0 || len(data) > 24<<10 {
			fail(invalid())
			break
		}
		var input dockerTerminalWSFrame
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil {
			fail(invalid())
			break
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			fail(invalid())
			break
		}
		if (input.Type != "input" && input.Type != "resize" && input.Type != "close") || (input.Type != "input" && (input.InputID != "" || input.Data != "")) || (input.Type != "resize" && (input.Rows != 0 || input.Cols != 0)) {
			fail(invalid())
			break
		}
		actor, e := p.dockerTerminalStreamActor(r, sessionID)
		if e != nil {
			fail(e)
			break
		}
		var action string
		var payload any
		switch input.Type {
		case "input":
			if !dockerTerminalInputIDPattern.MatchString(input.InputID) || len(input.Data) > 21848 {
				fail(invalid())
				break
			}
			action = "docker_terminal_input"
			payload = dockerTerminalInputInput{SessionID: sessionID, InputID: input.InputID, Data: input.Data}
		case "resize":
			if input.Rows < 1 || input.Rows > 500 || input.Cols < 1 || input.Cols > 1000 {
				fail(invalid())
				break
			}
			action = "docker_terminal_resize"
			payload = dockerTerminalResizeInput{SessionID: sessionID, Rows: input.Rows, Cols: input.Cols}
		case "close":
			action = "docker_terminal_close"
			payload = dockerTerminalSessionInput{SessionID: sessionID}
		}
		if action == "" {
			break
		}
		result, e := p.dispatchDockerTerminal(ctx, actor, action, payload)
		if e != nil {
			fail(e)
			break
		}
		if e = write(map[string]any{"type": "ack", "operation": input.Type, "input_id": input.InputID, "data": result}); e != nil {
			cancel()
			break
		}
		if input.Type == "close" {
			_ = connection.Close(websocket.StatusNormalClosure, "Docker terminal detached")
			cancel()
			break
		}
	}
	<-done
}
