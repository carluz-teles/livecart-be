package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"syscall"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/fiber/v2/middleware/requestid"
	"github.com/gofiber/swagger"
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

	"livecart/apps/api/internal/customer"
	"livecart/apps/api/internal/dashboard"
	"livecart/apps/api/internal/integration"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/integration/providers/erp"
	"livecart/apps/api/internal/integration/providers/payment"
	"livecart/apps/api/internal/invitation"
	"livecart/apps/api/internal/live"
	"livecart/apps/api/internal/member"
	"livecart/apps/api/internal/order"
	"livecart/apps/api/internal/product"
	"livecart/apps/api/internal/store"
	"livecart/apps/api/internal/user"
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

	databaseURL := config.DatabaseURL.Required()
	clerkFrontendAPI := config.ClerkFrontendAPI.Required()
	port := config.Port.StringOr("3001")

	// Run migrations in dev
	if !config.IsProduction() {
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

	app := newApp(log, pool, queries, validate, clerkClient, emailClient)

	go func() {
		if err := app.Listen(":" + port); err != nil {
			log.Sugar().Fatalf("server error: %v", err)
		}
	}()

	log.Sugar().Infof("server listening on :%s (env: %s)", port, config.Environment())

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutting down server")
	cancel()
	if err := app.Shutdown(); err != nil {
		log.Sugar().Errorf("server shutdown error: %v", err)
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

func newApp(log *zap.Logger, pool *pgxpool.Pool, queries *sqlc.Queries, validate *validator.Validate, clerkClient *clerk.Client, emailClient *email.Client) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			return httpx.HandleServiceError(c, err)
		},
	})

	app.Use(recover.New())
	app.Use(requestid.New())
	app.Use(cors.New())
	app.Use(httpx.RequestLogger(log))

	// Swagger UI
	app.Get("/swagger/*", swagger.HandlerDefault)

	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	// User repository and service (shared between webhook and API handlers)
	userRepo := user.NewRepository(queries)
	userSvc := user.NewService(userRepo, log)

	// Integration Layer setup
	var integrationSvc *integration.Service
	var integrationWebhookHandler *integration.WebhookHandler

	if config.EncryptionKey.IsSet() {
		encryptor, err := crypto.NewEncryptor(config.EncryptionKey.String())
		if err != nil {
			log.Sugar().Warnf("integration layer disabled: %v", err)
		} else {
			// Create rate limit manager for integration providers
			rateLimitManager := ratelimit.NewManager(log)

			// Create provider factory with constructors
			providerFactory := providers.NewFactory(providers.FactoryConfig{
				Logger:               log,
				MercadoPagoAppID:     config.MercadoPagoAppID.String(),
				MercadoPagoAppSecret: config.MercadoPagoAppSecret.String(),
				RateLimitManager:     rateLimitManager,
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
				log,
			)

			// Set log function for providers
			providerFactory = providers.NewFactory(providers.FactoryConfig{
				Logger:               log,
				LogFunc:              integrationSvc.LogIntegrationOperation,
				MercadoPagoAppID:     config.MercadoPagoAppID.String(),
				MercadoPagoAppSecret: config.MercadoPagoAppSecret.String(),
				RateLimitManager:     rateLimitManager,
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
			})

			// Recreate service with logging-enabled factory
			integrationSvc = integration.NewService(
				integrationRepo,
				providerFactory,
				encryptor,
				idempotencySvc,
				log,
			)

			// Create webhook handler
			integrationWebhookHandler = integration.NewWebhookHandler(integrationSvc, log)

			log.Info("integration layer initialized")
		}
	}

	// Webhook routes (unauthenticated, use webhook signature)
	webhookHandler := user.NewWebhookHandler(userSvc)
	webhookHandler.RegisterRoutes(app)

	// Integration webhook routes (if enabled)
	if integrationWebhookHandler != nil {
		integrationWebhookHandler.RegisterRoutes(app)
	}

	// Protected routes (user-scoped)
	api := app.Group("/api/v1")
	api.Use(httpx.AuthMiddleware(clerkClient))
	api.Use(httpx.SubscriptionMiddleware())

	// User routes (not store-scoped)
	userHandler := user.NewHandler(userSvc, validate)
	userHandler.RegisterRoutes(api)

	// Store routes (user's own store management)
	storeRepo := store.NewRepository(queries)
	membershipCreator := user.NewMembershipCreatorAdapter(userSvc)
	userLookup := user.NewUserLookupAdapter(userSvc)
	storeSvc := store.NewService(storeRepo, membershipCreator, userLookup, log)
	storeHandler := store.NewHandler(storeSvc, validate)
	storeHandler.RegisterRoutes(api)

	// Store-scoped routes (require store access validation)
	storeScoped := api.Group("/stores/:storeId")
	storeScoped.Use(httpx.StoreAccessMiddleware(userRepo))

	// Store cart settings (store-scoped)
	storeHandler.RegisterStoreScopedRoutes(storeScoped)

	productRepo := product.NewRepository(queries, pool)
	productSvc := product.NewService(productRepo, log)
	productHandler := product.NewHandler(productSvc, validate)
	productHandler.RegisterRoutes(storeScoped)

	// Wire product syncer for ERP webhooks
	if integrationSvc != nil {
		integrationSvc.SetProductSyncer(product.NewProductSyncerAdapter(productSvc))
	}

	liveRepo := live.NewRepository(queries, pool)
	liveSvc := live.NewService(liveRepo, log)
	liveHandler := live.NewHandler(liveSvc, validate)
	liveHandler.RegisterRoutes(storeScoped)

	orderRepo := order.NewRepository(pool)
	orderSvc := order.NewService(orderRepo, log)
	orderHandler := order.NewHandler(orderSvc, validate)
	orderHandler.RegisterRoutes(storeScoped)

	customerRepo := customer.NewRepository(pool)
	customerSvc := customer.NewService(customerRepo, log)
	customerHandler := customer.NewHandler(customerSvc, validate)
	customerHandler.RegisterRoutes(storeScoped)

	dashboardRepo := dashboard.NewRepository(pool)
	dashboardSvc := dashboard.NewService(dashboardRepo, log)
	dashboardHandler := dashboard.NewHandler(dashboardSvc, validate)
	dashboardHandler.RegisterRoutes(storeScoped)

	// Integration routes (store-scoped)
	if integrationSvc != nil {
		integrationHandler := integration.NewHandler(integrationSvc, validate)
		integrationHandler.RegisterRoutes(storeScoped)
	}

	// Member routes (store-scoped)
	memberRepo := member.NewRepository(queries)
	memberSvc := member.NewService(memberRepo, log)
	memberHandler := member.NewHandler(memberSvc, validate)
	memberHandler.RegisterRoutes(storeScoped)

	// Invitation routes
	invitationRepo := invitation.NewRepository(queries)
	storeLookup := store.NewStoreLookupAdapter(storeSvc)
	memberLookup := member.NewMemberLookupAdapter(memberRepo)
	membershipLookup := member.NewMembershipLookupAdapter(memberRepo)
	invitationSvc := invitation.NewService(invitationRepo, emailClient, userLookup, storeLookup, memberLookup, membershipLookup, log)
	invitationHandler := invitation.NewHandler(invitationSvc, validate)

	// Public invitation routes (viewing invitation by token)
	// Using /api/public prefix to avoid auth middleware on /api/v1
	app.Get("/api/public/invitations/token/:token", invitationHandler.GetByToken)

	// Accept invitation route (requires auth but not store-scoped)
	invitationHandler.RegisterAcceptRoute(api)

	// Store-scoped invitation routes (create, list, revoke)
	invitationHandler.RegisterRoutes(storeScoped)

	return app
}
