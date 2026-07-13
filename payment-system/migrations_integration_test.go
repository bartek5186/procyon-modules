package paymentsystem

import (
	"os"
	"testing"

	"github.com/bartek5186/procyon-modules/payment-system/models"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestPaymentMigrationsOnConfiguredDatabases(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		open func(string) gorm.Dialector
	}{
		{name: "postgres", dsn: os.Getenv("PAYMENT_TEST_POSTGRES_DSN"), open: func(dsn string) gorm.Dialector { return postgres.Open(dsn) }},
		{name: "mysql", dsn: os.Getenv("PAYMENT_TEST_MYSQL_DSN"), open: func(dsn string) gorm.Dialector { return mysql.Open(dsn) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.dsn == "" {
				t.Skip("database DSN not configured")
			}
			db, err := gorm.Open(test.open(test.dsn), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if err := runPaymentMigrations(db); err != nil {
				t.Fatal(err)
			}
			if err := runPaymentMigrations(db); err != nil {
				t.Fatal(err)
			}
			if !db.Migrator().HasTable(&models.PaymentWebhookEvent{}) {
				t.Fatal("webhook table missing")
			}
		})
	}
}
