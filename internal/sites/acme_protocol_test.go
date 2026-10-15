package sites

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

// This CA implements a strict local RFC8555 HTTPS protocol fixture. It verifies
// real ES256 JWS, single-use nonces, URL/account binding and CSR proof of key
// possession, then signs a real test chain. It makes no external CA/DNS claim.
type qaACMEOrder struct {
	id    string
	names []string
	valid []bool
	chain []byte
}
type qaACMECA struct {
	mu                                     sync.Mutex
	server                                 *httptest.Server
	nonces                                 map[string]bool
	account                                *ecdsa.PublicKey
	thumbprint                             string
	orders                                 map[string]*qaACMEOrder
	names                                  []string
	ca                                     *x509.Certificate
	caKey                                  *ecdsa.PrivateKey
	caPEM                                  []byte
	webroot                                string
	verifiedJWS, verifiedCSR, verifiedHTTP int
	failures                               []string
	challengeStarted, challengeContinue    chan struct{}
	blockOnce                              sync.Once
}

func qaNewACMECA(t *testing.T, names []string) *qaACMECA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(9001), Subject: pkix.Name{CommonName: "PowerLinux owned local protocol CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	f := &qaACMECA{nonces: map[string]bool{}, orders: map[string]*qaACMEOrder{}, names: names, ca: cert, caKey: key, caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	transport := f.server.Client().Transport.(*http.Transport)
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("protocol fixture must verify its explicitly trusted HTTPS certificate")
	}
	return f
}

func (f *qaACMECA) nonce(w http.ResponseWriter) {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(value[:])
	f.nonces[encoded] = true
	w.Header().Set("Replay-Nonce", encoded)
}
func (f *qaACMECA) reject(w http.ResponseWriter, reason string) {
	f.failures = append(f.failures, reason)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(400)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "urn:ietf:params:acme:error:malformed", "detail": "strict QA protocol verification rejected request"})
}
func (f *qaACMECA) verify(r *http.Request) ([]byte, error) {
	if r.Method != "POST" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/jose+json") {
		return nil, errors.New("POST/JWS content type required")
	}
	var wire struct{ Protected, Payload, Signature string }
	decoder := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return nil, errors.New("invalid JWS envelope")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("trailing JWS document")
	}
	protected, err := base64.RawURLEncoding.DecodeString(wire.Protected)
	if err != nil {
		return nil, err
	}
	var header struct {
		Alg, Nonce, URL, Kid string
		JWK                  *struct{ Kty, Crv, X, Y string }
	}
	if json.Unmarshal(protected, &header) != nil || header.Alg != "ES256" || header.URL != f.server.URL+r.URL.RequestURI() {
		return nil, errors.New("JWS algorithm or URL mismatch")
	}
	if !f.nonces[header.Nonce] {
		return nil, errors.New("unknown/replayed nonce")
	}
	delete(f.nonces, header.Nonce)
	key := f.account
	if header.JWK != nil {
		if header.Kid != "" || r.URL.Path != "/account" || header.JWK.Kty != "EC" || header.JWK.Crv != "P-256" {
			return nil, errors.New("unexpected embedded JWK")
		}
		x, xe := base64.RawURLEncoding.DecodeString(header.JWK.X)
		y, ye := base64.RawURLEncoding.DecodeString(header.JWK.Y)
		if xe != nil || ye != nil || len(x) != 32 || len(y) != 32 {
			return nil, errors.New("invalid JWK coordinates")
		}
		key = &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !key.Curve.IsOnCurve(key.X, key.Y) {
			return nil, errors.New("JWK point not on curve")
		}
		if f.account != nil && (f.account.X.Cmp(key.X) != 0 || f.account.Y.Cmp(key.Y) != 0) {
			return nil, errors.New("account key changed")
		}
	} else if key == nil || header.Kid != f.server.URL+"/account/1" {
		return nil, errors.New("wrong or missing account KID")
	}
	signature, err := base64.RawURLEncoding.DecodeString(wire.Signature)
	if err != nil || len(signature) != 64 {
		return nil, errors.New("invalid ES256 signature encoding")
	}
	digest := sha256.Sum256([]byte(wire.Protected + "." + wire.Payload))
	if !ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		return nil, errors.New("JWS signature failed")
	}
	if header.JWK != nil && f.account == nil {
		f.account = key
		canonical := fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":%q,"y":%q}`, header.JWK.X, header.JWK.Y)
		hash := sha256.Sum256([]byte(canonical))
		f.thumbprint = base64.RawURLEncoding.EncodeToString(hash[:])
	}
	payload, err := base64.RawURLEncoding.DecodeString(wire.Payload)
	if err == nil {
		f.verifiedJWS++
	}
	return payload, err
}

