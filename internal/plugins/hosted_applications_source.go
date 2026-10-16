package plugins

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/httpapi"
)

const (
	maxHostedAppArchiveBytes    = int64(1 << 30)
	maxHostedAppMCPArchiveBytes = 512 << 10
	maxHostedAppArchiveEntries  = 50000
)

type hostedAppSourceUploadInput struct {
	ID               string `json:"id"`
	ExpectedRevision int64  `json:"expected_revision"`
	ExpectedBytes    int64  `json:"expected_bytes"`
	ExpectedSHA256   string `json:"expected_sha256"`
	ArchiveBase64    string `json:"archive_base64,omitempty"`
}

type hostedAppSourceUploadBodyKey struct{}
type hostedAppSourceUploadBody struct{ io.ReadCloser }

func hostedAppSourceUploadSchema() map[string]any {
	return obj(map[string]any{
		"id":                idSchema(),
		"expected_revision": revisionSchema(),
		"expected_bytes":    map[string]any{"type": "integer", "minimum": 1, "maximum": maxHostedAppArchiveBytes},
		"expected_sha256":   map[string]any{"type": "string", "pattern": "^[a-fA-F0-9]{64}$"},
		"archive_base64":    map[string]any{"type": "string", "maxLength": 699052, "contentEncoding": "base64"},
	}, "id", "expected_revision", "expected_bytes", "expected_sha256")
}

func validHostedAppSourceInput(raw json.RawMessage) error {
	var input hostedAppSourceUploadInput
	if err := parse(raw, &input); err != nil {
		return err
	}
	if !hostedAppValidID(input.ID) || input.ExpectedRevision < 1 || input.ExpectedBytes < 1 || input.ExpectedBytes > maxHostedAppArchiveBytes || !validHostedAppSHA256(input.ExpectedSHA256) {
		return failure("invalid_input", "Application source archive descriptor is invalid or outside its size bound", 400)
	}
	if input.ArchiveBase64 != "" {
		archive, err := base64.StdEncoding.DecodeString(input.ArchiveBase64)
		if err != nil || len(archive) == 0 || len(archive) > maxHostedAppMCPArchiveBytes || int64(len(archive)) != input.ExpectedBytes {
			return failure("invalid_input", "MCP application source archive must be valid base64 no larger than 512 KiB and match its declared size", 400)
		}
		digest := sha256.Sum256(archive)
		if !strings.EqualFold(hex.EncodeToString(digest[:]), input.ExpectedSHA256) {
			return failure("invalid_input", "MCP application source archive digest does not match its descriptor", 400)
		}
	}
	return nil
}

func validHostedAppSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func (m *Manager) streamHostedAppSource(ctx context.Context, prior core.Actor, input hostedAppSourceUploadInput) (HostedApplication, error) {
	var output HostedApplication
	actor, err := m.deps.ResolveActor(ctx, prior)
	if err != nil {
		return output, err
	}
	resource := hostedAppResource(input.ID)
	if !m.deps.Authorize(actor, "hosted_apps:manage", resource) {
		return output, failure("forbidden", "Application source is outside the authorized resources", 403)
	}
	body, _ := ctx.Value(hostedAppSourceUploadBodyKey{}).(*hostedAppSourceUploadBody)
	if body == nil || body.ReadCloser == nil {
		archive, decodeErr := base64.StdEncoding.DecodeString(input.ArchiveBase64)
		if decodeErr != nil || len(archive) == 0 || int64(len(archive)) != input.ExpectedBytes {
			return output, failure("stream_required", "Upload the source through the REST streaming endpoint or provide a valid archive_base64 value up to 512 KiB", 400)
		}
		body = &hostedAppSourceUploadBody{ReadCloser: io.NopCloser(bytes.NewReader(archive))}
	}
	defer body.Close()
	streamer, ok := m.executor.(interface {
		Stream(context.Context, agent.Request, agent.IO) (agent.Result, error)
	})
	if !ok {
		return output, failure("runtime_unavailable", "Controlled Agent streaming is required for application source imports", 412)
	}
	input.ArchiveBase64 = ""
	raw, _ := json.Marshal(input)
	result, err := streamer.Stream(ctx, agent.Request{Operation: "hosted_app_source_upload", Input: raw, ActorID: actor.ID}, agent.IO{Stdin: body})
	if err != nil {
		return output, err
	}
	if result.ExitCode != 0 || len(result.Data) == 0 || json.Unmarshal(result.Data, &output) != nil {
		return HostedApplication{}, failure("agent_protocol_error", "Application source import receipt was invalid", 502)
	}
	if output.ID != input.ID || output.Revision != input.ExpectedRevision+1 || output.Status != "created" || !strings.EqualFold(output.SourceSHA256, input.ExpectedSHA256) {
		return HostedApplication{}, failure("ownership_invalid", "Application source receipt does not match the authorized upload", 412)
	}
	return output, nil
}

