package valueobject

import (
	"database/sql/driver"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// ID represents a generic UUID identifier.
type ID struct {
	value uuid.UUID
}

// NewID creates a new ID from a string.
func NewID(raw string) (ID, error) {
	if raw == "" {
		return ID{}, ErrEmptyUUID
	}

	parsed, err := uuid.Parse(raw)
	if err != nil {
		return ID{}, ErrInvalidUUID
	}

	return ID{value: parsed}, nil
}

// GenerateID creates a new random ID.
func GenerateID() ID {
	return ID{value: uuid.New()}
}

// MustNewID creates a new ID or panics if invalid.
func MustNewID(raw string) ID {
	id, err := NewID(raw)
	if err != nil {
		panic(err)
	}
	return id
}

// String returns the ID as a string.
func (id ID) String() string {
	return id.value.String()
}

// IsZero returns true if the ID is empty.
func (id ID) IsZero() bool {
	return id.value == uuid.Nil
}

// Equals compares two IDs for equality.
func (id ID) Equals(other ID) bool {
	return id.value == other.value
}

// ToPgUUID converts to pgtype.UUID for database operations.
func (id ID) ToPgUUID() pgtype.UUID {
	return pgtype.UUID{Bytes: id.value, Valid: !id.IsZero()}
}

// Value implements driver.Valuer for database serialization.
func (id ID) Value() (driver.Value, error) {
	if id.IsZero() {
		return nil, nil
	}
	return id.value.String(), nil
}

// Scan implements sql.Scanner for database deserialization.
func (id *ID) Scan(src any) error {
	if src == nil {
		id.value = uuid.Nil
		return nil
	}

	switch v := src.(type) {
	case string:
		parsed, err := uuid.Parse(v)
		if err != nil {
			return ErrInvalidUUID
		}
		id.value = parsed
		return nil
	case []byte:
		parsed, err := uuid.Parse(string(v))
		if err != nil {
			// Try parsing as raw bytes (16 bytes)
			if len(v) == 16 {
				id.value = uuid.UUID(v)
				return nil
			}
			return ErrInvalidUUID
		}
		id.value = parsed
		return nil
	case [16]byte:
		id.value = uuid.UUID(v)
		return nil
	default:
		return ErrInvalidUUID
	}
}

// ============================================
// Typed IDs for different entities
// ============================================

// StoreID represents a store's unique identifier.
type StoreID struct{ ID }

func NewStoreID(raw string) (StoreID, error) {
	id, err := NewID(raw)
	if err != nil {
		return StoreID{}, err
	}
	return StoreID{ID: id}, nil
}

func MustNewStoreID(raw string) StoreID {
	return StoreID{ID: MustNewID(raw)}
}

func GenerateStoreID() StoreID {
	return StoreID{ID: GenerateID()}
}

func (id StoreID) Equals(other StoreID) bool {
	return id.ID.Equals(other.ID)
}

// MemberID represents a member's unique identifier (store_user.id).
type MemberID struct{ ID }

func NewMemberID(raw string) (MemberID, error) {
	id, err := NewID(raw)
	if err != nil {
		return MemberID{}, err
	}
	return MemberID{ID: id}, nil
}

func MustNewMemberID(raw string) MemberID {
	return MemberID{ID: MustNewID(raw)}
}

func GenerateMemberID() MemberID {
	return MemberID{ID: GenerateID()}
}

func (id MemberID) Equals(other MemberID) bool {
	return id.ID.Equals(other.ID)
}

// UserID represents a user's unique identifier (clerk user id - string, not UUID).
type UserID struct {
	value string
}

func NewUserID(raw string) (UserID, error) {
	if raw == "" {
		return UserID{}, ErrEmptyUUID
	}
	return UserID{value: raw}, nil
}

func MustNewUserID(raw string) UserID {
	id, err := NewUserID(raw)
	if err != nil {
		panic(err)
	}
	return id
}

func (id UserID) String() string  { return id.value }
func (id UserID) IsZero() bool    { return id.value == "" }
func (id UserID) Equals(other UserID) bool { return id.value == other.value }

// ProductID represents a product's unique identifier.
type ProductID struct{ ID }

func NewProductID(raw string) (ProductID, error) {
	id, err := NewID(raw)
	if err != nil {
		return ProductID{}, err
	}
	return ProductID{ID: id}, nil
}

func MustNewProductID(raw string) ProductID {
	return ProductID{ID: MustNewID(raw)}
}

func GenerateProductID() ProductID {
	return ProductID{ID: GenerateID()}
}

func (id ProductID) Equals(other ProductID) bool {
	return id.ID.Equals(other.ID)
}

// OrderID represents an order's unique identifier.
type OrderID struct{ ID }

func NewOrderID(raw string) (OrderID, error) {
	id, err := NewID(raw)
	if err != nil {
		return OrderID{}, err
	}
	return OrderID{ID: id}, nil
}

func MustNewOrderID(raw string) OrderID {
	return OrderID{ID: MustNewID(raw)}
}

func GenerateOrderID() OrderID {
	return OrderID{ID: GenerateID()}
}

func (id OrderID) Equals(other OrderID) bool {
	return id.ID.Equals(other.ID)
}

// InvitationID represents an invitation's unique identifier.
type InvitationID struct{ ID }

func NewInvitationID(raw string) (InvitationID, error) {
	id, err := NewID(raw)
	if err != nil {
		return InvitationID{}, err
	}
	return InvitationID{ID: id}, nil
}

func MustNewInvitationID(raw string) InvitationID {
	return InvitationID{ID: MustNewID(raw)}
}

func GenerateInvitationID() InvitationID {
	return InvitationID{ID: GenerateID()}
}

func (id InvitationID) Equals(other InvitationID) bool {
	return id.ID.Equals(other.ID)
}

// LiveID represents a live session's unique identifier.
type LiveID struct{ ID }

func NewLiveID(raw string) (LiveID, error) {
	id, err := NewID(raw)
	if err != nil {
		return LiveID{}, err
	}
	return LiveID{ID: id}, nil
}

func MustNewLiveID(raw string) LiveID {
	return LiveID{ID: MustNewID(raw)}
}

func GenerateLiveID() LiveID {
	return LiveID{ID: GenerateID()}
}

func (id LiveID) Equals(other LiveID) bool {
	return id.ID.Equals(other.ID)
}

// CustomerID represents a customer's unique identifier.
type CustomerID struct{ ID }

func NewCustomerID(raw string) (CustomerID, error) {
	id, err := NewID(raw)
	if err != nil {
		return CustomerID{}, err
	}
	return CustomerID{ID: id}, nil
}

func MustNewCustomerID(raw string) CustomerID {
	return CustomerID{ID: MustNewID(raw)}
}

func GenerateCustomerID() CustomerID {
	return CustomerID{ID: GenerateID()}
}

func (id CustomerID) Equals(other CustomerID) bool {
	return id.ID.Equals(other.ID)
}

// IntegrationID represents an integration's unique identifier.
type IntegrationID struct{ ID }

func NewIntegrationID(raw string) (IntegrationID, error) {
	id, err := NewID(raw)
	if err != nil {
		return IntegrationID{}, err
	}
	return IntegrationID{ID: id}, nil
}

func MustNewIntegrationID(raw string) IntegrationID {
	return IntegrationID{ID: MustNewID(raw)}
}

func GenerateIntegrationID() IntegrationID {
	return IntegrationID{ID: GenerateID()}
}

func (id IntegrationID) Equals(other IntegrationID) bool {
	return id.ID.Equals(other.ID)
}
