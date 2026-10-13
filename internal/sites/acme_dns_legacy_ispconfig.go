package sites

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type legacyDNSID string

var legacyDNSNumericID = regexp.MustCompile("^[1-9][0-9]{0,19}$")

func (id *legacyDNSID) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		*id = ""
		return nil
	}
	value := string(data)
	if len(data) > 0 && data[0] == '"' {
		if json.Unmarshal(data, &value) != nil {
			return invalid("Invalid DNS API record identifier")
		}
	}
	if !legacyDNSNumericID.MatchString(value) {
		return invalid("DNS API record identifier must be a bounded positive number")
	}
	*id = legacyDNSID(value)
	return nil
}

type legacyISPZone struct {
	ID        legacyDNSID `json:"id"`
	ServerID  legacyDNSID `json:"server_id"`
	SysUserID legacyDNSID `json:"sys_userid"`
	Origin    string      `json:"origin"`
}

type legacyISPRecord struct {
	ID   legacyDNSID `json:"id"`
	Name string      `json:"name"`
	Type string      `json:"type"`
	Data string      `json:"data"`
	Zone legacyDNSID `json:"zone"`
}

type legacyISPBackend struct {
	client  *legacyDNSClient
	base    *url.URL
	session string
	zones   map[string]legacyISPZone
}

func init() {
	legacyDNSFactories["dns_ispconfig"] = newLegacyISPBackend
}

func newLegacyISPBackend(client *legacyDNSClient) (legacyDNSBackend, error) {
	endpoint := client.options.Credentials["ISPC_Api"]
	base, err := legacyDNSAPIURL(endpoint)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(base.Path, "/remote/json.php") {
		return nil, invalid("ISPConfig uses its verified remote/json.php endpoint")
	}
	if !slices.Contains(client.options.AllowedAPIURLs, endpoint) {
		return nil, failure("policy_denied", "ISPConfig API URL must exactly match the root-configured allowlist", 403)
	}
	return &legacyISPBackend{client: client, base: base, zones: map[string]legacyISPZone{}}, nil
}

func (i *legacyISPBackend) call(ctx context.Context, operation string, payload any) (json.RawMessage, error) {
	endpoint := *i.base
	endpoint.RawQuery = operation
	headers := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}}
	data, _, err := i.client.request(ctx, http.MethodPost, &endpoint, headers, encode(payload))
	if err != nil {
		return nil, err
	}
	var response struct {
		Code     string          `json:"code"`
		Response json.RawMessage `json:"response"`
	}
	if err := legacyDNSDecode(data, &response); err != nil {
		return nil, err
	}
	if response.Code != "ok" {
		code, status := "dns_request_failed", 502
		if operation == "login" || strings.Contains(response.Code, "session") || strings.Contains(response.Code, "permission") {
			code, status = "dns_auth_failed", 403
		}
		return nil, failure(code, "ISPConfig rejected the typed DNS operation", status)
	}
	return response.Response, nil
}

func (i *legacyISPBackend) login(ctx context.Context) error {
	if i.session != "" {
		return nil
	}
	body, err := i.call(ctx, "login", map[string]any{
		"username":     i.client.options.Credentials["ISPC_User"],
		"password":     i.client.options.Credentials["ISPC_Password"],
		"client_login": false,
	})
	if err != nil {
		return err
	}
	if json.Unmarshal(body, &i.session) != nil || len(i.session) == 0 || len(i.session) > 1024 || strings.ContainsAny(i.session, "\x00\r\n") {
		return failure("dns_response_invalid", "ISPConfig returned an invalid session identity", 502)
	}
	return nil
}

// Decode record arrays incrementally, stopping before allocating an unbounded
// provider-controlled array of structs. Some ISPConfig versions return a
// single object for a primary ID and an array for a filter.
func legacyDNSObjects[T any](raw json.RawMessage) ([]T, error) {
	data := bytes.TrimSpace(raw)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return []T{}, nil
	}
	if data[0] != '[' {
		var one T
		if err := legacyDNSDecode(data, &one); err != nil {
			return nil, err
		}
		return []T{one}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if _, err := decoder.Token(); err != nil {
		return nil, failure("dns_response_invalid", "DNS provider returned an invalid record array", 502)
	}
	results := []T{}
	for decoder.More() {
		if len(results) >= 2000 {
			return nil, failure("dns_record_limit", "DNS provider record collection exceeds 2000 entries", 413)
		}
		var item T
		if err := decoder.Decode(&item); err != nil {
			return nil, failure("dns_response_invalid", "DNS provider returned an invalid record schema", 502)
		}
		results = append(results, item)
	}
	if _, err := decoder.Token(); err != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, failure("dns_response_invalid", "DNS provider returned an invalid record array", 502)
	}
	return results, nil
}

