# Generic HTTP Adapter - Developer Guide

## Overview

The Generic HTTP adapter lets you integrate any payment provider without writing Go code. It registers as `provider_{name}` where `{name}` comes from the env var prefix, and clients use:

```json
{
  "providerId": "provider_{name}"
}
```

The adapter is driven entirely by `GENERIC_{NAME}_*` environment variables. It supports payment intent creation, webhook signature validation (HMAC-SHA256, HMAC-SHA512, or none), status polling, cancellation, and refunds — all configured via env vars and Go `text/template` bodies and gjson paths.

The server discovers Generic providers automatically at startup by scanning for `GENERIC_{NAME}_AUTH_HEADER` env vars. No code changes are required.

## Configuration

All variables use the prefix `GENERIC_{NAME}_` where `{NAME}` is the uppercase provider name (e.g. `GENERIC_MYPROVIDER_`). The resulting provider ID is `provider_{name}` (lowercase).

### Required Variables

| Variable | Description | Example |
|---|---|---|
| `GENERIC_{NAME}_AUTH_HEADER` | HTTP header name for authentication. Its presence enables the provider. | `Authorization` |
| `GENERIC_{NAME}_AUTH_VALUE` | Value of the auth header. | `Bearer sk_test_abc123` |
| `GENERIC_{NAME}_CREATE_INTENT_URL` | URL to POST for creating a payment intent. | `https://api.myprovider.com/v1/payments` |
| `GENERIC_{NAME}_CREATE_INTENT_BODY_TEMPLATE` | Go `text/template` for the request body. | See below. |
| `GENERIC_{NAME}_PAYMENT_URL_JSON_PATH` | gjson path to the checkout URL in the create response. | `data.checkout_url` |
| `GENERIC_{NAME}_PROVIDER_TX_ID_JSON_PATH` | gjson path to the provider transaction ID in the create response. | `data.id` |
| `GENERIC_{NAME}_WEBHOOK_SIGNATURE_METHOD` | Signature algorithm: `HMAC_SHA256`, `HMAC_SHA512`, or `NONE`. | `HMAC_SHA256` |
| `GENERIC_{NAME}_WEBHOOK_SIGNATURE_HEADER` | HTTP header name that carries the webhook signature. | `x-signature` |
| `GENERIC_{NAME}_WEBHOOK_SIGNATURE_SECRET` | Secret used to compute the HMAC (required unless method is `NONE`). | `whsec_abc123` |
| `GENERIC_{NAME}_WEBHOOK_TX_ID_JSON_PATH` | gjson path to the transaction ID in the webhook body. | `order_id` |
| `GENERIC_{NAME}_WEBHOOK_SUCCESS_STATUS_PATH` | gjson path to the status field in the webhook body. | `status` |
| `GENERIC_{NAME}_WEBHOOK_SUCCESS_STATUS_VALUE` | Value at that path that means payment succeeded. | `PAID` |
| `GENERIC_{NAME}_WEBHOOK_FAILED_STATUS_VALUE` | Value at that path that means payment failed. | `FAILED` |
| `GENERIC_{NAME}_REFUND_URL` | URL to POST for refunds. | `https://api.myprovider.com/v1/refunds` |
| `GENERIC_{NAME}_REFUND_BODY_TEMPLATE` | Go `text/template` for the refund request body. | See below. |

### Optional Variables

