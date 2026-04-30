package xendit

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	xenditsdk "github.com/xendit/xendit-go/v7"
	transaction "github.com/xendit/xendit-go/v7/balance_and_transaction"
	"github.com/xendit/xendit-go/v7/common"
	"github.com/xendit/xendit-go/v7/refund"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
)

const callbackTokenHeader = "x-callback-token"
const defaultXenditAPIBaseURL = "https://api.xendit.co"
const xenditPaymentsAPIVersion = "2024-11-11"

type refundClient interface {
	CreateRefund(ctx context.Context, internalOrderID, paymentRequestID string, amount int64, currencyCode string, idempotencyKey string) error
	ListRefundsByPaymentRequestID(ctx context.Context, paymentRequestID string) ([]xenditRefundRecord, error)
}

type transactionHistoryClient interface {
	ListByReferenceID(ctx context.Context, referenceID string) ([]xenditTransactionRecord, error)
	ListByProductID(ctx context.Context, productID string) ([]xenditTransactionRecord, error)
}

type sdkTransactionHistoryClient struct {
	api transaction.TransactionApi
}

type sdkRefundClient struct {
	api refund.RefundApi
}

// Adapter implements adapter.PaymentProvider using Xendit's Payment Sessions (hosted checkout)
// and the official Go SDK for refunds. All new transactions use Payment Sessions (ps- IDs).
type Adapter struct {
	cfg                *Config
	refunds            refundClient
	transactionHistory transactionHistoryClient
	httpClient         *http.Client
}

func New(cfg *Config) (*Adapter, error) {
	if cfg == nil {
		return nil, fmt.Errorf("xendit config is required")
	}
	client := xenditsdk.NewClient(cfg.SecretAPIKey)
	if sdkCfg, ok := client.GetConfig().(*xenditsdk.Configuration); ok {
		baseURL := strings.TrimRight(cfg.APIBaseURL, "/")
		if baseURL == "" {
			baseURL = defaultXenditAPIBaseURL
		}
		sdkCfg.Servers = xenditsdk.ServerConfigurations{{URL: baseURL}}
	}
	return &Adapter{
		cfg:                cfg,
		refunds:            sdkRefundClient{api: client.RefundApi},
		transactionHistory: sdkTransactionHistoryClient{api: client.TransactionApi},
		httpClient:         http.DefaultClient,
	}, nil
}

func (a *Adapter) Name() string { return "xendit" }

func (c sdkTransactionHistoryClient) ListByReferenceID(ctx context.Context, referenceID string) ([]xenditTransactionRecord, error) {
	if strings.TrimSpace(referenceID) == "" {
		return nil, nil
	}
	return c.list(ctx, func(req transaction.ApiGetAllTransactionsRequest) transaction.ApiGetAllTransactionsRequest {
		return req.ReferenceId(referenceID)
	})
}

func (c sdkTransactionHistoryClient) ListByProductID(ctx context.Context, productID string) ([]xenditTransactionRecord, error) {
	if strings.TrimSpace(productID) == "" {
		return nil, nil
	}
	return c.list(ctx, func(req transaction.ApiGetAllTransactionsRequest) transaction.ApiGetAllTransactionsRequest {
		return req.ProductId(productID)
	})
}

func (c sdkTransactionHistoryClient) list(ctx context.Context, filter func(transaction.ApiGetAllTransactionsRequest) transaction.ApiGetAllTransactionsRequest) ([]xenditTransactionRecord, error) {
	if c.api == nil {
		return nil, nil
	}
	req := c.api.GetAllTransactions(ctx).
		Types([]transaction.TransactionTypes{
			transaction.TRANSACTIONTYPES_PAYMENT,
			transaction.TRANSACTIONTYPES_REFUND,
		}).
		Limit(50)
	req = filter(req)
	resp, httpResp, sdkErr := req.Execute()
	if sdkErr != nil {
		return nil, formatSDKError("xendit GetAllTransactions", sdkErr, httpResp)
	}
	if resp == nil {
		return nil, nil
	}
	records := make([]xenditTransactionRecord, 0, len(resp.Data))
	for _, item := range resp.Data {
		records = append(records, xenditTransactionRecord{
			ID:          item.Id,
			ProductID:   item.ProductId,
			Type:        transactionTypeString(item.Type),
			Status:      string(item.Status),
			ReferenceID: item.ReferenceId,
			Currency:    string(item.Currency),
			Amount:      int64(item.Amount),
		})
	}
	return records, nil
}

