// Package plugins owns trusted software recipes and typed service adapters.
// Its catalog never contains legacy Python/Shell entrypoints. The web/MCP
// identity and durable task engine are injected by core, not recreated here.
package plugins

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

//go:embed catalog/core.json
var catalogFiles embed.FS

type Catalog struct {
	SchemaVersion int        `json:"schema_version"`
	Items         []Manifest `json:"items"`
	byID          map[string]int
}
type Manifest struct {
	ID                 string            `json:"id"`
	Name               string            `json:"name"`
	Description        string            `json:"description"`
	Category           string            `json:"category"`
	IntegrationVersion string            `json:"integration_version"`
	Versions           []Version         `json:"versions"`
	Platforms          []Platform        `json:"platforms"`
	Dependencies       []string          `json:"dependencies,omitempty"`
	Capabilities       []Capability      `json:"capabilities"`
	Coexist            bool              `json:"coexist"`
	Service            ServiceDefinition `json:"service"`
	Verification       Verification      `json:"verification"`
}
type Version struct {
	ID           string       `json:"id"`
	Version      string       `json:"version"`
	Default      bool         `json:"default"`
	LegacyLabels []string     `json:"legacy_labels,omitempty"`
	Source       Source       `json:"source"`
	Verification Verification `json:"verification"`
}
type Platform struct {
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Distro  string `json:"distro"`
	Release string `json:"release"`
}
type Source struct {
	Kind             string   `json:"kind"` // signed_package, verified_archive, integration
	Packages         []string `json:"packages,omitempty"`
	Repository       string   `json:"repository,omitempty"`
	RepositoryKeyURL string   `json:"repository_key_url,omitempty"`
	URL              string   `json:"url,omitempty"`
	SHA256           string   `json:"sha256,omitempty"`
	ArchiveFormat    string   `json:"archive_format,omitempty"`
	StripComponents  int      `json:"strip_components,omitempty"`
	BuildRecipe      string   `json:"build_recipe,omitempty"`
	OS               string   `json:"os,omitempty"`
	Architecture     string   `json:"architecture,omitempty"`
	Trust            string   `json:"trust"`
}
type Capability struct {
	Name          string   `json:"name"`
	LegacyActions []string `json:"legacy_actions"`
	Status        string   `json:"status"`
	Reason        string   `json:"reason,omitempty"`
}
type Verification struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}
type ServiceDefinition struct {
	Programs    []string `json:"programs,omitempty"`
	UnitBase    string   `json:"unit_base,omitempty"`
	Health      string   `json:"health,omitempty"`
	DefaultPort int      `json:"default_port,omitempty"`
	ConfigKind  string   `json:"config_kind,omitempty"`
}

