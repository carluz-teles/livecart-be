package order_test

// O detalhe do pedido não mostrava NADA para carrinho ainda não pago.
//
// A Fatia B1 mudou a fonte dos itens do detalhe para `order_items` — o snapshot
// imutável da venda. Mas a linha em `orders` só nasce no PAGAMENTO
// (OnCartPaid), e `GetItems` fazia `order_items JOIN orders ON o.cart_id = $1`.
// Para carrinho em aberto o JOIN não casa nada: o lojista abria "Aguardando
// pagamento", clicava no pedido e via "Itens (0)" com total R$ 0,00.
//
// Em campo isso apareceu como impossibilidade de passar orçamento: a lojista
// precisa abrir e imprimir o carrinho ANTES do pagamento — é o documento que ela
// manda para a cliente decidir. A lista já mostrava a linha desde o fix da G1;
// o que faltava era o conteúdo dela.
//
// Invariantes travadas aqui:
//   Q1 SEM Order: os itens do detalhe vêm do CART (e o total fecha com a
//      função canônica cart_product_total_cents, a mesma que a lista usa)
//   Q2 A parcela em FILA é distinguível item a item — sem isso o orçamento
//      cobra por unidade que a loja não tem para entregar
//   Q3 COM Order: nada muda. O fallback não pode virar porta dos fundos para o
//      cart voltar a ser lido depois da venda selada (invariante B1b)
//   Q4 A fila do carrinho aparece no detalhe — é o que o lojista precisa dizer
//      à cliente que está reservado mas não dá para pagar ainda

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/order"
)

// addItemComFila insere um cart_item com parte da quantidade em fila de espera.
// `qty` é o TOTAL pedido; `waitlisted` é a parcela SEM estoque. O que a cliente
// pode pagar agora é `qty - waitlisted` (mesma conta do checkout público).
func addItemComFila(t *testing.T, cartID, productID string, qty, waitlisted int, price int64) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, $3, $4, $5)`, cartID, productID, qty, price, waitlisted,
	); err != nil {
		t.Fatalf("addItemComFila: %v", err)
	}
}

func enfileirar(t *testing.T, eventID, cartID, productID, handle string, qty, position int, status string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO waitlist_items (event_id, product_id, platform_user_id, platform_handle,
		   quantity, position, status, cart_id)
		 VALUES ($1, $2, 'u-fila', $3, $4, $5, $6, $7)`,
		eventID, productID, handle, qty, position, status, cartID,
	); err != nil {
		t.Fatalf("enfileirar: %v", err)
	}
}

// ─── Q1 + Q2 ────────────────────────────────────────────────────────────────

func TestOrcamento_CarrinhoSemPagamentoMostraOsItens(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := order.NewService(order.NewRepository(testPool), zap.NewNop())

	storeID, eventID := seedIsolatedStore(t, "orc1")
	vestido := seedProduct(t, storeID, 8990)
	bolsa := seedProduct(t, storeID, 12500)

	cartID := insertCart(t, eventID, "@cliente", "tok-orc1", 88101, "pending", nil)
	addItem(t, cartID, vestido, 2, 8990)
	addItemComFila(t, cartID, bolsa, 3, 1, 12500) // 2 disponíveis, 1 em fila

	detail, err := svc.GetDetailByID(ctx, cartID, storeID)
	if err != nil {
		t.Fatalf("GetDetailByID: %v", err)
	}

	if len(detail.Items) == 0 {
		t.Fatal("Q1: o detalhe do carrinho em aberto veio SEM itens — é a tela " +
			"em branco que impedia a lojista de passar orçamento")
	}
	if len(detail.Items) != 2 {
		t.Fatalf("Q1: esperava 2 itens, veio %d", len(detail.Items))
	}

	// O total tem de fechar com a MESMA função que alimenta a lista de pedidos.
	// Divergir aqui faz a linha da lista e o detalhe mostrarem valores
	// diferentes para o mesmo carrinho.
	var canonico int64
	if err := testPool.QueryRow(ctx,
		`SELECT cart_product_total_cents($1)`, cartID,
	).Scan(&canonico); err != nil {
		t.Fatalf("total canônico: %v", err)
	}
	if detail.TotalAmount != canonico {
		t.Errorf("Q1: TotalAmount=%d != cart_product_total_cents=%d — detalhe e "+
			"lista mostrariam números diferentes para o mesmo carrinho",
			detail.TotalAmount, canonico)
	}

	// ─── Q2: a parcela em fila é distinguível ────────────────────────────────

	porProduto := make(map[string]order.OrderItemOutput, len(detail.Items))
	for _, item := range detail.Items {
		porProduto[item.ProductID] = item
	}

	if got := porProduto[vestido].WaitlistedQuantity; got != 0 {
		t.Errorf("Q2: vestido tem estoque e veio com %d em fila", got)
	}
	if got := porProduto[bolsa].WaitlistedQuantity; got != 1 {
		t.Errorf("Q2: bolsa deveria ter 1 unidade em fila, veio %d — sem esse "+
			"número o orçamento cobra por unidade que a loja não tem", got)
	}
	if got := porProduto[bolsa].Quantity; got != 3 {
		t.Errorf("Q2: Quantity da bolsa = %d; deve ser o TOTAL pedido (3), com a "+
			"fila destacada em WaitlistedQuantity", got)
	}

	// Nome e preço têm de vir preenchidos: um orçamento com produto sem nome
	// não serve para mandar para a cliente.
	if porProduto[vestido].ProductName == "" {
		t.Error("Q1: item veio sem nome do produto")
	}
	if porProduto[vestido].UnitPrice == 0 {
		t.Error("Q1: item veio sem preço unitário")
	}
}

