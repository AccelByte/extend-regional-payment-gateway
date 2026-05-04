package memory

import (
	"context"
	"testing"
	"time"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/store"
)

func TestAttachProviderTransaction(t *testing.T) {
	ctx := context.Background()
	mem := New()
	tx := &model.Transaction{
		ID:            "txn-1",
		ClientOrderID: "order-1",
		UserID:        "user-1",
		Namespace:     "ns",
		Status:        model.StatusPending,
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(time.Minute),
	}
	if err := mem.CreateTransaction(ctx, tx); err != nil {
		t.Fatalf("CreateTransaction error: %v", err)
	}

	if err := mem.AttachProviderTransaction(ctx, tx.ID, "provider_test", "", "provider-tx-1", "https://pay.example.test/1"); err != nil {
		t.Fatalf("AttachProviderTransaction error: %v", err)
	}

	got, err := mem.FindByID(ctx, tx.ID)
	if err != nil {
		t.Fatalf("FindByID error: %v", err)
	}
	if got.ProviderID != "provider_test" || got.ProviderTxID != "provider-tx-1" || got.PaymentURL == "" {
		t.Fatalf("unexpected provider attachment: provider=%q providerTxID=%q paymentURL=%q", got.ProviderID, got.ProviderTxID, got.PaymentURL)
	}
}

func TestAttachProviderTransactionRejectsSecondAttachment(t *testing.T) {
	ctx := context.Background()
	mem := New()
	tx := &model.Transaction{
		ID:            "txn-1",
		ClientOrderID: "order-1",
		UserID:        "user-1",
		Namespace:     "ns",
		Status:        model.StatusPending,
		ProviderTxID:  "provider-tx-1",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(time.Minute),
	}
	if err := mem.CreateTransaction(ctx, tx); err != nil {
		t.Fatalf("CreateTransaction error: %v", err)
	}

	err := mem.AttachProviderTransaction(ctx, tx.ID, "provider_test", "", "provider-tx-2", "https://pay.example.test/2")
	if err != store.ErrNoDocuments {
		t.Fatalf("expected ErrNoDocuments, got %v", err)
	}
}

func TestClearProviderTransactionIfPending(t *testing.T) {
	ctx := context.Background()
	mem := New()
	tx := &model.Transaction{
		ID:            "txn-1",
		ClientOrderID: "order-1",
		UserID:        "user-1",
		Namespace:     "ns",
		Status:        model.StatusPending,
		ProviderID:    "xendit",
		ProviderTxID:  "ps-1",
		PaymentURL:    "https://pay.example.test/ps-1",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(time.Minute),
	}
	if err := mem.CreateTransaction(ctx, tx); err != nil {
		t.Fatalf("CreateTransaction error: %v", err)
	}

	if err := mem.ClearProviderTransactionIfPending(ctx, tx.ID, "ps-1"); err != nil {
		t.Fatalf("ClearProviderTransactionIfPending error: %v", err)
	}
	got, err := mem.FindByID(ctx, tx.ID)
	if err != nil {
		t.Fatalf("FindByID error: %v", err)
	}
	if got.ProviderID != "" || got.ProviderTxID != "" || got.PaymentURL != "" {
		t.Fatalf("provider state was not cleared: %+v", got)
	}
}

func TestListTransactionsExactSearch(t *testing.T) {
	ctx := context.Background()
	mem := New()
	now := time.Now().UTC()
	seed := []*model.Transaction{
		{
			ID:            "txn-1",
			ClientOrderID: "order-1",
			UserID:        "user-1",
			Namespace:     "ns",
			ProviderID:    "provider_test",
			ProviderTxID:  "provider-1",
			ItemID:        "item-a",
			Status:        model.StatusFulfilled,
			CreatedAt:     now.Add(3 * time.Minute),
			ExpiresAt:     now.Add(time.Hour),
		},
		{
			ID:            "txn-2",
			ClientOrderID: "order-2",
			UserID:        "user-2",
			Namespace:     "ns",
			ProviderID:    "xendit",
			ProviderTxID:  "provider-2",
			ItemID:        "item-a",
			Status:        model.StatusPending,
			CreatedAt:     now.Add(2 * time.Minute),
			ExpiresAt:     now.Add(time.Hour),
		},
		{
			ID:            "txn-3",
			ClientOrderID: "order-3",
			UserID:        "user-1",
			Namespace:     "other-ns",
			ProviderID:    "provider_test",
			ProviderTxID:  "provider-1",
			ItemID:        "item-a",
			Status:        model.StatusFulfilled,
			CreatedAt:     now.Add(time.Minute),
			ExpiresAt:     now.Add(time.Hour),
		},
	}
	for _, tx := range seed {
		if err := mem.CreateTransaction(ctx, tx); err != nil {
			t.Fatalf("CreateTransaction(%s) error: %v", tx.ID, err)
		}
	}

	tests := []struct {
		name string
		q    store.ListQuery
		want []string
	}{
		{
			name: "transaction id",
			q:    store.ListQuery{Namespace: "ns", Search: "txn-1"},
			want: []string{"txn-1"},
		},
		{
			name: "provider tx id namespace scoped",
			q:    store.ListQuery{Namespace: "ns", Search: "provider-1"},
			want: []string{"txn-1"},
		},
		{
			name: "item id with provider and status filters",
			q:    store.ListQuery{Namespace: "ns", Search: "item-a", ProviderID: "provider_test", StatusFilter: model.StatusFulfilled},
			want: []string{"txn-1"},
		},
		{
			name: "item id returns newest first",
			q:    store.ListQuery{Namespace: "ns", Search: "item-a", PageSize: 10},
			want: []string{"txn-1", "txn-2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := mem.ListTransactions(ctx, tt.q)
			if err != nil {
				t.Fatalf("ListTransactions error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d transactions, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, tx := range got {
				if tx.ID != tt.want[i] {
					t.Fatalf("result[%d] = %q, want %q", i, tx.ID, tt.want[i])
				}
			}
		})
	}
}

func TestListTransactionsSearchPagination(t *testing.T) {
	ctx := context.Background()
	mem := New()
	now := time.Now().UTC()
	for i, id := range []string{"txn-1", "txn-2", "txn-3"} {
		if err := mem.CreateTransaction(ctx, &model.Transaction{
			ID:            id,
			ClientOrderID: "order-" + id,
			UserID:        "user-1",
			Namespace:     "ns",
			ItemID:        "item-paged",
			Status:        model.StatusPending,
			CreatedAt:     now.Add(time.Duration(3-i) * time.Minute),
			ExpiresAt:     now.Add(time.Hour),
		}); err != nil {
			t.Fatalf("CreateTransaction(%s) error: %v", id, err)
		}
	}

	first, next, err := mem.ListTransactions(ctx, store.ListQuery{Namespace: "ns", Search: "item-paged", PageSize: 2})
	if err != nil {
		t.Fatalf("first page error: %v", err)
	}
	if len(first) != 2 || next == "" {
		t.Fatalf("unexpected first page len=%d next=%q", len(first), next)
	}
	second, next, err := mem.ListTransactions(ctx, store.ListQuery{Namespace: "ns", Search: "item-paged", PageSize: 2, Cursor: next})
	if err != nil {
		t.Fatalf("second page error: %v", err)
	}
	if len(second) != 1 || second[0].ID != "txn-3" || next != "" {
		t.Fatalf("unexpected second page len=%d next=%q transactions=%+v", len(second), next, second)
	}
}
