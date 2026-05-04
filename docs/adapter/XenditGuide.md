# Xendit Adapter - Developer Guide

## Overview

The Xendit adapter is a hosted checkout integration registered by default as provider ID `provider_xendit`. Clients use:

```json
{
  "providerId": "provider_xendit"
}
```

The adapter creates a Xendit Payment Session via `POST /sessions` (`session_type=PAY`, `mode=PAYMENT_LINK`) and returns the `payment_link_url` as the checkout URL. The `payment_session_id` is stored as the provider transaction ID. Webhooks are validated via constant-time comparison of the `x-callback-token` header. Cancellation uses `POST /sessions/{payment_session_id}/cancel`. Refunds resolve the underlying `payment_request_id` from the session before calling the SDK `RefundApi.CreateRefund`. Sync queries session status, payment requests, payments V3, transaction history, and refund list to recover missed webhooks.

## Note

- When using Xendit in sandbox or test mode, configure your test item price and checkout region so the transaction uses IDR. Other currencies may be valid in live mode, but sandbox/test payments are expected to use IDR.

## Dashboard Setup

1. Go to your Xendit admin dashboard.
2. First, create a new API Key by going to Settings > API Keys.
3. Click on the Generate Secret Key button to create a new API Key. Set the permission to:

    ```text
    Money-in-products | WRITE
    Money-out-products | READ
    Transaction | READ
    ```

4. Keep the API key somewhere safe. You will need this for the configuration later.
5. Next, configure the webhook by going to Settings > Webhooks.
6. Copy and keep the Webhook verification token somewhere safe. You will need this for the configuration later.
7. Next, look for the Payment Session section. Fill the Payment Session Completed and Payment Session Expired fields with your Extend App URL by following the format below.

    ```text
    {PUBLIC_BASE_URL}{BASE_PATH}/v1/webhook/provider_xendit
    ```

    Example:

    ```text
    https://abc123.ngrok-free.app/payment/v1/webhook/provider_xendit
    ```

8. After saving the webhook URLs, make sure the webhook configuration is active, then copy the secret API key and webhook verification token into your Extend app environment variables. Restart the gateway after updating the environment so the Xendit adapter can load the new configuration.

## Configuration

| Variable | Required | Default | Description | Example |
|---|---|---|---|---|
| `XENDIT_SECRET_API_KEY` | Yes | - | Secret API key from Xendit Dashboard. Used for HTTP Basic Auth. Enables the adapter when set. | `xnd_development_abc123` |
| `XENDIT_CALLBACK_TOKEN` | Yes | - | Expected `x-callback-token` header value for webhook validation. | `abc123token` |
| `XENDIT_PROVIDER_ID` | No | `provider_xendit` | Stable provider ID used in API requests and webhook URL routing. | `provider_xendit` |
| `XENDIT_DISPLAY_NAME` | No | `Xendit` | Human-readable name returned for UI display. | `Xendit` |
| `XENDIT_API_BASE_URL` | No | `https://api.xendit.co` | Base URL for Xendit APIs. | `https://api.xendit.co` |
| `XENDIT_DEFAULT_COUNTRY` | No | `ID` | Country code used when the request `regionCode` is empty. Must be in the allowed countries list. | `ID` |
| `XENDIT_ALLOWED_COUNTRIES` | No | `ID,PH,VN,TH,SG,MY,HK,MX` | Comma-separated country code allowlist. | `ID,PH,SG` |
| `XENDIT_ALLOWED_CURRENCIES` | No | `IDR,PHP,VND,THB,SGD,MYR,USD,HKD,AUD,GBP,EUR,JPY,MXN` | Comma-separated currency code allowlist. | `IDR,PHP,SGD` |

Example `.env.local` block:

```ini
XENDIT_SECRET_API_KEY=xnd_development_abc123
XENDIT_CALLBACK_TOKEN=abc123token
XENDIT_PROVIDER_ID=provider_xendit
XENDIT_DISPLAY_NAME=Xendit
XENDIT_API_BASE_URL=https://api.xendit.co
XENDIT_DEFAULT_COUNTRY=ID
XENDIT_ALLOWED_COUNTRIES=ID,PH,VN,TH,SG,MY,HK,MX
XENDIT_ALLOWED_CURRENCIES=IDR,PHP,VND,THB,SGD,MYR,USD,HKD,AUD,GBP,EUR,JPY,MXN
```

