// Package domain holds the billing aggregate: the Subscription entity that
// carries a store's persisted subscription state plus the access-enforcement
// rules (PRD 007 §4/§5). The entity is loaded via Reconstruct from persistence
// (no validation) and exposes immutable getters; the presentation layer maps it
// to a Response, the domain never knows the Response.
package domain

import "time"

// Subscription status constants (subset of Stripe's, plus paused). Persisted
// locally as the access source of truth.
const (
	StatusTrialing = "trialing"
	StatusActive   = "active"
	StatusPastDue  = "past_due"
	StatusPaused   = "paused"
	StatusUnpaid   = "unpaid"
	StatusCanceled = "canceled"
)

// Subscription is the billing aggregate root for a store.
type Subscription struct {
	storeID            string
	status             string
	plan               string
	trialEndsAt        *time.Time
	currentPeriodStart *time.Time
	currentPeriodEnd   *time.Time
	graceUntil         *time.Time
	cancelAtPeriodEnd  bool
	manualOverride     bool
}

// Reconstruct rebuilds a Subscription from persistence data (no validation).
func Reconstruct(
	storeID string,
	status string,
	plan string,
	trialEndsAt *time.Time,
	currentPeriodStart *time.Time,
	currentPeriodEnd *time.Time,
	graceUntil *time.Time,
	cancelAtPeriodEnd bool,
	manualOverride bool,
) *Subscription {
	return &Subscription{
		storeID:            storeID,
		status:             status,
		plan:               plan,
		trialEndsAt:        trialEndsAt,
		currentPeriodStart: currentPeriodStart,
		currentPeriodEnd:   currentPeriodEnd,
		graceUntil:         graceUntil,
		cancelAtPeriodEnd:  cancelAtPeriodEnd,
		manualOverride:     manualOverride,
	}
}

// ============================================
// Getters (immutable access)
// ============================================

func (s *Subscription) StoreID() string                { return s.storeID }
func (s *Subscription) Status() string                 { return s.status }
func (s *Subscription) Plan() string                   { return s.plan }
func (s *Subscription) TrialEndsAt() *time.Time        { return s.trialEndsAt }
func (s *Subscription) CurrentPeriodStart() *time.Time { return s.currentPeriodStart }
func (s *Subscription) CurrentPeriodEnd() *time.Time   { return s.currentPeriodEnd }
func (s *Subscription) GraceUntil() *time.Time         { return s.graceUntil }
func (s *Subscription) CancelAtPeriodEnd() bool        { return s.cancelAtPeriodEnd }
func (s *Subscription) ManualOverride() bool           { return s.manualOverride }

// ============================================
// Business rules
// ============================================

// HasPaymentMethod is a display hint: a converted subscription (active or in
// grace) has a card on file.
func (s *Subscription) HasPaymentMethod() bool {
	return s.status == StatusActive || s.status == StatusPastDue
}

// TrialDaysLeft returns the whole days remaining in the trial (0 outside a
// trialing status or once the trial has lapsed).
func (s *Subscription) TrialDaysLeft(now time.Time) int {
	if s.status != StatusTrialing || s.trialEndsAt == nil {
		return 0
	}
	days := int(s.trialEndsAt.Sub(now).Hours()/24) + 1
	if days < 0 {
		return 0
	}
	return days
}

// IsBlocked computes access denial (PRD 007 §4/§5): manual_override always
// grants access; hard-blocked statuses deny; past_due denies only after the
// grace window; a stale trialing row past its end denies as a safety net for
// delayed webhooks.
func (s *Subscription) IsBlocked(now time.Time) bool {
	if s.manualOverride {
		return false
	}
	switch s.status {
	case StatusPaused, StatusUnpaid, StatusCanceled:
		return true
	case StatusPastDue:
		return s.graceUntil == nil || now.After(*s.graceUntil)
	case StatusTrialing:
		return s.trialEndsAt != nil && now.After(*s.trialEndsAt)
	default:
		return false
	}
}
