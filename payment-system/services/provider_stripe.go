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

	coreevents "github.com/bartek5186/procyon-core/events"
	"github.com/bartek5186/procyon-modules/payment-system/contracts"
	"github.com/bartek5186/procyon-modules/payment-system/models"
	"github.com/stripe/stripe-go/v84"
	"github.com/stripe/stripe-go/v84/webhook"
	"go.uber.org/zap"
)

const stripePaymentProviderName = "stripe"

type stripePaymentProvider struct {
	repo                 paymentRepository
	client               *stripe.Client
	logger               *zap.Logger
	webhookSecret        string
	trialDays            int
	cacheMu              sync.RWMutex
	prices               []models.PaymentPriceOption
	pricesAt             time.Time
	products             map[string]Product
	webhookLease         time.Duration
	eventBus             *coreevents.Bus
	resolveCheckoutPrice func(context.Context, string) (string, error)
}

func init() {
	registerPaymentProviderFactory(newStripePaymentProvider)
}

func newStripePaymentProvider(repo paymentRepository, logger *zap.Logger, eventBus *coreevents.Bus, config RuntimeConfig) (PaymentProvider, bool, error) {
	if !containsProvider(config.EnabledProviders, stripePaymentProviderName) {
		return nil, false, nil
	}
	secretKey := strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY"))
	if secretKey == "" {
		return nil, false, errors.New("STRIPE_SECRET_KEY is required when Stripe is enabled")
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
		products:      config.Products[stripePaymentProviderName],
		webhookLease:  config.WebhookLease,
		eventBus:      eventBus,
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
		productConfig, allowed := p.products[price.ID]
		if !allowed {
			continue
		}
		if productConfig.Currency != "" && !strings.EqualFold(productConfig.Currency, string(price.Currency)) {
			return nil, fmt.Errorf("Stripe price %s currency does not match payment catalog", price.ID)
		}
		if productConfig.AmountMinor > 0 && productConfig.AmountMinor != price.UnitAmount {
			return nil, fmt.Errorf("Stripe price %s amount does not match payment catalog", price.ID)
		}
		priceKind := "one_time"
		interval := ""
		if price.Recurring != nil {
			priceKind = "subscription"
			interval = fmt.Sprintf("%d-%s", price.Recurring.IntervalCount, price.Recurring.Interval)
		}
		if productConfig.Kind != priceKind || (productConfig.Interval != "" && productConfig.Interval != interval) {
			return nil, fmt.Errorf("Stripe price %s kind or interval does not match payment catalog", price.ID)
		}
		option := models.PaymentPriceOption{
			ID:          price.ID,
			AmountMinor: price.UnitAmount,
			Currency:    string(price.Currency),
			Description: firstPaymentValue(price.Nickname, price.Metadata["description"], productConfig.PlanCode),
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

func (p *stripePaymentProvider) CreateCheckout(ctx context.Context, identityID, priceID, successURL, cancelURL, idempotencyKey string) (string, error) {
	product, ok := p.products[priceID]
	if !ok || product.Kind != "one_time" {
		return "", models.ErrPaymentProductRejected
	}
	metadata := stripeCheckoutMetadata(identityID, priceID, product.PlanCode)
	params := &stripe.CheckoutSessionCreateParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{
			{Price: stripe.String(priceID), Quantity: stripe.Int64(1)},
		},
		SuccessURL:        stripe.String(successURL),
		CancelURL:         stripe.String(cancelURL),
		Metadata:          metadata,
		PaymentIntentData: &stripe.CheckoutSessionCreatePaymentIntentDataParams{Metadata: metadata},
	}
	params.SetIdempotencyKey(idempotencyKey)
	session, err := p.client.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return "", err
	}
	return session.URL, nil
}

func (p *stripePaymentProvider) CreateSubscription(ctx context.Context, identityID, email, priceID, successURL, cancelURL, idempotencyKey string) (string, error) {
	product, ok := p.products[priceID]
	if !ok || product.Kind != "subscription" {
		return "", models.ErrPaymentProductRejected
	}
	metadata := stripeCheckoutMetadata(identityID, priceID, product.PlanCode)
	params := &stripe.CheckoutSessionCreateParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{
			{Price: stripe.String(priceID), Quantity: stripe.Int64(1)},
		},
		SuccessURL: stripe.String(successURL),
		CancelURL:  stripe.String(cancelURL),
		Metadata:   metadata,
		SubscriptionData: &stripe.CheckoutSessionCreateSubscriptionDataParams{
			Metadata: metadata,
		},
	}
	if customerID, err := p.repo.CustomerID(ctx, p.Name(), identityID); err != nil {
		return "", err
	} else if customerID != "" {
		params.Customer = stripe.String(customerID)
	} else if strings.TrimSpace(email) != "" {
		params.CustomerEmail = stripe.String(email)
	}
	params.SetIdempotencyKey(idempotencyKey)
	if p.trialDays > 0 {
		params.SubscriptionData.TrialEnd = stripe.Int64(time.Now().UTC().Add(time.Duration(p.trialDays) * 24 * time.Hour).Unix())
	}
	session, err := p.client.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return "", err
	}
	return session.URL, nil
}