type hostedAppSourceResourceHandler struct {
	service *core.Service
	slots   chan struct{}
}

func (m *Manager) HostedAppSourceResourceHandler(service *core.Service) httpapi.ResourceHandler {
	return &hostedAppSourceResourceHandler{service: service, slots: make(chan struct{}, 2)}
}

func (h *hostedAppSourceResourceHandler) HandleResource(w http.ResponseWriter, r *http.Request, actor core.Actor) bool {
	if r.URL.Path != "/api/v1/applications/source/import" {
		return false
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		hostedAppHTTPError(w, failure("method_not_allowed", "Method not allowed", 405))
		return true
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		hostedAppHTTPError(w, failure("rate_limited", "Application source upload slots are busy", 429))
		return true
	}
	query := r.URL.Query()
	if len(query) != 2 || len(query["id"]) != 1 || len(query["expected_revision"]) != 1 || len(r.URL.RawQuery) > 512 {
		hostedAppHTTPError(w, failure("invalid_input", "One application ID and expected revision are required", 400))
		return true
	}
	revision, err := parseRevision(query.Get("expected_revision"))
	if err != nil || !hostedAppValidID(query.Get("id")) {
		hostedAppHTTPError(w, failure("invalid_input", "Invalid application source upload target", 400))
		return true
	}
	resource := hostedAppResource(query.Get("id"))
	if !h.service.Authorized(actor, "hosted_apps:manage", resource) {
		hostedAppHTTPError(w, failure("forbidden", "Application source is outside the authorized resources", 403))
		return true
	}
	mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/octet-stream" || len(r.Header.Values("X-Content-SHA256")) != 1 || !validHostedAppSHA256(r.Header.Get("X-Content-SHA256")) {
		hostedAppHTTPError(w, failure("invalid_input", "Content-Type and one SHA-256 digest header are required", 400))
		return true
	}
	expectedBytes := r.ContentLength
	if expectedBytes < 1 || expectedBytes > maxHostedAppArchiveBytes {
		hostedAppHTTPError(w, failure("invalid_input", "Upload requires a known size between 1 byte and 1 GiB", 413))
		return true
	}
	input := hostedAppSourceUploadInput{ID: query.Get("id"), ExpectedRevision: revision, ExpectedBytes: expectedBytes, ExpectedSHA256: strings.ToLower(r.Header.Get("X-Content-SHA256"))}
	raw, _ := json.Marshal(input)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	body := &hostedAppSourceUploadBody{ReadCloser: http.MaxBytesReader(w, r.Body, expectedBytes)}
	defer body.Close()
	lease := &hostedAppSourceUploadLease{r: r, service: h.service, resource: resource, body: body, cancel: cancel}
	ctx = context.WithValue(ctx, hostedAppSourceUploadBodyKey{}, body)
	go lease.watch(ctx)
	data, err := h.service.Execute(ctx, actor, "hosted_apps_source_upload", raw)
	if err != nil {
		if leaseErr := lease.err.Load(); leaseErr != nil {
			err = leaseErr
		}
		hostedAppHTTPError(w, err)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(data)
	return true
}

func parseRevision(value string) (int64, error) {
	if value == "" || len(value) > 19 {
		return 0, errors.New("invalid revision")
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, err
	}
	if revision < 1 {
		return 0, errors.New("invalid revision")
	}
	return revision, nil
}

