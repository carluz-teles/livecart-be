package integration

// Juntar pedidos NO ERP, mantendo-os separados no LiveCart.
//
// O caso é sempre o mesmo: a compradora já tinha um pedido em aberto — comprou
// fora da live, pelo WhatsApp, na loja — e aí comenta na transmissão. O LiveCart
// cria o pedido dele, e ficam dois pedidos da mesma pessoa no ERP, cada um
// segurando peça, cada um querendo o seu frete e a sua nota.
//
// A junção acontece só de um lado. No ERP vira UM pedido com tudo dentro; no
// LiveCart os dois carrinhos continuam existindo, cada um com o seu histórico, o
// seu link e o seu pagamento — foram duas compras, e o lojista precisa poder
// olhar cada uma.
//
// ═══ QUEM É O ANFITRIÃO ═══
//
// O anfitrião é quem fica com o pedido no ERP, e a escolha não é de gosto:
//
//	um pago e um não pago  →  o PAGO, sempre
//	os dois pagos          →  o mais antigo
//	nenhum pago            →  o mais antigo
//
// O pago manda porque o pedido dele já está aprovado no ERP e tem cobrança
// atrelada; cancelá-lo para reabrir a venda no outro seria desfazer um
// pagamento aceito. O mais antigo desempata porque é o pedido que a compradora
// já conhece — é o número que ela viu primeiro, e é o que o lojista já pode ter
// citado numa conversa.
//
// ═══ A ORDEM ═══
//
// Igual à fusão do VIP e pelo mesmo motivo: o pedido do anfitrião CRESCE antes
// de o outro ser cancelado. Invertido existe um instante em que as peças não
// estão em pedido nenhum, e numa loja com estoque disputado isso é tempo de
// sobra para outra pessoa levá-las.

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// JoinCartsInput é o pedido de junção, como o painel o manda.
type JoinCartsInput struct {
	StoreID string
	// Os dois pedidos. Qual vira anfitrião é decidido pelas regras acima, não
	// pela ordem em que chegam — mandar o lojista adivinhar a direção certa
	// seria transferir para ele uma regra que o sistema já conhece.
	CartAID string
	CartBID string
	// ConfirmarCompradorDiferente libera juntar pedidos de compradores
	// distintos. Fechado por padrão: só o lojista sabe que dois @ são a mesma
	// pessoa, e juntar a compra de duas pessoas manda uma delas para o frete da
	// outra.
	ConfirmarCompradorDiferente bool
}

// JoinCartsResult conta o que a junção fez.
type JoinCartsResult struct {
	HostCartID   string `json:"hostCartId"`
	JoinedCartID string `json:"joinedCartId"`
	// ExternalOrderID é o pedido que sobrou no ERP, com tudo dentro.
	ExternalOrderID string `json:"externalOrderId,omitempty"`
	// OrderReleased é o pedido que foi cancelado por ter perdido o conteúdo.
	OrderReleased string `json:"orderReleased,omitempty"`
	// OutstandingCents é o que falta pagar no pedido resultante, somando as
	// cobranças dos dois carrinhos.
	OutstandingCents int64 `json:"outstandingCents"`
}

