package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type TrustedProgram struct {
	Program   string   `json:"program"`
	Arguments []string `json:"arguments"`
}
type UtilityInput struct {
	ID               string `json:"id"`
	Command          string `json:"command"`
	Name             string `json:"name,omitempty"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
	Password         string `json:"password,omitempty"`
	ReadOnly         bool   `json:"read_only,omitempty"`
	ProgramID        string `json:"program_id,omitempty"`
	Enabled          bool   `json:"enabled,omitempty"`
}
type utilityAccount struct {
	Name      string `json:"name"`
	ReadOnly  bool   `json:"read_only,omitempty"`
	ProgramID string `json:"program_id,omitempty"`
	Enabled   bool   `json:"enabled,omitempty"`
	Password  string `json:"password,omitempty"`
}
type utilityRecord struct {
	Revision int64                     `json:"revision"`
	Items    map[string]utilityAccount `json:"items"`
}

var utilityName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func utilityStateID(instance Instance) string { return "utility_" + instance.ID }
func (e *PrivilegedExecutor) utilityRecord(instance Instance) (utilityRecord, error) {
	record, _, err := e.utilityRecordState(instance)
	return record, err
}
func (e *PrivilegedExecutor) utilityRecordState(instance Instance) (utilityRecord, bool, error) {
	record := utilityRecord{Revision: 1, Items: map[string]utilityAccount{}}
	err := e.load("private", utilityStateID(instance), &record)
	if core.ErrorCode(err) == "not_found" {
		return record, false, nil
	}
	return record, err == nil, err
}
func (e *PrivilegedExecutor) validateUtility(raw json.RawMessage) error {
	var input UtilityInput
	if err := parse(raw, &input); err != nil {
		return err
	}
	if !validInstanceID(input.ID) {
		return failure("invalid_input", "Invalid utility instance ID", 400)
	}
	switch input.Command {
	case "list", "status":
	case "create", "update", "delete", "start", "stop", "restart":
		if !utilityName.MatchString(input.Name) {
			return failure("invalid_input", "Account/program name must be a simple bounded identifier", 400)
		}
		if input.ExpectedRevision < 1 {
			return failure("invalid_input", "expected_revision is required", 400)
		}
	default:
		return failure("invalid_input", "Unknown utility action", 400)
	}
	if strings.ContainsAny(input.Password, "\x00\r\n") || len(input.Password) > 1024 {
		return failure("invalid_input", "Invalid password", 400)
	}
	if input.ProgramID != "" && !utilityName.MatchString(input.ProgramID) {
		return failure("invalid_input", "Invalid trusted program profile", 400)
	}
	return nil
}
func (e *PrivilegedExecutor) configureUtility(ctx context.Context, instance *Instance) error {
	bind, _ := instance.Config["bind"].(string)
	switch instance.PluginID {
	case "pureftp":
		if os.Geteuid() != 0 {
			return failure("privilege_required", "PureFTP master needs root to isolate virtual users", 412)
		}
		// pure-ftpd gets only our own hashed PureDB, not system PAM accounts.
		if _, err := os.Stat(filepath.Join(instance.Paths.ConfigDir, "pureftpd.passwd")); os.IsNotExist(err) {
			if err = writeAtomicRoot(instance.Paths.ConfigDir, "pureftpd.passwd", nil, 0600, 0); err != nil {
				return err
			}
			if _, err = runFixed(ctx, "/usr/bin/pure-pw", []string{"mkdb", filepath.Join(instance.Paths.ConfigDir, "pureftpd.pdb"), "-f", filepath.Join(instance.Paths.ConfigDir, "pureftpd.passwd")}, 15*time.Second, nil); err != nil {
				return err
			}
		}
	case "rsyncd":
		return e.writeRsync(*instance)
	case "supervisor":
		return e.writeSupervisor(*instance)
	case "docker":
		if os.Geteuid() != 0 {
			return failure("privilege_required", "A controlled Docker daemon requires a root Agent", 412)
		}
		if os.Geteuid() == 0 {
			for _, directory := range []string{instance.Paths.RunDir, instance.Paths.DataDir} {
				if err := os.Chown(directory, 0, 0); err != nil {
					return err
				}
				if err := os.Chmod(directory, 0700); err != nil {
					return err
				}
			}
		}
		config := map[string]any{"hosts": []string{"unix://" + filepath.Join(instance.Paths.RunDir, "docker.sock")}, "data-root": instance.Paths.DataDir, "exec-root": filepath.Join(instance.Paths.RunDir, "exec"), "pidfile": filepath.Join(instance.Paths.RunDir, "server.pid"), "live-restore": true, "log-driver": "local", "log-opts": map[string]string{"max-size": "10m", "max-file": "3"}}
		raw, _ := json.Marshal(config)
		return writeAtomicRoot(instance.Paths.ConfigDir, "daemon.json", raw, 0600, 0)
	}
	_ = bind
	return nil
}
func utilityServiceArgs(instance Instance, command string) ([]string, error) {
	switch instance.PluginID {
	case "pureftp":
		switch command {
		case "check":
			return []string{"--help"}, nil
		case "start":
			bind, _ := instance.Config["bind"].(string)
			return []string{"-B", "-A", "-E", "-H", "-R", "-l", "puredb:" + filepath.Join(instance.Paths.ConfigDir, "pureftpd.pdb"), "-S", bind + "," + strconv.Itoa(setting(instance.Config, "port", 18221)), "-g", filepath.Join(instance.Paths.RunDir, "server.pid"), "-p", "30000:30009"}, nil
		}
	case "rsyncd":
		switch command {
		case "check":
			return []string{"--version"}, nil
		case "start":
			return []string{"--daemon", "--config=" + filepath.Join(instance.Paths.ConfigDir, "rsyncd.conf")}, nil
		}
	case "supervisor":
		switch command {
		case "check":
			return []string{"--version"}, nil
		case "start":
			return []string{"-c", filepath.Join(instance.Paths.ConfigDir, "supervisord.conf")}, nil
		}
	case "docker":
		switch command {
		case "check":
			return []string{"--validate", "--config-file", filepath.Join(instance.Paths.ConfigDir, "daemon.json")}, nil
		case "start":
			return []string{"--config-file", filepath.Join(instance.Paths.ConfigDir, "daemon.json")}, nil
		}
	}
	return nil, failure("unsupported_service_verb", "This utility requires a controlled restart", 412)
}
func (e *PrivilegedExecutor) utilityProbe(ctx context.Context, instance Instance) Health {
	health := Health{Status: "unhealthy", CheckedAt: time.Now().UTC(), Reason: "Utility protocol is not responding"}
	switch instance.PluginID {
	case "supervisor":
		if _, err := e.supervisorRPC(ctx, instance, "supervisor.getState", ""); err == nil {
			health.Status = "healthy"
			health.Reason = ""
		}
	case "docker":
		var response map[string]any
		if err := e.dockerRequest(ctx, instance, http.MethodGet, "/info", nil, &response); err == nil {
			health.Status = "healthy"
			health.Reason = ""
		}
	}
	return health
}
func (e *PrivilegedExecutor) writeRsync(instance Instance) error {
	record, _, err := e.utilityRecordState(instance)
	if err != nil {
		return err
	}
	return e.writeRsyncRecord(instance, record)
}
func (e *PrivilegedExecutor) writeRsyncRecord(instance Instance, record utilityRecord) error {
	bind, _ := instance.Config["bind"].(string)
	body := "uid = " + strconv.FormatUint(uint64(e.uid), 10) + "\ngid = " + strconv.FormatUint(uint64(e.gid), 10) + "\nuse chroot = no\naddress = " + bind + "\nport = " + strconv.Itoa(setting(instance.Config, "port", 18873)) + "\npid file = " + filepath.Join(instance.Paths.RunDir, "server.pid") + "\nlog file = " + filepath.Join(instance.Paths.LogDir, "rsync.log") + "\nmax connections = 16\n"
	secrets := ""
	names := []string{}
	for name := range record.Items {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		account := record.Items[name]
		directory := filepath.Join(instance.Paths.DataDir, "modules", name)
		if err := e.ensureUtilityDataDirectory(instance, filepath.Join("modules", name)); err != nil {
			return err
		}
		readOnly := "yes"
		if !account.ReadOnly {
			readOnly = "no"
		}
		body += "\n[" + name + "]\npath = " + directory + "\nread only = " + readOnly + "\nlist = yes\nauth users = " + name + "\nsecrets file = " + filepath.Join(instance.Paths.ConfigDir, "rsync.secrets") + "\nstrict modes = yes\n"
		secrets += name + ":" + account.Password + "\n"
	}
	if err := writeAtomicRoot(instance.Paths.ConfigDir, "rsync.secrets", []byte(secrets), 0440, int(e.gid)); err != nil {
		return err
	}
	return writeAtomicRoot(instance.Paths.ConfigDir, "rsyncd.conf", []byte(body), 0640, int(e.gid))
}
func supervisorQuote(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(value) + `"`
}
func (e *PrivilegedExecutor) writeSupervisor(instance Instance) error {
	record, err := e.utilityRecord(instance)
	if err != nil {
		return err
	}
	return e.writeSupervisorRecord(instance, record)
}
func (e *PrivilegedExecutor) writeSupervisorRecord(instance Instance, record utilityRecord) error {
	socket := filepath.Join(instance.Paths.RunDir, "supervisor.sock")
	body := "[unix_http_server]\nfile=" + socket + "\nchmod=0700\n[supervisord]\nlogfile=" + filepath.Join(instance.Paths.LogDir, "supervisord.log") + "\nlogfile_maxbytes=10MB\nlogfile_backups=3\npidfile=" + filepath.Join(instance.Paths.RunDir, "server.pid") + "\nchildlogdir=" + instance.Paths.LogDir + "\n[rpcinterface:supervisor]\nsupervisor.rpcinterface_factory=supervisor.rpcinterface:make_main_rpcinterface\n[supervisorctl]\nserverurl=unix://" + socket + "\n"
	names := []string{}
	for name := range record.Items {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := record.Items[name]
		profile, ok := e.config.Programs[entry.ProgramID]
		if !ok {
			return failure("program_not_trusted", "Supervisor program profile is absent from root policy", 412)
		}
		canonical, err := validateTrustedProgramPath(profile.Program)
		if err != nil {
			return failure("program_not_trusted", "Supervisor program path is no longer protected by root policy", 412)
		}
		command := supervisorQuote(canonical)
		for _, arg := range profile.Arguments {
			command += " " + supervisorQuote(arg)
		}
		directory := filepath.Join(instance.Paths.DataDir, "programs", name)
		if err = e.ensureUtilityDataDirectory(instance, filepath.Join("programs", name)); err != nil {
			return err
		}
		enabled := "false"
		if entry.Enabled {
			enabled = "true"
		}
		body += "\n[program:" + name + "]\ncommand=" + command + "\ndirectory=" + directory + "\nautostart=" + enabled + "\nautorestart=unexpected\nstartsecs=1\nstopasgroup=true\nkillasgroup=true\nstdout_logfile=" + filepath.Join(instance.Paths.LogDir, name+".log") + "\nstdout_logfile_maxbytes=10MB\nstdout_logfile_backups=3\nstderr_logfile=" + filepath.Join(instance.Paths.LogDir, name+".err") + "\nstderr_logfile_maxbytes=10MB\n"
	}
	return writeAtomicRoot(instance.Paths.ConfigDir, "supervisord.conf", []byte(body), 0640, int(e.gid))
}

