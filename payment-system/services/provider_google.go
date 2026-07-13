package services

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bartek5186/procyon-modules/payment-system/models"
	"go.uber.org/zap"
	"google.golang.org/api/androidpublisher/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/idtoken"
	"google.golang.org/api/option"
)

const googlePaymentProviderName = "google"

type googlePaymentProvider struct {
	repo              paymentRepository
	client            *androidpublisher.Service
	logger            *zap.Logger
	packageName       string
	pubsubAudience    string
	encryptionKey     []byte
	products          map[string]Product
	webhookLease      time.Duration
	validatePushToken func(context.Context, string, string) error
}

type googlePushEnvelope struct {
	Message struct {
		Data string `json:"data"`
		ID   string `json:"messageId"`
	} `json:"message"`
}

type googleDeveloperNotification struct {
	PackageName     string `json:"packageName"`
	EventTimeMillis string `json:"eventTimeMillis"`
	Subscription    *struct {
		NotificationType int    `json:"notificationType"`
		PurchaseToken    string `json:"purchaseToken"`
	} `json:"subscriptionNotification"`
	Voided *struct {
		PurchaseToken string `json:"purchaseToken"`
		OrderID       string `json:"orderId"`
		ProductType   int    `json:"productType"`
		RefundType    int    `json:"refundType"`
	} `json:"voidedPurchaseNotification"`
	Test *struct{} `json:"testNotification"`
}

func init() { registerPaymentProviderFactory(newGooglePaymentProvider) }

func newGooglePaymentProvider(repo paymentRepository, logger *zap.Logger, config RuntimeConfig) (PaymentProvider, bool, error) {
	if !containsProvider(config.EnabledProviders, googlePaymentProviderName) {
		return nil, false, nil
	}
	credentialsFile := strings.TrimSpace(os.Getenv("GOOGLE_PLAY_SERVICE_ACCOUNT_FILE"))
	packageName := strings.TrimSpace(os.Getenv("GOOGLE_PLAY_PACKAGE_NAME"))
	audience := strings.TrimSpace(os.Getenv("GOOGLE_PLAY_PUBSUB_AUDIENCE"))
	if credentialsFile == "" || packageName == "" || audience == "" {
		return nil, false, fmt.Errorf("GOOGLE_PLAY_SERVICE_ACCOUNT_FILE, GOOGLE_PLAY_PACKAGE_NAME and GOOGLE_PLAY_PUBSUB_AUDIENCE are required")
	}
	client, err := androidpublisher.NewService(context.Background(), option.WithCredentialsFile(credentialsFile))
	if err != nil {
		return nil, false, err
	}
	return &googlePaymentProvider{repo: repo, client: client, logger: logger, packageName: packageName,
		pubsubAudience: audience, encryptionKey: config.EncryptionKey, products: config.Products[googlePaymentProviderName],
		webhookLease: config.WebhookLease, validatePushToken: func(ctx context.Context, token, audience string) error {
			_, err := idtoken.Validate(ctx, token, audience)
			return err
		}}, true, nil
}

func (p *googlePaymentProvider) Name() string { return googlePaymentProviderName }

func (p *googlePaymentProvider) VerifyStorePurchase(ctx context.Context, identityID string, input models.PaymentStoreVerificationInput) error {
	if input.PackageName != "" && input.PackageName != p.packageName {
		return models.ErrPaymentProductRejected
	}
	return p.verifyAndStore(ctx, identityID, input.PurchaseToken, input.ProductID, true)
}

