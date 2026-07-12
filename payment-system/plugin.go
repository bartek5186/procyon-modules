package paymentsystem

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bartek5186/procyon-core/authz"
	coreplugins "github.com/bartek5186/procyon-core/plugins"
	"github.com/bartek5186/procyon-modules/payment-system/controllers"
	"github.com/bartek5186/procyon-modules/payment-system/models"
	"github.com/bartek5186/procyon-modules/payment-system/services"
	"github.com/bartek5186/procyon-modules/payment-system/store"
	"github.com/labstack/echo/v4"
)

const Name = "payment-system"

type Config struct {
	Providers []string          `json:"providers"`
	Values    map[string]string `json:"values"`
}

type Plugin struct {
	db         database
	controller *controllers.PaymentSystemController
}

type database interface {
	AutoMigrate(...any) error
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
	repository := store.NewPaymentSystemStore(dependencies.DB)
	service := services.NewPaymentSystemService(repository, dependencies.Logger, config.Providers)
	return &Plugin{
		db:         dependencies.DB,
		controller: controllers.NewPaymentSystemController(service, dependencies.Logger),
	}, nil
}

func (p *Plugin) Name() string { return Name }

func (p *Plugin) Migrate(context.Context) error {
	return p.db.AutoMigrate(
		&models.PaymentEvent{},
		&models.PaymentWebhookEvent{},
		&models.PaymentSubscription{},
	)
}

func (p *Plugin) Policies() []authz.Policy {
	return []authz.Policy{{Role: authz.RoleUser, Domain: "*", Object: "payment_system", Action: "use"}}
}

func (p *Plugin) RegisterRoutes(routes coreplugins.Routes) {
	if routes.Public != nil {
		routes.Public.GET("/payments/prices/:provider", p.controller.PriceList)
		routes.Public.POST("/payments/webhooks/:provider", p.controller.Notify)
	}
	if routes.Authenticated == nil {
		return
	}
	paymentRoutes := routes.Authenticated.Group("/payments")
	middleware := permissionMiddleware(routes)
	paymentRoutes.POST("/checkout", p.controller.CreateCheckout, middleware...)
	paymentRoutes.POST("/subscriptions/checkout", p.controller.CreateSubscription, middleware...)
	paymentRoutes.GET("/subscriptions", p.controller.SubscriptionList, middleware...)
	paymentRoutes.POST("/subscriptions/cancel", p.controller.CancelSubscription, middleware...)
	paymentRoutes.POST("/portal", p.controller.CreatePortalSession, middleware...)
	paymentRoutes.POST("/verify/:provider", p.controller.VerifyStorePurchase, middleware...)
}

func (p *Plugin) Shutdown(context.Context) error { return nil }

func permissionMiddleware(routes coreplugins.Routes) []echo.MiddlewareFunc {
	if routes.Require == nil {
		return nil
	}
	return []echo.MiddlewareFunc{routes.Require("*", "payment_system", "use")}
}
