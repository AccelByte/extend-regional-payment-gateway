package komoju

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
)

func testConfig(baseURL string) *Config {
	return &Config{
		SecretKey:         "sk_test",
		WebhookSecret:     "whsec_test",
		APIVersion:        defaultAPIVersion,
		APIBaseURL:        baseURL,
		DefaultLocale:     defaultLocale,
		AllowedCurrencies: parseCodeSet(defaultCurrenciesCSV),
	}
}

func TestLoad(t *testing.T) {
	t.Setenv("KOMOJU_SECRET_KEY", "sk_test")
	t.Setenv("KOMOJU_WEBHOOK_SECRET", "secret")
	t.Setenv("KOMOJU_API_VERSION", "2025-01-28")
	t.Setenv("KOMOJU_DEFAULT_LOCALE", "ja")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.SecretKey != "sk_test" || cfg.WebhookSecret != "secret" {
		t.Fatalf("unexpected secrets: %+v", cfg)
	}
	if cfg.DefaultLocale != "ja" {
		t.Fatalf("DefaultLocale = %q, want ja", cfg.DefaultLocale)
	}
	for _, currency := range []string{"JPY", "USD", "EUR", "TWD", "KRW", "PLN", "GBP", "HKD", "SGD", "NZD", "AUD", "IDR", "MYR", "PHP", "THB", "CNY", "BRL", "CHF", "CAD", "VND"} {
		if _, ok := cfg.AllowedCurrencies[currency]; !ok {
			t.Fatalf("expected default currency %s to be allowed", currency)
		}
	}
}

func TestLoadRequiresWebhookSecret(t *testing.T) {
	t.Setenv("KOMOJU_SECRET_KEY", "sk_test")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "KOMOJU_WEBHOOK_SECRET") {
		t.Fatalf("expected webhook secret error, got %v", err)
	}
}

func TestCreatePaymentIntentCreatesHostedPageSession(t *testing.T) {
	var gotAuthUser, gotAuthPass string
	var gotAPIVersion, gotIdempotency string
	var gotReq createSessionRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		gotAuthUser, gotAuthPass, _ = r.BasicAuth()
		gotAPIVersion = r.Header.Get(apiVersionHeader)
		gotIdempotency = r.Header.Get(idempotencyHeader)
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"sess_123","session_url":"https://komoju.test/sessions/sess_123","status":"pending","amount":1000,"currency":"JPY"}`)) //nolint:errcheck
	}))
	defer server.Close()

	a, err := New(testConfig(server.URL))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	intent, err := a.CreatePaymentIntent(context.Background(), adapter.PaymentInitRequest{
		InternalOrderID: "txn-123",
		UserID:          "user-1",
		RegionCode:      "jp",
		Amount:          30000,
		CurrencyCode:    "idr",
		ReturnURL:       "https://example.test/payment-result?transactionId=txn-123",
		ExpiryDuration:  48 * time.Hour,
	})
	if err != nil {
		t.Fatalf("CreatePaymentIntent returned error: %v", err)
	}
	if intent.ProviderTransactionID != "sess_123" || intent.PaymentURL == "" {
		t.Fatalf("unexpected intent: %+v", intent)
	}
	if gotAuthUser != "sk_test" || gotAuthPass != "" {
		t.Fatalf("unexpected basic auth user/pass: %q/%q", gotAuthUser, gotAuthPass)
	}
	if gotAPIVersion != defaultAPIVersion {
		t.Fatalf("API version = %q, want %q", gotAPIVersion, defaultAPIVersion)
	}
	if gotIdempotency != "session-txn-123" {
		t.Fatalf("idempotency = %q, want session-txn-123", gotIdempotency)
	}
	if gotReq.Mode != "payment" || gotReq.Amount != 3000000 || gotReq.Currency != "IDR" {
		t.Fatalf("unexpected session request: %+v", gotReq)
	}
	if gotReq.ExpiresInSeconds != maxSessionExpirySecond {
		t.Fatalf("ExpiresInSeconds = %d, want cap %d", gotReq.ExpiresInSeconds, maxSessionExpirySecond)
	}
	if gotReq.PaymentData.ExternalOrderNum != "txn-123" || gotReq.PaymentData.Capture != "auto" {
		t.Fatalf("unexpected payment_data: %+v", gotReq.PaymentData)
	}
	if gotReq.Metadata["internalOrderId"] != "txn-123" || gotReq.Metadata["userId"] != "user-1" || gotReq.Metadata["regionCode"] != "JP" {
		t.Fatalf("unexpected metadata: %+v", gotReq.Metadata)
	}
}

func TestCreatePaymentIntentScalesJPYByCurrencyScale(t *testing.T) {
	var gotReq createSessionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"sess_123","session_url":"https://komoju.test/sessions/sess_123"}`)) //nolint:errcheck
	}))
	defer server.Close()

	a, err := New(testConfig(server.URL))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	_, err = a.CreatePaymentIntent(context.Background(), adapter.PaymentInitRequest{
		InternalOrderID: "txn-123",
		Amount:          100,
		CurrencyCode:    "JPY",
		ExpiryDuration:  time.Minute,
	})
	if err != nil {
		t.Fatalf("CreatePaymentIntent returned error: %v", err)
	}
	if gotReq.Amount != 100 {
		t.Fatalf("JPY amount = %d, want 100", gotReq.Amount)
	}
}

