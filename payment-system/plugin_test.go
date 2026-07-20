package paymentsystem

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	coreevents "github.com/bartek5186/procyon-core/events"
	coreplugins "github.com/bartek5186/procyon-core/plugins"
	"github.com/glebarez/sqlite"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

func TestPluginStartupMigrationRoutesAndShutdown(t *testing.T) {
	plugin := newTestPlugin(t)
	if plugin.Name() != Name || len(plugin.Policies()) == 0 {
		t.Fatalf("invalid plugin contract: %s / %+v", plugin.Name(), plugin.Policies())
	}
	if err := plugin.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	e := echo.New()
	plugin.RegisterRoutes(coreplugins.Routes{Public: e.Group(""), Authenticated: e.Group("/api"), Admin: e.Group("/admin")})
	if starter, ok := plugin.(coreplugins.Starter); !ok {
		t.Fatal("plugin does not implement Starter")
	} else if err := starter.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	var routes []string
	for _, route := range e.Routes() {
		routes = append(routes, route.Method+" "+route.Path)
	}
	sort.Strings(routes)
	for _, expected := range []string{
		"GET /admin/payments/providers", "GET /api/payments/entitlement", "GET /api/payments/history",
		"GET /payments/prices/:provider", "POST /payments/webhooks/:provider",
	} {
		if !containsRoute(routes, expected) {
			t.Fatalf("missing route %q in %+v", expected, routes)
		}
	}
	if err := plugin.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestPluginRejectsInvalidProviderConfiguration(t *testing.T) {
	db := pluginTestDB(t)
	_, err := New(context.Background(), coreplugins.Dependencies{DB: db, Logger: zap.NewNop(), Events: coreevents.New()}, json.RawMessage(`{"providers":["unknown"]}`))
	if err == nil {
		t.Fatal("expected invalid provider configuration error")
	}
}

func TestPluginRequiresEventBus(t *testing.T) {
	_, err := New(context.Background(), coreplugins.Dependencies{DB: pluginTestDB(t), Logger: zap.NewNop()}, nil)
	if err == nil || err.Error() != "payment-system requires the Procyon event bus; update the application runtime wiring" {
		t.Fatalf("unexpected event bus error: %v", err)
	}
}

func newTestPlugin(t *testing.T) coreplugins.Plugin {
	t.Helper()
	directory := t.TempDir()
	products := filepath.Join(directory, "products.json")
	if err := os.WriteFile(products, []byte(`[{"provider":"stripe","product_id":"price_test","plan_code":"premium","kind":"subscription","currency":"PLN","amount_minor":1999,"interval":"1-month"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_fixture")
	t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_fixture")
	raw, _ := json.Marshal(Config{Providers: []string{"stripe"}, Values: map[string]string{"products_file": products, "reconcile_every": "24h"}})
	plugin, err := New(context.Background(), coreplugins.Dependencies{DB: pluginTestDB(t), Logger: zap.NewNop(), Events: coreevents.New()}, raw)
	if err != nil {
		t.Fatal(err)
	}
	return plugin
}

func pluginTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func containsRoute(routes []string, expected string) bool {
	for _, route := range routes {
		if route == expected {
			return true
		}
	}
	return false
}
