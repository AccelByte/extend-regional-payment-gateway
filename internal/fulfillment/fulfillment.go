package fulfillment

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	entitlementclient "github.com/AccelByte/accelbyte-go-sdk/platform-sdk/pkg/platformclient/entitlement"
	fulfillmentclient "github.com/AccelByte/accelbyte-go-sdk/platform-sdk/pkg/platformclient/fulfillment"
	walletclient "github.com/AccelByte/accelbyte-go-sdk/platform-sdk/pkg/platformclient/wallet"
	"github.com/AccelByte/accelbyte-go-sdk/platform-sdk/pkg/platformclientmodels"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
)

type fulfillmentService interface {
	FulfillItemShort(*fulfillmentclient.FulfillItemParams) (*platformclientmodels.FulfillmentResult, error)
	QueryFulfillmentHistoriesShort(*fulfillmentclient.QueryFulfillmentHistoriesParams) (*platformclientmodels.FulfillmentHistoryPagingSlicedResult, error)
}

type walletService interface {
	DebitUserWalletByCurrencyCodeShort(*walletclient.DebitUserWalletByCurrencyCodeParams) (*platformclientmodels.WalletInfo, error)
}

type entitlementService interface {
	RevokeUserEntitlementsShort(*entitlementclient.RevokeUserEntitlementsParams) (*platformclientmodels.BulkOperationResult, error)
}

// Fulfiller wraps AGS fulfillment and entitlement SDK calls.
type Fulfiller struct {
	fulfillmentService fulfillmentService
	walletService      walletService
	entitlementService entitlementService
	namespace          string
}

func NewFulfiller(
	fulfillmentSvc fulfillmentService,
	walletSvc walletService,
	entitlementSvc entitlementService,
	namespace string,
) *Fulfiller {
	return &Fulfiller{
		fulfillmentService: fulfillmentSvc,
		walletService:      walletSvc,
		entitlementService: entitlementSvc,
		namespace:          namespace,
	}
}

// FulfillUserItem grants an AGS store item to a user.
// The orderNo parameter (= transaction UUID) makes the call idempotent in AGS.
func (f *Fulfiller) FulfillUserItem(ctx context.Context, userID, itemID, orderNo string, qty int32) error {
	input := &fulfillmentclient.FulfillItemParams{
		Namespace: f.namespace,
		UserID:    userID,
		Body: &platformclientmodels.FulfillmentRequest{
			ItemID:   itemID,
			Quantity: &qty,
			OrderNo:  orderNo,
			Source:   platformclientmodels.FulfillmentRequestSourcePURCHASE,
		},
		HTTPClient: &http.Client{},
	}

	resp, err := f.fulfillmentService.FulfillItemShort(input)
	if err != nil {
		slog.Error("FulfillItemShort failed", "namespace", f.namespace, "user_id", userID, "item_id", itemID, "order_no", orderNo, "error", err)
		return fmt.Errorf("FulfillUserItem: %w", err)
	}

	if resp == nil {
		return fmt.Errorf("FulfillUserItem returned nil response")
	}

	return nil
}

