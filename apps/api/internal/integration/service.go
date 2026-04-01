package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/live"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/crypto"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/idempotency"
	"livecart/apps/api/lib/ratelimit"
)

// ProductSyncer syncs products from external ERP systems into the local database.
type ProductSyncer interface {
	HasProduct(ctx context.Context, storeID, externalID, externalSource string) (bool, error)
	GetProduct(ctx context.Context, storeID, productID string) (externalID, externalSource string, err error)
	SyncProduct(ctx context.Context, storeID, externalID, externalSource, name string, price int64, imageURL string, stock int, active bool) error
}

// Service handles business logic for integrations.
type Service struct {
	repo          *Repository
	factory       *providers.Factory
	encryptor     *crypto.Encryptor
	idempotency   *idempotency.Service
	liveService   *live.Service
	productSyncer ProductSyncer
	logger        *zap.Logger
}

// NewService creates a new integration service.
func NewService(
	repo *Repository,
	factory *providers.Factory,
	encryptor *crypto.Encryptor,
	idempotency *idempotency.Service,
	liveService *live.Service,
	logger *zap.Logger,
) *Service {
	return &Service{
		repo:        repo,
		factory:     factory,
		encryptor:   encryptor,
		idempotency: idempotency,
		liveService: liveService,
		logger:      logger,
	}
}

// SetProductSyncer sets the product syncer for webhook processing.
func (s *Service) SetProductSyncer(syncer ProductSyncer) {
	s.productSyncer = syncer
}

// =============================================================================
// INTEGRATION CRUD
// =============================================================================

// Create creates a new integration.
func (s *Service) Create(ctx context.Context, input CreateIntegrationInput) (*CreateIntegrationOutput, error) {
	// Encrypt credentials
	encryptedCreds, err := s.encryptor.EncryptJSON(input.Credentials)
	if err != nil {
		return nil, fmt.Errorf("encrypting credentials: %w", err)
	}

	// Determine token expiration if present
	var tokenExpiresAt *time.Time
	if input.Credentials != nil && !input.Credentials.ExpiresAt.IsZero() {
		tokenExpiresAt = &input.Credentials.ExpiresAt
	}

	row, err := s.repo.Create(ctx, CreateIntegrationParams{
		StoreID:        input.StoreID,
		Type:           input.Type,
		Provider:       input.Provider,
		Status:         "pending_auth",
		Credentials:    encryptedCreds,
		TokenExpiresAt: tokenExpiresAt,
		Metadata:       input.Metadata,
	})
	if err != nil {
		return nil, err
	}

	return s.toCreateOutput(row), nil
}

// GetByID retrieves an integration by ID.
func (s *Service) GetByID(ctx context.Context, id, storeID string) (*CreateIntegrationOutput, error) {
	row, err := s.repo.GetByID(ctx, id, storeID)
	if err != nil {
		return nil, err
	}
	return s.toCreateOutput(row), nil
}

// List lists all integrations for a store.
func (s *Service) List(ctx context.Context, input ListIntegrationsInput) (*ListIntegrationsOutput, error) {
	input.Pagination.Normalize()

	rows, total, err := s.repo.ListByStore(ctx, input.StoreID, input.Pagination)
	if err != nil {
		return nil, err
	}

	result := make([]CreateIntegrationOutput, len(rows))
	for i, row := range rows {
		result[i] = *s.toCreateOutput(&row)
	}

	return &ListIntegrationsOutput{
		Integrations: result,
		Pagination:   input.Pagination,
		Total:        total,
	}, nil
}

// Delete deletes an integration.
func (s *Service) Delete(ctx context.Context, id, storeID string) error {
	return s.repo.Delete(ctx, id, storeID)
}

// UpdateStatus updates an integration's status.
func (s *Service) UpdateStatus(ctx context.Context, id, status string) error {
	return s.repo.UpdateStatus(ctx, id, status)
}

// TestConnection tests if the integration credentials are valid and the provider is reachable.
func (s *Service) TestConnection(ctx context.Context, input TestConnectionInput) (*TestConnectionOutput, error) {
	provider, err := s.GetProvider(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}

	result, err := provider.TestConnection(ctx)
	if err != nil {
		s.handleProviderError(ctx, input.IntegrationID, "test_connection", err)
		return &TestConnectionOutput{
			Success:  false,
			Message:  fmt.Sprintf("Erro ao testar conexão: %v", err),
			TestedAt: time.Now(),
		}, nil
	}

	return &TestConnectionOutput{
		Success:     result.Success,
		Message:     result.Message,
		Latency:     result.Latency,
		AccountInfo: result.AccountInfo,
		TestedAt:    result.TestedAt,
	}, nil
}

// =============================================================================
// OAUTH OPERATIONS
// =============================================================================

// GetOAuthURL generates the OAuth authorization URL for a provider.
func (s *Service) GetOAuthURL(ctx context.Context, input GetOAuthURLInput) (*GetOAuthURLOutput, error) {
	switch input.Provider {
	case "mercado_pago":
		return s.getMercadoPagoOAuthURL(input.StoreID)
	case "tiny":
		return s.getTinyOAuthURL(input.StoreID)
	default:
		return nil, httpx.ErrUnprocessable("unknown provider: " + input.Provider)
	}
}

// getMercadoPagoOAuthURL generates the Mercado Pago OAuth URL.
func (s *Service) getMercadoPagoOAuthURL(storeID string) (*GetOAuthURLOutput, error) {
	appID := config.MercadoPagoAppID.String()
	if appID == "" {
		return nil, httpx.ErrUnprocessable("Mercado Pago app not configured")
	}

	redirectURI := config.WebhookBaseURL.String() + "/api/webhooks/integrations/mercado_pago/oauth/callback"

	// Generate state with store ID for callback
	state := storeID

	authURL := fmt.Sprintf(
		"https://auth.mercadopago.com/authorization?client_id=%s&response_type=code&platform_id=mp&redirect_uri=%s&state=%s",
		appID,
		redirectURI,
		state,
	)

	return &GetOAuthURLOutput{
		AuthURL: authURL,
		State:   state,
	}, nil
}

// getTinyOAuthURL generates the Tiny ERP OAuth URL using stored credentials.
func (s *Service) getTinyOAuthURL(storeID string) (*GetOAuthURLOutput, error) {
	// Find existing integration (active or pending_auth) to get client_id
	existing, err := s.repo.GetByProvider(context.Background(), storeID, "erp", "tiny")
	if err != nil || existing == nil {
		return nil, httpx.ErrUnprocessable("Crie primeiro o aplicativo Tiny e salve as credenciais")
	}

	// Decrypt credentials to get client_id
	creds, err := s.decryptCredentials(existing.Credentials)
	if err != nil {
		return nil, fmt.Errorf("decrypting credentials: %w", err)
	}

	clientID := creds.Extra["client_id"]
	if clientID == nil || clientID == "" {
		return nil, httpx.ErrUnprocessable("Client ID não encontrado nas credenciais")
	}

	redirectURI := config.WebhookBaseURL.String() + "/api/webhooks/integrations/tiny/oauth/callback"

	// Generate state with store ID for callback
	state := storeID

	authURL := fmt.Sprintf(
		"https://accounts.tiny.com.br/realms/tiny/protocol/openid-connect/auth?client_id=%s&redirect_uri=%s&scope=openid&response_type=code&state=%s",
		clientID,
		redirectURI,
		state,
	)

	return &GetOAuthURLOutput{
		AuthURL: authURL,
		State:   state,
	}, nil
}

