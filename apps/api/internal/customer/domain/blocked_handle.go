package domain

import "time"

// BlockedHandle is a platform handle a store has blocked from purchasing.
type BlockedHandle struct {
	id           string
	handle       string
	reason       *string
	blockedAt    time.Time
	unblockedAt  *time.Time
	blockedByID  *string
	cartsRemoved int
}

// ReconstructBlockedHandle rebuilds a BlockedHandle from persistence.
func ReconstructBlockedHandle(
	id string,
	handle string,
	reason *string,
	blockedAt time.Time,
	unblockedAt *time.Time,
	blockedByID *string,
) *BlockedHandle {
	return &BlockedHandle{
		id:          id,
		handle:      handle,
		reason:      reason,
		blockedAt:   blockedAt,
		unblockedAt: unblockedAt,
		blockedByID: blockedByID,
	}
}

func (b *BlockedHandle) ID() string              { return b.id }
func (b *BlockedHandle) Handle() string          { return b.handle }
func (b *BlockedHandle) Reason() *string         { return b.reason }
func (b *BlockedHandle) BlockedAt() time.Time    { return b.blockedAt }
func (b *BlockedHandle) UnblockedAt() *time.Time { return b.unblockedAt }
func (b *BlockedHandle) BlockedByID() *string    { return b.blockedByID }
func (b *BlockedHandle) CartsRemoved() int       { return b.cartsRemoved }

// SetCartsRemoved records how many open carts were cancelled as a side effect
// of the block. It is not persisted; it enriches the immediate response only.
func (b *BlockedHandle) SetCartsRemoved(n int) { b.cartsRemoved = n }
