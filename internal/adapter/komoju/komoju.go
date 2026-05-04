package komoju

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/govalues/money"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
)

const (
	signatureHeader        = "x-komoju-signature"
	apiVersionHeader       = "X-KOMOJU-API-VERSION"
	idempotencyHeader      = "X-KOMOJU-IDEMPOTENCY"
	maxSessionExpirySecond = 86400
)

type Adapter struct {
	cfg        *Config
	httpClient *http.Client
}

func New(cfg *Config) (*Adapter, error) {
	if cfg == nil {
		return nil, fmt.Errorf("komoju config is required")
	}
	if cfg.ProviderID == "" {
		cfg.ProviderID = "provider_komoju"
	}
	if cfg.DisplayName == "" {
		cfg.DisplayName = "KOMOJU"
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = defaultAPIBaseURL
	}
	return &Adapter{
		cfg:        cfg,
		httpClient: http.DefaultClient,
	}, nil
}

func (a *Adapter) Info() adapter.ProviderInfo {
	return adapter.ProviderInfo{ID: a.cfg.ProviderID, DisplayName: a.cfg.DisplayName}
}

func (a *Adapter) ValidatePaymentInit(req adapter.PaymentInitRequest) error {
	currency := strings.ToUpper(strings.TrimSpace(req.CurrencyCode))
	if _, ok := a.cfg.AllowedCurrencies[currency]; !ok {
		return fmt.Errorf("komoju currency %q is not allowed; allowed=%v", currency, sortedCodes(a.cfg.AllowedCurrencies))
	}
	_, err := toKomojuAmount(req.Amount, currency)
	return err
}

func (a *Adapter) CreatePaymentIntent(ctx context.Context, req adapter.PaymentInitRequest) (*adapter.PaymentIntent, error) {
	currency := strings.ToUpper(strings.TrimSpace(req.CurrencyCode))
	if err := a.ValidatePaymentInit(req); err != nil {
		return nil, err
	}

	expiresIn := int(req.ExpiryDuration.Seconds())
	if expiresIn <= 0 || expiresIn > maxSessionExpirySecond {
		expiresIn = maxSessionExpirySecond
	}
	amount, err := toKomojuAmount(req.Amount, currency)
	if err != nil {
		return nil, err
	}
	createReq := createSessionRequest{
		Mode:             "payment",
		Amount:           amount,
		Currency:         currency,
		ReturnURL:        req.ReturnURL,
		ExpiresInSeconds: expiresIn,
		DefaultLocale:    a.cfg.DefaultLocale,
		PaymentData: paymentData{
			ExternalOrderNum: req.InternalOrderID,
			Capture:          "auto",
		},
		Metadata: map[string]string{
			"internalOrderId": req.InternalOrderID,
			"userId":          req.UserID,
			"regionCode":      strings.ToUpper(strings.TrimSpace(req.RegionCode)),
		},
	}

	var session sessionResponse
	if err := a.doJSON(ctx, http.MethodPost, "/api/v1/sessions", "session-"+req.InternalOrderID, createReq, &session); err != nil {
		return nil, err
	}
	if session.ID == "" {
		return nil, fmt.Errorf("komoju CreateSession: empty session id")
	}
	if session.SessionURL == "" {
		return nil, fmt.Errorf("komoju CreateSession: empty session_url")
	}

	slog.Info("komoju CreateSession success",
		"internalOrderID", req.InternalOrderID,
		"sessionID", session.ID,
		"currency", currency,
	)

	return &adapter.PaymentIntent{
		ProviderTransactionID: session.ID,
		PaymentURL:            session.SessionURL,
		ExpiresAt:             time.Now().Add(time.Duration(expiresIn) * time.Second),
	}, nil
}

func (a *Adapter) GetPaymentStatus(ctx context.Context, providerTxID string) (*adapter.ProviderPaymentStatus, error) {
	var session sessionResponse
	err := a.doJSON(ctx, http.MethodGet, "/api/v1/sessions/"+providerTxID, "", nil, &session)
	if err == nil {
		return providerStatusFromSession(providerTxID, &session)
	}
	if !isHTTPStatus(err, http.StatusNotFound) {
		return nil, err
	}

	var payment paymentResponse
	if err := a.doJSON(ctx, http.MethodGet, "/api/v1/payments/"+providerTxID, "", nil, &payment); err != nil {
		return nil, err
	}
	return providerStatusFromPayment(&payment)
}

