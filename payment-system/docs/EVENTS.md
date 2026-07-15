# Payment System Events

Payment-system publishes typed, provider-neutral contracts through the shared
`procyon-core/events` bus. The bus is synchronous and in-process; the stored
provider webhook remains the durable source responsible for retries.

## One-time purchase completed

Topic: `payment.purchase.completed.v1`

Contract: `contracts.PurchaseCompletedV1`

The payload contains:

- provider and external payment identifier;
- provider Checkout identifier;
- authenticated Procyon identity identifier;
- provider Price ID and stable catalog `plan_code`;
- amount in the currency's minor unit and currency;
- provider-confirmed completion time.

Stripe publishes this event only for a paid one-time Checkout Session. A
pending or failed Checkout, Payment Intent event and subscription Checkout do
not publish it.

The message ID is stable for the provider payment. Reprocessing the same
webhook publishes the same ID. A handler error fails webhook processing so a
provider delivery or the admin retry endpoint can run it again.

## Registering a handler

An application or another compile-time plugin imports only the payment contract
package and registers during startup:

```go
err := coreevents.Subscribe(
    eventBus,
    paymentcontracts.PurchaseCompletedV1Topic,
    "points.credit-purchase",
    pointsService.HandlePurchase,
)
```

Registration must finish before the host calls `eventBus.Seal()`. Payment-system
fails startup when the host has not provided the shared event bus.

## Fulfillment requirements

The handler translates `plan_code` into application behavior, such as granting
1000 points. It must use the message ID as a unique fulfillment key.

The business side effect and fulfillment ledger entry must be committed in one
database transaction. If the ID already exists, the handler returns success
without applying the benefit again. This makes provider retries and repeated
admin retries safe.

Do not grant a benefit from the browser success redirect. The event is emitted
only after payment-system verifies a signed provider webhook and records the
successful payment.

Handlers must remain fast. Slow external calls, email delivery or cross-service
work should use a durable outbox and worker owned by the consuming application.

Refund, dispute and subscription lifecycle events are not part of the v1 event
contract.
