package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/files"
	"github.com/Aoody743/PowerLinux/internal/httpapi"
	"github.com/Aoody743/PowerLinux/internal/mcpbridge"
	"github.com/Aoody743/PowerLinux/internal/webassets"
)

var version = "0.1.0-dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Printf("powerlinux: %v", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: powerlinux serve|init|mcp|agent|healthcheck|agent-healthcheck|recover-access|version")
	}
	switch args[0] {
	case "version", "--version":
		fmt.Println(version)
		return nil
	case "serve":
		return serve(args[1:])
	case "init":
		return initialize(args[1:])
	case "mcp":
		return stdio(args[1:])
	case "agent":
		return serveAgent(args[1:])
	case "healthcheck":
		return healthcheck(args[1:])
	case "agent-healthcheck":
		return agentHealthcheck(args[1:])
	case "recover-access":
		return recoverAccess(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func dataDefault() string {
	if value := os.Getenv("POWERLINUX_DATA_DIR"); value != "" {
		return value
	}
	if value, err := os.UserConfigDir(); err == nil {
		return filepath.Join(value, "powerlinux")
	}
	return ".powerlinux"
}

func acquireLock(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("data directory must be a real directory")
	}
	name := filepath.Join(dir, "server.lock")
	fd, err := syscall.Open(name, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("data directory is already used by a server or upgrade")
	}
	return f, nil
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	dir := flags.String("data-dir", dataDefault(), "private runtime data directory")
	address := flags.String("listen", "127.0.0.1:9000", "HTTP bind address")
	public := flags.String("public-url", "", "canonical HTTPS MCP URL ending in /mcp; loopback HTTP permitted")
	origins := flags.String("allowed-origins", "", "comma-separated exact trusted browser origins")
	cert := flags.String("tls-cert", "", "TLS certificate path")
	key := flags.String("tls-key", "", "TLS key path")
	secureCookie := flags.Bool("secure-cookies", false, "require HTTPS cookies when TLS is terminated by a trusted reverse proxy")
	allowRoot := flags.Bool("allow-root", false, "explicitly allow root for isolated testing; production API should run as powerlinux user")
	configPath := flags.String("server-config", defaultServerConfig, "root-owned server configuration; explicit CLI flags take precedence")
	agentSocket := flags.String("agent-socket", "", "optional root-owned privileged host executor socket")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected serve arguments")
	}
	explicit := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	configuration, err := loadServerConfiguration(*configPath, explicit["server-config"])
	if err != nil {
		return err
	}
	if !explicit["listen"] && configuration.Listen != "" {
		*address = configuration.Listen
	}
	if !explicit["public-url"] {
		*public = configuration.PublicURL
	}
	if !explicit["allowed-origins"] {
		*origins = strings.Join(configuration.AllowedOrigins, ",")
	}
	if !explicit["secure-cookies"] {
		*secureCookie = configuration.SecureCookies
	}
	if !explicit["tls-cert"] {
		*cert = configuration.TLSCert
	}
	if !explicit["tls-key"] {
		*key = configuration.TLSKey
	}
	if os.Geteuid() == 0 && !*allowRoot {
		return errors.New("run the API as an ordinary user; use the isolated agent for privileged actions")
	}
	if (*cert == "") != (*key == "") {
		return errors.New("TLS certificate and key must both be configured")
	}
	lock, err := acquireLock(*dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	svc, err := core.New(*dir)
	if err != nil {
		return fmt.Errorf("open data: %w", err)
	}
	var trustedAgent *agent.UnixClient
	if *agentSocket != "" {
		trustedAgent, err = agent.NewUnixClient(*agentSocket, 0)
		if err != nil {
			svc.Close()
			return err
		}
		defer trustedAgent.Close()
	}
	var remoteFiles *files.Remote
	if trustedAgent != nil {
		remoteFiles = &files.Remote{Client: trustedAgent}
	}
	fileManager, err := registerFiles(svc, *dir, remoteFiles)
	if err != nil {
		svc.Close()
		return fmt.Errorf("register managed files: %w", err)
	}
	modules, err := registerModules(svc, trustedAgent)
	if err != nil {
		svc.Close()
		fileManager.files.Close()
		return err
	}
	defer func() {
		modules.system.Close()
		modules.terminal.Close()
		svc.Close()
		fileManager.files.Close()
	}()
	trusted := []string{}
	for _, origin := range strings.Split(*origins, ",") {
		if value := strings.TrimSpace(origin); value != "" {
			trusted = append(trusted, value)
		}
	}
	mcp, err := mcpbridge.NewHTTP(svc, mcpbridge.HTTPOptions{PublicURL: *public, AllowedOrigins: trusted})
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", httpapi.NewWithOptions(svc, httpapi.Options{SecureCookies: *cert != "" || *secureCookie || strings.HasPrefix(*public, "https://"), AllowedOrigins: trusted, ResourceHandler: resourceHandlers{fileManager, modules.terminal, modules.database, modules.website, modules.apps}}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, `{"status":"ok","version":%q,"ready":true}`, version)
	})
	for _, route := range []string{"/mcp", "/oauth/", "/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource"} {
		mux.Handle(route, mcp)
	}
	mux.Handle("/.well-known/oauth-protected-resource/", mcp)
	mux.Handle("/", webassets.Handler())
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		return err
	}
	defer listener.Close()
	if *cert == "" {
		bound, ok := listener.Addr().(*net.TCPAddr)
		if !ok || !bound.IP.IsLoopback() {
			return errors.New("plaintext HTTP must bind a loopback address; configure TLS for remote access")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	entry := httpapi.NewFrontdoor(svc, httpapi.FrontdoorOptions{SecureCookies: *cert != "" || *secureCookie || strings.HasPrefix(*public, "https://"), AllowedOrigins: trusted, MCPAudience: *public}, mux)
	lifecycle := newHTTPLifecycle(ctx, entry)
	server := &http.Server{Handler: lifecycle, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	defer func() { stop(); server.Close(); lifecycle.Close() }()
	done := make(chan error, 1)
	go func() {
		if *cert != "" {
			done <- server.ServeTLS(listener, *cert, *key)
		} else {
			done <- server.Serve(listener)
		}
	}()
	log.Printf("Folio %s listening on %s", version, listener.Addr())
	if !svc.Initialized(ctx) {
		log.Printf("Administrator setup required; local bootstrap token stored at %s (private file)", svc.SetupSecretFile())
	}
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

func initialize(args []string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	dir := flags.String("data-dir", dataDefault(), "private data directory; stop API before CLI initialization")
	username := flags.String("username", "admin", "administrator name")
	passwordFile := flags.String("password-file", "", "private file containing password; otherwise read one line from stdin")
	if err := flags.Parse(args); err != nil {
		return err
	}
	lock, err := acquireLock(*dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	svc, err := core.New(*dir)
	if err != nil {
		return err
	}
	defer svc.Close()
	if svc.Initialized(context.Background()) {
		return errors.New("administrator already initialized")
	}
	var password string
	if *passwordFile != "" {
		info, err := os.Lstat(*passwordFile)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.New("password file must be a private regular file (0600)")
		}
		value, err := os.ReadFile(*passwordFile)
		if err != nil {
			return err
		}
		password = strings.TrimRight(string(value), "\r\n")
	} else {
		value, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && value == "" {
			return err
		}
		password = strings.TrimRight(value, "\r\n")
	}
	bootstrap, err := os.ReadFile(svc.SetupSecretFile())
	if err != nil {
		return err
	}
	_, err = svc.Setup(context.Background(), strings.TrimSpace(string(bootstrap)), *username, password)
	if err == nil {
		fmt.Println("Administrator initialized.")
	}
	return err
}

func stdio(args []string) error {
	flags := flag.NewFlagSet("mcp", flag.ContinueOnError)
	mode := flags.Bool("stdio", false, "serve MCP on stdin/stdout")
	endpoint := flags.String("endpoint", "http://127.0.0.1:9000", "existing protected Folio service")
	tokenFile := flags.String("token-file", "", "private file containing scoped access token")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*mode {
		return errors.New("mcp requires --stdio")
	}
	token := os.Getenv("POWERLINUX_MCP_TOKEN")
	if *tokenFile != "" {
		info, err := os.Lstat(*tokenFile)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.New("token file must be a private regular file (0600)")
		}
		value, err := os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		token = strings.TrimSpace(string(value))
	}
	if token == "" {
		return errors.New("MCP token required via POWERLINUX_MCP_TOKEN or --token-file")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return mcpbridge.RunStdio(ctx, mcpbridge.ProxyOptions{Endpoint: *endpoint, Token: token})
}
