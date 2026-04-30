package service

import (
	"context"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/config"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/store"
	pb "github.com/accelbyte/extend-regional-payment-gateway/pkg/pb"
)

const (
	defaultPublicSyncBatchSize = int32(10)
	maxPublicSyncBatchSize     = int32(20)
	defaultPublicSyncCooldown  = 60 * time.Second
)

type PublicService struct {
	pb.UnimplementedPublicServiceServer
	txStore  store.Store
	cfg      *config.Config
	syncer   *transactionReconciler
	cooldown publicSyncCooldown
}

type publicSyncCooldown struct {
	mu       sync.Mutex
	lastSeen map[string]time.Time
}

func NewPublicService(txStore store.Store, registry *adapter.Registry, reverser FulfillmentReverser, cfg *config.Config) *PublicService {
	return &PublicService{
		txStore: txStore,
		cfg:     cfg,
		syncer:  newTransactionReconciler(txStore, registry, reverser, cfg),
		cooldown: publicSyncCooldown{
			lastSeen: make(map[string]time.Time),
		},
	}
}

func (s *PublicService) SyncMyTransaction(ctx context.Context, req *pb.PublicSyncTransactionRequest) (*pb.SyncTransactionResponse, error) {
	userID, err := userIDFromContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "missing user identity")
	}
	if strings.TrimSpace(req.Namespace) == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace is required")
	}
	if strings.TrimSpace(req.TransactionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "transaction_id is required")
	}

	tx, err := s.txStore.FindByID(ctx, req.TransactionId)
	if err == store.ErrNotFound {
		return nil, status.Error(codes.NotFound, "transaction not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to retrieve transaction")
	}
	if tx.Namespace != req.Namespace || tx.UserID != userID {
		return nil, status.Error(codes.NotFound, "transaction not found")
	}
	if err := s.claimCooldown(req.Namespace, userID, resolveProviderName(tx)); err != nil {
		return nil, err
	}

	return &pb.SyncTransactionResponse{Result: s.syncer.syncTransaction(ctx, tx)}, nil
}

func (s *PublicService) SyncMyTransactions(ctx context.Context, req *pb.PublicSyncTransactionsRequest) (*pb.SyncTransactionsResponse, error) {
	userID, err := userIDFromContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "missing user identity")
	}
	if strings.TrimSpace(req.Namespace) == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace is required")
	}
	if err := s.claimCooldown(req.Namespace, userID, req.Provider); err != nil {
		return nil, err
	}

	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = s.defaultPageSize()
	}
	if maxPageSize := s.maxPageSize(); pageSize > maxPageSize {
		pageSize = maxPageSize
	}

	txns, nextCursor, err := s.txStore.ListTransactions(ctx, store.ListQuery{
		Namespace:     req.Namespace,
		UserID:        userID,
		Provider:      req.Provider,
		StatusFilters: []string{model.StatusPending, model.StatusFulfilled},
		PageSize:      pageSize,
		Cursor:        req.Cursor,
	})
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to list transactions for sync")
	}

	results := make([]*pb.SyncTransactionResult, 0, len(txns))
	for _, tx := range txns {
		results = append(results, s.syncer.syncTransaction(ctx, tx))
	}
	return &pb.SyncTransactionsResponse{
		Results:    results,
		NextCursor: nextCursor,
	}, nil
}

