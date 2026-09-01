package erp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	// "Não sei" é SituacaoDesconhecida, e NÃO zero: zero é "Em aberto", um
	// estágio real. Esta asserção já exigiu zero — e era ela que deixava passar
	// o disfarce que ressuscitava carrinho cancelado.
	if got != providers.SituacaoDesconhecida {
		t.Errorf("sem análogo honesto a resposta devia ser SituacaoDesconhecida (%d), veio %d",
			providers.SituacaoDesconhecida, got)
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
		if got != providers.SituacaoDesconhecida {
			t.Errorf("valor %d virou situação %d — parcial não é concluído, e a resposta "+
				"tem de ser SituacaoDesconhecida (%d), nunca zero (Em aberto)",
				valor, got, providers.SituacaoDesconhecida)
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

// ─── AS PARCELAS TÊM DE BATER COM O TOTAL ───────────────────────────────────
//
// O defeito que travava o pedido no PRIMEIRO item: o documento vem do GET com
// as parcelas do total ANTIGO, trocar `itens` muda o total, e o Bling recusa a
// venda inteira com code 22 — "O somatório do valor das parcelas difere do
// total da venda". Todo segundo produto da compradora morria em HTTP 400.
//
// A conta foi MEDIDA contra a conta real em 31/08/2026, um componente por vez.

func TestTotalDaVendaReproduzAContaDoBling(t *testing.T) {
	itens := []blingItemPedido{
		{Quantidade: 1, Valor: 10},
		{Quantidade: 1, Valor: 20},
	}
	casos := []struct {
		nome  string
		cru   map[string]any
		quero int64 // centavos
	}{
		{"só itens", map[string]any{}, 3000},
		{"+ outrasDespesas", map[string]any{"outrasDespesas": 5.0}, 3500},
		{"+ frete", map[string]any{"transporte": map[string]any{"frete": 7.0}}, 3700},
		{"− desconto em REAL", map[string]any{
			"desconto": map[string]any{"valor": 10.0, "unidade": "REAL"}}, 2000},
		{
			// MEDIDO: 10% sobre 30 = 3, e o frete NÃO entra na base.
			// O Bling aceitou 34 (30−3+7) e recusou 33,30 (37−3,70).
			nome: "− desconto PERCENTUAL não incide sobre o frete",
			cru: map[string]any{
				"desconto":   map[string]any{"valor": 10.0, "unidade": "PERCENTUAL"},
				"transporte": map[string]any{"frete": 7.0},
			},
			quero: 3400,
		},
		{"desconto maior que tudo não fica negativo", map[string]any{
			"desconto": map[string]any{"valor": 999.0, "unidade": "REAL"}}, 0},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			if got := totalDaVenda(c.cru, itens); got != c.quero {
				t.Errorf("total = %d centavos, queria %d", got, c.quero)
			}
		})
	}
}

func TestRebasearParcelasPreservaAProporcaoEFechaOTotal(t *testing.T) {
	casos := []struct {
		nome   string
		antes  []float64
		total  int64
		depois []float64
	}{
		{"parcela única vira o total", []float64{10}, 3000, []float64{30}},
		{"duas parcelas mantêm a proporção", []float64{18, 12}, 6000, []float64{36, 24}},
		{"a sobra do arredondamento vai na última", []float64{10, 10, 10}, 10000, []float64{33.33, 33.33, 33.34}},
		{"soma zero joga tudo na primeira", []float64{0, 0}, 5000, []float64{50, 0}},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			parcelas := make([]any, 0, len(c.antes))
			for _, v := range c.antes {
				parcelas = append(parcelas, map[string]any{"valor": v, "dataVencimento": "2026-09-07"})
			}
			cru := map[string]any{"parcelas": parcelas}
			rebasearParcelas(cru, c.total)

			var soma int64
			for i, pa := range parcelas {
				m := pa.(map[string]any)
				got := m["valor"].(float64)
				soma += centavos(got)
				if centavos(got) != centavos(c.depois[i]) {
					t.Errorf("parcela %d = %.2f, queria %.2f", i, got, c.depois[i])
				}
				// Os outros campos da parcela têm de sobreviver: o vencimento e
				// a forma de pagamento são do lojista.
				if m["dataVencimento"] != "2026-09-07" {
					t.Errorf("parcela %d perdeu o vencimento", i)
				}
			}
			if soma != c.total {
				t.Errorf("a soma deu %d centavos e o total é %d — é exatamente a "+
					"diferença que o Bling recusa com code 22", soma, c.total)
			}
		})
	}
}

