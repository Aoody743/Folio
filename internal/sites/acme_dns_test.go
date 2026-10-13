package sites

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDNSCatalogKeepsEveryLegacyProviderAndExplicitUnavailableAdapters(t *testing.T) {
	specs := DNSProviders()
	if len(specs) != 34 {
		t.Fatalf("legacy DNS union contains %d providers, want 34", len(specs))
	}
	seen := map[string]bool{}
	for _, spec := range specs {
		if seen[spec.ID] || spec.Name == "" || len(spec.Fields) == 0 || spec.Reason == "" {
			t.Fatalf("invalid DNS provider spec %+v", spec)
		}
		seen[spec.ID] = true
		if spec.Available && spec.ClientProvider == "" && (spec.Driver != "native" || legacyDNSFactories[spec.ID] == nil) {
			t.Fatal("available adapter has no native client", spec)
		}
		if !spec.Available && ValidateDNSCredentials(spec.ID, map[string]string{spec.Fields[0]: "fixture"}) == nil {
			t.Fatal("unavailable adapter accepted credentials", spec.ID)
		}
	}
	if !seen["dns_huaweicloud"] || !seen["dns_tencent"] || !seen["dns_dpi"] {
		t.Fatal("core-only/international provider disappeared")
	}
	specs[0].Fields[0] = "mutated"
	if DNSProviders()[0].Fields[0] == "mutated" {
		t.Fatal("caller mutated provider registry")
	}
}
func TestDNSCredentialSchemasRejectSecretsInUnsupportedOptionsAndRequireTLS(t *testing.T) {
	for _, test := range []struct {
		id     string
		fields map[string]string
	}{{"dns_cf", map[string]string{"CF_Token": "fixture", "LD_PRELOAD": "evil.so"}}, {"dns_cf", map[string]string{"CF_Token": "fixture\npoison"}}, {"dns_cf", map[string]string{"CF_Token": "fixture", "CF_Key": "key", "CF_Email": "a@example.com"}}, {"dns_pdns", map[string]string{"PDNS_Url": "http://dns.example.com/api/v1", "PDNS_ServerId": "localhost", "PDNS_Token": "fixture"}}, {"dns_pdns", map[string]string{"PDNS_Url": "https://name:secret@dns.example.com/api/v1", "PDNS_ServerId": "localhost", "PDNS_Token": "fixture"}}, {"dns_ispconfig", map[string]string{"ISPC_User": "name", "ISPC_Password": "secret", "ISPC_Api": "https://dns.example.com", "ISPC_Api_Insecure": "true"}}, {"dns_gcloud", map[string]string{"CLOUDSDK_ACTIVE_CONFIG_NAME": "../../root/.config"}}, {"dns_huaweicloud", map[string]string{"HUAWEICLOUD_Username": "name", "HUAWEICLOUD_Password": "secret", "HUAWEICLOUD_DomainName": "domain"}}, {"dns_dp", map[string]string{"DP_Id": "invalid", "DP_Key": "key"}}} {
		if err := ValidateDNSCredentials(test.id, test.fields); err == nil {
			t.Fatalf("unsafe/incomplete credentials accepted for %s", test.id)
		}
	}
	for _, test := range []struct {
		id     string
		fields map[string]string
	}{{"dns_cf", map[string]string{"CF_Token": "fixture", "CF_Key": "", "CF_Email": ""}}, {"dns_dp", map[string]string{"DP_Id": "123", "DP_Key": "fixture"}}, {"dns_pdns", map[string]string{"PDNS_Url": "https://dns.example.com/api/v1", "PDNS_ServerId": "localhost", "PDNS_Token": "fixture", "PDNS_Ttl": "120"}}} {
		if err := ValidateDNSCredentials(test.id, test.fields); err != nil {
			t.Fatal(test.id, err)
		}
	}
}
func TestDNSCredentialsArePrivateFilesAndNeverProcessArgv(t *testing.T) {
	adapter, err := findDNSAdapter("dns_cf")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	secret := "must-not-be-in-process-argv-or-env"
	environment, err := prepareDNSEnvironment(adapter, map[string]string{"CF_Token": secret}, work)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, value := range environment {
		if strings.Contains(value, secret) {
			t.Fatal("DNS secret entered process environment")
		}
		if strings.HasPrefix(value, "CLOUDFLARE_DNS_API_TOKEN_FILE=") {
			found = true
			name := strings.TrimPrefix(value, "CLOUDFLARE_DNS_API_TOKEN_FILE=")
			info, err := os.Stat(name)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("credential file is not private", err)
			}
			body, err := os.ReadFile(name)
			if err != nil || string(body) != secret {
				t.Fatal("credential file content missing", err)
			}
		}
	}
	if !found {
		t.Fatal("native provider did not receive its private credential file")
	}
	args := legoArgs(ACMEInput{Domains: []string{"example.com", "*.example.com"}, Email: "admin@example.com", Server: productionACMEDirectory}, "cloudflare", work)
	for _, arg := range args {
		if strings.Contains(arg, secret) || arg == "/bin/sh" || arg == "-c" {
			t.Fatal("Lego argv contains credentials or shell entrypoint")
		}
	}
	adapter, _ = findDNSAdapter("dns_dp")
	work = t.TempDir()
	environment, err = prepareDNSEnvironment(adapter, map[string]string{"DP_Id": "42", "DP_Key": "key"}, work)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range environment {
		if strings.HasPrefix(value, "DNSPOD_API_KEY_FILE=") {
			body, _ := os.ReadFile(strings.TrimPrefix(value, "DNSPOD_API_KEY_FILE="))
			if string(body) != "42,key" {
				t.Fatal("DNSPod China token composition changed")
			}
		}
	}
}

