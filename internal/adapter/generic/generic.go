package generic

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/tidwall/gjson"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/config"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
)

// Adapter implements adapter.PaymentProvider entirely via environment variable configuration.
// No code changes are required to add a new provider — only new GENERIC_{NAME}_* env vars.
type Adapter struct {
	name       string
	cfg        *config.GenericProviderConfig
	httpClient *http.Client

	// Pre-parsed templates (fail fast at construction time)
	createBodyTmpl *template.Template
	statusURLTmpl  *template.Template
	refundBodyTmpl *template.Template
	cancelURLTmpl  *template.Template
	cancelBodyTmpl *template.Template
}

// New creates a Generic adapter from a validated config.
// Returns an error if any template is malformed — this surfaces misconfiguration at startup.
func New(cfg *config.GenericProviderConfig) (*Adapter, error) {
	if cfg.DisplayName == "" {
		cfg.DisplayName = cfg.Name
	}
	a := &Adapter{
		name:       cfg.Name,
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}

	var err error
	a.createBodyTmpl, err = template.New("create").Parse(cfg.CreateIntentBodyTemplate)
	if err != nil {
		return nil, fmt.Errorf("GENERIC_%s_CREATE_INTENT_BODY_TEMPLATE: invalid template: %w", strings.ToUpper(cfg.Name), err)
	}
	a.refundBodyTmpl, err = template.New("refund").Parse(cfg.RefundBodyTemplate)
	if err != nil {
		return nil, fmt.Errorf("GENERIC_%s_REFUND_BODY_TEMPLATE: invalid template: %w", strings.ToUpper(cfg.Name), err)
	}
	if cfg.StatusURLTemplate != "" {
		a.statusURLTmpl, err = template.New("status").Parse(cfg.StatusURLTemplate)
		if err != nil {
			return nil, fmt.Errorf("GENERIC_%s_STATUS_URL_TEMPLATE: invalid template: %w", strings.ToUpper(cfg.Name), err)
		}
	}
	if cfg.CancelURLTemplate != "" {
		a.cancelURLTmpl, err = template.New("cancel_url").Parse(cfg.CancelURLTemplate)
		if err != nil {
			return nil, fmt.Errorf("GENERIC_%s_CANCEL_URL_TEMPLATE: invalid template: %w", strings.ToUpper(cfg.Name), err)
		}
		if cfg.CancelBodyTemplate != "" {
			a.cancelBodyTmpl, err = template.New("cancel").Parse(cfg.CancelBodyTemplate)
			if err != nil {
				return nil, fmt.Errorf("GENERIC_%s_CANCEL_BODY_TEMPLATE: invalid template: %w", strings.ToUpper(cfg.Name), err)
			}
		}
	}
	return a, nil
}

func (a *Adapter) Info() adapter.ProviderInfo {
	return adapter.ProviderInfo{ID: a.name, DisplayName: a.cfg.DisplayName}
}

func (a *Adapter) ValidatePaymentInit(_ adapter.PaymentInitRequest) error {
	return nil
}

func (a *Adapter) CreatePaymentIntent(ctx context.Context, req adapter.PaymentInitRequest) (*adapter.PaymentIntent, error) {
	body, err := a.renderTemplate(a.createBodyTmpl, map[string]any{
		"OrderID":       req.InternalOrderID,
		"Amount":        req.Amount,
		"CurrencyCode":  req.CurrencyCode,
		"CallbackURL":   req.CallbackURL,
		"Description":   req.Description,
		"ExpirySeconds": int64(req.ExpiryDuration.Seconds()),
	})
	if err != nil {
		return nil, fmt.Errorf("render create body: %w", err)
	}

	respBody, err := a.doRequest(ctx, http.MethodPost, a.cfg.CreateIntentURL, body)
	if err != nil {
		return nil, fmt.Errorf("create intent request: %w", err)
	}

	slog.Info("create intent response", "provider", a.name, "body", respBody, "payment_url_path", a.cfg.PaymentURLJSONPath, "payment_url", gjson.Get(respBody, a.cfg.PaymentURLJSONPath).String(), "qr_path", a.cfg.QRCodeDataJSONPath, "qr_data", gjson.Get(respBody, a.cfg.QRCodeDataJSONPath).String())
	paymentURL := gjson.Get(respBody, a.cfg.PaymentURLJSONPath).String()
	providerTxID := gjson.Get(respBody, a.cfg.ProviderTxIDJSONPath).String()

	var qrCodeData string
	if a.cfg.QRCodeDataJSONPath != "" {
		qrCodeData = gjson.Get(respBody, a.cfg.QRCodeDataJSONPath).String()
	}

	return &adapter.PaymentIntent{
		ProviderTransactionID: providerTxID,
		PaymentURL:            paymentURL,
		QRCodeData:            qrCodeData,
		ExpiresAt:             time.Now().Add(req.ExpiryDuration),
	}, nil
}

