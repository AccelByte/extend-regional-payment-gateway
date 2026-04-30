// Copyright (c) 2026 AccelByte Inc. All Rights Reserved.
// This is licensed software from AccelByte Inc, for limitations
// and restrictions contact your company contract manager.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-openapi/loads"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	prometheusGrpc "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/prometheus/client_golang/prometheus"
	prometheusCollectors "github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.mongodb.org/mongo-driver/v2/mongo"
	mongoOpts "go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/propagators/b3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"github.com/AccelByte/accelbyte-go-sdk/services-api/pkg/factory"
	"github.com/AccelByte/accelbyte-go-sdk/services-api/pkg/repository"
	"github.com/AccelByte/accelbyte-go-sdk/services-api/pkg/service/iam"
	"github.com/AccelByte/accelbyte-go-sdk/services-api/pkg/service/platform"
	sdkAuth "github.com/AccelByte/accelbyte-go-sdk/services-api/pkg/utils/auth"

	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter"
	dana "github.com/accelbyte/extend-regional-payment-gateway/internal/adapter/dana"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/adapter/generic"
	komoju "github.com/accelbyte/extend-regional-payment-gateway/internal/adapter/komoju"
	xendit "github.com/accelbyte/extend-regional-payment-gateway/internal/adapter/xendit"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/checkout"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/config"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/fulfillment"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/store/db"
	"github.com/accelbyte/extend-regional-payment-gateway/internal/store/docdb"
	"github.com/accelbyte/extend-regional-payment-gateway/pkg/common"
	pb "github.com/accelbyte/extend-regional-payment-gateway/pkg/pb"
	"github.com/accelbyte/extend-regional-payment-gateway/pkg/service"
)

const (
	grpcServerPort      = 6565
	grpcGatewayHTTPPort = 8000
	metricsPort         = 8080
	metricsEndpoint     = "/metrics"
)

var serviceName = "extend-regional-payment-gateway"

