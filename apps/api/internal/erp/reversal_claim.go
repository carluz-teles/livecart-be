package erp

// Ponto ÚNICO do estorno de reserva contra o ERP.
//
// Existia a mesma sequência copiada em cinco lugares — expiração, pós-pagamento,
// conversão design C, expiração por produto e bloqueio de cliente — e as cinco
// tinham o mesmo defeito: chamar o ERP primeiro e marcar a linha depois. Três
// delas até documentavam o risco em comentário ("um retry re-estornaria esta row
// e DUPLICARIA a entrada E") e mesmo assim seguiam adiante, logando.
//
// Em 08/08 isso saiu do comentário e virou estoque: a marcação da reserva
// f4590b1f morreu em "context deadline exceeded" depois de o Tiny já ter
// aceitado a entrada, a asynq retentou, e o extrato do Tiny ficou com duas
// entradas de 2 unidades para o mesmo movimento. Um produto com 5 unidades
// terminou com 7.
//
// A ordem correta é reivindicar antes de agir, e ela mora aqui para não haver
// uma sexta cópia com a ordem errada.

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
)

// ReversibleReservation é o mínimo que o estorno precisa saber de uma reserva.
// Struct própria em vez do row do banco para os dois pacotes chamadores
// poderem usar o mesmo helper sem arrastar o tipo de um para o outro.
type ReversibleReservation struct {
	ID                string
	ExternalProductID string
	Quantity          int
	// Identidade para o razão de movimentos (000132/000133). Vazios no modo
	// legado — o razão só é usado quando hooks != nil, e aí os chamadores
	// preenchem a partir da StockReservationRow.
	CartID    string
	EventID   string
	ProductID string
}

// ReservationClaimer é a parte do repositório que o estorno usa. Interface
// estreita de propósito: quem chama já tem um repositório inteiro, e amarrar o
// helper a ele impediria o reuso entre pacotes.
type ReservationClaimer interface {
	// ClaimReservationForReversal devolve true só para quem ganhou a corrida.
	ClaimReservationForReversal(ctx context.Context, reservationID string) (bool, error)
	// RestoreReservationToActive desfaz a reivindicação quando o ERP recusa.
	RestoreReservationToActive(ctx context.Context, reservationID string) error
}

// claimCtxRepo roteia a reivindicação e a restauração por um contexto PRÓPRIO,
// desligado do cancelamento do handler.
//
// O handler de expiração tem 15s de orçamento e cada chamada ao ERP custa ~1s
// pelo limitador. Herdando o contexto dele, as últimas reivindicações de um
// carrinho grande morrem por prazo — e foi exatamente nelas que a marcação
// falhou em 08/08. O registro do que aconteceu no ERP não pode depender de
// sobrar tempo no handler.
//
// Guarda uma FÁBRICA de contextos, não um contexto. Guardar um só era a outra
// metade do mesmo defeito, apesar de o comentário vizinho já prometer o
// contrário: um orçamento de 10s criado antes do laço é consumido pelas
// chamadas ao ERP, e as últimas restaurações rodam com o prazo já vencido.
//
// Em 12/08/2026 isso custou uma unidade do Perfume Cebolinha: a reserva
// 158b36e7 foi reivindicada com 1,5s de orçamento restante, o Tiny travou, e o
// RestoreReservationToActive rodou com o contexto morto há 5 segundos. A linha
// ficou 'reversed' sem movimento, e a retentativa não a viu — ela filtra por
// 'active'. A unidade saiu do Tiny e não voltou nunca.
type claimCtxRepo struct {
	repo   ERPRepository
	newCtx func() (context.Context, context.CancelFunc)
}

func (c claimCtxRepo) ClaimReservationForReversal(_ context.Context, id string) (bool, error) {
	ctx, cancel := c.newCtx()
	defer cancel()
	return c.repo.ClaimReservationForReversal(ctx, id)
}

func (c claimCtxRepo) RestoreReservationToActive(_ context.Context, id string) error {
	ctx, cancel := c.newCtx()
	defer cancel()
	return c.repo.RestoreReservationToActive(ctx, id)
}

