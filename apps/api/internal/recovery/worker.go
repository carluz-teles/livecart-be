// Package recovery implements the WhatsApp cart-recovery worker (PRD 006
// sprint 3). It is the "active expirer" counterpart to the lazy cart
// expiration: it sweeps expired, unpaid carts with phone + consent,
// regenerates the checkout (existing order-service flow, normal stock rules)
// and sends the approved recovery template via the store's WhatsApp number.
//
// Rules (PRD 006 §4):
//   - never extends expiration just to message: recovery happens after the
//     cart expired naturally, via RegenerateCheckout
//   - one attempt per cart (NOT EXISTS on notification_logs in the sweep;
//     failures are logged so they are not retried in a loop)
//   - quiet hours: skipped rows simply wait for a later sweep
//   - opted-out customers are excluded in the sweep
//
// Worker pattern mirrors coupon.RedemptionExpirerWorker (ticker + stop
// channel + WaitGroup). Lives in its own package because it crosses the
// order + integration + notification domains.
package recovery

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/billing"
	"livecart/apps/api/internal/integration"
	"livecart/apps/api/internal/notification"
	"livecart/apps/api/internal/order"
	"livecart/apps/api/lib/config"
)

// saoPaulo is the reference timezone for quiet hours. Stores are BR-focused;
// per-store timezones can come later without touching the sweep.
var saoPaulo = func() *time.Location {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		return time.FixedZone("BRT", -3*60*60)
	}
	return loc
}()

// Worker periodically recovers expired carts via WhatsApp.
type Worker struct {
	queries      *sqlc.Queries
	orders       *order.Service
	integrations *integration.Service
	billing      *billing.Service // optional paywall gate (PRD 007)
	logger       *zap.Logger
	interval     time.Duration
	limit        int32
	stopCh       chan struct{}
	wg           sync.WaitGroup
}

// WorkerConfig configures the recovery worker.
type WorkerConfig struct {
	Queries      *sqlc.Queries
	Orders       *order.Service
	Integrations *integration.Service
	Billing      *billing.Service
	Logger       *zap.Logger
	Interval     time.Duration // default: 5 minutes
	Limit        int32         // rows per sweep, default: 100
}

// NewWorker creates the recovery worker.
func NewWorker(cfg WorkerConfig) *Worker {
	interval := cfg.Interval
	if interval == 0 {
		interval = 5 * time.Minute
	}
	limit := cfg.Limit
	if limit == 0 {
		limit = 100
	}
	return &Worker{
		queries:      cfg.Queries,
		orders:       cfg.Orders,
		integrations: cfg.Integrations,
		billing:      cfg.Billing,
		logger:       cfg.Logger,
		interval:     interval,
		limit:        limit,
		stopCh:       make(chan struct{}),
	}
}

// Start launches the background sweep loop.
func (w *Worker) Start() {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
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
	}()
	w.logger.Info("whatsapp recovery worker started",
		zap.Duration("interval", w.interval),
		zap.Int32("limit", w.limit),
	)
}

// Stop shuts the worker down and waits for an in-flight sweep.
func (w *Worker) Stop() {
	close(w.stopCh)
	w.wg.Wait()
}

func (w *Worker) sweep() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	rows, err := w.queries.ListCartsForWhatsAppRecovery(ctx, w.limit)
	if err != nil {
		w.logger.Error("recovery sweep query failed", zap.Error(err))
		return
	}
	if len(rows) == 0 {
		return
	}

	now := time.Now().In(saoPaulo)
	recovered, skipped := 0, 0
	for _, row := range rows {
		if inQuietHours(now, int(row.QuietHoursStart), int(row.QuietHoursEnd)) {
			skipped++
			continue // a later sweep picks it up after the quiet window
		}
		// Paywall (PRD 007): no recovery messages for blocked stores. The
		// cart stays eligible for when the subscription is regularized.
		if w.billing != nil && w.billing.IsStoreBlocked(ctx, uuidString(row.StoreID)) {
			skipped++
			continue
		}
		if err := w.processCart(ctx, row); err != nil {
			w.logger.Warn("cart recovery attempt failed",
				zap.String("cart_id", uuidString(row.ID)),
				zap.Error(err),
			)
		} else {
			recovered++
		}
	}

	w.logger.Info("whatsapp recovery sweep done",
		zap.Int("candidates", len(rows)),
		zap.Int("sent", recovered),
		zap.Int("quiet_hours_skipped", skipped),
	)
}

