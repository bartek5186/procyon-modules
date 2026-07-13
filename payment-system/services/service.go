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
	"time"

	"github.com/bartek5186/procyon-modules/payment-system/models"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

var (
	ErrPaymentProviderUnsupported = errors.New("payment provider is not enabled")
	ErrPaymentCapabilityMissing   = errors.New("payment capability is not supported")
	ErrPaymentInvalidSignature    = errors.New("invalid payment webhook signature")
	ErrPaymentInvalidReturnURL    = errors.New("payment return URL is not allowed")
	ErrPaymentSubscriptionActive  = errors.New("an active subscription already exists")
)

type paymentRepository interface {
	ClaimWebhook(context.Context, models.PaymentWebhookEvent, time.Duration) (string, bool, error)
	FinishWebhook(context.Context, string, string, string, error) error
	GetWebhook(context.Context, string, string) (models.PaymentWebhookEvent, error)
	MarkWebhookForRetry(context.Context, string, string) error
	ListFailedWebhooks(context.Context, int, int) ([]models.PaymentWebhookEvent, error)
	CleanupWebhookPayloads(context.Context, time.Time) error
	UpsertPayment(context.Context, models.PaymentEvent) error
	GetPayment(context.Context, string, string) (models.PaymentEvent, error)
	UpsertSubscription(context.Context, models.PaymentSubscription) error
	HasActiveSubscription(context.Context, string) (bool, error)
	ListSubscriptions(context.Context, string, int, int) ([]models.PaymentSubscription, error)
	GetSubscription(context.Context, string, string, string) (models.PaymentSubscription, error)
	GetSubscriptionByExternal(context.Context, string, string) (models.PaymentSubscription, error)
	CustomerID(context.Context, string, string) (string, error)
	ActiveEntitlement(context.Context, string, string) (models.PaymentSubscription, error)
	ListPayments(context.Context, string, int, int) ([]models.PaymentEvent, error)
	ListSubscriptionsAfter(context.Context, uint, int) ([]models.PaymentSubscription, error)
}

type PaymentProvider interface {
	Name() string
}

type paymentPriceProvider interface {
	Prices(context.Context) ([]models.PaymentPriceOption, error)
}

type paymentCheckoutProvider interface {
	CreateCheckout(context.Context, string, string, string, string, string) (string, error)
}

type paymentSubscriptionProvider interface {
	CreateSubscription(context.Context, string, string, string, string, string, string) (string, error)
	CancelSubscription(context.Context, string, bool) error
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

type paymentReconciler interface {
	ReconcileSubscription(context.Context, models.PaymentSubscription) error
}

type paymentProviderFactory func(paymentRepository, *zap.Logger, RuntimeConfig) (PaymentProvider, bool, error)

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
	config         RuntimeConfig
	metricsMu      sync.Mutex
	metrics        map[string]providerMetrics
}

type providerMetrics struct {
	requests, failures uint64
	totalLatency       time.Duration
}

