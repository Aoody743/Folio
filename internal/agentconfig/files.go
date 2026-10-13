package agentconfig

import (
	"errors"
	"github.com/Aoody743/PowerLinux/internal/files"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

type FileRoot struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Path       string `json:"path"`
	ReadOnly   bool   `json:"read_only"`
	HostAccess bool   `json:"host_access"`
}
type FileConfig struct {
	Version          int               `json:"version"`
	StateDir         string            `json:"state_dir"`
	APIDataDir       string            `json:"api_data_dir"`
	Roots            []FileRoot        `json:"roots"`
	ProtectedPaths   []string          `json:"protected_paths,omitempty"`
	ArchiveTools     map[string]string `json:"archive_tools,omitempty"`
	DownloadHosts    []string          `json:"download_hosts,omitempty"`
	MaxTransferBytes int64             `json:"max_transfer_bytes,omitempty"`
}

func validateFiles(config FileConfig) error {
	if config.Version != 1 || !filepath.IsAbs(config.StateDir) || filepath.Clean(config.StateDir) == "/" || len(config.Roots) > 128 {
		return errors.New("files require policy version one, private state and at most 128 roots")
	}
	if config.MaxTransferBytes < 0 || config.MaxTransferBytes > 64<<30 {
		return errors.New("invalid filesystem transfer budget")
	}
	if err := protectedFileAuthority(config.StateDir); err != nil {
		return err
	}
	if info, err := os.Lstat(config.StateDir); err == nil {
		if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return errors.New("filesystem state must be a private root-owned directory")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for name, program := range config.ArchiveTools {
		if name != "xz" && name != "bzip2" && name != "7z" && name != "rar" {
			return errors.New("unknown trusted archive tool")
		}
		if !filepath.IsAbs(program) {
			return errors.New("archive tools must have absolute trusted paths")
		}
		canonical, err := filepath.EvalSymlinks(program)
		if err != nil {
			return err
		}
		info, err := os.Stat(canonical)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
			return errors.New("archive tools must be protected root-owned executables")
		}
		if err = protectedFileAuthority(filepath.Dir(canonical)); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	validID := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,79}$`)
	for _, root := range config.Roots {
		if !validID.MatchString(root.ID) || seen[root.ID] || root.ID == "workspace" || !filepath.IsAbs(root.Path) {
			return errors.New("file roots must have distinct IDs and absolute paths; workspace is reserved for the API")
		}
		seen[root.ID] = true
		if filepath.Clean(root.Path) == "/" && !root.HostAccess {
			return errors.New("a full host root requires explicit host_access")
		}
		if root.HostAccess && (!filepath.IsAbs(config.APIDataDir) || filepath.Clean(config.APIDataDir) == "/") {
			return errors.New("host roots require the exact protected API data directory")
		}
		info, err := os.Lstat(root.Path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("configured file roots must already be real directories")
		}
	}
	for _, path := range append(append([]string{}, config.ProtectedPaths...), config.StateDir) {
		if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
			return errors.New("private file paths must be absolute non-root directories")
		}
	}
	return nil
}
func protectedFileAuthority(path string) error {
	for parent := filepath.Clean(path); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || stat.Uid != 0 || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0) {
				return errors.New("filesystem authority parents must be protected root-owned real directories")
			}
		}
		if parent == "/" {
			return nil
		}
	}
}
func (config FileConfig) Options(privatePaths []string) files.Options {
	roots := make([]files.RootSpec, 0, len(config.Roots))
	for _, root := range config.Roots {
		roots = append(roots, files.RootSpec{ID: root.ID, Label: root.Label, Path: root.Path, ReadOnly: root.ReadOnly, HostAccess: root.HostAccess, DefaultFileMode: 0600, DefaultDirMode: 0700})
	}
	protected := append([]string{"/etc/powerlinux", "/var/lib/powerlinux", "/var/lib/powerlinux-agent"}, privatePaths...)
	protected = append(protected, config.ProtectedPaths...)
	if config.APIDataDir != "" {
		protected = append(protected, config.APIDataDir)
	}
	return files.Options{Roots: roots, StateDir: config.StateDir, ProtectedPaths: protected, ArchiveTools: config.ArchiveTools, DownloadHosts: config.DownloadHosts, MaxTransferBytes: config.MaxTransferBytes}
}
