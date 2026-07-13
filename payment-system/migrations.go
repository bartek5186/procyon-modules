package paymentsystem

import (
	"errors"
	"time"

	"github.com/bartek5186/procyon-modules/payment-system/models"
	"gorm.io/gorm"
)

type paymentMigration struct {
	version int
	up      func(*gorm.DB) error
}

var paymentMigrations = []paymentMigration{
	{version: 1, up: func(tx *gorm.DB) error {
		if err := tx.AutoMigrate(&models.PaymentEvent{}, &models.PaymentWebhookEvent{}, &models.PaymentSubscription{}); err != nil {
			return err
		}
		if err := tx.Model(&models.PaymentWebhookEvent{}).Where("processed_at IS NOT NULL").Update("status", models.WebhookStatusSucceeded).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.PaymentWebhookEvent{}).Where("processed_at IS NULL AND last_error <> ''").Update("status", models.WebhookStatusFailed).Error; err != nil {
			return err
		}
		return tx.Model(&models.PaymentWebhookEvent{}).
			Where("processed_at IS NULL AND (last_error = '' OR last_error IS NULL) AND processing_started_at IS NULL").
			Update("processing_started_at", gorm.Expr("COALESCE(updated_at, created_at)")).Error
	}},
}

func runPaymentMigrations(db *gorm.DB) error {
	if err := db.AutoMigrate(&models.PaymentModuleMigration{}); err != nil {
		return err
	}
	for _, migration := range paymentMigrations {
		err := db.Where("version = ?", migration.version).First(&models.PaymentModuleMigration{}).Error
		if err == nil {
			continue
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := db.Transaction(func(tx *gorm.DB) error {
			if err := migration.up(tx); err != nil {
				return err
			}
			return tx.Create(&models.PaymentModuleMigration{Version: migration.version, AppliedAt: time.Now().UTC()}).Error
		}); err != nil {
			return err
		}
	}
	return nil
}
