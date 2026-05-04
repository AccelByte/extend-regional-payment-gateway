package checkout

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	pb "github.com/accelbyte/extend-regional-payment-gateway/pkg/pb"
)

// ExistingTransactionPaymentCreator is the subset of PaymentService used by this handler.
type ExistingTransactionPaymentCreator interface {
	CreatePaymentForExistingTransaction(ctx context.Context, transactionID string, providerID string, description string) (*pb.CreatePaymentIntentResponse, error)
	CancelPaymentForExistingTransaction(ctx context.Context, transactionID string, reason string) (*pb.CancelTransactionResponse, error)
	CancelSelectedProviderForExistingTransaction(ctx context.Context, transactionID string, reason string) (*pb.CancelTransactionResponse, error)
	GetTransaction(ctx context.Context, req *pb.GetTransactionRequest) (*pb.TransactionResponse, error)
}

// Handler serves the provider selection page and processes the player's choice.
type Handler struct {
	store      *Store
	registry   *adapter.Registry
	paymentSvc ExistingTransactionPaymentCreator
	basePath   string // e.g. "/payment"
}

func NewHandler(store *Store, registry *adapter.Registry, paymentSvc ExistingTransactionPaymentCreator, basePath string) *Handler {
	return &Handler{
		store:      store,
		registry:   registry,
		paymentSvc: paymentSvc,
		basePath:   basePath,
	}
}