// JoinCarts junta dois pedidos num só no ERP.
func (s *Service) JoinCarts(ctx context.Context, in JoinCartsInput) (JoinCartsResult, error) {
	var out JoinCartsResult
	ctx = logger.WithStore(ctx, in.StoreID, "")

	if in.CartAID == in.CartBID {
		return out, httpx.DomainError(422, httpx.CodeValidationFailed, "os dois pedidos são o mesmo")
	}
	a, err := s.repo.GetCartForJoin(ctx, in.CartAID, in.StoreID)
	if err != nil {
		return out, httpx.DomainError(404, httpx.CodeValidationFailed, "pedido não encontrado")
	}
	b, err := s.repo.GetCartForJoin(ctx, in.CartBID, in.StoreID)
	if err != nil {
		return out, httpx.DomainError(404, httpx.CodeValidationFailed, "pedido não encontrado")
	}
	if err := podemSerJuntados(a, b, in.ConfirmarCompradorDiferente); err != nil {
		return out, err
	}

	host, juntado := escolherAnfitriao(a, b)
	out.HostCartID, out.JoinedCartID = host.CartID, juntado.CartID
	out.ExternalOrderID = host.ExternalOrderID

	// 1. O vínculo. A partir daqui a grade do anfitrião já inclui os itens do
	//    outro — a query do grid soma o grupo — e o carrinho juntado deixa de
	//    ter pedido próprio.
	pedidoASoltar := juntado.ExternalOrderID
	feito, err := s.repo.JoinCartIntoHost(ctx, juntado.CartID, host.CartID)
	if err != nil {
		return out, fmt.Errorf("vinculando os pedidos: %w", err)
	}
	if !feito {
		return out, httpx.DomainError(409, httpx.CodeJoinAlreadyLinked,
			"um dos pedidos já faz parte de outra junção")
	}

	// 2. Os pedidos do ERP seguem: o do anfitrião cresce com a grade somada, e
	//    só então o outro é cancelado.
	if pedidoASoltar != "" {
		rel, mErr := s.MergeERPOrdersIntoCart(ctx, host.CartID, in.StoreID,
			[]erp.ERPOrderMerge{{SourceCartID: juntado.CartID, ExternalOrderID: pedidoASoltar}})
		if rel != nil && len(rel.Released) > 0 {
			out.OrderReleased = rel.Released[0]
		}
		if mErr != nil {
			// O vínculo está feito e a grade do anfitrião já cresceu; o que
			// falhou foi soltar o pedido velho. Ele continua reservando peça, e
			// isso não pode passar em silêncio.
			logger.From(ctx, s.logger).Error("orders joined but the old ERP order was not released — it is still holding stock",
				zap.String("host_cart_id", host.CartID),
				zap.String("joined_cart_id", juntado.CartID),
				zap.String("external_order_id", pedidoASoltar),
				zap.Error(mErr))
			return out, fmt.Errorf("os pedidos foram juntados, mas o pedido %s continua reservando peça no ERP: %w",
				pedidoASoltar, mErr)
		}
	} else {
		// O carrinho juntado não tinha pedido (ninguém comentou nele ainda).
		// Mesmo assim a grade do anfitrião cresceu, e precisa subir.
		if mErr := s.MutateERPOrderItems(ctx, host.CartID, in.StoreID); mErr != nil {
			return out, fmt.Errorf("levando a grade somada ao pedido: %w", mErr)
		}
	}

	// 3. O dinheiro volta a dizer a verdade, agora somando as cobranças dos
	//    dois carrinhos — o extrato do ERP lê o grupo.
	if split, sErr := s.RecomporParcelasDoPedidoPago(ctx, host.CartID, in.StoreID); sErr != nil {
		logger.From(ctx, s.logger).Error("orders joined but the paid/outstanding split could not be restored",
			zap.String("host_cart_id", host.CartID), zap.Error(sErr))
	} else if split != nil {
		out.OutstandingCents = split.SaldoCents
	}

	logger.From(ctx, s.logger).Info("orders joined into a single ERP order",
		zap.String("host_cart_id", host.CartID),
		zap.String("joined_cart_id", juntado.CartID),
		zap.String("external_order_id", out.ExternalOrderID),
		zap.String("order_released", out.OrderReleased))
	return out, nil
}

// podemSerJuntados é o mapa dos casos, num lugar só.
func podemSerJuntados(a, b CartForJoin, confirmouCompradorDiferente bool) error {
	for _, c := range []CartForJoin{a, b} {
		if c.Terminated {
			return httpx.DomainError(422, httpx.CodeValidationFailed,
				"um dos pedidos está cancelado ou vencido — não há venda a juntar")
		}
		if c.Refunded {
			return httpx.DomainError(422, httpx.CodeValidationFailed,
				"um dos pedidos foi estornado — o dinheiro já voltou")
		}
		// A nota é o portão, aqui como em toda edição de pedido: emitida, somar
		// item é emitir nota errada. E juntar é somar item.
		if c.Invoiced {
			return httpx.DomainError(422, httpx.CodeErpOrderInvoiced,
				"um dos pedidos já foi faturado — a nota está emitida e ele não recebe mais item")
		}
		if c.AlreadyJoined {
			return httpx.DomainError(409, httpx.CodeJoinAlreadyLinked,
				"um dos pedidos já faz parte de outra junção")
		}
	}
	if a.PlatformUserID != b.PlatformUserID && !confirmouCompradorDiferente {
		return httpx.DomainError(409, httpx.CodeJoinDifferentBuyers,
			fmt.Sprintf("os pedidos são de compradores diferentes (%s e %s) — confirme se é a mesma pessoa",
				a.PlatformHandle, b.PlatformHandle))
	}
	return nil
}

// escolherAnfitriao aplica a regra de quem fica com o pedido. Ver o topo.
func escolherAnfitriao(a, b CartForJoin) (host, juntado CartForJoin) {
	switch {
	case a.Paid && !b.Paid:
		return a, b
	case b.Paid && !a.Paid:
		return b, a
	}
	if a.CreatedAt.Before(b.CreatedAt) {
		return a, b
	}
	return b, a
}

// CartForJoin é o que a decisão de junção precisa saber de um pedido.
type CartForJoin struct {
	CartID          string
	PlatformUserID  string
	PlatformHandle  string
	ExternalOrderID string
	CreatedAt       time.Time
	Paid            bool
	Refunded        bool
	Terminated      bool
	// Invoiced vem da situação que o webhook do ERP deixou no carrinho.
	Invoiced bool
	// AlreadyJoined: já é anfitrião de alguém, ou já foi juntado a outro.
	AlreadyJoined bool
}