func (a *Adapter) GetPaymentStatus(ctx context.Context, providerTxID string) (*adapter.ProviderPaymentStatus, error) {
	if a.statusURLTmpl == nil {
		return nil, adapter.ErrNotSupported
	}

	statusURL, err := a.renderTemplate(a.statusURLTmpl, map[string]any{
		"ProviderTxID": providerTxID,
	})
	if err != nil {
		return nil, fmt.Errorf("render status URL: %w", err)
	}

	method := a.cfg.StatusMethod
	if method == "" {
		method = http.MethodGet
	}

	respBody, err := a.doRequest(ctx, method, statusURL, "")
	if err != nil {
		return nil, fmt.Errorf("status request: %w", err)
	}

	statusVal := gjson.Get(respBody, a.cfg.StatusPaymentStatusPath).String()
	status := a.mapStatus(statusVal)

	return &adapter.ProviderPaymentStatus{
		ProviderTxID: providerTxID,
		Status:       status,
	}, nil
}

func (a *Adapter) SyncTransactionStatus(ctx context.Context, tx *model.Transaction) (*adapter.ProviderSyncResult, error) {
	if tx == nil || strings.TrimSpace(tx.ProviderTxID) == "" {
		return &adapter.ProviderSyncResult{
			PaymentStatus: adapter.SyncPaymentStatusUnsupported,
			RefundStatus:  adapter.SyncRefundStatusUnsupported,
			Message:       "missing provider transaction id",
		}, nil
	}
	if a.statusURLTmpl == nil {
		return &adapter.ProviderSyncResult{
			ProviderTxID:  tx.ProviderTxID,
			PaymentStatus: adapter.SyncPaymentStatusUnsupported,
			RefundStatus:  adapter.SyncRefundStatusUnsupported,
			Message:       "generic provider status query is not configured",
		}, nil
	}

	statusURL, err := a.renderTemplate(a.statusURLTmpl, map[string]any{
		"ProviderTxID": tx.ProviderTxID,
	})
	if err != nil {
		return nil, fmt.Errorf("render status URL: %w", err)
	}
	method := a.cfg.StatusMethod
	if method == "" {
		method = http.MethodGet
	}
	respBody, err := a.doRequest(ctx, method, statusURL, "")
	if err != nil {
		return nil, fmt.Errorf("status request: %w", err)
	}

	statusVal := gjson.Get(respBody, a.cfg.StatusPaymentStatusPath).String()
	paymentStatus := adapter.SyncPaymentStatusFromPaymentStatus(a.mapStatus(statusVal))
	refundStatus := adapter.SyncRefundStatusNone
	var refundAmount int64
	var refundCurrency string
	if a.cfg.StatusRefundValue != "" && statusVal == a.cfg.StatusRefundValue {
		refundStatus = adapter.SyncRefundStatusRefunded
		paymentStatus = adapter.SyncPaymentStatusPaid
	}
	if a.cfg.StatusRefundAmountPath != "" {
		refundAmount = gjson.Get(respBody, a.cfg.StatusRefundAmountPath).Int()
		if refundAmount > 0 && refundAmount < tx.Amount {
			refundStatus = adapter.SyncRefundStatusPartialRefunded
		}
	}
	if a.cfg.StatusRefundCurrencyPath != "" {
		refundCurrency = gjson.Get(respBody, a.cfg.StatusRefundCurrencyPath).String()
	}
	return &adapter.ProviderSyncResult{
		ProviderTxID:       tx.ProviderTxID,
		PaymentStatus:      paymentStatus,
		RefundStatus:       refundStatus,
		RawPaymentStatus:   statusVal,
		RawRefundStatus:    statusVal,
		RefundAmount:       refundAmount,
		RefundCurrencyCode: refundCurrency,
		Message:            "generic provider status synced",
	}, nil
}

