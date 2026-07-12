package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/bartek5186/procyon-core/apierr"
	mid "github.com/bartek5186/procyon-core/middleware"
	"github.com/bartek5186/procyon-modules/payment-system/models"
	"github.com/bartek5186/procyon-modules/payment-system/services"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type paymentSystemService interface {
	Providers() []string
	Prices(context.Context, string) ([]models.PaymentPriceOption, error)
	CreateCheckout(context.Context, string, models.PaymentCheckoutInput) (string, error)
	CreateSubscription(context.Context, string, string, models.PaymentSubscriptionCheckoutInput) (string, error)
	CancelSubscription(context.Context, string, models.PaymentCancelSubscriptionInput) error
	CreatePortal(context.Context, string, models.PaymentPortalInput) (string, error)
	Notify(context.Context, string, []byte, string) error
	VerifyStorePurchase(context.Context, string, string, models.PaymentStoreVerificationInput) error
	ListSubscriptions(context.Context, string) ([]models.PaymentSubscriptionResponse, error)
}

type PaymentSystemController struct {
	service paymentSystemService
	logger  *zap.Logger
}

func NewPaymentSystemController(service paymentSystemService, logger *zap.Logger) *PaymentSystemController {
	return &PaymentSystemController{service: service, logger: logger}
}

func (c *PaymentSystemController) PriceList(ctx echo.Context) error {
	items, err := c.service.Prices(ctx.Request().Context(), ctx.Param("provider"))
	if err != nil {
		return paymentHTTPError(err)
	}
	return ctx.JSON(http.StatusOK, map[string]any{"items": items})
}

func (c *PaymentSystemController) CreateCheckout(ctx echo.Context) error {
	identityID, _, ok := paymentIdentity(ctx)
	if !ok {
		return apierr.Unauthorized("unauthorized")
	}
	var input models.PaymentCheckoutInput
	if err := ctx.Bind(&input); err != nil {
		return apierr.BadRequest("invalid request")
	}
	if err := ctx.Validate(&input); err != nil {
		return apierr.Wrap(err, http.StatusBadRequest, "validation_failed", "validation failed")
	}
	url, err := c.service.CreateCheckout(ctx.Request().Context(), identityID, input)
	if err != nil {
		return paymentHTTPError(err)
	}
	return ctx.JSON(http.StatusCreated, map[string]string{"checkout_url": url})
}

func (c *PaymentSystemController) CreateSubscription(ctx echo.Context) error {
	identityID, email, ok := paymentIdentity(ctx)
	if !ok {
		return apierr.Unauthorized("unauthorized")
	}
	var input models.PaymentSubscriptionCheckoutInput
	if err := ctx.Bind(&input); err != nil {
		return apierr.BadRequest("invalid request")
	}
	if err := ctx.Validate(&input); err != nil {
		return apierr.Wrap(err, http.StatusBadRequest, "validation_failed", "validation failed")
	}
	url, err := c.service.CreateSubscription(ctx.Request().Context(), identityID, email, input)
	if err != nil {
		return paymentHTTPError(err)
	}
	return ctx.JSON(http.StatusCreated, map[string]string{"checkout_url": url})
}

func (c *PaymentSystemController) SubscriptionList(ctx echo.Context) error {
	identityID, _, ok := paymentIdentity(ctx)
	if !ok {
		return apierr.Unauthorized("unauthorized")
	}
	items, err := c.service.ListSubscriptions(ctx.Request().Context(), identityID)
	if err != nil {
		return paymentHTTPError(err)
	}
	return ctx.JSON(http.StatusOK, map[string]any{"items": items})
}

func (c *PaymentSystemController) CancelSubscription(ctx echo.Context) error {
	identityID, _, ok := paymentIdentity(ctx)
	if !ok {
		return apierr.Unauthorized("unauthorized")
	}
	var input models.PaymentCancelSubscriptionInput
	if err := ctx.Bind(&input); err != nil {
		return apierr.BadRequest("invalid request")
	}
	if err := ctx.Validate(&input); err != nil {
		return apierr.Wrap(err, http.StatusBadRequest, "validation_failed", "validation failed")
	}
	if err := c.service.CancelSubscription(ctx.Request().Context(), identityID, input); err != nil {
		return paymentHTTPError(err)
	}
	return ctx.NoContent(http.StatusNoContent)
}

