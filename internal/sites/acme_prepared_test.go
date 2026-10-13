package sites

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/jobs"
)

func acmeOperationTasks(t *testing.T, f credentialVersionFixture, operation string) []map[string]any {
	t.Helper()
	items := []map[string]any{}
	for _, job := range credentialListTasks(t, f) {
		if job["operation"] == operation {
			items = append(items, job)
		}
	}
	return items
}

func acmeStoredTaskPayload(t *testing.T, f credentialVersionFixture, id string) string {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(f.dataDir, "powerlinux.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var payload string
	if err := db.QueryRow("SELECT payload FROM tasks WHERE id=?", id).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func acmeManualInput(id string) acmePublicInput {
	return acmePublicInput{ACMEInput: ACMEInput{ID: id, Domains: []string{"example.test"}, Email: "fixture@example.test", Challenge: "manual_dns"}}
}

func TestACMEPreparedConcurrentIssueCommitsOneTaskWithFullResourcesAndPinnedMetadata(t *testing.T) {
	f := newCredentialVersionFixture(t)
	f.save(t, 0, "prepared-credential-token")
	if err := f.state.Delete(context.Background(), "dns_credential_versions", dnsVersionID("version-dns", 1)); err != nil {
		t.Fatal(err)
	}
	f.agent.mu.Lock()
	f.agent.sites = []Site{{ID: "prepared-site", TLS: TLSBinding{Enabled: true, CertificateID: "prepared-certificate"}}}
	f.agent.mu.Unlock()
	release := credentialHoldFourSlots(t, f)

	start := make(chan struct{})
	type outcome struct {
		value any
		err   error
	}
	results := make(chan outcome, 2)
	for _, domain := range []string{"first.example.test", "second.example.test"} {
		input := credentialInput("prepared-certificate")
		input.Domains = []string{domain}
		go func(raw json.RawMessage) {
			<-start
			value, err := f.service.Execute(context.Background(), f.actor, "certificates_acme_issue", raw)
			results <- outcome{value: value, err: err}
		}(credentialPublicRaw(input))
	}
	close(start)
	var accepted map[string]any
	acceptedCount, pendingCount := 0, 0
	for range 2 {
		result := <-results
		if result.err == nil {
			accepted = result.value.(map[string]any)
			acceptedCount++
		} else if core.ErrorCode(result.err) == "acme_submission_pending" {
			pendingCount++
		} else {
			t.Fatal("concurrent ACME submission failed unexpectedly", result.err)
		}
	}
	if acceptedCount != 1 || pendingCount != 1 {
		t.Fatal("concurrent issue requests did not admit exactly one durable task", acceptedCount, pendingCount)
	}
	jobID := accepted["id"].(string)
	job, err := f.provider.deps.Tasks.Get(context.Background(), f.actor, jobID)
	if err != nil || job["status"] != "queued" {
		t.Fatal("ACME job was not durably queued behind occupied workers", err)
	}
	resources, ok := job["resources"].([]string)
	if !ok || !slices.Contains(resources, CertificateResource("prepared-certificate")) || !slices.Contains(resources, dnsResource("version-dns")) || !slices.Contains(resources, Resource("prepared-site")) {
		t.Fatal("prepared task did not preserve the complete action resource set", resources)
	}
	if got := len(acmeOperationTasks(t, f, "certificate_acme_issue")); got != 1 {
		t.Fatal("concurrent issue requests committed multiple task rows", got)
	}
	settingsRaw, err := f.state.Get(context.Background(), "acme_settings", "prepared-certificate")
	var settings ACMEInput
	if err != nil || json.Unmarshal(settingsRaw, &settings) != nil || settings.CredentialRevision != 1 {
		t.Fatal("ACME settings and credential pin did not commit with the task", err)
	}
	submissionRaw, err := f.state.Get(context.Background(), "acme_submissions", "prepared-certificate")
	var submission acmeSubmission
	if err != nil || json.Unmarshal(submissionRaw, &submission) != nil || submission.JobID != jobID || submission.ActorID != f.actor.ID {
		t.Fatal("the committed submission receipt did not bind the durable task and actor", err)
	}
	if _, err := f.state.Get(context.Background(), "dns_credential_versions", dnsVersionID("version-dns", 1)); err != nil {
		t.Fatal("credential version was not pinned in the prepared transaction", err)
	}
	payload := acmeStoredTaskPayload(t, f, jobID)
	if strings.Contains(payload, "prepared-credential-token") || strings.Contains(payload, "CF_Token") || strings.Contains(string(settingsRaw), "secret_ref") {
		t.Fatal("stored task or ACME settings exposed DNS credential material")
	}
	select {
	case <-f.agent.received:
		t.Fatal("ACME executor ran before the four existing tasks released their slots")
	default:
	}
	release()
	credentialAwaitJob(t, f, jobID, "succeeded")
	issued := <-f.agent.received
	if issued.ACME.CredentialRevision != 1 || issued.Credentials["CF_Token"] != "prepared-credential-token" {
		t.Fatal("the committed task did not resolve its pinned private credential")
	}
}

type cancelAfterPreparedTasks struct {
	core.TaskSubmitter
	cancel context.CancelFunc
}

func (t cancelAfterPreparedTasks) SubmitPrepared(ctx context.Context, actor core.Actor, operation string, input json.RawMessage, metadata core.TaskMetadata, prepare func(core.MetadataStore, jobs.Job) error) (map[string]any, error) {
	value, err := t.TaskSubmitter.SubmitPrepared(ctx, actor, operation, input, metadata, prepare)
	if err == nil {
		t.cancel()
	}
	return value, err
}

func TestACMEPreparedCallerCancellationAfterCommitKeepsJobAndSettings(t *testing.T) {
	f := newCredentialVersionFixture(t)
	f.save(t, 0, "cancel-window-token")
	release := credentialHoldFourSlots(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	f.provider.deps.Tasks = cancelAfterPreparedTasks{TaskSubmitter: f.provider.deps.Tasks, cancel: cancel}
	input := credentialInput("cancel-window-certificate")
	value, err := f.service.Execute(ctx, f.actor, "certificates_acme_issue", credentialPublicRaw(input))
	if err != nil || ctx.Err() == nil {
		t.Fatal("caller cancellation after prepared commit hid the durable task", err)
	}
	jobID := value.(map[string]any)["id"].(string)
	job, err := f.provider.deps.Tasks.Get(context.Background(), f.actor, jobID)
	if err != nil || job["status"] != "queued" {
		t.Fatal("caller cancellation changed the committed task", err)
	}
	var submission acmeSubmission
	raw, err := f.state.Get(context.Background(), "acme_submissions", input.ID)
	if err != nil || json.Unmarshal(raw, &submission) != nil || submission.JobID != jobID {
		t.Fatal("caller cancellation rolled back the committed ACME receipt", err)
	}
	if _, err := f.state.Get(context.Background(), "acme_settings", input.ID); err != nil {
		t.Fatal("caller cancellation rolled back the committed ACME settings", err)
	}
	release()
	credentialAwaitJob(t, f, jobID, "succeeded")
	if got := <-f.agent.received; got.ID != input.ID {
		t.Fatal("committed ACME task did not run after caller cancellation")
	}
}

func TestACMEPreparedOrderActionsUseOneOrderReceiptAndNeverReplayCanceledTask(t *testing.T) {
	f := newCredentialVersionFixture(t)
	f.save(t, 0, "order-action-token")
	f.agent.mu.Lock()
	f.agent.sites = []Site{{ID: "order-site", TLS: TLSBinding{Enabled: true, CertificateID: "order-action-certificate"}}}
	f.agent.mu.Unlock()
	input := credentialInput("order-action-certificate")
	issued, err := f.service.Execute(context.Background(), f.actor, "certificates_acme_issue", credentialPublicRaw(input))
	if err != nil {
		t.Fatal(err)
	}
	credentialAwaitJob(t, f, issued.(map[string]any)["id"].(string), "succeeded")
	if got := <-f.agent.received; got.ID != input.ID || got.ACME == nil {
		t.Fatal("initial ACME order was not created")
	}
	release := credentialHoldFourSlots(t, f)

	value, err := f.service.Execute(context.Background(), f.actor, "certificates_acme_cancel", encode(map[string]any{"id": input.ID}))
	if err != nil {
		t.Fatal("manual order cancellation was not admitted as a prepared task", err)
	}
	jobID := value.(map[string]any)["id"].(string)
	job, err := f.provider.deps.Tasks.Get(context.Background(), f.actor, jobID)
	if err != nil || job["status"] != "queued" {
		t.Fatal("order cancellation was not held in the durable queue", err)
	}
	resources, ok := job["resources"].([]string)
	if !ok || !slices.Contains(resources, CertificateResource(input.ID)) || !slices.Contains(resources, dnsResource(input.CredentialID)) || !slices.Contains(resources, Resource("order-site")) {
		t.Fatal("order action task lost one of its authorized resources", resources)
	}
	f.agent.mu.Lock()
	orderRef := f.agent.refs[input.ID]
	f.agent.mu.Unlock()
	raw, err := f.state.Get(context.Background(), "acme_order_actions", acmeOrderActionKey(input.ID, orderRef))
	var receipt acmeOrderSubmission
	if err != nil || json.Unmarshal(raw, &receipt) != nil || receipt.JobID != jobID || receipt.Kind != "cancel" {
		t.Fatal("order action receipt was not atomically bound to its task", err)
	}
	if strings.Contains(string(raw), "order-action-token") || strings.Contains(string(raw), "secret_ref") {
		t.Fatal("order action receipt exposed DNS credential material")
	}
	for _, action := range []string{"certificates_acme_cancel", "certificates_acme_confirm"} {
		_, err := f.service.Execute(context.Background(), f.actor, action, encode(map[string]any{"id": input.ID}))
		if core.ErrorCode(err) != "acme_order_action_submitted" {
			t.Fatal("duplicate or opposing order action was not blocked by its durable receipt", action, err)
		}
	}
	if got := len(acmeOperationTasks(t, f, "certificate_acme_order")); got != 1 {
		t.Fatal("an order retry created another remote action task", got)
	}
	if _, err := f.service.Execute(context.Background(), f.actor, "tasks_cancel", encode(map[string]any{"id": jobID})); err != nil {
		t.Fatal("could not cancel the queued durable order task", err)
	}
	credentialAwaitJob(t, f, jobID, "canceled")
	retry, err := f.service.Execute(context.Background(), f.actor, "certificates_acme_confirm", encode(map[string]any{"id": input.ID}))
	if err != nil {
		t.Fatal("a task proven canceled before its executor started could not be recovered", err)
	}
	retryID := retry.(map[string]any)["id"].(string)
	raw, err = f.state.Get(context.Background(), "acme_order_actions", acmeOrderActionKey(input.ID, orderRef))
	if err != nil || json.Unmarshal(raw, &receipt) != nil || receipt.JobID != retryID || receipt.Kind != "confirm" {
		t.Fatal("safe retry did not atomically replace the canceled action receipt", err)
	}
	if got := len(acmeOperationTasks(t, f, "certificate_acme_order")); got != 2 {
		t.Fatal("safe canceled-task recovery did not preserve exactly one task per admitted action", got)
	}
	release()
	credentialAwaitJob(t, f, retryID, "succeeded")
	select {
	case action := <-f.agent.received:
		if action.Kind != "confirm" {
			t.Fatal("the canceled action was replayed instead of its verified replacement", action.Kind)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("verified replacement ACME order action did not run")
	}
}

func TestACMEPreparedRestartHelper(t *testing.T) {
	dir := os.Getenv("POWERLINUX_ACME_RESTART_DIR")
	if dir == "" {
		return
	}
	idPath := os.Getenv("POWERLINUX_ACME_RESTART_ID_PATH")
	f := newCredentialVersionFixtureAt(t, dir)
	_ = credentialHoldFourSlots(t, f)
	input := acmeManualInput("restart-certificate")
	value, err := f.service.Execute(context.Background(), f.actor, "certificates_acme_issue", credentialPublicRaw(input))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idPath, []byte(value.(map[string]any)["id"].(string)), 0600); err != nil {
		t.Fatal(err)
	}
	// Simulate process loss with an admitted task still queued behind four real
	// workers. os.Exit intentionally skips Service.Close and task cancellation.
	os.Exit(0)
}

func TestACMEPreparedRestartMarksQueuedTaskInterruptedWithoutReplay(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "powerlinux-data")
	idPath := filepath.Join(t.TempDir(), "acme-task-id")
	cmd := exec.Command(os.Args[0], "-test.run=^TestACMEPreparedRestartHelper$")
	cmd.Env = append(os.Environ(), "POWERLINUX_ACME_RESTART_DIR="+dir, "POWERLINUX_ACME_RESTART_ID_PATH="+idPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restart fixture process failed: %v: %s", err, output)
	}
	idBytes, err := os.ReadFile(idPath)
	if err != nil {
		t.Fatal("child did not persist the prepared task identity", err)
	}
	jobID := strings.TrimSpace(string(idBytes))

	restarted, err := core.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	login, err := restarted.Login(context.Background(), "version-admin", "credential version fixture password", "", "restart-fixture")
	if err != nil {
		t.Fatal(err)
	}
	session, err := restarted.AuthenticateSession(context.Background(), login.ID)
	if err != nil {
		t.Fatal(err)
	}
	actor := session.Actor
	deps := restarted.Runtime("sites")
	receivingAgent := &credentialVersionAgent{orders: map[string]ACMEInput{}, refs: map[string]string{}, received: make(chan request, 8)}
	provider, err := New(deps, Options{Agent: receivingAgent})
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.RegisterProvider(provider); err != nil {
		t.Fatal(err)
	}
	if err = restarted.RegisterExecutor(JobOperationNames(), provider); err != nil {
		t.Fatal(err)
	}
	job, err := provider.deps.Tasks.Get(context.Background(), actor, jobID)
	if err != nil || job["status"] != "interrupted" {
		t.Fatal("restart did not retain the queued ACME task as interrupted", err)
	}
	raw, err := deps.State.Get(context.Background(), "acme_submissions", "restart-certificate")
	var submission acmeSubmission
	if err != nil || json.Unmarshal(raw, &submission) != nil || submission.JobID != jobID {
		t.Fatal("restart lost the atomic ACME submission receipt", err)
	}
	if _, err := deps.State.Get(context.Background(), "acme_settings", "restart-certificate"); err != nil {
		t.Fatal("restart lost settings committed with the ACME task", err)
	}
	select {
	case action := <-receivingAgent.received:
		t.Fatal("restart automatically replayed an unfinished remote ACME action", action)
	case <-time.After(30 * time.Millisecond):
	}
}