func (p *stripePaymentProvider) CancelSubscription(ctx context.Context, subscriptionID string, atPeriodEnd bool) error {
	var err error
	if atPeriodEnd {
		_, err = p.client.V1Subscriptions.Update(ctx, subscriptionID, &stripe.SubscriptionUpdateParams{CancelAtPeriodEnd: stripe.Bool(true)})
	} else {
		_, err = p.client.V1Subscriptions.Cancel(ctx, subscriptionID, nil)
	}
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
	leaseID, claimed, err := p.repo.ClaimWebhook(ctx, models.PaymentWebhookEvent{Provider: p.Name(), EventID: event.ID,
		EventType: string(event.Type), Payload: payload, Signature: signature}, p.webhookLease)
	if err != nil || !claimed {
		return err
	}
	processErr := p.handleStripeEvent(ctx, event)
	if finishErr := p.repo.FinishWebhook(ctx, p.Name(), event.ID, leaseID, processErr); finishErr != nil && processErr == nil {
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

	case stripe.EventTypeCheckoutSessionCompleted, stripe.EventTypeCheckoutSessionAsyncPaymentSucceeded,
		stripe.EventTypeCheckoutSessionAsyncPaymentFailed:
		var session stripe.CheckoutSession
		if err := json.Unmarshal(event.Data.Raw, &session); err != nil {
			return err
		}
		return p.handleStripeCheckout(ctx, &session, event.ID, time.Unix(event.Created, 0).UTC(), event.Type)

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

	case stripe.EventTypeRefundCreated, stripe.EventTypeRefundUpdated, stripe.EventTypeRefundFailed:
		var refund stripe.Refund
		if err := json.Unmarshal(event.Data.Raw, &refund); err != nil {
			return err
		}
		status := models.PaymentStatusRefunded
		if refund.Status == stripe.RefundStatusFailed {
			status = models.PaymentStatusFailed
		}
		identityID := refund.Metadata["identity_id"]
		if identityID == "" && refund.PaymentIntent != nil {
			identityID = refund.PaymentIntent.Metadata["identity_id"]
			if identityID == "" {
				if original, lookupErr := p.repo.GetPayment(ctx, p.Name(), refund.PaymentIntent.ID); lookupErr == nil {
					identityID = original.IdentityID
				}
			}
		}
		return p.repo.UpsertPayment(ctx, models.PaymentEvent{Provider: p.Name(), ExternalID: refund.ID,
			IdentityID: identityID, Status: status, Kind: "refund", AmountMinor: refund.Amount,
			Currency: string(refund.Currency), OccurredAt: time.Unix(event.Created, 0).UTC()})

	case stripe.EventTypeChargeDisputeCreated, stripe.EventTypeChargeDisputeUpdated,
		stripe.EventTypeChargeDisputeClosed, stripe.EventTypeChargeDisputeFundsWithdrawn,
		stripe.EventTypeChargeDisputeFundsReinstated:
		var dispute stripe.Dispute
		if err := json.Unmarshal(event.Data.Raw, &dispute); err != nil {
			return err
		}
		status := models.PaymentStatusDisputed
		if dispute.Status == stripe.DisputeStatusWon || dispute.Status == stripe.DisputeStatusPrevented {
			status = models.PaymentStatusSucceeded
		}
		identityID := dispute.Metadata["identity_id"]
		if identityID == "" && dispute.PaymentIntent != nil {
			identityID = dispute.PaymentIntent.Metadata["identity_id"]
			if identityID == "" {
				if original, lookupErr := p.repo.GetPayment(ctx, p.Name(), dispute.PaymentIntent.ID); lookupErr == nil {
					identityID = original.IdentityID
				}
			}
		}
		return p.repo.UpsertPayment(ctx, models.PaymentEvent{Provider: p.Name(), ExternalID: dispute.ID,
			IdentityID: identityID, Status: status, Kind: "dispute", AmountMinor: dispute.Amount,
			Currency: string(dispute.Currency), OccurredAt: time.Unix(event.Created, 0).UTC()})
	}
	return nil
}

