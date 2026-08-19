package erp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
	"livecart/apps/api/lib/ratelimit"
)

// Tiny API v3 base URL. `var` e não `const` só para o teste poder apontar o
// provider a um servidor local — nada em produção reatribui isto.
var tinyAPIBaseURL = "https://api.tiny.com.br/public-api/v3"

// Tiny is a Brazilian ERP and interprets `data` fields against São Paulo
// local time. Sending UTC made orders created late at night land on the next
// day from Tiny's perspective, putting them outside the merchant's "últimos 30
// dias" filter. Brazil dropped DST in 2019, so a fixed -3h offset is correct.
var tinyLocation = time.FixedZone("America/Sao_Paulo", -3*60*60)

// OAuth URLs for Tiny
const (
	tinyAuthURL  = "https://accounts.tiny.com.br/realms/tiny/protocol/openid-connect/auth"
	tinyTokenURL = "https://accounts.tiny.com.br/realms/tiny/protocol/openid-connect/token"
)

// Tiny implements the ERPProvider interface for Tiny ERP using API v3.
type Tiny struct {
	*providers.BaseProvider
	credentials  *Credentials
	clientID     string
	clientSecret string

	// useAvailableStock faz o provider espelhar o saldo VENDÁVEL em vez do
	// físico. Desligado por padrão: mudar o significado do estoque de uma loja
	// sem ela pedir é alterar o que ela vende.
	useAvailableStock bool
}

// TinyConfig contains configuration for the Tiny provider.
type TinyConfig struct {
	IntegrationID string
	StoreID       string
	Credentials   *Credentials
	ClientID      string
	ClientSecret  string
	Logger        *zap.Logger
	LogFunc       providers.LogFunc
	RateLimiter   ratelimit.RateLimiter
	// UseAvailableStock liga a leitura do saldo disponível. Ver o campo
	// homônimo em Tiny.
	UseAvailableStock bool
}

// NewTiny creates a new Tiny ERP provider.
func NewTiny(cfg TinyConfig) (*Tiny, error) {
	if cfg.Credentials == nil {
		return nil, fmt.Errorf("credentials are required")
	}
	if cfg.Credentials.AccessToken == "" {
		return nil, fmt.Errorf("access_token is required")
	}

	return &Tiny{
		useAvailableStock: cfg.UseAvailableStock,
		BaseProvider: providers.NewBaseProvider(providers.BaseProviderConfig{
			IntegrationID: cfg.IntegrationID,
			StoreID:       cfg.StoreID,
			Logger:        cfg.Logger,
			LogFunc:       cfg.LogFunc,
			Timeout:       30 * time.Second,
			RateLimiter:   cfg.RateLimiter,
		}),
		credentials:  cfg.Credentials,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
	}, nil
}

// Type returns the provider type.
func (t *Tiny) Type() providers.ProviderType {
	return providers.ProviderTypeERP
}

// Name returns the provider name.
func (t *Tiny) Name() providers.ProviderName {
	return providers.ProviderTiny
}

// ValidateCredentials validates the API token by making a test request.
func (t *Tiny) ValidateCredentials(ctx context.Context) error {
	params := ListProductsParams{
		PageSize: 1,
	}

	_, err := t.ListProducts(ctx, params)
	if err != nil {
		return fmt.Errorf("invalid credentials: %w", err)
	}
	return nil
}

// RefreshToken refreshes the OAuth access token using the refresh token.
func (t *Tiny) RefreshToken(ctx context.Context) (*Credentials, error) {
	if t.credentials.RefreshToken == "" {
		return nil, fmt.Errorf("no refresh token available")
	}

	// Get client_id and client_secret from stored credentials or config
	clientID := t.clientID
	clientSecret := t.clientSecret
	if clientID == "" {
		if id, ok := t.credentials.Extra["client_id"].(string); ok {
			clientID = id
		}
	}
	if clientSecret == "" {
		if secret, ok := t.credentials.Extra["client_secret"].(string); ok {
			clientSecret = secret
		}
	}

	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("client_id or client_secret not available")
	}

	data := fmt.Sprintf(
		"grant_type=refresh_token&client_id=%s&client_secret=%s&refresh_token=%s",
		clientID,
		clientSecret,
		t.credentials.RefreshToken,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tinyTokenURL, strings.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("creating refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing refresh request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh token failed: status %d, body: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}

	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}

	// Log token refresh result for debugging
	logger.From(ctx, t.Logger).Info("Tiny token refresh successful",
		zap.Int("expires_in", tokenResp.ExpiresIn),
		zap.Bool("has_new_refresh_token", tokenResp.RefreshToken != ""),
	)

	// Default to 4 hours if expires_in is 0 or not provided
	// Tiny access tokens typically last about 4 hours
	expiresInSeconds := tokenResp.ExpiresIn
	if expiresInSeconds <= 0 {
		logger.From(ctx, t.Logger).Warn("Tiny token refresh: expires_in is 0 or negative, defaulting to 4 hours",
			zap.Int("original_expires_in", tokenResp.ExpiresIn),
		)
		expiresInSeconds = 14400 // 4 hours in seconds
	}

	// Preserve client_id and client_secret in the new credentials
	expiresAt := time.Now().Add(time.Duration(expiresInSeconds) * time.Second)
	return &Credentials{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		TokenType:    tokenResp.TokenType,
		ExpiresAt:    expiresAt,
		Extra: map[string]any{
			"client_id":     clientID,
			"client_secret": clientSecret,
		},
	}, nil
}

// TestConnection tests the connection to Tiny API.
func (t *Tiny) TestConnection(ctx context.Context) (*providers.TestConnectionResult, error) {
	start := time.Now()
	endpoint := tinyAPIBaseURL + "/info"

	resp, body, err := t.DoRequest(ctx, http.MethodGet, endpoint, nil, t.authHeaders())
	latency := time.Since(start)

	result := &providers.TestConnectionResult{
		Latency:  latency,
		TestedAt: time.Now(),
	}

	if err != nil {
		result.Success = false
		result.Message = fmt.Sprintf("Falha na conexão: %v", err)
		return result, nil
	}

	if resp.StatusCode == http.StatusUnauthorized {
		result.Success = false
		result.Message = "API Key inválida"
		return result, nil
	}

	if resp.StatusCode == http.StatusForbidden {
		result.Success = false
		result.Message = "Acesso negado - verifique as permissões da API Key"
		return result, nil
	}

	if !providers.IsSuccessStatus(resp.StatusCode) {
		result.Success = false
		result.Message = fmt.Sprintf("Erro na API: status %d", resp.StatusCode)
		return result, nil
	}

	// Parse account info
	var info struct {
		Empresa struct {
			Nome   string `json:"nome"`
			CNPJ   string `json:"cnpj"`
			Cidade string `json:"cidade"`
			UF     string `json:"uf"`
		} `json:"empresa"`
		Plano struct {
			Nome string `json:"nome"`
		} `json:"plano"`
	}
	if err := json.Unmarshal(body, &info); err == nil && info.Empresa.Nome != "" {
		result.AccountInfo = map[string]any{
			"empresa": info.Empresa.Nome,
			"cnpj":    info.Empresa.CNPJ,
			"cidade":  info.Empresa.Cidade,
			"uf":      info.Empresa.UF,
			"plano":   info.Plano.Nome,
		}
	}

	result.Success = true
	result.Message = "Conexão estabelecida com sucesso"
	return result, nil
}

