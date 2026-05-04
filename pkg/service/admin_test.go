package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/config"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
	memstore "github.com/accelbyte/extend-regional-payment-gateway/internal/store/memory"
	pb "github.com/accelbyte/extend-regional-payment-gateway/pkg/pb"
)

type refundAdapter struct {
	refundCalls        int
	refundErr          error
	amount             int64
	syncPaymentStatus  adapter.SyncPaymentStatus
	syncRefundStatus   adapter.SyncRefundStatus
	rawPaymentStatus   string
	rawRefundStatus    string
	refundAmount       int64
	refundCurrencyCode string
	syncProviderTxID   string
	syncErr            error
	cancelStatus       adapter.CancelStatus
	cancelErr          error
	cancelCalls        int
}

func (r *refundAdapter) Info() adapter.ProviderInfo {
	return adapter.ProviderInfo{ID: "refund_stub", DisplayName: "Refund Stub"}
}
func (r *refundAdapter) ValidatePaymentInit(adapter.PaymentInitRequest) error { return nil }
func (r *refundAdapter) CreatePaymentIntent(context.Context, adapter.PaymentInitRequest) (*adapter.PaymentIntent, error) {
	return nil, errors.New("not implemented")
}
func (r *refundAdapter) GetPaymentStatus(context.Context, string) (*adapter.ProviderPaymentStatus, error) {
	return nil, adapter.ErrNotSupported
}
func (r *refundAdapter) ValidateWebhookSignature(context.Context, []byte, map[string]string) error {
	return nil
}
func (r *refundAdapter) HandleWebhook(context.Context, []byte, map[string]string) (*adapter.PaymentResult, error) {
	return nil, errors.New("not implemented")
}
func (r *refundAdapter) RefundPayment(_ context.Context, _ string, _ string, amount int64, _ string) error {
	r.refundCalls++
	r.amount = amount
	return r.refundErr
}
func (r *refundAdapter) ValidateCredentials(context.Context) error { return nil }
func (r *refundAdapter) CancelPayment(context.Context, *model.Transaction, string) (*adapter.CancelResult, error) {
	r.cancelCalls++
	if r.cancelErr != nil {
		return nil, r.cancelErr
	}
	status := r.cancelStatus
	if status == "" {
		status = adapter.CancelStatusCanceled
	}
	return &adapter.CancelResult{Status: status, ProviderStatus: string(status), ProviderTxID: "provider-tx-1"}, nil
}
func (r *refundAdapter) SyncTransactionStatus(context.Context, *model.Transaction) (*adapter.ProviderSyncResult, error) {
	if r.syncErr != nil {
		return nil, r.syncErr
	}
	paymentStatus := r.syncPaymentStatus
	refundStatus := r.syncRefundStatus
	if paymentStatus == "" {
		paymentStatus = adapter.SyncPaymentStatusUnsupported
	}
	if refundStatus == "" {
		refundStatus = adapter.SyncRefundStatusUnsupported
	}
	providerTxID := r.syncProviderTxID
	if providerTxID == "" {
		providerTxID = "provider-tx-1"
	}
	return &adapter.ProviderSyncResult{
		ProviderTxID:       providerTxID,
		PaymentStatus:      paymentStatus,
		RefundStatus:       refundStatus,
		RawPaymentStatus:   r.rawPaymentStatus,
		RawRefundStatus:    r.rawRefundStatus,
		RefundAmount:       r.refundAmount,
		RefundCurrencyCode: r.refundCurrencyCode,
		Message:            "sync result",
	}, nil
}

type syncFulfillment struct {
	fulfillCalls int
	reverseCalls int
	fulfillErr   error
	reverseErr   error
}

func (s *syncFulfillment) FulfillUserItem(context.Context, string, string, string, int32) error {
	s.fulfillCalls++
	return s.fulfillErr
}

