package inventory

import "time"

// This file holds the neutral DTOs the Inventory service and its persistence
// port exchange. The strangler-fig migration (Bloco B3) pulls the waitlist/fila
// logic out of internal/integration slice by slice; B3a lays the foundation
// (Service + port) and moves two low-blast-radius methods.
//
// Import rule (sacred, same as internal/erp): inventory MUST NOT import
// internal/integration (the package) — that would form a cycle, since
// integration imports inventory to satisfy the port and alias these DTOs. The
// canonical home of the waitlist projections is now this package; integration
// aliases them so the Repository (which owns the SQL) keeps compiling without
// churn (padrão erp B2b).

// WaitlistItemRow is a single waitlist entry projection. Canonical home is this
// package (Bloco B3a); internal/integration aliases it so the repository (which
// builds it from sqlc) and the B3b flows still in integration keep compiling.
type WaitlistItemRow struct {
	ID             string
	EventID        string
	ProductID      string
	PlatformUserID string
	PlatformHandle string
	Quantity       int
	Position       int
	Status         string
	CartID         string
	NotifiedAt     *time.Time
	ExpiresAt      *time.Time
}

// ListActiveByCartRow is the projection returned to the public checkout ("produtos
// em fila"). Canonical home is this package (Bloco B3a); internal/integration
// aliases it so the checkout call sites and the delegation keep compiling.
type ListActiveByCartRow struct {
	ID              string
	EventID         string
	ProductID       string
	ProductName     string
	ProductKeyword  string
	ProductImageURL string
	ProductPrice    int64
	Quantity        int
	Position        int
	Status          string
	NotifiedAt      *time.Time
	ExpiresAt       *time.Time
	CreatedAt       *time.Time
}

// CartRef is the slim cart view the waitlist-cancel flow needs — just the owning
// store and the buyer handle. It deliberately does NOT mirror the full
// integration CartRow (which stays integration-owned to avoid the import cycle);
// the boot-wired adapter maps CartRow → CartRef, mirroring erp.ShipmentInvoiceRef.
// A nil *CartRef means the cart is gone (the release-fallback branch).
type CartRef struct {
	StoreID        string
	PlatformHandle string
}
