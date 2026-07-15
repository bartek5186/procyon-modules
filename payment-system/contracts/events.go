// Package contracts contains the stable event contracts published by the
// payment-system module. Consumers depend on this package instead of payment
// provider or persistence internals.
package contracts

import (
	"time"

	coreevents "github.com/bartek5186/procyon-core/events"
)

// PurchaseCompletedV1Topic is emitted after a one-time purchase is confirmed
// by a payment provider and recorded by payment-system.
const PurchaseCompletedV1Topic coreevents.Topic[PurchaseCompletedV1] = "payment.purchase.completed.v1"

// PurchaseCompletedV1 is the provider-neutral payload used to fulfill a
// one-time purchase in the consuming application.
type PurchaseCompletedV1 struct {
	Provider          string
	ExternalPaymentID string
	CheckoutID        string
	IdentityID        string
	PriceID           string
	PlanCode          string
	AmountMinor       int64
	Currency          string
	CompletedAt       time.Time
}
