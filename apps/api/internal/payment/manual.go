package payment

// Pagamento recebido FORA do LiveCart, confirmado à mão pelo lojista.
//
// A cliente pagou por Pix na maquininha, em dinheiro no balcão, por
// transferência — o dinheiro entrou, mas não pelo checkout. O pedido fica
// parado em "aguardando pagamento" e nada acontece: não vira Order, não chega
// ao ERP, o estoque não é lançado.
//
// O ciclo aqui é o MESMO do pagamento normal, de propósito. A confirmação
// manual não reimplementa nada: aplica a mesma escrita guardada
// (UpdateCartPaymentStatus, serializada contra a expiração) e emite o MESMO
// fato `cart.paid`. Tudo o que acontece depois — materializar a Order, criar o
// pedido no ERP, lançar estoque, resgatar cupom, e-mail, métrica de billing —
// são reatores daquele fato e rodam sem saber que o dinheiro veio por fora.
// Qualquer atalho que pulasse o evento perderia um desses passos em silêncio.
//
// Duas diferenças, e só duas:
//
//   1. Não há consulta ao gateway: não existe cobrança para consultar. Os fatos
//      do pagamento vêm do lojista, e o `payment_id` é sintético.
//   2. Não vai lançamento financeiro para o ERP. `createFinalERPOrder` só monta
//      `order.Payment` quando recebe um snapshot; mandando nil, o pedido chega
//      ao Tiny sem contas a receber — que é onde o lojista quer registrar isso,
//      com os dados que só ele tem.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// manualPaymentMethod é o `payment_method` gravado no carrinho e carregado no
// fato. Marca a origem do dinheiro no histórico do pedido — sem ele, um
// pagamento por fora ficaria indistinguível de um do gateway na hora de
// conferir o caixa.
const manualPaymentMethod = "manual"

// manualPaymentID monta o identificador sintético do pagamento.
//
// Determinístico por carrinho, e é isso que torna a confirmação idempotente: o
// `dedup_key` do fato é derivado dele, então dois cliques (ou uma retentativa
// do consumidor) colapsam no MESMO evento em vez de rodarem o fan-out duas
// vezes — o que criaria um segundo pedido no ERP.
func manualPaymentID(cartID string) string {
	return "manual:" + cartID
}

// ConfirmManualPayment marca o carrinho como pago e dispara o ciclo normal.
//
// A posse da loja já foi resolvida por quem chama (o domínio order, pela
// consulta escopada por store_id); o storeID viaja aqui para o fato emitido.
func (s *Service) ConfirmManualPayment(ctx context.Context, cartID, storeID string) error {
	if s.gateway == nil {
		return fmt.Errorf("payment: cart payment gateway not wired")
	}
	log := logger.From(ctx, s.logger)

	// Já pago é recusa, não no-op silencioso.
	//
	// O guard da query barra expirado e cancelado, mas NÃO barra carrinho já
	// pago: ele reescreveria paid_at (perdendo a hora real do pagamento) e
	// emitiria um cart.paid com dedup_key diferente do que o gateway usou —
	// dedup por payment_id, e o sintético não bate com o do provedor. O fan-out
	// rodaria de novo. Hoje o ON CONFLICT da materialização da Order absorveria
	// a duplicata, mas depender disso é confiar numa rede de segurança para uma
	// ação que mexe em dinheiro e em estoque no ERP.
	atual, err := s.gateway.CartPaymentStatus(ctx, cartID)
	if err != nil {
		return fmt.Errorf("reading cart payment status: %w", err)
	}
	switch atual {
	case "":
		return httpx.ErrNotFound("pedido não encontrado")
	case "paid":
		return httpx.DomainError(409, httpx.CodeOrderAlreadyPaid,
			"este pedido já está pago")
	case "refunded":
		return httpx.DomainError(409, httpx.CodeOrderRefunded, "este pedido foi estornado")
	}

	// Sem itens não há pedido a mandar. `createFinalERPOrder` já pula a criação
	// quando não sobra item vinculado ao ERP, mas ali é tarde: o carrinho já
	// teria sido marcado como pago, e sobraria uma venda sem nada para separar.
	gmvCents, err := s.gateway.CartGMVCents(ctx, cartID)
	if err != nil {
		return fmt.Errorf("reading cart gmv: %w", err)
	}
	if gmvCents <= 0 {
		return httpx.ErrUnprocessable("pedido sem itens para enviar ao ERP")
	}

	agora := time.Now()
	paymentID := manualPaymentID(cartID)

	liveEventID, err := s.gateway.UpdateCartPaymentStatus(
		ctx, cartID, "paid", paymentID, &agora, manualPaymentMethod)
	if err != nil {
		if !errors.Is(err, ErrCartNotPayable) {
			return fmt.Errorf("updating cart payment status: %w", err)
		}
		// Mesma inversão do webhook: o carrinho foi cancelado pela loja e o
		// dinheiro entrou assim mesmo. O dinheiro manda — restaura e segue o
		// fluxo normal, com o estoque retomado na mesma transação.
		restaurado, restoredEventID, restoreErr := s.gateway.RestoreCancelledCartAsPaid(
			ctx, cartID, storeID, "paid", paymentID, &agora, manualPaymentMethod)
		if restoreErr != nil {
			return fmt.Errorf("restoring cancelled cart as paid: %w", restoreErr)
		}
		if !restaurado {
			// Expirado, ou cancelado de um jeito que a restauração não cobre. Não
			// marcamos pago às escondidas: o prazo precisa voltar antes, e quem
			// decide isso é o lojista (regerar o link reabre o carrinho).
			return httpx.DomainError(409, httpx.CodeCartExpired,
				"este pedido expirou — regere o link do checkout antes de confirmar o pagamento")
		}
		liveEventID = restoredEventID
		log.Warn("manual payment on a store-cancelled cart — cancellation reverted",
			zap.String("cart_id", cartID))
	}

	log.Info("manual payment confirmed",
		zap.String("cart_id", cartID),
		zap.String("store_id", storeID),
		zap.Int64("gmv_cents", gmvCents),
	)

	// O MESMO fato do pagamento normal. PaymentSnapshot nil de propósito: é ele
	// que vira contas a receber no Tiny, e o lojista lança isso lá com os dados
	// da forma como recebeu.
	payload, _ := json.Marshal(struct {
		CartID    string `json:"cart_id"`
		StoreID   string `json:"store_id"`
		PaymentID string `json:"payment_id"`
		Method    string `json:"payment_method"`
		GMVCents  int64  `json:"gmv_cents,omitempty"`
	}{cartID, storeID, paymentID, manualPaymentMethod, gmvCents})

	return s.gateway.EmitEvent(ctx, events.Envelope{
		Name:        events.CartPaid,
		Source:      events.SourceInternal,
		DedupKey:    string(events.CartPaid) + ":" + paymentID,
		LiveEventID: liveEventID,
		Payload:     payload,
	})
}