type hostedAppSourceUploadLease struct {
	r        *http.Request
	service  *core.Service
	resource string
	body     io.Closer
	cancel   context.CancelFunc
	err      atomic.Pointer[core.Error]
}

func (l *hostedAppSourceUploadLease) watch(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			actor, err := httpapi.RevalidateActor(l.r)
			if err == nil && !l.service.Authorized(actor, "hosted_apps:manage", l.resource) {
				err = failure("forbidden", "Application source upload authorization changed", 403)
			}
			if err != nil {
				var domain *core.Error
				if !errors.As(err, &domain) {
					domain = &core.Error{Code: "unauthorized", Message: "Application source upload lease is invalid", Status: 401}
				}
				l.err.Store(domain)
				l.cancel()
				_ = l.body.Close()
				return
			}
		}
	}
}

func hostedAppHTTPError(w http.ResponseWriter, err error) {
	var domain *core.Error
	if !errors.As(err, &domain) {
		domain = &core.Error{Code: "internal_error", Message: "Application source import could not be completed", Status: 500}
	}
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(domain.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": domain})
}

func restoreParkedHostedAppSource(previous, project string, uid, gid uint32) error {
	if err := os.Rename(previous, project); err != nil {
		return err
	}
	if err := os.Chown(project, int(uid), int(gid)); err != nil {
		return err
	}
	return os.Chmod(project, 0750)
}

type hostedArchiveHashReader struct {
	reader io.Reader
	hash   hash.Hash
	bytes  int64
}

func (r *hostedArchiveHashReader) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	if n > 0 {
		_, _ = r.hash.Write(data[:n])
		r.bytes += int64(n)
	}
	return n, err
}

type hostedArchiveDecodedReader struct {
	reader io.Reader
	bytes  int64
}

func (r *hostedArchiveDecodedReader) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	if n > 0 {
		r.bytes += int64(n)
		if r.bytes > maxExtractedBytes {
			return n, failure("application_archive_too_large", "Expanded application source exceeds its 4 GiB bound", 413)
		}
	}
	return n, err
}

func extractHostedAppArchive(ctx context.Context, destination, runtime string, compressed io.Reader) (int64, error) {
	gz, err := gzip.NewReader(&contextReader{ctx: ctx, reader: compressed})
	if err != nil {
		return 0, failure("application_archive_invalid", "Application source must be a valid gzip-compressed tar archive", 400)
	}
	decoded := &hostedArchiveDecodedReader{reader: gz}
	archive := tar.NewReader(decoded)
	seen := map[string]struct{}{}
	var total int64
	entries := 0
	for {
		if err = ctx.Err(); err != nil {
			return total, err
		}
		header, nextErr := archive.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return total, failure("application_archive_invalid", "Application source archive is truncated or malformed", 400)
		}
		name, nameErr := hostedArchivePath(header.Name, header.Typeflag == tar.TypeDir)
		if nameErr != nil {
			return total, nameErr
		}
		if name == "." {
			continue
		}
		entries++
		if entries > maxHostedAppArchiveEntries {
			return total, failure("application_archive_too_many_entries", "Application source archive has too many entries", 413)
		}
		if _, exists := seen[name]; exists {
			return total, failure("application_archive_duplicate_path", "Application source archive contains duplicate paths", 400)
		}
		seen[name] = struct{}{}
		target := filepath.Join(destination, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err = os.MkdirAll(target, 0750); err != nil {
				return total, err
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > maxExtractedBytes || total+header.Size > maxExtractedBytes {
				return total, failure("application_archive_too_large", "Expanded application source exceeds its 4 GiB bound", 413)
			}
			if err = os.MkdirAll(filepath.Dir(target), 0750); err != nil {
				return total, err
			}
			file, openErr := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
			if openErr != nil {
				return total, failure("application_archive_duplicate_path", "Application source archive paths collide", 400)
			}
			written, copyErr := io.CopyN(file, archive, header.Size)
			if copyErr == nil && written != header.Size {
				copyErr = io.ErrUnexpectedEOF
			}
			if copyErr == nil {
				copyErr = file.Sync()
			}
			if closeErr := file.Close(); copyErr == nil {
				copyErr = closeErr
			}
			if copyErr != nil {
				return total, failure("application_archive_invalid", "Application source archive contains an incomplete file", 400)
			}
			total += written
			mode := os.FileMode(0640)
			if header.Mode&0111 != 0 {
				mode = 0750
			}
			if err = os.Chmod(target, mode); err != nil {
				return total, err
			}
		default:
			return total, failure("application_archive_unsafe_entry", "Application source archives may contain only regular files and directories", 400)
		}
	}
	buf := make([]byte, 32<<10)
	for {
		n, readErr := decoded.Read(buf)
		for _, value := range buf[:n] {
			if value != 0 {
				return total, failure("application_archive_invalid", "Application source archive contains data after its tar end marker", 400)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return total, failure("application_archive_invalid", "Application source archive failed its gzip integrity check", 400)
		}
	}
	if err = gz.Close(); err != nil {
		return total, failure("application_archive_invalid", "Application source archive failed its gzip integrity check", 400)
	}
	manifest := "package.json"
	if runtime == "go" {
		manifest = "go.mod"
	}
	info, statErr := os.Lstat(filepath.Join(destination, manifest))
	if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return total, failure("application_manifest_required", "Application archive must contain a root-level "+manifest+" file", 400)
	}
	return total, nil
}