func (a *Adapter) ValidateWebhookSignature(_ context.Context, rawBody []byte, headers map[string]string) error {
	method := strings.ToUpper(a.cfg.WebhookSignatureMethod)
	if method == "NONE" {
		return nil
	}

	provided := headers[a.cfg.WebhookSignatureHeader]
	if provided == "" {
		return fmt.Errorf("missing signature header %q", a.cfg.WebhookSignatureHeader)
	}

	computed, err := a.computeHMAC(method, a.cfg.WebhookSignatureSecret, rawBody)
	if err != nil {
		return fmt.Errorf("compute hmac: %w", err)
	}

	providedBytes, err := hex.DecodeString(provided)
	if err != nil {
		// Some providers send base64 or raw — try raw byte comparison
		providedBytes = []byte(provided)
	}

	if subtle.ConstantTimeCompare([]byte(computed), providedBytes) != 1 &&
		subtle.ConstantTimeCompare([]byte(hex.EncodeToString([]byte(provided))), []byte(computed)) != 1 {
		// Try comparing the hex strings directly (both as strings)
		computedHex := computed
		if subtle.ConstantTimeCompare([]byte(computedHex), []byte(provided)) != 1 {
			return fmt.Errorf("webhook signature mismatch")
		}
	}
	return nil
}

func (a *Adapter) HandleWebhook(_ context.Context, rawBody []byte, _ map[string]string) (*adapter.PaymentResult, error) {
	bodyStr := string(rawBody)

	txID := gjson.Get(bodyStr, a.cfg.WebhookTxIDJSONPath).String()
	if txID == "" {
		return nil, fmt.Errorf("webhook missing transaction ID at path %q", a.cfg.WebhookTxIDJSONPath)
	}

	statusVal := gjson.Get(bodyStr, a.cfg.WebhookSuccessStatusPath).String()
	status := a.mapWebhookStatus(statusVal)

	return &adapter.PaymentResult{
		InternalOrderID:       txID,
		ProviderTransactionID: txID,
		Status:                status,
		RawProviderStatus:     statusVal,
		RawPayload:            rawBody,
	}, nil
}

func (a *Adapter) RefundPayment(ctx context.Context, internalOrderID string, providerTxID string, amount int64, currencyCode string) error {
	body, err := a.renderTemplate(a.refundBodyTmpl, map[string]any{
		"OrderID":      internalOrderID,
		"Amount":       amount,
		"CurrencyCode": currencyCode,
		"ProviderTxID": providerTxID,
	})
	if err != nil {
		return fmt.Errorf("render refund body: %w", err)
	}

	_, err = a.doRequest(ctx, http.MethodPost, a.cfg.RefundURL, body)
	return err
}

