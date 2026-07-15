# Stripe Web Integration

This document defines how a web application should integrate with the
`payment-system` module when Stripe is the payment provider. It is an
implementation specification rather than a frontend framework tutorial.

## Integration model

The web application owns the product presentation, purchase buttons, loading
states, success and cancellation pages. The payment module owns the allowed
catalog, Stripe credentials, Checkout Session creation, webhook verification,
payment persistence, subscriptions and entitlements.

The current integration uses Stripe-hosted Checkout. Product cards are embedded
in the application, but the payment form is not. After a user selects a product,
the backend creates a Checkout Session and the browser redirects to the URL
returned by the backend.

Do not call authenticated Stripe APIs or expose Stripe secret keys in the web
application. Do not treat a browser redirect as proof that a payment succeeded.

## Backend preparation

1. Enable the `payment-system` module with the Stripe provider.
2. Run the application migrations.
3. Create the products and prices in Stripe.
4. Add every sellable Stripe price to the server-owned product catalog.
5. Configure the Stripe secret key and webhook signing secret.
6. Add every permitted web application origin to
   `PAYMENT_ALLOWED_RETURN_ORIGINS`, including its scheme and port when used.
7. Register the Stripe webhook endpoint at
   `POST /v1/payments/webhooks/stripe` and subscribe it to the events listed in
   [CONFIGURATION.md](CONFIGURATION.md).
8. Confirm that the application starts successfully and that provider readiness
   is reported by `GET /v1/admin/payments/providers`.

For Stripe catalog entries, the `product_id` field currently contains a Stripe
Price ID beginning with `price_`, not a Stripe Product ID beginning with
`prod_`. A Product describes what is sold; a Price describes the amount,
currency and billing frequency used by Checkout.

The catalog is the application's allowlist. A price that exists in Stripe but
is absent or inconsistent in the local catalog must not be offered or accepted.

## Loading the available products

The web application obtains the sellable Stripe products from
`GET /v1/payments/prices/stripe`. It must use this endpoint instead of querying
Stripe directly.

The response provides the Stripe Price ID, amount in the currency's minor unit,
currency, product name, product description, payment kind, billing interval and
Stripe Product ID. Only active prices accepted by the server-owned catalog are
returned.

The frontend should:

- use the Price ID supplied by the API when starting Checkout;
- format the amount according to the returned currency and locale;
- distinguish a one-time purchase from a recurring subscription;
- show the billing interval for subscriptions;
- handle an empty catalog and a catalog loading failure explicitly;
- avoid using a display name as a business identifier.

Marketing content such as images, feature lists, badges and longer sales copy
is not currently returned by the payment API. Keep that content in the
application's presentation layer and associate it with the stable Stripe Product
ID. If the module later exposes `plan_code` in the public catalog response,
prefer it as the application-level identifier.

## Starting a purchase

Starting Checkout is an authenticated operation. The current Procyon identity
must have the `payment_system:use` permission.

Use `POST /v1/payments/checkout` for a one-time product and
`POST /v1/payments/subscriptions/checkout` for a recurring product. The request
identifies the Stripe provider, selected Price ID, success URL and cancellation
URL.

Every Checkout request requires an `Idempotency-Key`. Generate one key for one
logical purchase attempt and reuse that same key if an uncertain network result
is retried. A new user-initiated purchase attempt receives a new key.

The success and cancellation URLs must belong to an origin configured in
`PAYMENT_ALLOWED_RETURN_ORIGINS`. For a separate web and API origin, the
frontend must use the application's existing authenticated API client. A
cookie-based session requires credentialed requests and matching CORS settings
on the API.

After the backend returns the Checkout URL, redirect the browser to it. Do not
construct Stripe Checkout URLs in the frontend and do not accept an arbitrary
Price ID that was not loaded from the module catalog.

## Completing the purchase

Stripe sends the authoritative payment lifecycle result to the module webhook.
The webhook verifies the signature, persists the provider event and updates the
payment, subscription and entitlement state.

The success page should initially present a pending confirmation state because
the browser redirect and webhook can arrive in either order. It should obtain
the confirmed state from the backend for a short, bounded period:

- for a subscription, query
  `GET /v1/payments/entitlement?plan_code=<plan_code>`;
