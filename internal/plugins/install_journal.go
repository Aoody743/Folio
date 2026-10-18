package plugins

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

const maxInstallJournalBytes = 512 << 10

var installNoncePattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type installRecoveryInspectInput struct {
	ID string `json:"id"`
}

type installRecoveryActionInput struct {
	ID                      string `json:"id"`
	Command                 string `json:"command"`
	ExpectedJournalRevision int64  `json:"expected_journal_revision"`
}

type installRecoveryView struct {
	ID             string    `json:"id"`
	State          string    `json:"state"`
	Checkpoint     string    `json:"checkpoint,omitempty"`
	Revision       int64     `json:"revision,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
	Available      []string  `json:"available_actions"`
	Reason         string    `json:"reason,omitempty"`
	ReceiptPresent bool      `json:"receipt_present"`
}

type installJournal struct {
	Version              int                                   `json:"version"`
	ID                   string                                `json:"id"`
	Revision             int64                                 `json:"revision"`
	Phase                string                                `json:"phase"`
	Plan                 Plan                                  `json:"plan"`
	Instance             Instance                              `json:"instance"`
	Previous             *Instance                             `json:"previous_receipt,omitempty"`
	Nonce                string                                `json:"nonce"`
	IdempotencySHA       string                                `json:"idempotency_sha256"`
	DesiredBoot          *bool                                 `json:"desired_boot,omitempty"`
	DistributionServices map[string]distributionNginxUnitState `json:"distribution_services_before_packages,omitempty"`
	ProgramProof         *fileProof                            `json:"program_proof,omitempty"`
	ConfigSHA256         string                                `json:"config_sha256,omitempty"`
	UnitSHA256           string                                `json:"unit_sha256,omitempty"`
	FailureCode          string                                `json:"failure_code,omitempty"`
	CreatedAt            time.Time                             `json:"created_at"`
	UpdatedAt            time.Time                             `json:"updated_at"`
}

type fileProof struct {
	Path         string `json:"path"`
	ResolvedPath string `json:"resolved_path"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	Mode         uint32 `json:"mode"`
	UID          uint32 `json:"uid"`
	GID          uint32 `json:"gid"`
}

func installJournalKey(id string) string { return "install_" + id }

func newInstallNonce() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func (e *PrivilegedExecutor) newInstallJournal(ctx context.Context, plan Plan, input ApplyInput, instance Instance, previous *Instance, desiredBoot *bool, expectedJournalRevision int64) (installJournal, error) {
	var journal installJournal
	nonce, err := newInstallNonce()
	if err != nil {
		return journal, err
	}
	keyHash := sha256.Sum256([]byte(input.IdempotencyKey))
	now := time.Now().UTC()
	journal = installJournal{
		Version: 1, ID: instance.ID, Phase: "prepared", Plan: plan, Instance: instance,
		Previous: previous, Nonce: nonce, IdempotencySHA: hex.EncodeToString(keyHash[:]), DesiredBoot: desiredBoot,
		CreatedAt: now, UpdatedAt: now,
	}
	if err = e.saveInstallJournal(ctx, &journal, expectedJournalRevision); err != nil {
		return installJournal{}, err
	}
	return journal, nil
}

func (e *PrivilegedExecutor) recordInstallCandidate(ctx context.Context, journal *installJournal, instance Instance) error {
	if journal == nil || journal.ID != instance.ID {
		return failure("install_journal_invalid", "Install candidate does not match its journal", 412)
	}
	configHash, err := hashManagedTree(instance.Paths.ConfigDir)
	if err != nil {
		return err
	}
	var program *fileProof
	if instance.Service.Program != "" {
		proof, err := hashManagedExecutable(instance.Service.Program)
		if err != nil {
			return err
		}
		program = &proof
	}
	journal.Instance = instance
	journal.ConfigSHA256 = configHash
	journal.ProgramProof = program
	return e.saveNextInstallJournal(ctx, journal, "configured")
}

func (e *PrivilegedExecutor) writeInstallUnit(ctx context.Context, instance *Instance, nonce string) error {
	if instance == nil || !e.config.UseSystemd || instance.Service.Program == "" || instance.Service.Kind == "cli" {
		return failure("install_journal_invalid", "Install journal unit write requires a managed systemd daemon", 412)
	}
	journal, err := e.loadInstallJournal(instance.ID)
	if err != nil {
		return err
	}
	if journal.Nonce != nonce || journal.Phase != "configure_started" {
		return failure("install_journal_invalid", "Install unit write is not bound to the prepared journal checkpoint", 412)
	}
	unit, err := systemdUnitBytes(e, *instance, nonce)
	if err != nil {
		return err
	}
	if err = e.checkSystemdIdentity(ctx, instance.Service.Unit, true); err != nil {
		return err
	}
	digest := sha256.Sum256(unit)
	proof, err := hashManagedExecutable(instance.Service.Program)
	if err != nil {
		return err
	}
	configHash, err := hashManagedTree(instance.Paths.ConfigDir)
	if err != nil {
		return err
	}
	unitSHA := hex.EncodeToString(digest[:])
	instance.Service.UnitSHA256 = unitSHA
	journal.Instance = *instance
	journal.ProgramProof = &proof
	journal.ConfigSHA256 = configHash
	journal.UnitSHA256 = unitSHA
	if err = e.saveNextInstallJournal(ctx, &journal, "unit_write_pending"); err != nil {
		return err
	}
	dir := e.systemdUnitDir()
	if e.installUnitCreateHook != nil {
		err = e.installUnitCreateHook(dir, instance.Service.Unit, unit)
	} else {
		err = createOnlyUnitFile(dir, instance.Service.Unit, unit)
	}
	if err != nil {
		return err
	}
	if e.systemdReloadHook != nil {
		err = e.systemdReloadHook(ctx)
	} else {
		_, err = runFixed(ctx, "/usr/bin/systemctl", []string{"daemon-reload"}, 20*time.Second, nil)
	}
	if err != nil {
		return err
	}
	journal.Instance = *instance
	return e.saveNextInstallJournal(ctx, &journal, "unit_written")
}

