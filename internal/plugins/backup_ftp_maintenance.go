package plugins

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/jobs"
	"github.com/jlaffaye/ftp"
)

const (
	ftpMaintenanceOperation       = "backup_ftp_maintenance_job"
	ftpMaintenancePreviewNS       = "ftp_maintenance_previews"
	ftpMaintenancePreviewTTL      = 5 * time.Minute
	ftpMaintenanceMaxPath         = 1024
	ftpMaintenanceMaxSegments     = 16
	ftpMaintenanceMaxRootSegments = 32
	ftpMaintenanceMaxEntries      = 256
	ftpMaintenanceMaxListing      = 1 << 20
)

var ftpMaintenancePreviewPattern = regexp.MustCompile(`^ftpv_[0-9a-f]{48}$`)

type FTPMaintenanceCreateInput struct {
	ID               string `json:"id"`
	Path             string `json:"path"`
	Name             string `json:"name"`
	ExpectedRevision int64  `json:"expected_revision"`
}

type FTPMaintenancePreviewInput struct {
	ID               string `json:"id"`
	Path             string `json:"path"`
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	ExpectedRevision int64  `json:"expected_revision"`
}

type FTPMaintenanceDeleteInput struct {
	ID               string `json:"id"`
	Path             string `json:"path"`
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	ExpectedRevision int64  `json:"expected_revision"`
	PreviewID        string `json:"preview_id"`
}

type ftpMaintenanceJobInput struct {
	Command          string `json:"command"`
	ID               string `json:"id"`
	Path             string `json:"path"`
	Name             string `json:"name"`
	Kind             string `json:"kind,omitempty"`
	ExpectedRevision int64  `json:"expected_revision"`
	PreviewID        string `json:"preview_id,omitempty"`
}

type ftpMaintenanceEntryIdentity struct {
	Kind       string    `json:"kind"`
	Size       uint64    `json:"size"`
	Modified   time.Time `json:"modified"`
	LinkTarget string    `json:"link_target,omitempty"`
}

type ftpMaintenancePreview struct {
	Version          int                         `json:"version"`
	ID               string                      `json:"id"`
	ActorID          string                      `json:"actor_id"`
	PreviewID        string                      `json:"preview_id"`
	Path             string                      `json:"path"`
	Name             string                      `json:"name"`
	Kind             string                      `json:"kind"`
	ExpectedRevision int64                       `json:"expected_revision"`
	Identity         ftpMaintenanceEntryIdentity `json:"identity"`
	CreatedAt        time.Time                   `json:"created_at"`
	ExpiresAt        time.Time                   `json:"expires_at"`
	Status           string                      `json:"status"`
	TaskID           string                      `json:"task_id,omitempty"`
	CompletedAt      time.Time                   `json:"completed_at,omitempty"`
}

type ftpMaintenanceLockEntry struct {
	gate chan struct{}
	refs int
}

var ftpMaintenanceLocks = struct {
	sync.Mutex
	entries map[string]*ftpMaintenanceLockEntry
}{entries: map[string]*ftpMaintenanceLockEntry{}}

func acquireFTPMaintenanceLock(ctx context.Context, id string) (func(), error) {
	ftpMaintenanceLocks.Lock()
	entry := ftpMaintenanceLocks.entries[id]
	if entry == nil {
		entry = &ftpMaintenanceLockEntry{gate: make(chan struct{}, 1)}
		entry.gate <- struct{}{}
		ftpMaintenanceLocks.entries[id] = entry
	}
	entry.refs++
	ftpMaintenanceLocks.Unlock()

	releaseRef := func() {
		ftpMaintenanceLocks.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(ftpMaintenanceLocks.entries, id)
		}
		ftpMaintenanceLocks.Unlock()
	}
	select {
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	case <-entry.gate:
		var once sync.Once
		return func() {
			once.Do(func() {
				entry.gate <- struct{}{}
				releaseRef()
			})
		}, nil
	}
}

func FTPMaintenanceJobOperations() []string { return []string{ftpMaintenanceOperation} }

// FTPMaintenanceExecutor is an API-host executor. It obtains the durable
// actor, current grant, destination revision and secret at task execution time.
type FTPMaintenanceExecutor struct{ manager *Manager }

func NewFTPMaintenanceExecutor(manager *Manager) *FTPMaintenanceExecutor {
	return &FTPMaintenanceExecutor{manager: manager}
}

func (e *FTPMaintenanceExecutor) Validate(request agent.Request) error {
	if e == nil || e.manager == nil || request.Operation != ftpMaintenanceOperation {
		return agent.ErrUnauthorized
	}
	var input ftpMaintenanceJobInput
	if err := parse(request.Input, &input); err != nil {
		return err
	}
	if err := validateFTPMaintenanceJob(input); err != nil {
		return err
	}
	if e.manager.deps.State == nil || e.manager.deps.Secrets == nil || e.manager.deps.ResolveActor == nil || e.manager.deps.Authorize == nil {
		return failure("agent_unavailable", "FTP maintenance requires the protected credential store and current identity service", 503)
	}
	return nil
}

