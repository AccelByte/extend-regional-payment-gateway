package xendit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
)

func testConfig() *Config {
	return &Config{
		SecretAPIKey:      "test-key",
		CallbackToken:     "callback-token",
		DefaultCountry:    "ID",
		AllowedCountries:  parseCodeSet(defaultCountries),
		AllowedCurrencies: parseCodeSet(defaultCurrencies),
	}
}

type fakeRefundClient struct {
	createCalls      int
	paymentRequestID string
	idempotencyKey   string
	refunds          []xenditRefundRecord
}

func (f *fakeRefundClient) CreateRefund(_ context.Context, _ string, paymentRequestID string, _ int64, _ string, idempotencyKey string) error {
	f.createCalls++
	f.paymentRequestID = paymentRequestID
	f.idempotencyKey = idempotencyKey
	return nil
}

func (f *fakeRefundClient) ListRefundsByPaymentRequestID(context.Context, string) ([]xenditRefundRecord, error) {
	return f.refunds, nil
}

type fakeTransactionHistory struct {
	byReference map[string][]xenditTransactionRecord
	byProduct   map[string][]xenditTransactionRecord
}

func (f fakeTransactionHistory) ListByReferenceID(_ context.Context, referenceID string) ([]xenditTransactionRecord, error) {
	return f.byReference[referenceID], nil
}

func (f fakeTransactionHistory) ListByProductID(_ context.Context, productID string) ([]xenditTransactionRecord, error) {
	return f.byProduct[productID], nil
}

func TestResolveCountry(t *testing.T) {
	a := &Adapter{cfg: testConfig()}

	tests := []struct {
		name       string
		regionCode string
		want       string
		wantErr    bool
	}{
		{name: "uses default for empty region", regionCode: "", want: "ID"},
		{name: "normalizes lowercase country", regionCode: "ph", want: "PH"},
		{name: "uses country prefix from compound region", regionCode: "sg-prod", want: "SG"},
		{name: "rejects unsupported region", regionCode: "US", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := a.resolveCountry(tt.regionCode)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveCountry returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveCountry = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCreatePaymentIntentCreatesPaymentSession(t *testing.T) {
	var gotReq xenditPaymentSessionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"payment_session_id":"ps-123","status":"ACTIVE","payment_link_url":"https://xendit.test/pay/ps-123","amount":10000,"currency":"IDR"}`)) //nolint:errcheck
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.APIBaseURL = server.URL
	a := &Adapter{cfg: cfg, httpClient: server.Client()}

	intent, err := a.CreatePaymentIntent(context.Background(), adapter.PaymentInitRequest{
		InternalOrderID: "txn-123",
		UserID:          "user-1",
		RegionCode:      "id",
		Amount:          10000,
		CurrencyCode:    "IDR",
		ReturnURL:       "https://example.test/result?transactionId=txn-123",
		ExpiryDuration:  time.Minute,
	})
	if err != nil {
		t.Fatalf("CreatePaymentIntent returned error: %v", err)
	}
	if intent.ProviderTransactionID != "ps-123" || intent.PaymentURL == "" {
		t.Fatalf("unexpected intent: %+v", intent)
	}
	if gotReq.ReferenceID != "txn-123" || gotReq.SessionType != "PAY" || gotReq.Mode != "PAYMENT_LINK" || gotReq.Currency != "IDR" {
		t.Fatalf("unexpected payment session request: %+v", gotReq)
	}
}

func TestCancelPaymentSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/ps-123/cancel" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"payment_session_id":"ps-123","status":"CANCELED"}`)) //nolint:errcheck
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.APIBaseURL = server.URL
	a := &Adapter{cfg: cfg, httpClient: server.Client()}
	result, err := a.CancelPayment(context.Background(), &model.Transaction{ID: "txn-123", ProviderTxID: "ps-123"}, "cancel")
	if err != nil {
		t.Fatalf("CancelPayment returned error: %v", err)
	}
	if result.Status != adapter.CancelStatusCanceled {
		t.Fatalf("cancel status = %q, want %q", result.Status, adapter.CancelStatusCanceled)
	}
}

func TestValidateWebhookSignature(t *testing.T) {
	a := &Adapter{cfg: testConfig()}

	if err := a.ValidateWebhookSignature(context.Background(), nil, map[string]string{callbackTokenHeader: "callback-token"}); err != nil {
		t.Fatalf("expected valid callback token: %v", err)
	}
	if err := a.ValidateWebhookSignature(context.Background(), nil, map[string]string{callbackTokenHeader: "wrong"}); err == nil {
		t.Fatal("expected wrong callback token to fail")
	}
	if err := a.ValidateWebhookSignature(context.Background(), nil, map[string]string{}); err == nil {
		t.Fatal("expected missing callback token to fail")
	}
}

