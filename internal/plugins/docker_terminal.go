package plugins

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

const (
	dockerTerminalMaxAge     = 30 * time.Minute
	dockerTerminalMaxID      = 128
	dockerTerminalRetention  = 24 * time.Hour
	dockerTerminalMaxRecords = 128
)

var dockerTerminalSessionID = regexp.MustCompile(`^dct_[a-f0-9]{32}$`)
var dockerTerminalRequestID = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)
var dockerTerminalPrivateRequestID = regexp.MustCompile(`^dctreq_[a-f0-9]{32}$`)

// DockerTerminalAgentOperations are registered only in the privileged Agent.
// The API process invokes Stream over the protected Unix pipe; it never opens
// the managed Docker socket itself.
func DockerTerminalAgentOperations() []string {
	return []string{"docker_terminal_open", "docker_terminal_info", "docker_terminal_status", "docker_terminal_resize", "docker_terminal_close", "docker_terminal_stream"}
}

type DockerTerminalAgentExecutor struct {
	executor     *PrivilegedExecutor
	mu           sync.Mutex
	openCapacity sync.Mutex
	requests     map[string]*dockerTerminalRequestLock
	active       map[string]bool
}

type dockerTerminalRequestLock struct {
	mu   sync.Mutex
	refs int
}

func NewDockerTerminalAgentExecutor(executor *PrivilegedExecutor) *DockerTerminalAgentExecutor {
	return &DockerTerminalAgentExecutor{executor: executor, requests: map[string]*dockerTerminalRequestLock{}, active: map[string]bool{}}
}

func (e *DockerTerminalAgentExecutor) setActive(sessionID string, active bool) {
	e.mu.Lock()
	if active {
		e.active[sessionID] = true
	} else {
		delete(e.active, sessionID)
	}
	e.mu.Unlock()
}

func (e *DockerTerminalAgentExecutor) isActive(sessionID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.active[sessionID]
}

func (e *DockerTerminalAgentExecutor) lockRequest(key string) func() {
	e.mu.Lock()
	lock := e.requests[key]
	if lock == nil {
		lock = &dockerTerminalRequestLock{}
		e.requests[key] = lock
	}
	lock.refs++
	e.mu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		e.mu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(e.requests, key)
		}
		e.mu.Unlock()
	}
}

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

type dockerTerminalResizeInput struct {
	SessionID string `json:"session_id"`
	Rows      int    `json:"rows"`
	Cols      int    `json:"cols"`
}

