package common

import (
	"context"
	"errors"
	"testing"

	"github.com/AccelByte/accelbyte-go-sdk/services-api/pkg/service/iam"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
