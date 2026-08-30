package erp

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

// Cada teste aqui trava um comportamento MEDIDO contra a conta real em
// 29/08/2026 (planning-bling/05-MEDICOES-CONTA-REAL.md). Não são testes de
// "será que compila" — são as armadilhas que custariam uma live.

func bancadaBling(t *testing.T, h http.HandlerFunc) (*Bling, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	baseAntes := blingAPIBaseURL
	blingAPIBaseURL = srv.URL
	t.Cleanup(func() { blingAPIBaseURL = baseAntes })

	b, err := NewBling(BlingConfig{
		IntegrationID: "int-1",
		StoreID:       "loja-1",
		ClientID:      "cid",
		ClientSecret:  "csec",
		Credentials:   &providers.Credentials{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)},
		Logger:        zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b, srv
}

func respJSON(corpo string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(corpo))
	}
}

// ---------------------------------------------------------------- catálogo

// O default de `criterio` no Bling é 1 ("últimos incluídos") e ESCONDE produto.
// Medido: numa conta com 2 produtos, o default devolveu 1. O adapter tem de
// mandar sempre explícito, senão o lojista perde catálogo sem saber.
func TestBlingListProductsMandaCriterioETipoExplicitos(t *testing.T) {
	var vista url.Values
	b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
		vista = r.URL.Query()
		respJSON(`{"data":[]}`)(w, r)
	})

	if _, err := b.ListProducts(context.Background(), providers.ListProductsParams{}); err != nil {
		t.Fatal(err)
	}
	if got := vista.Get("criterio"); got != "5" {
		t.Errorf("criterio = %q, queria \"5\" (todos) — o default do Bling é 1 e esconde produto", got)
	}
	if got := vista.Get("tipo"); got != "T" {
		t.Errorf("tipo = %q, queria \"T\"", got)
	}
	if vista.Get("pagina") == "" || vista.Get("limite") == "" {
		t.Errorf("paginação tem de ir explícita: %v", vista)
	}

	// ActiveOnly muda o critério para 2, não volta ao default.
	if _, err := b.ListProducts(context.Background(), providers.ListProductsParams{ActiveOnly: true}); err != nil {
		t.Fatal(err)
	}
	if got := vista.Get("criterio"); got != "2" {
		t.Errorf("com ActiveOnly, criterio = %q, queria \"2\"", got)
	}
}

// O SKU pode vir VAZIO (medido: os dois produtos da conta real têm codigo:"").
// O adapter não pode inventar SKU nem falhar — o vínculo é pelo ID.
func TestBlingProdutoSemSKUNaoQuebraOVinculo(t *testing.T) {
	b, _ := bancadaBling(t, respJSON(`{"data":[
		{"id":16698952209,"nome":"Playstation 5","codigo":"","gtin":"7891234567918",
		 "preco":3900,"situacao":"A","tipo":"P","formato":"S",
		 "estoque":{"saldoVirtualTotal":5}}]}`))

	r, err := b.ListProducts(context.Background(), providers.ListProductsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Products) != 1 {
		t.Fatalf("veio %d produtos", len(r.Products))
	}
	p := r.Products[0]
	if p.ID != "16698952209" {
		t.Errorf("ID = %q — é ele que ancora o vínculo quando o SKU falta", p.ID)
	}
	if p.SKU != "" {
		t.Errorf("SKU = %q, queria vazio (não inventar)", p.SKU)
	}
	if p.GTIN != "7891234567918" {
		t.Errorf("GTIN = %q — é a segunda chave possível", p.GTIN)
	}
}

