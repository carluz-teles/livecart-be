package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/live"
	"livecart/apps/api/internal/notification"
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
	// SyncProduct updates an existing LiveCart product with the latest ERP data.
	// When product.Shipping is non-nil, dimensions are refreshed too; otherwise
	// the local shipping profile is preserved. skipStock=true keeps local stock
	// (e.g. during an active live event).
	SyncProduct(ctx context.Context, storeID, externalSource string, product providers.ERPProduct, skipStock bool) error
	// ImportProduct creates a new simple product in LiveCart from an ERP source.
	// Returns the new LiveCart product UUID.
	ImportProduct(ctx context.Context, storeID, externalSource string, product providers.ERPProduct) (productID string, err error)
}

// ProductGroupSyncer handles ERP products that ship with variations
// (e.g. Tiny tipo=V). Wired separately from ProductSyncer to avoid pulling the
// productgroup package into the product package.
type ProductGroupSyncer interface {
	SyncFromERP(ctx context.Context, storeID, externalSource string, parent providers.ERPProduct) error
	// ImportFromERP creates a new product_group in LiveCart with the given
	// (already filtered) variants. Returns the new group UUID and the external
	// IDs of the variants that were persisted.
	ImportFromERP(ctx context.Context, storeID, externalSource string, parent providers.ERPProduct) (groupID string, importedExternalIDs []string, err error)
}

// Service handles business logic for integrations.
type Service struct {
	repo                *Repository
	factory             *providers.Factory
	encryptor           *crypto.Encryptor
	idempotency         *idempotency.Service
	liveService         *live.Service
	productSyncer       ProductSyncer
	productGroupSyncer  ProductGroupSyncer
	notificationService *notification.Service
	logger              *zap.Logger
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

// SetProductGroupSyncer wires the syncer used when an imported ERP product has
// variations (Tiny tipo=V, etc.).
func (s *Service) SetProductGroupSyncer(syncer ProductGroupSyncer) {
	s.productGroupSyncer = syncer
}

// SetNotificationService sets the notification service for sending DMs.
func (s *Service) SetNotificationService(svc *notification.Service) {
	s.notificationService = svc
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
	case "instagram":
		return s.getInstagramOAuthURL(input.StoreID)
	case "melhor_envio":
		return s.getMelhorEnvioOAuthURL(input.StoreID)
	default:
		return nil, httpx.ErrUnprocessable("unknown provider: " + input.Provider)
	}
}

// getMercadoPagoOAuthURL generates the Mercado Pago OAuth URL with PKCE.
func (s *Service) getMercadoPagoOAuthURL(storeID string) (*GetOAuthURLOutput, error) {
	appID := config.MercadoPagoAppID.String()
	if appID == "" {
		return nil, httpx.ErrUnprocessable("Mercado Pago app not configured")
	}

	redirectURI := config.WebhookBaseURL.String() + "/api/v1/integrations/oauth/mercado_pago/callback"

	// Generate unique state
	state := uuid.New().String()

	// Generate PKCE code_verifier (43-128 characters, URL-safe)
	codeVerifier := generateCodeVerifier()

	// Generate code_challenge (SHA256 hash of code_verifier, base64url encoded)
	codeChallenge := generateCodeChallenge(codeVerifier)

	// Store state and code_verifier for later retrieval in callback
	ctx := context.Background()
	if err := s.repo.CreateOAuthState(ctx, state, storeID, "mercado_pago", codeVerifier); err != nil {
		return nil, fmt.Errorf("storing OAuth state: %w", err)
	}

	authURL := fmt.Sprintf(
		"https://auth.mercadopago.com/authorization?client_id=%s&response_type=code&platform_id=mp&redirect_uri=%s&state=%s&code_challenge=%s&code_challenge_method=S256",
		appID,
		url.QueryEscape(redirectURI),
		state,
		codeChallenge,
	)

	return &GetOAuthURLOutput{
		AuthURL: authURL,
		State:   state,
	}, nil
}

// generateCodeVerifier generates a random code verifier for PKCE (43-128 chars).
func generateCodeVerifier() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// generateCodeChallenge generates the code challenge from the verifier (S256 method).
func generateCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// getInstagramOAuthURL generates the Instagram Business Login OAuth URL.
func (s *Service) getInstagramOAuthURL(storeID string) (*GetOAuthURLOutput, error) {
	appID := config.InstagramAppID.String()
	if appID == "" {
		return nil, httpx.ErrUnprocessable("Instagram app not configured")
	}

	redirectURI := config.WebhookBaseURL.String() + "/api/v1/integrations/oauth/instagram/callback"

	// Generate unique state
	state := uuid.New().String()

	// Store state for later retrieval in callback
	ctx := context.Background()
	if err := s.repo.CreateOAuthState(ctx, state, storeID, "instagram", ""); err != nil {
		return nil, fmt.Errorf("storing OAuth state: %w", err)
	}

	// Build authorization URL
	// Scopes:
	//   instagram_business_basic            (required)
	//   instagram_business_manage_comments  (live_comments webhooks)
	//   instagram_business_manage_messages  (send DMs after event end)
	authURL := fmt.Sprintf(
		"https://www.instagram.com/oauth/authorize?client_id=%s&redirect_uri=%s&response_type=code&scope=%s&state=%s",
		appID,
		url.QueryEscape(redirectURI),
		url.QueryEscape("instagram_business_basic,instagram_business_manage_comments,instagram_business_manage_messages"),
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

	redirectURI := config.WebhookBaseURL.String() + "/api/v1/integrations/oauth/tiny/callback"

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
	case "instagram":
		return s.handleInstagramCallback(ctx, input)
	case "melhor_envio":
		return s.handleMelhorEnvioCallback(ctx, input)
	default:
		return nil, httpx.ErrUnprocessable("unknown provider: " + input.Provider)
	}
}

// handleMercadoPagoCallback exchanges the code for tokens and creates the integration.
func (s *Service) handleMercadoPagoCallback(ctx context.Context, input OAuthCallbackInput) (*OAuthCallbackOutput, error) {
	appID := config.MercadoPagoAppID.String()
	appSecret := config.MercadoPagoAppSecret.String()
	redirectURI := config.WebhookBaseURL.String() + "/api/v1/integrations/oauth/mercado_pago/callback"

	if appID == "" || appSecret == "" {
		return nil, httpx.ErrUnprocessable("Mercado Pago app not configured")
	}

	// Retrieve OAuth state (includes code_verifier for PKCE)
	oauthState, err := s.repo.GetOAuthState(ctx, input.State)
	if err != nil {
		s.logger.Error("OAuth state not found or expired",
			zap.String("state", input.State),
			zap.Error(err),
		)
		return nil, httpx.ErrUnprocessable("OAuth state expired or invalid")
	}

	// Clean up the state after retrieval
	defer s.repo.DeleteOAuthState(ctx, input.State)

	// Override input.State with actual store_id from database
	input.State = oauthState.StoreID.String()

	// Exchange code for tokens (with PKCE code_verifier)
	tokenURL := "https://api.mercadopago.com/oauth/token"
	payload := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     appID,
		"client_secret": appSecret,
		"code":          input.Code,
		"redirect_uri":  redirectURI,
		"code_verifier": oauthState.CodeVerifier,
	}

	// Add test_token parameter to get TEST credentials instead of production
	if config.MercadoPagoTestMode.String() == "true" {
		payload["test_token"] = "true"
		s.logger.Info("Mercado Pago OAuth: requesting TEST credentials (test_token=true)")
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

	redirectURI := config.WebhookBaseURL.String() + "/api/v1/integrations/oauth/tiny/callback"

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

	// Log token expiration info for debugging
	s.logger.Info("Tiny OAuth token received",
		zap.Int("expires_in", tokenResp.ExpiresIn),
		zap.Bool("has_access_token", tokenResp.AccessToken != ""),
		zap.Bool("has_refresh_token", tokenResp.RefreshToken != ""),
	)

	// Default to 4 hours if expires_in is 0 or not provided
	// Tiny access tokens typically last about 4 hours
	expiresInSeconds := tokenResp.ExpiresIn
	if expiresInSeconds <= 0 {
		s.logger.Warn("Tiny OAuth: expires_in is 0 or negative, defaulting to 4 hours",
			zap.Int("original_expires_in", tokenResp.ExpiresIn),
		)
		expiresInSeconds = 14400 // 4 hours in seconds
	}

	// Create credentials preserving client_id and client_secret
	expiresAt := time.Now().Add(time.Duration(expiresInSeconds) * time.Second)
	creds := &providers.Credentials{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		TokenType:    tokenResp.TokenType,
		ExpiresAt:    expiresAt,
		Extra: map[string]any{
			"client_id":     clientID,
			"client_secret": clientSecret,
		},
	}

	s.logger.Info("Tiny OAuth credentials created",
		zap.Time("expires_at", expiresAt),
		zap.Int("expires_in_seconds_used", expiresInSeconds),
	)

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

// handleInstagramCallback exchanges the code for tokens and creates the integration.
func (s *Service) handleInstagramCallback(ctx context.Context, input OAuthCallbackInput) (*OAuthCallbackOutput, error) {
	appID := config.InstagramAppID.String()
	appSecret := config.InstagramAppSecret.String()
	redirectURI := config.WebhookBaseURL.String() + "/api/v1/integrations/oauth/instagram/callback"

	if appID == "" || appSecret == "" {
		return nil, httpx.ErrUnprocessable("Instagram app not configured")
	}

	// Retrieve OAuth state
	oauthState, err := s.repo.GetOAuthState(ctx, input.State)
	if err != nil {
		s.logger.Error("OAuth state not found or expired",
			zap.String("state", input.State),
			zap.Error(err),
		)
		return nil, httpx.ErrUnprocessable("OAuth state expired or invalid")
	}

	// Clean up the state after retrieval
	defer s.repo.DeleteOAuthState(ctx, input.State)

	storeID := oauthState.StoreID.String()

	// Step 1: Exchange code for short-lived token
	shortLivedToken, instagramUserID, err := s.exchangeInstagramCode(ctx, appID, appSecret, redirectURI, input.Code)
	if err != nil {
		return nil, fmt.Errorf("exchanging code for token: %w", err)
	}

	// Step 2: Exchange short-lived token for long-lived token
	longLivedToken, expiresIn, err := s.exchangeInstagramLongLivedToken(ctx, appSecret, shortLivedToken)
	if err != nil {
		s.logger.Warn("failed to get long-lived token, using short-lived",
			zap.Error(err),
		)
		// Fall back to short-lived token (1 hour)
		longLivedToken = shortLivedToken
		expiresIn = 3600
	}

	// Step 3: Get user profile info (username)
	username, err := s.getInstagramUserProfile(ctx, longLivedToken)
	if err != nil {
		s.logger.Warn("failed to get Instagram username",
			zap.Error(err),
		)
		username = instagramUserID // fallback to user ID
	}

	// Create credentials
	creds := &providers.Credentials{
		AccessToken: longLivedToken,
		TokenType:   "bearer",
		ExpiresAt:   time.Now().Add(time.Duration(expiresIn) * time.Second),
		Extra: map[string]any{
			"instagram_user_id": instagramUserID,
			"username":          username,
		},
	}

	// Encrypt credentials
	encryptedCreds, err := s.encryptor.EncryptJSON(creds)
	if err != nil {
		return nil, fmt.Errorf("encrypting credentials: %w", err)
	}

	tokenExpiresAt := creds.ExpiresAt

	// Check if integration already exists for this store
	existing, _ := s.repo.GetActiveByProvider(ctx, storeID, "social", "instagram")

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
			Type:           "social",
			Provider:       "instagram",
			Status:         "active",
			Credentials:    encryptedCreds,
			TokenExpiresAt: &tokenExpiresAt,
			Metadata: map[string]any{
				"instagram_user_id": instagramUserID,
				"username":          username,
				"connected_at":      time.Now(),
			},
		})
		if err != nil {
			return nil, fmt.Errorf("creating integration: %w", err)
		}
		integrationID = row.ID
	}

	s.logger.Info("Instagram OAuth completed",
		zap.String("store_id", storeID),
		zap.String("integration_id", integrationID),
		zap.String("instagram_user_id", instagramUserID),
		zap.String("username", username),
	)

	return &OAuthCallbackOutput{
		IntegrationID: integrationID,
		StoreID:       storeID,
		Provider:      "instagram",
		Status:        "active",
	}, nil
}

