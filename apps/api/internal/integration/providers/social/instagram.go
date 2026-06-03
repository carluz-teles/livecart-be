package social

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
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

// SendPrivateReply sends a private DM to the user who made a comment.
// This uses the Instagram Private Reply feature which sends a DM in response to a comment.
// Unlike ReplyToComment (which posts publicly), this sends a private message.
// commentID is the Instagram comment ID to reply to.
// text is the reply message (max 1000 characters).
func (i *Instagram) SendPrivateReply(ctx context.Context, commentID, text string) error {
	if commentID == "" {
		return fmt.Errorf("comment id is required")
	}
	if text == "" {
		return fmt.Errorf("reply text is required")
	}
	if len(text) > 1000 {
		text = text[:997] + "..."
	}

	// Instagram Graph API: POST /me/messages with recipient.comment_id
	// This sends a private DM to the commenter
	url := fmt.Sprintf("%s/%s/me/messages",
		instagramGraphAPIBaseURL,
		instagramGraphAPIVersion,
	)

	payload := map[string]any{
		"recipient": map[string]string{
			"comment_id": commentID,
		},
		"message": map[string]string{
			"text": text,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling private reply payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating private reply request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+i.credentials.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := i.client.Do(req)
	if err != nil {
		return fmt.Errorf("sending private reply request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		bodyStr := string(respBody)
		if len(bodyStr) > 256 {
			bodyStr = bodyStr[:256] + "..."
		}
		i.logger.Error("instagram private reply failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", bodyStr),
			zap.String("comment_id", commentID),
		)
		return fmt.Errorf("instagram private reply failed: status %d, body: %s", resp.StatusCode, bodyStr)
	}

	i.logger.Info("instagram private reply sent",
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

// HideComment hides or unhides an Instagram comment.
// Instagram Graph API: POST /{comment-id} with hide=true|false.
// Instagram has no endpoint to edit a comment's text, so hide/unhide is the
// supported "update" moderation action.
func (i *Instagram) HideComment(ctx context.Context, commentID string, hidden bool) error {
	if commentID == "" {
		return fmt.Errorf("comment id is required")
	}

	url := fmt.Sprintf("%s/%s/%s?hide=%t",
		instagramGraphAPIBaseURL,
		instagramGraphAPIVersion,
		commentID,
		hidden,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("creating hide request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+i.credentials.AccessToken)

	resp, err := i.client.Do(req)
	if err != nil {
		return fmt.Errorf("sending hide request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		bodyStr := string(respBody)
		if len(bodyStr) > 256 {
			bodyStr = bodyStr[:256] + "..."
		}
		i.logger.Error("instagram hide comment failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", bodyStr),
			zap.String("comment_id", commentID),
		)
		return fmt.Errorf("instagram hide comment failed: status %d, body: %s", resp.StatusCode, bodyStr)
	}

	i.logger.Info("instagram comment hidden",
		zap.String("comment_id", commentID),
		zap.Bool("hidden", hidden),
	)
	return nil
}

// DeleteComment deletes an Instagram comment owned by the connected account.
// Instagram Graph API: DELETE /{comment-id}.
func (i *Instagram) DeleteComment(ctx context.Context, commentID string) error {
	if commentID == "" {
		return fmt.Errorf("comment id is required")
	}

	url := fmt.Sprintf("%s/%s/%s",
		instagramGraphAPIBaseURL,
		instagramGraphAPIVersion,
		commentID,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("creating delete request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+i.credentials.AccessToken)

	resp, err := i.client.Do(req)
	if err != nil {
		return fmt.Errorf("sending delete request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		bodyStr := string(respBody)
		if len(bodyStr) > 256 {
			bodyStr = bodyStr[:256] + "..."
		}
		i.logger.Error("instagram delete comment failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", bodyStr),
			zap.String("comment_id", commentID),
		)
		return fmt.Errorf("instagram delete comment failed: status %d, body: %s", resp.StatusCode, bodyStr)
	}

	i.logger.Info("instagram comment deleted",
		zap.String("comment_id", commentID),
	)
	return nil
}

// GetUserMedia lists recent published posts/reels (newest first) for the post
// selector. `after` pages through results using the Graph API cursor.
func (i *Instagram) GetUserMedia(ctx context.Context, limit int, after string) (*providers.MediaPage, error) {
	if limit <= 0 || limit > 50 {
		limit = 24
	}

	url := fmt.Sprintf("%s/%s/me/media?fields=id,caption,media_type,media_url,thumbnail_url,permalink,timestamp,comments_count&limit=%d&access_token=%s",
		instagramGraphAPIBaseURL,
		instagramGraphAPIVersion,
		limit,
		i.credentials.AccessToken,
	)
	if after != "" {
		url += "&after=" + neturl.QueryEscape(after)
	}

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
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("instagram API error: status %d, body: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Data   []providers.MediaPost `json:"data"`
		Paging struct {
			Cursors struct {
				After string `json:"after"`
			} `json:"cursors"`
			Next string `json:"next"`
		} `json:"paging"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	// Only expose a cursor when there is actually a next page.
	nextCursor := ""
	if result.Paging.Next != "" {
		nextCursor = result.Paging.Cursors.After
	}

	i.logger.Info("fetched instagram user media",
		zap.Int("count", len(result.Data)),
		zap.Bool("has_more", nextCursor != ""),
	)
	return &providers.MediaPage{Posts: result.Data, After: nextCursor}, nil
}

// PublishImagePost creates an image feed container and publishes it, returning
// the published media id. The image must be a public JPEG URL Instagram can
// fetch. Two-step flow: POST /me/media then POST /me/media_publish.
func (i *Instagram) PublishImagePost(ctx context.Context, imageURL, caption string) (string, error) {
	if imageURL == "" {
		return "", fmt.Errorf("image url is required")
	}

	// Step 1 — create the media container. The container-create endpoint reads
	// its parameters from the query string, so we pass image_url/caption there
	// (sending them only in the JSON body can drop the caption).
	createURL := fmt.Sprintf("%s/%s/me/media?image_url=%s&caption=%s&access_token=%s",
		instagramGraphAPIBaseURL,
		instagramGraphAPIVersion,
		neturl.QueryEscape(imageURL),
		neturl.QueryEscape(caption),
		neturl.QueryEscape(i.credentials.AccessToken),
	)
	containerID, err := i.postGraphURL(ctx, createURL)
	if err != nil {
		return "", fmt.Errorf("creating media container: %w", err)
	}

	// Step 2 — publish the container.
	mediaID, err := i.postGraph(ctx, "/me/media_publish", map[string]any{
		"creation_id": containerID,
	})
	if err != nil {
		return "", fmt.Errorf("publishing media: %w", err)
	}

	i.logger.Info("instagram image post published",
		zap.String("container_id", containerID),
		zap.String("media_id", mediaID),
	)
	return mediaID, nil
}

// postGraph POSTs a JSON body to a graph endpoint and returns the response "id".
func (i *Instagram) postGraph(ctx context.Context, path string, payload map[string]any) (string, error) {
	url := fmt.Sprintf("%s/%s%s", instagramGraphAPIBaseURL, instagramGraphAPIVersion, path)
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshaling payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+i.credentials.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	return i.doGraphIDRequest(req)
}

// postGraphURL POSTs to a fully-built URL (params in the query string) and
// returns the response "id".
func (i *Instagram) postGraphURL(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	return i.doGraphIDRequest(req)
}

// doGraphIDRequest executes a request expected to return a JSON object with "id".
func (i *Instagram) doGraphIDRequest(req *http.Request) (string, error) {
	resp, err := i.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("instagram API error: status %d, body: %s", resp.StatusCode, truncate(string(respBody), 300))
	}

	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("instagram API returned no id")
	}
	return out.ID, nil
}

// PublishReel publishes a Reel by streaming the video bytes directly to
// Instagram via the resumable upload protocol — no public hosting required.
// Flow: init resumable container -> upload bytes -> poll status until FINISHED
// -> media_publish. Returns the published media id.
func (i *Instagram) PublishReel(ctx context.Context, video io.Reader, size int64, caption string) (string, error) {
	if video == nil || size <= 0 {
		return "", fmt.Errorf("video is required")
	}

	// Step 1 — init a resumable container.
	containerID, uploadURI, err := i.initResumableReel(ctx, caption)
	if err != nil {
		return "", fmt.Errorf("init reel container: %w", err)
	}

	// Step 2 — upload the bytes to the returned URI.
	if uploadURI == "" {
		uploadURI = fmt.Sprintf("https://rupload.facebook.com/ig-api-upload/%s/%s", instagramGraphAPIVersion, containerID)
	}
	if err := i.uploadResumable(ctx, uploadURI, video, size); err != nil {
		return "", fmt.Errorf("uploading reel: %w", err)
	}

	// Step 3 — wait for Instagram to finish processing the video.
	if err := i.waitContainerFinished(ctx, containerID); err != nil {
		return "", err
	}

	// Step 4 — publish.
	mediaID, err := i.postGraph(ctx, "/me/media_publish", map[string]any{"creation_id": containerID})
	if err != nil {
		return "", fmt.Errorf("publishing reel: %w", err)
	}

	i.logger.Info("instagram reel published",
		zap.String("container_id", containerID),
		zap.String("media_id", mediaID),
	)
	return mediaID, nil
}

// initResumableReel creates a REELS container with upload_type=resumable and
// returns the container id and the upload URI (when provided by the API).
func (i *Instagram) initResumableReel(ctx context.Context, caption string) (containerID, uploadURI string, err error) {
	url := fmt.Sprintf("%s/%s/me/media", instagramGraphAPIBaseURL, instagramGraphAPIVersion)
	payload := map[string]any{
		"media_type":  "REELS",
		"upload_type": "resumable",
		"caption":     caption,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+i.credentials.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := i.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("status %d, body: %s", resp.StatusCode, truncate(string(respBody), 300))
	}
	var out struct {
		ID  string `json:"id"`
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", "", err
	}
	if out.ID == "" {
		return "", "", fmt.Errorf("no container id returned")
	}
	return out.ID, out.URI, nil
}

// uploadResumable streams the video bytes to the resumable upload URI.
func (i *Instagram) uploadResumable(ctx context.Context, uploadURI string, video io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURI, video)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "OAuth "+i.credentials.AccessToken)
	req.Header.Set("offset", "0")
	req.Header.Set("file_size", fmt.Sprintf("%d", size))
	req.ContentLength = size

	resp, err := i.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload status %d, body: %s", resp.StatusCode, truncate(string(respBody), 300))
	}
	return nil
}

// waitContainerFinished polls the container status until FINISHED (or fails).
// Instagram recommends polling ~once/minute for up to 5 minutes; we poll a bit
// more frequently with the same overall budget.
func (i *Instagram) waitContainerFinished(ctx context.Context, containerID string) error {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		status, err := i.containerStatus(ctx, containerID)
		if err != nil {
			return err
		}
		switch status {
		case "FINISHED":
			return nil
		case "ERROR", "EXPIRED":
			return fmt.Errorf("instagram video processing failed: %s", status)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("instagram video still processing after timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(6 * time.Second):
		}
	}
}

// containerStatus reads the status_code of a media container.
func (i *Instagram) containerStatus(ctx context.Context, containerID string) (string, error) {
	url := fmt.Sprintf("%s/%s/%s?fields=status_code&access_token=%s",
		instagramGraphAPIBaseURL, instagramGraphAPIVersion, containerID, i.credentials.AccessToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := i.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d, body: %s", resp.StatusCode, truncate(string(respBody), 200))
	}
	var out struct {
		StatusCode string `json:"status_code"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", err
	}
	return out.StatusCode, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// GetMediaDetails fetches metadata (permalink, thumbnail, caption) for a media id.
func (i *Instagram) GetMediaDetails(ctx context.Context, mediaID string) (*providers.MediaPost, error) {
	if mediaID == "" {
		return nil, fmt.Errorf("media id is required")
	}
	url := fmt.Sprintf("%s/%s/%s?fields=id,caption,media_type,media_url,thumbnail_url,permalink,timestamp&access_token=%s",
		instagramGraphAPIBaseURL,
		instagramGraphAPIVersion,
		mediaID,
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
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("instagram API error: status %d, body: %s", resp.StatusCode, string(respBody))
	}
	var post providers.MediaPost
	if err := json.NewDecoder(resp.Body).Decode(&post); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &post, nil
}

// GetMediaComments lists top-level comments on a media object.
func (i *Instagram) GetMediaComments(ctx context.Context, mediaID string) ([]providers.MediaComment, error) {
	if mediaID == "" {
		return nil, fmt.Errorf("media id is required")
	}

	url := fmt.Sprintf("%s/%s/%s/comments?fields=id,text,timestamp,username,from&access_token=%s",
		instagramGraphAPIBaseURL,
		instagramGraphAPIVersion,
		mediaID,
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
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("instagram API error: status %d, body: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Data []providers.MediaComment `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return result.Data, nil
}
