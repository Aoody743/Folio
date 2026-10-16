package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/httpapi"
)

func databaseImportUploadSchema() map[string]any {
	return obj(map[string]any{
		"instance_id":       idSchema(),
		"database":          map[string]any{"type": "string", "pattern": databaseIdentifier.String()},
		"expected_revision": revisionSchema(),
		"import_id":         map[string]any{"type": "string", "pattern": databaseImportIDPattern.String()},
		"expected_bytes":    map[string]any{"type": "integer", "minimum": 1, "maximum": maxExtractedBytes},
		"expected_sha256":   map[string]any{"type": "string", "pattern": "^[a-fA-F0-9]{64}$"},
	}, "instance_id", "database", "expected_revision", "import_id", "expected_bytes", "expected_sha256")
}

type databaseImportUploadInput struct {
	InstanceID       string `json:"instance_id"`
	Database         string `json:"database"`
	ExpectedRevision int64  `json:"expected_revision"`
	ImportID         string `json:"import_id"`
	ExpectedBytes    int64  `json:"expected_bytes"`
	ExpectedSHA256   string `json:"expected_sha256"`
}

func (m *Manager) databaseImportBindings() []core.Binding {
	binding := m.binding("managed_database_import_upload", "Upload a bounded external database artifact after authorizing its exact managed database", "databases:restore", false, databaseImportUploadSchema(), managedDatabaseResources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		var upload databaseImportUploadInput
		if err := parse(raw, &upload); err != nil {
			return core.ActionResult{}, err
		}
		resources, err := managedDatabaseResources(ctx, actor, raw)
		if err != nil {
			return core.ActionResult{}, err
		}
		input := databaseImportStreamInput{InstanceID: upload.InstanceID, Database: upload.Database, DatabaseRevision: upload.ExpectedRevision, ImportID: upload.ImportID, ExpectedBytes: upload.ExpectedBytes, ExpectedSHA256: upload.ExpectedSHA256}
		imported, err := m.streamDatabaseImport(ctx, actor, input)
		if err != nil {
			return core.ActionResult{}, err
		}
		return core.ActionResult{Data: imported, Resources: resources, AuditSummary: map[string]any{"import_id": imported.ID, "bytes": imported.Bytes, "sha256": imported.SHA256}}, nil
	})
	binding.Action.Audit = true
	return []core.Binding{binding}
}

func (m *Manager) streamDatabaseImport(ctx context.Context, prior core.Actor, input databaseImportStreamInput) (DatabaseImport, error) {
	var imported DatabaseImport
	actor, err := m.deps.ResolveActor(ctx, prior)
	if err != nil {
		return imported, err
	}
	resource := DatabaseResource(input.InstanceID, input.Database)
	if !m.deps.Authorize(actor, "databases:restore", resource) {
		return imported, failure("forbidden", "Database import resource is not authorized", 403)
	}
	body, ok := ctx.Value(databaseImportBodyKey{}).(*databaseImportBody)
	if !ok || body == nil || body.ReadCloser == nil {
		return imported, failure("stream_required", "Database import requires an authenticated bounded upload stream", 400)
	}
	streamer, ok := m.executor.(databaseStreamExecutor)
	if !ok {
		return imported, failure("runtime_unavailable", "Controlled Agent streaming is required for database imports", 412)
	}
	raw, _ := json.Marshal(input)
	result, err := streamer.Stream(ctx, agent.Request{Operation: "database_import_artifact", Input: raw, ActorID: actor.ID}, agent.IO{Stdin: body})
	if err != nil {
		return imported, err
	}
	if result.ExitCode != 0 || len(result.Data) == 0 || json.Unmarshal(result.Data, &imported) != nil {
		return DatabaseImport{}, failure("agent_protocol_error", "Database import receipt was invalid", 502)
	}
	if imported.ID != input.ImportID || imported.InstanceID != input.InstanceID || imported.Database != input.Database || imported.Bytes != input.ExpectedBytes || imported.BytesReceived != input.ExpectedBytes || !strings.EqualFold(imported.SHA256, input.ExpectedSHA256) || imported.State != "uploaded" || imported.Revision != 2 {
		return DatabaseImport{}, failure("ownership_invalid", "Database import receipt does not match the authorized upload", 412)
	}
	return imported, nil
}

