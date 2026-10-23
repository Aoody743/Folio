package serverbrowser

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/jobs"
)

const AppOperation = "browser_app_change"
const wasmVersion = "0.0.1"
const wasmURL = "https://github.com/HeyPuter/firefox-wasm/releases/download/v0.0.1/chrome-demo-v0.0.1.tar.gz"
const wasmSHA = "c6fd1c2101722f12cc5a933ec405af8a20c6d0ae849c66a5f1703b6d26db8e8b"
const chromeVersion = "156.0.8078.47"
const chromeURL = "https://storage.googleapis.com/chrome-for-testing-public/156.0.8078.47/linux64/chrome-headless-shell-linux64.zip"
const chromeSHA = "0d5acc62c3e213f8c87d3c298d66ffcf9a236d12ea4b9f85318bf85dbc279daa"

type browserApp struct {
	ID         string `json:"id"`
	Installed  bool   `json:"installed"`
	Version    string `json:"version,omitempty"`
	Revision   int64  `json:"revision"`
	TaskID     string `json:"task_id,omitempty"`
	TaskStatus string `json:"task_status,omitempty"`
	Available  bool   `json:"available"`
	Reason     string `json:"reason,omitempty"`
}
type appChange struct {
	ID               string `json:"id"`
	Operation        string `json:"operation"`
	ExpectedRevision int64  `json:"expected_revision"`
}

