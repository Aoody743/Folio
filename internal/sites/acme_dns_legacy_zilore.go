package sites

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const legacyZiloreAPI = "https://api.zilore.com/dns/v1"

// Reviewed official schemas: zilore.com/en/help/api/records/{list_records,
// add_record,delete_record} and domains/list_domains (research snapshot
// 2026-10-13). Provider-controlled links are never followed.
type legacyZiloreBackend struct {
	client *legacyDNSClient
	base   *url.URL
}
type legacyZiloreDomain struct {
	ID   legacyDNSID `json:"domain_id"`
	Name string      `json:"domain_name"`
}
type legacyZiloreRecord struct {
	ID     legacyDNSID `json:"record_id"`
	Name   string      `json:"record_name"`
	Type   string      `json:"record_type"`
	Value  string      `json:"record_value"`
	Status string      `json:"record_status"`
}

func init() { legacyDNSFactories["dns_zilore"] = newLegacyZiloreBackend }
func newLegacyZiloreBackend(client *legacyDNSClient) (legacyDNSBackend, error) {
	base, err := legacyFixedDNSAPI(client, legacyZiloreAPI)
	if err != nil {
		return nil, err
	}
	return &legacyZiloreBackend{client: client, base: base}, nil
}

func (z *legacyZiloreBackend) call(ctx context.Context, method, path string, query, form url.Values) (json.RawMessage, error) {
	endpoint := *z.base
	endpoint.Path += path
	endpoint.RawQuery = query.Encode()
	headers := http.Header{"X-Auth-Key": {z.client.options.Credentials["Zilore_Key"]}, "Accept": {"application/json"}}
	var body []byte
	if form != nil {
		headers.Set("Content-Type", "application/x-www-form-urlencoded")
		body = []byte(form.Encode())
	}
	data, _, err := z.client.request(ctx, method, &endpoint, headers, body)
	if err != nil {
		return nil, err
	}
	var response struct {
		Status   string          `json:"status"`
		Response json.RawMessage `json:"response"`
	}
	if err := legacyDNSDecode(data, &response); err != nil {
		return nil, err
	}
	if response.Status != "ok" {
		return nil, failure("dns_request_failed", "Zilore rejected the typed DNS operation", 502)
	}
	return response.Response, nil
}

func (z *legacyZiloreBackend) domain(ctx context.Context, name string) (legacyZiloreDomain, error) {
	const pageSize = 100
	for offset := 0; offset < 2000; offset += pageSize {
		raw, err := z.call(ctx, http.MethodGet, "/domains", url.Values{
			"search_text": {name}, "offset": {strconv.Itoa(offset)}, "limit": {strconv.Itoa(pageSize)},
			"order_by": {"idn_domain_name"}, "order_param": {"asc"},
		}, nil)
		if err != nil {
			return legacyZiloreDomain{}, err
		}
		domains, err := legacyDNSObjects[legacyZiloreDomain](raw)
		if err != nil {
			return legacyZiloreDomain{}, err
		}
		if len(domains) > pageSize {
			return legacyZiloreDomain{}, failure("dns_record_limit", "Zilore ignored its bounded domain page size", 413)
		}
		for _, domain := range domains {
			canonical, err := legacyDNSResponseName(domain.Name)
			if err != nil || domain.ID == "" {
				return legacyZiloreDomain{}, failure("dns_response_invalid", "Zilore returned an invalid domain identity", 502)
			}
			if strings.TrimSuffix(canonical, ".") == name {
				domain.Name = name
				return domain, nil
			}
		}
		if len(domains) < pageSize {
			return legacyZiloreDomain{}, failure("dns_zone_missing", "No permitted Zilore zone matched the challenge", 412)
		}
	}
	return legacyZiloreDomain{}, failure("dns_record_limit", "Zilore domain discovery exceeds its pagination budget", 413)
}

func (z *legacyZiloreBackend) zone(ctx context.Context, name string) (string, error) {
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for offset := 1; offset < len(labels)-1 && offset <= 32; offset++ {
		candidate := strings.Join(labels[offset:], ".")
		domain, err := z.domain(ctx, candidate)
		if err == nil {
			return legacyDNSZoneKey(string(domain.ID), candidate), nil
		}
		var typed interface{ AgentError() (string, int) }
		if !errors.As(err, &typed) {
			return "", err
		}
		if code, _ := typed.AgentError(); code != "dns_zone_missing" {
			return "", err
		}
	}
	return "", failure("dns_zone_missing", "No permitted Zilore zone matched the challenge", 412)
}

