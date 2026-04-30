//go:build integration

package dana_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	danasdk "github.com/dana-id/dana-go/v2"
	danaconfig "github.com/dana-id/dana-go/v2/config"
	danautils "github.com/dana-id/dana-go/v2/utils"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter/dana"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/config"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/fulfillment"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
	memstore "github.com/accelbyte/extend-regional-payment-gateway/internal/store/memory"
	pb "github.com/accelbyte/extend-regional-payment-gateway/pkg/pb"
	"github.com/accelbyte/extend-regional-payment-gateway/pkg/service"
)

const (
	sandboxCreateOrderEndpoint = "https://api.sandbox.dana.id/payment-gateway/v1.0/debit/payment-host-to-host.htm"
	sandboxCreateOrderPath     = "/payment-gateway/v1.0/debit/payment-host-to-host.htm"
)

var testCfg *dana.Config

func TestMain(m *testing.M) {
	cfg, err := dana.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "DANA config error: %v\n", err)
		os.Exit(1)
	}
	if cfg == nil {
		fmt.Println("DANA_MERCHANT_ID not set — skipping integration tests")
		os.Exit(0)
	}
	testCfg = cfg
	os.Exit(m.Run())
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func jakartaValidUpTo(daysAhead int) string {
	jakartaOffset := time.FixedZone("WIB", 7*3600)
	return time.Now().Add(time.Duration(daysAhead) * 24 * time.Hour).In(jakartaOffset).Format("2006-01-02T15:04:05+07:00")
}

// sendRawCreateOrder POSTs a raw JSON body to DANA's CreateOrder endpoint,
// optionally overriding generated SNAP headers. Returns DANA's responseCode string.
func sendRawCreateOrder(t *testing.T, body interface{}, customHeaders map[string]string) string {
	t.Helper()
	return sendRawCreateOrderWithKey(t, body, testCfg.PrivateKeyPEM, testCfg.PrivateKeyPath, customHeaders)
}

func sendRawCreateOrderWithKey(t *testing.T, body interface{}, privateKeyPEM, privateKeyPath string, customHeaders map[string]string) string {
	t.Helper()

	bodyJSON, err := json.Marshal(body)
	require.NoError(t, err)

	compact := &bytes.Buffer{}
	require.NoError(t, json.Compact(compact, bodyJSON))

	apiKey := &danaconfig.APIKey{
		ENV:              testCfg.Env,
		DANA_ENV:         testCfg.Env,
		X_PARTNER_ID:     testCfg.PartnerID,
		CHANNEL_ID:       testCfg.ChannelID,
		ORIGIN:           testCfg.Origin,
		CLIENT_SECRET:    testCfg.ClientSecret,
		PRIVATE_KEY:      privateKeyPEM,
		PRIVATE_KEY_PATH: privateKeyPath,
	}

	headerParams := make(map[string]string)
	danautils.SetSnapHeaders(headerParams, apiKey, compact.String(), "POST", sandboxCreateOrderPath, false)

	headers := map[string]string{
		"X-PARTNER-ID":  testCfg.PartnerID,
		"CHANNEL-ID":    testCfg.ChannelID,
		"ORIGIN":        testCfg.Origin,
		"Content-Type":  "application/json",
		"X-TIMESTAMP":   headerParams["X-TIMESTAMP"],
		"X-SIGNATURE":   headerParams["X-SIGNATURE"],
		"X-EXTERNAL-ID": "sdk" + uuid.New().String(),
	}
	for k, v := range customHeaders {
		headers[k] = v
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, sandboxCreateOrderEndpoint, bytes.NewReader(compact.Bytes()))
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	t.Logf("DANA response [%d]: %s", resp.StatusCode, string(respBody))

	var result map[string]string
	_ = json.Unmarshal(respBody, &result)
	return result["responseCode"]
}

func buildCreateOrderBody(partnerRefNo string, amountValue string) map[string]interface{} {
	return map[string]interface{}{
		"partnerReferenceNo": partnerRefNo,
		"merchantId":         testCfg.MerchantID,
		"amount": map[string]string{
			"value":    amountValue,
			"currency": "IDR",
		},
		"validUpTo": jakartaValidUpTo(1),
		"urlParams": []map[string]string{
			{"url": "https://example.com/return", "type": "PAY_RETURN", "isDeeplink": "N"},
			{"url": "https://example.com/notify", "type": "NOTIFICATION", "isDeeplink": "N"},
		},
		"additionalInfo": map[string]interface{}{
			"mcc": "5732",
			"envInfo": map[string]string{
				"sourcePlatform": "IPG",
				"terminalType":   "SYSTEM",
			},
			"order": map[string]string{
				"orderTitle": "Integration Test",
			},
		},
	}
}