func (e *PrivilegedExecutor) ensureUtilityDataDirectory(instance Instance, relative string) error {
	if !filepath.IsLocal(relative) {
		return failure("invalid_path", "Utility data path must remain inside its managed instance", 400)
	}
	root, err := os.OpenRoot(instance.Paths.DataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	if _, err = inspectUtilityDataPath(root, relative); err != nil {
		return err
	}
	if err = root.MkdirAll(relative, 0750); err != nil {
		return err
	}
	if _, err = inspectUtilityDataPath(root, relative); err != nil {
		return err
	}
	directory, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return failure("ownership_invalid", "Utility data directory must be a real directory inside its managed instance", 412)
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return failure("ownership_invalid", "Utility data path is not a directory", 412)
	}
	if os.Geteuid() == 0 {
		if err = directory.Chown(int(e.uid), int(e.gid)); err != nil {
			return err
		}
		if err = directory.Chmod(0750); err != nil {
			return err
		}
	}
	return nil
}

func (e *PrivilegedExecutor) setUtilityDataDirectory(instance Instance, relative string, uid, gid int, mode os.FileMode) error {
	if !filepath.IsLocal(relative) {
		return failure("invalid_path", "Utility data path must remain inside its managed instance", 400)
	}
	root, err := os.OpenRoot(instance.Paths.DataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	exists, err := inspectUtilityDataPath(root, relative)
	if err != nil {
		return err
	}
	if !exists {
		return failure("not_found", "Utility data directory does not exist", 404)
	}
	directory, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return failure("ownership_invalid", "Utility data directory must be a real directory inside its managed instance", 412)
	}
	defer directory.Close()
	if os.Geteuid() == 0 {
		if err = directory.Chown(uid, gid); err != nil {
			return err
		}
	}
	return directory.Chmod(mode)
}
func (e *PrivilegedExecutor) supervisorRPC(ctx context.Context, instance Instance, method, name string) (string, error) {
	var escaped bytes.Buffer
	_ = xml.EscapeText(&escaped, []byte(name))
	params := ""
	if name != "" {
		params = "<param><value><string>" + escaped.String() + "</string></value></param>"
	}
	body := "<?xml version=\"1.0\"?><methodCall><methodName>" + method + "</methodName><params>" + params + "</params></methodCall>"
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(instance.Paths.RunDir, "supervisor.sock"))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://supervisor/RPC2", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "text/xml")
	response, err := client.Do(request)
	if err != nil {
		return "", failure("supervisor_unavailable", "Protected Supervisor socket is unavailable", 502)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 256<<10))
	if err != nil {
		return "", err
	}
	if strings.Contains(string(data), "<fault>") && strings.Contains(string(data), "BAD_NAME") {
		return "", failure("supervisor_program_missing", "Supervisor program group is not loaded", 404)
	}
	if response.StatusCode != 200 || strings.Contains(string(data), "<fault>") {
		return "", failure("supervisor_operation_failed", "Supervisor rejected the typed program operation", 502)
	}
	return string(data), nil
}