func (e *FTPMaintenanceExecutor) Run(ctx context.Context, request agent.Request) (result agent.Result, returned error) {
	result = agent.Result{Operation: request.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	defer func() { result.FinishedAt = time.Now().UTC() }()
	if err := e.Validate(request); err != nil {
		return result, err
	}
	m := e.manager
	var input ftpMaintenanceJobInput
	_ = parse(request.Input, &input)
	actor, err := m.deps.ResolveActor(ctx, core.Actor{ID: request.ActorID})
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, failure("identity_unavailable", "The task identity is no longer available", 403)
	}
	resource := FTPResource(input.ID)
	if !m.deps.Authorize(actor, "backup:manage", resource) {
		return result, failure("forbidden", "The current identity no longer manages this FTP destination", 403)
	}
	unlock, err := acquireFTPMaintenanceLock(ctx, input.ID)
	if err != nil {
		return result, err
	}
	defer unlock()
	profile, err := m.ftpProfile(ctx, input.ID)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, failure("destination_unavailable", "FTP destination is unavailable", 404)
	}
	if profile.Revision != input.ExpectedRevision {
		return result, ftpMaintenanceRevisionConflict()
	}
	password, err := ftpMaintenancePassword(ctx, m, actor, profile)
	if err != nil {
		return result, err
	}
	connection, err := ftpDialMaintenance(ctx, profile, string(password))
	clear(password)
	if err != nil {
		return result, err
	}
	defer connection.Quit()
	if err = ctx.Err(); err != nil {
		return result, err
	}
	base, err := ftpMaintenanceConfiguredRoot(connection, profile.Directory)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, err
	}
	_, err = ftpMaintenanceEnterPath(connection, base, input.Path)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	switch input.Command {
	case "create_directory":
		entries, listErr := ftpMaintenanceList(connection)
		if listErr != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, listErr
		}
		if _, exists := ftpMaintenanceFind(entries, input.Name); exists {
			return result, failure("target_exists", "A remote entry already has that name", 412)
		}
		if err = ctx.Err(); err != nil {
			return result, err
		}
		if err = m.ftpMaintenanceCheckRevision(ctx, input.ID, input.ExpectedRevision); err != nil {
			return result, err
		}
		if err = connection.MakeDir(input.Name); err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, failure("ftp_create_failed", "FTP server did not create the requested directory", 502)
		}
		return ftpMaintenanceSuccess(result, map[string]any{"created": true, "path": ftpMaintenanceRelativeTarget(input.Path, input.Name), "kind": "directory", "revision": input.ExpectedRevision})
	case "delete":
		preview, err := m.claimFTPMaintenancePreview(ctx, actor, request.JobID, input)
		if err != nil {
			return result, err
		}
		entries, listErr := ftpMaintenanceList(connection)
		if listErr != nil {
			if ctx.Err() != nil {
				_ = m.finishFTPMaintenancePreview(ctx, preview, "canceled")
				return result, ctx.Err()
			}
			_ = m.finishFTPMaintenancePreview(ctx, preview, "failed")
			return result, listErr
		}
		entry, found := ftpMaintenanceFind(entries, input.Name)
		if !found || !ftpMaintenanceIdentityMatches(entry, preview.Identity) {
			_ = m.finishFTPMaintenancePreview(ctx, preview, "stale")
			return result, ftpMaintenancePreviewStale()
		}
		if input.Kind == "directory" {
			if entry.Type != ftp.EntryTypeFolder {
				_ = m.finishFTPMaintenancePreview(ctx, preview, "stale")
				return result, ftpMaintenancePreviewStale()
			}
			if err = connection.ChangeDir(input.Name); err != nil {
				_ = m.finishFTPMaintenancePreview(ctx, preview, "stale")
				return result, ftpMaintenancePreviewStale()
			}
			current, pwdErr := connection.CurrentDir()
			expected := path.Join(base, input.Path, input.Name)
			if pwdErr != nil || !ftpMaintenanceSameRemotePath(current, expected) {
				_ = m.finishFTPMaintenancePreview(ctx, preview, "stale")
				return result, ftpMaintenancePreviewStale()
			}
			contents, contentErr := ftpMaintenanceList(connection)
			if contentErr != nil {
				_ = m.finishFTPMaintenancePreview(ctx, preview, "failed")
				return result, contentErr
			}
			if ftpMaintenanceHasContent(contents) {
				_ = m.finishFTPMaintenancePreview(ctx, preview, "stale")
				return result, ftpMaintenancePreviewStale()
			}
			if err = connection.ChangeDirToParent(); err != nil {
				_ = m.finishFTPMaintenancePreview(ctx, preview, "failed")
				return result, failure("ftp_delete_failed", "FTP could not return to the checked parent directory", 502)
			}
		} else if entry.Type != ftp.EntryTypeFile {
			_ = m.finishFTPMaintenancePreview(ctx, preview, "stale")
			return result, ftpMaintenancePreviewStale()
		}
		if err = ctx.Err(); err != nil {
			_ = m.finishFTPMaintenancePreview(ctx, preview, "canceled")
			return result, err
		}
		if err = m.ftpMaintenanceCheckRevision(ctx, input.ID, input.ExpectedRevision); err != nil {
			_ = m.finishFTPMaintenancePreview(ctx, preview, "stale")
			return result, err
		}
		// FTP has no compare-and-delete command. The frozen listing and this
		// immediate recheck protect against stale UI state, but cannot close the
		// final race with an independent writer on the FTP server.
		if input.Kind == "file" {
			err = connection.Delete(input.Name)
		} else {
			err = connection.RemoveDir(input.Name)
		}
		if err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			_ = m.finishFTPMaintenancePreview(ctx, preview, "failed")
			return result, failure("ftp_delete_failed", "FTP did not confirm removal of the previewed target", 502)
		}
		if err = m.finishFTPMaintenancePreview(ctx, preview, "completed"); err != nil {
			return result, failure("operation_recovery_required", "FTP confirmed removal but its preview receipt could not be committed; inspect the remote destination", 409)
		}
		return ftpMaintenanceSuccess(result, map[string]any{"deleted": true, "path": ftpMaintenanceRelativeTarget(input.Path, input.Name), "kind": input.Kind, "revision": input.ExpectedRevision})
	default:
		return result, failure("invalid_input", "Unsupported FTP maintenance operation", 400)
	}
}

