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
	"livecart/apps/api/lib/config"
)

// Service owns the subscription lifecycle. The local table is the access
// source of truth; Stripe webhooks keep it in sync.
type Service struct {
	queries *sqlc.Queries
	stripe  *StripeClient // nil when STRIPE_SECRET_KEY is absent (local-only trials)
	logger  *zap.Logger
}

// NewService builds the billing service.
func NewService(queries *sqlc.Queries, logger *zap.Logger) *Service {
	return &Service{
		queries: queries,
		stripe:  NewStripeClient(config.StripeSecretKey.String(), logger),
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

	// Mirror on Stripe when configured and not yet linked. Failures are
	// non-fatal: the local trial grants access and the next call retries.
	if s.stripe != nil && !row.StripeSubscriptionID.Valid {
		if err := s.provisionStripe(ctx, &row, storeID, storeName, email); err != nil {
			s.logger.Warn("stripe trial provisioning pending",
				zap.String("store_id", storeID),
				zap.Error(err),
			)
		}
	}

	state := s.toState(&row)
	return &state, nil
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

	// Trial anchored on the Grow plan (PRD 007 decision: plan chosen at
	// conversion; prices only matter once a card exists).
	trialEnd := row.TrialEndsAt.Time
	if !row.TrialEndsAt.Valid || trialEnd.Before(time.Now()) {
		trialEnd = time.Now().Add(TrialDays * 24 * time.Hour)
	}
	sub, err := s.stripe.CreateTrialSubscription(ctx, customerID, Plans()[PlanGrow], trialEnd)
	if err != nil {
		// Persist the customer even when the subscription fails, so the
		// retry skips customer creation.
		_, _ = s.queries.SetSubscriptionStripeRefs(ctx, sqlc.SetSubscriptionStripeRefsParams{
			StoreID:          row.StoreID,
			StripeCustomerID: pgtype.Text{String: customerID, Valid: true},
		})
		return err
	}

	updated, err := s.queries.SetSubscriptionStripeRefs(ctx, sqlc.SetSubscriptionStripeRefsParams{
		StoreID:              row.StoreID,
		StripeCustomerID:     pgtype.Text{String: customerID, Valid: true},
		StripeSubscriptionID: pgtype.Text{String: sub.ID, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("persisting stripe refs: %w", err)
	}
	*row = updated

	s.logger.Info("stripe trial provisioned",
		zap.String("store_id", storeID),
		zap.String("customer", customerID),
		zap.String("subscription", sub.ID),
	)
	return nil
}

// =============================================================================
// STATE (middleware / sync / FE)
// =============================================================================

// GetState returns the access snapshot for a store. Missing row (legacy
// store) returns nil — caller decides whether to lazily ensure.
func (s *Service) GetState(ctx context.Context, storeID string) (*SubscriptionState, error) {
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
	state := s.toState(&row)
	return &state, nil
}

func (s *Service) toState(row *sqlc.Subscription) SubscriptionState {
	now := time.Now()
	state := SubscriptionState{
		Status:            row.Status,
		Plan:              Plan(row.Plan),
		CancelAtPeriodEnd: row.CancelAtPeriodEnd,
		HasPaymentMethod:  row.Status == StatusActive || row.Status == StatusPastDue,
	}
	if row.TrialEndsAt.Valid {
		t := row.TrialEndsAt.Time
		state.TrialEndsAt = &t
		if days := int(time.Until(t).Hours()/24) + 1; days > 0 && row.Status == StatusTrialing {
			state.TrialDaysLeft = days
		}
	}
	if row.CurrentPeriodEnd.Valid {
		t := row.CurrentPeriodEnd.Time
		state.CurrentPeriodEnd = &t
	}
	if row.GraceUntil.Valid {
		t := row.GraceUntil.Time
		state.GraceUntil = &t
	}
	state.Blocked = blocked(row.Status, row.ManualOverride, state.TrialEndsAt, state.GraceUntil, now)
	return state
}

// =============================================================================
// WEBHOOK PROCESSING
// =============================================================================

// ProcessWebhookEvent applies a verified Stripe event to the local table.
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
		if session.Mode != "setup" {
			return nil
		}
		return s.completeConversion(ctx, &session)

	case "invoice.payment_failed":
		// Grace handling rides the subscription.updated → past_due event;
		// logged here for the audit trail.
		s.logger.Warn("stripe invoice payment failed", zap.String("event", event.ID))
		return nil

	default:
		s.logger.Debug("stripe event ignored", zap.String("type", event.Type))
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
			plan = string(PlanGrow)
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
		CurrentPeriodStart:   unixToTimestamptz(sub.CurrentPeriodStart),
		CurrentPeriodEnd:     unixToTimestamptz(sub.CurrentPeriodEnd),
		CancelAtPeriodEnd:    sub.CancelAtPeriodEnd,
		GraceUntil:           graceUntil,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger.Warn("webhook for unknown subscription",
				zap.String("subscription", sub.ID),
			)
			return nil
		}
		return fmt.Errorf("applying subscription update: %w", err)
	}

	s.logger.Info("subscription updated from stripe",
		zap.String("store_id", uuidToString(row.StoreID)),
		zap.String("status", status),
		zap.String("plan", row.Plan),
	)
	return nil
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
func (s *Service) CreateConversionCheckout(ctx context.Context, storeID string, plan Plan) (string, error) {
	cfg, ok := Plans()[plan]
	if !ok || !cfg.SelfService {
		return "", fmt.Errorf("plano inválido para contratação self-service: %s", plan)
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
	if !row.StripeCustomerID.Valid || !row.StripeSubscriptionID.Valid {
		return "", fmt.Errorf("assinatura Stripe ainda não provisionada — tente novamente em instantes")
	}

	frontend := strings.TrimRight(config.FrontendURL.String(), "/")
	session, err := s.stripe.CreateSetupCheckoutSession(ctx,
		row.StripeCustomerID.String,
		frontend+"/settings/billing?billing=success",
		frontend+"/settings/billing?billing=cancelled",
		map[string]string{
			"store_id":        storeID,
			"plan":            string(plan),
			"subscription_id": row.StripeSubscriptionID.String,
		},
	)
	if err != nil {
		return "", err
	}
	return session.URL, nil
}

// completeConversion runs on checkout.session.completed (setup mode): grabs
// the collected card, activates the chosen plan and syncs the local row.
func (s *Service) completeConversion(ctx context.Context, session *CheckoutSession) error {
	plan := Plan(session.Metadata["plan"])
	subID := session.Metadata["subscription_id"]
	if plan == "" || subID == "" {
		s.logger.Debug("checkout session without conversion metadata — ignored",
			zap.String("session", session.ID))
		return nil
	}
	cfg, ok := Plans()[plan]
	if !ok {
		return fmt.Errorf("unknown plan in session metadata: %s", plan)
	}

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

	s.logger.Info("subscription converted",
		zap.String("store_id", session.Metadata["store_id"]),
		zap.String("plan", string(plan)),
		zap.String("status", activated.Status),
	)
	// Sync imediato (o customer.subscription.updated também chega e é idempotente).
	return s.applySubscription(ctx, activated, "conversion")
}

// =============================================================================
// PORTAL + PLAN CHANGE (Sprint 3)
// =============================================================================

// CreatePortalSession opens the Customer Portal for the store.
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

// ChangePlan applies an upgrade immediately (prorated) or schedules a
// downgrade for the period end. Requires an active (converted) subscription —
// trials convert through the checkout flow instead.
func (s *Service) ChangePlan(ctx context.Context, storeID string, target Plan) (*SubscriptionState, error) {
	cfg, ok := Plans()[target]
	if !ok || !cfg.SelfService {
		return nil, fmt.Errorf("plano inválido: %s", target)
	}
	if s.stripe == nil {
		return nil, fmt.Errorf("billing não configurado")
	}

	sid, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}
	row, err := s.queries.GetSubscriptionByStoreID(ctx, sid)
	if err != nil || !row.StripeSubscriptionID.Valid {
		return nil, fmt.Errorf("assinatura Stripe não encontrada para esta loja")
	}
	if row.Status != StatusActive && row.Status != StatusPastDue {
		return nil, fmt.Errorf("mudança de plano requer assinatura ativa — finalize a contratação primeiro")
	}
	if Plan(row.Plan) == target {
		state := s.toState(&row)
		return &state, nil
	}

	sub, err := s.stripe.GetSubscription(ctx, row.StripeSubscriptionID.String)
	if err != nil {
		return nil, err
	}

	current := Plans()[Plan(row.Plan)]
	if cfg.FlatCents > current.FlatCents {
		// Upgrade: imediato com proração.
		updated, err := s.stripe.UpgradeSubscription(ctx, sub, cfg)
		if err != nil {
			return nil, err
		}
		if err := s.applySubscription(ctx, updated, "plan_change"); err != nil {
			return nil, err
		}
	} else {
		// Downgrade: agendado para o fim do período (sem estorno).
		if err := s.stripe.ScheduleDowngrade(ctx, sub, cfg); err != nil {
			return nil, err
		}
		// O plano local muda quando a fase virar (webhook subscription.updated).
	}

	fresh, err := s.queries.GetSubscriptionByStoreID(ctx, sid)
	if err != nil {
		return nil, err
	}
	state := s.toState(&fresh)
	return &state, nil
}

// =============================================================================
// LEDGER DE GMV (append-only) + METERING (PRD 007)
// =============================================================================

// ReportPaidGMV records the paid sale on the local ledger (source of truth
// for the merchant's Financeiro and platform GMV) and reports it to the
// Stripe meter. Fire-and-forget: failures are logged, never propagated.
//
// Idempotent end to end: the ledger has UNIQUE (cart_id, 'sale') and the
// meter event carries identifier gmv-<cart_id>.
func (s *Service) ReportPaidGMV(ctx context.Context, storeID, cartID string, amountCents int64) {
	if amountCents <= 0 {
		return
	}
	sid, err := parseUUID(storeID)
	if err != nil {
		return
	}
	cid, err := parseUUID(cartID)
	if err != nil {
		return
	}

	sub, err := s.queries.GetSubscriptionByStoreID(ctx, sid)
	if err != nil {
		s.logger.Warn("gmv not recorded: no subscription row",
			zap.String("store_id", storeID), zap.Error(err))
		return
	}

	cfg := Plans()[Plan(sub.Plan)]
	// billable = a taxa deste ciclo sera cobrada (assinatura convertida).
	// Trial/paused/etc: registramos a venda para visibilidade, com fee
	// snapshot, mas billable=false (estorno nao gera credito).
	billable := sub.Status == StatusActive || sub.Status == StatusPastDue
	feeCents := amountCents * int64(cfg.GMVBps) / 10000

	entry, err := s.queries.InsertLedgerEntry(ctx, sqlc.InsertLedgerEntryParams{
		StoreID:     sid,
		CartID:      cid,
		EntryType:   "sale",
		AmountCents: amountCents,
		Plan:        sub.Plan,
		FeeBps:      int32(cfg.GMVBps),
		FeeCents:    feeCents,
		Billable:    billable,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return // conflito (cart_id, sale) — ja registrado, retry de webhook
		}
		s.logger.Error("failed to record gmv ledger entry",
			zap.String("cart_id", cartID), zap.Error(err))
		return
	}

	// Meter na Stripe (melhor esforco; a linha do ledger fica sem stripe_ref
	// quando falha — visivel para reconciliacao).
	if s.stripe != nil && sub.StripeCustomerID.Valid {
		event := config.StripeGMVMeterEvent.StringOr("gmv_cents")
		if err := s.stripe.SendMeterEvent(ctx, event, sub.StripeCustomerID.String, "gmv-"+cartID, amountCents); err != nil {
			s.logger.Warn("failed to report gmv meter event",
				zap.String("cart_id", cartID), zap.Error(err))
		} else {
			_ = s.queries.SetLedgerEntryStripeRef(ctx, sqlc.SetLedgerEntryStripeRefParams{
				ID:        entry.ID,
				StripeRef: pgtype.Text{String: "meter:gmv-" + cartID, Valid: true},
			})
		}
	}

	s.logger.Info("gmv sale recorded",
		zap.String("store_id", storeID),
		zap.String("cart_id", cartID),
		zap.Int64("amount_cents", amountCents),
		zap.Bool("billable", billable),
	)
}

// OnCartRefunded records the refund on the ledger and reimburses the fee via
// a Stripe customer-balance credit — at the bps charged ON THE SALE, even
// when the refund lands on a later billing cycle (the credit auto-applies to
// the next invoice). Trial sales (billable=false) record the entry for
// visibility but credit nothing (no fee was ever charged).
func (s *Service) OnCartRefunded(ctx context.Context, storeID, cartID string) {
	sid, err := parseUUID(storeID)
	if err != nil {
		return
	}
	cid, err := parseUUID(cartID)
	if err != nil {
		return
	}

	sale, err := s.queries.GetLedgerSaleEntry(ctx, cid)
	if err != nil {
		s.logger.Debug("refund without a recorded sale — ignored",
			zap.String("cart_id", cartID))
		return
	}

	feeCredit := int64(0)
	if sale.Billable {
		feeCredit = sale.FeeCents
	}

	entry, err := s.queries.InsertLedgerEntry(ctx, sqlc.InsertLedgerEntryParams{
		StoreID:     sid,
		CartID:      cid,
		EntryType:   "refund_credit",
		AmountCents: -sale.AmountCents,
		Plan:        sale.Plan,
		FeeBps:      sale.FeeBps,
		FeeCents:    -feeCredit,
		Billable:    sale.Billable,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return // estorno ja registrado (retry de webhook)
		}
		s.logger.Error("failed to record refund ledger entry",
			zap.String("cart_id", cartID), zap.Error(err))
		return
	}

	if feeCredit > 0 && s.stripe != nil {
		sub, err := s.queries.GetSubscriptionByStoreID(ctx, sid)
		if err == nil && sub.StripeCustomerID.Valid {
			desc := fmt.Sprintf("Estorno de venda — devolução da taxa (%.2f%% de R$ %.2f)",
				float64(sale.FeeBps)/100, float64(sale.AmountCents)/100)
			if err := s.stripe.CreateCustomerBalanceCredit(ctx, sub.StripeCustomerID.String, feeCredit, desc); err != nil {
				s.logger.Error("failed to create refund balance credit",
					zap.String("cart_id", cartID), zap.Error(err))
			} else {
				_ = s.queries.SetLedgerEntryStripeRef(ctx, sqlc.SetLedgerEntryStripeRefParams{
					ID:        entry.ID,
					StripeRef: pgtype.Text{String: "balance_credit", Valid: true},
				})
				s.logger.Info("refund fee credited",
					zap.String("store_id", storeID),
					zap.String("cart_id", cartID),
					zap.Int64("credit_cents", feeCredit),
				)
			}
		}
	}
}

// =============================================================================
// FINANCEIRO (usage + extrato)
// =============================================================================

// PeriodUsage resumes the current billing cycle for the Financeiro hero.
type PeriodUsage struct {
	PeriodStart        time.Time `json:"periodStart"`
	GMVCents           int64     `json:"gmvCents"`
	FeeCents           int64     `json:"feeCents"`
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
	return &PeriodUsage{
		PeriodStart:        since,
		GMVCents:           row.GmvCents,
		FeeCents:           row.FeeCents,
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
