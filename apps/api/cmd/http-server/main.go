package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/fiber/v2/middleware/requestid"
	"github.com/gofiber/swagger"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	_ "livecart/apps/api/docs"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/clerk"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/crypto"
	"livecart/apps/api/lib/database"
	"livecart/apps/api/lib/email"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/idempotency"
	"livecart/apps/api/lib/logger"
	"livecart/apps/api/lib/ratelimit"

	"livecart/apps/api/internal/billing"
	"livecart/apps/api/internal/cart"
	"livecart/apps/api/internal/checkout"
	"livecart/apps/api/internal/coupon"
	couponlisteners "livecart/apps/api/internal/coupon/listeners"
	"livecart/apps/api/internal/customer"
	"livecart/apps/api/internal/dashboard"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/idea"
	"livecart/apps/api/internal/integration"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/integration/providers/communication"
	"livecart/apps/api/internal/integration/providers/erp"
	"livecart/apps/api/internal/integration/providers/payment"
	"livecart/apps/api/internal/integration/providers/shipping"
	"livecart/apps/api/internal/integration/providers/social"
	"livecart/apps/api/internal/inventory"
	inventorylisteners "livecart/apps/api/internal/inventory/listeners"
	"livecart/apps/api/internal/invitation"
	"livecart/apps/api/internal/live"
	"livecart/apps/api/internal/member"
	"livecart/apps/api/internal/notification"
	notiflisteners "livecart/apps/api/internal/notification/listeners"
	notificationinbox "livecart/apps/api/internal/notification_inbox"
	"livecart/apps/api/internal/order"
	orderlisteners "livecart/apps/api/internal/order/listeners"
	paymentdomain "livecart/apps/api/internal/payment"
	"livecart/apps/api/internal/postcheckout"
	"livecart/apps/api/internal/product"
	"livecart/apps/api/internal/productgroup"
	"livecart/apps/api/internal/store"
	"livecart/apps/api/internal/telemetry"
	"livecart/apps/api/internal/user"
	"livecart/apps/api/lib/storage"
)

// @title           LiveCart API
// @version         1.0
// @description     API REST for LiveCart SaaS — live commerce order detection, cart consolidation, and integrations.
// @termsOfService  https://livecart.com/terms

// @contact.name   LiveCart Support
// @contact.email  support@livecart.com

// @license.name  Proprietary

// @host      localhost:3001
// @BasePath  /

// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
// @description Enter your bearer token in the format: Bearer <token>
func main() {
	// Load environment variables from .env file
	if err := config.Load(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log, err := logger.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync()

	// Set logger for httpx error handling
	httpx.SetLogger(log)

	// OpenTelemetry tracing. No-op when OTEL_EXPORTER_OTLP_ENDPOINT is unset
	// (local dev without a collector, tests); exports to Jaeger locally and to
	// the Datadog agent in staging/prod.
	otelShutdown, err := telemetry.Init(ctx, telemetry.Config{
		Endpoint:    config.OTELExporterEndpoint.String(),
		ServiceName: config.OTELServiceName.StringOr("livecart-api"),
		Environment: config.Environment(),
	})
	if err != nil {
		log.Sugar().Fatalf("initializing telemetry: %v", err)
	}
	defer func() {
		if err := otelShutdown(context.Background()); err != nil {
			log.Sugar().Errorf("telemetry shutdown error: %v", err)
		}
	}()

	databaseURL := config.DatabaseURL.Required()
	clerkFrontendAPI := config.ClerkFrontendAPI.Required()
	port := config.Port.StringOr("3001")

	// Run migrations automatically in non-production environments. Production
	// opts in by setting RUN_MIGRATIONS_ON_STARTUP=true on the service — if a
	// bad migration ships, the boot fails fast with a clear log and the
	// platform keeps the previous container serving traffic (zero downtime).
	shouldMigrate := !config.IsProduction() || os.Getenv("RUN_MIGRATIONS_ON_STARTUP") == "true"
	if shouldMigrate {
		if err := database.RunMigrations(databaseURL, "apps/api/db/migrations"); err != nil {
			log.Sugar().Fatalf("running migrations: %v", err)
		}
		log.Info("migrations applied")
	}

	pool, err := database.NewPool(ctx, databaseURL)
	if err != nil {
		log.Sugar().Fatalf("connecting to database: %v", err)
	}
	defer pool.Close()

	queries := sqlc.New(pool)
	validate := validator.New()
	registerCustomValidators(validate)
	clerkClient := clerk.NewClient(clerkFrontendAPI)

	// Email client for sending invitation emails (reads from env vars)
	emailClient := email.NewClient(log)
	// Trilha unificada de auditoria: todo envio de e-mail (sent/failed/skipped)
	// vira uma linha em notification_logs (channel='email'), lado a lado com os
	// DMs/WhatsApp do módulo de comunicações.
	emailClient.SetAuditHook(emailAuditHook(log, queries))

	// The async event pipeline (asynq client/server + outbox relay) is built and
	// started inside newApp, where the domain services its consumers dispatch to
	// are constructed. Its stop hooks are registered on the returned lifecycle.
	app, lifecycle := newApp(log, pool, queries, validate, clerkClient, emailClient)

	go func() {
		if err := app.Listen(":" + port); err != nil {
			log.Sugar().Fatalf("server error: %v", err)
		}
	}()

	log.Sugar().Infof("server listening on :%s (env: %s)", port, config.Environment())

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	// Shutdown: stop the background workers, event server and relay first (in
	// reverse registration order) so nothing new is picked up, then drain HTTP
	// with a bounded timeout — app.Shutdown() waits on idle keep-alive
	// connections indefinitely and would otherwise hang the process until the
	// orchestrator SIGKILLs it. Finally close the publisher.
	log.Info("shutting down server")
	cancel()
	lifecycle.shutdown()
	if err := app.ShutdownWithTimeout(10 * time.Second); err != nil {
		log.Sugar().Errorf("server shutdown error: %v", err)
	}
}

// appLifecycle collects components that must be stopped on shutdown and stops
// them in reverse registration order. It also closes a pre-existing gap: the
// background workers created in newApp had no reference held by main(), so
// SIGTERM never stopped them cleanly.
type appLifecycle struct {
	log      *zap.Logger
	stoppers []namedStopper
}

type namedStopper struct {
	name string
	stop func()
}

func (l *appLifecycle) add(name string, stop func()) {
	l.stoppers = append(l.stoppers, namedStopper{name: name, stop: stop})
}

func (l *appLifecycle) shutdown() {
	for i := len(l.stoppers) - 1; i >= 0; i-- {
		s := l.stoppers[i]
		l.log.Info("stopping component", zap.String("component", s.name))
		s.stop()
	}
}

// slugRegex matches valid URL slugs: lowercase letters, numbers, and hyphens
// Cannot start or end with a hyphen
var slugRegex = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func registerCustomValidators(validate *validator.Validate) {
	validate.RegisterValidation("slug", func(fl validator.FieldLevel) bool {
		return slugRegex.MatchString(fl.Field().String())
	})
}

// emailAuditHook grava cada tentativa de envio do lib/email em
// notification_logs (channel='email'), unificando a trilha de auditoria com
// os DMs/WhatsApp do módulo de comunicações. Best-effort: qualquer falha no
// insert vira log estruturado — nunca erro no caminho de envio.
func emailAuditHook(log *zap.Logger, queries *sqlc.Queries) email.AuditHook {
	auditLog := log.Named("email_audit")
	return func(ctx context.Context, e email.AuditEntry) {
		// store_id é NOT NULL na tabela — sem ele não há linha válida. Callers
		// sem contexto de loja ficam só no log estruturado do client.
		storeID := pgUUIDFromString(e.StoreID)
		if !storeID.Valid {
			auditLog.Debug("email audit entry without store_id, skipping db log",
				zap.String("kind", e.Kind),
				zap.String("to", e.ToEmail),
				zap.String("status", e.Status),
			)
			return
		}

		// O envio costuma acontecer dentro de handlers de webhook que cancelam
		// o contexto logo depois do ACK — desacopla o insert desse cancelamento.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		params := sqlc.CreateEmailNotificationLogParams{
			StoreID:           storeID,
			EventID:           pgUUIDFromString(e.EventID),
			CartID:            pgUUIDFromString(e.CartID),
			PlatformUserID:    e.ToEmail,
			NotificationType:  e.Kind,
			Status:            e.Status,
			MessageText:       pgTextFromString(e.Subject),
			ErrorMessage:      pgTextFromString(e.ErrorMessage),
			ProviderMessageID: pgTextFromString(e.ProviderMessageID),
		}
		if e.Status == email.AuditStatusSent {
			params.SentAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
		}

		if err := queries.CreateEmailNotificationLog(ctx, params); err != nil {
			auditLog.Warn("failed to record email in notification_logs",
				zap.String("kind", e.Kind),
				zap.String("to", e.ToEmail),
				zap.String("status", e.Status),
				zap.Error(err),
			)
		}

		// Group I: notification.sent (channel=email) — the single canonical point
		// for every outbound email (order receipts, invitations, ...). Merged into
		// the unified notification.* vocabulary (was email.sent); the channel lives
		// in the payload so analytics has one fact across channels. Best-effort.
		if e.Status == email.AuditStatusSent {
			dedup := e.ProviderMessageID
			if dedup == "" {
				dedup = e.StoreID + ":" + e.Kind + ":" + e.ToEmail
			}
			_ = events.EmitInternal(ctx, queries, events.NotificationSent, "notification.sent:email:"+dedup, struct {
				StoreID          string `json:"store_id"`
				NotificationType string `json:"notification_type"`
				Channel          string `json:"channel"`
				Recipient        string `json:"recipient"`
				CartID           string `json:"cart_id,omitempty"`
			}{e.StoreID, e.Kind, "email", e.ToEmail, e.CartID})
		}
	}
}

