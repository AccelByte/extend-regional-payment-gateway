package service

import (
	"context"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/store"
)

func reverseAGSRefund(ctx context.Context, txStore store.Store, reverser FulfillmentReverser, tx *model.Transaction) error {
	if tx == nil {
		return status.Error(codes.Internal, "AGS reversal failed: transaction is not loaded")
	}
	if reverser == nil {
		reason := "AGS reversal failed after provider refund: reverser is not configured"
		if markErr := txStore.MarkRefundFailed(ctx, tx.ID, reason); markErr != nil {
			slog.Error("Refund: MarkRefundFailed failed", "txn_id", tx.ID, "error", markErr)
		}
		return status.Error(codes.Internal, "AGS reversal failed: reverser is not configured")
	}
	if reverseErr := reverser.ReverseFulfillment(ctx, tx); reverseErr != nil {
		slog.Error("Refund: AGS ReverseFulfillment failed", "txn_id", tx.ID, "error", reverseErr)
		reason := "AGS reversal failed after provider refund: " + reverseErr.Error()
		if markErr := txStore.MarkRefundFailed(ctx, tx.ID, reason); markErr != nil {
			slog.Error("Refund: MarkRefundFailed failed", "txn_id", tx.ID, "error", markErr)
		}
		return status.Errorf(codes.Internal, "AGS reversal failed: %v", reverseErr)
	}
	if commitErr := txStore.CommitRefund(ctx, tx.ID); commitErr != nil {
		slog.Error("Refund: CommitRefund failed", "txn_id", tx.ID, "error", commitErr)
		return status.Error(codes.Internal, "failed to commit refund status")
	}
	return nil
}
