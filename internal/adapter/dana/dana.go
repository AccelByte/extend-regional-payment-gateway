package dana

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	danasdk "github.com/dana-id/dana-go/v2"
	danaconfig "github.com/dana-id/dana-go/v2/config"
	paymentgw "github.com/dana-id/dana-go/v2/payment_gateway/v1"
	"github.com/dana-id/dana-go/v2/webhook"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
)

const webhookPath = "/payment/v1/webhook/dana"

// Adapter implements adapter.PaymentProvider using the DANA Go SDK.
// RSA request signing and webhook signature verification are handled by the SDK.
type Adapter struct {
	cfg    *Config
	client *danasdk.APIClient
}

// New creates a DANA adapter from a validated Config.
func New(cfg *Config) (*Adapter, error) {
	sdkCfg := danaconfig.NewConfiguration()
	sdkCfg.APIKey = &danaconfig.APIKey{
		ENV:              cfg.Env,
		DANA_ENV:         cfg.Env,
		ORIGIN:           cfg.Origin,
		X_PARTNER_ID:     cfg.PartnerID, // "Client ID" from DANA dashboard → X-PARTNER-ID header
		CHANNEL_ID:       cfg.ChannelID,
		CLIENT_SECRET:    cfg.ClientSecret,
		PRIVATE_KEY_PATH: cfg.PrivateKeyPath,
	}

	if cfg.PrivateKeyPEM != "" {
		sdkCfg.APIKey.PRIVATE_KEY = normalizePEM(cfg.PrivateKeyPEM, "RSA PRIVATE KEY")
	}

	return &Adapter{
		cfg:    cfg,
		client: danasdk.NewAPIClient(sdkCfg),
	}, nil
}

// Name returns "dana".
func (a *Adapter) Name() string { return "dana" }

// CreatePaymentIntent creates a DANA payment order and returns the redirect URL.
// Checkout mode is controlled by DANA_CHECKOUT_MODE:
//   - "DANA_BALANCE" (default): balance-only direct payment
//   - "DANA_GAPURA": Gapura Hosted Checkout — user picks payment method on DANA's page
func (a *Adapter) CreatePaymentIntent(ctx context.Context, req adapter.PaymentInitRequest) (*adapter.PaymentIntent, error) {
	if !strings.EqualFold(req.CurrencyCode, "IDR") {
		return nil, fmt.Errorf("dana only supports IDR currency, got %s", req.CurrencyCode)
	}
	expiresAt := time.Now().Add(req.ExpiryDuration)
	jakartaOffset := time.FixedZone("WIB", 7*3600)
	validUpTo := expiresAt.In(jakartaOffset).Format("2006-01-02T15:04:05+07:00")

	var createReq paymentgw.CreateOrderRequest
	if strings.EqualFold(a.cfg.CheckoutMode, "DANA_GAPURA") {
		createReq = a.buildRedirectOrder(req, validUpTo)
	} else {
		createReq = a.buildAPIOrder(req, validUpTo)
	}

	resp, httpResp, err := a.client.PaymentGatewayAPI.CreateOrder(ctx).
		CreateOrderRequest(createReq).
		Execute()
	if err != nil {
		if httpResp != nil {
			body, _ := io.ReadAll(httpResp.Body)
			httpResp.Body.Close()
			slog.Error("dana CreateOrder failed", "status", httpResp.StatusCode, "body", string(body))
		}
		return nil, fmt.Errorf("dana CreateOrder: %w", err)
	}

	providerTxID := resp.GetReferenceNo()
	paymentURL := resp.GetWebRedirectUrl()

	if providerTxID == "" {
		return nil, fmt.Errorf("dana CreateOrder: empty referenceNo in response (responseCode=%s)", resp.ResponseCode)
	}

	slog.Info("dana CreateOrder success",
		"internalOrderID", req.InternalOrderID,
		"providerTxID", providerTxID,
		"paymentURL", paymentURL,
		"checkoutMode", a.cfg.CheckoutMode,
	)

	return &adapter.PaymentIntent{
		ProviderTransactionID: providerTxID,
		PaymentURL:            paymentURL,
		ExpiresAt:             expiresAt,
	}, nil
}

// danaTitle returns a non-empty title for the DANA order. DANA rejects blank titles.
func danaTitle(description string) string {
	if description != "" {
		return description
	}
	return "Payment"
}