// ReverseReservationsClaimFirst estorna cada reserva no máximo UMA vez, mesmo
// sob retentativa ou execução concorrente.
//
// Devolve o número de reservas efetivamente estornadas nesta execução e se
// TODAS as pedidas terminaram resolvidas. Uma reserva que outra execução já
// estornou não conta como estornada aqui, e também não é falha — é o caso
// normal da retentativa.
//
// A direção do erro é deliberada: falhar entre a reivindicação e a resposta do
// ERP deixa a unidade FORA do ERP, nunca inventada nele. Faltar unidade é
// visível e reconciliável; unidade fantasma vira venda de produto inexistente.
func ReverseReservationsClaimFirst(
	ctx context.Context,
	log *zap.Logger,
	claimer ReservationClaimer,
	provider providers.ERPProvider,
	rows []ReversibleReservation,
	observation func(ReversibleReservation) string,
	hooks *ReversalLedger,
) (reversed int, allResolved bool) {
	ids, ok := reverseAndCollect(ctx, log, claimer, provider, rows, observation, hooks)
	return len(ids), ok
}

// executarEntradaNoERP é a única chamada crua de estorno autorizada pela
// catraca de convenção — e o motivo de ela morar NESTE arquivo. Todo caminho
// que chega aqui tem a reserva já reivindicada: o inline logo abaixo reivindica
// antes, e o resolver do razão só re-executa movimento cuja reivindicação
// nunca foi desfeita.
func executarEntradaNoERP(ctx context.Context, provider providers.ERPProvider, externalProductID string, quantity int, obs string) (string, error) {
	return provider.ReverseStockReservation(ctx, externalProductID, quantity, 0, obs)
}

// reverseAndCollect é o corpo do estorno e devolve QUAIS reservas moveram
// estoque nesta execução. A finalização pós-pagamento precisa dessa lista: se o
// pedido falhar depois, ela re-cria exatamente as saídas que desfez — recriar
// uma que nunca foi estornada baixaria estoque duas vezes.
func reverseAndCollect(
	ctx context.Context,
	log *zap.Logger,
	claimer ReservationClaimer,
	provider providers.ERPProvider,
	rows []ReversibleReservation,
	observation func(ReversibleReservation) string,
	hooks *ReversalLedger,
) (reversedIDs []string, allResolved bool) {
	allResolved = true

	for _, r := range rows {
		claimed, claimErr := claimer.ClaimReservationForReversal(ctx, r.ID)
		if claimErr != nil {
			allResolved = false
			logger.From(ctx, log).Error("failed to claim reservation for reversal",
				zap.String("reservation_id", r.ID),
				zap.String("external_product_id", r.ExternalProductID),
				zap.Error(claimErr))
			continue
		}
		if !claimed {
			// Outra execução já cuidou desta. Silêncio: é o caminho feliz da
			// retentativa, e barulho aqui esconderia o que importa.
			continue
		}

		// Modo razão: a intenção antes da chamada, e a ambiguidade NUNCA volta
		// para retry cego. Ver estornarComRazao para as regras.
		if hooks != nil && hooks.Movements != nil {
			if estornarComRazao(ctx, log, provider, r, observation(r), hooks, claimer) {
				reversedIDs = append(reversedIDs, r.ID)
			} else {
				allResolved = false
			}
			continue
		}

		if _, reverseErr := provider.ReverseStockReservation(ctx, r.ExternalProductID, r.Quantity, 0, observation(r)); reverseErr != nil {
			allResolved = false
			restoreErr := claimer.RestoreReservationToActive(ctx, r.ID)
			logger.From(ctx, log).Warn("failed to reverse ERP reservation",
				zap.String("reservation_id", r.ID),
				zap.String("external_product_id", r.ExternalProductID),
				zap.Int("quantity", r.Quantity),
				zap.Bool("claim_restored", restoreErr == nil),
				zap.Error(reverseErr))
			if restoreErr != nil {
				// A reserva fica reivindicada sem o movimento ter acontecido:
				// a unidade some do ERP e nenhuma tentativa futura a enxerga.
				// É o pior desfecho possível deste caminho, e precisa gritar.
				logger.From(ctx, log).Error("ERP refused and the claim could not be restored — unit stuck out of the ERP",
					zap.String("reservation_id", r.ID),
					zap.String("external_product_id", r.ExternalProductID),
					zap.Error(restoreErr))
			}
			continue
		}
		reversedIDs = append(reversedIDs, r.ID)
	}
	return reversedIDs, allResolved
}

