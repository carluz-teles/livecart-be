package ratelimit

import (
	"sync"

	"go.uber.org/zap"
)

// Manager creates and caches AdaptiveLimiter instances keyed by integration ID.
// Each integration gets its own limiter that auto-calibrates via API headers.
type Manager struct {
	mu       sync.RWMutex
	limiters map[string]*AdaptiveLimiter
	logger   *zap.Logger
}

// NewManager creates a new rate limit manager.
func NewManager(logger *zap.Logger) *Manager {
	return &Manager{
		limiters: make(map[string]*AdaptiveLimiter),
		logger:   logger.Named("ratelimit"),
	}
}

// GetOrCreate returns an existing limiter for the integration or creates a new one.
func (m *Manager) GetOrCreate(integrationID string) *AdaptiveLimiter {
	// Fast path: read lock
	m.mu.RLock()
	if limiter, ok := m.limiters[integrationID]; ok {
		m.mu.RUnlock()
		return limiter
	}
	m.mu.RUnlock()

	// Slow path: write lock
	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	if limiter, ok := m.limiters[integrationID]; ok {
		return limiter
	}

	limiter := NewAdaptiveLimiter(m.logger.With(zap.String("integration_id", integrationID)))
	m.limiters[integrationID] = limiter
	return limiter
}

// Remove cleans up the limiter for a deleted integration.
func (m *Manager) Remove(integrationID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.limiters, integrationID)
}