// 189.90 * 100 em float64 é 18989.999…; truncar perderia um centavo por produto.
func TestBlingPrecoArredondaEmVezDeTruncar(t *testing.T) {
	b, _ := bancadaBling(t, respJSON(`{"data":[
		{"id":1,"nome":"x","preco":189.90,"situacao":"A","formato":"S"}]}`))

	r, err := b.ListProducts(context.Background(), providers.ListProductsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Products[0].Price; got != 18990 {
		t.Errorf("Price = %d centavos, queria 18990 (truncar daria 18989)", got)
	}
}

// As imagens `internas` do Bling são links S3 ASSINADOS que EXPIRAM (medido:
// cheia ~7 dias, miniatura ~30 min). As `externas` são do lojista e não expiram
// — por isso vêm primeiro.
func TestBlingImagensExternasVemAntesDasQueExpiram(t *testing.T) {
	b, _ := bancadaBling(t, respJSON(`{"data":[
		{"id":1,"nome":"x","situacao":"A","formato":"S","midia":{"imagens":{
			"externas":[{"link":"https://loja.com/eterna.jpg"}],
			"internas":[{"link":"https://s3/assinada.jpg?Expires=123","validade":"2026-09-05 20:44:02"}]
		}}}]}`))

	r, err := b.ListProducts(context.Background(), providers.ListProductsParams{})
	if err != nil {
		t.Fatal(err)
	}
	p := r.Products[0]
	if p.ImageURL != "https://loja.com/eterna.jpg" {
		t.Errorf("a principal devia ser a externa (não expira), veio %q", p.ImageURL)
	}
	if len(p.ImageURLs) != 2 {
		t.Fatalf("queria as 2 imagens, veio %d", len(p.ImageURLs))
	}
}

// ---------------------------------------------------------------- estoque

// A armadilha do filtroSaldoEstoque: o default do Bling é 1 (só positivo), e um
// produto ESGOTADO some da resposta. O adapter pede os TRÊS filtros e une, para
// que ausência volte a significar "o ERP não conhece" em vez de "está zerado".
func TestBlingSaldoEmLotePedeOsTresFiltros(t *testing.T) {
	var filtros []string
	b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
		f := r.URL.Query().Get("filtroSaldoEstoque")
		filtros = append(filtros, f)
		switch f {
		case "1":
			respJSON(`{"data":[{"produto":{"id":1},"saldoFisicoTotal":5,"saldoVirtualTotal":5}]}`)(w, r)
		case "0":
			respJSON(`{"data":[{"produto":{"id":2},"saldoFisicoTotal":0,"saldoVirtualTotal":0}]}`)(w, r)
		default:
			respJSON(`{"data":[{"produto":{"id":3},"saldoFisicoTotal":-2,"saldoVirtualTotal":-2}]}`)(w, r)
		}
	})

	m, err := b.GetProductStockBatch(context.Background(), []string{"1", "2", "3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtros) != 3 {
		t.Fatalf("pediu %d filtros (%v), queria os 3 — sem isso produto esgotado some", len(filtros), filtros)
	}
	for _, id := range []string{"1", "2", "3"} {
		if _, ok := m[id]; !ok {
			t.Errorf("produto %s não voltou; com filtro único ele sumiria em silêncio", id)
		}
	}
	if m["2"].Available != 0 {
		t.Errorf("o zerado devia vir com Available=0 EXPLÍCITO, veio %d", m["2"].Available)
	}
}

