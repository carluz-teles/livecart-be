package integration

// A divisão pago × a pagar, contra o Postgres real.
//
// O carrinho deixou de morrer no pagamento: enquanto o pedido não vira nota ele
// continua recebendo item, e o lojista despacha olhando duas metades — o que a
// compradora já pagou e o que ela ainda deve. Estes testes rodam as queries
// GERADAS, não uma cópia à mão delas: o que se quer provar é justamente que a
// aritmética dentro do CTE fecha.
//
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	  go test ./apps/api/internal/integration/ -run DivisaoDoPagamento -v

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
)

type carrinhoDeTeste struct {
	id       string
	produtoA string
	produtoB string
	q        *sqlc.Queries
}

func semearCarrinho(t *testing.T) carrinhoDeTeste {
	t.Helper()
	ctx := context.Background()
	n := fmt.Sprintf("%d", time.Now().UnixNano())
	c := carrinhoDeTeste{q: sqlc.New(testPool)}

	var storeID, eventID string
	mustScan := func(dst *string, sql string, args ...any) {
		t.Helper()
		if err := testPool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("semeando: %v", err)
		}
	}
	mustScan(&storeID, `INSERT INTO stores (name, slug) VALUES ('Loja Split','split-'||$1) RETURNING id::text`, n)
	mustScan(&eventID, `INSERT INTO live_events (store_id, status, title, ends_at)
		VALUES ($1,'active','Live Split',now()+interval '7 days') RETURNING id::text`, storeID)
	mustScan(&c.id, `INSERT INTO carts (event_id, store_id, platform_user_id, platform_handle, token, short_id, status)
		VALUES ($1,$2,'u-'||$3,'@split'||$3,'tok-'||$3, ($3)::bigint % 100000, 'active') RETURNING id::text`, eventID, storeID, n)
	// keyword é char(4): a loja é nova a cada teste, então duas fixas bastam.
	mustScan(&c.produtoA, `INSERT INTO products (store_id, name, keyword, external_source, price)
		VALUES ($1,'Vestido','vest','manual', 2000) RETURNING id::text`, storeID)
	mustScan(&c.produtoB, `INSERT INTO products (store_id, name, keyword, external_source, price)
		VALUES ($1,'Brinco','brin','manual', 500) RETURNING id::text`, storeID)
	return c
}

