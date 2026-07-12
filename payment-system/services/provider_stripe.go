package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bartek5186/procyon-modules/payment-system/models"
	"github.com/stripe/stripe-go/v84"
	"github.com/stripe/stripe-go/v84/webhook"
	"go.uber.org/zap"
)

const stripePaymentProviderName = "stripe"

type stripePaymentProvider struct {
	repo          paymentRepository
	client        *stripe.Client
	logger        *zap.Logger
	webhookSecret string
	trialDays     int
	cacheMu       sync.RWMutex
	prices        []models.PaymentPriceOption
	pricesAt      time.Time
}

func init() {
	registerPaymentProviderFactory(newStripePaymentProvider)
}

func newStripePaymentProvider(repo paymentRepository, logger *zap.Logger) (PaymentProvider, bool, error) {
	secretKey := strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY"))
	if secretKey == "" {
		return nil, false, nil
	}
	webhookSecret := strings.TrimSpace(os.Getenv("STRIPE_WEBHOOK_SECRET"))
	if webhookSecret == "" {
		return nil, false, errors.New("STRIPE_WEBHOOK_SECRET is required when Stripe is enabled")
	}
	trialDays := 0
	if raw := strings.TrimSpace(os.Getenv("STRIPE_TRIAL_DAYS")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 || value > 365 {
			return nil, false, fmt.Errorf("STRIPE_TRIAL_DAYS must be between 0 and 365")
		}
		trialDays = value
	}
	return &stripePaymentProvider{
		repo:          repo,
		client:        stripe.NewClient(secretKey),
		logger:        logger,
		webhookSecret: webhookSecret,
		trialDays:     trialDays,
	}, true, nil
}

func (p *stripePaymentProvider) Name() string { return stripePaymentProviderName }

func (p *stripePaymentProvider) Prices(ctx context.Context) ([]models.PaymentPriceOption, error) {
	p.cacheMu.RLock()
	if time.Since(p.pricesAt) < 30*time.Minute && len(p.prices) > 0 {
		out := append([]models.PaymentPriceOption(nil), p.prices...)
		p.cacheMu.RUnlock()
		return out, nil
	}
	p.cacheMu.RUnlock()

	params := &stripe.PriceListParams{Active: stripe.Bool(true)}
	params.AddExpand("data.product")
	prices := make([]models.PaymentPriceOption, 0)
	for price, err := range p.client.V1Prices.List(ctx, params) {
		if err != nil {
			return nil, err
		}
		option := models.PaymentPriceOption{
			ID:          price.ID,
			AmountMinor: price.UnitAmount,
			Currency:    string(price.Currency),
			Description: firstPaymentValue(price.Nickname, price.Metadata["description"]),
			Kind:        string(price.Type),
		}
		if price.Recurring != nil {
			option.Interval = fmt.Sprintf("%d-%s", price.Recurring.IntervalCount, price.Recurring.Interval)
		}
		if price.Product != nil {
			option.ProductID = price.Product.ID
			option.ProductName = price.Product.Name
		}
		prices = append(prices, option)
	}
	p.cacheMu.Lock()
	p.prices = append([]models.PaymentPriceOption(nil), prices...)
	p.pricesAt = time.Now().UTC()
	p.cacheMu.Unlock()
	return prices, nil
}

func (p *stripePaymentProvider) CreateCheckout(ctx context.Context, identityID, priceID, successURL, cancelURL string) (string, error) {
	session, err := p.client.V1CheckoutSessions.Create(ctx, &stripe.CheckoutSessionCreateParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{
			{Price: stripe.String(priceID), Quantity: stripe.Int64(1)},
		},
		SuccessURL: stripe.String(successURL),
		CancelURL:  stripe.String(cancelURL),
		Metadata:   map[string]string{"identity_id": identityID},
	})
	if err != nil {
		return "", err
	}
	return session.URL, nil
}

func (p *stripePaymentProvider) CreateSubscription(ctx context.Context, identityID, email, priceID, successURL, cancelURL string) (string, error) {
	params := &stripe.CheckoutSessionCreateParams{
		Mode:          stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		CustomerEmail: stripe.String(email),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{
			{Price: stripe.String(priceID), Quantity: stripe.Int64(1)},
		},
		SuccessURL: stripe.String(successURL),
		CancelURL:  stripe.String(cancelURL),
		Metadata:   map[string]string{"identity_id": identityID},
		SubscriptionData: &stripe.CheckoutSessionCreateSubscriptionDataParams{
			Metadata: map[string]string{"identity_id": identityID},
		},
	}
	if p.trialDays > 0 {
		params.SubscriptionData.TrialEnd = stripe.Int64(time.Now().UTC().Add(time.Duration(p.trialDays) * 24 * time.Hour).Unix())
	}
	session, err := p.client.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return "", err
	}
	return session.URL, nil
}

