package integration

// O fluxo do pedido contra um Postgres de verdade.
//
// Os testes unitários em internal/erp provam as regras contra simuladores; estes
// provam que a máquina de estados sobrevive ao banco real — CAS concorrente,
// advisory lock, colunas que precisam existir. Cada cenário derruba o fluxo num
// passo e exige que a retomada convirja sem duplicar pedido nem mover estoque.
//
//	docker compose up -d postgres
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	  go test ./apps/api/internal/integration/ -run ERP -v

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

// ─── Criação ────────────────────────────────────────────────────────────────

// O caminho inteiro: comentário → pedido → pagamento → aprovado. Sem uma única
// movimentação de estoque partindo daqui.
func TestERPFluxoCompletoNaoMoveEstoque(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 3, 0)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	ctx := context.Background()

	if err := svc.EnsureERPOrderForCart(ctx, fx.cartID, fx.storeID); err != nil {
		t.Fatalf("criando pedido: %v", err)
	}
	state, orderID, _, launched := cartERPState(t, fx.cartID)
	if state != "open" || orderID == "" {
		t.Fatalf("estado=%q pedido=%q, quero 'open' com pedido gravado", state, orderID)
	}
	if launched {
		t.Error("erp_stock_launched verdadeiro logo após a criação — nada foi lançado")
	}

	if err := svc.ConfirmERPOrderPayment(ctx, fx.cartID, fx.storeID, testPaymentStatus()); err != nil {
		t.Fatalf("confirmando: %v", err)
	}

	state, _, _, _ = cartERPState(t, fx.cartID)
	if state != "confirmed" {
		t.Fatalf("estado pós-pagamento = %q, quero 'confirmed'", state)
	}
	if c := fake.count("Reverse:"); c != 0 {
		t.Errorf("estornos = %d, quero 0: %v", c, fake.calls)
	}
	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Errorf("reservas manuais = %d, quero 0 — o pedido é a reserva", n)
	}
	status, _, _, attempts, hasSnapshot := cartFinalisationState(t, fx.cartID)
	if status != "done" || attempts < 1 || !hasSnapshot {
		t.Errorf("finalização: status=%q attempts=%d snapshot=%v", status, attempts, hasSnapshot)
	}
}

// Criar o pedido duas vezes é uma vez. O CAS none→converting é o portão.
func TestERPCriacaoEhIdempotente(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := svc.EnsureERPOrderForCart(ctx, fx.cartID, fx.storeID); err != nil {
			t.Fatalf("criação %d: %v", i, err)
		}
	}
	if c := fake.count("CreateOrder"); c != 1 {
		t.Errorf("pedidos criados = %d, quero 1", c)
	}
}

// Comentários simultâneos do mesmo carrinho: o CAS do Postgres decide, e só um
// pedido nasce. Este é o cenário da live.
func TestERPComentariosSimultaneosCriamUmPedidoSo(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 5, 0)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = svc.ERP().ReserveStockInERP(ctx, fx.storeID, fx.cartID, fx.eventID, fx.productID, 1, 1000, "@buyer")
		}()
	}
	wg.Wait()

	if c := fake.count("CreateOrder"); c != 1 {
		t.Errorf("pedidos criados = %d, quero 1 — seis comentários do mesmo carrinho "+
			"não podem virar seis pedidos: %v", c, fake.calls)
	}
	if c := fake.count("Reverse:"); c != 0 {
		t.Errorf("estornos = %d, quero 0", c)
	}
	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Errorf("reservas manuais = %d, quero 0", n)
	}
}

// Falha na criação deixa o carrinho em 'converting' e a retomada NÃO cria um
// segundo pedido: acha o primeiro pelo marcador.
func TestERPFalhaNaCriacaoRetomaSemDuplicar(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 2, 0)
	fake := newScriptedERP()
	fake.failures["CreateOrder"] = 1
	svc := newFinalisationService(fake)
	ctx := context.Background()

	if err := svc.EnsureERPOrderForCart(ctx, fx.cartID, fx.storeID); err == nil {
		t.Fatal("a criação devia falhar")
	}
	if state, _, _, _ := cartERPState(t, fx.cartID); state != "converting" {
		t.Fatalf("estado = %q, quero 'converting' — voltar para 'none' reabriria o "+
			"caminho e criaria um segundo pedido", state)
	}

	// O pagamento chega: cria o pedido que faltava e aprova.
	if err := svc.ConfirmERPOrderPayment(ctx, fx.cartID, fx.storeID, testPaymentStatus()); err != nil {
		t.Fatalf("confirmando após falha: %v", err)
	}
	if c := fake.count("CreateOrder"); c != 2 {
		t.Errorf("chamadas a CreateOrder = %d, quero 2 (a que falhou + a que valeu)", c)
	}
	state, orderID, _, _ := cartERPState(t, fx.cartID)
	if state != "confirmed" || orderID == "" {
		t.Errorf("estado=%q pedido=%q, quero 'confirmed' com pedido", state, orderID)
	}
}