func (c sdkRefundClient) CreateRefund(ctx context.Context, internalOrderID, paymentRequestID string, amount int64, currencyCode string, idempotencyKey string) error {
	if c.api == nil {
		return fmt.Errorf("xendit refund API is not configured")
	}
	refundReq := buildRefundRequestByPaymentRequestID(internalOrderID, paymentRequestID, amount, currencyCode)
	_, httpResp, sdkErr := c.api.CreateRefund(ctx).
		IdempotencyKey(idempotencyKey).
		CreateRefund(*refundReq).
		Execute()
	if sdkErr != nil {
		return formatSDKError("xendit CreateRefund", sdkErr, httpResp)
	}
	return nil
}

func (c sdkRefundClient) ListRefundsByPaymentRequestID(ctx context.Context, paymentRequestID string) ([]xenditRefundRecord, error) {
	if strings.TrimSpace(paymentRequestID) == "" || c.api == nil {
		return nil, nil
	}
	resp, httpResp, sdkErr := c.api.GetAllRefunds(ctx).
		PaymentRequestId(paymentRequestID).
		Limit(100).
		Execute()
	if sdkErr != nil {
		return nil, formatSDKError("xendit GetAllRefunds", sdkErr, httpResp)
	}
	if resp == nil {
		return nil, nil
	}
	out := make([]xenditRefundRecord, 0, len(resp.Data))
	for _, item := range resp.Data {
		out = append(out, xenditRefundRecord{
			ID:       item.GetId(),
			Status:   "SUCCEEDED",
			Amount:   item.GetAmount(),
			Currency: item.GetCurrency(),
		})
	}
	return out, nil
}

func (a *Adapter) CreatePaymentIntent(ctx context.Context, req adapter.PaymentInitRequest) (*adapter.PaymentIntent, error) {
	expiresAt := time.Now().Add(req.ExpiryDuration)
	country, createReq, err := a.buildCreatePaymentSessionRequest(req)
	if err != nil {
		return nil, err
	}

	var resp xenditPaymentSessionResponse
	if err := a.doJSON(ctx, http.MethodPost, "/sessions", createReq, &resp); err != nil {
		return nil, fmt.Errorf("xendit CreatePaymentSession: %w", err)
	}
	providerTxID := resp.PaymentSessionID
	if providerTxID == "" {
		providerTxID = resp.ID
	}
	paymentURL := resp.PaymentLinkURL
	if providerTxID == "" {
		return nil, fmt.Errorf("xendit CreatePaymentSession: empty session id")
	}
	if paymentURL == "" {
		return nil, fmt.Errorf("xendit CreatePaymentSession: empty payment_link_url")
	}

	slog.Info("xendit CreatePaymentSession success",
		"internalOrderID", req.InternalOrderID,
		"providerTxID", providerTxID,
		"country", country,
		"currency", createReq.Currency,
	)

	return &adapter.PaymentIntent{
		ProviderTransactionID: providerTxID,
		PaymentURL:            paymentURL,
		ExpiresAt:             expiresAt,
	}, nil
}

func (a *Adapter) GetPaymentStatus(ctx context.Context, providerTxID string) (*adapter.ProviderPaymentStatus, error) {
	var session xenditPaymentSessionResponse
	if err := a.doJSON(ctx, http.MethodGet, "/sessions/"+providerTxID, nil, &session); err != nil {
		return nil, err
	}
	return providerStatusFromPaymentSession(providerTxID, &session), nil
}

