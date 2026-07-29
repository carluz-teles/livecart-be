package integration

import (
	"context"

	"go.uber.org/zap"

	"livecart/apps/api/lib/logger"
)

// StockOp labels the business operation behind a stock reservation change. It
// travels in the event payload and dedup key so consumers can tell a cart-add
// reservation from a waitlist promotion or an expiry release apart.
type StockOp string

const (
	// StockOpUnspecified lets a caller of AdjustStockReservationDelta defer to
	// the default sign-based label (qty_increase / qty_decrease).
	StockOpUnspecified StockOp = ""

	StockOpCartAdd         StockOp = "cart_add"
	StockOpQtyIncrease     StockOp = "qty_increase"
	StockOpQtyDecrease     StockOp = "qty_decrease"
	StockOpWaitlistPromote StockOp = "waitlist_promote"
	StockOpCartExpiry      StockOp = "cart_expiry"
	StockOpCancelBlocked   StockOp = "cancel_blocked"
	// StockOpCartCancelled: cancelamento MANUAL do carrinho pelo lojista.
	// StockOpCancelReverted: o pagamento venceu esse cancelamento e o estoque
	// devolvido foi retomado (pode deixar o saldo negativo — venda real).
	StockOpCartCancelled  StockOp = "cart_cancelled"
	StockOpCancelReverted StockOp = "cancel_reverted"
	StockOpWaitlistCancel StockOp = "waitlist_cancel"
	StockOpWaitlistExpire StockOp = "waitlist_expire"
)

// stockReservationRepo is the narrow slice of the repository the manager needs.
// Keeping it small makes the manager unit-testable and states its dependencies
// explicitly (interface segregation).
type stockReservationRepo interface {
	IncrementProductStock(ctx context.Context, productID string, quantity int) error
	EmitStockReserved(ctx context.Context, p StockEventParams) error
	EmitStockReleased(ctx context.Context, p StockEventParams) error
}

// StockReservations is the maintenance point for the stock.reserved /
// stock.released events over LOCAL product stock. Callers declare WHICH
// operation they perform (the op) and this owns the emission, so the semantics
// are not scattered across the ~8 stock call sites.
//
// Three emission modes, by design (not accident):
//
//  1. Terminal release (expiry-fallback, cancel, blocked, recovery release):
//     Release() mutates local stock and emits — safe because giving stock back
//     is never rolled back.
//  2. Provisional reserve (cart_add, waitlist promote, recovery re-secure): the
//     caller does the raw decrement itself (it owns the rollback) and calls
//     NoteReserved ONLY at the definitive success point. Emitting at decrement
//     time would orphan the event when the op rolls back.
//  3. Transactional flows (cart expiry in ExpireCartAndReleaseStock): the stock
//     mutation and the event must commit atomically in the SAME tx, so those
//     emit via the repo's emitStockEvent on the tx handle rather than through
//     this best-effort component. emitStockEvent is the single event SHAPE for
//     all three modes.
//
// Rollbacks that undo a just-failed op bypass this entirely: they are
// corrections, not domain movements, and must not emit.
//
// The emitted event is best-effort: Release returns the raw mutation error
// UNCHANGED so callers' error handling is preserved, and a failed emit is
// logged, never surfaced.
type StockReservations struct {
	repo   stockReservationRepo
	logger *zap.Logger
}

// NewStockReservations builds the manager over the repository slice it needs.
func NewStockReservations(repo stockReservationRepo, logger *zap.Logger) *StockReservations {
	return &StockReservations{repo: repo, logger: logger}
}

// ReserveParams describes a reservation (or release) of Quantity units of a
// product. ReservationID is the ERP reservation row id when one exists
// (design-A stores); it becomes the reserved event's idempotency key.
type ReserveParams struct {
	Op            StockOp
	ProductID     string
	Quantity      int
	CartID        string
	EventID       string
	ReservationID string
}

// ReleaseParams is the same shape as ReserveParams.
type ReleaseParams = ReserveParams

// Release increments local stock and emits stock.released. Releases are terminal
// (giving stock back is never rolled back), so mutating + emitting together here
// is safe.
func (m *StockReservations) Release(ctx context.Context, p ReleaseParams) error {
	if err := m.repo.IncrementProductStock(ctx, p.ProductID, p.Quantity); err != nil {
		return err
	}
	m.NoteReleased(ctx, p)
	return nil
}

// NoteReserved / NoteReleased emit the event WITHOUT mutating stock.
//
// Reserves are PROVISIONAL: cart-add, waitlist promotion and cart recovery all
// decrement stock and then may roll it back on a later failure. Emitting
// stock.reserved at decrement time would orphan the event when the operation
// rolls back, so those callers do the raw decrement themselves and call
// NoteReserved only at the definitive success point (with the real cart_id).
// AdjustStockReservationDelta likewise owns its local mutation + rollback and
// notes at its guarded success point. There is deliberately no immediate-emit
// Reserve helper — it would invite the orphan bug.
func (m *StockReservations) NoteReserved(ctx context.Context, p ReserveParams) {
	if err := m.repo.EmitStockReserved(ctx, p.toEventParams()); err != nil {
		logger.From(ctx, m.logger).Warn("failed to emit stock.reserved",
			zap.String("product_id", p.ProductID), zap.String("op", string(p.Op)), zap.Error(err))
	}
}

func (m *StockReservations) NoteReleased(ctx context.Context, p ReleaseParams) {
	if err := m.repo.EmitStockReleased(ctx, p.toEventParams()); err != nil {
		logger.From(ctx, m.logger).Warn("failed to emit stock.released",
			zap.String("product_id", p.ProductID), zap.String("op", string(p.Op)), zap.Error(err))
	}
}

func (p ReserveParams) toEventParams() StockEventParams {
	return StockEventParams{
		Op:            string(p.Op),
		ProductID:     p.ProductID,
		Quantity:      p.Quantity,
		CartID:        p.CartID,
		EventID:       p.EventID,
		ReservationID: p.ReservationID,
	}
}
