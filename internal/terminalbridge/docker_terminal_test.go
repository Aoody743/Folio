package terminalbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type dockerTerminalAgentFixture struct {
	mu      sync.Mutex
	status  dockerTerminalAgentStatus
	started chan struct{}
	done    chan struct{}
	once    sync.Once
}

func newDockerTerminalAgentFixture() *dockerTerminalAgentFixture {
	return &dockerTerminalAgentFixture{started: make(chan struct{}), done: make(chan struct{})}
}

func (f *dockerTerminalAgentFixture) Validate(agent.Request) error { return nil }

func (f *dockerTerminalAgentFixture) Run(_ context.Context, request agent.Request) (agent.Result, error) {
	f.mu.Lock()
	switch request.Operation {
	case "docker_terminal_open":
		var input dockerTerminalOpenInput
		if json.Unmarshal(request.Input, &input) != nil {
			f.mu.Unlock()
			return agent.Result{}, invalid()
		}
		rows, cols := input.Rows, input.Cols
		if rows == 0 {
			rows = 24
		}
		if cols == 0 {
			cols = 80
		}
		f.status = dockerTerminalAgentStatus{ID: input.ID, Name: input.Name, SessionID: "dct_0123456789abcdef0123456789abcdef", TargetID: "docker:" + input.ID + ":" + input.Name, Type: "docker", Rows: rows, Cols: cols, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(dockerTerminalLifetime), ExecutionState: "created"}
	case "docker_terminal_status", "docker_terminal_info":
	case "docker_terminal_resize":
		var input dockerTerminalResizeInput
		if json.Unmarshal(request.Input, &input) != nil {
			f.mu.Unlock()
			return agent.Result{}, invalid()
		}
		if f.status.CloseRequested {
			f.mu.Unlock()
			return agent.Result{}, failure("session_closed", "closed", 410)
		}
		f.status.Rows, f.status.Cols = input.Rows, input.Cols
	case "docker_terminal_close":
		f.status.CloseRequested = true
		f.status.Attached = false
	default:
		f.mu.Unlock()
		return agent.Result{}, failure("forbidden", "unexpected fixture operation", 403)
	}
	status := f.status
	f.mu.Unlock()
	return agent.Result{Operation: request.Operation, Data: encoded(status)}, nil
}

func (f *dockerTerminalAgentFixture) Stream(ctx context.Context, request agent.Request, streams agent.IO) (agent.Result, error) {
	if request.Operation != "docker_terminal_stream" {
		return agent.Result{}, failure("forbidden", "unexpected fixture stream", 403)
	}
	f.mu.Lock()
	f.status.ExecutionState = "running"
	f.status.Attached = true
	f.status.StreamStarted = true
	f.mu.Unlock()
	f.once.Do(func() { close(f.started) })
	if streams.Stdout != nil {
		_, _ = io.WriteString(streams.Stdout, "fixture terminal output\r\n")
	}
	<-ctx.Done()
	f.mu.Lock()
	f.status.Attached = false
	f.mu.Unlock()
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	return agent.Result{Operation: request.Operation, ExitCode: -1}, ctx.Err()
}

func dockerTerminalActor(t *testing.T, f fixture) (core.Actor, core.TokenCreated) {
	t.Helper()
	resources := dockerTerminalResources("docker-test", "web")
	created, err := f.service.CreateToken(context.Background(), f.admin, core.TokenRequest{Name: "Docker terminal fixture", Kind: "mcp", Scopes: []string{"containers:manage", "containers:read"}, Resources: resources, ExpiresInSeconds: 300})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := f.service.AuthenticateToken(context.Background(), created.Token)
	if err != nil {
		t.Fatal(err)
	}
	return actor, created
}

func openDockerTerminalFixture(t *testing.T, f fixture, executor *dockerTerminalAgentFixture, actor core.Actor) dockerTerminalOutput {
	t.Helper()
	f.bridge.options.Agent = executor
	result, err := f.service.Execute(context.Background(), actor, "docker_terminal_open", encoded(dockerTerminalOpenInput{ID: "docker-test", Name: "web", RequestID: "terminal-open-01", Rows: 24, Cols: 80}))
	if err != nil {
		t.Fatal("Docker terminal fixture open failed:", err)
	}
	var output dockerTerminalOutput
	if json.Unmarshal(encoded(result), &output) != nil || output.SessionID == "" {
		t.Fatalf("invalid Docker terminal open response: %#v", result)
	}
	return output
}

func assertDockerTerminalOpenMetadata(t *testing.T, value any) {
	t.Helper()
	var fields map[string]json.RawMessage
	if json.Unmarshal(encoded(value), &fields) != nil {
		t.Fatalf("invalid terminal response: %#v", value)
	}
	for _, key := range []string{"id", "name", "session_id", "target_id", "type", "rows", "cols", "created_at", "expires_at", "execution_state", "attached", "stream_started", "output_available"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("terminal response omitted common open metadata field %q", key)
		}
	}
}

