package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AccelByte/accelbyte-go-sdk/services-api/pkg/service/iam"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/checkout"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
	memstore "github.com/accelbyte/extend-regional-payment-gateway/internal/store/memory"
	"github.com/accelbyte/extend-regional-payment-gateway/pkg/common"
	"github.com/accelbyte/extend-regional-payment-gateway/pkg/service"
)

func TestRenderPaymentResultPage(t *testing.T) {
	withTransaction := renderPaymentResultPage("/payment", "txn-123")
	for _, want := range []string{
		"AccelByte Payment",
		"Payment Processing",
		"Payment Canceled",
		"Payment Expired",
		"txn-123",
		"/payment-result/status?transactionId=",
	} {
		if !strings.Contains(withTransaction, want) {
			t.Fatalf("payment result page missing %q", want)
		}
	}

	withoutTransaction := renderPaymentResultPage("/payment", "")
	if !strings.Contains(withoutTransaction, "status-missing") {
		t.Fatal("missing transaction page should render missing status state")
	}
	if !strings.Contains(withoutTransaction, `class="details hidden"`) {
		t.Fatal("missing transaction page should hide transaction details")
	}
}

func TestPaymentResultStatusEndpoint(t *testing.T) {
	txStore := memstore.New()
	now := time.Now().UTC()
	for _, tx := range []*model.Transaction{
		{
			ID:                  "txn-pending",
			ClientOrderID:       "order-pending",
			UserID:              "user-1",
			Namespace:           "ns",
			ProviderID:          model.ProviderKomoju,
			ProviderDisplayName: "KOMOJU",
			ItemName:            "Crystal Pack",
			ItemID:              "item-1",
			Quantity:            2,
			Amount:              21000,
			CurrencyCode:        "IDR",
			Status:              model.StatusPending,
			CreatedAt:           now,
			ExpiresAt:           now.Add(time.Hour),
			UpdatedAt:           now,
		},
		{
			ID:                  "txn-fulfilled",
			ClientOrderID:       "order-fulfilled",
			UserID:              "user-1",
			Namespace:           "ns",
			ProviderID:          model.ProviderKomoju,
			ProviderDisplayName: "KOMOJU",
			ItemName:            "Coin Bundle",
			ItemID:              "item-2",
			Quantity:            1,
			Amount:              25000,
			CurrencyCode:        "IDR",
			Status:              model.StatusFulfilled,
			CreatedAt:           now,
			ExpiresAt:           now.Add(time.Hour),
			UpdatedAt:           now,
		},
		{
			ID:                  "txn-xendit",
			ClientOrderID:       "order-xendit",
			UserID:              "user-1",
			Namespace:           "ns",
			ProviderID:          model.ProviderXendit,
			ProviderDisplayName: "XENDIT",
			ItemName:            "Regional Pack",
			ItemID:              "item-xendit",
			Quantity:            1,
			Amount:              120000,
			CurrencyCode:        "IDR",
			Status:              model.StatusPending,
			CreatedAt:           now,
			ExpiresAt:           now.Add(time.Hour),
			UpdatedAt:           now,
		},
		{
			ID:                  "txn-failed",
			ClientOrderID:       "order-failed",
			UserID:              "user-1",
			Namespace:           "ns",
			ProviderID:          "provider_shopee_pay",
			ProviderDisplayName: "Shopee Pay",
			ItemName:            "Starter Pack",
			ItemID:              "item-3",
			Quantity:            1,
			Amount:              5000,
			CurrencyCode:        "IDR",
			Status:              model.StatusFailed,
			CreatedAt:           now,
			ExpiresAt:           now.Add(time.Hour),
			UpdatedAt:           now,
		},
		{
			ID:                  "txn-canceled",
			ClientOrderID:       "order-canceled",
			UserID:              "user-1",
			Namespace:           "ns",
			ProviderID:          model.ProviderXendit,
			ProviderDisplayName: "XENDIT",
			ItemName:            "Canceled Pack",
			ItemID:              "item-4",
			Quantity:            1,
			Amount:              9000,
			CurrencyCode:        "IDR",
			Status:              model.StatusCanceled,
			CreatedAt:           now,
			ExpiresAt:           now.Add(time.Hour),
			UpdatedAt:           now,
		},
		{
			ID:                  "txn-expired",
			ClientOrderID:       "order-expired",
			UserID:              "user-1",
			Namespace:           "ns",
			ProviderID:          model.ProviderXendit,
			ProviderDisplayName: "XENDIT",
			ItemName:            "Expired Pack",
			ItemID:              "item-5",
			Quantity:            1,
			Amount:              7000,
			CurrencyCode:        "IDR",
			Status:              model.StatusExpired,
			CreatedAt:           now,
			ExpiresAt:           now.Add(time.Hour),
			UpdatedAt:           now,
		},
	} {
		if err := txStore.CreateTransaction(context.Background(), tx); err != nil {
			t.Fatalf("seed transaction %s: %v", tx.ID, err)
		}
	}

	paymentSvc := service.NewPaymentService(txStore, adapter.NewRegistry(), nil, nil)
	server := newHTTPServer(
		":0",
		http.NotFoundHandler(),
		nil,
		paymentSvc,
		nil,
		nil,
		adapter.NewRegistry(),
		checkout.NewStore(context.Background()),
		nil,
		"/payment",
		"https://example.test",
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	tests := []struct {
		transactionID string
		wantStatus    string
		wantProvider  string
		wantAmount    string
	}{
		{"txn-pending", "PENDING", "KOMOJU", "21.000 IDR"},
		{"txn-fulfilled", "FULFILLED", "KOMOJU", "25.000 IDR"},
		{"txn-xendit", "PENDING", "XENDIT", "120.000 IDR"},
		{"txn-failed", "FAILED", "Shopee Pay", "5.000 IDR"},
		{"txn-canceled", "CANCELED", "XENDIT", "9.000 IDR"},
		{"txn-expired", "EXPIRED", "XENDIT", "7.000 IDR"},
	}
	for _, tt := range tests {
		t.Run(tt.transactionID, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/payment/payment-result/status?transactionId="+tt.transactionID, nil)
			rec := httptest.NewRecorder()
			server.Handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			var body paymentResultStatusResponse
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.Status != tt.wantStatus || body.Provider != tt.wantProvider || body.Amount != tt.wantAmount {
				t.Fatalf("unexpected response: %+v", body)
			}
		})
	}
}

