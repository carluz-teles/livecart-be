package erp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

// Pedidos de venda no Bling.
//
// Duas ausências moldam este arquivo, e as duas foram verificadas nas 257
// operações do spec:
//
//  1. NÃO existe `PUT /pedidos/vendas/{id}/itens`. Mudar a grade custa
//     GET + PUT do pedido INTEIRO — o dobro do consumo do caminho quente de uma
//     live contra um teto de 3 req/s.
//  2. NÃO existe 409 no POST (as respostas declaradas são 201 e 400). O
//     `adoptExistingOrder` do Tiny se apoia num 409 que aqui não acontece, então
//     a defesa contra pedido duplicado é o CLAIM por âncora ANTES do POST —
//     possível porque `numerosLojas[]` filtra de verdade (medido), ao contrário
//     do `?numeroOrdemCompra=` do Tiny, que é ignorado em silêncio.

// blingMarcador é a âncora que liga o pedido do Bling ao carrinho do LiveCart.
// Vai em `numeroLoja`, que é string livre — e MEDIDO: volta intacto na leitura.
func blingMarcador(cartID string) string { return "lc-cart-" + cartID }

type blingItemPedido struct {
	ID         int64   `json:"id,omitempty"`
	Codigo     string  `json:"codigo,omitempty"`
	Unidade    string  `json:"unidade,omitempty"`
	Quantidade float64 `json:"quantidade"`
	Valor      float64 `json:"valor"`
	Descricao  string  `json:"descricao"`
	Produto    struct {
		ID int64 `json:"id"`
	} `json:"produto"`
	DescricaoDetalhada string `json:"descricaoDetalhada,omitempty"`
}

type blingParcela struct {
	ID             int64   `json:"id,omitempty"`
	DataVencimento string  `json:"dataVencimento"`
	Valor          float64 `json:"valor"`
	Observacoes    string  `json:"observacoes,omitempty"`
	FormaPagamento struct {
		ID int64 `json:"id"`
	} `json:"formaPagamento"`
}

type blingPedido struct {
	ID            int64             `json:"id,omitempty"`
	Numero        int64             `json:"numero,omitempty"`
	NumeroLoja    string            `json:"numeroLoja,omitempty"`
	Data          string            `json:"data"`
	DataSaida     string            `json:"dataSaida"`
	DataPrevista  string            `json:"dataPrevista"`
	Total         float64           `json:"total,omitempty"`
	TotalProdutos float64           `json:"totalProdutos,omitempty"`
	Observacoes   string            `json:"observacoes,omitempty"`
	Contato       blingRefContato   `json:"contato"`
	Situacao      *blingSituacaoRef `json:"situacao,omitempty"`
	NotaFiscal    *struct {
		ID int64 `json:"id"`
	} `json:"notaFiscal,omitempty"`
	Itens    []blingItemPedido `json:"itens"`
	Parcelas []blingParcela    `json:"parcelas"`
}

type blingRefContato struct {
	ID   int64  `json:"id"`
	Nome string `json:"nome,omitempty"`
}

type blingSituacaoRef struct {
	ID    int64 `json:"id"`
	Valor int   `json:"valor,omitempty"`
}