// Produto que o ERP não conhece tem de ficar AUSENTE do mapa, nunca zero:
// zero é indistinguível de "esgotado" e faria o espelho zerar o portão.
func TestBlingSaldoAusenteNaoViraZero(t *testing.T) {
	b, _ := bancadaBling(t, respJSON(`{"data":[]}`))

	m, err := b.GetProductStockBatch(context.Background(), []string{"999"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["999"]; ok {
		t.Error("produto ausente na resposta não pode aparecer no mapa — ausência não é zero")
	}
	if _, err := b.GetProductStockDetail(context.Background(), "999"); err == nil {
		t.Error("a leitura unitária de um produto ausente tem de ERRAR, não devolver zero")
	}
}

// O disponível é o VIRTUAL, nunca o físico: o físico conta peça já reservada
// por outro pedido, e vendê-la é oversell.
func TestBlingDisponivelEhOVirtualEReservadoEhADiferenca(t *testing.T) {
	b, _ := bancadaBling(t, respJSON(
		`{"data":[{"produto":{"id":1},"saldoFisicoTotal":10,"saldoVirtualTotal":7}]}`))

	d, err := b.GetProductStockDetail(context.Background(), "1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Balance != 10 || d.Available != 7 || d.Reserved != 3 {
		t.Errorf("saldo = %+v, queria {Balance:10 Available:7 Reserved:3}", d)
	}
}

// ---------------------------------------------------------------- oauth

// A doc é literal: as credenciais vão no header Basic e "não é permitida a
// inserção destes parâmetros no body". Mandar no body devolve invalid_client
// mesmo com credencial certa, e o erro não diz que o problema é o LUGAR.
func TestBlingTokenMandaBasicENuncaCredencialNoBody(t *testing.T) {
	var auth string
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		auth, form = r.Header.Get("Authorization"), r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","token_type":"bearer","expires_in":21600}`))
	}))
	defer srv.Close()

	antes := blingTokenURL
	blingTokenURL = srv.URL
	defer func() { blingTokenURL = antes }()

	cred, err := BlingExchangeCode(context.Background(), srv.Client(), "cid", "csec", "code-123")
	if err != nil {
		t.Fatal(err)
	}

	quer := "Basic " + base64.StdEncoding.EncodeToString([]byte("cid:csec"))
	if auth != quer {
		t.Errorf("Authorization = %q, queria %q", auth, quer)
	}
	for _, proibido := range []string{"client_id", "client_secret"} {
		if form.Get(proibido) != "" {
			t.Errorf("%s foi no BODY — o Bling recusa com invalid_client", proibido)
		}
	}
	// MEDIDO: o Bling devolve "bearer" minúsculo. Normalizar evita que cada
	// consumidor precise lembrar de comparar case-insensitive.
	if cred.TokenType != "Bearer" {
		t.Errorf("TokenType = %q, queria %q normalizado", cred.TokenType, "Bearer")
	}
	// MEDIDO: 21600 s = 6 horas, não 1.
	if d := time.Until(cred.ExpiresAt); d < 5*time.Hour {
		t.Errorf("validade = %s, queria ~6h", d)
	}
}

// O Bling ignora redirect_uri e scope na autorização e usa os do cadastro.
// Mandá-los sugere um controle que não temos.
func TestBlingAuthorizeURLNaoMandaRedirectNemScope(t *testing.T) {
	u := BlingAuthorizeURL("cid", "st4te")
	for _, ausente := range []string{"redirect_uri", "scope"} {
		if strings.Contains(u, ausente) {
			t.Errorf("mandou %q, que o Bling ignora: %s", ausente, u)
		}
	}
	for _, presente := range []string{"response_type=code", "client_id=cid", "state=st4te"} {
		if !strings.Contains(u, presente) {
			t.Errorf("faltou %q: %s", presente, u)
		}
	}
}

// Se o Bling parar de rotacionar o refresh token, a resposta vem sem ele —
// e descartar o antigo desconectaria a loja.
func TestBlingRefreshPreservaTokenAntigoQuandoRespostaNaoTraz(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"novo","token_type":"bearer","expires_in":21600}`))
	}))
	defer srv.Close()
	antes := blingTokenURL
	blingTokenURL = srv.URL
	defer func() { blingTokenURL = antes }()

	b, _ := bancadaBling(t, respJSON(`{}`))
	b.credentials = &providers.Credentials{AccessToken: "velho", RefreshToken: "rt-VELHO"}

	novo, err := b.RefreshToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if novo.RefreshToken != "rt-VELHO" {
		t.Errorf("perdemos o refresh token: %q", novo.RefreshToken)
	}
}

// ---------------------------------------------------------------- erros

// O envelope de erro do Bling é ANINHADO. Se o parse falhar, o operador vê
// "HTTP 400" e nada mais.
func TestBlingErroAninhadoViraMensagemUtil(t *testing.T) {
	b, _ := bancadaBling(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-amzn-RequestId", "req-42")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"VALIDATION_ERROR","description":"campo X invalido"}}`))
	})

	_, err := b.Empresa(context.Background())
	if err == nil {
		t.Fatal("queria erro")
	}
	for _, esperado := range []string{"VALIDATION_ERROR", "campo X invalido", "req-42"} {
		if !strings.Contains(err.Error(), esperado) {
			t.Errorf("mensagem sem %q: %v", esperado, err)
		}
	}
	// 4xx é recusa de validação: o provedor processou e rejeitou ANTES de
	// aplicar. Marcar como comprovadamente não entregue é o que deixa o
	// chamador repetir com segurança.
	if !isProvenUndelivered(err) {
		t.Error("4xx devia carregar ErrProvenUndelivered — sem isso o retry vira aposta")
	}
}

