package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/agentconfig"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/files"
	"github.com/Aoody743/PowerLinux/internal/plugins"
	"github.com/Aoody743/PowerLinux/internal/sites"
	"github.com/Aoody743/PowerLinux/internal/systemops"
	"github.com/Aoody743/PowerLinux/internal/terminal"
)

const defaultAgentSocket = "/run/powerlinux-agent/agent.sock"

type agentHealth struct {
	Status       string   `json:"status"`
	Ready        bool     `json:"ready"`
	Version      string   `json:"version"`
	PolicySHA256 string   `json:"policy_sha256"`
	Modules      []string `json:"modules"`
}
type healthExecutor struct{ health agentHealth }

type unavailableExecutor struct{}

func (unavailableExecutor) Validate(agent.Request) error {
	return &core.Error{Code: "module_unavailable", Message: "This Agent module is not enabled by its trusted policy", Status: 503}
}
func (e unavailableExecutor) Run(_ context.Context, r agent.Request) (agent.Result, error) {
	return agent.Result{}, e.Validate(r)
}

func (e healthExecutor) Validate(r agent.Request) error {
	if r.Operation != "agent_health" || r.ActorID == "" {
		return agent.ErrUnauthorized
	}
	var input struct{}
	d := json.NewDecoder(bytes.NewReader(r.Input))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF {
		return agent.ErrInvalidInput
	}
	return nil
}
func (e healthExecutor) Run(ctx context.Context, r agent.Request) (agent.Result, error) {
	if err := e.Validate(r); err != nil {
		return agent.Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return agent.Result{}, err
	}
	data, err := json.Marshal(e.health)
	now := time.Now().UTC()
	return agent.Result{Operation: r.Operation, ExitCode: 0, StartedAt: now, FinishedAt: now, Data: data}, err
}

// Runtime selection reads only the privileged software inventory.
type siteRuntimes struct {
	software *plugins.PrivilegedExecutor
	stateDir string
}

func (r siteRuntimes) Web(ctx context.Context, id string) (sites.WebRuntime, error) {
	t, err := r.software.ReadRuntimeTarget(ctx, id)
	if err != nil {
		return sites.WebRuntime{}, err
	}
	if t.NginxConf == "" || t.SitesDir == "" {
		return sites.WebRuntime{}, errors.New("selected instance is not a Web runtime")
	}
	return sites.WebRuntime{ID: id, Program: t.Instance.Service.Program, MainConfig: t.NginxConf, SnippetsDir: t.SitesDir, Prefix: t.Prefix, WorkerUser: t.WorkerUser, WorkerGroup: t.WorkerGroup}, nil
}
func (r siteRuntimes) PHP(ctx context.Context, id string) (sites.PHPRuntime, error) {
	t, err := r.software.ReadRuntimeTarget(ctx, id)
	if err != nil {
		return sites.PHPRuntime{}, err
	}
	if t.PHPIni == "" || t.PoolDir == "" {
		return sites.PHPRuntime{}, errors.New("selected instance is not a PHP runtime")
	}
	return sites.PHPRuntime{ID: id, Program: t.Instance.Service.Program, MainConfig: filepath.Join(t.Instance.Paths.ConfigDir, "php-fpm.conf"), PoolDir: t.PoolDir, Prefix: t.Prefix, CLIProgram: t.PHPCLIProgram, CLIConfig: t.PHPCLIConfig}, nil
}
func (r siteRuntimes) Check(ctx context.Context, id string) error  { return r.software.Check(ctx, id) }
func (r siteRuntimes) Reload(ctx context.Context, id string) error { return r.software.Reload(ctx, id) }
func (r siteRuntimes) RecoverRuntime(ctx context.Context, id string) error {
	return r.software.RecoverRuntime(ctx, id)
}
func (r siteRuntimes) WithRuntimeTransaction(ctx context.Context, ids []string, callback func(context.Context) error) error {
	return r.software.WithRuntimeTransaction(ctx, ids, callback)
}