func (p *googlePaymentProvider) HandleWebhook(ctx context.Context, body []byte, authorization string) error {
	token := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	if token == authorization || token == "" {
		return ErrPaymentInvalidSignature
	}
	validate := p.validatePushToken
	if validate == nil {
		validate = func(ctx context.Context, token, audience string) error {
			_, err := idtoken.Validate(ctx, token, audience)
			return err
		}
	}
	if err := validate(ctx, token, p.pubsubAudience); err != nil {
		return ErrPaymentInvalidSignature
	}
	var envelope googlePushEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Message.ID == "" {
		return fmt.Errorf("invalid Google Pub/Sub envelope")
	}
	decoded, err := base64.StdEncoding.DecodeString(envelope.Message.Data)
	if err != nil {
		return fmt.Errorf("decode Google RTDN: %w", err)
	}
	var notification googleDeveloperNotification
	if err := json.Unmarshal(decoded, &notification); err != nil {
		return fmt.Errorf("parse Google RTDN: %w", err)
	}
	if notification.PackageName != p.packageName {
		return ErrPaymentInvalidSignature
	}
	leaseID, claimed, err := p.repo.ClaimWebhook(ctx, models.PaymentWebhookEvent{Provider: p.Name(), EventID: envelope.Message.ID,
		EventType: googleNotificationType(notification), Payload: body, Signature: authorization}, p.webhookLease)
	if err != nil || !claimed {
		return err
	}
	processErr := p.processNotification(ctx, notification)
	if finishErr := p.repo.FinishWebhook(ctx, p.Name(), envelope.Message.ID, leaseID, processErr); finishErr != nil && processErr == nil {
		return finishErr
	}
	return processErr
}

func (p *googlePaymentProvider) processNotification(ctx context.Context, notification googleDeveloperNotification) error {
	if notification.Test != nil {
		return nil
	}
	if notification.Voided != nil {
		key := googleTokenKey(notification.Voided.PurchaseToken)
		stored, err := p.repo.GetSubscriptionByExternal(ctx, p.Name(), key)
		if err != nil {
			return err
		}
		stored.Status = models.SubscriptionStatusRefunded
		if err := p.repo.UpsertSubscription(ctx, stored); err != nil {
			return err
		}
		return p.repo.UpsertPayment(ctx, models.PaymentEvent{Provider: p.Name(), ExternalID: notification.Voided.OrderID,
			IdentityID: stored.IdentityID, SubscriptionID: key, PriceID: stored.PlanCode, Status: models.PaymentStatusRefunded,
			Kind: "refund", OccurredAt: time.Now().UTC()})
	}
	if notification.Subscription == nil {
		return fmt.Errorf("unsupported Google RTDN")
	}
	key := googleTokenKey(notification.Subscription.PurchaseToken)
	stored, err := p.repo.GetSubscriptionByExternal(ctx, p.Name(), key)
	if err != nil {
		return err
	}
	return p.verifyAndStore(ctx, stored.IdentityID, notification.Subscription.PurchaseToken, "", false)
}