func validateFTPMaintenanceJob(in ftpMaintenanceJobInput) error {
	if !validInstanceID(in.ID) || in.ExpectedRevision < 1 || !ftpMaintenanceValidPath(in.Path) || !ftpMaintenanceValidName(in.Name) {
		return failure("invalid_input", "Invalid FTP maintenance target or destination revision", 400)
	}
	switch in.Command {
	case "create_directory":
		if in.Kind != "" || in.PreviewID != "" {
			return failure("invalid_input", "Directory creation does not accept a delete preview", 400)
		}
	case "delete":
		if (in.Kind != "file" && in.Kind != "directory") || !ftpMaintenancePreviewPattern.MatchString(in.PreviewID) {
			return failure("invalid_input", "Deletion requires a file or directory and a current preview", 400)
		}
	default:
		return failure("invalid_input", "Unsupported FTP maintenance command", 400)
	}
	return nil
}

func ftpMaintenanceValidPath(value string) bool {
	if len(value) > ftpMaintenanceMaxPath || strings.ContainsAny(value, "\\\x00\r\n") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") {
		return false
	}
	if value == "" {
		return true
	}
	if path.Clean(value) != value || value == "." {
		return false
	}
	parts := strings.Split(value, "/")
	if len(parts) > ftpMaintenanceMaxSegments {
		return false
	}
	for _, part := range parts {
		if !ftpMaintenanceValidName(part) {
			return false
		}
	}
	return true
}

func ftpMaintenanceValidName(value string) bool {
	return len(value) > 0 && len(value) <= 255 && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00\r\n")
}

func ftpMaintenanceRevisionConflict() error {
	return failure("revision_conflict", "FTP destination configuration changed; refresh it before continuing", 412)
}

func ftpMaintenancePreviewStale() error {
	return failure("preview_stale", "Remote target changed or is no longer an empty regular file or directory; create a fresh preview", 412)
}

func ftpMaintenanceContextError(ctx context.Context, fallback error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fallback
}

func ftpMaintenancePassword(ctx context.Context, manager *Manager, actor core.Actor, profile FTPDestination) ([]byte, error) {
	if profile.CredentialRef == "" {
		return nil, failure("credential_unavailable", "FTP destination credential is unavailable", 412)
	}
	secret, err := manager.deps.Secrets.Get(ctx, actor, FTPResource(profile.ID), profile.CredentialRef)
	if err != nil || len(secret) == 0 {
		clear(secret)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failure("credential_unavailable", "FTP destination credential is unavailable", 412)
	}
	return secret, nil
}

func ftpDialMaintenance(ctx context.Context, profile FTPDestination, password string) (*ftp.ServerConn, error) {
	if profile.Host == "" || strings.ContainsAny(profile.Host, "/\\\x00\r\n") || profile.Port < 1 || profile.Port > 65535 {
		return nil, failure("invalid_destination", "FTP host and port are invalid", 400)
	}
	options := []ftp.DialOption{ftp.DialWithContext(ctx), ftp.DialWithTimeout(15 * time.Second), ftp.DialWithShutTimeout(time.Second), ftp.DialWithTrustPasvIP(false), ftp.DialWithDisabledMLSD(true)}
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
		limit := int64(ftpMaintenanceMaxListing + 1)
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
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failure("ftp_unavailable", "FTP destination could not be reached with its required transport security", 502)
	}
	if err = connection.Login(profile.Username, password); err != nil {
		_ = connection.Quit()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failure("ftp_auth_failed", "FTP rejected the destination credentials", 401)
	}
	return connection, nil
}

