package paymentsystem

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/bartek5186/procyon-core/authz"
	coreplugins "github.com/bartek5186/procyon-core/plugins"
	"github.com/bartek5186/procyon-modules/payment-system/controllers"
	"github.com/bartek5186/procyon-modules/payment-system/services"
	"github.com/bartek5186/procyon-modules/payment-system/store"
	"github.com/labstack/echo/v4"
	echomiddleware "github.com/labstack/echo/v4/middleware"
	"gorm.io/gorm"
)

const Name = "payment-system"

type Config struct {
	Providers []string          `json:"providers"`
	Values    map[string]string `json:"values"`
}

type Plugin struct {
	db             *gorm.DB
	controller     *controllers.PaymentSystemController
	service        *services.PaymentSystemService
	reconcileEvery time.Duration
	stop           context.CancelFunc
	wg             sync.WaitGroup
	startOnce      sync.Once
}

func New(_ context.Context, dependencies coreplugins.Dependencies, raw json.RawMessage) (coreplugins.Plugin, error) {
	if dependencies.DB == nil {
		return nil, fmt.Errorf("payment-system requires a database")
	}
	var config Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &config); err != nil {
			return nil, fmt.Errorf("parse payment-system config: %w", err)
		}
	}
	runtimeConfig, err := services.LoadRuntimeConfig(config.Providers, config.Values)
	if err != nil {
		return nil, fmt.Errorf("configure payment-system: %w", err)
	}
	repository := store.NewPaymentSystemStore(dependencies.DB)
	service, err := services.NewPaymentSystemService(repository, dependencies.Logger, runtimeConfig)
	if err != nil {
		return nil, fmt.Errorf("initialize payment-system providers: %w", err)
	}
	return &Plugin{
		db:         dependencies.DB,
		controller: controllers.NewPaymentSystemController(service, dependencies.Logger),
		service:    service, reconcileEvery: runtimeConfig.ReconcileEvery,
	}, nil
}

func (p *Plugin) Name() string { return Name }

func (p *Plugin) Migrate(context.Context) error {
	return runPaymentMigrations(p.db)
}

func (p *Plugin) Policies() []authz.Policy {
	return []authz.Policy{{Role: authz.RoleUser, Domain: "*", Object: "payment_system", Action: "use"}}
}

func (p *Plugin) RegisterRoutes(routes coreplugins.Routes) {
	p.startBackgroundReconciliation()
	if routes.Public != nil {
		routes.Public.GET("/payments/prices/:provider", p.controller.PriceList)
		routes.Public.POST("/payments/webhooks/:provider", p.controller.Notify)
	}
	if routes.Authenticated == nil {
		return
	}
	paymentRoutes := routes.Authenticated.Group("/payments")
	paymentRoutes.Use(echomiddleware.BodyLimit("64K"))
	middleware := permissionMiddleware(routes)
	paymentRoutes.POST("/checkout", p.controller.CreateCheckout, middleware...)
	paymentRoutes.POST("/subscriptions/checkout", p.controller.CreateSubscription, middleware...)
	paymentRoutes.GET("/subscriptions", p.controller.SubscriptionList, middleware...)
	paymentRoutes.POST("/subscriptions/cancel", p.controller.CancelSubscription, middleware...)
	paymentRoutes.POST("/portal", p.controller.CreatePortalSession, middleware...)
	paymentRoutes.POST("/verify/:provider", p.controller.VerifyStorePurchase, middleware...)
	paymentRoutes.GET("/history", p.controller.PaymentHistory, middleware...)
	paymentRoutes.GET("/entitlement", p.controller.Entitlement, middleware...)
	if routes.Admin != nil {
		admin := routes.Admin.Group("/payments")
		admin.Use(echomiddleware.BodyLimit("64K"))
		admin.GET("/providers", p.controller.ProviderStatus)
		admin.GET("/webhooks/failed", p.controller.FailedWebhooks)
		admin.POST("/webhooks/retry", p.controller.RetryWebhook)
		admin.POST("/reconcile", p.controller.Reconcile)
	}
}

func (p *Plugin) Shutdown(context.Context) error {
	if p.stop != nil {
		p.stop()
	}
	p.wg.Wait()
	return nil
}

func (p *Plugin) startBackgroundReconciliation() {
	p.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		p.stop = cancel
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			ticker := time.NewTicker(p.reconcileEvery)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					_ = p.service.Reconcile(ctx)
				}
			}
		}()
	})
}

func permissionMiddleware(routes coreplugins.Routes) []echo.MiddlewareFunc {
	if routes.Require == nil {
		return nil
	}
	return []echo.MiddlewareFunc{routes.Require("*", "payment_system", "use")}
}