// somar encena o comentário da live: entra no carrinho pelo mesmo upsert.
func (c carrinhoDeTeste) somar(t *testing.T, produto string, qtd int, preco int64) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price)
		 VALUES ($1,$2,$3,$4)
		 ON CONFLICT (cart_id, product_id) DO UPDATE SET quantity = cart_items.quantity + EXCLUDED.quantity`,
		c.id, produto, qtd, preco); err != nil {
		t.Fatalf("somando item: %v", err)
	}
}

// pagar roda a query GERADA de pagamento.
func (c carrinhoDeTeste) pagar(t *testing.T, idDoPagamento string) {
	t.Helper()
	agora := time.Now()
	if _, err := c.q.UpdateCartPayment(context.Background(), sqlc.UpdateCartPaymentParams{
		ID:            pgUUID(t, c.id),
		PaymentStatus: pgtype.Text{String: "paid", Valid: true},
		CheckoutID:    pgtype.Text{String: idDoPagamento, Valid: true},
		PaidAt:        pgtype.Timestamptz{Time: agora, Valid: true},
		PaymentMethod: pgtype.Text{String: "pix", Valid: true},
	}); err != nil {
		t.Fatalf("pagando: %v", err)
	}
}

func (c carrinhoDeTeste) dinheiro(t *testing.T) (pago, total, falta int64) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(),
		`SELECT paid_amount_cents, cart_product_total_cents(id), cart_unpaid_total_cents(id)
		 FROM carts WHERE id = $1`, c.id).Scan(&pago, &total, &falta); err != nil {
		t.Fatalf("lendo o dinheiro: %v", err)
	}
	return
}

func pgUUID(t *testing.T, id string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(id); err != nil {
		t.Fatalf("uuid %q: %v", id, err)
	}
	return u
}

// ─── O caso do lojista ──────────────────────────────────────────────────────

// Segunda: 2 vestidos + 1 brinco, R$ 45, pago. Quinta: mais 1 vestido.
// O pedido passa a valer R$ 65 e deve R$ 20 — e o vestido a mais fica na MESMA
// linha do que já foi pago, que é onde a marca por linha mentiria.
func TestDivisaoDoPagamentoSeparaOQueEntrouDepois(t *testing.T) {
	requireDB(t)
	c := semearCarrinho(t)
	c.somar(t, c.produtoA, 2, 2000)
	c.somar(t, c.produtoB, 1, 500)
	c.pagar(t, "pay-1")

	if pago, total, falta := c.dinheiro(t); pago != 4500 || total != 4500 || falta != 0 {
		t.Fatalf("depois de pagar: pago=%d total=%d falta=%d, quero 4500/4500/0", pago, total, falta)
	}

	c.somar(t, c.produtoA, 1, 2000) // mesma linha do produto já pago

	pago, total, falta := c.dinheiro(t)
	if pago != 4500 {
		t.Errorf("pago = %d, quero 4500 — somar item não pode fazer o carrinho "+
			"afirmar que ela pagou mais do que pagou", pago)
	}
	if total != 6500 {
		t.Errorf("total = %d, quero 6500", total)
	}
	if falta != 2000 {
		t.Errorf("falta pagar %d, quero 2000 — é a unidade que entrou na quinta", falta)
	}

	var qtd, pagas int
	if err := testPool.QueryRow(context.Background(),
		`SELECT quantity, paid_quantity FROM cart_items WHERE cart_id=$1 AND product_id=$2`,
		c.id, c.produtoA).Scan(&qtd, &pagas); err != nil {
		t.Fatalf("lendo a linha: %v", err)
	}
	if qtd != 3 || pagas != 2 {
		t.Errorf("a linha tem %d un. com %d paga(s), quero 3 e 2 — a contagem é "+
			"por unidade justamente porque as três moram na mesma linha", qtd, pagas)
	}
}

// O segundo pagamento cobre só o saldo, nunca o que já estava pago.
func TestSegundoPagamentoCobreApenasOSaldo(t *testing.T) {
	requireDB(t)
	c := semearCarrinho(t)
	c.somar(t, c.produtoA, 2, 2000)
	c.pagar(t, "pay-1")
	c.somar(t, c.produtoB, 3, 500)
	c.pagar(t, "pay-2")

	pago, total, falta := c.dinheiro(t)
	if pago != 5500 || total != 5500 || falta != 0 {
		t.Errorf("pago=%d total=%d falta=%d, quero 5500/5500/0 — o segundo "+
			"pagamento tinha de somar só os R$ 15 que faltavam", pago, total, falta)
	}
}

// Reentrega do webhook: o mesmo pagamento chegando duas vezes não pode dobrar o
// valor pago.
func TestReentregaDoPagamentoNaoDobraOValor(t *testing.T) {
	requireDB(t)
	c := semearCarrinho(t)
	c.somar(t, c.produtoA, 2, 2000)
	c.pagar(t, "pay-1")
	c.pagar(t, "pay-1")
	c.pagar(t, "pay-1")

	if pago, _, falta := c.dinheiro(t); pago != 4000 || falta != 0 {
		t.Errorf("pago = %d (falta %d), quero 4000 — três entregas do MESMO "+
			"pagamento não são três pagamentos", pago, falta)
	}
}

// O lojista reduz, pelo painel do ERP, uma quantidade já paga: o que falta pagar
// nunca fica negativo. O crédito a devolver aparece como total < pago, que é o
// sinal que RecomporParcelasDoPedidoPago recusa e denuncia.
func TestReducaoAbaixoDoPagoNaoGeraSaldoNegativo(t *testing.T) {
	requireDB(t)
	c := semearCarrinho(t)
	c.somar(t, c.produtoA, 3, 2000)
	c.pagar(t, "pay-1")

	if _, err := testPool.Exec(context.Background(),
		`UPDATE cart_items SET quantity = 1 WHERE cart_id=$1 AND product_id=$2`,
		c.id, c.produtoA); err != nil {
		t.Fatalf("reduzindo: %v", err)
	}

	pago, total, falta := c.dinheiro(t)
	if falta != 0 {
		t.Errorf("falta pagar %d, quero 0 — saldo negativo viraria cobrança "+
			"fantasma na tela", falta)
	}
	if total >= pago {
		t.Errorf("total=%d pago=%d — reduzir abaixo do pago tem de deixar o "+
			"total MENOR, que é o sinal de crédito a devolver", total, pago)
	}
}

// Carrinho não pago não tem nada pago: a divisão só existe depois do dinheiro.
func TestCarrinhoNaoPagoDeveTudo(t *testing.T) {
	requireDB(t)
	c := semearCarrinho(t)
	c.somar(t, c.produtoA, 2, 2000)

	if pago, total, falta := c.dinheiro(t); pago != 0 || falta != total {
		t.Errorf("pago=%d total=%d falta=%d, quero 0 pago e falta=total",
			pago, total, falta)
	}
}