func TestCreatePaymentIntentScalesUSDByCurrencyScale(t *testing.T) {
	var gotReq createSessionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"sess_123","session_url":"https://komoju.test/sessions/sess_123"}`)) //nolint:errcheck
	}))
	defer server.Close()

	a, err := New(testConfig(server.URL))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	_, err = a.CreatePaymentIntent(context.Background(), adapter.PaymentInitRequest{
		InternalOrderID: "txn-123",
		Amount:          100,
		CurrencyCode:    "USD",
		ExpiryDuration:  time.Minute,
	})
	if err != nil {
		t.Fatalf("CreatePaymentIntent returned error: %v", err)
	}
	if gotReq.Amount != 10000 {
		t.Fatalf("USD amount = %d, want 10000", gotReq.Amount)
	}
}

func TestCreatePaymentIntentScalesByCurrencyScale(t *testing.T) {
	tests := []struct {
		name     string
		amount   int64
		currency string
	}{
		{name: "JPY", amount: 100, currency: "JPY"},
		{name: "IDR", amount: 10000, currency: "IDR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotReq createSessionRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"sess_123","session_url":"https://komoju.test/sessions/sess_123"}`)) //nolint:errcheck
			}))
			defer server.Close()

			a, err := New(testConfig(server.URL))
			if err != nil {
				t.Fatalf("New returned error: %v", err)
			}
			_, err = a.CreatePaymentIntent(context.Background(), adapter.PaymentInitRequest{
				InternalOrderID: "txn-123",
				Amount:          tt.amount,
				CurrencyCode:    tt.currency,
				ExpiryDuration:  time.Minute,
			})
			if err != nil {
				t.Fatalf("CreatePaymentIntent returned error: %v", err)
			}
			want := tt.amount
			if tt.currency == "IDR" {
				want = tt.amount * 100
			}
			if gotReq.Amount != want {
				t.Fatalf("virtual amount = %d, want %d", gotReq.Amount, want)
			}
		})
	}
}