// 5xx NÃO pode ser marcado como não-entregue: o provedor pode ter aplicado e
// falhado só em responder. Foi assim que dois timeouts idênticos tiveram
// desfechos opostos numa live real.
func TestBling5xxNaoEhMarcadoComoNaoEntregue(t *testing.T) {
	b, _ := bancadaBling(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"type":"SERVER_ERROR"}}`))
	})

	_, err := b.Empresa(context.Background())
	if err == nil {
		t.Fatal("queria erro")
	}
	if isProvenUndelivered(err) {
		t.Error("5xx NÃO pode ser ErrProvenUndelivered — o Bling pode ter aplicado e falhado só em responder")
	}
}

// O 429 tem de explicar que a cota é COMPARTILHADA com os outros apps do
// lojista, senão vira "erro 429" no log e ninguém entende por que sumiu.
func TestBling429ExplicaACotaPorConta(t *testing.T) {
	b, _ := bancadaBling(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"TOO_MANY_REQUESTS"}}`))
	})

	_, err := b.Empresa(context.Background())
	if err == nil {
		t.Fatal("queria erro")
	}
	for _, esperado := range []string{"3 req/s", "POR CONTA", "Retry-After"} {
		if !strings.Contains(err.Error(), esperado) {
			t.Errorf("a mensagem do 429 não menciona %q: %v", esperado, err)
		}
	}
}

// ---------------------------------------------------------------- contratos

// O adapter tem de satisfazer as capacidades que o fluxo resolve por assertion.
func TestBlingSatisfazAsCapacidades(t *testing.T) {
	var b any = &Bling{}
	if _, ok := b.(providers.ERPStockReader); !ok {
		t.Error("Bling não é ERPStockReader")
	}
	if _, ok := b.(providers.ERPStockDetailReader); !ok {
		t.Error("Bling não é ERPStockDetailReader")
	}
	if _, ok := b.(providers.ERPStockBatchReader); !ok {
		t.Error("Bling não é ERPStockBatchReader — é a leitura em lote que evita o 429")
	}
}

// Nenhum arquivo de provider pode construir o próprio http.Client: é assim que
// o RefreshToken do Tiny escapou das DUAS camadas de rate limit.
//
// A exceção é o token endpoint, que precisa de cliente dedicado justamente para
// NÃO passar pelo logging do BaseProvider (o refresh_token iria em texto claro
// para integration_logs). A exceção é nomeada, não silenciosa.
func TestBlingSoConstroiHTTPClientNoCaminhoDoToken(t *testing.T) {
	b, err := lerFonte("bling.go")
	if err != nil {
		t.Fatal(err)
	}
	for i, linha := range strings.Split(b, "\n") {
		if !strings.Contains(linha, "&http.Client{") {
			continue
		}
		if !dentroDeBlingTokenRequest(b, i) {
			t.Errorf("bling.go:%d constrói http.Client fora do caminho do token — "+
				"isso escapa das duas camadas de rate limit.\n  linha: %s", i+1, strings.TrimSpace(linha))
		}
	}
}

func isProvenUndelivered(err error) bool {
	return err != nil && strings.Contains(err.Error(), providers.ErrProvenUndelivered.Error())
}

func lerFonte(nome string) (string, error) {
	b, err := os.ReadFile(nome)
	return string(b), err
}