func (p *stripePaymentProvider) CancelSubscription(ctx context.Context, subscriptionID string) error {
	_, err := p.client.V1Subscriptions.Cancel(ctx, subscriptionID, nil)
	return err
}

func (p *stripePaymentProvider) CreatePortal(ctx context.Context, customerID, returnURL string) (string, error) {
	session, err := p.client.V1BillingPortalSessions.Create(ctx, &stripe.BillingPortalSessionCreateParams{
		Customer: stripe.String(customerID), ReturnURL: stripe.String(returnURL),
	})
	if err != nil {
		return "", err
	}
	return session.URL, nil
}

func (p *stripePaymentProvider) HandleWebhook(ctx context.Context, payload []byte, signature string) error {
	event, err := webhook.ConstructEvent(payload, signature, p.webhookSecret)
	if err != nil {
		return ErrPaymentInvalidSignature
	}
	claimed, err := p.repo.BeginWebhook(ctx, p.Name(), event.ID, string(event.Type))
	if err != nil || !claimed {
		return err
	}
	processErr := p.handleStripeEvent(ctx, event)
	if finishErr := p.repo.FinishWebhook(ctx, p.Name(), event.ID, processErr); finishErr != nil && processErr == nil {
		return finishErr
	}
	return processErr
}

func (p *stripePaymentProvider) handleStripeEvent(ctx context.Context, event stripe.Event) error {
	switch event.Type {
	case stripe.EventTypePaymentIntentCreated,
		stripe.EventTypePaymentIntentSucceeded,
		stripe.EventTypePaymentIntentPaymentFailed,
		stripe.EventTypePaymentIntentCanceled:
		var intent stripe.PaymentIntent
		if err := json.Unmarshal(event.Data.Raw, &intent); err != nil {
			return err
		}
		status := models.PaymentStatusPending
		switch event.Type {
		case stripe.EventTypePaymentIntentSucceeded:
			status = models.PaymentStatusSucceeded
		case stripe.EventTypePaymentIntentPaymentFailed:
			status = models.PaymentStatusFailed
		case stripe.EventTypePaymentIntentCanceled:
			status = models.PaymentStatusCanceled
		}
		return p.repo.UpsertPayment(ctx, models.PaymentEvent{
			Provider:    p.Name(),
			ExternalID:  intent.ID,
			IdentityID:  intent.Metadata["identity_id"],
			CustomerID:  stripeCustomerID(intent.Customer),
			Status:      status,
			Kind:        "one_time",
			AmountMinor: intent.Amount,
			Currency:    string(intent.Currency),
			OccurredAt:  time.Unix(event.Created, 0).UTC(),
		})

	case stripe.EventTypeCheckoutSessionCompleted:
		var session stripe.CheckoutSession
		if err := json.Unmarshal(event.Data.Raw, &session); err != nil {
			return err
		}
		return p.handleStripeCheckout(ctx, &session, time.Unix(event.Created, 0).UTC())

	case stripe.EventTypeInvoicePaid, stripe.EventTypeInvoicePaymentFailed:
		var invoice stripe.Invoice
		if err := json.Unmarshal(event.Data.Raw, &invoice); err != nil {
			return err
		}
		return p.handleStripeInvoice(ctx, &invoice, event.Type == stripe.EventTypeInvoicePaid, time.Unix(event.Created, 0).UTC())

	case stripe.EventTypeCustomerSubscriptionUpdated, stripe.EventTypeCustomerSubscriptionDeleted:
		var subscription stripe.Subscription
		if err := json.Unmarshal(event.Data.Raw, &subscription); err != nil {
			return err
		}
		return p.upsertStripeSubscription(ctx, &subscription, subscription.Metadata["identity_id"])
	}
	return nil
}

func (p *stripePaymentProvider) handleStripeCheckout(ctx context.Context, session *stripe.CheckoutSession, occurredAt time.Time) error {
	externalID := session.ID
	if session.PaymentIntent != nil && session.PaymentIntent.ID != "" {
		externalID = session.PaymentIntent.ID
	} else if session.Invoice != nil && session.Invoice.ID != "" {
		externalID = session.Invoice.ID
	}
	identityID := session.Metadata["identity_id"]
	subscriptionID := ""
	if session.Subscription != nil {
		subscriptionID = session.Subscription.ID
	}
	payment := models.PaymentEvent{
		Provider:       p.Name(),
		ExternalID:     externalID,
		IdentityID:     identityID,
		CustomerID:     stripeCustomerID(session.Customer),
		SubscriptionID: subscriptionID,
		Status:         models.PaymentStatusSucceeded,
		Kind:           "one_time",
		AmountMinor:    session.AmountTotal,
		Currency:       string(session.Currency),
		OccurredAt:     occurredAt,
	}
	if session.Mode == stripe.CheckoutSessionModeSubscription {
		payment.Kind = "subscription_cycle"
	}
	if err := p.repo.UpsertPayment(ctx, payment); err != nil {
		return err
	}
	if subscriptionID == "" {
		return nil
	}
	subscription, err := p.client.V1Subscriptions.Retrieve(ctx, subscriptionID, nil)
	if err != nil {
		return err
	}
	return p.upsertStripeSubscription(ctx, subscription, identityID)
}

