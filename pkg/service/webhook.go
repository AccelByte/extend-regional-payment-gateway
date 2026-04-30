package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/config"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/fulfillment"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/store"
	pb "github.com/accelbyte/extend-regional-payment-gateway/pkg/pb"
)

// ItemFulfiller is the interface WebhookService uses to grant items to users.
// *fulfillment.Fulfiller satisfies this interface.
type ItemFulfiller interface {
	FulfillUserItem(ctx context.Context, userID, itemID, orderNo string, qty int32) error
}

// WebhookService implements the gRPC WebhookService.
// This is the most critical service — it runs the atomic PENDING→FULFILLING→FULFILLED state machine.
type WebhookService struct {
	pb.UnimplementedWebhookServiceServer
	txStore   store.Store
	registry  *adapter.Registry
	fulfiller ItemFulfiller
	reverser  FulfillmentReverser
	notifier  *fulfillment.Notifier
	cfg       *config.Config
}

func NewWebhookService(
	txStore store.Store,
	registry *adapter.Registry,
	fulfiller ItemFulfiller,
	reverser FulfillmentReverser,
	notifier *fulfillment.Notifier,
	cfg *config.Config,
) *WebhookService {
	return &WebhookService{
		txStore:   txStore,
		registry:  registry,
		fulfiller: fulfiller,
		reverser:  reverser,
		notifier:  notifier,
		cfg:       cfg,
	}
}

// HandleWebhook is the critical path: validate → atomic claim → fulfill → commit.
//
// This method is called directly (not through gRPC-Gateway) for the webhook route
// so that the raw body bytes are preserved for signature validation.
func (s *WebhookService) HandleWebhook(ctx context.Context, req *pb.WebhookRequest) (*pb.WebhookResponse, error) {
	providerName := req.ProviderName
	headers := req.Headers
	rawBody := req.RawPayload

	// Step 1: Resolve adapter
	prov, err := s.registry.Get(providerName)
	if err != nil {
		slog.Warn("webhook: unknown provider", "provider", providerName)
		return nil, status.Errorf(codes.InvalidArgument, "unknown provider: %s", providerName)
	}

	// Step 2: Validate signature BEFORE any state mutation
	if err := prov.ValidateWebhookSignature(ctx, rawBody, headers); err != nil {
		slog.Warn("webhook: signature validation failed", "provider", providerName, "error", err)
		return nil, status.Errorf(codes.InvalidArgument, "invalid webhook signature: %v", err)
	}

	// Step 2b: Force-error mode for DANA certification (WEBHOOK_FORCE_ERROR=true).
	// Returns 5005601 immediately so DANA's dashboard records the error scenario.
	// Unset the env var before DANA retries so the retry receives 2005600.
	if s.cfg.WebhookForceError {
		slog.Warn("webhook: WEBHOOK_FORCE_ERROR is set — returning forced error", "provider", providerName)
		return nil, status.Error(codes.Internal, "forced error for certification")
	}

	// Step 3: Parse webhook payload
	result, err := prov.HandleWebhook(ctx, rawBody, headers)
	if err != nil {
		slog.Error("webhook: HandleWebhook parse failed", "provider", providerName, "error", err)
		return nil, status.Errorf(codes.InvalidArgument, "failed to parse webhook: %v", err)
	}

	// Step 4: Handle provider-confirmed refund (e.g. Xendit refund.succeeded webhook)
	if result.Status == adapter.PaymentStatusRefunded {
		return s.handleProviderRefund(ctx, result)
	}

	// Step 4b: Handle failed payment
	if result.Status == adapter.PaymentStatusCanceled || result.Status == adapter.PaymentStatusExpired {
		deleteAt := time.Now().UTC().AddDate(0, 0, s.cfg.RecordRetentionDays)
		var markErr error
		statusName := model.StatusCanceled
		if result.Status == adapter.PaymentStatusExpired {
			statusName = model.StatusExpired
			markErr = s.txStore.MarkExpiredIfPending(ctx, result.InternalOrderID, result.FailureReason, result.RawProviderStatus, deleteAt)
		} else {
			markErr = s.txStore.MarkCanceledIfPending(ctx, result.InternalOrderID, result.FailureReason, result.RawProviderStatus, deleteAt)
		}
		if markErr != nil && markErr != store.ErrNotFound && markErr != store.ErrNoDocuments {
			slog.Error("webhook: mark terminal error", "txn_id", result.InternalOrderID, "status", statusName, "error", markErr)
		}
		tx, _ := s.txStore.FindByID(ctx, result.InternalOrderID)
		if tx != nil {
			s.notifier.NotifyPaymentResult(ctx, tx.UserID, tx.ID, statusName, tx.ItemID)
		}
		return &pb.WebhookResponse{Message: strings.ToLower(statusName)}, nil
	}

	if result.Status == adapter.PaymentStatusFailed {
		deleteAt := time.Now().UTC().AddDate(0, 0, 7)
		if failErr := s.txStore.MarkFailed(ctx, result.InternalOrderID, result.FailureReason, deleteAt); failErr != nil && failErr != store.ErrNotFound {
			slog.Error("webhook: MarkFailed error", "txn_id", result.InternalOrderID, "error", failErr)
		}
		tx, _ := s.txStore.FindByID(ctx, result.InternalOrderID)
		if tx != nil {
			s.notifier.NotifyPaymentResult(ctx, tx.UserID, tx.ID, model.StatusFailed, tx.ItemID)
		}
		return &pb.WebhookResponse{Message: "payment failed"}, nil
	}

	// Step 5: Only proceed for confirmed SUCCESS
	if result.Status != adapter.PaymentStatusSuccess {
		slog.Info("webhook: ignoring non-terminal status", "provider", providerName, "status", result.Status)
		return &pb.WebhookResponse{Message: "ok"}, nil
	}

	// Step 6: Atomic claim — PENDING → FULFILLING (prevents double-grants)
	tx, claimErr := s.txStore.AtomicClaimFulfilling(ctx, result.InternalOrderID, result.ProviderTransactionID)
	if claimErr == store.ErrNoDocuments {
		// Document exists but is already FULFILLING/FULFILLED/FAILED — another process handled it
		slog.Info("webhook: transaction already being handled", "txn_id", result.InternalOrderID)
		return &pb.WebhookResponse{Message: "ok"}, nil
	}
	if claimErr == store.ErrNotFound {
		// Transaction unknown — may have been cleaned up or belong to another service instance.
		// Return 200 so the provider doesn't retry indefinitely.
		slog.Warn("webhook: transaction not found, ignoring", "txn_id", result.InternalOrderID)
		return &pb.WebhookResponse{Message: "ok"}, nil
	}
	if claimErr != nil {
		slog.Error("webhook: AtomicClaimFulfilling failed", "txn_id", result.InternalOrderID, "error", claimErr)
		return nil, status.Error(codes.Internal, "failed to claim transaction")
	}

	// Step 7: Call AGS FulfillUserItem
	// On error: reset to PENDING so the provider retry or scheduler can reclaim it, then return 500.
	if fulfillErr := s.fulfiller.FulfillUserItem(ctx, tx.UserID, tx.ItemID, tx.ID, tx.Quantity); fulfillErr != nil {
		slog.Error("webhook: FulfillUserItem failed", "txn_id", tx.ID, "error", fulfillErr)
		if resetErr := s.txStore.ResetToPending(ctx, tx.ID); resetErr != nil {
			slog.Error("webhook: ResetToPending failed", "txn_id", tx.ID, "error", resetErr)
		}
		return nil, status.Errorf(codes.Internal, "fulfillment failed: %v", fulfillErr)
	}

	// Step 8: Commit FULFILLING → FULFILLED
	deleteAt := time.Now().UTC().AddDate(0, 0, s.cfg.RecordRetentionDays)
	if commitErr := s.txStore.CommitFulfilled(ctx, tx.ID, result.RawProviderStatus, deleteAt); commitErr != nil {
		slog.Error("webhook: CommitFulfilled failed", "txn_id", tx.ID, "error", commitErr)
		// Don't return error — item was already granted. Scheduler will fix the status.
	}

	// Step 9: Notify player (fire-and-forget)
	s.notifier.NotifyPaymentResult(ctx, tx.UserID, tx.ID, model.StatusFulfilled, tx.ItemID)

	slog.Info("webhook: transaction fulfilled", "txn_id", tx.ID, "provider", providerName)
	return &pb.WebhookResponse{Message: "ok"}, nil
}

