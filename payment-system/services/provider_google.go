package services

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bartek5186/procyon-modules/payment-system/models"
	"go.uber.org/zap"
	"google.golang.org/api/androidpublisher/v3"
	"google.golang.org/api/option"
)

const googlePaymentProviderName = "google"

type googlePaymentProvider struct {
	repo        paymentRepository
	client      *androidpublisher.Service
	logger      *zap.Logger
	packageName string
}

func init() {
	registerPaymentProviderFactory(newGooglePaymentProvider)
}

func newGooglePaymentProvider(repo paymentRepository, logger *zap.Logger) (PaymentProvider, bool, error) {
	credentialsFile := strings.TrimSpace(os.Getenv("GOOGLE_PLAY_SERVICE_ACCOUNT_FILE"))
	if credentialsFile == "" {
		return nil, false, nil
	}
	client, err := androidpublisher.NewService(context.Background(), option.WithCredentialsFile(credentialsFile))
	if err != nil {
		return nil, false, err
	}
	return &googlePaymentProvider{
		repo: repo, client: client, logger: logger,
		packageName: strings.TrimSpace(os.Getenv("GOOGLE_PLAY_PACKAGE_NAME")),
	}, true, nil
}

func (p *googlePaymentProvider) Name() string { return googlePaymentProviderName }

func (p *googlePaymentProvider) VerifyStorePurchase(ctx context.Context, identityID string, input models.PaymentStoreVerificationInput) error {
	packageName := strings.TrimSpace(input.PackageName)
	if packageName == "" {
		packageName = p.packageName
	}
	if packageName == "" || strings.TrimSpace(input.ProductID) == "" || strings.TrimSpace(input.PurchaseToken) == "" {
		return fmt.Errorf("package_name, product_id and purchase_token are required")
	}
	purchase, err := p.client.Purchases.Subscriptions.Get(packageName, input.ProductID, input.PurchaseToken).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("verify Google Play subscription: %w", err)
	}
	if purchase.AcknowledgementState == 0 {
		if err := p.client.Purchases.Subscriptions.Acknowledge(
			packageName,
			input.ProductID,
			input.PurchaseToken,
			&androidpublisher.SubscriptionPurchasesAcknowledgeRequest{},
		).Context(ctx).Do(); err != nil {
			return fmt.Errorf("acknowledge Google Play subscription: %w", err)
		}
	}

	expiresAt, err := googleMillisecondsTime(purchase.ExpiryTimeMillis)
	if err != nil {
		return err
	}
	startsAt, _ := googleMillisecondsTime(purchase.StartTimeMillis)
	status := models.SubscriptionStatusActive
	if purchase.PaymentState != nil && *purchase.PaymentState == 0 {
		status = models.SubscriptionStatusPastDue
	}
	if purchase.CancelReason != 0 {
		status = models.SubscriptionStatusCanceled
	}
	if !expiresAt.After(time.Now().UTC()) {
		status = models.SubscriptionStatusExpired
	}
	return p.repo.UpsertSubscription(ctx, models.PaymentSubscription{
		Provider:               p.Name(),
		ExternalSubscriptionID: input.PurchaseToken,
		IdentityID:             identityID,
		PlanCode:               input.ProductID,
		Status:                 status,
		CurrentPeriodStart:     startsAt,
		CurrentPeriodEnd:       expiresAt,
		CancelAtPeriodEnd:      purchase.AutoRenewing == false,
	})
}

func googleMillisecondsTime(milliseconds int64) (time.Time, error) {
	if milliseconds <= 0 {
		return time.Time{}, fmt.Errorf("invalid Google Play timestamp")
	}
	return time.UnixMilli(milliseconds).UTC(), nil
}