// CreateOrder cria o pedido de venda.
//
// Obrigatórios MEDIDOS (o 201 saiu com exatamente estes): `contato{id}`,
// `data`, `dataSaida`, `dataPrevista`, `itens[]` e `parcelas[]`.
//
// ⚠ `valorLista` aparece como `required` no schema de item, mas NÃO existe como
// property em lugar nenhum do spec — e o pedido de teste foi criado sem ele.
// Medido: o validador não o exige. Mandá-lo seria inventar campo.
//
// As datas vão em horário de São Paulo pelo mesmo motivo do Tiny: em UTC, um
// pedido feito à noite cai no dia seguinte e some do filtro do lojista.
func (b *Bling) CreateOrder(ctx context.Context, order providers.ERPOrder) (*providers.OrderResult, error) {
	marcador := blingMarcador(order.ExternalID)

	// CLAIM ANTES DO POST. Sem 409, é a única defesa contra duplicata: se uma
	// tentativa anterior morreu DEPOIS de o Bling gravar, o pedido já existe e
	// criar outro venderia a mesma peça duas vezes.
	if existente, err := b.FindOrderIDByMarker(ctx, marcador); err == nil && existente != "" {
		return &providers.OrderResult{OrderID: existente, Status: "adopted"}, nil
	}

	contatoID, err := strconv.ParseInt(order.ContactID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("bling: contato inválido %q: %w", order.ContactID, err)
	}

	// `parcelas[]` é obrigatório e cada parcela exige `formaPagamento.id`, que é
	// POR CONTA. Resolvido uma vez e cacheado; sem forma conhecida o pedido não
	// nasce — e falhar aqui é melhor do que criar pedido com forma errada, que o
	// lojista só descobre no fechamento do caixa.
	formaPagamento, err := b.formaPagamentoPadrao(ctx)
	if err != nil {
		return nil, err
	}

	agora := time.Now().In(blingLocation)
	p := blingPedido{
		NumeroLoja:   marcador,
		Data:         agora.Format("2006-01-02"),
		DataSaida:    agora.Format("2006-01-02"),
		DataPrevista: agora.AddDate(0, 0, 7).Format("2006-01-02"),
		Observacoes:  order.Observation,
		Contato:      blingRefContato{ID: contatoID},
		Itens:        blingItens(order.Items),
		Parcelas:     blingParcelasDe(order, agora, formaPagamento),
	}

	var env struct {
		Data struct {
			ID      int64    `json:"id"`
			Alertas []string `json:"alertas"`
		} `json:"data"`
	}
	if err := b.escrever(ctx, http.MethodPost, "/pedidos/vendas", p, &env); err != nil {
		return nil, err
	}
	// ⚠ O 201 NÃO devolve `numero` (medido) — só id, alertas e rastreamento.
	// O número humano do pedido só aparece na primeira leitura. É diferença
	// visível de UX em relação ao Tiny e tem de ser dita ao lojista.
	return &providers.OrderResult{
		OrderID: strconv.FormatInt(env.Data.ID, 10),
		Status:  "created",
	}, nil
}

func blingItens(itens []providers.ERPOrderItem) []blingItemPedido {
	out := make([]blingItemPedido, 0, len(itens))
	for _, it := range itens {
		var b blingItemPedido
		id, _ := strconv.ParseInt(it.ProductID, 10, 64)
		b.Produto.ID = id
		b.Quantidade = float64(it.Quantity)
		b.Valor = float64(it.UnitPrice) / 100
		b.Descricao = it.Name
		b.DescricaoDetalhada = it.Note
		if b.DescricaoDetalhada == "" {
			// Toda linha nossa carrega o marcador: o PUT substitui a grade
			// inteira, e sem ele não há como separar o que é nosso do que o
			// lojista digitou à mão — que precisa ser reenviado junto.
			b.DescricaoDetalhada = providers.LiveCartItemMarker
		}
		out = append(out, b)
	}
	return out
}

// blingParcelasDe monta as parcelas. `parcelas[]` é OBRIGATÓRIO no POST, e o
// pedido nasce no primeiro comentário — antes de existir pagamento. A parcela
// inicial é o compromisso, não o dinheiro recebido; o valor real entra na
// confirmação.
func blingParcelasDe(order providers.ERPOrder, agora time.Time, formaPagamento int64) []blingParcela {
	var total float64
	for _, it := range order.Items {
		total += float64(it.UnitPrice) / 100 * float64(it.Quantity)
	}
	var p blingParcela
	p.DataVencimento = agora.AddDate(0, 0, 7).Format("2006-01-02")
	p.Valor = total
	p.FormaPagamento.ID = formaPagamento
	return []blingParcela{p}
}

// FindOrderIDByMarker reencontra o pedido pela âncora.
//
// `numerosLojas[]` filtra DE VERDADE (medido) — ao contrário do
// `?numeroOrdemCompra=` do Tiny, que é ignorado em silêncio e por isso nunca
// serviu de claim. É o que torna o desfecho ambíguo resolvível com UM GET, em
// vez de retry cego.
func (b *Bling) FindOrderIDByMarker(ctx context.Context, marker string) (string, error) {
	q := url.Values{}
	q.Add("numerosLojas[]", marker)
	q.Set("limite", "2")

	var env struct {
		Data []blingPedido `json:"data"`
	}
	if err := b.get(ctx, "/pedidos/vendas", q, &env); err != nil {
		return "", err
	}
	for _, p := range env.Data {
		if p.Situacao != nil {
			// De graça, e no melhor momento possível: este GET precede TODA
			// criação de pedido, então a tabela já está corroborada (ou
			// desmentida) antes da primeira escrita de situação da live.
			b.corroborarTabelaPadrao(p.Situacao.ID, p.Situacao.Valor)
		}
		// Conferência explícita: o filtro é do servidor, e confiar nele sem
		// checar o que voltou é como se adota o pedido errado.
		if p.NumeroLoja == marker {
			return strconv.FormatInt(p.ID, 10), nil
		}
	}
	return "", nil
}

