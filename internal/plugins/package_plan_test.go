package plugins

import (
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestAPTTransactionPlanningIncludesExactDependenciesAndRejectsRemoval(t *testing.T) {
	output := "Reading package lists...\nInst php8.4-cli [8.4.23-1] (8.4.24-1 Debian:stable [" + runtime.GOARCH + "])\nInst libargon2-1 (0~20190702+dfsg-4 Debian:stable [" + runtime.GOARCH + "])\nConf libargon2-1 (0~20190702+dfsg-4 Debian:stable [" + runtime.GOARCH + "])\nConf php8.4-cli (8.4.24-1 Debian:stable [" + runtime.GOARCH + "])\n"
	changes, err := parseAPTChanges(output)
	if err != nil || len(changes) != 2 || changes["php8.4-cli"] != "8.4.24-1" || changes["libargon2-1"] != "0~20190702+dfsg-4" {
		t.Fatal("Exact dependency planning failed", changes, err)
	}
	for _, input := range []string{"Remv production-database [1.0]", "Purg production-database [1.0]", "Inst php8.4-cli (8.4.24)\nConf php8.4-cli (8.4.25)", "Inst ../../secret (1.0)", "Inst foreign:i386 (1.0)", "Inst package-with-unknown-format"} {
		if _, err := parseAPTChanges(input); err == nil {
			t.Fatal("Unreviewable/removing package transaction accepted", input)
		}
	}
	var many strings.Builder
	for i := 0; i < 129; i++ {
		many.WriteString("Inst pkg" + strconv.Itoa(i) + " (1.0)\n")
	}
	if _, err := parseAPTChanges(many.String()); err == nil {
		t.Fatal("Unbounded dependency transaction accepted")
	}
}
func TestQueuedPluginActionsDeclareTasksOnlyWhenActuallyQueued(t *testing.T) {
	catalog, err := BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{catalog: catalog}
	for _, binding := range manager.Bindings() {
		name := binding.Action.Name
		if name == "software_apply" || name == "software_service_control" || name == "backup_ftp_transfer" || name == "php_extension_toggle" || name == "php_configure" || name == "docker_image_pull" || name == "docker_container_start" {
			if !binding.Action.Task {
				t.Fatal("Queued operation lacks task metadata", name)
			}
		}
		if name == "docker_container_create" || name == "ftp_create" || name == "backup_ftp_destination_register" || name == "software_plan" {
			if binding.Action.Task {
				t.Fatal("Synchronous operation falsely declares a task", name)
			}
		}
	}
	_, err = parseAPTChanges("Remv local-service [1.0]")
	if core.ErrorCode(err) != "package_removal_forbidden" {
		t.Fatal("Removal failure loses actionable code", err)
	}
}

func TestStartupPolicyOnlyRecognizesAnUnconditionalDenyAndRPMNamesAreCanonical(t *testing.T) {
	if !packageDenyPolicy.MatchString(packageStartupBody) {
		t.Fatal("Owned deny policy is not recognized")
	}
	for _, body := range []string{"#!/bin/sh\nexit 0\n", "#!/bin/sh\nif [ \"$1\" = production ]; then exit 0; fi\nexit 101\n", "#!/bin/sh\nexit 101\nrm -rf /", "#!/bin/sh\nexec /unreviewed-policy\n"} {
		if packageDenyPolicy.MatchString(body) {
			t.Fatal("Conditional or foreign policy treated as unconditional deny", body)
		}
	}
	for _, name := range []string{"../package.rpm", "package\nRSA/SHA256 Signature: OK.rpm", "--unreviewed.rpm", "package.rpm\x00"} {
		if rpmArtifactName.MatchString(name) {
			t.Fatal("Untrusted RPM filename can contaminate signature output", name)
		}
	}
	if !rpmArtifactName.MatchString("php84-php-fpm-8.4.24-1.el9.remi.x86_64.rpm") {
		t.Fatal("Canonical signed RPM filename rejected")
	}
}
