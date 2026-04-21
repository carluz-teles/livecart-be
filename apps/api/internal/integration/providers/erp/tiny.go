package erp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"io"
	"net/http"
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

	var p struct {
		ID                    int64  `json:"id"`
		SKU                   string `json:"sku"`
		Descricao             string `json:"descricao"`
		DescricaoComplementar string `json:"descricaoComplementar"`
		Situacao              string `json:"situacao"`
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
	}

	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("parsing product response: %w", err)
	}

	price := p.Precos.Preco
	if p.Precos.PrecoPromocional > 0 {
		price = p.Precos.PrecoPromocional
	}

	var updatedAt time.Time
	if p.DataAlteracao != "" {
		updatedAt, _ = time.Parse("2006-01-02 15:04:05", p.DataAlteracao)
	}

	// Get image URL from attachments
	var imageURL string
	for _, a := range p.Anexos {
		if a.URL != "" {
			imageURL = a.URL
			break
		}
	}

	return &ERPProduct{
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
	}, nil
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
// Tiny API v3 requires idContato (integer) instead of inline customer data.
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
		"itens":       items,
		"observacoes": order.Observation,
		"ecommerce": map[string]any{
			"numeroPedidoEcommerce": order.ExternalID,
			"nomeEcommerce":        "LiveCart",
		},
	}

	resp, body, err := t.DoRequest(ctx, http.MethodPost, endpoint, payload, t.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("creating order: %w", err)
	}

	if !providers.IsSuccessStatus(resp.StatusCode) {
		var errResp struct {
			Mensagem string `json:"mensagem"`
		}
		_ = json.Unmarshal(body, &errResp)
		return nil, fmt.Errorf("create order failed: status %d, message: %s", resp.StatusCode, errResp.Mensagem)
	}

	var orderResp struct {
		ID     int64  `json:"id"`
		Numero string `json:"numeroPedido"`
	}

	if err := json.Unmarshal(body, &orderResp); err != nil {
		return nil, fmt.Errorf("parsing order response: %w", err)
	}

	return &OrderResult{
		OrderID:     strconv.FormatInt(orderResp.ID, 10),
		OrderNumber: orderResp.Numero,
		Status:      "created",
	}, nil
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
		var errResp struct {
			Mensagem string `json:"mensagem"`
		}
		_ = json.Unmarshal(body, &errResp)
		return nil, fmt.Errorf("create contact failed: status %d, message: %s", resp.StatusCode, errResp.Mensagem)
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

// authHeaders returns the authorization headers for API v3 requests.
func (t *Tiny) authHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + t.credentials.AccessToken,
	}
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
