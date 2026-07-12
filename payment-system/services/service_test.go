package services

import (
	"context"
	"errors"
	"testing"

	"github.com/bartek5186/procyon-modules/payment-system/models"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type fakePaymentModuleRepository struct {
	active bool
}

func (r *fakePaymentModuleRepository) BeginWebhook(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (r *fakePaymentModuleRepository) FinishWebhook(context.Context, string, string, error) error {
	return nil
}
func (r *fakePaymentModuleRepository) UpsertPayment(context.Context, models.PaymentEvent) error {
	return nil
}
func (r *fakePaymentModuleRepository) UpsertSubscription(context.Context, models.PaymentSubscription) error {
	return nil
}
func (r *fakePaymentModuleRepository) HasActiveSubscription(context.Context, string) (bool, error) {
	return r.active, nil
}
func (r *fakePaymentModuleRepository) ListSubscriptions(context.Context, string) ([]models.PaymentSubscription, error) {
	return nil, nil
}
func (r *fakePaymentModuleRepository) GetSubscription(context.Context, string, string, string) (models.PaymentSubscription, error) {
	return models.PaymentSubscription{}, gorm.ErrRecordNotFound
}
func (r *fakePaymentModuleRepository) CustomerID(context.Context, string, string) (string, error) {
	return "", nil
}

type fakePaymentModuleProvider struct{}

func (fakePaymentModuleProvider) Name() string { return "fake" }
func (fakePaymentModuleProvider) Prices(context.Context) ([]models.PaymentPriceOption, error) {
	return []models.PaymentPriceOption{
		{ID: "price-1", AmountMinor: 1299, Currency: "pln"},
	}, nil
}
func (fakePaymentModuleProvider) CreateSubscription(context.Context, string, string, string, string, string) (string, error) {
	return "https://checkout.example/session", nil
}
func (fakePaymentModuleProvider) CancelSubscription(context.Context, string) error { return nil }

func newTestPaymentSystem(repo *fakePaymentModuleRepository) *PaymentSystemService {
	service := NewPaymentSystemService(repo, zap.NewNop(), nil)
	service.providers = map[string]PaymentProvider{"fake": fakePaymentModuleProvider{}}
	service.allowedReturns = map[string]bool{"https://app.example.com": true}
	return service
}

func TestPaymentSystemPricesUsesSelectedProvider(t *testing.T) {
	service := newTestPaymentSystem(&fakePaymentModuleRepository{})
	items, err := service.Prices(context.Background(), "fake")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(items) != 1 || items[0].AmountMinor != 1299 {
		t.Fatalf("unexpected prices: %+v", items)
	}
}

func TestPaymentSystemRejectsUnknownProvider(t *testing.T) {
	service := newTestPaymentSystem(&fakePaymentModuleRepository{})
	_, err := service.Prices(context.Background(), "missing")
	if !errors.Is(err, ErrPaymentProviderUnsupported) {
		t.Fatalf("expected ErrPaymentProviderUnsupported, got %v", err)
	}
}

func TestPaymentSystemRejectsUntrustedReturnOrigin(t *testing.T) {
	service := newTestPaymentSystem(&fakePaymentModuleRepository{})
	_, err := service.CreateSubscription(context.Background(), "user-1", "user@example.com", models.PaymentSubscriptionCheckoutInput{
		Provider: "fake", PriceID: "price-1",
		SuccessURL: "https://attacker.example/success",
		CancelURL:  "https://app.example.com/cancel",
	})
	if !errors.Is(err, ErrPaymentInvalidReturnURL) {
		t.Fatalf("expected ErrPaymentInvalidReturnURL, got %v", err)
	}
}

func TestPaymentSystemPreventsSecondActiveSubscription(t *testing.T) {
	service := newTestPaymentSystem(&fakePaymentModuleRepository{active: true})
	_, err := service.CreateSubscription(context.Background(), "user-1", "user@example.com", models.PaymentSubscriptionCheckoutInput{
		Provider: "fake", PriceID: "price-1",
		SuccessURL: "https://app.example.com/success",
		CancelURL:  "https://app.example.com/cancel",
	})
	if !errors.Is(err, ErrPaymentSubscriptionActive) {
		t.Fatalf("expected ErrPaymentSubscriptionActive, got %v", err)
	}
}
