package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/livecart/instagram-emulator/internal/cli"
	"github.com/livecart/instagram-emulator/internal/config"
	"github.com/livecart/instagram-emulator/internal/server"
	"github.com/livecart/instagram-emulator/internal/simulator"
	"github.com/livecart/instagram-emulator/internal/webhook"
)

func main() {
	// Load configuration
	cfg := config.Load()

	// Initialize simulator session
	session := simulator.NewSession(cfg.AccountID, cfg.Username, cfg.MediaID)

	// Initialize webhook sender
	sender := webhook.NewSender(cfg.WebhookURL)

	// Start HTTP server in background
	srv := server.New(cfg, session)
	go func() {
		if err := srv.Start(); err != nil {
			fmt.Printf("Server error: %v\n", err)
			os.Exit(1)
		}
	}()

	// Handle graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// Start CLI REPL
	repl := cli.NewREPL(cfg, session, sender)

	// Run REPL in goroutine
	done := make(chan struct{})
	go func() {
		repl.Run()
		close(done)
	}()

	// Wait for either REPL to finish or signal
	select {
	case <-done:
		// REPL exited normally
	case <-quit:
		fmt.Println("\nShutting down...")
	}

	// Shutdown server
	if err := srv.Shutdown(); err != nil {
		fmt.Printf("Error shutting down server: %v\n", err)
	}
}
