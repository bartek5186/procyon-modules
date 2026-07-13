package models

import "errors"

var (
	ErrPaymentOwnershipConflict = errors.New("payment resource belongs to another identity")
	ErrPaymentProductRejected   = errors.New("payment product is not allowed")
	ErrPaymentIdempotencyKey    = errors.New("valid idempotency key is required")
	ErrPaymentWebhookLeaseLost  = errors.New("payment webhook processing lease was lost")
)