func (a *Adapter) SyncTransactionStatus(ctx context.Context, tx *model.Transaction) (*adapter.ProviderSyncResult, error) {
	if tx == nil || strings.TrimSpace(tx.ProviderTxID) == "" {
		return &adapter.ProviderSyncResult{
			PaymentStatus: adapter.SyncPaymentStatusUnsupported,
			RefundStatus:  adapter.SyncRefundStatusUnsupported,
			Message:       "missing provider transaction id",
		}, nil
	}
	ps, err := a.GetPaymentStatus(ctx, tx.ProviderTxID)
	if err != nil {
		return nil, err
	}
	paymentStatus := adapter.SyncPaymentStatusFromPaymentStatus(ps.Status)
	refundStatus := adapter.SyncRefundStatusFromPaymentStatus(ps.Status)
	refundAmount := int64(0)
	refundCurrency := ""
	if refundStatus == adapter.SyncRefundStatusRefunded {
		paymentStatus = adapter.SyncPaymentStatusPaid
		refundAmount = tx.Amount
		refundCurrency = tx.CurrencyCode
	}
	return &adapter.ProviderSyncResult{
		ProviderTxID:       ps.ProviderTxID,
		PaymentStatus:      paymentStatus,
		RefundStatus:       refundStatus,
		RawPaymentStatus:   string(ps.Status),
		RawRefundStatus:    string(ps.Status),
		RefundAmount:       refundAmount,
		RefundCurrencyCode: refundCurrency,
		Message:            "komoju payment status synced",
	}, nil
}

func (a *Adapter) ValidateWebhookSignature(_ context.Context, rawBody []byte, headers map[string]string) error {
	provided := strings.TrimSpace(headers[signatureHeader])
	if provided == "" {
		return fmt.Errorf("missing %s header", signatureHeader)
	}
	mac := hmac.New(sha256.New, []byte(a.cfg.WebhookSecret))
	mac.Write(rawBody)
	expected := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(provided)), []byte(expected)) != 1 {
		return fmt.Errorf("komoju webhook signature mismatch")
	}
	return nil
}

func (a *Adapter) HandleWebhook(_ context.Context, rawBody []byte, _ map[string]string) (*adapter.PaymentResult, error) {
	var event webhookEvent
	if err := json.Unmarshal(rawBody, &event); err != nil {
		return nil, fmt.Errorf("komoju webhook parse: %w", err)
	}
	if event.Data.ID == "" {
		return nil, fmt.Errorf("komoju webhook: missing data.id")
	}
	internalOrderID := strings.TrimSpace(event.Data.ExternalOrderNum)
	if internalOrderID == "" {
		internalOrderID = strings.TrimSpace(event.Data.Metadata["internalOrderId"])
	}
	if internalOrderID == "" {
		return nil, fmt.Errorf("komoju webhook: missing external_order_num or metadata.internalOrderId")
	}

	status := mapPaymentStatus(event.Data.Status)
	failureReason := ""
	if status == adapter.PaymentStatusFailed {
		failureReason = event.Data.Status
	}
	amount, err := fromKomojuAmount(event.Data.Amount, event.Data.Currency)
	if err != nil {
		return nil, err
	}

	return &adapter.PaymentResult{
		ProviderTransactionID: event.Data.ID,
		InternalOrderID:       internalOrderID,
		Status:                status,
		RawProviderStatus:     event.Data.Status,
		Amount:                amount,
		CurrencyCode:          event.Data.Currency,
		FailureReason:         failureReason,
		RawPayload:            rawBody,
	}, nil
}

func (a *Adapter) RefundPayment(ctx context.Context, internalOrderID string, providerTxID string, amount int64, currencyCode string) error {
	refundAmount, err := toKomojuAmount(amount, currencyCode)
	if err != nil {
		return err
	}
	body := refundRequest{Amount: refundAmount}
	var out map[string]any
	return a.doJSON(ctx, http.MethodPost, "/api/v1/payments/"+providerTxID+"/refund", "refund-"+internalOrderID, body, &out)
}

