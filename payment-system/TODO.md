# Payment System TODO / completion report

Audit opened: 2026-07-13. Engineering work completed: 2026-07-13.

This file now records the production-readiness work completed for `v0.3.0`.
The only remaining release gate is live provider sandbox validation, which
requires application-owned accounts, products, credentials, and console setup.

## P0 - production blockers

### Provider startup and configuration

- [x] Plugin startup fails when a selected provider is unavailable or invalid.
- [x] Unknown provider names are rejected.
- [x] Stripe key/webhook secret, Google service account/package/Push audience,
      and Apple application/server API configuration are required and validated.
- [x] Typed shared/provider configuration is composed once at startup.
- [x] Missing and partially configured providers have deterministic tests.

### Purchase ownership

- [x] The first owner of an external subscription is immutable in the store.
- [x] Cross-account attempts return a dedicated ownership conflict.
- [x] Google verifies `obfuscatedExternalAccountId` against the Procyon identity.
- [x] Google always uses the configured package and rejects client mismatch.
- [x] Apple requires `appAccountToken` to match the authenticated identity.
- [x] Google and Apple replay/cross-account paths have tests.

### Webhook processing

- [x] Webhook states are `PROCESSING`, `SUCCEEDED`, and `FAILED`.
- [x] Claims are atomic and contain lease ID, attempt count, and timestamps.
- [x] Interrupted work can be reclaimed after the configured lease timeout.
- [x] A stale worker cannot finalize a lease owned by a newer worker.
- [x] Errors are bounded and finalization failures leave recoverable state.
- [x] Duplicate, failure/retry, stale lease, and stale finalizer paths are tested.

### Product catalogue

- [x] A server-owned provider product allowlist is required.
- [x] Provider/product/kind/plan/currency/amount/interval fields are validated.
- [x] Stripe exposes only allowed active prices and checks provider values.
- [x] Provider product IDs map to stable internal plan codes.
- [x] Unknown, disabled, wrong-kind, and mismatched products are rejected.

### Migrations

- [x] Unused standalone SQL files were removed.
- [x] Ordered module migrations are recorded in `payment_module_migrations`.
- [x] Clean install and idempotent upgrade tests exist.
- [x] PostgreSQL 17 and MySQL 8.4 migration tests run in CI.
- [x] Disable/removal data behavior is documented.

### Apple verification

- [x] ES256, Apple leaf/intermediate OIDs, exact chain, validity time, configured
      root, future signed date, application, bundle, and environment are checked.
- [x] Production online OCSP checks cover leaf and intermediate certificates.
- [x] Valid, untrusted-chain, wrong-environment, ownership, notification, refund,
      grace-period, and reconciliation JWS fixtures are tested.

## P1 - correctness and lifecycle

### Stripe

- [x] One-time PaymentIntent metadata carries `identity_id`.
- [x] Checkout status is derived instead of always succeeding.
- [x] Async success/failure, PaymentIntent, invoice, subscription, refund, and
      dispute events are handled.
- [x] Stripe API requests use client-provided idempotency keys.
- [x] Immediate and period-end cancellation are explicit API choices.
- [x] Existing customers are reused and non-empty owners cannot be erased.

### Google Play

- [x] Verification uses `purchases.subscriptionsv2.get`.
- [x] Authenticated Pub/Sub RTDN handles test, subscription, and void/refund
      notifications.
- [x] Active, grace, hold, pause, canceled, expired, revoked, and refund states
      are represented and tested.
- [x] Renewals are stored as payment events.
- [x] Pending purchases are acknowledged; already-acknowledged conflicts are
      idempotent.
- [x] Purchase tokens are SHA-256 indexed and AES-256-GCM encrypted at rest.

### Apple lifecycle

- [x] Signed transaction and renewal payloads, subtype/environment, auto-renew,
      expiration intent, grace, expiration, refund, and revoke are processed.
- [x] Valid notifications without transaction data complete safely.
- [x] Payment amount/currency comes from the trusted product catalogue.
- [x] App Store Server API `Get All Subscription Statuses` reconciliation uses a
      short-lived ES256 JWT and verifies returned JWS data.

### Entitlements and API

- [x] Provider-independent entitlement query by internal plan is available.
- [x] Authenticated entitlement and paginated history/subscription endpoints
      are available.
- [x] Trials/grace remain entitled; past-due/canceled/expired/refunded/revoked do
      not.
- [x] Store verification and all request/query fields are validated and bounded.
- [x] Provider rejection paths map to stable safe 4xx codes.
- [x] Provider internals, credentials, and raw tokens are excluded from public
      responses and logs.

### Operations and observability

- [x] Scheduled/on-demand reconciliation covers Stripe, Google, and Apple.
- [x] Admin APIs expose readiness, failed webhooks, safe retry, and reconciliation.
- [x] Per-provider requests, failures, and average latency are reported.
- [x] Structured entitlement-change audit logs are emitted.
- [x] Successful webhook payload cleanup follows configured retention.

## P2 - maintenance and documentation

- [x] Plugin values own non-secret paths/durations; secrets stay in environment
      variables or the referenced secret files.
- [x] Runtime provider selection and compile-time dependency behavior are clear.
- [x] Google token encryption, webhook retention, accounting preservation, and
      identity deletion implications are documented.
- [x] Every route, auth mode, stable error, retry behavior, reconciliation flow,
      and incident path is documented.
- [x] Stripe CLI, Google Play, and Apple sandbox runbooks are included.
- [x] Module Postman examples cover checkout, subscriptions, history,
      entitlement, admin operations, and all three webhook providers.
- [x] The Procyon Postman generator discovers plugin routes/examples and matches
      concrete provider examples to `:provider` routes.

## Test and release gate

- [x] Controller tests cover auth, validation, idempotency, status, forwarding,
      and request-size limits.
- [x] Store tests cover ownership, status ordering, pagination, leases, retry,
      stale finalization, and migration behavior.
- [x] Deterministic Stripe, Google, and Apple provider fixtures avoid live APIs.
- [x] Plugin tests cover startup, invalid config, migrations, policies, routes,
      and shutdown.
- [x] CI runs PostgreSQL/MySQL migrations, `go vet`, race tests, and a 25%
      repository coverage floor focused on critical paths.
- [ ] Run live sandbox end-to-end scenarios for every enabled provider before
      publishing stable `v1.0.0` (requires project-owned provider credentials,
      products, mobile apps/license testers, and webhook console configuration).
