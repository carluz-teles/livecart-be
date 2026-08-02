package integration

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/lib/logger"
)

// TokenRefreshWorker proactively refreshes OAuth tokens before they expire.
// This ensures tokens are always valid when API calls are made.
type TokenRefreshWorker struct {
	service  *Service
	logger   *zap.Logger
	interval time.Duration
	window   time.Duration // Refresh tokens expiring within this window
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// TokenRefreshWorkerConfig contains configuration for the token refresh worker.
type TokenRefreshWorkerConfig struct {
	Service  *Service
	Logger   *zap.Logger
	Interval time.Duration // How often to check for expiring tokens (default: 5 minutes)
	Window   time.Duration // Refresh tokens expiring within this window (default: 30 minutes)
}

// NewTokenRefreshWorker creates a new token refresh worker.
func NewTokenRefreshWorker(cfg TokenRefreshWorkerConfig) *TokenRefreshWorker {
	interval := cfg.Interval
	if interval == 0 {
		interval = 5 * time.Minute
	}

	window := cfg.Window
	if window == 0 {
		window = 30 * time.Minute
	}

	return &TokenRefreshWorker{
		service:  cfg.Service,
		logger:   cfg.Logger,
		interval: interval,
		window:   window,
		stopCh:   make(chan struct{}),
	}
}

// Start begins the background token refresh process.
func (w *TokenRefreshWorker) Start() {
	w.wg.Add(1)
	go w.run()
	w.logger.Info("token refresh worker started",
		zap.Duration("interval", w.interval),
		zap.Duration("window", w.window),
	)
}

// Stop gracefully stops the worker.
func (w *TokenRefreshWorker) Stop() {
	close(w.stopCh)
	w.wg.Wait()
	w.logger.Info("token refresh worker stopped")
}

func (w *TokenRefreshWorker) run() {
	defer w.wg.Done()

	// Run immediately on start
	w.refreshExpiringTokens()

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.refreshExpiringTokens()
		case <-w.stopCh:
			return
		}
	}
}

func (w *TokenRefreshWorker) refreshExpiringTokens() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Find integrations with tokens expiring within the window
	expiresBefore := time.Now().Add(w.window)
	integrations, err := w.service.repo.ListWithExpiringTokens(ctx, expiresBefore)
	if err != nil {
		logger.From(ctx, w.logger).Error("failed to list integrations with expiring tokens", zap.Error(err))
		return
	}

	if len(integrations) == 0 {
		logger.From(ctx, w.logger).Debug("no tokens expiring soon")
		return
	}

	logger.From(ctx, w.logger).Info("found integrations with expiring tokens",
		zap.Int("count", len(integrations)),
		zap.Time("expires_before", expiresBefore),
	)

	// Refresh each token
	var refreshed, failed int
	for _, integration := range integrations {
		itemCtx := logger.WithStore(ctx, integration.StoreID, "")
		if err := w.refreshToken(itemCtx, &integration); err != nil {
			logger.From(itemCtx, w.logger).Warn("failed to refresh token",
				zap.String("integration_id", integration.ID),
				zap.String("provider", integration.Provider),
				zap.Error(err),
			)
			failed++
		} else {
			logger.From(itemCtx, w.logger).Info("token refreshed successfully",
				zap.String("integration_id", integration.ID),
				zap.String("provider", integration.Provider),
			)
			refreshed++
		}
	}

	logger.From(ctx, w.logger).Info("token refresh cycle completed",
		zap.Int("refreshed", refreshed),
		zap.Int("failed", failed),
	)
}

func (w *TokenRefreshWorker) refreshToken(ctx context.Context, integration *IntegrationRow) error {
	// Decrypt credentials
	creds, err := w.service.decryptCredentials(integration.Credentials)
	if err != nil {
		return err
	}

	// Sem credencial de renovação, não há o que fazer aqui.
	//
	// O guard olhava SÓ o refresh_token, e isso escondia um buraco: o Instagram
	// não emite refresh_token — a credencial de renovação é o próprio token de
	// longa duração. Ou seja, nenhuma integração de Instagram passava por este
	// worker, e a renovação só acontecia dentro de createProviderFromRow,
	// quando o token já estava a 5 minutos de vencer, no meio de um request.
	// Com a publicação agendada (RN-31), entre agendar e disparar podem passar
	// semanas — a renovação proativa deixou de ser conforto.
	if creds.RefreshToken == "" && !providerSelfRefreshes(integration.Provider) {
		logger.From(ctx, w.logger).Debug("skipping integration without refresh credential",
			zap.String("integration_id", integration.ID),
			zap.String("provider", integration.Provider),
		)
		return nil
	}

	// Attempt refresh
	_, err = w.service.refreshToken(ctx, integration, creds)
	return err
}

// providerSelfRefreshes diz se a renovação do provider usa o PRÓPRIO access
// token, sem refresh_token. Hoje só o Instagram
// (GET /refresh_access_token?grant_type=ig_refresh_token).
//
// Lista explícita, e não "tenta em todo mundo": os providers que exigem
// refresh_token devolvem ERRO quando ele falta, e o chamador marca a integração
// como 'error'. Relaxar o guard para todos transformaria "este provider não
// renova" em "esta integração está quebrada".
func providerSelfRefreshes(provider string) bool {
	return provider == "instagram"
}
