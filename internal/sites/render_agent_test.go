package sites

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

func qaRenderSite() Site {
	return Site{ID: "qa-site", Name: "QA website", Domains: []string{"example.net"}, WebInstance: "owned-web", Status: "running", RunPath: ".", Index: []string{"index.html"}, AccessLog: true}
}
func qaRenderPaths() renderedPaths {
	return renderedPaths{DocumentRoot: "/srv/qa/public", LogDir: "/srv/qa/logs", RunDir: "/srv/qa/run", TempDir: "/srv/qa/tmp", Certificate: "/var/lib/qa/cert.crt", PrivateKey: "/var/lib/qa/cert.key", PasswordFile: "/srv/qa/auth/passwords"}
}

// Parse actual server blocks, including nested location blocks, so tests
// compare listener/name/root semantics across blocks rather than an entire
// string copied from the renderer. This helper is not an Nginx config check.
func qaServerBlocks(t *testing.T, config string) []string {
	t.Helper()
	var blocks []string
	for start := 0; ; {
		at := strings.Index(config[start:], "server {\n")
		if at < 0 {
			break
		}
		at += start
		depth, quote, escaped, end := 0, byte(0), false, -1
		for i := at + len("server "); i < len(config); i++ {
			c := config[i]
			if escaped {
				escaped = false
				continue
			}
			if quote != 0 {
				if c == '\\' {
					escaped = true
				} else if c == quote {
					quote = 0
				}
				continue
			}
			if c == '"' || c == '\'' {
				quote = c
				continue
			}
			if c == '{' {
				depth++
			} else if c == '}' {
				depth--
				if depth == 0 {
					end = i + 1
					break
				}
			}
		}
		if end < 0 {
			t.Fatal("rendered server block is unbalanced")
		}
		blocks = append(blocks, config[at:end])
		start = end
	}
	return blocks
}

func TestQARenderIDNAIPv6TLSBindingsHaveUniqueHTTPSNames(t *testing.T) {
	site := qaRenderSite()
	site.Domains = []string{"BÜCHER.example:80", "secondary.example:8080", "[2001:db8::1]:8080"}
	site.Default, site.IPv6 = true, true
	site.TLS = TLSBinding{Enabled: true, CertificateID: "qa-cert", Redirect: true}
	site.DirectoryBindings = []DirectoryBinding{{ID: "subdir", Path: "blog", Domains: []string{"blog.example:80", "blog.example:8080"}}}
	config, err := renderWebsite(site, qaRenderPaths(), WebRuntime{MainConfig: "/opt/web/nginx.conf"}, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	blocks := qaServerBlocks(t, config)
	if len(blocks) != 6 {
		t.Fatalf("actual grouped HTTP/TLS server count %d, want 6", len(blocks))
	}
	var tlsNames []string
	defaultListeners := map[string]int{}
	for _, block := range blocks {
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "listen ") && strings.Contains(line, "default_server") {
				defaultListeners[strings.Fields(line)[1]]++
			}
		}
		if strings.Contains(block, "listen 443 ssl") {
			line := regexp.MustCompile(`(?m)^\s*server_name ([^;]+);`).FindStringSubmatch(block)
			if len(line) != 2 {
				t.Fatal("HTTPS block lacks actual server names")
			}
			tlsNames = append(tlsNames, strings.Fields(line[1])...)
			if strings.Contains(line[1], "blog.example") && !strings.Contains(block, `root "/srv/qa/public/blog";`) {
				t.Fatal("TLS directory binding lost its own document root")
			}
		}
	}
	seen := map[string]bool{}
	for _, name := range tlsNames {
		if seen[name] {
			t.Fatalf("duplicate HTTPS binding for %s", name)
		}
		seen[name] = true
	}
	for _, name := range []string{"xn--bcher-kva.example", "secondary.example", "[2001:db8::1]", "blog.example"} {
		if !seen[name] {
			t.Fatalf("missing canonical HTTPS name %s", name)
		}
	}
	for listener, count := range defaultListeners {
		if count != 1 {
			t.Fatalf("default listener %s emitted %d times", listener, count)
		}
	}
	if !strings.Contains(config, "listen [::]:443 ssl") {
		t.Fatal("IPv6 TLS listener missing")
	}
}