func (a *Adapter) SyncTransactionStatus(ctx context.Context, tx *model.Transaction) (*adapter.ProviderSyncResult, error) {
	if tx == nil || strings.TrimSpace(tx.ProviderTxID) == "" {
		return &adapter.ProviderSyncResult{
			PaymentStatus: adapter.SyncPaymentStatusUnsupported,
			RefundStatus:  adapter.SyncRefundStatusUnsupported,
			Message:       "missing provider transaction id",
		}, nil
	}

	providerTxID := strings.TrimSpace(tx.ProviderTxID)
	result := &adapter.ProviderSyncResult{
		ProviderTxID:     providerTxID,
		PaymentStatus:    adapter.SyncPaymentStatusPending,
		RefundStatus:     adapter.SyncRefundStatusNone,
		RawPaymentStatus: "",
		Message:          "xendit transaction history synced",
	}

	session := &xenditPaymentSessionResponse{}
	var paymentRequestID, paymentID string
	if strings.HasPrefix(providerTxID, "ps") {
		if err := a.doJSON(ctx, http.MethodGet, "/sessions/"+providerTxID, nil, session); err != nil {
			return nil, fmt.Errorf("xendit sync fetch session: %w", err)
		}
		if sessionID := paymentSessionID(session); sessionID != "" {
			result.ProviderTxID = sessionID
		}
		result.PaymentStatus = adapter.SyncPaymentStatusFromPaymentStatus(mapPaymentSessionStatus(session.Status))
		result.RawPaymentStatus = session.Status
		paymentRequestID = strings.TrimSpace(session.PaymentRequestID)
		paymentID = strings.TrimSpace(session.PaymentID)
	} else {
		paymentRequestID = providerTxID
	}

	if paymentRequestID != "" {
		pr, err := a.getPaymentRequestStatus(ctx, paymentRequestID)
		if err != nil {
			return nil, err
		}
		mergePaymentRequestStatus(result, pr)
		if paymentID == "" {
			paymentID = strings.TrimSpace(pr.LatestPaymentID)
		}
	}

	if paymentID != "" {
		payment, err := a.getPaymentStatusV3(ctx, paymentID)
		if err != nil {
			return nil, err
		}
		mergePaymentStatus(result, payment)
	}

	history, err := a.collectTransactionHistory(ctx, tx.ID, paymentID, paymentRequestID)
	if err != nil {
		return nil, err
	}
	mergeTransactionHistory(result, history, tx.Amount, tx.CurrencyCode)

	if paymentRequestID != "" {
		refunds, err := a.listRefundsByPaymentRequestID(ctx, paymentRequestID)
		if err != nil {
			return nil, err
		}
		mergeRefundSummary(result, refunds, tx.Amount, tx.CurrencyCode)
	}

	if result.RefundStatus == adapter.SyncRefundStatusRefunded || result.RefundStatus == adapter.SyncRefundStatusPartialRefunded {
		result.PaymentStatus = adapter.SyncPaymentStatusPaid
	}
	if result.Message == "" {
		result.Message = "xendit transaction history synced"
	}
	slog.Debug("xendit SyncTransactionStatus",
		"provider_tx_id", tx.ProviderTxID,
		"payment_request_id", paymentRequestID,
		"payment_id", paymentID,
		"payment_status", result.PaymentStatus,
		"refund_status", result.RefundStatus,
	)
	return result, nil
}

func (a *Adapter) ValidateWebhookSignature(_ context.Context, _ []byte, headers map[string]string) error {
	provided := strings.TrimSpace(headers[callbackTokenHeader])
	if provided == "" {
		return fmt.Errorf("missing %s header", callbackTokenHeader)
	}
	if subtle.ConstantTimeCompare([]byte(provided), []byte(a.cfg.CallbackToken)) != 1 {
		return fmt.Errorf("xendit callback token mismatch")
	}
	return nil
}

func (a *Adapter) HandleWebhook(_ context.Context, rawBody []byte, _ map[string]string) (*adapter.PaymentResult, error) {
	if result, ok := parsePaymentSessionWebhook(rawBody); ok {
		return result, nil
	}
	if result, ok := parseRefundWebhook(rawBody); ok {
		return result, nil
	}
	return nil, fmt.Errorf("xendit webhook: unrecognized payload format (expected payment session or refund event)")
}

func (a *Adapter) RefundPayment(ctx context.Context, internalOrderID string, providerTxID string, amount int64, currencyCode string) error {
	paymentRequestID, err := a.resolvePaymentRequestID(ctx, providerTxID)
	if err != nil {
		return err
	}

	// Append provider tx id suffix so retries get a fresh key rather than a 409 from a prior attempt.
	idempotencyKey := internalOrderID + "-" + providerTxID
	if a.refunds == nil {
		return fmt.Errorf("xendit refund API is not configured")
	}
	return a.refunds.CreateRefund(ctx, internalOrderID, paymentRequestID, amount, currencyCode, idempotencyKey)
}

func (a *Adapter) CancelPayment(ctx context.Context, tx *model.Transaction, reason string) (*adapter.CancelResult, error) {
	if tx == nil || strings.TrimSpace(tx.ProviderTxID) == "" {
		return &adapter.CancelResult{Status: adapter.CancelStatusUnsupported, Message: "missing xendit payment session id"}, nil
	}
	if !strings.HasPrefix(tx.ProviderTxID, "ps") {
		return &adapter.CancelResult{Status: adapter.CancelStatusUnsupported, Message: "xendit cancellation is only supported for payment sessions (ps- prefix)"}, nil
	}
	var session xenditPaymentSessionResponse
	if err := a.doJSON(ctx, http.MethodPost, "/sessions/"+tx.ProviderTxID+"/cancel", map[string]string{"reason": reason}, &session); err != nil {
		return nil, err
	}
	result := cancelResultFromPaymentSession(tx.ProviderTxID, &session)
	if result.Status == adapter.CancelStatusFailed {
		ps := providerStatusFromPaymentSession(tx.ProviderTxID, &session)
		switch ps.Status {
		case adapter.PaymentStatusSuccess, adapter.PaymentStatusRefunded:
			result.Status = adapter.CancelStatusAlreadyPaid
		case adapter.PaymentStatusExpired:
			result.Status = adapter.CancelStatusExpired
		case adapter.PaymentStatusCanceled:
			result.Status = adapter.CancelStatusCanceled
		}
	}
	return result, nil
}

