package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	dana "github.com/accelbyte/extend-regional-payment-gateway/internal/adapter/dana"
	komoju "github.com/accelbyte/extend-regional-payment-gateway/internal/adapter/komoju"
	xendit "github.com/accelbyte/extend-regional-payment-gateway/internal/adapter/xendit"
)

var genericNameRegex = regexp.MustCompile(`^GENERIC_([A-Z0-9_]+)_AUTH_HEADER$`)

// GenericProviderConfig holds all env-var configuration for one Generic HTTP provider.
type GenericProviderConfig struct {
	Name string

	// Auth
	AuthHeader string
	AuthValue  string

	// Payment intent
	CreateIntentURL          string
	CreateIntentBodyTemplate string
	PaymentURLJSONPath       string // redirect URL (e.g. deep-link for DANA)
	QRCodeDataJSONPath       string // QR string for QRIS-based providers (optional)
	ProviderTxIDJSONPath     string

	// Status query (optional — if empty, GetPaymentStatus returns ErrNotSupported)
	StatusURLTemplate        string
	StatusMethod             string
	StatusPaymentStatusPath  string
	StatusSuccessValue       string
	StatusPendingValue       string
	StatusFailedValues       []string // comma-split from env var
	StatusRefundValue        string
	StatusRefundAmountPath   string
	StatusRefundCurrencyPath string

	// Webhook
	WebhookSignatureMethod    string // HMAC_SHA256 | HMAC_SHA512 | NONE
	WebhookSignatureSecret    string
	WebhookSignatureHeader    string
	WebhookTxIDJSONPath       string
	WebhookSuccessStatusPath  string
	WebhookSuccessStatusValue string
	WebhookFailedStatusValue  string
	// Optional: replay prevention
	WebhookTimestampJSONPath string
	WebhookTimestampFormat   string

	// Refund
	RefundURL          string
	RefundBodyTemplate string

	// Cancel (optional)
	CancelURLTemplate  string
	CancelMethod       string
	CancelBodyTemplate string
	CancelStatusPath   string
	CancelSuccessValues []string
	CancelExpiredValues []string
	CancelPaidValues    []string
	CancelPendingValues []string
}

type Config struct {
	// AccelByte M2M
	ABBaseURL      string
	ABClientID     string
	ABClientSecret string
	ABNamespace    string
	PublicBaseURL  string

	// DocumentDB (auto-injected by Extend platform)
	DocDBHost         string
	DocDBUsername     string
	DocDBPassword     string
	DocDBCAFilePath   string
	DocDBDatabaseName string

	// Generic providers discovered at startup
	GenericProviders map[string]*GenericProviderConfig

	// DANAConfig is non-nil when DANA_PARTNER_ID is set in the environment.
	DANAConfig *dana.Config

	// XenditConfig is non-nil when XENDIT_SECRET_API_KEY is set in the environment.
	XenditConfig *xendit.Config

	// KomojuConfig is non-nil when KOMOJU_SECRET_KEY is set in the environment.
	KomojuConfig *komoju.Config

	// Ports
	GRPCPort    int
	HTTPPort    int
	MetricsPort int

	// Tuning
	PaymentExpiryDefault        time.Duration
	RecordRetentionDays         int
	MaxConcurrentIntentPerUser  int
	PublicSyncCooldown          time.Duration
	PublicSyncDefaultPageSize   int32
	PublicSyncMaxPageSize       int32
	WebhookMaxAge               time.Duration
	MaxRetries                  int
	LogLevel                    string
	BasePath                    string
	PluginGRPCServerAuthEnabled bool

	// WebhookForceError makes every incoming webhook return a 5005601 error response
	// without processing fulfillment. Set WEBHOOK_FORCE_ERROR=true during DANA
	// certification to satisfy the "simulate internal server error" scenario, then
	// unset it so DANA's retry receives the normal 2005600 success response.
	WebhookForceError bool
}

