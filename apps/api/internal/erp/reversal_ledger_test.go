package erp

// Simulação em massa do estorno contra um razão de estoque no estilo Tiny.
//
// O contrato entre os dois lados é ASSIMÉTRICO, e é dele que nasce esta classe
// de bug: nós mandamos DELTAS ("saiu 1", "entrou 2") e o Tiny devolve SALDO
// ABSOLUTO no webhook. Delta aplicado duas vezes não é detectável olhando só
// para o nosso lado — vira saldo divergente do outro.
//
// O bug de campo, 08/08. O extrato do Tiny registrou, para a MESMA reserva
// f4590b1f do carrinho bb3b513e:
//
//	11:51   saída 1   Reserva LiveCart
//	11:55   saída 1   Ajuste reserva LiveCart (+1)
//	12:29   ENTRADA 2 Estorno expiração carrinho
//	12:30   ENTRADA 2 Estorno expiração carrinho   <-- a mesma, de novo
//
// Saíram 2 unidades e voltaram 4. O produto terminou com 7 onde deviam existir
// 5. A causa está no log da API às 15:29:28: o Tiny confirmou a entrada e o
// UPDATE que marcaria a reserva como 'reversed' morreu em "context deadline
// exceeded". A reserva continuou 'active', o handler devolveu erro, a asynq
// retentou, e a retentativa viu uma reserva ativa cujo estorno já tinha
// acontecido.
//
// A invariante que estes testes protegem: CADA RESERVA PRODUZ NO MÁXIMO UM
// MOVIMENTO DE ENTRADA NO ERP, não importa quantas vezes a operação seja
// repetida, nem em que ponto ela falhe.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

// ledgerRepo é um ERPRepository com estado REAL de reserva: a reivindicação é
// atômica, como no banco. Sem isso o teste não conseguiria distinguir a ordem
// certa da errada — que é justamente o que está sendo testado.
type ledgerRepo struct {
	mockRepo
	mu       sync.Mutex
	status   map[string]string // id -> active | reversed
	claimErr error
	restErr  error

	claims   int
	restores int
}

func newLedgerRepo(reservations []StockReservationRow) *ledgerRepo {
	st := map[string]string{}
	for _, r := range reservations {
		st[r.ID] = "active"
	}
	return &ledgerRepo{status: st}
}

func (l *ledgerRepo) ClaimReservationForReversal(_ context.Context, id string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.claims++
	if l.claimErr != nil {
		return false, l.claimErr
	}
	if l.status[id] != "active" {
		return false, nil // já reivindicada — é o caso da retentativa
	}
	l.status[id] = "reversed"
	return true, nil
}

func (l *ledgerRepo) RestoreReservationToActive(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.restores++
	if l.restErr != nil {
		return l.restErr
	}
	if l.status[id] == "reversed" {
		l.status[id] = "active"
	}
	return nil
}

// ListActiveReservationsByCart é o que a função sob teste enxerga. O mockRepo
// embutido devolve nil fixo; aqui ela devolve o que a execução preparou, que é
// como a retentativa vê só o que sobrou.
func (l *ledgerRepo) ListActiveReservationsByCart(context.Context, string) ([]StockReservationRow, error) {
	return l.existing, nil
}

func (l *ledgerRepo) activeIDs(all []StockReservationRow) []StockReservationRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []StockReservationRow
	for _, r := range all {
		if l.status[r.ID] == "active" {
			out = append(out, r)
		}
	}
	return out
}

// tinyLedger imita o extrato do Tiny: só sabe somar e subtrair o que mandamos,
// e não tem como saber que um delta chegou duas vezes.
type tinyLedger struct {
	providers.ERPProvider
	mu sync.Mutex

	stock       map[string]int // externalProductID -> saldo
	entradas    map[string]int // externalProductID -> quantas ENTRADAS recebeu
	saidas      map[string]int // externalProductID -> quantas SAÍDAS recebeu
	failFor     map[string]int // externalProductID -> falhar as N primeiras vezes
	movimentos  []string
	totalCalls  int
	failAllOnce bool
}

