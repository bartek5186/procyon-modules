package models

type PaymentCheckoutInput struct {
	Provider   string `json:"provider" validate:"required"`
	PriceID    string `json:"price_id" validate:"required"`
	SuccessURL string `json:"success_url" validate:"required,url"`
	CancelURL  string `json:"cancel_url" validate:"required,url"`
}

type PaymentSubscriptionCheckoutInput struct {
	Provider   string `json:"provider" validate:"required"`
	PriceID    string `json:"price_id" validate:"required"`
	SuccessURL string `json:"success_url" validate:"required,url"`
	CancelURL  string `json:"cancel_url" validate:"required,url"`
}

type PaymentCancelSubscriptionInput struct {
	Provider       string `json:"provider" validate:"required"`
	SubscriptionID string `json:"subscription_id" validate:"required"`
}

type PaymentPortalInput struct {
	Provider  string `json:"provider" validate:"required"`
	ReturnURL string `json:"return_url" validate:"required,url"`
}

type PaymentStoreVerificationInput struct {
	PackageName   string `json:"package_name"`
	ProductID     string `json:"product_id" validate:"required"`
	PurchaseToken string `json:"purchase_token" validate:"required"`
	SignedPayload string `json:"signed_payload"`
}
