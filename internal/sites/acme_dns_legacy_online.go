package sites

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

const legacyOnlineAPI = "https://api.online.net/api/v1"

// The official console.online.net/en/api/swagger.json defines GET domain/zone
// and PATCH active ResourceRecordOperation ADD/DELETE with an exact data filter.
// Do not use its PUT/reset APIs or the old temporary empty-zone version switch.
type legacyOnlineBackend struct {
	client *legacyDNSClient
	base   *url.URL
}
type legacyOnlineDomain struct {
	ID   legacyDNSID `json:"id"`
	Name string      `json:"name"`
}
type legacyOnlineRecord struct {
	ID       legacyDNSID        `json:"id"`
	Name     string             `json:"name"`
	Type     string             `json:"type"`
	Data     string             `json:"data"`
	TTL      int                `json:"ttl"`
	Priority int                `json:"priority,omitempty"`
	Domain   legacyOnlineDomain `json:"domain"`
}
type legacyOnlineChange struct {
	Name    string               `json:"name"`
	Type    string               `json:"type"`
	Kind    string               `json:"changeType"`
	Data    string               `json:"data,omitempty"`
	Records []legacyOnlineInsert `json:"records,omitempty"`
}
type legacyOnlineInsert struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	TTL      int    `json:"ttl"`
	Priority int    `json:"priority"`
	Data     string `json:"data"`
}

func init() { legacyDNSFactories["dns_online"] = newLegacyOnlineBackend }
func newLegacyOnlineBackend(client *legacyDNSClient) (legacyDNSBackend, error) {
	base, err := legacyFixedDNSAPI(client, legacyOnlineAPI)
	if err != nil {
		return nil, err
	}
	return &legacyOnlineBackend{client: client, base: base}, nil
}
func (o *legacyOnlineBackend) call(ctx context.Context, method, path string, payload any) ([]byte, error) {
	endpoint := *o.base
	endpoint.Path += path
	headers := http.Header{"Authorization": {"Bearer " + o.client.options.Credentials["ONLINE_API_KEY"]}, "Accept": {"application/json"}}
	var body []byte
	if payload != nil {
		headers.Set("Content-Type", "application/json")
		body = encode(payload)
	}
	data, _, err := o.client.request(ctx, method, &endpoint, headers, body)
	return data, err
}
func (o *legacyOnlineBackend) domain(ctx context.Context, name string) (legacyOnlineDomain, error) {
	data, err := o.call(ctx, http.MethodGet, "/domain/"+name, nil)
	if err != nil {
		return legacyOnlineDomain{}, err
	}
	var domain legacyOnlineDomain
	if err := legacyDNSDecode(data, &domain); err != nil {
		return domain, err
	}
	canonical, err := legacyDNSResponseName(domain.Name)
	if err != nil || strings.TrimSuffix(canonical, ".") != name || domain.ID == "" {
		return domain, failure("dns_response_invalid", "Online returned an inconsistent domain identity", 502)
	}
	domain.Name = name
	return domain, nil
}
func (o *legacyOnlineBackend) zone(ctx context.Context, name string) (string, error) {
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for offset := 1; offset < len(labels)-1 && offset <= 32; offset++ {
		candidate := strings.Join(labels[offset:], ".")
		domain, err := o.domain(ctx, candidate)
		if err == nil {
			return legacyDNSZoneKey(string(domain.ID), candidate), nil
		}
		if !isNotFound(err) {
			return "", err
		}
	}
	return "", failure("dns_zone_missing", "No permitted Online zone matched the challenge", 412)
}

func legacyOnlineRecordName(relative, domain string) (string, error) {
	if relative == "@" {
		return domain + ".", nil
	}
	absolute := strings.TrimSuffix(relative, ".")
	if strings.HasSuffix(relative, ".") || strings.EqualFold(absolute, domain) || strings.HasSuffix(strings.ToLower(absolute), "."+domain) {
		return legacyDNSResponseName(relative)
	}
	return legacyDNSResponseName(relative + "." + domain)
}

