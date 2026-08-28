package erp

// Repositório em memória com a mesma semântica de concorrência do de verdade:
// a transição de estado é um CAS sob trava, que é o que torna o single-flight
// possível. Um duplo que deixasse duas goroutines "ganharem" o mesmo CAS
// esconderia exatamente a corrida que os testes existem para provar.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"livecart/apps/api/internal/integration/providers"
)

type carrinhoSimulado struct {
	state           string
	externalOrderID string
	stockLaunched   bool
	itens           []NonWaitlistedCartItem
	statusERP       string
	statusEm        time.Time
	numeroPedido    string
	// operacaoEm é o carimbo de quando a operação ERP em curso começou, igual ao
	// erp_op_started_at da coluna.
	operacaoEm time.Time
	// O dinheiro, como a 000140 o guarda: quanto já entrou no carrinho e quantas
	// unidades de cada produto algum pagamento cobriu. Modelado aqui porque a
	// divisão pago/a pagar é decidida por estes números.
	pagoCents int64
	pagoEm    time.Time
	pagoQtd   map[string]int
	// livro é a razão de pagamentos, como a 000141 a guarda: uma linha por
	// cobrança, com o que entrou e o preço cheio que aquilo liquidou.
	livro []CartPayment
}

type repoSimulado struct {
	mu        sync.Mutex
	carrinhos map[string]*carrinhoSimulado
	estoque   map[string]int
	// travaConfirm simula o advisory lock por carrinho.
	travados map[string]bool

	semIntegracao bool
	snapshot      []byte
	// idadeDaOperacao força o relógio da operação ERP em curso. Zero = "velha o
	// bastante para retomar", que é o padrão dos testes de fluxo.
	idadeDaOperacao time.Duration
	// presos é o que a varredura enxerga como operação abandonada.
	presos []StuckERPOrderOp

	transicoesGanhas int
	reservados       []StockEventParams
	liberados        []StockEventParams
	statusEventos    []ERPOrderStatusObservation
}

func novoRepoSimulado() *repoSimulado {
	return &repoSimulado{
		carrinhos: map[string]*carrinhoSimulado{},
		estoque:   map[string]int{},
		travados:  map[string]bool{},
	}
}

func (r *repoSimulado) criarCarrinho(id string, itens ...NonWaitlistedCartItem) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.carrinhos[id] = &carrinhoSimulado{state: OrderStateNone, itens: itens}
}

func (r *repoSimulado) carrinho(id string) carrinhoSimulado {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.carrinhos[id]; ok {
		return *c
	}
	return carrinhoSimulado{}
}

func (r *repoSimulado) definirItens(cartID string, itens ...NonWaitlistedCartItem) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.carrinhos[cartID]; ok {
		c.itens = itens
	}
}

// --- ERPRepository ----------------------------------------------------------

func (r *repoSimulado) GetActiveByProvider(context.Context, string, string, string) (*Integration, error) {
	if r.semIntegracao {
		return nil, pgx.ErrNoRows
	}
	return &Integration{ID: "int-1", StoreID: "loja-1", Provider: "tiny"}, nil
}

func (r *repoSimulado) GetByProvider(ctx context.Context, s, t, p string) (*Integration, error) {
	return r.GetActiveByProvider(ctx, s, t, p)
}

func (r *repoSimulado) AcquireCartFinalisationLock(_ context.Context, cartID string) (func(), bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.travados[cartID] {
		return nil, false, nil
	}
	r.travados[cartID] = true
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.travados, cartID)
	}, true, nil
}

func (r *repoSimulado) GetCartERPOrderState(_ context.Context, cartID string) (*CartERPOrderState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.carrinhos[cartID]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return &CartERPOrderState{
		State:           c.state,
		StockLaunched:   c.stockLaunched,
		ExternalOrderID: c.externalOrderID,
		OrderStatus:     c.statusERP,
		PaidAmountCents: c.pagoCents,
		PaidAt:          c.pagoEm,
	}, nil
}

