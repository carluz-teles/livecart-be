package order

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// ERPFinalisationRetrier is fulfilled by integration.Service. We invert the
// dependency so the order package doesn't import integration directly (which
// would loop the import graph).
type ERPFinalisationRetrier interface {
	RetryERPFinalisation(ctx context.Context, cartID, storeID string) error
}

// CartInvoiceSyncer is fulfilled by integration.Service. The order handler
// uses it to expose a manual "Verificar NFe" button without importing the
// integration package directly. We don't surface the fetched invoice state on
// this side of the boundary — the handler re-reads the order so the FE picks
// up the new erp_invoice_* columns through the regular GetDetailByID payload.
type CartInvoiceSyncer interface {
	SyncCartInvoiceFromERP(ctx context.Context, storeID, cartID, invoiceID string) error
}

// CartCanceller is fulfilled by integration.Service. Cancelar um pedido é, no
// modelo atual (pedido É o carrinho), cancelar o cart e desfazer tudo que ele
// segurava — estoque local, reserva/pedido no ERP e fila de espera. Essa
// orquestração vive no integration (que fala com o Tiny e com a waitlist), então
// invertemos a dependência para o order não importar integration.
type CartCanceller interface {
	CancelCart(ctx context.Context, cartID, storeID string) error
}

// BlockedHandleChecker reports whether a buyer's handle is currently blocked.
// Implemented by customer.Service and wired at boot to break the package cycle.
type BlockedHandleChecker interface {
	IsHandleBlocked(ctx context.Context, storeID uuid.UUID, handle string) (bool, error)
}

type Service struct {
	repo            *Repository
	logger          *zap.Logger
	erpRetryService ERPFinalisationRetrier
	invoiceSyncer   CartInvoiceSyncer
	blockChecker    BlockedHandleChecker
	cartCanceller   CartCanceller
	itemEditor      CartItemEditor
}

func NewService(repo *Repository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger.Named("order"),
	}
}

// SetERPFinalisationRetrier wires the integration service that knows how to
// rerun the post-payment ERP flow on a previously-failed cart. Called once
// at app boot from main.go after both services exist.
func (s *Service) SetERPFinalisationRetrier(r ERPFinalisationRetrier) {
	s.erpRetryService = r
}

// SetCartInvoiceSyncer wires the integration service for manual NFe sync.
// Same boot-time injection pattern as SetERPFinalisationRetrier — called from
// main.go once both services exist.
func (s *Service) SetCartInvoiceSyncer(syncer CartInvoiceSyncer) {
	s.invoiceSyncer = syncer
}

// SetBlockedHandleChecker wires customer.Service so GetDetailByID can flag
// orders whose buyer is currently blocked. Optional — if unset, the FE just
// never sees a "Cliente bloqueado" badge, which is the right degraded mode.
func (s *Service) SetBlockedHandleChecker(c BlockedHandleChecker) {
	s.blockChecker = c
}

// SetCartCanceller wires integration.Service, que sabe cancelar o cart e
// desfazer estoque/ERP/fila. Mesma injeção de boot dos demais.
func (s *Service) SetCartCanceller(c CartCanceller) {
	s.cartCanceller = c
}

// Cancel cancela um pedido NÃO PAGO por decisão do lojista.
//
// Aqui só moram as regras que o Order conhece: posse da loja (GetByID escopado)
// e o estado visível do pedido. A corrida de verdade — pagamento chegando no
// mesmo instante — é resolvida uma camada abaixo, com advisory lock e flip
// guard-first (integration.CancelCart): esta checagem é apenas o "não" barato e
// com mensagem boa; ela NÃO é a garantia.
func (s *Service) Cancel(ctx context.Context, orderID, storeID string) error {
	if s.cartCanceller == nil {
		return httpx.DomainError(422, httpx.CodeCancelUnavailable, "cancelamento indisponível")
	}
	order, err := s.GetByID(ctx, orderID, storeID)
	if err != nil {
		return err
	}
	if order.PaymentStatus == "paid" || order.PaymentStatus == "refunded" {
		return httpx.DomainError(409, httpx.CodeOrderAlreadyPaid, "pedido já foi pago — não é possível cancelar")
	}
	if order.Status == "cancelled" {
		return httpx.DomainError(409, httpx.CodeOrderAlreadyCancelled, "pedido já está cancelado")
	}
	if order.Status == "expired" {
		return httpx.DomainError(409, httpx.CodeOrderExpired, "pedido já expirou")
	}
	return s.cartCanceller.CancelCart(ctx, orderID, storeID)
}

