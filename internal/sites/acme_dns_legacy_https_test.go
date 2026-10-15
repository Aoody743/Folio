package sites

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

type legacyHTTPSRecord struct {
	id, name, kind, wire, status string
}
type legacyHTTPSFixture struct {
	mu                                    sync.Mutex
	provider                              string
	domainID                              string
	records                               []legacyHTTPSRecord
	pending                               []legacyHTTPSRecord
	requests, adds, deletes, pages        int
	failCreate, deferCreate, failResponse bool
	repeatPage                            bool
	paginatedDomains                      bool
	oversized                             bool
	redirect                              string
}

// The Go-only transport retargets a fixed reviewed API to a local HTTPS server.
// It preserves TLS verification with the fixture CA and rejects other hosts;
// production code has no endpoint credential or public transport option.
type legacyFixtureTransport struct {
	official, server *url.URL
	inner            http.RoundTripper
}

func (tr legacyFixtureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != tr.official.Host || !strings.HasPrefix(request.URL.Path, tr.official.Path+"/") {
		return nil, errors.New("unreviewed fixture API")
	}
	copy := request.Clone(request.Context())
	endpoint := *copy.URL
	endpoint.Scheme, endpoint.Host = tr.server.Scheme, tr.server.Host
	copy.URL, copy.Host = &endpoint, tr.server.Host
	return tr.inner.RoundTrip(copy)
}

func legacyReviewedHTTPSFixture(t *testing.T, provider string) (legacyDNSOptions, *legacyHTTPSFixture) {
	t.Helper()
	official, field := legacyOnlineAPI, "ONLINE_API_KEY"
	if provider == "dns_zilore" {
		official, field = legacyZiloreAPI, "Zilore_Key"
	}
	if provider == "dns_dpi" {
		official, field = legacyDPIAPI, "DPI_Key"
	}
	state := &legacyHTTPSFixture{provider: provider, domainID: "21", records: []legacyHTTPSRecord{
		{id: "9", name: "_acme-challenge.example.test", kind: "TXT", wire: strconv.Quote("unrelated-existing"), status: "enabled"},
		{id: "8", name: "www.example.test", kind: "A", wire: "192.0.2.10", status: "enabled"},
	}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		state.requests++
		writer.Header().Set("Content-Type", "application/json")
		if request.TLS == nil || strings.Contains(request.URL.RawQuery, "fixture-private-key") {
			t.Error("DNS request skipped HTTPS or sent a secret in its URL")
		}
		auth := request.Header.Get("X-Auth-Key")
		if provider == "dns_online" {
			auth = strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		}
		if provider == "dns_dpi" {
			if request.Method != http.MethodPost || request.ParseForm() != nil {
				t.Error("DPI must use POST forms")
			}
			if request.PostForm.Get("login_token") == "123,fixture-private-key" || request.PostForm.Get("user_token") == "123,fixture-private-key" {
				if request.PostForm.Get("login_token") != "" && request.PostForm.Get("user_token") != "" {
					t.Error("DPI sent both authentication schemes")
				}
				auth = "fixture-private-key"
			}
		}
		if auth != "fixture-private-key" {
			writer.WriteHeader(401)
			writer.Write([]byte("provider echoes fixture-private-key"))
			return
		}
		if state.redirect != "" {
			writer.Header().Set("Location", state.redirect)
			writer.WriteHeader(307)
			return
		}
		if state.oversized {
			writer.Write([]byte(strings.Repeat("x", (2<<20)+1)))
			return
		}
		if state.failResponse {
			writer.WriteHeader(500)
			writer.Write([]byte("provider echoes fixture-private-key"))
			return
		}
		if provider == "dns_dpi" {
			legacyDPIFixtureRequest(t, state, writer, request)
		} else if provider == "dns_zilore" {
			legacyZiloreFixtureRequest(t, state, writer, request)
		} else {
			legacyOnlineFixtureRequest(t, state, writer, request)
		}
	}))
	t.Cleanup(server.Close)
	base, _ := url.Parse(official)
	local, _ := url.Parse(server.URL)
	client := server.Client()
	client.Transport = legacyFixtureTransport{official: base, server: local, inner: client.Transport}
	credentials := map[string]string{field: "fixture-private-key"}
	if provider == "dns_dpi" {
		credentials["DPI_Id"] = "123"
	}
	return legacyDNSOptions{Provider: provider, OwnerID: "https-certificate", State: legacyPrivateDNSState(t),
		Credentials: credentials, AllowedAPIURLs: []string{official}, HTTPClient: client}, state
}

