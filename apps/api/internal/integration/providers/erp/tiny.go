package erp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/ratelimit"
)

const (
	// Tiny API v3 base URL
	tinyAPIBaseURL = "https://api.tiny.com.br/public-api/v3"
)

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
	t.Logger.Info("Tiny token refresh successful",
		zap.Int("expires_in", tokenResp.ExpiresIn),
		zap.Bool("has_new_refresh_token", tokenResp.RefreshToken != ""),
	)

	// Default to 4 hours if expires_in is 0 or not provided
	// Tiny access tokens typically last about 4 hours
	expiresInSeconds := tokenResp.ExpiresIn
	if expiresInSeconds <= 0 {
		t.Logger.Warn("Tiny token refresh: expires_in is 0 or negative, defaulting to 4 hours",
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

	// Build query string
	query := "?"
	if params.PageSize > 0 {
		query += fmt.Sprintf("limit=%d&", params.PageSize)
	}
	if params.GTIN != "" {
		query += fmt.Sprintf("gtin=%s&", params.GTIN)
	} else if params.SKU != "" {
		query += fmt.Sprintf("codigo=%s&", params.SKU)
	} else if params.Search != "" {
		query += fmt.Sprintf("nome=%s&", params.Search)
	}
	if params.ActiveOnly {
		query += "situacao=A&"
	}
	if params.UpdatedAfter != nil {
		query += fmt.Sprintf("dataAlteracao=%s&", params.UpdatedAfter.Format("2006-01-02 15:04:05"))
	}

	fullURL := endpoint + query

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
			Stock:     0,                  // Not available in list response — enriched via GetProduct
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
func (t *Tiny) GetProduct(ctx context.Context, productID string) (*ERPProduct, error) {
	endpoint := fmt.Sprintf("%s/produtos/%s", tinyAPIBaseURL, productID)

	resp, body, err := t.DoRequest(ctx, http.MethodGet, endpoint, nil, t.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("getting product: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("product not found: %s", productID)
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("get product failed: status %d", resp.StatusCode)
	}

	var p tinyProductPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("parsing product response: %w", err)
	}

	out := tinyPayloadToERP(p)

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
		t.Logger.Info("tiny GetProduct shipping resolution", fields...)
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
		ID:          strconv.FormatInt(p.ID, 10),
		SKU:         p.SKU,
		GTIN:        p.GTIN,
		Name:        p.Descricao,
		Description: p.DescricaoComplementar,
		Price:       int64(math.Round(price * 100)),
		Stock:       int(p.Estoque.Quantidade),
		Active:      p.Situacao == "A",
		ImageURL:    imageURL,
		UpdatedAt:   updatedAt,
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
		"idContato":   contactID,
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

		// Try to resolve the formaEnvio id, preferring the carrier name
		// (Correios / Jadlog / etc.) and falling back to "SmartEnvios" so
		// stores that cadastrou só o agregador também batem.
		formaEnvioID, formaEnvioErr := t.lookupFormaEnvioID(ctx, ship.Carrier)
		formaEnvioName := ship.Carrier
		if formaEnvioErr == nil && formaEnvioID == 0 && ship.Carrier != "SmartEnvios" {
			id, err := t.lookupFormaEnvioID(ctx, "SmartEnvios")
			if err == nil && id > 0 {
				formaEnvioID = id
				formaEnvioName = "SmartEnvios"
			}
		}
		switch {
		case formaEnvioErr != nil:
			t.Logger.Warn("tiny formaEnvio lookup failed, sending order without it",
				zap.String("carrier", ship.Carrier),
				zap.Error(formaEnvioErr),
			)
		case formaEnvioID > 0:
			transportador["formaEnvio"] = map[string]any{"id": formaEnvioID}
			t.Logger.Info("tiny formaEnvio matched",
				zap.String("carrier", ship.Carrier),
				zap.String("matched_name", formaEnvioName),
				zap.Int64("forma_envio_id", formaEnvioID),
			)
		default:
			t.Logger.Warn("tiny formaEnvio lookup returned no match, order will use Tiny default",
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

	// Pagamento: Tiny v3 expects parcelas nested inside `pagamento`, and the
	// payment-method reference is `meioPagamento` (Pix/Cartão/etc.) — not the
	// `formaPagamento` key we used in v2. The v3 lookup endpoint is still
	// `/formas-pagamento` and returns IDs that map to `meioPagamento`.
	//
	// For credit-card sales we expand into one parcela per installment with
	// vencimentos D+30, D+60, ... so contas a receber reflects what Mercado
	// Pago will repass per cycle. Pix / debit / boleto stay as a single
	// parcela on the payment date (already settled or settles next-day).
	if pay := order.Payment; pay != nil {
		var meioRef map[string]any
		meioID, err := t.lookupFormaPagamentoID(ctx, pay.Method)
		switch {
		case err != nil:
			t.Logger.Warn("tiny meioPagamento lookup failed, sending parcelas without it",
				zap.String("method", pay.Method),
				zap.Error(err),
			)
		case meioID > 0:
			meioRef = map[string]any{"id": meioID}
		default:
			// No match for our payment-method name in the merchant's
			// /formas-pagamento — Tiny falls back to "Conta a Receber" on
			// the parcela's "Forma" column. Likely the merchant cadastrou
			// the meio with a different name (e.g. "Crédito", "Cartão MP").
			t.Logger.Warn("tiny meioPagamento lookup returned no match, parcela will show 'Conta a Receber'",
				zap.String("method", pay.Method),
			)
		}

		parcelas := buildTinyParcelas(pay, meioRef)
		pagamento := map[string]any{
			"parcelas": parcelas,
		}
		if meioRef != nil {
			// Mirror at parent level so Tiny applies it as default for the
			// pagamento block (matches the v3 schema example).
			pagamento["meioPagamento"] = meioRef
		}
		payload["pagamento"] = pagamento

		// Snapshot of the parcela schedule for log audit. With this we can
		// reconcile the merchant's contas a receber against what we sent
		// (Tiny may rewrite values but not the count / dates).
		firstDue, _ := parcelas[0]["data"].(string)
		lastDue, _ := parcelas[len(parcelas)-1]["data"].(string)
		t.Logger.Info("tiny order parcelas prepared",
			zap.String("method", pay.Method),
			zap.Int("installments", pay.Installments),
			zap.Int("parcelas_count", len(parcelas)),
			zap.String("first_due", firstDue),
			zap.String("last_due", lastDue),
			zap.Int64("amount_cents", pay.Amount),
			zap.Bool("had_money_release_date", pay.MoneyReleaseDate != nil),
		)
	}

	t.Logger.Info("tiny CreateOrder sending payload",
		zap.String("external_id", order.ExternalID),
		zap.String("contact_id", order.ContactID),
		zap.Int("items_count", len(order.Items)),
		zap.Int64("total_amount_cents", order.TotalAmount),
		zap.Bool("has_shipping_address", order.ShippingAddress != nil),
		zap.Bool("has_shipping_method", order.Shipping != nil),
		zap.Bool("has_payment", order.Payment != nil),
	)

	resp, body, err := t.DoRequest(ctx, http.MethodPost, endpoint, payload, t.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("creating order: %w", err)
	}

	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("create order failed: status %d: %s", resp.StatusCode, tinyErrorDetail(body))
	}

	var orderResp struct {
		ID     int64  `json:"id"`
		Numero string `json:"numeroPedido"`
	}

	if err := json.Unmarshal(body, &orderResp); err != nil {
		return nil, fmt.Errorf("parsing order response: %w", err)
	}

	orderID := strconv.FormatInt(orderResp.ID, 10)
	t.Logger.Info("tiny order created",
		zap.String("order_id", orderID),
		zap.String("numero_pedido", orderResp.Numero),
		zap.String("external_id", order.ExternalID),
	)

	// Approve the order so it shows under "Pedidos de Venda" when already paid.
	// Failure here is non-fatal — the order still exists in Tiny.
	if order.Payment != nil {
		if approveErr := t.ApproveOrder(ctx, orderID); approveErr != nil {
			t.Logger.Warn("failed to approve tiny order after creation",
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
func buildTinyParcelas(pay *providers.ERPOrderPayment, meioRef map[string]any) []map[string]any {
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

	if count == 1 {
		parcela := map[string]any{
			"dias":        firstDays,
			"data":        firstDue.In(tinyLocation).Format("2006-01-02"),
			"valor":       float64(pay.Amount) / 100,
			"observacoes": fmt.Sprintf("Pago via %s - ID %s", pay.Method, pay.PaymentID),
		}
		if meioRef != nil {
			parcela["meioPagamento"] = meioRef
		}
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
		if meioRef != nil {
			parcela["meioPagamento"] = meioRef
		}
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

	resp, body, err := t.DoRequest(ctx, http.MethodGet, endpoint, nil, t.authHeaders())
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
	if len(result.Itens) > 0 {
		return result.Itens[0].ID, nil
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

	resp, body, err := t.DoRequest(ctx, http.MethodGet, endpoint, nil, t.authHeaders())
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

// LaunchOrderStock decrements stock in Tiny for all items in the order.
// POST /pedidos/{idPedido}/lancar-estoque
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
			t.Logger.Info("stock already launched by Tiny automatically",
				zap.String("order_id", orderID),
			)
			return nil
		}

		return fmt.Errorf("launch stock failed: status %d, message: %s", resp.StatusCode, errResp.Mensagem)
	}

	t.Logger.Info("tiny order stock launched",
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

	t.Logger.Info("tiny order approved",
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
		t.Logger.Warn("failed to reverse stock before cancel, continuing",
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

// ReserveStock creates a manual stock exit (tipo S) in Tiny for the given product.
// POST /estoque/{idProduto} — returns the movement ID (idLancamento).
func (t *Tiny) ReserveStock(ctx context.Context, productID string, qty int, unitPrice float64, obs string) (string, error) {
	endpoint := fmt.Sprintf("%s/estoque/%s", tinyAPIBaseURL, productID)
	payload := map[string]any{
		"tipo":          "S",
		"quantidade":    qty,
		"precoUnitario": unitPrice,
		"observacoes":   obs,
	}

	resp, body, err := t.DoRequest(ctx, http.MethodPost, endpoint, payload, t.authHeaders())
	if err != nil {
		return "", fmt.Errorf("reserving stock: %w", err)
	}

	if !providers.IsSuccessStatus(resp.StatusCode) {
		var errResp struct {
			Mensagem string `json:"mensagem"`
		}
		_ = json.Unmarshal(body, &errResp)
		return "", fmt.Errorf("reserve stock failed: status %d, message: %s", resp.StatusCode, errResp.Mensagem)
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
func (t *Tiny) SearchContacts(ctx context.Context, params SearchContactsParams) ([]ERPContactResult, error) {
	endpoint := tinyAPIBaseURL + "/contatos?"

	if params.Name != "" {
		endpoint += fmt.Sprintf("nome=%s&", params.Name)
	}
	if params.CpfCnpj != "" {
		endpoint += fmt.Sprintf("cpfCnpj=%s&", params.CpfCnpj)
	}
	endpoint += "limit=10"

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
		payload["cpfCnpj"] = contact.CpfCnpj
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
		payload["cpfCnpj"] = contact.CpfCnpj
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
func tinyErrorDetail(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var parsed struct {
		Mensagem  string `json:"mensagem"`
		Mensagens []struct {
			Campo    string `json:"campo"`
			Mensagem string `json:"mensagem"`
		} `json:"mensagens"`
		Erros []struct {
			Campo    string `json:"campo"`
			Mensagem string `json:"mensagem"`
		} `json:"erros"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return truncate(string(body), 400)
	}

	parts := make([]string, 0, len(parsed.Mensagens)+len(parsed.Erros)+1)
	if parsed.Mensagem != "" {
		parts = append(parts, parsed.Mensagem)
	}
	for _, m := range parsed.Mensagens {
		if m.Campo != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", m.Campo, m.Mensagem))
		} else {
			parts = append(parts, m.Mensagem)
		}
	}
	for _, e := range parsed.Erros {
		if e.Campo != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", e.Campo, e.Mensagem))
		} else {
			parts = append(parts, e.Mensagem)
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
