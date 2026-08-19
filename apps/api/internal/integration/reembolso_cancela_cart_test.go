package integration

// Reembolso tem que matar o carrinho — senão o pedido fica preso na triagem.
//
// Dois pedidos de teste reembolsados em 16/08 ficaram três dias em "Precisam
// atenção": o fan-out do estorno mexia em cobrança, cupom, e-mail e ERP, mas
// nunca no status do carrinho. Com o carrinho 'active'+refunded: a aba
// "Cancelados" nunca o mostra (filtra por status do carrinho, de propósito), o
// matcher de atenção casa payment_status='refunded' para sempre, e o
// cancelamento manual RECUSA carrinho não-pendente ("pagamento vence"). Uma
// fila de triagem sem saída.

import (
	"context"
	"testing"
)

func refundarCart(t *testing.T, cartID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE carts SET payment_status = 'refunded' WHERE id = $1`, cartID); err != nil {
		t.Fatalf("marcando reembolso: %v", err)
	}
}

func estadoDoCart(t *testing.T, cartID string) (status, reason string) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(),
		`SELECT status, COALESCE(cancelled_reason,'') FROM carts WHERE id = $1`, cartID).
		Scan(&status, &reason); err != nil {
		t.Fatalf("lendo cart: %v", err)
	}
	return
}

func TestReembolsoCancelaOCarrinho(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	refundarCart(t, fx.cartID)
	svc := newFinalisationService(newScriptedERP())

	if err := svc.ReactCartRefunded(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("ReactCartRefunded: %v", err)
	}

	status, reason := estadoDoCart(t, fx.cartID)
	if status != "cancelled" || reason != "refunded" {
		t.Fatalf("cart = %s/%s; esperava cancelled/refunded — sem o flip o pedido "+
			"não chega em 'Cancelados' e fica na triagem para sempre", status, reason)
	}

	// Redelivery do asynq: o flip é idempotente e não vira erro.
	if err := svc.ReactCartRefunded(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("segunda entrega: %v", err)
	}
	if status, reason = estadoDoCart(t, fx.cartID); status != "cancelled" || reason != "refunded" {
		t.Fatalf("redelivery mudou o estado: %s/%s", status, reason)
	}
}

// O guard: o flip NUNCA toca carrinho que não foi reembolsado. Um cart.refunded
// entregue atrasado depois de um RestoreCancelledCartAsPaid (pagamento venceu)
// não pode matar a venda restaurada.
func TestFlipDeReembolsoNaoTocaCarrinhoPago(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0) // payment_status = 'paid'
	svc := newFinalisationService(newScriptedERP())

	if err := svc.ReactCartRefunded(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("ReactCartRefunded: %v", err)
	}
	if status, _ := estadoDoCart(t, fx.cartID); status == "cancelled" {
		t.Fatal("um carrinho PAGO foi cancelado pelo reator de reembolso")
	}
}

// A saída da triagem, de ponta a ponta no SQL do painel: reembolsado+cancelado
// não casa mais o matcher de atenção, e reembolsado ainda-ativo (o estoque
// histórico pré-migração) continua casando — a 000135 é quem o converte.
func TestReembolsadoCanceladoSaiDaTriagem(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	refundarCart(t, fx.cartID)

	// O MESMO matcher do repositório de pedidos (order/repository.go), com o
	// join real: LEFT JOIN order_payments op ON op.order_id = o.id.
	matcher := `(COALESCE(op.erp_finalisation_status,'') = 'failed'
	              OR (c.payment_status IN ('failed','refunded') AND c.status NOT IN ('cancelled','expired'))
	              OR EXISTS (SELECT 1 FROM shipments sh WHERE sh.cart_id = c.id AND sh.status IN ('issue')))`
	pega := func() bool {
		var n int
		if err := testPool.QueryRow(context.Background(),
			`SELECT count(*) FROM carts c
			  LEFT JOIN orders o ON o.cart_id = c.id
			  LEFT JOIN order_payments op ON op.order_id = o.id
			 WHERE c.id = $1 AND `+matcher, fx.cartID).Scan(&n); err != nil {
			t.Fatalf("matcher: %v", err)
		}
		return n > 0
	}

	if !pega() {
		t.Fatal("reembolsado AINDA ATIVO tem que estar na triagem — é trabalho pendente")
	}
	svc := newFinalisationService(newScriptedERP())
	if err := svc.ReactCartRefunded(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatal(err)
	}
	if pega() {
		t.Fatal("reembolsado+cancelado continua na triagem — a fila ficou sem saída de novo")
	}
}
