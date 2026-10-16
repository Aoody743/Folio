package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/httpapi"
)

type databaseResourceHandler struct {
	service     *core.Service
	slots       chan struct{}
	importSlots chan struct{}
}

// DatabaseResourceHandler joins the HTTP adapter's authenticated stream hook;
// it must be composed there, rather than mounted as an unauthenticated route.
func (m *Manager) DatabaseResourceHandler(service *core.Service) httpapi.ResourceHandler {
	return &databaseResourceHandler{service: service, slots: make(chan struct{}, 8), importSlots: make(chan struct{}, 2)}
}
func (h *databaseResourceHandler) HandleResource(w http.ResponseWriter, r *http.Request, a core.Actor) bool {
	if r.URL.Path == "/api/v1/databases/backups/imports" {
		return h.handleImport(w, r, a)
	}
	if r.URL.Path != "/api/v1/databases/backups/download" {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		databaseHTTPError(w, failure("method_not_allowed", "Method not allowed", 405))
		return true
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		databaseHTTPError(w, failure("rate_limited", "Database download slots are busy", 429))
		return true
	}
	query := r.URL.Query()
	if len(query) != 3 || len(query["instance_id"]) != 1 || len(query["database"]) != 1 || len(query["backup_id"]) != 1 {
		databaseHTTPError(w, failure("invalid_input", "A concrete instance, database and backup are required", 400))
		return true
	}
	raw, _ := json.Marshal(databaseArtifactInput{InstanceID: query.Get("instance_id"), Database: query.Get("database"), BackupID: query.Get("backup_id")})
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	writer := &databaseHTTPWriter{w: w, r: r, service: h.service, actor: a, resource: DatabaseResource(query.Get("instance_id"), query.Get("database")), cancel: cancel}
	defer http.NewResponseController(w).SetWriteDeadline(time.Time{})
	if r.Method == http.MethodGet {
		go writer.watchLease(ctx)
	}
	if r.Method == http.MethodGet {
		ctx = context.WithValue(ctx, databaseDownloadKey{}, databasePreparedWriter(writer))
	}
	data, err := h.service.Execute(ctx, a, "managed_database_backup_download", raw)
	if err != nil {
		if leaseErr := writer.leaseErr.Load(); leaseErr != nil {
			err = leaseErr
		}
		if writer.wrote.Load() {
			panic(http.ErrAbortHandler)
		}
		databaseHTTPError(w, err)
		return true
	}
	if r.Method == http.MethodHead {
		if value, ok := data.(map[string]any); ok {
			if backup, ok := value["backup"].(DatabaseBackup); ok {
				_, _, _ = writer.Prepare(backup)
			}
		}
		w.WriteHeader(http.StatusOK)
	}
	return true
}
func databaseHTTPError(w http.ResponseWriter, err error) {
	var domain *core.Error
	if !errors.As(err, &domain) {
		domain = &core.Error{Code: "internal_error", Message: "Database download could not be completed", Status: 500}
	}
	w.Header().Del("Content-Length")
	w.Header().Del("Content-Disposition")
	w.Header().Del("ETag")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(domain.Status)
	json.NewEncoder(w).Encode(map[string]any{"error": domain})
}

type databaseHTTPWriter struct {
	w         http.ResponseWriter
	r         *http.Request
	service   *core.Service
	actor     core.Actor
	resource  string
	cancel    context.CancelFunc
	lastCheck time.Time
	wrote     atomic.Bool
	closed    atomic.Bool
	status    int
	leaseErr  atomic.Pointer[core.Error]
}