func (p *googlePaymentProvider) verifyAndStore(ctx context.Context, identityID, purchaseToken, expectedProduct string, requireOwnership bool) error {
	if strings.TrimSpace(identityID) == "" || strings.TrimSpace(purchaseToken) == "" {
		return fmt.Errorf("identity and purchase token are required")
	}
	purchase, err := p.client.Purchases.Subscriptionsv2.Get(p.packageName, purchaseToken).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("verify Google Play subscription: %w", err)
	}
	if requireOwnership {
		expected := p.accountBinding(identityID)
		actual := ""
		if purchase.ExternalAccountIdentifiers != nil {
			actual = purchase.ExternalAccountIdentifiers.ObfuscatedExternalAccountId
		}
		if actual == "" || !hmac.Equal([]byte(actual), []byte(expected)) {
			return models.ErrPaymentOwnershipConflict
		}
	}
	if len(purchase.LineItems) == 0 {
		return fmt.Errorf("Google subscription has no line items")
	}
	item := purchase.LineItems[0]
	if expectedProduct != "" && item.ProductId != expectedProduct {
		return models.ErrPaymentProductRejected
	}
	product, err := RuntimeConfig{Products: map[string]map[string]Product{p.Name(): p.products}}.Product(p.Name(), item.ProductId, "subscription")
	if err != nil {
		return err
	}
	if purchase.AcknowledgementState == "ACKNOWLEDGEMENT_STATE_PENDING" {
		err := p.client.Purchases.Subscriptions.Acknowledge(p.packageName, item.ProductId, purchaseToken,
			&androidpublisher.SubscriptionPurchasesAcknowledgeRequest{}).Context(ctx).Do()
		if err != nil && !googleAcknowledgeAlreadyDone(err) {
			return fmt.Errorf("acknowledge Google Play subscription: %w", err)
		}
	}
	start, err := time.Parse(time.RFC3339Nano, purchase.StartTime)
	if err != nil && purchase.StartTime != "" {
		return fmt.Errorf("invalid Google start time: %w", err)
	}
	end, err := time.Parse(time.RFC3339Nano, item.ExpiryTime)
	if err != nil {
		return fmt.Errorf("invalid Google expiry time: %w", err)
	}
	providerData, err := p.encryptToken(purchaseToken)
	if err != nil {
		return err
	}
	status := mapGoogleV2Status(purchase.SubscriptionState)
	autoRenew := item.AutoRenewingPlan != nil && item.AutoRenewingPlan.AutoRenewEnabled
	subscription := models.PaymentSubscription{Provider: p.Name(), ExternalSubscriptionID: googleTokenKey(purchaseToken),
		IdentityID: identityID, PlanCode: product.PlanCode, Status: status, CurrentPeriodStart: start,
		CurrentPeriodEnd: end, CancelAtPeriodEnd: !autoRenew, ProviderData: providerData}
	if err := p.repo.UpsertSubscription(ctx, subscription); err != nil {
		return err
	}
	logPaymentAudit(p.logger, "entitlement_changed", p.Name(), subscription.IdentityID, subscription.ExternalSubscriptionID, subscription.PlanCode, subscription.Status)
	orderID := item.LatestSuccessfulOrderId
	if orderID == "" {
		orderID = purchase.LatestOrderId
	}
	if orderID != "" && (status == models.SubscriptionStatusActive || status == models.SubscriptionStatusTrialing) {
		return p.repo.UpsertPayment(ctx, models.PaymentEvent{Provider: p.Name(), ExternalID: orderID, IdentityID: identityID,
			SubscriptionID: subscription.ExternalSubscriptionID, PriceID: product.PlanCode, Status: models.PaymentStatusSucceeded,
			Kind: "subscription_cycle", AmountMinor: product.AmountMinor, Currency: product.Currency, OccurredAt: start})
	}
	return nil
}

func (p *googlePaymentProvider) ReconcileSubscription(ctx context.Context, stored models.PaymentSubscription) error {
	token, err := p.decryptToken(stored.ProviderData)
	if err != nil {
		return err
	}
	return p.verifyAndStore(ctx, stored.IdentityID, token, "", false)
}

func (p *googlePaymentProvider) accountBinding(identityID string) string {
	sum := sha256.Sum256([]byte("procyon-payment:" + strings.TrimSpace(identityID)))
	return hex.EncodeToString(sum[:])
}

func googleTokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (p *googlePaymentProvider) encryptToken(token string) (string, error) {
	block, err := aes.NewCipher(p.encryptionKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(token), []byte(p.packageName))
	return base64.RawStdEncoding.EncodeToString(sealed), nil
}

func (p *googlePaymentProvider) decryptToken(encoded string) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(p.encryptionKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("invalid encrypted Google token")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], []byte(p.packageName))
	return string(plain), err
}

func mapGoogleV2Status(value string) models.SubscriptionStatus {
	switch value {
	case "SUBSCRIPTION_STATE_ACTIVE":
		return models.SubscriptionStatusActive
	case "SUBSCRIPTION_STATE_IN_GRACE_PERIOD":
		return models.SubscriptionStatusTrialing
	case "SUBSCRIPTION_STATE_ON_HOLD", "SUBSCRIPTION_STATE_PAUSED":
		return models.SubscriptionStatusPastDue
	case "SUBSCRIPTION_STATE_CANCELED":
		return models.SubscriptionStatusCanceled
	default:
		return models.SubscriptionStatusExpired
	}
}

func googleNotificationType(notification googleDeveloperNotification) string {
	if notification.Test != nil {
		return "TEST"
	}
	if notification.Voided != nil {
		return "VOIDED_PURCHASE"
	}
	if notification.Subscription != nil {
		return fmt.Sprintf("SUBSCRIPTION_%d", notification.Subscription.NotificationType)
	}
	return "UNKNOWN"
}

func googleAcknowledgeAlreadyDone(err error) bool {
	var apiError *googleapi.Error
	return errors.As(err, &apiError) && (apiError.Code == 400 || apiError.Code == 409)
}
