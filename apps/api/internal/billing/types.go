// Package billing implements subscriptions and the paywall via Stripe
// (PRD 007): 7-day cardless trial created at store creation, plan chosen at
// conversion, flat monthly price + metered GMV fee, access enforcement from
// the local subscriptions table (webhooks are the source of truth).
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
	PlanStart      Plan = "start"
	PlanGrow       Plan = "grow"
	PlanScale      Plan = "scale"
	PlanEnterprise Plan = "enterprise"
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

// PlanConfig carries the Stripe price pair and display data for a plan.
type PlanConfig struct {
	Plan          Plan
	Name          string
	FlatCents     int64
	GMVBps        int    // basis points (180 = 1,80%)
	FlatPriceID   string // Stripe price (recurring flat)
	MeterPriceID  string // Stripe price (metered, GMV)
	SelfService   bool   // Enterprise is dashboard-managed
}

// Plans returns the plan registry with price IDs resolved from env.
func Plans() map[Plan]PlanConfig {
	return map[Plan]PlanConfig{
		PlanStart: {
			Plan: PlanStart, Name: "Start", FlatCents: 14700, GMVBps: 180,
			FlatPriceID:  config.StripePriceStartFlat.String(),
			MeterPriceID: config.StripePriceStartMetered.String(),
			SelfService:  true,
		},
		PlanGrow: {
			Plan: PlanGrow, Name: "Grow", FlatCents: 29700, GMVBps: 130,
			FlatPriceID:  config.StripePriceGrowFlat.String(),
			MeterPriceID: config.StripePriceGrowMetered.String(),
			SelfService:  true,
		},
		PlanScale: {
			Plan: PlanScale, Name: "Scale", FlatCents: 69700, GMVBps: 100,
			FlatPriceID:  config.StripePriceScaleFlat.String(),
			MeterPriceID: config.StripePriceScaleMetered.String(),
			SelfService:  true,
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
		if cfg.FlatPriceID == priceID || cfg.MeterPriceID == priceID {
			return p
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

// CreateCheckoutRequest picks the plan being contracted.
type CreateCheckoutRequest struct {
	Plan string `json:"plan"`
}

// Validate is the syntactic gate (ozzo): only self-service plans are contractable.
func (r CreateCheckoutRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Plan, validation.Required, validation.In("start", "grow", "scale")),
	)
}

// ToInput builds the usecase input for a store-scoped checkout.
func (r CreateCheckoutRequest) ToInput(storeID string) (CheckoutInput, error) {
	return CheckoutInput{StoreID: storeID, Plan: Plan(r.Plan)}, nil
}

// ChangePlanRequest picks the target plan.
type ChangePlanRequest struct {
	Plan string `json:"plan"`
}

// Validate is the syntactic gate (ozzo): only self-service plans are targetable.
func (r ChangePlanRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Plan, validation.Required, validation.In("start", "grow", "scale")),
	)
}

// ToInput builds the usecase input for a store-scoped plan change.
func (r ChangePlanRequest) ToInput(storeID string) (ChangePlanInput, error) {
	return ChangePlanInput{StoreID: storeID, Plan: Plan(r.Plan)}, nil
}

// ============================================
// Service layer - Input types
// ============================================

// CheckoutInput is the usecase input for opening a conversion checkout.
type CheckoutInput struct {
	StoreID string
	Plan    Plan
}

// ChangePlanInput is the usecase input for an upgrade/downgrade.
type ChangePlanInput struct {
	StoreID string
	Plan    Plan
}
