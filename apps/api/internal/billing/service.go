package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/billing/domain"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/logger"
)

// TrialReminderScheduler arms an ETA task (trial.ending_soon) at
// trial_ends_at - N days, so the merchant is reminded precisely instead of
// polled. Implemented over the asynq client in main.go (billing must not import
// events at the client level, so this stays a local interface).
type TrialReminderScheduler interface {
	ScheduleTrialEndingSoon(ctx context.Context, storeID string, at time.Time) error
}

// TrialReminderLeadDays is how far before trial_ends_at the reminder fires.
const TrialReminderLeadDays = 2

// Service owns the subscription lifecycle. The local table is the access
// source of truth; Stripe webhooks keep it in sync.
type Service struct {
	queries        *sqlc.Queries
	stripe         stripeGateway // nil when STRIPE_SECRET_KEY is absent (local-only trials)
	trialScheduler TrialReminderScheduler
	logger         *zap.Logger
}

// SetTrialReminderScheduler wires the ETA scheduler for the trial-ending
// reminder (optional — unset in tests / when the event pipeline is down).
func (s *Service) SetTrialReminderScheduler(sch TrialReminderScheduler) { s.trialScheduler = sch }

// NewService builds the billing service.
func NewService(queries *sqlc.Queries, logger *zap.Logger) *Service {
	// NewStripeClient returns nil when the key is empty; assigning nil *StripeClient
	// directly to the stripeGateway interface would produce a non-nil interface with
	// nil value, breaking the `s.stripe != nil` guard. Assign through a typed local.
	var stripe stripeGateway
	if sc := NewStripeClient(config.StripeSecretKey.String(), logger); sc != nil {
		stripe = sc
	}
	return &Service{
		queries: queries,
		stripe:  stripe,
		logger:  logger.Named("billing"),
	}
}

// =============================================================================
// TRIAL PROVISIONING (store creation + lazy on sync)
// =============================================================================

