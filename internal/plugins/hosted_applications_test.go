package plugins

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostedAppIdentityAndPaths(t *testing.T) {
	for _, id := range []string{"web", "next_1", "go-api"} {
		if !hostedAppValidID(id) {
			t.Fatalf("valid application id rejected: %q", id)
		}
	}
	for _, id := range []string{"", "../app", "Upper", "-bad", "a" + strings.Repeat("b", 32)} {
		if hostedAppValidID(id) {
			t.Fatalf("invalid application id accepted: %q", id)
		}
	}
	if !pathsOverlap("/srv/apps", "/srv/apps/demo") || pathsOverlap("/srv/apps", "/srv/other") {
		t.Fatal("protected path overlap check returned an unexpected result")
	}
}

func TestHostedAppTemplatesEscapeNameAndProduceValidMetadata(t *testing.T) {
	name := `hello"</script><script>alert(1)</script>`
	input := hostedAppCreateInput{ID: "demo", Name: name, Runtime: "go"}
	files := hostedAppTemplates(input)
	if _, err := parser.ParseFile(token.NewFileSet(), "main.go", files["main.go"], parser.AllErrors); err != nil {
		t.Fatalf("Go template is not valid source: %v", err)
	}
	if !strings.Contains(string(files["main.go"]), `\"`) || strings.Contains(string(files["main.go"]), `Fprintln(w, "`+name+`"`) {
		t.Fatal("Go template did not encode the application name as a string literal")
	}
	if !strings.Contains(string(files["main.go"]), `127.0.0.1`) {
		t.Fatal("Go template must bind its listener to loopback only")
	}

	input.Runtime = "nextjs"
	files = hostedAppTemplates(input)
	var manifest struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(files["package.json"], &manifest); err != nil {
		t.Fatalf("Next.js package manifest is invalid JSON: %v", err)
	}
	if manifest.Dependencies["next"] == "" || manifest.Dependencies["react"] == "" || manifest.Dependencies["react-dom"] == "" {
		t.Fatal("Next.js template is missing runtime dependencies")
	}
	if strings.Contains(string(files["app/page.js"]), name) {
		t.Fatal("Next.js template interpolated raw user input into JavaScript")
	}
}

func TestHostedAppSystemdUnitUsesOwnedLoopbackService(t *testing.T) {
	receipt := hostedAppReceipt{
		HostedApplication: HostedApplication{ID: "demo", Runtime: "go", Port: 43127, User: "plapp_1234567890abcdef"},
		RuntimeProgram:    "/usr/bin/go",
		ProjectDir:        filepath.Join(string(filepath.Separator), "srv", "powerlinux-apps", "demo", "project"),
		DataDir:           filepath.Join(string(filepath.Separator), "srv", "powerlinux-apps", "demo", "data"),
	}
	unit := string(hostedAppUnit(receipt))
	for _, expected := range []string{"User=plapp_1234567890abcdef", "NoNewPrivileges=true", "ProtectSystem=strict", "Environment=DATA_DIR=", "powerlinux-app"} {
		if !strings.Contains(unit, expected) {
			t.Fatalf("generated app unit does not include %q: %s", expected, unit)
		}
	}
	if strings.Contains(unit, "User=root") || strings.Contains(unit, "0.0.0.0") {
		t.Fatalf("generated app unit has unsafe service binding: %s", unit)
	}
}
