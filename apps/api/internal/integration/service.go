package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/integration/providers/payment"
	"livecart/apps/api/internal/live"
	"livecart/apps/api/internal/notification"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/crypto"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/idempotency"
	"livecart/apps/api/lib/logger"
	"livecart/apps/api/lib/ratelimit"
	"livecart/apps/api/lib/storage"
)

// ProductSyncer syncs products from external ERP systems into the local database.
type ProductSyncer interface {
	HasProduct(ctx context.Context, storeID, externalID, externalSource string) (bool, error)
	// FilterRegisteredExternalIDs returns which of the given ERP external IDs
	// are already imported for the store+source (batch, no pagination gaps).
	FilterRegisteredExternalIDs(ctx context.Context, storeID, externalSource string, externalIDs []string) ([]string, error)
	GetProduct(ctx context.Context, storeID, productID string) (externalID, externalSource string, err error)
	// SyncProduct updates an existing LiveCart product with the latest ERP data.
	// When product.Shipping is non-nil, dimensions are refreshed too; otherwise
	// the local shipping profile is preserved. skipStock=true keeps local stock
	// entirely (DB-error fail-safe); downgradeOnly=true applies the ERP stock
	// only when it is LOWER than local (safe reductions during the guard
	// window). Both false = normal full stock sync.
	SyncProduct(ctx context.Context, storeID, externalSource string, product providers.ERPProduct, skipStock, downgradeOnly bool) error
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

// PostCheckoutHook is the customer-facing post-payment flow: cancellation/
// shipment/delivery transactional emails + timeline. Best-effort by design —
// implementations must swallow their own errors so the payment webhook ACKs
// regardless. Wired from the postcheckout package via SetPostCheckoutHook.
//
// OnCartPaid saiu daqui (Fatia A4): o tracking_token e a timeline
// `payment_confirmed` nascem na materialização da Order (order/listeners.OnCartPaid).
type PostCheckoutHook interface {
	// OnCartCancelled envia o e-mail transacional do cancelamento (cobrança não
	// concluída). Idempotente na implementação (timeline unique). O e-mail de
	// ESTORNO saiu daqui: virou reactor do domínio Notification
	// (notification/listeners.OnCartRefunded → SendRefundEmail), que reage ao
	// fato cart.refunded em vez de ser chamado inline neste fan-out.
	OnCartCancelled(ctx context.Context, cartID string)
	// OnShipmentPosted fires after a shipment is created or has a
	// tracking_code attached. Idempotent at the implementation: subsequent
	// calls for the same cart are no-ops.
	OnShipmentPosted(ctx context.Context, cartID, trackingCode string)
	// OnDelivered records the terminal state. Source explains who saw it
	// first: "merchant" (dashboard click), "customer" (public page click),
	// "system" (tracking poller saw carrier flip to delivered).
	OnDelivered(ctx context.Context, cartID, source string)
}

// ERPOrderMirror projects ERP state changes into the Order aggregate.
// Implemented by order/listeners.Listener; wired at boot to break the import cycle.
type ERPOrderMirror interface {
	MirrorCartERPToOrder(ctx context.Context, cartID string)
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
	postCheckoutHook    PostCheckoutHook
	notificationService *notification.Service
	storage             *storage.S3Client
	billingGate         BillingGate
	stock               *StockReservations
	expiryScheduler     CartExpiryScheduler
	logger              *zap.Logger

	// erpProviderFactory lets the finalisation state-machine tests inject a
	// scripted ERP provider. nil in production (falls back to getERPProvider,
	// which builds the real Tiny client from the integration credentials).
	erpProviderFactory func(ctx context.Context, integration *IntegrationRow) (providers.ERPProvider, error)

	// Fase 3 do fix do race: lojas com finalização INVERTIDA (criar pedido e
	// lançar estoque ANTES de estornar as reservas — o saldo do Tiny nunca
	// sobe acima do real). Populado de ERP_FINALISE_INVERTED_STORE_IDS
	// (lista separada por vírgula; "*" liga para todas). Pré-requisito da
	// loja: "Permissão de estoque negativo = Sim" no Tiny — sem isso o
	// fallback degrada para a ordem legada, protegida pelos guards da Fase 1.
	invertFinalisationAll      bool
	invertFinalisationStoreIDs map[string]bool

	// Design C: lojas no modo pedido-como-reserva (conversão na iniciação do
	// pagamento). Populado de ERP_ORDER_AT_CHECKOUT_STORE_IDS ("*" = todas).
	orderAtCheckoutAll      bool
	orderAtCheckoutStoreIDs map[string]bool

	// erpOrderMirror projects ERP state changes into the Order aggregate.
	// Wired at boot from main.go; nil = mirror disabled (e.g. order module not yet live).
	erpOrderMirror ERPOrderMirror
}

// finalisationInverted reports whether this store runs the launch-first
// finalisation order (Fase 3).
func (s *Service) finalisationInverted(storeID string) bool {
	return s.invertFinalisationAll || s.invertFinalisationStoreIDs[storeID]
}

// erpProviderFor resolves the ERP provider for an integration, honouring the
// test seam when set.
func (s *Service) erpProviderFor(ctx context.Context, integration *IntegrationRow) (providers.ERPProvider, error) {
	if s.erpProviderFactory != nil {
		return s.erpProviderFactory(ctx, integration)
	}
	return s.getERPProvider(ctx, integration)
}

// SetStorage wires the object storage client (used to delete transient post
// images after they are published to Instagram).
func (s *Service) SetStorage(c *storage.S3Client) {
	s.storage = c
}

// BillingGate blocks new-cart creation for stores with an inactive
// subscription (PRD 007). Implemented by billing.Service; narrow interface
// to keep the packages decoupled.
type BillingGate interface {
	IsStoreBlocked(ctx context.Context, storeID string) bool
	// OnCartPaid registra a venda no ledger (GMV) e reporta o meter ao Stripe.
	// Propaga erro para retry+DLQ (idempotente via ON CONFLICT DO NOTHING).
	OnCartPaid(ctx context.Context, storeID, cartID string, gmvCents int64) error
	// OnCartRefunded registra o estorno no ledger e devolve a taxa cobrada
	// (crédito na próxima fatura). Propaga erro para retry+DLQ.
	OnCartRefunded(ctx context.Context, storeID, cartID string) error
}

// SetBillingGate wires the paywall gate (optional — absent means no gating).
func (s *Service) SetBillingGate(gate BillingGate) {
	s.billingGate = gate
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
	parseStoreFlag := func(envName string) (bool, map[string]bool) {
		all := false
		ids := map[string]bool{}
		for _, part := range strings.Split(os.Getenv(envName), ",") {
			part = strings.TrimSpace(part)
			switch {
			case part == "*":
				all = true
			case part != "":
				ids[part] = true
			}
		}
		return all, ids
	}
	invertAll, invertIDs := parseStoreFlag("ERP_FINALISE_INVERTED_STORE_IDS")
	orderModeAll, orderModeIDs := parseStoreFlag("ERP_ORDER_AT_CHECKOUT_STORE_IDS")
	return &Service{
		repo:                       repo,
		factory:                    factory,
		encryptor:                  encryptor,
		idempotency:                idempotency,
		liveService:                liveService,
		stock:                      NewStockReservations(repo, logger),
		logger:                     logger,
		invertFinalisationAll:      invertAll,
		invertFinalisationStoreIDs: invertIDs,
		orderAtCheckoutAll:         orderModeAll,
		orderAtCheckoutStoreIDs:    orderModeIDs,
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

// SetPostCheckoutHook wires the customer-facing post-payment flow. The hook
// fires after the cart is marked paid and is responsible for tracking-token
// generation and email receipts.
func (s *Service) SetPostCheckoutHook(hook PostCheckoutHook) {
	s.postCheckoutHook = hook
}

// SetNotificationService sets the notification service for sending DMs.
func (s *Service) SetNotificationService(svc *notification.Service) {
	s.notificationService = svc
}

// SetERPOrderMirror wires the order/listeners.Listener so ERP state changes are
// projected into the Order aggregate (best-effort). Called once at boot from main.go.
func (s *Service) SetERPOrderMirror(m ERPOrderMirror) {
	s.erpOrderMirror = m
}

// mirrorToOrder projects the current ERP state of a cart into the Order aggregate.
// Best-effort: mirror failures are swallowed inside MirrorCartERPToOrder and only
// logged — they never propagate to callers.
func (s *Service) mirrorToOrder(ctx context.Context, cartID string) {
	if s.erpOrderMirror == nil {
		return
	}
	s.erpOrderMirror.MirrorCartERPToOrder(ctx, cartID)
}

// =============================================================================
// INTEGRATION CRUD
// =============================================================================

// Create creates a new integration.
func (s *Service) Create(ctx context.Context, input CreateIntegrationInput) (*CreateIntegrationOutput, error) {
	// Enforce single-ERP-per-store: a merchant must disconnect the current ERP
	// before connecting a new one. Mirrors the partial unique index in the DB
	// but surfaces a friendly PT-BR message instead of a constraint violation.
	if input.Type == string(providers.ProviderTypeERP) {
		if existing, err := s.repo.GetAnyByType(ctx, input.StoreID, string(providers.ProviderTypeERP)); err != nil {
			return nil, err
		} else if existing != nil && existing.Provider != input.Provider {
			return nil, httpx.ErrConflict(fmt.Sprintf(
				"você já tem o ERP %s conectado. Desconecte-o antes de conectar outro ERP.",
				existing.Provider,
			))
		}
	}

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

// UpdatePriority sets the priority of an integration scoped to a store. Lower
// number = higher priority in the checkout selection ordering.
func (s *Service) UpdatePriority(ctx context.Context, id, storeID string, priority int) error {
	return s.repo.UpdatePriority(ctx, id, storeID, priority)
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

// RunERPHealthCheck audits the merchant's cadastros against the canonical
// names the order-creation flow looks up (formas-pagamento,
// formas-recebimento, formas-envio). Returns supported=false when the
// underlying ERP provider doesn't implement the optional ERPHealthChecker
// capability, so the FE can hide the section instead of erroring.
func (s *Service) RunERPHealthCheck(ctx context.Context, integrationID, storeID string) (*ERPHealthCheckResponse, error) {
	erpProvider, err := s.GetERPProvider(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}

	checker, ok := erpProvider.(providers.ERPHealthChecker)
	if !ok {
		return &ERPHealthCheckResponse{
			Supported: false,
			CheckedAt: time.Now(),
			Items:     nil,
		}, nil
	}

	result, err := checker.HealthCheck(ctx)
	if err != nil {
		s.handleProviderError(ctx, integrationID, "erp_health_check", err)
		return nil, fmt.Errorf("running ERP health check: %w", err)
	}

	return &ERPHealthCheckResponse{
		Supported: true,
		CheckedAt: result.CheckedAt,
		Items:     result.Items,
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
	//   instagram_business_manage_comments  (comments + live_comments webhooks, moderation)
	//   instagram_business_manage_messages  (checkout-link DMs, story-reply DMs)
	//   instagram_business_content_publish  (publish posts/Reels/Stories from the app)
	authURL := fmt.Sprintf(
		"https://www.instagram.com/oauth/authorize?client_id=%s&redirect_uri=%s&response_type=code&scope=%s&state=%s",
		appID,
		url.QueryEscape(redirectURI),
		url.QueryEscape("instagram_business_basic,instagram_business_manage_comments,instagram_business_manage_messages,instagram_business_content_publish"),
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

// GetProviderURLs returns the redirect (OAuth callback) and webhook URLs the
// merchant must paste into the provider's app config. The webhook URL embeds
// the store ID so it stays stable across reconnects.
func (s *Service) GetProviderURLs(_ context.Context, input GetProviderURLsInput) (*GetProviderURLsOutput, error) {
	urls := buildProviderURLs(input.Provider, input.StoreID)
	if urls.RedirectURL == "" && urls.WebhookURL == "" {
		return nil, httpx.ErrUnprocessable("provider has no setup URLs: " + input.Provider)
	}
	return urls, nil
}

// buildProviderURLs resolves the setup URLs for a provider. Returns an output
// with empty fields when the provider has nothing to expose. Kept as a pure
// helper so it can be reused when assembling integration responses.
func buildProviderURLs(provider, storeID string) *GetProviderURLsOutput {
	base := strings.TrimRight(config.WebhookBaseURL.String(), "/")
	out := &GetProviderURLsOutput{Provider: provider}
	switch provider {
	case "tiny":
		out.RedirectURL = base + "/api/v1/integrations/oauth/tiny/callback"
		if storeID != "" {
			out.WebhookURL = base + "/api/webhooks/tiny/" + storeID
		}
	case "mercado_pago":
		out.RedirectURL = base + "/api/v1/integrations/oauth/mercado_pago/callback"
		if storeID != "" {
			out.WebhookURL = base + "/api/webhooks/mercado_pago/" + storeID
		}
	case "pagarme":
		if storeID != "" {
			out.WebhookURL = base + "/api/webhooks/pagarme/" + storeID
		}
	case "instagram":
		out.RedirectURL = base + "/api/v1/integrations/oauth/instagram/callback"
	case "melhor_envio":
		out.RedirectURL = base + "/api/v1/integrations/oauth/melhor_envio/callback"
		if storeID != "" {
			out.WebhookURL = base + "/api/webhooks/melhor_envio/" + storeID
		}
	}
	return out
}

// RecordWebhookPing stamps webhookLastPingAt on the integration metadata so the
// admin UI can show whether the merchant has the webhook URL correctly wired in
// the provider's app. Best-effort: failures are logged and swallowed so they
// can't block the webhook 200 response (Tiny disables URLs after 20 non-200s).
func (s *Service) RecordWebhookPing(ctx context.Context, storeID, provider string) {
	integrationType := "payment"
	switch provider {
	case "tiny":
		integrationType = "erp"
	case "instagram":
		integrationType = "social"
	case "twilio_whatsapp":
		integrationType = "communication"
	}

	integration, err := s.repo.GetByProvider(ctx, storeID, integrationType, provider)
	if err != nil || integration == nil {
		// Webhook arrived before the merchant created the integration — nothing to stamp.
		return
	}

	metadata := integration.Metadata
	if metadata == nil {
		metadata = make(map[string]any)
	}
	metadata["webhookLastPingAt"] = time.Now().UTC().Format(time.RFC3339)

	if err := s.repo.UpdateMetadata(ctx, integration.ID, metadata); err != nil {
		logger.From(ctx, s.logger).Warn("failed to stamp webhook ping",
			zap.String("store_id", storeID),
			zap.String("provider", provider),
			zap.Error(err),
		)
	}
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
		logger.From(ctx, s.logger).Error("OAuth state not found or expired",
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
		logger.From(ctx, s.logger).Info("Mercado Pago OAuth: requesting TEST credentials (test_token=true)")
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
		logger.From(ctx, s.logger).Error("OAuth token exchange failed",
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

	logger.From(ctx, s.logger).Info("Mercado Pago OAuth completed",
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
	logger.From(ctx, s.logger).Info("Tiny OAuth token exchange - credentials loaded",
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

	logger.From(ctx, s.logger).Info("Tiny OAuth token exchange - request params",
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
		logger.From(ctx, s.logger).Error("Tiny OAuth token exchange failed",
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
	logger.From(ctx, s.logger).Info("Tiny OAuth token received",
		zap.Int("expires_in", tokenResp.ExpiresIn),
		zap.Bool("has_access_token", tokenResp.AccessToken != ""),
		zap.Bool("has_refresh_token", tokenResp.RefreshToken != ""),
	)

	// Default to 4 hours if expires_in is 0 or not provided
	// Tiny access tokens typically last about 4 hours
	expiresInSeconds := tokenResp.ExpiresIn
	if expiresInSeconds <= 0 {
		logger.From(ctx, s.logger).Warn("Tiny OAuth: expires_in is 0 or negative, defaulting to 4 hours",
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

	logger.From(ctx, s.logger).Info("Tiny OAuth credentials created",
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

	logger.From(ctx, s.logger).Info("Tiny OAuth completed",
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
		logger.From(ctx, s.logger).Error("OAuth state not found or expired",
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
		logger.From(ctx, s.logger).Warn("failed to get long-lived token, using short-lived",
			zap.Error(err),
		)
		// Fall back to short-lived token (1 hour)
		longLivedToken = shortLivedToken
		expiresIn = 3600
	}

	// Step 3: Get user profile info (username)
	username, err := s.getInstagramUserProfile(ctx, longLivedToken)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to get Instagram username",
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

	logger.From(ctx, s.logger).Info("Instagram OAuth completed",
		zap.String("store_id", storeID),
		zap.String("integration_id", integrationID),
		zap.String("instagram_user_id", instagramUserID),
		zap.String("username", username),
	)

	// Inscreve a conta nos eventos de webhook. Sem este passo a Meta NÃO
	// entrega nada para a conta — foi o que derrubou a venda por Story do
	// primeiro cliente (18/07/2026): comentário seguia funcionando pelo
	// polling, mas resposta de Story (DM) não tem fallback e morria em
	// silêncio. Best-effort aqui para não quebrar a conexão; o resultado fica
	// no log e o lojista pode reexecutar por EnsureInstagramWebhookSubscription.
	if err := s.SubscribeInstagramWebhooks(ctx, storeID); err != nil {
		logger.From(ctx, s.logger).Error("instagram connected BUT webhook subscription failed — story/DM sales will not work until this is fixed",
			zap.String("store_id", storeID),
			zap.String("integration_id", integrationID),
			zap.Error(err),
		)
	}

	return &OAuthCallbackOutput{
		IntegrationID: integrationID,
		StoreID:       storeID,
		Provider:      "instagram",
		Status:        "active",
	}, nil
}

// SubscribeInstagramWebhooks subscribes the store's connected Instagram account
// to the webhook fields LiveCart consumes (comments + messages). Idempotent:
// Meta accepts repeated calls, so it is safe to run on every connect and from
// the admin repair endpoint.
func (s *Service) SubscribeInstagramWebhooks(ctx context.Context, storeID string) error {
	provider, err := s.resolveInstagramSocialProvider(ctx, storeID)
	if err != nil {
		return err
	}
	subscriber, ok := provider.(interface {
		SubscribeWebhooks(ctx context.Context) error
	})
	if !ok {
		return fmt.Errorf("instagram provider does not support webhook subscription")
	}
	if err := subscriber.SubscribeWebhooks(ctx); err != nil {
		return fmt.Errorf("subscribing instagram webhooks: %w", err)
	}
	logger.From(ctx, s.logger).Info("instagram webhook subscription ensured",
		zap.String("store_id", storeID),
	)
	return nil
}

// GetInstagramWebhookSubscription reports which webhook fields the account is
// currently subscribed to, so the dashboard (and support) can tell at a glance
// whether story/DM sales will work.
func (s *Service) GetInstagramWebhookSubscription(ctx context.Context, storeID string) ([]string, error) {
	provider, err := s.resolveInstagramSocialProvider(ctx, storeID)
	if err != nil {
		return nil, err
	}
	lister, ok := provider.(interface {
		ListSubscribedFields(ctx context.Context) ([]string, error)
	})
	if !ok {
		return nil, fmt.Errorf("instagram provider does not support reading the subscription")
	}
	return lister.ListSubscribedFields(ctx)
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
		logger.From(ctx, s.logger).Error("Instagram token exchange failed",
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
		logger.From(ctx, s.logger).Error("Instagram long-lived token exchange failed",
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
		logger.From(ctx, s.logger).Error("Instagram profile fetch failed",
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
		logger.From(ctx, s.logger).Error("Instagram token refresh failed",
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
			logger.From(ctx, s.logger).Warn("failed to instantiate shipping provider — skipping",
				zap.String("integration_id", rows[i].ID),
				zap.String("provider", rows[i].Provider),
				zap.Error(err),
			)
			continue
		}
		sp, ok := provider.(providers.ShippingProvider)
		if !ok {
			logger.From(ctx, s.logger).Warn("integration is marked as shipping but does not implement ShippingProvider",
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

// =============================================================================
// CONNECT PAGAR.ME
// =============================================================================

// ConnectPagarmeInput is the admin payload to set up or rotate the Pagar.me
// payment integration for a store. Pagar.me uses static API keys (no OAuth),
// so the merchant pastes secret + public into a form and we validate live.
type ConnectPagarmeInput struct {
	StoreID         string
	SecretKey       string
	PublicKey       string
	WebhookUsername string
	WebhookPassword string
}

// ConnectPagarmeOutput mirrors CreateIntegrationOutput for API responses.
type ConnectPagarmeOutput = CreateIntegrationOutput

// ConnectPagarme validates a Pagar.me secret key via TestConnection and
// persists (or updates) the integration as active. The "environment" is
// derived from the key prefix (sk_test_ → sandbox, plain sk_ → production)
// — Pagar.me has no separate sandbox host; the prefix is the only switch.
// The live TestConnection probe below is the authoritative check: the prefix
// rule only catches obvious mistakes like swapping the secret and public keys.
func (s *Service) ConnectPagarme(ctx context.Context, input ConnectPagarmeInput) (*ConnectPagarmeOutput, error) {
	secret := strings.TrimSpace(input.SecretKey)
	public := strings.TrimSpace(input.PublicKey)
	if secret == "" {
		return nil, httpx.ErrBadRequest("chave secreta é obrigatória")
	}
	if public == "" {
		return nil, httpx.ErrBadRequest("chave pública é obrigatória")
	}

	secretEnv := pagarmeKeyEnvironment(secret, "sk_")
	publicEnv := pagarmeKeyEnvironment(public, "pk_")
	if secretEnv == "" {
		return nil, httpx.ErrBadRequest("chave secreta inválida — deve começar com sk_")
	}
	if publicEnv == "" {
		return nil, httpx.ErrBadRequest("chave pública inválida — deve começar com pk_")
	}
	if secretEnv != publicEnv {
		return nil, httpx.ErrBadRequest("as chaves precisam ser do mesmo ambiente (ambas de teste ou ambas de produção)")
	}

	creds := &providers.Credentials{
		APIKey:    secret,
		APISecret: public,
		Extra: map[string]any{
			"public_key":  public,
			"environment": secretEnv,
		},
	}
	if input.WebhookUsername != "" {
		creds.Extra["webhook_username"] = input.WebhookUsername
	}
	if input.WebhookPassword != "" {
		creds.Extra["webhook_password"] = input.WebhookPassword
	}

	probe, err := s.factory.CreatePaymentProvider(providers.ProviderConfig{
		IntegrationID: "probe",
		StoreID:       input.StoreID,
		Type:          providers.ProviderTypePayment,
		Name:          providers.ProviderPagarme,
		Credentials:   creds,
	})
	if err != nil {
		return nil, fmt.Errorf("instantiating pagarme provider: %w", err)
	}
	probeResult, err := probe.TestConnection(ctx)
	if err != nil {
		return nil, httpx.ErrUnprocessable("falha ao validar chave Pagar.me: " + err.Error())
	}
	if probeResult == nil || !probeResult.Success {
		msg := "chave rejeitada pela Pagar.me"
		if probeResult != nil && probeResult.Message != "" {
			msg = probeResult.Message
		}
		return nil, httpx.ErrUnprocessable("falha ao validar chave Pagar.me: " + msg)
	}

	encrypted, err := s.encryptor.EncryptJSON(creds)
	if err != nil {
		return nil, fmt.Errorf("encrypting pagarme credentials: %w", err)
	}

	metadata := map[string]any{
		"public_key":  public,
		"environment": secretEnv,
	}
	if probeResult.AccountInfo != nil {
		if name, ok := probeResult.AccountInfo["name"].(string); ok && name != "" {
			metadata["accountName"] = name
		}
	}

	existing, err := s.repo.GetByProvider(ctx, input.StoreID, string(providers.ProviderTypePayment), string(providers.ProviderPagarme))
	if err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "not found") {
			return nil, err
		}
		existing = nil
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
		Type:        string(providers.ProviderTypePayment),
		Provider:    string(providers.ProviderPagarme),
		Status:      "active",
		Credentials: encrypted,
		Metadata:    metadata,
	})
	if err != nil {
		return nil, err
	}
	return s.toCreateOutput(row), nil
}

// ValidatePagarmeWebhookAuth reads the integration's stored webhook
// username/password (set at connect time, optional) and validates an
// inbound `Authorization: Basic ...` header against them. Returns
// (true, nil) when the merchant has not configured Basic Auth — we
// can't verify what wasn't provided, and Pagar.me does not sign payloads
// so there is no fallback verification path. Returns (false, nil) only
// when creds ARE configured and the inbound auth is wrong/missing.
func (s *Service) ValidatePagarmeWebhookAuth(ctx context.Context, storeID, authHeader string) (bool, error) {
	row, err := s.repo.GetByProvider(ctx, storeID, string(providers.ProviderTypePayment), string(providers.ProviderPagarme))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return true, nil
		}
		return false, err
	}
	creds, err := s.decryptCredentials(row.Credentials)
	if err != nil {
		return false, err
	}
	expectedUser, _ := creds.Extra["webhook_username"].(string)
	expectedPass, _ := creds.Extra["webhook_password"].(string)
	if expectedUser == "" && expectedPass == "" {
		return true, nil
	}

	const prefix = "Basic "
	if !strings.HasPrefix(authHeader, prefix) {
		return false, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(authHeader, prefix))
	if err != nil {
		return false, nil
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return false, nil
	}
	gotUser, gotPass := parts[0], parts[1]
	userOK := subtle.ConstantTimeCompare([]byte(gotUser), []byte(expectedUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(gotPass), []byte(expectedPass)) == 1
	return userOK && passOK, nil
}

// pagarmeKeyEnvironment returns "sandbox" / "production" / "" based on the key
// prefix. Pagar.me only tags SANDBOX keys: sk_test_/pk_test_. Production keys
// carry no environment segment — they are just sk_/pk_ followed by the token.
// There is no sk_live_ (that's Stripe's convention); assuming there was made
// the connect form reject every real production key. Returns "" only when the
// key lacks the scope prefix entirely (e.g. secret and public swapped).
func pagarmeKeyEnvironment(key, scope string) string {
	if !strings.HasPrefix(key, scope) || len(key) <= len(scope) {
		return ""
	}
	if strings.HasPrefix(key, scope+"test_") {
		return "sandbox"
	}
	return "production"
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
		logger.From(ctx, s.logger).Warn("failed to send instagram dm",
			zap.String("store_id", storeID),
			zap.String("recipient_id", recipientID),
			zap.Error(err),
		)
		return err
	}

	logger.From(ctx, s.logger).Info("instagram dm sent",
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
		logger.From(ctx, s.logger).Warn("failed to send private reply to instagram comment",
			zap.String("store_id", storeID),
			zap.String("comment_id", commentID),
			zap.Error(err),
		)
		return err
	}

	// Instagram allows exactly ONE private reply per comment — record that this
	// one is spent so the resend lookup picks a different (fresh) comment.
	if err := s.repo.MarkLiveCommentPrivateReplyUsed(ctx, commentID); err != nil {
		logger.From(ctx, s.logger).Warn("failed to mark private reply as used",
			zap.String("comment_id", commentID), zap.Error(err))
	}

	logger.From(ctx, s.logger).Info("instagram private reply sent",
		zap.String("store_id", storeID),
		zap.String("comment_id", commentID),
	)
	return nil
}

// resolveInstagramSocialProvider returns the active Instagram social provider
// for a store, or an error when the integration is missing/inactive.
func (s *Service) resolveInstagramSocialProvider(ctx context.Context, storeID string) (providers.SocialProvider, error) {
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
	return socialProvider, nil
}

// PublicReplyToInstagramComment posts a PUBLIC reply comment under the buyer's
// comment (visible on the live/post), as opposed to ReplyToInstagramComment
// which sends a private DM. Used by the comment-moderation surface.
func (s *Service) PublicReplyToInstagramComment(ctx context.Context, storeID, commentID, text string) error {
	provider, err := s.resolveInstagramSocialProvider(ctx, storeID)
	if err != nil {
		return err
	}
	if err := provider.ReplyToComment(ctx, commentID, text); err != nil {
		logger.From(ctx, s.logger).Warn("failed to post public reply to instagram comment",
			zap.String("store_id", storeID),
			zap.String("comment_id", commentID),
			zap.Error(err),
		)
		return err
	}
	logger.From(ctx, s.logger).Info("instagram public reply posted",
		zap.String("store_id", storeID),
		zap.String("comment_id", commentID),
	)
	return nil
}

// HideInstagramComment hides or unhides a comment on the connected account.
func (s *Service) HideInstagramComment(ctx context.Context, storeID, commentID string, hidden bool) error {
	provider, err := s.resolveInstagramSocialProvider(ctx, storeID)
	if err != nil {
		return err
	}
	if err := provider.HideComment(ctx, commentID, hidden); err != nil {
		logger.From(ctx, s.logger).Warn("failed to hide instagram comment",
			zap.String("store_id", storeID),
			zap.String("comment_id", commentID),
			zap.Bool("hidden", hidden),
			zap.Error(err),
		)
		return err
	}
	// Mirror the hidden state locally: a hidden comment can't receive a private
	// reply, so the resend lookup must skip it (and pick it back up on unhide).
	if err := s.repo.SetLiveCommentHidden(ctx, commentID, hidden); err != nil {
		logger.From(ctx, s.logger).Warn("failed to mirror comment hidden state",
			zap.String("comment_id", commentID), zap.Error(err))
	}
	logger.From(ctx, s.logger).Info("instagram comment hidden",
		zap.String("store_id", storeID),
		zap.String("comment_id", commentID),
		zap.Bool("hidden", hidden),
	)
	return nil
}

// DeleteInstagramComment deletes a comment on the connected account.
func (s *Service) DeleteInstagramComment(ctx context.Context, storeID, commentID string) error {
	provider, err := s.resolveInstagramSocialProvider(ctx, storeID)
	if err != nil {
		return err
	}
	if err := provider.DeleteComment(ctx, commentID); err != nil {
		logger.From(ctx, s.logger).Warn("failed to delete instagram comment",
			zap.String("store_id", storeID),
			zap.String("comment_id", commentID),
			zap.Error(err),
		)
		return err
	}
	// Mirror the deletion locally so the comment leaves the merchant's list, just
	// like it left the Instagram post. This also excludes it from the resend
	// lookup — a deleted comment can never receive a private reply.
	if err := s.repo.MarkLiveCommentDeleted(ctx, commentID); err != nil {
		logger.From(ctx, s.logger).Warn("failed to mirror comment deletion",
			zap.String("comment_id", commentID), zap.Error(err))
	}
	logger.From(ctx, s.logger).Info("instagram comment deleted",
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
		logger.From(ctx, s.logger).Warn("failed to fetch instagram lives",
			zap.String("store_id", storeID),
			zap.Error(err),
		)
		return nil, err
	}

	logger.From(ctx, s.logger).Info("fetched instagram lives",
		zap.String("store_id", storeID),
		zap.Int("count", len(lives)),
	)
	return lives, nil
}

// FetchInstagramMedia lists recent published posts/reels of the store's
// connected Instagram account (newest first), for the post-event selector.
// `after` pages through results.
func (s *Service) FetchInstagramMedia(ctx context.Context, storeID string, limit int, after string) (*providers.MediaPage, error) {
	provider, err := s.resolveInstagramSocialProvider(ctx, storeID)
	if err != nil {
		return nil, err
	}
	page, err := provider.GetUserMedia(ctx, limit, after)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to fetch instagram media",
			zap.String("store_id", storeID),
			zap.Error(err),
		)
		return nil, err
	}
	logger.From(ctx, s.logger).Info("fetched instagram media",
		zap.String("store_id", storeID),
		zap.Int("count", len(page.Posts)),
	)
	return page, nil
}

// CreateInstagramPostEvent publishes an image post on the connected Instagram
// account and creates a post-commerce event bound to the new post, in one step.
// Reuses live.Service.CreatePostEvent so the post then sells via comments exactly
// like a manually-selected post.
func (s *Service) CreateInstagramPostEvent(ctx context.Context, input CreateInstagramPostInput) (live.CreateLiveOutput, error) {
	return s.publishWithIdempotency(ctx, input, "create_instagram_post",
		func() { s.deleteTransientImage(ctx, input.ImageKey) },
		func() (live.CreateLiveOutput, error) { return s.publishInstagramPostEvent(ctx, input) },
	)
}

func (s *Service) publishInstagramPostEvent(ctx context.Context, input CreateInstagramPostInput) (live.CreateLiveOutput, error) {
	provider, err := s.resolveInstagramSocialProvider(ctx, input.StoreID)
	if err != nil {
		return live.CreateLiveOutput{}, err
	}

	// Publish the image post.
	mediaID, err := provider.PublishImagePost(ctx, input.ImageURL, input.Caption)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to publish instagram image post",
			zap.String("store_id", input.StoreID), zap.Error(err))
		// Clean up the uploaded image even when publishing fails, so a failed
		// attempt doesn't leave an orphan in storage.
		s.deleteTransientImage(ctx, input.ImageKey)
		return live.CreateLiveOutput{}, httpx.ErrUnprocessable("failed to publish the post on Instagram")
	}

	// Instagram has now fetched and stored the image, so the transient upload
	// can be removed. Logged (not swallowed) so a delete failure is visible.
	s.deleteTransientImage(ctx, input.ImageKey)

	// Fetch the new post's permalink/thumbnail (best-effort).
	permalink, thumbnail := "", ""
	if details, dErr := provider.GetMediaDetails(ctx, mediaID); dErr == nil && details != nil {
		permalink = details.Permalink
		thumbnail = details.ThumbnailURL
		if thumbnail == "" {
			thumbnail = details.MediaURL
		}
	}

	// Create the post-commerce event bound to the freshly published post.
	out, err := s.liveService.CreatePostEvent(ctx, live.CreatePostInput{
		StoreID:                input.StoreID,
		Title:                  input.Title,
		MediaID:                mediaID,
		MediaPermalink:         permalink,
		MediaThumbnailURL:      thumbnail,
		MediaCaption:           input.Caption,
		ProductIDs:             input.ProductIDs,
		StartsAt:               input.StartsAt,
		EndsAt:                 input.EndsAt,
		CartExpirationMinutes:  input.CartExpirationMinutes,
		CartMaxQuantityPerItem: input.CartMaxQuantityPerItem,
	})
	if err != nil {
		// The post is already live on Instagram; surface the event error so the
		// merchant can retry binding via "select a post".
		logger.From(ctx, s.logger).Error("post published but event creation failed",
			zap.String("store_id", input.StoreID),
			zap.String("media_id", mediaID),
			zap.Error(err))
		return live.CreateLiveOutput{}, err
	}

	logger.From(ctx, s.logger).Info("instagram post created and event bound",
		zap.String("store_id", input.StoreID),
		zap.String("media_id", mediaID),
		zap.String("event_id", out.ID),
	)
	return out, nil
}

// deleteTransientImage removes a just-published post image from storage. The
// result is logged (success or failure) so a stuck object is visible — the
// presigned URL also expires on its own, so this is best-effort.
func (s *Service) deleteTransientImage(ctx context.Context, key string) {
	if key == "" {
		return
	}
	if s.storage == nil {
		logger.From(ctx, s.logger).Warn("cannot delete post image: storage not wired", zap.String("key", key))
		return
	}
	if err := s.storage.DeleteByKey(ctx, key); err != nil {
		logger.From(ctx, s.logger).Error("failed to delete post image from storage",
			zap.String("key", key), zap.Error(err))
		return
	}
	logger.From(ctx, s.logger).Info("deleted transient post image", zap.String("key", key))
}

// publishWithIdempotency dedupes a publish+bind so a retried submit (e.g. the
// client timed out on a slow publish and the user resubmitted) returns the
// original event instead of publishing the same media to Instagram again.
//
// Dedup is by explicit client key when provided, falling back to a hash of the
// stable inputs within a short window — the uploaded media URL/key are excluded
// from that hash since they differ on every upload, so a resubmit of the same
// post still matches. On a cache hit, onDuplicate cleans up the just-uploaded
// transient media that won't be published.
func (s *Service) publishWithIdempotency(
	ctx context.Context,
	input CreateInstagramPostInput,
	operation string,
	onDuplicate func(),
	publish func() (live.CreateLiveOutput, error),
) (live.CreateLiveOutput, error) {
	integrationID := s.instagramIntegrationID(ctx, input.StoreID)
	// Without the idempotency service or a known integration we can't dedup
	// safely (the record FK needs the integration id) — publish directly.
	if s.idempotency == nil || integrationID == "" {
		return publish()
	}

	// Stable dedup payload: everything that identifies the post EXCEPT the
	// per-upload media url/key, so resubmits of the same content collide.
	dedup := struct {
		Operation              string
		Title                  string
		Caption                string
		ProductIDs             []string
		StartsAt               *time.Time
		EndsAt                 *time.Time
		CartExpirationMinutes  *int
		CartMaxQuantityPerItem *int
	}{
		Operation:              operation,
		Title:                  input.Title,
		Caption:                input.Caption,
		ProductIDs:             input.ProductIDs,
		StartsAt:               input.StartsAt,
		EndsAt:                 input.EndsAt,
		CartExpirationMinutes:  input.CartExpirationMinutes,
		CartMaxQuantityPerItem: input.CartMaxQuantityPerItem,
	}

	req := idempotency.CheckRequest{
		IdempotencyKey: input.IdempotencyKey,
		StoreID:        input.StoreID,
		IntegrationID:  integrationID,
		Operation:      operation,
		Payload:        dedup,
	}

	if cached, err := s.idempotency.Check(ctx, req); err != nil {
		logger.From(ctx, s.logger).Warn("instagram publish idempotency check failed", zap.Error(err))
	} else if cached != nil && cached.Found {
		var out live.CreateLiveOutput
		if json.Unmarshal(cached.Response, &out) == nil && out.ID != "" {
			logger.From(ctx, s.logger).Info("instagram publish deduped, returning original event",
				zap.String("store_id", input.StoreID),
				zap.String("operation", operation),
				zap.String("event_id", out.ID))
			if onDuplicate != nil {
				onDuplicate()
			}
			return out, nil
		}
	}

	rec, err := s.idempotency.Start(ctx, req)
	if err != nil {
		// Couldn't record the attempt — publish anyway rather than block the user.
		logger.From(ctx, s.logger).Warn("instagram publish idempotency start failed", zap.Error(err))
		return publish()
	}

	out, pErr := publish()
	if pErr != nil {
		_ = s.idempotency.Fail(ctx, rec.ID, pErr)
		return out, pErr
	}
	_ = s.idempotency.Complete(ctx, rec.ID, out)
	return out, nil
}

// instagramIntegrationID returns the store's Instagram integration id, or ""
// when none is available (used as the idempotency record FK).
func (s *Service) instagramIntegrationID(ctx context.Context, storeID string) string {
	integ, err := s.repo.GetByProvider(ctx, storeID, "social", "instagram")
	if err != nil || integ == nil {
		return ""
	}
	return integ.ID
}

// CreateInstagramReelEvent publishes a Reel from a public video URL and creates
// the bound post event. The transient video (input.ImageKey) is deleted after.
func (s *Service) CreateInstagramReelEvent(ctx context.Context, input CreateInstagramPostInput, videoURL string) (live.CreateLiveOutput, error) {
	return s.publishWithIdempotency(ctx, input, "create_instagram_reel",
		func() { s.deleteTransientImage(ctx, input.ImageKey) },
		func() (live.CreateLiveOutput, error) { return s.publishInstagramReelEvent(ctx, input, videoURL) },
	)
}

func (s *Service) publishInstagramReelEvent(ctx context.Context, input CreateInstagramPostInput, videoURL string) (live.CreateLiveOutput, error) {
	provider, err := s.resolveInstagramSocialProvider(ctx, input.StoreID)
	if err != nil {
		return live.CreateLiveOutput{}, err
	}

	mediaID, err := provider.PublishReel(ctx, videoURL, input.Caption)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to publish instagram reel",
			zap.String("store_id", input.StoreID), zap.Error(err))
		s.deleteTransientImage(ctx, input.ImageKey)
		return live.CreateLiveOutput{}, httpx.ErrUnprocessable("failed to publish the reel on Instagram")
	}

	// Instagram has fetched and processed the video; remove the transient upload.
	s.deleteTransientImage(ctx, input.ImageKey)

	permalink, thumbnail := "", ""
	if details, dErr := provider.GetMediaDetails(ctx, mediaID); dErr == nil && details != nil {
		permalink = details.Permalink
		thumbnail = details.ThumbnailURL
		if thumbnail == "" {
			thumbnail = details.MediaURL
		}
	}

	out, err := s.liveService.CreatePostEvent(ctx, live.CreatePostInput{
		StoreID:                input.StoreID,
		Title:                  input.Title,
		MediaID:                mediaID,
		MediaPermalink:         permalink,
		MediaThumbnailURL:      thumbnail,
		MediaCaption:           input.Caption,
		ProductIDs:             input.ProductIDs,
		StartsAt:               input.StartsAt,
		EndsAt:                 input.EndsAt,
		CartExpirationMinutes:  input.CartExpirationMinutes,
		CartMaxQuantityPerItem: input.CartMaxQuantityPerItem,
	})
	if err != nil {
		logger.From(ctx, s.logger).Error("reel published but event creation failed",
			zap.String("store_id", input.StoreID),
			zap.String("media_id", mediaID), zap.Error(err))
		return live.CreateLiveOutput{}, err
	}

	logger.From(ctx, s.logger).Info("instagram reel created and event bound",
		zap.String("store_id", input.StoreID),
		zap.String("media_id", mediaID),
		zap.String("event_id", out.ID),
	)
	return out, nil
}

// CreateInstagramStoryEvent publishes a Story (photo or video) from a public URL
// and creates the bound story-commerce event (type='story', 24h window). Buyers
// reply to the Story via DM; ProcessInstagramMessage feeds those into the same
// cart pipeline. The transient media (input.ImageKey) is deleted after publish.
func (s *Service) CreateInstagramStoryEvent(ctx context.Context, input CreateInstagramPostInput, mediaURL string, isVideo bool) (live.CreateLiveOutput, error) {
	return s.publishWithIdempotency(ctx, input, "create_instagram_story",
		func() { s.deleteTransientImage(ctx, input.ImageKey) },
		func() (live.CreateLiveOutput, error) {
			return s.publishInstagramStoryEvent(ctx, input, mediaURL, isVideo)
		},
	)
}

func (s *Service) publishInstagramStoryEvent(ctx context.Context, input CreateInstagramPostInput, mediaURL string, isVideo bool) (live.CreateLiveOutput, error) {
	provider, err := s.resolveInstagramSocialProvider(ctx, input.StoreID)
	if err != nil {
		return live.CreateLiveOutput{}, err
	}

	mediaID, err := provider.PublishStory(ctx, mediaURL, isVideo)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to publish instagram story",
			zap.String("store_id", input.StoreID), zap.Error(err))
		s.deleteTransientImage(ctx, input.ImageKey)
		return live.CreateLiveOutput{}, httpx.ErrUnprocessable("failed to publish the story on Instagram")
	}

	// Instagram has fetched the media; remove the transient upload.
	s.deleteTransientImage(ctx, input.ImageKey)

	// Best-effort metadata (a Story may not expose a public permalink).
	permalink, thumbnail := "", ""
	if details, dErr := provider.GetMediaDetails(ctx, mediaID); dErr == nil && details != nil {
		permalink = details.Permalink
		thumbnail = details.ThumbnailURL
		if thumbnail == "" {
			thumbnail = details.MediaURL
		}
	}

	// Stories expire after 24h — the event ends then too (effective status is
	// derived from ends_at, no background job). Computed here (not in the dedup
	// input) so a retried publish still hashes identically.
	endsAt := time.Now().Add(24 * time.Hour)

	out, err := s.liveService.CreatePostEvent(ctx, live.CreatePostInput{
		StoreID:                input.StoreID,
		Type:                   "story",
		Title:                  input.Title,
		MediaID:                mediaID,
		MediaPermalink:         permalink,
		MediaThumbnailURL:      thumbnail,
		MediaCaption:           input.Caption,
		ProductIDs:             input.ProductIDs,
		EndsAt:                 &endsAt,
		CartExpirationMinutes:  input.CartExpirationMinutes,
		CartMaxQuantityPerItem: input.CartMaxQuantityPerItem,
	})
	if err != nil {
		logger.From(ctx, s.logger).Error("story published but event creation failed",
			zap.String("store_id", input.StoreID),
			zap.String("media_id", mediaID), zap.Error(err))
		return live.CreateLiveOutput{}, err
	}

	logger.From(ctx, s.logger).Info("instagram story created and event bound",
		zap.String("store_id", input.StoreID),
		zap.String("media_id", mediaID),
		zap.String("event_id", out.ID),
	)
	return out, nil
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
	allRateLimited := true
	var firstErr error
	var firstNonRateLimitErr error
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
				var rl *ratelimit.ErrRateLimited
				if !errors.As(r.err, &rl) {
					allRateLimited = false
					if firstNonRateLimitErr == nil {
						firstNonRateLimitErr = r.err
					}
				}
				logger.From(ctx, s.logger).Warn("ERP product search partial failure",
					zap.String("field", r.field),
					zap.String("integration_id", input.IntegrationID),
					zap.Bool("rate_limited", rl != nil),
					zap.Error(r.err),
				)
				continue
			}
			allErrored = false
			allRateLimited = false
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
		// All jobs hit Tiny's rate limit — degrade to "no results" instead of
		// 500 so the merchant can retry instead of seeing an internal-error
		// toast. handleProviderError still flags the integration so the
		// dashboard reflects the throttle.
		if allRateLimited {
			s.handleProviderError(ctx, input.IntegrationID, "search_products", firstErr)
			logger.From(ctx, s.logger).Warn("ERP product search throttled, returning empty results",
				zap.String("integration_id", input.IntegrationID),
			)
			return &SearchProductsOutput{Products: []ERPProductResponse{}, HasMore: false}, nil
		}
		// At least one job failed for a non-rate-limit reason — escalate.
		s.handleProviderError(ctx, input.IntegrationID, "search_products", firstNonRateLimitErr)
		return nil, fmt.Errorf("searching products: %w", firstNonRateLimitErr)
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
			logger.From(ctx, s.logger).Warn("failed to get product details, skipping",
				zap.String("product_id", listed.ID),
				zap.Error(err),
			)
			continue
		}

		isParent := detailed.IsParent && len(detailed.Variants) > 0
		effectiveStock := detailed.Stock
		var variantsResp []ERPVariantResponse
		if isParent {
			// NOTE: NOT calling enrichVariantsFromIndividualGets here on
			// purpose — that helper does N extra Tiny GetProducts (one per
			// variation) just to pull imageUrl + per-variant shipping for the
			// picker preview. With Tiny's 1 req/s rate limit and products
			// carrying 9+ variations, the search request was taking ~15-20s
			// and the front was timing out ("A busca demorou demais").
			// The per-variation GetProduct happens later when the merchant
			// actually imports the product, where the latency is acceptable.
			// At search time we settle for whatever came in the parent's
			// `variacoes[]` (id/sku/stock/attributes) — enough to render the
			// picker.

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

	// Flag products already in the store's catalog so the FE can badge them
	// "já cadastrado" and block re-importing (single batch query — no
	// pagination gaps). Best-effort: a failure here just leaves the flags off.
	if s.productSyncer != nil {
		externalIDs := make([]string, 0, len(products))
		for _, p := range products {
			externalIDs = append(externalIDs, p.ID)
		}
		registered, err := s.productSyncer.FilterRegisteredExternalIDs(ctx, input.StoreID, string(erpProvider.Name()), externalIDs)
		if err != nil {
			logger.From(ctx, s.logger).Warn("failed to check already-imported products",
				zap.String("integration_id", input.IntegrationID),
				zap.Error(err),
			)
		} else if len(registered) > 0 {
			registeredSet := make(map[string]struct{}, len(registered))
			for _, id := range registered {
				registeredSet[id] = struct{}{}
			}
			for i := range products {
				if _, ok := registeredSet[products[i].ID]; ok {
					products[i].AlreadyImported = true
				}
			}
		}
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
		logger.From(ctx, s.logger).Debug("variant already has its own shipping, no parent lookup needed",
			zap.String("tiny_id", detailed.ID))
		return
	}
	if detailed.ParentExternalID == "" {
		logger.From(ctx, s.logger).Debug("product has no parent, cannot inherit shipping",
			zap.String("tiny_id", detailed.ID),
			zap.Bool("is_parent", detailed.IsParent))
		return
	}
	parent, err := erpProvider.GetProduct(ctx, detailed.ParentExternalID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to fetch parent for shipping inheritance",
			zap.String("variant_id", detailed.ID),
			zap.String("parent_id", detailed.ParentExternalID),
			zap.Error(err))
		return
	}
	if parent == nil || parent.Shipping == nil {
		logger.From(ctx, s.logger).Info("parent has no shipping either — variation will land without dimensions",
			zap.String("variant_id", detailed.ID),
			zap.String("parent_id", detailed.ParentExternalID),
			zap.Bool("parent_returned", parent != nil))
		return
	}
	detailed.Shipping = parent.Shipping
	logger.From(ctx, s.logger).Info("inherited shipping from parent",
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
		logger.From(ctx, s.logger).Warn("failed to load store shipping defaults",
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
		logger.From(ctx, s.logger).Info("completed shipping with store defaults",
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

	logger.From(ctx, s.logger).Info("ERP product imported into catalog",
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
	if err := s.productSyncer.SyncProduct(ctx, input.StoreID, externalSource, *detailed, false, false); err != nil {
		return nil, fmt.Errorf("syncing product: %w", err)
	}

	logger.From(ctx, s.logger).Info("product synced manually",
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
//
// The boolean return reports whether the LOCAL STOCK counter was actually
// overwritten by this sync ("stock applied"). The waitlist backstop keys off
// it: promoting after a sync that skipped stock (guard armed) or failed would
// act on a stale/poisoned counter.
func (s *Service) ProcessProductWebhook(ctx context.Context, storeID, provider, externalProductID string) (bool, error) {
	if s.productSyncer == nil {
		logger.From(ctx, s.logger).Warn("product syncer not configured, skipping product webhook")
		return false, nil
	}

	// Resolve integration from store_id + provider
	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", provider)
	if err != nil {
		return false, fmt.Errorf("no active ERP integration found for store %s provider %s: %w", storeID, provider, err)
	}

	// Check if product exists in LiveCart before calling the ERP API
	exists, err := s.productSyncer.HasProduct(ctx, integration.StoreID, externalProductID, integration.Provider)
	if err != nil {
		return false, fmt.Errorf("checking product existence: %w", err)
	}
	if !exists {
		logger.From(ctx, s.logger).Debug("product not registered in livecart, ignoring webhook",
			zap.String("store_id", storeID),
			zap.String("integration_id", integration.ID),
			zap.String("external_product_id", externalProductID),
		)
		return false, nil
	}

	var lastErr error
	for attempt := 0; attempt <= productWebhookMaxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			logger.From(ctx, s.logger).Warn("retrying product webhook processing",
				zap.String("store_id", storeID),
				zap.String("integration_id", integration.ID),
				zap.String("product_id", externalProductID),
				zap.Int("attempt", attempt+1),
				zap.Duration("backoff", backoff),
			)
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(backoff):
			}
		}

		stockApplied, syncErr := s.processProductSync(ctx, integration, externalProductID)
		if syncErr == nil {
			return stockApplied, nil
		}
		lastErr = syncErr
	}

	logger.From(ctx, s.logger).Error("product webhook processing failed after retries",
		zap.String("store_id", storeID),
		zap.String("integration_id", integration.ID),
		zap.String("product_id", externalProductID),
		zap.Int("max_retries", productWebhookMaxRetries),
		zap.Error(lastErr),
	)

	return false, lastErr
}

func (s *Service) processProductSync(ctx context.Context, integration *IntegrationRow, externalProductID string) (bool, error) {
	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return false, fmt.Errorf("creating provider: %w", err)
	}

	erpProvider, ok := provider.(providers.ERPProvider)
	if !ok {
		return false, fmt.Errorf("integration %s is not an ERP provider", integration.ID)
	}

	detailed, err := erpProvider.GetProduct(ctx, externalProductID)
	if err != nil {
		s.handleProviderError(ctx, integration.ID, "webhook_get_product", err)
		return false, fmt.Errorf("fetching product from ERP: %w", err)
	}

	// Variant-aware branch: if the ERP returned a parent product with children
	// (e.g. Tiny tipo=V), enrich each variant from its individual GET (where
	// per-variant shipping actually lives) before delegating the whole tree
	// to the productgroup syncer.
	if detailed.IsParent && len(detailed.Variants) > 0 && s.productGroupSyncer != nil {
		s.enrichVariantsFromIndividualGets(ctx, erpProvider, detailed)
		s.applyStoreDefaultDimensions(ctx, integration.StoreID, detailed)
		if err := s.productGroupSyncer.SyncFromERP(ctx, integration.StoreID, integration.Provider, *detailed); err != nil {
			return false, fmt.Errorf("syncing product group: %w", err)
		}
		return true, nil
	}

	// Variation child without its own dimensions inherits from the parent,
	// then falls back to merchant-configured store defaults.
	s.inheritShippingFromParent(ctx, erpProvider, detailed)
	s.applyStoreDefaultDimensions(ctx, integration.StoreID, detailed)

	// Guard do overwrite de estoque. Enquanto houver reserva ativa numa live
	// OU finalização ERP em voo para o produto, o webhook de estoque do Tiny
	// não pode SUBIR o contador local (a reversão de reservas na finalização
	// infla o saldo do Tiny por segundos → oferta falsa → promoção fantasma da
	// waitlist). Mas REDUÇÕES do lojista no Tiny durante a live são legítimas e
	// devem refletir — então na janela do guard usamos "downgrade-only": aplica
	// só quando o valor do ERP é menor que o local (direção segura, nunca
	// causa promoção fantasma). Fora da janela, sync normal. Fail-safe: em erro
	// de DB, preserva o local inteiro.
	skipStock := false
	downgradeOnly := false
	guarded, guardErr := s.repo.HasStockGuardForProduct(ctx, externalProductID, integration.StoreID, integration.Provider)
	if guardErr != nil {
		skipStock = true
		logger.From(ctx, s.logger).Warn("failed to check stock guard for product, skipping stock sync as precaution",
			zap.String("external_product_id", externalProductID),
			zap.Error(guardErr),
		)
	} else if guarded {
		downgradeOnly = true
		logger.From(ctx, s.logger).Info("ERP stock sync in guard window: applying reductions only (reservation/finalisation in flight)",
			zap.String("external_product_id", externalProductID),
			zap.String("store_id", integration.StoreID),
		)
	}

	if err := s.productSyncer.SyncProduct(ctx, integration.StoreID, integration.Provider, *detailed, skipStock, downgradeOnly); err != nil {
		return false, fmt.Errorf("syncing product: %w", err)
	}

	// O backstop de waitlist só deve rodar quando o estoque pôde AUMENTAR (sync
	// normal). Na janela do guard nunca subimos o local, então não promove;
	// uma redução não libera unidade para ninguém.
	logger.From(ctx, s.logger).Info("product synced from webhook",
		zap.String("integration_id", integration.ID),
		zap.String("external_product_id", externalProductID),
		zap.String("store_id", integration.StoreID),
		zap.Bool("skip_stock", skipStock),
		zap.Bool("downgrade_only", downgradeOnly),
	)

	// stockApplied gate para o backstop de waitlist: só quando o estoque pôde
	// subir (sync normal). skip e downgrade-only nunca sobem o local.
	return !skipStock && !downgradeOnly, nil
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
		logger.From(ctx, s.logger).Warn("idempotency check failed", zap.Error(err))
	}
	if cached != nil && cached.Found {
		var output CreateCheckoutOutput
		if err := json.Unmarshal(cached.Response, &output); err == nil {
			logger.From(ctx, s.logger).Debug("returning cached checkout response",
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
			logger.From(ctx, s.logger).Warn("idempotency start failed", zap.Error(err))
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

// GetPagarmeWebhookStatus checks whether the merchant has registered our
// webhook URL on the Pagar.me dashboard by inspecting recent delivery history
// (GET /hooks). Pagar.me v5 has no public API to list webhook subscriptions,
// so we infer "configured" from at least one matching delivery in the recent
// window, and surface health via the last attempt's response_status.
func (s *Service) GetPagarmeWebhookStatus(ctx context.Context, integrationID, storeID string) (*PagarmeWebhookStatusOutput, error) {
	paymentProvider, err := s.GetPaymentProvider(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}
	pagarme, ok := paymentProvider.(*payment.Pagarme)
	if !ok {
		return nil, httpx.ErrUnprocessable("integration is not Pagar.me")
	}

	urls := buildProviderURLs("pagarme", storeID)
	expectedURL := urls.WebhookURL

	deliveries, err := pagarme.ListRecentHookDeliveries(ctx, 30)
	if err != nil {
		s.handleProviderError(ctx, integrationID, "list_hook_deliveries", err)
		return nil, fmt.Errorf("listing hook deliveries: %w", err)
	}

	out := &PagarmeWebhookStatusOutput{
		ExpectedURL: expectedURL,
		Configured:  false,
	}
	// Pagar.me returns deliveries newest-first. Walk once: count matches,
	// stamp the most recent attempt, and capture the last response_status
	// so the UI can warn when the URL is configured but failing (e.g. 5xx
	// during a deploy, or 401 because basic auth drifted).
	for _, d := range deliveries {
		if d.URL != expectedURL {
			continue
		}
		out.Configured = true
		out.MatchCount++
		if out.LastDeliveryAt.IsZero() || d.LastAttempt.After(out.LastDeliveryAt) {
			out.LastDeliveryAt = d.LastAttempt
			out.LastDeliveryStatus = d.Status
			out.LastResponseStatus = d.ResponseStatus
			out.LastEvent = d.Event
		}
	}
	return out, nil
}

// pagarmeWebhookTestType marks the synthetic event the loopback self-test
// POSTs to our own webhook URL. The receiver (HandlePagarme) recognizes it and
// returns 200 as a pure no-op — no audit row, no ping stamp, no cart change —
// so the delivery-history probe stays honest about real Pagar.me deliveries.
const pagarmeWebhookTestType = "livecart.webhook_test"

// TestPagarmeWebhookEndpoint runs a loopback self-test: it POSTs a synthetic
// event to the store's OWN public Pagar.me webhook URL (reconstructing the
// stored Basic Auth) and reports whether our endpoint is reachable and healthy.
// This lets the merchant validate the webhook right after configuring it,
// instead of waiting for a real customer payment. It never touches Pagar.me.
func (s *Service) TestPagarmeWebhookEndpoint(ctx context.Context, integrationID, storeID string) (*PagarmeWebhookTestOutput, error) {
	// Load the same integration + credentials the inbound path validates
	// against, so the test exercises the real Basic Auth check.
	row, err := s.repo.GetByProvider(ctx, storeID, string(providers.ProviderTypePayment), string(providers.ProviderPagarme))
	if err != nil {
		return nil, httpx.ErrNotFound("integração Pagar.me não encontrada")
	}
	creds, err := s.decryptCredentials(row.Credentials)
	if err != nil {
		return nil, fmt.Errorf("decrypting credentials: %w", err)
	}
	webhookUser, _ := creds.Extra["webhook_username"].(string)
	webhookPass, _ := creds.Extra["webhook_password"].(string)
	authConfigured := webhookUser != "" || webhookPass != ""

	url := buildProviderURLs("pagarme", storeID).WebhookURL
	if url == "" {
		return nil, httpx.ErrUnprocessable("URL de webhook indisponível — verifique a configuração do servidor")
	}

	out := &PagarmeWebhookTestOutput{URL: url, AuthConfigured: authConfigured}

	// Synthetic payload shaped like a Pagar.me v5 event so it flows through the
	// exact same parsing as a real one. The nonce only aids log correlation —
	// the caller observes the HTTP response directly, so no async matching is
	// needed. The URL is built from our own config + a UUID storeId, never from
	// user input, so there is no SSRF surface.
	nonce := uuid.New().String()
	payload := map[string]any{
		"id":         "livecart_test_" + nonce,
		"type":       pagarmeWebhookTestType,
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"data":       map[string]any{"id": "", "nonce": nonce},
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshaling test payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("building test request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authConfigured {
		basic := base64.StdEncoding.EncodeToString([]byte(webhookUser + ":" + webhookPass))
		req.Header.Set("Authorization", "Basic "+basic)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	out.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		// Transport-level failure: DNS, TLS, connection refused, timeout — the
		// endpoint is not reachable over the public internet.
		out.Reachable = false
		out.Healthy = false
		out.Message = "Não foi possível alcançar o endpoint (timeout ou falha de conexão). Verifique se a API está no ar e acessível publicamente."
		logger.From(ctx, s.logger).Warn("pagarme webhook self-test unreachable",
			zap.String("store_id", storeID),
			zap.String("url", url),
			zap.Error(err),
		)
		return out, nil
	}
	defer resp.Body.Close()

	out.Reachable = true
	out.HTTPStatus = resp.StatusCode
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		out.Healthy = true
		out.Message = "Endpoint no ar e respondendo. O lado do LiveCart está pronto para receber webhooks."
	case resp.StatusCode == http.StatusUnauthorized:
		out.Healthy = false
		out.Message = "O endpoint respondeu 401. As credenciais Basic Auth salvas aqui não conferem — confira usuário/senha do webhook."
	default:
		out.Healthy = false
		out.Message = fmt.Sprintf("O endpoint respondeu HTTP %d. Verifique os logs da API.", resp.StatusCode)
	}
	return out, nil
}

// pagarmeWebhookTestOrderPrefix marks the throwaway order created by the live
// (round-trip) webhook test, so its order code can be matched in Pagar.me's
// delivery history.
const pagarmeWebhookTestOrderPrefix = "LCWHTEST-"

// sameWebhookURL compares the URL Pagar.me reports in its delivery history
// against the URL we expect, ignoring differences that do NOT change where the
// request lands: surrounding whitespace, a trailing slash (Fiber routes
// /path and /path/ to the same handler), case of scheme/host, and any query
// string the merchant may have appended.
//
// Field bug (first client, 18/07/2026): the merchant pasted the URL with a
// trailing slash. Pagar.me delivered every event correctly — our handler logged
// them — but the strict string comparison reported "URL diferente da nossa",
// sending the merchant to fix something that was already right.
func sameWebhookURL(a, b string) bool {
	norm := func(raw string) string {
		raw = strings.TrimSpace(raw)
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return strings.TrimRight(raw, "/")
		}
		return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.Path, "/")
	}
	return norm(a) == norm(b)
}

// RunPagarmeWebhookLiveTest validates the REAL webhook delivery path on demand:
// it creates a throwaway PIX order so Pagar.me fires a real order.created
// webhook to the merchant's configured endpoint, then polls Pagar.me's own
// delivery history (GET /hooks) for that order to confirm the event reached our
// URL and what status we returned. The order is canceled at the end. Unlike the
// loopback self-test, this exercises the merchant's dashboard config end-to-end
// without waiting for a real customer payment.
func (s *Service) RunPagarmeWebhookLiveTest(ctx context.Context, integrationID, storeID string) (*PagarmeWebhookLiveTestOutput, error) {
	paymentProvider, err := s.GetPaymentProvider(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}
	pagarme, ok := paymentProvider.(*payment.Pagarme)
	if !ok {
		return nil, httpx.ErrUnprocessable("integration is not Pagar.me")
	}

	expectedURL := buildProviderURLs("pagarme", storeID).WebhookURL
	if expectedURL == "" {
		return nil, httpx.ErrUnprocessable("URL de webhook indisponível — verifique a configuração do servidor")
	}

	code := pagarmeWebhookTestOrderPrefix + uuid.New().String()[:8]
	order, err := pagarme.CreateWebhookTestOrder(ctx, code)
	if err != nil {
		s.handleProviderError(ctx, integrationID, "create_webhook_test_order", err)
		return nil, httpx.ErrUnprocessable("não foi possível criar o pedido de teste na Pagar.me: " + err.Error())
	}

	// Always clean up the throwaway charge, whatever the outcome. Detached
	// context so cleanup runs even if the request context is canceled.
	defer func() {
		if order.ChargeID == "" {
			return
		}
		if cErr := pagarme.CancelTestCharge(context.Background(), order.ChargeID); cErr != nil {
			// A cobrança de teste costuma FALHAR sozinha na Pagar.me (é um Pix
			// descartável que ninguém paga) e aí não há o que cancelar — ela já
			// está em estado terminal. Isso não é problema: baixa o nível para
			// não poluir os alertas com um warn a cada teste de webhook.
			if strings.Contains(cErr.Error(), "cannot be canceled") {
				logger.From(ctx, s.logger).Info("webhook test charge already in a terminal state, nothing to cancel",
					zap.String("store_id", storeID),
					zap.String("charge_id", order.ChargeID),
				)
			} else {
				logger.From(ctx, s.logger).Warn("failed to cancel webhook test charge",
					zap.String("store_id", storeID),
					zap.String("charge_id", order.ChargeID),
					zap.Error(cErr),
				)
			}
		}
	}()

	out := &PagarmeWebhookLiveTestOutput{ExpectedURL: expectedURL, OrderCode: code}

	// Delivery is async — Pagar.me usually posts order.created within a few
	// seconds. Poll its delivery history for OUR order for up to ~18s.
	deadline := time.Now().Add(18 * time.Second)
	for {
		deliveries, derr := pagarme.ListRecentHookDeliveries(ctx, 30)
		if derr == nil {
			for _, d := range deliveries {
				// Match order.* deliveries by order id/code, and charge.*
				// deliveries by the charge id — both belong to our test order.
				if d.OrderCode != code && d.OrderID != order.OrderID && d.OrderID != order.ChargeID {
					continue
				}
				out.Delivered = true
				out.Event = d.Event
				out.HTTPStatus = d.ResponseStatus
				out.ResponseRaw = d.ResponseRaw
				out.DeliveredURL = d.URL
				switch {
				case !sameWebhookURL(d.URL, expectedURL):
					// Delivered our test event, but to a different URL than
					// ours — the dashboard endpoint doesn't match.
					out.Healthy = false
					out.Message = fmt.Sprintf(
						"A Pagar.me entregou o evento de teste, mas para uma URL diferente da nossa (entregue em %q, esperada %q). Ajuste a URL do webhook no painel para a exibida acima.",
						d.URL, expectedURL,
					)
				case d.ResponseStatus >= 200 && d.ResponseStatus < 300:
					out.Healthy = true
					out.Message = "Webhook validado de ponta a ponta: a Pagar.me disparou um evento real e nosso endpoint recebeu com sucesso (HTTP " + fmt.Sprintf("%d", d.ResponseStatus) + ")."
				default:
					out.Healthy = false
					msg := fmt.Sprintf("A Pagar.me entregou o evento, mas nosso endpoint respondeu HTTP %d.", d.ResponseStatus)
					if d.ResponseStatus == http.StatusUnauthorized {
						msg += " As credenciais Basic Auth do painel não conferem com as cadastradas aqui."
					}
					out.Message = msg
				}
				return out, nil
			}
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	// No delivery for our order within the window: either the merchant didn't
	// subscribe to order.created, or the dashboard URL differs, or nothing was
	// wired at all.
	out.Delivered = false
	out.Healthy = false
	out.Message = "Criamos um pedido de teste, mas a Pagar.me não registrou nenhuma entrega no nosso endpoint. Confirme no painel da Pagar.me que a URL do webhook é idêntica à exibida acima e que o evento \"order.created\" (ou \"order.*\") está marcado."
	return out, nil
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
		PaymentID:         result.PaymentID,
		Status:            string(result.Status),
		StatusDetail:      result.StatusDetail,
		Message:           result.Message,
		Amount:            result.Amount,
		Installments:      result.Installments,
		LastFourDigits:    result.LastFourDigits,
		CardBrand:         result.CardBrand,
		AuthorizationCode: result.AuthorizationCode,
		PaidAt:            result.PaidAt,
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
		logger.From(ctx, s.logger).Debug("duplicate webhook event, skipping",
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
// cartPaymentFact is the specific-fact strategy: the resolved cart payment
// status → the canonical fact the reactors subscribe to. paid/refunded carry the
// fan-out (cart.paid/cart.refunded reactors); failed/cancelled are terminal facts
// (cancelled's email stays inline — shared-producer). pending emits nothing
// (deprecated telemetry). Refund vs chargeback stay conflated in cart.refunded
// until the provider status exposes the dispute type.
var cartPaymentFact = map[string]events.Name{
	"paid":      events.CartPaid,
	"refunded":  events.CartRefunded,
	"failed":    events.PaymentFailed,
	"cancelled": events.CartCancelled,
}

// DispatchPaymentProcess is the thin webhook edge (L1 of the event choreography):
// it emits a payment.process COMMAND to the transactional outbox instead of
// running the reconciliation in a detached goroutine. The command consumer
// (main.newApp) runs ProcessPaymentNotification with asynq retry + dead-letter,
// and the outbox makes it crash-durable. dedup_key is empty on purpose — see the
// events.PaymentProcess doc.
func (s *Service) DispatchPaymentProcess(ctx context.Context, input ProcessPaymentInput) error {
	return events.EmitInternal(ctx, s.repo.queries, events.PaymentProcess, "", input)
}

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

	logger.From(ctx, s.logger).Info("payment notification processed",
		zap.String("payment_id", input.PaymentID),
		zap.String("status", string(status.Status)),
		zap.String("external_reference", status.ExternalReference),
	)

	// ExternalReference contains the cart ID (set when creating checkout)
	if status.ExternalReference == "" {
		logger.From(ctx, s.logger).Warn("payment notification has no external reference, cannot update cart",
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
		// Cart JÁ PAGO cuja cobrança foi cancelada = dinheiro devolvido =
		// estorno. O Pagar.me responde "canceled" ao refund de charge não
		// liquidada (praticamente todo estorno same-day) — sem esta
		// transição os hooks de estorno (devolução da taxa de sucesso,
		// e-mail, ERP) nunca rodam nesses casos. "refunded" é terminal: o
		// estorno dispara uma rajada de webhooks (charge.refunded +
		// order.canceled) e o segundo não pode rebaixar o estado.
		if cart, cErr := s.repo.GetCartByID(ctx, status.ExternalReference); cErr == nil &&
			cart != nil && (cart.PaymentStatus == "paid" || cart.PaymentStatus == "refunded") {
			cartPaymentStatus = "refunded"
		}
	case providers.PaymentRefunded:
		cartPaymentStatus = "refunded"
	case providers.PaymentPending, providers.PaymentInProcess:
		cartPaymentStatus = "pending"
	default:
		cartPaymentStatus = "pending"
	}

	// Update cart payment status and payment method
	if err := s.repo.UpdateCartPaymentStatus(ctx, status.ExternalReference, cartPaymentStatus, status.PaymentID, status.PaidAt, status.PaymentMethod); err != nil {
		if errors.Is(err, ErrCartNotPayable) {
			// O cart expirou/cancelou entre a cobrança e este webhook. Não
			// finalizamos (não marca pago, não toca ERP). Se dinheiro entrou
			// mesmo, fica para a reconciliação (E6). ACK benigno.
			logger.From(ctx, s.logger).Info("payment webhook for cart no longer payable (expired/cancelled), skipping finalization",
				zap.String("cart_id", status.ExternalReference),
				zap.String("payment_status", cartPaymentStatus),
			)
			return nil
		}
		logger.From(ctx, s.logger).Error("failed to update cart payment status",
			zap.String("cart_id", status.ExternalReference),
			zap.String("payment_status", cartPaymentStatus),
			zap.Error(err),
		)
		return fmt.Errorf("updating cart payment status: %w", err)
	}

	logger.From(ctx, s.logger).Info("cart payment status updated",
		zap.String("cart_id", status.ExternalReference),
		zap.String("payment_status", cartPaymentStatus),
		zap.String("payment_method", status.PaymentMethod),
	)

	// Emit the canonical CART payment fact (specific-fact strategy — L3). It only
	// fires when the guarded UpdateCartPaymentStatus actually held. The fan-out
	// (coupon, order/GMV/email/waitlist, billing) now lives in REACTORS that
	// consume cart.paid / cart.refunded (registered in main.newApp) — decoupled
	// and retriable — instead of the inline cascade this method used to run.
	// dedup by payment_id so the provider's at-least-once burst collapses.
	if name, ok := cartPaymentFact[cartPaymentStatus]; ok {
		// The paid fact carries the FRESH gateway snapshot (installments, fees,
		// money-release date); OnCartPaid freezes it into the order.paid payload so
		// record the payment in Tiny without re-fetching — the same data the
		// resumable state machine persists to carts.erp_payment_snapshot for retry.
		var snap *providers.PaymentStatus
		if cartPaymentStatus == "paid" {
			snap = status
		}
		// gmv_cents = soma pura de itens (exclui frete e cupom) — fonte única de
		// verdade via GetCartGMVCents. Falha → emite com 0 (receptor usa fallback).
		var gmvCents int64
		if cartPaymentStatus == "paid" {
			if cid, err := uuid.Parse(status.ExternalReference); err == nil {
				cartUUID := pgtype.UUID{Bytes: cid, Valid: true}
				if v, err := s.repo.queries.GetCartGMVCents(ctx, cartUUID); err == nil {
					gmvCents = v
				} else {
					logger.From(ctx, s.logger).Warn("cart.paid: GetCartGMVCents failed, emitting gmv_cents=0",
						zap.String("cart_id", status.ExternalReference), zap.Error(err))
				}
			}
		}
		payload, _ := json.Marshal(struct {
			CartID          string                   `json:"cart_id"`
			StoreID         string                   `json:"store_id"`
			PaymentID       string                   `json:"payment_id"`
			Method          string                   `json:"payment_method"`
			GMVCents        int64                    `json:"gmv_cents,omitempty"`
			PaymentSnapshot *providers.PaymentStatus `json:"payment_snapshot,omitempty"`
		}{status.ExternalReference, input.StoreID, status.PaymentID, status.PaymentMethod, gmvCents, snap})
		_ = events.Emit(ctx, s.repo.queries, events.Envelope{
			Name:     name,
			Source:   events.Source(input.Provider),
			DedupKey: string(name) + ":" + status.PaymentID,
			Payload:  payload,
		})
	}

	// cart.cancelled has a SECOND producer (blocked-handle cancel) with a
	// different intent, so its reactor can't be shared safely — keep the
	// payment-cancel email inline here.
	if cartPaymentStatus == "cancelled" && s.postCheckoutHook != nil {
		s.postCheckoutHook.OnCartCancelled(ctx, status.ExternalReference)
	}

	// ERP now runs entirely as event reactors: finalisation on order.paid
	// (ReactOrderPaidERP — the gateway snapshot rides the order.paid payload frozen
	// by OnCartPaid), refund on order.refunded (ReactOrderRefundedERP), reversal on
	// cart.expired (ReactCartExpiredERP — pre-payment reservation, no Order). They
	// get asynq retry + DLQ instead of the swallowed inline best-effort this method
	// used to run.
	return nil
}

// ReactCartPaid is the cart.paid fan-out reactor (L3): records GMV on the billing
// ledger (billingGate.OnCartPaid). The customer post-payment flow no longer runs
// here — o tracking_token e a timeline `payment_confirmed` nascem na materialização
// da Order (order/listeners.OnCartPaid, Fatia A4), que roda ANTES deste reactor.
// The coupon redemption confirmation also reacts to cart.paid as its own Coupon
// reactor (coupon/listeners.OnCartPaid → ConfirmRedemption), alongside the buyer
// receipt (notification/listeners.OnCartPaid → SendPaidReceipt) and the waitlist
// fulfilment (inventory/listeners.OnCartPaid) reactors that react to the fact
// instead of being called inline in this fan-out.
// The billing step is idempotent (ledger UNIQUE), so asynq redelivery/retry is safe.
// ERP finalisation is a separate reactor (ReactOrderPaidERP, triggered by order.paid)
// because it needs the gateway snapshot.
func (s *Service) ReactCartPaid(ctx context.Context, cartID, storeID string, gmvCents int64) error {
	if s.billingGate != nil {
		if err := s.billingGate.OnCartPaid(ctx, storeID, cartID, gmvCents); err != nil {
			return fmt.Errorf("billing ledger on cart.paid: %w", err)
		}
	}
	return nil
}

// ReactOrderPaidERP is the order.paid reactor that finalises the ERP (Tiny) order.
// It is triggered by the order.paid fact (emitted transactionally by OnCartPaid
// once the immutable Order exists), NOT by cart.paid directly — decoupling the ERP
// retry loop from the customer-facing cart.paid fan-out. It needs the FRESH gateway
// snapshot (installments, fees, money-release date), which OnCartPaid froze into the
// order.paid payload; the resumable state machine also persists it to
// carts.erp_payment_snapshot for admin retry. The finalisation markers are
// authoritative in order_payments (resolved by cart_id, Fatia 11b-1). Errors are
// returned so asynq retries + dead-letters (idempotent via the advisory lock +
// resumable markers). Stores without a Tiny integration no-op so a paid order never
// churns retries. A nil snapshot finalises without payment details — admin retry can
// replay from the persisted snapshot afterwards.
func (s *Service) ReactOrderPaidERP(ctx context.Context, cartID, storeID string, snapshotJSON []byte) error {
	ctx = logger.WithStore(ctx, storeID, "")
	if _, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny"); err != nil {
		return nil // no active ERP integration — nothing to finalise
	}
	var status *providers.PaymentStatus
	if len(snapshotJSON) > 0 {
		var st providers.PaymentStatus
		if err := json.Unmarshal(snapshotJSON, &st); err == nil {
			status = &st
		} else {
			logger.From(ctx, s.logger).Warn("order.paid ERP reactor: bad payment snapshot, finalising without payment details",
				zap.String("cart_id", cartID), zap.Error(err))
		}
	}
	return s.finalizeOrConfirmCartERP(ctx, cartID, storeID, status)
}

// ReactCartRefunded is the cart.refunded fan-out reactor (L3): credits the success
// fee on the billing ledger (billingGate.OnCartRefunded). The coupon redemption
// refund is NO LONGER here: it reacts to cart.refunded as its own Coupon reactor
// (coupon/listeners.OnCartRefunded → RefundRedemption). The billing step is
// idempotent (ledger UNIQUE), so asynq redelivery/retry is safe. The ERP refund
// moved to ReactOrderRefundedERP (triggered by the order.refunded fact, Fatia
// 11b-2), and the refund EMAIL reacts to cart.refunded as its own Notification
// reactor (notification/listeners.OnCartRefunded) — both decoupled from this fan-out.
func (s *Service) ReactCartRefunded(ctx context.Context, cartID, storeID string) error {
	if s.billingGate != nil {
		if err := s.billingGate.OnCartRefunded(ctx, storeID, cartID); err != nil {
			return fmt.Errorf("billing ledger on cart.refunded: %w", err)
		}
	}
	return nil
}

// ReactOrderRefundedERP is the order.refunded reactor that cancels the converted
// ERP (Tiny) order and returns its stock. It is triggered by the order.refunded
// fact (emitted transactionally by OnCartRefunded once the Order is flipped to
// 'refunded'), NOT by cart.refunded directly — decoupling the ERP retry loop from
// the coupon/billing fan-out. Idempotent by erp_order_state (once cancelled, a
// re-run no-ops), so returning the error to get asynq retry + DLQ is safe — this
// path needs no payment snapshot.
func (s *Service) ReactOrderRefundedERP(ctx context.Context, cartID, storeID string) error {
	ctx = logger.WithStore(ctx, storeID, "")
	if err := s.RefundConvertedCartOrder(ctx, cartID, storeID); err != nil {
		return fmt.Errorf("erp refund on order.refunded: %w", err)
	}
	return nil
}

// ReactCartExpiredERP is the cart.expired reactor (L3): it reverses the cart's
// ERP footprint in Tiny, decoupled from ExpireCart's eligibility flip so it gets
// its own asynq retry + DLQ. Design-C converted carts get their order cancelled
// (idempotent by erp_order_state, so the error is returned for retry); non-converted
// carts have their saída-manual reservations reversed best-effort (the local rows
// are marked reversed regardless, so that path does not meaningfully retry).
func (s *Service) ReactCartExpiredERP(ctx context.Context, cartID, storeID string) error {
	ctx = logger.WithStore(ctx, storeID, "")
	st, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart ERP order state on expiry: %w", err)
	}
	if st.State != erpOrderStateNone && st.State != erpOrderStateCancelled {
		// Design C: cancelling the converted order returns stock in Tiny.
		return s.CancelERPOrderForCart(ctx, cartID, storeID)
	}
	// Legacy (non-converted): reverse the saída-manual reservations per cart.
	s.reverseCartReservationsInERP(ctx, cartID, storeID)
	return nil
}

// finalizeCartERPOrder is the post-payment ERP workflow: reverse the Tiny
// saída-manual reservations held during the live, then create a single sales
// order already marked as paid, with customer identity and delivery address.
//
// Resumable state machine (Fase 2): every step leaves a durable marker and
// re-entry only moves FORWARD — no compensation on retry:
//
//	[L]  advisory lock por cart — webhooks de gateway duplicados perdem o
//	     lock e retornam cedo; o vencedor termina o trabalho
//	[S1] snapshot do gateway + carimbo da tentativa persistidos ANTES de
//	     tocar o ERP (retry admin faz replay sem novo webhook)
//	[S0] external_order_id já gravado ⇒ RESUME: re-lança o estoque
//	     (tolerante a "Estoque já lançado." — validado em sandbox 11/07) e
//	     estorna só as reservas ainda 'active'. Mata os Gaps A e B (carts
//	     zumbis presos em 'pending' que o retry recusava).
//	[S2] estornos per-row: cada reserva só é marcada 'reversed' após o Tiny
//	     confirmar a entrada E; falha aborta com 'failed' retomável (antes:
//	     marcação em massa mesmo com Tiny falho ⇒ saída órfã)
//	[S3] createFinalERPOrder (grava external_order_id antes do launch)
//	[S4] done
//
// Failure recovery: if the order creation throws AFTER we have already
// reversed reservations, we re-create the saída-manual exits in Tiny and
// new active reservation rows in the DB so the unit stays held against this
// cart — stock is never silently released against a paid cart. The retry
// then resumes via [S0] (order exists) or re-runs from [S2] (it doesn't).
func (s *Service) finalizeCartERPOrder(ctx context.Context, cartID, storeID string, status *providers.PaymentStatus) error {
	logger.From(ctx, s.logger).Info("starting ERP finalisation for paid cart",
		zap.String("store_id", storeID),
		zap.String("cart_id", cartID),
	)

	// [L] Claim único por cart. Perder o lock significa que outra entrega do
	// mesmo webhook está finalizando AGORA — sem isso, duas goroutines listam
	// as mesmas reservas 'active' e cada uma aplica sua entrada E (saldo do
	// Tiny acima do real: a invariante central do fix quebraria).
	release, acquired, lockErr := s.repo.AcquireCartFinalisationLock(ctx, cartID)
	if lockErr != nil {
		return fmt.Errorf("acquiring finalisation lock: %w", lockErr)
	}
	if !acquired {
		logger.From(ctx, s.logger).Info("ERP finalisation already in flight for cart, skipping duplicate trigger",
			zap.String("cart_id", cartID),
		)
		return nil
	}
	defer release()

	// Idempotência dura: um trigger tardio (redelivery de horas depois) sobre
	// cart já finalizado não deve custar nem uma chamada ao Tiny.
	if stRow, stErr := s.repo.GetCartERPFinalisationStatus(ctx, cartID); stErr == nil && stRow.Status == "done" {
		logger.From(ctx, s.logger).Info("cart ERP finalisation already done, skipping",
			zap.String("cart_id", cartID),
		)
		return nil
	}

	erpIntegration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		// Disambiguate "merchant never set up Tiny" (info, expected) from
		// "Tiny exists but is in error state" (warn, recoverable) so we don't
		// keep losing paid carts under a silent debug log.
		if any, _ := s.repo.GetByProvider(ctx, storeID, "erp", "tiny"); any != nil {
			logger.From(ctx, s.logger).Warn("Tiny integration is not active, skipping paid-order creation",
				zap.String("store_id", storeID),
				zap.String("cart_id", cartID),
				zap.String("integration_id", any.ID),
				zap.String("status", any.Status),
			)
		} else {
			logger.From(ctx, s.logger).Info("no Tiny integration configured, skipping paid-order creation",
				zap.String("store_id", storeID),
				zap.String("cart_id", cartID),
			)
		}
		return nil
	}

	cart, err := s.repo.GetCartForPaidOrder(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart for ERP order: %w", err)
	}

	// [S1] Snapshot + carimbo ANTES de agir. Best-effort: a finalização não
	// para por falha aqui, mas o retry perde o replay se o snapshot faltar.
	var snapshot []byte
	if status != nil {
		if b, encErr := json.Marshal(status); encErr == nil {
			snapshot = b
		} else {
			logger.From(ctx, s.logger).Warn("failed to encode payment status snapshot",
				zap.String("cart_id", cartID),
				zap.Error(encErr),
			)
		}
	}
	if markErr := s.repo.MarkCartERPFinalisationAttempt(ctx, cartID, snapshot); markErr != nil {
		logger.From(ctx, s.logger).Warn("failed to mark ERP finalisation attempt",
			zap.String("cart_id", cartID),
			zap.Error(markErr),
		)
	}

	erpProvider, err := s.erpProviderFor(ctx, erpIntegration)
	if err != nil {
		return fmt.Errorf("creating ERP provider: %w", err)
	}

	// [S0] RESUME: o pedido já existe no Tiny — crash ou falha após o
	// CreateOrder de uma tentativa anterior (Gaps A/B). Nunca pular o launch:
	// ele é tolerante a "já lançado", e pulá-lo devolveria as reservas sem o
	// pedido ter baixado estoque (oversell contra cart pago).
	if cart.ExternalOrderID != "" {
		return s.resumeCartERPFinalisation(ctx, erpProvider, cartID, cart.ExternalOrderID, snapshot)
	}

	// Fase 3: lojas com a flag ligada finalizam em ordem invertida
	// (launch-first) — a perna de oferta do race morre na origem.
	if s.finalisationInverted(storeID) {
		return s.finalizeCartERPOrderInverted(ctx, erpProvider, erpIntegration, storeID, cart, status, snapshot)
	}

	// [S2] Reverse all active saída-manual reservations for this cart — the
	// final order will decrement stock itself via LaunchOrderStock, so keeping
	// the reservations would double-count. Per-row: a row only flips to
	// 'reversed' after Tiny confirmed the entrada E; on failure we abort with
	// a RESUMABLE 'failed' instead of proceeding (the old behaviour created
	// the order anyway, leaving an orphan saída holding phantom stock).
	reservations, err := s.repo.ListActiveReservationsByCart(ctx, cartID)
	if err != nil {
		return fmt.Errorf("listing cart reservations: %w", err)
	}
	logger.From(ctx, s.logger).Info("reversing ERP stock reservations before creating paid order",
		zap.String("cart_id", cartID),
		zap.Int("reservations_count", len(reservations)),
	)

	// Track which reservations actually made it through the Tiny reversal so
	// we know which ones to re-create in the failure path. A reservation that
	// failed to reverse is still "active" on Tiny's side — re-creating it
	// would double-deduct stock.
	reversedSnapshot := make([]StockReservationRow, 0, len(reservations))
	for _, r := range reservations {
		obs := fmt.Sprintf("Estorno reserva pós-pagamento - Cart %s", cartID)
		if _, reverseErr := erpProvider.ReverseStockReservation(ctx, r.ExternalProductID, r.Quantity, 0, obs); reverseErr != nil {
			msg := fmt.Sprintf("estorno de reserva pendente (produto %s): %v", r.ExternalProductID, reverseErr)
			if markErr := s.repo.MarkCartERPFinalisationFailed(ctx, cartID, msg, snapshot); markErr != nil {
				logger.From(ctx, s.logger).Error("failed to mark cart ERP finalisation failed",
					zap.String("cart_id", cartID),
					zap.Error(markErr),
				)
			}
			s.emitERPFinalizationFailed(ctx, cartID, msg)
			logger.From(ctx, s.logger).Warn("failed to reverse ERP reservation on paid cart, aborting for retry",
				zap.String("cart_id", cartID),
				zap.String("reservation_id", r.ID),
				zap.String("external_product_id", r.ExternalProductID),
				zap.Int("quantity", r.Quantity),
				zap.Error(reverseErr),
			)
			return fmt.Errorf("reversing reservation %s: %w", r.ID, reverseErr)
		}
		if dbErr := s.repo.ReverseReservationByID(ctx, r.ID); dbErr != nil {
			// Tiny estornou mas a marcação local falhou: um retry re-estornaria
			// esta row e DUPLICARIA a entrada E (sandbox T10: o Tiny aceita e
			// infla o saldo em silêncio). Loga alto com o movementID para
			// reconciliação manual e segue — a direção do erro é estoque
			// segurado a mais, nunca oferta falsa.
			logger.From(ctx, s.logger).Error("reservation reversed on Tiny but local mark failed — reconcile manually",
				zap.String("cart_id", cartID),
				zap.String("reservation_id", r.ID),
				zap.String("erp_movement_id", r.ERPMovementID),
				zap.Error(dbErr),
			)
		}
		reversedSnapshot = append(reversedSnapshot, r)
	}
	logger.From(ctx, s.logger).Info("ERP stock reservations reversed",
		zap.String("cart_id", cartID),
		zap.Int("requested", len(reservations)),
		zap.Int("succeeded", len(reversedSnapshot)),
	)

	// [S3] Create the paid sales order. On failure, re-reserve and surface
	//     the error to the merchant via cart.erp_finalisation_status='failed'.
	createErr := s.createFinalERPOrder(ctx, erpProvider, erpIntegration, storeID, cart.EventID, *cart, status, true)
	if createErr != nil {
		s.reReserveAfterFailedFinalisation(ctx, erpProvider, cartID, cart.EventID, reversedSnapshot)
		if markErr := s.repo.MarkCartERPFinalisationFailed(ctx, cartID, createErr.Error(), snapshot); markErr != nil {
			logger.From(ctx, s.logger).Error("failed to mark cart ERP finalisation failed",
				zap.String("cart_id", cartID),
				zap.Error(markErr),
			)
		}
		s.emitERPFinalizationFailed(ctx, cartID, createErr.Error())
		return createErr
	}

	// [S4]
	if markErr := s.repo.MarkCartERPFinalisationDone(ctx, cartID); markErr != nil {
		// The order is in Tiny — don't propagate. Just log so the cart
		// shows up in the admin "stuck pending" view if the column ever
		// drifts from reality.
		logger.From(ctx, s.logger).Error("failed to mark cart ERP finalisation done",
			zap.String("cart_id", cartID),
			zap.Error(markErr),
		)
	}
	s.emitERPOrderFinalized(ctx, storeID, cartID)
	return nil
}

// emitERPOrderFinalized publishes the group G erp.order_finalized fact for a
// cart that reached the terminal 'done' state (legacy [S4], inverted, resume or
// confirm). Best-effort and dedup by cart — the terminal marker is monotonic,
// so the fact rides the single transition to done. Reloads the external order
// id best-effort for the payload.
func (s *Service) emitERPOrderFinalized(ctx context.Context, storeID, cartID string) {
	externalOrderID := ""
	if fresh, err := s.repo.GetCartForPaidOrder(ctx, cartID); err == nil {
		externalOrderID = fresh.ExternalOrderID
	}
	_ = events.EmitInternal(ctx, s.repo.queries, events.ERPOrderFinalized, "erp.order_finalized:"+cartID, struct {
		StoreID         string `json:"store_id"`
		CartID          string `json:"cart_id"`
		ExternalOrderID string `json:"external_order_id"`
		Provider        string `json:"provider"`
	}{StoreID: storeID, CartID: cartID, ExternalOrderID: externalOrderID, Provider: "tiny"})
}

// resumeCartERPFinalisation finishes a finalisation that was interrupted
// AFTER the Tiny order already existed (Gaps A/B): re-launches the order
// stock — LaunchOrderStock treats "Estoque já lançado." as success, validated
// against the real API in the 11/07 sandbox battery — then reverses only the
// reservations still 'active' (per-row marks from the first attempt survive),
// and marks the cart done. Monotonic: it only ever moves forward; running it
// twice is a no-op beyond one tolerated launch call.
func (s *Service) resumeCartERPFinalisation(ctx context.Context, erpProvider providers.ERPProvider, cartID, externalOrderID string, snapshot []byte) error {
	logger.From(ctx, s.logger).Info("resuming ERP finalisation for cart with existing order",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", externalOrderID),
	)

	if err := erpProvider.LaunchOrderStock(ctx, externalOrderID); err != nil {
		msg := fmt.Sprintf("relançamento de estoque do pedido %s falhou: %v", externalOrderID, err)
		if markErr := s.repo.MarkCartERPFinalisationFailed(ctx, cartID, msg, snapshot); markErr != nil {
			logger.From(ctx, s.logger).Error("failed to mark cart ERP finalisation failed",
				zap.String("cart_id", cartID),
				zap.Error(markErr),
			)
		}
		s.emitERPFinalizationFailed(ctx, cartID, msg)
		return fmt.Errorf("re-launching stock for order %s: %w", externalOrderID, err)
	}

	if err := s.reverseCartReservationsPerRow(ctx, erpProvider, cartID); err != nil {
		s.markFinalisationFailed(ctx, cartID, err.Error(), snapshot)
		return fmt.Errorf("reversing reservations on resume: %w", err)
	}

	if markErr := s.repo.MarkCartERPFinalisationDone(ctx, cartID); markErr != nil {
		logger.From(ctx, s.logger).Error("failed to mark cart ERP finalisation done after resume",
			zap.String("cart_id", cartID),
			zap.Error(markErr),
		)
	}
	// Group G fact (best-effort): terminal 'done' reached via the resume path.
	// storeID isn't threaded here; reload it best-effort for the payload.
	resumeStoreID := ""
	if fresh, err := s.repo.GetCartForPaidOrder(ctx, cartID); err == nil {
		resumeStoreID = fresh.StoreID
	}
	_ = events.EmitInternal(ctx, s.repo.queries, events.ERPOrderFinalized, "erp.order_finalized:"+cartID, struct {
		StoreID         string `json:"store_id"`
		CartID          string `json:"cart_id"`
		ExternalOrderID string `json:"external_order_id"`
		Provider        string `json:"provider"`
	}{StoreID: resumeStoreID, CartID: cartID, ExternalOrderID: externalOrderID, Provider: "tiny"})
	logger.From(ctx, s.logger).Info("ERP finalisation resumed to done",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", externalOrderID),
	)
	return nil
}

// reverseCartReservationsPerRow estorna todas as reservas 'active' do cart,
// marcando cada row somente após o Tiny confirmar a entrada E correspondente.
// NÃO altera erp_finalisation_status — quem decide o efeito de uma falha é o
// caller (a finalização pós-pago marca 'failed'; a conversão pré-pagamento do
// design C não toca nessa coluna).
func (s *Service) reverseCartReservationsPerRow(ctx context.Context, erpProvider providers.ERPProvider, cartID string) error {
	reservations, err := s.repo.ListActiveReservationsByCart(ctx, cartID)
	if err != nil {
		return fmt.Errorf("listing cart reservations: %w", err)
	}
	for _, r := range reservations {
		obs := fmt.Sprintf("Estorno reserva pós-pagamento - Cart %s", cartID)
		if _, reverseErr := erpProvider.ReverseStockReservation(ctx, r.ExternalProductID, r.Quantity, 0, obs); reverseErr != nil {
			return fmt.Errorf("reversing reservation %s: estorno de reserva pendente (produto %s): %w", r.ID, r.ExternalProductID, reverseErr)
		}
		if dbErr := s.repo.ReverseReservationByID(ctx, r.ID); dbErr != nil {
			// Tiny estornou mas a marcação local falhou: um retry re-estornaria
			// esta row e duplicaria a entrada E (sandbox T10: o Tiny aceita e
			// infla em silêncio). Loga alto com o movementID para reconciliação.
			logger.From(ctx, s.logger).Error("reservation reversed on Tiny but local mark failed — reconcile manually",
				zap.String("cart_id", cartID),
				zap.String("reservation_id", r.ID),
				zap.String("erp_movement_id", r.ERPMovementID),
				zap.Error(dbErr),
			)
		}
	}
	return nil
}

// markFinalisationFailed grava o estado 'failed' com o erro e loga falhas da
// própria marcação (nunca as propaga — o sinal primário é o erro do caller).
func (s *Service) markFinalisationFailed(ctx context.Context, cartID, msg string, snapshot []byte) {
	if markErr := s.repo.MarkCartERPFinalisationFailed(ctx, cartID, msg, snapshot); markErr != nil {
		logger.From(ctx, s.logger).Error("failed to mark cart ERP finalisation failed",
			zap.String("cart_id", cartID),
			zap.Error(markErr),
		)
	}
	s.emitERPFinalizationFailed(ctx, cartID, msg)
}

// emitERPFinalizationFailed publishes the group G erp.finalization_failed fact
// for a cart whose ERP finalisation aborted (retryable). Best-effort. Dedup is
// intentionally coarse (cart-scoped) so a retry-after-failure re-surfaces the
// signal on the outbox rather than being silently collapsed; storeID is
// reloaded best-effort for the payload.
func (s *Service) emitERPFinalizationFailed(ctx context.Context, cartID, reason string) {
	storeID := ""
	externalOrderID := ""
	if fresh, err := s.repo.GetCartForPaidOrder(ctx, cartID); err == nil {
		storeID = fresh.StoreID
		externalOrderID = fresh.ExternalOrderID
	}
	_ = events.EmitInternal(ctx, s.repo.queries, events.ERPFinalizationFailed, "erp.finalization_failed:"+cartID, struct {
		StoreID         string `json:"store_id"`
		CartID          string `json:"cart_id"`
		ExternalOrderID string `json:"external_order_id"`
		Provider        string `json:"provider"`
		Reason          string `json:"reason"`
	}{StoreID: storeID, CartID: cartID, ExternalOrderID: externalOrderID, Provider: "tiny", Reason: reason})
}

// finalizeCartERPOrderInverted é a Fase 3 do fix: cria o pedido e LANÇA o
// estoque ANTES de estornar as reservas. O saldo do Tiny nunca sobe acima do
// valor real durante a finalização — o perfil vira um mergulho transitório
// (desce no launch, volta ao real nos estornos), e mergulho não gera oferta
// falsa: a perna de oferta do race morre na origem, sem depender do guard.
// Bônus estrutural: falha de CreateOrder não compensa NADA (as reservas nunca
// foram tocadas) — reReserveAfterFailedFinalisation é exclusiva do legado.
//
// Fallback: se o launch falhar (ex.: conta com "estoque negativo = Não" e o
// saldo preso nas próprias saídas manuais das reservas — o caso típico de
// live esgotada), estorna as reservas PRIMEIRO e re-tenta o launch uma vez,
// degradando para a ordem legada sob a proteção dos guards da Fase 1. Não há
// matcher confiável de "saldo insuficiente" (mensagem não documentada), então
// o fallback dispara para qualquer erro de launch — inofensivo quando a falha
// era transiente, e o resume cobre se o re-launch também falhar.
func (s *Service) finalizeCartERPOrderInverted(ctx context.Context, erpProvider providers.ERPProvider, erpIntegration *IntegrationRow, storeID string, cart *CartRow, status *providers.PaymentStatus, snapshot []byte) error {
	cartID := cart.ID
	logger.From(ctx, s.logger).Info("finalising cart with inverted order (launch-first)",
		zap.String("cart_id", cartID),
		zap.String("store_id", storeID),
	)

	// [I1] Cria o pedido SEM lançar — o launch é orquestrado aqui fora para
	// permitir o fallback.
	if createErr := s.createFinalERPOrder(ctx, erpProvider, erpIntegration, storeID, cart.EventID, *cart, status, false); createErr != nil {
		if markErr := s.repo.MarkCartERPFinalisationFailed(ctx, cartID, createErr.Error(), snapshot); markErr != nil {
			logger.From(ctx, s.logger).Error("failed to mark cart ERP finalisation failed",
				zap.String("cart_id", cartID),
				zap.Error(markErr),
			)
		}
		s.emitERPFinalizationFailed(ctx, cartID, createErr.Error())
		return createErr
	}

	// createFinalERPOrder grava external_order_id no sucesso; recarrega para
	// obtê-lo. Vazio = cart sem itens vinculados ao ERP (create pulou): não há
	// pedido para lançar — só devolve as reservas e encerra.
	fresh, err := s.repo.GetCartForPaidOrder(ctx, cartID)
	if err != nil {
		return fmt.Errorf("reloading cart after order creation: %w", err)
	}
	orderID := fresh.ExternalOrderID
	if orderID == "" {
		if err := s.reverseCartReservationsPerRow(ctx, erpProvider, cartID); err != nil {
			s.markFinalisationFailed(ctx, cartID, err.Error(), snapshot)
			return err
		}
		if markErr := s.repo.MarkCartERPFinalisationDone(ctx, cartID); markErr != nil {
			logger.From(ctx, s.logger).Error("failed to mark cart ERP finalisation done",
				zap.String("cart_id", cartID),
				zap.Error(markErr),
			)
		}
		return nil
	}

	// [I2] Launch-first, com fallback reverse-first.
	if launchErr := erpProvider.LaunchOrderStock(ctx, orderID); launchErr != nil {
		logger.From(ctx, s.logger).Warn("launch-first failed, falling back to reverse-first order",
			zap.String("cart_id", cartID),
			zap.String("external_order_id", orderID),
			zap.Bool("insufficient_balance", isTinyInsufficientBalanceErr(launchErr)),
			zap.Error(launchErr),
		)
		if err := s.reverseCartReservationsPerRow(ctx, erpProvider, cartID); err != nil {
			s.markFinalisationFailed(ctx, cartID, err.Error(), snapshot)
			return err
		}
		if retryErr := erpProvider.LaunchOrderStock(ctx, orderID); retryErr != nil {
			msg := fmt.Sprintf("lançamento de estoque do pedido %s falhou após fallback: %v", orderID, retryErr)
			if markErr := s.repo.MarkCartERPFinalisationFailed(ctx, cartID, msg, snapshot); markErr != nil {
				logger.From(ctx, s.logger).Error("failed to mark cart ERP finalisation failed",
					zap.String("cart_id", cartID),
					zap.Error(markErr),
				)
			}
			s.emitERPFinalizationFailed(ctx, cartID, msg)
			// O pedido existe e external_order_id está gravado: o retry entra
			// pelo RESUME (launch tolerante) e termina o trabalho.
			return fmt.Errorf("launching stock for order %s after fallback: %w", orderID, retryErr)
		}
		if markErr := s.repo.MarkCartERPFinalisationDone(ctx, cartID); markErr != nil {
			logger.From(ctx, s.logger).Error("failed to mark cart ERP finalisation done",
				zap.String("cart_id", cartID),
				zap.Error(markErr),
			)
		}
		s.emitERPOrderFinalized(ctx, storeID, cartID)
		return nil
	}

	// [I3] Estornos per-row: o saldo do Tiny volta ao valor real. Falha aqui é
	// retomável — o resume re-lança (no-op tolerado) e estorna o restante.
	if err := s.reverseCartReservationsPerRow(ctx, erpProvider, cartID); err != nil {
		s.markFinalisationFailed(ctx, cartID, err.Error(), snapshot)
		return err
	}

	// [I4]
	if markErr := s.repo.MarkCartERPFinalisationDone(ctx, cartID); markErr != nil {
		logger.From(ctx, s.logger).Error("failed to mark cart ERP finalisation done",
			zap.String("cart_id", cartID),
			zap.Error(markErr),
		)
	}
	s.emitERPOrderFinalized(ctx, storeID, cartID)
	logger.From(ctx, s.logger).Info("inverted ERP finalisation completed",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", orderID),
	)
	return nil
}

// reReserveAfterFailedFinalisation re-creates Tiny saída-manual exits for the
// reservations we reversed during the failed finalisation, and inserts new
// active rows in stock_reservations so the cart still owns the units. Errors
// here are logged but never returned — the caller's primary signal is the
// upstream createFinalERPOrder error, and we don't want to mask it. Even a
// partial success keeps more stock held than the alternative of releasing
// everything.
func (s *Service) reReserveAfterFailedFinalisation(ctx context.Context, erpProvider providers.ERPProvider, cartID, eventID string, snapshot []StockReservationRow) {
	if len(snapshot) == 0 {
		return
	}
	logger.From(ctx, s.logger).Warn("re-reserving stock after failed ERP finalisation",
		zap.String("cart_id", cartID),
		zap.Int("reservations_count", len(snapshot)),
	)
	restored := 0
	for _, r := range snapshot {
		obs := fmt.Sprintf("Re-reserva pós-falha de finalização - Cart %s", cartID)
		movementID, reserveErr := erpProvider.ReserveStock(ctx, r.ExternalProductID, r.Quantity, 0, obs)
		if reserveErr != nil {
			logger.From(ctx, s.logger).Error("failed to re-reserve stock after finalisation failure",
				zap.String("cart_id", cartID),
				zap.String("external_product_id", r.ExternalProductID),
				zap.Int("quantity", r.Quantity),
				zap.Error(reserveErr),
			)
			continue
		}
		if _, dbErr := s.repo.CreateStockReservation(ctx, CreateStockReservationParams{
			EventID:           eventID,
			CartID:            cartID,
			ProductID:         r.ProductID,
			ExternalProductID: r.ExternalProductID,
			Quantity:          r.Quantity,
			ERPMovementID:     movementID,
		}); dbErr != nil {
			// Tiny holds the stock, but our DB row failed. The reservation
			// is real on the merchant's side — flag loudly so we can
			// reconcile manually instead of silently losing the link.
			logger.From(ctx, s.logger).Error("re-reserved on Tiny but failed to persist reservation row",
				zap.String("cart_id", cartID),
				zap.String("external_product_id", r.ExternalProductID),
				zap.String("erp_movement_id", movementID),
				zap.Int("quantity", r.Quantity),
				zap.Error(dbErr),
			)
			continue
		}
		restored++
	}
	logger.From(ctx, s.logger).Info("re-reservation after failed ERP finalisation completed",
		zap.String("cart_id", cartID),
		zap.Int("requested", len(snapshot)),
		zap.Int("succeeded", restored),
	)
}

// RetryERPFinalisation is the admin-triggered retry of the post-payment ERP
// flow. Runs on 'failed' carts (replaying the persisted gateway snapshot) and
// — Fase 2 — also on 'pending' zombies: carts whose finalisation crashed
// mid-flight (Gap A/B). A pending cart is retryable when the Tiny order
// already exists (resume path) or when the last attempt is old/absent; a
// FRESH pending still gets a 422 because the initial flow is running right
// now (and the advisory lock would make this retry a no-op anyway).
func (s *Service) RetryERPFinalisation(ctx context.Context, cartID, storeID string) error {
	row, err := s.repo.GetCartERPFinalisationStatus(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart ERP status: %w", err)
	}
	switch row.Status {
	case "done":
		return nil
	case "pending":
		stale := row.LastAttemptAt == nil || time.Since(*row.LastAttemptAt) > 15*time.Minute
		if row.ExternalOrderID == "" && !stale {
			return httpx.ErrUnprocessable("aguarde a finalização inicial concluir antes de tentar de novo")
		}
	case "failed":
		// proceed
	default:
		return httpx.ErrUnprocessable("estado inválido para retry: " + row.Status)
	}

	// Replay the original gateway PaymentStatus snapshot we captured on the
	// first attempt (S1). The snapshot has the canonical fee/net/installments/
	// money-release values the order needs — re-fetching from the gateway
	// would work too but adds an external dependency to a manual action.
	if len(row.PaymentSnapshot) == 0 {
		if row.ExternalOrderID != "" {
			// Resume puro: o pedido já existe no Tiny, e launch + estornos não
			// usam dados de pagamento — dá para terminar sem snapshot (carts
			// zumbis pré-deploy caem aqui).
			return s.finalizeCartERPOrder(ctx, cartID, storeID, nil)
		}
		return httpx.ErrUnprocessable("snapshot de pagamento ausente — retry não disponível")
	}
	var status providers.PaymentStatus
	if err := json.Unmarshal(row.PaymentSnapshot, &status); err != nil {
		return fmt.Errorf("decoding payment snapshot: %w", err)
	}

	return s.finalizeCartERPOrder(ctx, cartID, storeID, &status)
}

// =============================================================================
// MELHOR ENVIO WEBHOOK
// =============================================================================

// ApplyMelhorEnvioWebhookInput is the normalised payload extracted from a
// melhor_envio webhook. The handler validates and parses; the service applies
// the side effects (status update + tracking event).
type ApplyMelhorEnvioWebhookInput struct {
	StoreID           string
	ProviderOrderID   string
	Event             string
	Status            string
	TrackingCode      string
	PublicTrackingURL string
	PostedAt          *string
	DeliveredAt       *string
	CanceledAt        *string
	ExpiredAt         *string
}

// ApplyMelhorEnvioWebhook updates the local shipment row to reflect a ME
// order.* webhook and appends a corresponding tracking event. No-op when the
// shipment isn't known locally — that happens when the ME account fires
// webhooks for orders created outside LiveCart and we don't want to leak
// errors back to the provider (which disables the webhook URL after enough
// failures).
func (s *Service) ApplyMelhorEnvioWebhook(ctx context.Context, in ApplyMelhorEnvioWebhookInput) error {
	if in.ProviderOrderID == "" {
		return nil
	}
	shipment, err := s.repo.GetShipmentByProviderOrderID(ctx, "melhor_envio", in.ProviderOrderID)
	if err != nil {
		return fmt.Errorf("looking up me shipment: %w", err)
	}
	if shipment == nil {
		logger.From(ctx, s.logger).Debug("melhor_envio webhook for unknown shipment — ignoring",
			zap.String("store_id", in.StoreID),
			zap.String("provider_order_id", in.ProviderOrderID),
		)
		return nil
	}

	normalised := mapMelhorEnvioWebhookStatus(in.Event, in.Status)

	// Persist the canonical status + tracking code on the parent row.
	if err := s.repo.UpdateShipmentStatus(ctx, shipment.ID, string(normalised), 0, in.Status, in.TrackingCode); err != nil {
		return fmt.Errorf("updating shipment status: %w", err)
	}
	// Mirror the public tracking URL when the carrier eventually exposes one
	// (it lands on order.posted).
	if in.PublicTrackingURL != "" || in.TrackingCode != "" {
		if err := s.repo.UpdateShipmentLabels(ctx, shipment.ID, shipment.LabelURL, in.PublicTrackingURL, in.TrackingCode); err != nil {
			logger.From(ctx, s.logger).Warn("failed to mirror tracking url onto shipment",
				zap.String("shipment_id", shipment.ID),
				zap.Error(err),
			)
		}
	}

	// Append the event to the timeline. We pick the most specific timestamp
	// available for the event so the FE timeline matches what the merchant
	// sees on the ME panel.
	eventAt := pickMelhorEnvioEventTime(in)
	if err := s.repo.InsertTrackingEvents(ctx, shipment.ID, []TrackingEventInput{{
		Status:      string(normalised),
		RawCode:     0,
		RawName:     in.Event,
		Observation: meWebhookObservation(in.Event),
		EventAt:     eventAt,
		Source:      "webhook",
	}}); err != nil {
		return fmt.Errorf("inserting tracking event: %w", err)
	}

	// Group H fact (best-effort): carrier-level status transition from a ME
	// webhook (posted/in_transit/out_for_delivery/returned/etc). Dedup by
	// shipment + normalised status so redelivered webhooks collapse per state.
	_ = events.EmitInternal(ctx, s.repo.queries, events.ShipmentStatusUpdated, "shipment.status_updated:"+shipment.ID+":"+string(normalised), struct {
		StoreID      string `json:"store_id"`
		ShipmentID   string `json:"shipment_id"`
		OrderID      string `json:"order_id"`
		Provider     string `json:"provider"`
		Status       string `json:"status"`
		TrackingCode string `json:"tracking_code"`
	}{StoreID: in.StoreID, ShipmentID: shipment.ID, OrderID: shipment.CartID, Provider: "melhor_envio", Status: string(normalised), TrackingCode: in.TrackingCode})

	// Group H fact (best-effort): delivery confirmed. Dedup by shipment id.
	if normalised == providers.TrackingStatusDelivered {
		_ = events.EmitInternal(ctx, s.repo.queries, events.DeliveryConfirmed, "shipment.delivered:"+shipment.ID, struct {
			StoreID      string `json:"store_id"`
			ShipmentID   string `json:"shipment_id"`
			OrderID      string `json:"order_id"`
			Provider     string `json:"provider"`
			TrackingCode string `json:"tracking_code"`
		}{StoreID: in.StoreID, ShipmentID: shipment.ID, OrderID: shipment.CartID, Provider: "melhor_envio", TrackingCode: in.TrackingCode})
	}

	// Customer-facing notification on first dispatch — same hook the manual
	// CreateShipment flow uses when the carrier returns the tracking inline.
	if normalised == providers.TrackingStatusInTransit && in.TrackingCode != "" && s.postCheckoutHook != nil {
		s.postCheckoutHook.OnShipmentPosted(ctx, shipment.CartID, in.TrackingCode)
	}
	return nil
}

func mapMelhorEnvioWebhookStatus(event, raw string) providers.TrackingStatus {
	switch event {
	case "order.posted":
		return providers.TrackingStatusInTransit
	case "order.delivered":
		return providers.TrackingStatusDelivered
	case "order.cancelled", "order.canceled":
		return providers.TrackingStatusCanceled
	case "order.undelivered":
		return providers.TrackingStatusNotDelivered
	case "order.suspended":
		return providers.TrackingStatusShipmentBlocked
	case "order.released":
		return providers.TrackingStatusAwaitingPickup
	case "order.created", "order.pending":
		return providers.TrackingStatusPending
	}
	// Fallback: trust the raw `status` field on the payload.
	return mapMelhorEnvioStatusRaw(raw)
}

func mapMelhorEnvioStatusRaw(raw string) providers.TrackingStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "pending":
		return providers.TrackingStatusPending
	case "released":
		return providers.TrackingStatusAwaitingPickup
	case "posted":
		return providers.TrackingStatusInTransit
	case "delivered":
		return providers.TrackingStatusDelivered
	case "cancelled", "canceled":
		return providers.TrackingStatusCanceled
	case "undelivered":
		return providers.TrackingStatusNotDelivered
	case "suspended":
		return providers.TrackingStatusShipmentBlocked
	default:
		return providers.TrackingStatusUnknown
	}
}

func pickMelhorEnvioEventTime(in ApplyMelhorEnvioWebhookInput) time.Time {
	candidates := []*string{in.DeliveredAt, in.CanceledAt, in.ExpiredAt, in.PostedAt}
	for _, c := range candidates {
		if c == nil || *c == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, *c); err == nil {
			return t
		}
	}
	return time.Now()
}

func meWebhookObservation(event string) string {
	switch event {
	case "order.created":
		return "Etiqueta gerada"
	case "order.released":
		return "Etiqueta paga"
	case "order.posted":
		return "Postado"
	case "order.delivered":
		return "Entregue"
	case "order.cancelled", "order.canceled":
		return "Cancelado"
	case "order.undelivered":
		return "Não entregue"
	case "order.suspended":
		return "Suspenso"
	case "order.pending":
		return "Aguardando pagamento"
	default:
		return event
	}
}

// =============================================================================
// CART NFe SYNC (ERP → carts.erp_invoice_*)
// =============================================================================

// CartInvoiceState is the normalised view of a cart's NFe used by the order
// detail handler and the manual-sync response. It mirrors providers.ERPInvoice
// without exposing provider-specific quirks.
type CartInvoiceState struct {
	InvoiceID  string
	InvoiceKey string
	Status     string // pending | authorized | cancelled | rejected | "" (none)
	EmittedAt  *time.Time
}

// SyncCartInvoiceFromERP pulls the NFe state for a paid cart from the active
// ERP integration (today: Tiny) and persists it on carts.erp_invoice_*.
//
// invoiceID is optional: when the caller already knows the ERP-side notafiscal
// id (e.g. from a webhook payload) we fetch by id; otherwise we ask the ERP
// for whatever NFe is attached to the order. Returns nil error when the
// merchant hasn't emitted the NFe yet — that's the "Aguardando NFe" branch
// on the frontend, surfaced via the absence of erp_invoice_* fields.
//
// Idempotent: re-running the same sync produces the same row state. Callers
// can hit it from the Tiny webhook handler, the manual "Verificar NFe" button,
// or a future poller without coordination.
//
// Implements order.CartInvoiceSyncer.
func (s *Service) SyncCartInvoiceFromERP(ctx context.Context, storeID, cartID, invoiceID string) error {
	_, err := s.fetchAndPersistCartInvoice(ctx, storeID, cartID, invoiceID)
	return err
}

// fetchAndPersistCartInvoice is the workhorse used by both the public
// SyncCartInvoiceFromERP entry point and SyncCartInvoiceByExternalOrder. The
// state it returns is consumed by the webhook path for logging, but not
// surfaced past the integration package boundary.
func (s *Service) fetchAndPersistCartInvoice(ctx context.Context, storeID, cartID, invoiceID string) (*CartInvoiceState, error) {
	cart, err := s.repo.GetCartForPaidOrder(ctx, cartID)
	if err != nil {
		return nil, fmt.Errorf("loading cart for invoice sync: %w", err)
	}
	if cart.StoreID != storeID {
		return nil, httpx.ErrNotFound("cart not found")
	}
	// Without an ERP order id we have no anchor on the Tiny side and nothing
	// to fetch. Returning nil lets the caller render "Aguardando criação no
	// ERP" without surfacing a generic error.
	if cart.ExternalOrderID == "" && invoiceID == "" {
		return nil, nil
	}

	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		return nil, httpx.ErrUnprocessable("ERP integration not active for store")
	}

	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return nil, fmt.Errorf("creating ERP provider: %w", err)
	}
	invoiceProvider, ok := provider.(providers.ERPInvoiceProvider)
	if !ok {
		return nil, httpx.ErrUnprocessable("ERP provider does not expose invoice operations")
	}

	var (
		invoice  *providers.ERPInvoice
		fetchErr error
	)
	if invoiceID != "" {
		invoice, fetchErr = invoiceProvider.GetInvoiceByID(ctx, invoiceID)
	} else {
		invoice, fetchErr = invoiceProvider.GetInvoiceByOrder(ctx, cart.ExternalOrderID)
	}
	if errors.Is(fetchErr, providers.ErrInvoiceNotFound) {
		// Tiny knows the order but no NFe is attached yet — merchant still
		// has to emit it in the ERP. Persist nothing and let the frontend
		// surface "Aguardando NFe".
		return nil, nil
	}
	if fetchErr != nil {
		s.handleProviderError(ctx, integration.ID, "sync_cart_invoice", fetchErr)
		return nil, fmt.Errorf("fetching NFe from ERP: %w", fetchErr)
	}

	// Fatia 11b: the NFe is written authoritatively to order_payments (resolved
	// from cart_id). 0 rows = no Order for this cart yet — a benign skip (NF is
	// always post-confirmation, so the Order should already exist; we log rather
	// than error so a stray webhook never dead-letters).
	rows, err := s.repo.UpsertCartERPInvoice(ctx, UpsertCartERPInvoiceParams{
		CartID:        cartID,
		InvoiceID:     invoice.InvoiceID,
		InvoiceKey:    invoice.AccessKey,
		InvoiceStatus: string(invoice.Status),
		EmittedAt:     invoiceTimePtr(invoice.IssuedAt),
	})
	if err != nil {
		return nil, fmt.Errorf("persisting order NFe: %w", err)
	}
	if rows == 0 {
		logger.From(ctx, s.logger).Warn("nota fiscal received for cart without a materialised order, skipping invoice persist",
			zap.String("cart_id", cartID),
			zap.String("external_order_id", cart.ExternalOrderID),
			zap.String("invoice_id", invoice.InvoiceID),
		)
	}

	// Mirror the chave on any existing shipment so the carrier provider can
	// pick it up the next time the merchant clicks "Anexar NFe" / generates a
	// label. We don't auto-call AttachInvoice on the carrier here because the
	// merchant-driven flow is explicit.
	if invoice.AccessKey != "" {
		if sh, _ := s.repo.GetShipmentByOrderID(ctx, cartID); sh != nil && sh.InvoiceKey == "" {
			if err := s.repo.UpdateShipmentInvoice(ctx, sh.ID, invoice.AccessKey, "nfe"); err != nil {
				logger.From(ctx, s.logger).Warn("failed to mirror NFe key onto existing shipment",
					zap.String("cart_id", cartID),
					zap.String("shipment_id", sh.ID),
					zap.Error(err),
				)
			}
		}
	}

	return &CartInvoiceState{
		InvoiceID:  invoice.InvoiceID,
		InvoiceKey: invoice.AccessKey,
		Status:     string(invoice.Status),
		EmittedAt:  invoiceTimePtr(invoice.IssuedAt),
	}, nil
}

// SyncCartInvoiceByExternalOrder is the webhook entry point: Tiny only sends
// the pedido id (and sometimes the notafiscal id) in nota_fiscal events, so
// we resolve the cart by external_order_id first, then delegate to the
// regular sync.
func (s *Service) SyncCartInvoiceByExternalOrder(ctx context.Context, storeID, externalOrderID, invoiceID string) (*CartInvoiceState, error) {
	cartID, err := s.repo.FindCartByExternalOrderID(ctx, externalOrderID, storeID)
	if err != nil {
		logger.From(ctx, s.logger).Debug("nota_fiscal webhook for unknown pedido — skipping",
			zap.String("store_id", storeID),
			zap.String("external_order_id", externalOrderID),
			zap.Error(err),
		)
		return nil, nil
	}
	return s.fetchAndPersistCartInvoice(ctx, storeID, cartID, invoiceID)
}

func invoiceTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// =============================================================================
// INSTAGRAM WEBHOOK OPERATIONS
// =============================================================================

// DispatchCommentReceived is the inbound edge of the inverted comment flow: the
// webhook validates and dispatches a canonical comment.received event (via the
// transactional outbox) instead of processing inline. The domain work runs in
// the event consumer, which calls ProcessInstagramComment. The origin
// (live/story/dm) travels as the event Source, keeping the event canonical.
//
// Idempotency: ProcessInstagramComment already dedups by platform_comment_id,
// so at-least-once redelivery is safe. dedup_key mirrors that for visibility.
func (s *Service) DispatchCommentReceived(ctx context.Context, input ProcessInstagramCommentInput, source events.Source) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("marshaling comment.received payload: %w", err)
	}
	tx, err := s.repo.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin comment.received tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	if err := events.Emit(ctx, s.repo.queries.WithTx(tx), events.Envelope{
		Name:     events.CommentReceived,
		Source:   source,
		DedupKey: "comment.received:" + input.CommentID,
		Metadata: map[string]string{"comment_id": input.CommentID, "media_id": input.MediaID},
		Payload:  payload,
	}); err != nil {
		return fmt.Errorf("emitting comment.received: %w", err)
	}
	return tx.Commit(ctx)
}

// ProcessInstagramComment processes a live comment from Instagram webhook.
// All comments are saved to DB. Purchase intents trigger stock check → cart or waitlist.
func (s *Service) ProcessInstagramComment(ctx context.Context, input ProcessInstagramCommentInput) error {
	logger.From(ctx, s.logger).Info("processing instagram comment",
		zap.String("account_id", input.AccountID),
		zap.String("media_id", input.MediaID),
		zap.String("comment_id", input.CommentID),
		zap.String("user_id", input.UserID),
		zap.String("username", input.Username),
		zap.String("text", input.Text),
	)

	// Idempotency guard: a comment can reach us from BOTH the real-time webhook
	// and the polling capture. Skip if we've already stored this comment id, so
	// we never create a duplicate cart for the same comment.
	if input.CommentID != "" {
		if exists, _ := s.repo.LiveCommentExistsByPlatformID(ctx, input.CommentID); exists {
			logger.From(ctx, s.logger).Info("comment already processed, skipping",
				zap.String("comment_id", input.CommentID),
			)
			return nil
		}
	}

	// Find live session by platform_live_id (media_id)
	session, err := s.liveService.GetSessionByPlatformLiveID(ctx, input.MediaID)
	if err != nil {
		return fmt.Errorf("finding live session: %w", err)
	}
	if session == nil {
		logger.From(ctx, s.logger).Warn("no active live session found for media_id",
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
		logger.From(ctx, s.logger).Warn("no active live event found for media_id",
			zap.String("media_id", input.MediaID),
		)
		return nil
	}

	// Store resolved (media_id → event): carry it on the ctx so every log below
	// gets store_id without manual fields. Slug lookup skipped on this hot path.
	ctx = logger.WithStore(ctx, event.StoreID, "")

	// Paywall (PRD 007): blocked stores stop creating carts from comments.
	// Existing checkouts and payment webhooks keep working elsewhere.
	if s.billingGate != nil && s.billingGate.IsStoreBlocked(ctx, event.StoreID) {
		logger.From(ctx, s.logger).Info("comment ignored: store subscription blocked",
			zap.String("comment_id", input.CommentID),
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
			logger.From(ctx, s.logger).Error("failed to store instagram webhook event",
				zap.String("comment_id", input.CommentID),
				zap.Error(err),
			)
			// Don't return error - continue processing the comment
		}
	}

	// Increment comment counter on session
	if err := s.repo.IncrementLiveSessionComments(ctx, session.ID); err != nil {
		logger.From(ctx, s.logger).Error("failed to increment comment counter",
			zap.String("session_id", session.ID),
			zap.Error(err),
		)
	}

	// Check if processing is paused
	if event.ProcessingPaused {
		logger.From(ctx, s.logger).Info("processing paused, storing comment only",
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
			logger.From(ctx, s.logger).Error("failed to save paused comment", zap.Error(err))
		}
		return nil
	}

	// Block list: if the merchant has blocked this handle, drop the comment
	// from the purchase flow. Still persist it with result='blocked' so the
	// live feed can show "ignorado" badge and the merchant can see that the
	// person is still trying to buy.
	blocked, blockErr := s.repo.IsHandleBlocked(ctx, event.StoreID, strings.ToLower(strings.TrimPrefix(strings.TrimSpace(input.Username), "@")))
	if blockErr != nil {
		logger.From(ctx, s.logger).Error("failed to check blocked handle, proceeding",
			zap.String("username", input.Username),
			zap.Error(blockErr),
		)
	} else if blocked {
		logger.From(ctx, s.logger).Info("comment from blocked handle ignored",
			zap.String("event_id", event.ID),
			zap.String("username", input.Username),
			zap.String("comment_id", input.CommentID),
		)
		_, err := s.repo.CreateLiveComment(ctx, CreateLiveCommentParams{
			SessionID:         session.ID,
			EventID:           event.ID,
			Platform:          "instagram",
			PlatformCommentID: input.CommentID,
			PlatformUserID:    input.UserID,
			PlatformHandle:    input.Username,
			Text:              input.Text,
			HasPurchaseIntent: false,
			Result:            "blocked",
		})
		if err != nil {
			logger.From(ctx, s.logger).Error("failed to save blocked comment", zap.Error(err))
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
			logger.From(ctx, s.logger).Info("no keyword match, trying active product fallback",
				zap.String("event_id", event.ID),
				zap.String("active_product_id", *event.CurrentActiveProductID),
			)
			product, _ = s.repo.GetProductByID(ctx, event.StoreID, *event.CurrentActiveProductID)
		}
	}

	// Post-commerce events (feed posts and Stories) apply their own rules: only
	// the selected products participate, a single-product promotion auto-adds on a
	// bare "EU QUERO", and an unavailable or ambiguous request gets a private
	// reply listing what's available. When fully handled here, persist and stop.
	if isPostCommerce(event.Type) && hasPurchaseIntent {
		// Window gates (no background job): a buyer who comments before the
		// event starts or after it ends gets a private reply explaining when,
		// instead of having a cart created.
		now := time.Now()
		if event.ScheduledAt != nil && now.Before(*event.ScheduledAt) {
			s.replyPostNotStarted(ctx, event, input, *event.ScheduledAt)
			s.savePostComment(ctx, session.ID, event.ID, input, "event_not_started")
			return nil
		}
		if event.Status == "ended" || (event.EndsAt != nil && now.After(*event.EndsAt)) {
			s.replyPostEnded(ctx, event, input)
			s.savePostComment(ctx, session.ID, event.ID, input, "event_ended")
			return nil
		}

		resolved, handled, resultLabel := s.resolvePostEventProduct(ctx, event, input, intent, product)
		if handled {
			s.savePostComment(ctx, session.ID, event.ID, input, resultLabel)
			return nil
		}
		product = resolved
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
		logger.From(ctx, s.logger).Error("failed to save live comment", zap.Error(err))
		// Continue processing even if save fails
	}

	// If no purchase intent or no product match, we're done
	if !hasPurchaseIntent || product == nil {
		return nil
	}

	logger.From(ctx, s.logger).Info("purchase intent detected with product match",
		zap.String("username", input.Username),
		zap.String("product_id", product.ID),
		zap.String("keyword", product.Keyword),
		zap.Int("quantity", intent.Quantity),
		zap.Int("stock", product.Stock),
	)

	// A expiração de carrinhos deixou de ser lazy (por-comentário, por-produto):
	// era cega a carts 'checkout' pós-live e corria com o webhook de pagamento
	// sem lock. Agora a schedule asynq (cart.expire → RunScheduledExpiry →
	// ExpireCart) expira por-cart, com advisory lock, filtro de pago e devolução
	// de TODOS os itens.
	// A promoção de waitlist do produto liberado passou a ser disparada pelo
	// próprio worker; aqui não é mais necessário nada.

	// Validate maxQuantityPerItem limit
	storeInfo, _ := s.repo.GetStoreInfo(ctx, event.StoreID)
	if storeInfo != nil && storeInfo.MaxQuantityPerItem > 0 {
		currentQty, _ := s.repo.GetProductQuantityInUserCart(ctx, event.ID, input.UserID, product.ID)
		maxAllowed := storeInfo.MaxQuantityPerItem

		if currentQty >= maxAllowed {
			// User already has max quantity, ignore this request
			logger.From(ctx, s.logger).Info("user already at max quantity for product, ignoring",
				zap.String("username", input.Username),
				zap.String("product_id", product.ID),
				zap.Int("current_qty", currentQty),
				zap.Int("max_allowed", maxAllowed),
			)
			if commentID != "" {
				_ = s.repo.UpdateLiveCommentResult(ctx, commentID, false, product.ID, intent.Quantity, "max_quantity_reached")
			}
			// Send reply notifying user they've reached the limit.
			// Detached goroutine: never carry the (recyclable) request ctx —
			// hand it a Background ctx enriched with the store instead.
			go s.sendMaxQuantityReply(logger.WithStore(context.Background(), event.StoreID, ""), event.StoreID, input.Channel, input.CommentID, input.UserID, input.Username, product.Name, maxAllowed, true)
			return nil
		}

		// Cap quantity to remaining allowed
		remaining := maxAllowed - currentQty
		if intent.Quantity > remaining {
			logger.From(ctx, s.logger).Info("capping quantity to max allowed",
				zap.String("username", input.Username),
				zap.String("product_id", product.ID),
				zap.Int("requested", intent.Quantity),
				zap.Int("capped_to", remaining),
			)
			// Send reply notifying user their quantity was capped.
			// Detached goroutine: same ctx rule as above.
			go s.sendMaxQuantityReply(logger.WithStore(context.Background(), event.StoreID, ""), event.StoreID, input.Channel, input.CommentID, input.UserID, input.Username, product.Name, maxAllowed, false)
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

	// Reserve available stock (provisional: rolled back below if AddToCart
	// fails). stock.reserved is emitted only after the add succeeds, with the
	// real cart_id — see NoteReserved after AddToCart.
	if availableQty > 0 {
		if stockErr := s.repo.DecrementProductStock(ctx, product.ID, availableQty); stockErr != nil {
			// Failed to reserve even available stock - put all in waitlist
			logger.From(ctx, s.logger).Warn("failed to decrement stock, putting all in waitlist",
				zap.Error(stockErr),
				zap.Int("attempted", availableQty),
			)
			availableQty = 0
			waitlistQty = intent.Quantity
		}
	}

	// Handle waitlist gating: if user already has a row, skip the waitlist
	// portion (we don't double-queue) and either return early or fall back
	// to adding only the available portion to the cart.
	createWaitlistRow := false
	var waitlistPosition int
	if waitlistQty > 0 {
		alreadyWaiting, _ := s.repo.GetWaitlistItemByEventUserProduct(ctx, event.ID, input.UserID, product.ID)
		if alreadyWaiting {
			logger.From(ctx, s.logger).Info("user already on waitlist, ignoring waitlist portion",
				zap.String("username", input.Username),
				zap.String("product_id", product.ID),
				zap.Int("waitlist_qty", waitlistQty),
			)
			if availableQty == 0 {
				if commentID != "" {
					_ = s.repo.UpdateLiveCommentResult(ctx, commentID, true, product.ID, intent.Quantity, "already_waitlisted")
				}
				return nil
			}
			waitlistQty = 0
		} else {
			// Defer the actual INSERT to after AddToCart so we can stamp
			// cart_id on the row (the public checkout lists waitlist items
			// by cart_id). Position is read here to keep ordering stable
			// even if two intents race on the same event+product.
			waitlistPosition, _ = s.repo.GetNextWaitlistPosition(ctx, event.ID, product.ID)
			createWaitlistRow = true
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
		StoreID:            event.StoreID,
		EventID:            event.ID,
		SessionID:          session.ID,
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

	// Reservation is now definitive — emit stock.reserved keyed by the real cart.
	if availableQty > 0 {
		s.stock.NoteReserved(ctx, ReserveParams{Op: StockOpCartAdd, ProductID: product.ID, Quantity: availableQty, CartID: result.CartID, EventID: event.ID})
	}

	// Persist the waitlist row now that we have the cart_id from AddToCart.
	if createWaitlistRow {
		if _, wlErr := s.repo.CreateWaitlistItem(ctx, CreateWaitlistItemParams{
			EventID:        event.ID,
			ProductID:      product.ID,
			PlatformUserID: input.UserID,
			PlatformHandle: input.Username,
			Quantity:       waitlistQty,
			Position:       waitlistPosition,
			CartID:         result.CartID,
		}); wlErr != nil {
			logger.From(ctx, s.logger).Error("failed to create waitlist item", zap.Error(wlErr))
		} else {
			logger.From(ctx, s.logger).Info("user added to waitlist (partial fulfillment)",
				zap.String("username", input.Username),
				zap.String("product_id", product.ID),
				zap.String("cart_id", result.CartID),
				zap.Int("available_qty", availableQty),
				zap.Int("waitlist_qty", waitlistQty),
				zap.Int("position", waitlistPosition),
			)
		}
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
			logger.From(ctx, s.logger).Error("failed to increment order counter",
				zap.String("event_id", event.ID),
				zap.Error(err),
			)
		}
	}

	// Reserve stock in ERP (only for available items)
	if availableQty > 0 {
		if syncErr := s.ReserveStockInERP(ctx, event.StoreID, result.CartID, event.ID, product.ID, availableQty, product.Price, input.Username); syncErr != nil {
			logger.From(ctx, s.logger).Warn("failed to reserve stock in ERP",
				zap.String("cart_id", result.CartID),
				zap.Error(syncErr),
			)
		}
	}

	// Send immediate notification (fire-and-forget, doesn't block the flow). For
	// story replies (Channel="dm") there is no comment to reply on, so we clear
	// the comment id — the notification service then delivers straight via DM to
	// the buyer's IGSID.
	notifyCommentID := input.CommentID
	if input.Channel == "dm" {
		notifyCommentID = ""
	}
	s.sendImmediateNotification(ctx, sendNotificationInput{
		StoreID:           event.StoreID,
		EventID:           event.ID,
		EventTitle:        event.Title,
		CartID:            result.CartID,
		CartToken:         result.CartToken,
		PlatformUserID:    input.UserID,
		PlatformHandle:    input.Username,
		PlatformCommentID: notifyCommentID,
		ProductName:       product.Name,
		ProductKeyword:    product.Keyword,
		Quantity:          intent.Quantity,
		TotalItems:        result.TotalItems,
		TotalCents:        result.TotalCents,
		IsNewCart:         result.IsNewCart,
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
		logger.From(ctx, s.logger).Warn("failed to check notification settings",
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
		logger.From(ctx, s.logger).Warn("failed to get store info for notification",
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
		logger.From(ctx, s.logger).Warn("notification send error",
			zap.String("cart_id", input.CartID),
			zap.Error(err),
		)
		return
	}

	logger.From(ctx, s.logger).Info("immediate notification processed",
		zap.String("cart_id", input.CartID),
		zap.String("status", string(result.Status)),
		zap.Bool("is_new_cart", input.IsNewCart),
	)
}

// sendMaxQuantityReply sends a reply to the user when they've reached or exceeded the max quantity limit.
// This is fire-and-forget - errors are logged but don't affect the main flow.
// isAtLimit: true = already at limit (rejected), false = quantity was capped
func (s *Service) sendMaxQuantityReply(ctx context.Context, storeID, channel, commentID, userID, username, productName string, maxAllowed int, isAtLimit bool) {
	var message string
	if isAtLimit {
		message = fmt.Sprintf("Oi @%s! Você já atingiu o limite de %d unidades de %s. 🛒", username, maxAllowed, productName)
	} else {
		message = fmt.Sprintf("Oi @%s! Adicionei o máximo permitido (%d unidades) de %s ao seu carrinho. 🛒", username, maxAllowed, productName)
	}

	// Story replies have no comment to answer — DM the buyer directly.
	if channel == "dm" {
		if dmErr := s.SendInstagramDM(ctx, storeID, userID, message); dmErr != nil {
			logger.From(ctx, s.logger).Warn("failed to send max quantity DM",
				zap.String("user_id", userID), zap.Error(dmErr))
		}
		return
	}

	if commentID == "" {
		return
	}

	// Try comment reply first, then DM fallback
	err := s.ReplyToInstagramComment(ctx, storeID, commentID, message)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to send max quantity reply via comment, trying DM",
			zap.String("comment_id", commentID),
			zap.Error(err),
		)
		// Fallback to DM
		if dmErr := s.SendInstagramDM(ctx, storeID, userID, message); dmErr != nil {
			logger.From(ctx, s.logger).Warn("failed to send max quantity DM",
				zap.String("user_id", userID),
				zap.Error(dmErr),
			)
		}
	}

	logger.From(ctx, s.logger).Info("max quantity reply sent",
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
			logger.From(ctx, s.logger).Error("failed to lookup product by keyword",
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
	logger.From(ctx, s.logger).Info("processing instagram message",
		zap.String("account_id", input.AccountID),
		zap.String("sender_id", input.SenderID),
		zap.String("message_id", input.MessageID),
		zap.String("text", input.Text),
		zap.String("reply_to_story_id", input.ReplyToStoryID),
		zap.Bool("is_echo", input.IsEcho),
	)

	// Skip echoes of our own outbound messages.
	if input.IsEcho {
		return nil
	}

	// Story-commerce: a DM that replies to one of our published Stories carries
	// reply_to.story.id. Resolve the store from the story EVENT (not the account
	// lookup, which the comment/live paths also avoid) and feed it into the same
	// intent→cart pipeline as comments, answering via DM.
	if input.ReplyToStoryID != "" {
		if err := s.processStoryReply(ctx, input); err != nil {
			logger.From(ctx, s.logger).Warn("failed to process story reply",
				zap.String("story_id", input.ReplyToStoryID),
				zap.Error(err))
		}
		return nil
	}

	// Non-story DM: resolve the store from the Instagram account ID for audit +
	// the "Testar notificação" setup capture.
	integration, err := s.repo.GetByInstagramUserID(ctx, input.AccountID)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to find integration by instagram account",
			zap.String("account_id", input.AccountID),
			zap.Error(err),
		)
		return nil // Don't fail the webhook, just skip storage
	}
	if integration == nil {
		logger.From(ctx, s.logger).Warn("no integration found for instagram account",
			zap.String("account_id", input.AccountID),
		)
		return nil
	}

	// Store resolved (account_id → integration): enrich the ctx for the logs below.
	ctx = logger.WithStore(ctx, integration.StoreID, "")

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
			logger.From(ctx, s.logger).Error("failed to store instagram dm webhook event",
				zap.String("message_id", input.MessageID),
				zap.Error(err),
			)
			// Don't return error - continue processing
		}
	}

	// If the message text matches an active "Testar notificação" setup code,
	// capture this sender as the store's test recipient. We swallow errors
	// here because a webhook should never fail on optional bookkeeping.
	if s.notificationService != nil && input.Text != "" {
		storeID, setupErr := s.notificationService.CompleteTestRecipientSetup(ctx, input.Text, input.SenderID, "")
		if setupErr != nil {
			logger.From(ctx, s.logger).Warn("failed to complete test recipient setup",
				zap.String("account_id", input.AccountID),
				zap.Error(setupErr),
			)
		} else if storeID != "" {
			logger.From(ctx, s.logger).Info("test recipient configured",
				zap.String("store_id", storeID),
				zap.String("sender_id", input.SenderID),
			)
		}
	}

	return nil
}

// processStoryReply turns a DM reply to a published Story into a purchase intent.
// It resolves the bound story-commerce event by the replied-to story media id,
// best-effort resolves the buyer's @handle, then reuses the comment→cart
// pipeline with the DM reply channel (answers go straight to the buyer's DM).
func (s *Service) processStoryReply(ctx context.Context, input ProcessInstagramMessageInput) error {
	// Only act on replies to one of our story-commerce events. The event carries
	// the store, so we don't need the (separate, sometimes-missing) account lookup.
	event, err := s.liveService.GetEventByPlatformLiveID(ctx, input.ReplyToStoryID)
	if err != nil {
		return fmt.Errorf("resolving story event: %w", err)
	}
	if event == nil || event.Type != "story" {
		// Reply to a non-commerce story (or unknown) — nothing to do.
		logger.From(ctx, s.logger).Info("story reply ignored: no matching story event",
			zap.String("story_id", input.ReplyToStoryID))
		return nil
	}
	storeID := event.StoreID
	// Store resolved (reply_to_story → event): enrich the ctx for the logs below.
	ctx = logger.WithStore(ctx, storeID, "")

	// Resolve the buyer's @handle from their IGSID (best-effort — the DM still
	// works without it; we just fall back to a generic label).
	username := ""
	if provider, perr := s.resolveInstagramSocialProvider(ctx, storeID); perr == nil {
		if uname, uerr := provider.GetUsername(ctx, input.SenderID); uerr == nil {
			username = uname
		} else {
			logger.From(ctx, s.logger).Warn("failed to resolve story buyer username",
				zap.String("sender_id", input.SenderID), zap.Error(uerr))
		}
	}
	if username == "" {
		username = "cliente"
	}

	// Inverted flow: dispatch canonical comment.received with the story origin.
	// Channel="dm" is preserved in the payload so the consumer replies via DM.
	return s.DispatchCommentReceived(ctx, ProcessInstagramCommentInput{
		AccountID:  input.AccountID,
		MediaID:    input.ReplyToStoryID,
		CommentID:  input.MessageID, // DM mid — dedup key
		UserID:     input.SenderID,  // IGSID — DM recipient
		Username:   username,
		Text:       input.Text,
		Timestamp:  input.Timestamp,
		Channel:    "dm",
		RawPayload: input.RawPayload,
	}, events.SourceInstagramStory)
}

// MarkPostEventWebhookActive flags the post event mapped to mediaID as
// webhook-driven, so the polling capture stops. Delegates to the live service.
func (s *Service) MarkPostEventWebhookActive(ctx context.Context, mediaID string) error {
	return s.liveService.MarkPostEventWebhookActive(ctx, mediaID)
}

// StartPostCommentPolling launches a background loop that captures comments on
// active post events until the real-time `comments` webhook takes over. The
// loop stops when ctx is cancelled.
func (s *Service) StartPostCommentPolling(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		logger.From(ctx, s.logger).Info("post-comment polling started")
		for {
			select {
			case <-ctx.Done():
				logger.From(ctx, s.logger).Info("post-comment polling stopped")
				return
			case <-ticker.C:
				s.pollPostCommentsOnce(ctx)
			}
		}
	}()
}

// isMediaGoneError reports whether an Instagram Graph error means the media is
// permanently unreachable for us (deleted or no longer accessible) rather than a
// transient failure. Graph signals this with code 100 / subcode 33 and the
// "does not exist, cannot be loaded due to missing permissions" message.
// isPostCommerce reports whether an event type uses the post-commerce intent
// rules (whitelisted products, single-product auto-add, window gates). Both feed
// posts and Stories share these rules — only the reply channel differs.
func isPostCommerce(eventType string) bool {
	return eventType == "post" || eventType == "story"
}

func isMediaGoneError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "error_subcode\":33") ||
		strings.Contains(msg, "does not exist, cannot be loaded")
}

// pollPostCommentsOnce processes one capture pass over active post events that
// are not yet webhook-driven. New comments (deduped by platform_comment_id) are
// fed into the same ProcessInstagramComment path used by the webhook.
func (s *Service) pollPostCommentsOnce(ctx context.Context) {
	events, err := s.liveService.ListActivePostEvents(ctx)
	if err != nil {
		logger.From(ctx, s.logger).Warn("post polling: failed to list active post events", zap.Error(err))
		return
	}
	for _, ev := range events {
		if ev.MediaID == "" {
			continue
		}
		// The polling loop runs on the app-level ctx (no store): enrich a
		// per-event ctx with the store each event belongs to.
		evCtx := logger.WithStore(ctx, ev.StoreID, "")
		provider, err := s.resolveInstagramSocialProvider(evCtx, ev.StoreID)
		if err != nil {
			continue
		}
		comments, err := provider.GetMediaComments(evCtx, ev.MediaID)
		if err != nil {
			// Media gone (deleted / no longer accessible): close the event so we
			// stop hammering a dead media id every tick instead of warning forever.
			if isMediaGoneError(err) {
				if endErr := s.liveService.EndPostEventByMediaID(evCtx, ev.MediaID); endErr != nil {
					logger.From(evCtx, s.logger).Warn("post polling: failed to end event for missing media",
						zap.String("media_id", ev.MediaID), zap.Error(endErr))
				} else {
					logger.From(evCtx, s.logger).Info("post polling: ended event, media no longer accessible",
						zap.String("media_id", ev.MediaID))
				}
				continue
			}
			logger.From(evCtx, s.logger).Warn("post polling: failed to fetch comments",
				zap.String("media_id", ev.MediaID), zap.Error(err))
			continue
		}
		for _, c := range comments {
			if c.ID == "" {
				continue
			}
			exists, _ := s.repo.LiveCommentExistsByPlatformID(evCtx, c.ID)
			if exists {
				continue
			}
			username := c.From.Username
			if username == "" {
				username = c.Username
			}
			if err := s.ProcessInstagramComment(evCtx, ProcessInstagramCommentInput{
				MediaID:   ev.MediaID,
				CommentID: c.ID,
				UserID:    c.From.ID,
				Username:  username,
				Text:      c.Text,
			}); err != nil {
				logger.From(evCtx, s.logger).Warn("post polling: failed to process comment",
					zap.String("comment_id", c.ID), zap.Error(err))
			}
		}
	}
}

// =============================================================================
// POST-COMMERCE COMMENT RULES
// =============================================================================

// resolvePostEventProduct applies post-event rules. It returns the product to
// add (resolved from a single-product promotion when the comment is a bare
// "EU QUERO"), and handled=true when it already answered the commenter (product
// not in the promotion, or ambiguous request), in which case the caller saves
// the comment with resultLabel and stops.
func (s *Service) resolvePostEventProduct(
	ctx context.Context,
	event *live.EventOutput,
	input ProcessInstagramCommentInput,
	intent *PurchaseIntent,
	matched *ProductRow,
) (resolved *ProductRow, handled bool, resultLabel string) {
	whitelist, err := s.liveService.ListEventProducts(ctx, event.ID, event.StoreID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to load post promotion products", zap.Error(err))
		return matched, false, ""
	}

	inPromo := func(productID string) bool {
		for _, w := range whitelist {
			if w.ProductID == productID {
				return true
			}
		}
		return false
	}

	// Case A: a code matched a real product.
	if matched != nil {
		if inPromo(matched.ID) {
			if matched.Stock <= 0 {
				s.replyPostOutOfStock(ctx, event, input, matched.Name, whitelist)
				return nil, true, "out_of_stock"
			}
			return matched, false, "" // proceed to the normal cart flow
		}
		s.replyPostUnavailable(ctx, event, input, whitelist)
		return nil, true, "not_in_promo"
	}

	// Case B: no product matched. A typed-but-unknown code is "unavailable";
	// a bare trigger with one promo product auto-adds it, with many it asks.
	if codes := ExtractPossibleKeywords(input.Text); len(codes) > 0 {
		s.replyPostUnavailable(ctx, event, input, whitelist)
		return nil, true, "not_in_promo"
	}

	available := availablePromoProducts(whitelist)
	switch len(available) {
	case 1:
		p, err := s.repo.GetProductByID(ctx, event.StoreID, available[0].ProductID)
		if err == nil && p != nil {
			return p, false, ""
		}
		return nil, true, "no_product"
	case 0:
		// No available products: if the promotion has products but all are out
		// of stock, tell the buyer; otherwise stay silent.
		if len(whitelist) > 0 {
			s.replyPostOutOfStock(ctx, event, input, "", whitelist)
			return nil, true, "out_of_stock"
		}
		return nil, true, "no_product"
	default:
		s.replyPostChooseProduct(ctx, event, input, available)
		return nil, true, "needs_keyword"
	}
}

// savePostComment persists a post comment that was fully handled by the rules.
func (s *Service) savePostComment(ctx context.Context, sessionID, eventID string, input ProcessInstagramCommentInput, result string) {
	if _, err := s.repo.CreateLiveComment(ctx, CreateLiveCommentParams{
		SessionID:         sessionID,
		EventID:           eventID,
		Platform:          "instagram",
		PlatformCommentID: input.CommentID,
		PlatformUserID:    input.UserID,
		PlatformHandle:    input.Username,
		Text:              input.Text,
		HasPurchaseIntent: true,
		Result:            result,
	}); err != nil {
		logger.From(ctx, s.logger).Error("failed to save post comment", zap.Error(err))
	}
}

// replyPostNotStarted privately tells the buyer the promotion hasn't started and
// when it will (formatted in Brazil time, UTC-3).
func (s *Service) replyPostNotStarted(ctx context.Context, event *live.EventOutput, input ProcessInstagramCommentInput, startsAt time.Time) {
	msg := fmt.Sprintf(
		"Oi @%s! Esta promoção ainda não começou. 🗓️\nEla começa em %s. Volte lá pra garantir o seu! 💜",
		input.Username, live.FormatBRT(startsAt),
	)
	s.sendPostReply(ctx, event, input, msg)
}

// replyPostEnded privately tells the buyer the promotion has ended.
func (s *Service) replyPostEnded(ctx context.Context, event *live.EventOutput, input ProcessInstagramCommentInput) {
	msg := fmt.Sprintf("Oi @%s! Esta promoção já foi encerrada. 😕 Fique de olho que logo teremos novidades! 💜", input.Username)
	s.sendPostReply(ctx, event, input, msg)
}

// replyPostOutOfStock privately tells the buyer the product is sold out and
// lists what's still available (when there is anything).
func (s *Service) replyPostOutOfStock(ctx context.Context, event *live.EventOutput, input ProcessInstagramCommentInput, productName string, whitelist []live.EventProductOutput) {
	available := availablePromoProducts(whitelist)
	var msg string
	switch {
	case productName != "" && len(available) > 0:
		msg = fmt.Sprintf("Oi @%s! O produto %s esgotou. 😕\nAinda temos:\n%s\n\nComente o código do que você quer. 💜", input.Username, productName, promoProductLines(available))
	case productName != "":
		msg = fmt.Sprintf("Oi @%s! O produto %s esgotou. 😕", input.Username, productName)
	default:
		msg = fmt.Sprintf("Oi @%s! Os produtos desta promoção esgotaram. 😕 Fique de olho nas próximas! 💜", input.Username)
	}
	s.sendPostReply(ctx, event, input, msg)
}

// sendPostReply privately answers the buyer. For a comment-channel event it
// replies on the comment thread (which Instagram delivers as a private reply);
// for a story (Channel="dm") it messages the buyer's IGSID directly, since a
// story reply arrives as a DM and has no public comment to answer.
func (s *Service) sendPostReply(ctx context.Context, event *live.EventOutput, input ProcessInstagramCommentInput, msg string) {
	if input.Channel == "dm" {
		if err := s.SendInstagramDM(ctx, event.StoreID, input.UserID, msg); err != nil {
			logger.From(ctx, s.logger).Warn("failed to send story DM reply",
				zap.String("event_id", event.ID),
				zap.String("user_id", input.UserID),
				zap.Error(err),
			)
		}
		return
	}
	if err := s.ReplyToInstagramComment(ctx, event.StoreID, input.CommentID, msg); err != nil {
		logger.From(ctx, s.logger).Warn("failed to send post reply",
			zap.String("event_id", event.ID),
			zap.String("comment_id", input.CommentID),
			zap.Error(err),
		)
	}
}

// availablePromoProducts filters the promotion to active, in-stock products.
func availablePromoProducts(whitelist []live.EventProductOutput) []live.EventProductOutput {
	out := make([]live.EventProductOutput, 0, len(whitelist))
	for _, w := range whitelist {
		if w.ProductActive && w.Stock > 0 {
			out = append(out, w)
		}
	}
	return out
}

// promoProductLines renders "• CODE — Name" lines for a list of products.
func promoProductLines(products []live.EventProductOutput) string {
	var b strings.Builder
	for _, p := range products {
		b.WriteString(fmt.Sprintf("• %s — %s\n", p.Keyword, p.Name))
	}
	return strings.TrimRight(b.String(), "\n")
}

// replyPostUnavailable privately tells the commenter the product isn't in this
// promotion and lists what is available.
func (s *Service) replyPostUnavailable(ctx context.Context, event *live.EventOutput, input ProcessInstagramCommentInput, whitelist []live.EventProductOutput) {
	available := availablePromoProducts(whitelist)
	var msg string
	if len(available) == 0 {
		msg = fmt.Sprintf("Oi @%s! Esse produto não está disponível nesta promoção no momento. 😕", input.Username)
	} else {
		msg = fmt.Sprintf(
			"Oi @%s! Esse produto não está disponível nesta promoção. 😕\nDisponíveis nesta publicação:\n%s\n\nComente o código do produto que você quer. 💜",
			input.Username, promoProductLines(available),
		)
	}
	s.sendPostReply(ctx, event, input, msg)
}

// replyPostChooseProduct privately asks the commenter to specify which product
// (used when a bare "EU QUERO" is posted on a multi-product promotion).
func (s *Service) replyPostChooseProduct(ctx context.Context, event *live.EventOutput, input ProcessInstagramCommentInput, available []live.EventProductOutput) {
	msg := fmt.Sprintf(
		"Oi @%s! Pra adicionar ao carrinho, comente o código do produto que você quer:\n%s 💜",
		input.Username, promoProductLines(available),
	)
	s.sendPostReply(ctx, event, input, msg)
}

// =============================================================================
// CART → ERP STOCK RESERVATION
// =============================================================================

// ReserveStockInERP creates a manual stock exit (tipo S) in the ERP for a product
// added to a cart. The movement is tracked in stock_reservations for later reversal.
func (s *Service) ReserveStockInERP(ctx context.Context, storeID, cartID, eventID, productID string, quantity int, unitPrice int64, platformHandle string) error {
	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		logger.From(ctx, s.logger).Debug("no active ERP integration, skipping stock reservation",
			zap.String("store_id", storeID),
		)
		return nil
	}

	// Cart convertido em pedido-como-reserva (design C): quem segura a peça é
	// o PEDIDO, não saídas manuais — a grade nova (o caller já gravou o item
	// no cart) entra pelo ciclo estornar→PUT→lançar. Cobre o live-add pós-pix
	// e a promoção de waitlist de cart convertido num único ponto.
	if st, stErr := s.repo.GetCartERPOrderState(ctx, cartID); stErr == nil &&
		st.State != erpOrderStateNone && st.State != erpOrderStateCancelled {
		if mutErr := s.MutateERPOrderItems(ctx, cartID, storeID); mutErr != nil {
			return fmt.Errorf("applying grid to converted cart order: %w", mutErr)
		}
		return nil
	}

	erpProvider, err := s.erpProviderFor(ctx, integration)
	if err != nil {
		return fmt.Errorf("creating ERP provider: %w", err)
	}

	// Get external product ID
	if s.productSyncer == nil {
		return nil
	}
	externalID, _, err := s.productSyncer.GetProduct(ctx, storeID, productID)
	if err != nil || externalID == "" {
		logger.From(ctx, s.logger).Debug("product not linked to ERP, skipping stock reservation",
			zap.String("product_id", productID),
		)
		return nil
	}

	// Idempotency: check if an active reservation already exists for this cart+product
	existing, _ := s.repo.ListActiveReservationsByCartAndProduct(ctx, cartID, productID)
	if len(existing) > 0 {
		logger.From(ctx, s.logger).Debug("stock reservation already exists for cart+product, skipping",
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
		logger.From(ctx, s.logger).Error("failed to save stock reservation, attempting ERP reversal",
			zap.String("cart_id", cartID),
			zap.String("product_id", productID),
			zap.String("erp_movement_id", movementID),
			zap.Error(err),
		)
		reverseObs := fmt.Sprintf("Estorno compensatório - falha DB - Cart %s", cartID)
		if _, reverseErr := erpProvider.ReverseStockReservation(ctx, externalID, quantity, 0, reverseObs); reverseErr != nil {
			logger.From(ctx, s.logger).Error("CRITICAL: failed to compensate ERP stock after DB failure — manual reconciliation required",
				zap.String("external_product_id", externalID),
				zap.Int("quantity", quantity),
				zap.String("erp_movement_id", movementID),
				zap.Error(reverseErr),
			)
		}
		return fmt.Errorf("saving stock reservation: %w", err)
	}

	logger.From(ctx, s.logger).Info("stock reserved in ERP",
		zap.String("cart_id", cartID),
		zap.String("product_id", productID),
		zap.String("external_product_id", externalID),
		zap.Int("quantity", quantity),
		zap.String("erp_movement_id", movementID),
	)

	return nil
}

// AdjustStockReservationDelta applies a quantity delta (positive or negative)
// to a (cart, product) reservation. It mutates both the local products.stock
// counter and the ERP reservation in a single call so the two stay in sync.
//
// Local stock is the source-of-truth gate for waitlist promotion
// (ProcessWaitlistForProduct's atomic DecrementProductStock) and live-add
// availability (processLiveAdd reads product.Stock). It is mutated FIRST so
// that even stores without an ERP integration get correct waitlist behavior.
// A failure in the optional ERP sync rolls the local mutation back.
//
// On delta > 0, an insufficient-stock condition returns httpx 422 instead of
// silently over-allocating (the previous behavior caused buyers reducing then
// re-increasing their cart to over-allocate against the original stock).
//
// Returns the ERP movement_id (empty when no ERP integration is configured or
// the product is not linked — both treated as no-ops for the ERP side; local
// stock is still updated).
// op labels the emitted stock event; pass StockOpUnspecified to use the default
// sign-based label (qty_increase / qty_decrease), or a specific op (e.g.
// waitlist_cancel / waitlist_expire) when the delta represents a domain action
// other than a buyer quantity edit.
func (s *Service) AdjustStockReservationDelta(ctx context.Context, storeID, cartID, eventID, productID string, delta int, unitPrice int64, platformHandle string, op StockOp) (string, error) {
	if delta == 0 {
		return "", nil
	}

	// 1. Local stock mutation — atomic gate for delta>0, mirror of the ERP
	//    reversal for delta<0. Runs unconditionally so waitlist promotion sees
	//    freed units immediately, even when the store has no ERP integration.
	if delta > 0 {
		if err := s.repo.DecrementProductStock(ctx, productID, delta); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", httpx.ErrUnprocessable("estoque insuficiente para esse aumento")
			}
			return "", fmt.Errorf("decrementing local stock: %w", err)
		}
	} else {
		if err := s.repo.IncrementProductStock(ctx, productID, -delta); err != nil {
			return "", fmt.Errorf("releasing local stock: %w", err)
		}
	}

	// localCommitted tracks whether the local stock mutation above stands. It is
	// cleared by rollbackLocal so the deferred emit below fires ONLY on the
	// definitive success of this operation (every rollback path returns an error).
	localCommitted := true

	// Rollback helper used when ERP sync fails after local stock already moved.
	rollbackLocal := func() {
		localCommitted = false
		if delta > 0 {
			if err := s.repo.IncrementProductStock(ctx, productID, delta); err != nil {
				logger.From(ctx, s.logger).Error("failed to rollback local stock decrement after ERP failure",
					zap.String("product_id", productID),
					zap.Int("delta", delta),
					zap.Error(err),
				)
			}
		} else {
			if err := s.repo.DecrementProductStock(ctx, productID, -delta); err != nil {
				logger.From(ctx, s.logger).Error("failed to rollback local stock increment after ERP failure",
					zap.String("product_id", productID),
					zap.Int("delta", delta),
					zap.Error(err),
				)
			}
		}
	}

	// stock.reserved / stock.released — the single emit point for this operation.
	// Deferred + guarded so every nil-error return below funnels through here
	// without instrumenting each one, and rollbacks (which clear localCommitted)
	// stay silent.
	defer func() {
		if !localCommitted {
			return
		}
		if delta > 0 {
			reserveOp := op
			if reserveOp == StockOpUnspecified {
				reserveOp = StockOpQtyIncrease
			}
			s.stock.NoteReserved(ctx, ReserveParams{Op: reserveOp, ProductID: productID, Quantity: delta, CartID: cartID, EventID: eventID})
		} else {
			releaseOp := op
			if releaseOp == StockOpUnspecified {
				releaseOp = StockOpQtyDecrease
			}
			s.stock.NoteReleased(ctx, ReleaseParams{Op: releaseOp, ProductID: productID, Quantity: -delta, CartID: cartID, EventID: eventID})
		}
	}()

	// Cart convertido (design C): a mutação vai para o PEDIDO via ciclo
	// estornar→PUT→lançar — a grade final já está no banco (o checkout grava
	// o cart_item ANTES de chamar este método). Sem movimentação manual e sem
	// movementID; falha desfaz o estoque local para o comprador ver o erro.
	if st, stErr := s.repo.GetCartERPOrderState(ctx, cartID); stErr == nil &&
		st.State != erpOrderStateNone && st.State != erpOrderStateCancelled {
		if mutErr := s.MutateERPOrderItems(ctx, cartID, storeID); mutErr != nil {
			rollbackLocal()
			return "", fmt.Errorf("applying grid to converted cart order: %w", mutErr)
		}
		return "", nil
	}

	// 2. ERP sync — optional. Anything below is best-effort against the ERP;
	//    any failure rolls back the local change above.
	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		logger.From(ctx, s.logger).Debug("no active ERP integration, skipping reservation delta",
			zap.String("store_id", storeID),
		)
		return "", nil
	}

	erpProvider, err := s.erpProviderFor(ctx, integration)
	if err != nil {
		rollbackLocal()
		return "", fmt.Errorf("creating ERP provider: %w", err)
	}

	if s.productSyncer == nil {
		return "", nil
	}
	externalID, _, err := s.productSyncer.GetProduct(ctx, storeID, productID)
	if err != nil || externalID == "" {
		logger.From(ctx, s.logger).Debug("product not linked to ERP, skipping reservation delta",
			zap.String("product_id", productID),
		)
		return "", nil
	}

	existing, _ := s.repo.ListActiveReservationsByCartAndProduct(ctx, cartID, productID)

	if delta > 0 {
		obs := fmt.Sprintf("Ajuste reserva LiveCart (+%d) - @%s - Cart %s", delta, platformHandle, cartID)
		movementID, err := erpProvider.ReserveStock(ctx, externalID, delta, float64(unitPrice)/100, obs)
		if err != nil {
			rollbackLocal()
			return "", fmt.Errorf("reserving stock delta in ERP: %w", err)
		}

		if len(existing) == 0 {
			if _, err := s.repo.CreateStockReservation(ctx, CreateStockReservationParams{
				EventID:           eventID,
				CartID:            cartID,
				ProductID:         productID,
				ExternalProductID: externalID,
				Quantity:          delta,
				ERPMovementID:     movementID,
			}); err != nil {
				return movementID, fmt.Errorf("recording new reservation: %w", err)
			}
		} else if _, err := s.repo.AdjustActiveReservationQuantity(ctx, cartID, productID, delta, movementID); err != nil {
			return movementID, fmt.Errorf("bumping reservation quantity: %w", err)
		}

		logger.From(ctx, s.logger).Info("ERP reservation increased",
			zap.String("cart_id", cartID),
			zap.String("product_id", productID),
			zap.Int("delta", delta),
			zap.String("erp_movement_id", movementID),
		)
		return movementID, nil
	}

	// delta < 0
	if len(existing) == 0 {
		logger.From(ctx, s.logger).Warn("no active reservation to decrease for cart+product, skipping ERP call",
			zap.String("cart_id", cartID),
			zap.String("product_id", productID),
			zap.Int("delta", delta),
		)
		return "", nil
	}

	// Sum across all active rows (in practice the unique index keeps it to 1).
	currentQty := 0
	for _, r := range existing {
		currentQty += r.Quantity
	}
	newQty := currentQty + delta

	obs := fmt.Sprintf("Ajuste reserva LiveCart (%d) - @%s - Cart %s", delta, platformHandle, cartID)
	movementID, err := erpProvider.ReverseStockReservation(ctx, externalID, -delta, 0, obs)
	if err != nil {
		rollbackLocal()
		return "", fmt.Errorf("reversing stock delta in ERP: %w", err)
	}

	// Full reversal: skip the UPDATE — stock_reservations.quantity has a
	// CHECK (quantity > 0) constraint, so we cannot zero the row in place.
	// Mark it reversed instead.
	if newQty <= 0 {
		if err := s.repo.ReverseReservationsByCartAndProduct(ctx, cartID, productID); err != nil {
			return movementID, fmt.Errorf("marking reservation reversed: %w", err)
		}
	} else if _, err := s.repo.AdjustActiveReservationQuantity(ctx, cartID, productID, delta, movementID); err != nil {
		return movementID, fmt.Errorf("decreasing reservation quantity: %w", err)
	}

	logger.From(ctx, s.logger).Info("ERP reservation decreased",
		zap.String("cart_id", cartID),
		zap.String("product_id", productID),
		zap.Int("delta", delta),
		zap.Int("new_qty", newQty),
		zap.String("erp_movement_id", movementID),
	)
	return movementID, nil
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
	logger.From(ctx, s.logger).Debug("FinalizeEventERP called — no-op under paid-first ERP flow",
		zap.String("store_id", storeID),
		zap.String("event_id", eventID),
	)
	return nil
}

// createFinalERPOrder creates a single paid sales order in the ERP for a cart
// whose payment was just confirmed. Uses the customer identity + shipping
// address captured at checkout and the payment details from the provider.
func (s *Service) createFinalERPOrder(ctx context.Context, erpProvider providers.ERPProvider, integration *IntegrationRow, storeID, eventID string, cart CartRow, paymentStatus *providers.PaymentStatus, launchStock bool) error {
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
		logger.From(ctx, s.logger).Warn("paid cart has no ERP-linked items, skipping order creation",
			zap.String("cart_id", cart.ID),
			zap.Int("cart_items_total", len(items)),
		)
		return nil
	}

	logger.From(ctx, s.logger).Info("ERP order items prepared",
		zap.String("cart_id", cart.ID),
		zap.Int("erp_items", len(erpItems)),
		zap.Int("cart_items_total", len(items)),
		zap.Int64("items_total_cents", totalAmount),
		zap.String("contact_id", contactID),
	)

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
			logger.From(ctx, s.logger).Warn("failed to parse cart shipping_address",
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
			Method:           paymentStatus.PaymentMethod,
			PaymentID:        paymentStatus.PaymentID,
			Installments:     paymentStatus.Installments,
			PaidAt:           paidAt,
			Amount:           totalAmount,
			MoneyReleaseDate: paymentStatus.MoneyReleaseDate,
			FeeAmountCents:   paymentStatus.FeeAmountCents,
			NetAmountCents:   paymentStatus.NetAmountCents,
		}

		// Snapshot of the financial breakdown right before we hand the
		// order to the ERP adapter. This is the single point that
		// connects "what the gateway told us" to "what we asked the ERP
		// to record" — if the two ever drift (rounding, fee changes,
		// refund), the diff shows up between this log and the
		// `tiny CreateOrder sending payload` log that follows.
		logger.From(ctx, s.logger).Info("ERP order payment snapshot prepared",
			zap.String("cart_id", cart.ID),
			zap.String("payment_id", paymentStatus.PaymentID),
			zap.String("payment_method", paymentStatus.PaymentMethod),
			zap.String("payment_status", string(paymentStatus.Status)),
			zap.Int("installments", paymentStatus.Installments),
			zap.Int64("order_total_cents", totalAmount),
			zap.Int64("paid_amount_cents", paymentStatus.Amount),
			zap.Int64("fee_amount_cents", paymentStatus.FeeAmountCents),
			zap.Int64("net_amount_cents", paymentStatus.NetAmountCents),
			zap.Bool("has_money_release_date", paymentStatus.MoneyReleaseDate != nil),
		)
	}

	result, err := erpProvider.CreateOrder(ctx, order)
	if err != nil {
		return fmt.Errorf("creating ERP order: %w", err)
	}

	// Save external order ID on cart first — ensures idempotency if we retry
	if err := s.repo.UpdateCartExternalOrderID(ctx, cart.ID, result.OrderID); err != nil {
		return fmt.Errorf("saving external order ID: %w", err)
	}

	// Group G fact (best-effort): the order now exists in the ERP. This is the
	// single point that creates it (both legacy and Design C conversion paths
	// funnel through here). Dedup by the ERP external order id.
	_ = events.EmitInternal(ctx, s.repo.queries, events.ERPOrderCreated, "erp.order_created:"+result.OrderID, struct {
		StoreID         string `json:"store_id"`
		CartID          string `json:"cart_id"`
		ExternalOrderID string `json:"external_order_id"`
		Provider        string `json:"provider"`
	}{StoreID: storeID, CartID: cart.ID, ExternalOrderID: result.OrderID, Provider: "tiny"})

	// Launch stock (permanent decrement). O fluxo invertido (Fase 3) passa
	// launchStock=false e orquestra o launch no caller, com fallback próprio.
	if launchStock {
		if err := erpProvider.LaunchOrderStock(ctx, result.OrderID); err != nil {
			return fmt.Errorf("launching stock for order %s: %w", result.OrderID, err)
		}
	}

	logFields := []zap.Field{
		zap.String("cart_id", cart.ID),
		zap.String("erp_order_id", result.OrderID),
		zap.Int("items", len(erpItems)),
	}
	// paymentStatus é nil na conversão pré-pagamento do design C (pedido
	// nasce Aberta, sem parcelas).
	if paymentStatus != nil {
		logFields = append(logFields,
			zap.String("payment_id", paymentStatus.PaymentID),
			zap.String("payment_method", paymentStatus.PaymentMethod),
			zap.Int64("fee_amount_cents", paymentStatus.FeeAmountCents),
			zap.Int64("net_amount_cents", paymentStatus.NetAmountCents),
		)
	}
	logger.From(ctx, s.logger).Info("ERP order created for cart", logFields...)

	return nil
}

// =============================================================================
// ERP HELPERS
// =============================================================================

// resolveERPContact finds or creates an ERP contact for the platform user.
// When the cart carries checkout customer data (name, document, email, phone)
// the contact is enriched on every paid order — without this, a contact
// created earlier under the Instagram @handle stays with the @handle as its
// nome forever and Tiny orders display the handle instead of the real name.
func (s *Service) resolveERPContact(ctx context.Context, erpProvider providers.ERPProvider, integration *IntegrationRow, storeID, platformUserID, platformHandle, customerName, customerDocument, customerEmail, customerPhone string) (string, error) {
	enrich := providers.ERPContactInput{
		Name:       customerName,
		CpfCnpj:    customerDocument,
		Email:      customerEmail,
		Phone:      customerPhone,
		PersonType: "F",
	}

	// Check cache first
	cachedID, err := s.repo.GetERPContact(ctx, storeID, integration.ID, platformUserID)
	if err != nil {
		return "", err
	}
	if cachedID != "" {
		logger.From(ctx, s.logger).Info("ERP contact resolved from cache",
			zap.String("contact_id", cachedID),
			zap.String("platform_user_id", platformUserID),
		)
		s.bestEffortUpdateContact(ctx, erpProvider, cachedID, enrich)
		return cachedID, nil
	}

	// Search by document — most reliable key in Tiny.
	if customerDocument != "" {
		results, err := erpProvider.SearchContacts(ctx, providers.SearchContactsParams{
			CpfCnpj: customerDocument,
		})
		if err == nil && len(results) > 0 {
			logger.From(ctx, s.logger).Info("ERP contact resolved by document",
				zap.String("contact_id", results[0].ContactID),
				zap.String("platform_user_id", platformUserID),
			)
			_ = s.repo.UpsertERPContact(ctx, storeID, integration.ID, platformUserID, platformHandle, results[0].ContactID)
			s.bestEffortUpdateContact(ctx, erpProvider, results[0].ContactID, enrich)
			return results[0].ContactID, nil
		}
	}

	// Note: previously fell back to SearchContacts(Name: platformHandle).
	// That found contacts left over from prior orders whose nome was the
	// @handle and reused them — masking the customerName the buyer typed at
	// checkout. Removed: when there's no document match we just create a
	// fresh contact so the new order carries the real name.

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

	logger.From(ctx, s.logger).Info("ERP contact created",
		zap.String("contact_id", contact.ContactID),
		zap.String("platform_user_id", platformUserID),
		zap.String("contact_name", contactName),
	)

	// Cache
	_ = s.repo.UpsertERPContact(ctx, storeID, integration.ID, platformUserID, platformHandle, contact.ContactID)
	return contact.ContactID, nil
}

// bestEffortUpdateContact pushes the latest checkout customer data into a
// long-lived ERP contact. Skipped when we have nothing useful to send. Errors
// are logged and swallowed — the order must still go through even if the
// enrichment call fails.
func (s *Service) bestEffortUpdateContact(ctx context.Context, erpProvider providers.ERPProvider, contactID string, contact providers.ERPContactInput) {
	if contact.Name == "" && contact.CpfCnpj == "" && contact.Email == "" && contact.Phone == "" {
		return
	}
	if err := erpProvider.UpdateContact(ctx, contactID, contact); err != nil {
		logger.From(ctx, s.logger).Warn("failed to enrich ERP contact, proceeding with order anyway",
			zap.String("contact_id", contactID),
			zap.Error(err),
		)
	}
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

// ExpireCart expira UM carrinho, com segurança contra a corrida do pagamento.
// Ordem (crítica):
//  1. Advisory lock por cart — serializa contra o confirm/finalize do webhook
//     de pagamento (ConfirmERPOrderPayment/finalizeCartERPOrder tomam o mesmo
//     lock). !acquired = pagamento finalizando; sai.
//  2. Flip 'expired' + devolução de estoque local de TODOS os itens numa única
//     transação (ExpireCartAndReleaseStock). O flip é guard-first: se o cart
//     foi pago/expirado no intervalo, 0 rows → NÃO elegível → aborta sem tocar
//     ERP (a ação irreversível de cancelar pedido só roda com o cart já 'expired').
//  3. ERP (best-effort, fora do tx): design C cancela o pedido (situação 2 +
//     estorno); senão reverte as reservas saída-manual POR CART.
//  4. Promove a waitlist de cada produto liberado (pós-commit, fire-and-forget).
//
// CartExpiryScheduler arms an ETA task that fires a cart's expiry at its
// expires_at, so expiration is precise instead of waiting on the 5-min sweep.
// Implemented over the asynq client in main.go (events package must not import
// domain packages). The sweep remains as a safety net for any lost task.
type CartExpiryScheduler interface {
	ScheduleCartExpiry(ctx context.Context, cartID string, at time.Time) error
}

// SetCartExpiryScheduler wires the ETA scheduler (optional — when unset, only
// the sweep expires carts, preserving today's behaviour).
func (s *Service) SetCartExpiryScheduler(sch CartExpiryScheduler) { s.expiryScheduler = sch }

// ScheduleExpiry arms (or re-arms) the cart.expire ETA task for a cart's current
// expires_at. Best-effort: a failure or a lost task is caught by the sweep. Skips
// carts that are already terminal or have no window.
func (s *Service) ScheduleExpiry(ctx context.Context, cartID string) error {
	if s.expiryScheduler == nil {
		return nil
	}
	snap, err := s.repo.GetCartExpirySnapshot(ctx, cartID)
	if err != nil || snap == nil {
		return err
	}
	if snap.ExpiresAt == nil || cartExpiryTerminal(snap) {
		return nil
	}
	return s.expiryScheduler.ScheduleCartExpiry(ctx, cartID, *snap.ExpiresAt)
}

// RunScheduledExpiry is the cart.expire task handler. It is guard-first against
// the window-extension case (waitlist promotion pushes expires_at out): if the
// cart is now terminal it is a no-op; if the window moved into the future it
// re-arms instead of expiring; otherwise it runs the same guarded ExpireCart as
// the sweep. Idempotent — safe under asynq at-least-once redelivery.
func (s *Service) RunScheduledExpiry(ctx context.Context, cartID string) error {
	snap, err := s.repo.GetCartExpirySnapshot(ctx, cartID)
	if err != nil {
		return err
	}
	if snap == nil || snap.ExpiresAt == nil || cartExpiryTerminal(snap) {
		return nil
	}
	if snap.ExpiresAt.After(time.Now().UTC()) {
		// Window extended after the task was armed — re-arm for the new time.
		if s.expiryScheduler != nil {
			return s.expiryScheduler.ScheduleCartExpiry(ctx, cartID, *snap.ExpiresAt)
		}
		return nil
	}
	s.ExpireCart(ctx, cartID, snap.StoreID)
	return nil
}

// cartExpiryTerminal reports whether a cart is already in a state where expiry
// must not run (paid/refunded or already expired/cancelled).
func cartExpiryTerminal(s *CartExpirySnapshot) bool {
	return s.Status == "expired" || s.Status == "cancelled" ||
		s.PaymentStatus == "paid" || s.PaymentStatus == "refunded"
}

func (s *Service) ExpireCart(ctx context.Context, cartID, storeID string) {
	ctx = logger.WithStore(ctx, storeID, "")
	release, acquired, err := s.repo.AcquireCartFinalisationLock(ctx, cartID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("expiry: failed to acquire cart lock", zap.String("cart_id", cartID), zap.Error(err))
		return
	}
	if !acquired {
		// Webhook de pagamento está finalizando este mesmo cart. Ele decide.
		logger.From(ctx, s.logger).Info("expiry: skip, finalisation in progress", zap.String("cart_id", cartID))
		return
	}
	defer release()

	res, err := s.repo.ExpireCartAndReleaseStock(ctx, cartID, storeID)
	if err != nil {
		logger.From(ctx, s.logger).Error("expiry: failed to expire cart", zap.String("cart_id", cartID), zap.Error(err))
		return
	}
	if !res.Eligible {
		// Pago ou já expirado/cancelado entre a seleção e o flip. Nada a fazer.
		logger.From(ctx, s.logger).Info("expiry: cart no longer eligible (paid/terminal in gap)", zap.String("cart_id", cartID))
		return
	}

	// ERP reversal (Tiny cancel/estorno) now runs in the cart.expired reactor
	// (ReactCartExpiredERP), decoupled from this eligibility flip so it gets its
	// own asynq retry + DLQ. The cart.expired fact was emitted transactionally
	// inside ExpireCartAndReleaseStock above.

	logger.From(ctx, s.logger).Info("expired cart processed",
		zap.String("cart_id", cartID),
		zap.Int("items_released", len(res.FreedProductIDs)),
	)

	// Promove o próximo da fila para cada produto liberado. Idempotente.
	for _, productID := range res.FreedProductIDs {
		s.ProcessWaitlistForProduct(ctx, res.EventID, productID, storeID)
	}
}

// reverseCartReservationsInERP estorna todas as reservas saída-manual ativas de
// um cart no Tiny e marca as rows como revertidas. Best-effort: espelha o passo
// do block-sweep (CancelOpenCartsForBlockedHandle) mas por cart único.
func (s *Service) reverseCartReservationsInERP(ctx context.Context, cartID, storeID string) {
	reservations, resErr := s.repo.ListActiveReservationsByCart(ctx, cartID)
	if resErr != nil {
		logger.From(ctx, s.logger).Error("expiry: failed to list reservations", zap.String("cart_id", cartID), zap.Error(resErr))
		return
	}
	if len(reservations) == 0 {
		return
	}

	erpReversed := true
	if integration, intErr := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny"); intErr == nil {
		if erpProvider, provErr := s.erpProviderFor(ctx, integration); provErr == nil {
			for _, r := range reservations {
				obs := fmt.Sprintf("Estorno expiração carrinho LiveCart - Cart %s", cartID)
				if _, reverseErr := erpProvider.ReverseStockReservation(ctx, r.ExternalProductID, r.Quantity, 0, obs); reverseErr != nil {
					erpReversed = false
					logger.From(ctx, s.logger).Warn("expiry: failed to reverse ERP reservation",
						zap.String("cart_id", cartID),
						zap.String("external_product_id", r.ExternalProductID),
						zap.Error(reverseErr))
				}
			}
		} else {
			erpReversed = false
			logger.From(ctx, s.logger).Error("expiry: failed to build ERP provider", zap.String("cart_id", cartID), zap.Error(provErr))
		}
	} else {
		erpReversed = false
		logger.From(ctx, s.logger).Warn("expiry: no active ERP integration, marking reservations reversed locally only",
			zap.String("store_id", storeID))
	}

	if markErr := s.repo.ReverseReservationsByCart(ctx, cartID); markErr != nil {
		logger.From(ctx, s.logger).Error("expiry: failed to mark reservations reversed", zap.String("cart_id", cartID), zap.Error(markErr))
	}
	if !erpReversed {
		logger.From(ctx, s.logger).Warn("expiry: ERP reservations NOT fully reversed — manual reconciliation may be needed",
			zap.String("cart_id", cartID))
	}
}

// ProcessExpiredCartsForProduct handles expired carts that contain the given product.
// DEPRECATED: substituída pela expiração por-cart (schedule cart.expire → ExpireCart).
// Não é mais chamada em produção — o único caller remanescente é teste. Mantida
// temporariamente para não quebrar o teste de resume; não usar em código novo.
func (s *Service) ProcessExpiredCartsForProduct(ctx context.Context, eventID, productID string) {
	carts, err := s.repo.ListExpiredCartsByEventAndProduct(ctx, eventID, productID)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to list expired carts", zap.Error(err))
		return
	}

	for _, cart := range carts {
		// Mark cart as expired
		if err := s.repo.UpdateCartStatus(ctx, cart.ID, "expired"); err != nil {
			logger.From(ctx, s.logger).Error("failed to expire cart", zap.String("cart_id", cart.ID), zap.Error(err))
			continue
		}

		// Cart convertido (design C): quem devolve o estoque é o cancelamento
		// do PEDIDO (situação 2 → estorno incondicional). As saídas manuais já
		// foram estornadas na conversão — o branch de reservas abaixo no-opa.
		if st, stErr := s.repo.GetCartERPOrderState(ctx, cart.ID); stErr == nil &&
			st.State != erpOrderStateNone && st.State != erpOrderStateCancelled {
			if cancelErr := s.CancelERPOrderForCart(ctx, cart.ID, cart.StoreID); cancelErr != nil {
				logger.From(ctx, s.logger).Error("failed to cancel converted cart order on expiry",
					zap.String("cart_id", cart.ID),
					zap.Error(cancelErr),
				)
			}
		}

		// Release stock back to product — the real non-waitlisted quantity of
		// the item, not 1: o add da live decrementou (quantity -
		// waitlisted_quantity) unidades e é isso que a expiração devolve.
		// Antes deste fix o valor era hardcoded em 1 e o local sub-contava
		// para itens com qty>1, "consertado" por acaso pelo overwrite do
		// webhook — que o guard novo passou a segurar.
		releaseQty, qtyErr := s.repo.GetCartItemAvailableQty(ctx, cart.ID, productID)
		if qtyErr != nil || releaseQty < 1 {
			if qtyErr != nil {
				logger.From(ctx, s.logger).Warn("failed to read cart item quantity for expiry release, falling back to 1",
					zap.String("cart_id", cart.ID),
					zap.String("product_id", productID),
					zap.Error(qtyErr),
				)
			}
			releaseQty = 1
		}
		if err := s.repo.IncrementProductStock(ctx, productID, releaseQty); err != nil {
			logger.From(ctx, s.logger).Error("failed to release stock", zap.String("product_id", productID), zap.Error(err))
		}

		// Reverse ERP stock reservations for this cart+product
		reservations, resErr := s.repo.ListActiveReservationsByCartAndProduct(ctx, cart.ID, productID)
		if resErr != nil {
			logger.From(ctx, s.logger).Error("failed to list reservations for expired cart",
				zap.String("cart_id", cart.ID),
				zap.String("product_id", productID),
				zap.Error(resErr),
			)
		}
		if len(reservations) > 0 {
			erpReversed := false
			integration, intErr := s.repo.GetActiveByProvider(ctx, cart.StoreID, "erp", "tiny")
			if intErr != nil {
				logger.From(ctx, s.logger).Warn("no active ERP integration for expired cart reversal, marking reservations as reversed locally only",
					zap.String("store_id", cart.StoreID),
				)
			} else {
				erpProvider, provErr := s.erpProviderFor(ctx, integration)
				if provErr != nil {
					logger.From(ctx, s.logger).Error("failed to create ERP provider for expired cart reversal",
						zap.String("cart_id", cart.ID),
						zap.Error(provErr),
					)
				} else {
					erpReversed = true
					for _, res := range reservations {
						obs := fmt.Sprintf("Estorno expiração carrinho LiveCart - Cart %s", cart.ID)
						if _, reverseErr := erpProvider.ReverseStockReservation(ctx, res.ExternalProductID, res.Quantity, 0, obs); reverseErr != nil {
							erpReversed = false
							logger.From(ctx, s.logger).Warn("failed to reverse expired cart stock reservation in ERP",
								zap.String("cart_id", cart.ID),
								zap.String("external_product_id", res.ExternalProductID),
								zap.Error(reverseErr),
							)
						}
					}
				}
			}
			if markErr := s.repo.ReverseReservationsByCartAndProduct(ctx, cart.ID, productID); markErr != nil {
				logger.From(ctx, s.logger).Error("failed to mark reservations as reversed",
					zap.String("cart_id", cart.ID),
					zap.String("product_id", productID),
					zap.Error(markErr),
				)
			}
			if !erpReversed {
				logger.From(ctx, s.logger).Warn("ERP stock reservations NOT reversed for expired cart — manual reconciliation may be needed",
					zap.String("cart_id", cart.ID),
					zap.String("product_id", productID),
				)
			}
		}

		logger.From(ctx, s.logger).Info("expired cart processed",
			zap.String("cart_id", cart.ID),
			zap.String("product_id", productID),
		)
	}

	// Após liberar todo o estoque dos carts expirados, tenta promover o
	// próximo da fila. Idempotente: se ninguém está esperando ou se o
	// stock zerou no meio do caminho, ProcessWaitlistForProduct no-ops.
	if len(carts) > 0 {
		// fire-and-forget: o release acima já completou e os logs vão
		// indicar se a promoção rodou; não faz sentido bloquear o caller.
		s.ProcessWaitlistForProduct(ctx, eventID, productID, carts[0].StoreID)
	}
}