func TestDockerTerminalPublicActionsRetainOpenMetadataAndCloseWinsResize(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	actor, _ := dockerTerminalActor(t, f)
	agentFixture := newDockerTerminalAgentFixture()
	opened := openDockerTerminalFixture(t, f, agentFixture, actor)
	select {
	case <-agentFixture.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Agent stream did not start")
	}
	assertDockerTerminalOpenMetadata(t, opened)

	status, err := f.service.Execute(ctx, actor, "docker_terminal_status", encoded(dockerTerminalSessionInput{SessionID: opened.SessionID}))
	if err != nil {
		t.Fatal(err)
	}
	assertDockerTerminalOpenMetadata(t, status)

	resized, err := f.service.Execute(ctx, actor, "docker_terminal_resize", encoded(dockerTerminalResizeInput{SessionID: opened.SessionID, Rows: 31, Cols: 99}))
	if err != nil {
		t.Fatal(err)
	}
	assertDockerTerminalOpenMetadata(t, resized)

	read, err := f.service.Execute(ctx, actor, "docker_terminal_read", encoded(dockerTerminalReadInput{SessionID: opened.SessionID, Limit: 8}))
	if err != nil {
		t.Fatal(err)
	}
	assertDockerTerminalOpenMetadata(t, read)
	var readResult dockerTerminalReadOutput
	if json.Unmarshal(encoded(read), &readResult) != nil || len(readResult.Frames) == 0 {
		t.Fatal("read did not return streamed terminal output")
	}

	runtime, err := f.bridge.runtimeFor(ctx, actor.ID, opened.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// Hold the shared control gate so resize and close overlap. Close persists
	// its monotonic marker before either operation can issue Engine control I/O.
	runtime.controlGate <- struct{}{}
	resizeDone := make(chan error, 1)
	go func() {
		_, resizeErr := f.bridge.resizeDockerTerminal(ctx, actor, dockerTerminalResizeInput{SessionID: opened.SessionID, Rows: 45, Cols: 120})
		resizeDone <- resizeErr
	}()
	closeDone := make(chan error, 1)
	go func() {
		_, closeErr := f.bridge.closeDockerTerminal(ctx, actor, opened.SessionID)
		closeDone <- closeErr
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		runtime.mu.Lock()
		closing := runtime.record.CloseRequested
		runtime.mu.Unlock()
		if closing {
			break
		}
		time.Sleep(time.Millisecond)
	}
	runtime.mu.Lock()
	closing := runtime.record.CloseRequested
	runtime.mu.Unlock()
	if !closing {
		<-runtime.controlGate
		t.Fatal("close did not persist its state before waiting for resize")
	}
	<-runtime.controlGate
	if resizeErr := <-resizeDone; core.ErrorCode(resizeErr) != "session_expired" {
		t.Fatalf("late resize was not rejected after close intent: %v", resizeErr)
	}
	if closeErr := <-closeDone; closeErr != nil {
		t.Fatal("close failed:", closeErr)
	}
	var final dockerTerminalOutput
	if err = json.Unmarshal(encoded(func() any {
		value, e := f.service.Execute(ctx, actor, "docker_terminal_status", encoded(dockerTerminalSessionInput{SessionID: opened.SessionID}))
		if e != nil {
			t.Fatal(e)
		}
		return value
	}()), &final); err != nil {
		t.Fatal(err)
	}
	assertDockerTerminalOpenMetadata(t, final)
	if !final.CloseRequested || final.ExecutionState != "running" || final.Rows != 31 || final.Cols != 99 {
		t.Fatalf("close or earlier resize state was overwritten: %#v", final)
	}
}

func TestDockerTerminalActorGrantRevocationDetachesWithoutClaimingExecWasKilled(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	actor, token := dockerTerminalActor(t, f)
	agentFixture := newDockerTerminalAgentFixture()
	opened := openDockerTerminalFixture(t, f, agentFixture, actor)
	select {
	case <-agentFixture.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Agent stream did not start")
	}
	if err := f.service.RevokeToken(ctx, f.admin, token.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-agentFixture.done:
	case <-time.After(7 * time.Second):
		t.Fatal("revoked actor's Agent stream did not detach")
	}
	runtime, err := f.bridge.runtimeFor(ctx, actor.ID, opened.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		runtime.mu.Lock()
		active := runtime.streamActive
		runtime.mu.Unlock()
		if !active {
			break
		}
		time.Sleep(time.Millisecond)
	}
	runtime.mu.Lock()
	active, state := runtime.streamActive, runtime.record.ExecutionState
	runtime.mu.Unlock()
	if active || state != "running" {
		t.Fatalf("grant revocation misreported the actual Engine exec state: attached=%v state=%q", active, state)
	}
	if _, err := f.service.Execute(ctx, actor, "docker_terminal_read", encoded(dockerTerminalReadInput{SessionID: opened.SessionID})); core.ErrorCode(err) != "unauthorized" {
		t.Fatalf("revoked actor retained Docker terminal read access: %v", err)
	}
}

func TestDockerTerminalRuntimeAndDurableHistoryLimitsAreBounded(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	now := time.Now().UTC()
	makeRecord := func(actorID string, n int) dockerTerminalStored {
		sessionID := fmt.Sprintf("dct_%032x", n+1)
		requestID := fmt.Sprintf("history-%04d", n)
		requestKey := dockerTerminalRequestKey(actorID, requestID)
		return dockerTerminalStored{ActorID: actorID, ID: "docker-test", Name: "web", RequestKey: requestKey, SessionID: sessionID, Rows: 24, Cols: 80, CreatedAt: now, ExpiresAt: now.Add(dockerTerminalLifetime), ExecutionState: "running"}
	}

	// The durable receipt cap is lower than the native Agent cap and preserves
	// session/request pairs for the complete lease plus retry retention.
	for i := 0; i < dockerTerminalRecordLimit; i++ {
		actorID := fmt.Sprintf("actor-%02d", i/dockerTerminalActorLimit)
		record := makeRecord(actorID, i)
		requestID := fmt.Sprintf("history-%04d", i)
		requestKey := dockerTerminalRequestKey(actorID, requestID)
		if err := f.bridge.deps.State.Save(ctx, dockerTerminalSessionsKind, record.SessionID, encoded(record)); err != nil {
			t.Fatal(err)
		}
		request := dockerTerminalRequestRecord{ActorID: actorID, ID: record.ID, Name: record.Name, Rows: record.Rows, Cols: record.Cols, SessionID: record.SessionID, CreatedAt: record.CreatedAt, ExpiresAt: record.ExpiresAt}
		if err := f.bridge.deps.State.Save(ctx, dockerTerminalRequestsKind, requestKey, encoded(request)); err != nil {
			t.Fatal(err)
		}
	}
	byActor, total, err := f.bridge.pruneDockerTerminalRecords(ctx)
	if err != nil || total != dockerTerminalRecordLimit || byActor["actor-00"] != dockerTerminalActorLimit {
		t.Fatalf("durable session history escaped its global/per-actor cap: total=%d actors=%v err=%v", total, byActor, err)
	}
	if _, err = f.bridge.openDockerTerminal(ctx, core.Actor{ID: "actor-00"}, dockerTerminalOpenInput{ID: "docker-test", Name: "web", RequestID: "history-overflow-0001"}); core.ErrorCode(err) != "session_limit" {
		t.Fatalf("open exceeded the durable history cap: %v", err)
	}

	// The in-memory output cache has a smaller independent cap. Eviction drops
	// frames and reports them unavailable, while a full active cache fails closed.
	p := f.bridge
	p.docker.mu.Lock()
	for i := 0; i < dockerTerminalRuntimeLimit; i++ {
		record := makeRecord("cache-actor", 1000+i)
		runtime := newDockerTerminalRuntime(record)
		runtime.frames = []dockerTerminalFrame{{Sequence: 1, Data: "YQ=="}}
		runtime.frameBytes = len(runtime.frames[0].Data)
		runtime.touchedAt = now.Add(time.Duration(i) * time.Millisecond)
		p.docker.runtimes[record.SessionID] = runtime
	}
	p.docker.mu.Unlock()
	newRecord := makeRecord("cache-actor", 2000)
	if err = p.deps.State.Save(ctx, dockerTerminalSessionsKind, newRecord.SessionID, encoded(newRecord)); err != nil {
		t.Fatal(err)
	}
	if _, err = p.runtimeFor(ctx, "cache-actor", newRecord.SessionID); err != nil {
		t.Fatal("inactive cache eviction refused a new session:", err)
	}
	p.docker.mu.Lock()
	if len(p.docker.runtimes) != dockerTerminalRuntimeLimit {
		p.docker.mu.Unlock()
		t.Fatalf("runtime cache exceeded its cap: %d", len(p.docker.runtimes))
	}
	for _, runtime := range p.docker.runtimes {
		runtime.mu.Lock()
		runtime.streamActive = true
		runtime.mu.Unlock()
	}
	p.docker.mu.Unlock()
	blocked := makeRecord("cache-actor", 2001)
	if err = p.deps.State.Save(ctx, dockerTerminalSessionsKind, blocked.SessionID, encoded(blocked)); err != nil {
		t.Fatal(err)
	}
	if _, err = p.runtimeFor(ctx, "cache-actor", blocked.SessionID); core.ErrorCode(err) != "busy" {
		t.Fatalf("all-active runtime cache should reject excess sessions: %v", err)
	}
}
