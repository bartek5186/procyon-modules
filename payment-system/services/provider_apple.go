package services

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bartek5186/procyon-modules/payment-system/models"
	"go.uber.org/zap"
	"golang.org/x/crypto/ocsp"
	"gorm.io/gorm"
)

const applePaymentProviderName = "apple"

type applePaymentConfig struct {
	BundleID     string `json:"bundle_id"`
	AppAppleID   int64  `json:"app_apple_id"`
	Environment  string `json:"environment"`
	RootCA       string `json:"root_ca"`
	OnlineChecks *bool  `json:"online_checks"`
	IssuerID     string `json:"issuer_id"`
	KeyID        string `json:"key_id"`
	PrivateKey   string `json:"private_key"`
}

type applePaymentProvider struct {
	repo         paymentRepository
	logger       *zap.Logger
	bundleID     string
	appAppleID   int64
	environment  string
	roots        *x509.CertPool
	products     map[string]Product
	webhookLease time.Duration
	onlineChecks bool
	httpClient   *http.Client
	issuerID     string
	keyID        string
	privateKey   *ecdsa.PrivateKey
	serverAPIURL string
}

type appleStatusResponse struct {
	Environment string `json:"environment"`
	BundleID    string `json:"bundleId"`
	AppAppleID  int64  `json:"appAppleId"`
	Data        []struct {
		LastTransactions []struct {
			OriginalTransactionID string `json:"originalTransactionId"`
			Status                int    `json:"status"`
			SignedTransactionInfo string `json:"signedTransactionInfo"`
			SignedRenewalInfo     string `json:"signedRenewalInfo"`
		} `json:"lastTransactions"`
	} `json:"data"`
}

type appleNotificationEnvelope struct {
	SignedPayload string `json:"signedPayload"`
}

type appleNotificationPayload struct {
	NotificationType string `json:"notificationType"`
	Subtype          string `json:"subtype"`
	NotificationUUID string `json:"notificationUUID"`
	SignedDate       int64  `json:"signedDate"`
	Data             struct {
		SignedTransactionInfo string `json:"signedTransactionInfo"`
		SignedRenewalInfo     string `json:"signedRenewalInfo"`
		BundleID              string `json:"bundleId"`
		Environment           string `json:"environment"`
		AppAppleID            int64  `json:"appAppleId"`
	} `json:"data"`
}

type appleTransactionPayload struct {
	TransactionID         string `json:"transactionId"`
	OriginalTransactionID string `json:"originalTransactionId"`
	ProductID             string `json:"productId"`
	BundleID              string `json:"bundleId"`
	AppAccountToken       string `json:"appAccountToken"`
	PurchaseDate          int64  `json:"purchaseDate"`
	ExpiresDate           int64  `json:"expiresDate"`
	RevocationDate        int64  `json:"revocationDate"`
	SignedDate            int64  `json:"signedDate"`
	Environment           string `json:"environment"`
}

type appleRenewalPayload struct {
	OriginalTransactionID  string `json:"originalTransactionId"`
	ProductID              string `json:"productId"`
	AutoRenewProductID     string `json:"autoRenewProductId"`
	AutoRenewStatus        int    `json:"autoRenewStatus"`
	ExpirationIntent       int    `json:"expirationIntent"`
	GracePeriodExpiresDate int64  `json:"gracePeriodExpiresDate"`
	SignedDate             int64  `json:"signedDate"`
	Environment            string `json:"environment"`
}

type appleJWSHeader struct {
	Algorithm string   `json:"alg"`
	X5C       []string `json:"x5c"`
}

func init() {
	registerPaymentProviderFactory(newApplePaymentProvider)
}

