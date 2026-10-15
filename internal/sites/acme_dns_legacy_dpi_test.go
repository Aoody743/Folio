package sites

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Aoody743/PowerLinux/internal/core"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func legacyDPIFixtureRequest(t *testing.T, state *legacyHTTPSFixture, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	form := r.PostForm
	response := map[string]any{"status": map[string]string{"code": "1"}}
	domain := map[string]string{"id": state.domainID, "name": "example.test"}
	switch r.URL.Path {
	case "/Domain.Info":
		if form.Get("domain") != "example.test" {
			response["status"] = map[string]string{"code": "6"}
		} else {
			response["domain"] = domain
		}
	case "/Record.List":
		if form.Get("domain_id") != state.domainID || form.Get("length") != "100" {
			t.Error("DPI list omitted bound domain or page size")
		}
		offset, err := strconv.Atoi(form.Get("offset"))
		if err != nil || offset < 0 {
			t.Fatal("DPI invalid page offset")
		}
		state.pages++
		if state.repeatPage && offset > 0 {
			offset = 0
		}
		offset = min(offset, len(state.records))
		end := min(offset+100, len(state.records))
		rows := []map[string]string{}
		for _, record := range state.records[offset:end] {
			value := record.wire
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
			enabled := "1"
			if record.status == "disabled" {
				enabled = "0"
			}
			rows = append(rows, map[string]string{"id": record.id, "name": strings.TrimSuffix(record.name, ".example.test"), "type": record.kind, "value": value, "enabled": enabled})
		}
		response["domain"] = domain
		response["records"] = rows
		response["info"] = map[string]string{"record_total": strconv.Itoa(len(state.records))}
	case "/Record.Create":
		if form.Get("domain_id") != state.domainID || form.Get("sub_domain") != "_acme-challenge" || form.Get("record_type") != "TXT" || form.Get("record_line") != "default" || form.Get("ttl") != "600" {
			t.Error("DPI create exceeded typed schema")
		}
		state.adds++
		record := legacyHTTPSRecord{id: strconv.Itoa(999 + state.adds), name: "_acme-challenge.example.test", kind: "TXT", wire: strconv.Quote(form.Get("value")), status: "enabled"}
		if state.deferCreate {
			state.pending = append(state.pending, record)
		} else {
			state.records = append(state.records, record)
		}
		if state.failCreate {
			w.WriteHeader(500)
			fmt.Fprint(w, "provider echoes fixture-private-key")
			return
		}
		response["record"] = map[string]string{"id": record.id, "name": "_acme-challenge", "status": "enable"}
	case "/Record.Remove":
		if form.Get("domain_id") != state.domainID || form.Get("record_id") == "" {
			t.Error("DPI cleanup omitted exact IDs")
		}
		found := false
		for i, record := range state.records {
			if record.id == form.Get("record_id") {
				state.records = append(state.records[:i], state.records[i+1:]...)
				state.deletes++
				found = true
				break
			}
		}
		if !found {
			response["status"] = map[string]string{"code": "8"}
		}
	default:
		t.Error("unreviewed DPI action")
		w.WriteHeader(400)
		return
	}
	json.NewEncoder(w).Encode(response)
}
func TestLegacyDPIVerifiedHTTPSOwnershipAndDurableCleanup(t *testing.T) {
	legacyHTTPSExerciseLifecycle(t, "dns_dpi")
}
func TestLegacyDPIResponseLossAndDelayedCommitRecovery(t *testing.T) {
	legacyHTTPSExerciseLostResponse(t, "dns_dpi")
}
func TestLegacyDPIBorrowedRecordsAndChangedIdentities(t *testing.T) {
	legacyHTTPSExerciseBorrowedAndIdentityConflicts(t, "dns_dpi")
}
func TestLegacyDPIFixedAPIAndVerifiedTLSSafeFailure(t *testing.T) {
	legacyHTTPSExerciseFixedPolicyTLSRedirectAndResponseLimits(t, "dns_dpi")
}

