package memory

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/store"
)

// Store is a thread-safe in-memory implementation for unit tests.
// Atomic methods hold the write lock across the read-check-write sequence,
// preserving the same compare-and-swap semantics as DocumentDB.
type Store struct {
	mu   sync.RWMutex
	data map[string]*model.Transaction
}

func New() *Store {
	return &Store{data: make(map[string]*model.Transaction)}
}

func (s *Store) CreateTransaction(_ context.Context, tx *model.Transaction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.data {
		if existing.ClientOrderID == tx.ClientOrderID {
			return store.ErrDuplicateClientOrderID
		}
	}
	cp := *tx
	s.data[tx.ID] = &cp
	return nil
}

func (s *Store) FindByID(_ context.Context, id string) (*model.Transaction, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tx, ok := s.data[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *tx
	return &cp, nil
}

func (s *Store) FindByClientOrderID(_ context.Context, clientOrderID string) (*model.Transaction, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, tx := range s.data {
		if tx.ClientOrderID == clientOrderID {
			cp := *tx
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *Store) AtomicClaimFulfilling(_ context.Context, txnID, providerTxID string) (*model.Transaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, ok := s.data[txnID]
	if !ok {
		return nil, store.ErrNotFound
	}
	if tx.Status != model.StatusPending {
		return nil, store.ErrNoDocuments
	}
	tx.Status = model.StatusFulfilling
	tx.ProviderTxID = providerTxID
	tx.UpdatedAt = time.Now().UTC()
	cp := *tx
	return &cp, nil
}

func (s *Store) CommitFulfilled(_ context.Context, txnID, providerStatus string, deleteAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, ok := s.data[txnID]
	if !ok {
		return store.ErrNotFound
	}
	tx.Status = model.StatusFulfilled
	tx.ProviderStatus = providerStatus
	tx.DeleteAt = deleteAt
	tx.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *Store) MarkFailed(_ context.Context, txnID, reason string, deleteAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, ok := s.data[txnID]
	if !ok {
		return store.ErrNotFound
	}
	tx.Status = model.StatusFailed
	tx.FailureReason = reason
	tx.DeleteAt = deleteAt
	tx.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *Store) MarkCanceledIfPending(_ context.Context, txnID, reason, providerStatus string, deleteAt time.Time) error {
	return s.markTerminalIfPending(txnID, model.StatusCanceled, reason, providerStatus, deleteAt)
}

func (s *Store) MarkExpiredIfPending(_ context.Context, txnID, reason, providerStatus string, deleteAt time.Time) error {
	return s.markTerminalIfPending(txnID, model.StatusExpired, reason, providerStatus, deleteAt)
}

func (s *Store) markTerminalIfPending(txnID, terminalStatus, reason, providerStatus string, deleteAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, ok := s.data[txnID]
	if !ok {
		return store.ErrNotFound
	}
	if tx.Status != model.StatusPending {
		return store.ErrNoDocuments
	}
	tx.Status = terminalStatus
	tx.FailureReason = reason
	tx.ProviderStatus = providerStatus
	tx.DeleteAt = deleteAt
	tx.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *Store) AttachProviderTransaction(_ context.Context, txnID, provider, customProviderName, providerTxID, paymentURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, ok := s.data[txnID]
	if !ok {
		return store.ErrNotFound
	}
	if tx.Status != model.StatusPending || tx.ProviderTxID != "" {
		return store.ErrNoDocuments
	}
	tx.Provider = provider
	tx.CustomProviderName = customProviderName
	tx.ProviderTxID = providerTxID
	tx.PaymentURL = paymentURL
	tx.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *Store) ClearProviderTransactionIfPending(_ context.Context, txnID, providerTxID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, ok := s.data[txnID]
	if !ok {
		return store.ErrNotFound
	}
	if tx.Status != model.StatusPending || tx.ProviderTxID == "" || tx.ProviderTxID != providerTxID {
		return store.ErrNoDocuments
	}
	tx.Provider = ""
	tx.CustomProviderName = ""
	tx.ProviderTxID = ""
	tx.PaymentURL = ""
	tx.ProviderStatus = ""
	tx.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *Store) UpdateProviderTransactionID(_ context.Context, txnID, providerTxID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, ok := s.data[txnID]
	if !ok {
		return store.ErrNotFound
	}
	tx.ProviderTxID = providerTxID
	tx.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *Store) DeleteTransaction(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, id)
	return nil
}

func (s *Store) ListTransactions(_ context.Context, q store.ListQuery) ([]*model.Transaction, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	pageSize := q.PageSize
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}
	search := strings.TrimSpace(q.Search)

	all := make([]*model.Transaction, 0, len(s.data))
	for _, tx := range s.data {
		all = append(all, tx)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].ID > all[j].ID
		}
		return all[i].CreatedAt.After(all[j].CreatedAt)
	})

	var results []*model.Transaction
	pastCursor := q.Cursor == ""
	for _, tx := range all {
		if !pastCursor {
			if tx.ID == q.Cursor {
				pastCursor = true
			}
			continue
		}
		if q.Namespace != "" && tx.Namespace != q.Namespace {
			continue
		}
		if q.UserID != "" && tx.UserID != q.UserID {
			continue
		}
		if q.StatusFilter != "" && tx.Status != q.StatusFilter {
			continue
		}
		if len(q.StatusFilters) > 0 {
			matched := false
			for _, status := range q.StatusFilters {
				if tx.Status == status {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		if q.Provider != "" && tx.Provider != q.Provider {
			continue
		}
		if search != "" && tx.ID != search && tx.ProviderTxID != search && tx.ItemID != search {
			continue
		}
		cp := *tx
		results = append(results, &cp)
	}

	var nextCursor string
	if int32(len(results)) > pageSize {
		results = results[:pageSize]
		nextCursor = results[len(results)-1].ID
	}
	return results, nextCursor, nil
}

func (s *Store) CountPendingByUser(_ context.Context, namespace, userID string) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var count int64
	for _, tx := range s.data {
		if tx.Namespace == namespace && tx.UserID == userID && tx.Status == model.StatusPending {
			count++
		}
	}
	return count, nil
}

func (s *Store) FindStuckFulfilling(_ context.Context, olderThan time.Time) ([]*model.Transaction, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var results []*model.Transaction
	for _, tx := range s.data {
		if tx.Status == model.StatusFulfilling && tx.UpdatedAt.Before(olderThan) {
			cp := *tx
			results = append(results, &cp)
		}
	}
	return results, nil
}

func (s *Store) FindStuckPending(_ context.Context, olderThan time.Time) ([]*model.Transaction, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now().UTC()
	var results []*model.Transaction
	for _, tx := range s.data {
		if tx.Status == model.StatusPending && tx.ExpiresAt.After(now) && tx.CreatedAt.Before(olderThan) {
			cp := *tx
			results = append(results, &cp)
		}
	}
	return results, nil
}

func (s *Store) FindExpiredPending(_ context.Context, now time.Time) ([]*model.Transaction, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var results []*model.Transaction
	for _, tx := range s.data {
		if tx.Status == model.StatusPending && !tx.ExpiresAt.IsZero() && !tx.ExpiresAt.After(now) {
			cp := *tx
			results = append(results, &cp)
		}
	}
	return results, nil
}

func (s *Store) IncrementRetries(_ context.Context, txnID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, ok := s.data[txnID]
	if !ok {
		return store.ErrNotFound
	}
	tx.Retries++
	tx.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *Store) ResetToPending(_ context.Context, txnID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, ok := s.data[txnID]
	if !ok {
		return store.ErrNotFound
	}
	tx.Status = model.StatusPending
	tx.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *Store) AtomicClaimRefunding(_ context.Context, txnID, reason string) (*model.Transaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, ok := s.data[txnID]
	if !ok {
		return nil, store.ErrNotFound
	}
	if tx.Status != model.StatusFulfilled {
		return nil, store.ErrNoDocuments
	}
	if tx.Refund != nil && tx.Refund.Status != model.RefundStatusRefundFailed {
		return nil, store.ErrNoDocuments
	}
	now := time.Now().UTC()
	if tx.Refund == nil {
		tx.Refund = &model.RefundSubDoc{CreatedAt: now}
	}
	tx.Refund.Status = model.RefundStatusRefunding
	tx.Refund.Reason = reason
	tx.Refund.FailureReason = ""
	tx.Refund.UpdatedAt = now
	tx.UpdatedAt = now
	cp := *tx
	return &cp, nil
}

func (s *Store) AtomicClaimExternalRefunding(_ context.Context, txnID, reason string) (*model.Transaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, ok := s.data[txnID]
	if !ok {
		return nil, store.ErrNotFound
	}
	if tx.Status != model.StatusFulfilled {
		return nil, store.ErrNoDocuments
	}
	if tx.Refund != nil && tx.Refund.Status != model.RefundStatusRefundFailed {
		return nil, store.ErrNoDocuments
	}
	now := time.Now().UTC()
	if tx.Refund == nil {
		tx.Refund = &model.RefundSubDoc{CreatedAt: now}
	}
	tx.Refund.Status = model.RefundStatusRefunding
	tx.Refund.Reason = reason
	tx.Refund.FailureReason = ""
	tx.Refund.ProviderRefunded = true
	tx.Refund.UpdatedAt = now
	tx.UpdatedAt = now
	cp := *tx
	return &cp, nil
}

func (s *Store) MarkRefundProviderSucceeded(_ context.Context, txnID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, ok := s.data[txnID]
	if !ok {
		return store.ErrNotFound
	}
	if tx.Refund != nil {
		tx.Refund.ProviderRefunded = true
		tx.Refund.UpdatedAt = time.Now().UTC()
	}
	tx.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *Store) CommitRefund(_ context.Context, txnID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, ok := s.data[txnID]
	if !ok {
		return store.ErrNotFound
	}
	if tx.Refund != nil {
		tx.Refund.Status = model.RefundStatusRefunded
		tx.Refund.ProviderRefunded = true
		tx.Refund.UpdatedAt = time.Now().UTC()
	}
	tx.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *Store) MarkRefundFailed(_ context.Context, txnID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, ok := s.data[txnID]
	if !ok {
		return store.ErrNotFound
	}
	if tx.Refund != nil {
		tx.Refund.Status = model.RefundStatusRefundFailed
		tx.Refund.FailureReason = reason
		tx.Refund.UpdatedAt = time.Now().UTC()
	}
	tx.UpdatedAt = time.Now().UTC()
	return nil
}