func main() {
	// ── Config ───────────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// ── Logger ───────────────────────────────────────────────────────────────
	logLevel := parseSlogLevel(cfg.LogLevel)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)
	slog.Info("config loaded", "summary", cfg.String())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── DocumentDB ───────────────────────────────────────────────────────────
	mongoClient, err := mongo.Connect(
		mongoOpts.Client().ApplyURI(cfg.DocDBURI()).SetRetryWrites(false),
	)
	if err != nil {
		slog.Error("failed to connect to DocumentDB", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := mongoClient.Disconnect(ctx); err != nil {
			slog.Error("failed to disconnect from DocumentDB", "error", err)
		}
	}()

	coll := mongoClient.Database(cfg.DocDBDatabaseName).Collection("transactions")

	if err := db.EnsureIndexes(ctx, coll); err != nil {
		slog.Error("failed to ensure indexes", "error", err)
		os.Exit(1)
	}

	txStore := docdb.New(coll)

	// ── AccelByte SDK ────────────────────────────────────────────────────────
	var tokenRepo repository.TokenRepository = sdkAuth.DefaultTokenRepositoryImpl()
	var configRepo repository.ConfigRepository = sdkAuth.DefaultConfigRepositoryImpl()
	var refreshRepo repository.RefreshTokenRepository = &sdkAuth.RefreshTokenImpl{RefreshRate: 0.8, AutoRefresh: true}

	oauthService := iam.OAuth20Service{
		Client:                 factory.NewIamClient(configRepo),
		TokenRepository:        tokenRepo,
		RefreshTokenRepository: refreshRepo,
		ConfigRepository:       configRepo,
	}

	clientID := configRepo.GetClientId()
	clientSecret := configRepo.GetClientSecret()
	if err := oauthService.LoginClient(&clientID, &clientSecret); err != nil {
		slog.Error("failed to login with M2M credentials", "error", err)
		os.Exit(1)
	}

	// ── AGS Services ─────────────────────────────────────────────────────────
	itemService := &platform.ItemService{
		Client:          factory.NewPlatformClient(configRepo),
		TokenRepository: tokenRepo,
	}
	fulfillmentService := &platform.FulfillmentService{
		Client:          factory.NewPlatformClient(configRepo),
		TokenRepository: tokenRepo,
	}
	walletService := &platform.WalletService{
		Client:          factory.NewPlatformClient(configRepo),
		TokenRepository: tokenRepo,
	}
	entitlementService := &platform.EntitlementService{
		Client:          factory.NewPlatformClient(configRepo),
		TokenRepository: tokenRepo,
	}

	fulfiller := fulfillment.NewFulfiller(fulfillmentService, walletService, entitlementService, cfg.ABNamespace)
	notifier := fulfillment.NewNotifier(cfg.ABNamespace)

	// ── Adapter Registry ─────────────────────────────────────────────────────
	registry := adapter.NewRegistry()
	for name, provCfg := range cfg.GenericProviders {
		a, adapterErr := generic.New(provCfg)
		if adapterErr != nil {
			slog.Error("failed to create generic adapter", "name", name, "error", adapterErr)
			os.Exit(1)
		}
		registry.Register(a)
		slog.Info("registered generic adapter", "provider", a.Name())
	}

	// ── DANA Adapter (conditional on DANA_PARTNER_ID being set) ─────────────
	if cfg.DANAConfig != nil {
		danaAdapter, adapterErr := dana.New(cfg.DANAConfig)
		if adapterErr != nil {
			slog.Error("failed to create DANA adapter", "error", adapterErr)
			os.Exit(1)
		}
		registry.Register(danaAdapter)
		slog.Info("registered DANA adapter", "provider", danaAdapter.Name())
	}

	// ── Services ─────────────────────────────────────────────────────────────
	if cfg.XenditConfig != nil {
		xenditAdapter, adapterErr := xendit.New(cfg.XenditConfig)
		if adapterErr != nil {
			slog.Error("failed to create Xendit adapter", "error", adapterErr)
			os.Exit(1)
		}
		registry.Register(xenditAdapter)
		slog.Info("registered Xendit adapter", "provider", xenditAdapter.Name())
	}

	if cfg.KomojuConfig != nil {
		komojuAdapter, adapterErr := komoju.New(cfg.KomojuConfig)
		if adapterErr != nil {
			slog.Error("failed to create KOMOJU adapter", "error", adapterErr)
			os.Exit(1)
		}
		registry.Register(komojuAdapter)
		slog.Info("registered KOMOJU adapter", "provider", komojuAdapter.Name())
	}

	paymentSvc := service.NewPaymentService(txStore, registry, itemService, cfg)
	webhookSvc := service.NewWebhookService(txStore, registry, fulfiller, fulfiller, notifier, cfg)
	adminSvc := service.NewAdminService(txStore, registry, fulfiller, cfg)
	publicSvc := service.NewPublicService(txStore, registry, fulfiller, cfg)
	schedulerSvc := service.NewSchedulerService(txStore, registry, fulfiller, notifier, cfg)

	// ── gRPC Server ──────────────────────────────────────────────────────────
	loggingOptions := []logging.Option{
		logging.WithLogOnEvents(logging.StartCall, logging.FinishCall),
		logging.WithFieldsFromContext(func(ctx context.Context) logging.Fields {
			if span := trace.SpanContextFromContext(ctx); span.IsSampled() {
				return logging.Fields{"traceID", span.TraceID().String()}
			}
			return nil
		}),
	}

	unaryInterceptors := []grpc.UnaryServerInterceptor{
		prometheusGrpc.UnaryServerInterceptor,
		logging.UnaryServerInterceptor(common.InterceptorLogger(logger), loggingOptions...),
	}
	streamInterceptors := []grpc.StreamServerInterceptor{
		prometheusGrpc.StreamServerInterceptor,
		logging.StreamServerInterceptor(common.InterceptorLogger(logger), loggingOptions...),
	}

	if cfg.PluginGRPCServerAuthEnabled {
		refreshInterval := 600
		common.Validator = common.NewTokenValidator(oauthService, time.Duration(refreshInterval)*time.Second, true)
		if err := common.Validator.Initialize(ctx); err != nil {
			slog.Warn("token validator initialization warning", "error", err)
		}
		unaryInterceptors = append(unaryInterceptors, common.NewUnaryAuthServerIntercept())
		streamInterceptors = append(streamInterceptors, common.NewStreamAuthServerIntercept())
		slog.Info("auth interceptors enabled")
	}

	grpcServer := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(unaryInterceptors...),
		grpc.ChainStreamInterceptor(streamInterceptors...),
	)

	pb.RegisterPaymentServiceServer(grpcServer, paymentSvc)
	pb.RegisterPublicServiceServer(grpcServer, publicSvc)
	pb.RegisterWebhookServiceServer(grpcServer, webhookSvc)
	pb.RegisterAdminServiceServer(grpcServer, adminSvc)
	reflection.Register(grpcServer)
	grpc_health_v1.RegisterHealthServer(grpcServer, health.NewServer())

	// ── gRPC-Gateway (HTTP) ──────────────────────────────────────────────────
	grpcGateway, err := common.NewGateway(ctx, fmt.Sprintf("localhost:%d", grpcServerPort), cfg.BasePath)
	if err != nil {
		slog.Error("failed to create gRPC-Gateway", "error", err)
		os.Exit(1)
	}

	checkoutStore := checkout.NewStore(ctx)
	checkoutHandler := checkout.NewHandler(checkoutStore, registry, paymentSvc, cfg.BasePath)

	go func() {
		httpServer := newHTTPServer(fmt.Sprintf(":%d", grpcGatewayHTTPPort), grpcGateway, webhookSvc, paymentSvc, publicSvc, adminSvc, registry, checkoutStore, checkoutHandler, cfg.BasePath, cfg.PublicBaseURL, logger)
		slog.Info("starting HTTP gateway", "port", grpcGatewayHTTPPort)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP gateway failed", "error", err)
			os.Exit(1)
		}
	}()

	// ── Prometheus ───────────────────────────────────────────────────────────
	prometheusGrpc.Register(grpcServer)
	prometheusRegistry := prometheus.NewRegistry()
	prometheusRegistry.MustRegister(
		prometheusCollectors.NewGoCollector(),
		prometheusCollectors.NewProcessCollector(prometheusCollectors.ProcessCollectorOpts{}),
		prometheusGrpc.DefaultServerMetrics,
	)
	go func() {
		http.Handle(metricsEndpoint, promhttp.HandlerFor(prometheusRegistry, promhttp.HandlerOpts{}))
		if err := http.ListenAndServe(fmt.Sprintf(":%d", metricsPort), nil); err != nil {
			slog.Error("metrics server failed", "error", err)
		}
	}()

	// ── OpenTelemetry ────────────────────────────────────────────────────────
	if val := common.GetEnv("OTEL_SERVICE_NAME", ""); val != "" {
		serviceName = "extend-app-" + strings.ToLower(val)
	}
	tracerProvider, err := common.NewTracerProvider(serviceName)
	if err != nil {
		slog.Error("failed to create tracer provider", "error", err)
		os.Exit(1)
	}
	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		b3.New(), propagation.TraceContext{}, propagation.Baggage{},
	))
	defer func() {
		if err := tracerProvider.Shutdown(ctx); err != nil {
			slog.Error("failed to shutdown tracer provider", "error", err)
		}
	}()

	// ── Recovery Scheduler ───────────────────────────────────────────────────
	schedulerSvc.Start(ctx)

	// ── gRPC Listener ────────────────────────────────────────────────────────
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", grpcServerPort))
	if err != nil {
		slog.Error("failed to listen on gRPC port", "port", grpcServerPort, "error", err)
		os.Exit(1)
	}
	go func() {
		slog.Info("starting gRPC server", "port", grpcServerPort)
		if err := grpcServer.Serve(lis); err != nil {
			slog.Error("gRPC server failed", "error", err)
			os.Exit(1)
		}
	}()

	slog.Info("service started", "service", serviceName)

	// ── Graceful Shutdown ─────────────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	slog.Info("shutdown signal received")
	grpcServer.GracefulStop()
}