func (s *syncFulfillment) ReverseFulfillment(context.Context, *model.Transaction) error {
	s.reverseCalls++
	return s.reverseErr
}

func TestAdminRefund_HappyPathRefundsProviderAndReversesAGS(t *testing.T) {
	txStore, txID := newFulfilledSyncTestStore(t, "txn-refund-1")
	prov := &refundAdapter{}
	fulfillment := &syncFulfillment{}
	svc := newRefundTestService(txStore, prov, fulfillment)

	resp, err := svc.Refund(context.Background(), &pb.RefundRequest{TransactionId: txID, Reason: "test refund"})

	require.NoError(t, err)
	assert.True(t, resp.Success)
	assert.Equal(t, 1, prov.refundCalls)
	assert.Equal(t, int64(10000), prov.amount)
	assert.Equal(t, 1, fulfillment.reverseCalls)

	tx, fetchErr := txStore.FindByID(context.Background(), txID)
	require.NoError(t, fetchErr)
	require.NotNil(t, tx.Refund)
	assert.Equal(t, model.RefundStatusRefunded, tx.Refund.Status)
	assert.True(t, tx.Refund.ProviderRefunded)
}

func TestAdminRefund_ProviderFailureDoesNotReverseAGS(t *testing.T) {
	txStore, txID := newFulfilledSyncTestStore(t, "txn-refund-1")
	prov := &refundAdapter{refundErr: errors.New("provider unavailable")}
	fulfillment := &syncFulfillment{}
	svc := newRefundTestService(txStore, prov, fulfillment)

	_, err := svc.Refund(context.Background(), &pb.RefundRequest{TransactionId: txID, Reason: "test refund"})

	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.Internal, st.Code())
	assert.Equal(t, 1, prov.refundCalls)
	assert.Equal(t, 0, fulfillment.reverseCalls)

	tx, fetchErr := txStore.FindByID(context.Background(), txID)
	require.NoError(t, fetchErr)
	require.NotNil(t, tx.Refund)
	assert.Equal(t, model.RefundStatusRefundFailed, tx.Refund.Status)
	assert.False(t, tx.Refund.ProviderRefunded)
	assert.Contains(t, tx.Refund.FailureReason, "provider refund failed")
}

func TestAdminRefund_NamespaceMismatchDoesNotClaimRefund(t *testing.T) {
	txStore, txID := newFulfilledSyncTestStore(t, "txn-refund-namespace")
	prov := &refundAdapter{}
	fulfillment := &syncFulfillment{}
	svc := newRefundTestService(txStore, prov, fulfillment)

	_, err := svc.Refund(context.Background(), &pb.RefundRequest{Namespace: "other-ns", TransactionId: txID, Reason: "wrong namespace"})

	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.NotFound, st.Code())
	assert.Equal(t, 0, prov.refundCalls)
	assert.Equal(t, 0, fulfillment.reverseCalls)
	tx, fetchErr := txStore.FindByID(context.Background(), txID)
	require.NoError(t, fetchErr)
	assert.Nil(t, tx.Refund)
}

func TestAdminGetTransactionDetail_NamespaceMismatchNotFound(t *testing.T) {
	txStore, txID := newFulfilledSyncTestStore(t, "txn-detail-namespace")
	svc := newRefundTestService(txStore, &refundAdapter{}, &syncFulfillment{})

	_, err := svc.GetTransactionDetail(context.Background(), &pb.GetTransactionRequest{Namespace: "other-ns", TransactionId: txID})

	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.NotFound, st.Code())
}