// ReverseFulfillment reverts the AGS grants produced by the transaction's
// fulfillment history. The transaction ID is the AGS orderNo used on grant.
func (f *Fulfiller) ReverseFulfillment(ctx context.Context, tx *model.Transaction) error {
	if tx == nil {
		return fmt.Errorf("ReverseFulfillment: nil transaction")
	}
	if f.fulfillmentService == nil || f.walletService == nil || f.entitlementService == nil {
		return fmt.Errorf("ReverseFulfillment: AGS reversal services are not configured")
	}

	history, err := f.findSuccessfulFulfillmentHistory(ctx, tx)
	if err != nil {
		return err
	}

	for _, credit := range history.CreditSummaries {
		if credit == nil || credit.Amount == nil || *credit.Amount == 0 {
			continue
		}
		currencyCode := strings.TrimSpace(credit.CurrencyCode)
		if currencyCode == "" {
			return fmt.Errorf("ReverseFulfillment: credit summary for order %s has empty currency code", tx.ID)
		}
		input := &walletclient.DebitUserWalletByCurrencyCodeParams{
			Namespace:    f.namespace,
			UserID:       tx.UserID,
			CurrencyCode: currencyCode,
			Body: &platformclientmodels.DebitByCurrencyCodeRequest{
				Amount:         credit.Amount,
				AllowOverdraft: false,
				BalanceSource:  platformclientmodels.DebitByCurrencyCodeRequestBalanceSourceORDERREVOCATION,
				Reason:         "payment refund " + tx.ID,
				Metadata: map[string]string{
					"transaction_id": tx.ID,
					"item_id":        tx.ItemID,
				},
			},
			HTTPClient: &http.Client{},
		}
		if _, debitErr := f.walletService.DebitUserWalletByCurrencyCodeShort(input); debitErr != nil {
			return fmt.Errorf("ReverseFulfillment: debit wallet currency %s amount %d: %w", currencyCode, *credit.Amount, debitErr)
		}
	}

	var entitlementIDs []string
	for _, entitlement := range history.EntitlementSummaries {
		if entitlement == nil || entitlement.ID == nil || strings.TrimSpace(*entitlement.ID) == "" {
			continue
		}
		entitlementIDs = append(entitlementIDs, strings.TrimSpace(*entitlement.ID))
	}
	if len(entitlementIDs) > 0 {
		input := &entitlementclient.RevokeUserEntitlementsParams{
			Namespace:      f.namespace,
			UserID:         tx.UserID,
			EntitlementIds: strings.Join(entitlementIDs, ","),
			HTTPClient:     &http.Client{},
		}
		if _, revokeErr := f.entitlementService.RevokeUserEntitlementsShort(input); revokeErr != nil {
			return fmt.Errorf("ReverseFulfillment: revoke entitlements %s: %w", strings.Join(entitlementIDs, ","), revokeErr)
		}
	}

	slog.Info("AGS fulfillment reversed", "txn_id", tx.ID, "wallet_credits", len(history.CreditSummaries), "entitlements", len(entitlementIDs))
	return nil
}

func (f *Fulfiller) findSuccessfulFulfillmentHistory(ctx context.Context, tx *model.Transaction) (*platformclientmodels.FulfillmentHistoryInfo, error) {
	const limit int32 = 100
	status := platformclientmodels.FulfillmentHistoryInfoStatusSUCCESS
	userID := tx.UserID

	for offset := int32(0); ; offset += limit {
		input := &fulfillmentclient.QueryFulfillmentHistoriesParams{
			Namespace:  f.namespace,
			Limit:      int32Ptr(limit),
			Offset:     int32Ptr(offset),
			Status:     &status,
			UserID:     &userID,
			HTTPClient: &http.Client{},
			Context:    ctx,
		}
		result, err := f.fulfillmentService.QueryFulfillmentHistoriesShort(input)
		if err != nil {
			return nil, fmt.Errorf("ReverseFulfillment: query fulfillment history: %w", err)
		}
		if result == nil {
			return nil, fmt.Errorf("ReverseFulfillment: query fulfillment history returned nil")
		}
		for _, history := range result.Data {
			if history != nil && history.OrderNo == tx.ID {
				return history, nil
			}
		}
		if int32(len(result.Data)) < limit {
			break
		}
	}

	return nil, fmt.Errorf("ReverseFulfillment: successful fulfillment history not found for order %s", tx.ID)
}

func int32Ptr(v int32) *int32 {
	return &v
}

// Notifier sends a fire-and-forget Lobby push notification after payment completes.
type Notifier struct {
	namespace string
}

func NewNotifier(namespace string) *Notifier {
	return &Notifier{namespace: namespace}
}

// NotifyPaymentResult pushes a payment result notification to the player's Lobby WebSocket.
// This is best-effort — errors are logged but not propagated.
func (n *Notifier) NotifyPaymentResult(ctx context.Context, userID, txnID, status, itemID string) {
	// Executed in a goroutine so it never blocks the caller.
	go func() {
		slog.Info("payment notification sent",
			"user_id", userID,
			"transaction_id", txnID,
			"status", status,
			"item_id", itemID,
		)
		// TODO: wire AGS Lobby FreeFormNotification when lobby SDK is integrated.
		// lobbyClient.FreeFormNotification(userID, namespace, &lobbyclientmodels.ModelFreeFormNotificationRequest{
		//   Topic:   "payment.result",
		//   Message: fmt.Sprintf(`{"type":"payment.result","transaction_id":"%s","status":"%s","item_id":"%s"}`, txnID, status, itemID),
		// })
	}()
}