// ListProducts retrieves products from Tiny using API v3.
func (t *Tiny) ListProducts(ctx context.Context, params ListProductsParams) (*ProductListResult, error) {
	endpoint := tinyAPIBaseURL + "/produtos"

	// Build query string. Every value MUST be URL-encoded — a previous version
	// concatenated raw search terms which made any query containing a space or
	// accent (`"Camiseta Tech"`, `"Coração"`) malformed. Tiny then rejected
	// the request with HTTP 400 and an empty body, which our service layer
	// surfaced as a 500 to the user.
	q := url.Values{}
	if params.PageSize > 0 {
		q.Set("limit", strconv.Itoa(params.PageSize))
	}
	switch {
	case params.GTIN != "":
		q.Set("gtin", params.GTIN)
	case params.SKU != "":
		q.Set("codigo", params.SKU)
	case params.Search != "":
		q.Set("nome", params.Search)
	}
	if params.ActiveOnly {
		q.Set("situacao", "A")
	}
	if params.UpdatedAfter != nil {
		q.Set("dataAlteracao", params.UpdatedAfter.Format("2006-01-02 15:04:05"))
	}

	fullURL := endpoint
	if encoded := q.Encode(); encoded != "" {
		fullURL += "?" + encoded
	}

	resp, body, err := t.DoRequest(ctx, http.MethodGet, fullURL, nil, t.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("listing products: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("unauthorized: invalid token")
	}
	if resp.StatusCode == http.StatusNoContent {
		return &ProductListResult{
			Products:   []ERPProduct{},
			TotalCount: 0,
			HasMore:    false,
		}, nil
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		// Surface as a typed rate-limit error so the search service can treat
		// it as a "no results" partial failure instead of escalating to 500.
		// Reuses the same sentinel BaseProvider.DoRequest emits after retries
		// so handleProviderError keeps the consistent integration-state path.
		return nil, &ratelimit.ErrRateLimited{}
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("list products failed: status %d, body: %s", resp.StatusCode, string(body))
	}

	var tinyResp struct {
		Itens []struct {
			ID            int64  `json:"id"`
			SKU           string `json:"sku"`
			Descricao     string `json:"descricao"`
			Tipo          string `json:"tipo"`
			Situacao      string `json:"situacao"` // "A" = Ativo, "I" = Inativo, "E" = Excluído
			GTIN          string `json:"gtin"`
			DataCriacao   string `json:"dataCriacao"`
			DataAlteracao string `json:"dataAlteracao"`
			Precos        struct {
				Preco            float64 `json:"preco"`
				PrecoPromocional float64 `json:"precoPromocional"`
			} `json:"precos"`
		} `json:"itens"`
		Paginacao struct {
			Limit  int `json:"limit"`
			Offset int `json:"offset"`
			Total  int `json:"total"`
		} `json:"paginacao"`
	}

	if err := json.Unmarshal(body, &tinyResp); err != nil {
		return nil, fmt.Errorf("parsing products response: %w", err)
	}

	products := make([]ERPProduct, len(tinyResp.Itens))
	for i, p := range tinyResp.Itens {
		price := p.Precos.Preco
		if p.Precos.PrecoPromocional > 0 {
			price = p.Precos.PrecoPromocional
		}

		var updatedAt time.Time
		if p.DataAlteracao != "" {
			updatedAt, _ = time.Parse("2006-01-02 15:04:05", p.DataAlteracao)
		}

		products[i] = ERPProduct{
			ID:        strconv.FormatInt(p.ID, 10),
			SKU:       p.SKU,
			GTIN:      p.GTIN,
			Name:      p.Descricao,
			Price:     int64(math.Round(price * 100)), // Convert to cents
			Stock:     0,                              // Not available in list response — enriched via GetProduct
			Active:    p.Situacao == "A",
			UpdatedAt: updatedAt,
			Type:      p.Tipo,
			IsParent:  p.Tipo == "V",
		}
	}

	hasMore := tinyResp.Paginacao.Offset+tinyResp.Paginacao.Limit < tinyResp.Paginacao.Total

	return &ProductListResult{
		Products:   products,
		TotalCount: tinyResp.Paginacao.Total,
		Page:       tinyResp.Paginacao.Offset / max(tinyResp.Paginacao.Limit, 1),
		PageSize:   tinyResp.Paginacao.Limit,
		HasMore:    hasMore,
	}, nil
}

// GetProduct retrieves a single product by ID.
// saldoDisponivel busca em `GET /estoque/{idProduto}` o saldo que pode de fato
// ser vendido.
//
// Devolve (0, false) sempre que não dá para afirmar: chamada falhou, resposta
// ilegível, ou nenhum dos campos conhecidos apareceu. Quem chama preserva o
// saldo físico nesse caso — que é o comportamento de hoje, e errar para o lado
// de "vende demais" é ruim, mas trocar por um número inventado é pior.
//
// O corpo cru vai para o log porque o schema desta resposta não está na
// documentação pública que consultei. Assim que os nomes estiverem confirmados,
// o log sai e a lista de candidatos encolhe para o campo real.
// GetProductStock devolve o SALDO físico do produto no Tiny — o campo que a
// reconciliação compara com a contabilidade local. Saldo, e não disponível, de
// propósito: as nossas reservas são saídas manuais e descontam do saldo; o
// "disponível" ainda subtrai as reservas de PEDIDO da própria Tiny, que não são
// nossas e poluiriam a comparação.
func (t *Tiny) GetProductStock(ctx context.Context, productID string) (int, error) {
	endpoint := fmt.Sprintf("%s/estoque/%s", tinyAPIBaseURL, productID)
	resp, body, err := t.DoRequest(ctx, http.MethodGet, endpoint, nil, t.authHeaders())
	if err != nil {
		return 0, fmt.Errorf("reading stock: %w", err)
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return 0, fmt.Errorf("reading stock: status %d", resp.StatusCode)
	}
	var out struct {
		Saldo float64 `json:"saldo"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("parsing stock response: %w", err)
	}
	return int(out.Saldo), nil
}

func (t *Tiny) saldoDisponivel(ctx context.Context, productID string) (int, bool) {
	endpoint := fmt.Sprintf("%s/estoque/%s", tinyAPIBaseURL, productID)
	resp, body, err := t.DoRequest(ctx, http.MethodGet, endpoint, nil, t.authHeaders())
	if err != nil || !providers.IsSuccessStatus(resp.StatusCode) {
		// O status importa e faltava aqui: sem ele, um 404 (produto sem controle
		// de estoque) e um 429 (estrangulado) viram a mesma linha muda, e as duas
		// pedem coisas diferentes. Enquanto isso o produto é oferecido pelo saldo
		// FÍSICO — de novo o furo que esta função existe para fechar —, então a
		// linha precisa dizer o suficiente para agir.
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		if t.Logger != nil {
			t.Logger.Warn("tiny available stock unavailable; falling back to physical",
				zap.String("external_product_id", productID),
				zap.Int("status", status),
				zap.ByteString("body", body),
				zap.Error(err),
			)
		}
		return 0, false
	}

	var cru map[string]any
	if json.Unmarshal(body, &cru) != nil {
		return 0, false
	}

	n, campo, ok := ExtrairSaldoDisponivel(cru)
	if !ok {
		if t.Logger != nil {
			// Com o corpo. Sem ele esta linha só diz que falhou, e o produto segue
			// sendo oferecido pelo saldo FÍSICO — que é exatamente o furo. Dois
			// produtos caíram aqui na varredura de 14/08 e não sobrou nada para
			// descobrir por quê.
			t.Logger.Warn("tiny stock endpoint had no known available-balance field",
				zap.String("external_product_id", productID),
				zap.ByteString("body", body),
			)
		}
		return 0, false
	}
	if t.Logger != nil {
		// Os três números juntos: é a conta que decide o que a loja pode vender,
		// e vê-la inteira poupa arqueologia quando um saldo parecer errado.
		t.Logger.Info("tiny available stock resolved",
			zap.String("external_product_id", productID),
			zap.String("campo", campo),
			zap.Any("saldo", cru["saldo"]),
			zap.Any("reservado", cru["reservado"]),
			zap.Int("disponivel", n),
		)
	}
	return n, true
}

// ExtrairSaldoDisponivel escolhe, na resposta de estoque do Tiny, o campo que
// representa o saldo VENDÁVEL.
//
// O schema não está na documentação pública; foi confirmado por resposta de
// produção em 14/08/2026:
//
//	{"id":830590845,"nome":"Carrossel Musical Azul - 17cm","codigo":"3583A",
//	 "unidade":"UN","saldo":4,"reservado":1,"disponivel":3,"depositos":[...]}
//
// `saldo` fica de fora de propósito: é o saldo FÍSICO, o mesmo número que já vem
// em `estoque.quantidade` do GET /produtos e que causou o furo. Aceitá-lo aqui
// recriaria o bug com outro nome e com uma chamada HTTP a mais de custo.
//
// Negativo não é saldo, é sintoma — o Tiny aceita ir abaixo de zero, e copiar
// isso propagaria o defeito em vez de mostrá-lo.
func ExtrairSaldoDisponivel(cru map[string]any) (saldo int, campo string, ok bool) {
	for _, c := range []string{"disponivel"} {
		v, existe := cru[c]
		if !existe {
			continue
		}
		n, isNum := v.(float64)
		if !isNum || n < 0 {
			continue
		}
		return int(n), c, true
	}
	return 0, "", false
}

func (t *Tiny) GetProduct(ctx context.Context, productID string) (*ERPProduct, error) {
	endpoint := fmt.Sprintf("%s/produtos/%s", tinyAPIBaseURL, productID)

	resp, body, err := t.DoRequest(ctx, http.MethodGet, endpoint, nil, t.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("getting product: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("product not found: %s", productID)
	}
	// 429 tipado, como em ListProducts. Sem isto o estrangulamento do Tiny
	// (1 req/s) chegava ao chamador como um erro genérico indistinguível de
	// "esse produto deu problema" — e a busca, que faz um GetProduct POR
	// resultado, descartava produto por produto até sobrar zero e dizer ao
	// lojista que o produto não existe no ERP.
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &ratelimit.ErrRateLimited{}
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("get product failed: status %d", resp.StatusCode)
	}

	var p tinyProductPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("parsing product response: %w", err)
	}

	out := tinyPayloadToERP(p)

	// O saldo que vale para vender é o DISPONÍVEL, e ele não vem aqui.
	//
	// `GET /produtos/{id}` devolve `estoque.quantidade`, que é o saldo FÍSICO —
	// provado pelo lojista e confirmado no log: o Carrossel Musical estava com
	// físico 4 e disponível 3 no Tiny, e chegou aqui como 4. O nó inteiro é
	// {controlar, sobEncomenda, diasPreparacao, localizacao, minimo, maximo,
	// quantidade}: não existe reservado nem disponível para parsear.
	//
	// A diferença entre os dois é peça reservada por orçamento salvo no Tiny.
	// Ela continua no físico e sai do disponível — oferecê-la é vender o que já
	// tem dono, e é furo de estoque do tamanho de quantas estiverem reservadas.
	// Só quando a loja pediu. Desligado, o comportamento é exatamente o de
	// antes — inclusive sem a chamada extra ao Tiny.
	if t.useAvailableStock {
		if disponivel, ok := t.saldoDisponivel(ctx, productID); ok {
			out.Stock = disponivel
		}
		// A regra vale para CADA VARIAÇÃO, não só para o pai.
		//
		// O bloco acima trocava apenas `out.Stock` — o saldo do produto pai. As
		// variações seguiam com o `estoque.quantidade` que veio dentro do payload
		// do pai, que é o FÍSICO: aquela resposta não tem reservado nem
		// disponível para parsear, então não havia como corrigi-las depois sem
		// perguntar por cada uma.
		//
		// A loja ligou a configuração e produto com variação continuou chegando
		// com o físico. Resolver aqui é resolver para todo mundo: quem lê
		// `Variants[].Stock` — a tela de importação, o grupo de produtos, a
		// varredura — passa a receber o vendável sem precisar lembrar da regra.
		//
		// Uma consulta por variação, pelo mesmo limitador do resto. Só quando a
		// loja pediu, e só quando o produto tem variação; falha em uma não
		// contamina as outras nem derruba o produto (mantém o que veio, que é o
		// comportamento de sempre quando não dá para afirmar).
		for i := range out.Variants {
			if out.Variants[i].ID == "" {
				continue
			}
			if disponivel, ok := t.saldoDisponivel(ctx, out.Variants[i].ID); ok {
				out.Variants[i].Stock = disponivel
			}
		}
	}

	// TEMP DEBUG: log shipping resolution so we can pinpoint why some Tiny
	// products land in LiveCart with no dimensions. Remove once the variation
	// sync flow is confirmed working in production.
	if t.Logger != nil {
		hasDim := p.Dimensoes != nil
		hasFlat := p.Peso > 0 || p.Altura > 0 || p.Largura > 0 || p.Profundidade > 0
		hasShipping := out.Shipping != nil
		fields := []zap.Field{
			zap.String("tiny_id", out.ID),
			zap.String("tipo", p.Tipo),
			zap.Bool("has_dimensoes_block", hasDim),
			zap.Bool("has_flat_dimensions", hasFlat),
			zap.Bool("resulting_shipping_set", hasShipping),
			zap.String("parent_external_id", out.ParentExternalID),
		}
		if hasDim {
			fields = append(fields,
				zap.Float64("dim_largura", p.Dimensoes.Largura),
				zap.Float64("dim_altura", p.Dimensoes.Altura),
				zap.Float64("dim_comprimento", p.Dimensoes.Comprimento),
				zap.Float64("dim_peso_bruto", p.Dimensoes.PesoBruto),
				zap.Float64("dim_peso_liquido", p.Dimensoes.PesoLiquido),
			)
			if p.Dimensoes.Embalagem != nil {
				fields = append(fields,
					zap.String("embalagem_tipo_raw", string(p.Dimensoes.Embalagem.Tipo)),
					zap.String("embalagem_nome", p.Dimensoes.Embalagem.Nome),
					zap.String("embalagem_resolved", mapTinyEmbalagem(p.Dimensoes.Embalagem)),
				)
			} else {
				fields = append(fields, zap.String("embalagem", "<nil>"))
			}
		}
		if hasFlat {
			fields = append(fields,
				zap.Float64("flat_peso", p.Peso),
				zap.Float64("flat_altura", p.Altura),
				zap.Float64("flat_largura", p.Largura),
				zap.Float64("flat_profundidade", p.Profundidade),
			)
		}
		if !hasDim && !hasFlat {
			// dump first 800 chars of raw body so we see exactly what Tiny sent
			snippet := string(body)
			if len(snippet) > 800 {
				snippet = snippet[:800]
			}
			fields = append(fields, zap.String("raw_body_snippet", snippet))
		}
		logger.From(ctx, t.Logger).Info("tiny GetProduct shipping resolution", fields...)
	}

	return &out, nil
}

// tinyProductPayload mirrors Tiny v3 GET /produtos/{id} response, including the
// variation fields documented at https://erp.tiny.com.br/public-api/v3/swagger/swagger.json.
type tinyProductPayload struct {
	ID                    int64  `json:"id"`
	SKU                   string `json:"sku"`
	Descricao             string `json:"descricao"`
	DescricaoComplementar string `json:"descricaoComplementar"`
	Situacao              string `json:"situacao"`
	Tipo                  string `json:"tipo"` // S, V, K, F, M
	GTIN                  string `json:"gtin"`
	DataAlteracao         string `json:"dataAlteracao"`
	Precos                struct {
		Preco            float64 `json:"preco"`
		PrecoPromocional float64 `json:"precoPromocional"`
	} `json:"precos"`
	Estoque struct {
		Quantidade float64 `json:"quantidade"`
	} `json:"estoque"`
	Anexos []struct {
		URL     string `json:"url"`
		Externo bool   `json:"externo"`
	} `json:"anexos"`
	Dimensoes  *tinyDimensoes       `json:"dimensoes"`  // physical profile for parents/simples (DimensoesProdutoResponseModel)
	Grade      []string             `json:"grade"`      // dimension keys for parents (tipo=V), e.g. ["Tamanho","Cor"]
	ProdutoPai *tinyParentRef       `json:"produtoPai"` // present when this is a child variation
	Variacoes  []tinyVariantPayload `json:"variacoes"`  // children when tipo=V

	// Some Tiny endpoints (notably GET /produtos/{idVariacao}) return dimensions
	// as flat top-level fields instead of inside `dimensoes`. We capture both
	// shapes and resolve at mapping time.
	Peso         float64 `json:"peso"`
	Altura       float64 `json:"altura"`
	Largura      float64 `json:"largura"`
	Profundidade float64 `json:"profundidade"`
}

// tinyDimensoes mirrors DimensoesProdutoResponseModel: weight in kilograms,
// dimensions in centimeters. Used by parent/simple products.
type tinyDimensoes struct {
	Embalagem   *tinyEmbalagem `json:"embalagem"`
	Largura     float64        `json:"largura"`
	Altura      float64        `json:"altura"`
	Comprimento float64        `json:"comprimento"`
	Diametro    *float64       `json:"diametro"`
	PesoLiquido float64        `json:"pesoLiquido"`
	PesoBruto   float64        `json:"pesoBruto"`
}

// tinyEmbalagem is intentionally permissive — in practice Tiny v3 returns
// `tipo` as either a string ("caixa", "envelope") OR a numeric enum id, and
// the swagger does not pin down which. We capture it as RawMessage and resolve
// at mapping time. `nome` carries the human label and is the most reliable
// signal when present.
type tinyEmbalagem struct {
	Tipo json.RawMessage `json:"tipo"`
	Nome string          `json:"nome"`
}

type tinyParentRef struct {
	ID  int64  `json:"id"`
	SKU string `json:"sku"`
}

type tinyVariantPayload struct {
	ID        int64  `json:"id"`
	Descricao string `json:"descricao"`
	SKU       string `json:"sku"`
	GTIN      string `json:"gtin"`
	Precos    struct {
		Preco            float64 `json:"preco"`
		PrecoPromocional float64 `json:"precoPromocional"`
	} `json:"precos"`
	Estoque struct {
		Quantidade float64 `json:"quantidade"`
	} `json:"estoque"`
	// Variant physical profile, returned flat by Tiny (NOT inside `dimensoes`)
	// per the example in CriarProdutoComVariacoesResponseModel. Weight is in
	// kilograms; dimensions are in centimeters; "profundidade" maps to length.
	Peso         float64 `json:"peso"`
	Altura       float64 `json:"altura"`
	Largura      float64 `json:"largura"`
	Profundidade float64 `json:"profundidade"`
	// Grade for variants is returned as an object map ({"Cor":"Azul","Tamanho":"M"})
	// in some Tiny endpoints — capture both shapes.
	GradeMap map[string]string `json:"-"`
	GradeRaw json.RawMessage   `json:"grade"`
}

func tinyPayloadToERP(p tinyProductPayload) ERPProduct {
	price := p.Precos.Preco
	if p.Precos.PrecoPromocional > 0 {
		price = p.Precos.PrecoPromocional
	}

	var updatedAt time.Time
	if p.DataAlteracao != "" {
		updatedAt, _ = time.Parse("2006-01-02 15:04:05", p.DataAlteracao)
	}

	var imageURL string
	for _, a := range p.Anexos {
		if a.URL != "" {
			imageURL = a.URL
			break
		}
	}

	// Dimensions: prefer the structured `dimensoes` block; fall back to
	// top-level flat fields (peso/altura/largura/profundidade) which Tiny
	// returns when the product is a variation fetched individually.
	shipping := dimensoesToShipping(p.Dimensoes)
	if shipping == nil {
		shipping = flatDimensionsToShipping(p.Peso, p.Altura, p.Largura, p.Profundidade)
	}

	// Capture weight even when dimensions are missing — the integration service
	// can complete the profile using store-level default dimensions.
	weightHint := topLevelWeightHintGrams(p)

	prod := ERPProduct{
		ID:              strconv.FormatInt(p.ID, 10),
		SKU:             p.SKU,
		GTIN:            p.GTIN,
		Name:            p.Descricao,
		Description:     p.DescricaoComplementar,
		Price:           int64(math.Round(price * 100)),
		Stock:           int(p.Estoque.Quantidade),
		Active:          p.Situacao == "A",
		ImageURL:        imageURL,
		UpdatedAt:       updatedAt,
		Type:            p.Tipo,
		IsParent:        p.Tipo == "V",
		GradeKeys:       p.Grade,
		Shipping:        shipping,
		WeightGramsHint: weightHint,
	}

	if p.ProdutoPai != nil && p.ProdutoPai.ID != 0 {
		prod.ParentExternalID = strconv.FormatInt(p.ProdutoPai.ID, 10)
	}

	if len(p.Variacoes) > 0 {
		variants := make([]ERPProduct, 0, len(p.Variacoes))
		for _, v := range p.Variacoes {
			vPrice := v.Precos.Preco
			if v.Precos.PrecoPromocional > 0 {
				vPrice = v.Precos.PrecoPromocional
			}
			attrs := decodeTinyGrade(v.GradeRaw, p.Grade)
			vShipping := variantToShipping(v)
			// Variants without their own dimensions inherit the parent's profile —
			// common for clothing where every size has the same weight/box.
			if vShipping == nil {
				vShipping = prod.Shipping
			}
			vWeightHint := variantWeightHintGrams(v)
			if vWeightHint == 0 {
				vWeightHint = weightHint // inherit hint from parent if variant has no own weight
			}
			variants = append(variants, ERPProduct{
				ID:               strconv.FormatInt(v.ID, 10),
				SKU:              v.SKU,
				GTIN:             v.GTIN,
				Name:             v.Descricao,
				Price:            int64(math.Round(vPrice * 100)),
				Stock:            int(v.Estoque.Quantidade),
				Active:           prod.Active, // Tiny variants inherit `situacao` from the parent.
				ParentExternalID: prod.ID,
				Attributes:       attrs,
				Shipping:         vShipping,
				WeightGramsHint:  vWeightHint,
			})
		}
		prod.Variants = variants
	}

	return prod
}

// dimensoesToShipping converts the parent/simple `dimensoes` block into our
// ERPShippingProfile. Validation rules differ by package format:
//
//   - Envelope (letter): height is meaningless (paper); merchants legitimately
//     leave altura=0 in the Tiny panel. We accept and substitute height with
//     the carrier minimum (1cm).
//   - Box / roll: requires all four (peso + altura + largura + comprimento)
//     to be positive — partial profiles are silently dropped because the
//     carrier won't quote a box without height.
//
// Returns nil when the profile is incomplete for the resolved format.
func dimensoesToShipping(d *tinyDimensoes) *ERPShippingProfile {
	if d == nil {
		return nil
	}
	// Use the larger of bruto/liquido. Bruto is supposed to include packaging,
	// so it should be >= liquido — but merchants regularly typo into the wrong
	// field, so picking max protects shipping quotes from a 25g vs 200g
	// mismatch breaking the carrier check.
	weightKg := d.PesoBruto
	if d.PesoLiquido > weightKg {
		weightKg = d.PesoLiquido
	}
	if weightKg <= 0 {
		return nil
	}

	format := mapTinyEmbalagem(d.Embalagem)

	// Envelope: only width and length are meaningful; altura defaults to 1cm.
	if format == "letter" {
		if d.Largura <= 0 || d.Comprimento <= 0 {
			return nil
		}
		return &ERPShippingProfile{
			WeightGrams:   int(math.Round(weightKg * 1000)),
			HeightCm:      1,
			WidthCm:       int(math.Round(d.Largura)),
			LengthCm:      int(math.Round(d.Comprimento)),
			PackageFormat: format,
		}
	}

	// Box / roll: all four required.
	if d.Altura <= 0 || d.Largura <= 0 || d.Comprimento <= 0 {
		return nil
	}
	return &ERPShippingProfile{
		WeightGrams:   int(math.Round(weightKg * 1000)),
		HeightCm:      int(math.Round(d.Altura)),
		WidthCm:       int(math.Round(d.Largura)),
		LengthCm:      int(math.Round(d.Comprimento)),
		PackageFormat: format,
	}
}

// variantToShipping converts the flat `peso/altura/largura/profundidade` Tiny
// returns inside variacoes[]. Same all-or-nothing contract as the parent.
func variantToShipping(v tinyVariantPayload) *ERPShippingProfile {
	return flatDimensionsToShipping(v.Peso, v.Altura, v.Largura, v.Profundidade)
}

// topLevelWeightHintGrams returns the weight (in grams) the Tiny payload carries
// for a parent/simple product, regardless of whether dimensions are present.
// Used so the integration service can combine it with store-level defaults.
//
// Picks max(pesoBruto, pesoLiquido) when both are present — see the comment in
// dimensoesToShipping for why we don't blindly trust bruto.
func topLevelWeightHintGrams(p tinyProductPayload) int {
	weightKg := 0.0
	if p.Dimensoes != nil {
		if p.Dimensoes.PesoBruto > weightKg {
			weightKg = p.Dimensoes.PesoBruto
		}
		if p.Dimensoes.PesoLiquido > weightKg {
			weightKg = p.Dimensoes.PesoLiquido
		}
	}
	if p.Peso > weightKg {
		weightKg = p.Peso
	}
	if weightKg <= 0 {
		return 0
	}
	return int(math.Round(weightKg * 1000))
}

// variantWeightHintGrams is the same as topLevelWeightHintGrams but for an
// inline variation entry (variacoes[i] of the parent payload).
func variantWeightHintGrams(v tinyVariantPayload) int {
	if v.Peso <= 0 {
		return 0
	}
	return int(math.Round(v.Peso * 1000))
}

// flatDimensionsToShipping is the shared kg+cm flat-field converter used both
// by inline variations (variacoes[]) and by individual GETs of variations
// (which return dimensions at the top level instead of inside `dimensoes`).
// Returns nil unless all four fields are positive — partial profiles are not
// useful and are rejected by the LiveCart domain validation.
func flatDimensionsToShipping(weightKg, heightCm, widthCm, lengthCm float64) *ERPShippingProfile {
	if weightKg <= 0 || heightCm <= 0 || widthCm <= 0 || lengthCm <= 0 {
		return nil
	}
	return &ERPShippingProfile{
		WeightGrams:   int(math.Round(weightKg * 1000)),
		HeightCm:      int(math.Round(heightCm)),
		WidthCm:       int(math.Round(widthCm)),
		LengthCm:      int(math.Round(lengthCm)),
		PackageFormat: "box",
	}
}

// mapTinyEmbalagem best-effort maps Tiny's package category to our
// box|roll|letter enum. Tiny may return `tipo` as a string OR a numeric id,
// so we try `nome` (human label) first, then string `tipo`, then numeric
// `tipo`, falling back to "box" when nothing matches.
//
// Numeric `tipo` values (observed empirically against the Tiny v3 panel —
// the swagger does not document the enum):
//
//	0 — Pacote (default box)
//	1 — Envelope                → letter
//	2 — Caixa                   → box
//	3 — Rolo / Cilindro / Tubo  → roll (assumed; revisit when we see one in the wild)
func mapTinyEmbalagem(e *tinyEmbalagem) string {
	if e == nil {
		return "box"
	}
	// Prefer the human label when present — it's stable across Tiny versions.
	if mapped := mapEmbalagemLabel(e.Nome); mapped != "" {
		return mapped
	}
	if len(e.Tipo) > 0 {
		// Try as string first (some Tiny endpoints return "envelope"/"caixa").
		var asString string
		if err := json.Unmarshal(e.Tipo, &asString); err == nil {
			if mapped := mapEmbalagemLabel(asString); mapped != "" {
				return mapped
			}
		}
		// Fall back to numeric id.
		var asNumber float64
		if err := json.Unmarshal(e.Tipo, &asNumber); err == nil {
			switch int(asNumber) {
			case 1:
				return "letter"
			case 3:
				return "roll"
			}
		}
	}
	return "box"
}

func mapEmbalagemLabel(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "envelope", "carta", "letter":
		return "letter"
	case "rolo", "cilindro", "tubo", "roll":
		return "roll"
	case "caixa", "pacote", "box":
		return "box"
	}
	return ""
}

// decodeTinyGrade accepts both `{"Cor":"Azul","Tamanho":"M"}` (object map, common in
// GET /produtos response) and `[{"chave":"Cor","valor":"Azul"}, ...]` (array form,
// used in the request model). gradeKeys is used to preserve order when the source
// is an object map.
func decodeTinyGrade(raw json.RawMessage, gradeKeys []string) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	// Try object form first.
	var asMap map[string]string
	if err := json.Unmarshal(raw, &asMap); err == nil && len(asMap) > 0 {
		_ = gradeKeys // order is preserved by the producer; map iteration order does not matter for our usage
		return asMap
	}
	// Fall back to array form.
	var asArray []struct {
		Chave string `json:"chave"`
		Valor string `json:"valor"`
	}
	if err := json.Unmarshal(raw, &asArray); err == nil && len(asArray) > 0 {
		out := make(map[string]string, len(asArray))
		for _, kv := range asArray {
			out[kv.Chave] = kv.Valor
		}
		return out
	}
	return nil
}

// SyncProduct updates a product in Tiny.
func (t *Tiny) SyncProduct(ctx context.Context, product ERPProduct) (*SyncResult, error) {
	endpoint := fmt.Sprintf("%s/produtos/%s", tinyAPIBaseURL, product.ID)

	payload := map[string]any{
		"codigo":    product.SKU,
		"nome":      product.Name,
		"descricao": product.Description,
		"preco":     float64(product.Price) / 100,
		"estoque":   product.Stock,
		"situacao":  boolToSituacao(product.Active),
	}

	resp, body, err := t.DoRequest(ctx, http.MethodPut, endpoint, payload, t.authHeaders())
	if err != nil {
		return &SyncResult{
			ProductID: product.ID,
			Action:    "failed",
			Success:   false,
			Error:     err.Error(),
		}, nil
	}

	if resp.StatusCode == http.StatusNoContent || providers.IsSuccessStatus(resp.StatusCode) {
		return &SyncResult{
			ProductID: product.ID,
			Action:    "updated",
			Success:   true,
		}, nil
	}

	var errResp struct {
		Mensagem string `json:"mensagem"`
	}
	_ = json.Unmarshal(body, &errResp)

	return &SyncResult{
		ProductID: product.ID,
		Action:    "failed",
		Success:   false,
		Error:     fmt.Sprintf("status %d: %s", resp.StatusCode, errResp.Mensagem),
	}, nil
}

// CreateOrder creates an order in Tiny for invoicing.
// Tiny API v3 requires `idContato` (integer) instead of inline customer data,
// and `data` (issue date, YYYY-MM-DD).
// If order.Payment is set, the parcela goes inside `pagamento.parcelas` and
// the order is approved separately via ApproveOrder so it shows up under
// "Pedidos de Venda" already settled.
// If order.ShippingAddress is set, it is shipped as `enderecoEntrega`.
// If order.Shipping is set, the cost goes to top-level `valorFrete` and a
// `transportador` block is sent; carrier/service/deadline (which we cannot
// translate to Tiny IDs locally) are stamped on `observacoesInternas` for
// the merchant.
func (t *Tiny) CreateOrder(ctx context.Context, order ERPOrder) (*OrderResult, error) {
	endpoint := tinyAPIBaseURL + "/pedidos"

	contactID, err := strconv.ParseInt(order.ContactID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid contact ID %q: %w", order.ContactID, err)
	}

	// Build order items
	items := make([]map[string]any, len(order.Items))
	for i, item := range order.Items {
		productID, _ := strconv.ParseInt(item.ProductID, 10, 64)
		items[i] = map[string]any{
			"produto": map[string]any{
				"id": productID,
			},
			"quantidade":    item.Quantity,
			"valorUnitario": float64(item.UnitPrice) / 100,
		}
	}

	payload := map[string]any{
		"idContato": contactID,
		// Tiny v3 requires the order issue date — sent in São Paulo local
		// time so the order lands on the merchant's "today" rather than UTC's
		// (otherwise late-night UTC orders fall a day ahead and disappear from
		// the merchant's "últimos 30 dias" filter).
		"data":        time.Now().In(tinyLocation).Format("2006-01-02"),
		"itens":       items,
		"observacoes": order.Observation,
		"ecommerce": map[string]any{
			"numeroPedidoEcommerce": order.ExternalID,
		},
		// numeroPedidoEcommerce é IGNORADO em contas sem canal e-commerce
		// cadastrado (validado em sandbox 11/07: o GET devolve o campo vazio).
		// numeroOrdemCompra é gravado sempre — é o vínculo pedido↔cart que
		// sobrevive no Tiny e o insumo da reconciliação por varredura.
		"numeroOrdemCompra": "lc-cart-" + order.ExternalID,
	}

	if addr := order.ShippingAddress; addr != nil {
		payload["enderecoEntrega"] = map[string]any{
			"endereco":         addr.Street,
			"enderecoNro":      addr.Number,
			"complemento":      addr.Complement,
			"bairro":           addr.Neighborhood,
			"municipio":        addr.City,
			"cep":              addr.ZipCode,
			"uf":               addr.State,
			"fone":             addr.Phone,
			"nomeDestinatario": addr.RecipientName,
			"cpfCnpj":          addr.Document,
			"tipoPessoa":       "F",
		}
	}

	// Frete (envio): Tiny v3 expects the cost as the top-level `valorFrete`
	// and a `transportador` block. When the merchant cadastrou a forma de
	// envio matching the carrier (e.g. "SmartEnvios", "Correios", "Jadlog")
	// we resolve its id via /formas-envio and link it under
	// `transportador.formaEnvio.id` — that's how the order shows up under
	// the right separação/etiqueta queue in Tiny.
	if ship := order.Shipping; ship != nil {
		payload["valorFrete"] = float64(ship.CostCents) / 100

		// "D" = Destinatário paga o frete (modelo padrão LiveCart). If the
		// store ever runs a free-shipping promo where the merchant absorbs
		// the cost we may want to flip this to "R" (remetente).
		transportador := map[string]any{
			"fretePorConta": "D",
		}

		// Retirada na loja não é remessa: não existe transportadora a
		// resolver, e o fallback abaixo pegaria o agregador de frete e
		// carimbaria uma entrega que ninguém vai despachar. Foi assim que um
		// pedido de retirada saiu com "SmartEnvios" e o Tiny recusou o pedido
		// inteiro com "Forma de envio não habilitada".
		var (
			formaEnvioID   int64
			formaEnvioErr  error
			formaEnvioName = ship.Carrier
		)
		if !isStorePickup(ship.Carrier) {
			// Try to resolve the formaEnvio id, preferring the carrier name
			// (Correios / Jadlog / etc.) and falling back to "SmartEnvios" so
			// stores that cadastrou só o agregador também batem.
			formaEnvioID, formaEnvioErr = t.lookupFormaEnvioID(ctx, ship.Carrier)
			if formaEnvioErr == nil && formaEnvioID == 0 && ship.Carrier != "SmartEnvios" {
				id, err := t.lookupFormaEnvioID(ctx, "SmartEnvios")
				if err == nil && id > 0 {
					formaEnvioID = id
					formaEnvioName = "SmartEnvios"
				}
			}
		}
		// UM lugar decide a mensagem. Quando a retirada só pulava a consulta lá
		// em cima, ela desembocava no `default` e saía como WARN dizendo que a
		// busca não achou nada — afirmando uma consulta que nunca houve, num
		// caso perfeitamente normal. Todo pedido de retirada gerava esse aviso
		// falso, e quem fosse investigar procuraria um problema de transportadora
		// que não existe.
		switch {
		case isStorePickup(ship.Carrier):
			logger.From(ctx, t.Logger).Info("tiny: retirada na loja, pedido segue sem forma de envio",
				zap.String("carrier", ship.Carrier),
			)
		case formaEnvioErr != nil:
			logger.From(ctx, t.Logger).Warn("tiny formaEnvio lookup failed, sending order without it",
				zap.String("carrier", ship.Carrier),
				zap.Error(formaEnvioErr),
			)
		case formaEnvioID > 0:
			transportador["formaEnvio"] = map[string]any{"id": formaEnvioID}
			logger.From(ctx, t.Logger).Info("tiny formaEnvio matched",
				zap.String("carrier", ship.Carrier),
				zap.String("matched_name", formaEnvioName),
				zap.Int64("forma_envio_id", formaEnvioID),
			)
		default:
			logger.From(ctx, t.Logger).Warn("tiny formaEnvio lookup returned no match, order will use Tiny default",
				zap.String("carrier", ship.Carrier),
			)
		}

		payload["transportador"] = transportador

		// Carrier / serviço / prazo / cost fall into observacoesInternas
		// for merchant visibility — the formaEnvio id alone tells Tiny
		// "this order is a SmartEnvios shipment" but not which service.
		var notes []string
		if ship.Carrier != "" {
			notes = append(notes, "Transportadora: "+ship.Carrier)
		}
		if ship.Service != "" {
			notes = append(notes, "Serviço: "+ship.Service)
		}
		if ship.DeadlineDays > 0 {
			notes = append(notes, fmt.Sprintf("Prazo: %d dia(s)", ship.DeadlineDays))
		}
		if ship.CostCents > 0 {
			notes = append(notes, fmt.Sprintf("Custo real: R$ %.2f", float64(ship.CostCents)/100))
		}
		if len(notes) > 0 {
			payload["observacoesInternas"] = strings.Join(notes, " | ")
		}
	}

	// Pagamento: Tiny v3's ParcelaModelRequest declares BOTH `meioPagamento`
	// (the instrument — Cartão/Pix/Boleto) AND `formaRecebimento` (the AR
	// flow type — "Cartão de Crédito", "À Vista", etc.) as required. Lookups
	// hit two distinct cadastros in Tiny: /formas-pagamento (under Cadastros)
	// and /formas-recebimento (under Financeiro → Cadastros). When either is
	// missing or doesn't match by name, Tiny silently falls back to the
	// generic "Conta a Receber" categorization on the parcela's display.
	//
	// For credit-card sales we expand into one parcela per installment with
	// vencimentos D+30, D+60, ... so contas a receber reflects what Mercado
	// Pago will repass per cycle. Pix / debit / boleto stay as a single
	// parcela on the payment date (already settled or settles next-day).
	if pay := order.Payment; pay != nil {
		// meioPagamento is intentionally NOT looked up / sent on the parcela.
		// /formas-pagamento returns the merchant's local cadastro IDs, but the
		// `meioPagamento.id` field on a parcela is validated against a
		// different namespace inside Tiny — sending the cadastro ID there
		// fails strict validation with:
		//   pagamento.parcelas[0].meioPagamento.id: Meio de pagamento não encontrado
		// Omitting the field lets Tiny apply its default and the order goes
		// through; formaRecebimento alone is enough to keep the parcela out
		// of the generic "Conta a Receber" bucket. If/when we map our
		// payment methods to Tiny's system enum we can re-enable this.
		var meioRef map[string]any

		// formaRecebimento is the OTHER half of the parcela. /formas-recebimento
		// doesn't expose a name filter (only limit/offset), so we paginate
		// once with limit=100 — which covers any realistic merchant — and
		// match in memory.
		var recebRef map[string]any
		recebID, recebErr := t.lookupFormaRecebimentoID(ctx, pay.Method)
		switch {
		case recebErr != nil:
			logger.From(ctx, t.Logger).Warn("tiny formaRecebimento lookup failed, sending parcelas without it",
				zap.String("method", pay.Method),
				zap.Error(recebErr),
			)
		case recebID > 0:
			recebRef = map[string]any{"id": recebID}
		default:
			logger.From(ctx, t.Logger).Warn("tiny formaRecebimento lookup returned no match, parcela will fall back to default",
				zap.String("method", pay.Method),
			)
		}

		parcelas := buildTinyParcelas(pay, meioRef, recebRef)
		pagamento := map[string]any{
			"parcelas": parcelas,
		}
		// meioPagamento and formaRecebimento live INSIDE each parcela
		// (see ParcelaModelRequest in the Tiny v3 swagger). An earlier
		// version mirrored them at the pagamento parent level "as a
		// default" — Tiny started strict-validating that field and
		// rejecting orders with `pagamento.meioPagamento.id: Meio de
		// pagamento não encontrado`. The parcela-level refs alone are
		// what the API actually expects.
		payload["pagamento"] = pagamento

		// Snapshot of the parcela schedule for log audit. With this we can
		// reconcile the merchant's contas a receber against what we sent
		// (Tiny may rewrite values but not the count / dates). Each parcela
		// is logged individually (date + valor) so a future Phase 2
		// (subtract MP fee from parcela.valor or attach a desconto) can be
		// validated against the same baseline.
		firstDue, _ := parcelas[0]["data"].(string)
		lastDue, _ := parcelas[len(parcelas)-1]["data"].(string)
		schedule := make([]map[string]any, 0, len(parcelas))
		for _, p := range parcelas {
			schedule = append(schedule, map[string]any{
				"data":  p["data"],
				"valor": p["valor"],
				"dias":  p["dias"],
			})
		}
		logger.From(ctx, t.Logger).Info("tiny order parcelas prepared",
			zap.String("method", pay.Method),
			zap.Int("installments", pay.Installments),
			zap.Int("parcelas_count", len(parcelas)),
			zap.String("first_due", firstDue),
			zap.String("last_due", lastDue),
			zap.Int64("amount_cents", pay.Amount),
			zap.Int64("fee_amount_cents", pay.FeeAmountCents),
			zap.Int64("net_amount_cents", pay.NetAmountCents),
			zap.Bool("had_money_release_date", pay.MoneyReleaseDate != nil),
			zap.Bool("meio_pagamento_matched", meioRef != nil),
			zap.Bool("forma_recebimento_matched", recebRef != nil),
			zap.Any("schedule", schedule),
		)
	}

	feeCents := int64(0)
	netCents := int64(0)
	paymentMethod := ""
	if order.Payment != nil {
		feeCents = order.Payment.FeeAmountCents
		netCents = order.Payment.NetAmountCents
		paymentMethod = order.Payment.Method
	}
	logger.From(ctx, t.Logger).Info("tiny CreateOrder sending payload",
		zap.String("external_id", order.ExternalID),
		zap.String("contact_id", order.ContactID),
		zap.Int("items_count", len(order.Items)),
		zap.Int64("total_amount_cents", order.TotalAmount),
		zap.Bool("has_shipping_address", order.ShippingAddress != nil),
		zap.Bool("has_shipping_method", order.Shipping != nil),
		zap.Bool("has_payment", order.Payment != nil),
		zap.String("payment_method", paymentMethod),
		zap.Int64("fee_amount_cents", feeCents),
		zap.Int64("net_amount_cents", netCents),
	)

	resp, body, err := t.DoRequest(ctx, http.MethodPost, endpoint, payload, t.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("creating order: %w", err)
	}

	// Uma forma de envio recusada não pode custar o pedido.
	//
	// O vínculo com a transportadora depende de cadastro do lojista dentro do
	// Tiny, e a listagem de /formas-envio devolve só id, nome e tipo — não há
	// campo dizendo se ela está habilitada. Não dá para escolher certo na
	// consulta; só o POST revela. Quando o Tiny recusa por causa desse campo,
	// reenviamos sem ele: o pedido é registrado e o lojista corrige a forma de
	// envio no Tiny. O contrário é o que aconteceu em produção — pedido pago,
	// carrinho concluído e nada no ERP, que é o pior dos dois mundos.
	// 409 significa que o pedido JÁ EXISTE — a tentativa anterior chegou ao
	// Tiny e só a resposta se perdeu. Repetir o POST nunca vai passar, então
	// tratar como falha custava três retentativas e uma dead letter para um
	// pedido que estava lá o tempo todo.
	//
	// O marcador gravado na criação é o que permite reencontrá-lo. Achando,
	// devolvemos sucesso: o chamador grava o external_order_id e a máquina de
	// estados retomável assume dali (o relançamento de estoque já tolera
	// "Estoque já lançado").
	if resp.StatusCode == http.StatusConflict {
		if adopted, err := t.adoptExistingOrder(ctx, order); err == nil && adopted != nil {
			return adopted, nil
		}
	}

	if !providers.IsSuccessStatus(resp.StatusCode) && isFormaEnvioRejection(body) && dropFormaEnvio(payload) {
		logger.From(ctx, t.Logger).Warn("tiny recusou a forma de envio; reenviando o pedido sem ela",
			zap.String("external_id", order.ExternalID),
			zap.Int("status", resp.StatusCode),
			zap.String("detail", tinyErrorDetail(body)),
		)
		resp, body, err = t.DoRequest(ctx, http.MethodPost, endpoint, payload, t.authHeaders())
		if err != nil {
			return nil, fmt.Errorf("creating order without forma de envio: %w", err)
		}
		if providers.IsSuccessStatus(resp.StatusCode) {
			logger.From(ctx, t.Logger).Warn("pedido registrado no Tiny sem forma de envio; ajustar no ERP",
				zap.String("external_id", order.ExternalID),
			)
		}
	}

	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("create order failed: status %d: %s", resp.StatusCode, tinyErrorDetail(body))
	}

	// `situacao` is Tiny's order status code (e.g. "aberto", "aprovado").
	// Capturing it on creation lets us spot when Tiny rejected fields
	// silently (status comes back inconsistent vs what we sent).
	var orderResp struct {
		ID       int64  `json:"id"`
		Numero   string `json:"numeroPedido"`
		Situacao string `json:"situacao"`
	}

	if err := json.Unmarshal(body, &orderResp); err != nil {
		return nil, fmt.Errorf("parsing order response: %w", err)
	}

	orderID := strconv.FormatInt(orderResp.ID, 10)
	logger.From(ctx, t.Logger).Info("tiny order created",
		zap.String("order_id", orderID),
		zap.String("numero_pedido", orderResp.Numero),
		zap.String("situacao", orderResp.Situacao),
		zap.String("external_id", order.ExternalID),
		zap.Int64("total_amount_cents", order.TotalAmount),
		zap.Int64("fee_amount_cents", feeCents),
		zap.Int64("net_amount_cents", netCents),
	)

	// Marca o pedido com o vínculo do carrinho.
	//
	// É o que permite reencontrá-lo quando a resposta do POST se perde: o
	// `numeroOrdemCompra` viaja no corpo mas não é filtro de busca, e
	// `marcadores` é. Sem este carimbo, um timeout entre o POST e a resposta
	// deixa o pedido existindo no Tiny sem nenhuma forma de achá-lo pela API —
	// foi o que aconteceu com 2 pedidos pagos em 16/08.
	//
	// Best-effort, como a aprovação: falhar aqui não invalida o pedido.
	marker := tinyCartMarker(order.ExternalID)
	if markErr := t.AddOrderMarker(ctx, orderID, marker); markErr != nil {
		logger.From(ctx, t.Logger).Warn("failed to tag tiny order with cart marker",
			zap.String("order_id", orderID),
			zap.String("marker", marker),
			zap.Error(markErr),
		)
	}

	// Aprova quando a VENDA está fechada — não quando há bloco financeiro.
	// Ver ERPOrder.Approve: pagamento por fora aprova sem lançar recebimento.
	// Falha aqui não é fatal — o pedido existe no Tiny de qualquer forma.
	if order.Approve {
		if approveErr := t.ApproveOrder(ctx, orderID); approveErr != nil {
			logger.From(ctx, t.Logger).Warn("failed to approve tiny order after creation",
				zap.String("order_id", orderID),
				zap.Error(approveErr),
			)
		}
	}

	return &OrderResult{
		OrderID:     orderID,
		OrderNumber: orderResp.Numero,
		Status:      "created",
	}, nil
}