// newHTTPServer builds the HTTP mux with:
//   - /v1/webhook/* handled directly (raw body preserved for signature validation)
//   - /v1/payment/* and /v1/admin/* handled directly (stub gRPC-Gateway has no routes)
//   - All other routes forwarded through gRPC-Gateway (real routes after make proto)
func newHTTPServer(addr string, gateway http.Handler, webhookSvc *service.WebhookService, paymentSvc *service.PaymentService, publicSvc *service.PublicService, adminSvc *service.AdminService, registry *adapter.Registry, checkoutStore *checkout.Store, checkoutHandler *checkout.Handler, basePath string, publicBaseURL string, logger *slog.Logger) *http.Server {
	mux := http.NewServeMux()

	// Direct handlers — active until `make proto` replaces the stub pb package
	common.RegisterDirectHandlers(mux, basePath, paymentSvc, publicSvc, adminSvc, checkoutStore, publicBaseURL)

	webhookPath := basePath + "/v1/webhook/"

	// Webhook handler: intercepts before gRPC-Gateway to preserve raw bytes
	mux.HandleFunc(webhookPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Cap body at 1 MB (DoS prevention)
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
			return
		}

		// Extract provider name from URL path after /v1/webhook/
		providerName := strings.TrimPrefix(r.URL.Path, webhookPath)
		if providerName == "" {
			http.Error(w, "provider name required in path", http.StatusBadRequest)
			return
		}

		// Convert HTTP headers to lowercase map for consistent lookup
		headers := make(map[string]string, len(r.Header))
		for k, v := range r.Header {
			if len(v) > 0 {
				headers[strings.ToLower(k)] = v[0]
			}
		}

		// Call webhook service directly — bypasses gRPC-Gateway JSON decoding
		resp, svcErr := webhookSvc.HandleWebhook(r.Context(), &pb.WebhookRequest{
			ProviderName: providerName,
			RawPayload:   rawBody,
			Headers:      headers,
		})

		w.Header().Set("Content-Type", "application/json")
		if svcErr != nil {
			w.WriteHeader(grpcStatusToHTTP(svcErr))
			if prov, lookupErr := registry.Get(providerName); lookupErr == nil {
				if errAcker, ok := prov.(adapter.WebhookErrorAcknowledger); ok {
					w.Write(errAcker.WebhookErrorAckBody()) //nolint:errcheck
					return
				}
			}
			json.NewEncoder(w).Encode(map[string]string{"message": svcErr.Error()})
			return
		}
		w.WriteHeader(http.StatusOK)
		// If the provider requires a custom ack body (e.g. DANA expects a specific responseCode),
		// write it instead of the default gRPC response encoding.
		if prov, lookupErr := registry.Get(providerName); lookupErr == nil {
			if acker, ok := prov.(adapter.WebhookAcknowledger); ok {
				w.Write(acker.WebhookAckBody()) //nolint:errcheck
				return
			}
		}
		if resp != nil {
			json.NewEncoder(w).Encode(resp)
		}
	})

	// Hosted checkout: provider selection page + provider select handler
	checkoutPrefix := basePath + "/checkout/"
	mux.HandleFunc(checkoutPrefix, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/select") {
			checkoutHandler.HandleProviderSelect(w, r)
		} else if strings.HasSuffix(r.URL.Path, "/cancel-selected-provider") {
			checkoutHandler.HandleCancelSelectedProvider(w, r)
		} else if strings.HasSuffix(r.URL.Path, "/cancel") {
			checkoutHandler.HandleCancel(w, r)
		} else {
			checkoutHandler.HandleCheckoutPage(w, r)
		}
	})

	// Payment result landing page — browser redirect destination after DANA payment
	mux.HandleFunc(basePath+"/payment-result/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		transactionID := r.URL.Query().Get("transactionId")
		if transactionID == "" {
			http.Error(w, "transactionId is required", http.StatusBadRequest)
			return
		}
		tx, err := paymentSvc.GetTransaction(r.Context(), &pb.GetTransactionRequest{TransactionId: transactionID})
		if err != nil {
			http.Error(w, "transaction not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(toPaymentResultStatus(tx)) //nolint:errcheck
	})

	mux.HandleFunc(basePath+"/payment-result", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(renderPaymentResultPage(basePath, r.URL.Query().Get("transactionId")))) //nolint:errcheck
	})

	// All other routes through gRPC-Gateway
	mux.Handle("/", gateway)

	// Swagger UI
	swaggerUIDir := "third_party/swagger-ui"
	swaggerUiPath := basePath + "/apidocs/"
	mux.Handle(swaggerUiPath, http.StripPrefix(swaggerUiPath, http.FileServer(http.Dir(swaggerUIDir))))

	// Swagger JSON
	mux.HandleFunc(basePath+"/apidocs/api.json", func(w http.ResponseWriter, r *http.Request) {
		matches, err := filepath.Glob("gateway/apidocs/*.swagger.json")
		if err != nil || len(matches) == 0 {
			http.Error(w, "swagger spec not found", http.StatusInternalServerError)
			return
		}
		swagger, err := loads.Spec(matches[0])
		if err != nil {
			http.Error(w, "failed to parse swagger spec", http.StatusInternalServerError)
			return
		}
		swagger.Spec().BasePath = basePath
		raw, _ := swagger.Spec().MarshalJSON()
		var pretty bytes.Buffer
		json.Indent(&pretty, raw, "", "  ")
		w.Header().Set("Content-Type", "application/json")
		w.Write(pretty.Bytes())
	})

	loggedMux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		mux.ServeHTTP(w, r)
		logger.Info("HTTP request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})

	return &http.Server{
		Addr:     addr,
		Handler:  loggedMux,
		ErrorLog: log.New(os.Stderr, "httpSrv: ", log.LstdFlags),
	}
}