func supervisorProcessState(response string) (string, error) {
	decoder := xml.NewDecoder(strings.NewReader(response))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "member" {
			continue
		}
		var member struct {
			Name  string `xml:"name"`
			Value struct {
				Text string `xml:"string"`
			} `xml:"value"`
		}
		if err := decoder.DecodeElement(&member, &start); err != nil {
			return "", err
		}
		if member.Name == "statename" {
			if member.Value.Text == "" {
				break
			}
			return member.Value.Text, nil
		}
	}
	return "", failure("supervisor_response_invalid", "Supervisor omitted the program state", 502)
}

func (e *PrivilegedExecutor) supervisorProcessState(ctx context.Context, instance Instance, name string) (string, bool, error) {
	response, err := e.supervisorRPC(ctx, instance, "supervisor.getProcessInfo", name)
	if err != nil {
		if core.ErrorCode(err) == "supervisor_program_missing" {
			return "", false, nil
		}
		return "", false, err
	}
	state, err := supervisorProcessState(response)
	if err != nil {
		return "", false, err
	}
	return state, true, nil
}

func (e *PrivilegedExecutor) supervisorProgramRunning(ctx context.Context, instance Instance, name string) (bool, error) {
	state, exists, err := e.supervisorProcessState(ctx, instance, name)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, failure("supervisor_program_missing", "Supervisor program group is not loaded", 404)
	}
	switch state {
	case "RUNNING":
		return true, nil
	case "STOPPED", "EXITED", "FATAL", "BACKOFF":
		return false, nil
	default:
		return false, failure("supervisor_process_busy", "Supervisor program is changing state; retry after it settles", 409)
	}
}

