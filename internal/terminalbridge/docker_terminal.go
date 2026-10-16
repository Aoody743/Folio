package terminalbridge

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/httpapi"
)

const (
	dockerTerminalSessionsKind = "docker_terminal_sessions"
	dockerTerminalRequestsKind = "docker_terminal_requests"
	dockerTerminalLifetime     = 30 * time.Minute
	dockerTerminalRingBytes    = 88 << 10
	dockerTerminalRingFrames   = 256
	dockerTerminalInputBytes   = 16 << 10
	dockerTerminalInputIDs     = 512
	dockerTerminalReadFrames   = 32
	dockerTerminalRuntimeLimit = 32
	dockerTerminalRecordLimit  = 64
	dockerTerminalActorLimit   = 16
	dockerTerminalRetention    = 24 * time.Hour
)

var (
	dockerTerminalSessionIDPattern = regexp.MustCompile(`^dct_[a-f0-9]{32}$`)
	dockerTerminalRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)
	dockerTerminalInputIDPattern   = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)
	dockerTerminalInstancePattern  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,127}$`)
	dockerTerminalNamePattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
)

type dockerTerminalOpenInput struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	RequestID string `json:"request_id"`
	Rows      int    `json:"rows,omitempty"`
	Cols      int    `json:"cols,omitempty"`
}

type dockerTerminalSessionInput struct {
	SessionID string `json:"session_id"`
}

type dockerTerminalReadInput struct {
	SessionID string `json:"session_id"`
	After     uint64 `json:"after,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type dockerTerminalInputInput struct {
	SessionID string `json:"session_id"`
	InputID   string `json:"input_id"`
	Data      string `json:"data"`
}

type dockerTerminalResizeInput struct {
	SessionID string `json:"session_id"`
	Rows      int    `json:"rows"`
	Cols      int    `json:"cols"`
}

type dockerTerminalAgentStatus struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	SessionID      string    `json:"session_id"`
	TargetID       string    `json:"target_id"`
	Type           string    `json:"type"`
	Rows           int       `json:"rows"`
	Cols           int       `json:"cols"`
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	ExecutionState string    `json:"execution_state"`
	Attached       bool      `json:"attached"`
	StreamStarted  bool      `json:"stream_started"`
	CloseRequested bool      `json:"close_requested,omitempty"`
	ExitCode       *int      `json:"exit_code,omitempty"`
}

type dockerTerminalFrame struct {
	Sequence uint64 `json:"sequence"`
	Data     string `json:"data"`
}

type dockerTerminalOutput struct {
	dockerTerminalAgentStatus
	OutputAvailable bool `json:"output_available"`
}

type dockerTerminalReadOutput struct {
	dockerTerminalOutput
	Frames            []dockerTerminalFrame `json:"frames"`
	Truncated         bool                  `json:"truncated"`
	Next              uint64                `json:"next"`
	OutputUnavailable bool                  `json:"output_unavailable,omitempty"`
}

type dockerTerminalStored struct {
	ActorID           string            `json:"actor_id"`
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	RequestKey        string            `json:"request_key"`
	SessionID         string            `json:"session_id"`
	Rows              int               `json:"rows"`
	Cols              int               `json:"cols"`
	CreatedAt         time.Time         `json:"created_at"`
	ExpiresAt         time.Time         `json:"expires_at"`
	ExecutionState    string            `json:"execution_state"`
	Attached          bool              `json:"attached"`
	StreamStarted     bool              `json:"stream_started"`
	StreamAttempted   bool              `json:"stream_attempted"`
	CloseRequested    bool              `json:"close_requested,omitempty"`
	ExitCode          *int              `json:"exit_code,omitempty"`
	StatusCheckedAt   time.Time         `json:"status_checked_at,omitempty"`
	InputFingerprints map[string]string `json:"input_fingerprints,omitempty"`
	InputOrder        []string          `json:"input_order,omitempty"`
}