func Load() (*Config, error) {
	cfg := &Config{
		ABBaseURL:                   requireEnv("AB_BASE_URL"),
		ABClientID:                  requireEnv("AB_CLIENT_ID"),
		ABClientSecret:              requireEnv("AB_CLIENT_SECRET"),
		ABNamespace:                 getEnv("AB_NAMESPACE", "accelbyte"),
		PublicBaseURL:               getEnv("PUBLIC_BASE_URL", requireEnv("AB_BASE_URL")),
		DocDBHost:                   getEnv("DOCDB_HOST", ""),
		DocDBUsername:               getEnv("DOCDB_USERNAME", ""),
		DocDBPassword:               getEnv("DOCDB_PASSWORD", ""),
		DocDBCAFilePath:             getEnv("DOCDB_CA_CERT_FILE_PATH", ""),
		DocDBDatabaseName:           getEnv("DOCDB_DATABASE_NAME", "payment"),
		GRPCPort:                    getEnvInt("GRPC_PORT", 6565),
		HTTPPort:                    getEnvInt("HTTP_PORT", 8000),
		MetricsPort:                 getEnvInt("METRICS_PORT", 8080),
		PaymentExpiryDefault:        getEnvDuration("PAYMENT_EXPIRY_DEFAULT", 15*time.Minute),
		RecordRetentionDays:         getEnvInt("RECORD_RETENTION_DAYS", 90),
		MaxConcurrentIntentPerUser:  getEnvInt("MAX_CONCURRENT_INTENT_PER_USER", 5),
		PublicSyncCooldown:          getEnvDuration("PUBLIC_SYNC_COOLDOWN", 60*time.Second),
		PublicSyncDefaultPageSize:   int32(getEnvInt("PUBLIC_SYNC_DEFAULT_PAGE_SIZE", 10)),
		PublicSyncMaxPageSize:       int32(getEnvInt("PUBLIC_SYNC_MAX_PAGE_SIZE", 20)),
		WebhookMaxAge:               getEnvDuration("WEBHOOK_MAX_AGE", 5*time.Minute),
		MaxRetries:                  getEnvInt("MAX_RETRIES", 3),
		LogLevel:                    getEnv("LOG_LEVEL", "info"),
		BasePath:                    getEnv("BASE_PATH", "/payment"),
		PluginGRPCServerAuthEnabled: strings.ToLower(getEnv("PLUGIN_GRPC_SERVER_AUTH_ENABLED", "true")) == "true",
		WebhookForceError:           strings.ToLower(getEnv("WEBHOOK_FORCE_ERROR", "false")) == "true",
	}

	providers, err := discoverGenericProviders()
	if err != nil {
		return nil, err
	}
	cfg.GenericProviders = providers

	danaConfig, err := dana.Load()
	if err != nil {
		return nil, fmt.Errorf("DANA config: %w", err)
	}
	cfg.DANAConfig = danaConfig

	xenditConfig, err := xendit.Load()
	if err != nil {
		return nil, fmt.Errorf("Xendit config: %w", err)
	}
	cfg.XenditConfig = xenditConfig

	komojuConfig, err := komoju.Load()
	if err != nil {
		return nil, fmt.Errorf("KOMOJU config: %w", err)
	}
	cfg.KomojuConfig = komojuConfig

	return cfg, nil
}

func discoverGenericProviders() (map[string]*GenericProviderConfig, error) {
	seen := make(map[string]struct{})
	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) != 2 {
			continue
		}
		if m := genericNameRegex.FindStringSubmatch(parts[0]); m != nil {
			seen[strings.ToLower(m[1])] = struct{}{}
		}
	}

	providers := make(map[string]*GenericProviderConfig, len(seen))
	for name := range seen {
		p, err := loadGenericProvider(name)
		if err != nil {
			return nil, fmt.Errorf("generic provider %q: %w", name, err)
		}
		providers[name] = p
	}
	return providers, nil
}

