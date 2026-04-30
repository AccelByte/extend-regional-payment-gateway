// Copyright (c) 2023-2025 AccelByte Inc. All Rights Reserved.
// This is licensed software from AccelByte Inc, for limitations
// and restrictions contact your company contract manager.

package common

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/AccelByte/accelbyte-go-sdk/services-api/pkg/service/iam"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/checkout"
	pb "github.com/accelbyte/extend-regional-payment-gateway/pkg/pb"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Gateway struct {
	mux      *runtime.ServeMux
	basePath string
}

type transactionHTTPResponse struct {
	TransactionId       string `json:"transactionId"`
	UserId              string `json:"userId"`
	Namespace           string `json:"namespace"`
	Provider            string `json:"provider,omitempty"`
	Amount              int64  `json:"amount"`
	CurrencyCode        string `json:"currencyCode"`
	ItemName            string `json:"itemName,omitempty"`
	ItemId              string `json:"itemId"`
	Quantity            int32  `json:"quantity"`
	Status              string `json:"status"`
	ProviderStatus      string `json:"providerStatus,omitempty"`
	ProviderTxId        string `json:"providerTxId,omitempty"`
	PaymentUrl          string `json:"paymentUrl,omitempty"`
	FailureReason       string `json:"failureReason,omitempty"`
	RefundStatus        string `json:"refundStatus,omitempty"`
	RefundReason        string `json:"refundReason,omitempty"`
	RefundFailureReason string `json:"refundFailureReason,omitempty"`
	CustomProviderName  string `json:"customProviderName,omitempty"`
	CreatedAt           string `json:"createdAt,omitempty"`
	UpdatedAt           string `json:"updatedAt,omitempty"`
	ExpiresAt           string `json:"expiresAt,omitempty"`
}

type listTransactionsHTTPResponse struct {
	Transactions []transactionHTTPResponse `json:"transactions"`
	NextCursor   string                    `json:"nextCursor,omitempty"`
}

type cancelTransactionHTTPResponse struct {
	TransactionId string                  `json:"transactionId"`
	Success       bool                    `json:"success"`
	Transaction   transactionHTTPResponse `json:"transaction"`
	Message       string                  `json:"message,omitempty"`
}

func NewGateway(ctx context.Context, grpcServerEndpoint string, basePath string) (*Gateway, error) {
	mux := runtime.NewServeMux()
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}

	if err := pb.RegisterPaymentServiceHandlerFromEndpoint(ctx, mux, grpcServerEndpoint, opts); err != nil {
		return nil, err
	}
	if err := pb.RegisterPublicServiceHandlerFromEndpoint(ctx, mux, grpcServerEndpoint, opts); err != nil {
		return nil, err
	}
	if err := pb.RegisterWebhookServiceHandlerFromEndpoint(ctx, mux, grpcServerEndpoint, opts); err != nil {
		return nil, err
	}
	if err := pb.RegisterAdminServiceHandlerFromEndpoint(ctx, mux, grpcServerEndpoint, opts); err != nil {
		return nil, err
	}

	return &Gateway{
		mux:      mux,
		basePath: basePath,
	}, nil
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Strip the base path since the base_path proto annotation doesn't affect gRPC-Gateway routing.
	// Reference: https://github.com/grpc-ecosystem/grpc-gateway/pull/919/commits/1c34df861cfc0d6cb19ea617921d7d9eaa209977
	http.StripPrefix(g.basePath, g.mux).ServeHTTP(w, r)
}

