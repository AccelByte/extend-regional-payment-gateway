package adapter

import (
	"context"
	"errors"
	"time"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
)

// ErrNotSupported is returned by GetPaymentStatus when the provider has no status query API.
var ErrNotSupported = errors.New("operation not supported by this provider")

type PaymentStatus string

const (
	PaymentStatusSuccess  PaymentStatus = "SUCCESS"
	PaymentStatusFailed   PaymentStatus = "FAILED"
	PaymentStatusPending  PaymentStatus = "PENDING"
	PaymentStatusRefunded PaymentStatus = "REFUNDED"
	PaymentStatusCanceled PaymentStatus = "CANCELED"
	PaymentStatusExpired  PaymentStatus = "EXPIRED"
)

type CancelStatus string

const (
	CancelStatusCanceled    CancelStatus = "CANCELED"
	CancelStatusExpired     CancelStatus = "EXPIRED"
	CancelStatusAlreadyPaid CancelStatus = "ALREADY_PAID"
	CancelStatusPending     CancelStatus = "PENDING"
	CancelStatusUnsupported CancelStatus = "UNSUPPORTED"
	CancelStatusFailed      CancelStatus = "FAILED"
)

type PaymentInitRequest struct {
	InternalOrderID string
	UserID          string
	RegionCode      string
	Amount          int64
	CurrencyCode    string
	Description     string
	CallbackURL     string // server-to-server webhook (POST)
	ReturnURL       string // browser redirect after payment (GET); may be empty
	ExpiryDuration  time.Duration
}

type PaymentIntent struct {
	ProviderTransactionID string
	PaymentURL            string
	QRCodeData            string
	ExpiresAt             time.Time
}

type PaymentResult struct {
	ProviderTransactionID string
	InternalOrderID       string
	Status                PaymentStatus
	RawProviderStatus     string // raw status string from provider payload
	Amount                int64
	CurrencyCode          string
	FailureReason         string
	RawPayload            []byte
}

type ProviderPaymentStatus struct {
	ProviderTxID  string
	Status        PaymentStatus
	Amount        int64
	CurrencyCode  string
	FailureReason string
}

type CancelResult struct {
	Status            CancelStatus
	ProviderStatus    string
	ProviderTxID      string
	Message           string
	FailureReason     string
	Retryable         bool
	RawPaymentStatus  string
	RawProviderStatus string
}

type PaymentCanceler interface {
	CancelPayment(ctx context.Context, tx *model.Transaction, reason string) (*CancelResult, error)
}

type SyncPaymentStatus string

const (
	SyncPaymentStatusPending     SyncPaymentStatus = "PENDING"
	SyncPaymentStatusPaid        SyncPaymentStatus = "PAID"
	SyncPaymentStatusFailed      SyncPaymentStatus = "FAILED"
	SyncPaymentStatusUnknown     SyncPaymentStatus = "UNKNOWN"
	SyncPaymentStatusUnsupported SyncPaymentStatus = "UNSUPPORTED"
)

type SyncRefundStatus string

const (
	SyncRefundStatusNone            SyncRefundStatus = "NONE"
	SyncRefundStatusPending         SyncRefundStatus = "PENDING"
	SyncRefundStatusPartialRefunded SyncRefundStatus = "PARTIAL_REFUNDED"
	SyncRefundStatusRefunded        SyncRefundStatus = "REFUNDED"
	SyncRefundStatusFailed          SyncRefundStatus = "FAILED"
	SyncRefundStatusUnknown         SyncRefundStatus = "UNKNOWN"
	SyncRefundStatusUnsupported     SyncRefundStatus = "UNSUPPORTED"
)

type ProviderSyncResult struct {
	ProviderTxID       string
	PaymentStatus      SyncPaymentStatus
	RefundStatus       SyncRefundStatus
	RawPaymentStatus   string
	RawRefundStatus    string
	RefundAmount       int64
	RefundCurrencyCode string
	Message            string
}

type TransactionSyncer interface {
	SyncTransactionStatus(ctx context.Context, tx *model.Transaction) (*ProviderSyncResult, error)
}

// WebhookAcknowledger is an optional interface adapters implement when the provider
// requires a specific JSON acknowledgement body in the webhook response.
// The HTTP webhook handler checks for this interface after successful processing.
type WebhookAcknowledger interface {
	WebhookAckBody() []byte
}

// WebhookErrorAcknowledger is an optional interface adapters implement when the provider
// expects a specific JSON body even on error responses (e.g. DANA's 5005601 format).
// The HTTP webhook handler checks for this interface before writing a 500 error body.
type WebhookErrorAcknowledger interface {
	WebhookErrorAckBody() []byte
}

// PaymentProvider is the extensibility contract for all payment providers.
// The Generic HTTP Adapter implements this interface entirely via env vars.
type PaymentProvider interface {
	// Name returns the canonical provider slug (e.g. "generic_dana", "generic_shopeepay").
	Name() string

	// CreatePaymentIntent creates a charge at the provider.
	CreatePaymentIntent(ctx context.Context, req PaymentInitRequest) (*PaymentIntent, error)

	// GetPaymentStatus queries the provider directly for the current payment status.
	// Returns ErrNotSupported if the provider has no status query API.
	GetPaymentStatus(ctx context.Context, providerTxID string) (*ProviderPaymentStatus, error)

	// ValidateWebhookSignature verifies the raw body + headers came from the legitimate provider.
	// Must be called BEFORE any state mutation.
	ValidateWebhookSignature(ctx context.Context, rawBody []byte, headers map[string]string) error

	// HandleWebhook parses a validated webhook into a PaymentResult.
	HandleWebhook(ctx context.Context, rawBody []byte, headers map[string]string) (*PaymentResult, error)

	// RefundPayment initiates a refund at the provider.
	RefundPayment(ctx context.Context, internalOrderID string, providerTxID string, amount int64, currencyCode string) error

	// ValidateCredentials performs a lightweight API call to verify credentials.
	ValidateCredentials(ctx context.Context) error
}