func (a *Adapter) CancelPayment(ctx context.Context, tx *model.Transaction, reason string) (*adapter.CancelResult, error) {
	var session sessionResponse
	err := a.doJSON(ctx, http.MethodPost, "/api/v1/sessions/"+tx.ProviderTxID+"/cancel", "cancel-"+tx.ID, nil, &session)
	if err == nil {
		return cancelResultFromKomojuSession(tx.ProviderTxID, &session), nil
	}
	if !isHTTPStatus(err, http.StatusUnprocessableEntity) && !isHTTPStatus(err, http.StatusNotFound) {
		return nil, err
	}

	status, statusErr := a.GetPaymentStatus(ctx, tx.ProviderTxID)
	if statusErr != nil {
		return &adapter.CancelResult{
			Status:         adapter.CancelStatusFailed,
			ProviderStatus: "cancel_failed",
			FailureReason:  err.Error(),
		}, nil
	}
	switch status.Status {
	case adapter.PaymentStatusSuccess, adapter.PaymentStatusRefunded:
		return &adapter.CancelResult{Status: adapter.CancelStatusAlreadyPaid, ProviderStatus: string(status.Status), ProviderTxID: status.ProviderTxID}, nil
	case adapter.PaymentStatusFailed, adapter.PaymentStatusCanceled:
		return &adapter.CancelResult{Status: adapter.CancelStatusCanceled, ProviderStatus: string(status.Status), ProviderTxID: status.ProviderTxID}, nil
	case adapter.PaymentStatusExpired:
		return &adapter.CancelResult{Status: adapter.CancelStatusExpired, ProviderStatus: string(status.Status), ProviderTxID: status.ProviderTxID}, nil
	}

	var payment paymentResponse
	paymentID := status.ProviderTxID
	if paymentID == "" {
		paymentID = tx.ProviderTxID
	}
	if cancelErr := a.doJSON(ctx, http.MethodPost, "/api/v1/payments/"+paymentID+"/cancel", "cancel-"+tx.ID, nil, &payment); cancelErr != nil {
		return nil, cancelErr
	}
	return cancelResultFromKomojuPayment(&payment), nil
}

func (a *Adapter) ValidateCredentials(ctx context.Context) error {
	var out map[string]any
	if err := a.doJSON(ctx, http.MethodGet, "/api/v1/payments?per_page=1", "", nil, &out); err != nil {
		if statusErr, ok := err.(*httpStatusError); ok && (statusErr.statusCode == http.StatusUnauthorized || statusErr.statusCode == http.StatusForbidden) {
			return fmt.Errorf("komoju ValidateCredentials: HTTP %d - check KOMOJU_SECRET_KEY", statusErr.statusCode)
		}
		return err
	}
	return nil
}

func (a *Adapter) doJSON(ctx context.Context, method string, path string, idempotencyKey string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, a.cfg.APIBaseURL+path, reader)
	if err != nil {
		return err
	}
	req.SetBasicAuth(a.cfg.SecretKey, "")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(apiVersionHeader, a.cfg.APIVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set(idempotencyHeader, idempotencyKey)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpStatusError{statusCode: resp.StatusCode, body: string(respBody)}
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("komoju response parse: %w", err)
	}
	return nil
}

type httpStatusError struct {
	statusCode int
	body       string
}

func (e *httpStatusError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("komoju API HTTP %d", e.statusCode)
	}
	return fmt.Sprintf("komoju API HTTP %d: %s", e.statusCode, e.body)
}

func isHTTPStatus(err error, statusCode int) bool {
	statusErr, ok := err.(*httpStatusError)
	return ok && statusErr.statusCode == statusCode
}