func (b *Bling) pedido(ctx context.Context, orderID string) (*blingPedido, map[string]any, error) {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := b.get(ctx, "/pedidos/vendas/"+url.PathEscape(orderID), nil, &env); err != nil {
		return nil, nil, err
	}
	var tipado blingPedido
	if err := json.Unmarshal(env.Data, &tipado); err != nil {
		return nil, nil, fmt.Errorf("bling: pedido ilegível: %w", err)
	}
	// O documento CRU é devolvido junto porque a mutação precisa dele: o PUT
	// substitui o pedido inteiro, e reconstruir o corpo a partir de um struct
	// tipado apagaria todo bloco que ainda não modelamos.
	var cru map[string]any
	if err := json.Unmarshal(env.Data, &cru); err != nil {
		return nil, nil, err
	}
	return &tipado, cru, nil
}

func (b *Bling) GetOrderTotal(ctx context.Context, orderID string) (int64, bool, error) {
	p, _, err := b.pedido(ctx, orderID)
	if err != nil {
		return 0, false, err
	}
	temNota := p.NotaFiscal != nil && p.NotaFiscal.ID != 0
	return int64(p.Total*100 + 0.5), temNota, nil
}

func (b *Bling) GetOrderItems(ctx context.Context, orderID string) ([]providers.ERPOrderItem, error) {
	p, _, err := b.pedido(ctx, orderID)
	if err != nil {
		return nil, err
	}
	out := make([]providers.ERPOrderItem, 0, len(p.Itens))
	for _, it := range p.Itens {
		out = append(out, providers.ERPOrderItem{
			ProductID: strconv.FormatInt(it.Produto.ID, 10),
			Quantity:  int(it.Quantidade),
			UnitPrice: int64(it.Valor*100 + 0.5),
			Name:      it.Descricao,
			Note:      it.DescricaoDetalhada,
		})
	}
	return out, nil
}

// UpdateOrderItems substitui a grade — GET + PUT do documento CRU.
//
// O corpo do PUT SAI DO DOCUMENTO LIDO no mesmo instante e sobrescreve APENAS
// `itens`. Nunca de um DTO tipado: o corpo tem 25 blocos de topo, e um struct
// que não modela `transporte`, `desconto`, `categoria`, `vendedor`, `taxas`,
// `intermediador` os apagaria com HTTP 200 e em silêncio.
func (b *Bling) UpdateOrderItems(ctx context.Context, orderID string, itens []providers.ERPOrderItem) error {
	_, cru, err := b.pedido(ctx, orderID)
	if err != nil {
		return err
	}

	novos := make([]any, 0, len(itens))
	for _, it := range blingItens(itens) {
		m := map[string]any{
			"produto":    map[string]any{"id": it.Produto.ID},
			"quantidade": it.Quantidade,
			"valor":      it.Valor,
			"descricao":  it.Descricao,
		}
		if it.DescricaoDetalhada != "" {
			m["descricaoDetalhada"] = it.DescricaoDetalhada
		}
		novos = append(novos, m)
	}
	cru["itens"] = novos
	limparReadOnly(cru)

	return b.escrever(ctx, http.MethodPut, "/pedidos/vendas/"+url.PathEscape(orderID), cru, nil)
}

// limparReadOnly tira do corpo os campos que o spec marca `readOnly`.
//
// Em OAS 3.0 `readOnly` significa "SHOULD NOT be sent in a request", e o eco
// cru do GET carrega todos eles. Mandar de volta é, na melhor hipótese,
// ignorado; na pior, 400 no meio de uma live.
func limparReadOnly(cru map[string]any) {
	for _, k := range []string{"id", "numero", "total", "totalProdutos", "situacao", "notaFiscal"} {
		delete(cru, k)
	}
	if itens, ok := cru["itens"].([]any); ok {
		for _, it := range itens {
			if m, ok := it.(map[string]any); ok {
				delete(m, "id")
			}
		}
	}
	if c, ok := cru["contato"].(map[string]any); ok {
		delete(c, "nome")
	}
}