func (f *qaACMECA) orderJSON(o *qaACMEOrder) map[string]any {
	status := "ready"
	for _, valid := range o.valid {
		if !valid {
			status = "pending"
		}
	}
	if len(o.chain) > 0 {
		status = "valid"
	}
	auth := []string{}
	identifiers := []map[string]string{}
	for i, name := range o.names {
		auth = append(auth, fmt.Sprintf("%s/authz/%s/%d", f.server.URL, o.id, i))
		identifiers = append(identifiers, map[string]string{"type": "dns", "value": name})
	}
	value := map[string]any{"status": status, "expires": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "identifiers": identifiers, "authorizations": auth, "finalize": f.server.URL + "/finalize/" + o.id}
	if len(o.chain) > 0 {
		value["certificate"] = f.server.URL + "/cert/" + o.id
	}
	return value
}
func (f *qaACMECA) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nonce(w)
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/directory" && r.Method == "GET" {
		_ = json.NewEncoder(w).Encode(map[string]any{"newNonce": f.server.URL + "/nonce", "newAccount": f.server.URL + "/account", "newOrder": f.server.URL + "/new-order", "meta": map[string]string{"termsOfService": f.server.URL + "/terms"}})
		return
	}
	if r.URL.Path == "/nonce" && r.Method == "HEAD" {
		w.WriteHeader(204)
		return
	}
	payload, err := f.verify(r)
	if err != nil {
		f.reject(w, err.Error())
		return
	}
	jsonReply := func(status int, data any) { w.WriteHeader(status); _ = json.NewEncoder(w).Encode(data) }
	switch r.URL.Path {
	case "/account":
		var in struct {
			Contact                                  []string
			TermsOfServiceAgreed, OnlyReturnExisting bool
		}
		if json.Unmarshal(payload, &in) != nil || (!in.OnlyReturnExisting && (!in.TermsOfServiceAgreed || !reflect.DeepEqual(in.Contact, []string{"mailto:qa@example.net"}))) {
			f.reject(w, "invalid ACME account agreement/contact")
			return
		}
		w.Header().Set("Location", f.server.URL+"/account/1")
		code := 201
		if in.OnlyReturnExisting {
			code = 200
		}
		jsonReply(code, map[string]any{"status": "valid", "contact": []string{"mailto:qa@example.net"}, "orders": f.server.URL + "/accounts/1/orders"})
		return
	case "/new-order":
		var in struct {
			Identifiers []struct{ Type, Value string }
		}
		if json.Unmarshal(payload, &in) != nil {
			f.reject(w, "invalid order identifiers")
			return
		}
		names := []string{}
		for _, id := range in.Identifiers {
			if id.Type != "dns" {
				f.reject(w, "order identifier is not DNS")
				return
			}
			names = append(names, id.Value)
		}
		if !reflect.DeepEqual(names, f.names) {
			f.reject(w, "order identifiers differ from authorized names")
			return
		}
		id := strconv.Itoa(len(f.orders) + 1)
		order := &qaACMEOrder{id: id, names: names, valid: make([]bool, len(names))}
		f.orders[id] = order
		w.Header().Set("Location", f.server.URL+"/order/"+id)
		jsonReply(201, f.orderJSON(order))
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 || f.orders[parts[1]] == nil {
		f.reject(w, "unknown ACME resource")
		return
	}
	o := f.orders[parts[1]]
	switch parts[0] {
	case "authz", "challenge":
		if len(parts) != 3 {
			f.reject(w, "invalid authorization URL")
			return
		}
		index, err := strconv.Atoi(parts[2])
		if err != nil || index < 0 || index >= len(o.names) {
			f.reject(w, "authorization index out of range")
			return
		}
		token := "plqa-order" + o.id + "-auth" + strconv.Itoa(index)
		if parts[0] == "challenge" {
			if string(payload) != "{}" {
				f.reject(w, "challenge acknowledgement must be an object")
				return
			}
			if f.webroot != "" {
				res, err := http.Get(f.webroot + "/.well-known/acme-challenge/" + token)
				if err != nil {
					f.reject(w, "actual webroot HTTP fetch failed")
					return
				}
				body, readErr := io.ReadAll(io.LimitReader(res.Body, 2048))
				res.Body.Close()
				if readErr != nil || res.StatusCode != 200 || string(body) != token+"."+f.thumbprint {
					f.reject(w, "actual HTTP01 key authorization is wrong")
					return
				}
				f.verifiedHTTP++
			}
			if f.challengeStarted != nil {
				f.blockOnce.Do(func() { close(f.challengeStarted) })
				select {
				case <-f.challengeContinue:
				case <-r.Context().Done():
					f.reject(w, "challenge fixture canceled")
					return
				}
			}
			o.valid[index] = true
			kind := "dns-01"
			if f.webroot != "" {
				kind = "http-01"
			}
			jsonReply(200, map[string]any{"type": kind, "url": f.server.URL + r.URL.Path, "status": "valid", "token": token})
			return
		}
		if len(payload) != 0 {
			f.reject(w, "authorization must use POST-as-GET")
			return
		}
		status := "pending"
		if o.valid[index] {
			status = "valid"
		}
		challengeURL := f.server.URL + "/challenge/" + o.id + "/" + strconv.Itoa(index)
		jsonReply(200, map[string]any{"status": status, "identifier": map[string]string{"type": "dns", "value": strings.TrimPrefix(o.names[index], "*.")}, "wildcard": strings.HasPrefix(o.names[index], "*."), "challenges": []map[string]any{{"type": "http-01", "url": challengeURL, "status": status, "token": token}, {"type": "dns-01", "url": challengeURL, "status": status, "token": token}}})
		return
	case "order":
		if len(payload) != 0 {
			f.reject(w, "order must use POST-as-GET")
			return
		}
		w.Header().Set("Location", f.server.URL+r.URL.Path)
		jsonReply(200, f.orderJSON(o))
		return
	case "finalize":
		for _, valid := range o.valid {
			if !valid {
				f.reject(w, "CSR finalized before authorization")
				return
			}
		}
		var in struct{ CSR string }
		if json.Unmarshal(payload, &in) != nil {
			f.reject(w, "invalid finalize object")
			return
		}
		der, err := base64.RawURLEncoding.DecodeString(in.CSR)
		if err != nil {
			f.reject(w, "invalid CSR encoding")
			return
		}
		csr, err := x509.ParseCertificateRequest(der)
		if err != nil || csr.CheckSignature() != nil || !reflect.DeepEqual(csr.DNSNames, o.names) || len(csr.IPAddresses) > 0 {
			f.reject(w, "CSR signature or SAN authorization failed")
			return
		}
		serial, _ := strconv.Atoi(o.id)
		leaf := &x509.Certificate{SerialNumber: big.NewInt(int64(serial)), Subject: pkix.Name{CommonName: "local protocol certificate"}, DNSNames: csr.DNSNames, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		signed, err := x509.CreateCertificate(rand.Reader, leaf, f.ca, csr.PublicKey, f.caKey)
		if err != nil {
			f.reject(w, "local fixture certificate signing failed")
			return
		}
		o.chain = append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: signed}), f.caPEM...)
		f.verifiedCSR++
		w.Header().Set("Location", f.server.URL+"/order/"+o.id)
		jsonReply(200, f.orderJSON(o))
		return
	case "cert":
		if len(payload) != 0 || len(o.chain) == 0 {
			f.reject(w, "certificate is not ready or POST-as-GET is invalid")
			return
		}
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		w.WriteHeader(200)
		_, _ = w.Write(o.chain)
		return
	default:
		f.reject(w, "unexpected ACME endpoint")
	}
}

