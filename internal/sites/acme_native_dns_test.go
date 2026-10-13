package sites

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"golang.org/x/crypto/acme"
)

func TestNativeDNSPropagationRequiresExactTXTAndHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	e := &AgentExecutor{nativeDNSLookup: func(ctx context.Context, name string) ([]string, error) {
		if name != "_acme-challenge.example.test" {
			t.Fatal("resolver queried a different DNS identity")
		}
		cancel()
		return []string{"unrelated-value"}, nil
	}}
	if err := e.waitNativeDNSTXT(ctx, "_acme-challenge.example.test", "required-value"); !errors.Is(err, context.Canceled) {
		t.Fatal("unrelated TXT became success or cancellation was ignored", err)
	}
	e.nativeDNSLookup = func(context.Context, string) ([]string, error) {
		return []string{"unrelated-value", "required-value"}, nil
	}
	if err := e.waitNativeDNSTXT(context.Background(), "_acme-challenge.example.test", "required-value"); err != nil {
		t.Fatal(err)
	}
}

type qaNativeRoundTrip func(*http.Request) (*http.Response, error)

func (f qaNativeRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestQARootNativeDNSAlreadyIssuedOrderRecoversWithoutRefinalizing(t *testing.T) {
	e, in, f, _, _, _ := qaNativeDNSACME(t, "dns_knot")
	previous := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/finalize/") {
			f.mu.Lock()
			order := f.orders[strings.TrimPrefix(r.URL.Path, "/finalize/")]
			issued := order != nil && len(order.chain) > 0
			f.mu.Unlock()
			if issued {
				f.mu.Lock()
				f.nonce(w)
				f.reject(w, "valid ACME order must not be finalized again")
				f.mu.Unlock()
				return
			}
			recorder := httptest.NewRecorder()
			previous.ServeHTTP(recorder, r)
			for key, values := range recorder.Header() {
				if key != "Location" {
					w.Header()[key] = values
				}
			}
			var body map[string]any
			if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &body) != nil {
				t.Error("strict CA did not actually finalize the CSR")
				w.WriteHeader(500)
				return
			}
			// The CA has signed the real chain but its accepted response reports
			// processing and omits Location, which RFC8555 permits.
			body["status"] = "processing"
			delete(body, "certificate")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(body)
			return
		}
		previous.ServeHTTP(w, r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := *f.server.Client()
	transport := client.Transport
	client.Transport = qaNativeRoundTrip(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/cert/") {
			cancel()
			return nil, io.ErrUnexpectedEOF
		}
		return transport.RoundTrip(r)
	})
	e.acmeHTTPClient = &client
	if _, err := qaAgentRun(ctx, e, "certificate_acme_issue", in); core.ErrorCode(err) != "acme_issuance_failed" {
		t.Fatal("fault did not interrupt an actually issued certificate download", err)
	}
	f.mu.Lock()
	issued := f.verifiedCSR == 1 && len(f.orders["1"].chain) > 0
	f.mu.Unlock()
	if !issued {
		t.Fatal("CA did not sign the actual CSR before the injected download interruption")
	}
	if _, err := e.certificate(in.ID); !isNotFound(err) {
		t.Fatal("interrupted download published a private certificate", err)
	}
	if _, err := e.getACMEOrder(in.ID); err != nil {
		t.Fatal("issued but unimported order was discarded", err)
	}
	config, runtimes := e.config, e.runtimes
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewAgentExecutor(config, runtimes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	restarted.acmeHTTPClient = f.server.Client()
	in.Kind = "confirm"
	result, err := qaAgentRun(context.Background(), restarted, "certificate_acme_order", in)
	if err != nil || !strings.Contains(string(result.Data), `"state":"issued"`) {
		t.Fatal("valid order was not recovered through its actual certificate URL", err, string(result.Data))
	}
	qaAssertIssuedCertificate(t, restarted, in.ID, f)
	qaAssertACMETrace(t, f, 1, 0)
}

func TestNativeDNSRecoveryRejectsMissingAndReplacedOrderReferences(t *testing.T) {
	state := legacyPrivateDNSState(t)
	if err := state.MkdirAll("acme", 0700); err != nil {
		t.Fatal(err)
	}
	order := acmeOrderState{Version: 1, ID: "certificate-one", Order: &acme.Order{URI: "https://ca.example.test/order/one"}, KeyPEM: "private-fixture-generation", Input: ACMEInput{ID: "certificate-one", Challenge: "dns"}}
	if err := atomicRoot(state, "acme/certificate-one.order.json", encode(order), 0600); err != nil {
		t.Fatal(err)
	}
	e := &AgentExecutor{state: state}
	for _, ref := range []string{"", strings.Repeat("0", 64)} {
		in := request{ID: order.ID, Kind: "cancel", OrderRef: ref, Actor: core.Actor{ID: "fixture-human", Kind: "human"}}
		if _, err := e.Run(context.Background(), agent.Request{Operation: "certificate_acme_order", ActorID: in.Actor.ID, Input: encode(in)}); core.ErrorCode(err) != "acme_order_changed" {
			t.Fatal("root recovery accepted an unspecified or replaced order", err)
		}
		if _, err := e.getACMEOrder(order.ID); err != nil {
			t.Fatal("denied recovery modified the private order", err)
		}
	}
}

