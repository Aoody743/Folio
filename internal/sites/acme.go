package sites

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/acme"
)

const productionACMEDirectory = "https://acme-v02.api.letsencrypt.org/directory"

type dnsChallenge struct {
	Name             string          `json:"name"`
	Type             string          `json:"type"`
	Value            string          `json:"value"`
	AuthorizationURL string          `json:"authorization_url"`
	Challenge        *acme.Challenge `json:"challenge"`
}
type httpACMEChallenge struct {
	Domain           string          `json:"domain"`
	URL              string          `json:"url"`
	Path             string          `json:"path"`
	Body             string          `json:"body"`
	AuthorizationURL string          `json:"authorization_url"`
	Challenge        *acme.Challenge `json:"challenge"`
}
type acmeOrderState struct {
	Version          int                 `json:"version"`
	ID               string              `json:"id"`
	Input            ACMEInput           `json:"input"`
	Order            *acme.Order         `json:"order"`
	Records          []dnsChallenge      `json:"records"`
	HTTPChallenges   []httpACMEChallenge `json:"http_challenges,omitempty"`
	KeyPEM           string              `json:"key_pem"`
	ExpectedRevision int64               `json:"expected_revision"`
	CreatedBy        string              `json:"created_by"`
	CreatedAt        time.Time           `json:"created_at"`
	ExpiresAt        time.Time           `json:"expires_at"`
}

func validateACME(in *ACMEInput, config AgentConfig) error {
	if in == nil || !safeID.MatchString(in.ID) || len(in.Domains) < 1 || len(in.Domains) > 100 {
		return invalid("ACME requires a certificate ID and one to 100 DNS names")
	}
	address, err := mail.ParseAddress(in.Email)
	if err != nil || address.Address != in.Email || len(in.Email) > 254 {
		return invalid("ACME requires a valid contact email")
	}
	if in.Challenge != "http_webroot" && in.Challenge != "manual_http" && in.Challenge != "manual_dns" && in.Challenge != "dns" {
		return invalid("Unknown ACME challenge method")
	}
	seen := map[string]bool{}
	for index, value := range in.Domains {
		canonical, host, port, err := domain(value)
		if err != nil || port != 80 || strings.Contains(value, ":") || net.ParseIP(host) != nil || !strings.Contains(host, ".") || seen[canonical] {
			return invalid("ACME identifiers must be distinct DNS names without ports")
		}
		if strings.HasPrefix(host, "*.") && (in.Challenge == "http_webroot" || in.Challenge == "manual_http") {
			return invalid("Wildcard certificates require DNS validation")
		}
		in.Domains[index] = canonical
		seen[canonical] = true
	}
	if in.Challenge == "http_webroot" && !safeID.MatchString(in.SiteID) {
		return invalid("HTTP validation requires an existing owned website")
	}
	if in.Challenge == "manual_http" && (in.SiteID != "" || in.DNSAlias != "" || in.Provider != "" || in.CredentialID != "" || in.CredentialRevision != 0) {
		return invalid("Manual HTTP validation does not use a website or DNS credential")
	}
	if in.Server == "" {
		in.Server = productionACMEDirectory
	}
	u, err := url.Parse(in.Server)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return invalid("ACME directory must be an HTTPS endpoint")
	}
	allowed := config.AllowedACMEServers
	if len(allowed) == 0 {
		allowed = []string{productionACMEDirectory, "https://acme-staging-v02.api.letsencrypt.org/directory"}
	}
	if !slices.Contains(allowed, in.Server) {
		return failure("policy_denied", "ACME directory is outside the trusted Agent policy", 403)
	}
	if in.DNSAlias != "" {
		_, host, port, err := domain(in.DNSAlias)
		if err != nil || port != 80 || net.ParseIP(host) != nil || strings.HasPrefix(host, "*.") {
			return invalid("Invalid DNS challenge alias")
		}
	}
	return nil
}