// estornarComRazao executa a entrada de UMA reserva já reivindicada, com a
// intenção gravada antes. Devolve true só quando o ERP confirmou.
//
// A diferença de comportamento em relação ao modo legado está nos erros, e é a
// razão de a fase 2 existir:
//
//	provado não-entregue  a reserva FICA reivindicada e o retry é do resolver,
//	                      espaçado e com teto — restaurá-la para 'active'
//	                      deixaria a próxima varredura repetir sem prova.
//	ambíguo (timeout/5xx) NADA repete. Em 08/08 um retry cego de entrada deixou
//	                      um produto de 5 unidades com 7; a linha fica visível e
//	                      o desempate é o extrato, pela chave na observação.
//
// A direção do erro segue a do claim-first: falha deixa a unidade FORA do
// ERP (a menos, nunca a mais). Faltar é visível e reconciliável; sobrar vira
// venda de produto inexistente.
//
// Escritas do razão rodam com contexto desligado do cancelamento do chamador —
// foi um contexto morto na hora de registrar que perdeu a unidade do Perfume
// em 12/08.
func estornarComRazao(
	ctx context.Context,
	log *zap.Logger,
	provider providers.ERPProvider,
	r ReversibleReservation,
	obs string,
	hooks *ReversalLedger,
	claimer ReservationClaimer,
) bool {
	dbCtx, dbCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer dbCancel()

	mov, movErr := hooks.Movements.CreateERPStockMovement(dbCtx, CreateStockMovementParams{
		StoreID:           hooks.StoreID,
		CartID:            r.CartID,
		EventID:           r.EventID,
		ProductID:         r.ProductID,
		ExternalProductID: r.ExternalProductID,
		Direction:         "in",
		Quantity:          r.Quantity,
		ReservationID:     r.ID,
	})
	if movErr != nil {
		// Sem registro de intenção não há chamada — é o registro que garante que
		// nenhum desfecho se perde. Aqui nada foi enviado, então restaurar é
		// seguro e devolve a reserva para a próxima varredura.
		if restoreErr := claimer.RestoreReservationToActive(ctx, r.ID); restoreErr != nil {
			logger.From(ctx, log).Error("could not record reversal intent NOR restore the claim — unit stuck out of the ERP",
				zap.String("reservation_id", r.ID), zap.Error(movErr), zap.Error(restoreErr))
		} else {
			logger.From(ctx, log).Error("could not record reversal intent; claim restored for a future sweep",
				zap.String("reservation_id", r.ID), zap.Error(movErr))
		}
		return false
	}

	obsComChave := fmt.Sprintf("%s [%s]", obs, mov.IdempotencyKey)
	postCtx, postCancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer postCancel()

	erpMovementID, postErr := executarEntradaNoERP(postCtx, provider, r.ExternalProductID, r.Quantity, obsComChave)
	l := logger.From(ctx, log).With(
		zap.String("reservation_id", r.ID),
		zap.String("movement_id", mov.ID),
		zap.String("idempotency_key", mov.IdempotencyKey),
		zap.String("external_product_id", r.ExternalProductID),
		zap.Int("quantity", r.Quantity))

	if postErr == nil {
		if markErr := hooks.Movements.MarkERPStockMovementConfirmed(dbCtx, mov.ID, erpMovementID); markErr != nil {
			// A entrada EXISTE no Tiny e o razão não sabe. Grita com a chave —
			// e a reserva segue 'reversed', que é o estado verdadeiro.
			l.Error("reversal confirmed by ERP but the ledger update failed — reconcile by the idempotency key",
				zap.String("erp_movement_id", erpMovementID), zap.Error(markErr))
		}
		return true
	}

	status := movementStatusForError(postErr)
	if markErr := hooks.Movements.MarkERPStockMovementOutcome(dbCtx, mov.ID, status, postErr.Error()); markErr != nil {
		l.Error("failed to record reversal outcome", zap.Error(markErr), zap.Error(postErr))
		return false
	}
	if status == MovementFailed {
		attempts := mov.Attempts + 1
		if attempts >= movementMaxAttempts || hooks.Scheduler == nil {
			l.Error("reversal provably undelivered and out of retries — parked; the unit stays out of the ERP until resolved",
				zap.Int("attempts", attempts), zap.Error(postErr))
			return false
		}
		l.Warn("reversal provably undelivered — retry scheduled",
			zap.Int("attempts", attempts),
			zap.Duration("next_in", movementRetryDelay(attempts)),
			zap.Error(postErr))
		if schedErr := hooks.Scheduler.ScheduleStockMovementResolve(ctx, mov.ID, time.Now().Add(movementRetryDelay(attempts))); schedErr != nil {
			l.Warn("failed to schedule reversal retry — the finalisation gate remains the backstop", zap.Error(schedErr))
		}
		return false
	}

	// Ambíguo: o Tiny pode ter aplicado a entrada e falhado só em responder.
	// Repetir dobraria o estoque (08/08). A reserva fica 'reversed' — o estado
	// mais provável e o mais seguro (unidade a menos no ERP, nunca a mais) — e
	// a linha do razão fica visível para o desempate humano pelo extrato.
	l.Error("reversal outcome UNKNOWN — check the product's Tiny extract for the idempotency key",
		zap.Error(postErr))
	return false
}