// dentroDeBlingTokenRequest diz se a linha `i` cai dentro da função que faz a
// troca de token — a única autorizada a ter cliente próprio.
func dentroDeBlingTokenRequest(fonte string, linha int) bool {
	linhas := strings.Split(fonte, "\n")
	for i := linha; i >= 0; i-- {
		if strings.HasPrefix(linhas[i], "func ") {
			return strings.Contains(linhas[i], "blingTokenRequest")
		}
	}
	return false
}

// ---------------------------------------------------------------- situação

// A tradução de situação usa `valor`, que é normalizado pelo Bling e vale em
// QUALQUER conta. O `id` é do lojista e varia — usá-lo exigiria o escopo de
// Situações, que nesta conta responde 403.
func TestBlingSituacaoUsaOValorNormalizadoENaoOId(t *testing.T) {
	// Ids deliberadamente ESTRANHOS: se a tradução olhasse o id, ela erraria.
	b, _ := bancadaBling(t, respJSON(
		`{"data":{"id":1,"situacao":{"id":987654,"valor":2},"itens":[],"parcelas":[]}}`))

	got, err := b.GetOrderSituacao(context.Background(), "1")
	if err != nil {
		t.Fatal(err)
	}
	if got != providers.SituacaoCancelada {
		t.Errorf("situação = %d, queria %d (cancelada) — a tradução deve vir do `valor`, não do `id`",
			got, providers.SituacaoCancelada)
	}
}

// A sobreposição numérica entre os dois enums é ARMADILHA: o valor 3 no Bling é
// "Em andamento", e o 3 do núcleo é "Aprovada". Traduzir um pelo outro faria o
// LiveCart dar por fechada uma venda que não está.
func TestBlingValor3NaoViraAprovada(t *testing.T) {
	b, _ := bancadaBling(t, respJSON(
		`{"data":{"id":1,"situacao":{"id":15,"valor":3},"itens":[],"parcelas":[]}}`))

	got, err := b.GetOrderSituacao(context.Background(), "1")
	if err != nil {
		t.Fatal(err)
	}
	if got == providers.SituacaoAprovada {
		t.Error("o valor 3 do Bling (Em andamento) virou SituacaoAprovada — " +
			"são coisas diferentes, e a coincidência numérica é armadilha")
	}
	if got != 0 {
		t.Errorf("sem análogo honesto a resposta devia ser 0 (não sei), veio %d", got)
	}
}

// Parciais NÃO podem arredondar para cima: dar por atendida uma venda que ainda
// tem item para sair fecharia o pedido antes da hora.
func TestBlingParciaisNaoArredondamParaCima(t *testing.T) {
	for _, valor := range []int{5, 6} { // faturado parcial, atendido parcial
		b, _ := bancadaBling(t, respJSON(
			`{"data":{"id":1,"situacao":{"id":99,"valor":`+strconv.Itoa(valor)+`},"itens":[],"parcelas":[]}}`))

		got, err := b.GetOrderSituacao(context.Background(), "1")
		if err != nil {
			t.Fatal(err)
		}
		if got != 0 {
			t.Errorf("valor %d virou situação %d — parcial não é concluído", valor, got)
		}
	}
}

// Ler um pedido ENSINA o id daquela conta, para a escrita poder usá-lo depois
// sem o escopo de Situações.
func TestBlingAprendeOIdDaSituacaoAoLer(t *testing.T) {
	b, _ := bancadaBling(t, respJSON(
		`{"data":{"id":1,"situacao":{"id":12,"valor":2},"itens":[],"parcelas":[]}}`))

	if _, ok := b.situacaoDaConta(providers.SituacaoCancelada); ok {
		t.Fatal("o mapa devia começar vazio")
	}
	if _, err := b.GetOrderSituacao(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}
	id, ok := b.situacaoDaConta(providers.SituacaoCancelada)
	if !ok || id != 12 {
		t.Errorf("depois de ler, o mapa devia saber que cancelada=12 nesta conta; veio %d/%v", id, ok)
	}
}