// O id da parcela apodrece como o do item: um PUT anterior pode ter trocado a
// lista, e o id ecoado de um GET velho já não existe. Medido: HTTP 400,
// "O id (19493753062) da parcela é inválido".
func TestLimparReadOnlyTiraOIdDaParcela(t *testing.T) {
	cru := map[string]any{
		"parcelas": []any{
			map[string]any{"id": 19493753062, "valor": 30.0, "dataVencimento": "2026-09-07"},
		},
		"itens": []any{map[string]any{"id": 123, "quantidade": 1.0}},
	}
	limparReadOnly(cru)

	p := cru["parcelas"].([]any)[0].(map[string]any)
	if _, tem := p["id"]; tem {
		t.Error("o id da parcela sobreviveu — o Bling recusa o PUT quando ele está obsoleto")
	}
	if p["valor"] != 30.0 || p["dataVencimento"] != "2026-09-07" {
		t.Error("limpar o id não pode levar o resto da parcela junto")
	}
}

// O erro do Bling tem de dizer QUAL campo. Sem isso, um PUT recusado dizia só
// "A venda não pode ser salva" — idêntico para uma dúzia de causas diferentes,
// e foi preciso reproduzir na conta real com curl para descobrir a verdadeira.
func TestErroDoBlingCarregaOCampoQueFalhou(t *testing.T) {
	corpo := []byte(`{"error":{"type":"VALIDATION_ERROR",
	  "description":"A venda não pode ser salva, pois ocorreram problemas em sua validação.",
	  "fields":[{"code":22,"msg":"O somatório do valor das parcelas difere do total da venda",
	             "element":"parcelas","namespace":"VENDAS"}]}}`)

	msg := blingErro(corpo)
	for _, querido := range []string{"parcelas", "somatório", "code 22"} {
		if !strings.Contains(msg, querido) {
			t.Errorf("a mensagem não traz %q: %s", querido, msg)
		}
	}
}

// A situação lida do pedido vira o status canônico que o rastreamento entende.
//
// É o que faz um cancelamento no Bling chegar ao carrinho: ObserveOrderStatus
// já sabe cancelar o carrinho, marcar pagamento lançado por fora e ressuscitar
// — só faltava o Bling traduzir a situação para o vocabulário dele.
func TestSituacaoLidaViraStatusCanonico(t *testing.T) {
	casos := []struct {
		nome    string
		valor   int
		quero   providers.ERPOrderStatus
		observa bool
	}{
		{"cancelado", 2, providers.ERPOrderStatusCancelado, true},
		{"em aberto", 0, providers.ERPOrderStatusAberto, true},
		{"atendido vira faturado", 1, providers.ERPOrderStatusFaturado, true},
		// "Em andamento" não tem análogo honesto: não afirma aprovação.
		// Inventar um faria o rastreamento concluir o que ninguém disse.
		{"em andamento não observa", 3, "", false},
		{"aguardando pagamento não observa", 7, "", false},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			var gets int
			b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
				gets++
				respJSON(fmt.Sprintf(
					`{"data":{"id":26737111955,"situacao":{"id":6,"valor":%d}}}`, c.valor))(w, r)
			})

			canonico, err := b.GetOrderSituacao(context.Background(), "26737111955")
			if err != nil {
				t.Fatalf("lendo a situação: %v", err)
			}
			status, conhecido := providers.ERPOrderStatusFromSituacao(canonico)

			if conhecido != c.observa {
				t.Fatalf("conhecido = %v (canônico %d), queria %v", conhecido, canonico, c.observa)
			}
			if c.observa && status != c.quero {
				t.Errorf("status = %q, queria %q", status, c.quero)
			}
			if gets != 1 {
				t.Errorf("gastou %d GETs, quero exatamente 1", gets)
			}
		})
	}
}

