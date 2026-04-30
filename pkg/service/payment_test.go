package service

import (
	"context"
	"testing"
	"time"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/config"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
	memstore "github.com/accelbyte/extend-regional-payment-gateway/internal/store/memory"
	pb "github.com/accelbyte/extend-regional-payment-gateway/pkg/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestReturnURLWithTransactionID(t *testing.T) {
	tests := []struct {
		name          string
		rawURL        string
		transactionID string
		want          string
	}{
		{
			name:          "adds first query parameter",
			rawURL:        "https://example.test/payment/payment-result",
			transactionID: "txn-123",
			want:          "https://example.test/payment/payment-result?transactionId=txn-123",
		},
		{
			name:          "preserves existing query parameters",
			rawURL:        "https://example.test/payment/payment-result?source=dana",
			transactionID: "txn-123",
			want:          "https://example.test/payment/payment-result?source=dana&transactionId=txn-123",
		},
		{
			name:          "escapes transaction id",
			rawURL:        "https://example.test/payment/payment-result",
			transactionID: "txn 123",
			want:          "https://example.test/payment/payment-result?transactionId=txn+123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := returnURLWithTransactionID(tt.rawURL, tt.transactionID); got != tt.want {
				t.Fatalf("returnURLWithTransactionID() = %q, want %q", got, tt.want)
			}
		})
	}
}

type namedProvider struct {
	name string
}

func (p namedProvider) Name() string { return p.name }

func (namedProvider) CreatePaymentIntent(context.Context, adapter.PaymentInitRequest) (*adapter.PaymentIntent, error) {
	return nil, nil
}

func (namedProvider) GetPaymentStatus(context.Context, string) (*adapter.ProviderPaymentStatus, error) {
	return nil, adapter.ErrNotSupported
}

func (namedProvider) ValidateWebhookSignature(context.Context, []byte, map[string]string) error {
	return nil
}

func (namedProvider) HandleWebhook(context.Context, []byte, map[string]string) (*adapter.PaymentResult, error) {
	return nil, nil
}

func (namedProvider) RefundPayment(context.Context, string, string, int64, string) error {
	return nil
}

func (namedProvider) ValidateCredentials(context.Context) error {
	return nil
}