func (a *Adapter) CancelPayment(ctx context.Context, tx *model.Transaction, reason string) (*adapter.CancelResult, error) {
	if a.cancelURLTmpl == nil {
		return &adapter.CancelResult{Status: adapter.CancelStatusUnsupported, Message: "generic provider cancel is not configured"}, nil
	}
	cancelURL, err := a.renderTemplate(a.cancelURLTmpl, map[string]any{
		"OrderID":      tx.ID,
		"Amount":       tx.Amount,
		"CurrencyCode": tx.CurrencyCode,
		"ProviderTxID": tx.ProviderTxID,
		"Reason":       reason,
	})
	if err != nil {
		return nil, fmt.Errorf("render cancel URL: %w", err)
	}
	body := ""
	if a.cancelBodyTmpl != nil {
		body, err = a.renderTemplate(a.cancelBodyTmpl, map[string]any{
			"OrderID":      tx.ID,
			"Amount":       tx.Amount,
			"CurrencyCode": tx.CurrencyCode,
			"ProviderTxID": tx.ProviderTxID,
			"Reason":       reason,
		})
		if err != nil {
			return nil, fmt.Errorf("render cancel body: %w", err)
		}
	}
	method := a.cfg.CancelMethod
	if method == "" {
		method = http.MethodPost
	}
	respBody, err := a.doRequest(ctx, method, cancelURL, body)
	if err != nil {
		return nil, err
	}
	rawStatus := ""
	if a.cfg.CancelStatusPath != "" {
		rawStatus = gjson.Get(respBody, a.cfg.CancelStatusPath).String()
	}
	return &adapter.CancelResult{
		Status:         a.mapCancelStatus(rawStatus),
		ProviderStatus: rawStatus,
		ProviderTxID:   tx.ProviderTxID,
		Message:        "generic provider cancel completed",
	}, nil
}

func (a *Adapter) ValidateCredentials(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.CreateIntentURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set(a.cfg.AuthHeader, a.cfg.AuthValue)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 401/403 means credentials are definitively invalid.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("credential validation failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func (a *Adapter) doRequest(ctx context.Context, method, url, body string) (string, error) {
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return "", err
	}
	req.Header.Set(a.cfg.AuthHeader, a.cfg.AuthValue)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("provider returned HTTP %d: %s", resp.StatusCode, string(respBytes))
	}
	return string(respBytes), nil
}

func (a *Adapter) renderTemplate(tmpl *template.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (a *Adapter) computeHMAC(method, secret string, body []byte) (string, error) {
	var h hash.Hash
	switch method {
	case "HMAC_SHA256":
		h = hmac.New(sha256.New, []byte(secret))
	case "HMAC_SHA512":
		h = hmac.New(sha512.New, []byte(secret))
	default:
		return "", fmt.Errorf("unsupported signature method: %s", method)
	}
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (a *Adapter) mapStatus(val string) adapter.PaymentStatus {
	if val == a.cfg.StatusSuccessValue {
		return adapter.PaymentStatusSuccess
	}
	if val == a.cfg.StatusPendingValue {
		return adapter.PaymentStatusPending
	}
	if a.cfg.StatusRefundValue != "" && val == a.cfg.StatusRefundValue {
		return adapter.PaymentStatusRefunded
	}
	for _, fv := range a.cfg.StatusFailedValues {
		if val == fv {
			return adapter.PaymentStatusFailed
		}
	}
	return adapter.PaymentStatusPending
}

func (a *Adapter) mapWebhookStatus(val string) adapter.PaymentStatus {
	if val == a.cfg.WebhookSuccessStatusValue {
		return adapter.PaymentStatusSuccess
	}
	if val == a.cfg.WebhookFailedStatusValue {
		return adapter.PaymentStatusFailed
	}
	return adapter.PaymentStatusPending
}

func (a *Adapter) mapCancelStatus(val string) adapter.CancelStatus {
	if matchesAny(val, a.cfg.CancelSuccessValues) {
		return adapter.CancelStatusCanceled
	}
	if matchesAny(val, a.cfg.CancelExpiredValues) {
		return adapter.CancelStatusExpired
	}
	if matchesAny(val, a.cfg.CancelPaidValues) {
		return adapter.CancelStatusAlreadyPaid
	}
	if matchesAny(val, a.cfg.CancelPendingValues) {
		return adapter.CancelStatusPending
	}
	if val == "" && len(a.cfg.CancelSuccessValues) == 0 {
		return adapter.CancelStatusCanceled
	}
	return adapter.CancelStatusFailed
}

func matchesAny(val string, values []string) bool {
	for _, candidate := range values {
		if val == candidate {
			return true
		}
	}
	return false
}
