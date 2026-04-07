package social

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/ratelimit"
)

const instagramGraphAPIBaseURL = "https://graph.instagram.com"

// Instagram implements the SocialProvider interface for Instagram.
type Instagram struct {
	integrationID string
	storeID       string
	credentials   *providers.Credentials
	logger        *zap.Logger
	logFunc       providers.LogFunc
	rateLimiter   ratelimit.RateLimiter
	client        *http.Client
}

// InstagramConfig contains configuration for Instagram provider.
type InstagramConfig struct {
	IntegrationID string
	StoreID       string
	Credentials   *providers.Credentials
	Logger        *zap.Logger
	LogFunc       providers.LogFunc
	RateLimiter   ratelimit.RateLimiter
}

// NewInstagram creates a new Instagram provider instance.
func NewInstagram(cfg InstagramConfig) (providers.SocialProvider, error) {
	if cfg.Credentials == nil || cfg.Credentials.AccessToken == "" {
		return nil, fmt.Errorf("instagram credentials are required")
	}

	return &Instagram{
		integrationID: cfg.IntegrationID,
		storeID:       cfg.StoreID,
		credentials:   cfg.Credentials,
		logger:        cfg.Logger,
		logFunc:       cfg.LogFunc,
		rateLimiter:   cfg.RateLimiter,
		client:        &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// Type returns the provider type.
func (i *Instagram) Type() providers.ProviderType {
	return providers.ProviderTypeSocial
}

// Name returns the provider name.
func (i *Instagram) Name() providers.ProviderName {
	return providers.ProviderInstagram
}

// ValidateCredentials checks if the credentials are valid.
func (i *Instagram) ValidateCredentials(ctx context.Context) error {
	_, err := i.GetProfile(ctx)
	return err
}

// RefreshToken refreshes the OAuth token.
func (i *Instagram) RefreshToken(ctx context.Context) (*providers.Credentials, error) {
	// Instagram long-lived tokens can be refreshed
	// This would be implemented when needed
	return nil, nil
}

// TestConnection tests the connection to Instagram API.
func (i *Instagram) TestConnection(ctx context.Context) (*providers.TestConnectionResult, error) {
	start := time.Now()

	profile, err := i.GetProfile(ctx)
	latency := time.Since(start)

	result := &providers.TestConnectionResult{
		Latency:  latency,
		TestedAt: time.Now(),
	}

	if err != nil {
		result.Success = false
		result.Message = fmt.Sprintf("Falha ao conectar: %v", err)
		return result, nil
	}

	result.Success = true
	result.Message = fmt.Sprintf("Conectado como @%s", profile.Username)
	result.AccountInfo = map[string]any{
		"id":       profile.ID,
		"username": profile.Username,
		"name":     profile.Name,
	}

	return result, nil
}

// GetProfile retrieves the connected Instagram account profile.
func (i *Instagram) GetProfile(ctx context.Context) (*providers.SocialProfile, error) {
	url := fmt.Sprintf("%s/me?fields=id,username,name&access_token=%s",
		instagramGraphAPIBaseURL,
		i.credentials.AccessToken,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := i.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("instagram API error: status %d", resp.StatusCode)
	}

	var profileResp struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Name     string `json:"name"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&profileResp); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &providers.SocialProfile{
		ID:       profileResp.ID,
		Username: profileResp.Username,
		Name:     profileResp.Name,
	}, nil
}
