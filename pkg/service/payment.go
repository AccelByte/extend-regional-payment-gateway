package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/AccelByte/accelbyte-go-sdk/platform-sdk/pkg/platformclient/item"
	"github.com/AccelByte/accelbyte-go-sdk/platform-sdk/pkg/platformclientmodels"
	"github.com/AccelByte/accelbyte-go-sdk/services-api/pkg/service/platform"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/config"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/model"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/store"
	pb "github.com/accelbyte/extend-regional-payment-gateway/pkg/pb"
)

// PaymentService implements the gRPC PaymentService.
type PaymentService struct {
	pb.UnimplementedPaymentServiceServer
	txStore     store.Store
	registry    *adapter.Registry
	itemService *platform.ItemService
	cfg         *config.Config
}

func NewPaymentService(
	txStore store.Store,
	registry *adapter.Registry,
	itemService *platform.ItemService,
	cfg *config.Config,
) *PaymentService {
	return &PaymentService{
		txStore:     txStore,
		registry:    registry,
		itemService: itemService,
		cfg:         cfg,
	}
}

func (s *PaymentService) CreatePaymentIntent(ctx context.Context, req *pb.CreatePaymentIntentRequest) (*pb.CreatePaymentIntentResponse, error) {
	// Extract userID from gRPC metadata (set by auth interceptor)
	userID, err := userIDFromContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "missing user identity")
	}

	// Validate request
	if req.ItemId == "" {
		return nil, status.Error(codes.InvalidArgument, "item_id is required")
	}
	if req.Quantity < 1 {
		return nil, status.Error(codes.InvalidArgument, "quantity must be >= 1")
	}
	if req.ClientOrderId == "" {
		return nil, status.Error(codes.InvalidArgument, "client_order_id is required")
	}
	if strings.TrimSpace(req.ProviderId) == "" {
		return nil, status.Error(codes.InvalidArgument, "provider_id is required")
	}

	providerID := strings.TrimSpace(req.ProviderId)
	prov, err := s.registry.Get(providerID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "unknown provider: %s", providerID)
	}

	itemDetails, err := s.lookupItemDetails(req.ItemId, req.RegionCode)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to get item price: %v", err)
	}
	totalAmount := itemDetails.UnitPrice * int64(req.Quantity)
	// Rate limit: max PENDING transactions per user
	count, err := s.txStore.CountPendingByUser(ctx, s.cfg.ABNamespace, userID)
	if err != nil {
		slog.Error("CountPendingByUser failed", "error", err)
		return nil, status.Error(codes.Internal, "rate limit check failed")
	}
	if count >= int64(s.cfg.MaxConcurrentIntentPerUser) {
		return nil, status.Errorf(codes.ResourceExhausted, "too many pending transactions (max %d)", s.cfg.MaxConcurrentIntentPerUser)
	}

	now := time.Now().UTC()
	expiresAt := now.Add(s.cfg.PaymentExpiryDefault)
	txID := uuid.New().String()

	tx := &model.Transaction{
		ID:                  txID,
		ClientOrderID:       req.ClientOrderId,
		UserID:              userID,
		Namespace:           s.cfg.ABNamespace,
		ProviderID:          providerID,
		ProviderDisplayName: prov.Info().DisplayName,
		ItemName:            itemDetails.Name,
		ItemID:              req.ItemId,
		Quantity:            req.Quantity,
		RegionCode:          itemDetails.RegionCode,
		Amount:              totalAmount,
		CurrencyCode:        itemDetails.CurrencyCode,
		Status:              model.StatusPending,
		CreatedAt:           now,
		ExpiresAt:           expiresAt,
		UpdatedAt:           now,
		DeleteAt:            expiresAt.AddDate(0, 0, s.cfg.RecordRetentionDays),
	}

	// Insert row FIRST (prevents webhook-before-row race), then call provider
	if err := s.txStore.CreateTransaction(ctx, tx); err != nil {
		if err == store.ErrDuplicateClientOrderID {
			existing, findErr := s.txStore.FindByClientOrderID(ctx, req.ClientOrderId)
			if findErr != nil {
				return nil, status.Error(codes.Internal, "failed to retrieve existing transaction")
			}
			return txToCreateResponse(existing), nil
		}
		return nil, status.Error(codes.Internal, "failed to create transaction")
	}

	initReq := adapter.PaymentInitRequest{
		InternalOrderID: txID,
		UserID:          userID,
		RegionCode:      itemDetails.RegionCode,
		Amount:          totalAmount,
		CurrencyCode:    itemDetails.CurrencyCode,
		Description:     req.Description,
		CallbackURL:     fmt.Sprintf("%s%s/v1/webhook/%s", s.cfg.PublicBaseURL, s.cfg.BasePath, providerID),
		ReturnURL:       returnURLWithTransactionID(s.cfg.PublicBaseURL+s.cfg.BasePath+"/payment-result", txID),
		ExpiryDuration:  s.cfg.PaymentExpiryDefault,
	}
	if err := prov.ValidatePaymentInit(initReq); err != nil {
		_ = s.txStore.DeleteTransaction(ctx, txID)
		return nil, status.Errorf(codes.InvalidArgument, "invalid provider request: %v", err)
	}
	intent, err := prov.CreatePaymentIntent(ctx, initReq)
	if err != nil {
		// Clean up the PENDING row — no webhook can arrive for a failed provider call
		_ = s.txStore.DeleteTransaction(ctx, txID)
		slog.Error("CreatePaymentIntent provider call failed", "provider_id", providerID, "error", err)
		return nil, status.Errorf(codes.Unavailable, "payment provider unavailable: %v", err)
	}

	return &pb.CreatePaymentIntentResponse{
		TransactionId: txID,
		PaymentUrl:    intent.PaymentURL,
		QrCodeData:    intent.QRCodeData,
		ExpiresAt:     expiresAt.Format(time.RFC3339),
		Status:        pb.TransactionStatus_PENDING,
	}, nil
}