func TestQARenderRejectsUntrustedNginxAndPHPInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Site)
	}{
		{"domain-directive", func(s *Site) { s.Domains = []string{"example.net; include /etc/passwd"} }},
		{"IDNA-duplicate", func(s *Site) { s.Domains = []string{"BÜCHER.example", "xn--bcher-kva.example"} }},
		{"relative-escape", func(s *Site) { s.RunPath = "../../private" }},
		{"index-directive", func(s *Site) { s.Index = []string{"index.html; include"} }},
		{"raw-rewrite-config", func(s *Site) { s.Rewrite = "include /etc/passwd;" }},
		{"rewrite-breakout", func(s *Site) { s.Rewrite = "return 200; } server { listen 9;" }},
		{"proxy-header-breakout", func(s *Site) {
			s.Proxies = []Proxy{{ID: "p", Path: "/", Upstream: "http://127.0.0.1:8081", Enabled: true, Headers: map[string]string{"X-QA": "safe\ninclude /etc/passwd;"}}}
		}},
		{"proxy-URI-breakout", func(s *Site) {
			s.Proxies = []Proxy{{ID: "p", Path: "/", Upstream: "http://localhost/;include", Enabled: true}}
		}},
		{"duplicate-route", func(s *Site) {
			s.Proxies = []Proxy{{ID: "p", Path: "/same", Upstream: "http://localhost", Enabled: true}}
			s.Redirects = []Redirect{{ID: "r", Path: "/same", Destination: "https://example.net", Status: 308, Enabled: true}}
		}},
		{"binding-escape", func(s *Site) {
			s.DirectoryBindings = []DirectoryBinding{{ID: "b", Path: "../private", Domains: []string{"blog.example"}}}
		}},
		{"PHP-line-breakout", func(s *Site) { s.PHP.Values = map[string]string{"memory_limit": "128M\nuser=root"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			site := qaRenderSite()
			test.mutate(&site)
			if _, err := renderWebsite(site, qaRenderPaths(), WebRuntime{MainConfig: "/opt/web/nginx.conf"}, "", time.Now()); err == nil {
				t.Fatal("untrusted input produced configuration")
			}
		})
	}
	// Ordinary rewrites remain functional without accepting unrelated global
	// server directives. The values here are inputs, not an output template.
	site := qaRenderSite()
	site.Rewrite = "try_files $uri $uri/ /index.php?$query_string;"
	if _, err := renderWebsite(site, qaRenderPaths(), WebRuntime{MainConfig: "/opt/web/nginx.conf"}, "", time.Now()); err != nil {
		t.Fatal("safe ordinary rewrite rejected", err)
	}
}

func TestQARenderFPMUsesIsolatedIdentityAndTypedDirectoryBounds(t *testing.T) {
	site := qaRenderSite()
	site.PHPInstance = "owned-php"
	site.PHP = PHPSettings{OpenBasedir: true, Values: map[string]string{"memory_limit": "128M"}}
	worker := Identity{User: "plsite_qa", Group: "plsite_qa", UID: 1001, GID: 1001}
	web := WebRuntime{WorkerUser: "qa-web", WorkerGroup: "qa-web"}
	pool, err := renderPool(site, qaRenderPaths(), worker, web)
	if err != nil {
		t.Fatal(err)
	}
	directives := map[string]string{}
	for _, line := range strings.Split(pool, "\n") {
		if key, value, ok := strings.Cut(line, " = "); ok {
			directives[key] = value
		}
	}
	if directives["user"] != worker.User || directives["group"] != worker.Group || directives["listen.owner"] != web.WorkerUser || directives["listen.group"] != web.WorkerGroup {
		t.Fatal("FPM pool identity or socket ownership incorrect")
	}
	if directives["clear_env"] != "yes" || directives["security.limit_extensions"] != ".php" {
		t.Fatal("FPM environment/extension bounds absent")
	}
	if directives["php_admin_value[open_basedir]"] != `"/srv/qa/public:/srv/qa/tmp"` {
		t.Fatal("FPM open_basedir escaped isolated public/temp roots")
	}
	worker.UID = 0
	if _, err := renderPool(site, qaRenderPaths(), worker, web); err == nil {
		t.Fatal("root PHP worker accepted")
	}
	worker.UID = 1001
	site.PHP.Values["open_basedir"] = "/"
	if _, err := renderPool(site, qaRenderPaths(), worker, web); err == nil {
		t.Fatal("arbitrary PHP setting replaced protected open_basedir")
	}
}

