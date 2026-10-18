package systemops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const managedHeader = "# Managed by PowerLinux systemops\n"

func (e *HostExecutor) validateChange(change Change) error {
	if !e.kinds[change.Kind] {
		return errors.New("host change kind is not explicitly enabled")
	}
	extra := change
	extra.Kind = ""
	switch change.Kind {
	case "service_state":
		if _, exists := e.services[change.ServiceID]; !exists {
			return errors.New("service is outside trusted profiles")
		}
		switch change.ServiceState {
		case "start", "stop", "restart", "reload", "enable", "disable":
		default:
			return errors.New("invalid service state")
		}
		extra.ServiceID = ""
		extra.ServiceState = ""
	case "ssh_config":
		if change.SSH == nil || len(change.SSH.Ports) < 1 || len(change.SSH.Ports) > 16 {
			return errors.New("SSH requires 1..16 ports")
		}
		seen := map[int]bool{}
		for _, port := range change.SSH.Ports {
			if port < 1 || port > 65535 || seen[port] {
				return errors.New("invalid or duplicate SSH port")
			}
			seen[port] = true
		}
		if !seen[e.config.ProtectedPorts[0]] {
			return errors.New("SSH configuration must retain the policy's protected management port")
		}
		switch change.SSH.PermitRootLogin {
		case "no", "prohibit-password", "yes":
		default:
			return errors.New("invalid SSH root-login setting")
		}
		if len(change.SSH.AllowUsers) > 64 {
			return errors.New("SSH user limit exceeded")
		}
		for _, name := range change.SSH.AllowUsers {
			if !identifier.MatchString(name) {
				return errors.New("invalid SSH user")
			}
		}
		extra.SSH = nil
	case "firewall_rules":
		if change.Firewall == nil || len(change.Firewall.Rules) > 128 {
			return errors.New("bounded firewall rules required")
		}
		if change.Firewall.DefaultPolicy != "accept" && change.Firewall.DefaultPolicy != "drop" {
			return errors.New("firewall default policy must be accept or drop")
		}
		seen := map[string]bool{}
		for _, rule := range change.Firewall.Rules {
			if !identifier.MatchString(rule.ID) || seen[rule.ID] {
				return errors.New("invalid or duplicate firewall rule ID")
			}
			seen[rule.ID] = true
			if rule.Protocol != "tcp" && rule.Protocol != "udp" && rule.Protocol != "any" {
				return errors.New("invalid firewall protocol")
			}
			if rule.Port < 0 || rule.Port > 65535 || (rule.Protocol == "any" && rule.Port != 0) {
				return errors.New("invalid firewall port")
			}
			if rule.Action != "accept" && rule.Action != "drop" && rule.Action != "reject" {
				return errors.New("invalid firewall action")
			}
			if rule.Source != "" {
				if _, err := netip.ParsePrefix(rule.Source); err != nil {
					return errors.New("firewall source must be CIDR")
				}
			}
			if rule.Action != "accept" && rule.Protocol == "tcp" {
				for _, port := range e.config.ProtectedPorts {
					if rule.Port == port {
						return errors.New("cannot deny a protected management port")
					}
				}
			}
		}
		extra.Firewall = nil
	case "timezone":
		if change.Timezone == "" || len(change.Timezone) > 128 || strings.Contains(change.Timezone, "..") || strings.ContainsAny(change.Timezone, "\x00\n\r ") || filepath.IsAbs(change.Timezone) {
			return errors.New("invalid IANA timezone")
		}
		file, err := os.Stat(e.path("/usr/share/zoneinfo/" + change.Timezone))
		if err != nil || !file.Mode().IsRegular() {
			return errors.New("timezone is not installed")
		}
		extra.Timezone = ""
	case "ntp":
		if change.NTP == nil {
			return errors.New("NTP boolean required")
		}
		extra.NTP = nil
	case "power":
		if change.Power != "reboot" && change.Power != "poweroff" {
			return errors.New("invalid power action")
		}
		if change.DelaySeconds < 60 || change.DelaySeconds > 600 {
			return errors.New("power delay must be 60..600 seconds")
		}
		extra.Power = ""
		extra.DelaySeconds = 0
	case "panel_config":
		if change.Panel == nil {
			return errors.New("panel config required")
		}
		if err := ValidateServerConfig(*change.Panel); err != nil {
			return err
		}
		if _, ok := e.services["panel"]; !ok {
			return errors.New("panel service must be explicitly declared")
		}
		extra.Panel = nil
	default:
		return errors.New("unsupported change")
	}
	if !reflect.DeepEqual(extra, Change{}) {
		return errors.New("parameters do not belong to the selected change kind")
	}
	return nil
}