func (p *Provider) app(ctx context.Context, id string) (browserApp, error) {
	a := browserApp{ID: id, Available: true}
	if p.deps.State == nil {
		return a, nil
	}
	raw, err := p.deps.State.Get(ctx, "apps", id)
	if err != nil {
		if core.ErrorStatus(err) == 404 {
			return a, nil
		}
		return a, err
	}
	if len(raw) > 0 {
		err = json.Unmarshal(raw, &a)
	}
	return a, err
}
func (p *Provider) installed(ctx context.Context, id string) bool {
	a, err := p.app(ctx, id)
	return err == nil && a.Installed
}
func (p *Provider) engineOptions() EngineOptions {
	opts := p.options.Engine
	if opts.Executable == "" && p.options.AppsDir != "" {
		candidate := filepath.Join(p.options.AppsDir, "panel", "chrome-headless-shell-linux64", "chrome-headless-shell")
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			opts.Executable = candidate
		}
	}
	return opts
}
func (p *Provider) appBindings() []core.Binding {
	var bindings []core.Binding
	for _, name := range []string{"browser_apps_list", "browser_app_change"} {
		name := name
		read := name == "browser_apps_list"
		scope := "software:manage"
		props := map[string]any{}
		required := []string{}
		if read {
			scope = Scope
		} else {
			props = map[string]any{"id": enum("panel", "firefox-wasm"), "operation": enum("install", "uninstall"), "expected_revision": integer(0, 1<<52)}
			required = []string{"id", "operation", "expected_revision"}
		}
		bindings = append(bindings, core.Binding{Action: core.Action{Name: name, Description: "Manage independently optional Folio browser applications", Scope: scope, ReadOnly: read, InputSchema: schema(props, required...)}, Resources: func(ctx context.Context, a core.Actor, raw json.RawMessage) ([]string, error) {
			return []string{Resource}, nil
		}, Execute: func(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			if read {
				items := []browserApp{}
				for _, id := range []string{"panel", "firefox-wasm"} {
					app, err := p.app(ctx, id)
					if err != nil {
						return core.ActionResult{}, err
					}
					if app.TaskID != "" {
						if t, e := p.deps.Tasks.Get(ctx, a, app.TaskID); e == nil {
							app.TaskStatus, _ = t["status"].(string)
						}
					}
					if id == "panel" {
						_, reason := availableEngine(p.engineOptions())
						if reason != "" && (app.Installed || runtime.GOARCH != "amd64") {
							app.Available = false
							app.Reason = reason
						}
					}
					if id == "firefox-wasm" && app.Installed {
						for _, name := range []string{"index.html", "gecko.wasm.zst", "chrome-assets.tar.zst"} {
							if info, e := os.Stat(filepath.Join(p.options.AppsDir, id, name)); e != nil || !info.Mode().IsRegular() {
								app.Available = false
								app.Reason = "Firefox assets are missing; uninstall and install this app again"
								break
							}
						}
					}
					if p.options.AppsDir == "" {
						app.Available = false
						app.Reason = "Browser app storage is not configured"
					}
					items = append(items, app)
				}
				return core.ActionResult{Data: map[string]any{"items": items}, Resources: []string{Resource}}, nil
			}
			var in appChange
			if err := json.Unmarshal(raw, &in); err != nil {
				return core.ActionResult{}, invalid()
			}
			if !p.deps.Authorize(a, Scope, Resource) {
				return core.ActionResult{}, denied()
			}
			if p.options.AppsDir == "" {
				return core.ActionResult{}, failure("unavailable", "Browser app storage is not configured", 503)
			}
			current, err := p.app(ctx, in.ID)
			if err != nil {
				return core.ActionResult{}, err
			}
			if current.TaskStatus == jobs.Queued || current.TaskStatus == jobs.Running {
				previous, e := p.deps.Tasks.Get(ctx, a, current.TaskID)
				if e != nil || previous["status"] == jobs.Queued || previous["status"] == jobs.Running {
					return core.ActionResult{}, failure("conflict", "A change is already in progress; view its task first", 409)
				}
			}
			task, err := p.deps.Tasks.SubmitPrepared(ctx, a, AppOperation, encoded(in), core.TaskMetadata{Resources: []string{Resource}}, func(store core.MetadataStore, job jobs.Job) error {
				raw, version, e := store.GetVersioned(ctx, "apps", in.ID)
				if e != nil && core.ErrorStatus(e) != 404 {
					return e
				}
				record := browserApp{ID: in.ID, Available: true}
				if len(raw) > 0 {
					if e = json.Unmarshal(raw, &record); e != nil {
						return e
					}
				}
				if record.Revision != in.ExpectedRevision {
					return failure("conflict", "Browser app changed; refresh and retry", 409)
				}
				record.Revision++
				record.TaskID = job.ID
				record.TaskStatus = jobs.Queued
				_, e = store.CompareAndSwap(ctx, "apps", in.ID, version, encoded(record))
				return e
			})
			if err != nil {
				return core.ActionResult{}, err
			}
			id, _ := task["id"].(string)
			return core.ActionResult{Data: map[string]any{"task": task, "task_id": id, "status": "submitted"}, TaskID: id, Resources: []string{Resource}, AuditSummary: map[string]any{"app": in.ID, "operation": in.Operation}}, nil
		}})
	}
	return bindings
}
func (p *Provider) Validate(req agent.Request) error {
	var in appChange
	if req.Operation != AppOperation || json.Unmarshal(req.Input, &in) != nil || (in.ID != "panel" && in.ID != "firefox-wasm") || (in.Operation != "install" && in.Operation != "uninstall") {
		return invalid()
	}
	return nil
}
func (p *Provider) Run(ctx context.Context, req agent.Request) (result agent.Result, runErr error) {
	result = agent.Result{Operation: req.Operation, StartedAt: time.Now().UTC()}
	if err := p.Validate(req); err != nil {
		return result, err
	}
	select {
	case p.appWork <- struct{}{}:
		defer func() { <-p.appWork }()
	case <-ctx.Done():
		return result, ctx.Err()
	}
	var in appChange
	_ = json.Unmarshal(req.Input, &in)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		current, err := p.app(cleanup, in.ID)
		if err == nil && current.TaskID == req.JobID {
			current.TaskStatus = jobs.Succeeded
			if runErr != nil {
				current.TaskStatus = jobs.Failed
				if ctx.Err() != nil {
					current.TaskStatus = jobs.Canceled
				}
			}
			_ = p.deps.State.Save(cleanup, "apps", in.ID, encoded(current))
		}
	}()

	actor, err := p.deps.ResolveActor(ctx, core.Actor{ID: req.ActorID})
	if err != nil {
		return result, err
	}
	if !p.deps.Authorize(actor, "software:manage", Resource) || !p.deps.Authorize(actor, Scope, Resource) {
		return result, denied()
	}
	record, err := p.app(ctx, in.ID)
	if err != nil {
		return result, err
	}
	if req.JobID == "" || record.TaskID != req.JobID {
		return result, failure("conflict", "Browser app task was superseded", 409)
	}
	emit := func(message string) {
		if req.Output != nil {
			select {
			case req.Output <- agent.Output{Stream: "stdout", Text: message + "\n"}:
			default:
			}
		}
	}
	emit("Preparing browser app change")
	record.TaskStatus = jobs.Running
	if err = p.deps.State.Save(ctx, "apps", in.ID, encoded(record)); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err = os.MkdirAll(p.options.AppsDir, 0700); err != nil {
		return result, err
	}
	info, err := os.Lstat(p.options.AppsDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return result, errors.New("browser app storage must be a private directory")
	}
	path := filepath.Join(p.options.AppsDir, in.ID)
	if in.Operation == "uninstall" {
		p.appGate.Lock()
		defer p.appGate.Unlock()
		record.Installed = false
		record.Version = ""
		if err = p.deps.State.Save(ctx, "apps", in.ID, encoded(record)); err != nil {
			return result, err
		}
		p.stopApp(in.ID)
		err = os.RemoveAll(path)
	} else if !record.Installed {
		stage, e := os.MkdirTemp(p.options.AppsDir, ".install-")
		if e != nil {
			return result, e
		}
		defer os.RemoveAll(stage)
		if in.ID == "firefox-wasm" {
			emit("Downloading and verifying Firefox WASM (about 55 MB)")
			e = fetchArchive(ctx, wasmURL, wasmSHA, stage, false)
			if e == nil {
				e = prepareWasm(filepath.Join(stage, "dist"))
			}
			if e == nil {
				e = os.Rename(filepath.Join(stage, "dist"), filepath.Join(stage, "ready"))
			}
			record.Version = wasmVersion
		} else {
			_, reason := availableEngine(p.options.Engine)
			if reason != "" {
				if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
					return result, errors.New("Install a distribution Chromium browser first on this architecture, then retry")
				}
				emit("Downloading and verifying Chromium headless shell (about 125 MB)")
				e = fetchArchive(ctx, chromeURL, chromeSHA, stage, true)
				if e == nil {
					testOpts := p.options.Engine
					testOpts.Executable = filepath.Join(stage, "chrome-headless-shell-linux64", "chrome-headless-shell")
					var engine *engine
					engine, e = startEngine(ctx, testOpts, 640, 480)
					if engine != nil {
						engine.Close()
					}
				}
			}
			if e == nil {
				e = os.MkdirAll(filepath.Join(stage, "ready"), 0700)
			}
			if e == nil {
				if _, statErr := os.Stat(filepath.Join(stage, "chrome-headless-shell-linux64")); statErr == nil {
					e = os.Rename(filepath.Join(stage, "chrome-headless-shell-linux64"), filepath.Join(stage, "ready", "chrome-headless-shell-linux64"))
				}
			}
			if e == nil && reason == "" {
				emit("Checking system Chromium sandbox startup")
				var probe *engine
				probe, e = startEngine(ctx, p.options.Engine, 640, 480)
				if probe != nil {
					probe.Close()
				}
			}
			record.Version = "system"
			if reason != "" {
				record.Version = chromeVersion
			}
		}
		if e != nil {
			return result, fmt.Errorf("browser installation failed; check network, available storage and Chromium system libraries/sandbox: %w", e)
		}
		fresh, e := p.deps.ResolveActor(ctx, actor)
		if e != nil {
			return result, e
		}
		if !p.deps.Authorize(fresh, "software:manage", Resource) || !p.deps.Authorize(fresh, Scope, Resource) {
			return result, denied()
		}
		p.appGate.Lock()
		defer p.appGate.Unlock()
		if e = os.RemoveAll(path); e == nil {
			e = os.Rename(filepath.Join(stage, "ready"), path)
		}
		if e != nil {
			return result, e
		}
		record.Installed = true
		err = p.deps.State.Save(ctx, "apps", in.ID, encoded(record))
	}
	if err != nil {
		return result, err
	}
	emit("Browser app change completed")
	record.TaskStatus = jobs.Succeeded
	result.FinishedAt = time.Now().UTC()
	result.Stdout = "Browser app change completed"
	result.Data = encoded(record)
	return result, nil
}
func (p *Provider) stopApp(id string) {
	p.mu.Lock()
	sessions := []*ownedSession{}
	if id == "panel" {
		for _, s := range p.sessions {
			sessions = append(sessions, s)
		}
	} else {
		for c := range p.wisps {
			c.cancel()
		}
	}
	p.mu.Unlock()
	for _, s := range sessions {
		s.mu.Lock()
		if !s.closed {
			p.endSession(s)
		}
		s.mu.Unlock()
	}
}

