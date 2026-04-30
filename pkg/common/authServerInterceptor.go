// Copyright (c) 2023 AccelByte Inc. All Rights Reserved.
// This is licensed software from AccelByte Inc, for limitations
// and restrictions contact your company contract manager.

package common

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2/options"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/AccelByte/accelbyte-go-sdk/iam-sdk/pkg/iamclientmodels"
	"github.com/AccelByte/accelbyte-go-sdk/services-api/pkg/service/iam"
	"github.com/AccelByte/accelbyte-go-sdk/services-api/pkg/utils/auth/validator"

	pb "github.com/accelbyte/extend-regional-payment-gateway/pkg/pb"
)

var (
	Validator validator.AuthTokenValidator
)

// AuthRequirement represents the authentication requirements for a method.
type AuthRequirement struct {
	RequireToken bool
	Permission   *iam.Permission
}

func parseFullMethod(fullMethod string) (string, string, error) {
	// Define the regular expression according to example shown here https://github.com/grpc/grpc-java/issues/4726
	re := regexp.MustCompile(`^/([^/]+)/([^/]+)$`)
	matches := re.FindStringSubmatch(fullMethod)

	// Validate the match
	if matches == nil {
		return "", "", fmt.Errorf("invalid FullMethod format")
	}

	// Extract service and method names
	serviceName, methodName := matches[1], matches[2]

	if len(serviceName) == 0 {
		return "", "", fmt.Errorf("invalid FullMethod format: service name is empty")
	}

	if len(methodName) == 0 {
		return "", "", fmt.Errorf("invalid FullMethod format: method name is empty")
	}

	return serviceName, methodName, nil
}

func extractAuthRequirement(infoUnary *grpc.UnaryServerInfo, infoStream *grpc.StreamServerInfo) (*AuthRequirement, error) {
	if infoUnary != nil && infoStream != nil {
		return nil, errors.New("both infoUnary and infoStream cannot be filled at the same time")
	}

	var serviceName string
	var methodName string
	var err error

	if infoUnary != nil {
		serviceName, methodName, err = parseFullMethod(infoUnary.FullMethod)
	} else if infoStream != nil {
		serviceName, methodName, err = parseFullMethod(infoStream.FullMethod)
	} else {
		return nil, errors.New("both infoUnary and infoStream are nil")
	}
	if err != nil {
		return nil, err
	}

	// Read the method descriptor from the proto file
	desc, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(serviceName))
	if err != nil {
		return nil, err
	}

	serviceDesc := desc.(protoreflect.ServiceDescriptor)
	method := serviceDesc.Methods().ByName(protoreflect.Name(methodName))
	methodOptions := method.Options()

	// Check if the OpenAPI v2 operation specifies security requirements (e.g., Bearer auth)
	hasBearerSecurity := hasSecurityScheme(methodOptions, "Bearer")

	// Check for permission.action and permission.resource via pb helpers.
	// The helpers are stubs before proto generation; after `make proto` they
	// read the real proto extensions from the method options.
	resource := pb.GetResourceExtension(methodOptions)
	action := pb.GetActionExtension(methodOptions)

	// If both permission.action and permission.resource are set, require permission
	var permission *iam.Permission
	if resource != "" && action.Number() != 0 {
		permission = &iam.Permission{
			Action:   int(action.Number()),
			Resource: resource,
		}
	}

	return &AuthRequirement{
		RequireToken: hasBearerSecurity,
		Permission:   permission,
	}, nil
}

func hasSecurityScheme(methodOptions protoreflect.ProtoMessage, schemeName string) bool {
	// Get the openapiv2_operation extension
	opExt := proto.GetExtension(methodOptions, options.E_Openapiv2Operation)
	if opExt == nil {
		return false
	}

	operation, ok := opExt.(*options.Operation)
	if !ok || operation == nil {
		return false
	}

	// Check if security is defined and has at least one security requirement with the specified scheme
	for _, securityReq := range operation.Security {
		if securityReq == nil {
			continue
		}
		// Check if the security requirement map contains the specified scheme key
		if _, hasScheme := securityReq.SecurityRequirement[schemeName]; hasScheme {
			return true
		}
	}

	return false
}