// exchangeInstagramCode exchanges the authorization code for a short-lived access token.
func (s *Service) exchangeInstagramCode(ctx context.Context, appID, appSecret, redirectURI, code string) (string, string, error) {
	tokenURL := "https://api.instagram.com/oauth/access_token"

	// Instagram requires form-urlencoded for this endpoint
	formData := url.Values{}
	formData.Set("client_id", appID)
	formData.Set("client_secret", appSecret)
	formData.Set("grant_type", "authorization_code")
	formData.Set("redirect_uri", redirectURI)
	formData.Set("code", code)

	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return "", "", fmt.Errorf("creating token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("sending token request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		s.logger.Error("Instagram token exchange failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", string(body)),
		)
		return "", "", fmt.Errorf("token exchange failed: status %d", resp.StatusCode)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		UserID      int64  `json:"user_id"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", "", fmt.Errorf("parsing token response: %w", err)
	}

	return tokenResp.AccessToken, fmt.Sprintf("%d", tokenResp.UserID), nil
}

// exchangeInstagramLongLivedToken exchanges a short-lived token for a long-lived token (60 days).
func (s *Service) exchangeInstagramLongLivedToken(ctx context.Context, appSecret, shortLivedToken string) (string, int, error) {
	tokenURL := fmt.Sprintf(
		"https://graph.instagram.com/access_token?grant_type=ig_exchange_token&client_secret=%s&access_token=%s",
		appSecret,
		shortLivedToken,
	)

	req, err := http.NewRequestWithContext(ctx, "GET", tokenURL, nil)
	if err != nil {
		return "", 0, fmt.Errorf("creating long-lived token request: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("sending long-lived token request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		s.logger.Error("Instagram long-lived token exchange failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", string(body)),
		)
		return "", 0, fmt.Errorf("long-lived token exchange failed: status %d", resp.StatusCode)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", 0, fmt.Errorf("parsing long-lived token response: %w", err)
	}

	return tokenResp.AccessToken, tokenResp.ExpiresIn, nil
}

// getInstagramUserProfile fetches the user's Instagram username.
func (s *Service) getInstagramUserProfile(ctx context.Context, accessToken string) (string, error) {
	profileURL := fmt.Sprintf(
		"https://graph.instagram.com/me?fields=user_id,username&access_token=%s",
		accessToken,
	)

	req, err := http.NewRequestWithContext(ctx, "GET", profileURL, nil)
	if err != nil {
		return "", fmt.Errorf("creating profile request: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending profile request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		s.logger.Error("Instagram profile fetch failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", string(body)),
		)
		return "", fmt.Errorf("profile fetch failed: status %d", resp.StatusCode)
	}

	var profileResp struct {
		UserID   string `json:"user_id"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(body, &profileResp); err != nil {
		return "", fmt.Errorf("parsing profile response: %w", err)
	}

	return profileResp.Username, nil
}

// RefreshInstagramToken refreshes a long-lived Instagram token for another 60 days.
func (s *Service) RefreshInstagramToken(ctx context.Context, accessToken string) (string, int, error) {
	refreshURL := fmt.Sprintf(
		"https://graph.instagram.com/refresh_access_token?grant_type=ig_refresh_token&access_token=%s",
		accessToken,
	)

	req, err := http.NewRequestWithContext(ctx, "GET", refreshURL, nil)
	if err != nil {
		return "", 0, fmt.Errorf("creating refresh request: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("sending refresh request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		s.logger.Error("Instagram token refresh failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", string(body)),
		)
		return "", 0, fmt.Errorf("token refresh failed: status %d", resp.StatusCode)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", 0, fmt.Errorf("parsing refresh response: %w", err)
	}

	return tokenResp.AccessToken, tokenResp.ExpiresIn, nil
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

// GetShippingProvider returns the ShippingProvider for the store's active
// shipping integration. Returns httpx.ErrNotFound when no shipping integration
// is configured for the store.
//
// Deprecated: prefer GetShippingProviders (plural) when quoting — stores may
// have more than one active shipping integration and the checkout should show
// options from all of them. This method is kept for callers that genuinely
// need a single provider by ID.
func (s *Service) GetShippingProvider(ctx context.Context, storeID string) (providers.ShippingProvider, error) {
	all, err := s.GetShippingProviders(ctx, storeID)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, httpx.ErrNotFound("no active shipping integration for this store")
	}
	return all[0], nil
}

// GetShippingProviders returns every active shipping integration for the
// store, fully initialized. Returns an empty slice (not an error) when the
// store has no shipping integration configured — callers decide how to treat
// that (checkout will surface an UnprocessableEntity to the customer).
func (s *Service) GetShippingProviders(ctx context.Context, storeID string) ([]providers.ShippingProvider, error) {
	rows, err := s.repo.ListByType(ctx, storeID, string(providers.ProviderTypeShipping))
	if err != nil {
		return nil, err
	}
	out := make([]providers.ShippingProvider, 0, len(rows))
	for i := range rows {
		if rows[i].Status != "active" {
			continue
		}
		provider, err := s.createProviderFromRow(ctx, &rows[i])
		if err != nil {
			s.logger.Warn("failed to instantiate shipping provider — skipping",
				zap.String("integration_id", rows[i].ID),
				zap.String("provider", rows[i].Provider),
				zap.Error(err),
			)
			continue
		}
		sp, ok := provider.(providers.ShippingProvider)
		if !ok {
			s.logger.Warn("integration is marked as shipping but does not implement ShippingProvider",
				zap.String("integration_id", rows[i].ID),
				zap.String("provider", rows[i].Provider),
			)
			continue
		}
		out = append(out, sp)
	}
	return out, nil
}

// GetShippingProviderByName returns the ShippingProvider of a specific
// integration (store + provider name). Returns httpx.ErrNotFound when the
// integration is absent, httpx.ErrUnprocessable when it exists but is not
// active.
func (s *Service) GetShippingProviderByName(ctx context.Context, storeID string, providerName providers.ProviderName) (providers.ShippingProvider, error) {
	integration, err := s.repo.GetByProvider(ctx, storeID, string(providers.ProviderTypeShipping), string(providerName))
	if err != nil {
		return nil, err
	}
	if integration.Status != "active" {
		return nil, httpx.ErrUnprocessable(fmt.Sprintf("%s integration is not active (status=%s)", providerName, integration.Status))
	}
	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return nil, err
	}
	sp, ok := provider.(providers.ShippingProvider)
	if !ok {
		return nil, httpx.ErrUnprocessable("failed to cast to shipping provider")
	}
	return sp, nil
}

// GetShippingOrderProvider returns the ShippingOrderProvider for a specific
// integration (identified by store + provider name). Returns
// providers.ErrOperationNotSupported when the provider does not support the
// order-lifecycle operations (quote-only aggregators).
func (s *Service) GetShippingOrderProvider(ctx context.Context, storeID string, providerName providers.ProviderName) (providers.ShippingOrderProvider, error) {
	sp, err := s.GetShippingProviderByName(ctx, storeID, providerName)
	if err != nil {
		return nil, err
	}
	osp, ok := sp.(providers.ShippingOrderProvider)
	if !ok {
		return nil, providers.ErrOperationNotSupported
	}
	return osp, nil
}

// ConnectSmartEnviosInput is the admin payload to set up or rotate the
// SmartEnvios integration for a store.
type ConnectSmartEnviosInput struct {
	StoreID string
	Token   string
	Env     string // "sandbox" | "production" — defaults to "production"
}

// ConnectSmartEnviosOutput mirrors CreateIntegrationOutput for API responses.
type ConnectSmartEnviosOutput = CreateIntegrationOutput

