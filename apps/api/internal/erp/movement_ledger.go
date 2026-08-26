package erp

// Razão de movimentos de estoque contra o ERP — a intenção antes da chamada.
//
// O problema que isto fecha tem data: 17/08/2026, 21:17. Dois POSTs de reserva
// deram timeout na mesma live. Um tinha ENTRADO no Tiny (o lançamento aparece
// no extrato, com o webhook de estoque chegando segundos depois); o outro não.
// Nenhum dos dois deixou registro do nosso lado, porque a linha em
// stock_reservations só nascia com a resposta em mãos — e a API v3 do Tiny não
// oferece consulta de lançamentos para desempatar depois. Um virou reserva
// órfã (baixa dobrada quando o carrinho pagar); o outro, unidade sem lastro.
//
// A regra que governa tudo aqui: só se re-executa com PROVA de não-entrega
// (providers.ErrProvenUndelivered — discagem ou recusa 4xx). Timeout e 5xx
// nunca são repetidos às cegas: viram `unconfirmed`, ficam visíveis, e travam a
// finalização do carrinho até alguém decidir com o extrato na mão. Dos dois
// erros possíveis, o barato é segurar um pagamento por minutos; o caro é criar
// um segundo lançamento invisível que ninguém jamais estorna.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"

	"livecart/apps/api/internal/erp/erpwrite"
)

// Estados de um movimento. A linha nunca é apagada — só anda.
const (
	MovementPending     = "pending"     // chamada em voo (ou processo morto no meio dela)
	MovementConfirmed   = "confirmed"   // o ERP devolveu o id do lançamento
	MovementFailed      = "failed"      // provado que NÃO chegou; seguro re-executar
	MovementUnconfirmed = "unconfirmed" // ambíguo; NUNCA re-executado às cegas
	MovementResolving   = "resolving"   // reivindicado por um resolver
)

// movementMaxAttempts é o teto de execuções (a inline + as do resolver) antes
// de a linha parar em `failed` e virar caso de gente.
const movementMaxAttempts = 5

// StockMovementRow é uma linha do razão.
type StockMovementRow struct {
	ID                string
	StoreID           string
	CartID            string
	EventID           string
	ProductID         string
	ExternalProductID string
	Direction         string // "out" (reserva) | "in" (estorno)
	Quantity          int
	UnitPriceCents    int64
	IdempotencyKey    string
	Status            string
	ERPMovementID     string
	Attempts          int
	LastError         string
	CreatedAt         time.Time
	// ReservationID liga o movimento 'in' à reserva que ele desfaz (000133).
	ReservationID string
}

// CreateStockMovementParams grava a intenção.
type CreateStockMovementParams struct {
	StoreID           string
	CartID            string
	EventID           string
	ProductID         string
	ExternalProductID string
	Direction         string
	Quantity          int
	UnitPriceCents    int64
	ReservationID     string
}

// StockMovementLedger é o razão persistente. Interface separada de
// ERPRepository de propósito: o ledger é opcional (nil = caminho legado,
// síncrono e sem registro), o que dá rollout gradual e mantém os dublês de
// teste existentes intactos.
type StockMovementLedger interface {
	CreateERPStockMovement(ctx context.Context, p CreateStockMovementParams) (*StockMovementRow, error)
	GetERPStockMovement(ctx context.Context, id string) (*StockMovementRow, error)
	MarkERPStockMovementConfirmed(ctx context.Context, id, erpMovementID string) error
	// MarkERPStockMovementOutcome grava failed|unconfirmed com o erro, e conta a
	// tentativa.
	MarkERPStockMovementOutcome(ctx context.Context, id, status, lastError string) error
	// ClaimERPStockMovement reivindica a linha (status -> resolving) SE ela
	// estiver no estado esperado — CAS. nil sem erro = outro resolver levou, ou
	// o estado mudou. Os guards de idade (pending >2min, resolving >5min) moram
	// na query.
	ClaimERPStockMovement(ctx context.Context, id, fromStatus string) (*StockMovementRow, error)
	ListUnresolvedERPStockMovementsByCart(ctx context.Context, cartID string) ([]StockMovementRow, error)
}

