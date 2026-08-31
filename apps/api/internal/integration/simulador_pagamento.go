package integration

// O simulador de PAGAMENTOS — SÓ EM STAGING.
//
// Irmão do simulador de live, e pelo mesmo motivo: em staging não há gateway
// cobrando de verdade. O Mercado Pago sandbox exige conta de comprador de teste
// e um QR que ninguém escaneia; o Pagar.me não simula webhook. Sem pagamento
// entrando não dá para exercitar o que vem DEPOIS dele — que é metade do
// sistema: o pedido no ERP fecha, o cupom conta uso, a fila de espera promove,
// o e-mail sai, a comissão é lançada.
//
// ═══ O QUE ELE SIMULA, E O QUE NÃO ═══
//
// Ele simula O GATEWAY, e só. Monta o `providers.PaymentStatus` que o Mercado
// Pago ou o Pagar.me devolveria e o entrega a payment.Service —
// AplicarStatusDePagamento, que é literalmente a função que o webhook real
// chama depois de consultar o gateway. Daí para a frente não existe caminho
// paralelo: a escrita guardada no carrinho, o fato `cart.paid` e todos os
// reatores que o consomem rodam exatamente como em produção.
//
// Ele NÃO entra por ProcessPaymentNotification porque o primeiro gesto de lá é
// RECONSULTAR o gateway pelo id do pagamento — e num pagamento simulado não há
// o que consultar. É a única costura, e ela é o ponto mais alto possível.
//
// O cupom NÃO é simulado: o simulador aplica um cupom DE VERDADE, pelo mesmo
// coupon.Service.ApplyToCart que a compradora aciona no checkout. Inventar um
// desconto aqui gravaria `coupon_discount_cents` sem cupom nenhum atrás, e o
// pedido no ERP e a comissão veriam um abatimento sem origem.
//
// ═══ SEGURANÇA ═══
//
// Mesmas três camadas do simulador de live, e pela mesma razão — aqui o risco é
// maior, porque forjar pagamento marca venda como paga: as rotas NÃO SÃO
// REGISTRADAS fora de staging, cada handler reconfere `config.IsStaging()`, e
// tudo mora sob o grupo autenticado por loja.

import (
	"fmt"
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/go-ozzo/ozzo-validation/v4/is"
	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	paymentdomain "livecart/apps/api/internal/payment"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// RegisterPaymentSimulatorRoutes monta o simulador — e só monta em staging.
func (h *WebhookHandler) RegisterPaymentSimulatorRoutes(router fiber.Router) {
	if !config.IsStaging() {
		return
	}
	g := router.Group("/simulador/pagamento", h.somenteStagingPagamento)
	g.Get("/carrinhos", h.SimularListarCarrinhos)
	g.Post("/pagar", h.SimularPagamento)
}

func (h *WebhookHandler) somenteStagingPagamento(c *fiber.Ctx) error {
	if !config.IsStaging() {
		return httpx.DomainError(403, httpx.CodeStagingOnly,
			"o simulador de pagamentos existe apenas em staging")
	}
	return c.Next()
}

// =============================================================================
// LISTAR O QUE DÁ PARA PAGAR
// =============================================================================

// CarrinhoPagavel é um carrinho aberto, com a conta do dinheiro pronta.
//
// A conta vem montada do servidor porque é ELA que o simulador vai cobrar. Se a
// tela recalculasse por conta própria, existiriam duas fórmulas para o mesmo
// número e a divergência apareceria como "o simulador cobrou diferente do que
// mostrou".
type CarrinhoPagavel struct {
	CartID          string    `json:"cartId"`
	ShortID         int32     `json:"shortId"`
	Handle          string    `json:"handle"`
	EventID         string    `json:"eventId"`
	EventTitle      string    `json:"eventTitle"`
	Itens           int32     `json:"itens"`
	SubtotalCents   int64     `json:"subtotalCents"`
	ShippingCents   int64     `json:"shippingCents"`
	CupomCodigo     string    `json:"cupomCodigo,omitempty"`
	CupomDescCents  int64     `json:"cupomDescontoCents"`
	PixPercentEvent int32     `json:"pixPercentDoEvento"`
	CriadoEm        time.Time `json:"criadoEm"`
}

// CupomDoEvento é um cupom que existe de verdade e pode ser aplicado.
type CupomDoEvento struct {
	Codigo     string `json:"codigo"`
	Tipo       string `json:"tipo"`
	PercentBPS int    `json:"percentBps,omitempty"`
	ValorCents int64  `json:"valorCents,omitempty"`
	EventID    string `json:"eventId"`
}

// CarrinhosPagaveisResponse é o que a tela precisa para montar o formulário.
type CarrinhosPagaveisResponse struct {
	Carrinhos []CarrinhoPagavel `json:"carrinhos"`
	Cupons    []CupomDoEvento   `json:"cupons"`
}

// SimularListarCarrinhos lista os carrinhos abertos da loja.
//
// @Summary  Carrinhos que o simulador pode pagar (staging)
// @Tags     simulador
// @Success  200 {object} httpx.Envelope{data=CarrinhosPagaveisResponse}
// @Router   /stores/{storeId}/simulador/pagamento/carrinhos [get]
func (h *WebhookHandler) SimularListarCarrinhos(c *fiber.Ctx) error {
	storeID, _ := c.Locals("store_id").(string)

	out, err := h.service.ListarCarrinhosPagaveis(c.Context(), storeID)
	if err != nil {
		return err
	}
	return httpx.OK(c, out)
}

// =============================================================================
// PAGAR
// =============================================================================

// SimularPagamentoRequest é o gesto: quem paga, como, e com qual abatimento.
type SimularPagamentoRequest struct {
	CartID string `json:"cartId"`
	// Metodo é "pix" ou "credit_card" — os dois nomes que os gateways usam e
	// que o resto do sistema já sabe ler.
	Metodo string `json:"metodo"`
	// CupomCodigo aplica um cupom DE VERDADE antes de cobrar, pelo mesmo
	// caminho do checkout. Vazio = não mexe no cupom que o carrinho já tenha.
	CupomCodigo string `json:"cupomCodigo"`
	// PixDescontoPercent sobrepõe o percentual do evento NESTA cobrança.
	//
	// Sobrepor é fiel, e não um atalho: num pagamento simulado nós SOMOS o
	// gateway, e o desconto de PIX é justamente o gateway cobrando menos. -1
	// significa "usa o do evento".
	PixDescontoPercent int `json:"pixDescontoPercent"`
	// Parcelas do cartão. Ignorado no PIX.
	Parcelas int `json:"parcelas"`
}

func (r SimularPagamentoRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.CartID, validation.Required, is.UUIDv4),
		validation.Field(&r.Metodo, validation.Required, validation.In("pix", "credit_card")),
		validation.Field(&r.CupomCodigo, validation.Length(0, 64)),
		validation.Field(&r.PixDescontoPercent, validation.Min(-1), validation.Max(100)),
		validation.Field(&r.Parcelas, validation.Min(0), validation.Max(12)),
	)
}