// A slow receiver may block Write. Check the original credential independently
// so revocation cancels the root stream and interrupts backpressure within the
// same one-second lease, instead of waiting for the next successful write.
func (w *databaseHTTPWriter) watchLease(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a, err := httpapi.RevalidateActor(w.r)
			if err == nil && !w.service.Authorized(a, "databases:backup", w.resource) {
				err = failure("forbidden", "Database export authorization changed", 403)
			}
			if err != nil {
				var domain *core.Error
				if !errors.As(err, &domain) {
					domain = &core.Error{Code: "unauthorized", Message: "Database export lease is invalid", Status: 401}
				}
				w.leaseErr.Store(domain)
				w.cancel()
				if w.wrote.Load() {
					_ = http.NewResponseController(w.w).SetWriteDeadline(time.Now())
				}
				return
			}
		}
	}
}

func (w *databaseHTTPWriter) Prepare(backup DatabaseBackup) (int64, int64, error) {
	start, length := int64(0), int64(0)
	etag := `"sha256-` + backup.SHA256 + `"`
	header := w.r.Header.Get("Range")
	if w.r.Method == http.MethodGet && header != "" && (w.r.Header.Get("If-Range") == "" || w.r.Header.Get("If-Range") == etag) {
		var err error
		start, length, err = parseDatabaseRange(header, backup.Bytes)
		if err != nil {
			w.w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(backup.Bytes, 10))
			return 0, 0, err
		}
		w.status = http.StatusPartialContent
		w.w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(start+length-1, 10)+"/"+strconv.FormatInt(backup.Bytes, 10))
	}
	w.w.Header().Set("Content-Type", "application/octet-stream")
	w.w.Header().Set("Accept-Ranges", "bytes")
	w.w.Header().Set("Content-Disposition", `attachment; filename="`+backup.Filename+`"`)
	size := backup.Bytes
	if length > 0 {
		size = length
	}
	w.w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.w.Header().Set("ETag", etag)
	w.w.Header().Set("Cache-Control", "no-store")
	return start, length, nil
}
func parseDatabaseRange(header string, size int64) (int64, int64, error) {
	invalid := func() (int64, int64, error) {
		return 0, 0, failure("range_not_satisfiable", "One valid bounded byte range is required", 416)
	}
	if size < 1 || len(header) > 128 || !strings.HasPrefix(header, "bytes=") || strings.Contains(header, ",") {
		return invalid()
	}
	startText, endText, ok := strings.Cut(strings.TrimPrefix(header, "bytes="), "-")
	if !ok {
		return invalid()
	}
	parse := func(text string) (int64, error) {
		if text == "" || strings.ContainsAny(text, "+- \t\r\n") {
			return 0, errors.New("invalid byte range")
		}
		return strconv.ParseInt(text, 10, 64)
	}
	if startText == "" {
		suffix, err := parse(endText)
		if err != nil || suffix <= 0 {
			return invalid()
		}
		length := min(suffix, size)
		return size - length, length, nil
	}
	start, err := parse(startText)
	if err != nil || start < 0 || start >= size {
		return invalid()
	}
	end := size - 1
	if endText != "" {
		end, err = parse(endText)
		if err != nil || end < start {
			return invalid()
		}
		end = min(end, size-1)
	}
	return start, end - start + 1, nil
}
func (w *databaseHTTPWriter) Write(p []byte) (int, error) {
	if w.closed.Load() {
		return 0, context.Canceled
	}
	if err := w.r.Context().Err(); err != nil {
		return 0, err
	}
	if w.lastCheck.IsZero() || time.Since(w.lastCheck) >= time.Second {
		a, err := httpapi.RevalidateActor(w.r)
		if err != nil {
			return 0, err
		}
		if !w.service.Authorized(a, "databases:backup", w.resource) {
			return 0, failure("forbidden", "Database export authorization changed", 403)
		}
		w.lastCheck = time.Now()
	}
	_ = http.NewResponseController(w.w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	if !w.wrote.Swap(true) && w.status != 0 {
		w.w.WriteHeader(w.status)
	}
	return w.w.Write(p)
}
func (w *databaseHTTPWriter) Close() error {
	w.closed.Store(true)
	w.cancel()
	if w.wrote.Load() {
		_ = http.NewResponseController(w.w).SetWriteDeadline(time.Now())
	}
	return nil
}
