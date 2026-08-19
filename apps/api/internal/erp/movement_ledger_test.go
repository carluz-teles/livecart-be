package erp

// O razão de movimentos, validado contra os DOIS desfechos reais de 17/08/2026.
//
// Naquela noite, dois POSTs de reserva do mesmo produto (1419) deram timeout com
// 15 segundos de diferença. O da @elima2013 tinha ENTRADO no Tiny (o lançamento
// está no extrato); o da @amandinha2903, não. Qualquer política que trate os
// dois igual com uma ESCRITA está errada em um deles — foi provado em produção.
// A única resposta correta para a ambiguidade é: registrar, não repetir,
// aparecer, e travar o dinheiro até resolver.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

// fakeLedger é o razão em memória.
type fakeLedger struct {
	mu     sync.Mutex
	rows   map[string]*StockMovementRow
	seq    int
	stale  map[string]bool // linhas tratadas como envelhecidas pelos guards do claim
	failAt string          // id cuja escrita de desfecho deve falhar
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{rows: map[string]*StockMovementRow{}, stale: map[string]bool{}}
}

func (f *fakeLedger) CreateERPStockMovement(_ context.Context, p CreateStockMovementParams) (*StockMovementRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	row := &StockMovementRow{
		ID:                fmt.Sprintf("mov-%d", f.seq),
		StoreID:           p.StoreID,
		CartID:            p.CartID,
		EventID:           p.EventID,
		ProductID:         p.ProductID,
		ExternalProductID: p.ExternalProductID,
		Direction:         p.Direction,
		Quantity:          p.Quantity,
		UnitPriceCents:    p.UnitPriceCents,
		IdempotencyKey:    fmt.Sprintf("key-%d", f.seq),
		Status:            MovementPending,
		CreatedAt:         time.Now(),
	}
	f.rows[row.ID] = row
	// Cópia de verdade: o repositório real devolve structs independentes do
	// banco, e o aliasing aqui esconderia contagem de tentativa dobrada.
	c := *row
	return &c, nil
}

func (f *fakeLedger) GetERPStockMovement(_ context.Context, id string) (*StockMovementRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rows[id]; ok {
		c := *r
		return &c, nil
	}
	return nil, nil
}

func (f *fakeLedger) MarkERPStockMovementConfirmed(_ context.Context, id, erpMovementID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rows[id]
	r.Status = MovementConfirmed
	r.ERPMovementID = erpMovementID
	return nil
}

func (f *fakeLedger) MarkERPStockMovementOutcome(_ context.Context, id, status, lastError string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAt == id {
		return errors.New("ledger write refused (scripted)")
	}
	r := f.rows[id]
	r.Status = status
	r.LastError = lastError
	r.Attempts++
	return nil
}

func (f *fakeLedger) ClaimERPStockMovement(_ context.Context, id, fromStatus string) (*StockMovementRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok || r.Status != fromStatus {
		return nil, nil
	}
	// Espelha os guards de idade da query: pending/resolving recentes pertencem
	// a quem os criou e não são reivindicáveis.
	if (fromStatus == MovementPending || fromStatus == MovementResolving) && !f.stale[id] {
		return nil, nil
	}
	r.Status = MovementResolving
	c := *r
	return &c, nil
}