func legacyZiloreFixtureRequest(t *testing.T, state *legacyHTTPSFixture, writer http.ResponseWriter, request *http.Request) {
	t.Helper()
	switch request.URL.Path {
	case "/dns/v1/domains":
		if request.Method != http.MethodGet {
			t.Error("Zilore domain discovery was not GET")
			writer.WriteHeader(400)
			return
		}
		domains := []map[string]any{}
		if request.URL.Query().Get("search_text") == "example.test" {
			domains = append(domains, map[string]any{"domain_id": state.domainID, "domain_name": "example.test"})
			if state.paginatedDomains {
				domains = []map[string]any{}
				for index := range 110 {
					domains = append(domains, map[string]any{"domain_id": strconv.Itoa(3000 + index), "domain_name": fmt.Sprintf("nearby%d.example.test", index)})
				}
				domains[105] = map[string]any{"domain_id": state.domainID, "domain_name": "example.test"}
				offset, _ := strconv.Atoi(request.URL.Query().Get("offset"))
				offset = min(max(offset, 0), len(domains))
				domains = domains[offset:min(offset+100, len(domains))]
			}
		}
		json.NewEncoder(writer).Encode(map[string]any{"status": "ok", "response": domains})
	case "/dns/v1/domains/example.test/records":
		switch request.Method {
		case http.MethodGet:
			query := request.URL.Query()
			if query.Get("limit") != "100" || query.Get("order_by") != "record_id" || query.Get("order_param") != "asc" || query.Get("strict_search") != "yes" || query.Get("search_record_type") != "TXT" {
				t.Error("Zilore records did not use bounded stable exact-type discovery")
			}
			offset, err := strconv.Atoi(query.Get("offset"))
			if err != nil || offset < 0 {
				t.Error("invalid record offset")
				writer.WriteHeader(400)
				return
			}
			state.pages++
			all := []legacyZiloreRecord{}
			for _, record := range state.records {
				if record.kind != "TXT" {
					continue
				}
				// Include a nearby name to verify the driver does not trust
				// provider search filtering as an identity/ownership check.
				all = append(all, legacyZiloreRecord{ID: legacyDNSID(record.id), Name: record.name, Type: record.kind, Value: record.wire, Status: record.status})
			}
			if state.repeatPage && offset > 0 {
				offset = 0
			}
			if offset > len(all) {
				offset = len(all)
			}
			end := min(offset+100, len(all))
			json.NewEncoder(writer).Encode(map[string]any{"status": "ok", "response": all[offset:end]})
		case http.MethodPost:
			if request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || request.ParseForm() != nil {
				t.Error("Zilore create did not use its reviewed form schema")
				writer.WriteHeader(400)
				return
			}
			form := request.PostForm
			if form.Get("record_name") != "_acme-challenge.example.test" || form.Get("record_type") != "TXT" || form.Get("record_ttl") != "600" {
				t.Error("Zilore creation exceeded its typed challenge fields")
			}
			record := legacyHTTPSRecord{id: "1000", name: form.Get("record_name"), kind: "TXT", wire: form.Get("record_value"), status: "enabled"}
			state.adds++
			if state.deferCreate {
				state.pending = append(state.pending, record)
			} else {
				state.records = append(state.records, record)
			}
			if state.failCreate {
				writer.WriteHeader(500)
				writer.Write([]byte("provider echoes fixture-private-key"))
				return
			}
			json.NewEncoder(writer).Encode(map[string]any{"status": "ok", "response": map[string]any{"record_id": 1000, "record_status": "added"}})
		case http.MethodDelete:
			id := request.URL.Query().Get("record_id")
			if id != "1000" {
				t.Error("Zilore deletion was not the exact newly owned ID")
				writer.WriteHeader(400)
				return
			}
			state.deletes++
			kept := []legacyHTTPSRecord{}
			for _, record := range state.records {
				if record.id != id {
					kept = append(kept, record)
				}
			}
			state.records = kept
			json.NewEncoder(writer).Encode(map[string]any{"status": "ok", "response": map[string]any{"record_status": "deleted"}})
		default:
			t.Error("unreviewed Zilore HTTP method")
			writer.WriteHeader(400)
		}
	default:
		t.Error("unreviewed Zilore endpoint")
		writer.WriteHeader(400)
	}
}

