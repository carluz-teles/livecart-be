package erp

// A onda de estornos do fechamento de evento, em escala real.
//
// Quando "Semana 16 a 21" fechar (21/08), ~400 reservas de ~120 carrinhos vão
// virar entradas na Tiny em rajada — o pior momento estatístico para timeout.
// Estes testes reproduzem a onda inteira contra uma Tiny de mentira que APLICA
// estoque de verdade, e injetam as falhas nas proporções observadas em produção
// (2 timeouts em ~200 chamadas na live de 17/08).
//
// O invariante central é o de 08/08, quando um retry cego deixou um produto de
// 5 unidades com 7: NENHUMA reserva pode receber duas entradas, em nenhuma
// combinação de falha, retry e concorrência.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

// ─── A Tiny de mentira: aplica estoque e falha sob roteiro ───────────────────

type comportamento int

const (
	entraNormal comportamento = iota
	timeoutQueENTROU
	timeoutQueNaoEntrou
	falhaDeDiscagem
)

type tinyDeOnda struct {
	providers.ERPProvider
	mu       sync.Mutex
	saldo    map[string]int
	roteiro  map[int]comportamento // nº da chamada (1-based) → comportamento
	chamadas int
	// aplicadas registra cada entrada que a Tiny DE FATO aplicou, pela chave de
	// idempotência achada na observação — é o extrato.
	aplicadas map[string]int
	seq       int64
}

func newTinyDeOnda() *tinyDeOnda {
	return &tinyDeOnda{saldo: map[string]int{}, roteiro: map[int]comportamento{}, aplicadas: map[string]int{}}
}

func chaveDaObs(obs string) string {
	i := strings.LastIndex(obs, "[")
	j := strings.LastIndex(obs, "]")
	if i < 0 || j < i {
		return obs
	}
	return obs[i+1 : j]
}

func (t *tinyDeOnda) ReverseStockReservation(_ context.Context, productID string, qty int, _ float64, obs string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.chamadas++
	c := t.roteiro[t.chamadas]
	switch c {
	case falhaDeDiscagem:
		return "", fmt.Errorf("reversing stock: %w",
			errors.Join(providers.ErrProvenUndelivered, errors.New("dial tcp: connection refused")))
	case timeoutQueNaoEntrou:
		return "", fmt.Errorf("reversing stock: %w", context.DeadlineExceeded)
	case timeoutQueENTROU:
		t.saldo[productID] += qty
		t.aplicadas[chaveDaObs(obs)]++
		return "", fmt.Errorf("reversing stock: %w", context.DeadlineExceeded)
	default:
		t.saldo[productID] += qty
		t.aplicadas[chaveDaObs(obs)]++
		t.seq++
		return fmt.Sprintf("mov-%d", t.seq), nil
	}
}

// ─── Reservas em memória com claim atômico ───────────────────────────────────

type claimerDeOnda struct {
	mu     sync.Mutex
	estado map[string]string // reservation id → active | reversed
}

func newClaimerDeOnda() *claimerDeOnda { return &claimerDeOnda{estado: map[string]string{}} }

func (c *claimerDeOnda) ClaimReservationForReversal(_ context.Context, id string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.estado[id] != "active" {
		return false, nil
	}
	c.estado[id] = "reversed"
	return true, nil
}

func (c *claimerDeOnda) RestoreReservationToActive(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.estado[id] == "reversed" {
		c.estado[id] = "active"
	}
	return nil
}

func (c *claimerDeOnda) contar(estado string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.estado {
		if e == estado {
			n++
		}
	}
	return n
}

// ─── Montagem da onda ─────────────────────────────────────────────────────────

type onda struct {
	svc     *Service
	ledger  *fakeLedger
	tiny    *tinyDeOnda
	claimer *claimerDeOnda
	sched   *agendadorEspiao
	rows    []ReversibleReservation // todas, na ordem
	porCart map[string][]ReversibleReservation
}

// montarOnda cria nReservas espalhadas por nCarts, ~120 produtos.
func montarOnda(t *testing.T, nReservas, nCarts int) *onda {
	t.Helper()
	tiny := newTinyDeOnda()
	repo := &mockRepo{}
	collab := &mockCollab{provider: tiny, linked: true, externalID: "x"}
	svc := NewService(repo, collab, zap.NewNop())
	ledger := newFakeLedger()
	svc.SetStockMovementLedger(ledger)
	sched := &agendadorEspiao{}
	svc.SetStockMovementScheduler(sched)
	claimer := newClaimerDeOnda()

	o := &onda{svc: svc, ledger: ledger, tiny: tiny, claimer: claimer, sched: sched,
		porCart: map[string][]ReversibleReservation{}}
	for i := 0; i < nReservas; i++ {
		r := ReversibleReservation{
			ID:                fmt.Sprintf("res-%03d", i),
			ExternalProductID: fmt.Sprintf("ext-%03d", i%120),
			Quantity:          1 + i%3,
			CartID:            fmt.Sprintf("cart-%03d", i%nCarts),
			EventID:           "ev-1",
			ProductID:         fmt.Sprintf("prod-%03d", i%120),
		}
		claimer.estado[r.ID] = "active"
		o.rows = append(o.rows, r)
		o.porCart[r.CartID] = append(o.porCart[r.CartID], r)
	}
	return o
}