func ftpMaintenanceConfiguredRoot(connection *ftp.ServerConn, configured string) (string, error) {
	if configured == "" || len(configured) > ftpMaintenanceMaxPath || strings.ContainsAny(configured, "\\\x00\r\n") || path.Clean(configured) != configured {
		return "", failure("destination_root_unsafe", "Configured FTP destination root is not a clean bounded path", 412)
	}
	absolute := path.IsAbs(configured)
	var current string
	var parts []string
	if absolute {
		if err := connection.ChangeDir("/"); err != nil {
			return "", failure("destination_root_unsafe", "Configured FTP destination root could not be opened safely", 412)
		}
		current = "/"
		parts = strings.Split(strings.TrimPrefix(configured, "/"), "/")
	} else {
		pwd, err := connection.CurrentDir()
		if err != nil || !ftpMaintenanceCleanAbsolute(pwd) {
			return "", failure("destination_root_unsafe", "FTP login directory could not be verified", 412)
		}
		current = path.Clean(pwd)
		parts = strings.Split(configured, "/")
	}
	if len(parts) == 1 && parts[0] == "" && absolute {
		return current, nil
	}
	if len(parts) > ftpMaintenanceMaxRootSegments {
		return "", failure("destination_root_unsafe", "Configured FTP root has too many path segments", 413)
	}
	for _, part := range parts {
		if part == "" {
			continue
		}
		if !ftpMaintenanceValidName(part) {
			return "", failure("destination_root_unsafe", "Configured FTP destination root contains an unsafe path segment", 412)
		}
		entries, err := ftpMaintenanceList(connection)
		if err != nil {
			return "", err
		}
		entry, found := ftpMaintenanceFind(entries, part)
		if !found || entry.Type != ftp.EntryTypeFolder {
			return "", failure("destination_root_unsafe", "Configured FTP destination root contains a missing directory or link", 412)
		}
		if err = connection.ChangeDir(part); err != nil {
			return "", failure("destination_root_unsafe", "Configured FTP destination root could not be opened safely", 412)
		}
		current = path.Join(current, part)
	}
	pwd, err := connection.CurrentDir()
	if err != nil || !ftpMaintenanceCleanAbsolute(pwd) || !ftpMaintenanceSameRemotePath(pwd, current) {
		return "", failure("destination_root_unsafe", "FTP server resolved the configured root to an unexpected path", 412)
	}
	return path.Clean(pwd), nil
}

func ftpMaintenanceEnterPath(connection *ftp.ServerConn, root, relative string) (string, error) {
	if !ftpMaintenanceValidPath(relative) {
		return "", failure("invalid_input", "FTP path must be relative to the configured root and contain no traversal", 400)
	}
	current := root
	if relative == "" {
		return current, nil
	}
	for _, part := range strings.Split(relative, "/") {
		entries, err := ftpMaintenanceList(connection)
		if err != nil {
			return "", err
		}
		entry, found := ftpMaintenanceFind(entries, part)
		if !found || entry.Type != ftp.EntryTypeFolder {
			return "", failure("path_unsafe", "FTP path contains a missing directory or symbolic link", 412)
		}
		if err = connection.ChangeDir(part); err != nil {
			return "", failure("path_unsafe", "FTP path could not be opened as a real directory", 412)
		}
		current = path.Join(current, part)
		pwd, pwdErr := connection.CurrentDir()
		if pwdErr != nil || !ftpMaintenanceCleanAbsolute(pwd) || !ftpMaintenanceSameRemotePath(pwd, current) {
			return "", failure("path_unsafe", "FTP path resolved outside its checked directory", 412)
		}
	}
	return current, nil
}

func ftpMaintenanceList(connection *ftp.ServerConn) ([]*ftp.Entry, error) {
	entries, err := connection.List(".")
	if err != nil {
		return nil, failure("ftp_list_failed", "FTP server could not provide a bounded directory listing", 502)
	}
	if len(entries) > ftpMaintenanceMaxEntries {
		return nil, failure("result_too_large", "FTP directory has more than 256 entries", 413)
	}
	bytes := 0
	for _, entry := range entries {
		if entry == nil || len(entry.Name) > 255 || (entry.Name != "." && entry.Name != ".." && !ftpMaintenanceValidName(entry.Name)) || strings.ContainsAny(entry.Target, "\x00\r\n") {
			return nil, failure("listing_unsafe", "FTP listing contains an unsupported or unsafe entry", 412)
		}
		bytes += len(entry.Name) + len(entry.Target) + 64
		if bytes > ftpMaintenanceMaxListing {
			return nil, failure("result_too_large", "FTP directory listing exceeds one MiB", 413)
		}
	}
	return entries, nil
}

func ftpMaintenanceFind(entries []*ftp.Entry, name string) (*ftp.Entry, bool) {
	var found *ftp.Entry
	for _, entry := range entries {
		if entry.Name == name {
			if found != nil {
				return nil, false
			}
			found = entry
		}
	}
	return found, found != nil
}

func ftpMaintenanceIdentity(entry *ftp.Entry) ftpMaintenanceEntryIdentity {
	kind := "unsupported"
	switch entry.Type {
	case ftp.EntryTypeFile:
		kind = "file"
	case ftp.EntryTypeFolder:
		kind = "directory"
	case ftp.EntryTypeLink:
		kind = "link"
	}
	return ftpMaintenanceEntryIdentity{Kind: kind, Size: entry.Size, Modified: entry.Time.UTC(), LinkTarget: entry.Target}
}

