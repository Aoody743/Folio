package serverbrowser

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// EngineOptions belongs to trusted server composition, never to action input.
// The sandbox override exists only to exercise the engine inside isolated CI.
type EngineOptions struct {
	Executable                 string
	UnsafeNoSandboxForTests    bool
	StartupDiagnosticsForTests io.Writer
}

type cdpResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Method    string          `json:"method"`
	SessionID string          `json:"sessionId"`
	Params    json.RawMessage `json:"params"`
}

type engine struct {
	cmd          *exec.Cmd
	profile      string
	commands     *os.File
	messages     *os.File
	writeMu      sync.Mutex
	mu           sync.Mutex
	pending      map[int64]chan cdpResponse
	tabs         map[string]string // issued target ID -> private flattened CDP session
	dialogs      map[string]Dialog // private CDP session -> pending website dialog
	frames       map[string][]byte
	changed      chan struct{}
	order        []string
	next         atomic.Int64
	initializing atomic.Bool
	done         chan struct{}
	processDone  chan struct{}
	events       chan struct{}
	closeOnce    sync.Once
	cleanupOnce  sync.Once
}

func availableEngine(opts EngineOptions) (string, string) {
	if os.Geteuid() == 0 && !opts.UnsafeNoSandboxForTests {
		return "", "The server browser must run as an unprivileged user with the Chromium sandbox enabled."
	}
	if opts.Executable != "" {
		path, err := exec.LookPath(opts.Executable)
		if err != nil {
			return "", "The configured Chromium executable is not available."
		}
		return path, ""
	}
	// Prefer a native executable across all known packages before falling back
	// to launch scripts. In particular, Ubuntu may expose a Snap installer stub
	// as chromium even when a usable native Google Chrome is already installed.
	wrappers := []string{}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if nativeBrowserFile(path) {
			return path, ""
		}
		resolved, _ := filepath.EvalSymlinks(path)
		candidates := []string{}
		if resolved != "" && strings.HasPrefix(name, "google-chrome") {
			candidates = append(candidates, filepath.Join(filepath.Dir(resolved), "chrome"))
		}
		if resolved == "/usr/bin/chromium" {
			candidates = append(candidates, "/usr/lib/chromium/chromium")
		}
		for _, candidate := range candidates {
			if actual, e := exec.LookPath(candidate); e == nil && nativeBrowserFile(actual) {
				return actual, ""
			}
		}
		wrappers = append(wrappers, path)
	}
	if len(wrappers) > 0 {
		return wrappers[0], ""
	}
	return "", "Install Chromium or Google Chrome on this server to use the server browser."
}

// Inspect only a regular installed file, never run a launcher during discovery.
func nativeBrowserFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	var magic [4]byte
	_, err = io.ReadFull(file, magic[:])
	return err == nil && bytes.Equal(magic[:], []byte{0x7f, 'E', 'L', 'F'})
}

