package common

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AccelByte/accelbyte-go-sdk/services-api/pkg/service/iam"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/checkout"
	pb "github.com/accelbyte/extend-regional-payment-gateway/pkg/pb"
)

func TestValidateBearerToken(t *testing.T) {
	previous := Validator
	t.Cleanup(func() { Validator = previous })

	Validator = &fakeAuthValidator{}
	if err := ValidateBearerToken("", "ns"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("missing token code = %s, want Unauthenticated", status.Code(err))
	}

	Validator = &fakeAuthValidator{err: errors.New("invalid token")}
	if err := ValidateBearerToken("Bearer token", "ns"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("invalid token code = %s, want PermissionDenied", status.Code(err))
	}

	validator := &fakeAuthValidator{}
	Validator = validator
	if err := ValidateBearerToken("Bearer token", "ns"); err != nil {
		t.Fatalf("valid token returned error: %v", err)
	}
	if validator.token != "token" || validator.namespace != "ns" {
		t.Fatalf("unexpected validation input token=%q namespace=%q", validator.token, validator.namespace)
	}
}

func TestValidateBearerPermission(t *testing.T) {
	previous := Validator
	t.Cleanup(func() { Validator = previous })

	validator := &fakeAuthValidator{}
	Validator = validator
	permission := &iam.Permission{Action: int(pb.Action_ACTION_UPDATE), Resource: "ADMIN:NAMESPACE:ns:PAYMENT:TRANSACTION"}
	if err := ValidateBearerPermission("Bearer token", "ns", permission); err != nil {
		t.Fatalf("valid token returned error: %v", err)
	}
	if validator.permission == nil || validator.permission.Action != int(pb.Action_ACTION_UPDATE) || validator.permission.Resource != permission.Resource {
		t.Fatalf("unexpected permission: %+v", validator.permission)
	}
}

func TestNewCheckoutSessionFromTransactionCarriesRegionCode(t *testing.T) {
	before := time.Now()
	tx := &pb.TransactionResponse{
		TransactionId: "txn-1",
		ItemName:      "Starter Pack",
		ItemId:        "starter-pack",
		Quantity:      2,
		Amount:        30000,
		CurrencyCode:  "IDR",
		RegionCode:    "ID",
	}

	sess := newCheckoutSessionFromTransaction(tx, "user-1", "buy starter pack")

	if sess.TransactionID != "txn-1" || sess.UserID != "user-1" || sess.Description != "buy starter pack" {
		t.Fatalf("unexpected session identity: %+v", sess)
	}
	if sess.ItemName != "Starter Pack" || sess.ItemID != "starter-pack" || sess.Quantity != 2 {
		t.Fatalf("unexpected session item details: %+v", sess)
	}
	if sess.TotalPrice != 30000 || sess.UnitPrice != 15000 || sess.CurrencyCode != "IDR" {
		t.Fatalf("unexpected session price details: %+v", sess)
	}
	if sess.RegionCode != "ID" {
		t.Fatalf("RegionCode = %q, want ID", sess.RegionCode)
	}
	if sess.ExpiresAt.Before(before.Add(checkout.CheckoutSessionExpiry - time.Second)) {
		t.Fatalf("ExpiresAt too early: %s", sess.ExpiresAt)
	}
}

type fakeAuthValidator struct {
	token      string
	namespace  string
	permission *iam.Permission
	err        error
}

func (f *fakeAuthValidator) Initialize(ctx ...context.Context) error { return nil }

func (f *fakeAuthValidator) Validate(token string, permission *iam.Permission, namespace *string, _ *string) error {
	f.token = token
	f.permission = permission
	if namespace != nil {
		f.namespace = *namespace
	}
	return f.err
}
