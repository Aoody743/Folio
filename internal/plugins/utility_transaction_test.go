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

func utilityTransactionFixture(t *testing.T, pluginID string) (*PrivilegedExecutor, Instance) {
	t.Helper()
	e := testAgent(t)
	label := "utility-transaction"
	paths := e.instancePaths(pluginID, label)
	for _, dir := range []string{paths.ConfigDir, paths.DataDir, paths.RunDir, paths.LogDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	id := pluginID + "-utility-transaction"
	instance := Instance{
		ID: id, PluginID: pluginID, InstanceID: label, Revision: 1, Status: "stopped",
		Paths: paths, Config: map[string]any{"bind": "127.0.0.1", "port": 18873},
		Ownership: "managed",
		Service:   ServiceProfile{ID: id, PluginID: pluginID, InstanceID: label, Unit: "powerlinux-plugin-" + id + ".service", ConfigPath: paths.ConfigDir, DataPath: paths.DataDir, Ownership: "managed", Kind: "daemon", Program: "/usr/bin/rsync"},
	}
	if err := e.save("receipts", id, instance); err != nil {
		t.Fatal("save utility fixture receipt", err)
	}
	return e, instance
}

func utilityInputJSON(t *testing.T, input UtilityInput) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRsyncUtilityMutationRollsBackApplyAndPostRenameMetadataFailures(t *testing.T) {
	ctx := context.Background()
	e, instance := utilityTransactionFixture(t, "rsyncd")
	secretPath := filepath.Join(instance.Paths.ConfigDir, "rsync.secrets")
	configPath := filepath.Join(instance.Paths.ConfigDir, "rsyncd.conf")
	oldSecret := []byte("prior-user:prior-secret\n")
	oldConfig := []byte("prior rsync configuration\n")
	if err := os.WriteFile(secretPath, oldSecret, 0440); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, oldConfig, 0640); err != nil {
		t.Fatal(err)
	}
	input := UtilityInput{ID: instance.ID, Command: "create", Name: "backup", ExpectedRevision: 1, Password: "long-enough-rsync-password"}
	applyCalls := 0
	e.utilityApplyHook = func(_ context.Context, got Instance, candidate utilityRecord, running bool) error {
		applyCalls++
		if got.ID != instance.ID || running {
			t.Fatalf("stopped rsync runtime state was not preserved: id=%q running=%v", got.ID, running)
		}
		if applyCalls == 1 {
			if _, ok := candidate.Items[input.Name]; !ok {
				t.Fatal("candidate record was not rendered before reload")
			}
			visible, err := e.utilityRecord(instance)
			if err != nil || visible.Revision != 1 || len(visible.Items) != 0 {
				t.Fatalf("candidate metadata became visible before apply succeeded: %+v, %v", visible, err)
			}
			return failure("test_reload_failed", "injected utility reload failure", 502)
		}
		if candidate.Revision != 1 || len(candidate.Items) != 0 {
			t.Fatalf("rollback did not reapply the original record: %+v", candidate)
		}
		return nil
	}
	if _, err := e.utility(ctx, utilityInputJSON(t, input)); core.ErrorCode(err) != "test_reload_failed" {
		t.Fatalf("late utility apply failure was not returned after rollback: %v", err)
	}
	if applyCalls != 2 {
		t.Fatalf("prior stopped runtime was not restored after failed apply: apply calls=%d", applyCalls)
	}
	if got, err := os.ReadFile(secretPath); err != nil || string(got) != string(oldSecret) {
		t.Fatalf("rsync secrets file was not restored exactly: %q, %v", got, err)
	}
	if got, err := os.ReadFile(configPath); err != nil || string(got) != string(oldConfig) {
		t.Fatalf("rsync config file was not restored exactly: %q, %v", got, err)
	}
	if record, err := e.utilityRecord(instance); err != nil || record.Revision != 1 || len(record.Items) != 0 {
		t.Fatalf("failed utility change left candidate metadata behind: %+v, %v", record, err)
	}
	if _, err := os.Stat(filepath.Join(instance.Paths.DataDir, "modules", input.Name)); !os.IsNotExist(err) {
		t.Fatalf("failed create left its managed data directory behind: %v", err)
	}

	e.utilityApplyHook = nil
	result, err := e.utility(ctx, utilityInputJSON(t, input))
	if err != nil {
		t.Fatal("stopped rsync configuration was not committed", err)
	}
	if result.(map[string]any)["revision"] != int64(2) {
		t.Fatalf("successful utility mutation returned an unexpected revision: %#v", result)
	}
	record, err := e.utilityRecord(instance)
	if err != nil || record.Revision != 2 || record.Items[input.Name].Password != input.Password {
		t.Fatalf("successful utility mutation did not commit its record: %+v, %v", record, err)
	}
	secretAfterCreate, err := os.ReadFile(secretPath)
	if err != nil || !strings.Contains(string(secretAfterCreate), input.Password) {
		t.Fatalf("rsync did not render the authorized account secret: %q, %v", secretAfterCreate, err)
	}

	privateRecordPath := filepath.Join(e.config.StateDir, "private", utilityStateID(instance)+".json")
	oldPrivateBytes, err := os.ReadFile(privateRecordPath)
	if err != nil {
		t.Fatal(err)
	}
	updatedInput := UtilityInput{ID: instance.ID, Command: "update", Name: input.Name, ExpectedRevision: 2, Password: "replacement-rsync-password"}
	e.utilitySaveHook = func(got Instance, candidate utilityRecord) error {
		if err := e.save("private", utilityStateID(got), candidate); err != nil {
			return err
		}
		return errors.New("injected private-record directory sync failure")
	}
	if _, err = e.utility(ctx, utilityInputJSON(t, updatedInput)); err == nil || core.ErrorCode(err) == "operation_recovery_required" {
		t.Fatalf("post-rename metadata failure was not rolled back cleanly: %v", err)
	}
	e.utilitySaveHook = nil
	if got, err := os.ReadFile(privateRecordPath); err != nil || string(got) != string(oldPrivateBytes) {
		t.Fatalf("failed metadata commit did not restore the exact prior private record: bytes_equal=%v err=%v", string(got) == string(oldPrivateBytes), err)
	}
	if got, err := os.ReadFile(secretPath); err != nil || string(got) != string(secretAfterCreate) {
		t.Fatalf("failed metadata commit left candidate rsync credentials active: %q, %v", got, err)
	}
	if record, err = e.utilityRecord(instance); err != nil || record.Revision != 2 || record.Items[input.Name].Password != input.Password {
		t.Fatalf("failed metadata commit changed the protected utility record: %+v, %v", record, err)
	}
}

