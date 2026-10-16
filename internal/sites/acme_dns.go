package sites

import (
	"context"
	"debug/buildinfo"
	"debug/elf"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

// Available denotes an implemented adapter, never successful external DNS
// verification. Unsupported legacy authentication formats remain explicit.
type DNSProviderSpec struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	ClientProvider string   `json:"client_provider,omitempty"`
	Driver         string   `json:"driver,omitempty"`
	Fields         []string `json:"fields"`
	Available      bool     `json:"available"`
	Reason         string   `json:"reason"`
}
type dnsAdapter struct {
	DNSProviderSpec
	environment map[string]string
	required    [][]string
}

func dnsAdapters() []dnsAdapter {
	const pending = "Lego credential adapter implemented; actual provider account and DNS issuance are not externally verified. The trusted client's provider/env support is checked before credentials are injected."
	row := func(id, name, client string, mapping map[string]string, required ...[]string) dnsAdapter {
		fields := []string{}
		for field := range mapping {
			fields = append(fields, field)
		}
		slices.Sort(fields)
		return dnsAdapter{DNSProviderSpec: DNSProviderSpec{ID: id, Name: name, ClientProvider: client, Driver: "lego", Fields: fields, Available: true, Reason: pending}, environment: mapping, required: required}
	}
	native := func(id, name string) dnsAdapter {
		return dnsAdapter{DNSProviderSpec: DNSProviderSpec{ID: id, Name: name, Driver: "native", Fields: append([]string(nil), legacyDNSCredentialFields[id]...), Available: true, Reason: "Native exact-TXT adapter and persistent ownership receipts are implemented; actual external provider accounts and issuance remain unverified."}, required: [][]string{legacyDNSRequiredFields[id]}}
	}
	unavailable := func(id, name, reason string, fields ...string) dnsAdapter {
		return dnsAdapter{DNSProviderSpec: DNSProviderSpec{ID: id, Name: name, Fields: fields, Available: false, Reason: reason}}
	}
	return []dnsAdapter{
		row("dns_cf", "Cloudflare", "cloudflare", map[string]string{"CF_Key": "CLOUDFLARE_API_KEY", "CF_Email": "CLOUDFLARE_EMAIL", "CF_Token": "CLOUDFLARE_DNS_API_TOKEN", "CF_Zone_API_Token": "CLOUDFLARE_ZONE_API_TOKEN"}, []string{"CF_Key", "CF_Email"}, []string{"CF_Token"}),
		row("dns_dp", "DNSPod China", "dnspod", map[string]string{"DP_Id": "", "DP_Key": "DNSPOD_API_KEY"}, []string{"DP_Id", "DP_Key"}),
		native("dns_dpi", "DNSPod International"),
		row("dns_gd", "GoDaddy", "godaddy", map[string]string{"GD_Key": "GODADDY_API_KEY", "GD_Secret": "GODADDY_API_SECRET"}, []string{"GD_Key", "GD_Secret"}),
		row("dns_pdns", "PowerDNS", "pdns", map[string]string{"PDNS_Url": "PDNS_API_URL", "PDNS_ServerId": "PDNS_SERVER_NAME", "PDNS_Token": "PDNS_API_KEY", "PDNS_Ttl": "PDNS_TTL"}, []string{"PDNS_Url", "PDNS_ServerId", "PDNS_Token"}),
		row("dns_lua", "LuaDNS", "luadns", map[string]string{"LUA_Key": "LUADNS_API_TOKEN", "LUA_Email": "LUADNS_API_USERNAME"}, []string{"LUA_Key", "LUA_Email"}),
		row("dns_me", "DNS Made Easy", "dnsmadeeasy", map[string]string{"ME_Key": "DNSMADEEASY_API_KEY", "ME_Secret": "DNSMADEEASY_API_SECRET"}, []string{"ME_Key", "ME_Secret"}),
		row("dns_aws", "Route 53", "route53", map[string]string{"AWS_ACCESS_KEY_ID": "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY": "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN": "", "AWS_REGION": "AWS_REGION", "AWS_HOSTED_ZONE_ID": "AWS_HOSTED_ZONE_ID"}, []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}),
		row("dns_ali", "Aliyun", "alidns", map[string]string{"Ali_Key": "ALICLOUD_ACCESS_KEY", "Ali_Secret": "ALICLOUD_SECRET_KEY"}, []string{"Ali_Key", "Ali_Secret"}),
		native("dns_ispconfig", "ISPConfig"),
		row("dns_ad", "Alwaysdata", "alwaysdata", map[string]string{"AD_API_KEY": "ALWAYSDATA_API_KEY"}, []string{"AD_API_KEY"}),
		row("dns_linode_v4", "Linode", "linode", map[string]string{"LINODE_V4_API_KEY": "LINODE_TOKEN"}, []string{"LINODE_V4_API_KEY"}),
		unavailable("dns_freedns", "FreeDNS", "The real reviewed Lego v4.35.2 client has no FreeDNS provider; a dedicated HTTPS adapter is required.", "FREEDNS_User", "FREEDNS_Password"),
		unavailable("dns_cyon", "Cyon", "The legacy OTP/login workflow has not been verified against a native client. Credentials are retained in the migration schema without claiming issuance support.", "CY_Username", "CY_Password", "CY_OTP_Secret"),
		row("dns_gandi_livedns", "Gandi LiveDNS", "gandiv5", map[string]string{"GANDI_LIVEDNS_TOKEN": "GANDIV5_API_KEY", "GANDI_PERSONAL_ACCESS_TOKEN": "GANDIV5_PERSONAL_ACCESS_TOKEN"}, []string{"GANDI_LIVEDNS_TOKEN"}, []string{"GANDI_PERSONAL_ACCESS_TOKEN"}),
		native("dns_knot", "Knot"),
		row("dns_dgon", "DigitalOcean", "digitalocean", map[string]string{"DO_API_KEY": "DO_AUTH_TOKEN"}, []string{"DO_API_KEY"}),
		row("dns_cloudns", "ClouDNS", "cloudns", map[string]string{"CLOUDNS_SUB_AUTH_ID": "CLOUDNS_SUB_AUTH_ID", "CLOUDNS_AUTH_ID": "CLOUDNS_AUTH_ID", "CLOUDNS_AUTH_PASSWORD": "CLOUDNS_AUTH_PASSWORD"}, []string{"CLOUDNS_SUB_AUTH_ID", "CLOUDNS_AUTH_PASSWORD"}, []string{"CLOUDNS_AUTH_ID", "CLOUDNS_AUTH_PASSWORD"}),
		row("dns_namesilo", "NameSilo", "namesilo", map[string]string{"Namesilo_Key": "NAMESILO_API_KEY"}, []string{"Namesilo_Key"}),
		row("dns_azure", "Azure DNS", "azuredns", map[string]string{"AZUREDNS_SUBSCRIPTIONID": "AZURE_SUBSCRIPTION_ID", "AZUREDNS_TENANTID": "AZURE_TENANT_ID", "AZUREDNS_APPID": "AZURE_CLIENT_ID", "AZUREDNS_CLIENTSECRET": "AZURE_CLIENT_SECRET", "AZUREDNS_RESOURCEGROUP": "AZURE_RESOURCE_GROUP"}, []string{"AZUREDNS_SUBSCRIPTIONID", "AZUREDNS_TENANTID", "AZUREDNS_APPID", "AZUREDNS_CLIENTSECRET"}),
		row("dns_selectel", "Selectel", "selectel", map[string]string{"SL_Key": "SELECTEL_API_TOKEN"}, []string{"SL_Key"}),
		row("dns_zonomi", "Zonomi", "zonomi", map[string]string{"ZM_Key": "ZONOMI_API_KEY"}, []string{"ZM_Key"}),
		unavailable("dns_kinghost", "KingHost", "Legacy username/password API authentication does not yet have a verified native adapter.", "KINGHOST_Username", "KINGHOST_Password"),
		native("dns_zilore", "Zilore"),
		row("dns_gcloud", "Google Cloud DNS", "gcloud", map[string]string{"GCE_SERVICE_ACCOUNT_JSON": "GCE_SERVICE_ACCOUNT_FILE", "GCE_PROJECT": "GCE_PROJECT", "CLOUDSDK_ACTIVE_CONFIG_NAME": ""}, []string{"GCE_SERVICE_ACCOUNT_JSON"}),
		row("dns_mydnsjp", "MyDNS.JP", "mydnsjp", map[string]string{"MYDNSJP_MasterID": "MYDNSJP_MASTER_ID", "MYDNSJP_Password": "MYDNSJP_PASSWORD"}, []string{"MYDNSJP_MasterID", "MYDNSJP_Password"}),
		row("dns_doapi", "do.de", "dode", map[string]string{"DO_LETOKEN": "DODE_TOKEN"}, []string{"DO_LETOKEN"}),
		native("dns_online", "Online"),
		row("dns_cn", "Core Networks", "corenetworks", map[string]string{"CN_User": "CORENETWORKS_LOGIN", "CN_Password": "CORENETWORKS_PASSWORD"}, []string{"CN_User", "CN_Password"}),
		row("dns_ultra", "UltraDNS", "ultradns", map[string]string{"ULTRA_USR": "ULTRADNS_USERNAME", "ULTRA_PWD": "ULTRADNS_PASSWORD"}, []string{"ULTRA_USR", "ULTRA_PWD"}),
		row("dns_hetzner", "Hetzner DNS", "hetzner", map[string]string{"HETZNER_Token": "HETZNER_API_TOKEN"}, []string{"HETZNER_Token"}),
		unavailable("dns_ddnss", "DDNSS.de", "The legacy dynamic-DNS TXT API requires a verified native adapter; an unverified provider name is not advertised as available.", "DDNSS_Token"),
		row("dns_huaweicloud", "Huawei Cloud", "huaweicloud", map[string]string{"Huawei_AK": "HUAWEICLOUD_ACCESS_KEY_ID", "Huawei_SK": "HUAWEICLOUD_SECRET_ACCESS_KEY", "Huawei_Region": "HUAWEICLOUD_REGION", "HUAWEICLOUD_Username": "", "HUAWEICLOUD_Password": "", "HUAWEICLOUD_DomainName": ""}, []string{"Huawei_AK", "Huawei_SK", "Huawei_Region"}),
		row("dns_tencent", "Tencent Cloud", "tencentcloud", map[string]string{"Tencent_SecretId": "TENCENTCLOUD_SECRET_ID", "Tencent_SecretKey": "TENCENTCLOUD_SECRET_KEY"}, []string{"Tencent_SecretId", "Tencent_SecretKey"}),
	}
}
func DNSProviders() []DNSProviderSpec {
	result := []DNSProviderSpec{}
	for _, adapter := range dnsAdapters() {
		spec := adapter.DNSProviderSpec
		spec.Fields = append([]string(nil), spec.Fields...)
		result = append(result, spec)
	}
	return result
}
func findDNSAdapter(id string) (dnsAdapter, error) {
	for _, adapter := range dnsAdapters() {
		if adapter.ID == id {
			return adapter, nil
		}
	}
	return dnsAdapter{}, invalid("Unknown DNS provider")
}

