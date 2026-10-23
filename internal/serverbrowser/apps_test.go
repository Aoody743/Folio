package serverbrowser

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/httpapi"
	"github.com/coder/websocket"
)

func waitAppTask(t *testing.T, f providerFixture, result any) {
	t.Helper()
	var response struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(encoded(result), &response)
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		job, e := f.service.Runtime("serverbrowser").Tasks.Get(context.Background(), f.admin, response.TaskID)
		if e != nil {
			t.Fatal(e)
		}
		switch job["status"] {
		case "succeeded":
			return
		case "failed", "canceled", "interrupted":
			if file, ok := f.browser.options.Engine.StartupDiagnosticsForTests.(*os.File); ok {
				body, _ := os.ReadFile(file.Name())
				t.Logf("Chromium startup diagnostics: %s", body)
			}
			t.Fatalf("app task: %v", job)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("app task timed out")
}
func TestOptionalBrowserLifecycleAndPermissions(t *testing.T) {
	diagnostics, err := os.CreateTemp(t.TempDir(), "chromium-startup-")
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostics.Close()
	f := setupProvider(t, Options{Engine: EngineOptions{UnsafeNoSandboxForTests: os.Geteuid() == 0, StartupDiagnosticsForTests: diagnostics}})
	ctx := context.Background()
	state := f.service.Runtime("serverbrowser").State
	_ = state.Delete(ctx, "apps", "panel")
	status := call(t, f, f.admin, "browser_status", map[string]any{})
	if bytes.Contains(encoded(status), []byte(`"available":true`)) {
		t.Fatal("browser enabled by default")
	}
	if _, err := f.service.Execute(ctx, f.admin, "browser_open", encoded(map[string]any{})); core.ErrorStatus(err) != 409 {
		t.Fatalf("uninstalled browser opened: %v", err)
	}
	restricted, _ := createActor(t, f, []string{Scope}, []string{Resource})
	if _, err := f.service.Execute(ctx, restricted, "browser_app_change", encoded(appChange{ID: "panel", Operation: "install"})); core.ErrorStatus(err) != 403 {
		t.Fatalf("use-only identity installed app: %v", err)
	}
	if _, reason := availableEngine(f.browser.options.Engine); reason != "" {
		t.Skip(reason)
	}
	waitAppTask(t, f, call(t, f, f.admin, "browser_app_change", appChange{ID: "panel", Operation: "install"}))
	if !f.browser.installed(ctx, "panel") || f.browser.installed(ctx, "firefox-wasm") {
		t.Fatal("independent install state lost")
	}
	if _, err := f.service.Execute(ctx, f.admin, "browser_app_change", encoded(appChange{ID: "panel", Operation: "uninstall", ExpectedRevision: 0})); core.ErrorStatus(err) != 409 {
		t.Fatalf("stale revision allowed: %v", err)
	}
	session := readSession(t, call(t, f, f.admin, "browser_open", map[string]any{}))
	waitAppTask(t, f, call(t, f, f.admin, "browser_app_change", appChange{ID: "panel", Operation: "uninstall", ExpectedRevision: 1}))
	if f.browser.installed(ctx, "panel") {
		t.Fatal("uninstalled browser remains installed")
	}
	if _, err := f.browser.get(f.admin, session.ID); core.ErrorStatus(err) != 404 {
		t.Fatalf("uninstall retained session: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.browser.options.AppsDir, "panel")); !os.IsNotExist(err) {
		t.Fatalf("uninstall retained app files: %v", err)
	}
}
func TestBrowserArchiveIntegrityAndTraversal(t *testing.T) {
	makeArchive := func(name, body string) []byte {
		var b bytes.Buffer
		g := gzip.NewWriter(&b)
		tr := tar.NewWriter(g)
		_ = tr.WriteHeader(&tar.Header{Name: name, Size: int64(len(body)), Mode: 0600})
		_, _ = tr.Write([]byte(body))
		_ = tr.Close()
		_ = g.Close()
		return b.Bytes()
	}
	for _, test := range []struct {
		name            string
		digestOK, valid bool
	}{{"dist/test.txt", true, true}, {"../escape", true, false}, {"/absolute", true, false}, {"dist/test.txt", false, false}} {
		data := makeArchive(test.name, "fixture")
		hash := sha256.Sum256(data)
		digest := hex.EncodeToString(hash[:])
		if !test.digestOK {
			digest = strings.Repeat("0", 64)
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) }))
		err := fetchArchive(context.Background(), server.URL, digest, t.TempDir(), false)
		server.Close()
		if (err == nil) != test.valid {
			t.Fatalf("archive %q valid=%v: %v", test.name, test.valid, err)
		}
	}
}
func TestWispRequiresInstallAndAuthorizationAndClosesOnRevocation(t *testing.T) {
	f := setupProvider(t, Options{})
	server := httptest.NewServer(httpapi.NewWithOptions(f.service, httpapi.Options{ResourceHandler: f.browser}))
	defer server.Close()
	actor, token := createActor(t, f, []string{Scope}, []string{Resource})
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/browser/firefox-wasm/wisp/"
	dial := func(rawToken string) (*websocket.Conn, *http.Response, error) {
		return websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + rawToken}, "Origin": []string{server.URL}}})
	}
	c, response, err := dial(token.Token)
	if err == nil {
		c.CloseNow()
		t.Fatal("uninstalled proxy was accessible")
	}
	if response.StatusCode != 409 {
		t.Fatal(response.StatusCode)
	}
	_ = f.service.Runtime("serverbrowser").State.Save(context.Background(), "apps", "firefox-wasm", encoded(browserApp{ID: "firefox-wasm", Installed: true}))
	c, response, err = dial("invalid")
	if err == nil {
		c.CloseNow()
		t.Fatal("unauthenticated Wisp accepted")
	}
	if response.StatusCode != 401 {
		t.Fatal(response.StatusCode)
	}
	c, _, err = dial(token.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	_, packet, err := c.Read(ctx)
	if err != nil || len(packet) != 9 || packet[0] != 3 || binary.LittleEndian.Uint32(packet[5:]) != wispWindow {
		t.Fatalf("Wisp greeting: %x %v", packet, err)
	}
	website := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "server-network-proof") }))
	defer website.Close()
	u, _ := url.Parse(website.URL)
	port, _ := strconv.Atoi(u.Port())
	payload := []byte{1, byte(port), byte(port >> 8)}
	payload = append(payload, []byte(u.Hostname())...)
	write := func(kind byte, id uint32, body []byte) {
		packet := make([]byte, 5)
		packet[0] = kind
		binary.LittleEndian.PutUint32(packet[1:], id)
		packet = append(packet, body...)
		if err := c.Write(ctx, websocket.MessageBinary, packet); err != nil {
			t.Fatal(err)
		}
	}
	write(1, 1, payload)
	write(2, 1, []byte("GET / HTTP/1.1\r\nHost: "+u.Host+"\r\nConnection: close\r\n\r\n"))
	var received []byte
	for !bytes.Contains(received, []byte("server-network-proof")) {
		_, packet, err = c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if packet[0] == 2 {
			received = append(received, packet[5:]...)
		}
	}

	// A closed TCP stream may still have DATA in flight. Neither that DATA nor
	// reuse of a native socket ID may tear down the multiplexed connection.
	for {
		_, packet, err = c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if packet[0] == 4 && binary.LittleEndian.Uint32(packet[1:5]) == 1 {
			break
		}
	}
	write(2, 1, []byte("late data"))
	write(1, 1, payload)
	write(2, 1, []byte("GET / HTTP/1.1\r\nHost: "+u.Host+"\r\nConnection: close\r\n\r\n"))
	received = nil
	for !bytes.Contains(received, []byte("server-network-proof")) {
		_, packet, err = c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if packet[0] == 2 {
			received = append(received, packet[5:]...)
		}
	}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	go func() {
		buf := make([]byte, 64)
		n, peer, e := udp.ReadFromUDP(buf)
		if e == nil {
			_, _ = udp.WriteToUDP(buf[:n], peer)
		}
	}()
	udpPort := udp.LocalAddr().(*net.UDPAddr).Port
	udpPayload := append([]byte{2, byte(udpPort), byte(udpPort >> 8)}, []byte("127.0.0.1")...)
	write(1, 2, udpPayload)
	write(2, 2, []byte("wisp-udp-proof"))
	for {
		_, packet, err = c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if packet[0] == 2 && binary.LittleEndian.Uint32(packet[1:5]) == 2 {
			if string(packet[5:]) != "wisp-udp-proof" {
				t.Fatal("bad UDP response")
			}
			break
		}
	}
	if actor.ID == "" {
		t.Fatal("missing actor")
	}
	if err = f.service.RevokeToken(context.Background(), f.admin, token.ID); err != nil {
		t.Fatal(err)
	}
	for {
		_, _, err = c.Read(ctx)
		if err != nil {
			break
		}
	}
	if ctx.Err() != nil {
		t.Fatal("revoked Wisp remained open until test timeout")
	}
}

func TestCanceledQueuedAppChangeCanBeRetried(t *testing.T) {
	f := setupProvider(t, Options{Engine: EngineOptions{UnsafeNoSandboxForTests: os.Geteuid() == 0}})
	ctx := context.Background()
	// Occupy the installer without starting a download. A waiting task must be
	// cancelable, and its persisted reservation must not block later retries.
	f.browser.appWork <- struct{}{}
	defer func() { <-f.browser.appWork }()
	submitted := call(t, f, f.admin, "browser_app_change", appChange{ID: "panel", Operation: "install"})
	var task struct {
		ID string `json:"task_id"`
	}
	_ = json.Unmarshal(encoded(submitted), &task)
	if _, err := f.service.Runtime("serverbrowser").Tasks.Cancel(ctx, f.admin, task.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := f.service.Runtime("serverbrowser").Tasks.Get(ctx, f.admin, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current["status"] == "canceled" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	next := call(t, f, f.admin, "browser_app_change", appChange{ID: "panel", Operation: "install", ExpectedRevision: 1})
	_ = json.Unmarshal(encoded(next), &task)
	if _, err := f.service.Runtime("serverbrowser").Tasks.Cancel(ctx, f.admin, task.ID); err != nil {
		t.Fatal(err)
	}
}
