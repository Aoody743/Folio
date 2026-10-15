package plugins

import (
	"context"
	"maps"
	"reflect"
	"regexp"
	"strings"
	"time"
)

var phpExtensionVersionPattern = regexp.MustCompile(`^8\.(2|3|4)\.[0-9]{1,3}(?:[-+][A-Za-z0-9.+~_-]{1,64})?$`)

// PHPExtensionRecoveryInspection is intentionally limited to the exact public
// plan tuple needed to resume a pending operation. Package paths, credentials,
// hashes of the private intent, and secret state remain inside the Agent.
type PHPExtensionRecoveryInspection struct {
	ID               string    `json:"id"`
	Pending          bool      `json:"pending"`
	PlanID           string    `json:"plan_id,omitempty"`
	Operation        string    `json:"operation,omitempty"`
	ModuleID         string    `json:"module_id,omitempty"`
	ExpectedRevision int64     `json:"expected_revision,omitempty"`
	Phase            string    `json:"phase,omitempty"`
	StartedAt        time.Time `json:"started_at,omitempty"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
	ReplayAvailable  bool      `json:"replay_available"`
}

func (e *PrivilegedExecutor) inspectPHPExtensionRecovery(instance Instance) (PHPExtensionRecoveryInspection, error) {
	result := PHPExtensionRecoveryInspection{ID: instance.ID}
	state, err := e.loadPHPExtensionState(instance.ID)
	if err != nil {
		return result, err
	}
	if state.Pending == nil {
		return result, nil
	}
	intent := state.Pending
	if _, err = e.validatePHPExtensionPendingIntent(context.Background(), instance.ID, *intent); err != nil {
		return result, err
	}
	plan := intent.Plan.Public
	result.Pending = true
	result.PlanID = intent.PlanID
	result.Operation = plan.Operation
	result.ModuleID = plan.ModuleID
	result.ExpectedRevision = intent.Previous.Revision
	result.Phase = intent.Phase
	result.StartedAt = intent.StartedAt
	result.UpdatedAt = intent.UpdatedAt
	result.ReplayAvailable = true
	return result, nil
}

// validatePHPExtensionPendingIntent proves that a recovery journal is still
// tied to the exact managed PHP receipt and the trusted, non-removing module
// recipe. It is intentionally structural: runtime/package state is rechecked
// by the normal apply path before any mutation resumes.
func (e *PrivilegedExecutor) validatePHPExtensionPendingIntent(ctx context.Context, id string, intent phpExtensionIntent) (Instance, error) {
	invalid := func() (Instance, error) {
		return Instance{}, failure("extension_state_invalid", "Protected PHP extension recovery intent does not match its exact managed instance and plan", 412)
	}
	if !validInstanceID(id) || !validPlanID(intent.PlanID) || intent.PlanID != intent.Plan.Public.PlanID ||
		!validPHPExtensionIntentPhase(intent.Phase) || intent.StartedAt.IsZero() || intent.UpdatedAt.IsZero() || intent.UpdatedAt.Before(intent.StartedAt) {
		return invalid()
	}
	previous := intent.Previous
	planRecord := intent.Plan
	plan := planRecord.Public
	if previous.ID != id || previous.PluginID != "php-apt" || previous.Revision < 1 || previous.Ownership != "managed" ||
		!validInstanceID(previous.InstanceID) || previous.Status == "uninstalled" ||
		previous.Paths != e.instancePaths(previous.PluginID, previous.InstanceID) {
		return invalid()
	}
	recipe, supported := phpExtensionPlatform(plan.Platform)
	if !supported || !phpExtensionVersionPattern.MatchString(plan.PHPVersion) || phpExtensionMinor(plan.PHPVersion) != recipe.Minor || plan.PHPModuleAPI != recipe.API {
		return invalid()
	}
	if plan.ID != id || plan.ExpectedRevision != previous.Revision ||
		(plan.Operation != "install" && plan.Operation != "uninstall") || !supportedPHPModuleID(plan.ModuleID) || plan.PackageRemovable ||
		len(plan.Resources) != 2 || plan.Resources[0] != InstanceResource(id) || plan.Resources[1] != "host:packages" ||
		!packageName.MatchString(planRecord.PackageName) || !artifactHash.MatchString(planRecord.ExtensionDirSHA256) {
		return invalid()
	}
	wantPackage, err := phpExtensionPackage(recipe.Minor, plan.ModuleID)
	if err != nil || wantPackage != planRecord.PackageName {
		return invalid()
	}
	createdAt := plan.CreatedAt
	if !validPHPModulePlanWindow(createdAt, plan.ExpiresAt) ||
		plan.AlreadyInstalled != planRecord.TargetPackageWasInstalled || plan.AlreadyEnabled != previous.Extensions[plan.ModuleID] ||
		(planRecord.OwnedPackageAtPlan && (!planRecord.TargetPackageWasInstalled || planRecord.TargetPackageVersion == "")) {
		return invalid()
	}
	if planRecord.TargetPackageWasInstalled {
		if !debVersion.MatchString(planRecord.TargetPackageVersion) {
			return invalid()
		}
	} else if planRecord.TargetPackageVersion != "" {
		return invalid()
	}
	if plan.Operation == "install" {
		if len(plan.PackagePins) == 0 || len(plan.PackagePins) > 128 {
			return invalid()
		}
		seen := make(map[string]struct{}, len(plan.PackagePins))
		modulePinCount := 0
		for _, pin := range plan.PackagePins {
			if pin.Format != "" || !packageName.MatchString(pin.Name) || !debVersion.MatchString(pin.Version) ||
				(pin.Architecture != "all" && pin.Architecture != plan.Platform.Arch) ||
				!trustedPHPRepository(plan.Platform, pin.Repository) || !artifactHash.MatchString(pin.SHA256) {
				return invalid()
			}
			if _, exists := seen[pin.Name]; exists {
				return invalid()
			}
			seen[pin.Name] = struct{}{}
			if pin.Name == planRecord.PackageName {
				modulePinCount++
				if planRecord.TargetPackageWasInstalled && pin.Version != planRecord.TargetPackageVersion {
					return invalid()
				}
			} else if strings.HasPrefix(pin.Name, "php") || strings.HasPrefix(pin.Name, "libapache2-mod-php") {
				return invalid()
			}
		}
		if modulePinCount != 1 {
			return invalid()
		}
	} else if len(plan.PackagePins) != 0 {
		return invalid()
	}
	if previous.Service.ID != id || previous.Service.Unit != "powerlinux-plugin-"+id+".service" ||
		previous.Service.PluginID != previous.PluginID || previous.Service.InstanceID != previous.InstanceID ||
		previous.Service.Ownership != "managed" || previous.Service.Kind != "daemon" ||
		previous.Service.Program != "/usr/sbin/php-fpm"+recipe.Minor ||
		previous.Service.ConfigPath != previous.Paths.ConfigDir || previous.Service.DataPath != previous.Paths.DataDir {
		return invalid()
	}
	wantDesired := previous
	wantDesired.Extensions = maps.Clone(previous.Extensions)
	if wantDesired.Extensions == nil {
		wantDesired.Extensions = map[string]bool{}
	}
	wantEnabled := plan.Operation == "install"
	wantDesired.Extensions[plan.ModuleID] = wantEnabled
	if previous.Extensions[plan.ModuleID] != wantEnabled {
		wantDesired.Revision++
	}
	// The service probe may refresh health while committing the receipt; all
	// other receipt fields must be the previous exact receipt plus this toggle.
	wantDesired.Health = intent.Desired.Health
	if !reflect.DeepEqual(wantDesired, intent.Desired) || intent.Desired.ID != id ||
		intent.Desired.Paths != e.instancePaths(intent.Desired.PluginID, intent.Desired.InstanceID) ||
		intent.Desired.Service.ID != id || intent.Desired.Service.Unit != "powerlinux-plugin-"+id+".service" {
		return invalid()
	}
	current, err := e.get(ctx, id, false)
	if err != nil {
		return Instance{}, err
	}
	if !isPHPRuntime(current.PluginID) || (!reflect.DeepEqual(current, previous) && !reflect.DeepEqual(current, intent.Desired)) {
		return invalid()
	}
	return current, nil
}

func phpModulePlanWindow(now time.Time) (time.Time, time.Time) {
	createdAt := now.UTC()
	return createdAt, createdAt.Add(phpExtensionPlanTTL)
}

func validPHPModulePlanWindow(createdAt, expiresAt time.Time) bool {
	return !createdAt.IsZero() && !expiresAt.IsZero() && !expiresAt.Before(createdAt) &&
		expiresAt.Sub(createdAt) <= phpExtensionPlanTTL+phpExtensionPlanTTLTolerance
}

func validPHPExtensionIntentPhase(phase string) bool {
	switch phase {
	case "prepared", "package_install_started", "package_installed", "config_applied", "receipt_commit_started", "rollback_required":
		return true
	default:
		return false
	}
}

func validatePHPExtensionRecoveryInspectionInput(in PHPInput) error {
	if !validInstanceID(in.ID) || in.Command != "extension_recovery_inspect" || in.ExpectedRevision != 0 ||
		in.Values != nil || in.Extension != "" || in.Enabled || in.Operation != "" || in.ModuleID != "" ||
		in.PlanID != "" || in.IdempotencyKey != "" {
		return failure("invalid_input", "PHP extension recovery inspection accepts only a managed instance ID", 400)
	}
	return nil
}
