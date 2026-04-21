package integration

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
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
		w.logger.Error("failed to list integrations with expiring tokens", zap.Error(err))
		return
	}

	if len(integrations) == 0 {
		w.logger.Debug("no tokens expiring soon")
		return
	}

	w.logger.Info("found integrations with expiring tokens",
		zap.Int("count", len(integrations)),
		zap.Time("expires_before", expiresBefore),
	)

	// Refresh each token
	var refreshed, failed int
	for _, integration := range integrations {
		if err := w.refreshToken(ctx, &integration); err != nil {
			w.logger.Warn("failed to refresh token",
				zap.String("integration_id", integration.ID),
				zap.String("provider", integration.Provider),
				zap.Error(err),
			)
			failed++
		} else {
			w.logger.Info("token refreshed successfully",
				zap.String("integration_id", integration.ID),
				zap.String("provider", integration.Provider),
			)
			refreshed++
		}
	}

	w.logger.Info("token refresh cycle completed",
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

	// Skip if no refresh token
	if creds.RefreshToken == "" {
		w.logger.Debug("skipping integration without refresh token",
			zap.String("integration_id", integration.ID),
		)
		return nil
	}

	// Attempt refresh
	_, err = w.service.refreshToken(ctx, integration, creds)
	return err
}