// ReversalLedger é o que o estorno claim-first precisa do razão. Struct em vez
// de parâmetros soltos porque atravessa a função livre de reversal_claim.go —
// nil inteiro = modo legado (restore-and-retry), preservado para rollout.
type ReversalLedger struct {
	Movements StockMovementLedger
	Scheduler StockMovementScheduler // pode ser nil; o gate da finalização cobre
	StoreID   string
}

// ReversalLedgerHooks monta os hooks para os chamadores do estorno. nil quando
// o razão não está ligado — a função livre entende nil como modo legado.
func (s *Service) ReversalLedgerHooks(storeID string) *ReversalLedger {
	if s.movements == nil {
		return nil
	}
	return &ReversalLedger{Movements: s.movements, Scheduler: s.movementScheduler, StoreID: storeID}
}

// StockMovementScheduler agenda a próxima tentativa de resolução.
type StockMovementScheduler interface {
	ScheduleStockMovementResolve(ctx context.Context, movementID string, at time.Time) error
}

// SetStockMovementLedger liga o razão. Sem ele, ReserveStockInERP segue no
// caminho legado (síncrono, sem registro de intenção).
func (s *Service) SetStockMovementLedger(l StockMovementLedger) { s.movements = l }

// SetStockMovementScheduler liga o agendador de retries. Opcional: sem ele, o
// retry de `failed` acontece só no gate da finalização.
func (s *Service) SetStockMovementScheduler(sch StockMovementScheduler) { s.movementScheduler = sch }

// movementRetryDelay espaça as tentativas: rede que caiu por segundos não
// custa a reserva, e rede que caiu por minutos não vira loop quente.
func movementRetryDelay(attempts int) time.Duration {
	switch {
	case attempts <= 1:
		return 30 * time.Second
	case attempts == 2:
		return 2 * time.Minute
	default:
		return 10 * time.Minute
	}
}

// cartRef é a referência do carrinho carimbada nos lançamentos do Tiny: o
// #short_id que o lojista vê no LiveCart, para copiar do extrato e achar o
// carrinho. Cai no UUID do carrinho se o número não puder ser lido, para a
// referência nunca se perder.
func (s *Service) cartRef(ctx context.Context, cartID string) string {
	if sid, err := s.repo.GetCartShortID(ctx, cartID); err == nil && sid > 0 {
		return fmt.Sprintf("#%d", sid)
	}
	return cartID
}

// movementObservacao é o texto que viaja no lançamento do Tiny: só o @ do
// comprador e o número do carrinho, para o lojista copiar e localizar o
// carrinho no LiveCart. A chave de idempotência mora na linha do razão (e no
// índice único cart+produto), não neste texto voltado ao humano.
func movementObservacao(cartRef, platformHandle string, retry int) string {
	if retry > 0 {
		return fmt.Sprintf("Cart %s (retry %d)", cartRef, retry)
	}
	return fmt.Sprintf("@%s - Cart %s", platformHandle, cartRef)
}

