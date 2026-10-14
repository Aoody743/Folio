package plugins

import (
	"context"

	"github.com/Aoody743/PowerLinux/internal/core"
)

// ensureInstallFinalized prevents ordinary mutations from advancing a receipt
// while an install journal still needs recovery. Call it only after acquiring
// the shared per-instance lock. Read-only operations and recovery operations
// must not use this guard.
func (e *PrivilegedExecutor) ensureInstallFinalized(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	journal, err := e.loadInstallJournal(id)
	if core.ErrorCode(err) == "not_found" {
		return nil
	}
	if err != nil {
		return err
	}
	if journal.Phase != "completed" {
		return failure("operation_recovery_required", "A protected install is unfinished; inspect and recover it before changing this instance", 409)
	}
	return nil
}
