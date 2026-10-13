// Package agentconfig parses operator-owned deployment policy. It has no
// side-effecting constructors; check-policy uses only read-only validators.
package agentconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/plugins"
	"github.com/Aoody743/PowerLinux/internal/sites"
	"github.com/Aoody743/PowerLinux/internal/systemops"
	"github.com/Aoody743/PowerLinux/internal/terminal"
)

type Config struct {
	Version      int                       `json:"version"`
	AllowedUIDs  []uint32                  `json:"allowed_uids,omitempty"`
	AllowedUsers []string                  `json:"allowed_users,omitempty"`
	SocketGroup  string                    `json:"socket_group,omitempty"`
	Roots        []agent.RootSpec          `json:"roots,omitempty"`
	Operations   []string                  `json:"operations,omitempty"`
	MaxFileBytes int64                     `json:"max_file_bytes,omitempty"`
	Plugins      *plugins.PrivilegedConfig `json:"plugins,omitempty"`
	Host         *systemops.HostConfig     `json:"host,omitempty"`
	Sites        *sites.AgentConfig        `json:"sites,omitempty"`
	Terminal     *terminal.Config          `json:"terminal,omitempty"`
	Files        *FileConfig               `json:"files,omitempty"`
	SHA256       string                    `json:"-"`
}

func Load(path string) (Config, error) {
	var config Config
	if !filepath.IsAbs(path) {
		return config, errors.New("Agent policy path must be absolute")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return config, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return config, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return config, err
	}
	if !os.SameFile(before, info) || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return config, errors.New("policy must be a stable regular file without group/world write permission")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return config, errors.New("policy must belong to the Agent service UID")
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return config, err
	}
	if len(data) > 1<<20 {
		return config, errors.New("Agent policy exceeds one MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&config) != nil || decoder.Decode(new(any)) != io.EOF {
		return config, errors.New("Agent policy must contain exactly one known JSON object")
	}
	if config.Version == 2 {
		if err := protectedFileAuthority(filepath.Dir(path)); err != nil {
			return config, err
		}
	}
	digest := sha256.Sum256(data)
	config.SHA256 = hex.EncodeToString(digest[:])
	if err = Validate(&config); err != nil {
		return Config{}, err
	}
	return config, nil
}

func Validate(config *Config) error {
	if config.Version != 1 && config.Version != 2 {
		return errors.New("Agent policy version must be 1 or 2")
	}
	if config.Version == 1 && (len(config.AllowedUsers) > 0 || config.SocketGroup != "" || config.Plugins != nil || config.Host != nil || config.Sites != nil || config.Terminal != nil || config.Files != nil) {
		return errors.New("typed Agent modules require policy version 2")
	}
	if config.Version == 2 && os.Geteuid() != 0 {
		return errors.New("privileged module policies must be checked and served by root")
	}
	if len(config.AllowedUIDs)+len(config.AllowedUsers) < 1 || len(config.AllowedUIDs)+len(config.AllowedUsers) > 64 {
		return errors.New("Agent policy requires one to 64 explicitly allowed peer identities")
	}
	seen := map[uint32]bool{}
	uids := []uint32{}
	for _, uid := range config.AllowedUIDs {
		if !seen[uid] {
			uids = append(uids, uid)
			seen[uid] = true
		}
	}
	for _, name := range config.AllowedUsers {
		record, err := user.Lookup(name)
		if err != nil {
			return errors.New("an allowed Agent user does not exist")
		}
		number, err := strconv.ParseUint(record.Uid, 10, 32)
		if err != nil {
			return errors.New("an allowed Agent user has an invalid UID")
		}
		uid := uint32(number)
		if !seen[uid] {
			uids = append(uids, uid)
			seen[uid] = true
		}
	}
	config.AllowedUIDs = uids
	if config.SocketGroup != "" {
		if _, err := user.LookupGroup(config.SocketGroup); err != nil {
			return errors.New("Agent socket group does not exist")
		}
	}
	if (len(config.Roots) == 0) != (len(config.Operations) == 0) {
		return errors.New("legacy file roots and operations must be configured together")
	}
	if config.MaxFileBytes < 0 || config.MaxFileBytes > 128<<20 {
		return errors.New("invalid Agent filesystem budget")
	}
	if len(config.Roots) > 128 || len(config.Operations) > 3 {
		return errors.New("Agent filesystem policy exceeds its limits")
	}
	for _, operation := range config.Operations {
		if operation != "file_write" && operation != "file_hash" && operation != "file_backup" {
			return errors.New("legacy file operation is not allowed")
		}
	}
	if config.Plugins != nil {
		// Module validators below must remain read-only.
		if err := plugins.ValidatePrivilegedConfig(*config.Plugins); err != nil {
			return err
		}
	}
	rootIDs := map[string]bool{}
	rootName := regexp.MustCompile(`^[a-z][a-z0-9_]{0,95}$`)
	for _, root := range config.Roots {
		if !rootName.MatchString(root.ID) || rootIDs[root.ID] || !filepath.IsAbs(root.Path) {
			return errors.New("legacy file roots require distinct valid IDs and absolute paths")
		}
		rootIDs[root.ID] = true
		info, err := os.Lstat(root.Path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("legacy file roots must already be real directories")
		}
	}
	if config.Host != nil {
		if err := systemops.ValidateHostConfig(*config.Host); err != nil {
			return err
		}
	}
	if config.Terminal != nil {
		if err := terminal.ValidateConfig(*config.Terminal); err != nil {
			return err
		}
	}
	if config.Sites != nil {
		if config.Plugins == nil {
			return errors.New("website policy requires receipt-backed Web/PHP software management")
		}
		if err := sites.ValidateAgentConfig(*config.Sites); err != nil {
			return err
		}
	}
	if config.Files != nil {
		if err := validateFiles(*config.Files); err != nil {
			return err
		}
	}
	if err := validateSourceBuilderIsolation(*config); err != nil {
		return err
	}
	if len(config.Roots) == 0 && config.Plugins == nil && config.Host == nil && config.Terminal == nil && config.Sites == nil && config.Files == nil {
		return errors.New("Agent policy must enable at least one explicit module")
	}
	return nil
}

func (c Config) SocketGID() (*uint32, error) {
	if c.SocketGroup == "" {
		return nil, nil
	}
	group, err := user.LookupGroup(c.SocketGroup)
	if err != nil {
		return nil, err
	}
	value, err := strconv.ParseUint(group.Gid, 10, 32)
	if err != nil {
		return nil, err
	}
	gid := uint32(value)
	return &gid, nil
}
func (c Config) LegacyPolicy() agent.Policy {
	return agent.Policy{Version: 1, AllowedUIDs: c.AllowedUIDs, Roots: c.Roots, Operations: c.Operations, MaxFileBytes: c.MaxFileBytes}
}