// buildAPIOrder builds a direct balance-only DANA order request (DANA_CHECKOUT_MODE=API).
func (a *Adapter) buildAPIOrder(req adapter.PaymentInitRequest, validUpTo string) paymentgw.CreateOrderRequest {
	amountStr := formatDANAAmount(req.Amount)
	scenario := "API"
	merchantTransType := "SPECIAL_MOVIE"

	returnURL := req.ReturnURL
	if returnURL == "" {
		returnURL = req.CallbackURL
	}
	orderReq := paymentgw.CreateOrderByApiRequest{
		PartnerReferenceNo: req.InternalOrderID,
		MerchantId:         a.cfg.MerchantID,
		Amount:             paymentgw.Money{Value: amountStr, Currency: strings.ToUpper(req.CurrencyCode)},
		ValidUpTo:          validUpTo,
		UrlParams: []paymentgw.UrlParam{
			{Url: returnURL, Type: "PAY_RETURN", IsDeeplink: "N"},
			{Url: req.CallbackURL, Type: "NOTIFICATION", IsDeeplink: "N"},
		},
		PayOptionDetails: []paymentgw.PayOptionDetail{
			{
				PayMethod:   "BALANCE",
				PayOption:   "",
				TransAmount: paymentgw.Money{Value: amountStr, Currency: strings.ToUpper(req.CurrencyCode)},
			},
		},
		AdditionalInfo: &paymentgw.CreateOrderByApiAdditionalInfo{
			Mcc: "5732",
			Order: &paymentgw.OrderApiObject{
				OrderTitle:        danaTitle(req.Description),
				Scenario:          &scenario,
				MerchantTransType: &merchantTransType,
			},
			EnvInfo: paymentgw.EnvInfo{
				SourcePlatform: "IPG",
				TerminalType:   "SYSTEM",
			},
		},
	}
	if err := orderReq.AdditionalInfo.EnvInfo.SetOrderTerminalType("WEB"); err != nil {
		slog.Warn("dana: failed to set orderTerminalType", "error", err)
	}
	return paymentgw.CreateOrderByApiRequestAsCreateOrderRequest(&orderReq)
}

// buildRedirectOrder builds a Gapura Hosted Checkout order request (DANA_CHECKOUT_MODE=REDIRECT).
// DANA shows a hosted payment page where the user can choose from all available payment methods.
func (a *Adapter) buildRedirectOrder(req adapter.PaymentInitRequest, validUpTo string) paymentgw.CreateOrderRequest {
	returnURL := req.ReturnURL
	if returnURL == "" {
		returnURL = req.CallbackURL
	}
	orderReq := paymentgw.CreateOrderByRedirectRequest{
		PartnerReferenceNo: req.InternalOrderID,
		MerchantId:         a.cfg.MerchantID,
		Amount:             paymentgw.Money{Value: formatDANAAmount(req.Amount), Currency: strings.ToUpper(req.CurrencyCode)},
		ValidUpTo:          validUpTo,
		UrlParams: []paymentgw.UrlParam{
			{Url: returnURL, Type: "PAY_RETURN", IsDeeplink: "N"},
			{Url: req.CallbackURL, Type: "NOTIFICATION", IsDeeplink: "N"},
		},
		AdditionalInfo: &paymentgw.CreateOrderByRedirectAdditionalInfo{
			Mcc: "5732",
			EnvInfo: paymentgw.EnvInfo{
				SourcePlatform: "IPG",
				TerminalType:   "SYSTEM",
			},
			Order: &paymentgw.OrderRedirectObject{
				OrderTitle: req.Description,
			},
		},
	}
	return paymentgw.CreateOrderByRedirectRequestAsCreateOrderRequest(&orderReq)
}

