package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/Aoody743/PowerLinux/internal/core"
)

const supervisorLogReadLimit = 64 << 10
const supervisorLogClearVerifyLimit = 16 << 20

type supervisorLogFingerprint struct {
	Identity SupervisorLogIdentity
	SHA256   string
}

// SupervisorLogIdentity is a safe, path-free snapshot of one managed log.
// Filesystem numbers are decimal strings because inode/device IDs can exceed
// JavaScript's exact integer range. Size is informational: a live Supervisor
// worker may append between preview and clear, while replacement is rejected
// by the device/inode comparison.
type SupervisorLogIdentity struct {
	Exists    bool   `json:"exists"`
	Size      string `json:"size"`
	ModTimeNS string `json:"mod_time_ns"`
	Device    string `json:"device"`
	Inode     string `json:"inode"`
}

type SupervisorLogInput struct {
	ID               string                 `json:"id"`
	Command          string                 `json:"command"`
	Name             string                 `json:"name"`
	Stream           string                 `json:"stream,omitempty"`
	MaxBytes         int                    `json:"max_bytes,omitempty"`
	ExpectedRevision int64                  `json:"expected_revision,omitempty"`
	ExpectedStdout   *SupervisorLogIdentity `json:"expected_stdout,omitempty"`
	ExpectedStderr   *SupervisorLogIdentity `json:"expected_stderr,omitempty"`
}

var absentSupervisorLog = SupervisorLogIdentity{Exists: false, Size: "0", ModTimeNS: "0", Device: "0", Inode: "0"}