// SetOrderSituacao muda a situação do pedido.
//
// ⚠ O id da situação é POR CONTA no Bling — o lojista cria as dele em
// POST /situacoes. Escrever um id fixo na conta errada pode significar
// "entregue" em vez de "aprovado", ou disparar uma transição com efeito de
// estoque (GET /situacoes/modulos/{id}/transicoes devolve `acoes[]`, e o spec
// traz o exemplo literal `estornarEstoque`).
//
// Por isso a tradução passa pelo mapa da conta, guardado no metadata da
// integração no momento da conexão. Sem entrada no mapa a operação RECUSA e
// gasta ZERO requisição — nunca chuta um id.
func (b *Bling) SetOrderSituacao(ctx context.Context, orderID string, situacao int) error {
	idSituacao, ok := b.situacaoDaConta(situacao)
	if !ok {
		// UMA leitura do próprio pedido que se vai mudar, e só quando o id é
		// desconhecido. É a corroboração no único momento em que ela tem de
		// existir: o pedido está em ALGUMA situação, e o par (id, valor) dela
		// confirma ou desmente a tabela semeada.
		//
		// O GET de claim não serve para isto — ele filtra pelo marcador e, num
		// carrinho novo, volta vazio. Descobrir isso custou uma execução do
		// ensaio contra a conta real.
		if _, err := b.GetOrderSituacao(ctx, orderID); err != nil {
			return fmt.Errorf("bling: lendo a situação atual do pedido %s para "+
				"descobrir os ids desta conta: %w", orderID, err)
		}
		idSituacao, ok = b.situacaoDaConta(situacao)
	}
	if !ok {
		return fmt.Errorf("%w: situação %d não está mapeada para esta conta Bling — "+
			"os ids de situação são por conta e escrever um id não mapeado pode "+
			"disparar ação de estoque na conta do lojista",
			providers.ErrOperationNotSupported, situacao)
	}
	caminho := fmt.Sprintf("/pedidos/vendas/%s/situacoes/%d", url.PathEscape(orderID), idSituacao)
	return b.escrever(ctx, http.MethodPatch, caminho, nil, nil)
}

// blingSituacoesPadrao é a tabela que o Bling SEMEIA em toda conta nova para o
// módulo de pedidos de venda. MEDIDA em 30/08/2026, passando um pedido
// descartável por todas elas e lendo `situacao` de volta a cada transição:
//
//	id  valor  nome              reserva estoque?
//	 6    0    Em aberto         SIM   (virtual 4 de 5)
//	 9    1    Atendido          não   (virtual volta a 5)
//	12    2    Cancelado         não
//	15    3    Em andamento      SIM
//	18    4    Venda agenciada   não
//	21   10    Em digitação      SIM
//	24   11    Verificado        SIM
//
// O FÍSICO não se mexeu em nenhuma das sete transições — mudar situação nunca
// dá baixa, que é a propriedade de que este modelo inteiro depende.
//
// A tabela é só um PALPITE INFORMADO até ser corroborada contra a conta, porque
// o lojista pode criar situações próprias, e escrever um id que significa outra
// coisa mexeria no estoque dele. Ver corroborarTabelaPadrao.
var blingSituacoesPadrao = map[int]int64{
	providers.SituacaoAberta:    6,
	providers.SituacaoFaturada:  9,
	providers.SituacaoCancelada: 12,
	// ⚠ O par que exige julgamento: o Bling não tem "Aprovada". "Em andamento"
	// é o destino honesto de um pedido cujo pagamento entrou — continua
	// reservando (medido) e deixa de ser só "em aberto".
	//
	// É a direção de ESCRITA e só ela. Na LEITURA a tradução inversa continua
	// recusada de propósito (situacaoCanonicaDoValor), porque "em andamento" no
	// Bling não afirma que a venda foi aprovada: nós é que sabemos que foi.
	providers.SituacaoAprovada: 15,
}

// blingValorDaSituacaoPadrao é a mesma tabela pela outra chave: id → valor
// normalizado. É com ela que uma leitura qualquer confirma ou desmente que esta
// conta usa os ids semeados.
var blingValorDaSituacaoPadrao = map[int64]int{
	6: 0, 9: 1, 12: 2, 15: 3, 18: 4, 21: 10, 24: 11,
}