// resolvePaymentRequestID returns the payment_request_id (pr-) suitable for the Refund API.
// For ps- (Payment Session) IDs, it fetches the session to get the payment_request_id.
// For py-/pr- IDs stored from a prior sync, it uses them directly.
func (a *Adapter) resolvePaymentRequestID(ctx context.Context, providerTxID string) (string, error) {
	providerTxID = strings.TrimSpace(providerTxID)
	if strings.HasPrefix(providerTxID, "pr") {
		return providerTxID, nil
	}
	if strings.HasPrefix(providerTxID, "py") {
		payment, err := a.getPaymentStatusV3(ctx, providerTxID)
		if err != nil {
			return "", fmt.Errorf("xendit resolvePaymentRequestID: fetch payment: %w", err)
		}
		if payment.PaymentRequestID == "" {
			return "", fmt.Errorf("xendit resolvePaymentRequestID: payment %s has no payment_request_id", providerTxID)
		}
		return payment.PaymentRequestID, nil
	}
	if !strings.HasPrefix(providerTxID, "ps") {
		return "", fmt.Errorf("xendit resolvePaymentRequestID: provider transaction id %s is not a payment_request_id or payment session id", providerTxID)
	}
	var session xenditPaymentSessionResponse
	if err := a.doJSON(ctx, http.MethodGet, "/sessions/"+providerTxID, nil, &session); err != nil {
		return "", fmt.Errorf("xendit resolvePaymentRequestID: fetch session: %w", err)
	}
	if session.PaymentRequestID == "" {
		return "", fmt.Errorf("xendit resolvePaymentRequestID: session %s has no payment_request_id (payment may not be completed yet)", providerTxID)
	}
	return session.PaymentRequestID, nil
}

