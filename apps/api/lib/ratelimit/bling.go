package ratelimit

import (
	"context"
	"net/http"
	"time"
)

// Bling shares one account budget across reads, writes and application replicas.
// Other merchants' apps remain outside our control; a 429 pauses this budget.
type Bling struct {
	*requestBudget
	account string
}

func (m *Manager) GetOrCreateBling(account string, rps float64) *Bling {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bling == nil {
		m.bling = make(map[string]*Bling)
	}
	if b := m.bling[account]; b != nil {
		return b
	}
	if rps <= 0 {
		rps = 2
	}
	b := &Bling{requestBudget: newRequestBudget(m.pool, max(1, int64(1000/rps))), account: account}
	m.bling[account] = b
	return b
}

func (b *Bling) Allow(ctx context.Context) (*Reservation, error) { return b.claim(ctx, b.account) }
func (b *Bling) Wait(ctx context.Context) error                  { return b.wait(ctx, b.account) }
func (b *Bling) WaitRequest(ctx context.Context, _ string) error { return b.Wait(ctx) }
func (b *Bling) UpdateFromHeaders(int, int)                      {}
func (b *Bling) BlockFor(ctx context.Context, d time.Duration) error {
	return b.update(ctx, b.account, 0, d)
}
func (b *Bling) ObserveResponse(ctx context.Context, _ string, status int, headers http.Header) error {
	if status != http.StatusTooManyRequests {
		return nil
	}
	return b.BlockFor(ctx, RetryAfter(headers, 2*time.Second))
}
