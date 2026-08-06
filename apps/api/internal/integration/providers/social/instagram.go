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
	"livecart/apps/api/lib/logger"
	"livecart/apps/api/lib/ratelimit"
)

const (
	instagramGraphAPIVersion = "v25.0"
	instagramDMTextMaxBytes  = 1000
)

// instagramGraphAPIBaseURL é var, e não const, por UM motivo: a renovação do
// token tem duas recusas da Meta cuja regressão só aparece 60 dias depois (ver
// RefreshToken), e testá-las exige apontar para uma Graph falsa. Nada em
// produção reescreve isto.
var instagramGraphAPIBaseURL = "https://graph.instagram.com"

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
		// [IGTRACE] client instrumentado — ver instagram_trace.go. TODO remover.
		client: tracedClient(cfg.Logger),
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

// RefreshToken renova o token de longa duração por mais 60 dias.
//
// O Instagram NÃO emite refresh_token: a credencial de renovação é o próprio
// token de longa duração (GET /refresh_access_token com
// grant_type=ig_refresh_token). Era por isso que este método era um stub
// `return nil, nil` e o Instagram nunca renovava — o que só não virou incidente
// porque a única consequência visível chega 60 dias depois da conexão.
//
// Com a publicação agendada (RN-31) isso deixou de ser latente: entre agendar e
// disparar podem passar semanas, e um disparo com token vencido é uma
// publicação que simplesmente não sai.
//
// Duas condições da Meta estão codificadas aqui, e violá-las devolve erro — que
// o chamador transforma em integração 'error':
//   - o token tem de ter ao menos 24h de vida. Não guardamos a data de emissão,
//     mas ExpiresAt a denuncia: token renovado há pouco vence em ~60 dias.
//   - o token não pode estar VENCIDO. Aí não há renovação possível: só
//     reconectar. Devolver erro é o certo — é o estado que o lojista precisa ver.
func (i *Instagram) RefreshToken(ctx context.Context) (*providers.Credentials, error) {
	if i.credentials == nil || i.credentials.AccessToken == "" {
		return nil, nil
	}
	// Vencido: a Graph recusa e a única saída é reconectar a conta.
	if !i.credentials.ExpiresAt.IsZero() && time.Now().After(i.credentials.ExpiresAt) {
		return nil, fmt.Errorf("instagram token expired at %s — the account must be reconnected",
			i.credentials.ExpiresAt.Format(time.RFC3339))
	}
	// Renovado há menos de 24h: a Graph recusa. Não é falha — é cedo demais.
	if !i.credentials.ExpiresAt.IsZero() && time.Until(i.credentials.ExpiresAt) > 59*24*time.Hour {
		return nil, nil
	}

	url := fmt.Sprintf("%s/refresh_access_token?grant_type=ig_refresh_token&access_token=%s",
		instagramGraphAPIBaseURL, neturl.QueryEscape(i.credentials.AccessToken))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating refresh request: %w", err)
	}
	resp, err := i.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending refresh request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("instagram token refresh failed: status %d, body: %s",
			resp.StatusCode, truncate(string(body), 300))
	}

	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decoding refresh response: %w", err)
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("instagram token refresh returned no access_token")
	}

	// Copia as credenciais para preservar o que não vem na resposta (Extra
	// carrega o instagram_user_id, que a resolução de loja usa).
	refreshed := *i.credentials
	refreshed.AccessToken = out.AccessToken
	if out.TokenType != "" {
		refreshed.TokenType = out.TokenType
	}
	if out.ExpiresIn > 0 {
		refreshed.ExpiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}

	logger.From(ctx, i.logger).Info("instagram long-lived token refreshed",
		zap.String("integration_id", i.integrationID),
		zap.Time("expires_at", refreshed.ExpiresAt),
	)
	return &refreshed, nil
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
	logger.From(ctx, i.logger).Warn("HUMAN_AGENT tag failed, trying standard message",
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
		logger.From(ctx, i.logger).Error("instagram send dm failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", bodyStr),
			zap.String("recipient_id", recipientID),
		)
		return fmt.Errorf("instagram send dm failed: status %d, body: %s", resp.StatusCode, bodyStr)
	}

	logger.From(ctx, i.logger).Info("instagram dm sent",
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
		logger.From(ctx, i.logger).Error("instagram reply to comment failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", bodyStr),
			zap.String("comment_id", commentID),
		)
		return fmt.Errorf("instagram reply failed: status %d, body: %s", resp.StatusCode, bodyStr)
	}

	logger.From(ctx, i.logger).Info("instagram comment reply sent",
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

	var lastErr error
	for attempt := 1; attempt <= privateReplyMaxAttempts; attempt++ {
		err, transient := i.sendPrivateReplyOnce(ctx, commentID, text)
		if err == nil {
			if attempt > 1 {
				logger.From(ctx, i.logger).Info("instagram private reply sent after retry",
					zap.String("comment_id", commentID),
					zap.Int("attempt", attempt),
				)
			}
			return nil
		}
		lastErr = err

		// Recusa por REGRA não vira sucesso repetindo: o 403/2534066 ("verify if
		// the comment id is valid") e o 400 são o Instagram dizendo que este
		// comentário não aceita resposta privada. Insistir só somaria latência
		// no caminho síncrono do comprador, que é o que não podemos gastar.
		if !transient {
			return err
		}
		if attempt < privateReplyMaxAttempts {
			select {
			case <-ctx.Done():
				return lastErr
			case <-time.After(privateReplyRetryDelay):
			}
		}
	}
	return lastErr
}

