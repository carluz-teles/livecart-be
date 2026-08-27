package erp

// Rastreamento da situação do pedido no ERP.
//
// O pedido de venda nasce no primeiro comentário da live e vive meses depois
// dela: é aprovado no pagamento, faturado, separado, despachado, entregue. Esse
// trajeto todo acontece no ERP, e o LiveCart só sabia dele até a aprovação — do
// faturamento em diante o lojista tinha de abrir o outro sistema.
//
// Duas fontes, uma escrita. O webhook de vendas avisa em cada transição e é a
// fonte primária; a varredura pergunta de volta para quem parou de se mexer, e é
// o conserto de webhook perdido. Ambas passam por RecordOrderStatus, que só grava
// quando a situação MUDOU — o ERP reentrega o mesmo aviso até dez vezes quando
// não recebe 200, e redelivery não é transição.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
)

// Fonte de uma observação de situação. A diferença importa no diagnóstico: uma
// sequência de 'sweep' é a prova de que o webhook parou de chegar.
const (
	StatusSourceWebhook = "webhook"
	StatusSourceSweep   = "sweep"
	// StatusSourceCreate é a situação que não precisou ser observada: o pedido
	// acabou de ser criado por nós, em situação Aberta.
	//
	// Existe por uma corrida medida: o webhook `inclusao_pedido` do ERP chega
	// ~90ms DEPOIS do POST responder e ~90ms ANTES de gravarmos o
	// external_order_id no carrinho. Nessa janela a primeira situação do pedido
	// não encontra dono e é arquivada como avulsa — e o carrinho fica sem
	// situação nenhuma, invisível para a varredura, para sempre.
	StatusSourceCreate = "create"
)

// ERPOrderStatusObservation é uma situação observada num pedido do ERP.
type ERPOrderStatusObservation struct {
	StoreID         string
	CartID          string // vazio quando o pedido não é de nenhum carrinho nosso
	ExternalOrderID string
	OrderNumber     string
	Status          providers.ERPOrderStatus
	Source          string
	Payload         json.RawMessage
}

// ERPOrderStatusTransition é o que uma observação produziu quando de fato mudou
// alguma coisa.
type ERPOrderStatusTransition struct {
	CartID         string
	PreviousStatus string
	Status         string
	ObservedAt     time.Time
}

// StaleERPOrderStatus é um pedido parado num estágio não terminal — candidato a
// ter perdido um webhook.
type StaleERPOrderStatus struct {
	CartID          string
	StoreID         string
	ExternalOrderID string
	Status          string
	StatusAt        time.Time
}

// ERPOrderStatusRepository é a persistência do rastreamento. Separada de
// ERPRepository porque é uma fatia com dono próprio, e porque o webhook de
// pedido pode chegar sem carrinho nenhum do lado de cá.
type ERPOrderStatusRepository interface {
	// RecordOrderStatus grava a situação de um pedido e devolve a transição.
	//
	// A resolução do carrinho é DELA, a partir do id do pedido — não do chamador.
	// Resolver em Go significaria ler antes e gravar depois, e o webhook de
	// inclusão do ERP chega exatamente nessa janela. Transition.CartID vazio
	// significa "não é de nenhum carrinho nosso"; changed=false significa que a
	// situação já era essa (reentrega, ou varredura confirmando o que sabíamos).
	RecordOrderStatus(ctx context.Context, obs ERPOrderStatusObservation) (t ERPOrderStatusTransition, changed bool, err error)
	// ListStaleOrderStatuses lista pedidos não terminais parados há mais que a
	// janela, do mais antigo para o mais novo.
	ListStaleOrderStatuses(ctx context.Context, staleAfter time.Duration, limit int) ([]StaleERPOrderStatus, error)
	// AdoptOrphanOrderStatusEvents vincula ao carrinho as passagens que chegaram
	// antes de o pedido existir do lado de cá — a janela entre o POST responder e
	// o external_order_id ser gravado, onde nem o SQL consegue resolver o dono.
	AdoptOrphanOrderStatusEvents(ctx context.Context, cartID, externalOrderID string) (int64, error)
}

