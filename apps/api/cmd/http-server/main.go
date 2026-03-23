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
	"livecart/apps/api/lib/database"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"

	"livecart/apps/api/internal/customer"
	"livecart/apps/api/internal/dashboard"
	"livecart/apps/api/internal/live"
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

	app := newApp(log, pool, queries, validate, clerkClient)

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

func newApp(log *zap.Logger, pool *pgxpool.Pool, queries *sqlc.Queries, validate *validator.Validate, clerkClient *clerk.Client) *fiber.App {
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
	userSvc := user.NewService(userRepo)

	// Webhook routes (unauthenticated, use webhook signature)
	webhookHandler := user.NewWebhookHandler(userSvc)
	webhookHandler.RegisterRoutes(app)

	// Protected routes (user-scoped)
	api := app.Group("/api/v1")
	api.Use(httpx.AuthMiddleware(clerkClient))
	api.Use(httpx.SubscriptionMiddleware())

	// User routes (not store-scoped)
	userHandler := user.NewHandler(userSvc, validate)
	userHandler.RegisterRoutes(api)

	// Store routes (user's own store management)
	storeRepo := store.NewRepository(queries)
	storeSvc := store.NewService(storeRepo)
	storeHandler := store.NewHandler(storeSvc, validate)
	storeHandler.RegisterRoutes(api)

	// Store-scoped routes (require store access validation)
	storeScoped := api.Group("/stores/:storeId")
	storeScoped.Use(httpx.StoreAccessMiddleware(userRepo))

	productRepo := product.NewRepository(queries, pool)
	productSvc := product.NewService(productRepo)
	productHandler := product.NewHandler(productSvc, validate)
	productHandler.RegisterRoutes(storeScoped)

	liveRepo := live.NewRepository(queries, pool)
	liveSvc := live.NewService(liveRepo)
	liveHandler := live.NewHandler(liveSvc, validate)
	liveHandler.RegisterRoutes(storeScoped)

	orderRepo := order.NewRepository(pool)
	orderSvc := order.NewService(orderRepo)
	orderHandler := order.NewHandler(orderSvc, validate)
	orderHandler.RegisterRoutes(storeScoped)

	customerRepo := customer.NewRepository(pool)
	customerSvc := customer.NewService(customerRepo)
	customerHandler := customer.NewHandler(customerSvc, validate)
	customerHandler.RegisterRoutes(storeScoped)

	dashboardRepo := dashboard.NewRepository(pool)
	dashboardSvc := dashboard.NewService(dashboardRepo)
	dashboardHandler := dashboard.NewHandler(dashboardSvc)
	dashboardHandler.RegisterRoutes(storeScoped)

	return app
}
