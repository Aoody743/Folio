package plugins

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestFTPAccountStateLegacyDefaultAndRedactedViews(t *testing.T) {
	legacy, err := json.Marshal(utilityRecord{Revision: 4, Items: map[string]utilityAccount{
		"legacy": {Name: "legacy", ReadOnly: true, Password: "private-legacy-password"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded utilityRecord
	if err := json.Unmarshal(legacy, &decoded); err != nil {
		t.Fatal(err)
	}
	if !ftpAccountEnabled(decoded.Items["legacy"]) {
		t.Fatal("a legacy FTP record without ftp_enabled must remain active")
	}
	paused := utilityRecord{Revision: 5, Items: map[string]utilityAccount{
		"paused": {Name: "paused", FTPEnabled: ftpAccountEnabledValue(false), Password: "private-paused-password"},
	}}
	items := ftpAccountStateViews(paused)
	if len(items) != 1 || items[0].Name != "paused" || items[0].Enabled || items[0].ReadOnly {
		t.Fatalf("paused account view mismatch: %#v", items)
	}
	encoded, err := json.Marshal(map[string]any{"revision": paused.Revision, "items": ftpAccountStateViews(decoded), "accounts": items})
	if err != nil {
		t.Fatal(err)
	}
	public := string(encoded)
	if strings.Contains(public, "private-legacy-password") || strings.Contains(public, "private-paused-password") {
		t.Fatal("FTP account list/status view exposed a credential")
	}
	if !strings.Contains(public, `"enabled":true`) || !strings.Contains(public, `"enabled":false`) {
		t.Fatalf("views did not report the old-active and paused states: %s", public)
	}
}

func TestPureFTPAccountPauseResumeUsesScopedDenyListWithoutCredentials(t *testing.T) {
	instance := Instance{PluginID: "pureftp", Paths: Paths{ConfigDir: "/srv/powerlinux/ftp/config"}}
	paused := pureFTPAccountStateArgs(instance, "report_user", false)
	wantPaused := []string{"usermod", "report_user", "-f", "/srv/powerlinux/ftp/config/pureftpd.passwd", "-R", ftpAccountPauseDenyList}
	if !reflect.DeepEqual(paused, wantPaused) {
		t.Fatalf("pause command mismatch: %#v", paused)
	}
	resumed := pureFTPAccountStateArgs(instance, "report_user", true)
	wantResumed := []string{"usermod", "report_user", "-f", "/srv/powerlinux/ftp/config/pureftpd.passwd", "-R", ""}
	if !reflect.DeepEqual(resumed, wantResumed) {
		t.Fatalf("resume command mismatch: %#v", resumed)
	}
	for _, arg := range append(paused, resumed...) {
		if strings.Contains(arg, "password") || arg == "private-secret" {
			t.Fatal("pause/resume command carried a credential")
		}
	}
}

func TestFTPAccountStateBindingsAreDurableScopedAndRevisioned(t *testing.T) {
	_, _, manager := databaseManagerFixture(t)
	bindings := manager.ftpAccountStateBindings()
	if len(bindings) != 2 {
		t.Fatalf("expected pause and resume bindings, got %d", len(bindings))
	}
	for _, binding := range bindings {
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