func TestHandleWebhookMapsPaymentSessionEvent(t *testing.T) {
	a := &Adapter{cfg: testConfig()}
	result, err := a.HandleWebhook(context.Background(), []byte(`{
		"type": "payment_session.completed",
		"data": {
			"payment_session_id": "ps-123",
			"reference_id": "txn-123",
			"status": "COMPLETED",
			"amount": 10000,
			"currency": "IDR"
		}
	}`), nil)
	if err != nil {
		t.Fatalf("HandleWebhook returned error: %v", err)
	}
	if result.InternalOrderID != "txn-123" {
		t.Fatalf("InternalOrderID = %q, want txn-123", result.InternalOrderID)
	}
	if result.ProviderTransactionID != "ps-123" {
		t.Fatalf("ProviderTransactionID = %q, want ps-123", result.ProviderTransactionID)
	}
	if result.Status != adapter.PaymentStatusSuccess {
		t.Fatalf("Status = %q, want SUCCESS", result.Status)
	}
}

func TestHandleWebhookMapsRefundEvent(t *testing.T) {
	a := &Adapter{cfg: testConfig()}
	result, err := a.HandleWebhook(context.Background(), []byte(`{
		"event": "refund.succeeded",
		"business_id": "biz-1",
		"created": "2026-04-29T00:00:00Z",
		"data": {
			"id": "22c2dfcb-5215-4877-9b81-24dcbe4270e4",
			"status": "SUCCEEDED",
			"payment_id": "py-54f8e134-a06c-4e9e-8e8d-b674cbaa5835",
			"amount": 10000,
			"currency": "IDR",
			"reference_id": "txn-123"
		}
	}`), nil)
	if err != nil {
		t.Fatalf("HandleWebhook returned error: %v", err)
	}
	if result.InternalOrderID != "txn-123" {
		t.Fatalf("InternalOrderID = %q, want txn-123", result.InternalOrderID)
	}
	if result.Status != adapter.PaymentStatusRefunded {
		t.Fatalf("Status = %q, want REFUNDED", result.Status)
	}
	if result.ProviderTransactionID != "22c2dfcb-5215-4877-9b81-24dcbe4270e4" {
		t.Fatalf("ProviderTransactionID = %q", result.ProviderTransactionID)
	}
}