// privateReplyMaxAttempts é a primeira tentativa mais duas — o teto pedido para
// o atraso somado ficar abaixo de um segundo no pior caso.
const privateReplyMaxAttempts = 3

// privateReplyRetryDelay: os 500 medidos vinham em rajada curta, e o objetivo é
// atravessar o soluço sem que o comprador sinta a espera.
const privateReplyRetryDelay = 300 * time.Millisecond

// sendPrivateReplyOnce faz UMA tentativa. O segundo retorno diz se a falha é
// transitória — só erro de rede e 5xx são. O 500 "An unknown error has
// occurred" da Meta é o caso que motivou o retry: cinco dos vinte replies
// recusados numa janela de oito minutos vieram assim, e o comentário seguinte
// da mesma mídia passava.
func (i *Instagram) sendPrivateReplyOnce(ctx context.Context, commentID, text string) (err error, transient bool) {
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
	body, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return fmt.Errorf("marshaling private reply payload: %w", marshalErr), false
	}

	req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if reqErr != nil {
		return fmt.Errorf("creating private reply request: %w", reqErr), false
	}
	req.Header.Set("Authorization", "Bearer "+i.credentials.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, doErr := i.client.Do(req)
	if doErr != nil {
		return fmt.Errorf("sending private reply request: %w", doErr), true
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		bodyStr := string(respBody)
		if len(bodyStr) > 256 {
			bodyStr = bodyStr[:256] + "..."
		}
		logger.From(ctx, i.logger).Error("instagram private reply failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", bodyStr),
			zap.String("comment_id", commentID),
		)
		return fmt.Errorf("instagram private reply failed: status %d, body: %s", resp.StatusCode, bodyStr),
			resp.StatusCode >= 500
	}

	logger.From(ctx, i.logger).Info("instagram private reply sent",
		zap.String("comment_id", commentID),
		zap.Int("text_bytes", len(text)),
	)
	return nil, false
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

	logger.From(ctx, i.logger).Info("fetched active instagram lives",
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
		logger.From(ctx, i.logger).Error("instagram hide comment failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", bodyStr),
			zap.String("comment_id", commentID),
		)
		return fmt.Errorf("instagram hide comment failed: status %d, body: %s", resp.StatusCode, bodyStr)
	}

	logger.From(ctx, i.logger).Info("instagram comment hidden",
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
		logger.From(ctx, i.logger).Error("instagram delete comment failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", bodyStr),
			zap.String("comment_id", commentID),
		)
		return fmt.Errorf("instagram delete comment failed: status %d, body: %s", resp.StatusCode, bodyStr)
	}

	logger.From(ctx, i.logger).Info("instagram comment deleted",
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

	logger.From(ctx, i.logger).Info("fetched instagram user media",
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

	// Step 1 — create the image container (JSON body). Long presigned image URLs
	// don't fit reliably in the query string, so the params go in the body.
	containerID, err := i.postGraph(ctx, "/me/media", map[string]any{
		"image_url": imageURL,
		"caption":   caption,
	})
	if err != nil {
		return "", fmt.Errorf("creating media container: %w", err)
	}

	// Step 2 — wait for the container to finish. Despite images usually being
	// ready in ~1s, graph.instagram.com can still return code 9007/2207027
	// ("media is not ready for publishing") if we publish too soon, so we poll
	// the container status first (returns immediately once FINISHED).
	if err := i.waitContainerFinished(ctx, containerID); err != nil {
		return "", err
	}

	// Step 3 — publish.
	mediaID, err := i.postGraph(ctx, "/me/media_publish", map[string]any{
		"creation_id": containerID,
	})
	if err != nil {
		return "", fmt.Errorf("publishing media: %w", err)
	}

	logger.From(ctx, i.logger).Info("instagram image post published",
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

// PublishReel publishes a Reel from a public video URL. The Instagram-Login API
// (graph.instagram.com) requires video_url (it does not support the resumable
// upload protocol, which is Facebook-Login only). Flow: create REELS container
// -> poll status until FINISHED (video processing) -> media_publish.
func (i *Instagram) PublishReel(ctx context.Context, videoURL, caption string) (string, error) {
	if videoURL == "" {
		return "", fmt.Errorf("video url is required")
	}

	containerID, err := i.postGraph(ctx, "/me/media", map[string]any{
		"media_type": "REELS",
		"video_url":  videoURL,
		"caption":    caption,
	})
	if err != nil {
		return "", fmt.Errorf("creating reel container: %w", err)
	}

	if err := i.waitContainerFinished(ctx, containerID); err != nil {
		return "", err
	}

	mediaID, err := i.postGraph(ctx, "/me/media_publish", map[string]any{"creation_id": containerID})
	if err != nil {
		return "", fmt.Errorf("publishing reel: %w", err)
	}

	logger.From(ctx, i.logger).Info("instagram reel published",
		zap.String("container_id", containerID),
		zap.String("media_id", mediaID),
	)
	return mediaID, nil
}

// PublishStory publishes a Story (media_type=STORIES) from a public media URL.
// Stories accept a photo (image_url) or a video (video_url) and expire after 24h.
// Flow mirrors posts/Reels: create container -> wait until FINISHED -> publish.
// Stories have no caption and no public comments — buyers engage via DM replies.
func (i *Instagram) PublishStory(ctx context.Context, mediaURL string, isVideo bool) (string, error) {
	if mediaURL == "" {
		return "", fmt.Errorf("media url is required")
	}

	params := map[string]any{"media_type": "STORIES"}
	if isVideo {
		params["video_url"] = mediaURL
	} else {
		params["image_url"] = mediaURL
	}

	containerID, err := i.postGraph(ctx, "/me/media", params)
	if err != nil {
		return "", fmt.Errorf("creating story container: %w", err)
	}

	if err := i.waitContainerFinished(ctx, containerID); err != nil {
		return "", err
	}

	mediaID, err := i.postGraph(ctx, "/me/media_publish", map[string]any{"creation_id": containerID})
	if err != nil {
		return "", fmt.Errorf("publishing story: %w", err)
	}

	logger.From(ctx, i.logger).Info("instagram story published",
		zap.String("container_id", containerID),
		zap.String("media_id", mediaID),
		zap.Bool("is_video", isVideo),
	)
	return mediaID, nil
}

// instagramWebhookFields are the webhook fields LiveCart depends on:
//   - comments: keyword sales on posts/lives (has a polling fallback)
//   - messages: story replies and DM sales (NO fallback — dead without this)
//   - live_comments: comentário DURANTE a transmissão ao vivo. É um campo
//     PRÓPRIO na API, não faz parte de `comments` — e faltava aqui, embora a
//     venda por live seja o produto. Chegavam mesmo assim porque o app está
//     inscrito no campo no painel da Meta e a inscrição por conta não permite
//     customização por conta ("if an app user is subscribed to any Instagram
//     webhook field, the app receives notifications for all subscribed
//     fields"). Ou seja: funcionava por acidente da configuração do app, não
//     por escolha nossa — e qualquer app novo nasceria sem live.
const instagramWebhookFields = "comments,live_comments,messages"

// SubscribeWebhooks subscribes the connected account to LiveCart's webhook
// fields (POST /me/subscribed_apps). WITHOUT this call Meta never delivers
// webhooks for the account, no matter how the app-level webhook is configured
// or which permissions were granted.
//
// Field bug (18/07/2026): this step didn't exist, so every merchant who
// connected Instagram got zero webhook deliveries. Comment sales still worked
// because of the polling fallback, which masked the problem — but story-reply
// (DM) sales silently produced no orders. It "worked" only on accounts that had
// been subscribed by hand during development.
func (i *Instagram) SubscribeWebhooks(ctx context.Context) error {
	url := fmt.Sprintf("%s/%s/me/subscribed_apps?subscribed_fields=%s",
		instagramGraphAPIBaseURL, instagramGraphAPIVersion, instagramWebhookFields)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+i.credentials.AccessToken)

	resp, err := i.client.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("instagram API error: status %d, body: %s", resp.StatusCode, truncate(string(respBody), 300))
	}

	// Meta answers {"success": true}; anything else means the subscription did
	// not take effect and webhooks would silently not arrive.
	var out struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}
	if !out.Success {
		return fmt.Errorf("instagram did not confirm the subscription: %s", truncate(string(respBody), 300))
	}

	logger.From(ctx, i.logger).Info("instagram webhook subscription active",
		zap.String("fields", instagramWebhookFields),
	)
	return nil
}

