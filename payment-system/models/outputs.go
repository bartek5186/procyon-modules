package models

import "time"

type PaymentPriceOption struct {
	ID          string `json:"id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	Interval    string `json:"interval,omitempty"`
	ProductID   string `json:"product_id,omitempty"`
	ProductName string `json:"product_name,omitempty"`
}

type PaymentSubscriptionResponse struct {
	Provider           string             `json:"provider"`
	SubscriptionID     string             `json:"subscription_id"`
	PlanCode           string             `json:"plan_code"`
	Status             SubscriptionStatus `json:"status"`
	CurrentPeriodStart time.Time          `json:"current_period_start"`
	CurrentPeriodEnd   time.Time          `json:"current_period_end"`
	CancelAtPeriodEnd  bool               `json:"cancel_at_period_end"`
}

type PaymentEventResponse struct {
	Provider       string        `json:"provider"`
	ExternalID     string        `json:"external_id"`
	SubscriptionID string        `json:"subscription_id,omitempty"`
	PlanCode       string        `json:"plan_code,omitempty"`
	Status         PaymentStatus `json:"status"`
	Kind           string        `json:"kind"`
	AmountMinor    int64         `json:"amount_minor"`
	Currency       string        `json:"currency"`
	OccurredAt     time.Time     `json:"occurred_at"`
}

type PaymentEntitlementResponse struct {
	Active    bool               `json:"active"`
	Provider  string             `json:"provider,omitempty"`
	PlanCode  string             `json:"plan_code,omitempty"`
	Status    SubscriptionStatus `json:"status,omitempty"`
	ExpiresAt time.Time          `json:"expires_at,omitempty"`
}

type PaymentProviderStatus struct {
	Name             string  `json:"name"`
	Ready            bool    `json:"ready"`
	Requests         uint64  `json:"requests"`
	Failures         uint64  `json:"failures"`
	AverageLatencyMS float64 `json:"average_latency_ms"`
}

type PaymentWebhookResponse struct {
	Provider      string        `json:"provider"`
	EventID       string        `json:"event_id"`
	EventType     string        `json:"event_type"`
	Status        WebhookStatus `json:"status"`
	Attempts      int           `json:"attempts"`
	LastError     string        `json:"last_error,omitempty"`
	LastAttemptAt *time.Time    `json:"last_attempt_at,omitempty"`
	ProcessedAt   *time.Time    `json:"processed_at,omitempty"`
}
