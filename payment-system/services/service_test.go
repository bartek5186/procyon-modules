package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bartek5186/procyon-modules/payment-system/models"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type fakePaymentModuleRepository struct {
	active               bool
	payments             []models.PaymentEvent
	subscriptions        []models.PaymentSubscription
	webhooks             []models.PaymentWebhookEvent
	webhookResults       []error
	externalSubscription *models.PaymentSubscription
	allSubscriptions     []models.PaymentSubscription
}

func (r *fakePaymentModuleRepository) ClaimWebhook(_ context.Context, event models.PaymentWebhookEvent, _ time.Duration) (string, bool, error) {
	r.webhooks = append(r.webhooks, event)
	return "lease-test", true, nil
}
func (r *fakePaymentModuleRepository) FinishWebhook(_ context.Context, _, _, _ string, processErr error) error {
	r.webhookResults = append(r.webhookResults, processErr)
	return nil
}
func (r *fakePaymentModuleRepository) UpsertPayment(_ context.Context, payment models.PaymentEvent) error {
	r.payments = append(r.payments, payment)
	return nil
}
func (r *fakePaymentModuleRepository) GetPayment(_ context.Context, provider, externalID string) (models.PaymentEvent, error) {
	for _, payment := range r.payments {
		if payment.Provider == provider && payment.ExternalID == externalID {
			return payment, nil
		}
	}
	return models.PaymentEvent{}, gorm.ErrRecordNotFound
}
func (r *fakePaymentModuleRepository) UpsertSubscription(_ context.Context, subscription models.PaymentSubscription) error {
	r.subscriptions = append(r.subscriptions, subscription)
	return nil
}
func (r *fakePaymentModuleRepository) HasActiveSubscription(context.Context, string) (bool, error) {
	return r.active, nil
}
func (r *fakePaymentModuleRepository) ListSubscriptions(context.Context, string, int, int) ([]models.PaymentSubscription, error) {
	return nil, nil
}
func (r *fakePaymentModuleRepository) GetSubscription(context.Context, string, string, string) (models.PaymentSubscription, error) {
	return models.PaymentSubscription{}, gorm.ErrRecordNotFound
}
func (r *fakePaymentModuleRepository) CustomerID(context.Context, string, string) (string, error) {
	return "", nil
}
func (r *fakePaymentModuleRepository) GetWebhook(context.Context, string, string) (models.PaymentWebhookEvent, error) {
	return models.PaymentWebhookEvent{}, gorm.ErrRecordNotFound
}
func (r *fakePaymentModuleRepository) MarkWebhookForRetry(context.Context, string, string) error {
	return nil
}
func (r *fakePaymentModuleRepository) ListFailedWebhooks(context.Context, int, int) ([]models.PaymentWebhookEvent, error) {
	return nil, nil
}
func (r *fakePaymentModuleRepository) CleanupWebhookPayloads(context.Context, time.Time) error {
	return nil
}
func (r *fakePaymentModuleRepository) GetSubscriptionByExternal(context.Context, string, string) (models.PaymentSubscription, error) {
	if r.externalSubscription != nil {
		return *r.externalSubscription, nil
	}
	return models.PaymentSubscription{}, gorm.ErrRecordNotFound
}
func (r *fakePaymentModuleRepository) ActiveEntitlement(context.Context, string, string) (models.PaymentSubscription, error) {
	return models.PaymentSubscription{}, gorm.ErrRecordNotFound
}
func (r *fakePaymentModuleRepository) ListPayments(context.Context, string, int, int) ([]models.PaymentEvent, error) {
	return nil, nil
}
func (r *fakePaymentModuleRepository) ListSubscriptionsAfter(_ context.Context, afterID uint, limit int) ([]models.PaymentSubscription, error) {
	items := make([]models.PaymentSubscription, 0, limit)
	for _, item := range r.allSubscriptions {
		if item.ID <= afterID {
			continue
		}
		items = append(items, item)
		if len(items) == limit {
			break
		}
	}
	return items, nil
}

type fakePaymentModuleProvider struct{}

