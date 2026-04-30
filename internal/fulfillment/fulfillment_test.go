package fulfillment

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	entitlementclient "github.com/AccelByte/accelbyte-go-sdk/platform-sdk/pkg/platformclient/entitlement"
	fulfillmentclient "github.com/AccelByte/accelbyte-go-sdk/platform-sdk/pkg/platformclient/fulfillment"
	walletclient "github.com/AccelByte/accelbyte-go-sdk/platform-sdk/pkg/platformclient/wallet"
	"github.com/AccelByte/accelbyte-go-sdk/platform-sdk/pkg/platformclientmodels"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
)

type mockFulfillmentService struct {
	history *platformclientmodels.FulfillmentHistoryPagingSlicedResult
	err     error
}

func (m *mockFulfillmentService) FulfillItemShort(*fulfillmentclient.FulfillItemParams) (*platformclientmodels.FulfillmentResult, error) {
	return nil, errors.New("not implemented")
}

func (m *mockFulfillmentService) QueryFulfillmentHistoriesShort(*fulfillmentclient.QueryFulfillmentHistoriesParams) (*platformclientmodels.FulfillmentHistoryPagingSlicedResult, error) {
	return m.history, m.err
}

type mockWalletService struct {
	debits []*walletclient.DebitUserWalletByCurrencyCodeParams
	err    error
}

func (m *mockWalletService) DebitUserWalletByCurrencyCodeShort(input *walletclient.DebitUserWalletByCurrencyCodeParams) (*platformclientmodels.WalletInfo, error) {
	m.debits = append(m.debits, input)
	return &platformclientmodels.WalletInfo{}, m.err
}

type mockEntitlementService struct {
	revokes []*entitlementclient.RevokeUserEntitlementsParams
	err     error
}

func (m *mockEntitlementService) RevokeUserEntitlementsShort(input *entitlementclient.RevokeUserEntitlementsParams) (*platformclientmodels.BulkOperationResult, error) {
	m.revokes = append(m.revokes, input)
	return &platformclientmodels.BulkOperationResult{Affected: 2}, m.err
}

func TestReverseFulfillment_MixedWalletCreditsAndEntitlements(t *testing.T) {
	amount := int64(500)
	entitlementID1 := "ent-1"
	entitlementID2 := "ent-2"
	fulfillmentSvc := &mockFulfillmentService{
		history: &platformclientmodels.FulfillmentHistoryPagingSlicedResult{
			Data: []*platformclientmodels.FulfillmentHistoryInfo{
				{OrderNo: "another-order"},
				{
					OrderNo: "txn-1",
					CreditSummaries: []*platformclientmodels.CreditSummary{
						{Amount: &amount, CurrencyCode: "COIN"},
					},
					EntitlementSummaries: []*platformclientmodels.EntitlementSummary{
						{ID: &entitlementID1},
						{ID: &entitlementID2},
					},
				},
			},
		},
	}
	walletSvc := &mockWalletService{}
	entitlementSvc := &mockEntitlementService{}
	reverser := NewFulfiller(fulfillmentSvc, walletSvc, entitlementSvc, "test-ns")

	err := reverser.ReverseFulfillment(context.Background(), &model.Transaction{
		ID:     "txn-1",
		UserID: "user-1",
		ItemID: "item-1",
	})

	require.NoError(t, err)
	require.Len(t, walletSvc.debits, 1)
	assert.Equal(t, "test-ns", walletSvc.debits[0].Namespace)
	assert.Equal(t, "user-1", walletSvc.debits[0].UserID)
	assert.Equal(t, "COIN", walletSvc.debits[0].CurrencyCode)
	require.NotNil(t, walletSvc.debits[0].Body)
	assert.Equal(t, amount, *walletSvc.debits[0].Body.Amount)
	assert.Equal(t, platformclientmodels.DebitByCurrencyCodeRequestBalanceSourceORDERREVOCATION, walletSvc.debits[0].Body.BalanceSource)

	require.Len(t, entitlementSvc.revokes, 1)
	assert.Equal(t, "test-ns", entitlementSvc.revokes[0].Namespace)
	assert.Equal(t, "user-1", entitlementSvc.revokes[0].UserID)
	assert.Equal(t, "ent-1,ent-2", entitlementSvc.revokes[0].EntitlementIds)
}