func loadGenericProvider(name string) (*GenericProviderConfig, error) {
	prefix := "GENERIC_" + strings.ToUpper(name) + "_"

	req := func(key string) (string, error) {
		v := os.Getenv(prefix + key)
		if v == "" {
			return "", fmt.Errorf("required env var %s%s is not set", prefix, key)
		}
		return v, nil
	}
	opt := func(key string) string {
		return os.Getenv(prefix + key)
	}

	authHeader, err := req("AUTH_HEADER")
	if err != nil {
		return nil, err
	}
	authValue, err := req("AUTH_VALUE")
	if err != nil {
		return nil, err
	}
	createURL, err := req("CREATE_INTENT_URL")
	if err != nil {
		return nil, err
	}
	createTmpl, err := req("CREATE_INTENT_BODY_TEMPLATE")
	if err != nil {
		return nil, err
	}
	paymentURLPath, err := req("PAYMENT_URL_JSON_PATH")
	if err != nil {
		return nil, err
	}
	providerTxIDPath, err := req("PROVIDER_TX_ID_JSON_PATH")
	if err != nil {
		return nil, err
	}
	webhookSigMethod, err := req("WEBHOOK_SIGNATURE_METHOD")
	if err != nil {
		return nil, err
	}
	webhookSigHeader, err := req("WEBHOOK_SIGNATURE_HEADER")
	if err != nil {
		return nil, err
	}
	webhookTxIDPath, err := req("WEBHOOK_TX_ID_JSON_PATH")
	if err != nil {
		return nil, err
	}
	webhookSuccessPath, err := req("WEBHOOK_SUCCESS_STATUS_PATH")
	if err != nil {
		return nil, err
	}
	webhookSuccessVal, err := req("WEBHOOK_SUCCESS_STATUS_VALUE")
	if err != nil {
		return nil, err
	}
	webhookFailedVal, err := req("WEBHOOK_FAILED_STATUS_VALUE")
	if err != nil {
		return nil, err
	}
	refundURL, err := req("REFUND_URL")
	if err != nil {
		return nil, err
	}
	refundTmpl, err := req("REFUND_BODY_TEMPLATE")
	if err != nil {
		return nil, err
	}

	// Signature secret required unless NONE
	webhookSigSecret := opt("WEBHOOK_SIGNATURE_SECRET")
	if strings.ToUpper(webhookSigMethod) != "NONE" && webhookSigSecret == "" {
		return nil, fmt.Errorf("required env var %sWEBHOOK_SIGNATURE_SECRET is not set (required when method != NONE)", prefix)
	}

	// Strip leading $. from JSON paths (gjson uses data.field, not $.data.field)
	paymentURLPath = stripJSONPathPrefix(paymentURLPath)
	providerTxIDPath = stripJSONPathPrefix(providerTxIDPath)
	qrCodeDataPath := stripJSONPathPrefix(opt("QR_CODE_DATA_JSON_PATH"))
	webhookTxIDPath = stripJSONPathPrefix(webhookTxIDPath)
	webhookSuccessPath = stripJSONPathPrefix(webhookSuccessPath)

	var statusFailedValues []string
	if raw := opt("STATUS_FAILED_VALUE"); raw != "" {
		for _, v := range strings.Split(raw, ",") {
			statusFailedValues = append(statusFailedValues, strings.TrimSpace(v))
		}
	}

	statusPaymentStatusPath := opt("STATUS_PAYMENT_STATUS_PATH")
	if statusPaymentStatusPath != "" {
		statusPaymentStatusPath = stripJSONPathPrefix(statusPaymentStatusPath)
	}
	statusRefundAmountPath := opt("STATUS_REFUND_AMOUNT_PATH")
	if statusRefundAmountPath != "" {
		statusRefundAmountPath = stripJSONPathPrefix(statusRefundAmountPath)
	}
	statusRefundCurrencyPath := opt("STATUS_REFUND_CURRENCY_PATH")
	if statusRefundCurrencyPath != "" {
		statusRefundCurrencyPath = stripJSONPathPrefix(statusRefundCurrencyPath)
	}
	cancelStatusPath := opt("CANCEL_STATUS_PATH")
	if cancelStatusPath != "" {
		cancelStatusPath = stripJSONPathPrefix(cancelStatusPath)
	}

	return &GenericProviderConfig{
		Name:                      name,
		AuthHeader:                authHeader,
		AuthValue:                 authValue,
		CreateIntentURL:           createURL,
		CreateIntentBodyTemplate:  createTmpl,
		PaymentURLJSONPath:        paymentURLPath,
		QRCodeDataJSONPath:        qrCodeDataPath,
		ProviderTxIDJSONPath:      providerTxIDPath,
		StatusURLTemplate:         opt("STATUS_URL_TEMPLATE"),
		StatusMethod:              getEnvDefault(opt("STATUS_METHOD"), "GET"),
		StatusPaymentStatusPath:   statusPaymentStatusPath,
		StatusSuccessValue:        opt("STATUS_SUCCESS_VALUE"),
		StatusPendingValue:        opt("STATUS_PENDING_VALUE"),
		StatusFailedValues:        statusFailedValues,
		StatusRefundValue:         opt("STATUS_REFUND_VALUE"),
		StatusRefundAmountPath:    statusRefundAmountPath,
		StatusRefundCurrencyPath:  statusRefundCurrencyPath,
		WebhookSignatureMethod:    webhookSigMethod,
		WebhookSignatureSecret:    webhookSigSecret,
		WebhookSignatureHeader:    strings.ToLower(webhookSigHeader),
		WebhookTxIDJSONPath:       webhookTxIDPath,
		WebhookSuccessStatusPath:  webhookSuccessPath,
		WebhookSuccessStatusValue: webhookSuccessVal,
		WebhookFailedStatusValue:  webhookFailedVal,
		WebhookTimestampJSONPath:  opt("WEBHOOK_TIMESTAMP_JSON_PATH"),
		WebhookTimestampFormat:    opt("WEBHOOK_TIMESTAMP_FORMAT"),
		RefundURL:                 refundURL,
		RefundBodyTemplate:        refundTmpl,
		CancelURLTemplate:         opt("CANCEL_URL_TEMPLATE"),
		CancelMethod:              getEnvDefault(opt("CANCEL_METHOD"), "POST"),
		CancelBodyTemplate:        opt("CANCEL_BODY_TEMPLATE"),
		CancelStatusPath:          cancelStatusPath,
		CancelSuccessValues:       splitCSV(opt("CANCEL_SUCCESS_VALUES")),
		CancelExpiredValues:       splitCSV(opt("CANCEL_EXPIRED_VALUES")),
		CancelPaidValues:          splitCSV(opt("CANCEL_PAID_VALUES")),
		CancelPendingValues:       splitCSV(opt("CANCEL_PENDING_VALUES")),
	}, nil
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := []string{}
	for _, v := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func stripJSONPathPrefix(path string) string {
	return strings.TrimPrefix(path, "$.")
}

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		// Non-fatal here — caller can decide. For required M2M vars we panic early.
		return ""
	}
	return v
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getEnvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func getEnvDefault(val, def string) string {
	if val == "" {
		return def
	}
	return val
}

