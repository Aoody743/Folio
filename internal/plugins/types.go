package plugins

import (
	"encoding/json"
	"time"
)

type Paths struct {
	Root      string `json:"root"`
	ConfigDir string `json:"config_dir"`
	DataDir   string `json:"data_dir"`
	RunDir    string `json:"run_dir"`
	LogDir    string `json:"log_dir"`
	BackupDir string `json:"backup_dir"`
}
type ServiceProfile struct {
	Autostart  *bool  `json:"autostart,omitempty"`
	Kind       string `json:"kind,omitempty"`
	ID         string `json:"id"`
	PluginID   string `json:"plugin_id"`
	InstanceID string `json:"instance_id"`
	Unit       string `json:"unit"`
	Program    string `json:"program"`
	ConfigPath string `json:"config_path"`
	DataPath   string `json:"data_path"`
	Ownership  string `json:"ownership"`
	UnitSHA256 string `json:"unit_sha256,omitempty"`
}
type Health struct {
	Status    string    `json:"status"`
	CheckedAt time.Time `json:"checked_at"`
	Reason    string    `json:"reason,omitempty"`
}
type Instance struct {
	ID             string          `json:"id"`
	PluginID       string          `json:"plugin_id"`
	InstanceID     string          `json:"instance_id"`
	Version        string          `json:"installed_version"`
	SourceVersion  string          `json:"source_version"`
	Revision       int64           `json:"revision"`
	Status         string          `json:"status"`
	Paths          Paths           `json:"paths"`
	Service        ServiceProfile  `json:"service"`
	Health         Health          `json:"health"`
	Config         map[string]any  `json:"config"`
	UpdatedAt      time.Time       `json:"updated_at"`
	Ownership      string          `json:"ownership"`
	ArtifactSHA256 string          `json:"artifact_sha256,omitempty"`
	Packages       []PackagePin    `json:"packages,omitempty"`
	Extensions     map[string]bool `json:"extensions,omitempty"`
}
type PackagePin struct {
	Format       string `json:"format,omitempty"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
	Repository   string `json:"repository"`
	SHA256       string `json:"sha256"`
}
type Plan struct {
	Resources        []string       `json:"resources"`
	ID               string         `json:"id"`
	InstanceID       string         `json:"instance_id"`
	PluginID         string         `json:"plugin_id"`
	Operation        string         `json:"operation"`
	Version          Version        `json:"version"`
	ExpectedRevision int64          `json:"expected_revision"`
	Config           map[string]any `json:"config,omitempty"`
	Packages         []PackagePin   `json:"packages,omitempty"`
	Changes          []Change       `json:"changes"`
	CreatedAt        time.Time      `json:"created_at"`
	ExpiresAt        time.Time      `json:"expires_at"`
	State            string         `json:"state"`
	Result           *Instance      `json:"result,omitempty"`
	Failure          string         `json:"failure,omitempty"`
	KeepData         bool           `json:"keep_data"`
}
type Change struct {
	Kind        string `json:"kind"`
	Target      string `json:"target"`
	Description string `json:"description"`
}
type PlanInput struct {
	Operation  string         `json:"operation"`
	PluginID   string         `json:"plugin_id"`
	InstanceID string         `json:"instance_id,omitempty"`
	Version    string         `json:"version,omitempty"`
	Config     map[string]any `json:"config,omitempty"`
	KeepData   *bool          `json:"keep_data,omitempty"`
}
type ApplyInput struct {
	PlanID           string `json:"plan_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	IdempotencyKey   string `json:"idempotency_key"`
}
type IDInput struct {
	ID string `json:"id"`
}
type ServiceInput struct {
	ID      string `json:"id"`
	Command string `json:"command"`
}
type RuntimeTarget struct {
	Instance      Instance `json:"instance"`
	NginxConf     string   `json:"nginx_conf,omitempty"`
	SitesDir      string   `json:"sites_dir,omitempty"`
	PHPIni        string   `json:"php_ini,omitempty"`
	FPMAddress    string   `json:"fpm_address,omitempty"`
	PoolDir       string   `json:"pool_dir,omitempty"`
	WorkerUser    string   `json:"worker_user,omitempty"`
	WorkerGroup   string   `json:"worker_group,omitempty"`
	Prefix        string   `json:"prefix,omitempty"`
	PHPCLIProgram string   `json:"php_cli_program,omitempty"`
	PHPCLIConfig  string   `json:"php_cli_config,omitempty"`
}
type ConfigInput struct {
	ID               string          `json:"id"`
	ExpectedRevision int64           `json:"expected_revision"`
	Values           json.RawMessage `json:"values"`
}

func InstanceResource(id string) string { return "software:" + id }