func (c *PaymentSystemController) CreatePortalSession(ctx echo.Context) error {
	identityID, _, ok := paymentIdentity(ctx)
	if !ok {
		return apierr.Unauthorized("unauthorized")
	}
	var input models.PaymentPortalInput
	if err := ctx.Bind(&input); err != nil {
		return apierr.BadRequest("invalid request")
	}
	if err := ctx.Validate(&input); err != nil {
		return apierr.Wrap(err, http.StatusBadRequest, "validation_failed", "validation failed")
	}
	url, err := c.service.CreatePortal(ctx.Request().Context(), identityID, input)
	if err != nil {
		return paymentHTTPError(err)
	}
	return ctx.JSON(http.StatusCreated, map[string]string{"portal_url": url})
}

func (c *PaymentSystemController) VerifyStorePurchase(ctx echo.Context) error {
	identityID, _, ok := paymentIdentity(ctx)
	if !ok {
		return apierr.Unauthorized("unauthorized")
	}
	var input models.PaymentStoreVerificationInput
	if err := ctx.Bind(&input); err != nil {
		return apierr.BadRequest("invalid request")
	}
	if err := c.service.VerifyStorePurchase(ctx.Request().Context(), ctx.Param("provider"), identityID, input); err != nil {
		return paymentHTTPError(err)
	}
	return ctx.NoContent(http.StatusNoContent)
}

func (c *PaymentSystemController) Notify(ctx echo.Context) error {
	ctx.Request().Body = http.MaxBytesReader(ctx.Response(), ctx.Request().Body, 256<<10)
	payload, err := io.ReadAll(ctx.Request().Body)
	if err != nil {
		return apierr.BadRequest("invalid webhook body")
	}
	provider := strings.ToLower(strings.TrimSpace(ctx.Param("provider")))
	signature := ""
	if provider == "stripe" {
		signature = ctx.Request().Header.Get("Stripe-Signature")
	}
	if err := c.service.Notify(ctx.Request().Context(), provider, payload, signature); err != nil {
		c.logger.Warn("payment webhook failed", zap.String("provider", provider), zap.Error(err))
		return paymentHTTPError(err)
	}
	return ctx.NoContent(http.StatusOK)
}

func paymentIdentity(ctx echo.Context) (string, string, bool) {
	session, ok := mid.SessionFromContext(ctx)
	if !ok || session == nil || session.Identity == nil {
		return "", "", false
	}
	identityID := strings.TrimSpace(session.Identity.Id)
	if identityID == "" {
		return "", "", false
	}
	traits := map[string]any{}
	if raw, err := json.Marshal(session.Identity.Traits); err == nil {
		_ = json.Unmarshal(raw, &traits)
	}
	email, _ := traits["email"].(string)
	return identityID, strings.TrimSpace(email), true
}

func paymentHTTPError(err error) error {
	switch {
	case errors.Is(err, services.ErrPaymentProviderUnsupported):
		return apierr.BadRequest("payment provider is not enabled")
	case errors.Is(err, services.ErrPaymentCapabilityMissing):
		return apierr.BadRequest("payment capability is not supported")
	case errors.Is(err, services.ErrPaymentInvalidSignature):
		return apierr.BadRequest("invalid webhook signature")
	case errors.Is(err, services.ErrPaymentInvalidReturnURL):
		return apierr.BadRequest("return URL is not allowed")
	case errors.Is(err, services.ErrPaymentSubscriptionActive):
		return apierr.Conflict("an active subscription already exists")
	case errors.Is(err, gorm.ErrRecordNotFound):
		return apierr.NotFound("payment resource not found")
	default:
		return apierr.Wrap(err, http.StatusInternalServerError, "payment_error", "payment operation failed")
	}
}