// buildTinyParcelas turns the captured payment into the parcela list Tiny
// expects under `pagamento.parcelas`. Credit-card sales are split into one
// parcela per installment so contas a receber tracks each repasse — Pix,
// débito and boleto produce a single parcela dated on the payment date
// (already cleared or clears D+1).
//
// First-parcela date: when the gateway told us when it will release the
// money (pay.MoneyReleaseDate, surfaced by Mercado Pago) we honour that —
// it already encodes whether the merchant has antecipation enabled (D+1)
// or runs on default (D+30). Otherwise we fall back to PaidAt for non-card
// (instant clears) and PaidAt+30 for credit card. Subsequent installments
// stagger by 30 days from the first.
//
// Cents-to-reais split absorbs the rounding remainder on the LAST parcela
// so the parcelas always sum back to pay.Amount exactly.
func buildTinyParcelas(pay *providers.ERPOrderPayment, meioRef, recebRef map[string]any) []map[string]any {
	count := pay.Installments
	if pay.Method != "credit_card" || count < 1 {
		count = 1
	}

	// Resolve the first-parcela due date.
	var firstDue time.Time
	if pay.MoneyReleaseDate != nil {
		firstDue = *pay.MoneyReleaseDate
	} else if pay.Method == "credit_card" {
		firstDue = pay.PaidAt.AddDate(0, 0, 30)
	} else {
		firstDue = pay.PaidAt
	}
	// `dias` is days from the order issue date — equivalent to days from
	// PaidAt for our purposes since we set `data` (top-level) to today
	// (São Paulo). Negative offsets shouldn't happen in practice; clamp to 0.
	firstDays := int(firstDue.Sub(pay.PaidAt).Hours() / 24)
	if firstDays < 0 {
		firstDays = 0
	}

	applyRefs := func(p map[string]any) {
		if meioRef != nil {
			p["meioPagamento"] = meioRef
		}
		if recebRef != nil {
			p["formaRecebimento"] = recebRef
		}
	}

	if count == 1 {
		parcela := map[string]any{
			"dias":        firstDays,
			"data":        firstDue.In(tinyLocation).Format("2006-01-02"),
			"valor":       float64(pay.Amount) / 100,
			"observacoes": fmt.Sprintf("Pago via %s - ID %s", pay.Method, pay.PaymentID),
		}
		applyRefs(parcela)
		return []map[string]any{parcela}
	}

	parcelas := make([]map[string]any, count)
	perCents := pay.Amount / int64(count)
	remainder := pay.Amount - perCents*int64(count)

	for i := 0; i < count; i++ {
		cents := perCents
		if i == count-1 {
			// Absorb the rounding remainder on the last parcela so the
			// total matches pay.Amount to the cent.
			cents += remainder
		}
		days := firstDays + 30*i
		dueDate := firstDue.AddDate(0, 0, 30*i).In(tinyLocation).Format("2006-01-02")
		parcela := map[string]any{
			"dias":        days,
			"data":        dueDate,
			"valor":       float64(cents) / 100,
			"observacoes": fmt.Sprintf("Parcela %d/%d via %s - ID %s", i+1, count, pay.Method, pay.PaymentID),
		}
		applyRefs(parcela)
		parcelas[i] = parcela
	}
	return parcelas
}