func startEngine(ctx context.Context, opts EngineOptions, width, height int) (*engine, error) {
	executable, reason := availableEngine(opts)
	if executable == "" {
		return nil, failure("browser_unavailable", reason, 503)
	}
	profile, err := os.MkdirTemp("", "folio-browser-")
	if err != nil {
		return nil, failure("browser_unavailable", "Could not create a private browser profile", 503)
	}
	fail := func(err error) (*engine, error) { _ = os.RemoveAll(profile); return nil, err }
	commandRead, commandWrite, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	messageRead, messageWrite, err := os.Pipe()
	if err != nil {
		commandRead.Close()
		commandWrite.Close()
		return fail(err)
	}
	args := []string{"--headless=new", "--remote-debugging-pipe", "--user-data-dir=" + profile,
		"--no-first-run", "--no-default-browser-check", "--no-startup-window",
		"--disable-background-networking", "--disable-component-update", "--disable-default-apps",
		"--disable-sync", "--disable-extensions", "--disable-notifications", "--deny-permission-prompts",
		"--disable-breakpad", "--password-store=basic", "--use-mock-keychain",
		"--disable-features=MediaRouter,OptimizationHints,AutofillServerCommunication",
		"--window-size=" + fmt.Sprint(width) + "," + fmt.Sprint(height)}
	if opts.UnsafeNoSandboxForTests {
		args = append(args, "--no-sandbox")
	}
	command := exec.Command(executable, args...)
	command.ExtraFiles = []*os.File{commandRead, messageWrite}
	command.Env = append(os.Environ(), "XDG_CACHE_HOME="+profile+"/cache", "XDG_CONFIG_HOME="+profile+"/config", "XDG_DATA_HOME="+profile+"/data")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Browser diagnostics may include visited URLs. Do not copy them into
	// Folio logs, action output or persistent audit records.
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if opts.StartupDiagnosticsForTests != nil {
		command.Stderr = opts.StartupDiagnosticsForTests
	}
	if err = command.Start(); err != nil {
		commandRead.Close()
		commandWrite.Close()
		messageRead.Close()
		messageWrite.Close()
		return fail(failure("browser_unavailable", "Chromium could not be started", 503))
	}
	commandRead.Close()
	messageWrite.Close()
	e := &engine{cmd: command, profile: profile, commands: commandWrite, messages: messageRead, pending: map[int64]chan cdpResponse{}, tabs: map[string]string{}, dialogs: map[string]Dialog{}, frames: map[string][]byte{}, changed: make(chan struct{}), done: make(chan struct{}), processDone: make(chan struct{}), events: make(chan struct{}, 128)}
	go e.readLoop()
	go func() { _ = command.Wait(); close(e.processDone); e.shutdown(); e.cleanup() }()
	// Installation health probes can afford a cold browser start. The caller
	// still bounds interactive opens, and individual website controls keep
	// their ordinary timeout after initialization.
	e.initializing.Store(true)
	initCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	phase := "Browser.getVersion"
	if err = e.call(initCtx, "", "Browser.getVersion", nil, nil); err == nil {
		phase = "Browser.setDownloadBehavior"
		err = e.call(initCtx, "", "Browser.setDownloadBehavior", map[string]any{"behavior": "deny"}, nil)
	}
	if err == nil {
		phase = "Target.setDiscoverTargets"
		err = e.call(initCtx, "", "Target.setDiscoverTargets", map[string]any{"discover": true}, nil)
	}
	if err == nil {
		phase = "initial blank tab"
		_, err = e.NewTab(initCtx, "about:blank", width, height)
	}
	e.initializing.Store(false)
	if err != nil {
		e.Close()
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, failure("browser_timeout", "Chromium startup timed out during "+phase+"; retry or check server load", 504)
		}
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, failure("browser_sandbox_unavailable", "Chromium could not start during "+phase+" ("+err.Error()+"). Check the server's Chromium installation and sandbox support.", 503)
	}
	return e, nil
}

