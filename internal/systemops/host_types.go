// Package systemops implements declared Linux administration capabilities.
// The API provider and root Agent share typed plans, never arbitrary commands.
package systemops

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

type ServiceProfile struct {
	ID         string `json:"id"`
	PluginID   string `json:"plugin_id,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
	Unit       string `json:"unit"`
	Ownership  string `json:"ownership"`
	Protected  bool   `json:"protected,omitempty"`
	UnitSHA256 string `json:"unit_sha256,omitempty"`
}

// HostConfig is root-owned deployment configuration, not an API document.
// RootPrefix is only for constructing an isolated filesystem in tests/guests.
type HostConfig struct {
	Version             int               `json:"version"`
	StateDir            string            `json:"state_dir"`
	RootPrefix          string            `json:"root_prefix,omitempty"`
	Services            []ServiceProfile  `json:"services"`
	APIUser             string            `json:"api_user,omitempty"`
	Programs            map[string]string `json:"programs"`
	ProtectedPorts      []int             `json:"protected_ports"`
	ConfirmationSeconds int               `json:"confirmation_seconds,omitempty"`
	EnabledKinds        []string          `json:"enabled_kinds"`
	ScriptProfiles      []ScriptProfile   `json:"script_profiles,omitempty"`
	EnableRootScripts   bool              `json:"enable_root_scripts,omitempty"`
}

type SSHConfig struct {
	Ports                  []int    `json:"ports"`
	PasswordAuthentication bool     `json:"password_authentication"`
	PermitRootLogin        string   `json:"permit_root_login"`
	AllowUsers             []string `json:"allow_users,omitempty"`
}
type FirewallRule struct {
	ID       string `json:"id"`
	Protocol string `json:"protocol"`
	Port     int    `json:"port,omitempty"`
	Source   string `json:"source,omitempty"`
	Action   string `json:"action"`
}
type FirewallConfig struct {
	DefaultPolicy string         `json:"default_policy"`
	Rules         []FirewallRule `json:"rules"`
}
type Change struct {
	Kind         string          `json:"kind"`
	ServiceID    string          `json:"service_id,omitempty"`
	ServiceState string          `json:"service_state,omitempty"`
	SSH          *SSHConfig      `json:"ssh,omitempty"`
	Firewall     *FirewallConfig `json:"firewall,omitempty"`
	Timezone     string          `json:"timezone,omitempty"`
	NTP          *bool           `json:"ntp,omitempty"`
	Power        string          `json:"power,omitempty"`
	DelaySeconds int             `json:"delay_seconds,omitempty"`
	Panel        *ServerConfig   `json:"panel,omitempty"`
}

// ServerConfig is the same explicit deployment document consumed by the CLI.
type ServerConfig struct {
	Version        int      `json:"version"`
	Listen         string   `json:"listen"`
	PublicURL      string   `json:"public_url,omitempty"`
	AllowedOrigins []string `json:"allowed_origins"`
	SecureCookies  bool     `json:"secure_cookies"`
	TLSCert        string   `json:"tls_cert,omitempty"`
	TLSKey         string   `json:"tls_key,omitempty"`
}
type hostRequest struct {
	ActorID   string  `json:"actor_id"`
	ID        string  `json:"id,omitempty"`
	ServiceID string  `json:"service_id,omitempty"`
	Change    *Change `json:"change,omitempty"`
}
type snapshot struct {
	Value  string `json:"value,omitempty"`
	Exists bool   `json:"exists"`
	File   []byte `json:"file,omitempty"`
	Hash   string `json:"hash"`
}
type HostPlan struct {
	ID                   string     `json:"id"`
	ActorID              string     `json:"actor_id"`
	Change               Change     `json:"change"`
	State                string     `json:"state"`
	CreatedAt            time.Time  `json:"created_at"`
	ExpiresAt            time.Time  `json:"expires_at"`
	ConfirmationDeadline *time.Time `json:"confirmation_deadline,omitempty"`
	Before               snapshot   `json:"-"`
	Error                string     `json:"error,omitempty"`
}
type storedPlan struct {
	HostPlan
	Before snapshot  `json:"before"`
	After  *snapshot `json:"after,omitempty"`
}
type ServiceState struct {
	ServiceProfile
	ActiveState   string `json:"active_state"`
	UnitFileState string `json:"unit_file_state"`
	FragmentPath  string `json:"fragment_path,omitempty"`
}

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,95}$`)
var unitName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.@-]{0,150}\.(service|timer)$`)

type HostError struct {
	Code, Message string
	Status        int
}

func (e *HostError) Error() string             { return e.Message }
func (e *HostError) AgentError() (string, int) { return e.Code, e.Status }

var ErrPlanConflict = &HostError{Code: "host_conflict", Message: "host state changed since plan preparation", Status: 409}
var ErrPlanExpired = &HostError{Code: "plan_expired", Message: "host change plan expired", Status: 410}
var ErrConfirmation = &HostError{Code: "confirmation_required", Message: "connection protection requires explicit confirmation", Status: 409}

func strictJSON(raw []byte, target any) error {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return errors.New("bounded JSON input required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("one JSON document required")
	}
	return nil
}
func hashBytes(value []byte) string { sum := sha256.Sum256(value); return hex.EncodeToString(sum[:]) }

func LoadHostConfig(path string) (HostConfig, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return HostConfig{}, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return HostConfig{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return HostConfig{}, errors.New("system policy must be private and owned by Agent UID")
	}
	value, err := io.ReadAll(io.LimitReader(file, 64<<10+1))
	if err != nil {
		return HostConfig{}, err
	}
	var result HostConfig
	err = strictJSON(value, &result)
	return result, err
}

func readPrivateJSON(path string, budget int64) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > budget {
		return nil, errors.New("invalid root-private metadata")
	}
	value, err := io.ReadAll(io.LimitReader(file, budget+1))
	if err != nil {
		return nil, err
	}
	if int64(len(value)) > budget || !json.Valid(value) {
		return nil, errors.New("invalid bounded private JSON")
	}
	return value, nil
}

func privateDir(path string) error {
	if !filepath.IsAbs(path) || path == "/" || path == "/tmp" || path == "/workspace" {
		return errors.New("dedicated absolute private directory required")
	}
	if err := trustedRootParents(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || st.Uid != uint32(os.Geteuid()) {
		return errors.New("Agent state must be an owned private real directory")
	}
	return nil
}
func trustedProgram(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("trusted absolute program required")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if err = trustedRootParents(resolved); err != nil {
		return err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("program is not trusted by Agent UID: %s", path)
	}
	return nil
}

// A root-owned child beneath an API-owned parent can still be renamed by the
// API. Production state/program ancestors must therefore remain root-owned.
// The sticky root-owned /tmp ancestor is safe for private test directories.
func trustedRootParents(path string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	for parent := filepath.Dir(filepath.Clean(path)); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if os.IsNotExist(err) {
			if parent == "/" {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0) {
			return errors.New("privileged path ancestor is writable or owned by another UID")
		}
		if parent == "/" {
			return nil
		}
	}
}

func writePrivate(path string, value []byte) error {
	dir := filepath.Dir(path)
	if err := privateDir(dir); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".transaction-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(value)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
