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
type claimCtxRepo struct {
	repo ERPRepository
	ctx  context.Context
}

func (c claimCtxRepo) ClaimReservationForReversal(_ context.Context, id string) (bool, error) {
	return c.repo.ClaimReservationForReversal(c.ctx, id)
}

func (c claimCtxRepo) RestoreReservationToActive(_ context.Context, id string) error {
	return c.repo.RestoreReservationToActive(c.ctx, id)
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
) (reversed int, allResolved bool) {
	ids, ok := reverseAndCollect(ctx, log, claimer, provider, rows, observation)
	return len(ids), ok
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
