package sites

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// ValidateAgentConfig reads trusted filesystem metadata without creating users,
// directories, sockets, services or restoration timers.
func ValidateAgentConfig(config AgentConfig) error {
	if config.Version != 1 || !filepath.IsAbs(config.RootDir) || !filepath.IsAbs(config.StateDir) || filepath.Clean(config.RootDir) == "/" || filepath.Clean(config.StateDir) == "/" {
		return errors.New("website Agent needs version one and absolute owned root/state directories")
	}
	if filepath.Clean(config.RootDir) == filepath.Clean(config.StateDir) {
		return errors.New("website public root and private state must be separate")
	}
	for _, path := range []string{config.RootDir, config.StateDir} {
		if slices.Contains([]string{"/etc", "/usr", "/usr/local", "/var", "/var/lib", "/var/log", "/srv", "/opt", "/tmp", "/workspace", "/home", "/root"}, filepath.Clean(path)) {
			return errors.New("website authority requires dedicated directories rather than shared system roots")
		}
	}
	if len(config.AllowedPorts) > 128 {
		return errors.New("too many allowed website ports")
	}
	for _, port := range config.AllowedPorts {
		if port < 1 || port > 65535 {
			return errors.New("invalid website port policy")
		}
	}
	if err := validateNativeDNSPolicy(config); err != nil {
		return err
	}
	for name, program := range config.Programs {
		if name != "useradd" && name != "userdel" && name != "usermod" {
			return errors.New("unrecognized website administration program")
		}
		if err := trustedProgram(program); err != nil {
			return err
		}
	}
	if config.ACMEProgram != "" {
		if err := trustedProgram(config.ACMEProgram); err != nil {
			return err
		}
	}
	for _, directory := range []string{config.RootDir, config.StateDir} {
		if err := protectedParents(directory); err != nil {
			return err
		}
	}
	return nil
}

func validateNativeDNSPolicy(config AgentConfig) error {
	if len(config.AllowedDNSUpdateServers) > 1 || len(config.AllowedDNSExclusiveTXTSlots) > 1 {
		return errors.New("native DNS policies only support explicitly named Knot servers and DDNSS TXT slots")
	}
	for provider, endpoints := range config.AllowedDNSUpdateServers {
		if provider != "dns_knot" || len(endpoints) < 1 || len(endpoints) > 16 {
			return errors.New("Knot requires one to sixteen explicit update endpoints")
		}
		seen := map[string]bool{}
		for _, endpoint := range endpoints {
			address, err := netip.ParseAddrPort(endpoint)
			if err != nil || address.Port() == 0 || address.Addr().IsUnspecified() || address.Addr().IsMulticast() || address.String() != endpoint || seen[endpoint] {
				return errors.New("DNS update endpoints require distinct canonical literal IPv4 or bracketed IPv6 addresses and nonzero ports")
			}
			seen[endpoint] = true
		}
	}
	for provider, names := range config.AllowedDNSExclusiveTXTSlots {
		if provider != "dns_ddnss" || len(names) < 1 || len(names) > 256 {
			return errors.New("DDNSS requires one to 256 explicit exclusive TXT challenge names")
		}
		seen := map[string]bool{}
		for _, name := range names {
			if !strings.HasPrefix(name, "_acme-challenge.") || strings.HasSuffix(name, ".") || seen[name] {
				return errors.New("exclusive DNS slots require distinct full normalized ACME TXT names")
			}
			base := strings.TrimPrefix(name, "_acme-challenge.")
			canonical, _, port, err := domain(base)
			if err != nil || canonical != base || port != 80 || strings.ContainsAny(base, ":*") {
				return errors.New("exclusive DNS slots require canonical ACME challenge hostnames")
			}
			seen[name] = true
		}
	}
	return nil
}

func trustedProgram(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("website programs must be absolute trusted paths")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 || !ok || stat.Uid != 0 {
		return errors.New("website programs must be executable, root-owned and not writable by workers")
	}
	return protectedParents(filepath.Dir(canonical))
}

func protectedParents(path string) error {
	for directory := filepath.Clean(path); ; directory = filepath.Dir(directory) {
		info, err := os.Lstat(directory)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || stat.Uid != 0 {
				return errors.New("website authority paths must have root-owned real directory parents")
			}
			if info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0 {
				return errors.New("website authority parents must not be writable by API/PHP users")
			}
		}
		if directory == "/" {
			break
		}
	}
	return nil
}
