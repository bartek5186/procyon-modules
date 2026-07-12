package store

import (
	"context"
	"errors"
	"time"

	"github.com/bartek5186/procyon-modules/payment-system/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type PaymentSystemStore struct {
	db *gorm.DB
}

func NewPaymentSystemStore(db *gorm.DB) *PaymentSystemStore {
	return &PaymentSystemStore{db: db}
}

func (s *PaymentSystemStore) BeginWebhook(ctx context.Context, provider, eventID, eventType string) (bool, error) {
	event := models.PaymentWebhookEvent{Provider: provider, EventID: eventID, EventType: eventType}
	result := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "provider"},
			{Name: "event_id"},
		},
		DoNothing: true,
	}).Create(&event)
	if result.Error != nil || result.RowsAffected == 1 {
		return result.RowsAffected == 1, result.Error
	}
	var existing models.PaymentWebhookEvent
	if err := s.db.WithContext(ctx).
		Where("provider = ? AND event_id = ?", provider, eventID).
		First(&existing).Error; err != nil {
		return false, err
	}
	return existing.ProcessedAt == nil && existing.LastError != "", nil
}

func (s *PaymentSystemStore) FinishWebhook(ctx context.Context, provider, eventID string, processErr error) error {
	updates := map[string]any{"updated_at": time.Now().UTC()}
	if processErr == nil {
		now := time.Now().UTC()
		updates["processed_at"] = &now
		updates["last_error"] = ""
	} else {
		updates["last_error"] = processErr.Error()
	}
	return s.db.WithContext(ctx).Model(&models.PaymentWebhookEvent{}).
		Where("provider = ? AND event_id = ?", provider, eventID).
		Updates(updates).Error
}

func (s *PaymentSystemStore) UpsertPayment(ctx context.Context, payment models.PaymentEvent) error {
	if payment.OccurredAt.IsZero() {
		payment.OccurredAt = time.Now().UTC()
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing models.PaymentEvent
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("provider = ? AND external_id = ?", payment.Provider, payment.ExternalID).
			First(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return tx.Create(&payment).Error
		}
		if err != nil {
			return err
		}
		updates := map[string]any{"updated_at": time.Now().UTC()}
		for key, value := range map[string]string{
			"identity_id": payment.IdentityID, "customer_id": payment.CustomerID,
			"subscription_id": payment.SubscriptionID, "price_id": payment.PriceID,
			"kind": payment.Kind, "currency": payment.Currency,
		} {
			if value != "" {
				updates[key] = value
			}
		}
		if payment.AmountMinor != 0 {
			updates["amount_minor"] = payment.AmountMinor
		}
		if payment.OccurredAt.After(existing.OccurredAt) {
			updates["occurred_at"] = payment.OccurredAt
		}
		if paymentStatusRank(payment.Status) >= paymentStatusRank(existing.Status) {
			updates["status"] = payment.Status
		}
		return tx.Model(&models.PaymentEvent{}).Where("id = ?", existing.ID).Updates(updates).Error
	})
}

func paymentStatusRank(status models.PaymentStatus) int {
	switch status {
	case models.PaymentStatusPending:
		return 1
	case models.PaymentStatusSucceeded:
		return 2
	case models.PaymentStatusCanceled, models.PaymentStatusFailed:
		return 3
	case models.PaymentStatusRefunded:
		return 4
	default:
		return 0
	}
}

func (s *PaymentSystemStore) UpsertSubscription(ctx context.Context, subscription models.PaymentSubscription) error {
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "provider"},
			{Name: "external_subscription_id"},
		},
		DoUpdates: clause.AssignmentColumns([]string{
			"identity_id", "customer_id", "plan_code", "status", "current_period_start",
			"current_period_end", "cancel_at", "cancel_at_period_end", "updated_at",
		}),
	}).Create(&subscription).Error
}

func (s *PaymentSystemStore) HasActiveSubscription(ctx context.Context, identityID string) (bool, error) {
	var count int64
	err := s.db.WithContext(ctx).Model(&models.PaymentSubscription{}).
		Where("identity_id = ?", identityID).
		Where("status IN ?", []models.SubscriptionStatus{models.SubscriptionStatusActive, models.SubscriptionStatusTrialing}).
		Where("current_period_end > ?", time.Now().UTC()).Count(&count).Error
	return count > 0, err
}

func (s *PaymentSystemStore) ListSubscriptions(ctx context.Context, identityID string) ([]models.PaymentSubscription, error) {
	var subscriptions []models.PaymentSubscription
	err := s.db.WithContext(ctx).Where("identity_id = ?", identityID).
		Order("current_period_end DESC").Find(&subscriptions).Error
	return subscriptions, err
}

func (s *PaymentSystemStore) GetSubscription(ctx context.Context, provider, externalID, identityID string) (models.PaymentSubscription, error) {
	var subscription models.PaymentSubscription
	err := s.db.WithContext(ctx).
		Where("provider = ? AND external_subscription_id = ? AND identity_id = ?", provider, externalID, identityID).
		First(&subscription).Error
	return subscription, err
}

func (s *PaymentSystemStore) CustomerID(ctx context.Context, provider, identityID string) (string, error) {
	var subscription models.PaymentSubscription
	err := s.db.WithContext(ctx).Select("customer_id").
		Where("provider = ? AND identity_id = ? AND customer_id <> ''", provider, identityID).
		Order("updated_at DESC").First(&subscription).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	return subscription.CustomerID, err
}