// executeStockMovement faz UMA execução do movimento e grava o desfecho.
//
// Roda em contexto próprio com prazo folgado, desacoplado da requisição que o
// originou — foi o prazo compartilhado que transformou a lentidão do Tiny em
// resposta perdida às 21:17. O prazo folgado converte a maioria dos antigos
// "timeouts" em confirmação atrasada, encolhendo a classe ambígua.
func (s *Service) executeStockMovement(ctx context.Context, provider providers.ERPProvider, mov *StockMovementRow, obs string) {
	dctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	// Com o pipeline ligado, a escrita passa pelo teto real da API e pela fila
	// serial do PEDIDO antes de sair. Se qualquer um dos dois recusar, nada foi
	// despachado — e o erro carrega ErrProvenUndelivered, que é o contrato que
	// finishStockMovement já entende como "seguro repetir".
	//
	// Foi essa distinção que faltou numa live simulada de 26/08: 115 das 170
	// reservas morreram esperando a fila até o prazo estourar e viraram
	// `unconfirmed`, que nunca re-tenta e trava o carrinho pago. Nenhuma delas
	// havia saído da máquina.
	if s.pipeline != nil {
		if waitErr := s.pipeline.lim.Wait(dctx); waitErr != nil {
			s.finishStockMovement(ctx, mov, "", naoDespachada(waitErr))
			return
		}
	}

	executar := func(dctx context.Context) (string, error) {
		if mov.Direction == "in" {
			return executarEntradaNoERP(dctx, provider, mov.ExternalProductID, mov.Quantity, obs)
		}
		return provider.ReserveStock(dctx, mov.ExternalProductID, mov.Quantity, float64(mov.UnitPriceCents)/100, obs)
	}

	if s.pipeline != nil {
		var movementID string
		var despachou bool
		filaErr := s.pipeline.fila.Do(dctx, chaveDeSerializacao(mov), func(dctx context.Context) error {
			despachou = true
			var inner error
			movementID, inner = executar(dctx)
			return inner
		})
		if !despachou {
			// Desistiu na espera da vez: provadamente não aplicado.
			s.finishStockMovement(ctx, mov, "", naoDespachada(filaErr))
			return
		}
		s.finishStockMovement(ctx, mov, movementID, filaErr)
		return
	}

	var movementID string
	var err error
	if mov.Direction == "in" {
		// A chamada crua vive em reversal_claim.go (a catraca de convenção
		// autoriza só lá). Aqui só se chega com a reserva JÁ reivindicada — a
		// reivindicação aconteceu antes do primeiro POST e nunca é desfeita no
		// modo razão, então a retentativa é single-flight por construção.
		movementID, err = executarEntradaNoERP(dctx, provider, mov.ExternalProductID, mov.Quantity, obs)
	} else {
		movementID, err = provider.ReserveStock(dctx, mov.ExternalProductID, mov.Quantity, float64(mov.UnitPriceCents)/100, obs)
	}
	s.finishStockMovement(ctx, mov, movementID, err)
}

// movementStatusForError traduz o erro do provider no estado do movimento. É a
// ÚNICA tabela de classificação — reserva e estorno usam a mesma, porque a
// física é a mesma: só prova de não-entrega autoriza repetir.
func movementStatusForError(err error) string {
	if errors.Is(err, providers.ErrProvenUndelivered) {
		return MovementFailed
	}
	return MovementUnconfirmed
}

// finishStockMovement classifica o desfecho e aplica as consequências.
func (s *Service) finishStockMovement(ctx context.Context, mov *StockMovementRow, erpMovementID string, err error) {
	log := logger.From(ctx, s.logger).With(
		zap.String("movement_id", mov.ID),
		zap.String("idempotency_key", mov.IdempotencyKey),
		zap.String("cart_id", mov.CartID),
		zap.String("product_id", mov.ProductID),
		zap.String("external_product_id", mov.ExternalProductID),
		zap.Int("quantity", mov.Quantity),
	)

	switch {
	case err == nil:
		if markErr := s.movements.MarkERPStockMovementConfirmed(ctx, mov.ID, erpMovementID); markErr != nil {
			// O lançamento EXISTE no ERP e o razão não sabe. É o único caminho
			// que reabre o vão que esta tabela fecha — por isso grita.
			log.Error("stock movement confirmed by ERP but the ledger update failed — reconcile by the idempotency key",
				zap.String("erp_movement_id", erpMovementID), zap.Error(markErr))
			return
		}
		// O agregado (stock_reservations) continua sendo o que o resto do
		// sistema lê — reversão no pagamento, promoção de fila, reconciliação.
		// Upsert atômico: cobre criação e repeat-add sem ler antes (12/08).
		if mov.Direction == "out" {
			if _, upErr := s.repo.UpsertActiveReservationQuantity(ctx, UpsertReservationParams{
				EventID:           mov.EventID,
				CartID:            mov.CartID,
				ProductID:         mov.ProductID,
				ExternalProductID: mov.ExternalProductID,
				IncQty:            mov.Quantity,
				ERPMovementID:     erpMovementID,
			}); upErr != nil {
				log.Error("stock movement confirmed but the reservation aggregate was not applied — the payment-time reversal will miss it",
					zap.String("erp_movement_id", erpMovementID), zap.Error(upErr))
				return
			}
		}
		log.Info("stock movement confirmed",
			zap.String("erp_movement_id", erpMovementID),
			zap.String("direction", mov.Direction))

	case errors.Is(err, providers.ErrProvenUndelivered):
		if markErr := s.movements.MarkERPStockMovementOutcome(ctx, mov.ID, MovementFailed, err.Error()); markErr != nil {
			log.Error("failed to record stock movement failure", zap.Error(markErr))
			return
		}
		attempts := mov.Attempts + 1
		if attempts >= movementMaxAttempts {
			log.Error("stock movement gave up after max attempts — parked as failed, blocking this cart's finalisation",
				zap.Int("attempts", attempts), zap.Error(err))
			return
		}
		log.Warn("stock movement provably undelivered — retry scheduled",
			zap.Int("attempts", attempts),
			zap.Duration("next_in", movementRetryDelay(attempts)),
			zap.Error(err))
		s.scheduleMovementResolve(ctx, mov.ID, attempts)

	default:
		// Timeout, 5xx, resposta ilegível: o ERP pode ter aplicado. Nenhuma
		// escrita cega resolve isto — qualquer palpite é errado num dos dois
		// mundos (provado no incidente: os dois mundos aconteceram na mesma
		// noite). Fica visível e trava a finalização do carrinho.
		if markErr := s.movements.MarkERPStockMovementOutcome(ctx, mov.ID, MovementUnconfirmed, err.Error()); markErr != nil {
			log.Error("failed to record ambiguous stock movement", zap.Error(markErr))
			return
		}
		log.Error("stock movement outcome UNKNOWN — check the product's Tiny extract for the idempotency key; finalisation is blocked until resolved",
			zap.Error(err))
	}
}

