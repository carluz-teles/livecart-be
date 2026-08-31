package erp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
	"livecart/apps/api/lib/ratelimit"
)

// Bling é o adapter da API v3 do Bling.
//
// Tudo aqui é consequência de MEDIÇÃO contra uma conta real em 29/08/2026
// (relatório em planning-bling/05-MEDICOES-CONTA-REAL.md), não de leitura de
// spec. Os quatro fatos que mais moldam este arquivo:
//
//  1. A API NÃO devolve header de cota nenhum — nem X-RateLimit-*, nem
//     Retry-After. O teto é 3 req/s POR CONTA somando TODOS os apps do lojista.
//     Um limitador que espera header (o AdaptiveLimiter) sai sem freio nenhum.
//  2. O token endpoint fica em bling.com.br (NÃO em api.bling.com.br) e exige
//     as credenciais no header Basic — a doc proíbe mandá-las no corpo.
//  3. O `codigo` (SKU) do produto pode vir VAZIO. O vínculo com o produto do
//     LiveCart é pelo `id`, nunca pelo SKU.
//  4. As URLs de imagem são links S3 ASSINADOS que EXPIRAM (a cheia em ~7 dias,
//     a miniatura em ~30 minutos). Guardar a URL é garantir imagem quebrada.
//     Ver BlingImagemExpira e o consumidor em internal/product.
var (
	blingAPIBaseURL   = "https://api.bling.com.br/Api/v3"
	blingTokenURL     = "https://bling.com.br/Api/v3/oauth/token"
	blingAuthorizeURL = "https://bling.com.br/Api/v3/oauth/authorize"
	blingRevokeURL    = "https://bling.com.br/Api/v3/oauth/revoke"
)

// BlingImagemExpira é a validade MEDIDA da URL de imagem cheia. Serve de
// documentação executável para quem for decidir cache: qualquer coisa acima
// disto é imagem quebrada na cara da compradora.
const BlingImagemExpira = 7 * 24 * time.Hour

type Bling struct {
	*providers.BaseProvider

	credentials  *providers.Credentials
	clientID     string
	clientSecret string

	// mapaSituacoes traduz o código canônico de situação (o enum do Tiny, que o
	// núcleo usa) no id daquela CONTA Bling. Os ids são por conta — o lojista
	// cria as dele — então escrever um id fixo pode significar outra coisa, ou
	// disparar uma transição com efeito de estoque. Resolvido na conexão e
	// guardado no metadata da integração; sem entrada, a escrita RECUSA.
	mapaSituacoes map[int]int64

	// padraoConfirmado/padraoContradito guardam o veredito sobre a tabela de
	// situações semeada pelo Bling, decidido por leituras da própria conta.
	// Contradito vence confirmado e nunca volta atrás.
	padraoConfirmado bool
	padraoContradito bool

	// mu protege os caches resolvidos sob demanda (forma de pagamento) e o
	// veredito da tabela de situações.
	mu                  sync.Mutex
	formaPagamentoCache int64
	// formaPagamentoEmVoo colapsa as leituras concorrentes numa só.
	//
	// Medido no ensaio de live simulada: 15 compradoras chegando juntas erravam
	// o cache TODAS antes de qualquer uma preenchê-lo, e disparavam 15 leituras
	// idênticas de /formas-pagamentos — 15 das 45 requisições do ensaio, um
	// TERÇO da cota da live gasto para descobrir a mesma coisa quinze vezes.
	//
	// Não é sync.Once porque a resolução pode FALHAR (429, rede), e uma falha
	// não pode congelar o adapter para sempre: quem chega depois de um erro
	// tenta de novo.
	formaPagamentoEmVoo chan struct{}

	// contaID é o `data.id` de GET /empresas/me/dados-basicos — a identidade da
	// CONTA. É o mesmo valor que o webhook manda como `companyId` (medido,
	// byte-idêntico), e é a chave de cota correta: o teto do Bling é por conta,
	// não por integração.
	contaID string
}

type BlingConfig struct {
	IntegrationID string
	StoreID       string
	Credentials   *providers.Credentials
	ClientID      string
	ClientSecret  string
	ContaID       string
	MapaSituacoes map[int]int64
	Logger        *zap.Logger
	LogFunc       providers.LogFunc
	RateLimiter   ratelimit.RateLimiter
}

