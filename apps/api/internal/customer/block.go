package customer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
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

type BlockedHandleOutput struct {
	ID           string     `json:"id"`
	Handle       string     `json:"handle"`
	Reason       *string    `json:"reason,omitempty"`
	BlockedAt    time.Time  `json:"blockedAt"`
	UnblockedAt  *time.Time `json:"unblockedAt,omitempty"`
	BlockedByID  *string    `json:"blockedByUserId,omitempty"`
	CartsRemoved int        `json:"cartsRemoved,omitempty"`
}

type ListBlockedHandlesInput struct {
	StoreID         uuid.UUID
	IncludeInactive bool
	Limit           int32
	Offset          int32
}

type BlockHandleRequest struct {
	Handle string `json:"handle" validate:"required,min=1"`
	Reason string `json:"reason,omitempty"`
}

// =============================================================================
// REPOSITORY
// =============================================================================

func (r *Repository) BlockHandle(ctx context.Context, input BlockHandleInput) (*sqlc.BlockedHandle, error) {
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
	return &row, nil
}

func (r *Repository) UnblockHandle(ctx context.Context, input UnblockHandleInput) (*sqlc.BlockedHandle, error) {
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
	return &row, nil
}

func (r *Repository) IsHandleBlocked(ctx context.Context, storeID uuid.UUID, handle string) (bool, error) {
	return r.queries.IsHandleBlocked(ctx, sqlc.IsHandleBlockedParams{
		StoreID:        uuidToPgtype(storeID),
		PlatformHandle: normalizeHandle(handle),
	})
}

func (r *Repository) ListBlockedHandles(ctx context.Context, input ListBlockedHandlesInput) ([]sqlc.BlockedHandle, int, error) {
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
	return rows, int(total), nil
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
func (s *Service) BlockHandle(ctx context.Context, input BlockHandleInput) (*BlockedHandleOutput, error) {
	input.Handle = normalizeHandle(input.Handle)
	if input.Handle == "" {
		return nil, httpx.ErrBadRequest("handle is required")
	}

	row, err := s.repo.BlockHandle(ctx, input)
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

	out := toBlockedHandleOutput(row)
	out.CartsRemoved = cartsRemoved
	return out, nil
}

// UnblockHandle reverses a previous block. Returns nil if the handle was never
// blocked.
func (s *Service) UnblockHandle(ctx context.Context, input UnblockHandleInput) (*BlockedHandleOutput, error) {
	input.Handle = normalizeHandle(input.Handle)
	if input.Handle == "" {
		return nil, httpx.ErrBadRequest("handle is required")
	}

	row, err := s.repo.UnblockHandle(ctx, input)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, httpx.ErrNotFound(fmt.Sprintf("handle %q is not blocked", input.Handle))
	}

	logger.From(ctx, s.logger).Info("handle unblocked",
		zap.String("storeId", input.StoreID.String()),
		zap.String("handle", input.Handle),
	)

	return toBlockedHandleOutput(row), nil
}

// IsHandleBlocked is the fast path used by the Instagram comment processor.
func (s *Service) IsHandleBlocked(ctx context.Context, storeID uuid.UUID, handle string) (bool, error) {
	return s.repo.IsHandleBlocked(ctx, storeID, handle)
}

// ListBlockedHandles returns all blocked handles for the given store.
func (s *Service) ListBlockedHandles(ctx context.Context, input ListBlockedHandlesInput) ([]BlockedHandleOutput, int, error) {
	rows, total, err := s.repo.ListBlockedHandles(ctx, input)
	if err != nil {
		return nil, 0, err
	}
	out := make([]BlockedHandleOutput, len(rows))
	for i, r := range rows {
		r := r
		out[i] = *toBlockedHandleOutput(&r)
	}
	return out, total, nil
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
// @Success      200 {object} httpx.Envelope{data=BlockedHandleOutput}
// @Router       /api/v1/stores/{storeId}/customers/blocks [post]
// @Security     BearerAuth
func (h *Handler) Block(c *fiber.Ctx) error {
	storeIDStr := c.Locals("store_id").(string)
	storeID, err := uuid.Parse(storeIDStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid store id")
	}

	var req BlockHandleRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}
	if err := h.validate.Struct(&req); err != nil {
		return httpx.BadRequest(c, err.Error())
	}

	input := BlockHandleInput{
		StoreID: storeID,
		Handle:  req.Handle,
	}
	if req.Reason != "" {
		input.Reason = &req.Reason
	}
	if uid := httpx.GetInternalUserID(c); uid != "" {
		if parsed, err := uuid.Parse(uid); err == nil {
			input.BlockedByUserID = &parsed
		}
	}

	out, err := h.service.BlockHandle(c.Context(), input)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, out)
}

// Unblock godoc
// @Summary      Unblock a customer handle
// @Tags         customers
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        handle path string true "Instagram handle (without @)"
// @Success      200 {object} httpx.Envelope{data=BlockedHandleOutput}
// @Failure      404 {object} httpx.Envelope
// @Router       /api/v1/stores/{storeId}/customers/blocks/{handle} [delete]
// @Security     BearerAuth
func (h *Handler) Unblock(c *fiber.Ctx) error {
	storeIDStr := c.Locals("store_id").(string)
	storeID, err := uuid.Parse(storeIDStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid store id")
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

	out, err := h.service.UnblockHandle(c.Context(), input)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, out)
}

// ListBlocked godoc
// @Summary      List blocked customer handles
// @Tags         customers
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        includeInactive query bool false "Include previously-unblocked handles"
// @Param        limit query int false "Items per page" default(50)
// @Param        offset query int false "Offset" default(0)
// @Success      200 {object} httpx.Envelope{data=[]BlockedHandleOutput}
// @Router       /api/v1/stores/{storeId}/customers/blocks [get]
// @Security     BearerAuth
func (h *Handler) ListBlocked(c *fiber.Ctx) error {
	storeIDStr := c.Locals("store_id").(string)
	storeID, err := uuid.Parse(storeIDStr)
	if err != nil {
		return httpx.BadRequest(c, "invalid store id")
	}

	includeInactive := c.QueryBool("includeInactive", false)
	limit := int32(c.QueryInt("limit", 50))
	offset := int32(c.QueryInt("offset", 0))

	rows, total, err := h.service.ListBlockedHandles(c.Context(), ListBlockedHandlesInput{
		StoreID:         storeID,
		IncludeInactive: includeInactive,
		Limit:           limit,
		Offset:          offset,
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, fiber.Map{
		"data":  rows,
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

func toBlockedHandleOutput(row *sqlc.BlockedHandle) *BlockedHandleOutput {
	out := &BlockedHandleOutput{
		ID:        pgtypeToUUID(row.ID).String(),
		Handle:    row.PlatformHandle,
		BlockedAt: row.BlockedAt.Time,
	}
	if row.Reason.Valid {
		s := row.Reason.String
		out.Reason = &s
	}
	if row.UnblockedAt.Valid {
		t := row.UnblockedAt.Time
		out.UnblockedAt = &t
	}
	if row.BlockedByUserID.Valid {
		id := pgtypeToUUID(row.BlockedByUserID).String()
		out.BlockedByID = &id
	}
	return out
}
