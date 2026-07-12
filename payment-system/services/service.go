package services

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/bartek5186/procyon-modules/payment-system/models"
	"go.uber.org/zap"
)

var (
	ErrPaymentProviderUnsupported = errors.New("payment provider is not enabled")
	ErrPaymentCapabilityMissing   = errors.New("payment capability is not supported")
	ErrPaymentInvalidSignature    = errors.New("invalid payment webhook signature")
	ErrPaymentInvalidReturnURL    = errors.New("payment return URL is not allowed")
	ErrPaymentSubscriptionActive  = errors.New("an active subscription already exists")
)

type paymentRepository interface {
	BeginWebhook(context.Context, string, string, string) (bool, error)
	FinishWebhook(context.Context, string, string, error) error
	UpsertPayment(context.Context, models.PaymentEvent) error
	UpsertSubscription(context.Context, models.PaymentSubscription) error
	HasActiveSubscription(context.Context, string) (bool, error)
	ListSubscriptions(context.Context, string) ([]models.PaymentSubscription, error)
	GetSubscription(context.Context, string, string, string) (models.PaymentSubscription, error)
	CustomerID(context.Context, string, string) (string, error)
}

type PaymentProvider interface {
	Name() string
}

type paymentPriceProvider interface {
	Prices(context.Context) ([]models.PaymentPriceOption, error)
}

type paymentCheckoutProvider interface {
	CreateCheckout(context.Context, string, string, string, string) (string, error)
}

type paymentSubscriptionProvider interface {
	CreateSubscription(context.Context, string, string, string, string, string) (string, error)
	CancelSubscription(context.Context, string) error
}

type paymentPortalProvider interface {
	CreatePortal(context.Context, string, string) (string, error)
}

type paymentWebhookProvider interface {
	HandleWebhook(context.Context, []byte, string) error
}

type paymentStoreVerifier interface {
	VerifyStorePurchase(context.Context, string, models.PaymentStoreVerificationInput) error
}

type paymentProviderFactory func(paymentRepository, *zap.Logger) (PaymentProvider, bool, error)

var (
	paymentFactoriesMu sync.Mutex
	paymentFactories   []paymentProviderFactory
)

func registerPaymentProviderFactory(factory paymentProviderFactory) {
	paymentFactoriesMu.Lock()
	defer paymentFactoriesMu.Unlock()
	paymentFactories = append(paymentFactories, factory)
}

type PaymentSystemService struct {
	repo           paymentRepository
	logger         *zap.Logger
	providers      map[string]PaymentProvider
	allowedReturns map[string]bool
}

func NewPaymentSystemService(repo paymentRepository, logger *zap.Logger, enabledProviders []string) *PaymentSystemService {
	if logger == nil {
		logger = zap.NewNop()
	}
	service := &PaymentSystemService{
		repo:           repo,
		logger:         logger,
		providers:      map[string]PaymentProvider{},
		allowedReturns: paymentAllowedReturnOrigins(),
	}
	allowedProviders := make(map[string]bool, len(enabledProviders))
	for _, name := range enabledProviders {
		allowedProviders[strings.ToLower(strings.TrimSpace(name))] = true
	}
	paymentFactoriesMu.Lock()
	factories := append([]paymentProviderFactory(nil), paymentFactories...)
	paymentFactoriesMu.Unlock()
	for _, factory := range factories {
		provider, enabled, err := factory(repo, logger)
		if err != nil {
			logger.Error("payment provider initialization failed", zap.Error(err))
			continue
		}
		if !enabled || provider == nil {
			continue
		}
		name := strings.ToLower(provider.Name())
		if len(enabledProviders) > 0 && !allowedProviders[name] {
			continue
		}
		service.providers[name] = provider
	}
	return service
}