// processCart regenerates the checkout and sends the recovery template. Every
// outcome writes a notification_logs row so the sweep's NOT EXISTS clause
// never retries the same cart in a loop.
func (w *Worker) processCart(ctx context.Context, row sqlc.ListCartsForWhatsAppRecoveryRow) error {
	cartID := uuidString(row.ID)
	storeID := uuidString(row.StoreID)
	phone := row.CustomerPhone.String

	// Re-check + regenerate under the existing order rules (rejects paid
	// carts and carts with shipments; re-reserves stock under normal rules).
	token, _, err := w.orders.RegenerateCheckout(ctx, cartID, storeID)
	if err != nil {
		w.log(ctx, row, "skipped", "", fmt.Sprintf("regenerate: %v", err))
		return fmt.Errorf("regenerating checkout: %w", err)
	}

	link := strings.TrimRight(config.FrontendURL.String(), "/") + "/cart/" + token
	vars := map[string]string{
		"1": firstName(row.CustomerName.String),
		"2": fmt.Sprintf("%d %s · %s", row.TotalItems, pluralItem(row.TotalItems), notification.FormatCurrency(row.TotalCents)),
		"3": link,
	}

	logID := w.log(ctx, row, "pending", fmt.Sprintf("cart_recovery vars=%v", vars), "")

	_, err = w.integrations.SendWhatsAppTemplate(ctx, integration.SendWhatsAppTemplateInput{
		StoreID:           storeID,
		To:                phone,
		Variables:         vars,
		NotificationLogID: logID,
	})
	if err != nil {
		w.updateLog(ctx, logID, "failed", err.Error())
		return fmt.Errorf("sending recovery template: %w", err)
	}

	w.updateLog(ctx, logID, "sent", "")
	w.logger.Info("cart recovery message sent",
		zap.String("cart_id", cartID),
		zap.String("store_id", storeID),
	)
	return nil
}

// log writes a notification_logs row and returns its ID ("" on failure).
func (w *Worker) log(ctx context.Context, row sqlc.ListCartsForWhatsAppRecoveryRow, status, message, errMsg string) string {
	created, err := w.queries.CreateNotificationLog(ctx, sqlc.CreateNotificationLogParams{
		StoreID:          row.StoreID,
		EventID:          row.EventID,
		CartID:           row.ID,
		PlatformUserID:   row.CustomerPhone.String, // phone doubles as the user key on this channel
		PlatformHandle:   row.CustomerName,
		NotificationType: string(notification.TypeCartRecovery),
		Channel:          string(notification.ChannelWhatsApp),
		Status:           status,
		MessageText:      pgtype.Text{String: message, Valid: message != ""},
	})
	if err != nil {
		w.logger.Warn("failed to create recovery log", zap.Error(err))
		return ""
	}
	if errMsg != "" {
		w.updateLog(ctx, uuidString(created.ID), status, errMsg)
	}
	return uuidString(created.ID)
}

func (w *Worker) updateLog(ctx context.Context, logID, status, errMsg string) {
	if logID == "" {
		return
	}
	var id pgtype.UUID
	if err := id.Scan(logID); err != nil {
		return
	}
	if err := w.queries.UpdateNotificationLogStatus(ctx, sqlc.UpdateNotificationLogStatusParams{
		ID:           id,
		Status:       status,
		SentAt:       pgtype.Timestamptz{Time: time.Now(), Valid: status == "sent"},
		ErrorMessage: pgtype.Text{String: errMsg, Valid: errMsg != ""},
	}); err != nil {
		w.logger.Warn("failed to update recovery log", zap.Error(err))
	}
}

// inQuietHours reports whether the local hour falls inside the [start, end)
// overnight window (e.g. 21→8).
func inQuietHours(now time.Time, start, end int) bool {
	h := now.Hour()
	if start == end {
		return false // window disabled
	}
	if start < end {
		return h >= start && h < end
	}
	return h >= start || h < end // overnight wrap (21 → 8)
}

func firstName(full string) string {
	name := strings.TrimSpace(full)
	if name == "" {
		return "Cliente"
	}
	if i := strings.IndexByte(name, ' '); i > 0 {
		return name[:i]
	}
	return name
}

func pluralItem(n int32) string {
	if n == 1 {
		return "item"
	}
	return "itens"
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
