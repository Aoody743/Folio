// Package sites owns typed website and certificate operations. HTTP and MCP
// share these bindings; privileged paths and executables are resolved only by
// an explicitly configured Agent from its root-owned software receipts.
package sites

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type Site struct {
	ID                string             `json:"id"`
	Name              string             `json:"name"`
	Domains           []string           `json:"domains"`
	WebInstance       string             `json:"web_instance"`
	PHPInstance       string             `json:"php_instance,omitempty"`
	PHPVersion        string             `json:"php_version,omitempty"`
	DocumentRoot      string             `json:"document_root"`
	RunPath           string             `json:"run_path"`
	CategoryID        string             `json:"category_id,omitempty"`
	Status            string             `json:"status"`
	Default           bool               `json:"default"`
	IPv6              bool               `json:"ipv6"`
	Index             []string           `json:"index"`
	Description       string             `json:"description"`
	ExpiresAt         string             `json:"expires_at,omitempty"`
	Revision          int64              `json:"revision"`
	ConfigurationMode string             `json:"configuration_mode,omitempty"`
	TLS               TLSBinding         `json:"tls"`
	Rewrite           string             `json:"rewrite"`
	RateLimit         RateLimit          `json:"rate_limit"`
	Protection        Protection         `json:"protection"`
	PHP               PHPSettings        `json:"php"`
	DirectoryBindings []DirectoryBinding `json:"directory_bindings"`
	Proxies           []Proxy            `json:"proxies"`
	Redirects         []Redirect         `json:"redirects"`
	AccessLog         bool               `json:"access_log"`
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at"`
	UpdatedBy         core.Actor         `json:"updated_by"`
}