func newApplePaymentProvider(repo paymentRepository, logger *zap.Logger, runtime RuntimeConfig) (PaymentProvider, bool, error) {
	if !containsProvider(runtime.EnabledProviders, applePaymentProviderName) {
		return nil, false, nil
	}
	configPath := strings.TrimSpace(os.Getenv("APPLE_APP_STORE_CONFIG_FILE"))
	if configPath == "" {
		return nil, false, errors.New("APPLE_APP_STORE_CONFIG_FILE is required when Apple is enabled")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, false, err
	}
	var config applePaymentConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, false, fmt.Errorf("parse Apple payment config: %w", err)
	}
	config.BundleID = strings.TrimSpace(config.BundleID)
	config.Environment = strings.TrimSpace(config.Environment)
	if config.BundleID == "" || (config.Environment != "Sandbox" && config.Environment != "Production") {
		return nil, false, errors.New("Apple bundle_id and environment (Sandbox or Production) are required")
	}
	if config.Environment == "Production" && config.AppAppleID <= 0 {
		return nil, false, errors.New("Apple app_apple_id is required in Production")
	}
	config.IssuerID = strings.TrimSpace(config.IssuerID)
	config.KeyID = strings.TrimSpace(config.KeyID)
	config.PrivateKey = strings.TrimSpace(config.PrivateKey)
	if config.IssuerID == "" || config.KeyID == "" || config.PrivateKey == "" {
		return nil, false, errors.New("Apple issuer_id, key_id and private_key are required for reconciliation")
	}
	privateKeyPath := config.PrivateKey
	if !strings.HasPrefix(privateKeyPath, "/") {
		privateKeyPath = filepath.Join(filepath.Dir(configPath), privateKeyPath)
	}
	privateKeyPEM, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, false, fmt.Errorf("read Apple private key: %w", err)
	}
	privateKey, err := parseApplePrivateKey(privateKeyPEM)
	if err != nil {
		return nil, false, err
	}
	onlineChecks := config.Environment == "Production"
	if config.OnlineChecks != nil {
		onlineChecks = *config.OnlineChecks
	}
	rootPEM := []byte(config.RootCA)
	if !bytesContainsPEM(rootPEM) {
		rootPath := strings.TrimSpace(config.RootCA)
		if rootPath == "" {
			return nil, false, errors.New("Apple payment root_ca is required")
		}
		if !strings.HasPrefix(rootPath, "/") {
			rootPath = filepath.Join(filepath.Dir(configPath), rootPath)
		}
		rootPEM, err = os.ReadFile(rootPath)
		if err != nil {
			return nil, false, err
		}
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM) {
		return nil, false, errors.New("invalid Apple root CA")
	}
	serverAPIURL := "https://api.storekit.apple.com"
	if config.Environment == "Sandbox" {
		serverAPIURL = "https://api.storekit-sandbox.apple.com"
	}
	return &applePaymentProvider{repo: repo, logger: logger, bundleID: config.BundleID, appAppleID: config.AppAppleID,
		environment: config.Environment, roots: roots, products: runtime.Products[applePaymentProviderName],
		webhookLease: runtime.WebhookLease, onlineChecks: onlineChecks, httpClient: &http.Client{Timeout: 10 * time.Second},
		issuerID: config.IssuerID, keyID: config.KeyID, privateKey: privateKey, serverAPIURL: serverAPIURL}, true, nil
}

func (p *applePaymentProvider) Name() string { return applePaymentProviderName }

func (p *applePaymentProvider) HandleWebhook(ctx context.Context, body []byte, _ string) error {
	var envelope appleNotificationEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil || strings.TrimSpace(envelope.SignedPayload) == "" {
		return fmt.Errorf("invalid Apple notification envelope")
	}
	var notification appleNotificationPayload
	if err := p.verifyJWS(envelope.SignedPayload, &notification); err != nil {
		return ErrPaymentInvalidSignature
	}
	if notification.Data.BundleID != p.bundleID || notification.Data.Environment != p.environment ||
		(p.environment == "Production" && notification.Data.AppAppleID != p.appAppleID) {
		return ErrPaymentInvalidSignature
	}
	if notification.NotificationUUID == "" {
		return fmt.Errorf("Apple notification UUID is missing")
	}
	leaseID, claimed, err := p.repo.ClaimWebhook(ctx, models.PaymentWebhookEvent{Provider: p.Name(), EventID: notification.NotificationUUID,
		EventType: notification.NotificationType + ":" + notification.Subtype, Payload: body}, p.webhookLease)
	if err != nil || !claimed {
		return err
	}
	processErr := p.processAppleNotification(ctx, notification)
	if finishErr := p.repo.FinishWebhook(ctx, p.Name(), notification.NotificationUUID, leaseID, processErr); finishErr != nil && processErr == nil {
		return finishErr
	}
	return processErr
}