func (o *onda) hooks() *ReversalLedger {
	return o.svc.ReversalLedgerHooks("loja-1")
}

// rodar estorna carrinho a carrinho, como a expiração faz.
func (o *onda) rodar(t *testing.T) {
	t.Helper()
	for cart, rows := range o.porCart {
		ReverseReservationsClaimFirst(context.Background(), zap.NewNop(), o.claimer, o.tiny, rows,
			func(r ReversibleReservation) string {
				return fmt.Sprintf("Estorno expiração carrinho LiveCart - Cart %s - Reserva %s", cart, r.ID)
			}, o.hooks())
	}
}

// resolverTudo roda o resolver sobre toda linha não confirmada, n passadas.
func (o *onda) resolverTudo(t *testing.T, passadas int) {
	t.Helper()
	for p := 0; p < passadas; p++ {
		o.ledger.mu.Lock()
		var ids []string
		for id, r := range o.ledger.rows {
			if r.Status != MovementConfirmed {
				ids = append(ids, id)
			}
		}
		o.ledger.mu.Unlock()
		for _, id := range ids {
			if err := o.svc.RunScheduledMovementResolve(context.Background(), id); err != nil {
				t.Fatalf("resolver(%s): %v", id, err)
			}
		}
	}
}

// contabilidade fecha o livro: movimento a movimento contra o extrato da Tiny.
func (o *onda) contabilidade() (porStatus map[string]int, porReserva map[string][]*StockMovementRow) {
	porStatus = map[string]int{}
	porReserva = map[string][]*StockMovementRow{}
	o.ledger.mu.Lock()
	defer o.ledger.mu.Unlock()
	for _, r := range o.ledger.rows {
		porStatus[r.Status]++
		c := *r
		porReserva[r.ReservationID] = append(porReserva[r.ReservationID], &c)
	}
	return
}

// ─── T1: onda saudável, 400 estornos ─────────────────────────────────────────

func TestOndaDe400EstornosSaudavel(t *testing.T) {
	o := montarOnda(t, 400, 121)
	o.rodar(t)

	if o.tiny.chamadas != 400 {
		t.Fatalf("POSTs = %d; esperava exatamente 400", o.tiny.chamadas)
	}
	porStatus, porReserva := o.contabilidade()
	if porStatus[MovementConfirmed] != 400 || len(porStatus) != 1 {
		t.Fatalf("razão fora do lugar: %v", porStatus)
	}
	if n := o.claimer.contar("reversed"); n != 400 {
		t.Fatalf("reservas reversed = %d", n)
	}
	// Lei de conservação: exatamente UMA entrada confirmada por reserva, e cada
	// uma existe no extrato.
	for res, movs := range porReserva {
		if len(movs) != 1 {
			t.Fatalf("reserva %s tem %d movimentos", res, len(movs))
		}
		if o.tiny.aplicadas[movs[0].IdempotencyKey] != 1 {
			t.Fatalf("movimento confirmado %s não está no extrato da Tiny", movs[0].IdempotencyKey)
		}
	}
}

// ─── T2: a onda com as falhas de produção injetadas ──────────────────────────