// SyncInvoice fetches the NFe state from the ERP and persists it on the
// cart. The order detail returned by GetDetailByID picks up the new fields
// on the next read, so the handler can simply return the refreshed order
// after a successful sync.
//
// Returns nil error when the merchant hasn't emitted the NFe yet — the FE
// re-reads the cart and surfaces "Aguardando NFe na Tiny" via the absence of
// the erp_invoice_* fields, without treating it as an error.
func (s *Service) SyncInvoice(ctx context.Context, orderID, storeID string) error {
	if s.invoiceSyncer == nil {
		return httpx.DomainError(422, httpx.CodeNfeSyncUnavailable, "sync de NFe indisponível")
	}
	if _, err := s.GetByID(ctx, orderID, storeID); err != nil {
		return err
	}
	return s.invoiceSyncer.SyncCartInvoiceFromERP(ctx, storeID, orderID, "")
}

// RetryERPFinalisation re-runs the post-payment ERP order creation for an
// order whose Tiny finalisation previously failed. Validates that the order
// belongs to the store before delegating — the integration service trusts
// its callers to have done the storeID check.
func (s *Service) RetryERPFinalisation(ctx context.Context, orderID, storeID string) error {
	if s.erpRetryService == nil {
		return httpx.DomainError(422, httpx.CodeErpRetryUnavailable, "retry de ERP indisponível")
	}
	// Reuse the existing scoped lookup so a hostile storeID can't trigger a
	// retry on someone else's cart.
	if _, err := s.GetByID(ctx, orderID, storeID); err != nil {
		return err
	}
	return s.erpRetryService.RetryERPFinalisation(ctx, orderID, storeID)
}

func (s *Service) List(ctx context.Context, input ListOrdersInput) (ListOrdersOutput, error) {
	input.Pagination.Normalize()
	input.Sorting.Normalize("created_at")

	result, err := s.repo.List(ctx, ListOrdersParams{
		StoreID:    input.StoreID,
		Search:     input.Search,
		Pagination: input.Pagination,
		Sorting:    input.Sorting,
		Filters:    input.Filters,
	})
	if err != nil {
		return ListOrdersOutput{}, err
	}

	cartIDs := make([]string, len(result.Orders))
	for i, row := range result.Orders {
		cartIDs[i] = row.ID
	}
	previewByCart, err := s.repo.GetItemsPreviewByCartIDs(ctx, cartIDs)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to load item previews", zap.Error(err))
		previewByCart = map[string][]OrderItemPreviewRow{}
	}

	orders := make([]OrderOutput, len(result.Orders))
	for i, row := range result.Orders {
		previews := previewByCart[row.ID]
		previewOut := make([]OrderItemPreviewOutput, len(previews))
		for j, p := range previews {
			previewOut[j] = OrderItemPreviewOutput{
				ProductName:  p.ProductName,
				ProductImage: p.ProductImage,
				Quantity:     p.Quantity,
			}
		}
		orders[i] = OrderOutput{
			ID:                    row.ID,
			ShortID:               row.ShortID,
			EventID:               row.EventID,
			EventTitle:            row.LiveTitle,
			LivePlatform:          row.LivePlatform,
			CustomerHandle:        row.PlatformHandle,
			CustomerID:            row.PlatformUserID,
			CustomerName:          row.CustomerName,
			CustomerEmail:         row.CustomerEmail,
			FreeShipping:          row.FreeShipping,
			Status:                row.Status,
			PaymentStatus:         row.PaymentStatus,
			ShipmentStatus:        row.ShipmentStatus,
			HasShipping:           row.HasShipping,
			TotalItems:            row.TotalItems,
			TotalAmount:           row.TotalAmount,
			PaidAt:                row.PaidAt,
			CreatedAt:             row.CreatedAt,
			ExpiresAt:             row.ExpiresAt,
			IsFirstPurchase:       row.IsFirstPurchase,
			ERPFinalisationStatus: row.ERPFinalisationStatus,
			Items:                 []OrderItemOutput{}, // Items loaded separately when needed
			ItemsPreview:          previewOut,
		}
	}

	return ListOrdersOutput{
		Orders:     orders,
		Total:      result.Total,
		Pagination: input.Pagination,
	}, nil
}

