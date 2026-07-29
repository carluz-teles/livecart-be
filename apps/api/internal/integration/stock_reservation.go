package integration

import (
	"go.uber.org/zap"

	"livecart/apps/api/internal/erp"
)

// The stock.reserved / stock.released component and its types now live in
// internal/erp (Bloco B2b). These in-package aliases keep the remaining
// integration call sites — checkout, waitlist promotion, cancel, expiry — and
// the tests compiling by their existing names. The cleanup (call sites pointing
// straight at erp) is B2e.
// StockEventParams is aliased in repository.go (next to the emission code).
type (
	StockOp           = erp.StockOp
	ReserveParams     = erp.ReserveParams
	ReleaseParams     = erp.ReleaseParams
	StockReservations = erp.StockReservations
)

const (
	StockOpUnspecified     = erp.StockOpUnspecified
	StockOpCartAdd         = erp.StockOpCartAdd
	StockOpQtyIncrease     = erp.StockOpQtyIncrease
	StockOpQtyDecrease     = erp.StockOpQtyDecrease
	StockOpWaitlistPromote = erp.StockOpWaitlistPromote
	StockOpCartExpiry      = erp.StockOpCartExpiry
	StockOpCancelBlocked   = erp.StockOpCancelBlocked
	StockOpWaitlistCancel  = erp.StockOpWaitlistCancel
	StockOpWaitlistExpire  = erp.StockOpWaitlistExpire
)

// NewStockReservations builds the erp stock manager over the Repository. Kept as
// an in-package helper so the remaining integration call sites and the tests
// keep constructing it by the old name.
func NewStockReservations(repo *Repository, logger *zap.Logger) *StockReservations {
	return erp.NewStockReservations(repo, logger)
}
