package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func pureFTPLegacyTestLine(name, passwordHash string, deny string) string {
	fields := []string{name, passwordHash, "1000", "1000", "fixture", "/srv/" + name, "", "", "", "", "", "", "", "allow-local", "deny-local", "allow-client", deny, ""}
	return strings.Join(fields, ":")
}

func TestPureFTPLegacyRecoveryClearsOnlyExactGeneratedDenyField(t *testing.T) {
	name := "report_user"
	passwordHash := "$6$original$fixturehash"
	otherLine := pureFTPLegacyTestLine("second_user", "$6$other$hash", "")
	input := []byte("# protected fixture\n" + pureFTPLegacyTestLine(name, passwordHash, ftpLegacyDenyRule) + "\n" + otherLine + "\n")

	updated, before, after, err := pureFTPClearLegacyDenyRule(input, name, passwordHash)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("clearing the deny field did not change the protected field fingerprint")
	}
	if !strings.HasPrefix(string(updated), "# protected fixture\n") || !strings.HasSuffix(string(updated), otherLine+"\n") {
		t.Fatal("comment or unrelated account row was not preserved")
	}
	start, end, parsedHash, _, err := pureFTPAccountFields(updated, name)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(string(updated[start:end]), ":")
	if parsedHash != passwordHash || len(fields) != 18 || fields[ftpLegacyDenyFieldIndex] != "" || fields[17] != "" || fields[13] != "allow-local" || fields[14] != "deny-local" || fields[15] != "allow-client" {
		t.Fatalf("recovery changed fields beyond the reviewed deny rule: hash=%q fields=%#v", parsedHash, fields)
	}
	if _, _, _, err = pureFTPClearLegacyDenyRule(updated, name, passwordHash); err == nil {
		t.Fatal("recovery accepted a row after the exact legacy rule had already been cleared")
	}
	for _, unknown := range []string{"0.0.0.0/0", "::/0", "10.0.0.0/8", "0.0.0.0/0,::/0,192.0.2.1"} {
		bad := []byte(pureFTPLegacyTestLine(name, passwordHash, unknown) + "\n")
		if _, _, _, clearErr := pureFTPClearLegacyDenyRule(bad, name, passwordHash); clearErr == nil {
			t.Fatalf("recovery accepted unknown deny-client-IP rule %q", unknown)
		}
	}
	malformedTime := []byte(pureFTPLegacyTestLine(name, passwordHash, ftpLegacyDenyRule) + "unknown\n")
	if _, _, _, err = pureFTPClearLegacyDenyRule(malformedTime, name, passwordHash); err == nil {
		t.Fatal("recovery accepted a noncanonical trailing time field")
	}
	if _, _, _, err = pureFTPClearLegacyDenyRule(input, name, "$6$changed$hash"); err == nil {
		t.Fatal("recovery accepted a changed password verifier")
	}
	// The old -R call added literal colons to a colon-delimited row. Verify the
	// only non-generated field moved by that serialization (time) is restored.
	// Debian pure-pw emits these values with `%u-%u`, so valid serialized
	// times are canonical decimal values without zero padding.
	legacyWithTime := pureFTPLegacyTestLine(name, passwordHash, ftpLegacyDenyRule) + "100-200"
	updated, _, _, err = pureFTPClearLegacyDenyRule([]byte(legacyWithTime+"\n"), name, passwordHash)
	if err != nil {
		t.Fatal(err)
	}
	start, end, _, _, err = pureFTPAccountFields(updated, name)
	if err != nil {
		t.Fatal(err)
	}
	fields = strings.Split(string(updated[start:end]), ":")
	if len(fields) != 18 || fields[ftpLegacyDenyFieldIndex] != "" || fields[17] != "100-200" {
		t.Fatalf("recovery did not restore the original time restriction to field 17: %#v", fields)
	}
}

