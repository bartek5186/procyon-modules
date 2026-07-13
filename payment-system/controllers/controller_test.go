package controllers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bartek5186/procyon-core/apierr"
	coremiddleware "github.com/bartek5186/procyon-core/middleware"
	"github.com/bartek5186/procyon-modules/payment-system/models"
	"github.com/labstack/echo/v4"
	ory "github.com/ory/client-go"
	"go.uber.org/zap"
)

type controllerTestService struct {
	paymentSystemService
	checkoutInput models.PaymentCheckoutInput
	verifyInput   models.PaymentStoreVerificationInput
	notifyPayload []byte
	notifyHeader  string
}

func (service *controllerTestService) CreateCheckout(_ context.Context, _ string, input models.PaymentCheckoutInput) (string, error) {
	service.checkoutInput = input
	return "https://checkout.example/session", nil
}

func (service *controllerTestService) VerifyStorePurchase(_ context.Context, _, _ string, input models.PaymentStoreVerificationInput) error {
	service.verifyInput = input
	return nil
}

func (service *controllerTestService) Notify(_ context.Context, _ string, payload []byte, signature string) error {
	service.notifyPayload = append([]byte(nil), payload...)
	service.notifyHeader = signature
	return nil
}

func TestCreateCheckoutRequiresAuthentication(t *testing.T) {
	controller := NewPaymentSystemController(&controllerTestService{}, zap.NewNop())
	ctx := newControllerContext(http.MethodPost, "/payments/checkout", `{}`)
	err := controller.CreateCheckout(ctx)
	var apiError *apierr.Error
	if !errors.As(err, &apiError) || apiError.Status != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized, got %v", err)
	}
}

func TestCreateCheckoutValidatesAndForwardsIdempotencyKey(t *testing.T) {
	service := &controllerTestService{}
	controller := NewPaymentSystemController(service, zap.NewNop())
	body := `{"provider":"stripe","price_id":"price_1","success_url":"https://app.example/success","cancel_url":"https://app.example/cancel"}`
	ctx := newControllerContext(http.MethodPost, "/payments/checkout", body)
	setControllerIdentity(ctx)
	err := controller.CreateCheckout(ctx)
	var apiError *apierr.Error
	if !errors.As(err, &apiError) || apiError.Status != http.StatusBadRequest {
		t.Fatalf("expected missing idempotency validation error, got %v", err)
	}

	ctx = newControllerContext(http.MethodPost, "/payments/checkout", body)
	ctx.Request().Header.Set("Idempotency-Key", "request-123")
	setControllerIdentity(ctx)
	if err := controller.CreateCheckout(ctx); err != nil {
		t.Fatalf("create checkout: %v", err)
	}
	if service.checkoutInput.IdempotencyKey != "request-123" || ctx.Response().Status != http.StatusCreated {
		t.Fatalf("unexpected input/status: %+v / %d", service.checkoutInput, ctx.Response().Status)
	}
}

func TestVerifyStorePurchaseRunsValidation(t *testing.T) {
	controller := NewPaymentSystemController(&controllerTestService{}, zap.NewNop())
	ctx := newControllerContext(http.MethodPost, "/payments/verify/google", `{}`)
	ctx.SetPath("/payments/verify/:provider")
	ctx.SetParamNames("provider")
	ctx.SetParamValues("google")
	setControllerIdentity(ctx)
	err := controller.VerifyStorePurchase(ctx)
	var apiError *apierr.Error
	if !errors.As(err, &apiError) || apiError.Status != http.StatusBadRequest {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestWebhookRequestSizeAndGoogleAuthorization(t *testing.T) {
	service := &controllerTestService{}
	controller := NewPaymentSystemController(service, zap.NewNop())
	ctx := newControllerContext(http.MethodPost, "/payments/webhooks/google", `{"message":"ok"}`)
	ctx.SetParamNames("provider")
	ctx.SetParamValues("google")
	ctx.Request().Header.Set("Authorization", "Bearer signed")
	if err := controller.Notify(ctx); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if service.notifyHeader != "Bearer signed" || string(service.notifyPayload) != `{"message":"ok"}` {
		t.Fatalf("webhook data not forwarded: %q / %q", service.notifyHeader, service.notifyPayload)
	}

	ctx = newControllerContext(http.MethodPost, "/payments/webhooks/stripe", strings.Repeat("x", 256<<10+1))
	ctx.SetParamNames("provider")
	ctx.SetParamValues("stripe")
	err := controller.Notify(ctx)
	var apiError *apierr.Error
	if !errors.As(err, &apiError) || apiError.Status != http.StatusBadRequest {
		t.Fatalf("expected oversized body rejection, got %v", err)
	}
}

func newControllerContext(method, target, body string) echo.Context {
	e := echo.New()
	e.Validator = controllerTestValidator{}
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	return e.NewContext(request, httptest.NewRecorder())
}

type controllerTestValidator struct{}

func (controllerTestValidator) Validate(value any) error {
	switch input := value.(type) {
	case *models.PaymentCheckoutInput:
		if input.Provider == "" || input.PriceID == "" || input.SuccessURL == "" || input.CancelURL == "" || input.IdempotencyKey == "" {
			return errors.New("validation failed")
		}
	case *models.PaymentStoreVerificationInput:
		if input.PurchaseToken == "" && input.SignedPayload == "" {
			return errors.New("validation failed")
		}
	}
	return nil
}

func setControllerIdentity(ctx echo.Context) {
	ctx.Set(coremiddleware.ContextKeySession, &ory.Session{Identity: &ory.Identity{Id: "user-1", Traits: map[string]any{"email": "user@example.com"}}})
}