func (e *PrivilegedExecutor) applyRsyncUtilityRecord(ctx context.Context, instance Instance, record utilityRecord, running bool) error {
	if e.utilityApplyHook != nil {
		return e.utilityApplyHook(ctx, instance, record, running)
	}
	if !running {
		return nil
	}
	_, err := e.controlLocked(ctx, ServiceInput{ID: instance.ID, Command: "restart"}, instance)
	return err
}

func (e *PrivilegedExecutor) applySupervisorUtilityRecord(ctx context.Context, instance Instance, record utilityRecord, serviceRunning, processExisted, processRunning bool, name, command string) error {
	if e.utilityApplyHook != nil {
		return e.utilityApplyHook(ctx, instance, record, serviceRunning)
	}
	if !serviceRunning {
		return nil
	}
	if _, err := e.supervisorRPC(ctx, instance, "supervisor.reloadConfig", ""); err != nil {
		return err
	}
	if processExisted && processRunning && (command == "update" || command == "delete") {
		if _, err := e.supervisorRPC(ctx, instance, "supervisor.stopProcessGroup", name); err != nil {
			return err
		}
	}
	if command == "delete" || command == "update" {
		if _, err := e.supervisorRPC(ctx, instance, "supervisor.removeProcessGroup", name); err != nil {
			return err
		}
	}
	if command == "create" || command == "update" {
		if _, err := e.supervisorRPC(ctx, instance, "supervisor.addProcessGroup", name); err != nil {
			return err
		}
	}
	if processExisted && command == "update" && processRunning {
		_, err := e.supervisorRPC(ctx, instance, "supervisor.startProcessGroup", name)
		return err
	}
	return nil
}