func ftpMaintenanceIdentityMatches(entry *ftp.Entry, identity ftpMaintenanceEntryIdentity) bool {
	current := ftpMaintenanceIdentity(entry)
	return current.Kind == identity.Kind && current.Size == identity.Size && current.Modified.Equal(identity.Modified) && current.LinkTarget == identity.LinkTarget
}

func ftpMaintenanceHasContent(entries []*ftp.Entry) bool {
	for _, entry := range entries {
		if entry.Name != "." && entry.Name != ".." {
			return true
		}
	}
	return false
}

func ftpMaintenanceCleanAbsolute(value string) bool {
	return strings.HasPrefix(value, "/") && len(value) <= ftpMaintenanceMaxPath && strings.ContainsAny(value, "\x00\r\n\\") == false && path.Clean(value) == value
}

func ftpMaintenanceSameRemotePath(actual, expected string) bool {
	return ftpMaintenanceCleanAbsolute(actual) && ftpMaintenanceCleanAbsolute(expected) && path.Clean(actual) == path.Clean(expected)
}

func ftpMaintenanceRelativeTarget(parent, name string) string {
	if parent == "" {
		return name
	}
	return path.Join(parent, name)
}

func (m *Manager) ftpMaintenanceBindings() []core.Binding {
	resources := func(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
		var input struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &input) != nil || !validInstanceID(input.ID) {
			return nil, failure("invalid_input", "A valid FTP destination ID is required", 400)
		}
		return []string{FTPResource(input.ID)}, nil
	}
	list := m.binding("backup_ftp_directory_list", "List bounded entries beneath the configured FTP backup root without following directory links", "backup:read", true, obj(map[string]any{
		"id": idSchema(), "path": map[string]any{"type": "string", "maxLength": ftpMaintenanceMaxPath}, "expected_revision": map[string]any{"type": "integer", "minimum": 0},
	}, "id", "path"), resources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		var in struct {
			ID               string `json:"id"`
			Path             string `json:"path"`
			ExpectedRevision int64  `json:"expected_revision"`
		}
		if err := parse(raw, &in); err != nil {
			return core.ActionResult{}, err
		}
		if !validInstanceID(in.ID) || !ftpMaintenanceValidPath(in.Path) || in.ExpectedRevision < 0 {
			return core.ActionResult{}, failure("invalid_input", "Invalid FTP directory or destination revision", 400)
		}
		unlock, err := acquireFTPMaintenanceLock(ctx, in.ID)
		if err != nil {
			return core.ActionResult{}, err
		}
		defer unlock()
		profile, err := m.ftpProfile(ctx, in.ID)
		if err != nil {
			if ctx.Err() != nil {
				return core.ActionResult{}, ctx.Err()
			}
			return core.ActionResult{}, failure("destination_unavailable", "FTP destination is unavailable", 404)
		}
		if in.ExpectedRevision > 0 && profile.Revision != in.ExpectedRevision {
			return core.ActionResult{}, ftpMaintenanceRevisionConflict()
		}
		password, err := ftpMaintenancePassword(ctx, m, actor, profile)
		if err != nil {
			return core.ActionResult{}, err
		}
		connection, err := ftpDialMaintenance(ctx, profile, string(password))
		clear(password)
		if err != nil {
			return core.ActionResult{}, err
		}
		defer connection.Quit()
		base, err := ftpMaintenanceConfiguredRoot(connection, profile.Directory)
		if err != nil {
			return core.ActionResult{}, ftpMaintenanceContextError(ctx, err)
		}
		if _, err = ftpMaintenanceEnterPath(connection, base, in.Path); err != nil {
			return core.ActionResult{}, ftpMaintenanceContextError(ctx, err)
		}
		entries, err := ftpMaintenanceList(connection)
		if err != nil {
			return core.ActionResult{}, ftpMaintenanceContextError(ctx, err)
		}
		items := make([]map[string]any, 0, len(entries))
		for _, entry := range entries {
			if entry.Name == "." || entry.Name == ".." {
				continue
			}
			identity := ftpMaintenanceIdentity(entry)
			items = append(items, map[string]any{"name": entry.Name, "kind": identity.Kind, "size": identity.Size, "modified": identity.Modified})
		}
		return core.ActionResult{Data: map[string]any{"id": in.ID, "revision": profile.Revision, "path": in.Path, "items": items}, Resources: []string{FTPResource(in.ID)}}, nil
	})
	create := m.binding("backup_ftp_directory_create", "Create one directory beneath a configured FTP backup destination", "backup:manage", false, obj(map[string]any{
		"id": idSchema(), "path": map[string]any{"type": "string", "maxLength": ftpMaintenanceMaxPath}, "name": map[string]any{"type": "string", "minLength": 1, "maxLength": 255}, "expected_revision": revisionSchema(),
	}, "id", "path", "name", "expected_revision"), resources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		var in FTPMaintenanceCreateInput
		if err := parse(raw, &in); err != nil {
			return core.ActionResult{}, err
		}
		jobInput := ftpMaintenanceJobInput{Command: "create_directory", ID: in.ID, Path: in.Path, Name: in.Name, ExpectedRevision: in.ExpectedRevision}
		if err := validateFTPMaintenanceJob(jobInput); err != nil {
			return core.ActionResult{}, err
		}
		if err := m.ftpMaintenanceCheckRevision(ctx, in.ID, in.ExpectedRevision); err != nil {
			return core.ActionResult{}, err
		}
		encoded, _ := json.Marshal(jobInput)
		resource := FTPResource(in.ID)
		task, err := m.deps.Tasks.Submit(ctx, actor, ftpMaintenanceOperation, encoded, []string{resource})
		if err != nil {
			return core.ActionResult{}, err
		}
		taskID := ftpMaintenanceTaskID(task)
		return core.ActionResult{Data: task, TaskID: taskID, Resources: []string{resource}, AuditSummary: map[string]any{"id": in.ID, "operation": "create_directory", "path": ftpMaintenanceRelativeTarget(in.Path, in.Name)}}, nil
	})

	preview := m.binding("backup_ftp_delete_preview", "Preview one regular file or empty directory beneath a configured FTP backup destination", "backup:manage", false, obj(map[string]any{
		"id": idSchema(), "path": map[string]any{"type": "string", "maxLength": ftpMaintenanceMaxPath}, "name": map[string]any{"type": "string", "minLength": 1, "maxLength": 255}, "kind": enumSchema("file", "directory"), "expected_revision": revisionSchema(),
	}, "id", "path", "name", "kind", "expected_revision"), resources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		var in FTPMaintenancePreviewInput
		if err := parse(raw, &in); err != nil {
			return core.ActionResult{}, err
		}
		jobInput := ftpMaintenanceJobInput{Command: "delete", ID: in.ID, Path: in.Path, Name: in.Name, Kind: in.Kind, ExpectedRevision: in.ExpectedRevision, PreviewID: "ftpv_" + strings.Repeat("0", 48)}
		if err := validateFTPMaintenanceJob(jobInput); err != nil {
			return core.ActionResult{}, err
		}
		if err := m.ftpMaintenanceCheckRevision(ctx, in.ID, in.ExpectedRevision); err != nil {
			return core.ActionResult{}, err
		}
		unlock, err := acquireFTPMaintenanceLock(ctx, in.ID)
		if err != nil {
			return core.ActionResult{}, err
		}
		defer unlock()
		profile, err := m.ftpProfile(ctx, in.ID)
		if err != nil || profile.Revision != in.ExpectedRevision {
			if err != nil {
				if ctx.Err() != nil {
					return core.ActionResult{}, ctx.Err()
				}
				return core.ActionResult{}, failure("destination_unavailable", "FTP destination is unavailable", 404)
			}
			return core.ActionResult{}, ftpMaintenanceRevisionConflict()
		}
		password, err := ftpMaintenancePassword(ctx, m, actor, profile)
		if err != nil {
			return core.ActionResult{}, err
		}
		connection, err := ftpDialMaintenance(ctx, profile, string(password))
		clear(password)
		if err != nil {
			return core.ActionResult{}, err
		}
		defer connection.Quit()
		base, err := ftpMaintenanceConfiguredRoot(connection, profile.Directory)
		if err != nil {
			return core.ActionResult{}, ftpMaintenanceContextError(ctx, err)
		}
		if _, err = ftpMaintenanceEnterPath(connection, base, in.Path); err != nil {
			return core.ActionResult{}, ftpMaintenanceContextError(ctx, err)
		}
		entries, err := ftpMaintenanceList(connection)
		if err != nil {
			return core.ActionResult{}, ftpMaintenanceContextError(ctx, err)
		}
		entry, found := ftpMaintenanceFind(entries, in.Name)
		if !found {
			return core.ActionResult{}, failure("not_found", "The requested FTP entry does not exist", 404)
		}
		identity := ftpMaintenanceIdentity(entry)
		if identity.Kind != in.Kind || identity.Kind == "link" || identity.Kind == "unsupported" {
			return core.ActionResult{}, failure("target_type_mismatch", "The requested FTP entry is not the selected regular file or directory", 412)
		}
		if in.Kind == "directory" {
			if err = connection.ChangeDir(in.Name); err != nil {
				return core.ActionResult{}, ftpMaintenanceContextError(ctx, failure("path_unsafe", "FTP directory could not be opened as a real directory", 412))
			}
			current, pwdErr := connection.CurrentDir()
			expected := path.Join(base, in.Path, in.Name)
			if pwdErr != nil || !ftpMaintenanceSameRemotePath(current, expected) {
				return core.ActionResult{}, ftpMaintenanceContextError(ctx, failure("path_unsafe", "FTP directory resolved outside the checked parent", 412))
			}
			contents, contentErr := ftpMaintenanceList(connection)
			if contentErr != nil {
				return core.ActionResult{}, ftpMaintenanceContextError(ctx, contentErr)
			}
			if ftpMaintenanceHasContent(contents) {
				return core.ActionResult{}, failure("directory_not_empty", "Only empty FTP directories can be deleted", 412)
			}
		}
		if err = ctx.Err(); err != nil {
			return core.ActionResult{}, err
		}
		if err = m.pruneFTPMaintenancePreviews(ctx); err != nil {
			return core.ActionResult{}, err
		}
		previewID, err := newFTPMaintenancePreviewID()
		if err != nil {
			return core.ActionResult{}, failure("internal", "A safe delete preview could not be created", 500)
		}
		now := time.Now().UTC()
		stored := ftpMaintenancePreview{Version: 1, ID: in.ID, ActorID: actor.ID, PreviewID: previewID, Path: in.Path, Name: in.Name, Kind: in.Kind, ExpectedRevision: in.ExpectedRevision, Identity: identity, CreatedAt: now, ExpiresAt: now.Add(ftpMaintenancePreviewTTL), Status: "ready"}
		encoded, _ := json.Marshal(stored)
		if _, err = m.deps.State.CompareAndSwap(ctx, ftpMaintenancePreviewNS, previewID, 0, encoded); err != nil {
			if ctx.Err() != nil {
				return core.ActionResult{}, ctx.Err()
			}
			return core.ActionResult{}, failure("internal", "A safe delete preview could not be stored", 500)
		}
		result := map[string]any{"id": in.ID, "revision": in.ExpectedRevision, "preview_id": previewID, "expires_at": stored.ExpiresAt, "candidate": map[string]any{"path": ftpMaintenanceRelativeTarget(in.Path, in.Name), "name": in.Name, "kind": in.Kind, "size": identity.Size, "modified": identity.Modified}}
		return core.ActionResult{Data: result, Resources: []string{FTPResource(in.ID)}, AuditSummary: map[string]any{"id": in.ID, "path": ftpMaintenanceRelativeTarget(in.Path, in.Name), "kind": in.Kind}}, nil
	})

	remove := m.binding("backup_ftp_delete", "Delete one previewed regular FTP file or empty directory beneath the configured root", "backup:manage", false, obj(map[string]any{
		"id": idSchema(), "path": map[string]any{"type": "string", "maxLength": ftpMaintenanceMaxPath}, "name": map[string]any{"type": "string", "minLength": 1, "maxLength": 255}, "kind": enumSchema("file", "directory"), "expected_revision": revisionSchema(), "preview_id": map[string]any{"type": "string", "pattern": ftpMaintenancePreviewPattern.String(), "maxLength": 53},
	}, "id", "path", "name", "kind", "expected_revision", "preview_id"), resources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		var in FTPMaintenanceDeleteInput
		if err := parse(raw, &in); err != nil {
			return core.ActionResult{}, err
		}
		jobInput := ftpMaintenanceJobInput{Command: "delete", ID: in.ID, Path: in.Path, Name: in.Name, Kind: in.Kind, ExpectedRevision: in.ExpectedRevision, PreviewID: in.PreviewID}
		if err := validateFTPMaintenanceJob(jobInput); err != nil {
			return core.ActionResult{}, err
		}
		if err := m.ftpMaintenanceCheckRevision(ctx, in.ID, in.ExpectedRevision); err != nil {
			return core.ActionResult{}, err
		}
		encoded, _ := json.Marshal(jobInput)
		resource := FTPResource(in.ID)
		task, err := m.deps.Tasks.SubmitPrepared(ctx, actor, ftpMaintenanceOperation, encoded, core.TaskMetadata{Resources: []string{resource}}, func(store core.MetadataStore, job jobs.Job) error {
			return ftpMaintenanceQueuePreview(ctx, store, actor.ID, job.ID, jobInput)
		})
		if err != nil {
			return core.ActionResult{}, err
		}
		taskID := ftpMaintenanceTaskID(task)
		return core.ActionResult{Data: task, TaskID: taskID, Resources: []string{resource}, AuditSummary: map[string]any{"id": in.ID, "operation": "delete", "path": ftpMaintenanceRelativeTarget(in.Path, in.Name), "kind": in.Kind}}, nil
	})
	preview.Action.Audit = true
	create.Action.Task = true
	remove.Action.Task = true
	return []core.Binding{list, create, preview, remove}
}