// ConnectSmartEnvios validates a SmartEnvios token via a live call to
// /quote/services and persists (or updates) the integration as active. No
// OAuth involved — token is static and provided by the merchant.
func (s *Service) ConnectSmartEnvios(ctx context.Context, input ConnectSmartEnviosInput) (*ConnectSmartEnviosOutput, error) {
	token := strings.TrimSpace(input.Token)
	if token == "" {
		return nil, httpx.ErrBadRequest("token is required")
	}
	env := input.Env
	if env == "" {
		env = "production"
	}
	if env != "production" && env != "sandbox" {
		return nil, httpx.ErrBadRequest("env must be 'sandbox' or 'production'")
	}

	// Validate the token with a real call so we never persist garbage.
	creds := &providers.Credentials{AccessToken: token}
	probe, err := s.factory.CreateShippingProvider(providers.ProviderConfig{
		IntegrationID: "probe",
		StoreID:       input.StoreID,
		Type:          providers.ProviderTypeShipping,
		Name:          providers.ProviderSmartEnvios,
		Credentials:   creds,
		Metadata:      map[string]any{"environment": env},
	})
	if err != nil {
		return nil, fmt.Errorf("instantiating smartenvios provider: %w", err)
	}
	// TestConnection follows the shared provider convention: it returns
	// (result, nil) on both success AND failure — failures are surfaced via
	// result.Success == false. Checking only `err != nil` silently accepts
	// bad tokens and stores the integration as active. Check both.
	probeResult, err := probe.TestConnection(ctx)
	if err != nil {
		return nil, httpx.ErrUnprocessable("falha ao validar token SmartEnvios: " + err.Error())
	}
	if probeResult == nil || !probeResult.Success {
		msg := "token rejeitado pela SmartEnvios"
		if probeResult != nil && probeResult.Message != "" {
			msg = probeResult.Message
		}
		return nil, httpx.ErrUnprocessable("falha ao validar token SmartEnvios: " + msg)
	}

	encrypted, err := s.encryptor.EncryptJSON(creds)
	if err != nil {
		return nil, fmt.Errorf("encrypting smartenvios token: %w", err)
	}

	// If an integration already exists, update it; otherwise create it.
	// The repository surfaces "not found" as httpx-wrapped errors whose kind
	// is opaque here — we treat any error that mentions "not found" as
	// "integration is missing, go create it".
	existing, err := s.repo.GetByProvider(ctx, input.StoreID, string(providers.ProviderTypeShipping), string(providers.ProviderSmartEnvios))
	if err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "not found") {
			return nil, err
		}
		existing = nil
	}

	// Build metadata the admin UI can use to render "Informações da Conta".
	// SmartEnvios has no /me endpoint, so we surface what we can observe:
	// environment + list of enabled carrier services. The `accountName` field
	// mirrors the shape used by other providers so the UI doesn't need a
	// provider-specific branch to pick something to display.
	metadata := map[string]any{"environment": env}
	if probeResult != nil && probeResult.AccountInfo != nil {
		if names, ok := probeResult.AccountInfo["service_names"].([]string); ok && len(names) > 0 {
			metadata["accountName"] = fmt.Sprintf("%d serviços habilitados", len(names))
			metadata["enabledServices"] = names
		}
	}
	if existing != nil {
		if err := s.repo.UpdateCredentials(ctx, existing.ID, encrypted, nil); err != nil {
			return nil, err
		}
		if err := s.repo.UpdateMetadata(ctx, existing.ID, metadata); err != nil {
			return nil, err
		}
		if err := s.repo.UpdateStatus(ctx, existing.ID, "active"); err != nil {
			return nil, err
		}
		row, err := s.repo.GetByID(ctx, existing.ID, input.StoreID)
		if err != nil {
			return nil, err
		}
		return s.toCreateOutput(row), nil
	}
	row, err := s.repo.Create(ctx, CreateIntegrationParams{
		StoreID:     input.StoreID,
		Type:        string(providers.ProviderTypeShipping),
		Provider:    string(providers.ProviderSmartEnvios),
		Status:      "active",
		Credentials: encrypted,
		Metadata:    metadata,
	})
	if err != nil {
		return nil, err
	}
	return s.toCreateOutput(row), nil
}

// GetSocialProvider returns a SocialProvider for the given integration.
func (s *Service) GetSocialProvider(ctx context.Context, integrationID, storeID string) (providers.SocialProvider, error) {
	integration, err := s.repo.GetByID(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}

	if integration.Type != string(providers.ProviderTypeSocial) {
		return nil, httpx.ErrUnprocessable("integration is not a social provider")
	}

	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return nil, err
	}

	socialProvider, ok := provider.(providers.SocialProvider)
	if !ok {
		return nil, httpx.ErrUnprocessable("failed to cast to social provider")
	}

	return socialProvider, nil
}

// SendInstagramDM resolves the active Instagram integration of a store and sends a DM
// to the given platform user. Best-effort: callers should treat errors as non-fatal.
func (s *Service) SendInstagramDM(ctx context.Context, storeID, recipientID, text string) error {
	// GetByProvider returns httpx.ErrNotFound when there is no integration —
	// no need for a separate nil check.
	integration, err := s.repo.GetByProvider(ctx, storeID, "social", "instagram")
	if err != nil {
		return fmt.Errorf("instagram integration unavailable: %w", err)
	}
	if integration.Status != "active" {
		return fmt.Errorf("instagram integration is not active (status=%s)", integration.Status)
	}

	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return fmt.Errorf("instantiating instagram provider: %w", err)
	}

	socialProvider, ok := provider.(providers.SocialProvider)
	if !ok {
		return fmt.Errorf("provider is not a social provider")
	}

	if err := socialProvider.SendDirectMessage(ctx, recipientID, text); err != nil {
		s.logger.Warn("failed to send instagram dm",
			zap.String("store_id", storeID),
			zap.String("recipient_id", recipientID),
			zap.Error(err),
		)
		return err
	}

	s.logger.Info("instagram dm sent",
		zap.String("store_id", storeID),
		zap.String("recipient_id", recipientID),
	)
	return nil
}

// ReplyToInstagramComment resolves the active Instagram integration of a store and sends
// a private DM to the user who made the comment using Instagram's Private Reply feature.
// This sends a DM in response to a comment (not a public reply).
func (s *Service) ReplyToInstagramComment(ctx context.Context, storeID, commentID, text string) error {
	integration, err := s.repo.GetByProvider(ctx, storeID, "social", "instagram")
	if err != nil {
		return fmt.Errorf("instagram integration unavailable: %w", err)
	}
	if integration.Status != "active" {
		return fmt.Errorf("instagram integration is not active (status=%s)", integration.Status)
	}

	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return fmt.Errorf("instantiating instagram provider: %w", err)
	}

	socialProvider, ok := provider.(providers.SocialProvider)
	if !ok {
		return fmt.Errorf("provider is not a social provider")
	}

	// Use Private Reply to send a DM in response to the comment
	if err := socialProvider.SendPrivateReply(ctx, commentID, text); err != nil {
		s.logger.Warn("failed to send private reply to instagram comment",
			zap.String("store_id", storeID),
			zap.String("comment_id", commentID),
			zap.Error(err),
		)
		return err
	}

	s.logger.Info("instagram private reply sent",
		zap.String("store_id", storeID),
		zap.String("comment_id", commentID),
	)
	return nil
}

// FetchInstagramLives retrieves all active Instagram lives for a store.
// Returns an empty slice if no lives are currently streaming.
func (s *Service) FetchInstagramLives(ctx context.Context, storeID string) ([]providers.LiveMedia, error) {
	integration, err := s.repo.GetByProvider(ctx, storeID, "social", "instagram")
	if err != nil {
		return nil, fmt.Errorf("instagram integration unavailable: %w", err)
	}
	if integration.Status != "active" {
		return nil, fmt.Errorf("instagram integration is not active (status=%s)", integration.Status)
	}

	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return nil, fmt.Errorf("instantiating instagram provider: %w", err)
	}

	socialProvider, ok := provider.(providers.SocialProvider)
	if !ok {
		return nil, fmt.Errorf("provider is not a social provider")
	}

	lives, err := socialProvider.GetActiveLives(ctx)
	if err != nil {
		s.logger.Warn("failed to fetch instagram lives",
			zap.String("store_id", storeID),
			zap.Error(err),
		)
		return nil, err
	}

	s.logger.Info("fetched instagram lives",
		zap.String("store_id", storeID),
		zap.Int("count", len(lives)),
	)
	return lives, nil
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

	baseParams := providers.ListProductsParams{
		PageSize:   pageSize,
		ActiveOnly: true,
	}

	type searchResult struct {
		field    string
		products []providers.ERPProduct
		err      error
	}

	type searchJob struct {
		field  string
		params providers.ListProductsParams
	}

	jobs := []searchJob{
		{"name", func() providers.ListProductsParams { p := baseParams; p.Search = input.Search; return p }()},
		{"sku", func() providers.ListProductsParams { p := baseParams; p.SKU = input.Search; return p }()},
	}
	if isGTIN(input.Search) {
		p := baseParams
		p.GTIN = input.Search
		jobs = append(jobs, searchJob{"gtin", p})
	}

	results := make([]searchResult, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, field string, params providers.ListProductsParams) {
			defer wg.Done()
			r, err := erpProvider.ListProducts(ctx, params)
			if err != nil {
				results[i] = searchResult{field: field, err: err}
				return
			}
			results[i] = searchResult{field: field, products: r.Products}
		}(i, j.field, j.params)
	}
	wg.Wait()

	merged := make([]providers.ERPProduct, 0)
	seen := make(map[string]struct{})
	allErrored := true
	var firstErr error
	priority := []string{"gtin", "sku", "name"}
	for _, prio := range priority {
		if len(merged) >= pageSize {
			break
		}
		for _, r := range results {
			if r.field != prio {
				continue
			}
			if r.err != nil {
				if firstErr == nil {
					firstErr = r.err
				}
				s.logger.Warn("ERP product search partial failure",
					zap.String("field", r.field),
					zap.String("integration_id", input.IntegrationID),
					zap.Error(r.err),
				)
				continue
			}
			allErrored = false
			for _, p := range r.products {
				if _, ok := seen[p.ID]; ok {
					continue
				}
				seen[p.ID] = struct{}{}
				merged = append(merged, p)
				if len(merged) >= pageSize {
					break
				}
			}
		}
	}

	if allErrored {
		s.handleProviderError(ctx, input.IntegrationID, "search_products", firstErr)
		return nil, fmt.Errorf("searching products: %w", firstErr)
	}

	if len(merged) == 0 {
		return nil, httpx.ErrNotFound("Produto não encontrado no ERP")
	}

	result := &providers.ProductListResult{
		Products: merged,
		HasMore:  false,
	}

	// Enrich each product with full details (stock, image, description)
	// The list endpoint doesn't return stock or images — GetProduct does.
	//
	// Parents with variations (Tiny tipo=V) carry no stock themselves; the real
	// stock is on each child. We aggregate it for the parent so the
	// "out of stock" filter doesn't accidentally hide products that have
	// inventory on at least one variation, and we surface the variations so the
	// front-end can let the user pick a SKU.
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

		isParent := detailed.IsParent && len(detailed.Variants) > 0
		effectiveStock := detailed.Stock
		var variantsResp []ERPVariantResponse
		if isParent {
			// Tiny doesn't include imageUrl, dimensoes or flat dimensions inside
			// variacoes[] of a parent response — fetch each child individually.
			s.enrichVariantsFromIndividualGets(ctx, erpProvider, detailed)

			effectiveStock = 0
			variantsResp = make([]ERPVariantResponse, len(detailed.Variants))
			for i, v := range detailed.Variants {
				effectiveStock += v.Stock
				variantsResp[i] = ERPVariantResponse{
					ID:         v.ID,
					SKU:        v.SKU,
					GTIN:       v.GTIN,
					Name:       v.Name,
					Price:      v.Price,
					Stock:      v.Stock,
					Active:     v.Active,
					ImageURL:   v.ImageURL,
					Shipping:   shippingPreviewFromERP(v.Shipping, v.WeightGramsHint),
					Attributes: v.Attributes,
				}
			}
		}

		if effectiveStock <= 0 {
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
			Stock:       effectiveStock,
			ImageURL:    detailed.ImageURL,
			Active:      detailed.Active,
			Shipping:    shippingPreviewFromERP(detailed.Shipping, detailed.WeightGramsHint),
			IsParent:    isParent,
			Variants:    variantsResp,
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

// inheritShippingFromParent fills detailed.Shipping with the parent product's
// shipping profile when the ERP returned a variation child without its own
// dimensions. Common Tiny setup: merchant fills `dimensoes` only on the parent
// and every variation reuses the same packaging — without this, syncing a
// variation would leave it un-shippable.
//
// No-op when the product is not a variation, already has its own shipping,
// or when fetching the parent fails (best-effort).
func (s *Service) inheritShippingFromParent(ctx context.Context, erpProvider providers.ERPProvider, detailed *providers.ERPProduct) {
	if detailed == nil {
		return
	}
	if detailed.Shipping != nil {
		s.logger.Debug("variant already has its own shipping, no parent lookup needed",
			zap.String("tiny_id", detailed.ID))
		return
	}
	if detailed.ParentExternalID == "" {
		s.logger.Debug("product has no parent, cannot inherit shipping",
			zap.String("tiny_id", detailed.ID),
			zap.Bool("is_parent", detailed.IsParent))
		return
	}
	parent, err := erpProvider.GetProduct(ctx, detailed.ParentExternalID)
	if err != nil {
		s.logger.Warn("failed to fetch parent for shipping inheritance",
			zap.String("variant_id", detailed.ID),
			zap.String("parent_id", detailed.ParentExternalID),
			zap.Error(err))
		return
	}
	if parent == nil || parent.Shipping == nil {
		s.logger.Info("parent has no shipping either — variation will land without dimensions",
			zap.String("variant_id", detailed.ID),
			zap.String("parent_id", detailed.ParentExternalID),
			zap.Bool("parent_returned", parent != nil))
		return
	}
	detailed.Shipping = parent.Shipping
	s.logger.Info("inherited shipping from parent",
		zap.String("variant_id", detailed.ID),
		zap.String("parent_id", detailed.ParentExternalID))
}

// enrichVariantsFromIndividualGets fetches GET /produtos/{idVariacao} for each
// variation in parallel and merges the response back. Tiny's
// VariacaoProdutoResponseModel inside the parent's `variacoes[]` does NOT carry
// imageUrl, dimensoes or per-variant flat dimensions — those only exist on the
// individual GET. Without this hop we'd discard per-variant shipping that the
// merchant actually cadastrou no ERP.
//
// Bounded concurrency keeps us under the per-account rate limit (60 req/min on
// Tiny basic). Failures are silent — variant keeps whatever it had.
func (s *Service) enrichVariantsFromIndividualGets(ctx context.Context, erpProvider providers.ERPProvider, parent *providers.ERPProduct) {
	if parent == nil || len(parent.Variants) == 0 {
		return
	}
	const enrichConcurrency = 5
	sem := make(chan struct{}, enrichConcurrency)
	var wg sync.WaitGroup
	for i := range parent.Variants {
		idx := i
		childID := parent.Variants[idx].ID
		if childID == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			child, err := erpProvider.GetProduct(ctx, childID)
			if err != nil || child == nil {
				return
			}
			if child.ImageURL != "" {
				parent.Variants[idx].ImageURL = child.ImageURL
			}
			if child.Shipping != nil {
				parent.Variants[idx].Shipping = child.Shipping
			}
			if child.WeightGramsHint > 0 {
				parent.Variants[idx].WeightGramsHint = child.WeightGramsHint
			}
		}()
	}
	wg.Wait()
}