// handleProviderRefund processes a provider-confirmed refund webhook (e.g. Xendit refund.succeeded).
// If the transaction is already marked refunded locally (our admin API issued the refund), it just
// acknowledges. If the transaction is still FULFILLED (dashboard-initiated refund), it reverses AGS.
func (s *WebhookService) handleProviderRefund(ctx context.Context, result *adapter.PaymentResult) (*pb.WebhookResponse, error) {
	tx, err := s.txStore.FindByID(ctx, result.InternalOrderID)
	if err == store.ErrNotFound {
		slog.Info("webhook: refund event for unknown transaction, ignoring", "txn_id", result.InternalOrderID)
		return &pb.WebhookResponse{Message: "ok"}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to look up transaction: %v", err)
	}

	if tx.Refund != nil && tx.Refund.Status == model.RefundStatusRefunded {
		slog.Info("webhook: refund event for already-refunded transaction", "txn_id", result.InternalOrderID)
		return &pb.WebhookResponse{Message: "ok"}, nil
	}
	if tx.Status != model.StatusFulfilled {
		slog.Info("webhook: refund event for non-fulfilled transaction", "txn_id", result.InternalOrderID, "tx_status", tx.Status)
		return &pb.WebhookResponse{Message: "ok"}, nil
	}

	claimed, claimErr := s.txStore.AtomicClaimExternalRefunding(ctx, tx.ID, "provider refund webhook")
	if claimErr == store.ErrNoDocuments {
		slog.Info("webhook: transaction not eligible for refund claim", "txn_id", result.InternalOrderID)
		return &pb.WebhookResponse{Message: "ok"}, nil
	}
	if claimErr != nil {
		return nil, status.Errorf(codes.Internal, "failed to claim refund: %v", claimErr)
	}

	if reverseErr := reverseAGSRefund(ctx, s.txStore, s.reverser, claimed); reverseErr != nil {
		slog.Error("webhook: reverseAGSRefund failed", "txn_id", result.InternalOrderID, "error", reverseErr)
		return nil, status.Errorf(codes.Internal, "AGS refund reversal failed: %v", reverseErr)
	}

	slog.Info("webhook: refund processed via provider notification", "txn_id", result.InternalOrderID)
	return &pb.WebhookResponse{Message: "ok"}, nil
}
