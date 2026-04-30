package service

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/config"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/store"
	pb "github.com/accelbyte/extend-regional-payment-gateway/pkg/pb"
)

func (s *PaymentService) CancelPaymentForExistingTransaction(ctx context.Context, transactionID string, reason string) (*pb.CancelTransactionResponse, error) {
	userID, err := userIDFromContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "missing user identity")
	}
	tx, err := s.txStore.FindByID(ctx, transactionID)
	if err == store.ErrNotFound {
		return nil, status.Error(codes.NotFound, "transaction not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to retrieve transaction")
	}
	if tx.UserID != userID {
		return nil, status.Error(codes.PermissionDenied, "transaction does not belong to user")
	}
	return cancelPendingTransaction(ctx, s.txStore, s.registry, s.cfg, tx, reason, nil)
}

func (s *PaymentService) CancelSelectedProviderForExistingTransaction(ctx context.Context, transactionID string, reason string) (*pb.CancelTransactionResponse, error) {
	userID, err := userIDFromContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "missing user identity")
	}
	tx, err := s.txStore.FindByID(ctx, transactionID)
	if err == store.ErrNotFound {
		return nil, status.Error(codes.NotFound, "transaction not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to retrieve transaction")
	}
	if tx.UserID != userID {
		return nil, status.Error(codes.PermissionDenied, "transaction does not belong to user")
	}
	if tx.Status != model.StatusPending {
		return nil, status.Errorf(codes.FailedPrecondition, "selected provider cannot be canceled from status %s", tx.Status)
	}
	if tx.ProviderTxID == "" {
		return &pb.CancelTransactionResponse{TransactionId: tx.ID, Success: true, Transaction: txToTransactionResponse(tx), Message: "no selected provider to cancel"}, nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "user canceled selected provider"
	}
	prov, regErr := s.registry.Get(resolveProviderName(tx))
	if regErr != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "unknown provider: %s", resolveProviderName(tx))
	}
	canceler, ok := prov.(adapter.PaymentCanceler)
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "provider cancellation is not supported for selected payment")
	}
	result, cancelErr := canceler.CancelPayment(ctx, tx, reason)
	if cancelErr != nil {
		return nil, status.Errorf(codes.Unavailable, "provider cancel failed: %v", cancelErr)
	}
	if result == nil {
		return nil, status.Error(codes.Unavailable, "provider cancel returned empty result")
	}
	switch result.Status {
	case adapter.CancelStatusCanceled, adapter.CancelStatusExpired:
		if err := s.txStore.ClearProviderTransactionIfPending(ctx, tx.ID, tx.ProviderTxID); err != nil {
			return nil, terminalMarkError(err, "clear selected provider")
		}
		updated, findErr := s.txStore.FindByID(ctx, tx.ID)
		if findErr != nil {
			return nil, status.Error(codes.Internal, "failed to retrieve updated transaction")
		}
		return &pb.CancelTransactionResponse{TransactionId: updated.ID, Success: true, Transaction: txToTransactionResponse(updated), Message: "selected provider canceled"}, nil
	case adapter.CancelStatusAlreadyPaid:
		return nil, status.Error(codes.FailedPrecondition, "provider reports payment already completed")
	case adapter.CancelStatusPending:
		msg := result.Message
		if msg == "" {
			msg = "provider cancellation is still pending"
		}
		return nil, status.Error(codes.Unavailable, msg)
	case adapter.CancelStatusUnsupported:
		return nil, status.Error(codes.FailedPrecondition, "provider cancellation is not supported for selected payment")
	default:
		msg := result.FailureReason
		if msg == "" {
			msg = result.Message
		}
		if msg == "" {
			msg = "provider cancellation failed"
		}
		return nil, status.Error(codes.Unavailable, msg)
	}
}

