package plugins

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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

func TestHostedAppRootAllowsTraversalWithoutListing(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("application root metadata repair requires root")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	receipt := hostedAppReceipt{HostedApplication: HostedApplication{ID: "qa-app"}, Root: root}
	if err := ensureHostedAppRootTraversable(receipt); err != nil {
		t.Fatal("protected application root was not made traversable", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0711 {
		t.Fatalf("application root must allow traversal without listing or writes: mode %04o", info.Mode().Perm())
	}
}

func TestHostedAppGitSourceMetadataRetainsOnlyPinnedPublicIdentity(t *testing.T) {
	commit := strings.Repeat("a", 40)
	if !validHostedAppGitSourceMetadata("https://github.com/owner/repo.git", "release/v1", commit) {
		t.Fatal("valid public HTTPS Git source metadata was rejected")
	}
	if !validHostedAppGitSourceMetadata("ssh://git@github.com/owner/repo.git", "main", commit) {
		t.Fatal("valid pinned SSH repository identity was rejected")
	}
	for _, source := range []struct{ repository, branch, commit string }{
		{"https://user:token@example.com/owner/repo.git", "main", commit},
		{"https://github.com/owner/repo.git", "main", "not-a-commit"},
		{"https://github.com/owner/repo.git?token=secret", "main", commit},
	} {
		if validHostedAppGitSourceMetadata(source.repository, source.branch, source.commit) {
			t.Fatalf("unsafe Git source metadata was accepted: %+v", source)
		}
	}
}

func TestHostedGitEnvironmentUsesOnlyUnauthenticatedValidatedProxies(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:39813")
	t.Setenv("https_proxy", "http://qa-user:proxy-secret@example.invalid:8080")
	t.Setenv("HTTP_PROXY", "file:///etc/passwd")
	t.Setenv("http_proxy", "http://proxy.example.invalid:8080/path")
	t.Setenv("SSL_CERT_FILE", "/etc/ssl/certs/qa-bundle.pem")
	remote, err := parseHostedAppGitRemote(hostedAppGitInput{ID: "demo", ExpectedRevision: 1, RepositoryURL: "https://github.com/owner/repo.git"})
	if err != nil {
		t.Fatal(err)
	}
	environment := strings.Join(hostedGitEnvironment("/tmp/home", remote, "", ""), "\n")
	if !strings.Contains(environment, "HTTPS_PROXY=http://127.0.0.1:39813") || !strings.Contains(environment, "SSL_CERT_FILE=/etc/ssl/certs/qa-bundle.pem") {
		t.Fatal("validated proxy and certificate settings were not forwarded to Git")
	}
	for _, rejected := range []string{"proxy-secret", "qa-user", "HTTP_PROXY=file:", "http_proxy=http://proxy.example.invalid:8080/path"} {
		if strings.Contains(environment, rejected) {
			t.Fatalf("unsafe proxy setting reached Git: %q", rejected)
		}
	}
}

func TestHostedAppBuildEnvironmentUsesOnlyValidatedNetworkSettings(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:39813")
	t.Setenv("https_proxy", "http://qa-user:proxy-secret@example.invalid:8080")
	t.Setenv("HTTP_PROXY", "file:///etc/passwd")
	t.Setenv("http_proxy", "http://proxy.example.invalid:8080/path")
	t.Setenv("SSL_CERT_FILE", "/etc/ssl/certs/qa-bundle.pem")
	environment := strings.Join(hostedAppOutboundNetworkEnvironment(), "\n")
	if !strings.Contains(environment, "HTTPS_PROXY=http://127.0.0.1:39813") || !strings.Contains(environment, "SSL_CERT_FILE=/etc/ssl/certs/qa-bundle.pem") || !strings.Contains(environment, "NODE_EXTRA_CA_CERTS=/etc/ssl/certs/qa-bundle.pem") {
		t.Fatal("validated proxy and certificate settings were not forwarded to managed builds")
	}
	for _, rejected := range []string{"proxy-secret", "qa-user", "HTTP_PROXY=file:", "http_proxy=http://proxy.example.invalid:8080/path"} {
		if strings.Contains(environment, rejected) {
			t.Fatalf("unsafe network setting reached managed build: %q", rejected)
		}
	}
}

func TestHostedAppTemplatesEscapeNameAndProduceValidMetadata(t *testing.T) {
	name := `hello"</script><script>alert(1)</script>`
	input := hostedAppCreateInput{ID: "demo", Name: name, Runtime: "go"}
	files := hostedAppTemplates(input)
	if _, ok := files["go.sum"]; !ok {
		t.Fatal("Go template is missing its required checksum file")
	}
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
		Scripts      map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(files["package.json"], &manifest); err != nil {
		t.Fatalf("Next.js package manifest is invalid JSON: %v", err)
	}
	if manifest.Dependencies["next"] == "" || manifest.Dependencies["react"] == "" || manifest.Dependencies["react-dom"] == "" {
		t.Fatal("Next.js template is missing runtime dependencies")
	}
	if manifest.Scripts["build"] == "" || manifest.Scripts["start"] == "" || len(files["package-lock.json"]) == 0 {
		t.Fatal("Next.js template is missing its build/start scripts or locked dependency tree")
	}
	if strings.Contains(string(files["app/page.js"]), name) {
		t.Fatal("Next.js template interpolated raw user input into JavaScript")
	}

	input.Runtime = "nodejs"
	files = hostedAppTemplates(input)
	if err := json.Unmarshal(files["package.json"], &manifest); err != nil {
		t.Fatalf("Node.js package manifest is invalid JSON: %v", err)
	}
	if manifest.Scripts["start"] == "" || strings.Contains(string(files["server.js"]), name) {
		t.Fatal("Node.js template must provide a start script and escape the application name")
	}
	var lock struct {
		Name            string `json:"name"`
		LockfileVersion int    `json:"lockfileVersion"`
	}
	if json.Unmarshal(files["package-lock.json"], &lock) != nil || lock.Name != input.ID || lock.LockfileVersion != 3 {
		t.Fatal("Node.js template package lock does not match the application identity")
	}
	project := t.TempDir()
	for filename, body := range files {
		if err := os.WriteFile(filepath.Join(project, filename), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateHostedAppProjectManifest(project, "nodejs"); err != nil {
		t.Fatalf("generated Node.js project does not satisfy its manifest contract: %v", err)
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

func TestHostedNodeAppSystemdUnitRunsDeclaredScriptWithLoopbackEnvironment(t *testing.T) {
	receipt := hostedAppReceipt{
		HostedApplication: HostedApplication{ID: "demo", Runtime: "nodejs", Port: 43127, User: "plapp_1234567890abcdef"},
		RuntimeProgram:    "/opt/folio/node/bin/node",
		RuntimeNPMCLI:     "/opt/folio/node/lib/node_modules/npm/bin/npm-cli.js",
		ProjectDir:        filepath.Join(string(filepath.Separator), "srv", "powerlinux-apps", "demo", "project"),
		DataDir:           filepath.Join(string(filepath.Separator), "srv", "powerlinux-apps", "demo", "data"),
	}
	unit := string(hostedAppUnit(receipt))
	for _, expected := range []string{`ExecStart="/opt/folio/node/bin/node" "/opt/folio/node/lib/node_modules/npm/bin/npm-cli.js" "run" "start"`, "Environment=HOST=127.0.0.1", "Environment=HOSTNAME=127.0.0.1", "Environment=PORT=43127", "User=plapp_1234567890abcdef"} {
		if !strings.Contains(unit, expected) {
			t.Fatalf("generated Node.js unit does not include %q: %s", expected, unit)
		}
	}
	if !strings.Contains(unit, "WorkingDirectory="+receipt.ProjectDir+"\n") || strings.Contains(unit, "WorkingDirectory=\""+receipt.ProjectDir) {
		t.Fatalf("systemd working directory must be an absolute unquoted path: %s", unit)
	}
	legacy := string(hostedAppLegacyQuotedWorkingDirectoryUnit(receipt))
	if !strings.Contains(legacy, "WorkingDirectory=\""+receipt.ProjectDir+"\"\n") || !hostedAppUnitMatches(receipt, []byte(legacy)) {
		t.Fatalf("known legacy systemd unit was not recognized for safe migration: %s", legacy)
	}
}

func TestHostedAppNodeBuildSwapKeepsPreviousProjectForRollback(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected build rollback fixture requires root")
	}
	root := t.TempDir()
	project := filepath.Join(root, "project")
	stage := filepath.Join(root, ".project.build-fixture")
	for directory, version := range map[string]string{project: "old", stage: "new"} {
		if err := os.Mkdir(directory, 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "version"), []byte(version), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, ".powerlinux-node-built"), []byte("built\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(directory, 65534, 65534); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"version", ".powerlinux-node-built"} {
			if err := os.Chown(filepath.Join(directory, name), 65534, 65534); err != nil {
				t.Fatal(err)
			}
		}
	}
	receipt := hostedAppReceipt{
		HostedApplication: HostedApplication{ID: "demo", Runtime: "nodejs"},
		UID:               65534,
		GID:               65534,
		Root:              root,
		ProjectDir:        project,
	}
	stageExists := true
	executor := &PrivilegedExecutor{}
	if err := executor.commitHostedAppBuild(receipt, stage, &stageExists); err != nil {
		t.Fatalf("could not atomically switch to built project: %v", err)
	}
	if stageExists || !hostedAppProjectBuilt(project, "nodejs") {
		t.Fatal("successful build was not installed at the active project path")
	}
	previous := filepath.Join(root, ".project.build.previous")
	version, err := os.ReadFile(filepath.Join(previous, "version"))
	if err != nil || string(version) != "old" {
		t.Fatalf("previous project was not preserved: %q, %v", version, err)
	}
	if err := rollbackHostedNodeProject(receipt); err != nil {
		t.Fatalf("project rollback failed: %v", err)
	}
	version, err = os.ReadFile(filepath.Join(project, "version"))
	if err != nil || string(version) != "old" || !hostedAppProjectBuilt(project, "nodejs") {
		t.Fatalf("rollback did not restore the previous built project: %q, %v", version, err)
	}
}

func TestHostedAppReadyCheckUsesLoopbackUpstream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	_, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = waitHostedAppReady(ctx, port); err != nil {
		t.Fatalf("loopback HTTP response was not recognized as ready: %v", err)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if err = waitHostedAppReady(canceled, port); err == nil {
		t.Fatal("a canceled application readiness check succeeded")
	}
}
