# Payment Operations

## Webhooks

Webhook records move through `PROCESSING`, `SUCCEEDED`, and `FAILED`. A process
interruption is recovered after `PAYMENT_WEBHOOK_LEASE`; concurrent deliveries
cannot claim a live lease. Failed events can be inspected and retried through
the admin API. Successfully processed payloads are cleared after
`PAYMENT_WEBHOOK_RETENTION` while event IDs and audit state remain.

## Reconciliation

Stripe, Google, and Apple subscriptions are reconciled every
`PAYMENT_RECONCILE_EVERY` and on demand through the admin API. Apple uses Get All
Subscription Statuses with a short-lived ES256 server token. Provider request,
failure, and average-latency counters are available from the admin provider
status endpoint.

## Provider sandbox checklist

### Stripe

1. Use a Stripe Sandbox/test key and a catalog containing test Price IDs.
2. Run `stripe listen --forward-to localhost:8080/v1/payments/webhooks/stripe`.
3. Set the printed signing secret as `STRIPE_WEBHOOK_SECRET`.
4. Test one-time, subscription, delayed payment failure/success, cancellation,
   renewal failure, and refund.

### Google Play

1. Configure a license tester and service account access.
2. Configure authenticated Pub/Sub push and send the Play Console test event.
3. Purchase with the required obfuscated account ID.
4. Verify acknowledgement, renewal, cancellation, grace period, revoke, and
   void/refund transitions.

### Apple

1. Use the Sandbox environment and Apple root certificate.
2. Configure an In-App Purchase key for Server API reconciliation.
3. Set StoreKit 2 `appAccountToken` to the authenticated identity UUID.
4. Configure the sandbox notification URL and request a test notification.
5. Test purchase, renewal, billing retry, grace period, expiration, refund, and
   revoke.

Real sandbox runs require credentials and store-console configuration; CI uses
deterministic fixtures and does not contain provider secrets.
