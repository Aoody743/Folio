package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Aoody743/PowerLinux/internal/core"
	"golang.org/x/sys/unix"
)

const (
	ftpLegacyDenyRule       = "0.0.0.0/0,::/0"
	ftpLegacyDenyFieldIndex = 16 // Debian Pure-FTPd 1.0.50: denied client IP.
	ftpLegacyJournalVersion = 1
)

type FTPLegacyRecoveryInput struct {
	ID                         string `json:"id"`
	Name                       string `json:"name"`
	ExpectedRevision           int64  `json:"expected_revision"`
	ConfirmClearLegacyDenyRule bool   `json:"confirm_clear_legacy_deny_rule"`
}

func (m *Manager) ftpLegacyRecoveryBinding() core.Binding {
	binding := m.binding("ftp_account_state_legacy_recover", "Explicitly clear only the known legacy-generated FTP deny rule after review", "ftp:manage", false, obj(map[string]any{
		"id":                             idSchema(),
		"name":                           map[string]any{"type": "string", "pattern": utilityName.String()},
		"expected_revision":              revisionSchema(),
		"confirm_clear_legacy_deny_rule": map[string]any{"type": "boolean", "const": true},
	}, "id", "name", "expected_revision", "confirm_clear_legacy_deny_rule"), idResource, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		var input FTPLegacyRecoveryInput
		if err := parse(raw, &input); err != nil {
			return core.ActionResult{}, err
		}
		if !validInstanceID(input.ID) || !utilityName.MatchString(input.Name) || input.ExpectedRevision < 1 || !input.ConfirmClearLegacyDenyRule {
			return core.ActionResult{}, failure("invalid_input", "Explicit confirmation and a current FTP account revision are required", 400)
		}
		var instance Instance
		idRaw, _ := json.Marshal(IDInput{ID: input.ID})
		if err := m.agentJSON(ctx, "plugin_get", idRaw, &instance); err != nil {
			return core.ActionResult{}, err
		}
		if instance.PluginID != "pureftp" {
			return core.ActionResult{}, failure("runtime_type_mismatch", "Legacy FTP recovery requires a managed PureFTP instance", 412)
		}
		jobInput, _ := json.Marshal(UtilityInput{
			ID: input.ID, Name: input.Name, Command: "legacy_recover", ExpectedRevision: input.ExpectedRevision,
			ConfirmClearLegacyDenyRule: true,
		})
		resource := InstanceResource(input.ID)
		job, err := m.deps.Tasks.Submit(ctx, actor, "utility_action", jobInput, []string{resource})
		if err != nil {
			return core.ActionResult{}, err
		}
		taskID, _ := job["id"].(string)
		if taskID == "" {
			taskID, _ = job["task_id"].(string)
		}
		return core.ActionResult{Data: job, TaskID: taskID, Resources: []string{resource}, AuditSummary: map[string]any{
			"instance_id": input.ID, "account": input.Name, "command": "legacy_recover", "confirmed": true,
		}}, nil
	})
	binding.Action.Task = true
	binding.Action.Audit = true
	return binding
}

type ftpLegacyRecoveryJournal struct {
	Version                int    `json:"version"`
	InstanceID             string `json:"instance_id"`
	Name                   string `json:"name"`
	Phase                  string `json:"phase"`
	ExpectedRecordRevision int64  `json:"expected_record_revision"`
	PasswordHash           string `json:"password_hash"`
	LegacyFieldsSHA256     string `json:"legacy_fields_sha256"`
	RecoveredFieldsSHA256  string `json:"recovered_fields_sha256"`
}

func ftpLegacyJournalID(instanceID, name string) string {
	digest := sha256.Sum256([]byte(instanceID + "\x00" + name))
	return "ftp_legacy_" + hex.EncodeToString(digest[:16])
}

func validFTPLegacyJournal(j ftpLegacyRecoveryJournal, instance Instance, name string) bool {
	if j.Version != ftpLegacyJournalVersion || j.InstanceID != instance.ID || j.Name != name || !utilityName.MatchString(name) ||
		j.ExpectedRecordRevision < 1 || j.PasswordHash == "" || len(j.PasswordHash) > 512 || strings.ContainsAny(j.PasswordHash, ":\x00\r\n") ||
		!validFTPSHA256(j.LegacyFieldsSHA256) || !validFTPSHA256(j.RecoveredFieldsSHA256) {
		return false
	}
	switch j.Phase {
	case "prepared", "file_replaced", "database_rebuilt", "record_saved":
		return true
	default:
		return false
	}
}

func validFTPSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (e *PrivilegedExecutor) loadFTPLegacyRecoveryJournal(instance Instance, name string) (*ftpLegacyRecoveryJournal, error) {
	var journal ftpLegacyRecoveryJournal
	err := e.load("private", ftpLegacyJournalID(instance.ID, name), &journal)
	if core.ErrorCode(err) == "not_found" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !validFTPLegacyJournal(journal, instance, name) {
		return nil, failure("ftp_account_state_invalid", "Protected FTP legacy-recovery journal is invalid", 412)
	}
	return &journal, nil
}

func (e *PrivilegedExecutor) saveFTPLegacyRecoveryJournal(instance Instance, journal ftpLegacyRecoveryJournal) error {
	if !validFTPLegacyJournal(journal, instance, journal.Name) {
		return failure("ftp_account_state_invalid", "Protected FTP legacy-recovery journal failed validation", 412)
	}
	return e.save("private", ftpLegacyJournalID(instance.ID, journal.Name), journal)
}

func (e *PrivilegedExecutor) removeFTPLegacyRecoveryJournal(instance Instance, name string) error {
	dir := filepath.Join(e.config.StateDir, "private")
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	key := ftpLegacyJournalID(instance.ID, name) + ".json"
	file, err := root.OpenFile(key, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	info, statErr := file.Stat()
	_ = file.Close()
	if statErr != nil || info == nil {
		return failure("ftp_account_state_invalid", "Protected FTP legacy-recovery journal could not be inspected", 412)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return failure("ftp_account_state_invalid", "Protected FTP legacy-recovery journal permissions are invalid", 412)
	}
	if err = root.Remove(key); err != nil {
		return err
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err == nil {
		err = closeErr
	}
	return err
}

func pureFTPLegacyDenyState(data []byte, name string) (passwordHash, fieldsSHA256 string, fields []string, err error) {
	start, end, passwordHash, fieldsSHA256, err := pureFTPAccountFields(data, name)
	if err != nil {
		return "", "", nil, err
	}
	fields = strings.Split(string(data[start:end]), ":")
	if len(fields) <= ftpLegacyDenyFieldIndex {
		return "", "", nil, errors.New("PureFTP account line lacks the reviewed deny-client-IP field")
	}
	return passwordHash, fieldsSHA256, fields, nil
}

// pure-pw 1.0.50 writes the -R argument verbatim into a colon-delimited
// passwd row. The old IPv4/IPv6 sentinel therefore serializes as three fields
// after the IPv4 prefix: "0.0.0.0/0,", "", and "/0". Its following time
// restriction shifts from field 17 to field 19. Accept only that exact shape.
func pureFTPLegacyDenyFields(fields []string) bool {
	return len(fields) == 20 && fields[ftpLegacyDenyFieldIndex] == "0.0.0.0/0," && fields[17] == "" && fields[18] == "/0" && pureFTPTimeField(fields[19])
}

func pureFTPTimeField(value string) bool {
	if value == "" {
		return true
	}
	startText, endText, found := strings.Cut(value, "-")
	if !found || startText == "" || endText == "" || strings.Contains(endText, "-") {
		return false
	}
	start, startErr := strconv.Atoi(startText)
	end, endErr := strconv.Atoi(endText)
	return startErr == nil && endErr == nil && strconv.Itoa(start) == startText && strconv.Itoa(end) == endText && start < 2360 && start%100 < 60 && end < 2360 && end%100 < 60
}

func pureFTPClearLegacyDenyRule(data []byte, name, expectedHash string) (updated []byte, beforeFieldsSHA256, afterFieldsSHA256 string, err error) {
	start, end, passwordHash, fieldsSHA256, err := pureFTPAccountFields(data, name)
	if err != nil {
		return nil, "", "", err
	}
	if passwordHash != expectedHash {
		return nil, "", "", errors.New("PureFTP account verifier changed")
	}
	fields := strings.Split(string(data[start:end]), ":")
	if !pureFTPLegacyDenyFields(fields) {
		return nil, "", "", errors.New("PureFTP account does not have the exact generated legacy deny rule")
	}
	// Remove only the exact malformed -R suffix and restore the original time
	// restriction to Pure-FTPd's actual field 17. The IPv6 fragment was never a
	// separate usable PureDB field; preserving it would keep the row malformed.
	recoveredFields := append([]string(nil), fields[:ftpLegacyDenyFieldIndex]...)
	recoveredFields = append(recoveredFields, "", fields[19])
	replacement := strings.Join(recoveredFields, ":")
	updated = make([]byte, 0, len(data)-len(data[start:end])+len(replacement))
	updated = append(updated, data[:start]...)
	updated = append(updated, replacement...)
	updated = append(updated, data[end:]...)
	return updated, fieldsSHA256, pureFTPFieldsFingerprint(recoveredFields), nil
}

// atomicReplacePureFTPLegacyDeny cooperates with pure-pw's documented
// pureftpd.passwd.tmp O_EXCL sidecar. A preexisting sidecar is left untouched.
func atomicReplacePureFTPLegacyDeny(instance Instance, expected pureFTPPasswdSnapshot, name, expectedHash string) (pureFTPPasswdSnapshot, error) {
	dirfd, err := openPureFTPConfigDir(instance.Paths.ConfigDir)
	if err != nil {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_invalid", "PureFTP account storage is not protected", 412)
	}
	defer unix.Close(dirfd)
	current, err := snapshotFTPAccountAt(dirfd, name)
	if err != nil {
		return pureFTPPasswdSnapshot{}, err
	}
	if current.Stamp != expected.Stamp || current.SHA256 != expected.SHA256 || current.PasswordHash != expectedHash || current.FieldsSHA256 != expected.FieldsSHA256 {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_conflict", "PureFTP account changed before legacy recovery", 409)
	}
	temp := pureFTPPasswdName + ".tmp"
	fd, err := unix.Openat(dirfd, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(current.Stamp.Mode&0777))
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			return pureFTPPasswdSnapshot{}, failure("ftp_account_state_conflict", "PureFTP account writer sidecar is busy; it was preserved", 409)
		}
		return pureFTPPasswdSnapshot{}, err
	}
	createdTemp := true
	defer func() {
		if createdTemp {
			_ = unix.Unlinkat(dirfd, temp, 0)
		}
	}()
	file := os.NewFile(uintptr(fd), temp)
	updated, _, afterFieldsSHA, err := pureFTPClearLegacyDenyRule(current.Data, name, expectedHash)
	if err != nil {
		_ = file.Close()
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_conflict", "PureFTP account no longer has the reviewed legacy deny rule", 409)
	}
	// Re-read after reserving the upstream writer sidecar, so a pure-pw change
	// which completed between our first read and reservation cannot be lost.
	current, err = snapshotFTPAccountAt(dirfd, name)
	if err != nil || current.Stamp != expected.Stamp || current.SHA256 != expected.SHA256 || current.PasswordHash != expectedHash {
		_ = file.Close()
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_conflict", "PureFTP account changed while reserving its writer sidecar", 409)
	}
	if _, err = file.Write(updated); err == nil {
		err = file.Sync()
	}
	if err == nil {
		err = file.Chown(int(current.Stamp.UID), int(current.Stamp.GID))
	}
	if err == nil {
		err = file.Chmod(os.FileMode(current.Stamp.Mode & 0777))
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return pureFTPPasswdSnapshot{}, err
	}
	current, err = snapshotFTPAccountAt(dirfd, name)
	if err != nil || current.Stamp != expected.Stamp || current.SHA256 != expected.SHA256 || current.PasswordHash != expectedHash {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_conflict", "PureFTP account changed before legacy recovery commit", 409)
	}
	pathfd, pathErr := openPureFTPConfigDir(instance.Paths.ConfigDir)
	if pathErr != nil {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_conflict", "PureFTP configuration directory changed before legacy recovery commit", 409)
	}
	pathDevice, pathInode, identityErr := pureFTPDirectoryIdentity(pathfd)
	_ = unix.Close(pathfd)
	dirDevice, dirInode, dirErr := pureFTPDirectoryIdentity(dirfd)
	if identityErr != nil || dirErr != nil || pathDevice != dirDevice || pathInode != dirInode {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_conflict", "PureFTP configuration directory changed before legacy recovery commit", 409)
	}
	if err = unix.Renameat(dirfd, temp, dirfd, pureFTPPasswdName); err != nil {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_failed", "PureFTP account file could not be atomically replaced", 502)
	}
	createdTemp = false
	if err = unix.Fsync(dirfd); err != nil {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_failed", "PureFTP account replacement could not be committed", 502)
	}
	result, err := snapshotFTPAccountAt(dirfd, name)
	if err != nil || result.PasswordHash != expectedHash || result.FieldsSHA256 != afterFieldsSHA {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_conflict", "PureFTP account changed during legacy recovery", 409)
	}
	return result, nil
}