func (e *HostExecutor) readOwnedConfig(path string) (snapshot, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return snapshot{Hash: hashBytes(nil)}, nil
	}
	if err != nil {
		return snapshot{}, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return snapshot{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return snapshot{}, errors.New("managed configuration is not owned/trusted")
	}
	value, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil {
		return snapshot{}, err
	}
	if len(value) > 1<<20 {
		return snapshot{}, errors.New("managed configuration too large")
	}
	if !bytes.HasPrefix(value, []byte(managedHeader)) {
		return snapshot{}, errors.New("refusing to replace an unowned configuration file")
	}
	return snapshot{Exists: true, File: value, Hash: hashBytes(value)}, nil
}
func (e *HostExecutor) capture(ctx context.Context, change Change) (snapshot, error) {
	var result snapshot
	switch change.Kind {
	case "service_state":
		state, err := e.serviceState(ctx, e.services[change.ServiceID])
		if err != nil {
			return snapshot{}, err
		}
		raw, _ := json.Marshal(state)
		result.Value = string(raw)
		result.Exists = true
	case "ssh_config":
		var err error
		result, err = e.readOwnedConfig(e.path("/etc/ssh/sshd_config.d/powerlinux.conf"))
		return result, err
	case "firewall_rules":
		out, err := e.command(ctx, "nft", "list", "table", "inet", "powerlinux")
		if err != nil {
			if strings.Contains(out.Stderr, "No such file or directory") {
				result.Hash = hashBytes(nil)
				return result, nil
			}
			return snapshot{}, err
		}
		if _, err = os.Stat(filepath.Join(e.config.StateDir, "firewall-owned")); err != nil {
			return snapshot{}, errors.New("existing firewall table is not owned by Folio")
		}
		result.Value = out.Stdout
		result.Exists = true
	case "timezone", "ntp":
		property := "Timezone"
		if change.Kind == "ntp" {
			property = "NTP"
		}
		out, err := e.command(ctx, "timedatectl", "show", "--property="+property, "--value")
		if err != nil {
			return snapshot{}, err
		}
		result.Value = strings.TrimSpace(out.Stdout)
		result.Exists = true
	case "power":
		value, err := os.ReadFile(e.path("/run/systemd/shutdown/scheduled"))
		if err == nil {
			result.Value = string(value)
			result.Exists = true
		} else if !os.IsNotExist(err) {
			return snapshot{}, err
		}
	case "panel_config":
		value, err := readPrivateServerConfig(e.path("/etc/powerlinux/server.json"))
		if err != nil {
			return snapshot{}, err
		}
		result.File = value
		result.Exists = true
		result.Hash = hashBytes(value)
		return result, nil
	default:
		return snapshot{}, errors.New("invalid change")
	}
	result.Hash = hashBytes([]byte(result.Value))
	return result, nil
}
func (e *HostExecutor) preflight(ctx context.Context, change Change, before snapshot) error {
	if err := e.validateChange(change); err != nil {
		return err
	}
	switch change.Kind {
	case "ssh_config":
		_, err := e.command(ctx, "sshd", "-t")
		return err
	case "firewall_rules":
		value := e.firewallText(*change.Firewall, before.Exists)
		file, err := e.stage(value)
		if err != nil {
			return err
		}
		defer os.Remove(file)
		_, err = e.command(ctx, "nft", "--check", "--file", file)
		return err
	case "ntp":
		out, err := e.command(ctx, "timedatectl", "show", "--property=CanNTP", "--value")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out.Stdout) != "yes" {
			return errors.New("host has no supported time-synchronization service")
		}
	case "power":
		if before.Exists {
			return errors.New("another power transition is already scheduled")
		}
	}
	return nil
}

