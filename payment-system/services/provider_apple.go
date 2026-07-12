package services

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bartek5186/procyon-modules/payment-system/models"
	"go.uber.org/zap"
)

const applePaymentProviderName = "apple"

type applePaymentConfig struct {
	BundleID string `json:"bundle_id"`
	RootCA   string `json:"root_ca"`
}

type applePaymentProvider struct {
	repo     paymentRepository
	logger   *zap.Logger
	bundleID string
	roots    *x509.CertPool
}

type appleNotificationEnvelope struct {
	SignedPayload string `json:"signedPayload"`
}

type appleNotificationPayload struct {
	NotificationType string `json:"notificationType"`
	Subtype          string `json:"subtype"`
	NotificationUUID string `json:"notificationUUID"`
	Data             struct {
		SignedTransactionInfo string `json:"signedTransactionInfo"`
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
}

type appleJWSHeader struct {
	Algorithm string   `json:"alg"`
	X5C       []string `json:"x5c"`
}

func init() {
	registerPaymentProviderFactory(newApplePaymentProvider)
}

func newApplePaymentProvider(repo paymentRepository, logger *zap.Logger) (PaymentProvider, bool, error) {
	configPath := strings.TrimSpace(os.Getenv("APPLE_APP_STORE_CONFIG_FILE"))
	if configPath == "" {
		return nil, false, nil
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, false, err
	}
	var config applePaymentConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, false, fmt.Errorf("parse Apple payment config: %w", err)
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
	return &applePaymentProvider{repo: repo, logger: logger, bundleID: strings.TrimSpace(config.BundleID), roots: roots}, true, nil
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
	if notification.NotificationUUID == "" {
		return fmt.Errorf("Apple notification UUID is missing")
	}
	claimed, err := p.repo.BeginWebhook(ctx, p.Name(), notification.NotificationUUID, notification.NotificationType)
	if err != nil || !claimed {
		return err
	}
	processErr := p.processAppleTransaction(ctx, notification.Data.SignedTransactionInfo, "", notification.NotificationType)
	if finishErr := p.repo.FinishWebhook(ctx, p.Name(), notification.NotificationUUID, processErr); finishErr != nil && processErr == nil {
		return finishErr
	}
	return processErr
}

func (p *applePaymentProvider) VerifyStorePurchase(ctx context.Context, identityID string, input models.PaymentStoreVerificationInput) error {
	if strings.TrimSpace(input.SignedPayload) == "" {
		return fmt.Errorf("signed_payload is required for Apple verification")
	}
	return p.processAppleTransaction(ctx, input.SignedPayload, identityID, "PURCHASE_VERIFIED")
}

func (p *applePaymentProvider) processAppleTransaction(ctx context.Context, signedTransaction, fallbackIdentity, notificationType string) error {
	if strings.TrimSpace(signedTransaction) == "" {
		return fmt.Errorf("Apple signed transaction is missing")
	}
	var transaction appleTransactionPayload
	if err := p.verifyJWS(signedTransaction, &transaction); err != nil {
		return ErrPaymentInvalidSignature
	}
	if p.bundleID != "" && transaction.BundleID != p.bundleID {
		return ErrPaymentInvalidSignature
	}
	identityID := strings.TrimSpace(transaction.AppAccountToken)
	if identityID == "" {
		identityID = strings.TrimSpace(fallbackIdentity)
	}
	if identityID == "" || transaction.OriginalTransactionID == "" {
		return fmt.Errorf("Apple transaction cannot be associated with an identity")
	}
	status := appleSubscriptionStatus(notificationType, transaction)
	start := appleMillisecondsTime(transaction.PurchaseDate)
	end := appleMillisecondsTime(transaction.ExpiresDate)
	if err := p.repo.UpsertSubscription(ctx, models.PaymentSubscription{
		Provider:               p.Name(),
		ExternalSubscriptionID: transaction.OriginalTransactionID,
		IdentityID:             identityID,
		PlanCode:               transaction.ProductID,
		Status:                 status,
		CurrentPeriodStart:     start,
		CurrentPeriodEnd:       end,
	}); err != nil {
		return err
	}
	if transaction.TransactionID != "" {
		return p.repo.UpsertPayment(ctx, models.PaymentEvent{
			Provider:       p.Name(),
			ExternalID:     transaction.TransactionID,
			IdentityID:     identityID,
			SubscriptionID: transaction.OriginalTransactionID,
			PriceID:        transaction.ProductID,
			Status:         models.PaymentStatusSucceeded,
			Kind:           "subscription_cycle",
			OccurredAt:     start,
		})
	}
	return nil
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
	if header.Algorithm != "ES256" || len(header.X5C) == 0 {
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
	intermediates := x509.NewCertPool()
	for _, certificate := range certificates[1:] {
		intermediates.AddCert(certificate)
	}
	if _, err := certificates[0].Verify(x509.VerifyOptions{
		Roots: p.roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return err
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