// lookupFormaPagamentoID resolves our payment method string (pix/credit_card/...)
// to the Tiny formaPagamento ID by matching names (best-effort, no cache).
// Returns 0 without error if no match is found.
func (t *Tiny) lookupFormaPagamentoID(ctx context.Context, method string) (int64, error) {
	var queryName string
	switch method {
	case "pix":
		queryName = "Pix"
	case "credit_card":
		queryName = "Cartão de Crédito"
	case "debit_card":
		queryName = "Cartão de Débito"
	case "boleto":
		queryName = "Boleto"
	default:
		return 0, nil
	}

	endpoint := fmt.Sprintf("%s/formas-pagamento?nome=%s&situacao=1&limit=10",
		tinyAPIBaseURL, url.QueryEscape(queryName))

	// Retry on 5xx/429: under HealthCheck the 7 lookups burst together and
	// occasionally one gets rate-limited or hits a transient server error,
	// flipping the displayed count between refreshes.
	resp, body, err := t.DoRequestWithRetry(ctx, 2, http.MethodGet, endpoint, nil, t.authHeaders())
	if err != nil {
		return 0, fmt.Errorf("listing formas de pagamento: %w", err)
	}
	if resp.StatusCode == http.StatusNoContent {
		return 0, nil
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return 0, fmt.Errorf("list formas de pagamento failed: status %d", resp.StatusCode)
	}

	var result struct {
		Itens []struct {
			ID   int64  `json:"id"`
			Nome string `json:"nome"`
		} `json:"itens"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, fmt.Errorf("parsing formas de pagamento: %w", err)
	}

	// Prefer an exact (case-insensitive) name match; fall back to the first result.
	for _, item := range result.Itens {
		if strings.EqualFold(item.Nome, queryName) {
			return item.ID, nil
		}
	}
	// Sem o "pega o primeiro da lista" que existia aqui. O parâmetro `nome`
	// não está na doc da v3 (a listagem documenta só limit/offset), então
	// quando o Tiny o ignora a lista volta inteira e o primeiro item é uma
	// forma de envio arbitrária — não a que o lojista usa. Vincular a
	// transportadora errada é pior que não vincular nenhuma: sem o campo o
	// Tiny aplica o padrão dele, com o campo errado o pedido é recusado.
	return 0, nil
}

// expectedFormaRecebimentoName returns the canonical name we expect the
// merchant to have cadastrado in Tiny → Configurações → Finanças → Formas
// de Recebimento (Tiny seeds these by default; merchants only need to
// confirm they're enabled). Mirrors the names lookupFormaPagamentoID
// searches for, so a single name carries both halves of the parcela.
func expectedFormaRecebimentoName(method string) string {
	switch method {
	case "pix":
		return "Pix"
	case "credit_card":
		return "Cartão de Crédito"
	case "debit_card":
		return "Cartão de Débito"
	case "boleto":
		return "Boleto"
	}
	return ""
}

// formaRecebimentoAliases lists the variants Tiny actually returns for
// each method. The default seed names changed across versions ("Cartão
// Crédito" without "de", lowercase "cartão de crédito", "PIX" all caps),
// and merchants sometimes rename — our lookup matches if the cadastro
// name equals or contains any alias (case + accent insensitive).
func formaRecebimentoAliases(method string) []string {
	switch method {
	case "pix":
		return []string{"pix"}
	case "credit_card":
		return []string{"cartao de credito", "cartao credito", "credito"}
	case "debit_card":
		return []string{"cartao de debito", "cartao debito", "debito"}
	case "boleto":
		return []string{"boleto"}
	}
	return nil
}

// stripAccents folds a Latin-1 string to ASCII for fuzzy name matching.
// Tiny's seed cadastros use accented forms ("Crédito"); merchants who
// rename sometimes drop the accent ("Credito"), so equality must compare
// the normalized forms or we report missing on a present cadastro.
func stripAccents(s string) string {
	r := []rune(strings.ToLower(s))
	for i, c := range r {
		switch c {
		case 'á', 'à', 'â', 'ã', 'ä':
			r[i] = 'a'
		case 'é', 'è', 'ê', 'ë':
			r[i] = 'e'
		case 'í', 'ì', 'î', 'ï':
			r[i] = 'i'
		case 'ó', 'ò', 'ô', 'õ', 'ö':
			r[i] = 'o'
		case 'ú', 'ù', 'û', 'ü':
			r[i] = 'u'
		case 'ç':
			r[i] = 'c'
		case 'ñ':
			r[i] = 'n'
		}
	}
	return string(r)
}

// matchesFormaRecebimento returns true when the cadastro's `nome` is the
// same method as `method`, comparing accent/case-folded forms with
// equality first and substring match as fallback. The substring match is
// only ever applied to the alias list (not free text) so a "Cartão de
// Crédito - 12x" merchant variant is still picked up but unrelated names
// like "Recebimento Manual" never match by accident.
func matchesFormaRecebimento(method, nome string) bool {
	normalized := stripAccents(nome)
	for _, alias := range formaRecebimentoAliases(method) {
		if normalized == alias || strings.Contains(normalized, alias) {
			return true
		}
	}
	return false
}

// lookupFormaRecebimentoID resolves the AR-flow side of the parcela
// (formaRecebimento on Tiny v3 ParcelaModelRequest) by paginating
// /formas-recebimento and matching by name in memory — the endpoint
// doesn't accept a `nome` filter, only limit/offset (default 100). For
// any realistic merchant the first page covers everything; we cap at
// 5 pages (500 items) before giving up to avoid pathological loops.
//
// Returns 0 without error when no match is found, so the caller can
// log "no match" and fall back without aborting the order creation.
func (t *Tiny) lookupFormaRecebimentoID(ctx context.Context, method string) (int64, error) {
	target := expectedFormaRecebimentoName(method)
	if target == "" {
		return 0, nil
	}

	const pageSize = 100
	const maxPages = 5
	for page := 0; page < maxPages; page++ {
		endpoint := fmt.Sprintf("%s/formas-recebimento?limit=%d&offset=%d",
			tinyAPIBaseURL, pageSize, page*pageSize)

		resp, body, err := t.DoRequestWithRetry(ctx, 2, http.MethodGet, endpoint, nil, t.authHeaders())
		if err != nil {
			return 0, fmt.Errorf("listing formas de recebimento: %w", err)
		}
		if resp.StatusCode == http.StatusNoContent {
			return 0, nil
		}
		if !providers.IsSuccessStatus(resp.StatusCode) {
			return 0, fmt.Errorf("list formas de recebimento failed: status %d", resp.StatusCode)
		}

		var result struct {
			Itens []struct {
				ID       int64  `json:"id"`
				Nome     string `json:"nome"`
				Ativo    bool   `json:"ativo"`
				Padrao   bool   `json:"padrao"`
				Excluido bool   `json:"excluido"`
			} `json:"itens"`
			Paginacao struct {
				TotalRegistros int `json:"totalRegistros"`
			} `json:"paginacao"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return 0, fmt.Errorf("parsing formas de recebimento: %w", err)
		}

		// Snapshot of what came back so we can debug "cadastro existe e
		// não bate" reports — Tiny seeds each account with default
		// formas-recebimento that older code expected to find by exact
		// name, but the seeds have changed across versions and some
		// accounts have customized labels.
		snapshot := make([]map[string]any, 0, len(result.Itens))
		for _, item := range result.Itens {
			snapshot = append(snapshot, map[string]any{
				"id":       item.ID,
				"nome":     item.Nome,
				"ativo":    item.Ativo,
				"padrao":   item.Padrao,
				"excluido": item.Excluido,
			})
		}
		logger.From(ctx, t.Logger).Debug("tiny formas-recebimento page fetched",
			zap.String("target_method", method),
			zap.Int("page", page),
			zap.Int("count", len(result.Itens)),
			zap.Int("total_registros", result.Paginacao.TotalRegistros),
			zap.Any("itens", snapshot),
		)

		for _, item := range result.Itens {
			if item.Excluido {
				continue
			}
			// We tolerate ativo=false here. Tiny's UI can show a
			// cadastro toggled off but with active=false and the
			// merchant who confirms "está cadastrado" will be confused
			// by us reporting missing; the order-create endpoint will
			// reject the parcela later if ativo is genuinely off, so
			// the worst case is a deferred error.
			if matchesFormaRecebimento(method, item.Nome) {
				return item.ID, nil
			}
		}

		// Last page reached.
		if len(result.Itens) < pageSize {
			return 0, nil
		}
	}
	return 0, nil
}