func (s *PaymentService) CreateCheckoutTransaction(ctx context.Context, req *pb.CreatePaymentIntentRequest) (*pb.TransactionResponse, error) {
	userID, err := userIDFromContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "missing user identity")
	}

	tx, err := s.createPendingTransaction(ctx, userID, req)
	if err != nil {
		return nil, err
	}
	return txToTransactionResponse(tx), nil
}

func (s *PaymentService) CreatePaymentForExistingTransaction(ctx context.Context, transactionID string, providerID string, description string) (*pb.CreatePaymentIntentResponse, error) {
	userID, err := userIDFromContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "missing user identity")
	}
	if transactionID == "" {
		return nil, status.Error(codes.InvalidArgument, "transaction_id is required")
	}

	tx, err := s.txStore.FindByID(ctx, transactionID)
	if err == store.ErrNotFound {
		return nil, status.Error(codes.NotFound, "transaction not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to retrieve transaction")
	}
	if tx.UserID != userID {
		return nil, status.Error(codes.PermissionDenied, "transaction does not belong to user")
	}
	if tx.Status != model.StatusPending {
		return nil, status.Error(codes.FailedPrecondition, "transaction is not pending")
	}
	if tx.ProviderTxID != "" {
		return nil, status.Error(codes.FailedPrecondition, "payment provider already selected")
	}

	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return nil, status.Error(codes.InvalidArgument, "provider_id is required")
	}
	prov, err := s.registry.Get(providerID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "unknown provider: %s", providerID)
	}

	initReq := adapter.PaymentInitRequest{
		InternalOrderID: tx.ID,
		UserID:          tx.UserID,
		RegionCode:      tx.RegionCode,
		Amount:          tx.Amount,
		CurrencyCode:    tx.CurrencyCode,
		Description:     description,
		CallbackURL:     fmt.Sprintf("%s%s/v1/webhook/%s", s.cfg.PublicBaseURL, s.cfg.BasePath, providerID),
		ReturnURL:       returnURLWithTransactionID(s.cfg.PublicBaseURL+s.cfg.BasePath+"/payment-result", tx.ID),
		ExpiryDuration:  s.cfg.PaymentExpiryDefault,
	}
	if err := prov.ValidatePaymentInit(initReq); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid provider request: %v", err)
	}
	intent, err := prov.CreatePaymentIntent(ctx, initReq)
	if err != nil {
		deleteAt := time.Now().UTC().AddDate(0, 0, 7)
		_ = s.txStore.MarkFailed(ctx, tx.ID, "payment provider unavailable: "+err.Error(), deleteAt)
		slog.Error("CreatePaymentForExistingTransaction provider call failed", "provider_id", providerID, "txn_id", tx.ID, "error", err)
		return nil, status.Errorf(codes.Unavailable, "payment provider unavailable: %v", err)
	}

	if err := s.txStore.AttachProviderTransaction(ctx, tx.ID, providerID, prov.Info().DisplayName, intent.ProviderTransactionID, intent.PaymentURL); err != nil {
		if err == store.ErrNoDocuments {
			current, findErr := s.txStore.FindByID(ctx, tx.ID)
			if findErr == nil && current.ProviderTxID == intent.ProviderTransactionID {
				return &pb.CreatePaymentIntentResponse{
					TransactionId: tx.ID,
					PaymentUrl:    intent.PaymentURL,
					QrCodeData:    intent.QRCodeData,
					ExpiresAt:     tx.ExpiresAt.Format(time.RFC3339),
					Status:        mapStatus(current.Status),
				}, nil
			}
			return nil, status.Error(codes.FailedPrecondition, "payment provider already selected")
		}
		if err == store.ErrNotFound {
			return nil, status.Error(codes.NotFound, "transaction not found")
		}
		return nil, status.Error(codes.Internal, "failed to attach payment provider")
	}

	return &pb.CreatePaymentIntentResponse{
		TransactionId: tx.ID,
		PaymentUrl:    intent.PaymentURL,
		QrCodeData:    intent.QRCodeData,
		ExpiresAt:     tx.ExpiresAt.Format(time.RFC3339),
		Status:        pb.TransactionStatus_PENDING,
	}, nil
}

