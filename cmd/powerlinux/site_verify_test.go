package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/sites"
)

func TestWebsiteVerifierRequiresActuallyServedRevision(t *testing.T) {
	var revision atomic.Int64
	revision.Store(1)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Host != "panel.example" || r.URL.Path != "/.__powerlinux_health" {
			t.Error("untrusted probe target", r.Host, r.URL.Path)
		}
		w.Header().Set("X-Folio-Site", "site")
		if revision.Load() == 2 {
			w.Header().Set("X-Folio-Revision", "2")
		} else {
			w.Header().Set("X-Folio-Revision", "1")
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	port := server.URL[strings.LastIndex(server.URL, ":")+1:]
	site := sites.Site{ID: "site", Revision: 2, Domains: []string{"panel.example:" + port}}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	if err := (siteRuntimes{}).VerifySite(ctx, site); err == nil {
		t.Fatal("successful reload signal accepted a stale running revision")
	}
	cancel()
	revision.Store(2)
	if err := (siteRuntimes{}).VerifySite(context.Background(), site); err != nil {
		t.Fatal(err)
	}
	if requests.Load() < 2 {
		t.Fatal("running server was not probed")
	}
}

func TestWebsiteVerifierAcceptsLegacyManagedSiteHeadersDuringUpgrade(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-PowerLinux-Site", "legacy-site")
		w.Header().Set("X-PowerLinux-Revision", "4")
		w.WriteHeader(204)
	}))
	defer server.Close()
	port := server.URL[strings.LastIndex(server.URL, ":")+1:]
	site := sites.Site{ID: "legacy-site", Revision: 4, Domains: []string{"legacy.example:" + port}}
	if err := (siteRuntimes{}).VerifySite(context.Background(), site); err != nil {
		t.Fatalf("legacy generated site was not accepted for upgrade: %v", err)
	}
}

func TestWebsiteTLSVerifierRejectsOlderValidLeafFromSameIssuer(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Isolated test issuer"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	makeLeaf := func(serial int64) (tls.Certificate, []byte) {
		t.Helper()
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{"panel.example"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		body := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
		return tls.Certificate{Certificate: [][]byte{der, caDER}, PrivateKey: leafKey}, body
	}
	oldLeaf, _ := makeLeaf(2)
	newLeaf, deployed := makeLeaf(3)
	var selected atomic.Pointer[tls.Certificate]
	selected.Store(&oldLeaf)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return selected.Load(), nil }}
	server.StartTLS()
	defer server.Close()
	config, err := websiteTLSConfig(deployed, "panel.example")
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSClientConfig: config}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	if response, err := client.Get(server.URL); err == nil {
		response.Body.Close()
		t.Fatal("older valid leaf from the trusted issuer was accepted as a successful renewal")
	}
	selected.Store(&newLeaf)
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal("new deployed leaf was rejected", err)
	}
	response.Body.Close()
	wrongHost, err := websiteTLSConfig(deployed, "other.example")
	if err != nil {
		t.Fatal(err)
	}
	transport.TLSClientConfig = wrongHost
	if response, err := client.Get(server.URL); err == nil {
		response.Body.Close()
		t.Fatal("host verification was bypassed by the fingerprint check")
	}
}