// CartReopener ressuscita o carrinho quando o lojista reabre o pedido no ERP.
//
// Interface aqui, e não import do integration, pelo mesmo motivo dos outros
// colaboradores deste pacote: manter o erp sem ciclo com quem o usa.
type CartReopener interface {
	ReopenCartFromERP(ctx context.Context, cartID, storeID string) (ReopenReport, error)
}

// ReopenReport é o que a ressurreição recuperou.
type ReopenReport struct {
	Reopened    bool
	Recuperadas int
	EmFila      int
}

// SetCartReopener liga o seguimento do pedido reaberto. Opcional: sem ele a
// volta é apenas registrada, que era o comportamento anterior.
func (s *Service) SetCartReopener(r CartReopener) { s.reopener = r }

// SetOrderStatusRepository liga a persistência do rastreamento. Opcional: sem
// ela o rastreamento simplesmente não acontece, e o resto do fluxo segue igual.
func (s *Service) SetOrderStatusRepository(r ERPOrderStatusRepository) { s.status = r }

// ObserveOrderStatus é a entrada única das duas fontes.
//
// Resolve o carrinho pelo id do pedido, grava se mudou, e loga a transição. Não
// devolve erro para "não mudou" nem para "não é nosso": as duas são respostas
// normais, e transformá-las em erro faria o webhook devolver não-200 — o que
// leva o ERP a reentregar e, depois de vinte falhas, a apagar a URL.
func (s *Service) ObserveOrderStatus(ctx context.Context, storeID, externalOrderID, orderNumber string, status providers.ERPOrderStatus, source string, payload json.RawMessage) error {
	if s.status == nil {
		return nil
	}
	if externalOrderID == "" {
		return fmt.Errorf("observação de situação sem id de pedido")
	}

	t, changed, err := s.status.RecordOrderStatus(ctx, ERPOrderStatusObservation{
		StoreID:         storeID,
		ExternalOrderID: externalOrderID,
		OrderNumber:     orderNumber,
		Status:          status,
		Source:          source,
		Payload:         payload,
	})
	if err != nil {
		return fmt.Errorf("recording ERP order status: %w", err)
	}
	if !changed {
		return nil
	}

	if t.CartID == "" {
		// Pedido que não é de nenhum carrinho nosso — o lojista criou direto no
		// ERP, ou veio de outro canal. A passagem fica guardada: é o sinal vivo
		// de que a entrega de webhook está funcionando, e o silêncio total desta
		// tabela é o sintoma de URL descadastrada.
		logger.From(ctx, s.logger).Debug("ERP order status for an order LiveCart does not own",
			zap.String("external_order_id", externalOrderID),
			zap.String("status", string(status)),
		)
		return nil
	}

	// O pedido voltou a viver e o carrinho não. Sobe como ERRO, e não como
	// info: dali em diante existe uma unidade reservada no ERP que nenhum
	// carrinho reclama, e ela some do disponível até alguém reparar no Tiny.
	//
	// O LiveCart não desfaz o cancelamento sozinho de propósito. Cancelar aqui
	// devolveu estoque local, desativou o link e avisou a compradora; reabrir o
	// pedido no ERP desfaz UMA dessas coisas, e ressuscitar o carrinho por
	// conta própria tentaria refazer as outras — inclusive re-reservar uma peça
	// que pode já ter sido vendida no meio tempo. Quem decide é gente; o que o
	// sistema deve é não deixar isso invisível.
	if providers.ERPOrderStatus(t.Status).VoltouAViver() && s.reopener != nil {
		if morto, err := s.repo.CartIsTerminated(ctx, t.CartID); err == nil && morto {
			rel, rErr := s.reopener.ReopenCartFromERP(ctx, t.CartID, storeID)
			switch {
			case rErr != nil:
				// Falhar aqui é grave e não pode passar em silêncio: o pedido
				// segue vivo no ERP reservando peça, e o carrinho continua
				// morto. A aba "Precisam atenção" pega o caso pelo estado, mas
				// o motivo só existe aqui.
				logger.From(ctx, s.logger).Error("the ERP order came back to life and the cart could not follow — a unit stays reserved with no cart behind it",
					zap.String("cart_id", t.CartID),
					zap.String("external_order_id", externalOrderID),
					zap.Error(rErr),
				)
			case rel.Reopened:
				logger.From(ctx, s.logger).Info("cart reopened following the ERP order",
					zap.String("cart_id", t.CartID),
					zap.String("external_order_id", externalOrderID),
					zap.Int("units_recovered", rel.Recuperadas),
					zap.Int("units_waitlisted", rel.EmFila),
				)
			default:
				// Não elegível: vencido, pago, ou cancelado por outro motivo
				// que não a mão do lojista. Fica para a triagem humana.
				logger.From(ctx, s.logger).Warn("ERP order came back to life but the cart cannot be reopened — it is reserving stock with no cart behind it",
					zap.String("cart_id", t.CartID),
					zap.String("external_order_id", externalOrderID),
				)
			}
		}
	}

	logger.From(ctx, s.logger).Info("ERP order status advanced",
		zap.String("cart_id", t.CartID),
		zap.String("external_order_id", externalOrderID),
		zap.String("order_number", orderNumber),
		zap.String("from", t.PreviousStatus),
		zap.String("to", t.Status),
		zap.String("source", source),
	)

	// O espelho projeta a situação nova na Order, que é o que a tela de pedidos
	// lê. Best-effort de propósito: o rastreamento já está gravado, e falhar aqui
	// não pode desfazer isso nem devolver não-200 ao ERP.
	s.collab.MirrorToOrder(ctx, t.CartID)
	return nil
}

