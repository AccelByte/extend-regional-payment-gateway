package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/config"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/store"
	pb "github.com/accelbyte/extend-regional-payment-gateway/pkg/pb"
)

type transactionReconciler struct {
	txStore  store.Store
	registry *adapter.Registry
	reverser FulfillmentReverser
	cfg      *config.Config
}

func newTransactionReconciler(txStore store.Store, registry *adapter.Registry, reverser FulfillmentReverser, cfg *config.Config) *transactionReconciler {
	return &transactionReconciler{
		txStore:  txStore,
		registry: registry,
		reverser: reverser,
		cfg:      cfg,
	}
}

func (r *transactionReconciler) syncTransaction(ctx context.Context, tx *model.Transaction) *pb.SyncTransactionResult {
	result := &pb.SyncTransactionResult{
		TransactionId: tx.ID,
		Provider:      resolveProviderName(tx),
		Outcome:       syncOutcomeUnchanged,
		Message:       "provider state did not require local changes",
	}
	if tx.ProviderTxID == "" {
		result.Outcome = syncOutcomeUnsupported
		result.Message = "missing provider transaction id"
		return result
	}

	prov, err := r.registry.Get(resolveProviderName(tx))
	if err != nil {
		result.Outcome = syncOutcomeSyncFailed
		result.Message = "unknown provider: " + resolveProviderName(tx)
		return result
	}
	syncer, ok := prov.(adapter.TransactionSyncer)
	if !ok {
		result.Outcome = syncOutcomeUnsupported
		result.Message = "provider does not support transaction sync"
		return result
	}

	providerResult, err := syncer.SyncTransactionStatus(ctx, tx)
	if err != nil {
		result.Outcome = syncOutcomeSyncFailed
		result.Message = "provider sync failed: " + err.Error()
		return result
	}
	if providerResult == nil {
		result.Outcome = syncOutcomeSyncFailed
		result.Message = "provider sync returned empty result"
		return result
	}

	result.ProviderTxId = providerResult.ProviderTxID
	if result.ProviderTxId == "" {
		result.ProviderTxId = tx.ProviderTxID
	}
	result.PaymentStatus = string(providerResult.PaymentStatus)
	result.RefundStatus = string(providerResult.RefundStatus)
	result.RawPaymentStatus = providerResult.RawPaymentStatus
	result.RawRefundStatus = providerResult.RawRefundStatus
	result.RefundAmount = providerResult.RefundAmount
	result.RefundCurrencyCode = providerResult.RefundCurrencyCode
	result.Message = providerResult.Message
	r.maybeUpdateProviderTxID(ctx, tx, result.ProviderTxId)

	if providerResult.RefundStatus == adapter.SyncRefundStatusRefunded {
		return r.syncProviderRefund(ctx, tx, result)
	}
	if providerResult.RefundStatus == adapter.SyncRefundStatusPartialRefunded {
		result.Outcome = syncOutcomePartialRefundUnchanged
		if result.Message == "" {
			result.Message = "provider reports partial refund; AGS reversal is not automatic"
		}
		return result
	}

	switch providerResult.PaymentStatus {
	case adapter.SyncPaymentStatusPaid:
		return r.syncProviderPaid(ctx, tx, result)
	case adapter.SyncPaymentStatusFailed:
		return r.syncProviderFailed(ctx, tx, result)
	case adapter.SyncPaymentStatusUnsupported:
		result.Outcome = syncOutcomeUnsupported
		if result.Message == "" {
			result.Message = "provider cannot prove transaction state"
		}
		return result
	case adapter.SyncPaymentStatusUnknown:
		result.Outcome = syncOutcomeUnsupported
		if result.Message == "" {
			result.Message = "provider transaction state is unknown"
		}
		return result
	default:
		result.Outcome = syncOutcomeUnchanged
		if result.Message == "" {
			result.Message = "provider state did not require local changes"
		}
		return result
	}
}

