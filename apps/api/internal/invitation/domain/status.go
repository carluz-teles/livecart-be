package domain

import (
	"database/sql/driver"
	"errors"
)

// InvitationStatus represents the status of an invitation.
type InvitationStatus struct {
	value string
}

// Status constants
var (
	StatusPending  = InvitationStatus{value: "pending"}
	StatusAccepted = InvitationStatus{value: "accepted"}
	StatusExpired  = InvitationStatus{value: "expired"}
	StatusRevoked  = InvitationStatus{value: "revoked"}
)

var (
	ErrInvalidInvitationStatus = errors.New("invalid invitation status")
)

var validStatuses = map[string]InvitationStatus{
	"pending":  StatusPending,
	"accepted": StatusAccepted,
	"expired":  StatusExpired,
	"revoked":  StatusRevoked,
}

// NewInvitationStatus creates a new InvitationStatus from a string.
func NewInvitationStatus(raw string) (InvitationStatus, error) {
	if raw == "" {
		return StatusPending, nil
	}

	status, ok := validStatuses[raw]
	if !ok {
		return InvitationStatus{}, ErrInvalidInvitationStatus
	}

	return status, nil
}

// MustNewInvitationStatus creates a new InvitationStatus or panics.
func MustNewInvitationStatus(raw string) InvitationStatus {
	s, err := NewInvitationStatus(raw)
	if err != nil {
		panic(err)
	}
	return s
}

// String returns the status as a string.
func (s InvitationStatus) String() string {
	return s.value
}

// IsZero returns true if the status is empty.
func (s InvitationStatus) IsZero() bool {
	return s.value == ""
}

// IsPending returns true if the invitation is pending.
func (s InvitationStatus) IsPending() bool {
	return s.value == StatusPending.value
}

// IsAccepted returns true if the invitation was accepted.
func (s InvitationStatus) IsAccepted() bool {
	return s.value == StatusAccepted.value
}

// IsExpired returns true if the invitation has expired.
func (s InvitationStatus) IsExpired() bool {
	return s.value == StatusExpired.value
}

// IsRevoked returns true if the invitation was revoked.
func (s InvitationStatus) IsRevoked() bool {
	return s.value == StatusRevoked.value
}

// CanBeAccepted returns true if the invitation can still be accepted.
func (s InvitationStatus) CanBeAccepted() bool {
	return s.IsPending()
}

// Value implements driver.Valuer for database serialization.
func (s InvitationStatus) Value() (driver.Value, error) {
	if s.IsZero() {
		return "pending", nil
	}
	return s.value, nil
}

// Scan implements sql.Scanner for database deserialization.
func (s *InvitationStatus) Scan(src any) error {
	if src == nil {
		*s = StatusPending
		return nil
	}

	switch v := src.(type) {
	case string:
		status, err := NewInvitationStatus(v)
		if err != nil {
			s.value = v
			return nil
		}
		*s = status
		return nil
	case []byte:
		status, err := NewInvitationStatus(string(v))
		if err != nil {
			s.value = string(v)
			return nil
		}
		*s = status
		return nil
	default:
		return ErrInvalidInvitationStatus
	}
}
