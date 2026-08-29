// Command migrate-billing-plan is a one-off tool that moves every store
// still on the retired 3-plan model (start/grow/scale) to the single "pro"
// plan (monthly interval) and forgives any GMV commission accrued but not
// yet invoiced — see docs/../billing pricing overhaul (2026-08).
//
// Safe by default: runs in dry-run mode (reports what it WOULD do) unless
// --confirm is passed. Idempotent — safe to re-run; already-migrated stores
// (plan='pro') simply don't show up again.
//
// Usage:
//
//	go run ./cmd/migrate-billing-plan           # dry-run
//	go run ./cmd/migrate-billing-plan --confirm # executes against Stripe + DB
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/billing"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/database"
)

func main() {
	confirm := flag.Bool("confirm", false, "execute the migration for real (default: dry-run, no writes)")
	flag.Parse()

	log, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintln(os.Stderr, "building logger:", err)
		os.Exit(1)
	}
	defer log.Sync()

	ctx := context.Background()
	databaseURL := config.DatabaseURL.Required()

	pool, err := database.NewPool(ctx, databaseURL)
	if err != nil {
		log.Sugar().Fatalf("connecting to database: %v", err)
	}
	defer pool.Close()

	queries := sqlc.New(pool)
	svc := billing.NewService(queries, log)

	if !*confirm {
		log.Info("running in DRY-RUN mode — pass --confirm to execute against Stripe/DB",
			zap.String("environment", config.Environment()))
	} else {
		log.Warn("running with --confirm — this WILL change live Stripe subscriptions and the database",
			zap.String("environment", config.Environment()))
	}

	results, err := svc.MigrateLegacySubscriptions(ctx, *confirm)
	if err != nil {
		log.Sugar().Fatalf("migration failed: %v", err)
	}

	counts := map[string]int{}
	for _, r := range results {
		counts[r.Status]++
		log.Info("store",
			zap.String("store_id", r.StoreID),
			zap.String("old_plan", r.OldPlan),
			zap.String("status", r.Status),
			zap.String("detail", r.Detail),
		)
	}
	log.Info("migration summary",
		zap.Int("total", len(results)),
		zap.Any("by_status", counts),
	)
}
