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

// tetoDeTokensPorMinuto é quantas chamadas ao endpoint de TOKEN cada provedor
// tolera por minuto, do nosso IP.
//
// Só entra provider cujo teto foi LIDO na documentação — um palpite aqui vira
// bloqueio de IP em produção. Ausente = sem teto conhecido, e o worker não
// espaça por causa dele.
var tetoDeTokensPorMinuto = map[string]int{
	// Doc do Bling: "20 requisições a /oauth/token em 60 segundos → bloqueio de
	// 60 minutos". Usamos 10, metade do teto, porque o MESMO endpoint atende o
	// authorization_code: gastar a cota toda em renovação impediria um lojista
	// de conectar.
	"bling": 10,
}

// espacamentoEntreRenovacoes devolve quanto esperar entre uma renovação e a
// seguinte para não estourar o teto de nenhum provedor da leva.
func espacamentoEntreRenovacoes(integracoes []IntegrationRow) time.Duration {
	pior := 0
	for _, i := range integracoes {
		teto, tem := tetoDeTokensPorMinuto[i.Provider]
		if !tem {
			continue
		}
		if pior == 0 || teto < pior {
			pior = teto
		}
	}
	if pior <= 0 {
		return 0
	}
	return time.Minute / time.Duration(pior)
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

	// Refresh each token, ESPAÇADO.
	//
	// O laço nu — que era o que existia aqui — dispara as 100 renovações o mais
	// rápido que a rede permite. Isso passou despercebido enquanto só havia
	// Tiny, Mercado Pago e Instagram, que não punem rajada no endpoint de token.
	//
	// O Bling pune: 20 chamadas a /oauth/token em 60 SEGUNDOS bloqueiam o IP por
	// 60 MINUTOS. O IP é o NAT compartilhado do Railway, então o castigo é
	// coletivo — durante ele nenhuma loja renova e nenhuma loja nova conecta.
	// Com 21 lojas Bling vencendo na mesma janela, uma passada do worker
	// derrubaria a frota por uma hora.
	//
	// O intervalo é calculado do teto mais apertado entre os providers desta
	// leva, e não de um número fixo: assim acrescentar um provider novo com
	// limite pior é uma linha em tetoDeTokensPorMinuto, não uma descoberta em
	// produção.
	espacamento := espacamentoEntreRenovacoes(integrations)
	if espacamento > 0 {
		logger.From(ctx, w.logger).Info("espaçando as renovações para não estourar o teto do provedor",
			zap.Duration("intervalo", espacamento),
			zap.Int("integracoes", len(integrations)),
		)
	}

	var refreshed, failed int
	for i, integration := range integrations {
		if i > 0 && espacamento > 0 {
			select {
			case <-ctx.Done():
				return
			case <-w.stopCh:
				return
			case <-time.After(espacamento):
			}
		}
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