// "Não sei" NÃO pode ser confundido com "Em aberto".
//
// Zero é um estágio real (SituacaoAberta). O adapter devolvia zero para uma
// situação sem análogo, e um pedido "Em andamento" chegava ao rastreamento como
// ABERTO — o que faz VoltouAViver() ser verdadeiro e pode RESSUSCITAR um
// carrinho que o lojista cancelou. O adapter do Tiny já trazia o aviso no
// comentário ("ausente é diferente de zero"); este teste impede a recaída.
func TestSituacaoDesconhecidaNaoSeDisfarcaDeEmAberto(t *testing.T) {
	semAnalogo := []int{
		blingValorEmAndamento, blingValorAguardandoPagto,
		blingValorFaturadoParcial, blingValorAtendidoParcial,
	}
	for _, valor := range semAnalogo {
		b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
			respJSON(fmt.Sprintf(`{"data":{"id":1,"situacao":{"id":6,"valor":%d}}}`, valor))(w, r)
		})
		got, err := b.GetOrderSituacao(context.Background(), "1")
		if err != nil {
			t.Fatalf("valor %d: %v", valor, err)
		}
		if got == providers.SituacaoAberta {
			t.Errorf("valor %d virou SituacaoAberta (%d) — o rastreamento concluiria que "+
				"o pedido está aberto e poderia ressuscitar um carrinho cancelado",
				valor, providers.SituacaoAberta)
		}
		if _, conhecido := providers.ERPOrderStatusFromSituacao(got); conhecido {
			t.Errorf("valor %d virou um status conhecido (%d) sem ter análogo", valor, got)
		}
	}
}

// Pedido SEM o campo situacao é ERRO, não um palpite — mesma regra do Tiny.
func TestPedidoSemCampoSituacaoEhErro(t *testing.T) {
	b, _ := bancadaBling(t, respJSON(`{"data":{"id":1}}`))
	got, err := b.GetOrderSituacao(context.Background(), "1")
	if err == nil {
		t.Error("aceitou um pedido sem situação em silêncio")
	}
	if got == providers.SituacaoAberta {
		t.Error("devolveu 'Em aberto' para um pedido cuja situação não veio")
	}
}

// O BLING RECUSA UM PUT QUE NÃO MUDA NADA.
//
//	400 VALIDATION_ERROR [Informações idênticas a última venda salva,
//	                      altere alguma informação caso deseje prosseguir (code 3)]
//
// Medido em staging em 31/08/2026, num PAGAMENTO. O fluxo reconcilia a grade
// antes de aprovar — sempre, de propósito, porque é a rede que pega o
// comentário que caiu entre a última leitura e a liberação do estado. No Tiny
// esse PUT é idempotente; aqui ele derrubou a finalização inteira e a venda
// paga não chegou ao ERP.
func TestPutIdenticoNaoEhEnviado(t *testing.T) {
	itens := []providers.ERPOrderItem{
		{ProductID: "16698953100", Quantity: 2, UnitPrice: 1000, Name: "A", Note: providers.LiveCartItemMarker},
	}

	var metodos []string
	b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
		metodos = append(metodos, r.Method)
		// O pedido JÁ tem exatamente essa grade — com os campos calculados que
		// o Bling devolve e que nunca enviamos.
		respJSON(`{"data":{"id":1,"situacao":{"id":6,"valor":0},
		  "itens":[{"id":999,"unidade":"un","aliquotaIPI":0,
		            "produto":{"id":16698953100},"quantidade":2,"valor":10,
		            "descricao":"A","descricaoDetalhada":"[LiveCart]"}],
		  "parcelas":[{"id":7,"valor":20}]}}`)(w, r)
	})

	if err := b.UpdateOrderItems(context.Background(), "1", itens); err != nil {
		t.Fatalf("grade idêntica devia ser no-op, veio erro: %v", err)
	}
	for _, m := range metodos {
		if m == http.MethodPut {
			t.Error("mandou o PUT com a grade idêntica — o Bling recusa com code 3 e " +
				"derruba a finalização da venda paga")
		}
	}
	if len(metodos) != 1 || metodos[0] != http.MethodGet {
		t.Errorf("requisições = %v, quero só o GET que a comparação já precisava", metodos)
	}
}

