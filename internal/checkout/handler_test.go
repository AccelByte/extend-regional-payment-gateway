package checkout

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
	pb "github.com/accelbyte/extend-regional-payment-gateway/pkg/pb"
)

type stubPaymentSvc struct {
	tx                   *pb.TransactionResponse
	createErr            error
	cancelSelectedErr    error
	cancelSelectedCalled bool
}

func (s *stubPaymentSvc) CreatePaymentForExistingTransaction(context.Context, string, string, string) (*pb.CreatePaymentIntentResponse, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	return &pb.CreatePaymentIntentResponse{PaymentUrl: "https://example.test/pay"}, nil
}

func (s *stubPaymentSvc) CancelPaymentForExistingTransaction(context.Context, string, string) (*pb.CancelTransactionResponse, error) {
	return &pb.CancelTransactionResponse{Success: true}, nil
}

func (s *stubPaymentSvc) CancelSelectedProviderForExistingTransaction(context.Context, string, string) (*pb.CancelTransactionResponse, error) {
	s.cancelSelectedCalled = true
	if s.cancelSelectedErr != nil {
		return nil, s.cancelSelectedErr
	}
	return &pb.CancelTransactionResponse{Success: true}, nil
}

func (s *stubPaymentSvc) GetTransaction(context.Context, *pb.GetTransactionRequest) (*pb.TransactionResponse, error) {
	if s.tx != nil {
		return s.tx, nil
	}
	return &pb.TransactionResponse{Status: pb.TransactionStatus_PENDING}, nil
}

type stubProvider struct {
	name string
}

func (p stubProvider) Info() adapter.ProviderInfo {
	return adapter.ProviderInfo{ID: p.name, DisplayName: displayName(p.name)}
}

func (stubProvider) ValidatePaymentInit(adapter.PaymentInitRequest) error {
	return nil
}

func (stubProvider) CreatePaymentIntent(context.Context, adapter.PaymentInitRequest) (*adapter.PaymentIntent, error) {
	return nil, nil
}

func (stubProvider) GetPaymentStatus(context.Context, string) (*adapter.ProviderPaymentStatus, error) {
	return nil, adapter.ErrNotSupported
}

func (stubProvider) SyncTransactionStatus(context.Context, *model.Transaction) (*adapter.ProviderSyncResult, error) {
	return &adapter.ProviderSyncResult{PaymentStatus: adapter.SyncPaymentStatusUnsupported, RefundStatus: adapter.SyncRefundStatusUnsupported}, nil
}

func (stubProvider) ValidateWebhookSignature(context.Context, []byte, map[string]string) error {
	return nil
}

func (stubProvider) HandleWebhook(context.Context, []byte, map[string]string) (*adapter.PaymentResult, error) {
	return nil, nil
}

func (stubProvider) RefundPayment(context.Context, string, string, int64, string) error {
	return nil
}

func (stubProvider) CancelPayment(context.Context, *model.Transaction, string) (*adapter.CancelResult, error) {
	return &adapter.CancelResult{Status: adapter.CancelStatusUnsupported}, nil
}

func (stubProvider) ValidateCredentials(context.Context) error {
	return nil
}