// GetCartERPOpAge conta a partir do instante em que o carrinho ENTROU na
// operação — como a coluna erp_op_started_at faz no banco.
//
// Devolver uma idade alta por padrão parecia conveniente e é falso: durante uma
// rajada real a operação tem segundos de vida, e todo comentário concorrente a
// veria como abandonada, retomaria, e criaria um pedido a mais. Um duplo que
// mente sobre o relógio esconde exatamente a corrida que ele deveria expor.
//
// idadeDaOperacao sobrepõe isto para o teste que QUER a retomada.
func (r *repoSimulado) GetCartERPOpAge(_ context.Context, cartID string) (time.Duration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.idadeDaOperacao != 0 {
		return r.idadeDaOperacao, nil
	}
	c, ok := r.carrinhos[cartID]
	if !ok || c.operacaoEm.IsZero() {
		return 0, nil
	}
	return time.Since(c.operacaoEm), nil
}

func (r *repoSimulado) GetCartERPFinalisationStatus(_ context.Context, cartID string) (*CartFinalisationStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.carrinhos[cartID]
	if c == nil {
		return nil, pgx.ErrNoRows
	}
	return &CartFinalisationStatus{CartID: cartID, ExternalOrderID: c.externalOrderID, PaymentSnapshot: r.snapshot}, nil
}

func (r *repoSimulado) MarkCartERPFinalisationAttempt(context.Context, string, []byte) error {
	return nil
}
func (r *repoSimulado) GetCartShortID(context.Context, string) (int32, error) { return 42, nil }

func (r *repoSimulado) DecrementProductStock(_ context.Context, produtoID string, q int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.estoque[produtoID]-q < 0 {
		return pgx.ErrNoRows
	}
	r.estoque[produtoID] -= q
	return nil
}

func (r *repoSimulado) IncrementProductStock(_ context.Context, produtoID string, q int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.estoque[produtoID] += q
	return nil
}

func (r *repoSimulado) EmitStockReserved(_ context.Context, p StockEventParams) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reservados = append(r.reservados, p)
	return nil
}

func (r *repoSimulado) EmitStockReleased(_ context.Context, p StockEventParams) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.liberados = append(r.liberados, p)
	return nil
}

// TransitionCartERPOrderState é o CAS. Devolve false sem alterar nada quando o
// estado atual não é o esperado — é isto que faz o single-flight funcionar.
//
// Respeita o cancelamento do contexto, como um driver de banco de verdade. Um
// duplo que ignorasse isso deixaria passar a classe inteira de defeitos em que a
// COMPENSAÇÃO morre junto com a operação que ela compensa — e foi exatamente
// essa que travou seis carrinhos numa live simulada.
func (r *repoSimulado) TransitionCartERPOrderState(ctx context.Context, cartID, de, para string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.carrinhos[cartID]
	if !ok || c.state != de {
		return false, nil
	}
	c.state = para
	if para == OrderStateConverting || para == OrderStateMutating {
		c.operacaoEm = time.Now()
	}
	r.transicoesGanhas++
	return true, nil
}

func (r *repoSimulado) UpdateCartExternalOrderID(_ context.Context, cartID, orderID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.carrinhos[cartID]; ok {
		c.externalOrderID = orderID
	}
	return nil
}

func (r *repoSimulado) SetCartERPStockLaunched(_ context.Context, cartID string, v bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.carrinhos[cartID]; ok {
		c.stockLaunched = v
	}
	return nil
}

func (r *repoSimulado) MarkCartERPFinalisationDone(context.Context, string) error { return nil }

func (r *repoSimulado) ListNonWaitlistedCartItems(_ context.Context, cartID string) ([]NonWaitlistedCartItem, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.carrinhos[cartID]
	if !ok {
		return nil, nil
	}
	out := make([]NonWaitlistedCartItem, len(c.itens))
	copy(out, c.itens)
	return out, nil
}

func (r *repoSimulado) ListStuckERPOrderOps(context.Context, time.Duration) ([]StuckERPOrderOp, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]StuckERPOrderOp, len(r.presos))
	copy(out, r.presos)
	for i := range out {
		if c, ok := r.carrinhos[out[i].CartID]; ok {
			out[i].State = c.state
			out[i].ExternalOrderID = c.externalOrderID
		}
	}
	return out, nil
}