func hostedArchivePath(name string, directory bool) (string, error) {
	if len(name) == 0 || len(name) > 4096 || strings.ContainsRune(name, '\x00') || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") {
		return "", failure("application_archive_unsafe_path", "Application source archive contains an unsafe path", 400)
	}
	if name == "./" || name == "." {
		if directory {
			return ".", nil
		}
		return "", failure("application_archive_unsafe_path", "Application source archive contains an unsafe file path", 400)
	}
	name = strings.TrimPrefix(name, "./")
	if directory {
		name = strings.TrimSuffix(name, "/")
	}
	clean := path.Clean(name)
	if name == "" || clean != name || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || !filepath.IsLocal(filepath.FromSlash(clean)) {
		return "", failure("application_archive_unsafe_path", "Application source archive contains a path traversal entry", 400)
	}
	if !strings.Contains(clean, "/") && (clean == ".home" || clean == ".next" || strings.HasPrefix(clean, ".next.") || strings.HasPrefix(clean, ".powerlinux-app")) {
		return "", failure("application_archive_reserved_path", "Application source archive contains a reserved PowerLinux runtime path", 400)
	}
	return clean, nil
}

func (e *PrivilegedExecutor) hostedAppSourceUpload(ctx context.Context, request agent.Request, streams agent.IO) (result agent.Result, returned error) {
	result = agent.Result{Operation: request.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	defer func() { result.FinishedAt = time.Now().UTC() }()
	if err := validHostedAppSourceInput(request.Input); err != nil {
		return result, err
	}
	if streams.Stdin == nil {
		return result, failure("stream_required", "Application source import requires an input stream", 400)
	}
	var input hostedAppSourceUploadInput
	_ = json.Unmarshal(request.Input, &input)
	unlock, err := e.lockContext(ctx, "application-"+input.ID)
	if err != nil {
		return result, err
	}
	defer unlock()
	stop := context.AfterFunc(ctx, func() { _ = streams.Stdin.Close() })
	defer stop()
	receipt, err := e.hostedAppReceipt(input.ID)
	if err != nil {
		return result, err
	}
	if receipt.Status == "retained" || receipt.Revision != input.ExpectedRevision {
		return result, failure("revision_conflict", "Application changed or was retained; reload before importing source", 412)
	}
	if running, stateErr := e.hostedAppRunning(ctx, receipt, true); stateErr != nil {
		return result, stateErr
	} else if running {
		return result, failure("application_running", "Stop the application before importing new source", 412)
	}
	stageID, err := randomPlanID()
	if err != nil {
		return result, err
	}
	stage := filepath.Join(receipt.Root, ".source-import-"+strings.TrimPrefix(stageID, "plan_"))
	if err = os.Mkdir(stage, 0700); err != nil {
		return result, err
	}
	stageExists := true
	defer func() {
		if stageExists {
			_ = os.RemoveAll(stage)
		}
	}()
	inputStream := &hostedArchiveHashReader{reader: io.LimitReader(&contextReader{ctx: ctx, reader: streams.Stdin}, input.ExpectedBytes+1), hash: sha256.New()}
	_, err = extractHostedAppArchive(ctx, stage, receipt.Runtime, inputStream)
	if err != nil {
		return result, err
	}
	if inputStream.bytes != input.ExpectedBytes || !strings.EqualFold(hex.EncodeToString(inputStream.hash.Sum(nil)), input.ExpectedSHA256) {
		return result, failure("application_archive_integrity_failed", "Uploaded application source size or SHA-256 digest does not match its descriptor", 400)
	}
	if err = filepath.WalkDir(stage, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == stage {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || (!entry.IsDir() && !entry.Type().IsRegular()) {
			return failure("application_archive_unsafe_entry", "Application source tree contains a non-regular entry", 400)
		}
		if entry.IsDir() {
			if err := os.Chown(name, int(receipt.UID), int(receipt.GID)); err != nil {
				return err
			}
			return os.Chmod(name, 0750)
		}
		if err := os.Chown(name, int(receipt.UID), int(receipt.GID)); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return result, err
	}
	home := filepath.Join(stage, ".home")
	if info, statErr := os.Lstat(home); statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return result, failure("application_archive_unsafe_entry", "Source archive cannot replace the protected application home directory", 400)
		}
		return result, failure("application_archive_reserved_path", "Source archive cannot include the reserved .home directory", 400)
	} else if !os.IsNotExist(statErr) {
		return result, statErr
	}
	if err = os.Mkdir(home, 0700); err != nil {
		return result, err
	}
	if err = os.Chown(home, int(receipt.UID), int(receipt.GID)); err != nil {
		return result, err
	}
	previous := hostedAppPreviousProject(receipt)
	if _, statErr := os.Lstat(previous); statErr == nil {
		if err = validateParkedHostedAppSource(previous); err != nil {
			return result, err
		}
		if err = os.RemoveAll(previous); err != nil {
			return result, err
		}
	} else if !os.IsNotExist(statErr) {
		return result, statErr
	}
	if err = os.Rename(receipt.ProjectDir, previous); err != nil {
		return result, err
	}
	if err = parkHostedAppSource(previous); err != nil {
		_ = os.Rename(previous, receipt.ProjectDir)
		return result, err
	}
	if err = os.Rename(stage, receipt.ProjectDir); err != nil {
		_ = restoreParkedHostedAppSource(previous, receipt.ProjectDir, receipt.UID, receipt.GID)
		return result, err
	}
	stageExists = false
	if err = os.Chown(receipt.ProjectDir, int(receipt.UID), int(receipt.GID)); err != nil {
		_ = os.Rename(receipt.ProjectDir, stage)
		stageExists = true
		_ = restoreParkedHostedAppSource(previous, receipt.ProjectDir, receipt.UID, receipt.GID)
		return result, err
	}
	if err = os.Chmod(receipt.ProjectDir, 0750); err != nil {
		_ = os.Rename(receipt.ProjectDir, stage)
		stageExists = true
		_ = restoreParkedHostedAppSource(previous, receipt.ProjectDir, receipt.UID, receipt.GID)
		return result, err
	}
	oldHash := receipt.SourceSHA256
	receipt.PreviousSourceSHA256 = oldHash
	receipt.SourceSHA256 = hex.EncodeToString(inputStream.hash.Sum(nil))
	receipt.Status = "created"
	receipt.Revision++
	receipt.UpdatedAt = time.Now().UTC()
	if err = e.saveHostedApp(receipt); err != nil {
		current, readErr := e.hostedAppReceipt(receipt.ID)
		if readErr != nil || current.Revision != receipt.Revision || current.SourceSHA256 != receipt.SourceSHA256 {
			if removeErr := os.RemoveAll(receipt.ProjectDir); removeErr == nil {
				_ = restoreParkedHostedAppSource(previous, receipt.ProjectDir, receipt.UID, receipt.GID)
			}
			return result, err
		}
	}
	result.Data, _ = json.Marshal(receipt.HostedApplication)
	result.StdoutBytes = inputStream.bytes
	result.ExitCode = 0
	return result, nil
}