func parsePrivateKey(raw []byte) (crypto.Signer, error) {
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("invalid private ACME key record")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, errors.New("invalid ACME signer")
	}
	return signer, nil
}
func generatePrivateKey() (crypto.Signer, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}
func (e *AgentExecutor) acmeClient(ctx context.Context, in ACMEInput) (*acme.Client, error) {
	unlock, err := e.acmeAccounts.acquire(ctx, in.Server+"\x00"+in.Email)
	if err != nil {
		return nil, err
	}
	defer unlock()
	digest := sha256.Sum256([]byte(in.Server + "\x00" + in.Email))
	path := filepath.Join("acme", "account-"+hex.EncodeToString(digest[:])+".pem")
	raw, err := e.state.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		_, raw, err = generatePrivateKey()
		if err == nil {
			err = atomicRoot(e.state, path, raw, 0600)
		}
	}
	if err != nil {
		return nil, err
	}
	key, err := parsePrivateKey(raw)
	if err != nil {
		return nil, err
	}
	client := &acme.Client{Key: key, DirectoryURL: in.Server, HTTPClient: &http.Client{Timeout: 30 * time.Second}, UserAgent: "Folio"}
	if e.acmeHTTPClient != nil {
		client.HTTPClient = e.acmeHTTPClient
	}
	return client, nil
}
func acmePublicOrder(order acmeOrderState) any {
	if order.Input.Challenge == "manual_http" {
		challenges := make([]map[string]string, 0, len(order.HTTPChallenges))
		for _, challenge := range order.HTTPChallenges {
			challenges = append(challenges, map[string]string{"domain": challenge.Domain, "url": challenge.URL, "path": challenge.Path, "body": challenge.Body})
		}
		return map[string]any{"certificate_id": order.ID, "state": "pending_http", "http_challenges": challenges, "expires_at": order.ExpiresAt, "input": order.Input, "order_ref": acmeOrderRef(order), "confirmation_action": "certificates_acme_confirm"}
	}
	records := make([]map[string]string, 0, len(order.Records))
	for _, r := range order.Records {
		records = append(records, map[string]string{"name": r.Name, "type": r.Type, "value": r.Value})
	}
	return map[string]any{"certificate_id": order.ID, "state": "pending_dns", "records": records, "expires_at": order.ExpiresAt, "input": order.Input, "order_ref": acmeOrderRef(order), "confirmation_action": "certificates_acme_confirm"}
}

func acmeOrderRef(state acmeOrderState) string {
	if state.Order == nil {
		return ""
	}
	digest := sha256.Sum256([]byte(state.Order.URI + "\x00" + state.KeyPEM))
	return hex.EncodeToString(digest[:])
}
func (e *AgentExecutor) getACMEOrder(id string) (acmeOrderState, error) {
	var state acmeOrderState
	if !safeID.MatchString(id) {
		return state, invalid("Invalid ACME certificate identity")
	}
	if err := rootJSON(e.state, filepath.Join("acme", id+".order.json"), &state); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return state, failure("not_found", "ACME order was not found", 404)
		}
		return state, err
	}
	if state.Version != 1 || state.ID != id || state.Order == nil {
		return state, failure("metadata_invalid", "ACME order needs recovery inspection", 500)
	}
	return state, nil
}
func (e *AgentExecutor) issueCertificate(ctx context.Context, in request) (any, error) {
	if in.ACME == nil || in.ID != in.ACME.ID {
		return nil, invalid("ACME certificate input is required")
	}
	input := *in.ACME
	input.Domains = append([]string{}, input.Domains...)
	if err := validateACME(&input, e.config); err != nil {
		return nil, err
	}
	if input.Challenge == "dns" {
		return e.issueDNSCertificate(ctx, in, input)
	}
	return e.issueACMEOrder(ctx, in, input, nil)
}

