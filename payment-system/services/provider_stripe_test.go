package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	coreevents "github.com/bartek5186/procyon-core/events"
	"github.com/bartek5186/procyon-modules/payment-system/contracts"
	"github.com/bartek5186/procyon-modules/payment-system/models"
	"github.com/stripe/stripe-go/v84"
	"github.com/stripe/stripe-go/v84/webhook"
	"go.uber.org/zap"
)

func TestStripeWebhookVerifiesSignatureAndRecordsAsyncFailure(t *testing.T) {
	repo := &fakePaymentModuleRepository{}
	provider := &stripePaymentProvider{repo: repo, logger: zap.NewNop(), webhookSecret: "whsec_test", webhookLease: time.Minute}
	object := map[string]any{
		"id": "cs_test_1", "object": "checkout.session", "mode": "payment",
		"payment_status": "unpaid", "amount_total": 1299, "currency": "pln",
		"metadata": map[string]string{"identity_id": "user-1"},
	}
	payload, err := json.Marshal(map[string]any{
		"id": "evt_test_1", "object": "event", "api_version": stripe.APIVersion,
		"created": time.Now().UTC().Unix(), "type": string(stripe.EventTypeCheckoutSessionAsyncPaymentFailed),
		"data": map[string]any{"object": object},
	})
	if err != nil {
		t.Fatal(err)
	}
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: provider.webhookSecret})
	if err := provider.HandleWebhook(context.Background(), payload, signed.Header); err != nil {
		t.Fatalf("handle signed Stripe webhook: %v", err)
	}
	if len(repo.payments) != 1 || repo.payments[0].Status != models.PaymentStatusFailed || repo.payments[0].IdentityID != "user-1" {
		t.Fatalf("unexpected payment: %+v", repo.payments)
	}
	if err := provider.HandleWebhook(context.Background(), payload, "t=1,v1=bad"); err != ErrPaymentInvalidSignature {
		t.Fatalf("expected signature error, got %v", err)
	}
}