type xenditPaymentSessionRequest struct {
	ReferenceID      string            `json:"reference_id"`
	SessionType      string            `json:"session_type"`
	Mode             string            `json:"mode"`
	Amount           int64             `json:"amount"`
	Currency         string            `json:"currency"`
	Country          string            `json:"country,omitempty"`
	Description      string            `json:"description,omitempty"`
	SuccessReturnURL string            `json:"success_return_url,omitempty"`
	FailureReturnURL string            `json:"failure_return_url,omitempty"`
	CancelReturnURL  string            `json:"cancel_return_url,omitempty"`
	ExpiresAt        string            `json:"expires_at,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

type xenditPaymentSessionResponse struct {
	ID               string            `json:"id"`
	PaymentSessionID string            `json:"payment_session_id"`
	Status           string            `json:"status"`
	PaymentLinkURL   string            `json:"payment_link_url"`
	ReferenceID      string            `json:"reference_id"`
	Amount           int64             `json:"amount"`
	Currency         string            `json:"currency"`
	PaymentRequestID string            `json:"payment_request_id"`
	PaymentID        string            `json:"payment_id"`
	Metadata         map[string]string `json:"metadata"`
}

type xenditPaymentRequestResponse struct {
	ID               string         `json:"id"`
	PaymentRequestID string         `json:"payment_request_id"`
	ReferenceID      string         `json:"reference_id"`
	Status           string         `json:"status"`
	RequestAmount    float64        `json:"request_amount"`
	Amount           float64        `json:"amount"`
	Currency         string         `json:"currency"`
	LatestPaymentID  string         `json:"latest_payment_id"`
	FailureCode      string         `json:"failure_code"`
	Metadata         map[string]any `json:"metadata"`
}

type xenditPaymentResponse struct {
	PaymentID        string         `json:"payment_id"`
	ReferenceID      string         `json:"reference_id"`
	PaymentRequestID string         `json:"payment_request_id"`
	Status           string         `json:"status"`
	RequestAmount    float64        `json:"request_amount"`
	Amount           float64        `json:"amount"`
	Currency         string         `json:"currency"`
	FailureCode      string         `json:"failure_code"`
	Metadata         map[string]any `json:"metadata"`
}

type xenditTransactionRecord struct {
	ID          string
	ProductID   string
	Type        string
	Status      string
	ReferenceID string
	Currency    string
	Amount      int64
}

func (a *Adapter) buildCreatePaymentSessionRequest(req adapter.PaymentInitRequest) (string, *xenditPaymentSessionRequest, error) {
	country, err := a.resolveCountry(req.RegionCode)
	if err != nil {
		return "", nil, err
	}
	currency := strings.ToUpper(strings.TrimSpace(req.CurrencyCode))
	if _, ok := a.cfg.AllowedCurrencies[currency]; !ok {
		return "", nil, fmt.Errorf("xendit currency %q is not allowed; allowed=%v", currency, sortedCodes(a.cfg.AllowedCurrencies))
	}
	description := strings.TrimSpace(req.Description)
	if description == "" {
		description = "Payment"
	}
	return country, &xenditPaymentSessionRequest{
		ReferenceID:      req.InternalOrderID,
		SessionType:      "PAY",
		Mode:             "PAYMENT_LINK",
		Amount:           req.Amount,
		Currency:         currency,
		Country:          country,
		Description:      description,
		SuccessReturnURL: req.ReturnURL,
		FailureReturnURL: req.ReturnURL,
		CancelReturnURL:  req.ReturnURL,
		ExpiresAt:        time.Now().Add(req.ExpiryDuration).UTC().Format(time.RFC3339),
		Metadata: map[string]string{
			"internalOrderId": req.InternalOrderID,
			"userId":          req.UserID,
			"country":         country,
			"regionCode":      normalizeRegionCode(req.RegionCode),
		},
	}, nil
}

func providerStatusFromPaymentSession(fallbackTxID string, session *xenditPaymentSessionResponse) *adapter.ProviderPaymentStatus {
	providerTxID := paymentSessionID(session)
	if providerTxID == "" {
		providerTxID = fallbackTxID
	}
	return &adapter.ProviderPaymentStatus{
		ProviderTxID:  providerTxID,
		Status:        mapPaymentSessionStatus(session.Status),
		Amount:        session.Amount,
		CurrencyCode:  session.Currency,
		FailureReason: session.Status,
	}
}

func cancelResultFromPaymentSession(fallbackTxID string, session *xenditPaymentSessionResponse) *adapter.CancelResult {
	providerTxID := paymentSessionID(session)
	if providerTxID == "" {
		providerTxID = fallbackTxID
	}
	return &adapter.CancelResult{
		Status:         mapPaymentSessionCancelStatus(session.Status),
		ProviderStatus: session.Status,
		ProviderTxID:   providerTxID,
	}
}

func parsePaymentSessionWebhook(rawBody []byte) (*adapter.PaymentResult, bool) {
	var evt struct {
		Type string                       `json:"type"`
		Data xenditPaymentSessionResponse `json:"data"`
	}
	if err := json.Unmarshal(rawBody, &evt); err != nil {
		return nil, false
	}
	sid := paymentSessionID(&evt.Data)
	if sid == "" || !strings.HasPrefix(sid, "ps") {
		return nil, false
	}
	internalOrderID := strings.TrimSpace(evt.Data.ReferenceID)
	if internalOrderID == "" && evt.Data.Metadata != nil {
		internalOrderID = strings.TrimSpace(evt.Data.Metadata["internalOrderId"])
	}
	if internalOrderID == "" {
		return nil, false
	}
	return &adapter.PaymentResult{
		ProviderTransactionID: paymentSessionID(&evt.Data),
		InternalOrderID:       internalOrderID,
		Status:                mapPaymentSessionStatus(evt.Data.Status),
		RawProviderStatus:     evt.Data.Status,
		Amount:                evt.Data.Amount,
		CurrencyCode:          evt.Data.Currency,
		FailureReason:         evt.Data.Status,
		RawPayload:            rawBody,
	}, true
}

// parseRefundWebhook parses Xendit's refund.succeeded / refund.failed webhook events.
// The reference_id in the refund data is the internalOrderID we set when creating the refund.
func parseRefundWebhook(rawBody []byte) (*adapter.PaymentResult, bool) {
	var evt struct {
		Event string `json:"event"`
		Data  struct {
			ID          string  `json:"id"`
			Status      string  `json:"status"`
			Amount      float64 `json:"amount"`
			Currency    string  `json:"currency"`
			ReferenceID string  `json:"reference_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rawBody, &evt); err != nil || !strings.HasPrefix(evt.Event, "refund.") {
		return nil, false
	}
	internalOrderID := strings.TrimSpace(evt.Data.ReferenceID)
	if internalOrderID == "" {
		return nil, false
	}

	var ps adapter.PaymentStatus
	switch strings.ToUpper(strings.TrimSpace(evt.Data.Status)) {
	case "SUCCEEDED":
		ps = adapter.PaymentStatusRefunded
	case "FAILED", "CANCELLED", "CANCELED":
		ps = adapter.PaymentStatusFailed
	default:
		ps = adapter.PaymentStatusPending
	}

	return &adapter.PaymentResult{
		ProviderTransactionID: evt.Data.ID,
		InternalOrderID:       internalOrderID,
		Status:                ps,
		RawProviderStatus:     evt.Data.Status,
		Amount:                int64(evt.Data.Amount),
		CurrencyCode:          evt.Data.Currency,
		RawPayload:            rawBody,
	}, true
}

