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

func TestFTPAccountStateLegacyDefaultAndRedactedViews(t *testing.T) {
	e, instance, name, _, _ := pureFTPStateFixture(t)
	legacy, err := json.Marshal(utilityRecord{Revision: 4, Items: map[string]utilityAccount{
		name: {Name: name, ReadOnly: true, Password: "private-legacy-password"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded utilityRecord
	if err := json.Unmarshal(legacy, &decoded); err != nil {
		t.Fatal(err)
	}
	if !ftpAccountEnabled(decoded.Items[name]) {
		t.Fatal("a legacy FTP record without ftp_enabled must remain active")
	}
	activeViews, err := e.ftpAccountStateViews(instance, decoded)
	if err != nil || len(activeViews) != 1 || activeViews[0].Enabled == nil || !*activeViews[0].Enabled || activeViews[0].RecoveryRequired {
		t.Fatalf("legacy account was not reported as confirmed active: %#v %v", activeViews, err)
	}
	pausedHash := "$6$paused$redacted-view"
	if err = e.setPureFTPAccountEnabledUsing(context.Background(), instance, name, false, true, func(context.Context, Instance) (string, error) {
		return pausedHash, nil
	}, func(context.Context, Instance) error { return nil }); err != nil {
		t.Fatal(err)
	}
	paused := utilityRecord{Revision: 5, Items: map[string]utilityAccount{
		name: {Name: name, FTPEnabled: ftpAccountEnabledValue(false), Password: "private-paused-password"},
	}}
	items, err := e.ftpAccountStateViews(instance, paused)
	if err != nil || len(items) != 1 || items[0].Name != name || items[0].Enabled == nil || *items[0].Enabled || items[0].ReadOnly || items[0].RecoveryRequired {
		t.Fatalf("paused account view mismatch: %#v", items)
	}
	encoded, err := json.Marshal(map[string]any{"revision": paused.Revision, "items": activeViews, "accounts": items})
	if err != nil {
		t.Fatal(err)
	}
	public := string(encoded)
	if strings.Contains(public, "private-legacy-password") || strings.Contains(public, "private-paused-password") || strings.Contains(public, pausedHash) {
		t.Fatal("FTP account list/status view exposed a credential")
	}
	if !strings.Contains(public, `"enabled":true`) || !strings.Contains(public, `"enabled":false`) {
		t.Fatalf("views did not report the old-active and paused states: %s", public)
	}
}

func pureFTPStateFixture(t *testing.T) (*PrivilegedExecutor, Instance, string, string, string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("PureFTP protected file operations require root")
	}
	e := testAgent(t)
	name := "report_user"
	configDir := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	instance := Instance{ID: "pureftp-state-fixture", PluginID: "pureftp", Paths: Paths{ConfigDir: configDir, DataDir: filepath.Join(t.TempDir(), "data")}}
	fields := []string{name, "$6$original$fixturehash", "1000", "1000", "", filepath.Join(instance.Paths.DataDir, "users", name), "", "", "", "", "", "", "", "", "", "", "", ""}
	original := strings.Join(fields, ":")
	if err := os.WriteFile(filepath.Join(configDir, pureFTPPasswdName), []byte(original+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return e, instance, name, fields[1], original
}

func TestPureFTPGeneratedVerifierAccountNameAndUnsupportedReadOnly(t *testing.T) {
	name, err := pureFTPTemporaryAccountName()
	if err != nil || len(name) > 32 || !utilityName.MatchString(name) {
		t.Fatalf("generated PureFTP account name is invalid: %q %v", name, err)
	}
	fields := []string{name, "$6$temporary$hash", "1000", "1000", "", "/srv/temporary", "", "", "", "", "", "", "", "", "", "", "", ""}
	if _, _, parsedHash, _, parseErr := pureFTPAccountFields([]byte(strings.Join(fields, ":")+"\n"), name); parseErr != nil || parsedHash != fields[1] {
		t.Fatalf("generated name was rejected by the exact PureFTP parser: %q %v", parsedHash, parseErr)
	}

	e, instance, account, _, _ := pureFTPStateFixture(t)
	directory := filepath.Join(instance.Paths.DataDir, "users", account)
	if err = os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	err = e.pureFTPAccount(context.Background(), instance, UtilityInput{Command: "update", Name: account, ReadOnly: true})
	if core.ErrorCode(err) != "unsupported_action" {
		t.Fatalf("unsupported PureFTP read-only request was not rejected: %v", err)
	}
	after, statErr := os.Stat(directory)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if before.Mode() != after.Mode() {
		t.Fatalf("rejected read-only request changed existing directory permissions: before=%v after=%v", before.Mode(), after.Mode())
	}
}

func TestPureFTPPauseResumeRestoresOriginalVerifierAndReportsPendingRead(t *testing.T) {
	e, instance, name, originalHash, originalLine := pureFTPStateFixture(t)
	ctx := context.Background()
	pausedHash := "$6$paused$replacementhash"
	builds := 0
	makeHash := func(context.Context, Instance) (string, error) { return pausedHash, nil }
	buildDB := func(context.Context, Instance) error { builds++; return nil }
	if err := e.setPureFTPAccountEnabledUsing(ctx, instance, name, false, true, makeHash, buildDB); err != nil {
		t.Fatal(err)
	}
	paused, err := snapshotFTPAccount(instance, name)
	if err != nil || paused.PasswordHash != pausedHash || string(paused.Data) == originalLine+"\n" {
		t.Fatalf("pause did not replace the active verifier: hash=%q err=%v", paused.PasswordHash, err)
	}
	if builds != 1 {
		t.Fatalf("pause did not rebuild PureDB exactly once: %d", builds)
	}
	journal, err := e.loadPureFTPStateJournal(instance, name)
	if err != nil || journal == nil || journal.Phase != "paused" || journal.OriginalHash != originalHash {
		t.Fatalf("pause journal lost the exact original verifier: %#v %v", journal, err)
	}
	sealed, err := os.ReadFile(filepath.Join(e.config.StateDir, "private", ftpPauseJournalID(instance.ID, name)+".json"))
	if err != nil || strings.Contains(string(sealed), originalHash) || strings.Contains(string(sealed), pausedHash) {
		t.Fatal("FTP verifier journal was not private-encrypted")
	}

	// Simulate a crash after PureDB was restored but before the utility record
	// was updated. A read reports pending and must not repair either record.
	record := utilityRecord{Revision: 4, Items: map[string]utilityAccount{name: {Name: name, FTPEnabled: ftpAccountEnabledValue(false)}}}
	if err = e.save("private", utilityStateID(instance), record); err != nil {
		t.Fatal(err)
	}
	if err = e.setPureFTPAccountEnabledUsing(ctx, instance, name, true, true, makeHash, buildDB); err != nil {
		t.Fatal(err)
	}
	journal, err = e.loadPureFTPStateJournal(instance, name)
	if err != nil || journal == nil || journal.Phase != "resumed" {
		t.Fatalf("resume did not retain its crash marker until state persistence: %#v %v", journal, err)
	}
	beforeRecord, err := e.utilityRecord(instance)
	if err != nil {
		t.Fatal(err)
	}
	beforeFile, err := snapshotFTPAccount(instance, name)
	if err != nil {
		t.Fatal(err)
	}
	buildsBeforeRead := builds
	state, err := e.inspectFTPAccountState(instance, beforeRecord, name)
	if err != nil || !state.RecoveryRequired || state.Phase != "resume_pending" || state.LastConfirmedEnabled != nil || len(state.AllowedRecoveryCommands) != 1 || state.AllowedRecoveryCommands[0] != "resume" {
		t.Fatalf("read inspection did not safely describe unfinished resume: %#v %v", state, err)
	}
	publicState, marshalErr := json.Marshal(state)
	if marshalErr != nil || strings.Contains(string(publicState), "last_confirmed_enabled") || strings.Contains(string(publicState), originalHash) || strings.Contains(string(publicState), pausedHash) {
		t.Fatalf("pending inspection did not redact confirmed state and verifier: %s %v", publicState, marshalErr)
	}
	afterRecord, err := e.utilityRecord(instance)
	if err != nil || afterRecord.Revision != beforeRecord.Revision || ftpAccountEnabled(afterRecord.Items[name]) {
		t.Fatalf("read inspection changed the account record: %#v %v", afterRecord, err)
	}
	afterFile, err := snapshotFTPAccount(instance, name)
	if err != nil || afterFile.PasswordHash != beforeFile.PasswordHash || builds != buildsBeforeRead {
		t.Fatalf("read inspection changed the account verifier: %q %v", afterFile.PasswordHash, err)
	}
	// The explicit resume task's caller commits enabled state and retires the
	// journal only after that durable record write succeeds.
	afterRecord.Items[name] = utilityAccount{Name: name, FTPEnabled: ftpAccountEnabledValue(true)}
	afterRecord.Revision++
	if err = e.save("private", utilityStateID(instance), afterRecord); err != nil {
		t.Fatal(err)
	}
	if err = e.removePureFTPStateJournal(instance, name); err != nil {
		t.Fatal(err)
	}
	active, err := snapshotFTPAccount(instance, name)
	if err != nil || active.PasswordHash != originalHash || string(active.Data) != originalLine+"\n" {
		t.Fatalf("resume did not restore the exact original line: %q %v", active.Data, err)
	}
}

func TestPureFTPPauseRecoveryAndAmbiguousExternalChangeFailClosed(t *testing.T) {
	e, instance, name, originalHash, _ := pureFTPStateFixture(t)
	ctx := context.Background()
	pausedHash := "$6$paused$recoveryhash"
	makeHash := func(context.Context, Instance) (string, error) { return pausedHash, nil }
	buildCount := 0
	buildDB := func(context.Context, Instance) error { buildCount++; return nil }
	initial, err := snapshotFTPAccount(instance, name)
	if err != nil {
		t.Fatal(err)
	}
	journal := pureFTPAccountStateJournal{
		Version: pureFTPPauseJournalVersion, InstanceID: instance.ID, Name: name, Phase: "pause_prepared",
		OriginalHash: originalHash, PausedHash: pausedHash, FieldsSHA256: initial.FieldsSHA256,
	}
	if err = e.savePureFTPStateJournal(instance, journal); err != nil {
		t.Fatal(err)
	}
	// Simulate the atomic file replacement occurring immediately before a
	// crash, leaving the journal in its prepared phase.
	if _, err = atomicReplacePureFTPHash(instance, initial, name, originalHash, pausedHash); err != nil {
		t.Fatal(err)
	}
	if err = e.save("private", utilityStateID(instance), utilityRecord{Revision: 3, Items: map[string]utilityAccount{name: {Name: name}}}); err != nil {
		t.Fatal(err)
	}
	if err = e.setPureFTPAccountEnabledUsing(ctx, instance, name, false, true, makeHash, buildDB); err != nil {
		t.Fatal(err)
	}
	record, err := e.utilityRecord(instance)
	if err != nil || buildCount != 1 {
		t.Fatalf("prepared pause was not safely completed by an explicit action: %#v builds=%d err=%v", record, buildCount, err)
	}

	// A different password hash after the pause proof invalidates resume CAS;
	// it must not overwrite an administrator's credential change.
	current, err := snapshotFTPAccount(instance, name)
	if err != nil {
		t.Fatal(err)
	}
	externalHash := "$6$external$administrator-change"
	changed, _, err := pureFTPReplaceHash(current.Data, name, pausedHash, externalHash)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(instance.Paths.ConfigDir, pureFTPPasswdName), changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err = e.setPureFTPAccountEnabledUsing(ctx, instance, name, true, true, makeHash, buildDB); err == nil || core.ErrorCode(err) != "ftp_account_state_conflict" {
		t.Fatalf("ambiguous external credential update was overwritten: %v", err)
	}
	current, err = snapshotFTPAccount(instance, name)
	if err != nil || current.PasswordHash != externalHash {
		t.Fatalf("external verifier was changed: %q %v", current.PasswordHash, err)
	}
}

func TestPureFTPPausedAccountsSurviveOtherAccountEdits(t *testing.T) {
	e, instance, nameA, originalA, lineA := pureFTPStateFixture(t)
	nameB := "second_user"
	fieldsB := []string{nameB, "$6$original$secondhash", "1000", "1000", "", filepath.Join(instance.Paths.DataDir, "users", nameB), "", "", "", "", "", "", "", "", "", "", "", ""}
	lineB := strings.Join(fieldsB, ":")
	pausedA := "$6$paused$accounta"
	pausedB := "$6$paused$accountb"
	makeHash := func(_ context.Context, _ Instance) (string, error) { return pausedA, nil }
	builds := 0
	buildDB := func(context.Context, Instance) error { builds++; return nil }
	ctx := context.Background()
	if err := e.setPureFTPAccountEnabledUsing(ctx, instance, nameA, false, true, makeHash, buildDB); err != nil {
		t.Fatal(err)
	}
	// Adding B changes the whole passfile after A's journal is settled. A's
	// verifier and account metadata remain independently recoverable.
	path := filepath.Join(instance.Paths.ConfigDir, pureFTPPasswdName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(data, []byte(lineB+"\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	makeHash = func(_ context.Context, _ Instance) (string, error) { return pausedB, nil }
	if err = e.setPureFTPAccountEnabledUsing(ctx, instance, nameB, false, true, makeHash, buildDB); err != nil {
		t.Fatal(err)
	}
	if err = e.setPureFTPAccountEnabledUsing(ctx, instance, nameA, true, true, makeHash, buildDB); err != nil {
		t.Fatalf("pausing B invalidated A's journal: %v", err)
	}
	if err = e.setPureFTPAccountEnabledUsing(ctx, instance, nameB, true, true, makeHash, buildDB); err != nil {
		t.Fatalf("resuming A invalidated B's journal: %v", err)
	}
	activeA, err := snapshotFTPAccount(instance, nameA)
	if err != nil || activeA.PasswordHash != originalA {
		t.Fatalf("A's original verifier was not restored: %q %v", activeA.PasswordHash, err)
	}
	activeB, err := snapshotFTPAccount(instance, nameB)
	if err != nil || activeB.PasswordHash != fieldsB[1] {
		t.Fatalf("B's original verifier was not restored: %q %v", activeB.PasswordHash, err)
	}
	if builds != 4 {
		t.Fatalf("expected one PureDB rebuild for each pause and resume, got %d", builds)
	}
	if !strings.Contains(string(activeA.Data), lineA) || !strings.Contains(string(activeA.Data), lineB) {
		t.Fatal("other account rows were not preserved")
	}
}

func TestPureFTPHashParserAndCASRejectUnsupportedOrChangedData(t *testing.T) {
	_, _, name, originalHash, line := pureFTPStateFixture(t)
	other := strings.Join([]string{"other", "$6$other$hash", "1000", "1000", "", "/srv/other", "", "", "", "", "", "", "", "", "", "", "", ""}, ":")
	input := []byte(line + "\n" + other + "\n")
	updated, fieldsHash, err := pureFTPReplaceHash(input, name, originalHash, "$6$new$hash")
	if err != nil {
		t.Fatal(err)
	}
	_, _, actualHash, actualFields, err := pureFTPAccountFields(updated, name)
	if err != nil || actualHash != "$6$new$hash" || actualFields != fieldsHash {
		t.Fatalf("password-only rewrite changed another account field: hash=%q fingerprint=%q err=%v", actualHash, actualFields, err)
	}
	if _, _, err = pureFTPReplaceHash(updated, name, originalHash, "$6$stale$hash"); err == nil {
		t.Fatal("stale verifier CAS was accepted")
	}
	if _, _, _, _, err = pureFTPAccountFields([]byte("report_user:hash:too:few:fields\n"), name); err == nil {
		t.Fatal("unsupported account line format was accepted")
	}
}

func TestPureFTPPauseDatabaseFailureLeavesRecoverableJournal(t *testing.T) {
	e, instance, name, originalHash, _ := pureFTPStateFixture(t)
	pausedHash := "$6$paused$db-failure"
	makeHash := func(context.Context, Instance) (string, error) { return pausedHash, nil }
	if err := e.setPureFTPAccountEnabledUsing(context.Background(), instance, name, false, true, makeHash, func(context.Context, Instance) error {
		return errors.New("fixture mkdb failure")
	}); err == nil {
		t.Fatal("failed database rebuild reported success")
	}
	journal, err := e.loadPureFTPStateJournal(instance, name)
	if err != nil || journal == nil || journal.Phase != "pause_replaced" || journal.OriginalHash != originalHash {
		t.Fatalf("failed pause lost its recovery journal: %#v %v", journal, err)
	}
	if err = e.setPureFTPAccountEnabledUsing(context.Background(), instance, name, false, true, makeHash, func(context.Context, Instance) error { return nil }); err != nil {
		t.Fatal("known pending pause could not finish safely", err)
	}
	journal, err = e.loadPureFTPStateJournal(instance, name)
	if err != nil || journal == nil || journal.Phase != "paused" {
		t.Fatalf("database recovery did not settle the pause journal: %#v %v", journal, err)
	}
}

func TestFTPAccountStateBindingsAreDurableScopedAndRevisioned(t *testing.T) {
	_, _, manager := databaseManagerFixture(t)
	bindings := manager.ftpAccountStateBindings()
	if len(bindings) != 3 {
		t.Fatalf("expected pause, resume and inspect bindings, got %d", len(bindings))
	}
	for _, binding := range bindings {
		if binding.Action.Name == "ftp_account_state_inspect" {
			if binding.Action.Scope != "ftp:read" || !binding.Action.ReadOnly || binding.Action.Task {
				t.Fatalf("FTP state inspection must be read-only under ftp:read: %#v", binding.Action)
			}
			resources, err := binding.Resources(nil, core.Actor{}, []byte(`{"id":"ftp-production","name":"report_user"}`))
			if err != nil || len(resources) != 1 || resources[0] != InstanceResource("ftp-production") {
				t.Fatalf("FTP state inspect did not scope the managed instance: %#v %v", resources, err)
			}
			continue
		}
		if binding.Action.Scope != "ftp:manage" || binding.Action.ReadOnly || !binding.Action.Task || !binding.Action.Audit {
			t.Fatalf("FTP state action is missing durable audited manage scope: %#v", binding.Action)
		}
		var input FTPAccountStateInput
		if err := json.Unmarshal([]byte(`{"id":"ftp-production","name":"report_user","expected_revision":8}`), &input); err != nil || input.ExpectedRevision != 8 {
			t.Fatal("FTP state action revision payload could not be decoded", err)
		}
		resources, err := binding.Resources(nil, core.Actor{}, []byte(`{"id":"ftp-production","name":"report_user","expected_revision":8}`))
		if err != nil || len(resources) != 1 || resources[0] != InstanceResource("ftp-production") {
			t.Fatalf("FTP state action did not scope the managed instance: %#v %v", resources, err)
		}
	}
}
