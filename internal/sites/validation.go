package sites

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"golang.org/x/net/idna"
)

var safeID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,95}$`)
var headerName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,79}$`)
var filename = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
var phpKey = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,95}$`)

func failure(code, message string, status int) error {
	return &core.Error{Code: code, Message: message, Status: status}
}
func invalid(message string) error { return failure("invalid_input", message, 400) }
func denied() error                { return failure("forbidden", "Website or certificate is not authorized", 403) }
func strict(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return invalid("Invalid website parameters")
	}
	return nil
}

func relative(value string) (string, error) {
	if value == "" {
		return ".", nil
	}
	if strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\x00\\\r\n") {
		return "", invalid("Only relative website directories are accepted")
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", invalid("Website directories cannot escape their root")
		}
	}
	return path.Clean(value), nil
}

// Domain preserves explicit HTTP ports while normalizing international names.
// Wildcards are legal server names but never contain Nginx syntax or variables.
func domain(value string) (string, string, int, error) {
	if value == "" || len(value) > 300 || strings.ContainsAny(value, "\x00\r\n\t /\\;{}$\"'") {
		return "", "", 0, invalid("Invalid website domain")
	}
	host := value
	port := 80
	if parsed, p, err := net.SplitHostPort(value); err == nil {
		host = parsed
		number, e := strconv.Atoi(p)
		if e != nil || number < 1 || number > 65535 {
			return "", "", 0, invalid("Invalid domain port")
		}
		port = number
	} else if strings.Count(value, ":") == 1 {
		parts := strings.Split(value, ":")
		host = parts[0]
		number, e := strconv.Atoi(parts[1])
		if e != nil || number < 1 || number > 65535 {
			return "", "", 0, invalid("Invalid domain port")
		}
		port = number
	} else if strings.Contains(value, ":") {
		return "", "", 0, invalid("IPv6 domain addresses must include brackets and a port")
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else {
		wildcard := strings.HasPrefix(host, "*.")
		if wildcard {
			host = strings.TrimPrefix(host, "*.")
		}
		ascii, err := idna.Lookup.ToASCII(strings.TrimSuffix(strings.ToLower(host), "."))
		if err != nil || len(ascii) > 253 || ascii == "" {
			return "", "", 0, invalid("Invalid international domain")
		}
		for _, label := range strings.Split(ascii, ".") {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", "", 0, invalid("Invalid domain label")
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return "", "", 0, invalid("Invalid domain character")
				}
			}
		}
		host = ascii
		if wildcard {
			host = "*." + host
		}
	}
	canonical := host
	if port != 80 || strings.Contains(host, ":") {
		canonical = net.JoinHostPort(host, strconv.Itoa(port))
	}
	return canonical, host, port, nil
}

func validateSite(site *Site) error {
	if !safeID.MatchString(site.ID) || len(site.ID) > 75 || strings.TrimSpace(site.Name) == "" || len(site.Name) > 160 || !safeID.MatchString(site.WebInstance) {
		return invalid("Website requires a valid ID, name and installed Web instance")
	}
	if site.PHPInstance != "" && !safeID.MatchString(site.PHPInstance) {
		return invalid("Invalid PHP instance")
	}
	if site.PHP.SlowlogEnabled {
		if site.PHPInstance == "" {
			return invalid("PHP slow logging requires an assigned managed PHP runtime")
		}
		if site.PHP.SlowlogTimeoutSeconds == 0 {
			site.PHP.SlowlogTimeoutSeconds = 5
		}
		if site.PHP.SlowlogTimeoutSeconds < 1 || site.PHP.SlowlogTimeoutSeconds > 300 {
			return invalid("PHP slowlog timeout must be between one and 300 seconds")
		}
	} else if site.PHP.SlowlogTimeoutSeconds != 0 {
		return invalid("PHP slowlog timeout requires slow logging to be enabled")
	}
	if len(site.Domains) < 1 || len(site.Domains) > 100 {
		return invalid("Website needs between one and 100 domains")
	}
	seen := map[string]bool{}
	for i, value := range site.Domains {
		canonical, _, _, err := domain(value)
		if err != nil {
			return err
		}
		if seen[canonical] {
			return invalid("Duplicate domain")
		}
		seen[canonical] = true
		site.Domains[i] = canonical
	}
	var err error
	if site.RunPath, err = relative(site.RunPath); err != nil {
		return err
	}
	if site.Status != "running" && site.Status != "stopped" {
		return invalid("Website status must be running or stopped")
	}
	if site.CategoryID != "" && !safeID.MatchString(site.CategoryID) {
		return invalid("Invalid website category")
	}
	if len(site.Description) > 4000 {
		return invalid("Website description exceeds budget")
	}
	if site.ExpiresAt != "" {
		if _, err = time.Parse(time.RFC3339, site.ExpiresAt); err != nil {
			return invalid("Expiration must be an RFC3339 timestamp")
		}
	}
	if len(site.Index) == 0 {
		site.Index = []string{"index.html", "index.htm", "index.php"}
	}
	if len(site.Index) > 16 {
		return invalid("Too many index files")
	}
	for _, name := range site.Index {
		if !filename.MatchString(name) || name == ".." || name == "." {
			return invalid("Index must contain safe file names")
		}
	}
	if err = validateRewrite(site.Rewrite); err != nil {
		return err
	}
	if site.RateLimit.Connections < 0 || site.RateLimit.Connections > 100000 || site.RateLimit.BytesPerSecond < 0 || site.RateLimit.BytesPerSecond > 1<<40 || site.RateLimit.RequestsPerSecond < 0 || site.RateLimit.RequestsPerSecond > 100000 {
		return invalid("Invalid website rate limits")
	}
	if site.Protection.Enabled && (site.Protection.Username == "" || len(site.Protection.Username) > 64 || strings.ContainsAny(site.Protection.Username, ":\r\n\x00")) {
		return invalid("Invalid HTTP authentication username")
	}
	if len(site.Protection.DenyPaths) > 64 || len(site.Protection.AllowedReferers) > 100 {
		return invalid("Website protection list exceeds budget")
	}
	for _, value := range site.Protection.DenyPaths {
		if !safeLocation(value) {
			return invalid("Invalid protected location")
		}
	}
	for _, value := range site.Protection.AllowedReferers {
		if _, _, _, err = domain(value); err != nil {
			return err
		}
	}
	if len(site.Proxies) > 64 || len(site.Redirects) > 64 || len(site.DirectoryBindings) > 64 {
		return invalid("Too many website rules")
	}
	ids := map[string]bool{}
	locations := map[string]bool{}
	for _, proxy := range site.Proxies {
		if !safeID.MatchString(proxy.ID) || ids[proxy.ID] || !safeLocation(proxy.Path) || proxy.CacheSeconds < 0 || proxy.CacheSeconds > 86400 {
			return invalid("Invalid or duplicate proxy rule")
		}
		ids[proxy.ID] = true
		if proxy.Enabled {
			if locations[proxy.Path] {
				return invalid("Enabled proxy and redirect locations must be distinct")
			}
			locations[proxy.Path] = true
		}
		u, e := url.Parse(proxy.Upstream)
		if e != nil || !(u.Scheme == "http" || u.Scheme == "https") || u.Host == "" || u.User != nil || u.Fragment != "" || strings.ContainsAny(proxy.Upstream, "\r\n;{}$\"'") {
			return invalid("Proxy upstream must be an HTTP(S) URL without credentials")
		}
		if len(proxy.Headers) > 32 {
			return invalid("Too many proxy headers")
		}
		for key, value := range proxy.Headers {
			if !headerName.MatchString(key) || len(value) > 1024 || strings.ContainsAny(value, "\r\n\x00;{}$\"'") {
				return invalid("Invalid proxy header")
			}
		}
	}
	ids = map[string]bool{}
	for _, redirect := range site.Redirects {
		if !safeID.MatchString(redirect.ID) || ids[redirect.ID] || !safeLocation(redirect.Path) || !slices.Contains([]int{301, 302, 307, 308}, redirect.Status) {
			return invalid("Invalid or duplicate redirect rule")
		}
		ids[redirect.ID] = true
		if redirect.Enabled {
			if locations[redirect.Path] {
				return invalid("Enabled proxy and redirect locations must be distinct")
			}
			locations[redirect.Path] = true
		}
		u, e := url.Parse(redirect.Destination)
		if e != nil || !(u.Scheme == "http" || u.Scheme == "https") || u.Host == "" || u.User != nil || strings.ContainsAny(redirect.Destination, "\r\n;{}$\"'") {
			return invalid("Redirect destination must be an HTTP(S) URL")
		}
	}
	ids = map[string]bool{}
	for index := range site.DirectoryBindings {
		binding := &site.DirectoryBindings[index]
		if !safeID.MatchString(binding.ID) || ids[binding.ID] || len(binding.Domains) == 0 || len(binding.Domains) > 100 {
			return invalid("Invalid directory binding")
		}
		ids[binding.ID] = true
		if binding.Path, err = relative(binding.Path); err != nil {
			return err
		}
		for index, value := range binding.Domains {
			if binding.Domains[index], _, _, err = domain(value); err != nil {
				return err
			}
			if seen[binding.Domains[index]] {
				return invalid("Domain is assigned to multiple website directories")
			}
			seen[binding.Domains[index]] = true
		}
		if err = validateRewrite(binding.Rewrite); err != nil {
			return err
		}
	}
	if len(site.PHP.Values) > 32 {
		return invalid("Too many PHP settings")
	}
	for key, value := range site.PHP.Values {
		if !phpKey.MatchString(key) || len(value) > 1024 || strings.ContainsAny(value, "\x00\r\n[]") {
			return invalid("Invalid PHP setting")
		}
	}
	return nil
}

func safeLocation(value string) bool {
	return value != "/.__powerlinux_health" && strings.HasPrefix(value, "/") && !strings.ContainsAny(value, "\x00\r\n\t ;{}$\"'\\") && !strings.Contains(value, "..") && len(value) <= 1024
}
func allSiteDomains(site Site) []string {
	domains := append([]string{}, site.Domains...)
	for _, binding := range site.DirectoryBindings {
		domains = append(domains, binding.Domains...)
	}
	return domains
}

// The ordinary rewrite action deliberately supports rewrite/try_files/return
// statements. Raw server configuration has a separate host administration
// scope and never passes through this parser.
func validateRewrite(value string) error {
	if len(value) > 16<<10 || strings.ContainsAny(value, "\x00{}\"'\\") {
		return invalid("Unsupported rewrite syntax; use a trusted template or the separately authorized configuration action")
	}
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasSuffix(line, ";") || strings.Count(line, ";") != 1 {
			return invalid("Each rewrite statement must end in one semicolon")
		}
		fields := strings.Fields(strings.TrimSuffix(line, ";"))
		if len(fields) < 2 {
			return invalid("Invalid rewrite statement")
		}
		switch fields[0] {
		case "rewrite":
			if len(fields) < 3 || len(fields) > 4 {
				return invalid("Invalid rewrite directive")
			}
			if len(fields) == 4 && !slices.Contains([]string{"last", "break", "redirect", "permanent"}, fields[3]) {
				return invalid("Invalid rewrite flag")
			}
		case "try_files":
			if len(fields) > 16 {
				return invalid("Too many try_files candidates")
			}
		case "return":
			code, e := strconv.Atoi(fields[1])
			if e != nil || code < 100 || code > 599 || len(fields) > 3 {
				return invalid("Invalid return directive")
			}
		default:
			return invalid("Rewrite directive is not supported by the ordinary website action")
		}
	}
	return nil
}

func inspectCertificate(id, name, certPEM, keyPEM string) (Certificate, error) {
	if !safeID.MatchString(id) || name == "" || len(name) > 160 || len(certPEM) > 64<<10 || len(keyPEM) > 16<<10 {
		return Certificate{}, invalid("Invalid certificate metadata or PEM size")
	}
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil || len(pair.Certificate) == 0 {
		return Certificate{}, invalid("Certificate and private key must be valid PEM and match")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return Certificate{}, invalid("Invalid X.509 certificate")
	}
	if time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		return Certificate{}, invalid("Certificate is not currently valid")
	}
	for remaining := []byte(certPEM); len(bytes.TrimSpace(remaining)) > 0; {
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" {
			return Certificate{}, invalid("Certificate PEM contains unsupported material")
		}
		if _, err = x509.ParseCertificate(block.Bytes); err != nil {
			return Certificate{}, invalid("Certificate chain is malformed")
		}
		remaining = rest
	}
	domains := append([]string{}, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		domains = append(domains, ip.String())
	}
	if len(domains) == 0 {
		return Certificate{}, invalid("Certificate needs subject alternative names")
	}
	digest := sha256.Sum256(leaf.Raw)
	return Certificate{ID: id, Name: name, Domains: domains, Issuer: leaf.Issuer.String(), Serial: leaf.SerialNumber.String(), SHA256: hex.EncodeToString(digest[:]), NotBefore: leaf.NotBefore, ExpiresAt: leaf.NotAfter, Source: "uploaded", UpdatedAt: time.Now().UTC()}, nil
}
