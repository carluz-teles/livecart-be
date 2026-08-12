package erp

// A vida INTEIRA de um carrinho contra o razão da Tiny.
//
// A frase que define a invariante é do lojista: "a gente sempre decrementa ou
// acrescenta unidades". Se toda saída tem a sua entrada, então quando um
// carrinho termina a vida sem virar pedido, o saldo da Tiny tem de estar
// EXATAMENTE onde estava antes de o primeiro comentário chegar.
//
// Os testes que já existiam olhavam cada operação sozinha — reservar, ajustar,
// estornar. Nenhum somava a conta do começo ao fim, que é onde o erro de campo
// aparecia: as quatro linhas do extrato de 08/08 estavam individualmente
// plausíveis, e só a soma denunciava (saíram 2, voltaram 4).

import (
	"context"
	"fmt"
	"testing"

	"go.uber.org/zap"
)

// cicloRepo guarda reservas de verdade, para as três operações do ciclo
// enxergarem o efeito uma da outra — que é o que um mock de retorno fixo não
// consegue reproduzir.
type cicloRepo struct {
	ledgerRepo
	rows   map[string]*StockReservationRow
	nextID int
}

func newCicloRepo() *cicloRepo {
	r := &cicloRepo{rows: map[string]*StockReservationRow{}}
	r.status = map[string]string{}
	r.integration = &Integration{}
	return r
}

func (l *cicloRepo) CreateStockReservation(_ context.Context, p CreateStockReservationParams) (*StockReservationRow, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextID++
	row := &StockReservationRow{
		ID:                fmt.Sprintf("res-%d", l.nextID),
		CartID:            p.CartID,
		ProductID:         p.ProductID,
		ExternalProductID: p.ExternalProductID,
		Quantity:          p.Quantity,
		ERPMovementID:     p.ERPMovementID,
	}
	l.rows[row.ID] = row
	l.status[row.ID] = "active"
	return row, nil
}

func (l *cicloRepo) AdjustActiveReservationQuantity(_ context.Context, cartID, productID string, delta int, movementID string) (*StockReservationRow, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.rows {
		if r.CartID == cartID && r.ProductID == productID && l.status[r.ID] == "active" {
			r.Quantity += delta
			r.ERPMovementID = movementID
			return r, nil
		}
	}
	return nil, fmt.Errorf("sem reserva ativa para %s/%s", cartID, productID)
}

// DecrementActiveReservationQuantity espelha a query real: só baixa se a reserva
// tiver o tanto pedido, e quando a baixa consome tudo a linha sai de 'active'
// com a quantidade intacta (o CHECK (quantity > 0) proíbe zerar em vigor).
func (l *cicloRepo) DecrementActiveReservationQuantity(_ context.Context, cartID, productID string, dec int) (ReservationDecrement, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out ReservationDecrement
	for _, r := range l.rows {
		if r.CartID != cartID || r.ProductID != productID || l.status[r.ID] != "active" {
			continue
		}
		if r.Quantity < dec {
			return out, nil
		}
		out.Applied = true
		out.ReservationIDs = append(out.ReservationIDs, r.ID)
		if r.Quantity > dec {
			r.Quantity -= dec
			out.Remaining += r.Quantity
		} else {
			l.status[r.ID] = "reversed"
		}
		return out, nil
	}
	return out, nil
}

func (l *cicloRepo) RestoreReservationQuantityByID(_ context.Context, reservationID string, inc int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.rows[reservationID]
	if !ok {
		return fmt.Errorf("reserva %s inexistente", reservationID)
	}
	if l.status[reservationID] != "reversed" {
		r.Quantity += inc
	}
	l.status[reservationID] = "active"
	return nil
}

