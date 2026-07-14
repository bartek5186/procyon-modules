The Payment System plugin provides one server-side payment model for Stripe,
Google Play, and the Apple App Store. Applications use the same history,
subscription, entitlement, webhook, and operational APIs regardless of where a
purchase originated.

The backend is the source of truth. A browser redirect or a successful response
from a mobile billing SDK is not enough to grant access. Access becomes valid
only after the provider data has been verified and normalized into the local
payment tables.

## Provider capabilities

| Capability | Stripe | Google Play | Apple App Store |
| --- | --- | --- | --- |
| One-time hosted checkout | Yes | No | No |
| Subscription creation | Stripe Checkout | Created in the Android app | Created in the iOS app |
| Initial server verification | Signed webhook | Google Play Developer API | StoreKit 2 signed transaction |
| Lifecycle updates | Stripe webhooks | Real-time developer notifications | App Store Server Notifications V2 |
| Customer self-service | Billing Portal | Managed by Google Play | Managed by the App Store |
| Server reconciliation | Stripe API | Google Play Developer API | App Store Server API |

`payment-products.json` is the server-side allowlist connecting external Price
or Product IDs with stable application `plan_code` values. Products absent from
that catalog are rejected even if they exist at the provider.

## Common access flow

1. The signed-in user starts a purchase in the web or mobile client.
2. The provider completes the financial transaction.
3. The backend verifies provider authenticity, product, application identity,
   environment, and ownership of the purchase.
4. The module stores a normalized payment or subscription linked to the Procyon
   identity ID.
5. `GET /v1/payments/entitlement` resolves current access from normalized local
   state, not from client-provided purchase data.
6. Provider notifications and periodic reconciliation keep that state current
   after renewals, cancellations, billing failures, refunds, disputes, grace
   periods, expiration, and revocation.

Authenticated payment endpoints require the `payment_system:use` permission.
Public webhook endpoints are authenticated by the provider-specific signature
or identity mechanism. Admin endpoints use the application's admin key.

## Stripe flow

### One-time payment

1. The client loads allowed Stripe prices from
   `GET /v1/payments/prices/stripe`.
2. It calls `POST /v1/payments/checkout` with a catalog Price ID, allowed return
   URLs, and a unique `Idempotency-Key` header.
3. The backend creates a Stripe Checkout Session with the Procyon identity ID
   in Stripe metadata and returns `checkout_url`.
4. The browser completes payment on Stripe. The success redirect is only a UX
   signal and must not grant access by itself.
5. Signed Stripe webhooks update the normalized payment record. Payment history
   becomes available through `GET /v1/payments/history`.

### Stripe subscription

1. The client calls `POST /v1/payments/subscriptions/checkout` with a recurring
   catalog Price ID and an `Idempotency-Key`.
2. The backend reuses the identity's existing Stripe Customer when possible and
   creates a subscription Checkout Session.
3. Checkout, invoice, and subscription webhooks create and update the local
   subscription. Entitlement follows the verified subscription status.
4. `POST /v1/payments/portal` creates a Billing Portal session for customer
   self-service.
5. `POST /v1/payments/subscriptions/cancel` supports immediate cancellation or
   cancellation at the end of the paid period.

Stripe webhook signatures use `STRIPE_WEBHOOK_SECRET`. Delivery is idempotent:
an event is claimed with a processing lease, duplicate live claims are ignored,
and interrupted or failed events can be retried safely.

## Google Play flow

Google subscriptions are purchased in the Android application through Google
Play Billing. The backend does not create a Google checkout session.

1. The Android client selects a configured subscription Product ID and starts
   the Google Play Billing flow.
2. The billing request sets `obfuscatedAccountId` to the module-defined SHA-256
   binding of the authenticated Procyon identity. This prevents a valid token
   from being attached to another account.
3. After purchase, the client sends `product_id`, `purchase_token`, and
   `package_name` to `POST /v1/payments/verify/google`.
4. The backend calls Google Play `subscriptionsv2.get`, verifies package,
   product catalog membership and account binding, acknowledges a pending
   purchase, and stores the normalized subscription.
5. The raw purchase token is encrypted at rest. Lookup and ownership checks use
   a one-way digest rather than exposing the token.
6. Google Real-time Developer Notifications arrive through authenticated
   Pub/Sub push at `POST /v1/payments/webhooks/google`. The backend validates the
   OIDC bearer token and configured audience before processing the event.
7. Renewals, grace periods, pauses, cancellations, expiration, revoke, void and
   refund events update subscription and payment state. Reconciliation queries
   Google again if a notification is missed.

The application should check `GET /v1/payments/entitlement` after verification
and whenever it resumes. It must not grant premium access merely because the
local Billing SDK reports a purchase.

## Apple App Store flow

Apple subscriptions are purchased in the iOS application with StoreKit 2. The
backend does not create an Apple checkout session.

1. Before starting the StoreKit purchase, the app sets `appAccountToken` to the
   authenticated Procyon identity UUID.
2. StoreKit returns a signed transaction after the App Store completes the
   purchase.
3. The client sends `signed_payload` to
   `POST /v1/payments/verify/apple`.
4. The backend verifies the JWS certificate chain against the configured Apple
   root, performs the configured online certificate checks, and validates the
   bundle ID, App Apple ID, Sandbox or Production environment, Product ID, and
   `appAccountToken` ownership.
5. A valid transaction is normalized into the local subscription model and can
   produce an entitlement.
6. App Store Server Notifications V2 arrive at
   `POST /v1/payments/webhooks/apple`. Their signed transaction and renewal data
   update renewals, billing retry, grace period, expiration, refund, revocation,
   and auto-renew state.
7. Periodic or admin-triggered reconciliation uses the App Store Server API and
   a short-lived ES256 token to repair state after missed notifications.

The signed transaction is evidence to be verified by the backend; clients must
not decode it and treat its fields as trusted authorization data.

## Webhooks, retries, and reconciliation

Webhook records move through `PROCESSING`, `SUCCEEDED`, and `FAILED`. Processing
leases make provider retries and concurrent deliveries safe. Failed deliveries
are visible through `GET /v1/admin/payments/webhooks/failed` and can be retried
with `POST /v1/admin/payments/webhooks/retry`.

All enabled providers are reconciled periodically according to
`PAYMENT_RECONCILE_EVERY`. Operators can also run
`POST /v1/admin/payments/reconcile`. Webhooks are the normal fast path;
reconciliation is the recovery path.

## Integration rules

- Treat provider redirects and client SDK results as pending until backend
  verification succeeds.
- Use a new `Idempotency-Key` for each logical Stripe purchase attempt and reuse
  it only when retrying the same attempt.
- Never accept arbitrary provider product identifiers from business logic;
  maintain them in the server-side product catalog.
- Query the entitlement endpoint for authorization. Payment history is an audit
  view and is not itself proof of current access.
- Keep provider secrets, Google service-account data, Apple private keys,
  purchase tokens, and webhook signatures out of clients and source control.
- Use the admin provider-status endpoint for readiness and metrics without
  exposing credentials.
