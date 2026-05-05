package coupon

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// RedemptionExpirerWorker periodically returns reserved-redemption slots
// from carts that will never be paid (expired / cancelled / failed). The
// public ApplyToCart flow holds slots optimistically — without this sweep,
// the merchant's max_uses cap would creep upward across abandoned carts.
//
// Pattern mirrors integration.TokenRefreshWorker: ticker + stop channel +
// WaitGroup for clean shutdown.
type RedemptionExpirerWorker struct {
	service  *Service
	logger   *zap.Logger
	interval time.Duration
	limit    int // rows per sweep, capped to avoid long-running txns
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// RedemptionExpirerWorkerConfig contains configuration for the worker.
type RedemptionExpirerWorkerConfig struct {
	Service  *Service
	Logger   *zap.Logger
	Interval time.Duration // default: 5 minutes
	Limit    int           // default: 200
}

func NewRedemptionExpirerWorker(cfg RedemptionExpirerWorkerConfig) *RedemptionExpirerWorker {
	interval := cfg.Interval
	if interval == 0 {
		interval = 5 * time.Minute
	}
	limit := cfg.Limit
	if limit == 0 {
		limit = 200
	}
	return &RedemptionExpirerWorker{
		service:  cfg.Service,
		logger:   cfg.Logger.Named("coupon-expirer"),
		interval: interval,
		limit:    limit,
		stopCh:   make(chan struct{}),
	}
}

func (w *RedemptionExpirerWorker) Start() {
	w.wg.Add(1)
	go w.run()
	w.logger.Info("redemption expirer started",
		zap.Duration("interval", w.interval),
		zap.Int("limit_per_sweep", w.limit),
	)
}

func (w *RedemptionExpirerWorker) Stop() {
	close(w.stopCh)
	w.wg.Wait()
	w.logger.Info("redemption expirer stopped")
}

func (w *RedemptionExpirerWorker) run() {
	defer w.wg.Done()

	// Run once on boot so a service that crashed and missed sweeps catches
	// up immediately instead of after the first interval.
	w.sweep()

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.sweep()
		case <-w.stopCh:
			return
		}
	}
}

func (w *RedemptionExpirerWorker) sweep() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	expired, skipped, err := w.service.ExpireStaleReservedRedemptions(ctx, w.limit)
	if err != nil {
		w.logger.Error("expirer sweep failed", zap.Error(err))
		return
	}
	if expired == 0 && skipped == 0 {
		// Quiet successful no-op — most sweeps in steady state.
		w.logger.Debug("expirer sweep clean")
		return
	}
	w.logger.Info("expirer sweep finished",
		zap.Int("expired", expired),
		zap.Int("skipped_terminal_race", skipped),
	)
}