func (fakePaymentModuleProvider) Name() string { return "fake" }
func (fakePaymentModuleProvider) Prices(context.Context) ([]models.PaymentPriceOption, error) {
	return []models.PaymentPriceOption{
		{ID: "price-1", AmountMinor: 1299, Currency: "pln"},
	}, nil
}
func (fakePaymentModuleProvider) CreateSubscription(context.Context, string, string, string, string, string, string) (string, error) {
	return "https://checkout.example/session", nil
}
func (fakePaymentModuleProvider) CancelSubscription(context.Context, string, bool) error { return nil }

type fakeReconcileProvider struct{ count int }

func (*fakeReconcileProvider) Name() string { return "reconcile" }
func (provider *fakeReconcileProvider) ReconcileSubscription(_ context.Context, subscription models.PaymentSubscription) error {
	provider.count++
	if subscription.ExternalSubscriptionID == "failure" {
		return errors.New("provider failure")
	}
	return nil
}

func newTestPaymentSystem(repo *fakePaymentModuleRepository) *PaymentSystemService {
	return &PaymentSystemService{repo: repo, logger: zap.NewNop(), providers: map[string]PaymentProvider{"fake": fakePaymentModuleProvider{}},
		metrics:        map[string]providerMetrics{},
		allowedReturns: map[string]bool{"https://app.example.com": true}, config: RuntimeConfig{Products: map[string]map[string]Product{
			"fake": {"price-1": {Provider: "fake", ProductID: "price-1", PlanCode: "premium", Kind: "subscription"}},
		}}}
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
		SuccessURL:     "https://attacker.example/success",
		CancelURL:      "https://app.example.com/cancel",
		IdempotencyKey: "test-key",
	})
	if !errors.Is(err, ErrPaymentInvalidReturnURL) {
		t.Fatalf("expected ErrPaymentInvalidReturnURL, got %v", err)
	}
}

func TestPaymentSystemPreventsSecondActiveSubscription(t *testing.T) {
	service := newTestPaymentSystem(&fakePaymentModuleRepository{active: true})
	_, err := service.CreateSubscription(context.Background(), "user-1", "user@example.com", models.PaymentSubscriptionCheckoutInput{
		Provider: "fake", PriceID: "price-1",
		SuccessURL:     "https://app.example.com/success",
		CancelURL:      "https://app.example.com/cancel",
		IdempotencyKey: "test-key",
	})
	if !errors.Is(err, ErrPaymentSubscriptionActive) {
		t.Fatalf("expected ErrPaymentSubscriptionActive, got %v", err)
	}
}

func TestPaymentSystemRejectsProductOutsideCatalog(t *testing.T) {
	service := newTestPaymentSystem(&fakePaymentModuleRepository{})
	_, err := service.CreateSubscription(context.Background(), "user-1", "user@example.com", models.PaymentSubscriptionCheckoutInput{
		Provider: "fake", PriceID: "unknown-price", SuccessURL: "https://app.example.com/success",
		CancelURL: "https://app.example.com/cancel", IdempotencyKey: "test-key",
	})
	if !errors.Is(err, models.ErrPaymentProductRejected) {
		t.Fatalf("expected rejected product, got %v", err)
	}
}

func TestReconcilePagesAllSubscriptionsAndContinuesAfterFailure(t *testing.T) {
	repo := &fakePaymentModuleRepository{}
	for index := 1; index <= 300; index++ {
		externalID := "ok"
		if index == 20 {
			externalID = "failure"
		}
		repo.allSubscriptions = append(repo.allSubscriptions, models.PaymentSubscription{ID: uint(index), Provider: "reconcile",
			ExternalSubscriptionID: externalID})
	}
	provider := &fakeReconcileProvider{}
	service := &PaymentSystemService{repo: repo, providers: map[string]PaymentProvider{"reconcile": provider},
		config: RuntimeConfig{WebhookRetention: time.Hour}}
	if err := service.Reconcile(context.Background()); err == nil {
		t.Fatal("expected aggregate reconciliation error")
	}
	if provider.count != 300 {
		t.Fatalf("reconciled %d subscriptions, want 300", provider.count)
	}
}
