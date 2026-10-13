package sites

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

type acmePublicInput struct {
	ACMEInput
	ExpectedRevision int64 `json:"expected_revision"`
}

func dnsResource(id string) string { return "dns:" + id }
func (p *Provider) acmeBindings() []core.Binding {
	fields := map[string]any{"id": idField(), "expected_revision": number(0, 1<<30), "site_id": text(96), "domains": array(text(253), 100), "email": text(254), "challenge": choice("http_webroot", "manual_dns", "dns"), "provider": text(96), "credential_id": text(96), "credential_revision": number(0, 1<<30), "server": text(2048), "force": boolean(), "dns_alias": text(253)}
	bindings := []core.Binding{}
	for _, name := range []string{"certificates_acme_issue", "certificates_acme_renew"} {
		bindings = append(bindings, core.Binding{Action: core.Action{Name: name, Description: "Issue or renew an actual ACME certificate; manual DNS returns pending records until explicit confirmation", Scope: "certificates:write", Task: true, InputSchema: object(fields, "id", "expected_revision")}, Resources: p.acmeResources, Execute: func(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			return p.submitACME(ctx, a, name, raw)
		}})
	}
	for _, spec := range []struct {
		name, kind, scope string
		read, task        bool
	}{{"certificates_acme_status", "status", "certificates:read", true, false}, {"certificates_acme_confirm", "confirm", "certificates:write", false, true}, {"certificates_acme_cancel", "cancel", "certificates:write", false, true}} {
		resources := certResource
		if !spec.read {
			resources = p.acmeRecoveryResources
		}
		bindings = append(bindings, core.Binding{Action: core.Action{Name: spec.name, Description: "Inspect, confirm or cancel a persisted DNS ACME order without exposing account or private keys", Scope: spec.scope, ReadOnly: spec.read, Task: spec.task, InputSchema: object(map[string]any{"id": idField()}, "id")}, Resources: resources, Execute: func(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			var in struct {
				ID string `json:"id"`
			}
			if err := strict(raw, &in); err != nil {
				return core.ActionResult{}, err
			}
			request := request{ID: in.ID, Kind: spec.kind, Actor: a.Public()}
			resources := []string{CertificateResource(in.ID)}
			if spec.task {
				input, orderRef, err := p.recoveryACMEState(ctx, a, in.ID)
				if err != nil {
					return core.ActionResult{}, err
				}
				resources, err = p.acmeResources(ctx, a, encode(acmePublicInput{ACMEInput: input}))
				if err != nil {
					return core.ActionResult{}, err
				}
				request.ACME = &input
				request.OrderRef = orderRef
				request.AuthorizedSites = acmeDeclaredSites(resources)
				job, err := p.deps.Tasks.Submit(ctx, a, "certificate_acme_order", encode(request), resources)
				id, _ := job["id"].(string)
				return core.ActionResult{Data: job, Resources: resources, TaskID: id}, err
			}
			var out any
			err := p.call(ctx, a, "certificate_acme_order", request, &out)
			return core.ActionResult{Data: out, Resources: resources}, err
		}})
	}
	bindings = append(bindings, core.Binding{Action: core.Action{Name: "certificates_dns_providers", Description: "Discover reviewed DNS credential adapters and their actual availability", Scope: "certificates:read", ReadOnly: true, InputSchema: object(map[string]any{})}, Resources: noResources, Execute: func(context.Context, core.Actor, json.RawMessage) (core.ActionResult, error) {
		return core.ActionResult{Data: map[string]any{"items": DNSProviders()}}, nil
	}})
	for _, name := range []string{"certificates_dns_credentials_list", "certificates_dns_credentials_save", "certificates_dns_credentials_delete"} {
		read := strings.HasSuffix(name, "list")
		scope := "certificates:write"
		schema := object(map[string]any{"id": idField(), "expected_revision": number(0, 1<<30)}, "id", "expected_revision")
		resources := func(ctx context.Context, a core.Actor, raw json.RawMessage) ([]string, error) {
			var in struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &in) != nil || !safeID.MatchString(in.ID) {
				return nil, invalid("A DNS credential ID is required")
			}
			return []string{dnsResource(in.ID)}, nil
		}
		if read {
			scope = "certificates:read"
			schema = object(map[string]any{})
			resources = noResources
		}
		if strings.HasSuffix(name, "save") {
			schema = object(map[string]any{"id": idField(), "name": text(160), "provider": text(96), "expected_revision": number(0, 1<<30), "credentials": map[string]any{"type": "object", "maxProperties": 32, "additionalProperties": text(64 << 10)}}, "id", "name", "provider", "expected_revision")
		}
		bindings = append(bindings, core.Binding{Action: core.Action{Name: name, Description: "Manage resource-bound encrypted DNS credentials; list responses contain no secrets or secret references", Scope: scope, ReadOnly: read, InputSchema: schema}, Resources: resources, Execute: func(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			return p.dnsCredentialsAction(ctx, a, name, raw)
		}})
	}
	return bindings
}
func (p *Provider) loadDNSCredential(ctx context.Context, id string) (DNSCredential, int64, error) {
	raw, revision, err := p.deps.State.GetVersioned(ctx, "dns_credentials", id)
	var value DNSCredential
	if err != nil {
		return value, 0, err
	}
	if json.Unmarshal(raw, &value) != nil || value.ID != id || value.SecretRef == "" {
		return value, 0, failure("metadata_invalid", "DNS credential needs recovery inspection", 500)
	}
	return value, revision, nil
}
func publicDNSCredential(value DNSCredential, revision int64) any {
	return map[string]any{"id": value.ID, "name": value.Name, "provider": value.Provider, "revision": revision, "updated_at": value.UpdatedAt, "credential_configured": value.SecretRef != ""}
}