func TestLegacyDPIPaginationAndDisabledPreflight(t *testing.T) {
	for _, mode := range []string{"pages", "repeated_page", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			options, state := legacyReviewedHTTPSFixture(t, "dns_dpi")
			provider, err := newLegacyDNSProvider(options)
			if err != nil {
				t.Fatal(err)
			}
			const domain, token, authorization = "example.test", "dpi-page-token", "dpi-page-token.thumbprint"
			if mode == "disabled" {
				_, value, _ := provider.(*legacyDNSClient).challenge(domain, token, authorization)
				state.records = append(state.records, legacyHTTPSRecord{id: "10", name: "_acme-challenge.example.test", kind: "TXT", wire: strconv.Quote(value), status: "disabled"})
			} else {
				for i := range 105 {
					state.records = append(state.records, legacyHTTPSRecord{id: strconv.Itoa(2000 + i), name: "_acme-challenge.example.test", kind: "TXT", wire: strconv.Quote(fmt.Sprintf("unrelated-%d", i)), status: "enabled"})
				}
				state.repeatPage = mode == "repeated_page"
			}
			err = provider.Present(context.Background(), domain, token, authorization)
			if mode == "disabled" {
				if core.ErrorCode(err) != "dns_record_disabled" {
					t.Fatal(err)
				}
			} else if mode == "repeated_page" {
				if core.ErrorCode(err) != "dns_response_invalid" {
					t.Fatal(err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err = provider.CleanUp(context.Background(), domain, token, authorization); err != nil {
					t.Fatal(err)
				}
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if mode == "pages" {
				if state.adds != 1 || state.deletes != 1 || len(state.records) != 107 || state.pages < 4 {
					t.Fatal("pagination or preservation failed")
				}
			} else if state.adds != 0 || state.deletes != 0 {
				t.Fatal("unsafe preflight mutated records")
			}
		})
	}
}
func TestLegacyDPIConcurrentChallengesPreserveOtherValues(t *testing.T) {
	options, state := legacyReviewedHTTPSFixture(t, "dns_dpi")
	provider, err := newLegacyDNSProvider(options)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, token := range []string{"dpi-first", "dpi-second"} {
		wg.Add(1)
		go func(token string) {
			defer wg.Done()
			if err := provider.Present(context.Background(), "example.test", token, token+".thumbprint"); err != nil {
				t.Error(err)
			}
		}(token)
	}
	wg.Wait()
	state.mu.Lock()
	if state.adds != 2 || len(state.records) != 4 {
		t.Error("parallel values did not coexist")
	}
	state.mu.Unlock()
	for _, token := range []string{"dpi-first", "dpi-second"} {
		if err := provider.CleanUp(context.Background(), "example.test", token, token+".thumbprint"); err != nil {
			t.Fatal(err)
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.deletes != 2 || len(state.records) != 2 {
		t.Fatal("cleanup changed unrelated values")
	}
}

func TestLegacyDPIExplicitAuthenticationSchema(t *testing.T) {
	for _, field := range []string{"", "login_token", "user_token", "domain_id", "format"} {
		t.Run("auth_"+field, func(t *testing.T) {
			options, state := legacyReviewedHTTPSFixture(t, "dns_dpi")
			options.Credentials["DPI_AuthField"] = field
			schemaErr := ValidateDNSCredentials("dns_dpi", options.Credentials)
			provider, err := newLegacyDNSProvider(options)
			if field == "domain_id" || field == "format" {
				if err == nil || schemaErr == nil {
					t.Fatal("unreviewed authentication selector was accepted")
				}
				state.mu.Lock()
				defer state.mu.Unlock()
				if state.requests != 0 {
					t.Fatal("invalid schema sent credentials")
				}
				return
			}
			if err != nil || schemaErr != nil {
				t.Fatal(err, schemaErr)
			}
			const token = "dpi-auth-token"
			if err = provider.Present(context.Background(), "example.test", token, token+".thumbprint"); err != nil {
				t.Fatal(err)
			}
			if err = provider.CleanUp(context.Background(), "example.test", token, token+".thumbprint"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
