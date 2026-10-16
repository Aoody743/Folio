package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestPendingPHPExtensionIntentWithoutReceiptBlocksSameIDMutation(t *testing.T) {
	e, instance, _ := phpExtensionStartupFixture(t)
	receiptPath := filepath.Join(e.config.StateDir, "receipts", instance.ID+".json")
	if err := os.Remove(receiptPath); err != nil {
		t.Fatal(err)
	}

	// Lifecycle and other instance mutations enter through this shared guard.
	// A protected pending journal without its current managed receipt cannot
	// prove ownership, so it must fail closed instead of permitting ID reuse.
	if err := e.ensureInstallFinalized(context.Background(), instance.ID); core.ErrorCode(err) != "extension_state_invalid" {
		t.Fatalf("same-ID mutation was not blocked by an orphan pending extension journal: %v", err)
	}
}