func (s *PublicService) CancelMyTransaction(ctx context.Context, req *pb.CancelTransactionRequest) (*pb.CancelTransactionResponse, error) {
	userID, err := userIDFromContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "missing user identity")
	}
	if strings.TrimSpace(req.Namespace) == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace is required")
	}
	if strings.TrimSpace(req.TransactionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "transaction_id is required")
	}

	tx, err := s.txStore.FindByID(ctx, req.TransactionId)
	if err == store.ErrNotFound {
		return nil, status.Error(codes.NotFound, "transaction not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to retrieve transaction")
	}
	if tx.Namespace != req.Namespace || tx.UserID != userID {
		return nil, status.Error(codes.NotFound, "transaction not found")
	}
	if tx.Status == model.StatusCanceled {
		return &pb.CancelTransactionResponse{
			TransactionId: tx.ID,
			Success:       true,
			Transaction:   txToTransactionResponse(tx),
			Message:       "transaction already canceled",
		}, nil
	}
	if tx.Status != model.StatusPending {
		return nil, status.Errorf(codes.FailedPrecondition, "transaction cannot be canceled from status %s", tx.Status)
	}

	providerStatus := ""
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "user canceled transaction"
	}

	if tx.ProviderTxID != "" {
		prov, regErr := s.syncer.registry.Get(resolveProviderName(tx))
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
			// continue to local terminal transition
		case adapter.CancelStatusExpired:
			updated, markErr := s.markExpired(ctx, tx.ID, reason, providerStatus)
			if markErr != nil {
				return nil, markErr
			}
			return &pb.CancelTransactionResponse{
				TransactionId: updated.ID,
				Success:       true,
				Transaction:   txToTransactionResponse(updated),
				Message:       "transaction already expired at provider",
			}, nil
		case adapter.CancelStatusAlreadyPaid:
			s.syncer.syncTransaction(ctx, tx)
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

	updated, markErr := s.markCanceled(ctx, tx.ID, reason, providerStatus)
	if markErr != nil {
		return nil, markErr
	}
	return &pb.CancelTransactionResponse{
		TransactionId: updated.ID,
		Success:       true,
		Transaction:   txToTransactionResponse(updated),
		Message:       "transaction canceled",
	}, nil
}

func (s *PublicService) markCanceled(ctx context.Context, txnID, reason, providerStatus string) (*model.Transaction, error) {
	if err := s.txStore.MarkCanceledIfPending(ctx, txnID, reason, providerStatus, terminalDeleteAt(s.cfg)); err != nil {
		return nil, terminalMarkError(err, "cancel")
	}
	return s.txStore.FindByID(ctx, txnID)
}

func (s *PublicService) markExpired(ctx context.Context, txnID, reason, providerStatus string) (*model.Transaction, error) {
	if err := s.txStore.MarkExpiredIfPending(ctx, txnID, reason, providerStatus, terminalDeleteAt(s.cfg)); err != nil {
		return nil, terminalMarkError(err, "expire")
	}
	return s.txStore.FindByID(ctx, txnID)
}

func terminalDeleteAt(cfg *config.Config) time.Time {
	days := 7
	if cfg != nil && cfg.RecordRetentionDays > 0 {
		days = cfg.RecordRetentionDays
	}
	return time.Now().UTC().AddDate(0, 0, days)
}

func terminalMarkError(err error, action string) error {
	if err == store.ErrNotFound {
		return status.Error(codes.NotFound, "transaction not found")
	}
	if err == store.ErrNoDocuments {
		return status.Errorf(codes.FailedPrecondition, "transaction is no longer eligible to %s", action)
	}
	return status.Errorf(codes.Internal, "failed to %s transaction", action)
}

func (s *PublicService) claimCooldown(namespace, userID, provider string) error {
	cooldown := s.cooldownDuration()
	if cooldown < 0 {
		return nil
	}
	if provider == "" {
		provider = "*"
	}
	key := namespace + "|" + userID + "|" + provider
	now := time.Now().UTC()

	s.cooldown.mu.Lock()
	defer s.cooldown.mu.Unlock()
	if last, ok := s.cooldown.lastSeen[key]; ok && now.Sub(last) < cooldown {
		return status.Error(codes.ResourceExhausted, "public sync cooldown is active")
	}
	s.cooldown.lastSeen[key] = now
	return nil
}

func (s *PublicService) cooldownDuration() time.Duration {
	if s.cfg == nil || s.cfg.PublicSyncCooldown == 0 {
		return defaultPublicSyncCooldown
	}
	return s.cfg.PublicSyncCooldown
}

func (s *PublicService) defaultPageSize() int32 {
	if s.cfg == nil || s.cfg.PublicSyncDefaultPageSize <= 0 {
		return defaultPublicSyncBatchSize
	}
	return s.cfg.PublicSyncDefaultPageSize
}

func (s *PublicService) maxPageSize() int32 {
	if s.cfg == nil || s.cfg.PublicSyncMaxPageSize <= 0 {
		return maxPublicSyncBatchSize
	}
	return s.cfg.PublicSyncMaxPageSize
}
