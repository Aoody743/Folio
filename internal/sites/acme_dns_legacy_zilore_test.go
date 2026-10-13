package sites

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestLegacyZiloreVerifiedHTTPSOwnershipAndDurableCleanup(t *testing.T) {
	legacyHTTPSExerciseLifecycle(t, "dns_zilore")
}
func TestLegacyZiloreResponseLossAndDelayedCommitRecovery(t *testing.T) {
	legacyHTTPSExerciseLostResponse(t, "dns_zilore")
}
func TestLegacyZiloreBorrowedRecordsAndChangedIdentities(t *testing.T) {
	legacyHTTPSExerciseBorrowedAndIdentityConflicts(t, "dns_zilore")
}
func TestLegacyZiloreFixedAPIAndVerifiedTLSSafeFailure(t *testing.T) {
	legacyHTTPSExerciseFixedPolicyTLSRedirectAndResponseLimits(t, "dns_zilore")
}
func TestLegacyZiloreActualPaginationAndDisabledRecordPreflight(t *testing.T) {
	for _, mode := range []string{"pages", "repeated_page", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			options, state := legacyReviewedHTTPSFixture(t, "dns_zilore")
			provider, err := newLegacyDNSProvider(options)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			const domain, token, authorization = "example.test", "pagination-token", "pagination-token.thumbprint"
			if mode == "disabled" {
				_, value, _ := provider.(*legacyDNSClient).challenge(domain, token, authorization)
				state.records = append(state.records, legacyHTTPSRecord{id: "10", name: "_acme-challenge.example.test", kind: "TXT", wire: strconv.Quote(value), status: "disabled"})
				if err := provider.Present(ctx, domain, token, authorization); core.ErrorCode(err) != "dns_record_disabled" {
					t.Fatal("disabled borrowed TXT was treated as a published challenge", err)
				}
			} else {
				state.paginatedDomains = true
				for index := range 105 {
					state.records = append(state.records, legacyHTTPSRecord{id: strconv.Itoa(2000 + index), name: "_acme-challenge.example.test", kind: "TXT", wire: strconv.Quote(fmt.Sprintf("unrelated-%d", index)), status: "enabled"})
				}
				state.records = append(state.records, legacyHTTPSRecord{id: "4000", name: "nearby.example.test", kind: "TXT", wire: strconv.Quote("search-nearby"), status: "enabled"})
				state.repeatPage = mode == "repeated_page"
				err := provider.Present(ctx, domain, token, authorization)
				if mode == "repeated_page" {
					if core.ErrorCode(err) != "dns_response_invalid" {
						t.Fatal("a repeated record page was used as an ownership baseline", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					restarted, err := newLegacyDNSProvider(options)
					if err != nil {
						t.Fatal(err)
					}
					if err := restarted.CleanUp(ctx, domain, token, authorization); err != nil {
						t.Fatal(err)
					}
				}
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if mode == "pages" {
				if state.adds != 1 || state.deletes != 1 || len(state.records) != 108 || state.pages < 4 {
					t.Fatal("actual two-page discovery or unrelated-record preservation failed")
				}
			} else if state.adds != 0 || state.deletes != 0 {
				t.Fatal("unsafe record preflight caused a mutation")
			}
		})
	}
}