func serveAgent(args []string) error {
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	policyPath := flags.String("policy", "", "root-owned typed module and peer policy")
	socket := flags.String("socket", defaultAgentSocket, "Unix socket under a protected directory")
	check := flags.Bool("check-policy", false, "validate without creating directories, users or services")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *policyPath == "" || flags.NArg() != 0 {
		return errors.New("agent requires --policy and no positional arguments")
	}
	policy, err := agentconfig.Load(*policyPath)
	if err != nil {
		return err
	}
	if *check {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"valid": true, "policy_sha256": policy.SHA256, "policy_version": policy.Version, "version": version})
	}
	if err = validateAgentSocket(*socket); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	operations := map[string]agent.Executor{}
	modules := []string{}
	bind := func(names []string, executor agent.Executor) error {
		for _, name := range names {
			if operations[name] != nil {
				return fmt.Errorf("duplicate Agent operation %q", name)
			}
			operations[name] = executor
		}
		return nil
	}
	if len(policy.Roots) > 0 {
		executor, e := agent.NewPolicyExecutor(policy.LegacyPolicy())
		if e != nil {
			return e
		}
		defer executor.Close()
		if e = bind(policy.Operations, executor); e != nil {
			return e
		}
		modules = append(modules, "legacy_files")
	}
	var software *plugins.PrivilegedExecutor
	if policy.Plugins != nil {
		software, err = plugins.NewPrivilegedExecutor(*policy.Plugins)
		if err != nil {
			return err
		}
		if err = bind(plugins.Operations(), software); err != nil {
			return err
		}
		modules = append(modules, "software")
	}
	var privilegedFiles *files.Service
	if policy.Files != nil || policy.Sites != nil {
		privatePaths := []string{*policyPath}
		if policy.Plugins != nil {
			privatePaths = append(privatePaths, policy.Plugins.StateDir, policy.Plugins.RootDir)
			if policy.Plugins.EnableSourceBuild {
				privatePaths = append(privatePaths, policy.Plugins.BuildRootDir)
			}
		}
		if policy.Host != nil {
			privatePaths = append(privatePaths, policy.Host.StateDir)
		}
		if policy.Terminal != nil {
			privatePaths = append(privatePaths, policy.Terminal.StateDir)
		}
		if policy.Sites != nil {
			privatePaths = append(privatePaths, policy.Sites.StateDir)
		}
		var configuration agentconfig.FileConfig
		if policy.Files != nil {
			configuration = *policy.Files
		} else {
			configuration = agentconfig.FileConfig{Version: 1, StateDir: filepath.Join(policy.Sites.StateDir, "files")}
		}
		privilegedFiles, err = files.New(configuration.Options(privatePaths))
		if err != nil {
			return err
		}
		defer privilegedFiles.Close()
		if err = bind(files.AgentOperationNames(), &files.Executor{Service: privilegedFiles}); err != nil {
			return err
		}
		modules = append(modules, "files")
	}
	if policy.Sites != nil {
		executor, e := sites.NewAgentExecutorWithFiles(*policy.Sites, siteRuntimes{software: software, stateDir: policy.Sites.StateDir}, privilegedFiles)
		if e != nil {
			return e
		}
		defer executor.Close()
		if e = bind(sites.AgentOperationNames(), executor); e != nil {
			return e
		}
		modules = append(modules, "sites")
	}
	if policy.Terminal != nil {
		manager, e := terminal.New(terminal.Options{Config: *policy.Terminal, Context: ctx})
		if e != nil {
			return e
		}
		defer manager.Close()
		if e = bind(terminal.OperationNames(), &terminal.AgentExecutor{Manager: manager}); e != nil {
			return e
		}
		modules = append(modules, "terminal")
	}
	if policy.Host != nil {
		options := systemops.HostOptions{}
		if software != nil {
			options.ServiceProfiles = func(ctx context.Context) ([]systemops.ServiceProfile, error) {
				profiles, e := software.ServiceProfiles(ctx)
				if e != nil {
					return nil, e
				}
				out := make([]systemops.ServiceProfile, 0, len(profiles))
				for _, p := range profiles {
					out = append(out, systemops.ServiceProfile{ID: p.ID, PluginID: p.PluginID, InstanceID: p.InstanceID, Unit: p.Unit, Ownership: p.Ownership, UnitSHA256: p.UnitSHA256})
				}
				return out, nil
			}
		}
		executor, e := systemops.NewHostExecutorWithOptions(*policy.Host, options)
		if e != nil {
			return e
		}
		defer executor.Close()
		if e = bind(systemops.HostOperationNames, executor); e != nil {
			return e
		}
		modules = append(modules, "host")
	}
	operations["agent_health"] = healthExecutor{agentHealth{Status: "ok", Ready: true, Version: version, PolicySHA256: policy.SHA256, Modules: modules}}
	for _, names := range [][]string{files.AgentOperationNames(), sites.AgentOperationNames(), terminal.OperationNames(), plugins.Operations(), systemops.HostOperationNames} {
		for _, name := range names {
			if operations[name] == nil {
				operations[name] = unavailableExecutor{}
			}
		}
	}
	dispatcher, err := agent.NewDispatcher(operations)
	if err != nil {
		return err
	}
	gid, err := policy.SocketGID()
	if err != nil {
		return err
	}
	lifecycle := newAgentLifecycle(ctx, dispatcher)
	defer lifecycle.Close()
	server, err := agent.NewUnixServerWithOptions(agent.UnixOptions{Path: *socket, AllowedUIDs: policy.AllowedUIDs, SocketGID: gid}, lifecycle)
	if err != nil {
		return err
	}
	defer server.Close()
	return server.Serve(ctx)
}

func validateAgentSocket(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("Agent socket must be an absolute clean path")
	}
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) || (info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0) {
				return errors.New("Agent socket ancestors must be real protected directories owned by Agent UID")
			}
		}
		if parent == "/" {
			break
		}
	}
	return nil
}

func agentHealthcheck(args []string) error {
	flags := flag.NewFlagSet("agent-healthcheck", flag.ContinueOnError)
	socket := flags.String("socket", defaultAgentSocket, "protected Agent Unix socket")
	wantVersion := flags.String("expected-version", "", "require this exact running version")
	wantPolicy := flags.String("expected-policy-sha256", "", "require exact policy digest")
	timeout := flags.Duration("timeout", 3*time.Second, "readiness deadline")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *timeout <= 0 || *timeout > 30*time.Second {
		return errors.New("invalid Agent healthcheck arguments")
	}
	client, err := agent.NewUnixClient(*socket, 0)
	if err != nil {
		return err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	result, err := client.Run(ctx, agent.Request{ActorID: "healthcheck", Operation: "agent_health", Input: json.RawMessage(`{}`)})
	if err != nil {
		return err
	}
	var health agentHealth
	if json.Unmarshal(result.Data, &health) != nil || health.Status != "ok" || !health.Ready || health.Version == "" || len(health.PolicySHA256) != 64 {
		return errors.New("Agent is not ready")
	}
	if *wantVersion != "" && health.Version != *wantVersion {
		return errors.New("Agent version does not match installed release")
	}
	if *wantPolicy != "" && health.PolicySHA256 != *wantPolicy {
		return errors.New("Agent policy does not match installed configuration")
	}
	return json.NewEncoder(os.Stdout).Encode(health)
}