// Versions contain only resource-bound encrypted references. Their CAS-at-zero
// key is immutable even when the current credential's provider or name changes.
type dnsCredentialVersion struct {
	DNSCredential
	Revision int64 `json:"revision"`
}

func dnsVersionID(id string, revision int64) string { return fmt.Sprintf("%s@%d", id, revision) }

func ensureDNSCredentialVersion(ctx context.Context, store core.MetadataStore, value DNSCredential, revision int64) error {
	key := dnsVersionID(value.ID, revision)
	raw, _, err := store.GetVersioned(ctx, "dns_credential_versions", key)
	version := dnsCredentialVersion{DNSCredential: value, Revision: revision}
	if isNotFound(err) {
		_, err = store.CompareAndSwap(ctx, "dns_credential_versions", key, 0, encode(version))
		return err
	}
	if err != nil {
		return err
	}
	var saved dnsCredentialVersion
	if json.Unmarshal(raw, &saved) != nil || saved != version {
		return failure("metadata_invalid", "Immutable DNS credential version does not match its current reference", 500)
	}
	return nil
}

func (p *Provider) loadDNSCredentialVersion(ctx context.Context, id string, revision int64) (DNSCredential, error) {
	if !safeID.MatchString(id) || revision <= 0 {
		return DNSCredential{}, invalid("A pinned DNS credential revision is required")
	}
	raw, err := p.deps.State.Get(ctx, "dns_credential_versions", dnsVersionID(id, revision))
	if isNotFound(err) {
		// Adopt a pre-versioning current record only at its exact revision. Old
		// revisions cannot be reconstructed from newly rotated credentials.
		value, current, loadErr := p.loadDNSCredential(ctx, id)
		if loadErr != nil {
			return DNSCredential{}, loadErr
		}
		if current != revision {
			return DNSCredential{}, failure("credential_version_unavailable", "The original DNS credential version needs recovery inspection", 412)
		}
		err = p.deps.State.Transaction(ctx, func(store core.MetadataStore) error {
			currentRaw, currentRevision, err := store.GetVersioned(ctx, "dns_credentials", id)
			if err != nil {
				return err
			}
			if currentRevision != revision || string(currentRaw) != string(encode(value)) {
				return failure("revision_conflict", "DNS credential changed while its original version was pinned", 412)
			}
			return ensureDNSCredentialVersion(ctx, store, value, revision)
		})
		return value, err
	}
	if err != nil {
		return DNSCredential{}, err
	}
	var value dnsCredentialVersion
	if json.Unmarshal(raw, &value) != nil || value.ID != id || value.Revision != revision || value.Provider == "" || value.SecretRef == "" {
		return DNSCredential{}, failure("metadata_invalid", "DNS credential version needs recovery inspection", 500)
	}
	return value.DNSCredential, nil
}