func legacyOnlineFixtureRequest(t *testing.T, state *legacyHTTPSFixture, writer http.ResponseWriter, request *http.Request) {
	t.Helper()
	switch request.URL.Path {
	case "/api/v1/domain/example.test":
		if request.Method != http.MethodGet {
			t.Error("Online domain preflight was not GET")
			writer.WriteHeader(400)
			return
		}
		json.NewEncoder(writer).Encode(legacyOnlineDomain{ID: legacyDNSID(state.domainID), Name: "example.test"})
	case "/api/v1/domain/example.test/zone":
		if request.Method != http.MethodGet {
			t.Error("Online active zone was mutated directly")
			writer.WriteHeader(400)
			return
		}
		all := []legacyOnlineRecord{}
		for _, record := range state.records {
			all = append(all, legacyOnlineRecord{ID: legacyDNSID(record.id), Name: strings.TrimSuffix(record.name, ".example.test"), Type: record.kind, Data: record.wire,
				Domain: legacyOnlineDomain{ID: legacyDNSID(state.domainID), Name: "example.test"}})
		}
		json.NewEncoder(writer).Encode(all)
	case "/api/v1/domain/example.test/version/active":
		if request.Method != http.MethodPatch || request.Header.Get("Content-Type") != "application/json" {
			t.Error("Online used a zone reset/replace or unreviewed body")
			writer.WriteHeader(400)
			return
		}
		var changes []legacyOnlineChange
		if json.NewDecoder(io.LimitReader(request.Body, 4096)).Decode(&changes) != nil || len(changes) != 1 || changes[0].Name != "_acme-challenge" || changes[0].Type != "TXT" {
			t.Error("Online change was not one exact TXT operation")
			writer.WriteHeader(400)
			return
		}
		change := changes[0]
		switch change.Kind {
		case "ADD":
			if len(change.Records) != 1 || change.Data != "" || change.Records[0].Type != "TXT" || change.Records[0].Name != change.Name || change.Records[0].TTL != 60 || change.Records[0].Priority != 0 {
				t.Error("Online ADD exceeded one typed TXT value")
			}
			record := legacyHTTPSRecord{id: "1000", name: change.Name + ".example.test", kind: "TXT", wire: change.Records[0].Data, status: "enabled"}
			state.adds++
			if state.deferCreate {
				state.pending = append(state.pending, record)
			} else {
				state.records = append(state.records, record)
			}
			if state.failCreate {
				writer.WriteHeader(500)
				writer.Write([]byte("provider echoes fixture-private-key"))
				return
			}
		case "DELETE":
			if change.Data == "" || len(change.Records) != 0 {
				t.Error("Online DELETE omitted the exact data filter")
				writer.WriteHeader(400)
				return
			}
			state.deletes++
			kept := []legacyHTTPSRecord{}
			for _, record := range state.records {
				if record.name == change.Name+".example.test" && record.kind == "TXT" && record.wire == change.Data {
					if record.id != "1000" {
						t.Error("Online removed somebody else's TXT identity")
					}
				} else {
					kept = append(kept, record)
				}
			}
			state.records = kept
		default:
			t.Error("Online used an RRset REPLACE operation")
			writer.WriteHeader(400)
			return
		}
		writer.WriteHeader(201)
	default:
		if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/api/v1/domain/") {
			writer.WriteHeader(404)
			return
		}
		t.Error("Online used an unreviewed endpoint or temporary zone")
		writer.WriteHeader(400)
	}
}

