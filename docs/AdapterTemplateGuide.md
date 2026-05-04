# Adapter Template Guide

This guide walks you through adding a new first-class payment provider adapter to the gateway. Follow each step in order. When a step says "see an existing adapter for reference", the KOMOJU adapter (`internal/adapter/komoju/`) and the Xendit adapter (`internal/adapter/xendit/`) are the canonical examples.

---

## Step 1 — Decide on the Provider ID

Choose a stable, lowercase provider ID. It is used as the URL segment in the webhook route and as the key in the adapter registry. It never changes after launch.

```text
provider_{vendor}              # single integration
provider_{vendor}_{region}     # multi-region, e.g. provider_xendit_ph
provider_{vendor}_sandbox      # sandbox/test instance
```

Examples: `provider_komoju`, `provider_xendit`, `provider_xendit_id`

---

## Step 2 — Create the Adapter Package

Create a new directory under `internal/adapter/`:

```text
internal/adapter/{vendor}/
  config.go
  {vendor}.go
  {vendor}_test.go
  {vendor}_certification_test.go
```

---

## Step 3 — Write `config.go`

The config file loads all env vars for the provider. Follow this pattern:

```go
package {vendor}

import (
    "fmt"
    "os"
    "strings"
)

type Config struct {
    ProviderID  string
    DisplayName string
    SecretKey   string
    // ... other fields
}

// Load returns nil, nil when the required env var is absent.
// This lets main.go skip registration rather than fail at startup.
func Load() (*Config, error) {
    secret := strings.TrimSpace(os.Getenv("{VENDOR}_SECRET_KEY"))
    if secret == "" {
        return nil, nil
    }
    // Validate required fields
    webhookSecret := strings.TrimSpace(os.Getenv("{VENDOR}_WEBHOOK_SECRET"))
    if webhookSecret == "" {
        return nil, fmt.Errorf("{VENDOR}_WEBHOOK_SECRET is required when {VENDOR}_SECRET_KEY is set")
    }
    return &Config{
        ProviderID:  getEnv("{VENDOR}_PROVIDER_ID", "provider_{vendor}"),
        DisplayName: getEnv("{VENDOR}_DISPLAY_NAME", "{Vendor}"),
        SecretKey:   secret,
        // ...
    }, nil
}

func getEnv(key, def string) string {
    if v := strings.TrimSpace(os.Getenv(key)); v != "" {
        return v
    }
    return def
}
```

**Rules:**
- The primary enable/disable flag is the secret key env var. Return `nil, nil` when it is absent.
- Fail fast with a descriptive error on any required-but-missing secondary var (e.g., webhook secret).
- Strip trailing slashes from base URL fields.
- Normalize country/currency codes to uppercase.
- Parse CSV allowlists into `map[string]struct{}` for O(1) lookup.

---

## Step 4 — Implement `adapter.PaymentProvider`

Create `{vendor}.go` and implement every method of the `adapter.PaymentProvider` interface defined in `internal/adapter/provider.go`.

### 4a — `Info()`

Return the stable provider ID and display name from config:

```go
func (a *Adapter) Info() adapter.ProviderInfo {
    return adapter.ProviderInfo{
        ID:          a.cfg.ProviderID,
        DisplayName: a.cfg.DisplayName,
    }
}
```

### 4b — `ValidatePaymentInit(req)`

Validate provider-specific constraints before a transaction row is created. Common checks:

- Currency is in the allowed currencies set.
- Country/region is in the allowed countries set.
- Amount is positive.

Return a descriptive error if validation fails. Return `nil` on success.

### 4c — `CreatePaymentIntent(ctx, req)`

1. Call the provider API to create a payment session or charge.
2. Return `*adapter.PaymentIntent` with:
   - `ProviderTransactionID` — the provider's stable ID for this payment (used for all later calls).
   - `PaymentURL` — the hosted checkout URL the player opens, **or**
   - `QRCodeData` — the QR string for QRIS-style flows.
   - `ExpiresAt` — when the session expires (optional but recommended).

### 4d — `GetPaymentStatus(ctx, providerTxID)`

Query the provider for current payment status. If the provider has no direct status query API, return `adapter.ErrNotSupported`:

```go
func (a *Adapter) GetPaymentStatus(ctx context.Context, providerTxID string) (*adapter.ProviderPaymentStatus, error) {
    return nil, adapter.ErrNotSupported
}
```

