package sites

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
)

type legacyKnotBackend struct {
	client            *legacyDNSClient
	server, key, algo string
	secret, zoneName  string
}

func init() {
	legacyDNSFactories["dns_knot"] = newLegacyKnotBackend
}

func newLegacyKnotBackend(client *legacyDNSClient) (legacyDNSBackend, error) {
	credentials := client.options.Credentials
	server, err := netip.ParseAddrPort(credentials["KNOT_SERVER"])
	if err != nil {
		address, parseErr := netip.ParseAddr(credentials["KNOT_SERVER"])
		if parseErr != nil {
			return nil, invalid("Knot server must be a literal IP address with an optional port")
		}
		server = netip.AddrPortFrom(address, 53)
	}
	if server.Addr().IsUnspecified() || server.Addr().IsMulticast() || server.Port() == 0 || !slices.Contains(client.options.AllowedDNSServers, server.String()) {
		return nil, failure("policy_denied", "Knot update server must match the root-configured literal endpoint", 403)
	}
	fields := strings.Fields(credentials["KNOT_KEY"])
	if len(fields) != 2 {
		return nil, invalid("Knot key requires inline algorithm:name and a base64 secret; files are not read")
	}
	algorithm, key, explicit := strings.Cut(fields[0], ":")
	if !explicit {
		key, algorithm = fields[0], "hmac-sha256"
	}
	algorithm = strings.ToLower(strings.TrimSuffix(algorithm, ".")) + "."
	if !slices.Contains([]string{dns.HmacSHA1, dns.HmacSHA256, dns.HmacSHA384, dns.HmacSHA512}, algorithm) {
		return nil, failure("dns_tsig_algorithm", "Knot TSIG requires an explicitly supported HMAC algorithm", 400)
	}
	key = strings.ToLower(strings.TrimSuffix(key, "."))
	if len(key) == 0 || len(key) > 253 {
		return nil, invalid("Invalid Knot TSIG key name")
	}
	for _, label := range strings.Split(key, ".") {
		if !legacyDNSLabel.MatchString(label) {
			return nil, invalid("Invalid Knot TSIG key label")
		}
	}
	secret, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(secret) < 16 || len(secret) > 512 {
		return nil, invalid("Knot TSIG secret must be bounded base64 key material")
	}
	zone := ""
	if credentials["KNOT_ZONE"] != "" {
		zone, err = canonicalLegacyDNSName(credentials["KNOT_ZONE"])
		if err != nil {
			return nil, err
		}
	}
	return &legacyKnotBackend{client: client, server: server.String(), key: key + ".", algo: algorithm, secret: fields[1], zoneName: zone}, nil
}

func (k *legacyKnotBackend) exchange(ctx context.Context, message *dns.Msg) (*dns.Msg, error) {
	message.SetTsig(k.key, k.algo, 300, k.client.options.Now().Unix())
	client := &dns.Client{Net: "tcp", Timeout: 10 * time.Second, TsigSecret: map[string]string{k.key: k.secret}}
	response, _, err := client.ExchangeContext(ctx, message, k.server)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failure("dns_tsig_failed", "Knot signed DNS exchange could not be verified", 403)
	}
	if response == nil || response.IsTsig() == nil {
		return nil, failure("dns_tsig_failed", "Knot response must carry a verified TSIG", 403)
	}
	return response, nil
}

func (k *legacyKnotBackend) zone(ctx context.Context, name string) (string, error) {
	zones := []string{}
	if k.zoneName != "" {
		if name != k.zoneName && !strings.HasSuffix(name, "."+k.zoneName) {
			return "", failure("dns_zone_mismatch", "DNS challenge lies outside the configured Knot zone", 403)
		}
		zones = []string{k.zoneName}
	} else {
		labels := strings.Split(strings.TrimSuffix(name, "."), ".")
		for i := 1; i < len(labels)-1 && len(zones) < 32; i++ {
			zones = append(zones, strings.Join(labels[i:], ".")+".")
		}
	}
	for _, zone := range zones {
		message := new(dns.Msg).SetQuestion(zone, dns.TypeSOA)
		response, err := k.exchange(ctx, message)
		if err != nil {
			return "", err
		}
		if response.Rcode != dns.RcodeSuccess && response.Rcode != dns.RcodeNameError {
			return "", failure("dns_zone_failed", "Knot zone discovery failed", 502)
		}
		for _, record := range response.Answer {
			if soa, ok := record.(*dns.SOA); ok && strings.EqualFold(soa.Hdr.Name, zone) && response.Authoritative {
				return zone, nil
			}
		}
	}
	return "", failure("dns_zone_missing", "No authoritative permitted Knot zone was found", 412)
}