// E uma grade DIFERENTE continua sendo escrita — a comparação não pode virar
// um "nunca escreve".
func TestGradeDiferenteAindaEhEnviada(t *testing.T) {
	casos := []struct {
		nome  string
		itens []providers.ERPOrderItem
	}{
		{"quantidade mudou", []providers.ERPOrderItem{
			{ProductID: "16698953100", Quantity: 3, UnitPrice: 1000, Name: "A"}}},
		{"preço mudou", []providers.ERPOrderItem{
			{ProductID: "16698953100", Quantity: 2, UnitPrice: 1500, Name: "A"}}},
		{"produto mudou", []providers.ERPOrderItem{
			{ProductID: "16698952209", Quantity: 2, UnitPrice: 1000, Name: "A"}}},
		{"item a mais", []providers.ERPOrderItem{
			{ProductID: "16698953100", Quantity: 2, UnitPrice: 1000, Name: "A"},
			{ProductID: "16698952209", Quantity: 1, UnitPrice: 500, Name: "B"}}},
		{"grade vazia", nil},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			var viuPut bool
			b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					viuPut = true
				}
				respJSON(`{"data":{"id":1,"situacao":{"id":6,"valor":0},
				  "itens":[{"id":999,"produto":{"id":16698953100},"quantidade":2,"valor":10,
				            "descricao":"A","descricaoDetalhada":"[LiveCart]"}],
				  "parcelas":[{"id":7,"valor":20}]}}`)(w, r)
			})
			if err := b.UpdateOrderItems(context.Background(), "1", c.itens); err != nil {
				t.Fatalf("erro: %v", err)
			}
			if !viuPut {
				t.Error("a grade mudou e o PUT não foi enviado — o pedido ficaria " +
					"diferente do carrinho que a compradora pagou")
			}
		})
	}
}

// ─── A FORMA DE PAGAMENTO ───────────────────────────────────────────────────
//
// Um PIX de R$ 7.975,41 foi gravado no Bling como DINHEIRO. A causa não era um
// mapeamento errado: era um método que nunca era passado. formaPagamentoPadrao
// escolhia a forma marcada como padrão na conta — e nessa conta a padrão é
// Dinheiro. A observação da parcela dizia "pix" e o campo estruturado, que vira
// o tPag da NF-e e a linha do fechamento de caixa, dizia dinheiro.
//
// O fake abaixo serve EXATAMENTE as sete formas medidas na conta real em
// 31/08/2026 — inclusive a ausência de cartão de crédito, que é o caso que
// qualquer desenho tem de sobreviver.
const formasDaContaReal = `{"data":[
  {"id":11010299,"descricao":"Dinheiro","padrao":1,"situacao":1,"tipoPagamento":1,"finalidade":3},
  {"id":11010305,"descricao":"Pix","padrao":0,"situacao":1,"tipoPagamento":20,"finalidade":3},
  {"id":11010300,"descricao":"Boleto","padrao":0,"situacao":1,"tipoPagamento":15,"finalidade":2},
  {"id":11010301,"descricao":"Cheque","padrao":0,"situacao":1,"tipoPagamento":2,"finalidade":3},
  {"id":11010302,"descricao":"Depósito Bancário","padrao":0,"situacao":1,"tipoPagamento":16,"finalidade":3},
  {"id":11010303,"descricao":"Crediário","padrao":0,"situacao":1,"tipoPagamento":21,"finalidade":3},
  {"id":11010304,"descricao":"Vale-Troca","padrao":0,"situacao":1,"tipoPagamento":21,"finalidade":2}
]}`

// bancadaDeFormas responde /formas-pagamentos com a conta real e o resto com um
// pedido vazio, contando quantas vezes cada caminho foi chamado.
func bancadaDeFormas(t *testing.T, parcelasDoPedido string) (*Bling, *int) {
	t.Helper()
	var leiturasDeFormas int
	b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/formas-pagamentos") {
			leiturasDeFormas++
			respJSON(formasDaContaReal)(w, r)
			return
		}
		respJSON(`{"data":{"id":1,"situacao":{"id":6,"valor":0},"itens":[],"parcelas":`+
			parcelasDoPedido+`}}`)(w, r)
	})
	return b, &leiturasDeFormas
}

