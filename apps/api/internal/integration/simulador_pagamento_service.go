package integration

// A camada de serviço do simulador de pagamentos. Ver a nota do topo de
// simulador_pagamento.go para o desenho e para a segurança.

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"go.uber.org/zap"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// CouponApplier é o caminho REAL de aplicar cupom, o mesmo que a compradora
// aciona no checkout. O simulador não tem um seu.
type CouponApplier interface {
	AplicarCupomNoCarrinho(ctx context.Context, cartToken, codigo string) (int64, error)
	ListarCuponsDoEvento(ctx context.Context, eventID, storeID string) ([]CupomDoEvento, error)
}

// SetCouponApplier liga o serviço de cupons. Opcional: sem ele o simulador
// segue funcionando e apenas recusa aplicar cupom.
func (s *Service) SetCouponApplier(a CouponApplier) { s.coupons = a }

// ListarCarrinhosPagaveis devolve o que o simulador pode pagar, com a conta do
// dinheiro já montada pelo banco.
func (s *Service) ListarCarrinhosPagaveis(ctx context.Context, storeID string) (*CarrinhosPagaveisResponse, error) {
	linhas, err := s.repo.ListarCarrinhosPagaveis(ctx, storeID)
	if err != nil {
		return nil, fmt.Errorf("listando carrinhos pagáveis: %w", err)
	}

	out := &CarrinhosPagaveisResponse{Carrinhos: linhas}

	// Os cupons dos eventos que aparecem na lista — é deles que a tela monta o
	// seletor. Sem evento na lista não há cupom a oferecer.
	if s.coupons != nil {
		vistos := map[string]bool{}
		for _, c := range linhas {
			if c.EventID == "" || vistos[c.EventID] {
				continue
			}
			vistos[c.EventID] = true
			cupons, err := s.coupons.ListarCuponsDoEvento(ctx, c.EventID, storeID)
			if err != nil {
				logger.From(ctx, s.logger).Debug("simulador: não consegui listar cupons do evento",
					zap.String("event_id", c.EventID), zap.Error(err))
				continue
			}
			out.Cupons = append(out.Cupons, cupons...)
		}
	}
	return out, nil
}

// CobrancaSimulada é a conta fechada, pronta para virar PaymentStatus.
type CobrancaSimulada struct {
	CartID         string
	PaymentID      string
	Provider       string
	Metodo         string
	Parcelas       int
	SubtotalCents  int64
	CupomCodigo    string
	CupomDescCents int64
	PixPercent     int
	PixDescCents   int64
	ShippingCents  int64
	CobradoCents   int64
}

// Resposta é a conta aberta para a tela conferir.
func (c CobrancaSimulada) Resposta() SimularPagamentoResponse {
	return SimularPagamentoResponse{
		CartID: c.CartID, PaymentID: c.PaymentID, Metodo: c.Metodo,
		SubtotalCents: c.SubtotalCents, CupomCodigo: c.CupomCodigo,
		CupomDescCents: c.CupomDescCents, PixPercent: c.PixPercent,
		PixDescCents: c.PixDescCents, ShippingCents: c.ShippingCents,
		CobradoCents: c.CobradoCents,
	}
}

// PrepararCobrancaSimulada aplica o cupom (pelo caminho real) e fecha a conta.
//
// A ORDEM das parcelas do desconto é a mesma do checkout, e ela importa: o
// desconto de PIX incide sobre (subtotal − cupom), nunca sobre o frete. Inverter
// isso daria um número plausível e errado, do tipo que só aparece na conciliação
// do lojista semanas depois.
func (s *Service) PrepararCobrancaSimulada(
	ctx context.Context, storeID string, req SimularPagamentoRequest,
) (*CobrancaSimulada, error) {
	carrinho, err := s.repo.CarrinhoPagavelPorID(ctx, storeID, req.CartID)
	if err != nil {
		return nil, err
	}

	// O cupom entra ANTES, pelo mesmo ApplyToCart do checkout — e o valor do
	// desconto vem de lá, nunca calculado aqui.
	cupomCodigo := carrinho.CupomCodigo
	cupomDesconto := carrinho.CupomDescCents
	if codigo := strings.TrimSpace(req.CupomCodigo); codigo != "" {
		if s.coupons == nil {
			return nil, httpx.DomainError(503, httpx.CodeInternal,
				"serviço de cupons não configurado")
		}
		token, err := s.repo.TokenDoCarrinho(ctx, req.CartID)
		if err != nil {
			return nil, err
		}
		desconto, err := s.coupons.AplicarCupomNoCarrinho(ctx, token, codigo)
		if err != nil {
			return nil, err
		}
		cupomCodigo, cupomDesconto = codigo, desconto
	}

	base := carrinho.SubtotalCents - cupomDesconto
	if base < 0 {
		base = 0
	}

	// PIX: o percentual do evento, salvo se a simulação pedir outro.
	pixPercent := 0
	if req.Metodo == "pix" {
		pixPercent = int(carrinho.PixPercentEvent)
		if req.PixDescontoPercent >= 0 {
			pixPercent = req.PixDescontoPercent
		}
	}
	pixDesconto := base * int64(pixPercent) / 100

	cobrado := base - pixDesconto + carrinho.ShippingCents
	if cobrado < 0 {
		cobrado = 0
	}

	parcelas := req.Parcelas
	if req.Metodo == "pix" || parcelas < 1 {
		parcelas = 1
	}

	// O provider precisa ser um que a loja tenha, porque AplicarStatusDePagamento
	// o carrega no fato emitido. Qual deles é indiferente para o simulado.
	provider := s.providerDePagamentoDaLoja(ctx, storeID)

	return &CobrancaSimulada{
		CartID: req.CartID, PaymentID: "sim-" + uuid.NewString(),
		Provider: provider, Metodo: req.Metodo, Parcelas: parcelas,
		SubtotalCents: carrinho.SubtotalCents,
		CupomCodigo:   cupomCodigo, CupomDescCents: cupomDesconto,
		PixPercent: pixPercent, PixDescCents: pixDesconto,
		ShippingCents: carrinho.ShippingCents,
		CobradoCents:  cobrado,
	}, nil
}