- for payment history, query `GET /v1/payments/history`;
- for subscription details, query `GET /v1/payments/subscriptions`.

The cancellation page means that Checkout was abandoned. It must not cancel an
already active subscription or reverse a payment on its own.

## Subscription management

Use the module rather than direct Stripe browser calls for subscription state:

- `GET /v1/payments/entitlement` controls access to paid application features;
- `GET /v1/payments/subscriptions` displays the current subscription state;
- `POST /v1/payments/subscriptions/cancel` requests immediate or period-end
  cancellation;
- `POST /v1/payments/portal` creates a Stripe billing portal session for payment
  method, invoice and subscription management.

Authorization decisions must be based on the backend entitlement response, not
on local storage, the Stripe redirect URL or a frontend-only Premium flag.

The module adds the catalog Price ID and `plan_code` to new Stripe Checkout and
subscription metadata. Subscription lifecycle processing uses this value, with
the server catalog as a fallback, so entitlements remain available under the
application's stable plan name.

## One-time purchase fulfillment

Recording a successful one-time payment is not the same as delivering its
application-specific benefit. A product such as a points top-up requires a
backend fulfillment operation that grants the purchased value to the identity.

Fulfillment must:

- run only after an authoritative successful payment event;
- use a stable provider payment or Checkout identifier as its idempotency key;
- be safe when the webhook is delivered more than once;
- update the business state and record that the benefit was granted atomically,
  or with an equivalent recoverable workflow;
- never be triggered only by visiting the success page.

After recording a confirmed one-time Checkout, payment-system publishes the
typed `payment.purchase.completed.v1` event. The consuming application or
another plugin registers a handler that translates `plan_code` into the domain
benefit. For example, the application maps `points_1000` to a 1000-point credit.

The event uses a stable message ID. The handler must store that ID in a
fulfillment ledger and apply the domain change in the same database transaction.
A repeated webhook then returns success without granting the benefit twice. A
handler error fails webhook processing, allowing Stripe delivery or an admin
retry to run it again.

See [EVENTS.md](EVENTS.md) for the contract and registration lifecycle.

## Current compatibility notes

- A recurring Stripe price can currently be exposed with the kind `recurring`
  even though the module catalog calls the same business type `subscription`.
  A web integration must recognize both until the module normalizes the public
  response.
- The public price response does not currently expose the catalog `plan_code`.
  The presentation layer can use the Stripe Product ID, while entitlement checks
  continue to use the known application plan code.
- Changing a Stripe amount normally means creating a new Stripe Price and
  updating the server catalog. The frontend must not hard-code a Price ID as the
  only source of product availability.

These compatibility points should be normalized in the module before relying
on a single frontend representation across multiple applications.

## Error handling and user states

The web implementation must provide distinct states for catalog loading,
catalog unavailable, checkout creation, redirecting, payment confirmation,
confirmed success, cancellation and recoverable failure.

Client-visible module errors must be translated into useful messages without
exposing provider internals. In particular, handle disabled providers, rejected
products, rejected return URLs, invalid idempotency keys and an already active
subscription. Preserve enough diagnostic context in server logs to trace the
request and provider event.

## Production acceptance checklist

- The browser never receives a Stripe secret or webhook signing secret.
- The product list comes from `GET /v1/payments/prices/stripe`.
- Only catalog-approved active prices are displayed and accepted.
- One-time products and subscriptions use their respective Checkout endpoints.
- Every logical Checkout attempt has a correctly reused idempotency key.
- Return origins and credentialed CORS are configured for every deployed web
  target.
- Stripe-hosted Checkout opens from the backend-generated URL.
- The Stripe webhook is registered, signed events are accepted, and failed
  events are observable and retryable.
- Premium access is based on the entitlement endpoint.
- One-time benefits are fulfilled server-side and idempotently.
- Success and cancellation pages do not make authorization decisions from the
  redirect alone.
- Stripe test mode covers successful, cancelled, failed, asynchronous and
  repeated-webhook scenarios before live credentials are enabled.

See [API.md](API.md) for the route contract, [CONFIGURATION.md](CONFIGURATION.md)
for provider settings and [OPERATIONS.md](OPERATIONS.md) for production
operations and recovery.