// ─── CreateOrder Tests ────────────────────────────────────────────────────────

// Scenario 1: Valid CreateOrder request → success (2005400)
func TestIntegration_CreateOrder_Valid(t *testing.T) {
	cfg := *testCfg
	cfg.CheckoutMode = "DANA_GAPURA"
	a, err := dana.New(&cfg)
	require.NoError(t, err)

	intent, err := a.CreatePaymentIntent(context.Background(), adapter.PaymentInitRequest{
		InternalOrderID: uuid.New().String(),
		UserID:          "test-user",
		AmountIDR:       100, // 1.00 IDR (divided by 100 internally)
		Description:     "Integration Test",
		CallbackURL:     "https://example.com/notify",
		ReturnURL:       "https://example.com/return",
		ExpiryDuration:  20 * time.Minute, // DANA sandbox rejects validUpTo > 30 min in the future
	})
	require.NoError(t, err)
	assert.NotEmpty(t, intent.PaymentURL, "PaymentURL must not be empty")
	assert.NotEmpty(t, intent.ProviderTransactionID, "ProviderTransactionID must not be empty")
	t.Logf("Payment URL: %s", intent.PaymentURL)
	t.Logf("Provider TX ID: %s", intent.ProviderTransactionID)
}

// Scenario 2: Missing or invalid X-TIMESTAMP → 4005402
func TestIntegration_CreateOrder_InvalidMandatoryField(t *testing.T) {
	body := buildCreateOrderBody(uuid.New().String(), "1.00")
	respCode := sendRawCreateOrder(t, body, map[string]string{"X-TIMESTAMP": ""})
	assert.Equal(t, "4005402", respCode)
}

// Scenario 3: Invalid format for amount (no decimal) → 4005401
func TestIntegration_CreateOrder_InvalidFieldFormat(t *testing.T) {
	body := buildCreateOrderBody(uuid.New().String(), "100") // "100" instead of "100.00"
	respCode := sendRawCreateOrder(t, body, nil)
	assert.Equal(t, "4005401", respCode)
}

// Scenario 4: Same partnerReferenceNo, different amount → 4045418
func TestIntegration_CreateOrder_InconsistentRequest(t *testing.T) {
	ref := uuid.New().String()

	// First call — establish the reference
	body1 := buildCreateOrderBody(ref, "1.00")
	firstCode := sendRawCreateOrder(t, body1, nil)
	t.Logf("First call responseCode: %s", firstCode)

	// Second call — same ref, different amount
	body2 := buildCreateOrderBody(ref, "2.00")
	respCode := sendRawCreateOrder(t, body2, nil)
	assert.Equal(t, "4045418", respCode)
}

// Scenario 5: Invalid signature → 4015400
func TestIntegration_CreateOrder_Unauthorized(t *testing.T) {
	body := buildCreateOrderBody(uuid.New().String(), "1.00")
	respCode := sendRawCreateOrder(t, body, map[string]string{
		"X-SIGNATURE": "InvalidSignatureForIntegrationTest==",
	})
	assert.Equal(t, "4015400", respCode)
}

// ─── FinishNotify Tests ───────────────────────────────────────────────────────

// stubDanaAdapter is a minimal adapter that skips RSA verification so we can
// POST arbitrary payloads to the webhook handler in tests.
type stubDanaAdapter struct {
	internalOrderID string
}