func TestAWSAdapterUsesProtectedSharedCredentialFileInsteadOfUnsupportedFileAliases(t *testing.T) {
	adapter, err := findDNSAdapter("dns_aws")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	credentials := map[string]string{"AWS_ACCESS_KEY_ID": "EXPLICIT-ACCESS-KEY", "AWS_SECRET_ACCESS_KEY": "EXPLICIT-PRIVATE-SECRET", "AWS_SESSION_TOKEN": "EXPLICIT-SESSION-TOKEN"}
	if err = ValidateDNSCredentials(adapter.ID, credentials); err != nil {
		t.Fatal(err)
	}
	environment, err := prepareDNSEnvironment(adapter, credentials, work)
	if err != nil {
		t.Fatal(err)
	}
	path := ""
	for _, entry := range environment {
		for _, secret := range credentials {
			if strings.Contains(entry, secret) {
				t.Fatal("AWS secret entered process environment")
			}
		}
		if strings.HasPrefix(entry, "AWS_ACCESS_KEY_ID_FILE=") || strings.HasPrefix(entry, "AWS_SECRET_ACCESS_KEY_FILE=") {
			t.Fatal("AWS SDK received an unsupported FILE alias")
		}
		if strings.HasPrefix(entry, "AWS_SHARED_CREDENTIALS_FILE=") {
			path = strings.TrimPrefix(entry, "AWS_SHARED_CREDENTIALS_FILE=")
		}
	}
	if path == "" {
		t.Fatal("AWS explicit private credentials file missing")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("AWS credential file is not private", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range []string{"[default]", "aws_access_key_id=EXPLICIT-ACCESS-KEY", "aws_secret_access_key=EXPLICIT-PRIVATE-SECRET", "aws_session_token=EXPLICIT-SESSION-TOKEN"} {
		if !strings.Contains(string(body), setting) {
			t.Fatal("AWS SDK private file lost explicit credential", setting)
		}
	}
}
func TestDNSTrustedClientRejectsShellAndUnrelatedELF(t *testing.T) {
	for _, name := range []string{"/bin/sh", "/usr/bin/true"} {
		if _, err := os.Stat(name); err != nil {
			continue
		}
		if trustedLego(name) == nil {
			t.Fatalf("non-Lego entrypoint accepted: %s", name)
		}
	}
}
func makeDNSCertificate(t *testing.T, names []string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(123), Subject: pkix.Name{CommonName: "protocol fixture"}, DNSNames: names, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}
func TestDNSIssuanceRequiresMatchingPrivateKeyAndExactSubjectAlternativeNames(t *testing.T) {
	certificate, key := makeDNSCertificate(t, []string{"example.com", "*.example.com"})
	meta, err := validateIssuedDNSCertificate("fixture", []string{"example.com", "*.example.com"}, certificate, key)
	if err != nil || meta.Source != "acme" {
		t.Fatal("matching native certificate was rejected", err)
	}
	_, otherKey := makeDNSCertificate(t, []string{"example.com"})
	if _, err = validateIssuedDNSCertificate("fixture", []string{"example.com", "*.example.com"}, certificate, otherKey); err == nil {
		t.Fatal("certificate accepted another private key")
	}
	if _, err = validateIssuedDNSCertificate("fixture", []string{"other.example.com", "*.example.com"}, certificate, key); err == nil {
		t.Fatal("certificate accepted wrong SAN")
	}
	certificate, key = makeDNSCertificate(t, []string{"example.com", "example.com"})
	if _, err = validateIssuedDNSCertificate("fixture", []string{"example.com", "other.example.com"}, certificate, key); err == nil {
		t.Fatal("duplicate SAN concealed missing requested name")
	}
}
func TestZeroExitWithoutCertificateMaterialIsNeverIssued(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err = readDNSOutput(root, "certificates/example.com.crt", 64<<10); err == nil {
		t.Fatal("missing output could be marked issued")
	}
	outside := filepath.Join(t.TempDir(), "unowned-key")
	if err = os.WriteFile(outside, []byte("PRIVATE"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = root.Symlink(outside, "forged.key"); err != nil {
		t.Fatal(err)
	}
	if _, err = readDNSOutput(root, "forged.key", 16<<10); err == nil {
		t.Fatal("DNS output followed a private file symlink")
	}
}
func TestChallengeAliasRequiresRealCNAMEDelegation(t *testing.T) {
	input := ACMEInput{Domains: []string{"example.com", "*.example.com"}, DNSAlias: "delegated.example.net"}
	lookup := func(_ context.Context, name string) (string, error) {
		if name != "_acme-challenge.example.com" {
			t.Fatal("wrong original challenge name", name)
		}
		return "_acme-challenge.delegated.example.net.", nil
	}
	if err := verifyDNSDelegation(context.Background(), input, lookup); err != nil {
		t.Fatal(err)
	}
	if err := verifyDNSDelegation(context.Background(), input, func(context.Context, string) (string, error) { return "unrelated.example.net.", nil }); err == nil {
		t.Fatal("alias silently changed requested challenge ownership")
	}
}
func TestGoogleServiceAccountUsesExplicitPrivateJSONFile(t *testing.T) {
	_, key := makeDNSCertificate(t, []string{"example.com"})
	account, _ := json.Marshal(map[string]string{"type": "service_account", "project_id": "fixture-project", "client_email": "dns@fixture-project.iam.gserviceaccount.com", "private_key": key})
	credentials := map[string]string{"GCE_SERVICE_ACCOUNT_JSON": string(account)}
	if err := ValidateDNSCredentials("dns_gcloud", credentials); err != nil {
		t.Fatal(err)
	}
	adapter, _ := findDNSAdapter("dns_gcloud")
	environment, err := prepareDNSEnvironment(adapter, credentials, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	foundFile, foundProject := false, false
	for _, value := range environment {
		foundFile = foundFile || strings.HasPrefix(value, "GCE_SERVICE_ACCOUNT_FILE=")
		foundProject = foundProject || value == "GCE_PROJECT=fixture-project"
		if strings.Contains(value, "BEGIN PRIVATE KEY") {
			t.Fatal("Google key entered process environment")
		}
	}
	if !foundFile || !foundProject {
		t.Fatal("Google explicit credential adapter is incomplete")
	}
}
