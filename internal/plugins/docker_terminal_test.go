//go:build linux

package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type dockerTerminalEngineFixture struct {
	mu         sync.Mutex
	instance   Instance
	foreign    bool
	create     map[string]any
	start      map[string]any
	stdin      string
	resizePath string
	started    bool
	running    bool
}

func newDockerTerminalEngineFixture(t *testing.T, instance Instance, foreign bool, running bool) *dockerTerminalEngineFixture {
	t.Helper()
	if err := os.MkdirAll(instance.Paths.RunDir, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(instance.Paths.RunDir, "docker.sock"))
	if err != nil {
		t.Fatal("could not create isolated Docker Engine API socket", err)
	}
	fixture := &dockerTerminalEngineFixture{instance: instance, foreign: foreign, running: running}
	execID := strings.Repeat("b", 64)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			labels := map[string]string{"io.powerlinux.managed": "true", "io.powerlinux.instance": instance.ID}
			if foreign {
				labels["io.powerlinux.instance"] = "foreign-instance"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Labels": labels}, "State": map[string]any{"Running": true}})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/exec"):
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("Docker terminal create did not send JSON")
			}
			fixture.mu.Lock()
			fixture.create = body
			fixture.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": execID})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("Docker terminal start did not send JSON")
			}
			fixture.mu.Lock()
			fixture.start, fixture.started = body, true
			fixture.mu.Unlock()
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("Docker Engine fixture cannot hijack terminal connection")
				return
			}
			conn, rw, hijackErr := hijacker.Hijack()
			if hijackErr != nil {
				t.Error("could not hijack Docker terminal connection", hijackErr)
				return
			}
			_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			if rw.Flush() != nil {
				_ = conn.Close()
				return
			}
			input, readErr := io.ReadAll(rw.Reader)
			if readErr != nil {
				t.Error("could not read Docker terminal stdin", readErr)
			}
			fixture.mu.Lock()
			fixture.stdin = string(input)
			fixture.mu.Unlock()
			_, _ = conn.Write([]byte("tty-output\r\n"))
			_ = conn.Close()
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/exec/") && strings.HasSuffix(r.URL.Path, "/json"):
			fixture.mu.Lock()
			started, isRunning := fixture.started, fixture.running
			fixture.mu.Unlock()
			status := map[string]any{"Running": started && isRunning}
			if started && !isRunning {
				status["ExitCode"] = 0
			}
			_ = json.NewEncoder(w).Encode(status)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/exec/") && strings.Contains(r.URL.Path, "/resize"):
			fixture.mu.Lock()
			fixture.resizePath = r.URL.RequestURI()
			fixture.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-done
	})
	return fixture
}

type dockerTerminalTestWriter struct{ bytes.Buffer }

func (w *dockerTerminalTestWriter) Close() error { return nil }

func TestDockerTerminalUsesOwnedEngineTTYStreamResizeAndTruthfulClose(t *testing.T) {
	executor, instance := dockerImageFixtureAgent(t)
	engine := newDockerTerminalEngineFixture(t, instance, false, true)
	terminal := NewDockerTerminalAgentExecutor(executor)
	ctx := context.Background()
	request := agent.Request{ActorID: "actor-terminal-test", Operation: "docker_terminal_open", Input: json.RawMessage(`{"id":"docker-image-fixture","name":"web","request_id":"terminal-open-01","rows":31,"cols":99}`)}
	opened, err := terminal.Run(ctx, request)
	if err != nil {
		t.Fatal("managed terminal open failed:", err)
	}
	var session dockerTerminalPublic
	if json.Unmarshal([]byte(opened.Stdout), &session) != nil || session.SessionID == "" || session.ExecutionState != "created" || session.Rows != 31 || session.Cols != 99 {
		t.Fatalf("unexpected terminal open response: %#v", opened)
	}

	output := &dockerTerminalTestWriter{}
	streamRequest := agent.Request{ActorID: request.ActorID, Operation: "docker_terminal_stream", Input: mustJSON(dockerTerminalSessionInput{SessionID: session.SessionID})}
	streamResult, err := terminal.Stream(ctx, streamRequest, agent.IO{Stdin: io.NopCloser(strings.NewReader("typed input\n")), Stdout: output})
	if err != nil {
		t.Fatal("managed TTY stream failed:", err)
	}
	if output.String() != "tty-output\r\n" || streamResult.Operation != "docker_terminal_stream" {
		t.Fatalf("TTY stream did not preserve raw output: %q %#v", output.String(), streamResult)
	}

	resize, err := terminal.Run(ctx, agent.Request{ActorID: request.ActorID, Operation: "docker_terminal_resize", Input: mustJSON(dockerTerminalResizeInput{SessionID: session.SessionID, Rows: 45, Cols: 120})})
	if err != nil {
		t.Fatal("managed TTY resize failed:", err)
	}
	var resized dockerTerminalPublic
	if json.Unmarshal([]byte(resize.Stdout), &resized) != nil || resized.Rows != 45 || resized.Cols != 120 {
		t.Fatalf("terminal resize did not persist dimensions: %#v", resize)
	}

	closed, err := terminal.Run(ctx, agent.Request{ActorID: request.ActorID, Operation: "docker_terminal_close", Input: mustJSON(dockerTerminalSessionInput{SessionID: session.SessionID})})
	if err != nil {
		t.Fatal("managed TTY close inspection failed:", err)
	}
	var final dockerTerminalPublic
	if json.Unmarshal([]byte(closed.Stdout), &final) != nil || !final.CloseRequested || final.ExecutionState != "running" || final.Attached {
		t.Fatalf("close falsely claimed that Docker stopped the shell: %#v", closed)
	}
	if _, err = terminal.Run(ctx, agent.Request{ActorID: request.ActorID, Operation: "docker_terminal_resize", Input: mustJSON(dockerTerminalResizeInput{SessionID: session.SessionID, Rows: 50, Cols: 140})}); core.ErrorCode(err) != "session_closed" {
		t.Fatalf("resize changed a closed terminal: %v", err)
	}

	engine.mu.Lock()
	create, start, stdin, resizePath, started := engine.create, engine.start, engine.stdin, engine.resizePath, engine.started
	engine.mu.Unlock()
	if create["Tty"] != true || create["AttachStdin"] != true || create["AttachStdout"] != true || create["AttachStderr"] != true || create["Cmd"].([]any)[0] != "/bin/sh" || create["Env"].([]any)[0] != "TERM=xterm-256color" {
		t.Fatalf("terminal create escaped the fixed shell/TTY profile: %#v", create)
	}
	if start["Tty"] != true || start["Detach"] != false || !started || stdin != "typed input\n" {
		t.Fatalf("TTY attach lifecycle did not preserve interactive input: start=%#v stdin=%q", start, stdin)
	}
	if !strings.Contains(resizePath, "h=45") || !strings.Contains(resizePath, "w=120") {
		t.Fatalf("Docker resize dimensions were not applied: %q", resizePath)
	}
}