// lookupFormaEnvioID resolves a shipping name (e.g. "SmartEnvios", "Correios",
// "Jadlog") to the Tiny formaEnvio ID. Tiny v3 lists shipping methods at
// GET /formas-envio?nome=... — the merchant cadastra cada integração de
// frete (SmartEnvios, Melhor Envio, Correios direto, etc.) lá e cada uma
// recebe um id que o pedido referencia em transportador.formaEnvio.id.
//
// Returns 0 without error if no match. Best-effort, no cache.
func (t *Tiny) lookupFormaEnvioID(ctx context.Context, name string) (int64, error) {
	if name == "" {
		return 0, nil
	}
	endpoint := fmt.Sprintf("%s/formas-envio?nome=%s&limit=10",
		tinyAPIBaseURL, url.QueryEscape(name))

	resp, body, err := t.DoRequestWithRetry(ctx, 2, http.MethodGet, endpoint, nil, t.authHeaders())
	if err != nil {
		return 0, fmt.Errorf("listing formas de envio: %w", err)
	}
	if resp.StatusCode == http.StatusNoContent {
		return 0, nil
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return 0, fmt.Errorf("list formas de envio failed: status %d", resp.StatusCode)
	}

	var result struct {
		Itens []struct {
			ID   int64  `json:"id"`
			Nome string `json:"nome"`
		} `json:"itens"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, fmt.Errorf("parsing formas de envio: %w", err)
	}

	// Prefer exact (case-insensitive) match, then any case-insensitive
	// substring containing the query so "SmartEnvios" still matches a
	// merchant who cadastrou "SmartEnvios - PAC".
	lowerQuery := strings.ToLower(name)
	for _, item := range result.Itens {
		if strings.EqualFold(item.Nome, name) {
			return item.ID, nil
		}
	}
	for _, item := range result.Itens {
		if strings.Contains(strings.ToLower(item.Nome), lowerQuery) {
			return item.ID, nil
		}
	}
	if len(result.Itens) > 0 {
		return result.Itens[0].ID, nil
	}
	return 0, nil
}

// isFormaEnvioRejection reports whether the Tiny rejection is about the
// shipping method — and only about it. Retrying without the field only helps
// when that field is the problem; a rejection over items or contact would come
// back identical and we would have burned a second POST for nothing.
func isFormaEnvioRejection(body []byte) bool {
	return strings.Contains(strings.ToLower(tinyErrorDetail(body)), "formaenvio")
}

// dropFormaEnvio removes the shipping-method link from the payload, reporting
// whether there was one to remove. The `transportador` block itself stays: it
// still carries fretePorConta, and o valorFrete continua no pedido.
func dropFormaEnvio(payload map[string]any) bool {
	transportador, ok := payload["transportador"].(map[string]any)
	if !ok {
		return false
	}
	if _, ok := transportador["formaEnvio"]; !ok {
		return false
	}
	delete(transportador, "formaEnvio")

	// O lojista precisa saber por que o pedido entrou sem transportadora
	// vinculada; sem isso o pedido chega no Tiny silenciosamente diferente do
	// que foi vendido.
	const aviso = "Forma de envio não vinculada: recusada pelo Tiny (verificar cadastro em Cadastros → Formas de envio)"
	if atual, _ := payload["observacoesInternas"].(string); atual != "" {
		payload["observacoesInternas"] = atual + " | " + aviso
	} else {
		payload["observacoesInternas"] = aviso
	}
	return true
}

// isStorePickup reports whether the chosen "shipping" is actually the buyer
// picking the order up at the store.
func isStorePickup(carrier string) bool {
	return strings.EqualFold(strings.TrimSpace(carrier), providers.StorePickupCarrier)
}

// LaunchOrderStock decrements stock in Tiny for all items in the order.
// POST /pedidos/{idPedido}/lancar-estoque
// adoptExistingOrder reencontra, pelo marcador, o pedido que uma tentativa
// anterior já criou, e o devolve como sucesso.
//
// Também termina o que ficou pela metade: a criação só aprova DEPOIS do POST
// voltar, então um pedido criado por uma tentativa que morreu no caminho está
// no Tiny como "Em aberto". Aprovar aqui é o passo que faltava — e aprovar de
// novo um pedido já aprovado é inócuo.
//
// Devolve (nil, nil) quando o marcador não acha nada: é o caso dos pedidos
// criados ANTES de o carimbo existir, e aí o 409 segue sendo falha.
func (t *Tiny) adoptExistingOrder(ctx context.Context, order ERPOrder) (*OrderResult, error) {
	marker := tinyCartMarker(order.ExternalID)
	orderID, err := t.FindOrderIDByMarker(ctx, marker)
	if err != nil {
		return nil, fmt.Errorf("looking up existing order by marker %s: %w", marker, err)
	}
	if orderID == "" {
		return nil, nil
	}

	logger.From(ctx, t.Logger).Warn("tiny devolveu 409; adotando o pedido que já existe",
		zap.String("order_id", orderID),
		zap.String("marker", marker),
		zap.String("external_id", order.ExternalID),
	)

	if order.Approve {
		if approveErr := t.ApproveOrder(ctx, orderID); approveErr != nil {
			logger.From(ctx, t.Logger).Warn("failed to approve adopted tiny order",
				zap.String("order_id", orderID),
				zap.Error(approveErr),
			)
		}
	}

	return &OrderResult{OrderID: orderID, Status: "adopted"}, nil
}

// tinyCartMarker é o vínculo pedido↔carrinho, no mesmo formato que a varredura
// de reconciliação procura (erpOrderMarker). O `numeroOrdemCompra` carrega o
// mesmo valor no corpo do pedido, mas só `marcadores` é filtro de busca na API.
func tinyCartMarker(cartID string) string { return "lc-cart-" + cartID }

func (t *Tiny) LaunchOrderStock(ctx context.Context, orderID string) error {
	endpoint := fmt.Sprintf("%s/pedidos/%s/lancar-estoque", tinyAPIBaseURL, orderID)

	resp, body, err := t.DoRequest(ctx, http.MethodPost, endpoint, nil, t.authHeaders())
	if err != nil {
		return fmt.Errorf("launching order stock: %w", err)
	}

	if resp.StatusCode != http.StatusNoContent && !providers.IsSuccessStatus(resp.StatusCode) {
		var errResp struct {
			Mensagem string `json:"mensagem"`
		}
		_ = json.Unmarshal(body, &errResp)

		// "Estoque já lançado" means Tiny auto-launched stock on order creation — treat as success
		if strings.Contains(errResp.Mensagem, "já lançado") {
			logger.From(ctx, t.Logger).Info("stock already launched by Tiny automatically",
				zap.String("order_id", orderID),
			)
			return nil
		}

		return fmt.Errorf("launch stock failed: status %d, message: %s", resp.StatusCode, errResp.Mensagem)
	}

	logger.From(ctx, t.Logger).Info("tiny order stock launched",
		zap.String("order_id", orderID),
	)
	return nil
}

// ReverseOrderStock returns stock in Tiny for all items in the order.
// POST /pedidos/{idPedido}/estornar-estoque
func (t *Tiny) ReverseOrderStock(ctx context.Context, orderID string) error {
	endpoint := fmt.Sprintf("%s/pedidos/%s/estornar-estoque", tinyAPIBaseURL, orderID)

	resp, body, err := t.DoRequest(ctx, http.MethodPost, endpoint, nil, t.authHeaders())
	if err != nil {
		return fmt.Errorf("reversing order stock: %w", err)
	}

	if resp.StatusCode != http.StatusNoContent && !providers.IsSuccessStatus(resp.StatusCode) {
		var errResp struct {
			Mensagem string `json:"mensagem"`
		}
		_ = json.Unmarshal(body, &errResp)
		return fmt.Errorf("reverse stock failed: status %d, message: %s", resp.StatusCode, errResp.Mensagem)
	}

	return nil
}

// ApproveOrder sets the order status to "Aprovado" (3) in Tiny.
// This makes the order visible under "Pedidos de Venda" in the Tiny dashboard.
func (t *Tiny) ApproveOrder(ctx context.Context, orderID string) error {
	endpoint := fmt.Sprintf("%s/pedidos/%s/situacao", tinyAPIBaseURL, orderID)
	payload := map[string]any{
		"situacao": 3, // Aprovado
	}

	resp, body, err := t.DoRequest(ctx, http.MethodPut, endpoint, payload, t.authHeaders())
	if err != nil {
		return fmt.Errorf("approving order: %w", err)
	}

	if resp.StatusCode != http.StatusNoContent && !providers.IsSuccessStatus(resp.StatusCode) {
		var errResp struct {
			Mensagem string `json:"mensagem"`
		}
		_ = json.Unmarshal(body, &errResp)
		return fmt.Errorf("approve order failed: status %d, message: %s", resp.StatusCode, errResp.Mensagem)
	}

	logger.From(ctx, t.Logger).Info("tiny order approved",
		zap.String("order_id", orderID),
	)
	return nil
}

// CancelOrder reverses stock and cancels an order in Tiny.
// Steps: estornar-estoque → situacao=2 (Cancelada)
func (t *Tiny) CancelOrder(ctx context.Context, orderID string) error {
	// First reverse stock
	if err := t.ReverseOrderStock(ctx, orderID); err != nil {
		// Log but continue — order might not have stock launched yet
		logger.From(ctx, t.Logger).Warn("failed to reverse stock before cancel, continuing",
			zap.String("order_id", orderID),
			zap.Error(err),
		)
	}

	// Then cancel the order
	endpoint := fmt.Sprintf("%s/pedidos/%s/situacao", tinyAPIBaseURL, orderID)
	payload := map[string]any{
		"situacao": 2, // Cancelada
	}

	resp, body, err := t.DoRequest(ctx, http.MethodPut, endpoint, payload, t.authHeaders())
	if err != nil {
		return fmt.Errorf("cancelling order: %w", err)
	}

	if resp.StatusCode != http.StatusNoContent && !providers.IsSuccessStatus(resp.StatusCode) {
		var errResp struct {
			Mensagem string `json:"mensagem"`
		}
		_ = json.Unmarshal(body, &errResp)
		return fmt.Errorf("cancel order failed: status %d, message: %s", resp.StatusCode, errResp.Mensagem)
	}

	return nil
}

// UpdateOrderItems replaces the order's item grid via PUT /pedidos/{id}/itens.
// Spec (v3.1): "O corpo substitui os itens atuais; totais, impostos e valores
// das parcelas existentes são recalculados." — nunca chamar depois de gravar
// parcelas reais sem reenviá-las. Com estoque lançado o Tiny bloqueia com
// 400 motivosBloqueio "estoque lançado" (validado em sandbox 11/07): o ciclo
// obrigatório é estornar-estoque → PUT /itens → lancar-estoque.
func (t *Tiny) UpdateOrderItems(ctx context.Context, orderID string, items []providers.ERPOrderItem) error {
	endpoint := fmt.Sprintf("%s/pedidos/%s/itens", tinyAPIBaseURL, orderID)

	grid := make([]map[string]any, len(items))
	for i, item := range items {
		productID, _ := strconv.ParseInt(item.ProductID, 10, 64)
		grid[i] = map[string]any{
			"produto":       map[string]any{"id": productID},
			"quantidade":    item.Quantity,
			"valorUnitario": float64(item.UnitPrice) / 100,
		}
	}

	resp, body, err := t.DoRequest(ctx, http.MethodPut, endpoint, map[string]any{"itens": grid}, t.authHeaders())
	if err != nil {
		return fmt.Errorf("updating order items: %w", err)
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return fmt.Errorf("update order items failed: status %d: %s", resp.StatusCode, tinyErrorDetail(body))
	}
	logger.From(ctx, t.Logger).Info("tiny order items updated",
		zap.String("order_id", orderID),
		zap.Int("items", len(items)),
	)
	return nil
}

// UpdateOrderPayment writes the real gateway installments onto an existing
// order via PUT /pedidos/{id} — pagamento/parcelas only, zero movimentação de
// estoque (shape do swagger {dias,data,valor} validado em sandbox 11/07; o
// PUT parcial não anula campos omitidos).
func (t *Tiny) UpdateOrderPayment(ctx context.Context, orderID string, payment *providers.ERPOrderPayment) error {
	if payment == nil {
		return nil
	}
	endpoint := fmt.Sprintf("%s/pedidos/%s", tinyAPIBaseURL, orderID)

	// formaRecebimento é OMITIDO de propósito: o pedido do fluxo
	// pedido-como-reserva nasce SEM pagamento (situação Aberta), e o Tiny
	// valida a formaRecebimento da parcela contra a do PEDIDO — divergência
	// rejeita com 400 "Forma de recebimento da parcela diferente da forma de
	// recebimento do pedido" (observado no E2E de 11/07 contra a conta real;
	// o PUT /pedidos não aceita formaRecebimento no nível do pedido para
	// alinhar antes). Custo: a parcela cai na categorização default de contas
	// a receber — refinamento futuro: gravar pagamento.formaRecebimento já no
	// POST da conversão quando o método do checkout for conhecido.
	parcelas := buildTinyParcelas(payment, nil, nil)
	resp, body, err := t.DoRequest(ctx, http.MethodPut, endpoint, map[string]any{
		"pagamento": map[string]any{"parcelas": parcelas},
	}, t.authHeaders())
	if err != nil {
		return fmt.Errorf("updating order payment: %w", err)
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return fmt.Errorf("update order payment failed: status %d: %s", resp.StatusCode, tinyErrorDetail(body))
	}
	logger.From(ctx, t.Logger).Info("tiny order payment updated",
		zap.String("order_id", orderID),
		zap.String("method", payment.Method),
		zap.Int("parcelas", len(parcelas)),
	)
	return nil
}

// SetOrderSituacao transitions the order status via PUT /pedidos/{id}/situacao.
// Codes (swagger v3.1): 0 Aberta · 3 Aprovada · 2 Cancelada · 1 Faturada etc.
// Cancelar NÃO devolve estoque lançado (sandbox T7) — o par obrigatório do
// cancelamento é SetOrderSituacao(2) seguido de ReverseOrderStock.
func (t *Tiny) SetOrderSituacao(ctx context.Context, orderID string, situacao int) error {
	endpoint := fmt.Sprintf("%s/pedidos/%s/situacao", tinyAPIBaseURL, orderID)
	resp, body, err := t.DoRequest(ctx, http.MethodPut, endpoint, map[string]any{"situacao": situacao}, t.authHeaders())
	if err != nil {
		return fmt.Errorf("setting order situacao: %w", err)
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return fmt.Errorf("set order situacao %d failed: status %d: %s", situacao, resp.StatusCode, tinyErrorDetail(body))
	}
	return nil
}

// AddOrderMarker tags the order via POST /pedidos/{id}/marcadores. O marcador
// lc-cart-<cartID> é a âncora de idempotência do fluxo pedido-como-reserva.
func (t *Tiny) AddOrderMarker(ctx context.Context, orderID, marker string) error {
	endpoint := fmt.Sprintf("%s/pedidos/%s/marcadores", tinyAPIBaseURL, orderID)
	resp, body, err := t.DoRequest(ctx, http.MethodPost, endpoint, []map[string]any{{"descricao": marker}}, t.authHeaders())
	if err != nil {
		return fmt.Errorf("adding order marker: %w", err)
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return fmt.Errorf("add order marker failed: status %d: %s", resp.StatusCode, tinyErrorDetail(body))
	}
	return nil
}

// FindOrderIDByMarker resolves an order by marker via GET /pedidos?marcadores=.
// Match exato com read-after-write de ~300ms (sandbox T8; a forma com
// colchetes `marcadores[]=` NÃO funciona). Retorna "" quando não encontrado.
func (t *Tiny) FindOrderIDByMarker(ctx context.Context, marker string) (string, error) {
	endpoint := fmt.Sprintf("%s/pedidos?marcadores=%s", tinyAPIBaseURL, url.QueryEscape(marker))
	resp, body, err := t.DoRequestWithRetry(ctx, 2, http.MethodGet, endpoint, nil, t.authHeaders())
	if err != nil {
		return "", fmt.Errorf("searching order by marker: %w", err)
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return "", fmt.Errorf("search order by marker failed: status %d: %s", resp.StatusCode, tinyErrorDetail(body))
	}
	var result struct {
		Itens []struct {
			ID int64 `json:"id"`
		} `json:"itens"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parsing marker search response: %w", err)
	}
	if len(result.Itens) == 0 {
		return "", nil
	}
	return strconv.FormatInt(result.Itens[0].ID, 10), nil
}

