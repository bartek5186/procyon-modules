package services

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bartek5186/procyon-modules/payment-system/models"
)

type Product struct {
	Provider    string `json:"provider"`
	ProductID   string `json:"product_id"`
	PlanCode    string `json:"plan_code"`
	Kind        string `json:"kind"`
	Currency    string `json:"currency,omitempty"`
	AmountMinor int64  `json:"amount_minor,omitempty"`
	Interval    string `json:"interval,omitempty"`
	Enabled     *bool  `json:"enabled,omitempty"`
}

type RuntimeConfig struct {
	EnabledProviders []string
	Products         map[string]map[string]Product
	WebhookLease     time.Duration
	WebhookRetention time.Duration
	ReconcileEvery   time.Duration
	EncryptionKey    []byte
}

func LoadRuntimeConfig(enabledProviders []string, values map[string]string) (RuntimeConfig, error) {
	config := RuntimeConfig{
		EnabledProviders: normalizedProviders(enabledProviders),
		Products:         map[string]map[string]Product{}, WebhookLease: 5 * time.Minute,
		WebhookRetention: 30 * 24 * time.Hour, ReconcileEvery: 6 * time.Hour,
	}
	if len(config.EnabledProviders) == 0 {
		return RuntimeConfig{}, fmt.Errorf("payment-system requires at least one provider")
	}
	for _, name := range config.EnabledProviders {
		if name != "stripe" && name != "google" && name != "apple" {
			return RuntimeConfig{}, fmt.Errorf("unsupported configured payment provider %q", name)
		}
	}
	if err := parseDurationValue(&config.WebhookLease, value(values, "webhook_lease", "PAYMENT_WEBHOOK_LEASE")); err != nil {
		return RuntimeConfig{}, fmt.Errorf("payment webhook lease: %w", err)
	}
	if err := parseDurationValue(&config.WebhookRetention, value(values, "webhook_retention", "PAYMENT_WEBHOOK_RETENTION")); err != nil {
		return RuntimeConfig{}, fmt.Errorf("payment webhook retention: %w", err)
	}
	if err := parseDurationValue(&config.ReconcileEvery, value(values, "reconcile_every", "PAYMENT_RECONCILE_EVERY")); err != nil {
		return RuntimeConfig{}, fmt.Errorf("payment reconciliation interval: %w", err)
	}
	productsFile := value(values, "products_file", "PAYMENT_PRODUCTS_FILE")
	if strings.TrimSpace(productsFile) == "" {
		return RuntimeConfig{}, fmt.Errorf("PAYMENT_PRODUCTS_FILE is required")
	}
	raw, err := os.ReadFile(productsFile)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("read payment products: %w", err)
	}
	var products []Product
	if err := json.Unmarshal(raw, &products); err != nil {
		return RuntimeConfig{}, fmt.Errorf("parse payment products: %w", err)
	}
	for _, product := range products {
		product.Provider = strings.ToLower(strings.TrimSpace(product.Provider))
		product.ProductID = strings.TrimSpace(product.ProductID)
		product.PlanCode = strings.TrimSpace(product.PlanCode)
		product.Kind = strings.ToLower(strings.TrimSpace(product.Kind))
		product.Currency = strings.ToUpper(strings.TrimSpace(product.Currency))
		product.Interval = strings.ToLower(strings.TrimSpace(product.Interval))
		if product.Provider == "" || product.ProductID == "" || product.PlanCode == "" || product.AmountMinor <= 0 || len(product.Currency) != 3 ||
			(product.Kind != "one_time" && product.Kind != "subscription") {
			return RuntimeConfig{}, fmt.Errorf("invalid payment product entry for %q", product.ProductID)
		}
		if (product.Kind == "subscription" && !validProductInterval(product.Interval)) || (product.Kind == "one_time" && product.Interval != "") {
			return RuntimeConfig{}, fmt.Errorf("invalid payment product interval for %q", product.ProductID)
		}
		if product.Enabled != nil && !*product.Enabled {
			continue
		}
		if config.Products[product.Provider] == nil {
			config.Products[product.Provider] = map[string]Product{}
		}
		if _, exists := config.Products[product.Provider][product.ProductID]; exists {
			return RuntimeConfig{}, fmt.Errorf("duplicate payment product %s/%s", product.Provider, product.ProductID)
		}
		config.Products[product.Provider][product.ProductID] = product
	}
	for _, provider := range config.EnabledProviders {
		if len(config.Products[provider]) == 0 {
			return RuntimeConfig{}, fmt.Errorf("no enabled products configured for provider %s", provider)
		}
	}
	if containsProvider(config.EnabledProviders, "google") {
		encoded := strings.TrimSpace(os.Getenv("PAYMENT_DATA_ENCRYPTION_KEY"))
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(key) != 32 {
			return RuntimeConfig{}, fmt.Errorf("PAYMENT_DATA_ENCRYPTION_KEY must be a base64-encoded 32-byte key when Google Play is enabled")
		}
		config.EncryptionKey = key
	}
	return config, nil
}

func validProductInterval(value string) bool {
	count, unit, found := strings.Cut(value, "-")
	if !found {
		return false
	}
	number, err := strconv.Atoi(count)
	if err != nil || number <= 0 {
		return false
	}
	return unit == "day" || unit == "week" || unit == "month" || unit == "year"
}

func (c RuntimeConfig) Product(provider, productID, kind string) (Product, error) {
	product, ok := c.Products[strings.ToLower(strings.TrimSpace(provider))][strings.TrimSpace(productID)]
	if !ok || (kind != "" && product.Kind != kind) {
		return Product{}, models.ErrPaymentProductRejected
	}
	return product, nil
}

func normalizedProviders(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func containsProvider(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func value(values map[string]string, key, environment string) string {
	if raw := strings.TrimSpace(values[key]); raw != "" {
		return raw
	}
	return strings.TrimSpace(os.Getenv(environment))
}

func parseDurationValue(target *time.Duration, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds <= 0 {
			return fmt.Errorf("must be positive")
		}
		*target = time.Duration(seconds) * time.Second
		return nil
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		return fmt.Errorf("invalid positive duration %q", raw)
	}
	*target = duration
	return nil
}
