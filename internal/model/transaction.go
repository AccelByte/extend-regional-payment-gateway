package model

import "time"

const (
	StatusPending    = "PENDING"
	StatusFulfilling = "FULFILLING"
	StatusFulfilled  = "FULFILLED"
	StatusFailed     = "FAILED"
	StatusCanceled   = "CANCELED"
	StatusExpired    = "EXPIRED"

	RefundStatusRefunding    = "REFUNDING"
	RefundStatusRefunded     = "REFUNDED"
	RefundStatusRefundFailed = "REFUND_FAILED"

	ProviderXendit = "provider_xendit"
	ProviderKomoju = "provider_komoju"
)

type RefundSubDoc struct {
	Status           string    `bson:"status"`
	Reason           string    `bson:"reason"`
	FailureReason    string    `bson:"failure_reason,omitempty"`
	ProviderRefunded bool      `bson:"provider_refunded,omitempty"`
	CreatedAt        time.Time `bson:"created_at"`
	UpdatedAt        time.Time `bson:"updated_at"`
}

type Transaction struct {
	ID                  string        `bson:"_id"`
	ClientOrderID       string        `bson:"client_order_id"`
	UserID              string        `bson:"user_id"`
	Namespace           string        `bson:"namespace"`
	ProviderID          string        `bson:"provider_id"`
	ProviderDisplayName string        `bson:"provider_display_name,omitempty"`
	PaymentURL          string        `bson:"payment_url,omitempty"`
	ItemName            string        `bson:"item_name,omitempty"`
	ItemID              string        `bson:"item_id"`
	Quantity            int32         `bson:"quantity"`
	RegionCode          string        `bson:"region_code,omitempty"`
	Amount              int64         `bson:"amount"`
	CurrencyCode        string        `bson:"currency_code"`
	ProviderTxID        string        `bson:"provider_tx_id,omitempty"`
	ProviderStatus      string        `bson:"provider_status,omitempty"`
	Status              string        `bson:"status"`
	FailureReason       string        `bson:"failure_reason,omitempty"`
	Refund              *RefundSubDoc `bson:"refund,omitempty"`
	Retries             int32         `bson:"retries,omitempty"`
	CreatedAt           time.Time     `bson:"created_at"`
	ExpiresAt           time.Time     `bson:"expires_at"`
	UpdatedAt           time.Time     `bson:"updated_at"`
	DeleteAt            time.Time     `bson:"delete_at,omitempty"`
}
