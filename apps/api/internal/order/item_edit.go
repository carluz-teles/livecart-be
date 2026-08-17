package order

// Edição dos itens de um pedido AGUARDANDO PAGAMENTO, pelo painel do lojista.
//
// Nada aqui reimplementa a mecânica de mutação. Ela já existe, madura, no
// checkout público — e é onde moram as coisas que só se aprende em produção:
// trava otimista contra a janela de segundos da chamada ao Tiny, decremento que
// tira primeiro da parcela em FILA (a que não segura estoque), rollback do
// carrinho quando o ERP recusa, cancelamento da cobrança PIX pendente no
// gateway, reavaliação de cupom e promoção do próximo da fila quando estoque
// volta. Reescrever isso para o lojista seria reescrever cada um desses bugs.
//
// O que este arquivo faz é abrir a porta certa: valida a entrada, resolve o
// pedido pela consulta ESCOPADA POR LOJA (é ela que garante que a lojista não
// edita carrinho de outra loja) e delega marcando a edição como do lojista.

import (
	"context"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/go-ozzo/ozzo-validation/v4/is"
	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

// =============================================================================
// PORTA PARA O CHECKOUT
// =============================================================================

// CartItemEditor é cumprida por checkout.Service. Invertida para o order não
// importar o checkout (o grafo de import fecharia em ciclo), mesmo padrão de
// ERPFinalisationRetrier e CartCanceller.
//
// As três assinaturas recebem o TOKEN do carrinho, não o id: é o identificador
// que o checkout usa para carregar carrinho, e reusá-lo evita escrever uma
// segunda consulta de carregamento. Quem resolve o token a partir do pedido é
// este pacote, depois de checar a posse da loja.
type CartItemEditor interface {
	AddCartItemAsMerchant(ctx context.Context, token, productID string, quantity int) error
	SetCartItemQuantityAsMerchant(ctx context.Context, token, itemID string, quantity int) error
	RemoveCartItemAsMerchant(ctx context.Context, token, itemID string) error
}

// SetCartItemEditor liga o checkout.Service no boot, depois que os dois existem.
func (s *Service) SetCartItemEditor(e CartItemEditor) {
	s.itemEditor = e
}

// =============================================================================
// REQUESTS
// =============================================================================

// maxItemQuantity é o teto de sanidade da quantidade digitada no painel.
//
// Não é a regra de negócio: o teto por produto do evento e o estoque são
// checados no checkout, com os números certos. Aqui é só o portão sintático que
// impede um dedo escorregando no teclado ("100" virando "1000000") de virar uma
// tentativa de reserva absurda no ERP.
const maxItemQuantity = 999

// AddOrderItemRequest é o corpo de POST /orders/:id/items.
type AddOrderItemRequest struct {
	ProductID string `json:"productId"`
	Quantity  int    `json:"quantity"`
}

// Validate é o portão sintático (ozzo).
//
// Quantity leva Required junto com Min: sem ele, o ozzo pula toda regra no
// zero-value e um corpo sem o campo passaria como 0 — que o checkout recusaria
// depois, mas com mensagem pior e uma ida ao banco no meio.
func (r AddOrderItemRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.ProductID, validation.Required, is.UUIDv4),
		validation.Field(&r.Quantity, validation.Required,
			validation.Min(1), validation.Max(maxItemQuantity)),
	)
}

// ToInput carrega o escopo do pedido e da loja.
func (r AddOrderItemRequest) ToInput(orderID, storeID string) (AddOrderItemInput, error) {
	return AddOrderItemInput{
		OrderID:   orderID,
		StoreID:   storeID,
		ProductID: r.ProductID,
		Quantity:  r.Quantity,
	}, nil
}

type AddOrderItemInput struct {
	OrderID   string
	StoreID   string
	ProductID string
	Quantity  int
}

// SetOrderItemQuantityRequest é o corpo de PATCH /orders/:id/items/:itemId.
//
// Quantidade ABSOLUTA, não delta. Um delta ("+1") aplicado sobre um valor que
// mudou no servidor produz um resultado que ninguém pediu; a quantidade
// absoluta deixa a trava otimista do checkout recusar o passo obsoleto e a tela
// recarregar. Remover é o DELETE, não quantidade 0.
type SetOrderItemQuantityRequest struct {
	Quantity int `json:"quantity"`
}

func (r SetOrderItemQuantityRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Quantity, validation.Required,
			validation.Min(1), validation.Max(maxItemQuantity)),
	)
}