// SeedOrderStatusOnCreate registra a situação inicial do pedido que acabamos de
// criar. Fecha a corrida descrita em StatusSourceCreate: mesmo que o webhook de
// inclusão tenha chegado antes de o carrinho conhecer o pedido, o trajeto começa
// aqui — e um webhook posterior com "aberto" no-opa, porque a situação já é essa.
func (s *Service) SeedOrderStatusOnCreate(ctx context.Context, storeID, cartID, externalOrderID string) {
	if s.status == nil || externalOrderID == "" {
		return
	}
	// Primeiro adota o que já chegou. A ordem importa: a passagem órfã é a
	// 'aberto' que o ERP anunciou, e adotá-la ANTES de semear faz a semente
	// no-opar (a situação já é essa) em vez de criar uma segunda linha idêntica.
	if n, err := s.status.AdoptOrphanOrderStatusEvents(ctx, cartID, externalOrderID); err != nil {
		logger.From(ctx, s.logger).Warn("could not adopt orphan status events",
			zap.String("cart_id", cartID), zap.Error(err))
	} else if n > 0 {
		logger.From(ctx, s.logger).Debug("adopted status events that arrived before the order was linked",
			zap.String("cart_id", cartID), zap.Int64("events", n))
	}

	if _, _, err := s.status.RecordOrderStatus(ctx, ERPOrderStatusObservation{
		StoreID:         storeID,
		CartID:          cartID,
		ExternalOrderID: externalOrderID,
		Status:          providers.ERPOrderStatusAberto,
		Source:          StatusSourceCreate,
	}); err != nil {
		logger.From(ctx, s.logger).Warn("could not seed the order's initial status",
			zap.String("cart_id", cartID),
			zap.String("external_order_id", externalOrderID),
			zap.Error(err))
	}
}

