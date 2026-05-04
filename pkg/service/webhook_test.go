package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/config"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/fulfillment"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
	memstore "github.com/accelbyte/extend-regional-payment-gateway/internal/store/memory"
	pb "github.com/accelbyte/extend-regional-payment-gateway/pkg/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubAdapter is a minimal PaymentProvider that skips signature validation and
// always reports the webhook as a successful payment for a hard-coded order ID.
type stubAdapter struct {
	providerName    string
	internalOrderID string
	providerTxID    string
	status          adapter.PaymentStatus
	rawStatus       string
}

func (s *stubAdapter) Info() adapter.ProviderInfo {
	return adapter.ProviderInfo{ID: s.providerName, DisplayName: s.providerName}
}
func (s *stubAdapter) ValidatePaymentInit(adapter.PaymentInitRequest) error { return nil }
func (s *stubAdapter) ValidateWebhookSignature(_ context.Context, _ []byte, _ map[string]string) error {
	return nil
}
func (s *stubAdapter) HandleWebhook(_ context.Context, _ []byte, _ map[string]string) (*adapter.PaymentResult, error) {
	status := s.status
	if status == "" {
		status = adapter.PaymentStatusSuccess
	}
	rawStatus := s.rawStatus
	if rawStatus == "" {
		rawStatus = "00"
	}
	return &adapter.PaymentResult{
		InternalOrderID:       s.internalOrderID,
		ProviderTransactionID: s.providerTxID,
		Status:                status,
		RawProviderStatus:     rawStatus,
	}, nil
}
func (s *stubAdapter) CreatePaymentIntent(_ context.Context, _ adapter.PaymentInitRequest) (*adapter.PaymentIntent, error) {
	return nil, errors.New("not implemented")
}
func (s *stubAdapter) GetPaymentStatus(_ context.Context, _ string) (*adapter.ProviderPaymentStatus, error) {
	return nil, adapter.ErrNotSupported
}
func (s *stubAdapter) SyncTransactionStatus(context.Context, *model.Transaction) (*adapter.ProviderSyncResult, error) {
	return &adapter.ProviderSyncResult{PaymentStatus: adapter.SyncPaymentStatusUnsupported, RefundStatus: adapter.SyncRefundStatusUnsupported}, nil
}
func (s *stubAdapter) RefundPayment(_ context.Context, _, _ string, _ int64, _ string) error {
	return errors.New("not implemented")
}
func (s *stubAdapter) CancelPayment(context.Context, *model.Transaction, string) (*adapter.CancelResult, error) {
	return &adapter.CancelResult{Status: adapter.CancelStatusUnsupported}, nil
}
func (s *stubAdapter) ValidateCredentials(_ context.Context) error { return nil }

// failingFulfiller always returns an error from FulfillUserItem.
type failingFulfiller struct{ err error }

func (f *failingFulfiller) FulfillUserItem(_ context.Context, _, _, _ string, _ int32) error {
	return f.err
}

// okFulfiller always succeeds.
type okFulfiller struct{}

func (o *okFulfiller) FulfillUserItem(_ context.Context, _, _, _ string, _ int32) error { return nil }

func newTestSvc(txStore *memstore.Store, fulfiller ItemFulfiller, stub *stubAdapter) *WebhookService {
	reg := adapter.NewRegistry()
	reg.Register(stub)
	return &WebhookService{
		txStore:   txStore,
		registry:  reg,
		fulfiller: fulfiller,
		notifier:  fulfillment.NewNotifier("test-ns"),
		cfg:       &config.Config{RecordRetentionDays: 90},
	}
}