func (o *legacyOnlineBackend) records(ctx context.Context, name, zone string) ([]legacyOnlineRecord, error) {
	id, domain, err := legacyDNSZoneParts(zone)
	if err != nil || !legacyDNSWithinZone(name, domain) {
		return nil, failure("dns_receipt_conflict", "Online receipt does not cover this challenge", 409)
	}
	current, err := o.domain(ctx, domain)
	if err != nil {
		return nil, err
	}
	if string(current.ID) != id {
		return nil, failure("dns_zone_conflict", "Online domain identity changed; no record was modified", 409)
	}
	data, err := o.call(ctx, http.MethodGet, "/domain/"+domain+"/zone", nil)
	if err != nil {
		return nil, err
	}
	all, err := legacyDNSObjects[legacyOnlineRecord](data)
	if err != nil {
		return nil, err
	}
	records := []legacyOnlineRecord{}
	seen := map[string]bool{}
	for _, record := range all {
		if record.ID == "" || seen[string(record.ID)] {
			return nil, failure("dns_response_invalid", "Online returned a missing or duplicate record identity", 502)
		}
		seen[string(record.ID)] = true
		recordDomain, err := legacyDNSResponseName(record.Domain.Name)
		if err != nil || strings.TrimSuffix(recordDomain, ".") != domain || string(record.Domain.ID) != id {
			return nil, failure("dns_response_invalid", "Online returned a record outside the observed domain identity", 502)
		}
		if record.Type != "TXT" {
			continue
		}
		canonical, err := legacyOnlineRecordName(record.Name, domain)
		if err != nil {
			// Legal wildcard TXT records elsewhere in the zone must survive.
			if strings.Contains(record.Name, "*") {
				continue
			}
			return nil, err
		}
		if canonical == name {
			records = append(records, record)
		}
	}
	return records, nil
}
func (o *legacyOnlineBackend) list(ctx context.Context, name, zone string) ([]legacyDNSTXTRecord, error) {
	all, err := o.records(ctx, name, zone)
	if err != nil {
		return nil, err
	}
	records := []legacyDNSTXTRecord{}
	for _, item := range all {
		value, err := legacyDNSTXTData(item.Data)
		if err != nil {
			return nil, err
		}
		records = append(records, legacyDNSTXTRecord{ID: string(item.ID), Name: name, Value: value})
	}
	return records, nil
}
func (o *legacyOnlineBackend) create(ctx context.Context, receipt *legacyDNSReceipt) error {
	_, domain, err := legacyDNSZoneParts(receipt.Zone)
	if err != nil || !legacyDNSWithinZone(receipt.Name, domain) {
		return failure("dns_receipt_conflict", "Online creation requires its observed challenge zone", 409)
	}
	relative := strings.TrimSuffix(receipt.Name, "."+domain+".")
	record := legacyOnlineInsert{Name: relative, Type: "TXT", TTL: 60, Data: "\"" + receipt.Value + "\""}
	_, err = o.call(ctx, http.MethodPatch, "/domain/"+domain+"/version/active", []legacyOnlineChange{{Name: relative, Type: "TXT", Kind: "ADD", Records: []legacyOnlineInsert{record}}})
	// The documented response is 201 without an ID. Common recovery queries
	// the exact value and requires one new numeric ID outside the baseline.
	return err
}
func (o *legacyOnlineBackend) remove(ctx context.Context, receipt legacyDNSReceipt) error {
	_, domain, err := legacyDNSZoneParts(receipt.Zone)
	if err != nil || !legacyDNSNumericID.MatchString(receipt.RecordID) {
		return failure("dns_receipt_conflict", "Online cleanup requires its owned numeric record ID", 409)
	}
	all, err := o.records(ctx, receipt.Name, receipt.Zone)
	if err != nil {
		return err
	}
	matches := []legacyOnlineRecord{}
	for _, record := range all {
		value, err := legacyDNSTXTData(record.Data)
		if err != nil {
			return err
		}
		if value == receipt.Value {
			matches = append(matches, record)
		}
	}
	if len(matches) != 1 || string(matches[0].ID) != receipt.RecordID {
		return failure("dns_ownership_ambiguous", "Online exact-value deletion requires one matching owned identity; nothing was modified", 409)
	}
	relative := strings.TrimSuffix(receipt.Name, "."+domain+".")
	_, err = o.call(ctx, http.MethodPatch, "/domain/"+domain+"/version/active", []legacyOnlineChange{{Name: relative, Type: "TXT", Kind: "DELETE", Data: matches[0].Data}})
	return err
}