func (m *Manager) ftpMaintenanceCheckRevision(ctx context.Context, id string, expected int64) error {
	profile, err := m.ftpProfile(ctx, id)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return failure("destination_unavailable", "FTP destination is unavailable", 404)
	}
	if profile.Revision != expected {
		return ftpMaintenanceRevisionConflict()
	}
	return nil
}

func ftpMaintenanceTaskID(task map[string]any) string {
	if id, _ := task["task_id"].(string); id != "" {
		return id
	}
	id, _ := task["id"].(string)
	return id
}

func newFTPMaintenancePreviewID() (string, error) {
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return "ftpv_" + hex.EncodeToString(token[:]), nil
}

func (m *Manager) pruneFTPMaintenancePreviews(ctx context.Context) error {
	rows, err := m.deps.State.List(ctx, ftpMaintenancePreviewNS)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return failure("metadata_unavailable", "FTP preview metadata could not be inspected", 503)
	}
	active := 0
	for id, raw := range rows {
		var preview ftpMaintenancePreview
		if json.Unmarshal(raw, &preview) != nil || preview.Version != 1 || time.Now().After(preview.ExpiresAt) {
			if err = m.deps.State.Delete(ctx, ftpMaintenancePreviewNS, id); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return failure("metadata_unavailable", "Expired FTP previews could not be removed", 503)
			}
			continue
		}
		if preview.Status == "ready" || preview.Status == "queued" || preview.Status == "applying" {
			active++
		}
	}
	if active >= 1024 {
		return failure("preview_capacity", "Too many unexpired FTP delete previews exist", 429)
	}
	return nil
}