func NewBling(cfg BlingConfig) (*Bling, error) {
	if cfg.Credentials == nil {
		return nil, fmt.Errorf("bling: credenciais ausentes")
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, fmt.Errorf("bling: client_id e client_secret do aplicativo são obrigatórios")
	}
	return &Bling{
		BaseProvider: providers.NewBaseProvider(providers.BaseProviderConfig{
			IntegrationID: cfg.IntegrationID,
			StoreID:       cfg.StoreID,
			Logger:        cfg.Logger,
			LogFunc:       cfg.LogFunc,
			Timeout:       45 * time.Second,
			RateLimiter:   cfg.RateLimiter,
		}),
		credentials:   cfg.Credentials,
		clientID:      cfg.ClientID,
		clientSecret:  cfg.ClientSecret,
		contaID:       cfg.ContaID,
		mapaSituacoes: cfg.MapaSituacoes,
	}, nil
}

func (b *Bling) Type() providers.ProviderType { return providers.ProviderTypeERP }
func (b *Bling) Name() providers.ProviderName { return providers.ProviderBling }

// ContaID expõe a identidade da conta para quem monta a chave de cota.
func (b *Bling) ContaID() string { return b.contaID }

// =============================================================================
// OAUTH
// =============================================================================

// BlingAuthorizeURL monta a URL de autorização.
//
// redirect_uri e scope NÃO são enviados de propósito: o Bling os IGNORA e usa
// sempre os do cadastro do aplicativo. Mandá-los sugere um controle que não
// temos e engana quem for depurar um callback que não chega.
func BlingAuthorizeURL(clientID, state string) string {
	p := url.Values{
		"response_type": {"code"},
		"client_id":     {clientID},
		"state":         {state},
	}
	return blingAuthorizeURL + "?" + p.Encode()
}

type blingTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int    `json:"expires_in"`
}

// BlingExchangeCode troca o authorization code pelos tokens.
//
// ⚠ SEM RETRY, e isso é regra e não descuido: a doc do Bling avisa que reusar um
// code ainda válido faz o usuário ter "o seu acesso revogado por medidas de
// segurança". Uma segunda tentativa não é uma chance a mais — é o risco de
// desconectar o lojista. O code vive 1 minuto.
func BlingExchangeCode(ctx context.Context, cli *http.Client, clientID, clientSecret, code string) (*providers.Credentials, error) {
	return blingTokenRequest(ctx, cli, clientID, clientSecret, url.Values{
		"grant_type": {"authorization_code"},
		"code":       {code},
	})
}

