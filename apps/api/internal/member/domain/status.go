package domain

import (
	"database/sql/driver"
	"errors"
)

// MemberStatus represents the status of a member in a store.
type MemberStatus struct {
	value string
}

// Status constants
var (
	StatusActive  = MemberStatus{value: "active"}
	StatusPending = MemberStatus{value: "pending"}
	StatusRemoved = MemberStatus{value: "removed"}
)

var (
	ErrInvalidStatus = errors.New("invalid member status")
)

var validStatuses = map[string]MemberStatus{
	"active":  StatusActive,
	"pending": StatusPending,
	"removed": StatusRemoved,
}

// NewMemberStatus creates a new MemberStatus from a string.
func NewMemberStatus(raw string) (MemberStatus, error) {
	if raw == "" {
		return StatusActive, nil // default to active
	}

	status, ok := validStatuses[raw]
	if !ok {
		return MemberStatus{}, ErrInvalidStatus
	}

	return status, nil
}

// MustNewMemberStatus creates a new MemberStatus or panics.
func MustNewMemberStatus(raw string) MemberStatus {
	s, err := NewMemberStatus(raw)
	if err != nil {
		panic(err)
	}
	return s
}

// String returns the status as a string.
func (s MemberStatus) String() string {
	return s.value
}

// IsZero returns true if the status is empty.
func (s MemberStatus) IsZero() bool {
	return s.value == ""
}

// IsActive returns true if the member is active.
func (s MemberStatus) IsActive() bool {
	return s.value == StatusActive.value
}

// IsPending returns true if the member is pending (invited but not accepted).
func (s MemberStatus) IsPending() bool {
	return s.value == StatusPending.value
}

// IsRemoved returns true if the member was removed.
func (s MemberStatus) IsRemoved() bool {
	return s.value == StatusRemoved.value
}

// Value implements driver.Valuer for database serialization.
func (s MemberStatus) Value() (driver.Value, error) {
	if s.IsZero() {
		return "active", nil
	}
	return s.value, nil
}

// Scan implements sql.Scanner for database deserialization.
func (s *MemberStatus) Scan(src any) error {
	if src == nil {
		*s = StatusActive
		return nil
	}

	switch v := src.(type) {
	case string:
		status, err := NewMemberStatus(v)
		if err != nil {
			s.value = v
			return nil
		}
		*s = status
		return nil
	case []byte:
		status, err := NewMemberStatus(string(v))
		if err != nil {
			s.value = string(v)
			return nil
		}
		*s = status
		return nil
	default:
		return ErrInvalidStatus
	}
}