func (e *PrivilegedExecutor) recoverLegacyPureFTPAccount(ctx context.Context, instance Instance, input UtilityInput, record utilityRecord, buildDB func(context.Context, Instance) error) (map[string]any, error) {
	if instance.PluginID != "pureftp" || !utilityName.MatchString(input.Name) || !input.ConfirmClearLegacyDenyRule {
		return nil, failure("invalid_input", "Explicit confirmation is required to clear the known legacy FTP deny rule", 400)
	}
	account, exists := record.Items[input.Name]
	if !exists {
		return nil, failure("not_found", "FTP account does not exist", 404)
	}
	modern, err := e.loadPureFTPStateJournal(instance, input.Name)
	if err != nil {
		return nil, err
	}
	if modern != nil {
		return nil, failure("ftp_account_state_pending", "Complete the journaled FTP pause or resume instead of legacy recovery", 409)
	}
	journal, err := e.loadFTPLegacyRecoveryJournal(instance, input.Name)
	if err != nil {
		return nil, err
	}
	if journal == nil {
		if record.Revision != input.ExpectedRevision {
			return nil, failure("revision_conflict", "FTP account configuration changed", 412)
		}
		if account.FTPEnabled == nil || *account.FTPEnabled {
			return nil, failure("ftp_account_state_conflict", "Only records explicitly paused by the legacy FTP action can be recovered", 409)
		}
		snapshot, snapshotErr := snapshotFTPAccount(instance, input.Name)
		if snapshotErr != nil {
			return nil, snapshotErr
		}
		_, _, fields, parseErr := pureFTPLegacyDenyState(snapshot.Data, input.Name)
		if parseErr != nil || !pureFTPLegacyDenyFields(fields) {
			return nil, failure("ftp_account_state_unrecoverable", "The protected account does not contain the exact legacy-generated deny rule", 409)
		}
		updated, beforeFields, afterFields, updateErr := pureFTPClearLegacyDenyRule(snapshot.Data, input.Name, snapshot.PasswordHash)
		if updateErr != nil {
			return nil, failure("ftp_account_state_unrecoverable", "The protected account does not contain the exact legacy-generated deny rule", 409)
		}
		clear(updated)
		journal = &ftpLegacyRecoveryJournal{
			Version: ftpLegacyJournalVersion, InstanceID: instance.ID, Name: input.Name, Phase: "prepared",
			ExpectedRecordRevision: record.Revision, PasswordHash: snapshot.PasswordHash,
			LegacyFieldsSHA256: beforeFields, RecoveredFieldsSHA256: afterFields,
		}
		if err = e.saveFTPLegacyRecoveryJournal(instance, *journal); err != nil {
			return nil, err
		}
	}
	if input.ExpectedRevision != journal.ExpectedRecordRevision || record.Revision < journal.ExpectedRecordRevision {
		return nil, failure("revision_conflict", "FTP legacy recovery is pinned to a different account revision", 412)
	}
	if journal.Phase == "prepared" {
		current, readErr := snapshotFTPAccount(instance, input.Name)
		if readErr != nil {
			return nil, readErr
		}
		if current.PasswordHash == journal.PasswordHash && current.FieldsSHA256 == journal.LegacyFieldsSHA256 {
			current, err = atomicReplacePureFTPLegacyDeny(instance, current, input.Name, journal.PasswordHash)
			if err != nil {
				return nil, err
			}
		} else if current.PasswordHash != journal.PasswordHash || current.FieldsSHA256 != journal.RecoveredFieldsSHA256 {
			return nil, failure("ftp_account_state_conflict", "PureFTP account changed during legacy recovery; manual review is required", 409)
		}
		journal.Phase = "file_replaced"
		if err = e.saveFTPLegacyRecoveryJournal(instance, *journal); err != nil {
			return nil, err
		}
	}
	if journal.Phase == "file_replaced" {
		current, readErr := snapshotFTPAccount(instance, input.Name)
		if readErr != nil || current.PasswordHash != journal.PasswordHash || current.FieldsSHA256 != journal.RecoveredFieldsSHA256 {
			return nil, failure("ftp_account_state_conflict", "PureFTP account no longer matches its legacy recovery journal", 409)
		}
		if err = buildDB(ctx, instance); err != nil {
			return nil, err
		}
		journal.Phase = "database_rebuilt"
		if err = e.saveFTPLegacyRecoveryJournal(instance, *journal); err != nil {
			return nil, err
		}
	}
	if journal.Phase == "database_rebuilt" {
		latest, readErr := e.utilityRecord(instance)
		if readErr != nil {
			return nil, readErr
		}
		latestAccount, exists := latest.Items[input.Name]
		if !exists || latest.Revision < journal.ExpectedRecordRevision {
			return nil, failure("revision_conflict", "FTP account record changed during legacy recovery", 412)
		}
		if !ftpAccountEnabled(latestAccount) {
			latestAccount.FTPEnabled = ftpAccountEnabledValue(true)
			latest.Items[input.Name] = latestAccount
			latest.Revision++
			if err = e.save("private", utilityStateID(instance), latest); err != nil {
				return nil, err
			}
		} else if latest.Revision == journal.ExpectedRecordRevision {
			return nil, failure("revision_conflict", "FTP account record changed during legacy recovery", 412)
		}
		journal.Phase = "record_saved"
		if err = e.saveFTPLegacyRecoveryJournal(instance, *journal); err != nil {
			return nil, err
		}
	}
	if journal.Phase != "record_saved" {
		return nil, failure("ftp_account_state_invalid", "FTP legacy recovery journal has an unsupported phase", 412)
	}
	latest, err := e.utilityRecord(instance)
	if err != nil {
		return nil, err
	}
	latestAccount, exists := latest.Items[input.Name]
	if !exists || !ftpAccountEnabled(latestAccount) || latest.Revision < journal.ExpectedRecordRevision+1 {
		return nil, failure("ftp_account_state_conflict", "FTP account record does not confirm completed legacy recovery", 409)
	}
	current, err := snapshotFTPAccount(instance, input.Name)
	if err != nil || current.PasswordHash != journal.PasswordHash || current.FieldsSHA256 != journal.RecoveredFieldsSHA256 {
		return nil, failure("ftp_account_state_conflict", "FTP account no longer matches completed legacy recovery", 409)
	}
	if err = e.removeFTPLegacyRecoveryJournal(instance, input.Name); err != nil {
		return nil, err
	}
	return map[string]any{"id": instance.ID, "name": input.Name, "command": "legacy_recover", "revision": latest.Revision, "enabled": true, "recovery": "legacy_deny_cleared"}, nil
}

func (e *PrivilegedExecutor) inspectLegacyPureFTPAccount(instance Instance, account utilityAccount) (string, []string, int64, error) {
	legacy, err := e.loadFTPLegacyRecoveryJournal(instance, account.Name)
	if err != nil {
		return "", nil, 0, err
	}
	if legacy != nil {
		return "legacy_recovery_pending", []string{"legacy_recover"}, legacy.ExpectedRecordRevision, nil
	}
	if account.FTPEnabled == nil || *account.FTPEnabled {
		return "", nil, 0, nil
	}
	snapshot, err := snapshotFTPAccount(instance, account.Name)
	if err != nil {
		return "legacy_unrecoverable", nil, 0, nil
	}
	_, _, fields, err := pureFTPLegacyDenyState(snapshot.Data, account.Name)
	if err != nil || !pureFTPLegacyDenyFields(fields) {
		return "legacy_unrecoverable", nil, 0, nil
	}
	return "legacy_recovery_required", []string{"legacy_recover"}, 0, nil
}
