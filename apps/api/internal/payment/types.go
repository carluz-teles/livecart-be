package payment

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// PaymentStatus represents the possible states of a payment
type PaymentStatus string

const (
	PaymentStatusPending   PaymentStatus = "pending"
	PaymentStatusApproved  PaymentStatus = "approved"
	PaymentStatusRejected  PaymentStatus = "rejected"
	PaymentStatusRefunded  PaymentStatus = "refunded"
	PaymentStatusCancelled PaymentStatus = "cancelled"
)

// Payment represents a payment attempt for a cart
type Payment struct {
	ID                uuid.UUID       `json:"id"`
	CartID            uuid.UUID       `json:"cartId"`
	IntegrationID     *uuid.UUID      `json:"integrationId,omitempty"`
	ExternalPaymentID *string         `json:"externalPaymentId,omitempty"`
	Provider          string          `json:"provider"`
	AmountCents       int64           `json:"amountCents"`
	Currency          string          `json:"currency"`
	Method            *string         `json:"method,omitempty"`
	Status            PaymentStatus   `json:"status"`
	StatusDetail      *string         `json:"statusDetail,omitempty"`
	ProviderResponse  json.RawMessage `json:"providerResponse,omitempty"`
	CreatedAt         time.Time       `json:"createdAt"`
	UpdatedAt         time.Time       `json:"updatedAt"`
	PaidAt            *time.Time      `json:"paidAt,omitempty"`
	IdempotencyKey    *string         `json:"idempotencyKey,omitempty"`
}

// CreatePaymentInput is the input for creating a new payment
type CreatePaymentInput struct {
	CartID            uuid.UUID
	IntegrationID     *uuid.UUID
	ExternalPaymentID *string
	Provider          string
	AmountCents       int64
	Currency          string
	Method            *string
	Status            PaymentStatus
	StatusDetail      *string
	ProviderResponse  json.RawMessage
	IdempotencyKey    *string
}

// UpdatePaymentStatusInput is the input for updating payment status
type UpdatePaymentStatusInput struct {
	Status       PaymentStatus
	StatusDetail *string
	PaidAt       *time.Time
}

// UpdatePaymentByExternalIDInput is used by webhooks to update payment
type UpdatePaymentByExternalIDInput struct {
	ExternalPaymentID string
	Status            PaymentStatus
	StatusDetail      *string
	PaidAt            *time.Time
	Method            *string
	ProviderResponse  json.RawMessage
}

// PaymentStats represents aggregated payment statistics
type PaymentStats struct {
	TotalPayments       int   `json:"totalPayments"`
	ApprovedPayments    int   `json:"approvedPayments"`
	TotalApprovedAmount int64 `json:"totalApprovedAmount"`
}

// PaymentStatusCount represents count per status
type PaymentStatusCount struct {
	Status string `json:"status"`
	Count  int    `json:"count"`
}