// SimularPagamentoResponse mostra a conta que foi cobrada, aberta.
//
// Aberta de propósito: o número final sozinho não deixa conferir nada, e a
// primeira pergunta de quem testa desconto é sempre "de onde saiu esse valor".
type SimularPagamentoResponse struct {
	CartID         string `json:"cartId"`
	PaymentID      string `json:"paymentId"`
	Metodo         string `json:"metodo"`
	SubtotalCents  int64  `json:"subtotalCents"`
	CupomCodigo    string `json:"cupomCodigo,omitempty"`
	CupomDescCents int64  `json:"cupomDescontoCents"`
	PixPercent     int    `json:"pixDescontoPercent"`
	PixDescCents   int64  `json:"pixDescontoCents"`
	ShippingCents  int64  `json:"shippingCents"`
	CobradoCents   int64  `json:"cobradoCents"`
}

// SimularPagamento encena o gateway aprovando a cobrança.
//
// @Summary  Marca um carrinho como pago, como o gateway faria (staging)
// @Tags     simulador
// @Success  200 {object} httpx.Envelope{data=SimularPagamentoResponse}
// @Router   /stores/{storeId}/simulador/pagamento/pagar [post]
func (h *WebhookHandler) SimularPagamento(c *fiber.Ctx) error {
	var req SimularPagamentoRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.DomainError(400, httpx.CodeValidationFailed, "corpo inválido")
	}
	if err := req.Validate(); err != nil {
		return err
	}
	if h.payment == nil {
		return httpx.DomainError(503, httpx.CodeInternal, "serviço de pagamento não configurado")
	}
	storeID, _ := c.Locals("store_id").(string)

	// A conta sai do servidor, com o cupom já aplicado pelo caminho real.
	conta, err := h.service.PrepararCobrancaSimulada(c.Context(), storeID, req)
	if err != nil {
		return err
	}

	// O GATEWAY SIMULADO. `providers.PaymentStatus` é a MESMA estrutura que os
	// adapters do Mercado Pago e do Pagar.me devolvem — daqui para a frente
	// ninguém distingue um pagamento simulado de um real.
	pagoEm := time.Now()
	liberacao := pagoEm.AddDate(0, 0, 1)
	status := &providers.PaymentStatus{
		PaymentID:         conta.PaymentID,
		Status:            providers.PaymentApproved,
		Amount:            conta.CobradoCents,
		PaidAt:            &pagoEm,
		ExternalReference: conta.CartID,
		PaymentMethod:     conta.Metodo,
		Installments:      conta.Parcelas,
		MoneyReleaseDate:  &liberacao,
		Metadata:          map[string]any{"simulado": true, "origem": "simulador-staging"},
	}

	// A MESMA função que o webhook real chama depois de consultar o gateway.
	if err := h.payment.AplicarStatusDePagamento(c.Context(),
		paymentdomain.ProcessPaymentInput{
			StoreID:   storeID,
			Provider:  conta.Provider,
			PaymentID: conta.PaymentID,
		}, status); err != nil {
		return fmt.Errorf("aplicando o pagamento simulado: %w", err)
	}

	logger.From(c.Context(), h.logger).Info("payment simulator approved a charge",
		zap.String("cart_id", conta.CartID),
		zap.String("payment_id", conta.PaymentID),
		zap.String("metodo", conta.Metodo),
		zap.Int64("cobrado_cents", conta.CobradoCents),
		zap.Int64("cupom_desconto_cents", conta.CupomDescCents),
		zap.Int("pix_desconto_percent", conta.PixPercent),
	)
	return httpx.OK(c, conta.Resposta())
}