func (z *legacyZiloreBackend) list(ctx context.Context, name, zone string) ([]legacyDNSTXTRecord, error) {
	id, domain, err := legacyDNSZoneParts(zone)
	if err != nil || !legacyDNSWithinZone(name, domain) {
		return nil, failure("dns_receipt_conflict", "Zilore receipt does not cover this challenge", 409)
	}
	current, err := z.domain(ctx, domain)
	if err != nil {
		return nil, err
	}
	if string(current.ID) != id {
		return nil, failure("dns_zone_conflict", "Zilore domain identity changed; no record was modified", 409)
	}
	const pageSize = 100
	records := []legacyDNSTXTRecord{}
	seen := map[string]bool{}
	for offset := 0; offset < 2000; offset += pageSize {
		raw, err := z.call(ctx, http.MethodGet, "/domains/"+domain+"/records", url.Values{
			"search_text": {strings.TrimSuffix(name, ".")}, "strict_search": {"yes"}, "search_record_type": {"TXT"},
			"offset": {strconv.Itoa(offset)}, "limit": {strconv.Itoa(pageSize)}, "order_by": {"record_id"}, "order_param": {"asc"},
		}, nil)
		if err != nil {
			return nil, err
		}
		page, err := legacyDNSObjects[legacyZiloreRecord](raw)
		if err != nil {
			return nil, err
		}
		if len(page) > pageSize {
			return nil, failure("dns_record_limit", "Zilore ignored its bounded record page size", 413)
		}
		for _, item := range page {
			if item.ID == "" || seen[string(item.ID)] {
				return nil, failure("dns_response_invalid", "Zilore pagination repeated or omitted a record identity", 502)
			}
			seen[string(item.ID)] = true
			if strings.HasPrefix(item.Name, "*.") {
				if _, err := legacyDNSResponseName(strings.TrimPrefix(item.Name, "*.")); err != nil {
					return nil, err
				}
				continue
			}
			canonical, err := legacyDNSResponseName(item.Name)
			if err != nil {
				return nil, err
			}
			if canonical != name || item.Type != "TXT" {
				continue
			}
			if item.Status != "enabled" && item.Status != "disabled" {
				return nil, failure("dns_response_invalid", "Zilore returned an invalid TXT publication state", 502)
			}
			value, err := legacyDNSTXTData(item.Value)
			if err != nil {
				return nil, err
			}
			records = append(records, legacyDNSTXTRecord{ID: string(item.ID), Name: canonical, Value: value, Disabled: item.Status == "disabled"})
		}
		if len(page) < pageSize {
			return records, nil
		}
	}
	return nil, failure("dns_record_limit", "Zilore TXT discovery exceeds its pagination budget", 413)
}

func (z *legacyZiloreBackend) create(ctx context.Context, receipt *legacyDNSReceipt) error {
	_, domain, err := legacyDNSZoneParts(receipt.Zone)
	if err != nil {
		return err
	}
	raw, err := z.call(ctx, http.MethodPost, "/domains/"+domain+"/records", nil, url.Values{
		"record_name": {strings.TrimSuffix(receipt.Name, ".")}, "record_type": {"TXT"},
		"record_value": {strconv.Quote(receipt.Value)}, "record_ttl": {"600"},
	})
	if err != nil {
		return err
	}
	var response struct {
		ID     legacyDNSID `json:"record_id"`
		Status string      `json:"record_status"`
	}
	if err := legacyDNSDecode(raw, &response); err != nil || response.ID == "" || response.Status != "added" {
		return failure("dns_response_invalid", "Zilore did not acknowledge an exact added record identity", 502)
	}
	receipt.RecordID = string(response.ID)
	return nil
}

func (z *legacyZiloreBackend) remove(ctx context.Context, receipt legacyDNSReceipt) error {
	_, domain, err := legacyDNSZoneParts(receipt.Zone)
	if err != nil || !legacyDNSNumericID.MatchString(receipt.RecordID) {
		return failure("dns_receipt_conflict", "Zilore cleanup requires its exact owned numeric ID", 409)
	}
	_, err = z.call(ctx, http.MethodDelete, "/domains/"+domain+"/records", url.Values{"record_id": {receipt.RecordID}}, nil)
	return err
}
