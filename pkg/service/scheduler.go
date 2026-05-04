package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/config"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/fulfillment"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/store"
)

// SchedulerService runs two background recovery jobs.
//
// Job 1 (every 2 min): retries transactions stuck in FULFILLING > 2 min.
// Job 2 (every 5 min): polls provider status for PENDING transactions > 5 min old.
//
// These jobs can be triggered by the AccelByte Extend Task Scheduler via the
// OnJobTriggered handler, or run autonomously via Start().
type SchedulerService struct {
	txStore   store.Store
	registry  *adapter.Registry
	fulfiller *fulfillment.Fulfiller
	notifier  *fulfillment.Notifier
	cfg       *config.Config
}

func NewSchedulerService(
	txStore store.Store,
	registry *adapter.Registry,
	fulfiller *fulfillment.Fulfiller,
	notifier *fulfillment.Notifier,
	cfg *config.Config,
) *SchedulerService {
	return &SchedulerService{
		txStore:   txStore,
		registry:  registry,
		fulfiller: fulfiller,
		notifier:  notifier,
		cfg:       cfg,
	}
}

// Start runs both recovery jobs on their respective intervals until ctx is cancelled.
func (s *SchedulerService) Start(ctx context.Context) {
	go s.runTicker(ctx, 2*time.Minute, "stuck-fulfillment-recovery", s.runStuckFulfillmentRecovery)
	go s.runTicker(ctx, 5*time.Minute, "lost-webhook-recovery", s.runLostWebhookRecovery)
}

// OnJobTriggered is the entry point for the AccelByte Extend Task Scheduler.
// Wire this to the bidirectional streaming handler when the Task Scheduler proto is available.
func (s *SchedulerService) OnJobTriggered(ctx context.Context, jobName string) {
	switch jobName {
	case "stuck-fulfillment-recovery":
		s.runStuckFulfillmentRecovery(ctx)
	case "lost-webhook-recovery":
		s.runLostWebhookRecovery(ctx)
	default:
		slog.Warn("unknown scheduler job", "job", jobName)
	}
}

func (s *SchedulerService) runTicker(ctx context.Context, interval time.Duration, name string, fn func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			slog.Info("scheduler job starting", "job", name)
			fn(ctx)
			slog.Info("scheduler job finished", "job", name)
		}
	}
}

// runStuckFulfillmentRecovery finds transactions stuck in FULFILLING > 2 min and retries fulfillment.
func (s *SchedulerService) runStuckFulfillmentRecovery(ctx context.Context) {
	olderThan := time.Now().UTC().Add(-2 * time.Minute)
	txns, err := s.txStore.FindStuckFulfilling(ctx, olderThan)
	if err != nil {
		slog.Error("scheduler: FindStuckFulfilling failed", "error", err)
		return
	}

	for _, tx := range txns {
		s.recoverFulfilling(ctx, tx)
	}
}

func (s *SchedulerService) recoverFulfilling(ctx context.Context, tx *model.Transaction) {
	err := s.fulfiller.FulfillUserItem(ctx, tx.UserID, tx.ItemID, tx.ID, tx.Quantity)
	if err == nil {
		deleteAt := time.Now().UTC().AddDate(0, 0, s.cfg.RecordRetentionDays)
		if commitErr := s.txStore.CommitFulfilled(ctx, tx.ID, "FULFILLED_BY_SCHEDULER", deleteAt); commitErr != nil {
			slog.Error("scheduler: CommitFulfilled failed", "txn_id", tx.ID, "error", commitErr)
			return
		}
		s.notifier.NotifyPaymentResult(ctx, tx.UserID, tx.ID, model.StatusFulfilled, tx.ItemID)
		slog.Info("scheduler: recovered stuck fulfillment", "txn_id", tx.ID)
		return
	}

	slog.Warn("scheduler: FulfillUserItem failed", "txn_id", tx.ID, "error", err)
	if tx.Retries >= int32(s.cfg.MaxRetries) {
		if failErr := s.txStore.MarkFailed(ctx, tx.ID, "max retries exceeded: "+err.Error(), time.Now().UTC().AddDate(0, 0, 7)); failErr != nil {
			slog.Error("scheduler: MarkFailed failed", "txn_id", tx.ID, "error", failErr)
		}
		s.notifier.NotifyPaymentResult(ctx, tx.UserID, tx.ID, model.StatusFailed, tx.ItemID)
		return
	}

	if incErr := s.txStore.IncrementRetries(ctx, tx.ID); incErr != nil {
		slog.Error("scheduler: IncrementRetries failed", "txn_id", tx.ID, "error", incErr)
	}
}

// runLostWebhookRecovery polls provider status for PENDING transactions older than 5 min.
func (s *SchedulerService) runLostWebhookRecovery(ctx context.Context) {
	expired, expErr := s.txStore.FindExpiredPending(ctx, time.Now().UTC())
	if expErr != nil {
		slog.Error("scheduler: FindExpiredPending failed", "error", expErr)
	} else {
		for _, tx := range expired {
			s.expirePending(ctx, tx)
		}
	}

	olderThan := time.Now().UTC().Add(-5 * time.Minute)
	txns, err := s.txStore.FindStuckPending(ctx, olderThan)
	if err != nil {
		slog.Error("scheduler: FindStuckPending failed", "error", err)
		return
	}

	for _, tx := range txns {
		s.recoverPending(ctx, tx)
	}
}

