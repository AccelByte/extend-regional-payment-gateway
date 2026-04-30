# Xendit Adapter - Developer Guide

AccelByte Engineering / Developer Experience
April 2026

## Overview

The Xendit adapter is a first-class hosted checkout integration registered as provider `"xendit"`.

New payments use Xendit Payment Sessions:

- `POST /sessions`
- `session_type = PAY`
- `mode = PAYMENT_LINK`

The returned `payment_session_id` is stored as `provider_tx_id`, and the returned `payment_link_url` is returned to the caller as the hosted checkout URL.

Webhooks are still supported as the fast asynchronous fulfillment path, but they are not the source of truth for manual sync. Sync is reconciliation: it queries Xendit for current session/payment status and transaction history so missed webhooks can be recovered after downtime.

## Configuration

| Variable | Required | Default | Description |
|---|---|---|---|
| `XENDIT_SECRET_API_KEY` | Yes | - | Secret API key from Xendit Dashboard. |
| `XENDIT_CALLBACK_TOKEN` | Yes | - | Expected `x-callback-token` for webhook validation. |
| `XENDIT_API_BASE_URL` | No | `https://api.xendit.co` | Base URL for Xendit APIs. |
| `XENDIT_DEFAULT_COUNTRY` | No | `ID` | Country used when request `regionCode` is empty. |
| `XENDIT_ALLOWED_COUNTRIES` | No | `ID,PH,VN,TH,SG,MY,HK,MX` | Country allowlist. |
| `XENDIT_ALLOWED_CURRENCIES` | No | `IDR,PHP,VND,THB,SGD,MYR,USD,HKD,AUD,GBP,EUR,JPY,MXN` | Currency allowlist. |

Configure the Payment Sessions webhook URL in the Xendit Dashboard:

```text
{PUBLIC_BASE_URL}{BASE_PATH}/v1/webhook/xendit
```

## Payment Flow

```text
Client
  -> POST /payment/v1/payment/intent
       provider = PROVIDER_XENDIT
       -> create local PENDING transaction
       -> POST /sessions
       -> store payment_session_id as provider_tx_id
       -> return payment_link_url
```

The Payment Session request uses:

- `reference_id` = internal transaction ID
- `amount` and `currency` = AGS item total
- `country` = normalized AGS region/country
- `success_return_url`, `failure_return_url`, `cancel_return_url` = local payment result page
- `metadata.internalOrderId` = internal transaction ID

## Webhooks

`POST /payment/v1/webhook/xendit` is public and validated with `x-callback-token` before any state mutation.

Webhook handling only parses provider notifications:

- Payment Session events update fulfilled/failed/canceled/expired payment state.
- Refund events can reverse AGS fulfillment when a dashboard-side refund is reported.

Webhook delivery is best-effort. If the service is down, the sync endpoint must be used to recover state.

## Refunds

Refunds use the Xendit SDK Refund API.

For Payment Session transactions, the adapter first resolves the underlying `payment_request_id` from `GET /sessions/{payment_session_id}`. Refund creation never sends the Payment Session ID as the refund target.

```text
Admin refund
  -> FULFILLED -> REFUNDING
  -> resolve payment_request_id
  -> SDK RefundApi.CreateRefund(payment_request_id, amount, currency)
  -> reverse AGS fulfillment
  -> REFUNDED
```

If provider refund succeeds but AGS reversal fails, retry skips the provider refund and retries only AGS reversal.

## Sync / Reconciliation

Sync is not webhook handling. It reads provider state and transaction history:

```text
Sync transaction
  -> GET /sessions/{payment_session_id}
  -> GET /v3/payment_requests/{payment_request_id}, when available
  -> GET /v3/payments/{payment_id}, when available
  -> SDK TransactionApi.GetAllTransactions by reference_id and product_id
  -> SDK RefundApi.GetAllRefunds by payment_request_id
  -> derive local action
```

Rules:

- Provider `PAYMENT + SUCCESS` means local `PENDING` can be fulfilled.
- Provider failed/expired/canceled/voided/reversed state means local `PENDING` can be marked failed.
- Provider full refund means fulfilled local entitlement can be reversed.
- Provider partial refund is reported as `PARTIAL_REFUNDED`; AGS reversal is not automatic.

Transaction history is the recovery path for missed webhooks, including dashboard-created refunds.

## Tests

Run:

```bash
go test ./internal/adapter/xendit ./pkg/service -count=1
go test ./... -count=1
```

Key coverage:

- Hosted checkout creates a Payment Session and returns `payment_link_url`.
- Sync recovers a paid transaction from transaction history when webhook was missed.
- Sync marks failed/expired provider sessions terminal.
- Sync detects dashboard refunds from transaction history.
- Refund resolves `payment_request_id` before calling the Refund API.
- Partial refunds are reported without automatic AGS reversal.