func paymentSessionID(session *xenditPaymentSessionResponse) string {
	if session == nil {
		return ""
	}
	if session.PaymentSessionID != "" {
		return session.PaymentSessionID
	}
	return session.ID
}

func (a *Adapter) getPaymentRequestStatus(ctx context.Context, paymentRequestID string) (*xenditPaymentRequestResponse, error) {
	var pr xenditPaymentRequestResponse
	if err := a.doJSON(ctx, http.MethodGet, "/v3/payment_requests/"+paymentRequestID, nil, &pr); err != nil {
		return nil, fmt.Errorf("xendit GetPaymentRequest %s: %w", paymentRequestID, err)
	}
	if pr.PaymentRequestID == "" {
		pr.PaymentRequestID = pr.ID
	}
	return &pr, nil
}

func (a *Adapter) getPaymentStatusV3(ctx context.Context, paymentID string) (*xenditPaymentResponse, error) {
	var payment xenditPaymentResponse
	if err := a.doJSON(ctx, http.MethodGet, "/v3/payments/"+paymentID, nil, &payment); err != nil {
		return nil, fmt.Errorf("xendit GetPayment %s: %w", paymentID, err)
	}
	if payment.PaymentID == "" {
		payment.PaymentID = paymentID
	}
	return &payment, nil
}