// pgUUIDFromString converte um uuid em string (possivelmente vazio) para
// pgtype.UUID — Valid=false quando vazio ou malformado.
func pgUUIDFromString(s string) pgtype.UUID {
	var u pgtype.UUID
	if s == "" {
		return u
	}
	_ = u.Scan(s) // erro deixa o zero value (Valid=false)
	return u
}

// pgTextFromString mapeia string vazia para NULL.
func pgTextFromString(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}

func newApp(log *zap.Logger, pool *pgxpool.Pool, queries *sqlc.Queries, validate *validator.Validate, clerkClient *clerk.Client, emailClient *email.Client) (*fiber.App, *appLifecycle) {
	lifecycle := &appLifecycle{log: log}
	app := fiber.New(fiber.Config{
		// Single error handler: handlers `return err` and this renders the
		// FE-facing response (ozzo validation.Errors → 422 {error, fields};
		// ServiceError/fiber.Error/unexpected → HandleServiceError).
		ErrorHandler: httpx.ErrorHandler,
		// Reels can be up to 300MB; stream the request body to disk instead of
		// buffering it in memory.
		BodyLimit:         320 * 1024 * 1024,
		StreamRequestBody: true,
	})

	// Panics viram log estruturado com o stack da goroutine que quebrou — o
	// default do middleware imprime em stderr fora do formato do agregador.
	app.Use(recover.New(recover.Config{
		EnableStackTrace: true,
		StackTraceHandler: func(c *fiber.Ctx, e any) {
			log.Error("panic recovered",
				zap.Any("panic", e),
				zap.String("request_id", httpx.RequestID(c)),
				zap.String("method", c.Method()),
				zap.String("path", c.Path()),
				zap.String("stack", string(debug.Stack())),
			)
		},
	}))
	app.Use(requestid.New())
	app.Use(cors.New())
	// Span-per-request: opens/continues the trace and puts trace_id in Locals so
	// the access logger and error logger below emit it. Must run before the
	// request logger and after requestid.
	app.Use(telemetry.Middleware())
	app.Use(httpx.RequestLogger(log))

	// Swagger UI
	app.Get("/swagger/*", swagger.HandlerDefault)

	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	// Dev-only smoke test for the event pipeline: emits a canonical test.ping
	// through the transactional outbox, exercising the full path
	// (Emit -> outbox -> relay -> Redis -> consumer).
	if !config.IsProduction() {
		app.Post("/internal/dev/ping", func(c *fiber.Ctx) error {
			ctx := c.UserContext()
			env := events.Envelope{EventID: uuid.NewString(), Name: events.TestPing, Source: events.SourceInternal}
			tx, err := pool.Begin(ctx)
			if err != nil {
				return httpx.HandleServiceError(c, err)
			}
			defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
			if err := events.Emit(ctx, sqlc.New(tx), env); err != nil {
				return httpx.HandleServiceError(c, err)
			}
			if err := tx.Commit(ctx); err != nil {
				return httpx.HandleServiceError(c, err)
			}
			return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"event_id": env.EventID})
		})
	}

	// User repository and service (shared between webhook and API handlers)
	userRepo := user.NewRepository(queries)
	userSvc := user.NewService(userRepo, log)

	// Billing / paywall (PRD 007): trial de 7 dias, estado de assinatura no
	// sync e webhook Stripe.
	billingSvc := billing.NewService(queries, log)
	userSvc.SetBilling(billingSvc)
	billingWebhook := billing.NewWebhookHandler(billingSvc, log)

	// Live session service (needed by integration for cart operations)
	liveRepo := live.NewRepository(queries, pool)
	liveSvc := live.NewService(liveRepo, log)

	// Customer service is created early so the live service can upsert
	// customers as carts are created during a live event.
	customerRepo := customer.NewRepository(queries)
	customerSvc := customer.NewService(customerRepo, log)
	liveSvc.SetCustomerUpserter(customerSvc)

	// Integration Layer setup
	var integrationSvc *integration.Service
	var integrationWebhookHandler *integration.WebhookHandler
	var notificationSvc *notification.Service
	var postCheckoutSvc *postcheckout.Service

	if config.EncryptionKey.IsSet() {
		encryptor, err := crypto.NewEncryptor(config.EncryptionKey.String())
		if err != nil {
			log.Sugar().Warnf("integration layer disabled: %v", err)
		} else {
			// Create rate limit manager for integration providers
			rateLimitManager := ratelimit.NewManager(log)

			// Create provider factory with constructors
			providerFactory := providers.NewFactory(providers.FactoryConfig{
				Logger:                  log,
				MercadoPagoAppID:        config.MercadoPagoAppID.String(),
				MercadoPagoAppSecret:    config.MercadoPagoAppSecret.String(),
				MelhorEnvioClientID:     config.MelhorEnvioClientID.String(),
				MelhorEnvioClientSecret: config.MelhorEnvioClientSecret.String(),
				MelhorEnvioEnv:          config.MelhorEnvioEnv.StringOr("sandbox"),
				MelhorEnvioUserAgent:    config.MelhorEnvioUserAgent.StringOr("LiveCart (contato@livecart.com.br)"),
				MelhorEnvioRedirectURI:  config.MelhorEnvioRedirectURI.String(),
				SmartEnviosEnv:          config.SmartEnviosEnv.StringOr("production"),
				SmartEnviosUserAgent:    config.SmartEnviosUserAgent.StringOr("LiveCart (contato@livecart.com.br)"),
				TwilioAccountSID:        config.TwilioAccountSID.String(),
				TwilioAuthToken:         config.TwilioAuthToken.String(),
				RateLimitManager:        rateLimitManager,
				MelhorEnvioConstructor: func(cfg providers.MelhorEnvioConfig) (providers.ShippingProvider, error) {
					return shipping.New(cfg)
				},
				SmartEnviosConstructor: func(cfg providers.SmartEnviosConfig) (providers.ShippingProvider, error) {
					return shipping.NewSmartEnvios(cfg)
				},
				MercadoPagoConstructor: func(cfg providers.MercadoPagoConfig) (providers.PaymentProvider, error) {
					return payment.NewMercadoPago(payment.MercadoPagoConfig{
						IntegrationID: cfg.IntegrationID,
						StoreID:       cfg.StoreID,
						Credentials:   cfg.Credentials,
						AppID:         cfg.AppID,
						AppSecret:     cfg.AppSecret,
						Logger:        cfg.Logger,
						LogFunc:       cfg.LogFunc,
						RateLimiter:   cfg.RateLimiter,
					})
				},
				PagarmeConstructor: func(cfg providers.PagarmeConfig) (providers.PaymentProvider, error) {
					return payment.NewPagarme(payment.PagarmeConfig{
						IntegrationID: cfg.IntegrationID,
						StoreID:       cfg.StoreID,
						Credentials:   cfg.Credentials,
						Logger:        cfg.Logger,
						LogFunc:       cfg.LogFunc,
						RateLimiter:   cfg.RateLimiter,
					})
				},
				TinyConstructor: func(cfg providers.TinyConfig) (providers.ERPProvider, error) {
					return erp.NewTiny(erp.TinyConfig{
						IntegrationID: cfg.IntegrationID,
						StoreID:       cfg.StoreID,
						Credentials:   cfg.Credentials,
						ClientID:      cfg.ClientID,
						ClientSecret:  cfg.ClientSecret,
						Logger:        cfg.Logger,
						LogFunc:       cfg.LogFunc,
						RateLimiter:   cfg.RateLimiter,
					})
				},
				InstagramConstructor: func(cfg providers.InstagramConfig) (providers.SocialProvider, error) {
					return social.NewInstagram(social.InstagramConfig{
						IntegrationID: cfg.IntegrationID,
						StoreID:       cfg.StoreID,
						Credentials:   cfg.Credentials,
						Logger:        cfg.Logger,
						LogFunc:       cfg.LogFunc,
						RateLimiter:   cfg.RateLimiter,
					})
				},
				TwilioConstructor: func(cfg providers.TwilioConfig) (providers.CommunicationProvider, error) {
					return communication.NewTwilio(cfg)
				},
			})

			// Create repositories
			integrationRepo := integration.NewRepository(queries, pool)
			idempotencyRepo := integration.NewIdempotencyRepository(queries)
			idempotencySvc := idempotency.NewService(idempotencyRepo)

			// Create service
			integrationSvc = integration.NewService(
				integrationRepo,
				providerFactory,
				encryptor,
				idempotencySvc,
				liveSvc,
				log,
			)

			// Set log function for providers
			providerFactory = providers.NewFactory(providers.FactoryConfig{
				Logger:                  log,
				LogFunc:                 integrationSvc.LogIntegrationOperation,
				MercadoPagoAppID:        config.MercadoPagoAppID.String(),
				MercadoPagoAppSecret:    config.MercadoPagoAppSecret.String(),
				MelhorEnvioClientID:     config.MelhorEnvioClientID.String(),
				MelhorEnvioClientSecret: config.MelhorEnvioClientSecret.String(),
				MelhorEnvioEnv:          config.MelhorEnvioEnv.StringOr("sandbox"),
				MelhorEnvioUserAgent:    config.MelhorEnvioUserAgent.StringOr("LiveCart (contato@livecart.com.br)"),
				MelhorEnvioRedirectURI:  config.MelhorEnvioRedirectURI.String(),
				SmartEnviosEnv:          config.SmartEnviosEnv.StringOr("production"),
				SmartEnviosUserAgent:    config.SmartEnviosUserAgent.StringOr("LiveCart (contato@livecart.com.br)"),
				TwilioAccountSID:        config.TwilioAccountSID.String(),
				TwilioAuthToken:         config.TwilioAuthToken.String(),
				RateLimitManager:        rateLimitManager,
				MelhorEnvioConstructor: func(cfg providers.MelhorEnvioConfig) (providers.ShippingProvider, error) {
					return shipping.New(cfg)
				},
				SmartEnviosConstructor: func(cfg providers.SmartEnviosConfig) (providers.ShippingProvider, error) {
					return shipping.NewSmartEnvios(cfg)
				},
				MercadoPagoConstructor: func(cfg providers.MercadoPagoConfig) (providers.PaymentProvider, error) {
					return payment.NewMercadoPago(payment.MercadoPagoConfig{
						IntegrationID: cfg.IntegrationID,
						StoreID:       cfg.StoreID,
						Credentials:   cfg.Credentials,
						AppID:         cfg.AppID,
						AppSecret:     cfg.AppSecret,
						Logger:        cfg.Logger,
						LogFunc:       cfg.LogFunc,
						RateLimiter:   cfg.RateLimiter,
					})
				},
				PagarmeConstructor: func(cfg providers.PagarmeConfig) (providers.PaymentProvider, error) {
					return payment.NewPagarme(payment.PagarmeConfig{
						IntegrationID: cfg.IntegrationID,
						StoreID:       cfg.StoreID,
						Credentials:   cfg.Credentials,
						Logger:        cfg.Logger,
						LogFunc:       cfg.LogFunc,
						RateLimiter:   cfg.RateLimiter,
					})
				},
				TinyConstructor: func(cfg providers.TinyConfig) (providers.ERPProvider, error) {
					return erp.NewTiny(erp.TinyConfig{
						IntegrationID: cfg.IntegrationID,
						StoreID:       cfg.StoreID,
						Credentials:   cfg.Credentials,
						ClientID:      cfg.ClientID,
						ClientSecret:  cfg.ClientSecret,
						Logger:        cfg.Logger,
						LogFunc:       cfg.LogFunc,
						RateLimiter:   cfg.RateLimiter,
					})
				},
				InstagramConstructor: func(cfg providers.InstagramConfig) (providers.SocialProvider, error) {
					return social.NewInstagram(social.InstagramConfig{
						IntegrationID: cfg.IntegrationID,
						StoreID:       cfg.StoreID,
						Credentials:   cfg.Credentials,
						Logger:        cfg.Logger,
						LogFunc:       cfg.LogFunc,
						RateLimiter:   cfg.RateLimiter,
					})
				},
				TwilioConstructor: func(cfg providers.TwilioConfig) (providers.CommunicationProvider, error) {
					return communication.NewTwilio(cfg)
				},
			})

			// Recreate service with logging-enabled factory
			integrationSvc = integration.NewService(
				integrationRepo,
				providerFactory,
				encryptor,
				idempotencySvc,
				liveSvc,
				log,
			)

			// Payment-provider resolution lives in the extracted payment.Service
			// (strangler-fig B1a). integrationSvc is its resolver; GetPaymentProvider
			// delegates back into it.
			paymentSvc := paymentdomain.NewService(integrationSvc, idempotencySvc, log)
			integrationSvc.SetPaymentService(paymentSvc)

			// Create webhook handler
			integrationWebhookHandler = integration.NewWebhookHandler(integrationSvc, log)

			// Wire Notifier into liveSvc (lazy injection breaks the dependency
			// cycle: integration.Service depends on live.Service, and the
			// notifier impl depends on integration.Service).
			liveSvc.SetNotifier(newLiveNotifierAdapter(integration.NewInstagramNotifier(integrationSvc, log)))
			liveSvc.SetERPFinalizer(newERPFinalizerAdapter(integrationSvc))

			// Customer block flow needs the integration service to sweep
			// open carts (release local + ERP stock, promote waitlist).
			customerSvc.SetCartCanceler(integrationSvc)

			// Create notification service and wire into integration service
			// (integrationSvc implements notification.DMSender via SendInstagramDM)
			notificationSvc = notification.NewService(queries, integrationSvc, log)
			notificationSvc.SetEmailSender(emailClient)
			// PRD 006: reminder fallback IG -> WhatsApp
			notificationSvc.SetWhatsAppSender(integrationSvc)
			// PRD 007: paywall gate — lojas bloqueadas param de criar carrinhos
			integrationSvc.SetBillingGate(billingSvc)
			integrationSvc.SetNotificationService(notificationSvc)

			// Customer-facing post-payment flow (tracking token + receipt
			// email). Plugged into both the webhook path on integrationSvc
			// and the synchronous card path on checkoutSvc further down.
			postCheckoutSvc = postcheckout.NewService(
				postcheckout.NewRepository(queries),
				emailClient,
				log,
			)
			postCheckoutSvc.SetNotificationService(notificationSvc)
			integrationSvc.SetPostCheckoutHook(postCheckoutSvc)

			// Start background token refresh worker
			tokenWorker := integration.NewTokenRefreshWorker(integration.TokenRefreshWorkerConfig{
				Service:  integrationSvc,
				Logger:   log,
				Interval: 5 * time.Minute,  // Check every 5 minutes
				Window:   30 * time.Minute, // Refresh tokens expiring within 30 minutes
			})
			tokenWorker.Start()
			lifecycle.add("token-refresh", tokenWorker.Stop)

			// Background tracking poller. Pulls SmartEnvios tracking every
			// 6h and fires OnDelivered when the carrier reports delivered.
			// Melhor Envio is not polled — its integration is quote-only.
			trackingPoller := integration.NewTrackingPoller(integration.TrackingPollerConfig{
				Service:  integrationSvc,
				Logger:   log,
				Interval: 6 * time.Hour,
			})
			trackingPoller.Start()
			lifecycle.add("tracking-poller", trackingPoller.Stop)

			// Capture comments on active post-commerce events via polling until
			// the real-time `comments` webhook takes over for each post.
			integrationSvc.StartPostCommentPolling(context.Background())

			// Sweep do pedido-como-reserva (design C): reconcilia conversões e
			// mutações presas em voo (processo morto no meio do ciclo) — adota
			// pedidos órfãos via marcador e termina o trabalho. O health-check
			// de entrega de webhook roda no mesmo loop, a cada hora: o Tiny
			// REMOVE a URL cadastrada após falhas consecutivas e para de
			// entregar em silêncio — sem este alarme, a loja fica sem sync até
			// alguém notar.
			go func() {
				sweepTicker := time.NewTicker(5 * time.Minute)
				webhookTicker := time.NewTicker(1 * time.Hour)
				defer sweepTicker.Stop()
				defer webhookTicker.Stop()
				// Boot run: finaliza post/story cujo ends_at já fechou enquanto o
				// serviço estava fora do ar (catch-up no deploy), em vez de
				// esperar o primeiro tick. Mesmo padrão dos demais workers.
				liveSvc.SweepEndedTimedEvents(context.Background())
				for {
					select {
					case <-sweepTicker.C:
						integrationSvc.RunERPOrderOpsSweep(context.Background())
						// D5: finaliza post/story cujo ends_at fechou (arma
						// expires_at nos carts → o expiry worker os alcança).
						liveSvc.SweepEndedTimedEvents(context.Background())
					case <-webhookTicker.C:
						integrationSvc.CheckTinyStockWebhookDelivery(context.Background(), 12*time.Hour)
					}
				}
			}()

			log.Info("integration layer initialized")
		}
	}

	// storeRepo é criado antes dos webhooks: além do domínio store, ele resolve
	// store_id→slug para enriquecer os logs das rotas de webhook.
	storeRepo := store.NewRepository(queries)

	// Webhook routes (unauthenticated, use webhook signature)
	webhookHandler := user.NewWebhookHandler(userSvc)
	webhookHandler.RegisterRoutes(app)

	// Stripe billing webhook (PRD 007) — independent of the integration layer.
	billingWebhook.RegisterRoutes(app)

	// Integration webhook routes (if enabled)
	if integrationWebhookHandler != nil {
		integrationWebhookHandler.RegisterRoutes(app, storeRepo)
	}

	// Initialize S3 client for logo uploads (optional)
	// Support both standard (S3_BUCKET) and Railway (AWS_S3_BUCKET_NAME) naming
	var s3Client *storage.S3Client
	if config.S3Bucket.IsSet() || config.AWSS3BucketName.IsSet() {
		var err error
		s3Client, err = storage.NewS3Client()
		if err != nil {
			log.Sugar().Warnf("S3 storage disabled: %v", err)
		} else {
			log.Info("S3 storage initialized")
		}
	}

	// Wire storage into the integration service so it can delete transient
	// Instagram post images after they're published.
	if integrationSvc != nil && s3Client != nil {
		integrationSvc.SetStorage(s3Client)
	}

	// Public checkout routes (no authentication required). Hoisted outside
	// the integrationSvc guard because downstream wiring (coupon lifecycle)
	// reaches in to set hooks on the service.
	var checkoutSvc *checkout.Service
	if integrationSvc != nil {
		checkoutRepo := checkout.NewRepository(queries)
		checkoutSvc = checkout.NewService(checkoutRepo, pool, integrationSvc, log)
		if postCheckoutSvc != nil {
			checkoutSvc.SetPostCheckoutHook(postCheckoutSvc)
		}
		checkoutHandler := checkout.NewHandler(checkoutSvc, s3Client)
		checkoutHandler.RegisterRoutes(app)
	}

	// Public order-tracking endpoint. Lives outside the integrationSvc guard
	// because the read path doesn't depend on payment providers — it only
	// reads carts that already have a tracking_token populated by the
	// post-checkout flow.
	postCheckoutHandler := postcheckout.NewHandler(postcheckout.NewRepository(queries), postCheckoutSvc, pool, log)
	postCheckoutHandler.RegisterPublicRoutes(app)

	// Protected routes (user-scoped)
	api := app.Group("/api/v1")
	api.Use(httpx.AuthMiddleware(clerkClient))
	api.Use(httpx.SubscriptionMiddleware())

	// User routes (not store-scoped)
	userHandler := user.NewHandler(userSvc, s3Client)
	userHandler.RegisterRoutes(api)

	// Ideas channel + in-app notifications inbox: authenticated but global
	// (not bound to a store). UserOnlyMiddleware resolves the internal user
	// UUID so handlers can use httpx.GetInternalUserID(c).
	userScoped := api.Group("", httpx.UserOnlyMiddleware(userSvc))

	ideaRepo := idea.NewRepository(pool)
	notifInboxRepo := notificationinbox.NewRepository(pool)
	notifInboxSvc := notificationinbox.NewService(notifInboxRepo, log)
	notifInboxWriter := notificationinbox.NewWriter(notifInboxRepo)

	ideaSvc := idea.NewService(ideaRepo, notifInboxWriter, log)
	ideaHandler := idea.NewHandler(ideaSvc)
	ideaHandler.RegisterRoutes(userScoped)

	notifInboxHandler := notificationinbox.NewHandler(notifInboxSvc)
	notifInboxHandler.RegisterRoutes(userScoped)

	// Store routes (user's own store management)
	membershipCreator := user.NewMembershipCreatorAdapter(userSvc)
	userLookup := user.NewUserLookupAdapter(userSvc)
	storeSvc := store.NewService(storeRepo, membershipCreator, userLookup, log)
	storeSvc.SetBilling(billingSvc)

	storeHandler := store.NewHandler(storeSvc, s3Client)
	storeHandler.RegisterRoutes(api)

	// Store-scoped routes (require store access validation)
	storeScoped := api.Group("/stores/:storeId")
	storeScoped.Use(httpx.StoreAccessMiddleware(userRepo))
	// Paywall (PRD 007): 402 para lojas com assinatura bloqueada; endpoints
	// de billing ficam na allowlist para o lojista conseguir regularizar.
	storeScoped.Use(billingSvc.AccessGuard())
	billingHandler := billing.NewHandler(billingSvc)
	billingHandler.RegisterRoutes(storeScoped)

	// Store cart settings (store-scoped)
	storeHandler.RegisterStoreScopedRoutes(storeScoped)

	productRepo := product.NewRepository(queries, pool)
	productSvc := product.NewService(productRepo, log)
	productHandler := product.NewHandler(productSvc)
	productHandler.RegisterRoutes(storeScoped)

	productGroupRepo := productgroup.NewRepository(queries, pool)
	productGroupSvc := productgroup.NewService(productGroupRepo, log)
	productGroupHandler := productgroup.NewHandler(productGroupSvc)
	productGroupHandler.RegisterRoutes(storeScoped)

	// Wire product syncer for ERP webhooks
	if integrationSvc != nil {
		integrationSvc.SetProductSyncer(product.NewProductSyncerAdapter(productSvc))
		integrationSvc.SetProductGroupSyncer(productgroup.NewSyncerAdapter(productGroupSvc, productSvc))
	}

	liveHandler := live.NewHandler(liveSvc, validate)
	liveHandler.RegisterRoutes(storeScoped)

	orderRepo := order.NewRepository(pool)
	orderSvc := order.NewService(orderRepo, log)
	if integrationSvc != nil {
		// Wire the ERP-finalisation retry path: the admin "tentar novamente"
		// button on a failed paid cart routes through orderSvc but the actual
		// re-creation lives on the integration service.
		orderSvc.SetERPFinalisationRetrier(integrationSvc)
		// Same pattern for the manual "Verificar NFe" button: orderSvc is the
		// HTTP entry point but the ERP fetch lives on the integration service.
		orderSvc.SetCartInvoiceSyncer(integrationSvc)
	}
	// Block-status lookup for the order detail page; customerSvc is the
	// authoritative source for blocked_handles.
	orderSvc.SetBlockedHandleChecker(customerSvc)
	orderHandler := order.NewHandler(orderSvc)

	// Order materialisation reactor: listens to cart.paid and materialises the
	// immutable Order snapshot. Independent of billing/ERP — keyed by cart_id.
	orderListener := orderlisteners.New(pool, queries, log)
	// Notification reactor: sends the buyer receipt / refund email in reaction to
	// cart.paid / cart.refunded — postCheckoutSvc satisfies its ReceiptSender.
	// Nil when the post-checkout flow isn't wired (same guard as its handlers).
	var notificationListener *notiflisteners.Listener
	if postCheckoutSvc != nil {
		notificationListener = notiflisteners.New(postCheckoutSvc, log)
	}
	// Inventory reactor (Fatia A2): reconciles the waitlist in reaction to
	// cart.paid ('notified' → 'fulfilled' for the cart's rows), replacing the
	// inline call that used to live in postcheckout.OnCartPaid. Its repo owns the
	// canonical MarkWaitlistFulfilledByCart query + the waitlist.fulfilled emission.
	inventoryListener := inventorylisteners.New(inventory.NewRepository(queries), log)
	// Fatia 3: ERP mirror — projeta estado ERP do cart na Order (best-effort, aditivo).
	if integrationSvc != nil {
		integrationSvc.SetERPOrderMirror(orderListener)
	}
	orderHandler.RegisterRoutes(storeScoped)

	// Carrinhos pendentes (não-pagos) — página de recuperação. Lê carts SEM
	// order correspondente via cart_product_total_cents (canônico).
	cartRepo := cart.NewRepository(pool)
	cartSvc := cart.NewService(cartRepo, log)
	cartHandler := cart.NewHandler(cartSvc)
	cartHandler.RegisterRoutes(storeScoped)

	// Merchant-side post-checkout actions (Marcar como entregue) live next
	// to the read-only orders routes to share the same store_id middleware.
	if postCheckoutSvc != nil {
		postCheckoutHandler.RegisterMerchantRoutes(storeScoped)
	}

	couponRepo := coupon.NewRepository(pool)
	couponSvc := coupon.NewService(couponRepo, pool, log)
	couponHandler := coupon.NewHandler(couponSvc)
	couponHandler.RegisterRoutes(storeScoped)
	couponHandler.RegisterPublicRoutes(app)

	// The redemption confirm/refund on payment facts now reacts to cart.paid /
	// cart.refunded as its own Coupon reactor (couponListener below) instead of
	// being wired onto the integration fan-out. couponLifecycle stays for
	// checkout.CouponLifecycle so a free-shipping / subtotal coupon keeps in sync
	// across shipping changes and cart mutations.
	couponLifecycle := coupon.NewRedemptionSyncer(couponSvc)
	if checkoutSvc != nil {
		checkoutSvc.SetCouponLifecycle(couponLifecycle)
	}
	// Coupon reactor: confirms the redemption on cart.paid (reserved → confirmed)
	// and refunds it on cart.refunded (→ refunded, slot back to circulation),
	// reacting to the facts instead of running inline in integration's fan-out.
	// couponSvc satisfies its RedemptionConfirmer; idempotent in the service.
	couponListener := couponlisteners.New(couponSvc, log)

	// NOTE: the periodic sweep workers (coupon-expirer, cart-recovery,
	// cart-expiry) were removed — cart expiration is now driven exclusively by
	// the asynq ETA schedule (cart.expire, armed at cart.checkout_armed /
	// cart.reopened → RunScheduledExpiry). Coupon-slot reclamation moved to the
	// cart.expired / cart.cancelled reactors (couponSvc.ExpireRedemptionForCart).
	// WhatsApp cart-recovery (PRD 006) was retired with its worker.

	customerHandler := customer.NewHandler(customerSvc)
	customerHandler.RegisterRoutes(storeScoped)

	dashboardRepo := dashboard.NewRepository(pool)
	dashboardSvc := dashboard.NewService(dashboardRepo, log)
	dashboardHandler := dashboard.NewHandler(dashboardSvc, validate)
	dashboardHandler.RegisterRoutes(storeScoped)

	// Integration routes (store-scoped)
	if integrationSvc != nil {
		integrationHandler := integration.NewHandler(integrationSvc, validate, s3Client)
		integrationHandler.RegisterRoutes(storeScoped)

		// Notification settings routes (depends on integration service)
		notificationHandler := notification.NewHandler(notificationSvc, log)
		notificationHandler.RegisterRoutes(storeScoped)
	}

	// Member routes (store-scoped)
	memberRepo := member.NewRepository(queries)
	memberSvc := member.NewService(memberRepo, log)
	memberHandler := member.NewHandler(memberSvc)
	memberHandler.RegisterRoutes(storeScoped)

	// Invitation routes
	invitationRepo := invitation.NewRepository(queries, pool)
	storeLookup := store.NewStoreLookupAdapter(storeSvc)
	memberLookup := member.NewMemberLookupAdapter(memberRepo)
	membershipLookup := member.NewMembershipLookupAdapter(memberRepo)
	invitationSvc := invitation.NewService(invitationRepo, emailClient, userLookup, storeLookup, memberLookup, membershipLookup, log)
	invitationHandler := invitation.NewHandler(invitationSvc)

	// Public invitation routes (viewing invitation by token)
	// Using /api/public prefix to avoid auth middleware on /api/v1
	app.Get("/api/public/invitations/token/:token", invitationHandler.GetByToken)

	// Accept invitation route (requires auth but not store-scoped)
	invitationHandler.RegisterAcceptRoute(api)

	// Store-scoped invitation routes (create, list, revoke)
	invitationHandler.RegisterRoutes(storeScoped)

	// ---------------------------------------------------------------------------
	// Async event pipeline (asynq + Redis) — built here, after the domain
	// services, so consumer handlers can dispatch to them. Publisher feeds the
	// outbox relay; server consumes; relay drains the outbox to Redis.
	// ---------------------------------------------------------------------------
	// Prefer a full REDIS_URL (managed Redis like Railway carries user/password
	// and, with rediss://, TLS) and fall back to a bare REDIS_ADDR for local/
	// no-auth Redis. asynq.ParseRedisURI handles redis:// and rediss://.
	var redisOpt asynq.RedisConnOpt = asynq.RedisClientOpt{Addr: config.RedisAddr.StringOr("localhost:6379")}
	if redisURL := config.RedisURL.StringOr(""); redisURL != "" {
		parsed, err := asynq.ParseRedisURI(redisURL)
		if err != nil {
			log.Sugar().Fatalf("parsing REDIS_URL: %v", err)
		}
		redisOpt = parsed
	}
	eventsClient := events.NewClient(redisOpt)
	eventsServer := events.NewServer(events.ServerConfig{RedisOpt: redisOpt, Logger: log})
	if integrationSvc != nil {
		// Inverted comment flow: the webhook dispatches comment.received; the
		// domain work (cart/stock/waitlist) runs here, idempotent by comment_id.
		eventsServer.Register(events.CommentReceived, func(ctx context.Context, t *asynq.Task) error {
			var env events.Envelope
			if err := json.Unmarshal(t.Payload(), &env); err != nil {
				return asynq.SkipRetry
			}
			var input integration.ProcessInstagramCommentInput
			if err := json.Unmarshal(env.Payload, &input); err != nil {
				return asynq.SkipRetry
			}
			return integrationSvc.ProcessInstagramComment(ctx, input)
		})

		// Payment choreography L2: the payment.process command (emitted by the
		// thin webhook dispatcher) runs the guarded reconciliation, now with
		// asynq retry + dead-letter instead of a fire-and-forget goroutine.
		eventsServer.Register(events.PaymentProcess, func(ctx context.Context, t *asynq.Task) error {
			var env events.Envelope
			if err := json.Unmarshal(t.Payload(), &env); err != nil {
				return asynq.SkipRetry
			}
			var input integration.ProcessPaymentInput
			if err := json.Unmarshal(env.Payload, &input); err != nil {
				return asynq.SkipRetry
			}
			return integrationSvc.ProcessPaymentNotification(ctx, input)
		})

		// Payment choreography L3: reactors of the cart payment facts. The fan-out
		// that ProcessPaymentNotification used to run inline (coupon, order/GMV/
		// email/waitlist, billing) now reacts to cart.paid / cart.refunded, each
		// idempotent so asynq retry/dead-letter is safe. (ERP finalisation stays
		// inline in the producer — needs the gateway snapshot.)
		eventsServer.Register(events.CartPaid, func(ctx context.Context, t *asynq.Task) error {
			var env events.Envelope
			if err := json.Unmarshal(t.Payload(), &env); err != nil {
				return asynq.SkipRetry
			}
			var p struct {
				CartID          string          `json:"cart_id"`
				StoreID         string          `json:"store_id"`
				GMVCents        int64           `json:"gmv_cents"`
				PaymentSnapshot json.RawMessage `json:"payment_snapshot"`
			}
			if err := json.Unmarshal(env.Payload, &p); err != nil || p.CartID == "" {
				return asynq.SkipRetry
			}
			// Order materialisation (Fase A): freeze the immutable snapshot.
			// Runs first — idempotent by cart_id UNIQUE; errors retry the task.
			if err := orderListener.OnCartPaid(ctx, p.CartID, p.StoreID, p.GMVCents, p.PaymentSnapshot); err != nil {
				return err
			}
			// Coupon reactor: confirm the redemption (reserved → confirmed) in
			// reaction to cart.paid, replacing the inline coupon confirm that ran
			// first in integration's fan-out. Idempotent (no-op unless 'reserved');
			// error → asynq retry.
			if couponListener != nil {
				if err := couponListener.OnCartPaid(ctx, p.CartID); err != nil {
					return err
				}
			}
			// Customer-facing fan-out (tracking, timeline, GMV ledger). An error
			// retries the whole task (each step is idempotent) and dead-letters after
			// MaxRetry. ERP finalisation is NO LONGER here (Fatia 11b-2): it reacts to
			// the order.paid fact emitted by OnCartPaid, so its retry loop is decoupled
			// from this fan-out.
			if err := integrationSvc.ReactCartPaid(ctx, p.CartID, p.StoreID, p.GMVCents); err != nil {
				return err
			}
			// Inventory reactor (Fatia A2): the waitlist fulfilment reacts to
			// cart.paid on its own instead of running inline in the fan-out above
			// (extracted from postcheckout.OnCartPaid). Marks 'notified'→'fulfilled'
			// for this cart; idempotent (WHERE status='notified') so a redelivery is
			// a no-op and never re-emits waitlist.fulfilled. Error → asynq retry. No
			// ordering dependency with the Order materialisation above.
			if inventoryListener != nil {
				if err := inventoryListener.OnCartPaid(ctx, p.CartID); err != nil {
					return err
				}
			}
			// Notification reactor (Fatia A1): the buyer receipt reacts to cart.paid
			// on its own instead of being sent inline in the fan-out above. It runs
			// after orderListener.OnCartPaid materialised the Order + tracking token
			// (first step above, Fatia A4); its guard returns ErrReceiptNotReady (→
			// asynq retry) if they're not yet ready, and it is idempotent so a
			// redelivery never mails a second receipt.
			if notificationListener != nil {
				return notificationListener.OnCartPaid(ctx, p.CartID)
			}
			return nil
		})
		eventsServer.Register(events.CartRefunded, func(ctx context.Context, t *asynq.Task) error {
			var env events.Envelope
			if err := json.Unmarshal(t.Payload(), &env); err != nil {
				return asynq.SkipRetry
			}
			var p struct {
				CartID  string `json:"cart_id"`
				StoreID string `json:"store_id"`
			}
			if err := json.Unmarshal(env.Payload, &p); err != nil || p.CartID == "" || p.StoreID == "" {
				return asynq.SkipRetry
			}
			// Order state (Fatia E1): flip the immutable Order + its payment row to
			// 'refunded'. Idempotent; runs first so the authoritative order state is
			// consistent before the customer-facing fan-out. A missing Order (cart.paid
			// materialisation still in flight) returns an error → asynq retries.
			if err := orderListener.OnCartRefunded(ctx, p.CartID, p.StoreID); err != nil {
				return err
			}
			// Coupon reactor: refund the redemption (→ refunded, slot back to
			// circulation) in reaction to cart.refunded, replacing the inline coupon
			// refund from integration's fan-out. Idempotent (no-op if absent / already
			// refunded); error → asynq retry.
			if couponListener != nil {
				if err := couponListener.OnCartRefunded(ctx, p.CartID); err != nil {
					return err
				}
			}
			if err := integrationSvc.ReactCartRefunded(ctx, p.CartID, p.StoreID); err != nil {
				return err
			}
			// Notification reactor (Fatia A1): the refund email reacts to
			// cart.refunded on its own instead of being sent inline in the fan-out
			// above. Idempotent via the payment_refunded timeline marker.
			if notificationListener != nil {
				return notificationListener.OnCartRefunded(ctx, p.CartID)
			}
			return nil
		})

		// Order post-payment ERP reactors (Fatia 11b-2): the ERP finalisation/refund
		// moved off cart.paid/cart.refunded onto the order.* facts, emitted
		// transactionally by OnCartPaid/OnCartRefunded once the immutable Order exists.
		// Its asynq retry loop is now isolated from the coupon/billing/customer fan-out
		// (an ERP hiccup no longer re-runs those). Registered ONLY here — the 11a no-op
		// logEvent handlers for order.paid/order.refunded were removed from the default
		// registry, so this is the single registration (double registration panics
		// asynq), mirroring cart.paid/cart.refunded.
		eventsServer.Register(events.OrderPaid, func(ctx context.Context, t *asynq.Task) error {
			var env events.Envelope
			if err := json.Unmarshal(t.Payload(), &env); err != nil {
				return asynq.SkipRetry
			}
			var p struct {
				CartID          string          `json:"cart_id"`
				StoreID         string          `json:"store_id"`
				PaymentSnapshot json.RawMessage `json:"payment_snapshot"`
			}
			if err := json.Unmarshal(env.Payload, &p); err != nil || p.CartID == "" || p.StoreID == "" {
				return asynq.SkipRetry
			}
			// The Order already exists (materialised in the same tx that emitted this
			// fact), so finalisation resolves order_payments by cart_id. Snapshot rides
			// the payload (frozen by OnCartPaid); a nil snapshot finalises without
			// payment details.
			return integrationSvc.ReactOrderPaidERP(ctx, p.CartID, p.StoreID, p.PaymentSnapshot)
		})
		eventsServer.Register(events.OrderRefunded, func(ctx context.Context, t *asynq.Task) error {
			var env events.Envelope
			if err := json.Unmarshal(t.Payload(), &env); err != nil {
				return asynq.SkipRetry
			}
			var p struct {
				CartID  string `json:"cart_id"`
				StoreID string `json:"store_id"`
			}
			if err := json.Unmarshal(env.Payload, &p); err != nil || p.CartID == "" || p.StoreID == "" {
				return asynq.SkipRetry
			}
			return integrationSvc.ReactOrderRefundedERP(ctx, p.CartID, p.StoreID)
		})

		// cart.cancelled reactor (Fatia E1): flip the Order to 'cancelled'. Two
		// producers (payment-cancel webhook + blocked-handle cancel); the latter's
		// payload has no store_id, so store_id is optional here. Registered in the
		// composition root (NOT the default logEvent registry) — double registration
		// would panic asynq. Unpaid carts have no Order → ErrOrderNotMaterialised is
		// swallowed as a benign skip (the payment-cancel email stays inline in the
		// producer). The blocked-handle producer's SkipRetry-on-empty-store guard is
		// relaxed since that fact legitimately omits store_id.
		eventsServer.Register(events.CartCancelled, func(ctx context.Context, t *asynq.Task) error {
			var env events.Envelope
			if err := json.Unmarshal(t.Payload(), &env); err != nil {
				return asynq.SkipRetry
			}
			var p struct {
				CartID  string `json:"cart_id"`
				StoreID string `json:"store_id"`
			}
			if err := json.Unmarshal(env.Payload, &p); err != nil || p.CartID == "" {
				return asynq.SkipRetry
			}
			// Return the cart's reserved coupon slot to circulation (replaces the
			// removed coupon-expirer sweep). Idempotent; retried with the handler.
			if err := couponSvc.ExpireRedemptionForCart(ctx, p.CartID); err != nil {
				logger.From(ctx, log).Warn("cart.cancelled: coupon release failed",
					zap.String("cart_id", p.CartID), zap.Error(err))
				return err
			}
			if err := orderListener.OnCartCancelled(ctx, p.CartID, p.StoreID); err != nil {
				if errors.Is(err, orderlisteners.ErrOrderNotMaterialised) {
					// Unpaid cart cancelled → no Order was ever materialised. Expected.
					return nil
				}
				return err
			}
			return nil
		})

		// cart.expired reactor: reverse the cart's ERP footprint (Tiny cancel /
		// reservation estorno). Decoupled from the expiry flip so it retries + DLQs
		// on its own. store_id rides the fact payload (emitted transactionally in
		// ExpireCartAndReleaseStock). Registered here — cart.expired is NOT in the
		// default logEvent registry (double registration would panic asynq).
		eventsServer.Register(events.CartExpired, func(ctx context.Context, t *asynq.Task) error {
			var env events.Envelope
			if err := json.Unmarshal(t.Payload(), &env); err != nil {
				return asynq.SkipRetry
			}
			var p struct {
				CartID  string `json:"cart_id"`
				StoreID string `json:"store_id"`
			}
			if err := json.Unmarshal(env.Payload, &p); err != nil || p.CartID == "" || p.StoreID == "" {
				return asynq.SkipRetry
			}
			logger.From(ctx, log).Info("event observed",
				zap.String("event", string(env.Name)),
				zap.String("event_id", env.EventID),
			)
			// Return the cart's reserved coupon slot to circulation (replaces the
			// removed coupon-expirer sweep). Idempotent; retried with the handler.
			if err := couponSvc.ExpireRedemptionForCart(ctx, p.CartID); err != nil {
				logger.From(ctx, log).Warn("cart.expired: coupon release failed",
					zap.String("cart_id", p.CartID), zap.Error(err))
				return err
			}
			return integrationSvc.ReactCartExpiredERP(ctx, p.CartID, p.StoreID)
		})

		// ETA-based cart expiry: schedule a cart.expire task at the cart's
		// expires_at (asynq ProcessAt) so it expires on the second, with the
		// 5-min sweep kept as a safety net. The window is set at checkout-arm
		// (live carts have no expires_at until the event ends) and at reopen —
		// so those two events arm the timer. They are registered HERE (not in the
		// default logEvent registry) with a handler that logs AND arms — double
		// registration would panic asynq.
		integrationSvc.SetCartExpiryScheduler(cartExpiryScheduler{client: eventsClient})
		armCartExpiry := func(ctx context.Context, t *asynq.Task) error {
			var env events.Envelope
			if err := json.Unmarshal(t.Payload(), &env); err != nil {
				return asynq.SkipRetry
			}
			var p struct {
				CartID string `json:"cart_id"`
			}
			if err := json.Unmarshal(env.Payload, &p); err != nil || p.CartID == "" {
				return asynq.SkipRetry
			}
			logger.From(ctx, log).Info("event observed",
				zap.String("event", string(env.Name)),
				zap.String("event_id", env.EventID),
			)
			return integrationSvc.ScheduleExpiry(ctx, p.CartID)
		}
		eventsServer.Register(events.CartCheckoutArmed, armCartExpiry)
		eventsServer.Register(events.CartReopened, armCartExpiry)

		// The scheduled command itself: run the guarded expiry, or re-arm if the
		// window was pushed out (waitlist promotion) after the task was armed.
		eventsServer.Register(events.CartExpire, func(ctx context.Context, t *asynq.Task) error {
			var env events.Envelope
			if err := json.Unmarshal(t.Payload(), &env); err != nil {
				return asynq.SkipRetry
			}
			var p struct {
				CartID string `json:"cart_id"`
			}
			if err := json.Unmarshal(env.Payload, &p); err != nil || p.CartID == "" {
				return asynq.SkipRetry
			}
			return integrationSvc.RunScheduledExpiry(ctx, p.CartID)
		})

		// ETA-based timed-event close: schedule event.window_close at ends_at for
		// post/story events so their window finalizes on time, with the
		// SweepEndedTimedEvents sweep kept as the backstop.
		liveSvc.SetEventCloseScheduler(eventCloseScheduler{client: eventsClient})
		eventsServer.Register(events.EventWindowClose, func(ctx context.Context, t *asynq.Task) error {
			var env events.Envelope
			if err := json.Unmarshal(t.Payload(), &env); err != nil {
				return asynq.SkipRetry
			}
			var p struct {
				EventID string `json:"event_id"`
				StoreID string `json:"store_id"`
			}
			if err := json.Unmarshal(env.Payload, &p); err != nil || p.EventID == "" || p.StoreID == "" {
				return asynq.SkipRetry
			}
			return liveSvc.RunScheduledEventClose(ctx, p.EventID, p.StoreID)
		})
	}

	// Billing choreography L2: the subscription.process command (emitted by the
	// thin Stripe webhook dispatcher) runs the guarded ProcessWebhookEvent with
	// asynq retry + dead-letter instead of processing synchronously in-request.
	// It reconstructs the raw Stripe event and reconciles the local subscription,
	// emitting the canonical subscription.* facts.
	eventsServer.Register(events.SubscriptionProcess, func(ctx context.Context, t *asynq.Task) error {
		var env events.Envelope
		if err := json.Unmarshal(t.Payload(), &env); err != nil {
			return asynq.SkipRetry
		}
		var event billing.StripeEvent
		if err := json.Unmarshal(env.Payload, &event); err != nil {
			return asynq.SkipRetry
		}
		return billingSvc.ProcessWebhookEvent(ctx, &event)
	})

	// Group J: trial-ending reminder ETA task (billing — not gated on the
	// integration layer). Armed at trial_ends_at - N days in EnsureTrialSubscription;
	// the handler no-ops if the store already converted, else logs the signal
	// (phase 08 hooks the merchant notification there).
	billingSvc.SetTrialReminderScheduler(trialReminderScheduler{client: eventsClient})
	eventsServer.Register(events.TrialEndingSoon, func(ctx context.Context, t *asynq.Task) error {
		var env events.Envelope
		if err := json.Unmarshal(t.Payload(), &env); err != nil {
			return asynq.SkipRetry
		}
		var p struct {
			StoreID string `json:"store_id"`
		}
		if err := json.Unmarshal(env.Payload, &p); err != nil || p.StoreID == "" {
			return asynq.SkipRetry
		}
		return billingSvc.RunTrialEndingSoon(ctx, p.StoreID)
	})

	eventsRelay := events.NewRelay(events.RelayConfig{Pool: pool, Client: eventsClient, Logger: log})

	if err := eventsServer.Start(); err != nil {
		log.Sugar().Fatalf("starting event server: %v", err)
	}
	eventsRelay.Start()
	// Reverse stop order: relay (producer) -> server (consumer) -> client close.
	lifecycle.add("events-client", func() { _ = eventsClient.Close() })
	lifecycle.add("events-server", eventsServer.Stop)
	lifecycle.add("events-relay", eventsRelay.Stop)

	return app, lifecycle
}