func cancelPendingTransaction(ctx context.Context, txStore store.Store, registry *adapter.Registry, cfg *config.Config, tx *model.Transaction, reason string, onAlreadyPaid func()) (*pb.CancelTransactionResponse, error) {
	if tx.Status == model.StatusCanceled {
		return &pb.CancelTransactionResponse{TransactionId: tx.ID, Success: true, Transaction: txToTransactionResponse(tx), Message: "transaction already canceled"}, nil
	}
	if tx.Status != model.StatusPending {
		return nil, status.Errorf(codes.FailedPrecondition, "transaction cannot be canceled from status %s", tx.Status)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "user canceled transaction"
	}
	providerStatus := ""
	if tx.ProviderTxID != "" {
		prov, regErr := registry.Get(resolveProviderName(tx))
		if regErr != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "unknown provider: %s", resolveProviderName(tx))
		}
		canceler, ok := prov.(adapter.PaymentCanceler)
		if !ok {
			return nil, status.Error(codes.FailedPrecondition, "provider cancellation is not supported for selected payment")
		}
		result, cancelErr := canceler.CancelPayment(ctx, tx, reason)
		if cancelErr != nil {
			return nil, status.Errorf(codes.Unavailable, "provider cancel failed: %v", cancelErr)
		}
		if result == nil {
			return nil, status.Error(codes.Unavailable, "provider cancel returned empty result")
		}
		providerStatus = result.ProviderStatus
		if providerStatus == "" {
			providerStatus = string(result.Status)
		}
		switch result.Status {
		case adapter.CancelStatusCanceled:
		case adapter.CancelStatusExpired:
			updated, markErr := markExpiredTransaction(ctx, txStore, cfg, tx.ID, reason, providerStatus)
			if markErr != nil {
				return nil, markErr
			}
			return &pb.CancelTransactionResponse{TransactionId: updated.ID, Success: true, Transaction: txToTransactionResponse(updated), Message: "transaction already expired at provider"}, nil
		case adapter.CancelStatusAlreadyPaid:
			if onAlreadyPaid != nil {
				onAlreadyPaid()
			}
			return nil, status.Error(codes.FailedPrecondition, "provider reports payment already completed")
		case adapter.CancelStatusPending:
			msg := result.Message
			if msg == "" {
				msg = "provider cancellation is still pending"
			}
			return nil, status.Error(codes.Unavailable, msg)
		case adapter.CancelStatusUnsupported:
			return nil, status.Error(codes.FailedPrecondition, "provider cancellation is not supported for selected payment")
		default:
			msg := result.FailureReason
			if msg == "" {
				msg = result.Message
			}
			if msg == "" {
				msg = "provider cancellation failed"
			}
			return nil, status.Error(codes.Unavailable, msg)
		}
	}
	updated, markErr := markCanceledTransaction(ctx, txStore, cfg, tx.ID, reason, providerStatus)
	if markErr != nil {
		return nil, markErr
	}
	return &pb.CancelTransactionResponse{TransactionId: updated.ID, Success: true, Transaction: txToTransactionResponse(updated), Message: "transaction canceled"}, nil
}

func markCanceledTransaction(ctx context.Context, txStore store.Store, cfg *config.Config, txnID, reason, providerStatus string) (*model.Transaction, error) {
	if err := txStore.MarkCanceledIfPending(ctx, txnID, reason, providerStatus, cancelTerminalDeleteAt(cfg)); err != nil {
		return nil, terminalMarkError(err, "cancel")
	}
	return txStore.FindByID(ctx, txnID)
}

func markExpiredTransaction(ctx context.Context, txStore store.Store, cfg *config.Config, txnID, reason, providerStatus string) (*model.Transaction, error) {
	if err := txStore.MarkExpiredIfPending(ctx, txnID, reason, providerStatus, cancelTerminalDeleteAt(cfg)); err != nil {
		return nil, terminalMarkError(err, "expire")
	}
	return txStore.FindByID(ctx, txnID)
}

func cancelTerminalDeleteAt(cfg *config.Config) time.Time {
	days := 7
	if cfg != nil && cfg.RecordRetentionDays > 0 {
		days = cfg.RecordRetentionDays
	}
	return time.Now().UTC().AddDate(0, 0, days)
}