func NewPaymentSystemService(repo paymentRepository, logger *zap.Logger, config RuntimeConfig) (*PaymentSystemService, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	service := &PaymentSystemService{
		repo:           repo,
		logger:         logger,
		providers:      map[string]PaymentProvider{},
		allowedReturns: paymentAllowedReturnOrigins(),
		config:         config,
		metrics:        map[string]providerMetrics{},
	}
	allowedProviders := make(map[string]bool, len(config.EnabledProviders))
	for _, name := range config.EnabledProviders {
		allowedProviders[strings.ToLower(strings.TrimSpace(name))] = true
	}
	paymentFactoriesMu.Lock()
	factories := append([]paymentProviderFactory(nil), paymentFactories...)
	paymentFactoriesMu.Unlock()
	for _, factory := range factories {
		provider, enabled, err := factory(repo, logger, config)
		if err != nil {
			return nil, err
		}
		if !enabled || provider == nil {
			continue
		}
		name := strings.ToLower(provider.Name())
		if !allowedProviders[name] {
			continue
		}
		service.providers[name] = provider
	}
	for _, name := range config.EnabledProviders {
		if service.providers[name] == nil {
			return nil, fmt.Errorf("configured payment provider %s is not usable", name)
		}
	}
	return service, nil
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
	started := time.Now()
	items, callErr := capability.Prices(ctx)
	s.observe(providerName, started, callErr)
	return items, callErr
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
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return "", models.ErrPaymentIdempotencyKey
	}
	if _, err := s.config.Product(input.Provider, input.PriceID, "one_time"); err != nil {
		return "", err
	}
	started := time.Now()
	result, callErr := capability.CreateCheckout(ctx, identityID, input.PriceID, input.SuccessURL, input.CancelURL, input.IdempotencyKey)
	s.observe(input.Provider, started, callErr)
	return result, callErr
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
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return "", models.ErrPaymentIdempotencyKey
	}
	if _, err := s.config.Product(input.Provider, input.PriceID, "subscription"); err != nil {
		return "", err
	}
	started := time.Now()
	result, callErr := capability.CreateSubscription(ctx, identityID, email, input.PriceID, input.SuccessURL, input.CancelURL, input.IdempotencyKey)
	s.observe(input.Provider, started, callErr)
	return result, callErr
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
	started := time.Now()
	callErr := capability.CancelSubscription(ctx, input.SubscriptionID, input.CancelAtPeriodEnd)
	s.observe(input.Provider, started, callErr)
	return callErr
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
	started := time.Now()
	result, callErr := capability.CreatePortal(ctx, customerID, input.ReturnURL)
	s.observe(input.Provider, started, callErr)
	return result, callErr
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
	started := time.Now()
	callErr := capability.HandleWebhook(ctx, payload, signature)
	s.observe(providerName, started, callErr)
	return callErr
}

func (s *PaymentSystemService) VerifyStorePurchase(ctx context.Context, providerName, identityID string, input models.PaymentStoreVerificationInput) error {
	if input.ProductID != "" {
		if _, err := s.config.Product(providerName, input.ProductID, "subscription"); err != nil {
			return err
		}
	}
	provider, err := s.provider(providerName)
	if err != nil {
		return err
	}
	capability, ok := provider.(paymentStoreVerifier)
	if !ok {
		return ErrPaymentCapabilityMissing
	}
	started := time.Now()
	callErr := capability.VerifyStorePurchase(ctx, identityID, input)
	s.observe(providerName, started, callErr)
	if callErr == nil {
		s.logger.Info("payment entitlement verified", zap.String("provider", providerName), zap.String("identity_id", identityID))
	}
	return callErr
}

func (s *PaymentSystemService) PaymentHistory(ctx context.Context, identityID string, limit, offset int) ([]models.PaymentEventResponse, error) {
	if limit <= 0 {
		limit = 50
	}
	items, err := s.repo.ListPayments(ctx, identityID, limit, offset)
	if err != nil {
		return nil, err
	}
	out := make([]models.PaymentEventResponse, 0, len(items))
	for _, item := range items {
		out = append(out, models.PaymentEventResponse{Provider: item.Provider, ExternalID: item.ExternalID,
			SubscriptionID: item.SubscriptionID, PlanCode: item.PriceID, Status: item.Status, Kind: item.Kind,
			AmountMinor: item.AmountMinor, Currency: item.Currency, OccurredAt: item.OccurredAt})
	}
	return out, nil
}

func (s *PaymentSystemService) Entitlement(ctx context.Context, identityID, planCode string) (models.PaymentEntitlementResponse, error) {
	item, err := s.repo.ActiveEntitlement(ctx, identityID, planCode)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return models.PaymentEntitlementResponse{Active: false}, nil
	}
	if err != nil {
		return models.PaymentEntitlementResponse{}, err
	}
	return models.PaymentEntitlementResponse{Active: true, Provider: item.Provider, PlanCode: item.PlanCode,
		Status: item.Status, ExpiresAt: item.CurrentPeriodEnd}, nil
}