func validSupervisorLogDecimal(value string, maxDigits int) bool {
	if len(value) == 0 || len(value) > maxDigits || (len(value) > 1 && value[0] == '0') {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func validSupervisorLogSignedDecimal(value string, maxDigits int) bool {
	if value == "0" {
		return true
	}
	if strings.HasPrefix(value, "-") {
		unsigned := strings.TrimPrefix(value, "-")
		return unsigned != "0" && validSupervisorLogDecimal(unsigned, maxDigits)
	}
	return validSupervisorLogDecimal(value, maxDigits)
}

func validateSupervisorLogIdentity(identity *SupervisorLogIdentity) bool {
	if identity == nil || !validSupervisorLogDecimal(identity.Size, 19) || !validSupervisorLogSignedDecimal(identity.ModTimeNS, 19) || !validSupervisorLogDecimal(identity.Device, 20) || !validSupervisorLogDecimal(identity.Inode, 20) {
		return false
	}
	size, sizeErr := strconv.ParseInt(identity.Size, 10, 64)
	modTime, modTimeErr := strconv.ParseInt(identity.ModTimeNS, 10, 64)
	device, deviceErr := strconv.ParseUint(identity.Device, 10, 64)
	inode, inodeErr := strconv.ParseUint(identity.Inode, 10, 64)
	if sizeErr != nil || modTimeErr != nil || deviceErr != nil || inodeErr != nil {
		return false
	}
	if !identity.Exists {
		return size == 0 && modTime == 0 && device == 0 && inode == 0
	}
	return inode != 0
}

func (e *PrivilegedExecutor) validateSupervisorLog(raw json.RawMessage) error {
	var input SupervisorLogInput
	if err := parse(raw, &input); err != nil {
		return err
	}
	if !validInstanceID(input.ID) || !utilityName.MatchString(input.Name) {
		return failure("invalid_input", "A valid managed instance and Supervisor job name are required", 400)
	}
	switch input.Command {
	case "read":
		if input.Stream != "stdout" && input.Stream != "stderr" {
			return failure("invalid_input", "Choose the stdout or stderr stream", 400)
		}
		if input.MaxBytes < 0 || input.MaxBytes > supervisorLogReadLimit {
			return failure("invalid_input", "Supervisor log reads are limited to 64 KiB", 400)
		}
		if input.ExpectedRevision != 0 || input.ExpectedStdout != nil || input.ExpectedStderr != nil {
			return failure("invalid_input", "Log reads do not accept clear confirmation fields", 400)
		}
	case "preview_clear":
		if input.Stream != "" || input.MaxBytes != 0 || input.ExpectedRevision != 0 || input.ExpectedStdout != nil || input.ExpectedStderr != nil {
			return failure("invalid_input", "Log clear preview accepts only an instance and registered job", 400)
		}
	case "clear":
		if input.Stream != "" || input.MaxBytes != 0 || input.ExpectedRevision < 1 || !validateSupervisorLogIdentity(input.ExpectedStdout) || !validateSupervisorLogIdentity(input.ExpectedStderr) {
			return failure("invalid_input", "Clearing logs requires the job revision and both stream identities from a preview", 400)
		}
	default:
		return failure("invalid_input", "Unknown Supervisor log action", 400)
	}
	return nil
}

func supervisorLogIdentityMatches(current, expected SupervisorLogIdentity) bool {
	if current.Exists != expected.Exists {
		return false
	}
	if !current.Exists {
		return true
	}
	return current.Device == expected.Device && current.Inode == expected.Inode
}

func supervisorLogFileName(name, stream string) (string, error) {
	if !utilityName.MatchString(name) {
		return "", failure("invalid_input", "Invalid registered Supervisor job", 400)
	}
	switch stream {
	case "stdout":
		return name + ".log", nil
	case "stderr":
		return name + ".err", nil
	default:
		return "", failure("invalid_input", "Choose the stdout or stderr stream", 400)
	}
}

func (e *PrivilegedExecutor) openSupervisorLog(instance Instance, name, stream string) (*os.File, SupervisorLogIdentity, error) {
	fileName, err := supervisorLogFileName(name, stream)
	if err != nil {
		return nil, absentSupervisorLog, err
	}
	if err = realDirectory(instance.Paths.Root); err != nil {
		return nil, absentSupervisorLog, failure("log_ownership_invalid", "Managed Supervisor log directory is unavailable or unsafe", 412)
	}
	if err = realDirectory(instance.Paths.LogDir); err != nil {
		return nil, absentSupervisorLog, failure("log_ownership_invalid", "Managed Supervisor log directory is unavailable or unsafe", 412)
	}
	directory, err := os.OpenFile(instance.Paths.LogDir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, absentSupervisorLog, failure("log_ownership_invalid", "Managed Supervisor log directory is unavailable or unsafe", 412)
	}
	directoryInfo, statErr := directory.Stat()
	closeErr := directory.Close()
	if statErr != nil || closeErr != nil {
		return nil, absentSupervisorLog, failure("log_ownership_invalid", "Managed Supervisor log directory is unavailable or unsafe", 412)
	}
	directoryStat, ok := directoryInfo.Sys().(*syscall.Stat_t)
	if !ok || !directoryInfo.IsDir() || directoryInfo.Mode().Perm()&0022 != 0 || (directoryStat.Uid != e.uid && directoryStat.Uid != uint32(os.Geteuid())) {
		return nil, absentSupervisorLog, failure("log_ownership_invalid", "Managed Supervisor log directory ownership or permissions changed", 412)
	}
	root, err := os.OpenRoot(instance.Paths.LogDir)
	if err != nil {
		return nil, absentSupervisorLog, failure("log_ownership_invalid", "Managed Supervisor log directory is unavailable or unsafe", 412)
	}
	defer root.Close()
	entryInfo, err := root.Lstat(fileName)
	if errors.Is(err, os.ErrNotExist) {
		return nil, absentSupervisorLog, nil
	}
	if err != nil {
		return nil, absentSupervisorLog, failure("log_file_unsafe", "Managed Supervisor log file could not be inspected safely", 412)
	}
	entryStat, ok := entryInfo.Sys().(*syscall.Stat_t)
	if !ok || !entryInfo.Mode().IsRegular() || entryInfo.Mode().Perm()&0022 != 0 || entryStat.Nlink != 1 || (entryStat.Uid != e.uid && entryStat.Uid != uint32(os.Geteuid())) {
		return nil, absentSupervisorLog, failure("log_file_unsafe", "Supervisor job log must be an unshared regular file owned by the Agent", 412)
	}
	file, err := root.OpenFile(fileName, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, absentSupervisorLog, failure("log_file_unsafe", "Managed Supervisor log file could not be opened safely", 412)
	}
	identity, err := e.supervisorLogIdentityForFile(file)
	if err != nil {
		file.Close()
		return nil, absentSupervisorLog, err
	}
	if identity.Device != strconv.FormatUint(uint64(entryStat.Dev), 10) || identity.Inode != strconv.FormatUint(entryStat.Ino, 10) {
		file.Close()
		return nil, absentSupervisorLog, failure("log_changed", "Supervisor replaced the log while it was being inspected; retry the action", 409)
	}
	return file, identity, nil
}

func (e *PrivilegedExecutor) supervisorLogIdentityForFile(file *os.File) (SupervisorLogIdentity, error) {
	info, err := file.Stat()
	if err != nil {
		return absentSupervisorLog, failure("log_file_unsafe", "Managed Supervisor log file could not be inspected safely", 412)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Size() < 0 || info.Mode().Perm()&0022 != 0 || stat.Nlink != 1 || (stat.Uid != e.uid && stat.Uid != uint32(os.Geteuid())) {
		return absentSupervisorLog, failure("log_file_unsafe", "Supervisor job log must be an unshared regular file owned by the Agent", 412)
	}
	return SupervisorLogIdentity{Exists: true, Size: strconv.FormatInt(info.Size(), 10), ModTimeNS: strconv.FormatInt(info.ModTime().UnixNano(), 10), Device: strconv.FormatUint(uint64(stat.Dev), 10), Inode: strconv.FormatUint(stat.Ino, 10)}, nil
}

func (e *PrivilegedExecutor) supervisorLogSnapshot(instance Instance, name, stream string) (SupervisorLogIdentity, error) {
	file, identity, err := e.openSupervisorLog(instance, name, stream)
	if file != nil {
		_ = file.Close()
	}
	return identity, err
}

func (e *PrivilegedExecutor) supervisorLogClearFingerprint(instance Instance, name, stream string) (supervisorLogFingerprint, error) {
	file, identity, err := e.openSupervisorLog(instance, name, stream)
	if err != nil {
		return supervisorLogFingerprint{}, err
	}
	if !identity.Exists {
		return supervisorLogFingerprint{Identity: identity}, nil
	}
	defer file.Close()
	size, err := strconv.ParseInt(identity.Size, 10, 64)
	if err != nil || size < 0 || size > supervisorLogClearVerifyLimit {
		return supervisorLogFingerprint{}, failure("log_clear_verification_failed", "Supervisor log exceeds the safe clear-verification bound; no clear is claimed", 413)
	}
	data := make([]byte, int(size))
	if size > 0 {
		if _, err = io.ReadFull(io.NewSectionReader(file, 0, size), data); err != nil {
			return supervisorLogFingerprint{}, failure("log_clear_verification_failed", "Supervisor log changed while its clear state was being checked", 409)
		}
	}
	afterRead, err := e.supervisorLogIdentityForFile(file)
	if err != nil || afterRead != identity {
		return supervisorLogFingerprint{}, failure("log_clear_verification_failed", "Supervisor log changed while its clear state was being checked", 409)
	}
	currentPath, err := e.supervisorLogSnapshot(instance, name, stream)
	if err != nil || !supervisorLogIdentityMatches(currentPath, identity) {
		return supervisorLogFingerprint{}, failure("log_clear_verification_failed", "Supervisor log was replaced while its clear state was being checked", 409)
	}
	digest := sha256.Sum256(data)
	return supervisorLogFingerprint{Identity: identity, SHA256: hex.EncodeToString(digest[:])}, nil
}

func supervisorLogClearWasNotObserved(before, after supervisorLogFingerprint) bool {
	if !before.Identity.Exists || !after.Identity.Exists ||
		before.Identity.Device != after.Identity.Device || before.Identity.Inode != after.Identity.Inode ||
		before.Identity.Size != after.Identity.Size || before.SHA256 == "" || before.SHA256 != after.SHA256 {
		return false
	}
	size, err := strconv.ParseInt(before.Identity.Size, 10, 64)
	return err == nil && size > 0
}

func (e *PrivilegedExecutor) readSupervisorLog(instance Instance, name, stream string, maxBytes int) (map[string]any, error) {
	file, identity, err := e.openSupervisorLog(instance, name, stream)
	if err != nil {
		return nil, err
	}
	if identity.Exists {
		defer file.Close()
	}
	if maxBytes == 0 {
		maxBytes = supervisorLogReadLimit
	}
	if !identity.Exists {
		return map[string]any{"stream": stream, "text": "", "bytes": 0, "truncated": false, "file_identity": identity}, nil
	}
	size, _ := strconv.ParseInt(identity.Size, 10, 64)
	readSize := int64(maxBytes)
	if size < readSize {
		readSize = size
	}
	data := make([]byte, int(readSize))
	if readSize > 0 {
		if _, err = file.ReadAt(data, size-readSize); err != nil {
			return nil, failure("log_changed", "Supervisor log changed while it was being read; retry the read", 409)
		}
	}
	openedIdentity, err := e.supervisorLogIdentityForFile(file)
	if err != nil {
		return nil, err
	}
	pathIdentity, err := e.supervisorLogSnapshot(instance, name, stream)
	if err != nil {
		return nil, err
	}
	if !supervisorLogIdentityMatches(openedIdentity, identity) || !supervisorLogIdentityMatches(pathIdentity, identity) {
		return nil, failure("log_changed", "Supervisor rotated the log while it was being read; retry the read", 409)
	}
	text := string(data)
	if !utf8.Valid(data) {
		text = strings.ToValidUTF8(text, "\uFFFD")
	}
	textTruncated := false
	if len(text) > maxBytes {
		cut := 0
		for cut < len(text) {
			_, width := utf8.DecodeRuneInString(text[cut:])
			if cut+width > maxBytes {
				break
			}
			cut += width
		}
		text = text[:cut]
		textTruncated = true
	}
	return map[string]any{"stream": stream, "text": text, "bytes": len(data), "truncated": size > readSize || textTruncated, "file_identity": identity}, nil
}

func (e *PrivilegedExecutor) registeredSupervisorLogJob(instance Instance, name string) (utilityRecord, error) {
	if instance.PluginID != "supervisor" || instance.Ownership != "managed" || instance.Status == "uninstalled" || instance.Service.Ownership != "managed" || instance.Service.PluginID != "supervisor" || instance.Service.InstanceID != instance.InstanceID {
		return utilityRecord{}, failure("ownership_invalid", "Supervisor logs require an installed protected Supervisor instance", 412)
	}
	record, err := e.utilityRecord(instance)
	if err != nil {
		return utilityRecord{}, err
	}
	if record.Revision < 1 {
		return utilityRecord{}, failure("ownership_invalid", "Protected Supervisor job revision is invalid", 412)
	}
	entry, ok := record.Items[name]
	if !ok {
		return utilityRecord{}, failure("not_found", "Registered Supervisor job does not exist", 404)
	}
	if entry.Name != name || !utilityName.MatchString(entry.Name) {
		return utilityRecord{}, failure("ownership_invalid", "Protected Supervisor job record is invalid", 412)
	}
	if _, ok := e.config.Programs[entry.ProgramID]; !ok {
		return utilityRecord{}, failure("program_not_trusted", "Supervisor job is no longer protected by root policy", 412)
	}
	return record, nil
}

func (e *PrivilegedExecutor) supervisorLog(ctx context.Context, raw json.RawMessage) (any, error) {
	var input SupervisorLogInput
	if err := parse(raw, &input); err != nil {
		return nil, err
	}
	unlock, err := e.lockContext(ctx, input.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if input.Command == "clear" {
		if err = e.ensureInstallFinalized(ctx, input.ID); err != nil {
			return nil, err
		}
	}
	instance, err := e.get(ctx, input.ID, false)
	if err != nil {
		return nil, err
	}
	record, err := e.registeredSupervisorLogJob(instance, input.Name)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	switch input.Command {
	case "read":
		result, err := e.readSupervisorLog(instance, input.Name, input.Stream, input.MaxBytes)
		if err != nil {
			return nil, err
		}
		result["id"] = instance.ID
		result["name"] = input.Name
		result["revision"] = record.Revision
		return result, nil
	case "preview_clear":
		stdout, err := e.supervisorLogSnapshot(instance, input.Name, "stdout")
		if err != nil {
			return nil, err
		}
		stderr, err := e.supervisorLogSnapshot(instance, input.Name, "stderr")
		if err != nil {
			return nil, err
		}
		return map[string]any{"id": instance.ID, "name": input.Name, "revision": record.Revision, "stdout": stdout, "stderr": stderr, "clears_streams": []string{"stdout", "stderr"}}, nil
	case "clear":
		if record.Revision != input.ExpectedRevision {
			return nil, failure("revision_conflict", "Supervisor job configuration changed; create a new log clear preview", 412)
		}
		stdout, err := e.supervisorLogSnapshot(instance, input.Name, "stdout")
		if err != nil {
			return nil, err
		}
		stderr, err := e.supervisorLogSnapshot(instance, input.Name, "stderr")
		if err != nil {
			return nil, err
		}
		if !supervisorLogIdentityMatches(stdout, *input.ExpectedStdout) || !supervisorLogIdentityMatches(stderr, *input.ExpectedStderr) {
			return nil, failure("revision_conflict", "A Supervisor job log was replaced after preview; create a new preview", 412)
		}
		stdoutBefore, err := e.supervisorLogClearFingerprint(instance, input.Name, "stdout")
		if err != nil {
			return nil, err
		}
		stderrBefore, err := e.supervisorLogClearFingerprint(instance, input.Name, "stderr")
		if err != nil {
			return nil, err
		}
		if !supervisorLogIdentityMatches(stdoutBefore.Identity, *input.ExpectedStdout) || !supervisorLogIdentityMatches(stderrBefore.Identity, *input.ExpectedStderr) {
			return nil, failure("revision_conflict", "A Supervisor job log was replaced after preview; create a new preview", 412)
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		// The RPC is scoped to the registered program, but Supervisor can return
		// true when a failed process has no live output dispatcher to clear.
		response, err := e.supervisorRPC(ctx, instance, "supervisor.clearProcessLogs", input.Name)
		if err != nil {
			return nil, err
		}
		if err = supervisorRequireXMLRPCTrue(response); err != nil {
			return nil, err
		}
		stdoutAfter, stdoutErr := e.supervisorLogClearFingerprint(instance, input.Name, "stdout")
		stderrAfter, stderrErr := e.supervisorLogClearFingerprint(instance, input.Name, "stderr")
		if stdoutErr != nil || stderrErr != nil {
			return nil, failure("log_clear_verification_failed", "Supervisor accepted the request, but the resulting log files could not be verified safely; no clear is claimed", 502)
		}
		if supervisorLogClearWasNotObserved(stdoutBefore, stdoutAfter) || supervisorLogClearWasNotObserved(stderrBefore, stderrAfter) {
			return nil, failure("log_clear_verification_failed", "Supervisor accepted the request, but nonempty log contents were unchanged; no clear is claimed", 502)
		}
		return map[string]any{"id": instance.ID, "name": input.Name, "cleared_streams": []string{"stdout", "stderr"}, "revision": record.Revision, "stdout_after": stdoutAfter.Identity, "stderr_after": stderrAfter.Identity, "live_output_may_reappear": true}, nil
	default:
		return nil, failure("invalid_input", "Unknown Supervisor log action", 400)
	}
}

func supervisorRequireXMLRPCTrue(body string) error {
	var response struct {
		Params struct {
			Param []struct {
				Value struct {
					Boolean string `xml:"boolean"`
				} `xml:"value"`
			} `xml:"param"`
		} `xml:"params"`
	}
	if err := xml.Unmarshal([]byte(body), &response); err != nil || len(response.Params.Param) != 1 || response.Params.Param[0].Value.Boolean != "1" {
		return failure("supervisor_operation_failed", "Supervisor did not confirm clearing the registered job logs", 502)
	}
	return nil
}

func supervisorLogIdentitySchema() map[string]any {
	return obj(map[string]any{
		"exists":      map[string]any{"type": "boolean"},
		"size":        map[string]any{"type": "string", "pattern": `^(0|[1-9][0-9]{0,18})$`},
		"mod_time_ns": map[string]any{"type": "string", "pattern": `^(0|-?[1-9][0-9]{0,18})$`},
		"device":      map[string]any{"type": "string", "pattern": `^(0|[1-9][0-9]{0,19})$`},
		"inode":       map[string]any{"type": "string", "pattern": `^(0|[1-9][0-9]{0,19})$`},
	}, "exists", "size", "mod_time_ns", "device", "inode")
}

func (m *Manager) supervisorLogBindings() []core.Binding {
	read := m.binding("supervisor_log_read", "Read a bounded UTF-8 tail from a registered Supervisor job stdout or stderr log", "supervisor:read", true, obj(map[string]any{
		"id":        idSchema(),
		"name":      map[string]any{"type": "string", "pattern": utilityName.String()},
		"stream":    enumSchema("stdout", "stderr"),
		"max_bytes": map[string]any{"type": "integer", "minimum": 1, "maximum": supervisorLogReadLimit},
	}, "id", "name", "stream"), idResource, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		var input SupervisorLogInput
		if err := parse(raw, &input); err != nil {
			return core.ActionResult{}, err
		}
		if _, err := m.supervisorLogInstance(ctx, input.ID); err != nil {
			return core.ActionResult{}, err
		}
		input.Command = "read"
		forward, _ := json.Marshal(input)
		var output any
		if err := m.agentJSON(ctx, "supervisor_log_action", forward, &output); err != nil {
			return core.ActionResult{}, err
		}
		return core.ActionResult{Data: output, Resources: []string{InstanceResource(input.ID)}}, nil
	})
	preview := m.binding("supervisor_log_clear_preview", "Preview both registered Supervisor job logs before a revision and inode checked clear", "supervisor:read", true, obj(map[string]any{
		"id":   idSchema(),
		"name": map[string]any{"type": "string", "pattern": utilityName.String()},
	}, "id", "name"), idResource, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		var input SupervisorLogInput
		if err := parse(raw, &input); err != nil {
			return core.ActionResult{}, err
		}
		if _, err := m.supervisorLogInstance(ctx, input.ID); err != nil {
			return core.ActionResult{}, err
		}
		input.Command = "preview_clear"
		forward, _ := json.Marshal(input)
		var output any
		if err := m.agentJSON(ctx, "supervisor_log_action", forward, &output); err != nil {
			return core.ActionResult{}, err
		}
		return core.ActionResult{Data: output, Resources: []string{InstanceResource(input.ID)}}, nil
	})
	clear := m.binding("supervisor_log_clear", "Clear both stdout and stderr logs for one registered job using Supervisor's process log RPC", "supervisor:manage", false, obj(map[string]any{
		"id":                idSchema(),
		"name":              map[string]any{"type": "string", "pattern": utilityName.String()},
		"expected_revision": revisionSchema(),
		"expected_stdout":   supervisorLogIdentitySchema(),
		"expected_stderr":   supervisorLogIdentitySchema(),
	}, "id", "name", "expected_revision", "expected_stdout", "expected_stderr"), idResource, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		var input SupervisorLogInput
		if err := parse(raw, &input); err != nil {
			return core.ActionResult{}, err
		}
		if _, err := m.supervisorLogInstance(ctx, input.ID); err != nil {
			return core.ActionResult{}, err
		}
		input.Command = "clear"
		forward, _ := json.Marshal(input)
		resources := []string{InstanceResource(input.ID)}
		job, err := m.deps.Tasks.Submit(ctx, actor, "supervisor_log_action", forward, resources)
		if err != nil {
			return core.ActionResult{}, err
		}
		id, _ := job["id"].(string)
		return core.ActionResult{Data: job, TaskID: id, Resources: resources}, nil
	})
	clear.Action.Task = true
	return []core.Binding{read, preview, clear}
}

func (m *Manager) supervisorLogInstance(ctx context.Context, id string) (Instance, error) {
	var instance Instance
	raw, _ := json.Marshal(IDInput{ID: id})
	if err := m.agentJSON(ctx, "plugin_get", raw, &instance); err != nil {
		return Instance{}, err
	}
	if instance.PluginID != "supervisor" {
		return Instance{}, failure("runtime_type_mismatch", "Supervisor log actions require a managed Supervisor instance", 412)
	}
	return instance, nil
}
