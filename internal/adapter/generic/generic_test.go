package generic_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter/generic"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/config"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
)

func makeConfig(srv *httptest.Server, overrides ...func(*config.GenericProviderConfig)) *config.GenericProviderConfig {
	baseURL := ""
	if srv != nil {
		baseURL = srv.URL
	}
	cfg := &config.GenericProviderConfig{
		Name:                      "test",
		AuthHeader:                "Authorization",
		AuthValue:                 "Bearer test-key",
		CreateIntentURL:           baseURL + "/charges",
		CreateIntentBodyTemplate:  `{"order_id":"{{.OrderID}}","amount":{{.Amount}},"currency":"{{.CurrencyCode}}","callback":"{{.CallbackURL}}"}`,
		PaymentURLJSONPath:        "data.redirect_url",
		ProviderTxIDJSONPath:      "data.transaction_id",
		WebhookSignatureMethod:    "HMAC_SHA256",
		WebhookSignatureSecret:    "test-secret",
		WebhookSignatureHeader:    "x-signature",
		WebhookTxIDJSONPath:       "transaction_id",
		WebhookSuccessStatusPath:  "status",
		WebhookSuccessStatusValue: "SUCCESS",
		WebhookFailedStatusValue:  "FAILED",
		RefundURL:                 baseURL + "/refunds",
		RefundBodyTemplate:        `{"transaction_id":"{{.ProviderTxID}}","amount":{{.Amount}},"currency":"{{.CurrencyCode}}"}`,
	}
	for _, fn := range overrides {
		fn(cfg)
	}
	return cfg
}

func TestNew_MalformedTemplate(t *testing.T) {
	cfg := makeConfig(nil, func(c *config.GenericProviderConfig) {
		c.CreateIntentBodyTemplate = `{"bad": {{.Missing}`
	})
	_, err := generic.New(cfg)
	assert.Error(t, err, "malformed template must fail at New(), not at call time")
}

func TestName(t *testing.T) {
	a, err := generic.New(makeConfig(nil))
	require.NoError(t, err)
	assert.Equal(t, "test", a.Info().ID)
}

func TestCreatePaymentIntent_TemplateRendering(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"redirect_url":"https://pay.example.com/pay123","transaction_id":"TX-001"}}`))
	}))
	defer srv.Close()

	a, err := generic.New(makeConfig(srv))
	require.NoError(t, err)

	intent, err := a.CreatePaymentIntent(context.Background(), adapter.PaymentInitRequest{
		InternalOrderID: "order-abc",
		Amount:          50000,
		CurrencyCode:    "IDR",
		CallbackURL:     "https://my.app/webhook/test",
		ExpiryDuration:  900 * 1000000000, // 15 min
	})
	require.NoError(t, err)
	assert.Equal(t, "https://pay.example.com/pay123", intent.PaymentURL)
	assert.Equal(t, "TX-001", intent.ProviderTransactionID)
}

func TestGetPaymentStatus_NotSupported(t *testing.T) {
	cfg := makeConfig(nil, func(c *config.GenericProviderConfig) {
		c.StatusURLTemplate = "" // no status query configured
	})
	a, err := generic.New(cfg)
	require.NoError(t, err)

	_, err = a.GetPaymentStatus(context.Background(), "TX-001")
	assert.ErrorIs(t, err, adapter.ErrNotSupported)
}

func TestGetPaymentStatus_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"status":"COMPLETED"}}`))
	}))
	defer srv.Close()

	cfg := makeConfig(srv, func(c *config.GenericProviderConfig) {
		c.StatusURLTemplate = srv.URL + "/charges/{{.ProviderTxID}}"
		c.StatusMethod = "GET"
		c.StatusPaymentStatusPath = "data.status"
		c.StatusSuccessValue = "COMPLETED"
		c.StatusPendingValue = "PENDING"
		c.StatusFailedValues = []string{"FAILED", "EXPIRED"}
	})
	a, err := generic.New(cfg)
	require.NoError(t, err)

	status, err := a.GetPaymentStatus(context.Background(), "TX-001")
	require.NoError(t, err)
	assert.Equal(t, adapter.PaymentStatusSuccess, status.Status)
}