// Processo morreu depois do POST e antes de gravar o id: a adoção pelo marcador
// reencontra o pedido em vez de criar outro.
func TestERPAdocaoPeloMarcadorEvitaPedidoDuplicado(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	ctx := context.Background()

	if err := svc.EnsureERPOrderForCart(ctx, fx.cartID, fx.storeID); err != nil {
		t.Fatalf("criando: %v", err)
	}
	_, orderID, _, _ := cartERPState(t, fx.cartID)

	// Encena a morte: apaga o id e volta o estado para 'converting'.
	if _, err := testPool.Exec(ctx,
		`UPDATE carts SET external_order_id = NULL, erp_order_state = 'converting' WHERE id = $1`,
		fx.cartID); err != nil {
		t.Fatalf("encenando a morte do processo: %v", err)
	}

	if err := svc.ConfirmERPOrderPayment(ctx, fx.cartID, fx.storeID, testPaymentStatus()); err != nil {
		t.Fatalf("confirmando: %v", err)
	}
	if c := fake.count("CreateOrder"); c != 1 {
		t.Errorf("pedidos criados = %d, quero 1 — o marcador existe para isto", c)
	}
	_, adotado, _, _ := cartERPState(t, fx.cartID)
	if adotado != orderID {
		t.Errorf("adotou %q, quero %q", adotado, orderID)
	}
}

// ─── Pagamento ──────────────────────────────────────────────────────────────

// Webhooks de gateway chegam duplicados, em goroutines concorrentes. O advisory
// lock por carrinho garante um pedido e uma aprovação.
func TestERPWebhooksConcorrentesUmPedidoUmaAprovacao(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 2, 0)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = svc.ConfirmERPOrderPayment(ctx, fx.cartID, fx.storeID, testPaymentStatus())
		}()
	}
	wg.Wait()

	if c := fake.count("CreateOrder"); c != 1 {
		t.Errorf("pedidos = %d, quero 1: %v", c, fake.calls)
	}
	aprovacoes := 0
	for _, c := range fake.callsWithPrefix("Situacao:") {
		if strings.HasSuffix(c, fmt.Sprintf(":%d", providers.SituacaoAprovada)) {
			aprovacoes++
		}
	}
	if aprovacoes != 1 {
		t.Errorf("aprovações = %d, quero 1: %v", aprovacoes, fake.calls)
	}
	if state, _, _, _ := cartERPState(t, fx.cartID); state != "confirmed" {
		t.Errorf("estado = %q, quero 'confirmed'", state)
	}
}

// A gravação das parcelas falha: o pedido NÃO é aprovado, e o painel mostra
// 'failed' com o retrato do gateway guardado para o reenvio.
func TestERPFalhaNasParcelasNaoAprovaEGuardaOSnapshot(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	fake := newScriptedERP()
	fake.failures["UpdateOrderPayment"] = 1
	svc := newFinalisationService(fake)
	ctx := context.Background()

	if err := svc.ConfirmERPOrderPayment(ctx, fx.cartID, fx.storeID, testPaymentStatus()); err == nil {
		t.Fatal("a confirmação devia falhar")
	}
	aprovacoes := 0
	for _, c := range fake.callsWithPrefix("Situacao:") {
		if strings.HasSuffix(c, fmt.Sprintf(":%d", providers.SituacaoAprovada)) {
			aprovacoes++
		}
	}
	if aprovacoes != 0 {
		t.Errorf("aprovou o pedido sem conseguir gravar as parcelas: %v", fake.calls)
	}
	status, _, _, _, hasSnapshot := cartFinalisationState(t, fx.cartID)
	if status != "failed" {
		t.Errorf("status = %q, quero 'failed'", status)
	}
	if !hasSnapshot {
		t.Error("o retrato do gateway não foi guardado — sem ele o reenvio aprova a " +
			"venda sem o financeiro junto")
	}

	// O reenvio do painel relê o retrato e converge.
	fake.failures["UpdateOrderPayment"] = 0
	if err := svc.RetryERPFinalisation(ctx, fx.cartID, fx.storeID); err != nil {
		t.Fatalf("reenvio: %v", err)
	}
	if state, _, _, _ := cartERPState(t, fx.cartID); state != "confirmed" {
		t.Errorf("estado após reenvio = %q, quero 'confirmed'", state)
	}
	if c := fake.count("Payment:"); c != 2 {
		t.Errorf("gravações de parcela = %d, quero 2 (a que falhou + a do reenvio)", c)
	}
}

