# KOMOJU Adapter - Developer Guide

## Overview

The KOMOJU adapter is a first-class hosted checkout integration. It registers as `komoju` and clients use:

```json
{
  "provider": "PROVIDER_KOMOJU"
}
```

The adapter uses KOMOJU Hosted Page through `POST /api/v1/sessions`, returns KOMOJU `session_url` to the player, validates webhook HMAC signatures from `X-Komoju-Signature`, polls `GET /api/v1/sessions/{id}` or `GET /api/v1/payments/{id}`, cancels pending sessions through `POST /api/v1/sessions/{id}/cancel`, and refunds through `POST /api/v1/payments/{id}/refund`.

## Dashboard Setup

Configure a webhook in the KOMOJU dashboard:

```text
{PUBLIC_BASE_URL}{BASE_PATH}/v1/webhook/komoju
```

Enable at least:

```text
payment.captured
payment.authorized
payment.updated
payment.expired
payment.cancelled
payment.failed
payment.refunded
```

Use the same webhook secret token as `KOMOJU_WEBHOOK_SECRET`.

## Configuration

| Variable | Required | Default | Description |
|---|---|---|---|
| `KOMOJU_SECRET_KEY` | Yes | - | Secret API key. Enables the adapter when set. |
| `KOMOJU_WEBHOOK_SECRET` | Yes | - | HMAC-SHA256 webhook signing secret. |
| `KOMOJU_API_VERSION` | No | `2025-01-28` | Sent as `X-KOMOJU-API-VERSION`. |
| `KOMOJU_DEFAULT_LOCALE` | No | `en` | Hosted Page locale: `en`, `ja`, or `ko`. |
| `KOMOJU_ALLOWED_CURRENCIES` | No | KOMOJU Session enum currencies | Currency allowlist for session creation. |

Example:

```ini
KOMOJU_SECRET_KEY=sk_test_...
KOMOJU_WEBHOOK_SECRET=...
KOMOJU_API_VERSION=2025-01-28
KOMOJU_DEFAULT_LOCALE=en
KOMOJU_ALLOWED_CURRENCIES=JPY,USD,EUR,TWD,KRW,PLN,GBP,HKD,SGD,NZD,AUD,IDR,MYR,PHP,THB,CNY,BRL,CHF,CAD,VND
```

## Flow

```text
Client -> POST /payment/v1/payment/intent { provider: "PROVIDER_KOMOJU" }
  -> PaymentService creates a PENDING transaction
  -> KOMOJU adapter creates a Hosted Page Session
  -> Client opens paymentUrl = session_url
  -> KOMOJU sends payment webhooks to /payment/v1/webhook/komoju
  -> payment.captured fulfills the AGS item
```

The initial stored provider transaction ID is the KOMOJU Session ID. Once KOMOJU creates a Payment, webhook handling or scheduler polling replaces it with the Payment ID so refunds can call the Payment Refund API.

## Status Mapping

| KOMOJU payment status | Adapter status |
|---|---|
| `captured` | `SUCCESS` |
| `authorized`, `pending`, other updates | `PENDING` |
| `expired`, `cancelled`, `failed` | `FAILED` |
| `refunded` | `REFUNDED` |

## Testing

```bash
go test ./internal/adapter/komoju -count=1
go test ./pkg/service ./internal/checkout ./pkg/common -count=1
go test ./... -count=1
```