func getNamespace() string {
	return GetEnv("AB_NAMESPACE", "accelbyte")
}

func checkAuthorizationMetadata(ctx context.Context, permission *iam.Permission) error {
	if Validator == nil {
		return status.Error(codes.Internal, "authorization token validator is not set")
	}

	meta, found := metadata.FromIncomingContext(ctx)

	if !found {
		return status.Error(codes.Unauthenticated, "metadata is missing")
	}

	if _, ok := meta["authorization"]; !ok {
		return status.Error(codes.Unauthenticated, "authorization metadata is missing")
	}

	if len(meta["authorization"]) == 0 {
		return status.Error(codes.Unauthenticated, "authorization metadata length is 0")
	}

	authorization := meta["authorization"][0]
	token := strings.TrimPrefix(authorization, "Bearer ")
	namespace := getNamespace()

	err := Validator.Validate(token, permission, &namespace, nil)

	if err != nil {
		return status.Error(codes.PermissionDenied, err.Error())
	}

	return nil
}

func ValidateBearerToken(authHeader, namespace string) error {
	return validateBearer(authHeader, namespace, nil)
}

func ValidateBearerPermission(authHeader, namespace string, permission *iam.Permission) error {
	return validateBearer(authHeader, namespace, permission)
}

func validateBearer(authHeader, namespace string, permission *iam.Permission) error {
	if Validator == nil {
		return status.Error(codes.Internal, "authorization token validator is not set")
	}
	if authHeader == "" {
		return status.Error(codes.Unauthenticated, "authorization header is missing")
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == "" || token == authHeader {
		return status.Error(codes.Unauthenticated, "bearer token is missing")
	}
	if namespace == "" {
		namespace = getNamespace()
	}
	if err := Validator.Validate(token, permission, &namespace, nil); err != nil {
		return status.Error(codes.PermissionDenied, err.Error())
	}
	return nil
}

func NewUnaryAuthServerIntercept() func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) { // nolint
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		// Extract auth requirement from the proto file
		requirement, err := extractAuthRequirement(info, nil)
		if err != nil {
			return nil, err
		}

		// If no auth requirement, skip all auth checks (public access)
		if requirement == nil {
			return handler(ctx, req)
		}

		// Enforce auth whenever the proto declares Bearer security or explicit permissions
		// (treat permissions as authoritative even if the security block was omitted by mistake)
		if requirement.RequireToken || requirement.Permission != nil {
			err = checkAuthorizationMetadata(ctx, requirement.Permission)
			if err != nil {
				return nil, err
			}
		}

		return handler(ctx, req)
	}
}

func NewStreamAuthServerIntercept() func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		// Extract auth requirement from the proto file
		requirement, err := extractAuthRequirement(nil, info)
		if err != nil {
			return err
		}

		// If no auth requirement, skip all auth checks (public access)
		if requirement == nil {
			return handler(srv, ss)
		}

		// Enforce auth whenever the proto declares Bearer security or explicit permissions
		// (treat permissions as authoritative even if the security block was omitted by mistake)
		if requirement.RequireToken || requirement.Permission != nil {
			err = checkAuthorizationMetadata(ss.Context(), requirement.Permission)
			if err != nil {
				return err
			}
		}

		return handler(srv, ss)
	}
}

func NewTokenValidator(authService iam.OAuth20Service, refreshInterval time.Duration, validateLocally bool) validator.AuthTokenValidator {
	return &iam.TokenValidator{
		AuthService:     authService,
		RefreshInterval: refreshInterval,

		Filter:                nil,
		JwkSet:                nil,
		JwtClaims:             iam.JWTClaims{},
		JwtEncoding:           *base64.URLEncoding.WithPadding(base64.NoPadding),
		PublicKeys:            make(map[string]*rsa.PublicKey),
		LocalValidationActive: validateLocally,
		RevokedUsers:          make(map[string]time.Time),
		Roles:                 make(map[string]*iamclientmodels.ModelRolePermissionResponseV3),
	}
}
