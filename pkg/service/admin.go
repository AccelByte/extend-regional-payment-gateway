package service

import (
	"context"
	"log/slog"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/config"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/store"
	pb "github.com/accelbyte/extend-regional-payment-gateway/pkg/pb"
)

const (
	syncOutcomeFulfilled              = "FULFILLED"
	syncOutcomeFailed                 = "FAILED"
	syncOutcomeRefunded               = "REFUNDED"
	syncOutcomePartialRefundUnchanged = "PARTIAL_REFUND_UNCHANGED"
	syncOutcomeUnchanged              = "UNCHANGED"
	syncOutcomeUnsupported            = "UNSUPPORTED"
	syncOutcomeSyncFailed             = "SYNC_FAILED"
	maxSyncBatchSize                  = int32(100)
	defaultSyncBatchSize              = int32(20)
)

type FulfillmentReverser interface {
	ReverseFulfillment(ctx context.Context, tx *model.Transaction) error
}

// AdminService implements the gRPC AdminService.
type AdminService struct {
	pb.UnimplementedAdminServiceServer
	txStore  store.Store
	registry *adapter.Registry
	reverser FulfillmentReverser
	cfg      *config.Config
	syncer   *transactionReconciler
}

func NewAdminService(
	txStore store.Store,
	registry *adapter.Registry,
	reverser FulfillmentReverser,
	cfg *config.Config,
) *AdminService {
	svc := &AdminService{
		txStore:  txStore,
		registry: registry,
		reverser: reverser,
		cfg:      cfg,
	}
	svc.syncer = newTransactionReconciler(txStore, registry, reverser, cfg)
	return svc
}

func (s *AdminService) ListTransactions(ctx context.Context, req *pb.ListTransactionsRequest) (*pb.ListTransactionsResponse, error) {
	q := store.ListQuery{
		Namespace:    req.Namespace,
		UserID:       req.UserId,
		StatusFilter: statusFilterString(req.StatusFilter),
		ProviderID:   req.ProviderId,
		Search:       strings.TrimSpace(req.Search),
		PageSize:     req.PageSize,
		Cursor:       req.Cursor,
	}

	txns, nextCursor, err := s.txStore.ListTransactions(ctx, q)
	if err != nil {
		slog.Error("ListTransactions failed", "error", err)
		return nil, status.Error(codes.Internal, "failed to list transactions")
	}

	var responses []*pb.TransactionResponse
	for _, tx := range txns {
		responses = append(responses, txToTransactionResponse(tx))
	}

	return &pb.ListTransactionsResponse{
		Transactions: responses,
		NextCursor:   nextCursor,
	}, nil
}

func (s *AdminService) GetTransactionDetail(ctx context.Context, req *pb.GetTransactionRequest) (*pb.TransactionResponse, error) {
	tx, err := s.txStore.FindByID(ctx, req.TransactionId)
	if err == store.ErrNotFound {
		return nil, status.Error(codes.NotFound, "transaction not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to retrieve transaction")
	}
	if req.Namespace != "" && tx.Namespace != req.Namespace {
		return nil, status.Error(codes.NotFound, "transaction not found in namespace")
	}

	resp := txToTransactionResponse(tx)

	if tx.ProviderTxID != "" {
		providerID := resolveProviderID(tx)
		if prov, getErr := s.registry.Get(providerID); getErr == nil {
			if ps, psErr := prov.GetPaymentStatus(ctx, tx.ProviderTxID); psErr == nil {
				resp.ProviderStatus = string(ps.Status)
			}
		}
	}

	return resp, nil
}