type paymentResultStatusResponse struct {
	TransactionID string `json:"transactionId"`
	ItemName      string `json:"itemName,omitempty"`
	Quantity      int32  `json:"quantity"`
	AmountValue   int64  `json:"amountValue"`
	CurrencyCode  string `json:"currencyCode"`
	Amount        string `json:"amount"`
	Provider      string `json:"provider,omitempty"`
	Status        string `json:"status"`
}

func toPaymentResultStatus(tx *pb.TransactionResponse) paymentResultStatusResponse {
	if tx == nil {
		return paymentResultStatusResponse{}
	}
	itemName := tx.ItemName
	if itemName == "" {
		itemName = tx.ItemId
	}
	return paymentResultStatusResponse{
		TransactionID: tx.TransactionId,
		ItemName:      itemName,
		Quantity:      tx.Quantity,
		AmountValue:   tx.Amount,
		CurrencyCode:  tx.CurrencyCode,
		Amount:        formatResultCurrencyAmount(tx.Amount, tx.CurrencyCode),
		Provider:      resultProviderName(tx.Provider, tx.CustomProviderName),
		Status:        tx.Status.String(),
	}
}

func resultProviderName(provider pb.Provider, customProviderName string) string {
	switch provider {
	case pb.Provider_PROVIDER_DANA:
		return "DANA"
	case pb.Provider_PROVIDER_XENDIT:
		return "XENDIT"
	case pb.Provider_PROVIDER_KOMOJU:
		return "KOMOJU"
	case pb.Provider_PROVIDER_CUSTOM:
		if customProviderName != "" {
			return displayResultName(customProviderName)
		}
		return "Custom Provider"
	default:
		return ""
	}
}

