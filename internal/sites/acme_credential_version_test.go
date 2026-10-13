package sites

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

// This fixture models only the trusted Agent boundary. The actual SQLite,
// durable four-slot queue, persisted identities, AEAD secrets and ACL checks
// all use core's production implementation; DNS protocols have separate tests.
type credentialVersionAgent struct {
	mu         sync.Mutex
	orders     map[string]ACMEInput
	refs       map[string]string
	generation uint64
	sites      []Site
	received   chan request
}

func (a *credentialVersionAgent) Validate(agent.Request) error { return nil }
func (a *credentialVersionAgent) Run(_ context.Context, operation agent.Request) (agent.Result, error) {
	var in request
	if err := strict(operation.Input, &in); err != nil {
		return agent.Result{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch operation.Operation {
	case "site_list":
		return agent.Result{Data: encode(append([]Site{}, a.sites...))}, nil
	case "certificate_acme_issue":
		if in.ACME == nil {
			return agent.Result{}, invalid("missing pinned issuance input")
		}
		a.orders[in.ID] = *in.ACME
		a.generation++
		a.refs[in.ID] = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%d", in.ID, a.generation))))
		a.received <- in
		return agent.Result{Data: encode(map[string]any{"state": "pending_dns"})}, nil
	case "certificate_acme_order":
		input, ok := a.orders[in.ID]
		if !ok {
			return agent.Result{}, failure("not_found", "No fixture root order", 404)
		}
		if in.Kind == "status" {
			return agent.Result{Data: encode(map[string]any{"certificate_id": in.ID, "input": input, "order_ref": a.refs[in.ID]})}, nil
		}
		a.received <- in
		return agent.Result{Data: encode(map[string]any{"state": "fixture_recovered"})}, nil
	default:
		return agent.Result{}, agent.ErrUnauthorized
	}
}

type credentialTrackedSecrets struct {
	core.SecretStore
	mu   sync.Mutex
	refs []string
	gets atomic.Int32
}

func (s *credentialTrackedSecrets) Put(ctx context.Context, a core.Actor, resource, name string, value []byte) (string, error) {
	ref, err := s.SecretStore.Put(ctx, a, resource, name, value)
	if err == nil {
		s.mu.Lock()
		s.refs = append(s.refs, ref)
		s.mu.Unlock()
	}
	return ref, err
}
func (s *credentialTrackedSecrets) Get(ctx context.Context, a core.Actor, resource, ref string) ([]byte, error) {
	s.gets.Add(1)
	return s.SecretStore.Get(ctx, a, resource, ref)
}
func (s *credentialTrackedSecrets) latest() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refs[len(s.refs)-1]
}

type credentialFaultState struct {
	core.MetadataStore
	failSettings, failVersion, failSubmissionCommit *atomic.Bool
}

func (s credentialFaultState) CompareAndSwap(ctx context.Context, kind, id string, expected int64, raw json.RawMessage) (int64, error) {
	if kind == "acme_settings" && s.failSettings.Load() || kind == "dns_credential_versions" && s.failVersion.Load() || kind == "acme_submissions" && expected > 0 && s.failSubmissionCommit.Load() {
		return 0, failure("fixture_persistence_failed", "Injected persistence failure", 500)
	}
	return s.MetadataStore.CompareAndSwap(ctx, kind, id, expected, raw)
}
func (s credentialFaultState) Transaction(ctx context.Context, fn func(core.MetadataStore) error) error {
	return s.MetadataStore.Transaction(ctx, func(tx core.MetadataStore) error {
		s.MetadataStore = tx
		return fn(s)
	})
}

type credentialVersionFixture struct {
	service  *core.Service
	actor    core.Actor
	provider *Provider
	agent    *credentialVersionAgent
	secrets  *credentialTrackedSecrets
	state    credentialFaultState
}

func newCredentialVersionFixture(t *testing.T) credentialVersionFixture {
	t.Helper()
	s, err := core.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	setup, err := os.ReadFile(s.SetupSecretFile())
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Setup(context.Background(), strings.TrimSpace(string(setup)), "version-admin", "credential version fixture password")
	if err != nil {
		t.Fatal(err)
	}
	deps := s.Runtime("sites")
	secrets := &credentialTrackedSecrets{SecretStore: deps.Secrets}
	state := credentialFaultState{MetadataStore: deps.State, failSettings: &atomic.Bool{}, failVersion: &atomic.Bool{}, failSubmissionCommit: &atomic.Bool{}}
	deps.State, deps.Secrets = state, secrets
	agentFixture := &credentialVersionAgent{orders: map[string]ACMEInput{}, refs: map[string]string{}, received: make(chan request, 16)}
	p, err := New(deps, Options{Agent: agentFixture})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterProvider(p); err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterExecutor(JobOperationNames(), p); err != nil {
		t.Fatal(err)
	}
	return credentialVersionFixture{s, a, p, agentFixture, secrets, state}
}

func (f credentialVersionFixture) save(t *testing.T, expected int64, token string) {
	t.Helper()
	_, err := f.service.Execute(context.Background(), f.actor, "certificates_dns_credentials_save", encode(map[string]any{
		"id": "version-dns", "name": "Version fixture", "provider": "dns_cf", "expected_revision": expected,
		"credentials": map[string]string{"CF_Token": token},
	}))
	if err != nil {
		t.Fatal(err)
	}
}
func credentialInput(id string) acmePublicInput {
	return acmePublicInput{ACMEInput: ACMEInput{ID: id, Domains: []string{"example.test"}, Email: "fixture@example.test", Challenge: "dns", Provider: "dns_cf", CredentialID: "version-dns"}}
}
func credentialPublicRaw(in acmePublicInput) json.RawMessage {
	var fields map[string]any
	json.Unmarshal(encode(in), &fields)
	delete(fields, "renew") // The action name determines renewal; it is not public input.
	return encode(fields)
}
func credentialAwaitJob(t *testing.T, f credentialVersionFixture, id, status string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		job, err := f.provider.deps.Tasks.Get(ctx, f.actor, id)
		if err != nil {
			t.Fatal(err)
		}
		if job["status"] == status {
			return
		}
		if job["status"] == "failed" || job["status"] == "succeeded" || job["status"] == "canceled" {
			t.Fatalf("unexpected task state %v; wanted %s", job["status"], status)
		}
		select {
		case <-ctx.Done():
			t.Fatal("task did not reach expected terminal state")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

type credentialQueueGate struct {
	entered chan struct{}
	release chan struct{}
}

func (g *credentialQueueGate) Validate(agent.Request) error { return nil }
func (g *credentialQueueGate) Run(ctx context.Context, _ agent.Request) (agent.Result, error) {
	g.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return agent.Result{}, ctx.Err()
	case <-g.release:
		return agent.Result{Data: encode(map[string]any{"done": true})}, nil
	}
}
func credentialHoldFourSlots(t *testing.T, f credentialVersionFixture) func() {
	t.Helper()
	gate := &credentialQueueGate{entered: make(chan struct{}, 4), release: make(chan struct{})}
	if err := f.service.RegisterExecutor([]string{"fixture_credential_hold"}, gate); err != nil {
		t.Fatal(err)
	}
	binding := core.Binding{Action: core.Action{Name: "fixture_credential_hold", Description: "Hold a real durable worker slot", Scope: "certificates:write", Task: true, InputSchema: object(map[string]any{})},
		Resources: func(context.Context, core.Actor, json.RawMessage) ([]string, error) {
			return []string{CertificateResource("hold")}, nil
		},
		Execute: func(ctx context.Context, a core.Actor, _ json.RawMessage) (core.ActionResult, error) {
			job, err := f.provider.deps.Tasks.Submit(ctx, a, "fixture_credential_hold", encode(map[string]any{}), []string{CertificateResource("hold")})
			id, _ := job["id"].(string)
			return core.ActionResult{Data: job, TaskID: id, Resources: []string{CertificateResource("hold")}}, err
		},
	}
	if err := f.service.RegisterProvider(core.Bindings{binding}); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if _, err := f.service.Execute(context.Background(), f.actor, "fixture_credential_hold", encode(map[string]any{})); err != nil {
			t.Fatal(err)
		}
	}
	for range 4 {
		select {
		case <-gate.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("real queue did not occupy all four slots")
		}
	}
	var once sync.Once
	release := func() { once.Do(func() { close(gate.release) }) }
	t.Cleanup(release)
	return release
}

func TestDNSCredentialVersionPinsQueuedIssuanceAndAuthoritativeRecoveryAcrossRotation(t *testing.T) {
	f := newCredentialVersionFixture(t)
	f.save(t, 0, "version-one-token")
	release := credentialHoldFourSlots(t, f)
	input := credentialInput("version-certificate")
	result, err := f.service.Execute(context.Background(), f.actor, "certificates_acme_issue", credentialPublicRaw(input))
	if err != nil {
		t.Fatal(err)
	}
	id := result.(map[string]any)["id"].(string)
	job, err := f.provider.deps.Tasks.Get(context.Background(), f.actor, id)
	if err != nil || job["status"] != "queued" {
		t.Fatal("issuance did not remain queued behind four real tasks", err)
	}
	f.save(t, 1, "version-two-token")
	release()
	credentialAwaitJob(t, f, id, "succeeded")
	issued := <-f.agent.received
	if issued.ACME.CredentialRevision != 1 || issued.Credentials["CF_Token"] != "version-one-token" {
		t.Fatal("queued issuance used a rotated credential instead of its immutable version")
	}
	// Mutable API renewal settings are deliberately replaced. Recovery must use
	// the root order's original version, never this later editable configuration.
	changed := credentialInput("version-certificate").ACMEInput
	changed.CredentialRevision, changed.CredentialID = 2, "different-dns"
	if err := f.state.Save(context.Background(), "acme_settings", input.ID, encode(changed)); err != nil {
		t.Fatal(err)
	}
	result, err = f.service.Execute(context.Background(), f.actor, "certificates_acme_confirm", encode(map[string]any{"id": input.ID}))
	if err != nil {
		t.Fatal(err)
	}
	credentialAwaitJob(t, f, result.(map[string]any)["id"].(string), "succeeded")
	recovered := <-f.agent.received
	if recovered.ACME.CredentialID != "version-dns" || recovered.ACME.CredentialRevision != 1 || recovered.Credentials["CF_Token"] != "version-one-token" {
		t.Fatal("recovery did not resolve the root order's original version")
	}
	versions, err := f.state.List(context.Background(), "dns_credential_versions")
	if err != nil || len(versions) != 2 {
		t.Fatal("immutable version history was not retained", err)
	}
	for _, raw := range versions {
		if strings.Contains(string(raw), "version-one-token") || strings.Contains(string(raw), "version-two-token") {
			t.Fatal("metadata persisted plaintext credentials")
		}
	}
	listed, err := f.service.Execute(context.Background(), f.actor, "certificates_dns_credentials_list", encode(map[string]any{}))
	if err != nil || strings.Contains(string(encode(listed)), "secret_ref") {
		t.Fatal("public listing exposed a secret reference", err)
	}
	explicit := credentialInput("explicit-version-certificate")
	explicit.CredentialRevision = 1
	result, err = f.service.Execute(context.Background(), f.actor, "certificates_acme_issue", credentialPublicRaw(explicit))
	if err != nil {
		t.Fatal(err)
	}
	credentialAwaitJob(t, f, result.(map[string]any)["id"].(string), "succeeded")
	if selected := <-f.agent.received; selected.ACME.CredentialRevision != 1 || selected.Credentials["CF_Token"] != "version-one-token" {
		t.Fatal("an explicitly selected immutable version was replaced by the current version")
	}
	token, err := f.service.CreateToken(context.Background(), f.actor, core.TokenRequest{Name: "certificate-only", Scopes: []string{"certificates:write"}, Resources: []string{CertificateResource(explicit.ID)}})
	if err != nil {
		t.Fatal(err)
	}
	limited, err := f.service.AuthenticateToken(context.Background(), token.Token)
	if err != nil {
		t.Fatal(err)
	}
	before := f.secrets.gets.Load()
	if _, err := f.service.Execute(context.Background(), limited, "certificates_acme_confirm", encode(map[string]any{"id": explicit.ID})); core.ErrorCode(err) != "forbidden" {
		t.Fatal("an old credential revision granted access without its DNS resource", err)
	}
	if f.secrets.gets.Load() != before {
		t.Fatal("resource denial occurred after resolving an old credential")
	}
}

func TestDNSCredentialVersionCASRollbackReclaimsNewSecretAndPreservesOldVersions(t *testing.T) {
	f := newCredentialVersionFixture(t)
	f.save(t, 0, "initial-token")
	old, _, err := f.provider.loadDNSCredential(context.Background(), "version-dns")
	if err != nil {
		t.Fatal(err)
	}
	f.state.failVersion.Store(true)
	_, err = f.service.Execute(context.Background(), f.actor, "certificates_dns_credentials_save", encode(map[string]any{"id": "version-dns", "name": "Version fixture", "provider": "dns_cf", "expected_revision": 1, "credentials": map[string]string{"CF_Token": "failed-rotation-token"}}))
	if core.ErrorCode(err) != "fixture_persistence_failed" {
		t.Fatal("version failure was not propagated", err)
	}
	failedRef := f.secrets.latest()
	current, revision, err := f.provider.loadDNSCredential(context.Background(), "version-dns")
	if err != nil || revision != 1 || current.SecretRef != old.SecretRef {
		t.Fatal("failed version transaction changed the active credential", err)
	}
	if _, err := f.secrets.SecretStore.Get(context.Background(), f.actor, dnsResource("version-dns"), failedRef); core.ErrorCode(err) != "not_found" {
		t.Fatal("failed CAS leaked a new encrypted secret reference", err)
	}
	f.state.failVersion.Store(false)
	f.save(t, 1, "second-token")
	_, err = f.service.Execute(context.Background(), f.actor, "certificates_dns_credentials_save", encode(map[string]any{"id": "version-dns", "name": "stale edit", "provider": "dns_cf", "expected_revision": 1, "credentials": map[string]string{"CF_Token": "stale-token"}}))
	if core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("stale editor replaced the active credential", err)
	}
	if _, err := f.secrets.SecretStore.Get(context.Background(), f.actor, dnsResource("version-dns"), f.secrets.latest()); core.ErrorCode(err) != "not_found" {
		t.Fatal("stale CAS did not reclaim its new secret", err)
	}
	version, err := f.provider.loadDNSCredentialVersion(context.Background(), "version-dns", 1)
	if err != nil || version.SecretRef != old.SecretRef {
		t.Fatal("rotation destroyed the immutable original version", err)
	}
	// Even forged metadata cannot turn a reference into another DNS resource's
	// credential: the production SecretStore checks the envelope binding.
	foreign := version
	foreign.ID = "other-dns"
	if _, err := f.state.CompareAndSwap(context.Background(), "dns_credential_versions", dnsVersionID(foreign.ID, 1), 0, encode(dnsCredentialVersion{foreign, 1})); err != nil {
		t.Fatal(err)
	}
	in := request{ID: "foreign-certificate", ACME: &ACMEInput{ID: "foreign-certificate", Challenge: "dns", Provider: "dns_cf", CredentialID: foreign.ID, CredentialRevision: 1}}
	if err := f.provider.resolveACMEJob(context.Background(), f.actor, &in); core.ErrorCode(err) != "forbidden" {
		t.Fatal("credential version bypassed its resource binding", err)
	}
}