type dockerTerminalRequestRecord struct {
	ActorID   string    `json:"actor_id"`
	RequestID string    `json:"request_id"`
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	SessionID string    `json:"session_id"`
	Rows      int       `json:"rows"`
	Cols      int       `json:"cols"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type dockerTerminalRecord struct {
	ActorID        string    `json:"actor_id"`
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	SessionID      string    `json:"session_id"`
	RequestID      string    `json:"request_id"`
	ExecutionID    string    `json:"execution_id"`
	State          string    `json:"state"`
	Rows           int       `json:"rows"`
	Cols           int       `json:"cols"`
	Started        bool      `json:"started"`
	Attached       bool      `json:"attached"`
	CloseRequested bool      `json:"close_requested,omitempty"`
	ExitCode       *int      `json:"exit_code,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type dockerTerminalPublic struct {
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

type dockerTerminalStatus struct {
	ExecutionState string
	Running        bool
	Attached       bool
	ExitCode       *int
}

func dockerTerminalRequestKey(actorID, requestID string) string {
	sum := sha256.Sum256([]byte(actorID + "\x00" + requestID))
	return "dctreq_" + hex.EncodeToString(sum[:16])
}

func (e *DockerTerminalAgentExecutor) pruneRecords(now time.Time) (int, map[string]int, error) {
	dir := filepath.Join(e.executor.config.StateDir, "private")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, nil, failure("private_state_unavailable", "Docker terminal private state could not be scanned", 500)
	}
	seen := map[string]string{}
	counts := map[string]int{}
	removed := false
	for _, entry := range entries {
		base := strings.TrimSuffix(entry.Name(), ".json")
		if base == entry.Name() || (!dockerTerminalSessionID.MatchString(base) && !dockerTerminalPrivateRequestID.MatchString(base)) {
			continue
		}
		if !entry.Type().IsRegular() {
			return 0, nil, failure("ownership_invalid", "Docker terminal private state contains an unsafe entry", 412)
		}
		path := filepath.Join(dir, entry.Name())
		actorID, sessionID, expiresAt := "", "", time.Time{}
		if dockerTerminalSessionID.MatchString(base) {
			var record dockerTerminalRecord
			if err = e.executor.load("private", base, &record); err != nil || record.SessionID != base || record.ActorID == "" || !validInstanceID(record.ID) || !utilityName.MatchString(record.Name) || record.ExpiresAt.IsZero() {
				return 0, nil, failure("ownership_invalid", "Docker terminal private state is invalid", 412)
			}
			actorID, sessionID, expiresAt = record.ActorID, record.SessionID, record.ExpiresAt
		} else {
			var record dockerTerminalRequestRecord
			if err = e.executor.load("private", base, &record); err != nil || record.ActorID == "" || record.RequestID == "" || record.SessionID == "" || !dockerTerminalSessionID.MatchString(record.SessionID) || !validInstanceID(record.ID) || !utilityName.MatchString(record.Name) {
				return 0, nil, failure("ownership_invalid", "Docker terminal request state is invalid", 412)
			}
			actorID, sessionID, expiresAt = record.ActorID, record.SessionID, record.ExpiresAt
			if expiresAt.IsZero() {
				expiresAt = record.CreatedAt.Add(dockerTerminalMaxAge)
			}
		}
		if !expiresAt.Add(dockerTerminalRetention).After(now) {
			if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
				return 0, nil, failure("private_state_unavailable", "Expired Docker terminal state could not be removed", 500)
			}
			removed = true
			continue
		}
		if previous, exists := seen[sessionID]; !exists {
			seen[sessionID] = actorID
			counts[actorID]++
		} else if previous != actorID {
			return 0, nil, failure("ownership_invalid", "Docker terminal request and session owners differ", 412)
		}
	}
	if removed {
		directory, openErr := os.Open(dir)
		if openErr != nil {
			return 0, nil, failure("private_state_unavailable", "Docker terminal private state directory could not be synced", 500)
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil || closeErr != nil {
			return 0, nil, failure("private_state_unavailable", "Expired Docker terminal state removal could not be synced", 500)
		}
	}
	return len(seen), counts, nil
}

func (e *DockerTerminalAgentExecutor) Validate(request agent.Request) error {
	if e == nil || e.executor == nil || request.ActorID == "" || len(request.ActorID) > dockerTerminalMaxID {
		return failure("unauthorized", "A trusted Docker terminal actor is required", 401)
	}
	switch request.Operation {
	case "docker_terminal_open":
		var in dockerTerminalOpenInput
		if err := parse(request.Input, &in); err != nil {
			return err
		}
		if !validInstanceID(in.ID) || !utilityName.MatchString(in.Name) || !dockerTerminalRequestID.MatchString(in.RequestID) {
			return failure("invalid_input", "Invalid owned Docker terminal identity", 400)
		}
		if in.Rows < 0 || in.Rows > 500 || in.Cols < 0 || in.Cols > 1000 {
			return failure("invalid_input", "Terminal dimensions are out of range", 400)
		}
	case "docker_terminal_info", "docker_terminal_status", "docker_terminal_close", "docker_terminal_stream":
		var in dockerTerminalSessionInput
		if err := parse(request.Input, &in); err != nil {
			return err
		}
		if !dockerTerminalSessionID.MatchString(in.SessionID) {
			return failure("invalid_input", "Invalid Docker terminal session identity", 400)
		}
	case "docker_terminal_resize":
		var in dockerTerminalResizeInput
		if err := parse(request.Input, &in); err != nil {
			return err
		}
		if !dockerTerminalSessionID.MatchString(in.SessionID) || in.Rows < 1 || in.Rows > 500 || in.Cols < 1 || in.Cols > 1000 {
			return failure("invalid_input", "Invalid Docker terminal dimensions or session", 400)
		}
	default:
		return agent.ErrUnauthorized
	}
	return nil
}