// Escrever situação NÃO mapeada tem de RECUSAR sem gastar requisição: os ids
// são por conta, e escrever um id desconhecido pode disparar uma transição com
// efeito de estoque na conta do lojista.
// A recusa custa UMA leitura, e no máximo uma.
//
// Este teste já exigiu recusa a custo ZERO. A troca foi deliberada e medida
// contra a conta real: sem ler o próprio pedido, o adapter nunca descobre os
// ids daquela conta, e TODO carrinho pago no Bling falhava na aprovação. Uma
// requisição é o preço de saber; o que não pode é virar tentativa em série.
func TestBlingRecusaEscreverSituacaoDepoisDeUmaUnicaLeitura(t *testing.T) {
	var chamadas int
	b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
		chamadas++
		// Pedido em situação que esta conta NÃO semeou: nada a corroborar.
		respJSON(`{"data":{"id":1,"situacao":{"id":777001,"valor":3}}}`)(w, r)
	})

	err := b.SetOrderSituacao(context.Background(), "1", providers.SituacaoAprovada)
	if err == nil {
		t.Fatal("queria recusa — o id 777001 é do lojista e não corrobora a tabela semeada")
	}
	if chamadas != 1 {
		t.Errorf("gastou %d requisições, quero exatamente 1 — a descoberta é uma leitura, não um laço", chamadas)
	}
	if !strings.Contains(err.Error(), providers.ErrOperationNotSupported.Error()) {
		t.Errorf("erro devia carregar ErrOperationNotSupported: %v", err)
	}
}

// E quando a leitura CORROBORA, a escrita acontece — sem escopo de Situações,
// que é 403 nesta conta. É o caminho que o ensaio contra a conta real percorre.
func TestBlingAprendeOsIdsLendoOProprioPedidoEEntaoEscreve(t *testing.T) {
	var caminhos []string
	b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
		caminhos = append(caminhos, r.Method+" "+r.URL.Path)
		// Todo pedido do Bling nasce em "Em aberto" = id 6, valor 0 — o par que
		// confirma que esta conta usa os ids semeados.
		respJSON(`{"data":{"id":1,"situacao":{"id":6,"valor":0}}}`)(w, r)
	})

	if err := b.SetOrderSituacao(context.Background(), "1", providers.SituacaoAprovada); err != nil {
		t.Fatalf("queria que escrevesse depois de corroborar: %v", err)
	}
	if len(caminhos) != 2 {
		t.Fatalf("requisições = %v, queria uma leitura e uma escrita", caminhos)
	}
	// 15 é "Em andamento", o destino medido para um pedido pago.
	if !strings.HasSuffix(caminhos[1], "/situacoes/15") {
		t.Errorf("escreveu em %q, queria terminar em /situacoes/15", caminhos[1])
	}

	// E a segunda vez não relê: o veredito ficou guardado.
	caminhos = nil
	if err := b.SetOrderSituacao(context.Background(), "1", providers.SituacaoCancelada); err != nil {
		t.Fatalf("segunda transição: %v", err)
	}
	if len(caminhos) != 1 {
		t.Errorf("requisições = %v, queria só a escrita — a corroboração não pode repetir por pedido", caminhos)
	}
}

// ─── A TABELA DE SITUAÇÕES ──────────────────────────────────────────────────
//
// Os ids de situação do Bling são POR CONTA. A tabela semeada (medida em
// 30/08/2026) resolve o caso comum, mas escrever um id que naquela conta
// significa outra coisa mexeria no estoque do lojista. Por isso ela só vale
// depois de corroborada por uma leitura da própria conta.

func TestTabelaDeSituacoesSoValeDepoisDeCorroborada(t *testing.T) {
	b := &Bling{}

	if _, ok := b.situacaoDaConta(providers.SituacaoAprovada); ok {
		t.Fatal("a tabela semeada foi usada sem corroboração — escrever um id não " +
			"confirmado pode disparar a transição errada na conta do lojista")
	}

	// Uma leitura qualquer da conta mostrando um par que BATE autoriza a tabela.
	b.corroborarTabelaPadrao(6, 0) // Em aberto, como todo pedido nasce
	id, ok := b.situacaoDaConta(providers.SituacaoAprovada)
	if !ok || id != 15 {
		t.Errorf("depois de corroborada: id=%d ok=%v, queria 15 e true", id, ok)
	}
}

