package erp

// Resolução HUMANA de um movimento em dúvida.
//
// A API v3 do Tiny não tem consulta de lançamentos; o extrato na tela é a única
// fonte de verdade para um `unconfirmed`. Este arquivo é a ponte entre o olho
// humano e o razão: o lojista confere o extrato pela chave de idempotência
// (impressa na observação de cada lançamento) e responde UMA pergunta — o
// lançamento está lá?
//
// Antes disto, a resposta virava UPDATE manual no banco de produção. Foi assim
// que o caso elima2013 foi destravado em 19/08 — funcionou, mas escrever SQL em
// produção não pode ser o procedimento normal de operação.

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// PendingStockMovement é a linha do painel de pendências: o movimento mais o
// contexto que o humano precisa para achar o lançamento no extrato.
type PendingStockMovement struct {
	StockMovementRow
	ProductName    string
	ProductKeyword string
	CartHandle     string
}

// StockMovementResolution é a parte do repositório que a resolução manual usa.
type StockMovementResolution interface {
	ListUnresolvedERPStockMovementsByStore(ctx context.Context, storeID string) ([]PendingStockMovement, error)
	// ConfirmERPStockMovementManually faz o CAS failed|unconfirmed → confirmed.
	// nil sem erro = o estado mudou por baixo (resolver em voo, já resolvido).
	ConfirmERPStockMovementManually(ctx context.Context, movementID, storeID string) (*StockMovementRow, error)
	// ResetERPStockMovementForRetry faz o CAS failed|unconfirmed → failed com
	// as tentativas zeradas, autorizando o resolver a re-executar.
	ResetERPStockMovementForRetry(ctx context.Context, movementID, storeID string) (*StockMovementRow, error)
}

// SetStockMovementResolution liga a resolução manual (opcional, como o razão).
func (s *Service) SetStockMovementResolution(r StockMovementResolution) { s.movementResolution = r }

// ListPendingStockMovements lista as pendências da loja para o painel.
func (s *Service) ListPendingStockMovements(ctx context.Context, storeID string) ([]PendingStockMovement, error) {
	if s.movementResolution == nil {
		return nil, nil
	}
	return s.movementResolution.ListUnresolvedERPStockMovementsByStore(ctx, storeID)
}

// ResolveStockMovementManually aplica a decisão humana sobre UM movimento.
//
// landed=true  → o lançamento está no extrato: confirma e aplica o agregado
//
//	(é o agregado que o estorno do pagamento enxerga).
//
// landed=false → não está: provado não-entregue pelo olho, zera as tentativas
//
//	e agenda a re-execução imediata pelo resolver.
//
// O CAS na query é quem protege contra corrida com o resolver: linha em
// 'resolving' (ou que confirmou por baixo) não aceita decisão manual — o
// chamador recebe 409 e recarrega o painel.
func (s *Service) ResolveStockMovementManually(ctx context.Context, storeID, movementID string, landed bool) (*StockMovementRow, error) {
	if s.movementResolution == nil || s.movements == nil {
		return nil, httpx.ErrNotFound("razão de movimentos não está habilitado")
	}

	current, err := s.movements.GetERPStockMovement(ctx, movementID)
	if err != nil {
		return nil, fmt.Errorf("loading stock movement: %w", err)
	}
	if current == nil || current.StoreID != storeID {
		return nil, httpx.ErrNotFound("movimento não encontrado")
	}

	log := logger.From(ctx, s.logger).With(
		zap.String("movement_id", movementID),
		zap.String("idempotency_key", current.IdempotencyKey),
		zap.String("cart_id", current.CartID),
		zap.Bool("landed", landed),
	)

	if !landed {
		row, err := s.movementResolution.ResetERPStockMovementForRetry(ctx, movementID, storeID)
		if err != nil {
			return nil, fmt.Errorf("resetting stock movement for retry: %w", err)
		}
		if row == nil {
			return nil, httpx.DomainError(409, httpx.CodeStockMovementStale,
				"o movimento mudou de estado — recarregue as pendências")
		}
		log.Info("stock movement manually resolved as not-landed — retry authorized")
		s.scheduleMovementResolve(ctx, movementID, 0)
		return row, nil
	}

	row, err := s.movementResolution.ConfirmERPStockMovementManually(ctx, movementID, storeID)
	if err != nil {
		return nil, fmt.Errorf("confirming stock movement manually: %w", err)
	}
	if row == nil {
		return nil, httpx.DomainError(409, httpx.CodeStockMovementStale,
			"o movimento mudou de estado — recarregue as pendências")
	}

	// O agregado é o que o resto do sistema lê (estorno no pagamento, fila,
	// reconciliação). Confirmar o movimento sem aplicá-lo deixaria o lançamento
	// órfão de novo — exatamente o que a confirmação manual existe para fechar.
	if row.Direction == "out" {
		if _, upErr := s.repo.UpsertActiveReservationQuantity(ctx, UpsertReservationParams{
			EventID:           row.EventID,
			CartID:            row.CartID,
			ProductID:         row.ProductID,
			ExternalProductID: row.ExternalProductID,
			IncQty:            row.Quantity,
			ERPMovementID:     row.ERPMovementID,
		}); upErr != nil {
			log.Error("manual confirmation applied but the reservation aggregate failed — the payment-time reversal will miss it",
				zap.Error(upErr))
			return nil, fmt.Errorf("applying reservation aggregate: %w", upErr)
		}
	}
	log.Info("stock movement manually resolved as landed")
	return row, nil
}
