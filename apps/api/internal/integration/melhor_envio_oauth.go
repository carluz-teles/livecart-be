package integration

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/integration/providers/shipping"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
)

// Scopes requested for the Melhor Envio app. LiveCart only needs read-only
// endpoints (quote + list carriers); we ask for users-read as a convenience
// for account display.
var melhorEnvioScopes = []string{
	"shipping-calculate",
	"shipping-companies",
	"users-read",
}

// getMelhorEnvioOAuthURL builds the Melhor Envio authorize URL and persists
// the state so the callback can recover the store ID.
func (s *Service) getMelhorEnvioOAuthURL(storeID string) (*GetOAuthURLOutput, error) {
	clientID := config.MelhorEnvioClientID.String()
	if clientID == "" {
		return nil, httpx.ErrUnprocessable("Melhor Envio app not configured")
	}

	redirectURI := config.MelhorEnvioRedirectURI.String()
	if redirectURI == "" {
		redirectURI = config.WebhookBaseURL.String() + "/api/v1/integrations/oauth/melhor_envio/callback"
	}

	env := config.MelhorEnvioEnv.StringOr("sandbox")

	state := uuid.New().String()
	ctx := context.Background()
	if err := s.repo.CreateOAuthState(ctx, state, storeID, "melhor_envio", ""); err != nil {
		return nil, fmt.Errorf("storing OAuth state: %w", err)
	}

	authURL := shipping.BuildAuthorizeURL(env, clientID, redirectURI, state, melhorEnvioScopes)
	return &GetOAuthURLOutput{
		AuthURL: authURL,
		State:   state,
	}, nil
}

// handleMelhorEnvioCallback exchanges the authorization code for tokens, then
// creates or updates the shipping integration for the store.
func (s *Service) handleMelhorEnvioCallback(ctx context.Context, input OAuthCallbackInput) (*OAuthCallbackOutput, error) {
	clientID := config.MelhorEnvioClientID.String()
	clientSecret := config.MelhorEnvioClientSecret.String()
	if clientID == "" || clientSecret == "" {
		return nil, httpx.ErrUnprocessable("Melhor Envio app not configured")
	}

	env := config.MelhorEnvioEnv.StringOr("sandbox")
	redirectURI := config.MelhorEnvioRedirectURI.String()
	if redirectURI == "" {
		redirectURI = config.WebhookBaseURL.String() + "/api/v1/integrations/oauth/melhor_envio/callback"
	}
	userAgent := config.MelhorEnvioUserAgent.StringOr("LiveCart (contato@livecart.com.br)")

	// Validate state and recover the store ID
	oauthState, err := s.repo.GetOAuthState(ctx, input.State)
	if err != nil {
		s.logger.Error("melhor envio OAuth state not found",
			zap.String("state", input.State),
			zap.Error(err),
		)
		return nil, httpx.ErrUnprocessable("OAuth state expired or invalid")
	}
	defer s.repo.DeleteOAuthState(ctx, input.State)

	storeID := oauthState.StoreID.String()

	creds, err := shipping.ExchangeAuthorizationCode(ctx, env, clientID, clientSecret, redirectURI, input.Code, userAgent)
	if err != nil {
		return nil, fmt.Errorf("exchanging Melhor Envio code: %w", err)
	}
	if creds.Extra == nil {
		creds.Extra = map[string]any{}
	}
	creds.Extra["env"] = env

	encryptedCreds, err := s.encryptor.EncryptJSON(creds)
	if err != nil {
		return nil, fmt.Errorf("encrypting credentials: %w", err)
	}
	tokenExpiresAt := creds.ExpiresAt

	existing, _ := s.repo.GetActiveByProvider(ctx, storeID, string(providers.ProviderTypeShipping), "melhor_envio")
	var integrationID string
	if existing != nil {
		if err := s.repo.UpdateCredentials(ctx, existing.ID, encryptedCreds, &tokenExpiresAt); err != nil {
			return nil, fmt.Errorf("updating credentials: %w", err)
		}
		if err := s.repo.UpdateStatus(ctx, existing.ID, "active"); err != nil {
			return nil, fmt.Errorf("updating status: %w", err)
		}
		integrationID = existing.ID
	} else {
		row, err := s.repo.Create(ctx, CreateIntegrationParams{
			StoreID:        storeID,
			Type:           string(providers.ProviderTypeShipping),
			Provider:       "melhor_envio",
			Status:         "active",
			Credentials:    encryptedCreds,
			TokenExpiresAt: &tokenExpiresAt,
			Metadata: map[string]any{
				"env":          env,
				"scopes":       strings.Join(melhorEnvioScopes, " "),
				"connected_at": time.Now(),
			},
		})
		if err != nil {
			return nil, fmt.Errorf("creating integration: %w", err)
		}
		integrationID = row.ID
	}

	s.logger.Info("Melhor Envio OAuth completed",
		zap.String("store_id", storeID),
		zap.String("integration_id", integrationID),
		zap.String("env", env),
	)

	return &OAuthCallbackOutput{
		IntegrationID: integrationID,
		StoreID:       storeID,
		Provider:      "melhor_envio",
		Status:        "active",
	}, nil
}