func TestStripePaidCheckoutPublishesPurchaseCompleted(t *testing.T) {
	repo := &fakePaymentModuleRepository{}
	bus := coreevents.New()
	var received coreevents.Message[contracts.PurchaseCompletedV1]
	if err := coreevents.Subscribe(bus, contracts.PurchaseCompletedV1Topic, "test.fulfillment", func(_ context.Context, message coreevents.Message[contracts.PurchaseCompletedV1]) error {
		received = message
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	bus.Seal()
	provider := &stripePaymentProvider{
		repo: repo, logger: zap.NewNop(), webhookSecret: "whsec_test", webhookLease: time.Minute,
		eventBus: bus, products: map[string]Product{"price_points": {
			Provider: "stripe", ProductID: "price_points", PlanCode: "points_1000", Kind: "one_time",
		}},
	}
	payload, signature := stripeCheckoutWebhook(t, provider.webhookSecret, "evt_paid", stripe.EventTypeCheckoutSessionCompleted, map[string]any{
		"id": "cs_paid", "object": "checkout.session", "mode": "payment", "payment_status": "paid",
		"amount_total": 4999, "currency": "pln", "payment_intent": "pi_paid",
		"metadata": stripeCheckoutMetadata("user-1", "price_points", "points_1000"),
	})
	if err := provider.HandleWebhook(context.Background(), payload, signature); err != nil {
		t.Fatalf("handle webhook: %v", err)
	}
	if len(repo.payments) != 1 || repo.payments[0].PriceID != "price_points" || repo.payments[0].Status != models.PaymentStatusSucceeded {
		t.Fatalf("payment: %+v", repo.payments)
	}
	want := contracts.PurchaseCompletedV1{
		Provider: "stripe", ExternalPaymentID: "pi_paid", CheckoutID: "cs_paid", IdentityID: "user-1",
		PriceID: "price_points", PlanCode: "points_1000", AmountMinor: 4999, Currency: "PLN",
		CompletedAt: received.OccurredAt,
	}
	if received.ID != "payment.purchase.completed.v1:stripe:pi_paid" || received.Source != "payment-system" || received.CorrelationID != "evt_paid" || !reflect.DeepEqual(received.Payload, want) {
		t.Fatalf("event: %+v", received)
	}
}

func TestStripePaidCheckoutResolvesLegacyPrice(t *testing.T) {
	repo := &fakePaymentModuleRepository{}
	bus := coreevents.New()
	var received contracts.PurchaseCompletedV1
	if err := coreevents.Subscribe(bus, contracts.PurchaseCompletedV1Topic, "test.legacy", func(_ context.Context, message coreevents.Message[contracts.PurchaseCompletedV1]) error {
		received = message.Payload
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	bus.Seal()
	provider := &stripePaymentProvider{
		repo: repo, logger: zap.NewNop(), webhookSecret: "whsec_test", webhookLease: time.Minute, eventBus: bus,
		products:             map[string]Product{"price_legacy": {Provider: "stripe", ProductID: "price_legacy", PlanCode: "points_legacy", Kind: "one_time"}},
		resolveCheckoutPrice: func(context.Context, string) (string, error) { return "price_legacy", nil },
	}
	payload, signature := stripeCheckoutWebhook(t, provider.webhookSecret, "evt_legacy", stripe.EventTypeCheckoutSessionCompleted, map[string]any{
		"id": "cs_legacy", "object": "checkout.session", "mode": "payment", "payment_status": "paid",
		"amount_total": 1200, "currency": "pln", "payment_intent": "pi_legacy",
		"metadata": map[string]string{"identity_id": "user-legacy"},
	})
	if err := provider.HandleWebhook(context.Background(), payload, signature); err != nil {
		t.Fatalf("handle legacy webhook: %v", err)
	}
	if received.PriceID != "price_legacy" || received.PlanCode != "points_legacy" {
		t.Fatalf("legacy event: %+v", received)
	}
}

func TestStripePurchaseHandlerFailureMakesWebhookRetryable(t *testing.T) {
	repo := &fakePaymentModuleRepository{}
	bus := coreevents.New()
	wanted := errors.New("temporary fulfillment failure")
	attempts := 0
	var messageIDs []string
	if err := coreevents.Subscribe(bus, contracts.PurchaseCompletedV1Topic, "test.retry", func(_ context.Context, message coreevents.Message[contracts.PurchaseCompletedV1]) error {
		attempts++
		messageIDs = append(messageIDs, message.ID)
		if attempts == 1 {
			return wanted
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	bus.Seal()
	provider := &stripePaymentProvider{
		repo: repo, logger: zap.NewNop(), webhookSecret: "whsec_test", webhookLease: time.Minute, eventBus: bus,
		products: map[string]Product{"price_retry": {Provider: "stripe", ProductID: "price_retry", PlanCode: "points_retry", Kind: "one_time"}},
	}
	payload, signature := stripeCheckoutWebhook(t, provider.webhookSecret, "evt_retry", stripe.EventTypeCheckoutSessionCompleted, map[string]any{
		"id": "cs_retry", "object": "checkout.session", "mode": "payment", "payment_status": "paid",
		"amount_total": 1000, "currency": "pln", "payment_intent": "pi_retry",
		"metadata": stripeCheckoutMetadata("user-retry", "price_retry", "points_retry"),
	})
	if err := provider.HandleWebhook(context.Background(), payload, signature); !errors.Is(err, wanted) {
		t.Fatalf("first webhook: %v", err)
	}
	if err := provider.HandleWebhook(context.Background(), payload, signature); err != nil {
		t.Fatalf("retried webhook: %v", err)
	}
	if len(repo.webhookResults) != 2 || !errors.Is(repo.webhookResults[0], wanted) || repo.webhookResults[1] != nil {
		t.Fatalf("webhook results: %+v", repo.webhookResults)
	}
	if len(messageIDs) != 2 || messageIDs[0] != messageIDs[1] {
		t.Fatalf("message IDs: %+v", messageIDs)
	}
}

func TestStripeDuplicateDeliveryCanBeFulfilledIdempotently(t *testing.T) {
	repo := &fakePaymentModuleRepository{}
	bus := coreevents.New()
	fulfilled := map[string]bool{}
	points := 0
	if err := coreevents.Subscribe(bus, contracts.PurchaseCompletedV1Topic, "test.points-ledger", func(_ context.Context, message coreevents.Message[contracts.PurchaseCompletedV1]) error {
		if fulfilled[message.ID] {
			return nil
		}
		fulfilled[message.ID] = true
		points += 1000
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	bus.Seal()
	provider := &stripePaymentProvider{
		repo: repo, logger: zap.NewNop(), webhookSecret: "whsec_test", webhookLease: time.Minute, eventBus: bus,
		products: map[string]Product{"price_points": {Provider: "stripe", ProductID: "price_points", PlanCode: "points_1000", Kind: "one_time"}},
	}
	payload, signature := stripeCheckoutWebhook(t, provider.webhookSecret, "evt_duplicate", stripe.EventTypeCheckoutSessionCompleted, map[string]any{
		"id": "cs_duplicate", "object": "checkout.session", "mode": "payment", "payment_status": "paid",
		"amount_total": 4999, "currency": "pln", "payment_intent": "pi_duplicate",
		"metadata": stripeCheckoutMetadata("user-1", "price_points", "points_1000"),
	})
	for attempt := 0; attempt < 2; attempt++ {
		if err := provider.HandleWebhook(context.Background(), payload, signature); err != nil {
			t.Fatalf("delivery %d: %v", attempt+1, err)
		}
	}
	if points != 1000 || len(fulfilled) != 1 {
		t.Fatalf("points=%d fulfilled=%v", points, fulfilled)
	}
}

func TestStripeDoesNotPublishPurchaseForNonFinalOrNonCheckoutEvents(t *testing.T) {
	tests := []struct {
		name      string
		eventType stripe.EventType
		object    map[string]any
	}{
		{"pending checkout", stripe.EventTypeCheckoutSessionCompleted, map[string]any{
			"id": "cs_pending", "object": "checkout.session", "mode": "payment", "payment_status": "unpaid",
			"amount_total": 1000, "currency": "pln", "metadata": map[string]string{"identity_id": "user-1"},
		}},
		{"failed checkout", stripe.EventTypeCheckoutSessionAsyncPaymentFailed, map[string]any{
			"id": "cs_failed", "object": "checkout.session", "mode": "payment", "payment_status": "unpaid",
			"amount_total": 1000, "currency": "pln", "metadata": map[string]string{"identity_id": "user-1"},
		}},
		{"payment intent", stripe.EventTypePaymentIntentSucceeded, map[string]any{
			"id": "pi_succeeded", "object": "payment_intent", "status": "succeeded", "amount": 1000,
			"currency": "pln", "metadata": map[string]string{"identity_id": "user-1", "price_id": "price_1"},
		}},
		{"subscription checkout", stripe.EventTypeCheckoutSessionCompleted, map[string]any{
			"id": "cs_subscription", "object": "checkout.session", "mode": "subscription", "payment_status": "paid",
			"amount_total": 1000, "currency": "pln", "metadata": map[string]string{"identity_id": "user-1"},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := &fakePaymentModuleRepository{}
			bus := coreevents.New()
			calls := 0
			if err := coreevents.Subscribe(bus, contracts.PurchaseCompletedV1Topic, "test.no-event", func(context.Context, coreevents.Message[contracts.PurchaseCompletedV1]) error {
				calls++
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			bus.Seal()
			provider := &stripePaymentProvider{repo: repo, logger: zap.NewNop(), webhookSecret: "whsec_test", webhookLease: time.Minute, eventBus: bus}
			payload, signature := stripeCheckoutWebhook(t, provider.webhookSecret, "evt_no_event", test.eventType, test.object)
			if err := provider.HandleWebhook(context.Background(), payload, signature); err != nil {
				t.Fatalf("handle webhook: %v", err)
			}
			if calls != 0 {
				t.Fatalf("published %d purchase events", calls)
			}
		})
	}
}

func TestStripePaidCheckoutRequiresIdentityMetadata(t *testing.T) {
	bus := coreevents.New()
	bus.Seal()
	provider := &stripePaymentProvider{
		repo: &fakePaymentModuleRepository{}, logger: zap.NewNop(), webhookSecret: "whsec_test",
		webhookLease: time.Minute, eventBus: bus,
		products: map[string]Product{"price_points": {Provider: "stripe", ProductID: "price_points", PlanCode: "points_1000", Kind: "one_time"}},
	}
	payload, signature := stripeCheckoutWebhook(t, provider.webhookSecret, "evt_no_identity", stripe.EventTypeCheckoutSessionCompleted, map[string]any{
		"id": "cs_no_identity", "object": "checkout.session", "mode": "payment", "payment_status": "paid",
		"amount_total": 4999, "currency": "pln", "payment_intent": "pi_no_identity",
		"metadata": map[string]string{"price_id": "price_points", "plan_code": "points_1000"},
	})
	if err := provider.HandleWebhook(context.Background(), payload, signature); err == nil || err.Error() != "Stripe Checkout is missing Procyon identity metadata" {
		t.Fatalf("unexpected missing identity result: %v", err)
	}
}

func TestStripeCheckoutStatus(t *testing.T) {
	tests := []struct {
		name      string
		status    stripe.CheckoutSessionPaymentStatus
		eventType stripe.EventType
		expected  models.PaymentStatus
	}{
		{"completed but unpaid", stripe.CheckoutSessionPaymentStatusUnpaid, stripe.EventTypeCheckoutSessionCompleted, models.PaymentStatusPending},
		{"completed and paid", stripe.CheckoutSessionPaymentStatusPaid, stripe.EventTypeCheckoutSessionCompleted, models.PaymentStatusSucceeded},
		{"async success", stripe.CheckoutSessionPaymentStatusUnpaid, stripe.EventTypeCheckoutSessionAsyncPaymentSucceeded, models.PaymentStatusSucceeded},
		{"async failure", stripe.CheckoutSessionPaymentStatusUnpaid, stripe.EventTypeCheckoutSessionAsyncPaymentFailed, models.PaymentStatusFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := stripeCheckoutPaymentStatus(&stripe.CheckoutSession{PaymentStatus: test.status}, test.eventType)
			if actual != test.expected {
				t.Fatalf("expected %s, got %s (%s)", test.expected, actual, fmt.Sprint(test.status))
			}
		})
	}
}

func TestStripeDisputeWebhookRecordsDisputedPayment(t *testing.T) {
	repo := &fakePaymentModuleRepository{payments: []models.PaymentEvent{{Provider: "stripe", ExternalID: "pi_original", IdentityID: "user-1"}}}
	provider := &stripePaymentProvider{repo: repo, logger: zap.NewNop(), webhookSecret: "whsec_test", webhookLease: time.Minute}
	payload, _ := json.Marshal(map[string]any{
		"id": "evt_dispute", "object": "event", "api_version": stripe.APIVersion, "created": time.Now().UTC().Unix(),
		"type": string(stripe.EventTypeChargeDisputeCreated), "data": map[string]any{"object": map[string]any{
			"id": "dp_1", "object": "dispute", "amount": 1299, "currency": "pln", "status": "needs_response",
			"payment_intent": "pi_original",
		}},
	})
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: provider.webhookSecret})
	if err := provider.HandleWebhook(context.Background(), payload, signed.Header); err != nil {
		t.Fatal(err)
	}
	if len(repo.payments) != 2 || repo.payments[1].Status != models.PaymentStatusDisputed || repo.payments[1].Kind != "dispute" || repo.payments[1].IdentityID != "user-1" {
		t.Fatalf("unexpected dispute: %+v", repo.payments)
	}
}

func TestStripeCheckoutMetadataContainsServerCatalogIdentity(t *testing.T) {
	metadata := stripeCheckoutMetadata("user-1", "price-1", "points_1000")
	want := map[string]string{"identity_id": "user-1", "price_id": "price-1", "plan_code": "points_1000"}
	if !reflect.DeepEqual(metadata, want) {
		t.Fatalf("metadata: %+v", metadata)
	}
}

func stripeCheckoutWebhook(t *testing.T, secret, eventID string, eventType stripe.EventType, object map[string]any) ([]byte, string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"id": eventID, "object": "event", "api_version": stripe.APIVersion,
		"created": time.Now().UTC().Unix(), "type": string(eventType), "data": map[string]any{"object": object},
	})
	if err != nil {
		t.Fatal(err)
	}
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: secret})
	return payload, signed.Header
}
