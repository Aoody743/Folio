package sites

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// These are trusted Go dependencies, not HTTP/MCP input. Credentials have
// already been resolved from the certificate's resource-bound secret reference.
type legacyDNSOptions struct {
	Provider          string
	Credentials       map[string]string
	OwnerID           string
	State             *os.Root
	DNSAlias          string
	AllowedAPIURLs    []string
	AllowedDNSServers []string
	ExclusiveTXTSlots []string
	HTTPClient        *http.Client
	LookupTXT         func(context.Context, string) ([]string, error)
	Now               func() time.Time
}

type legacyDNSProvider interface {
	Present(context.Context, string, string, string) error
	CleanUp(context.Context, string, string, string) error
}

type legacyDNSTXTRecord struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Value    string `json:"value"`
	Disabled bool   `json:"disabled,omitempty"`
}
type legacyDNSReceipt struct {
	Version     int                  `json:"version"`
	OwnerID     string               `json:"owner_id"`
	Provider    string               `json:"provider"`
	AccountHash string               `json:"account_hash"`
	Name        string               `json:"name"`
	Value       string               `json:"value"`
	Zone        string               `json:"zone"`
	RecordID    string               `json:"record_id,omitempty"`
	State       string               `json:"state"`
	Before      []legacyDNSTXTRecord `json:"before"`
	UpdatedAt   time.Time            `json:"updated_at"`
}

type legacyDNSBackend interface {
	zone(context.Context, string) (string, error)
	list(context.Context, string, string) ([]legacyDNSTXTRecord, error)
	create(context.Context, *legacyDNSReceipt) error
	remove(context.Context, legacyDNSReceipt) error
}

type legacyDNSClient struct {
	options     legacyDNSOptions
	http        *http.Client
	backend     legacyDNSBackend
	accountHash string
}

var legacyDNSFactories = map[string]func(*legacyDNSClient) (legacyDNSBackend, error){}
var legacyDNSOperationLocks certificateLocks
var legacyDNSCredentialFields = map[string][]string{
	"dns_dpi":       {"DPI_Id", "DPI_Key", "DPI_AuthField"},
	"dns_ispconfig": {"ISPC_User", "ISPC_Password", "ISPC_Api", "ISPC_Api_Insecure"},
	"dns_freedns":   {"FREEDNS_User", "FREEDNS_Password"},
	"dns_cyon":      {"CY_Username", "CY_Password", "CY_OTP_Secret"},
	"dns_knot":      {"KNOT_SERVER", "KNOT_KEY", "KNOT_ZONE"},
	"dns_kinghost":  {"KINGHOST_Username", "KINGHOST_Password"},
	"dns_zilore":    {"Zilore_Key"},
	"dns_online":    {"ONLINE_API_KEY"},
	"dns_ddnss":     {"DDNSS_Token"},
}
var legacyDNSRequiredFields = map[string][]string{
	"dns_dpi":       {"DPI_Id", "DPI_Key"},
	"dns_ispconfig": {"ISPC_User", "ISPC_Password", "ISPC_Api"},
	"dns_freedns":   {"FREEDNS_User", "FREEDNS_Password"},
	"dns_cyon":      {"CY_Username", "CY_Password"},
	"dns_knot":      {"KNOT_SERVER", "KNOT_KEY"},
	"dns_kinghost":  {"KINGHOST_Username", "KINGHOST_Password"},
	"dns_zilore":    {"Zilore_Key"},
	"dns_online":    {"ONLINE_API_KEY"},
	"dns_ddnss":     {"DDNSS_Token"},
}

