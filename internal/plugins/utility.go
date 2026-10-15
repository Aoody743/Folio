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
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type TrustedProgram struct {
	Program   string   `json:"program"`
	Arguments []string `json:"arguments"`
}
type UtilityInput struct {
	ID                         string `json:"id"`
	Command                    string `json:"command"`
	Name                       string `json:"name,omitempty"`
	ExpectedRevision           int64  `json:"expected_revision,omitempty"`
	Password                   string `json:"password,omitempty"`
	ReadOnly                   bool   `json:"read_only,omitempty"`
	ProgramID                  string `json:"program_id,omitempty"`
	Enabled                    bool   `json:"enabled,omitempty"`
	ConfirmClearLegacyDenyRule bool   `json:"confirm_clear_legacy_deny_rule,omitempty"`
}
type utilityAccount struct {
	Name       string `json:"name"`
	ReadOnly   bool   `json:"read_only,omitempty"`
	ProgramID  string `json:"program_id,omitempty"`
	Enabled    bool   `json:"enabled,omitempty"`
	FTPEnabled *bool  `json:"ftp_enabled,omitempty"`
	Password   string `json:"password,omitempty"`
}
type utilityRecord struct {
	Revision int64                     `json:"revision"`
	Items    map[string]utilityAccount `json:"items"`
}

