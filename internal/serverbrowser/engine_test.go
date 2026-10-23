package serverbrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestEngineRealBrowserNavigationInputScreenshotAndCleanup(t *testing.T) {
	opts := EngineOptions{UnsafeNoSandboxForTests: true}
	if path, _ := availableEngine(opts); path == "" {
		t.Skip("Chromium not installed")
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/one" {
			_, _ = w.Write([]byte(`<html><head><title>Server browser one</title></head><body><input id="typing" style="position:absolute;left:20px;top:20px;width:500px;height:30px"><button style="position:absolute;left:20px;top:80px" onclick="document.title='Typed '+document.querySelector('input').value">Submit</button></body></html>`))
			return
		}
		_, _ = w.Write([]byte(`<html><head><title>Server browser two</title></head><body>Second real server page</body></html>`))
	}))
	defer origin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	e, err := startEngine(ctx, opts, 640, 480)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	profile := e.profile
	tabs, err := e.Tabs(ctx)
	if err != nil || len(tabs) != 1 {
		t.Fatalf("initial browser tabs: %v %v", tabs, err)
	}
	tab := tabs[0]
	if err = e.Navigate(ctx, tab.ID, origin.URL+"/one"); err != nil {
		t.Fatal(err)
	}
	waitTitle(t, ctx, e, tab.ID, "Server browser one")
	for _, in := range []Input{
		{Kind: "mouse", Event: "mousePressed", X: 50, Y: 35, Button: "left"},
		{Kind: "mouse", Event: "mouseReleased", X: 50, Y: 35, Button: "left"},
		{Kind: "text", Text: "服务器 Chinese input"},
		{Kind: "mouse", Event: "mousePressed", X: 50, Y: 88, Button: "left"},
		{Kind: "mouse", Event: "mouseReleased", X: 50, Y: 88, Button: "left"},
	} {
		if err = e.Input(ctx, tab.ID, in); err != nil {
			t.Fatal(err)
		}
	}
	waitTitle(t, ctx, e, tab.ID, "Typed 服务器 Chinese input")
	image, err := e.Frame(ctx, tab.ID)
	if err != nil {
		t.Fatal(err)
	}
	size, err := jpeg.DecodeConfig(bytes.NewReader(image))
	if err != nil || size.Width != 640 || size.Height != 480 {
		t.Fatalf("real screenshot size=%v err=%v", size, err)
	}
	if err = e.Navigate(ctx, tab.ID, origin.URL+"/two"); err != nil {
		t.Fatal(err)
	}
	waitTitle(t, ctx, e, tab.ID, "Server browser two")
	if err = e.History(ctx, tab.ID, "back"); err != nil {
		t.Fatal(err)
	}
	waitTitle(t, ctx, e, tab.ID, "Typed 服务器 Chinese input")
	if err = e.History(ctx, tab.ID, "forward"); err != nil {
		t.Fatal(err)
	}
	waitTitle(t, ctx, e, tab.ID, "Server browser two")
	extra, err := e.NewTab(ctx, origin.URL+"/one", 800, 600)
	if err != nil {
		t.Fatal(err)
	}
	waitTitle(t, ctx, e, extra.ID, "Server browser one")
	if err = e.CloseTab(ctx, extra.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Frame(ctx, extra.ID); err == nil {
		t.Fatal("closed tab still readable")
	}
	for _, address := range []string{"file:///etc/passwd", "chrome://settings", "javascript:alert(1)", "data:text/html,hello", "https://username:password@example.test/"} {
		if e.Navigate(ctx, tab.ID, address) == nil {
			t.Fatalf("unsafe navigation accepted: %s", address)
		}
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(profile); !os.IsNotExist(err) {
		t.Fatalf("browser profile retained after close: %v", err)
	}
	if _, err = e.Tabs(ctx); err == nil {
		t.Fatal("closed browser still readable")
	}
}

func waitTitle(t *testing.T, ctx context.Context, e *engine, id, want string) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		tabs, err := e.Tabs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, tab := range tabs {
			if tab.ID == id && tab.Title == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	tabs, _ := e.Tabs(ctx)
	t.Fatalf("tab title did not become %q: %+v", want, tabs)
}

func TestEngineURLAndCDPFraming(t *testing.T) {
	for _, item := range []struct {
		address           string
		document, allowed bool
	}{
		{"about:blank", true, true}, {"https://example.com/path", true, true}, {"http://127.0.0.1:9000", true, true},
		{"file:///etc/shadow", false, false}, {"data:text/html,abc", true, false}, {"data:image/png;base64,AAA", false, true},
		{"https://a:b@example.com", true, false}, {"javascript:alert(1)", true, false}, {"http:", true, false},
	} {
		if actual := allowedNetworkURL(item.address, item.document); actual != item.allowed {
			t.Fatalf("%q document=%v allowed=%v", item.address, item.document, actual)
		}
	}
	raw := []byte(`{"id":1}` + "\x00" + `{"id":2}` + "\x00")
	n, token, err := splitNUL(raw, false)
	if err != nil || n != 9 || !json.Valid(token) {
		t.Fatalf("CDP frame split %d %q %v", n, token, err)
	}
	if _, _, err = splitNUL([]byte("unterminated"), true); err == nil {
		t.Fatal("accepted truncated CDP frame")
	}
}

func TestEngineProductionSandboxStartup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("production engine deliberately refuses root")
	}
	if executable, _ := availableEngine(EngineOptions{}); executable == "" {
		t.Skip("Chromium not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	e, err := startEngine(ctx, EngineOptions{}, 640, 480)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, arg := range e.cmd.Args {
		if arg == "--no-sandbox" || arg == "--ignore-certificate-errors" {
			t.Fatalf("production security override: %s", arg)
		}
	}
	tabs, err := e.Tabs(ctx)
	if err != nil || len(tabs) != 1 {
		t.Fatalf("sandboxed browser: %v %v", tabs, err)
	}
}
