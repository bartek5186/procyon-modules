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
	ListSubscriptions(context.Context, string, int, int) ([]models.PaymentSubscriptionResponse, error)
	PaymentHistory(context.Context, string, int, int) ([]models.PaymentEventResponse, error)
	Entitlement(context.Context, string, string) (models.PaymentEntitlementResponse, error)
	ProviderStatuses() []models.PaymentProviderStatus
	FailedWebhooks(context.Context, int, int) ([]models.PaymentWebhookResponse, error)
	RetryWebhook(context.Context, string, string) error
	Reconcile(context.Context) error
}

type PaymentSystemController struct {
	service paymentSystemService
	logger  *zap.Logger
}

func NewPaymentSystemController(service paymentSystemService, logger *zap.Logger) *PaymentSystemController {
	return &PaymentSystemController{service: service, logger: logger}
}

// PriceList returns the configured, purchasable products for a provider.
//
// This public endpoint only exposes products allowed by the application's
// payment catalog. Use stripe, google, or apple as the provider path value.
// Amounts use minor currency units, for example 4999 means PLN 49.99.
func (c *PaymentSystemController) PriceList(ctx echo.Context) error {
	items, err := c.service.Prices(ctx.Request().Context(), ctx.Param("provider"))
	if err != nil {
		return paymentHTTPError(err)
	}
	return ctx.JSON(http.StatusOK, map[string]any{"items": items})
}

// CreateCheckout creates a one-time Stripe Checkout session for the signed-in
// identity and returns the hosted checkout URL.
//
// Send a unique Idempotency-Key header with every logical purchase attempt.
// The price must be present in the payment catalog and the success and cancel
// URLs must match the configured return URL allowlist.
func (c *PaymentSystemController) CreateCheckout(ctx echo.Context) error {
	identityID, _, ok := paymentIdentity(ctx)
	if !ok {
		return apierr.Unauthorized("unauthorized")
	}
	var input models.PaymentCheckoutInput
	if err := ctx.Bind(&input); err != nil {
		return apierr.BadRequest("invalid request")
	}
	input.IdempotencyKey = strings.TrimSpace(ctx.Request().Header.Get("Idempotency-Key"))
	if err := ctx.Validate(&input); err != nil {
		return apierr.Wrap(err, http.StatusBadRequest, "validation_failed", "validation failed")
	}
	url, err := c.service.CreateCheckout(ctx.Request().Context(), identityID, input)
	if err != nil {
		return paymentHTTPError(err)
	}
	return ctx.JSON(http.StatusCreated, map[string]string{"checkout_url": url})
}

// CreateSubscription creates a recurring Stripe Checkout session for the
// signed-in identity and returns the hosted checkout URL.
//
// Send a unique Idempotency-Key header. A conflicting active subscription can
// return payment_subscription_active. Store subscriptions are activated with
// VerifyStorePurchase instead of this endpoint.
func (c *PaymentSystemController) CreateSubscription(ctx echo.Context) error {
	identityID, email, ok := paymentIdentity(ctx)
	if !ok {
		return apierr.Unauthorized("unauthorized")
	}
	var input models.PaymentSubscriptionCheckoutInput
	if err := ctx.Bind(&input); err != nil {
		return apierr.BadRequest("invalid request")
	}
	input.IdempotencyKey = strings.TrimSpace(ctx.Request().Header.Get("Idempotency-Key"))
	if err := ctx.Validate(&input); err != nil {
		return apierr.Wrap(err, http.StatusBadRequest, "validation_failed", "validation failed")
	}
	url, err := c.service.CreateSubscription(ctx.Request().Context(), identityID, email, input)
	if err != nil {
		return paymentHTTPError(err)
	}
	return ctx.JSON(http.StatusCreated, map[string]string{"checkout_url": url})
}

// SubscriptionList returns subscriptions owned by the signed-in identity.
//
// Results may contain Stripe, Google Play, and Apple subscriptions. Pagination
// uses limit and offset; limit must be between 1 and 100 when provided.
func (c *PaymentSystemController) SubscriptionList(ctx echo.Context) error {
	identityID, _, ok := paymentIdentity(ctx)
	if !ok {
		return apierr.Unauthorized("unauthorized")
	}
	var query models.PaymentSubscriptionQuery
	if err := ctx.Bind(&query); err != nil {
		return apierr.BadRequest("invalid query")
	}
	if err := ctx.Validate(&query); err != nil {
		return apierr.Wrap(err, http.StatusBadRequest, "validation_failed", "validation failed")
	}
	items, err := c.service.ListSubscriptions(ctx.Request().Context(), identityID, query.Limit, query.Offset)
	if err != nil {
		return paymentHTTPError(err)
	}
	return ctx.JSON(http.StatusOK, map[string]any{"items": items})
}

