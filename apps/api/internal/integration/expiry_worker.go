package integration

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// CartExpiryWorker periodically expires carts whose payment window elapsed. It
// is the missing clock: before it, cart expiration was purely lazy (triggered
// by a new purchase comment on the same product in a live event) and blind to
// post-live 'checkout' carts, so their local stock and Tiny reservations leaked
// forever. The worker sweeps globally, independent of comment traffic and of
// the event status.
//
// Pattern mirrors coupon.RedemptionExpirerWorker: run-once on boot + ticker +
// stop channel + WaitGroup for clean shutdown. Each cart is expired under a
// per-cart advisory lock (see Service.ExpireCart), so it never races the
// payment webhook.
type CartExpiryWorker struct {
	service  *Service
	logger   *zap.Logger
	interval time.Duration
	limit    int32 // carts per sweep, capped to bound each cycle
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// CartExpiryWorkerConfig configures the worker.
type CartExpiryWorkerConfig struct {
	Service  *Service
	Logger   *zap.Logger
	Interval time.Duration // default: 5 minutes
	Limit    int32         // default: 200
}

func NewCartExpiryWorker(cfg CartExpiryWorkerConfig) *CartExpiryWorker {
	interval := cfg.Interval
	if interval == 0 {
		interval = 5 * time.Minute
	}
	limit := cfg.Limit
	if limit == 0 {
		limit = 200
	}
	return &CartExpiryWorker{
		service:  cfg.Service,
		logger:   cfg.Logger.Named("cart-expiry"),
		interval: interval,
		limit:    limit,
		stopCh:   make(chan struct{}),
	}
}

func (w *CartExpiryWorker) Start() {
	w.wg.Add(1)
	go w.run()
	w.logger.Info("cart expiry worker started",
		zap.Duration("interval", w.interval),
		zap.Int32("limit_per_sweep", w.limit),
	)
}

func (w *CartExpiryWorker) Stop() {
	close(w.stopCh)
	w.wg.Wait()
	w.logger.Info("cart expiry worker stopped")
}

func (w *CartExpiryWorker) run() {
	defer w.wg.Done()

	// Run once on boot so a service that missed sweeps (e.g. after a crash or a
	// deploy) catches up immediately, not after the first interval.
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

func (w *CartExpiryWorker) sweep() {
	// Generous timeout: each expired cart may make best-effort ERP calls.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	w.service.SweepExpiredCarts(ctx, w.limit)
}
