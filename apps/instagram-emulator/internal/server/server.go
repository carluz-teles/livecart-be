package server

import (
	"fmt"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/livecart/instagram-emulator/internal/config"
	"github.com/livecart/instagram-emulator/internal/simulator"
)

// Server represents the HTTP server that emulates Meta's API
type Server struct {
	app     *fiber.App
	config  *config.Config
	session *simulator.Session
}

// New creates a new server instance
func New(cfg *config.Config, session *simulator.Session) *Server {
	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
	})

	// Middleware
	app.Use(logger.New(logger.Config{
		Format: "[${time}] ${status} - ${method} ${path}\n",
	}))
	app.Use(cors.New())

	s := &Server{
		app:     app,
		config:  cfg,
		session: session,
	}

	// Setup routes
	s.setupRoutes()

	return s
}

// Start starts the HTTP server
func (s *Server) Start() error {
	addr := fmt.Sprintf(":%d", s.config.Port)
	return s.app.Listen(addr)
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown() error {
	return s.app.Shutdown()
}
