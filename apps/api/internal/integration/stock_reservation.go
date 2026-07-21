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
	StockOpCartAdd         StockOp = "cart_add"
	StockOpQtyIncrease     StockOp = "qty_increase"
	StockOpQtyDecrease     StockOp = "qty_decrease"
	StockOpWaitlistPromote StockOp = "waitlist_promote"
	StockOpCartRecovery    StockOp = "cart_recovery"
	StockOpRecoveryRelease StockOp = "recovery_release"
	StockOpCartExpiry      StockOp = "cart_expiry"
	StockOpCancelBlocked   StockOp = "cancel_blocked"
	StockOpWaitlistCancel  StockOp = "waitlist_cancel"
	StockOpWaitlistExpire  StockOp = "waitlist_expire"
)

// stockReservationRepo is the narrow slice of the repository the manager needs.
// Keeping it small makes the manager unit-testable and states its dependencies
// explicitly (interface segregation).
type stockReservationRepo interface {
	DecrementProductStock(ctx context.Context, productID string, quantity int) error
	TryDecrementProductStock(ctx context.Context, productID string, quantity int) (bool, error)
	DecrementProductStockUpTo(ctx context.Context, productID string, want int) (int, error)
	IncrementProductStock(ctx context.Context, productID string, quantity int) error
	EmitStockReserved(ctx context.Context, p StockEventParams) error
	EmitStockReleased(ctx context.Context, p StockEventParams) error
}

// StockReservations is the SINGLE maintenance point for reserving and releasing
// LOCAL product stock. Every domain reservation change funnels through it so the
// stock mutation and its canonical event (stock.reserved / stock.released) live
// in one place instead of scattered across ~8 call sites. Callers declare WHICH
// operation they perform (the op); the manager owns the mutation + emission.
//
// Rollbacks — undoing a just-failed operation — intentionally bypass it: they
// are corrections, not domain movements, and must not emit.
//
// The emitted event is a best-effort side-effect: the reserve/release methods
// return the raw mutation error UNCHANGED so callers' error handling is
// preserved, and a failed emit is logged, never surfaced.
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

// Reserve decrements local stock and emits stock.reserved.
func (m *StockReservations) Reserve(ctx context.Context, p ReserveParams) error {
	if err := m.repo.DecrementProductStock(ctx, p.ProductID, p.Quantity); err != nil {
		return err
	}
	m.NoteReserved(ctx, p)
	return nil
}

// TryReserve decrements all-or-nothing and emits only when stock was actually
// taken. Mirrors Repository.TryDecrementProductStock.
func (m *StockReservations) TryReserve(ctx context.Context, p ReserveParams) (bool, error) {
	ok, err := m.repo.TryDecrementProductStock(ctx, p.ProductID, p.Quantity)
	if err != nil || !ok {
		return ok, err
	}
	m.NoteReserved(ctx, p)
	return true, nil
}

// ReserveUpTo takes up to p.Quantity units, emits stock.reserved for the amount
// actually taken (when > 0) and returns it. Mirrors DecrementProductStockUpTo.
func (m *StockReservations) ReserveUpTo(ctx context.Context, p ReserveParams) (int, error) {
	taken, err := m.repo.DecrementProductStockUpTo(ctx, p.ProductID, p.Quantity)
	if err != nil || taken <= 0 {
		return taken, err
	}
	q := p
	q.Quantity = taken
	m.NoteReserved(ctx, q)
	return taken, nil
}

// Release increments local stock and emits stock.released.
func (m *StockReservations) Release(ctx context.Context, p ReleaseParams) error {
	if err := m.repo.IncrementProductStock(ctx, p.ProductID, p.Quantity); err != nil {
		return err
	}
	m.NoteReleased(ctx, p)
	return nil
}

// NoteReserved / NoteReleased emit the event WITHOUT mutating stock, for callers
// that own their stock mutation (and its rollback) directly — currently
// AdjustStockReservationDelta, whose optional ERP step can roll the local change
// back, so the event must fire only at the definitive success point rather than
// at mutation time.
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