func (p *stripePaymentProvider) handleStripeInvoice(ctx context.Context, invoice *stripe.Invoice, paid bool, occurredAt time.Time) error {
	status := models.PaymentStatusFailed
	amount := invoice.AmountDue
	if paid {
		status = models.PaymentStatusSucceeded
		amount = invoice.AmountPaid
	}
	subscriptionID, identityID := stripeInvoiceSubscription(invoice)
	if err := p.repo.UpsertPayment(ctx, models.PaymentEvent{
		Provider:       p.Name(),
		ExternalID:     invoice.ID,
		IdentityID:     identityID,
		CustomerID:     stripeCustomerID(invoice.Customer),
		SubscriptionID: subscriptionID,
		Status:         status,
		Kind:           "subscription_cycle",
		AmountMinor:    amount,
		Currency:       string(invoice.Currency),
		OccurredAt:     occurredAt,
	}); err != nil {
		return err
	}
	if subscriptionID == "" {
		return nil
	}
	subscription, err := p.client.V1Subscriptions.Retrieve(ctx, subscriptionID, nil)
	if err != nil {
		return err
	}
	if identityID == "" {
		identityID = subscription.Metadata["identity_id"]
	}
	return p.upsertStripeSubscription(ctx, subscription, identityID)
}

func (p *stripePaymentProvider) upsertStripeSubscription(ctx context.Context, subscription *stripe.Subscription, identityID string) error {
	if subscription == nil || subscription.ID == "" {
		return nil
	}
	model := models.PaymentSubscription{
		Provider:               p.Name(),
		ExternalSubscriptionID: subscription.ID,
		IdentityID:             identityID,
		CustomerID:             stripeCustomerID(subscription.Customer),
		Status:                 mapStripePaymentSubscriptionStatus(subscription.Status),
		CancelAtPeriodEnd:      subscription.CancelAtPeriodEnd,
	}
	if subscription.CancelAt != 0 {
		cancelAt := time.Unix(subscription.CancelAt, 0).UTC()
		model.CancelAt = &cancelAt
	}
	if subscription.Items != nil && len(subscription.Items.Data) > 0 && subscription.Items.Data[0] != nil {
		item := subscription.Items.Data[0]
		model.CurrentPeriodStart = time.Unix(item.CurrentPeriodStart, 0).UTC()
		model.CurrentPeriodEnd = time.Unix(item.CurrentPeriodEnd, 0).UTC()
		if item.Price != nil {
			model.PlanCode = firstPaymentValue(item.Price.Metadata["plan_code"], item.Price.ID)
		}
	}
	return p.repo.UpsertSubscription(ctx, model)
}

func stripeInvoiceSubscription(invoice *stripe.Invoice) (string, string) {
	if invoice == nil || invoice.Parent == nil || invoice.Parent.SubscriptionDetails == nil {
		return "", ""
	}
	details := invoice.Parent.SubscriptionDetails
	subscriptionID := ""
	if details.Subscription != nil {
		subscriptionID = details.Subscription.ID
	}
	return subscriptionID, details.Metadata["identity_id"]
}

func stripeCustomerID(customer *stripe.Customer) string {
	if customer == nil {
		return ""
	}
	return customer.ID
}

func mapStripePaymentSubscriptionStatus(status stripe.SubscriptionStatus) models.SubscriptionStatus {
	switch status {
	case stripe.SubscriptionStatusActive:
		return models.SubscriptionStatusActive
	case stripe.SubscriptionStatusTrialing:
		return models.SubscriptionStatusTrialing
	case stripe.SubscriptionStatusPastDue, stripe.SubscriptionStatusUnpaid:
		return models.SubscriptionStatusPastDue
	case stripe.SubscriptionStatusCanceled, stripe.SubscriptionStatusIncompleteExpired:
		return models.SubscriptionStatusCanceled
	default:
		return models.SubscriptionStatusExpired
	}
}

func firstPaymentValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