// Pagamento MANUAL: o snapshot é nil DE PROPÓSITO — é ele que viraria conta a
// receber no Tiny, e no manual quem lança isso é o lojista. Se a primeira
// finalização travar ANTES de criar o pedido (por exemplo, bloqueada por um
// movimento de estoque em dúvida que depois se resolve), o carrinho fica
// 'failed' sem snapshot E sem pedido.
//
// Esse era o estado que o reenvio recusava com 422 "snapshot de pagamento
// ausente", e o pedido pago ficava preso fora do ERP para sempre. O
// comportamento novo — finalizar com nil — chegou como correção em produção
// (5c3aaa8) num arquivo que esta refatoração apagou; o teste veio junto para
// que a guarda não se perdesse na fusão.
func TestReenvioDePagamentoManualFinalizaSemSnapshot(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	// O estado que o 422 recusava: falhou, sem retrato do gateway e sem pedido.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE order_payments op SET erp_finalisation_status = 'failed', erp_payment_snapshot = NULL
		 FROM orders o WHERE o.id = op.order_id AND o.cart_id = $1`, fx.cartID); err != nil {
		t.Fatalf("semeando o manual travado: %v", err)
	}
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	if err := svc.RetryERPFinalisation(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("reenvio manual (sem snapshot, sem pedido): %v", err)
	}
	status, _, orderID, _, _ := cartFinalisationState(t, fx.cartID)
	if status != "done" || orderID == "" {
		t.Fatalf("status=%q pedido=%q — o manual devia finalizar com nil", status, orderID)
	}
	if n := fake.count("CreateOrder"); n != 1 {
		t.Fatalf("pedidos criados = %d, quero 1 — o reenvio manual precisa criar o pedido", n)
	}
}

// ─── Mutação ────────────────────────────────────────────────────────────────

// Pedido travado por lançamento manual: um estorno destrava, e o PUT repete.
func TestERPPedidoTravadoPorLancamentoManualDestravaComUmEstorno(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 2, 0)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	ctx := context.Background()

	if err := svc.EnsureERPOrderForCart(ctx, fx.cartID, fx.storeID); err != nil {
		t.Fatalf("criando: %v", err)
	}
	// O ERP recusa a PRIMEIRA edição por estoque lançado.
	fake.bloqueiaProximosPuts = 1

	if err := svc.MutateERPOrderItems(ctx, fx.cartID, fx.storeID); err != nil {
		t.Fatalf("mutação: %v", err)
	}
	if c := fake.count("Reverse:"); c != 1 {
		t.Errorf("estornos = %d, quero exatamente 1: %v", c, fake.calls)
	}
	if c := fake.count("PutItens:"); c != 2 {
		t.Errorf("PUTs = %d, quero 2 (o bloqueado + o que valeu)", c)
	}
	if _, _, _, launched := cartERPState(t, fx.cartID); launched {
		t.Error("erp_stock_launched continuou verdadeiro após o estorno")
	}
}

// Erro comum no PUT não autoriza estorno, e devolve o carrinho para 'open' — se
// ele ficasse preso em 'mutating', nenhum comentário seguinte entraria.
func TestERPErroNaMutacaoNaoEstornaEDevolveOCarrinho(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 2, 0)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	ctx := context.Background()

	if err := svc.EnsureERPOrderForCart(ctx, fx.cartID, fx.storeID); err != nil {
		t.Fatalf("criando: %v", err)
	}
	fake.failures["UpdateOrderItems"] = 1
	if err := svc.MutateERPOrderItems(ctx, fx.cartID, fx.storeID); err == nil {
		t.Fatal("a mutação devia falhar")
	}
	if c := fake.count("Reverse:"); c != 0 {
		t.Errorf("estornos = %d, quero 0 — só a recusa por estoque lançado autoriza um", c)
	}
	if state, _, _, _ := cartERPState(t, fx.cartID); state != "open" {
		t.Errorf("estado = %q, quero 'open' — preso em 'mutating' o carrinho nunca "+
			"mais é editável", state)
	}
}

// ─── Rastreamento ───────────────────────────────────────────────────────────

// O trajeto do pedido é gravado uma vez por transição, e a reentrega do mesmo
// aviso não vira linha nova.
func TestERPRastreamentoGravaTrajetoSemDuplicar(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	ctx := context.Background()

	if err := svc.EnsureERPOrderForCart(ctx, fx.cartID, fx.storeID); err != nil {
		t.Fatalf("criando: %v", err)
	}
	_, orderID, _, _ := cartERPState(t, fx.cartID)

	trajeto := []providers.ERPOrderStatus{
		providers.ERPOrderStatusAberto,
		providers.ERPOrderStatusAprovado,
		providers.ERPOrderStatusFaturado,
		providers.ERPOrderStatusEnviado,
		providers.ERPOrderStatusEntregue,
	}
	for _, st := range trajeto {
		for i := 0; i < 3; i++ { // reentregas
			if err := svc.ERP().ObserveOrderStatus(ctx, fx.storeID, orderID, "77", st, "webhook", nil); err != nil {
				t.Fatalf("observando %s: %v", st, err)
			}
		}
	}

	// 'aberto' já foi semeado na criação; observá-lo de novo no-opa, então o
	// histórico tem exatamente uma linha por situação do trajeto.
	hist := erpStatusHistory(t, fx.cartID)
	if len(hist) != len(trajeto) {
		t.Fatalf("histórico = %v (%d linhas), quero %d — reentrega não é transição",
			hist, len(hist), len(trajeto))
	}
	for i, st := range trajeto {
		if hist[i] != string(st) {
			t.Errorf("passagem %d = %q, quero %q", i, hist[i], st)
		}
	}
	_, _, atual, _ := cartERPState(t, fx.cartID)
	if atual != string(providers.ERPOrderStatusEntregue) {
		t.Errorf("situação atual = %q, quero 'entregue'", atual)
	}
	var numero string
	if err := testPool.QueryRow(ctx, `SELECT COALESCE(erp_order_number,'') FROM carts WHERE id=$1`, fx.cartID).Scan(&numero); err != nil {
		t.Fatalf("lendo número do pedido: %v", err)
	}
	if numero != "77" {
		t.Errorf("número do pedido = %q, quero '77' — é como o lojista chama o pedido "+
			"ao telefone", numero)
	}
}

// Entregas simultâneas da mesma situação: o FOR UPDATE serializa e só uma linha
// nasce.
func TestERPRastreamentoSobEntregasSimultaneas(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	ctx := context.Background()

	if err := svc.EnsureERPOrderForCart(ctx, fx.cartID, fx.storeID); err != nil {
		t.Fatalf("criando: %v", err)
	}
	_, orderID, _, _ := cartERPState(t, fx.cartID)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = svc.ERP().ObserveOrderStatus(ctx, fx.storeID, orderID, "77",
				providers.ERPOrderStatusEnviado, "webhook", nil)
		}()
	}
	wg.Wait()

	// O trajeto já começa com a situação semeada na criação ('aberto'), então a
	// asserção é sobre as passagens por 'enviado'.
	enviados := 0
	for _, s := range erpStatusHistory(t, fx.cartID) {
		if s == string(providers.ERPOrderStatusEnviado) {
			enviados++
		}
	}
	if enviados != 1 {
		t.Errorf("oito entregas simultâneas da mesma situação viraram %d linhas, quero 1", enviados)
	}
}

// A varredura pergunta a situação de quem parou e grava a diferença como
// 'sweep' — a fonte é o que denuncia webhook que deixou de chegar.
func TestERPVarreduraDeSituacaoFechaADiferenca(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	ctx := context.Background()

	if err := svc.EnsureERPOrderForCart(ctx, fx.cartID, fx.storeID); err != nil {
		t.Fatalf("criando: %v", err)
	}
	_, orderID, _, _ := cartERPState(t, fx.cartID)
	if err := svc.ERP().ObserveOrderStatus(ctx, fx.storeID, orderID, "77",
		providers.ERPOrderStatusAprovado, "webhook", nil); err != nil {
		t.Fatalf("primeira observação: %v", err)
	}
	// Envelhece a marca para a varredura enxergá-la.
	if _, err := testPool.Exec(ctx,
		`UPDATE carts SET erp_order_status_at = NOW() - INTERVAL '3 days' WHERE id = $1`, fx.cartID); err != nil {
		t.Fatalf("envelhecendo a marca: %v", err)
	}
	// O lojista despachou no ERP e o webhook se perdeu.
	fake.situacoes[orderID] = providers.SituacaoEnviada

	svc.RunERPOrderStatusSweep(ctx, 0, 100)

	_, _, atual, _ := cartERPState(t, fx.cartID)
	if atual != string(providers.ERPOrderStatusEnviado) {
		t.Fatalf("situação = %q, quero 'enviado'", atual)
	}
	var fonte string
	if err := testPool.QueryRow(ctx,
		`SELECT source FROM erp_order_status_events WHERE cart_id = $1 ORDER BY observed_at DESC, id DESC LIMIT 1`,
		fx.cartID).Scan(&fonte); err != nil {
		t.Fatalf("lendo a fonte: %v", err)
	}
	if fonte != "sweep" {
		t.Errorf("fonte = %q, quero 'sweep'", fonte)
	}
}

// Pedido em situação terminal sai da varredura — perguntar de novo só gasta cota.
func TestERPVarreduraIgnoraPedidoTerminal(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	ctx := context.Background()

	if err := svc.EnsureERPOrderForCart(ctx, fx.cartID, fx.storeID); err != nil {
		t.Fatalf("criando: %v", err)
	}
	_, orderID, _, _ := cartERPState(t, fx.cartID)
	if err := svc.ERP().ObserveOrderStatus(ctx, fx.storeID, orderID, "77",
		providers.ERPOrderStatusEntregue, "webhook", nil); err != nil {
		t.Fatalf("observando: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE carts SET erp_order_status_at = NOW() - INTERVAL '30 days' WHERE id = $1`, fx.cartID); err != nil {
		t.Fatalf("envelhecendo: %v", err)
	}

	// A asserção é sobre ESTE pedido: o database de teste é compartilhado pela
	// execução inteira, e a varredura legitimamente enxerga os carrinhos dos
	// outros cenários.
	svc.RunERPOrderStatusSweep(ctx, 0, 100)
	if n := fake.count("GetSituacao:" + orderID); n != 0 {
		t.Errorf("a varredura consultou %d× um pedido já entregue — perguntar de novo "+
			"só gasta cota da conta", n)
	}
}