func (p *Provider) pinACMECredential(ctx context.Context, a core.Actor, input *ACMEInput) error {
	if input.Challenge != "dns" {
		return nil
	}
	if !safeID.MatchString(input.CredentialID) || !p.deps.Authorize(a, "certificates:write", dnsResource(input.CredentialID)) {
		return denied()
	}
	if input.CredentialRevision == 0 {
		value, revision, err := p.loadDNSCredential(ctx, input.CredentialID)
		if err != nil {
			return err
		}
		if value.Provider != input.Provider {
			return invalid("DNS credential provider does not match the selected adapter")
		}
		input.CredentialRevision = revision
	}
	value, err := p.loadDNSCredentialVersion(ctx, input.CredentialID, input.CredentialRevision)
	if err != nil {
		return err
	}
	if value.Provider != input.Provider {
		return invalid("Pinned DNS credential provider does not match the selected adapter")
	}
	return nil
}
func (p *Provider) dnsCredentialsAction(ctx context.Context, a core.Actor, name string, raw json.RawMessage) (core.ActionResult, error) {
	if strings.HasSuffix(name, "list") {
		values, err := p.deps.State.List(ctx, "dns_credentials")
		if err != nil {
			return core.ActionResult{}, err
		}
		items := []any{}
		for id := range values {
			if !p.deps.Authorize(a, "certificates:read", dnsResource(id)) {
				continue
			}
			value, revision, err := p.loadDNSCredential(ctx, id)
			if err != nil {
				return core.ActionResult{}, err
			}
			items = append(items, publicDNSCredential(value, revision))
		}
		return core.ActionResult{Data: map[string]any{"items": items, "total": len(items)}}, nil
	}
	var in struct {
		ID          string            `json:"id"`
		Name        string            `json:"name"`
		Provider    string            `json:"provider"`
		Expected    int64             `json:"expected_revision"`
		Credentials map[string]string `json:"credentials"`
	}
	if err := strict(raw, &in); err != nil {
		return core.ActionResult{}, err
	}
	resource := dnsResource(in.ID)
	if strings.HasSuffix(name, "delete") {
		value, revision, err := p.loadDNSCredential(ctx, in.ID)
		if err != nil {
			return core.ActionResult{}, err
		}
		if revision != in.Expected {
			return core.ActionResult{}, failure("conflict", "DNS credential revision changed", 409)
		}
		refs := []string{value.SecretRef}
		err = p.deps.State.Transaction(ctx, func(store core.MetadataStore) error {
			settings, err := store.List(ctx, "acme_settings")
			if err != nil {
				return err
			}
			for _, raw := range settings {
				var input ACMEInput
				if json.Unmarshal(raw, &input) != nil {
					return failure("metadata_invalid", "ACME settings need recovery inspection", 500)
				}
				if input.CredentialID == in.ID {
					return failure("credential_in_use", "Remove certificate renewal references before deleting this DNS credential", 409)
				}
			}
			_, current, err := store.GetVersioned(ctx, "dns_credentials", in.ID)
			if err != nil {
				return err
			}
			if current != in.Expected {
				return failure("conflict", "DNS credential revision changed", 409)
			}
			versions, err := store.List(ctx, "dns_credential_versions")
			if err != nil {
				return err
			}
			for key, raw := range versions {
				if !strings.HasPrefix(key, in.ID+"@") {
					continue
				}
				var version dnsCredentialVersion
				if json.Unmarshal(raw, &version) != nil || version.ID != in.ID || key != dnsVersionID(in.ID, version.Revision) || version.SecretRef == "" {
					return failure("metadata_invalid", "DNS credential history needs recovery inspection", 500)
				}
				if !slices.Contains(refs, version.SecretRef) {
					refs = append(refs, version.SecretRef)
				}
				if err := store.Delete(ctx, "dns_credential_versions", key); err != nil {
					return err
				}
			}
			return store.Delete(ctx, "dns_credentials", in.ID)
		})
		if err == nil {
			for _, ref := range refs {
				err = errors.Join(err, p.deps.Secrets.Delete(ctx, a, resource, ref))
			}
		}
		return core.ActionResult{Data: map[string]any{"deleted": err == nil, "id": in.ID}, Resources: []string{resource}}, err
	}
	if strings.TrimSpace(in.Name) == "" {
		return core.ActionResult{}, invalid("DNS credential name is required")
	}
	value := DNSCredential{ID: in.ID, Name: strings.TrimSpace(in.Name), Provider: in.Provider, UpdatedAt: time.Now().UTC()}
	newRef := ""
	if len(in.Credentials) == 0 {
		old, revision, err := p.loadDNSCredential(ctx, in.ID)
		if err != nil {
			return core.ActionResult{}, err
		}
		if revision != in.Expected || old.Provider != in.Provider {
			return core.ActionResult{}, invalid("Changing the DNS provider requires new credentials and its current revision")
		}
		value.SecretRef = old.SecretRef
	} else {
		if err := ValidateDNSCredentials(in.Provider, in.Credentials); err != nil {
			return core.ActionResult{}, err
		}
		ref, err := p.deps.Secrets.Put(ctx, a, resource, "dns-provider-credentials", encode(in.Credentials))
		if err != nil {
			return core.ActionResult{}, err
		}
		value.SecretRef = ref
		newRef = ref
	}
	var revision int64
	err := p.deps.State.Transaction(ctx, func(store core.MetadataStore) error {
		if in.Expected > 0 {
			raw, oldRevision, err := store.GetVersioned(ctx, "dns_credentials", in.ID)
			if err != nil {
				return err
			}
			if oldRevision != in.Expected {
				return failure("revision_conflict", "DNS credential revision changed", 412)
			}
			var old DNSCredential
			if json.Unmarshal(raw, &old) != nil || old.ID != in.ID || old.SecretRef == "" {
				return failure("metadata_invalid", "DNS credential needs recovery inspection", 500)
			}
			if err = ensureDNSCredentialVersion(ctx, store, old, oldRevision); err != nil {
				return err
			}
		}
		var err error
		revision, err = store.CompareAndSwap(ctx, "dns_credentials", in.ID, in.Expected, encode(value))
		if err != nil {
			return err
		}
		return ensureDNSCredentialVersion(ctx, store, value, revision)
	})
	if err != nil && newRef != "" {
		// Cleanup must still run when the request deadline expired after Put.
		// SecretStore rechecks the actor; this never bypasses a revocation.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		err = errors.Join(err, p.deps.Secrets.Delete(cleanupCtx, a, resource, newRef))
	}
	return core.ActionResult{Data: publicDNSCredential(value, revision), Resources: []string{resource}}, err
}
func (p *Provider) effectiveACME(ctx context.Context, raw json.RawMessage) (acmePublicInput, error) {
	var in acmePublicInput
	if err := strict(raw, &in); err != nil {
		return in, err
	}
	if len(in.Domains) == 0 || in.Email == "" || in.Challenge == "" {
		previous, err := p.deps.State.Get(ctx, "acme_settings", in.ID)
		if err != nil {
			return in, err
		}
		var saved ACMEInput
		if json.Unmarshal(previous, &saved) != nil {
			return in, failure("metadata_invalid", "ACME renewal settings need recovery inspection", 500)
		}
		// Renewal uses a complete saved configuration. Partial replacements must
		// supply all issuance fields and are validated as a new configuration.
		if len(in.Domains) > 0 || in.Email != "" || in.Challenge != "" {
			return in, invalid("Provide all ACME issuance fields or only the certificate ID for renewal")
		}
		force, credentialRevision := in.Force, in.CredentialRevision
		in.ACMEInput = saved
		// A new renewal pins the current version. An existing root order is
		// recovered separately from its authoritative, already pinned input.
		in.CredentialRevision = credentialRevision
		in.Force = force
		in.Renew = true
	}
	return in, nil
}
func (p *Provider) acmeResources(ctx context.Context, a core.Actor, raw json.RawMessage) ([]string, error) {
	var identity struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &identity) != nil || !safeID.MatchString(identity.ID) {
		return nil, invalid("A certificate identity is required")
	}
	if !p.deps.Authorize(a, "certificates:write", CertificateResource(identity.ID)) {
		return nil, denied()
	}
	in, err := p.effectiveACME(ctx, raw)
	if err != nil {
		return nil, err
	}
	resources := []string{CertificateResource(in.ID)}
	if in.SiteID != "" {
		if !p.deps.Authorize(a, "sites:write", Resource(in.SiteID)) {
			return nil, denied()
		}
		resources = append(resources, Resource(in.SiteID))
	}
	if in.Challenge == "dns" {
		if err := p.pinACMECredential(ctx, a, &in.ACMEInput); err != nil {
			return nil, err
		}
		resources = append(resources, dnsResource(in.CredentialID))
	}
	sites, err := p.certificateSites(ctx, a, in.ID)
	if err != nil {
		return nil, err
	}
	for _, id := range sites {
		resources = append(resources, Resource(id))
	}
	return resources, nil
}