func TestAdminDirectHandlerRequiresReadPermission(t *testing.T) {
	previous := common.Validator
	t.Cleanup(func() { common.Validator = previous })

	txStore := memstore.New()
	now := time.Now().UTC()
	if err := txStore.CreateTransaction(context.Background(), &model.Transaction{
		ID:            "txn-admin",
		ClientOrderID: "order-admin",
		UserID:        "user-1",
		Namespace:     "ns",
		ProviderID:    model.ProviderKomoju,
		ItemID:        "item-1",
		Quantity:      1,
		Amount:        10000,
		CurrencyCode:  "IDR",
		Status:        model.StatusPending,
		CreatedAt:     now,
		UpdatedAt:     now,
		ExpiresAt:     now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed transaction: %v", err)
	}

	adminSvc := service.NewAdminService(txStore, adapter.NewRegistry(), nil, nil)
	server := newHTTPServer(
		":0",
		http.NotFoundHandler(),
		nil,
		nil,
		nil,
		adminSvc,
		adapter.NewRegistry(),
		checkout.NewStore(context.Background()),
		nil,
		"/payment",
		"https://example.test",
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	common.Validator = &adminHTTPAuthValidator{}
	missingAuthReq := httptest.NewRequest(http.MethodGet, "/payment/v1/admin/namespace/ns/transactions", nil)
	missingAuthRec := httptest.NewRecorder()
	server.Handler.ServeHTTP(missingAuthRec, missingAuthReq)
	if missingAuthRec.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth code = %d, want 401: %s", missingAuthRec.Code, missingAuthRec.Body.String())
	}

	validator := &adminHTTPAuthValidator{}
	common.Validator = validator
	req := httptest.NewRequest(http.MethodGet, "/payment/v1/admin/namespace/ns/transactions?search=txn-admin", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin list code = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if validator.permission == nil || validator.permission.Action != 2 || validator.permission.Resource != "ADMIN:NAMESPACE:ns:PAYMENT:TRANSACTION" {
		t.Fatalf("unexpected permission: %+v", validator.permission)
	}
}

type adminHTTPAuthValidator struct {
	permission *iam.Permission
}

func (v *adminHTTPAuthValidator) Initialize(ctx ...context.Context) error { return nil }

func (v *adminHTTPAuthValidator) Validate(_ string, permission *iam.Permission, _ *string, _ *string) error {
	v.permission = permission
	return nil
}
