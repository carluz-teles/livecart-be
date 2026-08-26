package erp

// O modo RESERVA: o pedido de venda é a reserva.
//
// A descoberta que dispensa o movimento manual: criar um pedido de venda no Tiny
// já segura a peça, sem tocar no saldo físico. Medido em 26/08/2026 na conta com
// o módulo de reserva ativo:
//
//	criar pedido de 2 un.   →  saldo 42 (inalterado) · reservado 0→2 · disponivel 42→40
//	PUT /itens 2→5          →  204, e reservado acompanha: 2→5
//	cancelar (situacao=2)   →  reservado volta a 0, sozinho
//	lancar-estoque          →  saldo −3 e reservado −3: converte, sem contar duas vezes
//
// O que isso mata, de uma vez: a saída manual tipo `S` que baixa o físico, o
// webhook de estoque que ela dispara e realimenta a fila, o par estorno→criação
// no pagamento, e a classe inteira de reservas órfãs.
//
// 🔴 A regra que a medição impõe, e que é fácil errar: **no modo reserva,
// `estornar-estoque` num pedido que só reservou NÃO é no-op — ele RE-RESERVA.**
// Três chamadas seguidas num pedido de 3 unidades levaram o reservado de 12 para
// 21, +3 a cada uma, sem teto. É o oposto do modo lançamento, onde estornar sobre
// pedido não lançado é 204 inócuo. Por isso a mutação aqui é `PUT /itens` e nada
// mais — o ciclo estornar→PUT→lançar pertence ao outro modo.

import (
	"context"

	"go.uber.org/zap"

	"livecart/apps/api/lib/logger"
)

// MetadataOrderReservation é a chave, no metadata da integração, que liga o modo
// reserva. Vive por INTEGRAÇÃO e não por loja porque a diferença entre reservar
// pelo pedido e baixar por movimento é um conceito do Tiny, e um lojista pode ter
// mais de um ERP ligado.
//
// Exige que a conta tenha o módulo de Reserva de Estoque ativo. Sem ele o pedido
// não reserva nada e a live venderia às cegas — por isso o padrão é desligado e a
// migração é explícita, por integração.
const MetadataOrderReservation = "order_reservation"

// reserveModeEnabled diz se esta loja opera no modo reserva.
//
// Falha fechado de propósito: sem integração, sem metadata ou com valor
// inesperado, responde false e o caminho legado continua. Ligar por engano
// significaria parar de segurar estoque no meio de uma live.
func (s *Service) reserveModeEnabled(ctx context.Context, storeID string) bool {
	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil || integration == nil {
		return false
	}
	return metadataFlag(integration.Metadata, MetadataOrderReservation)
}

// metadataFlag lê um booleano do metadata tolerando as formas que o JSON produz.
func metadataFlag(metadata map[string]any, key string) bool {
	if metadata == nil {
		return false
	}
	switch v := metadata[key].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	default:
		return false
	}
}

// logReserveMode registra a decisão uma vez por operação, para o log dizer em
// qual dos dois mundos aquele carrinho está andando.
func (s *Service) logReserveMode(ctx context.Context, cartID string, on bool) {
	logger.From(ctx, s.logger).Debug("erp stock mode resolved",
		zap.String("cart_id", cartID),
		zap.Bool("reserve_mode", on),
	)
}