// Both protocol endpoints are real local sockets with verified TLS/TSIG.
// Resolver injection routes this fixture to those owned endpoints, and does
// not establish external public DNS or third-party account compatibility.
func qaNativeDNSACME(t *testing.T, provider string) (*AgentExecutor, request, *qaACMECA, legacyDNSOptions, *legacyKnotFixture, *legacyISPFixture) {
	t.Helper()
	e, _ := qaRootExecutor(t)
	var options legacyDNSOptions
	var knot *legacyKnotFixture
	var isp *legacyISPFixture
	names := []string{"example.test"}
	if provider == "dns_knot" {
		options, knot = legacyKnotProtocolFixture(t)
		names = append(names, "*.example.test")
	} else {
		options, isp = legacyISPProtocolFixture(t)
	}
	id := "qa-native-" + strings.TrimPrefix(provider, "dns_")
	options.State, options.OwnerID = e.state, id
	if len(options.AllowedAPIURLs) > 0 {
		e.config.AllowedDNSAPIURLs = map[string][]string{provider: options.AllowedAPIURLs}
	}
	if len(options.AllowedDNSServers) > 0 {
		e.config.AllowedDNSUpdateServers = map[string][]string{provider: options.AllowedDNSServers}
	}
	e.nativeDNSHTTPClient = options.HTTPClient
	probe, err := newLegacyDNSProvider(options)
	if err != nil {
		t.Fatal(err)
	}
	observer := probe.(*legacyDNSClient)
	lookup := func(ctx context.Context, name string) ([]string, error) {
		canonical, err := canonicalLegacyDNSName(name)
		if err != nil {
			return nil, err
		}
		zone, err := observer.backend.zone(ctx, canonical)
		if err != nil {
			return nil, err
		}
		records, err := observer.backend.list(ctx, canonical, zone)
		if err != nil {
			return nil, err
		}
		values := []string{}
		for _, record := range records {
			if record.Name == canonical {
				values = append(values, record.Value)
			}
		}
		return values, nil
	}
	e.nativeDNSLookup = lookup
	f := qaNewACMECA(t, names)
	e.config.AllowedACMEServers = []string{f.server.URL + "/directory"}
	e.acmeHTTPClient = f.server.Client()
	previous := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) == 3 && parts[0] == "challenge" {
			index, err := strconv.Atoi(parts[2])
			f.mu.Lock()
			order := f.orders[parts[1]]
			if err != nil || order == nil || index < 0 || index >= len(order.names) {
				f.reject(w, "invalid owned native DNS authorization")
				f.mu.Unlock()
				return
			}
			token := "plqa-order" + order.id + "-auth" + strconv.Itoa(index)
			name := "_acme-challenge." + strings.TrimPrefix(order.names[index], "*.")
			digest := sha256.Sum256([]byte(token + "." + f.thumbprint))
			f.mu.Unlock()
			values, err := lookup(r.Context(), name)
			if err != nil || !slices.Contains(values, base64.RawURLEncoding.EncodeToString(digest[:])) {
				f.mu.Lock()
				f.reject(w, "native DNS did not contain the actual ACME account-bound TXT")
				f.mu.Unlock()
				return
			}
		}
		previous.ServeHTTP(w, r)
	})
	input := &ACMEInput{ID: id, Domains: names, Email: "qa@example.net", Challenge: "dns", Provider: provider, CredentialID: "qa-bound-dns", CredentialRevision: 1, Server: f.server.URL + "/directory"}
	return e, request{ID: id, ACME: input, Credentials: options.Credentials}, f, options, knot, isp
}