func TestCreatePaymentIntentRejectsUnsupportedCurrency(t *testing.T) {
	a, err := New(&Config{
		SecretKey:         "sk_test",
		WebhookSecret:     "secret",
		APIVersion:        defaultAPIVersion,
		APIBaseURL:        "https://komoju.test",
		DefaultLocale:     defaultLocale,
		AllowedCurrencies: parseCodeSet("JPY"),
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	_, err = a.CreatePaymentIntent(context.Background(), adapter.PaymentInitRequest{
		InternalOrderID: "txn-123",
		Amount:          1000,
		CurrencyCode:    "USD",
		ExpiryDuration:  time.Minute,
	})
	if err == nil {
		t.Fatal("expected unsupported currency error")
	}
}

func TestValidateWebhookSignature(t *testing.T) {
	a, err := New(testConfig("https://komoju.test"))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	body := []byte(`{"type":"payment.captured"}`)
	signature := sign(body, "whsec_test")

	if err := a.ValidateWebhookSignature(context.Background(), body, map[string]string{signatureHeader: signature}); err != nil {
		t.Fatalf("expected valid signature: %v", err)
	}
	if err := a.ValidateWebhookSignature(context.Background(), body, map[string]string{signatureHeader: "wrong"}); err == nil {
		t.Fatal("expected wrong signature to fail")
	}
	if err := a.ValidateWebhookSignature(context.Background(), body, map[string]string{}); err == nil {
		t.Fatal("expected missing signature to fail")
	}
}

func TestHandleWebhookMapsStatuses(t *testing.T) {
	a, err := New(testConfig("https://komoju.test"))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	tests := map[string]adapter.PaymentStatus{
		"captured":   adapter.PaymentStatusSuccess,
		"authorized": adapter.PaymentStatusPending,
		"pending":    adapter.PaymentStatusPending,
		"expired":    adapter.PaymentStatusExpired,
		"cancelled":  adapter.PaymentStatusCanceled,
		"failed":     adapter.PaymentStatusFailed,
		"refunded":   adapter.PaymentStatusRefunded,
	}
	for raw, want := range tests {
		t.Run(raw, func(t *testing.T) {
			payload := []byte(`{"id":"evt_123","type":"payment.updated","resource":"event","data":{"id":"pay_123","status":"` + raw + `","amount":3000000,"currency":"IDR","external_order_num":"txn-123","metadata":{"internalOrderId":"metadata-txn"}}}`)
			result, err := a.HandleWebhook(context.Background(), payload, nil)
			if err != nil {
				t.Fatalf("HandleWebhook returned error: %v", err)
			}
			if result.Status != want {
				t.Fatalf("Status = %q, want %q", result.Status, want)
			}
			if result.InternalOrderID != "txn-123" || result.ProviderTransactionID != "pay_123" {
				t.Fatalf("unexpected result ids: %+v", result)
			}
			if result.Amount != 30000 {
				t.Fatalf("Amount = %d, want internal 30000", result.Amount)
			}
		})
	}
}

func TestHandleWebhookFallsBackToMetadataOrderID(t *testing.T) {
	a, err := New(testConfig("https://komoju.test"))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	result, err := a.HandleWebhook(context.Background(), []byte(`{"data":{"id":"pay_123","status":"captured","amount":100,"currency":"JPY","metadata":{"internalOrderId":"txn-meta"}}}`), nil)
	if err != nil {
		t.Fatalf("HandleWebhook returned error: %v", err)
	}
	if result.InternalOrderID != "txn-meta" {
		t.Fatalf("InternalOrderID = %q, want txn-meta", result.InternalOrderID)
	}
	if result.Amount != 100 {
		t.Fatalf("Amount = %d, want internal 100", result.Amount)
	}
}

func TestGetPaymentStatusMapsCompletedSessionPaymentID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/sess_123" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"sess_123","status":"completed","amount":3000000,"currency":"IDR","payment":{"id":"pay_123","status":"captured","amount":3000000,"total":3000000,"currency":"IDR"}}`)) //nolint:errcheck
	}))
	defer server.Close()

	a, err := New(testConfig(server.URL))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	status, err := a.GetPaymentStatus(context.Background(), "sess_123")
	if err != nil {
		t.Fatalf("GetPaymentStatus returned error: %v", err)
	}
	if status.ProviderTxID != "pay_123" || status.Status != adapter.PaymentStatusSuccess || status.Amount != 30000 {
		t.Fatalf("unexpected provider status: %+v", status)
	}
}

func TestGetPaymentStatusFallsBackToPaymentShow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/sessions/pay_123":
			http.NotFound(w, r)
		case "/api/v1/payments/pay_123":
			w.Write([]byte(`{"id":"pay_123","status":"captured","amount":100,"currency":"JPY"}`)) //nolint:errcheck
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	a, err := New(testConfig(server.URL))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	status, err := a.GetPaymentStatus(context.Background(), "pay_123")
	if err != nil {
		t.Fatalf("GetPaymentStatus returned error: %v", err)
	}
	if status.ProviderTxID != "pay_123" || status.Status != adapter.PaymentStatusSuccess || status.Amount != 100 {
		t.Fatalf("unexpected provider status: %+v", status)
	}
}