func TestHandleCheckoutPageRendersOrderDetailsAndProviders(t *testing.T) {
	store := NewStore(context.Background())
	sessionID := store.Create(&Session{
		TransactionID: "txn-123",
		UserID:        "user-1",
		ItemName:      "Crystal Pack",
		ItemID:        "item-crystal-pack",
		Quantity:      2,
		UnitPrice:     10500,
		TotalPrice:    21000,
		CurrencyCode:  "IDR",
		ExpiresAt:     time.Now().Add(30 * time.Minute),
	})

	registry := adapter.NewRegistry()
	registry.Register(stubProvider{name: "provider_shopee_pay"})
	registry.Register(stubProvider{name: "provider_komoju"})

	handler := NewHandler(store, registry, &stubPaymentSvc{}, "/payment")
	req := httptest.NewRequest(http.MethodGet, "/payment/checkout/"+sessionID, nil)
	rec := httptest.NewRecorder()

	handler.HandleCheckoutPage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"AccelByte Checkout",
		"Crystal Pack",
		"Item ID: item-crystal-pack",
		"txn-123",
		"10.500 IDR",
		"21.000 IDR",
		"Choose payment method",
		"Komoju",
		"Shopee Pay",
		`action="/payment/checkout/` + sessionID + `/select"`,
		`action="/payment/checkout/` + sessionID + `/cancel"`,
		`name="provider" value="provider_komoju"`,
		`name="provider" value="provider_shopee_pay"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("rendered checkout page missing %q\nbody:\n%s", want, body)
		}
	}
}

func TestHandleProviderSelectDoesNotConsumeSession(t *testing.T) {
	store := NewStore(context.Background())
	sessionID := store.Create(&Session{
		TransactionID: "txn-123",
		UserID:        "user-1",
		ExpiresAt:     time.Now().Add(30 * time.Minute),
	})
	registry := adapter.NewRegistry()
	registry.Register(stubProvider{name: "provider_xendit"})
	handler := NewHandler(store, registry, &stubPaymentSvc{}, "/payment")

	req := httptest.NewRequest(http.MethodPost, "/payment/checkout/"+sessionID+"/select", strings.NewReader("provider=provider_xendit"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.HandleProviderSelect(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected redirect, got %d", rec.Code)
	}
	if _, ok := store.Get(sessionID); !ok {
		t.Fatal("expected provider selection to keep checkout session")
	}
}

func TestHandleProviderSelectAlreadySelectedRedirectsToCheckout(t *testing.T) {
	store := NewStore(context.Background())
	sessionID := store.Create(&Session{
		TransactionID: "txn-123",
		UserID:        "user-1",
		ExpiresAt:     time.Now().Add(30 * time.Minute),
	})
	registry := adapter.NewRegistry()
	registry.Register(stubProvider{name: "provider_xendit"})
	handler := NewHandler(store, registry, &stubPaymentSvc{createErr: errors.New("rpc error: code = FailedPrecondition desc = payment provider already selected")}, "/payment")

	req := httptest.NewRequest(http.MethodPost, "/payment/checkout/"+sessionID+"/select", strings.NewReader("provider=provider_xendit"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.HandleProviderSelect(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected redirect, got %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/payment/checkout/"+sessionID {
		t.Fatalf("Location = %q", got)
	}
}

func TestHandleProviderSelectGetRedirectsToCheckout(t *testing.T) {
	handler := NewHandler(NewStore(context.Background()), adapter.NewRegistry(), &stubPaymentSvc{}, "/payment")
	req := httptest.NewRequest(http.MethodGet, "/payment/checkout/session-1/select", nil)
	rec := httptest.NewRecorder()
	handler.HandleProviderSelect(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected redirect, got %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/payment/checkout/session-1" {
		t.Fatalf("Location = %q", got)
	}
}

func TestHandleCheckoutPageRendersSelectedProviderState(t *testing.T) {
	store := NewStore(context.Background())
	sessionID := store.Create(&Session{
		TransactionID: "txn-123",
		UserID:        "user-1",
		ItemID:        "item-1",
		ExpiresAt:     time.Now().Add(30 * time.Minute),
	})
	handler := NewHandler(store, adapter.NewRegistry(), &stubPaymentSvc{tx: &pb.TransactionResponse{
		Status:              pb.TransactionStatus_PENDING,
		ProviderId:          "provider_xendit",
		ProviderDisplayName: "Xendit",
		ProviderTxId:        "ps-123",
		PaymentUrl:          "https://xendit.test/pay/ps-123",
	}}, "/payment")

	req := httptest.NewRequest(http.MethodGet, "/payment/checkout/"+sessionID, nil)
	rec := httptest.NewRecorder()
	handler.HandleCheckoutPage(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		"Payment method selected",
		"Xendit is waiting for payment confirmation.",
		`action="https://xendit.test/pay/ps-123"`,
		`action="/payment/checkout/` + sessionID + `/cancel-selected-provider"`,
		"Cancel selected method",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("rendered selected-provider page missing %q\nbody:\n%s", want, body)
		}
	}
}

func TestHandleCancelSelectedProvider(t *testing.T) {
	store := NewStore(context.Background())
	sessionID := store.Create(&Session{
		TransactionID: "txn-123",
		UserID:        "user-1",
		ExpiresAt:     time.Now().Add(30 * time.Minute),
	})
	svc := &stubPaymentSvc{}
	handler := NewHandler(store, adapter.NewRegistry(), svc, "/payment")

	req := httptest.NewRequest(http.MethodPost, "/payment/checkout/"+sessionID+"/cancel-selected-provider", nil)
	rec := httptest.NewRecorder()
	handler.HandleCancelSelectedProvider(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected redirect, got %d", rec.Code)
	}
	if !svc.cancelSelectedCalled {
		t.Fatal("expected cancel selected provider service call")
	}
}

func TestFormatCurrencyAmount(t *testing.T) {
	tests := map[int64]string{
		0:        "0 USD",
		500:      "500 USD",
		10500:    "10.500 USD",
		12500000: "12.500.000 USD",
		-10500:   "-10.500 USD",
	}
	for amount, want := range tests {
		if got := formatCurrencyAmount(amount, "usd"); got != want {
			t.Fatalf("formatCurrencyAmount(%d) = %q, want %q", amount, got, want)
		}
	}
}