func (s *Service) GetByID(ctx context.Context, id string, storeID string) (*OrderOutput, error) {
	row, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, httpx.ErrNotFound(fmt.Sprintf("order %s not found", id))
	}

	// Check store ownership
	if row.StoreID != storeID {
		return nil, httpx.ErrNotFound(fmt.Sprintf("order %s not found", id))
	}

	// Get items
	itemRows, err := s.repo.GetItems(ctx, id)
	if err != nil {
		return nil, err
	}

	items := make([]OrderItemOutput, len(itemRows))
	var totalAmount int64
	var totalItems int
	for i, item := range itemRows {
		itemTotal := item.UnitPrice * int64(item.Quantity)
		items[i] = OrderItemOutput{
			ID:                 item.ID,
			ProductID:          item.ProductID,
			ProductName:        item.ProductName,
			ProductImage:       item.ProductImage,
			Keyword:            item.ProductKeyword,
			Size:               item.Size,
			Quantity:           item.Quantity,
			UnitPrice:          item.UnitPrice,
			TotalPrice:         itemTotal,
			WaitlistedQuantity: item.WaitlistedQuantity,
			WeightGrams:        item.WeightGrams,
			HeightCm:           item.HeightCm,
			WidthCm:            item.WidthCm,
			LengthCm:           item.LengthCm,
			PackageFormat:      item.PackageFormat,
		}
		totalAmount += itemTotal
		totalItems += item.Quantity
	}

	return &OrderOutput{
		ID:                    row.ID,
		ShortID:               row.ShortID,
		EventID:               row.EventID,
		EventTitle:            row.LiveTitle,
		LivePlatform:          row.LivePlatform,
		CustomerHandle:        row.PlatformHandle,
		CustomerID:            row.PlatformUserID,
		Status:                row.Status,
		PaymentStatus:         row.PaymentStatus,
		Items:                 items,
		TotalItems:            totalItems,
		TotalAmount:           totalAmount,
		PaidAt:                row.PaidAt,
		CreatedAt:             row.CreatedAt,
		ExpiresAt:             row.ExpiresAt,
		IsFirstPurchase:       row.IsFirstPurchase,
		ERPFinalisationStatus: row.ERPFinalisationStatus,
	}, nil
}

