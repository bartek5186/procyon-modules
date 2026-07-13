# Payment System

Versioned payments, subscriptions, entitlements, and provider reconciliation for
Procyon applications. The module is linked as a normal Go dependency; its code
is not copied into the application.

## Providers

- Stripe: catalog-filtered prices, one-time and subscription Checkout,
  idempotency, customer reuse, billing portal, immediate or period-end
  cancellation, signed webhooks, asynchronous payments, refunds, and
  reconciliation.
- Google Play: SubscriptionsV2 verification, account binding, encrypted purchase
  tokens, acknowledgement, authenticated Pub/Sub RTDN, voided purchases, and
  reconciliation.
- Apple App Store: App Store Server Notification V2 and StoreKit 2 transaction
  JWS verification, OCSP checks, application/environment checks, renewal
  information, App Store Server API reconciliation, and ownership through
  `appAccountToken`.

## Install

```bash
procyon-cli module add payment-system --provider stripe
procyon-cli module add payment-system --provider stripe,google,apple
```

Copy `docs/products.example.json` to the application configuration directory,
replace all provider IDs, and set `PAYMENT_PRODUCTS_FILE`. The module refuses to
start if a selected provider, product catalog, credential, or required security
setting is missing.

Then run application migrations:

```bash
go run . -migrate=true
```

Migrations are ordered and recorded in `payment_module_migrations`. Disabling
the plugin removes its runtime wiring but intentionally preserves payment data.

## Security contract

- Every checkout request requires an `Idempotency-Key` header.
- Return URLs must match `PAYMENT_ALLOWED_RETURN_ORIGINS`.
- Only products declared in the server-side catalog can be sold or verified.
- A verified external subscription cannot be reassigned to another identity.
- Google purchase tokens are encrypted at rest and indexed by SHA-256 digest.
- Google Pub/Sub pushes require an OIDC bearer token with the configured
  audience.
- Apple transactions require `appAccountToken` to equal the authenticated
  Procyon identity ID.
- Webhooks use atomic processing leases and can recover after interrupted
  processing; a stale worker cannot finalize a lease taken by another worker.

See [configuration](docs/CONFIGURATION.md), [API](docs/API.md),
[operations](docs/OPERATIONS.md), and [data handling](docs/DATA_HANDLING.md).

## Verification

```bash
go test -race ./...
go vet ./...
```

CI also runs migration tests against PostgreSQL and MySQL. Live sandbox tests
use real provider credentials and are described in `docs/OPERATIONS.md`.
