# Payment System Configuration

The CLI writes selected providers to `config/plugins.generated.json`. Provider
code is part of one Go module and all provider packages are compiled; selection
controls startup and runtime availability.

Plugin `values` may contain `products_file`, `webhook_lease`,
`webhook_retention`, and `reconcile_every`. Environment variables with the names
below are the fallback. Secrets are accepted only from environment variables.

## Shared

```dotenv
PAYMENT_ALLOWED_RETURN_ORIGINS=https://app.example.com,https://admin.example.com
PAYMENT_PRODUCTS_FILE=./config/payment-products.json
PAYMENT_WEBHOOK_LEASE=5m
PAYMENT_WEBHOOK_RETENTION=720h
PAYMENT_RECONCILE_EVERY=6h
```

The product file is a JSON array matching `docs/products.example.json`. Every
selected provider needs at least one enabled product. `kind` is `one_time` or
`subscription`; `plan_code` is the stable application entitlement name.
`currency`, positive `amount_minor`, and a subscription interval such as
`1-month` are mandatory. One-time products must not define an interval.

## Stripe

```dotenv
STRIPE_SECRET_KEY=
STRIPE_WEBHOOK_SECRET=
STRIPE_TRIAL_DAYS=0
```

Webhook URL: `POST /v1/payments/webhooks/stripe`. Configure at least these
events in Stripe Workbench:

- `payment_intent.created`, `payment_intent.succeeded`,
  `payment_intent.payment_failed`, `payment_intent.canceled`;
- `checkout.session.completed`, `checkout.session.async_payment_succeeded`,
  `checkout.session.async_payment_failed`;
- `invoice.paid`, `invoice.payment_failed`;
- `customer.subscription.updated`, `customer.subscription.deleted`;
- `refund.created`, `refund.updated`, `refund.failed`.
- `charge.dispute.created`, `charge.dispute.updated`, `charge.dispute.closed`,
  `charge.dispute.funds_withdrawn`, `charge.dispute.funds_reinstated`.

## Google Play

```dotenv
GOOGLE_PLAY_SERVICE_ACCOUNT_FILE=./config/google-play-service-account.json
GOOGLE_PLAY_PACKAGE_NAME=com.example.app
GOOGLE_PLAY_PUBSUB_AUDIENCE=https://api.example.com/v1/payments/webhooks/google
PAYMENT_DATA_ENCRYPTION_KEY=<base64-encoded-32-byte-key>
```

Generate the encryption key once and keep it in a secret manager:

```bash
openssl rand -base64 32
```

Configure Pub/Sub authenticated push to
`POST /v1/payments/webhooks/google`, with the same audience. The Android client
must set `obfuscatedAccountId` to lowercase hex SHA-256 of:

```text
procyon-payment:<authenticated Procyon identity ID>
```

The backend ignores client package names and always queries the configured
package with `purchases.subscriptionsv2.get`.

## Apple App Store

```dotenv
APPLE_APP_STORE_CONFIG_FILE=./config/apple-app-store.json
```

Example production configuration:

```json
{
  "bundle_id": "com.example.app",
  "app_apple_id": 1234567890,
  "environment": "Production",
  "root_ca": "./AppleRootCA-G3.cer.pem",
  "online_checks": true,
  "issuer_id": "57246542-96fe-1a63-e053-0824d011072a",
  "key_id": "2X9R4HXF34",
  "private_key": "./SubscriptionKey_2X9R4HXF34.p8"
}
```

Use `Sandbox` and omit `app_apple_id` only for sandbox. The issuer, key ID, and
ES256 In-App Purchase private key authorize App Store Server API reconciliation;
the private key must remain outside source control. Webhook URL: `POST
/v1/payments/webhooks/apple`. StoreKit 2 purchases must use the authenticated
Procyon identity UUID as `appAccountToken`.

## Startup behavior

Startup fails when selected provider configuration is missing or invalid. This
is intentional: a deployment must not report itself healthy while silently
dropping payment lifecycle events.