var pageTemplate = template.Must(template.New("checkout").Parse(`<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Checkout</title>
  <style>
    *, *::before, *::after { box-sizing: border-box; }
    :root {
      --ab-blue: #006DFF;
      --ab-blue-dark: #003B8F;
      --ab-navy: #071A3A;
      --ab-text: #0F274A;
      --ab-muted: #5F7190;
      --ab-line: #D8E6F7;
      --ab-soft: #EEF6FF;
      --ab-white: #FFFFFF;
    }
    body {
      min-height: 100vh;
      margin: 0;
      font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      color: var(--ab-text);
      background:
        radial-gradient(circle at top left, rgba(0, 109, 255, .16), transparent 34rem),
        linear-gradient(135deg, #F8FBFF 0%, #EEF6FF 100%);
    }
    .page {
      min-height: 100vh;
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 32px 18px;
    }
    .checkout-shell {
      display: grid;
      grid-template-columns: minmax(280px, 430px) minmax(320px, 480px);
      width: min(940px, 100%);
      overflow: hidden;
      border: 1px solid rgba(0, 109, 255, .12);
      border-radius: 8px;
      background: var(--ab-white);
      box-shadow: 0 24px 70px rgba(7, 26, 58, .14);
    }
    .details-panel {
      display: flex;
      min-height: 560px;
      flex-direction: column;
      justify-content: space-between;
      padding: 32px;
      color: var(--ab-white);
      background: linear-gradient(160deg, var(--ab-navy) 0%, var(--ab-blue-dark) 58%, var(--ab-blue) 100%);
    }
    .eyebrow {
      margin: 0 0 8px;
      color: #BFD9FF;
      font-size: 12px;
      font-weight: 700;
      letter-spacing: 0;
      text-transform: uppercase;
    }
    h1, h2, p { margin-top: 0; }
    h1 {
      margin-bottom: 22px;
      color: var(--ab-white);
      font-size: 26px;
      line-height: 1.2;
    }
    .summary-card {
      border: 1px solid rgba(255, 255, 255, .22);
      border-radius: 8px;
      background: rgba(255, 255, 255, .08);
      overflow: hidden;
    }
    .line-item {
      display: grid;
      grid-template-columns: 1fr auto;
      gap: 16px;
      padding: 18px;
      border-bottom: 1px solid rgba(255, 255, 255, .18);
    }
    .item-name {
      margin: 0 0 6px;
      font-size: 18px;
      font-weight: 750;
      line-height: 1.25;
      overflow-wrap: anywhere;
    }
    .item-id, .muted {
      color: #D8E8FF;
      font-size: 13px;
      line-height: 1.45;
      overflow-wrap: anywhere;
    }
    .item-price {
      color: var(--ab-white);
      font-size: 17px;
      font-weight: 750;
      text-align: right;
      white-space: nowrap;
    }
    .detail-row, .total-row {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 18px;
      padding: 14px 18px;
      color: #D8E8FF;
      font-size: 14px;
    }
    .detail-row strong, .total-row strong {
      color: var(--ab-white);
      font-size: 15px;
      text-align: right;
    }
    .total-row {
      border-top: 1px solid rgba(255, 255, 255, .22);
      color: var(--ab-white);
    }
    .total-row strong {
      font-size: 26px;
      line-height: 1.15;
    }
    .order-meta {
      margin-top: 24px;
      color: #C6DDFF;
      font-size: 12px;
      line-height: 1.6;
      overflow-wrap: anywhere;
    }
    .payment-panel {
      padding: 32px;
      background: var(--ab-white);
    }
    h2 {
      margin-bottom: 8px;
      color: var(--ab-navy);
      font-size: 24px;
      line-height: 1.25;
    }
    .intro {
      margin-bottom: 26px;
      color: var(--ab-muted);
      font-size: 14px;
      line-height: 1.5;
    }
    .section-title {
      margin: 0 0 12px;
      color: var(--ab-navy);
      font-size: 13px;
      font-weight: 800;
      text-transform: uppercase;
    }
    .methods {
      display: flex;
      flex-direction: column;
      gap: 12px;
    }
    form { margin: 0; }
    button {
      display: flex;
      width: 100%;
      min-height: 64px;
      align-items: center;
      justify-content: space-between;
      gap: 16px;
      padding: 14px 16px;
      border: 1px solid var(--ab-line);
      border-radius: 8px;
      background: var(--ab-white);
      color: var(--ab-text);
      cursor: pointer;
      font: inherit;
      font-size: 16px;
      font-weight: 750;
      text-align: left;
      transition: border-color .15s, box-shadow .15s, background .15s, color .15s;
    }
    button:hover, button:focus-visible {
      border-color: var(--ab-blue);
      background: var(--ab-soft);
      color: var(--ab-blue-dark);
      box-shadow: 0 0 0 3px rgba(0, 109, 255, .14);
      outline: none;
    }
    .provider-mark {
      display: inline-flex;
      width: 38px;
      height: 38px;
      flex: 0 0 auto;
      align-items: center;
      justify-content: center;
      border-radius: 8px;
      background: var(--ab-blue);
      color: var(--ab-white);
      font-size: 14px;
      font-weight: 850;
    }
    .provider-copy {
      display: flex;
      min-width: 0;
      flex: 1;
      flex-direction: column;
      gap: 2px;
    }
    .provider-name {
      overflow-wrap: anywhere;
      text-transform: uppercase;
    }
    .provider-sub {
      color: var(--ab-muted);
      font-size: 12px;
      font-weight: 600;
    }
    .arrow {
      color: var(--ab-blue);
      font-size: 24px;
      line-height: 1;
    }
    .empty-state {
      padding: 18px;
      border: 1px dashed var(--ab-line);
      border-radius: 8px;
      color: var(--ab-muted);
      background: #F8FBFF;
      font-size: 14px;
    }
    .cancel-form { margin-top: 18px; }
    .cancel-button {
      min-height: 48px;
      border-color: #F3B4B4;
      color: #9F1D1D;
      justify-content: center;
    }
    .status-panel {
      padding: 18px;
      border: 1px solid var(--ab-line);
      border-radius: 8px;
      background: #F8FBFF;
      color: var(--ab-muted);
      line-height: 1.5;
    }
    @media (max-width: 760px) {
      .page { align-items: stretch; padding: 0; }
      .checkout-shell {
        display: block;
        min-height: 100vh;
        grid-template-columns: 1fr;
        border: 0;
        border-radius: 0;
        box-shadow: none;
      }
      .details-panel {
        min-height: auto;
        padding: 26px 20px;
      }
      .payment-panel { padding: 26px 20px 32px; }
      .line-item { grid-template-columns: 1fr; }
      .item-price { text-align: left; }
      .total-row strong { font-size: 23px; }
    }
  </style>
</head>
<body>
  <main class="page">
    <div class="checkout-shell">
      <section class="details-panel" aria-labelledby="checkout-title">
        <div>
          <p class="eyebrow">AccelByte Checkout</p>
          <h1 id="checkout-title">Complete Your Purchase</h1>
          <div class="summary-card">
            <div class="line-item">
              <div>
                <p class="item-name">{{.ItemName}}</p>
                <div class="item-id">Item ID: {{.ItemID}}</div>
              </div>
              <div class="item-price">{{.UnitPrice}}</div>
            </div>
            <div class="detail-row">
              <span>Quantity</span>
              <strong>{{.Quantity}}</strong>
            </div>
            <div class="detail-row">
              <span>Subtotal</span>
              <strong>{{.TotalPrice}}</strong>
            </div>
            <div class="total-row">
              <span>Total</span>
              <strong>{{.TotalPrice}}</strong>
            </div>
          </div>
        </div>
        <div class="order-meta">
          <div>Order ID: {{.TransactionID}}</div>
          <div>Session expires: {{.ExpiresAt}}</div>
        </div>
      </section>

      <section class="payment-panel" aria-labelledby="payment-title">
        <h2 id="payment-title">Choose payment method</h2>
        {{if .Terminal}}
        <div class="status-panel">
          <strong>{{.StatusTitle}}</strong>
          <div>{{.StatusMessage}}</div>
        </div>
        {{else if .ProviderSelected}}
        <div class="status-panel">
          <strong>Payment method selected</strong>
          <div>{{.SelectedProviderName}} is waiting for payment confirmation.</div>
        </div>
        {{if .PaymentURL}}
        <form class="cancel-form" method="GET" action="{{.PaymentURL}}">
          <button type="submit" aria-label="Continue payment">Continue payment</button>
        </form>
        {{end}}
        <form class="cancel-form" method="POST" action="{{.CancelSelectedProviderURL}}">
          <button class="cancel-button" type="submit" aria-label="Cancel selected payment method">Cancel selected method</button>
        </form>
        {{else}}
        <p class="intro">Select a payment option to continue securely with the provider.</p>
        <p class="section-title">Payment options</p>
        <div class="methods">
          {{if .Providers}}
            {{range .Providers}}
            <form method="POST" action="{{$.SelectURL}}">
              <input type="hidden" name="provider" value="{{.Key}}">
              <button type="submit" aria-label="Pay with {{.DisplayName}}">
                <span class="provider-mark">{{.Initials}}</span>
                <span class="provider-copy">
                  <span class="provider-name">{{.DisplayName}}</span>
                  <span class="provider-sub">Continue to payment</span>
                </span>
                <span class="arrow" aria-hidden="true">&rsaquo;</span>
              </button>
            </form>
            {{end}}
          {{else}}
            <div class="empty-state">No payment methods are available right now.</div>
          {{end}}
        </div>
        <form class="cancel-form" method="POST" action="{{.CancelURL}}">
          <button class="cancel-button" type="submit" aria-label="Cancel payment">Cancel payment</button>
        </form>
        {{end}}
      </section>
    </div>
  </main>
</body>
</html>`))