func TestSyncTransactionStatus_Refunded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"status":"REFUNDED"}}`))
	}))
	defer srv.Close()

	cfg := makeConfig(srv, func(c *config.GenericProviderConfig) {
		c.StatusURLTemplate = srv.URL + "/charges/{{.ProviderTxID}}"
		c.StatusMethod = "GET"
		c.StatusPaymentStatusPath = "data.status"
		c.StatusSuccessValue = "COMPLETED"
		c.StatusPendingValue = "PENDING"
		c.StatusFailedValues = []string{"FAILED", "EXPIRED"}
		c.StatusRefundValue = "REFUNDED"
	})
	a, err := generic.New(cfg)
	require.NoError(t, err)

	result, err := a.SyncTransactionStatus(context.Background(), &model.Transaction{ProviderTxID: "TX-001"})

	require.NoError(t, err)
	assert.Equal(t, adapter.SyncRefundStatusRefunded, result.RefundStatus)
	assert.Equal(t, adapter.SyncPaymentStatusPaid, result.PaymentStatus)
}

func TestValidateWebhookSignature_HMAC256_Valid(t *testing.T) {
	body := []byte(`{"transaction_id":"TX-001","status":"SUCCESS"}`)
	mac := hmac.New(sha256.New, []byte("test-secret"))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	a, err := generic.New(makeConfig(nil))
	require.NoError(t, err)

	err = a.ValidateWebhookSignature(context.Background(), body, map[string]string{
		"x-signature": sig,
	})
	assert.NoError(t, err)
}

func TestValidateWebhookSignature_HMAC256_Tampered(t *testing.T) {
	body := []byte(`{"transaction_id":"TX-001","status":"SUCCESS"}`)
	tamperedBody := []byte(`{"transaction_id":"TX-001","status":"HACKED"}`)
	mac := hmac.New(sha256.New, []byte("test-secret"))
	mac.Write(tamperedBody)
	sig := hex.EncodeToString(mac.Sum(nil))

	a, err := generic.New(makeConfig(nil))
	require.NoError(t, err)

	err = a.ValidateWebhookSignature(context.Background(), body, map[string]string{
		"x-signature": sig,
	})
	assert.Error(t, err, "tampered body must fail signature validation")
}

func TestValidateWebhookSignature_NONE(t *testing.T) {
	cfg := makeConfig(nil, func(c *config.GenericProviderConfig) {
		c.WebhookSignatureMethod = "NONE"
		c.WebhookSignatureSecret = ""
	})
	a, err := generic.New(cfg)
	require.NoError(t, err)

	err = a.ValidateWebhookSignature(context.Background(), []byte("any body"), map[string]string{})
	assert.NoError(t, err, "NONE method must always pass")
}

func TestHandleWebhook_Success(t *testing.T) {
	body := []byte(`{"transaction_id":"TX-001","status":"SUCCESS"}`)
	a, err := generic.New(makeConfig(nil))
	require.NoError(t, err)

	result, err := a.HandleWebhook(context.Background(), body, nil)
	require.NoError(t, err)
	assert.Equal(t, adapter.PaymentStatusSuccess, result.Status)
	assert.Equal(t, "TX-001", result.InternalOrderID)
}

func TestHandleWebhook_Failed(t *testing.T) {
	body := []byte(`{"transaction_id":"TX-002","status":"FAILED"}`)
	a, err := generic.New(makeConfig(nil))
	require.NoError(t, err)

	result, err := a.HandleWebhook(context.Background(), body, nil)
	require.NoError(t, err)
	assert.Equal(t, adapter.PaymentStatusFailed, result.Status)
}

func TestHandleWebhook_Pending(t *testing.T) {
	body := []byte(`{"transaction_id":"TX-003","status":"UNKNOWN_STATUS"}`)
	a, err := generic.New(makeConfig(nil))
	require.NoError(t, err)

	result, err := a.HandleWebhook(context.Background(), body, nil)
	require.NoError(t, err)
	assert.Equal(t, adapter.PaymentStatusPending, result.Status)
}

func TestHandleWebhook_MissingTxID(t *testing.T) {
	body := []byte(`{"status":"SUCCESS"}`)
	a, err := generic.New(makeConfig(nil))
	require.NoError(t, err)

	_, err = a.HandleWebhook(context.Background(), body, nil)
	assert.Error(t, err, "missing transaction ID must return error")
}

func TestCancelPaymentConfigured(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"CANCELED"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	a, err := generic.New(makeConfig(srv, func(c *config.GenericProviderConfig) {
		c.CancelURLTemplate = srv.URL + "/charges/{{.ProviderTxID}}/cancel"
		c.CancelStatusPath = "status"
		c.CancelSuccessValues = []string{"CANCELED"}
	}))
	require.NoError(t, err)

	result, err := a.CancelPayment(context.Background(), &model.Transaction{ID: "txn-1", ProviderTxID: "provider-1"}, "cancel")
	require.NoError(t, err)
	assert.Equal(t, "/charges/provider-1/cancel", gotPath)
	assert.Equal(t, adapter.CancelStatusCanceled, result.Status)
}

func TestCancelPaymentUnsupportedWhenNotConfigured(t *testing.T) {
	a, err := generic.New(makeConfig(nil))
	require.NoError(t, err)
	result, err := a.CancelPayment(context.Background(), &model.Transaction{ID: "txn-1", ProviderTxID: "provider-1"}, "cancel")
	require.NoError(t, err)
	assert.Equal(t, adapter.CancelStatusUnsupported, result.Status)
}