func (e *AgentExecutor) issueACMEOrder(ctx context.Context, in request, input ACMEInput, dnsProvider legacyDNSProvider) (result any, resultErr error) {
	previous, err := e.certificate(in.ID)
	if err != nil && !isNotFound(err) {
		return nil, err
	}
	if err == nil {
		if !input.Renew {
			return nil, failure("certificate_exists", "Renew explicitly or use a new certificate ID", 409)
		}
		if previous.Revision != in.ExpectedRevision {
			return nil, failure("revision_conflict", "Certificate revision changed", 412)
		}
		if !input.Force && time.Until(previous.ExpiresAt) > 30*24*time.Hour {
			return map[string]any{"certificate": previous, "state": "not_due"}, nil
		}
	} else if in.ExpectedRevision != 0 {
		return nil, failure("revision_conflict", "New certificates start at revision zero", 412)
	}
	if old, err := e.getACMEOrder(in.ID); err == nil && (time.Now().Before(old.ExpiresAt) || old.Input.Challenge == "dns") {
		return nil, failure("order_exists", "Confirm or cancel the existing ACME order first", 409)
	} else if err != nil && !isNotFound(err) {
		return nil, err
	}
	client, err := e.acmeClient(ctx, input)
	if err != nil {
		return nil, err
	}
	_, err = client.Register(ctx, &acme.Account{Contact: []string{"mailto:" + input.Email}}, acme.AcceptTOS)
	if err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, failure("acme_registration_failed", "ACME account registration failed", 502)
	}
	identifiers := make([]acme.AuthzID, 0, len(input.Domains))
	for _, name := range input.Domains {
		identifiers = append(identifiers, acme.AuthzID{Type: "dns", Value: name})
	}
	order, err := client.AuthorizeOrder(ctx, identifiers)
	if err != nil {
		return nil, failure("acme_order_failed", "ACME certificate order could not be created", 502)
	}
	_, keyPEM, err := generatePrivateKey()
	if err != nil {
		return nil, err
	}
	state := acmeOrderState{Version: 1, ID: in.ID, Input: input, Order: order, Records: []dnsChallenge{}, HTTPChallenges: []httpACMEChallenge{}, KeyPEM: string(keyPEM), ExpectedRevision: in.ExpectedRevision, CreatedBy: in.Actor.ID, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(24 * time.Hour)}
	if !order.Expires.IsZero() && order.Expires.Before(state.ExpiresAt) {
		state.ExpiresAt = order.Expires
	}
	manualHTTP := input.Challenge == "manual_http"
	if manualHTTP {
		// Retain the remote order before reading its authorizations so a process
		// restart cannot strand an unreferenced CA order with no cancel receipt.
		if err = atomicRoot(e.state, filepath.Join("acme", state.ID+".order.json"), encode(state), 0600); err != nil {
			return nil, err
		}
	}
	var root *os.Root
	if input.Challenge == "http_webroot" {
		site, err := e.get(input.SiteID)
		if err != nil {
			return nil, err
		}
		for _, name := range input.Domains {
			matched := false
			for _, d := range site.Domains {
				_, host, _, _ := domain(d)
				if name == host {
					matched = true
				}
			}
			if !matched {
				return nil, invalid("HTTP validation identifiers must belong to the selected website")
			}
		}
		root, err = e.content.OpenRoot(filepath.Join(site.ID, "public", site.RunPath))
		if err != nil {
			return nil, err
		}
		defer root.Close()
	}
	cleanup := []string{}
	dnsCleanupAttempted := false
	defer func() {
		if root != nil {
			for _, path := range cleanup {
				_ = root.Remove(path)
			}
		}
		if dnsProvider != nil && !dnsCleanupAttempted {
			if err := e.cleanupNativeDNS(client, dnsProvider, state); err != nil {
				result = nativeACMEPublicOrder(state)
				resultErr = errors.Join(resultErr, failure("dns_cleanup_required", "Owned DNS records require cleanup; the private order and ownership receipts were retained", 502))
				return
			}
			if resultErr != nil {
				_ = e.state.Remove(filepath.Join("acme", state.ID+".order.json"))
			}
		}
	}()
	for _, authURL := range order.AuthzURLs {
		authorization, err := client.GetAuthorization(ctx, authURL)
		if err != nil {
			return nil, failure("acme_authorization_failed", "ACME authorization could not be read", 502)
		}
		if authorization.Status == acme.StatusValid {
			continue
		}
		kind := "dns-01"
		if root != nil || manualHTTP {
			kind = "http-01"
		}
		var challenge *acme.Challenge
		for _, ch := range authorization.Challenges {
			if ch.Type == kind {
				challenge = ch
				break
			}
		}
		if challenge == nil {
			return nil, failure("challenge_unavailable", "ACME server did not offer the selected challenge", 412)
		}
		if manualHTTP {
			entry, challengeErr := makeManualHTTPChallenge(client, authorization, authURL, challenge, input.Domains)
			if challengeErr != nil {
				return nil, challengeErr
			}
			state.HTTPChallenges = append(state.HTTPChallenges, entry)
			if err = atomicRoot(e.state, filepath.Join("acme", state.ID+".order.json"), encode(state), 0600); err != nil {
				return nil, err
			}
			continue
		}
		if root == nil {
			value, err := client.DNS01ChallengeRecord(challenge.Token)
			if err != nil {
				return nil, err
			}
			name := "_acme-challenge." + strings.TrimPrefix(authorization.Identifier.Value, "*.")
			if input.DNSAlias != "" {
				name = input.DNSAlias
			}
			state.Records = append(state.Records, dnsChallenge{Name: name, Type: "TXT", Value: value, AuthorizationURL: authURL, Challenge: challenge})
			if dnsProvider != nil {
				state.Records[len(state.Records)-1].Name = nativeDNSChallengeName(input, authorization.Identifier.Value)
				// Persist the token and account-bound order before changing DNS.
				// Credentials remain in the encrypted API secret store only.
				if err = atomicRoot(e.state, filepath.Join("acme", state.ID+".order.json"), encode(state), 0600); err != nil {
					return nil, err
				}
				if err = e.authorizeNativeDNS(ctx, client, dnsProvider, state.Records[len(state.Records)-1], authorization.Identifier.Value); err != nil {
					return nil, err
				}
			}
			continue
		}
		response, err := client.HTTP01ChallengeResponse(challenge.Token)
		if err != nil {
			return nil, err
		}
		path := strings.TrimPrefix(client.HTTP01ChallengePath(challenge.Token), "/")
		if !filepath.IsLocal(path) || strings.ContainsAny(challenge.Token, "/\\\x00") {
			return nil, invalid("Invalid ACME challenge token")
		}
		if err = root.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, err
		}
		file, err := root.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0644)
		if err != nil {
			return nil, err
		}
		cleanup = append(cleanup, path)
		_, err = file.WriteString(response)
		if err == nil {
			err = file.Chmod(0644)
		}
		if err == nil {
			err = file.Sync()
		}
		err = errors.Join(err, file.Close())
		if err != nil {
			return nil, err
		}
		if _, err = client.Accept(ctx, challenge); err != nil {
			return nil, failure("acme_challenge_failed", "ACME challenge was rejected", 502)
		}
		if _, err = client.WaitAuthorization(ctx, authURL); err != nil {
			return nil, failure("acme_validation_failed", "ACME HTTP validation failed", 502)
		}
	}
	if input.Challenge == "manual_dns" && len(state.Records) > 0 {
		if err = atomicRoot(e.state, filepath.Join("acme", state.ID+".order.json"), encode(state), 0600); err != nil {
			return nil, err
		}
		return acmePublicOrder(state), nil
	}
	if manualHTTP && len(state.HTTPChallenges) > 0 {
		return acmePublicOrder(state), nil
	}
	if dnsProvider != nil {
		dnsCleanupAttempted = true
		if err = e.cleanupNativeDNS(client, dnsProvider, state); err != nil {
			return nativeACMEPublicOrder(state), failure("dns_cleanup_required", "Owned DNS records require cleanup before certificate publication", 502)
		}
	}
	return e.finalizeACME(ctx, client, state, in)
}
func (e *AgentExecutor) manualACME(ctx context.Context, in request) (any, error) {
	state, err := e.getACMEOrder(in.ID)
	if err != nil {
		return nil, err
	}
	if (in.Kind == "confirm" || in.Kind == "cancel") && (in.OrderRef == "" || in.OrderRef != acmeOrderRef(state)) {
		return nil, failure("acme_order_changed", "The ACME order changed after this recovery action was prepared", 409)
	}
	if state.Input.Challenge == "dns" {
		return e.resumeNativeDNSCertificate(ctx, in, state)
	}
	if in.Kind == "cancel" {
		if err = e.state.Remove(filepath.Join("acme", in.ID+".order.json")); err != nil {
			return nil, err
		}
		return map[string]any{"certificate_id": in.ID, "state": "cancelled"}, nil
	}
	if !time.Now().Before(state.ExpiresAt) {
		return nil, failure("order_expired", "ACME order expired; cancel and request a new order", 410)
	}
	if in.Kind == "status" {
		return acmePublicOrder(state), nil
	}
	if in.Kind != "confirm" {
		return nil, invalid("Unknown ACME order action")
	}
	if state.Input.Challenge == "manual_http" {
		return e.confirmManualHTTPACME(ctx, in, state)
	}
	client, err := e.acmeClient(ctx, state.Input)
	if err != nil {
		return nil, err
	}
	for _, record := range state.Records {
		if record.Challenge == nil || record.Challenge.Type != "dns-01" {
			return nil, failure("metadata_invalid", "Stored ACME challenge is invalid", 500)
		}
		// RawMessage nil becomes JSON null on persistence. RFC 8555 requires
		// an empty acknowledgement object for DNS-01, never the literal null.
		challenge := *record.Challenge
		challenge.Payload = nil
		if _, err = client.Accept(ctx, &challenge); err != nil {
			return nil, failure("acme_challenge_failed", "ACME DNS challenge was rejected", 502)
		}
		if _, err = client.WaitAuthorization(ctx, record.AuthorizationURL); err != nil {
			return nil, failure("acme_validation_failed", "ACME DNS validation failed; inspect TXT records before retrying", 502)
		}
	}
	in.ExpectedRevision = state.ExpectedRevision
	in.ACME = &state.Input
	return e.finalizeACME(ctx, client, state, in)
}