// CancelSubscription cancels an owned provider subscription immediately or at
// the end of its current billing period.
//
// Stripe supports this operation. Set cancel_at_period_end to true to retain
// access until current_period_end. The endpoint returns 204 on success.
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

// CreatePortalSession creates a Stripe Billing Portal session for the signed-in
// identity and returns its short-lived URL.
//
// The identity must already be associated with a Stripe customer. return_url
// must match the configured allowlist.
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

// VerifyStorePurchase verifies and records a Google Play purchase token or an
// Apple StoreKit 2 signed transaction for the signed-in identity.
//
// Use google or apple as the provider path value. Google requests require
// product_id and purchase_token; Apple requests require signed_payload. The
// endpoint returns 204 when verification succeeds.
func (c *PaymentSystemController) VerifyStorePurchase(ctx echo.Context) error {
	identityID, _, ok := paymentIdentity(ctx)
	if !ok {
		return apierr.Unauthorized("unauthorized")
	}
	var input models.PaymentStoreVerificationInput
	if err := ctx.Bind(&input); err != nil {
		return apierr.BadRequest("invalid request")
	}
	if err := ctx.Validate(&input); err != nil {
		return apierr.Wrap(err, http.StatusBadRequest, "validation_failed", "validation failed")
	}
	if err := c.service.VerifyStorePurchase(ctx.Request().Context(), ctx.Param("provider"), identityID, input); err != nil {
		return paymentHTTPError(err)
	}
	return ctx.NoContent(http.StatusNoContent)
}

// Notify receives provider-signed payment events and updates local payment and
// subscription state.
//
// This public endpoint is intended only for provider delivery. Stripe uses the
// Stripe-Signature header, Google uses an OIDC Authorization bearer token, and
// Apple sends a signedPayload body. The raw payload limit is 256 KiB.
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
	} else if provider == "google" {
		signature = ctx.Request().Header.Get("Authorization")
	}
	if err := c.service.Notify(ctx.Request().Context(), provider, payload, signature); err != nil {
		c.logger.Warn("payment webhook failed", zap.String("provider", provider), zap.Error(err))
		return paymentHTTPError(err)
	}
	return ctx.NoContent(http.StatusOK)
}

// PaymentHistory returns payment events owned by the signed-in identity.
//
// Entries cover one-time purchases, recurring charges, refunds, disputes, and
// cancellations across all enabled providers. Pagination uses limit and offset.
func (c *PaymentSystemController) PaymentHistory(ctx echo.Context) error {
	identityID, _, ok := paymentIdentity(ctx)
	if !ok {
		return apierr.Unauthorized("unauthorized")
	}
	var query models.PaymentHistoryQuery
	if err := ctx.Bind(&query); err != nil {
		return apierr.BadRequest("invalid query")
	}
	if err := ctx.Validate(&query); err != nil {
		return apierr.Wrap(err, http.StatusBadRequest, "validation_failed", "validation failed")
	}
	items, err := c.service.PaymentHistory(ctx.Request().Context(), identityID, query.Limit, query.Offset)
	if err != nil {
		return paymentHTTPError(err)
	}
	return ctx.JSON(http.StatusOK, map[string]any{"items": items})
}

// Entitlement resolves whether the signed-in identity currently has access to
// a plan.
//
// Pass plan_code to check a specific product entitlement or omit it to resolve
// any active entitlement. The response includes provider, status, and expiry
// information when access is active.
func (c *PaymentSystemController) Entitlement(ctx echo.Context) error {
	identityID, _, ok := paymentIdentity(ctx)
	if !ok {
		return apierr.Unauthorized("unauthorized")
	}
	var query models.PaymentEntitlementQuery
	if err := ctx.Bind(&query); err != nil {
		return apierr.BadRequest("invalid query")
	}
	if err := ctx.Validate(&query); err != nil {
		return apierr.Wrap(err, http.StatusBadRequest, "validation_failed", "validation failed")
	}
	item, err := c.service.Entitlement(ctx.Request().Context(), identityID, query.PlanCode)
	if err != nil {
		return paymentHTTPError(err)
	}
	return ctx.JSON(http.StatusOK, item)
}