type providerItem struct {
	Key         string
	DisplayName string
	Initials    string
}

type pageData struct {
	Providers                 []providerItem
	SelectURL                 string
	TransactionID             string
	ItemName                  string
	ItemID                    string
	Quantity                  int32
	UnitPrice                 string
	TotalPrice                string
	ExpiresAt                 string
	CancelURL                 string
	Terminal                  bool
	StatusTitle               string
	StatusMessage             string
	ProviderSelected          bool
	SelectedProviderName      string
	PaymentURL                string
	CancelSelectedProviderURL string
}

// HandleCheckoutPage serves GET /payment/checkout/{sessionId}
func (h *Handler) HandleCheckoutPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessionID := strings.TrimPrefix(r.URL.Path, h.basePath+"/checkout/")
	sessionID = strings.TrimSuffix(sessionID, "/")
	if sessionID == "" {
		http.Error(w, "missing sessionId", http.StatusBadRequest)
		return
	}

	sess, ok := h.store.Get(sessionID)
	if !ok {
		http.Error(w, "checkout session not found or expired", http.StatusNotFound)
		return
	}
	tx, _ := h.paymentSvc.GetTransaction(r.Context(), &pb.GetTransactionRequest{TransactionId: sess.TransactionID})

	infos := h.registry.Infos()
	sort.Slice(infos, func(i, j int) bool { return infos[i].DisplayName < infos[j].DisplayName })
	providers := make([]providerItem, 0, len(infos))
	for _, info := range infos {
		display := fallback(info.DisplayName, displayName(info.ID))
		providers = append(providers, providerItem{
			Key:         info.ID,
			DisplayName: display,
			Initials:    initials(display),
		})
	}

	selectURL := fmt.Sprintf("%s/checkout/%s/select", h.basePath, sessionID)
	cancelURL := fmt.Sprintf("%s/checkout/%s/cancel", h.basePath, sessionID)
	cancelSelectedProviderURL := fmt.Sprintf("%s/checkout/%s/cancel-selected-provider", h.basePath, sessionID)
	terminal, statusTitle, statusMessage := checkoutStatusCopy(tx)
	providerSelected := tx != nil && tx.Status == pb.TransactionStatus_PENDING && strings.TrimSpace(tx.ProviderTxId) != ""
	selectedProviderName := ""
	paymentURL := ""
	if providerSelected {
		selectedProviderName = transactionProviderDisplayName(tx)
		paymentURL = tx.PaymentUrl
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	pageTemplate.Execute(w, pageData{ //nolint:errcheck
		Providers:                 providers,
		SelectURL:                 selectURL,
		TransactionID:             sess.TransactionID,
		ItemName:                  fallback(sess.ItemName, sess.ItemID),
		ItemID:                    sess.ItemID,
		Quantity:                  sess.Quantity,
		UnitPrice:                 formatCurrencyAmount(sess.UnitPrice, sess.CurrencyCode),
		TotalPrice:                formatCurrencyAmount(sess.TotalPrice, sess.CurrencyCode),
		ExpiresAt:                 sess.ExpiresAt.Local().Format("02 Jan 2006, 15:04 MST"),
		CancelURL:                 cancelURL,
		Terminal:                  terminal,
		StatusTitle:               statusTitle,
		StatusMessage:             statusMessage,
		ProviderSelected:          providerSelected,
		SelectedProviderName:      selectedProviderName,
		PaymentURL:                paymentURL,
		CancelSelectedProviderURL: cancelSelectedProviderURL,
	})
}