func TestDNSCredentialVersionConcurrentEditorsKeepOneImmutableHeadAndReclaimLoser(t *testing.T) {
	f := newCredentialVersionFixture(t)
	f.save(t, 0, "initial-token")
	start := make(chan struct{})
	errors := make(chan error, 2)
	for _, value := range []string{"editor-one-token", "editor-two-token"} {
		go func() {
			<-start
			_, err := f.service.Execute(context.Background(), f.actor, "certificates_dns_credentials_save", encode(map[string]any{
				"id": "version-dns", "name": "concurrent edit", "provider": "dns_cf", "expected_revision": 1, "credentials": map[string]string{"CF_Token": value},
			}))
			errors <- err
		}()
	}
	close(start)
	succeeded, conflicted := 0, 0
	for range 2 {
		err := <-errors
		if err == nil {
			succeeded++
		} else if core.ErrorCode(err) == "revision_conflict" {
			conflicted++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatal("concurrent current/version transactions admitted two editors")
	}
	current, revision, err := f.provider.loadDNSCredential(context.Background(), "version-dns")
	if err != nil || revision != 2 {
		t.Fatal("concurrent CAS lost the head revision", err)
	}
	version, err := f.provider.loadDNSCredentialVersion(context.Background(), "version-dns", 2)
	if err != nil || version.SecretRef != current.SecretRef {
		t.Fatal("head and immutable version did not commit together", err)
	}
	f.secrets.mu.Lock()
	refs := append([]string(nil), f.secrets.refs...)
	f.secrets.mu.Unlock()
	live, reclaimed := 0, 0
	for _, ref := range refs {
		_, err := f.secrets.SecretStore.Get(context.Background(), f.actor, dnsResource("version-dns"), ref)
		if err == nil {
			live++
		} else if core.ErrorCode(err) == "not_found" {
			reclaimed++
		} else {
			t.Fatal(err)
		}
	}
	if len(refs) != 3 || live != 2 || reclaimed != 1 {
		t.Fatal("a losing concurrent CAS leaked its new secret or deleted a retained version")
	}
}

func TestDNSCredentialVersionQueuedRevocationStopsResolutionBeforeAgent(t *testing.T) {
	f := newCredentialVersionFixture(t)
	f.save(t, 0, "revocation-fixture-token")
	release := credentialHoldFourSlots(t, f)
	input := credentialInput("revoked-certificate")
	token, err := f.service.CreateToken(context.Background(), f.actor, core.TokenRequest{
		Name: "queued-issuance", Scopes: []string{"certificates:write", "tasks:read"},
		Resources: []string{CertificateResource(input.ID), dnsResource(input.CredentialID)},
	})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := f.service.AuthenticateToken(context.Background(), token.Token)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.service.Execute(context.Background(), actor, "certificates_acme_issue", credentialPublicRaw(input))
	if err != nil {
		t.Fatal(err)
	}
	id := result.(map[string]any)["id"].(string)
	before := f.secrets.gets.Load()
	if err := f.service.RevokeToken(context.Background(), f.actor, token.ID); err != nil {
		t.Fatal(err)
	}
	release()
	credentialAwaitJob(t, f, id, "failed")
	if f.secrets.gets.Load() != before {
		t.Fatal("a revoked queued identity resolved an immutable credential")
	}
	select {
	case <-f.agent.received:
		t.Fatal("revoked queued work reached the Agent")
	default:
	}
}

func TestDNSCredentialVersionRecoveryQueueRejectsReplacedOrderBeforeReadingSecrets(t *testing.T) {
	f := newCredentialVersionFixture(t)
	f.save(t, 0, "order-reference-fixture-token")
	input := credentialInput("replaced-order-certificate")
	result, err := f.service.Execute(context.Background(), f.actor, "certificates_acme_issue", credentialPublicRaw(input))
	if err != nil {
		t.Fatal(err)
	}
	credentialAwaitJob(t, f, result.(map[string]any)["id"].(string), "succeeded")
	<-f.agent.received
	release := credentialHoldFourSlots(t, f)
	result, err = f.service.Execute(context.Background(), f.actor, "certificates_acme_cancel", encode(map[string]any{"id": input.ID}))
	if err != nil {
		t.Fatal(err)
	}
	before := f.secrets.gets.Load()
	f.agent.mu.Lock()
	// A distinct order deliberately has exactly the same safe configuration.
	// Only the root's stable opaque order reference distinguishes it.
	f.agent.refs[input.ID] = fmt.Sprintf("%x", sha256.Sum256([]byte("replacement-order-generation")))
	f.agent.mu.Unlock()
	release()
	credentialAwaitJob(t, f, result.(map[string]any)["id"].(string), "failed")
	if f.secrets.gets.Load() != before {
		t.Fatal("an old cancellation resolved credentials for a replacement order")
	}
	select {
	case <-f.agent.received:
		t.Fatal("the old queued cancellation reached the replacement order")
	default:
	}
}

func TestDNSCredentialVersionQueuedCertificateDoesNotAdoptUndeclaredWebsiteReferences(t *testing.T) {
	f := newCredentialVersionFixture(t)
	f.save(t, 0, "reference-fixture-token")
	release := credentialHoldFourSlots(t, f)
	input := credentialInput("new-reference-certificate")
	result, err := f.service.Execute(context.Background(), f.actor, "certificates_acme_issue", credentialPublicRaw(input))
	if err != nil {
		t.Fatal(err)
	}
	before := f.secrets.gets.Load()
	f.agent.mu.Lock()
	f.agent.sites = []Site{{ID: "later-site", TLS: TLSBinding{Enabled: true, CertificateID: input.ID}}}
	f.agent.mu.Unlock()
	release()
	credentialAwaitJob(t, f, result.(map[string]any)["id"].(string), "failed")
	if f.secrets.gets.Load() != before {
		t.Fatal("an undeclared website reference was admitted before credential resolution")
	}
	select {
	case <-f.agent.received:
		t.Fatal("queued issuance reached the Agent with undeclared website references")
	default:
	}
}

func TestACMESubmissionPreparationFailureAndUnknownCommitNeverLoseOrOverwriteSettings(t *testing.T) {
	f := newCredentialVersionFixture(t)
	f.save(t, 0, "initial-token")
	input := credentialInput("prepared-certificate")
	f.state.failSettings.Store(true)
	if _, err := f.service.Execute(context.Background(), f.actor, "certificates_acme_issue", credentialPublicRaw(input)); core.ErrorCode(err) != "fixture_persistence_failed" {
		t.Fatal("settings preparation failure was not propagated", err)
	}
	if _, err := f.state.Get(context.Background(), "acme_settings", input.ID); !isNotFound(err) {
		t.Fatal("failed settings transaction left a partial configuration", err)
	}
	select {
	case <-f.agent.received:
		t.Fatal("a task ran before its settings committed")
	default:
	}
	f.state.failSettings.Store(false)
	release := credentialHoldFourSlots(t, f)
	f.state.failSubmissionCommit.Store(true)
	result, err := f.service.Execute(context.Background(), f.actor, "certificates_acme_issue", credentialPublicRaw(input))
	if err != nil || result.(map[string]any)["submission_metadata_pending"] != true {
		t.Fatal("a real queued job was hidden by a post-submit persistence failure", err)
	}
	id := result.(map[string]any)["id"].(string)
	input.Domains = []string{"changed.example.test"}
	if _, err := f.service.Execute(context.Background(), f.actor, "certificates_acme_issue", credentialPublicRaw(input)); core.ErrorCode(err) != "acme_submission_pending" {
		t.Fatal("unknown submission was replayed or overwritten", err)
	}
	raw, err := f.state.Get(context.Background(), "acme_settings", input.ID)
	var saved ACMEInput
	if err != nil || json.Unmarshal(raw, &saved) != nil || saved.Domains[0] != "example.test" || saved.CredentialRevision != 1 {
		t.Fatal("pending submission lost its original pinned settings", err)
	}
	f.state.failSubmissionCommit.Store(false)
	release()
	credentialAwaitJob(t, f, id, "succeeded")
	issued := <-f.agent.received
	if issued.ACME.Domains[0] != "example.test" {
		t.Fatal("the actual durable job received replacement settings")
	}
}

func TestACMESubmissionKnownPrequeueDenialRollsBackOnlyItsOwnSettings(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			f := newCredentialVersionFixture(t)
			f.save(t, 0, "initial-token")
			input := credentialInput("denied-certificate")
			var previous json.RawMessage
			if existing {
				old := input.ACMEInput
				old.Domains = []string{"original.example.test"}
				previous = encode(old)
				if err := f.state.Save(context.Background(), "acme_settings", input.ID, previous); err != nil {
					t.Fatal(err)
				}
			}
			// Calling Submit without core's authorized-action context is rejected
			// before job creation by the real TaskSubmitter, not a fake scheduler.
			if _, err := f.provider.submitACME(context.Background(), f.actor, "certificates_acme_issue", encode(input)); core.ErrorCode(err) != "forbidden" {
				t.Fatal("core did not reject an unauthorized queue context", err)
			}
			raw, err := f.state.Get(context.Background(), "acme_settings", input.ID)
			if existing {
				if err != nil || string(raw) != string(previous) {
					t.Fatal("prequeue denial failed to restore the previous settings", err)
				}
			} else if !isNotFound(err) {
				t.Fatal("prequeue denial left new settings", err)
			}
			if _, err := f.state.Get(context.Background(), "acme_submissions", input.ID); !isNotFound(err) {
				t.Fatal("proven prequeue denial left a pending submission", err)
			}
		})
	}
}
