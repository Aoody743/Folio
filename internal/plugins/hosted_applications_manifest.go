package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxHostedAppManifestBytes = 2 << 20
	maxHostedAppLockBytes     = 64 << 20
)

type hostedAppPackageManifest struct {
	Scripts map[string]string `json:"scripts"`
}

// validateHostedAppProjectManifest is shared by archive, Git, and build paths so
// the same project contract is enforced before a user supplied script runs.
func validateHostedAppProjectManifest(project, runtime string) error {
	regular := func(name string, maxBytes int64) bool {
		info, err := os.Lstat(filepath.Join(project, name))
		return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Size() >= 0 && info.Size() <= maxBytes
	}
	switch runtime {
	case "nextjs", "nodejs":
		if !regular("package.json", maxHostedAppManifestBytes) {
			return failure("application_manifest_required", "Node.js project must contain a regular root package.json", 400)
		}
		if !regular("package-lock.json", maxHostedAppLockBytes) && !regular("npm-shrinkwrap.json", maxHostedAppLockBytes) {
			return failure("application_lockfile_required", "Node.js project must contain package-lock.json or npm-shrinkwrap.json", 400)
		}
		body, err := os.ReadFile(filepath.Join(project, "package.json"))
		if err != nil {
			return failure("application_manifest_invalid", "Node.js package.json could not be read", 400)
		}
		var manifest hostedAppPackageManifest
		if json.Unmarshal(body, &manifest) != nil || strings.TrimSpace(manifest.Scripts["start"]) == "" {
			return failure("application_start_script_required", "Node.js package.json must declare a non-empty scripts.start command", 400)
		}
		if runtime == "nextjs" && strings.TrimSpace(manifest.Scripts["build"]) == "" {
			return failure("application_build_script_required", "Next.js package.json must declare a non-empty scripts.build command", 400)
		}
	case "go":
		if !regular("go.mod", maxHostedAppManifestBytes) || !regular("go.sum", maxHostedAppLockBytes) {
			return failure("application_manifest_required", "Go project must contain regular root go.mod and go.sum files", 400)
		}
	default:
		return failure("invalid_input", "Unsupported hosted application runtime", 400)
	}
	return nil
}
