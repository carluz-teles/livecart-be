package integration

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
)

// TrackingPoller periodically pulls carrier tracking for active shipments and
// emits `delivered` events on the customer-facing timeline when the carrier
// reports the shipment as delivered.
//
// Today only SmartEnvios is supported — Melhor Envio's read-only integration
// doesn't expose tracking. Adding a second provider is two lines: add the
// provider to the `providers` slice and ensure ListShipmentsForPolling
// returns its rows.
type TrackingPoller struct {
	service  *Service
	logger   *zap.Logger
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// TrackingPollerConfig is the input to NewTrackingPoller.
type TrackingPollerConfig struct {
	Service  *Service
	Logger   *zap.Logger
	Interval time.Duration // how often to poll (default 6h)
}

// supportedProviders are the carriers that have TrackShipment implemented and
// a status mapping that can resolve "delivered". See providers/social/* and
// providers/shipping/* for details.
var supportedTrackingProviders = []providers.ProviderName{
	providers.ProviderSmartEnvios,
}

func NewTrackingPoller(cfg TrackingPollerConfig) *TrackingPoller {
	interval := cfg.Interval
	if interval == 0 {
		interval = 6 * time.Hour
	}
	return &TrackingPoller{
		service:  cfg.Service,
		logger:   cfg.Logger.Named("tracking-poller"),
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

func (w *TrackingPoller) Start() {
	w.wg.Add(1)
	go w.run()
	w.logger.Info("tracking poller started", zap.Duration("interval", w.interval))
}

func (w *TrackingPoller) Stop() {
	close(w.stopCh)
	w.wg.Wait()
	w.logger.Info("tracking poller stopped")
}

func (w *TrackingPoller) run() {
	defer w.wg.Done()

	// Stagger the first poll so a freshly booted server doesn't slam the
	// carrier API the moment the deploy completes.
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			w.pollAll()
			timer.Reset(w.interval)
		case <-w.stopCh:
			return
		}
	}
}

func (w *TrackingPoller) pollAll() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	for _, name := range supportedTrackingProviders {
		w.pollProvider(ctx, name)
	}
}

func (w *TrackingPoller) pollProvider(ctx context.Context, providerName providers.ProviderName) {
	rows, err := w.service.repo.ListShipmentsForPolling(ctx, string(providerName), 200)
	if err != nil {
		logger.From(ctx, w.logger).Warn("failed to list shipments for polling",
			zap.String("provider", string(providerName)),
			zap.Error(err),
		)
		return
	}
	if len(rows) == 0 {
		return
	}

	logger.From(ctx, w.logger).Info("polling shipments",
		zap.String("provider", string(providerName)),
		zap.Int("count", len(rows)),
	)

	for _, sh := range rows {
		w.pollShipment(ctx, providerName, sh)
	}
}

func (w *TrackingPoller) pollShipment(ctx context.Context, providerName providers.ProviderName, sh *ShipmentRow) {
	ctx = logger.WithStore(ctx, sh.StoreID, "")
	osp, err := w.service.GetShippingOrderProvider(ctx, sh.StoreID, providerName)
	if err != nil {
		// Integration may have been disconnected between shipment creation
		// and now — common during poller's lifetime. Skip without raising.
		logger.From(ctx, w.logger).Debug("provider unavailable for shipment, skipping",
			zap.String("shipment_id", sh.ID),
			zap.Error(err),
		)
		return
	}

	// Prefer the carrier-issued provider_order_id, fall back to tracking_code.
	// The TrackShipmentRequest contract is "exactly ONE field set" — sending
	// multiple values may yield ambiguous lookups on some carriers.
	req := providers.TrackShipmentRequest{}
	if sh.ProviderOrderID != "" {
		req.ProviderOrderID = sh.ProviderOrderID
	} else {
		req.TrackingCode = sh.TrackingCode
	}
	result, err := osp.TrackShipment(ctx, req)
	if err != nil {
		logger.From(ctx, w.logger).Warn("track shipment failed",
			zap.String("shipment_id", sh.ID),
			zap.String("tracking_code", sh.TrackingCode),
			zap.Error(err),
		)
		return
	}
	if result == nil {
		return
	}

	// Persist new events. InsertTrackingEvents dedupes against
	// (shipment_id, event_at, raw_code) — re-polling is safe.
	events := make([]TrackingEventInput, 0, len(result.Events))
	for _, ev := range result.Events {
		events = append(events, TrackingEventInput{
			Status:      string(ev.Status),
			RawCode:     ev.RawCode,
			RawName:     ev.RawName,
			Observation: ev.Observation,
			EventAt:     ev.EventAt,
			Source:      "poll",
		})
	}
	if len(events) > 0 {
		if err := w.service.repo.InsertTrackingEvents(ctx, sh.ID, events); err != nil {
			logger.From(ctx, w.logger).Warn("failed to persist tracking events",
				zap.String("shipment_id", sh.ID),
				zap.Error(err),
			)
		}
	}

	// Update the shipment row with the latest status snapshot. We pull the
	// most recent event and keep the existing tracking_code intact.
	currentStatus := string(result.CurrentStatus)
	if currentStatus != "" && currentStatus != sh.Status {
		var lastRawCode int
		var lastRawName string
		if len(result.Events) > 0 {
			last := result.Events[len(result.Events)-1]
			lastRawCode = last.RawCode
			lastRawName = last.RawName
		}
		if err := w.service.repo.UpdateShipmentStatus(ctx, sh.ID, currentStatus, lastRawCode, lastRawName, sh.TrackingCode); err != nil {
			logger.From(ctx, w.logger).Warn("failed to update shipment status",
				zap.String("shipment_id", sh.ID),
				zap.Error(err),
			)
		}
	}

	// Fire the delivered hook when the carrier reports terminal delivery.
	// postcheckoutHook handles its own idempotency (one delivered event per
	// cart), so re-polling after the first hit is a no-op.
	if result.CurrentStatus == providers.TrackingStatusDelivered && w.service.postCheckoutHook != nil {
		w.service.postCheckoutHook.OnDelivered(ctx, sh.OrderID, "system")
	}
}
