# Payment System

Reusable payments and subscriptions for Procyon applications.

This directory is a standalone Go module. Its models, persistence, provider
implementations, controllers and migrations remain versioned here; installing
it does not copy them into the application repository.

Supported provider capabilities:

- Stripe: prices, one-time checkout, subscription checkout, billing portal,
  cancellation and signed webhooks;
- Google Play: authenticated subscription purchase verification;
- Apple App Store: signed App Store Server Notification V2 webhooks.

Install one or more providers:

```bash
procyon-cli module add payment-system --provider stripe
procyon-cli module add payment-system --provider stripe,google,apple
```

Secrets are read from environment variables and are never generated into JSON
configuration files. See `docs/CONFIGURATION.md`.
