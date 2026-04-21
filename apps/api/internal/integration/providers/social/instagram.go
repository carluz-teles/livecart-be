package social

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/ratelimit"
)

const (
	instagramGraphAPIBaseURL = "https://graph.instagram.com"
	instagramGraphAPIVersion = "v25.0"
	instagramDMTextMaxBytes  = 1000
)

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

// SendDirectMessage sends a text DM to a user via Instagram Graph API.
// recipientID must be the Instagram-scoped ID (IGSID) of the recipient.
// Uses HUMAN_AGENT tag to extend messaging window from 24h to 7 days.
// If HUMAN_AGENT fails (not approved), falls back to standard messaging.
func (i *Instagram) SendDirectMessage(ctx context.Context, recipientID, text string) error {
	if recipientID == "" {
		return fmt.Errorf("recipient id is required")
	}
	if text == "" {
		return fmt.Errorf("message text is required")
	}
	if len(text) > instagramDMTextMaxBytes {
		return fmt.Errorf("message text exceeds %d bytes", instagramDMTextMaxBytes)
	}

	url := fmt.Sprintf("%s/%s/me/messages", instagramGraphAPIBaseURL, instagramGraphAPIVersion)

	// Try with HUMAN_AGENT tag first (extends window to 7 days)
	payload := map[string]any{
		"recipient":      map[string]string{"id": recipientID},
		"message":        map[string]string{"text": text},
		"messaging_type": "MESSAGE_TAG",
		"tag":            "HUMAN_AGENT",
	}

	err := i.sendDMRequest(ctx, url, payload, recipientID, text)
	if err == nil {
		return nil
	}

	// If HUMAN_AGENT fails, try standard message (24h window)
	i.logger.Warn("HUMAN_AGENT tag failed, trying standard message",
		zap.String("recipient_id", recipientID),
		zap.Error(err),
	)

	payload = map[string]any{
		"recipient": map[string]string{"id": recipientID},
		"message":   map[string]string{"text": text},
	}

	return i.sendDMRequest(ctx, url, payload, recipientID, text)
}

// sendDMRequest handles the actual HTTP request for sending DMs.
func (i *Instagram) sendDMRequest(ctx context.Context, url string, payload map[string]any, recipientID, text string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling dm payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating dm request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+i.credentials.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := i.client.Do(req)
	if err != nil {
		return fmt.Errorf("sending dm request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		bodyStr := string(respBody)
		if len(bodyStr) > 256 {
			bodyStr = bodyStr[:256] + "..."
		}
		i.logger.Error("instagram send dm failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", bodyStr),
			zap.String("recipient_id", recipientID),
		)
		return fmt.Errorf("instagram send dm failed: status %d, body: %s", resp.StatusCode, bodyStr)
	}

	i.logger.Info("instagram dm sent",
		zap.String("recipient_id", recipientID),
		zap.Int("text_bytes", len(text)),
	)
	return nil
}

// ReplyToComment replies to an Instagram comment (live or post).
// This method does NOT have the 24h messaging window restriction.
// commentID is the Instagram comment ID to reply to.
// text is the reply message (max 1000 characters).
func (i *Instagram) ReplyToComment(ctx context.Context, commentID, text string) error {
	if commentID == "" {
		return fmt.Errorf("comment id is required")
	}
	if text == "" {
		return fmt.Errorf("reply text is required")
	}
	if len(text) > 1000 {
		text = text[:997] + "..."
	}

	// Instagram Graph API: POST /{comment-id}/replies
	url := fmt.Sprintf("%s/%s/%s/replies",
		instagramGraphAPIBaseURL,
		instagramGraphAPIVersion,
		commentID,
	)

	payload := map[string]string{
		"message": text,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling reply payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating reply request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+i.credentials.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := i.client.Do(req)
	if err != nil {
		return fmt.Errorf("sending reply request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		bodyStr := string(respBody)
		if len(bodyStr) > 256 {
			bodyStr = bodyStr[:256] + "..."
		}
		i.logger.Error("instagram reply to comment failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", bodyStr),
			zap.String("comment_id", commentID),
		)
		return fmt.Errorf("instagram reply failed: status %d, body: %s", resp.StatusCode, bodyStr)
	}

	i.logger.Info("instagram comment reply sent",
		zap.String("comment_id", commentID),
		zap.Int("text_bytes", len(text)),
	)
	return nil
}

// GetActiveLives retrieves all live videos currently being broadcast by the user.
// This endpoint only returns lives that are actively streaming at the time of the request.
func (i *Instagram) GetActiveLives(ctx context.Context) ([]providers.LiveMedia, error) {
	url := fmt.Sprintf("%s/%s/me/live_media?fields=id,media_type,media_product_type,username,timestamp&access_token=%s",
		instagramGraphAPIBaseURL,
		instagramGraphAPIVersion,
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

	var result struct {
		Data []providers.LiveMedia `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	i.logger.Info("fetched active instagram lives",
		zap.Int("count", len(result.Data)),
	)

	return result.Data, nil
}