func (r *repoSimulado) ListTinyIntegrationsWithStaleStockWebhook(context.Context, time.Duration) ([]StaleStockWebhookIntegration, error) {
	return nil, nil
}
func (r *repoSimulado) StampIntegrationStockWebhookAlert(context.Context, string) error { return nil }
func (r *repoSimulado) GetCartInvoiceAnchor(context.Context, string) (string, string, error) {
	return "", "", nil
}
func (r *repoSimulado) UpsertCartERPInvoice(context.Context, UpsertCartERPInvoiceParams) (int64, error) {
	return 0, nil
}

func (r *repoSimulado) FindCartByExternalOrderID(_ context.Context, orderID, _ string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, c := range r.carrinhos {
		if c.externalOrderID == orderID {
			return id, nil
		}
	}
	return "", pgx.ErrNoRows
}

func (r *repoSimulado) GetShipmentByOrderID(context.Context, string) (*ShipmentInvoiceRef, error) {
	return nil, nil
}
func (r *repoSimulado) UpdateShipmentInvoice(context.Context, string, string, string) error {
	return nil
}

// --- ERPOrderStatusRepository ----------------------------------------------

// RecordOrderStatus resolve o carrinho pelo id do PEDIDO, como a query real —
// resolver pelo cart_id do chamador esconderia justamente a corrida que a query
// existe para fechar.
func (r *repoSimulado) RecordOrderStatus(_ context.Context, obs ERPOrderStatusObservation) (ERPOrderStatusTransition, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var cartID string
	var c *carrinhoSimulado
	for id, cand := range r.carrinhos {
		if cand.externalOrderID == obs.ExternalOrderID {
			cartID, c = id, cand
			break
		}
	}
	if c == nil {
		// Pedido que não é de nenhum carrinho nosso: guarda a passagem avulsa.
		r.statusEventos = append(r.statusEventos, obs)
		return ERPOrderStatusTransition{Status: string(obs.Status)}, true, nil
	}
	if c.statusERP == string(obs.Status) {
		return ERPOrderStatusTransition{}, false, nil
	}
	anterior := c.statusERP
	c.statusERP = string(obs.Status)
	c.statusEm = time.Now()
	if obs.OrderNumber != "" {
		c.numeroPedido = obs.OrderNumber
	}
	obs.CartID = cartID
	r.statusEventos = append(r.statusEventos, obs)
	return ERPOrderStatusTransition{
		CartID: cartID, PreviousStatus: anterior,
		Status: string(obs.Status), ObservedAt: c.statusEm,
	}, true, nil
}

func (r *repoSimulado) AdoptOrphanOrderStatusEvents(_ context.Context, cartID, externalOrderID string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for i := range r.statusEventos {
		if r.statusEventos[i].CartID == "" && r.statusEventos[i].ExternalOrderID == externalOrderID {
			r.statusEventos[i].CartID = cartID
			n++
		}
	}
	return n, nil
}

func (r *repoSimulado) ListStaleOrderStatuses(_ context.Context, _ time.Duration, _ int) ([]StaleERPOrderStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []StaleERPOrderStatus{}
	for id, c := range r.carrinhos {
		st := providers.ERPOrderStatus(c.statusERP)
		if c.statusERP == "" || st.Terminal() || c.externalOrderID == "" {
			continue
		}
		out = append(out, StaleERPOrderStatus{
			CartID: id, StoreID: "loja-1", ExternalOrderID: c.externalOrderID, Status: c.statusERP,
		})
	}
	return out, nil
}

// --- StockCollaborators -----------------------------------------------------

type colabSimulado struct {
	// erp é a interface, não o tipo concreto: a drenagem embrulha o simulador
	// para acrescentar o estorno da saída manual, e o colaborador precisa
	// devolver ESSE objeto — senão a criação do pedido e o estorno falariam com
	// ERPs diferentes, e a sequência que os testes afirmam deixaria de existir.
	erp  providers.ERPProvider
	repo *repoSimulado

	mu             sync.Mutex
	espelhos       int
	finalizados    int
	cancelados     int
	falhasMarcadas []string
	semVinculo     bool
}