func TestHandleWebhookRejectsUnrecognizedPayload(t *testing.T) {
	a := &Adapter{cfg: testConfig()}
	_, err := a.HandleWebhook(context.Background(), []byte(`{"external_id":"txn-123","status":"PAID"}`), nil)
	if err == nil {
		t.Fatal("expected error for unrecognized payload")
	}
	if !strings.Contains(err.Error(), "unrecognized") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMapPaymentSessionStatus(t *testing.T) {
	tests := map[string]adapter.PaymentStatus{
		"COMPLETED": adapter.PaymentStatusSuccess,
		"PAID":      adapter.PaymentStatusSuccess,
		"SUCCEEDED": adapter.PaymentStatusSuccess,
		"CANCELED":  adapter.PaymentStatusCanceled,
		"CANCELLED": adapter.PaymentStatusCanceled,
		"EXPIRED":   adapter.PaymentStatusExpired,
		"FAILED":    adapter.PaymentStatusFailed,
		"ACTIVE":    adapter.PaymentStatusPending,
	}
	for raw, want := range tests {
		if got := mapPaymentSessionStatus(raw); got != want {
			t.Fatalf("mapPaymentSessionStatus(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestSummarizeXenditRefundsFullAndPartial(t *testing.T) {
	fullStatus, _, fullAmount, _ := summarizeXenditRefunds([]xenditRefundRecord{
		{Status: "SUCCEEDED", Amount: 6000, Currency: "IDR"},
		{Status: "SUCCEEDED", Amount: 4000, Currency: "IDR"},
	}, 10000, "IDR")
	if fullStatus != adapter.SyncRefundStatusRefunded || fullAmount != 10000 {
		t.Fatalf("full refund summary = %q amount=%d", fullStatus, fullAmount)
	}

	partialStatus, _, partialAmount, _ := summarizeXenditRefunds([]xenditRefundRecord{
		{Status: "SUCCEEDED", Amount: 3000, Currency: "IDR"},
	}, 10000, "IDR")
	if partialStatus != adapter.SyncRefundStatusPartialRefunded || partialAmount != 3000 {
		t.Fatalf("partial refund summary = %q amount=%d", partialStatus, partialAmount)
	}
}

func TestSummarizeXenditRefundsPendingAndFailed(t *testing.T) {
	pendingStatus, _, _, _ := summarizeXenditRefunds([]xenditRefundRecord{{Status: "PENDING", Currency: "IDR"}}, 10000, "IDR")
	if pendingStatus != adapter.SyncRefundStatusPending {
		t.Fatalf("pending refund summary = %q", pendingStatus)
	}
	failedStatus, _, _, _ := summarizeXenditRefunds([]xenditRefundRecord{{Status: "FAILED", Currency: "IDR"}}, 10000, "IDR")
	if failedStatus != adapter.SyncRefundStatusFailed {
		t.Fatalf("failed refund summary = %q", failedStatus)
	}
}

func TestBuildRefundRequestByPaymentRequestID(t *testing.T) {
	refundReq := buildRefundRequestByPaymentRequestID("txn-123", "pr-456", 10000, "idr")
	if got := refundReq.GetPaymentRequestId(); got != "pr-456" {
		t.Fatalf("PaymentRequestId = %q, want pr-456", got)
	}
	if got := refundReq.GetReferenceId(); got != "txn-123" {
		t.Fatalf("ReferenceId = %q, want txn-123", got)
	}
	if got := refundReq.GetAmount(); got != 10000 {
		t.Fatalf("Amount = %f, want 10000", got)
	}
	if got := refundReq.GetCurrency(); got != "IDR" {
		t.Fatalf("Currency = %q, want IDR", got)
	}
	if got := refundReq.GetReason(); got != "REQUESTED_BY_CUSTOMER" {
		t.Fatalf("Reason = %q, want REQUESTED_BY_CUSTOMER", got)
	}
}

func TestSyncTransactionStatusPaymentSession(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions/ps-abc", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"payment_session_id":"ps-abc","status":"COMPLETED","amount":10000,"currency":"IDR","payment_request_id":"pr-xyz","payment_id":"py-abc"}`)) //nolint:errcheck
	})
	mux.HandleFunc("/v3/payment_requests/pr-xyz", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("api-version"); got != xenditPaymentsAPIVersion {
			t.Fatalf("api-version = %q, want %q", got, xenditPaymentsAPIVersion)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"payment_request_id":"pr-xyz","reference_id":"txn-123","status":"SUCCEEDED","request_amount":10000,"currency":"IDR","latest_payment_id":"py-abc"}`)) //nolint:errcheck
	})
	mux.HandleFunc("/v3/payments/py-abc", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("api-version"); got != xenditPaymentsAPIVersion {
			t.Fatalf("api-version = %q, want %q", got, xenditPaymentsAPIVersion)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"payment_id":"py-abc","payment_request_id":"pr-xyz","reference_id":"txn-123","status":"SUCCEEDED","request_amount":10000,"currency":"IDR"}`)) //nolint:errcheck
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := testConfig()
	cfg.APIBaseURL = server.URL
	a := &Adapter{
		cfg:        cfg,
		httpClient: server.Client(),
		refunds: &fakeRefundClient{refunds: []xenditRefundRecord{
			{ID: "rfd-1", Status: "SUCCEEDED", Amount: 10000, Currency: "IDR"},
		}},
	}

	result, err := a.SyncTransactionStatus(context.Background(), &model.Transaction{
		ID:           "txn-123",
		ProviderTxID: "ps-abc",
		Amount:       10000,
		CurrencyCode: "IDR",
	})
	if err != nil {
		t.Fatalf("SyncTransactionStatus returned error: %v", err)
	}
	if result.PaymentStatus != adapter.SyncPaymentStatusPaid {
		t.Fatalf("PaymentStatus = %q, want PAID", result.PaymentStatus)
	}
	if result.RefundStatus != adapter.SyncRefundStatusRefunded {
		t.Fatalf("RefundStatus = %q, want REFUNDED", result.RefundStatus)
	}
	if result.RefundAmount != 10000 {
		t.Fatalf("RefundAmount = %d, want 10000", result.RefundAmount)
	}
}

func TestSyncTransactionStatusUsesHistoryWhenWebhookWasMissed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions/ps-abc", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"payment_session_id":"ps-abc","status":"ACTIVE","amount":10000,"currency":"IDR","payment_request_id":"pr-xyz","payment_id":"py-abc"}`)) //nolint:errcheck
	})
	mux.HandleFunc("/v3/payment_requests/pr-xyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"payment_request_id":"pr-xyz","reference_id":"txn-123","status":"REQUIRES_ACTION","request_amount":10000,"currency":"IDR","latest_payment_id":"py-abc"}`)) //nolint:errcheck
	})
	mux.HandleFunc("/v3/payments/py-abc", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"payment_id":"py-abc","payment_request_id":"pr-xyz","reference_id":"txn-123","status":"REQUIRES_ACTION","request_amount":10000,"currency":"IDR"}`)) //nolint:errcheck
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := testConfig()
	cfg.APIBaseURL = server.URL
	a := &Adapter{
		cfg:        cfg,
		httpClient: server.Client(),
		refunds:    &fakeRefundClient{},
		transactionHistory: fakeTransactionHistory{byReference: map[string][]xenditTransactionRecord{
			"txn-123": {
				{ID: "txn-pay-1", ProductID: "py-abc", Type: "PAYMENT", Status: "SUCCESS", ReferenceID: "txn-123", Amount: 10000, Currency: "IDR"},
			},
		}},
	}

	result, err := a.SyncTransactionStatus(context.Background(), &model.Transaction{
		ID:           "txn-123",
		ProviderTxID: "ps-abc",
		Amount:       10000,
		CurrencyCode: "IDR",
	})
	if err != nil {
		t.Fatalf("SyncTransactionStatus returned error: %v", err)
	}
	if result.PaymentStatus != adapter.SyncPaymentStatusPaid {
		t.Fatalf("PaymentStatus = %q, want PAID", result.PaymentStatus)
	}
}

func TestSyncTransactionStatusMarksFailedFromSessionStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions/ps-abc", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"payment_session_id":"ps-abc","status":"EXPIRED","amount":10000,"currency":"IDR"}`)) //nolint:errcheck
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := testConfig()
	cfg.APIBaseURL = server.URL
	a := &Adapter{cfg: cfg, httpClient: server.Client()}

	result, err := a.SyncTransactionStatus(context.Background(), &model.Transaction{
		ID:           "txn-123",
		ProviderTxID: "ps-abc",
		Amount:       10000,
		CurrencyCode: "IDR",
	})
	if err != nil {
		t.Fatalf("SyncTransactionStatus returned error: %v", err)
	}
	if result.PaymentStatus != adapter.SyncPaymentStatusFailed {
		t.Fatalf("PaymentStatus = %q, want FAILED", result.PaymentStatus)
	}
	if result.RawPaymentStatus != "EXPIRED" {
		t.Fatalf("RawPaymentStatus = %q, want EXPIRED", result.RawPaymentStatus)
	}
}