func (s *PaymentService) GetTransaction(ctx context.Context, req *pb.GetTransactionRequest) (*pb.TransactionResponse, error) {
	tx, err := s.txStore.FindByID(ctx, req.TransactionId)
	if err == store.ErrNotFound {
		return nil, status.Error(codes.NotFound, "transaction not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to retrieve transaction")
	}

	resp := txToTransactionResponse(tx)

	// Live-poll provider status only for PENDING transactions; terminal states use stored value.
	if tx.Status == model.StatusPending && tx.ProviderTxID != "" {
		providerID := resolveProviderID(tx)
		if prov, getErr := s.registry.Get(providerID); getErr == nil {
			if ps, psErr := prov.GetPaymentStatus(ctx, tx.ProviderTxID); psErr == nil {
				resp.ProviderStatus = string(ps.Status)
			}
		}
	}

	return resp, nil
}

func (s *PaymentService) GetTransactionByClientOrder(ctx context.Context, clientOrderID string) (*pb.TransactionResponse, error) {
	tx, err := s.txStore.FindByClientOrderID(ctx, clientOrderID)
	if err == store.ErrNotFound {
		return nil, status.Error(codes.NotFound, "transaction not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to retrieve transaction")
	}

	resp := txToTransactionResponse(tx)

	if tx.Status == model.StatusPending && tx.ProviderTxID != "" {
		providerID := resolveProviderID(tx)
		if prov, getErr := s.registry.Get(providerID); getErr == nil {
			if ps, psErr := prov.GetPaymentStatus(ctx, tx.ProviderTxID); psErr == nil {
				resp.ProviderStatus = string(ps.Status)
			}
		}
	}

	return resp, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

type itemDetails struct {
	Name         string
	UnitPrice    int64
	CurrencyCode string
	RegionCode   string
}

const defaultRegionCode = "ID"

func (s *PaymentService) lookupItemDetails(itemID string, regionCode string) (itemDetails, error) {
	resp, err := s.itemService.GetItemShort(&item.GetItemParams{
		Namespace: s.cfg.ABNamespace,
		ItemID:    itemID,
	})
	if err != nil {
		return itemDetails{}, fmt.Errorf("item lookup failed: %w", err)
	}
	if resp == nil {
		return itemDetails{}, fmt.Errorf("item not found: %s", itemID)
	}
	return itemDetailsFromFullItem(itemID, regionCode, resp)
}

func itemDetailsFromFullItem(itemID string, regionCode string, resp *platformclientmodels.FullItemInfo) (itemDetails, error) {
	name := itemID
	if resp.Name != nil && strings.TrimSpace(*resp.Name) != "" {
		name = strings.TrimSpace(*resp.Name)
	}

	regionCode = normalizeRegionCode(regionCode)
	items, ok := resp.RegionData[regionCode]
	if !ok || len(items) == 0 {
		return itemDetails{}, fmt.Errorf("item %s has no price configured for AGS region %s", itemID, regionCode)
	}
	price := items[0].Price
	if price <= 0 {
		return itemDetails{}, fmt.Errorf("item %s has no positive price configured for AGS region %s", itemID, regionCode)
	}
	currencyCode := ""
	if items[0].CurrencyCode != nil {
		currencyCode = strings.TrimSpace(*items[0].CurrencyCode)
	}
	if currencyCode == "" {
		return itemDetails{}, fmt.Errorf("item %s has no currency code configured for AGS region %s", itemID, regionCode)
	}
	currencyType := ""
	if items[0].CurrencyType != nil {
		currencyType = strings.TrimSpace(*items[0].CurrencyType)
	}
	unitPrice, err := normalizeAGSPrice(price, currencyType)
	if err != nil {
		return itemDetails{}, fmt.Errorf("item %s has invalid price configuration for AGS region %s: %w", itemID, regionCode, err)
	}
	return itemDetails{
		Name:         name,
		UnitPrice:    unitPrice,
		CurrencyCode: strings.ToUpper(currencyCode),
		RegionCode:   regionCode,
	}, nil
}

func (s *PaymentService) lookupItemPrice(itemID string, regionCode string) (int64, string, error) {
	details, err := s.lookupItemDetails(itemID, regionCode)
	if err != nil {
		return 0, "", err
	}
	return details.UnitPrice, details.CurrencyCode, nil
}

func normalizeRegionCode(regionCode string) string {
	regionCode = strings.TrimSpace(regionCode)
	if regionCode == "" {
		return defaultRegionCode
	}
	return strings.ToUpper(regionCode)
}

func normalizeAGSPrice(price int32, currencyType string) (int64, error) {
	switch strings.ToUpper(strings.TrimSpace(currencyType)) {
	case platformclientmodels.RegionDataItemCurrencyTypeREAL:
		return int64(price) / 100, nil
	case platformclientmodels.RegionDataItemCurrencyTypeVIRTUAL:
		return int64(price), nil
	case "":
		return 0, fmt.Errorf("missing currency type")
	default:
		return 0, fmt.Errorf("unsupported currency type %q", currencyType)
	}
}

func returnURLWithTransactionID(rawURL string, transactionID string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		separator := "?"
		if strings.Contains(rawURL, "?") {
			separator = "&"
		}
		return rawURL + separator + "transactionId=" + url.QueryEscape(transactionID)
	}
	q := u.Query()
	q.Set("transactionId", transactionID)
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *PaymentService) createPendingTransaction(ctx context.Context, userID string, req *pb.CreatePaymentIntentRequest) (*model.Transaction, error) {
	if req.ItemId == "" {
		return nil, status.Error(codes.InvalidArgument, "item_id is required")
	}
	if req.Quantity < 1 {
		return nil, status.Error(codes.InvalidArgument, "quantity must be >= 1")
	}
	if req.ClientOrderId == "" {
		return nil, status.Error(codes.InvalidArgument, "client_order_id is required")
	}

	itemDetails, err := s.lookupItemDetails(req.ItemId, req.RegionCode)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to get item price: %v", err)
	}
	totalAmount := itemDetails.UnitPrice * int64(req.Quantity)

	count, err := s.txStore.CountPendingByUser(ctx, s.cfg.ABNamespace, userID)
	if err != nil {
		slog.Error("CountPendingByUser failed", "error", err)
		return nil, status.Error(codes.Internal, "rate limit check failed")
	}
	if count >= int64(s.cfg.MaxConcurrentIntentPerUser) {
		return nil, status.Errorf(codes.ResourceExhausted, "too many pending transactions (max %d)", s.cfg.MaxConcurrentIntentPerUser)
	}

	now := time.Now().UTC()
	expiresAt := now.Add(s.cfg.PaymentExpiryDefault)
	txID := uuid.New().String()
	tx := &model.Transaction{
		ID:            txID,
		ClientOrderID: req.ClientOrderId,
		UserID:        userID,
		Namespace:     s.cfg.ABNamespace,
		ItemName:      itemDetails.Name,
		ItemID:        req.ItemId,
		Quantity:      req.Quantity,
		RegionCode:    itemDetails.RegionCode,
		Amount:        totalAmount,
		CurrencyCode:  itemDetails.CurrencyCode,
		Status:        model.StatusPending,
		CreatedAt:     now,
		ExpiresAt:     expiresAt,
		UpdatedAt:     now,
		DeleteAt:      expiresAt.AddDate(0, 0, s.cfg.RecordRetentionDays),
	}

	if err := s.txStore.CreateTransaction(ctx, tx); err != nil {
		if err == store.ErrDuplicateClientOrderID {
			existing, findErr := s.txStore.FindByClientOrderID(ctx, req.ClientOrderId)
			if findErr != nil {
				return nil, status.Error(codes.Internal, "failed to retrieve existing transaction")
			}
			return existing, nil
		}
		return nil, status.Error(codes.Internal, "failed to create transaction")
	}

	return tx, nil
}