func (c *colabSimulado) ResolveProvider(context.Context, *Integration) (providers.ERPProvider, error) {
	return c.erp, nil
}

func (c *colabSimulado) ResolveExternalProduct(_ context.Context, _, produtoID string) (string, bool) {
	if c.semVinculo {
		return "", false
	}
	return "ext-" + produtoID, true
}

func (c *colabSimulado) ResolveERPContact(context.Context, providers.ERPProvider, *Integration, string, string, string, string, string, string, string) (string, error) {
	return "contato-1", nil
}

// CreateERPOrderForCart faz o que o colaborador de verdade faz: monta a grade a
// partir do banco, cria o pedido e grava o external_order_id.
func (c *colabSimulado) CreateERPOrderForCart(ctx context.Context, p providers.ERPProvider, _ *Integration, _, cartID string) ([]providers.ERPOrderItem, error) {
	itens, _ := c.repo.ListNonWaitlistedCartItems(ctx, cartID)
	grade := make([]providers.ERPOrderItem, 0, len(itens))
	for _, it := range itens {
		if it.ProductExternalID == "" {
			continue
		}
		grade = append(grade, providers.ERPOrderItem{
			ProductID: it.ProductExternalID, Name: it.ProductName,
			Quantity: it.Quantity, UnitPrice: it.UnitPrice,
			// Marca a linha, como o colaborador de verdade faz. Um duplo que
			// esquecesse disso faria as próprias linhas parecerem do lojista na
			// primeira mutação.
			Note: providers.LiveCartItemMarker + " " + it.ProductKeyword,
		})
	}
	if len(grade) == 0 {
		return nil, nil // carrinho sem item vinculado: nada a criar, e nada gravado
	}
	res, err := p.CreateOrder(ctx, providers.ERPOrder{ExternalID: cartID, ContactID: "contato-1", Items: grade})
	if err != nil {
		return nil, err
	}
	return grade, c.repo.UpdateCartExternalOrderID(ctx, cartID, res.OrderID)
}

func (c *colabSimulado) MirrorToOrder(context.Context, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.espelhos++
}

func (c *colabSimulado) MarkFinalisationFailed(_ context.Context, _, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.falhasMarcadas = append(c.falhasMarcadas, msg)
}

func (c *colabSimulado) EmitERPOrderFinalized(context.Context, string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finalizados++
}

func (c *colabSimulado) EmitERPOrderCancelled(context.Context, string, string, string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelados++
}
func (c *colabSimulado) ResolveERPProviderByID(context.Context, string, string) (providers.ERPProvider, error) {
	return c.erp, nil
}
func (c *colabSimulado) HandleProviderError(context.Context, string, string, error) {}

var (
	_ ERPRepository            = (*repoSimulado)(nil)
	_ ERPOrderStatusRepository = (*repoSimulado)(nil)
	_ StockCollaborators       = (*colabSimulado)(nil)
)

// pagarCarrinho encena o pagamento como a 000140 o grava: soma ao carrinho o que
// ainda não estava pago e carimba as unidades que aquele dinheiro cobriu.
//
// Repetir a chamada não soma de novo — não porque o teste seja gentil, mas
// porque o `paid_quantity < quantity` da query também não deixa.
func (r *repoSimulado) pagarCarrinho(cartID string, quando time.Time) int64 {
	return r.cobrar(cartID, quando, -1, "pix")
}

