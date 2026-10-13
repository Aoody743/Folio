package sites

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func ownedCertificateMaterial(t *testing.T, serial int64) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{"example.net"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Duration(serial) * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))
}

type certificateProbeFixture struct {
	*qaRuntimeFixture
	rejectNext bool
	probes     int
}

func (probe *certificateProbeFixture) VerifySite(context.Context, Site) error {
	probe.probes++
	if probe.rejectNext {
		probe.rejectNext = false
		return errors.New("isolated probe fixture rejects stale runtime")
	}
	return nil
}

func TestQARootCertificateRenewalRollsBackOnProbeFailureAndKeepsConfigGeneration(t *testing.T) {
	e, runtime := qaRootExecutor(t)
	site := rootOwnedTestSite(t)
	oldCert, oldKey := ownedCertificateMaterial(t, 24)
	newCert, newKey := ownedCertificateMaterial(t, 48)
	certificateID := "qa-cert"
	meta := Certificate{ID: certificateID, Name: "QA isolated certificate"}
	if _, err := qaAgentRun(context.Background(), e, "certificate_import", request{ID: certificateID, Certificate: &meta, CertPEM: oldCert, KeyPEM: oldKey}); err != nil {
		t.Fatal(err)
	}
	site.TLS = TLSBinding{Enabled: true, CertificateID: certificateID}
	created, err := qaAgentRun(context.Background(), e, "site_apply", request{ID: site.ID, Site: &site})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(created.Data, &site); err != nil {
		t.Fatal(err)
	}
	beforeConfig, err := os.ReadFile(filepath.Join(runtime.web.SnippetsDir, site.ID+".conf"))
	if err != nil {
		t.Fatal(err)
	}
	probe := &certificateProbeFixture{qaRuntimeFixture: runtime, rejectNext: true}
	e.runtimes = probe
	renew := request{ID: certificateID, Certificate: &meta, CertPEM: newCert, KeyPEM: newKey, ExpectedRevision: 1, ACME: &ACMEInput{Renew: true}, AuthorizedSites: []string{site.ID}}
	if _, err = qaAgentRun(context.Background(), e, "certificate_import", renew); core.ErrorCode(err) != "runtime_not_applied" {
		t.Fatal("stale served certificate accepted", err)
	}
	actualCert, _ := e.state.ReadFile(filepath.Join("certificates", certificateID+".crt"))
	actualKey, _ := e.state.ReadFile(filepath.Join("certificates", certificateID+".key"))
	if string(actualCert) != oldCert || string(actualKey) != oldKey {
		t.Fatal("failed renewal did not restore exact certificate/key bytes")
	}
	current, err := e.get(site.ID)
	if err != nil || current.Revision != site.Revision || current.TLS.CertificateRevision != 1 {
		t.Fatal("failed renewal changed website metadata", err)
	}
	if _, err = qaAgentRun(context.Background(), e, "certificate_import", renew); err != nil {
		t.Fatal(err)
	}
	current, err = e.get(site.ID)
	if err != nil || current.Revision != site.Revision || current.TLS.CertificateRevision != 2 || current.TLS.ExpiresAt == site.TLS.ExpiresAt {
		t.Fatal("certificate revision and derived metadata inconsistent", err)
	}
	afterConfig, err := os.ReadFile(filepath.Join(runtime.web.SnippetsDir, site.ID+".conf"))
	if err != nil || string(afterConfig) != string(beforeConfig) || probe.probes != 2 {
		t.Fatal("renewal changed site configuration generation or omitted running-state checks", err)
	}
	// Actual TLS handshake/fingerprint verification has separate CLI tests;
	// these root fixtures prove protected file/metadata transaction rollback.
}

func TestQARootAdvancedConfigRequiresExplicitResetForTypedSettings(t *testing.T) {
	e, runtime := qaRootExecutor(t)
	site := rootOwnedTestSite(t)
	created, err := qaAgentRun(context.Background(), e, "site_apply", request{ID: site.ID, Site: &site})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(created.Data, &site); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(runtime.web.SnippetsDir, site.ID+".conf"))
	if err != nil {
		t.Fatal(err)
	}
	e.config.AllowAdvancedConfig = true
	advanced, err := qaAgentRun(context.Background(), e, "site_config_set", request{ID: site.ID, ExpectedRevision: site.Revision, Config: string(body)})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(advanced.Data, &site); err != nil {
		t.Fatal(err)
	}
	if _, err = qaAgentRun(context.Background(), e, "site_apply", request{ID: site.ID, Site: &site, ExpectedRevision: site.Revision}); core.ErrorCode(err) != "advanced_configuration_active" {
		t.Fatal("typed settings silently replaced custom configuration", err)
	}
	reset, err := qaAgentRun(context.Background(), e, "site_apply", request{ID: site.ID, Site: &site, ExpectedRevision: site.Revision, ResetAdvancedConfig: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(reset.Data, &site); err != nil || site.ConfigurationMode != "managed" {
		t.Fatal("explicit reset did not restore managed generation", err)
	}
}