// GetPaymentStatus queries DANA for the current status of a transaction.
func (a *Adapter) GetPaymentStatus(ctx context.Context, providerTxID string) (*adapter.ProviderPaymentStatus, error) {
	queryReq := paymentgw.QueryPaymentRequest{
		MerchantId:          a.cfg.MerchantID,
		ServiceCode:         "54",
		OriginalReferenceNo: ptrStr(providerTxID),
	}

	resp, _, err := a.client.PaymentGatewayAPI.QueryPayment(ctx).
		QueryPaymentRequest(queryReq).
		Execute()
	if err != nil {
		return nil, fmt.Errorf("dana QueryPayment: %w", err)
	}

	rawStatus := resp.LatestTransactionStatus
	status := mapDANAStatus(rawStatus)

	var amount int64
	var currencyCode string
	if resp.Amount != nil {
		amount = parseDANAAmount(resp.Amount.Value)
		currencyCode = resp.Amount.Currency
	}

	return &adapter.ProviderPaymentStatus{
		ProviderTxID: providerTxID,
		Status:       status,
		Amount:       amount,
		CurrencyCode: currencyCode,
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
	ps, err := a.GetPaymentStatus(ctx, tx.ProviderTxID)
	if err != nil {
		return nil, err
	}
	return &adapter.ProviderSyncResult{
		ProviderTxID:     ps.ProviderTxID,
		PaymentStatus:    adapter.SyncPaymentStatusFromPaymentStatus(ps.Status),
		RefundStatus:     adapter.SyncRefundStatusUnsupported,
		RawPaymentStatus: string(ps.Status),
		Message:          "DANA payment status synced; external dashboard refund status is unsupported",
	}, nil
}

// ValidateWebhookSignature verifies the RSA signature on an incoming DANA webhook.
// The SDK uses the hardcoded DANA sandbox public key automatically when no key is provided.
func (a *Adapter) ValidateWebhookSignature(_ context.Context, rawBody []byte, headers map[string]string) error {
	parser, err := a.newWebhookParser()
	if err != nil {
		return fmt.Errorf("dana webhook parser init: %w", err)
	}

	_, err = parser.ParseWebhook("POST", webhookPath, webhook.MapHeaderGetter(headers), string(rawBody))
	if err != nil {
		return fmt.Errorf("dana webhook RSA verification failed: %w", err)
	}
	return nil
}

// HandleWebhook parses a DANA finish-notify webhook into a PaymentResult.
// Must be called after ValidateWebhookSignature succeeds.
func (a *Adapter) HandleWebhook(_ context.Context, rawBody []byte, headers map[string]string) (*adapter.PaymentResult, error) {
	parser, err := a.newWebhookParser()
	if err != nil {
		return nil, fmt.Errorf("dana webhook parser init: %w", err)
	}

	notify, err := parser.ParseWebhook("POST", webhookPath, webhook.MapHeaderGetter(headers), string(rawBody))
	if err != nil {
		return nil, fmt.Errorf("dana HandleWebhook parse: %w", err)
	}

	// OriginalPartnerReferenceNo is the InternalOrderID we set on CreateOrder
	// OriginalReferenceNo is DANA's referenceNo
	internalOrderID := notify.OriginalPartnerReferenceNo
	providerTxID := notify.OriginalReferenceNo
	rawStatus := notify.LatestTransactionStatus

	if internalOrderID == "" {
		return nil, fmt.Errorf("dana webhook: missing originalPartnerReferenceNo")
	}

	status := mapDANAStatus(rawStatus)
	amount := parseDANAAmount(notify.Amount.Value)

	var failureReason string
	if status == adapter.PaymentStatusFailed {
		failureReason = rawStatus
		if notify.TransactionStatusDesc != nil {
			failureReason = *notify.TransactionStatusDesc
		}
	}

	slog.Info("dana webhook received",
		"internalOrderID", internalOrderID,
		"providerTxID", providerTxID,
		"status", rawStatus,
	)

	return &adapter.PaymentResult{
		ProviderTransactionID: providerTxID,
		InternalOrderID:       internalOrderID,
		Status:                status,
		RawProviderStatus:     rawStatus,
		Amount:                amount,
		CurrencyCode:          notify.Amount.Currency,
		FailureReason:         failureReason,
		RawPayload:            rawBody,
	}, nil
}

// RefundPayment initiates a refund at DANA.
func (a *Adapter) RefundPayment(ctx context.Context, internalOrderID string, providerTxID string, amount int64, currencyCode string) error {
	if !strings.EqualFold(currencyCode, "IDR") {
		return fmt.Errorf("dana only supports IDR currency, got %s", currencyCode)
	}
	reason := "Customer refund request"
	refundReq := paymentgw.RefundOrderRequest{
		MerchantId:                 a.cfg.MerchantID,
		OriginalPartnerReferenceNo: internalOrderID,
		PartnerRefundNo:            internalOrderID,
		RefundAmount:               paymentgw.Money{Value: formatDANAAmount(amount), Currency: strings.ToUpper(currencyCode)},
		Reason:                     &reason,
	}

	_, httpResp, err := a.client.PaymentGatewayAPI.RefundOrder(ctx).
		RefundOrderRequest(refundReq).
		Execute()
	if err != nil {
		if httpResp != nil {
			body, _ := io.ReadAll(httpResp.Body)
			httpResp.Body.Close()
			slog.Error("dana RefundOrder failed",
				"status", httpResp.StatusCode,
				"body", string(body),
				"internalOrderID", internalOrderID,
				"providerTxID", providerTxID,
			)
		}
		return fmt.Errorf("dana RefundOrder: %w", err)
	}
	return nil
}

func (a *Adapter) CancelPayment(ctx context.Context, tx *model.Transaction, reason string) (*adapter.CancelResult, error) {
	if tx == nil {
		return &adapter.CancelResult{Status: adapter.CancelStatusFailed, FailureReason: "missing transaction"}, nil
	}
	if !strings.EqualFold(tx.CurrencyCode, "IDR") {
		return &adapter.CancelResult{Status: adapter.CancelStatusFailed, FailureReason: "dana only supports IDR currency"}, nil
	}
	if reason == "" {
		reason = "Customer cancel request"
	}
	cancelReq := paymentgw.CancelOrderRequest{
		OriginalPartnerReferenceNo: tx.ID,
		MerchantId:                 a.cfg.MerchantID,
		Reason:                     &reason,
		Amount: &paymentgw.Money{
			Value:    formatDANAAmount(tx.Amount),
			Currency: strings.ToUpper(tx.CurrencyCode),
		},
	}
	if tx.ProviderTxID != "" {
		cancelReq.OriginalReferenceNo = &tx.ProviderTxID
	}

	resp, httpResp, err := a.client.PaymentGatewayAPI.CancelOrder(ctx).
		CancelOrderRequest(cancelReq).
		Execute()
	if err != nil {
		if httpResp != nil {
			body, _ := io.ReadAll(httpResp.Body)
			httpResp.Body.Close()
			slog.Error("dana CancelOrder failed", "status", httpResp.StatusCode, "body", string(body), "txn_id", tx.ID)
		}
		return nil, fmt.Errorf("dana CancelOrder: %w", err)
	}
	if resp == nil {
		return &adapter.CancelResult{Status: adapter.CancelStatusFailed, FailureReason: "dana CancelOrder: empty response"}, nil
	}
	status := mapDANACancelResponse(resp.ResponseCode)
	return &adapter.CancelResult{
		Status:         status,
		ProviderStatus: resp.ResponseCode,
		ProviderTxID:   tx.ProviderTxID,
		Message:        resp.ResponseMessage,
		Retryable:      status == adapter.CancelStatusPending,
	}, nil
}

// ValidateCredentials performs a lightweight QueryPayment to check that the DANA credentials are valid.
func (a *Adapter) ValidateCredentials(ctx context.Context) error {
	queryReq := paymentgw.QueryPaymentRequest{
		MerchantId:                 a.cfg.MerchantID,
		ServiceCode:                "54",
		OriginalPartnerReferenceNo: ptrStr("credential-check-probe"),
	}

	_, httpResp, err := a.client.PaymentGatewayAPI.QueryPayment(ctx).
		QueryPaymentRequest(queryReq).
		Execute()

	// Network/TLS failure = config is broken
	if err != nil && httpResp == nil {
		return fmt.Errorf("dana ValidateCredentials: %w", err)
	}
	// HTTP 401/403 = bad credentials
	if httpResp != nil && (httpResp.StatusCode == 401 || httpResp.StatusCode == 403) {
		return fmt.Errorf("dana ValidateCredentials: HTTP %d — check DANA_* credentials", httpResp.StatusCode)
	}
	// Any other response (404, DANA business error for unknown tx) = credentials are accepted
	return nil
}

// WebhookAckBody returns the DANA-specific webhook acknowledgement body.
// DANA requires {"responseCode":"2005600","responseMessage":"Successful"} or it will retry.
func (a *Adapter) WebhookAckBody() []byte {
	return []byte(`{"responseCode":"2005600","responseMessage":"Successful"}`)
}

// WebhookErrorAckBody returns the DANA-specific error body for failed webhook processing.
// DANA interprets this as an internal server error and retries the notification.
func (a *Adapter) WebhookErrorAckBody() []byte {
	return []byte(`{"responseCode":"5005601","responseMessage":"Internal Server Error"}`)
}

// newWebhookParser constructs a WebhookParser using the production public key if configured,
// or falls back to the SDK's hardcoded sandbox public key.
func (a *Adapter) newWebhookParser() (*webhook.WebhookParser, error) {
	var pubKeyPEM *string
	var pubKeyPath *string

	if a.cfg.PublicKeyPEM != "" {
		v := normalizePEM(a.cfg.PublicKeyPEM, "PUBLIC KEY")
		pubKeyPEM = &v
	} else if a.cfg.PublicKeyPath != "" {
		pubKeyPath = &a.cfg.PublicKeyPath
	}
	// Both nil → SDK uses the hardcoded DANA sandbox public key

	return webhook.NewWebhookParser(pubKeyPEM, pubKeyPath)
}

// mapDANAStatus maps DANA's latestTransactionStatus codes to adapter.PaymentStatus.
// "00" = paid/success, "05" = cancelled/expired, "01"/"02" = in-progress → PENDING.
func mapDANAStatus(raw string) adapter.PaymentStatus {
	switch raw {
	case "00":
		return adapter.PaymentStatusSuccess
	case "05":
		return adapter.PaymentStatusFailed
	default:
		return adapter.PaymentStatusPending
	}
}

func mapDANACancelResponse(code string) adapter.CancelStatus {
	switch code {
	case "2005700":
		return adapter.CancelStatusCanceled
	case "2025700", "4295700", "5005701":
		return adapter.CancelStatusPending
	case "4045701":
		return adapter.CancelStatusExpired
	default:
		return adapter.CancelStatusFailed
	}
}

// formatDANAAmount converts an integer major-unit amount to DANA's decimal string format.
func formatDANAAmount(amount int64) string {
	return fmt.Sprintf("%d.00", amount)
}

// parseDANAAmount parses DANA's decimal amount string to int64 IDR: "10000.00" → 10000.
func parseDANAAmount(s string) int64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		slog.Warn("dana: failed to parse amount", "value", s)
		return 0
	}
	return int64(f)
}

