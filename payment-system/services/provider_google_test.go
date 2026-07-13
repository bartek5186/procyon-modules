package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bartek5186/procyon-modules/payment-system/models"
	"go.uber.org/zap"
	"google.golang.org/api/androidpublisher/v3"
	"google.golang.org/api/option"
)

func TestGoogleVerifyUsesSubscriptionsV2AndEncryptsToken(t *testing.T) {
	var acknowledged bool
	httpClient := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, ":acknowledge") {
			acknowledged = true
			return jsonHTTPResponse(http.StatusOK, `{}`), nil
		}
		if !strings.Contains(request.URL.Path, "/purchases/subscriptionsv2/tokens/token-secret") {
			return jsonHTTPResponse(http.StatusNotFound, `{}`), nil
		}
		return jsonHTTPResponse(http.StatusOK, `{
			"startTime":"2026-07-13T10:00:00Z",
			"subscriptionState":"SUBSCRIPTION_STATE_ACTIVE",
			"acknowledgementState":"ACKNOWLEDGEMENT_STATE_PENDING",
			"externalAccountIdentifiers":{"obfuscatedExternalAccountId":"`+googleAccountBinding("user-1")+`"},
			"lineItems":[{"productId":"premium.monthly","expiryTime":"2026-08-13T10:00:00Z",
			"latestSuccessfulOrderId":"GPA.1","autoRenewingPlan":{"autoRenewEnabled":true}}]
		}`), nil
	})}
	client, err := androidpublisher.NewService(context.Background(), option.WithEndpoint("https://google.test/"), option.WithHTTPClient(httpClient))
	if err != nil {
		t.Fatal(err)
	}
	repo := &fakePaymentModuleRepository{}
	provider := &googlePaymentProvider{repo: repo, client: client, logger: zap.NewNop(), packageName: "com.example.app",
		encryptionKey: []byte("01234567890123456789012345678901"), webhookLease: time.Minute,
		products: map[string]Product{"premium.monthly": {Provider: "google", ProductID: "premium.monthly", PlanCode: "premium", Kind: "subscription"}}}
	if err := provider.verifyAndStore(context.Background(), "user-1", "token-secret", "premium.monthly", true); err != nil {
		t.Fatalf("verify Google purchase: %v", err)
	}
	if !acknowledged {
		t.Fatal("pending purchase was not acknowledged")
	}
	if len(repo.subscriptions) != 1 || repo.subscriptions[0].PlanCode != "premium" || repo.subscriptions[0].ProviderData == "token-secret" {
		t.Fatalf("unexpected subscription: %+v", repo.subscriptions)
	}
	plain, err := provider.decryptToken(repo.subscriptions[0].ProviderData)
	if err != nil || plain != "token-secret" {
		t.Fatalf("encrypted token cannot be restored: %q, %v", plain, err)
	}
	if len(repo.payments) != 1 || repo.payments[0].ExternalID != "GPA.1" || repo.payments[0].Status != models.PaymentStatusSucceeded {
		t.Fatalf("unexpected payment: %+v", repo.payments)
	}
}

func TestGoogleRejectsCrossAccountPurchase(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		return jsonHTTPResponse(http.StatusOK, `{
			"startTime":"2026-07-13T10:00:00Z",
			"subscriptionState":"SUBSCRIPTION_STATE_ACTIVE",
			"acknowledgementState":"ACKNOWLEDGEMENT_STATE_ACKNOWLEDGED",
			"externalAccountIdentifiers":{"obfuscatedExternalAccountId":"another-account"},
			"lineItems":[{"productId":"premium.monthly","expiryTime":"2026-08-13T10:00:00Z"}]
		}`), nil
	})}
	client, err := androidpublisher.NewService(context.Background(), option.WithEndpoint("https://google.test/"), option.WithHTTPClient(httpClient))
	if err != nil {
		t.Fatal(err)
	}
	provider := &googlePaymentProvider{repo: &fakePaymentModuleRepository{}, client: client, packageName: "com.example.app",
		encryptionKey: []byte("01234567890123456789012345678901"), products: map[string]Product{"premium.monthly": {
			Provider: "google", ProductID: "premium.monthly", PlanCode: "premium", Kind: "subscription",
		}}}
	if err := provider.verifyAndStore(context.Background(), "user-1", "token", "premium.monthly", true); err != models.ErrPaymentOwnershipConflict {
		t.Fatalf("expected ownership conflict, got %v", err)
	}
}

