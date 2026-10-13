package sites

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// This checks the real reviewed CLI's provider surface without provider
// credentials or an ACME account. It is not DNS issuance acceptance.
func TestReviewedLegoActualCredentialSurface(t *testing.T) {
	program := os.Getenv("POWERLINUX_QA_LEGO")
	if program == "" {
		t.Skip("Set POWERLINUX_QA_LEGO to the real checksum-verified v4.35.2 CLI fixture")
	}
	for _, adapter := range dnsAdapters() {
		if !adapter.Available || adapter.Driver != "lego" {
			continue
		}
		t.Run(adapter.ID, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, program, "dnshelp", "--code", adapter.ClientProvider)
			command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatal("Reviewed CLI does not have this provider", adapter.ClientProvider, err, string(output))
			}
			for field, variable := range adapter.environment {
				if variable != "" && !strings.Contains(string(output), variable) {
					t.Errorf("Legacy field %s maps to an environment variable missing from actual reviewed CLI: %s", field, variable)
				}
			}
		})
	}
}