func newTinyLedger(initial map[string]int) *tinyLedger {
	st := map[string]int{}
	for k, v := range initial {
		st[k] = v
	}
	return &tinyLedger{stock: st, entradas: map[string]int{}, saidas: map[string]int{}, failFor: map[string]int{}}
}

// ReserveStock é a SAÍDA: tira do saldo, como o Tiny faz.
func (t *tinyLedger) ReserveStock(_ context.Context, productID string, qty int, _ float64, obs string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.totalCalls++
	if t.failAllOnce {
		t.failAllOnce = false
		return "", errors.New("tiny fora do ar")
	}
	t.stock[productID] -= qty
	t.saidas[productID]++
	t.movimentos = append(t.movimentos, fmt.Sprintf("SAIDA %d %s | %s", qty, productID, obs))
	return fmt.Sprintf("mov-%d", t.totalCalls), nil
}

func (t *tinyLedger) ReverseStockReservation(_ context.Context, productID string, qty int, _ float64, obs string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.totalCalls++
	if t.failAllOnce {
		t.failAllOnce = false
		return "", errors.New("tiny fora do ar")
	}
	if n := t.failFor[productID]; n > 0 {
		t.failFor[productID] = n - 1
		return "", errors.New("tiny recusou o movimento")
	}
	t.stock[productID] += qty
	t.entradas[productID]++
	t.movimentos = append(t.movimentos, fmt.Sprintf("ENTRADA %d %s | %s", qty, productID, obs))
	return fmt.Sprintf("mov-%d", t.totalCalls), nil
}

// ledgerCollab entrega o razão como provider do ERP.
type ledgerCollab struct {
	mockCollab
	ledger *tinyLedger
}

func (c *ledgerCollab) ResolveProvider(context.Context, *Integration) (providers.ERPProvider, error) {
	return c.ledger, nil
}

// Sem isto o mockCollab embutido devolve linked=false, ReserveStockInERP sai
// cedo e o teste inteiro vira um no-op que passa sem exercitar nada. Foi o que
// aconteceu na primeira versão deste arquivo.


func (c *ledgerCollab) ResolveExternalProduct(context.Context, string, string) (string, bool) {
	return "ext-1", true
}

// cenario monta o serviço, o razão e as reservas de um caso.
type cenario struct {
	svc    *Service
	repo   *ledgerRepo
	ledger *tinyLedger
	res    []StockReservationRow
}

func montaCenario(t *testing.T, produtos map[string]int, res []StockReservationRow) *cenario {
	t.Helper()
	repo := newLedgerRepo(res)
	repo.integration = &Integration{}
	ledger := newTinyLedger(produtos)
	svc := NewService(repo, &ledgerCollab{ledger: ledger}, zap.NewNop())
	return &cenario{svc: svc, repo: repo, ledger: ledger, res: res}
}

// roda executa UMA tentativa do estorno, enxergando só o que continua ativo —
// exatamente como ListActiveReservationsByCart faz em produção.
func (c *cenario) roda(t *testing.T, cartID string) error {
	t.Helper()
	c.repo.existing = c.repo.activeIDs(c.res)
	return c.svc.reverseCartReservationsInERP(context.Background(), cartID, "loja")
}

func reservas(cartID string, specs ...[2]interface{}) []StockReservationRow {
	var out []StockReservationRow
	for i, s := range specs {
		out = append(out, StockReservationRow{
			ID:                fmt.Sprintf("res-%d", i),
			CartID:            cartID,
			ExternalProductID: s[0].(string),
			Quantity:          s[1].(int),
		})
	}
	return out
}

// -----------------------------------------------------------------------------

