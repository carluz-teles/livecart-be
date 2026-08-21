package domain

import "time"

// VipHandle is a platform handle a store marked as a VIP customer: their cart
// never expires and accumulates items across events until paid or cancelled.
// The conceptual opposite of BlockedHandle.
type VipHandle struct {
	id           string
	handle       string
	addedAt      time.Time
	removedAt    *time.Time
	addedByID    *string
	cartsUpdated int
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

// SetCartsUpdated records how many existing open carts were turned eternal as a
// side effect of adding the VIP. Not persisted; enriches the response only.
func (v *VipHandle) SetCartsUpdated(n int) { v.cartsUpdated = n }
