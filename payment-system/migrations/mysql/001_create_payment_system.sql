-- +goose Up
CREATE TABLE IF NOT EXISTS payment_events (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
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
  occurred_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NULL,
  updated_at DATETIME(3) NULL,
  UNIQUE KEY uq_payment_provider_external (provider, external_id),
  KEY idx_payment_events_identity (identity_id, occurred_at),
  KEY idx_payment_events_subscription (subscription_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS payment_webhook_events (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  provider VARCHAR(32) NOT NULL,
  event_id VARCHAR(191) NOT NULL,
  event_type VARCHAR(100) NOT NULL,
  processed_at DATETIME(3) NULL,
  last_error TEXT NULL,
  created_at DATETIME(3) NULL,
  updated_at DATETIME(3) NULL,
  UNIQUE KEY uq_payment_webhook_provider_event (provider, event_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS payment_subscriptions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  provider VARCHAR(32) NOT NULL,
  external_subscription_id VARCHAR(191) NOT NULL,
  identity_id VARCHAR(191) NOT NULL,
  customer_id VARCHAR(191) NOT NULL DEFAULT '',
  plan_code VARCHAR(191) NOT NULL DEFAULT '',
  status VARCHAR(32) NOT NULL,
  current_period_start DATETIME(3) NULL,
  current_period_end DATETIME(3) NULL,
  cancel_at DATETIME(3) NULL,
  cancel_at_period_end BOOLEAN NOT NULL DEFAULT FALSE,
  created_at DATETIME(3) NULL,
  updated_at DATETIME(3) NULL,
  UNIQUE KEY uq_payment_subscription_provider_external (provider, external_subscription_id),
  KEY idx_payment_subscriptions_identity (identity_id, status, current_period_end)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +goose Down
DROP TABLE IF EXISTS payment_subscriptions;
DROP TABLE IF EXISTS payment_webhook_events;
DROP TABLE IF EXISTS payment_events;