// EnsureTrialSubscription guarantees the store has a subscription row (local
// trial) and, best-effort, the mirroring Stripe customer + trial
// subscription. Idempotent — safe to call on every /users/sync.
func (s *Service) EnsureTrialSubscription(ctx context.Context, storeID, storeName, email string) (*SubscriptionState, error) {
	sid, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	trialEnd := time.Now().Add(TrialDays * 24 * time.Hour)
	row, err := s.queries.EnsureTrialSubscription(ctx, sqlc.EnsureTrialSubscriptionParams{
		StoreID:     sid,
		TrialEndsAt: pgtype.Timestamptz{Time: trialEnd, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("ensuring local trial: %w", err)
	}

	// Arm the trial-ending reminder at trial_ends_at - N days (ETA task). Dedup
	// by store so the repeated /users/sync calls don't re-arm; skip when the
	// reminder window is already in the past. Best-effort — no access impact.
	if s.trialScheduler != nil && row.TrialEndsAt.Valid {
		remindAt := row.TrialEndsAt.Time.Add(-TrialReminderLeadDays * 24 * time.Hour)
		if remindAt.After(time.Now()) {
			if err := s.trialScheduler.ScheduleTrialEndingSoon(ctx, storeID, remindAt); err != nil {
				logger.From(ctx, s.logger).Warn("failed to schedule trial-ending reminder",
					zap.String("store_id", storeID), zap.Error(err))
			}
		}
	}

	// Mirror on Stripe when configured and not yet linked. Failures are
	// non-fatal: the local trial grants access and the next call retries.
	if s.stripe != nil && !row.StripeSubscriptionID.Valid {
		if err := s.provisionStripe(ctx, &row, storeID, storeName, email); err != nil {
			logger.From(ctx, s.logger).Warn("stripe trial provisioning pending",
				zap.String("store_id", storeID),
				zap.Error(err),
			)
		}
	}

	state := s.toState(&row)
	return &state, nil
}

// RunTrialEndingSoon is the trial.ending_soon ETA-task handler. Guard-first: if
// the store already converted/canceled by the time it fires, it is a no-op;
// otherwise it is the "trial ending soon" signal (logged now; phase 08 hooks the
// merchant reminder here). It does NOT re-emit trial.ending_soon — the scheduled
// delivery IS the event, so re-emitting would loop.
func (s *Service) RunTrialEndingSoon(ctx context.Context, storeID string) error {
	sid, err := parseUUID(storeID)
	if err != nil {
		return nil
	}
	row, err := s.queries.GetSubscriptionByStoreID(ctx, sid)
	if err != nil {
		return nil // subscription gone
	}
	ctx = logger.WithStore(ctx, storeID, "")
	if row.Status != StatusTrialing {
		logger.From(ctx, s.logger).Debug("trial-ending reminder skipped: no longer trialing",
			zap.String("status", row.Status))
		return nil
	}
	logger.From(ctx, s.logger).Info("trial ending soon",
		zap.Time("trial_ends_at", row.TrialEndsAt.Time))
	return nil
}

func (s *Service) provisionStripe(ctx context.Context, row *sqlc.Subscription, storeID, storeName, email string) error {
	customerID := row.StripeCustomerID.String
	if customerID == "" {
		customer, err := s.stripe.CreateCustomer(ctx, storeID, storeName, email)
		if err != nil {
			return err
		}
		customerID = customer.ID
	}

	// Trial-local model: onboarding provisions ONLY the Stripe customer. The
	// paid subscription is created later by the subscription-mode Checkout at
	// conversion (see CreateCheckoutSession), so the customer can redeem a
	// promo code natively and never sees the metered per-unit micro-price.
	// Legacy stores that already carry a Stripe trial subscription are not
	// re-provisioned here (guarded by !StripeSubscriptionID.Valid at the call
	// site) and drain through the setup-mode conversion path.
	updated, err := s.queries.SetSubscriptionStripeRefs(ctx, sqlc.SetSubscriptionStripeRefsParams{
		StoreID:          row.StoreID,
		StripeCustomerID: pgtype.Text{String: customerID, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("persisting stripe customer: %w", err)
	}
	*row = updated

	logger.From(ctx, s.logger).Info("stripe customer provisioned (trial-local)",
		zap.String("store_id", storeID),
		zap.String("customer", customerID),
	)
	return nil
}

// =============================================================================
// STATE (middleware / sync / FE)
// =============================================================================

// GetState returns the access snapshot for a store. Missing row (legacy
// store) returns nil — caller decides whether to lazily ensure. Shared
// read-model used by the paywall middleware and the /users/sync payload.
func (s *Service) GetState(ctx context.Context, storeID string) (*SubscriptionState, error) {
	sub, err := s.GetSubscription(ctx, storeID)
	if err != nil {
		return nil, err
	}
	if sub == nil {
		return nil, nil
	}
	state := NewSubscriptionResponse(sub, config.PaywallEnabled.Bool(), time.Now())
	return &state, nil
}

// GetSubscription loads the Subscription domain entity for a store. Missing row
// (legacy store) returns (nil, nil).
func (s *Service) GetSubscription(ctx context.Context, storeID string) (*domain.Subscription, error) {
	sid, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}
	row, err := s.queries.GetSubscriptionByStoreID(ctx, sid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return rowToSubscription(&row), nil
}

func (s *Service) toState(row *sqlc.Subscription) SubscriptionState {
	return NewSubscriptionResponse(rowToSubscription(row), config.PaywallEnabled.Bool(), time.Now())
}

// rowToSubscription maps a sqlc Subscription row to the domain entity.
func rowToSubscription(row *sqlc.Subscription) *domain.Subscription {
	return domain.Reconstruct(
		uuidToString(row.StoreID),
		row.Status,
		row.Plan,
		timePtr(row.TrialEndsAt),
		timePtr(row.CurrentPeriodStart),
		timePtr(row.CurrentPeriodEnd),
		timePtr(row.GraceUntil),
		row.CancelAtPeriodEnd,
		row.ManualOverride,
	)
}

func timePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

// =============================================================================
// WEBHOOK PROCESSING
// =============================================================================

// DispatchWebhookEvent is the thin webhook edge (billing L1 of the event
// choreography): it emits a subscription.process COMMAND carrying the raw Stripe
// event to the transactional outbox instead of running ProcessWebhookEvent
// synchronously in the request. The command consumer (main.newApp) runs the
// guarded processing with asynq retry + dead-letter, and the outbox makes it
// crash-durable. Dedup by the Stripe event id so at-least-once webhook
// redelivery collapses to a single command (ProcessWebhookEvent is idempotent
// besides). Emitting unconditionally keeps this edge free of domain knowledge —
// the consumer's switch no-ops the event types billing doesn't handle.
func (s *Service) DispatchWebhookEvent(ctx context.Context, event *StripeEvent) error {
	return events.EmitInternal(ctx, s.queries, events.SubscriptionProcess,
		"subscription.process:"+event.ID, event)
}

// ProcessWebhookEvent applies a verified Stripe event to the local table. It is
// the billing L2 command executor: run by the subscription.process consumer,
// guarded + retriable, and it emits the canonical subscription.* facts.
func (s *Service) ProcessWebhookEvent(ctx context.Context, event *StripeEvent) error {
	switch event.Type {
	case "customer.subscription.created",
		"customer.subscription.updated",
		"customer.subscription.deleted",
		"customer.subscription.paused",
		"customer.subscription.resumed":
		var sub StripeSubscription
		if err := json.Unmarshal(event.Data.Object, &sub); err != nil {
			return fmt.Errorf("parsing subscription object: %w", err)
		}
		return s.applySubscription(ctx, &sub, event.Type)

	case "checkout.session.completed":
		var session CheckoutSession
		if err := json.Unmarshal(event.Data.Object, &session); err != nil {
			return fmt.Errorf("parsing checkout session: %w", err)
		}
		switch session.Mode {
		case "setup":
			return s.completeConversion(ctx, &session)
		case "subscription":
			return s.completeSubscriptionConversion(ctx, &session)
		default:
			return nil
		}

	case "invoice.created":
		// Renewal invoice just drafted — inject the accrued GMV commission as an
		// invoice item before it finalizes (billed on the same charge).
		var inv StripeInvoice
		if err := json.Unmarshal(event.Data.Object, &inv); err != nil {
			return fmt.Errorf("parsing invoice: %w", err)
		}
		return s.OnSubscriptionCycleInvoice(ctx, &inv)

	case "invoice.payment_failed":
		// Grace handling rides the subscription.updated → past_due event;
		// logged here for the audit trail.
		logger.From(ctx, s.logger).Warn("stripe invoice payment failed", zap.String("event", event.ID))
		return nil

	default:
		logger.From(ctx, s.logger).Debug("stripe event ignored", zap.String("type", event.Type))
		return nil
	}
}

func (s *Service) applySubscription(ctx context.Context, sub *StripeSubscription, eventType string) error {
	status := mapStripeStatus(sub.Status, eventType)

	// Resolve plan from the flat price on the subscription items; when the
	// items don't map (e.g. Enterprise custom prices), keep the stored plan.
	plan := ""
	for _, item := range sub.Items.Data {
		if p := planFromPriceID(item.Price.ID); p != "" {
			plan = string(p)
			break
		}
	}
	if plan == "" {
		if current, err := s.queries.GetSubscriptionByStripeSubID(ctx, pgtype.Text{String: sub.ID, Valid: true}); err == nil {
			plan = current.Plan
		} else {
			plan = string(PlanPro)
		}
	}

	var graceUntil pgtype.Timestamptz
	if status == StatusPastDue {
		graceUntil = pgtype.Timestamptz{Time: time.Now().Add(GraceDays * 24 * time.Hour), Valid: true}
	}

	row, err := s.queries.UpdateSubscriptionFromStripe(ctx, sqlc.UpdateSubscriptionFromStripeParams{
		StripeSubscriptionID: pgtype.Text{String: sub.ID, Valid: true},
		Status:               status,
		Plan:                 plan,
		TrialEndsAt:          unixToTimestamptz(sub.TrialEnd),
		CurrentPeriodStart:   unixToTimestamptz(sub.PeriodStart()),
		CurrentPeriodEnd:     unixToTimestamptz(sub.PeriodEnd()),
		// The Customer Portal schedules end-of-cycle cancellation via cancel_at
		// (a timestamp), NOT cancel_at_period_end — so treat either as "canceling".
		CancelAtPeriodEnd: sub.CancelAtPeriodEnd || sub.CancelAt > 0,
		GraceUntil:        graceUntil,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			logger.From(ctx, s.logger).Warn("webhook for unknown subscription",
				zap.String("subscription", sub.ID),
			)
			return nil
		}
		return fmt.Errorf("applying subscription update: %w", err)
	}

	// Store resolvido a partir da subscription do evento — enriquece o ctx
	// para os logs seguintes do fluxo.
	storeID := uuidToString(row.StoreID)
	ctx = logger.WithStore(ctx, storeID, "")
	logger.From(ctx, s.logger).Info("subscription updated from stripe",
		zap.String("status", status),
		zap.String("plan", row.Plan),
	)

	// Group J: emit the canonical subscription lifecycle fact. Best-effort — the
	// webhook must ACK. Dedup by (sub, status, period) so Stripe's repeated
	// updated webhooks for the same state collapse to one downstream signal.
	if name, ok := subscriptionEventName(status, eventType); ok {
		_ = events.EmitInternal(ctx, s.queries, name,
			string(name)+":"+sub.ID+":"+fmt.Sprint(sub.PeriodEnd()), struct {
				StoreID        string `json:"store_id"`
				SubscriptionID string `json:"subscription_id"`
				Plan           string `json:"plan"`
				Status         string `json:"status"`
			}{storeID, sub.ID, row.Plan, status})
	}
	return nil
}

// subscriptionEventName maps the resolved local status (+ the Stripe event type)
// to its canonical group-J event. trialing/unknown emit nothing (trial creation
// is trial.started; a trialing webhook update is noise).
func subscriptionEventName(status, eventType string) (events.Name, bool) {
	if eventType == "customer.subscription.resumed" {
		return events.SubscriptionResumed, true
	}
	switch status {
	case StatusActive:
		return events.SubscriptionActivated, true
	case StatusPastDue:
		return events.SubscriptionPastDue, true
	case StatusPaused:
		return events.SubscriptionPaused, true
	case StatusCanceled:
		return events.SubscriptionCanceled, true
	case StatusUnpaid:
		return events.SubscriptionGraceExpired, true
	default:
		return "", false
	}
}

// mapStripeStatus converts Stripe statuses to our local set.
func mapStripeStatus(stripeStatus, eventType string) string {
	if eventType == "customer.subscription.deleted" {
		return StatusCanceled
	}
	switch stripeStatus {
	case "trialing":
		return StatusTrialing
	case "active":
		return StatusActive
	case "past_due":
		return StatusPastDue
	case "paused":
		return StatusPaused
	case "canceled":
		return StatusCanceled
	case "unpaid", "incomplete", "incomplete_expired":
		return StatusUnpaid
	default:
		return StatusUnpaid
	}
}

// =============================================================================
// HELPERS
// =============================================================================

func parseUUID(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		return u, fmt.Errorf("invalid uuid %q: %w", s, err)
	}
	return u, nil
}

func uuidToString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func unixToTimestamptz(ts int64) pgtype.Timestamptz {
	if ts == 0 {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: time.Unix(ts, 0), Valid: true}
}

// =============================================================================
// CONVERSION (trial → active) — PRD 007
// =============================================================================

// CreateConversionCheckout opens a hosted Stripe Checkout (setup mode) where
// the merchant picks... rather: the plan was picked in our UI; the session
// collects the card. Conversion completes on the webhook.
func (s *Service) CreateConversionCheckout(ctx context.Context, input CheckoutInput) (string, error) {
	storeID := input.StoreID
	cfg := Plans()[PlanPro]
	price, ok := cfg.Prices[input.Interval]
	if !ok || price.PriceID == "" {
		return "", fmt.Errorf("intervalo de cobrança inválido: %s", input.Interval)
	}
	if s.stripe == nil {
		return "", fmt.Errorf("billing não configurado (STRIPE_SECRET_KEY ausente)")
	}

	// Garante customer+subscription (lazy — cobre lojas legadas).
	if _, err := s.EnsureTrialSubscription(ctx, storeID, "", ""); err != nil {
		return "", err
	}
	sid, err := parseUUID(storeID)
	if err != nil {
		return "", err
	}
	row, err := s.queries.GetSubscriptionByStoreID(ctx, sid)
	if err != nil {
		return "", fmt.Errorf("loading subscription: %w", err)
	}
	if !row.StripeCustomerID.Valid {
		return "", fmt.Errorf("cliente Stripe ainda não provisionado — tente novamente em instantes")
	}

	frontend := strings.TrimRight(config.FrontendURL.String(), "/")
	successURL := frontend + "/settings/billing?billing=success"
	cancelURL := frontend + "/settings/billing?billing=cancelled"

	// Coexistence by state: legacy stores that still carry a Stripe trial
	// subscription convert through the setup-mode Checkout (card only) + API
	// activation. Trial-local stores (no Stripe subscription yet) convert
	// through a subscription-mode Checkout that creates the paid subscription
	// directly and exposes Stripe's native promo field. No GMV fee to disclose
	// anymore — the commission was eliminated for everyone.
	var session *CheckoutSession
	if row.StripeSubscriptionID.Valid {
		session, err = s.stripe.CreateSetupCheckoutSession(ctx,
			row.StripeCustomerID.String, successURL, cancelURL,
			map[string]string{
				"store_id":        storeID,
				"plan":            string(PlanPro),
				"interval":        string(input.Interval),
				"subscription_id": row.StripeSubscriptionID.String,
			},
		)
	} else {
		session, err = s.stripe.CreateSubscriptionCheckoutSession(ctx,
			row.StripeCustomerID.String, price.PriceID,
			successURL, cancelURL,
			map[string]string{
				"store_id": storeID,
				"plan":     string(PlanPro),
				"interval": string(input.Interval),
			},
		)
	}
	if err != nil {
		return "", err
	}

	return session.URL, nil
}

// completeConversion runs on checkout.session.completed (setup mode): grabs
// the collected card, activates the chosen plan and syncs the local row.
func (s *Service) completeConversion(ctx context.Context, session *CheckoutSession) error {
	subID := session.Metadata["subscription_id"]
	if subID == "" {
		logger.From(ctx, s.logger).Debug("checkout session without conversion metadata — ignored",
			zap.String("session", session.ID))
		return nil
	}
	// Store resolvido a partir do metadata da session — enriquece o ctx para
	// os logs seguintes do fluxo.
	ctx = logger.WithStore(ctx, session.Metadata["store_id"], "")
	cfg := Plans()[PlanPro]

	paymentMethod, err := s.stripe.GetSetupIntentPaymentMethod(ctx, session.SetupIntent)
	if err != nil {
		return err
	}
	if paymentMethod == "" {
		return fmt.Errorf("setup intent %s has no payment method", session.SetupIntent)
	}

	sub, err := s.stripe.GetSubscription(ctx, subID)
	if err != nil {
		return err
	}
	activated, err := s.stripe.ActivateSubscription(ctx, sub, cfg, paymentMethod)
	if err != nil {
		return err
	}

	logger.From(ctx, s.logger).Info("subscription converted",
		zap.String("plan", string(PlanPro)),
		zap.String("interval", session.Metadata["interval"]),
		zap.String("status", activated.Status),
	)
	// Sync imediato (o customer.subscription.updated também chega e é idempotente).
	return s.applySubscription(ctx, activated, "conversion")
}

// completeSubscriptionConversion runs on checkout.session.completed in
// subscription mode (trial-local): the paid subscription was just created with
// the single Pro price for the chosen interval. Links it to the store,
// persists the interval and syncs the local row. Idempotent —
// SetSubscriptionStripeRefs tolerates webhook redelivery.
func (s *Service) completeSubscriptionConversion(ctx context.Context, session *CheckoutSession) error {
	if session.Subscription == "" {
		logger.From(ctx, s.logger).Debug("subscription checkout without conversion metadata — ignored",
			zap.String("session", session.ID))
		return nil
	}
	ctx = logger.WithStore(ctx, session.Metadata["store_id"], "")
	interval := session.Metadata["interval"]

	sub, err := s.stripe.GetSubscription(ctx, session.Subscription)
	if err != nil {
		return err
	}

	// Link the freshly created subscription to the store and persist the
	// chosen billing interval (defaults to monthly if unset/unknown).
	if storeID := session.Metadata["store_id"]; storeID != "" {
		if sid, perr := parseUUID(storeID); perr == nil {
			if _, serr := s.queries.SetSubscriptionStripeRefs(ctx, sqlc.SetSubscriptionStripeRefsParams{
				StoreID:              sid,
				StripeCustomerID:     pgtype.Text{String: session.Customer, Valid: session.Customer != ""},
				StripeSubscriptionID: pgtype.Text{String: sub.ID, Valid: true},
			}); serr != nil {
				return fmt.Errorf("linking subscription: %w", serr)
			}
			if interval == "" {
				interval = string(IntervalMonthly)
			}
			if serr := s.queries.SetSubscriptionInterval(ctx, sqlc.SetSubscriptionIntervalParams{
				StoreID:         sid,
				BillingInterval: interval,
			}); serr != nil {
				logger.From(ctx, s.logger).Warn("failed to persist billing interval",
					zap.String("interval", interval), zap.Error(serr))
			}
		}
	}

	logger.From(ctx, s.logger).Info("subscription converted (trial-local)",
		zap.String("plan", string(PlanPro)),
		zap.String("interval", interval),
		zap.String("subscription", sub.ID),
		zap.String("status", sub.Status),
	)
	return s.applySubscription(ctx, sub, "conversion")
}

// CreateTaxaPromo registers a commission (taxa) discount for a store, consumed
// one cycle at a time by OnSubscriptionCycleInvoice. This is the taxa side of a
// full-cycle promo; the mensalidade side is a native Stripe coupon (e.g.
// CANTODAART). discountBps 5000 = 50%.
func (s *Service) CreateTaxaPromo(ctx context.Context, storeID string, discountBps, cycles int, code, description string) error {
	sid, err := parseUUID(storeID)
	if err != nil {
		return err
	}
	if _, err := s.queries.InsertTaxaPromo(ctx, sqlc.InsertTaxaPromoParams{
		StoreID:         sid,
		DiscountBps:     int32(discountBps),
		CyclesRemaining: int32(cycles),
		Code:            pgtype.Text{String: code, Valid: code != ""},
		Description:     pgtype.Text{String: description, Valid: description != ""},
	}); err != nil {
		return fmt.Errorf("creating taxa promo: %w", err)
	}
	logger.From(ctx, s.logger).Info("taxa promo created",
		zap.String("store_id", storeID),
		zap.Int("discount_bps", discountBps),
		zap.Int("cycles", cycles),
		zap.String("code", code),
	)
	return nil
}

// OnSubscriptionCycleInvoice bills the accrued GMV commission as an invoice item
// on each renewal invoice (billing_reason=subscription_cycle), so the mensalidade
// and the just-closed cycle's commission land on ONE charge. The amount comes
// from the ledger (fee_cents per sale, net of refunds), not a metered price.
// Idempotent: swept fees are marked invoiced and AddInvoiceItem carries an
// idempotency key, so a redelivered webhook won't rebill.
func (s *Service) OnSubscriptionCycleInvoice(ctx context.Context, inv *StripeInvoice) error {
	subID := inv.SubscriptionID()
	if inv.BillingReason != "subscription_cycle" || inv.ID == "" || subID == "" {
		return nil
	}
	row, err := s.queries.GetSubscriptionByStripeSubID(ctx, pgtype.Text{String: subID, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // unknown subscription (e.g. a legacy metered sub still draining)
		}
		return fmt.Errorf("cycle invoice: load subscription: %w", err)
	}
	storeID := uuidToString(row.StoreID)
	ctx = logger.WithStore(ctx, storeID, "")

	// Cutoff = start of the new cycle (= end of the closed cycle): sales before
	// it belong to the closed cycle and are billed now.
	cutoff := unixToTimestamptz(inv.PeriodStart)

	feeCents, err := s.queries.SumUnbilledBillableFees(ctx, sqlc.SumUnbilledBillableFeesParams{
		StoreID:   row.StoreID,
		CreatedAt: cutoff,
	})
	if err != nil {
		return fmt.Errorf("cycle invoice: sum fees: %w", err)
	}
	if feeCents <= 0 {
		return nil // nothing billable this cycle (or fully offset by refunds → carries forward)
	}
	if !row.StripeCustomerID.Valid {
		return fmt.Errorf("cycle invoice: store %s has no stripe customer", storeID)
	}

	// Apply an active taxa promo (discount on the commission), if any. The promo
	// is consumed one cycle at a time (billing_taxa_promos). The mark-invoiced
	// step below gates re-entry, so a redelivery won't double-consume.
	desc := "Comissão sobre vendas (GMV)"
	promo, perr := s.queries.GetActiveTaxaPromo(ctx, row.StoreID)
	if perr != nil && !errors.Is(perr, pgx.ErrNoRows) {
		return fmt.Errorf("cycle invoice: load taxa promo: %w", perr)
	}
	promoApplied := perr == nil
	if promoApplied {
		feeCents -= feeCents * int64(promo.DiscountBps) / 10000
		desc = fmt.Sprintf("Comissão sobre vendas (GMV) — desconto de %d%%", promo.DiscountBps/100)
	}

	// A 100%-off promo nets to zero: skip the invoice item but still mark fees
	// invoiced and consume the promo.
	if feeCents > 0 {
		if err := s.stripe.AddInvoiceItem(ctx, row.StripeCustomerID.String, inv.ID, feeCents, "brl", desc); err != nil {
			return err
		}
	}
	if err := s.queries.MarkStoreFeesInvoiced(ctx, sqlc.MarkStoreFeesInvoicedParams{
		StoreID:   row.StoreID,
		CreatedAt: cutoff,
		StripeRef: pgtype.Text{String: "invoice:" + inv.ID, Valid: true},
	}); err != nil {
		return fmt.Errorf("cycle invoice: mark fees invoiced: %w", err)
	}
	if promoApplied {
		if err := s.queries.ConsumeTaxaPromoCycle(ctx, promo.ID); err != nil {
			logger.From(ctx, s.logger).Warn("billed commission but failed to consume taxa promo",
				zap.String("store_id", storeID), zap.Error(err))
		}
	}
	logger.From(ctx, s.logger).Info("gmv commission billed",
		zap.String("store_id", storeID),
		zap.String("invoice", inv.ID),
		zap.Int64("fee_cents", feeCents),
		zap.Bool("promo_applied", promoApplied),
	)
	return nil
}

// =============================================================================
// PORTAL (Sprint 3)
// =============================================================================

// CreatePortalSession opens the Customer Portal for the store. Switching
// billing interval (monthly/semestral/annual) is done here too — the 3 Pro
// prices are grouped on the same product in the Portal configuration.
func (s *Service) CreatePortalSession(ctx context.Context, storeID string) (string, error) {
	if s.stripe == nil {
		return "", fmt.Errorf("billing não configurado")
	}
	sid, err := parseUUID(storeID)
	if err != nil {
		return "", err
	}
	row, err := s.queries.GetSubscriptionByStoreID(ctx, sid)
	if err != nil || !row.StripeCustomerID.Valid {
		return "", fmt.Errorf("assinatura Stripe não encontrada para esta loja")
	}
	frontend := strings.TrimRight(config.FrontendURL.String(), "/")
	return s.stripe.CreatePortalSession(ctx, row.StripeCustomerID.String, frontend+"/settings/billing")
}

// =============================================================================
// LEDGER DE GMV (append-only) + METERING (PRD 007)
// =============================================================================

// OnCartPaid is the billing reactor for cart.paid: records the sale on the
// ledger and best-effort reports the meter event to Stripe. Errors propagate
// so asynq retries + DLQ protect against ledger loss (AC2/AC9 symmetry with
// OnCartRefunded). Idempotent via UNIQUE (cart_id, 'sale') DO NOTHING.
//
// Compat/rollout: gmvCents=0 → fallback to GetCartGMVCents; if still 0 → no-op
// (preserves current behaviour for carts with no items / pure-frete carts).
func (s *Service) OnCartPaid(ctx context.Context, storeID, cartID string, gmvCents int64) error {
	sid, err := parseUUID(storeID)
	if err != nil {
		return fmt.Errorf("billing OnCartPaid: invalid store_id %q: %w", storeID, err)
	}
	cid, err := parseUUID(cartID)
	if err != nil {
		return fmt.Errorf("billing OnCartPaid: invalid cart_id %q: %w", cartID, err)
	}

	// (1) Validate subscription — absent → error (no subscription = DLQ).
	sub, err := s.queries.GetSubscriptionByStoreID(ctx, sid)
	if err != nil {
		return fmt.Errorf("billing OnCartPaid: no subscription for store %q: %w", storeID, err)
	}

	// (2) Base da COMISSÃO = valor LÍQUIDO que o cliente pagou pelos produtos
	// (bruto − cupom − desconto PIX, SEM frete). O gmv_cents do evento é o BRUTO
	// (é o que o pedido usa em total_cents); a comissão incide sobre o LÍQUIDO —
	// a loja recebe o valor com desconto, então cobrar sobre o cheio cobraria
	// taxa de dinheiro que não entrou. GetCartCommissionBaseCents é a fonte única
	// da regra. Falha na query → cai no bruto do evento (conservador).
	baseCents, err := s.queries.GetCartCommissionBaseCents(ctx, cid)
	if err != nil {
		logger.From(ctx, s.logger).Warn("OnCartPaid: commission base query failed, using gross gmv",
			zap.String("cart_id", cartID), zap.Error(err))
		baseCents = gmvCents
	}

	// (3) Base 0 (sem itens ou puro-frete) → early return, não grava linha.
	if baseCents <= 0 {
		return nil
	}

	// billable = venda contabilizada neste ciclo (assinatura convertida).
	// Trial/paused/etc: grava para visibilidade, billable=false.
	// A comissão sobre GMV foi eliminada para todo mundo — fee sempre 0. O
	// ledger/gmv.recorded continuam existindo só para o dashboard de analytics.
	billable := sub.Status == StatusActive || sub.Status == StatusPastDue
	feeCents := int64(0)

	// (4) Insert do ledger — propaga erro real; ErrNoRows = idempotente.
	// No more commission: fee_bps/fee_cents are always 0. The row still exists
	// for the analytics dashboard (gmv.recorded) and the extrato.
	if _, err = s.queries.InsertLedgerEntry(ctx, sqlc.InsertLedgerEntryParams{
		StoreID:     sid,
		CartID:      cid,
		EntryType:   "sale",
		AmountCents: baseCents,
		Plan:        sub.Plan,
		FeeBps:      0,
		FeeCents:    feeCents,
		Billable:    billable,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // conflito (cart_id, 'sale') — já registrado, retry idempotente
		}
		return fmt.Errorf("billing OnCartPaid: insert ledger entry: %w", err)
	}

	logger.From(ctx, s.logger).Info("gmv sale recorded",
		zap.String("store_id", storeID),
		zap.String("cart_id", cartID),
		zap.Int64("amount_cents", baseCents),
		zap.Bool("billable", billable),
	)

	// gmv.recorded fires only on a NEW ledger row (ErrNoRows path = idempotency
	// guard), so it is exactly-once per sale.
	_ = events.EmitInternal(ctx, s.queries, events.GMVRecorded, "gmv.recorded:"+cartID, struct {
		StoreID     string `json:"store_id"`
		CartID      string `json:"cart_id"`
		AmountCents int64  `json:"amount_cents"`
		FeeCents    int64  `json:"fee_cents"`
		Billable    bool   `json:"billable"`
	}{storeID, cartID, baseCents, feeCents, billable})
	return nil
}

// OnCartRefunded records the refund on the ledger and reimburses the fee via
// a Stripe customer-balance credit — at the bps charged ON THE SALE, even
// when the refund lands on a later billing cycle (the credit auto-applies to
// the next invoice). Trial sales (billable=false) record the entry for
// visibility but credit nothing (no fee was ever charged).
// Returns error on ledger insert failure so ReactCartRefunded can DLQ.
func (s *Service) OnCartRefunded(ctx context.Context, storeID, cartID string) error {
	sid, err := parseUUID(storeID)
	if err != nil {
		return fmt.Errorf("billing OnCartRefunded: invalid store_id %q: %w", storeID, err)
	}
	cid, err := parseUUID(cartID)
	if err != nil {
		return fmt.Errorf("billing OnCartRefunded: invalid cart_id %q: %w", cartID, err)
	}

	sale, err := s.queries.GetLedgerSaleEntry(ctx, cid)
	if err != nil {
		// No sale = nothing to refund (DLQ path não aplicável; retorno nil).
		logger.From(ctx, s.logger).Debug("refund without a recorded sale — ignored",
			zap.String("cart_id", cartID))
		return nil
	}

	feeCredit := int64(0)
	if sale.Billable {
		feeCredit = sale.FeeCents
	}

	// The refund records a negative-fee refund_credit entry (stripe_ref NULL).
	// It nets against the next cycle's commission invoice item automatically
	// (SumUnbilledBillableFees includes it), so NO separate balance credit is
	// issued — that would double-credit. Cross-cycle refunds land the credit on
	// the next invoice; a net-negative cycle carries forward until offset.
	if _, err = s.queries.InsertLedgerEntry(ctx, sqlc.InsertLedgerEntryParams{
		StoreID:     sid,
		CartID:      cid,
		EntryType:   "refund_credit",
		AmountCents: -sale.AmountCents,
		Plan:        sale.Plan,
		FeeBps:      sale.FeeBps,
		FeeCents:    -feeCredit,
		Billable:    sale.Billable,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // estorno já registrado (retry idempotente)
		}
		return fmt.Errorf("billing OnCartRefunded: insert ledger entry: %w", err)
	}

	// gmv.refunded exactly-once (ErrNoRows guard above).
	_ = events.EmitInternal(ctx, s.queries, events.GMVRefunded, "gmv.refunded:"+cartID, struct {
		StoreID        string `json:"store_id"`
		CartID         string `json:"cart_id"`
		AmountCents    int64  `json:"amount_cents"`
		FeeCreditCents int64  `json:"fee_credit_cents"`
	}{storeID, cartID, sale.AmountCents, feeCredit})

	logger.From(ctx, s.logger).Info("refund recorded (fee credit nets next cycle)",
		zap.String("store_id", storeID),
		zap.String("cart_id", cartID),
		zap.Int64("credit_cents", feeCredit),
	)
	return nil
}

// =============================================================================
// FINANCEIRO (usage + extrato)
// =============================================================================

// PeriodUsage resumes the current billing cycle for the Financeiro hero.
type PeriodUsage struct {
	PeriodStart        time.Time `json:"periodStart"`
	GMVCents           int64     `json:"gmvCents"`
	FeeCents           int64     `json:"feeCents"`      // net — after an active taxa promo (what will be charged)
	FeeCentsGross      int64     `json:"feeCentsGross"` // before the promo discount
	TaxaDiscountBps    int32     `json:"taxaDiscountBps"`
	Sales              int32     `json:"sales"`
	Refunds            int32     `json:"refunds"`
	RefundCreditsCents int64     `json:"refundCreditsCents"`
}

// GetUsage returns the ledger summary for the store's current cycle
// (falls back to the last 30 days when no period anchor exists).
func (s *Service) GetUsage(ctx context.Context, storeID string) (*PeriodUsage, error) {
	sid, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	since := time.Now().Add(-30 * 24 * time.Hour)
	if sub, err := s.queries.GetSubscriptionByStoreID(ctx, sid); err == nil && sub.CurrentPeriodStart.Valid {
		since = sub.CurrentPeriodStart.Time
	}

	row, err := s.queries.GetLedgerUsageSince(ctx, sqlc.GetLedgerUsageSinceParams{
		StoreID:   sid,
		CreatedAt: pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("loading ledger usage: %w", err)
	}
	// Apply the active taxa promo so the preview matches what OnSubscriptionCycleInvoice
	// will actually charge this cycle (the reactor discounts the commission there).
	feeNet := row.FeeCents
	var taxaDiscountBps int32
	if promo, perr := s.queries.GetActiveTaxaPromo(ctx, sid); perr == nil {
		taxaDiscountBps = promo.DiscountBps
		feeNet -= feeNet * int64(promo.DiscountBps) / 10000
	}

	return &PeriodUsage{
		PeriodStart:        since,
		GMVCents:           row.GmvCents,
		FeeCents:           feeNet,
		FeeCentsGross:      row.FeeCents,
		TaxaDiscountBps:    taxaDiscountBps,
		Sales:              row.Sales,
		Refunds:            row.Refunds,
		RefundCreditsCents: row.RefundCreditsCents,
	}, nil
}

// StatementEntry is one extrato line for the merchant.
type StatementEntry struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"` // sale | refund_credit | adjustment
	AmountCents  int64     `json:"amountCents"`
	FeeCents     int64     `json:"feeCents"`
	FeeBps       int32     `json:"feeBps"`
	Billable     bool      `json:"billable"`
	CustomerName string    `json:"customerName,omitempty"`
	Handle       string    `json:"handle,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

// GetStatement returns the paginated extrato (most recent first).
func (s *Service) GetStatement(ctx context.Context, storeID string, page, limit int) ([]StatementEntry, error) {
	sid, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	if page < 1 {
		page = 1
	}

	rows, err := s.queries.ListLedgerEntries(ctx, sqlc.ListLedgerEntriesParams{
		StoreID: sid,
		Limit:   int32(limit),
		Offset:  int32((page - 1) * limit),
	})
	if err != nil {
		return nil, fmt.Errorf("loading statement: %w", err)
	}

	entries := make([]StatementEntry, len(rows))
	for i, r := range rows {
		entries[i] = StatementEntry{
			ID:           uuidToString(r.ID),
			Type:         r.EntryType,
			AmountCents:  r.AmountCents,
			FeeCents:     r.FeeCents,
			FeeBps:       r.FeeBps,
			Billable:     r.Billable,
			CustomerName: r.CustomerName.String,
			Handle:       r.PlatformHandle,
			CreatedAt:    r.CreatedAt.Time,
		}
	}
	return entries, nil
}
