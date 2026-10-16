package plugins

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	supervisorScriptPath      = "/usr/bin/supervisord"
	supervisorInterpreterPath = "/usr/bin/python3"
)

// supervisorIdentityPaths keeps the test fixture configurable without making
// process identity policy configurable through the API or Agent request.
type supervisorIdentityPaths struct {
	procRoot          string
	unitRoot          string
	interpreter       string
	script            string
	trustedFileUID    uint32
	requireUnitCgroup bool
}

func verifySupervisorProcessIdentity(instance Instance, pid int) error {
	return verifySupervisorProcessIdentityAt(instance, pid, supervisorIdentityPaths{
		procRoot:          "/proc",
		unitRoot:          "/etc/systemd/system",
		interpreter:       supervisorInterpreterPath,
		script:            supervisorScriptPath,
		trustedFileUID:    0,
		requireUnitCgroup: true,
	})
}

func verifySupervisorProcessIdentityAt(instance Instance, pid int, paths supervisorIdentityPaths) error {
	invalid := func() error {
		return failure("ownership_invalid", "Supervisor PID does not match its protected script, unit, and worker identity", 412)
	}
	if pid < 2 || instance.PluginID != "supervisor" || instance.Status == "uninstalled" || instance.Ownership != "managed" ||
		instance.Service.Kind != "daemon" || instance.Service.Ownership != "managed" || instance.Service.PluginID != "supervisor" ||
		instance.Service.ID != instance.ID || instance.Service.InstanceID != instance.InstanceID || !ownedUnit(instance) ||
		instance.Service.Program != paths.script || instance.Service.ConfigPath != instance.Paths.ConfigDir ||
		!filepath.IsAbs(paths.script) || instance.Service.UnitSHA256 == "" ||
		!artifactHash.MatchString(instance.Service.UnitSHA256) || !filepath.IsAbs(instance.Paths.ConfigDir) || !filepath.IsAbs(instance.Paths.RunDir) ||
		filepath.Clean(instance.Paths.ConfigDir) != instance.Paths.ConfigDir || filepath.Clean(instance.Paths.RunDir) != instance.Paths.RunDir ||
		!hasPinnedSupervisorPackage(instance.Packages) || paths.procRoot == "" || paths.unitRoot == "" || paths.interpreter == "" {
		return invalid()
	}

	// The system package receipt is the trust anchor for this fixed script path;
	// the script itself must still be a root-protected regular file with the
	// exact interpreter declaration used by the Debian package.
	script, scriptInfo, err := readProtectedRegular(paths.script, 4<<20, paths.trustedFileUID, true)
	if err != nil || filepath.Clean(paths.script) != paths.script || scriptInfo.Mode()&os.ModeSymlink != 0 ||
		!bytes.HasPrefix(script, []byte("#!/usr/bin/python3\n")) {
		return invalid()
	}
	scriptResolved, err := filepath.EvalSymlinks(paths.script)
	if err != nil || scriptResolved != paths.script {
		return invalid()
	}

	interpreterResolved, err := filepath.EvalSymlinks(paths.interpreter)
	if err != nil || filepath.Dir(interpreterResolved) != "/usr/bin" || !strings.HasPrefix(filepath.Base(interpreterResolved), "python3") {
		return invalid()
	}
	_, interpreterInfo, err := readProtectedRegular(interpreterResolved, 64<<20, 0, true)
	if err != nil || !interpreterInfo.Mode().IsRegular() {
		return invalid()
	}

	unitPath := filepath.Join(paths.unitRoot, instance.Service.Unit)
	unitBody, _, err := readProtectedRegular(unitPath, 64<<10, paths.trustedFileUID, false)
	if err != nil {
		return invalid()
	}
	unitDigest := sha256.Sum256(unitBody)
	if hex.EncodeToString(unitDigest[:]) != instance.Service.UnitSHA256 {
		return invalid()
	}
	workerUID, workerGID, err := supervisorUnitWorkerIdentity(unitBody, instance, paths.script)
	if err != nil || workerUID == 0 {
		return invalid()
	}

	procDir := filepath.Join(paths.procRoot, strconv.Itoa(pid))
	startBefore, err := processStartTime(filepath.Join(procDir, "stat"))
	if err != nil {
		if os.IsNotExist(err) {
			return failure("runtime_not_running", "Managed process is not running", 412)
		}
		return invalid()
	}
	exe, err := os.Readlink(filepath.Join(procDir, "exe"))
	if err != nil || exe != interpreterResolved {
		return invalid()
	}
	cmdline, err := os.ReadFile(filepath.Join(procDir, "cmdline"))
	if err != nil || len(cmdline) == 0 || len(cmdline) > 4096 {
		return invalid()
	}
	expectedConfig := filepath.Join(instance.Paths.ConfigDir, "supervisord.conf")
	if !exactSupervisorArgv(cmdline, paths.interpreter, paths.script, expectedConfig) {
		return invalid()
	}
	status, err := os.ReadFile(filepath.Join(procDir, "status"))
	if err != nil || !supervisorProcessCredentialsMatch(status, workerUID, workerGID) {
		return invalid()
	}
	if paths.requireUnitCgroup {
		cgroup, readErr := os.ReadFile(filepath.Join(procDir, "cgroup"))
		if readErr != nil || !supervisorCgroupMatchesUnit(cgroup, instance.Service.Unit) {
			return invalid()
		}
	}
	startAfter, err := processStartTime(filepath.Join(procDir, "stat"))
	if err != nil || startBefore != startAfter {
		return invalid()
	}
	pidFile, _, err := readProtectedRegular(filepath.Join(instance.Paths.RunDir, "server.pid"), 32, workerUID, false)
	if err != nil || strings.TrimSpace(string(pidFile)) != strconv.Itoa(pid) {
		return invalid()
	}
	return nil
}

