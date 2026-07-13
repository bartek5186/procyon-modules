package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bartek5186/procyon-modules/payment-system/models"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func paymentTestStore(t *testing.T) (*PaymentSystemStore, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.PaymentEvent{}, &models.PaymentWebhookEvent{}, &models.PaymentSubscription{}); err != nil {
		t.Fatal(err)
	}
	return NewPaymentSystemStore(db), db
}

func TestSubscriptionOwnerCannotBeReassigned(t *testing.T) {
	repo, _ := paymentTestStore(t)
	ctx := context.Background()
	end := time.Now().UTC().Add(time.Hour)
	first := models.PaymentSubscription{Provider: "google", ExternalSubscriptionID: "token", IdentityID: "user-1",
		PlanCode: "premium", Status: models.SubscriptionStatusActive, CurrentPeriodEnd: end}
	if err := repo.UpsertSubscription(ctx, first); err != nil {
		t.Fatal(err)
	}
	first.IdentityID = "user-2"
	if err := repo.UpsertSubscription(ctx, first); !errors.Is(err, models.ErrPaymentOwnershipConflict) {
		t.Fatalf("expected ownership conflict, got %v", err)
	}
	stored, err := repo.GetSubscription(ctx, "google", "token", "user-1")
	if err != nil || stored.IdentityID != "user-1" {
		t.Fatalf("owner changed: %+v, %v", stored, err)
	}
}

func TestWebhookLeaseCanRecoverInterruptedProcessing(t *testing.T) {
	repo, db := paymentTestStore(t)
	ctx := context.Background()
	lease := time.Minute
	event := models.PaymentWebhookEvent{Provider: "stripe", EventID: "evt-1", EventType: "test", Payload: []byte("first")}
	leaseID, claimed, err := repo.ClaimWebhook(ctx, event, lease)
	if err != nil || !claimed {
		t.Fatalf("first claim = %v, %v", claimed, err)
	}
	_, claimed, err = repo.ClaimWebhook(ctx, event, lease)
	if err != nil || claimed {
		t.Fatalf("duplicate claim = %v, %v", claimed, err)
	}
	old := time.Now().UTC().Add(-2 * lease)
	if err := db.Model(&models.PaymentWebhookEvent{}).Where("provider = ? AND event_id = ?", "stripe", "evt-1").
		Update("processing_started_at", old).Error; err != nil {
		t.Fatal(err)
	}
	newLeaseID, claimed, err := repo.ClaimWebhook(ctx, event, lease)
	if err != nil || !claimed {
		t.Fatalf("stale claim = %v, %v", claimed, err)
	}
	if err := repo.FinishWebhook(ctx, "stripe", "evt-1", leaseID, nil); !errors.Is(err, models.ErrPaymentWebhookLeaseLost) {
		t.Fatalf("expected stale lease rejection, got %v", err)
	}
	if err := repo.FinishWebhook(ctx, "stripe", "evt-1", newLeaseID, nil); err != nil {
		t.Fatal(err)
	}
	_, claimed, err = repo.ClaimWebhook(ctx, event, lease)
	if err != nil || claimed {
		t.Fatalf("processed claim = %v, %v", claimed, err)
	}
}

func TestFailedWebhookCanBeRetried(t *testing.T) {
	repo, _ := paymentTestStore(t)
	ctx := context.Background()
	event := models.PaymentWebhookEvent{Provider: "apple", EventID: "evt-2", EventType: "test"}
	leaseID, claimed, err := repo.ClaimWebhook(ctx, event, time.Minute)
	if err != nil || !claimed {
		t.Fatal(err)
	}
	if err := repo.FinishWebhook(ctx, "apple", "evt-2", leaseID, errors.New("temporary")); err != nil {
		t.Fatal(err)
	}
	_, claimed, err = repo.ClaimWebhook(ctx, event, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("failed retry = %v, %v", claimed, err)
	}
	stored, err := repo.GetWebhook(ctx, "apple", "evt-2")
	if err != nil || stored.Attempts != 2 {
		t.Fatalf("attempts = %d, err %v", stored.Attempts, err)
	}
}

func TestPaymentStatusCannotRegress(t *testing.T) {
	repo, db := paymentTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	base := models.PaymentEvent{Provider: "stripe", ExternalID: "pi-1", IdentityID: "user-1", Status: models.PaymentStatusSucceeded,
		Kind: "one_time", OccurredAt: now}
	if err := repo.UpsertPayment(ctx, base); err != nil {
		t.Fatal(err)
	}
	base.Status = models.PaymentStatusPending
	if err := repo.UpsertPayment(ctx, base); err != nil {
		t.Fatal(err)
	}
	var stored models.PaymentEvent
	if err := db.Where("provider = ? AND external_id = ?", "stripe", "pi-1").First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != models.PaymentStatusSucceeded {
		t.Fatalf("payment status regressed to %s", stored.Status)
	}
	base.Status = models.PaymentStatusRefunded
	if err := repo.UpsertPayment(ctx, base); err != nil {
		t.Fatal(err)
	}
	base.Status = models.PaymentStatusDisputed
	if err := repo.UpsertPayment(ctx, base); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("provider = ? AND external_id = ?", "stripe", "pi-1").First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != models.PaymentStatusRefunded {
		t.Fatalf("refund status regressed to %s", stored.Status)
	}
}

func TestSubscriptionAndPaymentPagination(t *testing.T) {
	repo, _ := paymentTestStore(t)
	ctx := context.Background()
	for index := 0; index < 3; index++ {
		end := time.Now().UTC().Add(time.Duration(index+1) * time.Hour)
		if err := repo.UpsertSubscription(ctx, models.PaymentSubscription{Provider: "stripe", ExternalSubscriptionID: string(rune('a' + index)),
			IdentityID: "user-1", Status: models.SubscriptionStatusActive, CurrentPeriodEnd: end}); err != nil {
			t.Fatal(err)
		}
		if err := repo.UpsertPayment(ctx, models.PaymentEvent{Provider: "stripe", ExternalID: string(rune('a' + index)), IdentityID: "user-1",
			Status: models.PaymentStatusSucceeded, Kind: "one_time", OccurredAt: end}); err != nil {
			t.Fatal(err)
		}
	}
	subscriptions, err := repo.ListSubscriptions(ctx, "user-1", 1, 1)
	if err != nil || len(subscriptions) != 1 || subscriptions[0].ExternalSubscriptionID != "b" {
		t.Fatalf("subscription page = %+v, %v", subscriptions, err)
	}
	payments, err := repo.ListPayments(ctx, "user-1", 1, 1)
	if err != nil || len(payments) != 1 || payments[0].ExternalID != "b" {
		t.Fatalf("payment page = %+v, %v", payments, err)
	}
}