func userIDFromContext(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", fmt.Errorf("no metadata in context")
	}
	// AccelByte auth interceptor puts the validated namespace+userId claims in metadata.
	vals := md.Get("x-auth-user-id")
	if len(vals) > 0 && vals[0] != "" {
		return vals[0], nil
	}
	// Fallback: some versions use "userid"
	vals = md.Get("userid")
	if len(vals) > 0 && vals[0] != "" {
		return vals[0], nil
	}
	vals = md.Get("authorization")
	if len(vals) > 0 {
		if userID := userIDFromBearer(vals[0]); userID != "" {
			return userID, nil
		}
	}
	return "", fmt.Errorf("user ID not found in context")
}

func userIDFromBearer(authHeader string) string {
	token := strings.TrimPrefix(authHeader, "Bearer ")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	sub, _ := claims["sub"].(string)
	return sub
}

func txToCreateResponse(tx *model.Transaction) *pb.CreatePaymentIntentResponse {
	return &pb.CreatePaymentIntentResponse{
		TransactionId: tx.ID,
		ExpiresAt:     tx.ExpiresAt.Format(time.RFC3339),
		Status:        mapStatus(tx.Status),
	}
}

func txToTransactionResponse(tx *model.Transaction) *pb.TransactionResponse {
	resp := &pb.TransactionResponse{
		TransactionId:       tx.ID,
		UserId:              tx.UserID,
		Namespace:           tx.Namespace,
		ProviderId:          tx.ProviderID,
		ProviderDisplayName: tx.ProviderDisplayName,
		Amount:              tx.Amount,
		CurrencyCode:        tx.CurrencyCode,
		ItemName:            tx.ItemName,
		ItemId:              tx.ItemID,
		Quantity:            tx.Quantity,
		Status:              mapStatus(tx.Status),
		ProviderTxId:        tx.ProviderTxID,
		ProviderStatus:      tx.ProviderStatus,
		FailureReason:       tx.FailureReason,
		PaymentUrl:          tx.PaymentURL,
		CreatedAt:           timestamppb.New(tx.CreatedAt),
		ExpiresAt:           timestamppb.New(tx.ExpiresAt),
		UpdatedAt:           timestamppb.New(tx.UpdatedAt),
	}
	if tx.Refund != nil {
		resp.RefundStatus = tx.Refund.Status
		resp.RefundReason = tx.Refund.Reason
		resp.RefundFailureReason = tx.Refund.FailureReason
	}
	return resp
}

func mapStatus(s string) pb.TransactionStatus {
	switch s {
	case model.StatusPending:
		return pb.TransactionStatus_PENDING
	case model.StatusFulfilling:
		return pb.TransactionStatus_FULFILLING
	case model.StatusFulfilled:
		return pb.TransactionStatus_FULFILLED
	case model.StatusFailed:
		return pb.TransactionStatus_FAILED
	case model.StatusCanceled:
		return pb.TransactionStatus_CANCELED
	case model.StatusExpired:
		return pb.TransactionStatus_EXPIRED
	default:
		return pb.TransactionStatus_TRANSACTION_STATUS_UNSPECIFIED
	}
}