// Situação de um pedido que não é de nenhum carrinho nosso é guardada como
// histórico solto — e não vira erro, porque erro faria o webhook devolver
// não-200, e vinte desses fazem o ERP apagar a URL.
func TestERPSituacaoDePedidoAlheioViraHistoricoSolto(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	ctx := context.Background()

	if err := svc.ERP().ObserveOrderStatus(ctx, fx.storeID, "PED-DE-OUTRO-CANAL", "1",
		providers.ERPOrderStatusFaturado, "webhook", nil); err != nil {
		t.Fatalf("observação de pedido alheio virou erro: %v", err)
	}
	var n int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM erp_order_status_events WHERE store_id = $1 AND cart_id IS NULL`,
		fx.storeID).Scan(&n); err != nil {
		t.Fatalf("contando: %v", err)
	}
	if n != 1 {
		t.Errorf("linhas soltas = %d, quero 1 — é o sinal vivo de que a entrega de "+
			"webhook está funcionando", n)
	}
}

// ─── O portão não é reabastecido pelo espelho ───────────────────────────────

// Entre o comentário baixar o contador local e o pedido existir no ERP, aquela
// unidade não aparece no `disponivel` de lá. Um espelho que grave o disponível
// cru nesse intervalo devolve a unidade ao portão — e ela é oferecida duas vezes.
//
// Foi assim que 25 admissões saíram de 20 unidades em 26/08. A conta que evita
// isso desconta exatamente o que o ERP ainda não conhece: carrinho vivo, sem
// pedido.
func TestUnidadesPrometidasSemPedidoSaoDescontadasDoEspelho(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 3, 0)
	ctx := context.Background()

	// O carrinho existe com 3 unidades e ainda não tem pedido no ERP.
	prometido, err := testRepo.SumPromisedWithoutERPOrder(ctx, extIDDoProduto(t, fx.productID))
	if err != nil {
		t.Fatalf("somando prometidas: %v", err)
	}
	if prometido != 3 {
		t.Fatalf("prometidas sem pedido = %d, quero 3", prometido)
	}

	// Assim que o pedido existe, o `disponivel` do ERP já o desconta — somar
	// aqui de novo seria descontar duas vezes.
	if _, err := testPool.Exec(ctx,
		`UPDATE carts SET external_order_id = 'ORD-X' WHERE id = $1`, fx.cartID); err != nil {
		t.Fatalf("vinculando pedido: %v", err)
	}
	prometido, err = testRepo.SumPromisedWithoutERPOrder(ctx, extIDDoProduto(t, fx.productID))
	if err != nil {
		t.Fatalf("somando prometidas: %v", err)
	}
	if prometido != 0 {
		t.Errorf("prometidas = %d depois de o pedido existir, quero 0 — o disponível "+
			"do ERP já desconta essas unidades, e somá-las de novo tiraria o dobro",
			prometido)
	}
}

// Carrinho morto não segura nada.
func TestCarrinhoCanceladoNaoContaComoPrometido(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 4, 0)
	ctx := context.Background()
	ext := extIDDoProduto(t, fx.productID)

	if _, err := testPool.Exec(ctx, `UPDATE carts SET status='cancelled' WHERE id=$1`, fx.cartID); err != nil {
		t.Fatalf("cancelando: %v", err)
	}
	prometido, err := testRepo.SumPromisedWithoutERPOrder(ctx, ext)
	if err != nil {
		t.Fatalf("somando: %v", err)
	}
	if prometido != 0 {
		t.Errorf("carrinho cancelado contou %d unidades como prometidas", prometido)
	}
}

// Unidade em FILA de espera não foi prometida a ninguém.
func TestUnidadeEmFilaNaoContaComoPrometida(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 5, 0)
	ctx := context.Background()
	ext := extIDDoProduto(t, fx.productID)

	// Das 5, três estão em fila.
	if _, err := testPool.Exec(ctx,
		`UPDATE cart_items SET waitlisted_quantity = 3 WHERE cart_id = $1`, fx.cartID); err != nil {
		t.Fatalf("marcando fila: %v", err)
	}
	prometido, err := testRepo.SumPromisedWithoutERPOrder(ctx, ext)
	if err != nil {
		t.Fatalf("somando: %v", err)
	}
	if prometido != 2 {
		t.Errorf("prometidas = %d, quero 2 — a fila não tirou estoque de ninguém", prometido)
	}
}

func extIDDoProduto(t *testing.T, productID string) string {
	t.Helper()
	var ext string
	if err := testPool.QueryRow(context.Background(),
		`SELECT external_id FROM products WHERE id = $1`, productID).Scan(&ext); err != nil {
		t.Fatalf("lendo external_id: %v", err)
	}
	return ext
}

// O PEDIDO APAGADO NO ERP TEM DE SAIR DA VARREDURA.
//
// Medido em produção 01/09/2026, loja cantodaart: 44 pedidos que o lojista
// apagou no Tiny sendo perguntados DE HORA EM HORA, indefinidamente — 30 deles
// no mesmo minuto, contra um teto de 30 req/min POR CONTA que é o mesmo da
// live. 88 leituras em 66 minutos, todas respondendo `404 Pedido não
// encontrado`, nenhuma concluindo nada.
//
// A causa era um `continue`: o 404 não registrava fato nenhum, então o pedido
// continuava "parado há tempo demais" e voltava no ciclo seguinte. Para sempre.
func TestVarreduraParaDePerguntarPorPedidoApagadoNoERP(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	ctx := context.Background()

	if err := svc.EnsureERPOrderForCart(ctx, fx.cartID, fx.storeID); err != nil {
		t.Fatalf("criando: %v", err)
	}
	_, orderID, _, _ := cartERPState(t, fx.cartID)

	// O lojista apagou o pedido no ERP.
	fake.sumidos[orderID] = true

	svc.RunERPOrderStatusSweep(ctx, 0, 100)

	_, _, atual, _ := cartERPState(t, fx.cartID)
	if atual != string(providers.ERPOrderStatusNaoEncontrado) {
		t.Fatalf("situação = %q, quero %q — sem registrar o fato a varredura nunca para",
			atual, providers.ERPOrderStatusNaoEncontrado)
	}

	// ═══ A PARTE QUE IMPORTA ═══
	// A segunda varredura não pode perguntar de novo. É isto que a produção
	// fazia 44 vezes por hora.
	antes := len(fake.callsWithPrefix("GetSituacao:" + orderID))
	svc.RunERPOrderStatusSweep(ctx, 0, 100)
	depois := len(fake.callsWithPrefix("GetSituacao:" + orderID))

	if depois != antes {
		t.Errorf("a segunda varredura perguntou de novo (%d → %d): o pedido não saiu da lista de atrasados",
			antes, depois)
	}
}