func ptrStr(s string) *string { return &s }

// normalizePEM accepts a key in any of these formats and always returns a
// properly formatted PEM block with real newlines and the correct header:
//   - Raw base64 string (no headers)
//   - PEM with \n escape sequences (env-file friendly)
//   - Already valid multi-line PEM (possibly with wrong header — auto-corrected)
//
// DANA's dashboard sometimes issues PKCS#8 private keys with a
// "-----BEGIN RSA PRIVATE KEY-----" header (which is PKCS#1). This function
// detects the actual format and corrects the header so Go's crypto library
// can parse it.
func normalizePEM(raw, keyType string) string {
	// Unescape literal \n sequences
	s := strings.ReplaceAll(raw, `\n`, "\n")
	s = strings.TrimSpace(s)

	var bodyB64 string
	if strings.HasPrefix(s, "-----BEGIN") {
		// Strip existing headers to get the raw base64 body
		block, _ := pem.Decode([]byte(s))
		if block != nil {
			bodyB64 = strings.ReplaceAll(string(pem.EncodeToMemory(block)), "-----BEGIN "+block.Type+"-----\n", "")
			bodyB64 = strings.ReplaceAll(bodyB64, "\n-----END "+block.Type+"-----\n", "")
			// Re-derive correct header from the actual DER bytes
			keyType = detectPrivateKeyType(block.Bytes, keyType)
		} else {
			// pem.Decode failed (malformed) — strip headers manually and re-wrap
			bodyB64 = extractBase64Body(s)
		}
	} else {
		bodyB64 = strings.ReplaceAll(strings.ReplaceAll(s, "\n", ""), " ", "")
	}

	return "-----BEGIN " + keyType + "-----\n" + bodyB64 + "\n-----END " + keyType + "-----"
}

// detectPrivateKeyType inspects raw DER bytes and returns the correct PEM block type.
// Fixes DANA's common mistake of labelling PKCS#8 keys as "RSA PRIVATE KEY".
func detectPrivateKeyType(der []byte, fallback string) string {
	if _, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		return "PRIVATE KEY"
	}
	if _, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return "RSA PRIVATE KEY"
	}
	return fallback
}

func extractBase64Body(s string) string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-----") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
