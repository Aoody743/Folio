package plugins

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestInstallMutationGuardBlocksReceiptPendingCandidate(t *testing.T) {
	e, journal := installRecoveryFixture(t, "integration", false)
	if err := e.saveNextInstallJournal(context.Background(), &journal, "receipt_pending"); err != nil {
		t.Fatal(err)
	}
	if err := e.saveReceipt(journal.ID, journal.Instance); err != nil {
		t.Fatal(err)
	}
	candidate, err := e.loadInstallJournal(journal.ID)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := e.lockContext(context.Background(), journal.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = e.ensureInstallFinalized(context.Background(), journal.ID)
	unlock()
	if core.ErrorCode(err) != "operation_recovery_required" {
		t.Fatal("ordinary mutation was not gated until journal finalization", err)
	}
	// Queries and the recovery UI remain available while operational mutations
	// are gated. A read-only check must not change the candidate receipt.
	if _, err = e.get(context.Background(), journal.ID, false); err != nil {
		t.Fatal("receipt query was blocked by the mutation guard", err)
	}
	if _, err = e.control(context.Background(), ServiceInput{ID: journal.ID, Command: "check"}); err != nil {
		t.Fatal("read-only service check was blocked by the mutation guard", err)
	}
	view, err := e.installRecoveryInspect(context.Background(), journal.ID)
	if err != nil || !containsString(view.Available, "continue") {
		t.Fatal("recovery inspection/action was blocked by the mutation guard", view, err)
	}
	if _, err = e.control(context.Background(), ServiceInput{ID: journal.ID, Command: "start"}); core.ErrorCode(err) != "operation_recovery_required" {
		t.Fatal("service mutation was not gated until finalization", err)
	}
	if _, err = e.plan(context.Background(), PlanInput{Operation: "config", PluginID: "redis", InstanceID: "journal"}); core.ErrorCode(err) != "operation_recovery_required" {
		t.Fatal("configuration plan was not gated until finalization", err)
	}
	var saved Instance
	if err = e.load("receipts", journal.ID, &saved); err != nil || !reflect.DeepEqual(saved, candidate.Instance) {
		t.Fatal("denied mutations changed the committed candidate receipt", err)
	}
	stored, err := e.loadInstallJournal(journal.ID)
	if err != nil || stored.Phase != "receipt_pending" {
		t.Fatal("guard changed the pending journal", stored.Phase, err)
	}
	if _, err = e.installRecoveryAction(context.Background(), installRecoveryActionInput{ID: journal.ID, Command: "continue", ExpectedJournalRevision: journal.Revision}); err != nil {
		t.Fatal("the recovery action itself was blocked by the mutation guard", err)
	}
	if _, err = e.plan(context.Background(), PlanInput{Operation: "config", PluginID: "redis", InstanceID: "journal"}); err != nil {
		t.Fatal("a completed journal still blocked configuration planning", err)
	}
}

func TestInstallMutationGuardAllowsMissingAndCompletedJournals(t *testing.T) {
	e := testAgent(t)
	if err := e.ensureInstallFinalized(context.Background(), "redis-absent"); err != nil {
		t.Fatal("a missing journal blocked ordinary operations", err)
	}

	e, journal := installRecoveryFixture(t, "integration", false)
	if err := e.saveNextInstallJournal(context.Background(), &journal, "receipt_pending"); err != nil {
		t.Fatal(err)
	}
	if err := e.saveReceipt(journal.ID, journal.Instance); err != nil {
		t.Fatal(err)
	}
	if err := e.saveNextInstallJournal(context.Background(), &journal, "completed"); err != nil {
		t.Fatal(err)
	}

	// A completed installation permits normal receipt evolution afterward.
	updated := journal.Instance
	updated.Status = "stopped"
	updated.Revision++
	updated.UpdatedAt = time.Now().UTC()
	if err := e.save("receipts", updated.ID, updated); err != nil {
		t.Fatal(err)
	}
	unlock, err := e.lockContext(context.Background(), journal.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = e.ensureInstallFinalized(context.Background(), journal.ID)
	unlock()
	if err != nil {
		t.Fatal("a completed journal blocked ordinary receipt evolution", err)
	}
}
