package xendit

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

const (
	defaultCountry    = "ID"
	defaultCountries  = "ID,PH,VN,TH,SG,MY,HK,MX"
	defaultCurrencies = "IDR,PHP,VND,THB,SGD,MYR,USD,HKD,AUD,GBP,EUR,JPY,MXN"
)

// Config holds Xendit adapter configuration loaded from environment variables.
type Config struct {
	ProviderID        string
	DisplayName       string
	SecretAPIKey      string
	CallbackToken     string
	APIBaseURL        string
	DefaultCountry    string
	AllowedCountries  map[string]struct{}
	AllowedCurrencies map[string]struct{}
}

// Load reads Xendit config from environment variables.
// Returns (nil, nil) when XENDIT_SECRET_API_KEY is unset.
func Load() (*Config, error) {
	secret := os.Getenv("XENDIT_SECRET_API_KEY")
	if secret == "" {
		return nil, nil
	}

	callbackToken := os.Getenv("XENDIT_CALLBACK_TOKEN")
	if callbackToken == "" {
		return nil, fmt.Errorf("XENDIT_CALLBACK_TOKEN is required when XENDIT_SECRET_API_KEY is set")
	}

	defaultCountry := normalizeCode(getEnv("XENDIT_DEFAULT_COUNTRY", defaultCountry))
	allowedCountries := parseCodeSet(getEnv("XENDIT_ALLOWED_COUNTRIES", defaultCountries))
	allowedCurrencies := parseCodeSet(getEnv("XENDIT_ALLOWED_CURRENCIES", defaultCurrencies))
	apiBaseURL := strings.TrimRight(getEnv("XENDIT_API_BASE_URL", defaultXenditAPIBaseURL), "/")

	if _, ok := allowedCountries[defaultCountry]; !ok {
		return nil, fmt.Errorf("XENDIT_DEFAULT_COUNTRY %q must be included in XENDIT_ALLOWED_COUNTRIES", defaultCountry)
	}

	return &Config{
		ProviderID:        getEnv("XENDIT_PROVIDER_ID", "provider_xendit"),
		DisplayName:       getEnv("XENDIT_DISPLAY_NAME", "Xendit"),
		SecretAPIKey:      secret,
		CallbackToken:     callbackToken,
		APIBaseURL:        apiBaseURL,
		DefaultCountry:    defaultCountry,
		AllowedCountries:  allowedCountries,
		AllowedCurrencies: allowedCurrencies,
	}, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseCodeSet(raw string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		code := normalizeCode(part)
		if code != "" {
			out[code] = struct{}{}
		}
	}
	return out
}

func normalizeCode(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func sortedCodes(codes map[string]struct{}) []string {
	out := make([]string, 0, len(codes))
	for code := range codes {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}