// O caso exato de campo: uma reserva de 2, e o handler retentando.
func TestEstornoNaoDuplicaNaRetentativa(t *testing.T) {
	const cart = "bb3b513e-6981-4330-99a9-9504564f940c"
	res := reservas(cart, [2]interface{}{"perfume", 2})
	c := montaCenario(t, map[string]int{"perfume": 3}, res)

	// A asynq retenta até 3 vezes; o handler roda 4 vezes no pior caso.
	for i := 0; i < 4; i++ {
		if err := c.roda(t, cart); err != nil {
			t.Fatalf("tentativa %d: %v", i+1, err)
		}
	}

	if got := c.ledger.entradas["perfume"]; got != 1 {
		t.Errorf("o Tiny recebeu %d entradas, quero exatamente 1 — foi assim que o produto virou 7 unidades", got)
		for _, m := range c.ledger.movimentos {
			t.Logf("  %s", m)
		}
	}
	if got := c.ledger.stock["perfume"]; got != 5 {
		t.Errorf("saldo final no Tiny = %d, quero 5 (3 + as 2 devolvidas)", got)
	}
}

// Carrinho com vários itens: o prazo do laço não pode contaminar os últimos.
func TestEstornoDeCarrinhoGrandeEstornaCadaReservaUmaVez(t *testing.T) {
	const cart = "cart-grande"
	res := reservas(cart,
		[2]interface{}{"p1", 1}, [2]interface{}{"p2", 2}, [2]interface{}{"p3", 3},
		[2]interface{}{"p4", 1}, [2]interface{}{"p5", 4},
	)
	inicial := map[string]int{"p1": 10, "p2": 10, "p3": 10, "p4": 10, "p5": 10}
	c := montaCenario(t, inicial, res)

	for i := 0; i < 3; i++ {
		if err := c.roda(t, cart); err != nil {
			t.Fatalf("tentativa %d: %v", i+1, err)
		}
	}

	esperado := map[string]int{"p1": 11, "p2": 12, "p3": 13, "p4": 11, "p5": 14}
	for prod, want := range esperado {
		if got := c.ledger.entradas[prod]; got != 1 {
			t.Errorf("%s: %d entradas no Tiny, quero 1", prod, got)
		}
		if got := c.ledger.stock[prod]; got != want {
			t.Errorf("%s: saldo %d, quero %d", prod, got, want)
		}
	}
}

// O ERP recusa: a reserva TEM de voltar a ativa, senão a unidade fica presa
// fora do Tiny para sempre — nenhuma tentativa futura voltaria a enxergá-la.
func TestErpRecusaDevolveAReservaParaAtiva(t *testing.T) {
	const cart = "cart-recusa"
	res := reservas(cart, [2]interface{}{"jogo", 2})
	c := montaCenario(t, map[string]int{"jogo": 4}, res)
	c.ledger.failFor["jogo"] = 1 // falha só na primeira

	if err := c.roda(t, cart); err == nil {
		t.Fatal("primeira tentativa devia falhar — o erro é o que faz a asynq retentar")
	}
	if c.repo.restores != 1 {
		t.Errorf("restaurações = %d, quero 1 — sem restaurar, a reserva some do radar", c.repo.restores)
	}

	// A retentativa conclui.
	if err := c.roda(t, cart); err != nil {
		t.Fatalf("retentativa: %v", err)
	}
	if got := c.ledger.entradas["jogo"]; got != 1 {
		t.Errorf("entradas = %d, quero 1", got)
	}
	if got := c.ledger.stock["jogo"]; got != 6 {
		t.Errorf("saldo = %d, quero 6", got)
	}
}

// Duas execuções ao mesmo tempo — worker duplicado, deploy sobreposto, retry
// disparando junto com o handler original. Só uma pode mover o estoque.
func TestEstornoConcorrenteMoveEstoqueUmaVezSo(t *testing.T) {
	const cart = "cart-corrida"
	res := reservas(cart,
		[2]interface{}{"a", 2}, [2]interface{}{"b", 3}, [2]interface{}{"c", 1},
	)
	c := montaCenario(t, map[string]int{"a": 5, "b": 5, "c": 5}, res)

	// Cada goroutine enxerga a MESMA lista de ativas — o pior caso, em que as
	// duas leram antes de qualquer uma escrever.
	c.repo.existing = c.res
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.svc.reverseCartReservationsInERP(context.Background(), cart, "loja")
		}()
	}
	wg.Wait()

	for prod, want := range map[string]int{"a": 7, "b": 8, "c": 6} {
		if got := c.ledger.entradas[prod]; got != 1 {
			t.Errorf("%s: %d entradas com 8 execuções concorrentes, quero 1", prod, got)
		}
		if got := c.ledger.stock[prod]; got != want {
			t.Errorf("%s: saldo %d, quero %d", prod, got, want)
		}
	}
}

