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
	fixos    map[string]*Fixo
	logger   *zap.Logger
}

// NewManager creates a new rate limit manager.
func NewManager(logger *zap.Logger) *Manager {
	return &Manager{
		limiters: make(map[string]*AdaptiveLimiter),
		fixos:    make(map[string]*Fixo),
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

// GetOrCreateFixo devolve um limitador PREDITIVO para a chave, criando se
// preciso. É o caminho dos provedores que não devolvem header de cota.
//
// ⚠ A CHAVE não é o integration id. Para o Bling ela é a CONTA do ERP, porque o
// teto é por conta somando todos os apps do lojista: duas lojas LiveCart na
// mesma empresa Bling têm de dividir UM balde, e chavear por integração daria
// dois baldes para uma cota só — o dobro do teto, descoberto por 429 no meio da
// venda.
func (m *Manager) GetOrCreateFixo(chave string, rps float64) *Fixo {
	m.mu.RLock()
	if f, ok := m.fixos[chave]; ok {
		m.mu.RUnlock()
		return f
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if f, ok := m.fixos[chave]; ok {
		return f
	}
	f := NovoFixo(rps)
	m.fixos[chave] = f
	return f
}

// Remove cleans up the limiter for a deleted integration.
func (m *Manager) Remove(integrationID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.limiters, integrationID)
	delete(m.fixos, integrationID)
}