func (s *Service) scheduleMovementResolve(ctx context.Context, movementID string, attempts int) {
	if s.movementScheduler == nil {
		return
	}
	at := time.Now().Add(movementRetryDelay(attempts))
	if err := s.movementScheduler.ScheduleStockMovementResolve(ctx, movementID, at); err != nil {
		logger.From(ctx, s.logger).Warn("failed to schedule stock movement resolve — the finalisation gate remains the backstop",
			zap.String("movement_id", movementID), zap.Error(err))
	}
}

// RunScheduledMovementResolve é o resolver: uma passada sobre UM movimento.
//
// Chamado pelo comando agendado (asynq) e, inline, pelo gate da finalização.
// Claim-first: dois resolvers podem mirar a mesma linha, e quem não reivindicou
// não age — mesmo desenho da reversão de reservas.
func (s *Service) RunScheduledMovementResolve(ctx context.Context, movementID string) error {
	if s.movements == nil {
		return nil
	}
	row, err := s.movements.GetERPStockMovement(ctx, movementID)
	if err != nil {
		return fmt.Errorf("loading stock movement: %w", err)
	}
	if row == nil {
		return nil
	}
	log := logger.From(ctx, s.logger).With(
		zap.String("movement_id", row.ID),
		zap.String("idempotency_key", row.IdempotencyKey),
		zap.String("cart_id", row.CartID))

	switch row.Status {
	case MovementConfirmed:
		return nil

	case MovementUnconfirmed:
		// Sem prova não há ação automática. A linha existe para ser vista: o
		// extrato do produto no Tiny, filtrado pela chave na observação, é o
		// desempate — e ele é humano até a API oferecer consulta.
		log.Error("stock movement still unconfirmed — needs a human look at the Tiny extract (search the observação for the idempotency key)",
			zap.Int("attempts", row.Attempts),
			zap.String("last_error", row.LastError))
		return nil

	case MovementPending, MovementResolving:
		// Envelhecido = o processo morreu com a chamada em voo. O desfecho é
		// desconhecido por definição — vira unconfirmed, nunca retry. Os guards
		// de idade estão na query: linha recente não é reivindicável e cai no
		// nil abaixo (a goroutine dona ainda vai gravar o desfecho).
		claimed, claimErr := s.movements.ClaimERPStockMovement(ctx, row.ID, row.Status)
		if claimErr != nil {
			return fmt.Errorf("claiming stale stock movement: %w", claimErr)
		}
		if claimed == nil {
			return nil
		}
		if markErr := s.movements.MarkERPStockMovementOutcome(ctx, row.ID, MovementUnconfirmed,
			"process died mid-call; outcome unknown"); markErr != nil {
			return fmt.Errorf("marking stale movement unconfirmed: %w", markErr)
		}
		log.Error("stock movement was in-flight when its process died — parked as unconfirmed")
		return nil

	case MovementFailed:
		if row.Attempts >= movementMaxAttempts {
			log.Error("stock movement parked after max attempts — blocking this cart's finalisation",
				zap.Int("attempts", row.Attempts), zap.String("last_error", row.LastError))
			return nil
		}
		claimed, claimErr := s.movements.ClaimERPStockMovement(ctx, row.ID, MovementFailed)
		if claimErr != nil {
			return fmt.Errorf("claiming failed stock movement: %w", claimErr)
		}
		if claimed == nil {
			return nil
		}
		integration, err := s.repo.GetActiveByProvider(ctx, row.StoreID, "erp", "tiny")
		if err != nil {
			// Integração sumiu (desligada?): devolve a failed para o próximo
			// gate decidir com contexto.
			_ = s.movements.MarkERPStockMovementOutcome(ctx, row.ID, MovementFailed, "no active ERP integration at retry time")
			return nil
		}
		provider, err := s.collab.ResolveProvider(ctx, integration)
		if err != nil {
			_ = s.movements.MarkERPStockMovementOutcome(ctx, row.ID, MovementFailed, fmt.Sprintf("resolving provider at retry: %v", err))
			return nil
		}
		var obs string
		if claimed.Direction == "in" {
			obs = fmt.Sprintf("Estorno LiveCart [%s] - Cart %s (retry %d)", claimed.IdempotencyKey, claimed.CartID, claimed.Attempts)
		} else {
			obs = movementObservacao(s.cartRef(ctx, claimed.CartID), "", claimed.Attempts)
		}
		s.executeStockMovement(ctx, provider, claimed, obs)
		return nil
	}
	return nil
}