func newLegacyDNSProvider(options legacyDNSOptions) (legacyDNSProvider, error) {
	fields, known := legacyDNSCredentialFields[options.Provider]
	if !known || !safeID.MatchString(options.OwnerID) || options.State == nil {
		return nil, invalid("Legacy DNS requires a known provider, trusted owner and private receipt store")
	}
	if len(options.Credentials) == 0 || len(options.Credentials) > 16 {
		return nil, invalid("Legacy DNS credential fields exceed their limit")
	}
	options.Credentials = cloneLegacyDNSCredentials(options.Credentials)
	for name, value := range options.Credentials {
		if !slices.Contains(fields, name) || len(value) > 64<<10 || strings.ContainsAny(value, "\x00\r\n") {
			return nil, invalid("Unknown or oversized legacy DNS credential field")
		}
		if strings.Contains(strings.ToLower(name), "insecure") && value != "" && value != "0" && value != "false" {
			return nil, failure("tls_required", "DNS API certificate verification cannot be disabled", 400)
		}
	}
	for _, field := range legacyDNSRequiredFields[options.Provider] {
		if options.Credentials[field] == "" {
			return nil, invalid("Required legacy DNS credential fields are missing")
		}
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.LookupTXT == nil {
		options.LookupTXT = net.DefaultResolver.LookupTXT
	}
	info, err := options.State.Stat(".")
	if err != nil {
		return nil, err
	}
	identity, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0077 != 0 || identity.Uid != uint32(os.Geteuid()) {
		return nil, failure("policy_denied", "DNS receipt state must be private and owned by the Agent identity", 403)
	}
	factory := legacyDNSFactories[options.Provider]
	if factory == nil {
		return nil, failure("dns_provider_unavailable", "Native legacy DNS adapter is unavailable", 412)
	}
	client := options.HTTPClient
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		client = &http.Client{Transport: transport, Timeout: 30 * time.Second}
	} else {
		copy := *client
		client = &copy
		if transport, ok := client.Transport.(*http.Transport); ok && transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
			return nil, failure("tls_required", "DNS API client must verify TLS", 400)
		}
		client.Timeout = 30 * time.Second
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	account := sha256.Sum256(encode(options.Credentials))
	provider := &legacyDNSClient{options: options, http: client, accountHash: hex.EncodeToString(account[:])}
	provider.backend, err = factory(provider)
	if err != nil {
		return nil, err
	}
	if err := options.State.MkdirAll("acme/dns-receipts", 0700); err != nil {
		return nil, err
	}
	return provider, nil
}

func cloneLegacyDNSCredentials(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for name, value := range input {
		result[name] = value
	}
	return result
}

var legacyDNSLabel = regexp.MustCompile("^[a-z0-9_](?:[a-z0-9_-]{0,61}[a-z0-9_])?$")
var legacyDNSToken = regexp.MustCompile("^[A-Za-z0-9_-]{1,512}$")

func canonicalLegacyDNSName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if len(name) == 0 || len(name) > 253 || !strings.Contains(name, ".") {
		return "", invalid("DNS challenge needs a bounded fully qualified name")
	}
	for _, label := range strings.Split(name, ".") {
		if !legacyDNSLabel.MatchString(label) {
			return "", invalid("DNS challenge name contains an invalid label")
		}
	}
	return name + ".", nil
}