// RunERPOrderStatusSweep pergunta ao ERP a situação dos pedidos que pararam de
// se mexer. É o conserto de webhook perdido — o ERP desiste depois de dez
// tentativas, e apaga a URL depois de falhas seguidas; nos dois casos o silêncio
// é indistinguível de "nada aconteceu".
//
// Uma consulta por pedido, com o mesmo limitador das escritas: a varredura não
// pode consumir a cota que a live precisa.
func (s *Service) RunERPOrderStatusSweep(ctx context.Context, staleAfter time.Duration, limit int) {
	if s.status == nil {
		return
	}
	stale, err := s.status.ListStaleOrderStatuses(ctx, staleAfter, limit)
	if err != nil {
		logger.From(ctx, s.logger).Error("ERP order status sweep failed to list", zap.Error(err))
		return
	}
	for _, p := range stale {
		itemCtx := logger.WithStore(ctx, p.StoreID, "")
		erpProvider, provErr := s.providerFor(itemCtx, p.StoreID)
		if provErr != nil {
			continue
		}
		// A leitura passa pela mesma cota das escritas: o balde do ERP conta os
		// dois, e uma varredura de duzentos pedidos consumiria sozinha a cota de
		// uma live inteira. Chave por carrinho, para não disputar com as escritas
		// de outro pedido.
		var situacao int
		readErr := s.escreverNoERP(itemCtx, p.StoreID, p.CartID, func(ctx context.Context) error {
			var err error
			situacao, err = erpProvider.GetOrderSituacao(ctx, p.ExternalOrderID)
			return err
		})
		if readErr != nil {
			logger.From(itemCtx, s.logger).Warn("sweep could not read ERP order situation",
				zap.String("cart_id", p.CartID),
				zap.String("external_order_id", p.ExternalOrderID),
				zap.Error(readErr))
			continue
		}
		status, known := providers.ERPOrderStatusFromSituacao(situacao)
		if !known {
			// Situação nova numa versão futura da API. Registrar um nome
			// inventado seria pior do que dizer que não conhecemos aquela.
			logger.From(itemCtx, s.logger).Warn("ERP returned an unknown order situation",
				zap.String("external_order_id", p.ExternalOrderID),
				zap.Int("situacao", situacao))
			continue
		}
		if err := s.ObserveOrderStatus(itemCtx, p.StoreID, p.ExternalOrderID, "", status, StatusSourceSweep, nil); err != nil {
			logger.From(itemCtx, s.logger).Warn("sweep failed to record ERP order status",
				zap.String("cart_id", p.CartID),
				zap.Error(err))
		}
	}
}

// =============================================================================
// REENVIO MANUAL
// =============================================================================

// RetryERPFinalisation reexecuta o caminho pago de um pedido que falhou — o
// botão de reenviar do painel.
//
// Reexecutar hoje é reexecutar o confirm, e mais nada. O que ele fazia antes era
// diferente e muito maior: criar o pedido do zero, lançar estoque, estornar as
// reservas manuais, e decidir a ordem entre essas três coisas conforme a loja.
// Com o pedido criado desde o primeiro comentário, sobrou gravar as parcelas e
// aprovar — as duas idempotentes.
//
// O snapshot do gateway é relido do banco: é o mesmo que o webhook original
// congelou, e sem ele o reenvio aprovaria a venda sem o financeiro junto.
func (s *Service) RetryERPFinalisation(ctx context.Context, cartID, storeID string) error {
	st, err := s.repo.GetCartERPFinalisationStatus(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart finalisation status: %w", err)
	}

	var status *providers.PaymentStatus
	if len(st.PaymentSnapshot) > 0 {
		var snap providers.PaymentStatus
		if err := json.Unmarshal(st.PaymentSnapshot, &snap); err == nil {
			status = &snap
		} else {
			logger.From(ctx, s.logger).Warn("retry: bad payment snapshot, confirming without payment details",
				zap.String("cart_id", cartID), zap.Error(err))
		}
	}

	// O carimbo da tentativa acontece dentro do confirm, junto com o retrato do
	// gateway — um ponto só, para o painel não depender de quem chamou.
	return s.ConfirmERPOrderPayment(ctx, cartID, storeID, status)
}