// providerDePagamentoDaLoja devolve o gateway conectado, ou "mercado_pago" como
// nome de fachada quando não há nenhum — o simulado não precisa de credencial,
// e recusar por falta de gateway impediria justamente o teste que ele existe
// para permitir.
func (s *Service) providerDePagamentoDaLoja(ctx context.Context, storeID string) string {
	for _, p := range []string{"mercado_pago", "pagarme"} {
		if i, err := s.repo.GetActiveByProvider(ctx, storeID, "payment", p); err == nil && i != nil {
			return p
		}
	}
	return "mercado_pago"
}

// =============================================================================
// REPOSITÓRIO
// =============================================================================

// ListarCarrinhosPagaveis lê a lista já com a conta montada pelo banco.
func (r *Repository) ListarCarrinhosPagaveis(ctx context.Context, storeID string) ([]CarrinhoPagavel, error) {
	loja, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}
	linhas, err := r.queries.ListCartsParaSimuladorDePagamento(ctx, loja)
	if err != nil {
		return nil, err
	}
	out := make([]CarrinhoPagavel, 0, len(linhas))
	for _, l := range linhas {
		out = append(out, CarrinhoPagavel{
			CartID:          uuidToString(l.ID),
			ShortID:         l.ShortID,
			Handle:          l.PlatformHandle,
			EventID:         uuidToString(l.EventID),
			EventTitle:      l.EventTitle.String,
			Itens:           l.Itens,
			SubtotalCents:   l.SubtotalCents,
			ShippingCents:   l.ShippingCostCents,
			CupomCodigo:     l.CouponCode,
			CupomDescCents:  l.CouponDiscountCents,
			PixPercentEvent: l.PixDiscountPercent,
			CriadoEm:        l.CreatedAt.Time,
		})
	}
	return out, nil
}

// CarrinhoPagavelPorID acha UM carrinho da lista. Reusa a mesma query de
// propósito: dois caminhos para a mesma conta divergiriam.
func (r *Repository) CarrinhoPagavelPorID(ctx context.Context, storeID, cartID string) (*CarrinhoPagavel, error) {
	todos, err := r.ListarCarrinhosPagaveis(ctx, storeID)
	if err != nil {
		return nil, err
	}
	for i := range todos {
		if todos[i].CartID == cartID {
			return &todos[i], nil
		}
	}
	return nil, httpx.DomainError(422, httpx.CodeValidationFailed,
		"carrinho não está aberto para pagamento (pode já estar pago, expirado ou vazio)")
}

// TokenDoCarrinho devolve o token público, que é a chave por onde o serviço de
// cupons trabalha — a mesma que a compradora usa no checkout.
func (r *Repository) TokenDoCarrinho(ctx context.Context, cartID string) (string, error) {
	id, err := parseUUID(cartID)
	if err != nil {
		return "", err
	}
	var token string
	if err := r.pool.QueryRow(ctx, `SELECT token FROM carts WHERE id=$1`, id).Scan(&token); err != nil {
		return "", fmt.Errorf("lendo o token do carrinho: %w", err)
	}
	return token, nil
}

// =============================================================================
// ADAPTADOR DO SERVIÇO DE CUPONS
// =============================================================================

// aplicadorDeCupom liga o coupon.Service ao simulador sem que o pacote de
// integração passe a depender do de cupons — a interface é daqui, a
// implementação é injetada no boot.
type aplicadorDeCupom struct {
	aplicar func(ctx context.Context, cartToken, code string) (int64, error)
	listar  func(ctx context.Context, eventID, storeID string) ([]CupomDoEvento, error)
}

func (a aplicadorDeCupom) AplicarCupomNoCarrinho(ctx context.Context, cartToken, codigo string) (int64, error) {
	return a.aplicar(ctx, cartToken, codigo)
}

func (a aplicadorDeCupom) ListarCuponsDoEvento(ctx context.Context, eventID, storeID string) ([]CupomDoEvento, error) {
	return a.listar(ctx, eventID, storeID)
}

// NovoAplicadorDeCupom monta o adaptador a partir das duas funções do
// coupon.Service. Fica aqui, e não no main, para o main não precisar conhecer
// a forma de CupomDoEvento.
func NovoAplicadorDeCupom(
	aplicar func(ctx context.Context, cartToken, code string) (int64, error),
	listar func(ctx context.Context, eventID, storeID string) ([]CupomDoEvento, error),
) CouponApplier {
	return aplicadorDeCupom{aplicar: aplicar, listar: listar}
}