func TestSyncTransactionStatusRefunded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/sessions/pay_123":
			http.NotFound(w, r)
		case "/api/v1/payments/pay_123":
			w.Write([]byte(`{"id":"pay_123","status":"refunded","amount":100,"currency":"JPY"}`)) //nolint:errcheck
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	a, err := New(testConfig(server.URL))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	result, err := a.SyncTransactionStatus(context.Background(), &model.Transaction{ProviderTxID: "pay_123"})
	if err != nil {
		t.Fatalf("SyncTransactionStatus returned error: %v", err)
	}
	if result.RefundStatus != adapter.SyncRefundStatusRefunded {
		t.Fatalf("refund sync status = %q, want %q", result.RefundStatus, adapter.SyncRefundStatusRefunded)
	}
	if result.PaymentStatus != adapter.SyncPaymentStatusPaid {
		t.Fatalf("payment sync status = %q, want %q", result.PaymentStatus, adapter.SyncPaymentStatusPaid)
	}
}

func TestRefundPayment(t *testing.T) {
	var gotIdempotency string
	var gotReq refundRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/payments/pay_123/refund" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		gotIdempotency = r.Header.Get(idempotencyHeader)
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode refund request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"pay_123","status":"refunded"}`)) //nolint:errcheck
	}))
	defer server.Close()

	a, err := New(testConfig(server.URL))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if err := a.RefundPayment(context.Background(), "txn-123", "pay_123", 30000, "IDR"); err != nil {
		t.Fatalf("RefundPayment returned error: %v", err)
	}
	if gotIdempotency != "refund-txn-123" {
		t.Fatalf("idempotency = %q, want refund-txn-123", gotIdempotency)
	}
	if gotReq.Amount != 3000000 {
		t.Fatalf("refund amount = %d, want 3000000", gotReq.Amount)
	}
}

func TestCancelPaymentCancelsSession(t *testing.T) {
	var gotIdempotency string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/sess_123/cancel" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		gotIdempotency = r.Header.Get(idempotencyHeader)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"sess_123","status":"cancelled"}`)) //nolint:errcheck
	}))
	defer server.Close()

	a, err := New(testConfig(server.URL))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	result, err := a.CancelPayment(context.Background(), &model.Transaction{ID: "txn-123", ProviderTxID: "sess_123"}, "cancel")
	if err != nil {
		t.Fatalf("CancelPayment returned error: %v", err)
	}
	if result.Status != adapter.CancelStatusCanceled {
		t.Fatalf("cancel status = %q, want %q", result.Status, adapter.CancelStatusCanceled)
	}
	if gotIdempotency != "cancel-txn-123" {
		t.Fatalf("idempotency = %q, want cancel-txn-123", gotIdempotency)
	}
}

func TestKomojuAmountConversion(t *testing.T) {
	tests := []struct {
		name     string
		currency string
		internal int64
		komoju   int64
	}{
		{name: "IDR", currency: "IDR", internal: 30000, komoju: 3000000},
		{name: "JPY", currency: "JPY", internal: 100, komoju: 100},
		{name: "USD", currency: "USD", internal: 100, komoju: 10000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := toKomojuAmount(tt.internal, tt.currency)
			if err != nil {
				t.Fatalf("toKomojuAmount returned error: %v", err)
			}
			if got != tt.komoju {
				t.Fatalf("toKomojuAmount = %d, want %d", got, tt.komoju)
			}
			back, err := fromKomojuAmount(tt.komoju, tt.currency)
			if err != nil {
				t.Fatalf("fromKomojuAmount returned error: %v", err)
			}
			if back != tt.internal {
				t.Fatalf("fromKomojuAmount = %d, want %d", back, tt.internal)
			}
		})
	}
}

func TestKomojuAmountConversionRejectsUnknownCurrency(t *testing.T) {
	if _, err := toKomojuAmount(100, "NOPE"); err == nil {
		t.Fatal("expected unknown currency error")
	}
	if _, err := fromKomojuAmount(100, "NOPE"); err == nil {
		t.Fatal("expected unknown currency error")
	}
}

func sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