func TestSupervisorProcessStateParserReadsOnlyNamedState(t *testing.T) {
	response := `<?xml version="1.0"?><methodResponse><params><param><value><struct><member><name>name</name><value><string>worker</string></value></member><member><name>statename</name><value><string>RUNNING</string></value></member></struct></value></param></params></methodResponse>`
	state, err := supervisorProcessState(response)
	if err != nil || state != "RUNNING" {
		t.Fatalf("Supervisor RUNNING process response was not parsed: state=%q err=%v", state, err)
	}
	for _, raw := range []string{"<methodResponse/>", "<fault><string>missing</string></fault>"} {
		if _, err = supervisorProcessState(raw); err == nil {
			t.Fatalf("response without a statename was accepted: %s", raw)
		}
	}
}

func TestUtilityDataDirectoryCreationCannotEscapeManagedRoot(t *testing.T) {
	for _, inRoot := range []bool{false, true} {
		name := "external"
		if inRoot {
			name = "internal-sibling"
		}
		t.Run(name, func(t *testing.T) {
			e, instance := utilityTransactionFixture(t, "rsyncd")
			target := t.TempDir()
			if inRoot {
				target = filepath.Join(instance.Paths.DataDir, "sibling")
				if err := os.MkdirAll(target, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, filepath.Join(instance.Paths.DataDir, "modules")); err != nil {
				t.Fatal(err)
			}
			err := e.ensureUtilityDataDirectory(instance, filepath.Join("modules", "outside"))
			if err == nil {
				t.Fatal("utility data directory creation followed an unexpected symbolic link")
			}
			if _, err = os.Stat(filepath.Join(target, "outside")); !os.IsNotExist(err) {
				t.Fatalf("utility data path followed a symbolic link: %v", err)
			}
		})
	}
}

func TestUtilityCreateDoesNotAdoptOrRemoveUnknownDataDirectory(t *testing.T) {
	for _, pluginID := range []string{"rsyncd", "supervisor"} {
		t.Run(pluginID, func(t *testing.T) {
			e, instance := utilityTransactionFixture(t, pluginID)
			name := "worker"
			relative := filepath.Join("modules", name)
			if pluginID == "supervisor" {
				relative = filepath.Join("programs", name)
				e.config.Programs = map[string]TrustedProgram{name: {Program: "/usr/bin/true"}}
			}
			dataDirectory := filepath.Join(instance.Paths.DataDir, relative)
			if err := os.MkdirAll(dataDirectory, 0700); err != nil {
				t.Fatal(err)
			}
			input := UtilityInput{ID: instance.ID, Command: "create", Name: name, ExpectedRevision: 1}
			if pluginID == "rsyncd" {
				input.Password = "long-enough-rsync-password"
			} else {
				input.ProgramID = name
			}
			if _, err := e.utility(context.Background(), utilityInputJSON(t, input)); core.ErrorCode(err) != "utility_data_path_conflict" {
				t.Fatalf("create adopted a data directory without protected metadata: %v", err)
			}
			if info, err := os.Stat(dataDirectory); err != nil || !info.IsDir() {
				t.Fatalf("failed create removed an unowned pre-existing data directory: %v", err)
			}
			if record, err := e.utilityRecord(instance); err != nil || record.Revision != 1 || len(record.Items) != 0 {
				t.Fatalf("failed create changed the protected utility record: %+v, %v", record, err)
			}
		})
	}
}

func TestPureFTPUtilityApplyAndLateMetadataFailuresRestoreFilesDirectoryAndRecord(t *testing.T) {
	ctx := context.Background()
	e, instance := utilityTransactionFixture(t, "pureftp")
	passwdPath := filepath.Join(instance.Paths.ConfigDir, "pureftpd.passwd")
	pdbPath := filepath.Join(instance.Paths.ConfigDir, "pureftpd.pdb")
	oldPasswd := []byte("existing virtual users\n")
	oldPDB := []byte("existing compiled database\n")
	if err := os.WriteFile(passwdPath, oldPasswd, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pdbPath, oldPDB, 0600); err != nil {
		t.Fatal(err)
	}
	input := UtilityInput{ID: instance.ID, Command: "create", Name: "ftpuser", ExpectedRevision: 1, Password: "long-enough-ftp-password"}
	e.utilityApplyHook = func(_ context.Context, got Instance, candidate utilityRecord, _ bool) error {
		if got.ID != instance.ID || candidate.Revision != 2 {
			t.Fatalf("unexpected PureFTP candidate: id=%q record=%+v", got.ID, candidate)
		}
		if err := os.WriteFile(passwdPath, []byte("partial candidate user list\n"), 0600); err != nil {
			return err
		}
		if err := os.WriteFile(pdbPath, []byte("partial compiled database\n"), 0600); err != nil {
			return err
		}
		return failure("test_pureftp_apply_failed", "injected mkdb failure", 502)
	}
	if _, err := e.utility(ctx, utilityInputJSON(t, input)); core.ErrorCode(err) != "test_pureftp_apply_failed" {
		t.Fatalf("late PureFTP apply failure was not returned after rollback: %v", err)
	}
	for path, expected := range map[string][]byte{passwdPath: oldPasswd, pdbPath: oldPDB} {
		if got, err := os.ReadFile(path); err != nil || string(got) != string(expected) {
			t.Fatalf("failed PureFTP change did not restore %s: %q, %v", path, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(instance.Paths.DataDir, "users", input.Name)); !os.IsNotExist(err) {
		t.Fatalf("failed PureFTP create left a newly created account directory: %v", err)
	}
	if record, err := e.utilityRecord(instance); err != nil || record.Revision != 1 || len(record.Items) != 0 {
		t.Fatalf("failed PureFTP operation changed protected metadata: %+v, %v", record, err)
	}

	e.utilityApplyHook = func(_ context.Context, got Instance, candidate utilityRecord, _ bool) error {
		if got.ID != instance.ID || candidate.Revision != 2 {
			t.Fatalf("unexpected PureFTP candidate: id=%q record=%+v", got.ID, candidate)
		}
		if err := e.ensureUtilityDataDirectory(instance, filepath.Join("users", input.Name)); err != nil {
			return err
		}
		if err := os.WriteFile(passwdPath, []byte("committed virtual users\n"), 0600); err != nil {
			return err
		}
		return os.WriteFile(pdbPath, []byte("committed compiled database\n"), 0600)
	}
	if _, err := e.utility(ctx, utilityInputJSON(t, input)); err != nil {
		t.Fatalf("successful PureFTP transaction did not commit: %v", err)
	}
	oldPrivateBytes, err := os.ReadFile(filepath.Join(e.config.StateDir, "private", utilityStateID(instance)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	oldDirectory, err := snapshotUtilityDataDirectory(instance, filepath.Join("users", input.Name))
	if err != nil || !oldDirectory.Exists {
		t.Fatalf("successful PureFTP create did not establish its managed directory: %+v, %v", oldDirectory, err)
	}
	oldPasswd, err = os.ReadFile(passwdPath)
	if err != nil {
		t.Fatal(err)
	}
	oldPDB, err = os.ReadFile(pdbPath)
	if err != nil {
		t.Fatal(err)
	}
	update := UtilityInput{ID: instance.ID, Command: "update", Name: input.Name, ExpectedRevision: 2, ReadOnly: true}
	e.utilityApplyHook = func(_ context.Context, got Instance, candidate utilityRecord, _ bool) error {
		if got.ID != instance.ID || candidate.Revision != 3 {
			t.Fatalf("unexpected PureFTP update candidate: id=%q record=%+v", got.ID, candidate)
		}
		if err := e.setUtilityDataDirectory(instance, filepath.Join("users", input.Name), 0, int(e.gid), 0550); err != nil {
			return err
		}
		return os.WriteFile(passwdPath, []byte("candidate update\n"), 0600)
	}
	e.utilitySaveHook = func(got Instance, candidate utilityRecord) error {
		if err := e.save("private", utilityStateID(got), candidate); err != nil {
			return err
		}
		return errors.New("injected private metadata directory-sync failure")
	}
	if _, err = e.utility(ctx, utilityInputJSON(t, update)); err == nil || core.ErrorCode(err) == "operation_recovery_required" {
		t.Fatalf("post-rename PureFTP metadata failure was not rolled back cleanly: %v", err)
	}
	e.utilitySaveHook = nil
	if got, err := os.ReadFile(filepath.Join(e.config.StateDir, "private", utilityStateID(instance)+".json")); err != nil || string(got) != string(oldPrivateBytes) {
		t.Fatalf("failed PureFTP metadata commit did not restore the exact record: bytes_equal=%v err=%v", string(got) == string(oldPrivateBytes), err)
	}
	for path, expected := range map[string][]byte{passwdPath: oldPasswd, pdbPath: oldPDB} {
		if got, err := os.ReadFile(path); err != nil || string(got) != string(expected) {
			t.Fatalf("failed PureFTP metadata commit did not restore %s: %q, %v", path, got, err)
		}
	}
	if got, err := snapshotUtilityDataDirectory(instance, filepath.Join("users", input.Name)); err != nil || got != oldDirectory {
		t.Fatalf("failed PureFTP metadata commit did not restore data directory ownership/mode: got=%+v want=%+v err=%v", got, oldDirectory, err)
	}
}
