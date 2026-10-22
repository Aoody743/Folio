package sites

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

type backupResourceHandler struct {
	service *core.Service
	slots   chan struct{}
}

// Mount only through the shared HTTP adapter's authenticated resource hook.
func (p *Provider) BackupResourceHandler(service *core.Service) httpapi.ResourceHandler {
	return &backupResourceHandler{service: service, slots: make(chan struct{}, 8)}
}

func (h *backupResourceHandler) HandleResource(w http.ResponseWriter, r *http.Request, actor core.Actor) bool {
	if r.URL.Path != "/api/v1/sites/backups/download" {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		backupHTTPError(w, failure("method_not_allowed", "Method not allowed", 405))
		return true
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		backupHTTPError(w, failure("rate_limited", "Website download slots are busy", 429))
		return true
	}
	query := r.URL.Query()
	if len(query) != 2 || len(query["id"]) != 1 || len(query["backup_id"]) != 1 || len(r.URL.RawQuery) > 512 {
		backupHTTPError(w, invalid("One concrete website and backup are required"))
		return true
	}
	raw := encode(backupDownloadInput{ID: query.Get("id"), BackupID: query.Get("backup_id")})
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	writer := &backupHTTPWriter{w: w, r: r, service: h.service, siteID: query.Get("id"), cancel: cancel}
	defer http.NewResponseController(w).SetWriteDeadline(time.Time{})
	go writer.watchCredential(ctx)
	if r.Method == http.MethodGet {
		ctx = context.WithValue(ctx, backupDownloadKey{}, backupPreparedWriter(writer))
	}
	output, err := h.service.Execute(ctx, actor, "sites_backup_download", raw)
	if err == nil && r.Method == http.MethodHead {
		value, ok := output.(map[string]any)
		if !ok {
			err = failure("artifact_invalid", "Website download metadata is invalid", 502)
		} else if backup, ok := value["backup"].(Backup); ok {
			_, _, err = writer.Prepare(backup)
		} else {
			err = failure("artifact_invalid", "Website download metadata is invalid", 502)
		}
	}
	if err != nil {
		if leaseErr := writer.leaseErr.Load(); leaseErr != nil {
			err = leaseErr
		}
		if writer.wrote.Load() {
			panic(http.ErrAbortHandler)
		}
		backupHTTPError(w, err)
		return true
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
	}
	return true
}

func backupHTTPError(w http.ResponseWriter, err error) {
	var domain *core.Error
	if !errors.As(err, &domain) {
		domain = &core.Error{Code: "backup_export_failed", Message: "Website backup export could not be completed", Status: 500}
	}
	for _, header := range []string{"Content-Length", "Content-Disposition", "ETag", "Accept-Ranges"} {
		w.Header().Del(header)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(domain.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": domain})
}

type backupHTTPWriter struct {
	w         http.ResponseWriter
	r         *http.Request
	service   *core.Service
	siteID    string
	cancel    context.CancelFunc
	lastCheck time.Time
	status    int
	wrote     atomic.Bool
	closed    atomic.Bool
	leaseErr  atomic.Pointer[core.Error]
}

func (w *backupHTTPWriter) authorization() error {
	actor, err := httpapi.RevalidateActor(w.r)
	if err != nil {
		return err
	}
	if !w.service.Authorized(actor, "sites:backup", Resource(w.siteID)) || !w.service.Authorized(actor, "files:read", "fs:site-"+w.siteID) {
		return denied()
	}
	return nil
}

// Original cookie/token validation is independent of blocked socket writes.
// ResolveActor alone cannot detect logout of a human's particular cookie.
func (w *backupHTTPWriter) watchCredential(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.authorization(); err != nil {
				var domain *core.Error
				if !errors.As(err, &domain) {
					domain = &core.Error{Code: "unauthorized", Message: "Website export credential lease expired", Status: 401}
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

func (w *backupHTTPWriter) Prepare(backup Backup) (int64, int64, error) {
	if err := w.authorization(); err != nil {
		return 0, 0, err
	}
	start, length := int64(0), int64(0)
	etag := `"sha256-` + backup.SHA256 + `"`
	if header := w.r.Header.Get("Range"); w.r.Method == http.MethodGet && header != "" && (w.r.Header.Get("If-Range") == "" || w.r.Header.Get("If-Range") == etag) {
		var err error
		start, length, err = parseBackupRange(header, backup.Size)
		if err != nil {
			w.w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(backup.Size, 10))
			return 0, 0, err
		}
		w.status = http.StatusPartialContent
		w.w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(start+length-1, 10)+"/"+strconv.FormatInt(backup.Size, 10))
	}
	w.w.Header().Set("Content-Type", "application/gzip")
	w.w.Header().Set("Content-Disposition", `attachment; filename="`+backup.ID+`.tar.gz"`)
	w.w.Header().Set("Accept-Ranges", "bytes")
	size := backup.Size
	if length > 0 {
		size = length
	}
	w.w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.w.Header().Set("ETag", etag)
	w.w.Header().Set("Cache-Control", "no-store")
	return start, length, nil
}

func parseBackupRange(header string, size int64) (int64, int64, error) {
	invalidRange := func() (int64, int64, error) {
		return 0, 0, failure("range_not_satisfiable", "One valid bounded byte range is required", 416)
	}
	if size < 1 || len(header) > 128 || !strings.HasPrefix(header, "bytes=") || strings.Contains(header, ",") {
		return invalidRange()
	}
	startText, endText, ok := strings.Cut(strings.TrimPrefix(header, "bytes="), "-")
	if !ok {
		return invalidRange()
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
			return invalidRange()
		}
		length := min(suffix, size)
		return size - length, length, nil
	}
	start, err := parse(startText)
	if err != nil || start < 0 || start >= size {
		return invalidRange()
	}
	end := size - 1
	if endText != "" {
		end, err = parse(endText)
		if err != nil || end < start {
			return invalidRange()
		}
		end = min(end, size-1)
	}
	return start, end - start + 1, nil
}

func (w *backupHTTPWriter) Write(data []byte) (int, error) {
	if w.closed.Load() {
		return 0, context.Canceled
	}
	if err := w.r.Context().Err(); err != nil {
		return 0, err
	}
	if w.lastCheck.IsZero() || time.Since(w.lastCheck) >= time.Second {
		if err := w.authorization(); err != nil {
			return 0, err
		}
		w.lastCheck = time.Now()
	}
	_ = http.NewResponseController(w.w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	if !w.wrote.Swap(true) && w.status != 0 {
		w.w.WriteHeader(w.status)
	}
	return w.w.Write(data)
}

func (w *backupHTTPWriter) Close() error {
	w.closed.Store(true)
	w.cancel()
	if w.wrote.Load() {
		_ = http.NewResponseController(w.w).SetWriteDeadline(time.Now())
	}
	return nil
}