func (e *PrivilegedExecutor) installRecoveryLock(ctx context.Context, id string) (func(), error) {
	if !validInstanceID(id) {
		return nil, failure("invalid_input", "Invalid software instance ID", 400)
	}
	path := filepath.Join(e.config.StateDir, "locks", "install-"+id+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		_ = file.Close()
		return nil, failure("ownership_invalid", "Install recovery lock is unsafe", 412)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		_ = file.Close()
		return nil, failure("ownership_invalid", "Install recovery lock belongs to another Agent", 412)
	}
	for {
		if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return func() {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN && err != syscall.EINTR {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (e *PrivilegedExecutor) loadInstallJournal(id string) (installJournal, error) {
	var journal installJournal
	if !validInstanceID(id) {
		return journal, failure("invalid_input", "Invalid software instance ID", 400)
	}
	key := installJournalKey(id)
	path := filepath.Join(e.config.StateDir, "private", key+".json")
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		return journal, failure("not_found", "No protected install recovery journal exists", 404)
	}
	if err != nil {
		return journal, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxInstallJournalBytes+4096 || info.Mode().Perm()&0077 != 0 {
		return journal, failure("install_journal_invalid", "Protected install recovery journal permissions or size are invalid", 412)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return journal, failure("install_journal_invalid", "Protected install recovery journal belongs to another Agent", 412)
	}
	sealed, err := io.ReadAll(io.LimitReader(file, maxInstallJournalBytes+4097))
	if err != nil || len(sealed) > maxInstallJournalBytes+4096 {
		return journal, failure("install_journal_invalid", "Protected install recovery journal exceeds its size limit", 412)
	}
	plain, legacy, err := e.openPrivate(key, sealed)
	if err != nil || legacy {
		return journal, failure("install_journal_invalid", "Protected install recovery journal cannot be authenticated", 412)
	}
	defer clear(plain)
	decoder := json.NewDecoder(strings.NewReader(string(plain)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&journal); err != nil {
		return journal, failure("install_journal_invalid", "Protected install recovery journal is corrupt", 412)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return journal, failure("install_journal_invalid", "Protected install recovery journal has trailing data", 412)
	}
	if err = e.validateInstallJournal(journal, id); err != nil {
		return journal, err
	}
	return journal, nil
}

func (e *PrivilegedExecutor) validateInstallJournal(j installJournal, id string) error {
	if j.Version != 1 || j.ID != id || j.Revision < 1 || !installNoncePattern.MatchString(j.Nonce) {
		return failure("install_journal_invalid", "Protected install recovery journal metadata is invalid", 412)
	}
	if j.Plan.Operation != "install" || !validPlanID(j.Plan.ID) || j.Plan.InstanceID != id || j.Plan.PluginID != j.Instance.PluginID {
		return failure("install_journal_invalid", "Protected install recovery journal does not match its trusted plan identity", 412)
	}
	if j.Instance.ID != id || j.Instance.InstanceID == "" || j.Instance.Paths != e.instancePaths(j.Instance.PluginID, j.Instance.InstanceID) {
		return failure("install_journal_invalid", "Protected install recovery journal has invalid instance identity or paths", 412)
	}
	if j.Instance.Service.ID != id || j.Instance.Service.PluginID != j.Instance.PluginID || j.Instance.Service.InstanceID != j.Instance.InstanceID ||
		j.Instance.Service.Unit != "powerlinux-plugin-"+id+".service" || j.Instance.Service.ConfigPath != j.Instance.Paths.ConfigDir || j.Instance.Service.DataPath != j.Instance.Paths.DataDir {
		return failure("install_journal_invalid", "Protected install recovery journal has invalid service identity or paths", 412)
	}
	if j.Instance.Service.Ownership != "managed" || j.Instance.Ownership != "managed" {
		return failure("install_journal_invalid", "Protected install recovery journal does not prove managed ownership", 412)
	}
	if j.Instance.Revision != j.Plan.ExpectedRevision+1 {
		return failure("install_journal_invalid", "Protected install recovery journal revision does not match its trusted plan", 412)
	}
	if j.Instance.SourceVersion != j.Plan.Version.ID {
		return failure("install_journal_invalid", "Protected install recovery journal source version does not match its trusted plan", 412)
	}
	if !sameJSONConfig(j.Instance.Config, j.Plan.Config) {
		return failure("install_journal_invalid", "Protected install recovery journal does not match its trusted plan and instance", 412)
	}
	if !artifactHash.MatchString(j.IdempotencySHA) {
		return failure("install_journal_invalid", "Protected install recovery journal idempotency proof is invalid", 412)
	}
	if j.Plan.ExpectedRevision < 0 || len(j.Plan.Packages) > 128 || len(j.Instance.Packages) > 128 || len(j.Plan.Packages) != len(j.Instance.Packages) {
		return failure("install_journal_invalid", "Protected install recovery package pins are invalid", 412)
	}
	for index := range j.Plan.Packages {
		if j.Plan.Packages[index] != j.Instance.Packages[index] {
			return failure("install_journal_invalid", "Protected install recovery package pins disagree", 412)
		}
	}
	expectedResources := []string{InstanceResource(id)}
	if sourceUsesSystemPackages(j.Plan.Version.Source) {
		expectedResources = append(expectedResources, "host:packages")
	}
	if !slices.Equal(j.Plan.Resources, expectedResources) {
		return failure("install_journal_invalid", "Protected install recovery resources differ from the reviewed plan", 412)
	}
	if j.Plan.ExpectedRevision == 0 && j.Previous != nil || j.Plan.ExpectedRevision > 0 && (j.Previous == nil || j.Previous.ID != id || j.Previous.Revision != j.Plan.ExpectedRevision || j.Previous.Status != "uninstalled" || j.Previous.Ownership != "managed" || j.Previous.Paths != j.Instance.Paths) {
		return failure("install_journal_invalid", "Protected install recovery previous receipt is invalid", 412)
	}
	if !strings.HasPrefix(id, j.Plan.PluginID+"-") || !validID.MatchString(j.Instance.InstanceID) || j.Plan.ExpectedRevision != j.Instance.Revision-1 {
		return failure("install_journal_invalid", "Protected install recovery identity or revision is invalid", 412)
	}
	if j.Phase == "" || j.CreatedAt.IsZero() || j.UpdatedAt.IsZero() || len(j.FailureCode) > 80 {
		return failure("install_journal_invalid", "Protected install recovery journal checkpoint is invalid", 412)
	}
	if len(j.Instance.Service.Program) > 4096 || j.UnitSHA256 != "" && !artifactHash.MatchString(j.UnitSHA256) || j.ConfigSHA256 != "" && !artifactHash.MatchString(j.ConfigSHA256) {
		return failure("install_journal_invalid", "Protected install recovery proof has invalid paths or hashes", 412)
	}
	if j.UnitSHA256 != "" && j.Instance.Service.Kind == "daemon" && j.DesiredBoot == nil {
		return failure("install_journal_invalid", "Protected systemd install journal is missing its desired boot state", 412)
	}
	if j.ProgramProof != nil && (!filepath.IsAbs(j.ProgramProof.Path) || len(j.ProgramProof.Path) > 4096 || !filepath.IsAbs(j.ProgramProof.ResolvedPath) || len(j.ProgramProof.ResolvedPath) > 4096 || !artifactHash.MatchString(j.ProgramProof.SHA256) || j.ProgramProof.Size < 1 || j.ProgramProof.Size > 1<<30) {
		return failure("install_journal_invalid", "Protected install program proof is invalid", 412)
	}
	return nil
}

func sameJSONConfig(left, right map[string]any) bool {
	if len(left) == 0 && len(right) == 0 {
		return true
	}
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func (e *PrivilegedExecutor) saveInstallJournal(ctx context.Context, j *installJournal, expectedRevision int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if j == nil || !validInstanceID(j.ID) || expectedRevision < 0 {
		return failure("invalid_input", "Invalid install journal revision", 400)
	}
	current, err := e.loadInstallJournal(j.ID)
	if expectedRevision == 0 {
		if err == nil {
			return failure("revision_conflict", "Install recovery journal already exists", 412)
		}
		if core.ErrorCode(err) != "not_found" {
			return err
		}
	} else if err != nil {
		return err
	} else if current.Revision != expectedRevision {
		return failure("revision_conflict", "Install recovery journal changed; inspect it again before recovery", 412)
	}
	next := *j
	next.Revision = expectedRevision + 1
	next.UpdatedAt = time.Now().UTC()
	if err = e.validateInstallJournal(next, next.ID); err != nil {
		return err
	}
	if e.installJournalSaveHook != nil {
		if err = e.installJournalSaveHook(ctx, next, expectedRevision); err != nil {
			return err
		}
	}
	plain, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if len(plain) > maxInstallJournalBytes {
		return failure("install_journal_too_large", "Protected install recovery journal exceeds its size limit", 413)
	}
	sealed, err := e.sealPrivate(installJournalKey(next.ID), plain)
	clear(plain)
	if err != nil {
		return err
	}
	dir := filepath.Join(e.config.StateDir, "private")
	temp, err := os.CreateTemp(dir, ".install-journal-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err = temp.Chmod(0600); err == nil {
		_, err = temp.Write(sealed)
	}
	clear(sealed)
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.Rename(tempName, filepath.Join(dir, installJournalKey(next.ID)+".json")); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr = directory.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	*j = next
	return nil
}

func (e *PrivilegedExecutor) saveNextInstallJournal(ctx context.Context, j *installJournal, phase string) error {
	if j == nil || phase == "" {
		return failure("install_journal_invalid", "Invalid install recovery checkpoint", 412)
	}
	previous := j.Revision
	j.Phase = phase
	if err := e.saveInstallJournal(ctx, j, previous); err != nil {
		return err
	}
	return nil
}

func (e *PrivilegedExecutor) installRecoveryInspect(ctx context.Context, id string) (installRecoveryView, error) {
	view := installRecoveryView{ID: id, State: "journal_missing", Available: []string{}, Reason: "No protected install journal exists; automatic recovery is unavailable."}
	if !validInstanceID(id) {
		return view, failure("invalid_input", "Invalid software instance ID", 400)
	}
	j, err := e.loadInstallJournal(id)
	if err != nil {
		if core.ErrorCode(err) == "not_found" {
			if _, receiptErr := e.get(ctx, id, false); receiptErr == nil {
				view.ReceiptPresent = true
				view.State = "receipt_present"
				view.Reason = "A protected software receipt exists; no incomplete-install recovery action is available."
			} else if core.ErrorCode(receiptErr) != "not_found" {
				return view, receiptErr
			}
			return view, nil
		}
		return view, err
	}
	receiptState, receiptPresent, err := e.installJournalReceiptState(j)
	if err != nil {
		return view, err
	}
	view.ReceiptPresent = receiptPresent
	view.State = "in_progress"
	view.Checkpoint = j.Phase
	view.Revision = j.Revision
	view.UpdatedAt = j.UpdatedAt
	view.Reason = "The protected install journal proves this attempt; recovery is limited to its verified checkpoint."
	if j.Phase == "completed" {
		// Normal service/config/uninstall operations may advance or update the
		// receipt after installation. A completed journal grants no recovery and
		// must not describe that ordinary evolution as an interrupted install.
		current, currentErr := e.get(ctx, id, false)
		if currentErr == nil && current.Ownership == "managed" && current.Paths == j.Instance.Paths && current.Revision >= j.Instance.Revision {
			view.State = "completed"
			view.Reason = "The install receipt is committed."
		} else {
			view.State = "journal_conflict"
			view.Reason = "The completed journal has no current software receipt; automatic recovery is denied."
		}
		return view, nil
	}
	receiptConflict := receiptState == "conflict" || receiptState == "candidate" && j.Phase != "receipt_pending" && j.Phase != "completed"
	if receiptConflict {
		view.State = "journal_conflict"
		view.Reason = "The protected install journal and current receipt do not prove a safe recovery state."
		return view, nil
	}
	// A committed candidate needs only journal finalization. Otherwise every
	// advertised mutation requires current ownership proof, just like execution.
	proofValid := receiptState == "candidate"
	healthFailedContinue := false
	if !proofValid && (j.Phase == "health_verified" || j.Phase == "boot_verified" || j.Phase == "receipt_pending" || e.installStopPreserveAllowed(j)) {
		proofValid = e.verifyInstallJournalProofs(ctx, j) == nil
	}
	if proofValid && j.Phase == "health_failed" {
		_, healthErr := e.verifyHealthFailedInstallRecovery(ctx, j)
		healthFailedContinue = healthErr == nil
	}
	if (proofValid && (j.Phase == "health_verified" || j.Phase == "boot_verified" || j.Phase == "receipt_pending")) || healthFailedContinue {
		view.Available = append(view.Available, "continue")
	}
	if receiptState != "candidate" && e.installStopPreserveAllowed(j) && proofValid {
		view.Available = append(view.Available, "stop_preserve")
	} else if !proofValid {
		view.Reason = "The journal exists, but current ownership proofs do not permit a recovery action."
	} else if j.Phase == "health_failed" && proofValid && !healthFailedContinue {
		view.Reason = "The exact install proofs remain valid, but the running service is not currently proven healthy; it was preserved."
	} else if receiptState == "candidate" {
		view.Reason = "The install receipt is committed; continue only to finalize its journal checkpoint."
	}
	if j.Phase == "preserved_stopped" {
		view.State = "preserved_stopped"
		view.Available = []string{}
		view.Reason = "The verified owned service is stopped and disabled; its package, data, configuration, and runtime were preserved."
	}
	if j.Phase != "preserved_stopped" && j.Phase != "health_verified" && j.Phase != "boot_verified" && j.Phase != "receipt_pending" && !e.installStopPreserveAllowed(j) {
		view.State = "inspection_only"
		view.Reason = "This checkpoint has no verified running-service proof. Automatic recovery is unavailable; preserve existing state for operator review."
	}
	return view, nil
}

func (e *PrivilegedExecutor) installJournalReceiptState(j installJournal) (string, bool, error) {
	var existing Instance
	err := e.load("receipts", j.ID, &existing)
	if err == nil {
		if sameInstallReceipt(existing, j.Instance) {
			return "candidate", true, nil
		}
		if j.Previous != nil && reflect.DeepEqual(existing, *j.Previous) {
			return "previous", true, nil
		}
		return "conflict", true, nil
	}
	if core.ErrorCode(err) != "not_found" {
		return "", false, err
	}
	if j.Previous != nil {
		return "conflict", false, nil
	}
	return "missing", false, nil
}

func (e *PrivilegedExecutor) installRecoveryAction(ctx context.Context, input installRecoveryActionInput) (installRecoveryView, error) {
	if !validInstanceID(input.ID) || input.ExpectedJournalRevision < 1 || (input.Command != "continue" && input.Command != "stop_preserve") {
		return installRecoveryView{}, failure("invalid_input", "Invalid install recovery command or journal revision", 400)
	}
	unlock, err := e.lockContext(ctx, input.ID)
	if err != nil {
		return installRecoveryView{}, err
	}
	defer unlock()
	j, err := e.loadInstallJournal(input.ID)
	if err != nil {
		return installRecoveryView{}, err
	}
	if j.Revision != input.ExpectedJournalRevision {
		return installRecoveryView{}, failure("revision_conflict", "Install recovery journal changed; inspect it again before recovery", 412)
	}
	if input.Command == "continue" {
		err = e.continueInstallRecovery(ctx, &j)
	} else {
		err = e.stopPreserveInstallRecovery(ctx, &j)
	}
	if err != nil {
		return installRecoveryView{}, err
	}
	return e.installRecoveryInspect(ctx, input.ID)
}

func (e *PrivilegedExecutor) continueInstallRecovery(ctx context.Context, j *installJournal) error {
	switch j.Phase {
	case "health_failed", "health_verified", "boot_verified", "receipt_pending":
	default:
		return failure("recovery_checkpoint_unsupported", "Continue is permitted only after the install health checkpoint", 412)
	}
	receiptState, _, err := e.installJournalReceiptState(*j)
	if err != nil {
		return err
	}
	if receiptState == "conflict" || receiptState == "candidate" && j.Phase != "receipt_pending" {
		return failure("ownership_conflict", "The current receipt no longer matches the protected install checkpoint; no recovery action was taken", 409)
	}
	if receiptState == "candidate" {
		// The receipt has already committed. Do not probe or change a running
		// service again: it may have been deliberately stopped since that commit.
		return e.saveNextInstallJournal(ctx, j, "completed")
	}
	if j.Phase == "health_failed" {
		actual, err := e.verifyHealthFailedInstallRecovery(ctx, *j)
		if err != nil {
			return err
		}
		j.Instance.Status = "running"
		j.Instance.Health = actual
		if err := e.saveNextInstallJournal(ctx, j, "health_verified"); err != nil {
			return err
		}
	} else {
		if err := e.verifyInstallJournalProofs(ctx, *j); err != nil {
			return err
		}
		if j.Instance.Health.Status != "healthy" && j.Instance.Health.Status != "available" && j.Instance.Health.Status != "configuration_required" {
			return failure("recovery_checkpoint_unsupported", "The recorded install did not pass its health check", 412)
		}
		if j.Instance.Service.Program != "" && j.Instance.Service.Kind != "cli" {
			actual := Health{}
			if e.installHealthHook != nil {
				actual = e.installHealthHook(ctx, j.Instance)
			} else {
				actual = e.probe(ctx, j.Instance)
			}
			if actual.Status != "healthy" {
				return failure("install_recovery_health_failed", "The verified install is no longer healthy; it was preserved for inspection", 409)
			}
		}
	}
	if j.DesiredBoot != nil && j.Instance.Service.Program != "" && j.Instance.Service.Kind == "daemon" {
		var bootErr error
		if *j.DesiredBoot {
			bootErr = e.enableJournalOwnedUnit(ctx, j)
		} else {
			bootErr = e.setJournalOwnedBootState(ctx, *j, false)
		}
		if bootErr != nil {
			return bootErr
		}
		value := *j.DesiredBoot
		j.Instance.Service.Autostart = &value
	}
	if j.Phase != "boot_verified" && j.DesiredBoot != nil {
		if err := e.saveNextInstallJournal(ctx, j, "boot_verified"); err != nil {
			return err
		}
	}
	if err := e.saveNextInstallJournal(ctx, j, "receipt_pending"); err != nil {
		return err
	}
	if err := e.commitInstallReceipt(ctx, j); err != nil {
		return err
	}
	return e.saveNextInstallJournal(ctx, j, "completed")
}

func (e *PrivilegedExecutor) verifyHealthFailedInstallRecovery(ctx context.Context, j installJournal) (Health, error) {
	if err := ctx.Err(); err != nil {
		return Health{}, err
	}
	if j.Phase != "health_failed" || !e.installStopPreserveAllowed(j) {
		return Health{}, failure("recovery_checkpoint_unsupported", "Health-failed recovery requires an exact owned systemd daemon proof", 412)
	}
	if err := e.verifyInstallJournalProofs(ctx, j); err != nil {
		return Health{}, err
	}
	if err := e.verifyInitializedRuntimeCredential(j.Instance); err != nil {
		return Health{}, err
	}
	ownedPID, err := ownedProcessID(j.Instance)
	if err != nil {
		return Health{}, failure("install_recovery_health_failed", "The owned runtime process is not currently proven; it was preserved", 409)
	}
	if err := e.verifyJournalUnitMainPID(ctx, j.Instance.Service.Unit, ownedPID); err != nil {
		return Health{}, err
	}
	actual := e.probe(ctx, j.Instance)
	if err := ctx.Err(); err != nil {
		return Health{}, err
	}
	if actual.Status != "healthy" {
		return Health{}, failure("install_recovery_health_failed", "The owned service is not currently protocol-healthy; it was preserved", 409)
	}
	return actual, nil
}

func (e *PrivilegedExecutor) verifyJournalUnitMainPID(ctx context.Context, unit string, ownedPID int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var result agent.Result
	var runErr error
	if e.installSystemdMainPIDHook != nil {
		result, runErr = e.installSystemdMainPIDHook(ctx, unit)
	} else {
		operation := agent.Operation{Name: "plugin_active_main_pid", Program: "/usr/bin/systemctl", Timeout: 15 * time.Second,
			BuildArgs: func(json.RawMessage) ([]string, error) {
				return []string{"show", "--property=ActiveState,MainPID", "--", unit}, nil
			}}
		runner, err := agent.NewRunner(agent.Options{Operations: []agent.Operation{operation}, MaxOutputBytes: 1024, MaxConcurrent: 1})
		if err != nil {
			return failure("install_recovery_state_unknown", "The owned service process could not be verified", 502)
		}
		result, runErr = runner.Run(ctx, agent.Request{Operation: operation.Name, Input: json.RawMessage(`{}`)})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return decodeSystemdActiveMainPID(result, runErr, ownedPID)
}

func decodeSystemdActiveMainPID(result agent.Result, runErr error, ownedPID int) error {
	unknown := failure("install_recovery_state_unknown", "The owned service main process state could not be verified", 502)
	if result.Operation != "plugin_active_main_pid" || result.Truncated || result.Canceled || result.TimedOut || result.DroppedEvents != 0 || result.Stderr != "" || runErr != nil || result.ExitCode != 0 || ownedPID < 2 {
		return unknown
	}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || (key != "ActiveState" && key != "MainPID") {
			return unknown
		}
		if _, exists := fields[key]; exists {
			return unknown
		}
		fields[key] = value
	}
	if len(fields) != 2 {
		return unknown
	}
	mainPID, err := strconv.Atoi(fields["MainPID"])
	if err != nil || mainPID < 2 {
		return unknown
	}
	if fields["ActiveState"] != "active" {
		return failure("install_recovery_health_failed", "The journal-owned systemd unit is not active; it was preserved", 409)
	}
	if mainPID != ownedPID {
		return failure("ownership_conflict", "The systemd main process does not match the journal-owned runtime process; it was preserved", 409)
	}
	return nil
}

func (e *PrivilegedExecutor) verifyInitializedRuntimeCredential(instance Instance) error {
	switch instance.PluginID {
	case "mysql", "mysql-community", "mariadb", "postgresql", "mongodb":
	default:
		return nil
	}
	var credential managedDatabaseCredential
	if err := e.load("private", databasePrivateID(instance), &credential); err != nil {
		return failure("credential_unavailable", "Owned database initialization receipt is unavailable", 412)
	}
	expectedUser := "root"
	if instance.PluginID == "postgresql" {
		expectedUser = e.config.RunUser
	} else if instance.PluginID == "mongodb" {
		expectedUser = "powerlinux_admin"
	}
	if !credential.Initialized || credential.Engine != instance.PluginID || credential.DataDir != instance.Paths.DataDir || credential.Username != expectedUser || credential.Password == "" {
		return failure("credential_unavailable", "Owned database initialization receipt does not match its protected runtime", 412)
	}
	return nil
}

func (e *PrivilegedExecutor) commitInstallReceipt(ctx context.Context, j *installJournal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var existing Instance
	err := e.load("receipts", j.ID, &existing)
	if err == nil {
		if sameInstallReceipt(existing, j.Instance) {
			return nil
		}
		if j.Previous == nil || !reflect.DeepEqual(existing, *j.Previous) {
			return failure("ownership_conflict", "An existing managed receipt differs from the protected install journal; it was preserved", 409)
		}
	} else if core.ErrorCode(err) != "not_found" {
		return err
	} else if j.Previous != nil {
		return failure("ownership_conflict", "The prior uninstalled receipt required by this recovery is missing; no new receipt was written", 409)
	}
	return e.saveReceipt(j.ID, j.Instance)
}

func sameInstallReceipt(existing, candidate Instance) bool {
	return reflect.DeepEqual(existing, candidate)
}

func (e *PrivilegedExecutor) saveReceipt(id string, instance Instance) error {
	if e.receiptSaveHook != nil {
		return e.receiptSaveHook(id, instance)
	}
	return e.save("receipts", id, instance)
}

func (e *PrivilegedExecutor) installStopPreserveAllowed(j installJournal) bool {
	if !e.config.UseSystemd || j.Instance.Service.Program == "" || j.Instance.Service.Kind != "daemon" || j.UnitSHA256 == "" || j.ProgramProof == nil || j.ConfigSHA256 == "" {
		return false
	}
	switch j.Phase {
	case "unit_write_pending", "unit_written", "service_starting", "health_failed", "health_verified", "boot_verified", "receipt_pending", "preserve_started":
		return true
	default:
		return false
	}
}

func (e *PrivilegedExecutor) stopPreserveInstallRecovery(ctx context.Context, j *installJournal) error {
	if !e.installStopPreserveAllowed(*j) {
		return failure("recovery_checkpoint_unsupported", "Stop and preserve requires an exact verified owned unit and runtime proof", 412)
	}
	receiptState, _, err := e.installJournalReceiptState(*j)
	if err != nil {
		return err
	}
	if receiptState == "candidate" || receiptState == "conflict" {
		return failure("ownership_conflict", "The current receipt does not prove an interrupted uncommitted install; the service and receipt were preserved", 409)
	}
	if err := e.verifyInstallJournalProofs(ctx, *j); err != nil {
		return err
	}
	if j.Phase != "preserve_started" {
		if err := e.saveNextInstallJournal(ctx, j, "preserve_started"); err != nil {
			return err
		}
	}
	if _, err := runFixed(ctx, "/usr/bin/systemctl", []string{"disable", "--now", j.Instance.Service.Unit}, 30*time.Second, nil); err != nil {
		return failure("install_recovery_stop_failed", "The verified owned service could not be confirmed stopped; its data was preserved", 502)
	}
	active, err := systemdUnitActive(ctx, j.Instance.Service.Unit)
	if err != nil || active {
		return failure("install_recovery_stop_failed", "The verified owned service is not confirmed inactive", 502)
	}
	enabled, state, err := e.journalBootState(ctx, *j)
	if err != nil || enabled || state != "disabled" {
		return failure("install_recovery_stop_failed", "The verified owned service is not confirmed disabled", 502)
	}
	return e.saveNextInstallJournal(ctx, j, "preserved_stopped")
}

func systemdUnitActive(ctx context.Context, unit string) (bool, error) {
	runner, err := agent.NewRunner(agent.Options{Operations: []agent.Operation{{Name: "plugin_active_state", Program: "/usr/bin/systemctl", Timeout: 15 * time.Second,
		BuildArgs: func(json.RawMessage) ([]string, error) { return []string{"is-active", unit}, nil }}}, MaxOutputBytes: 1024, MaxConcurrent: 1})
	if err != nil {
		return false, err
	}
	result, err := runner.Run(ctx, agent.Request{Operation: "plugin_active_state", Input: json.RawMessage(`{}`)})
	return decodeSystemdActiveState(result, err)
}

func decodeSystemdActiveState(result agent.Result, runErr error) (bool, error) {
	unknown := failure("install_recovery_state_unknown", "The owned service active state is unknown", 502)
	if result.Operation != "plugin_active_state" || result.Truncated || result.Canceled || result.TimedOut || result.Stderr != "" {
		return false, unknown
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != result.ExitCode {
			return false, unknown
		}
	} else if result.ExitCode != 0 {
		return false, unknown
	}
	state := strings.TrimSpace(result.Stdout)
	if state == "active" && runErr == nil && result.ExitCode == 0 {
		return true, nil
	}
	if (state == "inactive" || state == "failed") && result.ExitCode == 3 {
		return false, nil
	}
	return false, unknown
}

func (e *PrivilegedExecutor) enableJournalOwnedUnit(ctx context.Context, j *installJournal) error {
	if e.installEnableHook != nil {
		return e.installEnableHook(ctx, j)
	}
	return e.setJournalOwnedBootState(ctx, *j, true)
}

func (e *PrivilegedExecutor) setJournalOwnedBootState(ctx context.Context, j installJournal, desired bool) error {
	if err := e.verifyJournalUnit(j); err != nil {
		return err
	}
	enabled, state, err := e.journalBootState(ctx, j)
	if err != nil {
		return err
	}
	if enabled == desired && (desired || state == "disabled") {
		return nil
	}
	if desired && state != "disabled" || !desired && state != "enabled" {
		return failure("install_recovery_state_unknown", "The unit boot state is not safely changeable", 412)
	}
	command := "disable"
	if desired {
		command = "enable"
	}
	if _, err = runFixed(ctx, "/usr/bin/systemctl", []string{command, j.Instance.Service.Unit}, 20*time.Second, nil); err != nil {
		return err
	}
	enabled, state, err = e.journalBootState(ctx, j)
	if err != nil || enabled != desired || desired && state != "enabled" || !desired && state != "disabled" {
		return failure("install_recovery_boot_failed", "The verified owned service boot state could not be confirmed", 502)
	}
	return nil
}

func (e *PrivilegedExecutor) journalBootState(ctx context.Context, j installJournal) (bool, string, error) {
	if !e.config.UseSystemd || !ownedUnit(j.Instance) || j.UnitSHA256 == "" {
		return false, "", failure("autostart_unavailable", "Install recovery requires a verified owned systemd unit", 412)
	}
	if err := e.verifyJournalUnit(j); err != nil {
		return false, "", err
	}
	if err := e.checkSystemdIdentity(ctx, j.Instance.Service.Unit, false); err != nil {
		return false, "", err
	}
	runner, err := agent.NewRunner(agent.Options{Operations: []agent.Operation{{Name: "plugin_boot_state", Program: "/usr/bin/systemctl", Timeout: 15 * time.Second,
		BuildArgs: func(json.RawMessage) ([]string, error) { return []string{"is-enabled", j.Instance.Service.Unit}, nil }}}, MaxOutputBytes: 1024, MaxConcurrent: 1})
	if err != nil {
		return false, "", err
	}
	result, err := runner.Run(ctx, agent.Request{Operation: "plugin_boot_state", Input: json.RawMessage(`{}`)})
	return decodeBootState(result, err)
}

func (e *PrivilegedExecutor) verifyInstallJournalProofs(ctx context.Context, j installJournal) error {
	if e.installProofHook != nil {
		return e.installProofHook(ctx, j)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if j.Instance.Paths != e.instancePaths(j.Instance.PluginID, j.Instance.InstanceID) || j.Instance.Packages == nil && len(j.Plan.Packages) != 0 {
		return failure("install_journal_invalid", "Install recovery path or package proof is invalid", 412)
	}
	manifest, err := e.catalog.Get(j.Instance.PluginID)
	if err != nil {
		return failure("install_journal_invalid", "Install recovery plugin is no longer in the trusted catalog", 412)
	}
	version, err := manifest.SelectVersion(j.Instance.SourceVersion)
	if err != nil || !reflect.DeepEqual(version, j.Plan.Version) {
		return failure("install_journal_invalid", "Install recovery recipe differs from the trusted catalog", 412)
	}
	var savedPlan Plan
	if err = e.load("plans", j.Plan.ID, &savedPlan); err != nil || !reflect.DeepEqual(savedPlan, j.Plan) {
		return failure("install_journal_invalid", "Install recovery plan differs from its durable protected plan", 412)
	}
	if err = verifyInstalledPackagePins(ctx, j.Instance.Packages); err != nil {
		return err
	}
	if (version.Source.Kind == "verified_archive" || version.Source.Kind == "verified_source") && j.Instance.ArtifactSHA256 != version.Source.SHA256 {
		return failure("install_journal_invalid", "Installed artifact differs from the trusted source hash", 412)
	}
	configHash, err := hashManagedTree(j.Instance.Paths.ConfigDir)
	if err != nil || configHash != j.ConfigSHA256 {
		return failure("ownership_conflict", "Managed install configuration differs from its protected recovery proof; it was preserved", 409)
	}
	if j.Instance.Service.Program == "" {
		if j.UnitSHA256 != "" || j.ProgramProof != nil || j.Instance.Service.Kind != "integration" {
			return failure("install_journal_invalid", "Install recovery is missing its runtime proof", 412)
		}
		return nil
	}
	if j.ProgramProof == nil {
		return failure("install_journal_invalid", "Install recovery is missing its program proof", 412)
	}
	if j.ProgramProof.Path != j.Instance.Service.Program {
		return failure("install_journal_invalid", "Install program path differs from its protected proof", 412)
	}
	switch version.Source.Kind {
	case "verified_archive", "verified_source":
		relative, relErr := filepath.Rel(filepath.Join(j.Instance.Paths.Root, "runtime"), j.Instance.Service.Program)
		resolvedRelative, resolvedErr := filepath.Rel(filepath.Join(j.Instance.Paths.Root, "runtime"), j.ProgramProof.ResolvedPath)
		if relErr != nil || !filepath.IsLocal(relative) || resolvedErr != nil || !filepath.IsLocal(resolvedRelative) {
			return failure("install_journal_invalid", "Install archive program escaped its owned runtime", 412)
		}
	case "signed_package":
		expectedProgram, programErr := resolvePackageProgram(manifest, version, j.Instance.Packages)
		if programErr != nil || expectedProgram != j.Instance.Service.Program {
			return failure("install_journal_invalid", "Install package program differs from the trusted package recipe", 412)
		}
	default:
		return failure("install_journal_invalid", "Trusted install source cannot own an executable", 412)
	}
	actual, err := hashManagedExecutable(j.Instance.Service.Program)
	if err != nil || actual != *j.ProgramProof {
		return failure("ownership_conflict", "Installed program differs from its protected recovery proof; it was preserved", 409)
	}
	if e.config.UseSystemd && j.Instance.Service.Kind != "cli" {
		if err := e.verifyJournalUnit(j); err != nil {
			return err
		}
		if err := e.checkSystemdIdentity(ctx, j.Instance.Service.Unit, false); err != nil {
			return err
		}
	} else if j.UnitSHA256 != "" {
		return failure("install_journal_invalid", "Unexpected systemd unit proof is present", 412)
	}
	return nil
}

func hashManagedExecutable(path string) (fileProof, error) {
	var proof fileProof
	if !filepath.IsAbs(path) {
		return proof, failure("install_journal_invalid", "Install program path is invalid", 412)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return proof, err
	}
	file, err := os.OpenFile(resolved, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return proof, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 || info.Size() < 1 || info.Size() > 1<<30 {
		return proof, failure("install_journal_invalid", "Install program is not a protected executable", 412)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
		return proof, failure("install_journal_invalid", "Install program is not owned by a trusted system identity", 412)
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(file, (1<<30)+1))
	if err != nil || count != info.Size() {
		return proof, failure("install_journal_invalid", "Install program could not be hashed safely", 412)
	}
	proof = fileProof{Path: path, ResolvedPath: resolved, SHA256: hex.EncodeToString(hash.Sum(nil)), Size: info.Size(), Mode: uint32(info.Mode().Perm()), UID: stat.Uid, GID: stat.Gid}
	return proof, nil
}

func hashManagedTree(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", failure("install_journal_invalid", "Managed configuration path is invalid", 412)
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", failure("install_journal_invalid", "Managed configuration directory is unavailable", 412)
	}
	if rootInfo.Mode().Perm()&0022 != 0 {
		return "", failure("ownership_conflict", "Managed configuration directory is writable outside its owner; it was preserved", 409)
	}
	hash := sha256.New()
	count := 0
	var total int64
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		count++
		if count > 2048 {
			return failure("install_journal_too_large", "Managed configuration has too many files to prove safely", 413)
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() && !info.Mode().IsRegular() {
			return failure("install_journal_invalid", "Managed configuration contains an unsafe file", 412)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || !filepath.IsLocal(relative) && relative != "." {
			return failure("install_journal_invalid", "Managed configuration escaped its protected directory", 412)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode().Perm()&0022 != 0 || stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
			return failure("install_journal_invalid", "Managed configuration owner could not be verified", 412)
		}
		_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%d\x00%d\x00%d\x00", relative, info.Mode().String(), info.Size(), stat.Uid, stat.Gid)
		if info.IsDir() {
			return nil
		}
		total += info.Size()
		if total > 64<<20 {
			return failure("install_journal_too_large", "Managed configuration exceeds the recovery proof limit", 413)
		}
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		count, copyErr := io.Copy(hash, io.LimitReader(file, info.Size()+1))
		if copyErr == nil && count != info.Size() {
			copyErr = failure("install_journal_invalid", "Managed configuration changed while it was being proved", 412)
		}
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if _, err = io.WriteString(hash, "\x00"); err != nil {
			return err
		}
		return closeErr
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyInstalledPackagePins(ctx context.Context, pins []PackagePin) error {
	if len(pins) == 0 {
		return nil
	}
	if len(pins) > 128 {
		return failure("install_journal_invalid", "Install package proof exceeds its bounds", 412)
	}
	for _, pin := range pins {
		if !packageName.MatchString(pin.Name) || !debVersion.MatchString(pin.Version) || !artifactHash.MatchString(pin.SHA256) ||
			(pin.Format != "" && pin.Format != "rpm") || pin.Architecture == "" || len(pin.Architecture) > 32 || len(pin.Repository) > 512 {
			return failure("install_journal_invalid", "Install package pin is invalid", 412)
		}
		var output string
		var err error
		if pin.Format == "rpm" {
			output, err = runFixed(ctx, "/usr/bin/rpm", []string{"-q", "--qf", "%{EPOCHNUM}:%{VERSION}-%{RELEASE}|%{ARCH}\n", pin.Name}, 15*time.Second, nil)
		} else {
			output, err = runFixed(ctx, "/usr/bin/dpkg-query", []string{"-W", "-f=${Version}|${Architecture}\n", pin.Name}, 15*time.Second, nil)
		}
		actual := strings.TrimSpace(output)
		version, architecture, found := strings.Cut(actual, "|")
		if err != nil || !found || version != pin.Version || architecture != pin.Architecture {
			return failure("package_recovery_proof_failed", "Installed packages differ from the exact protected plan; no recovery action was taken", 412)
		}
	}
	return nil
}

func systemdUnitBytes(e *PrivilegedExecutor, instance Instance, nonce string) ([]byte, error) {
	if !ownedUnit(instance) || nonce != "" && !installNoncePattern.MatchString(nonce) {
		return nil, failure("ownership_invalid", "Managed unit identity or install nonce is invalid", 412)
	}
	args, err := e.serviceArgs(instance, "start")
	if err != nil {
		return nil, err
	}
	switch instance.PluginID {
	case "openresty", "nginx":
		args = append(args, "-g", "daemon off;")
	case "php", "php-apt", "php-yum":
		args[len(args)-1] = "-F"
	case "redis", "valkey":
		args = append(args, "--daemonize", "no")
	case "memcached", "pureftp":
		args = args[1:]
	case "rsyncd", "supervisor":
		args = append(args, map[string]string{"rsyncd": "--no-detach", "supervisor": "-n"}[instance.PluginID])
	}
	command := unitQuote(instance.Service.Program)
	for _, arg := range args {
		command += " " + unitQuote(arg)
	}
	identity := ""
	if instance.PluginID != "openresty" && instance.PluginID != "nginx" && !isPHPRuntime(instance.PluginID) && instance.PluginID != "pureftp" && instance.PluginID != "docker" {
		group := e.config.RunGroup
		if group == "" {
			group = e.config.RunUser
		}
		identity = "User=" + e.config.RunUser + "\nGroup=" + group + "\n"
	}
	reload := ""
	if instance.PluginID == "openresty" || instance.PluginID == "nginx" {
		reload = "ExecReload=" + unitQuote(instance.Service.Program)
		reloadArgs, _ := e.serviceArgs(instance, "reload")
		for _, arg := range reloadArgs {
			reload += " " + unitQuote(arg)
		}
		reload += "\n"
	}
	if isPHPRuntime(instance.PluginID) {
		reload = "ExecReload=/bin/kill -USR2 $MAINPID\n"
	}
	environment := ""
	for _, variable := range runtimeEnvironment(instance) {
		environment += "Environment=" + unitQuote(variable) + "\n"
	}
	unit := "[Unit]\nDescription=Folio managed " + instance.ID + "\nAfter=network.target\n[Service]\nType=simple\n" + identity + environment + "ExecStart=" + command + "\n" + reload + "Restart=on-failure\nNoNewPrivileges=true\nPrivateTmp=true\nProtectHome=true\nProtectSystem=strict\nReadWritePaths=" + unitQuote(instance.Paths.Root) + " -/srv/powerlinux-sites\n[Install]\nWantedBy=multi-user.target\n"
	if nonce != "" {
		unit += "# PowerLinux install journal nonce: " + nonce + "\n"
	}
	return []byte(unit), nil
}

func createOnlyUnitFile(dir, name string, data []byte) error {
	if !filepath.IsAbs(dir) || !filepath.IsLocal(name) || strings.ContainsAny(name, `/\\`) {
		return failure("invalid_path", "Managed systemd unit path is invalid", 400)
	}
	if err := realDirectory(dir); err != nil {
		return failure("ownership_invalid", "Systemd unit directory is not a protected real directory", 412)
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil || dirInfo.Mode().Perm()&0022 != 0 {
		return failure("ownership_invalid", "Systemd unit directory permissions are unsafe", 412)
	}
	if os.Geteuid() == 0 {
		if stat, ok := dirInfo.Sys().(*syscall.Stat_t); !ok || stat.Uid != 0 {
			return failure("ownership_invalid", "Systemd unit directory is not root owned", 412)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	nonce, err := newInstallNonce()
	if err != nil {
		return err
	}
	temporary := ".powerlinux-unit-" + nonce
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	_, writeErr := file.Write(data)
	if writeErr == nil && os.Geteuid() == 0 {
		writeErr = file.Chown(0, 0)
	}
	if writeErr == nil {
		writeErr = file.Chmod(0644)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return writeErr
	}
	// Link publishes the complete synced inode atomically and refuses an
	// existing name. A crash during the write leaves only an ignored temporary
	// file, never a partial service unit or an overwritten foreign unit.
	if err = root.Link(temporary, name); err != nil {
		if os.IsExist(err) {
			return failure("ownership_conflict", "An existing service unit is not owned by this install journal; it was preserved", 409)
		}
		return err
	}
	if err = root.Remove(temporary); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr = directory.Close()
	if err == nil {
		err = closeErr
	}
	return err
}

func (e *PrivilegedExecutor) verifyJournalUnit(j installJournal) error {
	if !e.config.UseSystemd || j.UnitSHA256 == "" || !ownedUnit(j.Instance) {
		return failure("install_journal_invalid", "Install recovery does not contain an owned systemd unit proof", 412)
	}
	dir := e.systemdUnitDir()
	dropInDir := filepath.Join(dir, j.Instance.Service.Unit+".d")
	if info, err := os.Lstat(dropInDir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return failure("ownership_conflict", "The journal-owned service has an unsafe systemd drop-in path; it was preserved", 409)
		}
		entries, readErr := os.ReadDir(dropInDir)
		if readErr != nil || len(entries) != 0 {
			return failure("ownership_conflict", "The journal-owned service has an external systemd drop-in; it was preserved", 409)
		}
	} else if !os.IsNotExist(err) {
		return failure("ownership_conflict", "The journal-owned systemd drop-in state could not be verified; it was preserved", 409)
	}
	file, err := os.OpenFile(filepath.Join(dir, j.Instance.Service.Unit), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return failure("ownership_conflict", "The journal-owned service unit is missing or unsafe; it was preserved", 409)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 || info.Mode().Perm()&0022 != 0 {
		return failure("ownership_conflict", "The journal-owned service unit is unsafe; it was preserved", 409)
	}
	if os.Geteuid() == 0 {
		if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != 0 {
			return failure("ownership_conflict", "The journal-owned service unit is not root owned; it was preserved", 409)
		}
	}
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return failure("ownership_conflict", "The journal-owned service unit could not be verified", 409)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != j.UnitSHA256 || !strings.Contains(string(data), "# PowerLinux install journal nonce: "+j.Nonce+"\n") {
		return failure("ownership_conflict", "The service unit differs from the journal's exact nonce and hash; it was preserved", 409)
	}
	return nil
}

func (e *PrivilegedExecutor) systemdUnitDir() string {
	if e.unitDir != "" {
		return e.unitDir
	}
	return "/etc/systemd/system"
}
