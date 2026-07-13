package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
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

func (s *PaymentSystemStore) ClaimWebhook(ctx context.Context, event models.PaymentWebhookEvent, lease time.Duration) (string, bool, error) {
	now := time.Now().UTC()
	leaseID, err := webhookLeaseID()
	if err != nil {
		return "", false, err
	}
	event.Status = models.WebhookStatusProcessing
	event.Attempts = 1
	event.ProcessingStartedAt = &now
	event.LastAttemptAt = &now
	event.LeaseID = leaseID
	result := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "provider"},
			{Name: "event_id"},
		},
		DoNothing: true,
	}).Create(&event)
	if result.Error != nil || result.RowsAffected == 1 {
		return leaseID, result.RowsAffected == 1, result.Error
	}
	cutoff := now.Add(-lease)
	updates := map[string]any{
		"status": models.WebhookStatusProcessing, "processing_started_at": &now,
		"last_attempt_at": &now, "last_error": "", "payload": event.Payload,
		"signature": event.Signature, "event_type": event.EventType,
		"lease_id": leaseID, "attempts": gorm.Expr("attempts + 1"),
	}
	claimed := s.db.WithContext(ctx).Model(&models.PaymentWebhookEvent{}).
		Where("provider = ? AND event_id = ?", event.Provider, event.EventID).
		Where("status = ? OR (status = ? AND processing_started_at < ?)",
			models.WebhookStatusFailed, models.WebhookStatusProcessing, cutoff).
		Updates(updates)
	if claimed.Error != nil || claimed.RowsAffected == 0 {
		return "", false, claimed.Error
	}
	return leaseID, true, nil
}

func (s *PaymentSystemStore) FinishWebhook(ctx context.Context, provider, eventID, leaseID string, processErr error) error {
	updates := map[string]any{"updated_at": time.Now().UTC(), "processing_started_at": nil}
	if processErr == nil {
		now := time.Now().UTC()
		updates["processed_at"] = &now
		updates["last_error"] = ""
		updates["status"] = models.WebhookStatusSucceeded
	} else {
		message := processErr.Error()
		if len(message) > 4096 {
			message = message[:4096]
		}
		updates["last_error"] = message
		updates["status"] = models.WebhookStatusFailed
	}
	result := s.db.WithContext(ctx).Model(&models.PaymentWebhookEvent{}).
		Where("provider = ? AND event_id = ? AND status = ? AND lease_id = ?", provider, eventID, models.WebhookStatusProcessing, leaseID).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return models.ErrPaymentWebhookLeaseLost
	}
	return nil
}

func webhookLeaseID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func (s *PaymentSystemStore) GetWebhook(ctx context.Context, provider, eventID string) (models.PaymentWebhookEvent, error) {
	var event models.PaymentWebhookEvent
	err := s.db.WithContext(ctx).Where("provider = ? AND event_id = ?", provider, eventID).First(&event).Error
	return event, err
}

func (s *PaymentSystemStore) MarkWebhookForRetry(ctx context.Context, provider, eventID string) error {
	result := s.db.WithContext(ctx).Model(&models.PaymentWebhookEvent{}).
		Where("provider = ? AND event_id = ? AND status = ?", provider, eventID, models.WebhookStatusFailed).
		Updates(map[string]any{"processing_started_at": nil, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func (s *PaymentSystemStore) ListFailedWebhooks(ctx context.Context, limit, offset int) ([]models.PaymentWebhookEvent, error) {
	var events []models.PaymentWebhookEvent
	err := s.db.WithContext(ctx).Where("status = ?", models.WebhookStatusFailed).
		Order("updated_at DESC").Limit(limit).Offset(offset).Find(&events).Error
	return events, err
}

func (s *PaymentSystemStore) CleanupWebhookPayloads(ctx context.Context, before time.Time) error {
	return s.db.WithContext(ctx).Model(&models.PaymentWebhookEvent{}).
		Where("status = ? AND processed_at < ?", models.WebhookStatusSucceeded, before).
		Updates(map[string]any{"payload": nil, "signature": ""}).Error
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

func (s *PaymentSystemStore) GetPayment(ctx context.Context, provider, externalID string) (models.PaymentEvent, error) {
	var payment models.PaymentEvent
	err := s.db.WithContext(ctx).Where("provider = ? AND external_id = ?", provider, externalID).First(&payment).Error
	return payment, err
}

func paymentStatusRank(status models.PaymentStatus) int {
	switch status {
	case models.PaymentStatusPending:
		return 1
	case models.PaymentStatusSucceeded:
		return 2
	case models.PaymentStatusCanceled, models.PaymentStatusFailed:
		return 3
	case models.PaymentStatusDisputed:
		return 4
	case models.PaymentStatusRefunded:
		return 5
	default:
		return 0
	}
}

func (s *PaymentSystemStore) UpsertSubscription(ctx context.Context, subscription models.PaymentSubscription) error {
	result := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "provider"},
			{Name: "external_subscription_id"},
		},
		DoNothing: true,
	}).Create(&subscription)
	if result.Error != nil || result.RowsAffected == 1 {
		return result.Error
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing models.PaymentSubscription
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("provider = ? AND external_subscription_id = ?", subscription.Provider, subscription.ExternalSubscriptionID).
			First(&existing).Error; err != nil {
			return err
		}
		if existing.IdentityID != "" && subscription.IdentityID != "" && existing.IdentityID != subscription.IdentityID {
			return models.ErrPaymentOwnershipConflict
		}
		updates := map[string]any{
			"customer_id": subscription.CustomerID, "plan_code": subscription.PlanCode,
			"status": subscription.Status, "current_period_start": subscription.CurrentPeriodStart,
			"current_period_end": subscription.CurrentPeriodEnd, "cancel_at": subscription.CancelAt,
			"cancel_at_period_end": subscription.CancelAtPeriodEnd, "updated_at": time.Now().UTC(),
		}
		if existing.IdentityID == "" && subscription.IdentityID != "" {
			updates["identity_id"] = subscription.IdentityID
		}
		if subscription.ProviderData != "" {
			updates["provider_data"] = subscription.ProviderData
		}
		return tx.Model(&models.PaymentSubscription{}).Where("id = ?", existing.ID).Updates(updates).Error
	})
}