| Variable | Default | Description | Example |
|---|---|---|---|
| `GENERIC_{NAME}_DISPLAY_NAME` | Derived from name | Human-readable name for UI. | `My Provider` |
| `GENERIC_{NAME}_QR_CODE_DATA_JSON_PATH` | - | gjson path to QR string (for QRIS-style flows). | `data.qr_string` |
| `GENERIC_{NAME}_STATUS_URL_TEMPLATE` | - | Go `text/template` for the status query URL. Omit to disable polling. | `https://api.myprovider.com/v1/payments/{{.ProviderTxID}}` |
| `GENERIC_{NAME}_STATUS_METHOD` | `GET` | HTTP method for status query. | `GET` |
| `GENERIC_{NAME}_STATUS_PAYMENT_STATUS_PATH` | - | gjson path to the status field in the status response. | `data.status` |
| `GENERIC_{NAME}_STATUS_SUCCESS_VALUE` | - | Value that means paid. | `PAID` |
| `GENERIC_{NAME}_STATUS_PENDING_VALUE` | - | Value that means pending. | `PENDING` |
| `GENERIC_{NAME}_STATUS_FAILED_VALUE` | - | Comma-separated values that mean failed. | `FAILED,EXPIRED` |
| `GENERIC_{NAME}_STATUS_REFUND_VALUE` | - | Value that means refunded. | `REFUNDED` |
| `GENERIC_{NAME}_STATUS_REFUND_AMOUNT_PATH` | - | gjson path to the refunded amount. | `data.refund_amount` |
| `GENERIC_{NAME}_STATUS_REFUND_CURRENCY_PATH` | - | gjson path to the refund currency. | `data.currency` |
| `GENERIC_{NAME}_WEBHOOK_TIMESTAMP_JSON_PATH` | - | gjson path to the webhook timestamp (for replay prevention). | `timestamp` |
| `GENERIC_{NAME}_WEBHOOK_TIMESTAMP_FORMAT` | - | Go time format for parsing the timestamp. | `2006-01-02T15:04:05Z` |
| `GENERIC_{NAME}_CANCEL_URL_TEMPLATE` | - | Go `text/template` for the cancel URL. Omit to disable cancellation. | `https://api.myprovider.com/v1/payments/{{.ProviderTxID}}/cancel` |
| `GENERIC_{NAME}_CANCEL_METHOD` | `POST` | HTTP method for cancellation. | `POST` |
| `GENERIC_{NAME}_CANCEL_BODY_TEMPLATE` | - | Go `text/template` for the cancel body (optional). | `{"reason":"{{.Reason}}"}` |
| `GENERIC_{NAME}_CANCEL_STATUS_PATH` | - | gjson path to the status in the cancel response. | `status` |
| `GENERIC_{NAME}_CANCEL_SUCCESS_VALUES` | - | Comma-separated values meaning successfully canceled. | `CANCELED,CANCELLED` |
| `GENERIC_{NAME}_CANCEL_EXPIRED_VALUES` | - | Comma-separated values meaning already expired. | `EXPIRED` |
| `GENERIC_{NAME}_CANCEL_PAID_VALUES` | - | Comma-separated values meaning already paid. | `PAID,SETTLED` |
| `GENERIC_{NAME}_CANCEL_PENDING_VALUES` | - | Comma-separated values meaning still processing. | `PENDING` |

### Template Variables

`CREATE_INTENT_BODY_TEMPLATE` and `REFUND_BODY_TEMPLATE` are Go `text/template` strings. Available fields:

| Field | Type | Description |
|---|---|---|
| `.InternalOrderID` | string | Internal transaction UUID |
| `.UserID` | string | AGS user ID |
| `.Amount` | int64 | Amount in minor units |
| `.CurrencyCode` | string | ISO 4217 currency code |
| `.Description` | string | Payment description |
| `.CallbackURL` | string | Webhook URL the provider should POST to |
| `.ReturnURL` | string | Browser redirect URL after payment |
| `.ProviderTxID` | string | Provider transaction ID (refund template only) |

### gjson Path Format

JSON paths use [gjson](https://github.com/tidwall/gjson) syntax. Strip any leading `$.` — the adapter removes it automatically.

```text
data.id              → response body: {"data":{"id":"txn_123"}}
data.checkout_url    → response body: {"data":{"checkout_url":"https://..."}}
```

## Example Configuration

```ini
# Enable the provider
GENERIC_MYPROVIDER_AUTH_HEADER=Authorization
GENERIC_MYPROVIDER_AUTH_VALUE=Bearer sk_test_abc123
GENERIC_MYPROVIDER_DISPLAY_NAME=My Provider

# Payment intent
GENERIC_MYPROVIDER_CREATE_INTENT_URL=https://api.myprovider.com/v1/payments
GENERIC_MYPROVIDER_CREATE_INTENT_BODY_TEMPLATE={"order_id":"{{.InternalOrderID}}","amount":{{.Amount}},"currency":"{{.CurrencyCode}}","redirect_url":"{{.ReturnURL}}","callback_url":"{{.CallbackURL}}"}
GENERIC_MYPROVIDER_PAYMENT_URL_JSON_PATH=data.checkout_url
GENERIC_MYPROVIDER_PROVIDER_TX_ID_JSON_PATH=data.id

# Webhook
GENERIC_MYPROVIDER_WEBHOOK_SIGNATURE_METHOD=HMAC_SHA256
GENERIC_MYPROVIDER_WEBHOOK_SIGNATURE_HEADER=x-signature
GENERIC_MYPROVIDER_WEBHOOK_SIGNATURE_SECRET=whsec_abc123
GENERIC_MYPROVIDER_WEBHOOK_TX_ID_JSON_PATH=order_id
GENERIC_MYPROVIDER_WEBHOOK_SUCCESS_STATUS_PATH=status
GENERIC_MYPROVIDER_WEBHOOK_SUCCESS_STATUS_VALUE=PAID
GENERIC_MYPROVIDER_WEBHOOK_FAILED_STATUS_VALUE=FAILED

# Status polling (optional)
GENERIC_MYPROVIDER_STATUS_URL_TEMPLATE=https://api.myprovider.com/v1/payments/{{.ProviderTxID}}
GENERIC_MYPROVIDER_STATUS_PAYMENT_STATUS_PATH=data.status
GENERIC_MYPROVIDER_STATUS_SUCCESS_VALUE=PAID
GENERIC_MYPROVIDER_STATUS_PENDING_VALUE=PENDING
GENERIC_MYPROVIDER_STATUS_FAILED_VALUE=FAILED,EXPIRED

# Refund
GENERIC_MYPROVIDER_REFUND_URL=https://api.myprovider.com/v1/refunds
GENERIC_MYPROVIDER_REFUND_BODY_TEMPLATE={"payment_id":"{{.ProviderTxID}}","amount":{{.Amount}},"reason":"customer request"}

# Cancellation (optional)
GENERIC_MYPROVIDER_CANCEL_URL_TEMPLATE=https://api.myprovider.com/v1/payments/{{.ProviderTxID}}/cancel
GENERIC_MYPROVIDER_CANCEL_STATUS_PATH=status
GENERIC_MYPROVIDER_CANCEL_SUCCESS_VALUES=CANCELED,CANCELLED
GENERIC_MYPROVIDER_CANCEL_EXPIRED_VALUES=EXPIRED
GENERIC_MYPROVIDER_CANCEL_PAID_VALUES=PAID,SETTLED
GENERIC_MYPROVIDER_CANCEL_PENDING_VALUES=PENDING
```

With this block, the adapter registers as `provider_myprovider` and clients use `"providerId": "provider_myprovider"`.

## Flow

### Payment

```text
Client -> POST /payment/v1/payment/intent { providerId: "provider_myprovider" }
  -> PaymentService creates a PENDING transaction
  -> Generic adapter POSTs CREATE_INTENT_BODY_TEMPLATE to CREATE_INTENT_URL
  -> Extracts paymentUrl from PAYMENT_URL_JSON_PATH
  -> Extracts providerTxID from PROVIDER_TX_ID_JSON_PATH
  -> Returns paymentUrl to client
  -> Client opens paymentUrl in browser
  -> Provider POSTs webhook to /payment/v1/webhook/provider_myprovider
  -> Signature validated via WEBHOOK_SIGNATURE_METHOD
  -> WEBHOOK_SUCCESS_STATUS_VALUE match: PENDING -> FULFILLING -> AGS item granted -> FULFILLED
```

### Cancellation

```text
Cancel -> renders CANCEL_URL_TEMPLATE with ProviderTxID
  -> HTTP CANCEL_METHOD to rendered URL
  -> reads CANCEL_STATUS_PATH from response
  -> matches CANCEL_SUCCESS_VALUES  -> CancelStatusCanceled
  -> matches CANCEL_EXPIRED_VALUES  -> CancelStatusExpired
  -> matches CANCEL_PAID_VALUES     -> CancelStatusAlreadyPaid
  -> matches CANCEL_PENDING_VALUES  -> CancelStatusPending
  -> (other / error)                -> CancelStatusFailed
```

Cancellation is disabled when `CANCEL_URL_TEMPLATE` is not set.

### Refund

```text
Admin refund -> renders REFUND_BODY_TEMPLATE with ProviderTxID and Amount
  -> POST REFUND_URL with rendered body
  -> reverse AGS fulfillment
```

### Sync

```text
Sync -> renders STATUS_URL_TEMPLATE with ProviderTxID
  -> HTTP STATUS_METHOD to rendered URL
  -> reads STATUS_PAYMENT_STATUS_PATH from response
  -> matches STATUS_SUCCESS_VALUE   -> SyncPaymentStatusPaid
  -> matches STATUS_PENDING_VALUE   -> SyncPaymentStatusPending
  -> matches STATUS_FAILED_VALUE    -> SyncPaymentStatusFailed
  -> matches STATUS_REFUND_VALUE    -> SyncPaymentStatusPaid + SyncRefundStatusRefunded
```

Status polling is disabled (returns `ErrNotSupported`) when `STATUS_URL_TEMPLATE` is not set.

## Status Mapping

| Generic adapter status | App status |
|---|---|
| Matches `WEBHOOK_SUCCESS_STATUS_VALUE` | `SUCCESS` |
| Matches `WEBHOOK_FAILED_STATUS_VALUE` | `FAILED` |
| (others in webhook) | `PENDING` |
| Matches `STATUS_SUCCESS_VALUE` | `PAID` (sync) |
| Matches `STATUS_FAILED_VALUE` | `FAILED` (sync) |
| Matches `STATUS_REFUND_VALUE` | `PAID` + `REFUNDED` (sync) |

## Testing

```bash
# Generic adapter unit tests
go test ./internal/adapter/generic -count=1

# Full test suite
go test ./... -count=1
```
