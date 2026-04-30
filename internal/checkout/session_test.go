package checkout

import (
	"context"
	"testing"
	"time"
)

func TestStoreGetValidForSelectionDoesNotConsumeSession(t *testing.T) {
	store := NewStore(context.Background())
	sessionID := store.Create(&Session{
		TransactionID: "txn-1",
		UserID:        "user-1",
		ItemName:      "Starter Pack",
		ItemID:        "starter-pack",
		Quantity:      3,
		UnitPrice:     10000,
		TotalPrice:    30000,
		CurrencyCode:  "IDR",
		ExpiresAt:     time.Now().Add(time.Minute),
	})

	sess, ok := store.GetValidForSelection(sessionID)
	if !ok {
		t.Fatal("expected first selection lookup to succeed")
	}
	if sess.TransactionID != "txn-1" {
		t.Fatalf("unexpected transaction id: %s", sess.TransactionID)
	}
	if sess.ItemName != "Starter Pack" || sess.ItemID != "starter-pack" || sess.Quantity != 3 {
		t.Fatalf("unexpected checkout details: %+v", sess)
	}

	if _, ok := store.GetValidForSelection(sessionID); !ok {
		t.Fatal("expected second selection lookup to still succeed")
	}
}

func TestStoreGetValidForSelectionRejectsExpiredSession(t *testing.T) {
	store := NewStore(context.Background())
	sessionID := store.Create(&Session{
		TransactionID: "txn-1",
		UserID:        "user-1",
		ExpiresAt:     time.Now().Add(-time.Minute),
	})

	if _, ok := store.GetValidForSelection(sessionID); ok {
		t.Fatal("expected expired selection lookup to fail")
	}

	if _, ok := store.Get(sessionID); !ok {
		t.Fatal("expected expired session to remain readable for terminal rendering")
	}
}
