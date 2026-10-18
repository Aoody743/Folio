package sites

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const legacyDPIAPI = "https://api.dnspod.com"

// Protocol sources: DNSPod/dnspod-int-api-docs source/{domains,records}.rst
// and acmesh-official/acme.sh dnsapi/dns_dpi.sh. This is the international API.
type legacyDPIBackend struct {
	client    *legacyDNSClient
	base      *url.URL
	authField string
}
type legacyDPIDomain struct {
	ID   legacyDNSID `json:"id"`
	Name string      `json:"name"`
}
type legacyDPIRecord struct {
	ID      legacyDNSID `json:"id"`
	Name    string      `json:"name"`
	Type    string      `json:"type"`
	Value   string      `json:"value"`
	Enabled string      `json:"enabled"`
}
type legacyDPIResponse struct {
	Status struct {
		Code string `json:"code"`
	} `json:"status"`
	Domain legacyDPIDomain `json:"domain"`
	Info   struct {
		Total string `json:"record_total"`
	} `json:"info"`
	Records []legacyDPIRecord `json:"records"`
	Record  struct {
		ID     legacyDNSID `json:"id"`
		Name   string      `json:"name"`
		Status string      `json:"status"`
	} `json:"record"`
}

func init() { legacyDNSFactories["dns_dpi"] = newLegacyDPIBackend }
func newLegacyDPIBackend(client *legacyDNSClient) (legacyDNSBackend, error) {
	base, err := legacyFixedDNSAPI(client, legacyDPIAPI)
	if err != nil {
		return nil, err
	}
	if !legacyDNSNumericID.MatchString(client.options.Credentials["DPI_Id"]) || strings.Contains(client.options.Credentials["DPI_Key"], ",") {
		return nil, invalid("DNSPod International credential ID/token format is invalid")
	}
	field := client.options.Credentials["DPI_AuthField"]
	if field == "" {
		field = "login_token"
	}
	if field != "login_token" && field != "user_token" {
		return nil, invalid("DNSPod International authentication field must be login_token or user_token")
	}
	return &legacyDPIBackend{client: client, base: base, authField: field}, nil
}
func (d *legacyDPIBackend) call(ctx context.Context, action string, form url.Values) (legacyDPIResponse, error) {
	endpoint := *d.base
	endpoint.Path += "/" + action
	form.Set(d.authField, d.client.options.Credentials["DPI_Id"]+","+d.client.options.Credentials["DPI_Key"])
	form.Set("format", "json")
	form.Set("error_on_empty", "yes")
	data, _, err := d.client.request(ctx, http.MethodPost, &endpoint, http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Accept": {"application/json"}, "User-Agent": {"Folio/1.0 (https://github.com/Aoody743/Folio)"}}, []byte(form.Encode()))
	var response legacyDPIResponse
	if err != nil {
		return response, err
	}
	if err = legacyDNSDecode(data, &response); err != nil {
		return response, err
	}
	if response.Status.Code == "" {
		return response, failure("dns_response_invalid", "DNSPod International omitted its operation status", 502)
	}
	return response, nil
}
func legacyDPIRejected() error {
	return failure("dns_request_failed", "DNSPod International rejected the typed DNS operation", 502)
}
func (d *legacyDPIBackend) domain(ctx context.Context, name string) (legacyDPIDomain, error) {
	response, err := d.call(ctx, "Domain.Info", url.Values{"domain": {name}})
	if err != nil {
		return legacyDPIDomain{}, err
	}
	if response.Status.Code == "6" {
		return legacyDPIDomain{}, failure("dns_zone_missing", "No permitted DNSPod International zone matched the challenge", 412)
	}
	if response.Status.Code != "1" {
		return legacyDPIDomain{}, legacyDPIRejected()
	}
	canonical, err := legacyDNSResponseName(response.Domain.Name)
	if err != nil || canonical != name+"." || !legacyDNSNumericID.MatchString(string(response.Domain.ID)) {
		return legacyDPIDomain{}, failure("dns_response_invalid", "DNSPod International returned an invalid domain identity", 502)
	}
	return response.Domain, nil
}
func (d *legacyDPIBackend) zone(ctx context.Context, name string) (string, error) {
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for offset := 1; offset < len(labels)-1 && offset <= 32; offset++ {
		candidate := strings.Join(labels[offset:], ".")
		domain, err := d.domain(ctx, candidate)
		if err == nil {
			return legacyDNSZoneKey(string(domain.ID), candidate), nil
		}
		if typed, ok := err.(interface{ AgentError() (string, int) }); ok {
			if code, _ := typed.AgentError(); code == "dns_zone_missing" {
				continue
			}
		}
		return "", err
	}
	return "", failure("dns_zone_missing", "No permitted DNSPod International zone matched the challenge", 412)
}
func (d *legacyDPIBackend) list(ctx context.Context, name, zone string) ([]legacyDNSTXTRecord, error) {
	id, domain, err := legacyDNSZoneParts(zone)
	if err != nil || !legacyDNSWithinZone(name, domain) {
		return nil, failure("dns_receipt_conflict", "DNSPod International receipt does not cover this challenge", 409)
	}
	current, err := d.domain(ctx, domain)
	if err != nil {
		return nil, err
	}
	if string(current.ID) != id {
		return nil, failure("dns_zone_conflict", "DNSPod International domain identity changed", 409)
	}
	const pageSize = 100
	records := []legacyDNSTXTRecord{}
	seen := map[string]bool{}
	total := -1
	for offset := 0; offset < 2000; offset += pageSize {
		response, err := d.call(ctx, "Record.List", url.Values{"domain_id": {id}, "offset": {strconv.Itoa(offset)}, "length": {strconv.Itoa(pageSize)}})
		if err != nil {
			return nil, err
		}
		if response.Status.Code == "10" {
			if offset == 0 || len(seen) == total {
				return records, nil
			}
			return nil, failure("dns_response_invalid", "DNSPod International truncated record pagination", 502)
		}
		if response.Status.Code != "1" {
			return nil, legacyDPIRejected()
		}
		canonical, err := legacyDNSResponseName(response.Domain.Name)
		if err != nil || canonical != domain+"." || string(response.Domain.ID) != id {
			return nil, failure("dns_zone_conflict", "DNSPod International list returned another domain identity", 409)
		}
		if len(response.Records) > pageSize {
			return nil, failure("dns_record_limit", "DNSPod International ignored its bounded page size", 413)
		}
		pageTotal, parseErr := strconv.Atoi(response.Info.Total)
		if parseErr != nil || pageTotal < 0 || (total >= 0 && pageTotal != total) {
			return nil, failure("dns_response_invalid", "DNSPod International returned inconsistent pagination metadata", 502)
		}
		total = pageTotal
		if total > 2000 {
			return nil, failure("dns_record_limit", "DNSPod International record count exceeds its discovery budget", 413)
		}
		for _, item := range response.Records {
			recordID := string(item.ID)
			if !legacyDNSNumericID.MatchString(recordID) || seen[recordID] {
				return nil, failure("dns_response_invalid", "DNSPod International repeated or omitted a record identity", 502)
			}
			seen[recordID] = true
			if item.Type != "TXT" {
				continue
			}
			canonical, err := legacyOnlineRecordName(item.Name, domain)
			if err != nil {
				return nil, err
			}
			if canonical != name {
				continue
			}
			if item.Enabled != "1" && item.Enabled != "0" {
				return nil, failure("dns_response_invalid", "DNSPod International omitted TXT publication state", 502)
			}
			// DNSPod returns TXT content as a literal string (not a DNS presentation string).
			if len(item.Value) > 64<<10 || strings.ContainsAny(item.Value, "\x00\r\n") {
				return nil, failure("dns_response_invalid", "DNSPod International returned invalid TXT content", 502)
			}
			records = append(records, legacyDNSTXTRecord{ID: recordID, Name: canonical, Value: item.Value, Disabled: item.Enabled == "0"})
		}
		if len(seen) > total {
			return nil, failure("dns_response_invalid", "DNSPod International record count disagrees with its page", 502)
		}
		if len(seen) == total {
			return records, nil
		}
		if len(response.Records) < pageSize {
			return nil, failure("dns_response_invalid", "DNSPod International truncated record pagination", 502)
		}
	}
	return nil, failure("dns_record_limit", "DNSPod International discovery exceeds its pagination budget", 413)
}
func (d *legacyDPIBackend) create(ctx context.Context, receipt *legacyDNSReceipt) error {
	id, domain, err := legacyDNSZoneParts(receipt.Zone)
	if err != nil || !legacyDNSWithinZone(receipt.Name, domain) {
		return failure("dns_receipt_conflict", "DNSPod International create needs a matching zone", 409)
	}
	relative := strings.TrimSuffix(strings.TrimSuffix(receipt.Name, "."), "."+domain)
	response, err := d.call(ctx, "Record.Create", url.Values{"domain_id": {id}, "sub_domain": {relative}, "record_type": {"TXT"}, "record_line": {"default"}, "value": {receipt.Value}, "ttl": {"600"}})
	if err != nil {
		return err
	}
	if response.Status.Code != "1" {
		return legacyDPIRejected()
	}
	if !legacyDNSNumericID.MatchString(string(response.Record.ID)) || response.Record.Name != relative || response.Record.Status != "enable" {
		return failure("dns_response_invalid", "DNSPod International did not acknowledge an exact enabled record identity", 502)
	}
	receipt.RecordID = string(response.Record.ID)
	return nil
}
func (d *legacyDPIBackend) remove(ctx context.Context, receipt legacyDNSReceipt) error {
	id, domain, err := legacyDNSZoneParts(receipt.Zone)
	if err != nil || !legacyDNSWithinZone(receipt.Name, domain) || !legacyDNSNumericID.MatchString(receipt.RecordID) {
		return failure("dns_receipt_conflict", "DNSPod International cleanup requires its exact owned numeric ID", 409)
	}
	response, err := d.call(ctx, "Record.Remove", url.Values{"domain_id": {id}, "record_id": {receipt.RecordID}})
	if err != nil {
		return err
	}
	if response.Status.Code != "1" {
		return legacyDPIRejected()
	}
	return nil
}
