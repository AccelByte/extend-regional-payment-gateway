package service

import (
	"testing"

	"github.com/AccelByte/accelbyte-go-sdk/platform-sdk/pkg/platformclientmodels"
)

func strPtr(value string) *string {
	return &value
}

func regionDataItem(price int32, currencyCode string, currencyType string) platformclientmodels.RegionDataItem {
	return platformclientmodels.RegionDataItem{
		Price:        price,
		CurrencyCode: strPtr(currencyCode),
		CurrencyType: strPtr(currencyType),
	}
}

func TestItemDetailsFromFullItemUsesRequestedRegionCurrency(t *testing.T) {
	item := &platformclientmodels.FullItemInfo{
		Name: strPtr("Crystal Pack"),
		RegionData: map[string][]platformclientmodels.RegionDataItem{
			"ID": {
				regionDataItem(1050000, "IDR", platformclientmodels.RegionDataItemCurrencyTypeREAL),
			},
			"US": {
				regionDataItem(499, "USD", platformclientmodels.RegionDataItemCurrencyTypeREAL),
			},
		},
	}

	details, err := itemDetailsFromFullItem("item-1", "us", item)
	if err != nil {
		t.Fatalf("itemDetailsFromFullItem returned error: %v", err)
	}
	if details.Name != "Crystal Pack" {
		t.Fatalf("Name = %q, want Crystal Pack", details.Name)
	}
	if details.RegionCode != "US" {
		t.Fatalf("RegionCode = %q, want US", details.RegionCode)
	}
	if details.UnitPrice != 4 {
		t.Fatalf("UnitPrice = %d, want 4", details.UnitPrice)
	}
	if details.CurrencyCode != "USD" {
		t.Fatalf("CurrencyCode = %q, want USD", details.CurrencyCode)
	}
}

func TestItemDetailsFromFullItemDefaultsRegionToID(t *testing.T) {
	item := &platformclientmodels.FullItemInfo{
		RegionData: map[string][]platformclientmodels.RegionDataItem{
			"ID": {
				regionDataItem(1050000, "IDR", platformclientmodels.RegionDataItemCurrencyTypeREAL),
			},
		},
	}

	details, err := itemDetailsFromFullItem("item-1", "", item)
	if err != nil {
		t.Fatalf("itemDetailsFromFullItem returned error: %v", err)
	}
	if details.RegionCode != "ID" {
		t.Fatalf("RegionCode = %q, want ID", details.RegionCode)
	}
	if details.CurrencyCode != "IDR" {
		t.Fatalf("CurrencyCode = %q, want IDR", details.CurrencyCode)
	}
	if details.UnitPrice != 10500 {
		t.Fatalf("UnitPrice = %d, want 10500", details.UnitPrice)
	}
}

func TestItemDetailsFromFullItemDividesIDRRealPriceByTwoDecimalMinorUnits(t *testing.T) {
	item := &platformclientmodels.FullItemInfo{
		RegionData: map[string][]platformclientmodels.RegionDataItem{
			"ID": {
				regionDataItem(1000000, "IDR", platformclientmodels.RegionDataItemCurrencyTypeREAL),
			},
		},
	}

	details, err := itemDetailsFromFullItem("item-idr", "id", item)
	if err != nil {
		t.Fatalf("itemDetailsFromFullItem returned error: %v", err)
	}
	if details.UnitPrice != 10000 {
		t.Fatalf("UnitPrice = %d, want 10000", details.UnitPrice)
	}
}

func TestItemDetailsFromFullItemDividesJPYRealPriceByTwoDecimalMinorUnits(t *testing.T) {
	item := &platformclientmodels.FullItemInfo{
		RegionData: map[string][]platformclientmodels.RegionDataItem{
			"JP": {
				regionDataItem(10000, "JPY", platformclientmodels.RegionDataItemCurrencyTypeREAL),
			},
		},
	}

	details, err := itemDetailsFromFullItem("item-jpy", "jp", item)
	if err != nil {
		t.Fatalf("itemDetailsFromFullItem returned error: %v", err)
	}
	if details.UnitPrice != 100 {
		t.Fatalf("UnitPrice = %d, want 100", details.UnitPrice)
	}
}

func TestItemDetailsFromFullItemKeepsVirtualCurrencyPriceAsIs(t *testing.T) {
	item := &platformclientmodels.FullItemInfo{
		RegionData: map[string][]platformclientmodels.RegionDataItem{
			"ID": {
				regionDataItem(1000, "COIN", platformclientmodels.RegionDataItemCurrencyTypeVIRTUAL),
			},
		},
	}

	details, err := itemDetailsFromFullItem("coin-pack", "id", item)
	if err != nil {
		t.Fatalf("itemDetailsFromFullItem returned error: %v", err)
	}
	if details.UnitPrice != 1000 {
		t.Fatalf("UnitPrice = %d, want 1000", details.UnitPrice)
	}
	if details.CurrencyCode != "COIN" {
		t.Fatalf("CurrencyCode = %q, want COIN", details.CurrencyCode)
	}
}

func TestItemDetailsFromFullItemKeepsVirtualJPYPriceAsIs(t *testing.T) {
	item := &platformclientmodels.FullItemInfo{
		RegionData: map[string][]platformclientmodels.RegionDataItem{
			"JP": {
				regionDataItem(100, "JPY", platformclientmodels.RegionDataItemCurrencyTypeVIRTUAL),
			},
		},
	}

	details, err := itemDetailsFromFullItem("virtual-jpy", "jp", item)
	if err != nil {
		t.Fatalf("itemDetailsFromFullItem returned error: %v", err)
	}
	if details.UnitPrice != 100 {
		t.Fatalf("UnitPrice = %d, want 100", details.UnitPrice)
	}
}

func TestItemDetailsFromFullItemRejectsMissingCurrencyType(t *testing.T) {
	item := &platformclientmodels.FullItemInfo{
		RegionData: map[string][]platformclientmodels.RegionDataItem{
			"ID": {
				{Price: 1000, CurrencyCode: strPtr("COIN")},
			},
		},
	}

	if _, err := itemDetailsFromFullItem("coin-pack", "id", item); err == nil {
		t.Fatal("expected missing currency type to return an error")
	}
}

func TestItemDetailsFromFullItemRejectsUnsupportedCurrencyType(t *testing.T) {
	item := &platformclientmodels.FullItemInfo{
		RegionData: map[string][]platformclientmodels.RegionDataItem{
			"ID": {
				regionDataItem(1000, "COIN", "BONUS"),
			},
		},
	}

	if _, err := itemDetailsFromFullItem("coin-pack", "id", item); err == nil {
		t.Fatal("expected unsupported currency type to return an error")
	}
}

func TestValidateProviderCurrencyRejectsNonIDRDANA(t *testing.T) {
	if err := validateProviderCurrency("dana", "USD"); err == nil {
		t.Fatal("expected DANA to reject non-IDR currency")
	}
	if err := validateProviderCurrency("dana", "IDR"); err != nil {
		t.Fatalf("expected DANA to accept IDR: %v", err)
	}
}