// ReserveStock creates a manual stock exit (tipo S) in Tiny for the given product.
// POST /estoque/{idProduto} — returns the movement ID (idLancamento).
//
// Retenta, mas NÃO em tudo — e a distinção é a coisa mais importante desta
// função. Este POST CRIA um lançamento: não é idempotente, e a API do Tiny não
// oferece consulta de lançamentos (só criar e estornar), então não há como
// perguntar depois "chegou?".
//
// Numa falha de discagem — conexão recusada, host não resolvido, rede
// inalcançável — a requisição comprovadamente não chegou à aplicação do Tiny, e
// repetir é seguro. Num TIMEOUT não se sabe: o Tiny pode ter processado a saída
// e demorado a responder. Repetir ali cria um SEGUNDO lançamento, e o índice
// único de reserva ativa por cart+produto garante que só um seria registrado do
// nosso lado — o outro fica órfão, retirando do Tiny estoque que ninguém
// comprou, e o estorno da expiração devolve só um.
//
// Perder a reserva é ruim e detectável. Criar uma reserva fantasma é ruim,
// invisível e permanente. Enquanto não existir estado `pending` para retomar a
// tentativa com segurança, o timeout sobe como erro.
func (t *Tiny) ReserveStock(ctx context.Context, productID string, qty int, unitPrice float64, obs string) (string, error) {
	endpoint := fmt.Sprintf("%s/estoque/%s", tinyAPIBaseURL, productID)
	payload := map[string]any{
		"tipo":          "S",
		"quantidade":    qty,
		"precoUnitario": unitPrice,
		"observacoes":   obs,
	}

	resp, body, err := t.postComRetryDeDiscagem(ctx, endpoint, payload)
	if err != nil {
		// Falha de discagem que sobreviveu ao retry: nenhum byte chegou à
		// aplicação do Tiny. O sentinela diz ao ledger que re-executar é seguro.
		if falhaDeDiscagem(err) {
			return "", fmt.Errorf("reserving stock: %w", errors.Join(providers.ErrProvenUndelivered, err))
		}
		return "", fmt.Errorf("reserving stock: %w", err)
	}

	if !providers.IsSuccessStatus(resp.StatusCode) {
		var errResp struct {
			Mensagem string `json:"mensagem"`
		}
		_ = json.Unmarshal(body, &errResp)
		reject := fmt.Errorf("reserve stock failed: status %d, message: %s", resp.StatusCode, errResp.Mensagem)
		// 4xx é recusa de validação: o Tiny processou e disse não ANTES de
		// aplicar. Provado não-aplicado; repetível (vai falhar igual até a causa
		// ser corrigida, e o teto de tentativas para o loop).
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return "", errors.Join(providers.ErrProvenUndelivered, reject)
		}
		// 5xx fica ambíguo de propósito: o servidor respondeu, e pode ter
		// aplicado antes de quebrar.
		return "", reject
	}

	var result struct {
		IDLancamento int64 `json:"idLancamento"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parsing reserve stock response: %w", err)
	}

	return strconv.FormatInt(result.IDLancamento, 10), nil
}

// ReverseStockReservation creates a manual stock entry (tipo E) in Tiny for the given product.
// POST /estoque/{idProduto} — returns the movement ID (idLancamento).
func (t *Tiny) ReverseStockReservation(ctx context.Context, productID string, qty int, unitPrice float64, obs string) (string, error) {
	endpoint := fmt.Sprintf("%s/estoque/%s", tinyAPIBaseURL, productID)
	payload := map[string]any{
		"tipo":          "E",
		"quantidade":    qty,
		"precoUnitario": unitPrice,
		"observacoes":   obs,
	}

	resp, body, err := t.DoRequest(ctx, http.MethodPost, endpoint, payload, t.authHeaders())
	if err != nil {
		return "", fmt.Errorf("reversing stock reservation: %w", err)
	}

	if !providers.IsSuccessStatus(resp.StatusCode) {
		var errResp struct {
			Mensagem string `json:"mensagem"`
		}
		_ = json.Unmarshal(body, &errResp)
		return "", fmt.Errorf("reverse stock reservation failed: status %d, message: %s", resp.StatusCode, errResp.Mensagem)
	}

	var result struct {
		IDLancamento int64 `json:"idLancamento"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parsing reverse stock response: %w", err)
	}

	return strconv.FormatInt(result.IDLancamento, 10), nil
}