func (s *stubDanaAdapter) Name() string { return "dana" }
func (s *stubDanaAdapter) ValidateWebhookSignature(_ context.Context, _ []byte, _ map[string]string) error {
	return nil // skip RSA check
}
func (s *stubDanaAdapter) HandleWebhook(_ context.Context, _ []byte, _ map[string]string) (*adapter.PaymentResult, error) {
	return &adapter.PaymentResult{
		InternalOrderID:       s.internalOrderID,
		ProviderTransactionID: "prov-test",
		Status:                adapter.PaymentStatusSuccess,
		RawProviderStatus:     "00",
	}, nil
}
func (s *stubDanaAdapter) WebhookAckBody() []byte {
	return []byte(`{"responseCode":"2005600","responseMessage":"Successful"}`)
}
func (s *stubDanaAdapter) WebhookErrorAckBody() []byte {
	return []byte(`{"responseCode":"5005601","responseMessage":"Internal Server Error"}`)
}
func (s *stubDanaAdapter) CreatePaymentIntent(_ context.Context, _ adapter.PaymentInitRequest) (*adapter.PaymentIntent, error) {
	return nil, errors.New("not implemented")
}
func (s *stubDanaAdapter) GetPaymentStatus(_ context.Context, _ string) (*adapter.ProviderPaymentStatus, error) {
	return nil, adapter.ErrNotSupported
}
func (s *stubDanaAdapter) RefundPayment(_ context.Context, _, _ string, _ int64, _ string) error {
	return errors.New("not implemented")
}
func (s *stubDanaAdapter) ValidateCredentials(_ context.Context) error { return nil }

type okFulfiller struct{}

func (o *okFulfiller) FulfillUserItem(_ context.Context, _, _, _ string, _ int32) error { return nil }

type failFulfiller struct{}

func (f *failFulfiller) FulfillUserItem(_ context.Context, _, _, _ string, _ int32) error {
	return errors.New("AGS unavailable")
}