func TestOndaComTimeoutsEDiscagens(t *testing.T) {
	o := montarOnda(t, 400, 121)
	// Proporções da vida real, um pouco piores: a cada 50 chamadas um timeout
	// que ENTROU (o caso elima2013), a cada 61 um que não entrou (amandinha), a
	// cada 83 uma queda de rede.
	for i := 1; i <= 400; i++ {
		switch {
		case i%50 == 0:
			o.tiny.roteiro[i] = timeoutQueENTROU
		case i%61 == 0:
			o.tiny.roteiro[i] = timeoutQueNaoEntrou
		case i%83 == 0:
			o.tiny.roteiro[i] = falhaDeDiscagem
		}
	}

	o.rodar(t)
	o.resolverTudo(t, 3) // os failed re-executam (a Tiny já voltou: roteiro só vale para as 400 primeiras chamadas)

	porStatus, porReserva := o.contabilidade()

	// O invariante de 08/08, sob 400 chamadas e três tipos de falha:
	for res, movs := range porReserva {
		aplicadasDaReserva := 0
		for _, m := range movs {
			aplicadasDaReserva += o.tiny.aplicadas[m.IdempotencyKey]
		}
		if aplicadasDaReserva > 1 {
			t.Fatalf("reserva %s recebeu %d entradas na Tiny — é o estoque fantasma "+
				"de 08/08 de volta", res, aplicadasDaReserva)
		}
		if len(movs) != 1 {
			t.Fatalf("reserva %s tem %d linhas de movimento; a retentativa reusa a "+
				"linha, nunca cria outra", res, len(movs))
		}
	}

	// Timeouts (ambos os mundos) ficaram unconfirmed com UM POST cada; nada de
	// retry cego.
	unconfirmed := porStatus[MovementUnconfirmed]
	esperadosAmbiguos := 8 + 6 // 400/50 + os %61 que não colidem com %50
	if unconfirmed != esperadosAmbiguos {
		t.Errorf("unconfirmed = %d; esperava %d (os timeouts injetados)", unconfirmed, esperadosAmbiguos)
	}
	// As quedas de rede terminaram confirmadas pelo resolver.
	if porStatus[MovementFailed] != 0 {
		t.Errorf("failed remanescentes = %d; o resolver devia ter re-executado todos", porStatus[MovementFailed])
	}
	if porStatus[MovementConfirmed] != 400-unconfirmed {
		t.Errorf("confirmed = %d; esperava %d", porStatus[MovementConfirmed], 400-unconfirmed)
	}

	// TODA reserva segue 'reversed' (o modo razão nunca restaura em falha de
	// POST): nenhuma varredura futura vai reenviar nada.
	if n := o.claimer.contar("active"); n != 0 {
		t.Errorf("%d reservas voltaram a 'active' — é a porta do retry cego", n)
	}

	// E o extrato fecha com o razão: entradas aplicadas na Tiny = confirmadas +
	// os timeouts que entraram sem ninguém saber.
	totalAplicadas := 0
	o.tiny.mu.Lock()
	for _, n := range o.tiny.aplicadas {
		totalAplicadas += n
	}
	o.tiny.mu.Unlock()
	if totalAplicadas != porStatus[MovementConfirmed]+8 {
		t.Errorf("extrato da Tiny (%d) ≠ confirmadas (%d) + timeouts-que-entraram (8)",
			totalAplicadas, porStatus[MovementConfirmed])
	}
}

// ─── T3: a onda inteira entregue DUAS vezes (asynq redelivery) ───────────────

func TestOndaConcorrenteNaoDuplicaNenhumaEntrada(t *testing.T) {
	o := montarOnda(t, 400, 121)

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o.rodar(t)
		}()
	}
	wg.Wait()

	if o.tiny.chamadas != 400 {
		t.Fatalf("POSTs = %d com a onda entregue em dobro; o claim tinha que "+
			"segurar em exatamente 400", o.tiny.chamadas)
	}
	porStatus, _ := o.contabilidade()
	if porStatus[MovementConfirmed] != 400 {
		t.Fatalf("confirmadas = %d", porStatus[MovementConfirmed])
	}
}

// ─── T4: resolver concorrente sobre o mesmo movimento ────────────────────────

func TestResolverConcorrenteExecutaUmaVezSo(t *testing.T) {
	o := montarOnda(t, 1, 1)
	o.tiny.roteiro[1] = falhaDeDiscagem
	o.rodar(t)

	porStatus, _ := o.contabilidade()
	if porStatus[MovementFailed] != 1 {
		t.Fatalf("pré-condição: %v", porStatus)
	}
	var id string
	o.ledger.mu.Lock()
	for k := range o.ledger.rows {
		id = k
	}
	o.ledger.mu.Unlock()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = o.svc.RunScheduledMovementResolve(context.Background(), id)
		}()
	}
	wg.Wait()

	if o.tiny.chamadas != 2 {
		t.Fatalf("POSTs = %d; a falha (1) + exatamente UMA retentativa — o claim "+
			"do movimento é quem garante", o.tiny.chamadas)
	}
}

// ─── T5: intenção não gravada = chamada não feita ────────────────────────────

func TestSemIntencaoNaoHaChamadaEAReservaVolta(t *testing.T) {
	o := montarOnda(t, 1, 1)
	o.ledger.createErr = errors.New("banco recusou (scripted)")

	o.rodar(t)

	if o.tiny.chamadas != 0 {
		t.Fatalf("POST feito sem intenção gravada — é o vão de 17/08 de volta")
	}
	if n := o.claimer.contar("active"); n != 1 {
		t.Errorf("a reserva não voltou para 'active' (%d) — nada foi enviado, "+
			"restaurar é seguro e devolve a unidade à próxima varredura", n)
	}
}