// HandleProviderSelect serves POST /payment/checkout/{sessionId}/select
func (h *Handler) HandleProviderSelect(w http.ResponseWriter, r *http.Request) {
	// Browser back/refresh may revisit the form action. Make it friendly.
	if r.Method == http.MethodGet {
		trimmed := strings.TrimPrefix(r.URL.Path, h.basePath+"/checkout/")
		trimmed = strings.TrimSuffix(trimmed, "/select")
		if trimmed == "" {
			http.Error(w, "missing sessionId", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("%s/checkout/%s", h.basePath, trimmed), http.StatusFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Path: /payment/checkout/{sessionId}/select
	trimmed := strings.TrimPrefix(r.URL.Path, h.basePath+"/checkout/")
	trimmed = strings.TrimSuffix(trimmed, "/select")
	sessionID := trimmed
	if sessionID == "" {
		http.Error(w, "missing sessionId", http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	providerKey := r.FormValue("provider")
	if providerKey == "" {
		http.Error(w, "provider is required", http.StatusBadRequest)
		return
	}

	sess, ok := h.store.GetValidForSelection(sessionID)
	if !ok {
		http.Error(w, "checkout session not found or expired", http.StatusNotFound)
		return
	}

	// Inject userID into gRPC metadata so CreatePaymentIntent can read it.
	ctx := metadata.NewIncomingContext(r.Context(), metadata.Pairs("x-auth-user-id", sess.UserID))

	resp, err := h.paymentSvc.CreatePaymentForExistingTransaction(ctx, sess.TransactionID, providerKey, sess.Description)
	if err != nil {
		if strings.Contains(err.Error(), "payment provider already selected") {
			http.Redirect(w, r, fmt.Sprintf("%s/checkout/%s", h.basePath, sessionID), http.StatusFound)
			return
		}
		renderError(w, "Payment provider error: "+err.Error())
		return
	}

	http.Redirect(w, r, resp.PaymentUrl, http.StatusFound)
}

func (h *Handler) HandleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, h.basePath+"/checkout/")
	trimmed = strings.TrimSuffix(trimmed, "/cancel")
	sessionID := trimmed
	if sessionID == "" {
		http.Error(w, "missing sessionId", http.StatusBadRequest)
		return
	}
	sess, ok := h.store.Get(sessionID)
	if !ok {
		http.Error(w, "checkout session not found or expired", http.StatusNotFound)
		return
	}
	ctx := metadata.NewIncomingContext(r.Context(), metadata.Pairs("x-auth-user-id", sess.UserID))
	if _, err := h.paymentSvc.CancelPaymentForExistingTransaction(ctx, sess.TransactionID, "user canceled checkout"); err != nil {
		renderError(w, "Cancel failed: "+err.Error())
		return
	}
	http.Redirect(w, r, fmt.Sprintf("%s/checkout/%s", h.basePath, sessionID), http.StatusFound)
}

func (h *Handler) HandleCancelSelectedProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, h.basePath+"/checkout/")
	trimmed = strings.TrimSuffix(trimmed, "/cancel-selected-provider")
	sessionID := trimmed
	if sessionID == "" {
		http.Error(w, "missing sessionId", http.StatusBadRequest)
		return
	}
	sess, ok := h.store.Get(sessionID)
	if !ok {
		http.Error(w, "checkout session not found or expired", http.StatusNotFound)
		return
	}
	ctx := metadata.NewIncomingContext(r.Context(), metadata.Pairs("x-auth-user-id", sess.UserID))
	if _, err := h.paymentSvc.CancelSelectedProviderForExistingTransaction(ctx, sess.TransactionID, "user canceled selected provider"); err != nil {
		renderError(w, "Cancel selected method failed: "+err.Error())
		return
	}
	http.Redirect(w, r, fmt.Sprintf("%s/checkout/%s", h.basePath, sessionID), http.StatusFound)
}

// displayName converts a registry key to a human-readable label.
func displayName(key string) string {
	name := strings.TrimPrefix(key, "provider_")
	// Title-case each word separated by underscores or hyphens.
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '_' || r == '-' })
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

