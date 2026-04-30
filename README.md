# Extend Regional Payment Gateway

A payment gateway service extension for [AccelByte Game Services (AGS)](https://accelbyte.io) that connects your game economy to regional payment providers. It handles payment intent creation, webhook processing, item fulfillment, and refunds — so your players can pay with the methods they trust.

---

## Supported Payment Providers

| Provider | Type | Notes |
|---|---|---|
| [Xendit](https://dashboard.xendit.co) | First-class SDK | Hosted checkout via Payment Sessions, multi-country/currency |
| [KOMOJU](https://komoju.com) | First-class SDK | Hosted checkout via Sessions API, HMAC-SHA256 webhooks |
| Generic HTTP | Config-driven | Any provider configured via `GENERIC_{NAME}_*` env vars — no code changes required |

---

## Prerequisites

- Go 1.21+
- Docker (for local MongoDB)
- [ngrok](https://ngrok.com/download) (to receive provider webhooks locally)
- Provider sandbox/test accounts as needed

---

## Quick Start

### 1. Clone and configure

```bash
git clone https://github.com/nauvalfirdaus/extend-regional-payment-gateway.git
cd extend-regional-payment-gateway
cp .env.example .env.local
# Edit .env.local with your credentials
```

### 2. Start MongoDB

```bash
docker run -d -p 27017:27017 mongo:6
```

Leave `DOCDB_HOST` empty in `.env.local` — the server connects to `localhost:27017` automatically.

### 3. Start ngrok (for receiving webhooks)

```bash
ngrok http 8000
```

Copy the HTTPS forwarding URL and set it as `PUBLIC_BASE_URL` in `.env.local`. Also configure this URL as the webhook endpoint in your provider's dashboard.

### 4. Run the server

```bash
set -a && source .env.local && set +a && go run .
```

Expected output:
```
registered Xendit adapter provider=xendit
starting HTTP gateway port=8000
starting gRPC server port=6565
```

### 5. Explore the API

Open [http://localhost:8000/payment/apidocs/](http://localhost:8000/payment/apidocs/) in your browser.

---

## Key Environment Variables

| Variable | Required | Description |
|---|---|---|
| `AB_BASE_URL` | Yes | AccelByte base URL |
| `AB_CLIENT_ID` | Yes | AccelByte client ID |
| `AB_CLIENT_SECRET` | Yes | AccelByte client secret |
| `AB_NAMESPACE` | Yes | AccelByte namespace |
| `PUBLIC_BASE_URL` | Yes | Public URL (ngrok URL in local dev) |
| `PLUGIN_GRPC_SERVER_AUTH_ENABLED` | No | Set `false` to disable auth locally |
| `XENDIT_SECRET_API_KEY` | Xendit | Enables the Xendit adapter |
| `KOMOJU_SECRET_KEY` | KOMOJU | Enables the KOMOJU adapter |
| `GENERIC_{NAME}_AUTH_HEADER` | Generic | Enables a generic provider named `{NAME}` |

See `.env.example` for the full list of variables.

---

## API Endpoints

All HTTP endpoints are served under `/payment`.

### Create a Payment Intent

```
POST /payment/v1/payment/intent
```

```json
{
  "provider": "PROVIDER_XENDIT",
  "itemId": "<ags-item-id>",
  "quantity": 1,
  "clientOrderId": "order-001",
  "description": "Test Payment",
  "regionCode": "ID"
}
```

Returns a `paymentUrl` to redirect the user to.

### Check Transaction Status

```
GET /payment/v1/payment/{transactionId}
```

Returns `status`: `PENDING → FULFILLING → FULFILLED / FAILED`.

### Refund a Transaction

```
POST /v1/admin/namespace/{namespace}/transactions/{transactionId}/refund
```

Requires a fulfilled transaction. Reverses the AGS fulfillment and calls the provider's refund API.

---

## Running Tests

```bash
# All tests
go test ./... -v -count=1

# Unit tests only (no external deps)
go test ./internal/adapter/generic/... ./internal/store/memory/... -v -count=1

# Integration tests (requires local MongoDB)
docker run -d -p 27017:27017 mongo:6
go test ./internal/store/docdb/... -v
```

Or via Makefile:

```bash
make test        # all tests
make test-unit   # unit tests only
```

---

## Docker Build

```bash
docker build -t extend-regional-payment-gateway:latest .
# or
make docker-build
```

Exposed ports: `6565` (gRPC), `8000` (HTTP), `8080` (Prometheus).

---

## Architecture

```
Client
  └─ POST /payment/v1/payment/intent
       └─ PaymentService.CreatePaymentIntent
            └─ Insert PENDING transaction
            └─ Call provider → return paymentUrl

Provider
  └─ POST /payment/v1/webhook/{provider}
       └─ WebhookService.HandleWebhook
            └─ Claim row: PENDING → FULFILLING
            └─ FulfillItemShort (AGS)
            └─ FULFILLING → FULFILLED
```

Key design points:
- **Adapter pattern** — each provider implements a common `PaymentProvider` interface
- **Idempotent webhooks** — atomic claim prevents double-fulfillment
- **Background scheduler** — retries stuck FULFILLING rows and polls for lost webhooks
- **Generic adapter** — any provider can be added without code changes via `GENERIC_{NAME}_*` env vars

---

## Adding a New Provider

**No code changes required** for generic providers — add a `GENERIC_{NAME}_*` env block to your environment file and restart. The server discovers and registers it at startup.

For first-class SDK integrations, follow the Xendit or KOMOJU adapter pattern under `internal/adapter/{provider}/`.

---

## Proto Regeneration

The service is defined in `pkg/proto/payment.proto`. To regenerate Go code and Swagger docs:

```bash
# Install: protoc, protoc-gen-go, protoc-gen-go-grpc, protoc-gen-grpc-gateway, protoc-gen-openapiv2
make proto
```

---

## Documentation

Provider integration guides are in [`docs/`](docs/):

- [`docs/Xendit_Adapter.md`](docs/Xendit_Adapter.md) — Xendit integration guide
- [`docs/KOMOJU_Adapter.md`](docs/KOMOJU_Adapter.md) — KOMOJU integration guide