// cartExpiryScheduler adapts the events client to integration.CartExpiryScheduler,
// enqueueing a cart.expire ETA task keyed "cart-expire:<id>" for dedup.
type cartExpiryScheduler struct{ client *events.Client }

func (s cartExpiryScheduler) ScheduleCartExpiry(ctx context.Context, cartID string, at time.Time) error {
	return s.client.Schedule(ctx, at, events.CartExpire, "cart-expire:"+cartID, struct {
		CartID string `json:"cart_id"`
	}{CartID: cartID})
}

// eventCloseScheduler adapts the events client to live.EventCloseScheduler,
// enqueueing an event.window_close ETA task keyed "event-close:<id>" for dedup.
type eventCloseScheduler struct{ client *events.Client }

func (s eventCloseScheduler) ScheduleEventClose(ctx context.Context, eventID, storeID string, at time.Time) error {
	return s.client.Schedule(ctx, at, events.EventWindowClose, "event-close:"+eventID, struct {
		EventID string `json:"event_id"`
		StoreID string `json:"store_id"`
	}{EventID: eventID, StoreID: storeID})
}

// trialReminderScheduler adapts the events client to billing.TrialReminderScheduler,
// enqueueing a trial.ending_soon ETA task keyed "trial-ending:<store>" for dedup.
type trialReminderScheduler struct{ client *events.Client }

