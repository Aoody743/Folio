package main

import (
	"context"
	"net/http"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/httpapi"
	"github.com/Aoody743/PowerLinux/internal/plugins"
	"github.com/Aoody743/PowerLinux/internal/sites"
	"github.com/Aoody743/PowerLinux/internal/systemops"
	"github.com/Aoody743/PowerLinux/internal/terminalbridge"
)

type runtimeModules struct {
	system   *systemops.Provider
	terminal *terminalbridge.Provider
	database httpapi.ResourceHandler
	website  httpapi.ResourceHandler
}
type resourceHandlers []httpapi.ResourceHandler

func (handlers resourceHandlers) HandleResource(w http.ResponseWriter, r *http.Request, a core.Actor) bool {
	for _, handler := range handlers {
		if handler != nil && handler.HandleResource(w, r, a) {
			return true
		}
	}
	return false
}

func registerModules(service *core.Service, client *agent.UnixClient) (*runtimeModules, error) {
	if err := service.RegisterScopes("files:permissions", "files:host", "terminal:root", "terminal:share", "host:packages", "databases:credentials"); err != nil {
		return nil, err
	}
	// Assign an interface only when the concrete pointer is non-nil. Typed nils
	// otherwise make optional Agent operations panic in ordinary deployments.
	var executor agent.Executor
	if client != nil {
		executor = client
	}
	software, err := plugins.New(service.Runtime("plugins"), plugins.Options{Executor: executor})
	if err != nil {
		return nil, err
	}
	if err = service.RegisterProvider(software); err != nil {
		return nil, err
	}
	if executor != nil {
		if err = service.RegisterExecutor(plugins.AgentJobOperations(), executor); err != nil {
			return nil, err
		}
	}
	if err = service.RegisterExecutor(append(plugins.ManagedJobOperations(), plugins.NativeOperations()...), software); err != nil {
		return nil, err
	}
	if err = service.RegisterExecutor([]string{"docker_registry_pull"}, plugins.NewDockerRegistryExecutor(software, client)); err != nil {
		return nil, err
	}
	if err = service.RegisterExecutor(plugins.DatabaseAdminJobOperations(), plugins.NewDatabaseAdminExecutor(software, client)); err != nil {
		return nil, err
	}
	if err = service.RegisterExecutor(plugins.PostgreSQLMaintenanceJobOperations(), plugins.NewPostgreSQLMaintenanceExecutor(software, client)); err != nil {
		return nil, err
	}
	if err = service.RegisterExecutor(plugins.PHPExtensionJobOperations(), plugins.NewPHPExtensionExecutor(software, client)); err != nil {
		return nil, err
	}
	if err = service.RegisterExecutor(plugins.DockerExecJobOperations(), plugins.NewDockerExecExecutor(software, client)); err != nil {
		return nil, err
	}
	if err = service.RegisterExecutor(plugins.FTPMaintenanceJobOperations(), plugins.NewFTPMaintenanceExecutor(software)); err != nil {
		return nil, err
	}
	websites, err := sites.New(service.Runtime("sites"), sites.Options{Agent: executor})
	if err != nil {
		return nil, err
	}
	if err = service.RegisterProvider(websites); err != nil {
		return nil, err
	}
	if err = service.RegisterExecutor(sites.JobOperationNames(), websites); err != nil {
		return nil, err
	}
	term, err := terminalbridge.New(service.Runtime("terminal"), terminalbridge.Options{Agent: executor, DispatchWithKey: service.ExecuteWithKey})
	if err != nil {
		return nil, err
	}
	registered := false
	defer func() {
		if !registered {
			term.Close()
		}
	}()
	if err = service.RegisterProvider(term); err != nil {
		return nil, err
	}
	sys, err := systemops.New(service.Runtime("systemops"), systemops.Options{Agent: executor, Status: func(ctx context.Context) (core.SystemStatus, error) {
		if err := ctx.Err(); err != nil {
			return core.SystemStatus{}, err
		}
		return service.StatusSnapshot(), nil
	}, AllowedScheduleActions: []string{"files_backup", "files_hash", "sites_backup_create", "certificates_acme_renew", "notifications_send", "scripts_run"}})
	if err != nil {
		return nil, err
	}
	if err = service.RegisterProvider(sys); err != nil {
		sys.Close()
		return nil, err
	}
	if err = service.RegisterExecutor(systemops.JobOperationNames, sys); err != nil {
		sys.Close()
		return nil, err
	}
	registered = true
	return &runtimeModules{system: sys, terminal: term, database: software.DatabaseResourceHandler(service), website: websites.BackupResourceHandler(service)}, nil
}