func TestAdminRefund_AGSReversalFailureCanRetryWithoutSecondProviderRefund(t *testing.T) {
	txStore, txID := newFulfilledSyncTestStore(t, "txn-refund-1")
	prov := &refundAdapter{}
	fulfillment := &syncFulfillment{reverseErr: errors.New("AGS unavailable")}
	svc := newRefundTestService(txStore, prov, fulfillment)

	_, err := svc.Refund(context.Background(), &pb.RefundRequest{TransactionId: txID, Reason: "test refund"})

	require.Error(t, err)
	assert.Equal(t, 1, prov.refundCalls)
	assert.Equal(t, 1, fulfillment.reverseCalls)
	tx, fetchErr := txStore.FindByID(context.Background(), txID)
	require.NoError(t, fetchErr)
	require.NotNil(t, tx.Refund)
	assert.Equal(t, model.RefundStatusRefundFailed, tx.Refund.Status)
	assert.True(t, tx.Refund.ProviderRefunded)
	assert.Contains(t, tx.Refund.FailureReason, "AGS reversal failed after provider refund")

	fulfillment.reverseErr = nil
	resp, retryErr := svc.Refund(context.Background(), &pb.RefundRequest{TransactionId: txID, Reason: "retry"})

	require.NoError(t, retryErr)
	assert.True(t, resp.Success)
	assert.Equal(t, 1, prov.refundCalls, "provider refund must not be issued twice")
	assert.Equal(t, 2, fulfillment.reverseCalls)
	tx, fetchErr = txStore.FindByID(context.Background(), txID)
	require.NoError(t, fetchErr)
	assert.Equal(t, model.RefundStatusRefunded, tx.Refund.Status)
}

func TestAdminSyncTransaction_PendingProviderPaidFulfillsAGS(t *testing.T) {
	txStore, txID := newPendingSyncTestStore(t, "txn-sync-paid")
	prov := &refundAdapter{
		syncPaymentStatus: adapter.SyncPaymentStatusPaid,
		syncRefundStatus:  adapter.SyncRefundStatusNone,
		rawPaymentStatus:  "captured",
	}
	fulfillment := &syncFulfillment{}
	svc := newRefundTestService(txStore, prov, fulfillment)

	resp, err := svc.SyncTransaction(context.Background(), &pb.SyncTransactionRequest{Namespace: "test-ns", TransactionId: txID})

	require.NoError(t, err)
	require.NotNil(t, resp.Result)
	assert.Equal(t, syncOutcomeFulfilled, resp.Result.Outcome)
	assert.Equal(t, string(adapter.SyncPaymentStatusPaid), resp.Result.PaymentStatus)
	assert.Equal(t, 1, fulfillment.fulfillCalls)

	tx, fetchErr := txStore.FindByID(context.Background(), txID)
	require.NoError(t, fetchErr)
	assert.Equal(t, model.StatusFulfilled, tx.Status)
	assert.Equal(t, "captured", tx.ProviderStatus)
}

func TestAdminSyncTransaction_DuplicatePaidSyncDoesNotFulfillAgain(t *testing.T) {
	txStore, txID := newPendingSyncTestStore(t, "txn-sync-paid")
	prov := &refundAdapter{
		syncPaymentStatus: adapter.SyncPaymentStatusPaid,
		syncRefundStatus:  adapter.SyncRefundStatusNone,
	}
	fulfillment := &syncFulfillment{}
	svc := newRefundTestService(txStore, prov, fulfillment)

	_, err := svc.SyncTransaction(context.Background(), &pb.SyncTransactionRequest{Namespace: "test-ns", TransactionId: txID})
	require.NoError(t, err)

	resp, err := svc.SyncTransaction(context.Background(), &pb.SyncTransactionRequest{Namespace: "test-ns", TransactionId: txID})

	require.NoError(t, err)
	assert.Equal(t, syncOutcomeUnchanged, resp.Result.Outcome)
	assert.Equal(t, 1, fulfillment.fulfillCalls)
}