// SearchContacts searches for contacts by name in Tiny.
// GET /contatos?nome={name}&limit=10
//
// CPF/CNPJ note: Tiny's /contatos search does literal string matching on the
// stored value, and Tiny stores the document with its canonical mask
// ("096.760.139-86" for CPF, "00.000.000/0000-00" for CNPJ). Sending raw
// digits silently returns no matches even when the contact exists, which then
// drives a duplicate-create against the very contact we were trying to find.
// We normalise to the formatted variant before querying.
func (t *Tiny) SearchContacts(ctx context.Context, params SearchContactsParams) ([]ERPContactResult, error) {
	q := url.Values{}
	if params.Name != "" {
		q.Set("nome", params.Name)
	}
	if params.CpfCnpj != "" {
		q.Set("cpfCnpj", formatBrazilianDocument(params.CpfCnpj))
	}
	q.Set("limit", "10")
	endpoint := tinyAPIBaseURL + "/contatos?" + q.Encode()

	resp, body, err := t.DoRequest(ctx, http.MethodGet, endpoint, nil, t.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("searching contacts: %w", err)
	}

	if resp.StatusCode == http.StatusNoContent {
		return []ERPContactResult{}, nil
	}

	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("search contacts failed: status %d", resp.StatusCode)
	}

	var contactResp struct {
		Itens []struct {
			ID   int64  `json:"id"`
			Nome string `json:"nome"`
		} `json:"itens"`
	}

	if err := json.Unmarshal(body, &contactResp); err != nil {
		return nil, fmt.Errorf("parsing contacts response: %w", err)
	}

	results := make([]ERPContactResult, len(contactResp.Itens))
	for i, c := range contactResp.Itens {
		results[i] = ERPContactResult{
			ContactID: strconv.FormatInt(c.ID, 10),
			Name:      c.Nome,
		}
	}

	return results, nil
}

// CreateContact creates a new contact in Tiny.
// POST /contatos
func (t *Tiny) CreateContact(ctx context.Context, contact ERPContactInput) (*ERPContactResult, error) {
	endpoint := tinyAPIBaseURL + "/contatos"

	payload := map[string]any{
		"nome": contact.Name,
	}
	if contact.PersonType != "" {
		payload["tipoPessoa"] = contact.PersonType
	} else {
		payload["tipoPessoa"] = "F" // Default: Pessoa Física
	}
	if contact.CpfCnpj != "" {
		// Tiny stores the document in its canonical masked format, so we
		// send it that way too. Keeps create/update symmetric with
		// SearchContacts and avoids letting Tiny silently double-store
		// the same document under different representations.
		payload["cpfCnpj"] = formatBrazilianDocument(contact.CpfCnpj)
	}
	if contact.Email != "" {
		payload["email"] = contact.Email
	}
	if contact.Phone != "" {
		payload["celular"] = contact.Phone
	}

	resp, body, err := t.DoRequest(ctx, http.MethodPost, endpoint, payload, t.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("creating contact: %w", err)
	}

	if !providers.IsSuccessStatus(resp.StatusCode) {
		// Log the (masked) payload alongside Tiny's per-field error so we can
		// see which value was rejected — the generic top-level "Ocorreram
		// erros de validação" is useless on its own.
		if t.Logger != nil {
			logger.From(ctx, t.Logger).Warn("tiny CreateContact rejected",
				zap.Int("status", resp.StatusCode),
				zap.String("error", tinyErrorDetail(body)),
				zap.String("nome", contact.Name),
				zap.Int("nome_len", len(contact.Name)),
				zap.String("tipoPessoa", payload["tipoPessoa"].(string)),
				zap.String("cpfCnpj_masked", maskDocument(contact.CpfCnpj)),
				zap.Int("cpfCnpj_len", len(contact.CpfCnpj)),
				zap.String("email_masked", maskEmail(contact.Email)),
				zap.Int("email_len", len(contact.Email)),
				zap.String("celular_masked", maskPhone(contact.Phone)),
				zap.Int("celular_len", len(contact.Phone)),
			)
		}
		return nil, fmt.Errorf("create contact failed: status %d: %s", resp.StatusCode, tinyErrorDetail(body))
	}

	var contactResp struct {
		ID int64 `json:"id"`
	}

	if err := json.Unmarshal(body, &contactResp); err != nil {
		return nil, fmt.Errorf("parsing contact response: %w", err)
	}

	return &ERPContactResult{
		ContactID: strconv.FormatInt(contactResp.ID, 10),
		Name:      contact.Name,
	}, nil
}

// formatBrazilianDocument returns CPF / CNPJ in Tiny's canonical masked
// representation: "000.000.000-00" for 11 digits, "00.000.000/0000-00" for
// 14 digits. Strips any existing punctuation/whitespace before formatting,
// so it's safe to call on already-formatted input. Anything else (empty,
// wrong length) is returned untouched — the caller will get back exactly
// what it sent and Tiny will report whatever validation error applies.
func formatBrazilianDocument(doc string) string {
	digits := make([]byte, 0, len(doc))
	for i := 0; i < len(doc); i++ {
		if doc[i] >= '0' && doc[i] <= '9' {
			digits = append(digits, doc[i])
		}
	}
	switch len(digits) {
	case 11:
		return fmt.Sprintf("%s.%s.%s-%s", digits[0:3], digits[3:6], digits[6:9], digits[9:11])
	case 14:
		return fmt.Sprintf("%s.%s.%s/%s-%s", digits[0:2], digits[2:5], digits[5:8], digits[8:12], digits[12:14])
	}
	return doc
}

// maskEmail returns "abc***@domain.com" — preserves the domain (where most
// validation issues live: TLD, length, accepted characters) and the first
// three local-part chars without leaking the full address into logs.
func maskEmail(email string) string {
	if email == "" {
		return ""
	}
	at := strings.IndexByte(email, '@')
	if at < 0 {
		return "***"
	}
	local := email[:at]
	domain := email[at:]
	if len(local) <= 3 {
		return "***" + domain
	}
	return local[:3] + "***" + domain
}

// maskDocument keeps the first 3 and last 2 digits of a CPF/CNPJ — enough
// to spot formatting issues (dots/dashes/spaces, wrong length) without
// exposing the full document.
func maskDocument(doc string) string {
	if doc == "" {
		return ""
	}
	if len(doc) <= 5 {
		return "***"
	}
	return doc[:3] + "***" + doc[len(doc)-2:]
}

// maskPhone keeps DDD + last 2 digits.
func maskPhone(phone string) string {
	if phone == "" {
		return ""
	}
	if len(phone) <= 4 {
		return "***"
	}
	return phone[:2] + "***" + phone[len(phone)-2:]
}

// UpdateContact patches an existing contact with fresh customer data. Used
// after the merchant types the customer's real name on checkout — the
// long-lived contact created by an earlier order under the @handle gets
// rewritten so the Tiny order shows "Alisson Augusto Dahlem" instead of
// "alisson_dahlem".
// PUT /contatos/{id}
func (t *Tiny) UpdateContact(ctx context.Context, contactID string, contact ERPContactInput) error {
	cID, err := strconv.ParseInt(contactID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid contact ID %q: %w", contactID, err)
	}
	endpoint := fmt.Sprintf("%s/contatos/%d", tinyAPIBaseURL, cID)

	payload := map[string]any{}
	if contact.Name != "" {
		payload["nome"] = contact.Name
	}
	if contact.PersonType != "" {
		payload["tipoPessoa"] = contact.PersonType
	}
	if contact.CpfCnpj != "" {
		// Tiny stores the document in its canonical masked format, so we
		// send it that way too. Keeps create/update symmetric with
		// SearchContacts and avoids letting Tiny silently double-store
		// the same document under different representations.
		payload["cpfCnpj"] = formatBrazilianDocument(contact.CpfCnpj)
	}
	if contact.Email != "" {
		payload["email"] = contact.Email
	}
	if contact.Phone != "" {
		payload["celular"] = contact.Phone
	}
	if len(payload) == 0 {
		return nil
	}

	resp, body, err := t.DoRequest(ctx, http.MethodPut, endpoint, payload, t.authHeaders())
	if err != nil {
		return fmt.Errorf("updating contact: %w", err)
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return fmt.Errorf("update contact failed: status %d: %s", resp.StatusCode, tinyErrorDetail(body))
	}
	return nil
}

// =============================================================================
// NFe (nota fiscal) lookup — implements providers.ERPInvoiceProvider.
// =============================================================================

// tinyNotaFiscalLite captures the subset of Tiny's nota fiscal payload we use.
// It maps both NotaFiscalModel (returned standalone by GET /notafiscal/{id})
// and the embedded notaFiscal field on a pedido response — fields not present
// in one shape stay zero-valued and the caller falls back accordingly.
type tinyNotaFiscalLite struct {
	ID          int64       `json:"id"`
	Numero      json.Number `json:"numero"`
	Serie       json.Number `json:"serie"`
	Situacao    json.Number `json:"situacao"`
	ChaveAcesso string      `json:"chaveAcesso"`
	LinkAcesso  string      `json:"linkAcesso"`
	DataEmissao string      `json:"dataEmissao"`
	XML         string      `json:"xml"`
}

// situacao codes returned by Tiny for NotaFiscalModel.situacao. Centralised so
// a future change to the mapping is a single edit.
const (
	tinyNFeSituacaoPendente            = 1
	tinyNFeSituacaoEmitida             = 2
	tinyNFeSituacaoCancelada           = 3
	tinyNFeSituacaoAguardandoRecibo    = 4
	tinyNFeSituacaoRejeitada           = 5
	tinyNFeSituacaoAutorizada          = 6
	tinyNFeSituacaoEmitidaDanfe        = 7
	tinyNFeSituacaoRegistrada          = 8
	tinyNFeSituacaoAguardandoProtocolo = 9
	tinyNFeSituacaoDenegada            = 10
)