func (s *AdminService) Refund(ctx context.Context, req *pb.RefundRequest) (*pb.RefundResponse, error) {
	if req.Namespace != "" {
		tx, err := s.txStore.FindByID(ctx, req.TransactionId)
		if err == store.ErrNotFound {
			return nil, status.Error(codes.NotFound, "transaction not found")
		}
		if err != nil {
			return nil, status.Error(codes.Internal, "failed to retrieve transaction")
		}
		if tx.Namespace != req.Namespace {
			return nil, status.Error(codes.NotFound, "transaction not found in namespace")
		}
	}

	// Step 1: Atomic claim — FULFILLED → REFUNDING
	// Filter allows re-entry from REFUND_FAILED for retry semantics.
	tx, err := s.txStore.AtomicClaimRefunding(ctx, req.TransactionId, req.Reason)
	if err == store.ErrNoDocuments {
		return nil, status.Error(codes.FailedPrecondition, "transaction is not eligible for refund (not FULFILLED, or already REFUNDING/REFUNDED)")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to claim refund")
	}
	// Step 2: Call provider refund API unless a previous attempt already
	// completed provider-side refund and failed during AGS reversal.
	providerID := resolveProviderID(tx)
	prov, regErr := s.registry.Get(providerID)
	if regErr != nil {
		if markErr := s.txStore.MarkRefundFailed(ctx, tx.ID, "unknown provider: "+providerID); markErr != nil {
			slog.Error("Refund: MarkRefundFailed failed", "txn_id", tx.ID, "error", markErr)
		}
		return nil, status.Errorf(codes.Internal, "unknown provider: %s", providerID)
	}

	if tx.Refund == nil || !tx.Refund.ProviderRefunded {
		if refundErr := prov.RefundPayment(ctx, tx.ID, tx.ProviderTxID, tx.Amount, tx.CurrencyCode); refundErr != nil {
			slog.Error("Refund: provider RefundPayment failed", "txn_id", tx.ID, "error", refundErr)
			if markErr := s.txStore.MarkRefundFailed(ctx, tx.ID, "provider refund failed: "+refundErr.Error()); markErr != nil {
				slog.Error("Refund: MarkRefundFailed failed", "txn_id", tx.ID, "error", markErr)
			}
			return nil, status.Errorf(codes.Internal, "provider refund failed: %v", refundErr)
		}
		if markErr := s.txStore.MarkRefundProviderSucceeded(ctx, tx.ID); markErr != nil {
			slog.Error("Refund: MarkRefundProviderSucceeded failed", "txn_id", tx.ID, "error", markErr)
			return nil, status.Error(codes.Internal, "failed to persist provider refund status")
		}
	}

	// Step 3: Reverse AGS fulfillment
	if reverseErr := reverseAGSRefund(ctx, s.txStore, s.reverser, tx); reverseErr != nil {
		return nil, reverseErr
	}

	slog.Info("refund completed", "txn_id", tx.ID, "amount", tx.Amount, "currency_code", tx.CurrencyCode)
	return &pb.RefundResponse{
		TransactionId: tx.ID,
		Success:       true,
	}, nil
}

func (s *AdminService) SyncTransaction(ctx context.Context, req *pb.SyncTransactionRequest) (*pb.SyncTransactionResponse, error) {
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
	if req.Namespace != "" && tx.Namespace != req.Namespace {
		return nil, status.Error(codes.NotFound, "transaction not found in namespace")
	}
	result := s.syncer.syncTransaction(ctx, tx)
	return &pb.SyncTransactionResponse{Result: result}, nil
}

func (s *AdminService) SyncTransactions(ctx context.Context, req *pb.SyncTransactionsRequest) (*pb.SyncTransactionsResponse, error) {
	if strings.TrimSpace(req.Namespace) == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace is required")
	}
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = defaultSyncBatchSize
	}
	if pageSize > maxSyncBatchSize {
		pageSize = maxSyncBatchSize
	}

	txns, nextCursor, err := s.txStore.ListTransactions(ctx, store.ListQuery{
		Namespace:    req.Namespace,
		ProviderID:   req.ProviderId,
		StatusFilter: statusFilterString(req.StatusFilter),
		PageSize:     pageSize,
		Cursor:       req.Cursor,
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

func statusFilterString(s pb.TransactionStatus) string {
	switch s {
	case pb.TransactionStatus_PENDING:
		return model.StatusPending
	case pb.TransactionStatus_FULFILLING:
		return model.StatusFulfilling
	case pb.TransactionStatus_FULFILLED:
		return model.StatusFulfilled
	case pb.TransactionStatus_FAILED:
		return model.StatusFailed
	case pb.TransactionStatus_CANCELED:
		return model.StatusCanceled
	case pb.TransactionStatus_EXPIRED:
		return model.StatusExpired
	default:
		return ""
	}
}

// resolveProviderID returns the registry key for a transaction's adapter.
func resolveProviderID(tx *model.Transaction) string {
	return tx.ProviderID
}
