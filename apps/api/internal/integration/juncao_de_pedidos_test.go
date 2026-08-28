package integration

// O mapa de casos da junção de pedidos.
//
// Junta-se NO ERP: um pedido só lá, com o conteúdo dos dois. No LiveCart os
// carrinhos continuam separados, cada um com o seu histórico e o seu pagamento.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func carrinho(id string, opts ...func(*CartForJoin)) CartForJoin {
	c := CartForJoin{
		CartID:         id,
		PlatformUserID: "u-maria",
		PlatformHandle: "@maria",
		CreatedAt:      time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC),
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func pago(c *CartForJoin)      { c.Paid = true }
func estornado(c *CartForJoin) { c.Refunded = true }
func morto(c *CartForJoin)     { c.Terminated = true }
func faturado(c *CartForJoin)  { c.Invoiced = true }
func jaJunto(c *CartForJoin)   { c.AlreadyJoined = true }
func outroComprador(c *CartForJoin) {
	c.PlatformUserID, c.PlatformHandle = "u-joana", "@joana"
}
func em(t time.Time) func(*CartForJoin) {
	return func(c *CartForJoin) { c.CreatedAt = t }
}

// ─── Quem pode ser juntado ──────────────────────────────────────────────────

func TestMapaDeCasosDaJuncao(t *testing.T) {
	agora := time.Now()
	casos := []struct {
		nome      string
		a, b      CartForJoin
		confirmou bool
		aceita    bool
		porque    string
	}{
		{"dois abertos, mesmo comprador", carrinho("a"), carrinho("b"), false, true,
			"é o caso normal"},
		{"um pago e um aberto", carrinho("a", pago), carrinho("b"), false, true,
			"o pago vira anfitrião; o aberto entra nele"},
		{"os dois pagos", carrinho("a", pago), carrinho("b", pago), false, true,
			"duas compras pagas no mesmo frete — o extrato mostra as duas"},
		{"um faturado", carrinho("a", faturado), carrinho("b"), false, false,
			"a nota está emitida e o pedido não recebe mais item"},
		{"o outro faturado", carrinho("a"), carrinho("b", faturado), false, false,
			"a ordem dos argumentos não pode mudar a regra"},
		{"um cancelado", carrinho("a", morto), carrinho("b"), false, false,
			"não há venda a juntar"},
		{"um estornado", carrinho("a", estornado), carrinho("b"), false, false,
			"o dinheiro já voltou"},
		{"já em outra junção", carrinho("a", jaJunto), carrinho("b"), false, false,
			"cadeia de dois níveis faria a resolução do anfitrião depender de saltos"},
		{"compradores diferentes, sem confirmar", carrinho("a"), carrinho("b", outroComprador), false, false,
			"juntar a compra de duas pessoas manda uma para o frete da outra"},
		{"compradores diferentes, confirmado", carrinho("a"), carrinho("b", outroComprador), true, true,
			"só o lojista sabe que os dois @ são a mesma pessoa"},
		{"pago e faturado", carrinho("a", pago), carrinho("b", faturado), false, false,
			"a nota vence o pagamento — ela já saiu"},
		{"criados no mesmo instante", carrinho("a", em(agora)), carrinho("b", em(agora)), false, true,
			"empate não pode travar a junção"},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			err := podemSerJuntados(c.a, c.b, c.confirmou)
			if c.aceita && err != nil {
				t.Errorf("recusou (%v) — devia aceitar: %s", err, c.porque)
			}
			if !c.aceita && err == nil {
				t.Errorf("aceitou — devia recusar: %s", c.porque)
			}
		})
	}
}

// ─── Quem vira anfitrião ────────────────────────────────────────────────────
//
// O anfitrião fica com o pedido no ERP. Escolher errado significa cancelar um
// pedido pago para reabrir a venda no outro.

func TestQuemViraAnfitriao(t *testing.T) {
	cedo := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	tarde := time.Date(2026, 8, 22, 18, 0, 0, 0, time.UTC)

	casos := []struct {
		nome      string
		a, b      CartForJoin
		queroHost string
		porque    string
	}{
		{"o pago vence o mais recente",
			carrinho("pago-antigo", pago, em(cedo)), carrinho("aberto-novo", em(tarde)),
			"pago-antigo", "cancelar o pedido pago desfaria um pagamento aceito"},
		{"o pago vence mesmo sendo o mais novo",
			carrinho("aberto-antigo", em(cedo)), carrinho("pago-novo", pago, em(tarde)),
			"pago-novo", "a idade não pode passar por cima do pagamento"},
		{"nenhum pago: o mais antigo",
			carrinho("antigo", em(cedo)), carrinho("novo", em(tarde)),
			"antigo", "é o número que a compradora já conhece"},
		{"os dois pagos: o mais antigo",
			carrinho("antigo", pago, em(cedo)), carrinho("novo", pago, em(tarde)),
			"antigo", "mesma regra de desempate"},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			host, juntado := escolherAnfitriao(c.a, c.b)
			if host.CartID != c.queroHost {
				t.Errorf("anfitrião = %s, quero %s — %s", host.CartID, c.queroHost, c.porque)
			}
			if juntado.CartID == host.CartID {
				t.Error("o mesmo carrinho virou anfitrião e juntado")
			}
		})
	}
}

