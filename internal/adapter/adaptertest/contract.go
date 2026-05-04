package adaptertest

import (
	"context"
	"strings"
	"testing"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
)

// Harness contains provider-specific fixtures for reusable adapter contract tests.
type Harness struct {
	Provider           adapter.PaymentProvider
	ValidInit          adapter.PaymentInitRequest
	InvalidInit        *adapter.PaymentInitRequest
	ProviderTxID       string
	WebhookBody        []byte
	WebhookHeaders     map[string]string
	Transaction        *model.Transaction
	ExpectCreateIntent bool
}

// RunContract verifies the generic adapter shape without asserting provider-specific API details.
func RunContract(t *testing.T, h Harness) {
	t.Helper()
	if h.Provider == nil {
		t.Fatal("Provider is required")
	}
	info := h.Provider.Info()
	if strings.TrimSpace(info.ID) == "" {
		t.Fatal("provider Info().ID must be non-empty")
	}
	if strings.TrimSpace(info.DisplayName) == "" {
		t.Fatal("provider Info().DisplayName must be non-empty")
	}
	if strings.ContainsAny(info.ID, " \t\r\n") {
		t.Fatalf("provider ID must be stable and whitespace-free, got %q", info.ID)
	}

	if err := h.Provider.ValidatePaymentInit(h.ValidInit); err != nil {
		t.Fatalf("ValidatePaymentInit(valid) returned error: %v", err)
	}
	if h.InvalidInit != nil {
		if err := h.Provider.ValidatePaymentInit(*h.InvalidInit); err == nil {
			t.Fatal("ValidatePaymentInit(invalid) expected error")
		}
	}

	if h.ExpectCreateIntent {
		intent, err := h.Provider.CreatePaymentIntent(context.Background(), h.ValidInit)
		if err != nil {
			t.Fatalf("CreatePaymentIntent returned error: %v", err)
		}
		if intent == nil || strings.TrimSpace(intent.ProviderTransactionID) == "" {
			t.Fatalf("CreatePaymentIntent must return provider transaction ID, got %+v", intent)
		}
		if strings.TrimSpace(intent.PaymentURL) == "" && strings.TrimSpace(intent.QRCodeData) == "" {
			t.Fatalf("CreatePaymentIntent must return payment URL or QR data, got %+v", intent)
		}
	}

	if h.WebhookBody != nil {
		if err := h.Provider.ValidateWebhookSignature(context.Background(), h.WebhookBody, h.WebhookHeaders); err != nil {
			t.Fatalf("ValidateWebhookSignature returned error: %v", err)
		}
		result, err := h.Provider.HandleWebhook(context.Background(), h.WebhookBody, h.WebhookHeaders)
		if err != nil {
			t.Fatalf("HandleWebhook returned error: %v", err)
		}
		if result == nil || strings.TrimSpace(result.InternalOrderID) == "" {
			t.Fatalf("HandleWebhook must return internal order ID, got %+v", result)
		}
		if result.Status == "" {
			t.Fatalf("HandleWebhook must return generic payment status, got %+v", result)
		}
	}

	tx := h.Transaction
	if tx == nil {
		tx = &model.Transaction{ID: "contract-tx", ProviderTxID: h.ProviderTxID}
	}
	if _, err := h.Provider.GetPaymentStatus(context.Background(), h.ProviderTxID); err != nil && err != adapter.ErrNotSupported {
		t.Fatalf("GetPaymentStatus returned non-standard error: %v", err)
	}
	if result, err := h.Provider.SyncTransactionStatus(context.Background(), tx); err != nil {
		t.Fatalf("SyncTransactionStatus returned error: %v", err)
	} else if result == nil || result.PaymentStatus == "" || result.RefundStatus == "" {
		t.Fatalf("SyncTransactionStatus must return payment/refund status, got %+v", result)
	}
	if result, err := h.Provider.CancelPayment(context.Background(), tx, "contract test"); err != nil {
		t.Fatalf("CancelPayment returned error: %v", err)
	} else if result == nil || result.Status == "" {
		t.Fatalf("CancelPayment must return a cancel status, got %+v", result)
	}
}