var validID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var packageName = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{0,127}$`)
var artifactHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

func BuiltinCatalog() (*Catalog, error) {
	file, err := catalogFiles.Open("catalog/core.json")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return DecodeCatalog(file)
}

// DecodeCatalog is for trusted configuration only. Uploads/API inputs cannot
// replace the embedded catalog or add executable paths/install URLs.
func DecodeCatalog(reader io.Reader) (*Catalog, error) {
	data, err := io.ReadAll(io.LimitReader(reader, (2<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 2<<20 {
		return nil, errors.New("catalog exceeds 2 MiB")
	}
	var catalog Catalog
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&catalog); err != nil {
		return nil, err
	}
	if catalog.SchemaVersion != 1 || len(catalog.Items) == 0 || len(catalog.Items) > 128 {
		return nil, errors.New("catalog requires schema version 1 and bounded plugin count")
	}
	catalog.byID = map[string]int{}
	for i, item := range catalog.Items {
		if !validID.MatchString(item.ID) || strings.TrimSpace(item.Name) == "" || item.IntegrationVersion == "" {
			return nil, fmt.Errorf("invalid manifest %q", item.ID)
		}
		if _, exists := catalog.byID[item.ID]; exists {
			return nil, fmt.Errorf("duplicate plugin %s", item.ID)
		}
		if len(item.Versions) == 0 || len(item.Versions) > 64 {
			return nil, fmt.Errorf("plugin %s has invalid version count", item.ID)
		}
		seen := map[string]bool{}
		for _, version := range item.Versions {
			if version.ID == "" || len(version.ID) > 80 || seen[version.ID] {
				return nil, fmt.Errorf("plugin %s has invalid/duplicate version", item.ID)
			}
			seen[version.ID] = true
			source := version.Source
			if source.OS != "" && source.OS != "linux" {
				return nil, errors.New("native artifacts support Linux only")
			}
			if source.Architecture != "" && !slices.Contains([]string{"amd64", "arm64"}, source.Architecture) {
				return nil, errors.New("native artifact architecture is unreviewed")
			}
			switch source.Kind {
			case "signed_package":
				if source.Trust != "repository_signature" || len(source.Packages) == 0 || len(source.Packages) > 32 {
					return nil, fmt.Errorf("plugin %s requires signed package selection", item.ID)
				}
				for _, pkg := range source.Packages {
					if !packageName.MatchString(pkg) {
						return nil, fmt.Errorf("invalid trusted package name %s", pkg)
					}
				}
			case "verified_archive", "verified_source":
				if source.Trust != "pinned_sha256" || !artifactHash.MatchString(source.SHA256) || !trustedHTTPS(source.URL) || source.StripComponents < 0 || source.StripComponents > 4 || !slices.Contains([]string{"tar.gz", "tar.xz", "zip"}, source.ArchiveFormat) {
					return nil, fmt.Errorf("plugin %s requires a trusted pinned artifact", item.ID)
				}
				if source.Kind == "verified_source" && !(item.ID == "openresty" && source.BuildRecipe == "openresty" || item.ID == "php" && source.BuildRecipe == "php-fpm") {
					return nil, errors.New("source builds require a compiled-in native recipe for this exact plugin")
				}
				if source.Kind == "verified_source" {
					for _, name := range source.Packages {
						if !packageName.MatchString(name) {
							return nil, errors.New("invalid trusted source-build dependency")
						}
					}
				}
			case "integration":
				if source.Trust != "compiled_in" {
					return nil, errors.New("integration must be compiled into the trusted binary")
				}
			case "unavailable":
				if version.Verification.Reason == "" {
					return nil, errors.New("unavailable version must explain its blocker")
				}
			default:
				return nil, fmt.Errorf("unknown source kind %s", source.Kind)
			}
			if source.Repository != "" && !trustedHTTPS(source.Repository) {
				return nil, errors.New("repository must use verified HTTPS")
			}
			if source.RepositoryKeyURL != "" && !trustedHTTPS(source.RepositoryKeyURL) {
				return nil, errors.New("repository key must use verified HTTPS")
			}
		}
		catalog.byID[item.ID] = i
	}
	return &catalog, nil
}

func trustedHTTPS(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == ""
}
func (c *Catalog) Get(id string) (Manifest, error) {
	if c == nil {
		return Manifest{}, errors.New("catalog not configured")
	}
	index, ok := c.byID[id]
	if !ok {
		return Manifest{}, fmt.Errorf("unknown plugin %q", id)
	}
	return c.Items[index], nil
}
func (m Manifest) SelectVersion(id string) (Version, error) {
	for _, version := range m.Versions {
		if version.ID == id || id == "" && version.Default {
			return version, nil
		}
	}
	return Version{}, errors.New("software version is not in the trusted catalog")
}
func CurrentPlatform() Platform {
	platform := Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
	raw, _ := os.ReadFile("/etc/os-release")
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"'`)
		switch key {
		case "ID":
			platform.Distro = value
		case "VERSION_ID":
			platform.Release = value
		}
	}
	return platform
}
func (m Manifest) Supports(platform Platform) bool {
	for _, candidate := range m.Platforms {
		if candidate.OS == platform.OS && candidate.Arch == platform.Arch && (candidate.Distro == "" || candidate.Distro == platform.Distro) && (candidate.Release == "" || candidate.Release == platform.Release || (!strings.Contains(candidate.Release, ".") && strings.HasPrefix(platform.Release, candidate.Release+"."))) {
			return true
		}
	}
	return false
}