// cobrar encena uma cobrança como a query de pagamento a grava: carimba as
// unidades que ela liquida, guarda o BRUTO que elas valiam e o que de fato
// entrou. cobradoCents negativo significa "sem desconto" — entrou o preço cheio.
//
// Repetir com o mesmo checkout id não soma de novo, pelo mesmo motivo que a
// query não soma: reentrega de webhook não é segundo pagamento.
func (r *repoSimulado) cobrar(cartID string, quando time.Time, cobradoCents int64, metodo string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.carrinhos[cartID]
	if c == nil {
		return 0
	}
	if c.pagoQtd == nil {
		c.pagoQtd = map[string]int{}
	}
	var bruto int64
	for i, it := range c.itens {
		falta := it.Quantity - c.pagoQtd[it.ProductID]
		if falta <= 0 {
			continue
		}
		bruto += int64(falta) * it.UnitPrice
		c.pagoQtd[it.ProductID] = it.Quantity
		_ = i
	}
	entrou := cobradoCents
	if entrou < 0 {
		entrou = bruto
	}
	c.pagoCents += entrou
	c.pagoEm = quando
	c.livro = append(c.livro, CartPayment{
		AmountCents:       entrou,
		GrossCoveredCents: bruto,
		Method:            metodo,
		CheckoutID:        fmt.Sprintf("pay-%d", len(c.livro)+1),
		PaidAt:            quando,
	})
	return entrou
}

// ListCartPayments satisfaz erp.CartPaymentLedger.
func (r *repoSimulado) ListCartPayments(_ context.Context, cartID string) ([]CartPayment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.carrinhos[cartID]
	if c == nil {
		return nil, nil
	}
	return append([]CartPayment(nil), c.livro...), nil
}

// faltaPagar é cart_unpaid_total_cents: o que o carrinho ainda deve.
func (r *repoSimulado) faltaPagar(cartID string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.carrinhos[cartID]
	if c == nil {
		return 0
	}
	var falta int64
	for _, it := range c.itens {
		if resto := it.Quantity - c.pagoQtd[it.ProductID]; resto > 0 {
			falta += int64(resto) * it.UnitPrice
		}
	}
	return falta
}

// acrescentarItem encena o comentário que chega: soma na linha do produto se ela
// já existe, como o ON CONFLICT (cart_id, product_id) do upsert faz.
func (r *repoSimulado) acrescentarItem(cartID string, novo NonWaitlistedCartItem) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.carrinhos[cartID]
	if c == nil {
		return
	}
	for i := range c.itens {
		if c.itens[i].ProductID == novo.ProductID {
			c.itens[i].Quantity += novo.Quantity
			return
		}
	}
	c.itens = append(c.itens, novo)
}

// quantidadeNoCarrinho é quanto o banco diz que o carrinho tem daquele produto.
func (r *repoSimulado) quantidadeNoCarrinho(cartID, produtoID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.carrinhos[cartID]
	if c == nil {
		return 0
	}
	for _, it := range c.itens {
		if it.ProductID == produtoID {
			return it.Quantity
		}
	}
	return 0
}

// definirStatusERP encena a situação que o webhook de vendas deixou no carrinho.
func (r *repoSimulado) definirStatusERP(cartID, situacao string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.carrinhos[cartID]; c != nil {
		c.statusERP = situacao
		c.statusEm = time.Now()
	}
}

// esvaziarCarrinho encena o que a consolidação faz com o carrinho de origem:
// os itens já foram para o eterno e a linha fica sem nada.
func (r *repoSimulado) esvaziarCarrinho(cartID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.carrinhos[cartID]; c != nil {
		c.itens = nil
	}
}

// ListERPLinkedProductsSample: a amostra que a checagem do módulo de Reserva lê.
func (r *repoSimulado) ListERPLinkedProductsSample(_ context.Context, _ string, limite int) ([]ERPLinkedProduct, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ERPLinkedProduct, 0, limite)
	for id, qtd := range r.estoque {
		if qtd <= 0 || len(out) >= limite {
			continue
		}
		out = append(out, ERPLinkedProduct{ID: id, Name: "Produto " + id, ExternalID: "ext-" + id})
	}
	return out, nil
}

// CartIsTerminated: o carrinho chegou a um fim de onde não sai sozinho.
func (r *repoSimulado) CartIsTerminated(_ context.Context, cartID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.carrinhos[cartID]
	if c == nil {
		return false, nil
	}
	return c.state == OrderStateCancelled, nil
}

// ListCartGridItems: a grade do grupo. No simulado não há junção, então é a
// lista do próprio carrinho.
func (r *repoSimulado) ListCartGridItems(ctx context.Context, cartID string) ([]NonWaitlistedCartItem, error) {
	return r.ListNonWaitlistedCartItems(ctx, cartID)
}
