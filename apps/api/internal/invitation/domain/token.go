package domain

import (
	"crypto/rand"
	"database/sql/driver"
	"encoding/hex"
	"errors"
)

const (
	// TokenLength is the length of the invitation token in bytes (will be hex encoded to 64 chars)
	TokenLength = 32
)

var (
	ErrInvalidToken = errors.New("invalid invitation token")
	ErrEmptyToken   = errors.New("invitation token cannot be empty")
)

// InvitationToken represents a secure invitation token.
type InvitationToken struct {
	value string
}

// NewInvitationToken creates a new InvitationToken from an existing string.
func NewInvitationToken(raw string) (InvitationToken, error) {
	if raw == "" {
		return InvitationToken{}, ErrEmptyToken
	}
	if len(raw) != TokenLength*2 { // hex encoded
		return InvitationToken{}, ErrInvalidToken
	}
	return InvitationToken{value: raw}, nil
}

// GenerateToken creates a new random invitation token.
func GenerateToken() (InvitationToken, error) {
	bytes := make([]byte, TokenLength)
	if _, err := rand.Read(bytes); err != nil {
		return InvitationToken{}, err
	}
	return InvitationToken{value: hex.EncodeToString(bytes)}, nil
}

// MustGenerateToken creates a new random token or panics.
func MustGenerateToken() InvitationToken {
	t, err := GenerateToken()
	if err != nil {
		panic(err)
	}
	return t
}

// String returns the token as a string.
func (t InvitationToken) String() string {
	return t.value
}

// IsZero returns true if the token is empty.
func (t InvitationToken) IsZero() bool {
	return t.value == ""
}

// Equals compares two tokens for equality.
func (t InvitationToken) Equals(other InvitationToken) bool {
	return t.value == other.value
}

// Value implements driver.Valuer for database serialization.
func (t InvitationToken) Value() (driver.Value, error) {
	if t.IsZero() {
		return nil, nil
	}
	return t.value, nil
}

// Scan implements sql.Scanner for database deserialization.
func (t *InvitationToken) Scan(src any) error {
	if src == nil {
		t.value = ""
		return nil
	}

	switch v := src.(type) {
	case string:
		t.value = v
		return nil
	case []byte:
		t.value = string(v)
		return nil
	default:
		return ErrInvalidToken
	}
}