// buildWebhookServer spins up an httptest.Server with the webhook handler wired.
// The fulfiller is swappable to control success/failure.
func buildWebhookServer(t *testing.T, txID string, f service.ItemFulfiller) (*httptest.Server, *memstore.Store) {
	t.Helper()

	stub := &stubDanaAdapter{internalOrderID: txID}
	reg := adapter.NewRegistry()
	reg.Register(stub)

	txStore := memstore.New()
	require.NoError(t, txStore.CreateTransaction(context.Background(), &model.Transaction{
		ID:            txID,
		UserID:        "user-1",
		ItemID:        "item-1",
		ClientOrderID: "co-" + txID,
		Status:        model.StatusPending,
		AmountIDR:     100,
		Quantity:      1,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}))

	webhookSvc := service.NewWebhookService(
		txStore,
		reg,
		f,
		fulfillment.NewNotifier("test-ns"),
		&config.Config{RecordRetentionDays: 90},
	)

	const basePath = "/payment"
	webhookPrefix := basePath + "/v1/webhook/"

	mux := http.NewServeMux()
	mux.HandleFunc(webhookPrefix, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rawBody, _ := io.ReadAll(r.Body)
		providerName := strings.TrimPrefix(r.URL.Path, webhookPrefix)

		headers := make(map[string]string, len(r.Header))
		for k, v := range r.Header {
			if len(v) > 0 {
				headers[strings.ToLower(k)] = v[0]
			}
		}

		resp, svcErr := webhookSvc.HandleWebhook(r.Context(), &pb.WebhookRequest{
			ProviderName: providerName,
			RawPayload:   rawBody,
			Headers:      headers,
		})

		w.Header().Set("Content-Type", "application/json")
		if svcErr != nil {
			st, _ := status.FromError(svcErr)
			switch st.Code() {
			case codes.Internal:
				w.WriteHeader(http.StatusInternalServerError)
			default:
				w.WriteHeader(http.StatusBadRequest)
			}
			if prov, err := reg.Get(providerName); err == nil {
				if errAcker, ok := prov.(adapter.WebhookErrorAcknowledger); ok {
					w.Write(errAcker.WebhookErrorAckBody()) //nolint:errcheck
					return
				}
			}
			json.NewEncoder(w).Encode(map[string]string{"message": svcErr.Error()}) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusOK)
		if prov, err := reg.Get(providerName); err == nil {
			if acker, ok := prov.(adapter.WebhookAcknowledger); ok {
				w.Write(acker.WebhookAckBody()) //nolint:errcheck
				return
			}
		}
		if resp != nil {
			json.NewEncoder(w).Encode(resp) //nolint:errcheck
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, txStore
}

func postWebhook(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	resp, err := http.Post(srv.URL+"/payment/v1/webhook/dana", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	t.Logf("Webhook response [%d]: %s", resp.StatusCode, string(body))
	return body
}

// Scenario 1: DANA sends notification → return 2005600 (success)
func TestIntegration_FinishNotify_Success(t *testing.T) {
	txID := uuid.New().String()
	srv, _ := buildWebhookServer(t, txID, &okFulfiller{})

	body := postWebhook(t, srv)
	assert.JSONEq(t, `{"responseCode":"2005600","responseMessage":"Successful"}`, string(body))
}

// Scenario 2: DANA sends notification → return 5005601 (internal server error)
func TestIntegration_FinishNotify_InternalServerError(t *testing.T) {
	txID := uuid.New().String()
	srv, txStore := buildWebhookServer(t, txID, &failFulfiller{})

	body := postWebhook(t, srv)
	assert.JSONEq(t, `{"responseCode":"5005601","responseMessage":"Internal Server Error"}`, string(body))

	tx, err := txStore.FindByID(context.Background(), txID)
	require.NoError(t, err)
	assert.Equal(t, model.StatusPending, tx.Status, "transaction must reset to PENDING for retry")
}

// Scenario 3: DANA retries after error → return 2005600 (success on retry)
func TestIntegration_FinishNotify_RetrySuccess(t *testing.T) {
	txID := uuid.New().String()

	// First call: fulfillment fails → 5005601, transaction stays PENDING
	srv, txStore := buildWebhookServer(t, txID, &failFulfiller{})
	firstBody := postWebhook(t, srv)
	assert.JSONEq(t, `{"responseCode":"5005601","responseMessage":"Internal Server Error"}`, string(firstBody))
	srv.Close()

	// Verify PENDING reset
	tx, err := txStore.FindByID(context.Background(), txID)
	require.NoError(t, err)
	require.Equal(t, model.StatusPending, tx.Status)

	// DANA retries: build a new server against the same store, now with okFulfiller
	stub := &stubDanaAdapter{internalOrderID: txID}
	reg := adapter.NewRegistry()
	reg.Register(stub)
	webhookSvc := service.NewWebhookService(
		txStore,
		reg,
		&okFulfiller{},
		fulfillment.NewNotifier("test-ns"),
		&config.Config{RecordRetentionDays: 90},
	)

	const webhookPrefix = "/payment/v1/webhook/"
	mux2 := http.NewServeMux()
	mux2.HandleFunc(webhookPrefix, func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ := io.ReadAll(r.Body)
		providerName := strings.TrimPrefix(r.URL.Path, webhookPrefix)
		headers := make(map[string]string)
		for k, v := range r.Header {
			if len(v) > 0 {
				headers[strings.ToLower(k)] = v[0]
			}
		}
		_, svcErr := webhookSvc.HandleWebhook(r.Context(), &pb.WebhookRequest{
			ProviderName: providerName,
			RawPayload:   rawBody,
			Headers:      headers,
		})
		w.Header().Set("Content-Type", "application/json")
		if svcErr != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write(stub.WebhookErrorAckBody()) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(stub.WebhookAckBody()) //nolint:errcheck
	})
	srv2 := httptest.NewServer(mux2)
	defer srv2.Close()

	retryBody, err := http.Post(srv2.URL+"/payment/v1/webhook/dana", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer retryBody.Body.Close()
	body, _ := io.ReadAll(retryBody.Body)
	t.Logf("Retry response [%d]: %s", retryBody.StatusCode, string(body))

	assert.JSONEq(t, `{"responseCode":"2005600","responseMessage":"Successful"}`, string(body))

	tx2, _ := txStore.FindByID(context.Background(), txID)
	assert.Equal(t, model.StatusFulfilled, tx2.Status)
}

// ─── SDK client helper (for scenarios that need the DANA API client directly) ─

func newDANAClientFromConfig(cfg *dana.Config) *danasdk.APIClient {
	sdkCfg := danaconfig.NewConfiguration()
	sdkCfg.APIKey = &danaconfig.APIKey{
		ENV:              cfg.Env,
		DANA_ENV:         cfg.Env,
		ORIGIN:           cfg.Origin,
		X_PARTNER_ID:     cfg.PartnerID,
		CHANNEL_ID:       cfg.ChannelID,
		CLIENT_SECRET:    cfg.ClientSecret,
		PRIVATE_KEY:      cfg.PrivateKeyPEM,
		PRIVATE_KEY_PATH: cfg.PrivateKeyPath,
	}
	return danasdk.NewAPIClient(sdkCfg)
}
