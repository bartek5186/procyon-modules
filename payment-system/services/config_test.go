package services

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestLoadRuntimeConfigRequiresCatalogForEveryProvider(t *testing.T) {
	file := filepath.Join(t.TempDir(), "products.json")
	if err := os.WriteFile(file, []byte(`[{"provider":"stripe","product_id":"price_1","plan_code":"premium","kind":"subscription","currency":"PLN","amount_minor":1999,"interval":"1-month"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAYMENT_PRODUCTS_FILE", file)
	config, err := LoadRuntimeConfig([]string{"stripe"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if product, err := config.Product("stripe", "price_1", "subscription"); err != nil || product.PlanCode != "premium" {
		t.Fatalf("product = %+v, %v", product, err)
	}
	if _, err := LoadRuntimeConfig([]string{"google"}, nil); err == nil {
		t.Fatal("expected missing Google product/key configuration error")
	}
}

func TestLoadRuntimeConfigAcceptsGoogleEncryptionKey(t *testing.T) {
	file := filepath.Join(t.TempDir(), "products.json")
	if err := os.WriteFile(file, []byte(`[{"provider":"google","product_id":"premium","plan_code":"premium","kind":"subscription","currency":"PLN","amount_minor":1999,"interval":"1-month"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAYMENT_PRODUCTS_FILE", file)
	t.Setenv("PAYMENT_DATA_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	config, err := LoadRuntimeConfig([]string{"google"}, nil)
	if err != nil || len(config.EncryptionKey) != 32 {
		t.Fatalf("config key = %d, %v", len(config.EncryptionKey), err)
	}
}

func TestLoadRuntimeConfigRejectsDisabledWrongKindAndInvalidPrice(t *testing.T) {
	tests := []string{
		`[{"provider":"stripe","product_id":"disabled","plan_code":"premium","kind":"subscription","currency":"PLN","amount_minor":1999,"interval":"1-month","enabled":false}]`,
		`[{"provider":"stripe","product_id":"wrong","plan_code":"premium","kind":"subscription","currency":"PLN","amount_minor":0,"interval":"1-month"}]`,
		`[{"provider":"stripe","product_id":"wrong","plan_code":"premium","kind":"one_time","currency":"PLN","amount_minor":1999,"interval":"1-month"}]`,
	}
	for index, body := range tests {
		file := filepath.Join(t.TempDir(), "products.json")
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadRuntimeConfig([]string{"stripe"}, map[string]string{"products_file": file}); err == nil {
			t.Fatalf("case %d: expected invalid product config", index)
		}
	}
}

func TestGoogleTokenEncryptionAndBinding(t *testing.T) {
	provider := &googlePaymentProvider{packageName: "com.example", encryptionKey: []byte("01234567890123456789012345678901")}
	encrypted, err := provider.encryptToken("purchase-token")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := provider.decryptToken(encrypted)
	if err != nil || plain != "purchase-token" {
		t.Fatalf("plain = %q, %v", plain, err)
	}
	if provider.accountBinding("user-1") == provider.accountBinding("user-2") {
		t.Fatal("account bindings collided")
	}
}

func TestEnabledProviderFactoriesFailFastOnMissingCredentials(t *testing.T) {
	runtime := RuntimeConfig{WebhookLease: time.Minute, Products: map[string]map[string]Product{
		"stripe": {"price": {Provider: "stripe", ProductID: "price", PlanCode: "premium", Kind: "subscription"}},
	}}
	runtime.EnabledProviders = []string{"stripe"}
	t.Setenv("STRIPE_SECRET_KEY", "")
	t.Setenv("STRIPE_WEBHOOK_SECRET", "")
	if _, _, err := newStripePaymentProvider(&fakePaymentModuleRepository{}, zap.NewNop(), runtime); err == nil {
		t.Fatal("expected missing Stripe credentials error")
	}
	runtime.EnabledProviders = []string{"google"}
	if _, _, err := newGooglePaymentProvider(&fakePaymentModuleRepository{}, zap.NewNop(), runtime); err == nil {
		t.Fatal("expected missing Google credentials error")
	}
	runtime.EnabledProviders = []string{"apple"}
	t.Setenv("APPLE_APP_STORE_CONFIG_FILE", "")
	if _, _, err := newApplePaymentProvider(&fakePaymentModuleRepository{}, zap.NewNop(), runtime); err == nil {
		t.Fatal("expected missing Apple config error")
	}
}