// ─── Q5 — o valor do orçamento ──────────────────────────────────────────────

// O documento que a cliente recebe não pode cobrar por unidade que a loja não
// tem para entregar. PayableAmount é o que ela consegue pagar agora;
// WaitlistedAmount é o que fica declarado como fora do total. As duas somas
// fecham em TotalAmount — que continua sendo a quantidade cheia, porque é dele
// que sai a receita do evento e o valor mostrado na lista.
func TestOrcamento_TotalCobraSoOQueTemEstoque(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := order.NewService(order.NewRepository(testPool), zap.NewNop())

	storeID, eventID := seedIsolatedStore(t, "orc5")
	comEstoque := seedProduct(t, storeID, 1000)
	parcial := seedProduct(t, storeID, 2000)
	semEstoque := seedProduct(t, storeID, 5000)

	cartID := insertCart(t, eventID, "@valor", "tok-orc5", 88501, "pending", nil)
	addItem(t, cartID, comEstoque, 3, 1000)           // 3 000 pagáveis
	addItemComFila(t, cartID, parcial, 4, 1, 2000)    // 6 000 pagáveis, 2 000 em fila
	addItemComFila(t, cartID, semEstoque, 2, 2, 5000) // 0 pagáveis, 10 000 em fila

	const (
		querPagavel    int64 = 3*1000 + 3*2000
		querEmFila     int64 = 1*2000 + 2*5000
		querTotalCheio       = querPagavel + querEmFila
	)

	detail, err := svc.GetDetailByID(ctx, cartID, storeID)
	if err != nil {
		t.Fatalf("GetDetailByID: %v", err)
	}

	if detail.PayableAmount != querPagavel {
		t.Errorf("Q5: PayableAmount=%d, esperava %d — o orçamento cobraria por "+
			"unidade que a loja não tem", detail.PayableAmount, querPagavel)
	}
	if detail.WaitlistedAmount != querEmFila {
		t.Errorf("Q5: WaitlistedAmount=%d, esperava %d", detail.WaitlistedAmount, querEmFila)
	}
	if detail.TotalAmount != querTotalCheio {
		t.Errorf("Q5: TotalAmount=%d, esperava %d (quantidade cheia — é a base da "+
			"receita do evento e do valor da lista)", detail.TotalAmount, querTotalCheio)
	}
	// A identidade é o que impede as três somas de divergirem silenciosamente.
	if detail.PayableAmount+detail.WaitlistedAmount != detail.TotalAmount {
		t.Errorf("Q5: %d + %d != %d — sobrou ou faltou unidade em algum lado",
			detail.PayableAmount, detail.WaitlistedAmount, detail.TotalAmount)
	}
}

// Sem nada em fila — o caso da esmagadora maioria dos pedidos — pagável e total
// têm de ser o MESMO número. Divergir aqui faria todo orçamento normal mostrar
// um total diferente do que a lista e o card de pagamento mostram.
func TestOrcamento_SemFilaPagavelIgualAoTotal(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := order.NewService(order.NewRepository(testPool), zap.NewNop())

	storeID, eventID := seedIsolatedStore(t, "orc6")
	prod := seedProduct(t, storeID, 7350)

	cartID := insertCart(t, eventID, "@normal", "tok-orc6", 88601, "pending", nil)
	addItem(t, cartID, prod, 2, 7350)

	detail, err := svc.GetDetailByID(ctx, cartID, storeID)
	if err != nil {
		t.Fatalf("GetDetailByID: %v", err)
	}

	if detail.PayableAmount != detail.TotalAmount {
		t.Errorf("PayableAmount=%d != TotalAmount=%d sem nada em fila",
			detail.PayableAmount, detail.TotalAmount)
	}
	if detail.WaitlistedAmount != 0 {
		t.Errorf("WaitlistedAmount=%d sem nada em fila", detail.WaitlistedAmount)
	}
}

// ─── Q3 — o fallback não afrouxa o cutover ──────────────────────────────────

