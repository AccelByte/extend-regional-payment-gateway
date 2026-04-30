package dana

import (
	"fmt"
	"os"
)

// Config holds DANA adapter configuration loaded from environment variables.
type Config struct {
	MerchantID     string // DANA_MERCHANT_ID — "Merchant ID" from dashboard, used in request bodies
	PartnerID      string // DANA_PARTNER_ID  — "Client ID" from dashboard, used as X-PARTNER-ID header
	ClientSecret   string // DANA_CLIENT_SECRET
	PrivateKeyPEM  string // DANA_PRIVATE_KEY — your RSA private key, inline PEM with \n escaped
	PrivateKeyPath string // DANA_PRIVATE_KEY_PATH — file path (takes precedence over inline)
	PublicKeyPEM   string // DANA_PUBLIC_KEY — DANA's RSA public key for webhook verification
	PublicKeyPath  string // DANA_PUBLIC_KEY_PATH — file path for DANA's public key (optional)
	ChannelID      string // DANA_CHANNEL_ID — optional, defaults to "95221"
	Env            string // DANA_ENV: "SANDBOX" (default) | "PRODUCTION"
	Origin         string // DANA_ORIGIN: HTTP Origin header value
	CheckoutMode   string // DANA_CHECKOUT_MODE: "DANA_BALANCE" (default, balance only) | "DANA_GAPURA" (Gapura Hosted Checkout)
}

// Load reads DANA config from environment variables.
// Returns (nil, nil) when DANA_MERCHANT_ID is unset — the caller treats this as DANA disabled.
// Returns an error if DANA_MERCHANT_ID is set but required fields are missing.
func Load() (*Config, error) {
	merchantID := os.Getenv("DANA_MERCHANT_ID")
	if merchantID == "" {
		return nil, nil
	}

	cfg := &Config{
		MerchantID:     merchantID,
		PartnerID:      os.Getenv("DANA_PARTNER_ID"),
		ClientSecret:   os.Getenv("DANA_CLIENT_SECRET"),
		PrivateKeyPEM:  os.Getenv("DANA_PRIVATE_KEY"),
		PrivateKeyPath: os.Getenv("DANA_PRIVATE_KEY_PATH"),
		PublicKeyPEM:   os.Getenv("DANA_PUBLIC_KEY"),
		PublicKeyPath:  os.Getenv("DANA_PUBLIC_KEY_PATH"),
		ChannelID:    getEnv("DANA_CHANNEL_ID", "95221"),
		Env:          getEnv("DANA_ENV", "SANDBOX"),
		Origin:       getEnv("DANA_ORIGIN", "https://dana.id"),
		CheckoutMode: getEnv("DANA_CHECKOUT_MODE", "DANA_BALANCE"),
	}

	if cfg.PartnerID == "" {
		return nil, fmt.Errorf("DANA_PARTNER_ID is required when DANA_MERCHANT_ID is set")
	}
	if cfg.ClientSecret == "" {
		return nil, fmt.Errorf("DANA_CLIENT_SECRET is required when DANA_MERCHANT_ID is set")
	}
	if cfg.PrivateKeyPEM == "" && cfg.PrivateKeyPath == "" {
		return nil, fmt.Errorf("either DANA_PRIVATE_KEY or DANA_PRIVATE_KEY_PATH is required when DANA_MERCHANT_ID is set")
	}

	return cfg, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