func (s trialReminderScheduler) ScheduleTrialEndingSoon(ctx context.Context, storeID string, at time.Time) error {
	return s.client.Schedule(ctx, at, events.TrialEndingSoon, "trial-ending:"+storeID, struct {
		StoreID string `json:"store_id"`
	}{StoreID: storeID})
}

// liveNotifierAdapter bridges integration.InstagramNotifier (concrete impl)
// to live.Notifier (local interface). The packages cannot share the params
// type without introducing an import cycle, so we translate at the boundary.
type liveNotifierAdapter struct {
	inner *integration.InstagramNotifier
}

func newLiveNotifierAdapter(inner *integration.InstagramNotifier) *liveNotifierAdapter {
	return &liveNotifierAdapter{inner: inner}
}

func (a *liveNotifierAdapter) NotifyEventCheckout(ctx context.Context, p live.NotifyEventCheckoutParams) error {
	return a.inner.NotifyEventCheckout(ctx, integration.NotifyEventCheckoutParams{
		StoreID:        p.StoreID,
		EventID:        p.EventID,
		CartID:         p.CartID,
		CartToken:      p.CartToken,
		PlatformUserID: p.PlatformUserID,
		PlatformHandle: p.PlatformHandle,
		// CommentID enables the private-reply delivery path (7-day window per
		// comment). Dropping it here silently forced every resend onto the
		// direct-message path, which Instagram rejects outside the 24h window
		// (error 2534022) — a comment alone never opens that window.
		CommentID:  p.CommentID,
		TotalItems: p.TotalItems,
		TotalValue: p.TotalValue,
	})
}

// erpFinalizerAdapter bridges integration.Service.FinalizeEventERP to
// live.ERPFinalizer (local interface), avoiding the import cycle.
type erpFinalizerAdapter struct {
	svc *integration.Service
}

func newERPFinalizerAdapter(svc *integration.Service) *erpFinalizerAdapter {
	return &erpFinalizerAdapter{svc: svc}
}

func (a *erpFinalizerAdapter) FinalizeEventERP(ctx context.Context, storeID, eventID string) error {
	return a.svc.FinalizeEventERP(ctx, storeID, eventID)
}