// ListSubscribedFields reads the account's current webhook subscription
// (GET /me/subscribed_apps) so the dashboard can tell the merchant whether
// story/DM sales will actually work.
func (i *Instagram) ListSubscribedFields(ctx context.Context) ([]string, error) {
	url := fmt.Sprintf("%s/%s/me/subscribed_apps", instagramGraphAPIBaseURL, instagramGraphAPIVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+i.credentials.AccessToken)

	resp, err := i.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("instagram API error: status %d, body: %s", resp.StatusCode, truncate(string(respBody), 300))
	}

	var parsed struct {
		Data []struct {
			SubscribedFields []string `json:"subscribed_fields"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	fields := []string{}
	for _, d := range parsed.Data {
		fields = append(fields, d.SubscribedFields...)
	}
	return fields, nil
}

// GetUsername resolves the @handle of a user from their Instagram-scoped id
// (IGSID), used to label story-reply DMs in the merchant UI and @mention the
// buyer. Best-effort: returns "" if the lookup is not permitted.
func (i *Instagram) GetUsername(ctx context.Context, igsid string) (string, error) {
	if igsid == "" {
		return "", fmt.Errorf("igsid is required")
	}
	url := fmt.Sprintf("%s/%s/%s?fields=username&access_token=%s",
		instagramGraphAPIBaseURL,
		instagramGraphAPIVersion,
		igsid,
		i.credentials.AccessToken,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	resp, err := i.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("instagram API error: status %d, body: %s", resp.StatusCode, truncate(string(respBody), 200))
	}
	var out struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}
	return out.Username, nil
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
			return fmt.Errorf("instagram media processing failed: %s", status)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("instagram media still processing after timeout")
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