func TestResolveProviderKeyFirstClassAndLegacyXendit(t *testing.T) {
	registry := adapter.NewRegistry()
	registry.Register(namedProvider{name: model.ProviderXendit})
	registry.Register(namedProvider{name: model.ProviderKomoju})

	tests := []struct {
		name string
		req  *pb.CreatePaymentIntentRequest
		want string
	}{
		{
			name: "first class xendit",
			req:  &pb.CreatePaymentIntentRequest{Provider: pb.Provider_PROVIDER_XENDIT},
			want: model.ProviderXendit,
		},
		{
			name: "legacy custom xendit",
			req:  &pb.CreatePaymentIntentRequest{Provider: pb.Provider_PROVIDER_CUSTOM, CustomProviderName: "xendit"},
			want: model.ProviderXendit,
		},
		{
			name: "first class dana",
			req:  &pb.CreatePaymentIntentRequest{Provider: pb.Provider_PROVIDER_DANA},
			want: model.ProviderDana,
		},
		{
			name: "first class komoju",
			req:  &pb.CreatePaymentIntentRequest{Provider: pb.Provider_PROVIDER_KOMOJU},
			want: model.ProviderKomoju,
		},
		{
			name: "legacy custom komoju",
			req:  &pb.CreatePaymentIntentRequest{Provider: pb.Provider_PROVIDER_CUSTOM, CustomProviderName: "komoju"},
			want: model.ProviderKomoju,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveProviderKey(tt.req, registry); got != tt.want {
				t.Fatalf("resolveProviderKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMapProviderFirstClassXendit(t *testing.T) {
	if got := mapProvider(model.ProviderXendit); got != pb.Provider_PROVIDER_XENDIT {
		t.Fatalf("mapProvider(xendit) = %v, want PROVIDER_XENDIT", got)
	}
}

func TestMapProviderFirstClassKomoju(t *testing.T) {
	if got := mapProvider(model.ProviderKomoju); got != pb.Provider_PROVIDER_KOMOJU {
		t.Fatalf("mapProvider(komoju) = %v, want PROVIDER_KOMOJU", got)
	}
}

func TestCancelSelectedProviderForExistingTransactionClearsProvider(t *testing.T) {
	ctx := context.Background()
	txStore := memstore.New()
	tx := &model.Transaction{
		ID:            "txn-clear-provider",
		ClientOrderID: "order-clear-provider",
		UserID:        "user-1",
		Namespace:     "ns",
		Status:        model.StatusPending,
		Provider:      "refund_stub",
		ProviderTxID:  "provider-tx-1",
		PaymentURL:    "https://pay.example.test/1",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(time.Minute),
	}
	if err := txStore.CreateTransaction(ctx, tx); err != nil {
		t.Fatalf("CreateTransaction error: %v", err)
	}
	prov := &refundAdapter{cancelStatus: adapter.CancelStatusCanceled}
	registry := adapter.NewRegistry()
	registry.Register(prov)
	svc := NewPaymentService(txStore, registry, nil, &config.Config{RecordRetentionDays: 7})

	callCtx := metadata.NewIncomingContext(ctx, metadata.Pairs("x-auth-user-id", "user-1"))
	resp, err := svc.CancelSelectedProviderForExistingTransaction(callCtx, tx.ID, "change method")
	if err != nil {
		t.Fatalf("CancelSelectedProviderForExistingTransaction error: %v", err)
	}
	if !resp.Success || prov.cancelCalls != 1 {
		t.Fatalf("unexpected response/cancel calls: resp=%+v calls=%d", resp, prov.cancelCalls)
	}
	got, err := txStore.FindByID(ctx, tx.ID)
	if err != nil {
		t.Fatalf("FindByID error: %v", err)
	}
	if got.Status != model.StatusPending || got.ProviderTxID != "" || got.PaymentURL != "" || got.Provider != "" {
		t.Fatalf("provider state was not cleared while preserving pending status: %+v", got)
	}
}

func TestCancelSelectedProviderAlreadyPaidDoesNotClearProvider(t *testing.T) {
	ctx := context.Background()
	txStore := memstore.New()
	tx := &model.Transaction{
		ID:            "txn-paid-provider",
		ClientOrderID: "order-paid-provider",
		UserID:        "user-1",
		Namespace:     "ns",
		Status:        model.StatusPending,
		Provider:      "refund_stub",
		ProviderTxID:  "provider-tx-1",
		PaymentURL:    "https://pay.example.test/1",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(time.Minute),
	}
	if err := txStore.CreateTransaction(ctx, tx); err != nil {
		t.Fatalf("CreateTransaction error: %v", err)
	}
	prov := &refundAdapter{cancelStatus: adapter.CancelStatusAlreadyPaid}
	registry := adapter.NewRegistry()
	registry.Register(prov)
	svc := NewPaymentService(txStore, registry, nil, &config.Config{RecordRetentionDays: 7})

	callCtx := metadata.NewIncomingContext(ctx, metadata.Pairs("x-auth-user-id", "user-1"))
	_, err := svc.CancelSelectedProviderForExistingTransaction(callCtx, tx.ID, "change method")
	if err == nil {
		t.Fatal("expected failed precondition")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Fatalf("code = %s, want FailedPrecondition", st.Code())
	}
	got, err := txStore.FindByID(ctx, tx.ID)
	if err != nil {
		t.Fatalf("FindByID error: %v", err)
	}
	if got.ProviderTxID == "" || got.PaymentURL == "" {
		t.Fatalf("provider state should remain selected: %+v", got)
	}
}