// GetInvoiceByOrder fetches the order in Tiny and returns the NFe attached to
// it (if any). Returns providers.ErrInvoiceNotFound when the pedido has no
// nota fiscal yet — the merchant is still expected to emit it in the ERP.
func (t *Tiny) GetInvoiceByOrder(ctx context.Context, orderID string) (*providers.ERPInvoice, error) {
	if strings.TrimSpace(orderID) == "" {
		return nil, fmt.Errorf("orderID is required")
	}
	endpoint := fmt.Sprintf("%s/pedidos/%s", tinyAPIBaseURL, orderID)

	resp, body, err := t.DoRequest(ctx, http.MethodGet, endpoint, nil, t.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("getting tiny order: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, providers.ErrInvoiceNotFound
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("get tiny order failed: status %d: %s", resp.StatusCode, tinyErrorDetail(body))
	}

	// Tiny v3 returns a single notaFiscal object on the pedido once the
	// merchant has emitted it. Some installations report it as an array of
	// historical entries (re-emissions), so we tolerate both and pick the
	// most "advanced" one (highest situacao wins) so a re-emitted NFe wins
	// over a previously-cancelled one.
	var orderResp struct {
		NotaFiscal   *tinyNotaFiscalLite  `json:"notaFiscal"`
		NotasFiscais []tinyNotaFiscalLite `json:"notasFiscais"`
		Ecommerce    struct {
			NotaFiscal *tinyNotaFiscalLite `json:"notaFiscal"`
		} `json:"ecommerce"`
	}
	if err := json.Unmarshal(body, &orderResp); err != nil {
		return nil, fmt.Errorf("parsing tiny order response: %w", err)
	}

	candidates := make([]tinyNotaFiscalLite, 0, len(orderResp.NotasFiscais)+2)
	if orderResp.NotaFiscal != nil {
		candidates = append(candidates, *orderResp.NotaFiscal)
	}
	if orderResp.Ecommerce.NotaFiscal != nil {
		candidates = append(candidates, *orderResp.Ecommerce.NotaFiscal)
	}
	candidates = append(candidates, orderResp.NotasFiscais...)

	// Discard empties (Tiny sometimes returns the field with id=0 for orders
	// that haven't been invoiced) and pick the best representative.
	best, ok := pickBestNotaFiscal(candidates)
	if !ok {
		return nil, providers.ErrInvoiceNotFound
	}
	return tinyNotaFiscalToERP(best), nil
}

// GetInvoiceByID fetches a NFe directly by its Tiny-side id. Used when a
// webhook hands us the notafiscal id without the chave de acesso.
func (t *Tiny) GetInvoiceByID(ctx context.Context, invoiceID string) (*providers.ERPInvoice, error) {
	if strings.TrimSpace(invoiceID) == "" {
		return nil, fmt.Errorf("invoiceID is required")
	}
	endpoint := fmt.Sprintf("%s/notafiscal/%s", tinyAPIBaseURL, invoiceID)

	resp, body, err := t.DoRequest(ctx, http.MethodGet, endpoint, nil, t.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("getting tiny nota fiscal: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, providers.ErrInvoiceNotFound
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("get tiny nota fiscal failed: status %d: %s", resp.StatusCode, tinyErrorDetail(body))
	}

	var nfe tinyNotaFiscalLite
	if err := json.Unmarshal(body, &nfe); err != nil {
		return nil, fmt.Errorf("parsing tiny nota fiscal response: %w", err)
	}
	if nfe.ID == 0 {
		return nil, providers.ErrInvoiceNotFound
	}
	return tinyNotaFiscalToERP(nfe), nil
}

// GetInvoiceXML fetches the XML payload for a NFe. Tiny returns it as a
// JSON-wrapped string ({ "xmlNfe": "..." }) so we unwrap before returning.
func (t *Tiny) GetInvoiceXML(ctx context.Context, invoiceID string) ([]byte, error) {
	if strings.TrimSpace(invoiceID) == "" {
		return nil, fmt.Errorf("invoiceID is required")
	}
	endpoint := fmt.Sprintf("%s/notafiscal/%s/xml", tinyAPIBaseURL, invoiceID)

	resp, body, err := t.DoRequest(ctx, http.MethodGet, endpoint, nil, t.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("getting tiny nota fiscal xml: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, providers.ErrInvoiceNotFound
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("get tiny nota fiscal xml failed: status %d: %s", resp.StatusCode, tinyErrorDetail(body))
	}

	var wrapped struct {
		XMLNfe string `json:"xmlNfe"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.XMLNfe != "" {
		return []byte(wrapped.XMLNfe), nil
	}
	// Fallback: some Tiny tenants return the xml as text/xml directly.
	return body, nil
}

// pickBestNotaFiscal chooses the most relevant NFe from a list. Cancelled
// (situacao=3) loses to anything authorised; among remaining candidates the
// one with the highest id (most recent) wins so re-emissions override stale
// drafts.
func pickBestNotaFiscal(in []tinyNotaFiscalLite) (tinyNotaFiscalLite, bool) {
	var best tinyNotaFiscalLite
	found := false
	for _, n := range in {
		if n.ID == 0 {
			continue
		}
		if !found {
			best = n
			found = true
			continue
		}
		bs, _ := best.Situacao.Int64()
		ns, _ := n.Situacao.Int64()
		// Prefer authorised over cancelled/rejected/denegada.
		bestActive := bs != tinyNFeSituacaoCancelada && bs != tinyNFeSituacaoRejeitada && bs != tinyNFeSituacaoDenegada
		newActive := ns != tinyNFeSituacaoCancelada && ns != tinyNFeSituacaoRejeitada && ns != tinyNFeSituacaoDenegada
		if newActive && !bestActive {
			best = n
			continue
		}
		if !newActive && bestActive {
			continue
		}
		// Tie-break by id (most recent emission wins).
		if n.ID > best.ID {
			best = n
		}
	}
	return best, found
}

// tinyNotaFiscalToERP normalises Tiny's NFe shape into providers.ERPInvoice.
func tinyNotaFiscalToERP(in tinyNotaFiscalLite) *providers.ERPInvoice {
	situacao, _ := in.Situacao.Int64()
	status := mapTinyNFeStatus(int(situacao))

	out := &providers.ERPInvoice{
		InvoiceID: strconv.FormatInt(in.ID, 10),
		Number:    in.Numero.String(),
		Series:    in.Serie.String(),
		AccessKey: strings.TrimSpace(in.ChaveAcesso),
		Status:    status,
		StatusRaw: in.Situacao.String(),
	}
	if in.XML != "" {
		out.XMLContent = []byte(in.XML)
	}
	if in.DataEmissao != "" {
		// Tiny emits dataEmissao either as RFC3339 or as Brazilian "dd/mm/aaaa hh:mm:ss".
		if t, err := time.Parse(time.RFC3339, in.DataEmissao); err == nil {
			out.IssuedAt = t
		} else if t, err := time.ParseInLocation("02/01/2006 15:04:05", in.DataEmissao, tinyLocation); err == nil {
			out.IssuedAt = t
		} else if t, err := time.ParseInLocation("02/01/2006", in.DataEmissao, tinyLocation); err == nil {
			out.IssuedAt = t
		}
	}
	return out
}

func mapTinyNFeStatus(situacao int) providers.ERPInvoiceStatus {
	switch situacao {
	case tinyNFeSituacaoEmitida,
		tinyNFeSituacaoAutorizada,
		tinyNFeSituacaoEmitidaDanfe:
		return providers.ERPInvoiceStatusAuthorized
	case tinyNFeSituacaoCancelada:
		return providers.ERPInvoiceStatusCancelled
	case tinyNFeSituacaoRejeitada,
		tinyNFeSituacaoDenegada:
		return providers.ERPInvoiceStatusRejected
	default:
		// Pendente, Aguardando Recibo, Aguardando Protocolo, Registrada — all
		// in-flight states from LiveCart's perspective.
		return providers.ERPInvoiceStatusPending
	}
}

// authHeaders returns the authorization headers for API v3 requests.
func (t *Tiny) authHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + t.credentials.AccessToken,
	}
}

// tinyErrorDetail extracts the per-field validation messages Tiny returns
// alongside the generic top-level "mensagem". Without these the merchant /
// log only sees "Ocorreram erros de validação" with no clue about which
// field is wrong. Falls back to the raw (truncated) body when nothing
// recognisable is found.
//
// Tiny v3 ErrorDTO shape: { "mensagem": "...", "detalhes": [{ "campo": "...", "mensagem": "..." }] }.
// Earlier versions of this parser looked for "mensagens" / "erros" arrays
// that don't exist in v3, so per-field details were silently dropped and
// every Tiny 400 surfaced as the generic top-level message only.
func tinyErrorDetail(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var parsed struct {
		Mensagem string `json:"mensagem"`
		Detalhes []struct {
			Campo    string `json:"campo"`
			Mensagem string `json:"mensagem"`
		} `json:"detalhes"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return truncate(string(body), 400)
	}

	parts := make([]string, 0, len(parsed.Detalhes)+1)
	if parsed.Mensagem != "" {
		parts = append(parts, parsed.Mensagem)
	}
	for _, d := range parsed.Detalhes {
		if d.Campo != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", d.Campo, d.Mensagem))
		} else {
			parts = append(parts, d.Mensagem)
		}
	}
	if len(parts) == 0 {
		return truncate(string(body), 400)
	}
	return strings.Join(parts, " | ")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func boolToSituacao(active bool) string {
	if active {
		return "A"
	}
	return "I"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// HealthCheck audits the merchant's Tiny cadastros against the canonical
// names LiveCart looks up at order time. Catches the silent fallback
// problem where a payment method or shipping carrier is missing and the
// parcela falls into "Conta a Receber" or the shipment uses Tiny's default
// carrier instead of the one the customer chose at checkout.
//
// All Tiny lookups run in parallel via errgroup — sequentially this took
// ~1.5s per audit (7 round trips × ~200ms each), and the FE banner stays
// invisible until the result lands. Going concurrent collapses the wall
// time to roughly the slowest single lookup. Best-effort: any item whose
// lookup errors out is reported as missing rather than failing the whole
// check, so the merchant always sees the rest.
func (t *Tiny) HealthCheck(ctx context.Context) (*providers.ERPHealthCheckResult, error) {
	type job struct {
		category     providers.ERPHealthCheckCategory
		expectedName string
		description  string
		panelPath    string
		run          func(ctx context.Context) (int64, error)
	}

	jobs := []job{
		// ATENÇÃO à nomenclatura da Tiny (relato de campo, jul/2026): o
		// endpoint /formas-pagamento valida os MEIOS que o painel exibe em
		// "Formas de RECEBIMENTO" (contas a receber = como o cliente paga o
		// lojista), e /formas-recebimento cobre o fluxo que o painel chama
		// de "Formas de Pagamento". As categorias/painéis abaixo apontam o
		// usuário pra página CERTA do painel — não "corrigir" pelos nomes
		// dos endpoints.
		{
			category:     providers.ERPHealthFormaRecebimento,
			expectedName: "Cartão de Crédito",
			description:  "Define o instrumento da parcela (Cartão de Crédito). Sem isso, a parcela fica como Conta a Receber genérica.",
			panelPath:    "Configurações → Finanças → Formas de Recebimento",
			run:          func(ctx context.Context) (int64, error) { return t.lookupFormaPagamentoID(ctx, "credit_card") },
		},
		{
			category:     providers.ERPHealthFormaRecebimento,
			expectedName: "Pix",
			description:  "Define o instrumento da parcela (Pix). Sem isso, a parcela fica como Conta a Receber genérica.",
			panelPath:    "Configurações → Finanças → Formas de Recebimento",
			run:          func(ctx context.Context) (int64, error) { return t.lookupFormaPagamentoID(ctx, "pix") },
		},
		{
			category:     providers.ERPHealthFormaPagamento,
			expectedName: "Cartão de Crédito",
			description:  "Define o fluxo financeiro da parcela (Cartão de Crédito). Necessário para classificar a entrada.",
			panelPath:    "Configurações → Finanças → Formas de Pagamento",
			run:          func(ctx context.Context) (int64, error) { return t.lookupFormaRecebimentoID(ctx, "credit_card") },
		},
		{
			category:     providers.ERPHealthFormaPagamento,
			expectedName: "Pix",
			description:  "Define o fluxo financeiro da parcela (Pix). Necessário para classificar a entrada.",
			panelPath:    "Configurações → Finanças → Formas de Pagamento",
			run:          func(ctx context.Context) (int64, error) { return t.lookupFormaRecebimentoID(ctx, "pix") },
		},
		// Formas de Envio (transportador.formaEnvio on order)
		{
			category:     providers.ERPHealthFormaEnvio,
			expectedName: "Correios",
			description:  "Necessária para que o pedido entre no Tiny já com a transportadora Correios associada (etiqueta + acompanhamento).",
			panelPath:    "Cadastros → Formas de Envio",
			run:          func(ctx context.Context) (int64, error) { return t.lookupFormaEnvioID(ctx, "Correios") },
		},
		{
			category:     providers.ERPHealthFormaEnvio,
			expectedName: "Jadlog",
			description:  "Necessária para que o pedido entre no Tiny já com a transportadora Jadlog associada (etiqueta + acompanhamento).",
			panelPath:    "Cadastros → Formas de Envio",
			run:          func(ctx context.Context) (int64, error) { return t.lookupFormaEnvioID(ctx, "Jadlog") },
		},
		{
			category:     providers.ERPHealthFormaEnvio,
			expectedName: "SmartEnvios",
			description:  "Necessária para que o pedido entre no Tiny já com a transportadora SmartEnvios associada (etiqueta + acompanhamento).",
			panelPath:    "Cadastros → Formas de Envio",
			run:          func(ctx context.Context) (int64, error) { return t.lookupFormaEnvioID(ctx, "SmartEnvios") },
		},
	}

	items := make([]providers.ERPHealthCheckItem, len(jobs))
	g, gctx := errgroup.WithContext(ctx)
	// Concorrência 3 (era 8): o burst de 7 lookups simultâneos estourava o
	// rate limit do Tiny e enchia a auditoria de falsos transitórios. Com 3,
	// o wall-time segue ~3 roundtrips e o 429 praticamente some — e quando
	// acontecer, agora vira status "unknown", não falsa pendência.
	g.SetLimit(3)

	for i := range jobs {
		i := i
		j := jobs[i]
		g.Go(func() error {
			id, err := j.run(gctx)
			items[i] = healthCheckItem(j.category, j.expectedName, id, err, j.description, j.panelPath)
			// Lookup-level failures are reported on the item itself
			// (status=missing). Returning nil keeps the rest of the
			// audit running even when one endpoint blips.
			return nil
		})
	}

	start := time.Now()
	if err := g.Wait(); err != nil {
		// errgroup returns the first non-nil error from a goroutine —
		// because each Go func above returns nil unconditionally, this
		// path is only reached if the parent context is cancelled.
		return nil, fmt.Errorf("running tiny health check: %w", err)
	}

	// Summary log to diagnose flapping counts. Per-item: name, category,
	// whether matched. Per-category: total/missing breakdown. We log even
	// on success so two consecutive runs can be diff'd to find which
	// specific lookup is unstable (e.g. one returning 5xx intermittently).
	missingByCategory := map[providers.ERPHealthCheckCategory]int{}
	itemSnapshot := make([]map[string]any, len(items))
	for i, it := range items {
		if it.Status == providers.ERPHealthStatusMissing {
			missingByCategory[it.Category]++
		}
		itemSnapshot[i] = map[string]any{
			"category":      string(it.Category),
			"expected_name": it.ExpectedName,
			"status":        string(it.Status),
			"matched_id":    it.MatchedID,
		}
	}
	totalMissing := 0
	for _, c := range missingByCategory {
		totalMissing += c
	}
	logger.From(ctx, t.Logger).Info("tiny health check completed",
		zap.Duration("duration", time.Since(start)),
		zap.Int("items_total", len(items)),
		zap.Int("items_missing", totalMissing),
		zap.Int("missing_forma_pagamento", missingByCategory[providers.ERPHealthFormaPagamento]),
		zap.Int("missing_forma_recebimento", missingByCategory[providers.ERPHealthFormaRecebimento]),
		zap.Int("missing_forma_envio", missingByCategory[providers.ERPHealthFormaEnvio]),
		zap.Any("items", itemSnapshot),
	)

	return &providers.ERPHealthCheckResult{
		CheckedAt: time.Now(),
		Items:     items,
	}, nil
}

func healthCheckItem(
	cat providers.ERPHealthCheckCategory,
	expectedName string,
	matchedID int64,
	lookupErr error,
	description string,
	panelPath string,
) providers.ERPHealthCheckItem {
	item := providers.ERPHealthCheckItem{
		Category:     cat,
		ExpectedName: expectedName,
		Description:  description,
		PanelPath:    panelPath,
	}
	if lookupErr == nil && matchedID > 0 {
		item.Status = providers.ERPHealthStatusOK
		item.MatchedID = matchedID
		item.MatchedName = expectedName // best-effort, lookup already case-folds
		return item
	}
	if lookupErr != nil {
		// Falha de lookup (429/timeout/5xx) ≠ cadastro ausente: reporta
		// "unknown" para o FE mostrar "não verificado" em vez de pendência.
		item.Status = providers.ERPHealthStatusUnknown
		return item
	}
	item.Status = providers.ERPHealthStatusMissing
	return item
}

// maxTentativasDeDiscagem é quantas vezes uma falha de discagem é repetida.
//
// Três tentativas, com espera crescente entre elas: uma queda de rede de alguns
// segundos deixa de custar a reserva.
const maxTentativasDeDiscagem = 3

// esperaEntreTentativas cresce a cada tentativa (1s, 3s), para não bater de novo
// no mesmo instante em que a rede está caída.
var esperaEntreTentativas = []time.Duration{time.Second, 3 * time.Second}

// postComRetryDeDiscagem repete o POST apenas quando a falha PROVA que nada foi
// processado do outro lado.
//
// Ver o comentário de ReserveStock: num endpoint que cria lançamento e não tem
// consulta, repetir o que é ambíguo troca um problema detectável por um
// invisível.
func (t *Tiny) postComRetryDeDiscagem(ctx context.Context, endpoint string, payload any) (*http.Response, []byte, error) {
	var resp *http.Response
	var body []byte
	var err error

	for tentativa := 0; tentativa < maxTentativasDeDiscagem; tentativa++ {
		if tentativa > 0 {
			espera := esperaEntreTentativas[min(tentativa-1, len(esperaEntreTentativas)-1)]
			logger.From(ctx, t.Logger).Warn("retrying Tiny stock movement after a dial failure",
				zap.Int("attempt", tentativa+1),
				zap.Duration("wait", espera),
				zap.Error(err),
			)
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(espera):
			}
		}

		resp, body, err = t.DoRequest(ctx, http.MethodPost, endpoint, payload, t.authHeaders())
		if err == nil || !falhaDeDiscagem(err) {
			return resp, body, err
		}
	}
	return resp, body, err
}

// falhaDeDiscagem diz se o erro prova que a requisição não chegou à aplicação.
//
// Conexão recusada, host não resolvido e rede inalcançável acontecem ANTES de
// qualquer byte ser processado. Timeout fica de fora de propósito: ele não prova
// nada, e é justamente o caso ambíguo.
func falhaDeDiscagem(err error) bool {
	if err == nil {
		return false
	}
	// Timeout nunca é falha de discagem para este fim, mesmo quando o pacote
	// de rede o classifica como tal.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH)
}