func TestQARootNativeDNSACMERealHTTPSAndSignedTXTThenPrivateImport(t *testing.T) {
	for _, provider := range []string{"dns_knot", "dns_ispconfig"} {
		t.Run(provider, func(t *testing.T) {
			e, in, f, _, knot, isp := qaNativeDNSACME(t, provider)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			result, err := qaAgentRun(ctx, e, "certificate_acme_issue", in)
			if err != nil || !strings.Contains(string(result.Data), `"state":"issued"`) {
				t.Fatal("native DNS did not reach actual private certificate import", err, string(result.Data))
			}
			qaAssertIssuedCertificate(t, e, in.ID, f)
			qaAssertACMETrace(t, f, 1, 0)
			if _, err := e.getACMEOrder(in.ID); !isNotFound(err) {
				t.Fatal("completed native order remained", err)
			}
			entries, err := os.ReadDir(filepath.Join(e.config.StateDir, "acme", "dns-receipts"))
			if err != nil || len(entries) != 0 {
				t.Fatal("owned DNS receipt remained after successful cleanup", err)
			}
			if knot != nil {
				knot.mu.Lock()
				defer knot.mu.Unlock()
				if len(knot.values) != 2 || !slices.Contains(knot.values, "unrelated-existing-value") || !slices.Contains(knot.values, "external-concurrent-value") {
					t.Fatal("signed native cleanup altered unrelated TXT records")
				}
			} else {
				isp.mu.Lock()
				defer isp.mu.Unlock()
				if isp.adds != 1 || isp.deletes != 1 || len(isp.records) != 1 || isp.records[0].ID != "9" {
					t.Fatal("HTTPS native cleanup crossed observed record ownership")
				}
			}
		})
	}
}

func TestQARootNativeDNSCancellationCleansUsingIndependentContext(t *testing.T) {
	e, in, _, _, knot, _ := qaNativeDNSACME(t, "dns_knot")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.nativeDNSLookup = func(context.Context, string) ([]string, error) { cancel(); return nil, context.Canceled }
	if _, err := qaAgentRun(ctx, e, "certificate_acme_issue", in); !errors.Is(err, context.Canceled) {
		t.Fatal("native issuance cancellation was swallowed", err)
	}
	if _, err := e.certificate(in.ID); !isNotFound(err) {
		t.Fatal("cancelled DNS order published a certificate", err)
	}
	if _, err := e.getACMEOrder(in.ID); !isNotFound(err) {
		t.Fatal("cleaned cancelled DNS order retained a false recovery entry", err)
	}
	knot.mu.Lock()
	defer knot.mu.Unlock()
	if len(knot.values) != 2 || !slices.Contains(knot.values, "unrelated-existing-value") || !slices.Contains(knot.values, "external-concurrent-value") {
		t.Fatal("cancellation cleanup failed or deleted unrelated DNS")
	}
}

func TestQARootNativeDNSLostCleanupRetainsOrderAndRecoversAfterRestart(t *testing.T) {
	e, in, _, _, _, isp := qaNativeDNSACME(t, "dns_ispconfig")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.nativeDNSLookup = func(context.Context, string) ([]string, error) {
		isp.mu.Lock()
		isp.fail = true
		isp.mu.Unlock()
		cancel()
		return nil, context.Canceled
	}
	result, err := qaAgentRun(ctx, e, "certificate_acme_issue", in)
	if err == nil || !strings.Contains(string(result.Data), `"state":"recovery_required"`) {
		t.Fatal("failed cleanup discarded recovery state or became success", err, string(result.Data))
	}
	if strings.Contains(string(result.Data), "PRIVATE KEY") || strings.Contains(string(result.Data), "not-a-real-account-password") || strings.Contains(string(result.Data), "sensitive-session") {
		t.Fatal("public native order leaked account or private certificate material")
	}
	if _, err := e.getACMEOrder(in.ID); err != nil {
		t.Fatal("lost cleanup order was not persisted", err)
	}
	if _, err := e.certificate(in.ID); !isNotFound(err) {
		t.Fatal("failed cleanup published a certificate", err)
	}
	if _, err := qaAgentRun(context.Background(), e, "certificate_acme_issue", in); core.ErrorCode(err) != "order_exists" {
		t.Fatal("unresolved DNS order was overwritten", err)
	}
	isp.mu.Lock()
	isp.fail = false
	isp.mu.Unlock()
	config, runtimes := e.config, e.runtimes
	caHTTP, dnsHTTP := e.acmeHTTPClient, e.nativeDNSHTTPClient
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewAgentExecutor(config, runtimes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	restarted.acmeHTTPClient, restarted.nativeDNSHTTPClient = caHTTP, dnsHTTP
	denied := request{ID: in.ID, Kind: "cancel"}
	if _, err := qaAgentRun(context.Background(), restarted, "certificate_acme_order", denied); core.ErrorCode(err) != "dns_recovery_credentials_required" {
		t.Fatal("native recovery accepted no credential binding", err)
	}
	in.Kind = "cancel"
	result, err = qaAgentRun(context.Background(), restarted, "certificate_acme_order", in)
	if err != nil || !strings.Contains(string(result.Data), `"state":"cancelled"`) {
		t.Fatal("native restart cleanup did not complete", err, string(result.Data))
	}
	isp.mu.Lock()
	defer isp.mu.Unlock()
	if isp.deletes != 1 || len(isp.records) != 1 || isp.records[0].ID != "9" {
		t.Fatal("restart cleanup changed unrelated DNS records")
	}
}
