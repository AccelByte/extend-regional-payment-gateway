package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/config"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
	memstore "github.com/accelbyte/extend-regional-payment-gateway/internal/store/memory"
	pb "github.com/accelbyte/extend-regional-payment-gateway/pkg/pb"
)

func TestPublicSyncMyTransaction_OwnPendingPaidFulfillsAGS(t *testing.T) {
	txStore := memstore.New()
	txID := createPublicSyncTransaction(t, txStore, "txn-public-paid", "user-1", "refund_stub", model.StatusPending)
	prov := &refundAdapter{
		syncPaymentStatus: adapter.SyncPaymentStatusPaid,
		syncRefundStatus:  adapter.SyncRefundStatusNone,
		rawPaymentStatus:  "captured",
	}
	fulfillment := &syncFulfillment{}
	svc := newPublicSyncTestService(txStore, prov, fulfillment, publicSyncTestConfig())

	resp, err := svc.SyncMyTransaction(publicSyncContext("user-1"), &pb.PublicSyncTransactionRequest{Namespace: "test-ns", TransactionId: txID})

	require.NoError(t, err)
	assert.Equal(t, syncOutcomeFulfilled, resp.Result.Outcome)
	assert.Equal(t, 1, fulfillment.fulfillCalls)
	tx, fetchErr := txStore.FindByID(context.Background(), txID)
	require.NoError(t, fetchErr)
	assert.Equal(t, model.StatusFulfilled, tx.Status)
}

func TestPublicSyncMyTransaction_RejectsOtherUsersTransaction(t *testing.T) {
	txStore := memstore.New()
	txID := createPublicSyncTransaction(t, txStore, "txn-public-other-user", "user-2", "refund_stub", model.StatusPending)
	prov := &refundAdapter{
		syncPaymentStatus: adapter.SyncPaymentStatusPaid,
		syncRefundStatus:  adapter.SyncRefundStatusNone,
	}
	fulfillment := &syncFulfillment{}
	svc := newPublicSyncTestService(txStore, prov, fulfillment, publicSyncTestConfig())

	_, err := svc.SyncMyTransaction(publicSyncContext("user-1"), &pb.PublicSyncTransactionRequest{Namespace: "test-ns", TransactionId: txID})

	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.NotFound, st.Code())
	assert.Equal(t, 0, fulfillment.fulfillCalls)
}

func TestPublicSyncMyTransaction_RejectsNamespaceMismatch(t *testing.T) {
	txStore := memstore.New()
	txID := createPublicSyncTransaction(t, txStore, "txn-public-other-ns", "user-1", "refund_stub", model.StatusPending)
	prov := &refundAdapter{
		syncPaymentStatus: adapter.SyncPaymentStatusPaid,
		syncRefundStatus:  adapter.SyncRefundStatusNone,
	}
	fulfillment := &syncFulfillment{}
	svc := newPublicSyncTestService(txStore, prov, fulfillment, publicSyncTestConfig())

	_, err := svc.SyncMyTransaction(publicSyncContext("user-1"), &pb.PublicSyncTransactionRequest{Namespace: "other-ns", TransactionId: txID})

	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.NotFound, st.Code())
	assert.Equal(t, 0, fulfillment.fulfillCalls)
}

func TestPublicSyncMyTransactions_FiltersCurrentUserProviderAndDefaultsPageSize(t *testing.T) {
	txStore := memstore.New()
	for i := 0; i < 12; i++ {
		createPublicSyncTransaction(t, txStore, "txn-public-user-1-"+string(rune('a'+i)), "user-1", "refund_stub", model.StatusPending)
	}
	createPublicSyncTransaction(t, txStore, "txn-public-user-2", "user-2", "refund_stub", model.StatusPending)
	createPublicSyncTransaction(t, txStore, "txn-public-other-provider", "user-1", "other_stub", model.StatusPending)
	prov := &refundAdapter{
		syncPaymentStatus: adapter.SyncPaymentStatusPaid,
		syncRefundStatus:  adapter.SyncRefundStatusNone,
	}
	reg := adapter.NewRegistry()
	reg.Register(prov)
	reg.Register(namedRefundAdapter{name: "other_stub", refundAdapter: prov})
	fulfillment := &syncFulfillment{}
	svc := NewPublicService(txStore, reg, fulfillment, publicSyncTestConfig())

	resp, err := svc.SyncMyTransactions(publicSyncContext("user-1"), &pb.PublicSyncTransactionsRequest{
		Namespace:  "test-ns",
		ProviderId: "refund_stub",
	})

	require.NoError(t, err)
	assert.Len(t, resp.Results, 10)
	assert.NotEmpty(t, resp.NextCursor)
	for _, result := range resp.Results {
		assert.Equal(t, "refund_stub", result.ProviderId)
	}
}

func TestPublicSyncMyTransactions_CapsPageSize(t *testing.T) {
	txStore := memstore.New()
	for i := 0; i < 25; i++ {
		createPublicSyncTransaction(t, txStore, "txn-public-cap-"+string(rune('a'+i)), "user-1", "refund_stub", model.StatusPending)
	}
	prov := &refundAdapter{
		syncPaymentStatus: adapter.SyncPaymentStatusPaid,
		syncRefundStatus:  adapter.SyncRefundStatusNone,
	}
	fulfillment := &syncFulfillment{}
	svc := newPublicSyncTestService(txStore, prov, fulfillment, publicSyncTestConfig())

	resp, err := svc.SyncMyTransactions(publicSyncContext("user-1"), &pb.PublicSyncTransactionsRequest{
		Namespace: "test-ns",
		PageSize:  100,
	})

	require.NoError(t, err)
	assert.Len(t, resp.Results, 20)
}

