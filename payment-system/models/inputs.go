package models

type PaymentCheckoutInput struct {
	Provider       string `json:"provider" validate:"required,max=32"`
	PriceID        string `json:"price_id" validate:"required,max=191"`
	SuccessURL     string `json:"success_url" validate:"required,url,max=2048"`
	CancelURL      string `json:"cancel_url" validate:"required,url,max=2048"`
	IdempotencyKey string `json:"-" validate:"required,max=255"`
}

type PaymentSubscriptionCheckoutInput struct {
	Provider       string `json:"provider" validate:"required,max=32"`
	PriceID        string `json:"price_id" validate:"required,max=191"`
	SuccessURL     string `json:"success_url" validate:"required,url,max=2048"`
	CancelURL      string `json:"cancel_url" validate:"required,url,max=2048"`
	IdempotencyKey string `json:"-" validate:"required,max=255"`
}

type PaymentCancelSubscriptionInput struct {
	Provider          string `json:"provider" validate:"required,max=32"`
	SubscriptionID    string `json:"subscription_id" validate:"required,max=191"`
	CancelAtPeriodEnd bool   `json:"cancel_at_period_end"`
}

type PaymentPortalInput struct {
	Provider  string `json:"provider" validate:"required,max=32"`
	ReturnURL string `json:"return_url" validate:"required,url,max=2048"`
}

type PaymentStoreVerificationInput struct {
	PackageName   string `json:"package_name,omitempty" validate:"omitempty,max=255"`
	ProductID     string `json:"product_id" validate:"required_without=SignedPayload,max=191"`
	PurchaseToken string `json:"purchase_token" validate:"required_without=SignedPayload,max=4096"`
	SignedPayload string `json:"signed_payload" validate:"required_without=PurchaseToken,max=262144"`
}

type PaymentHistoryQuery struct {
	Limit  int `query:"limit" validate:"omitempty,min=1,max=100"`
	Offset int `query:"offset" validate:"omitempty,min=0"`
}

type PaymentSubscriptionQuery struct {
	Limit  int `query:"limit" validate:"omitempty,min=1,max=100"`
	Offset int `query:"offset" validate:"omitempty,min=0"`
}

type PaymentEntitlementQuery struct {
	PlanCode string `query:"plan_code" validate:"omitempty,max=191"`
}

type PaymentWebhookListQuery struct {
	Limit  int `query:"limit" validate:"omitempty,min=1,max=100"`
	Offset int `query:"offset" validate:"omitempty,min=0"`
}

type PaymentWebhookRetryInput struct {
	Provider string `json:"provider" validate:"required,max=32"`
	EventID  string `json:"event_id" validate:"required,max=191"`
}
