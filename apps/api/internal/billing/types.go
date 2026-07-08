// Package billing implements subscriptions and the paywall via Stripe
// (PRD 007): 7-day cardless trial created at store creation, plan chosen at
// conversion, flat monthly price + metered GMV fee, access enforcement from
// the local subscriptions table (webhooks are the source of truth).
package billing

import (
	"time"

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

// SubscriptionState is the FE/middleware-facing snapshot. Embedded in the
// /users/sync payload and returned by the billing endpoints.
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
}

// blocked computes access denial (PRD 007 §4/§5): manual_override always
// grants access; hard-blocked statuses deny; past_due denies only after the
// grace window; a stale trialing row past its end denies as a safety net for
// delayed webhooks.
func blocked(status string, manualOverride bool, trialEndsAt, graceUntil *time.Time, now time.Time) bool {
	if manualOverride {
		return false
	}
	switch status {
	case StatusPaused, StatusUnpaid, StatusCanceled:
		return true
	case StatusPastDue:
		return graceUntil == nil || now.After(*graceUntil)
	case StatusTrialing:
		return trialEndsAt != nil && now.After(*trialEndsAt)
	default:
		return false
	}
}