func TestPureFTPLegacyRecoveryUsesPurePWExclusiveSidecar(t *testing.T) {
	_, instance, name, passwordHash, _ := pureFTPStateFixture(t)
	path := filepath.Join(instance.Paths.ConfigDir, pureFTPPasswdName)
	line := pureFTPLegacyTestLine(name, passwordHash, ftpLegacyDenyRule)
	if err := os.WriteFile(path, []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := snapshotFTPAccount(instance, name)
	if err != nil {
		t.Fatal(err)
	}
	sidecarPath := path + ".tmp"
	if err = os.WriteFile(sidecarPath, []byte("external-pure-pw-writer"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = atomicReplacePureFTPLegacyDeny(instance, before, name, passwordHash); core.ErrorCode(err) != "ftp_account_state_conflict" {
		t.Fatalf("preexisting pure-pw sidecar was not treated as a writer conflict: %v", err)
	}
	if sidecar, readErr := os.ReadFile(sidecarPath); readErr != nil || string(sidecar) != "external-pure-pw-writer" {
		t.Fatalf("preexisting sidecar was modified or removed: %q %v", sidecar, readErr)
	}
	unchanged, err := snapshotFTPAccount(instance, name)
	if err != nil || unchanged.SHA256 != before.SHA256 {
		t.Fatalf("busy writer sidecar changed the protected account file: %v", err)
	}
	if err = os.Remove(sidecarPath); err != nil {
		t.Fatal(err)
	}
	if _, err = atomicReplacePureFTPLegacyDeny(instance, before, name, passwordHash); err != nil {
		t.Fatal(err)
	}
	result, err := snapshotFTPAccount(instance, name)
	if err != nil || result.PasswordHash != passwordHash {
		t.Fatalf("legacy recovery did not preserve the original verifier: %q %v", result.PasswordHash, err)
	}
	_, end, _, _, err := pureFTPAccountFields(result.Data, name)
	if err != nil || strings.Split(string(result.Data[:end]), ":")[ftpLegacyDenyFieldIndex] != "" {
		t.Fatalf("legacy recovery did not clear exactly the deny field: %q %v", result.Data[:end], err)
	}
	if _, err = os.Lstat(sidecarPath); !os.IsNotExist(err) {
		t.Fatalf("successful replacement left the reserved sidecar behind: %v", err)
	}
}

func TestPureFTPPauseUsesPurePWExclusiveSidecar(t *testing.T) {
	_, instance, name, passwordHash, _ := pureFTPStateFixture(t)
	before, err := snapshotFTPAccount(instance, name)
	if err != nil {
		t.Fatal(err)
	}
	sidecarPath := filepath.Join(instance.Paths.ConfigDir, pureFTPPasswdName+".tmp")
	if err = os.WriteFile(sidecarPath, []byte("external-pure-pw-writer"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = atomicReplacePureFTPHash(instance, before, name, passwordHash, "$6$paused$sidecar-check"); core.ErrorCode(err) != "ftp_account_state_conflict" {
		t.Fatalf("pause did not fail closed on the pure-pw writer sidecar: %v", err)
	}
	if sidecar, readErr := os.ReadFile(sidecarPath); readErr != nil || string(sidecar) != "external-pure-pw-writer" {
		t.Fatalf("pause modified or removed the existing pure-pw sidecar: %q %v", sidecar, readErr)
	}
	unchanged, err := snapshotFTPAccount(instance, name)
	if err != nil || unchanged.SHA256 != before.SHA256 {
		t.Fatalf("blocked pause changed the account file: %v", err)
	}
	if err = os.Remove(sidecarPath); err != nil {
		t.Fatal(err)
	}
	if _, err = atomicReplacePureFTPHash(instance, before, name, passwordHash, "$6$paused$sidecar-check"); err != nil {
		t.Fatal(err)
	}
	paused, err := snapshotFTPAccount(instance, name)
	if err != nil || paused.PasswordHash != "$6$paused$sidecar-check" {
		t.Fatalf("pause writer failed after the sidecar cleared: %q %v", paused.PasswordHash, err)
	}
	if _, err = os.Lstat(sidecarPath); !os.IsNotExist(err) {
		t.Fatalf("successful pause left the reserved sidecar behind: %v", err)
	}
}

func TestLegacyPureFTPRecoveryJournalIsExplicitAndRetryable(t *testing.T) {
	e, instance, name, passwordHash, _ := pureFTPStateFixture(t)
	line := pureFTPLegacyTestLine(name, passwordHash, ftpLegacyDenyRule)
	passwd := filepath.Join(instance.Paths.ConfigDir, pureFTPPasswdName)
	if err := os.WriteFile(passwd, []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	record := utilityRecord{Revision: 7, Items: map[string]utilityAccount{
		name: {Name: name, FTPEnabled: ftpAccountEnabledValue(false), Password: "must-not-leak"},
	}}
	if err := e.save("private", utilityStateID(instance), record); err != nil {
		t.Fatal(err)
	}
	input := UtilityInput{ID: instance.ID, Name: name, Command: "legacy_recover", ExpectedRevision: 7, ConfirmClearLegacyDenyRule: true}
	builds := 0
	if _, err := e.recoverLegacyPureFTPAccount(context.Background(), instance, input, record, func(context.Context, Instance) error {
		builds++
		return errors.New("fixture mkdb failure")
	}); err == nil {
		t.Fatal("failed PureDB rebuild reported a successful legacy recovery")
	}
	journal, err := e.loadFTPLegacyRecoveryJournal(instance, name)
	if err != nil || journal == nil || journal.Phase != "file_replaced" {
		t.Fatalf("failed PureDB rebuild lost the recovery journal: %#v %v", journal, err)
	}
	state, err := e.inspectFTPAccountState(instance, record, name)
	if err != nil || state.Phase != "legacy_recovery_pending" || !state.RecoveryRequired || state.LastConfirmedEnabled != nil || state.Revision != 7 || len(state.AllowedRecoveryCommands) != 1 || state.AllowedRecoveryCommands[0] != "legacy_recover" {
		t.Fatalf("read-only inspection did not expose truthful recovery state: %#v %v", state, err)
	}
	listed, err := e.ftpAccountStateViews(instance, record)
	if err != nil || len(listed) != 1 || listed[0].Phase != "legacy_recovery_pending" || listed[0].Enabled != nil {
		t.Fatalf("list did not preserve pending-state truth: %#v %v", listed, err)
	}
	if encoded, marshalErr := json.Marshal(state); marshalErr != nil || strings.Contains(string(encoded), passwordHash) || strings.Contains(string(encoded), "must-not-leak") {
		t.Fatalf("inspection leaked protected credential data: %s %v", encoded, marshalErr)
	}
	unchanged, err := e.utilityRecord(instance)
	if err != nil || unchanged.Revision != record.Revision || ftpAccountEnabled(unchanged.Items[name]) {
		t.Fatalf("read-only inspection repaired the record: %#v %v", unchanged, err)
	}

	result, err := e.recoverLegacyPureFTPAccount(context.Background(), instance, input, unchanged, func(context.Context, Instance) error {
		builds++
		return nil
	})
	if err != nil || result["enabled"] != true || result["revision"] != int64(8) || builds != 2 {
		t.Fatalf("explicit retry did not finish recovery exactly once at the record boundary: %#v builds=%d err=%v", result, builds, err)
	}
	final, err := e.utilityRecord(instance)
	if err != nil || final.Revision != 8 || !ftpAccountEnabled(final.Items[name]) {
		t.Fatalf("successful recovery did not commit enabled state: %#v %v", final, err)
	}
	current, err := snapshotFTPAccount(instance, name)
	if err != nil || current.PasswordHash != passwordHash {
		t.Fatalf("successful recovery changed the account verifier: %q %v", current.PasswordHash, err)
	}
	_, end, _, _, err := pureFTPAccountFields(current.Data, name)
	if err != nil || strings.Split(string(current.Data[:end]), ":")[ftpLegacyDenyFieldIndex] != "" {
		t.Fatalf("successful recovery did not clear only the legacy deny field: %q %v", current.Data[:end], err)
	}
	if journal, err = e.loadFTPLegacyRecoveryJournal(instance, name); err != nil || journal != nil {
		t.Fatalf("completed recovery journal was not retired: %#v %v", journal, err)
	}
}

func TestLegacyFTPStatusDoesNotClaimTheMalformedRuleWasFullyPaused(t *testing.T) {
	e, instance, name, passwordHash, _ := pureFTPStateFixture(t)
	passwd := filepath.Join(instance.Paths.ConfigDir, pureFTPPasswdName)
	line := pureFTPLegacyTestLine(name, passwordHash, ftpLegacyDenyRule)
	if err := os.WriteFile(passwd, []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	record := utilityRecord{Revision: 9, Items: map[string]utilityAccount{
		name: {Name: name, FTPEnabled: ftpAccountEnabledValue(false)},
	}}
	state, err := e.inspectFTPAccountState(instance, record, name)
	if err != nil || state.Phase != "legacy_recovery_required" || !state.RecoveryRequired || state.LastConfirmedEnabled != nil || state.Revision != 9 || len(state.AllowedRecoveryCommands) != 1 || state.AllowedRecoveryCommands[0] != "legacy_recover" {
		t.Fatalf("legacy malformed deny rule was reported as a confirmed pause: %#v %v", state, err)
	}
	items, err := e.ftpAccountStateViews(instance, record)
	if err != nil || len(items) != 1 || items[0].Phase != "legacy_recovery_required" || items[0].Enabled != nil {
		t.Fatalf("legacy account list claimed an unverified enabled state: %#v %v", items, err)
	}
	// A custom deny list with similar pieces is not migrated as the old rule.
	custom := pureFTPLegacyTestLine(name, passwordHash, "0.0.0.0/0,::/0,192.0.2.1")
	if err = os.WriteFile(passwd, []byte(custom+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state, err = e.inspectFTPAccountState(instance, record, name)
	if err != nil || state.Phase != "legacy_unrecoverable" || len(state.AllowedRecoveryCommands) != 0 {
		t.Fatalf("unknown custom deny data was offered for automatic recovery: %#v %v", state, err)
	}
}

func TestLegacyPendingReadRemainsTruthfulAfterEnabledRecordSave(t *testing.T) {
	e, instance, name, passwordHash, _ := pureFTPStateFixture(t)
	line := pureFTPLegacyTestLine(name, passwordHash, ftpLegacyDenyRule)
	passwd := filepath.Join(instance.Paths.ConfigDir, pureFTPPasswdName)
	if err := os.WriteFile(passwd, []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := snapshotFTPAccount(instance, name)
	if err != nil {
		t.Fatal(err)
	}
	updated, legacyFields, recoveredFields, err := pureFTPClearLegacyDenyRule(before.Data, name, passwordHash)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(passwd, updated, 0600); err != nil {
		t.Fatal(err)
	}
	if err = e.saveFTPLegacyRecoveryJournal(instance, ftpLegacyRecoveryJournal{
		Version: ftpLegacyJournalVersion, InstanceID: instance.ID, Name: name, Phase: "database_rebuilt",
		ExpectedRecordRevision: 7, PasswordHash: passwordHash,
		LegacyFieldsSHA256: legacyFields, RecoveredFieldsSHA256: recoveredFields,
	}); err != nil {
		t.Fatal(err)
	}
	record := utilityRecord{Revision: 8, Items: map[string]utilityAccount{
		name: {Name: name, FTPEnabled: ftpAccountEnabledValue(true)},
	}}
	if err = e.save("private", utilityStateID(instance), record); err != nil {
		t.Fatal(err)
	}
	state, err := e.inspectFTPAccountState(instance, record, name)
	if err != nil || state.Phase != "legacy_recovery_pending" || !state.RecoveryRequired || state.LastConfirmedEnabled != nil || state.Revision != 7 || len(state.AllowedRecoveryCommands) != 1 || state.AllowedRecoveryCommands[0] != "legacy_recover" {
		t.Fatalf("read exposed an enabled record while its journal was still pending: %#v %v", state, err)
	}
	items, err := e.ftpAccountStateViews(instance, record)
	if err != nil || len(items) != 1 || items[0].Phase != "legacy_recovery_pending" || items[0].Enabled != nil {
		t.Fatalf("list exposed an enabled record while its journal was still pending: %#v %v", items, err)
	}
	input := UtilityInput{ID: instance.ID, Name: name, Command: "legacy_recover", ExpectedRevision: 7, ConfirmClearLegacyDenyRule: true}
	result, err := e.recoverLegacyPureFTPAccount(context.Background(), instance, input, record, func(context.Context, Instance) error {
		t.Fatal("database_rebuilt recovery unexpectedly reran PureDB rebuild")
		return nil
	})
	if err != nil || result["revision"] != int64(8) || result["enabled"] != true {
		t.Fatalf("recovery did not reconcile the saved record and journal: %#v %v", result, err)
	}
}

func TestLegacyPureFTPRecoveryBindingIsDurableAndPreciselyConfirmed(t *testing.T) {
	_, _, manager := databaseManagerFixture(t)
	binding := manager.ftpLegacyRecoveryBinding()
	if binding.Action.Name != "ftp_account_state_legacy_recover" || binding.Action.Scope != "ftp:manage" || binding.Action.ReadOnly || !binding.Action.Task || !binding.Action.Audit {
		t.Fatalf("legacy recovery binding is not durable audited FTP management: %#v", binding.Action)
	}
	raw := []byte(`{"id":"ftp-production","name":"report_user","expected_revision":8,"confirm_clear_legacy_deny_rule":true}`)
	resources, err := binding.Resources(nil, core.Actor{}, raw)
	if err != nil || len(resources) != 1 || resources[0] != InstanceResource("ftp-production") {
		t.Fatalf("legacy recovery binding did not scope the instance: %#v %v", resources, err)
	}
	serialized, err := json.Marshal(binding.Action.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(serialized), `"confirm_clear_legacy_deny_rule":{"const":true,"type":"boolean"}`) {
		t.Fatalf("legacy recovery schema does not require explicit true confirmation: %s", serialized)
	}
}
