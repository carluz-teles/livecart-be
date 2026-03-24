package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Service handles idempotency checking and caching for integration operations.
type Service struct {
	repo Repository
}

// Repository defines the database operations needed for idempotency.
type Repository interface {
	GetByKey(ctx context.Context, storeID, key string) (*Record, error)
	GetByHash(ctx context.Context, storeID, hash string, windowStart time.Time) (*Record, error)
	Create(ctx context.Context, record CreateParams) (*Record, error)
	Update(ctx context.Context, id string, response []byte, status string) error
}

// Record represents an idempotency record from the database.
type Record struct {
	ID             string
	IdempotencyKey string
	StoreID        string
	IntegrationID  string
	Operation      string
	RequestHash    string
	Response       []byte
	Status         string // pending, completed, failed
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

// CreateParams contains parameters for creating an idempotency record.
type CreateParams struct {
	IdempotencyKey string
	StoreID        string
	IntegrationID  string
	Operation      string
	RequestHash    string
}

// NewService creates a new idempotency service.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// CheckRequest contains the parameters for an idempotency check.
type CheckRequest struct {
	IdempotencyKey string // Explicit key from X-Idempotency-Key header
	StoreID        string
	IntegrationID  string
	Operation      string
	Payload        any
}

// CheckResult contains the result of an idempotency check.
type CheckResult struct {
	Found    bool
	Record   *Record
	Response []byte
}

// Check checks if a request is idempotent.
// Returns the cached response if found, nil otherwise.
func (s *Service) Check(ctx context.Context, req CheckRequest) (*CheckResult, error) {
	// First check explicit idempotency key
	if req.IdempotencyKey != "" {
		record, err := s.repo.GetByKey(ctx, req.StoreID, req.IdempotencyKey)
		if err != nil {
			return nil, fmt.Errorf("checking idempotency key: %w", err)
		}
		if record != nil && record.Status == "completed" {
			return &CheckResult{
				Found:    true,
				Record:   record,
				Response: record.Response,
			}, nil
		}
		// If found but pending/failed, we'll proceed with the request
		// This allows retrying failed operations with the same key
	}

	// Fallback: check by payload hash within time window
	hash := s.hashPayload(req.Payload)
	windowStart := time.Now().Add(-5 * time.Minute) // 5-minute deduplication window

	record, err := s.repo.GetByHash(ctx, req.StoreID, hash, windowStart)
	if err != nil {
		return nil, fmt.Errorf("checking payload hash: %w", err)
	}
	if record != nil && record.Status == "completed" {
		return &CheckResult{
			Found:    true,
			Record:   record,
			Response: record.Response,
		}, nil
	}

	return &CheckResult{Found: false}, nil
}

// Start creates a new idempotency record in pending state.
// Call this before executing the actual operation.
func (s *Service) Start(ctx context.Context, req CheckRequest) (*Record, error) {
	hash := s.hashPayload(req.Payload)

	record, err := s.repo.Create(ctx, CreateParams{
		IdempotencyKey: req.IdempotencyKey,
		StoreID:        req.StoreID,
		IntegrationID:  req.IntegrationID,
		Operation:      req.Operation,
		RequestHash:    hash,
	})
	if err != nil {
		return nil, fmt.Errorf("creating idempotency record: %w", err)
	}

	return record, nil
}

// Complete marks the idempotency record as completed with the response.
// Call this after the operation succeeds.
func (s *Service) Complete(ctx context.Context, id string, response any) error {
	respBytes, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("marshaling response: %w", err)
	}
	return s.repo.Update(ctx, id, respBytes, "completed")
}

// Fail marks the idempotency record as failed.
// Call this after the operation fails permanently.
func (s *Service) Fail(ctx context.Context, id string, opErr error) error {
	errResp, _ := json.Marshal(map[string]string{"error": opErr.Error()})
	return s.repo.Update(ctx, id, errResp, "failed")
}

// hashPayload creates a SHA-256 hash of the payload for deduplication.
func (s *Service) hashPayload(payload any) string {
	data, _ := json.Marshal(payload)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// =============================================================================
// HELPER FUNCTIONS
// =============================================================================

// ParseUUID parses a UUID string into pgtype.UUID.
func ParseUUID(s string) (pgtype.UUID, error) {
	var uuid pgtype.UUID
	if err := uuid.Scan(s); err != nil {
		return pgtype.UUID{}, fmt.Errorf("invalid UUID: %s", s)
	}
	return uuid, nil
}

// UUIDToString converts pgtype.UUID to string.
func UUIDToString(uuid pgtype.UUID) string {
	if !uuid.Valid {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		uuid.Bytes[0:4],
		uuid.Bytes[4:6],
		uuid.Bytes[6:8],
		uuid.Bytes[8:10],
		uuid.Bytes[10:16])
}