func (l *cicloRepo) ListActiveReservationsByCartAndProduct(_ context.Context, cartID, productID string) ([]StockReservationRow, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []StockReservationRow
	for _, r := range l.rows {
		if r.CartID == cartID && r.ProductID == productID && l.status[r.ID] == "active" {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (l *cicloRepo) ReverseReservationsByCartAndProduct(_ context.Context, cartID, productID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.rows {
		if r.CartID == cartID && r.ProductID == productID {
			l.status[r.ID] = "reversed"
		}
	}
	return nil
}

func (l *cicloRepo) ListActiveReservationsByCart(_ context.Context, cartID string) ([]StockReservationRow, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []StockReservationRow
	for _, r := range l.rows {
		if r.CartID == cartID && l.status[r.ID] == "active" {
			out = append(out, *r)
		}
	}
	return out, nil
}

// Carrinho de live comum: NÃO virou pedido no ERP, então quem segura a peça é
// a saída manual. OrderStateNone é o que empurra para esse caminho — o design C
// (pedido-como-reserva) tem ciclo próprio e não é o que quebrou em 08/08.
func (l *cicloRepo) GetCartERPOrderState(context.Context, string) (*CartERPOrderState, error) {
	return &CartERPOrderState{State: OrderStateNone}, nil
}

// -----------------------------------------------------------------------------

// operacao é um passo da vida do carrinho.
type operacao struct {
	tipo  string // "reserva" | "ajuste" | "expira"
	qtd   int    // quantidade da reserva, ou delta do ajuste
	repet int    // quantas vezes o passo é executado (simula retentativa)
}

// TestCicloDeVidaDevolveOSaldoExatoAoTiny é a conta que o lojista faz.
func TestCicloDeVidaDevolveOSaldoExatoAoTiny(t *testing.T) {
	casos := []struct {
		nome  string
		saldo int
		ops   []operacao
	}{
		{
			// O caso de campo: reserva 1, ajusta +1, expira.
			nome: "reserva 1, ajuste +1, expira",
			saldo: 5,
			ops: []operacao{
				{tipo: "reserva", qtd: 1},
				{tipo: "ajuste", qtd: +1},
				{tipo: "expira", repet: 4}, // asynq retentando
			},
		},
		{
			nome: "reserva sozinha e expira",
			saldo: 5,
			ops: []operacao{
				{tipo: "reserva", qtd: 2},
				{tipo: "expira", repet: 3},
			},
		},
		{
			nome:  "sobe e desce antes de expirar",
			saldo: 10,
			ops: []operacao{
				{tipo: "reserva", qtd: 1},
				{tipo: "ajuste", qtd: +3},
				{tipo: "ajuste", qtd: -2},
				{tipo: "expira", repet: 2},
			},
		},
		{
			nome:  "varios ajustes para cima",
			saldo: 20,
			ops: []operacao{
				{tipo: "reserva", qtd: 1},
				{tipo: "ajuste", qtd: +1},
				{tipo: "ajuste", qtd: +1},
				{tipo: "ajuste", qtd: +1},
				{tipo: "expira", repet: 4},
			},
		},
		{
			nome:  "expiracao chamada muitas vezes",
			saldo: 8,
			ops: []operacao{
				{tipo: "reserva", qtd: 3},
				{tipo: "expira", repet: 10}, // muito além do teto da asynq
			},
		},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			const (
				store   = "loja"
				cart    = "cart-1"
				event   = "evento-1"
				produto = "prod-1"
				externo = "ext-1"
			)
			repo := newCicloRepo()
			ledger := newTinyLedger(map[string]int{externo: c.saldo})
			svc := NewService(repo, &ledgerCollab{ledger: ledger}, zap.NewNop())

			for _, op := range c.ops {
				vezes := op.repet
				if vezes == 0 {
					vezes = 1
				}
				for i := 0; i < vezes; i++ {
					switch op.tipo {
					case "reserva":
						if err := svc.ReserveStockInERP(context.Background(), store, cart, event, produto, op.qtd, 2000, "@alisson"); err != nil {
							t.Fatalf("reserva: %v", err)
						}
					case "ajuste":
						if _, err := svc.AdjustStockReservationDelta(context.Background(), store, cart, event, produto, op.qtd, 2000, "@alisson", StockOpUnspecified); err != nil {
							t.Fatalf("ajuste %+d: %v", op.qtd, err)
						}
					case "expira":
						_ = svc.reverseCartReservationsInERP(context.Background(), cart, store)
					}
				}
			}

			if got := ledger.stock[externo]; got != c.saldo {
				t.Errorf("saldo final no Tiny = %d, quero %d (o inicial) — sobraram %d unidade(s) inventada(s)",
					got, c.saldo, got-c.saldo)
				for _, m := range ledger.movimentos {
					t.Logf("  %s", m)
				}
			}
		})
	}
}
