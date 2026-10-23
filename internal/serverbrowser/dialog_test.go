package serverbrowser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestWebsitePromptAndNewWindowRemainOperable(t *testing.T) {
	opts := EngineOptions{UnsafeNoSandboxForTests: os.Geteuid() == 0}
	if path, _ := availableEngine(opts); path == "" {
		t.Skip("Chromium not installed")
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/popup" {
			w.Write([]byte(`<title>New window destination</title>`))
			return
		}
		w.Write([]byte(`<title>Dialog fixture</title><button style="position:absolute;left:10px;top:10px;width:200px;height:50px" onclick="document.title='Answer '+prompt('Your response','default')">Prompt</button><a style="position:absolute;left:10px;top:100px" href="/popup" target="_blank">Open new window</a>`))
	}))
	defer origin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	e, err := startEngine(ctx, opts, 640, 480)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	tabs, err := e.Tabs(ctx)
	if err != nil || len(tabs) != 1 {
		t.Fatalf("initial tabs: %+v %v", tabs, err)
	}
	id := tabs[0].ID
	if err = e.Navigate(ctx, id, origin.URL); err != nil {
		t.Fatal(err)
	}
	waitTitle(t, ctx, e, id, "Dialog fixture")
	for _, event := range []string{"mousePressed", "mouseReleased"} {
		if err = e.Input(ctx, id, Input{Kind: "mouse", Event: event, Button: "left", X: 50, Y: 35}); err != nil {
			t.Fatal("prompt input", err)
		}
	}
	tabs, err = e.Tabs(ctx)
	if err != nil || len(tabs) != 1 || tabs[0].Dialog == nil || tabs[0].Dialog.Type != "prompt" {
		t.Fatalf("prompt not observable: %+v %v", tabs, err)
	}
	if err = e.HandleDialog(ctx, id, true, "中文 reply"); err != nil {
		t.Fatal(err)
	}
	waitTitle(t, ctx, e, id, "Answer 中文 reply")
	for _, event := range []string{"mousePressed", "mouseReleased"} {
		if err = e.Input(ctx, id, Input{Kind: "mouse", Event: event, Button: "left", X: 65, Y: 108}); err != nil {
			t.Fatal("new window input", err)
		}
	}
	waitTitle(t, ctx, e, id, "New window destination")
	tabs, err = e.Tabs(ctx)
	if err != nil || len(tabs) != 1 {
		t.Fatalf("uncontrolled popup escaped tab budget: %+v %v", tabs, err)
	}
}
