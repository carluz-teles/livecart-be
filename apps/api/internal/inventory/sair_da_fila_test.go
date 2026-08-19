package inventory_test

// Sair da fila em 'waiting' tem que sumir da TELA, não só da tabela.
//
// A fila vive em duas fontes: waitlist_items (a linha) e
// cart_items.waitlisted_quantity (o contador que o checkout mostra e que a
// cobrança/ERP subtraem). O cancelamento em 'waiting' matava só a linha. Na
// live de 17/08 a @daianyfer clicou para sair de duas filas — 7 e 5 unidades,
// 1,2s entre os cliques — e o carrinho dela continuou anunciando 12 unidades
// fantasma. O botão respondia "ok" e não mudava nada visível.

import (
	"context"
	"testing"

	"livecart/apps/api/internal/inventory"
)

func TestSairDaFilaEmWaitingDecrementaOContadorDoCarrinho(t *testing.T) {
	repo := &fakeRepo{
		item: &inventory.WaitlistItemRow{
			ID: "wl-1", CartID: "cart-daianyfer", ProductID: "p-1360",
			EventID: "ev-1", Quantity: 7, Status: "waiting",
		},
		found: true,
	}
	svc := newTestService(repo, &fakeCollab{})

	changed, err := svc.CancelWaitlistItem(context.Background(), "wl-1", "cart-daianyfer")
	if err != nil {
		t.Fatalf("CancelWaitlistItem: %v", err)
	}
	if !changed {
		t.Fatal("cancelamento não aconteceu")
	}
	if got := repo.waitlistedDecrements["cart-daianyfer|p-1360"]; got != 7 {
		t.Fatalf("contador do carrinho decrementado em %d; esperava 7 — sem isso "+
			"a compradora vê 'aguardando' para sempre", got)
	}
	// Em 'waiting' NADA foi separado: quantity, estoque e ERP não são tocados.
	if repo.decCalls != 0 {
		t.Errorf("quantity do item foi mexida (%d) — isso é só para 'notified'", repo.decCalls)
	}
}

func TestSairDaFilaEmNotifiedNaoMexeNoContadorDeFila(t *testing.T) {
	// Promovido: a promoção JÁ moveu fila→disponível; o cancelamento devolve a
	// unidade inteira (DecrementCartItem), não o contador de fila.
	repo := &fakeRepo{
		item: &inventory.WaitlistItemRow{
			ID: "wl-2", CartID: "cart-x", ProductID: "p-1",
			EventID: "ev-1", Quantity: 1, Status: "notified",
		},
	}
	svc := newTestService(repo, &fakeCollab{})

	if _, err := svc.CancelWaitlistItem(context.Background(), "wl-2", "cart-x"); err != nil {
		t.Fatalf("CancelWaitlistItem: %v", err)
	}
	if got := repo.waitlistedDecrements["cart-x|p-1"]; got != 0 {
		t.Errorf("contador de fila decrementado (%d) num 'notified' — dupla baixa", got)
	}
	if repo.decCalls != 1 {
		t.Errorf("DecrementCartItem chamado %d vez(es); esperava 1", repo.decCalls)
	}
}