func (s *Service) GetDetailByID(ctx context.Context, id string, storeID string) (*OrderDetailOutput, error) {
	// Get base order
	orderOutput, err := s.GetByID(ctx, id, storeID)
	if err != nil {
		return nil, err
	}

	// Get order detail row for event_id, platform_user_id, and all the extra
	// context captured at checkout (customer, shipping address, store origin).
	row, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	// Get customer comments for this event
	commentRows, err := s.repo.GetCustomerComments(ctx, row.EventID, row.PlatformUserID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to get customer comments", zap.Error(err))
		commentRows = []CommentRow{}
	}

	comments := make([]CommentOutput, len(commentRows))
	for i, c := range commentRows {
		comments[i] = CommentOutput{
			ID:        c.ID,
			Text:      c.Text,
			CreatedAt: c.CreatedAt,
		}
	}

	// Fila de espera do carrinho: produtos que a cliente pediu e a loja não
	// tinha. Best-effort — a fila é informação adicional, e perdê-la não pode
	// impedir o lojista de abrir o pedido.
	waitlist, err := s.repo.ListActiveWaitlist(ctx, id)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to load waitlist for order detail",
			zap.String("order_id", id), zap.Error(err))
		waitlist = []OrderWaitlistItemOutput{}
	}

	// Separa o que a cliente pode pagar do que está em fila. Uma passada só
	// pelos itens: quantity é o total pedido, waitlisted é a parcela sem
	// estoque, e as duas somas juntas fecham em TotalAmount.
	var payable, waitlisted int64
	for _, item := range orderOutput.Items {
		disponivel := item.Quantity - item.WaitlistedQuantity
		if disponivel > 0 {
			payable += item.UnitPrice * int64(disponivel)
		}
		if item.WaitlistedQuantity > 0 {
			waitlisted += item.UnitPrice * int64(item.WaitlistedQuantity)
		}
	}

	out := &OrderDetailOutput{
		OrderOutput:            *orderOutput,
		Token:                  row.Token,
		Comments:               comments,
		Waitlist:               waitlist,
		PayableAmount:          payable,
		WaitlistedAmount:       waitlisted,
		CancellationRevertedAt: row.CancellationRevertedAt,
	}

	// Customer: only expose when there's any data to surface.
	if row.CustomerName != "" || row.CustomerEmail != "" || row.CustomerDocument != "" || row.CustomerPhone != "" {
		out.Customer = &OrderCustomerOutput{
			Name:     row.CustomerName,
			Email:    row.CustomerEmail,
			Document: row.CustomerDocument,
			Phone:    row.CustomerPhone,
		}
	}

	// Shipping address (parsed from JSONB): require at least a zip code.
	if row.ShippingAddressZip != "" {
		out.ShippingAddress = &OrderShippingAddressOutput{
			ZipCode:      row.ShippingAddressZip,
			Street:       row.ShippingAddressStreet,
			Number:       row.ShippingAddressNumber,
			Complement:   row.ShippingAddressComplement,
			Neighborhood: row.ShippingAddressNeighborhood,
			City:         row.ShippingAddressCity,
			State:        row.ShippingAddressState,
		}
	}

	// Shipping selection (the freight option the customer picked).
	if row.ShippingServiceID != "" {
		out.Shipping = &OrderShippingOutput{
			Provider:      row.ShippingProvider,
			ServiceID:     row.ShippingServiceID,
			ServiceName:   row.ShippingServiceName,
			Carrier:       row.ShippingCarrier,
			CostCents:     row.ShippingCostCents,
			RealCostCents: row.ShippingCostRealCents,
			DeadlineDays:  row.ShippingDeadlineDays,
			FreeShipping:  row.EventFreeShipping,
		}
	}

	// Store info (always present; derived from the cart's event_id → store_id).
	out.Store = &OrderStoreOutput{
		ID:       row.StoreID,
		Name:     row.StoreName,
		LogoURL:  row.StoreLogoURL,
		Document: row.StoreCNPJ,
		Email:    row.StoreEmail,
		Phone:    row.StorePhone,
		Address: OrderShippingAddressOutput{
			ZipCode:      row.StoreAddressZip,
			Street:       row.StoreAddressStreet,
			Number:       row.StoreAddressNumber,
			Complement:   row.StoreAddressComplement,
			Neighborhood: row.StoreAddressDistrict,
			City:         row.StoreAddressCity,
			State:        row.StoreAddressState,
		},
		PackageWeightGrams: row.StoreDefaultPkgWeightGrams,
		PackageFormat:      row.StoreDefaultPkgFormat,
	}

	// ERP finalisation lifecycle. Always populated — the FE decides whether
	// to render the retry banner based on Status.
	out.ERPFinalisation = &ERPFinalisationOutput{
		Status:        row.ERPFinalisationStatus,
		LastError:     row.ERPLastError,
		LastAttemptAt: row.ERPLastAttemptAt,
		AttemptsCount: row.ERPAttemptsCount,
	}

	// ERP invoice (NFe). Pointer is left nil when nothing has been linked
	// yet so the FE can branch on its presence to show "Aguardando NFe"
	// vs the chave / status copy.
	if row.ERPInvoiceID != "" || row.ERPInvoiceKey != "" || row.ERPInvoiceStatus != "" {
		out.ERPInvoice = &ERPInvoiceOutput{
			InvoiceID:  row.ERPInvoiceID,
			InvoiceKey: row.ERPInvoiceKey,
			Status:     row.ERPInvoiceStatus,
			EmittedAt:  row.ERPInvoiceEmittedAt,
		}
	}

	// Buyer block status. Best-effort: log and skip on lookup error so the
	// detail page still loads even if the block table is unreachable.
	if s.blockChecker != nil && orderOutput.CustomerHandle != "" {
		if storeUUID, parseErr := uuid.Parse(storeID); parseErr == nil {
			if blocked, blockErr := s.blockChecker.IsHandleBlocked(ctx, storeUUID, orderOutput.CustomerHandle); blockErr == nil {
				out.CustomerBlocked = blocked
			} else {
				logger.From(ctx, s.logger).Warn("failed to look up block status for order detail",
					zap.String("order_id", id),
					zap.String("handle", orderOutput.CustomerHandle),
					zap.Error(blockErr),
				)
			}
		}
	}

	// Shipment (may be absent). Keyed by the real Order id (orders.id) — the
	// shipments FK was migrated off carts(id) → orders(id) — so resolve the
	// order id from the cart id first. No order yet (old/unpaid cart) → no
	// shipment. Events are loaded in a follow-up query.
	if orderID, rerr := s.repo.GetOrderIDByCartID(ctx, id); rerr != nil {
		logger.From(ctx, s.logger).Warn("failed to resolve order id for shipment", zap.Error(rerr))
	} else if orderID != "" {
		shipment, serr := s.repo.GetShipmentForOrder(ctx, orderID)
		if serr != nil {
			logger.From(ctx, s.logger).Warn("failed to load shipment for order", zap.Error(serr))
		} else if shipment != nil {
			events, eerr := s.repo.ListShipmentEvents(ctx, shipment.ID)
			if eerr != nil {
				logger.From(ctx, s.logger).Warn("failed to list shipment events", zap.Error(eerr))
				events = []OrderShipmentEventRecord{}
			}
			out.Shipment = &OrderShipmentOutput{
				OrderShipmentRecord: *shipment,
				Events:              events,
			}
		}
	}

	return out, nil
}