func TestAdminSyncTransaction_PendingProviderFailedMarksFailed(t *testing.T) {
	txStore, txID := newPendingSyncTestStore(t, "txn-sync-failed")
	prov := &refundAdapter{
		syncPaymentStatus: adapter.SyncPaymentStatusFailed,
		syncRefundStatus:  adapter.SyncRefundStatusNone,
		rawPaymentStatus:  "expired",
	}
	fulfillment := &syncFulfillment{}
	svc := newRefundTestService(txStore, prov, fulfillment)

	resp, err := svc.SyncTransaction(context.Background(), &pb.SyncTransactionRequest{Namespace: "test-ns", TransactionId: txID})

	require.NoError(t, err)
	assert.Equal(t, syncOutcomeFailed, resp.Result.Outcome)
	assert.Equal(t, 0, fulfillment.fulfillCalls)
	tx, fetchErr := txStore.FindByID(context.Background(), txID)
	require.NoError(t, fetchErr)
	assert.Equal(t, model.StatusFailed, tx.Status)
	assert.Equal(t, "expired", tx.FailureReason)
}

func TestAdminSyncTransaction_ProviderRefundedReversesAGS(t *testing.T) {
	txStore, txID := newFulfilledSyncTestStore(t, "txn-sync-refund")
	prov := &refundAdapter{
		syncPaymentStatus:  adapter.SyncPaymentStatusPaid,
		syncRefundStatus:   adapter.SyncRefundStatusRefunded,
		rawRefundStatus:    "refunded",
		refundAmount:       10000,
		refundCurrencyCode: "IDR",
	}
	fulfillment := &syncFulfillment{}
	svc := newRefundTestService(txStore, prov, fulfillment)

	resp, err := svc.SyncTransaction(context.Background(), &pb.SyncTransactionRequest{Namespace: "test-ns", TransactionId: txID})

	require.NoError(t, err)
	require.NotNil(t, resp.Result)
	assert.Equal(t, syncOutcomeRefunded, resp.Result.Outcome)
	assert.Equal(t, model.RefundStatusRefunded, resp.Result.RefundStatus)
	assert.Equal(t, int64(10000), resp.Result.RefundAmount)
	assert.Equal(t, 1, fulfillment.reverseCalls)

	tx, fetchErr := txStore.FindByID(context.Background(), txID)
	require.NoError(t, fetchErr)
	require.NotNil(t, tx.Refund)
	assert.Equal(t, model.RefundStatusRefunded, tx.Refund.Status)
	assert.True(t, tx.Refund.ProviderRefunded)
}

func TestAdminSyncTransaction_DuplicateRefundDoesNotReverseAgain(t *testing.T) {
	txStore, txID := newFulfilledSyncTestStore(t, "txn-sync-refund")
	prov := &refundAdapter{
		syncPaymentStatus: adapter.SyncPaymentStatusPaid,
		syncRefundStatus:  adapter.SyncRefundStatusRefunded,
	}
	fulfillment := &syncFulfillment{}
	svc := newRefundTestService(txStore, prov, fulfillment)

	_, err := svc.SyncTransaction(context.Background(), &pb.SyncTransactionRequest{Namespace: "test-ns", TransactionId: txID})
	require.NoError(t, err)

	resp, err := svc.SyncTransaction(context.Background(), &pb.SyncTransactionRequest{Namespace: "test-ns", TransactionId: txID})

	require.NoError(t, err)
	assert.Equal(t, syncOutcomeUnchanged, resp.Result.Outcome)
	assert.Equal(t, model.RefundStatusRefunded, resp.Result.RefundStatus)
	assert.Equal(t, 1, fulfillment.reverseCalls)
}

