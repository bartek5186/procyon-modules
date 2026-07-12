package models

import "time"

type PaymentStatus string

const (
	PaymentStatusPending   PaymentStatus = "PENDING"
	PaymentStatusSucceeded PaymentStatus = "SUCCEEDED"
	PaymentStatusFailed    PaymentStatus = "FAILED"
	PaymentStatusCanceled  PaymentStatus = "CANCELED"
	PaymentStatusRefunded  PaymentStatus = "REFUNDED"
)

type SubscriptionStatus string

const (
	SubscriptionStatusActive   SubscriptionStatus = "ACTIVE"
	SubscriptionStatusTrialing SubscriptionStatus = "TRIALING"
	SubscriptionStatusPastDue  SubscriptionStatus = "PAST_DUE"
	SubscriptionStatusCanceled SubscriptionStatus = "CANCELED"
	SubscriptionStatusExpired  SubscriptionStatus = "EXPIRED"
	SubscriptionStatusRefunded SubscriptionStatus = "REFUNDED"
	SubscriptionStatusRevoked  SubscriptionStatus = "REVOKED"
)

type PaymentEvent struct {
	ID             uint          `gorm:"primaryKey"`
	Provider       string        `gorm:"size:32;not null;uniqueIndex:idx_payment_provider_external,priority:1;index"`
	ExternalID     string        `gorm:"size:191;not null;uniqueIndex:idx_payment_provider_external,priority:2"`
	IdentityID     string        `gorm:"size:191;index"`
	CustomerID     string        `gorm:"size:191;index"`
	SubscriptionID string        `gorm:"size:191;index"`
	PriceID        string        `gorm:"size:191;index"`
	Status         PaymentStatus `gorm:"size:32;not null;index"`
	Kind           string        `gorm:"size:32;not null;index"`
	AmountMinor    int64         `gorm:"not null;default:0"`
	Currency       string        `gorm:"size:8"`
	OccurredAt     time.Time     `gorm:"not null;index"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (PaymentEvent) TableName() string { return "payment_events" }

type PaymentWebhookEvent struct {
	ID          uint       `gorm:"primaryKey"`
	Provider    string     `gorm:"size:32;not null;uniqueIndex:idx_webhook_provider_event,priority:1"`
	EventID     string     `gorm:"size:191;not null;uniqueIndex:idx_webhook_provider_event,priority:2"`
	EventType   string     `gorm:"size:100;not null;index"`
	ProcessedAt *time.Time `gorm:"index"`
	LastError   string     `gorm:"type:text"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (PaymentWebhookEvent) TableName() string { return "payment_webhook_events" }

type PaymentSubscription struct {
	ID                     uint               `gorm:"primaryKey"`
	Provider               string             `gorm:"size:32;not null;uniqueIndex:idx_subscription_provider_external,priority:1;index"`
	ExternalSubscriptionID string             `gorm:"size:191;not null;uniqueIndex:idx_subscription_provider_external,priority:2"`
	IdentityID             string             `gorm:"size:191;not null;index"`
	CustomerID             string             `gorm:"size:191;index"`
	PlanCode               string             `gorm:"size:191;index"`
	Status                 SubscriptionStatus `gorm:"size:32;not null;index"`
	CurrentPeriodStart     time.Time          `gorm:"index"`
	CurrentPeriodEnd       time.Time          `gorm:"index"`
	CancelAt               *time.Time         `gorm:"index"`
	CancelAtPeriodEnd      bool               `gorm:"not null;default:false"`
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

func (PaymentSubscription) TableName() string { return "payment_subscriptions" }
