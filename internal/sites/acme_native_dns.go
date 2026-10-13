package sites

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
)

func nativeDNSChallengeName(input ACMEInput, domain string) string {
	name := "_acme-challenge." + strings.TrimPrefix(domain, "*.")
	if input.DNSAlias != "" {
		name = input.DNSAlias
		if !strings.HasPrefix(name, "_acme-challenge.") {
			name = "_acme-challenge." + name
		}
	}
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

func (e *AgentExecutor) nativeDNSProvider(in request, input ACMEInput) (legacyDNSProvider, error) {
	apiURLs, explicit := e.config.AllowedDNSAPIURLs[input.Provider]
	if !explicit {
		switch input.Provider {
		case "dns_zilore":
			apiURLs = []string{legacyZiloreAPI}
		case "dns_online":
			apiURLs = []string{legacyOnlineAPI}
		}
	}
	return newLegacyDNSProvider(legacyDNSOptions{
		Provider: input.Provider, Credentials: in.Credentials, OwnerID: input.ID,
		State: e.state, DNSAlias: input.DNSAlias,
		AllowedAPIURLs:    apiURLs,
		AllowedDNSServers: e.config.AllowedDNSUpdateServers[input.Provider],
		ExclusiveTXTSlots: e.config.AllowedDNSExclusiveTXTSlots[input.Provider],
		HTTPClient:        e.nativeDNSHTTPClient, LookupTXT: e.nativeDNSLookup,
	})
}

func (e *AgentExecutor) issueNativeDNSCertificate(ctx context.Context, in request, input ACMEInput) (any, error) {
	if !safeID.MatchString(input.CredentialID) || input.CredentialRevision < 1 {
		return nil, invalid("Native DNS issuance requires a pinned resource-bound credential version")
	}
	provider, err := e.nativeDNSProvider(in, input)
	if err != nil {
		return nil, err
	}
	return e.issueACMEOrder(ctx, in, input, provider)
}

func (e *AgentExecutor) waitNativeDNSTXT(ctx context.Context, name, expected string) error {
	lookup := e.nativeDNSLookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupTXT
	}
	propagationCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for {
		queryCtx, queryCancel := context.WithTimeout(propagationCtx, 10*time.Second)
		values, err := lookup(queryCtx, name)
		queryCancel()
		if err == nil && slices.Contains(values, expected) {
			return nil
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-propagationCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return failure("dns_propagation_failed", "The exact challenge TXT value did not become visible through the DNS resolver", 502)
		case <-timer.C:
		}
	}
}

func (e *AgentExecutor) authorizeNativeDNS(ctx context.Context, client *acme.Client, provider legacyDNSProvider, record dnsChallenge, domain string) error {
	if record.Challenge == nil || record.Challenge.Type != "dns-01" {
		return failure("metadata_invalid", "Stored native DNS challenge is invalid", 500)
	}
	response, err := client.HTTP01ChallengeResponse(record.Challenge.Token)
	if err != nil {
		return err
	}
	if err = provider.Present(ctx, domain, record.Challenge.Token, response); err != nil {
		return err
	}
	if err = e.waitNativeDNSTXT(ctx, record.Name, record.Value); err != nil {
		return err
	}
	challenge := *record.Challenge
	challenge.Payload = nil
	if _, err = client.Accept(ctx, &challenge); err != nil {
		return failure("acme_challenge_failed", "ACME DNS challenge was rejected", 502)
	}
	if _, err = client.WaitAuthorization(ctx, record.AuthorizationURL); err != nil {
		return failure("acme_validation_failed", "ACME DNS validation failed", 502)
	}
	return nil
}

// Cleanup has an independent total budget so cancellation or token revocation
// still releases owned records without prolonging an unbounded network job.
func (e *AgentExecutor) cleanupNativeDNS(client *acme.Client, provider legacyDNSProvider, state acmeOrderState) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var result error
	for _, record := range state.Records {
		if record.Challenge == nil || record.Challenge.Type != "dns-01" {
			result = errors.Join(result, failure("metadata_invalid", "Stored native DNS challenge is invalid", 500))
			continue
		}
		response, err := client.HTTP01ChallengeResponse(record.Challenge.Token)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		domain := strings.TrimPrefix(record.Name, "_acme-challenge.")
		result = errors.Join(result, provider.CleanUp(ctx, domain, record.Challenge.Token, response))
	}
	return result
}

func nativeACMEPublicOrder(state acmeOrderState) any {
	return map[string]any{"certificate_id": state.ID, "state": "recovery_required", "provider": state.Input.Provider,
		"expires_at": state.ExpiresAt, "input": state.Input, "order_ref": acmeOrderRef(state), "confirmation_action": "certificates_acme_confirm", "cancellation_action": "certificates_acme_cancel"}
}

func (e *AgentExecutor) resumeNativeDNSCertificate(ctx context.Context, in request, state acmeOrderState) (result any, resultErr error) {
	if in.Kind == "status" {
		return nativeACMEPublicOrder(state), nil
	}
	if in.Kind != "cancel" && in.Kind != "confirm" {
		return nil, invalid("Unknown native DNS order recovery action")
	}
	if in.ACME == nil || in.ACME.Provider != state.Input.Provider || in.ACME.CredentialID != state.Input.CredentialID || in.ACME.CredentialRevision != state.Input.CredentialRevision || in.ACME.ID != state.ID {
		return nil, failure("dns_recovery_credentials_required", "Resolve the original resource-bound DNS credential before native order recovery", 412)
	}
	if err := validateACME(&state.Input, e.config); err != nil {
		return nil, err
	}
	provider, err := e.nativeDNSProvider(in, state.Input)
	if err != nil {
		return nil, err
	}
	client, err := e.acmeClient(ctx, state.Input)
	if err != nil {
		return nil, err
	}
	if in.Kind == "cancel" || !time.Now().Before(state.ExpiresAt) {
		if err = e.cleanupNativeDNS(client, provider, state); err != nil {
			return nativeACMEPublicOrder(state), failure("dns_cleanup_required", "Owned DNS records could not be removed; recovery state was retained", 502)
		}
		if err = e.state.Remove(filepath.Join("acme", state.ID+".order.json")); err != nil {
			return nil, err
		}
		return map[string]any{"certificate_id": state.ID, "state": "cancelled"}, nil
	}
	cleanupAttempted := false
	defer func() {
		if cleanupAttempted {
			return
		}
		if err := e.cleanupNativeDNS(client, provider, state); err != nil {
			result = nativeACMEPublicOrder(state)
			resultErr = errors.Join(resultErr, failure("dns_cleanup_required", "Owned DNS records require cleanup; recovery state was retained", 502))
		}
	}()
	for _, record := range state.Records {
		authorization, err := client.GetAuthorization(ctx, record.AuthorizationURL)
		if err != nil {
			return nil, failure("acme_authorization_failed", "ACME authorization could not be read", 502)
		}
		if authorization.Status == acme.StatusValid {
			continue
		}
		if err = e.authorizeNativeDNS(ctx, client, provider, record, authorization.Identifier.Value); err != nil {
			return nil, err
		}
	}
	cleanupAttempted = true
	if err = e.cleanupNativeDNS(client, provider, state); err != nil {
		return nativeACMEPublicOrder(state), failure("dns_cleanup_required", "Owned DNS records require cleanup before certificate publication", 502)
	}
	in.ExpectedRevision, in.ACME = state.ExpectedRevision, &state.Input
	return e.finalizeACME(ctx, client, state, in)
}
