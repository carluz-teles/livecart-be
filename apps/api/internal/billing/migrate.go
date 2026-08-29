package billing

// One-off migration: moves stores still on the retired 3-plan model
// (start/grow/scale) to the single "pro" plan, and forgives any GMV
// commission accrued but not yet invoiced (the commission is eliminated for
// everyone, effective immediately — see cmd/migrate-billing-plan). Lives in
// the billing package (not the CLI) because it needs the unexported
// stripeGateway and sqlc.Queries the Service already holds.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/logger"
)

// migrationForgivenRef marks ledger entries forgiven by this migration so
// OnSubscriptionCycleInvoice never picks them up again (same predicate as a
// normal invoiced sweep, just with a distinguishable stripe_ref).
const migrationForgivenRef = "forgiven:pricing-migration-2026-08"

// MigrationResult reports the outcome for one store.
type MigrationResult struct {
	StoreID string
	OldPlan string
	Status  string // "migrated" | "dry-run" | "skipped" | "error"
	Detail  string
}

// MigrateLegacySubscriptions moves every store still on start/grow/scale to
// the single Pro plan (monthly interval — the safe default for stores that
// never chose one) and forgives their unbilled GMV commission. confirm=false
// only reports what WOULD happen (no Stripe/DB writes) — callers must pass
// confirm=true explicitly to execute. Idempotent: re-running only picks up
// stores ListLegacyPlanSubscriptions still returns (already-migrated stores
// have plan='pro' and drop out of that query).
func (s *Service) MigrateLegacySubscriptions(ctx context.Context, confirm bool) ([]MigrationResult, error) {
	if s.stripe == nil {
		return nil, fmt.Errorf("billing não configurado (STRIPE_SECRET_KEY ausente)")
	}
	price, ok := Plans()[PlanPro].Prices[IntervalMonthly]
	if !ok || price.PriceID == "" {
		return nil, fmt.Errorf("preço mensal do plano Pro não configurado (STRIPE_PRICE_PRO_MONTHLY)")
	}

	rows, err := s.queries.ListLegacyPlanSubscriptions(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing legacy subscriptions: %w", err)
	}

	results := make([]MigrationResult, 0, len(rows))
	for _, row := range rows {
		storeID := uuidToString(row.StoreID)
		res := MigrationResult{StoreID: storeID, OldPlan: row.Plan}

		if !row.StripeSubscriptionID.Valid {
			res.Status = "skipped"
			res.Detail = "sem stripe_subscription_id"
			results = append(results, res)
			continue
		}
		if !confirm {
			res.Status = "dry-run"
			res.Detail = "migraria para pro/monthly + perdoaria saldo GMV pendente"
			results = append(results, res)
			continue
		}

		sub, err := s.stripe.GetSubscription(ctx, row.StripeSubscriptionID.String)
		if err != nil {
			res.Status, res.Detail = "error", "loading stripe subscription: "+err.Error()
			results = append(results, res)
			continue
		}
		if _, err := s.stripe.MigrateSubscriptionItems(ctx, sub, price.PriceID); err != nil {
			res.Status, res.Detail = "error", "stripe item swap: "+err.Error()
			results = append(results, res)
			continue
		}
		if err := s.queries.MigrateSubscriptionToPro(ctx, sqlc.MigrateSubscriptionToProParams{
			StoreID:         row.StoreID,
			BillingInterval: string(IntervalMonthly),
		}); err != nil {
			res.Status, res.Detail = "error", "stripe migrado mas gravação local falhou: "+err.Error()
			results = append(results, res)
			continue
		}
		if err := s.queries.MarkStoreFeesInvoiced(ctx, sqlc.MarkStoreFeesInvoicedParams{
			StoreID:   row.StoreID,
			CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			StripeRef: pgtype.Text{String: migrationForgivenRef, Valid: true},
		}); err != nil {
			// Plan/price already migrated — a failed forgive just means the
			// old fee could resurface on the next cycle invoice; log loudly
			// but don't report the store as failed.
			logger.From(ctx, s.logger).Warn("pricing migration: failed to forgive pending fees",
				zap.String("store_id", storeID), zap.Error(err))
			res.Detail = "migrado; ATENÇÃO: falha ao perdoar saldo pendente (ver logs)"
		}
		res.Status = "migrated"
		results = append(results, res)
	}
	return results, nil
}