// corroborarTabelaPadrao confere um par (id, valor) observado na conta contra a
// tabela semeada.
//
// Um par que BATE autoriza usar a tabela para escrever; um par que CONTRADIZ a
// desliga para sempre nesta sessão do adapter. É o que separa "sei porque vi"
// de "chutei um id no ERP do lojista": todo pedido lido — inclusive o GET de
// claim que precede toda criação — passa por aqui de graça.
func (b *Bling) corroborarTabelaPadrao(id int64, valor int) {
	if id == 0 {
		return
	}
	esperado, conhecido := blingValorDaSituacaoPadrao[id]
	if !conhecido {
		// Situação criada pelo lojista. Não diz nada sobre as semeadas.
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if esperado == valor {
		b.padraoConfirmado = true
		return
	}
	// Esta conta reaproveitou um id semeado para outra coisa. A tabela inteira
	// vira suspeita — escrever qualquer id dela poderia disparar a transição
	// errada.
	b.padraoContradito = true
}

// situacaoDaConta traduz o código canônico (o enum do Tiny, que o núcleo usa)
// no id daquela conta Bling.
//
// Três fontes, nesta ordem: o que já foi OBSERVADO nesta conta, a tabela
// semeada quando uma leitura a corroborou, e nada. "Nada" faz a escrita
// recusar, que é o comportamento seguro.
func (b *Bling) situacaoDaConta(canonico int) (int64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id, ok := b.mapaSituacoes[canonico]; ok {
		return id, true
	}
	if b.padraoConfirmado && !b.padraoContradito {
		id, ok := blingSituacoesPadrao[canonico]
		return id, ok
	}
	return 0, false
}

// Valores NORMALIZADOS de situação de venda do Bling.
//
// ⭐ `situacao.valor` é independente de conta — é o Bling que o deriva da
// situação que o lojista configurou, e o spec o marca "Ignorado no método POST"
// (é leitura). Isso resolve a LEITURA de situação sem mapa nenhum: o `id` varia
// entre contas, o `valor` não.
//
// Medido em 29/08/2026: um pedido novo nasceu com {id:6, valor:0} e o DELETE o
// levou a {id:12, valor:2} antes de apagar — batendo com o enum do spec.
const (
	blingValorEmAberto        = 0
	blingValorAtendido        = 1
	blingValorCancelado       = 2
	blingValorEmAndamento     = 3
	blingValorFaturadoParcial = 5
	blingValorAtendidoParcial = 6
	blingValorAguardandoPagto = 7
)

// situacaoCanonicaDoValor traduz o valor normalizado do Bling no código
// canônico que o núcleo usa (o enum do Tiny).
//
// Devolve 0 e false quando não há correspondência — "não sei" explícito, que o
// núcleo já trata. Chutar aqui faria a varredura de reconciliação parar num
// estágio errado, em silêncio.
// ⚠ A tradução é EXPLÍCITA, e não pela coincidência numérica.
//
// Os dois enums se sobrepõem em parte — e a sobreposição é ARMADILHA: o valor 3
// no Bling é "Em andamento", e o 3 do núcleo (herdado do Tiny) é "Aprovada".
// São coisas diferentes: "em andamento" não diz que a venda foi aprovada, e
// tratar uma pela outra faria o LiveCart dar por fechada uma venda que não está.
//
// Onde não há análogo honesto, devolve false — "não sei" explícito, que o núcleo
// já trata. Arredondar seria pior do que admitir.
func situacaoCanonicaDoValor(valor int) (int, bool) {
	switch valor {
	case blingValorEmAberto:
		return providers.SituacaoAberta, true
	case blingValorCancelado:
		return providers.SituacaoCancelada, true
	case blingValorAtendido:
		// "Atendido" no Bling é a venda concluída — o análogo mais próximo do
		// "Faturada" do núcleo. É o único par que exige julgamento; os dois
		// acima são idênticos em significado.
		return providers.SituacaoFaturada, true
	case blingValorEmAndamento, blingValorAguardandoPagto,
		blingValorFaturadoParcial, blingValorAtendidoParcial:
		// Sem análogo no enum do núcleo. "Em andamento" não é "aprovada", e os
		// parciais não são "atendido" — arredondar para cima faria o LiveCart
		// concluir uma venda que ainda tem item para sair.
		return 0, false
	default:
		return 0, false
	}
}

// GetOrderSituacao lê a situação do pedido.
//
// Usa `situacao.valor` e NÃO o `id`: o valor é normalizado pelo Bling e vale em
// qualquer conta, enquanto o id é do lojista. Sem isso, ler situação exigiria o
// mapa por conta — que depende do escopo de Situações, hoje 403 nesta conta.
func (b *Bling) GetOrderSituacao(ctx context.Context, orderID string) (int, error) {
	p, _, err := b.pedido(ctx, orderID)
	if err != nil {
		return 0, err
	}
	if p.Situacao == nil {
		return 0, nil
	}
	b.corroborarTabelaPadrao(p.Situacao.ID, p.Situacao.Valor)
	if canonico, ok := situacaoCanonicaDoValor(p.Situacao.Valor); ok {
		// Aprende o id daquela conta de graça, para a ESCRITA poder usá-lo
		// depois sem precisar do escopo de Situações.
		b.aprenderSituacao(canonico, p.Situacao.ID)
		return canonico, nil
	}
	return 0, nil
}

// aprenderSituacao guarda o par (canônico → id da conta) observado numa leitura.
//
// É como o adapter monta o mapa de escrita sem o escopo de Situações: todo
// pedido lido ensina um id. Não substitui um mapa explícito, mas transforma
// "recuso escrever porque não sei" em "sei, porque já vi".
func (b *Bling) aprenderSituacao(canonico int, id int64) {
	if id == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mapaSituacoes == nil {
		b.mapaSituacoes = map[int]int64{}
	}
	if _, jaSabe := b.mapaSituacoes[canonico]; !jaSabe {
		b.mapaSituacoes[canonico] = id
	}
}

// =============================================================================
// CONTATOS
// =============================================================================

type blingContato struct {
	ID              int64  `json:"id"`
	Nome            string `json:"nome"`
	NumeroDocumento string `json:"numeroDocumento"`
	Email           string `json:"email"`
	Celular         string `json:"celular"`
	Telefone        string `json:"telefone"`
}

// SearchContacts busca contato.
//
// ⚠ INVERSO do Tiny: aqui o documento vai com DÍGITO CRU. O adapter do Tiny
// FORMATA com máscara de propósito, porque lá dígito cru devolve zero resultado
// e faz criar um duplicado do contato que se estava procurando. Herdar a
// normalização do Tiny reproduziria o mesmo bug, invertido.
func (b *Bling) SearchContacts(ctx context.Context, params providers.SearchContactsParams) ([]providers.ERPContactResult, error) {
	q := url.Values{}
	if d := somenteDigitos(params.CpfCnpj); d != "" {
		q.Set("numeroDocumento", d)
	} else if params.Name != "" {
		q.Set("pesquisa", params.Name)
	} else {
		return nil, nil
	}
	q.Set("limite", "10")

	var env struct {
		Data []blingContato `json:"data"`
	}
	if err := b.get(ctx, "/contatos", q, &env); err != nil {
		return nil, err
	}
	out := make([]providers.ERPContactResult, 0, len(env.Data))
	for _, c := range env.Data {
		out = append(out, providers.ERPContactResult{
			ContactID: strconv.FormatInt(c.ID, 10),
			Name:      c.Nome,
		})
	}
	return out, nil
}

func (b *Bling) CreateContact(ctx context.Context, contato providers.ERPContactInput) (*providers.ERPContactResult, error) {
	tipo := contato.PersonType
	if tipo == "" {
		tipo = "F"
	}
	corpo := map[string]any{
		"nome":     contato.Name,
		"tipo":     tipo,
		"situacao": "A",
	}
	if d := somenteDigitos(contato.CpfCnpj); d != "" {
		corpo["numeroDocumento"] = d
	}
	if contato.Email != "" {
		corpo["email"] = contato.Email
	}
	if contato.Phone != "" {
		corpo["celular"] = contato.Phone
	}

	var env struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	if err := b.escrever(ctx, http.MethodPost, "/contatos", corpo, &env); err != nil {
		return nil, err
	}
	return &providers.ERPContactResult{
		ContactID: strconv.FormatInt(env.Data.ID, 10),
		Name:      contato.Name,
	}, nil
}

func (b *Bling) UpdateContact(ctx context.Context, contactID string, contato providers.ERPContactInput) error {
	corpo := map[string]any{"nome": contato.Name}
	if d := somenteDigitos(contato.CpfCnpj); d != "" {
		corpo["numeroDocumento"] = d
	}
	if contato.Email != "" {
		corpo["email"] = contato.Email
	}
	if contato.Phone != "" {
		corpo["celular"] = contato.Phone
	}
	return b.escrever(ctx, http.MethodPut, "/contatos/"+url.PathEscape(contactID), corpo, nil)
}

func somenteDigitos(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// =============================================================================
// OPERAÇÕES QUE O BLING NÃO TEM — recusa explícita, nunca silêncio
// =============================================================================

// ApproveOrder não existe como conceito próprio no Bling: aprovar é MUDAR A
// SITUAÇÃO, e a situação é por conta. Quem quiser aprovar chama SetOrderSituacao
// com o código canônico, que passa pelo mapa da conta.
func (b *Bling) ApproveOrder(ctx context.Context, orderID string) error {
	return fmt.Errorf("%w: no Bling aprovar é mudar a situação — use SetOrderSituacao",
		providers.ErrOperationNotSupported)
}

// ReverseOrderStock: o LiveCart NÃO escreve movimento de estoque no Bling.
//
// Decisão de arquitetura, não limitação da API. O Bling reserva NATIVAMENTE
// quando a conta tem "Considerar situações de vendas para obter o saldo atual"
// ligada — a reserva é efeito colateral da SITUAÇÃO do pedido, que é nossa.
// Nesse modelo não existe estorno para emitir nem para receber, e some a classe
// de bug em que um webhook de estorno reabria a fila de um produto.
//
// Os endpoints existem (`/pedidos/vendas/{id}/estornar-estoque`), mas ficam
// FORA do código de propósito: eles respondem sem leitura de volta possível —
// a tag Estoques não tem GET por id — então um timeout ali seria irresolúvel.
func (b *Bling) ReverseOrderStock(ctx context.Context, orderID string) error {
	return fmt.Errorf("%w: o LiveCart não movimenta estoque no Bling — "+
		"a reserva é nativa, ligada à situação do pedido",
		providers.ErrOperationNotSupported)
}

// ReverseLegacyStockExit é exclusivo da drenagem do modelo antigo do Tiny.
func (b *Bling) ReverseLegacyStockExit(ctx context.Context, productID string, qty int, obs string) (string, error) {
	return "", fmt.Errorf("%w: drenagem do modelo legado é exclusiva do Tiny",
		providers.ErrOperationNotSupported)
}

// SyncProduct: o LiveCart não escreve produto de volta no ERP. O catálogo é do
// lojista e a direção do fluxo é ERP → LiveCart.
func (b *Bling) SyncProduct(ctx context.Context, product providers.ERPProduct) (*providers.SyncResult, error) {
	return nil, fmt.Errorf("%w: o LiveCart não escreve produto no ERP",
		providers.ErrOperationNotSupported)
}

// escrever manda um verbo de escrita e decodifica a resposta.
func (b *Bling) escrever(ctx context.Context, metodo, caminho string, corpo any, destino any) error {
	endereco := blingAPIBaseURL + caminho
	resp, bruto, err := b.DoRequestRetrying429(ctx, 2, metodo, endereco, corpo, b.authHeaders())
	if err != nil {
		return fmt.Errorf("bling %s %s: %w", metodo, caminho, err)
	}
	if resp.StatusCode >= 400 {
		return blingErroDeStatus(resp.StatusCode, bruto, resp.Header.Get("x-amzn-RequestId"))
	}
	if destino == nil || len(bruto) == 0 {
		return nil
	}
	if err := json.Unmarshal(bruto, destino); err != nil {
		return fmt.Errorf("bling %s %s: resposta ilegível: %w", metodo, caminho, err)
	}
	return nil
}

// formaPagamentoPadrao resolve a forma de pagamento usada nas parcelas.
//
// `parcelas[]` é obrigatório no POST e cada parcela exige `formaPagamento.id`,
// que é POR CONTA. Preferimos a marcada como padrão pelo lojista; sem padrão,
// a primeira ativa. Sem nenhuma, RECUSA — criar pedido com forma arbitrária
// bagunça o financeiro dele de um jeito que só aparece no fechamento do caixa.
func (b *Bling) formaPagamentoPadrao(ctx context.Context) (int64, error) {
	b.mu.Lock()
	if b.formaPagamentoCache != 0 {
		id := b.formaPagamentoCache
		b.mu.Unlock()
		return id, nil
	}
	// Já tem alguém buscando: espera o resultado dele em vez de gastar outra
	// requisição da cota para descobrir a mesma coisa.
	if espera := b.formaPagamentoEmVoo; espera != nil {
		b.mu.Unlock()
		select {
		case <-espera:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		b.mu.Lock()
		id := b.formaPagamentoCache
		b.mu.Unlock()
		if id != 0 {
			return id, nil
		}
		// Quem estava em voo falhou. Cai para a busca própria — uma falha
		// transitória não pode condenar todo mundo que veio depois.
		b.mu.Lock()
	}
	espera := make(chan struct{})
	b.formaPagamentoEmVoo = espera
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		b.formaPagamentoEmVoo = nil
		b.mu.Unlock()
		close(espera)
	}()

	var env struct {
		Data []struct {
			ID        int64  `json:"id"`
			Descricao string `json:"descricao"`
			Situacao  int    `json:"situacao"`
			Padrao    int    `json:"padrao"`
		} `json:"data"`
	}
	if err := b.get(ctx, "/formas-pagamentos", nil, &env); err != nil {
		return 0, err
	}

	var primeiraAtiva int64
	for _, f := range env.Data {
		if f.Situacao != 1 {
			continue
		}
		if f.Padrao == 1 {
			b.guardarFormaPagamento(f.ID)
			return f.ID, nil
		}
		if primeiraAtiva == 0 {
			primeiraAtiva = f.ID
		}
	}
	if primeiraAtiva == 0 {
		return 0, fmt.Errorf("bling: a conta não tem forma de pagamento ativa — " +
			"o pedido de venda exige parcelas[].formaPagamento.id")
	}
	b.guardarFormaPagamento(primeiraAtiva)
	return primeiraAtiva, nil
}

func (b *Bling) guardarFormaPagamento(id int64) {
	b.mu.Lock()
	b.formaPagamentoCache = id
	b.mu.Unlock()
}

// SetOrderInstallments grava as parcelas EXPLICITAMENTE — quanto já entrou e
// quanto falta.
//
// ⚠ É O RISCO FINANCEIRO Nº 1 DO MÓDULO, e não pelo motivo que parecia.
//
// O medo original era o PUT apagar `parcelas` por omissão. Isso é
// INALCANÇÁVEL: `parcelas` é `required` no schema, e omitir dá 400. O desastre
// real é o servidor RECALCULAR as parcelas para fechar com o novo total — lei
// já MEDIDA no Tiny e documentada em providers.ERPInstallment: "a soma de todas
// TEM de dar o total do pedido, ou o ERP a reescreve sozinho".
//
// Read-modify-write não protege disso. A defesa é: escrever, RELER, e conferir
// que a soma e a quantidade de parcelas sobreviveram. Nunca confiar no 200.
func (b *Bling) SetOrderInstallments(ctx context.Context, orderID string, parcelas []providers.ERPInstallment) error {
	if len(parcelas) == 0 {
		return fmt.Errorf("bling: parcelas[] é obrigatório no pedido de venda")
	}

	forma, err := b.formaPagamentoPadrao(ctx)
	if err != nil {
		return err
	}

	_, cru, err := b.pedido(ctx, orderID)
	if err != nil {
		return err
	}

	var somaEnviada int64
	novas := make([]any, 0, len(parcelas))
	for _, p := range parcelas {
		somaEnviada += p.AmountCents
		novas = append(novas, map[string]any{
			"dataVencimento": p.DueDate.In(blingLocation).Format("2006-01-02"),
			"valor":          float64(p.AmountCents) / 100,
			"observacoes":    p.Note,
			"formaPagamento": map[string]any{"id": forma},
		})
	}
	cru["parcelas"] = novas
	limparReadOnly(cru)

	if err := b.escrever(ctx, http.MethodPut, "/pedidos/vendas/"+url.PathEscape(orderID), cru, nil); err != nil {
		return err
	}

	// A RELEITURA é a parte que não pode ser cortada. Um 200 aqui significa
	// "aceitei", não "gravei o que você mandou".
	depois, _, err := b.pedido(ctx, orderID)
	if err != nil {
		// Não dá para afirmar que as parcelas ficaram erradas — só que não
		// conseguimos conferir. Quem chama precisa saber a diferença.
		return fmt.Errorf("bling: parcelas enviadas mas NÃO CONFERIDAS (releitura falhou): %w", err)
	}
	var somaGravada int64
	for _, p := range depois.Parcelas {
		somaGravada += int64(p.Valor*100 + 0.5)
	}
	if len(depois.Parcelas) != len(parcelas) || somaGravada != somaEnviada {
		return fmt.Errorf("bling: o ERP REESCREVEU as parcelas — enviei %d somando %d centavos, "+
			"gravou %d somando %d. O registro financeiro do lojista não é o que mandamos",
			len(parcelas), somaEnviada, len(depois.Parcelas), somaGravada)
	}
	return nil
}

// UpdateOrderPayment grava o pagamento como parcela única já quitada.
//
// Cai no MESMO PUT de SetOrderInstallments porque o Bling não tem PUT parcial:
// um corpo só com `parcelas` é inválido por construção (`contato`, `data`,
// `dataPrevista`, `dataSaida`, `itens` e `parcelas` são todos required).
func (b *Bling) UpdateOrderPayment(ctx context.Context, orderID string, pagamento *providers.ERPOrderPayment) error {
	if pagamento == nil {
		return fmt.Errorf("bling: pagamento ausente")
	}
	quando := pagamento.PaidAt
	if quando.IsZero() {
		quando = time.Now()
	}
	return b.SetOrderInstallments(ctx, orderID, []providers.ERPInstallment{{
		AmountCents: pagamento.Amount,
		DueDate:     quando,
		Note:        "PAGO — " + pagamento.Method + " " + pagamento.PaymentID,
	}})
}