// applyStoreDefaultDimensions completes detailed.Shipping (and each variant's)
// using the merchant-configured store defaults when the ERP returned weight
// without dimensions, or vice-versa. No-op when the store has no defaults
// configured or the product is already shippable.
//
// Precedence (per product/variant):
//  1. Use what the ERP gave us as-is when complete.
//  2. Combine ERP weight (WeightGramsHint) with store default H/W/L when ERP
//     returned weight only.
//  3. When ERP returned no weight AND store has both default weight and H/W/L,
//     build a fully synthetic profile.
//  4. Otherwise leave Shipping nil (current behavior — merchant edits later).
func (s *Service) applyStoreDefaultDimensions(ctx context.Context, storeID string, detailed *providers.ERPProduct) {
	if detailed == nil {
		return
	}
	defaults, err := s.repo.GetStoreShippingDefaults(ctx, storeID)
	if err != nil {
		s.logger.Warn("failed to load store shipping defaults",
			zap.String("store_id", storeID), zap.Error(err))
		return
	}
	if !defaults.IsUsableForDimensionFallback() {
		return
	}

	completeFromDefaults := func(p *providers.ERPProduct) {
		if p.Shipping != nil {
			return
		}
		weight := p.WeightGramsHint
		if weight <= 0 {
			weight = defaults.WeightGrams
		}
		if weight <= 0 {
			return
		}
		format := defaults.PackageFormat
		if format == "" {
			format = "box"
		}
		p.Shipping = &providers.ERPShippingProfile{
			WeightGrams:   weight,
			HeightCm:      defaults.HeightCm,
			WidthCm:       defaults.WidthCm,
			LengthCm:      defaults.LengthCm,
			PackageFormat: format,
		}
		s.logger.Info("completed shipping with store defaults",
			zap.String("erp_id", p.ID),
			zap.Int("weight_g", weight),
			zap.Int("h_cm", defaults.HeightCm),
			zap.Int("w_cm", defaults.WidthCm),
			zap.Int("l_cm", defaults.LengthCm))
	}

	completeFromDefaults(detailed)
	for i := range detailed.Variants {
		completeFromDefaults(&detailed.Variants[i])
	}
}

