package komoju

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

const (
	defaultAPIBaseURL    = "https://komoju.com"
	defaultAPIVersion    = "2025-01-28"
	defaultLocale        = "en"
	defaultCurrenciesCSV = "JPY,USD,EUR,TWD,KRW,PLN,GBP,HKD,SGD,NZD,AUD,IDR,MYR,PHP,THB,CNY,BRL,CHF,CAD,VND"
)

// Config holds KOMOJU adapter configuration loaded from environment variables.
type Config struct {
	ProviderID        string
	DisplayName       string
	SecretKey         string
	WebhookSecret     string
	APIVersion        string
	APIBaseURL        string
	DefaultLocale     string
	AllowedCurrencies map[string]struct{}
}

// Load reads KOMOJU config from environment variables.
// Returns (nil, nil) when KOMOJU_SECRET_KEY is unset.
func Load() (*Config, error) {
	secret := strings.TrimSpace(os.Getenv("KOMOJU_SECRET_KEY"))
	if secret == "" {
		return nil, nil
	}

	webhookSecret := strings.TrimSpace(os.Getenv("KOMOJU_WEBHOOK_SECRET"))
	if webhookSecret == "" {
		return nil, fmt.Errorf("KOMOJU_WEBHOOK_SECRET is required when KOMOJU_SECRET_KEY is set")
	}

	return &Config{
		ProviderID:        getEnv("KOMOJU_PROVIDER_ID", "provider_komoju"),
		DisplayName:       getEnv("KOMOJU_DISPLAY_NAME", "KOMOJU"),
		SecretKey:         secret,
		WebhookSecret:     webhookSecret,
		APIVersion:        getEnv("KOMOJU_API_VERSION", defaultAPIVersion),
		APIBaseURL:        strings.TrimRight(getEnv("KOMOJU_BASE_URL", defaultAPIBaseURL), "/"),
		DefaultLocale:     normalizeLocale(getEnv("KOMOJU_DEFAULT_LOCALE", defaultLocale)),
		AllowedCurrencies: parseCodeSet(getEnv("KOMOJU_ALLOWED_CURRENCIES", defaultCurrenciesCSV)),
	}, nil
}

func getEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func parseCodeSet(raw string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		code := strings.ToUpper(strings.TrimSpace(part))
		if code != "" {
			out[code] = struct{}{}
		}
	}
	return out
}

func sortedCodes(codes map[string]struct{}) []string {
	out := make([]string, 0, len(codes))
	for code := range codes {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

func normalizeLocale(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "ja", "en", "ko":
		return value
	default:
		return defaultLocale
	}
}