// ProviderStatus returns operational readiness and aggregate request metrics
// for enabled payment providers.
//
// This is an admin endpoint. It never returns provider credentials or secrets.
func (c *PaymentSystemController) ProviderStatus(ctx echo.Context) error {
	return ctx.JSON(http.StatusOK, map[string]any{"items": c.service.ProviderStatuses()})
}

// FailedWebhooks returns provider events that could not be processed and may
// require an operator retry.
//
// This is an admin endpoint. Pagination uses limit and offset. last_error is
// diagnostic data and must not be exposed to public clients.
func (c *PaymentSystemController) FailedWebhooks(ctx echo.Context) error {
	var query models.PaymentWebhookListQuery
	if err := ctx.Bind(&query); err != nil {
		return apierr.BadRequest("invalid query")
	}
	if err := ctx.Validate(&query); err != nil {
		return apierr.Wrap(err, http.StatusBadRequest, "validation_failed", "validation failed")
	}
	items, err := c.service.FailedWebhooks(ctx.Request().Context(), query.Limit, query.Offset)
	if err != nil {
		return paymentHTTPError(err)
	}
	return ctx.JSON(http.StatusOK, map[string]any{"items": items})
}

// RetryWebhook reprocesses one failed webhook identified by provider and
// provider event ID.
//
// This is an admin endpoint. Retries are idempotent and return 204 after the
// event has been accepted and processed successfully.
func (c *PaymentSystemController) RetryWebhook(ctx echo.Context) error {
	var input models.PaymentWebhookRetryInput
	if err := ctx.Bind(&input); err != nil {
		return apierr.BadRequest("invalid request")
	}
	if err := ctx.Validate(&input); err != nil {
		return apierr.Wrap(err, http.StatusBadRequest, "validation_failed", "validation failed")
	}
	if err := c.service.RetryWebhook(ctx.Request().Context(), input.Provider, input.EventID); err != nil {
		return paymentHTTPError(err)
	}
	return ctx.NoContent(http.StatusNoContent)
}

// Reconcile compares stored subscriptions with every enabled provider and
// repairs stale subscription state.
//
// This is an admin recovery operation. Normal synchronization should happen
// through webhooks and background reconciliation. The endpoint returns 204.
func (c *PaymentSystemController) Reconcile(ctx echo.Context) error {
	if err := c.service.Reconcile(ctx.Request().Context()); err != nil {
		return paymentHTTPError(err)
	}
	return ctx.NoContent(http.StatusNoContent)
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
		return apierr.New(http.StatusBadRequest, "payment_provider_disabled", "payment provider is not enabled")
	case errors.Is(err, services.ErrPaymentCapabilityMissing):
		return apierr.New(http.StatusBadRequest, "payment_capability_unsupported", "payment capability is not supported")
	case errors.Is(err, services.ErrPaymentInvalidSignature):
		return apierr.New(http.StatusBadRequest, "payment_signature_invalid", "invalid webhook signature")
	case errors.Is(err, services.ErrPaymentInvalidReturnURL):
		return apierr.New(http.StatusBadRequest, "payment_return_url_rejected", "return URL is not allowed")
	case errors.Is(err, services.ErrPaymentSubscriptionActive):
		return apierr.New(http.StatusConflict, "payment_subscription_active", "an active subscription already exists")
	case errors.Is(err, models.ErrPaymentOwnershipConflict):
		return apierr.New(http.StatusConflict, "payment_ownership_conflict", "payment resource belongs to another identity")
	case errors.Is(err, models.ErrPaymentProductRejected):
		return apierr.New(http.StatusBadRequest, "payment_product_rejected", "payment product is not allowed")
	case errors.Is(err, models.ErrPaymentIdempotencyKey):
		return apierr.New(http.StatusBadRequest, "payment_idempotency_key_invalid", "valid Idempotency-Key header is required")
	case errors.Is(err, gorm.ErrRecordNotFound):
		return apierr.NotFound("payment resource not found")
	default:
		return apierr.Wrap(err, http.StatusInternalServerError, "payment_error", "payment operation failed")
	}
}
