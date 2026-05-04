# Extend Regional Payment Gateway

A payment gateway service extension for [AccelByte Game Services (AGS)](https://accelbyte.io) that connects your game economy to regional payment providers. It handles payment intent creation, webhook processing, item fulfillment, and refunds — so your players can pay with the methods they trust.

---

## Overview

This service runs as a single Go binary exposing:

- **gRPC** on port `6565`
- **HTTP/REST** (gRPC-Gateway) on port `8000`
- **Prometheus metrics** on port `8080`

Clients create a payment intent by specifying a `providerId`. The service creates a pending transaction, calls the provider to generate a checkout URL, and returns it to the client. The provider posts a webhook back when payment completes, which triggers AGS item fulfillment.

```
Client
  └─ POST /payment/v1/payment/intent
       └─ Insert PENDING transaction
       └─ Call provider → return paymentUrl

Provider
  └─ POST /payment/v1/webhook/{providerId}
       └─ Validate signature
       └─ Claim row: PENDING → FULFILLING
       └─ FulfillItemShort (AGS)
       └─ FULFILLING → FULFILLED
```

A background scheduler retries stuck `FULFILLING` rows and polls for lost webhooks.

---

## Built-in Payment Providers

| Provider | Type | Notes | Guide |
|---|---|---|---|
| [Xendit](https://dashboard.xendit.co) | SDK adapter | Hosted checkout via Payment Sessions; multi-country/currency | [XenditGuide.md](docs/adapter/XenditGuide.md) |
| [KOMOJU](https://komoju.com) | SDK adapter | Hosted checkout via Sessions API; HMAC-SHA256 webhooks | [KomojuGuide.md](docs/adapter/KomojuGuide.md) |
| Generic HTTP | Config-driven | Any provider configured via `GENERIC_{NAME}_*` env vars; no code required | [GenericGuide.md](docs/adapter/GenericGuide.md) |

---

## Adding a New Provider

There are two options:

**Option 1 — Generic HTTP adapter (no code required)**

Add a `GENERIC_{NAME}_*` env var block to your environment and restart. The server discovers and registers the provider at startup. See [GenericGuide.md](docs/adapter/GenericGuide.md) for the full list of variables and an example configuration.

**Option 2 — First-class SDK adapter (Go implementation)**

For providers with complex flows (multi-step ID resolution, SDK clients, partial refund detection), implement the `adapter.PaymentProvider` interface in a new package under `internal/adapter/{vendor}/`. Follow the step-by-step guide in [AdapterTemplateGuide.md](docs/AdapterTemplateGuide.md).

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

### 3. Start ngrok

```bash
ngrok http 8000
```

Copy the HTTPS forwarding URL and set it as `PUBLIC_BASE_URL` in `.env.local`. Configure this same URL as the webhook endpoint in your provider's dashboard.

### 4. Run the server

```bash
set -a && source .env.local && set +a && go run .
```

Expected output:

```
registered Xendit adapter provider_id=provider_xendit
starting HTTP gateway port=8000
starting gRPC server port=6565
```

### 5. Explore the API

Open [http://localhost:8000/payment/apidocs/](http://localhost:8000/payment/apidocs/) in your browser.

---

## Deploy to AGS

To upload and run this service on AccelByte Game Services, follow the step-by-step deployment guide:

[DeployToAGSGuide.md](docs/DeployToAGSGuide.md)

It covers building the Docker image, creating the app in the AGS Admin Portal, pushing the container image with `extend-helper-cli`, configuring environment variables, deploying, and verifying the deployment.

---

## Configuration

Core environment variables. Provider-specific variables are documented in each provider guide under [`docs/adapter/`](docs/adapter/).

| Variable | Required | Description | Example |
|---|---|---|---|
| `AB_BASE_URL` | Yes | AccelByte base URL | `https://dev.sdkteam.accelbyte.io` |
| `AB_CLIENT_ID` | Yes | AccelByte M2M client ID | `abc123` |
| `AB_CLIENT_SECRET` | Yes | AccelByte M2M client secret | `secret` |
| `AB_NAMESPACE` | Yes | AccelByte namespace | `mygame` |
| `PUBLIC_BASE_URL` | Yes | Public URL of this service (ngrok URL in local dev) | `https://abc123.ngrok-free.app` |
| `BASE_PATH` | No | HTTP base path prefix. Default: `/payment` | `/payment` |
| `PLUGIN_GRPC_SERVER_AUTH_ENABLED` | No | Set `false` to disable AGS token auth locally. Default: `true` | `false` |
| `DOCDB_HOST` | No | DocumentDB host. Empty = `localhost:27017` | `mycluster.docdb.amazonaws.com:27017` |
| `DOCDB_USERNAME` | No | DocumentDB username | `admin` |
| `DOCDB_PASSWORD` | No | DocumentDB password | `secret` |
| `DOCDB_DATABASE_NAME` | No | Database name. Default: `payment` | `payment` |
| `PAYMENT_EXPIRY_DEFAULT` | No | Default payment session expiry. Default: `15m` | `30m` |
| `MAX_CONCURRENT_INTENT_PER_USER` | No | Max simultaneous pending payments per user. Default: `5` | `5` |
| `MAX_RETRIES` | No | Scheduler retry limit for stuck transactions. Default: `3` | `3` |
| `RECORD_RETENTION_DAYS` | No | Days to retain completed transaction records. Default: `90` | `90` |
| `PUBLIC_SYNC_COOLDOWN` | No | Minimum interval between user-triggered syncs. Default: `60s` | `60s` |
| `WEBHOOK_MAX_AGE` | No | Reject webhooks older than this duration. Default: `5m` | `5m` |
| `LOG_LEVEL` | No | Log level: `debug`, `info`, `warn`, `error`. Default: `info` | `info` |

---

## API Endpoints

All HTTP endpoints are served under the `BASE_PATH` prefix (default `/payment`).

### Create a Payment Intent

```
POST /payment/v1/payment/intent
```

```json
{
  "providerId": "provider_xendit",
  "itemId": "<ags-item-id>",
  "quantity": 1,
  "clientOrderId": "order-001",
  "description": "Test Payment",
  "regionCode": "ID"
}
```

Returns a `paymentUrl` to redirect the player to.

### Check Transaction Status

```
GET /payment/v1/payment/{transactionId}
```

Returns `status`: `PENDING → FULFILLING → FULFILLED / FAILED / CANCELED / EXPIRED`.

### Sync Transaction Status

```
POST /payment/v1/payment/{transactionId}/sync
```

Queries the provider for current state and reconciles the local transaction. Rate-limited per user by `PUBLIC_SYNC_COOLDOWN`.

### Refund a Transaction (Admin)

```
POST /v1/admin/namespace/{namespace}/transactions/{transactionId}/refund
```

Requires a `FULFILLED` transaction and a valid Bearer token (or `PLUGIN_GRPC_SERVER_AUTH_ENABLED=false` locally). Reverses the AGS fulfillment and calls the provider refund API.

```json
{
  "reason": "customer request"
}
```

### Webhook Receiver (Provider → Service)

```
POST /payment/v1/webhook/{providerId}
```

Public endpoint. The provider posts here after payment events. Signature is validated before any state mutation.

---

## Proto Regeneration

The service is defined in `pkg/proto/payment.proto`. To regenerate Go code and Swagger docs:

```bash
# Install: protoc, protoc-gen-go, protoc-gen-go-grpc, protoc-gen-grpc-gateway, protoc-gen-openapiv2
make proto
```

The generated files replace `pkg/pb/payment.go` and update `gateway/apidocs/payment.swagger.json`.