func splitNUL(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	return 0, nil, nil
}
func (e *engine) readLoop() {
	scanner := bufio.NewScanner(e.messages)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	scanner.Split(splitNUL)
	for scanner.Scan() {
		var msg cdpResponse
		if json.Unmarshal(scanner.Bytes(), &msg) != nil {
			break
		}
		if msg.ID != 0 {
			e.mu.Lock()
			ch := e.pending[msg.ID]
			e.mu.Unlock()
			if ch != nil {
				select {
				case ch <- msg:
				default:
				}
			}
		} else if msg.Method == "Fetch.requestPaused" {
			// Interception replies must never block the CDP reader waiting
			// for the very response that this reader is responsible for.
			var paused struct {
				RequestID string `json:"requestId"`
				Request   struct {
					URL string `json:"url"`
				} `json:"request"`
				ResourceType string `json:"resourceType"`
			}
			if json.Unmarshal(msg.Params, &paused) != nil {
				continue
			}
			select {
			case e.events <- struct{}{}:
			default:
				e.shutdown()
				if e.cmd.Process != nil {
					_ = syscall.Kill(-e.cmd.Process.Pid, syscall.SIGKILL)
				}
				return
			}
			go func() {
				defer func() { <-e.events }()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				method := "Fetch.continueRequest"
				params := map[string]any{"requestId": paused.RequestID}
				if !allowedNetworkURL(paused.Request.URL, paused.ResourceType == "Document") {
					method = "Fetch.failRequest"
					params["errorReason"] = "BlockedByClient"
				}
				_ = e.call(ctx, msg.SessionID, method, params, nil)
			}()
		} else if msg.Method == "Page.javascriptDialogOpening" {
			var dialog struct {
				Type          string `json:"type"`
				Message       string `json:"message"`
				DefaultPrompt string `json:"defaultPrompt"`
			}
			if json.Unmarshal(msg.Params, &dialog) == nil {
				e.mu.Lock()
				e.dialogs[msg.SessionID] = Dialog{Type: dialog.Type, Message: boundedDialogText(dialog.Message), DefaultPrompt: boundedDialogText(dialog.DefaultPrompt)}
				close(e.changed)
				e.changed = make(chan struct{})
				e.mu.Unlock()
			}
		} else if msg.Method == "Page.javascriptDialogClosed" {
			e.mu.Lock()
			delete(e.dialogs, msg.SessionID)
			e.mu.Unlock()
		} else if msg.Method == "Page.windowOpen" {
			// New-window links use the already controlled parent tab. Unattached
			// popup targets are closed below, so they never escape our tab budget.
			var opened struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(msg.Params, &opened) == nil && allowedNetworkURL(opened.URL, true) {
				select {
				case e.events <- struct{}{}:
					go func() {
						defer func() { <-e.events }()
						ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
						defer cancel()
						_ = e.call(ctx, msg.SessionID, "Page.navigate", map[string]any{"url": opened.URL}, nil)
					}()
				default:
				}
			}
		} else if msg.Method == "Target.targetCreated" {
			var created struct {
				Info struct {
					ID     string `json:"targetId"`
					Type   string `json:"type"`
					Opener string `json:"openerId"`
				} `json:"targetInfo"`
			}
			if json.Unmarshal(msg.Params, &created) == nil && created.Info.Type == "page" && created.Info.Opener != "" {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					_ = e.call(ctx, "", "Target.closeTarget", map[string]any{"targetId": created.Info.ID}, nil)
				}()
			}
		}
	}
	e.shutdown()
	if e.cmd.Process != nil {
		_ = syscall.Kill(-e.cmd.Process.Pid, syscall.SIGKILL)
	}
}
func (e *engine) call(ctx context.Context, session, method string, params any, out any) error {
	limit := 12 * time.Second
	if e.initializing.Load() {
		limit = 45 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	id := e.next.Add(1)
	message := map[string]any{"id": id, "method": method}
	if params != nil {
		message["params"] = params
	}
	if session != "" {
		message["sessionId"] = session
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(raw) > 128<<10 {
		return errors.New("Browser command is too large")
	}
	raw = append(raw, 0)
	ch := make(chan cdpResponse, 1)
	e.mu.Lock()
	select {
	case <-e.done:
		e.mu.Unlock()
		return errors.New("Browser session has ended")
	default:
	}
	if _, open := e.dialogs[session]; open && method == "Page.captureScreenshot" {
		e.mu.Unlock()
		return failure("browser_dialog_pending", "Answer the website dialog to continue", 409)
	}
	e.pending[id] = ch
	changed := e.changed
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.pending, id); e.mu.Unlock() }()
	e.writeMu.Lock()
	if deadline, ok := ctx.Deadline(); ok {
		_ = e.commands.SetWriteDeadline(deadline)
	}
	_, err = e.commands.Write(raw)
	e.writeMu.Unlock()
	if err != nil {
		return errors.New("Browser session is disconnected")
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.done:
			return errors.New("Browser session has ended")
		case <-changed:
			e.mu.Lock()
			_, dialog := e.dialogs[session]
			changed = e.changed
			e.mu.Unlock()
			// Chromium pauses the originating input/navigation response until its
			// website dialog is answered. Release the bounded control request so
			// the client can observe and answer that dialog without deadlocking.
			if dialog && (strings.HasPrefix(method, "Input.") || method == "Page.navigate" || method == "Page.reload" || method == "Page.navigateToHistoryEntry") {
				return nil
			}
			if dialog && method == "Page.captureScreenshot" {
				return failure("browser_dialog_pending", "Answer the website dialog to continue", 409)
			}
		case response := <-ch:
			if response.Error != nil {
				return errors.New("Chromium could not complete this browser operation")
			}
			if out != nil {
				return json.Unmarshal(response.Result, out)
			}
			return nil
		}
	}
}
func (e *engine) shutdown() {
	e.closeOnce.Do(func() { close(e.done); e.commands.Close(); e.messages.Close() })
}
func (e *engine) cleanup() { e.cleanupOnce.Do(func() { _ = os.RemoveAll(e.profile) }) }
func (e *engine) Close() error {
	e.shutdown()
	if e.cmd.Process != nil {
		_ = syscall.Kill(-e.cmd.Process.Pid, syscall.SIGKILL)
	}
	select {
	case <-e.processDone:
	case <-time.After(5 * time.Second):
		return errors.New("Browser process did not exit")
	}
	e.cleanup()
	return nil
}
func (e *engine) tabSession(id string) (string, error) {
	e.mu.Lock()
	session := e.tabs[id]
	e.mu.Unlock()
	if session == "" {
		return "", errors.New("Browser tab was not found")
	}
	return session, nil
}
func (e *engine) NewTab(ctx context.Context, address string, width, height int) (Tab, error) {
	if !allowedNetworkURL(address, true) {
		return Tab{}, errors.New("Only HTTP and HTTPS pages are supported")
	}
	var target struct {
		ID string `json:"targetId"`
	}
	if err := e.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &target); err != nil {
		return Tab{}, err
	}
	var attached struct {
		ID string `json:"sessionId"`
	}
	err := e.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target.ID, "flatten": true}, &attached)
	if err != nil {
		_ = e.call(ctx, "", "Target.closeTarget", map[string]any{"targetId": target.ID}, nil)
		return Tab{}, err
	}
	e.mu.Lock()
	e.tabs[target.ID] = attached.ID
	e.order = append(e.order, target.ID)
	e.mu.Unlock()
	for _, step := range []struct {
		method string
		params any
	}{
		{"Page.enable", nil},
		{"Page.setInterceptFileChooserDialog", map[string]any{"enabled": true}},
		{"Fetch.enable", map[string]any{"patterns": []map[string]any{{"urlPattern": "*", "requestStage": "Request"}}}},
	} {
		if err = e.call(ctx, attached.ID, step.method, step.params, nil); err != nil {
			_ = e.CloseTab(ctx, target.ID)
			return Tab{}, err
		}
	}
	if err = e.Resize(ctx, target.ID, width, height); err == nil && address != "about:blank" {
		err = e.Navigate(ctx, target.ID, address)
	}
	if err != nil {
		_ = e.CloseTab(ctx, target.ID)
		return Tab{}, err
	}
	return Tab{ID: target.ID, URL: address}, nil
}
func (e *engine) CloseTab(ctx context.Context, id string) error {
	if _, err := e.tabSession(id); err != nil {
		return err
	}
	if err := e.call(ctx, "", "Target.closeTarget", map[string]any{"targetId": id}, nil); err != nil {
		return err
	}
	e.mu.Lock()
	delete(e.dialogs, e.tabs[id])
	delete(e.frames, id)
	delete(e.tabs, id)
	for i, candidate := range e.order {
		if candidate == id {
			e.order = append(e.order[:i], e.order[i+1:]...)
			break
		}
	}
	e.mu.Unlock()
	return nil
}
func (e *engine) Tabs(ctx context.Context) ([]Tab, error) {
	var targets struct {
		Infos []struct {
			ID    string `json:"targetId"`
			Type  string `json:"type"`
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"targetInfos"`
	}
	if err := e.call(ctx, "", "Target.getTargets", nil, &targets); err != nil {
		return nil, err
	}
	result := []Tab{}
	e.mu.Lock()
	order := append([]string(nil), e.order...)
	e.mu.Unlock()
	for _, id := range order {
		for _, target := range targets.Infos {
			if target.Type != "page" || target.ID != id {
				continue
			}
			session, err := e.tabSession(target.ID)
			if err != nil {
				continue
			}
			tab := Tab{ID: target.ID, Title: target.Title, URL: target.URL}
			e.mu.Lock()
			if dialog, ok := e.dialogs[session]; ok {
				copy := dialog
				tab.Dialog = &copy
			}
			e.mu.Unlock()
			var history struct {
				Index   int               `json:"currentIndex"`
				Entries []json.RawMessage `json:"entries"`
			}
			if tab.Dialog == nil && e.call(ctx, session, "Page.getNavigationHistory", nil, &history) == nil {
				tab.CanBack = history.Index > 0
				tab.CanForward = history.Index < len(history.Entries)-1
			}
			result = append(result, tab)
			break
		}
	}
	return result, nil
}
func allowedNetworkURL(address string, document bool) bool {
	if address == "about:blank" {
		return true
	}
	parsed, err := url.Parse(address)
	if err != nil {
		return false
	}
	if parsed.Scheme == "http" || parsed.Scheme == "https" {
		return parsed.Host != "" && parsed.User == nil
	}
	// Data/blob subresources are normal browser content. They never grant a
	// document navigation or direct access to the host filesystem.
	return !document && (parsed.Scheme == "data" || parsed.Scheme == "blob")
}
func (e *engine) Navigate(ctx context.Context, id, address string) error {
	if !allowedNetworkURL(address, true) {
		return errors.New("Only HTTP and HTTPS pages are supported")
	}
	session, err := e.tabSession(id)
	if err != nil {
		return err
	}
	var navigation struct {
		Error string `json:"errorText"`
	}
	if err = e.call(ctx, session, "Page.navigate", map[string]any{"url": address}, &navigation); err != nil {
		return err
	}
	if navigation.Error != "" {
		return errors.New("The server browser could not load this address. Check the address, server network and website certificate.")
	}
	return nil
}
func (e *engine) History(ctx context.Context, id, direction string) error {
	session, err := e.tabSession(id)
	if err != nil {
		return err
	}
	if direction == "reload" {
		return e.call(ctx, session, "Page.reload", map[string]any{"ignoreCache": false}, nil)
	}
	if direction == "stop" {
		return e.call(ctx, session, "Page.stopLoading", nil, nil)
	}
	if direction != "back" && direction != "forward" {
		return errors.New("Invalid browser history action")
	}
	var history struct {
		Index   int `json:"currentIndex"`
		Entries []struct {
			ID  int    `json:"id"`
			URL string `json:"url"`
		} `json:"entries"`
	}
	if err = e.call(ctx, session, "Page.getNavigationHistory", nil, &history); err != nil {
		return err
	}
	index := history.Index - 1
	if direction == "forward" {
		index = history.Index + 1
	}
	if index < 0 || index >= len(history.Entries) {
		return nil
	}
	if !allowedNetworkURL(history.Entries[index].URL, true) {
		return errors.New("This history entry cannot be opened")
	}
	return e.call(ctx, session, "Page.navigateToHistoryEntry", map[string]any{"entryId": history.Entries[index].ID}, nil)
}
func boundedDialogText(text string) string {
	if len(text) <= 16384 {
		return text
	}
	runes := []rune(text)
	return string(runes[:min(len(runes), 4096)]) + "…"
}
func (e *engine) HandleDialog(ctx context.Context, id string, accept bool, text string) error {
	session, err := e.tabSession(id)
	if err != nil {
		return err
	}
	e.mu.Lock()
	_, found := e.dialogs[session]
	e.mu.Unlock()
	if !found {
		return failure("browser_no_dialog", "This page no longer has an open dialog", 409)
	}
	return e.call(ctx, session, "Page.handleJavaScriptDialog", map[string]any{"accept": accept, "promptText": text}, nil)
}
func (e *engine) Resize(ctx context.Context, id string, width, height int) error {
	session, err := e.tabSession(id)
	if err != nil {
		return err
	}
	return e.call(ctx, session, "Emulation.setDeviceMetricsOverride", map[string]any{"width": width, "height": height, "deviceScaleFactor": 1, "mobile": false}, nil)
}
func (e *engine) dialogFrame(session, id string) ([]byte, bool) {
	e.mu.Lock()
	_, dialog := e.dialogs[session]
	previous := e.frames[id]
	e.mu.Unlock()
	if dialog {
		if previous != nil {
			return previous, true
		}
		// The website can open a dialog before its first rendered frame.
		// Its actual message is presented separately, never auto-accepted.
		placeholder := image.NewRGBA(image.Rect(0, 0, 16, 9))
		for i := 0; i < len(placeholder.Pix); i += 4 {
			copy(placeholder.Pix[i:i+4], []byte{245, 247, 251, 255})
		}
		var out bytes.Buffer
		_ = jpeg.Encode(&out, placeholder, &jpeg.Options{Quality: 75})
		return out.Bytes(), true
	}
	return nil, false
}
func (e *engine) Frame(ctx context.Context, id string) ([]byte, error) {
	session, err := e.tabSession(id)
	if err != nil {
		return nil, err
	}
	if frame, open := e.dialogFrame(session, id); open {
		return frame, nil
	}
	if err = e.call(ctx, session, "Page.bringToFront", nil, nil); err != nil {
		return nil, err
	}
	var shot struct {
		Data string `json:"data"`
	}
	if err = e.call(ctx, session, "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 75, "fromSurface": true, "captureBeyondViewport": false, "optimizeForSpeed": true}, &shot); err != nil {
		if frame, open := e.dialogFrame(session, id); open {
			return frame, nil
		}
		return nil, err
	}
	frame, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil || len(frame) == 0 || len(frame) > 6<<20 {
		return nil, errors.New("Chromium returned an invalid browser image")
	}
	e.mu.Lock()
	e.frames[id] = frame
	e.mu.Unlock()
	return frame, nil
}
func (e *engine) Input(ctx context.Context, id string, in Input) error {
	session, err := e.tabSession(id)
	if err != nil {
		return err
	}
	if err = e.call(ctx, session, "Page.bringToFront", nil, nil); err != nil {
		return err
	}
	switch in.Kind {
	case "text":
		return e.call(ctx, session, "Input.insertText", map[string]any{"text": in.Text}, nil)
	case "mouse", "wheel":
		event := in.Event
		if in.Kind == "wheel" {
			event = "mouseWheel"
		}
		if event != "mouseMoved" && event != "mousePressed" && event != "mouseReleased" && event != "mouseWheel" {
			return errors.New("Invalid browser pointer event")
		}
		button := in.Button
		if button == "" {
			button = "none"
		}
		params := map[string]any{"type": event, "x": in.X, "y": in.Y, "button": button, "modifiers": in.Modifiers}
		if event == "mousePressed" || event == "mouseReleased" {
			params["clickCount"] = 1
		}
		if event == "mouseWheel" {
			params["deltaX"] = in.DeltaX
			params["deltaY"] = in.DeltaY
		}
		return e.call(ctx, session, "Input.dispatchMouseEvent", params, nil)
	case "key":
		event := in.Event
		if event == "" {
			event = "keyDown"
		}
		if event != "keyDown" && event != "keyUp" && event != "rawKeyDown" {
			return errors.New("Invalid browser keyboard event")
		}
		params := map[string]any{"type": event, "key": in.Key, "code": in.Code, "modifiers": in.Modifiers}
		if virtual := virtualKey(in.Key); virtual != 0 {
			params["windowsVirtualKeyCode"] = virtual
			params["nativeVirtualKeyCode"] = virtual
		}
		return e.call(ctx, session, "Input.dispatchKeyEvent", params, nil)
	}
	return errors.New("Invalid browser input")
}
func virtualKey(key string) int {
	if len(key) == 1 && ((key[0] >= 'a' && key[0] <= 'z') || (key[0] >= 'A' && key[0] <= 'Z')) {
		return int(strings.ToUpper(key)[0])
	}
	switch key {
	case "Backspace":
		return 8
	case "Tab":
		return 9
	case "Enter":
		return 13
	case "Shift":
		return 16
	case "Control":
		return 17
	case "Alt":
		return 18
	case "Escape":
		return 27
	case " ":
		return 32
	case "PageUp":
		return 33
	case "PageDown":
		return 34
	case "End":
		return 35
	case "Home":
		return 36
	case "ArrowLeft":
		return 37
	case "ArrowUp":
		return 38
	case "ArrowRight":
		return 39
	case "ArrowDown":
		return 40
	case "Delete":
		return 46
	case "Meta":
		return 91
	}
	return 0
}