func (e *PrivilegedExecutor) restoreSupervisorUtilityState(ctx context.Context, instance Instance, oldRecord utilityRecord, serviceRunning, processExisted, processRunning bool, name string) error {
	if e.utilityApplyHook != nil {
		return e.utilityApplyHook(ctx, instance, oldRecord, serviceRunning)
	}
	if !serviceRunning {
		return nil
	}
	if _, err := e.supervisorRPC(ctx, instance, "supervisor.reloadConfig", ""); err != nil {
		return err
	}
	state, exists, err := e.supervisorProcessState(ctx, instance, name)
	if err != nil {
		return err
	}
	if processExisted {
		if exists {
			if state == "RUNNING" {
				if _, err = e.supervisorRPC(ctx, instance, "supervisor.stopProcessGroup", name); err != nil {
					return err
				}
			}
			if _, err = e.supervisorRPC(ctx, instance, "supervisor.removeProcessGroup", name); err != nil {
				return err
			}
		}
		if _, err = e.supervisorRPC(ctx, instance, "supervisor.addProcessGroup", name); err != nil {
			return err
		}
		if processRunning {
			_, err = e.supervisorRPC(ctx, instance, "supervisor.startProcessGroup", name)
			return err
		}
		return nil
	}
	if exists {
		if state == "RUNNING" {
			if _, err = e.supervisorRPC(ctx, instance, "supervisor.stopProcessGroup", name); err != nil {
				return err
			}
		}
		_, err = e.supervisorRPC(ctx, instance, "supervisor.removeProcessGroup", name)
	}
	return err
}

func (e *PrivilegedExecutor) mutateRsyncRecord(ctx context.Context, instance Instance, oldRecord utilityRecord, oldRecordPresent bool, nextRecord utilityRecord, input UtilityInput) error {
	privateSnapshot, err := snapshotUtilityFile(filepath.Join(e.config.StateDir, "private"), utilityStateID(instance)+".json")
	if err != nil {
		return err
	}
	if privateSnapshot.Exists != oldRecordPresent {
		return failure("operation_conflict", "Utility metadata changed while the instance lock was held", 409)
	}
	running, err := e.utilityRuntimeRunning(instance)
	if err != nil {
		return err
	}
	cleanup := []string{}
	if input.Command == "create" {
		dataDirectory := filepath.Join("modules", input.Name)
		if err := requireUtilityDataTargetAbsent(instance, dataDirectory); err != nil {
			return err
		}
		cleanup = append(cleanup, dataDirectory)
	}
	return e.commitUtilityMutation(instance, privateSnapshot, nextRecord, []string{"rsync.secrets", "rsyncd.conf"},
		func() error { return e.writeRsyncRecord(instance, nextRecord) },
		func(record utilityRecord) error { return e.applyRsyncUtilityRecord(ctx, instance, record, running) },
		func() error { return e.applyRsyncUtilityRecord(ctx, instance, oldRecord, running) }, cleanup)
}