// RegisterDirectHandlers wires HTTP routes directly to gRPC service implementations,
// bypassing gRPC-Gateway transcoding. Used when proto generation has not been run yet
// (stub RegisterXxxHandlerFromEndpoint returns nil without registering routes).
func RegisterDirectHandlers(
	mux *http.ServeMux,
	basePath string,
	paymentSvc pb.PaymentServiceServer,
	publicSvc pb.PublicServiceServer,
	adminSvc pb.AdminServiceServer,
	checkoutStore *checkout.Store,
	publicBaseURL string,
) {
	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}

	writeErr := func(w http.ResponseWriter, code int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg})
	}

	writeTransactionJSON := func(w http.ResponseWriter, code int, tx *pb.TransactionResponse) {
		writeJSON(w, code, toHTTPTransaction(tx))
	}

	// Inject HTTP headers into gRPC incoming metadata so userIDFromContext works.
	// Direct handlers bypass the gRPC interceptor, so we parse the JWT ourselves
	// to extract the user ID (sub claim) and inject it as x-auth-user-id.
	injectMeta := func(r *http.Request) context.Context {
		md := metadata.MD{}
		// Explicit header takes priority (e.g. for testing)
		if v := r.Header.Get("x-auth-user-id"); v != "" {
			md["x-auth-user-id"] = []string{v}
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			md["authorization"] = []string{auth}
			// Parse JWT payload to extract sub (user ID)
			if uid := jwtSub(auth); uid != "" && md["x-auth-user-id"] == nil {
				md["x-auth-user-id"] = []string{uid}
			}
		}
		return metadata.NewIncomingContext(r.Context(), md)
	}

	injectValidatedPublicMeta := func(r *http.Request, namespace string) (context.Context, error) {
		auth := r.Header.Get("Authorization")
		if err := ValidateBearerToken(auth, namespace); err != nil {
			return nil, err
		}
		userID := jwtSub(auth)
		if userID == "" {
			return nil, status.Error(codes.Unauthenticated, "missing user identity")
		}
		return metadata.NewIncomingContext(r.Context(), metadata.MD{
			"authorization":  []string{auth},
			"x-auth-user-id": []string{userID},
		}), nil
	}

	injectValidatedAdminMeta := func(r *http.Request, namespace string, action int) (context.Context, error) {
		auth := r.Header.Get("Authorization")
		if err := ValidateBearerPermission(auth, namespace, &iam.Permission{
			Action:   action,
			Resource: fmt.Sprintf("ADMIN:NAMESPACE:%s:PAYMENT:TRANSACTION", namespace),
		}); err != nil {
			return nil, err
		}
		md := metadata.MD{"authorization": []string{auth}}
		if uid := jwtSub(auth); uid != "" {
			md["x-auth-user-id"] = []string{uid}
		}
		return metadata.NewIncomingContext(r.Context(), md), nil
	}

	// POST /v1/payment/intent
	mux.HandleFunc(basePath+"/v1/payment/intent", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req pb.CreatePaymentIntentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		resp, err := paymentSvc.CreatePaymentIntent(injectMeta(r), &req)
		if err != nil {
			writeErr(w, grpcErrToHTTP(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})

	// GET /v1/payment/transaction/by-client-order/{clientOrderId}
	// Must be registered before the generic /transaction/ handler so it matches first.
	type clientOrderLookup interface {
		GetTransactionByClientOrder(ctx context.Context, clientOrderID string) (*pb.TransactionResponse, error)
	}
	mux.HandleFunc(basePath+"/v1/payment/transaction/by-client-order/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		clientOrderID := strings.TrimPrefix(r.URL.Path, basePath+"/v1/payment/transaction/by-client-order/")
		if clientOrderID == "" {
			writeErr(w, http.StatusBadRequest, "missing clientOrderId")
			return
		}
		finder, ok := paymentSvc.(clientOrderLookup)
		if !ok {
			writeErr(w, http.StatusInternalServerError, "lookup not supported")
			return
		}
		resp, err := finder.GetTransactionByClientOrder(injectMeta(r), clientOrderID)
		if err != nil {
			writeErr(w, grpcErrToHTTP(err), err.Error())
			return
		}
		writeTransactionJSON(w, http.StatusOK, resp)
	})

	// GET /v1/payment/transaction/{transactionId}
	mux.HandleFunc(basePath+"/v1/payment/transaction/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		txID := strings.TrimPrefix(r.URL.Path, basePath+"/v1/payment/transaction/")
		if txID == "" {
			writeErr(w, http.StatusBadRequest, "missing transactionId")
			return
		}
		resp, err := paymentSvc.GetTransaction(injectMeta(r), &pb.GetTransactionRequest{TransactionId: txID})
		if err != nil {
			writeErr(w, grpcErrToHTTP(err), err.Error())
			return
		}
		writeTransactionJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc(basePath+"/v1/public/namespace/", func(w http.ResponseWriter, r *http.Request) {
		trimmed := strings.TrimPrefix(r.URL.Path, basePath+"/v1/public/namespace/")
		parts := strings.Split(trimmed, "/")
		if len(parts) < 5 || parts[1] != "users" || parts[2] != "me" || parts[3] != "transactions" {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		ns := parts[0]
		ctx, authErr := injectValidatedPublicMeta(r, ns)
		if authErr != nil {
			writeErr(w, grpcErrToHTTP(authErr), authErr.Error())
			return
		}
		if len(parts) == 5 && parts[4] == "sync" {
			if r.Method != http.MethodPost {
				writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			var body struct {
				Provider string `json:"provider"`
				PageSize int32  `json:"pageSize"`
				Cursor   string `json:"cursor"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			resp, err := publicSvc.SyncMyTransactions(ctx, &pb.PublicSyncTransactionsRequest{
				Namespace: ns,
				Provider:  body.Provider,
				PageSize:  body.PageSize,
				Cursor:    body.Cursor,
			})
			if err != nil {
				writeErr(w, grpcErrToHTTP(err), err.Error())
				return
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
		if len(parts) == 6 && parts[5] == "sync" {
			if r.Method != http.MethodPost {
				writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			resp, err := publicSvc.SyncMyTransaction(ctx, &pb.PublicSyncTransactionRequest{
				Namespace:     ns,
				TransactionId: parts[4],
			})
			if err != nil {
				writeErr(w, grpcErrToHTTP(err), err.Error())
				return
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
		if len(parts) == 6 && parts[5] == "cancel" {
			if r.Method != http.MethodPost {
				writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			var body struct {
				Reason string `json:"reason"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			resp, err := publicSvc.CancelMyTransaction(ctx, &pb.CancelTransactionRequest{
				Namespace:     ns,
				TransactionId: parts[4],
				Reason:        body.Reason,
			})
			if err != nil {
				writeErr(w, grpcErrToHTTP(err), err.Error())
				return
			}
			writeJSON(w, http.StatusOK, cancelTransactionHTTPResponse{
				TransactionId: resp.TransactionId,
				Success:       resp.Success,
				Transaction:   toHTTPTransaction(resp.Transaction),
				Message:       resp.Message,
			})
			return
		}
		writeErr(w, http.StatusNotFound, "not found")
	})

	// GET /v1/admin/namespace/{namespace}/transactions
	// GET /v1/admin/namespace/{namespace}/transactions/{transactionId}
	// POST /v1/admin/namespace/{namespace}/transactions/{transactionId}/refund
	mux.HandleFunc(basePath+"/v1/admin/namespace/", func(w http.ResponseWriter, r *http.Request) {
		// Parse: /v1/admin/namespace/{ns}/transactions[/{tx_id}[/refund]]
		trimmed := strings.TrimPrefix(r.URL.Path, basePath+"/v1/admin/namespace/")
		parts := strings.SplitN(trimmed, "/", 4)
		// parts[0] = namespace, parts[1] = "transactions", parts[2] = tx_id (optional), parts[3] = "refund" (optional)

		if len(parts) < 2 || parts[1] != "transactions" {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		ns := parts[0]

		if len(parts) == 2 {
			// List transactions
			if r.Method != http.MethodGet {
				writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			ctx, authErr := injectValidatedAdminMeta(r, ns, int(pb.Action_ACTION_READ))
			if authErr != nil {
				writeErr(w, grpcErrToHTTP(authErr), authErr.Error())
				return
			}
			q := r.URL.Query()
			req := &pb.ListTransactionsRequest{
				Namespace:    ns,
				UserId:       q.Get("userId"),
				StatusFilter: parseTransactionStatus(q.Get("statusFilter")),
				Provider:     q.Get("provider"),
				Cursor:       q.Get("cursor"),
				Search:       q.Get("search"),
			}
			if ps := q.Get("pageSize"); ps != "" {
				var n int32
				json.Unmarshal([]byte(ps), &n)
				req.PageSize = n
			}
			resp, err := adminSvc.ListTransactions(ctx, req)
			if err != nil {
				writeErr(w, grpcErrToHTTP(err), err.Error())
				return
			}
			out := listTransactionsHTTPResponse{
				Transactions: make([]transactionHTTPResponse, 0, len(resp.Transactions)),
				NextCursor:   resp.NextCursor,
			}
			for _, tx := range resp.Transactions {
				out.Transactions = append(out.Transactions, toHTTPTransaction(tx))
			}
			writeJSON(w, http.StatusOK, out)
			return
		}

		if len(parts) == 3 && parts[2] == "sync" {
			if r.Method != http.MethodPost {
				writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			ctx, authErr := injectValidatedAdminMeta(r, ns, int(pb.Action_ACTION_UPDATE))
			if authErr != nil {
				writeErr(w, grpcErrToHTTP(authErr), authErr.Error())
				return
			}
			var body struct {
				Provider     string `json:"provider"`
				StatusFilter string `json:"statusFilter"`
				PageSize     int32  `json:"pageSize"`
				Cursor       string `json:"cursor"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			resp, err := adminSvc.SyncTransactions(ctx, &pb.SyncTransactionsRequest{
				Namespace:    ns,
				Provider:     body.Provider,
				StatusFilter: parseTransactionStatus(body.StatusFilter),
				PageSize:     body.PageSize,
				Cursor:       body.Cursor,
			})
			if err != nil {
				writeErr(w, grpcErrToHTTP(err), err.Error())
				return
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}

		txID := parts[2]

		if len(parts) == 4 && parts[3] == "refund" {
			// Refund
			if r.Method != http.MethodPost {
				writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			ctx, authErr := injectValidatedAdminMeta(r, ns, int(pb.Action_ACTION_UPDATE))
			if authErr != nil {
				writeErr(w, grpcErrToHTTP(authErr), authErr.Error())
				return
			}
			var body struct {
				Reason string `json:"reason"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			resp, err := adminSvc.Refund(ctx, &pb.RefundRequest{Namespace: ns, TransactionId: txID, Reason: body.Reason})
			if err != nil {
				writeErr(w, grpcErrToHTTP(err), err.Error())
				return
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}

		if len(parts) == 4 && parts[3] == "sync" {
			if r.Method != http.MethodPost {
				writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			ctx, authErr := injectValidatedAdminMeta(r, ns, int(pb.Action_ACTION_UPDATE))
			if authErr != nil {
				writeErr(w, grpcErrToHTTP(authErr), authErr.Error())
				return
			}
			resp, err := adminSvc.SyncTransaction(ctx, &pb.SyncTransactionRequest{Namespace: ns, TransactionId: txID})
			if err != nil {
				writeErr(w, grpcErrToHTTP(err), err.Error())
				return
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}

		// Get transaction detail
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		ctx, authErr := injectValidatedAdminMeta(r, ns, int(pb.Action_ACTION_READ))
		if authErr != nil {
			writeErr(w, grpcErrToHTTP(authErr), authErr.Error())
			return
		}
		resp, err := adminSvc.GetTransactionDetail(ctx, &pb.GetTransactionRequest{Namespace: ns, TransactionId: txID})
		if err != nil {
			writeErr(w, grpcErrToHTTP(err), err.Error())
			return
		}
		writeTransactionJSON(w, http.StatusOK, resp)
	})

	// POST /v1/payment/checkout — creates a hosted checkout session and a pollable transaction.
	type checkoutTransactionCreator interface {
		CreateCheckoutTransaction(ctx context.Context, req *pb.CreatePaymentIntentRequest) (*pb.TransactionResponse, error)
	}
	mux.HandleFunc(basePath+"/v1/payment/checkout", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		userID := jwtSub(r.Header.Get("Authorization"))
		if userID == "" {
			if v := r.Header.Get("x-auth-user-id"); v != "" {
				userID = v
			}
		}
		if userID == "" {
			writeErr(w, http.StatusUnauthorized, "missing or invalid authorization token")
			return
		}
		var body struct {
			ItemID        string `json:"itemId"`
			Quantity      int32  `json:"quantity"`
			ClientOrderID string `json:"clientOrderId"`
			Description   string `json:"description"`
			RegionCode    string `json:"regionCode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		if body.ItemID == "" {
			writeErr(w, http.StatusBadRequest, "itemId is required")
			return
		}
		if body.Quantity < 1 {
			writeErr(w, http.StatusBadRequest, "quantity must be >= 1")
			return
		}
		if body.ClientOrderID == "" {
			writeErr(w, http.StatusBadRequest, "clientOrderId is required")
			return
		}
		creator, ok := paymentSvc.(checkoutTransactionCreator)
		if !ok {
			writeErr(w, http.StatusInternalServerError, "checkout transaction creation not supported")
			return
		}
		tx, err := creator.CreateCheckoutTransaction(injectMeta(r), &pb.CreatePaymentIntentRequest{
			ItemId:        body.ItemID,
			Quantity:      body.Quantity,
			ClientOrderId: body.ClientOrderID,
			Description:   body.Description,
			RegionCode:    body.RegionCode,
		})
		if err != nil {
			writeErr(w, grpcErrToHTTP(err), err.Error())
			return
		}
		sess := &checkout.Session{
			TransactionID: tx.TransactionId,
			UserID:        userID,
			Description:   body.Description,
			ItemName:      tx.ItemName,
			ItemID:        tx.ItemId,
			Quantity:      tx.Quantity,
			UnitPrice:     unitPrice(tx.Amount, tx.Quantity),
			TotalPrice:    tx.Amount,
			CurrencyCode:  tx.CurrencyCode,
			ExpiresAt:     time.Now().Add(checkout.CheckoutSessionExpiry),
		}
		sessionID := checkoutStore.Create(sess)
		checkoutURL := publicBaseURL + basePath + "/checkout/" + sessionID
		writeJSON(w, http.StatusOK, map[string]string{
			"checkoutUrl":   checkoutURL,
			"transactionId": tx.TransactionId,
		})
	})
}

func toHTTPTransaction(tx *pb.TransactionResponse) transactionHTTPResponse {
	if tx == nil {
		return transactionHTTPResponse{}
	}
	return transactionHTTPResponse{
		TransactionId:       tx.TransactionId,
		UserId:              tx.UserId,
		Namespace:           tx.Namespace,
		Provider:            providerToJSON(tx.Provider),
		Amount:              tx.Amount,
		CurrencyCode:        tx.CurrencyCode,
		ItemName:            tx.ItemName,
		ItemId:              tx.ItemId,
		Quantity:            tx.Quantity,
		Status:              tx.Status.String(),
		ProviderStatus:      tx.ProviderStatus,
		ProviderTxId:        tx.ProviderTxId,
		PaymentUrl:          tx.PaymentUrl,
		FailureReason:       tx.FailureReason,
		RefundStatus:        tx.RefundStatus,
		RefundReason:        tx.RefundReason,
		RefundFailureReason: tx.RefundFailureReason,
		CustomProviderName:  tx.CustomProviderName,
		CreatedAt:           timestampToJSON(tx.CreatedAt),
		UpdatedAt:           timestampToJSON(tx.UpdatedAt),
		ExpiresAt:           timestampToJSON(tx.ExpiresAt),
	}
}

func unitPrice(amount int64, quantity int32) int64 {
	if quantity <= 0 {
		return amount
	}
	return amount / int64(quantity)
}

func providerToJSON(provider pb.Provider) string {
	switch provider {
	case pb.Provider_PROVIDER_DANA:
		return "dana"
	case pb.Provider_PROVIDER_XENDIT:
		return "xendit"
	case pb.Provider_PROVIDER_KOMOJU:
		return "komoju"
	case pb.Provider_PROVIDER_CUSTOM:
		return "PROVIDER_CUSTOM"
	default:
		return ""
	}
}

func timestampToJSON(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return ""
	}
	return ts.AsTime().UTC().Format(time.RFC3339Nano)
}

func parseTransactionStatus(value string) pb.TransactionStatus {
	switch value {
	case "PENDING":
		return pb.TransactionStatus_PENDING
	case "FULFILLING":
		return pb.TransactionStatus_FULFILLING
	case "FULFILLED":
		return pb.TransactionStatus_FULFILLED
	case "FAILED":
		return pb.TransactionStatus_FAILED
	case "CANCELED":
		return pb.TransactionStatus_CANCELED
	case "EXPIRED":
		return pb.TransactionStatus_EXPIRED
	default:
		return pb.TransactionStatus_TRANSACTION_STATUS_UNSPECIFIED
	}
}

// jwtSub decodes the payload of a Bearer JWT and returns the "sub" claim.
// No signature verification — the gRPC interceptor handles that separately.
func jwtSub(authHeader string) string {
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

func grpcErrToHTTP(err error) int {
	msg := err.Error()
	if strings.Contains(msg, "Unauthenticated") || strings.Contains(msg, "unauthenticated") {
		return http.StatusUnauthorized
	}
	if strings.Contains(msg, "PermissionDenied") || strings.Contains(msg, "permission denied") {
		return http.StatusForbidden
	}
	if strings.Contains(msg, "NotFound") || strings.Contains(msg, "not found") {
		return http.StatusNotFound
	}
	if strings.Contains(msg, "InvalidArgument") || strings.Contains(msg, "invalid") {
		return http.StatusBadRequest
	}
	if strings.Contains(msg, "ResourceExhausted") {
		return http.StatusTooManyRequests
	}
	if strings.Contains(msg, "Unavailable") {
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}