func qaOwnedRootVM(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 || os.Getenv("POWERLINUX_QA_OWNED_VM") != "1" {
		t.Skip("root-only integration: execute test binary in owned systemd QEMU VM; host pass is not VM evidence")
	}
	pid1, err := os.ReadFile("/proc/1/comm")
	if err != nil || strings.TrimSpace(string(pid1)) != "systemd" {
		t.Fatal("root test requires our actual systemd VM")
	}
}

type qaRuntimeFixture struct {
	web                   WebRuntime
	php                   PHPRuntime
	checks, reloads       int
	failCheck, failReload bool
}

func (r *qaRuntimeFixture) Web(context.Context, string) (WebRuntime, error) { return r.web, nil }
func (r *qaRuntimeFixture) PHP(context.Context, string) (PHPRuntime, error) { return r.php, nil }
func (r *qaRuntimeFixture) Check(context.Context, string) error {
	r.checks++
	if r.failCheck {
		r.failCheck = false
		return errors.New("intentional fixture configuration rejection")
	}
	return nil
}
func (r *qaRuntimeFixture) Reload(context.Context, string) error {
	r.reloads++
	if r.failReload {
		r.failReload = false
		return errors.New("intentional fixture reload rejection")
	}
	return nil
}

// RecoverRuntime models an already-running test runtime. Production composition
// uses plugins.PrivilegedExecutor.RecoverRuntime, which never starts a stopped
// service; this fixture records the revalidation/reload performed in tests.
func (r *qaRuntimeFixture) RecoverRuntime(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.Reload(ctx, id)
}

