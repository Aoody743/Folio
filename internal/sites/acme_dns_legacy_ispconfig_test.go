package sites

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

type legacyISPFixture struct {
	mu                    sync.Mutex
	records               []legacyISPRecord
	logins, adds, deletes int
	fail                  bool
	failAddResponse       bool
	deferAddCommit        bool
	pending               []legacyISPRecord
}

func legacyISPProtocolFixture(t *testing.T) (legacyDNSOptions, *legacyISPFixture) {
	t.Helper()
	state := &legacyISPFixture{records: []legacyISPRecord{{ID: "9", Name: "_acme-challenge.example.test.", Type: "TXT", Data: "unrelated-existing-value", Zone: "21"}}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		if request.Method != http.MethodPost || request.URL.Path != "/remote/json.php" || request.Header.Get("Content-Type") != "application/json" {
			t.Error("unexpected ISPConfig HTTPS RPC shape")
			writer.WriteHeader(400)
			return
		}
		if state.fail {
			writer.WriteHeader(500)
			writer.Write([]byte("not-a-real-account-password sensitive-session"))
			return
		}
		var input map[string]json.RawMessage
		if json.NewDecoder(request.Body).Decode(&input) != nil {
			t.Error("invalid RPC JSON")
			writer.WriteHeader(400)
			return
		}
		text := func(field string) string {
			var value string
			json.Unmarshal(input[field], &value)
			return value
		}
		operation := request.URL.RawQuery
		if operation != "login" && text("session_id") != "sensitive-session" {
			json.NewEncoder(writer).Encode(map[string]any{"code": "session_invalid", "response": "private diagnostic"})
			return
		}
		var response any
		switch operation {
		case "login":
			if text("username") != "fixture-remote-user" || text("password") != "not-a-real-account-password" {
				t.Error("credentials did not arrive exclusively in the login body")
			}
			state.logins++
			response = "sensitive-session"
		case "dns_zone_get":
			var primary map[string]string
			json.Unmarshal(input["primary_id"], &primary)
			if primary["origin"] == "example.test." {
				response = []map[string]any{{"id": "21", "server_id": 7, "sys_userid": "31", "origin": "example.test."}}
			} else {
				response = []any{}
			}
		case "client_get_id":
			if text("sys_userid") != "31" {
				t.Error("client lookup did not use the observed zone owner")
			}
			response = 43
		case "dns_txt_get":
			var primary map[string]string
			json.Unmarshal(input["primary_id"], &primary)
			if primary["type"] != "TXT" || primary["zone"] != "21" || primary["name"] != "_acme-challenge.example.test." {
				t.Error("TXT query crossed its observed name/zone")
			}
			response = append([]legacyISPRecord{}, state.records...)
		case "dns_txt_add":
			var params map[string]string
			json.Unmarshal(input["params"], &params)
			if params["server_id"] != "7" || params["zone"] != "21" || params["name"] != "_acme-challenge.example.test." || params["type"] != "txt" || text("client_id") != "43" {
				t.Error("record create did not use typed observed zone/server/client fields")
			}
			state.adds++
			record := legacyISPRecord{ID: "100", Name: params["name"], Type: "TXT", Data: params["data"], Zone: "21"}
			if state.deferAddCommit {
				state.pending = append(state.pending, record)
			} else {
				state.records = append(state.records, record)
			}
			if state.failAddResponse {
				writer.WriteHeader(500)
				writer.Write([]byte("not-a-real-account-password sensitive-session"))
				return
			}
			response = "100"
		case "dns_txt_delete":
			if text("primary_id") != "100" {
				t.Error("cleanup requested somebody else's record ID")
			}
			state.deletes++
			next := []legacyISPRecord{}
			for _, record := range state.records {
				if string(record.ID) != text("primary_id") {
					next = append(next, record)
				}
			}
			state.records = next
			response = 1
		default:
			t.Error("arbitrary ISPConfig RPC operation was attempted")
			writer.WriteHeader(400)
			return
		}
		json.NewEncoder(writer).Encode(map[string]any{"code": "ok", "response": response})
	}))
	t.Cleanup(server.Close)
	url := server.URL + "/remote/json.php"
	return legacyDNSOptions{
		Provider: "dns_ispconfig", OwnerID: "certificate-one", State: legacyPrivateDNSState(t),
		Credentials:    map[string]string{"ISPC_User": "fixture-remote-user", "ISPC_Password": "not-a-real-account-password", "ISPC_Api": url},
		AllowedAPIURLs: []string{url}, HTTPClient: server.Client(),
	}, state
}