type dockerTerminalRequestRecord struct {
	ActorID   string    `json:"actor_id"`
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Rows      int       `json:"rows"`
	Cols      int       `json:"cols"`
	SessionID string    `json:"session_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type dockerTerminalRuntime struct {
	mu                sync.Mutex
	record            dockerTerminalStored
	frames            []dockerTerminalFrame
	frameBytes        int
	next              uint64
	outputUnavailable bool
	inputWriter       *io.PipeWriter
	streamCancel      context.CancelFunc
	streamDone        chan struct{}
	streamActive      bool
	inputGate         chan struct{}
	controlGate       chan struct{}
	notify            chan struct{}
	touchedAt         time.Time
}

type dockerTerminalOpenLock struct {
	mu   sync.Mutex
	refs int
}

type dockerTerminalBridge struct {
	mu          sync.Mutex
	capacityMu  sync.Mutex
	closeOnce   sync.Once
	closing     bool
	websocketWG sync.WaitGroup
	runtimes    map[string]*dockerTerminalRuntime
	openLocks   map[string]*dockerTerminalOpenLock
	openSlots   chan struct{}
	streamSlots chan struct{}
	actorSlots  map[string]int
	ctx         context.Context
	cancel      context.CancelFunc
}

func newDockerTerminalBridge() *dockerTerminalBridge {
	ctx, cancel := context.WithCancel(context.Background())
	return &dockerTerminalBridge{runtimes: map[string]*dockerTerminalRuntime{}, openLocks: map[string]*dockerTerminalOpenLock{}, openSlots: make(chan struct{}, 64), streamSlots: make(chan struct{}, 16), actorSlots: map[string]int{}, ctx: ctx, cancel: cancel}
}

func newDockerTerminalRuntime(record dockerTerminalStored) *dockerTerminalRuntime {
	return &dockerTerminalRuntime{record: record, outputUnavailable: record.StreamAttempted, inputGate: make(chan struct{}, 1), controlGate: make(chan struct{}, 1), notify: make(chan struct{}), touchedAt: time.Now()}
}

func lockDockerTerminalGate(ctx context.Context, gate chan struct{}) error {
	select {
	case gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return bridgeError(ctx.Err())
	}
}

func unlockDockerTerminalGate(gate chan struct{}) { <-gate }

func (runtime *dockerTerminalRuntime) signalLocked() {
	if runtime.notify != nil {
		close(runtime.notify)
	}
	runtime.notify = make(chan struct{})
}

func (runtime *dockerTerminalRuntime) changed() <-chan struct{} {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.notify
}

func (p *Provider) evictDockerTerminalRuntimeLocked() bool {
	var oldestID string
	var oldest *dockerTerminalRuntime
	var oldestAt time.Time
	for id, runtime := range p.docker.runtimes {
		runtime.mu.Lock()
		active, touched := runtime.streamActive, runtime.touchedAt
		if !active && (oldest == nil || touched.Before(oldestAt)) {
			oldestID, oldest, oldestAt = id, runtime, touched
		}
		runtime.mu.Unlock()
	}
	if oldest == nil {
		return false
	}
	oldest.mu.Lock()
	oldest.frames = nil
	oldest.frameBytes = 0
	oldest.outputUnavailable = true
	oldest.signalLocked()
	oldest.mu.Unlock()
	delete(p.docker.runtimes, oldestID)
	return true
}

func (b *dockerTerminalBridge) shutdown() {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closing = true
		runtimes := make([]*dockerTerminalRuntime, 0, len(b.runtimes))
		done := make([]<-chan struct{}, 0, len(b.runtimes))
		for _, runtime := range b.runtimes {
			runtimes = append(runtimes, runtime)
		}
		b.mu.Unlock()
		b.cancel()
		for _, runtime := range runtimes {
			runtime.mu.Lock()
			cancel, writer, streamDone := runtime.streamCancel, runtime.inputWriter, runtime.streamDone
			if streamDone != nil && runtime.streamActive {
				done = append(done, streamDone)
			}
			runtime.signalLocked()
			runtime.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			if writer != nil {
				_ = writer.Close()
			}
		}
		deadline := time.NewTimer(4 * time.Second)
		defer deadline.Stop()
		for _, streamDone := range done {
			select {
			case <-streamDone:
			case <-deadline.C:
				return
			}
		}
		websocketsDone := make(chan struct{})
		go func() {
			b.websocketWG.Wait()
			close(websocketsDone)
		}()
		select {
		case <-websocketsDone:
		case <-deadline.C:
		}
	})
}

func (b *dockerTerminalBridge) beginWebsocket() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closing {
		return false
	}
	b.websocketWG.Add(1)
	return true
}

func (b *dockerTerminalBridge) lockOpen(key string) func() {
	b.mu.Lock()
	lock := b.openLocks[key]
	if lock == nil {
		lock = &dockerTerminalOpenLock{}
		b.openLocks[key] = lock
	}
	lock.refs++
	b.mu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		b.mu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(b.openLocks, key)
		}
		b.mu.Unlock()
	}
}

func dockerTerminalAction(name string) bool {
	switch name {
	case "docker_terminal_open", "docker_terminal_read", "docker_terminal_input", "docker_terminal_resize", "docker_terminal_close", "docker_terminal_status":
		return true
	default:
		return false
	}
}

func dockerTerminalManageAction(name string) bool {
	return name == "docker_terminal_open" || name == "docker_terminal_input" || name == "docker_terminal_resize" || name == "docker_terminal_close"
}

func dockerTerminalSchema(name string) map[string]any {
	stringField := func(minLength, maxLength int, pattern string) map[string]any {
		return map[string]any{"type": "string", "minLength": minLength, "maxLength": maxLength, "pattern": pattern}
	}
	integerField := func(minimum, maximum int) map[string]any {
		return map[string]any{"type": "integer", "minimum": minimum, "maximum": maximum}
	}
	fields := map[string]any{}
	var required []string
	switch name {
	case "docker_terminal_open":
		fields = map[string]any{"id": stringField(1, 128, dockerTerminalInstancePattern.String()), "name": stringField(1, 64, dockerTerminalNamePattern.String()), "request_id": stringField(8, 128, dockerTerminalRequestIDPattern.String()), "rows": integerField(1, 500), "cols": integerField(1, 1000)}
		required = []string{"id", "name", "request_id"}
	case "docker_terminal_input":
		fields = map[string]any{"session_id": stringField(36, 36, dockerTerminalSessionIDPattern.String()), "input_id": stringField(8, 128, dockerTerminalInputIDPattern.String()), "data": stringField(4, 21848, "^[A-Za-z0-9+/]*={0,2}$")}
		required = []string{"session_id", "input_id", "data"}
	case "docker_terminal_resize":
		fields = map[string]any{"session_id": stringField(36, 36, dockerTerminalSessionIDPattern.String()), "rows": integerField(1, 500), "cols": integerField(1, 1000)}
		required = []string{"session_id", "rows", "cols"}
	case "docker_terminal_read":
		fields = map[string]any{"session_id": stringField(36, 36, dockerTerminalSessionIDPattern.String()), "after": integerField(0, 9007199254740991), "limit": integerField(1, dockerTerminalReadFrames)}
		required = []string{"session_id"}
	case "docker_terminal_close", "docker_terminal_status":
		fields = map[string]any{"session_id": stringField(36, 36, dockerTerminalSessionIDPattern.String())}
		required = []string{"session_id"}
	}
	return map[string]any{"type": "object", "properties": fields, "required": required, "additionalProperties": false}
}

func (p *Provider) dockerTerminalBindings() []core.Binding {
	descriptors := []core.Action{
		{Name: "docker_terminal_open", Description: "Open a managed Docker container with a fixed interactive shell", Scope: "containers:manage", InputSchema: dockerTerminalSchema("docker_terminal_open")},
		{Name: "docker_terminal_read", Description: "Read bounded output from an owned managed Docker terminal", Scope: "containers:read", ReadOnly: true, InputSchema: dockerTerminalSchema("docker_terminal_read")},
		{Name: "docker_terminal_input", Description: "Send one bounded base64 input frame to an owned managed Docker terminal", Scope: "containers:manage", InputSchema: dockerTerminalSchema("docker_terminal_input")},
		{Name: "docker_terminal_resize", Description: "Resize an owned managed Docker terminal", Scope: "containers:manage", InputSchema: dockerTerminalSchema("docker_terminal_resize")},
		{Name: "docker_terminal_close", Description: "Detach an owned managed Docker terminal and inspect its actual execution state", Scope: "containers:manage", InputSchema: dockerTerminalSchema("docker_terminal_close")},
		{Name: "docker_terminal_status", Description: "Recheck an owned managed Docker terminal against Docker Engine", Scope: "containers:read", ReadOnly: true, InputSchema: dockerTerminalSchema("docker_terminal_status")},
	}
	out := make([]core.Binding, 0, len(descriptors))
	for _, descriptor := range descriptors {
		action := descriptor
		out = append(out, core.Binding{
			Action: action,
			Resources: func(ctx context.Context, actor core.Actor, raw json.RawMessage) ([]string, error) {
				return p.dockerTerminalResources(ctx, actor, action.Name, raw)
			},
			Execute: func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
				return p.executeDockerTerminal(ctx, actor, action.Name, raw)
			},
		})
	}
	return out
}

func dockerTerminalResources(id, name string) []string {
	return []string{"software:" + id, "docker:container:" + id + ":" + name}
}

func (p *Provider) dockerTerminalResources(ctx context.Context, actor core.Actor, name string, raw json.RawMessage) ([]string, error) {
	current, err := p.deps.ResolveActor(ctx, actor)
	if err != nil {
		return nil, err
	}
	scope := "containers:read"
	if dockerTerminalManageAction(name) {
		scope = "containers:manage"
	}
	var resources []string
	if name == "docker_terminal_open" {
		var in dockerTerminalOpenInput
		if err = decodeDockerTerminal(raw, &in); err != nil {
			return nil, err
		}
		if !validDockerTerminalTarget(in.ID, in.Name) || !dockerTerminalRequestIDPattern.MatchString(in.RequestID) || in.Rows < 0 || in.Rows > 500 || in.Cols < 0 || in.Cols > 1000 {
			return nil, invalid()
		}
		resources = dockerTerminalResources(in.ID, in.Name)
	} else {
		var in dockerTerminalSessionInput
		switch name {
		case "docker_terminal_read":
			var read dockerTerminalReadInput
			if err = decodeDockerTerminal(raw, &read); err != nil || read.After > 9007199254740991 || read.Limit < 0 || read.Limit > dockerTerminalReadFrames {
				return nil, invalid()
			}
			in.SessionID = read.SessionID
		case "docker_terminal_input":
			var input dockerTerminalInputInput
			if err = decodeDockerTerminal(raw, &input); err != nil || !dockerTerminalInputIDPattern.MatchString(input.InputID) {
				return nil, invalid()
			}
			in.SessionID = input.SessionID
		case "docker_terminal_resize":
			var resize dockerTerminalResizeInput
			if err = decodeDockerTerminal(raw, &resize); err != nil || resize.Rows < 1 || resize.Rows > 500 || resize.Cols < 1 || resize.Cols > 1000 {
				return nil, invalid()
			}
			in.SessionID = resize.SessionID
		case "docker_terminal_close", "docker_terminal_status":
			if err = decodeDockerTerminal(raw, &in); err != nil {
				return nil, err
			}
		default:
			return nil, invalid()
		}
		if !dockerTerminalSessionIDPattern.MatchString(in.SessionID) {
			return nil, invalid()
		}
		record, loadErr := p.loadDockerTerminalRecord(ctx, in.SessionID)
		if loadErr != nil {
			return nil, loadErr
		}
		if record.ActorID != current.ID {
			return nil, denied()
		}
		resources = dockerTerminalResources(record.ID, record.Name)
	}
	for _, resource := range resources {
		if !p.deps.Authorize(current, scope, resource) {
			return nil, denied()
		}
	}
	return resources, nil
}

func validDockerTerminalTarget(id, name string) bool {
	return dockerTerminalInstancePattern.MatchString(id) && dockerTerminalNamePattern.MatchString(name)
}

func decodeDockerTerminal(raw json.RawMessage, out any) error {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return invalid()
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil {
		return invalid()
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return invalid()
	}
	return nil
}

func dockerTerminalRequestKey(actorID, requestID string) string {
	sum := sha256.Sum256([]byte(actorID + "\x00" + requestID))
	return "dctreq_" + hex.EncodeToString(sum[:16])
}

func (p *Provider) loadDockerTerminalRecord(ctx context.Context, sessionID string) (dockerTerminalStored, error) {
	if !dockerTerminalSessionIDPattern.MatchString(sessionID) {
		return dockerTerminalStored{}, invalid()
	}
	raw, err := p.deps.State.Get(ctx, dockerTerminalSessionsKind, sessionID)
	if err != nil {
		return dockerTerminalStored{}, bridgeError(err)
	}
	var record dockerTerminalStored
	if json.Unmarshal(raw, &record) != nil || record.SessionID != sessionID || !validDockerTerminalTarget(record.ID, record.Name) || record.ActorID == "" || record.RequestKey == "" {
		return dockerTerminalStored{}, failure("invalid_state", "Docker terminal ownership record is invalid", 500)
	}
	return record, nil
}

func (p *Provider) pruneDockerTerminalRecords(ctx context.Context) (map[string]int, int, error) {
	sessions, err := p.deps.State.List(ctx, dockerTerminalSessionsKind)
	if err != nil {
		return nil, 0, failure("session_state_unavailable", "Docker terminal history could not be bounded", 500)
	}
	requests, err := p.deps.State.List(ctx, dockerTerminalRequestsKind)
	if err != nil {
		return nil, 0, failure("session_state_unavailable", "Docker terminal retry history could not be bounded", 500)
	}
	now := time.Now().UTC()
	byActor := map[string]int{}
	seen := map[string]string{}
	for id, raw := range sessions {
		var record dockerTerminalStored
		if json.Unmarshal(raw, &record) != nil || record.SessionID != id || !dockerTerminalSessionIDPattern.MatchString(id) || record.ActorID == "" || !validDockerTerminalTarget(record.ID, record.Name) || record.ExpiresAt.IsZero() {
			return nil, 0, failure("invalid_state", "Docker terminal ownership history is invalid", 500)
		}
		if !record.ExpiresAt.Add(dockerTerminalRetention).After(now) {
			if err = p.deps.State.Delete(ctx, dockerTerminalSessionsKind, id); err != nil {
				return nil, 0, failure("session_state_unavailable", "Expired Docker terminal metadata could not be removed", 500)
			}
			continue
		}
		seen[id] = record.ActorID
		byActor[record.ActorID]++
	}
	for key, raw := range requests {
		var record dockerTerminalRequestRecord
		if json.Unmarshal(raw, &record) != nil || !strings.HasPrefix(key, "dctreq_") || record.ActorID == "" || !validDockerTerminalTarget(record.ID, record.Name) || !dockerTerminalSessionIDPattern.MatchString(record.SessionID) {
			return nil, 0, failure("invalid_state", "Docker terminal retry history is invalid", 500)
		}
		expires := record.ExpiresAt
		if expires.IsZero() {
			expires = record.CreatedAt.Add(dockerTerminalLifetime)
			if sessionRaw, ok := sessions[record.SessionID]; ok {
				var session dockerTerminalStored
				if json.Unmarshal(sessionRaw, &session) == nil && !session.ExpiresAt.IsZero() {
					expires = session.ExpiresAt
				}
			}
		}
		if !expires.Add(dockerTerminalRetention).After(now) {
			if err = p.deps.State.Delete(ctx, dockerTerminalRequestsKind, key); err != nil {
				return nil, 0, failure("session_state_unavailable", "Expired Docker terminal retry metadata could not be removed", 500)
			}
			continue
		}
		if owner, exists := seen[record.SessionID]; exists && owner != record.ActorID {
			return nil, 0, failure("invalid_state", "Docker terminal retry owner does not match its session", 500)
		}
		if _, exists := seen[record.SessionID]; !exists {
			seen[record.SessionID] = record.ActorID
			byActor[record.ActorID]++
		}
	}
	return byActor, len(seen), nil
}

func (p *Provider) runtimeFor(ctx context.Context, actorID, sessionID string) (*dockerTerminalRuntime, error) {
	record, err := p.loadDockerTerminalRecord(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if record.ActorID != actorID {
		return nil, denied()
	}
	p.docker.mu.Lock()
	runtime := p.docker.runtimes[sessionID]
	if runtime != nil {
		runtime.mu.Lock()
		runtime.touchedAt = time.Now()
		runtime.mu.Unlock()
	} else {
		for len(p.docker.runtimes) >= dockerTerminalRuntimeLimit {
			if !p.evictDockerTerminalRuntimeLocked() {
				p.docker.mu.Unlock()
				return nil, failure("busy", "Docker terminal runtime cache is full", 429)
			}
		}
		runtime = newDockerTerminalRuntime(record)
		p.docker.runtimes[sessionID] = runtime
	}
	p.docker.mu.Unlock()
	return runtime, nil
}

func (p *Provider) executeDockerTerminal(ctx context.Context, actor core.Actor, name string, raw json.RawMessage) (core.ActionResult, error) {
	current, err := p.deps.ResolveActor(ctx, actor)
	if err != nil {
		return core.ActionResult{}, err
	}
	resources, err := p.dockerTerminalResources(ctx, current, name, raw)
	if err != nil {
		return core.ActionResult{}, err
	}
	switch name {
	case "docker_terminal_open":
		var in dockerTerminalOpenInput
		if err = decodeDockerTerminal(raw, &in); err != nil {
			return core.ActionResult{}, err
		}
		data, openErr := p.openDockerTerminal(ctx, current, in)
		if openErr != nil {
			return core.ActionResult{}, openErr
		}
		return core.ActionResult{Data: data, Resources: resources, AuditSummary: map[string]any{"session_id": data.SessionID, "execution_state": data.ExecutionState}}, nil
	case "docker_terminal_read":
		var in dockerTerminalReadInput
		if err = decodeDockerTerminal(raw, &in); err != nil {
			return core.ActionResult{}, err
		}
		data, readErr := p.readDockerTerminal(ctx, current, in)
		if readErr != nil {
			return core.ActionResult{}, readErr
		}
		return core.ActionResult{Data: data, Resources: resources, AuditSummary: map[string]any{"session_id": in.SessionID, "output_frames": len(data.Frames)}}, nil
	case "docker_terminal_input":
		var in dockerTerminalInputInput
		if err = decodeDockerTerminal(raw, &in); err != nil {
			return core.ActionResult{}, err
		}
		data, inputErr := p.inputDockerTerminal(ctx, current, in)
		if inputErr != nil {
			return core.ActionResult{}, inputErr
		}
		return core.ActionResult{Data: data, Resources: resources, AuditSummary: map[string]any{"session_id": in.SessionID, "input_bytes": data.Bytes, "duplicate": data.Duplicate}}, nil
	case "docker_terminal_resize":
		var in dockerTerminalResizeInput
		if err = decodeDockerTerminal(raw, &in); err != nil {
			return core.ActionResult{}, err
		}
		data, resizeErr := p.resizeDockerTerminal(ctx, current, in)
		if resizeErr != nil {
			return core.ActionResult{}, resizeErr
		}
		return core.ActionResult{Data: data, Resources: resources, AuditSummary: map[string]any{"session_id": in.SessionID, "rows": in.Rows, "cols": in.Cols}}, nil
	case "docker_terminal_close":
		var in dockerTerminalSessionInput
		if err = decodeDockerTerminal(raw, &in); err != nil {
			return core.ActionResult{}, err
		}
		data, closeErr := p.closeDockerTerminal(ctx, current, in.SessionID)
		if closeErr != nil {
			return core.ActionResult{}, closeErr
		}
		return core.ActionResult{Data: data, Resources: resources, AuditSummary: map[string]any{"session_id": in.SessionID, "execution_state": data.ExecutionState, "close_requested": true}}, nil
	case "docker_terminal_status":
		var in dockerTerminalSessionInput
		if err = decodeDockerTerminal(raw, &in); err != nil {
			return core.ActionResult{}, err
		}
		data, statusErr := p.statusDockerTerminal(ctx, current, in.SessionID)
		if statusErr != nil {
			return core.ActionResult{}, statusErr
		}
		return core.ActionResult{Data: data, Resources: resources, AuditSummary: map[string]any{"session_id": in.SessionID, "execution_state": data.ExecutionState}}, nil
	default:
		return core.ActionResult{}, invalid()
	}
}

func (p *Provider) dockerTerminalAgentCall(ctx context.Context, actorID, operation string, input any, output any) error {
	if p.options.Agent == nil {
		return failure("agent_unavailable", "Docker terminal Agent is not configured", 503)
	}
	rpc, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	result, err := p.options.Agent.Run(rpc, agent.Request{ActorID: actorID, Operation: operation, Input: encoded(input)})
	if err != nil {
		return bridgeError(err)
	}
	data := result.Data
	if len(data) == 0 {
		data = json.RawMessage(result.Stdout)
	}
	if len(data) == 0 || len(data) > 128<<10 || json.Unmarshal(data, output) != nil {
		return failure("agent_response", "Docker terminal Agent returned an invalid bounded result", 502)
	}
	return nil
}

func (p *Provider) openDockerTerminal(ctx context.Context, actor core.Actor, in dockerTerminalOpenInput) (dockerTerminalOutput, error) {
	select {
	case p.docker.openSlots <- struct{}{}:
		defer func() { <-p.docker.openSlots }()
	case <-ctx.Done():
		return dockerTerminalOutput{}, bridgeError(ctx.Err())
	default:
		return dockerTerminalOutput{}, failure("busy", "Docker terminal open budget is exhausted", 429)
	}
	rows, cols := in.Rows, in.Cols
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	requestKey := dockerTerminalRequestKey(actor.ID, in.RequestID)
	unlock := p.docker.lockOpen(requestKey)
	defer unlock()
	var prior dockerTerminalRequestRecord
	if raw, err := p.deps.State.Get(ctx, dockerTerminalRequestsKind, requestKey); err == nil {
		if json.Unmarshal(raw, &prior) != nil || prior.ActorID != actor.ID || prior.ID != in.ID || prior.Name != in.Name || prior.Rows != rows || prior.Cols != cols || !dockerTerminalSessionIDPattern.MatchString(prior.SessionID) {
			return dockerTerminalOutput{}, failure("idempotency_conflict", "Docker terminal request_id is bound to different session parameters", 409)
		}
		runtime, e := p.runtimeFor(ctx, actor.ID, prior.SessionID)
		if e != nil {
			return dockerTerminalOutput{}, e
		}
		status, e := p.refreshDockerTerminalStatus(ctx, actor.ID, runtime, true)
		if e != nil {
			return dockerTerminalOutput{}, e
		}
		if !status.StreamStarted && !status.CloseRequested {
			if e = p.startDockerTerminalStream(actor.ID, runtime); e != nil {
				return dockerTerminalOutput{}, e
			}
			status = p.runtimeOutput(runtime)
		}
		return status, nil
	} else if core.ErrorCode(err) != "not_found" {
		return dockerTerminalOutput{}, bridgeError(err)
	}
	var status dockerTerminalAgentStatus
	var record dockerTerminalStored
	if err := func() error {
		p.docker.capacityMu.Lock()
		defer p.docker.capacityMu.Unlock()
		byActor, total, capacityErr := p.pruneDockerTerminalRecords(ctx)
		if capacityErr != nil {
			return capacityErr
		}
		if total >= dockerTerminalRecordLimit || byActor[actor.ID] >= dockerTerminalActorLimit {
			return failure("session_limit", "Docker terminal session history is full until older leases expire", 429)
		}
		input := dockerTerminalOpenInput{ID: in.ID, Name: in.Name, RequestID: in.RequestID, Rows: rows, Cols: cols}
		if callErr := p.dockerTerminalAgentCall(ctx, actor.ID, "docker_terminal_open", input, &status); callErr != nil {
			return callErr
		}
		if !dockerTerminalSessionIDPattern.MatchString(status.SessionID) || status.ID != in.ID || status.Name != in.Name || status.TargetID != "docker:"+in.ID+":"+in.Name || status.Rows != rows || status.Cols != cols || !status.ExpiresAt.After(time.Now()) {
			return failure("agent_response", "Docker terminal Agent returned inconsistent ownership metadata", 502)
		}
		record = dockerTerminalStored{ActorID: actor.ID, ID: in.ID, Name: in.Name, RequestKey: requestKey, SessionID: status.SessionID, Rows: rows, Cols: cols, CreatedAt: status.CreatedAt, ExpiresAt: status.ExpiresAt, ExecutionState: status.ExecutionState, Attached: status.Attached, StreamStarted: status.StreamStarted, StatusCheckedAt: time.Now().UTC(), InputFingerprints: map[string]string{}}
		requestRecord := dockerTerminalRequestRecord{ActorID: actor.ID, ID: in.ID, Name: in.Name, Rows: rows, Cols: cols, SessionID: status.SessionID, CreatedAt: status.CreatedAt, ExpiresAt: status.ExpiresAt}
		if saveErr := p.deps.State.Transaction(ctx, func(tx core.MetadataStore) error {
			if saveErr := tx.Save(ctx, dockerTerminalSessionsKind, status.SessionID, encoded(record)); saveErr != nil {
				return saveErr
			}
			return tx.Save(ctx, dockerTerminalRequestsKind, requestKey, encoded(requestRecord))
		}); saveErr != nil {
			return failure("session_state_unavailable", "Docker terminal ownership could not be saved", 500)
		}
		return nil
	}(); err != nil {
		return dockerTerminalOutput{}, err
	}
	runtime, err := p.runtimeFor(ctx, actor.ID, status.SessionID)
	if err != nil {
		return dockerTerminalOutput{}, err
	}
	if !status.StreamStarted {
		if err := p.startDockerTerminalStream(actor.ID, runtime); err != nil {
			return p.runtimeOutput(runtime), err
		}
		status = p.runtimeOutput(runtime).dockerTerminalAgentStatus
	}
	return p.runtimeOutput(runtime), nil
}

func (p *Provider) startDockerTerminalStream(actorID string, runtime *dockerTerminalRuntime) error {
	_, ok := p.options.Agent.(agent.StreamExecutor)
	if !ok {
		return failure("agent_unavailable", "Docker terminal Agent does not support authenticated streams", 503)
	}
	runtime.mu.Lock()
	if runtime.streamActive || runtime.record.StreamStarted || runtime.record.StreamAttempted {
		runtime.mu.Unlock()
		return nil
	}
	if runtime.record.CloseRequested || !runtime.record.ExpiresAt.After(time.Now()) {
		runtime.mu.Unlock()
		return failure("session_expired", "Docker terminal session is closed or expired", 410)
	}
	runtime.mu.Unlock()
	p.docker.mu.Lock()
	if p.docker.actorSlots[actorID] >= 4 {
		p.docker.mu.Unlock()
		return failure("busy", "Docker terminal session limit is reached", 429)
	}
	select {
	case p.docker.streamSlots <- struct{}{}:
		p.docker.actorSlots[actorID]++
	default:
		p.docker.mu.Unlock()
		return failure("busy", "Docker terminal stream budget is exhausted", 429)
	}
	p.docker.mu.Unlock()
	runtime.mu.Lock()
	if runtime.streamActive || runtime.record.StreamStarted || runtime.record.StreamAttempted {
		runtime.mu.Unlock()
		p.releaseDockerTerminalSlot(actorID)
		return nil
	}
	record := runtime.record
	record.StreamAttempted = true
	if err := p.deps.State.Save(context.Background(), dockerTerminalSessionsKind, record.SessionID, encoded(record)); err != nil {
		runtime.mu.Unlock()
		p.releaseDockerTerminalSlot(actorID)
		return failure("session_state_unavailable", "Docker terminal stream intent could not be saved", 500)
	}
	streamCtx, cancel := context.WithDeadline(p.docker.ctx, record.ExpiresAt)
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	done := make(chan struct{})
	runtime.record = record
	runtime.inputWriter = stdinWriter
	runtime.streamCancel = cancel
	runtime.streamDone = done
	runtime.streamActive = true
	runtime.outputUnavailable = false
	runtime.touchedAt = time.Now()
	runtime.signalLocked()
	runtime.mu.Unlock()
	go p.runDockerTerminalStream(streamCtx, cancel, actorID, record, runtime, stdinReader, stdinWriter, stdoutReader, stdoutWriter, done)
	go p.watchDockerTerminalLease(streamCtx, actorID, record.SessionID, cancel)
	return nil
}

func (p *Provider) watchDockerTerminalLease(ctx context.Context, actorID, sessionID string, cancel context.CancelFunc) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, action := range []string{"docker_terminal_read", "docker_terminal_close"} {
				_, err := p.dockerTerminalResources(ctx, core.Actor{ID: actorID}, action, encoded(dockerTerminalSessionInput{SessionID: sessionID}))
				if err != nil {
					cancel()
					return
				}
			}
		}
	}
}

func (p *Provider) releaseDockerTerminalSlot(actorID string) {
	p.docker.mu.Lock()
	if p.docker.actorSlots[actorID] > 1 {
		p.docker.actorSlots[actorID]--
	} else {
		delete(p.docker.actorSlots, actorID)
	}
	p.docker.mu.Unlock()
	select {
	case <-p.docker.streamSlots:
	default:
	}
}

func (p *Provider) runDockerTerminalStream(ctx context.Context, cancel context.CancelFunc, actorID string, record dockerTerminalStored, runtime *dockerTerminalRuntime, stdinReader *io.PipeReader, stdinWriter *io.PipeWriter, stdoutReader *io.PipeReader, stdoutWriter *io.PipeWriter, done chan struct{}) {
	defer close(done)
	defer cancel()
	defer stdinReader.Close()
	defer stdoutReader.Close()
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		buffer := make([]byte, 4096)
		for {
			n, err := stdoutReader.Read(buffer)
			if n > 0 {
				runtime.appendOutput(buffer[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	input := dockerTerminalSessionInput{SessionID: record.SessionID}
	_, streamErr := p.options.Agent.(agent.StreamExecutor).Stream(ctx, agent.Request{ActorID: actorID, Operation: "docker_terminal_stream", Input: encoded(input)}, agent.IO{Stdin: stdinReader, Stdout: stdoutWriter})
	_ = stdinWriter.Close()
	_ = stdoutWriter.Close()
	select {
	case <-outputDone:
	case <-time.After(2 * time.Second):
	}
	if p.docker.ctx.Err() != nil {
		runtime.mu.Lock()
		runtime.streamActive = false
		runtime.inputWriter = nil
		runtime.streamCancel = nil
		runtime.signalLocked()
		runtime.mu.Unlock()
		p.releaseDockerTerminalSlot(actorID)
		return
	}
	statusCtx, statusCancel := context.WithTimeout(context.Background(), 5*time.Second)
	var status dockerTerminalAgentStatus
	statusErr := p.dockerTerminalAgentCall(statusCtx, actorID, "docker_terminal_status", input, &status)
	statusCancel()
	runtime.mu.Lock()
	if statusErr == nil {
		runtime.record.ExecutionState = status.ExecutionState
		runtime.record.Attached = status.Attached
		runtime.record.StreamStarted = status.StreamStarted
		runtime.record.CloseRequested = runtime.record.CloseRequested || status.CloseRequested
		runtime.record.ExitCode = status.ExitCode
		runtime.record.StatusCheckedAt = time.Now().UTC()
	}
	runtime.streamActive = false
	runtime.inputWriter = nil
	runtime.streamCancel = nil
	if runtime.record.CloseRequested || !runtime.record.ExpiresAt.After(time.Now()) {
		runtime.frames = nil
		runtime.frameBytes = 0
		runtime.outputUnavailable = true
	}
	runtime.touchedAt = time.Now()
	runtime.signalLocked()
	stored := runtime.record
	runtime.mu.Unlock()
	_ = p.deps.State.Save(context.Background(), dockerTerminalSessionsKind, record.SessionID, encoded(stored))
	_ = streamErr // Stream errors are reflected by the live Engine status, never by echoing stream bytes.
	p.releaseDockerTerminalSlot(actorID)
}

func (runtime *dockerTerminalRuntime) appendOutput(data []byte) {
	if len(data) == 0 {
		return
	}
	for len(data) > 0 {
		n := min(len(data), 4096)
		encoded := base64.StdEncoding.EncodeToString(data[:n])
		runtime.mu.Lock()
		if runtime.record.CloseRequested || !runtime.record.ExpiresAt.After(time.Now()) {
			runtime.frames = nil
			runtime.frameBytes = 0
			runtime.outputUnavailable = true
		} else {
			runtime.next++
			runtime.frames = append(runtime.frames, dockerTerminalFrame{Sequence: runtime.next, Data: encoded})
			runtime.frameBytes += len(encoded)
			for (runtime.frameBytes > dockerTerminalRingBytes || len(runtime.frames) > dockerTerminalRingFrames) && len(runtime.frames) > 1 {
				runtime.frameBytes -= len(runtime.frames[0].Data)
				runtime.frames = runtime.frames[1:]
			}
		}
		runtime.touchedAt = time.Now()
		runtime.signalLocked()
		runtime.mu.Unlock()
		data = data[n:]
	}
}

func (p *Provider) runtimeOutput(runtime *dockerTerminalRuntime) dockerTerminalOutput {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.touchedAt = time.Now()
	return dockerTerminalOutput{dockerTerminalAgentStatus: dockerTerminalAgentStatus{ID: runtime.record.ID, Name: runtime.record.Name, SessionID: runtime.record.SessionID, TargetID: "docker:" + runtime.record.ID + ":" + runtime.record.Name, Type: "docker", Rows: runtime.record.Rows, Cols: runtime.record.Cols, CreatedAt: runtime.record.CreatedAt, ExpiresAt: runtime.record.ExpiresAt, ExecutionState: runtime.record.ExecutionState, Attached: runtime.streamActive, StreamStarted: runtime.record.StreamStarted, CloseRequested: runtime.record.CloseRequested, ExitCode: runtime.record.ExitCode}, OutputAvailable: !runtime.outputUnavailable}
}

func (p *Provider) refreshDockerTerminalStatus(ctx context.Context, actorID string, runtime *dockerTerminalRuntime, force bool) (dockerTerminalOutput, error) {
	runtime.mu.Lock()
	if !force && time.Since(runtime.record.StatusCheckedAt) < 2*time.Second {
		output := p.runtimeOutputLocked(runtime)
		runtime.mu.Unlock()
		return output, nil
	}
	sessionID := runtime.record.SessionID
	runtime.mu.Unlock()
	var status dockerTerminalAgentStatus
	if err := p.dockerTerminalAgentCall(ctx, actorID, "docker_terminal_status", dockerTerminalSessionInput{SessionID: sessionID}, &status); err != nil {
		runtime.mu.Lock()
		changed := runtime.record.ExecutionState != "unknown"
		runtime.record.ExecutionState = "unknown"
		runtime.record.Attached = false
		runtime.record.StatusCheckedAt = time.Now().UTC()
		runtime.touchedAt = time.Now()
		if changed {
			runtime.signalLocked()
		}
		stored := runtime.record
		output := p.runtimeOutputLocked(runtime)
		runtime.mu.Unlock()
		_ = p.deps.State.Save(context.Background(), dockerTerminalSessionsKind, sessionID, encoded(stored))
		if core.ErrorCode(err) == "not_found" || core.ErrorCode(err) == "ownership_invalid" {
			return dockerTerminalOutput{}, err
		}
		return output, nil
	}
	runtime.mu.Lock()
	changed := runtime.record.ExecutionState != status.ExecutionState || runtime.record.StreamStarted != status.StreamStarted || runtime.record.CloseRequested != (runtime.record.CloseRequested || status.CloseRequested) || (runtime.record.ExitCode == nil) != (status.ExitCode == nil)
	if runtime.record.ExitCode != nil && status.ExitCode != nil && *runtime.record.ExitCode != *status.ExitCode {
		changed = true
	}
	runtime.record.ExecutionState = status.ExecutionState
	runtime.record.Attached = status.Attached
	runtime.record.StreamStarted = status.StreamStarted
	runtime.record.CloseRequested = runtime.record.CloseRequested || status.CloseRequested
	if !runtime.streamActive && !status.StreamStarted {
		runtime.record.StreamAttempted = false
	}
	runtime.record.ExitCode = status.ExitCode
	runtime.record.StatusCheckedAt = time.Now().UTC()
	runtime.touchedAt = time.Now()
	stored := runtime.record
	output := p.runtimeOutputLocked(runtime)
	if changed {
		runtime.signalLocked()
	}
	runtime.mu.Unlock()
	if err := p.deps.State.Save(context.Background(), dockerTerminalSessionsKind, sessionID, encoded(stored)); err != nil {
		return dockerTerminalOutput{}, failure("session_state_unavailable", "Docker terminal status could not be saved", 500)
	}
	return output, nil
}

func (p *Provider) runtimeOutputLocked(runtime *dockerTerminalRuntime) dockerTerminalOutput {
	runtime.touchedAt = time.Now()
	return dockerTerminalOutput{dockerTerminalAgentStatus: dockerTerminalAgentStatus{ID: runtime.record.ID, Name: runtime.record.Name, SessionID: runtime.record.SessionID, TargetID: "docker:" + runtime.record.ID + ":" + runtime.record.Name, Type: "docker", Rows: runtime.record.Rows, Cols: runtime.record.Cols, CreatedAt: runtime.record.CreatedAt, ExpiresAt: runtime.record.ExpiresAt, ExecutionState: runtime.record.ExecutionState, Attached: runtime.streamActive, StreamStarted: runtime.record.StreamStarted, CloseRequested: runtime.record.CloseRequested, ExitCode: runtime.record.ExitCode}, OutputAvailable: !runtime.outputUnavailable}
}

func (p *Provider) statusDockerTerminal(ctx context.Context, actor core.Actor, sessionID string) (dockerTerminalOutput, error) {
	runtime, err := p.runtimeFor(ctx, actor.ID, sessionID)
	if err != nil {
		return dockerTerminalOutput{}, err
	}
	return p.refreshDockerTerminalStatus(ctx, actor.ID, runtime, true)
}

func (p *Provider) readDockerTerminal(ctx context.Context, actor core.Actor, in dockerTerminalReadInput) (dockerTerminalReadOutput, error) {
	if in.After > 9007199254740991 {
		return dockerTerminalReadOutput{}, invalid()
	}
	limit := in.Limit
	if limit == 0 {
		limit = 8
	}
	if limit < 1 || limit > dockerTerminalReadFrames {
		return dockerTerminalReadOutput{}, invalid()
	}
	runtime, err := p.runtimeFor(ctx, actor.ID, in.SessionID)
	if err != nil {
		return dockerTerminalReadOutput{}, err
	}
	status, err := p.refreshDockerTerminalStatus(ctx, actor.ID, runtime, false)
	if err != nil {
		return dockerTerminalReadOutput{}, err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	output := dockerTerminalReadOutput{dockerTerminalOutput: status, Frames: []dockerTerminalFrame{}, Next: in.After, OutputUnavailable: runtime.outputUnavailable}
	if runtime.outputUnavailable {
		return output, nil
	}
	if len(runtime.frames) > 0 && in.After+1 < runtime.frames[0].Sequence {
		output.Truncated = true
	}
	for _, frame := range runtime.frames {
		if frame.Sequence > in.After {
			output.Frames = append(output.Frames, frame)
			output.Next = frame.Sequence
			if len(output.Frames) == limit {
				break
			}
		}
	}
	return output, nil
}

type dockerTerminalInputAck struct {
	SessionID string `json:"session_id"`
	InputID   string `json:"input_id"`
	Accepted  bool   `json:"accepted"`
	Duplicate bool   `json:"duplicate"`
	Bytes     int    `json:"bytes"`
}

func (p *Provider) inputDockerTerminal(ctx context.Context, actor core.Actor, in dockerTerminalInputInput) (dockerTerminalInputAck, error) {
	data, err := base64.StdEncoding.DecodeString(in.Data)
	if err != nil || len(data) == 0 || len(data) > dockerTerminalInputBytes || !dockerTerminalInputIDPattern.MatchString(in.InputID) {
		return dockerTerminalInputAck{}, failure("invalid_input", "Input requires an 8–128 character id and 1–16384 base64-encoded bytes", 400)
	}
	runtime, err := p.runtimeFor(ctx, actor.ID, in.SessionID)
	if err != nil {
		return dockerTerminalInputAck{}, err
	}
	digest := sha256.Sum256(data)
	fingerprint := hex.EncodeToString(digest[:])
	select {
	case runtime.inputGate <- struct{}{}:
	case <-ctx.Done():
		return dockerTerminalInputAck{}, bridgeError(ctx.Err())
	}
	defer func() { <-runtime.inputGate }()
	runtime.mu.Lock()
	if old, exists := runtime.record.InputFingerprints[in.InputID]; exists {
		runtime.mu.Unlock()
		if old != fingerprint {
			return dockerTerminalInputAck{}, failure("idempotency_conflict", "Input ID is already bound to different bytes", 409)
		}
		return dockerTerminalInputAck{SessionID: in.SessionID, InputID: in.InputID, Accepted: true, Duplicate: true, Bytes: len(data)}, nil
	}
	if runtime.record.CloseRequested || !runtime.record.ExpiresAt.After(time.Now()) {
		runtime.mu.Unlock()
		return dockerTerminalInputAck{}, failure("session_expired", "Docker terminal session is closed or expired", 410)
	}
	if !runtime.streamActive || runtime.inputWriter == nil {
		runtime.mu.Unlock()
		return dockerTerminalInputAck{}, failure("session_unavailable", "Docker terminal input stream is detached or unavailable", 409)
	}
	if len(runtime.record.InputFingerprints) >= dockerTerminalInputIDs {
		runtime.mu.Unlock()
		return dockerTerminalInputAck{}, failure("input_dedupe_full", "Docker terminal input retry history is full", 429)
	}
	if runtime.record.InputFingerprints == nil {
		runtime.record.InputFingerprints = map[string]string{}
	}
	runtime.record.InputFingerprints[in.InputID] = fingerprint
	runtime.record.InputOrder = append(runtime.record.InputOrder, in.InputID)
	if err = p.deps.State.Save(ctx, dockerTerminalSessionsKind, in.SessionID, encoded(runtime.record)); err != nil {
		delete(runtime.record.InputFingerprints, in.InputID)
		runtime.record.InputOrder = runtime.record.InputOrder[:len(runtime.record.InputOrder)-1]
		runtime.mu.Unlock()
		return dockerTerminalInputAck{}, failure("session_state_unavailable", "Docker terminal input retry identity could not be saved", 500)
	}
	writer := runtime.inputWriter
	runtime.mu.Unlock()
	written := make(chan error, 1)
	go func() {
		_, writeErr := writer.Write(data)
		written <- writeErr
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err = <-written:
		if err != nil {
			return dockerTerminalInputAck{}, failure("input_delivery_unknown", "Docker terminal input delivery is uncertain; the retry ID will not be replayed", 502)
		}
	case <-ctx.Done():
		return dockerTerminalInputAck{}, failure("input_delivery_unknown", "Docker terminal input delivery was interrupted; the retry ID will not be replayed", 409)
	case <-timer.C:
		return dockerTerminalInputAck{}, failure("input_delivery_unknown", "Docker terminal input delivery timed out; the retry ID will not be replayed", 504)
	}
	return dockerTerminalInputAck{SessionID: in.SessionID, InputID: in.InputID, Accepted: true, Bytes: len(data)}, nil
}

func (p *Provider) resizeDockerTerminal(ctx context.Context, actor core.Actor, in dockerTerminalResizeInput) (dockerTerminalOutput, error) {
	runtime, err := p.runtimeFor(ctx, actor.ID, in.SessionID)
	if err != nil {
		return dockerTerminalOutput{}, err
	}
	if err = lockDockerTerminalGate(ctx, runtime.controlGate); err != nil {
		return dockerTerminalOutput{}, err
	}
	defer unlockDockerTerminalGate(runtime.controlGate)
	runtime.mu.Lock()
	if runtime.record.CloseRequested || !runtime.record.ExpiresAt.After(time.Now()) {
		runtime.mu.Unlock()
		return dockerTerminalOutput{}, failure("session_expired", "Docker terminal session is closed or expired", 410)
	}
	runtime.mu.Unlock()
	var status dockerTerminalAgentStatus
	if err = p.dockerTerminalAgentCall(ctx, actor.ID, "docker_terminal_resize", in, &status); err != nil {
		return dockerTerminalOutput{}, err
	}
	runtime.mu.Lock()
	runtime.record.Rows, runtime.record.Cols = in.Rows, in.Cols
	runtime.record.ExecutionState, runtime.record.Attached = status.ExecutionState, status.Attached
	runtime.record.StreamStarted, runtime.record.ExitCode = status.StreamStarted, status.ExitCode
	runtime.record.CloseRequested = runtime.record.CloseRequested || status.CloseRequested
	runtime.record.StatusCheckedAt = time.Now().UTC()
	runtime.touchedAt = time.Now()
	if err = p.deps.State.Save(ctx, dockerTerminalSessionsKind, in.SessionID, encoded(runtime.record)); err != nil {
		runtime.mu.Unlock()
		return dockerTerminalOutput{}, failure("session_state_unavailable", "Docker terminal dimensions could not be saved", 500)
	}
	output := p.runtimeOutputLocked(runtime)
	runtime.signalLocked()
	runtime.mu.Unlock()
	return output, nil
}

func (p *Provider) closeDockerTerminal(ctx context.Context, actor core.Actor, sessionID string) (dockerTerminalOutput, error) {
	runtime, err := p.runtimeFor(ctx, actor.ID, sessionID)
	if err != nil {
		return dockerTerminalOutput{}, err
	}
	runtime.mu.Lock()
	runtime.record.CloseRequested = true
	runtime.frames = nil
	runtime.frameBytes = 0
	runtime.outputUnavailable = true
	runtime.touchedAt = time.Now()
	if err = p.deps.State.Save(ctx, dockerTerminalSessionsKind, sessionID, encoded(runtime.record)); err != nil {
		runtime.mu.Unlock()
		return dockerTerminalOutput{}, failure("session_state_unavailable", "Docker terminal close intent could not be saved", 500)
	}
	cancel, writer, done := runtime.streamCancel, runtime.inputWriter, runtime.streamDone
	runtime.signalLocked()
	runtime.mu.Unlock()
	if writer != nil {
		_ = writer.Close()
	}
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
	if err = lockDockerTerminalGate(ctx, runtime.controlGate); err != nil {
		return dockerTerminalOutput{}, err
	}
	defer unlockDockerTerminalGate(runtime.controlGate)
	var status dockerTerminalAgentStatus
	if err = p.dockerTerminalAgentCall(ctx, actor.ID, "docker_terminal_close", dockerTerminalSessionInput{SessionID: sessionID}, &status); err != nil {
		// The stream has been detached locally. Return the last observation as
		// unknown if Engine status cannot be refreshed; never claim it was killed.
		runtime.mu.Lock()
		runtime.record.ExecutionState, runtime.record.Attached = "unknown", false
		runtime.record.StatusCheckedAt = time.Now().UTC()
		runtime.signalLocked()
		output := p.runtimeOutputLocked(runtime)
		stored := runtime.record
		runtime.mu.Unlock()
		_ = p.deps.State.Save(context.Background(), dockerTerminalSessionsKind, sessionID, encoded(stored))
		return output, nil
	}
	runtime.mu.Lock()
	runtime.record.ExecutionState, runtime.record.Attached = status.ExecutionState, status.Attached
	runtime.record.StreamStarted, runtime.record.ExitCode = status.StreamStarted, status.ExitCode
	runtime.record.CloseRequested = runtime.record.CloseRequested || status.CloseRequested
	runtime.record.StatusCheckedAt = time.Now().UTC()
	runtime.signalLocked()
	if err = p.deps.State.Save(ctx, dockerTerminalSessionsKind, sessionID, encoded(runtime.record)); err != nil {
		runtime.mu.Unlock()
		return dockerTerminalOutput{}, failure("session_state_unavailable", "Docker terminal close status could not be saved", 500)
	}
	output := p.runtimeOutputLocked(runtime)
	runtime.mu.Unlock()
	return output, nil
}

func (p *Provider) dockerTerminalStreamActor(r *http.Request, sessionID string) (core.Actor, error) {
	actor, err := httpapi.RevalidateActor(r)
	if err != nil {
		return actor, err
	}
	if _, err = p.dockerTerminalResources(r.Context(), actor, "docker_terminal_read", encoded(dockerTerminalSessionInput{SessionID: sessionID})); err != nil {
		return actor, err
	}
	return actor, nil
}