func (h *databaseResourceHandler) handleImport(w http.ResponseWriter, r *http.Request, actor core.Actor) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		databaseHTTPError(w, failure("method_not_allowed", "Method not allowed", 405))
		return true
	}
	select {
	case h.importSlots <- struct{}{}:
		defer func() { <-h.importSlots }()
	default:
		databaseHTTPError(w, failure("rate_limited", "Database import slots are busy", 429))
		return true
	}
	query := r.URL.Query()
	if len(query) != 3 || len(query["instance_id"]) != 1 || len(query["database"]) != 1 || len(query["expected_revision"]) != 1 {
		databaseHTTPError(w, failure("invalid_input", "A concrete instance, database and revision are required", 400))
		return true
	}
	instanceID, database := query.Get("instance_id"), query.Get("database")
	expectedRevision, err := strconv.ParseInt(query.Get("expected_revision"), 10, 64)
	if !validInstanceID(instanceID) || !validDatabaseName(database) || err != nil || expectedRevision < 1 {
		databaseHTTPError(w, failure("invalid_input", "Invalid database import target or revision", 400))
		return true
	}
	mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/octet-stream" || len(r.Header.Values("X-Content-SHA256")) != 1 || !validDatabaseImportSHA256(r.Header.Get("X-Content-SHA256")) {
		databaseHTTPError(w, failure("invalid_input", "Content-Type and one SHA-256 digest header are required", 400))
		return true
	}
	expectedBytes := r.ContentLength
	if expectedBytes < 1 || expectedBytes > maxExtractedBytes {
		databaseHTTPError(w, failure("invalid_input", "Upload requires a known size between 1 byte and 4 GiB", 413))
		return true
	}
	importID, err := newDatabaseImportID()
	if err != nil {
		databaseHTTPError(w, failure("internal_error", "Database import ID could not be generated", 500))
		return true
	}
	input := databaseImportUploadInput{InstanceID: instanceID, Database: database, ImportID: importID, ExpectedBytes: expectedBytes, ExpectedSHA256: strings.ToLower(r.Header.Get("X-Content-SHA256")), ExpectedRevision: expectedRevision}
	raw, _ := json.Marshal(input)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	body := &databaseImportBody{ReadCloser: http.MaxBytesReader(w, r.Body, expectedBytes)}
	defer body.Close()
	lease := &databaseImportHTTPLease{r: r, service: h.service, actor: actor, resource: DatabaseResource(instanceID, database), body: body, cancel: cancel}
	ctx = context.WithValue(ctx, databaseImportBodyKey{}, body)
	go lease.watch(ctx)
	data, err := h.service.Execute(ctx, actor, "managed_database_import_upload", raw)
	if err != nil {
		if leaseErr := lease.err.Load(); leaseErr != nil {
			err = leaseErr
		}
		databaseHTTPError(w, err)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(data)
	return true
}

type databaseImportHTTPLease struct {
	r        *http.Request
	service  *core.Service
	actor    core.Actor
	resource string
	body     io.Closer
	cancel   context.CancelFunc
	err      atomic.Pointer[core.Error]
}

func (l *databaseImportHTTPLease) watch(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			actor, err := httpapi.RevalidateActor(l.r)
			if err == nil && !l.service.Authorized(actor, "databases:restore", l.resource) {
				err = failure("forbidden", "Database import authorization changed", 403)
			}
			if err != nil {
				var domain *core.Error
				if !errors.As(err, &domain) {
					domain = &core.Error{Code: "unauthorized", Message: "Database import lease is invalid", Status: 401}
				}
				l.err.Store(domain)
				l.cancel()
				_ = l.body.Close()
				return
			}
		}
	}
}
