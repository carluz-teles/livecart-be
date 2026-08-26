package customer

import (
	"context"
	"errors"
	"fmt"
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/customer/domain"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"

	"go.uber.org/zap"
)

// =============================================================================
// VIP CART ACTIVATOR INTERFACE
// =============================================================================

// VipActivation é o que a promoção fez com os carrinhos abertos do @.
type VipActivation struct {
	// EternalCartID é o carrinho que ficou eterno; vazio quando o @ não tinha
	// nenhum carrinho aberto.
	EternalCartID string
	// Merged é quantos carrinhos foram fundidos no eterno.
	Merged int
	// Skipped é quantos ficaram de fora por já terem pedido no ERP: seguem com
	// prazo, e portanto seguem expirando.
	Skipped int
}

// VipCartActivator is implemented by the integration service to handle the
// side effect of promoting a handle to VIP: consolidating their existing open
// carts into the single eternal one (never_expires=true, expires_at NULL).
// Narrow interface so customer doesn't import integration.
type VipCartActivator interface {
	ActivateVipCartsForHandle(ctx context.Context, storeID, handle string) (VipActivation, error)
}

// SetVipCartActivator is wired from main.go after both services exist.
func (s *Service) SetVipCartActivator(a VipCartActivator) {
	s.vipCartActivator = a
}

// =============================================================================
// TYPES
// =============================================================================

type AddVipInput struct {
	StoreID       uuid.UUID
	Handle        string
	AddedByUserID *uuid.UUID
}

type RemoveVipInput struct {
	StoreID         uuid.UUID
	Handle          string
	RemovedByUserID *uuid.UUID
}

type ListVipHandlesInput struct {
	StoreID         uuid.UUID
	IncludeInactive bool
	Limit           int32
	Offset          int32
}

type AddVipRequest struct {
	Handle string `json:"handle"`
}

// Validate is the syntactic gate (ozzo): handle is required.
func (r AddVipRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Handle, validation.Required, validation.Length(1, 0)),
	)
}

// ToInput builds AddVipInput. 422 when the store id is not a valid UUID.
func (r AddVipRequest) ToInput(storeID, actorID string) (AddVipInput, error) {
	storeUUID, err := uuid.Parse(storeID)
	if err != nil {
		return AddVipInput{}, httpx.ErrUnprocessable("invalid store id")
	}
	input := AddVipInput{StoreID: storeUUID, Handle: r.Handle}
	if actorID != "" {
		if parsed, err := uuid.Parse(actorID); err == nil {
			input.AddedByUserID = &parsed
		}
	}
	return input, nil
}

// VipHandleResponse is the API shape for a VIP handle.
type VipHandleResponse struct {
	ID           string     `json:"id"`
	Handle       string     `json:"handle"`
	AddedAt      time.Time  `json:"addedAt"`
	RemovedAt    *time.Time `json:"removedAt,omitempty"`
	AddedByID    *string    `json:"addedByUserId,omitempty"`
	CartsUpdated int        `json:"cartsUpdated,omitempty"`
	CartsMerged  int        `json:"cartsMerged,omitempty"`
	CartsSkipped int        `json:"cartsSkipped,omitempty"`
	// ActivationFailed: o @ virou VIP, mas os carrinhos que ele já tinha não
	// foram consolidados e seguem com prazo para expirar.
	ActivationFailed bool `json:"activationFailed,omitempty"`
}

func NewVipHandleResponse(v *domain.VipHandle) VipHandleResponse {
	return VipHandleResponse{
		ID:               v.ID(),
		Handle:           v.Handle(),
		AddedAt:          v.AddedAt(),
		RemovedAt:        v.RemovedAt(),
		AddedByID:        v.AddedByID(),
		CartsUpdated:     v.CartsUpdated(),
		CartsMerged:      v.CartsMerged(),
		CartsSkipped:     v.CartsSkipped(),
		ActivationFailed: v.ActivationFailed(),
	}
}

func NewListVipHandlesResponse(handles []*domain.VipHandle) []VipHandleResponse {
	out := make([]VipHandleResponse, len(handles))
	for i, v := range handles {
		out[i] = NewVipHandleResponse(v)
	}
	return out
}

// =============================================================================
// REPOSITORY
// =============================================================================

func (r *Repository) AddVipHandle(ctx context.Context, input AddVipInput) (*domain.VipHandle, error) {
	params := sqlc.AddVipHandleParams{
		StoreID:        uuidToPgtype(input.StoreID),
		PlatformHandle: normalizeHandle(input.Handle),
	}
	if input.AddedByUserID != nil {
		params.AddedByUserID = uuidToPgtype(*input.AddedByUserID)
	}
	row, err := r.queries.AddVipHandle(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("adding vip handle: %w", err)
	}
	return toDomainVipHandle(row), nil
}

