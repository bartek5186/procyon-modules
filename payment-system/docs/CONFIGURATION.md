# Payment System

The providers selected by `procyon-cli module add --provider` are compiled into
the application and enabled in the generated plugin registration. Provider
credentials still determine whether a selected provider can start.

## Shared configuration

```dotenv
PAYMENT_ALLOWED_RETURN_ORIGINS=https://app.example.com,https://admin.example.com
```

Return URLs supplied by clients must match one of these origins. The development
default permits only `http://localhost` and `http://127.0.0.1`.

## Stripe

```dotenv
STRIPE_SECRET_KEY=
STRIPE_WEBHOOK_SECRET=
STRIPE_TRIAL_DAYS=0
```

Webhook URL: `POST /v1/payments/webhooks/stripe`.

## Google Play

```dotenv
GOOGLE_PLAY_SERVICE_ACCOUNT_FILE=./config/google-play-service-account.json
GOOGLE_PLAY_PACKAGE_NAME=com.example.app
```

The service account file must not be committed. Purchase verification uses
`POST /v1/payments/verify/google`.

## Apple App Store

```dotenv
APPLE_APP_STORE_CONFIG_FILE=./config/apple-app-store.json
```

Example config (keep it outside source control):

```json
{
  "bundle_id": "com.example.app",
  "root_ca": "./AppleRootCA-G3.cer.pem"
}
```

Webhook URL: `POST /v1/payments/webhooks/apple`. Both the outer notification
and nested transaction JWS are verified against the configured Apple root CA.

## Security notes

- never commit provider credentials;
- configure Stripe webhook signing before enabling Stripe;
- configure an explicit production return-origin allowlist;
- use provider sandbox environments before processing production payments;
- the module stores amounts as integer minor units and deduplicates webhook
  event IDs per provider.