func pendingTx(id string) *model.Transaction {
	return &model.Transaction{
		ID:            id,
		UserID:        "user-1",
		ItemID:        "item-1",
		ClientOrderID: "co-" + id,
		Status:        model.StatusPending,
		Amount:        10000,
		CurrencyCode:  "IDR",
		Quantity:      1,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
}

// TestWebhook_FulfillmentError_ResetsToP checks that when FulfillUserItem fails:
//   - the webhook returns an Internal gRPC error
//   - the transaction is reset back to PENDING so the next retry can reclaim it
func TestWebhook_FulfillmentError_ResetsToPending(t *testing.T) {
	const txID = "tx-001"
	txStore := memstore.New()
	require.NoError(t, txStore.CreateTransaction(context.Background(), pendingTx(txID)))

	stub := &stubAdapter{providerName: "stub", internalOrderID: txID, providerTxID: "prov-001"}
	svc := newTestSvc(txStore, &failingFulfiller{err: errors.New("AGS down")}, stub)

	_, err := svc.HandleWebhook(context.Background(), &pb.WebhookRequest{
		ProviderId: "stub",
		RawPayload: []byte(`{}`),
	})

	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.Internal, st.Code())

	tx, fetchErr := txStore.FindByID(context.Background(), txID)
	require.NoError(t, fetchErr)
	assert.Equal(t, model.StatusPending, tx.Status, "transaction must be reset to PENDING for retry")
}

// TestWebhook_FulfillmentSuccess_Fulfills checks the happy path: transaction ends FULFILLED.
func TestWebhook_FulfillmentSuccess_Fulfills(t *testing.T) {
	const txID = "tx-002"
	txStore := memstore.New()
	require.NoError(t, txStore.CreateTransaction(context.Background(), pendingTx(txID)))

	stub := &stubAdapter{providerName: "stub", internalOrderID: txID, providerTxID: "prov-002"}
	svc := newTestSvc(txStore, &okFulfiller{}, stub)

	resp, err := svc.HandleWebhook(context.Background(), &pb.WebhookRequest{
		ProviderId: "stub",
		RawPayload: []byte(`{}`),
	})

	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Message)

	tx, fetchErr := txStore.FindByID(context.Background(), txID)
	require.NoError(t, fetchErr)
	assert.Equal(t, model.StatusFulfilled, tx.Status)
}

// TestWebhook_FulfillmentError_RetrySucceeds checks that after a fulfillment failure,
// the next webhook call successfully claims and fulfills the transaction.
func TestWebhook_FulfillmentError_RetrySucceeds(t *testing.T) {
	const txID = "tx-003"
	txStore := memstore.New()
	require.NoError(t, txStore.CreateTransaction(context.Background(), pendingTx(txID)))

	stub := &stubAdapter{providerName: "stub", internalOrderID: txID, providerTxID: "prov-003"}

	// First call: fulfillment fails → returns error → resets to PENDING
	svc := newTestSvc(txStore, &failingFulfiller{err: errors.New("AGS down")}, stub)
	_, err := svc.HandleWebhook(context.Background(), &pb.WebhookRequest{
		ProviderId: "stub",
		RawPayload: []byte(`{}`),
	})
	require.Error(t, err)

	// Second call: fulfillment succeeds → FULFILLED
	svc.fulfiller = &okFulfiller{}
	resp, err := svc.HandleWebhook(context.Background(), &pb.WebhookRequest{
		ProviderId: "stub",
		RawPayload: []byte(`{}`),
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Message)

	tx, _ := txStore.FindByID(context.Background(), txID)
	assert.Equal(t, model.StatusFulfilled, tx.Status)
}

func TestWebhook_RefundedStatusIsIgnoredByPaymentWebhook(t *testing.T) {
	const txID = "tx-refund-webhook-ignored"
	txStore := memstore.New()
	require.NoError(t, txStore.CreateTransaction(context.Background(), pendingTx(txID)))

	stub := &stubAdapter{
		providerName:    "stub",
		internalOrderID: txID,
		providerTxID:    "prov-refund-ignored",
		status:          adapter.PaymentStatusRefunded,
		rawStatus:       "refunded",
	}
	svc := newTestSvc(txStore, &okFulfiller{}, stub)

	resp, err := svc.HandleWebhook(context.Background(), &pb.WebhookRequest{
		ProviderId: "stub",
		RawPayload: []byte(`{}`),
	})

	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Message)
	tx, fetchErr := txStore.FindByID(context.Background(), txID)
	require.NoError(t, fetchErr)
	assert.Equal(t, model.StatusPending, tx.Status)
	assert.Nil(t, tx.Refund)
}