func (e *DockerTerminalAgentExecutor) Run(ctx context.Context, request agent.Request) (result agent.Result, returned error) {
	result = agent.Result{Operation: request.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	defer func() { result.FinishedAt = time.Now().UTC() }()
	if err := e.Validate(request); err != nil {
		return result, err
	}
	var output any
	var err error
	switch request.Operation {
	case "docker_terminal_open":
		var in dockerTerminalOpenInput
		_ = json.Unmarshal(request.Input, &in)
		output, err = e.open(ctx, request.ActorID, in)
	case "docker_terminal_info", "docker_terminal_status":
		var in dockerTerminalSessionInput
		_ = json.Unmarshal(request.Input, &in)
		output, err = e.status(ctx, request.ActorID, in.SessionID)
	case "docker_terminal_close":
		var in dockerTerminalSessionInput
		_ = json.Unmarshal(request.Input, &in)
		output, err = e.close(ctx, request.ActorID, in.SessionID)
	case "docker_terminal_resize":
		var in dockerTerminalResizeInput
		_ = json.Unmarshal(request.Input, &in)
		output, err = e.resize(ctx, request.ActorID, in)
	case "docker_terminal_stream":
		return result, failure("stream_required", "Docker terminal requires the authenticated duplex stream", 400)
	}
	if err != nil {
		return result, dockerTerminalSafeError(err)
	}
	raw, err := json.Marshal(output)
	if err != nil || len(raw) > 64<<10 {
		return result, failure("invalid_response", "Docker terminal state exceeded its response bound", 502)
	}
	result.Stdout = string(raw)
	result.ExitCode = 0
	return result, nil
}

func (e *DockerTerminalAgentExecutor) open(ctx context.Context, actorID string, in dockerTerminalOpenInput) (dockerTerminalPublic, error) {
	rows, cols := in.Rows, in.Cols
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	requestKey := dockerTerminalRequestKey(actorID, in.RequestID)
	unlockRequest := e.lockRequest(requestKey)
	defer unlockRequest()
	e.openCapacity.Lock()
	defer e.openCapacity.Unlock()
	total, byActor, err := e.pruneRecords(time.Now().UTC())
	if err != nil {
		return dockerTerminalPublic{}, err
	}
	var prior dockerTerminalRequestRecord
	if err = e.executor.load("private", requestKey, &prior); err == nil {
		if prior.ActorID != actorID || prior.RequestID != in.RequestID || prior.ID != in.ID || prior.Name != in.Name || prior.Rows != rows || prior.Cols != cols || !dockerTerminalSessionID.MatchString(prior.SessionID) {
			return dockerTerminalPublic{}, failure("ownership_invalid", "Protected Docker terminal request identity changed", 412)
		}
		info, publicErr := e.public(ctx, actorID, prior.SessionID)
		if core.ErrorCode(publicErr) == "not_found" {
			return dockerTerminalPublic{ID: prior.ID, Name: prior.Name, SessionID: prior.SessionID, TargetID: "docker:" + prior.ID + ":" + prior.Name, Type: "docker", Rows: rows, Cols: cols, CreatedAt: prior.CreatedAt, ExpiresAt: prior.CreatedAt.Add(dockerTerminalMaxAge), ExecutionState: "unknown"}, nil
		}
		return info, publicErr
	} else if core.ErrorCode(err) != "not_found" {
		return dockerTerminalPublic{}, err
	}
	if total >= dockerTerminalMaxRecords || byActor[actorID] >= dockerTerminalMaxRecords/4 {
		return dockerTerminalPublic{}, failure("session_limit", "Docker terminal session history is full until older leases expire", 429)
	}
	unlock, err := e.executor.lockContext(ctx, in.ID)
	if err != nil {
		return dockerTerminalPublic{}, err
	}
	defer unlock()
	instance, err := e.ownedInstance(ctx, in.ID, in.Name, true)
	if err != nil {
		return dockerTerminalPublic{}, err
	}
	var sessionBytes [16]byte
	if _, err = rand.Read(sessionBytes[:]); err != nil {
		return dockerTerminalPublic{}, failure("internal", "Docker terminal identity could not be generated", 500)
	}
	sessionID := "dct_" + hex.EncodeToString(sessionBytes[:])
	now := time.Now().UTC()
	prior = dockerTerminalRequestRecord{ActorID: actorID, RequestID: in.RequestID, ID: in.ID, Name: in.Name, SessionID: sessionID, Rows: rows, Cols: cols, CreatedAt: now, ExpiresAt: now.Add(dockerTerminalMaxAge)}
	// Reserve before Engine I/O so a retry with the same request_id can never
	// create another shell after an uncertain response or service interruption.
	if err = e.executor.save("private", requestKey, prior); err != nil {
		return dockerTerminalPublic{}, failure("private_state_unavailable", "Docker terminal retry identity could not be saved", 500)
	}
	record := dockerTerminalRecord{ActorID: actorID, ID: in.ID, Name: in.Name, SessionID: sessionID, RequestID: in.RequestID, State: "unknown", Rows: rows, Cols: cols, CreatedAt: now, ExpiresAt: now.Add(dockerTerminalMaxAge)}
	if err = e.executor.save("private", sessionID, record); err != nil {
		return dockerTerminalPublic{}, failure("private_state_unavailable", "Docker terminal recovery identity could not be saved", 500)
	}
	container := dockerOwnedName(instance, in.Name)
	var created struct {
		ID string `json:"Id"`
	}
	create := map[string]any{"AttachStdin": true, "AttachStdout": true, "AttachStderr": true, "Tty": true, "Cmd": []string{"/bin/sh", "-i"}, "Env": []string{"TERM=xterm-256color"}, "ConsoleSize": []uint16{uint16(rows), uint16(cols)}}
	if err = e.executor.dockerRequest(ctx, instance, http.MethodPost, "/containers/"+url.PathEscape(container)+"/exec", create, &created); err != nil {
		return dockerTerminalPublic{}, err
	}
	if !dockerExecID.MatchString(created.ID) {
		return dockerTerminalPublic{}, failure("invalid_response", "Docker returned an invalid terminal execution identity", 502)
	}
	record.ExecutionID, record.State = created.ID, "created"
	if err = e.executor.save("private", sessionID, record); err != nil {
		return dockerTerminalPublic{}, failure("private_state_unavailable", "Docker terminal recovery identity could not be saved", 500)
	}
	return e.public(ctx, actorID, sessionID)
}

func (e *DockerTerminalAgentExecutor) public(ctx context.Context, actorID, sessionID string) (dockerTerminalPublic, error) {
	var record dockerTerminalRecord
	if err := e.executor.load("private", sessionID, &record); err != nil || record.SessionID != sessionID || record.ActorID != actorID || !validInstanceID(record.ID) || !utilityName.MatchString(record.Name) {
		return dockerTerminalPublic{}, failure("not_found", "Owned Docker terminal session does not exist", 404)
	}
	if record.ExecutionID == "" {
		return dockerTerminalPublic{ID: record.ID, Name: record.Name, SessionID: sessionID, TargetID: "docker:" + record.ID + ":" + record.Name, Type: "docker", Rows: record.Rows, Cols: record.Cols, CreatedAt: record.CreatedAt, ExpiresAt: record.ExpiresAt, ExecutionState: "unknown", StreamStarted: record.Started, CloseRequested: record.CloseRequested}, nil
	}
	if !dockerExecID.MatchString(record.ExecutionID) {
		return dockerTerminalPublic{}, failure("ownership_invalid", "Protected Docker terminal execution identity is invalid", 412)
	}
	status, err := e.inspect(ctx, record)
	if err != nil {
		status.ExecutionState, status.Attached = "unknown", false
	}
	status.Attached = e.isActive(sessionID)
	state := status.ExecutionState
	if state == "unknown" && !record.Started {
		state = "created"
	}
	info := dockerTerminalPublic{ID: record.ID, Name: record.Name, SessionID: sessionID, TargetID: "docker:" + record.ID + ":" + record.Name, Type: "docker", Rows: record.Rows, Cols: record.Cols, CreatedAt: record.CreatedAt, ExpiresAt: record.ExpiresAt, ExecutionState: state, Attached: status.Attached, StreamStarted: record.Started, CloseRequested: record.CloseRequested}
	if status.ExitCode != nil {
		info.ExitCode = status.ExitCode
	}
	return info, nil
}

func (e *DockerTerminalAgentExecutor) status(ctx context.Context, actorID, sessionID string) (dockerTerminalPublic, error) {
	return e.public(ctx, actorID, sessionID)
}

func (e *DockerTerminalAgentExecutor) loadSession(actorID, sessionID string) (dockerTerminalRecord, error) {
	var record dockerTerminalRecord
	if err := e.executor.load("private", sessionID, &record); err != nil || record.SessionID != sessionID || record.ActorID != actorID || !validInstanceID(record.ID) || !utilityName.MatchString(record.Name) || (record.ExecutionID != "" && !dockerExecID.MatchString(record.ExecutionID)) {
		return dockerTerminalRecord{}, failure("not_found", "Owned Docker terminal session does not exist", 404)
	}
	return record, nil
}

func validateDockerTerminalMutable(record dockerTerminalRecord) error {
	if record.CloseRequested {
		return failure("session_closed", "Docker terminal session has been closed", 410)
	}
	if !record.ExpiresAt.After(time.Now()) {
		return failure("session_expired", "Docker terminal session lease expired", 410)
	}
	return nil
}

func (e *DockerTerminalAgentExecutor) close(ctx context.Context, actorID, sessionID string) (dockerTerminalPublic, error) {
	record, err := e.loadSession(actorID, sessionID)
	if err != nil {
		return dockerTerminalPublic{}, err
	}
	unlock, err := e.executor.lockContext(ctx, record.ID)
	if err != nil {
		return dockerTerminalPublic{}, err
	}
	record, err = e.loadSession(actorID, sessionID)
	if err != nil {
		unlock()
		return dockerTerminalPublic{}, err
	}
	record.CloseRequested = true
	record.State = "close_requested"
	if err = e.executor.save("private", sessionID, record); err != nil {
		unlock()
		return dockerTerminalPublic{}, failure("private_state_unavailable", "Docker terminal close state could not be saved", 500)
	}
	unlock()
	return e.public(ctx, actorID, sessionID)
}

func (e *DockerTerminalAgentExecutor) ownedInstance(ctx context.Context, id, name string, requireRunning bool) (Instance, error) {
	if err := e.executor.ensureInstallFinalized(ctx, id); err != nil {
		return Instance{}, err
	}
	instance, err := e.executor.get(ctx, id, false)
	if err != nil {
		return Instance{}, err
	}
	if instance.PluginID != "docker" || instance.Status == "uninstalled" {
		return Instance{}, failure("runtime_unavailable", "Managed Docker instance is unavailable", 412)
	}
	inspection, err := e.executor.dockerInspect(ctx, instance, "containers", name)
	if err != nil {
		return Instance{}, err
	}
	if requireRunning {
		state, _ := inspection["State"].(map[string]any)
		running, _ := state["Running"].(bool)
		if !running {
			return Instance{}, failure("container_not_running", "Owned Docker container must be running", 412)
		}
	}
	return instance, nil
}

func (e *DockerTerminalAgentExecutor) inspect(ctx context.Context, record dockerTerminalRecord) (dockerTerminalStatus, error) {
	instance, err := e.ownedInstance(ctx, record.ID, record.Name, false)
	if err != nil {
		return dockerTerminalStatus{}, err
	}
	status, err := e.executor.inspectDockerExec(ctx, instance, record.ExecutionID)
	if err != nil {
		return dockerTerminalStatus{}, err
	}
	state := mapDockerTerminalState(status, record)
	return dockerTerminalStatus{ExecutionState: state, Running: state == "running", Attached: record.Attached, ExitCode: status.ExitCode}, nil
}

func mapDockerTerminalState(status dockerExecStatusOutput, record dockerTerminalRecord) string {
	if status.Running {
		return "running"
	}
	if status.ExitCode != nil {
		return "exited"
	}
	if !record.Started {
		return "created"
	}
	return "unknown"
}

func (e *DockerTerminalAgentExecutor) resize(ctx context.Context, actorID string, in dockerTerminalResizeInput) (dockerTerminalPublic, error) {
	record, err := e.loadSession(actorID, in.SessionID)
	if err != nil {
		return dockerTerminalPublic{}, err
	}
	if !dockerExecID.MatchString(record.ExecutionID) {
		return e.public(ctx, actorID, in.SessionID)
	}
	unlock, err := e.executor.lockContext(ctx, record.ID)
	if err != nil {
		return dockerTerminalPublic{}, err
	}
	defer unlock()
	record, err = e.loadSession(actorID, in.SessionID)
	if err != nil {
		return dockerTerminalPublic{}, err
	}
	if err = validateDockerTerminalMutable(record); err != nil {
		return dockerTerminalPublic{}, err
	}
	instance, err := e.ownedInstance(ctx, record.ID, record.Name, true)
	if err != nil {
		return dockerTerminalPublic{}, err
	}
	status, err := e.inspect(ctx, record)
	if err != nil || status.ExecutionState != "running" {
		if err != nil {
			return dockerTerminalPublic{}, err
		}
		return e.public(ctx, actorID, in.SessionID)
	}
	path := "/exec/" + url.PathEscape(record.ExecutionID) + "/resize?h=" + strconv.Itoa(in.Rows) + "&w=" + strconv.Itoa(in.Cols)
	if err = e.executor.dockerRequest(ctx, instance, http.MethodPost, path, nil, nil); err != nil {
		return dockerTerminalPublic{}, err
	}
	record.Rows, record.Cols = in.Rows, in.Cols
	if err = e.executor.save("private", in.SessionID, record); err != nil {
		return dockerTerminalPublic{}, failure("private_state_unavailable", "Docker terminal dimensions could not be saved", 500)
	}
	return e.public(ctx, actorID, in.SessionID)
}

func (e *DockerTerminalAgentExecutor) Stream(ctx context.Context, request agent.Request, streams agent.IO) (result agent.Result, returned error) {
	result = agent.Result{Operation: request.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	defer func() { result.FinishedAt = time.Now().UTC() }()
	if err := e.Validate(request); err != nil {
		return result, err
	}
	var in dockerTerminalSessionInput
	_ = json.Unmarshal(request.Input, &in)
	record, err := e.loadSession(request.ActorID, in.SessionID)
	if err != nil {
		return result, err
	}
	if err = validateDockerTerminalMutable(record); err != nil {
		return result, err
	}
	if !dockerExecID.MatchString(record.ExecutionID) {
		return result, failure("session_unavailable", "Docker terminal execution identity is not recoverable", 409)
	}
	if !record.ExpiresAt.After(time.Now()) {
		return result, failure("session_expired", "Docker terminal session lease expired", 410)
	}
	streamCtx, streamCancel := context.WithDeadline(ctx, record.ExpiresAt)
	defer streamCancel()
	ctx = streamCtx
	if streams.Stdin == nil || streams.Stdout == nil {
		return result, failure("invalid_input", "Docker terminal requires bounded input and output streams", 400)
	}
	unlock, err := e.executor.lockContext(ctx, record.ID)
	if err != nil {
		return result, err
	}
	record, err = e.loadSession(request.ActorID, in.SessionID)
	if err != nil {
		unlock()
		return result, err
	}
	if err = validateDockerTerminalMutable(record); err != nil {
		unlock()
		return result, err
	}
	if e.isActive(in.SessionID) {
		unlock()
		return result, failure("session_attached", "Docker terminal already has an authenticated stream", 409)
	}
	if record.Started {
		unlock()
		return result, failure("session_unavailable", "Docker terminal stream cannot be resumed after detaching", 409)
	}
	instance, err := e.ownedInstance(ctx, record.ID, record.Name, true)
	if err != nil {
		unlock()
		return result, err
	}
	record.Started, record.Attached, record.State = true, false, "starting"
	if err = e.executor.save("private", in.SessionID, record); err != nil {
		unlock()
		return result, failure("private_state_unavailable", "Docker terminal start state could not be saved", 500)
	}
	e.setActive(in.SessionID, true)
	unlock()
	streamErr := startDockerTerminalStream(ctx, instance, record, streams)
	statusCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	status, statusErr := e.inspect(statusCtx, record)
	cancel()
	e.setActive(in.SessionID, false)
	updateCtx, updateCancel := context.WithTimeout(context.Background(), 5*time.Second)
	updateUnlock, updateErr := e.executor.lockContext(updateCtx, record.ID)
	if updateErr == nil {
		latest, loadErr := e.loadSession(request.ActorID, in.SessionID)
		if loadErr == nil && latest.ExecutionID == record.ExecutionID && latest.ID == record.ID && latest.Name == record.Name {
			latest.Attached = false
			if statusErr != nil {
				latest.State = "unknown"
				latest.ExitCode = nil
			} else {
				latest.State = status.ExecutionState
				latest.ExitCode = status.ExitCode
			}
			if saveErr := e.executor.save("private", in.SessionID, latest); saveErr != nil {
				updateErr = saveErr
			} else {
				record = latest
			}
		} else if loadErr != nil {
			updateErr = loadErr
		} else {
			updateErr = failure("ownership_invalid", "Docker terminal session identity changed during stream close", 412)
		}
		updateUnlock()
	}
	updateCancel()
	if updateErr != nil {
		return result, failure("private_state_unavailable", "Docker terminal final state could not be saved", 500)
	}
	data, _ := json.Marshal(map[string]any{"session_id": in.SessionID, "execution_state": record.State, "attached": false, "exit_code": record.ExitCode})
	result.Stdout, result.ExitCode = string(data), 0
	if streamErr != nil {
		return result, dockerTerminalSafeError(streamErr)
	}
	return result, nil
}

func startDockerTerminalStream(ctx context.Context, instance Instance, record dockerTerminalRecord, streams agent.IO) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(instance.Paths.RunDir, "docker.sock"))
	if err != nil {
		return failure("docker_unavailable", "Protected Docker socket is unavailable", 502)
	}
	closeDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-closeDone:
		}
	}()
	defer close(closeDone)
	defer conn.Close()
	body, _ := json.Marshal(map[string]any{"Detach": false, "Tty": true})
	request, err := http.NewRequest(http.MethodPost, "http://docker/v1.45/exec/"+url.PathEscape(record.ExecutionID)+"/start", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Host = "docker"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "tcp")
	if err = request.Write(conn); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return failure("docker_terminal_failed", "Docker rejected the owned terminal start request", 502)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return failure("docker_terminal_failed", "Docker terminal handshake failed", 502)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		_ = response.Body.Close()
		return failure("docker_terminal_failed", "Docker rejected the owned terminal start request", 502)
	}
	inputDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(conn, streams.Stdin)
		if unix, ok := conn.(*net.UnixConn); ok {
			_ = unix.CloseWrite()
		}
		inputDone <- copyErr
	}()
	_, outputErr := io.Copy(streams.Stdout, reader)
	_ = conn.Close()
	if streams.Stdin != nil {
		_ = streams.Stdin.Close()
	}
	select {
	case <-inputDone:
	case <-time.After(2 * time.Second):
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if outputErr != nil && !errors.Is(outputErr, io.EOF) {
		return failure("docker_terminal_failed", "Docker terminal output stream ended unexpectedly", 502)
	}
	return nil
}

func dockerTerminalSafeError(err error) error {
	if err == nil {
		return nil
	}
	var typed *core.Error
	if errors.As(err, &typed) {
		return err
	}
	var remote *agent.RemoteError
	if errors.As(err, &remote) {
		switch remote.Code {
		case "timeout", "canceled", "not_found", "ownership_invalid", "runtime_unavailable", "runtime_type_mismatch", "container_not_running", "docker_unavailable", "private_state_unavailable", "session_closed", "session_expired", "session_attached", "session_unavailable", "invalid_input", "forbidden":
			return failure(remote.Code, remote.Message, remote.Status)
		}
	}
	if errors.Is(err, context.Canceled) {
		return failure("canceled", "Docker terminal stream detached", 409)
	}
	return failure("docker_terminal_failed", "Protected Docker terminal could not complete", 502)
}

var _ agent.Executor = (*DockerTerminalAgentExecutor)(nil)
var _ agent.StreamExecutor = (*DockerTerminalAgentExecutor)(nil)