func legacyDNSValueID(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func (k *legacyKnotBackend) list(ctx context.Context, name, zone string) ([]legacyDNSTXTRecord, error) {
	if name != zone && !strings.HasSuffix(name, "."+zone) {
		return nil, failure("dns_zone_mismatch", "Knot ownership receipt lies outside its zone", 403)
	}
	response, err := k.exchange(ctx, new(dns.Msg).SetQuestion(name, dns.TypeTXT))
	if err != nil {
		return nil, err
	}
	if !response.Authoritative || (response.Rcode != dns.RcodeSuccess && response.Rcode != dns.RcodeNameError) {
		return nil, failure("dns_query_unverified", "Knot TXT query was not authoritative", 502)
	}
	if len(response.Answer) > 2000 {
		return nil, failure("dns_record_limit", "Knot TXT reply exceeds its record budget", 413)
	}
	records := []legacyDNSTXTRecord{}
	for _, record := range response.Answer {
		if _, isAlias := record.(*dns.CNAME); isAlias {
			return nil, failure("dns_alias_required", "Register and verify a DNS challenge alias before updating this CNAME", 412)
		}
		if txt, ok := record.(*dns.TXT); ok && strings.EqualFold(txt.Hdr.Name, name) {
			value := strings.Join(txt.Txt, "")
			records = append(records, legacyDNSTXTRecord{ID: legacyDNSValueID(value), Name: name, Value: value})
		}
	}
	return records, nil
}

func legacyKnotTXT(name, value string, ttl uint32) *dns.TXT {
	return &dns.TXT{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: ttl}, Txt: []string{value}}
}

func (k *legacyKnotBackend) create(ctx context.Context, receipt *legacyDNSReceipt) error {
	for retry := 0; retry < 3; retry++ {
		current, err := k.list(ctx, receipt.Name, receipt.Zone)
		if err != nil {
			return err
		}
		if len(legacyDNSMatching(current, receipt.Name, receipt.Value)) > 0 {
			receipt.RecordID, receipt.State = legacyDNSValueID(receipt.Value), "borrowed"
			return nil
		}
		message := new(dns.Msg).SetUpdate(receipt.Zone)
		if len(current) == 0 {
			message.RRsetNotUsed([]dns.RR{legacyKnotTXT(receipt.Name, receipt.Value, 0)})
		} else {
			observed := make([]dns.RR, 0, len(current))
			for _, record := range current {
				observed = append(observed, legacyKnotTXT(receipt.Name, record.Value, 0))
			}
			message.Used(observed)
		}
		// Never remove an RRset: its unrelated TXT values must survive.
		message.Insert([]dns.RR{legacyKnotTXT(receipt.Name, receipt.Value, 60)})
		response, err := k.exchange(ctx, message)
		if err != nil {
			return err
		}
		if response.Rcode == dns.RcodeSuccess {
			receipt.RecordID = legacyDNSValueID(receipt.Value)
			return nil
		}
		if response.Rcode != dns.RcodeNXRrset && response.Rcode != dns.RcodeYXRrset {
			return failure("dns_update_failed", "Knot rejected the signed TXT update", 502)
		}
	}
	return failure("dns_record_conflict", "Knot TXT records changed during conditional creation", 409)
}

func (k *legacyKnotBackend) remove(ctx context.Context, receipt legacyDNSReceipt) error {
	message := new(dns.Msg).SetUpdate(receipt.Zone)
	message.Remove([]dns.RR{legacyKnotTXT(receipt.Name, receipt.Value, 0)})
	response, err := k.exchange(ctx, message)
	if err != nil {
		return err
	}
	if response.Rcode != dns.RcodeSuccess {
		return failure("dns_cleanup_failed", "Knot rejected the signed exact-value cleanup", 502)
	}
	return nil
}