func legacyHTTPSExerciseLifecycle(t *testing.T, provider string) {
	t.Helper()
	options, state := legacyReviewedHTTPSFixture(t, provider)
	clientProvider, err := newLegacyDNSProvider(options)
	if err != nil {
		t.Fatal(err)
	}
	client := clientProvider.(*legacyDNSClient)
	const domain, token, authorization = "example.test", "https-token", "https-token.thumbprint"
	ctx := context.Background()
	if err := clientProvider.Present(ctx, "*."+domain, token, authorization); err != nil {
		t.Fatal(err)
	}
	name, value, _ := client.challenge(domain, token, authorization)
	receipt, err := client.loadReceipt(name, value)
	if err != nil || receipt.RecordID != "1000" || receipt.Zone != "21:example.test" || receipt.State != "created" {
		t.Fatal("exact observed domain/record ownership was not persisted", err)
	}
	raw, err := options.State.ReadFile(client.receiptPath(name, value))
	if err != nil || strings.Contains(string(raw), "fixture-private-key") {
		t.Fatal("receipt persisted plaintext credentials", err)
	}
	if err := clientProvider.Present(ctx, domain, token, authorization); err != nil {
		t.Fatal(err)
	}
	changedAccount := options
	changedAccount.Credentials = cloneLegacyDNSCredentials(options.Credentials)
	for field := range changedAccount.Credentials {
		if provider == "dns_dpi" && field == "DPI_Id" {
			continue
		}
		changedAccount.Credentials[field] = "rotated-private-key"
	}
	rotated, err := newLegacyDNSProvider(changedAccount)
	if err != nil {
		t.Fatal(err)
	}
	if err := rotated.CleanUp(ctx, domain, token, authorization); core.ErrorCode(err) != "dns_receipt_conflict" {
		t.Fatal("rotated account adopted an earlier account's receipt", err)
	}
	otherOwner := options
	otherOwner.OwnerID = "different-certificate"
	stranger, err := newLegacyDNSProvider(otherOwner)
	if err != nil {
		t.Fatal(err)
	}
	if err := stranger.CleanUp(ctx, domain, token, authorization); err != nil {
		t.Fatal(err)
	}
	restarted, err := newLegacyDNSProvider(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.CleanUp(ctx, domain, token, authorization); err != nil {
		t.Fatal(err)
	}
	if _, err := options.State.Stat(client.receiptPath(name, value)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("verified cleanup retained its completed receipt", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.adds != 1 || state.deletes != 1 || len(state.records) != 2 || state.records[0].id != "9" || state.records[1].id != "8" {
		t.Fatal("restart cleanup replayed creation or altered unrelated records")
	}
}

func legacyHTTPSExerciseLostResponse(t *testing.T, provider string) {
	t.Helper()
	for _, delayed := range []bool{false, true} {
		t.Run(fmt.Sprintf("delayed_%t", delayed), func(t *testing.T) {
			options, state := legacyReviewedHTTPSFixture(t, provider)
			state.failCreate, state.deferCreate = true, delayed
			clientProvider, err := newLegacyDNSProvider(options)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			const domain, token, authorization = "example.test", "lost-https-token", "lost-https-token.thumbprint"
			if err := clientProvider.Present(ctx, domain, token, authorization); err == nil || strings.Contains(err.Error(), "fixture-private-key") {
				t.Fatal("lost response was success or exposed credentials", err)
			}
			restarted, err := newLegacyDNSProvider(options)
			if err != nil {
				t.Fatal(err)
			}
			if delayed {
				if err := restarted.Present(ctx, domain, token, authorization); core.ErrorCode(err) != "dns_creation_pending" {
					t.Fatal("unresolved create was replayed", err)
				}
				if err := restarted.CleanUp(ctx, domain, token, authorization); core.ErrorCode(err) != "dns_cleanup_pending" {
					t.Fatal("delayed commit lost its recovery receipt", err)
				}
				state.mu.Lock()
				if state.adds != 1 || state.deletes != 0 {
					t.Error("unknown remote commit caused another mutation")
				}
				state.records = append(state.records, state.pending...)
				state.pending = nil
				state.mu.Unlock()
			}
			if err := restarted.CleanUp(ctx, domain, token, authorization); err != nil {
				t.Fatal(err)
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.adds != 1 || state.deletes != 1 || len(state.records) != 2 {
				t.Fatal("response recovery changed an unrelated TXT/A record")
			}
		})
	}
}

func legacyHTTPSExerciseBorrowedAndIdentityConflicts(t *testing.T, provider string) {
	t.Helper()
	for _, mutation := range []string{"borrowed", "record_value", "record_id", "domain_id"} {
		t.Run(mutation, func(t *testing.T) {
			options, state := legacyReviewedHTTPSFixture(t, provider)
			current, err := newLegacyDNSProvider(options)
			if err != nil {
				t.Fatal(err)
			}
			client := current.(*legacyDNSClient)
			ctx := context.Background()
			const domain, token, authorization = "example.test", "identity-token", "identity-token.thumbprint"
			name, value, _ := client.challenge(domain, token, authorization)
			if mutation == "borrowed" {
				state.records = append(state.records, legacyHTTPSRecord{id: "10", name: strings.TrimSuffix(name, "."), kind: "TXT", wire: strconv.Quote(value), status: "enabled"})
			}
			if err := current.Present(ctx, domain, token, authorization); err != nil {
				t.Fatal(err)
			}
			if mutation != "borrowed" {
				state.mu.Lock()
				switch mutation {
				case "domain_id":
					state.domainID = "22"
				case "record_value", "record_id":
					for index := range state.records {
						if state.records[index].id != "1000" {
							continue
						}
						if mutation == "record_value" {
							state.records[index].wire = strconv.Quote("externally-replaced-value")
						} else {
							state.records[index].id = "1001"
						}
					}
				}
				state.mu.Unlock()
			}
			restarted, err := newLegacyDNSProvider(options)
			if err != nil {
				t.Fatal(err)
			}
			err = restarted.CleanUp(ctx, domain, token, authorization)
			if mutation == "borrowed" {
				if err != nil {
					t.Fatal(err)
				}
				state.mu.Lock()
				defer state.mu.Unlock()
				if state.adds != 0 || state.deletes != 0 || len(state.records) != 3 {
					t.Fatal("pre-existing TXT was borrowed destructively")
				}
			} else {
				expected := "dns_record_conflict"
				if mutation == "domain_id" {
					expected = "dns_zone_conflict"
				}
				if core.ErrorCode(err) != expected {
					t.Fatal("changed ownership was deleted or misreported", err)
				}
				if _, err := client.loadReceipt(name, value); err != nil {
					t.Fatal("ownership conflict lost its recovery receipt", err)
				}
				state.mu.Lock()
				defer state.mu.Unlock()
				if state.deletes != 0 || len(state.records) != 3 {
					t.Fatal("ownership conflict caused destructive cleanup")
				}
			}
		})
	}
}

func legacyHTTPSExerciseFixedPolicyTLSRedirectAndResponseLimits(t *testing.T, provider string) {
	t.Helper()
	for _, negative := range []string{"policy", "tls", "redirect", "body_limit", "diagnostic"} {
		t.Run(negative, func(t *testing.T) {
			options, state := legacyReviewedHTTPSFixture(t, provider)
			if negative == "policy" {
				options.AllowedAPIURLs = []string{"https://unreviewed.invalid/api"}
				if _, err := newLegacyDNSProvider(options); core.ErrorCode(err) != "policy_denied" {
					t.Fatal("fixed provider accepted an unreviewed API", err)
				}
				state.mu.Lock()
				defer state.mu.Unlock()
				if state.requests != 0 {
					t.Fatal("denied policy contacted a provider")
				}
				return
			}
			var redirects atomic.Int32
			if negative == "tls" {
				transport := options.HTTPClient.Transport.(legacyFixtureTransport)
				transport.inner = http.DefaultTransport.(*http.Transport).Clone()
				options.HTTPClient = &http.Client{Transport: transport}
			} else if negative == "redirect" {
				sink := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirects.Add(1) }))
				t.Cleanup(sink.Close)
				state.redirect = sink.URL + "/steal"
			} else if negative == "body_limit" {
				state.oversized = true
			} else {
				state.failResponse = true
			}
			current, err := newLegacyDNSProvider(options)
			if err != nil {
				t.Fatal(err)
			}
			err = current.Present(context.Background(), "example.test", "negative-token", "negative-token.thumbprint")
			if err == nil || strings.Contains(err.Error(), "fixture-private-key") {
				t.Fatal("unsafe response was success or disclosed credentials", err)
			}
			if negative == "body_limit" && core.ErrorCode(err) != "dns_response_limit" {
				t.Fatal("oversized API response was not bounded", err)
			}
			if redirects.Load() != 0 {
				t.Fatal("the authenticated client followed a provider redirect")
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.adds != 0 || state.deletes != 0 {
				t.Fatal("failed API preflight caused a mutation")
			}
		})
	}
}

func TestLegacyReviewedHTTPSQuotedSegmentsAndDecimalEscapesKeepExactCleanup(t *testing.T) {
	for _, providerID := range []string{"dns_zilore", "dns_online"} {
		t.Run(providerID, func(t *testing.T) {
			options, state := legacyReviewedHTTPSFixture(t, providerID)
			provider, err := newLegacyDNSProvider(options)
			if err != nil {
				t.Fatal(err)
			}
			const domain, token, authorization = "example.test", "quoted-token", "quoted-token.thumbprint"
			ctx := context.Background()
			if err := provider.Present(ctx, domain, token, authorization); err != nil {
				t.Fatal(err)
			}
			_, value, _ := provider.(*legacyDNSClient).challenge(domain, token, authorization)
			state.mu.Lock()
			for index := range state.records {
				if state.records[index].id == "1000" {
					state.records[index].wire = fmt.Sprintf("\"\\%03d%s\" \"%s\"", value[0], value[1:len(value)/2], value[len(value)/2:])
				}
			}
			state.mu.Unlock()
			if err := provider.CleanUp(ctx, domain, token, authorization); err != nil {
				t.Fatal("equivalent quoted TXT data was not cleaned with its exact observed wire filter", err)
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.deletes != 1 || len(state.records) != 2 {
				t.Fatal("TXT escape handling deleted a different record")
			}
		})
	}
}