// CancelOpenCartsForBlockedHandle is invoked by the customer service after a
// handle is blocked: it sweeps every non-paid cart of that handle in the
// store, releases local + ERP stock for each item, marks the carts as
// cancelled with reason='customer_blocked', then promotes the waitlist for
// each freed product.
//
// All errors are logged; the function never fails hard — the block row is
// already persisted by the caller, so even if some cleanup fails the block
// itself is in effect (future comments are filtered).
func (s *Service) CancelOpenCartsForBlockedHandle(ctx context.Context, storeID, handle string) error {
	carts, err := s.repo.ListOpenCartsByHandle(ctx, storeID, handle)
	if err != nil {
		return fmt.Errorf("listing open carts for blocked handle: %w", err)
	}
	if len(carts) == 0 {
		return nil
	}

	type freed struct {
		eventID, productID string
	}
	freedKeys := make(map[freed]bool)

	// Resolve the ERP integration once — same one applies to every cart in
	// this store. nil is OK: handle the no-ERP case by skipping the remote
	// reversal step and only flipping the local DB.
	var erpProvider providers.ERPProvider
	if integration, intErr := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny"); intErr == nil {
		if provider, provErr := s.erpProviderFor(ctx, integration); provErr == nil {
			erpProvider = provider
		} else {
			logger.From(ctx, s.logger).Warn("failed to build ERP provider for block sweep",
				zap.String("store_id", storeID),
				zap.Error(provErr),
			)
		}
	}

	for _, cart := range carts {
		items, listErr := s.repo.ListNonWaitlistedCartItems(ctx, cart.ID)
		if listErr != nil {
			logger.From(ctx, s.logger).Error("failed to list cart items for blocked cart",
				zap.String("cart_id", cart.ID),
				zap.Error(listErr),
			)
			continue
		}

		// 1. Release local stock per item (waitlisted items don't hold stock
		//    so we ignore them — they auto-cancel when the cart is killed).
		for _, item := range items {
			if err := s.stock.Release(ctx, ReleaseParams{Op: StockOpCancelBlocked, ProductID: item.ProductID, Quantity: item.Quantity, CartID: cart.ID, EventID: cart.EventID}); err != nil {
				logger.From(ctx, s.logger).Error("failed to release local stock for blocked cart",
					zap.String("cart_id", cart.ID),
					zap.String("product_id", item.ProductID),
					zap.Error(err),
				)
			}
			freedKeys[freed{eventID: cart.EventID, productID: item.ProductID}] = true
		}

		// 2. Reverse ERP stock reservations (saída-manual) so Tiny gets its
		//    inventory back. Best-effort: if the ERP call fails we still mark
		//    the DB rows reversed and surface a warning for manual reconciliation.
		reservations, resErr := s.repo.ListActiveReservationsByCart(ctx, cart.ID)
		if resErr != nil {
			logger.From(ctx, s.logger).Error("failed to list reservations for blocked cart",
				zap.String("cart_id", cart.ID),
				zap.Error(resErr),
			)
		}
		if len(reservations) > 0 && erpProvider != nil {
			for _, r := range reservations {
				obs := fmt.Sprintf("Estorno cliente bloqueado - Cart %s", cart.ID)
				if _, reverseErr := erpProvider.ReverseStockReservation(ctx, r.ExternalProductID, r.Quantity, 0, obs); reverseErr != nil {
					logger.From(ctx, s.logger).Warn("failed to reverse ERP reservation on block",
						zap.String("cart_id", cart.ID),
						zap.String("external_product_id", r.ExternalProductID),
						zap.Int("quantity", r.Quantity),
						zap.Error(reverseErr),
					)
				}
			}
		}
		if len(reservations) > 0 {
			if err := s.repo.ReverseReservationsByCart(ctx, cart.ID); err != nil {
				logger.From(ctx, s.logger).Error("failed to mark reservations reversed for blocked cart",
					zap.String("cart_id", cart.ID),
					zap.Error(err),
				)
			}
		}

		// 3. Mark cart as cancelled (no-op if it was paid in the gap).
		if err := s.repo.CancelCartAsBlocked(ctx, cart.ID); err != nil {
			logger.From(ctx, s.logger).Error("failed to cancel cart as blocked",
				zap.String("cart_id", cart.ID),
				zap.Error(err),
			)
			continue
		}

		logger.From(ctx, s.logger).Info("cart cancelled by customer block",
			zap.String("cart_id", cart.ID),
			zap.String("handle", handle),
			zap.Int("items_released", len(items)),
		)
	}

	// 4. Try to promote the next person in line for each freed product.
	//    Fire-and-forget within this request — same pattern as cart-expiration.
	for key := range freedKeys {
		s.ProcessWaitlistForProduct(ctx, key.eventID, key.productID, storeID)
	}

	return nil
}