func TestAdminSyncTransaction_PartialRefundReportsOnly(t *testing.T) {
	txStore, txID := newFulfilledSyncTestStore(t, "txn-sync-partial")
	prov := &refundAdapter{
		syncPaymentStatus:  adapter.SyncPaymentStatusPaid,
		syncRefundStatus:   adapter.SyncRefundStatusPartialRefunded,
		refundAmount:       3000,
		refundCurrencyCode: "IDR",
	}
	fulfillment := &syncFulfillment{}
	svc := newRefundTestService(txStore, prov, fulfillment)

	resp, err := svc.SyncTransaction(context.Background(), &pb.SyncTransactionRequest{Namespace: "test-ns", TransactionId: txID})

	require.NoError(t, err)
	assert.Equal(t, syncOutcomePartialRefundUnchanged, resp.Result.Outcome)
	assert.Equal(t, int64(3000), resp.Result.RefundAmount)
	assert.Equal(t, 0, fulfillment.reverseCalls)
	tx, fetchErr := txStore.FindByID(context.Background(), txID)
	require.NoError(t, fetchErr)
	assert.Nil(t, tx.Refund)
}

func TestAdminSyncTransaction_UnsupportedDoesNotMutate(t *testing.T) {
	txStore, txID := newFulfilledSyncTestStore(t, "txn-sync-unsupported")
	prov := &refundAdapter{
		syncPaymentStatus: adapter.SyncPaymentStatusUnsupported,
		syncRefundStatus:  adapter.SyncRefundStatusUnsupported,
	}
	fulfillment := &syncFulfillment{}
	svc := newRefundTestService(txStore, prov, fulfillment)

	resp, err := svc.SyncTransaction(context.Background(), &pb.SyncTransactionRequest{Namespace: "test-ns", TransactionId: txID})

	require.NoError(t, err)
	assert.Equal(t, syncOutcomeUnsupported, resp.Result.Outcome)
	assert.Equal(t, 0, fulfillment.reverseCalls)
	tx, fetchErr := txStore.FindByID(context.Background(), txID)
	require.NoError(t, fetchErr)
	assert.Nil(t, tx.Refund)
}

func TestAdminSyncTransaction_AGSFailureMarksRefundFailed(t *testing.T) {
	txStore, txID := newFulfilledSyncTestStore(t, "txn-sync-refund-fail")
	prov := &refundAdapter{
		syncPaymentStatus: adapter.SyncPaymentStatusPaid,
		syncRefundStatus:  adapter.SyncRefundStatusRefunded,
	}
	fulfillment := &syncFulfillment{reverseErr: errors.New("AGS unavailable")}
	svc := newRefundTestService(txStore, prov, fulfillment)

	resp, err := svc.SyncTransaction(context.Background(), &pb.SyncTransactionRequest{Namespace: "test-ns", TransactionId: txID})

	require.NoError(t, err)
	assert.Equal(t, syncOutcomeSyncFailed, resp.Result.Outcome)
	assert.Equal(t, model.RefundStatusRefundFailed, resp.Result.RefundStatus)
	assert.Equal(t, 1, fulfillment.reverseCalls)
	tx, fetchErr := txStore.FindByID(context.Background(), txID)
	require.NoError(t, fetchErr)
	require.NotNil(t, tx.Refund)
	assert.True(t, tx.Refund.ProviderRefunded)
	assert.Equal(t, model.RefundStatusRefundFailed, tx.Refund.Status)
}

func TestAdminSyncTransactions_BatchContinuesAfterProviderQueryFailure(t *testing.T) {
	txStore, refundedID := newFulfilledSyncTestStore(t, "txn-sync-refunded")
	failedID := createSyncTransaction(t, txStore, "txn-sync-provider-fails", "failing_stub", model.StatusFulfilled)

	refundedProvider := &refundAdapter{
		syncPaymentStatus: adapter.SyncPaymentStatusPaid,
		syncRefundStatus:  adapter.SyncRefundStatusRefunded,
	}
	failingProvider := &refundAdapter{
		syncErr: errors.New("provider unavailable"),
	}
	reg := adapter.NewRegistry()
	reg.Register(refundedProvider)
	reg.Register(namedRefundAdapter{name: "failing_stub", refundAdapter: failingProvider})
	fulfillment := &syncFulfillment{}
	svc := NewAdminService(txStore, reg, fulfillment, &config.Config{})

	resp, err := svc.SyncTransactions(context.Background(), &pb.SyncTransactionsRequest{Namespace: "test-ns", PageSize: 10})

	require.NoError(t, err)
	require.Len(t, resp.Results, 2)
	outcomes := map[string]string{}
	for _, result := range resp.Results {
		outcomes[result.TransactionId] = result.Outcome
	}
	assert.Equal(t, syncOutcomeRefunded, outcomes[refundedID])
	assert.Equal(t, syncOutcomeSyncFailed, outcomes[failedID])
	assert.Equal(t, 1, fulfillment.reverseCalls)
}