// Sources and hashes are compiled in. No request can choose a download URL,
// archive path, executable, command, destination or checksum.
func fetchArchive(ctx context.Context, source, digest, dir string, isZip bool) error {
	req, err := http.NewRequestWithContext(ctx, "GET", source, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 8 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(filepath.Join(dir, "download"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(resp.Body, 256<<20+1))
	if err != nil {
		return err
	}
	if n > 256<<20 {
		return errors.New("archive exceeds download limit")
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("archive checksum mismatch")
	}
	if _, err = f.Seek(0, 0); err != nil {
		return err
	}
	var total int64
	count := 0
	write := func(name string, size int64, reader io.Reader, executable bool) error {
		count++
		total += size
		clean := filepath.Clean(name)
		if count > 10000 || size < 0 || size > 256<<20 || total > 768<<20 || filepath.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || strings.Contains(name, "\\") {
			return errors.New("unsafe or oversized browser archive")
		}
		dest := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return err
		}
		mode := os.FileMode(0600)
		if executable {
			mode = 0700
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		got, e := io.CopyN(out, reader, size)
		closeErr := out.Close()
		if e != nil {
			return e
		}
		if got != size {
			return io.ErrUnexpectedEOF
		}
		return closeErr
	}
	if isZip {
		zr, e := zip.NewReader(f, n)
		if e != nil {
			return e
		}
		for _, h := range zr.File {
			if h.FileInfo().IsDir() {
				continue
			}
			if !h.Mode().IsRegular() {
				return errors.New("nonregular browser archive member")
			}
			r, e := h.Open()
			if e != nil {
				return e
			}
			e = write(h.Name, int64(h.UncompressedSize64), r, h.Mode()&0111 != 0)
			r.Close()
			if e != nil {
				return e
			}
		}
	} else {
		gz, e := gzip.NewReader(f)
		if e != nil {
			return e
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			h, e := tr.Next()
			if e == io.EOF {
				break
			}
			if e != nil {
				return e
			}
			if h.Typeflag == tar.TypeDir {
				continue
			}
			if h.Typeflag != tar.TypeReg {
				return errors.New("nonregular browser archive member")
			}
			if e = write(h.Name, h.Size, tr, h.Mode&0111 != 0); e != nil {
				return e
			}
		}
	}
	return nil
}