func TestSyncTransactionStatusDetectsDashboardRefundFromHistory(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions/ps-abc", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"payment_session_id":"ps-abc","status":"COMPLETED","amount":10000,"currency":"IDR","payment_request_id":"pr-xyz","payment_id":"py-abc"}`)) //nolint:errcheck
	})
	mux.HandleFunc("/v3/payment_requests/pr-xyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"payment_request_id":"pr-xyz","reference_id":"txn-123","status":"SUCCEEDED","request_amount":10000,"currency":"IDR","latest_payment_id":"py-abc"}`)) //nolint:errcheck
	})
	mux.HandleFunc("/v3/payments/py-abc", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"payment_id":"py-abc","payment_request_id":"pr-xyz","reference_id":"txn-123","status":"SUCCEEDED","request_amount":10000,"currency":"IDR"}`)) //nolint:errcheck
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := testConfig()
	cfg.APIBaseURL = server.URL
	a := &Adapter{
		cfg:        cfg,
		httpClient: server.Client(),
		refunds:    &fakeRefundClient{},
		transactionHistory: fakeTransactionHistory{byProduct: map[string][]xenditTransactionRecord{
			"py-abc": {
				{ID: "txn-rfd-1", ProductID: "py-abc", Type: "REFUND", Status: "SUCCESS", ReferenceID: "txn-123", Amount: 10000, Currency: "IDR"},
			},
		}},
	}

	result, err := a.SyncTransactionStatus(context.Background(), &model.Transaction{
		ID:           "txn-123",
		ProviderTxID: "ps-abc",
		Amount:       10000,
		CurrencyCode: "IDR",
	})
	if err != nil {
		t.Fatalf("SyncTransactionStatus returned error: %v", err)
	}
	if result.RefundStatus != adapter.SyncRefundStatusRefunded {
		t.Fatalf("RefundStatus = %q, want REFUNDED", result.RefundStatus)
	}
	if result.RefundAmount != 10000 {
		t.Fatalf("RefundAmount = %d, want 10000", result.RefundAmount)
	}
}

func TestSyncTransactionStatusPartialRefundReportsOnly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions/ps-abc", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"payment_session_id":"ps-abc","status":"COMPLETED","amount":10000,"currency":"IDR","payment_request_id":"pr-xyz","payment_id":"py-abc"}`)) //nolint:errcheck
	})
	mux.HandleFunc("/v3/payment_requests/pr-xyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"payment_request_id":"pr-xyz","reference_id":"txn-123","status":"SUCCEEDED","request_amount":10000,"currency":"IDR","latest_payment_id":"py-abc"}`)) //nolint:errcheck
	})
	mux.HandleFunc("/v3/payments/py-abc", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"payment_id":"py-abc","payment_request_id":"pr-xyz","reference_id":"txn-123","status":"SUCCEEDED","request_amount":10000,"currency":"IDR"}`)) //nolint:errcheck
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := testConfig()
	cfg.APIBaseURL = server.URL
	a := &Adapter{
		cfg:        cfg,
		httpClient: server.Client(),
		refunds: &fakeRefundClient{refunds: []xenditRefundRecord{
			{ID: "rfd-1", Status: "SUCCEEDED", Amount: 3000, Currency: "IDR"},
		}},
	}

	result, err := a.SyncTransactionStatus(context.Background(), &model.Transaction{
		ID:           "txn-123",
		ProviderTxID: "ps-abc",
		Amount:       10000,
		CurrencyCode: "IDR",
	})
	if err != nil {
		t.Fatalf("SyncTransactionStatus returned error: %v", err)
	}
	if result.RefundStatus != adapter.SyncRefundStatusPartialRefunded {
		t.Fatalf("RefundStatus = %q, want PARTIAL_REFUNDED", result.RefundStatus)
	}
	if result.RefundAmount != 3000 {
		t.Fatalf("RefundAmount = %d, want 3000", result.RefundAmount)
	}
}

func TestRefundPaymentResolvesPaymentRequestIDFromSession(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions/ps-abc", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"payment_session_id":"ps-abc","status":"COMPLETED","payment_request_id":"pr-xyz","payment_id":"py-abc"}`)) //nolint:errcheck
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := testConfig()
	cfg.APIBaseURL = server.URL
	refunds := &fakeRefundClient{}
	a := &Adapter{cfg: cfg, httpClient: server.Client(), refunds: refunds}

	if err := a.RefundPayment(context.Background(), "txn-123", "ps-abc", 10000, "IDR"); err != nil {
		t.Fatalf("RefundPayment returned error: %v", err)
	}
	if refunds.createCalls != 1 {
		t.Fatalf("createCalls = %d, want 1", refunds.createCalls)
	}
	if refunds.paymentRequestID != "pr-xyz" {
		t.Fatalf("paymentRequestID = %q, want pr-xyz", refunds.paymentRequestID)
	}
	if refunds.idempotencyKey != "txn-123-ps-abc" {
		t.Fatalf("idempotencyKey = %q, want txn-123-ps-abc", refunds.idempotencyKey)
	}
}