var utilityName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func utilityStateID(instance Instance) string { return "utility_" + instance.ID }
func (e *PrivilegedExecutor) utilityRecord(instance Instance) (utilityRecord, error) {
	record := utilityRecord{Revision: 1, Items: map[string]utilityAccount{}}
	err := e.load("private", utilityStateID(instance), &record)
	if core.ErrorCode(err) == "not_found" {
		return record, nil
	}
	return record, err
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
	case "inspect":
		if !utilityName.MatchString(input.Name) {
			return failure("invalid_input", "FTP account name must be a simple bounded identifier", 400)
		}
	case "create", "update", "delete", "start", "stop", "restart", "pause", "resume", "legacy_recover":
		if !utilityName.MatchString(input.Name) {
			return failure("invalid_input", "Account/program name must be a simple bounded identifier", 400)
		}
		if input.ExpectedRevision < 1 {
			return failure("invalid_input", "expected_revision is required", 400)
		}
		if input.Command == "legacy_recover" && !input.ConfirmClearLegacyDenyRule {
			return failure("invalid_input", "Explicit confirmation is required to clear the known legacy FTP deny rule", 400)
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
	record, err := e.utilityRecord(instance)
	if err != nil {
		return err
	}
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
		if err = os.MkdirAll(directory, 0750); err != nil {
			return err
		}
		if os.Geteuid() == 0 {
			if err = os.Chown(directory, int(e.uid), int(e.gid)); err != nil {
				return err
			}
		}
		readOnly := "yes"
		if !account.ReadOnly {
			readOnly = "no"
		}
		body += "\n[" + name + "]\npath = " + directory + "\nread only = " + readOnly + "\nlist = yes\nauth users = " + name + "\nsecrets file = " + filepath.Join(instance.Paths.ConfigDir, "rsync.secrets") + "\nstrict modes = yes\n"
		secrets += name + ":" + account.Password + "\n"
	}
	if err = writeAtomicRoot(instance.Paths.ConfigDir, "rsync.secrets", []byte(secrets), 0440, int(e.gid)); err != nil {
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
		directory, pathErr := e.prepareSupervisorProgramDirectory(instance, name)
		if pathErr != nil {
			return pathErr
		}
		enabled := "false"
		if entry.Enabled {
			enabled = "true"
		}
		body += "\n[program:" + name + "]\ncommand=" + command + "\ndirectory=" + directory + "\nautostart=" + enabled + "\nautorestart=unexpected\nstartsecs=1\nstopasgroup=true\nkillasgroup=true\nstdout_logfile=" + filepath.Join(instance.Paths.LogDir, name+".log") + "\nstdout_logfile_maxbytes=10MB\nstdout_logfile_backups=3\nstderr_logfile=" + filepath.Join(instance.Paths.LogDir, name+".err") + "\nstderr_logfile_maxbytes=10MB\n"
	}
	return writeAtomicRoot(instance.Paths.ConfigDir, "supervisord.conf", []byte(body), 0640, int(e.gid))
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
	if response.StatusCode != 200 || strings.Contains(string(data), "<fault>") {
		return "", failure("supervisor_operation_failed", "Supervisor rejected the typed program operation", 502)
	}
	return string(data), nil
}
func (e *PrivilegedExecutor) utility(ctx context.Context, raw json.RawMessage) (any, error) {
	var input UtilityInput
	_ = json.Unmarshal(raw, &input)
	unlock, err := e.lockContext(ctx, input.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if input.Command != "list" && input.Command != "status" && input.Command != "inspect" {
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
	if (input.Command == "pause" || input.Command == "resume" || input.Command == "legacy_recover") && instance.PluginID != "pureftp" {
		return nil, failure("unsupported_action", "This per-account action is specific to managed PureFTP", 412)
	}
	record, err := e.utilityRecord(instance)
	if err != nil {
		return nil, err
	}
	if input.Command == "inspect" {
		if instance.PluginID != "pureftp" {
			return nil, failure("unsupported_action", "Per-account inspection is specific to managed PureFTP", 412)
		}
		return e.inspectFTPAccountState(instance, record, input.Name)
	}
	if input.Command == "legacy_recover" {
		if instance.PluginID != "pureftp" {
			return nil, failure("unsupported_action", "Legacy FTP recovery is specific to managed PureFTP", 412)
		}
		return e.recoverLegacyPureFTPAccount(ctx, instance, input, record, e.rebuildPureFTPPasswordDB)
	}
	if input.Command == "status" {
		result := map[string]any{"id": instance.ID, "health": e.probe(ctx, instance), "revision": record.Revision}
		if instance.PluginID == "pureftp" {
			items, viewErr := e.ftpAccountStateViews(instance, record)
			if viewErr != nil {
				return nil, viewErr
			}
			result["accounts"] = items
		}
		return result, nil
	}
	if input.Command == "list" {
		if instance.PluginID == "pureftp" {
			items, viewErr := e.ftpAccountStateViews(instance, record)
			if viewErr != nil {
				return nil, viewErr
			}
			return map[string]any{"items": items, "revision": record.Revision}, nil
		}
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
	if instance.PluginID == "pureftp" {
		legacyJournal, journalErr := e.loadFTPLegacyRecoveryJournal(instance, input.Name)
		if journalErr != nil {
			return nil, journalErr
		}
		if legacyJournal != nil {
			return nil, failure("ftp_account_state_pending", "Complete this FTP account's legacy recovery before changing it", 409)
		}
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
	old := record.Items[input.Name]
	if instance.PluginID == "pureftp" && input.ReadOnly && (input.Command == "create" || input.Command == "update") {
		return nil, failure("unsupported_action", "PureFTP per-account read-only access is unavailable until existing-file writes can be denied", 412)
	}
	var ftpJournal *pureFTPAccountStateJournal
	if instance.PluginID == "pureftp" && exists {
		if !ftpAccountEnabled(old) && input.Command == "update" {
			return nil, failure("ftp_account_paused", "Resume this FTP account before changing its settings", 409)
		}
		ftpJournal, err = e.loadPureFTPStateJournal(instance, input.Name)
		if err != nil {
			return nil, err
		}
		if input.Command != "pause" && input.Command != "resume" && ftpJournal != nil {
			return nil, failure("ftp_account_state_pending", "Complete this FTP account's pause or resume before changing it", 409)
		}
		if !ftpAccountEnabled(old) && ftpJournal == nil {
			return nil, failure("ftp_account_state_unrecoverable", "This paused FTP account has no protected verifier journal", 409)
		}
	}
	if instance.PluginID == "pureftp" && !ftpAccountEnabled(old) && input.Command == "delete" {
		return nil, failure("ftp_account_paused", "Resume this FTP account before deleting it", 409)
	}
	entry := utilityAccount{Name: input.Name, ReadOnly: input.ReadOnly, ProgramID: input.ProgramID, Enabled: input.Enabled, Password: input.Password}
	if instance.PluginID == "pureftp" {
		switch input.Command {
		case "create":
			entry.FTPEnabled = ftpAccountEnabledValue(true)
		case "update":
			entry.FTPEnabled = old.FTPEnabled
		case "pause", "resume":
			entry = old
		}
	}
	if entry.Password == "" {
		entry.Password = old.Password
	}
	if instance.PluginID == "supervisor" {
		if _, ok := e.config.Programs[entry.ProgramID]; !ok && input.Command != "delete" {
			return nil, failure("program_not_trusted", "Choose a program explicitly installed in root Agent policy", 412)
		}
	} else if input.Command != "delete" && input.Command != "pause" && input.Command != "resume" && len(entry.Password) < 12 && !(instance.PluginID == "pureftp" && input.Command == "update" && input.Password == "") {
		return nil, failure("weak_password", "FTP/rsync passwords require at least 12 characters", 400)
	}
	if instance.PluginID == "pureftp" {
		switch input.Command {
		case "pause", "resume":
			enabled := input.Command == "resume"
			if ftpAccountEnabled(old) != enabled || ftpJournal != nil {
				if err = e.setPureFTPAccountEnabled(ctx, instance, input.Name, enabled, true); err != nil {
					return nil, err
				}
			}
			entry.FTPEnabled = ftpAccountEnabledValue(enabled)
		default:
			if err = e.pureFTPAccount(ctx, instance, input); err != nil {
				return nil, err
			}
			if input.Command == "update" && !ftpAccountEnabled(entry) {
				if err = e.setPureFTPAccountEnabled(ctx, instance, input.Name, false, true); err != nil {
					return nil, err
				}
			}
		}
		entry.Password = ""
	}
	if input.Command == "delete" {
		delete(record.Items, input.Name)
	} else {
		record.Items[input.Name] = entry
	}
	record.Revision++
	if err = e.save("private", utilityStateID(instance), record); err != nil {
		return nil, err
	}
	if instance.PluginID == "pureftp" && input.Command == "resume" {
		if err = e.removePureFTPStateJournal(instance, input.Name); err != nil {
			return nil, err
		}
	}
	if instance.PluginID == "rsyncd" {
		if err = e.writeRsync(instance); err != nil {
			return nil, err
		}
		if _, err = e.controlLocked(ctx, ServiceInput{ID: instance.ID, Command: "restart"}, instance); err != nil {
			return nil, err
		}
	}
	if instance.PluginID == "supervisor" {
		if err = e.writeSupervisor(instance); err != nil {
			return nil, err
		}
		if _, err = e.supervisorRPC(ctx, instance, "supervisor.reloadConfig", ""); err != nil {
			return nil, err
		}
		if input.Command == "create" {
			if _, err = e.supervisorRPC(ctx, instance, "supervisor.addProcessGroup", input.Name); err != nil {
				return nil, err
			}
		}
		if input.Command == "delete" {
			_, _ = e.supervisorRPC(ctx, instance, "supervisor.stopProcessGroup", input.Name)
			if _, err = e.supervisorRPC(ctx, instance, "supervisor.removeProcessGroup", input.Name); err != nil {
				return nil, err
			}
		}
		if input.Command == "update" {
			_, _ = e.supervisorRPC(ctx, instance, "supervisor.stopProcessGroup", input.Name)
			if _, err = e.supervisorRPC(ctx, instance, "supervisor.removeProcessGroup", input.Name); err != nil {
				return nil, err
			}
			if _, err = e.supervisorRPC(ctx, instance, "supervisor.addProcessGroup", input.Name); err != nil {
				return nil, err
			}
		}
	}
	result := map[string]any{"id": instance.ID, "name": input.Name, "command": input.Command, "revision": record.Revision}
	if instance.PluginID == "pureftp" {
		result["enabled"] = ftpAccountEnabled(entry)
	}
	return result, nil
}
func (e *PrivilegedExecutor) pureFTPAccount(ctx context.Context, instance Instance, input UtilityInput) error {
	if input.ReadOnly && (input.Command == "create" || input.Command == "update") {
		return failure("unsupported_action", "PureFTP per-account read-only access is unavailable until existing-file writes can be denied", 412)
	}
	passwd := filepath.Join(instance.Paths.ConfigDir, "pureftpd.passwd")
	args := []string{}
	switch input.Command {
	case "delete":
		args = []string{"userdel", input.Name, "-f", passwd}
	case "create":
		directory := filepath.Join(instance.Paths.DataDir, "users", input.Name)
		if err := os.MkdirAll(directory, 0750); err != nil {
			return err
		}
		if err := os.Chown(directory, int(e.uid), int(e.gid)); err != nil {
			return err
		}
		args = []string{"useradd", input.Name, "-f", passwd, "-u", strconv.FormatUint(uint64(e.uid), 10), "-g", strconv.FormatUint(uint64(e.gid), 10), "-d", directory}
	case "update":
		if input.Password == "" {
			directory := filepath.Join(instance.Paths.DataDir, "users", input.Name)
			if err := os.Chown(directory, int(e.uid), int(e.gid)); err != nil {
				return err
			}
			return os.Chmod(directory, 0750)
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
		directory := filepath.Join(instance.Paths.DataDir, "users", input.Name)
		if err = os.Chown(directory, int(e.uid), int(e.gid)); err == nil {
			err = os.Chmod(directory, 0750)
		}
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
					if plugin == "ftp" {
						properties["read_only"] = map[string]any{"type": "boolean", "const": false, "default": false, "description": "Per-account read-only enforcement is unavailable; only read/write accounts are currently supported"}
					}
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
