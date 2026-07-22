package customer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/customer/domain"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// =============================================================================
// CART CANCELER INTERFACE
// =============================================================================

// CartCanceler is implemented by the integration service to handle the heavy
// side of a block: ERP stock reversal + waitlist promotion. Kept as a narrow
// interface so the customer package doesn't import the integration package.
type CartCanceler interface {
	CancelOpenCartsForBlockedHandle(ctx context.Context, storeID, handle string) error
}

// SetCartCanceler is called from main.go after both services exist. It's a
// setter (not a constructor arg) because customer.Service is built before the
// integration service to avoid a circular wiring dependency.
func (s *Service) SetCartCanceler(c CartCanceler) {
	s.cartCanceler = c
}

// =============================================================================
// TYPES
// =============================================================================

type BlockHandleInput struct {
	StoreID         uuid.UUID
	Handle          string
	Reason          *string
	BlockedByUserID *uuid.UUID
}

type UnblockHandleInput struct {
	StoreID           uuid.UUID
	Handle            string
	UnblockedByUserID *uuid.UUID
}

type ListBlockedHandlesInput struct {
	StoreID         uuid.UUID
	IncludeInactive bool
	Limit           int32
	Offset          int32
}

type BlockHandleRequest struct {
	Handle string `json:"handle"`
	Reason string `json:"reason,omitempty"`
}

// Validate is the syntactic gate (ozzo): handle is required.
func (r BlockHandleRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Handle, validation.Required, validation.Length(1, 0)),
	)
}

// ToInput builds the BlockHandleInput, constructing the store UUID and the
// optional actor UUID. Returns 422 when the store id is not a valid UUID.
func (r BlockHandleRequest) ToInput(storeID, actorID string) (BlockHandleInput, error) {
	storeUUID, err := uuid.Parse(storeID)
	if err != nil {
		return BlockHandleInput{}, httpx.ErrUnprocessable("invalid store id")
	}
	input := BlockHandleInput{
		StoreID: storeUUID,
		Handle:  r.Handle,
	}
	if r.Reason != "" {
		reason := r.Reason
		input.Reason = &reason
	}
	if actorID != "" {
		if parsed, err := uuid.Parse(actorID); err == nil {
			input.BlockedByUserID = &parsed
		}
	}
	return input, nil
}

// BlockedHandleResponse is the API shape for a blocked handle.
type BlockedHandleResponse struct {
	ID           string     `json:"id"`
	Handle       string     `json:"handle"`
	Reason       *string    `json:"reason,omitempty"`
	BlockedAt    time.Time  `json:"blockedAt"`
	UnblockedAt  *time.Time `json:"unblockedAt,omitempty"`
	BlockedByID  *string    `json:"blockedByUserId,omitempty"`
	CartsRemoved int        `json:"cartsRemoved,omitempty"`
}

// NewBlockedHandleResponse maps a domain BlockedHandle to its API response.
func NewBlockedHandleResponse(b *domain.BlockedHandle) BlockedHandleResponse {
	return BlockedHandleResponse{
		ID:           b.ID(),
		Handle:       b.Handle(),
		Reason:       b.Reason(),
		BlockedAt:    b.BlockedAt(),
		UnblockedAt:  b.UnblockedAt(),
		BlockedByID:  b.BlockedByID(),
		CartsRemoved: b.CartsRemoved(),
	}
}

// NewListBlockedHandlesResponse maps a slice of blocked-handle entities.
func NewListBlockedHandlesResponse(handles []*domain.BlockedHandle) []BlockedHandleResponse {
	out := make([]BlockedHandleResponse, len(handles))
	for i, b := range handles {
		out[i] = NewBlockedHandleResponse(b)
	}
	return out
}

// =============================================================================
// REPOSITORY
// =============================================================================

func (r *Repository) BlockHandle(ctx context.Context, input BlockHandleInput) (*domain.BlockedHandle, error) {
	params := sqlc.BlockHandleParams{
		StoreID:        uuidToPgtype(input.StoreID),
		PlatformHandle: normalizeHandle(input.Handle),
	}
	if input.Reason != nil {
		params.Reason = pgtype.Text{String: *input.Reason, Valid: true}
	}
	if input.BlockedByUserID != nil {
		params.BlockedByUserID = uuidToPgtype(*input.BlockedByUserID)
	}
	row, err := r.queries.BlockHandle(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("blocking handle: %w", err)
	}
	return toDomainBlockedHandle(row), nil
}

func (r *Repository) UnblockHandle(ctx context.Context, input UnblockHandleInput) (*domain.BlockedHandle, error) {
	params := sqlc.UnblockHandleParams{
		StoreID:        uuidToPgtype(input.StoreID),
		PlatformHandle: normalizeHandle(input.Handle),
	}
	if input.UnblockedByUserID != nil {
		params.UnblockedByUserID = uuidToPgtype(*input.UnblockedByUserID)
	}
	row, err := r.queries.UnblockHandle(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("unblocking handle: %w", err)
	}
	return toDomainBlockedHandle(row), nil
}