func (s *Service) Update(ctx context.Context, input UpdateOrderInput) (*OrderOutput, error) {
	// First verify the order exists and belongs to the store
	row, err := s.repo.GetByID(ctx, input.ID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, httpx.ErrNotFound(fmt.Sprintf("order %s not found", input.ID))
	}
	if row.StoreID != input.StoreID {
		return nil, httpx.ErrNotFound(fmt.Sprintf("order %s not found", input.ID))
	}

	// Update status if provided
	if input.Status != nil {
		if err := s.repo.UpdateStatus(ctx, input.ID, *input.Status); err != nil {
			return nil, err
		}
	}

	// Update payment status if provided
	if input.PaymentStatus != nil {
		if err := s.repo.UpdatePaymentStatus(ctx, input.ID, *input.PaymentStatus); err != nil {
			return nil, err
		}
	}

	// Return updated order
	return s.GetByID(ctx, input.ID, input.StoreID)
}

// GetUpsellSummary returns the upsell card payload for one order, scoped to
// the given store. Returns an empty (HasSnapshot=false) struct when the cart
// has no snapshot — the frontend treats that as "no changes during checkout".
func (s *Service) GetUpsellSummary(ctx context.Context, id, storeID string) (*OrderUpsellOutput, error) {
	row, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if row == nil || row.StoreID != storeID {
		return nil, httpx.ErrNotFound(fmt.Sprintf("order %s not found", id))
	}
	out, err := s.repo.GetUpsellSummary(ctx, id)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return &OrderUpsellOutput{HasSnapshot: false, InitialItems: []OrderUpsellItem{}, Mutations: []OrderUpsellMutation{}}, nil
	}
	if out.InitialItems == nil {
		out.InitialItems = []OrderUpsellItem{}
	}
	if out.Mutations == nil {
		out.Mutations = []OrderUpsellMutation{}
	}
	return out, nil
}