func (a *Adapter) collectTransactionHistory(ctx context.Context, referenceID, paymentID, paymentRequestID string) ([]xenditTransactionRecord, error) {
	if a.transactionHistory == nil {
		return nil, nil
	}
	var records []xenditTransactionRecord
	seen := map[string]struct{}{}
	appendUnique := func(items []xenditTransactionRecord) {
		for _, item := range items {
			key := item.ID
			if key == "" {
				key = item.Type + "|" + item.Status + "|" + item.ProductID + "|" + item.ReferenceID
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			records = append(records, item)
		}
	}
	if referenceID != "" {
		items, err := a.transactionHistory.ListByReferenceID(ctx, referenceID)
		if err != nil {
			return nil, err
		}
		appendUnique(items)
	}
	for _, productID := range []string{paymentID, paymentRequestID} {
		if productID == "" {
			continue
		}
		items, err := a.transactionHistory.ListByProductID(ctx, productID)
		if err != nil {
			return nil, err
		}
		appendUnique(items)
	}
	return records, nil
}

func (a *Adapter) doJSON(ctx context.Context, method string, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	baseURL := strings.TrimRight(a.cfg.APIBaseURL, "/")
	if baseURL == "" {
		baseURL = defaultXenditAPIBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reader)
	if err != nil {
		return err
	}
	req.SetBasicAuth(a.cfg.SecretAPIKey, "")
	req.Header.Set("Accept", "application/json")
	if strings.HasPrefix(path, "/v3/") {
		req.Header.Set("api-version", xenditPaymentsAPIVersion)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := a.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("xendit API HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

func buildRefundRequestByPaymentRequestID(internalOrderID string, paymentRequestID string, amount int64, currencyCode string) *refund.CreateRefund {
	refundReq := refund.NewCreateRefund()
	refundReq.SetPaymentRequestId(paymentRequestID)
	refundReq.SetReferenceId(internalOrderID)
	refundReq.SetAmount(float64(amount))
	refundReq.SetCurrency(strings.ToUpper(strings.TrimSpace(currencyCode)))
	refundReq.SetReason("REQUESTED_BY_CUSTOMER")
	refundReq.SetMetadata(map[string]any{
		"internalOrderId": internalOrderID,
	})
	return refundReq
}

type xenditRefundRecord struct {
	ID       string  `json:"id"`
	Status   string  `json:"status"`
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

func (a *Adapter) listRefundsByPaymentRequestID(ctx context.Context, paymentRequestID string) ([]xenditRefundRecord, error) {
	if strings.TrimSpace(paymentRequestID) == "" || a.refunds == nil {
		return nil, nil
	}
	return a.refunds.ListRefundsByPaymentRequestID(ctx, paymentRequestID)
}

func summarizeXenditRefunds(refunds []xenditRefundRecord, txAmount int64, txCurrency string) (adapter.SyncRefundStatus, string, int64, string) {
	if len(refunds) == 0 {
		return adapter.SyncRefundStatusNone, "", 0, ""
	}
	var succeededAmount int64
	var pending bool
	var failedOrCancelled bool
	var rawStatuses []string
	refundCurrency := ""
	expectedCurrency := strings.ToUpper(strings.TrimSpace(txCurrency))
	for _, r := range refunds {
		status := strings.ToUpper(strings.TrimSpace(r.Status))
		rawStatuses = append(rawStatuses, status)
		currency := strings.ToUpper(strings.TrimSpace(r.Currency))
		if refundCurrency == "" {
			refundCurrency = currency
		}
		switch status {
		case "SUCCEEDED":
			if expectedCurrency == "" || currency == expectedCurrency {
				succeededAmount += int64(r.Amount)
			}
		case "PENDING":
			pending = true
		case "FAILED", "CANCELLED":
			failedOrCancelled = true
		}
	}
	rawStatus := strings.Join(rawStatuses, ",")
	if succeededAmount >= txAmount && txAmount > 0 {
		return adapter.SyncRefundStatusRefunded, rawStatus, succeededAmount, refundCurrency
	}
	if succeededAmount > 0 {
		return adapter.SyncRefundStatusPartialRefunded, rawStatus, succeededAmount, refundCurrency
	}
	if pending {
		return adapter.SyncRefundStatusPending, rawStatus, 0, refundCurrency
	}
	if failedOrCancelled {
		return adapter.SyncRefundStatusFailed, rawStatus, 0, refundCurrency
	}
	return adapter.SyncRefundStatusUnknown, rawStatus, 0, refundCurrency
}

func mergePaymentRequestStatus(result *adapter.ProviderSyncResult, pr *xenditPaymentRequestResponse) {
	if result == nil || pr == nil {
		return
	}
	status := mapProviderPaymentStatus(pr.Status)
	if status != adapter.SyncPaymentStatusPending || result.PaymentStatus == "" {
		result.PaymentStatus = status
	}
	if pr.Status != "" {
		result.RawPaymentStatus = appendRawStatus(result.RawPaymentStatus, "payment_request:"+pr.Status)
	}
	if pr.FailureCode != "" {
		result.Message = "xendit payment request failure: " + pr.FailureCode
	}
}

func mergePaymentStatus(result *adapter.ProviderSyncResult, payment *xenditPaymentResponse) {
	if result == nil || payment == nil {
		return
	}
	status := mapProviderPaymentStatus(payment.Status)
	if status != adapter.SyncPaymentStatusPending || result.PaymentStatus == "" {
		result.PaymentStatus = status
	}
	if payment.Status != "" {
		result.RawPaymentStatus = appendRawStatus(result.RawPaymentStatus, "payment:"+payment.Status)
	}
	if payment.FailureCode != "" {
		result.Message = "xendit payment failure: " + payment.FailureCode
	}
}

func mergeTransactionHistory(result *adapter.ProviderSyncResult, records []xenditTransactionRecord, txAmount int64, txCurrency string) {
	if result == nil || len(records) == 0 {
		return
	}
	var refundRecords []xenditRefundRecord
	for _, record := range records {
		raw := strings.ToUpper(strings.TrimSpace(record.Type)) + ":" + strings.ToUpper(strings.TrimSpace(record.Status))
		switch strings.ToUpper(strings.TrimSpace(record.Type)) {
		case "PAYMENT":
			switch strings.ToUpper(strings.TrimSpace(record.Status)) {
			case "SUCCESS":
				result.PaymentStatus = adapter.SyncPaymentStatusPaid
				result.RawPaymentStatus = appendRawStatus(result.RawPaymentStatus, raw)
			case "FAILED", "VOIDED", "REVERSED":
				if result.PaymentStatus != adapter.SyncPaymentStatusPaid {
					result.PaymentStatus = adapter.SyncPaymentStatusFailed
				}
				result.RawPaymentStatus = appendRawStatus(result.RawPaymentStatus, raw)
			}
		case "REFUND":
			refundRecords = append(refundRecords, xenditRefundRecord{
				ID:       record.ID,
				Status:   mapTransactionRefundStatus(record.Status),
				Amount:   float64(record.Amount),
				Currency: record.Currency,
			})
		}
	}
	mergeRefundSummary(result, refundRecords, txAmount, txCurrency)
}

func mergeRefundSummary(result *adapter.ProviderSyncResult, refunds []xenditRefundRecord, txAmount int64, txCurrency string) {
	if result == nil || len(refunds) == 0 {
		return
	}
	refundStatus, rawStatus, refundAmount, refundCurrency := summarizeXenditRefunds(refunds, txAmount, txCurrency)
	if strongerRefundStatus(refundStatus, result.RefundStatus) {
		result.RefundStatus = refundStatus
		result.RawRefundStatus = appendRawStatus(result.RawRefundStatus, rawStatus)
		result.RefundAmount = refundAmount
		result.RefundCurrencyCode = refundCurrency
	}
}

func strongerRefundStatus(next, current adapter.SyncRefundStatus) bool {
	rank := func(status adapter.SyncRefundStatus) int {
		switch status {
		case adapter.SyncRefundStatusRefunded:
			return 5
		case adapter.SyncRefundStatusPartialRefunded:
			return 4
		case adapter.SyncRefundStatusPending:
			return 3
		case adapter.SyncRefundStatusFailed:
			return 2
		case adapter.SyncRefundStatusUnknown:
			return 1
		default:
			return 0
		}
	}
	return rank(next) > rank(current)
}

func appendRawStatus(existing, next string) string {
	next = strings.TrimSpace(next)
	if next == "" {
		return existing
	}
	if existing == "" {
		return next
	}
	if strings.Contains(existing, next) {
		return existing
	}
	return existing + "," + next
}

func mapProviderPaymentStatus(raw string) adapter.SyncPaymentStatus {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "SUCCEEDED", "SUCCESS", "COMPLETED", "PAID":
		return adapter.SyncPaymentStatusPaid
	case "FAILED", "CANCELED", "CANCELLED", "EXPIRED", "VOIDED", "REVERSED":
		return adapter.SyncPaymentStatusFailed
	case "":
		return adapter.SyncPaymentStatusUnknown
	default:
		return adapter.SyncPaymentStatusPending
	}
}

func mapTransactionRefundStatus(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "SUCCESS", "REVERSED", "VOIDED":
		return "SUCCEEDED"
	case "PENDING":
		return "PENDING"
	case "FAILED":
		return "FAILED"
	default:
		return raw
	}
}

func transactionTypeString(raw transaction.TransactionResponseType) string {
	if raw.TransactionTypes != nil {
		return string(*raw.TransactionTypes)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return ""
	}
	return strings.Trim(string(encoded), `"`)
}

func (a *Adapter) resolveCountry(regionCode string) (string, error) {
	country := normalizeRegionCode(regionCode)
	if country == "" {
		country = a.cfg.DefaultCountry
	}
	if _, ok := a.cfg.AllowedCountries[country]; !ok {
		return "", fmt.Errorf("xendit country %q is not allowed; allowed=%v", country, sortedCodes(a.cfg.AllowedCountries))
	}
	return country, nil
}

func normalizeRegionCode(regionCode string) string {
	regionCode = strings.ToUpper(strings.TrimSpace(regionCode))
	if regionCode == "" {
		return ""
	}
	if idx := strings.IndexAny(regionCode, "-_"); idx > 0 {
		regionCode = regionCode[:idx]
	}
	return regionCode
}

func mapPaymentSessionStatus(raw string) adapter.PaymentStatus {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "COMPLETED", "PAID", "SUCCEEDED", "SUCCESS":
		return adapter.PaymentStatusSuccess
	case "CANCELED", "CANCELLED":
		return adapter.PaymentStatusCanceled
	case "EXPIRED":
		return adapter.PaymentStatusExpired
	case "FAILED":
		return adapter.PaymentStatusFailed
	default:
		return adapter.PaymentStatusPending
	}
}

func mapPaymentSessionCancelStatus(raw string) adapter.CancelStatus {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "CANCELED", "CANCELLED":
		return adapter.CancelStatusCanceled
	case "EXPIRED":
		return adapter.CancelStatusExpired
	case "COMPLETED", "PAID", "SUCCEEDED", "SUCCESS":
		return adapter.CancelStatusAlreadyPaid
	case "ACTIVE", "PENDING":
		return adapter.CancelStatusPending
	default:
		return adapter.CancelStatusFailed
	}
}

// ValidateCredentials performs a lightweight call to verify the API key is valid.
func (a *Adapter) ValidateCredentials(ctx context.Context) error {
	baseURL := strings.TrimRight(a.cfg.APIBaseURL, "/")
	if baseURL == "" {
		baseURL = defaultXenditAPIBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/refunds?limit=1", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(a.cfg.SecretAPIKey, "")
	req.Header.Set("Accept", "application/json")
	client := a.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("xendit ValidateCredentials: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("xendit ValidateCredentials: HTTP %d - check XENDIT_SECRET_API_KEY", resp.StatusCode)
	}
	return nil
}

func formatSDKError(prefix string, sdkErr *common.XenditSdkError, httpResp *http.Response) error {
	if sdkErr == nil {
		return nil
	}
	if httpResp != nil {
		return fmt.Errorf("%s: HTTP %d: %s", prefix, httpResp.StatusCode, sdkErr.Error())
	}
	return fmt.Errorf("%s: %s", prefix, sdkErr.Error())
}