func ftpMaintenanceQueuePreview(ctx context.Context, store core.MetadataStore, actorID, taskID string, input ftpMaintenanceJobInput) error {
	raw, revision, err := store.GetVersioned(ctx, ftpMaintenancePreviewNS, input.PreviewID)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return failure("preview_expired", "Delete requires an unexpired FTP preview", 412)
	}
	var preview ftpMaintenancePreview
	if json.Unmarshal(raw, &preview) != nil || preview.Version != 1 || preview.PreviewID != input.PreviewID || preview.ID != input.ID || preview.ActorID != actorID || preview.Path != input.Path || preview.Name != input.Name || preview.Kind != input.Kind || preview.ExpectedRevision != input.ExpectedRevision {
		return failure("preview_stale", "FTP delete request does not match the frozen preview", 412)
	}
	if time.Now().After(preview.ExpiresAt) {
		return failure("preview_expired", "Delete requires an unexpired FTP preview", 412)
	}
	if preview.Status != "ready" {
		return failure("preview_used", "This FTP delete preview is already queued or consumed", 409)
	}
	preview.Status = "queued"
	preview.TaskID = taskID
	encoded, _ := json.Marshal(preview)
	if _, err = store.CompareAndSwap(ctx, ftpMaintenancePreviewNS, input.PreviewID, revision, encoded); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return failure("preview_used", "This FTP delete preview was queued by another request", 409)
	}
	return nil
}