func (r *Repository) IsHandleBlocked(ctx context.Context, storeID uuid.UUID, handle string) (bool, error) {
	return r.queries.IsHandleBlocked(ctx, sqlc.IsHandleBlockedParams{
		StoreID:        uuidToPgtype(storeID),
		PlatformHandle: normalizeHandle(handle),
	})
}

func (r *Repository) ListBlockedHandles(ctx context.Context, input ListBlockedHandlesInput) ([]*domain.BlockedHandle, int, error) {
	limit := input.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.queries.ListBlockedHandles(ctx, sqlc.ListBlockedHandlesParams{
		StoreID: uuidToPgtype(input.StoreID),
		Column2: input.IncludeInactive,
		Limit:   limit,
		Offset:  input.Offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("listing blocked handles: %w", err)
	}
	total, err := r.queries.CountBlockedHandles(ctx, sqlc.CountBlockedHandlesParams{
		StoreID: uuidToPgtype(input.StoreID),
		Column2: input.IncludeInactive,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("counting blocked handles: %w", err)
	}
	handles := make([]*domain.BlockedHandle, len(rows))
	for i := range rows {
		handles[i] = toDomainBlockedHandle(rows[i])
	}
	return handles, int(total), nil
}

// BlockedHandlesFor returns the subset of `handles` that are currently blocked
// for `storeID`. Used by order/customer detail endpoints to surface a badge
// without N round-trips.
func (r *Repository) BlockedHandlesFor(ctx context.Context, storeID uuid.UUID, handles []string) (map[string]bool, error) {
	if len(handles) == 0 {
		return map[string]bool{}, nil
	}
	normalized := make([]string, 0, len(handles))
	for _, h := range handles {
		if h = normalizeHandle(h); h != "" {
			normalized = append(normalized, h)
		}
	}
	rows, err := r.queries.ListBlockedHandlesForStore(ctx, sqlc.ListBlockedHandlesForStoreParams{
		StoreID: uuidToPgtype(storeID),
		Column2: normalized,
	})
	if err != nil {
		return nil, fmt.Errorf("looking up blocked handles batch: %w", err)
	}
	out := make(map[string]bool, len(rows))
	for _, h := range rows {
		out[h] = true
	}
	return out, nil
}

// =============================================================================
// SERVICE
// =============================================================================

// BlockHandle marks an Instagram handle as blocked for the given store and
// triggers cancellation of any of their open (non-paid) carts via the
// CartCanceler. Idempotent: re-blocking an already-blocked handle just bumps
// blocked_at and updates the reason.
func (s *Service) BlockHandle(ctx context.Context, input BlockHandleInput) (*domain.BlockedHandle, error) {
	input.Handle = normalizeHandle(input.Handle)
	if input.Handle == "" {
		return nil, httpx.ErrBadRequest("handle is required")
	}

	blocked, err := s.repo.BlockHandle(ctx, input)
	if err != nil {
		return nil, err
	}

	logger.From(ctx, s.logger).Info("handle blocked",
		zap.String("storeId", input.StoreID.String()),
		zap.String("handle", input.Handle),
	)

	cartsRemoved := 0
	if s.cartCanceler != nil {
		if err := s.cartCanceler.CancelOpenCartsForBlockedHandle(ctx, input.StoreID.String(), input.Handle); err != nil {
			// Log but don't fail the block: the row is in the DB, so future
			// comments will already be ignored. The admin can retry the
			// cleanup by toggling the block off and on, and we surface the
			// failure via logs / Sentry.
			logger.From(ctx, s.logger).Error("failed to cancel open carts after block",
				zap.String("storeId", input.StoreID.String()),
				zap.String("handle", input.Handle),
				zap.Error(err),
			)
		}
	}

	blocked.SetCartsRemoved(cartsRemoved)
	return blocked, nil
}

// UnblockHandle reverses a previous block. Returns nil if the handle was never
// blocked.
func (s *Service) UnblockHandle(ctx context.Context, input UnblockHandleInput) (*domain.BlockedHandle, error) {
	input.Handle = normalizeHandle(input.Handle)
	if input.Handle == "" {
		return nil, httpx.ErrBadRequest("handle is required")
	}

	blocked, err := s.repo.UnblockHandle(ctx, input)
	if err != nil {
		return nil, err
	}
	if blocked == nil {
		return nil, httpx.ErrNotFound(fmt.Sprintf("handle %q is not blocked", input.Handle))
	}

	logger.From(ctx, s.logger).Info("handle unblocked",
		zap.String("storeId", input.StoreID.String()),
		zap.String("handle", input.Handle),
	)

	return blocked, nil
}

// IsHandleBlocked is the fast path used by the Instagram comment processor.
func (s *Service) IsHandleBlocked(ctx context.Context, storeID uuid.UUID, handle string) (bool, error) {
	return s.repo.IsHandleBlocked(ctx, storeID, handle)
}

// ListBlockedHandles returns all blocked handles for the given store.
func (s *Service) ListBlockedHandles(ctx context.Context, input ListBlockedHandlesInput) ([]*domain.BlockedHandle, int, error) {
	return s.repo.ListBlockedHandles(ctx, input)
}

// BlockedHandlesFor exposes the batch lookup to other services (e.g. order).
func (s *Service) BlockedHandlesFor(ctx context.Context, storeID uuid.UUID, handles []string) (map[string]bool, error) {
	return s.repo.BlockedHandlesFor(ctx, storeID, handles)
}

// =============================================================================
// HANDLER
// =============================================================================

// RegisterBlockRoutes wires the block/unblock endpoints onto an existing
// customer route group. Called by Handler.RegisterRoutes.
func (h *Handler) RegisterBlockRoutes(group fiber.Router) {
	group.Post("/blocks", h.Block)
	group.Get("/blocks", h.ListBlocked)
	group.Delete("/blocks/:handle", h.Unblock)
}

// Block godoc
// @Summary      Block a customer handle
// @Description  Prevents future purchases from this Instagram handle. Cancels any open (non-paid) carts and reverses ERP stock reservations.
// @Tags         customers
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        request body BlockHandleRequest true "Handle to block"
// @Success      200 {object} httpx.Envelope{data=BlockedHandleResponse}
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/customers/blocks [post]
// @Security     BearerAuth
func (h *Handler) Block(c *fiber.Ctx) error {
	var req BlockHandleRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	input, err := req.ToInput(httpx.GetStoreID(c), httpx.GetInternalUserID(c))
	if err != nil {
		return err
	}
	blocked, err := h.service.BlockHandle(c.UserContext(), input)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewBlockedHandleResponse(blocked))
}

// Unblock godoc
// @Summary      Unblock a customer handle
// @Tags         customers
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        handle path string true "Instagram handle (without @)"
// @Success      200 {object} httpx.Envelope{data=BlockedHandleResponse}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/customers/blocks/{handle} [delete]
// @Security     BearerAuth
func (h *Handler) Unblock(c *fiber.Ctx) error {
	storeID, err := uuid.Parse(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrBadRequest("invalid store id")
	}

	input := UnblockHandleInput{
		StoreID: storeID,
		Handle:  c.Params("handle"),
	}
	if uid := httpx.GetInternalUserID(c); uid != "" {
		if parsed, err := uuid.Parse(uid); err == nil {
			input.UnblockedByUserID = &parsed
		}
	}

	blocked, err := h.service.UnblockHandle(c.UserContext(), input)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewBlockedHandleResponse(blocked))
}

