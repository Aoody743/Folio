package plugins

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

const ftpAccountPauseDenyList = "0.0.0.0/0,::/0"

type FTPAccountStateInput struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	ExpectedRevision int64  `json:"expected_revision"`
}

type FTPAccountStateView struct {
	Name     string `json:"name"`
	ReadOnly bool   `json:"read_only"`
	Enabled  bool   `json:"enabled"`
}

func ftpAccountEnabled(account utilityAccount) bool {
	// Records written before FTP account state existed have no ftp_enabled
	// field. Preserve their historical active behavior.
	return account.FTPEnabled == nil || *account.FTPEnabled
}

func ftpAccountEnabledValue(enabled bool) *bool { return &enabled }

func ftpAccountStateViews(record utilityRecord) []FTPAccountStateView {
	names := make([]string, 0, len(record.Items))
	for name := range record.Items {
		names = append(names, name)
	}
	sort.Strings(names)
	items := make([]FTPAccountStateView, 0, len(names))
	for _, name := range names {
		account := record.Items[name]
		items = append(items, FTPAccountStateView{Name: account.Name, ReadOnly: account.ReadOnly, Enabled: ftpAccountEnabled(account)})
	}
	return items
}

func pureFTPAccountStateArgs(instance Instance, name string, enabled bool) []string {
	denyList := ""
	if !enabled {
		denyList = ftpAccountPauseDenyList
	}
	return []string{"usermod", name, "-f", filepath.Join(instance.Paths.ConfigDir, "pureftpd.passwd"), "-R", denyList}
}

func (e *PrivilegedExecutor) setPureFTPAccountEnabled(ctx context.Context, instance Instance, name string, enabled bool, rebuild bool) error {
	if instance.PluginID != "pureftp" || !utilityName.MatchString(name) {
		return failure("runtime_type_mismatch", "FTP account state requires a managed virtual account", 412)
	}
	if _, err := runFixed(ctx, "/usr/bin/pure-pw", pureFTPAccountStateArgs(instance, name, enabled), 15*time.Second, nil); err != nil {
		return failure("ftp_account_failed", "PureFTP could not update the virtual account access state", 502)
	}
	if !rebuild {
		return nil
	}
	if _, err := runFixed(ctx, "/usr/bin/pure-pw", []string{"mkdb", filepath.Join(instance.Paths.ConfigDir, "pureftpd.pdb"), "-f", filepath.Join(instance.Paths.ConfigDir, "pureftpd.passwd")}, 15*time.Second, nil); err != nil {
		return failure("ftp_account_failed", "PureFTP could not rebuild its virtual account database", 502)
	}
	return nil
}

func (e *PrivilegedExecutor) reconcilePureFTPAccountStates(ctx context.Context, instance Instance) error {
	record, err := e.utilityRecord(instance)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(record.Items))
	for name, account := range record.Items {
		if !ftpAccountEnabled(account) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if err := e.setPureFTPAccountEnabled(ctx, instance, name, false, false); err != nil {
			return err
		}
	}
	if len(names) == 0 {
		return nil
	}
	if _, err := runFixed(ctx, "/usr/bin/pure-pw", []string{"mkdb", filepath.Join(instance.Paths.ConfigDir, "pureftpd.pdb"), "-f", filepath.Join(instance.Paths.ConfigDir, "pureftpd.passwd")}, 15*time.Second, nil); err != nil {
		return failure("ftp_account_failed", "PureFTP could not rebuild its paused account database", 502)
	}
	return nil
}

func (m *Manager) ftpAccountStateBindings() []core.Binding {
	resources := idResource
	bindings := make([]core.Binding, 0, 2)
	for _, action := range []struct {
		name    string
		command string
		enabled bool
	}{
		{name: "ftp_pause", command: "pause", enabled: false},
		{name: "ftp_resume", command: "resume", enabled: true},
	} {
		current := action
		binding := m.binding(current.name, "Queue a durable per-account FTP pause or resume without changing its credential or directory", "ftp:manage", false, obj(map[string]any{
			"id": idSchema(), "name": map[string]any{"type": "string", "pattern": utilityName.String()}, "expected_revision": revisionSchema(),
		}, "id", "name", "expected_revision"), resources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			var input FTPAccountStateInput
			if err := parse(raw, &input); err != nil {
				return core.ActionResult{}, err
			}
			if !validInstanceID(input.ID) || !utilityName.MatchString(input.Name) || input.ExpectedRevision < 1 {
				return core.ActionResult{}, failure("invalid_input", "A valid FTP account and expected revision are required", 400)
			}
			var instance Instance
			idRaw, _ := json.Marshal(IDInput{ID: input.ID})
			if err := m.agentJSON(ctx, "plugin_get", idRaw, &instance); err != nil {
				return core.ActionResult{}, err
			}
			if instance.PluginID != "pureftp" {
				return core.ActionResult{}, failure("runtime_type_mismatch", "FTP account state requires a managed PureFTP instance", 412)
			}
			jobInput, _ := json.Marshal(UtilityInput{ID: input.ID, Name: input.Name, Command: current.command, ExpectedRevision: input.ExpectedRevision})
			resource := InstanceResource(input.ID)
			job, err := m.deps.Tasks.Submit(ctx, actor, "utility_action", jobInput, []string{resource})
			if err != nil {
				return core.ActionResult{}, err
			}
			taskID, _ := job["id"].(string)
			if taskID == "" {
				taskID, _ = job["task_id"].(string)
			}
			return core.ActionResult{Data: job, TaskID: taskID, Resources: []string{resource}, AuditSummary: map[string]any{"instance_id": input.ID, "account": input.Name, "command": current.command, "enabled": current.enabled}}, nil
		})
		binding.Action.Task = true
		binding.Action.Audit = true
		bindings = append(bindings, binding)
	}
	return bindings
}