func TestUmParContraditorioDesligaATabelaParaSempre(t *testing.T) {
	b := &Bling{}
	b.corroborarTabelaPadrao(6, 0) // confirma
	if _, ok := b.situacaoDaConta(providers.SituacaoCancelada); !ok {
		t.Fatal("a corroboração não pegou")
	}

	// Esta conta reaproveitou o id 12 para outra coisa.
	b.corroborarTabelaPadrao(12, 7)
	if _, ok := b.situacaoDaConta(providers.SituacaoCancelada); ok {
		t.Error("a tabela continuou valendo depois de ser desmentida")
	}
	// E não volta atrás nem com mais pares que batem.
	b.corroborarTabelaPadrao(9, 1)
	if _, ok := b.situacaoDaConta(providers.SituacaoAberta); ok {
		t.Error("um par que bate reabilitou uma tabela já desmentida — uma conta com " +
			"ids reaproveitados nunca é segura de novo")
	}
}

func TestSituacaoCriadaPeloLojistaNaoDizNadaSobreATabela(t *testing.T) {
	b := &Bling{}
	// Id fora da faixa semeada: é situação do lojista, e não corrobora nem
	// desmente coisa nenhuma.
	b.corroborarTabelaPadrao(918273, 3)
	if _, ok := b.situacaoDaConta(providers.SituacaoAprovada); ok {
		t.Error("uma situação criada pelo lojista corroborou a tabela semeada")
	}
}

// O que foi OBSERVADO vence a tabela: um id aprendido de leitura é dado, não
// palpite.
func TestOAprendidoVenceOSemeado(t *testing.T) {
	b := &Bling{}
	b.corroborarTabelaPadrao(6, 0)
	b.aprenderSituacao(providers.SituacaoCancelada, 4242)

	id, ok := b.situacaoDaConta(providers.SituacaoCancelada)
	if !ok || id != 4242 {
		t.Errorf("id=%d ok=%v, queria 4242 — o observado na conta vence o semeado", id, ok)
	}
}

// A tradução na LEITURA continua recusando "Em andamento" → "Aprovada". As duas
// direções são deliberadamente assimétricas: nós escolhemos "Em andamento" como
// destino porque SABEMOS que o pagamento entrou; o Bling dizendo "em andamento"
// não afirma isso de volta.
func TestALeituraNaoTrocaEmAndamentoPorAprovada(t *testing.T) {
	if _, ok := situacaoCanonicaDoValor(blingValorEmAndamento); ok {
		t.Error("a leitura traduziu 'Em andamento' — o núcleo daria a venda por " +
			"aprovada sem que ninguém tenha aprovado")
	}
	// E o caminho de escrita usa exatamente esse id.
	if blingSituacoesPadrao[providers.SituacaoAprovada] != 15 {
		t.Errorf("a escrita de 'Aprovada' aponta para %d, e a medição de 30/08 diz 15",
			blingSituacoesPadrao[providers.SituacaoAprovada])
	}
}

// As duas tabelas descrevem a MESMA medição e não podem divergir.
func TestAsDuasTabelasConcordam(t *testing.T) {
	valorCanonico := map[int]int{ // canônico → valor normalizado do Bling
		providers.SituacaoAberta:    0,
		providers.SituacaoFaturada:  1,
		providers.SituacaoCancelada: 2,
		providers.SituacaoAprovada:  3,
	}
	for canonico, id := range blingSituacoesPadrao {
		valor, ok := blingValorDaSituacaoPadrao[id]
		if !ok {
			t.Errorf("o id %d (canônico %d) não está na tabela id→valor", id, canonico)
			continue
		}
		if querido := valorCanonico[canonico]; valor != querido {
			t.Errorf("canônico %d → id %d → valor %d, queria valor %d", canonico, id, valor, querido)
		}
	}
}