### 4e — `SyncTransactionStatus(ctx, tx)`

Used by the background scheduler and manual reconciliation. Query the provider for all signals that determine current payment and refund state, then return a `*adapter.ProviderSyncResult` with:

- `PaymentStatus` — one of `SyncPaymentStatusPaid`, `SyncPaymentStatusFailed`, `SyncPaymentStatusPending`, `SyncPaymentStatusUnknown`, `SyncPaymentStatusUnsupported`.
- `RefundStatus` — one of `SyncRefundStatusNone`, `SyncRefundStatusPending`, `SyncRefundStatusPartialRefunded`, `SyncRefundStatusRefunded`, `SyncRefundStatusFailed`, `SyncRefundStatusUnknown`, `SyncRefundStatusUnsupported`.

Both fields must always be non-empty.

### 4f — `ValidateWebhookSignature(ctx, rawBody, headers)`

Verify the raw body and headers came from the legitimate provider **before any state mutation**. Common approaches:

- **HMAC-SHA256**: compute `HMAC-SHA256(secret, rawBody)` and compare with `headers["x-signature"]` using `crypto/subtle.ConstantTimeCompare`.
- **Static token**: compare `headers["x-callback-token"]` with the configured token using `subtle.ConstantTimeCompare`.

Return a non-nil error if validation fails. The webhook handler will reject the request before calling `HandleWebhook`.

```go
import "crypto/subtle"

func (a *Adapter) ValidateWebhookSignature(_ context.Context, rawBody []byte, headers map[string]string) error {
    sig := headers["x-your-signature-header"]
    expected := computeHMAC(a.cfg.WebhookSecret, rawBody)
    if subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) != 1 {
        return errors.New("webhook signature mismatch")
    }
    return nil
}
```

### 4g — `HandleWebhook(ctx, rawBody, headers)`

Parse the validated webhook body and return `*adapter.PaymentResult`. The result must always include:

- `InternalOrderID` — extracted from the webhook payload (look for `metadata.internalOrderId` or an external order field).
- `Status` — one of the `adapter.PaymentStatus*` constants.
- `ProviderTransactionID` — the provider's transaction ID.

### 4h — `CancelPayment(ctx, tx, reason)`

Cancel a pending payment at the provider. Map the provider's response to a `*adapter.CancelResult` with one of the `CancelStatus*` constants:

| Provider response | `CancelResult.Status` |
|---|---|
| Successfully canceled | `CancelStatusCanceled` |
| Already expired | `CancelStatusExpired` |
| Already paid | `CancelStatusAlreadyPaid` |
| Still processing | `CancelStatusPending` |
| Provider error | `CancelStatusFailed` |
| Not implemented | `CancelStatusUnsupported` |

### 4i — `RefundPayment(ctx, internalOrderID, providerTxID, amount, currencyCode)`

Initiate a refund at the provider. Use an idempotency key (e.g., `refund-{internalOrderID}`) if the provider supports it. Return `nil` on success, an error on failure.

**Note:** If the provider requires a different ID for refunds than the stored `provider_tx_id` (e.g., a payment ID rather than a session ID), resolve it first by querying the provider.

### 4j — `ValidateCredentials(ctx)`

Perform a lightweight API call to verify that the configured credentials are valid. This is called by the certification test. A minimal read-only call (e.g., list one recent payment) is sufficient.

---

## Step 5 — Wire Config into `internal/config/config.go`

1. Import your new adapter package.
2. Add a field to the top-level `Config` struct:

```go
import {vendor} "github.com/accelbyte/extend-regional-payment-gateway/internal/adapter/{vendor}"

type Config struct {
    // ... existing fields
    {Vendor}Config *{vendor}.Config
}
```

3. Call `Load()` in the `config.Load()` function:

```go
vendorCfg, err := {vendor}.Load()
if err != nil {
    return nil, fmt.Errorf("loading {Vendor} config: %w", err)
}
cfg.{Vendor}Config = vendorCfg
```

---

## Step 6 — Register the Adapter in `main.go`

Add a conditional registration block after the existing adapter registrations:

```go
if cfg.{Vendor}Config != nil {
    {vendor}Adapter, adapterErr := {vendor}.New(cfg.{Vendor}Config)
    if adapterErr != nil {
        slog.Error("failed to create {Vendor} adapter", "error", adapterErr)
        os.Exit(1)
    }
    registry.Register({vendor}Adapter)
    slog.Info("registered {Vendor} adapter", "provider_id", {vendor}Adapter.Info().ID)
}
```

