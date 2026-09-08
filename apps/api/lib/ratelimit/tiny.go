package ratelimit

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Start at 80% of the smallest documented plan (30/minute), even when
// responses have no quota headers. Learn each category's quota independently.
const tinyDefaultIntervalMS int64 = 2500

// Tiny limits reads and writes independently. PostgreSQL coordinates LiveCart
// replicas; other applications on the merchant's account remain outside it.
type Tiny struct {
	account string
	*requestBudget
}

// requestBudget is the shared PostgreSQL/local dispatch clock. Provider adapters
// choose account keys and feedback; claiming a slot has one implementation.
type requestBudget struct {
	pool              *pgxpool.Pool
	defaultIntervalMS int64
	mu                sync.Mutex
	local             map[string]*tinyBudget
	now               func() time.Time
}

type tinyBudget struct {
	interval             time.Duration
	nextAt, blockedUntil time.Time
}

func (m *Manager) SetSharedPool(pool *pgxpool.Pool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pool = pool
}

func (m *Manager) GetOrCreateTiny(account string) *Tiny {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.tiny[account]; t != nil {
		return t
	}
	t := &Tiny{account: account, requestBudget: newRequestBudget(m.pool, tinyDefaultIntervalMS)}
	m.tiny[account] = t
	return t
}

func (t *Tiny) category(method string) string {
	if method == http.MethodGet || method == http.MethodHead {
		return t.account + ":read"
	}
	return t.account + ":write"
}

func (t *Tiny) Wait(ctx context.Context) error { return t.WaitRequest(ctx, http.MethodPost) }
func (t *Tiny) Allow(ctx context.Context) (*Reservation, error) {
	return t.claim(ctx, t.category(http.MethodPost))
}

// BaseProvider uses ObserveResponse: feedback without a method cannot safely
// be attributed to either budget.
func (t *Tiny) UpdateFromHeaders(int, int) {}

func (t *Tiny) WaitRequest(ctx context.Context, method string) error {
	return t.wait(ctx, t.category(method))
}

func (t *requestBudget) wait(ctx context.Context, key string) error {
	for {
		res, err := t.claim(ctx, key)
		if err != nil {
			return err
		}
		if res.Allowed {
			return nil
		}
		wait := res.RetryAfter + time.Millisecond
		if deadline, ok := ctx.Deadline(); ok && time.Now().Add(wait).After(deadline) {
			return ErrNaoDespachado
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w: %w", ErrNaoDespachado, ctx.Err())
		case <-timer.C:
		}
	}
}

func (t *requestBudget) localBudget(key string) *tinyBudget {
	if t.local[key] == nil {
		t.local[key] = &tinyBudget{interval: time.Duration(t.defaultIntervalMS) * time.Millisecond}
	}
	return t.local[key]
}

// Claim only the current slot. Waiting/cancelled callers never reserve future
// capacity, and they recheck cooldowns announced while they were waiting.
func (t *requestBudget) claim(ctx context.Context, key string) (*Reservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNaoDespachado, err)
	}
	if t.pool == nil {
		t.mu.Lock()
		defer t.mu.Unlock()
		budget, now := t.localBudget(key), t.now()
		wait := max(budget.nextAt.Sub(now), budget.blockedUntil.Sub(now), 0)
		if wait == 0 {
			budget.nextAt = now.Add(budget.interval)
		}
		return &Reservation{Allowed: wait == 0, RetryAfter: wait, Remaining: -1}, nil
	}
	if _, err := t.pool.Exec(ctx, `INSERT INTO api_rate_budgets(account_key,interval_ms) VALUES($1,$2) ON CONFLICT DO NOTHING`, key, t.defaultIntervalMS); err != nil {
		return nil, fmt.Errorf("reserving API budget: %w", err)
	}
	tx, err := t.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var delay float64
	err = tx.QueryRow(ctx, `SELECT GREATEST(0,EXTRACT(EPOCH FROM GREATEST(next_at,blocked_until)-clock_timestamp()))::float8 FROM api_rate_budgets WHERE account_key=$1 FOR UPDATE`, key).Scan(&delay)
	if err == nil && delay <= 0 {
		_, err = tx.Exec(ctx, `UPDATE api_rate_budgets SET next_at=clock_timestamp()+interval_ms*interval '1 millisecond' WHERE account_key=$1`, key)
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &Reservation{Allowed: delay <= 0, RetryAfter: time.Duration(delay * float64(time.Second)), Remaining: -1}, nil
}

func (t *Tiny) ObserveResponse(ctx context.Context, method string, status int, headers http.Header) error {
	key := t.category(method)
	remaining, remErr := strconv.Atoi(headers.Get("X-RateLimit-Remaining"))
	limit, _ := strconv.Atoi(headers.Get("X-RateLimit-Limit"))
	reset := RetryAfter(headers, 0)
	var intervalMS int64
	// Minute quotas start at 30. A measured burst response (Limit: 4, Reset: 1)
	// describes a different window; it must not replace the minute quota.
	if limit >= 30 && limit <= 1000000 {
		intervalMS = max(1, (75000+int64(limit)-1)/int64(limit))
		if remErr == nil && remaining > 0 && reset > 0 {
			intervalMS = max(intervalMS, (reset.Milliseconds()+int64(remaining)-1)/int64(remaining))
		}
	}
	var blocked time.Duration
	if status == http.StatusTooManyRequests || (remErr == nil && remaining <= 0) {
		blocked = reset
		if blocked <= 0 {
			blocked = time.Minute
		}
	}
	if intervalMS == 0 && blocked == 0 {
		return nil
	}
	return t.update(ctx, key, intervalMS, blocked)
}

func (t *requestBudget) update(ctx context.Context, key string, intervalMS int64, blocked time.Duration) error {
	if t.pool == nil {
		t.mu.Lock()
		defer t.mu.Unlock()
		budget := t.localBudget(key)
		if intervalMS > 0 {
			budget.interval = time.Duration(intervalMS) * time.Millisecond
		}
		if until := t.now().Add(blocked); until.After(budget.blockedUntil) {
			budget.blockedUntil = until
		}
		return nil
	}
	initialInterval := intervalMS
	if initialInterval == 0 {
		initialInterval = t.defaultIntervalMS
	}
	_, err := t.pool.Exec(ctx, `INSERT INTO api_rate_budgets(account_key,interval_ms,blocked_until)
  VALUES($1,$2,clock_timestamp()+$3*interval '1 millisecond')
  ON CONFLICT(account_key) DO UPDATE SET
  interval_ms=CASE WHEN $4::bigint>0 THEN EXCLUDED.interval_ms ELSE api_rate_budgets.interval_ms END,
  blocked_until=GREATEST(api_rate_budgets.blocked_until,EXCLUDED.blocked_until)`,
		key, initialInterval, blocked.Milliseconds(), intervalMS)
	return err
}

func newRequestBudget(pool *pgxpool.Pool, intervalMS int64) *requestBudget {
	return &requestBudget{pool: pool, defaultIntervalMS: intervalMS, local: make(map[string]*tinyBudget), now: time.Now}
}