// BlingRefresh renova o access token. MEDIDO: o refresh token ROTACIONA, e o
// access token vale 6 HORAS (não 1) — o worker pode ser bem menos agressivo do
// que o do Tiny, o que importa porque o Bling bloqueia o IP por 60 minutos
// depois de 20 chamadas a /oauth/token em 60 segundos.
func BlingRefresh(ctx context.Context, cli *http.Client, clientID, clientSecret, refreshToken string) (*providers.Credentials, error) {
	return blingTokenRequest(ctx, cli, clientID, clientSecret, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

func blingTokenRequest(ctx context.Context, cli *http.Client, clientID, clientSecret string, form url.Values) (*providers.Credentials, error) {
	if cli == nil {
		// Cliente DEDICADO, nunca o BaseProvider.DoRequest: aquele grava request
		// e response em integration_logs, e o refresh_token iria em texto claro
		// para uma tabela que o painel lê.
		cli = &http.Client{Timeout: 30 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, blingTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	basic := base64.StdEncoding.EncodeToString([]byte(clientID + ":" + clientSecret))
	req.Header.Set("Authorization", "Basic "+basic)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// O token opaco foi descontinuado em favor de JWT; o header pede o modelo
	// novo explicitamente para não sermos migrados de surpresa.
	req.Header.Set("enable-jwt", "1")

	resp, err := cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bling: chamando o token endpoint: %w", err)
	}
	defer resp.Body.Close()
	corpo, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bling: token endpoint devolveu %d: %s", resp.StatusCode, blingErro(corpo))
	}

	var tr blingTokenResponse
	if err := json.Unmarshal(corpo, &tr); err != nil {
		return nil, fmt.Errorf("bling: resposta do token endpoint ilegível: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("bling: token endpoint respondeu 200 sem access_token")
	}

	return &providers.Credentials{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		// MEDIDO: o Bling devolve "bearer" MINÚSCULO. Normalizamos para que
		// nenhum consumidor precise comparar case-insensitive.
		TokenType: "Bearer",
		ExpiresAt: time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
		Extra: map[string]any{
			"scope": tr.Scope,
		},
	}, nil
}

func (b *Bling) RefreshToken(ctx context.Context) (*providers.Credentials, error) {
	if b.credentials == nil || b.credentials.RefreshToken == "" {
		return nil, fmt.Errorf("bling: sem refresh token — a loja precisa reconectar")
	}
	novo, err := BlingRefresh(ctx, nil, b.clientID, b.clientSecret, b.credentials.RefreshToken)
	if err != nil {
		return nil, err
	}
	// O Bling rotaciona o refresh token; se algum dia parar de rotacionar, a
	// resposta vem sem ele e perder o antigo desconectaria a loja.
	if novo.RefreshToken == "" {
		novo.RefreshToken = b.credentials.RefreshToken
	}
	b.credentials = novo
	return novo, nil
}

func (b *Bling) ValidateCredentials(ctx context.Context) error {
	_, err := b.Empresa(ctx)
	return err
}

func (b *Bling) TestConnection(ctx context.Context) (*providers.TestConnectionResult, error) {
	inicio := time.Now()
	emp, err := b.Empresa(ctx)
	if err != nil {
		return &providers.TestConnectionResult{
			Success: false, Message: err.Error(),
			Latency: time.Since(inicio), TestedAt: time.Now(),
		}, nil
	}
	return &providers.TestConnectionResult{
		Success: true,
		Message: "Conectado a " + emp.Nome,
		Latency: time.Since(inicio),
		AccountInfo: map[string]any{
			"empresa":    emp.Nome,
			"cnpj":       emp.CNPJ,
			"company_id": emp.ID,
		},
		TestedAt: time.Now(),
	}, nil
}

// =============================================================================
// EMPRESA — a identidade da conta
// =============================================================================

// BlingEmpresa é a identidade da conta conectada.
//
// ⚠ GET /empresas puro NÃO EXISTE. O único path da tag Empresas é
// /empresas/me/dados-basicos, e o `id` é STRING (32 hex), não inteiro.
type BlingEmpresa struct {
	ID    string `json:"id"`
	Nome  string `json:"nome"`
	CNPJ  string `json:"cnpj"`
	Email string `json:"email"`
}

func (b *Bling) Empresa(ctx context.Context) (*BlingEmpresa, error) {
	var env struct {
		Data BlingEmpresa `json:"data"`
	}
	if err := b.get(ctx, "/empresas/me/dados-basicos", nil, &env); err != nil {
		return nil, err
	}
	return &env.Data, nil
}

// =============================================================================
// PRODUTOS
// =============================================================================

type blingProduto struct {
	ID             int64   `json:"id"`
	IDProdutoPai   int64   `json:"idProdutoPai"`
	Nome           string  `json:"nome"`
	Codigo         string  `json:"codigo"`
	GTIN           string  `json:"gtin"`
	Preco          float64 `json:"preco"`
	Situacao       string  `json:"situacao"` // A ativo, I inativo
	Tipo           string  `json:"tipo"`     // P produto, S serviço
	Formato        string  `json:"formato"`  // S simples, V com variações, E composição
	DescricaoCurta string  `json:"descricaoCurta"`
	ImagemURL      string  `json:"imagemURL"`
	Unidade        string  `json:"unidade"`
	PesoBruto      float64 `json:"pesoBruto"`
	PesoLiquido    float64 `json:"pesoLiquido"`

	Estoque struct {
		// ⚠ MESMO NOME, DESCRIÇÃO OPOSTA à de /estoques/saldos: aqui o spec diz
		// "considerando a reserva de estoque"; lá diz "desconsiderando produtos
		// reservados". Ver blingSaldo.
		SaldoVirtualTotal float64 `json:"saldoVirtualTotal"`
	} `json:"estoque"`

	Dimensoes struct {
		Altura        float64 `json:"altura"`
		Largura       float64 `json:"largura"`
		Profundidade  float64 `json:"profundidade"`
		UnidadeMedida int     `json:"unidadeMedida"`
	} `json:"dimensoes"`

	Midia struct {
		Imagens struct {
			Internas []struct {
				Link          string `json:"link"`
				LinkMiniatura string `json:"linkMiniatura"`
				Validade      string `json:"validade"`
			} `json:"internas"`
			Externas []struct {
				Link string `json:"link"`
			} `json:"externas"`
		} `json:"imagens"`
	} `json:"midia"`

	Variacoes []blingProduto `json:"variacoes"`
}

// ListProducts lista o catálogo.
//
// ⚠ `criterio` e `tipo` vão SEMPRE explícitos. O default do Bling para
// `criterio` é 1 ("últimos incluídos") e ESCONDE produto — medido: numa conta
// com 2 produtos, o default devolveu 1. Confiar no default é perder catálogo
// sem perceber que perdeu.
func (b *Bling) ListProducts(ctx context.Context, params providers.ListProductsParams) (*providers.ProductListResult, error) {
	q := url.Values{}
	pagina := params.Page
	if pagina < 1 {
		pagina = 1
	}
	limite := params.PageSize
	if limite < 1 || limite > 100 {
		limite = 100 // o teto do Bling
	}
	q.Set("pagina", strconv.Itoa(pagina))
	q.Set("limite", strconv.Itoa(limite))

	// 2 = só ativos; 5 = todos. Nunca o default.
	if params.ActiveOnly {
		q.Set("criterio", "2")
	} else {
		q.Set("criterio", "5")
	}
	q.Set("tipo", "T")

	if params.Search != "" {
		q.Set("nome", params.Search)
	}
	if params.SKU != "" {
		q.Add("codigos[]", params.SKU)
	}
	if params.GTIN != "" {
		q.Add("gtins[]", params.GTIN)
	}
	if params.UpdatedAfter != nil {
		q.Set("dataAlteracaoInicial", params.UpdatedAfter.In(blingLocation).Format("2006-01-02 15:04:05"))
	}

	var env struct {
		Data []blingProduto `json:"data"`
	}
	if err := b.get(ctx, "/produtos", q, &env); err != nil {
		return nil, err
	}

	out := make([]providers.ERPProduct, 0, len(env.Data))
	for _, p := range env.Data {
		out = append(out, blingProdutoParaERP(p))
	}
	return &providers.ProductListResult{
		Products: out,
		Page:     pagina,
		PageSize: limite,
		// O Bling não devolve total nem cursor: "veio página cheia" é a única
		// pista de que há mais. Prometer TotalCount seria inventar.
		HasMore: len(env.Data) == limite,
	}, nil
}

func (b *Bling) GetProduct(ctx context.Context, productID string) (*providers.ERPProduct, error) {
	var env struct {
		Data blingProduto `json:"data"`
	}
	if err := b.get(ctx, "/produtos/"+url.PathEscape(productID), nil, &env); err != nil {
		return nil, err
	}
	p := blingProdutoParaERP(env.Data)

	// Produto com variações: o pai não vende, os filhos vendem. A grade vem em
	// /produtos/variacoes/{idPai}, não no GET do produto.
	if env.Data.Formato == "V" {
		p.IsParent = true
		filhos, err := b.variacoes(ctx, env.Data.ID)
		if err != nil {
			// A grade faltar não pode derrubar a leitura do pai — o chamador
			// decide se um pai sem filhos serve. Mas tem de aparecer no log.
			logger.From(ctx, b.Logger).Warn("bling: não consegui ler as variações do produto",
				zap.String("produto_id", productID), zap.Error(err))
		} else {
			p.Variants = filhos
		}
	}
	return &p, nil
}

func (b *Bling) variacoes(ctx context.Context, idPai int64) ([]providers.ERPProduct, error) {
	var env struct {
		Data blingProduto `json:"data"`
	}
	if err := b.get(ctx, "/produtos/variacoes/"+strconv.FormatInt(idPai, 10), nil, &env); err != nil {
		return nil, err
	}
	out := make([]providers.ERPProduct, 0, len(env.Data.Variacoes))
	for _, v := range env.Data.Variacoes {
		f := blingProdutoParaERP(v)
		f.ParentExternalID = strconv.FormatInt(idPai, 10)
		out = append(out, f)
	}
	return out, nil
}

func blingProdutoParaERP(p blingProduto) providers.ERPProduct {
	out := providers.ERPProduct{
		ID:   strconv.FormatInt(p.ID, 10),
		SKU:  p.Codigo, // ⚠ pode vir VAZIO — o vínculo é pelo ID, nunca pelo SKU
		GTIN: p.GTIN,
		Name: p.Nome,
		// float64 → centavos com arredondamento: 189.90*100 em binário é
		// 18989.999..., e truncar perderia um centavo por produto.
		Price:       int64(p.Preco*100 + 0.5),
		Description: p.DescricaoCurta,
		Active:      p.Situacao == "A",
		Type:        p.Formato,
	}

	// O saldo de /produtos é "considerando a reserva" segundo o spec. Ele é uma
	// leitura CONHECIDA — o campo existe e veio. Quem quiser o número
	// autoritativo usa GetProductStockBatch, que lê /estoques/saldos.
	out.Stock = int(p.Estoque.SaldoVirtualTotal)
	out.StockKnown = true

	if p.IDProdutoPai != 0 {
		out.ParentExternalID = strconv.FormatInt(p.IDProdutoPai, 10)
	}

	out.ImageURL, out.ImageURLs = blingImagens(p)
	out.Shipping, out.WeightGramsHint = blingFrete(p)
	return out
}

// blingImagens junta as imagens na ordem em que o Bling as devolve.
//
// ⚠ As `internas` são links S3 ASSINADOS que EXPIRAM — medido em 29/08/2026:
// a cheia em ~7 dias, a miniatura em ~30 MINUTOS. Guardar essas URLs é garantir
// imagem quebrada. Quem importa TEM de baixar e re-hospedar; ver
// BlingImagemExpira e o consumidor no fluxo de import.
//
// As `externas` são URLs do próprio lojista e não expiram — por isso vêm
// primeiro na lista quando existem.
func blingImagens(p blingProduto) (principal string, todas []string) {
	for _, e := range p.Midia.Imagens.Externas {
		if e.Link != "" {
			todas = append(todas, e.Link)
		}
	}
	for _, i := range p.Midia.Imagens.Internas {
		if i.Link != "" {
			todas = append(todas, i.Link)
		}
	}
	if len(todas) == 0 && p.ImagemURL != "" {
		todas = append(todas, p.ImagemURL)
	}
	if len(todas) > 0 {
		principal = todas[0]
	}
	return principal, todas
}

// blingFrete monta o perfil de frete. O Bling dá dimensões e peso no topo do
// produto; `unidadeMedida` diz a unidade das dimensões (1 = centímetro).
func blingFrete(p blingProduto) (*providers.ERPShippingProfile, int) {
	pesoKg := p.PesoBruto
	if pesoKg <= 0 {
		pesoKg = p.PesoLiquido
	}
	gramas := int(pesoKg*1000 + 0.5)

	d := p.Dimensoes
	if d.Altura <= 0 || d.Largura <= 0 || d.Profundidade <= 0 || gramas <= 0 {
		// Perfil incompleto: devolve só a dica de peso, para o serviço combinar
		// com as dimensões padrão da loja. Inventar dimensão é pior do que não ter.
		return nil, gramas
	}
	cm := func(v float64) int { return int(v + 0.5) }
	if d.UnidadeMedida == 2 { // metro
		cm = func(v float64) int { return int(v*100 + 0.5) }
	}
	return &providers.ERPShippingProfile{
		WeightGrams: gramas,
		HeightCm:    cm(d.Altura),
		WidthCm:     cm(d.Largura),
		LengthCm:    cm(d.Profundidade),
	}, gramas
}

// =============================================================================
// ESTOQUE
// =============================================================================

type blingSaldo struct {
	Produto struct {
		ID     int64  `json:"id"`
		Codigo string `json:"codigo"`
	} `json:"produto"`
	SaldoFisicoTotal float64 `json:"saldoFisicoTotal"`
	// ⚠ O spec descreve este campo como "DESCONSIDERANDO produtos reservados",
	// e descreve o campo de MESMO NOME em /produtos como "CONSIDERANDO a
	// reserva". Medido em 29/08/2026 numa conta SEM reserva ligada: os dois
	// números batem. Com reserva ligada eles divergem, e é aí que a diferença
	// importa. Ver ERPStockDetail.Reserved.
	SaldoVirtualTotal float64 `json:"saldoVirtualTotal"`
	Depositos         []struct {
		ID           int64   `json:"id"`
		SaldoFisico  float64 `json:"saldoFisico"`
		SaldoVirtual float64 `json:"saldoVirtual"`
	} `json:"depositos"`
}

// GetProductStockBatch lê o saldo de VÁRIOS produtos numa requisição.
//
// É a vantagem real do Bling sobre o Tiny, onde é 1 GET por produto e foi a
// fonte dos 429: 300 produtos custam ~3 requisições em vez de 300.
//
// ⚠ `filtroSaldoEstoque` vai EXPLÍCITO. O default do Bling é 1 (só positivo), e
// com ele um produto ESGOTADO simplesmente não vem na resposta. Ausência não é
// zero e não é "não sei" — é consequência do filtro. Por isso pedimos os três
// filtros e unimos: só assim ausência volta a significar "o Bling não conhece
// este produto".
func (b *Bling) GetProductStockBatch(ctx context.Context, externalIDs []string) (map[string]providers.ERPStockDetail, error) {
	out := make(map[string]providers.ERPStockDetail, len(externalIDs))
	if len(externalIDs) == 0 {
		return out, nil
	}

	// 0 zerado, 1 positivo, 2 negativo — a união dos três é o catálogo inteiro.
	for _, filtro := range []string{"1", "0", "2"} {
		q := url.Values{}
		for _, id := range externalIDs {
			q.Add("idsProdutos[]", id)
		}
		q.Set("filtroSaldoEstoque", filtro)

		var env struct {
			Data []blingSaldo `json:"data"`
		}
		if err := b.get(ctx, "/estoques/saldos", q, &env); err != nil {
			return nil, err
		}
		for _, s := range env.Data {
			id := strconv.FormatInt(s.Produto.ID, 10)
			if _, jaTem := out[id]; jaTem {
				continue
			}
			fisico := int(s.SaldoFisicoTotal)
			virtual := int(s.SaldoVirtualTotal)
			reservado := fisico - virtual
			if reservado < 0 {
				reservado = 0
			}
			out[id] = providers.ERPStockDetail{
				Balance:  fisico,
				Reserved: reservado,
				// O disponível é o VIRTUAL, nunca o físico: o físico conta peça
				// já reservada por outro pedido, e vendê-la é oversell.
				Available: virtual,
			}
		}
	}
	return out, nil
}

// GetProductStock devolve o saldo DISPONÍVEL de um produto.
func (b *Bling) GetProductStock(ctx context.Context, productID string) (int, error) {
	d, err := b.GetProductStockDetail(ctx, productID)
	if err != nil {
		return 0, err
	}
	return d.Available, nil
}

func (b *Bling) GetProductStockDetail(ctx context.Context, productID string) (providers.ERPStockDetail, error) {
	m, err := b.GetProductStockBatch(ctx, []string{productID})
	if err != nil {
		return providers.ERPStockDetail{}, err
	}
	d, ok := m[productID]
	if !ok {
		return providers.ERPStockDetail{}, fmt.Errorf("bling: produto %s não veio na resposta de saldo", productID)
	}
	return d, nil
}

// BlingDeposito é um depósito da conta.
type BlingDeposito struct {
	ID                 int64  `json:"id"`
	Descricao          string `json:"descricao"`
	Situacao           int    `json:"situacao"`
	Padrao             bool   `json:"padrao"`
	DesconsiderarSaldo bool   `json:"desconsiderarSaldo"`
}

func (b *Bling) Depositos(ctx context.Context) ([]BlingDeposito, error) {
	var env struct {
		Data []BlingDeposito `json:"data"`
	}
	if err := b.get(ctx, "/depositos", nil, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// =============================================================================
// HTTP
// =============================================================================

var blingLocation = time.FixedZone("America/Sao_Paulo", -3*60*60)

func (b *Bling) authHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + b.credentials.AccessToken,
		"Accept":        "application/json",
	}
}

func (b *Bling) get(ctx context.Context, caminho string, q url.Values, destino any) error {
	endereco := blingAPIBaseURL + caminho
	if len(q) > 0 {
		endereco += "?" + q.Encode()
	}

	// DoRequestRetrying429 e não DoRequestWithRetry: aquele desiste quando a
	// espera não cabe no prazo do chamador, em vez de dormir 60 s dentro de um
	// checkout. O Bling NÃO manda Retry-After (medido), então o fallback
	// hardcoded seria uma bomba.
	resp, corpo, err := b.DoRequestRetrying429(ctx, 2, http.MethodGet, endereco, nil, b.authHeaders())
	if err != nil {
		return fmt.Errorf("bling GET %s: %w", caminho, err)
	}
	if resp.StatusCode >= 400 {
		return blingErroDeStatus(resp.StatusCode, corpo, resp.Header.Get("x-amzn-RequestId"))
	}
	if destino == nil {
		return nil
	}
	if err := json.Unmarshal(corpo, destino); err != nil {
		return fmt.Errorf("bling GET %s: resposta ilegível: %w", caminho, err)
	}
	return nil
}

// blingErro extrai a mensagem do envelope de erro do Bling, que é aninhado:
// {"error":{"type":"...","message":"...","description":"..."}}
// blingErro resume o corpo de erro do Bling.
//
// `fields[]` é o que importa e era JOGADO FORA. Sem ele, um PUT recusado dizia
// apenas "A venda não pode ser salva, pois ocorreram problemas em sua
// validação" — verdadeiro, inútil, e idêntico para uma dúzia de causas
// diferentes. Foi preciso reproduzir o caso na conta real e ler o corpo cru com
// curl para descobrir que o motivo era "O somatório do valor das parcelas
// difere do total da venda". Esse diagnóstico agora sai no log.
func blingErro(corpo []byte) string {
	var e struct {
		Error struct {
			Type        string `json:"type"`
			Description string `json:"description"`
			Fields      []struct {
				Msg     string `json:"msg"`
				Element string `json:"element"`
				Code    int    `json:"code"`
			} `json:"fields"`
		} `json:"error"`
	}
	if json.Unmarshal(corpo, &e) == nil && e.Error.Type != "" {
		out := e.Error.Type
		if e.Error.Description != "" && e.Error.Description != e.Error.Type {
			out += ": " + e.Error.Description
		}
		if len(e.Error.Fields) > 0 {
			detalhes := make([]string, 0, len(e.Error.Fields))
			for _, f := range e.Error.Fields {
				d := f.Msg
				if f.Element != "" {
					d = f.Element + ": " + d
				}
				if f.Code != 0 {
					d = fmt.Sprintf("%s (code %d)", d, f.Code)
				}
				detalhes = append(detalhes, d)
			}
			out += " [" + strings.Join(detalhes, "; ") + "]"
		}
		return out
	}
	s := strings.TrimSpace(string(corpo))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

func blingErroDeStatus(status int, corpo []byte, requestID string) error {
	msg := blingErro(corpo)
	base := fmt.Errorf("bling: HTTP %d — %s", status, msg)
	if requestID != "" {
		base = fmt.Errorf("%w (x-amzn-RequestId: %s)", base, requestID)
	}

	switch {
	case status == http.StatusUnauthorized:
		return base
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("%w — o teto do Bling é 3 req/s POR CONTA somando TODOS os apps do lojista, "+
			"e não há Retry-After para reconciliar", base)
	case status >= 400 && status < 500:
		// 4xx é recusa de validação: o provedor PROCESSOU e rejeitou antes de
		// aplicar. Marcar como comprovadamente não entregue é o que deixa o
		// chamador repetir com segurança.
		return fmt.Errorf("%w: %w", providers.ErrProvenUndelivered, base)
	default:
		// 5xx e timeout ficam de fora de propósito: o provedor pode ter
		// aplicado e falhado só em responder.
		return base
	}
}