func googleAccountBinding(identityID string) string {
	provider := googlePaymentProvider{}
	return provider.accountBinding(identityID)
}

func TestGoogleSubscriptionStateMapping(t *testing.T) {
	tests := map[string]models.SubscriptionStatus{
		"SUBSCRIPTION_STATE_ACTIVE":          models.SubscriptionStatusActive,
		"SUBSCRIPTION_STATE_IN_GRACE_PERIOD": models.SubscriptionStatusTrialing,
		"SUBSCRIPTION_STATE_ON_HOLD":         models.SubscriptionStatusPastDue,
		"SUBSCRIPTION_STATE_PAUSED":          models.SubscriptionStatusPastDue,
		"SUBSCRIPTION_STATE_CANCELED":        models.SubscriptionStatusCanceled,
		"SUBSCRIPTION_STATE_EXPIRED":         models.SubscriptionStatusExpired,
	}
	for input, expected := range tests {
		if actual := mapGoogleV2Status(input); actual != expected {
			t.Fatalf("%s: expected %s, got %s", input, expected, actual)
		}
	}
}

func TestGoogleRTDNAuthenticatedEnvelope(t *testing.T) {
	repo := &fakePaymentModuleRepository{}
	provider := &googlePaymentProvider{repo: repo, packageName: "com.example.app", pubsubAudience: "https://api.example/webhook",
		webhookLease: time.Minute, validatePushToken: func(_ context.Context, token, audience string) error {
			if token != "signed-token" || audience != "https://api.example/webhook" {
				return ErrPaymentInvalidSignature
			}
			return nil
		}}
	notification, _ := json.Marshal(map[string]any{"packageName": "com.example.app", "testNotification": map[string]any{}})
	envelope, _ := json.Marshal(map[string]any{"message": map[string]any{"messageId": "message-1",
		"data": base64.StdEncoding.EncodeToString(notification)}})
	if err := provider.HandleWebhook(context.Background(), envelope, "Bearer signed-token"); err != nil {
		t.Fatalf("handle Google RTDN: %v", err)
	}
	if len(repo.webhooks) != 1 || repo.webhooks[0].EventID != "message-1" || repo.webhooks[0].EventType != "TEST" {
		t.Fatalf("unexpected webhook claim: %+v", repo.webhooks)
	}
	if err := provider.HandleWebhook(context.Background(), envelope, "Bearer wrong"); err != ErrPaymentInvalidSignature {
		t.Fatalf("expected invalid push token error, got %v", err)
	}
}

func TestGoogleVoidedPurchaseRevokesEntitlementAndRecordsRefund(t *testing.T) {
	stored := models.PaymentSubscription{Provider: "google", ExternalSubscriptionID: googleTokenKey("token"), IdentityID: "user-1",
		PlanCode: "premium", Status: models.SubscriptionStatusActive}
	repo := &fakePaymentModuleRepository{externalSubscription: &stored}
	provider := &googlePaymentProvider{repo: repo}
	notification := googleDeveloperNotification{Voided: &struct {
		PurchaseToken string `json:"purchaseToken"`
		OrderID       string `json:"orderId"`
		ProductType   int    `json:"productType"`
		RefundType    int    `json:"refundType"`
	}{PurchaseToken: "token", OrderID: "GPA.refund"}}
	if err := provider.processNotification(context.Background(), notification); err != nil {
		t.Fatal(err)
	}
	if len(repo.subscriptions) != 1 || repo.subscriptions[0].Status != models.SubscriptionStatusRefunded ||
		len(repo.payments) != 1 || repo.payments[0].Status != models.PaymentStatusRefunded {
		t.Fatalf("unexpected void result: subscriptions=%+v payments=%+v", repo.subscriptions, repo.payments)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func jsonHTTPResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewBufferString(body))}
}