func TestLegacyISPConfigLostCreateResponseKeepsReceiptUntilCommitRecovery(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		name := "already_committed"
		if delayed {
			name = "delayed_commit"
		}
		t.Run(name, func(t *testing.T) {
			options, state := legacyISPProtocolFixture(t)
			state.failAddResponse, state.deferAddCommit = true, delayed
			provider, err := newLegacyDNSProvider(options)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			const domain, token, authorization = "example.test", "lost-response", "lost-response.thumbprint"
			if err := provider.Present(ctx, domain, token, authorization); err == nil || strings.Contains(err.Error(), "not-a-real-account-password") || strings.Contains(err.Error(), "sensitive-session") {
				t.Fatal("lost API response was accepted or exposed credentials", err)
			}
			client := provider.(*legacyDNSClient)
			challengeName, value, _ := client.challenge(domain, token, authorization)
			receipt, err := client.loadReceipt(challengeName, value)
			if err != nil || receipt.State != "prepared" || receipt.RecordID != "" {
				t.Fatal("lost response did not leave a durable preparation baseline", err)
			}
			restarted, err := newLegacyDNSProvider(options)
			if err != nil {
				t.Fatal(err)
			}
			if delayed {
				if err := restarted.Present(ctx, domain, token, authorization); core.ErrorCode(err) != "dns_creation_pending" {
					t.Fatal("unresolved creation was replayed instead of retained", err)
				}
				if err := restarted.CleanUp(ctx, domain, token, authorization); core.ErrorCode(err) != "dns_cleanup_pending" {
					t.Fatal("unknown remote commit was treated as proven absent", err)
				}
				if _, err := client.loadReceipt(challengeName, value); err != nil {
					t.Fatal("pending cleanup erased ownership recovery metadata", err)
				}
				state.mu.Lock()
				if state.adds != 1 || state.deletes != 0 || len(state.pending) != 1 {
					t.Error("pending response caused a second API create or premature deletion")
				}
				// The HTTPS fixture now completes the request whose acknowledgement
				// was lost; recovery must survive that remote commit arriving later.
				state.records = append(state.records, state.pending...)
				state.pending = nil
				state.mu.Unlock()
			}
			if err := restarted.CleanUp(ctx, domain, token, authorization); err != nil {
				t.Fatal(err)
			}
			if _, err := options.State.Stat(client.receiptPath(challengeName, value)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("verified cleanup did not retire its receipt", err)
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.adds != 1 || state.deletes != 1 || len(state.records) != 1 || state.records[0].ID != "9" {
				t.Fatal("response-loss recovery altered unrelated records or replayed creation")
			}
		})
	}
}

func TestLegacyISPConfigVerifiedHTTPSObservedZoneAndDurableCleanup(t *testing.T) {
	options, state := legacyISPProtocolFixture(t)
	provider, err := newLegacyDNSProvider(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := provider.Present(ctx, "example.test", "token-one", "token-one.thumbprint"); err != nil {
		t.Fatal(err)
	}
	client := provider.(*legacyDNSClient)
	name, value, _ := client.challenge("example.test", "token-one", "token-one.thumbprint")
	receipt, err := client.loadReceipt(name, value)
	if err != nil || receipt.RecordID != "100" || receipt.Zone != "21" || receipt.State != "created" {
		t.Fatal("native API ownership was not persisted", err)
	}
	raw, err := options.State.ReadFile(client.receiptPath(name, value))
	if err != nil || strings.Contains(string(raw), "not-a-real-account-password") || strings.Contains(string(raw), "sensitive-session") {
		t.Fatal("receipt stored credentials/session state", err)
	}
	// A new root-Agent provider must delete its persisted record ID without
	// depending on the old process's in-memory session or record map.
	restarted, err := newLegacyDNSProvider(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.CleanUp(ctx, "example.test", "token-one", "token-one.thumbprint"); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.adds != 1 || state.deletes != 1 || state.logins != 2 || len(state.records) != 1 || state.records[0].ID != "9" {
		t.Fatal("native cleanup altered an unrelated record or depended on memory-only ownership")
	}
}

func TestLegacyISPConfigBorrowedChangedRecordsPolicyAndTLSRejections(t *testing.T) {
	options, state := legacyISPProtocolFixture(t)
	ctx := context.Background()
	denied := options
	denied.AllowedAPIURLs = nil
	if _, err := newLegacyDNSProvider(denied); core.ErrorCode(err) != "policy_denied" {
		t.Fatal("custom DNS endpoint did not require root approval", err)
	}
	insecure := options
	insecure.HTTPClient = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if _, err := newLegacyDNSProvider(insecure); core.ErrorCode(err) != "tls_required" {
		t.Fatal("insecure TLS fixture was accepted", err)
	}
	provider, err := newLegacyDNSProvider(options)
	if err != nil {
		t.Fatal(err)
	}
	client := provider.(*legacyDNSClient)
	_, borrowed, _ := client.challenge("example.test", "borrowed-token", "borrowed-token.thumbprint")
	state.mu.Lock()
	state.records = append(state.records, legacyISPRecord{ID: "10", Name: "_acme-challenge.example.test.", Type: "TXT", Data: borrowed, Zone: "21"})
	state.mu.Unlock()
	if err := provider.Present(ctx, "example.test", "borrowed-token", "borrowed-token.thumbprint"); err != nil {
		t.Fatal(err)
	}
	if err := provider.CleanUp(ctx, "example.test", "borrowed-token", "borrowed-token.thumbprint"); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	if state.adds != 0 || state.deletes != 0 || len(state.records) != 2 {
		t.Error("borrowed pre-existing record was mutated")
	}
	state.mu.Unlock()
	if err := provider.Present(ctx, "example.test", "owned-token", "owned-token.thumbprint"); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	for index := range state.records {
		if state.records[index].ID == "100" {
			state.records[index].Data = "somebody-else-changed-this-record"
		}
	}
	state.mu.Unlock()
	if err := provider.CleanUp(ctx, "example.test", "owned-token", "owned-token.thumbprint"); core.ErrorCode(err) != "dns_record_conflict" {
		t.Fatal("changed owned ID was deleted instead of retained", err)
	}
	state.mu.Lock()
	state.fail = true
	state.mu.Unlock()
	if err := provider.Present(ctx, "example.test", "server-failed", "server-failed.thumbprint"); err == nil || strings.Contains(err.Error(), "not-a-real-account-password") || strings.Contains(err.Error(), "sensitive-session") {
		t.Fatal("provider diagnostics leaked secrets or became success")
	}
}