func (e *PrivilegedExecutor) mutateSupervisorRecord(ctx context.Context, instance Instance, oldRecord utilityRecord, oldRecordPresent bool, nextRecord utilityRecord, input UtilityInput) error {
	privateSnapshot, err := snapshotUtilityFile(filepath.Join(e.config.StateDir, "private"), utilityStateID(instance)+".json")
	if err != nil {
		return err
	}
	if privateSnapshot.Exists != oldRecordPresent {
		return failure("operation_conflict", "Utility metadata changed while the instance lock was held", 409)
	}
	serviceRunning, err := e.utilityRuntimeRunning(instance)
	if err != nil {
		return err
	}
	processExisted := input.Command != "create"
	processRunning := false
	if serviceRunning && processExisted {
		processRunning, err = e.supervisorProgramRunning(ctx, instance, input.Name)
		if err != nil {
			return err
		}
	}
	cleanup := []string{}
	if input.Command == "create" {
		dataDirectory := filepath.Join("programs", input.Name)
		if err := requireUtilityDataTargetAbsent(instance, dataDirectory); err != nil {
			return err
		}
		cleanup = append(cleanup, dataDirectory)
	}
	apply := func(record utilityRecord) error {
		return e.applySupervisorUtilityRecord(ctx, instance, record, serviceRunning, processExisted, processRunning, input.Name, input.Command)
	}
	return e.commitUtilityMutation(instance, privateSnapshot, nextRecord, []string{"supervisord.conf"},
		func() error { return e.writeSupervisorRecord(instance, nextRecord) }, apply,
		func() error {
			return e.restoreSupervisorUtilityState(ctx, instance, oldRecord, serviceRunning, processExisted, processRunning, input.Name)
		}, cleanup)
}

func (e *PrivilegedExecutor) mutatePureFTPRecord(ctx context.Context, instance Instance, oldRecord utilityRecord, oldRecordPresent bool, nextRecord utilityRecord, input UtilityInput) error {
	privateSnapshot, err := snapshotUtilityFile(filepath.Join(e.config.StateDir, "private"), utilityStateID(instance)+".json")
	if err != nil {
		return err
	}
	if privateSnapshot.Exists != oldRecordPresent {
		return failure("operation_conflict", "Utility metadata changed while the instance lock was held", 409)
	}
	dataDirectory := filepath.Join("users", input.Name)
	directorySnapshot := utilityDirectorySnapshot{}
	cleanup := []string{}
	switch input.Command {
	case "create":
		if err = requireUtilityDataTargetAbsent(instance, dataDirectory); err != nil {
			return err
		}
		cleanup = append(cleanup, dataDirectory)
	case "update":
		directorySnapshot, err = snapshotUtilityDataDirectory(instance, dataDirectory)
		if err != nil {
			return err
		}
		if !directorySnapshot.Exists || directorySnapshot.GID != int(e.gid) || (directorySnapshot.UID != int(e.uid) && directorySnapshot.UID != 0) {
			return failure("ownership_invalid", "PureFTP account data directory is not owned by this managed instance", 412)
		}
	}
	apply := func(record utilityRecord) error {
		if e.utilityApplyHook != nil {
			return e.utilityApplyHook(ctx, instance, record, false)
		}
		return e.pureFTPAccount(ctx, instance, input)
	}
	restore := func() error {
		if input.Command == "update" {
			return restoreUtilityDataDirectory(instance, dataDirectory, directorySnapshot)
		}
		return nil
	}
	return e.commitUtilityMutation(instance, privateSnapshot, nextRecord, []string{"pureftpd.passwd", "pureftpd.pdb"},
		func() error { return nil }, apply, restore, cleanup)
}