When the required env vars are absent, `cfg.{Vendor}Config` is `nil` and registration is silently skipped.

---

## Step 7 — Write Unit Tests in `{vendor}_test.go`

Use table-driven tests. Cover at minimum:

| Area | What to test |
|---|---|
| Config | Defaults, required-var error, CSV parsing |
| `ValidatePaymentInit` | Valid request passes, unsupported currency/country fails |
| `CreatePaymentIntent` | Correct request shape, response mapping |
| `ValidateWebhookSignature` | Valid signature passes, tampered body fails |
| `HandleWebhook` | Each status event maps to the correct `PaymentStatus` |
| `CancelPayment` | Each provider response maps to the correct `CancelStatus` |
| `RefundPayment` | Correct refund request shape |
| `SyncTransactionStatus` | Paid, failed, refunded, partial refund cases |

Use `httptest.NewServer` or a fake HTTP client to avoid real network calls.

Also call the shared contract test from `internal/adapter/adaptertest`:

```go
func TestContract(t *testing.T) {
    adaptertest.RunContract(t, adaptertest.Harness{
        Provider:       newTestAdapter(t),
        ValidInit:      validInitRequest(),
        InvalidInit:    &invalidInitRequest(),
        WebhookBody:    capturedWebhookBody,
        WebhookHeaders: capturedWebhookHeaders,
    })
}
```

`RunContract` verifies that `Info()`, `ValidatePaymentInit`, `ValidateWebhookSignature`, `HandleWebhook`, `GetPaymentStatus`, `SyncTransactionStatus`, and `CancelPayment` all return structurally valid results.

---

## Step 8 — Write a Certification Test in `{vendor}_certification_test.go`

The certification test runs against the real sandbox API and is gated by a build tag so it never runs in CI.

```go
//go:build {vendor}_cert

package {vendor}_test

import (
    "context"
    "os"
    "testing"

    {vendor} "github.com/accelbyte/extend-regional-payment-gateway/internal/adapter/{vendor}"
)

func Test{Vendor}CertificationCredentials(t *testing.T) {
    cfg, err := {vendor}.Load()
    if err != nil || cfg == nil {
        t.Skip("{VENDOR}_SECRET_KEY not set")
    }
    a, err := {vendor}.New(cfg)
    if err != nil {
        t.Fatal(err)
    }
    if err := a.ValidateCredentials(context.Background()); err != nil {
        t.Fatalf("ValidateCredentials failed: %v", err)
    }
}
```

Run it with:

```bash
go test -tags={vendor}_cert ./internal/adapter/{vendor} -count=1
```

---

## Step 9 — Write a Provider Guide in `docs/adapter/`

Create `docs/adapter/{Vendor}Guide.md` with these sections:

1. **Overview** — provider ID, how it works in one paragraph.
2. **Dashboard Setup** — numbered steps to configure the provider dashboard (API keys, webhook URL, events to enable).
3. **Configuration** — env var table: Variable / Required / Default / Description / Example.
4. **Flow** — payment, cancellation, refund, sync flows as text diagrams.
5. **Status Mapping** — provider status → app status table.
6. **Testing** — unit test and certification test commands.

---

## Step 10 — Verify End to End

```bash
# 1. Unit tests pass
go test ./internal/adapter/{vendor} -count=1 -v

# 2. Full test suite passes
go test ./... -count=1

# 3. Server starts and registers the adapter
{VENDOR}_SECRET_KEY=... go run . 2>&1 | grep "registered {Vendor} adapter"

# 4. Certification test passes against sandbox
go test -tags={vendor}_cert ./internal/adapter/{vendor} -count=1 -v
```

---

## Provider ID Convention

| Scenario | Provider ID |
|---|---|
| Single integration | `provider_{vendor}` |
| Multi-region instance | `provider_{vendor}_{region}` |
| Sandbox/test instance | `provider_{vendor}_sandbox` |

The provider ID is the URL segment that routes webhooks:

```text
POST /payment/v1/webhook/{providerID}
```

It is also the key used in API requests:

```json
{ "providerId": "provider_{vendor}" }
```

Keep it stable. Changing it after launch orphans existing transactions.