func (s *PaymentSystemStore) HasActiveSubscription(ctx context.Context, identityID string) (bool, error) {
	var count int64
	err := s.db.WithContext(ctx).Model(&models.PaymentSubscription{}).
		Where("identity_id = ?", identityID).
		Where("status IN ?", []models.SubscriptionStatus{models.SubscriptionStatusActive, models.SubscriptionStatusTrialing}).
		Where("current_period_end > ?", time.Now().UTC()).Count(&count).Error
	return count > 0, err
}

func (s *PaymentSystemStore) ActiveEntitlement(ctx context.Context, identityID, planCode string) (models.PaymentSubscription, error) {
	query := s.db.WithContext(ctx).Where("identity_id = ?", identityID).
		Where("status IN ?", []models.SubscriptionStatus{models.SubscriptionStatusActive, models.SubscriptionStatusTrialing}).
		Where("current_period_end > ?", time.Now().UTC())
	if strings.TrimSpace(planCode) != "" {
		query = query.Where("plan_code = ?", strings.TrimSpace(planCode))
	}
	var subscription models.PaymentSubscription
	err := query.Order("current_period_end DESC").First(&subscription).Error
	return subscription, err
}

func (s *PaymentSystemStore) ListPayments(ctx context.Context, identityID string, limit, offset int) ([]models.PaymentEvent, error) {
	var events []models.PaymentEvent
	err := s.db.WithContext(ctx).Where("identity_id = ?", identityID).
		Order("occurred_at DESC").Limit(limit).Offset(offset).Find(&events).Error
	return events, err
}

func (s *PaymentSystemStore) ListSubscriptionsAfter(ctx context.Context, afterID uint, limit int) ([]models.PaymentSubscription, error) {
	var subscriptions []models.PaymentSubscription
	err := s.db.WithContext(ctx).Where("id > ?", afterID).Order("id ASC").Limit(limit).Find(&subscriptions).Error
	return subscriptions, err
}

func (s *PaymentSystemStore) ListSubscriptions(ctx context.Context, identityID string, limit, offset int) ([]models.PaymentSubscription, error) {
	var subscriptions []models.PaymentSubscription
	err := s.db.WithContext(ctx).Where("identity_id = ?", identityID).
		Order("current_period_end DESC").Limit(limit).Offset(offset).Find(&subscriptions).Error
	return subscriptions, err
}

func (s *PaymentSystemStore) GetSubscription(ctx context.Context, provider, externalID, identityID string) (models.PaymentSubscription, error) {
	var subscription models.PaymentSubscription
	err := s.db.WithContext(ctx).
		Where("provider = ? AND external_subscription_id = ? AND identity_id = ?", provider, externalID, identityID).
		First(&subscription).Error
	return subscription, err
}

func (s *PaymentSystemStore) GetSubscriptionByExternal(ctx context.Context, provider, externalID string) (models.PaymentSubscription, error) {
	var subscription models.PaymentSubscription
	err := s.db.WithContext(ctx).Where("provider = ? AND external_subscription_id = ?", provider, externalID).
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