func (m *Manager) claimFTPMaintenancePreview(ctx context.Context, actor core.Actor, taskID string, input ftpMaintenanceJobInput) (ftpMaintenancePreview, error) {
	var preview ftpMaintenancePreview
	raw, revision, err := m.deps.State.GetVersioned(ctx, ftpMaintenancePreviewNS, input.PreviewID)
	if err != nil || json.Unmarshal(raw, &preview) != nil {
		return preview, failure("preview_expired", "Delete requires an unexpired FTP preview", 412)
	}
	if preview.Version != 1 || preview.PreviewID != input.PreviewID || preview.ID != input.ID || preview.ActorID != actor.ID || preview.Path != input.Path || preview.Name != input.Name || preview.Kind != input.Kind || preview.ExpectedRevision != input.ExpectedRevision || preview.TaskID != taskID {
		return preview, failure("preview_stale", "FTP delete task does not match its frozen preview", 412)
	}
	if time.Now().After(preview.ExpiresAt) {
		return preview, failure("preview_expired", "Delete requires an unexpired FTP preview", 412)
	}
	if preview.Status != "queued" {
		return preview, failure("preview_used", "This FTP delete preview is already consumed", 409)
	}
	preview.Status = "applying"
	encoded, _ := json.Marshal(preview)
	if _, err = m.deps.State.CompareAndSwap(ctx, ftpMaintenancePreviewNS, input.PreviewID, revision, encoded); err != nil {
		return preview, failure("preview_used", "This FTP delete preview was claimed by another task", 409)
	}
	return preview, nil
}

func (m *Manager) finishFTPMaintenancePreview(ctx context.Context, preview ftpMaintenancePreview, status string) error {
	finishCtx := ctx
	var cancel context.CancelFunc
	if ctx.Err() != nil {
		finishCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
	}
	raw, revision, err := m.deps.State.GetVersioned(finishCtx, ftpMaintenancePreviewNS, preview.PreviewID)
	if err != nil {
		return err
	}
	var current ftpMaintenancePreview
	if json.Unmarshal(raw, &current) != nil || current.TaskID != preview.TaskID || current.Status != "applying" {
		return errors.New("FTP delete preview state changed")
	}
	current.Status = status
	if status == "completed" {
		current.CompletedAt = time.Now().UTC()
	}
	encoded, _ := json.Marshal(current)
	_, err = m.deps.State.CompareAndSwap(finishCtx, ftpMaintenancePreviewNS, preview.PreviewID, revision, encoded)
	return err
}

func ftpMaintenanceSuccess(result agent.Result, value any) (agent.Result, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return result, failure("internal", "FTP maintenance result could not be encoded", 500)
	}
	result.ExitCode = 0
	result.Stdout = string(encoded)
	result.StdoutBytes = int64(len(encoded))
	return result, nil
}

var _ jobs.Executor = (*FTPMaintenanceExecutor)(nil)