// Massa: muitos carrinhos, quantidades variadas e falhas intermitentes do ERP,
// com retentativas até drenar. O saldo final tem de fechar exatamente.
func TestEmMassaSaldoFinalFechaComOTiny(t *testing.T) {
	const (
		nCarrinhos = 40
		nProdutos  = 7
	)

	inicial := map[string]int{}
	for p := 0; p < nProdutos; p++ {
		inicial[fmt.Sprintf("prod-%d", p)] = 100
	}

	// Quanto cada produto teve de saída, para conferir a devolução no fim.
	saiu := map[string]int{}
	ledger := newTinyLedger(inicial)

	for ci := 0; ci < nCarrinhos; ci++ {
		cart := fmt.Sprintf("cart-%02d", ci)
		var specs [][2]interface{}
		// Quantidade e produto variam de forma determinística: mesmo caso toda
		// execução, sem depender de aleatoriedade.
		for k := 0; k <= ci%4; k++ {
			prod := fmt.Sprintf("prod-%d", (ci+k)%nProdutos)
			qty := 1 + (ci+k)%5
			specs = append(specs, [2]interface{}{prod, qty})
			saiu[prod] += qty
		}
		res := reservas(cart, specs...)

		repo := newLedgerRepo(res)
		repo.integration = &Integration{}
		svc := NewService(repo, &ledgerCollab{ledger: ledger}, zap.NewNop())

		// Um em cada três carrinhos leva uma recusa do ERP na primeira volta.
		if ci%3 == 0 {
			ledger.failAllOnce = true
		}

		// Retenta até não sobrar reserva ativa, com o teto da asynq.
		for tentativa := 0; tentativa < 4; tentativa++ {
			repo.existing = repo.activeIDs(res)
			if len(repo.existing) == 0 {
				break
			}
			_ = svc.reverseCartReservationsInERP(context.Background(), cart, "loja")
		}

		// Nada pode ficar ativo no fim.
		if restou := repo.activeIDs(res); len(restou) != 0 {
			t.Errorf("%s: %d reserva(s) ainda ativa(s) depois de drenar", cart, len(restou))
		}
	}

	// A conta que o lojista faz olhando o Tiny.
	for prod, devolvido := range saiu {
		want := 100 + devolvido
		if got := ledger.stock[prod]; got != want {
			t.Errorf("%s: saldo no Tiny = %d, quero %d (100 + %d devolvidos) — diferença de %d unidade(s) fantasma",
				prod, got, want, devolvido, got-want)
		}
	}
}

// Reivindicação falhando no banco NÃO pode virar movimento no ERP: sem saber se
// ganhamos a corrida, mexer no estoque é apostar.
func TestFalhaAoReivindicarNaoTocaNoErp(t *testing.T) {
	const cart = "cart-claim-ruim"
	res := reservas(cart, [2]interface{}{"x", 2})
	c := montaCenario(t, map[string]int{"x": 5}, res)
	c.repo.claimErr = errors.New("banco indisponível")

	if err := c.roda(t, cart); err == nil {
		t.Fatal("devia falhar para a asynq retentar")
	}
	if c.ledger.totalCalls != 0 {
		t.Errorf("o ERP foi chamado %d vez(es) sem termos a reivindicação", c.ledger.totalCalls)
	}
	if got := c.ledger.stock["x"]; got != 5 {
		t.Errorf("saldo mexeu para %d sem reivindicação", got)
	}
}