func (r *Repository) RemoveVipHandle(ctx context.Context, input RemoveVipInput) (*domain.VipHandle, error) {
	params := sqlc.RemoveVipHandleParams{
		StoreID:        uuidToPgtype(input.StoreID),
		PlatformHandle: normalizeHandle(input.Handle),
	}
	if input.RemovedByUserID != nil {
		params.RemovedByUserID = uuidToPgtype(*input.RemovedByUserID)
	}
	row, err := r.queries.RemoveVipHandle(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("removing vip handle: %w", err)
	}
	return toDomainVipHandle(row), nil
}

func (r *Repository) IsVipHandle(ctx context.Context, storeID uuid.UUID, handle string) (bool, error) {
	return r.queries.IsVipHandle(ctx, sqlc.IsVipHandleParams{
		StoreID:        uuidToPgtype(storeID),
		PlatformHandle: normalizeHandle(handle),
	})
}

func (r *Repository) ListVipHandles(ctx context.Context, input ListVipHandlesInput) ([]*domain.VipHandle, int, error) {
	limit := input.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.queries.ListVipHandles(ctx, sqlc.ListVipHandlesParams{
		StoreID: uuidToPgtype(input.StoreID),
		Column2: input.IncludeInactive,
		Limit:   limit,
		Offset:  input.Offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("listing vip handles: %w", err)
	}
	total, err := r.queries.CountVipHandles(ctx, sqlc.CountVipHandlesParams{
		StoreID: uuidToPgtype(input.StoreID),
		Column2: input.IncludeInactive,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("counting vip handles: %w", err)
	}
	handles := make([]*domain.VipHandle, len(rows))
	for i := range rows {
		handles[i] = toDomainVipHandle(rows[i])
	}
	return handles, int(total), nil
}

// VipHandlesFor returns the subset of `handles` that are currently VIP for
// `storeID`. Batch lookup for order/customer detail badges.
func (r *Repository) VipHandlesFor(ctx context.Context, storeID uuid.UUID, handles []string) (map[string]bool, error) {
	if len(handles) == 0 {
		return map[string]bool{}, nil
	}
	normalized := make([]string, 0, len(handles))
	for _, h := range handles {
		if h = normalizeHandle(h); h != "" {
			normalized = append(normalized, h)
		}
	}
	rows, err := r.queries.ListVipHandlesForStore(ctx, sqlc.ListVipHandlesForStoreParams{
		StoreID: uuidToPgtype(storeID),
		Column2: normalized,
	})
	if err != nil {
		return nil, fmt.Errorf("looking up vip handles batch: %w", err)
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

// AddVipHandle marks an Instagram handle as VIP for the store and turns their
// existing open carts eternal via the VipCartActivator. Idempotent: re-adding
// bumps added_at.
func (s *Service) AddVipHandle(ctx context.Context, input AddVipInput) (*domain.VipHandle, error) {
	input.Handle = normalizeHandle(input.Handle)
	if input.Handle == "" {
		return nil, httpx.ErrBadRequest("handle is required")
	}

	vip, err := s.repo.AddVipHandle(ctx, input)
	if err != nil {
		return nil, err
	}

	logger.From(ctx, s.logger).Info("handle promoted to vip",
		zap.String("storeId", input.StoreID.String()),
		zap.String("handle", input.Handle),
	)

	if s.vipCartActivator != nil {
		// A linha do VIP já está no banco, então compras FUTURAS já caem no
		// carrinho eterno mesmo se a consolidação falhar aqui. Por isso a
		// promoção não vira erro — mas também não pode mais passar em silêncio:
		// o que falha aqui são os carrinhos que o comprador JÁ tem, e eles
		// continuam expirando. Quem chamou precisa saber para poder avisar.
		act, actErr := s.vipCartActivator.ActivateVipCartsForHandle(ctx, input.StoreID.String(), input.Handle)
		if actErr != nil {
			logger.From(ctx, s.logger).Error("failed to activate vip carts after promotion",
				zap.String("storeId", input.StoreID.String()),
				zap.String("handle", input.Handle),
				zap.Error(actErr),
			)
			vip.SetActivationFailed()
		} else {
			vip.SetCartsUpdated(activatedCount(act))
			vip.SetCartsMerged(act.Merged)
			vip.SetCartsSkipped(act.Skipped)
		}
	}

	return vip, nil
}

// RemoveVipHandle reverses a previous VIP mark. 404 if never a VIP. Carrinhos
// já eternos permanecem eternos: a remoção só impede NOVAS compras de caírem no
// carrinho eterno — não ressuscita a expiração de um carrinho vivo (seria
// mudar a regra de um carrinho que o comprador já está usando).
func (s *Service) RemoveVipHandle(ctx context.Context, input RemoveVipInput) (*domain.VipHandle, error) {
	input.Handle = normalizeHandle(input.Handle)
	if input.Handle == "" {
		return nil, httpx.ErrBadRequest("handle is required")
	}
	vip, err := s.repo.RemoveVipHandle(ctx, input)
	if err != nil {
		return nil, err
	}
	if vip == nil {
		return nil, httpx.ErrNotFound(fmt.Sprintf("handle %q is not a vip", input.Handle))
	}
	logger.From(ctx, s.logger).Info("handle removed from vip",
		zap.String("storeId", input.StoreID.String()),
		zap.String("handle", input.Handle),
	)
	return vip, nil
}

// IsVipHandle is the fast path used by the cart resolution in ingestion.
func (s *Service) IsVipHandle(ctx context.Context, storeID uuid.UUID, handle string) (bool, error) {
	return s.repo.IsVipHandle(ctx, storeID, handle)
}

func (s *Service) ListVipHandles(ctx context.Context, input ListVipHandlesInput) ([]*domain.VipHandle, int, error) {
	return s.repo.ListVipHandles(ctx, input)
}

func (s *Service) VipHandlesFor(ctx context.Context, storeID uuid.UUID, handles []string) (map[string]bool, error) {
	return s.repo.VipHandlesFor(ctx, storeID, handles)
}

// =============================================================================
// HANDLER
// =============================================================================

// RegisterVipRoutes wires the vip endpoints onto the customer route group.
func (h *Handler) RegisterVipRoutes(group fiber.Router) {
	group.Post("/vips", h.AddVip)
	group.Get("/vips", h.ListVips)
	group.Delete("/vips/:handle", h.RemoveVip)
}

// AddVip godoc
// @Summary      Mark a customer handle as VIP
// @Description  VIP customers get an eternal cart that accumulates items across events until paid or cancelled.
// @Tags         customers
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        request body AddVipRequest true "Handle to mark VIP"
// @Success      200 {object} httpx.Envelope{data=VipHandleResponse}
// @Router       /api/v1/stores/{storeId}/customers/vips [post]
// @Security     BearerAuth
func (h *Handler) AddVip(c *fiber.Ctx) error {
	var req AddVipRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	input, err := req.ToInput(httpx.GetStoreID(c), httpx.GetInternalUserID(c))
	if err != nil {
		return err
	}
	vip, err := h.service.AddVipHandle(c.UserContext(), input)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewVipHandleResponse(vip))
}

// RemoveVip godoc
// @Summary      Remove a customer handle from VIP
// @Tags         customers
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        handle path string true "Instagram handle (without @)"
// @Success      200 {object} httpx.Envelope{data=VipHandleResponse}
// @Router       /api/v1/stores/{storeId}/customers/vips/{handle} [delete]
// @Security     BearerAuth
func (h *Handler) RemoveVip(c *fiber.Ctx) error {
	storeID, err := uuid.Parse(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrBadRequest("invalid store id")
	}
	input := RemoveVipInput{StoreID: storeID, Handle: c.Params("handle")}
	if uid := httpx.GetInternalUserID(c); uid != "" {
		if parsed, err := uuid.Parse(uid); err == nil {
			input.RemovedByUserID = &parsed
		}
	}
	vip, err := h.service.RemoveVipHandle(c.UserContext(), input)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewVipHandleResponse(vip))
}

// ListVips godoc
// @Summary      List VIP customer handles
// @Tags         customers
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Success      200 {object} httpx.Envelope{data=[]VipHandleResponse}
// @Router       /api/v1/stores/{storeId}/customers/vips [get]
// @Security     BearerAuth
func (h *Handler) ListVips(c *fiber.Ctx) error {
	storeID, err := uuid.Parse(httpx.GetStoreID(c))
	if err != nil {
		return httpx.ErrBadRequest("invalid store id")
	}
	includeInactive := c.QueryBool("includeInactive", false)
	limit := int32(c.QueryInt("limit", 50))
	offset := int32(c.QueryInt("offset", 0))
	handles, total, err := h.service.ListVipHandles(c.UserContext(), ListVipHandlesInput{
		StoreID:         storeID,
		IncludeInactive: includeInactive,
		Limit:           limit,
		Offset:          offset,
	})
	if err != nil {
		return err
	}
	return httpx.OK(c, fiber.Map{
		"data":  NewListVipHandlesResponse(handles),
		"total": total,
	})
}

// =============================================================================
// HELPERS
// =============================================================================

func toDomainVipHandle(row sqlc.VipHandle) *domain.VipHandle {
	var addedByID *string
	var removedAt *time.Time
	if row.RemovedAt.Valid {
		t := row.RemovedAt.Time
		removedAt = &t
	}
	if row.AddedByUserID.Valid {
		id := pgtypeToUUID(row.AddedByUserID).String()
		addedByID = &id
	}
	return domain.ReconstructVipHandle(
		pgtypeToUUID(row.ID).String(),
		row.PlatformHandle,
		row.AddedAt.Time,
		removedAt,
		addedByID,
	)
}

// activatedCount mantém o contrato antigo de cartsUpdated: quantos carrinhos
// abertos passaram a nunca expirar. Com a consolidação isso é 1 (o eterno) ou 0
// (o @ não tinha carrinho aberto).
func activatedCount(a VipActivation) int {
	if a.EternalCartID == "" {
		return 0
	}
	return 1
}
