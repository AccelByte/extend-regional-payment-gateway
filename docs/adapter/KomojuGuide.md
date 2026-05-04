# KOMOJU Adapter - Developer Guide

## Overview

The KOMOJU adapter is a hosted checkout integration. It registers as `provider_komoju` by default and clients use:

```json
{
  "providerId": "provider_komoju"
}
```

The adapter creates a KOMOJU Hosted Page Session via `POST /api/v1/sessions` and returns the `session_url` to the player as the checkout URL. Webhook events are validated with HMAC-SHA256 on the `X-Komoju-Signature` header. Payment status is polled via `GET /api/v1/sessions/{id}` or `GET /api/v1/payments/{id}`. Cancellation uses `POST /api/v1/sessions/{id}/cancel` and refunds use `POST /api/v1/payments/{id}/refund`.

## Note

- When using KOMOJU in sandbox or test mode, configure your test item price and checkout region so the transaction uses JPY. Other currencies may be valid in live mode, but sandbox/test payments are expected to use JPY.

## Dashboard Setup

1. Go to your KOMOJU admin dashboard.
2. Then, go to Settings. Here you can find the API keys and other credentials that you will need for configuration later.
3. Next, configure the webhook by going to Manage > Webhooks.
4. Then, create a new webhook or edit an existing one.
5. Fill the Webhook URL with your Extend App URL by following the format below.

    ```text
    {PUBLIC_BASE_URL}{BASE_PATH}/v1/webhook/provider_komoju
    ```

    Example:

    ```text
    https://abc123.ngrok-free.app/payment/v1/webhook/provider_komoju
    ```

6. For the Secret Key, fill with any strong password and keep it somewhere safe. You will need this for the configuration later.
7. Then, enable these webhook events:

    ```text
    payment.authorized
    payment.captured
    payment.updated
    payment.expired
    payment.cancelled
    payment.refund.created
    payment.refunded
    payment.failed
    payment.marked.as.fraud
    settlement.created
    refund_request.updated
    ```

8. Once done, click on the Save webhook button to save the config.
9. After saving, make sure the webhook is active in the dashboard, then copy the API key and webhook secret into your Extend app environment variables. Restart the gateway after updating the environment so the KOMOJU adapter can load the new configuration.

## Configuration

| Variable | Required | Default | Description | Example |
|---|---|---|---|---|
| `KOMOJU_SECRET_KEY` | Yes | - | Secret API key. Enables the adapter when set. Used for Basic Auth. | `sk_test_abc123` |
| `KOMOJU_WEBHOOK_SECRET` | Yes | - | HMAC-SHA256 webhook signing secret. Must match the Secret Key set in the dashboard. | `whsec_abc123` |
| `KOMOJU_PROVIDER_ID` | No | `provider_komoju` | Stable provider ID used in API requests and webhook URL routing. | `provider_komoju` |
| `KOMOJU_DISPLAY_NAME` | No | `KOMOJU` | Human-readable name returned for UI display. | `KOMOJU` |
| `KOMOJU_API_VERSION` | No | `2025-01-28` | API version sent as `X-KOMOJU-API-VERSION` header. | `2025-01-28` |
| `KOMOJU_BASE_URL` | No | `https://komoju.com` | Base URL for KOMOJU API. Trailing slash is removed automatically. | `https://komoju.com` |
| `KOMOJU_DEFAULT_LOCALE` | No | `en` | Hosted Page locale. Valid values: `en`, `ja`, `ko`. Invalid values default to `en`. | `en` |
| `KOMOJU_ALLOWED_CURRENCIES` | No | `JPY,USD,EUR,TWD,KRW,PLN,GBP,HKD,SGD,NZD,AUD,IDR,MYR,PHP,THB,CNY,BRL,CHF,CAD,VND` | Comma-separated currency code allowlist for session creation. | `JPY,USD,IDR` |

Example `.env.local` block:

```ini
KOMOJU_SECRET_KEY=sk_test_abc123
KOMOJU_WEBHOOK_SECRET=whsec_abc123
KOMOJU_PROVIDER_ID=provider_komoju
KOMOJU_DISPLAY_NAME=KOMOJU
KOMOJU_API_VERSION=2025-01-28
KOMOJU_BASE_URL=https://komoju.com
KOMOJU_DEFAULT_LOCALE=en
KOMOJU_ALLOWED_CURRENCIES=JPY,USD,EUR,TWD,KRW,PLN,GBP,HKD,SGD,NZD,AUD,IDR,MYR,PHP,THB,CNY,BRL,CHF,CAD,VND
```

## Flow

### Payment

```text
Client -> POST /payment/v1/payment/intent { providerId: "provider_komoju" }
  -> PaymentService creates a PENDING transaction
  -> KOMOJU adapter calls POST /api/v1/sessions
  -> Returns session_url as paymentUrl to client
  -> Client opens paymentUrl in browser
  -> KOMOJU POSTs webhook events to /payment/v1/webhook/provider_komoju
  -> payment.captured webhook: PENDING -> FULFILLING -> AGS item granted -> FULFILLED
```

The initial stored provider transaction ID is the KOMOJU Session ID. Once KOMOJU creates a Payment object, webhook handling or scheduler polling replaces it with the Payment ID so that refunds can call the Payment Refund API.

### Cancellation

```text
Cancel -> POST /api/v1/sessions/{sessionId}/cancel
  -> On 422 or 404: check current status
     -> GET /api/v1/sessions/{sessionId} or GET /api/v1/payments/{paymentId}
     -> already paid/refunded  -> CancelStatusAlreadyPaid
     -> failed/canceled        -> CancelStatusCanceled
     -> expired                -> CancelStatusExpired
  -> On success: CancelStatusCanceled
```

### Refund

```text
Admin refund -> resolve payment ID from stored provider_tx_id
  -> POST /api/v1/payments/{paymentId}/refund
       idempotency key: refund-{internalOrderID}
  -> reverse AGS fulfillment
```

### Sync

```text
Sync -> GET /api/v1/sessions/{id}
     -> fallback: GET /api/v1/payments/{id}
     -> map KOMOJU status to sync status
     -> derive local action (fulfill / fail / refund)
```

## Status Mapping

| KOMOJU payment status | App status |
|---|---|
| `captured` | `SUCCESS` |
| `authorized`, `pending`, other updates | `PENDING` |
| `expired` | `EXPIRED` |
| `cancelled` / `canceled` | `CANCELED` |
| `failed` | `FAILED` |
| `refunded` | `REFUNDED` |

## Testing

```bash
# Adapter unit tests
go test ./internal/adapter/komoju -count=1

# Service and related tests
go test ./pkg/service ./internal/checkout ./pkg/common -count=1

# Full test suite
go test ./... -count=1

# Certification test — validates real credentials against the KOMOJU API
go test -tags=komoju_cert ./internal/adapter/komoju -count=1
```