type TLSBinding struct {
	Enabled             bool   `json:"enabled"`
	CertificateID       string `json:"certificate_id,omitempty"`
	Redirect            bool   `json:"redirect"`
	ExpiresAt           string `json:"expires_at,omitempty"`
	CertificateRevision int64  `json:"certificate_revision,omitempty"`
}
type RateLimit struct {
	Enabled           bool  `json:"enabled"`
	Connections       int   `json:"connections"`
	BytesPerSecond    int64 `json:"bytes_per_second"`
	RequestsPerSecond int   `json:"requests_per_second"`
}
type Protection struct {
	Enabled         bool     `json:"enabled"`
	Username        string   `json:"username,omitempty"`
	PasswordRef     string   `json:"password_ref,omitempty"`
	DenyPaths       []string `json:"deny_paths"`
	AntiHotlink     bool     `json:"anti_hotlink"`
	AllowedReferers []string `json:"allowed_referers"`
}
type PHPSettings struct {
	OpenBasedir bool              `json:"open_basedir"`
	Values      map[string]string `json:"values"`
}
type DirectoryBinding struct {
	ID      string   `json:"id"`
	Domains []string `json:"domains"`
	Path    string   `json:"path"`
	Rewrite string   `json:"rewrite"`
}
type Proxy struct {
	ID           string            `json:"id"`
	Path         string            `json:"path"`
	Upstream     string            `json:"upstream"`
	Enabled      bool              `json:"enabled"`
	PreserveHost bool              `json:"preserve_host"`
	CacheSeconds int               `json:"cache_seconds"`
	WebSocket    bool              `json:"websocket"`
	Headers      map[string]string `json:"headers"`
}
type Redirect struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	Destination string `json:"destination"`
	Status      int    `json:"status"`
	Enabled     bool   `json:"enabled"`
	PreserveURI bool   `json:"preserve_uri"`
}
type Category struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Revision int64  `json:"revision"`
}
type Certificate struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Domains       []string  `json:"domains"`
	Issuer        string    `json:"issuer"`
	Serial        string    `json:"serial"`
	SHA256        string    `json:"sha256"`
	NotBefore     time.Time `json:"not_before"`
	ExpiresAt     time.Time `json:"expires_at"`
	Revision      int64     `json:"revision"`
	Source        string    `json:"source"`
	CredentialRef string    `json:"credential_ref,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}
type Backup struct {
	ID          string    `json:"id"`
	SiteID      string    `json:"site_id"`
	CreatedAt   time.Time `json:"created_at"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	IncludesTLS bool      `json:"includes_tls"`
}
type DNSCredential struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Provider  string    `json:"provider"`
	SecretRef string    `json:"secret_ref"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Options struct{ Agent agent.Executor }
type Provider struct {
	deps    core.Dependencies
	options Options
}

// Runtimes is injected into the Agent by composition code. Its implementation
// reads protected software receipts, never the API's mutable metadata.
type Runtimes interface {
	Web(context.Context, string) (WebRuntime, error)
	PHP(context.Context, string) (PHPRuntime, error)
	Check(context.Context, string) error
	Reload(context.Context, string) error
}
type RuntimeVerifier interface {
	VerifySite(context.Context, Site) error
}
type WebRuntime struct {
	ID, Program, MainConfig, SnippetsDir, Prefix string
	WorkerUser, WorkerGroup                      string
}
type PHPRuntime struct{ ID, Program, MainConfig, PoolDir, Prefix, CLIProgram, CLIConfig string }
type AgentConfig struct {
	Version                     int                 `json:"version"`
	RootDir                     string              `json:"root_dir"`
	StateDir                    string              `json:"state_dir"`
	Programs                    map[string]string   `json:"programs"`
	AllowedPorts                []int               `json:"allowed_ports"`
	AllowIPv6                   bool                `json:"allow_ipv6"`
	ACMEProgram                 string              `json:"acme_program,omitempty"`
	AllowedACMEServers          []string            `json:"allowed_acme_servers,omitempty"`
	AllowedDNSAPIURLs           map[string][]string `json:"allowed_dns_api_urls,omitempty"`
	AllowedDNSUpdateServers     map[string][]string `json:"allowed_dns_update_servers,omitempty"`
	AllowedDNSExclusiveTXTSlots map[string][]string `json:"allowed_dns_exclusive_txt_slots,omitempty"`
	AllowAdvancedConfig         bool                `json:"allow_advanced_config,omitempty"`
}
type request struct {
	OrderRef            string            `json:"order_ref,omitempty"`
	Actor               core.Actor        `json:"actor"`
	ID                  string            `json:"id,omitempty"`
	ExpectedRevision    int64             `json:"expected_revision,omitempty"`
	Site                *Site             `json:"site,omitempty"`
	Certificate         *Certificate      `json:"certificate,omitempty"`
	CertPEM             string            `json:"cert_pem,omitempty"`
	KeyPEM              string            `json:"key_pem,omitempty"`
	Password            string            `json:"password,omitempty"`
	KeepData            bool              `json:"keep_data,omitempty"`
	Kind                string            `json:"kind,omitempty"`
	Offset              int               `json:"offset,omitempty"`
	Limit               int               `json:"limit,omitempty"`
	BackupID            string            `json:"backup_id,omitempty"`
	ArtifactOffset      int64             `json:"artifact_offset,omitempty"`
	ArtifactLength      int64             `json:"artifact_length,omitempty"`
	Config              string            `json:"config,omitempty"`
	ACME                *ACMEInput        `json:"acme,omitempty"`
	Credentials         map[string]string `json:"credentials,omitempty"`
	AuthorizedSites     []string          `json:"authorized_sites,omitempty"`
	ResetAdvancedConfig bool              `json:"reset_advanced_config,omitempty"`
	PreviousSiteID      string            `json:"previous_site_id,omitempty"`
	PreviousRevision    int64             `json:"previous_revision,omitempty"`
}
type ACMEInput struct {
	ID                 string   `json:"id"`
	SiteID             string   `json:"site_id,omitempty"`
	Domains            []string `json:"domains"`
	Email              string   `json:"email"`
	Challenge          string   `json:"challenge"`
	Provider           string   `json:"provider,omitempty"`
	CredentialID       string   `json:"credential_id,omitempty"`
	CredentialRevision int64    `json:"credential_revision,omitempty"`
	Server             string   `json:"server,omitempty"`
	Renew              bool     `json:"renew"`
	Force              bool     `json:"force"`
	DNSAlias           string   `json:"dns_alias,omitempty"`
}

func Resource(id string) string            { return "site:" + id }
func CertificateResource(id string) string { return "certificate:" + id }
func encode(value any) json.RawMessage     { raw, _ := json.Marshal(value); return raw }