func (e *HostExecutor) writeManaged(path string, value []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 || st.Uid != uint32(os.Geteuid()) {
		return errors.New("managed parent directory is not owned/trusted")
	}
	file, err := os.CreateTemp(dir, ".powerlinux-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0644); err == nil {
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
func (e *HostExecutor) stage(value []byte) (string, error) {
	file, err := os.CreateTemp(e.config.StateDir, ".preflight-")
	if err != nil {
		return "", err
	}
	name := file.Name()
	if _, err = file.Write(value); err != nil {
		file.Close()
		os.Remove(name)
		return "", err
	}
	if err = file.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}
func sshText(config SSHConfig) []byte {
	var out strings.Builder
	out.WriteString(managedHeader)
	ports := append([]int(nil), config.Ports...)
	sort.Ints(ports)
	for _, port := range ports {
		fmt.Fprintf(&out, "Port %d\n", port)
	}
	password := "no"
	if config.PasswordAuthentication {
		password = "yes"
	}
	fmt.Fprintf(&out, "PasswordAuthentication %s\nPermitRootLogin %s\n", password, config.PermitRootLogin)
	if len(config.AllowUsers) > 0 {
		fmt.Fprintf(&out, "AllowUsers %s\n", strings.Join(config.AllowUsers, " "))
	}
	return []byte(out.String())
}
func (e *HostExecutor) firewallText(config FirewallConfig, replace bool) []byte {
	var out strings.Builder
	if replace {
		out.WriteString("delete table inet powerlinux\n")
	}
	out.WriteString("table inet powerlinux {\n chain input {\n  type filter hook input priority 10; policy " + config.DefaultPolicy + ";\n  ct state established,related accept\n  iifname \"lo\" accept\n")
	ports := []string{}
	for _, port := range e.config.ProtectedPorts {
		ports = append(ports, strconv.Itoa(port))
	}
	fmt.Fprintf(&out, "  tcp dport { %s } accept\n", strings.Join(ports, ", "))
	for _, rule := range config.Rules {
		out.WriteString("  ")
		if rule.Source != "" {
			prefix, _ := netip.ParsePrefix(rule.Source)
			kind := "ip"
			if prefix.Addr().Is6() {
				kind = "ip6"
			}
			fmt.Fprintf(&out, "%s saddr %s ", kind, prefix.Masked())
		}
		if rule.Protocol != "any" {
			if rule.Port != 0 {
				fmt.Fprintf(&out, "%s dport %d ", rule.Protocol, rule.Port)
			} else {
				fmt.Fprintf(&out, "meta l4proto %s ", rule.Protocol)
			}
		}
		fmt.Fprintf(&out, "%s comment \"powerlinux:%s\"\n", rule.Action, rule.ID)
	}
	out.WriteString(" }\n}\n")
	return []byte(out.String())
}
func (e *HostExecutor) commit(ctx context.Context, change Change, before snapshot) error {
	switch change.Kind {
	case "service_state":
		_, err := e.command(ctx, "systemctl", change.ServiceState, e.services[change.ServiceID].Unit)
		return err
	case "timezone":
		_, err := e.command(ctx, "timedatectl", "set-timezone", change.Timezone)
		return err
	case "ntp":
		_, err := e.command(ctx, "timedatectl", "set-ntp", strconv.FormatBool(*change.NTP))
		return err
	case "ssh_config":
		if err := e.writeManaged(e.path("/etc/ssh/sshd_config.d/powerlinux.conf"), sshText(*change.SSH)); err != nil {
			return err
		}
		if _, err := e.command(ctx, "sshd", "-t"); err != nil {
			return err
		}
		profile, ok := e.services["ssh"]
		if !ok {
			return errors.New("SSH service must be explicitly adopted in policy")
		}
		_, err := e.command(ctx, "systemctl", "reload", profile.Unit)
		return err
	case "firewall_rules":
		file, err := e.stage(e.firewallText(*change.Firewall, before.Exists))
		if err != nil {
			return err
		}
		defer os.Remove(file)
		if _, err = e.command(ctx, "nft", "--file", file); err != nil {
			return err
		}
		if err = writePrivate(filepath.Join(e.config.StateDir, "firewall-owned"), []byte("PowerLinux nftables ownership v1\n")); err != nil {
			return err
		}
		return e.persistFirewall(e.firewallText(*change.Firewall, false))
	case "power":
		mode := "-r"
		if change.Power == "poweroff" {
			mode = "-P"
		}
		_, err := e.command(ctx, "shutdown", mode, "--no-wall", "+"+strconv.Itoa((change.DelaySeconds+59)/60))
		return err
	case "panel_config":
		raw, _ := json.MarshalIndent(change.Panel, "", "  ")
		if err := e.writeServerConfig(raw); err != nil {
			return err
		}
		_, err := e.command(ctx, "systemctl", "restart", e.services["panel"].Unit)
		return err
	}
	return errors.New("unsupported change")
}
func (e *HostExecutor) verify(ctx context.Context, change Change) error {
	switch change.Kind {
	case "service_state":
		state, err := e.serviceState(ctx, e.services[change.ServiceID])
		if err != nil {
			return err
		}
		want := change.ServiceState
		switch want {
		case "start", "restart", "reload":
			if state.ActiveState != "active" {
				return errors.New("service did not become active")
			}
		case "stop":
			if state.ActiveState != "inactive" {
				return errors.New("service did not stop")
			}
		case "enable":
			if state.UnitFileState != "enabled" {
				return errors.New("service not enabled")
			}
		case "disable":
			if state.UnitFileState != "disabled" {
				return errors.New("service not disabled")
			}
		}
		return nil
	case "timezone", "ntp":
		current, err := e.capture(ctx, change)
		if err != nil {
			return err
		}
		want := change.Timezone
		if change.Kind == "ntp" {
			want = "no"
			if *change.NTP {
				want = "yes"
			}
		}
		if current.Value != want {
			return errors.New("host setting did not match requested value")
		}
		return nil
	case "ssh_config":
		out, err := e.command(ctx, "sshd", "-T")
		if err != nil {
			return err
		}
		settings := map[string][]string{}
		for _, line := range strings.Split(out.Stdout, "\n") {
			fields := strings.Fields(line)
			if len(fields) > 1 {
				settings[fields[0]] = append(settings[fields[0]], strings.Join(fields[1:], " "))
			}
		}
		ports := make([]string, 0, len(change.SSH.Ports))
		for _, port := range change.SSH.Ports {
			ports = append(ports, strconv.Itoa(port))
		}
		sort.Strings(ports)
		sort.Strings(settings["port"])
		if !reflect.DeepEqual(ports, settings["port"]) {
			return errors.New("SSH ports were not effective; check Include precedence")
		}
		password := "no"
		if change.SSH.PasswordAuthentication {
			password = "yes"
		}
		if len(settings["passwordauthentication"]) != 1 || settings["passwordauthentication"][0] != password {
			return errors.New("SSH authentication setting was not effective")
		}
		root := change.SSH.PermitRootLogin
		if root == "prohibit-password" {
			root = "without-password"
		}
		if len(settings["permitrootlogin"]) != 1 || settings["permitrootlogin"][0] != root {
			return errors.New("SSH root login setting was not effective")
		}
		users := append([]string{}, change.SSH.AllowUsers...)
		actualUsers := strings.Fields(strings.Join(settings["allowusers"], " "))
		sort.Strings(users)
		sort.Strings(actualUsers)
		if strings.Join(users, " ") != strings.Join(actualUsers, " ") {
			return errors.New("SSH allowed users were not effective; check Include precedence")
		}
		return nil
	case "firewall_rules":
		return e.verifyFirewall(ctx, *change.Firewall)
	case "power":
		value, err := os.ReadFile(e.path("/run/systemd/shutdown/scheduled"))
		if err != nil {
			return err
		}
		if !strings.Contains(string(value), "MODE="+change.Power) {
			return errors.New("power transition was not scheduled as requested")
		}
		return nil
	case "panel_config":
		return e.checkPanelHealth(ctx, *change.Panel)
	}
	return errors.New("unsupported verification")
}
func (e *HostExecutor) restore(ctx context.Context, change Change, before snapshot) error {
	switch change.Kind {
	case "service_state":
		var state ServiceState
		if err := json.Unmarshal([]byte(before.Value), &state); err != nil {
			return err
		}
		unit := e.services[change.ServiceID].Unit
		if state.Unit != unit {
			return ErrPlanConflict
		}
		if state.UnitFileState == "enabled" || state.UnitFileState == "disabled" {
			if _, err := e.command(ctx, "systemctl", strings.TrimSuffix(state.UnitFileState, "d"), unit); err != nil {
				return err
			}
		}
		command := "stop"
		if state.ActiveState == "active" {
			command = "start"
		}
		_, err := e.command(ctx, "systemctl", command, unit)
		return err
	case "timezone":
		_, err := e.command(ctx, "timedatectl", "set-timezone", before.Value)
		return err
	case "ntp":
		value := "false"
		if before.Value == "yes" {
			value = "true"
		}
		_, err := e.command(ctx, "timedatectl", "set-ntp", value)
		return err
	case "ssh_config":
		path := e.path("/etc/ssh/sshd_config.d/powerlinux.conf")
		if before.Exists {
			if err := e.writeManaged(path, before.File); err != nil {
				return err
			}
		} else if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		if _, err := e.command(ctx, "sshd", "-t"); err != nil {
			return err
		}
		profile, ok := e.services["ssh"]
		if !ok {
			return errors.New("SSH service policy missing")
		}
		_, err := e.command(ctx, "systemctl", "reload", profile.Unit)
		return err
	case "firewall_rules":
		var value []byte
		current, err := e.capture(ctx, change)
		if err != nil {
			return err
		}
		if before.Exists {
			if current.Exists {
				value = []byte("delete table inet powerlinux\n")
			}
			value = append(value, []byte(before.Value)...)
		} else {
			if !current.Exists {
				if err = os.Remove(filepath.Join(e.config.StateDir, "firewall-current.json")); err != nil && !os.IsNotExist(err) {
					return err
				}
				return nil
			}
			value = []byte("delete table inet powerlinux\n")
		}
		file, err := e.stage(value)
		if err != nil {
			return err
		}
		defer os.Remove(file)
		_, err = e.command(ctx, "nft", "--file", file)
		if err != nil {
			return err
		}
		if before.Exists {
			return e.persistFirewall([]byte(before.Value))
		}
		if err = os.Remove(filepath.Join(e.config.StateDir, "firewall-current.json")); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	case "power":
		_, err := e.command(ctx, "shutdown", "-c", "--no-wall")
		return err
	case "panel_config":
		if err := e.writeServerConfig(before.File); err != nil {
			return err
		}
		_, err := e.command(ctx, "systemctl", "restart", e.services["panel"].Unit)
		if err != nil {
			return err
		}
		var config ServerConfig
		if err = json.Unmarshal(before.File, &config); err != nil {
			return err
		}
		return e.checkPanelHealth(ctx, config)
	}
	return errors.New("unsupported restoration")
}
