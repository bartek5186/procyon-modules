package services

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bartek5186/procyon-modules/payment-system/models"
	"go.uber.org/zap"
)

var (
	appleLeafOID         = []int{1, 2, 840, 113635, 100, 6, 11, 1}
	appleIntermediateOID = []int{1, 2, 840, 113635, 100, 6, 2, 1}
)

func TestAppleJWSFixtureAndOwnership(t *testing.T) {
	provider, signer, chain := appleFixtureProvider(t)
	now := time.Now().UTC()
	payload := appleTransactionPayload{TransactionID: "tx-1", OriginalTransactionID: "original-1", ProductID: "premium.monthly",
		BundleID: "com.example.app", AppAccountToken: "user-1", PurchaseDate: now.UnixMilli(),
		ExpiresDate: now.Add(30 * 24 * time.Hour).UnixMilli(), SignedDate: now.UnixMilli(), Environment: "Sandbox"}
	token := signAppleFixture(t, signer, chain, payload)
	if err := provider.processAppleTransaction(context.Background(), token, "user-1", "PURCHASE_VERIFIED", nil); err != nil {
		t.Fatalf("process valid Apple transaction: %v", err)
	}
	repo := provider.repo.(*fakePaymentModuleRepository)
	if len(repo.subscriptions) != 1 || repo.subscriptions[0].IdentityID != "user-1" || repo.subscriptions[0].PlanCode != "premium" {
		t.Fatalf("unexpected subscription: %+v", repo.subscriptions)
	}
	if len(repo.payments) != 1 || repo.payments[0].AmountMinor != 1999 || repo.payments[0].Currency != "PLN" {
		t.Fatalf("unexpected payment: %+v", repo.payments)
	}
	if err := provider.processAppleTransaction(context.Background(), token, "user-2", "PURCHASE_VERIFIED", nil); err != models.ErrPaymentOwnershipConflict {
		t.Fatalf("expected ownership conflict, got %v", err)
	}
}

func TestAppleJWSRejectsWrongEnvironmentAndChain(t *testing.T) {
	provider, signer, chain := appleFixtureProvider(t)
	payload := appleTransactionPayload{SignedDate: time.Now().UTC().UnixMilli(), BundleID: "com.example.app", Environment: "Production"}
	token := signAppleFixture(t, signer, chain, payload)
	if err := provider.processAppleTransaction(context.Background(), token, "", "PURCHASE_VERIFIED", nil); err != ErrPaymentInvalidSignature {
		t.Fatalf("expected environment signature rejection, got %v", err)
	}
	otherProvider, otherSigner, otherChain := appleFixtureProvider(t)
	_ = otherProvider
	wrongChainToken := signAppleFixture(t, otherSigner, otherChain, appleTransactionPayload{SignedDate: time.Now().UTC().UnixMilli()})
	var decoded appleTransactionPayload
	if err := provider.verifyJWS(wrongChainToken, &decoded); err == nil {
		t.Fatal("expected untrusted certificate chain rejection")
	}
}

func TestAppleRefundRecordsRefundedPayment(t *testing.T) {
	provider, signer, chain := appleFixtureProvider(t)
	now := time.Now().UTC()
	payload := appleTransactionPayload{TransactionID: "tx-refund", OriginalTransactionID: "original-1", ProductID: "premium.monthly",
		BundleID: "com.example.app", AppAccountToken: "user-1", PurchaseDate: now.UnixMilli(),
		ExpiresDate: now.Add(time.Hour).UnixMilli(), SignedDate: now.UnixMilli(), Environment: "Sandbox"}
	if err := provider.processAppleTransaction(context.Background(), signAppleFixture(t, signer, chain, payload), "", "REFUND", nil); err != nil {
		t.Fatal(err)
	}
	repo := provider.repo.(*fakePaymentModuleRepository)
	if len(repo.payments) != 1 || repo.payments[0].Status != models.PaymentStatusRefunded {
		t.Fatalf("unexpected refund payment: %+v", repo.payments)
	}
}

func TestAppleSubscriptionLifecycleMapping(t *testing.T) {
	future := time.Now().UTC().Add(time.Hour).UnixMilli()
	tests := map[string]models.SubscriptionStatus{
		"DID_RENEW":         models.SubscriptionStatusActive,
		"EXPIRED":           models.SubscriptionStatusExpired,
		"DID_FAIL_TO_RENEW": models.SubscriptionStatusPastDue,
		"REFUND":            models.SubscriptionStatusRefunded,
		"REVOKE":            models.SubscriptionStatusRevoked,
	}
	for notification, expected := range tests {
		if actual := appleSubscriptionStatus(notification, appleTransactionPayload{ExpiresDate: future}); actual != expected {
			t.Fatalf("%s: expected %s, got %s", notification, expected, actual)
		}
	}
}