func (e *PrivilegedExecutor) utility(ctx context.Context, raw json.RawMessage) (any, error) {
	var input UtilityInput
	_ = json.Unmarshal(raw, &input)
	unlock, err := e.lockContext(ctx, input.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if input.Command != "list" && input.Command != "status" {
		if err := e.ensureInstallFinalized(ctx, input.ID); err != nil {
			return nil, err
		}
	}
	instance, err := e.get(ctx, input.ID, false)
	if err != nil {
		return nil, err
	}
	if instance.PluginID != "pureftp" && instance.PluginID != "rsyncd" && instance.PluginID != "supervisor" {
		return nil, failure("runtime_type_mismatch", "This action requires a managed FTP, rsync or Supervisor instance", 412)
	}
	record, recordPresent, err := e.utilityRecordState(instance)
	if err != nil {
		return nil, err
	}
	if input.Command == "status" {
		return map[string]any{"id": instance.ID, "health": e.probe(ctx, instance), "revision": record.Revision}, nil
	}
	if input.Command == "list" {
		items := []utilityAccount{}
		for _, item := range record.Items {
			item.Password = ""
			items = append(items, item)
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
		return map[string]any{"items": items, "revision": record.Revision}, nil
	}
	if record.Revision != input.ExpectedRevision {
		return nil, failure("revision_conflict", "Utility accounts/programs changed", 412)
	}
	_, exists := record.Items[input.Name]
	if input.Command == "create" && exists {
		return nil, failure("already_exists", "Account/program already exists", 409)
	}
	if input.Command != "create" && !exists {
		return nil, failure("not_found", "Account/program does not exist", 404)
	}
	if input.Command == "start" || input.Command == "stop" || input.Command == "restart" {
		if instance.PluginID != "supervisor" {
			return nil, failure("unsupported_action", "Per-program control is specific to Supervisor", 412)
		}
		method := map[string]string{"start": "supervisor.startProcess", "stop": "supervisor.stopProcess", "restart": "supervisor.stopProcess"}[input.Command]
		if _, err = e.supervisorRPC(ctx, instance, method, input.Name); err != nil {
			return nil, err
		}
		if input.Command == "restart" {
			if _, err = e.supervisorRPC(ctx, instance, "supervisor.startProcess", input.Name); err != nil {
				return nil, err
			}
		}
		return map[string]any{"name": input.Name, "command": input.Command}, nil
	}
	if len(record.Items) >= 256 && input.Command == "create" {
		return nil, failure("capacity_exceeded", "At most 256 accounts/programs are supported per instance", 429)
	}
	oldRecord := cloneUtilityRecord(record)
	nextRecord := cloneUtilityRecord(record)
	old := record.Items[input.Name]
	entry := utilityAccount{Name: input.Name, ReadOnly: input.ReadOnly, ProgramID: input.ProgramID, Enabled: input.Enabled, Password: input.Password}
	if entry.Password == "" {
		entry.Password = old.Password
	}
	if instance.PluginID == "supervisor" {
		if _, ok := e.config.Programs[entry.ProgramID]; !ok && input.Command != "delete" {
			return nil, failure("program_not_trusted", "Choose a program explicitly installed in root Agent policy", 412)
		}
	} else if input.Command != "delete" && len(entry.Password) < 12 && !(instance.PluginID == "pureftp" && input.Command == "update" && input.Password == "") {
		return nil, failure("weak_password", "FTP/rsync passwords require at least 12 characters", 400)
	}
	if input.Command == "delete" {
		delete(nextRecord.Items, input.Name)
	} else {
		nextRecord.Items[input.Name] = entry
	}
	nextRecord.Revision++
	if instance.PluginID == "rsyncd" {
		if err = e.mutateRsyncRecord(ctx, instance, oldRecord, recordPresent, nextRecord, input); err != nil {
			return nil, err
		}
		return map[string]any{"id": instance.ID, "name": input.Name, "command": input.Command, "revision": nextRecord.Revision}, nil
	}
	if instance.PluginID == "supervisor" {
		if err = e.mutateSupervisorRecord(ctx, instance, oldRecord, recordPresent, nextRecord, input); err != nil {
			return nil, err
		}
		return map[string]any{"id": instance.ID, "name": input.Name, "command": input.Command, "revision": nextRecord.Revision}, nil
	}
	entry.Password = ""
	if input.Command != "delete" {
		nextRecord.Items[input.Name] = entry
	}
	if err = e.mutatePureFTPRecord(ctx, instance, oldRecord, recordPresent, nextRecord, input); err != nil {
		return nil, err
	}
	return map[string]any{"id": instance.ID, "name": input.Name, "command": input.Command, "revision": nextRecord.Revision}, nil
}
func (e *PrivilegedExecutor) pureFTPAccount(ctx context.Context, instance Instance, input UtilityInput) error {
	passwd := filepath.Join(instance.Paths.ConfigDir, "pureftpd.passwd")
	args := []string{}
	switch input.Command {
	case "delete":
		args = []string{"userdel", input.Name, "-f", passwd}
	case "create":
		directory := filepath.Join(instance.Paths.DataDir, "users", input.Name)
		if err := e.ensureUtilityDataDirectory(instance, filepath.Join("users", input.Name)); err != nil {
			return err
		}
		args = []string{"useradd", input.Name, "-f", passwd, "-u", strconv.FormatUint(uint64(e.uid), 10), "-g", strconv.FormatUint(uint64(e.gid), 10), "-d", directory}
	case "update":
		if input.Password == "" {
			mode := os.FileMode(0750)
			uid := int(e.uid)
			if input.ReadOnly {
				mode = 0550
				uid = 0
			}
			return e.setUtilityDataDirectory(instance, filepath.Join("users", input.Name), uid, int(e.gid), mode)
		}
		args = []string{"passwd", input.Name, "-f", passwd}
	default:
		return failure("unsupported_action", "Unsupported FTP account operation", 400)
	}
	operation := agent.Operation{Name: "pureftp_account", Program: "/usr/bin/pure-pw", Timeout: 15 * time.Second, BuildArgs: func(json.RawMessage) ([]string, error) { return args, nil }}
	runner, err := agent.NewRunner(agent.Options{Operations: []agent.Operation{operation}, MaxConcurrent: 1, MaxOutputBytes: 4096})
	if err != nil {
		return err
	}
	streams := agent.IO{Stdin: io.NopCloser(strings.NewReader(input.Password + "\n" + input.Password + "\n"))}
	result, err := runner.Stream(ctx, agent.Request{Operation: operation.Name, Input: json.RawMessage(`{}`)}, streams)
	if err != nil || result.ExitCode != 0 {
		return failure("ftp_account_failed", "PureFTP rejected the virtual user change", 502)
	}
	_, err = runFixed(ctx, "/usr/bin/pure-pw", []string{"mkdb", filepath.Join(instance.Paths.ConfigDir, "pureftpd.pdb"), "-f", passwd}, 15*time.Second, nil)
	if err == nil && input.Command != "delete" {
		mode := os.FileMode(0750)
		uid := int(e.uid)
		if input.ReadOnly {
			mode = 0550
			uid = 0
		}
		err = e.setUtilityDataDirectory(instance, filepath.Join("users", input.Name), uid, int(e.gid), mode)
	}
	return err
}
func (m *Manager) utilityBindings() []core.Binding {
	bindings := []core.Binding{}
	for _, plugin := range []string{"ftp", "rsync", "supervisor"} {
		for _, command := range []string{"list", "status", "create", "update", "delete", "start", "stop", "restart"} {
			if plugin != "supervisor" && (command == "start" || command == "stop" || command == "restart") {
				continue
			}
			read := command == "list" || command == "status"
			properties := map[string]any{"id": idSchema()}
			required := []string{"id"}
			if !read {
				properties["name"] = map[string]any{"type": "string", "pattern": utilityName.String()}
				properties["expected_revision"] = revisionSchema()
				required = append(required, "name", "expected_revision")
			}
			if command == "create" || command == "update" {
				if plugin == "supervisor" {
					properties["program_id"] = idSchema()
					properties["enabled"] = map[string]any{"type": "boolean"}
				} else {
					properties["password"] = map[string]any{"type": "string", "minLength": 12, "maxLength": 1024, "format": "password"}
					properties["read_only"] = map[string]any{"type": "boolean"}
				}
			}
			verb := command
			family := plugin
			scope := plugin + ":manage"
			if read {
				scope = plugin + ":read"
			}
			bindings = append(bindings, m.binding(plugin+"_"+command, "Typed managed utility action; secrets never enter argv or task records", scope, read, obj(properties, required...), idResource, func(ctx context.Context, _ core.Actor, raw json.RawMessage) (core.ActionResult, error) {
				var input UtilityInput
				if err := parse(raw, &input); err != nil {
					return core.ActionResult{}, err
				}
				var instance Instance
				idRaw, _ := json.Marshal(IDInput{ID: input.ID})
				if err := m.agentJSON(ctx, "plugin_get", idRaw, &instance); err != nil {
					return core.ActionResult{}, err
				}
				expected := map[string]string{"ftp": "pureftp", "rsync": "rsyncd", "supervisor": "supervisor"}[family]
				if instance.PluginID != expected {
					return core.ActionResult{}, failure("runtime_type_mismatch", "Utility family does not match this managed instance", 412)
				}
				input.Command = verb
				forward, _ := json.Marshal(input)
				var result any
				if err := m.agentJSON(ctx, "utility_action", forward, &result); err != nil {
					return core.ActionResult{}, err
				}
				return core.ActionResult{Data: result, Resources: []string{InstanceResource(input.ID)}}, nil
			}))
		}
	}
	return bindings
}