// ResolveCartMovementsBeforeFinalisation é o gate: nada de pedido pago com
// movimento em dúvida no carrinho.
//
// A finalização estorna as reservas ATIVAS e cria o pedido, que baixa o
// estoque de novo. Um movimento que entrou no ERP sem agregado (o caso órfão de
// 17/08) escapa desse estorno e vira baixa dobrada — permanente e invisível.
// Segurar a finalização por alguns minutos é o erro barato; o resumo dela já é
// retentável por desenho ([S0]).
//
// Dá uma chance inline a cada pendência (failed ganha o retry na hora; pending
// envelhecido vira unconfirmed) e só então decide.
func (s *Service) ResolveCartMovementsBeforeFinalisation(ctx context.Context, cartID string) error {
	if s.movements == nil {
		return nil
	}
	rows, err := s.movements.ListUnresolvedERPStockMovementsByCart(ctx, cartID)
	if err != nil {
		return fmt.Errorf("listing cart stock movements: %w", err)
	}
	for _, r := range rows {
		if resolveErr := s.RunScheduledMovementResolve(ctx, r.ID); resolveErr != nil {
			logger.From(ctx, s.logger).Warn("inline stock movement resolve failed",
				zap.String("movement_id", r.ID), zap.Error(resolveErr))
		}
	}
	rows, err = s.movements.ListUnresolvedERPStockMovementsByCart(ctx, cartID)
	if err != nil {
		return fmt.Errorf("re-listing cart stock movements: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, fmt.Sprintf("%s [%s]", r.Status, r.IdempotencyKey))
	}
	return fmt.Errorf("cart %s has %d unresolved ERP stock movement(s): %v — finalisation blocked to avoid a double stock decrement; resolve via the product's Tiny extract", cartID, len(rows), keys)
}

// chaveDeSerializacao é o que a fila serializa. É o CARRINHO, não o produto: as
// escritas que se corrompem entre si são as do mesmo pedido no ERP, e um
// carrinho vira um pedido. Serializar por produto deixaria duas escritas do
// mesmo pedido correrem juntas, que é exatamente a corrida medida.
func chaveDeSerializacao(mov *StockMovementRow) string {
	if mov.CartID != "" {
		return "cart:" + mov.CartID
	}
	return "prod:" + mov.ExternalProductID
}

// naoDespachada embrulha o erro no contrato que o razão já entende. Sem isto o
// desfecho cairia no ramo `default` de finishStockMovement e viraria
// `unconfirmed`, travando a finalização de um carrinho pago por uma escrita que
// nunca chegou a existir.
func naoDespachada(cause error) error {
	if cause == nil {
		cause = erpwrite.ErrNotDispatched
	}
	return fmt.Errorf("escrita não despachada: %w",
		errors.Join(providers.ErrProvenUndelivered, cause))
}