func qaRootExecutor(t *testing.T) (*AgentExecutor, *qaRuntimeFixture) {
	t.Helper()
	qaOwnedRootVM(t)
	base := t.TempDir()
	webPrefix := filepath.Join(base, "web")
	phpPrefix := filepath.Join(base, "php")
	for _, dir := range []string{filepath.Join(webPrefix, "conf.d"), filepath.Join(phpPrefix, "pool.d")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	runtime := &qaRuntimeFixture{web: WebRuntime{ID: "owned-web", Program: "/usr/bin/true", MainConfig: filepath.Join(webPrefix, "nginx.conf"), SnippetsDir: filepath.Join(webPrefix, "conf.d"), Prefix: webPrefix, WorkerUser: "nobody", WorkerGroup: "nogroup"}, php: PHPRuntime{ID: "owned-php", Program: "/usr/bin/true", MainConfig: filepath.Join(phpPrefix, "php-fpm.conf"), PoolDir: filepath.Join(phpPrefix, "pool.d"), Prefix: phpPrefix}}
	for _, file := range []string{runtime.web.MainConfig, runtime.php.MainConfig} {
		if err := os.WriteFile(file, []byte("QA trusted configuration\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	e, err := NewAgentExecutor(AgentConfig{Version: 1, RootDir: filepath.Join(base, "content"), StateDir: filepath.Join(base, "state"), Programs: map[string]string{"useradd": "/usr/sbin/useradd", "userdel": "/usr/sbin/userdel"}, AllowedPorts: []int{80, 443, 8080}, AllowIPv6: true}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e, runtime
}

func qaAgentRun(ctx context.Context, e *AgentExecutor, operation string, in request) (agent.Result, error) {
	if in.Actor.ID == "" {
		in.Actor = core.Actor{ID: "qa-sites-human", Kind: "human", Name: "QA"}
	}
	if operation == "certificate_acme_order" && (in.Kind == "confirm" || in.Kind == "cancel") && in.OrderRef == "" {
		// Snapshot when this fixture action is issued, like the public API.
		// Missing/stale-ref negatives call e.Run directly to test its guard.
		state, err := e.getACMEOrder(in.ID)
		if err != nil {
			return agent.Result{}, err
		}
		in.OrderRef = acmeOrderRef(state)
	}
	return e.Run(ctx, agent.Request{ActorID: in.Actor.ID, Operation: operation, Input: encode(in)})
}

func TestQARootVMWebsiteFailureRestoresActualConfigAuthAndReceipt(t *testing.T) {
	e, runtime := qaRootExecutor(t)
	var entropy [10]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		t.Fatal(err)
	}
	site := qaRenderSite()
	site.ID = "qa-" + hex.EncodeToString(entropy[:])
	site.Default = true
	site.Protection = Protection{Enabled: true, Username: "qa-http-user"}
	workerName := "plsite_" + siteTag(site.ID)
	if _, err := user.Lookup(workerName); err == nil {
		t.Fatal("QA site identity already exists; refuse to claim ownership")
	}
	t.Cleanup(func() {
		if _, err := user.Lookup(workerName); err == nil {
			if out, err := exec.Command("/usr/sbin/userdel", workerName).CombinedOutput(); err != nil {
				t.Errorf("owned QA worker cleanup failed: %v (%d diagnostic bytes)", err, len(out))
			}
		}
	})
	first, err := qaAgentRun(context.Background(), e, "site_apply", request{ID: site.ID, Site: &site, Password: "QA initial protected auth"})
	if err != nil {
		t.Fatal(err)
	}
	var saved Site
	if json.Unmarshal(first.Data, &saved) != nil || saved.Revision != 1 {
		t.Fatal("real website receipt did not commit revision one")
	}
	confPath := filepath.Join(runtime.web.SnippetsDir, site.ID+".conf")
	authPath := e.paths(site).PasswordFile
	oldConf, _ := os.ReadFile(confPath)
	oldAuth, _ := os.ReadFile(authPath)
	oldInfo, err := os.Stat(authPath)
	if err != nil {
		t.Fatal(err)
	}
	oldStat := oldInfo.Sys().(*syscall.Stat_t)
	for _, failure := range []string{"check", "reload"} {
		t.Run(failure, func(t *testing.T) {
			changed := saved
			changed.Name = "should never commit " + failure
			changed.Index = []string{"changed.html"}
			if failure == "check" {
				runtime.failCheck = true
			} else {
				runtime.failReload = true
			}
			_, err := qaAgentRun(context.Background(), e, "site_apply", request{ID: site.ID, Site: &changed, ExpectedRevision: 1, Password: "QA replacement should roll back"})
			if err == nil {
				t.Fatal("fixture runtime failure was reported as success")
			}
			actualConf, _ := os.ReadFile(confPath)
			actualAuth, _ := os.ReadFile(authPath)
			actualInfo, err := os.Stat(authPath)
			if err != nil {
				t.Fatal(err)
			}
			actualStat := actualInfo.Sys().(*syscall.Stat_t)
			if string(actualConf) != string(oldConf) || string(actualAuth) != string(oldAuth) || actualInfo.Mode() != oldInfo.Mode() || actualStat.Uid != oldStat.Uid || actualStat.Gid != oldStat.Gid {
				t.Fatal("actual configuration/auth bytes or ownership/mode not restored")
			}
			receipt, err := e.get(site.ID)
			if err != nil || receipt.Revision != 1 || receipt.Name != saved.Name {
				t.Fatal("failed transaction changed committed receipt", err)
			}
		})
	}
	if runtime.checks < 3 || runtime.reloads < 2 {
		t.Fatal("fixture did not reach configuration/reload failure boundaries")
	}
	conflict := qaRenderSite()
	conflict.ID = "qa-default-conflict"
	conflict.Domains = []string{"different.example"}
	conflict.Default = true
	_, err = qaAgentRun(context.Background(), e, "site_apply", request{ID: conflict.ID, Site: &conflict})
	if core.ErrorCode(err) != "default_site_conflict" {
		t.Fatal("another default website was accepted", err)
	}
	// Runtimes are explicit check/reload fixtures; this verifies real protected
	// filesystem rollback, not actual Nginx/OpenResty or PHP-FPM execution.
}