func (p *applePaymentProvider) VerifyStorePurchase(ctx context.Context, identityID string, input models.PaymentStoreVerificationInput) error {
	if strings.TrimSpace(input.SignedPayload) == "" {
		return fmt.Errorf("signed_payload is required for Apple verification")
	}
	return p.processAppleTransaction(ctx, input.SignedPayload, identityID, "PURCHASE_VERIFIED", nil)
}

func (p *applePaymentProvider) processAppleNotification(ctx context.Context, notification appleNotificationPayload) error {
	var renewal *appleRenewalPayload
	if notification.Data.SignedRenewalInfo != "" {
		var decoded appleRenewalPayload
		if err := p.verifyJWS(notification.Data.SignedRenewalInfo, &decoded); err != nil || decoded.Environment != p.environment {
			return ErrPaymentInvalidSignature
		}
		renewal = &decoded
	}
	if notification.Data.SignedTransactionInfo == "" {
		p.logger.Info("valid Apple notification without transaction", zap.String("type", notification.NotificationType))
		return nil
	}
	return p.processAppleTransaction(ctx, notification.Data.SignedTransactionInfo, "", notification.NotificationType, renewal)
}

func (p *applePaymentProvider) processAppleTransaction(ctx context.Context, signedTransaction, expectedIdentity, notificationType string, renewal *appleRenewalPayload) error {
	if strings.TrimSpace(signedTransaction) == "" {
		return fmt.Errorf("Apple signed transaction is missing")
	}
	var transaction appleTransactionPayload
	if err := p.verifyJWS(signedTransaction, &transaction); err != nil {
		return ErrPaymentInvalidSignature
	}
	if transaction.BundleID != p.bundleID || transaction.Environment != p.environment {
		return ErrPaymentInvalidSignature
	}
	identityID := strings.TrimSpace(transaction.AppAccountToken)
	if expectedIdentity != "" && identityID != strings.TrimSpace(expectedIdentity) {
		return models.ErrPaymentOwnershipConflict
	}
	if identityID == "" || transaction.OriginalTransactionID == "" {
		return fmt.Errorf("Apple transaction cannot be associated with an identity")
	}
	status := appleSubscriptionStatus(notificationType, transaction)
	cancelAtPeriodEnd := false
	if renewal != nil {
		if renewal.OriginalTransactionID != "" && renewal.OriginalTransactionID != transaction.OriginalTransactionID {
			return ErrPaymentInvalidSignature
		}
		cancelAtPeriodEnd = renewal.AutoRenewStatus == 0
		if renewal.GracePeriodExpiresDate > time.Now().UTC().UnixMilli() {
			status = models.SubscriptionStatusTrialing
		}
		if renewal.ExpirationIntent != 0 {
			status = models.SubscriptionStatusPastDue
		}
	}
	product, err := RuntimeConfig{Products: map[string]map[string]Product{p.Name(): p.products}}.Product(p.Name(), transaction.ProductID, "subscription")
	if err != nil {
		return err
	}
	start := appleMillisecondsTime(transaction.PurchaseDate)
	end := appleMillisecondsTime(transaction.ExpiresDate)
	subscription := models.PaymentSubscription{
		Provider:               p.Name(),
		ExternalSubscriptionID: transaction.OriginalTransactionID,
		IdentityID:             identityID,
		PlanCode:               product.PlanCode,
		Status:                 status,
		CurrentPeriodStart:     start,
		CurrentPeriodEnd:       end,
		CancelAtPeriodEnd:      cancelAtPeriodEnd,
	}
	if err := p.repo.UpsertSubscription(ctx, subscription); err != nil {
		return err
	}
	logPaymentAudit(p.logger, "entitlement_changed", p.Name(), subscription.IdentityID, subscription.ExternalSubscriptionID, subscription.PlanCode, subscription.Status)
	if transaction.TransactionID != "" {
		paymentStatus := models.PaymentStatusSucceeded
		if status == models.SubscriptionStatusRefunded {
			paymentStatus = models.PaymentStatusRefunded
		} else if status == models.SubscriptionStatusRevoked {
			paymentStatus = models.PaymentStatusCanceled
		}
		return p.repo.UpsertPayment(ctx, models.PaymentEvent{
			Provider:       p.Name(),
			ExternalID:     transaction.TransactionID,
			IdentityID:     identityID,
			SubscriptionID: transaction.OriginalTransactionID,
			PriceID:        product.PlanCode,
			Status:         paymentStatus,
			Kind:           "subscription_cycle",
			AmountMinor:    product.AmountMinor,
			Currency:       product.Currency,
			OccurredAt:     start,
		})
	}
	return nil
}

