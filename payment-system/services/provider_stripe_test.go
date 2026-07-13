package services

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

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