func hasPinnedSupervisorPackage(pins []PackagePin) bool {
	for _, pin := range pins {
		if pin.Name == "supervisor" && pin.Version != "" && artifactHash.MatchString(pin.SHA256) &&
			(pin.Format == "" || pin.Format == "rpm") && pin.Architecture != "" && pin.Repository != "" {
			return true
		}
	}
	return false
}

func readProtectedRegular(path string, maxBytes int64, ownerUID uint32, executable bool) ([]byte, os.FileInfo, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxBytes || info.Mode().Perm()&0022 != 0 ||
		info.Mode()&os.ModeSetuid != 0 || info.Mode()&os.ModeSetgid != 0 {
		return nil, nil, fmt.Errorf("unsafe protected file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != ownerUID || executable && info.Mode().Perm()&0111 == 0 {
		return nil, nil, fmt.Errorf("protected file owner or mode mismatch")
	}
	body, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(body)) > maxBytes {
		return nil, nil, fmt.Errorf("protected file read failed")
	}
	return body, info, nil
}

func supervisorUnitWorkerIdentity(unit []byte, instance Instance, script string) (uint32, uint32, error) {
	section := ""
	userName, groupName := "", ""
	userSeen, groupSeen, commandSeen := false, false, false
	expectedCommand := "ExecStart=" + unitQuote(script) + " " + unitQuote("-c") + " " +
		unitQuote(filepath.Join(instance.Paths.ConfigDir, "supervisord.conf")) + " " + unitQuote("-n")
	for _, line := range strings.Split(string(unit), "\n") {
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		if section != "[Service]" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "User":
			if userSeen || value == "" || strings.ContainsAny(value, " \t\r\n") {
				return 0, 0, fmt.Errorf("invalid unit user")
			}
			userSeen, userName = true, value
		case "Group":
			if groupSeen || value == "" || strings.ContainsAny(value, " \t\r\n") {
				return 0, 0, fmt.Errorf("invalid unit group")
			}
			groupSeen, groupName = true, value
		case "ExecStart":
			if commandSeen || line != expectedCommand {
				return 0, 0, fmt.Errorf("unexpected unit command")
			}
			commandSeen = true
		}
	}
	if !userSeen || !groupSeen || !commandSeen {
		return 0, 0, fmt.Errorf("incomplete unit identity")
	}
	worker, err := user.Lookup(userName)
	if err != nil {
		return 0, 0, err
	}
	group, err := user.LookupGroup(groupName)
	if err != nil {
		return 0, 0, err
	}
	uid, uidErr := strconv.ParseUint(worker.Uid, 10, 32)
	gid, gidErr := strconv.ParseUint(group.Gid, 10, 32)
	if uidErr != nil || gidErr != nil {
		return 0, 0, fmt.Errorf("invalid worker identity")
	}
	return uint32(uid), uint32(gid), nil
}

func exactSupervisorArgv(cmdline []byte, interpreter, script, config string) bool {
	if len(cmdline) == 0 || cmdline[len(cmdline)-1] != 0 {
		return false
	}
	parts := bytes.Split(cmdline, []byte{0})
	parts = parts[:len(parts)-1]
	expected := []string{interpreter, script, "-c", config, "-n"}
	if len(parts) != len(expected) {
		return false
	}
	for i := range expected {
		if string(parts[i]) != expected[i] {
			return false
		}
	}
	return true
}

func supervisorProcessCredentialsMatch(status []byte, expectedUID, expectedGID uint32) bool {
	uidFound, gidFound := false, false
	for _, line := range strings.Split(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 5 {
			continue
		}
		switch fields[0] {
		case "Uid:":
			for _, value := range fields[1:] {
				uid, err := strconv.ParseUint(value, 10, 32)
				if err != nil || uint32(uid) != expectedUID {
					return false
				}
			}
			uidFound = true
		case "Gid:":
			for _, value := range fields[1:] {
				gid, err := strconv.ParseUint(value, 10, 32)
				if err != nil || uint32(gid) != expectedGID {
					return false
				}
			}
			gidFound = true
		}
	}
	return uidFound && gidFound
}

func supervisorCgroupMatchesUnit(cgroup []byte, unit string) bool {
	for _, line := range strings.Split(string(cgroup), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			continue
		}
		for _, component := range strings.Split(fields[2], "/") {
			if component == unit {
				return true
			}
		}
	}
	return false
}

func processStartTime(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	closeParen := bytes.LastIndexByte(data, ')')
	if closeParen < 0 || closeParen+1 >= len(data) {
		return "", fmt.Errorf("invalid proc stat")
	}
	fields := strings.Fields(string(data[closeParen+1:]))
	// /proc/PID/stat starts this tail at field 3; starttime is field 22.
	if len(fields) <= 19 {
		return "", fmt.Errorf("incomplete proc stat")
	}
	return fields[19], nil
}