func TestFormaDePagamentoSaiDoMetodoENaoDoPadraoDaConta(t *testing.T) {
	casos := []struct {
		metodo    string
		queroID   int64
		queroWarn bool
		porque    string
	}{
		{"pix", 11010305, false, "o defeito que o lojista viu: hoje isto daria 11010299 (Dinheiro)"},
		{"boleto", 11010300, false, "tipoPagamento 15"},
		{"debit_card", 11010299, true, "a conta não tem débito — cai na padrão, COM aviso"},
		{"credit_card", 11010299, true, "a conta não tem cartão de crédito — o caso que o desenho tem de aguentar"},
		{"", 11010299, false, "DESCONTO e A PAGAR não têm instrumento: padrão em SILÊNCIO"},
		{"manual", 11010299, false, "pagamento por fora: instrumento mesmo desconhecido"},
		{"erp_manual", 11010299, false, "baixa lançada no ERP"},
		{"other", 11010299, false, "o gateway não soube dizer"},
	}

	for _, c := range casos {
		t.Run(c.metodo+" → "+strconv.FormatInt(c.queroID, 10), func(t *testing.T) {
			b, _ := bancadaDeFormas(t, `[]`)
			got, err := b.formaPagamentoPara(context.Background(), c.metodo)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.queroID {
				t.Errorf("método %q → forma %d, queria %d (%s)", c.metodo, got, c.queroID, c.porque)
			}
		})
	}
}

// A resolução por método NÃO pode multiplicar requisições: o teto do Bling é
// 3 req/s POR CONTA, somando todos os apps do lojista.
func TestResolverFormaPorMetodoNaoGastaRequisicaoAMais(t *testing.T) {
	b, leituras := bancadaDeFormas(t, `[]`)
	ctx := context.Background()

	for _, m := range []string{"pix", "credit_card", "boleto", "", "pix", "debit_card"} {
		if _, err := b.formaPagamentoPara(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if *leituras != 1 {
		t.Errorf("leu /formas-pagamentos %d vezes, quero 1 — o mapa por tipo sai da "+
			"MESMA leitura que resolve o padrão", *leituras)
	}
}

// Multi-pagamento: duas parcelas com instrumentos diferentes têm de sair com
// formas diferentes. Resolver uma vez por chamada carimbaria a primeira em todas.
func TestCadaParcelaLevaAFormaDoSeuProprioMetodo(t *testing.T) {
	var corpoPut map[string]any
	var leiturasDeFormas int
	// O fake guarda o que foi escrito: SetOrderInstallments RELÊ o pedido e
	// confere que o ERP não reescreveu a divisão — um fake sem memória faria a
	// verificação (correta) parecer falha do teste.
	gravadas := "[]"
	b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/formas-pagamentos") {
			leiturasDeFormas++
			respJSON(formasDaContaReal)(w, r)
			return
		}
		if r.Method == http.MethodPut {
			_ = json.NewDecoder(r.Body).Decode(&corpoPut)
			if bruto, err := json.Marshal(corpoPut["parcelas"]); err == nil {
				gravadas = string(bruto)
			}
			respJSON(`{"data":{"id":1}}`)(w, r)
			return
		}
		respJSON(`{"data":{"id":1,"situacao":{"id":6,"valor":0},"itens":[],"parcelas":`+
			gravadas+`}}`)(w, r)
	})

	agora := time.Now()
	err := b.SetOrderInstallments(context.Background(), "1", []providers.ERPInstallment{
		{AmountCents: 5000, DueDate: agora, Note: "PAGO — pix", Method: "pix"},
		{AmountCents: 3000, DueDate: agora, Note: "PAGO — cartão", Method: "credit_card"},
		{AmountCents: 1000, DueDate: agora, Note: "DESCONTO concedido", Method: ""},
	})
	if err != nil {
		t.Fatal(err)
	}

	parcelas, _ := corpoPut["parcelas"].([]any)
	if len(parcelas) != 3 {
		t.Fatalf("parcelas enviadas = %d, queria 3", len(parcelas))
	}
	formaDa := func(i int) int64 {
		m := parcelas[i].(map[string]any)
		f := m["formaPagamento"].(map[string]any)
		return int64(numeroDoCru(f["id"]))
	}
	if formaDa(0) != 11010305 {
		t.Errorf("a parcela do PIX saiu com a forma %d, queria 11010305 (Pix)", formaDa(0))
	}
	if formaDa(1) != 11010299 {
		t.Errorf("a parcela do cartão saiu com %d; esta conta não tem cartão, "+
			"então o esperado é a padrão 11010299", formaDa(1))
	}
	if formaDa(2) != 11010299 {
		t.Errorf("a linha de DESCONTO saiu com %d, queria a padrão 11010299", formaDa(2))
	}
	if formaDa(0) == formaDa(1) && formaDa(0) == 11010299 {
		t.Error("todas as parcelas saíram com a padrão — a resolução continua sendo " +
			"uma por chamada, e não uma por parcela")
	}
	if leiturasDeFormas != 1 {
		t.Errorf("leu /formas-pagamentos %d vezes para 3 parcelas, quero 1", leiturasDeFormas)
	}
}

