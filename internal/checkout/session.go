package checkout

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Session holds the context needed to create a payment intent after the player
// selects a provider on the hosted checkout page.
type Session struct {
	ID            string
	TransactionID string
	UserID        string
	Description   string
	ItemName      string
	ItemID        string
	Quantity      int32
	UnitPrice     int64
	TotalPrice    int64
	CurrencyCode  string
	ExpiresAt     time.Time
}

// Store is a thread-safe in-memory session store with expiry.
type Store struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

// NewStore creates a Store and starts a background goroutine that sweeps
// expired sessions every 5 minutes until ctx is cancelled.
func NewStore(ctx context.Context) *Store {
	s := &Store{sessions: make(map[string]*Session)}
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.sweep()
			}
		}
	}()
	return s
}

// Create stores a new session and returns its generated ID.
func (s *Store) Create(sess *Session) string {
	sess.ID = uuid.New().String()
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()
	return sess.ID
}

// Get returns the session by ID. Expired sessions are still returned so the
// checkout page can render the durable transaction's terminal state.
func (s *Store) Get(id string) (*Session, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[id]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return sess, true
}

// GetValidForSelection returns a non-expired session for provider selection
// without consuming it, so players can return to the checkout page after
// visiting a provider-hosted payment page.
func (s *Store) GetValidForSelection(id string) (*Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	if !ok || time.Now().After(sess.ExpiresAt) {
		return nil, false
	}
	return sess, true
}

func (s *Store) sweep() {
	// Keep sessions in memory after their page expiry so checkout URLs can
	// still render canceled/expired transaction state. Provider selection uses
	// Claim, which continues to enforce ExpiresAt.
	now := time.Now().Add(-24 * time.Hour)
	s.mu.Lock()
	for id, sess := range s.sessions {
		if now.After(sess.ExpiresAt) {
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()
}