func qaAssertACMETrace(t *testing.T, f *qaACMECA, certs, httpChallenges int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.failures) > 0 {
		t.Fatalf("strict real HTTPS protocol rejected client: %v", f.failures)
	}
	if f.verifiedJWS < 5 || f.verifiedCSR != certs || f.verifiedHTTP != httpChallenges {
		t.Fatalf("real JWS/CSR/HTTP proof counts: %d/%d/%d", f.verifiedJWS, f.verifiedCSR, f.verifiedHTTP)
	}
}
func qaAssertIssuedCertificate(t *testing.T, e *AgentExecutor, id string, f *qaACMECA) {
	t.Helper()
	meta, err := e.certificate(id)
	if err != nil || meta.Source != "acme" || meta.Revision != 1 {
		t.Fatal("actual imported ACME receipt incorrect", err)
	}
	certPath := filepath.Join(e.config.StateDir, "certificates", id+".crt")
	keyPath := filepath.Join(e.config.StateDir, "certificates", id+".key")
	for _, path := range []string{certPath, keyPath, filepath.Join(e.config.StateDir, "certificates", id+".json")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("actual certificate material mode is not private", err)
		}
	}
	chain, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(chain)
	if block == nil {
		t.Fatal("actual certificate file is not PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(f.ca)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: strings.TrimPrefix(f.names[0], "*.")}); err != nil {
		t.Fatal("actual chain did not verify against explicitly trusted local CA", err)
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inspectCertificate(id, "QA", string(chain), string(key)); err != nil {
		t.Fatal("actual stored private key does not match issued certificate", err)
	}
}

func TestQAACMEPolicyRejectsUntrustedDirectoriesAndInvalidIdentifiers(t *testing.T) {
	config := AgentConfig{AllowedACMEServers: []string{"https://qa.example.net/directory"}}
	for _, mutate := range []func(*ACMEInput){func(in *ACMEInput) { in.Server = "http://qa.example.net/directory" }, func(in *ACMEInput) { in.Server = "https://unknown.example.net/directory" }, func(in *ACMEInput) { in.Server = "https://name:secret@qa.example.net/directory" }, func(in *ACMEInput) { in.Domains = []string{"example.net:80"} }, func(in *ACMEInput) { in.Domains = []string{"127.0.0.1"} }, func(in *ACMEInput) { in.Domains = []string{"*.example.net"}; in.Challenge = "http_webroot" }, func(in *ACMEInput) { in.Domains = []string{"*.example.net"}; in.Challenge = "manual_http" }, func(in *ACMEInput) { in.SiteID = "owned-site"; in.Challenge = "manual_http" }, func(in *ACMEInput) { in.CredentialID = "dns-secret"; in.Challenge = "manual_http" }, func(in *ACMEInput) { in.Domains = []string{"EXAMPLE.net", "example.net"} }} {
		in := ACMEInput{ID: "qa-cert", Domains: []string{"example.net"}, Email: "qa@example.net", Challenge: "manual_dns", Server: "https://qa.example.net/directory"}
		mutate(&in)
		if validateACME(&in, config) == nil {
			t.Fatal("untrusted/invalid ACME input accepted")
		}
	}
}

func TestQARootVMACMEManualHTTPUploadSurvivesRestartAndConfirmsOnlyOnRequest(t *testing.T) {
	e, runtime := qaRootExecutor(t)
	f := qaNewACMECA(t, []string{"example.net"})
	e.config.AllowedACMEServers = []string{f.server.URL + "/directory"}
	e.acmeHTTPClient = f.server.Client()
	var uploadMu sync.Mutex
	uploads := map[string]string{}
	uploadReads := 0
	webroot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploadMu.Lock()
		defer uploadMu.Unlock()
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, ok := uploads[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		uploadReads++
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(webroot.Close)
	f.webroot = webroot.URL

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	input := &ACMEInput{ID: "qa-manual-http", Domains: []string{"example.net"}, Email: "qa@example.net", Challenge: "manual_http", Server: f.server.URL + "/directory"}
	result, err := qaAgentRun(ctx, e, "certificate_acme_issue", request{ID: input.ID, ACME: input})
	if err != nil {
		t.Fatal(err)
	}
	var pending struct {
		State          string `json:"state"`
		OrderRef       string `json:"order_ref"`
		HTTPChallenges []struct {
			Domain string `json:"domain"`
			URL    string `json:"url"`
			Path   string `json:"path"`
			Body   string `json:"body"`
		} `json:"http_challenges"`
	}
	if json.Unmarshal(result.Data, &pending) != nil || pending.State != "pending_http" || pending.OrderRef == "" || len(pending.HTTPChallenges) != 1 {
		t.Fatalf("manual HTTP upload instructions were not returned: %s", result.Data)
	}
	challenge := pending.HTTPChallenges[0]
	wantBody := "plqa-order1-auth0." + f.thumbprint
	if challenge.Domain != "example.net" || challenge.URL != "http://example.net"+challenge.Path || challenge.Path != "/.well-known/acme-challenge/plqa-order1-auth0" || challenge.Body != wantBody {
		t.Fatalf("manual HTTP instructions do not match the ACME challenge: %#v", challenge)
	}
	if strings.Contains(string(result.Data), "authorization_url") || strings.Contains(string(result.Data), "PRIVATE KEY") {
		t.Fatal("manual HTTP public instructions exposed protected ACME order material")
	}
	f.mu.Lock()
	acceptedBeforeUpload := f.orders["1"].valid[0] || f.verifiedHTTP != 0
	f.mu.Unlock()
	if acceptedBeforeUpload {
		t.Fatal("manual HTTP issue accepted or validated the challenge before user confirmation")
	}
	uploadMu.Lock()
	uploads[challenge.Path] = challenge.Body
	uploadMu.Unlock()
	orderPath := filepath.Join(e.config.StateDir, "acme", input.ID+".order.json")
	info, err := os.Stat(orderPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("manual HTTP order was not durably protected", err)
	}

	config := e.config
	e.Close()
	restarted, err := NewAgentExecutor(config, runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	restarted.acmeHTTPClient = f.server.Client()
	status, err := qaAgentRun(ctx, restarted, "certificate_acme_order", request{ID: input.ID, Kind: "status"})
	if err != nil || string(status.Data) != string(result.Data) {
		t.Fatal("manual HTTP pending upload instructions did not survive restart", err)
	}
	if _, err = qaAgentRun(ctx, restarted, "certificate_acme_order", request{ID: input.ID, Kind: "confirm", OrderRef: pending.OrderRef}); err != nil {
		f.mu.Lock()
		reasons := append([]string{}, f.failures...)
		f.mu.Unlock()
		t.Fatalf("manual HTTP confirm failed: %v; fixture failures: %v", err, reasons)
	}
	qaAssertIssuedCertificate(t, restarted, input.ID, f)
	uploadMu.Lock()
	if uploadReads != 1 {
		t.Fatalf("CA fixture did not make exactly one local HTTP validation request: %d", uploadReads)
	}
	uploadMu.Unlock()
	qaAssertACMETrace(t, f, 1, 1)

	second := *input
	second.ID = "qa-manual-http-tamper"
	result, err = qaAgentRun(ctx, restarted, "certificate_acme_issue", request{ID: second.ID, ACME: &second})
	if err != nil {
		t.Fatal(err)
	}
	var tampered acmeOrderState
	path := filepath.Join("acme", second.ID+".order.json")
	if err = rootJSON(restarted.state, path, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered.HTTPChallenges[0].Body = "changed-body"
	if err = atomicRoot(restarted.state, path, encode(tampered), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = qaAgentRun(ctx, restarted, "certificate_acme_order", request{ID: second.ID, Kind: "confirm", OrderRef: acmeOrderRef(tampered)}); core.ErrorCode(err) != "metadata_invalid" {
		t.Fatal("tampered manual HTTP body was not rejected before ACME acceptance", err)
	}
	f.mu.Lock()
	tamperAccepted := f.orders["2"].valid[0]
	f.mu.Unlock()
	if tamperAccepted {
		t.Fatal("tampered HTTP body reached ACME challenge acceptance")
	}
	if _, err = qaAgentRun(ctx, restarted, "certificate_acme_order", request{ID: second.ID, Kind: "cancel", OrderRef: acmeOrderRef(tampered)}); err != nil {
		t.Fatal("could not cancel tampered manual HTTP order", err)
	}
	if _, err = qaAgentRun(ctx, restarted, "certificate_acme_order", request{ID: second.ID, Kind: "status"}); core.ErrorCode(err) != "not_found" {
		t.Fatal("canceled manual HTTP order remained discoverable", err)
	}
	qaAssertACMETrace(t, f, 1, 1)
}

func TestQARootVMACMEHTTPRealJWSCSRAndConcurrentWebsiteRead(t *testing.T) {
	e, _ := qaRootExecutor(t)
	f := qaNewACMECA(t, []string{"example.net"})
	e.config.AllowedACMEServers = []string{f.server.URL + "/directory"}
	e.acmeHTTPClient = f.server.Client()
	site := qaRenderSite()
	site.ID = "qa-acme-http"
	site.DocumentRoot = e.paths(site).DocumentRoot
	if err := os.MkdirAll(site.DocumentRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := atomicRoot(e.state, filepath.Join("sites", site.ID+".json"), encode(site), 0600); err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(http.FileServer(http.Dir(site.DocumentRoot)))
	t.Cleanup(web.Close)
	f.webroot = web.URL
	f.challengeStarted = make(chan struct{})
	f.challengeContinue = make(chan struct{})
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(f.challengeContinue) }) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	input := &ACMEInput{ID: "qa-http-certificate", SiteID: site.ID, Domains: f.names, Email: "qa@example.net", Challenge: "http_webroot", Server: f.server.URL + "/directory"}
	complete := make(chan error, 1)
	go func() {
		_, err := qaAgentRun(ctx, e, "certificate_acme_issue", request{ID: input.ID, ACME: input})
		complete <- err
	}()
	select {
	case <-f.challengeStarted:
	case err := <-complete:
		t.Fatal("issuance did not reach real HTTP challenge", err)
	case <-ctx.Done():
		t.Fatal("real challenge fixture did not start")
	}
	read := make(chan error, 1)
	go func() {
		result, err := qaAgentRun(ctx, e, "site_get", request{ID: site.ID})
		if err == nil {
			var value Site
			if json.Unmarshal(result.Data, &value) != nil || value.ID != site.ID {
				err = errors.New("actual website read returned wrong receipt")
			}
		}
		read <- err
	}()
	select {
	case err := <-read:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(3 * time.Second):
		t.Error("long ACME network call blocked independent site_get")
	}
	unblock.Do(func() { close(f.challengeContinue) })
	if err := <-complete; err != nil {
		t.Fatal(err)
	}
	qaAssertIssuedCertificate(t, e, input.ID, f)
	qaAssertACMETrace(t, f, 1, 1)
	challengeDirectory := filepath.Join(site.DocumentRoot, ".well-known/acme-challenge")
	entries, err := os.ReadDir(challengeDirectory)
	if err != nil || len(entries) != 0 {
		t.Fatal("actual HTTP challenge file was not cleaned", err)
	}
}

func TestQARootVMACMEManualDNSPendingSurvivesRestartConfirmsAndCancels(t *testing.T) {
	e, runtime := qaRootExecutor(t)
	f := qaNewACMECA(t, []string{"example.net", "*.example.net"})
	e.config.AllowedACMEServers = []string{f.server.URL + "/directory"}
	e.acmeHTTPClient = f.server.Client()
	input := &ACMEInput{ID: "qa-manual-cert", Domains: f.names, Email: "qa@example.net", Challenge: "manual_dns", Server: f.server.URL + "/directory"}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	result, err := qaAgentRun(ctx, e, "certificate_acme_issue", request{ID: input.ID, ACME: input})
	if err != nil {
		t.Fatal(err)
	}
	var pending struct {
		State   string
		Records []struct{ Name, Type, Value string }
	}
	if json.Unmarshal(result.Data, &pending) != nil || pending.State != "pending_dns" || len(pending.Records) != 2 {
		t.Fatal("manual DNS pending records not returned")
	}
	if strings.Contains(string(result.Data), "PRIVATE KEY") {
		t.Fatal("pending response exposed private key")
	}
	for i, record := range pending.Records {
		if record.Name != "_acme-challenge.example.net" || record.Type != "TXT" {
			t.Fatal("incorrect wildcard manual TXT name")
		}
		expected := sha256.Sum256([]byte(fmt.Sprintf("plqa-order1-auth%d.%s", i, f.thumbprint)))
		if record.Value != base64.RawURLEncoding.EncodeToString(expected[:]) {
			t.Fatal("manual TXT value does not match verified account thumbprint")
		}
	}
	orderPath := filepath.Join(e.config.StateDir, "acme", input.ID+".order.json")
	info, err := os.Stat(orderPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("actual manual pending record is not private", err)
	}
	config := e.config
	e.Close()
	restarted, err := NewAgentExecutor(config, runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	restarted.acmeHTTPClient = f.server.Client()
	status, err := qaAgentRun(ctx, restarted, "certificate_acme_order", request{ID: input.ID, Kind: "status"})
	if err != nil {
		t.Fatal(err)
	}
	if string(status.Data) != string(result.Data) {
		t.Fatal("actual persisted manual status changed through restart")
	}
	if _, err := qaAgentRun(ctx, restarted, "certificate_acme_order", request{ID: input.ID, Kind: "confirm"}); err != nil {
		f.mu.Lock()
		reasons := append([]string{}, f.failures...)
		f.mu.Unlock()
		t.Fatalf("%v; strict protocol reasons: %v", err, reasons)
	}
	qaAssertIssuedCertificate(t, restarted, input.ID, f)
	if _, err := os.Stat(orderPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("confirmed pending record still exists")
	}
	second := *input
	second.ID = "qa-manual-cancel"
	if _, err := qaAgentRun(ctx, restarted, "certificate_acme_issue", request{ID: second.ID, ACME: &second}); err != nil {
		t.Fatal(err)
	}
	cancelled, err := qaAgentRun(ctx, restarted, "certificate_acme_order", request{ID: second.ID, Kind: "cancel"})
	if err != nil || !strings.Contains(string(cancelled.Data), `"cancelled"`) {
		t.Fatal("manual cancel failed", err)
	}
	if _, err := qaAgentRun(ctx, restarted, "certificate_acme_order", request{ID: second.ID, Kind: "status"}); core.ErrorCode(err) != "not_found" {
		t.Fatal("cancelled order retained discoverable state", err)
	}
	qaAssertACMETrace(t, f, 1, 0)
}