func manualHTTPTokenSafe(token string) bool {
	if len(token) == 0 || len(token) > 256 {
		return false
	}
	for _, c := range token {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func makeManualHTTPChallenge(client *acme.Client, authorization *acme.Authorization, authURL string, challenge *acme.Challenge, domains []string) (httpACMEChallenge, error) {
	var entry httpACMEChallenge
	if client == nil || authorization == nil || challenge == nil || challenge.Type != "http-01" || authorization.Identifier.Type != "dns" || authorization.Wildcard || !manualHTTPTokenSafe(challenge.Token) || authURL == "" || challenge.URI == "" {
		return entry, failure("metadata_invalid", "ACME HTTP challenge is incomplete or unsafe", 500)
	}
	canonical, host, port, err := domain(authorization.Identifier.Value)
	if err != nil || port != 80 || strings.Contains(authorization.Identifier.Value, ":") || strings.HasPrefix(host, "*.") || !slices.Contains(domains, canonical) {
		return entry, failure("metadata_invalid", "ACME HTTP authorization does not match a requested DNS name", 500)
	}
	path := client.HTTP01ChallengePath(challenge.Token)
	if len(path) > 512 || !strings.HasPrefix(path, "/.well-known/acme-challenge/") || strings.ContainsAny(path, "?#\\\x00") {
		return entry, failure("metadata_invalid", "ACME HTTP challenge path is invalid", 500)
	}
	body, err := client.HTTP01ChallengeResponse(challenge.Token)
	if err != nil || len(body) == 0 || len(body) > 2048 {
		return entry, failure("metadata_invalid", "ACME HTTP challenge response could not be derived", 500)
	}
	challengeCopy := *challenge
	challengeCopy.Payload = nil
	return httpACMEChallenge{Domain: canonical, URL: (&url.URL{Scheme: "http", Host: host, Path: path}).String(), Path: path, Body: body, AuthorizationURL: authURL, Challenge: &challengeCopy}, nil
}

func (e *AgentExecutor) confirmManualHTTPACME(ctx context.Context, in request, state acmeOrderState) (any, error) {
	if state.Input.Challenge != "manual_http" || len(state.HTTPChallenges) == 0 || len(state.HTTPChallenges) > len(state.Input.Domains) || len(state.HTTPChallenges) > 100 {
		return nil, failure("metadata_invalid", "Stored manual HTTP order has no valid pending challenges", 500)
	}
	input := state.Input
	if err := validateACME(&input, e.config); err != nil || input.ID != in.ID || string(encode(input)) != string(encode(state.Input)) {
		return nil, failure("metadata_invalid", "Stored manual HTTP order settings are invalid", 500)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	client, err := e.acmeClient(ctx, input)
	if err != nil {
		return nil, err
	}
	authURLs := make(map[string]bool, len(state.Order.AuthzURLs))
	for _, authURL := range state.Order.AuthzURLs {
		if authURL == "" || authURLs[authURL] {
			return nil, failure("metadata_invalid", "Stored ACME order authorizations are invalid", 500)
		}
		authURLs[authURL] = true
	}
	seenDomains := map[string]bool{}
	seenAuthURLs := map[string]bool{}
	for _, record := range state.HTTPChallenges {
		if record.Challenge == nil || record.Challenge.Type != "http-01" || !authURLs[record.AuthorizationURL] || seenAuthURLs[record.AuthorizationURL] || seenDomains[record.Domain] {
			return nil, failure("metadata_invalid", "Stored ACME HTTP challenge binding is invalid", 500)
		}
		auth, err := client.GetAuthorization(ctx, record.AuthorizationURL)
		if err != nil {
			return nil, failure("acme_authorization_failed", "ACME HTTP authorization could not be read", 502)
		}
		canonical, host, port, domainErr := domain(auth.Identifier.Value)
		if domainErr != nil || port != 80 || strings.Contains(auth.Identifier.Value, ":") || canonical != record.Domain || auth.Identifier.Type != "dns" || auth.Wildcard || !slices.Contains(input.Domains, canonical) {
			return nil, failure("metadata_invalid", "ACME HTTP authorization no longer matches the requested DNS name", 500)
		}
		matched := (*acme.Challenge)(nil)
		for _, challenge := range auth.Challenges {
			if challenge != nil && challenge.Type == "http-01" && challenge.URI == record.Challenge.URI && challenge.Token == record.Challenge.Token {
				if matched != nil {
					return nil, failure("metadata_invalid", "ACME authorization has duplicate HTTP challenges", 500)
				}
				matched = challenge
			}
		}
		if matched == nil || !manualHTTPTokenSafe(record.Challenge.Token) {
			return nil, failure("metadata_invalid", "Stored ACME HTTP challenge no longer matches its authorization", 500)
		}
		body, err := client.HTTP01ChallengeResponse(record.Challenge.Token)
		path := client.HTTP01ChallengePath(record.Challenge.Token)
		wantURL := (&url.URL{Scheme: "http", Host: host, Path: path}).String()
		if err != nil || body != record.Body || path != record.Path || wantURL != record.URL {
			return nil, failure("metadata_invalid", "Stored HTTP upload instructions do not match the protected ACME order", 500)
		}
		seenAuthURLs[record.AuthorizationURL] = true
		seenDomains[record.Domain] = true
		if auth.Status == acme.StatusValid {
			continue
		}
		if auth.Status != acme.StatusPending {
			return nil, failure("acme_validation_failed", "ACME HTTP authorization is no longer pending", 409)
		}
		challenge := *matched
		challenge.Payload = nil
		if challenge.Status == acme.StatusPending {
			if _, err = client.Accept(ctx, &challenge); err != nil {
				return nil, failure("acme_challenge_failed", "ACME HTTP challenge was rejected", 502)
			}
		} else if challenge.Status != acme.StatusProcessing && challenge.Status != acme.StatusValid {
			return nil, failure("acme_validation_failed", "ACME HTTP challenge is no longer pending", 409)
		}
		if _, err = client.WaitAuthorization(ctx, record.AuthorizationURL); err != nil {
			return nil, failure("acme_validation_failed", "ACME HTTP validation failed; verify the uploaded file is reachable over HTTP", 502)
		}
	}
	in.ExpectedRevision = state.ExpectedRevision
	in.ACME = &state.Input
	return e.finalizeACME(ctx, client, state, in)
}

func (e *AgentExecutor) finalizeACME(ctx context.Context, client *acme.Client, state acmeOrderState, in request) (any, error) {
	order, err := client.WaitOrder(ctx, state.Order.URI)
	if err != nil {
		return nil, failure("acme_order_failed", "ACME order did not become ready", 502)
	}
	key, err := parsePrivateKey([]byte(state.KeyPEM))
	if err != nil {
		return nil, err
	}
	var chain [][]byte
	if order.Status == acme.StatusValid {
		if order.CertURL == "" {
			return nil, failure("acme_issuance_incomplete", "The issued ACME order has no certificate URL", 502)
		}
		chain, err = client.FetchCert(ctx, order.CertURL, true)
	} else {
		csr, csrErr := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: state.Input.Domains}, key)
		if csrErr != nil {
			return nil, csrErr
		}
		// Keep the known order URI when a compliant finalize response omits
		// Location and certificate issuance needs another order poll.
		chain, _, err = client.CreateCertFromOrder(ctx, order, csr, true)
	}
	if err != nil {
		return nil, failure("acme_issuance_failed", "ACME certificate issuance failed", 502)
	}
	var certPEM strings.Builder
	for _, der := range chain {
		if certPEM.Len()+len(der)*2 > 64<<10 {
			return nil, failure("too_large", "ACME certificate chain exceeds its limit", 413)
		}
		certPEM.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	meta, err := validateIssuedDNSCertificate(in.ID, state.Input.Domains, certPEM.String(), state.KeyPEM)
	if err != nil {
		return nil, err
	}
	meta.Source = "acme"
	in.Certificate = &meta
	in.CertPEM = certPEM.String()
	in.KeyPEM = state.KeyPEM
	cert, err := e.importIssuedCertificate(ctx, in)
	if err != nil {
		return nil, err
	}
	_ = e.state.Remove(filepath.Join("acme", in.ID+".order.json"))
	return map[string]any{"certificate": cert, "state": "issued"}, nil
}
func (e *AgentExecutor) importIssuedCertificate(ctx context.Context, in request) (Certificate, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Certificate{}, err
	}
	var result Certificate
	err := e.withMutationRuntimes(ctx, "certificate_import", in, func(locked context.Context) error {
		var err error
		result, err = e.importCertificate(locked, in)
		return err
	})
	return result, err
}
