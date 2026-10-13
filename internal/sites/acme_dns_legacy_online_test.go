package sites

import (
	"context"
	"strconv"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestLegacyOnlineVerifiedHTTPSOwnershipAndDurableCleanup(t *testing.T) {
	legacyHTTPSExerciseLifecycle(t, "dns_online")
}
func TestLegacyOnlineResponseLossAndDelayedCommitRecovery(t *testing.T) {
	legacyHTTPSExerciseLostResponse(t, "dns_online")
}
func TestLegacyOnlineBorrowedRecordsAndChangedIdentities(t *testing.T) {
	legacyHTTPSExerciseBorrowedAndIdentityConflicts(t, "dns_online")
}
func TestLegacyOnlineFixedAPIAndVerifiedTLSSafeFailure(t *testing.T) {
	legacyHTTPSExerciseFixedPolicyTLSRedirectAndResponseLimits(t, "dns_online")
}
func TestLegacyOnlineExactDataFilterRejectsAmbiguousValueDeletion(t *testing.T) {
	options, state := legacyReviewedHTTPSFixture(t, "dns_online")
	provider, err := newLegacyDNSProvider(options)
	if err != nil {
		t.Fatal(err)
	}
	const domain, token, authorization = "example.test", "ambiguous-token", "ambiguous-token.thumbprint"
	ctx := context.Background()
	if err := provider.Present(ctx, domain, token, authorization); err != nil {
		t.Fatal(err)
	}
	client := provider.(*legacyDNSClient)
	name, value, _ := client.challenge(domain, token, authorization)
	state.mu.Lock()
	state.records = append(state.records, legacyHTTPSRecord{id: "1001", name: "_acme-challenge.example.test", kind: "TXT", wire: strconv.Quote(value), status: "enabled"})
	state.mu.Unlock()
	if err := provider.CleanUp(ctx, domain, token, authorization); core.ErrorCode(err) != "dns_ownership_ambiguous" {
		t.Fatal("value-filter API was allowed to delete another identical TXT identity", err)
	}
	if _, err := client.loadReceipt(name, value); err != nil {
		t.Fatal("ambiguous cleanup lost its ownership receipt", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.deletes != 0 || len(state.records) != 4 {
		t.Fatal("ambiguous cleanup changed provider records")
	}
}
