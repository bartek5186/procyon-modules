package paymentsystem

import (
	"testing"

	"github.com/bartek5186/procyon-modules/payment-system/models"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestPaymentMigrationsAreVersionedAndIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := runPaymentMigrations(db); err != nil {
		t.Fatal(err)
	}
	if err := runPaymentMigrations(db); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasTable(&models.PaymentSubscription{}) || !db.Migrator().HasTable(&models.PaymentWebhookEvent{}) {
		t.Fatal("payment tables missing")
	}
	var count int64
	if err := db.Model(&models.PaymentModuleMigration{}).Where("version = ?", 1).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("migration version count = %d, %v", count, err)
	}
}