func (s *PaymentSystemService) Providers() []string {
	out := make([]string, 0, len(s.providers))
	for name := range s.providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (s *PaymentSystemService) provider(name string) (PaymentProvider, error) {
	provider := s.providers[strings.ToLower(strings.TrimSpace(name))]
	if provider == nil {
		return nil, ErrPaymentProviderUnsupported
	}
	return provider, nil
}

func (s *PaymentSystemService) Prices(ctx context.Context, providerName string) ([]models.PaymentPriceOption, error) {
	provider, err := s.provider(providerName)
	if err != nil {
		return nil, err
	}
	capability, ok := provider.(paymentPriceProvider)
	if !ok {
		return nil, ErrPaymentCapabilityMissing
	}
	return capability.Prices(ctx)
}

func (s *PaymentSystemService) CreateCheckout(ctx context.Context, identityID string, input models.PaymentCheckoutInput) (string, error) {
	if err := s.validateReturnURLs(input.SuccessURL, input.CancelURL); err != nil {
		return "", err
	}
	provider, err := s.provider(input.Provider)
	if err != nil {
		return "", err
	}
	capability, ok := provider.(paymentCheckoutProvider)
	if !ok {
		return "", ErrPaymentCapabilityMissing
	}
	return capability.CreateCheckout(ctx, identityID, input.PriceID, input.SuccessURL, input.CancelURL)
}

func (s *PaymentSystemService) CreateSubscription(ctx context.Context, identityID, email string, input models.PaymentSubscriptionCheckoutInput) (string, error) {
	if err := s.validateReturnURLs(input.SuccessURL, input.CancelURL); err != nil {
		return "", err
	}
	active, err := s.repo.HasActiveSubscription(ctx, identityID)
	if err != nil {
		return "", err
	}
	if active {
		return "", ErrPaymentSubscriptionActive
	}
	provider, err := s.provider(input.Provider)
	if err != nil {
		return "", err
	}
	capability, ok := provider.(paymentSubscriptionProvider)
	if !ok {
		return "", ErrPaymentCapabilityMissing
	}
	return capability.CreateSubscription(ctx, identityID, email, input.PriceID, input.SuccessURL, input.CancelURL)
}

func (s *PaymentSystemService) CancelSubscription(ctx context.Context, identityID string, input models.PaymentCancelSubscriptionInput) error {
	if _, err := s.repo.GetSubscription(ctx, input.Provider, input.SubscriptionID, identityID); err != nil {
		return err
	}
	provider, err := s.provider(input.Provider)
	if err != nil {
		return err
	}
	capability, ok := provider.(paymentSubscriptionProvider)
	if !ok {
		return ErrPaymentCapabilityMissing
	}
	return capability.CancelSubscription(ctx, input.SubscriptionID)
}

func (s *PaymentSystemService) CreatePortal(ctx context.Context, identityID string, input models.PaymentPortalInput) (string, error) {
	if err := s.validateReturnURLs(input.ReturnURL); err != nil {
		return "", err
	}
	provider, err := s.provider(input.Provider)
	if err != nil {
		return "", err
	}
	capability, ok := provider.(paymentPortalProvider)
	if !ok {
		return "", ErrPaymentCapabilityMissing
	}
	customerID, err := s.repo.CustomerID(ctx, input.Provider, identityID)
	if err != nil {
		return "", err
	}
	if customerID == "" {
		return "", fmt.Errorf("payment customer not found")
	}
	return capability.CreatePortal(ctx, customerID, input.ReturnURL)
}

func (s *PaymentSystemService) Notify(ctx context.Context, providerName string, payload []byte, signature string) error {
	provider, err := s.provider(providerName)
	if err != nil {
		return err
	}
	capability, ok := provider.(paymentWebhookProvider)
	if !ok {
		return ErrPaymentCapabilityMissing
	}
	return capability.HandleWebhook(ctx, payload, signature)
}

func (s *PaymentSystemService) VerifyStorePurchase(ctx context.Context, providerName, identityID string, input models.PaymentStoreVerificationInput) error {
	provider, err := s.provider(providerName)
	if err != nil {
		return err
	}
	capability, ok := provider.(paymentStoreVerifier)
	if !ok {
		return ErrPaymentCapabilityMissing
	}
	return capability.VerifyStorePurchase(ctx, identityID, input)
}

func (s *PaymentSystemService) ListSubscriptions(ctx context.Context, identityID string) ([]models.PaymentSubscriptionResponse, error) {
	items, err := s.repo.ListSubscriptions(ctx, identityID)
	if err != nil {
		return nil, err
	}
	out := make([]models.PaymentSubscriptionResponse, 0, len(items))
	for _, item := range items {
		out = append(out, models.PaymentSubscriptionResponse{
			Provider:           item.Provider,
			SubscriptionID:     item.ExternalSubscriptionID,
			PlanCode:           item.PlanCode,
			Status:             item.Status,
			CurrentPeriodStart: item.CurrentPeriodStart,
			CurrentPeriodEnd:   item.CurrentPeriodEnd,
			CancelAtPeriodEnd:  item.CancelAtPeriodEnd,
		})
	}
	return out, nil
}

func (s *PaymentSystemService) validateReturnURLs(values ...string) error {
	for _, value := range values {
		parsed, err := url.Parse(strings.TrimSpace(value))
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return ErrPaymentInvalidReturnURL
		}
		origin := strings.ToLower(parsed.Scheme + "://" + parsed.Host)
		if !s.allowedReturns[origin] {
			return ErrPaymentInvalidReturnURL
		}
	}
	return nil
}

func paymentAllowedReturnOrigins() map[string]bool {
	raw := strings.TrimSpace(os.Getenv("PAYMENT_ALLOWED_RETURN_ORIGINS"))
	if raw == "" {
		raw = "http://localhost,http://127.0.0.1"
	}
	out := map[string]bool{}
	for _, origin := range strings.Split(raw, ",") {
		origin = strings.TrimRight(strings.ToLower(strings.TrimSpace(origin)), "/")
		if origin != "" {
			out[origin] = true
		}
	}
	return out
}