func displayResultName(value string) string {
	value = strings.TrimPrefix(value, "generic_")
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == '_' || r == '-' })
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}

func formatResultCurrencyAmount(amount int64, currencyCode string) string {
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

func renderPaymentResultPage(basePath string, transactionID string) string {
	basePathJSON, _ := json.Marshal(basePath)
	transactionIDJSON, _ := json.Marshal(transactionID)
	hasTransaction := transactionID != ""
	statusClass := "status-pending"
	title := "Payment Processing"
	message := "Your payment is being verified. Your item will be granted shortly."
	if !hasTransaction {
		statusClass = "status-missing"
		message = "We are waiting for the payment provider to confirm your payment."
	}
	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Payment Result</title>
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
      --success: #0A8F5A;
      --danger: #C73535;
      --white: #FFFFFF;
    }
    body {
      min-height: 100vh;
      margin: 0;
      font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      color: var(--ab-text);
      background:
        radial-gradient(circle at top left, rgba(0, 109, 255, .16), transparent 34rem),
        linear-gradient(135deg, #F8FBFF 0%%, #EEF6FF 100%%);
    }
    .page { min-height: 100vh; display: flex; align-items: center; justify-content: center; padding: 32px 18px; }
    .result-shell {
      width: min(760px, 100%%);
      overflow: hidden;
      border: 1px solid rgba(0, 109, 255, .12);
      border-radius: 8px;
      background: var(--white);
      box-shadow: 0 24px 70px rgba(7, 26, 58, .14);
    }
    .hero {
      padding: 32px;
      color: var(--white);
      background: linear-gradient(160deg, var(--ab-navy) 0%%, var(--ab-blue-dark) 58%%, var(--ab-blue) 100%%);
    }
    .eyebrow { margin: 0 0 8px; color: #BFD9FF; font-size: 12px; font-weight: 750; text-transform: uppercase; }
    h1 { margin: 0 0 10px; color: var(--white); font-size: 30px; line-height: 1.2; }
    .message { max-width: 560px; margin: 0; color: #D8E8FF; font-size: 15px; line-height: 1.6; }
    .content { padding: 28px 32px 32px; }
    .status-line { display: flex; align-items: center; gap: 12px; margin-bottom: 22px; color: var(--ab-muted); font-size: 14px; font-weight: 700; }
    .status-dot { width: 12px; height: 12px; border-radius: 999px; background: var(--ab-blue); box-shadow: 0 0 0 5px rgba(0, 109, 255, .12); }
    .status-success .status-dot { background: var(--success); box-shadow: 0 0 0 5px rgba(10, 143, 90, .12); }
    .status-failed .status-dot { background: var(--danger); box-shadow: 0 0 0 5px rgba(199, 53, 53, .12); }
    .status-canceled .status-dot, .status-expired .status-dot { background: #7A4F01; box-shadow: 0 0 0 5px rgba(122, 79, 1, .12); }
    .status-missing .status-dot { background: #7A8CA8; box-shadow: 0 0 0 5px rgba(122, 140, 168, .14); }
    .details { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 12px; margin-bottom: 22px; }
    .detail { min-height: 82px; padding: 16px; border: 1px solid var(--ab-line); border-radius: 8px; background: #F8FBFF; }
    .label { display: block; margin-bottom: 6px; color: var(--ab-muted); font-size: 12px; font-weight: 750; text-transform: uppercase; }
    .value { color: var(--ab-navy); font-size: 16px; font-weight: 400; overflow-wrap: anywhere; }
    .hidden { display: none; }
    .footer-note { margin: 0; color: var(--ab-muted); font-size: 14px; line-height: 1.6; }
    @media (max-width: 640px) {
      .page { align-items: stretch; padding: 0; }
      .result-shell { min-height: 100vh; border: 0; border-radius: 0; box-shadow: none; }
      .hero, .content { padding: 26px 20px; }
      .details { grid-template-columns: 1fr; }
      h1 { font-size: 26px; }
    }
  </style>
</head>
<body>
  <main class="page">
    <section class="result-shell %s" id="resultShell">
      <div class="hero">
        <p class="eyebrow">AccelByte Payment</p>
        <h1 id="title">%s</h1>
        <p class="message" id="message">%s</p>
      </div>
      <div class="content">
        <div class="status-line">
          <span class="status-dot" aria-hidden="true"></span>
          <span id="statusText">%s</span>
        </div>
        <div class="details %s" id="details">
          <div class="detail"><span class="label">Item</span><span class="value" id="itemName">-</span></div>
          <div class="detail"><span class="label">Amount</span><span class="value" id="amount">-</span></div>
          <div class="detail"><span class="label">Quantity</span><span class="value" id="quantity">-</span></div>
          <div class="detail"><span class="label">Provider</span><span class="value" id="provider">-</span></div>
          <div class="detail"><span class="label">Transaction ID</span><span class="value" id="transactionId">%s</span></div>
        </div>
        <p class="footer-note" id="footerNote">You may close this window after your payment is confirmed.</p>
      </div>
    </section>
  </main>
  <script>
    const basePath = %s;
    const transactionId = %s;
    const shell = document.getElementById('resultShell');
    const title = document.getElementById('title');
    const message = document.getElementById('message');
    const statusText = document.getElementById('statusText');
    const footerNote = document.getElementById('footerNote');
    const details = document.getElementById('details');
    const fields = {
      itemName: document.getElementById('itemName'),
      amount: document.getElementById('amount'),
      quantity: document.getElementById('quantity'),
      provider: document.getElementById('provider'),
      transactionId: document.getElementById('transactionId')
    };
    function setState(status) {
      shell.classList.remove('status-pending', 'status-success', 'status-failed', 'status-canceled', 'status-expired', 'status-missing');
      if (status === 'FULFILLED') {
        shell.classList.add('status-success');
        title.textContent = 'Payment Confirmed';
        message.textContent = 'Your payment has been verified and your item is being granted.';
        statusText.textContent = 'Fulfilled';
        footerNote.textContent = 'You may close this window.';
        return true;
      }
      if (status === 'FAILED') {
        shell.classList.add('status-failed');
        title.textContent = 'Payment Failed';
        message.textContent = 'The payment provider reported that this payment could not be completed.';
        statusText.textContent = 'Failed';
        footerNote.textContent = 'You may close this window and try again from the game.';
        return true;
      }
      if (status === 'CANCELED') {
        shell.classList.add('status-canceled');
        title.textContent = 'Payment Canceled';
        message.textContent = 'This payment was canceled before it was completed.';
        statusText.textContent = 'Canceled';
        footerNote.textContent = 'You may close this window and return to the game.';
        return true;
      }
      if (status === 'EXPIRED') {
        shell.classList.add('status-expired');
        title.textContent = 'Payment Expired';
        message.textContent = 'The payment window expired before the payment was completed.';
        statusText.textContent = 'Expired';
        footerNote.textContent = 'You may close this window and start a new purchase from the game.';
        return true;
      }
      shell.classList.add('status-pending');
      title.textContent = 'Payment Processing';
      message.textContent = 'Your payment is being verified. Your item will be granted shortly.';
      statusText.textContent = status === 'FULFILLING' ? 'Granting item' : 'Processing';
      footerNote.textContent = 'This page will refresh the payment status for a short time.';
      return false;
    }
    async function refreshStatus() {
      const response = await fetch(basePath + '/payment-result/status?transactionId=' + encodeURIComponent(transactionId), { headers: { 'Accept': 'application/json' } });
      if (!response.ok) { throw new Error('status request failed'); }
      const data = await response.json();
      details.classList.remove('hidden');
      fields.itemName.textContent = data.itemName || '-';
      fields.amount.textContent = data.amount || '-';
      fields.quantity.textContent = data.quantity ? String(data.quantity) : '-';
      fields.provider.textContent = data.provider || '-';
      fields.transactionId.textContent = data.transactionId || transactionId;
      return setState(data.status);
    }
    if (transactionId) {
      let attempts = 0;
      const maxAttempts = 12;
      const run = async () => {
        attempts += 1;
        try {
          const terminal = await refreshStatus();
          if (terminal || attempts >= maxAttempts) { return; }
        } catch (error) {
          if (attempts >= maxAttempts) {
            footerNote.textContent = 'We could not refresh the latest status. You may close this window.';
            return;
          }
        }
        window.setTimeout(run, 2500);
      };
      run();
    }
  </script>
</body>
</html>`, statusClass, template.HTMLEscapeString(title), template.HTMLEscapeString(message), template.HTMLEscapeString(title), hiddenClass(!hasTransaction), template.HTMLEscapeString(transactionID), string(basePathJSON), string(transactionIDJSON))
}

func hiddenClass(hidden bool) string {
	if hidden {
		return "hidden"
	}
	return ""
}

func grpcStatusToHTTP(err error) int {
	if err == nil {
		return http.StatusOK
	}
	msg := err.Error()
	if strings.Contains(msg, "InvalidArgument") || strings.Contains(msg, "code = InvalidArgument") {
		return http.StatusBadRequest
	}
	if strings.Contains(msg, "NotFound") || strings.Contains(msg, "code = NotFound") {
		return http.StatusNotFound
	}
	if strings.Contains(msg, "FailedPrecondition") || strings.Contains(msg, "code = FailedPrecondition") {
		return http.StatusConflict
	}
	if strings.Contains(msg, "Unauthenticated") {
		return http.StatusUnauthorized
	}
	return http.StatusInternalServerError
}

func parseSlogLevel(levelStr string) slog.Level {
	switch strings.ToLower(levelStr) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
