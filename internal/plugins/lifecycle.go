package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

type snapshot struct {
	Instance Instance `json:"instance"`
	Runtime  string   `json:"runtime"`
	Config   string   `json:"config"`
}

func (e *PrivilegedExecutor) plan(ctx context.Context, input PlanInput) (Plan, error) {
	manifest, err := e.catalog.Get(input.PluginID)
	if err != nil {
		return Plan{}, failure("not_found", "Unknown software catalog entry", 404)
	}
	label := input.InstanceID
	if label == "" {
		label = "default"
	}
	id := manifest.ID + "-" + label
	if isHostedAppRuntime(manifest.ID) {
		unlockRuntime, lockErr := e.lockContext(ctx, "hosted-app-runtimes")
		if lockErr != nil {
			return Plan{}, lockErr
		}
		defer unlockRuntime()
	}
	if isManagedWebEngine(manifest.ID) {
		unlockStack, lockErr := e.lockContext(ctx, "managed-web-stack")
		if lockErr != nil {
			return Plan{}, lockErr
		}
		defer unlockStack()
	}
	unlock, err := e.lockContext(ctx, id)
	if err != nil {
		return Plan{}, err
	}
	defer unlock()
	if input.Operation != "install" {
		if err := e.ensureInstallFinalized(ctx, id); err != nil {
			return Plan{}, err
		}
		if err := e.ensureDatabaseReplicationDetached(ctx, id); err != nil {
			return Plan{}, err
		}
	}
	current, loadErr := e.get(ctx, id, false)
	if loadErr != nil && core.ErrorCode(loadErr) != "not_found" {
		return Plan{}, loadErr
	}
	if input.Operation == "install" && loadErr == nil && current.Status != "uninstalled" {
		return Plan{}, failure("already_installed", "This managed instance already exists", 409)
	}
	if input.Operation == "install" && isManagedWebEngine(manifest.ID) {
		if err := e.ensureWebEngineAvailable(ctx, manifest.ID); err != nil {
			return Plan{}, err
		}
	}
	if manifest.ID == "nginx" && !e.config.UseSystemd {
		return Plan{}, failure("service_manager_required", "The managed Nginx stack requires systemd to isolate its distribution service", 412)
	}
	if input.Operation != "install" && loadErr != nil {
		return Plan{}, failure("not_found", "A managed instance is required for this operation", 404)
	}
	version, err := manifest.SelectVersion(input.Version)
	if err != nil && input.Operation == "install" {
		return Plan{}, failure("unsupported_version", "Version is absent from the trusted catalog", 412)
	}
	if input.Operation != "install" && input.Version == "" {
		for _, v := range manifest.Versions {
			if v.ID == current.SourceVersion {
				version = v
				err = nil
				break
			}
		}
	}
	if err != nil {
		return Plan{}, failure("unsupported_version", "Requested software version is unavailable", 412)
	}
	platform := CurrentPlatform()
	if version.Source.OS != "" && version.Source.OS != platform.OS || version.Source.Architecture != "" && version.Source.Architecture != platform.Arch {
		return Plan{}, failure("unsupported_platform", "Trusted artifact does not match this privileged host executor operating system and architecture", 412)
	}
	if version.Source.Kind == "unavailable" {
		return Plan{}, failure("artifact_unavailable", version.Verification.Reason, 412)
	}
	if version.Source.Kind == "verified_source" && !e.config.EnableSourceBuild {
		return Plan{}, failure("source_build_disabled", "Enable a dedicated native source builder in trusted Agent policy before planning this version", 412)
	}
	config := defaultConfig(manifest.ID)
	if loadErr == nil {
		config = cloneConfig(current.Config)
	}
	for key, value := range input.Config {
		config[key] = value
	}
	if err = validateConfiguration(manifest.ID, config); err != nil {
		return Plan{}, err
	}
	planID, err := randomPlanID()
	if err != nil {
		return Plan{}, err
	}
	keepData := true
	if input.KeepData != nil {
		keepData = *input.KeepData
	}
	plan := Plan{ID: planID, InstanceID: id, PluginID: manifest.ID, Operation: input.Operation, Version: version, Config: config, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(30 * time.Minute), State: "planned", KeepData: keepData}
	plan.Resources = []string{InstanceResource(id)}
	if (input.Operation == "install" || input.Operation == "upgrade") && sourceUsesSystemPackages(version.Source) {
		plan.Resources = append(plan.Resources, "host:packages")
	}
	if loadErr == nil {
		plan.ExpectedRevision = current.Revision
	}
	base := e.instancePaths(manifest.ID, label)
	switch input.Operation {
	case "install", "upgrade":
		if input.Operation == "install" {
			if err := e.validateInstallTarget(ctx, current, loadErr, manifest.ID, label); err != nil {
				return Plan{}, err
			}
		}
		if sourceUsesSystemPackages(version.Source) {
			if !manifest.Supports(CurrentPlatform()) {
				return Plan{}, failure("unsupported_platform", "This signed package recipe has not been reviewed for the current Linux distribution/architecture", 412)
			}
			plan.Packages, err = e.packages.resolve(ctx, version.Source)
			if err != nil {
				return Plan{}, err
			}
			if version.Source.Kind == "signed_package" && !versionMatchesRequested(version, plan.Packages) {
				return Plan{}, failure("package_series_unavailable", "Trusted repository candidate does not match the requested software series/version; choose its matching repository or version", 412)
			}
			for _, pin := range plan.Packages {
				plan.Changes = append(plan.Changes, Change{Kind: "signed_package", Target: pin.Name + "=" + pin.Version, Description: "Install exact signature-verified package; no unrelated package is removed"})
			}
		}
		if version.Source.Kind == "verified_source" {
			plan.Changes = append(plan.Changes, Change{Kind: "native_source_build", Target: version.Source.URL, Description: "Build the exact SHA-256 pinned official source as a dedicated non-API, non-runtime UID; promote only protected native ELF files"})
		}
		plan.Changes = append(plan.Changes, Change{Kind: "owned_runtime", Target: base.Root, Description: "Install verified runtime and managed configuration; retain application data"})
	case "config":
		plan.Changes = append(plan.Changes, Change{Kind: "owned_configuration", Target: base.ConfigDir, Description: "Validate, atomically replace configuration, reload and verify; restore old configuration on failure"})
	case "uninstall":
		if err := e.requireRuntimeUnused(current); err != nil {
			return Plan{}, err
		}
		plan.Changes = append(plan.Changes, Change{Kind: "owned_runtime", Target: base.Root, Description: "Stop owned service and remove only owned runtime/configuration; preserve data by default"})
		if !keepData {
			plan.Changes = append(plan.Changes, Change{Kind: "purge_owned_data", Target: base.DataDir, Description: "Explicitly delete owned data and backups"})
		}
	case "rollback":
		var prior snapshot
		if err = e.load("private", id, &prior); err != nil {
			return Plan{}, failure("rollback_unavailable", "No verified previous runtime/configuration snapshot exists", 412)
		}
		priorVersion, versionErr := manifest.SelectVersion(prior.Instance.SourceVersion)
		if versionErr != nil {
			return Plan{}, failure("rollback_recipe_unavailable", "Previous runtime recipe is no longer in the trusted catalog", 412)
		}
		if priorVersion.Source.Kind == "signed_package" && !samePackageRuntime(prior.Instance.Packages, current.Packages) {
			return Plan{}, failure("rollback_requires_package_plan", "Signed package rollback requires an exact previous package and dependency plan; data was preserved", 412)
		}
		plan.Changes = append(plan.Changes, Change{Kind: "rollback", Target: base.Root, Description: "Restore previous verified runtime and configuration without overwriting data"})
	}
	if err = e.save("plans", plan.ID, plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func (e *PrivilegedExecutor) apply(ctx context.Context, input ApplyInput) (instance Instance, returned error) {
	var plan Plan
	if err := e.load("plans", input.PlanID, &plan); err != nil {
		return instance, err
	}
	if isHostedAppRuntime(plan.PluginID) {
		unlockRuntime, lockErr := e.lockContext(ctx, "hosted-app-runtimes")
		if lockErr != nil {
			return instance, lockErr
		}
		defer unlockRuntime()
	}
	if isManagedWebEngine(plan.PluginID) {
		unlockStack, lockErr := e.lockContext(ctx, "managed-web-stack")
		if lockErr != nil {
			return instance, lockErr
		}
		defer unlockStack()
	}
	unlock, err := e.lockContext(ctx, plan.InstanceID)
	if err != nil {
		return instance, err
	}
	defer unlock()
	if plan.Operation != "install" {
		if err := e.ensureInstallFinalized(ctx, plan.InstanceID); err != nil {
			return instance, err
		}
		if err := e.ensureDatabaseReplicationDetached(ctx, plan.InstanceID); err != nil {
			return instance, err
		}
	}
	var durable struct {
		Plan Plan   `json:"plan"`
		Key  string `json:"key"`
	}
	// Plan key is written before any mutation; a crash is recovery_required,
	// never a license to replay an uncertain package/data operation.
	keyID := "apply_" + strings.TrimPrefix(plan.ID, "plan_")
	if err := e.load("private", keyID, &durable); err == nil {
		if durable.Key != input.IdempotencyKey {
			return instance, failure("idempotency_conflict", "Plan is already bound to another idempotency key", 409)
		}
		if durable.Plan.State == "succeeded" && durable.Plan.Result != nil {
			return *durable.Plan.Result, nil
		}
		return instance, failure("operation_recovery_required", "Previous plan execution is incomplete or failed; inspect state before making a fresh plan", 409)
	} else if core.ErrorCode(err) != "not_found" {
		return instance, err
	}
	installJournalRevision := int64(0)
	if plan.State != "planned" || time.Now().After(plan.ExpiresAt) {
		return instance, failure("plan_expired", "Software plan is expired or already consumed", 412)
	}
	current, loadErr := e.get(ctx, plan.InstanceID, false)
	if loadErr != nil && core.ErrorCode(loadErr) != "not_found" {
		return instance, loadErr
	}
	revision := int64(0)
	if loadErr == nil {
		revision = current.Revision
	}
	if revision != input.ExpectedRevision || revision != plan.ExpectedRevision {
		return instance, failure("revision_conflict", "Software state changed after planning", 412)
	}
	if plan.Operation == "install" {
		if isManagedWebEngine(plan.PluginID) {
			if err := e.ensureWebEngineAvailable(ctx, plan.PluginID); err != nil {
				return instance, err
			}
		}
		if priorJournal, journalErr := e.loadInstallJournal(plan.InstanceID); journalErr == nil {
			if priorJournal.Phase != "completed" || loadErr != nil || current.Status != "uninstalled" {
				return instance, failure("operation_recovery_required", "A protected install journal already exists; inspect it before any recovery action", 409)
			}
			installJournalRevision = priorJournal.Revision
		} else if core.ErrorCode(journalErr) != "not_found" {
			return instance, journalErr
		}
	}
	manifest, err := e.catalog.Get(plan.PluginID)
	if err != nil {
		return instance, err
	}
	var desiredAutostart *bool
	if e.config.UseSystemd && (plan.Operation == "install" || plan.Operation == "upgrade") {
		value := true // A successful new daemon installation persists desired-running.
		if plan.Operation == "upgrade" && current.Service.Kind == "daemon" && current.Service.Program != "" {
			if current.Service.Autostart != nil {
				value = *current.Service.Autostart
			} else {
				observed, _, observeErr := e.bootState(ctx, current)
				if observeErr != nil {
					return instance, observeErr
				}
				value = observed
			}
		}
		desiredAutostart = &value
	}
	label := strings.TrimPrefix(plan.InstanceID, plan.PluginID+"-")
	paths := e.instancePaths(plan.PluginID, label)
	if plan.Operation == "install" {
		if err := e.validateInstallTarget(ctx, current, loadErr, plan.PluginID, label); err != nil {
			return instance, err
		}
	}
	var install *installJournal
	if plan.Operation == "install" {
		instance = Instance{ID: plan.InstanceID, PluginID: plan.PluginID, InstanceID: label, Version: plan.Version.Version, SourceVersion: plan.Version.ID, Revision: revision + 1, Status: "configured", Paths: paths, Config: cloneConfig(plan.Config), UpdatedAt: time.Now().UTC(), Ownership: "managed", Packages: plan.Packages}
		if loadErr == nil {
			instance.Extensions = maps.Clone(current.Extensions)
		}
		instance.Service = ServiceProfile{ID: instance.ID, PluginID: instance.PluginID, InstanceID: label, Unit: "powerlinux-plugin-" + instance.ID + ".service", ConfigPath: paths.ConfigDir, DataPath: paths.DataDir, Ownership: "managed"}
		instance.Service.Kind = "daemon"
		if manifest.Service.Health == "cli" {
			instance.Service.Kind = "cli"
		}
		if plan.Version.Source.Kind == "integration" {
			instance.Service.Kind = "integration"
		}
		var previous *Instance
		if loadErr == nil {
			prior := current
			previous = &prior
		}
		created, createErr := e.newInstallJournal(ctx, plan, input, instance, previous, desiredAutostart, installJournalRevision)
		if createErr != nil {
			return instance, createErr
		}
		install = &created
	}
	plan.State = "running"
	durable.Plan = plan
	durable.Key = input.IdempotencyKey
	if err = e.save("private", keyID, durable); err != nil {
		return instance, err
	}
	defer func() {
		if returned != nil {
			durable.Plan.State = "failed"
			durable.Plan.Failure = "Operation failed; inspect runtime, data and previous snapshot before retry"
		} else {
			durable.Plan.State = "succeeded"
			durable.Plan.Result = &instance
		}
		if err := e.save("private", keyID, durable); err != nil && returned == nil {
			returned = failure("persistence_failed", "Software finished but its durable execution result could not be saved; inspect state before retrying", 500)
		}
	}()
	if plan.Operation == "uninstall" {
		if err := e.requireRuntimeUnused(current); err != nil {
			return instance, err
		}
		if _, err = e.controlLocked(ctx, ServiceInput{ID: current.ID, Command: "stop"}, current); err != nil && current.Status == "running" {
			return instance, err
		}
		if err = e.removeUnit(ctx, current); err != nil {
			return instance, err
		}
		for _, dir := range []string{filepath.Join(paths.Root, "runtime"), paths.ConfigDir, paths.RunDir} {
			if err = removeOwnedDirectory(paths.Root, dir); err != nil {
				return instance, err
			}
		}
		if !plan.KeepData {
			for _, dir := range []string{paths.DataDir, paths.BackupDir, paths.LogDir} {
				if err = removeOwnedDirectory(paths.Root, dir); err != nil {
					return instance, err
				}
			}
		}
		current.Status = "uninstalled"
		current.Revision++
		current.UpdatedAt = time.Now().UTC()
		current.Health = Health{Status: "uninstalled", CheckedAt: time.Now().UTC()}
		if err = e.save("receipts", current.ID, current); err != nil {
			return instance, err
		}
		return current, nil
	}
	if plan.Operation == "rollback" {
		return e.rollback(ctx, current)
	}
	if install != nil {
		if err = e.saveNextInstallJournal(ctx, install, "directories_starting"); err != nil {
			return instance, err
		}
	}
	if err = e.prepareInstanceDirectories(paths); err != nil {
		return instance, err
	}
	if install != nil {
		install.Instance = instance
		if err = e.saveNextInstallJournal(ctx, install, "directories_ready"); err != nil {
			return instance, err
		}
	}
	if plan.Operation != "install" {
		if err = e.takeSnapshot(ctx, current); err != nil {
			return instance, err
		}
		instance = Instance{ID: plan.InstanceID, PluginID: plan.PluginID, InstanceID: label, Version: plan.Version.Version, SourceVersion: plan.Version.ID, Revision: revision + 1, Status: "configured", Paths: paths, Config: cloneConfig(plan.Config), UpdatedAt: time.Now().UTC(), Ownership: "managed", Packages: plan.Packages}
		if loadErr == nil {
			instance.Extensions = maps.Clone(current.Extensions)
		}
		instance.Service = ServiceProfile{ID: instance.ID, PluginID: instance.PluginID, InstanceID: label, Unit: "powerlinux-plugin-" + instance.ID + ".service", ConfigPath: paths.ConfigDir, DataPath: paths.DataDir, Ownership: "managed"}
		instance.Service.Kind = "daemon"
		if manifest.Service.Health == "cli" {
			instance.Service.Kind = "cli"
		}
		if plan.Version.Source.Kind == "integration" {
			instance.Service.Kind = "integration"
		}
	}
	if plan.Operation == "config" {
		instance = current
		instance.Config = cloneConfig(plan.Config)
		instance.Revision++
		instance.UpdatedAt = time.Now().UTC()
	}
	if plan.Operation == "install" || plan.Operation == "upgrade" {
		switch plan.Version.Source.Kind {
		case "verified_archive", "verified_source":
			if install != nil {
				if err = e.saveNextInstallJournal(ctx, install, "artifact_fetch_started"); err != nil {
					return instance, err
				}
			}
			artifact, err := e.artifacts.fetch(ctx, plan.Version.Source)
			if err != nil {
				return instance, err
			}
			stage := ""
			if plan.Version.Source.Kind == "verified_source" {
				if len(plan.Packages) > 0 {
					if install != nil {
						if err = e.saveNextInstallJournal(ctx, install, "package_install_started"); err != nil {
							return instance, err
						}
					}
					if err = e.packages.install(ctx, plan.Packages); err != nil {
						return instance, err
					}
					if plan.PluginID == "nginx" {
						if err = e.isolateDistributionNginxService(ctx); err != nil {
							return instance, err
						}
					}
					if install != nil {
						if err = e.saveNextInstallJournal(ctx, install, "package_install_completed"); err != nil {
							return instance, err
						}
					}
				}
				if install != nil {
					if err = e.saveNextInstallJournal(ctx, install, "source_build_started"); err != nil {
						return instance, err
					}
				}
				stage, err = e.buildSource(ctx, plan.Version.Source, artifact, paths, manifest)
				if err != nil {
					return instance, err
				}
			} else {
				if install != nil {
					if err = e.saveNextInstallJournal(ctx, install, "archive_staging_started"); err != nil {
						return instance, err
					}
				}
				stage, err = os.MkdirTemp(paths.Root, ".staging-*")
				if err != nil {
					return instance, err
				}
				if err = extractArtifact(ctx, artifact, stage, plan.Version.Source); err != nil {
					_ = os.RemoveAll(stage)
					return instance, err
				}
				if err = secureRuntimeTree(stage, e.gid); err != nil {
					_ = os.RemoveAll(stage)
					return instance, err
				}
			}
			defer os.RemoveAll(stage)
			program, err := resolveArchiveProgram(stage, manifest)
			if err != nil {
				return instance, err
			}
			relative, _ := filepath.Rel(stage, program)
			if plan.Operation == "upgrade" && current.Service.Program != "" && current.Service.Kind != "cli" {
				if _, err = e.controlLocked(ctx, ServiceInput{ID: current.ID, Command: "stop"}, current); err != nil {
					return instance, err
				}
			}
			runtimeDir := filepath.Join(paths.Root, "runtime")
			if install != nil {
				if err = e.saveNextInstallJournal(ctx, install, "runtime_promotion_pending"); err != nil {
					return instance, err
				}
			}
			if _, err = os.Stat(runtimeDir); err == nil {
				if err = removeOwnedDirectory(paths.Root, runtimeDir); err != nil {
					return instance, err
				}
			}
			if err = os.Rename(stage, runtimeDir); err != nil {
				return instance, err
			}
			instance.Service.Program = filepath.Join(runtimeDir, relative)
			instance.ArtifactSHA256 = plan.Version.Source.SHA256
			if install != nil {
				install.Instance = instance
				if err = e.saveNextInstallJournal(ctx, install, "candidate_runtime_ready"); err != nil {
					return instance, err
				}
			}
		case "signed_package":
			distributionBefore, captureErr := e.captureDistributionServices(ctx, plan)
			if captureErr != nil {
				return instance, captureErr
			}
			if install != nil {
				install.DistributionServices = distributionBefore
				if err = e.saveNextInstallJournal(ctx, install, "package_install_started"); err != nil {
					return instance, err
				}
			}
			if err = e.packages.install(ctx, plan.Packages); err != nil {
				return instance, err
			}
			if err = isolateNewDistributionServices(ctx, distributionBefore); err != nil {
				return instance, err
			}
			if plan.PluginID == "nginx" {
				if err = e.isolateDistributionNginxService(ctx); err != nil {
					return instance, err
				}
			}
			if install != nil {
				if err = e.saveNextInstallJournal(ctx, install, "package_install_completed"); err != nil {
					return instance, err
				}
			}
			program, err := resolvePackageProgram(manifest, plan.Version, plan.Packages)
			if err != nil {
				return instance, err
			}
			instance.Service.Program = program
			if len(plan.Packages) > 0 {
				instance.Version = plan.Packages[0].Version
			}
			if install != nil {
				install.Instance = instance
				if err = e.saveNextInstallJournal(ctx, install, "candidate_runtime_ready"); err != nil {
					return instance, err
				}
			}
		case "integration":
			instance.Status = "enabled"
		default:
			return instance, failure("artifact_unavailable", "No trusted native installation recipe is available", 412)
		}
	}
	if install != nil {
		install.Instance = instance
		if err = e.saveNextInstallJournal(ctx, install, "configure_started"); err != nil {
			return instance, err
		}
	}
	if install != nil {
		err = e.configureWithInstallJournal(ctx, &instance, manifest, install.Nonce)
	} else {
		err = e.configure(ctx, &instance, manifest)
	}
	if err != nil {
		if plan.Operation != "install" {
			e.restoreSnapshot(ctx, current)
		}
		return instance, err
	}
	if install != nil {
		if e.config.UseSystemd && instance.Service.Program != "" && instance.Service.Kind != "cli" {
			stored, loadErr := e.loadInstallJournal(instance.ID)
			if loadErr != nil {
				return instance, loadErr
			}
			*install = stored
			instance.Service.UnitSHA256 = stored.UnitSHA256
			install.Instance = instance
		} else {
			if err = e.recordInstallCandidate(ctx, install, instance); err != nil {
				return instance, err
			}
		}
	}
	if e.config.UseSystemd && instance.Service.Program != "" && instance.Service.Kind != "cli" {
		unit, err := os.ReadFile(filepath.Join(e.systemdUnitDir(), instance.Service.Unit))
		if err != nil {
			return instance, err
		}
		digest := sha256.Sum256(unit)
		actualUnitSHA := hex.EncodeToString(digest[:])
		if install != nil && (actualUnitSHA != install.UnitSHA256 || !strings.Contains(string(unit), "# PowerLinux install journal nonce: "+install.Nonce+"\n")) {
			return instance, failure("ownership_conflict", "The new service unit changed after its create-only write; it was preserved", 409)
		}
		instance.Service.UnitSHA256 = actualUnitSHA
	}
	if install != nil {
		install.Instance = instance
		if err = e.verifyInstallJournalProofs(ctx, *install); err != nil {
			return instance, err
		}
	}
	if instance.Service.Program != "" && instance.Service.Kind != "cli" {
		control := func(input ServiceInput) (map[string]any, error) {
			if install != nil && e.installControlHook != nil {
				return e.installControlHook(ctx, input, instance)
			}
			return e.controlLocked(ctx, input, instance)
		}
		if install != nil {
			install.Instance = instance
			if err = e.saveNextInstallJournal(ctx, install, "service_starting"); err != nil {
				return instance, err
			}
		}
		if _, err = control(ServiceInput{ID: instance.ID, Command: "check"}); err != nil {
			if install != nil {
				install.Instance = instance
				install.FailureCode = "service_check_failed"
				_ = e.saveNextInstallJournal(ctx, install, "health_failed")
			}
			if plan.Operation != "install" {
				e.restoreSnapshot(ctx, current)
			}
			return instance, err
		}
		command := "start"
		if plan.Operation == "config" {
			if _, runningErr := ownedProcessID(current); runningErr == nil {
				command = "restart"
				if isManagedWebEngine(current.PluginID) || isPHPRuntime(current.PluginID) {
					command = "reload"
				}
			}
		}
		if _, err = control(ServiceInput{ID: instance.ID, Command: command}); err != nil {
			if install != nil {
				install.Instance = instance
				install.FailureCode = "service_start_failed"
				_ = e.saveNextInstallJournal(ctx, install, "health_failed")
			}
			if plan.Operation != "install" {
				e.restoreSnapshot(ctx, current)
			}
			return instance, err
		}
		instance.Status = "running"
	}
	if install != nil && e.installHealthHook != nil {
		instance.Health = e.installHealthHook(ctx, instance)
	} else {
		instance.Health = e.probe(ctx, instance)
	}
	if instance.Health.Status == "unhealthy" {
		if install != nil {
			install.Instance = instance
			install.FailureCode = "health_check_failed"
			_ = e.saveNextInstallJournal(ctx, install, "health_failed")
		}
		if plan.Operation != "install" {
			e.restoreSnapshot(ctx, current)
		}
		return instance, failure("health_check_failed", "Managed software did not pass its health check; its protected install state was preserved for inspection", 502)
	}
	if install != nil {
		install.Instance = instance
		install.FailureCode = ""
		if err = e.saveNextInstallJournal(ctx, install, "health_verified"); err != nil {
			return instance, err
		}
	}
	if desiredAutostart != nil && instance.Service.Kind == "daemon" && instance.Service.Program != "" {
		var bootErr error
		if install != nil && e.installEnableHook != nil {
			bootErr = e.installEnableHook(ctx, install)
		} else {
			bootErr = e.setBootState(ctx, &instance, *desiredAutostart)
		}
		if bootErr != nil {
			if plan.Operation != "install" {
				e.restoreSnapshot(ctx, current)
			}
			return instance, bootErr
		}
		if install != nil && e.installEnableHook != nil {
			value := *desiredAutostart
			instance.Service.Autostart = &value
		}
		if install != nil {
			install.Instance = instance
			if err = e.saveNextInstallJournal(ctx, install, "boot_verified"); err != nil {
				return instance, err
			}
		}
	}
	if install != nil {
		install.Instance = instance
		if install.Phase != "boot_verified" {
			if err = e.saveNextInstallJournal(ctx, install, "receipt_pending"); err != nil {
				return instance, err
			}
		} else if err = e.saveNextInstallJournal(ctx, install, "receipt_pending"); err != nil {
			return instance, err
		}
	}
	if plan.Operation == "install" {
		err = e.saveReceipt(instance.ID, instance)
	} else {
		err = e.save("receipts", instance.ID, instance)
	}
	if err != nil {
		return instance, err
	}
	if install != nil {
		install.Instance = instance
		if err = e.saveNextInstallJournal(ctx, install, "completed"); err != nil {
			return instance, failure("operation_recovery_required", "Install receipt committed but journal completion needs protected recovery", 409)
		}
	}
	return instance, nil
}

func isManagedWebEngine(plugin string) bool {
	return plugin == "openresty" || plugin == "nginx"
}

func (e *PrivilegedExecutor) ensureWebEngineAvailable(ctx context.Context, requested string) error {
	instances, err := e.list(ctx)
	if err != nil {
		return err
	}
	return validateManagedWebEngineAvailable(requested, instances)
}

func validateManagedWebEngineAvailable(requested string, instances []Instance) error {
	for _, instance := range instances {
		if instance.Status != "uninstalled" && isManagedWebEngine(instance.PluginID) && instance.PluginID != requested {
			return failure("web_engine_conflict", "Nginx and OpenResty are mutually exclusive managed website stacks; remove the active stack's website configurations, uninstall that stack, then install the other. Website content is retained by default when removed.", 409)
		}
	}
	return nil
}

func cloneConfig(input map[string]any) map[string]any {
	raw, _ := json.Marshal(input)
	var output map[string]any
	_ = json.Unmarshal(raw, &output)
	if output == nil {
		output = map[string]any{}
	}
	return output
}
func removeOwnedDirectory(base, name string) error {
	relative, err := filepath.Rel(base, name)
	if err != nil || !filepath.IsLocal(relative) || relative == "." {
		return failure("ownership_invalid", "Refusing to remove data outside its exact managed instance", 412)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.RemoveAll(relative)
}
func resolveArchiveProgram(stage string, manifest Manifest) (string, error) {
	for _, program := range manifest.Service.Programs {
		if strings.ContainsAny(program, "/\\") {
			return "", failure("invalid_catalog", "Catalog program must be a fixed basename", 500)
		}
		for _, dir := range []string{"bin", "sbin", ""} {
			name := filepath.Join(stage, dir, program)
			if info, err := os.Lstat(name); err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0 {
				return name, nil
			}
		}
	}
	return "", failure("artifact_layout_invalid", "Verified archive does not contain the expected executable", 412)
}
func resolvePackageProgram(manifest Manifest, version Version, pins []PackagePin) (string, error) {
	candidates := []string{}
	if manifest.ID == "postgresql" {
		major, err := reviewedPostgreSQLMajor(pins)
		if err != nil {
			return "", err
		}
		candidates = []string{filepath.Join("/usr/lib/postgresql", strconv.Itoa(major), "bin", "postgres")}
		if len(pins) > 0 && pins[0].Format == "rpm" {
			candidates = []string{"/usr/bin/postgres", "/usr/pgsql-" + strconv.Itoa(major) + "/bin/postgres"}
		}
	}
	if manifest.ID != "postgresql" {
		for _, program := range manifest.Service.Programs {
			for _, dir := range []string{"/usr/sbin", "/usr/bin", "/usr/local/openresty/bin", "/usr/local/openresty/nginx/sbin"} {
				candidates = append(candidates, filepath.Join(dir, program))
			}
		}
	}
	if manifest.ID == "php-apt" {
		major := strings.TrimPrefix(version.ID, "legacy-")
		if len(major) == 2 {
			major = major[:1] + "." + major[1:]
		}
		candidates = append([]string{"/usr/sbin/php-fpm" + major}, candidates...)
	}
	if manifest.ID == "php-yum" {
		major := strings.ReplaceAll(strings.TrimPrefix(version.ID, "legacy-"), ".", "")
		if len(major) == 2 {
			candidates = append([]string{filepath.Join("/opt/remi/php"+major, "root/usr/sbin/php-fpm")}, candidates...)
		}
	}
	for _, name := range candidates {
		info, err := os.Stat(name)
		if err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0 && info.Mode().Perm()&0022 == 0 {
			return name, nil
		}
	}
	return "", failure("runtime_unavailable", "Installed package does not provide the required software executable", 412)
}

func defaultConfig(plugin string) map[string]any {
	ports := map[string]int{"openresty": 18080, "nginx": 18081, "mysql": 18306, "mysql-community": 18307, "mariadb": 18308, "postgresql": 18432, "mongodb": 18017, "redis": 18379, "valkey": 18380, "memcached": 18211, "pureftp": 18221, "rsyncd": 18873}
	config := map[string]any{"bind": "127.0.0.1"}
	if port := ports[plugin]; port != 0 {
		config["port"] = port
	}
	if strings.HasPrefix(plugin, "php") {
		config["memory_limit_mb"] = 128
		config["max_children"] = 5
		config["max_execution_time"] = 30
		config["upload_max_filesize_mb"] = 16
		config["post_max_size_mb"] = 20
		config["slowlog_enabled"] = false
		config["slowlog_timeout_seconds"] = 5
	}
	return config
}
func validateConfiguration(plugin string, config map[string]any) error {
	allowed := map[string]bool{"bind": true, "port": true, "target_id": true}
	if strings.HasPrefix(plugin, "php") {
		for _, key := range []string{"memory_limit_mb", "max_children", "max_execution_time", "upload_max_filesize_mb", "post_max_size_mb", "disable_functions", "session_handler", "session_path", "max_input_vars", "timezone", "display_errors", "slowlog_enabled", "slowlog_timeout_seconds"} {
			allowed[key] = true
		}
	}
	if isManagedWebEngine(plugin) {
		allowed["worker_processes"] = true
		allowed["worker_connections"] = true
		allowed["client_max_body_size_mb"] = true
	}
	if plugin == "redis" || plugin == "valkey" || plugin == "memcached" {
		for _, key := range []string{"maxmemory_mb", "maxclients", "databases", "threads"} {
			allowed[key] = true
		}
	}
	for key, value := range config {
		if !allowed[key] {
			return failure("invalid_config", "Configuration field is not supported by this trusted recipe: "+key, 400)
		}
		if key == "bind" {
			text, ok := value.(string)
			if !ok || net.ParseIP(text) == nil {
				return failure("invalid_config", "bind must be a literal IP address", 400)
			}
		}
		if slices.Contains([]string{"disable_functions", "session_handler", "session_path", "timezone", "target_id"}, key) {
			text, ok := value.(string)
			if !ok || len(text) > 1024 || strings.ContainsAny(text, "\x00\r\n") {
				return failure("invalid_config", "String configuration value is invalid", 400)
			}
		}
		if key == "display_errors" || key == "slowlog_enabled" {
			if _, ok := value.(bool); !ok {
				return failure("invalid_config", key+" must be boolean", 400)
			}
		}
		if key == "port" {
			number, ok := number(value)
			if !ok || number < 1024 || number > 65535 {
				return failure("invalid_config", "Managed service port must be 1024–65535", 400)
			}
		}
		if strings.HasSuffix(key, "_mb") || slices.Contains([]string{"max_children", "max_execution_time", "max_input_vars", "slowlog_timeout_seconds", "worker_processes", "worker_connections", "maxclients", "databases", "threads"}, key) {
			number, ok := number(value)
			if !ok || number < 1 || number > 1048576 {
				return failure("invalid_config", "Numeric configuration field is outside its bounds", 400)
			}
			if key == "slowlog_timeout_seconds" && number > 300 {
				return failure("invalid_config", "PHP slowlog timeout must be between one and 300 seconds", 400)
			}
		}
	}
	return nil
}
func number(value any) (int, bool) {
	switch value := value.(type) {
	case int:
		return value, true
	case int64:
		return int(value), true
	case float64:
		return int(value), float64(int(value)) == value
	case json.Number:
		n, err := value.Int64()
		return int(n), err == nil
	}
	return 0, false
}
func versionMatchesRequested(version Version, pins []PackagePin) bool {
	if len(pins) == 0 {
		return false
	}
	requested := version.Version
	if !regexp.MustCompile(`^[0-9]+(?:\.[0-9]+){0,3}$`).MatchString(requested) {
		return true
	}
	actual := pins[0].Version
	if _, after, ok := strings.Cut(actual, ":"); ok {
		actual = after
	}
	return actual == requested || strings.HasPrefix(actual, requested+".") || strings.HasPrefix(actual, requested+"-") || strings.HasPrefix(actual, requested+"+") || strings.HasPrefix(actual, requested+"~")
}
func setting(config map[string]any, key string, fallback int) int {
	if value, ok := number(config[key]); ok {
		return value
	}
	return fallback
}

func (e *PrivilegedExecutor) takeSnapshot(ctx context.Context, current Instance) error {
	var err error
	if current.ID == "" {
		return nil
	}
	base := filepath.Join(e.config.StateDir, "snapshots", current.ID)
	if err = os.MkdirAll(base, 0700); err != nil {
		return err
	}
	prior := snapshot{Instance: current, Runtime: filepath.Join(base, "runtime"), Config: filepath.Join(base, "config")}
	for _, pair := range [][2]string{{filepath.Join(current.Paths.Root, "runtime"), prior.Runtime}, {current.Paths.ConfigDir, prior.Config}} {
		if _, err = os.Stat(pair[0]); os.IsNotExist(err) {
			continue
		}
		if err = os.RemoveAll(pair[1]); err != nil {
			return err
		}
		if err = copyTree(ctx, pair[0], pair[1]); err != nil {
			return err
		}
	}
	return e.save("private", current.ID, prior)
}
func (e *PrivilegedExecutor) restoreSnapshot(ctx context.Context, current Instance) {
	if current.ID == "" {
		return
	}
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	_, _ = e.rollback(recovery, current)
}
func (e *PrivilegedExecutor) rollback(ctx context.Context, current Instance) (Instance, error) {
	var prior snapshot
	if err := e.load("private", current.ID, &prior); err != nil {
		return current, err
	}
	_, _ = e.controlLocked(ctx, ServiceInput{ID: current.ID, Command: "stop"}, current)
	pairs := [][2]string{{prior.Runtime, filepath.Join(current.Paths.Root, "runtime")}}
	if !isPHPRuntime(current.PluginID) && !isManagedWebEngine(current.PluginID) {
		pairs = append(pairs, [2]string{prior.Config, current.Paths.ConfigDir})
	}
	for _, pair := range pairs {
		if _, err := os.Stat(pair[0]); os.IsNotExist(err) {
			continue
		}
		if err := removeOwnedDirectory(current.Paths.Root, pair[1]); err != nil {
			return current, err
		}
		if err := copyTree(ctx, pair[0], pair[1]); err != nil {
			return current, err
		}
	}
	if isPHPRuntime(current.PluginID) || isManagedWebEngine(current.PluginID) {
		if err := e.restoreOwnWebConfiguration(ctx, prior.Instance, prior.Config); err != nil {
			return current, err
		}
	}
	restored := prior.Instance
	restored.Revision = current.Revision + 1
	restored.UpdatedAt = time.Now().UTC()
	if e.config.UseSystemd && restored.Service.Program != "" && restored.Service.Kind != "cli" {
		if err := e.writeUnit(ctx, restored); err != nil {
			return current, err
		}
		unit, err := os.ReadFile(filepath.Join("/etc/systemd/system", restored.Service.Unit))
		if err != nil {
			return current, err
		}
		digest := sha256.Sum256(unit)
		restored.Service.UnitSHA256 = hex.EncodeToString(digest[:])
	}
	if restored.Service.Program != "" && restored.Service.Kind != "cli" {
		if _, err := e.controlLocked(ctx, ServiceInput{ID: restored.ID, Command: "start"}, restored); err != nil {
			return current, err
		}
	}
	restored.Health = e.probe(ctx, restored)
	if restored.Health.Status == "unhealthy" {
		return current, failure("rollback_health_failed", "Previous runtime also failed its health check; data remains preserved", 502)
	}
	if restored.Service.Autostart != nil && e.config.UseSystemd && restored.Service.Kind == "daemon" && restored.Service.Program != "" {
		if err := e.setBootState(ctx, &restored, *restored.Service.Autostart); err != nil {
			return current, err
		}
	}
	if err := e.save("receipts", restored.ID, restored); err != nil {
		return current, err
	}
	return restored, nil
}

func copyTree(ctx context.Context, source, destination string) error {
	if err := realDirectory(source); err != nil {
		return err
	}
	if err := os.MkdirAll(destination, 0750); err != nil {
		return err
	}
	sourceRoot, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer sourceRoot.Close()
	destinationRoot, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer destinationRoot.Close()
	rootInfo, err := os.Stat(source)
	if err != nil {
		return err
	}
	if err := preserveTreeMetadata(destinationRoot, ".", rootInfo); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(source, name)
		if err != nil || relative == "." {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return failure("snapshot_unsafe_entry", "Managed snapshot does not follow symbolic links", 412)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if err := destinationRoot.MkdirAll(relative, info.Mode().Perm()); err != nil {
				return err
			}
			return preserveTreeMetadata(destinationRoot, relative, info)
		}
		if !info.Mode().IsRegular() {
			return failure("snapshot_unsafe_entry", "Managed snapshot only includes regular configuration/runtime files", 412)
		}
		sourceFile, err := sourceRoot.Open(relative)
		if err != nil {
			return err
		}
		destinationFile, err := destinationRoot.OpenFile(relative, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			sourceFile.Close()
			return err
		}
		_, err = io.Copy(destinationFile, sourceFile)
		sourceClose := sourceFile.Close()
		if err == nil {
			err = sourceClose
		}
		if err == nil {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				err = destinationFile.Chown(int(stat.Uid), int(stat.Gid))
			} else {
				err = failure("snapshot_metadata_invalid", "Snapshot ownership metadata is unavailable", 412)
			}
		}
		if err == nil {
			err = destinationFile.Chmod(info.Mode().Perm())
		}
		if err == nil {
			err = destinationFile.Sync()
		}
		closeErr := destinationFile.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
}

func preserveTreeMetadata(root *os.Root, relative string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return failure("snapshot_metadata_invalid", "Snapshot ownership metadata is unavailable", 412)
	}
	if err := root.Chown(relative, int(stat.Uid), int(stat.Gid)); err != nil {
		return err
	}
	return root.Chmod(relative, info.Mode().Perm())
}
