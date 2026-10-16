package plugins

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/jlaffaye/ftp"
)

type FTPDestination struct {
	ID            string `json:"id"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Username      string `json:"username"`
	TLSMode       string `json:"tls_mode"`
	Directory     string `json:"directory"`
	CredentialRef string `json:"credential_ref"`
	Revision      int64  `json:"revision"`
}
type FTPRegisterInput struct {
	FTPDestination
	Password         string `json:"password"`
	ExpectedRevision int64  `json:"expected_revision"`
}
type FTPTransferInput struct {
	ID         string `json:"id"`
	SoftwareID string `json:"software_id"`
	Filename   string `json:"filename"`
	Direction  string `json:"direction"`
}

var artifactName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func FTPResource(id string) string   { return "backup:ftp:" + id }
func NativeOperations() []string     { return []string{"backup_ftp_transfer", "docker_registry_pull"} }
func ManagedJobOperations() []string { return []string{"plugin_apply"} }
func AgentJobOperations() []string {
	operations := []string{}
	for _, operation := range Operations() {
		if operation != "plugin_apply" {
			operations = append(operations, operation)
		}
	}
	return operations
}
func softwareArtifactAllowed(instance Instance) error {
	switch instance.PluginID {
	case "mysql", "mysql-community", "mariadb", "postgresql", "mongodb":
		return failure("database_backup_scope_required", "Database artifacts require their dedicated database-scoped export/import; generic software backup transfer is denied", 412)
	}
	return nil
}
func (m *Manager) checkSoftwareArtifact(ctx context.Context, id string) error {
	var instance Instance
	raw, _ := json.Marshal(IDInput{ID: id})
	if err := m.agentJSON(ctx, "plugin_get", raw, &instance); err != nil {
		return err
	}
	return softwareArtifactAllowed(instance)
}
func (m *Manager) ftpProfile(ctx context.Context, id string) (FTPDestination, error) {
	var profile FTPDestination
	raw, rev, err := m.deps.State.GetVersioned(ctx, "ftp_destinations", id)
	if err != nil {
		return profile, err
	}
	if err = json.Unmarshal(raw, &profile); err != nil {
		return profile, failure("invalid_state", "FTP destination metadata is corrupt", 500)
	}
	profile.Revision = rev
	return profile, nil
}
func ftpResources(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var input struct {
		ID         string `json:"id"`
		SoftwareID string `json:"software_id"`
	}
	if json.Unmarshal(raw, &input) != nil || !validInstanceID(input.ID) {
		return nil, failure("invalid_input", "Invalid FTP destination ID", 400)
	}
	resources := []string{FTPResource(input.ID)}
	if input.SoftwareID != "" {
		if !validInstanceID(input.SoftwareID) {
			return nil, failure("invalid_input", "Invalid owned artifact source", 400)
		}
		resources = append(resources, InstanceResource(input.SoftwareID))
	}
	return resources, nil
}
func ftpDial(ctx context.Context, profile FTPDestination, password string) (*ftp.ServerConn, error) {
	return ftpDialLimited(ctx, profile, password, maxExtractedBytes)
}

type ftpBoundedConn struct {
	net.Conn
	remaining int64
	stop      func() bool
}

func (c *ftpBoundedConn) Read(data []byte) (int, error) {
	if c.remaining <= 0 {
		return 0, io.ErrShortBuffer
	}
	if int64(len(data)) > c.remaining {
		data = data[:c.remaining]
	}
	_ = c.Conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	n, err := c.Conn.Read(data)
	c.remaining -= int64(n)
	return n, err
}
func (c *ftpBoundedConn) Write(data []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return c.Conn.Write(data)
}
func (c *ftpBoundedConn) Close() error {
	if c.stop != nil {
		c.stop()
	}
	return c.Conn.Close()
}
func ftpDialLimited(ctx context.Context, profile FTPDestination, password string, maxData int64) (*ftp.ServerConn, error) {
	if strings.ContainsAny(profile.Host, "/\\\x00\r\n") || profile.Host == "" || profile.Port < 1 || profile.Port > 65535 {
		return nil, failure("invalid_destination", "FTP host/port is invalid", 400)
	}
	if hasFTPCommandControl(profile.Username) || hasFTPCommandControl(password) {
		return nil, failure("invalid_credentials", "FTP credentials cannot contain protocol control characters", 400)
	}
	options := []ftp.DialOption{ftp.DialWithContext(ctx), ftp.DialWithTimeout(15 * time.Second), ftp.DialWithShutTimeout(time.Second), ftp.DialWithTrustPasvIP(false)}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: profile.Host}
	switch profile.TLSMode {
	case "explicit":
		options = append(options, ftp.DialWithExplicitTLS(tlsConfig))
	case "implicit":
		options = append(options, ftp.DialWithTLS(tlsConfig))
	case "loopback":
		ip := net.ParseIP(profile.Host)
		if ip == nil || !ip.IsLoopback() {
			return nil, failure("tls_required", "Plain FTP is allowed only for an explicit loopback destination", 400)
		}
	default:
		return nil, failure("tls_required", "Choose verified explicit or implicit FTP TLS", 400)
	}
	var dials atomic.Int32
	options = append(options, ftp.DialWithDialFunc(func(network, address string) (net.Conn, error) {
		connection, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		isControl := dials.Add(1) == 1
		limit := maxData + 1
		if isControl {
			limit = 2 << 20
		}
		bounded := &ftpBoundedConn{Conn: connection, remaining: limit}
		bounded.stop = context.AfterFunc(ctx, func() { _ = connection.Close() })
		if profile.TLSMode == "implicit" || (!isControl && profile.TLSMode == "explicit") {
			return tls.Client(bounded, tlsConfig), nil
		}
		return bounded, nil
	}))
	connection, err := ftp.Dial(net.JoinHostPort(profile.Host, strconv.Itoa(profile.Port)), options...)
	if err != nil {
		return nil, failure("ftp_unavailable", "FTP destination could not be reached with its required transport security", 502)
	}
	if err = connection.Login(profile.Username, password); err != nil {
		_ = connection.Quit()
		return nil, failure("ftp_auth_failed", "FTP rejected the destination credentials", 401)
	}
	return connection, nil
}

func hasFTPCommandControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func (m *Manager) ftpBindings() []core.Binding {
	bindings := []core.Binding{m.binding("backup_ftp_destinations_list", "List authorized FTP backup destinations without credentials", "backup:read", true, obj(map[string]any{}), func(context.Context, core.Actor, json.RawMessage) ([]string, error) { return nil, nil }, func(ctx context.Context, actor core.Actor, _ json.RawMessage) (core.ActionResult, error) {
		rows, err := m.deps.State.List(ctx, "ftp_destinations")
		if err != nil {
			return core.ActionResult{}, err
		}
		items := []FTPDestination{}
		for _, row := range rows {
			var item FTPDestination
			if json.Unmarshal(row, &item) == nil && m.deps.Authorize(actor, "backup:read", FTPResource(item.ID)) {
				item.CredentialRef = ""
				items = append(items, item)
			}
		}
		return core.ActionResult{Data: map[string]any{"items": items}}, nil
	}),
		m.binding("backup_ftp_destination_register", "Verify FTP login and store a resource-bound encrypted credential", "backup:manage", false, obj(map[string]any{"id": idSchema(), "host": textSchema(), "port": map[string]any{"type": "integer", "minimum": 1, "maximum": 65535}, "username": textSchema(), "password": map[string]any{"type": "string", "maxLength": 4096, "format": "password"}, "tls_mode": enumSchema("explicit", "implicit", "loopback"), "directory": map[string]any{"type": "string", "maxLength": 1024}, "expected_revision": revisionSchema()}, "id", "host", "port", "username", "password", "tls_mode", "directory", "expected_revision"), ftpResources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			var input FTPRegisterInput
			if err := parse(raw, &input); err != nil {
				return core.ActionResult{}, err
			}
			if strings.ContainsAny(input.Directory, "\x00\r\n") || path.Clean(input.Directory) != input.Directory || strings.Contains(input.Directory, "..") {
				return core.ActionResult{}, failure("invalid_destination", "FTP directory must be a clean bounded remote path", 400)
			}
			connection, err := ftpDial(ctx, input.FTPDestination, input.Password)
			if err != nil {
				return core.ActionResult{}, err
			}
			defer connection.Quit()
			if err = connection.ChangeDir(input.Directory); err != nil {
				return core.ActionResult{}, failure("ftp_directory_unavailable", "FTP account cannot access the requested destination directory", 412)
			}
			ref, err := m.deps.Secrets.Put(ctx, actor, FTPResource(input.ID), "ftp_password", []byte(input.Password))
			if err != nil {
				return core.ActionResult{}, err
			}
			input.CredentialRef = ref
			data, _ := json.Marshal(input.FTPDestination)
			revision, err := m.deps.State.CompareAndSwap(ctx, "ftp_destinations", input.ID, input.ExpectedRevision, data)
			if err != nil {
				_ = m.deps.Secrets.Delete(ctx, actor, FTPResource(input.ID), ref)
				return core.ActionResult{}, err
			}
			profile := input.FTPDestination
			profile.Revision = revision
			profile.CredentialRef = ""
			return core.ActionResult{Data: profile, Resources: []string{FTPResource(input.ID)}}, nil
		}),
		m.binding("backup_ftp_remote_list", "Read a bounded backup destination listing", "backup:read", true, obj(map[string]any{"id": idSchema()}, "id"), ftpResources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			var input IDInput
			if err := parse(raw, &input); err != nil {
				return core.ActionResult{}, err
			}
			profile, err := m.ftpProfile(ctx, input.ID)
			if err != nil {
				return core.ActionResult{}, err
			}
			password, err := m.deps.Secrets.Get(ctx, actor, FTPResource(input.ID), profile.CredentialRef)
			if err != nil {
				return core.ActionResult{}, err
			}
			connection, err := ftpDialLimited(ctx, profile, string(password), 2<<20)
			clear(password)
			if err != nil {
				return core.ActionResult{}, err
			}
			defer connection.Quit()
			items, err := connection.List(profile.Directory)
			if err != nil {
				return core.ActionResult{}, failure("ftp_list_failed", "FTP destination listing failed", 502)
			}
			if len(items) > 256 {
				return core.ActionResult{}, failure("result_too_large", "Destination has more than 256 entries; choose a smaller backup folder", 413)
			}
			return core.ActionResult{Data: map[string]any{"items": items}, Resources: []string{FTPResource(input.ID)}}, nil
		}),
		m.binding("backup_ftp_transfer", "Queue a streaming transfer of one owned backup artifact; task stores only destination and artifact IDs", "backup:manage", false, obj(map[string]any{"id": idSchema(), "software_id": idSchema(), "filename": map[string]any{"type": "string", "pattern": artifactName.String()}, "direction": enumSchema("upload", "download")}, "id", "software_id", "filename", "direction"), ftpResources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			resources, err := ftpResources(ctx, actor, raw)
			if err != nil {
				return core.ActionResult{}, err
			}
			var input FTPTransferInput
			if err = parse(raw, &input); err != nil {
				return core.ActionResult{}, err
			}
			if err = m.checkSoftwareArtifact(ctx, input.SoftwareID); err != nil {
				return core.ActionResult{}, err
			}
			job, err := m.deps.Tasks.Submit(ctx, actor, "backup_ftp_transfer", raw, resources)
			if err != nil {
				return core.ActionResult{}, err
			}
			id, _ := job["id"].(string)
			return core.ActionResult{Data: job, TaskID: id, Resources: resources}, nil
		})}
	for i := range bindings {
		bindings[i].Action.Task = bindings[i].Action.Name == "backup_ftp_transfer"
	}
	return bindings
}
func (m *Manager) Validate(request agent.Request) error {
	if request.Operation == "plugin_apply" {
		if m.executor == nil {
			return failure("agent_unavailable", "Software installation requires a configured protected Agent", 503)
		}
		return m.executor.Validate(request)
	}
	if request.Operation == "docker_registry_pull" {
		var input dockerRegistryPullTask
		if err := parse(request.Input, &input); err != nil {
			return err
		}
		if !validInstanceID(input.ID) || !dockerImage.MatchString(input.Image) || !dockerCredentialRef.MatchString(input.CredentialRef) {
			return failure("invalid_input", "Invalid private registry pull task", 400)
		}
		if m.executor == nil {
			return failure("agent_unavailable", "Private registry pull requires a configured protected Agent", 503)
		}
		return nil
	}
	if request.Operation != "backup_ftp_transfer" {
		return agent.ErrUnauthorized
	}
	var input FTPTransferInput
	if err := parse(request.Input, &input); err != nil {
		return err
	}
	if !validInstanceID(input.ID) || !validInstanceID(input.SoftwareID) || !artifactName.MatchString(input.Filename) || (input.Direction != "upload" && input.Direction != "download") {
		return failure("invalid_input", "Invalid streaming backup descriptor", 400)
	}
	if _, ok := m.executor.(interface {
		Stream(context.Context, agent.Request, agent.IO) (agent.Result, error)
	}); !ok {
		return failure("stream_unavailable", "Trusted Agent transport does not support streaming artifacts", 412)
	}
	return nil
}
func (m *Manager) Run(ctx context.Context, request agent.Request) (result agent.Result, returned error) {
	result = agent.Result{Operation: request.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	defer func() { result.FinishedAt = time.Now().UTC() }()
	if err := m.Validate(request); err != nil {
		return result, err
	}
	actor, err := m.deps.ResolveActor(ctx, core.Actor{ID: request.ActorID})
	if err != nil {
		return result, err
	}
	if request.Operation == "plugin_apply" {
		resources, err := m.applyResources(ctx, actor, request.Input)
		if err != nil {
			return result, err
		}
		for _, resource := range resources {
			if !m.deps.Authorize(actor, "software:manage", resource) {
				return result, failure("forbidden", "Software plan's original management grant is no longer valid", 403)
			}
		}
		return m.executor.Run(ctx, request)
	}
	if request.Operation == "docker_registry_pull" {
		return m.runDockerRegistryPull(ctx, request, actor)
	}
	resources, err := ftpResources(ctx, actor, request.Input)
	if err != nil {
		return result, err
	}
	for _, resource := range resources {
		if !m.deps.Authorize(actor, "backup:manage", resource) {
			return result, failure("forbidden", "Backup grant no longer authorizes this transfer", 403)
		}
	}
	var input FTPTransferInput
	_ = json.Unmarshal(request.Input, &input)
	if err = m.checkSoftwareArtifact(ctx, input.SoftwareID); err != nil {
		return result, err
	}
	profile, err := m.ftpProfile(ctx, input.ID)
	if err != nil {
		return result, err
	}
	password, err := m.deps.Secrets.Get(ctx, actor, FTPResource(input.ID), profile.CredentialRef)
	if err != nil {
		return result, err
	}
	connection, err := ftpDial(ctx, profile, string(password))
	clear(password)
	if err != nil {
		return result, err
	}
	defer connection.Quit()
	remote := path.Join(profile.Directory, input.Filename)
	operationInput, _ := json.Marshal(struct {
		ID        string `json:"id"`
		Filename  string `json:"filename"`
		Direction string `json:"direction"`
	}{input.SoftwareID, input.Filename, input.Direction})
	streamer := m.executor.(interface {
		Stream(context.Context, agent.Request, agent.IO) (agent.Result, error)
	})
	if input.Direction == "download" {
		size, err := connection.FileSize(remote)
		if err != nil || size < 0 || size > maxExtractedBytes {
			return result, failure("ftp_artifact_invalid", "Remote artifact size is unknown or exceeds four GiB", 413)
		}
		reader, err := connection.Retr(remote)
		if err != nil {
			return result, failure("ftp_transfer_failed", "FTP artifact download failed", 502)
		}
		defer reader.Close()
		operationInput, _ = json.Marshal(map[string]any{"id": input.SoftwareID, "filename": input.Filename, "direction": input.Direction, "expected_bytes": size})
		result, err = streamer.Stream(ctx, agent.Request{Operation: "backup_artifact", Input: operationInput}, agent.IO{Stdin: reader})
		if err != nil || result.ExitCode != 0 {
			return result, failure("ftp_transfer_failed", "Owned artifact download did not complete", 502)
		}
		if err = reader.Close(); err != nil {
			return result, failure("ftp_transfer_failed", "FTP rejected the completed download; inspect the artifact before restoring it", 502)
		}
		result.Operation = request.Operation
		return result, nil
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	done := make(chan error, 1)
	go func() {
		agentResult, err := streamer.Stream(ctx, agent.Request{Operation: "backup_artifact", Input: operationInput}, agent.IO{Stdout: writer})
		if err == nil && agentResult.ExitCode != 0 {
			err = failure("artifact_read_failed", "Owned backup artifact could not be streamed", 502)
		}
		_ = writer.CloseWithError(err)
		done <- err
	}()
	transferErr := connection.Stor(remote, reader)
	_ = reader.CloseWithError(transferErr)
	streamErr := <-done
	if transferErr != nil || streamErr != nil {
		return result, failure("ftp_transfer_failed", "FTP upload did not finish; the task is not successful", 502)
	}
	result.ExitCode = 0
	result.Stdout = `{"transferred":true}`
	return result, nil
}
func (e *PrivilegedExecutor) artifactStream(ctx context.Context, request agent.Request, streams agent.IO) (result agent.Result, err error) {
	result = agent.Result{Operation: request.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	defer func() { result.FinishedAt = time.Now().UTC() }()
	var input struct {
		ID            string `json:"id"`
		Filename      string `json:"filename"`
		Direction     string `json:"direction"`
		ExpectedBytes int64  `json:"expected_bytes,omitempty"`
	}
	if err = parse(request.Input, &input); err != nil {
		return result, err
	}
	if !validInstanceID(input.ID) || !artifactName.MatchString(input.Filename) {
		return result, failure("invalid_input", "Invalid owned backup artifact", 400)
	}
	unlock, err := e.lockContext(ctx, input.ID)
	if err != nil {
		return result, err
	}
	defer unlock()
	stop := context.AfterFunc(ctx, func() {
		if streams.Stdin != nil {
			_ = streams.Stdin.Close()
		}
		if streams.Stdout != nil {
			_ = streams.Stdout.Close()
		}
	})
	defer stop()
	instance, err := e.get(ctx, input.ID, false)
	if err != nil {
		return result, err
	}
	if err = softwareArtifactAllowed(instance); err != nil {
		return result, err
	}
	root, err := os.OpenRoot(instance.Paths.BackupDir)
	if err != nil {
		return result, err
	}
	defer root.Close()
	if input.Direction == "upload" {
		if streams.Stdout == nil {
			return result, failure("stream_required", "Artifact reader requires an output stream", 400)
		}
		file, err := root.OpenFile(input.Filename, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return result, err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxExtractedBytes {
			return result, failure("artifact_invalid", "Backup artifact must be a regular bounded file", 412)
		}
		written, copyErr := io.Copy(streams.Stdout, &contextReader{ctx: ctx, reader: file})
		err = copyErr
		result.StdoutBytes = written
		if err != nil {
			return result, err
		}
		result.ExitCode = 0
		return result, nil
	}
	if input.Direction != "download" || streams.Stdin == nil {
		return result, failure("stream_required", "Artifact writer requires an input stream", 400)
	}
	if _, err := root.Lstat(input.Filename); err == nil {
		return result, failure("artifact_exists", "Owned backup artifact already exists; replacement requires a fresh artifact ID", 409)
	} else if !os.IsNotExist(err) {
		return result, err
	}
	nonce, err := randomPlanID()
	if err != nil {
		return result, err
	}
	temp := ".incoming-" + nonce
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return result, failure("artifact_exists", "Owned artifact already exists or cannot be created", 409)
	}
	completed := false
	defer func() {
		file.Close()
		if !completed {
			_ = root.Remove(temp)
		}
	}()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(&contextReader{ctx: ctx, reader: streams.Stdin}, maxExtractedBytes+1))
	if err != nil || written > maxExtractedBytes || written != input.ExpectedBytes {
		return result, failure("artifact_transfer_failed", "Backup transfer failed or exceeded its bound", 413)
	}
	if err = file.Sync(); err != nil {
		return result, err
	}
	if err = file.Close(); err != nil {
		return result, err
	}
	if err = root.Rename(temp, input.Filename); err != nil {
		return result, err
	}
	result.Data, _ = json.Marshal(map[string]any{"filename": input.Filename, "bytes": written, "sha256": hex.EncodeToString(hash.Sum(nil))})
	completed = true
	result.ExitCode = 0
	return result, nil
}
func (e *PrivilegedExecutor) Stream(ctx context.Context, request agent.Request, streams agent.IO) (agent.Result, error) {
	if request.Operation == "database_import_artifact" {
		if err := e.Validate(request); err != nil {
			return agent.Result{}, err
		}
		return e.databaseImportArtifactStream(ctx, request, streams)
	}
	if request.Operation == "database_artifact" {
		if err := e.Validate(request); err != nil {
			return agent.Result{}, err
		}
		return e.databaseArtifactStream(ctx, request, streams)
	}
	if request.Operation != "backup_artifact" {
		return e.Run(ctx, request)
	}
	if err := e.Validate(request); err != nil {
		return agent.Result{}, err
	}
	return e.artifactStream(ctx, request, streams)
}