// acquireCartFinalisationLockRetry tenta o try-lock de finalização do cart com
// algumas tentativas curtas. Um lock momentaneamente retido por uma expiração
// concorrente (que agora se abstém e o solta rápido, graças ao guard de
// ExpireCart) é liberado entre as tentativas, então o promotor quase sempre o
// adquire sem perder a vez do cliente. Respeita o cancelamento do ctx no backoff.
func (s *Service) acquireCartFinalisationLockRetry(ctx context.Context, cartID string) (release func(), acquired bool, err error) {
	const attempts = 3
	const backoff = 25 * time.Millisecond
	for i := 0; i < attempts; i++ {
		release, acquired, err = s.repo.AcquireCartFinalisationLock(ctx, cartID)
		if err != nil || acquired {
			return release, acquired, err
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil, false, nil
}

// ProcessWaitlistForProduct promotes the next waiting customer to "notified"
// when stock is available. Called after any stock release (cart expired,
// cart paid, item removed, ERP webhook). Idempotent: if no one is waiting,
// or DecrementProductStock fails (race with another caller), it no-ops.
//
// Flow:
//  1. Decrement local stock (this is the gate — atomic, prevents double-promote)
//  2. Promote item to the next person's cart (WaitlistedQuantity=0)
//  3. Push the cart's expires_at by event.waitlist_notified_ttl_minutes (the
//     "gordura" the customer asked for)
//  4. Mark waitlist row as 'notified' with expires_at
//  5. Reserve stock in the ERP (Tiny saída)
//  6. Fire-and-forget DM via the notification service
//
// If anything in steps 2-3 fails we roll back the local stock decrement so
// nobody else loses the slot. Steps 4-6 are best-effort post-promotion.
func (s *Service) ProcessWaitlistForProduct(ctx context.Context, eventID, productID, storeID string) {
	// TTL primeiro: a reivindicação atômica já grava a janela de notificação
	// na row da fila, então precisamos de notifiedUntil antes do claim.
	ttl, ttlErr := s.repo.GetWaitlistNotifiedTTL(ctx, eventID)
	if ttlErr != nil {
		logger.From(ctx, s.logger).Warn("failed to read waitlist TTL, defaulting to 30min",
			zap.String("event_id", eventID),
			zap.Error(ttlErr),
		)
		ttl = 30 * time.Minute
	}
	notifiedUntil := time.Now().Add(ttl)

	// Reivindica ATOMICAMENTE o próximo da fila (FOR UPDATE SKIP LOCKED):
	// callers concorrentes pegam clientes DISTINTOS. Sem isso, duas
	// liberações simultâneas selecionavam o MESMO W1 e consumiam 2 unidades
	// avançando a fila só 1 — o próximo ficava preso com uma unidade perdida.
	next, err := s.repo.ClaimNextWaitlistItem(ctx, eventID, productID, notifiedUntil)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to claim next waitlist item", zap.Error(err))
		return
	}
	if next == nil {
		return // fila vazia
	}

	// Gate de estoque PRIMEIRO (gate atômico no contador do produto), ANTES do
	// advisory-lock. Callers concorrentes reivindicam carts DISTINTOS (SKIP
	// LOCKED), então cada um pegaria o lock do SEU cart e o seguraria (via defer)
	// enquanto abre uma 2ª conexão para as queries seguintes — sob uma multidão
	// de promoções simultâneas isso é hold-and-wait e esgota o pool (deadlock).
	// Passando o gate antes, só o(s) goroutine(s) que REALMENTE tomam unidade
	// seguem para o lock; os demais revertem para 'waiting' sem nunca prendê-lo.
	// Promoção PARCIAL: toma ATÉ a quantidade pedida — o cliente recebe o que
	// houver e continua na fila pelo restante. Take provisório: revertido +
	// estoque devolvido em qualquer falha abaixo, então stock.reserved só sai no
	// ponto de sucesso definitivo.
	taken, err := s.repo.DecrementProductStockUpTo(ctx, productID, next.Quantity)
	if err != nil {
		_ = s.repo.RevertWaitlistToWaiting(ctx, next.ID)
		logger.From(ctx, s.logger).Error("failed to take stock for waitlist promotion",
			zap.String("product_id", productID),
			zap.String("waitlist_item_id", next.ID),
			zap.Error(err),
		)
		return
	}
	if taken <= 0 {
		if revErr := s.repo.RevertWaitlistToWaiting(ctx, next.ID); revErr != nil {
			logger.From(ctx, s.logger).Error("failed to revert waitlist claim after stock gate miss",
				zap.String("waitlist_item_id", next.ID),
				zap.Error(revErr),
			)
		}
		logger.From(ctx, s.logger).Debug("waitlist promote skipped: stock not available",
			zap.String("product_id", productID),
			zap.String("waitlist_item_id", next.ID),
		)
		return
	}

	// Tomou a unidade → serializa a MUTAÇÃO do cart contra uma finalização de
	// PAGAMENTO concorrente do MESMO cart sob o advisory-lock que ExpireCart
	// também respeita. O guard de ExpireCart já abstém-se de um cart de
	// waitlister enquanto o item está 'waiting' ou 'notified' dentro da janela
	// (a reivindicação atômica grava wi.expires_at no futuro no MESMO passo em
	// que vira 'notified'), então a corrida com a EXPIRAÇÃO está fechada sem o
	// lock — ele é a 2ª linha, só contra o pagamento. Uma expiração concorrente
	// que porventura o segure abstém-se e o solta rápido, então um RETRY curto
	// quase sempre o adquire. Falha após as tentativas OU cart terminal (pago) →
	// DEVOLVE a unidade ao estoque e REVERTE a reivindicação para 'waiting' (NÃO
	// cancela): o cliente não perde a vez e é promovido no próximo ciclo.
	release, acquired, lockErr := s.acquireCartFinalisationLockRetry(ctx, next.CartID)
	if lockErr != nil {
		_ = s.repo.IncrementProductStock(ctx, productID, taken)
		_ = s.repo.RevertWaitlistToWaiting(ctx, next.ID)
		logger.From(ctx, s.logger).Warn("waitlist promote: failed to acquire cart lock",
			zap.String("cart_id", next.CartID), zap.Error(lockErr))
		return
	}
	if !acquired {
		// Pagamento/finalização do cart segurou o lock além do retry. Devolve a
		// unidade ao estoque e o cliente ao TOPO da fila (não cancela) — ele é
		// promovido no próximo release. Garante que ninguém perde a vez.
		_ = s.repo.IncrementProductStock(ctx, productID, taken)
		if revErr := s.repo.RevertWaitlistToWaiting(ctx, next.ID); revErr != nil {
			logger.From(ctx, s.logger).Warn("waitlist promote: failed to revert claim after lock contention",
				zap.String("waitlist_item_id", next.ID), zap.Error(revErr))
		}
		logger.From(ctx, s.logger).Info("waitlist promote deferred: cart lock held, buyer kept in queue",
			zap.String("cart_id", next.CartID), zap.String("waitlist_item_id", next.ID))
		return
	}
	defer release()

	// Sob o lock, relê o cart: se ele foi PAGO/cancelado logo antes de pegarmos o
	// lock, está terminal — não promover nele. Devolve a unidade e reverte a
	// reivindicação para 'waiting' (mantém o cliente na fila para a próxima).
	if snap, snapErr := s.repo.GetCartExpirySnapshot(ctx, next.CartID); snapErr != nil {
		_ = s.repo.IncrementProductStock(ctx, productID, taken)
		_ = s.repo.RevertWaitlistToWaiting(ctx, next.ID)
		logger.From(ctx, s.logger).Warn("waitlist promote: failed to read cart snapshot",
			zap.String("cart_id", next.CartID), zap.Error(snapErr))
		return
	} else if snap == nil || cartExpiryTerminal(snap) {
		_ = s.repo.IncrementProductStock(ctx, productID, taken)
		if revErr := s.repo.RevertWaitlistToWaiting(ctx, next.ID); revErr != nil {
			logger.From(ctx, s.logger).Warn("waitlist promote: failed to revert claim on terminal cart",
				zap.String("waitlist_item_id", next.ID), zap.Error(revErr))
		}
		logger.From(ctx, s.logger).Info("waitlist promote deferred: target cart terminal, buyer kept in queue",
			zap.String("cart_id", next.CartID))
		return
	}

	product, err := s.repo.GetProductByID(ctx, storeID, productID)
	if err != nil || product == nil {
		_ = s.repo.IncrementProductStock(ctx, productID, taken)
		_ = s.repo.RevertWaitlistToWaiting(ctx, next.ID)
		logger.From(ctx, s.logger).Error("failed to get product for waitlist promotion",
			zap.String("product_id", productID),
			zap.String("store_id", storeID),
			zap.Error(err),
		)
		return
	}

	// Promote: the cart_items row was created at waitlist signup with
	// quantity == waitlisted_quantity. Flip it to "available" by decrementing
	// waitlisted_quantity in place — re-running AddToCart here would hit
	// UpsertCartItem's ON CONFLICT branch and add both columns again, ending
	// up with quantity=2N, waitlisted=N (the shipping query then computes
	// available=N but the cart visibly carries phantom items, and a later
	// quantity edit on the inflated row leaves available <= 0, which makes
	// /shipping-quote return "nenhum item no carrinho para cotar" on a cart
	// that the FE still renders as non-empty).
	cartID := next.CartID
	found, err := s.repo.DecrementCartItemWaitlistedQuantity(ctx, cartID, productID, taken)
	if err != nil {
		_ = s.repo.IncrementProductStock(ctx, productID, taken)
		_ = s.repo.RevertWaitlistToWaiting(ctx, next.ID)
		logger.From(ctx, s.logger).Error("failed to decrement waitlisted quantity on promotion",
			zap.String("waitlist_item_id", next.ID),
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
		return
	}
	if !found {
		// Edge case: the cart_items row was deleted between waitlist signup
		// and promotion (manual remove from the dashboard, expiration sweep,
		// etc.). Recreate it via the standard AddToCart path — no existing
		// row, no upsert inflation.
		fbResult, fbErr := s.liveService.AddToCart(ctx, live.AddToCartInput{
			StoreID:            storeID,
			EventID:            eventID,
			PlatformUserID:     next.PlatformUserID,
			PlatformHandle:     next.PlatformHandle,
			ProductID:          productID,
			ProductPrice:       product.Price,
			Quantity:           taken,
			WaitlistedQuantity: 0,
		})
		if fbErr != nil {
			_ = s.repo.IncrementProductStock(ctx, productID, taken)
			_ = s.repo.RevertWaitlistToWaiting(ctx, next.ID)
			logger.From(ctx, s.logger).Error("failed to recreate cart item on promotion",
				zap.String("waitlist_item_id", next.ID),
				zap.Error(fbErr),
			)
			return
		}
		cartID = fbResult.CartID
	}

	// Promoção PARCIAL: se demos menos que o pedido, re-enfileira o restante
	// (o cliente ganhou `taken` no carrinho e continua na fila pelo resto). Se
	// atendemos tudo, a row já está 'notified' pela reivindicação atômica.
	remaining := next.Quantity - taken
	if remaining > 0 {
		if err := s.repo.RequeueWaitlistItemPartial(ctx, next.ID, remaining); err != nil {
			logger.From(ctx, s.logger).Warn("failed to requeue partial waitlist remainder",
				zap.String("waitlist_item_id", next.ID),
				zap.Int("remaining", remaining),
				zap.Error(err),
			)
		}
	}

	cartToken, tokenErr := s.repo.GetCartTokenByID(ctx, cartID)
	if tokenErr != nil {
		logger.From(ctx, s.logger).Warn("failed to read cart token after waitlist promotion",
			zap.String("cart_id", cartID),
			zap.Error(tokenErr),
		)
	}

	// "Gordura": empurra o cart.expires_at para garantir que o cliente tem
	// o TTL configurado para finalizar. Usa GREATEST no banco — não encolhe
	// um cart que já tinha um expires_at maior. (A row da fila já foi marcada
	// 'notified' com notifiedUntil pela reivindicação atômica no topo.)
	if extendErr := s.repo.ExtendCartExpiration(ctx, cartID, notifiedUntil); extendErr != nil {
		logger.From(ctx, s.logger).Warn("failed to extend cart expiration for notified waitlist",
			zap.String("cart_id", cartID),
			zap.Error(extendErr),
		)
	}

	// Re-arma a task asynq cart.expire para a NOVA janela estendida. A task
	// original (armada no finalize) já disparou — ou expirou o cart, ou pulou
	// por causa do lock acima — então, sem o sweep, nada mais expiraria este
	// cart quando o prazo estendido vencer. ScheduleExpiry lê o expires_at atual
	// (já estendido) e agenda no horário novo. Best-effort + idempotente.
	if armErr := s.ScheduleExpiry(ctx, cartID); armErr != nil {
		logger.From(ctx, s.logger).Warn("failed to re-arm expiry for promoted cart",
			zap.String("cart_id", cartID), zap.Error(armErr))
	}

	// ERP saída — reserva pareada às `taken` unidades recém-liberadas para o
	// cart. Falha aqui não bloqueia: o worker de reconciliação pega depois.
	if syncErr := s.ReserveStockInERP(ctx, storeID, cartID, eventID, productID, taken, product.Price, next.PlatformHandle); syncErr != nil {
		logger.From(ctx, s.logger).Warn("failed to reserve stock in ERP for promoted waitlist item",
			zap.String("cart_id", cartID),
			zap.Error(syncErr),
		)
	}

	// Fire-and-forget DM. Email é desnecessário aqui — o cliente foi
	// adicionado via comentário no Instagram, então não temos email
	// confirmado a essa altura. (Quando ele abrir o checkout e preencher
	// o email, o cart já vai estar com o item disponível.)
	s.sendWaitlistNotifiedDM(ctx, sendWaitlistNotifiedInput{
		StoreID:        storeID,
		EventID:        eventID,
		EventTitle:     "", // resolved below if needed
		CartID:         cartID,
		CartToken:      cartToken,
		PlatformUserID: next.PlatformUserID,
		PlatformHandle: next.PlatformHandle,
		ProductName:    product.Name,
		ProductKeyword: product.Keyword,
		Quantity:       taken,
		TTL:            ttl,
	})

	// Definitive success point: every revert path above returns early, so
	// reaching this line means the buyer was actually promoted. Emit the
	// provisional stock.reserved (op waitlist_promote) here, keyed by the cart.
	s.stock.NoteReserved(ctx, ReserveParams{Op: StockOpWaitlistPromote, ProductID: productID, Quantity: taken, CartID: cartID, EventID: eventID})

	// waitlist.notified — emitted only here, at the definitive success point:
	// every revert path above returns early, so reaching this line means the
	// buyer was actually promoted and notified. Best-effort (the promotion state
	// is already committed across several steps).
	if emitErr := s.repo.EmitWaitlistNotified(ctx, EmitWaitlistNotifiedParams{
		WaitlistItemID: next.ID,
		EventID:        eventID,
		ProductID:      productID,
		CartID:         cartID,
		Quantity:       taken,
		Remaining:      remaining,
	}); emitErr != nil {
		logger.From(ctx, s.logger).Warn("failed to emit waitlist.notified",
			zap.String("waitlist_item_id", next.ID),
			zap.Error(emitErr),
		)
	}

	logger.From(ctx, s.logger).Info("waitlist promoted",
		zap.String("user", next.PlatformHandle),
		zap.String("waitlist_item_id", next.ID),
		zap.String("cart_id", cartID),
		zap.String("product_id", productID),
		zap.Int("requested", next.Quantity),
		zap.Int("promoted", taken),
		zap.Int("still_waiting", remaining),
		zap.Bool("partial", remaining > 0),
		zap.Duration("ttl", ttl),
		zap.Time("notified_until", notifiedUntil),
	)
}

// ListActiveWaitlistByCart é a leitura usada pelo checkout para popular a
// seção "produtos em fila". Retorna apenas waiting/notified.
func (s *Service) ListActiveWaitlistByCart(ctx context.Context, cartID string) ([]ListActiveByCartRow, error) {
	return s.repo.ListActiveByCart(ctx, cartID)
}

// CancelWaitlistItem é a operação pública "sair da fila": cliente desiste
// de uma entry. Quando estava 'notified' (já promovido para o cart), o
// stock volta para o próximo da fila — mesmo fluxo do worker de expiração.
// Quando estava 'waiting', apenas marca como 'cancelled'.
//
// Ownership é validada pela query (cart_id no WHERE de CancelWaitlistItem).
// Retorna (true) se algo foi alterado, (false) se a row não existia ou já
// estava em estado terminal.
func (s *Service) CancelWaitlistItem(ctx context.Context, waitlistItemID, cartID string) (bool, error) {
	// Carrega antes do UPDATE para saber se precisamos disparar a
	// devolução de estoque (status='notified').
	item, err := s.repo.GetWaitlistItemForCart(ctx, waitlistItemID, cartID)
	if err != nil {
		return false, fmt.Errorf("loading waitlist item: %w", err)
	}
	if item == nil {
		return false, nil
	}
	if item.Status != "waiting" && item.Status != "notified" {
		// Já fulfilled / expired / cancelled — no-op.
		return false, nil
	}
	if err := s.repo.CancelWaitlistItem(ctx, waitlistItemID, cartID); err != nil {
		return false, fmt.Errorf("cancelling waitlist item: %w", err)
	}

	if item.Status == "notified" {
		// O cliente já tinha o item reservado no cart + ERP. Devolve
		// tudo via mesmo fluxo do worker de expiração.
		if _, err := s.repo.DecrementCartItem(ctx, cartID, item.ProductID, item.Quantity); err != nil {
			logger.From(ctx, s.logger).Warn("failed to decrement cart item on waitlist cancel",
				zap.String("waitlist_item_id", waitlistItemID),
				zap.Error(err),
			)
		}
		cart, _ := s.repo.GetCartByID(ctx, cartID)
		if cart != nil {
			// AdjustStockReservationDelta also bumps products.stock for delta<0,
			// so no separate IncrementProductStock call is needed here.
			if _, err := s.AdjustStockReservationDelta(ctx, cart.StoreID, cartID, item.EventID, item.ProductID, -item.Quantity, 0, cart.PlatformHandle, StockOpWaitlistCancel); err != nil {
				logger.From(ctx, s.logger).Warn("failed to reverse reservation on waitlist cancel",
					zap.String("waitlist_item_id", waitlistItemID),
					zap.Error(err),
				)
			}
		} else {
			// Cart desapareceu: ainda precisamos devolver o estoque local que
			// o promote consumiu, para a próxima entry da fila ser promovível.
			if err := s.stock.Release(ctx, ReleaseParams{Op: StockOpWaitlistCancel, ProductID: item.ProductID, Quantity: item.Quantity, CartID: cartID, EventID: item.EventID}); err != nil {
				logger.From(ctx, s.logger).Warn("failed to increment local stock on waitlist cancel (cart missing)",
					zap.String("waitlist_item_id", waitlistItemID),
					zap.Error(err),
				)
			}
		}
		// Promove o próximo da fila — best-effort.
		if cart != nil {
			s.ProcessWaitlistForProduct(ctx, item.EventID, item.ProductID, cart.StoreID)
		}
	}
	return true, nil
}

// ExpireNotifiedWaitlistItem expira uma entrada 'notified' cujo TTL passou:
// devolve o item ao estoque, reverte a reserva no ERP, marca como
// 'expired' e tenta promover o próximo da fila. Best-effort em todos os
// passos secundários — só falha se a marcação de status falhar (que é o
// gate de idempotência: enquanto status='notified' a row reaparece no
// próximo sweep).
func (s *Service) ExpireNotifiedWaitlistItem(ctx context.Context, item WaitlistItemRow) error {
	cartID := item.CartID
	productID := item.ProductID
	eventID := item.EventID

	if cartID != "" {
		// Devolve o item do carrinho. DecrementCartItem deleta a row se
		// zerar — mantém a invariante quantity>0.
		if _, err := s.repo.DecrementCartItem(ctx, cartID, productID, item.Quantity); err != nil {
			logger.From(ctx, s.logger).Warn("failed to decrement cart item on waitlist expire",
				zap.String("waitlist_item_id", item.ID),
				zap.String("cart_id", cartID),
				zap.String("product_id", productID),
				zap.Error(err),
			)
		}

		// Reverte a reserva no ERP e devolve o estoque local em uma única
		// chamada (AdjustStockReservationDelta lida com ambos para delta<0).
		// Falha aqui não bloqueia — o sweep tenta de novo no próximo tick.
		cart, _ := s.repo.GetCartByID(ctx, cartID)
		if cart != nil {
			if _, err := s.AdjustStockReservationDelta(ctx, cart.StoreID, cartID, eventID, productID, -item.Quantity, 0, cart.PlatformHandle, StockOpWaitlistExpire); err != nil {
				logger.From(ctx, s.logger).Warn("failed to reverse reservation on waitlist expire",
					zap.String("waitlist_item_id", item.ID),
					zap.String("cart_id", cartID),
					zap.Error(err),
				)
			}
		} else {
			// Cart desapareceu (raro): nada para reverter no ERP, mas ainda
			// precisamos devolver o estoque local que o promote consumiu.
			if err := s.stock.Release(ctx, ReleaseParams{Op: StockOpWaitlistExpire, ProductID: productID, Quantity: item.Quantity, CartID: cartID, EventID: eventID}); err != nil {
				logger.From(ctx, s.logger).Warn("failed to increment local stock on waitlist expire (cart missing)",
					zap.String("waitlist_item_id", item.ID),
					zap.Error(err),
				)
			}
		}
	}

	// Marca como expired — esse é o gate de idempotência. Se isso falhar,
	// a próxima varredura tenta de novo.
	now := time.Now()
	if err := s.repo.UpdateWaitlistItemStatus(ctx, item.ID, "expired", nil, nil, &now); err != nil {
		return fmt.Errorf("marking waitlist item expired: %w", err)
	}

	// waitlist.expired — emitido best-effort logo após o gate de idempotência
	// (o flip para 'expired'). Só chega aqui uma vez por item graças ao gate.
	if emitErr := s.repo.EmitWaitlistExpired(ctx, item.ID, eventID, productID, cartID); emitErr != nil {
		logger.From(ctx, s.logger).Warn("failed to emit waitlist.expired",
			zap.String("waitlist_item_id", item.ID),
			zap.Error(emitErr),
		)
	}

	// Tenta promover o próximo da fila para esse evento+produto.
	storeID := ""
	if item.CartID != "" {
		if cart, _ := s.repo.GetCartByID(ctx, item.CartID); cart != nil {
			storeID = cart.StoreID
		}
	}
	if storeID == "" {
		// Fallback: resolve via evento (se cart sumiu por algum motivo).
		// ProcessWaitlistForProduct tolera storeID vazio na lookup do
		// produto? Não — então só pulamos a promoção. O Tiny webhook
		// pega depois.
		logger.From(ctx, s.logger).Info("waitlist expired but storeID unresolved, skipping next-promotion",
			zap.String("waitlist_item_id", item.ID),
			zap.String("event_id", eventID),
		)
		return nil
	}
	s.ProcessWaitlistForProduct(ctx, eventID, productID, storeID)
	return nil
}

// ExpireNotifiedWaitlistSweep busca todos os 'notified' vencidos e chama
// ExpireNotifiedWaitlistItem em cada um. Retorna a contagem de processados
// e o primeiro erro encontrado (não interrompe — best-effort).
func (s *Service) ExpireNotifiedWaitlistSweep(ctx context.Context) (int, error) {
	items, err := s.repo.ListExpiredNotifiedWaitlist(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing expired notified waitlist: %w", err)
	}
	processed := 0
	var firstErr error
	for _, item := range items {
		if err := s.ExpireNotifiedWaitlistItem(ctx, item); err != nil {
			logger.From(ctx, s.logger).Warn("failed to expire notified waitlist item",
				zap.String("waitlist_item_id", item.ID),
				zap.Error(err),
			)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		processed++
	}
	return processed, firstErr
}

// ProcessWaitlistAfterStockWebhook é o backstop para mudanças de estoque
// vindas do ERP que não foram disparadas por uma operação local (ex.:
// merchant ajusta saldo manualmente no Tiny, devolução, importação).
// Resolve o produto local pelo external_id e tenta promover o próximo da
// fila em cada evento ativo. ProcessWaitlistForProduct é idempotente e
// gateado por DecrementProductStock — chamadas concorrentes não causam
// over-promotion.
func (s *Service) ProcessWaitlistAfterStockWebhook(ctx context.Context, storeID, externalSource, externalProductID string) error {
	productID, err := s.repo.GetProductIDByExternalID(ctx, storeID, externalSource, externalProductID)
	if err != nil {
		return fmt.Errorf("resolving product by external id: %w", err)
	}
	if productID == "" {
		// Produto não cadastrado no LiveCart — não temos fila para ele.
		return nil
	}

	// Defesa em profundidade: com finalização ERP em voo para algum cart pago
	// contendo o produto, o saldo reportado pelo webhook pode ser a inflação
	// transitória da reversão de reservas — promover agora enviaria a DM
	// (irreversível) contra unidades já vendidas. A promoção não é perdida:
	// o LaunchOrderStock/estornos subsequentes disparam novos webhooks e o
	// backstop roda de novo com o guard já desarmado. Fail-safe: em erro de
	// DB, adia a promoção (direção conservadora).
	inFlight, guardErr := s.repo.HasInFlightFinalisationForProduct(ctx, productID)
	if guardErr != nil {
		logger.From(ctx, s.logger).Warn("failed to check in-flight finalisation, deferring waitlist backstop",
			zap.String("product_id", productID),
			zap.Error(guardErr),
		)
		return nil
	}
	if inFlight {
		logger.From(ctx, s.logger).Info("deferring waitlist backstop: ERP finalisation in flight for product",
			zap.String("product_id", productID),
			zap.String("store_id", storeID),
		)
		return nil
	}

	eventIDs, err := s.repo.ListEventsWithWaitingByProduct(ctx, productID)
	if err != nil {
		return fmt.Errorf("listing events with waiting waitlist: %w", err)
	}
	for _, eventID := range eventIDs {
		s.ProcessWaitlistForProduct(ctx, eventID, productID, storeID)
	}
	return nil
}

// sendWaitlistNotifiedInput é o payload da DM "produto liberou".
type sendWaitlistNotifiedInput struct {
	StoreID        string
	EventID        string
	EventTitle     string
	CartID         string
	CartToken      string
	PlatformUserID string
	PlatformHandle string
	ProductName    string
	ProductKeyword string
	Quantity       int
	TTL            time.Duration
}

func (s *Service) sendWaitlistNotifiedDM(ctx context.Context, input sendWaitlistNotifiedInput) {
	if s.notificationService == nil {
		return
	}
	shouldNotify, err := s.notificationService.ShouldNotify(ctx, input.StoreID, notification.TypeWaitlistNotified, false)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to check notification settings for waitlist",
			zap.String("store_id", input.StoreID),
			zap.Error(err),
		)
		return
	}
	if !shouldNotify {
		return
	}

	storeInfo, err := s.repo.GetStoreInfo(ctx, input.StoreID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to get store info for waitlist notification",
			zap.String("store_id", input.StoreID),
			zap.Error(err),
		)
		return
	}

	frontendURL := config.FrontendURL.StringOr("http://localhost:3000")
	checkoutURL := fmt.Sprintf("%s/cart/%s", frontendURL, input.CartToken)

	vars := notification.TemplateVariables{
		Handle:     "@" + input.PlatformHandle,
		Produto:    input.ProductName,
		Keyword:    input.ProductKeyword,
		Quantidade: input.Quantity,
		Link:       checkoutURL,
		Loja:       storeInfo.Name,
		ExpiraEm:   notification.FormatExpiryMinutes(int(input.TTL.Minutes())),
		LiveTitulo: input.EventTitle,
	}

	result, err := s.notificationService.Send(ctx, notification.SendInput{
		StoreID:          input.StoreID,
		EventID:          input.EventID,
		CartID:           input.CartID,
		CartToken:        input.CartToken,
		PlatformUserID:   input.PlatformUserID,
		PlatformHandle:   input.PlatformHandle,
		NotificationType: notification.TypeWaitlistNotified,
		Variables:        vars,
	})
	if err != nil {
		logger.From(ctx, s.logger).Warn("waitlist notification send error",
			zap.String("store_id", input.StoreID),
			zap.String("cart_id", input.CartID),
			zap.Error(err),
		)
		return
	}
	logger.From(ctx, s.logger).Info("waitlist notification dispatched",
		zap.String("store_id", input.StoreID),
		zap.String("cart_id", input.CartID),
		zap.String("status", string(result.Status)),
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
		logger.From(ctx, s.logger).Debug("checking token expiration",
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
		logger.From(ctx, s.logger).Info("token expired, attempting refresh",
			zap.String("integration_id", integration.ID),
			zap.String("provider", integration.Provider),
			zap.Time("expires_at", creds.ExpiresAt),
		)
		creds, err = s.refreshToken(ctx, integration, creds)
		if err != nil {
			logger.From(ctx, s.logger).Warn("failed to refresh token",
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

	logger.From(ctx, s.logger).Info("token refreshed successfully",
		zap.String("integration_id", integration.ID),
	)

	return newCreds, nil
}

func (s *Service) toCreateOutput(row *IntegrationRow) *CreateIntegrationOutput {
	urls := buildProviderURLs(row.Provider, row.StoreID)

	var lastPing *time.Time
	if row.Metadata != nil {
		if raw, ok := row.Metadata["webhookLastPingAt"].(string); ok && raw != "" {
			if t, err := time.Parse(time.RFC3339, raw); err == nil {
				lastPing = &t
			}
		}
	}

	// Only surface a webhook status when the provider actually has a webhook
	// URL — otherwise the field is meaningless and would confuse consumers.
	var webhookStatus string
	if urls.WebhookURL != "" {
		if lastPing != nil {
			webhookStatus = "active"
		} else {
			webhookStatus = "pending"
		}
	}

	return &CreateIntegrationOutput{
		ID:                row.ID,
		StoreID:           row.StoreID,
		Type:              row.Type,
		Provider:          row.Provider,
		Status:            row.Status,
		Metadata:          row.Metadata,
		LastSyncedAt:      row.LastSyncedAt,
		CreatedAt:         row.CreatedAt,
		RedirectURL:       urls.RedirectURL,
		WebhookURL:        urls.WebhookURL,
		WebhookStatus:     webhookStatus,
		WebhookLastPingAt: lastPing,
		Priority:          row.Priority,
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
		logger.From(ctx, s.logger).Error("provider rate limited",
			zap.String("integration_id", integrationID),
			zap.String("operation", operation),
			zap.Duration("retry_after", rateLimitErr.RetryAfter),
		)

		// Mark integration as error so it's visible in the dashboard
		if updateErr := s.repo.UpdateStatus(ctx, integrationID, "error"); updateErr != nil {
			logger.From(ctx, s.logger).Warn("failed to update integration status after rate limit",
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