func (p *stripePaymentProvider) handleStripeCheckout(ctx context.Context, session *stripe.CheckoutSession, providerEventID string, occurredAt time.Time, eventType stripe.EventType) error {
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
		Status:         stripeCheckoutPaymentStatus(session, eventType),
		Kind:           "one_time",
		AmountMinor:    session.AmountTotal,
		Currency:       string(session.Currency),
		OccurredAt:     occurredAt,
	}
	if session.Mode == stripe.CheckoutSessionModeSubscription {
		payment.Kind = "subscription_cycle"
	}
	var product Product
	if payment.Status == models.PaymentStatusSucceeded && session.Mode == stripe.CheckoutSessionModePayment {
		if strings.TrimSpace(identityID) == "" {
			return errors.New("Stripe Checkout is missing Procyon identity metadata")
		}
		var err error
		payment.PriceID, product, err = p.checkoutProduct(ctx, session)
		if err != nil {
			return err
		}
	} else if session.Metadata != nil {
		payment.PriceID = strings.TrimSpace(session.Metadata["price_id"])
	}
	if err := p.repo.UpsertPayment(ctx, payment); err != nil {
		return err
	}
	if payment.Status == models.PaymentStatusSucceeded && session.Mode == stripe.CheckoutSessionModePayment {
		if p.eventBus == nil {
			return errors.New("payment-system requires the Procyon event bus to publish completed purchases")
		}
		return coreevents.Publish(ctx, p.eventBus, contracts.PurchaseCompletedV1Topic, coreevents.Message[contracts.PurchaseCompletedV1]{
			ID:            fmt.Sprintf("%s:%s:%s", contracts.PurchaseCompletedV1Topic, p.Name(), payment.ExternalID),
			OccurredAt:    occurredAt,
			Source:        "payment-system",
			CorrelationID: providerEventID,
			Payload: contracts.PurchaseCompletedV1{
				Provider: p.Name(), ExternalPaymentID: payment.ExternalID, CheckoutID: session.ID,
				IdentityID: identityID, PriceID: payment.PriceID, PlanCode: product.PlanCode,
				AmountMinor: payment.AmountMinor, Currency: strings.ToUpper(payment.Currency), CompletedAt: occurredAt,
			},
		})
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

func (p *stripePaymentProvider) ReconcileSubscription(ctx context.Context, stored models.PaymentSubscription) error {
	subscription, err := p.client.V1Subscriptions.Retrieve(ctx, stored.ExternalSubscriptionID, nil)
	if err != nil {
		return err
	}
	return p.upsertStripeSubscription(ctx, subscription, stored.IdentityID)
}

func stripeCheckoutPaymentStatus(session *stripe.CheckoutSession, eventType stripe.EventType) models.PaymentStatus {
	if eventType == stripe.EventTypeCheckoutSessionAsyncPaymentFailed {
		return models.PaymentStatusFailed
	}
	if eventType == stripe.EventTypeCheckoutSessionAsyncPaymentSucceeded || session.PaymentStatus == stripe.CheckoutSessionPaymentStatusPaid || session.PaymentStatus == stripe.CheckoutSessionPaymentStatusNoPaymentRequired {
		return models.PaymentStatusSucceeded
	}
	return models.PaymentStatusPending
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
			catalogPlanCode := ""
			if product, ok := p.products[item.Price.ID]; ok {
				catalogPlanCode = product.PlanCode
			}
			model.PlanCode = firstPaymentValue(item.Price.Metadata["plan_code"], subscription.Metadata["plan_code"], catalogPlanCode, item.Price.ID)
		}
	}
	if err := p.repo.UpsertSubscription(ctx, model); err != nil {
		return err
	}
	logPaymentAudit(p.logger, "entitlement_changed", p.Name(), model.IdentityID, model.ExternalSubscriptionID, model.PlanCode, model.Status)
	return nil
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

func stripeCheckoutMetadata(identityID, priceID, planCode string) map[string]string {
	return map[string]string{
		"identity_id": strings.TrimSpace(identityID),
		"price_id":    strings.TrimSpace(priceID),
		"plan_code":   strings.TrimSpace(planCode),
	}
}

func (p *stripePaymentProvider) checkoutProduct(ctx context.Context, session *stripe.CheckoutSession) (string, Product, error) {
	priceID := ""
	if session.Metadata != nil {
		priceID = strings.TrimSpace(session.Metadata["price_id"])
	}
	if priceID == "" && session.LineItems != nil && len(session.LineItems.Data) > 0 && session.LineItems.Data[0] != nil && session.LineItems.Data[0].Price != nil {
		priceID = session.LineItems.Data[0].Price.ID
	}
	if priceID == "" {
		var err error
		if p.resolveCheckoutPrice != nil {
			priceID, err = p.resolveCheckoutPrice(ctx, session.ID)
		} else {
			priceID, err = p.loadCheckoutPrice(ctx, session.ID)
		}
		if err != nil {
			return "", Product{}, fmt.Errorf("resolve Stripe Checkout %s price: %w", session.ID, err)
		}
	}
	product, ok := p.products[priceID]
	if !ok || product.Kind != "one_time" {
		return "", Product{}, models.ErrPaymentProductRejected
	}
	if session.Metadata != nil {
		if planCode := strings.TrimSpace(session.Metadata["plan_code"]); planCode != "" && planCode != product.PlanCode {
			return "", Product{}, models.ErrPaymentProductRejected
		}
	}
	return priceID, product, nil
}

func (p *stripePaymentProvider) loadCheckoutPrice(ctx context.Context, sessionID string) (string, error) {
	if p.client == nil || p.client.V1CheckoutSessions == nil {
		return "", errors.New("Stripe Checkout client is unavailable")
	}
	params := &stripe.CheckoutSessionListLineItemsParams{Session: stripe.String(sessionID)}
	for item, err := range p.client.V1CheckoutSessions.ListLineItems(ctx, params) {
		if err != nil {
			return "", err
		}
		if item != nil && item.Price != nil && strings.TrimSpace(item.Price.ID) != "" {
			return item.Price.ID, nil
		}
	}
	return "", errors.New("Stripe Checkout has no price line item")
}