func (r SetOrderItemQuantityRequest) ToInput(orderID, storeID, itemID string) (SetOrderItemQuantityInput, error) {
	return SetOrderItemQuantityInput{
		OrderID:  orderID,
		StoreID:  storeID,
		ItemID:   itemID,
		Quantity: r.Quantity,
	}, nil
}

type SetOrderItemQuantityInput struct {
	OrderID  string
	StoreID  string
	ItemID   string
	Quantity int
}

type RemoveOrderItemInput struct {
	OrderID string
	StoreID string
	ItemID  string
}

// =============================================================================
// SERVICE
// =============================================================================

// resolveEditableCartToken devolve o token do carrinho do pedido, garantindo
// que ele pertence à loja que está pedindo.
//
// GetByID é escopado por store_id e devolve 404 quando a loja não é a dona —
// então a posse é resolvida aqui, uma vez, e o checkout pode confiar no token
// que recebe (mesma fronteira de RetryERPFinalisation e SyncInvoice).
//
// Os estados que impedem a edição (pago, cancelado, expirado) NÃO são checados
// aqui: quem decide é o checkout, com a leitura fresca do carrinho e no mesmo
// instante da escrita. Duplicar a checagem daria uma segunda fonte da verdade
// que divergiria — e a corrida real (pagamento entrando agora) só o lado de lá
// resolve.
func (s *Service) resolveEditableCartToken(ctx context.Context, orderID, storeID string) (string, error) {
	if s.itemEditor == nil {
		return "", httpx.DomainError(422, httpx.CodeCartEditUnavailable,
			"edição de itens indisponível")
	}
	detail, err := s.repo.GetByID(ctx, orderID)
	if err != nil {
		return "", err
	}
	if detail == nil || detail.StoreID != storeID {
		return "", httpx.ErrNotFound("pedido não encontrado")
	}
	if detail.Token == "" {
		return "", httpx.ErrUnprocessable("pedido sem link de checkout para editar")
	}
	return detail.Token, nil
}

// AddItem adiciona um produto do catálogo ao pedido.
func (s *Service) AddItem(ctx context.Context, input AddOrderItemInput) error {
	token, err := s.resolveEditableCartToken(ctx, input.OrderID, input.StoreID)
	if err != nil {
		return err
	}
	return s.itemEditor.AddCartItemAsMerchant(ctx, token, input.ProductID, input.Quantity)
}

// SetItemQuantity fixa a quantidade de um item já no pedido.
func (s *Service) SetItemQuantity(ctx context.Context, input SetOrderItemQuantityInput) error {
	token, err := s.resolveEditableCartToken(ctx, input.OrderID, input.StoreID)
	if err != nil {
		return err
	}
	return s.itemEditor.SetCartItemQuantityAsMerchant(ctx, token, input.ItemID, input.Quantity)
}

// RemoveItem tira um item do pedido.
func (s *Service) RemoveItem(ctx context.Context, input RemoveOrderItemInput) error {
	token, err := s.resolveEditableCartToken(ctx, input.OrderID, input.StoreID)
	if err != nil {
		return err
	}
	return s.itemEditor.RemoveCartItemAsMerchant(ctx, token, input.ItemID)
}

// =============================================================================
// HANDLERS
// =============================================================================

// AddItem godoc
// @Summary      Add a catalog product to an unpaid order
// @Description  Merchant-side edit of an order still awaiting payment. Reserves stock, records the mutation as source=merchant, cancels any pending PIX and clears the shipping selection.
// @Tags         orders
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Order (cart) UUID"
// @Param        request body AddOrderItemRequest true "Product and quantity"
// @Success      200 {object} httpx.Envelope{data=OrderDetailResponse}
// @Failure      409 {object} httpx.Envelope "order already paid / cancelled"
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/orders/{id}/items [post]
// @Security     BearerAuth
func (h *Handler) AddItem(c *fiber.Ctx) error {
	var req AddOrderItemRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	input, err := req.ToInput(c.Params("id"), httpx.GetStoreID(c))
	if err != nil {
		return err
	}
	if err := h.service.AddItem(c.UserContext(), input); err != nil {
		return err
	}
	return h.respondWithDetail(c, input.OrderID, input.StoreID)
}

// SetItemQuantity godoc
// @Summary      Set the quantity of an item on an unpaid order
// @Tags         orders
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Order (cart) UUID"
// @Param        itemId path string true "Cart item UUID"
// @Param        request body SetOrderItemQuantityRequest true "Absolute quantity"
// @Success      200 {object} httpx.Envelope{data=OrderDetailResponse}
// @Failure      409 {object} httpx.Envelope "item changed while editing / order paid"
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/orders/{id}/items/{itemId} [patch]
// @Security     BearerAuth
func (h *Handler) SetItemQuantity(c *fiber.Ctx) error {
	var req SetOrderItemQuantityRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	input, err := req.ToInput(c.Params("id"), httpx.GetStoreID(c), c.Params("itemId"))
	if err != nil {
		return err
	}
	if err := h.service.SetItemQuantity(c.UserContext(), input); err != nil {
		return err
	}
	return h.respondWithDetail(c, input.OrderID, input.StoreID)
}