func TestAdminSyncTransactions_FiltersByNamespaceProviderAndStatus(t *testing.T) {
	txStore := memstore.New()
	includedID := createSyncTransaction(t, txStore, "txn-included", "refund_stub", model.StatusPending)
	createSyncTransaction(t, txStore, "txn-other-provider", "other_stub", model.StatusPending)
	createSyncTransaction(t, txStore, "txn-other-status", "refund_stub", model.StatusFulfilled)
	createSyncTransactionInNamespace(t, txStore, "txn-other-namespace", "other-ns", "refund_stub", model.StatusPending)

	prov := &refundAdapter{
		syncPaymentStatus: adapter.SyncPaymentStatusPaid,
		syncRefundStatus:  adapter.SyncRefundStatusNone,
	}
	reg := adapter.NewRegistry()
	reg.Register(prov)
	reg.Register(namedRefundAdapter{name: "other_stub", refundAdapter: prov})
	fulfillment := &syncFulfillment{}
	svc := NewAdminService(txStore, reg, fulfillment, &config.Config{})

	resp, err := svc.SyncTransactions(context.Background(), &pb.SyncTransactionsRequest{
		Namespace:    "test-ns",
		ProviderId:   "refund_stub",
		StatusFilter: pb.TransactionStatus_PENDING,
		PageSize:     1000,
	})

	require.NoError(t, err)
	require.Len(t, resp.Results, 1)
	assert.Equal(t, includedID, resp.Results[0].TransactionId)
	assert.Equal(t, syncOutcomeFulfilled, resp.Results[0].Outcome)
	assert.Equal(t, 1, fulfillment.fulfillCalls)
}

type namedRefundAdapter struct {
	name string
	*refundAdapter
}

func (n namedRefundAdapter) Info() adapter.ProviderInfo {
	return adapter.ProviderInfo{ID: n.name, DisplayName: n.name}
}

func newRefundTestService(txStore *memstore.Store, prov *refundAdapter, fulfillment *syncFulfillment) *AdminService {
	reg := adapter.NewRegistry()
	reg.Register(prov)
	return NewAdminService(txStore, reg, fulfillment, &config.Config{})
}

func newPendingSyncTestStore(t *testing.T, txID string) (*memstore.Store, string) {
	t.Helper()
	txStore := memstore.New()
	createSyncTransaction(t, txStore, txID, "refund_stub", model.StatusPending)
	return txStore, txID
}

func newFulfilledSyncTestStore(t *testing.T, txID string) (*memstore.Store, string) {
	t.Helper()
	txStore := memstore.New()
	createSyncTransaction(t, txStore, txID, "refund_stub", model.StatusFulfilled)
	return txStore, txID
}

func createSyncTransaction(t *testing.T, txStore *memstore.Store, txID, provider, txStatus string) string {
	t.Helper()
	return createSyncTransactionInNamespace(t, txStore, txID, "test-ns", provider, txStatus)
}

func createSyncTransactionInNamespace(t *testing.T, txStore *memstore.Store, txID, namespace, provider, txStatus string) string {
	t.Helper()
	now := time.Now().UTC()
	err := txStore.CreateTransaction(context.Background(), &model.Transaction{
		ID:            txID,
		ClientOrderID: "order-" + txID,
		UserID:        "user-1",
		Namespace:     namespace,
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