func TestLoad(t *testing.T) {
	t.Setenv("XENDIT_SECRET_API_KEY", "sk_test")
	t.Setenv("XENDIT_CALLBACK_TOKEN", "token")
	t.Setenv("XENDIT_DEFAULT_COUNTRY", "ph")
	t.Setenv("XENDIT_ALLOWED_COUNTRIES", "ID,PH")
	t.Setenv("XENDIT_ALLOWED_CURRENCIES", "IDR,PHP")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.DefaultCountry != "PH" {
		t.Fatalf("DefaultCountry = %q, want PH", cfg.DefaultCountry)
	}
	if _, ok := cfg.AllowedCountries["PH"]; !ok {
		t.Fatal("expected PH to be allowed")
	}
	if _, ok := cfg.AllowedCurrencies["PHP"]; !ok {
		t.Fatal("expected PHP to be allowed")
	}
}

func TestLoadRequiresDefaultCountryInAllowlist(t *testing.T) {
	t.Setenv("XENDIT_SECRET_API_KEY", "sk_test")
	t.Setenv("XENDIT_CALLBACK_TOKEN", "token")
	t.Setenv("XENDIT_DEFAULT_COUNTRY", "MX")
	t.Setenv("XENDIT_ALLOWED_COUNTRIES", "ID,PH")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "XENDIT_DEFAULT_COUNTRY") {
		t.Fatalf("expected default country error, got %v", err)
	}
}