// RemoveItem godoc
// @Summary      Remove an item from an unpaid order
// @Tags         orders
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Order (cart) UUID"
// @Param        itemId path string true "Cart item UUID"
// @Success      200 {object} httpx.Envelope{data=OrderDetailResponse}
// @Failure      409 {object} httpx.Envelope "order already paid / cancelled"
// @Router       /api/v1/stores/{storeId}/orders/{id}/items/{itemId} [delete]
// @Security     BearerAuth
func (h *Handler) RemoveItem(c *fiber.Ctx) error {
	input := RemoveOrderItemInput{
		OrderID: c.Params("id"),
		StoreID: httpx.GetStoreID(c),
		ItemID:  c.Params("itemId"),
	}
	if err := h.service.RemoveItem(c.UserContext(), input); err != nil {
		return err
	}
	return h.respondWithDetail(c, input.OrderID, input.StoreID)
}

// respondWithDetail devolve o pedido inteiro relido.
//
// A edição muda mais do que a linha tocada — total, subtotal pagável, valor em
// fila, frete (que é descartado) e o histórico de mutações. Devolver só o item
// obrigaria a tela a adivinhar o resto, ou a fazer uma segunda requisição que
// pode cruzar com outra edição.
func (h *Handler) respondWithDetail(c *fiber.Ctx, orderID, storeID string) error {
	detail, err := h.service.GetDetailByID(c.UserContext(), orderID, storeID)
	if err != nil {
		return err
	}
	return httpx.OK(c, NewOrderDetailResponse(*detail))
}

// =============================================================================
// PAGAMENTO MANUAL
// =============================================================================

// ManualPaymentConfirmer é cumprida por payment.Service. Invertida pelo mesmo
// motivo das demais: o order não importa o payment.
type ManualPaymentConfirmer interface {
	ConfirmManualPayment(ctx context.Context, cartID, storeID string) error
}

// SetManualPaymentConfirmer liga o payment.Service no boot.
func (s *Service) SetManualPaymentConfirmer(c ManualPaymentConfirmer) {
	s.manualPayment = c
}

// ConfirmManualPayment marca como pago um pedido cujo dinheiro entrou por fora.
//
// A posse da loja é resolvida aqui, pela mesma consulta escopada que as demais
// ações usam — o payment confia no par (cart, store) que recebe. Os estados que
// impedem a confirmação (já pago, estornado, expirado) são decididos do outro
// lado, com a leitura fresca no instante da escrita: duplicar aqui daria uma
// segunda fonte da verdade, e a corrida real (o pagamento do gateway entrando
// agora) só o lado de lá resolve.
func (s *Service) ConfirmManualPayment(ctx context.Context, orderID, storeID string) error {
	if s.manualPayment == nil {
		return httpx.DomainError(422, httpx.CodeManualPaymentUnavailable,
			"confirmação manual de pagamento indisponível")
	}
	if _, err := s.GetByID(ctx, orderID, storeID); err != nil {
		return err
	}
	return s.manualPayment.ConfirmManualPayment(ctx, orderID, storeID)
}

// ConfirmManualPayment godoc
// @Summary      Mark an unpaid order as paid outside LiveCart
// @Description  Runs the SAME cycle as a gateway payment: materialises the Order, creates the ERP sales order and launches stock. No financial entry is sent to the ERP — the merchant records that there.
// @Tags         orders
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        id path string true "Order (cart) UUID"
// @Success      200 {object} httpx.Envelope{data=OrderDetailResponse}
// @Failure      409 {object} httpx.Envelope "already paid / refunded / expired"
// @Failure      422 {object} httpx.Envelope "no items to send"
// @Router       /api/v1/stores/{storeId}/orders/{id}/confirm-manual-payment [post]
// @Security     BearerAuth
func (h *Handler) ConfirmManualPayment(c *fiber.Ctx) error {
	orderID := c.Params("id")
	storeID := httpx.GetStoreID(c)

	if err := h.service.ConfirmManualPayment(c.UserContext(), orderID, storeID); err != nil {
		return err
	}
	return h.respondWithDetail(c, orderID, storeID)
}
