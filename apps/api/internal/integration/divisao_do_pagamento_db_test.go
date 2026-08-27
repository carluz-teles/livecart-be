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
	// cobrado é quanto a próxima cobrança vai debitar. -1 significa "o preço
	// cheio do que ainda falta" — sem desconto.
	cobrado  int64
	id       string
	produtoA string
	produtoB string
	q        *sqlc.Queries
}

func semearCarrinho(t *testing.T) carrinhoDeTeste {
	t.Helper()
	ctx := context.Background()
	n := fmt.Sprintf("%d", time.Now().UnixNano())
	c := carrinhoDeTeste{q: sqlc.New(testPool), cobrado: -1}

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
	if c.cobrado < 0 {
		var falta int64
		if err := testPool.QueryRow(context.Background(),
			`SELECT cart_unpaid_total_cents($1) + COALESCE((SELECT shipping_cost_cents FROM carts WHERE id=$1),0)`,
			c.id).Scan(&falta); err != nil {
			t.Fatalf("lendo o que falta pagar: %v", err)
		}
		c.cobrado = falta
	}
	if _, err := c.q.UpdateCartPayment(context.Background(), sqlc.UpdateCartPaymentParams{
		CartID:        pgUUID(t, c.id),
		PaymentStatus: pgtype.Text{String: "paid", Valid: true},
		CheckoutID:    idDoPagamento,
		PaidAt:        pgtype.Timestamptz{Time: agora, Valid: true},
		PaymentMethod: pgtype.Text{String: "pix", Valid: true},
		AmountCents:   c.cobrado,
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

// ─── Desconto de cupom e de PIX ─────────────────────────────────────────────

func (c carrinhoDeTeste) extrato(t *testing.T) (entrou, bruto int64, cobrancas int) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount_cents),0)::bigint, COALESCE(SUM(gross_covered_cents),0)::bigint, COUNT(*)
		 FROM cart_payments WHERE cart_id = $1`, c.id).Scan(&entrou, &bruto, &cobrancas); err != nil {
		t.Fatalf("lendo o extrato: %v", err)
	}
	return
}

// PIX com desconto: entra menos que o preço cheio, e a diferença é abatimento,
// não dívida. O livro guarda as DUAS observações para que dê para saber qual é
// qual depois.
func TestCobrancaComDescontoGuardaOQueEntrouEOQueCobriu(t *testing.T) {
	requireDB(t)
	c := semearCarrinho(t)
	c.somar(t, c.produtoA, 5, 2000) // preço cheio: R$ 100
	c.cobrado = 9500                // PIX com 5% de desconto
	c.pagar(t, "pay-pix")

	entrou, bruto, n := c.extrato(t)
	if n != 1 {
		t.Fatalf("cobranças = %d, quero 1", n)
	}
	if entrou != 9500 {
		t.Errorf("entrou = %d, quero 9500 — é o que o gateway cobrou", entrou)
	}
	if bruto != 10000 {
		t.Errorf("bruto coberto = %d, quero 10000 — é o preço cheio das unidades "+
			"que essa cobrança liquidou", bruto)
	}
	if _, _, falta := c.dinheiro(t); falta != 0 {
		t.Errorf("falta pagar %d, quero 0 — os R$ 5 são desconto, não dívida", falta)
	}
}

// Duas cobranças, cada uma cobrindo o que faltava no seu momento.
func TestDuasCobrancasSaoDuasLinhasNoExtrato(t *testing.T) {
	requireDB(t)
	c := semearCarrinho(t)
	c.somar(t, c.produtoA, 2, 2000) // R$ 40
	c.cobrado = -1
	c.pagar(t, "pay-1")

	c.somar(t, c.produtoB, 3, 500) // + R$ 15
	c.cobrado = -1
	c.pagar(t, "pay-2")

	entrou, bruto, n := c.extrato(t)
	if n != 2 {
		t.Fatalf("cobranças = %d, quero 2 — uma por vez que o dinheiro entrou", n)
	}
	if entrou != 5500 || bruto != 5500 {
		t.Errorf("entrou=%d bruto=%d, quero 5500/5500", entrou, bruto)
	}
	var segunda int64
	if err := testPool.QueryRow(context.Background(),
		`SELECT amount_cents FROM cart_payments WHERE cart_id=$1 AND checkout_id='pay-2'`,
		c.id).Scan(&segunda); err != nil {
		t.Fatalf("lendo a segunda cobrança: %v", err)
	}
	if segunda != 1500 {
		t.Errorf("a segunda cobrança = %d, quero 1500 — ela cobre só o que "+
			"faltava, não o pedido inteiro de novo", segunda)
	}
}

// Reentrega do webhook não vira segunda linha nem soma de novo.
func TestReentregaNaoDuplicaNoExtrato(t *testing.T) {
	requireDB(t)
	c := semearCarrinho(t)
	c.somar(t, c.produtoA, 2, 2000)
	c.cobrado = 4000
	c.pagar(t, "pay-1")
	c.cobrado = 4000
	c.pagar(t, "pay-1")
	c.cobrado = 4000
	c.pagar(t, "pay-1")

	entrou, _, n := c.extrato(t)
	if n != 1 {
		t.Errorf("cobranças = %d, quero 1 — o gateway reentrega o mesmo aviso "+
			"até dez vezes, e reentrega não é pagamento novo", n)
	}
	if entrou != 4000 {
		t.Errorf("entrou = %d, quero 4000", entrou)
	}
	if pago, _, _ := c.dinheiro(t); pago != 4000 {
		t.Errorf("paid_amount_cents = %d, quero 4000 — a coluna acumulada tem de "+
			"seguir o extrato, não contar o eco", pago)
	}
}

// Cupom: mesmo mecanismo, mesma conta.
func TestCupomTambemNaoViraDivida(t *testing.T) {
	requireDB(t)
	c := semearCarrinho(t)
	c.somar(t, c.produtoA, 3, 2000) // R$ 60
	if _, err := testPool.Exec(context.Background(),
		`UPDATE carts SET coupon_code='OFF10', coupon_discount_cents=1000 WHERE id=$1`, c.id); err != nil {
		t.Fatalf("aplicando cupom: %v", err)
	}
	c.cobrado = 5000 // R$ 60 - R$ 10 de cupom
	c.pagar(t, "pay-cupom")

	entrou, bruto, _ := c.extrato(t)
	if bruto-entrou != 1000 {
		t.Errorf("desconto = %d, quero 1000 — é o cupom", bruto-entrou)
	}
	if _, _, falta := c.dinheiro(t); falta != 0 {
		t.Errorf("falta pagar %d, quero 0 — o cupom não é dívida", falta)
	}
}