var credentialField = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

func ValidateDNSCredentials(provider string, credentials map[string]string) error {
	adapter, err := findDNSAdapter(provider)
	if err != nil {
		return err
	}
	if len(credentials) == 0 || len(credentials) > 16 {
		return invalid("DNS credentials require a bounded field object")
	}
	for field, value := range credentials {
		if !credentialField.MatchString(field) || !slices.Contains(adapter.Fields, field) || len(value) > 64<<10 || strings.ContainsRune(value, 0) {
			return invalid("Unknown, empty or oversized DNS credential field")
		}
		if value == "" {
			continue
		}
		if strings.Contains(strings.ToLower(field), "insecure") && value != "0" && value != "false" {
			return failure("tls_required", "DNS API certificate verification cannot be disabled", 400)
		}
		if field != "GCE_SERVICE_ACCOUNT_JSON" && strings.ContainsAny(value, "\r\n") {
			return invalid("DNS credential values must not contain line breaks")
		}
	}
	if !adapter.Available {
		return failure("dns_provider_unavailable", adapter.Reason, 412)
	}
	if provider == "dns_gcloud" && credentials["CLOUDSDK_ACTIVE_CONFIG_NAME"] != "" {
		return failure("legacy_auth_unavailable", "Host Cloud SDK configuration names are not read; register explicit service-account JSON", 412)
	}
	if provider == "dns_huaweicloud" && (credentials["HUAWEICLOUD_Username"] != "" || credentials["HUAWEICLOUD_Password"] != "" || credentials["HUAWEICLOUD_DomainName"] != "") {
		return failure("legacy_auth_unavailable", "Legacy Huawei IAM user/password authentication requires a dedicated adapter; the current client requires explicit AK/SK/region credentials", 412)
	}
	valid := false
	for _, group := range adapter.required {
		complete := true
		for _, field := range group {
			if credentials[field] == "" {
				complete = false
			}
		}
		valid = valid || complete
	}
	if !valid {
		return invalid("Required DNS provider credential fields are missing")
	}
	if provider == "dns_dpi" {
		if !legacyDNSNumericID.MatchString(credentials["DPI_Id"]) || strings.Contains(credentials["DPI_Key"], ",") {
			return invalid("DNSPod International credential ID/token format is invalid")
		}
		field := credentials["DPI_AuthField"]
		if field != "" && field != "login_token" && field != "user_token" {
			return invalid("DNSPod International authentication field must be login_token or user_token")
		}
	}
	if provider == "dns_ispconfig" {
		u, err := url.Parse(credentials["ISPC_Api"])
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
			return failure("tls_required", "ISPConfig requires a verified HTTPS API endpoint", 400)
		}
	}
	if provider == "dns_knot" {
		host, port, err := net.SplitHostPort(credentials["KNOT_SERVER"])
		number, portErr := strconv.Atoi(port)
		if err != nil || net.ParseIP(host) == nil || portErr != nil || number < 1 || number > 65535 {
			return invalid("Knot requires a literal IP address and port selected by the root update-server policy")
		}
		if len(strings.Fields(credentials["KNOT_KEY"])) != 2 {
			return invalid("Knot requires an inline TSIG name/algorithm and base64 key, not a key-file path")
		}
	}
	if provider == "dns_cf" && credentials["CF_Token"] != "" && (credentials["CF_Key"] != "" || credentials["CF_Email"] != "") {
		return invalid("Choose either Cloudflare token or global key/email credentials")
	}
	if provider == "dns_dp" && (!regexp.MustCompile(`^[0-9]+$`).MatchString(credentials["DP_Id"]) || strings.Contains(credentials["DP_Key"], ",")) {
		return invalid("DNSPod credential ID/token format is invalid")
	}
	if provider == "dns_pdns" {
		u, err := url.Parse(credentials["PDNS_Url"])
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
			return failure("tls_required", "PowerDNS API must use a verified HTTPS endpoint", 400)
		}
		if value := credentials["PDNS_Ttl"]; value != "" {
			ttl, err := strconv.Atoi(value)
			if err != nil || ttl < 30 || ttl > 86400 {
				return invalid("PowerDNS TTL must be 30–86400 seconds")
			}
		}
		if !regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`).MatchString(credentials["PDNS_ServerId"]) {
			return invalid("PowerDNS server ID must be a simple bounded identifier")
		}
	}
	if provider == "dns_gcloud" {
		if credentials["CLOUDSDK_ACTIVE_CONFIG_NAME"] != "" {
			return failure("legacy_auth_unavailable", "Host Cloud SDK configuration names are not read; register explicit service-account JSON", 412)
		}
		var account struct {
			Type       string `json:"type"`
			Project    string `json:"project_id"`
			Email      string `json:"client_email"`
			PrivateKey string `json:"private_key"`
		}
		if json.Unmarshal([]byte(credentials["GCE_SERVICE_ACCOUNT_JSON"]), &account) != nil || account.Type != "service_account" || account.Project == "" || account.Email == "" || !strings.Contains(account.PrivateKey, "BEGIN PRIVATE KEY") {
			return invalid("Google DNS requires an explicit service-account JSON credential")
		}
		if _, err := parsePrivateKey([]byte(account.PrivateKey)); err != nil {
			return invalid("Google service-account private key is malformed")
		}
	}
	return nil
}

func trustedLego(program string) error {
	if err := trustedProgram(program); err != nil {
		return failure("acme_program_untrusted", "ACME program must be a protected root-owned executable", 412)
	}
	binary, err := elf.Open(program)
	if err != nil {
		return failure("acme_program_untrusted", "DNS ACME accepts only a trusted ELF Lego binary; scripts are not executed", 412)
	}
	_ = binary.Close()
	info, err := buildinfo.ReadFile(program)
	if err != nil || info.Main.Path != "github.com/go-acme/lego/v4" || info.Path != "github.com/go-acme/lego/v4/cmd/lego" {
		return failure("acme_program_untrusted", "ACME executable is not the official Go Lego client", 412)
	}
	if info.Main.Version != "v4.35.2" {
		return failure("acme_client_version_unreviewed", "DNS credential-file behavior is reviewed for the exact pinned Lego v4.35.2 client; upgrade the trusted software recipe before using another version", 412)
	}
	return nil
}
func legoArgs(input ACMEInput, provider, work string) []string {
	args := []string{"--accept-tos", "--email", input.Email, "--server", input.Server, "--path", work, "--dns", provider, "--key-type", "ec256"}
	for _, name := range input.Domains {
		args = append(args, "--domains", name)
	}
	return append(args, "run")
}
func verifyDNSDelegation(ctx context.Context, input ACMEInput, lookup func(context.Context, string) (string, error)) error {
	alias := strings.ToLower(strings.TrimSuffix(input.DNSAlias, "."))
	if !strings.HasPrefix(alias, "_acme-challenge.") {
		alias = "_acme-challenge." + alias
	}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, domain := range input.Domains {
		source := "_acme-challenge." + strings.TrimPrefix(domain, "*.")
		actual, err := lookup(checkCtx, source)
		if err != nil || strings.ToLower(strings.TrimSuffix(actual, ".")) != alias {
			return failure("dns_alias_unverified", "DNS challenge alias must already be an actual CNAME delegation from every requested domain; no alternate domain was silently substituted", 412)
		}
	}
	return nil
}
func runDNSCommand(ctx context.Context, program string, args, environment []string, timeout time.Duration) (agent.Result, error) {
	operation := agent.Operation{Name: "site_dns_acme", Program: program, Timeout: timeout, Environment: environment, BuildArgs: func(json.RawMessage) ([]string, error) { return append([]string(nil), args...), nil }}
	runner, err := agent.NewRunner(agent.Options{Operations: []agent.Operation{operation}, MaxOutputBytes: 32 << 10, MaxConcurrent: 1})
	if err != nil {
		return agent.Result{}, err
	}
	return runner.Run(ctx, agent.Request{Operation: operation.Name, Input: json.RawMessage(`{}`)})
}
func prepareDNSEnvironment(adapter dnsAdapter, credentials map[string]string, work string) ([]string, error) {
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	env := []string{"HOME=" + work, "LEGO_DISABLE_CNAME_SUPPORT=false"}
	if adapter.ID == "dns_aws" {
		body := "[default]\naws_access_key_id=" + credentials["AWS_ACCESS_KEY_ID"] + "\naws_secret_access_key=" + credentials["AWS_SECRET_ACCESS_KEY"] + "\n"
		if value := credentials["AWS_SESSION_TOKEN"]; value != "" {
			body += "aws_session_token=" + value + "\n"
		}
		if err := atomicRoot(root, "aws.credentials", []byte(body), 0600); err != nil {
			return nil, err
		}
		env = append(env, "AWS_SHARED_CREDENTIALS_FILE="+filepath.Join(work, "aws.credentials"), "AWS_PROFILE=default", "AWS_EC2_METADATA_DISABLED=true")
	}
	fields := []string{}
	for field := range credentials {
		fields = append(fields, field)
	}
	slices.Sort(fields)
	for index, field := range fields {
		if adapter.ID == "dns_aws" && slices.Contains([]string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"}, field) {
			continue
		}
		variable := adapter.environment[field]
		if variable == "" {
			continue
		}
		value := credentials[field]
		if value == "" {
			continue
		}
		if adapter.ID == "dns_dp" && field == "DP_Key" {
			value = credentials["DP_Id"] + "," + value
		}
		if field == "PDNS_Url" || field == "PDNS_ServerId" || field == "PDNS_Ttl" || field == "AWS_REGION" || field == "AWS_HOSTED_ZONE_ID" || field == "Huawei_Region" || field == "GCE_PROJECT" {
			env = append(env, variable+"="+value)
			continue
		}
		fileName := "credential-" + strconv.Itoa(index)
		file, err := root.OpenFile(fileName, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return nil, err
		}
		_, err = file.WriteString(value)
		if err == nil {
			err = file.Sync()
		}
		err = errors.Join(err, file.Close())
		if err != nil {
			return nil, err
		}
		suffix := "_FILE"
		if field == "GCE_SERVICE_ACCOUNT_JSON" {
			suffix = ""
		}
		env = append(env, variable+suffix+"="+filepath.Join(work, fileName))
	}
	if adapter.ID == "dns_gcloud" && credentials["GCE_PROJECT"] == "" {
		var account struct {
			Project string `json:"project_id"`
		}
		_ = json.Unmarshal([]byte(credentials["GCE_SERVICE_ACCOUNT_JSON"]), &account)
		env = append(env, "GCE_PROJECT="+account.Project)
	}
	// DNS credentials are explicit resources. Host instance roles and ambient
	// cloud CLI profiles must not silently replace the registered credential.
	if adapter.ID == "dns_aws" {
		env = append(env, "AWS_EC2_METADATA_DISABLED=true")
	}
	if adapter.ID == "dns_gcloud" {
		env = append(env, "GCE_METADATA_HOST=127.0.0.1:9")
		for _, entry := range env {
			if strings.HasPrefix(entry, "GCE_SERVICE_ACCOUNT_FILE=") {
				env = append(env, "GOOGLE_APPLICATION_CREDENTIALS="+strings.TrimPrefix(entry, "GCE_SERVICE_ACCOUNT_FILE="))
				break
			}
		}
	}
	return env, nil
}
func validateIssuedDNSCertificate(id string, domains []string, certificate, key string) (Certificate, error) {
	meta, err := inspectCertificate(id, id, certificate, key)
	if err != nil {
		return meta, err
	}
	if len(meta.Domains) != len(domains) {
		return Certificate{}, failure("acme_certificate_mismatch", "Issued certificate has unexpected subject alternative names", 502)
	}
	wanted := map[string]bool{}
	for _, name := range domains {
		wanted[strings.ToLower(strings.TrimSuffix(name, "."))] = true
	}
	for _, name := range meta.Domains {
		canonical := strings.ToLower(strings.TrimSuffix(name, "."))
		if !wanted[canonical] {
			return Certificate{}, failure("acme_certificate_mismatch", "Issued certificate does not cover the exact requested DNS names", 502)
		}
		delete(wanted, canonical)
	}
	if len(wanted) != 0 {
		return Certificate{}, failure("acme_certificate_mismatch", "Issued certificate is missing requested names", 502)
	}
	meta.Source = "acme"
	return meta, nil
}
func readDNSOutput(root *os.Root, name string, limit int64) (string, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", failure("acme_issuance_incomplete", "ACME client exited without complete certificate material; no certificate was imported", 502)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return "", failure("acme_issuance_incomplete", "ACME client output is not a bounded regular file", 502)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return "", failure("acme_issuance_incomplete", "ACME material could not be read within its limit", 502)
	}
	return string(data), nil
}
func (e *AgentExecutor) issueDNSCertificate(ctx context.Context, in request, input ACMEInput) (any, error) {
	if input.Challenge != "dns" || in.ID != input.ID {
		return nil, invalid("Native DNS issuance requires a matching certificate identity")
	}
	if err := validateACME(&input, e.config); err != nil {
		return nil, err
	}
	if err := ValidateDNSCredentials(input.Provider, in.Credentials); err != nil {
		return nil, err
	}
	if input.DNSAlias != "" {
		if err := verifyDNSDelegation(ctx, input, net.DefaultResolver.LookupCNAME); err != nil {
			return nil, err
		}
	}
	previous, err := e.certificate(in.ID)
	if err != nil && !isNotFound(err) {
		return nil, err
	}
	if err == nil {
		if !input.Renew {
			return nil, failure("certificate_exists", "Renew explicitly or choose a new certificate ID", 409)
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
	if input.Provider == "dns_knot" || input.Provider == "dns_ispconfig" || input.Provider == "dns_zilore" || input.Provider == "dns_online" {
		return e.issueNativeDNSCertificate(ctx, in, input)
	}
	if e.config.ACMEProgram == "" {
		return nil, failure("acme_program_unavailable", "Install the verified Lego software integration and configure its root-owned executable first", 412)
	}
	if err = trustedLego(e.config.ACMEProgram); err != nil {
		return nil, err
	}
	adapter, _ := findDNSAdapter(input.Provider)
	if input.Provider == "dns_pdns" {
		var policy struct {
			URLs map[string][]string `json:"allowed_dns_api_urls"`
		}
		_ = json.Unmarshal(encode(e.config), &policy)
		if !slices.Contains(policy.URLs[input.Provider], in.Credentials["PDNS_Url"]) {
			return nil, failure("policy_denied", "PowerDNS endpoint must match a root-configured HTTPS API allowlist", 403)
		}
	}
	check, err := runDNSCommand(ctx, e.config.ACMEProgram, []string{"dnshelp", "--code", adapter.ClientProvider}, nil, 10*time.Second)
	if err != nil || check.ExitCode != 0 {
		return nil, failure("dns_client_unavailable", "Trusted Lego version does not support the selected DNS provider", 412)
	}
	help := check.Stdout + check.Stderr
	// v4.35.2's checksum-reviewed env.GetOrFile implementation reads *_FILE.
	// Its dnshelp intentionally lists base names only; help text is not a
	// credential-file capability marker. No plaintext-secret fallback is used.
	for field, variable := range adapter.environment {
		if in.Credentials[field] != "" && variable != "" && !strings.Contains(help, variable) {
			return nil, failure("dns_client_schema_mismatch", "Trusted Lego provider credentials differ from this reviewed adapter; no credentials were injected", 412)
		}
	}
	work, err := os.MkdirTemp(filepath.Join(e.config.StateDir, "acme"), ".dns-"+input.ID+"-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	environment, err := prepareDNSEnvironment(adapter, in.Credentials, work)
	if err != nil {
		return nil, failure("dns_credential_prepare_failed", "Protected DNS credential files could not be prepared", 500)
	}
	result, err := runDNSCommand(ctx, e.config.ACMEProgram, legoArgs(input, adapter.ClientProvider, work), environment, 20*time.Minute)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, failure("acme_dns_failed", "ACME DNS validation or issuance failed; client output is withheld because it may contain credentials", 502)
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	base := strings.ReplaceAll(input.Domains[0], "*", "_")
	certificate, err := readDNSOutput(root, filepath.Join("certificates", base+".crt"), 64<<10)
	if err != nil {
		return nil, err
	}
	key, err := readDNSOutput(root, filepath.Join("certificates", base+".key"), 16<<10)
	if err != nil {
		return nil, err
	}
	meta, err := validateIssuedDNSCertificate(input.ID, input.Domains, certificate, key)
	if err != nil {
		return nil, err
	}
	in.Certificate = &meta
	in.CertPEM = certificate
	in.KeyPEM = key
	created, err := e.importIssuedCertificate(ctx, in)
	if err != nil {
		return nil, err
	}
	return map[string]any{"certificate": created, "state": "issued"}, nil
}
