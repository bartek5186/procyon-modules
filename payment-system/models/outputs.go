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