// DocDBURI builds the MongoDB connection URI for DocumentDB.
func (c *Config) DocDBURI() string {
	if c.DocDBHost == "" {
		// Local dev fallback — plain MongoDB without TLS
		return "mongodb://localhost:27017"
	}
	uri := fmt.Sprintf(
		"mongodb://%s:%s@%s/%s?serverSelectionTimeoutMS=10000&tls=true&tlsAllowInvalidHostnames=true",
		c.DocDBUsername, c.DocDBPassword, c.DocDBHost, c.DocDBDatabaseName,
	)
	if c.DocDBCAFilePath != "" {
		uri += "&tlsCAFile=" + c.DocDBCAFilePath
	}
	return uri
}

// String returns a redacted representation safe for logging.
func (c *Config) String() string {
	redact := "[REDACTED]"
	lines := []string{
		fmt.Sprintf("AB_BASE_URL=%s", c.ABBaseURL),
		fmt.Sprintf("AB_CLIENT_ID=%s", c.ABClientID),
		fmt.Sprintf("AB_CLIENT_SECRET=%s", redact),
		fmt.Sprintf("AB_NAMESPACE=%s", c.ABNamespace),
		fmt.Sprintf("PUBLIC_BASE_URL=%s", c.PublicBaseURL),
		fmt.Sprintf("DOCDB_HOST=%s", c.DocDBHost),
		fmt.Sprintf("DOCDB_USERNAME=%s", c.DocDBUsername),
		fmt.Sprintf("DOCDB_PASSWORD=%s", redact),
		fmt.Sprintf("DOCDB_DATABASE_NAME=%s", c.DocDBDatabaseName),
		fmt.Sprintf("GRPC_PORT=%d", c.GRPCPort),
		fmt.Sprintf("HTTP_PORT=%d", c.HTTPPort),
		fmt.Sprintf("PAYMENT_EXPIRY_DEFAULT=%s", c.PaymentExpiryDefault),
		fmt.Sprintf("MAX_CONCURRENT_INTENT_PER_USER=%d", c.MaxConcurrentIntentPerUser),
		fmt.Sprintf("GENERIC_PROVIDERS=%v", c.genericProviderNames()),
		fmt.Sprintf("XENDIT_ENABLED=%t", c.XenditConfig != nil),
		fmt.Sprintf("KOMOJU_ENABLED=%t", c.KomojuConfig != nil),
	}
	return strings.Join(lines, "\n")
}

func (c *Config) genericProviderNames() []string {
	names := make([]string, 0, len(c.GenericProviders))
	for n := range c.GenericProviders {
		names = append(names, n)
	}
	return names
}
