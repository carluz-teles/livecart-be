package erp

// A decisão humana pós-extrato, contra as corridas que ela pode perder.
//
// O perigo desta feature é ela virar uma segunda fonte de escrita disputando
// com o resolver: humano decide "não entrou" no mesmo instante em que o
// resolver confirma a re-execução. O CAS na query é quem arbitra — estes testes
// provam que quem perde recebe 409 e ninguém escreve por cima de ninguém.

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
)

// fakeResolution estende o fakeLedger com o CAS manual.
type fakeResolution struct{ *fakeLedger }

func (f *fakeResolution) ListUnresolvedERPStockMovementsByStore(_ context.Context, storeID string) ([]PendingStockMovement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []PendingStockMovement
	for _, r := range f.rows {
		if r.StoreID == storeID && r.Status != MovementConfirmed {
			out = append(out, PendingStockMovement{StockMovementRow: *r, ProductName: "Fecho Prático", ProductKeyword: "1419"})
		}
	}
	return out, nil
}

func (f *fakeResolution) casManual(id, storeID, para string, resetAttempts bool) (*StockMovementRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok || r.StoreID != storeID {
		return nil, nil
	}
	if r.Status != MovementFailed && r.Status != MovementUnconfirmed {
		return nil, nil // CAS perdeu: pending/resolving/confirmed não aceitam decisão manual
	}
	r.Status = para
	if resetAttempts {
		r.Attempts = 0
	}
	c := *r
	return &c, nil
}

func (f *fakeResolution) ConfirmERPStockMovementManually(_ context.Context, id, storeID string) (*StockMovementRow, error) {
	return f.casManual(id, storeID, MovementConfirmed, false)
}

func (f *fakeResolution) ResetERPStockMovementForRetry(_ context.Context, id, storeID string) (*StockMovementRow, error) {
	return f.casManual(id, storeID, MovementFailed, true)
}

func servicoComResolucao(t *testing.T) (*Service, *fakeResolution, *mockRepo, *agendadorEspiao) {
	t.Helper()
	repo := &mockRepo{}
	svc := NewService(repo, &mockCollab{linked: true}, zap.NewNop())
	ledger := newFakeLedger()
	res := &fakeResolution{fakeLedger: ledger}
	svc.SetStockMovementLedger(ledger)
	svc.SetStockMovementResolution(res)
	sched := &agendadorEspiao{}
	svc.SetStockMovementScheduler(sched)
	return svc, res, repo, sched
}

func movimentoParado(t *testing.T, res *fakeResolution, status string) *StockMovementRow {
	t.Helper()
	mov, err := res.CreateERPStockMovement(context.Background(), CreateStockMovementParams{
		StoreID: "loja-1", CartID: "cart-1", EventID: "ev-1", ProductID: "p-1419",
		ExternalProductID: "843169697", Direction: "out", Quantity: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	res.mu.Lock()
	res.rows[mov.ID].Status = status
	res.mu.Unlock()
	return mov
}

func TestConfirmarNoExtratoAplicaOAgregado(t *testing.T) {
	svc, res, repo, _ := servicoComResolucao(t)
	mov := movimentoParado(t, res, MovementUnconfirmed)

	row, err := svc.ResolveStockMovementManually(context.Background(), "loja-1", mov.ID, true)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if row.Status != MovementConfirmed {
		t.Errorf("status = %s", row.Status)
	}
	if repo.reservationUpserts != 1 {
		t.Errorf("agregado aplicado %d vez(es) — confirmar sem aplicar deixaria o "+
			"lançamento órfão de novo, que é o caso elima2013", repo.reservationUpserts)
	}
}

func TestNaoEntrouZeraTentativasEAgendaJa(t *testing.T) {
	svc, res, repo, sched := servicoComResolucao(t)
	mov := movimentoParado(t, res, MovementUnconfirmed)
	res.mu.Lock()
	res.rows[mov.ID].Attempts = 5 // estava no teto: parado
	res.mu.Unlock()

	row, err := svc.ResolveStockMovementManually(context.Background(), "loja-1", mov.ID, false)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if row.Status != MovementFailed || row.Attempts != 0 {
		t.Errorf("status/attempts = %s/%d; o humano provou não-entrega, o teto "+
			"volta cheio", row.Status, row.Attempts)
	}
	if len(sched.agendas) != 1 || sched.agendas[0] > time.Minute {
		t.Errorf("re-execução não foi agendada de imediato: %v", sched.agendas)
	}
	if repo.reservationUpserts != 0 {
		t.Errorf("agregado aplicado sem confirmação")
	}
}

func TestDecisaoManualPerdeOCASePara409(t *testing.T) {
	svc, res, _, _ := servicoComResolucao(t)
	mov := movimentoParado(t, res, MovementResolving) // resolver em voo

	_, err := svc.ResolveStockMovementManually(context.Background(), "loja-1", mov.ID, true)
	var svcErr *httpx.ServiceError
	if !errors.As(err, &svcErr) || svcErr.Code != 409 {
		t.Fatalf("erro = %v; esperava 409 — o resolver está com a linha e o "+
			"humano não pode escrever por cima", err)
	}
	if got := res.status(mov.ID); got != MovementResolving {
		t.Errorf("a decisão manual mexeu numa linha em resolução: %s", got)
	}
}

func TestMovimentoDeOutraLojaEh404(t *testing.T) {
	svc, res, _, _ := servicoComResolucao(t)
	mov := movimentoParado(t, res, MovementUnconfirmed)

	_, err := svc.ResolveStockMovementManually(context.Background(), "loja-INTRUSA", mov.ID, true)
	var svcErr *httpx.ServiceError
	if !errors.As(err, &svcErr) || svcErr.Code != 404 {
		t.Fatalf("erro = %v; movimento de outra loja não pode nem ser visto", err)
	}
}

func TestEstornoConfirmadoManualmenteNaoMexeNoAgregado(t *testing.T) {
	svc, res, repo, _ := servicoComResolucao(t)
	mov := movimentoParado(t, res, MovementUnconfirmed)
	res.mu.Lock()
	res.rows[mov.ID].Direction = "in" // estorno: a reserva já está 'reversed'
	res.mu.Unlock()

	if _, err := svc.ResolveStockMovementManually(context.Background(), "loja-1", mov.ID, true); err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if repo.reservationUpserts != 0 {
		t.Errorf("estorno confirmado criou reserva (%d upserts) — a direção 'in' "+
			"não tem agregado a aplicar", repo.reservationUpserts)
	}
}