// ListBlocked godoc
// @Summary      List blocked customer handles
// @Tags         customers
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        includeInactive query bool false "Include previously-unblocked handles"
// @Param        limit query int false "Items per page" default(50)
// @Param        offset query int false "Offset" default(0)
// @Success      200 {object} httpx.Envelope{data=[]BlockedHandleResponse}
// @Router       /api/v1/stores/{storeId}/customers/blocks [get]
// @Security     BearerAuth
func (h *Handler) ListBlocked(c *fiber.Ctx) error {
	storeID, err := uuid.Parse(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrBadRequest("invalid store id")
	}

	includeInactive := c.QueryBool("includeInactive", false)
	limit := int32(c.QueryInt("limit", 50))
	offset := int32(c.QueryInt("offset", 0))

	handles, total, err := h.service.ListBlockedHandles(c.UserContext(), ListBlockedHandlesInput{
		StoreID:         storeID,
		IncludeInactive: includeInactive,
		Limit:           limit,
		Offset:          offset,
	})
	if err != nil {
		return err
	}
	return httpx.OK(c, fiber.Map{
		"data":  NewListBlockedHandlesResponse(handles),
		"total": total,
	})
}

// =============================================================================
// HELPERS
// =============================================================================

// normalizeHandle strips a leading @ and lowercases for consistent lookup.
// Instagram handles are case-insensitive.
func normalizeHandle(h string) string {
	h = strings.TrimSpace(h)
	h = strings.TrimPrefix(h, "@")
	return strings.ToLower(h)
}

func toDomainBlockedHandle(row sqlc.BlockedHandle) *domain.BlockedHandle {
	var reason, blockedByID *string
	var unblockedAt *time.Time
	if row.Reason.Valid {
		s := row.Reason.String
		reason = &s
	}
	if row.UnblockedAt.Valid {
		t := row.UnblockedAt.Time
		unblockedAt = &t
	}
	if row.BlockedByUserID.Valid {
		id := pgtypeToUUID(row.BlockedByUserID).String()
		blockedByID = &id
	}
	return domain.ReconstructBlockedHandle(
		pgtypeToUUID(row.ID).String(),
		row.PlatformHandle,
		reason,
		row.BlockedAt.Time,
		unblockedAt,
		blockedByID,
	)
}
