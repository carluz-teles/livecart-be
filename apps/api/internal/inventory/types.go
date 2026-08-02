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

// ProductRef is the enxuto product view the waitlist promotion needs — just the
// price, name and keyword the promoted cart item + notification DM require. It
// deliberately does NOT mirror the full integration ProductRow (ID/Stock/
// ExternalID stay integration-owned); the boot-wired adapter maps ProductRow →
// ProductRef, mirroring CartRef / erp.ShipmentInvoiceRef. A nil *ProductRef means
// the product is gone (the promotion aborts and reverts). Canonical home is this
// package (Bloco B3b).
type ProductRef struct {
	Price   int64
	Name    string
	Keyword string
}

// CartExpirySnapshot holds the expiry-relevant fields of a cart — the store, the
// cart/payment status and the expires_at window. The waitlist core reads it to
// decide whether a cart is terminal (paid/expired/cancelled) before promoting or
// expiring against it. Canonical home is this package (Bloco B3b);
// internal/integration aliases it so the repository (which builds it from sqlc)
// and the ScheduleExpiry/RunScheduledExpiry that stay integration-owned keep
// compiling.
type CartExpirySnapshot struct {
	StoreID       string
	Status        string
	PaymentStatus string
	ExpiresAt     *time.Time
}

// EmitWaitlistNotifiedParams is the payload of the waitlist.notified fact, emitted
// at the promotion's definitive success point. Canonical home is this package
// (Bloco B3b); internal/integration aliases it so the repository emitter keeps
// compiling.
type EmitWaitlistNotifiedParams struct {
	WaitlistItemID string
	EventID        string
	ProductID      string
	CartID         string
	Quantity       int
	Remaining      int
}

// ExpireCartResult is the outcome of the atomic expire-and-release transaction.
// Eligible=false means the guard-first flip returned 0 rows (paid/terminal in the
// gap) — the caller aborts without touching the ERP. FreedProductIDs are the
// products whose local stock was returned (waitlist promotion targets, post-
// commit). Canonical home is this package (Bloco B3b); internal/integration
// aliases it so the repository transaction keeps compiling.
type ExpireCartResult struct {
	Eligible        bool
	EventID         string
	FreedProductIDs []string
}

// WaitlistNotifiedInput is the neutral payload the promotion flow hands to
// NotifyWaitlistPromoted (the "produto liberou" DM). It mirrors the integration-
// owned sendWaitlistNotifiedInput field-for-field; the collaborator maps it back
// and sends the DM (the notification wiring stays integration-owned — NIL-GUARD
// LAZY, read s.notificationService at call time).
type WaitlistNotifiedInput struct {
	StoreID        string
	EventID        string
	EventTitle     string
	CartID         string
	CartToken      string
	PlatformUserID string
	PlatformHandle string
	ProductName    string
	ProductKeyword string
	Quantity       int
	TTL            time.Duration
}