func TestAppleReconciliationUsesServerAPIAndSignedStatus(t *testing.T) {
	provider, signer, chain := appleFixtureProvider(t)
	now := time.Now().UTC()
	transaction := signAppleFixture(t, signer, chain, appleTransactionPayload{TransactionID: "tx-latest", OriginalTransactionID: "original-1",
		ProductID: "premium.monthly", BundleID: "com.example.app", AppAccountToken: "user-1", PurchaseDate: now.UnixMilli(),
		ExpiresDate: now.Add(24 * time.Hour).UnixMilli(), SignedDate: now.UnixMilli(), Environment: "Sandbox"})
	renewal := signAppleFixture(t, signer, chain, appleRenewalPayload{OriginalTransactionID: "original-1", ProductID: "premium.monthly",
		AutoRenewStatus: 1, GracePeriodExpiresDate: now.Add(time.Hour).UnixMilli(), SignedDate: now.UnixMilli(), Environment: "Sandbox"})
	body, _ := json.Marshal(map[string]any{"environment": "Sandbox", "bundleId": "com.example.app", "data": []any{map[string]any{
		"lastTransactions": []any{map[string]any{"originalTransactionId": "original-1", "status": 4,
			"signedTransactionInfo": transaction, "signedRenewalInfo": renewal}},
	}}})
	provider.issuerID = "issuer-fixture"
	provider.keyID = "key-fixture"
	provider.privateKey = signer
	provider.serverAPIURL = "https://apple.test"
	provider.httpClient = &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ") || !strings.HasSuffix(request.URL.Path, "/original-1") {
			return jsonHTTPResponse(http.StatusUnauthorized, `{}`), nil
		}
		return jsonHTTPResponse(http.StatusOK, string(body)), nil
	})}
	if err := provider.ReconcileSubscription(context.Background(), models.PaymentSubscription{Provider: "apple",
		ExternalSubscriptionID: "original-1", IdentityID: "user-1"}); err != nil {
		t.Fatalf("reconcile Apple subscription: %v", err)
	}
	repo := provider.repo.(*fakePaymentModuleRepository)
	if len(repo.subscriptions) != 1 || repo.subscriptions[0].Status != models.SubscriptionStatusTrialing {
		t.Fatalf("unexpected reconciled subscription: %+v", repo.subscriptions)
	}
}

func TestAppleSignedNotificationWebhook(t *testing.T) {
	provider, signer, chain := appleFixtureProvider(t)
	now := time.Now().UTC()
	transaction := signAppleFixture(t, signer, chain, appleTransactionPayload{TransactionID: "tx-webhook", OriginalTransactionID: "original-webhook",
		ProductID: "premium.monthly", BundleID: "com.example.app", AppAccountToken: "user-1", PurchaseDate: now.UnixMilli(),
		ExpiresDate: now.Add(time.Hour).UnixMilli(), SignedDate: now.UnixMilli(), Environment: "Sandbox"})
	notification := appleNotificationPayload{NotificationType: "DID_RENEW", NotificationUUID: "notification-1", SignedDate: now.UnixMilli()}
	notification.Data.BundleID = "com.example.app"
	notification.Data.Environment = "Sandbox"
	notification.Data.SignedTransactionInfo = transaction
	signedNotification := signAppleFixture(t, signer, chain, notification)
	body, _ := json.Marshal(appleNotificationEnvelope{SignedPayload: signedNotification})
	if err := provider.HandleWebhook(context.Background(), body, ""); err != nil {
		t.Fatalf("handle Apple notification: %v", err)
	}
	repo := provider.repo.(*fakePaymentModuleRepository)
	if len(repo.webhooks) != 1 || repo.webhooks[0].EventID != "notification-1" || len(repo.subscriptions) != 1 {
		t.Fatalf("unexpected Apple webhook result: webhooks=%+v subscriptions=%+v", repo.webhooks, repo.subscriptions)
	}
}

func appleFixtureProvider(t *testing.T) (*applePaymentProvider, *ecdsa.PrivateKey, []*x509.Certificate) {
	t.Helper()
	now := time.Now().UTC()
	rootKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rootTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Fixture Root"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := x509.ParseCertificate(rootDER)
	intermediateKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	intermediateTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Fixture Intermediate"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(12 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage:        x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		ExtraExtensions: []pkix.Extension{{Id: appleIntermediateOID, Value: []byte{5, 0}}}}
	intermediateDER, err := x509.CreateCertificate(rand.Reader, intermediateTemplate, root, &intermediateKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	intermediate, _ := x509.ParseCertificate(intermediateDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "Fixture Leaf"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(6 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtraExtensions: []pkix.Extension{{Id: appleLeafOID, Value: []byte{5, 0}}}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, intermediate, &leafKey.PublicKey, intermediateKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(leafDER)
	roots := x509.NewCertPool()
	roots.AddCert(root)
	return &applePaymentProvider{repo: &fakePaymentModuleRepository{}, logger: zap.NewNop(), bundleID: "com.example.app",
		environment: "Sandbox", roots: roots, products: map[string]Product{"premium.monthly": {Provider: "apple", ProductID: "premium.monthly",
			PlanCode: "premium", Kind: "subscription", AmountMinor: 1999, Currency: "PLN"}}, httpClient: http.DefaultClient}, leafKey, []*x509.Certificate{leaf, intermediate, root}
}

func signAppleFixture(t *testing.T, key *ecdsa.PrivateKey, certificates []*x509.Certificate, payload any) string {
	t.Helper()
	x5c := make([]string, 0, len(certificates))
	for _, certificate := range certificates {
		x5c = append(x5c, base64.StdEncoding.EncodeToString(certificate.Raw))
	}
	headerRaw, _ := json.Marshal(appleJWSHeader{Algorithm: "ES256", X5C: x5c})
	payloadRaw, _ := json.Marshal(payload)
	header := base64.RawURLEncoding.EncodeToString(headerRaw)
	body := base64.RawURLEncoding.EncodeToString(payloadRaw)
	digest := sha256.Sum256([]byte(header + "." + body))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return header + "." + body + "." + base64.RawURLEncoding.EncodeToString(signature)
}
