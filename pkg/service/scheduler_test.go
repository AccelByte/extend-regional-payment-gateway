package service

import "testing"

func TestProviderTxIDForClaimUsesPolledPaymentID(t *testing.T) {
	if got := providerTxIDForClaim("sess_123", "pay_123"); got != "pay_123" {
		t.Fatalf("providerTxIDForClaim() = %q, want pay_123", got)
	}
	if got := providerTxIDForClaim("sess_123", ""); got != "sess_123" {
		t.Fatalf("providerTxIDForClaim() fallback = %q, want sess_123", got)
	}
}