func TestDockerTerminalRefusesForeignContainerBeforeCreatingExec(t *testing.T) {
	executor, instance := dockerImageFixtureAgent(t)
	engine := newDockerTerminalEngineFixture(t, instance, true, true)
	terminal := NewDockerTerminalAgentExecutor(executor)
	request := agent.Request{ActorID: "actor-terminal-test", Operation: "docker_terminal_open", Input: json.RawMessage(`{"id":"docker-image-fixture","name":"web","request_id":"terminal-open-02"}`)}
	if _, err := terminal.Run(context.Background(), request); core.ErrorCode(err) != "ownership_invalid" {
		t.Fatalf("foreign container was accepted: %v", err)
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.create != nil || engine.started {
		t.Fatal("foreign container reached Docker exec create or start")
	}
}

func TestDockerTerminalAgentBoundsPrivateSessionHistoryAndPrunesExpiredReceipts(t *testing.T) {
	executor, _ := dockerImageFixtureAgent(t)
	terminal := NewDockerTerminalAgentExecutor(executor)
	now := time.Now().UTC()
	for i := 0; i < dockerTerminalMaxRecords; i++ {
		actorID := fmt.Sprintf("actor-%02d", i/(dockerTerminalMaxRecords/4))
		requestID := fmt.Sprintf("terminal-cap-%04d", i)
		sessionID := fmt.Sprintf("dct_%032x", i+1)
		record := dockerTerminalRecord{ActorID: actorID, ID: "docker-image-fixture", Name: "web", SessionID: sessionID, RequestID: requestID, State: "unknown", Rows: 24, Cols: 80, CreatedAt: now, ExpiresAt: now.Add(dockerTerminalMaxAge)}
		request := dockerTerminalRequestRecord{ActorID: actorID, RequestID: requestID, ID: record.ID, Name: record.Name, SessionID: sessionID, Rows: record.Rows, Cols: record.Cols, CreatedAt: now, ExpiresAt: record.ExpiresAt}
		if err := executor.save("private", sessionID, record); err != nil {
			t.Fatal(err)
		}
		if err := executor.save("private", dockerTerminalRequestKey(actorID, requestID), request); err != nil {
			t.Fatal(err)
		}
	}
	total, byActor, err := terminal.pruneRecords(now)
	if err != nil || total != dockerTerminalMaxRecords || byActor["actor-00"] != dockerTerminalMaxRecords/4 {
		t.Fatalf("Agent session/request history escaped its caps: total=%d actors=%v err=%v", total, byActor, err)
	}

	expiredID := "dct_ffffffffffffffffffffffffffffffff"
	expiredRequestID := "terminal-expired-1"
	expiredAt := now.Add(-dockerTerminalRetention - time.Second)
	expired := dockerTerminalRecord{ActorID: "actor-expired", ID: "docker-image-fixture", Name: "web", SessionID: expiredID, RequestID: expiredRequestID, State: "exited", Rows: 24, Cols: 80, CreatedAt: expiredAt.Add(-dockerTerminalMaxAge), ExpiresAt: expiredAt}
	request := dockerTerminalRequestRecord{ActorID: expired.ActorID, RequestID: expiredRequestID, ID: expired.ID, Name: expired.Name, SessionID: expiredID, Rows: 24, Cols: 80, CreatedAt: expired.CreatedAt, ExpiresAt: expired.ExpiresAt}
	if err = executor.save("private", expiredID, expired); err != nil {
		t.Fatal(err)
	}
	if err = executor.save("private", dockerTerminalRequestKey(expired.ActorID, expiredRequestID), request); err != nil {
		t.Fatal(err)
	}
	total, byActor, err = terminal.pruneRecords(now)
	if err != nil || total != dockerTerminalMaxRecords || byActor["actor-expired"] != 0 {
		t.Fatalf("expired private receipts were not pruned: total=%d actors=%v err=%v", total, byActor, err)
	}
	if _, err = terminal.open(context.Background(), "overflow-actor", dockerTerminalOpenInput{ID: "docker-image-fixture", Name: "web", RequestID: "terminal-overflow-1"}); core.ErrorCode(err) != "session_limit" {
		t.Fatalf("Agent accepted new shells past the private history cap: %v", err)
	}
}

func mustJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}