type createSessionRequest struct {
	Mode             string            `json:"mode"`
	Amount           int64             `json:"amount"`
	Currency         string            `json:"currency"`
	ReturnURL        string            `json:"return_url,omitempty"`
	ExpiresInSeconds int               `json:"expires_in_seconds,omitempty"`
	DefaultLocale    string            `json:"default_locale,omitempty"`
	PaymentData      paymentData       `json:"payment_data"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

type paymentData struct {
	ExternalOrderNum string `json:"external_order_num"`
	Capture          string `json:"capture"`
}

type sessionResponse struct {
	ID         string           `json:"id"`
	Status     string           `json:"status"`
	Amount     int64            `json:"amount"`
	Currency   string           `json:"currency"`
	SessionURL string           `json:"session_url"`
	Payment    *paymentResponse `json:"payment"`
}

type paymentResponse struct {
	ID               string            `json:"id"`
	Status           string            `json:"status"`
	Amount           int64             `json:"amount"`
	Total            int64             `json:"total"`
	Currency         string            `json:"currency"`
	ExternalOrderNum string            `json:"external_order_num"`
	Metadata         map[string]string `json:"metadata"`
}

type webhookEvent struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Resource  string          `json:"resource"`
	Data      paymentResponse `json:"data"`
	CreatedAt string          `json:"created_at"`
}

type refundRequest struct {
	Amount int64 `json:"amount,omitempty"`
}

func providerStatusFromSession(fallbackTxID string, session *sessionResponse) (*adapter.ProviderPaymentStatus, error) {
	if session.Payment != nil && session.Payment.ID != "" {
		return providerStatusFromPayment(session.Payment)
	}
	amount, err := fromKomojuAmount(session.Amount, session.Currency)
	if err != nil {
		return nil, err
	}
	return &adapter.ProviderPaymentStatus{
		ProviderTxID: fallbackTxID,
		Status:       mapSessionStatus(session.Status),
		Amount:       amount,
		CurrencyCode: session.Currency,
	}, nil
}

func providerStatusFromPayment(payment *paymentResponse) (*adapter.ProviderPaymentStatus, error) {
	amount := payment.Total
	if amount == 0 {
		amount = payment.Amount
	}
	normalizedAmount, err := fromKomojuAmount(amount, payment.Currency)
	if err != nil {
		return nil, err
	}
	return &adapter.ProviderPaymentStatus{
		ProviderTxID:  payment.ID,
		Status:        mapPaymentStatus(payment.Status),
		Amount:        normalizedAmount,
		CurrencyCode:  payment.Currency,
		FailureReason: failureReason(payment.Status),
	}, nil
}

func cancelResultFromKomojuSession(fallbackTxID string, session *sessionResponse) *adapter.CancelResult {
	if session.Payment != nil && session.Payment.ID != "" {
		return cancelResultFromKomojuPayment(session.Payment)
	}
	status := mapKomojuCancelStatus(session.Status)
	return &adapter.CancelResult{
		Status:         status,
		ProviderStatus: session.Status,
		ProviderTxID:   fallbackTxID,
	}
}

func cancelResultFromKomojuPayment(payment *paymentResponse) *adapter.CancelResult {
	return &adapter.CancelResult{
		Status:         mapKomojuCancelStatus(payment.Status),
		ProviderStatus: payment.Status,
		ProviderTxID:   payment.ID,
	}
}

func mapKomojuCancelStatus(raw string) adapter.CancelStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "cancelled", "canceled":
		return adapter.CancelStatusCanceled
	case "expired":
		return adapter.CancelStatusExpired
	case "captured", "refunded":
		return adapter.CancelStatusAlreadyPaid
	case "pending", "authorized":
		return adapter.CancelStatusPending
	default:
		return adapter.CancelStatusFailed
	}
}

func mapSessionStatus(raw string) adapter.PaymentStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "cancelled", "canceled":
		return adapter.PaymentStatusCanceled
	case "expired":
		return adapter.PaymentStatusExpired
	case "failed":
		return adapter.PaymentStatusFailed
	default:
		return adapter.PaymentStatusPending
	}
}

func mapPaymentStatus(raw string) adapter.PaymentStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "captured":
		return adapter.PaymentStatusSuccess
	case "refunded":
		return adapter.PaymentStatusRefunded
	case "cancelled", "canceled":
		return adapter.PaymentStatusCanceled
	case "expired":
		return adapter.PaymentStatusExpired
	case "failed":
		return adapter.PaymentStatusFailed
	default:
		return adapter.PaymentStatusPending
	}
}

func failureReason(raw string) string {
	if mapPaymentStatus(raw) == adapter.PaymentStatusFailed {
		return raw
	}
	return ""
}

func toKomojuAmount(amount int64, currencyCode string) (int64, error) {
	factor, err := currencyScaleFactor(currencyCode)
	if err != nil {
		return 0, err
	}
	return amount * factor, nil
}

func fromKomojuAmount(amount int64, currencyCode string) (int64, error) {
	factor, err := currencyScaleFactor(currencyCode)
	if err != nil {
		return 0, err
	}
	return amount / factor, nil
}

func currencyScaleFactor(currencyCode string) (int64, error) {
	currencyCode = strings.ToUpper(strings.TrimSpace(currencyCode))
	curr, err := money.ParseCurr(currencyCode)
	if err != nil {
		return 0, fmt.Errorf("komoju unsupported currency %q: %w", currencyCode, err)
	}
	var factor int64 = 1
	for i := 0; i < curr.Scale(); i++ {
		factor *= 10
	}
	return factor, nil
}