func (s *PaymentSystemService) ProviderStatuses() []models.PaymentProviderStatus {
	statuses := make([]models.PaymentProviderStatus, 0, len(s.config.EnabledProviders))
	for _, name := range s.config.EnabledProviders {
		s.metricsMu.Lock()
		metric := s.metrics[name]
		s.metricsMu.Unlock()
		average := float64(0)
		if metric.requests > 0 {
			average = float64(metric.totalLatency.Microseconds()) / 1000 / float64(metric.requests)
		}
		statuses = append(statuses, models.PaymentProviderStatus{Name: name, Ready: s.providers[name] != nil, Requests: metric.requests,
			Failures: metric.failures, AverageLatencyMS: average})
	}
	return statuses
}

func (s *PaymentSystemService) observe(provider string, started time.Time, err error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	s.metricsMu.Lock()
	defer s.metricsMu.Unlock()
	metric := s.metrics[provider]
	metric.requests++
	metric.totalLatency += time.Since(started)
	if err != nil {
		metric.failures++
	}
	s.metrics[provider] = metric
}

func (s *PaymentSystemService) FailedWebhooks(ctx context.Context, limit, offset int) ([]models.PaymentWebhookResponse, error) {
	if limit <= 0 {
		limit = 50
	}
	items, err := s.repo.ListFailedWebhooks(ctx, limit, offset)
	if err != nil {
		return nil, err
	}
	out := make([]models.PaymentWebhookResponse, 0, len(items))
	for _, item := range items {
		out = append(out, models.PaymentWebhookResponse{Provider: item.Provider, EventID: item.EventID,
			EventType: item.EventType, Status: item.Status, Attempts: item.Attempts, LastError: item.LastError,
			LastAttemptAt: item.LastAttemptAt, ProcessedAt: item.ProcessedAt})
	}
	return out, nil
}

func (s *PaymentSystemService) RetryWebhook(ctx context.Context, providerName, eventID string) error {
	event, err := s.repo.GetWebhook(ctx, providerName, eventID)
	if err != nil {
		return err
	}
	if err := s.repo.MarkWebhookForRetry(ctx, providerName, eventID); err != nil {
		return err
	}
	provider, err := s.provider(providerName)
	if err != nil {
		return err
	}
	capability, ok := provider.(paymentWebhookProvider)
	if !ok {
		return ErrPaymentCapabilityMissing
	}
	return capability.HandleWebhook(ctx, event.Payload, event.Signature)
}

func (s *PaymentSystemService) Reconcile(ctx context.Context) error {
	const batchSize = 250
	var afterID uint
	var reconcileErrors []error
	for {
		items, err := s.repo.ListSubscriptionsAfter(ctx, afterID, batchSize)
		if err != nil {
			reconcileErrors = append(reconcileErrors, err)
			break
		}
		for _, item := range items {
			afterID = item.ID
			provider := s.providers[item.Provider]
			reconciler, ok := provider.(paymentReconciler)
			if !ok {
				continue
			}
			if err := reconciler.ReconcileSubscription(ctx, item); err != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("reconcile %s/%s: %w", item.Provider, item.ExternalSubscriptionID, err))
			}
		}
		if len(items) < batchSize {
			break
		}
	}
	if err := s.repo.CleanupWebhookPayloads(ctx, time.Now().UTC().Add(-s.config.WebhookRetention)); err != nil {
		reconcileErrors = append(reconcileErrors, err)
	}
	return errors.Join(reconcileErrors...)
}

func (s *PaymentSystemService) ListSubscriptions(ctx context.Context, identityID string, limit, offset int) ([]models.PaymentSubscriptionResponse, error) {
	if limit <= 0 {
		limit = 50
	}
	items, err := s.repo.ListSubscriptions(ctx, identityID, limit, offset)
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

func logPaymentAudit(logger *zap.Logger, action, provider, identityID, externalID, planCode string, status models.SubscriptionStatus) {
	if logger == nil {
		return
	}
	logger.Info("payment audit", zap.String("action", action), zap.String("provider", provider),
		zap.String("identity_id", identityID), zap.String("external_id", externalID), zap.String("plan_code", planCode),
		zap.String("status", string(status)))
}
