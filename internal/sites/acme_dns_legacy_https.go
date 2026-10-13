package sites

import (
	"net/url"
	"slices"
	"strings"

	"github.com/miekg/dns"
	"golang.org/x/net/idna"
)

func legacyFixedDNSAPI(client *legacyDNSClient, official string) (*url.URL, error) {
	if !slices.Contains(client.options.AllowedAPIURLs, official) {
		return nil, failure("policy_denied", "DNS API requires its exact reviewed HTTPS endpoint in the trusted policy", 403)
	}
	return legacyDNSAPIURL(official)
}

// Providers may return Unicode names despite requiring Punycode on requests.
// Map each international label, retaining DNS's permitted underscore labels.
func legacyDNSResponseName(value string) (string, error) {
	labels := strings.Split(strings.TrimSuffix(strings.ToLower(value), "."), ".")
	if len(value) > 1024 {
		return "", failure("dns_response_invalid", "DNS provider returned an oversized name", 502)
	}
	for index, label := range labels {
		if legacyDNSLabel.MatchString(label) {
			continue
		}
		ascii, err := idna.Lookup.ToASCII(label)
		if err != nil {
			return "", failure("dns_response_invalid", "DNS provider returned an invalid name", 502)
		}
		labels[index] = ascii
	}
	name, err := canonicalLegacyDNSName(strings.Join(labels, "."))
	if err != nil {
		return "", failure("dns_response_invalid", "DNS provider returned an invalid name", 502)
	}
	return name, nil
}

func legacyDNSZoneKey(id, domain string) string { return id + ":" + domain }
func legacyDNSZoneParts(key string) (string, string, error) {
	id, domain, ok := strings.Cut(key, ":")
	if !ok || !legacyDNSNumericID.MatchString(id) {
		return "", "", failure("dns_receipt_conflict", "DNS receipt has an invalid domain identity", 409)
	}
	canonical, err := canonicalLegacyDNSName(domain)
	if err != nil || strings.TrimSuffix(canonical, ".") != domain {
		return "", "", failure("dns_receipt_conflict", "DNS receipt has an invalid canonical zone", 409)
	}
	return id, domain, nil
}

func legacyDNSWithinZone(name, zone string) bool {
	return strings.HasSuffix(strings.TrimSuffix(name, "."), "."+zone)
}

// Parse reviewed BIND-format TXT data, including multiple quoted segments and
// DNS decimal escapes. Go string unquoting has different escape semantics.
func legacyDNSTXTData(value string) (string, error) {
	if len(value) > 64<<10 || strings.ContainsAny(value, "\x00\r\n") {
		return "", failure("dns_response_invalid", "DNS provider returned oversized or invalid TXT data", 502)
	}
	record, err := dns.NewRR("receipt.invalid. 60 IN TXT " + value)
	if err != nil {
		return "", failure("dns_response_invalid", "DNS provider returned invalid TXT syntax", 502)
	}
	txt, ok := record.(*dns.TXT)
	if !ok {
		return "", failure("dns_response_invalid", "DNS provider returned the wrong record type", 502)
	}
	var decoded strings.Builder
	for _, segment := range txt.Txt {
		for index := 0; index < len(segment); index++ {
			value := segment[index]
			if value != '\\' {
				decoded.WriteByte(value)
				continue
			}
			index++
			if index >= len(segment) {
				return "", failure("dns_response_invalid", "DNS provider returned an incomplete TXT escape", 502)
			}
			if index+2 < len(segment) && segment[index] >= '0' && segment[index] <= '9' && segment[index+1] >= '0' && segment[index+1] <= '9' && segment[index+2] >= '0' && segment[index+2] <= '9' {
				value := int(segment[index]-'0')*100 + int(segment[index+1]-'0')*10 + int(segment[index+2]-'0')
				if value > 255 {
					return "", failure("dns_response_invalid", "DNS provider returned an invalid TXT octet escape", 502)
				}
				decoded.WriteByte(byte(value))
				index += 2
			} else {
				decoded.WriteByte(segment[index])
			}
		}
	}
	return decoded.String(), nil
}