// UpdateShippingAddress lets the admin fix a buyer-typed mistake (wrong
// number, missing complement, etc.) before the shipment is created. Hard
// blocks: order must belong to the store, must not be paid, and must not
// have a shipment row — once any of those is true editing the address would
// silently desynchronize with the carrier or the buyer's receipt.
func (s *Service) UpdateShippingAddress(ctx context.Context, input UpdateShippingAddressInput) error {
	row, err := s.repo.GetByID(ctx, input.ID)
	if err != nil {
		return err
	}
	if row == nil || row.StoreID != input.StoreID {
		return httpx.ErrNotFound(fmt.Sprintf("order %s not found", input.ID))
	}
	if row.PaymentStatus == "paid" {
		return httpx.DomainError(409, httpx.CodeOrderAddressLocked, "cannot edit shipping address after payment")
	}
	// Shipment guard keyed by the real Order id (orders.id). Resolve from the
	// cart id first; no order yet → no shipment → editing is allowed.
	if orderID, rerr := s.repo.GetOrderIDByCartID(ctx, input.ID); rerr != nil {
		logger.From(ctx, s.logger).Warn("failed to resolve order id for shipment", zap.Error(rerr))
	} else if orderID != "" {
		shipment, err := s.repo.GetShipmentForOrder(ctx, orderID)
		if err != nil {
			logger.From(ctx, s.logger).Warn("failed to load shipment for order", zap.Error(err))
		} else if shipment != nil {
			return httpx.DomainError(409, httpx.CodeOrderAddressLocked, "cannot edit shipping address after shipment is created")
		}
	}
	return s.repo.UpdateShippingAddress(ctx, input.ID, input.Address)
}

// RegenerateCheckout extends the cart's expiration window and resets it to
// a state where the buyer can complete payment again. Used when a PIX expired
// or the buyer abandoned the flow. Returns the cart token + new expiry so
// the admin UI can build a public link to share.
func (s *Service) RegenerateCheckout(
	ctx context.Context,
	id string,
	storeID string,
) (string, time.Time, error) {
	row, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return "", time.Time{}, err
	}
	if row == nil || row.StoreID != storeID {
		return "", time.Time{}, httpx.ErrNotFound(fmt.Sprintf("order %s not found", id))
	}
	if row.PaymentStatus == "paid" {
		return "", time.Time{}, httpx.DomainError(409, httpx.CodeOrderCheckoutLocked, "cannot regenerate checkout for a paid order")
	}
	// Cart cancelado não volta pelo "regerar link": o cancelamento já devolveu
	// o estoque e estornou a reserva no ERP, e o regenerate só mexe em
	// status/prazo — reabri-lo criaria um checkout vendendo unidade que não
	// está mais reservada para ninguém.
	if row.Status == "cancelled" {
		return "", time.Time{}, httpx.DomainError(409, httpx.CodeOrderRegenCancelled, "pedido cancelado — não é possível regerar o link")
	}
	// Shipment guard keyed by the real Order id (orders.id). Resolve from the
	// cart id first; no order yet → no shipment → regenerating is allowed.
	if orderID, rerr := s.repo.GetOrderIDByCartID(ctx, id); rerr != nil {
		logger.From(ctx, s.logger).Warn("failed to resolve order id for shipment", zap.Error(rerr))
	} else if orderID != "" {
		shipment, err := s.repo.GetShipmentForOrder(ctx, orderID)
		if err != nil {
			logger.From(ctx, s.logger).Warn("failed to load shipment for order", zap.Error(err))
		} else if shipment != nil {
			return "", time.Time{}, httpx.DomainError(409, httpx.CodeOrderCheckoutLocked, "cannot regenerate checkout after shipment is created")
		}
	}

	// RN-34: o prazo é o do EVENTO (curto ou estendido conforme
	// close_cart_on_event_end), com fallback para a loja. Antes lia só
	// stores.cart_expiration_minutes: um evento configurado com prazo estendido
	// de 7 dias dava 30 minutos ao comprador quando o lojista regerava o link.
	minutes := s.repo.GetEventCartExpirationMinutes(ctx, row.EventID)
	expiresAt := time.Now().Add(time.Duration(minutes) * time.Minute)

	if err := s.repo.RegenerateCheckout(ctx, id, expiresAt); err != nil {
		return "", time.Time{}, err
	}
	return row.Token, expiresAt, nil
}

func (s *Service) GetStats(ctx context.Context, storeID string, search string, filters OrderFilters) (*OrderStatsOutput, error) {
	stats, err := s.repo.GetStats(ctx, storeID, search, filters)
	if err != nil {
		return nil, err
	}
	return stats, nil
}