// Until core offers one SQLite transaction for metadata preparation and durable
// job creation, an unresolved preparation is a fail-closed submission receipt.
// It is never an assertion that a process-crash window was replayed safely.
type acmeSubmission struct {
	Input            ACMEInput `json:"input"`
	ExpectedRevision int64     `json:"expected_revision"`
	ActorID          string    `json:"actor_id"`
	JobID            string    `json:"job_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

var acmeSubmissionLocks certificateLocks

func acmeDeclaredSites(resources []string) []string {
	sites := []string{}
	for _, resource := range resources {
		if strings.HasPrefix(resource, "site:") {
			sites = append(sites, strings.TrimPrefix(resource, "site:"))
		}
	}
	return sites
}

func (p *Provider) checkACMESubmission(ctx context.Context, a core.Actor, id string) (int64, error) {
	var order map[string]json.RawMessage
	if err := p.call(ctx, a, "certificate_acme_order", request{ID: id, Kind: "status"}, &order); err == nil {
		return 0, failure("order_exists", "Confirm or cancel the existing ACME order before changing its settings", 409)
	} else if !isNotFound(err) {
		return 0, err
	}
	raw, revision, err := p.deps.State.GetVersioned(ctx, "acme_submissions", id)
	if isNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var previous acmeSubmission
	if json.Unmarshal(raw, &previous) != nil || previous.Input.ID != id || previous.ActorID == "" {
		return 0, failure("metadata_invalid", "ACME submission state needs recovery inspection", 500)
	}
	if previous.JobID == "" {
		return 0, failure("acme_submission_pending", "The previous task submission has an unresolved receipt; settings were retained without replay", 409)
	}
	job, err := p.deps.Tasks.Get(ctx, a, previous.JobID)
	if err != nil {
		return 0, err
	}
	status, _ := job["status"].(string)
	if !slices.Contains([]string{"succeeded", "failed", "canceled", "interrupted"}, status) {
		return 0, failure("acme_submission_pending", "The previous ACME task is still queued or running; its settings were retained", 409)
	}
	return revision, nil
}

func (p *Provider) submitACME(ctx context.Context, a core.Actor, name string, raw json.RawMessage) (core.ActionResult, error) {
	var identity struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &identity) != nil || !safeID.MatchString(identity.ID) {
		return core.ActionResult{}, invalid("A certificate identity is required")
	}
	if !p.deps.Authorize(a, "certificates:write", CertificateResource(identity.ID)) {
		return core.ActionResult{}, denied()
	}
	unlock, err := acmeSubmissionLocks.acquire(ctx, identity.ID)
	if err != nil {
		return core.ActionResult{}, err
	}
	defer unlock()
	in, err := p.effectiveACME(ctx, raw)
	if err != nil {
		return core.ActionResult{}, err
	}
	in.Renew = name == "certificates_acme_renew"
	resources, err := p.acmeResources(ctx, a, raw)
	if err != nil {
		return core.ActionResult{}, err
	}
	if err = p.pinACMECredential(ctx, a, &in.ACMEInput); err != nil {
		return core.ActionResult{}, err
	}
	submissionRevision, err := p.checkACMESubmission(ctx, a, in.ID)
	if err != nil {
		return core.ActionResult{}, err
	}
	prepared := acmeSubmission{Input: in.ACMEInput, ExpectedRevision: in.ExpectedRevision, ActorID: a.ID, CreatedAt: time.Now().UTC()}
	var previous json.RawMessage
	var settingsRevision int64
	err = p.deps.State.Transaction(ctx, func(store core.MetadataStore) error {
		var revision int64
		var err error
		previous, revision, err = store.GetVersioned(ctx, "acme_settings", in.ID)
		if err != nil && !isNotFound(err) {
			return err
		}
		settingsRevision, err = store.CompareAndSwap(ctx, "acme_settings", in.ID, revision, encode(in.ACMEInput))
		if err != nil {
			return err
		}
		submissionRevision, err = store.CompareAndSwap(ctx, "acme_submissions", in.ID, submissionRevision, encode(prepared))
		return err
	})
	if err != nil {
		return core.ActionResult{}, err
	}
	job, err := p.deps.Tasks.Submit(ctx, a, "certificate_acme_issue", encode(request{ID: in.ID, ExpectedRevision: in.ExpectedRevision, ACME: &in.ACMEInput, AuthorizedSites: acmeDeclaredSites(resources), Actor: a.Public()}), resources)
	if err != nil {
		// These codes are returned by core before calling jobs.Submit. A generic
		// task_rejected may reflect an uncertain persistence error, so it must
		// keep the preparation rather than pretending no job was created.
		if slices.Contains([]string{"forbidden", "unauthorized", "invalid_input"}, core.ErrorCode(err)) {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			rollbackErr := p.deps.State.Transaction(cleanupCtx, func(store core.MetadataStore) error {
				current, revision, e := store.GetVersioned(cleanupCtx, "acme_submissions", in.ID)
				if e != nil {
					return e
				}
				if revision != submissionRevision || string(current) != string(encode(prepared)) {
					return failure("revision_conflict", "ACME preparation changed; rollback left it untouched", 412)
				}
				current, revision, e = store.GetVersioned(cleanupCtx, "acme_settings", in.ID)
				if e != nil {
					return e
				}
				if revision != settingsRevision || string(current) != string(encode(in.ACMEInput)) {
					return failure("revision_conflict", "ACME settings changed; rollback left them untouched", 412)
				}
				if previous == nil {
					e = store.Delete(cleanupCtx, "acme_settings", in.ID)
				} else {
					_, e = store.CompareAndSwap(cleanupCtx, "acme_settings", in.ID, revision, previous)
				}
				if e != nil {
					return e
				}
				return store.Delete(cleanupCtx, "acme_submissions", in.ID)
			})
			err = errors.Join(err, rollbackErr)
		}
		return core.ActionResult{}, err
	}
	id, _ := job["id"].(string)
	if id == "" {
		return core.ActionResult{}, failure("acme_submission_pending", "Task submission returned no durable identity; its preparation was retained", 502)
	}
	prepared.JobID = id
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, err = p.deps.State.CompareAndSwap(commitCtx, "acme_submissions", in.ID, submissionRevision, encode(prepared))
	if err != nil {
		// A real job already exists. Preserve its response instead of making
		// the client retry and obscure the job behind an HTTP failure.
		job["submission_metadata_pending"] = true
	}
	return core.ActionResult{Data: job, TaskID: id, Resources: resources, AuditSummary: map[string]any{"submission_metadata_pending": err != nil}}, nil
}
func (p *Provider) resolveACMEJob(ctx context.Context, a core.Actor, in *request) error {
	if !p.deps.Authorize(a, "certificates:write", CertificateResource(in.ID)) {
		if in.ACME != nil || in.Kind == "confirm" || in.Kind == "cancel" {
			return denied()
		}
		return nil
	}
	if in.Kind == "confirm" || in.Kind == "cancel" {
		input, orderRef, err := p.recoveryACMEState(ctx, a, in.ID)
		if err != nil {
			return err
		}
		if in.OrderRef == "" || in.OrderRef != orderRef || in.ACME == nil || string(encode(input)) != string(encode(in.ACME)) {
			return failure("acme_order_changed", "The root ACME order changed after recovery was queued; nothing was resolved or applied", 412)
		}
		in.ACME = &input
	}
	if in.ACME == nil {
		return nil
	}
	if in.ACME.SiteID != "" && !p.deps.Authorize(a, "sites:write", Resource(in.ACME.SiteID)) {
		return denied()
	}
	sites, err := p.certificateSites(ctx, a, in.ID)
	if err != nil {
		return err
	}
	for _, id := range sites {
		if !slices.Contains(in.AuthorizedSites, id) {
			return failure("site_reference_changed", "The certificate's website references changed after task submission; submit a fresh authorized task", 412)
		}
	}
	in.AuthorizedSites = sites
	if in.ACME.Challenge != "dns" {
		return nil
	}
	id := in.ACME.CredentialID
	if !p.deps.Authorize(a, "certificates:write", dnsResource(id)) {
		return denied()
	}
	value, err := p.loadDNSCredentialVersion(ctx, id, in.ACME.CredentialRevision)
	if err != nil {
		return err
	}
	if value.Provider != in.ACME.Provider {
		return invalid("Queued DNS credential version does not match the pinned provider")
	}
	secret, err := p.deps.Secrets.Get(ctx, a, dnsResource(id), value.SecretRef)
	if err != nil {
		return err
	}
	if json.Unmarshal(secret, &in.Credentials) != nil {
		return failure("secret_invalid", "Stored DNS credential data is invalid", 500)
	}
	return ValidateDNSCredentials(value.Provider, in.Credentials)
}

func (p *Provider) recoveryACMEState(ctx context.Context, a core.Actor, id string) (ACMEInput, string, error) {
	if !safeID.MatchString(id) || !p.deps.Authorize(a, "certificates:write", CertificateResource(id)) {
		return ACMEInput{}, "", denied()
	}
	var status struct {
		CertificateID string     `json:"certificate_id"`
		Input         *ACMEInput `json:"input"`
		OrderRef      string     `json:"order_ref"`
	}
	if err := p.call(ctx, a, "certificate_acme_order", request{ID: id, Kind: "status"}, &status); err != nil {
		return ACMEInput{}, "", err
	}
	if status.CertificateID != id || status.Input == nil || status.Input.ID != id || len(status.OrderRef) != 64 || strings.Trim(status.OrderRef, "0123456789abcdef") != "" {
		return ACMEInput{}, "", failure("metadata_invalid", "Root ACME recovery identity or input is unavailable or inconsistent", 500)
	}
	if status.Input.Challenge == "dns" && status.Input.CredentialRevision <= 0 {
		return ACMEInput{}, "", failure("credential_version_unavailable", "The pending DNS order predates immutable credential versions; inspect its original account before recovery", 412)
	}
	return *status.Input, status.OrderRef, nil
}

func (p *Provider) acmeRecoveryResources(ctx context.Context, a core.Actor, raw json.RawMessage) ([]string, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := strict(raw, &in); err != nil || !safeID.MatchString(in.ID) {
		return nil, invalid("A certificate identity is required")
	}
	if !p.deps.Authorize(a, "certificates:write", CertificateResource(in.ID)) {
		return nil, denied()
	}
	input, _, err := p.recoveryACMEState(ctx, a, in.ID)
	if err != nil {
		return nil, err
	}
	return p.acmeResources(ctx, a, encode(acmePublicInput{ACMEInput: input}))
}
func (p *Provider) certificateSites(ctx context.Context, a core.Actor, id string) ([]string, error) {
	var sites []Site
	if err := p.call(ctx, a, "site_list", request{}, &sites); err != nil {
		return nil, err
	}
	refs := []string{}
	for _, site := range sites {
		if site.TLS.Enabled && site.TLS.CertificateID == id {
			if !p.deps.Authorize(a, "sites:write", Resource(site.ID)) {
				return nil, denied()
			}
			refs = append(refs, site.ID)
		}
	}
	return refs, nil
}