func transactionProviderDisplayName(tx *pb.TransactionResponse) string {
	if tx == nil {
		return "The selected payment method"
	}
	if strings.TrimSpace(tx.ProviderDisplayName) != "" {
		return tx.ProviderDisplayName
	}
	if strings.TrimSpace(tx.ProviderId) == "" {
		return "The selected payment method"
	}
	return displayName(tx.ProviderId)
}

func initials(name string) string {
	parts := strings.Fields(name)
	if len(parts) == 0 {
		return "P"
	}
	if len(parts) == 1 {
		return strings.ToUpper(string([]rune(parts[0])[0]))
	}
	return strings.ToUpper(string([]rune(parts[0])[0]) + string([]rune(parts[1])[0]))
}

func fallback(value string, fallbackValue string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallbackValue
}

func formatCurrencyAmount(amount int64, currencyCode string) string {
	sign := ""
	if amount < 0 {
		sign = "-"
		amount = -amount
	}
	raw := fmt.Sprintf("%d", amount)
	for i := len(raw) - 3; i > 0; i -= 3 {
		raw = raw[:i] + "." + raw[i:]
	}
	currencyCode = strings.TrimSpace(strings.ToUpper(currencyCode))
	if currencyCode == "" {
		return sign + raw
	}
	return sign + raw + " " + currencyCode
}

func checkoutStatusCopy(tx *pb.TransactionResponse) (bool, string, string) {
	if tx == nil {
		return false, "", ""
	}
	switch tx.Status {
	case pb.TransactionStatus_CANCELED:
		return true, "Payment canceled", "This checkout was canceled. You can return to the game and start a new purchase."
	case pb.TransactionStatus_EXPIRED:
		return true, "Payment expired", "The payment window expired before the purchase was completed."
	case pb.TransactionStatus_FAILED:
		return true, "Payment failed", fallback(tx.FailureReason, "The payment provider could not complete this payment.")
	case pb.TransactionStatus_FULFILLED:
		return true, "Payment complete", "Your purchase has been completed."
	default:
		return false, "", ""
	}
}

func renderError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Error</title>
<style>body{font-family:sans-serif;display:flex;align-items:center;justify-content:center;
height:100vh;margin:0;background:#f8fafc;}.card{text-align:center;padding:2rem 2.5rem;
background:#fff;border-radius:1rem;box-shadow:0 2px 16px rgba(0,0,0,.08);max-width:400px;}
h1{color:#dc2626;}p{color:#64748b;}</style></head>
<body><div class="card"><h1>Payment Error</h1><p>%s</p>
<p><a href="javascript:history.back()">Go back</a></p></div></body></html>`, template.HTMLEscapeString(msg))
}

// CheckoutSessionExpiry is the lifetime of a checkout session.
const CheckoutSessionExpiry = 30 * time.Minute
