package server

// setupRoutes configures all HTTP routes
func (s *Server) setupRoutes() {
	// Webhook verification endpoint
	s.app.Get("/webhook", s.handleWebhookVerification)

	// Live media endpoint (for polling)
	// Meta's format: GET /{ig-user-id}/live_media
	s.app.Get("/:userId/live_media", s.handleGetLiveMedia)

	// Health check
	s.app.Get("/health", s.handleHealth)
}