func TestPublicSyncMyTransaction_ProviderRefundedReversesAGS(t *testing.T) {
	txStore := memstore.New()
	txID := createPublicSyncTransaction(t, txStore, "txn-public-refund", "user-1", "refund_stub", model.StatusFulfilled)
	prov := &refundAdapter{
		syncPaymentStatus: adapter.SyncPaymentStatusPaid,
		syncRefundStatus:  adapter.SyncRefundStatusRefunded,
	}
	fulfillment := &syncFulfillment{}
	svc := newPublicSyncTestService(txStore, prov, fulfillment, publicSyncTestConfig())

	resp, err := svc.SyncMyTransaction(publicSyncContext("user-1"), &pb.PublicSyncTransactionRequest{Namespace: "test-ns", TransactionId: txID})

	require.NoError(t, err)
	assert.Equal(t, syncOutcomeRefunded, resp.Result.Outcome)
	assert.Equal(t, 1, fulfillment.reverseCalls)
	tx, fetchErr := txStore.FindByID(context.Background(), txID)
	require.NoError(t, fetchErr)
	require.NotNil(t, tx.Refund)
	assert.Equal(t, model.RefundStatusRefunded, tx.Refund.Status)
}

func TestPublicSyncMyTransaction_CooldownReturnsResourceExhausted(t *testing.T) {
	txStore := memstore.New()
	txID := createPublicSyncTransaction(t, txStore, "txn-public-cooldown", "user-1", "refund_stub", model.StatusFulfilled)
	prov := &refundAdapter{
		syncPaymentStatus: adapter.SyncPaymentStatusUnsupported,
		syncRefundStatus:  adapter.SyncRefundStatusUnsupported,
	}
	fulfillment := &syncFulfillment{}
	svc := newPublicSyncTestService(txStore, prov, fulfillment, &config.Config{PublicSyncCooldown: time.Minute})

	_, firstErr := svc.SyncMyTransaction(publicSyncContext("user-1"), &pb.PublicSyncTransactionRequest{Namespace: "test-ns", TransactionId: txID})
	require.NoError(t, firstErr)
	_, secondErr := svc.SyncMyTransaction(publicSyncContext("user-1"), &pb.PublicSyncTransactionRequest{Namespace: "test-ns", TransactionId: txID})

	require.Error(t, secondErr)
	st, _ := status.FromError(secondErr)
	assert.Equal(t, codes.ResourceExhausted, st.Code())
}

func TestCancelMyTransaction_NoProviderMarksCanceled(t *testing.T) {
	txStore := memstore.New()
	txID := createPublicSyncTransaction(t, txStore, "txn-public-cancel-local", "user-1", "", model.StatusPending)
	require.NoError(t, txStore.UpdateProviderTransactionID(context.Background(), txID, ""))

	svc := newPublicSyncTestService(txStore, &refundAdapter{}, &syncFulfillment{}, publicSyncTestConfig())
	resp, err := svc.CancelMyTransaction(publicSyncContext("user-1"), &pb.CancelTransactionRequest{
		Namespace:     "test-ns",
		TransactionId: txID,
		Reason:        "player closed checkout",
	})

	require.NoError(t, err)
	assert.True(t, resp.Success)
	assert.Equal(t, pb.TransactionStatus_CANCELED, resp.Transaction.Status)
	updated, fetchErr := txStore.FindByID(context.Background(), txID)
	require.NoError(t, fetchErr)
	assert.Equal(t, model.StatusCanceled, updated.Status)
}

func TestCancelMyTransaction_RejectsOtherUsersTransaction(t *testing.T) {
	txStore := memstore.New()
	txID := createPublicSyncTransaction(t, txStore, "txn-public-cancel-other", "user-2", "", model.StatusPending)
	svc := newPublicSyncTestService(txStore, &refundAdapter{}, &syncFulfillment{}, publicSyncTestConfig())

	_, err := svc.CancelMyTransaction(publicSyncContext("user-1"), &pb.CancelTransactionRequest{Namespace: "test-ns", TransactionId: txID})

	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.NotFound, st.Code())
}

func newPublicSyncTestService(txStore *memstore.Store, prov *refundAdapter, fulfillment *syncFulfillment, cfg *config.Config) *PublicService {
	reg := adapter.NewRegistry()
	reg.Register(prov)
	return NewPublicService(txStore, reg, fulfillment, cfg)
}

func publicSyncTestConfig() *config.Config {
	return &config.Config{PublicSyncCooldown: -1}
}

func publicSyncContext(userID string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-auth-user-id", userID))
}

func createPublicSyncTransaction(t *testing.T, txStore *memstore.Store, txID, userID, provider, txStatus string) string {
	t.Helper()
	now := time.Now().UTC()
	err := txStore.CreateTransaction(context.Background(), &model.Transaction{
		ID:            txID,
		ClientOrderID: "order-" + txID,
		UserID:        userID,
		Namespace:     "test-ns",
		ProviderID:    provider,
		ItemID:        "item-1",
		Quantity:      1,
		Amount:        10000,
		CurrencyCode:  "IDR",
		ProviderTxID:  "provider-tx-1",
		Status:        txStatus,
		CreatedAt:     now,
		UpdatedAt:     now,
		ExpiresAt:     now.Add(time.Hour),
	})
	require.NoError(t, err)
	return txID
}