func (i *legacyISPBackend) zone(ctx context.Context, name string) (string, error) {
	if err := i.login(ctx); err != nil {
		return "", err
	}
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for offset := 1; offset < len(labels)-1 && offset <= 32; offset++ {
		candidate := strings.Join(labels[offset:], ".") + "."
		raw, err := i.call(ctx, "dns_zone_get", map[string]any{"session_id": i.session, "primary_id": map[string]string{"origin": candidate}})
		if err != nil {
			return "", err
		}
		zones, err := legacyDNSObjects[legacyISPZone](raw)
		if err != nil {
			return "", err
		}
		for _, zone := range zones {
			if strings.EqualFold(zone.Origin, candidate) && zone.ID != "" && zone.ServerID != "" && zone.SysUserID != "" {
				i.zones[string(zone.ID)] = zone
				return string(zone.ID), nil
			}
		}
	}
	return "", failure("dns_zone_missing", "No permitted ISPConfig DNS zone matched the challenge", 412)
}

func (i *legacyISPBackend) list(ctx context.Context, name, zone string) ([]legacyDNSTXTRecord, error) {
	if !legacyDNSNumericID.MatchString(zone) {
		return nil, failure("dns_receipt_conflict", "ISPConfig receipt has an invalid zone identity", 409)
	}
	if err := i.login(ctx); err != nil {
		return nil, err
	}
	raw, err := i.call(ctx, "dns_txt_get", map[string]any{"session_id": i.session, "primary_id": map[string]string{"name": name, "type": "TXT", "zone": zone}})
	if err != nil {
		return nil, err
	}
	all, err := legacyDNSObjects[legacyISPRecord](raw)
	if err != nil {
		return nil, err
	}
	records := []legacyDNSTXTRecord{}
	for _, record := range all {
		canonical, err := canonicalLegacyDNSName(record.Name)
		if err != nil || record.ID == "" {
			return nil, failure("dns_response_invalid", "ISPConfig returned an invalid TXT identity", 502)
		}
		if canonical == name && strings.EqualFold(record.Type, "TXT") && string(record.Zone) == zone {
			records = append(records, legacyDNSTXTRecord{ID: string(record.ID), Name: canonical, Value: record.Data})
		}
	}
	return records, nil
}

func (i *legacyISPBackend) create(ctx context.Context, receipt *legacyDNSReceipt) error {
	zone, found := i.zones[receipt.Zone]
	if !found {
		return failure("dns_zone_missing", "ISPConfig creation requires a fresh zone preflight", 412)
	}
	raw, err := i.call(ctx, "client_get_id", map[string]string{"session_id": i.session, "sys_userid": string(zone.SysUserID)})
	if err != nil {
		return err
	}
	var clientID legacyDNSID
	if err := legacyDNSDecode(raw, &clientID); err != nil {
		return err
	}
	raw, err = i.call(ctx, "dns_txt_add", map[string]any{
		"session_id": i.session, "client_id": string(clientID), "update_serial": true,
		"params": map[string]string{
			"server_id": string(zone.ServerID), "zone": receipt.Zone,
			"name": receipt.Name, "type": "txt", "data": receipt.Value,
			"aux": "0", "ttl": "60", "active": "y",
			"stamp":  i.client.options.Now().UTC().Format("2006-01-02 15:04:05"),
			"serial": strconv.FormatInt(i.client.options.Now().Unix(), 10),
		},
	})
	if err != nil {
		return err
	}
	var recordID legacyDNSID
	if err := legacyDNSDecode(raw, &recordID); err != nil {
		return err
	}
	receipt.RecordID = string(recordID)
	return nil
}

func (i *legacyISPBackend) remove(ctx context.Context, receipt legacyDNSReceipt) error {
	if !legacyDNSNumericID.MatchString(receipt.RecordID) {
		return failure("dns_receipt_conflict", "ISPConfig cleanup requires the exact owned numeric record ID", 409)
	}
	_, err := i.call(ctx, "dns_txt_delete", map[string]any{"session_id": i.session, "primary_id": receipt.RecordID, "update_serial": true})
	return err
}
