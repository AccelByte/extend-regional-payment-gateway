package store

import (
	"context"
	"errors"
	"time"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
)

var (
	ErrNotFound               = errors.New("transaction not found")
	ErrDuplicateClientOrderID = errors.New("duplicate client_order_id")
	ErrNoDocuments            = errors.New("no documents matched the atomic filter")
)

// ListQuery holds filters and pagination parameters for ListTransactions.
type ListQuery struct {
	Namespace     string
	UserID        string
	StatusFilter  string
	StatusFilters []string
	Provider      string
	Search        string
	PageSize      int32
	Cursor        string // last _id seen; empty = first page
}

// Store is the storage abstraction over DocumentDB / in-memory.
type Store interface {
	CreateTransaction(ctx context.Context, tx *model.Transaction) error
	FindByID(ctx context.Context, id string) (*model.Transaction, error)
	FindByClientOrderID(ctx context.Context, clientOrderID string) (*model.Transaction, error)

	// AtomicClaimFulfilling does FindOneAndUpdate({_id, status:PENDING} → FULFILLING).
	// Returns ErrNoDocuments if the filter does not match (already FULFILLING/FULFILLED/FAILED).
	// Returns ErrNotFound if the transaction does not exist at all.
	AtomicClaimFulfilling(ctx context.Context, txnID, providerTxID string) (*model.Transaction, error)

	CommitFulfilled(ctx context.Context, txnID, providerStatus string, deleteAt time.Time) error
	MarkFailed(ctx context.Context, txnID, reason string, deleteAt time.Time) error
	MarkCanceledIfPending(ctx context.Context, txnID, reason, providerStatus string, deleteAt time.Time) error
	MarkExpiredIfPending(ctx context.Context, txnID, reason, providerStatus string, deleteAt time.Time) error
	AttachProviderTransaction(ctx context.Context, txnID, provider, customProviderName, providerTxID, paymentURL string) error
	ClearProviderTransactionIfPending(ctx context.Context, txnID, providerTxID string) error
	UpdateProviderTransactionID(ctx context.Context, txnID, providerTxID string) error
	DeleteTransaction(ctx context.Context, id string) error

	ListTransactions(ctx context.Context, q ListQuery) ([]*model.Transaction, string, error)
	CountPendingByUser(ctx context.Context, namespace, userID string) (int64, error)

	// Scheduler helpers
	FindStuckFulfilling(ctx context.Context, olderThan time.Time) ([]*model.Transaction, error)
	FindStuckPending(ctx context.Context, olderThan time.Time) ([]*model.Transaction, error)
	FindExpiredPending(ctx context.Context, now time.Time) ([]*model.Transaction, error)
	IncrementRetries(ctx context.Context, txnID string) error
	ResetToPending(ctx context.Context, txnID string) error

	// Refund
	AtomicClaimRefunding(ctx context.Context, txnID, reason string) (*model.Transaction, error)
	AtomicClaimExternalRefunding(ctx context.Context, txnID, reason string) (*model.Transaction, error)
	MarkRefundProviderSucceeded(ctx context.Context, txnID string) error
	CommitRefund(ctx context.Context, txnID string) error
	MarkRefundFailed(ctx context.Context, txnID, reason string) error
}