func (s *SchedulerService) expirePending(ctx context.Context, tx *model.Transaction) {
	if tx.ProviderTxID != "" {
		providerID := resolveProviderID(tx)
		if prov, err := s.registry.Get(providerID); err == nil {
			if ps, psErr := prov.GetPaymentStatus(ctx, tx.ProviderTxID); psErr == nil {
				switch ps.Status {
				case adapter.PaymentStatusSuccess:
					s.recoverPending(ctx, tx)
					return
				case adapter.PaymentStatusCanceled:
					if err := s.txStore.MarkCanceledIfPending(ctx, tx.ID, "provider: payment canceled", string(ps.Status), time.Now().UTC().AddDate(0, 0, s.cfg.RecordRetentionDays)); err != nil && err != store.ErrNoDocuments {
						slog.Error("scheduler: MarkCanceledIfPending failed", "txn_id", tx.ID, "error", err)
					}
					return
				case adapter.PaymentStatusExpired, adapter.PaymentStatusFailed:
					// fall through to mark expired below
				default:
					_, _ = prov.CancelPayment(ctx, tx, "payment expired")
				}
			}
		}
	}
	if err := s.txStore.MarkExpiredIfPending(ctx, tx.ID, "payment expired", "", time.Now().UTC().AddDate(0, 0, s.cfg.RecordRetentionDays)); err != nil && err != store.ErrNoDocuments {
		slog.Error("scheduler: MarkExpiredIfPending failed", "txn_id", tx.ID, "error", err)
	}
}

func (s *SchedulerService) recoverPending(ctx context.Context, tx *model.Transaction) {
	if tx.ProviderTxID == "" {
		return // no provider TX ID yet — cannot poll status
	}

	providerID := resolveProviderID(tx)
	prov, err := s.registry.Get(providerID)
	if err != nil {
		slog.Warn("scheduler: unknown provider for pending tx", "txn_id", tx.ID, "provider", providerID)
		return
	}

	ps, err := prov.GetPaymentStatus(ctx, tx.ProviderTxID)
	if err != nil {
		if err == adapter.ErrNotSupported {
			return // webhook-only provider — wait for webhook
		}
		slog.Warn("scheduler: GetPaymentStatus failed", "txn_id", tx.ID, "error", err)
		return
	}

	switch ps.Status {
	case adapter.PaymentStatusSuccess:
		providerTxID := providerTxIDForClaim(tx.ProviderTxID, ps.ProviderTxID)
		claimed, claimErr := s.txStore.AtomicClaimFulfilling(ctx, tx.ID, providerTxID)
		if claimErr == store.ErrNoDocuments {
			return // already handled concurrently
		}
		if claimErr != nil {
			slog.Error("scheduler: AtomicClaimFulfilling failed", "txn_id", tx.ID, "error", claimErr)
			return
		}
		if fulfillErr := s.fulfiller.FulfillUserItem(ctx, claimed.UserID, claimed.ItemID, claimed.ID, claimed.Quantity); fulfillErr != nil {
			slog.Error("scheduler: FulfillUserItem failed in lost-webhook-recovery", "txn_id", tx.ID, "error", fulfillErr)
			return
		}
		deleteAt := time.Now().UTC().AddDate(0, 0, s.cfg.RecordRetentionDays)
		if commitErr := s.txStore.CommitFulfilled(ctx, tx.ID, string(ps.Status), deleteAt); commitErr != nil {
			slog.Error("scheduler: CommitFulfilled failed", "txn_id", tx.ID, "error", commitErr)
		}
		s.notifier.NotifyPaymentResult(ctx, tx.UserID, tx.ID, model.StatusFulfilled, tx.ItemID)
		slog.Info("scheduler: recovered lost-webhook transaction", "txn_id", tx.ID)

	case adapter.PaymentStatusFailed:
		if failErr := s.txStore.MarkFailed(ctx, tx.ID, "provider: payment rejected", time.Now().UTC().AddDate(0, 0, 7)); failErr != nil {
			slog.Error("scheduler: MarkFailed failed", "txn_id", tx.ID, "error", failErr)
		}
		s.notifier.NotifyPaymentResult(ctx, tx.UserID, tx.ID, model.StatusFailed, tx.ItemID)
	case adapter.PaymentStatusCanceled:
		if cancelErr := s.txStore.MarkCanceledIfPending(ctx, tx.ID, "provider: payment canceled", string(ps.Status), time.Now().UTC().AddDate(0, 0, s.cfg.RecordRetentionDays)); cancelErr != nil {
			slog.Error("scheduler: MarkCanceledIfPending failed", "txn_id", tx.ID, "error", cancelErr)
		}
		s.notifier.NotifyPaymentResult(ctx, tx.UserID, tx.ID, model.StatusCanceled, tx.ItemID)
	case adapter.PaymentStatusExpired:
		if expireErr := s.txStore.MarkExpiredIfPending(ctx, tx.ID, "provider: payment expired", string(ps.Status), time.Now().UTC().AddDate(0, 0, s.cfg.RecordRetentionDays)); expireErr != nil {
			slog.Error("scheduler: MarkExpiredIfPending failed", "txn_id", tx.ID, "error", expireErr)
		}
		s.notifier.NotifyPaymentResult(ctx, tx.UserID, tx.ID, model.StatusExpired, tx.ItemID)

	default:
		// Still pending — player may still be on payment screen
	}
}

func providerTxIDForClaim(storedProviderTxID string, polledProviderTxID string) string {
	if polledProviderTxID != "" {
		return polledProviderTxID
	}
	return storedProviderTxID
}
