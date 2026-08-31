// Package billing implements subscriptions and the paywall via Stripe
// (PRD 007): 7-day cardless trial created at store creation, a single Pro
// plan chosen at conversion (monthly/semestral/annual — no GMV commission),
// access enforcement from the local subscriptions table (webhooks are the
// source of truth).
package billing

import (
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"

	"livecart/apps/api/internal/billing/domain"
	"livecart/apps/api/lib/config"
)

// Plan identifiers — mirror the public pricing (LP) and the DB check.
type Plan string

const (
	PlanPro        Plan = "pro"
	PlanEnterprise Plan = "enterprise"
)

// BillingInterval identifies which of the Pro plan's 3 recurring prices a
// subscription is on. Switching between them is done via the Stripe Customer
// Portal (the 3 prices are grouped on the same product there) — no API
// endpoint is needed for that.
type BillingInterval string

const (
	IntervalMonthly   BillingInterval = "monthly"
	IntervalSemestral BillingInterval = "semestral"
	IntervalAnnual    BillingInterval = "annual"
)

// Subscription statuses persisted locally (subset of Stripe's, plus paused).
const (
	StatusTrialing = "trialing"
	StatusActive   = "active"
	StatusPastDue  = "past_due"
	StatusPaused   = "paused"
	StatusUnpaid   = "unpaid"
	StatusCanceled = "canceled"
)

// TrialDays is the cardless trial length (LP promise: "7 dias grátis").
const TrialDays = 7

// GraceDays keeps access alive after the first failed charge while Stripe
// retries the card.
const GraceDays = 7

// IntervalPrice is one of the Pro plan's recurring prices.
type IntervalPrice struct {
	Cents   int64
	PriceID string
}

// PlanConfig carries the Stripe prices and display data for a plan. There is
// no more per-GMV commission: the fee charged is always 0 for every store.
type PlanConfig struct {
	Plan        Plan
	Name        string
	Prices      map[BillingInterval]IntervalPrice // empty for Enterprise (dashboard-managed)
	SelfService bool                              // Enterprise is dashboard-managed
}

// Plans returns the plan registry with price IDs resolved from env.
func Plans() map[Plan]PlanConfig {
	return map[Plan]PlanConfig{
		PlanPro: {
			Plan: PlanPro, Name: "Pro",
			Prices: map[BillingInterval]IntervalPrice{
				IntervalMonthly:   {Cents: 59700, PriceID: config.StripePriceProMonthly.String()},
				IntervalSemestral: {Cents: 340290, PriceID: config.StripePriceProSemestral.String()},
				IntervalAnnual:    {Cents: 644760, PriceID: config.StripePriceProAnnual.String()},
			},
			SelfService: true,
		},
		PlanEnterprise: {
			Plan: PlanEnterprise, Name: "Enterprise", SelfService: false,
		},
	}
}

// planFromPriceID resolves which plan a Stripe price belongs to (webhooks).
func planFromPriceID(priceID string) Plan {
	if priceID == "" {
		return ""
	}
	for p, cfg := range Plans() {
		for _, ip := range cfg.Prices {
			if ip.PriceID == priceID {
				return p
			}
		}
	}
	return ""
}

// SubscriptionState is the paywall/subscription Response. It doubles as the
// FE/middleware-facing snapshot embedded in the /users/sync payload and returned
// by the billing endpoints (shared read-model — cross-package consumers rely on
// this shape).
type SubscriptionState struct {
	Status            string     `json:"status"`
	Plan              Plan       `json:"plan"`
	TrialEndsAt       *time.Time `json:"trialEndsAt,omitempty"`
	TrialDaysLeft     int        `json:"trialDaysLeft"`
	CurrentPeriodEnd  *time.Time `json:"currentPeriodEnd,omitempty"`
	CancelAtPeriodEnd bool       `json:"cancelAtPeriodEnd"`
	GraceUntil        *time.Time `json:"graceUntil,omitempty"`
	HasPaymentMethod  bool       `json:"hasPaymentMethod"`
	Blocked           bool       `json:"blocked"`
	// Enforced=false: paywall globalmente desativado (PAYWALL_ENABLED) — o
	// estado continua sendo calculado/exibível, mas nada bloqueia e o FE
	// esconde banners de pressão.
	Enforced bool `json:"enforced"`
}

// NewSubscriptionResponse maps a domain Subscription entity to its API response.
// This is the controller's outbound mapper: presentation knows the domain, the
// domain never knows the Response. `enforced` reflects the global paywall kill
// switch — when off, nothing blocks even if the entity would.
func NewSubscriptionResponse(sub *domain.Subscription, enforced bool, now time.Time) SubscriptionState {
	state := SubscriptionState{
		Status:            sub.Status(),
		Plan:              Plan(sub.Plan()),
		TrialEndsAt:       sub.TrialEndsAt(),
		TrialDaysLeft:     sub.TrialDaysLeft(now),
		CurrentPeriodEnd:  sub.CurrentPeriodEnd(),
		CancelAtPeriodEnd: sub.CancelAtPeriodEnd(),
		GraceUntil:        sub.GraceUntil(),
		HasPaymentMethod:  sub.HasPaymentMethod(),
		Enforced:          enforced,
	}
	state.Blocked = enforced && sub.IsBlocked(now)
	return state
}

// ============================================
// Request types (ozzo syntactic gate + ToInput semantic build)
// ============================================

// CreateCheckoutRequest picks the billing interval for the (single) Pro plan.
// There is no plan choice anymore — switching between monthly/semestral/annual
// after conversion is handled by the Stripe Customer Portal.
type CreateCheckoutRequest struct {
	Interval string `json:"interval"`
}

// Validate is the syntactic gate (ozzo): only the 3 known intervals are contractable.
func (r CreateCheckoutRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Interval, validation.Required, validation.In("monthly", "semestral", "annual")),
	)
}

// ToInput builds the usecase input for a store-scoped checkout.
func (r CreateCheckoutRequest) ToInput(storeID string) (CheckoutInput, error) {
	return CheckoutInput{StoreID: storeID, Interval: BillingInterval(r.Interval)}, nil
}

// ============================================
// Service layer - Input types
// ============================================

// CheckoutInput is the usecase input for opening a conversion checkout. The
// plan is always PlanPro (implicit) — only the billing interval is chosen.
type CheckoutInput struct {
	StoreID  string
	Interval BillingInterval
}
