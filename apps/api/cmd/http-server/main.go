package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
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
	"livecart/apps/api/lib/database"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"

	"livecart/apps/api/internal/product"
	"livecart/apps/api/internal/store"
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log, err := logger.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	// Run migrations in dev
	if os.Getenv("APP_ENV") != "production" {
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

	app := newApp(log, pool, queries, validate)

	port := os.Getenv("PORT")
	if port == "" {
		port = "3001"
	}

	go func() {
		if err := app.Listen(":" + port); err != nil {
			log.Sugar().Fatalf("server error: %v", err)
		}
	}()

	log.Sugar().Infof("server listening on :%s", port)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutting down server")
	cancel()
	if err := app.Shutdown(); err != nil {
		log.Sugar().Errorf("server shutdown error: %v", err)
	}
}

func newApp(log *zap.Logger, pool *pgxpool.Pool, queries *sqlc.Queries, validate *validator.Validate) *fiber.App {
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

	// Protected routes
	api := app.Group("/api/v1")
	api.Use(httpx.AuthMiddleware(os.Getenv("CLERK_SECRET_KEY")))
	api.Use(httpx.SubscriptionMiddleware())

	// Register domain handlers
	storeRepo := store.NewRepository(queries)
	storeSvc := store.NewService(storeRepo)
	storeHandler := store.NewHandler(storeSvc, validate)
	storeHandler.RegisterRoutes(api)

	productRepo := product.NewRepository(queries)
	productSvc := product.NewService(productRepo)
	productHandler := product.NewHandler(productSvc, validate)
	productHandler.RegisterRoutes(api)

	return app
}