func (p *legacyDNSClient) challenge(domain, token, keyAuthorization string) (string, string, error) {
	if !legacyDNSToken.MatchString(token) || len(keyAuthorization) > 4096 || !strings.HasPrefix(keyAuthorization, token+".") || strings.ContainsAny(keyAuthorization, "\x00\r\n") {
		return "", "", invalid("DNS challenge token or key authorization is invalid")
	}
	name := "_acme-challenge." + strings.TrimPrefix(domain, "*.")
	if p.options.DNSAlias != "" {
		name = p.options.DNSAlias
		if !strings.HasPrefix(name, "_acme-challenge.") {
			name = "_acme-challenge." + name
		}
	}
	name, err := canonicalLegacyDNSName(name)
	if err != nil {
		return "", "", err
	}
	value := sha256.Sum256([]byte(keyAuthorization))
	return name, base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func (p *legacyDNSClient) receiptPath(name, value string) string {
	hash := sha256.Sum256([]byte(p.options.OwnerID + "\x00" + p.options.Provider + "\x00" + name + "\x00" + value))
	return filepath.Join("acme", "dns-receipts", hex.EncodeToString(hash[:])+".json")
}

func (p *legacyDNSClient) saveReceipt(record legacyDNSReceipt) error {
	record.UpdatedAt = p.options.Now().UTC()
	data := encode(record)
	if len(data) > 256<<10 {
		return failure("dns_record_limit", "DNS ownership receipt exceeds its metadata budget", 413)
	}
	return atomicRoot(p.options.State, p.receiptPath(record.Name, record.Value), data, 0600)
}

func (p *legacyDNSClient) loadReceipt(name, value string) (legacyDNSReceipt, error) {
	var record legacyDNSReceipt
	if err := rootJSON(p.options.State, p.receiptPath(name, value), &record); err != nil {
		return record, err
	}
	if record.Version != 1 || record.OwnerID != p.options.OwnerID || record.Provider != p.options.Provider || record.AccountHash != p.accountHash || record.Name != name || record.Value != value ||
		(record.State != "prepared" && record.State != "created" && record.State != "borrowed") {
		return record, failure("dns_receipt_conflict", "DNS ownership receipt does not match this trusted operation", 409)
	}
	return record, nil
}

func legacyDNSMatching(records []legacyDNSTXTRecord, name, value string) []legacyDNSTXTRecord {
	matches := []legacyDNSTXTRecord{}
	for _, record := range records {
		if strings.EqualFold(record.Name, name) && record.Value == value {
			matches = append(matches, record)
		}
	}
	return matches
}

func (p *legacyDNSClient) operationLock(ctx context.Context) (func(), error) {
	return legacyDNSOperationLocks.acquire(ctx, p.options.Provider+"\x00"+p.accountHash)
}

func (p *legacyDNSClient) recoverPrepared(ctx context.Context, receipt *legacyDNSReceipt) error {
	records, err := p.backend.list(ctx, receipt.Name, receipt.Zone)
	if err != nil {
		return err
	}
	candidates := legacyDNSMatching(records, receipt.Name, receipt.Value)
	if len(candidates) == 0 {
		return fs.ErrNotExist
	}
	if len(candidates) != 1 {
		return failure("dns_ownership_ambiguous", "DNS recovery found multiple matching records; nothing was deleted", 409)
	}
	receipt.RecordID, receipt.State = candidates[0].ID, "created"
	for _, original := range receipt.Before {
		if original.ID == candidates[0].ID {
			receipt.State = "borrowed"
		}
	}
	return p.saveReceipt(*receipt)
}

func (p *legacyDNSClient) Present(ctx context.Context, domain, token, keyAuthorization string) error {
	name, value, err := p.challenge(domain, token, keyAuthorization)
	if err != nil {
		return err
	}
	unlock, err := p.operationLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	receipt, err := p.loadReceipt(name, value)
	if err == nil {
		if receipt.State == "prepared" {
			if err := p.recoverPrepared(ctx, &receipt); err == nil {
				return nil
			} else if errors.Is(err, fs.ErrNotExist) {
				// The previous request may still commit after its response was
				// lost. Keep its baseline and do not issue a second creation.
				return failure("dns_creation_pending", "DNS creation has an unresolved ownership receipt; retry recovery after the API settles", 409)
			} else {
				return err
			}
		} else {
			records, err := p.backend.list(ctx, name, receipt.Zone)
			if err != nil {
				return err
			}
			for _, record := range legacyDNSMatching(records, name, value) {
				if record.ID == receipt.RecordID {
					if record.Disabled {
						return failure("dns_record_disabled", "The existing challenge TXT record is not published", 412)
					}
					return nil
				}
			}
			return failure("dns_record_conflict", "Owned DNS challenge changed; inspect its receipt before retrying", 409)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	zone, err := p.backend.zone(ctx, name)
	if err != nil {
		return err
	}
	before, err := p.backend.list(ctx, name, zone)
	if err != nil {
		return err
	}
	if len(before) > 2000 {
		return failure("dns_record_limit", "DNS challenge baseline exceeds 2000 records", 413)
	}
	receipt = legacyDNSReceipt{Version: 1, OwnerID: p.options.OwnerID, Provider: p.options.Provider, AccountHash: p.accountHash, Name: name, Value: value, Zone: zone, Before: before, State: "prepared"}
	matches := legacyDNSMatching(before, name, value)
	if len(matches) > 0 {
		for _, record := range matches {
			if !record.Disabled {
				receipt.State, receipt.RecordID = "borrowed", record.ID
				return p.saveReceipt(receipt)
			}
		}
		return failure("dns_record_disabled", "Matching challenge TXT records are disabled; no record was modified", 412)
	}
	if err := p.saveReceipt(receipt); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.backend.create(ctx, &receipt); err != nil {
		return err
	}
	if receipt.RecordID == "" {
		return p.recoverPrepared(ctx, &receipt)
	}
	records, err := p.backend.list(ctx, name, zone)
	if err != nil {
		return err
	}
	for _, record := range legacyDNSMatching(records, name, value) {
		if record.ID == receipt.RecordID {
			if record.Disabled {
				return failure("dns_record_disabled", "The newly created challenge TXT record is not published", 412)
			}
			if receipt.State != "borrowed" {
				receipt.State = "created"
			}
			return p.saveReceipt(receipt)
		}
	}
	return failure("dns_record_unverified", "DNS API creation did not produce the exact expected record", 502)
}

func (p *legacyDNSClient) CleanUp(ctx context.Context, domain, token, keyAuthorization string) error {
	name, value, err := p.challenge(domain, token, keyAuthorization)
	if err != nil {
		return err
	}
	unlock, err := p.operationLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	receipt, err := p.loadReceipt(name, value)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if receipt.State == "prepared" {
		err = p.recoverPrepared(ctx, &receipt)
		if errors.Is(err, fs.ErrNotExist) {
			// An API timeout does not prove that its write was rejected. A
			// delayed commit must remain recoverable after Agent restart.
			return failure("dns_cleanup_pending", "DNS creation is not yet observable; its ownership receipt was retained for cleanup recovery", 409)
		}
		if err != nil {
			return err
		}
	}
	if receipt.State != "borrowed" {
		records, err := p.backend.list(ctx, name, receipt.Zone)
		if err != nil {
			return err
		}
		found := false
		for _, record := range records {
			if record.ID == receipt.RecordID {
				if record.Name != name || record.Value != value {
					return failure("dns_record_conflict", "Owned DNS record changed; cleanup left it untouched", 409)
				}
				found = true
			}
		}
		if !found && len(legacyDNSMatching(records, name, value)) != 0 {
			return failure("dns_record_conflict", "The challenge value now has a different record identity; its ownership receipt was retained", 409)
		}
		if found {
			if err := p.backend.remove(ctx, receipt); err != nil {
				return err
			}
			after, err := p.backend.list(ctx, name, receipt.Zone)
			if err != nil {
				return err
			}
			for _, record := range after {
				if record.ID == receipt.RecordID {
					return failure("dns_cleanup_unverified", "DNS API cleanup did not remove the owned record", 502)
				}
			}
		}
	}
	return p.options.State.Remove(p.receiptPath(name, value))
}

func legacyDNSAPIURL(value string) (*url.URL, error) {
	result, err := url.Parse(value)
	if err != nil || result.Scheme != "https" || result.Host == "" || result.User != nil || result.RawQuery != "" || result.Fragment != "" {
		return nil, failure("tls_required", "DNS API must use a fixed verified HTTPS endpoint", 400)
	}
	return result, nil
}

// No provider body, URL or authentication fields are returned in an error:
// provider diagnostics and transport errors may echo submitted credentials.
func (p *legacyDNSClient) request(ctx context.Context, method string, endpoint *url.URL, headers http.Header, body []byte) ([]byte, int, error) {
	if endpoint == nil || endpoint.Scheme != "https" || endpoint.User != nil {
		return nil, 0, failure("tls_required", "DNS request needs a verified HTTPS endpoint", 400)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, invalid("Invalid typed DNS request")
	}
	request.Header = headers.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set("User-Agent", "Folio/2 DNS (https://github.com/Aoody743/Folio)")
	response, err := p.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, failure("dns_request_failed", "DNS provider request failed", 502)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20+1))
	if err != nil || len(data) > 2<<20 {
		return nil, response.StatusCode, failure("dns_response_limit", "DNS provider response exceeded its read budget", 502)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code, status := "dns_request_failed", 502
		if response.StatusCode == 401 || response.StatusCode == 403 {
			code, status = "dns_auth_failed", 403
		} else if response.StatusCode == 404 {
			code, status = "dns_record_missing", 404
		} else if response.StatusCode == 429 {
			code, status = "dns_rate_limited", 429
		}
		return nil, response.StatusCode, failure(code, "DNS provider rejected the typed request", status)
	}
	return data, response.StatusCode, nil
}

func legacyDNSDecode(data []byte, output any) error {
	if err := json.Unmarshal(data, output); err != nil {
		return failure("dns_response_invalid", "DNS provider response did not match its reviewed schema", 502)
	}
	return nil
}
