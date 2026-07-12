-- +goose Up
CREATE TABLE IF NOT EXISTS payment_events (
  id BIGSERIAL PRIMARY KEY,
  provider VARCHAR(32) NOT NULL,
  external_id VARCHAR(191) NOT NULL,
  identity_id VARCHAR(191) NOT NULL DEFAULT '',
  customer_id VARCHAR(191) NOT NULL DEFAULT '',
  subscription_id VARCHAR(191) NOT NULL DEFAULT '',
  price_id VARCHAR(191) NOT NULL DEFAULT '',
  status VARCHAR(32) NOT NULL,
  kind VARCHAR(32) NOT NULL,
  amount_minor BIGINT NOT NULL DEFAULT 0,
  currency VARCHAR(8) NOT NULL DEFAULT '',
  occurred_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ,
  updated_at TIMESTAMPTZ,
  CONSTRAINT uq_payment_provider_external UNIQUE (provider, external_id)
);
CREATE INDEX IF NOT EXISTS idx_payment_events_identity ON payment_events (identity_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_payment_events_subscription ON payment_events (subscription_id);

CREATE TABLE IF NOT EXISTS payment_webhook_events (
  id BIGSERIAL PRIMARY KEY,
  provider VARCHAR(32) NOT NULL,
  event_id VARCHAR(191) NOT NULL,
  event_type VARCHAR(100) NOT NULL,
  processed_at TIMESTAMPTZ,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ,
  updated_at TIMESTAMPTZ,
  CONSTRAINT uq_payment_webhook_provider_event UNIQUE (provider, event_id)
);

CREATE TABLE IF NOT EXISTS payment_subscriptions (
  id BIGSERIAL PRIMARY KEY,
  provider VARCHAR(32) NOT NULL,
  external_subscription_id VARCHAR(191) NOT NULL,
  identity_id VARCHAR(191) NOT NULL,
  customer_id VARCHAR(191) NOT NULL DEFAULT '',
  plan_code VARCHAR(191) NOT NULL DEFAULT '',
  status VARCHAR(32) NOT NULL,
  current_period_start TIMESTAMPTZ,
  current_period_end TIMESTAMPTZ,
  cancel_at TIMESTAMPTZ,
  cancel_at_period_end BOOLEAN NOT NULL DEFAULT FALSE,
  created_at TIMESTAMPTZ,
  updated_at TIMESTAMPTZ,
  CONSTRAINT uq_payment_subscription_provider_external UNIQUE (provider, external_subscription_id)
);
CREATE INDEX IF NOT EXISTS idx_payment_subscriptions_identity ON payment_subscriptions (identity_id, status, current_period_end DESC);

-- +goose Down
DROP TABLE IF EXISTS payment_subscriptions;
DROP TABLE IF EXISTS payment_webhook_events;
DROP TABLE IF EXISTS payment_events;
