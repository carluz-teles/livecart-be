package domain

import "time"

// VipHandle is a platform handle a store marked as a VIP customer: their cart
// never expires and accumulates items across events until paid or cancelled.
// The conceptual opposite of BlockedHandle.
type VipHandle struct {
	id               string
	handle           string
	addedAt          time.Time
	removedAt        *time.Time
	addedByID        *string
	cartsUpdated     int
	cartsMerged      int
	cartsSkipped     int
	activationFailed bool
}

// ReconstructVipHandle rebuilds a VipHandle from persistence.
func ReconstructVipHandle(
	id string,
	handle string,
	addedAt time.Time,
	removedAt *time.Time,
	addedByID *string,
) *VipHandle {
	return &VipHandle{
		id:        id,
		handle:    handle,
		addedAt:   addedAt,
		removedAt: removedAt,
		addedByID: addedByID,
	}
}

func (v *VipHandle) ID() string            { return v.id }
func (v *VipHandle) Handle() string        { return v.handle }
func (v *VipHandle) AddedAt() time.Time    { return v.addedAt }
func (v *VipHandle) RemovedAt() *time.Time { return v.removedAt }
func (v *VipHandle) AddedByID() *string    { return v.addedByID }
func (v *VipHandle) CartsUpdated() int     { return v.cartsUpdated }
func (v *VipHandle) CartsMerged() int      { return v.cartsMerged }
func (v *VipHandle) CartsSkipped() int     { return v.cartsSkipped }

// ActivationFailed diz que a promoção foi gravada mas a consolidação dos
// carrinhos que o comprador já tinha não rodou. Compras futuras caem no
// carrinho eterno; as antigas continuam com prazo — e é por isso que este
// estado precisa chegar à tela em vez de morrer no log.
func (v *VipHandle) ActivationFailed() bool { return v.activationFailed }

// SetCartsUpdated records how many existing open carts were turned eternal as a
// side effect of adding the VIP. Not persisted; enriches the response only.
func (v *VipHandle) SetCartsUpdated(n int) { v.cartsUpdated = n }

// SetCartsMerged records how many open carts gave up their contents to the
// eternal one. Not persisted.
func (v *VipHandle) SetCartsMerged(n int) { v.cartsMerged = n }

// SetCartsSkipped records how many open carts were left out of the merge for
// already having an ERP order. Not persisted.
func (v *VipHandle) SetCartsSkipped(n int) { v.cartsSkipped = n }

// SetActivationFailed marks that the cart consolidation did not run.
func (v *VipHandle) SetActivationFailed() { v.activationFailed = true }