func (r *transactionReconciler) syncProviderPaid(ctx context.Context, tx *model.Transaction, result *pb.SyncTransactionResult) *pb.SyncTransactionResult {
	if tx.Status == model.StatusFulfilled {
		result.Outcome = syncOutcomeUnchanged
		result.Message = "transaction already fulfilled"
		return result
	}
	if tx.Status != model.StatusPending {
		result.Outcome = syncOutcomeUnchanged
		result.Message = "provider is paid but local transaction is not pending"
		return result
	}
	claimed, claimErr := r.txStore.AtomicClaimFulfilling(ctx, tx.ID, providerTxIDForClaim(tx.ProviderTxID, result.ProviderTxId))
	if claimErr == store.ErrNoDocuments {
		result.Outcome = syncOutcomeUnchanged
		result.Message = "transaction already handled concurrently"
		return result
	}
	if claimErr != nil {
		result.Outcome = syncOutcomeSyncFailed
		result.Message = "failed to claim fulfillment sync: " + claimErr.Error()
		return result
	}
	fulfiller, ok := r.reverser.(ItemFulfiller)
	if !ok {
		if resetErr := r.txStore.ResetToPending(ctx, claimed.ID); resetErr != nil {
			slog.Error("SyncTransaction: ResetToPending failed", "txn_id", claimed.ID, "error", resetErr)
		}
		result.Outcome = syncOutcomeSyncFailed
		result.Message = "fulfiller is not configured"
		return result
	}
	if fulfillErr := fulfiller.FulfillUserItem(ctx, claimed.UserID, claimed.ItemID, claimed.ID, claimed.Quantity); fulfillErr != nil {
		if resetErr := r.txStore.ResetToPending(ctx, claimed.ID); resetErr != nil {
			slog.Error("SyncTransaction: ResetToPending failed", "txn_id", claimed.ID, "error", resetErr)
		}
		result.Outcome = syncOutcomeSyncFailed
		result.Message = "AGS fulfillment failed: " + fulfillErr.Error()
		return result
	}
	deleteAt := time.Now().UTC().AddDate(0, 0, r.cfg.RecordRetentionDays)
	if commitErr := r.txStore.CommitFulfilled(ctx, claimed.ID, result.RawPaymentStatus, deleteAt); commitErr != nil {
		result.Outcome = syncOutcomeSyncFailed
		result.Message = "failed to commit fulfilled status: " + commitErr.Error()
		return result
	}
	result.Outcome = syncOutcomeFulfilled
	result.Message = "provider payment synced and AGS fulfillment completed"
	return result
}

func (r *transactionReconciler) syncProviderFailed(ctx context.Context, tx *model.Transaction, result *pb.SyncTransactionResult) *pb.SyncTransactionResult {
	if tx.Status == model.StatusFailed {
		result.Outcome = syncOutcomeUnchanged
		result.Message = "transaction already failed"
		return result
	}
	if tx.Status != model.StatusPending {
		result.Outcome = syncOutcomeUnchanged
		result.Message = "provider is failed but local transaction is not pending"
		return result
	}
	reason := result.RawPaymentStatus
	if reason == "" {
		reason = "provider: payment expired or rejected"
	}
	if failErr := r.txStore.MarkFailed(ctx, tx.ID, reason, time.Now().UTC().AddDate(0, 0, 7)); failErr != nil {
		result.Outcome = syncOutcomeSyncFailed
		result.Message = "failed to mark transaction failed: " + failErr.Error()
		return result
	}
	result.Outcome = syncOutcomeFailed
	result.Message = "provider failure synced"
	return result
}

func (r *transactionReconciler) syncProviderRefund(ctx context.Context, tx *model.Transaction, result *pb.SyncTransactionResult) *pb.SyncTransactionResult {
	if tx.Refund != nil && tx.Refund.Status == model.RefundStatusRefunded {
		result.Outcome = syncOutcomeUnchanged
		result.RefundStatus = model.RefundStatusRefunded
		result.Message = "transaction already marked refunded"
		return result
	}

	if tx.Status != model.StatusFulfilled {
		result.Outcome = syncOutcomeUnchanged
		result.Message = "provider reports refund but local transaction is not fulfilled"
		return result
	}

	claimed, err := r.txStore.AtomicClaimExternalRefunding(ctx, tx.ID, "provider refund sync")
	if err == store.ErrNoDocuments {
		result.Outcome = syncOutcomeUnchanged
		result.Message = "transaction is not eligible for refund sync"
		return result
	}
	if err != nil {
		result.Outcome = syncOutcomeSyncFailed
		result.Message = "failed to claim refund sync: " + err.Error()
		return result
	}
	if reverseErr := reverseAGSRefund(ctx, r.txStore, r.reverser, claimed); reverseErr != nil {
		result.Outcome = syncOutcomeSyncFailed
		result.RefundStatus = model.RefundStatusRefundFailed
		result.Message = reverseErr.Error()
		return result
	}

	result.Outcome = syncOutcomeRefunded
	result.RefundStatus = model.RefundStatusRefunded
	result.Message = "provider refund synced and AGS fulfillment reversed"
	return result
}

func (r *transactionReconciler) maybeUpdateProviderTxID(ctx context.Context, tx *model.Transaction, providerTxID string) {
	if providerTxID == "" || providerTxID == tx.ProviderTxID {
		return
	}
	if err := r.txStore.UpdateProviderTransactionID(ctx, tx.ID, providerTxID); err != nil {
		slog.Warn("SyncTransaction: provider tx id update failed", "txn_id", tx.ID, "provider_tx_id", providerTxID, "error", err)
	}
}