// ImportERPProduct imports a product from the ERP into the LiveCart catalog.
// For products with variations, it creates a product_group + N variants in one
// transaction (filtered by VariantIDs when present). For simple products, it
// creates a single product.
func (s *Service) ImportERPProduct(ctx context.Context, input ImportERPProductInput) (*ImportERPProductOutput, error) {
	if s.productSyncer == nil {
		return nil, httpx.ErrUnprocessable("product syncer not configured")
	}

	erpProvider, err := s.GetERPProvider(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}
	integration, err := s.repo.GetByID(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}

	detailed, err := erpProvider.GetProduct(ctx, input.TinyProductID)
	if err != nil {
		s.handleProviderError(ctx, input.IntegrationID, "import_get_product", err)
		return nil, fmt.Errorf("fetching product from ERP: %w", err)
	}

	// === Simple product (no variations) ===
	if !detailed.IsParent || len(detailed.Variants) == 0 {
		if len(input.VariantIDs) > 0 {
			return nil, httpx.ErrUnprocessable("variantIds informado mas o produto não possui variações")
		}
		exists, err := s.productSyncer.HasProduct(ctx, input.StoreID, detailed.ID, integration.Provider)
		if err != nil {
			return nil, fmt.Errorf("checking product existence: %w", err)
		}
		if exists {
			return nil, httpx.ErrConflict("produto já importado neste catálogo")
		}
		// Same shipping completion chain we run for variants: parent inheritance
		// (no-op for simples) and store-default fallback. Without these the
		// product would land with an empty shipping profile when Tiny returns
		// only weight / partial dimensions.
		s.inheritShippingFromParent(ctx, erpProvider, detailed)
		s.applyStoreDefaultDimensions(ctx, input.StoreID, detailed)
		productID, err := s.productSyncer.ImportProduct(ctx, input.StoreID, integration.Provider, *detailed)
		if err != nil {
			return nil, fmt.Errorf("importing simple product: %w", err)
		}
		return &ImportERPProductOutput{
			ProductID: productID,
			IsParent:  false,
			Imported: []ImportedERPVariantSummary{{
				ExternalID: detailed.ID,
				SKU:        detailed.SKU,
			}},
		}, nil
	}

	// === Parent with variations ===
	if s.productGroupSyncer == nil {
		return nil, httpx.ErrUnprocessable("product group syncer not configured")
	}

	// Filter variants if a subset was requested.
	if len(input.VariantIDs) > 0 {
		want := make(map[string]struct{}, len(input.VariantIDs))
		for _, id := range input.VariantIDs {
			want[id] = struct{}{}
		}
		filtered := make([]providers.ERPProduct, 0, len(input.VariantIDs))
		for _, v := range detailed.Variants {
			if _, ok := want[v.ID]; ok {
				filtered = append(filtered, v)
			}
		}
		if len(filtered) == 0 {
			return nil, httpx.ErrUnprocessable("nenhuma das variantIds informadas existe no produto Tiny")
		}
		if len(filtered) != len(input.VariantIDs) {
			return nil, httpx.ErrUnprocessable("uma ou mais variantIds informadas não existem no produto Tiny")
		}
		detailed.Variants = filtered
	}

	// Tiny doesn't include imageUrl, dimensoes or flat dimensions inside
	// variacoes[] of a parent response — fetch each child individually so we
	// pick up per-variant images AND per-variant shipping the merchant
	// cadastrou no ERP.
	s.enrichVariantsFromIndividualGets(ctx, erpProvider, detailed)

	// Fall back to merchant-configured store defaults for any variant whose
	// shipping is still incomplete after the Tiny payload + parent inheritance.
	s.applyStoreDefaultDimensions(ctx, input.StoreID, detailed)

	groupID, importedIDs, err := s.productGroupSyncer.ImportFromERP(ctx, input.StoreID, integration.Provider, *detailed)
	if err != nil {
		return nil, fmt.Errorf("importing product group: %w", err)
	}

	imported := make([]ImportedERPVariantSummary, 0, len(importedIDs))
	for _, extID := range importedIDs {
		for _, v := range detailed.Variants {
			if v.ID == extID {
				imported = append(imported, ImportedERPVariantSummary{
					ExternalID: v.ID,
					SKU:        v.SKU,
					Attributes: v.Attributes,
				})
				break
			}
		}
	}

	s.logger.Info("ERP product imported into catalog",
		zap.String("integration_id", input.IntegrationID),
		zap.String("tiny_product_id", input.TinyProductID),
		zap.String("group_id", groupID),
		zap.Int("variants_imported", len(imported)),
	)

	return &ImportERPProductOutput{
		GroupID:  groupID,
		IsParent: true,
		Imported: imported,
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

	// If this is a variation child without its own dimensions, inherit from
	// the parent — common in Tiny when the merchant only filled the parent's
	// dimensoes and let every variation use the same packaging.
	s.inheritShippingFromParent(ctx, erpProvider, detailed)
	// Last resort: complete with merchant-configured store defaults when ERP
	// only carries weight (or nothing).
	s.applyStoreDefaultDimensions(ctx, input.StoreID, detailed)

	// Update the local product. Manual sync always refreshes stock and pulls
	// dimensions if the ERP returned them (detailed.Shipping non-nil).
	if err := s.productSyncer.SyncProduct(ctx, input.StoreID, externalSource, *detailed, false); err != nil {
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
func (s *Service) ProcessProductWebhook(ctx context.Context, storeID, provider, externalProductID string) error {
	if s.productSyncer == nil {
		s.logger.Warn("product syncer not configured, skipping product webhook")
		return nil
	}

	// Resolve integration from store_id + provider
	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", provider)
	if err != nil {
		return fmt.Errorf("no active ERP integration found for store %s provider %s: %w", storeID, provider, err)
	}

	// Check if product exists in LiveCart before calling the ERP API
	exists, err := s.productSyncer.HasProduct(ctx, integration.StoreID, externalProductID, integration.Provider)
	if err != nil {
		return fmt.Errorf("checking product existence: %w", err)
	}
	if !exists {
		s.logger.Debug("product not registered in livecart, ignoring webhook",
			zap.String("store_id", storeID),
			zap.String("integration_id", integration.ID),
			zap.String("external_product_id", externalProductID),
		)
		return nil
	}

	var lastErr error
	for attempt := 0; attempt <= productWebhookMaxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			s.logger.Warn("retrying product webhook processing",
				zap.String("store_id", storeID),
				zap.String("integration_id", integration.ID),
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
		zap.String("store_id", storeID),
		zap.String("integration_id", integration.ID),
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

	// Variant-aware branch: if the ERP returned a parent product with children
	// (e.g. Tiny tipo=V), enrich each variant from its individual GET (where
	// per-variant shipping actually lives) before delegating the whole tree
	// to the productgroup syncer.
	if detailed.IsParent && len(detailed.Variants) > 0 && s.productGroupSyncer != nil {
		s.enrichVariantsFromIndividualGets(ctx, erpProvider, detailed)
		s.applyStoreDefaultDimensions(ctx, integration.StoreID, detailed)
		if err := s.productGroupSyncer.SyncFromERP(ctx, integration.StoreID, integration.Provider, *detailed); err != nil {
			return fmt.Errorf("syncing product group: %w", err)
		}
		return nil
	}

	// Variation child without its own dimensions inherits from the parent,
	// then falls back to merchant-configured store defaults.
	s.inheritShippingFromParent(ctx, erpProvider, detailed)
	s.applyStoreDefaultDimensions(ctx, integration.StoreID, detailed)

	// Check if product has active stock reservations during a live event.
	// Fail-safe: on DB error, assume active event to avoid overwriting local stock.
	skipStock := false
	hasActive, guardErr := s.repo.HasActiveEventForProduct(ctx, externalProductID)
	if guardErr != nil {
		skipStock = true
		s.logger.Warn("failed to check active event for product, skipping stock sync as precaution",
			zap.String("external_product_id", externalProductID),
			zap.Error(guardErr),
		)
	} else if hasActive {
		skipStock = true
		s.logger.Info("skipping ERP stock sync during active event",
			zap.String("external_product_id", externalProductID),
			zap.String("store_id", integration.StoreID),
		)
	}

	if err := s.productSyncer.SyncProduct(ctx, integration.StoreID, integration.Provider, *detailed, skipStock); err != nil {
		return fmt.Errorf("syncing product: %w", err)
	}

	s.logger.Info("product synced from webhook",
		zap.String("integration_id", integration.ID),
		zap.String("external_product_id", externalProductID),
		zap.String("store_id", integration.StoreID),
		zap.Bool("skip_stock", skipStock),
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
			notifyURL = fmt.Sprintf("%s/api/webhooks/%s/%s",
				baseURL,
				paymentProvider.Name(),
				input.StoreID,
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
// TRANSPARENT CHECKOUT OPERATIONS
// =============================================================================

// GetCheckoutConfig retrieves the checkout configuration for a store.
func (s *Service) GetCheckoutConfig(ctx context.Context, integrationID, storeID string) (string, []string, error) {
	paymentProvider, err := s.GetPaymentProvider(ctx, integrationID, storeID)
	if err != nil {
		return "", nil, err
	}

	publicKey, err := paymentProvider.GetPublicKey(ctx)
	if err != nil {
		s.handleProviderError(ctx, integrationID, "get_public_key", err)
		return "", nil, fmt.Errorf("getting public key: %w", err)
	}

	methods, err := paymentProvider.GetPaymentMethods(ctx)
	if err != nil {
		s.handleProviderError(ctx, integrationID, "get_payment_methods", err)
		return "", nil, fmt.Errorf("getting payment methods: %w", err)
	}

	return publicKey, methods, nil
}

// ProcessCardPayment processes a card payment with a tokenized card.
func (s *Service) ProcessCardPayment(ctx context.Context, input ProcessCardPaymentInput) (*ProcessCardPaymentOutput, error) {
	paymentProvider, err := s.GetPaymentProvider(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}

	result, err := paymentProvider.ProcessCardPayment(ctx, providers.CardPaymentInput{
		CartID:          input.CartID,
		Token:           input.CardToken,
		Installments:    input.Installments,
		Customer:        input.Customer,
		Items:           input.Items,
		TotalAmount:     input.TotalAmount,
		Currency:        input.Currency,
		NotifyURL:       input.NotifyURL,
		Metadata:        input.Metadata,
		PaymentMethodID: input.PaymentMethodID,
		IssuerID:        input.IssuerID,
		DeviceID:        input.DeviceID,
	})
	if err != nil {
		s.handleProviderError(ctx, input.IntegrationID, "process_card_payment", err)
		return nil, fmt.Errorf("processing card payment: %w", err)
	}

	return &ProcessCardPaymentOutput{
		PaymentID:      result.PaymentID,
		Status:         string(result.Status),
		StatusDetail:   result.StatusDetail,
		Message:        result.Message,
		Amount:         result.Amount,
		Installments:   result.Installments,
		LastFourDigits: result.LastFourDigits,
		CardBrand:      result.CardBrand,
	}, nil
}

// GeneratePixPayment generates a PIX QR code for payment.
func (s *Service) GeneratePixPayment(ctx context.Context, input GeneratePixPaymentInput) (*GeneratePixPaymentOutput, error) {
	paymentProvider, err := s.GetPaymentProvider(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}

	result, err := paymentProvider.GeneratePixPayment(ctx, providers.PixPaymentInput{
		CartID:      input.CartID,
		Customer:    input.Customer,
		Items:       input.Items,
		TotalAmount: input.TotalAmount,
		Currency:    input.Currency,
		NotifyURL:   input.NotifyURL,
		Metadata:    input.Metadata,
	})
	if err != nil {
		s.handleProviderError(ctx, input.IntegrationID, "generate_pix_payment", err)
		return nil, fmt.Errorf("generating pix payment: %w", err)
	}

	return &GeneratePixPaymentOutput{
		PaymentID:  result.PaymentID,
		QRCode:     result.QRCode,
		QRCodeText: result.QRCodeText,
		Amount:     result.Amount,
		ExpiresAt:  result.ExpiresAt,
		TicketURL:  result.TicketURL,
	}, nil
}

// =============================================================================
// WEBHOOK OPERATIONS
// =============================================================================

// StoreWebhookEvent stores a webhook event for processing.
func (s *Service) StoreWebhookEvent(ctx context.Context, input StoreWebhookInput) error {
	// Resolve integration from store_id + provider
	integrationType := "payment"
	switch input.Provider {
	case "tiny":
		integrationType = "erp"
	case "instagram":
		integrationType = "social"
	}
	integration, err := s.repo.GetActiveByProvider(ctx, input.StoreID, integrationType, input.Provider)
	if err != nil {
		return fmt.Errorf("no active integration found for store %s provider %s: %w", input.StoreID, input.Provider, err)
	}
	input.IntegrationID = integration.ID

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
// On paid, it also reverses the cart's stock reservations in the ERP and creates
// one final sales order already marked as paid (with customer + shipping data).
//
// TODO(refunded): when status == refunded we currently only mark the cart —
// we should also cancel the Tiny sales order (CancelOrder) which reverses stock
// and puts the order in "Cancelada". See createFinalERPOrder for the creation side.
func (s *Service) ProcessPaymentNotification(ctx context.Context, input ProcessPaymentInput) error {
	// Resolve integration from store_id + provider
	integration, err := s.repo.GetActiveByProvider(ctx, input.StoreID, "payment", input.Provider)
	if err != nil {
		return fmt.Errorf("no active payment integration found for store %s provider %s: %w", input.StoreID, input.Provider, err)
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
		s.handleProviderError(ctx, integration.ID, "process_payment_notification", err)
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

	// Update cart payment status and payment method
	if err := s.repo.UpdateCartPaymentStatus(ctx, status.ExternalReference, cartPaymentStatus, status.PaymentID, status.PaidAt, status.PaymentMethod); err != nil {
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
		zap.String("payment_method", status.PaymentMethod),
	)

	// Only the paid path triggers the ERP finalization. Everything else (failed,
	// cancelled, pending, refunded) just updates the cart status for now — see
	// the refunded TODO at the top of this method.
	if cartPaymentStatus != "paid" {
		return nil
	}

	if err := s.finalizeCartERPOrder(ctx, status.ExternalReference, input.StoreID, status); err != nil {
		// Never propagate ERP errors to the payment provider — the money already
		// moved. Log and fall through so the webhook ACKs.
		s.logger.Error("failed to finalize ERP order for paid cart",
			zap.String("cart_id", status.ExternalReference),
			zap.String("payment_id", status.PaymentID),
			zap.Error(err),
		)
	}

	return nil
}

// finalizeCartERPOrder is the post-payment ERP workflow: reverse the Tiny
// saída-manual reservations held during the live, then create a single sales
// order already marked as paid, with customer identity and delivery address.
// Idempotent by cart.external_order_id.
func (s *Service) finalizeCartERPOrder(ctx context.Context, cartID, storeID string, status *providers.PaymentStatus) error {
	erpIntegration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		s.logger.Debug("no active ERP integration, skipping paid-order creation",
			zap.String("store_id", storeID),
			zap.String("cart_id", cartID),
		)
		return nil
	}

	cart, err := s.repo.GetCartForPaidOrder(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart for ERP order: %w", err)
	}
	if cart.ExternalOrderID != "" {
		s.logger.Debug("cart already has ERP order, skipping",
			zap.String("cart_id", cartID),
			zap.String("external_order_id", cart.ExternalOrderID),
		)
		return nil
	}

	erpProvider, err := s.getERPProvider(ctx, erpIntegration)
	if err != nil {
		return fmt.Errorf("creating ERP provider: %w", err)
	}

	// 1. Reverse all active saída-manual reservations for this cart — the final
	// order will decrement stock itself via LaunchOrderStock, so keeping the
	// reservations would double-count.
	reservations, err := s.repo.ListActiveReservationsByCart(ctx, cartID)
	if err != nil {
		return fmt.Errorf("listing cart reservations: %w", err)
	}
	for _, r := range reservations {
		obs := fmt.Sprintf("Estorno reserva pós-pagamento - Cart %s", cartID)
		if _, reverseErr := erpProvider.ReverseStockReservation(ctx, r.ExternalProductID, r.Quantity, 0, obs); reverseErr != nil {
			s.logger.Warn("failed to reverse ERP reservation on paid, proceeding anyway",
				zap.String("cart_id", cartID),
				zap.String("reservation_id", r.ID),
				zap.Error(reverseErr),
			)
		}
	}
	if err := s.repo.ReverseReservationsByCart(ctx, cartID); err != nil {
		s.logger.Error("failed to mark reservations reversed",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
	}

	// 2. Create the paid sales order.
	return s.createFinalERPOrder(ctx, erpProvider, erpIntegration, storeID, cart.EventID, *cart, status)
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

	// Store webhook event for audit trail (only if we have payload and store context)
	if len(input.RawPayload) > 0 {
		if err := s.StoreWebhookEvent(ctx, StoreWebhookInput{
			StoreID:        event.StoreID,
			Provider:       "instagram",
			EventType:      "live_comments",
			EventID:        input.CommentID,
			Payload:        input.RawPayload,
			SignatureValid: true, // Instagram webhook signature validation could be added
		}); err != nil {
			s.logger.Error("failed to store instagram webhook event",
				zap.String("store_id", event.StoreID),
				zap.String("comment_id", input.CommentID),
				zap.Error(err),
			)
			// Don't return error - continue processing the comment
		}
	}

	// Increment comment counter on session
	if err := s.repo.IncrementLiveSessionComments(ctx, session.ID); err != nil {
		s.logger.Error("failed to increment comment counter",
			zap.String("session_id", session.ID),
			zap.Error(err),
		)
	}

	// Check if processing is paused
	if event.ProcessingPaused {
		s.logger.Info("processing paused, storing comment only",
			zap.String("event_id", event.ID),
			zap.String("comment_id", input.CommentID),
			zap.String("username", input.Username),
		)

		// Save comment with "paused" result but don't process cart
		_, err := s.repo.CreateLiveComment(ctx, CreateLiveCommentParams{
			SessionID:         session.ID,
			EventID:           event.ID,
			Platform:          "instagram",
			PlatformCommentID: input.CommentID,
			PlatformUserID:    input.UserID,
			PlatformHandle:    input.Username,
			Text:              input.Text,
			HasPurchaseIntent: false, // Don't parse when paused
			Result:            "paused",
		})
		if err != nil {
			s.logger.Error("failed to save paused comment", zap.Error(err))
		}
		return nil
	}

	// Parse purchase intent
	intent := ParsePurchaseIntent(input.Text)
	hasPurchaseIntent := intent != nil

	// Try to match product by keyword
	var product *ProductRow
	if hasPurchaseIntent {
		product = s.findProductByKeyword(ctx, event.StoreID, input.Text)

		// If no keyword match but has purchase intent, try active product as fallback
		if product == nil && event.CurrentActiveProductID != nil && *event.CurrentActiveProductID != "" {
			s.logger.Info("no keyword match, trying active product fallback",
				zap.String("event_id", event.ID),
				zap.String("active_product_id", *event.CurrentActiveProductID),
			)
			product, _ = s.repo.GetProductByID(ctx, event.StoreID, *event.CurrentActiveProductID)
		}
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

	// Validate maxQuantityPerItem limit
	storeInfo, _ := s.repo.GetStoreInfo(ctx, event.StoreID)
	if storeInfo != nil && storeInfo.MaxQuantityPerItem > 0 {
		currentQty, _ := s.repo.GetProductQuantityInUserCart(ctx, event.ID, input.UserID, product.ID)
		maxAllowed := storeInfo.MaxQuantityPerItem

		if currentQty >= maxAllowed {
			// User already has max quantity, ignore this request
			s.logger.Info("user already at max quantity for product, ignoring",
				zap.String("username", input.Username),
				zap.String("product_id", product.ID),
				zap.Int("current_qty", currentQty),
				zap.Int("max_allowed", maxAllowed),
			)
			if commentID != "" {
				_ = s.repo.UpdateLiveCommentResult(ctx, commentID, false, product.ID, intent.Quantity, "max_quantity_reached")
			}
			// Send reply notifying user they've reached the limit
			go s.sendMaxQuantityReply(ctx, event.StoreID, input.CommentID, input.UserID, input.Username, product.Name, maxAllowed, true)
			return nil
		}

		// Cap quantity to remaining allowed
		remaining := maxAllowed - currentQty
		if intent.Quantity > remaining {
			s.logger.Info("capping quantity to max allowed",
				zap.String("username", input.Username),
				zap.String("product_id", product.ID),
				zap.Int("requested", intent.Quantity),
				zap.Int("capped_to", remaining),
			)
			// Send reply notifying user their quantity was capped
			go s.sendMaxQuantityReply(ctx, event.StoreID, input.CommentID, input.UserID, input.Username, product.Name, maxAllowed, false)
			intent.Quantity = remaining
		}
	}

	// Calculate partial fulfillment: how many available vs waitlisted
	availableQty := intent.Quantity
	if product.Stock < intent.Quantity {
		availableQty = product.Stock
	}
	if availableQty < 0 {
		availableQty = 0
	}
	waitlistQty := intent.Quantity - availableQty

	// Reserve available stock
	if availableQty > 0 {
		if stockErr := s.repo.DecrementProductStock(ctx, product.ID, availableQty); stockErr != nil {
			// Failed to reserve even available stock - put all in waitlist
			s.logger.Warn("failed to decrement stock, putting all in waitlist",
				zap.Error(stockErr),
				zap.Int("attempted", availableQty),
			)
			availableQty = 0
			waitlistQty = intent.Quantity
		}
	}

	// Handle waitlist items
	if waitlistQty > 0 {
		// Check if user already on waitlist for this product
		alreadyWaiting, _ := s.repo.GetWaitlistItemByEventUserProduct(ctx, event.ID, input.UserID, product.ID)
		if alreadyWaiting {
			s.logger.Info("user already on waitlist, ignoring waitlist portion",
				zap.String("username", input.Username),
				zap.String("product_id", product.ID),
				zap.Int("waitlist_qty", waitlistQty),
			)
			// If no available stock either, just return
			if availableQty == 0 {
				if commentID != "" {
					_ = s.repo.UpdateLiveCommentResult(ctx, commentID, true, product.ID, intent.Quantity, "already_waitlisted")
				}
				return nil
			}
			// Otherwise, only add the available portion to cart
			waitlistQty = 0
		} else {
			// Add to waitlist table
			position, _ := s.repo.GetNextWaitlistPosition(ctx, event.ID, product.ID)
			_, err = s.repo.CreateWaitlistItem(ctx, CreateWaitlistItemParams{
				EventID:        event.ID,
				ProductID:      product.ID,
				PlatformUserID: input.UserID,
				PlatformHandle: input.Username,
				Quantity:       waitlistQty,
				Position:       position,
			})
			if err != nil {
				s.logger.Error("failed to create waitlist item", zap.Error(err))
			}

			s.logger.Info("user added to waitlist (partial fulfillment)",
				zap.String("username", input.Username),
				zap.String("product_id", product.ID),
				zap.Int("available_qty", availableQty),
				zap.Int("waitlist_qty", waitlistQty),
				zap.Int("position", position),
			)
		}
	}

	// Determine total quantity to add to cart
	totalQtyToAdd := availableQty + waitlistQty
	if totalQtyToAdd == 0 {
		// Nothing to add
		return nil
	}

	// Add product to cart with partial fulfillment
	result, err := s.liveService.AddToCart(ctx, live.AddToCartInput{
		EventID:            event.ID,
		PlatformUserID:     input.UserID,
		PlatformHandle:     input.Username,
		ProductID:          product.ID,
		ProductPrice:       product.Price,
		Quantity:           totalQtyToAdd,
		WaitlistedQuantity: waitlistQty,
	})
	if err != nil {
		// If we reserved stock but failed to add to cart, release it
		if availableQty > 0 {
			_ = s.repo.IncrementProductStock(ctx, product.ID, availableQty)
		}
		return fmt.Errorf("adding to cart: %w", err)
	}

	// Update comment result
	if commentID != "" {
		if waitlistQty > 0 && availableQty > 0 {
			_ = s.repo.UpdateLiveCommentResult(ctx, commentID, true, product.ID, intent.Quantity, "partial_fulfillment")
		} else if waitlistQty > 0 {
			_ = s.repo.UpdateLiveCommentResult(ctx, commentID, true, product.ID, intent.Quantity, "waitlisted")
		} else {
			_ = s.repo.UpdateLiveCommentResult(ctx, commentID, true, product.ID, intent.Quantity, "added_to_cart")
		}
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

	// Reserve stock in ERP (only for available items)
	if availableQty > 0 {
		if syncErr := s.ReserveStockInERP(ctx, event.StoreID, result.CartID, event.ID, product.ID, availableQty, product.Price, input.Username); syncErr != nil {
			s.logger.Warn("failed to reserve stock in ERP",
				zap.String("cart_id", result.CartID),
				zap.Error(syncErr),
			)
		}
	}

	// Send immediate notification (fire-and-forget, doesn't block the flow)
	s.sendImmediateNotification(ctx, sendNotificationInput{
		StoreID:           event.StoreID,
		EventID:           event.ID,
		EventTitle:        event.Title,
		CartID:            result.CartID,
		CartToken:         result.CartToken,
		PlatformUserID:    input.UserID,
		PlatformHandle:    input.Username,
		PlatformCommentID: input.CommentID,
		ProductName:       product.Name,
		ProductKeyword:    product.Keyword,
		Quantity:          intent.Quantity,
		TotalItems:        result.TotalItems,
		TotalCents:        result.TotalCents,
		IsNewCart:      result.IsNewCart,
	})

	return nil
}

// sendNotificationInput contains all data needed for immediate notifications.
type sendNotificationInput struct {
	StoreID           string
	EventID           string
	EventTitle        string
	CartID            string
	CartToken         string
	PlatformUserID    string
	PlatformHandle    string
	PlatformCommentID string // Instagram comment ID for reply
	ProductName       string
	ProductKeyword    string
	Quantity          int
	TotalItems        int
	TotalCents        int64
	IsNewCart         bool
}

// sendImmediateNotification sends an immediate checkout notification via the notification service.
// This is fire-and-forget - errors are logged but don't affect the main flow.
func (s *Service) sendImmediateNotification(ctx context.Context, input sendNotificationInput) {
	// Skip if notification service not configured
	if s.notificationService == nil {
		return
	}

	// Determine notification type based on whether this is a new cart or adding to existing
	notifType := notification.TypeCheckoutImmediate
	if !input.IsNewCart {
		notifType = notification.TypeItemAdded
	}

	// Check if we should notify based on store settings
	shouldNotify, err := s.notificationService.ShouldNotify(ctx, input.StoreID, notifType, input.IsNewCart)
	if err != nil {
		s.logger.Warn("failed to check notification settings",
			zap.String("store_id", input.StoreID),
			zap.Error(err),
		)
		return
	}
	if !shouldNotify {
		return
	}

	// Get store info for notification
	storeInfo, err := s.repo.GetStoreInfo(ctx, input.StoreID)
	if err != nil {
		s.logger.Warn("failed to get store info for notification",
			zap.String("store_id", input.StoreID),
			zap.Error(err),
		)
		return
	}

	// Build checkout URL
	frontendURL := config.FrontendURL.StringOr("http://localhost:3000")
	checkoutURL := fmt.Sprintf("%s/cart/%s", frontendURL, input.CartToken)

	// Build template variables
	vars := notification.TemplateVariables{
		Handle:     "@" + input.PlatformHandle,
		Produto:    input.ProductName,
		Keyword:    input.ProductKeyword,
		Quantidade: input.Quantity,
		TotalItens: input.TotalItems,
		Total:      notification.FormatCurrency(input.TotalCents),
		TotalCents: input.TotalCents,
		Link:       checkoutURL,
		Loja:       storeInfo.Name,
		ExpiraEm:   notification.FormatExpiryMinutes(storeInfo.CartExpirationMinutes),
		LiveTitulo: input.EventTitle,
	}

	// Send notification
	result, err := s.notificationService.Send(ctx, notification.SendInput{
		StoreID:           input.StoreID,
		EventID:           input.EventID,
		CartID:            input.CartID,
		CartToken:         input.CartToken,
		PlatformUserID:    input.PlatformUserID,
		PlatformHandle:    input.PlatformHandle,
		PlatformCommentID: input.PlatformCommentID,
		NotificationType:  notifType,
		Variables:         vars,
	})

	if err != nil {
		s.logger.Warn("notification send error",
			zap.String("store_id", input.StoreID),
			zap.String("cart_id", input.CartID),
			zap.Error(err),
		)
		return
	}

	s.logger.Info("immediate notification processed",
		zap.String("store_id", input.StoreID),
		zap.String("cart_id", input.CartID),
		zap.String("status", string(result.Status)),
		zap.Bool("is_new_cart", input.IsNewCart),
	)
}

// sendMaxQuantityReply sends a reply to the user when they've reached or exceeded the max quantity limit.
// This is fire-and-forget - errors are logged but don't affect the main flow.
// isAtLimit: true = already at limit (rejected), false = quantity was capped
func (s *Service) sendMaxQuantityReply(ctx context.Context, storeID, commentID, userID, username, productName string, maxAllowed int, isAtLimit bool) {
	if commentID == "" {
		return
	}

	var message string
	if isAtLimit {
		message = fmt.Sprintf("Oi @%s! Você já atingiu o limite de %d unidades de %s. 🛒", username, maxAllowed, productName)
	} else {
		message = fmt.Sprintf("Oi @%s! Adicionei o máximo permitido (%d unidades) de %s ao seu carrinho. 🛒", username, maxAllowed, productName)
	}

	// Try comment reply first, then DM fallback
	err := s.ReplyToInstagramComment(ctx, storeID, commentID, message)
	if err != nil {
		s.logger.Warn("failed to send max quantity reply via comment, trying DM",
			zap.String("comment_id", commentID),
			zap.Error(err),
		)
		// Fallback to DM
		if dmErr := s.SendInstagramDM(ctx, storeID, userID, message); dmErr != nil {
			s.logger.Warn("failed to send max quantity DM",
				zap.String("user_id", userID),
				zap.Error(dmErr),
			)
		}
	}

	s.logger.Info("max quantity reply sent",
		zap.String("username", username),
		zap.String("product", productName),
		zap.Int("max_allowed", maxAllowed),
		zap.Bool("is_at_limit", isAtLimit),
	)
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

	// Resolve the store from the Instagram account ID
	integration, err := s.repo.GetByInstagramUserID(ctx, input.AccountID)
	if err != nil {
		s.logger.Error("failed to find integration by instagram account",
			zap.String("account_id", input.AccountID),
			zap.Error(err),
		)
		return nil // Don't fail the webhook, just skip storage
	}
	if integration == nil {
		s.logger.Warn("no integration found for instagram account",
			zap.String("account_id", input.AccountID),
		)
		return nil
	}

	// Store webhook event for audit trail
	if len(input.RawPayload) > 0 {
		if err := s.StoreWebhookEvent(ctx, StoreWebhookInput{
			StoreID:        integration.StoreID,
			Provider:       "instagram",
			EventType:      "messaging",
			EventID:        input.MessageID,
			Payload:        input.RawPayload,
			SignatureValid: true,
		}); err != nil {
			s.logger.Error("failed to store instagram dm webhook event",
				zap.String("store_id", integration.StoreID),
				zap.String("message_id", input.MessageID),
				zap.Error(err),
			)
			// Don't return error - continue processing
		}
	}

	// Future: Could be used to handle order confirmations, questions, etc.

	return nil
}

// =============================================================================
// CART → ERP STOCK RESERVATION
// =============================================================================

// ReserveStockInERP creates a manual stock exit (tipo S) in the ERP for a product
// added to a cart. The movement is tracked in stock_reservations for later reversal.
func (s *Service) ReserveStockInERP(ctx context.Context, storeID, cartID, eventID, productID string, quantity int, unitPrice int64, platformHandle string) error {
	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		s.logger.Debug("no active ERP integration, skipping stock reservation",
			zap.String("store_id", storeID),
		)
		return nil
	}

	erpProvider, err := s.getERPProvider(ctx, integration)
	if err != nil {
		return fmt.Errorf("creating ERP provider: %w", err)
	}

	// Get external product ID
	if s.productSyncer == nil {
		return nil
	}
	externalID, _, err := s.productSyncer.GetProduct(ctx, storeID, productID)
	if err != nil || externalID == "" {
		s.logger.Debug("product not linked to ERP, skipping stock reservation",
			zap.String("product_id", productID),
		)
		return nil
	}

	// Idempotency: check if an active reservation already exists for this cart+product
	existing, _ := s.repo.ListActiveReservationsByCartAndProduct(ctx, cartID, productID)
	if len(existing) > 0 {
		s.logger.Debug("stock reservation already exists for cart+product, skipping",
			zap.String("cart_id", cartID),
			zap.String("product_id", productID),
		)
		return nil
	}

	obs := fmt.Sprintf("Reserva LiveCart - @%s - Evento %s", platformHandle, eventID)
	movementID, err := erpProvider.ReserveStock(ctx, externalID, quantity, float64(unitPrice)/100, obs)
	if err != nil {
		return fmt.Errorf("reserving stock in ERP: %w", err)
	}

	_, err = s.repo.CreateStockReservation(ctx, CreateStockReservationParams{
		EventID:           eventID,
		CartID:            cartID,
		ProductID:         productID,
		ExternalProductID: externalID,
		Quantity:          quantity,
		ERPMovementID:     movementID,
	})
	if err != nil {
		// ERP movement was created but we can't track it locally — attempt compensating reversal
		s.logger.Error("failed to save stock reservation, attempting ERP reversal",
			zap.String("cart_id", cartID),
			zap.String("product_id", productID),
			zap.String("erp_movement_id", movementID),
			zap.Error(err),
		)
		reverseObs := fmt.Sprintf("Estorno compensatório - falha DB - Cart %s", cartID)
		if _, reverseErr := erpProvider.ReverseStockReservation(ctx, externalID, quantity, 0, reverseObs); reverseErr != nil {
			s.logger.Error("CRITICAL: failed to compensate ERP stock after DB failure — manual reconciliation required",
				zap.String("external_product_id", externalID),
				zap.Int("quantity", quantity),
				zap.String("erp_movement_id", movementID),
				zap.Error(reverseErr),
			)
		}
		return fmt.Errorf("saving stock reservation: %w", err)
	}

	s.logger.Info("stock reserved in ERP",
		zap.String("cart_id", cartID),
		zap.String("product_id", productID),
		zap.String("external_product_id", externalID),
		zap.Int("quantity", quantity),
		zap.String("erp_movement_id", movementID),
	)

	return nil
}

// =============================================================================
// EVENT END → ERP FINALIZATION
// =============================================================================

// FinalizeEventERP is a no-op in the current flow.
//
// Previously, when a live event ended we reversed every active Tiny reservation
// and created one sales order per cart — regardless of whether the customer had
// paid. The business rule changed: reservations now live until either the cart
// expires (ProcessExpiredCartsForProduct reverses them) or the payment is
// confirmed (ProcessPaymentNotification → finalizeCartERPOrder reverses and
// creates the paid order).
//
// The function is preserved so live.Service can still call it without any
// behavior change if the rule reverts, and so we have a well-known entry point
// for future end-of-event ERP work.
func (s *Service) FinalizeEventERP(ctx context.Context, storeID, eventID string) error {
	s.logger.Debug("FinalizeEventERP called — no-op under paid-first ERP flow",
		zap.String("store_id", storeID),
		zap.String("event_id", eventID),
	)
	return nil
}

// createFinalERPOrder creates a single paid sales order in the ERP for a cart
// whose payment was just confirmed. Uses the customer identity + shipping
// address captured at checkout and the payment details from the provider.
func (s *Service) createFinalERPOrder(ctx context.Context, erpProvider providers.ERPProvider, integration *IntegrationRow, storeID, eventID string, cart CartRow, paymentStatus *providers.PaymentStatus) error {
	// Resolve contact — enriched with customer identity when available, so the
	// Tiny contact ends up with CPF/email/phone instead of just the @handle.
	contactID, err := s.resolveERPContact(ctx, erpProvider, integration, storeID, cart.PlatformUserID, cart.PlatformHandle, cart.CustomerName, cart.CustomerDocument, cart.CustomerEmail, cart.CustomerPhone)
	if err != nil {
		return fmt.Errorf("resolving ERP contact: %w", err)
	}

	// Collect non-waitlisted items
	items, err := s.repo.ListNonWaitlistedCartItems(ctx, cart.ID)
	if err != nil {
		return fmt.Errorf("listing cart items: %w", err)
	}

	var erpItems []providers.ERPOrderItem
	var totalAmount int64
	for _, item := range items {
		if item.ProductExternalID == "" {
			continue
		}
		erpItems = append(erpItems, providers.ERPOrderItem{
			ProductID: item.ProductExternalID,
			Name:      item.ProductName,
			Quantity:  item.Quantity,
			UnitPrice: item.UnitPrice,
		})
		totalAmount += item.UnitPrice * int64(item.Quantity)
	}

	if len(erpItems) == 0 {
		return nil
	}

	order := providers.ERPOrder{
		ExternalID:  cart.ID,
		ContactID:   contactID,
		Items:       erpItems,
		TotalAmount: totalAmount,
		Observation: fmt.Sprintf("LiveCart - Evento %s - @%s", eventID, cart.PlatformHandle),
	}

	// Attach the delivery address from the cart when the customer submitted one.
	if len(cart.ShippingAddress) > 0 {
		var addr struct {
			ZipCode      string `json:"zipCode"`
			Street       string `json:"street"`
			Number       string `json:"number"`
			Complement   string `json:"complement"`
			Neighborhood string `json:"neighborhood"`
			City         string `json:"city"`
			State        string `json:"state"`
		}
		if err := json.Unmarshal(cart.ShippingAddress, &addr); err == nil && addr.Street != "" {
			order.ShippingAddress = &providers.ERPShippingAddress{
				RecipientName: cart.CustomerName,
				Document:      cart.CustomerDocument,
				Phone:         cart.CustomerPhone,
				ZipCode:       addr.ZipCode,
				Street:        addr.Street,
				Number:        addr.Number,
				Complement:    addr.Complement,
				Neighborhood:  addr.Neighborhood,
				City:          addr.City,
				State:         addr.State,
			}
		} else if err != nil {
			s.logger.Warn("failed to parse cart shipping_address",
				zap.String("cart_id", cart.ID),
				zap.Error(err),
			)
		}
	}

	// Attach the chosen shipping option (carrier + service + real cost) so the
	// ERP records the shipment alongside the sales order. Use the real cost
	// (merchant visibility) even when the event applied free shipping to the
	// customer, so the ERP order reflects the actual freight expense.
	if cart.ShippingServiceName != "" {
		order.Shipping = &providers.ERPOrderShipping{
			Carrier:      cart.ShippingCarrier,
			Service:      cart.ShippingServiceName,
			CostCents:    cart.ShippingRealCost,
			DeadlineDays: cart.ShippingDeadline,
		}
	}

	// Flag the order as already paid using the provider-reported details.
	if paymentStatus != nil {
		paidAt := time.Now()
		if paymentStatus.PaidAt != nil {
			paidAt = *paymentStatus.PaidAt
		}
		order.Payment = &providers.ERPOrderPayment{
			Method:       paymentStatus.PaymentMethod,
			PaymentID:    paymentStatus.PaymentID,
			Installments: paymentStatus.Installments,
			PaidAt:       paidAt,
			Amount:       totalAmount,
		}
	}

	result, err := erpProvider.CreateOrder(ctx, order)
	if err != nil {
		return fmt.Errorf("creating ERP order: %w", err)
	}

	// Save external order ID on cart first — ensures idempotency if we retry
	if err := s.repo.UpdateCartExternalOrderID(ctx, cart.ID, result.OrderID); err != nil {
		return fmt.Errorf("saving external order ID: %w", err)
	}

	// Launch stock (permanent decrement)
	if err := erpProvider.LaunchOrderStock(ctx, result.OrderID); err != nil {
		return fmt.Errorf("launching stock for order %s: %w", result.OrderID, err)
	}

	s.logger.Info("paid ERP order created for cart",
		zap.String("cart_id", cart.ID),
		zap.String("erp_order_id", result.OrderID),
		zap.Int("items", len(erpItems)),
		zap.String("payment_id", paymentStatus.PaymentID),
		zap.String("payment_method", paymentStatus.PaymentMethod),
	)

	return nil
}

// =============================================================================
// ERP HELPERS
// =============================================================================

// resolveERPContact finds or creates an ERP contact for the platform user.
// Optional customer fields (name, document, email, phone) enrich the contact
// when creating it; if the contact is already cached or found by handle,
// enrichment is skipped to keep the call idempotent and cheap.
func (s *Service) resolveERPContact(ctx context.Context, erpProvider providers.ERPProvider, integration *IntegrationRow, storeID, platformUserID, platformHandle, customerName, customerDocument, customerEmail, customerPhone string) (string, error) {
	// Check cache first
	cachedID, err := s.repo.GetERPContact(ctx, storeID, integration.ID, platformUserID)
	if err != nil {
		return "", err
	}
	if cachedID != "" {
		return cachedID, nil
	}

	// Search by document first when we have one — most reliable key in Tiny.
	if customerDocument != "" {
		results, err := erpProvider.SearchContacts(ctx, providers.SearchContactsParams{
			CpfCnpj: customerDocument,
		})
		if err == nil && len(results) > 0 {
			_ = s.repo.UpsertERPContact(ctx, storeID, integration.ID, platformUserID, platformHandle, results[0].ContactID)
			return results[0].ContactID, nil
		}
	}

	// Fall back to searching by platform handle.
	results, err := erpProvider.SearchContacts(ctx, providers.SearchContactsParams{
		Name: platformHandle,
	})
	if err == nil && len(results) > 0 {
		_ = s.repo.UpsertERPContact(ctx, storeID, integration.ID, platformUserID, platformHandle, results[0].ContactID)
		return results[0].ContactID, nil
	}

	// Create new contact in ERP. Prefer the real customer name over the handle.
	contactName := customerName
	if contactName == "" {
		contactName = platformHandle
	}
	contact, err := erpProvider.CreateContact(ctx, providers.ERPContactInput{
		Name:       contactName,
		CpfCnpj:    customerDocument,
		Email:      customerEmail,
		Phone:      customerPhone,
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

		// Reverse ERP stock reservations for this cart+product
		reservations, resErr := s.repo.ListActiveReservationsByCartAndProduct(ctx, cart.ID, productID)
		if resErr != nil {
			s.logger.Error("failed to list reservations for expired cart",
				zap.String("cart_id", cart.ID),
				zap.String("product_id", productID),
				zap.Error(resErr),
			)
		}
		if len(reservations) > 0 {
			erpReversed := false
			integration, intErr := s.repo.GetActiveByProvider(ctx, cart.StoreID, "erp", "tiny")
			if intErr != nil {
				s.logger.Warn("no active ERP integration for expired cart reversal, marking reservations as reversed locally only",
					zap.String("store_id", cart.StoreID),
				)
			} else {
				erpProvider, provErr := s.getERPProvider(ctx, integration)
				if provErr != nil {
					s.logger.Error("failed to create ERP provider for expired cart reversal",
						zap.String("cart_id", cart.ID),
						zap.Error(provErr),
					)
				} else {
					erpReversed = true
					for _, res := range reservations {
						obs := fmt.Sprintf("Estorno expiração carrinho LiveCart - Cart %s", cart.ID)
						if _, reverseErr := erpProvider.ReverseStockReservation(ctx, res.ExternalProductID, res.Quantity, 0, obs); reverseErr != nil {
							erpReversed = false
							s.logger.Warn("failed to reverse expired cart stock reservation in ERP",
								zap.String("cart_id", cart.ID),
								zap.String("external_product_id", res.ExternalProductID),
								zap.Error(reverseErr),
							)
						}
					}
				}
			}
			if markErr := s.repo.ReverseReservationsByCartAndProduct(ctx, cart.ID, productID); markErr != nil {
				s.logger.Error("failed to mark reservations as reversed",
					zap.String("cart_id", cart.ID),
					zap.String("product_id", productID),
					zap.Error(markErr),
				)
			}
			if !erpReversed {
				s.logger.Warn("ERP stock reservations NOT reversed for expired cart — manual reconciliation may be needed",
					zap.String("cart_id", cart.ID),
					zap.String("product_id", productID),
				)
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
	product, err := s.repo.GetProductByID(ctx, storeID, productID)
	if err != nil || product == nil {
		// Can't get product — return stock and bail
		_ = s.repo.IncrementProductStock(ctx, productID, next.Quantity)
		s.logger.Error("failed to get product for waitlist fulfillment",
			zap.String("product_id", productID),
			zap.String("store_id", storeID),
			zap.Error(err),
		)
		return
	}

	// Add to cart (no longer waitlisted - WaitlistedQuantity = 0)
	_, err = s.liveService.AddToCart(ctx, live.AddToCartInput{
		EventID:            eventID,
		PlatformUserID:     next.PlatformUserID,
		PlatformHandle:     next.PlatformHandle,
		ProductID:          productID,
		ProductPrice:       product.Price,
		Quantity:           next.Quantity,
		WaitlistedQuantity: 0, // Fulfilled from waitlist, all available now
	})
	if err != nil {
		// Return stock on failure
		_ = s.repo.IncrementProductStock(ctx, productID, next.Quantity)
		s.logger.Error("failed to add waitlisted item to cart", zap.Error(err))
		return
	}

	// Mark waitlist item as fulfilled
	now := time.Now()
	if statusErr := s.repo.UpdateWaitlistItemStatus(ctx, next.ID, "fulfilled", nil, &now, nil); statusErr != nil {
		s.logger.Warn("failed to mark waitlist item as fulfilled",
			zap.String("waitlist_item_id", next.ID),
			zap.Error(statusErr),
		)
	}

	// Reserve stock in ERP for waitlist-fulfilled item
	cart, cartErr := s.repo.GetCartByEventAndUser(ctx, eventID, next.PlatformUserID)
	if cartErr != nil {
		s.logger.Warn("failed to get cart for waitlist ERP reservation",
			zap.String("event_id", eventID),
			zap.String("platform_user_id", next.PlatformUserID),
			zap.Error(cartErr),
		)
	}
	if cart != nil {
		if syncErr := s.ReserveStockInERP(ctx, storeID, cart.ID, eventID, productID, next.Quantity, product.Price, next.PlatformHandle); syncErr != nil {
			s.logger.Warn("failed to reserve stock in ERP for waitlist-fulfilled item", zap.Error(syncErr))
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

	// Log credential expiration info for debugging (only for OAuth providers)
	if creds.AccessToken != "" && integration.Provider != "pagarme" {
		s.logger.Debug("checking token expiration",
			zap.String("integration_id", integration.ID),
			zap.String("provider", integration.Provider),
			zap.Time("expires_at", creds.ExpiresAt),
			zap.Bool("expires_at_is_zero", creds.ExpiresAt.IsZero()),
			zap.Bool("is_expired", creds.IsExpired()),
			zap.Bool("has_refresh_token", creds.RefreshToken != ""),
		)
	}

	// Check if token needs refresh
	if creds.IsExpired() {
		s.logger.Info("token expired, attempting refresh",
			zap.String("integration_id", integration.ID),
			zap.String("provider", integration.Provider),
			zap.Time("expires_at", creds.ExpiresAt),
		)
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
