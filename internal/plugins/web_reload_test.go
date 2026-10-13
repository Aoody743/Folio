package plugins

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestWebReloadGenerationRejectsHealthyOldCycleAndRedirect(t *testing.T) {
	old := strings.Repeat("a", 32)
	requested := strings.Repeat("b", 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(webGenerationHeader, old)
		_, _ = w.Write([]byte("PowerLinux OpenResty\n"))
	}))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	if err := probeWebGeneration(context.Background(), address, requested); core.ErrorCode(err) != "reload_not_applied" {
		t.Fatal("healthy old generation acknowledged new configuration", err)
	}
	if err := probeWebGeneration(context.Background(), address, old); err != nil {
		t.Fatal("actual exact generation was rejected", err)
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, server.URL, http.StatusFound) }))
	defer redirect.Close()
	if err := probeWebGeneration(context.Background(), strings.TrimPrefix(redirect.URL, "http://"), old); core.ErrorCode(err) != "reload_not_applied" {
		t.Fatal("unowned redirected responder acknowledged configuration", err)
	}
}

func TestWebMainGenerationOnlyAcceptsExactOwnedFactory(t *testing.T) {
	e := testAgent(t)
	instance := Instance{ID: "openresty-web", PluginID: "openresty", Paths: e.instancePaths("openresty", "web"), Config: defaultConfig("openresty")}
	for _, generation := range []string{"", strings.Repeat("e", 32)} {
		body := []byte(e.renderOpenRestyConfiguration(instance, generation))
		actual, err := e.webGeneration(instance, body)
		if err != nil || actual != generation {
			t.Fatal("owned old/new template was not recognized", actual, err)
		}
		body = append(body, []byte("# manually changed main\n")...)
		if _, err := e.webGeneration(instance, body); core.ErrorCode(err) != "runtime_profile_unknown" {
			t.Fatal("manual main was accepted for overwrite", err)
		}
	}
}