func (p *applePaymentProvider) ReconcileSubscription(ctx context.Context, stored models.PaymentSubscription) error {
	token, err := p.serverAPIToken(time.Now().UTC())
	if err != nil {
		return err
	}
	requestURL := strings.TrimRight(p.serverAPIURL, "/") + "/inApps/v1/subscriptions/" + url.PathEscape(stored.ExternalSubscriptionID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := p.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("Apple subscription status request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("Apple subscription status response %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var status appleStatusResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&status); err != nil {
		return err
	}
	if status.BundleID != p.bundleID || status.Environment != p.environment ||
		(p.environment == "Production" && status.AppAppleID != p.appAppleID) {
		return ErrPaymentInvalidSignature
	}
	for _, group := range status.Data {
		for _, item := range group.LastTransactions {
			if item.OriginalTransactionID != stored.ExternalSubscriptionID {
				continue
			}
			var renewal *appleRenewalPayload
			if item.SignedRenewalInfo != "" {
				var decoded appleRenewalPayload
				if err := p.verifyJWS(item.SignedRenewalInfo, &decoded); err != nil {
					return ErrPaymentInvalidSignature
				}
				renewal = &decoded
			}
			return p.processAppleTransaction(ctx, item.SignedTransactionInfo, stored.IdentityID, appleStatusNotification(item.Status), renewal)
		}
	}
	return gorm.ErrRecordNotFound
}

func (p *applePaymentProvider) serverAPIToken(now time.Time) (string, error) {
	if p.privateKey == nil || p.issuerID == "" || p.keyID == "" {
		return "", errors.New("Apple App Store Server API credentials are not configured")
	}
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": p.keyID, "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iss": p.issuerID, "iat": now.Unix(), "exp": now.Add(15 * time.Minute).Unix(),
		"aud": "appstoreconnect-v1", "bid": p.bundleID})
	encodedHeader := base64.RawURLEncoding.EncodeToString(header)
	encodedClaims := base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(encodedHeader + "." + encodedClaims))
	r, s, err := ecdsa.Sign(rand.Reader, p.privateKey, digest[:])
	if err != nil {
		return "", err
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return encodedHeader + "." + encodedClaims + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func parseApplePrivateKey(raw []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("invalid Apple private key PEM")
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if key, ok := parsed.(*ecdsa.PrivateKey); ok && key.Curve.Params().BitSize == 256 {
			return key, nil
		}
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil || key.Curve.Params().BitSize != 256 {
		return nil, errors.New("Apple private key must be an ES256 key")
	}
	return key, nil
}

func appleStatusNotification(status int) string {
	switch status {
	case 1:
		return "DID_RENEW"
	case 2:
		return "EXPIRED"
	case 3, 4:
		return "DID_FAIL_TO_RENEW"
	case 5:
		return "REVOKE"
	default:
		return "STATUS_UNKNOWN"
	}
}

func (p *applePaymentProvider) verifyJWS(token string, output any) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("invalid JWS")
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return err
	}
	var header appleJWSHeader
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return err
	}
	if header.Algorithm != "ES256" || len(header.X5C) != 3 {
		return errors.New("unsupported Apple JWS header")
	}
	certificates := make([]*x509.Certificate, 0, len(header.X5C))
	for _, encoded := range header.X5C {
		der, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return err
		}
		certificate, err := x509.ParseCertificate(der)
		if err != nil {
			return err
		}
		certificates = append(certificates, certificate)
	}
	if !hasCertificateExtension(certificates[0], "1.2.840.113635.100.6.11.1") || !hasCertificateExtension(certificates[1], "1.2.840.113635.100.6.2.1") {
		return errors.New("invalid Apple certificate purpose")
	}
	intermediates := x509.NewCertPool()
	intermediates.AddCert(certificates[1])
	effectiveTime := time.Now().UTC()
	now := time.Now().UTC()
	if payloadRaw, decodeErr := base64.RawURLEncoding.DecodeString(parts[1]); decodeErr == nil {
		var dated struct {
			SignedDate int64 `json:"signedDate"`
		}
		if json.Unmarshal(payloadRaw, &dated) == nil && dated.SignedDate > 0 {
			effectiveTime = time.UnixMilli(dated.SignedDate).UTC()
			if effectiveTime.After(now.Add(5 * time.Minute)) {
				return errors.New("Apple signed date is in the future")
			}
		} else {
			return errors.New("Apple signed date is missing")
		}
	} else {
		return errors.New("invalid Apple JWS payload")
	}
	if _, err := certificates[0].Verify(x509.VerifyOptions{
		Roots: p.roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}, CurrentTime: effectiveTime,
	}); err != nil {
		return err
	}
	if p.onlineChecks {
		if err := p.checkOCSP(certificates[0], certificates[1]); err != nil {
			return err
		}
		if err := p.checkOCSP(certificates[1], certificates[2]); err != nil {
			return err
		}
	}
	publicKey, ok := certificates[0].PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve.Params().BitSize != 256 {
		return errors.New("invalid Apple signing key")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != 64 {
		return errors.New("invalid Apple JWS signature")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	if !ecdsa.Verify(publicKey, digest[:], r, s) {
		return errors.New("Apple JWS signature verification failed")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, output)
}

func (p *applePaymentProvider) checkOCSP(certificate, issuer *x509.Certificate) error {
	if len(certificate.OCSPServer) == 0 {
		return errors.New("Apple certificate has no OCSP responder")
	}
	requestBody, err := ocsp.CreateRequest(certificate, issuer, &ocsp.RequestOptions{Hash: crypto.SHA256})
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPost, certificate.OCSPServer[0], bytes.NewReader(requestBody))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/ocsp-request")
	request.Header.Set("Accept", "application/ocsp-response")
	response, err := p.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("Apple OCSP request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Apple OCSP response: %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	status, err := ocsp.ParseResponseForCert(body, certificate, issuer)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	skew := time.Minute
	if status.Status != ocsp.Good || status.ThisUpdate.After(now.Add(skew)) || (!status.NextUpdate.IsZero() && status.NextUpdate.Before(now.Add(-skew))) {
		return errors.New("Apple certificate OCSP status is not good")
	}
	return nil
}

func hasCertificateExtension(certificate *x509.Certificate, expected string) bool {
	for _, extension := range certificate.Extensions {
		if extension.Id.String() == expected {
			return true
		}
	}
	return false
}

func appleSubscriptionStatus(notificationType string, transaction appleTransactionPayload) models.SubscriptionStatus {
	switch strings.ToUpper(notificationType) {
	case "REFUND":
		return models.SubscriptionStatusRefunded
	case "REVOKE":
		return models.SubscriptionStatusRevoked
	case "EXPIRED":
		return models.SubscriptionStatusExpired
	case "DID_FAIL_TO_RENEW", "GRACE_PERIOD_EXPIRED":
		return models.SubscriptionStatusPastDue
	}
	if transaction.RevocationDate > 0 {
		return models.SubscriptionStatusRevoked
	}
	if transaction.ExpiresDate > 0 && !appleMillisecondsTime(transaction.ExpiresDate).After(time.Now().UTC()) {
		return models.SubscriptionStatusExpired
	}
	return models.SubscriptionStatusActive
}

func appleMillisecondsTime(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(value).UTC()
}

func bytesContainsPEM(raw []byte) bool {
	block, _ := pem.Decode(raw)
	return block != nil
}