func (f *fakeLedger) ListUnresolvedERPStockMovementsByCart(_ context.Context, cartID string) ([]StockMovementRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []StockMovementRow
	for _, r := range f.rows {
		if r.CartID == cartID && r.Status != MovementConfirmed {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *fakeLedger) status(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[id].Status
}

// scriptedProvider devolve um desfecho por chamada, na ordem.
type scriptedProvider struct {
	providers.ERPProvider
	mu       sync.Mutex
	script   []func() (string, error)
	reserves int
}

func (p *scriptedProvider) ReserveStock(_ context.Context, _ string, _ int, _ float64, _ string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reserves++
	if len(p.script) == 0 {
		return "", errors.New("scripted provider exhausted")
	}
	next := p.script[0]
	p.script = p.script[1:]
	return next()
}

func ok(id string) func() (string, error) {
	return func() (string, error) { return id, nil }
}
func discagem() func() (string, error) {
	return func() (string, error) {
		return "", fmt.Errorf("reserving stock: %w",
			errors.Join(providers.ErrProvenUndelivered, errors.New("dial tcp: connection refused")))
	}
}
func timeoutAmbiguo() func() (string, error) {
	return func() (string, error) {
		return "", fmt.Errorf("reserving stock: executing request: %w", context.DeadlineExceeded)
	}
}

// agendadorEspiao registra o que foi agendado.
type agendadorEspiao struct {
	mu      sync.Mutex
	agendas []time.Duration
}

func (a *agendadorEspiao) ScheduleStockMovementResolve(_ context.Context, _ string, at time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.agendas = append(a.agendas, time.Until(at).Round(time.Second))
	return nil
}

func servicoComLedger(t *testing.T, prov *scriptedProvider) (*Service, *fakeLedger, *mockRepo, *agendadorEspiao) {
	t.Helper()
	repo := &mockRepo{}
	collab := &mockCollab{provider: prov, linked: true, externalID: "843169697"}
	svc := NewService(repo, collab, zap.NewNop())
	ledger := newFakeLedger()
	svc.SetStockMovementLedger(ledger)
	sched := &agendadorEspiao{}
	svc.SetStockMovementScheduler(sched)
	return svc, ledger, repo, sched
}

func reservar(t *testing.T, svc *Service, ledger *fakeLedger) *StockMovementRow {
	t.Helper()
	mov, err := ledger.CreateERPStockMovement(context.Background(), CreateStockMovementParams{
		StoreID: "loja-1", CartID: "cart-1", EventID: "ev-1", ProductID: "prod-1419",
		ExternalProductID: "843169697", Direction: "out", Quantity: 1, UnitPriceCents: 990,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Execução SÍNCRONA nos testes: a goroutine de produção chama exatamente
	// este método; o que se valida aqui é a máquina de estados, não o go.
	svc.executeStockMovement(context.Background(), svc.collabProvider(t), mov, movementObservacao(mov, "ana", 0))
	return mov
}

// collabProvider extrai o provider do collab do teste.
func (s *Service) collabProvider(t *testing.T) providers.ERPProvider {
	t.Helper()
	p, err := s.collab.ResolveProvider(context.Background(), &Integration{ID: "int-1"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ─── Caminho feliz ───────────────────────────────────────────────────────────

func TestMovimentoConfirmadoViraAtivoNoAgregado(t *testing.T) {
	prov := &scriptedProvider{script: []func() (string, error){ok("55501")}}
	svc, ledger, repo, _ := servicoComLedger(t, prov)

	mov := reservar(t, svc, ledger)

	if got := ledger.status(mov.ID); got != MovementConfirmed {
		t.Fatalf("status = %s; esperava confirmed", got)
	}
	if repo.reservationUpserts != 1 {
		t.Errorf("agregado aplicado %d vez(es); o resto do sistema (estorno no "+
			"pagamento, fila, reconciliação) lê o agregado", repo.reservationUpserts)
	}
	if prov.reserves != 1 {
		t.Errorf("POSTs = %d; esperava 1", prov.reserves)
	}
}

// ─── O caso @amandinha2903 e @elima2013: timeout é ambíguo ──────────────────

func TestTimeoutNuncaEhRepetidoAsCegas(t *testing.T) {
	prov := &scriptedProvider{script: []func() (string, error){timeoutAmbiguo()}}
	svc, ledger, repo, sched := servicoComLedger(t, prov)

	mov := reservar(t, svc, ledger)

	if got := ledger.status(mov.ID); got != MovementUnconfirmed {
		t.Fatalf("status = %s; esperava unconfirmed — timeout não prova nada", got)
	}
	if prov.reserves != 1 {
		t.Fatalf("POSTs = %d — repetir um timeout criaria o segundo lançamento "+
			"que ninguém estorna (foi o órfão da @elima2013)", prov.reserves)
	}
	if len(sched.agendas) != 0 {
		t.Errorf("retry agendado para movimento ambíguo: %v", sched.agendas)
	}
	if repo.reservationUpserts != 0 {
		t.Errorf("agregado aplicado sem confirmação: %d", repo.reservationUpserts)
	}

	// E o resolver também não toca nele:
	if err := svc.RunScheduledMovementResolve(context.Background(), mov.ID); err != nil {
		t.Fatal(err)
	}
	if prov.reserves != 1 {
		t.Errorf("o resolver re-executou um movimento ambíguo (%d POSTs)", prov.reserves)
	}
	if got := ledger.status(mov.ID); got != MovementUnconfirmed {
		t.Errorf("resolver mudou o status de um ambíguo para %s", got)
	}
}

// ─── Falha de discagem: prova de não-entrega, retry espaçado ─────────────────

func TestDiscagemViraFailedEResolverReexecutaComSucesso(t *testing.T) {
	prov := &scriptedProvider{script: []func() (string, error){discagem(), ok("55502")}}
	svc, ledger, repo, sched := servicoComLedger(t, prov)

	mov := reservar(t, svc, ledger)

	if got := ledger.status(mov.ID); got != MovementFailed {
		t.Fatalf("status = %s; conexão recusada prova que nada chegou", got)
	}
	if len(sched.agendas) != 1 || sched.agendas[0] != 30*time.Second {
		t.Errorf("agenda = %v; a primeira retentativa é em 30s", sched.agendas)
	}

	if err := svc.RunScheduledMovementResolve(context.Background(), mov.ID); err != nil {
		t.Fatal(err)
	}
	if got := ledger.status(mov.ID); got != MovementConfirmed {
		t.Fatalf("status pós-resolver = %s; esperava confirmed", got)
	}
	if prov.reserves != 2 {
		t.Errorf("POSTs = %d; esperava 2 (a falha e a retentativa)", prov.reserves)
	}
	if repo.reservationUpserts != 1 {
		t.Errorf("agregado aplicado %d vez(es) — a retentativa confirmada aplica UMA", repo.reservationUpserts)
	}
}

func TestRetentativasSaoEspacadasEComTeto(t *testing.T) {
	if d := movementRetryDelay(1); d != 30*time.Second {
		t.Errorf("1ª = %v", d)
	}
	if d := movementRetryDelay(2); d != 2*time.Minute {
		t.Errorf("2ª = %v", d)
	}
	if d := movementRetryDelay(3); d != 10*time.Minute {
		t.Errorf("3ª = %v", d)
	}

	// Depois do teto, o resolver PARA — a linha fica visível e o gate segura.
	prov := &scriptedProvider{script: []func() (string, error){
		discagem(), discagem(), discagem(), discagem(), discagem(), discagem(),
	}}
	svc, ledger, _, _ := servicoComLedger(t, prov)
	mov := reservar(t, svc, ledger)
	for i := 0; i < 10; i++ {
		if err := svc.RunScheduledMovementResolve(context.Background(), mov.ID); err != nil {
			t.Fatal(err)
		}
	}
	if prov.reserves > movementMaxAttempts {
		t.Errorf("POSTs = %d; o teto é %d — sem ele, Tiny fora do ar vira loop", prov.reserves, movementMaxAttempts)
	}
	if got := ledger.status(mov.ID); got != MovementFailed {
		t.Errorf("status final = %s; parado e visível é o combinado", got)
	}
}

// ─── Processo morto no meio da chamada ───────────────────────────────────────

func TestPendingEnvelhecidoViraUnconfirmedNuncaRetry(t *testing.T) {
	prov := &scriptedProvider{}
	svc, ledger, _, _ := servicoComLedger(t, prov)

	mov, _ := ledger.CreateERPStockMovement(context.Background(), CreateStockMovementParams{
		StoreID: "loja-1", CartID: "cart-1", ProductID: "p", ExternalProductID: "x",
		Direction: "out", Quantity: 1,
	})
	ledger.stale[mov.ID] = true // o deploy derrubou o pod com a chamada em voo

	if err := svc.RunScheduledMovementResolve(context.Background(), mov.ID); err != nil {
		t.Fatal(err)
	}
	if got := ledger.status(mov.ID); got != MovementUnconfirmed {
		t.Fatalf("status = %s; processo morto em voo é desfecho DESCONHECIDO", got)
	}
	if prov.reserves != 0 {
		t.Errorf("re-executou (%d POSTs) um movimento cujo desfecho ninguém sabe", prov.reserves)
	}
}

func TestPendingRecenteNaoEhTocado(t *testing.T) {
	prov := &scriptedProvider{}
	svc, ledger, _, _ := servicoComLedger(t, prov)
	mov, _ := ledger.CreateERPStockMovement(context.Background(), CreateStockMovementParams{
		StoreID: "loja-1", CartID: "cart-1", ProductID: "p", ExternalProductID: "x",
		Direction: "out", Quantity: 1,
	})
	// stale NÃO marcado: a goroutine dona ainda está com a chamada em voo.
	if err := svc.RunScheduledMovementResolve(context.Background(), mov.ID); err != nil {
		t.Fatal(err)
	}
	if got := ledger.status(mov.ID); got != MovementPending {
		t.Errorf("status = %s; pending recente pertence à goroutine que o criou", got)
	}
}

// ─── O gate do pagamento: o que impede a baixa dobrada ───────────────────────

func TestGateSeguraFinalizacaoComMovimentoEmDuvida(t *testing.T) {
	prov := &scriptedProvider{script: []func() (string, error){timeoutAmbiguo()}}
	svc, ledger, _, _ := servicoComLedger(t, prov)
	mov := reservar(t, svc, ledger)

	err := svc.ResolveCartMovementsBeforeFinalisation(context.Background(), "cart-1")
	if err == nil {
		t.Fatal("a finalização passou com movimento em dúvida — é a baixa dobrada " +
			"da @elima2013 acontecendo de novo")
	}
	if !strings.Contains(err.Error(), mov.IdempotencyKey) {
		t.Errorf("o erro não cita a chave de idempotência (%q) — é ela que o "+
			"humano procura no extrato do Tiny: %v", mov.IdempotencyKey, err)
	}
}

func TestGateDaChanceInlineAoFailedEDepoisLibera(t *testing.T) {
	// failed com retentativa que dá certo NO GATE: paga sem intervenção.
	prov := &scriptedProvider{script: []func() (string, error){discagem(), ok("55503")}}
	svc, ledger, repo, _ := servicoComLedger(t, prov)
	mov := reservar(t, svc, ledger)
	if got := ledger.status(mov.ID); got != MovementFailed {
		t.Fatalf("pré-condição: %s", got)
	}

	if err := svc.ResolveCartMovementsBeforeFinalisation(context.Background(), "cart-1"); err != nil {
		t.Fatalf("o gate não deu a chance inline: %v", err)
	}
	if got := ledger.status(mov.ID); got != MovementConfirmed {
		t.Errorf("status pós-gate = %s", got)
	}
	if repo.reservationUpserts != 1 {
		t.Errorf("agregado = %d aplicações", repo.reservationUpserts)
	}
}

func TestGateComTudoConfirmadoNaoAtrapalha(t *testing.T) {
	prov := &scriptedProvider{script: []func() (string, error){ok("55504")}}
	svc, ledger, _, _ := servicoComLedger(t, prov)
	reservar(t, svc, ledger)

	if err := svc.ResolveCartMovementsBeforeFinalisation(context.Background(), "cart-1"); err != nil {
		t.Fatalf("gate travou um carrinho saudável: %v", err)
	}
}

func TestGateSemLedgerEhNoop(t *testing.T) {
	svc := newSvc(&mockRepo{}, &mockCollab{})
	if err := svc.ResolveCartMovementsBeforeFinalisation(context.Background(), "cart-1"); err != nil {
		t.Fatalf("sem ledger o gate precisa ser transparente: %v", err)
	}
}

// ─── ReserveStockInERP não espera mais o Tiny ────────────────────────────────

func TestReservaNaoBloqueiaNoTinyLento(t *testing.T) {
	lento := &scriptedProvider{script: []func() (string, error){func() (string, error) {
		time.Sleep(2 * time.Second)
		return "55505", nil
	}}}
	svc, ledger, _, _ := servicoComLedger(t, lento)

	inicio := time.Now()
	if err := svc.ReserveStockInERP(context.Background(), "loja-1", "cart-1", "ev-1", "prod-1419", 1, 990, "ana"); err != nil {
		t.Fatalf("ReserveStockInERP: %v", err)
	}
	if dur := time.Since(inicio); dur > 500*time.Millisecond {
		t.Fatalf("a chamada bloqueou %v esperando o Tiny — foi o prazo "+
			"compartilhado que matou as DMs às 21:17", dur)
	}

	// A intenção já existe ANTES de o Tiny responder:
	rows, _ := ledger.ListUnresolvedERPStockMovementsByCart(context.Background(), "cart-1")
	if len(rows) != 1 || rows[0].Status != MovementPending {
		t.Fatalf("intenção não registrada: %+v", rows)
	}

	// E o desfecho chega sozinho:
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ledger.status(rows[0].ID) == MovementConfirmed {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("movimento nunca confirmou: %s", ledger.status(rows[0].ID))
}
