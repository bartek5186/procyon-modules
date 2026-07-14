# Payment API

All routes use JSON. Authenticated routes require the application's Procyon
session. Permission object: `payment_system`, action: `use`.

## Public

- `GET /v1/payments/prices/:provider` - catalog-filtered provider prices.
- `POST /v1/payments/webhooks/:provider` - signed/authenticated provider
  webhook. Payload limit: 256 KiB.

## Authenticated

- `POST /v1/payments/checkout` - one-time Stripe Checkout. Requires
  `Idempotency-Key`.
- `POST /v1/payments/subscriptions/checkout` - subscription Checkout. Requires
  `Idempotency-Key`.
- `GET /v1/payments/subscriptions?limit=50&offset=0` - paginated subscriptions
  owned by the identity.
- `POST /v1/payments/subscriptions/cancel` - immediate or period-end cancel via
  `cancel_at_period_end`.
- `POST /v1/payments/portal` - Stripe billing portal session.
- `POST /v1/payments/verify/:provider` - Google or Apple store verification.
- `GET /v1/payments/history?limit=50&offset=0` - payment history.
- `GET /v1/payments/entitlement?plan_code=premium_monthly` - active entitlement.

Example checkout:

```http
POST /v1/payments/subscriptions/checkout
Idempotency-Key: 24c116c9-c9a3-47bb-bfb1-9b9cb81fc477
Content-Type: application/json

{
  "provider": "stripe",
  "price_id": "price_123",
  "success_url": "https://app.example.com/payment/success",
  "cancel_url": "https://app.example.com/payment/cancel"
}
```

## Admin

- `GET /v1/admin/payments/providers` - provider readiness without secrets.
- `GET /v1/admin/payments/webhooks/failed?limit=50&offset=0` - failed events.
- `POST /v1/admin/payments/webhooks/retry` - retry one failed event.
- `POST /v1/admin/payments/reconcile` - reconcile stored subscriptions.

Stable client error codes include `validation_failed`, `payment_error`,
`payment_ownership_conflict`, `payment_provider_disabled`,
`payment_capability_unsupported`, `payment_product_rejected`,
`payment_return_url_rejected`, `payment_signature_invalid`,
`payment_idempotency_key_invalid`, and `payment_subscription_active`. Provider
internals and credentials are never returned.

## Postman documentation source

The generated Postman collection uses three versioned sources from this module:

- `docs/postman/overview.md` populates the **Payment System → Overview** tab
  with the shared architecture and provider-specific flows.
- Go documentation comments above handlers in `controllers/controller.go`
  populate the request **Docs** tab.
- `docs/postman/payment-system.json` contains named request and response
  variants for Stripe, Google Play, Apple and operational error cases.

The example marked with `"default": true` becomes the main request payload.
All remaining variants are generated as separately named Postman examples.
Provider path variants use a concrete example key and an explicit path value,
for example `GET /v1/payments/prices/stripe` together with
`"path": {"provider": "stripe"}`.

Keep examples executable in structure but safe in content: use test identifiers,
placeholder signatures and synthetic customer data. Never commit live provider
credentials, real webhook signatures, purchase tokens or personal data.
