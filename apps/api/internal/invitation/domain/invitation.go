package domain

import (
	"errors"
	"time"

	vo "livecart/apps/api/lib/valueobject"
)

const (
	// DefaultExpirationDays is the number of days an invitation is valid
	DefaultExpirationDays = 7
)

// Domain errors
var (
	ErrInvitationExpired       = errors.New("invitation has expired")
	ErrInvitationNotPending    = errors.New("invitation is not pending")
	ErrInvitationAlreadyExists = errors.New("invitation already exists for this email")
	ErrEmailMismatch           = errors.New("invitation email does not match your account")
	ErrCannotResendNonPending  = errors.New("can only resend pending invitations")
)

// Invitation represents an invitation to join a store.
type Invitation struct {
	id          vo.InvitationID
	storeID     vo.StoreID
	email       vo.Email
	role        vo.Role
	token       InvitationToken
	status      InvitationStatus
	invitedBy   vo.MemberID
	inviterName *string
	storeName   string
	storeSlug   string
	expiresAt   time.Time
	acceptedAt  *time.Time
	createdAt   time.Time
}

// NewInvitation creates a new Invitation entity.
func NewInvitation(
	storeID vo.StoreID,
	email vo.Email,
	role vo.Role,
	invitedBy vo.MemberID,
) (*Invitation, error) {
	token, err := GenerateToken()
	if err != nil {
		return nil, err
	}

	return &Invitation{
		id:        vo.GenerateInvitationID(),
		storeID:   storeID,
		email:     email,
		role:      role,
		token:     token,
		status:    StatusPending,
		invitedBy: invitedBy,
		expiresAt: time.Now().Add(time.Hour * 24 * DefaultExpirationDays),
		createdAt: time.Now(),
	}, nil
}

// Reconstruct rebuilds an Invitation from persistence data.
func Reconstruct(
	id vo.InvitationID,
	storeID vo.StoreID,
	email vo.Email,
	role vo.Role,
	token InvitationToken,
	status InvitationStatus,
	invitedBy vo.MemberID,
	inviterName *string,
	storeName string,
	storeSlug string,
	expiresAt time.Time,
	acceptedAt *time.Time,
	createdAt time.Time,
) *Invitation {
	return &Invitation{
		id:          id,
		storeID:     storeID,
		email:       email,
		role:        role,
		token:       token,
		status:      status,
		invitedBy:   invitedBy,
		inviterName: inviterName,
		storeName:   storeName,
		storeSlug:   storeSlug,
		expiresAt:   expiresAt,
		acceptedAt:  acceptedAt,
		createdAt:   createdAt,
	}
}

// ============================================
// Getters (immutable access)
// ============================================

func (i *Invitation) ID() vo.InvitationID      { return i.id }
func (i *Invitation) StoreID() vo.StoreID      { return i.storeID }
func (i *Invitation) Email() vo.Email          { return i.email }
func (i *Invitation) Role() vo.Role            { return i.role }
func (i *Invitation) Token() InvitationToken   { return i.token }
func (i *Invitation) Status() InvitationStatus { return i.status }
func (i *Invitation) InvitedBy() vo.MemberID   { return i.invitedBy }
func (i *Invitation) InviterName() *string     { return i.inviterName }
func (i *Invitation) StoreName() string        { return i.storeName }
func (i *Invitation) StoreSlug() string        { return i.storeSlug }
func (i *Invitation) ExpiresAt() time.Time     { return i.expiresAt }
func (i *Invitation) AcceptedAt() *time.Time   { return i.acceptedAt }
func (i *Invitation) CreatedAt() time.Time     { return i.createdAt }

// ============================================
// Business Rules
// ============================================

// IsPending returns true if the invitation is pending.
func (i *Invitation) IsPending() bool {
	return i.status.IsPending()
}

// IsExpired returns true if the invitation has expired.
func (i *Invitation) IsExpired() bool {
	return time.Now().After(i.expiresAt)
}

// CanBeAccepted checks if the invitation can be accepted.
func (i *Invitation) CanBeAccepted() error {
	if !i.status.IsPending() {
		return ErrInvitationNotPending
	}
	if i.IsExpired() {
		return ErrInvitationExpired
	}
	return nil
}

// CanBeAcceptedBy checks if the invitation can be accepted by the given email.
func (i *Invitation) CanBeAcceptedBy(email vo.Email) error {
	if err := i.CanBeAccepted(); err != nil {
		return err
	}
	if !i.email.Equals(email) {
		return ErrEmailMismatch
	}
	return nil
}

// CanBeResent checks if the invitation can be resent.
func (i *Invitation) CanBeResent() error {
	if !i.status.IsPending() {
		return ErrCannotResendNonPending
	}
	return nil
}

// ============================================
// State Changes
// ============================================

// Accept marks the invitation as accepted.
func (i *Invitation) Accept() {
	now := time.Now()
	i.status = StatusAccepted
	i.acceptedAt = &now
}

// Revoke marks the invitation as revoked.
func (i *Invitation) Revoke() {
	i.status = StatusRevoked
}

// Expire marks the invitation as expired.
func (i *Invitation) Expire() {
	i.status = StatusExpired
}

// RegenerateToken creates a new token and extends expiration.
func (i *Invitation) RegenerateToken() error {
	token, err := GenerateToken()
	if err != nil {
		return err
	}
	i.token = token
	i.expiresAt = time.Now().Add(time.Hour * 24 * DefaultExpirationDays)
	return nil
}