// Forma INATIVA nunca pode ser escolhida, mesmo que o servidor a devolva.
func TestFormaInativaNuncaEhEscolhida(t *testing.T) {
	b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/formas-pagamentos") {
			respJSON(`{"data":[
			  {"id":1,"descricao":"Pix desativado","padrao":0,"situacao":0,"tipoPagamento":20,"finalidade":3},
			  {"id":2,"descricao":"Dinheiro","padrao":1,"situacao":1,"tipoPagamento":1,"finalidade":3}
			]}`)(w, r)
			return
		}
		respJSON(`{"data":{"id":1}}`)(w, r)
	})
	got, err := b.formaPagamentoPara(context.Background(), "pix")
	if err != nil {
		t.Fatal(err)
	}
	if got == 1 {
		t.Error("escolheu uma forma INATIVA — a conferência em código existe porque " +
			"servidor que ignora o filtro em silêncio devolve a lista inteira")
	}
}

// Forma de finalidade 1 (só PAGAMENTOS) não serve para pedido de VENDA, que
// gera conta a RECEBER.
func TestFormaDeFinalidadeDePagamentoNaoServeParaVenda(t *testing.T) {
	b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/formas-pagamentos") {
			respJSON(`{"data":[
			  {"id":1,"descricao":"Pix de saída","padrao":0,"situacao":1,"tipoPagamento":20,"finalidade":1},
			  {"id":2,"descricao":"Dinheiro","padrao":1,"situacao":1,"tipoPagamento":1,"finalidade":3}
			]}`)(w, r)
			return
		}
		respJSON(`{"data":{"id":1}}`)(w, r)
	})
	got, err := b.formaPagamentoPara(context.Background(), "pix")
	if err != nil {
		t.Fatal(err)
	}
	if got == 1 {
		t.Error("escolheu forma de finalidade 1 (só pagamentos) para um pedido de venda — " +
			"o lançamento iria para o lugar errado")
	}
}

// Um pedido apagado no Bling responde 404. Isso é RESPOSTA, não falha: quem
// pergunta precisa poder dizer "perguntei e não existe" em vez de "não
// consegui perguntar". Sem a sentinela, o webhook do pedido tratava as duas
// como erro e enchia o log de produção de `falha ao observar a situação`.
func TestBlingPedidoApagadoRespondeNaoExisteENaoFalha(t *testing.T) {
	b, _ := bancadaBling(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"type":"NOT_FOUND","message":"Pedido não encontrado"}}`))
	})

	_, err := b.GetOrderSituacao(context.Background(), "42")
	if !errors.Is(err, providers.ErrOrderNotFound) {
		t.Fatalf("erro = %v, queria embrulhar providers.ErrOrderNotFound", err)
	}
	// A causa original tem de sobreviver: quem depurar precisa do corpo.
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("a mensagem perdeu o status original: %v", err)
	}
}

// 404 é a ÚNICA recusa que vira "não existe". Um 400 continua sendo recusa de
// validação — confundir os dois faria um pedido rejeitado passar por apagado, e
// o carrinho seguiria apontando para um pedido que nunca nasceu.
func TestBlingSo404ViraNaoExiste(t *testing.T) {
	b, _ := bancadaBling(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"VALIDATION_ERROR","message":"campo inválido"}}`))
	})

	_, err := b.GetOrderSituacao(context.Background(), "42")
	if errors.Is(err, providers.ErrOrderNotFound) {
		t.Fatalf("400 virou 'não existe': %v", err)
	}
	if !errors.Is(err, providers.ErrProvenUndelivered) {
		t.Errorf("400 deveria seguir sendo recusa comprovada: %v", err)
	}
}