// Depois da venda selada, o detalhe lê o SNAPSHOT. Se o fallback disparasse por
// "não achei itens" em vez de "não existe Order", uma Order sem itens (estado de
// bug) passaria a mostrar o cart vivo e mascararia o defeito — e pior, mutação
// no cart voltaria a mexer no que já foi vendido.
func TestOrcamento_ComVendaSeladaODetalheContinuaLendoOSnapshot(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := order.NewService(order.NewRepository(testPool), zap.NewNop())
	l := newListener(t)

	storeID, eventID := seedIsolatedStore(t, "orc2")
	prod := seedProduct(t, storeID, 5000)

	agora := time.Now()
	cartID := insertCart(t, eventID, "@paga", "tok-orc2", 88201, "paid", &agora)
	addItem(t, cartID, prod, 2, 5000)
	if err := l.OnCartPaid(ctx, cartID, storeID, 10000, nil); err != nil {
		t.Fatalf("OnCartPaid: %v", err)
	}

	antes, err := svc.GetDetailByID(ctx, cartID, storeID)
	if err != nil {
		t.Fatalf("GetDetailByID antes: %v", err)
	}
	if len(antes.Items) != 1 || antes.Items[0].Quantity != 2 {
		t.Fatalf("baseline inesperada: %d itens", len(antes.Items))
	}

	// Mutação no cart DEPOIS da venda: quantidade dobrada e um item novo.
	if _, err := testPool.Exec(ctx,
		`UPDATE cart_items SET quantity = 99 WHERE cart_id = $1`, cartID,
	); err != nil {
		t.Fatalf("mutando o cart: %v", err)
	}
	outro := seedProduct(t, storeID, 700)
	addItem(t, cartID, outro, 5, 700)

	depois, err := svc.GetDetailByID(ctx, cartID, storeID)
	if err != nil {
		t.Fatalf("GetDetailByID depois: %v", err)
	}

	if len(depois.Items) != 1 {
		t.Errorf("Q3: o detalhe passou a ver %d itens após mutar o cart — o "+
			"fallback vazou para pedido já materializado (quebra B1b)", len(depois.Items))
	}
	if depois.Items[0].Quantity != 2 {
		t.Errorf("Q3: quantidade do snapshot mudou para %d ao mutar o cart",
			depois.Items[0].Quantity)
	}
	if depois.TotalAmount != antes.TotalAmount {
		t.Errorf("Q3: total congelado mudou de %d para %d",
			antes.TotalAmount, depois.TotalAmount)
	}
}

// ─── Q4 — a fila aparece no detalhe ─────────────────────────────────────────

func TestOrcamento_FilaDeEsperaAparaceNoDetalhe(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := order.NewService(order.NewRepository(testPool), zap.NewNop())

	storeID, eventID := seedIsolatedStore(t, "orc3")
	disponivel := seedProduct(t, storeID, 4000)
	esgotado := seedProduct(t, storeID, 9900)

	cartID := insertCart(t, eventID, "@fila", "tok-orc3", 88301, "pending", nil)
	addItem(t, cartID, disponivel, 1, 4000)
	addItemComFila(t, cartID, esgotado, 2, 2, 9900) // tudo em fila
	enfileirar(t, eventID, cartID, esgotado, "@fila", 2, 1, "waiting")

	detail, err := svc.GetDetailByID(ctx, cartID, storeID)
	if err != nil {
		t.Fatalf("GetDetailByID: %v", err)
	}

	if len(detail.Waitlist) == 0 {
		t.Fatal("Q4: o detalhe não expôs a fila de espera — a cliente está " +
			"esperando um produto e o lojista não tem onde ver")
	}
	if len(detail.Waitlist) != 1 {
		t.Fatalf("Q4: esperava 1 entrada na fila, veio %d", len(detail.Waitlist))
	}

	fila := detail.Waitlist[0]
	if fila.ProductID != esgotado {
		t.Errorf("Q4: a fila apontou para o produto errado (%s)", fila.ProductID)
	}
	if fila.ProductName == "" {
		t.Error("Q4: entrada da fila sem nome do produto")
	}
	if fila.Quantity != 2 {
		t.Errorf("Q4: quantidade em fila = %d, esperava 2", fila.Quantity)
	}
	if fila.Status != "waiting" {
		t.Errorf("Q4: status = %q, esperava waiting", fila.Status)
	}
	if fila.Position != 1 {
		t.Errorf("Q4: posição = %d, esperava 1", fila.Position)
	}
}

// Fila encerrada não é fila: entrada cancelada/atendida/expirada não pode
// reaparecer no orçamento como se a cliente ainda estivesse esperando.
func TestOrcamento_FilaEncerradaNaoAparece(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := order.NewService(order.NewRepository(testPool), zap.NewNop())

	storeID, eventID := seedIsolatedStore(t, "orc4")
	prod := seedProduct(t, storeID, 4000)

	cartID := insertCart(t, eventID, "@desistiu", "tok-orc4", 88401, "pending", nil)
	addItem(t, cartID, prod, 1, 4000)

	for i, status := range []string{"cancelled", "fulfilled", "expired"} {
		p := seedProduct(t, storeID, 1000)
		enfileirar(t, eventID, cartID, p, "@desistiu", 1, i+1, status)
	}

	detail, err := svc.GetDetailByID(ctx, cartID, storeID)
	if err != nil {
		t.Fatalf("GetDetailByID: %v", err)
	}

	if len(detail.Waitlist) != 0 {
		t.Errorf("Q4: %d entrada(s) encerrada(s) apareceram como fila ativa",
			len(detail.Waitlist))
	}
}