## Flow

### Payment

```text
Client -> POST /payment/v1/payment/intent { providerId: "provider_xendit" }
  -> PaymentService creates a PENDING transaction
  -> Xendit adapter calls POST /sessions
       reference_id = internal transaction ID
       session_type = PAY, mode = PAYMENT_LINK
       country from regionCode or XENDIT_DEFAULT_COUNTRY
  -> Stores payment_session_id as provider_tx_id
  -> Returns payment_link_url as paymentUrl to client
  -> Client opens paymentUrl in browser
  -> Xendit POSTs webhook events to /payment/v1/webhook/provider_xendit
  -> Payment Session Completed webhook: PENDING -> FULFILLING -> AGS item granted -> FULFILLED
```

### Cancellation

```text
Cancel -> POST /sessions/{payment_session_id}/cancel  (only supported for ps- prefix IDs)
  -> COMPLETED / PAID / SUCCEEDED / SUCCESS -> CancelStatusAlreadyPaid
  -> CANCELED / CANCELLED                  -> CancelStatusCanceled
  -> EXPIRED                               -> CancelStatusExpired
  -> ACTIVE / PENDING                      -> CancelStatusPending
  -> (other)                               -> CancelStatusFailed
```

### Refund

```text
Admin refund
  -> FULFILLED -> REFUNDING
  -> Resolve payment_request_id:
       ps- prefix: GET /sessions/{payment_session_id} -> extract payment_request_id
       pr- prefix: use directly
       py- prefix: GET /v3/payments/{id} -> extract payment_request_id
  -> SDK RefundApi.CreateRefund(payment_request_id, amount, currency)
       idempotency key: {internalOrderID}-{providerTxID}
  -> Reverse AGS fulfillment
  -> REFUNDED
```

If the provider refund succeeds but AGS reversal fails, retry skips the provider refund and retries only AGS reversal.

### Sync / Reconciliation

```text
Sync transaction
  -> GET /sessions/{payment_session_id}
  -> GET /v3/payment_requests/{payment_request_id}, when available
  -> GET /v3/payments/{payment_id}, when available
  -> SDK TransactionApi.GetAllTransactions by reference_id and product_id
  -> SDK RefundApi.GetAllRefunds by payment_request_id
  -> Derive local action
```

Rules:

- Provider `PAYMENT + SUCCESS` means local `PENDING` can be fulfilled.
- Provider failed/expired/canceled/voided/reversed state means local `PENDING` can be marked failed.
- Provider full refund means fulfilled local entitlement can be reversed.
- Provider partial refund is reported as `PARTIAL_REFUNDED`; AGS reversal is not automatic.

Transaction history is the recovery path for missed webhooks, including dashboard-created refunds.

## Status Mapping

### Payment Session Status

| Xendit status | App status |
|---|---|
| `COMPLETED`, `PAID`, `SUCCEEDED`, `SUCCESS` | `SUCCESS` |
| `CANCELED`, `CANCELLED` | `CANCELED` |
| `EXPIRED` | `EXPIRED` |
| `FAILED` | `FAILED` |
| (others) | `PENDING` |

### Refund Summary Status

| Refund state | Sync status |
|---|---|
| Total succeeded amount ≥ transaction amount | `REFUNDED` |
| Some succeeded amount > 0 | `PARTIAL_REFUNDED` |
| Any pending | `PENDING` |
| Any failed / cancelled | `FAILED` |
| (no refunds) | `NONE` |

## Testing

```bash
# Adapter and service unit tests
go test ./internal/adapter/xendit ./pkg/service -count=1

# Full test suite
go test ./... -count=1

# Certification test — validates real credentials against the Xendit API
go test -tags=xendit_cert ./internal/adapter/xendit -count=1
```