// HandleOAuthCallback handles the OAuth callback and creates/updates the integration.
func (s *Service) HandleOAuthCallback(ctx context.Context, input OAuthCallbackInput) (*OAuthCallbackOutput, error) {
	switch input.Provider {
	case "mercado_pago":
		return s.handleMercadoPagoCallback(ctx, input)
	case "tiny":
		return s.handleTinyCallback(ctx, input)
	default:
		return nil, httpx.ErrUnprocessable("unknown provider: " + input.Provider)
	}
}

// handleMercadoPagoCallback exchanges the code for tokens and creates the integration.
func (s *Service) handleMercadoPagoCallback(ctx context.Context, input OAuthCallbackInput) (*OAuthCallbackOutput, error) {
	appID := config.MercadoPagoAppID.String()
	appSecret := config.MercadoPagoAppSecret.String()
	redirectURI := config.WebhookBaseURL.String() + "/api/webhooks/integrations/mercado_pago/oauth/callback"

	if appID == "" || appSecret == "" {
		return nil, httpx.ErrUnprocessable("Mercado Pago app not configured")
	}

	// Exchange code for tokens
	tokenURL := "https://api.mercadopago.com/oauth/token"
	payload := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     appID,
		"client_secret": appSecret,
		"code":          input.Code,
		"redirect_uri":  redirectURI,
	}

	payloadBytes, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("creating token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchanging code for token: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		s.logger.Error("OAuth token exchange failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", string(body)),
		)
		return nil, fmt.Errorf("token exchange failed: status %d", resp.StatusCode)
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
		UserID       int64  `json:"user_id"`
		PublicKey    string `json:"public_key"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}

	// State contains the store ID
	storeID := input.State

	// Create credentials
	creds := &providers.Credentials{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		TokenType:    tokenResp.TokenType,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
		Extra: map[string]any{
			"user_id":    tokenResp.UserID,
			"public_key": tokenResp.PublicKey,
		},
	}

	// Encrypt credentials
	encryptedCreds, err := s.encryptor.EncryptJSON(creds)
	if err != nil {
		return nil, fmt.Errorf("encrypting credentials: %w", err)
	}

	tokenExpiresAt := creds.ExpiresAt

	// Check if integration already exists for this store
	existing, _ := s.repo.GetActiveByProvider(ctx, storeID, "payment", "mercado_pago")

	var integrationID string
	if existing != nil {
		// Update existing integration
		err = s.repo.UpdateCredentials(ctx, existing.ID, encryptedCreds, &tokenExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("updating credentials: %w", err)
		}
		err = s.repo.UpdateStatus(ctx, existing.ID, "active")
		if err != nil {
			return nil, fmt.Errorf("updating status: %w", err)
		}
		integrationID = existing.ID
	} else {
		// Create new integration
		row, err := s.repo.Create(ctx, CreateIntegrationParams{
			StoreID:        storeID,
			Type:           "payment",
			Provider:       "mercado_pago",
			Status:         "active",
			Credentials:    encryptedCreds,
			TokenExpiresAt: &tokenExpiresAt,
			Metadata: map[string]any{
				"user_id":      tokenResp.UserID,
				"public_key":   tokenResp.PublicKey,
				"connected_at": time.Now(),
			},
		})
		if err != nil {
			return nil, fmt.Errorf("creating integration: %w", err)
		}
		integrationID = row.ID
	}

	s.logger.Info("Mercado Pago OAuth completed",
		zap.String("store_id", storeID),
		zap.String("integration_id", integrationID),
		zap.Int64("mp_user_id", tokenResp.UserID),
	)

	return &OAuthCallbackOutput{
		IntegrationID: integrationID,
		StoreID:       storeID,
		Provider:      "mercado_pago",
		Status:        "active",
	}, nil
}

// handleTinyCallback exchanges the code for tokens using stored credentials.
func (s *Service) handleTinyCallback(ctx context.Context, input OAuthCallbackInput) (*OAuthCallbackOutput, error) {
	// State contains the store ID
	storeID := input.State

	// Get existing integration with stored client_id/client_secret
	existing, err := s.repo.GetByProvider(ctx, storeID, "erp", "tiny")
	if err != nil || existing == nil {
		return nil, httpx.ErrUnprocessable("Integração Tiny não encontrada. Crie primeiro com client_id e client_secret.")
	}

	// Decrypt stored credentials to get client_id and client_secret
	storedCreds, err := s.decryptCredentials(existing.Credentials)
	if err != nil {
		return nil, fmt.Errorf("decrypting stored credentials: %w", err)
	}

	clientID, _ := storedCreds.Extra["client_id"].(string)
	clientSecret, _ := storedCreds.Extra["client_secret"].(string)

	// Debug logging to see what values we have
	clientIDPrefix := ""
	if len(clientID) > 20 {
		clientIDPrefix = clientID[:20] + "..."
	} else if clientID != "" {
		clientIDPrefix = clientID
	}
	s.logger.Info("Tiny OAuth token exchange - credentials loaded",
		zap.String("store_id", storeID),
		zap.String("integration_id", existing.ID),
		zap.String("client_id_prefix", clientIDPrefix),
		zap.Int("client_id_len", len(clientID)),
		zap.Bool("has_client_secret", clientSecret != ""),
		zap.Int("client_secret_len", len(clientSecret)),
	)

	if clientID == "" || clientSecret == "" {
		return nil, httpx.ErrUnprocessable("Client ID ou Client Secret não encontrado")
	}

	redirectURI := config.WebhookBaseURL.String() + "/api/webhooks/integrations/tiny/oauth/callback"

	// Use url.Values for proper URL encoding
	formData := url.Values{}
	formData.Set("grant_type", "authorization_code")
	formData.Set("client_id", clientID)
	formData.Set("client_secret", clientSecret)
	formData.Set("code", input.Code)
	formData.Set("redirect_uri", redirectURI)

	s.logger.Info("Tiny OAuth token exchange - request params",
		zap.String("redirect_uri", redirectURI),
		zap.Bool("has_code", input.Code != ""),
	)

	tokenURL := "https://accounts.tiny.com.br/realms/tiny/protocol/openid-connect/token"
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return nil, fmt.Errorf("creating token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchanging code for token: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		s.logger.Error("Tiny OAuth token exchange failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", string(body)),
		)
		return nil, fmt.Errorf("token exchange failed: status %d", resp.StatusCode)
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}

	// Create credentials preserving client_id and client_secret
	creds := &providers.Credentials{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		TokenType:    tokenResp.TokenType,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
		Extra: map[string]any{
			"client_id":     clientID,
			"client_secret": clientSecret,
		},
	}

	// Encrypt credentials
	encryptedCreds, err := s.encryptor.EncryptJSON(creds)
	if err != nil {
		return nil, fmt.Errorf("encrypting credentials: %w", err)
	}

	tokenExpiresAt := creds.ExpiresAt

	// Update existing integration with OAuth tokens
	err = s.repo.UpdateCredentials(ctx, existing.ID, encryptedCreds, &tokenExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("updating credentials: %w", err)
	}
	err = s.repo.UpdateStatus(ctx, existing.ID, "active")
	if err != nil {
		return nil, fmt.Errorf("updating status: %w", err)
	}

	s.logger.Info("Tiny OAuth completed",
		zap.String("store_id", storeID),
		zap.String("integration_id", existing.ID),
	)

	return &OAuthCallbackOutput{
		IntegrationID: existing.ID,
		StoreID:       storeID,
		Provider:      "tiny",
		Status:        "active",
	}, nil
}

// =============================================================================
// PROVIDER OPERATIONS
// =============================================================================

// GetProvider returns an initialized provider for the given integration.
func (s *Service) GetProvider(ctx context.Context, integrationID, storeID string) (providers.Provider, error) {
	integration, err := s.repo.GetByID(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}

	return s.createProviderFromRow(ctx, integration)
}

// GetPaymentProvider returns a PaymentProvider for the given integration.
func (s *Service) GetPaymentProvider(ctx context.Context, integrationID, storeID string) (providers.PaymentProvider, error) {
	integration, err := s.repo.GetByID(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}

	if integration.Type != string(providers.ProviderTypePayment) {
		return nil, httpx.ErrUnprocessable("integration is not a payment provider")
	}

	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return nil, err
	}

	paymentProvider, ok := provider.(providers.PaymentProvider)
	if !ok {
		return nil, httpx.ErrUnprocessable("failed to cast to payment provider")
	}

	return paymentProvider, nil
}

// GetERPProvider returns an ERPProvider for the given integration.
func (s *Service) GetERPProvider(ctx context.Context, integrationID, storeID string) (providers.ERPProvider, error) {
	integration, err := s.repo.GetByID(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}

	if integration.Type != string(providers.ProviderTypeERP) {
		return nil, httpx.ErrUnprocessable("integration is not an ERP provider")
	}

	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return nil, err
	}

	erpProvider, ok := provider.(providers.ERPProvider)
	if !ok {
		return nil, httpx.ErrUnprocessable("failed to cast to ERP provider")
	}

	return erpProvider, nil
}

// =============================================================================
// ERP OPERATIONS
// =============================================================================

// SearchProducts searches for products in an ERP integration.
// It lists products, then enriches each with full details (stock, images)
// via GetProduct, and filters to only return active products with stock > 0.
func (s *Service) SearchProducts(ctx context.Context, input SearchProductsInput) (*SearchProductsOutput, error) {
	erpProvider, err := s.GetERPProvider(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}

	pageSize := input.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}

	// Determine search strategy based on input
	params := providers.ListProductsParams{
		PageSize:   pageSize,
		ActiveOnly: true, // Only fetch active products from the API
	}

	if isGTIN(input.Search) {
		params.GTIN = input.Search
	} else {
		params.Search = input.Search
	}

	result, err := erpProvider.ListProducts(ctx, params)
	if err != nil {
		s.handleProviderError(ctx, input.IntegrationID, "search_products", err)
		return nil, fmt.Errorf("searching products: %w", err)
	}

	// If GTIN search returned no results, fallback to name search
	if len(result.Products) == 0 && params.GTIN != "" {
		params.GTIN = ""
		params.Search = input.Search
		result, err = erpProvider.ListProducts(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("searching products by name: %w", err)
		}
	}

	if len(result.Products) == 0 {
		return nil, httpx.ErrNotFound("Produto não encontrado no ERP")
	}

	// Enrich each product with full details (stock, image, description)
	// The list endpoint doesn't return stock or images — GetProduct does.
	var products []ERPProductResponse
	foundButNoStock := false
	for _, listed := range result.Products {
		detailed, err := erpProvider.GetProduct(ctx, listed.ID)
		if err != nil {
			s.logger.Warn("failed to get product details, skipping",
				zap.String("product_id", listed.ID),
				zap.Error(err),
			)
			continue
		}

		if detailed.Stock <= 0 {
			foundButNoStock = true
			continue
		}

		products = append(products, ERPProductResponse{
			ID:          detailed.ID,
			SKU:         detailed.SKU,
			GTIN:        detailed.GTIN,
			Name:        detailed.Name,
			Description: detailed.Description,
			Price:       detailed.Price,
			Stock:       detailed.Stock,
			ImageURL:    detailed.ImageURL,
			Active:      detailed.Active,
		})
	}

	if len(products) == 0 {
		if foundButNoStock {
			return nil, httpx.ErrUnprocessable("Produto encontrado, mas sem estoque disponível no momento")
		}
		return nil, httpx.ErrNotFound("Produto não encontrado no ERP")
	}

	return &SearchProductsOutput{
		Products:   products,
		TotalCount: len(products),
		HasMore:    result.HasMore,
	}, nil
}

// SyncProductManual fetches the latest product data from the ERP and updates the local product.
func (s *Service) SyncProductManual(ctx context.Context, input SyncProductInput) (*SyncProductOutput, error) {
	if s.productSyncer == nil {
		return nil, httpx.ErrUnprocessable("product syncer not configured")
	}

	// Get the product from LiveCart to find its external ID
	externalID, externalSource, err := s.productSyncer.GetProduct(ctx, input.StoreID, input.ProductID)
	if err != nil {
		return nil, err
	}

	if externalID == "" {
		return nil, httpx.ErrUnprocessable("produto não possui ID externo vinculado a um ERP")
	}

	// Verify integration belongs to this store and is an ERP
	erpProvider, err := s.GetERPProvider(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}

	// Verify the integration provider matches the product's external source
	integration, err := s.repo.GetByID(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}
	if integration.Provider != externalSource {
		return nil, httpx.ErrUnprocessable("integração não corresponde à origem do produto")
	}

	// Fetch latest product data from the ERP
	detailed, err := erpProvider.GetProduct(ctx, externalID)
	if err != nil {
		s.handleProviderError(ctx, input.IntegrationID, "manual_sync_product", err)
		return nil, fmt.Errorf("fetching product from ERP: %w", err)
	}

	// Update the local product
	if err := s.productSyncer.SyncProduct(ctx,
		input.StoreID,
		detailed.ID,
		externalSource,
		detailed.Name,
		detailed.Price,
		detailed.ImageURL,
		detailed.Stock,
		detailed.Active,
	); err != nil {
		return nil, fmt.Errorf("syncing product: %w", err)
	}

	s.logger.Info("product synced manually",
		zap.String("integration_id", input.IntegrationID),
		zap.String("product_id", input.ProductID),
		zap.String("external_id", externalID),
		zap.String("store_id", input.StoreID),
	)

	return &SyncProductOutput{
		ProductID:  input.ProductID,
		ExternalID: externalID,
		Name:       detailed.Name,
		Price:      detailed.Price,
		Stock:      detailed.Stock,
		ImageURL:   detailed.ImageURL,
		Active:     detailed.Active,
	}, nil
}

const productWebhookMaxRetries = 3

// ProcessProductWebhook checks if the product exists in LiveCart, then fetches
// full details from the ERP and syncs locally. Ignores unknown products.
// Retries on transient failures to avoid losing sync events.
func (s *Service) ProcessProductWebhook(ctx context.Context, integrationID, externalProductID string) error {
	if s.productSyncer == nil {
		s.logger.Warn("product syncer not configured, skipping product webhook")
		return nil
	}

	integration, err := s.repo.GetByIDOnly(ctx, integrationID)
	if err != nil {
		return fmt.Errorf("getting integration: %w", err)
	}

	// Check if product exists in LiveCart before calling the ERP API
	exists, err := s.productSyncer.HasProduct(ctx, integration.StoreID, externalProductID, integration.Provider)
	if err != nil {
		return fmt.Errorf("checking product existence: %w", err)
	}
	if !exists {
		s.logger.Debug("product not registered in livecart, ignoring webhook",
			zap.String("integration_id", integrationID),
			zap.String("external_product_id", externalProductID),
		)
		return nil
	}

	var lastErr error
	for attempt := 0; attempt <= productWebhookMaxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			s.logger.Warn("retrying product webhook processing",
				zap.String("integration_id", integrationID),
				zap.String("product_id", externalProductID),
				zap.Int("attempt", attempt+1),
				zap.Duration("backoff", backoff),
			)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		lastErr = s.processProductSync(ctx, integration, externalProductID)
		if lastErr == nil {
			return nil
		}
	}

	s.logger.Error("product webhook processing failed after retries",
		zap.String("integration_id", integrationID),
		zap.String("product_id", externalProductID),
		zap.Int("max_retries", productWebhookMaxRetries),
		zap.Error(lastErr),
	)

	return lastErr
}

func (s *Service) processProductSync(ctx context.Context, integration *IntegrationRow, externalProductID string) error {
	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return fmt.Errorf("creating provider: %w", err)
	}

	erpProvider, ok := provider.(providers.ERPProvider)
	if !ok {
		return fmt.Errorf("integration %s is not an ERP provider", integration.ID)
	}

	detailed, err := erpProvider.GetProduct(ctx, externalProductID)
	if err != nil {
		s.handleProviderError(ctx, integration.ID, "webhook_get_product", err)
		return fmt.Errorf("fetching product from ERP: %w", err)
	}

	if err := s.productSyncer.SyncProduct(ctx,
		integration.StoreID,
		detailed.ID,
		integration.Provider,
		detailed.Name,
		detailed.Price,
		detailed.ImageURL,
		detailed.Stock,
		detailed.Active,
	); err != nil {
		return fmt.Errorf("syncing product: %w", err)
	}

	s.logger.Info("product synced from webhook",
		zap.String("integration_id", integration.ID),
		zap.String("external_product_id", externalProductID),
		zap.String("store_id", integration.StoreID),
	)

	return nil
}

// isGTIN checks if a string looks like a GTIN/barcode (8+ digits).
func isGTIN(s string) bool {
	if len(s) < 8 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// =============================================================================
// PAYMENT OPERATIONS
// =============================================================================

// CreateCheckout creates a checkout session with idempotency support.
func (s *Service) CreateCheckout(ctx context.Context, input CreateCheckoutInput) (*CreateCheckoutOutput, error) {
	// Check idempotency
	idemReq := idempotency.CheckRequest{
		IdempotencyKey: input.IdempotencyKey,
		StoreID:        input.StoreID,
		IntegrationID:  input.IntegrationID,
		Operation:      "create_checkout",
		Payload:        input,
	}

	cached, err := s.idempotency.Check(ctx, idemReq)
	if err != nil {
		s.logger.Warn("idempotency check failed", zap.Error(err))
	}
	if cached != nil && cached.Found {
		var output CreateCheckoutOutput
		if err := json.Unmarshal(cached.Response, &output); err == nil {
			s.logger.Debug("returning cached checkout response",
				zap.String("idempotency_key", input.IdempotencyKey),
			)
			return &output, nil
		}
	}

	// Start idempotency tracking
	var idemRecord *idempotency.Record
	if input.IdempotencyKey != "" || s.idempotency != nil {
		idemRecord, err = s.idempotency.Start(ctx, idemReq)
		if err != nil {
			s.logger.Warn("idempotency start failed", zap.Error(err))
		}
	}

	// Get payment provider
	paymentProvider, err := s.GetPaymentProvider(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		if idemRecord != nil {
			_ = s.idempotency.Fail(ctx, idemRecord.ID, err)
		}
		return nil, err
	}

	// Build notify URL
	notifyURL := input.NotifyURL
	if notifyURL == "" {
		baseURL := config.WebhookBaseURL.String()
		if baseURL != "" {
			notifyURL = fmt.Sprintf("%s/api/webhooks/integrations/%s/%s",
				baseURL,
				paymentProvider.Name(),
				input.IntegrationID,
			)
		}
	}

	// Create checkout
	result, err := paymentProvider.CreateCheckout(ctx, providers.CheckoutOrder{
		ExternalID:  input.CartID,
		Items:       input.Items,
		Customer:    input.Customer,
		TotalAmount: input.TotalAmount,
		Currency:    input.Currency,
		NotifyURL:   notifyURL,
		SuccessURL:  input.SuccessURL,
		FailureURL:  input.FailureURL,
		Metadata:    input.Metadata,
	})
	if err != nil {
		s.handleProviderError(ctx, input.IntegrationID, "create_checkout", err)
		if idemRecord != nil {
			_ = s.idempotency.Fail(ctx, idemRecord.ID, err)
		}
		return nil, fmt.Errorf("creating checkout: %w", err)
	}

	output := &CreateCheckoutOutput{
		CheckoutID:  result.CheckoutID,
		CheckoutURL: result.CheckoutURL,
		ExpiresAt:   result.ExpiresAt,
	}

	// Complete idempotency
	if idemRecord != nil {
		_ = s.idempotency.Complete(ctx, idemRecord.ID, output)
	}

	return output, nil
}

// GetPaymentStatus retrieves the status of a payment.
func (s *Service) GetPaymentStatus(ctx context.Context, input GetPaymentStatusInput) (*GetPaymentStatusOutput, error) {
	paymentProvider, err := s.GetPaymentProvider(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}

	status, err := paymentProvider.GetPaymentStatus(ctx, input.PaymentID)
	if err != nil {
		s.handleProviderError(ctx, input.IntegrationID, "get_payment_status", err)
		return nil, fmt.Errorf("getting payment status: %w", err)
	}

	return &GetPaymentStatusOutput{
		PaymentID:     status.PaymentID,
		Status:        string(status.Status),
		Amount:        status.Amount,
		PaidAt:        status.PaidAt,
		RefundedAt:    status.RefundedAt,
		FailureReason: status.FailureReason,
		Metadata:      status.Metadata,
	}, nil
}

// RefundPayment initiates a refund.
func (s *Service) RefundPayment(ctx context.Context, input RefundPaymentInput) (*RefundPaymentOutput, error) {
	paymentProvider, err := s.GetPaymentProvider(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}

	result, err := paymentProvider.RefundPayment(ctx, input.PaymentID, input.Amount)
	if err != nil {
		s.handleProviderError(ctx, input.IntegrationID, "refund_payment", err)
		return nil, fmt.Errorf("refunding payment: %w", err)
	}

	return &RefundPaymentOutput{
		RefundID:  result.RefundID,
		Status:    result.Status,
		Amount:    result.Amount,
		CreatedAt: result.CreatedAt,
	}, nil
}

// =============================================================================
// WEBHOOK OPERATIONS
// =============================================================================

// StoreWebhookEvent stores a webhook event for processing.
func (s *Service) StoreWebhookEvent(ctx context.Context, input StoreWebhookInput) error {
	// Check for duplicate event
	existing, err := s.repo.GetWebhookEventByEventID(ctx, input.IntegrationID, input.EventID)
	if err != nil {
		return err
	}
	if existing != nil {
		s.logger.Debug("duplicate webhook event, skipping",
			zap.String("event_id", input.EventID),
		)
		return nil
	}

	_, err = s.repo.CreateWebhookEvent(ctx, input)
	return err
}

// ProcessPaymentNotification processes a payment webhook notification.
func (s *Service) ProcessPaymentNotification(ctx context.Context, input ProcessPaymentInput) error {
	integration, err := s.repo.GetByIDOnly(ctx, input.IntegrationID)
	if err != nil {
		return err
	}

	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return err
	}

	paymentProvider, ok := provider.(providers.PaymentProvider)
	if !ok {
		return fmt.Errorf("integration is not a payment provider")
	}

	status, err := paymentProvider.GetPaymentStatus(ctx, input.PaymentID)
	if err != nil {
		s.handleProviderError(ctx, input.IntegrationID, "process_payment_notification", err)
		return fmt.Errorf("getting payment status: %w", err)
	}

	s.logger.Info("payment notification processed",
		zap.String("payment_id", input.PaymentID),
		zap.String("status", string(status.Status)),
		zap.String("external_reference", status.ExternalReference),
	)

	// ExternalReference contains the cart ID (set when creating checkout)
	if status.ExternalReference == "" {
		s.logger.Warn("payment notification has no external reference, cannot update cart",
			zap.String("payment_id", input.PaymentID),
		)
		return nil
	}

	// Map payment status to cart payment status
	var cartPaymentStatus string
	switch status.Status {
	case providers.PaymentApproved:
		cartPaymentStatus = "paid"
	case providers.PaymentRejected:
		cartPaymentStatus = "failed"
	case providers.PaymentCancelled:
		cartPaymentStatus = "cancelled"
	case providers.PaymentRefunded:
		cartPaymentStatus = "refunded"
	case providers.PaymentPending, providers.PaymentInProcess:
		cartPaymentStatus = "pending"
	default:
		cartPaymentStatus = "pending"
	}

	// Update cart payment status
	if err := s.repo.UpdateCartPaymentStatus(ctx, status.ExternalReference, cartPaymentStatus, status.PaymentID, status.PaidAt); err != nil {
		s.logger.Error("failed to update cart payment status",
			zap.String("cart_id", status.ExternalReference),
			zap.String("payment_status", cartPaymentStatus),
			zap.Error(err),
		)
		return fmt.Errorf("updating cart payment status: %w", err)
	}

	s.logger.Info("cart payment status updated",
		zap.String("cart_id", status.ExternalReference),
		zap.String("payment_status", cartPaymentStatus),
	)

	return nil
}

// =============================================================================
// INSTAGRAM WEBHOOK OPERATIONS
// =============================================================================

// ProcessInstagramComment processes a live comment from Instagram webhook.
// All comments are saved to DB. Purchase intents trigger stock check → cart or waitlist.
func (s *Service) ProcessInstagramComment(ctx context.Context, input ProcessInstagramCommentInput) error {
	s.logger.Info("processing instagram comment",
		zap.String("account_id", input.AccountID),
		zap.String("media_id", input.MediaID),
		zap.String("comment_id", input.CommentID),
		zap.String("user_id", input.UserID),
		zap.String("username", input.Username),
		zap.String("text", input.Text),
	)

	// Find live session by platform_live_id (media_id)
	session, err := s.liveService.GetSessionByPlatformLiveID(ctx, input.MediaID)
	if err != nil {
		return fmt.Errorf("finding live session: %w", err)
	}
	if session == nil {
		s.logger.Warn("no active live session found for media_id",
			zap.String("media_id", input.MediaID),
		)
		return nil
	}

	// Get the event (which has store_id) from the session
	event, err := s.liveService.GetEventByPlatformLiveID(ctx, input.MediaID)
	if err != nil {
		return fmt.Errorf("finding live event: %w", err)
	}
	if event == nil {
		s.logger.Warn("no active live event found for media_id",
			zap.String("media_id", input.MediaID),
		)
		return nil
	}

	// Increment comment counter on session
	if err := s.repo.IncrementLiveSessionComments(ctx, session.ID); err != nil {
		s.logger.Error("failed to increment comment counter",
			zap.String("session_id", session.ID),
			zap.Error(err),
		)
	}

	// Parse purchase intent
	intent := ParsePurchaseIntent(input.Text)
	hasPurchaseIntent := intent != nil

	// Try to match product by keyword
	var product *ProductRow
	if hasPurchaseIntent {
		product = s.findProductByKeyword(ctx, event.StoreID, input.Text)
	}

	// Determine result for the comment record
	var commentResult string
	var matchedProductID string
	var matchedQuantity int
	if !hasPurchaseIntent {
		commentResult = "no_intent"
	} else if product == nil {
		commentResult = "no_product"
	}
	if product != nil && intent != nil {
		matchedProductID = product.ID
		matchedQuantity = intent.Quantity
	}

	// Save ALL comments to DB
	commentID, err := s.repo.CreateLiveComment(ctx, CreateLiveCommentParams{
		SessionID:         session.ID,
		EventID:           event.ID,
		Platform:          "instagram",
		PlatformCommentID: input.CommentID,
		PlatformUserID:    input.UserID,
		PlatformHandle:    input.Username,
		Text:              input.Text,
		HasPurchaseIntent: hasPurchaseIntent,
		MatchedProductID:  matchedProductID,
		MatchedQuantity:   matchedQuantity,
		Result:            commentResult,
	})
	if err != nil {
		s.logger.Error("failed to save live comment", zap.Error(err))
		// Continue processing even if save fails
	}

	// If no purchase intent or no product match, we're done
	if !hasPurchaseIntent || product == nil {
		return nil
	}

	s.logger.Info("purchase intent detected with product match",
		zap.String("username", input.Username),
		zap.String("product_id", product.ID),
		zap.String("keyword", product.Keyword),
		zap.Int("quantity", intent.Quantity),
		zap.Int("stock", product.Stock),
	)

	// Lazy expiration: process expired carts for this product before checking stock
	s.ProcessExpiredCartsForProduct(ctx, event.ID, product.ID)

	// Try to reserve stock atomically
	stockErr := s.repo.DecrementProductStock(ctx, product.ID, intent.Quantity)
	waitlisted := stockErr != nil

	if waitlisted {
		// Stock unavailable — check if user already on waitlist
		alreadyWaiting, _ := s.repo.GetWaitlistItemByEventUserProduct(ctx, event.ID, input.UserID, product.ID)
		if alreadyWaiting {
			s.logger.Info("user already on waitlist, ignoring duplicate",
				zap.String("username", input.Username),
				zap.String("product_id", product.ID),
			)
			// Update comment result
			if commentID != "" {
				_ = s.repo.UpdateLiveCommentResult(ctx, commentID, true, product.ID, intent.Quantity, "already_waitlisted")
			}
			return nil
		}

		// Add to waitlist
		position, _ := s.repo.GetNextWaitlistPosition(ctx, event.ID, product.ID)
		_, err = s.repo.CreateWaitlistItem(ctx, CreateWaitlistItemParams{
			EventID:        event.ID,
			ProductID:      product.ID,
			PlatformUserID: input.UserID,
			PlatformHandle: input.Username,
			Quantity:       intent.Quantity,
			Position:       position,
		})
		if err != nil {
			s.logger.Error("failed to create waitlist item", zap.Error(err))
		}

		// Update comment result
		if commentID != "" {
			_ = s.repo.UpdateLiveCommentResult(ctx, commentID, true, product.ID, intent.Quantity, "waitlisted")
		}

		s.logger.Info("user added to waitlist (out of stock)",
			zap.String("username", input.Username),
			zap.String("product_id", product.ID),
			zap.Int("position", position),
		)
	}

	// Add product to cart (waitlisted or not)
	result, err := s.liveService.AddToCart(ctx, live.AddToCartInput{
		EventID:        event.ID,
		PlatformUserID: input.UserID,
		PlatformHandle: input.Username,
		ProductID:      product.ID,
		ProductPrice:   product.Price,
		Quantity:       intent.Quantity,
		Waitlisted:     waitlisted,
	})
	if err != nil {
		// If we reserved stock but failed to add to cart, release it
		if !waitlisted {
			_ = s.repo.IncrementProductStock(ctx, product.ID, intent.Quantity)
		}
		return fmt.Errorf("adding to cart: %w", err)
	}

	// Update comment result for successful cart add
	if commentID != "" && !waitlisted {
		_ = s.repo.UpdateLiveCommentResult(ctx, commentID, true, product.ID, intent.Quantity, "added_to_cart")
	}

	// Increment order counter on event only for new carts
	if result.IsNewCart {
		if err := s.repo.IncrementLiveEventOrders(ctx, event.ID); err != nil {
			s.logger.Error("failed to increment order counter",
				zap.String("event_id", event.ID),
				zap.Error(err),
			)
		}
	}

	// Sync cart to ERP (only if we have non-waitlisted items)
	if !waitlisted {
		if syncErr := s.SyncCartToERP(ctx, event.StoreID, result.CartID, event.ID, input.UserID, input.Username); syncErr != nil {
			s.logger.Warn("failed to sync cart to ERP",
				zap.String("cart_id", result.CartID),
				zap.Error(syncErr),
			)
		}
	}

	return nil
}

// findProductByKeyword extracts possible keywords from text and tries to match with products.
func (s *Service) findProductByKeyword(ctx context.Context, storeID, text string) *ProductRow {
	keywords := ExtractPossibleKeywords(text)
	if len(keywords) == 0 {
		return nil
	}

	// Try each possible keyword until we find a match
	for _, keyword := range keywords {
		product, err := s.repo.GetProductByKeyword(ctx, storeID, keyword)
		if err != nil {
			s.logger.Error("failed to lookup product by keyword",
				zap.String("keyword", keyword),
				zap.Error(err),
			)
			continue
		}
		if product != nil {
			return product
		}
	}

	return nil
}

// ProcessInstagramMessage processes a DM from Instagram webhook.
func (s *Service) ProcessInstagramMessage(ctx context.Context, input ProcessInstagramMessageInput) error {
	s.logger.Info("processing instagram message",
		zap.String("account_id", input.AccountID),
		zap.String("sender_id", input.SenderID),
		zap.String("message_id", input.MessageID),
		zap.String("text", input.Text),
	)

	// For now, just log the message
	// Future: Could be used to handle order confirmations, questions, etc.

	return nil
}

// =============================================================================
// CART → ERP SYNC
// =============================================================================

// SyncCartToERP creates (or recreates) an order in the ERP for the given cart.
// Steps:
// 1. Resolve or create ERP contact for the platform user
// 2. If cart already has an external_order_id → cancel old order
// 3. Collect non-waitlisted items
// 4. Create order in ERP
// 5. Launch stock in ERP
// 6. Save external_order_id on cart
func (s *Service) SyncCartToERP(ctx context.Context, storeID, cartID, eventID, platformUserID, platformHandle string) error {
	// Get active ERP integration
	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		s.logger.Debug("no active ERP integration, skipping cart sync",
			zap.String("store_id", storeID),
		)
		return nil // No ERP integration = skip silently
	}

	erpProvider, err := s.getERPProvider(ctx, integration)
	if err != nil {
		return fmt.Errorf("creating ERP provider: %w", err)
	}

	// 1. Resolve contact
	contactID, err := s.resolveERPContact(ctx, erpProvider, integration, storeID, platformUserID, platformHandle)
	if err != nil {
		return fmt.Errorf("resolving ERP contact: %w", err)
	}

	// 2. Cancel existing order if any
	cart, err := s.repo.GetCartByEventAndUser(ctx, eventID, platformUserID)
	if err != nil {
		return fmt.Errorf("getting cart: %w", err)
	}
	if cart != nil && cart.ExternalOrderID != "" {
		if cancelErr := erpProvider.CancelOrder(ctx, cart.ExternalOrderID); cancelErr != nil {
			s.logger.Warn("failed to cancel previous ERP order",
				zap.String("order_id", cart.ExternalOrderID),
				zap.Error(cancelErr),
			)
		}
	}

	// 3. Collect non-waitlisted items
	items, err := s.repo.ListNonWaitlistedCartItems(ctx, cartID)
	if err != nil {
		return fmt.Errorf("listing cart items: %w", err)
	}

	s.logger.Info("cart items for ERP sync",
		zap.String("cart_id", cartID),
		zap.Int("total_items", len(items)),
	)

	// 4. Build ERP order — only include items with a valid external_id (linked to Tiny)
	var erpItems []providers.ERPOrderItem
	var totalAmount int64
	for _, item := range items {
		if item.ProductExternalID == "" {
			s.logger.Warn("skipping cart item without external_id (product not linked to ERP)",
				zap.String("product_id", item.ProductID),
				zap.String("product_name", item.ProductName),
			)
			continue
		}
		s.logger.Debug("including cart item in ERP order",
			zap.String("product_id", item.ProductID),
			zap.String("external_id", item.ProductExternalID),
			zap.String("name", item.ProductName),
			zap.Int("quantity", item.Quantity),
			zap.Int64("unit_price", item.UnitPrice),
		)
		erpItems = append(erpItems, providers.ERPOrderItem{
			ProductID: item.ProductExternalID,
			Name:      item.ProductName,
			Quantity:  item.Quantity,
			UnitPrice: item.UnitPrice,
		})
		totalAmount += item.UnitPrice * int64(item.Quantity)
	}

	if len(erpItems) == 0 {
		// No items with external_id, clear external order
		_ = s.repo.UpdateCartExternalOrderID(ctx, cartID, "")
		return nil
	}

	result, err := erpProvider.CreateOrder(ctx, providers.ERPOrder{
		ExternalID:  cartID,
		ContactID:   contactID,
		Items:       erpItems,
		TotalAmount: totalAmount,
		Observation: fmt.Sprintf("LiveCart - Evento %s - @%s", eventID, platformHandle),
	})
	if err != nil {
		return fmt.Errorf("creating ERP order: %w", err)
	}

	// 5. Launch stock in ERP (decrements ERP inventory).
	// If this fails (e.g., ERP stock is insufficient), the order still exists in the ERP
	// but without stock reservation. This is logged as a warning, not a fatal error.
	if err := erpProvider.LaunchOrderStock(ctx, result.OrderID); err != nil {
		s.logger.Warn("ERP stock launch failed — order created but stock not reserved in ERP. Check product stock in Tiny.",
			zap.String("order_id", result.OrderID),
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
	}

	// 6. Save external order ID
	if err := s.repo.UpdateCartExternalOrderID(ctx, cartID, result.OrderID); err != nil {
		return fmt.Errorf("saving external order ID: %w", err)
	}

	s.logger.Info("cart synced to ERP",
		zap.String("cart_id", cartID),
		zap.String("erp_order_id", result.OrderID),
		zap.Int("items", len(erpItems)),
	)

	return nil
}

// resolveERPContact finds or creates an ERP contact for the platform user.
func (s *Service) resolveERPContact(ctx context.Context, erpProvider providers.ERPProvider, integration *IntegrationRow, storeID, platformUserID, platformHandle string) (string, error) {
	// Check cache first
	cachedID, err := s.repo.GetERPContact(ctx, storeID, integration.ID, platformUserID)
	if err != nil {
		return "", err
	}
	if cachedID != "" {
		return cachedID, nil
	}

	// Search by platform handle in ERP
	results, err := erpProvider.SearchContacts(ctx, providers.SearchContactsParams{
		Name: platformHandle,
	})
	if err == nil && len(results) > 0 {
		// Cache and return the first match
		_ = s.repo.UpsertERPContact(ctx, storeID, integration.ID, platformUserID, platformHandle, results[0].ContactID)
		return results[0].ContactID, nil
	}

	// Create new contact in ERP
	contact, err := erpProvider.CreateContact(ctx, providers.ERPContactInput{
		Name:       platformHandle,
		PersonType: "F",
	})
	if err != nil {
		return "", fmt.Errorf("creating ERP contact: %w", err)
	}

	// Cache
	_ = s.repo.UpsertERPContact(ctx, storeID, integration.ID, platformUserID, platformHandle, contact.ContactID)
	return contact.ContactID, nil
}

// getERPProvider gets the ERP provider from an integration row.
func (s *Service) getERPProvider(ctx context.Context, integration *IntegrationRow) (providers.ERPProvider, error) {
	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return nil, err
	}
	erpProvider, ok := provider.(providers.ERPProvider)
	if !ok {
		return nil, fmt.Errorf("integration %s is not an ERP provider", integration.ID)
	}
	return erpProvider, nil
}

// =============================================================================
// LAZY EXPIRATION & WAITLIST PROCESSING
// =============================================================================

// ProcessExpiredCartsForProduct handles expired carts that contain the given product.
// Called lazily when stock might have freed up (e.g., after a new cart item is added).
func (s *Service) ProcessExpiredCartsForProduct(ctx context.Context, eventID, productID string) {
	carts, err := s.repo.ListExpiredCartsByEventAndProduct(ctx, eventID, productID)
	if err != nil {
		s.logger.Error("failed to list expired carts", zap.Error(err))
		return
	}

	for _, cart := range carts {
		// Mark cart as expired
		if err := s.repo.UpdateCartStatus(ctx, cart.ID, "expired"); err != nil {
			s.logger.Error("failed to expire cart", zap.String("cart_id", cart.ID), zap.Error(err))
			continue
		}

		// Release stock back to product
		if err := s.repo.IncrementProductStock(ctx, productID, 1); err != nil {
			s.logger.Error("failed to release stock", zap.String("product_id", productID), zap.Error(err))
		}

		// Cancel ERP order if exists
		if cart.ExternalOrderID != "" {
			integration, err := s.repo.GetActiveByProvider(ctx, cart.StoreID, "erp", "tiny")
			if err == nil {
				erpProvider, err := s.getERPProvider(ctx, integration)
				if err == nil {
					if cancelErr := erpProvider.CancelOrder(ctx, cart.ExternalOrderID); cancelErr != nil {
						s.logger.Warn("failed to cancel expired cart ERP order",
							zap.String("order_id", cart.ExternalOrderID),
							zap.Error(cancelErr),
						)
					}
				}
			}
		}

		s.logger.Info("expired cart processed",
			zap.String("cart_id", cart.ID),
			zap.String("product_id", productID),
		)
	}
}

// ProcessWaitlistForProduct checks if stock freed up and fulfills the next waitlisted person.
// Called after stock is released (expired cart, cancelled order, etc.).
func (s *Service) ProcessWaitlistForProduct(ctx context.Context, eventID, productID, storeID string) {
	// Get next person in waitlist
	next, err := s.repo.GetFirstWaitingByProduct(ctx, eventID, productID)
	if err != nil {
		s.logger.Error("failed to get waitlist", zap.Error(err))
		return
	}
	if next == nil {
		return // No one waiting
	}

	// Try to reserve stock
	if err := s.repo.DecrementProductStock(ctx, productID, next.Quantity); err != nil {
		// Stock not available yet
		return
	}

	// Get product info for price
	product, err := s.repo.GetProductByKeyword(ctx, storeID, "")
	if err != nil || product == nil {
		// Can't get product — return stock and bail
		_ = s.repo.IncrementProductStock(ctx, productID, next.Quantity)
		return
	}

	// Add to cart (no longer waitlisted)
	_, err = s.liveService.AddToCart(ctx, live.AddToCartInput{
		EventID:        eventID,
		PlatformUserID: next.PlatformUserID,
		PlatformHandle: next.PlatformHandle,
		ProductID:      productID,
		ProductPrice:   product.Price,
		Quantity:       next.Quantity,
		Waitlisted:     false,
	})
	if err != nil {
		// Return stock on failure
		_ = s.repo.IncrementProductStock(ctx, productID, next.Quantity)
		s.logger.Error("failed to add waitlisted item to cart", zap.Error(err))
		return
	}

	// Mark waitlist item as fulfilled
	now := time.Now()
	_ = s.repo.UpdateWaitlistItemStatus(ctx, next.ID, "fulfilled", nil, &now, nil)

	// Sync cart to ERP
	cart, _ := s.repo.GetCartByEventAndUser(ctx, eventID, next.PlatformUserID)
	if cart != nil {
		if syncErr := s.SyncCartToERP(ctx, storeID, cart.ID, eventID, next.PlatformUserID, next.PlatformHandle); syncErr != nil {
			s.logger.Warn("failed to sync waitlist-fulfilled cart to ERP", zap.Error(syncErr))
		}
	}

	s.logger.Info("waitlist fulfilled",
		zap.String("user", next.PlatformHandle),
		zap.String("product_id", productID),
		zap.Int("quantity", next.Quantity),
	)
}

// =============================================================================
// HELPERS
// =============================================================================

func (s *Service) createProviderFromRow(ctx context.Context, integration *IntegrationRow) (providers.Provider, error) {
	// Decrypt credentials
	creds, err := s.decryptCredentials(integration.Credentials)
	if err != nil {
		return nil, fmt.Errorf("decrypting credentials: %w", err)
	}

	// Check if token needs refresh
	if creds.IsExpired() {
		creds, err = s.refreshToken(ctx, integration, creds)
		if err != nil {
			s.logger.Warn("failed to refresh token",
				zap.String("integration_id", integration.ID),
				zap.Error(err),
			)
			// Continue with possibly expired credentials
			// The provider will fail if they're truly invalid
		}
	}

	return s.factory.CreateProvider(providers.ProviderConfig{
		IntegrationID: integration.ID,
		StoreID:       integration.StoreID,
		Type:          providers.ProviderType(integration.Type),
		Name:          providers.ProviderName(integration.Provider),
		Credentials:   creds,
		Metadata:      integration.Metadata,
	})
}

func (s *Service) decryptCredentials(encrypted []byte) (*providers.Credentials, error) {
	if encrypted == nil || len(encrypted) == 0 {
		return nil, httpx.ErrUnprocessable("no credentials found")
	}

	var creds providers.Credentials
	if err := s.encryptor.DecryptJSON(encrypted, &creds); err != nil {
		return nil, fmt.Errorf("decrypting credentials: %w", err)
	}

	return &creds, nil
}

func (s *Service) refreshToken(ctx context.Context, integration *IntegrationRow, creds *providers.Credentials) (*providers.Credentials, error) {
	provider, err := s.factory.CreateProvider(providers.ProviderConfig{
		IntegrationID: integration.ID,
		StoreID:       integration.StoreID,
		Type:          providers.ProviderType(integration.Type),
		Name:          providers.ProviderName(integration.Provider),
		Credentials:   creds,
		Metadata:      integration.Metadata,
	})
	if err != nil {
		return nil, err
	}

	newCreds, err := provider.RefreshToken(ctx)
	if err != nil {
		// Mark integration as error state
		_ = s.repo.UpdateStatus(ctx, integration.ID, "error")
		return nil, fmt.Errorf("refreshing token: %w", err)
	}

	if newCreds == nil {
		// Provider doesn't support token refresh
		return creds, nil
	}

	// Encrypt and save new credentials
	encrypted, err := s.encryptor.EncryptJSON(newCreds)
	if err != nil {
		return nil, fmt.Errorf("encrypting new credentials: %w", err)
	}

	var tokenExpiresAt *time.Time
	if !newCreds.ExpiresAt.IsZero() {
		tokenExpiresAt = &newCreds.ExpiresAt
	}

	if err := s.repo.UpdateCredentials(ctx, integration.ID, encrypted, tokenExpiresAt); err != nil {
		return nil, fmt.Errorf("saving new credentials: %w", err)
	}

	s.logger.Info("token refreshed successfully",
		zap.String("integration_id", integration.ID),
	)

	return newCreds, nil
}

func (s *Service) toCreateOutput(row *IntegrationRow) *CreateIntegrationOutput {
	return &CreateIntegrationOutput{
		ID:           row.ID,
		StoreID:      row.StoreID,
		Type:         row.Type,
		Provider:     row.Provider,
		Status:       row.Status,
		Metadata:     row.Metadata,
		LastSyncedAt: row.LastSyncedAt,
		CreatedAt:    row.CreatedAt,
	}
}

// handleProviderError checks if a provider error is rate-limit related and logs accordingly.
// If the error is an ErrRateLimited, it logs at Error level and marks the integration as 'error'.
func (s *Service) handleProviderError(ctx context.Context, integrationID string, operation string, err error) {
	if err == nil {
		return
	}

	var rateLimitErr *ratelimit.ErrRateLimited
	if errors.As(err, &rateLimitErr) {
		s.logger.Error("provider rate limited",
			zap.String("integration_id", integrationID),
			zap.String("operation", operation),
			zap.Duration("retry_after", rateLimitErr.RetryAfter),
		)

		// Mark integration as error so it's visible in the dashboard
		if updateErr := s.repo.UpdateStatus(ctx, integrationID, "error"); updateErr != nil {
			s.logger.Warn("failed to update integration status after rate limit",
				zap.String("integration_id", integrationID),
				zap.Error(updateErr),
			)
		}
	}
}

// LogIntegrationOperation logs an integration operation to the database.
// This is used by providers via the LogFunc callback.
func (s *Service) LogIntegrationOperation(ctx context.Context, log providers.IntegrationLog) error {
	return s.repo.CreateLog(
		ctx,
		log.IntegrationID,
		log.EntityType,
		log.EntityID,
		log.Direction,
		log.Status,
		log.RequestPayload,
		log.ResponsePayload,
		log.ErrorMessage,
	)
}
