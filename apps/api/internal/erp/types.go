// Package erp owns the ERP-integration domain (Tiny order-as-reservation, stock
// launch, finalisation state machine). Bloco B2 strangles this logic out of
// internal/integration slice by slice; B2a lays the foundation (package + ports
// + pure domain types) additively — no behaviour is moved yet.
//
// Import rule (sacred): erp MUST NOT import internal/integration (the package),
// otherwise integration→erp becomes a cycle. erp may import integration/providers
// (pure types). integration imports erp and adapts its concrete types into the
// neutral shapes declared here — the same "wired at boot to break the import
// cycle" approach already used by ERPOrderMirror.
package erp

import (
	"context"
	"errors"
	"time"
)

// ErrCartNotConverted signals to the caller (paid-webhook / finalisation) that
// the cart has no ERP order-as-reservation — the flow must fall back to the
// legacy finalisation path. Canonical home is this package; internal/integration
// keeps a backward-compatible alias while its logic is strangled out (Bloco B2).
var ErrCartNotConverted = errors.New("cart não convertido em pedido ERP")

// ERP order state machine (carts.erp_order_state column):
//
//	none → converting → open ⇄ mutating → confirmed
//	                      └──────────────→ cancelled
//
// Sacred rule: 'converting' NEVER goes back to 'none' — the in-flight call may
// have succeeded server-side, so resetting would reopen the legacy path and
// double the stock write. Canonical home is this package; integration aliases
// these to its existing unexported names to avoid churning ~30 call sites.
const (
	OrderStateNone       = "none"
	OrderStateConverting = "converting"
	OrderStateOpen       = "open"
	OrderStateMutating   = "mutating"
	OrderStateConfirmed  = "confirmed"
	OrderStateCancelled  = "cancelled"
)

// ERPOrderMirror projects ERP state changes into the Order aggregate.
// Implemented by order/listeners.Listener; wired at boot to break the import cycle.
type ERPOrderMirror interface {
	MirrorCartERPToOrder(ctx context.Context, cartID string)
}

// Integration is the ERP-domain view of an active integration row. It is the
// return type of ERPRepository.GetActiveByProvider so this package never has to
// import internal/integration (which owns the concrete IntegrationRow). The
// integration package supplies a thin adapter (Bloco B2b) that copies the row
// into this neutral shape. Mirror of integration.IntegrationRow — deliberate
// port DTO, not duplicated business logic.
type Integration struct {
	ID             string
	StoreID        string
	Type           string
	Provider       string
	Status         string
	Credentials    []byte // encrypted
	TokenExpiresAt *time.Time
	Metadata       map[string]any
	LastSyncedAt   *time.Time
	CreatedAt      time.Time
	Priority       int
}
