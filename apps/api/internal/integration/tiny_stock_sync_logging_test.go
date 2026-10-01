package integration

import (
	"errors"
	"fmt"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/logger"
)

func TestStockRecoveryLogsExpectedDeferralsWithoutHidingFailures(t *testing.T) {
	waitlistBusy := events.NewDeferredError("waitlist head temporarily unavailable")
	for _, tc := range []struct {
		name  string
		err   error
		level zapcore.Level
	}{
		{name: "cooldown", err: fmt.Errorf("refreshing credentials: %w", errTokenRefreshDeferred), level: zap.InfoLevel},
		{name: "stock lease", err: errERPStockReadBusy, level: zap.InfoLevel},
		{name: "invalidated snapshot", err: errERPStockSnapshotInvalidated, level: zap.InfoLevel},
		{name: "waitlist contention", err: waitlistBusy, level: zap.InfoLevel},
		{name: "database failure", err: errors.New("database unavailable"), level: zap.WarnLevel},
		{name: "actual provider rejection", err: errors.New("bling: HTTP 403"), level: zap.WarnLevel},
		{
			name: "joined persistence failure",
			err:  errors.Join(errERPStockReadBusy, errors.New("checkpoint write failed")), level: zap.WarnLevel,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)
			svc := &Service{logger: zap.New(core)}
			ctx := logger.WithStore(t.Context(), "store-1", "store-test")
			svc.logStockRecoveryError(ctx, "ERP stock recovery deferred", "product-1", tc.err)
			entries := logs.All()
			if len(entries) != 1 || entries[0].Level != tc.level {
				t.Fatalf("incorrect recovery severity: %+v", entries)
			}
			fields := entries[0].ContextMap()
			if fields["store_id"] != "store-1" || fields["external_product_id"] != "product-1" {
				t.Fatalf("lost recovery correlation: %v", fields)
			}
			if fields["error"] != tc.err.Error() {
				t.Fatalf("lost failure cause: %v", fields)
			}
		})
	}
}