// A escolha não pode depender da ordem em que os dois chegam — o painel manda
// os dois ids e não deve precisar saber qual pôr primeiro.
func TestAOrdemDosArgumentosNaoMudaOAnfitriao(t *testing.T) {
	cedo := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	tarde := time.Date(2026, 8, 22, 18, 0, 0, 0, time.UTC)
	a := carrinho("um", pago, em(tarde))
	b := carrinho("dois", em(cedo))

	h1, j1 := escolherAnfitriao(a, b)
	h2, j2 := escolherAnfitriao(b, a)
	if h1.CartID != h2.CartID || j1.CartID != j2.CartID {
		t.Errorf("inverter os argumentos mudou o resultado: %s/%s vs %s/%s",
			h1.CartID, j1.CartID, h2.CartID, j2.CartID)
	}
}

// ─── Contra o Postgres: a grade e o extrato passam a ser do GRUPO ───────────

func semearParaJuntar(t *testing.T) (host, juntado, produtoA, produtoB, storeID string) {
	t.Helper()
	ctx := contextoDeTeste()
	n := timestampUnico()
	must := func(dst *string, sql string, args ...any) {
		t.Helper()
		if err := testPool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("semeando: %v", err)
		}
	}
	// DOIS eventos, e não é detalhe do teste: o índice
	// carts_one_open_per_event_buyer impede dois carrinhos abertos do mesmo
	// comprador no MESMO evento. Ou seja, a junção é sempre entre campanhas
	// diferentes — que é exatamente o caso real, "ela já tinha um pedido de
	// antes e agora comentou na live".
	var eventoA, eventoB string
	must(&storeID, `INSERT INTO stores (name, slug) VALUES ('Loja Junta','junta-'||$1) RETURNING id::text`, n)
	must(&eventoA, `INSERT INTO live_events (store_id, status, title, ends_at)
		VALUES ($1,'active','Compra de antes',now()+interval '7 days') RETURNING id::text`, storeID)
	must(&eventoB, `INSERT INTO live_events (store_id, status, title, ends_at)
		VALUES ($1,'active','Live de hoje',now()+interval '7 days') RETURNING id::text`, storeID)
	must(&produtoA, `INSERT INTO products (store_id,name,keyword,external_source,external_id,price,stock)
		VALUES ($1,'A','junA','tiny','EXT-A-'||$2,2000,10) RETURNING id::text`, storeID, n)
	must(&produtoB, `INSERT INTO products (store_id,name,keyword,external_source,external_id,price,stock)
		VALUES ($1,'B','junB','tiny','EXT-B-'||$2,1500,10) RETURNING id::text`, storeID, n)

	novoCarrinho := func(dst *string, eventID, sufixo string) {
		must(dst, `INSERT INTO carts (event_id,store_id,platform_user_id,platform_handle,token,short_id,status,external_order_id,erp_order_state)
			VALUES ($1,$2,'u-maria','@maria','tok-'||$3,($4)::bigint % 100000,'checkout','TINY-'||$3,'open') RETURNING id::text`,
			eventID, storeID, n+sufixo, n)
	}
	novoCarrinho(&host, eventoA, "h")
	novoCarrinho(&juntado, eventoB, "j")
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id,product_id,quantity,unit_price) VALUES ($1,$2,2,2000),($3,$4,3,1500)`,
		host, produtoA, juntado, produtoB); err != nil {
		t.Fatalf("semeando itens: %v", err)
	}
	return
}

// A grade que sobe para o ERP passa a ser a soma dos dois carrinhos.
func TestGradeDoERPSomaOsCarrinhosJuntados(t *testing.T) {
	requireDB(t)
	host, juntado, _, _, _ := semearParaJuntar(t)
	ctx := contextoDeTeste()

	antes, err := testRepo.ListCartGridItems(ctx, host)
	if err != nil {
		t.Fatalf("grade antes: %v", err)
	}
	if len(antes) != 1 {
		t.Fatalf("grade antes tem %d linhas, quero 1", len(antes))
	}

	ok, err := testRepo.JoinCartIntoHost(ctx, juntado, host)
	if err != nil || !ok {
		t.Fatalf("juntando: ok=%v err=%v", ok, err)
	}

	depois, err := testRepo.ListCartGridItems(ctx, host)
	if err != nil {
		t.Fatalf("grade depois: %v", err)
	}
	if len(depois) != 2 {
		t.Errorf("grade do anfitrião tem %d linhas, quero 2 — o pedido no ERP é "+
			"um só e carrega o conteúdo dos dois", len(depois))
	}
	// E a do carrinho juntado fica VAZIA: ele não tem pedido próprio.
	doJuntado, err := testRepo.ListCartGridItems(ctx, juntado)
	if err != nil {
		t.Fatalf("grade do juntado: %v", err)
	}
	if len(doJuntado) != 0 {
		t.Errorf("o carrinho juntado ainda monta grade (%d linhas) — ele criaria "+
			"um segundo pedido para o mesmo conteúdo", len(doJuntado))
	}
}

// A trava de escrita resolve para o anfitrião. Sem isto os dois carrinhos
// tomariam travas diferentes para o MESMO pedido e escreveriam a grade ao mesmo
// tempo — que é como se corrompe um pedido cuja escrita substitui tudo.
func TestATravaResolveParaOAnfitriao(t *testing.T) {
	requireDB(t)
	host, juntado, _, _, _ := semearParaJuntar(t)
	ctx := contextoDeTeste()
	if ok, err := testRepo.JoinCartIntoHost(ctx, juntado, host); err != nil || !ok {
		t.Fatalf("juntando: %v", err)
	}

	// Quem tomar a trava pelo carrinho JUNTADO tem de travar a linha do
	// ANFITRIÃO — então a segunda tentativa, pelo anfitrião, falha.
	ganhou1, err := testRepo.TransitionCartERPOrderState(ctx, juntado, "open", "mutating")
	if err != nil {
		t.Fatalf("1ª trava: %v", err)
	}
	ganhou2, err := testRepo.TransitionCartERPOrderState(ctx, host, "open", "mutating")
	if err != nil {
		t.Fatalf("2ª trava: %v", err)
	}
	if !ganhou1 {
		t.Fatal("a trava pelo carrinho juntado não pegou")
	}
	if ganhou2 {
		t.Error("os dois pegaram a trava — são duas travas para o mesmo pedido, " +
			"e a grade pode ser escrita em paralelo")
	}

	// E o estado lido pelo juntado é o do anfitrião.
	st, err := testRepo.GetCartERPOrderState(ctx, juntado)
	if err != nil {
		t.Fatalf("lendo estado: %v", err)
	}
	if st.State != "mutating" {
		t.Errorf("estado lido pelo juntado = %q, quero mutating (o do anfitrião)", st.State)
	}
}

// O extrato de cobranças do pedido soma os dois carrinhos: cada um pode ser pago
// pelo seu link, e o pedido no ERP tem de mostrar todo o dinheiro que entrou.
func TestExtratoSomaAsCobrancasDosDoisCarrinhos(t *testing.T) {
	requireDB(t)
	host, juntado, _, _, _ := semearParaJuntar(t)
	ctx := contextoDeTeste()
	if ok, err := testRepo.JoinCartIntoHost(ctx, juntado, host); err != nil || !ok {
		t.Fatalf("juntando: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_payments (cart_id,amount_cents,gross_covered_cents,method,checkout_id)
		 VALUES ($1,4000,4000,'pix','pay-host'),($2,4500,4500,'pix','pay-juntado')`,
		host, juntado); err != nil {
		t.Fatalf("semeando cobranças: %v", err)
	}

	extrato, err := testRepo.ListCartPayments(ctx, host)
	if err != nil {
		t.Fatalf("lendo extrato: %v", err)
	}
	if len(extrato) != 2 {
		t.Fatalf("extrato tem %d cobranças, quero 2 — ler só um carrinho faria a "+
			"compra do outro aparecer como 'a pagar', cobrando de novo o que já "+
			"foi pago", len(extrato))
	}
	var total int64
	for _, p := range extrato {
		total += p.AmountCents
	}
	if total != 8500 {
		t.Errorf("total do extrato = %d, quero 8500", total)
	}
}

// Cadeia de dois níveis é recusada: a resolução do anfitrião passaria a depender
// de quantos saltos existem, e um ciclo a travaria para sempre.
func TestNaoAceitaCadeiaDeJuncao(t *testing.T) {
	requireDB(t)
	host, juntado, _, _, _ := semearParaJuntar(t)
	ctx := contextoDeTeste()
	if ok, _ := testRepo.JoinCartIntoHost(ctx, juntado, host); !ok {
		t.Fatal("1ª junção falhou")
	}
	// Tentar pendurar o anfitrião no carrinho que já está juntado.
	ok, err := testRepo.JoinCartIntoHost(ctx, host, juntado)
	if err != nil {
		t.Fatalf("2ª junção: %v", err)
	}
	if ok {
		t.Error("aceitou uma cadeia — a resolução do anfitrião viraria uma " +
			"caminhada de tamanho desconhecido, e um ciclo a travaria")
	}
}

func contextoDeTeste() context.Context { return context.Background() }

func timestampUnico() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }
